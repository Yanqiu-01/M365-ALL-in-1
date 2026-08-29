package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HandleV1 is the FlareSolverr-compatible JSON endpoint.
func HandleV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"status":"error","message":"POST required"}`, http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeFlare(w, flareReply{Status: "error", Message: err.Error()})
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeFlare(w, flareReply{Status: "error", Message: "invalid json"})
		return
	}
	cmd := strings.ToLower(strings.TrimSpace(fmt.Sprint(payload["cmd"])))
	switch cmd {
	case "request.get", "request.post":
	default:
		writeFlare(w, flareReply{Status: "error", Message: "unsupported cmd"})
		return
	}
	page := strings.TrimSpace(fmt.Sprint(payload["url"]))
	if page == "" {
		writeFlare(w, flareReply{Status: "error", Message: "url is required"})
		return
	}
	timeout := DefaultTimeout
	if v, ok := payload["maxTimeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	solved, err := solveLocal(ctx, page)
	if err != nil {
		writeFlare(w, flareReply{Status: "error", Message: err.Error()})
		return
	}
	html := solved.HTML
	if html == "" && solved.Token != "" {
		html = `<!doctype html><input name="cf-turnstile-response" value="` + htmlEscape(solved.Token) + `">`
	}
	writeFlare(w, flareReply{
		Status:  "ok",
		Message: "Challenge solved!",
		Solution: flareSolution{
			URL:       page,
			Status:    200,
			Response:  html,
			UserAgent: solved.UserAgent,
		},
	})
}

func writeFlare(w http.ResponseWriter, reply flareReply) {
	w.Header().Set("Content-Type", "application/json")
	if !strings.EqualFold(reply.Status, "ok") {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func htmlEscape(v string) string {
	r := strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `<`, "&lt;", `>`, "&gt;")
	return r.Replace(v)
}

// Listen serves the built-in FlareSolverr API on addr, typically 127.0.0.1:8191.
func Listen(ctx context.Context, addr string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1", HandleV1)
	mux.HandleFunc("/v1/", HandleV1)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("built-in FlareSolverr listening on http://%s/v1", addr)
	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type localSolution struct {
	Token     string
	HTML      string
	UserAgent string
}

func solveLocal(ctx context.Context, page string) (localSolution, error) {
	if dir := flareDir(); dir != "" {
		if solved, err := solveViaWebView(ctx, dir, page); err == nil {
			return solved, nil
		} else if !errors.Is(err, errNoWebView) {
			return localSolution{}, err
		}
	}
	return localSolution{}, errors.New("内置 FlareSolverr 需要 App 内的隐藏 WebView。请打开修改版M365 后再注册")
}

func flareDir() string {
	root := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
	if root == "" {
		return ""
	}
	return filepath.Join(root, "flare")
}

var (
	errNoWebView = errors.New("webview solver is not running")
	webviewMu    sync.Mutex
)

func solveViaWebView(ctx context.Context, dir, page string) (localSolution, error) {
	webviewMu.Lock()
	defer webviewMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return localSolution{}, err
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	job := filepath.Join(dir, "job")
	tmp := filepath.Join(dir, "job.tmp")
	result := filepath.Join(dir, "result")
	_ = os.Remove(result)
	if err := os.WriteFile(tmp, []byte(id+"\n"+page+"\n"), 0o600); err != nil {
		return localSolution{}, err
	}
	if err := os.Rename(tmp, job); err != nil {
		return localSolution{}, err
	}
	defer os.Remove(job)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	seenJob := false
	for {
		select {
		case <-ctx.Done():
			return localSolution{}, fmt.Errorf("WebView 求解超时: %w", ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(job); err == nil {
				seenJob = true
			} else if seenJob {
				// poller consumed the job; keep waiting for result
			}
			body, err := os.ReadFile(result)
			if err != nil {
				continue
			}
			lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
			if len(lines) < 2 || strings.TrimSpace(lines[0]) != id {
				continue
			}
			_ = os.Remove(result)
			token := strings.TrimSpace(lines[1])
			if token == "" {
				return localSolution{}, errors.New("WebView 未拿到 Turnstile token")
			}
			return localSolution{Token: token, UserAgent: "M365-WebView"}, nil
		}
	}
}
