package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/outbound"
)

// panelOAuthAccount 是批量授权的一条结果。Mode 取值：
//
//	ropc        账密直换 token，无需浏览器
//	device_code 已发出设备码，等待用户在另一台设备确认
//	pkce        需要打开 Microsoft 登录页
//	skipped     账号池里已有该邮箱
type panelOAuthAccount struct {
	Email            string `json:"email"`
	Status           string `json:"status"` // imported | pending | skipped | failed
	Mode             string `json:"mode,omitempty"`
	Detail           string `json:"detail,omitempty"`
	UserCode         string `json:"userCode,omitempty"`
	VerificationURI  string `json:"verificationUri,omitempty"`
	DeviceCode       string `json:"deviceCode,omitempty"`
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
	State            string `json:"state,omitempty"`
	RedirectURI      string `json:"redirectUri,omitempty"`
}

type panelOAuthBatchRequest struct {
	Emails   []string `json:"emails"`
	StartNum int      `json:"startNum"`
	EndNum   int      `json:"endNum"`
	Limit    int      `json:"limit"`
	Resume   bool     `json:"resume"`
}

type panelOAuthBatchReport struct {
	OK       bool                `json:"ok"`
	Total    int                 `json:"total"`
	Imported int                 `json:"imported"`
	Pending  int                 `json:"pending"`
	Skipped  int                 `json:"skipped"`
	Failed   int                 `json:"failed"`
	Accounts []panelOAuthAccount `json:"accounts"`
}

const panelOAuthBatchMax = 64

// oauthExitRotateEvery 是批量授权切换出口的间隔：每这么多个账号换一个代理。
//
// 为什么需要它：auth.ROPC 经由 outbound.HTTPClient()，而池子的 pick() 是确定性的，连续
// 调用返回同一个出口。不轮换就意味着整批账号的 ROPC 全部来自同一个 IP。
//
// 取 100 是用户按实际经验给的值。它大于 panelOAuthBatchMax(64)，所以单次请求内通常不会
// 触发轮换 —— 轮换真正生效在跨请求的恢复流程里（759 个账号需要多次调用），以及日后有人
// 放宽单批上限时。这个关系是有意的，不是遗漏。
const oauthExitRotateEvery = 100

func (s *Server) authorizeAccountWithPassword(email, password string) (panelOAuthAccount, error) {
	return s.authorizeAccountWithPasswordVia("", email, password)
}

// authorizeAccountWithPasswordVia 与上面相同，但把 ROPC 请求钉在指定出口上。
//
// exitRawURL 为空时行为完全一致（走池子的默认选择），所以单账号路径不受影响；批量授权
// 用它来实现「每 N 个账号换一个代理」。
func (s *Server) authorizeAccountWithPasswordVia(exitRawURL, email, password string) (panelOAuthAccount, error) {
	email = strings.TrimSpace(email)
	out := panelOAuthAccount{Email: email}
	// 与 attachPKCE 保持同一道守卫。两者是同一功能的两个入口，一个能容忍 nil
	// 接收者另一个直接解引用，调用方就得记住走哪条路才安全 —— 那是迟早会踩的坑。
	if s == nil {
		out.Status, out.Detail = "failed", "OAuth 服务不可用"
		return out, errors.New(out.Detail)
	}
	if !nativePanelValidEmail(email) {
		out.Status, out.Detail = "failed", "email 格式无效"
		return out, errors.New(out.Detail)
	}
	if strings.TrimSpace(password) == "" {
		out.Status, out.Mode, out.Detail = "pending", "pkce", "账密清单没有该账号的密码，改走交互式 PKCE"
		return s.attachPKCE(out)
	}

	set, err := auth.ROPCVia(exitRawURL, email, password)
	if err == nil {
		if strings.TrimSpace(set.Email) == "" {
			set.Email = email
		}
		if strings.TrimSpace(set.HomeOID) == "" {
			set.HomeOID = email
		}
		if s.tokens != nil {
			if _, upsertErr := s.tokens.Upsert(set); upsertErr != nil {
				out.Status, out.Mode, out.Detail = "failed", "ropc", upsertErr.Error()
				return out, upsertErr
			}
		}
		out.Status, out.Mode, out.Detail = "imported", "ropc", "obtained tokens without a browser"
		return out, nil
	}

	out.Mode = "device_code"
	out.Detail = "ROPC refused: " + err.Error()
	device, deviceErr := auth.StartDeviceCode()
	if deviceErr == nil {
		out.Status = "pending"
		out.UserCode = device.UserCode
		out.VerificationURI = device.VerificationURI
		out.DeviceCode = device.DeviceCode
		out.Detail += "；已发出设备码，请在 " + device.VerificationURI + " 输入 " + device.UserCode
		return out, nil
	}
	out.Detail += "；设备码也不可用: " + deviceErr.Error()
	return s.attachPKCE(out)
}

