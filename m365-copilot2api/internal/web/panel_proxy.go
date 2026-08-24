package web

// PanelProxy forwards registration and batch OAuth calls to the local Python
// panel (M365-自用). The panel owns the Playwright/CAPTCHA logic that cannot run
// inside a Go binary, so rather than reimplement it we proxy the requests.
//
// The user asked for "one port, one page". After this change they open only
// http://127.0.0.1:4141 — the dashboard calls these routes, which transparently
// hit the panel on 8555. If the panel is not running, the proxy returns a clear
// error instead of hanging.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const panelBaseURL = "http://127.0.0.1:8555"

// panelProxyTimeout bounds each proxied call. Registration and batch OAuth are
// long-running jobs on the panel side; this is just the HTTP round-trip for
// spawning them, not waiting for completion.
const panelProxyTimeout = 30 * time.Second

func (s *Server) panelProxy(w http.ResponseWriter, r *http.Request) {
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}

	// Map gateway path → panel path.
	var method string
	var panelPath string
	switch r.URL.Path {
	case "/api/admin/panel/register":
		method, panelPath = http.MethodPost, "/api/register"
	case "/api/admin/panel/oauth":
		method, panelPath = http.MethodPost, "/api/oauth"
	case "/api/admin/panel/oauth/batch":
		method, panelPath = http.MethodPost, "/api/oauth/batch"
	case "/api/admin/panel/state":
		method, panelPath = http.MethodGet, "/api/state"
	case "/api/admin/panel/job/stop":
		method, panelPath = http.MethodPost, "/api/job/stop"
	case "/api/admin/panel/job/poll":
		method, panelPath = http.MethodGet, "/api/job/poll"
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown panel route")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), panelProxyTimeout)
	defer cancel()

	var bodyReader io.Reader
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
			return
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, panelBaseURL+panelPath, bodyReader)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "proxy_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The most common cause: the panel process is not running.
		log.Printf("[panel-proxy] %s %s failed: %v", method, panelPath, err)
		writeOpenAIError(w, http.StatusServiceUnavailable, "panel_unavailable",
			fmt.Sprintf("M365 面板(8555)未响应，请先启动: python run.py --panel-only --no-browser (%v)", err))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 1024*1024))
}
