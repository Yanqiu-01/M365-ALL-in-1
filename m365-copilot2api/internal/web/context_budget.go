package web

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"m365-copilot2api/internal/chathub"
)

// configuredContextBudget is the usable input context recovered from the APK:
// configured context window less the configured output reservation.
func configuredContextBudget() int {
	settings := currentSettings()
	window := settings.ContextWindow
	output := settings.MaxOutputTokens
	if window < 1024 {
		window = 128000
	}
	if output < 1 || output >= window {
		output = 16384
		if output >= window {
			output = window / 4
		}
	}
	return window - output
}

func messageTokenCost(message oaiMsg, model string) int {
	count, _ := tokenEstimator(model)
	cost := messageProtocolTokens + count(strings.TrimSpace(message.Role))
	cost += serializedTokenCount(message.Content, count)
	cost += count(message.Name)
	cost += count(message.ToolCallID)
	for _, call := range message.ToolCalls {
		cost += serializedTokenCount(call, count)
	}
	return cost
}

func toolSchemaTokenCost(tools []chathub.Tool, toolChoice any, model string) int {
	count, _ := tokenEstimator(model)
	cost := 0
	for _, tool := range tools {
		cost += toolProtocolTokens + serializedTokenCount(tool, count)
	}
	if toolChoice != nil {
		cost += toolChoiceProtocolTokens + serializedTokenCount(toolChoice, count)
	}
	return cost
}

func isInstructionRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		return true
	default:
		return false
	}
}

type messageGroup struct {
	messages []oaiMsg
	cost     int
}

// messageGroups preserves message turn boundaries: system/developer messages
// are returned separately, while every user message and its following
// assistant/tool messages form one evictable group.
func messageGroups(messages []oaiMsg, model string) (instructions []oaiMsg, groups []messageGroup) {
	var current *messageGroup
	for _, message := range messages {
		if isInstructionRole(message.Role) {
			instructions = append(instructions, message)
			continue
		}
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") || current == nil {
			groups = append(groups, messageGroup{})
			current = &groups[len(groups)-1]
		}
		current.messages = append(current.messages, message)
		current.cost += messageTokenCost(message, model)
	}
	return instructions, groups
}

func trimMessagesToContext(messages []oaiMsg, tools []chathub.Tool, toolChoice any, model string) ([]oaiMsg, error) {
	return trimMessagesWithBudget(messages, tools, toolChoice, model, configuredContextBudget())
}

