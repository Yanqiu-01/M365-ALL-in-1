package chathub

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// signalRServer 起一个最小 SignalR 服务端，用来端到端驱动 chatWithHandlers。
//
// handle 拿到已完成 WebSocket 升级的连接，并且协议握手帧（{"protocol":"json"}）
// 与首个 chat payload 都已经读掉，可以直接开始发帧。
func signalRServer(t *testing.T, handle func(t *testing.T, conn *websocket.Conn)) *websocket.Dialer {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// 协议握手：客户端发 {"protocol":"json","version":1}，服务端回一个空对象。
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{}`+rs)); err != nil {
			return
		}
		// chat payload。
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		handle(t, conn)
	}))
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "https://")
	return &websocket.Dialer{
		// chathub 固定拨 wss://substrate.office.com，这里把目标改写到测试服务端。
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 5 * time.Second,
	}
}

func testAccount() Account {
	return Account{AccessToken: "token", OID: "oid-1", TID: "tid-1"}
}

// 写截止时间必须每次写之前刷新。
//
// 修复前 SetWriteDeadline 只在握手时调用一次，设的是一个绝对时刻。那一刻过去之
// 后，这条连接上的每一次写都在和一个已经过期的时刻赛跑：SignalR 的 type=6
// keepalive pong 全部以 i/o timeout 失败，gorilla 把这个失败记为致命、此后任何
// 写都直接返回同一个错误，而调用处用 `_ =` 把它丢掉了。长对话就这样静默失去
// keepalive。
//
// 本测试把单次写超时压到 60ms，然后让服务端在超过这个时间之后才发 ping。修复前
// 客户端的 pong 写失败且被忽略，服务端收不到任何东西；修复后 pong 正常抵达。
func TestWebSocketWriteDeadlineRefreshedPerWrite(t *testing.T) {
	old := wsWriteTimeout
	wsWriteTimeout = 60 * time.Millisecond
	t.Cleanup(func() { wsWriteTimeout = old })

	gotPong := make(chan string, 4)
	dialer := signalRServer(t, func(t *testing.T, conn *websocket.Conn) {
		// 让握手时设下的那个绝对截止时刻先过期。
		time.Sleep(4 * wsWriteTimeout)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs)); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			close(gotPong)
			return
		}
		gotPong <- string(msg)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":"hi"}]}]}`+rs))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":3,"invocationId":"0"}`+rs))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &Client{Dialer: dialer, HTTPHeader: http.Header{}}
	res, err := c.Chat(ctx, testAccount(), Request{Text: "hello"})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if res.Text != "hi" {
		t.Errorf("text=%q want %q", res.Text, "hi")
	}

	select {
	case pong, ok := <-gotPong:
		if !ok {
			t.Fatal("服务端没有收到 keepalive pong：写截止时间没有按次刷新，" +
				"pong 以 i/o timeout 失败且错误被丢弃")
		}
		if !strings.Contains(pong, `"type":6`) {
			t.Errorf("pong=%q", pong)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 keepalive pong 超时")
	}
}

// keepalive 写失败必须成为错误，而不是被丢弃。
func TestKeepaliveWriteFailureIsReported(t *testing.T) {
	dialer := signalRServer(t, func(t *testing.T, conn *websocket.Conn) {
		// 发一个 ping，然后立刻断开底层连接，让客户端的 pong 写失败。
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs))
		_ = conn.UnderlyingConn().Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &Client{Dialer: dialer, HTTPHeader: http.Header{}}
	_, err := c.Chat(ctx, testAccount(), Request{Text: "hello"})
	if err == nil {
		t.Fatal("连接已断开却返回成功")
	}
	// 无论是 pong 写失败还是随后的读失败，都必须是错误而不是「成功的空回答」。
	if !strings.Contains(err.Error(), "keepalive") && !strings.Contains(err.Error(), "ws read before completion") {
		t.Errorf("err=%v want keepalive 或 read 失败", err)
	}
}

