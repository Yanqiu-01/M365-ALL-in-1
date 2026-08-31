package web

import (
	"strings"
	"testing"
)

// operationalSettingsBase 返回一份合法的基线设置，供逐字段改坏。
func operationalSettingsBase(t *testing.T) runtimeSettings {
	t.Helper()
	// 清掉可能污染默认值的环境变量，让基线自身一定合法。
	for _, key := range []string{
		"M365_MAX_TOOL_CALLS_PER_TURN", "M365_MAX_TOOL_ROUNDS", "M365_CONTEXT_WINDOW",
		"M365_MAX_OUTPUT_TOKENS", "M365_CHAT_TIMEOUT_SECONDS", "M365_IMAGE_TIMEOUT_SECONDS",
		"M365_LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
	v := defaultRuntimeSettings()
	v.OutboundProxy = ""
	v.ProxyPool = nil
	if err := validateSettings(v); err != nil {
		t.Fatalf("baseline settings are not valid: %v", err)
	}
	return v
}

// 为管理台补的那批运行参数以前一个都不校验。它们经 ApplyStartupSettingsEnv 变成
// 环境变量，而每个消费方遇到越界值都是「忽略并用默认」—— 于是保存
// proxyGuardConcurrency=9999 会返回成功、写进 settings.json、界面上显示 9999，
// 实际跑的却是默认 6。界面显示的配置和进程真正使用的配置就此长期不一致。
func TestValidateSettingsRejectsOutOfRangeOperationalValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*runtimeSettings)
	}{
		{"proxyGuardConcurrency_above_ceiling", func(v *runtimeSettings) { v.ProxyGuardConcurrency = 9999 }},
		{"proxyGuardConcurrency_negative", func(v *runtimeSettings) { v.ProxyGuardConcurrency = -1 }},
		{"proxyGuardInterval_too_short", func(v *runtimeSettings) { v.ProxyGuardInterval = "1s" }},
		{"proxyGuardInterval_too_long", func(v *runtimeSettings) { v.ProxyGuardInterval = "48h" }},
		{"proxyGuardInterval_nonsense", func(v *runtimeSettings) { v.ProxyGuardInterval = "nonsense" }},
		{"maxConcurrentChats_above_ceiling", func(v *runtimeSettings) { v.MaxConcurrentChats = maxConfiguredChats + 1 }},
		{"maxConcurrentChats_negative", func(v *runtimeSettings) { v.MaxConcurrentChats = -5 }},
		{"accountConcurrency_above_ceiling", func(v *runtimeSettings) { v.AccountConcurrency = maxAccountConcurrency + 1 }},
		{"accountConcurrency_negative", func(v *runtimeSettings) { v.AccountConcurrency = -2 }},
		{"autoCleanupIntervalMinutes_negative", func(v *runtimeSettings) { v.AutoCleanupIntervalMinutes = -30 }},
		{"autoCleanupIntervalMinutes_absurd", func(v *runtimeSettings) { v.AutoCleanupIntervalMinutes = 100000 }},
		{"autoCleanupMaxAgeHours_negative", func(v *runtimeSettings) { v.AutoCleanupMaxAgeHours = -1 }},
		{"autoCleanupMaxAgeHours_absurd", func(v *runtimeSettings) { v.AutoCleanupMaxAgeHours = 1000000 }},
		{"autoCleanupKeepN_negative", func(v *runtimeSettings) { v.AutoCleanupKeepN = -1 }},
		{"autoCleanupKeepN_absurd", func(v *runtimeSettings) { v.AutoCleanupKeepN = maxCleanupKeepN + 1 }},
		{"persistInterval_below_floor", func(v *runtimeSettings) { v.PersistInterval = "1ms" }},
		{"persistInterval_too_long", func(v *runtimeSettings) { v.PersistInterval = "24h" }},
		{"persistInterval_nonsense", func(v *runtimeSettings) { v.PersistInterval = "soon" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := operationalSettingsBase(t)
			tc.mutate(&v)
			if err := validateSettings(v); err == nil {
				t.Fatal("out-of-range operational setting was accepted and would take effect as a silent default")
			}
		})
	}
}

