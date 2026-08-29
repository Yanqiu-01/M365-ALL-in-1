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
	Num    int    `json:"num"`
	Email  string `json:"email"`
	Status string `json:"status"` // success | failed | skipped
	IP     string `json:"ip,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type panelRegisterReport struct {
	OK       bool                   `json:"ok"`
	Mode     string                 `json:"mode"`
	Total    int                    `json:"total"`
	Success  int                    `json:"success"`
	Failed   int                    `json:"failed"`
	Accounts []panelRegisterAccount `json:"accounts"`
	Rotate   *exitrotate.Result     `json:"rotate,omitempty"`
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

	proxyURL := firstNonEmpty(request.Proxy, cfg.Register.PhoneSOCKS, cfg.Register.ClashProxy, outbound.PickRawURL())
	rotateReq := exitrotate.Request{
		Mode:        mode,
		PhoneSOCKS:  cfg.Register.PhoneSOCKS,
		ClashAPI:    cfg.Register.ClashAPI,
		ClashSecret: cfg.Register.ClashSecret,
		ClashGroup:  cfg.Register.ClashGroup,
		ClashProxy:  cfg.Register.ClashProxy,
		ClashNode:   firstNonEmpty(request.Node, firstClashNode(cfg)),
		ProbeProxy:  firstNonEmpty(request.Proxy, cfg.Register.ClashProxy, cfg.Register.PhoneSOCKS),
	}
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
		token, tokenErr := resolveTurnstileToken(ctx, cfg, request.TurnstileToken, proxyURL, display, username)
		if tokenErr != nil {
			item.Status, item.Detail = "failed", tokenErr.Error()
			report.Failed++
			report.Accounts = append(report.Accounts, item)
			continue
		}
		if err := postRegister(ctx, cfg, username, display, token, proxyURL); err != nil {
			item.Status, item.Detail = "failed", err.Error()
			report.Failed++
			report.Accounts = append(report.Accounts, item)
			continue
		}
		if err := nativePanelAppendCredential(credentialPath, email, cfg.Register.Password); err != nil {
			item.Status, item.Detail = "failed", "注册成功但写入账密失败: "+err.Error()
			report.Failed++
			report.Accounts = append(report.Accounts, item)
			continue
		}
		existing[email] = cfg.Register.Password
		item.Status, item.Detail = "success", "registered"
		report.Success++
		report.Accounts = append(report.Accounts, item)

		// 先联网过 CF、提交注册并写入本地账密，确认成功后再换出口，
		// 给下一个号用。开着飞行模式是过不了 Turnstile 的。
		if i < count-1 && mode != "proxy" {
			rotateReq.PrevIP = lastIP
			rotated, rotateErr := rotateExit(ctx, rotateReq)
			report.Rotate = &rotated
			if rotated.IP != "" {
				lastIP = rotated.IP
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

func resolveTurnstileToken(ctx context.Context, cfg nativePanelFileConfig, supplied, proxyURL, display, username string) (string, error) {
	if token := strings.TrimSpace(supplied); token != "" {
		return token, nil
	}
	endpoint := strings.TrimSpace(cfg.Register.FlareSolverrURL)
	if endpoint == "" {
		endpoint = turnstile.DefaultEndpoint
	}
	page := strings.TrimRight(strings.TrimSpace(cfg.Register.SiteURL), "/")
	if page == "" {
		return "", errors.New("缺少注册页地址，无法请求 FlareSolverr")
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
		return "", err
	}
	return solved.Token, nil
}

func firstClashNode(cfg nativePanelFileConfig) string {
	for _, node := range cfg.Register.ClashNodes {
		if name := strings.TrimSpace(node.Name); name != "" {
			return name
		}
	}
	return ""
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
