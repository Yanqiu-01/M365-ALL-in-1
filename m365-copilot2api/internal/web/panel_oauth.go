package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
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

func (s *Server) authorizeAccountWithPassword(email, password string) (panelOAuthAccount, error) {
	email = strings.TrimSpace(email)
	out := panelOAuthAccount{Email: email}
	if !nativePanelValidEmail(email) {
		out.Status, out.Detail = "failed", "email 格式无效"
		return out, errors.New(out.Detail)
	}
	if strings.TrimSpace(password) == "" {
		out.Status, out.Mode, out.Detail = "pending", "pkce", "账密清单没有该账号的密码，改走交互式 PKCE"
		return s.attachPKCE(out)
	}

	set, err := auth.ROPC(email, password)
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
	state, url, _, redirectURI, err := s.beginPKCEAuthorization("login")
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

	for _, email := range emails {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
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
		item, _ = s.authorizeAccountWithPassword(email, password)
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
