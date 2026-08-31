package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// readStageEntries 读回 stage 日志里的每一行 JSON。
func readStageEntries(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(diagPath())
	if err != nil {
		t.Fatalf("read stage log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("stage line %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

func stageEntry(t *testing.T, entries []map[string]any, name string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry["stage"] == name {
			return entry
		}
	}
	t.Fatalf("stage %q not found in %v", name, entries)
	return nil
}

// http_end 的 error 字段以前无法出现：唯一的生产调用方（openaiChat 的 defer）
// 永远传 nil，于是「客户端中途断开」这类请求级失败在 http_end 里没有任何痕迹。
// 现在 err 为 nil 时回退到请求上下文的终止原因。
func TestEndRequestRecordsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	resetDiagForTest(t)
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_STAGE_LOG", "1")
	t.Setenv("M365_STAGE_LOG_MAX_BYTES", "65536")

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	beginRequest("cancelled-request", request)
	// 客户端断开：上下文被取消，handler 随后照常走它的 defer。
	cancel()
	endRequest("cancelled-request", nil)

	entry := stageEntry(t, readStageEntries(t), "http_end")
	message, _ := entry["error"].(string)
	if message == "" {
		t.Fatalf("http_end carries no error after the request context was cancelled: %v", entry)
	}
	if !strings.Contains(message, "context canceled") {
		t.Fatalf("http_end error=%q, want the context cancellation reason", message)
	}
}

// 正常结束不得凭空多出一个 error 字段：net/http 在 ServeHTTP 返回之后才取消
// 上下文，handler 自己的 defer 早于此执行，所以成功路径上 ctx.Err() 为 nil。
func TestEndRequestOmitsErrorOnNormalCompletion(t *testing.T) {
	dir := t.TempDir()
	resetDiagForTest(t)
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_STAGE_LOG", "1")
	t.Setenv("M365_STAGE_LOG_MAX_BYTES", "65536")

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	beginRequest("ok-request", request)
	endRequest("ok-request", nil)

	entry := stageEntry(t, readStageEntries(t), "http_end")
	if _, present := entry["error"]; present {
		t.Fatalf("successful request logged an error field: %v", entry)
	}
}

// 显式传入的错误优先于上下文原因，并且照样过脱敏。
func TestEndRequestPrefersExplicitErrorAndRedactsIt(t *testing.T) {
	dir := t.TempDir()
	resetDiagForTest(t)
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_STAGE_LOG", "1")
	t.Setenv("M365_STAGE_LOG_MAX_BYTES", "65536")

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	beginRequest("explicit-request", request)
	endRequest("explicit-request", &stageTestError{"upstream refused: Bearer SECRET-TOKEN-VALUE"})

	entry := stageEntry(t, readStageEntries(t), "http_end")
	message, _ := entry["error"].(string)
	if !strings.Contains(message, "upstream refused") {
		t.Fatalf("explicit error was dropped: %v", entry)
	}
	if strings.Contains(message, "SECRET-TOKEN-VALUE") {
		t.Fatalf("stage log leaked a bearer token: %q", message)
	}
}

// inflight 快照进 /api/live 的响应，上下文不能被序列化出去。
func TestInflightSnapshotDoesNotSerializeTheContext(t *testing.T) {
	resetDiagForTest(t)
	t.Setenv("M365_STAGE_LOG", "")

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	beginRequest("snapshot-request", request)
	defer endRequest("snapshot-request", nil)

	encoded, err := json.Marshal(inflightSnapshot())
	if err != nil {
		t.Fatalf("marshal inflight snapshot: %v", err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "ctx") || strings.Contains(string(encoded), "context") {
		t.Fatalf("inflight snapshot exposes the request context: %s", encoded)
	}
}

type stageTestError struct{ message string }

func (e *stageTestError) Error() string { return e.message }
