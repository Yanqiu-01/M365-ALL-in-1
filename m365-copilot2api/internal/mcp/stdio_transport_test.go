package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncWriter 让测试从别的 goroutine 安全地检查子进程收到了什么。
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// readLoop 停止后，等待中的调用必须立刻拿到错误，而不是永远挂住。
//
// 修复前 readLoop 既不检查 Scanner 的错误，也没有任何人从 c.done 接收：一个超出
// bufio.Scanner 上限的帧会让循环静默结束，而 call() 只在回复通道和 ctx.Done() 上
// select。传给它一个没有 deadline 的 context —— 生产代码里完全正常的用法 —— 调用
// 方就永久挂住，而传输层看起来还活着。
//
// 本测试用一个没有 deadline 的 context 调用，并自己计时：修复前会走到超时分支。
func TestStdioCallFailsWhenReadLoopStops(t *testing.T) {
	pr, pw := io.Pipe()
	var stdin syncWriter
	c := newStdioClient(nil, &stdin, pr)

	// 关掉子进程的 stdout：readLoop 结束。
	go func() {
		// 等 call 把请求写出去再关，确保它已经在等回复。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(stdin.String(), `"initialize"`) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_ = pw.Close()
	}()

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		//nolint:usetesting // 必须是没有 deadline 的 context：被测的正是「没有
		// deadline 兜底时会不会永久挂住」。
		done <- result{err: c.Initialize(context.Background())}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("readLoop 已停止却返回成功")
		}
		if !strings.Contains(got.err.Error(), "transport closed") {
			t.Errorf("err=%v want 传输层已关闭", got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("调用永久挂住：readLoop 结束后没有人从 c.done 接收，" +
			"没有 deadline 的 context 就再也回不来了")
	}
}

// 超出帧上限时，错误必须说清楚是帧太大，而不是含糊的 EOF。
func TestStdioOversizedFrameIsReported(t *testing.T) {
	pr, pw := io.Pipe()
	var stdin syncWriter
	// 直接构造一个上限很小的客户端，避免真的产生 8 MiB 数据。
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 64), 256)
	c := &StdioClient{
		stdin:   &stdin,
		stdout:  scanner,
		pending: map[int64]chan json.RawMessage{},
		done:    make(chan struct{}),
	}
	go c.readLoop()

	go func() {
		// 一行远超 256 字节。
		_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"pad":"` +
			strings.Repeat("x", 4096) + `"}}` + "\n"))
		_ = pw.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.sendRequest(ctx, "tools/list", nil)
	if err == nil {
		t.Fatal("帧超限却返回成功")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("调用是靠 context 超时才回来的，说明 Scanner 的错误仍然被丢弃")
	}
	if !strings.Contains(err.Error(), "frame exceeds") {
		t.Errorf("err=%v want 说明帧超出上限", err)
	}
}

// 干净的 EOF 同样是传输结束，等待中的调用不能一直等。
func TestStdioCleanEOFReleasesWaiter(t *testing.T) {
	pr, pw := io.Pipe()
	var stdin syncWriter
	c := newStdioClient(nil, &stdin, pr)
	_ = pw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.sendRequest(ctx, "tools/list", nil)
	if err == nil {
		t.Fatal("stdout 已 EOF 却返回成功")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("靠 context 超时才回来，说明 done 仍然没有被观察")
	}
	if !strings.Contains(err.Error(), "transport closed") {
		t.Errorf("err=%v want 传输层已关闭", err)
	}
}

// 已经到达但还没被取走的回复不能因为 readLoop 刚结束而丢掉。
func TestStdioDeliversReplyThatArrivedBeforeClose(t *testing.T) {
	pr, pw := io.Pipe()
	var stdin syncWriter
	c := newStdioClient(nil, &stdin, pr)

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(stdin.String(), `"tools/list"`) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo"}]}}` + "\n"))
		// 回复之后立刻结束传输。
		_ = pw.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("回复已抵达却报错: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools=%+v", tools)
	}
}

// 默认帧上限必须远大于 bufio.Scanner 的 64 KiB：一个带真实 JSON Schema 的
// tools/list 回复很容易越过 64 KiB，光是帧太大就足以让客户端挂死。
func TestStdioAcceptsFrameLargerThanScannerDefault(t *testing.T) {
	if stdioMaxFrameBytes <= 64<<10 {
		t.Fatalf("stdioMaxFrameBytes=%d，没有超过 bufio.Scanner 的默认上限", stdioMaxFrameBytes)
	}

	pr, pw := io.Pipe()
	var stdin syncWriter
	c := newStdioClient(nil, &stdin, pr)

	// 构造一个 128 KiB 的合法回复。
	description := strings.Repeat("d", 128<<10)
	reply := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"big","description":"` +
		description + `"}]}}`
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(stdin.String(), `"tools/list"`) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_, _ = pw.Write([]byte(reply + "\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("128 KiB 的合法帧被拒绝: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "big" {
		t.Fatalf("tools=%d 个", len(tools))
	}
	if len(tools[0].Description) != len(description) {
		t.Errorf("description 长度=%d want %d", len(tools[0].Description), len(description))
	}
}

// 完整的 JSON-RPC 往返：initialize / tools/list / tools/call。
//
// 用进程内管道驱动，不依赖任何外部解释器，所以在 Windows 上也会真正执行。
func TestStdioRoundTripOverPipes(t *testing.T) {
	// 两条单向管道：客户端写请求 → 服务端读；服务端写回复 → 客户端读。
	serverIn, clientStdin := io.Pipe()
	clientStdout, serverOut := io.Pipe()
	c := newStdioClient(nil, clientStdin, clientStdout)
	t.Cleanup(func() {
		_ = clientStdin.Close()
		_ = serverOut.Close()
	})

	// 一个最小 MCP 服务端。
	go func() {
		scanner := bufio.NewScanner(serverIn)
		scanner.Buffer(make([]byte, 0, 64<<10), stdioMaxFrameBytes)
		for scanner.Scan() {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &req) != nil {
				continue
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
					"inputSchema": map[string]any{"type": "object"},
				}}}
			case "tools/call":
				text, _ := req.Params.Arguments["text"].(string)
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
			default:
				result = map[string]any{}
			}
			reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			if _, err := serverOut.Write(append(reply, '\n')); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	got, err := c.CallTool(ctx, "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content[0]["text"] != "hello" {
		t.Fatalf("result=%+v", got)
	}
	// RPC 层的 error 必须变成 Go error。
	if _, err := c.sendRequest(ctx, "tools/list", nil); err != nil {
		t.Fatalf("第二次请求失败（id 递增出问题）: %v", err)
	}
}

// RPC 错误对象必须变成错误返回。
func TestStdioRPCErrorBecomesError(t *testing.T) {
	pr, pw := io.Pipe()
	var stdin syncWriter
	c := newStdioClient(nil, &stdin, pr)

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(stdin.String(), `"tools/call"`) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no such tool"}}` + "\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.CallTool(ctx, "nope", nil); err == nil {
		t.Fatal("RPC error 被当成成功")
	}
}

// Close 在没有子进程时不得 panic（newStdioClient 允许纯管道驱动）。
func TestStdioCloseWithoutProcess(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	var stdin syncWriter
	c := newStdioClient(nil, &stdin, pr)
	if err := c.Close(); err != nil {
		t.Errorf("Close=%v want nil", err)
	}
}
