package web

import "testing"

// 邮箱编号解析：24s055026@office.bo.edu.kg → 5026。
func TestNativePanelEmailNum(t *testing.T) {
	cases := []struct {
		email, prefix string
		want          int
		ok            bool
	}{
		{"24s055026@office.bo.edu.kg", "24s05", 5026, true},
		{"24s055026@office.bo.edu.kg", "", 55026, true}, // 无前缀时取末尾连续数字
		{"24s055026", "24s05", 5026, true},              // 不带域名
		{"user@example.com", "24s05", 0, false},         // 无数字
		{"", "24s05", 0, false},
		// prefix 段解析出 0（无效）后回落到「末尾连续数字」，得到 50000。
		{"24s050000@x.y", "24s05", 50000, true},
		{"abc0@x.y", "zzz", 0, false}, // 回落路径同样拒绝编号 0
	}
	for _, c := range cases {
		got, ok := nativePanelEmailNum(c.email, c.prefix)
		if ok != c.ok || got != c.want {
			t.Errorf("nativePanelEmailNum(%q,%q) = (%d,%v), want (%d,%v)", c.email, c.prefix, got, ok, c.want, c.ok)
		}
	}
}

// 区间归一化与拒绝路径。
func TestResolveEmailRange(t *testing.T) {
	// 两者都缺省 → provided=false，调用方回落旧语义。
	if _, provided, err := resolveEmailRange(0, 0, 5); provided || err != nil {
		t.Fatalf("empty range should not be provided, got provided=%v err=%v", provided, err)
	}
	// 只给 startNum，用 count 推导 endNum。
	r, provided, err := resolveEmailRange(5026, 0, 10)
	if !provided || err != nil || r.Start != 5026 || r.End != 5035 || r.count() != 10 {
		t.Fatalf("startNum+count = %+v provided=%v err=%v", r, provided, err)
	}
	// 显式闭区间。
	r, _, err = resolveEmailRange(5026, 5100, 0)
	if err != nil || r.count() != 75 {
		t.Fatalf("explicit range count = %d err=%v, want 75", r.count(), err)
	}
	// 拒绝：end < start
	if _, _, err = resolveEmailRange(5100, 5026, 0); err == nil {
		t.Error("end < start must be rejected")
	}
	// 拒绝：缺少 startNum
	if _, _, err = resolveEmailRange(0, 5100, 0); err == nil {
		t.Error("missing startNum must be rejected")
	}
	// 拒绝：区间过大
	if _, _, err = resolveEmailRange(1, 5000, 0); err == nil {
		t.Error("oversized range must be rejected")
	}
}

// --start 语义翻译：phone/clash 收绝对编号，proxy 收 1-based 序号。
func TestRegisterArgsForRange(t *testing.T) {
	r := nativePanelEmailRange{Start: 5026, End: 5030}

	start, limit, err := registerArgsForRange(r, 5528, true)
	if err != nil || start != 5026 || limit != 5 {
		t.Fatalf("absolute: start=%d limit=%d err=%v, want 5026/5", start, limit, err)
	}

	// 序号模式：基准 5000 → 5026 是第 27 个。
	start, limit, err = registerArgsForRange(r, 5000, false)
	if err != nil || start != 27 || limit != 5 {
		t.Fatalf("positional: start=%d limit=%d err=%v, want 27/5", start, limit, err)
	}

	// 编号早于基准时无法用序号表达，必须报错而不是静默注册错账号。
	if _, _, err = registerArgsForRange(r, 6000, false); err == nil {
		t.Error("startNum below email_start_num must be rejected in positional mode")
	}
}

// 按编号区间从账密清单里选账号，并换算成脚本要的 1-based 下标。
func TestSelectEmailsInRange(t *testing.T) {
	creds := map[string]string{
		"24s055024@d": "p", // 5024
		"24s055026@d": "p", // 5026
		"24s055027@d": "p", // 5027
		"24s055030@d": "p", // 5030
		"24s055099@d": "p", // 5099
	}
	matched, position, err := selectEmailsInRange(creds, "24s05", nativePanelEmailRange{Start: 5026, End: 5030})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matched) != 3 {
		t.Fatalf("matched = %v, want 3 entries", matched)
	}
	// 排序后清单为 [5024,5026,5027,5030,5099]，5026 是第 2 个。
	if position != 2 {
		t.Errorf("position = %d, want 2", position)
	}
	// 空区间必须报错，而不是启动一个什么都不做的任务。
	if _, _, err = selectEmailsInRange(creds, "24s05", nativePanelEmailRange{Start: 9000, End: 9100}); err == nil {
		t.Error("range matching zero accounts must be rejected")
	}
	if _, _, err = selectEmailsInRange(map[string]string{}, "24s05", nativePanelEmailRange{Start: 1, End: 2}); err == nil {
		t.Error("empty credential list must be rejected")
	}
}

func TestCredentialEmailNumBounds(t *testing.T) {
	creds := map[string]string{"24s055026@d": "p", "24s055099@d": "p", "bad@d": "p"}
	min, max, ok := credentialEmailNumBounds(creds, "24s05")
	if !ok || min != 5026 || max != 5099 {
		t.Errorf("bounds = (%d,%d,%v), want (5026,5099,true)", min, max, ok)
	}
	if _, _, ok = credentialEmailNumBounds(map[string]string{}, "24s05"); ok {
		t.Error("empty list should report ok=false")
	}
}