// trimMessagesWithBudget keeps all instruction messages and the newest complete
// conversation turns that fit the budget. It never returns a dangling tool
// result without its preceding user/assistant context.
func trimMessagesWithBudget(messages []oaiMsg, tools []chathub.Tool, toolChoice any, model string, budget int) ([]oaiMsg, error) {
	if budget < 1 {
		return nil, fmt.Errorf("context budget must be positive")
	}
	instructions, groups := messageGroups(messages, model)
	used := toolSchemaTokenCost(tools, toolChoice, model)
	for _, message := range instructions {
		used += messageTokenCost(message, model)
	}
	if used > budget {
		return nil, fmt.Errorf("instruction and tool schemas exceed context budget (%d > %d tokens)", used, budget)
	}

	keepFrom := len(groups)
	for i := len(groups) - 1; i >= 0; i-- {
		if used+groups[i].cost > budget {
			break
		}
		used += groups[i].cost
		keepFrom = i
	}

	out := make([]oaiMsg, 0, len(instructions)+len(messages))
	out = append(out, instructions...)
	for _, group := range groups[keepFrom:] {
		out = append(out, group.messages...)
	}
	if len(out) == 0 && len(messages) > 0 {
		return nil, fmt.Errorf("latest message exceeds context budget (%d tokens)", budget)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Global ceiling for the Responses history store (s.responseMessages).
//
// What already existed before this block, and what did not:
//
//	response_history.go:157-161  drops entries older than one hour, but only
//	                             inside the bucket of the tenant being written.
//	response_history.go:164-176  caps ONE tenant at maxResponsesPerTenant,
//	                             evicting that bucket's oldest entry.
//	response_history.go:123-126  drops an expired entry when it is read.
//	server.go:250                maxResponsesPerTenant = 256, a compile-time
//	                             constant with no operator override.
//
// Nothing bounded the store as a whole. The tenant buckets at
// response_history.go:149-153 are created on demand and never removed, so the
// resident set was 256 entries x however many tenants have ever appeared, with
// no accounting of entry size at all. A single observed live request carried
// raw_bytes=1618794, which at the per-tenant cap alone is ~410 MB for one
// tenant. A tenant that stops writing also keeps its whole bucket forever,
// because the only sweep of expired entries runs on that tenant's own write
// path.
//
// The ceiling below is therefore expressed on the three axes that were open:
// total entry count, total estimated bytes, and idle buckets. Eviction is
// oldest-first by respHistory.At, matching both the per-tenant eviction in
// rememberResponse and the LRU in sessionResolver.evictLocked.
// ---------------------------------------------------------------------------

const (
	// responseHistoryRetention mirrors the one-hour window already enforced on
	// the read and write paths. It is restated (not changed) so the global
	// sweep cannot drift from per-tenant behaviour.
	responseHistoryRetention = time.Hour

	// defaultMaxResponseHistoryEntries bounds live entries across every tenant.
	// 2048 is eight full per-tenant buckets (8 x 256): a handful of busy
	// tenants keep their entire existing allowance, so normal traffic never
	// reaches this. It only truncates the pathological case the per-tenant cap
	// cannot see, namely many tenants each holding a full bucket.
	defaultMaxResponseHistoryEntries = 2048

	// defaultMaxResponseHistoryBytes bounds the estimated resident size of the
	// whole store. 128 MiB still holds ~80 chains the size of the largest
	// entry actually observed in production (1.6 MB), and far more at typical
	// sizes, while capping a store that previously had no size bound at all.
	// It sits below the session store's own worst case (100 sessions x 4 MiB,
	// session_resolver.go:99,114) so response history cannot become the
	// dominant consumer.
	defaultMaxResponseHistoryBytes = int64(128 << 20)

	// defaultResponseHistorySweepMinutes is deliberately much tighter than the
	// 30-minute cloud-conversation cleanup: this sweep bounds process memory,
	// and the interval is the window during which growth is still unbounded.
	defaultResponseHistorySweepMinutes = 5
)

// maxResponseHistoryEntries resolves the global entry ceiling using the same
// precedence as maxToolRounds (agent_ledger.go:319): an explicit environment
// variable wins and a malformed or out-of-range value there falls back to the
// compiled default rather than silently weakening the bound; otherwise the
// persisted setting applies; otherwise the default.
//
// The settings tier reaches this function through settingsEnvOverrides
// (settings.go:469-508), which is how every restart-scoped limit in this
// package is wired -- M365_SESSION_MAX at settings.go:493 is the direct
// precedent. Adding a console field is therefore a one-line
// putInt("M365_RESPONSE_HISTORY_MAX", s.ResponseHistoryMax) plus the struct
// field; no change is needed here. settings.go is outside the edit scope of
// this change, so that field is not added yet.
func maxResponseHistoryEntries() int {
	if raw, ok := os.LookupEnv("M365_RESPONSE_HISTORY_MAX"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n >= maxResponsesPerTenant && n <= 1_000_000 {
			return n
		}
		// A floor of one full per-tenant bucket keeps the global cap from
		// making the existing per-tenant allowance unreachable. Below that it
		// would be feature damage, not a guard rail -- the same reasoning as
		// the 64KB floor at session_resolver.go:164.
		return defaultMaxResponseHistoryEntries
	}
	return defaultMaxResponseHistoryEntries
}

// maxResponseHistoryBytes resolves the global byte ceiling. Same precedence
// and same fall-back-to-default-on-garbage rule as maxResponseHistoryEntries.
func maxResponseHistoryBytes() int64 {
	if raw, ok := os.LookupEnv("M365_RESPONSE_HISTORY_MAX_BYTES"); ok {
		// Floor of 4 MiB: the budget must comfortably hold more than one
		// largest-observed entry (1.6 MB), otherwise the cap would evict live
		// context on every ordinary request.
		if n, e := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); e == nil && n >= 4<<20 && n <= 8<<30 {
			return n
		}
		return defaultMaxResponseHistoryBytes
	}
	return defaultMaxResponseHistoryBytes
}

// responseHistorySweepInterval resolves how often the ceiling is enforced.
func responseHistorySweepInterval() time.Duration {
	minutes := defaultResponseHistorySweepMinutes
	if raw, ok := os.LookupEnv("M365_RESPONSE_HISTORY_SWEEP_MINUTES"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n >= 1 && n <= 1440 {
			minutes = n
		}
	}
	return time.Duration(minutes) * time.Minute
}

// responseHistoryValueBytes walks a decoded JSON value and sums its payload
// size without serialising it.
//
// estimateContextBytes (history_archive.go:116) is not reusable here: it sizes
// content through contentToString, which returns "" for image_url parts and
// for any non-text block (server.go:1549-1577). Base64 image payloads are
// exactly the entries that make this store large, so sizing them as zero would
// leave the byte cap blind to its main target. The type switch mirrors
// cloneResponseValue (response_history.go:47) so both agree on the shapes the
// request decoder can produce.
func responseHistoryValueBytes(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		return int64(len(typed))
	case []byte:
		return int64(len(typed))
	case map[string]any:
		// 2 bytes per key for JSON punctuation, matching the intent of the
		// flat +32 per message in estimateContextBytes.
		var total int64
		for key, item := range typed {
			total += int64(len(key)) + 2 + responseHistoryValueBytes(item)
		}
		return total
	case []any:
		var total int64
		for _, item := range typed {
			total += responseHistoryValueBytes(item) + 1
		}
		return total
	case []map[string]any:
		var total int64
		for _, item := range typed {
			total += responseHistoryValueBytes(item) + 1
		}
		return total
	case map[string]string:
		var total int64
		for key, item := range typed {
			total += int64(len(key)+len(item)) + 2
		}
		return total
	case []string:
		var total int64
		for _, item := range typed {
			total += int64(len(item)) + 1
		}
		return total
	default:
		// Numbers and booleans. Their in-memory cost is fixed and small; a
		// nominal constant keeps large arrays of scalars from sizing as free.
		return 8
	}
}

