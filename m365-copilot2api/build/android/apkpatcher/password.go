package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const defaultAdminPassword = "***REMOVED-CREDENTIAL***"

func patchAdminPassword(work, secretFile string) error {
	stringsFile := filepath.Join(work, "res", "values", "strings.xml")
	body, err := os.ReadFile(stringsFile)
	if err != nil {
		return fmt.Errorf("Android string resources not found: %w", err)
	}
	re := regexp.MustCompile(`<string name="default_admin_password">[^<]*</string>`)
	if !re.Match(body) {
		return fmt.Errorf("default_admin_password resource not found")
	}
	replaced := re.ReplaceAllString(string(body), `<string name="default_admin_password">`+defaultAdminPassword+`</string>`)
	if err := os.WriteFile(stringsFile, []byte(replaced), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(secretFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(secretFile, []byte(defaultAdminPassword+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote local admin password; recovery file: %s\n", secretFile)
	return nil
}
