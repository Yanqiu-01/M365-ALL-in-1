package outbound

import "testing"

func TestPickRawURLReturnsConfiguredPoolExit(t *testing.T) {
	if err := ConfigurePool([]string{"http://114.236.137.41:21000"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigurePool(nil) })
	if got := PickRawURL(); got != "http://114.236.137.41:21000" {
		t.Fatalf("PickRawURL = %q", got)
	}
}

func TestRemoveProxyNormalizesAndRejectsMissing(t *testing.T) {
	if err := ConfigurePool([]string{"http://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProxy("http://example.com"); err != nil {
		t.Fatal(err)
	}
	if len(ProxyPoolStatus()) != 0 {
		t.Fatalf("pool not empty: %#v", ProxyPoolStatus())
	}
	if err := RemoveProxy("http://missing.example"); err == nil {
		t.Fatal("expected missing proxy error")
	}
}