// responseHistoryEntryBytes estimates one stored response's resident size.
func responseHistoryEntryBytes(history respHistory) int64 {
	var total int64
	for _, message := range history.Messages {
		total += int64(len(message.Role) + len(message.Name) + len(message.ToolCallID) + len(message.ReasoningContent))
		total += responseHistoryValueBytes(message.Content)
		for _, call := range message.ToolCalls {
			total += responseHistoryValueBytes(call)
		}
		// Per-message struct and JSON framing overhead.
		total += 64
	}
	return total
}

// responseHistorySweep reports what one enforcement pass did. Counts and byte
// totals only: no response ids, no tenant keys, no message content. Tenant
// keys are derived from the caller's API key (requestTenantKey,
// session_resolver.go:904), so emitting one would leak account identity into
// the log. Same content-free rule as routerOutcome (router_outcome.go:7).
type responseHistorySweep struct {
	ExpiredEntries int
	EvictedCount   int
	EvictedBytes   int
	DroppedTenants int
	KeptEntries    int
	KeptBytes      int64
	Tenants        int
	// OverBudget is true when a single retained entry still exceeds the byte
	// ceiling on its own. The sweep keeps it rather than emptying the store,
	// so this flags a per-entry size problem that a global cap cannot fix.
	OverBudget bool
}