func (s *Server) attachPKCE(out panelOAuthAccount) (panelOAuthAccount, error) {
	if s == nil {
		out.Status = "failed"
		if out.Detail == "" {
			out.Detail = "OAuth 服务不可用"
		}
		return out, errors.New(out.Detail)
	}
	// keepOthers=true：attachPKCE 只在批量路径上被调用（runOAuthBatch 逐个账号、
	// runRegister 注册完一个就顺手授权一个），每个账号的 state 都要活到它自己
	// 回调为止。写死 false 时上一号的 state 会被下一号抹掉，报告里带回的
	// authorizationUrl 除最后一个以外全部作废，回调只会得到
	// "invalid or expired state"。
	state, url, _, redirectURI, err := s.beginPKCEAuthorization("login", true)
	if err != nil {
		out.Status, out.Mode, out.Detail = "failed", "pkce", err.Error()
		return out, err
	}
	out.Status = "pending"
	out.Mode = "pkce"
	out.State = state
	out.AuthorizationURL = url
	out.RedirectURI = redirectURI
	if out.Detail == "" {
		out.Detail = "请在打开的 Microsoft 页面完成登录"
	}
	return out, nil
}

func (s *Server) runOAuthBatch(ctx context.Context, manager *nativePanelManager, request panelOAuthBatchRequest) (panelOAuthBatchReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := panelOAuthBatchReport{OK: true, Accounts: []panelOAuthAccount{}}
	if manager == nil {
		return report, errors.New("本地面板不可用，无法定位账密清单")
	}
	paths, cfg, err := manager.panelData()
	if err != nil {
		return report, err
	}
	credentialPath, err := paths.credentialPath(cfg)
	if err != nil {
		return report, err
	}
	credentials, err := nativePanelReadCredentials(credentialPath)
	if err != nil {
		return report, err
	}

	emails := uniqueEmails(request.Emails)
	if len(emails) == 0 {
		if request.StartNum > 0 || request.EndNum > 0 {
			matched, rangeErr := emailsInRange(credentials, strings.TrimSpace(cfg.Register.EmailPrefix), request.StartNum, request.EndNum)
			if rangeErr != nil {
				return report, rangeErr
			}
			emails = matched
		} else {
			emails = sortedEmails(credentials)
		}
	}
	if request.Limit > 0 && request.Limit < len(emails) {
		emails = emails[:request.Limit]
	}
	if len(emails) > panelOAuthBatchMax {
		return report, fmt.Errorf("单次最多 %d 个账号，当前 %d 个", panelOAuthBatchMax, len(emails))
	}

	online := map[string]bool{}
	if s != nil && s.tokens != nil {
		for _, account := range s.tokens.List() {
			if email := strings.ToLower(strings.TrimSpace(account.Email)); email != "" {
				online[email] = true
			}
		}
	}

	// 出口轮换：每 oauthExitRotateEvery 个账号换一个代理。
	//
	// auth.ROPC 走的是 outbound.HTTPClient()，而 pick() 是确定性的 —— 连续调用返回同一
	// 个出口。不轮换的话几百个账号会全部从同一个 IP 发起 ROPC，那是最容易被上游判成异常
	// 的形态。这里按池子里的顺序推进，每满一个批次换下一个。
	//
	// 池子为空时 exits 为空，attempted 恒取到 ""，行为退回 HTTPClient() 的默认选择 ——
	// 不因为没有代理就中断整批。
	// 计数必须跨请求累计，不能每次从 0 开始。
	//
	// panelOAuthBatchMax 是 64，而轮换间隔是 100：如果计数器随请求重置，
	// processed%100==0 永远不成立，759 个账号分成 12 批就会全部走同一个出口 ——
	// 轮换等于没实现。恢复流程恰恰是「多次请求累计几百个账号」这种形态，所以计数
	// 归服务端所有。
	// 同 panel_register：只轮换 live 出口，否则每 100 个号可能换到一个不通的。
	exits := outbound.LiveProxyPoolRawURLs()
	if len(exits) == 0 {
		exits = outbound.ProxyPoolRawURLs()
	}
	currentExit := func() string {
		if len(exits) == 0 {
			return ""
		}
		return exits[int(s.oauthExitTurn.Load())%len(exits)]
	}
	if len(exits) > 0 {
		log.Printf("[panel-oauth] batch of %d accounts over %d pool exits, rotating every %d accounts",
			len(emails), len(exits), oauthExitRotateEvery)
	} else {
		log.Printf("[panel-oauth] batch of %d accounts with an empty proxy pool: every request will use "+
			"the default egress", len(emails))
	}

	for _, email := range emails {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}
		// 满一批就换出口。计数在服务端累计，所以跨请求也成立 —— 恢复流程是十几次
		// 请求凑几百个账号，计数若随请求重置就永远换不了出口。
		//
		// 跳过的账号也计入：否则一次 resume 跳过大量已有账号会让轮换迟迟不发生。
		if n := s.oauthExitProcessed.Add(1); n > 1 && (n-1)%oauthExitRotateEvery == 0 {
			s.oauthExitTurn.Add(1)
			if len(exits) > 0 {
				log.Printf("[panel-oauth] rotated egress after %d accounts -> %s",
					n-1, outbound.RedactProxyURL(currentExit()))
			}
		}
		item := panelOAuthAccount{Email: email}
		if request.Resume && online[strings.ToLower(email)] {
			item.Status, item.Mode, item.Detail = "skipped", "skipped", "账号池已有该邮箱"
			report.Skipped++
			report.Accounts = append(report.Accounts, item)
			continue
		}
		password := credentials[email]
		if password == "" {
			for stored, value := range credentials {
				if strings.EqualFold(stored, email) {
					password = value
					break
				}
			}
		}
		item, _ = s.authorizeAccountWithPasswordVia(currentExit(), email, password)
		switch item.Status {
		case "imported":
			report.Imported++
		case "pending":
			report.Pending++
		case "skipped":
			report.Skipped++
		default:
			report.Failed++
			report.OK = false
		}
		report.Accounts = append(report.Accounts, item)
		time.Sleep(30 * time.Millisecond)
	}
	report.Total = len(report.Accounts)
	return report, nil
}