// 整轮时限必须听调用方的 context，也就是 ChatTimeoutSeconds。
func TestChatResponseDeadlineHonoursContext(t *testing.T) {
	now := time.Now()

	// 调用方给了 deadline：必须原样采用，不能被内部上限截短。
	// 修复前内部写死 5 分钟，任何超过 5 分钟的 ChatTimeoutSeconds 都无效。
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(9*time.Minute))
	defer cancel()
	if got := chatResponseDeadline(ctx, now); !got.Equal(now.Add(9 * time.Minute)) {
		t.Errorf("deadline=%v want %v", got, now.Add(9*time.Minute))
	}

	// 比默认值更短的 deadline 同样原样采用。
	short, cancelShort := context.WithDeadline(context.Background(), now.Add(30*time.Second))
	defer cancelShort()
	if got := chatResponseDeadline(short, now); !got.Equal(now.Add(30 * time.Second)) {
		t.Errorf("deadline=%v want %v", got, now.Add(30*time.Second))
	}

	// 没有 deadline 时才用默认值。
	if got := chatResponseDeadline(context.Background(), now); !got.Equal(now.Add(chatResponseFallbackTimeout)) {
		t.Errorf("fallback deadline=%v want %v", got, now.Add(chatResponseFallbackTimeout))
	}

	// 默认值必须与 ChatTimeoutSeconds 的出厂默认（600s）一致，否则文档又变成谎话。
	if chatResponseFallbackTimeout != 600*time.Second {
		t.Errorf("fallback=%v want 600s，需与 M365_CHAT_TIMEOUT_SECONDS 默认值一致",
			chatResponseFallbackTimeout)
	}
}

// 端到端上半场：整轮时限来自可配置的兜底值，而不是写死在循环里的常量。
//
// 这一半是「修复前失败」的那一半。把兜底值压到 40ms、不给 context 任何 deadline、
// 服务端 250ms 后才发完成帧。修复后兜底值生效，请求在 40ms 左右放弃；修复前循环
// 用的是写死的 5 分钟，压小兜底值毫无作用，请求会成功返回 "late" —— 也就是说那个
// 5 分钟常量根本不受任何配置影响，这正是缺陷本身。
func TestChatDeadlineComesFromConfigurableFallback(t *testing.T) {
	old := chatResponseFallbackTimeout
	chatResponseFallbackTimeout = 40 * time.Millisecond
	t.Cleanup(func() { chatResponseFallbackTimeout = old })

	dialer := signalRServer(t, func(t *testing.T, conn *websocket.Conn) {
		time.Sleep(250 * time.Millisecond)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":"late"}]}]}`+rs))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":3,"invocationId":"0"}`+rs))
	})

	c := &Client{Dialer: dialer, HTTPHeader: http.Header{}}
	//nolint:usetesting // 这里必须是无 deadline 的 context，兜底值才是被测对象。
	res, err := c.Chat(context.Background(), testAccount(), Request{Text: "hello"})
	if err == nil {
		t.Fatalf("兜底时限未生效：请求在 %v 的上限下仍然跑完并返回 %q，"+
			"说明循环用的是写死的常量而不是可配置的时限",
			chatResponseFallbackTimeout, res.Text)
	}
	if !strings.Contains(err.Error(), "deadline exceeded before completion") {
		t.Errorf("err=%v want 整轮时限耗尽", err)
	}
}

// 端到端下半场：调用方给了 deadline 时，兜底值不得截短它。
//
// 兜底值同样压到 40ms，但 context 给 3s，服务端 250ms 后完成。必须成功 —— 这条
// 守的是「不要把兜底值当成硬上限」的反向错误，也就是 ChatTimeoutSeconds 一侧的
// 权威性：operator 设多久就是多久。
func TestContextDeadlineOverridesFallback(t *testing.T) {
	old := chatResponseFallbackTimeout
	chatResponseFallbackTimeout = 40 * time.Millisecond
	t.Cleanup(func() { chatResponseFallbackTimeout = old })

	dialer := signalRServer(t, func(t *testing.T, conn *websocket.Conn) {
		time.Sleep(250 * time.Millisecond)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":"late"}]}]}`+rs))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":3,"invocationId":"0"}`+rs))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := &Client{Dialer: dialer, HTTPHeader: http.Header{}}
	res, err := c.Chat(ctx, testAccount(), Request{Text: "hello"})
	if err != nil {
		t.Fatalf("内部兜底值截短了调用方的 3s 时限: %v", err)
	}
	if res.Text != "late" {
		t.Errorf("text=%q want %q", res.Text, "late")
	}
}

// 握手失败必须让本次尝试失败，而不是被 err 变量遮蔽后当成连接成功。
func TestHandshakeFailureIsNotMaskedByShadowedErr(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// 收到协议握手帧后不回复，直接断开：握手读必须失败。
		_, _, _ = conn.ReadMessage()
		_ = conn.UnderlyingConn().Close()
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "https://")
	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &Client{Dialer: dialer, HTTPHeader: http.Header{}}
	_, err := c.Chat(ctx, testAccount(), Request{Text: "hello"})
	if err == nil {
		t.Fatal("握手失败却返回成功")
	}
	if !strings.Contains(err.Error(), "handshake failed") {
		t.Errorf("err=%v want upstream handshake failed", err)
	}
}
