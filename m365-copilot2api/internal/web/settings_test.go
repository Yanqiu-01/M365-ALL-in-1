package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultRuntimeSettingsMatchOriginalAPK(t *testing.T) {
	for _, name := range []string{
		"M365_MAX_TOOL_CALLS_PER_TURN", "M365_MAX_TOOL_ROUNDS", "M365_CONTEXT_WINDOW",
		"M365_MAX_OUTPUT_TOKENS", "M365_CHAT_TIMEOUT_SECONDS", "M365_IMAGE_TIMEOUT_SECONDS",
	} {
		t.Setenv(name, "")
	}
	got := defaultRuntimeSettings()
	if got.MaxToolCallsPerTurn != 32 || got.MaxToolRounds != 512 || got.ContextWindow != 262144 || got.MaxOutputTokens != 16384 || got.ChatTimeoutSeconds != 600 || got.ImageTimeoutSeconds != 180 {
		t.Fatalf("original APK defaults not restored: %#v", got)
	}
	if got.ClientProfile != "office" {
		t.Fatalf("client profile=%q, want office", got.ClientProfile)
	}
	if len(got.ModelMappings) != 3 {
		t.Fatalf("model mappings=%#v", got.ModelMappings)
	}
	for _, mapping := range got.ModelMappings {
		if mapping.UpstreamTone != "Gpt_5_6_Reasoning" || mapping.DefaultReasoningLevel != "xhigh" {
			t.Fatalf("model mapping=%#v", mapping)
		}
	}
}

func TestLimitToolCalls(t *testing.T) {
	calls := []detectedToolCall{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := limitToolCalls(calls, 1)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("got %#v", got)
	}
	if len(limitToolCalls(calls, 2)) != 2 {
		t.Fatal("expected two calls")
	}
	if len(limitToolCalls(calls, 99)) != 3 {
		t.Fatal("must preserve calls below limit")
	}
}
func TestSettingsPersistAndValidate(t *testing.T) {
	s := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), v: defaultRuntimeSettings()}
	v := s.v
	v.MaxToolCallsPerTurn = 1
	v.MaxToolRounds = 32
	v.ChatTimeoutSeconds = 60
	v.ImageTimeoutSeconds = 90
	if err := s.save(v); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path); err != nil {
		t.Fatal(err)
	}
	v.MaxToolCallsPerTurn = 0
	if err := s.save(v); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestModelMappingsValidate(t *testing.T) {
	v := defaultRuntimeSettings()
	v.ModelMappings = []modelMapping{{PublicModel: "gpt-5.6-sol", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "low"}}
	if err := validateSettings(v); err != nil {
		t.Fatal(err)
	}
	v.ModelMappings[0].UpstreamTone = "unknown"
	if err := validateSettings(v); err == nil {
		t.Fatal("accepted unknown upstream tone")
	}
	v.ModelMappings[0].UpstreamTone = "Gpt_5_6_Reasoning"
	v.ModelMappings = append(v.ModelMappings, v.ModelMappings[0])
	if err := validateSettings(v); err == nil {
		t.Fatal("accepted duplicate public model")
	}
	v.ModelMappings = []modelMapping{{PublicModel: "custom-codex-route", UpstreamTone: "Gpt_5_6_Reasoning", DisplayName: "Custom Codex Route", DefaultReasoningLevel: "medium"}}
	if err := validateSettings(v); err != nil {
		t.Fatalf("rejected custom public model: %v", err)
	}
}

func TestOutboundProxySettingValidation(t *testing.T) {
	v := defaultRuntimeSettings()
	v.OutboundProxy = "socks5://proxy.example:1080"
	if err := validateSettings(v); err != nil {
		t.Fatalf("rejected SOCKS5 proxy: %v", err)
	}
	v.OutboundProxy = "https://proxy.example:8443"
	if err := validateSettings(v); err != nil {
		t.Fatalf("rejected HTTPS proxy: %v", err)
	}
	v.OutboundProxy = "ftp://proxy.example:21"
	if err := validateSettings(v); err == nil {
		t.Fatal("accepted unsupported proxy scheme")
	}
}

// 2026-09-08 规则重写后的期望：判定按批内 shell 类调用条数，而不是
// 「任一可变异工具就整批串行」。见 adaptiveToolCallLimit 的注释。
func TestAdaptiveToolCallLimitSerializesMultipleShellCalls(t *testing.T) {
	calls := []detectedToolCall{{Name: "bash"}, {Name: "bash"}}
	if got := adaptiveToolCallLimit(calls, 4); got != 1 {
		t.Fatalf("two shell calls must serialize, got %d", got)
	}
}

func TestAdaptiveToolCallLimitAllowsReadPlusOneShell(t *testing.T) {
	// read+bash 并行：读类不共享 shell 会话状态，一条 shell 与读类互不干扰。
	// 旧规则把这条组合串成 1，是「一次只能调用一个工具」的直接原因。
	calls := []detectedToolCall{{Name: "read_file"}, {Name: "bash"}}
	if got := adaptiveToolCallLimit(calls, 4); got != 4 {
		t.Fatalf("read+one-shell should parallelize, got %d", got)
	}
}

func TestAdaptiveToolCallLimitAllowsOneSubagentWithReads(t *testing.T) {
	// task（子组）+ 读类并行：子组调度本身不占 shell 会话。
	calls := []detectedToolCall{{Name: "task"}, {Name: "read_file"}, {Name: "glob"}}
	if got := adaptiveToolCallLimit(calls, 4); got != 4 {
		t.Fatalf("task+reads should parallelize, got %d", got)
	}
}

func TestAdaptiveToolCallLimitAllowsIndependentReadOnlyCalls(t *testing.T) {
	calls := []detectedToolCall{{Name: "read_file"}, {Name: "search_code"}}
	if got := adaptiveToolCallLimit(calls, 4); got != 4 {
		t.Fatalf("got %d, want 4", got)
	}
}
