package web

// 账密自动补齐。
//
// 现象：网关有 481 个账号，但加密 vault 里只有 33 条账密，于是「账密一键回调」
// 在密码留空时报「密码为空」——因为 vault 里确实没有。
//
// 原因：账号是由早期的 Python 注册/OAuth 脚本导入的，那些脚本把账密写进配置里
// 的文本清单（cred_file，格式 email----password，共 751 行），而 vault 是 Go 侧
// 独立的加密存储，两边从未同步。
//
// 做法：把文本清单里的账密按邮箱匹配到账号池，缺失的补进 vault。这样用户
// 「已经导入的账号」就自动带上账密，不需要再手动逐个录入。
//
// 那些脚本本身已经删除，但清单是留在用户机器上的既有数据，仍然是补齐账密的
// 唯一来源，因此读取逻辑保留（见 native_panel.go 的数据目录部分）。

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
	// Conflicts 是「vault 与清单对同一个账号给出不同密码」的条数。
	//
	// 之前这种情况被算进 AlreadyOK：vault 不声不响地赢了，报告说该账号已同步。
	// 可这两个值里至多一个能登录，而报告恰好把「有分歧」显示成「已就绪」——
	// 于是一键回调对着一个过期密码反复失败，操作员在报告里找不到任何线索。
	//
	// 保留 vault 的值（用户手工改过的更权威）这个决定不变，改变的只是它不再
	// 被隐瞒：分歧单独计数并给出邮箱样例，操作员据此决定删哪一边。
	Conflicts        int      `json:"conflicts"`
	ConflictExamples []string `json:"conflictExamples,omitempty"`
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
	paths, cfg, err := manager.panelData()
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
		// 但「不动」不等于「无话可说」：清单对同一个账号给出不同密码时，两个值
		// 里至多一个能登录，这必须报出来，而不是并入 AlreadyOK 当成已就绪。
		if stored, err := vault.Get(account.ID); err == nil {
			if listed, ok := byEmail[strings.ToLower(email)]; ok && listed != stored {
				report.Conflicts++
				if len(report.ConflictExamples) < 5 {
					report.ConflictExamples = append(report.ConflictExamples, email)
				}
				continue
			}
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
		log.Printf("[credential-sync] accounts=%d source_entries=%d added=%d already=%d conflicts=%d missing=%d failed=%d",
			report.Accounts, report.Total, report.Added, report.AlreadyOK, report.Conflicts, report.Missing, report.Failed)
		if report.Conflicts > 0 {
			// 分歧不该只躺在 JSON 报告里：启动期没人看那个接口。
			log.Printf("[credential-sync] %d 个账号的 vault 密码与清单不一致，已保留 vault 的值；"+
				"若一键回调持续失败请核对这些账号：%s",
				report.Conflicts, strings.Join(report.ConflictExamples, ", "))
		}
	})
}
