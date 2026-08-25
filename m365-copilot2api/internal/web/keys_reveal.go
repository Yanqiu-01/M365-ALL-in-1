package web

// API key 明文回显。
//
// 设计取舍：认证路径始终只信 sha256 摘要（keyHash），本文件保存的明文仅用于
// 「显示」。这样做的前提是明文必须加密落盘，因此直接复用项目已有的
// internal/auth.CredentialVault（Fernet + 0600 keyfile，见 account_credentials.go
// 的用法），而不是自己造一套加密。
//
// 列表接口保持只返回脱敏视图：仪表盘每隔几秒就轮询一次 /api/admin/keys，
// 若把明文塞进列表，等于每次轮询都在网络上搬运全部密钥。回显只走单个 key 的
// 显式请求，这个区分是有意为之。

import (
	"errors"
	"net/http"
	"strings"

	"m365-copilot2api/internal/auth"
)

// apiKeySecretVaultPrefix 给 key 明文在 vault 里的 accountID 加前缀，避免与
// M365 账号凭据（用 accountID / email 作键）撞车。
const apiKeySecretVaultPrefix = "apikey:"

// errKeySecretNotRetained 表示这个 key 是在启用明文保留之前创建的，磁盘上
// 只有摘要，明文在数学上不可恢复。必须如实告知，而不是回一个空串让前端渲染
// 成一个空白密钥。
var errKeySecretNotRetained = errors.New("该密钥创建于启用明文回显之前，系统只保存了它的哈希摘要，无法回显；请删除后重新创建")

func apiKeySecretVaultID(keyID string) string {
	return apiKeySecretVaultPrefix + strings.TrimSpace(keyID)
}

// retainAPIKeySecret 在创建 key 时保存明文。保存失败不应让创建失败——key 本身
// 已经可用了——但要把错误交回调用方决定如何提示。
func (s *Server) retainAPIKeySecret(keyID, name, secret string) error {
	if strings.TrimSpace(keyID) == "" || secret == "" {
		return errors.New("keyID 与明文都不能为空")
	}
	vault, err := s.credentialVault()
	if err != nil {
		return err
	}
	// email 参数在这里用于放 key 名称，纯展示用途。
	return vault.Put(apiKeySecretVaultID(keyID), "apikey:"+name, secret)
}

// revealAPIKeySecret 取回单个 key 的明文。
func (s *Server) revealAPIKeySecret(keyID string) (string, error) {
	if strings.TrimSpace(keyID) == "" {
		return "", errors.New("缺少 key id")
	}
	vault, err := s.credentialVault()
	if err != nil {
		return "", err
	}
	secret, err := vault.Get(apiKeySecretVaultID(keyID))
	if errors.Is(err, auth.ErrCredentialNotFound) {
		return "", errKeySecretNotRetained
	}
	if err != nil {
		return "", err
	}
	if secret == "" {
		return "", errKeySecretNotRetained
	}
	return secret, nil
}

// forgetAPIKeySecret 删除 key 时同步清掉明文，避免留下孤儿密钥。
func (s *Server) forgetAPIKeySecret(keyID string) {
	vault, err := s.credentialVault()
	if err != nil {
		return
	}
	_, _ = vault.Delete(apiKeySecretVaultID(keyID))
}

// adminKeyReveal 处理 GET /api/admin/keys/reveal?id=<keyID>。
// 一次只回一个 key，且必须带管理员会话（由 adminMiddleware 统一拦截）。
func (s *Server) adminKeyReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "缺少 key id")
		return
	}
	// 先确认这个 id 真的是本网关的 key，避免把 vault 当成任意读取接口。
	if !s.apiKeys.hasID(id) {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "key not found")
		return
	}
	secret, err := s.revealAPIKeySecret(id)
	if errors.Is(err, errKeySecretNotRetained) {
		// 409：请求合法，但服务端状态使其无法满足。前端据此显示解释文案。
		writeOpenAIError(w, http.StatusConflict, "key_secret_not_retained", err.Error())
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "credential_vault_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"id": id, "key": secret})
}

// hasID 判定 id 是否属于本网关的 key。放在这里而不是 keys.go，是为了让
// 「明文回显」这一关注点的代码集中在一个文件里。
func (s *apiKeyStore) hasID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Keys {
		if s.Keys[i].ID == id {
			return true
		}
	}
	return false
}
