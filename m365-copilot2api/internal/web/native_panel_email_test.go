package web

import "testing"

func TestNativePanelEmailNum(t *testing.T) {
	cases := []struct {
		email, prefix string
		want          int
		ok            bool
	}{
		{"24s055026@office.bo.edu.kg", "24s05", 5026, true},
		{"24s055026@office.bo.edu.kg", "", 55026, true},
		{"24s055026", "24s05", 5026, true},
		{"user@example.com", "24s05", 0, false},
		{"", "24s05", 0, false},
		{"24s05abc@office.bo.edu.kg", "24s05", 0, false},
	}
	for _, c := range cases {
		got, ok := nativePanelEmailNum(c.email, c.prefix)
		if got != c.want || ok != c.ok {
			t.Errorf("nativePanelEmailNum(%q,%q) = (%d,%v), want (%d,%v)", c.email, c.prefix, got, ok, c.want, c.ok)
		}
	}
}

func TestCredentialEmailNumBounds(t *testing.T) {
	creds := map[string]string{
		"24s055026@office.bo.edu.kg": "pw",
		"24s055100@office.bo.edu.kg": "pw",
		"24s055030@office.bo.edu.kg": "pw",
		"not-an-account":             "pw",
	}
	low, high, ok := credentialEmailNumBounds(creds, "24s05")
	if !ok || low != 5026 || high != 5100 {
		t.Fatalf("bounds = (%d,%d,%v), want (5026,5100,true)", low, high, ok)
	}
	if _, _, ok = credentialEmailNumBounds(map[string]string{}, "24s05"); ok {
		t.Error("empty credential list must not report bounds")
	}
	// 无法解析编号的清单同样不应给出区间，否则界面会显示 0-0。
	if _, _, ok = credentialEmailNumBounds(map[string]string{"admin@example.com": "pw"}, ""); ok {
		t.Error("unparsable credential list must not report bounds")
	}
}
