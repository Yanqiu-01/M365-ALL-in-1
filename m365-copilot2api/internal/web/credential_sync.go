package web

// 账密自动补齐。
//
// 现象：网关有 481 个账号，但加密 vault 里只有 33 条账密，于是「账密一键回调」
// 在密码留空时报「密码为空」——因为 vault 里确实没有。
//
// 原因：账号是由 Python 注册/OAuth 脚本导入的，那些脚本把账密写进配置里的
// 文本清单（cred_file，格式 email----password，共 751 行），而 vault 是 Go 侧
// 独立的加密存储，两边从未同步。
//
// 做法：把文本清单里的账密按邮箱匹配到账号池，缺失的补进 vault。这样用户
// 「已经导入的账号」就自动带上账密，不需要再手动逐个录入。

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"m365-copilot2api/internal/auth"
)

// credentialSyncReport 是一次补齐的结果，字段刻意可读，供前端直接展示。
type credentialSyncReport struct {
	Accounts  int      `json:"accounts"`
	SourceOK  bool     `json:"sourceOk"`
	Source    string   `json:"source"`
	Total     int      `json:"sourceEntries"`
	Added     int      `json:"added"`
	AlreadyOK int      `json:"alreadyPresent"`
	Missing   int      `json:"missing"`
	Failed    int      `json:"failed"`
	Examples  []string `json:"missingExamples,omitempty"`
}

// syncCredentialsFromPanel 把面板账密清单补进 vault，返回补齐报告。
func (s *Server) syncCredentialsFromPanel() (credentialSyncReport, error) {
	report := credentialSyncReport{}
	if s == nil || s.tokens == nil {
		return report, errors.New("账号池不可用")
	}
	vault, err := s.credentialVault()
	if err != nil {
		return report, err
	}

	manager := nativePanelManagerFor(s)
	if manager == nil {
		return report, errors.New("本地面板不可用，无法定位账密清单")
	}
	paths, cfg, err := manager.workerConfig()
	if err != nil {
		return report, err
	}
	credentialPath, err := paths.credentialPath(cfg)
	if err != nil {
		return report, err
	}
	entries, err := nativePanelReadCredentials(credentialPath)
	if err != nil {
		return report, err
	}
	report.Source, report.SourceOK, report.Total = credentialPath, true, len(entries)

	// 邮箱大小写不敏感地建索引：清单与账号池的大小写不一定一致。
	byEmail := make(map[string]string, len(entries))
	for email, password := range entries {
		if e := strings.ToLower(strings.TrimSpace(email)); e != "" && password != "" {
			byEmail[e] = password
		}
	}

	accounts := s.tokens.List()
	report.Accounts = len(accounts)
	for _, account := range accounts {
		email := strings.TrimSpace(account.Email)
		if email == "" {
			continue
		}
		// 已有账密就不动：vault 里的值可能是用户手工改过的，更权威。
		if _, err := vault.Get(account.ID); err == nil {
			report.AlreadyOK++
			continue
		} else if !errors.Is(err, auth.ErrCredentialNotFound) {
			report.Failed++
			continue
		}
		password, ok := byEmail[strings.ToLower(email)]
		if !ok {
			report.Missing++
			if len(report.Examples) < 5 {
				report.Examples = append(report.Examples, email)
			}
			continue
		}
		if err := vault.Put(account.ID, email, password); err != nil {
			report.Failed++
			continue
		}
		report.Added++
	}
	return report, nil
}

// handleCredentialSync 处理 POST /api/accounts/credentials/sync。
func (s *Server) handleCredentialSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.credentialAdminAllowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	report, err := s.syncCredentialsFromPanel()
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "credential_vault_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"ok": true, "report": report})
}

// SyncCredentialsAtStartup 在进程启动时补齐账密。失败只记日志：账密补齐是
// 便利功能，不该阻止网关启动。
func (s *Server) SyncCredentialsAtStartup() {
	safeGo("credentialSync.startup", func() {
		report, err := s.syncCredentialsFromPanel()
		if err != nil {
			log.Printf("[credential-sync] skipped: %v", err)
			return
		}
		log.Printf("[credential-sync] accounts=%d source_entries=%d added=%d already=%d missing=%d failed=%d",
			report.Accounts, report.Total, report.Added, report.AlreadyOK, report.Missing, report.Failed)
	})
}
