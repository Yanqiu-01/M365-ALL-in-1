package turnstile

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandleV1RequiresURL(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewReader([]byte(`{"cmd":"request.get"}`)))
	HandleV1(recorder, req)
	if recorder.Code == http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "url") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestHandleV1ReadsWebViewResult(t *testing.T) {
	root := t.TempDir()
	t.Setenv("M365_DATA_DIR", root)
	dir := filepath.Join(root, "flare")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := 0; i < 40; i++ {
			body, err := os.ReadFile(filepath.Join(dir, "job"))
			if err != nil {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			id := strings.Split(string(body), "\n")[0]
			_ = os.WriteFile(filepath.Join(dir, "result"), []byte(id+"\nwebview-token-abcdefghijklmnopqrstuvwxyz\n"), 0o600)
			return
		}
	}()
	recorder := httptest.NewRecorder()
	payload, _ := json.Marshal(map[string]any{"cmd": "request.get", "url": "https://office.example.test/", "maxTimeout": 4000})
	req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewReader(payload))
	HandleV1(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var reply flareReply
	if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Status != "ok" {
		t.Fatalf("%#v", reply)
	}
	if extractToken(reply.Solution.Response) != "webview-token-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("response = %q", reply.Solution.Response)
	}
}
