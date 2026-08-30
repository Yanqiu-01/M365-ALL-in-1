package turnstile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractTokenBothAttributeOrders(t *testing.T) {
	cases := []string{
		`<input type="hidden" name="cf-turnstile-response" value="tok_abcdefghijklmnopqrstuvwxyz">`,
		`<input value="tok_abcdefghijklmnopqrstuvwxyz" name="cf-turnstile-response">`,
	}
	for _, html := range cases {
		if got := extractToken(html); got != "tok_abcdefghijklmnopqrstuvwxyz" {
			t.Fatalf("extractToken(%q) = %q", html, got)
		}
	}
	if extractToken(`<div>no token</div>`) != "" {
		t.Fatal("expected empty")
	}
}

func TestSolveReadsTokenFromFlareSolverr(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" {
			http.NotFound(w, r)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["cmd"] != "request.get" {
			t.Errorf("cmd = %#v", payload["cmd"])
		}
		if payload["url"] != "https://office.example.test/" {
			t.Errorf("url = %#v", payload["url"])
		}
		if payload["username"] != "24s055026" || payload["displayName"] != "User1" {
			t.Errorf("credentials = %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"solution": map[string]any{
				"userAgent": "TestAgent",
				"cookies":   []map[string]string{{"name": "cf_clearance", "value": "clr"}},
				"response":  `<input name="cf-turnstile-response" value="solved-token-from-flare-solverr">`,
			},
		})
	}))
	defer server.Close()

	got, err := Solve(context.Background(), Request{
		Endpoint:    server.URL + "/v1",
		PageURL:     "https://office.example.test/",
		Timeout:     2 * time.Second,
		DisplayName: "User1",
		Username:    "24s055026",
		Password:    "Passw0rd!",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "solved-token-from-flare-solverr" || got.Clearance != "clr" {
		t.Fatalf("%#v", got)
	}
}

func TestSolveReportsMissingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"solution": map[string]any{"response": "<html>no widget</html>"},
		})
	}))
	defer server.Close()
	_, err := Solve(context.Background(), Request{Endpoint: server.URL, PageURL: "https://office.example.test/"})
	if err == nil || !strings.Contains(err.Error(), "没有完成填表和提交") {
		t.Fatalf("err = %v", err)
	}
}

func TestSolveReportsEndpointDown(t *testing.T) {
	_, err := Solve(context.Background(), Request{
		Endpoint: "http://127.0.0.1:1/v1",
		PageURL:  "https://office.example.test/",
		Timeout:  200 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "FlareSolverr 不可用") {
		t.Fatalf("err = %v", err)
	}
}
