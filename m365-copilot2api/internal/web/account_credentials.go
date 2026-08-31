package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/auth"
)

// credentialVaultMu 保护 lazily 打开的凭据保险库。延迟打开的目的是让
// New() 不因为密钥文件不可写而整体失败，同时保持既有测试里直接构造
// &Server{} 的用法可用。
var (
	credentialVaultMu   sync.Mutex
	credentialVaultOnce *auth.CredentialVault
)

// credentialVaultPathOverride 让测试把保险库指到临时目录，而不去碰用户
// 真实配置目录。
var credentialVaultPathOverride string

func (s *Server) credentialVault() (*auth.CredentialVault, error) {
	credentialVaultMu.Lock()
	defer credentialVaultMu.Unlock()
	if credentialVaultOnce != nil && credentialVaultPathOverride == "" {
		return credentialVaultOnce, nil
	}
	vault, err := auth.OpenCredentialVault(credentialVaultPathOverride)
	if err != nil {
		return nil, err
	}
	if credentialVaultPathOverride == "" {
		credentialVaultOnce = vault
	}
	return vault, nil
}

// accountCredentials 管理 M365 账号的账密。
//
//	GET    /api/accounts/credentials              -> 只列元数据，绝不返回口令
//	POST   /api/accounts/credentials              -> 新增或更新某账号的口令
//	DELETE /api/accounts/credentials?accountId=... -> 删除已存口令
//
// 口令始终经 Fernet 加密后落盘（internal/auth/credentials.go），没有明文
// 兜底路径；密钥不可用时整个请求失败，而不是退化成可读文件。
func (s *Server) accountCredentials(w http.ResponseWriter, r *http.Request) {
	// 与 provisionAccount 一致地再查一次管理员会话：这两个端点收发账号口令，
	// 不依赖中间件是唯一防线。
	if !s.credentialAdminAllowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	vault, err := s.credentialVault()
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "credential_vault_error", err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"credentials": vault.List(), "vaultPath": vault.Path(), "encrypted": true})
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var body struct {
			AccountID string `json:"accountId"`
			Email     string `json:"email"`
			Password  string `json:"password"`
			// Verify 为 true 时先用 ROPC 向 Microsoft 校验账密，校验失败则
			// 不写入，避免保存一个用不了的口令。
			Verify bool `json:"verify"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		accountID := strings.TrimSpace(body.AccountID)
		email := strings.TrimSpace(body.Email)
		if accountID == "" {
			accountID = email
		}
		if accountID == "" || body.Password == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "accountId (or email) and password are required")
			return
		}
		if email == "" && s.tokens != nil {
			if account, ok := s.tokens.Get(accountID); ok {
				email = account.Email
			}
		}
		verified := false
		if body.Verify {
			if email == "" {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "email is required to verify the credential")
				return
			}
			if _, err := auth.ROPC(email, body.Password); err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "credential_verification_failed", err.Error())
				return
			}
			verified = true
		}
		if err := vault.Put(accountID, email, body.Password); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "credential_vault_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{
			"status":    "updated",
			"accountId": accountID,
			"email":     email,
			"verified":  verified,
			"encrypted": true,
			"updatedAt": time.Now().UTC(),
		})
	case http.MethodDelete:
		accountID := strings.TrimSpace(r.URL.Query().Get("accountId"))
		if accountID == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "accountId is required")
			return
		}
		deleted, err := vault.Delete(accountID)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "credential_vault_error", err.Error())
			return
		}
		if !deleted {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "no stored credential for that account")
			return
		}
		jsonOut(w, map[string]any{"status": "deleted", "accountId": accountID})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// runScriptStep 是一键回调的单步进度。前端按顺序渲染即可。
type runScriptStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | failed | skipped
	Detail string `json:"detail,omitempty"`
}

// accountRunScripts 是「账密一键回调」端点。
//
//	POST /api/accounts/web/run-scripts
//
// 能力边界（重要）：仓库内没有无头浏览器依赖，因此本端点不模拟 Microsoft
// 登录页的交互。它按两级策略工作：
//
//  1. 优先走 auth.ROPC（资源所有者口令流）。这条路径确实是服务端全自动的：
//     给定账密即可直接换到 refresh token 并入库，无需浏览器。适用于未开启
//     MFA / 条件访问、且租户允许 ROPC 的账号。
//  2. ROPC 被拒（MFA、条件访问、需要交互式同意等）时，复用既有 PKCE 能力
//     生成一次授权，返回授权 URL、state 与明确的下一步指示，由人工在浏览器
//     里完成登录，回调仍由 /api/auth/callback 处理。
//
// 两种情况都在响应里用 mode 与 steps 明示，不假装全自动。
func (s *Server) accountRunScripts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.credentialAdminAllowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		AccountID string `json:"accountId"`
		Email     string `json:"email"`
		Password  string `json:"password"`
		// SaveCredential 为 true 时把本次使用的口令写入加密保险库，供后续
		// 重新授权复用。默认不保存。
		SaveCredential bool `json:"saveCredential"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(body.Email)
	accountID := strings.TrimSpace(body.AccountID)
	if accountID == "" {
		accountID = email
	}
	if email == "" && accountID != "" && s.tokens != nil {
		if account, ok := s.tokens.Get(accountID); ok {
			email = account.Email
		}
	}
	steps := []runScriptStep{}

	// 未直接提供口令时，回落到保险库里已存的账密。
	password := body.Password
	if password == "" {
		vault, err := s.credentialVault()
		if err != nil {
			steps = append(steps, runScriptStep{Name: "load_stored_credential", Status: "failed", Detail: err.Error()})
		} else if stored, err := vault.Get(accountID); err == nil {
			password = stored
			steps = append(steps, runScriptStep{Name: "load_stored_credential", Status: "ok", Detail: "used the credential stored in the encrypted vault"})
		} else if !errors.Is(err, auth.ErrCredentialNotFound) {
			steps = append(steps, runScriptStep{Name: "load_stored_credential", Status: "failed", Detail: err.Error()})
		}
	}
	if email == "" || password == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "email and password are required (or store the credential first)")
		return
	}
	steps = append(steps, runScriptStep{Name: "resolve_credential", Status: "ok", Detail: "credential resolved for " + email})

	// 第 1 级：服务端全自动的 ROPC。
	set, ropcErr := auth.ROPC(email, password)
	if ropcErr == nil {
		steps = append(steps, runScriptStep{Name: "authorize_ropc", Status: "ok", Detail: "obtained tokens without a browser"})
		acc, err := s.tokens.Upsert(set)
		if err != nil {
			steps = append(steps, runScriptStep{Name: "persist_account", Status: "failed", Detail: err.Error()})
			writeOpenAIError(w, http.StatusInternalServerError, "upsert_error", err.Error())
			return
		}
		hasRefresh := strings.TrimSpace(set.RefreshToken) != ""
		refreshStatus := "ok"
		refreshDetail := "refresh token stored"
		if !hasRefresh {
			refreshStatus = "failed"
			refreshDetail = "upstream returned no refresh token; the account cannot be renewed unattended"
		}
		steps = append(steps,
			runScriptStep{Name: "persist_account", Status: "ok", Detail: "account added to the pool"},
			runScriptStep{Name: "store_refresh_token", Status: refreshStatus, Detail: refreshDetail},
		)
		if body.SaveCredential {
			if vault, err := s.credentialVault(); err != nil {
				steps = append(steps, runScriptStep{Name: "save_credential", Status: "failed", Detail: err.Error()})
			} else if err := vault.Put(firstNonEmpty(acc.ID, accountID), email, password); err != nil {
				steps = append(steps, runScriptStep{Name: "save_credential", Status: "failed", Detail: err.Error()})
			} else {
				steps = append(steps, runScriptStep{Name: "save_credential", Status: "ok", Detail: "credential encrypted at rest"})
			}
		} else {
			steps = append(steps, runScriptStep{Name: "save_credential", Status: "skipped", Detail: "saveCredential was not requested"})
		}
		jsonOut(w, map[string]any{
			"status":   "completed",
			"mode":     "ropc",
			"complete": true,
			"steps":    steps,
			"account": map[string]any{
				"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
				"status": acc.Status, "expiresAt": acc.ExpiresAt, "updatedAt": acc.UpdatedAt,
			},
			"hasRefreshToken": hasRefresh,
		})
		return
	}

	// 第 2 级：ROPC 不可用，交回交互式 PKCE。
	steps = append(steps, runScriptStep{Name: "authorize_ropc", Status: "failed", Detail: ropcErr.Error()})
	// 单账号的交互式回退：保持独占语义，旧的终态条目残留会让 UI 反复进入
	// 上一次的回调。
	state, url, attempt, redirectURI, err := s.beginPKCEAuthorization("login", false)
	if err != nil {
		steps = append(steps, runScriptStep{Name: "prepare_interactive_authorization", Status: "failed", Detail: err.Error()})
		writeOpenAIError(w, http.StatusInternalServerError, "pkce_error", err.Error())
		return
	}
	steps = append(steps, runScriptStep{Name: "prepare_interactive_authorization", Status: "ok", Detail: "PKCE authorization prepared"})
	if body.SaveCredential {
		if vault, err := s.credentialVault(); err != nil {
			steps = append(steps, runScriptStep{Name: "save_credential", Status: "failed", Detail: err.Error()})
		} else if err := vault.Put(accountID, email, password); err != nil {
			steps = append(steps, runScriptStep{Name: "save_credential", Status: "failed", Detail: err.Error()})
		} else {
			steps = append(steps, runScriptStep{Name: "save_credential", Status: "ok", Detail: "credential encrypted at rest"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "manual_step_required",
		"mode":     "pkce",
		"complete": false,
		"steps":    steps,
		"reason":   ropcErr.Error(),
		"limitation": "This build has no headless browser, so the Microsoft sign-in page cannot be driven server-side. " +
			"ROPC was refused for this account (usually MFA or conditional access), so a human must complete sign-in once.",
		"authorizationUrl": url,
		"state":            state,
		"attempt":          attempt,
		"redirectUri":      redirectURI,
		"logoutUrl":        auth.LogoutURL(),
		"email":            email,
		"nextStep": "Open authorizationUrl in a browser and sign in as " + email + ". " +
			"The loopback redirect finishes automatically; otherwise POST the returned code to /api/auth/callback?state=<state>&code=<code>. " +
			"Poll /api/auth/status?state=<state> for completion.",
	})
}

// credentialAdminAllowed 判定调用方是否可以操作账号凭据。已配置管理员口令
// 时必须持有有效会话；未配置口令的场景（首次运行、单元测试）沿用
// adminMiddleware 的既有语义，由它统一处理。
func (s *Server) credentialAdminAllowed(r *http.Request) bool {
	if s == nil || s.adminPassword == "" {
		return true
	}
	return s.validAdminSession(r)
}
