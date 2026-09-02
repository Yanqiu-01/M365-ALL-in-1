package web

import "testing"

// Lead 1 的补丁把「围栏体不是 JSON 对象」从畸形帧改成整段跳过。散文里的 ```bash
// 因此不再毒化真正的调用，这是对的；但同一个改动也让「本来想写 JSON、只是写坏了」
// 的围栏体从失败关闭退化成静默跳过。跳过之后前面那一帧（模型已经推翻的旧决策）
// 就可能被当成末帧执行，正是 Lead 3 要修的那种错。
//
// 这个测试量的就是这条边界：体以 { 开头说明它是一次 JSON 参数尝试，写坏了应当
// 失败关闭；体是 shell 文本则根本不是调用。
func TestReview_BrokenJSONFenceBodyStillFailsClosed(t *testing.T) {
	tools := testTools()

	cases := []struct {
		name string
		text string
		// wantParsed=false 表示失败关闭（进修复轮），这是坏 JSON 体应有的结果。
		wantParsed bool
		wantName   string
	}{
		{
			name: "trailing comma in fenced body",
			text: "```get_weather\n{\"city\":\"Beijing\",}\n```",
			// 明显是 JSON 参数写坏了，应当失败关闭而不是当作无事发生。
			wantParsed: false,
		},
		{
			name: "stale frame must not win after a broken JSON fence",
			text: "```get_weather\n{\"city\":\"Shanghai\"}\n```\n\nActually that city was wrong, use this:\n\n```get_weather\n{\"city\":\"Beijing\",}\n```",
			// 末帧写坏了。执行上一帧等于发出模型已经推翻的那次调用。
			wantParsed: false,
		},
		{
			name:       "prose shell fence must not poison the real call",
			text:       "CALL_TOOL: get_weather({\"city\":\"Beijing\"})\n\nFor reference this is what you would run by hand:\n\n```bash\ncurl wttr.in/Beijing\n```",
			wantParsed: true,
			wantName:   "get_weather",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, ok := parseModelToolDecision(tc.text, tools, "auto")
			if ok != tc.wantParsed {
				t.Errorf("parsed=%v want %v; calls=%v", ok, tc.wantParsed, calls)
				return
			}
			if tc.wantName != "" {
				if len(calls) != 1 || calls[0].Name != tc.wantName {
					t.Errorf("want single call to %q, got %v", tc.wantName, calls)
				}
			}
		})
	}
}
