package chathub

import (
	"context"
	"net/http"
	"testing"
)

func TestShouldRetryWebSocketDial(t *testing.T) {
	ctx := context.Background()
	if !shouldRetryWebSocketDial(ctx, 0, nil) {
		t.Fatal("response-less first dial error should retry once")
	}
	if !shouldRetryWebSocketDial(ctx, 0, &http.Response{StatusCode: http.StatusBadGateway}) {
		t.Fatal("5xx websocket handshake should retry once")
	}
	if shouldRetryWebSocketDial(ctx, 0, &http.Response{StatusCode: http.StatusBadRequest}) {
		t.Fatal("4xx websocket handshake must not retry")
	}
	if shouldRetryWebSocketDial(ctx, maxWebSocketSetupAttempts-1, nil) {
		t.Fatal("retry limit was ignored")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if shouldRetryWebSocketDial(canceled, 0, nil) {
		t.Fatal("canceled context must not retry")
	}
}
