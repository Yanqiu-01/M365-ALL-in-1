package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// adminPasswordEnv names the environment variable that carries the admin
// password injected into the patched APK. The password is deliberately NOT
// compiled into this repository: keeping it in source would publish a live
// credential to anyone who can read the history.
const adminPasswordEnv = "M365_ADMIN_PASSWORD"

// adminPassword resolves the password to inject, preferring the environment
// variable and falling back to an existing recovery file from a previous run.
func adminPassword(secretFile string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(adminPasswordEnv)); v != "" {
		return v, nil
	}
	if secretFile != "" {
		if raw, err := os.ReadFile(secretFile); err == nil {
			if v := strings.TrimSpace(string(raw)); v != "" {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("%s is not set: export it (or keep the recovery file at %s) before patching", adminPasswordEnv, secretFile)
}

func patchAdminPassword(work, secretFile string) error {
	password, err := adminPassword(secretFile)
	if err != nil {
		return err
	}
	stringsFile := filepath.Join(work, "res", "values", "strings.xml")
	body, err := os.ReadFile(stringsFile)
	if err != nil {
		return fmt.Errorf("Android string resources not found: %w", err)
	}
	re := regexp.MustCompile(`<string name="default_admin_password">[^<]*</string>`)
	if !re.Match(body) {
		return errors.New("default_admin_password resource not found")
	}
	// The password is user-supplied, so escape it for XML and substitute it
	// literally: ReplaceAllString would expand $1-style sequences inside it.
	var escaped strings.Builder
	if err := xml.EscapeText(&escaped, []byte(password)); err != nil {
		return fmt.Errorf("escape admin password: %w", err)
	}
	replacement := `<string name="default_admin_password">` + escaped.String() + `</string>`
	replaced := re.ReplaceAllLiteralString(string(body), replacement)
	if err := os.WriteFile(stringsFile, []byte(replaced), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(secretFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(secretFile, []byte(password+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote local admin password from %s; recovery file: %s\n", adminPasswordEnv, secretFile)
	return nil
}
