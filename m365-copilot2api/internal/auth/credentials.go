package auth

// Credential vault for M365 account passwords. Passwords have to be
// recoverable rather than hashed because they are replayed to Microsoft for
// re-authorization (ROPC), so the file is always encrypted with the same
// Fernet primitives and key file that back the encrypted token store
// (fernet.go). There is deliberately no plaintext fallback: if the key cannot
// be loaded or created the vault fails closed instead of writing readable
// secrets to disk.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const credentialVaultMagic = "m365-credential-vault"

// ErrCredentialNotFound is returned when no credential is stored for an account.
var ErrCredentialNotFound = errors.New("no stored credential for account")

type credentialEntry struct {
	AccountID string    `json:"accountId"`
	Email     string    `json:"email,omitempty"`
	Password  string    `json:"password"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type credentialVaultBody struct {
	Credentials []credentialEntry `json:"credentials"`
}

type credentialVaultEnvelope struct {
	Magic      string `json:"magic"`
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Ciphertext string `json:"ciphertext"`
}

// CredentialVault stores account passwords encrypted at rest.
type CredentialVault struct {
	mu   sync.Mutex
	path string
	key  []byte
	body credentialVaultBody
}

// CredentialVaultPath resolves the vault location. It sits beside the token
// cache so a single data directory holds all account state.
func CredentialVaultPath() string {
	if p := strings.TrimSpace(os.Getenv("M365_CREDENTIAL_VAULT")); p != "" {
		return p
	}
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "credential-vault.json")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config", "m365-copilot2api", "credential-vault.json")
	}
	return filepath.Join(".", "credential-vault.json")
}

// OpenCredentialVault loads the vault, creating the encryption key on first
// use. A missing vault file is not an error; a corrupt or undecryptable one is.
func OpenCredentialVault(path string) (*CredentialVault, error) {
	if path == "" {
		path = CredentialVaultPath()
	}
	v := &CredentialVault{path: path}
	key, err := loadOrCreateFernetKey(path)
	if err != nil {
		return nil, err
	}
	v.key = key

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	var envelope credentialVaultEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode credential vault envelope: %w", err)
	}
	if envelope.Magic != credentialVaultMagic {
		return nil, errors.New("not an m365 credential vault")
	}
	if strings.TrimSpace(envelope.Ciphertext) == "" {
		return v, nil
	}
	token, err := base64.URLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		token, err = base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	}
	if err != nil {
		return nil, fmt.Errorf("decode credential vault ciphertext: %w", err)
	}
	plain, err := fernetDecrypt(token, v.key)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential vault: %w", err)
	}
	if err := json.Unmarshal(plain, &v.body); err != nil {
		return nil, fmt.Errorf("decode credential vault body: %w", err)
	}
	return v, nil
}

// Path reports the vault file location.
func (v *CredentialVault) Path() string { return v.path }

// loadOrCreateFernetKey reuses the encrypted-store key when one already
// exists, and otherwise generates a fresh 32-byte Fernet key with 0600
// permissions. Failing to obtain a key is fatal by design: the caller must
// never degrade to storing plaintext.
func loadOrCreateFernetKey(storePath string) ([]byte, error) {
	keyPath := storeKeyPath(storePath)
	key, loadErr := loadFernetKey(storePath)
	if loadErr == nil {
		return key, nil
	}
	// 只有「密钥文件确实不存在」才生成新的。
	//
	// 这里原本是 if err == nil { return } 然后无条件往下走生成并覆写。于是文件存在
	// 但一时读不出来 —— 权限被改、瞬时 I/O 错误、内容损坏、长度不对 —— 都会被当成
	// 首次运行，用一把新密钥覆盖旧的。凡是用旧密钥加密过的凭据从那一刻起永久不可
	// 解，而且覆写是静默的：调用方只看到「成功拿到密钥」。
	//
	// 首次运行（文件不存在）与「我读不到已有密钥」是两件完全不同的事，后者必须致命。
	if !errors.Is(loadErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("refusing to overwrite the existing store key %s: %w "+
			"(fix the key file instead; generating a new one would make every credential "+
			"encrypted with the old key permanently unreadable)", keyPath, loadErr)
	}
	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, err
	}
	encoded := base64.URLEncoding.EncodeToString(material)
	if err := os.WriteFile(keyPath, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("create store key %s: %w", keyPath, err)
	}
	return material, nil
}

// saveLocked serializes and encrypts the whole vault, then writes it
// atomically. Only ciphertext ever reaches the filesystem.
func (v *CredentialVault) saveLocked() error {
	plain, err := json.Marshal(v.body)
	if err != nil {
		return err
	}
	token, err := fernetEncrypt(plain, v.key)
	if err != nil {
		return err
	}
	envelope := credentialVaultEnvelope{
		Magic:      credentialVaultMagic,
		Version:    1,
		KDF:        "fernet",
		Ciphertext: base64.URLEncoding.EncodeToString(token),
	}
	out, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
		return err
	}
	return atomicWrite(v.path, out, 0o600)
}

// Put stores or replaces the password for an account.
func (v *CredentialVault) Put(accountID, email, password string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("accountId is required")
	}
	if password == "" {
		return errors.New("password is required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now().UTC()
	for i := range v.body.Credentials {
		if v.body.Credentials[i].AccountID == accountID {
			v.body.Credentials[i].Password = password
			v.body.Credentials[i].UpdatedAt = now
			if email != "" {
				v.body.Credentials[i].Email = email
			}
			return v.saveLocked()
		}
	}
	v.body.Credentials = append(v.body.Credentials, credentialEntry{
		AccountID: accountID, Email: email, Password: password, UpdatedAt: now,
	})
	return v.saveLocked()
}

// Get returns the stored password for an account.
func (v *CredentialVault) Get(accountID string) (string, error) {
	accountID = strings.TrimSpace(accountID)
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, entry := range v.body.Credentials {
		if entry.AccountID == accountID || (entry.Email != "" && entry.Email == accountID) {
			return entry.Password, nil
		}
	}
	return "", ErrCredentialNotFound
}

// Delete removes a stored credential. It reports whether an entry was present.
func (v *CredentialVault) Delete(accountID string) (bool, error) {
	accountID = strings.TrimSpace(accountID)
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := range v.body.Credentials {
		if v.body.Credentials[i].AccountID != accountID {
			continue
		}
		v.body.Credentials = append(v.body.Credentials[:i], v.body.Credentials[i+1:]...)
		return true, v.saveLocked()
	}
	return false, nil
}

// CredentialMeta describes a stored credential without exposing the secret.
type CredentialMeta struct {
	AccountID string    `json:"accountId"`
	Email     string    `json:"email,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// List returns credential metadata only. Passwords are never included.
func (v *CredentialVault) List() []CredentialMeta {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]CredentialMeta, 0, len(v.body.Credentials))
	for _, entry := range v.body.Credentials {
		out = append(out, CredentialMeta{AccountID: entry.AccountID, Email: entry.Email, UpdatedAt: entry.UpdatedAt})
	}
	return out
}