func uniqueEmails(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		email := strings.TrimSpace(raw)
		if email == "" || seen[strings.ToLower(email)] {
			continue
		}
		seen[strings.ToLower(email)] = true
		out = append(out, email)
	}
	return out
}

func sortedEmails(credentials map[string]string) []string {
	out := make([]string, 0, len(credentials))
	for email := range credentials {
		out = append(out, email)
	}
	sort.Strings(out)
	return out
}

func emailsInRange(credentials map[string]string, prefix string, start, end int) ([]string, error) {
	if start <= 0 {
		return nil, errors.New("邮箱编号区间无效：缺少起始编号 startNum")
	}
	if end <= 0 {
		end = start
	}
	if end < start {
		return nil, fmt.Errorf("邮箱编号区间无效：结束编号 %d 小于起始编号 %d", end, start)
	}
	if end-start+1 > panelOAuthBatchMax {
		return nil, fmt.Errorf("邮箱编号区间过大：%d-%d 共 %d 个，单次上限 %d 个", start, end, end-start+1, panelOAuthBatchMax)
	}
	matched := make([]string, 0)
	for _, email := range sortedEmails(credentials) {
		num, ok := nativePanelEmailNum(email, prefix)
		if !ok || num < start || num > end {
			continue
		}
		matched = append(matched, email)
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("账密清单中没有编号在 %d-%d 之间的账号", start, end)
	}
	return matched, nil
}
