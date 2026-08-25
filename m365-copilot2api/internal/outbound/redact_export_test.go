package outbound

import "testing"

// The admin settings endpoint used to echo runtimeSettings.ProxyPool verbatim,
// which put the proxy password into the dashboard payload and the devtools
// network tab. These tests pin the exported redaction so that cannot return.
func TestRedactProxyURLMasksPassword(t *testing.T) {
	got := RedactProxyURL("http://m365exit:s3cr3t@70.153.144.122:3128")
	want := "http://m365exit:***@70.153.144.122:3128"
	if got != want {
		t.Fatalf("password not masked: got %q want %q", got, want)
	}
}

func TestRedactProxyURLKeepsEmptyEmpty(t *testing.T) {
	// A direct-dial deployment has no exit configured. Reporting "<invalid>" here
	// would make the dashboard claim a broken proxy where there is simply none.
	if got := RedactProxyURL(""); got != "" {
		t.Fatalf("empty exit must stay empty, got %q", got)
	}
}

func TestRedactProxyURLLeavesCredentiallessExitAlone(t *testing.T) {
	raw := "socks5://183.56.224.230:45307"
	if got := RedactProxyURL(raw); got != raw {
		t.Fatalf("exit without userinfo must round-trip: got %q want %q", got, raw)
	}
}

func TestRedactProxyURLsPreservesOrderAndNil(t *testing.T) {
	if RedactProxyURLs(nil) != nil {
		t.Fatal("nil must stay nil so callers can tell unset from empty")
	}
	in := []string{"socks5://1.1.1.1:1080", "http://u:p@2.2.2.2:3128"}
	got := RedactProxyURLs(in)
	if len(got) != 2 || got[0] != in[0] || got[1] != "http://u:***@2.2.2.2:3128" {
		t.Fatalf("unexpected redaction result: %#v", got)
	}
	// The stored value must remain dialable; redaction is output-only.
	if in[1] != "http://u:p@2.2.2.2:3128" {
		t.Fatalf("input was mutated: %q", in[1])
	}
}
