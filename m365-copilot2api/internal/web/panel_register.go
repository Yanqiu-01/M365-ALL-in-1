package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"m365-copilot2api/internal/exitrotate"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/turnstile"
)

// panelRegisterRequest 是一次 Go 内置注册。Turnstile token 优先由本机
// FlareSolverr 从注册页取出；调用方仍可手动提供 token 作为回退。
// 每个号都先联网过 Cloudflare、提交 /api/register 并写入本地账密，
// 确认成功后再换出口 IP，给下一个号用。
type panelRegisterRequest struct {
	Mode           string `json:"mode"`
	Count          int    `json:"count"`
	StartNum       int    `json:"startNum"`
	EndNum         int    `json:"endNum"`
	TurnstileToken string `json:"turnstileToken"`
	Node           string `json:"node"`
	Proxy          string `json:"proxy"`
}

type panelRegisterAccount struct {
	Num              int    `json:"num"`
	Email            string `json:"email"`
	Status           string `json:"status"` // success | failed | skipped
	IP               string `json:"ip,omitempty"`
	Detail           string `json:"detail,omitempty"`
	OAuthStatus      string `json:"oauthStatus,omitempty"`
	OAuthDetail      string `json:"oauthDetail,omitempty"`
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
}

type panelRegisterReport struct {
	OK       bool                   `json:"ok"`
	Mode     string                 `json:"mode"`
	Total    int                    `json:"total"`
	Success  int                    `json:"success"`
	Failed   int                    `json:"failed"`
	Accounts []panelRegisterAccount `json:"accounts"`
	Rotate   *exitrotate.Result     `json:"rotate,omitempty"`
	// Notes 记录「做完了但没完全达到预期」的情况，例如代理池只有一个条目、整批
	// 账号只能复用同一出口。这类事不该只是静默通过，也不该算成失败。
	Notes []string `json:"notes,omitempty"`
}

const panelRegisterMax = 20

