package web

import (
	"context"
	"encoding/json"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"strings"
	"time"
)

// persistProxyPool writes the pool back to settings. It must use the raw URLs:
// the status view redacts the password, and persisting that would replace a
// working credential with the "***" placeholder on the next restart.
func (s *Server) persistProxyPool() error {
	v := s.settings.get()
	v.ProxyPool = outbound.ProxyPoolRawURLs()
	return s.settings.save(v)
}

// proxyAdmissionTimeout bounds the pre-admission probe so the admin request
// cannot hang on a dead candidate.
const proxyAdmissionTimeout = 10 * time.Second

func (s *Server) proxyPool(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && r.URL.Query().Get("action") == "check" {
		p := outbound.CurrentPool()
		if p == nil {
			jsonOut(w, map[string]any{"ok": true, "proxies": []map[string]any{}})
			return
		}
		jsonOut(w, map[string]any{"ok": true, "proxies": p.CheckAll(r.Context())})
		return
	}
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"proxies": outbound.ProxyPoolStatus()})
	case http.MethodPost:
		var body struct {
			URL  string   `json:"url"`
			URLs []string `json:"urls"`
			// SkipCheck is the escape hatch: it admits an exit without probing,
			// for an exit that is known good but temporarily unreachable from here.
			SkipCheck bool `json:"skipCheck"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		urls := append(body.URLs, body.URL)
		added := 0
		for _, raw := range urls {
			for _, v := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
				candidate := strings.TrimSpace(v)
				if candidate == "" {
					continue
				}
				// Admission gate. A batch of dead exits once took the 502 rate from
				// 0.1% to 63.8% precisely because AddProxy only parsed the URL.
				if !body.SkipCheck {
					ctx, cancel := context.WithTimeout(r.Context(), proxyAdmissionTimeout)
					err := outbound.ValidateProxyCandidate(ctx, candidate)
					cancel()
					if err != nil {
						writeOpenAIError(w, 400, "invalid_request_error", err.Error()+"（确认该出口可用可在请求体加 "+`"skipCheck":true`+" 跳过校验）")
						return
					}
				}
				if err := outbound.AddProxy(candidate); err != nil {
					writeOpenAIError(w, 400, "invalid_request_error", err.Error())
					return
				}
				if err := s.persistProxyPool(); err != nil {
					writeOpenAIError(w, 500, "storage_error", err.Error())
					return
				}
				added++
			}
		}
		jsonOut(w, map[string]any{"ok": true, "added": added, "proxies": outbound.ProxyPoolStatus()})
	case http.MethodDelete:
		raw := strings.TrimRight(strings.TrimSpace(r.URL.Query().Get("url")), "/")
		// ?id= is the credential-free way to delete: the status view exposes a
		// stable hash of host:port, so the dashboard never needs the password. ?url=
		// keeps working with either the raw or the redacted form (see sameProxyURL).
		if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
			if resolved := outbound.ProxyRawURLForID(id); resolved != "" {
				raw = resolved
			} else {
				writeOpenAIError(w, 400, "invalid_request_error", "proxy not found for id")
				return
			}
		}
		if raw == "" {
			if err := outbound.ConfigurePool(nil); err != nil {
				writeOpenAIError(w, 400, "invalid_request_error", err.Error())
				return
			}
		} else if err := outbound.RemoveProxy(raw); err != nil {
			writeOpenAIError(w, 400, "invalid_request_error", err.Error())
			return
		}
		if err := s.persistProxyPool(); err != nil {
			writeOpenAIError(w, 500, "storage_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true, "proxies": outbound.ProxyPoolStatus()})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}
