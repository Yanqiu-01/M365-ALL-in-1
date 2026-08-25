package web

import (
	"context"
	"encoding/json"
	"fmt"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"strings"
	"sync"
	"time"
)

// proxyPoolMutationMu serializes the settings-backed mutations made by this
// handler. The outbound pool is process-global, and taking a fresh snapshot
// while an import/delete is committing used to make concurrent edits overwrite
// each other. Health checks intentionally do not take this lock.
var proxyPoolMutationMu sync.Mutex

// persistProxyPool writes the pool back to settings. It must use the raw URLs:
// the status view redacts the password, and persisting that would replace a
// working credential with the "***" placeholder on the next restart.
func (s *Server) persistProxyPool() error {
	return s.persistProxyPoolURLs(outbound.ProxyPoolRawURLs())
}

// persistProxyPoolURLs persists a caller-supplied raw snapshot. It must never
// receive the status/API representation: that representation intentionally has
// passwords replaced with "***".
func (s *Server) persistProxyPoolURLs(raw []string) error {
	v := s.settings.get()
	v.ProxyPool = append([]string(nil), raw...)
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
		// Be explicit at the HTTP boundary: browser-facing views must never
		// receive proxy passwords even if the outbound representation changes.
		jsonOut(w, map[string]any{"proxies": outbound.ProxyPoolStatusRedacted()})
	case http.MethodPost:
		switch r.URL.Query().Get("action") {
		case "free-import":
			s.importFreeProxySource(w, r)
		case "":
			s.addManualProxies(w, r)
		default:
			writeOpenAIError(w, 400, "invalid_request_error", "unknown proxy pool action")
		}
	case http.MethodDelete:
		if r.URL.Query().Get("action") == "selected" {
			s.deleteSelectedProxies(w, r)
			return
		}
		if r.URL.Query().Get("action") != "" {
			writeOpenAIError(w, 400, "invalid_request_error", "unknown proxy pool action")
			return
		}
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
		if err := s.deleteProxy(raw); err != nil {
			writeOpenAIError(w, 400, "invalid_request_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true, "proxies": outbound.ProxyPoolStatusRedacted()})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *Server) addManualProxies(w http.ResponseWriter, r *http.Request) {
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

	urls := append(append([]string(nil), body.URLs...), body.URL)
	candidates := splitProxyInput(urls)
	if len(candidates) == 0 {
		writeOpenAIError(w, 400, "invalid_request_error", "proxy address is required")
		return
	}
	// 逐条判定，互不牵连。此前任何一条校验失败就 return，导致粘贴 10 个节点、
	// 只有 1 个不通，整批 10 个全部被拒 —— 用户看到的就是「添加 IP 时一个节点
	// 不过，整条都被拒绝」。准入门槛本身要保留（一批死出口曾把 502 率从 0.1%
	// 抬到 63.8%），但它应该只筛掉坏的那几个。
	accepted := make([]string, 0, len(candidates))
	rejected := make([]map[string]any, 0)
	for _, candidate := range candidates {
		if body.SkipCheck {
			accepted = append(accepted, candidate)
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), proxyAdmissionTimeout)
		err := freeProxyCandidateValidator(ctx, candidate)
		cancel()
		if err != nil {
			// 出口地址本身可能带凭据，回给浏览器的只用脱敏后的 host:port。
			rejected = append(rejected, map[string]any{
				"proxy":  outbound.RedactProxyURL(candidate),
				"reason": err.Error(),
			})
			continue
		}
		accepted = append(accepted, candidate)
	}

	// 全军覆没时仍然报 400：这时用户的输入确实没有一个可用，静默返回 200
	// 会让人以为添加成功了。
	if len(accepted) == 0 {
		writeOpenAIError(w, 400, "invalid_request_error",
			fmt.Sprintf("%d 个出口全部未通过校验；确认出口可用可在请求体加 %s 跳过校验", len(rejected), `"skipCheck":true`))
		return
	}

	added, err := s.appendProxyPool(accepted)
	if err != nil {
		writeOpenAIError(w, 500, "storage_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{
		"ok": true, "added": added,
		"accepted": len(accepted), "rejected": rejected,
		"proxies": outbound.ProxyPoolStatusRedacted(),
	})
}

func splitProxyInput(inputs []string) []string {
	var out []string
	for _, raw := range inputs {
		for _, v := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
			if candidate := strings.TrimSpace(v); candidate != "" {
				out = append(out, candidate)
			}
		}
	}
	return out
}

