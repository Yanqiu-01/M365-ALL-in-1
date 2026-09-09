package web

import "testing"

func TestToolArgumentsJSON(t *testing.T) {
	cases := []struct {
		name string
		call map[string]any
		want string
	}{
		{"string passthrough",
			map[string]any{"function": map[string]any{"arguments": `{"path":"a.py"}`}},
			`{"path":"a.py"}`},
		{"object marshalled",
			map[string]any{"function": map[string]any{"arguments": map[string]any{"path": "a.py"}}},
			`{"path":"a.py"}`},
		{"nil arguments",
			map[string]any{"function": map[string]any{"arguments": nil}},
			""},
		{"missing arguments",
			map[string]any{"function": map[string]any{}},
			""},
		{"missing function",
			map[string]any{"id": "call_1"},
			""},
		{"empty call",
			map[string]any{},
			""},
	}
	for _, tc := range cases {
		if got := toolArgumentsJSON(tc.call); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// APK 的关键字表实测 22 项（全局切片 0x1be6000+2656）。
func TestToolLooksObservational(t *testing.T) {
	for _, name := range []string{
		"read_file", "list_files", "get_status", "search_code", "find_symbol",
		"fetch_url", "inspect_state", "stat_path", "status", "describe_table",
		"info", "run_tests", "check_syntax", "verify_hash", "validate_schema",
		"browser_open", "lookup_id", "diff_files", "log_tail", "show_config",
		"view_source", "grep_repo",
	} {
		if !toolLooksObservational(name) {
			t.Errorf("%q should be observational", name)
		}
	}
	for _, name := range []string{"write_file", "delete_path", "apply_patch", "rename_symbol"} {
		if toolLooksObservational(name) {
			t.Errorf("%q must not be observational", name)
		}
	}
	// 大小写与空白归一化。
	if !toolLooksObservational("  READ_FILE  ") {
		t.Error("name should be trimmed and lowercased")
	}
}

// toolCanRepeatSameArguments 的极性契约（2026-09-09 起替代已删除的
// shouldSuppressCompletedCall 测试：那个名字按相反极性解释同一判据，
// 属死代码已删）。
func TestToolCanRepeatSameArgumentsPolarity(t *testing.T) {
	for _, name := range []string{"read_file", "list_files", "grep_repo"} {
		if !toolCanRepeatSameArguments(name) {
			t.Errorf("%q should be repeatable", name)
		}
	}
	for _, name := range []string{"write_file", "apply_patch", "delete_path"} {
		if toolCanRepeatSameArguments(name) {
			t.Errorf("%q must not be repeatable", name)
		}
	}
}
