package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultAdminPassword = "admin123"

const passwordVerifierPrefix = "v1$"
const passwordHashIterations = 100000

func newPasswordVerifier(password string) (string, error) {
	if password == "" {
		return "", errors.New("administrator password is empty")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := derivePasswordDigest(password, salt)
	return passwordVerifierPrefix + hex.EncodeToString(salt) + "$" + hex.EncodeToString(digest), nil
}

func derivePasswordDigest(password string, salt []byte) []byte {
	h := sha256.New()
	_, _ = h.Write(salt)
	_, _ = h.Write([]byte(password))
	digest := h.Sum(nil)
	for i := 1; i < passwordHashIterations; i++ {
		h.Reset()
		_, _ = h.Write(salt)
		_, _ = h.Write(digest)
		digest = h.Sum(digest[:0])
	}
	return digest
}

func parsePasswordVerifier(verifier string) ([]byte, []byte, bool) {
	parts := strings.Split(verifier, "$")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, nil, false
	}
	salt, err := hex.DecodeString(parts[1])
	if err != nil || len(salt) != 16 {
		return nil, nil, false
	}
	digest, err := hex.DecodeString(parts[2])
	if err != nil || len(digest) != sha256.Size {
		return nil, nil, false
	}
	return salt, digest, true
}

func verifyAdminPassword(password, verifier string) bool {
	salt, expected, ok := parsePasswordVerifier(verifier)
	if !ok {
		return false
	}
	actual := derivePasswordDigest(password, salt)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func configuredAdminPassword() string {
	if bootstrap := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE")); bootstrap != "" {
		if b, err := os.ReadFile(bootstrap); err == nil {
			if password := strings.TrimSpace(string(b)); password != "" {
				return password
			}
		}
	}
	return strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD"))
}

type loginAttempt struct {
	Failures                 int
	WindowStart, LockedUntil time.Time
}

func adminPasswordPath() string {
	if dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "admin-password")
	}
	if p := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD_FILE")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("M365_CONFIG")); p != "" {
		return filepath.Join(filepath.Dir(p), "admin-password")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "m365-copilot2api", "admin-password")
}
func loadAdminPassword() (string, bool) {
	if b, err := os.ReadFile(adminPasswordPath()); err == nil {
		if persisted := strings.TrimSpace(string(b)); persisted != "" {
			if persisted == defaultAdminPassword {
				configured := configuredAdminPassword()
				if configured == "" || configured == defaultAdminPassword {
					return "", true
				}
				verifier, err := newPasswordVerifier(configured)
				if err != nil {
					return "", true
				}
				if err := saveAdminPasswordVerifier(verifier); err != nil {
					return "", true
				}
				return verifier, false
			}
			if strings.HasPrefix(persisted, passwordVerifierPrefix) {
				if _, _, ok := parsePasswordVerifier(persisted); !ok {
					return "", true
				}
				return persisted, false
			}
			verifier, err := newPasswordVerifier(persisted)
			if err != nil {
				return "", true
			}
			if err := saveAdminPasswordVerifier(verifier); err != nil {
				return "", true
			}
			return verifier, false
		}
	}

	configured := configuredAdminPassword()
	if configured == "" || configured == defaultAdminPassword {
		return "", true
	}
	verifier, err := newPasswordVerifier(configured)
	if err != nil {
		return "", true
	}
	return verifier, false
}
func saveAdminPassword(password string) error {
	verifier, err := newPasswordVerifier(password)
	if err != nil {
		return err
	}
	return saveAdminPasswordVerifier(verifier)
}

func saveAdminPasswordVerifier(verifier string) error {
	if _, _, ok := parsePasswordVerifier(verifier); !ok {
		return errors.New("invalid administrator password verifier")
	}
	p := adminPasswordPath()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(verifier+"\n"), 0600)
}
func clientIP(r *http.Request) string {
	// Trust proxy headers only when the direct peer is loopback (normal local reverse-proxy deployment).
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if net.ParseIP(host).IsLoopback() {
		// A trusted reverse proxy appends the client address to XFF. Use the
		// right-most valid address rather than the attacker-controlled first one.
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
				return ip.String()
			}
		}
	}
	if host != "" {
		return host
	}
	return r.RemoteAddr
}
func validNewAdminPassword(p string) error {
	if p == defaultAdminPassword {
		return errors.New("new password must not be the default password")
	}
	if len(p) < 6 {
		return errors.New("new password must be at least 6 characters")
	}
	if len(p) > 256 {
		return errors.New("new password is too long")
	}
	return nil
}
func (s *Server) loginAllowed(ip string, now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.loginAttempts[ip]
	if now.Before(a.LockedUntil) {
		return false, time.Until(a.LockedUntil)
	}
	if a.WindowStart.IsZero() || now.Sub(a.WindowStart) > 15*time.Minute {
		delete(s.loginAttempts, ip)
	}
	return true, 0
}

const maxLoginAttemptEntries = 4096

func (s *Server) recordLoginFailure(ip string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.loginAttempts[ip]; !exists && len(s.loginAttempts) >= maxLoginAttemptEntries {
		for key, attempt := range s.loginAttempts {
			if now.Sub(attempt.WindowStart) > 15*time.Minute && now.After(attempt.LockedUntil) {
				delete(s.loginAttempts, key)
			}
		}
		if len(s.loginAttempts) >= maxLoginAttemptEntries {
			return
		}
	}
	a := s.loginAttempts[ip]
	if a.WindowStart.IsZero() || now.Sub(a.WindowStart) > 15*time.Minute {
		a = loginAttempt{WindowStart: now}
	}
	a.Failures++
	if a.Failures >= 5 {
		a.LockedUntil = now.Add(15 * time.Minute)
	}
	s.loginAttempts[ip] = a
}
func (s *Server) clearLoginFailures(ip string) {
	s.mu.Lock()
	delete(s.loginAttempts, ip)
	s.mu.Unlock()
}
func (s *Server) adminChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, 401, "auth_error", "administrator login required")
		return
	}
	var b struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&b) != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	s.mu.Lock()
	current := s.adminPassword
	s.mu.Unlock()
	if !verifyAdminPassword(b.Current, current) {
		writeOpenAIError(w, 401, "auth_error", "current password is invalid")
		return
	}
	if err := validNewAdminPassword(b.New); err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	verifier, err := newPasswordVerifier(b.New)
	if err != nil {
		writeOpenAIError(w, 500, "storage_error", "administrator password could not be prepared")
		return
	}
	if err := saveAdminPasswordVerifier(verifier); err != nil {
		writeOpenAIError(w, 500, "storage_error", "administrator password could not be saved; check the persistent data directory permissions")
		return
	}
	s.mu.Lock()
	s.adminPassword = verifier
	s.mustChangePassword = false
	s.adminSessions = map[string]time.Time{}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	jsonOut(w, map[string]any{"status": "password_changed", "reauthenticate": true})
}