func (r responseHistorySweep) changed() bool {
	return r.ExpiredEntries > 0 || r.EvictedCount > 0 || r.EvictedBytes > 0 || r.DroppedTenants > 0
}

// enforceResponseHistoryCaps applies the configured global ceiling.
func (s *Server) enforceResponseHistoryCaps() responseHistorySweep {
	return s.enforceResponseHistoryCapsWithLimits(maxResponseHistoryEntries(), maxResponseHistoryBytes(), time.Now())
}

// enforceResponseHistoryCapsWithLimits is the testable seam, following the
// trimMessagesToContext / trimMessagesWithBudget split above: the exported
// behaviour reads configuration, the implementation takes explicit limits.
//
// The whole pass runs under responseMu because respHistory carries no cached
// size (it is declared in server.go, outside this change's scope) and sizing
// requires reading Messages. At the default five-minute cadence a bounded
// walk of an already-capped store is an acceptable cost for correctness
// against the request paths that hold the same lock briefly.
func (s *Server) enforceResponseHistoryCapsWithLimits(maxEntries int, maxBytes int64, now time.Time) responseHistorySweep {
	var report responseHistorySweep
	if s == nil {
		return report
	}
	if maxEntries < 1 {
		maxEntries = defaultMaxResponseHistoryEntries
	}
	if maxBytes < 1 {
		maxBytes = defaultMaxResponseHistoryBytes
	}

	s.responseMu.Lock()
	defer s.responseMu.Unlock()
	if s.responseMessages == nil {
		return report
	}

	// Pass 1: retire expired entries in every bucket, not just the one being
	// written. This is what reclaims an idle tenant's history.
	type slot struct {
		tenant string
		id     string
		at     time.Time
		bytes  int64
	}
	slots := make([]slot, 0, 64)
	var totalBytes int64
	for tenant, bucket := range s.responseMessages {
		for id, history := range bucket {
			if now.Sub(history.At) > responseHistoryRetention {
				delete(bucket, id)
				report.ExpiredEntries++
				continue
			}
			size := responseHistoryEntryBytes(history)
			totalBytes += size
			slots = append(slots, slot{tenant: tenant, id: id, at: history.At, bytes: size})
		}
	}

	// Oldest first, matching rememberResponse's per-tenant eviction. Map
	// iteration order is random, so tenant and id break At ties to keep the
	// eviction deterministic.
	sort.Slice(slots, func(i, j int) bool {
		if !slots[i].at.Equal(slots[j].at) {
			return slots[i].at.Before(slots[j].at)
		}
		if slots[i].tenant != slots[j].tenant {
			return slots[i].tenant < slots[j].tenant
		}
		return slots[i].id < slots[j].id
	})

	drop := func(index int) {
		victim := slots[index]
		if bucket := s.responseMessages[victim.tenant]; bucket != nil {
			delete(bucket, victim.id)
		}
		totalBytes -= victim.bytes
	}

	cursor := 0
	// Pass 2: entry-count ceiling.
	for len(slots)-cursor > maxEntries {
		drop(cursor)
		cursor++
		report.EvictedCount++
	}
	// Pass 3: byte ceiling. Stop at one surviving entry: evicting the newest
	// response would break the client's previous_response_id chain without
	// bounding anything meaningful.
	for totalBytes > maxBytes && len(slots)-cursor > 1 {
		drop(cursor)
		cursor++
		report.EvictedBytes++
	}
	if totalBytes > maxBytes {
		report.OverBudget = true
	}

	// Pass 4: a bucket emptied by any of the above is pure map overhead that
	// the previous code never reclaimed. Removing empties bounds the tenant
	// map by the number of tenants holding live entries, which the entry
	// ceiling already bounds.
	for tenant, bucket := range s.responseMessages {
		if len(bucket) == 0 {
			delete(s.responseMessages, tenant)
			report.DroppedTenants++
		}
	}

	report.KeptEntries = len(slots) - cursor
	report.KeptBytes = totalBytes
	report.Tenants = len(s.responseMessages)
	return report
}
