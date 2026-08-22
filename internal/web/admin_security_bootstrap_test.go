package web

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapPasswordIsVerifiedWithoutPlaintextPersistence(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "data", "admin-password")
	bootstrap := filepath.Join(dir, "bootstrap")
	password := strings.Repeat("b", 24)
	t.Setenv("M365_DATA_DIR", filepath.Dir(persisted))
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", bootstrap)
	t.Setenv("M365_ADMIN_PASSWORD", "")
	if err := os.WriteFile(bootstrap, []byte(password+"\n"), 0400); err != nil {
		t.Fatal(err)
	}
	verifier, mustChange := loadAdminPassword()
	if mustChange || !verifyAdminPassword(password, verifier) {
		t.Fatal("bootstrap password was not converted to a verifier")
	}
	if _, err := os.Stat(persisted); !os.IsNotExist(err) {
		t.Fatal("bootstrap password was unexpectedly persisted")
	}
	if bytes.Contains([]byte(verifier), []byte(password)) {
		t.Fatal("verifier contains the plaintext bootstrap password")
	}
}

func TestPersistedVerifierOverridesBootstrapPassword(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "admin-password")
	bootstrap := filepath.Join(dir, "bootstrap")
	persistedPassword := strings.Repeat("s", 24)
	bootstrapPassword := strings.Repeat("t", 24)
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", bootstrap)
	t.Setenv("M365_ADMIN_PASSWORD", "")
	if err := saveAdminPassword(persistedPassword); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrap, []byte(bootstrapPassword+"\n"), 0400); err != nil {
		t.Fatal(err)
	}
	verifier, mustChange := loadAdminPassword()
	if mustChange || !verifyAdminPassword(persistedPassword, verifier) || verifyAdminPassword(bootstrapPassword, verifier) {
		t.Fatal("persisted verifier precedence is incorrect")
	}
}