func (cfg nativePanelFileConfig) registerReady() bool {
	for _, value := range []string{cfg.Register.SiteURL, cfg.Register.EmailDomain, cfg.Register.EmailPrefix, cfg.Register.Password} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func (s *Server) runRegister(ctx context.Context, manager *nativePanelManager, request panelRegisterRequest) (panelRegisterReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "" {
		mode = "proxy"
	}
	if mode != "phone" && mode != "clash" && mode != "proxy" {
		return panelRegisterReport{}, errors.New("未知注册模式，支持 phone / clash / proxy")
	}
	report := panelRegisterReport{Mode: mode, Accounts: []panelRegisterAccount{}}
	if manager == nil {
		return report, errors.New("本地面板不可用")
	}
	paths, cfg, err := manager.panelData()
	if err != nil {
		return report, err
	}
	if !cfg.registerReady() {
		return report, errors.New("注册配置不完整：需要 site_url、email_domain、email_prefix、password")
	}
	// clash 模式的前置条件要在这里一次性说清楚，而不是让 rotateClash 在第二个
	// 账号那里抛「clash api, group and node are required」。那条消息出现在
	// 「上一号已写入本地，但换 IP 失败」里，既不说缺什么，也不说去哪里填。
	if mode == "clash" {
		var missing []string
		if strings.TrimSpace(cfg.Register.ClashAPI) == "" {
			missing = append(missing, "clash_api")
		}
		if strings.TrimSpace(cfg.Register.ClashGroup) == "" {
			missing = append(missing, "clash_group")
		}
		if strings.TrimSpace(request.Node) == "" && len(clashNodes(cfg)) == 0 {
			missing = append(missing, "clash_nodes")
		}
		if len(missing) > 0 {
			return report, fmt.Errorf("clash 模式配置不完整，缺少 %s：请在面板「注册配置」里填写 Clash 外部控制地址、策略组与节点清单（POST /api/admin/panel/config 的 clashApi / clashGroup / clashNodes）", strings.Join(missing, "、"))
		}
	}
	start := request.StartNum
	if start <= 0 {
		start = cfg.Register.EmailStartNum
	}
	if start <= 0 {
		start = 1000
	}
	count := request.Count
	if request.EndNum > 0 {
		if request.EndNum < start {
			return report, fmt.Errorf("结束编号 %d 小于起始编号 %d", request.EndNum, start)
		}
		count = request.EndNum - start + 1
	}
	if count <= 0 {
		count = 1
	}
	if count > panelRegisterMax {
		return report, fmt.Errorf("单次最多注册 %d 个账号", panelRegisterMax)
	}

	credentialPath, err := paths.credentialPath(cfg)
	if err != nil {
		return report, err
	}
	existing, err := nativePanelReadCredentials(credentialPath)
	if err != nil {
		return report, err
	}

	// 出口优先取探测通过的那些。
	//
	// 这里原本是 outbound.PickRawURL()，它返回「最优」条目 —— 而 tier() 把已驱逐和正在
	// 冷却的都归为同一档，没有更好选择时 bestLocked 仍会把它交出来。这个出口随后被递给
	// FlareSolverr，容器里的 Chrome 拿到一个不通的代理只会回
	// ERR_PROXY_CONNECTION_FAILED：整轮注册作废，而报错指向浏览器，完全看不出是出口挑
	// 错了。池子本身知道哪些是 live 的（入池时 ValidateProxyCandidate 探过 L2，之后由巡检
	// 维护状态），所以这里该问它要一个 live 的。
	//
	// PickLiveRawURL 没有 live 出口时返回 ""，此时回落到 PickRawURL：那是刻意的降级 ——
	// 用一个状态未知的去试，仍然好过完全不带代理直连（用户明确要求不能走直连）。
	proxyURL := firstNonEmpty(request.Proxy, cfg.Register.PhoneSOCKS, cfg.Register.ClashProxy)
	if mode == "proxy" || strings.TrimSpace(proxyURL) == "" {
		proxyURL = firstNonEmpty(proxyURL, outbound.PickLiveRawURL(), outbound.PickRawURL())
	}
	rotateReq := exitrotate.Request{
		Mode:        mode,
		PhoneSOCKS:  cfg.Register.PhoneSOCKS,
		ClashAPI:    cfg.Register.ClashAPI,
		ClashSecret: cfg.Register.ClashSecret,
		ClashGroup:  cfg.Register.ClashGroup,
		ClashProxy:  cfg.Register.ClashProxy,
		ClashNode:   firstNonEmpty(request.Node, firstClashNode(cfg)),
		ExpectIP:    firstClashNodeExpectIP(cfg, request.Node),
		ProbeProxy:  firstNonEmpty(request.Proxy, cfg.Register.ClashProxy, cfg.Register.PhoneSOCKS),
	}
	// 出口轮换需要一个候选列表，不能靠「再问一次」拿到不同结果。
	//
	// clash：节点名原先在循环外只算一次，于是每轮都切到同一个节点，等于没换。
	// proxy：outbound.PickRawURL() 是确定性的，原先的 next != proxyURL 恒为假，
	// 整批号全用同一个代理注册。两者都要按列表推进。
	//
	// request.Node 非空表示调用方指定了节点，此时不轮换 —— 那是显式意图。
	nodes := clashNodes(cfg)
	pinnedNode := strings.TrimSpace(request.Node) != ""
	// 轮换候选也只用 live 的，否则轮换会把已驱逐的出口重新轮进来 ——
	// 那正是「注册随机失败」的形态：前一个号成功，下一个号换到死出口就报
	// ERR_PROXY_CONNECTION_FAILED。没有 live 出口时回落到全量列表，理由同上：
	// 状态未知也好过不带代理。
	poolURLs := outbound.LiveProxyPoolRawURLs()
	if len(poolURLs) == 0 {
		poolURLs = outbound.ProxyPoolRawURLs()
	}
	exitTurn := 0
	var lastIP string
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}
		num := start + i
		username := fmt.Sprintf("%s%d", strings.TrimSpace(cfg.Register.EmailPrefix), num)
		email := username + "@" + strings.TrimSpace(cfg.Register.EmailDomain)
		item := panelRegisterAccount{Num: num, Email: email}
		if _, ok := existing[email]; ok {
			item.Status, item.Detail = "skipped", "账密清单已有该邮箱"
			report.Accounts = append(report.Accounts, item)
			continue
		}

		displayBase := cfg.Register.DisplayBase
		if displayBase <= 0 {
			displayBase = 1
		}
		emailStart := cfg.Register.EmailStartNum
		if emailStart <= 0 {
			emailStart = start
		}
		display := fmt.Sprintf("User%d", num-(emailStart-displayBase))
		probeCtx, probeCancel := context.WithTimeout(ctx, 8*time.Second)
		if ip, err := probeRegisterIP(probeCtx, proxyURL); err == nil {
			item.IP = ip
			lastIP = ip
		}
		probeCancel()
		outcome, outcomeErr := completeRegister(ctx, cfg, request.TurnstileToken, proxyURL, display, username)
		if outcomeErr != nil {
			item.Status, item.Detail = "failed", outcomeErr.Error()
			report.Failed++
			report.Accounts = append(report.Accounts, item)
			continue
		}
		if upn := strings.TrimSpace(outcome.UPN); nativePanelValidEmail(upn) {
			email = upn
			item.Email = email
		}
		if err := nativePanelAppendCredential(credentialPath, email, cfg.Register.Password); err != nil {
			item.Status, item.Detail = "failed", "注册成功但写入账密失败: "+err.Error()
			report.Failed++
			report.Accounts = append(report.Accounts, item)
			continue
		}
		existing[email] = cfg.Register.Password
		item.Status, item.Detail = "success", "registered"
		if s != nil && s.tokens != nil {
			if oauth, _ := s.authorizeAccountWithPassword(email, cfg.Register.Password); oauth.Status != "" {
				item.OAuthStatus = oauth.Status
				item.OAuthDetail = oauth.Detail
				item.AuthorizationURL = oauth.AuthorizationURL
				if oauth.Status == "imported" {
					item.Detail = "registered+oauth"
				} else if oauth.AuthorizationURL != "" {
					item.Detail = "registered, oauth pending"
				}
			}
		}
		report.Success++
		report.Accounts = append(report.Accounts, item)

		// 先联网过 CF、提交注册并写入本地账密，确认成功后再换出口，
		// 给下一个号用。开着飞行模式是过不了 Turnstile 的。
		if i < count-1 {
			exitTurn++
			if mode == "proxy" {
				// 按池子里的顺序推进。池中只有一个代理时确实换不了出口，这不是
				// 静默成功：如实记一行，让用户知道整批号共用同一个出口。
				switch {
				case len(poolURLs) > 1:
					proxyURL = poolURLs[exitTurn%len(poolURLs)]
				case len(poolURLs) == 1 && strings.TrimSpace(proxyURL) == "":
					proxyURL = poolURLs[0]
				default:
					report.Notes = append(report.Notes,
						fmt.Sprintf("第 %d 号之后未能更换出口：代理池仅有 %d 个可用条目，后续账号将复用同一出口", num, len(poolURLs)))
				}
				continue
			}
			if !pinnedNode && len(nodes) > 0 {
				next := nodes[exitTurn%len(nodes)]
				rotateReq.ClashNode = next.Name
				// 顺带把该节点预期的出口 IP 交给 exitrotate，让它的 IP 不符告警
				// 真的能触发。
				rotateReq.ExpectIP = next.ExpectIP
			}
			rotateReq.PrevIP = lastIP
			rotated, rotateErr := rotateExit(ctx, rotateReq)
			report.Rotate = &rotated
			if rotated.IP != "" {
				lastIP = rotated.IP
			}
			// OK 只表示流程没报错，Changed 才表示出口真的换了。两者分开看：轮换没
			// 生效时下一个号会从同一个 IP 注册，这一点必须让用户知道，而不是当成
			// 成功一路走下去。
			if rotateErr == nil && !rotated.Changed {
				detail := strings.TrimSpace(rotated.Detail)
				if detail == "" {
					detail = "出口未发生变化"
				}
				report.Notes = append(report.Notes,
					fmt.Sprintf("第 %d 号之后换出口未生效：%s；后续账号可能仍从同一 IP 注册", num, detail))
			}
			if rotateErr != nil {
				for j := i + 1; j < count; j++ {
					nextNum := start + j
					nextUser := fmt.Sprintf("%s%d", strings.TrimSpace(cfg.Register.EmailPrefix), nextNum)
					nextEmail := nextUser + "@" + strings.TrimSpace(cfg.Register.EmailDomain)
					report.Failed++
					report.Accounts = append(report.Accounts, panelRegisterAccount{
						Num: nextNum, Email: nextEmail, Status: "failed",
						Detail: "上一号已写入本地，但换 IP 失败: " + rotateErr.Error(),
					})
				}
				break
			}
		}
	}
	report.Total = len(report.Accounts)
	report.OK = report.Failed == 0 && report.Success > 0
	return report, nil
}

