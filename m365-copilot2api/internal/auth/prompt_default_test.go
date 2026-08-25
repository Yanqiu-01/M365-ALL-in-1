package auth

import (
	"net/url"
	"strings"
	"testing"
)

// PC 版默认强制重新登录：不设 M365_PROMPT 时也要带上 login。
// select_account 仍会沿用单一已登录会话，换不了账号。
func TestPromptDefaultsToLogin(t *testing.T) {
	t.Setenv("M365_PROMPT", "")
	// 空字符串是显式设置，必须原样保留（AuthorizationURL 会丢掉空值）。
	if got := Prompt(); got != "" {
		t.Fatalf("explicit empty M365_PROMPT = %q, want empty", got)
	}
}

func TestPromptUnsetUsesDefault(t *testing.T) {
	if DefaultPrompt != "login" {
		t.Fatalf("DefaultPrompt = %q, want login", DefaultPrompt)
	}
	if got := Prompt(); got != DefaultPrompt {
		t.Fatalf("Prompt() = %q, want %q", got, DefaultPrompt)
	}
}

func TestPromptHonoursExplicitOverride(t *testing.T) {
	for _, want := range []string{"login", "consent", "none"} {
		t.Setenv("M365_PROMPT", want)
		if got := Prompt(); got != want {
			t.Fatalf("Prompt() = %q, want %q", got, want)
		}
	}
}

func TestAuthorizationURLCarriesDefaultPrompt(t *testing.T) {
	raw := AuthorizationURL("https://example.invalid/authorize", "cid", "http://127.0.0.1/cb", "st", "ch", "openid")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("prompt"); got != "login" {
		t.Fatalf("prompt = %q, want login", got)
	}
}

func TestLogoutURLFollowsAuthority(t *testing.T) {
	t.Setenv("M365_AUTHORITY", "")
	t.Setenv("M365_LOGOUT_ENDPOINT", "")
	if got := LogoutURL(); !strings.HasPrefix(got, DefaultAuthority) || !strings.HasSuffix(got, "/oauth2/v2.0/logout") {
		t.Fatalf("LogoutURL() = %q", got)
	}
	t.Setenv("M365_AUTHORITY", "https://login.microsoftonline.com/organizations")
	if got := LogoutURL(); got != "https://login.microsoftonline.com/organizations/oauth2/v2.0/logout" {
		t.Fatalf("LogoutURL() = %q", got)
	}
	t.Setenv("M365_LOGOUT_ENDPOINT", "https://example.invalid/signout")
	if got := LogoutURL(); got != "https://example.invalid/signout" {
		t.Fatalf("LogoutURL() = %q", got)
	}
}
