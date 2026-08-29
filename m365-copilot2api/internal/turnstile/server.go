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
	dir := flareDir()
	if dir == "" {
		return localSolution{}, errors.New("内置 FlareSolverr 需要 App 数据目录。请打开修改版M365 后再注册")
	}
	return solveViaWebView(ctx, dir, page)
}

func flareDir() string {
	root := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
	if root == "" {
		return ""
	}
	return filepath.Join(root, "flare")
}

var webviewMu sync.Mutex

func solveViaWebView(ctx context.Context, dir, page string) (localSolution, error) {
	webviewMu.Lock()
	defer webviewMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return localSolution{}, err
	}
	ready := filepath.Join(dir, "ready")
	if _, err := os.Stat(ready); err != nil {
		return localSolution{}, errors.New("验证页还没启动。请先打开修改版M365 主界面，等几秒后再点开始注册")
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	job := filepath.Join(dir, "job")
	tmp := filepath.Join(dir, "job.tmp")
	result := filepath.Join(dir, "result")
	status := filepath.Join(dir, "status")
	_ = os.Remove(result)
	_ = os.Remove(status)
	if err := os.WriteFile(tmp, []byte(id+"\n"+page+"\n"), 0o600); err != nil {
		return localSolution{}, err
	}
	if err := os.Rename(tmp, job); err != nil {
		return localSolution{}, err
	}
	defer os.Remove(job)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	jobTaken := false
	pageLoaded := false
	for {
		select {
		case <-ctx.Done():
			return localSolution{}, timeoutError(jobTaken, pageLoaded)
		case <-ticker.C:
			if _, err := os.Stat(job); err != nil {
				jobTaken = true
			}
			if body, err := os.ReadFile(status); err == nil && strings.Contains(string(body), "loaded") {
				pageLoaded = true
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
				return localSolution{}, errors.New("验证页已打开，但没有拿到 Turnstile token")
			}
			return localSolution{Token: token, UserAgent: "M365-WebView"}, nil
		}
	}
}

func timeoutError(jobTaken, pageLoaded bool) error {
	switch {
	case !jobTaken:
		return errors.New("验证页没有接到任务。请保持修改版M365 在前台，不要锁屏")
	case !pageLoaded:
		return errors.New("验证页没有打开注册站。请检查网络后重试")
	default:
		return errors.New("验证页已打开，但 Turnstile 没有给出 token。请在弹出的验证层里完成勾选")
	}
}
