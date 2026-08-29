package main

import (
	"fmt"
	"os"
	"strings"
)

func replaceOnce(text, old, new, label string) (string, error) {
	count := strings.Count(text, old)
	if count != 1 {
		return "", fmt.Errorf("%s: expected exactly one match, found %d", label, count)
	}
	return strings.Replace(text, old, new, 1), nil
}

func readFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func writeFile(path, text string) error {
	return os.WriteFile(path, []byte(text), 0o644)
}

func mustContain(text, needle, label string) error {
	if !strings.Contains(text, needle) {
		return fmt.Errorf("%s: missing %q", label, needle)
	}
	return nil
}

func mustNotContain(text, needle, label string) error {
	if strings.Contains(text, needle) {
		return fmt.Errorf("%s: still contains %q", label, needle)
	}
	return nil
}
