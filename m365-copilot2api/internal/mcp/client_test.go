package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperArg 标记「这次运行是被 StartStdio 拉起来的子进程」。
//
// 原来这里是一个用 python3 -c 跑的内嵌 MCP 服务端。它在 Windows 上被
// runtime.GOOS 跳过，也就是说 StartStdio 这条路径在本机从未真正执行过；而且它给
// 一个纯 Go 项目的测试套件引入了一个外部解释器依赖。改用测试二进制自己
// re-exec：同样是真的子进程、真的管道、真的 StartStdio，但不需要 Python。
const helperArg = "mcp-stdio-helper"

// TestStdioHelperServer 在被当作子进程拉起时充当 MCP 服务端，否则跳过。
func TestStdioHelperServer(t *testing.T) {
	if !isHelperInvocation() {
		t.Skip("仅在被 StartStdio 作为子进程拉起时运行")
	}
	serveHelperStdio()
}

func isHelperInvocation() bool {
	for _, arg := range os.Args[1:] {
		if arg == helperArg {
			return true
		}
	}
	return false
}

// serveHelperStdio 是一个最小 JSON-RPC MCP 服务端，读 stdin 写 stdout。
func serveHelperStdio() {
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := decoder.Decode(&req); err != nil {
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "test"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "echo",
				"description": "Echo text",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				},
			}}}
		case "tools/call":
			text, _ := req.Params.Arguments["text"].(string)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			result = map[string]any{}
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
			return
		}
	}
}

// StartStdio 的端到端往返，走真实子进程。
func TestStdioMCPRoundTrip(t *testing.T) {
	if isHelperInvocation() {
		t.Skip("这是 helper 子进程")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 让测试二进制以 helper 身份重新执行自己。-test.run 把它限定到 helper 用例，
	// helperArg 让那个用例知道自己该干活而不是跳过。
	c, err := StartStdio(ctx, os.Args[0],
		[]string{"-test.run=^TestStdioHelperServer$", "-test.v=false", helperArg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	got, err := c.CallTool(ctx, "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) == 0 || got.Content[0]["text"] != "hello" {
		t.Fatalf("result=%+v", got)
	}
}

// 子进程直接退出时，等待中的调用必须拿到传输层错误而不是挂住。
func TestStdioChildExitReleasesCaller(t *testing.T) {
	if isHelperInvocation() {
		t.Skip("这是 helper 子进程")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 一个立刻结束、什么都不回的子进程：-test.run 匹配不到任何用例。
	c, err := StartStdio(ctx, os.Args[0], []string{"-test.run=^NoSuchTestAtAll$"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()
	err = c.Initialize(callCtx)
	if err == nil {
		t.Fatal("子进程已退出却返回成功")
	}
	if !strings.Contains(err.Error(), "transport closed") {
		t.Errorf("err=%v want 传输层已关闭（挂到 context 超时说明 done 没有被观察）", err)
	}
}

var _ = exec.Command
