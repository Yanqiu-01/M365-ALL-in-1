package web

import (
	"encoding/json"
	"testing"
)

// 实测背景（2026-09-02，构建 33bf3db，Claude CLI 干净跑一次 /v1/messages，
// 36 个工具、choice=auto）：
//
//	stage=router_undecidable fallback=answer text_len=274
//	substage=repair       disposition=parser_failure  parsed=false raw_candidates=0
//	substage=validated    disposition=intent_retry    parsed=true  raw_candidates=0
//	substage=intent_retry disposition=retry_exhausted parsed=false raw_candidates=0
//
// 路由轮、修复轮、约束重试轮三段全部 parsed=false。下面这些形状是当时解析
// 不了的真实输出样式：每一种都只是「同一个决策的另一种写法」，此前都要走
// 一次修复轮（多一次上游往返），失败后整个请求降级成散文。
func TestParseToleratesRealisticDecisionShapes(t *testing.T) {
	tools := extractTestTools()
	cases := []struct {
		name string
		text string
		want string
		args string
	}{
		// chathub/tool_protocol.go 的 <tools> 约定：调用就是「info string 是
		// 工具名的围栏」。答案轮的 fencedToolCalls 一直认这个形状，路由轮不认，
		// 于是模型按被教过的协议作答反而解析失败。
		{"围栏以工具名命名", "I'll read it first.\n```read_file\n{\"path\":\"a.py\"}\n```", "read_file", `{"path":"a.py"}`},
		{"围栏参数跨行", "```read_file\n{\n  \"path\": \"a.py\"\n}\n```", "read_file", `{"path":"a.py"}`},
		{"围栏无参数工具", "```run_tests\n{}\n```", "run_tests", `{}`},

		// 省掉括号是最常见的偏离：规则原文写 name({...})，模型给 name {...}。
		{"省略括号同行", `CALL_TOOL: read_file {"path":"a.py"}`, "read_file", `{"path":"a.py"}`},
		{"省略括号换行", "CALL_TOOL: read_file\n{\"path\":\"a.py\"}", "read_file", `{"path":"a.py"}`},
		{"参数在下方围栏里", "CALL_TOOL: read_file\n```json\n{\"path\":\"a.py\"}\n```", "read_file", `{"path":"a.py"}`},
		{"裸名字无参数", "CALL_TOOL: run_tests", "run_tests", `{}`},
		{"名字带引号", `CALL_TOOL: "read_file"({"path":"a.py"})`, "read_file", `{"path":"a.py"}`},
		{"Python 展开号", `CALL_TOOL: read_file(**{"path":"a.py"})`, "read_file", `{"path":"a.py"}`},

		// arguments 是 JSON 字符串 —— OpenAI 线上就是这个形状
		// （function.arguments 是字符串），训练数据里到处都是。
		{"信封参数为字符串", `{"calls":[{"name":"read_file","arguments":"{\"path\":\"a.py\"}"}]}`, "read_file", `{"path":"a.py"}`},
		{"指令参数为字符串", `CALL_TOOL: read_file("{\"path\":\"a.py\"}")`, "read_file", `{"path":"a.py"}`},

		// 修复轮只说 "JSON only"，字段名与嵌套层级模型自由发挥。
		{"裸单调用对象", `{"name":"read_file","arguments":{"path":"a.py"}}`, "read_file", `{"path":"a.py"}`},
		{"tool_calls 别名", `{"tool_calls":[{"name":"read_file","arguments":{"path":"a.py"}}]}`, "read_file", `{"path":"a.py"}`},
		{"OpenAI 嵌套形状", `{"tool_calls":[{"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.py\"}"}}]}`, "read_file", `{"path":"a.py"}`},
		{"args 别名", `{"calls":[{"name":"read_file","args":{"path":"a.py"}}]}`, "read_file", `{"path":"a.py"}`},
		{"parameters 别名", `{"calls":[{"name":"read_file","parameters":{"path":"a.py"}}]}`, "read_file", `{"path":"a.py"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, parsed := parseModelToolDecision(c.text, tools, "auto")
			if !parsed {
				t.Fatalf("not parsed:\n%s", c.text)
			}
			if len(calls) != 1 || calls[0].Name != c.want {
				t.Fatalf("calls=%+v want %s", calls, c.want)
			}
			var got, want map[string]any
			if err := json.Unmarshal(calls[0].Arguments, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(c.args), &want); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("arguments=%s want %s", calls[0].Arguments, c.args)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("arguments=%s want %s", calls[0].Arguments, c.args)
				}
			}
		})
	}
}

// 放宽解析不得放宽校验。这些输入在放宽之后仍必须 parsed=false，
// 否则 router_undecidable 兜底（散文降级为普通回答）就失效了。
func TestParseToleranceKeepsRejectingNonDecisions(t *testing.T) {
	tools := extractTestTools()
	for name, text := range map[string]string{
		"纯散文":           "我觉得应该先看看文件里有什么。",
		"只提到标记不给决策":     "我不会输出 CALL_TOOL 这种东西的。",
		"未声明工具":         `CALL_TOOL: delete_everything({"path":"/"})`,
		"未声明工具裸名字":      "CALL_TOOL: delete_everything",
		"未声明名字的围栏":      "```delete_everything\n{}\n```",
		"普通 json 围栏":    "```json\n{\"path\":\"a.py\"}\n```",
		"普通 python 围栏":  "```python\nprint(1)\n```",
		"必填参数缺失":        "CALL_TOOL: read_file",
		"参数不合 schema":   `CALL_TOOL: read_file({"path":123})`,
		"参数非 JSON 也非对象": `CALL_TOOL: read_file(path=a.py)`,
		"括号未闭合":         `CALL_TOOL: read_file({"path":"a.py"`,
		"花括号未闭合":        `CALL_TOOL: read_file {"path":"a.py"`,
		"信封无调用字段":       `{"result":"done"}`,
		"散文里的裸 name 对象": `我们要调用的是 {"name":"read_file"}，稍后再说。`,
		"tools 定义块回声":   "{\"tools\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.py\"}}]}",
		"名字与参数之间有正文":    "CALL_TOOL: read_file 但先说明一下，参数形如 {\"path\":\"a.py\"}",
		"未声明工具的信封":      `{"calls":[{"name":"delete_everything","arguments":{}}]}`,
		"未声明工具的字符串参数信封": `{"calls":[{"name":"delete_everything","arguments":"{}"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			calls, parsed := parseModelToolDecision(text, tools, "auto")
			if parsed {
				t.Fatalf("must not parse: %q -> calls=%+v", text, calls)
			}
			if len(calls) != 0 {
				t.Fatalf("must yield no calls: %q -> %+v", text, calls)
			}
		})
	}
}

// 抽取器绝不凭空造名字：即使新增了围栏与裸名字两种形态，候选里的名字也必须
// 逐字来自原文，存在性判断留给校验阶段与下游 validateDetectedToolCalls。
func TestToleranceNeverFabricatesToolNames(t *testing.T) {
	tools := extractTestTools()
	// 围栏形态的边界依赖已声明工具名，所以 tools 为空时它一个候选也不该产生。
	if got := extractFencedDecisions("```read_file\n{\"path\":\"a.py\"}\n```", nil); len(got) != 0 {
		t.Fatalf("fenced extraction with no declared tools must yield nothing, got %+v", got)
	}
	// 未声明的围栏名同样不成帧。
	if got := extractFencedDecisions("```not_a_tool\n{}\n```", tools); len(got) != 0 {
		t.Fatalf("undeclared fence name became a decision frame: %+v", got)
	}
	// 裸名字与省括号形态仍原样交出候选，不做过滤。
	cands := extractDirectiveCandidates(normalizeDecisionText(
		"CALL_TOOL: unknown_tool\nCALL_TOOL: read_file {\"path\":\"a.py\"}"))
	if len(cands) != 2 || cands[0].Name != "unknown_tool" || cands[1].Name != "read_file" {
		t.Fatalf("extractor must pass through both names verbatim: %+v", cands)
	}
	// 下游校验淘汰未声明的名字。
	if _, parsed := parseModelToolDecision("CALL_TOOL: unknown_tool", tools, "auto"); parsed {
		t.Fatal("undeclared bare name must not parse")
	}
}

// 位置语义（取最后一帧）必须覆盖新增的围栏形态，否则被推翻的方案会被执行。
func TestFencedFrameFollowsLastFrameWins(t *testing.T) {
	tools := extractTestTools()

	// 围栏在后，指令在前 —— 围栏赢。
	calls, parsed := parseModelToolDecision(
		"CALL_TOOL: run_tests({})\n改主意了：\n```read_file\n{\"path\":\"a.py\"}\n```", tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("later fenced frame must win: parsed=%v calls=%+v", parsed, calls)
	}

	// 指令在后，围栏在前 —— 指令赢。
	calls, parsed = parseModelToolDecision(
		"```read_file\n{\"path\":\"a.py\"}\n```\n改主意了：\nCALL_TOOL: run_tests({})", tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "run_tests" {
		t.Fatalf("later directive must win: parsed=%v calls=%+v", parsed, calls)
	}

	// 末尾 NO_TOOL_NEEDED 取消在前的围栏调用。
	calls, parsed = parseModelToolDecision(
		"```read_file\n{\"path\":\"a.py\"}\n```\n\nNO_TOOL_NEEDED", tools, "auto")
	if !parsed || len(calls) != 0 {
		t.Fatalf("terminal no-tool must cancel an earlier fenced call: parsed=%v calls=%+v", parsed, calls)
	}

	// 末帧围栏参数不合法时失败关闭，不得复活在前的合法调用。
	calls, parsed = parseModelToolDecision(
		"CALL_TOOL: read_file({\"path\":\"a.py\"})\n```read_file\n{\"path\":123}\n```", tools, "auto")
	if parsed || len(calls) != 0 {
		t.Fatalf("malformed final fenced frame must not revive a stale call: parsed=%v calls=%+v", parsed, calls)
	}

	// 相邻围栏属于同一帧，可一次发起多个调用。
	calls, parsed = parseModelToolDecision(
		"```read_file\n{\"path\":\"a.py\"}\n```\n```run_tests\n{}\n```", tools, "auto")
	if !parsed || len(calls) != 2 || calls[0].Name != "read_file" || calls[1].Name != "run_tests" {
		t.Fatalf("adjacent fences are one frame: parsed=%v calls=%+v", parsed, calls)
	}

	// 中间夹了正文说明前一个已被推翻，只认最后那一帧。
	calls, parsed = parseModelToolDecision(
		"```read_file\n{\"path\":\"a.py\"}\n```\n不对，应该直接跑测试。\n```run_tests\n{}\n```", tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "run_tests" {
		t.Fatalf("prose between fences must split frames: parsed=%v calls=%+v", parsed, calls)
	}
}

// tool_choice 约束对新增形态同样生效。
func TestToleranceHonoursToolChoice(t *testing.T) {
	tools := extractTestTools()
	onlyRunTests := map[string]any{"type": "function", "function": map[string]any{"name": "run_tests"}}

	for _, text := range []string{
		"```read_file\n{\"path\":\"a.py\"}\n```",
		`CALL_TOOL: read_file {"path":"a.py"}`,
		`{"name":"read_file","arguments":{"path":"a.py"}}`,
	} {
		if _, parsed := parseModelToolDecision(text, tools, onlyRunTests); parsed {
			t.Errorf("tool_choice must reject a different tool: %q", text)
		}
	}
	// required 下裸名字仍要能成一次调用。
	calls, parsed := parseModelToolDecision("```run_tests\n{}\n```", tools, "required")
	if !parsed || len(calls) != 1 || calls[0].Name != "run_tests" {
		t.Fatalf("required + fenced call: parsed=%v calls=%+v", parsed, calls)
	}
	// required 下的空决策仍必须失败。
	if _, parsed := parseModelToolDecision("NO_TOOL_NEEDED", tools, "required"); parsed {
		t.Fatal("required must reject NO_TOOL_NEEDED")
	}
}

// 实测形状：模型先复述规则原文，再给出决策。此前 274 字符的散文里若含
// 规则回声，末尾指令仍要能取出来。
func TestParseDirectiveAfterRuleRestatement(t *testing.T) {
	tools := extractTestTools()
	reply := `I need to end with EXACTLY one line: CALL_TOOL: tool_name({"arg1":"value1"}).
Looking at the request, the file has to be read before anything else.

CALL_TOOL: read_file({"path":"a.py"})`
	calls, parsed := parseModelToolDecision(reply, tools, "auto")
	if !parsed || len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("directive after a rule restatement must parse: parsed=%v calls=%+v", parsed, calls)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "a.py" {
		t.Fatalf("took the restated example instead of the real decision: %s", calls[0].Arguments)
	}
}
