package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// contributionEntry is the accounting view for one authorized resource.
// It deliberately contains usage totals only and never exposes access or
// refresh tokens.
type contributionEntry struct {
	AccountEmail string `json:"account_email"`
	Requests     int64  `json:"requests"`
	Successes    int64  `json:"successes"`
	Failures     int64  `json:"failures"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CacheTokens  int64  `json:"cache_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	DurationMs   int64  `json:"duration_ms"`
	LastUsedAt   string `json:"last_used_at,omitempty"`
}

func (s *Server) resourceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s == nil || s.tokens == nil {
		jsonOut(w, map[string]any{
			"scheduler": map[string]any{
				"total_accounts":     0,
				"available_accounts": 0,
			},
		})
		return
	}

	accounts := s.tokens.List()
	health := map[string]map[string]any{}
	if s.accountPool != nil {
		health = s.accountPool.Snapshot()
	}
	concurrency := map[string]any{
		"limit":    defaultAccountConcurrency,
		"inflight": map[string]int{},
	}
	if s.accountConcurrency != nil {
		concurrency = s.accountConcurrency.Snapshot()
	}
	// 同 server.go 处：调度器由 New() 构造，这里的惰性初始化到不了，且会在
	// 请求路径上无锁写共享字段。

	available := func(string) bool { return true }
	if s.accountPool != nil && s.accountConcurrency != nil {
		available = s.accountAvailable
	}

	jsonOut(w, map[string]any{
		"scheduler": s.resourceScheduler.Snapshot(
			accounts,
			available,
			health,
			concurrency,
		),
	})
}

func (s *Server) contributionLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	days := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 3650 {
			days = parsed
		}
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	entries := map[string]*contributionEntry{}
	if s != nil && s.usage != nil {
		for _, record := range s.usage.snapshotRecords() {
			if record.Time.Before(cutoff) {
				continue
			}
			email := strings.TrimSpace(record.AccountEmail)
			if email == "" {
				email = "unattributed"
			}
			entry := entries[email]
			if entry == nil {
				entry = &contributionEntry{AccountEmail: email}
				entries[email] = entry
			}
			entry.Requests++
			if record.Status >= 200 && record.Status < 400 {
				entry.Successes++
			} else {
				entry.Failures++
			}
			entry.InputTokens += record.InputTokens
			entry.OutputTokens += record.OutputTokens
			entry.CacheTokens += record.CacheTokens
			entry.TotalTokens += record.InputTokens + record.OutputTokens + record.CacheTokens
			entry.DurationMs += record.DurationMs
			if entry.LastUsedAt == "" || record.Time.Format(time.RFC3339Nano) > entry.LastUsedAt {
				entry.LastUsedAt = record.Time.UTC().Format(time.RFC3339Nano)
			}
		}
	}

	ledger := make([]contributionEntry, 0, len(entries))
	var totalRequests, totalTokens int64
	for _, entry := range entries {
		ledger = append(ledger, *entry)
		totalRequests += entry.Requests
		totalTokens += entry.TotalTokens
	}
	sort.Slice(ledger, func(i, j int) bool {
		if ledger[i].TotalTokens != ledger[j].TotalTokens {
			return ledger[i].TotalTokens > ledger[j].TotalTokens
		}
		if ledger[i].Requests != ledger[j].Requests {
			return ledger[i].Requests > ledger[j].Requests
		}
		return ledger[i].AccountEmail < ledger[j].AccountEmail
	})

	jsonOut(w, map[string]any{
		"days": days,
		"summary": map[string]any{
			"resources": len(ledger),
			"requests":  totalRequests,
			"tokens":    totalTokens,
		},
		"ledger": ledger,
	})
}
