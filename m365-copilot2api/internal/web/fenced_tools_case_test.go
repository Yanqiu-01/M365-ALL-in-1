package web

import "testing"

// 真实客户端声明的拼写。Claude Code 声明 Bash/Read/Edit/Write/Glob/Grep，
// 全部首字母大写；既有围栏测试一律声明小写（"bash"、"read_file"），
// 所以整类大小写漏判从来没被夹具覆盖过。
func capitalizedClientTools() []map[string]any {
	spec := map[string][]string{
		"Bash":  {"command"},
		"Read":  {"file_path"},
		"Write": {"file_path", "content"},
	}
	out := make([]map[string]any, 0, len(spec))
	for name, params := range spec {
		props := map[string]any{}
		for _, p := range params {
			props[p] = map[string]any{"type": "string"}
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": "x",
			"parameters": map[string]any{"type": "object", "properties": props},
		}})
	}
	return out
}

// 声明 Bash 时，declaredShell 必须认出来并给出声明拼写。
// 返回 "" 会连带让 shell 特例和裸 JSON 兜底一起失效。
func TestDeclaredShellFindsACapitalizedShellTool(t *testing.T) {
	if got := declaredShell(allowedToolNames(capitalizedClientTools())); got != "Bash" {
		t.Fatalf("declaredShell = %q, want the declared spelling \"Bash\"", got)
	}
}

// 模型写 ```bash 而客户端声明 Bash：必须出调用，且名字用声明拼写。
// 提示词说的就是 "call the bash tool"，小写是模型最自然的写法。
func TestLowercaseFenceResolvesToTheDeclaredSpelling(t *testing.T) {
	tools := capitalizedClientTools()
	for _, text := range []string{
		"```bash\n{\"command\":\"go test ./...\"}\n```\n",
		"```bash\ngo test ./...\n```\n",
		"```Bash\n{\"command\":\"go test ./...\"}\n```\n",
		"```sh\n{\"command\":\"go test ./...\"}\n```\n",
	} {
		calls := fencedToolCalls(text, tools, "auto")
		if len(calls) != 1 {
			t.Errorf("%q -> %d calls, want 1", text, len(calls))
			continue
		}
		if calls[0].Name != "Bash" {
			t.Errorf("%q -> Name %q, want the declared spelling \"Bash\"", text, calls[0].Name)
		}
	}
}

// 非 shell 工具同样要忽略大小写，并且回声明拼写。
func TestNonShellFenceResolvesCaseInsensitively(t *testing.T) {
	tools := capitalizedClientTools()
	for _, tc := range []struct{ text, want string }{
		{"```read\n{\"file_path\":\"a.txt\"}\n```\n", "Read"},
		{"```Read\n{\"file_path\":\"a.txt\"}\n```\n", "Read"},
		{"```WRITE\n{\"file_path\":\"a.txt\",\"content\":\"x\"}\n```\n", "Write"},
	} {
		calls := fencedToolCalls(tc.text, tools, "auto")
		if len(calls) != 1 || calls[0].Name != tc.want {
			t.Errorf("%q -> %+v, want one call named %q", tc.text, calls, tc.want)
		}
	}
}

// declaredFenceStart 是流式路径缓冲与剥离围栏的唯一判据。它恒返回 -1 时，
// 模型写对的调用会作为可见正文漏给客户端。
func TestDeclaredFenceStartSeesCapitalizedAndLowercasedTags(t *testing.T) {
	tools := capitalizedClientTools()
	for _, text := range []string{
		"```bash\n{\"command\":\"ls\"}\n```\n",
		"```Bash\n{\"command\":\"ls\"}\n```\n",
		"```read\n{\"file_path\":\"a.txt\"}\n```\n",
		"```Read\n{\"file_path\":\"a.txt\"}\n```\n",
	} {
		if got := declaredFenceStart(text, tools, "auto"); got != 0 {
			t.Errorf("declaredFenceStart(%q) = %d, want 0", text, got)
		}
	}
	// 未声明的名字照旧不算围栏起点。
	if got := declaredFenceStart("```python\nprint(1)\n```\n", tools, "auto"); got != -1 {
		t.Errorf("undeclared tag must not start a declared fence, got %d", got)
	}
}

// 未完成的围栏头也要能忽略大小写匹配上，否则流式会把半个头当正文发走。
func TestDeclaredFencePrefixMatchesCaseInsensitively(t *testing.T) {
	allowed := allowedToolNames(capitalizedClientTools())
	shell := declaredShell(allowed)
	for _, partial := range []string{"", "ba", "bash", "re", "wri"} {
		if !declaredFencePrefix(partial, allowed, shell) {
			t.Errorf("partial %q must be treated as a possible declared fence", partial)
		}
	}
	if declaredFencePrefix("pyth", allowed, shell) {
		t.Error("undeclared partial must not be buffered")
	}
}

// tool_choice 指名某个工具时，判定也要走声明拼写，否则模型写小写就被判成
// 「不是你指定的那个工具」而丢掉。
func TestToolChoiceByNameStillMatchesALowercaseFence(t *testing.T) {
	tools := capitalizedClientTools()
	choice := map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}}
	calls := fencedToolCalls("```bash\n{\"command\":\"ls\"}\n```\n", tools, choice)
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("got %+v, want one call named \"Bash\"", calls)
	}
}