// appendProxyPool applies a batch atomically with respect to other proxy-pool
// mutations. It does not perform a health check; callers choose the admission
// policy before invoking it.
func (s *Server) appendProxyPool(candidates []string) (int, error) {
	proxyPoolMutationMu.Lock()
	defer proxyPoolMutationMu.Unlock()

	previous := outbound.ProxyPoolRawURLs()
	next := append([]string(nil), previous...)
	seen := make(map[string]struct{}, len(next)+len(candidates))
	for _, raw := range next {
		seen[proxyPoolRawKey(raw)] = struct{}{}
	}
	added := 0
	for _, candidate := range candidates {
		key := proxyPoolRawKey(candidate)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		next = append(next, candidate)
		added++
	}
	if added == 0 {
		return 0, nil
	}
	if err := outbound.ConfigurePool(next); err != nil {
		return 0, err
	}
	if err := s.persistProxyPoolURLs(next); err != nil {
		_ = outbound.ConfigurePool(previous)
		return 0, err
	}
	return added, nil
}

func proxyPoolRawKey(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func (s *Server) deleteProxy(raw string) error {
	proxyPoolMutationMu.Lock()
	defer proxyPoolMutationMu.Unlock()

	previous := outbound.ProxyPoolRawURLs()
	if raw == "" {
		if err := outbound.ConfigurePool(nil); err != nil {
			return err
		}
	} else if err := outbound.RemoveProxy(raw); err != nil {
		return err
	}
	if err := s.persistProxyPool(); err != nil {
		_ = outbound.ConfigurePool(previous)
		return err
	}
	return nil
}

// deleteSelectedProxies removes only ids present in the active pool. IDs are
// derived from scheme + host:port and never expose proxy credentials.
func (s *Server) deleteSelectedProxies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}

	ids := make(map[string]struct{}, len(body.IDs))
	for _, id := range body.IDs {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		writeOpenAIError(w, 400, "invalid_request_error", "at least one proxy id is required")
		return
	}

	deleted, missing, err := s.deleteProxyIDs(ids)
	if err != nil {
		writeOpenAIError(w, 500, "storage_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{
		"ok": true, "deleted": deleted, "missing": missing,
		"proxies": outbound.ProxyPoolStatusRedacted(),
	})
}

func (s *Server) deleteProxyIDs(ids map[string]struct{}) (deleted, missing int, err error) {
	proxyPoolMutationMu.Lock()
	defer proxyPoolMutationMu.Unlock()

	previous := outbound.ProxyPoolRawURLs()
	remove := make(map[string]struct{}, len(ids))
	for id := range ids {
		if raw := outbound.ProxyRawURLForID(id); raw != "" {
			remove[raw] = struct{}{}
		} else {
			missing++
		}
	}
	if len(remove) == 0 {
		return 0, missing, nil
	}
	next := make([]string, 0, len(previous)-len(remove))
	for _, raw := range previous {
		if _, selected := remove[raw]; selected {
			deleted++
			continue
		}
		next = append(next, raw)
	}
	if err := outbound.ConfigurePool(next); err != nil {
		return 0, missing, err
	}
	if err := s.persistProxyPoolURLs(next); err != nil {
		_ = outbound.ConfigurePool(previous)
		return 0, missing, err
	}
	return deleted, missing, nil
}