var rotateExit = exitrotate.Rotate

func probeRegisterIP(ctx context.Context, proxyURL string) (string, error) {
	result, err := rotateExit(ctx, exitrotate.Request{Mode: "proxy", ProbeProxy: proxyURL})
	if err != nil {
		return "", err
	}
	return result.IP, nil
}

type registerOutcome struct {
	UPN string
}

func completeRegister(ctx context.Context, cfg nativePanelFileConfig, supplied, proxyURL, display, username string) (registerOutcome, error) {
	token := strings.TrimSpace(supplied)
	if token == "" {
		endpoint := strings.TrimSpace(cfg.Register.FlareSolverrURL)
		if endpoint == "" {
			endpoint = turnstile.DefaultEndpoint
		}
		page := strings.TrimRight(strings.TrimSpace(cfg.Register.SiteURL), "/")
		if page == "" {
			return registerOutcome{}, errors.New("缺少注册页地址")
		}
		solved, err := turnstile.Solve(ctx, turnstile.Request{
			Endpoint:    endpoint,
			PageURL:     page,
			Proxy:       proxyURL,
			DisplayName: display,
			Username:    username,
			Password:    cfg.Register.Password,
		})
		if err != nil {
			return registerOutcome{}, err
		}
		token = strings.TrimSpace(solved.Token)
	}
	if strings.HasPrefix(token, "ERROR:") {
		return registerOutcome{}, errors.New(strings.TrimPrefix(token, "ERROR:"))
	}
	if strings.HasPrefix(token, "SUBMITTED:") {
		upn := strings.TrimPrefix(token, "SUBMITTED:")
		if upn == "registered-ok" {
			upn = ""
		}
		return registerOutcome{UPN: upn}, nil
	}
	if token == "" {
		return registerOutcome{}, errors.New("注册页已打开，但没有完成填表和提交")
	}
	if err := postRegister(ctx, cfg, username, display, token, proxyURL); err != nil {
		return registerOutcome{}, err
	}
	return registerOutcome{}, nil
}

