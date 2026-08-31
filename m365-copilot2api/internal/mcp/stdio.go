package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// stdioMaxFrameBytes bounds one JSON-RPC line from the child.
//
// bufio.Scanner's own default is 64 KiB, which an ordinary tools/list reply
// exceeds as soon as a server ships a handful of tools with real JSON schemas.
// Past the limit Scan() stops and reports ErrTooLong, so the frame size alone
// used to be enough to strand the client.
const stdioMaxFrameBytes = 8 << 20

// StdioClient 通过子进程 stdin/stdout 交换 JSON-RPC 消息，用于测试与轻量桥接。
type StdioClient struct {
	cmd     *exec.Cmd
	stdin   interface{ Write([]byte) (int, error) }
	stdout  *bufio.Scanner
	mu      sync.Mutex
	pending map[int64]chan json.RawMessage
	nextID  int64
	done    chan struct{}
	// readErr explains why readLoop stopped. Guarded by mu, written once
	// before done is closed and only read after done is closed.
	readErr error
}

// StartStdio 启动一个子进程作为 MCP JSON-RPC 服务器并返回客户端。
// opts 保留用于未来的启动选项，当前忽略。
//
// 可达性说明（已核实，2026-08）：网关本身不调用 StartStdio。整个 internal/mcp
// 只有四个服务端处理器被 internal/web/server.go 引用（HandleSSE、
// HandleStreamable、HandleMessage、HandleToolsList）—— 也就是说本网关在 MCP 里
// 扮演的是「被连接的服务端」。客户端一侧的两种传输（client.go 的 HTTP+SSE 与本
// 文件的 stdio）在生产路径上都没有调用方，只有测试会构造它们。
//
// 保留而不删除的理由：
//   - 两种传输是成对的客户端实现，只删 stdio 会留下一个更奇怪的半成品；
//   - 保留它没有运行期成本：不被调用就不会有子进程，exec 只发生在显式调用时；
//   - 它带着一个真实的挂死缺陷（见 readLoop），修掉比删掉更能说明问题。
//
// 若将来要接入 stdio MCP 服务器，命令与参数必须来自受信任的配置而不是请求体：
// 这里会 exec 任意命令。当前没有任何请求路径能到达这里。
//
// hideChildWindow 与之同生共死，只被 StartStdio 调用。internal/web 有一份同名
// 实现，那份是被真实使用的；两者互不依赖。
func StartStdio(ctx context.Context, command string, args []string, opts any) (*StdioClient, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	hideChildWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return newStdioClient(cmd, stdin, stdout), nil
}

// newStdioClient wires the framing and starts the read loop. Split out of
// StartStdio so the buffer limit and the read loop can be exercised over a plain
// pipe, without a child process.
func newStdioClient(cmd *exec.Cmd, stdin io.Writer, stdout io.Reader) *StdioClient {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), stdioMaxFrameBytes)
	c := &StdioClient{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  scanner,
		pending: map[int64]chan json.RawMessage{},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// readLoop delivers replies until the child's stdout ends, then records why and
// closes done so every waiter in call() is released.
//
// It used to discard the scanner error and nothing ever received from done. A
// frame past bufio.Scanner's limit therefore ended the loop silently, and every
// caller blocked on its reply channel until its context expired — with no
// context, forever. The transport looked alive while no reply could ever arrive.
func (c *StdioClient) readLoop() {
	defer func() {
		err := c.stdout.Err()
		if err == nil {
			// A clean EOF is still the end of the transport: any request already
			// waiting for a reply will never get one.
			err = io.EOF
		}
		if errors.Is(err, bufio.ErrTooLong) {
			err = fmt.Errorf("mcp stdio: frame exceeds %d bytes: %w", stdioMaxFrameBytes, err)
		}
		c.mu.Lock()
		c.readErr = err
		c.mu.Unlock()
		close(c.done)
	}()
	for c.stdout.Scan() {
		line := c.stdout.Bytes()
		if len(line) == 0 {
			continue
		}
		var meta struct {
			ID *int64 `json:"id"`
		}
		if err := json.Unmarshal(line, &meta); err != nil || meta.ID == nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[*meta.ID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- append(json.RawMessage(nil), line...):
			default:
			}
		}
	}
}

func (c *StdioClient) call(ctx context.Context, method string, id int64, params any) (json.RawMessage, error) {
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	b = append(b, '\n')
	c.mu.Lock()
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if _, err := c.stdin.Write(b); err != nil {
		return nil, err
	}
	select {
	case raw := <-ch:
		return raw, nil
	case <-c.done:
		// The read loop has stopped, so this reply can never arrive. Drain first:
		// the reply may already be buffered from just before the loop ended.
		select {
		case raw := <-ch:
			return raw, nil
		default:
		}
		return nil, c.transportErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// transportErr reports why the read loop stopped. Only valid once done is closed.
func (c *StdioClient) transportErr() error {
	c.mu.Lock()
	err := c.readErr
	c.mu.Unlock()
	if err == nil {
		err = io.EOF
	}
	return fmt.Errorf("mcp stdio transport closed: %w", err)
}

// Initialize 发送初始化握手。
func (c *StdioClient) Initialize(ctx context.Context) error {
	_, err := c.sendRequest(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "m365-copilot2api-mcp-stdio", "version": "0.1.0"},
	})
	return err
}

func (c *StdioClient) sendRequest(ctx context.Context, method string, params any) (map[string]any, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	raw, err := c.call(ctx, method, id, params)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if e, ok := obj["error"]; ok && e != nil {
		return nil, fmt.Errorf("rpc error: %v", e)
	}
	return obj, nil
}

// ListTools 列出子进程提供的工具。
func (c *StdioClient) ListTools(ctx context.Context) ([]Tool, error) {
	obj, err := c.sendRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	result, _ := json.Marshal(obj["result"])
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool 调用子进程提供的工具。
func (c *StdioClient) CallTool(ctx context.Context, name string, arguments map[string]any) (CallResult, error) {
	var result CallResult
	obj, err := c.sendRequest(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return result, err
	}
	res, _ := json.Marshal(obj["result"])
	if err := json.Unmarshal(res, &result); err != nil {
		return result, err
	}
	return result, nil
}

// Close 终止子进程。newStdioClient 允许没有子进程（管道驱动），此时无事可做。
func (c *StdioClient) Close() error {
	if c.cmd == nil {
		return nil
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}
