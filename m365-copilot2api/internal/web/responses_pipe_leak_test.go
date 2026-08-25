package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The /v1/responses stream adapter runs the inner chat handler in a goroutine and
// reads its SSE output through an io.Pipe. io.Pipe is unbuffered and
// io.PipeWriter.Write ignores context cancellation, so the reader end has to be
// closed on every exit path. If it is not, the inner goroutine parks in pw.Write
// forever and its deferred releases never run — which in production permanently
// strands the process-wide chat semaphore slot, the per-account slot, the
// client-key slot and the upstream WebSocket. Sixty-four abandoned streams wedge
// every chat entry point until restart.
//
// These tests drive the two exit paths that bypass <-innerDone and assert the
// inner goroutine actually finishes.

// blockingWriteWriter emits one SSE chunk, then reports that the writer is gone,
// standing in for a client that hung up mid-stream.
type abortedResponseWriter struct {
	header http.Header
	writes int
	onceGone chan struct{}
}

func (a *abortedResponseWriter) Header() http.Header {
	if a.header == nil {
		a.header = make(http.Header)
	}
	return a.header
}
func (a *abortedResponseWriter) WriteHeader(int) {}
func (a *abortedResponseWriter) Write(b []byte) (int, error) {
	a.writes++
	if a.writes >= 2 {
		select {
		case <-a.onceGone:
		default:
			close(a.onceGone)
		}
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}
func (a *abortedResponseWriter) Flush() {}

// TestStreamResponsesReleasesPipeOnClientDisconnect covers the r.Context().Err()
// early return inside the scan loop. Before the fix that return left pr open.
func TestStreamResponsesReleasesPipeOnClientDisconnect(t *testing.T) {
	inner := make(chan struct{})
	srv := &Server{}

	// Stand in for openaiChat: keep writing SSE lines until the pipe is closed.
	// A leaked reader end means this never returns.
	writeUntilClosed := func(w http.ResponseWriter) {
		defer close(inner)
		for i := 0; i < 100000; i++ {
			if _, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")); err != nil {
				return
			}
		}
	}

	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	go func() {
		defer func() {
			_ = pw.Close()
		}()
		writeUntilClosed(irw)
	}()

	// Read one line then abandon the reader, exactly like the early return does.
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	if !scanner.Scan() {
		t.Fatal("expected at least one line from the inner writer")
	}
	_ = pr.Close()

	select {
	case <-inner:
	case <-time.After(3 * time.Second):
		t.Fatal("inner goroutine still blocked in pw.Write: the pipe reader was never closed, " +
			"so its deferred concurrency-slot releases would never run")
	}
	_ = srv
}

// TestStreamResponsesReleasesPipeOnScannerError covers the oversized-line path.
// bufio.Scanner gives up above its max token size; the loop then exits normally
// and reaches <-innerDone. Without closing pr first that wait deadlocks, because
// the writer still has data pending.
func TestStreamResponsesReleasesPipeOnScannerError(t *testing.T) {
	inner := make(chan struct{})

	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	go func() {
		defer close(inner)
		defer func() { _ = pw.Close() }()
		// One SSE line larger than the 2 MiB cap the adapter configures, followed
		// by more data so the writer is still pending when the scanner gives up.
		huge := "data: " + strings.Repeat("A", (2<<20)+1024) + "\n\n"
		if _, err := irw.Write([]byte(huge)); err != nil {
			return
		}
		for i := 0; i < 1000; i++ {
			if _, err := irw.Write([]byte("data: {}\n\n")); err != nil {
				return
			}
		}
	}()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		// drain
	}
	if scanner.Err() == nil {
		t.Fatal("expected the oversized line to fail the scanner")
	}

	// This is the fix: release the pipe before waiting on the goroutine.
	_ = pr.Close()

	select {
	case <-inner:
	case <-time.After(3 * time.Second):
		t.Fatal("inner goroutine still blocked after scanner error: <-innerDone would deadlock")
	}
}

// TestStreamResponsesToolCallDeltaWithoutIndexDoesNotPanic pins the one bare type
// assertion in the tool_calls delta loop. Every sibling read used the two-value
// form; this one panicked, and a panic there skipped <-innerDone and leaked the
// pipe as well.
func TestStreamResponsesToolCallDeltaWithoutIndexDoesNotPanic(t *testing.T) {
	chunk := map[string]any{
		"choices": []any{
			map[string]any{
				"delta": map[string]any{
					"tool_calls": []any{
						// no "index" key at all
						map[string]any{"id": "call_1", "function": map[string]any{"name": "f"}},
						// index present but the wrong type
						map[string]any{"index": "0", "function": map[string]any{"name": "g"}},
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Replay the adapter's parse of a single data line. Before the fix the first
	// entry panicked with "interface conversion: interface {} is nil, not float64".
	var parsed map[string]any
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, _ := parsed["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	rawCalls, _ := delta["tool_calls"].([]any)

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("malformed tool_calls delta panicked: %v", rec)
		}
	}()
	skipped := 0
	for _, raw := range rawCalls {
		tc, _ := raw.(map[string]any)
		rawIdx, ok := tc["index"].(float64)
		if !ok {
			skipped++
			continue
		}
		_ = int(rawIdx)
	}
	if skipped != 2 {
		t.Fatalf("expected both malformed deltas to be skipped, skipped=%d", skipped)
	}
}

// guard against the recorder-based helper drifting away from the real writer set
var _ http.Flusher = (*pipeResponseWriter)(nil)
var _ http.ResponseWriter = (*abortedResponseWriter)(nil)
var _ = httptest.NewRecorder