// firstClashNodeExpectIP 给出首轮节点预期的出口 IP。
//
// 调用方显式指定了节点时，就按那个节点的配置取；否则用列表里的第一个。没有配
// expect_ip 就返回空串，exitrotate 会跳过 IP 校验。
func firstClashNodeExpectIP(cfg nativePanelFileConfig, requested string) string {
	nodes := clashNodes(cfg)
	if pinned := strings.TrimSpace(requested); pinned != "" {
		for _, node := range nodes {
			if node.Name == pinned {
				return node.ExpectIP
			}
		}
		return ""
	}
	if len(nodes) > 0 {
		return nodes[0].ExpectIP
	}
	return ""
}

// firstClashNode 给出首轮使用的节点名。后续轮换由 clashNodes 提供完整列表。
func firstClashNode(cfg nativePanelFileConfig) string {
	if nodes := clashNodes(cfg); len(nodes) > 0 {
		return nodes[0].Name
	}
	return ""
}

// clashNode 是一个可切换的 Clash 出口：节点名，以及该节点预期的出口 IP。
type clashNode struct {
	Name     string
	ExpectIP string
}

// clashNodes 返回配置里全部可用节点，供批量注册在账号之间轮换出口。
//
// 只取第一个节点是不够的：换出口的目的就是让下一个号从另一个 IP 注册，若每轮都
// 切到同一个节点，Clash 侧其实没有任何变化，注册仍然来自同一出口。
//
// expect_ip 一并带出。配置里早就有这个键，exitrotate 也早就会在实际 IP 与预期不符
// 时给出告警（rotate.go 的 ExpectIP 分支），但两头从来没接起来 —— 于是那个告警永
// 远不可能触发，配置项形同虚设。
func clashNodes(cfg nativePanelFileConfig) []clashNode {
	out := make([]clashNode, 0, len(cfg.Register.ClashNodes))
	for _, node := range cfg.Register.ClashNodes {
		name := strings.TrimSpace(node.Name)
		if name == "" {
			continue
		}
		out = append(out, clashNode{Name: name, ExpectIP: strings.TrimSpace(node.ExpectIP)})
	}
	return out
}

func postRegister(ctx context.Context, cfg nativePanelFileConfig, username, display, turnstile, proxyURL string) error {
	site := strings.TrimRight(strings.TrimSpace(cfg.Register.SiteURL), "/")
	if site == "" {
		return errors.New("site_url 为空")
	}
	planID := strings.TrimSpace(cfg.Register.PlanID)
	if planID == "" {
		planID = "1"
	}
	domainID := strings.TrimSpace(cfg.Register.DomainID)
	if domainID == "" {
		domainID = "1"
	}
	payload, _ := json.Marshal(map[string]any{
		"planId":            planID,
		"domainId":          domainID,
		"inviteCode":        "",
		"displayName":       display,
		"username":          username,
		"password":          cfg.Register.Password,
		"verificationEmail": "",
		"emailCode":         "",
		"turnstileToken":    turnstile,
	})
	client := outbound.HTTPClient()
	if strings.TrimSpace(proxyURL) != "" {
		if clients, err := outbound.New(proxyURL); err == nil && clients != nil && clients.HTTP != nil {
			client = clients.HTTP
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, site+"/api/register", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if v, exists := parsed["ok"]; exists {
		if flag, isBool := v.(bool); isBool {
			ok = flag
		}
	}
	if !ok {
		msg := strings.TrimSpace(fmt.Sprint(parsed["message"]))
		if msg == "" || msg == "<nil>" {
			msg = strings.TrimSpace(string(body))
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

func nativePanelAppendCredential(path, email, password string) error {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return errors.New("email and password required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s----%s\n", email, password)
	return err
}
