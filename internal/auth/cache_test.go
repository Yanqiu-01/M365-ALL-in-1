package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertAndList(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Upsert(TokenSet{
		AccessToken:  "a",
		RefreshToken: "r",
		Email:        "a@example.com",
		DisplayName:  "A",
		HomeOID:      "oid-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Email != "a@example.com" {
		t.Fatalf("unexpected email: %s", acc.Email)
	}
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 account, got %d", len(list))
	}
}

func TestEncryptedStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "m365-store.key")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	encoded := base64.URLEncoding.EncodeToString(key)
	if err := os.WriteFile(keyPath, []byte(encoded+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "accounts.json")
	t.Setenv("M365_STORE_KEY_FILE", keyPath)
	body, err := json.MarshalIndent(Cache{Accounts: []AccountToken{{
		ID: "acct-1", Email: "one@example.com", RefreshToken: "refresh-secret",
		AccessToken: "access-secret", ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
	}}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := encryptStoreBody(body, path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("refresh-secret")) || bytes.Contains(encrypted, []byte("access-secret")) {
		t.Fatal("encrypted store contains token plaintext")
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.List(); len(got) != 1 || got[0].RefreshToken != "refresh-secret" {
		t.Fatalf("decrypted accounts = %#v", got)
	}
	if err := store.UpdateRefreshToken("acct-1", "rotated-refresh"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.List(); len(got) != 1 || got[0].RefreshToken != "rotated-refresh" {
		t.Fatalf("reopened accounts = %#v", got)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte("rotated-refresh")) {
		t.Fatal("rotated refresh token persisted in plaintext")
	}
}
