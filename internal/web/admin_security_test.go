package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func syntheticAdminPassword() string { return strings.Repeat("p", 24) }

func setupAdminTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_CONFIG", filepath.Join(dir, "accounts.json"))
	t.Setenv("M365_API_KEYS", filepath.Join(dir, "api-keys.json"))
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(dir, "admin-password"))
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", "")
	return dir
}

func adminTestClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	t.Cleanup(ts.Close)
	return ts, c
}

func postJSON(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMissingAdminPasswordFailsClosed(t *testing.T) {
	setupAdminTestEnv(t)
	verifier, mustChange := loadAdminPassword()
	if verifier != "" || !mustChange {
		t.Fatalf("missing password did not fail closed: configured=%v mustChange=%v", verifier != "", mustChange)
	}
}

func TestAdminPasswordIsHashedAtRest(t *testing.T) {
	setupAdminTestEnv(t)
	password := syntheticAdminPassword()
	if err := saveAdminPassword(password); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(adminPasswordPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(password)) {
		t.Fatal("administrator password was written in plaintext")
	}
	verifier, mustChange := loadAdminPassword()
	if mustChange || !verifyAdminPassword(password, verifier) {
		t.Fatal("stored administrator password verifier did not validate")
	}
}

func TestLegacyAdminPasswordFileMigratesWithoutPlaintextPersistence(t *testing.T) {
	setupAdminTestEnv(t)
	password := syntheticAdminPassword()
	if err := os.WriteFile(adminPasswordPath(), []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	verifier, mustChange := loadAdminPassword()
	if mustChange || !verifyAdminPassword(password, verifier) {
		t.Fatal("legacy administrator password was not migrated")
	}
	stored, err := os.ReadFile(adminPasswordPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(password)) {
		t.Fatal("legacy administrator password remained in plaintext")
	}
}

func TestConfiguredAdminPasswordLoginAndChange(t *testing.T) {
	setupAdminTestEnv(t)
	oldPassword := syntheticAdminPassword()
	newPassword := strings.Repeat("n", 24)
	t.Setenv("M365_ADMIN_PASSWORD", oldPassword)
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, client := adminTestClient(t, s.Routes())
	response := postJSON(t, client, ts.URL+"/api/admin/login", map[string]string{"password": oldPassword})
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("configured login status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = postJSON(t, client, ts.URL+"/api/admin/change-password", map[string]string{"current_password": oldPassword, "new_password": newPassword})
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("password change status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = postJSON(t, client, ts.URL+"/api/admin/login", map[string]string{"password": newPassword})
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("re-login status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func TestAdminLoginLocksAfterFiveFailures(t *testing.T) {
	setupAdminTestEnv(t)
	t.Setenv("M365_ADMIN_PASSWORD", syntheticAdminPassword())
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, client := adminTestClient(t, s.Routes())
	wrongPassword := strings.Repeat("q", 24)
	for i := 0; i < 5; i++ {
		response := postJSON(t, client, ts.URL+"/api/admin/login", map[string]string{"password": wrongPassword})
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d", i+1, response.StatusCode)
		}
	}
	response := postJSON(t, client, ts.URL+"/api/admin/login", map[string]string{"password": wrongPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked login status=%d", response.StatusCode)
	}
}

func TestExpiredLoginWindowResets(t *testing.T) {
	s := &Server{loginAttempts: map[string]loginAttempt{"synthetic": {Failures: 4, WindowStart: time.Now().Add(-16 * time.Minute)}}}
	if ok, _ := s.loginAllowed("synthetic", time.Now()); !ok {
		t.Fatal("expired login window remained locked")
	}
}
