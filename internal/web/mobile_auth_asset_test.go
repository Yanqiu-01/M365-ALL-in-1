package web

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMobilePKCEAssetUsesTrustedSamePageNavigation(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test source")
	}
	asset := filepath.Join(filepath.Dir(thisFile), "..", "..", "web", "index.html")
	body, err := os.ReadFile(asset)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"function isTrustedMicrosoftAuthorizeURL(raw){",
		"u.hostname.toLowerCase()==='login.microsoftonline.com'",
		"window.location.assign(d.url);",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("mobile authorization asset missing %q", required)
		}
	}
	if strings.Contains(text, "window.open(d.url") {
		t.Fatal("mobile authorization asset still opens an unchecked popup URL")
	}
}