// 零值/空串必须继续放行：settingsEnvOverrides 只注入非零值，
// 「零值 = 沿用内置默认」是这批字段既有的语义。
func TestValidateSettingsAcceptsUnsetOperationalValues(t *testing.T) {
	v := operationalSettingsBase(t)
	v.ProxyGuardInterval = ""
	v.ProxyGuardConcurrency = 0
	v.MaxConcurrentChats = 0
	v.AccountConcurrency = 0
	v.AutoCleanupIntervalMinutes = 0
	v.AutoCleanupMaxAgeHours = 0
	v.AutoCleanupKeepN = 0
	v.PersistInterval = ""
	if err := validateSettings(v); err != nil {
		t.Fatalf("unset operational settings were rejected: %v", err)
	}
}

// 合法的边界值必须被接受，不能把校验做成一刀切。
func TestValidateSettingsAcceptsInRangeOperationalValues(t *testing.T) {
	v := operationalSettingsBase(t)
	v.ProxyGuardInterval = "5s"
	v.ProxyGuardConcurrency = maxProxyGuardConcurrency
	v.MaxConcurrentChats = maxConfiguredChats
	v.AccountConcurrency = maxAccountConcurrency
	v.AutoCleanupIntervalMinutes = 1440
	v.AutoCleanupMaxAgeHours = 8760
	v.AutoCleanupKeepN = maxCleanupKeepN
	v.PersistInterval = "100ms"
	if err := validateSettings(v); err != nil {
		t.Fatalf("in-range operational settings were rejected: %v", err)
	}

	// 巡检间隔也接受裸秒数，与 outbound.guardTimingFromEnv 的解析一致。
	v.ProxyGuardInterval = "120"
	if err := validateSettings(v); err != nil {
		t.Fatalf("bare-seconds guard interval was rejected: %v", err)
	}
}

// 布尔开关没有取值范围，不得被误拦。
func TestValidateSettingsAcceptsOperationalToggles(t *testing.T) {
	v := operationalSettingsBase(t)
	v.ProxyGuardDisabled = true
	v.AllowFakeIPSource = true
	v.AutoCleanupDisabled = true
	v.HTTPTraceVerbose = true
	if err := validateSettings(v); err != nil {
		t.Fatalf("operational toggles were rejected: %v", err)
	}
}

// 每一个走 settingsEnvOverrides 的数值/时长字段都必须有校验覆盖。
// 这条断言防的是「以后又加一个设置项、又忘了校验」重演一次同样的缺陷。
func TestEveryOperationalNumericSettingIsValidated(t *testing.T) {
	unchecked := []struct {
		field  string
		mutate func(*runtimeSettings)
	}{
		{"proxyGuardInterval", func(v *runtimeSettings) { v.ProxyGuardInterval = "99h" }},
		{"proxyGuardConcurrency", func(v *runtimeSettings) { v.ProxyGuardConcurrency = 1 << 20 }},
		{"maxConcurrentChats", func(v *runtimeSettings) { v.MaxConcurrentChats = 1 << 20 }},
		{"accountConcurrency", func(v *runtimeSettings) { v.AccountConcurrency = 1 << 20 }},
		{"autoCleanupIntervalMinutes", func(v *runtimeSettings) { v.AutoCleanupIntervalMinutes = 1 << 20 }},
		{"autoCleanupMaxAgeHours", func(v *runtimeSettings) { v.AutoCleanupMaxAgeHours = 1 << 20 }},
		{"autoCleanupKeepN", func(v *runtimeSettings) { v.AutoCleanupKeepN = 1 << 20 }},
		{"persistInterval", func(v *runtimeSettings) { v.PersistInterval = "0s" }},
	}
	var missing []string
	for _, item := range unchecked {
		v := operationalSettingsBase(t)
		item.mutate(&v)
		if err := validateSettings(v); err == nil {
			missing = append(missing, item.field)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("these operational settings accept an absurd value unvalidated: %s", strings.Join(missing, ", "))
	}
}

// 校验必须在保存路径上生效，而不只是一个能过单测的纯函数。
func TestSettingsStoreRejectsInvalidOperationalValuesOnSave(t *testing.T) {
	dir := t.TempDir()
	store := &settingsStore{path: dir + "/settings.json", v: operationalSettingsBase(t)}
	bad := store.v
	bad.MaxConcurrentChats = 100000
	if err := store.save(bad); err == nil {
		t.Fatal("save accepted an out-of-range maxConcurrentChats")
	}
	if got := store.get().MaxConcurrentChats; got == 100000 {
		t.Fatal("rejected value was applied anyway")
	}
}
