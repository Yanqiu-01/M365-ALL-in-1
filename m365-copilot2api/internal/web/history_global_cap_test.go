package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedResponseHistory fills the store with n entries for one tenant, oldest
// first, each carrying a content string of the requested size. Returns the ids
// in age order so a test can assert which end was evicted.
func seedResponseHistory(s *Server, tenant string, n int, contentBytes int, base time.Time) []string {
	if s.responseMessages == nil {
		s.responseMessages = map[string]map[string]respHistory{}
	}
	if s.responseMessages[tenant] == nil {
		s.responseMessages[tenant] = map[string]respHistory{}
	}
	ids := make([]string, 0, n)
	body := strings.Repeat("x", contentBytes)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("resp-%05d", i)
		ids = append(ids, id)
		s.responseMessages[tenant][id] = respHistory{
			At:       base.Add(time.Duration(i) * time.Second),
			Messages: []oaiMsg{{Role: "user", Content: body}},
		}
	}
	return ids
}

func totalStoredResponses(s *Server) int {
	n := 0
	for _, bucket := range s.responseMessages {
		n += len(bucket)
	}
	return n
}

// Under both ceilings nothing may be touched. A cap that trims a store which
// is already within budget would silently destroy usable context.
func TestResponseHistoryCapKeepsEverythingUnderBudget(t *testing.T) {
	s := &Server{}
	now := time.Now()
	ids := seedResponseHistory(s, "tenant-a", 50, 128, now.Add(-time.Minute))

	report := s.enforceResponseHistoryCapsWithLimits(2048, 128<<20, now)

	if report.EvictedCount != 0 || report.EvictedBytes != 0 || report.ExpiredEntries != 0 {
		t.Fatalf("nothing should be evicted under budget: %+v", report)
	}
	if got := totalStoredResponses(s); got != 50 {
		t.Fatalf("stored entries = %d, want 50", got)
	}
	if report.KeptEntries != 50 {
		t.Fatalf("report kept_entries = %d, want 50", report.KeptEntries)
	}
	if report.KeptBytes <= 0 {
		t.Fatalf("kept_bytes = %d, want a positive size estimate", report.KeptBytes)
	}
	for _, id := range ids {
		if _, ok := s.responseMessages["tenant-a"][id]; !ok {
			t.Fatalf("entry %q was evicted while under budget", id)
		}
	}
}

// Crossing the entry ceiling must evict oldest-first, matching the per-tenant
// eviction in rememberResponse and the LRU in sessionResolver.evictLocked.
func TestResponseHistoryCapEvictsOldestFirstOnCount(t *testing.T) {
	s := &Server{}
	now := time.Now()
	ids := seedResponseHistory(s, "tenant-a", 100, 64, now.Add(-time.Hour/2))

	report := s.enforceResponseHistoryCapsWithLimits(60, 128<<20, now)

	if report.EvictedCount != 40 {
		t.Fatalf("evicted_count = %d, want 40: %+v", report.EvictedCount, report)
	}
	if got := totalStoredResponses(s); got != 60 {
		t.Fatalf("stored entries = %d, want 60", got)
	}
	// The 40 oldest are gone; the 60 newest survive.
	for _, id := range ids[:40] {
		if _, ok := s.responseMessages["tenant-a"][id]; ok {
			t.Fatalf("older entry %q survived while a newer one was expected to win", id)
		}
	}
	for _, id := range ids[40:] {
		if _, ok := s.responseMessages["tenant-a"][id]; !ok {
			t.Fatalf("newer entry %q was evicted before an older one", id)
		}
	}
}

// The count ceiling is global, not per tenant: many tenants each holding a
// legal per-tenant bucket is the case the old per-tenant cap could not see.
func TestResponseHistoryCapIsGlobalAcrossTenants(t *testing.T) {
	s := &Server{}
	now := time.Now()
	base := now.Add(-time.Hour / 2)
	for i := 0; i < 8; i++ {
		seedResponseHistory(s, fmt.Sprintf("tenant-%d", i), 20, 64, base.Add(time.Duration(i)*time.Minute))
	}
	if got := totalStoredResponses(s); got != 160 {
		t.Fatalf("seeded entries = %d, want 160", got)
	}

	report := s.enforceResponseHistoryCapsWithLimits(100, 128<<20, now)

	if got := totalStoredResponses(s); got != 100 {
		t.Fatalf("stored entries = %d, want 100 across all tenants", got)
	}
	if report.EvictedCount != 60 {
		t.Fatalf("evicted_count = %d, want 60: %+v", report.EvictedCount, report)
	}
	// The three oldest tenants seeded first, so their buckets go first and the
	// emptied buckets must be reclaimed rather than left as map overhead.
	if report.DroppedTenants < 1 {
		t.Fatalf("emptied tenant buckets were not reclaimed: %+v", report)
	}
	if _, ok := s.responseMessages["tenant-0"]; ok {
		t.Fatal("oldest tenant bucket should have been emptied and dropped")
	}
	if len(s.responseMessages) != report.Tenants {
		t.Fatalf("report tenants = %d, store has %d", report.Tenants, len(s.responseMessages))
	}
}

// Crossing the byte ceiling evicts until the store is under budget, even when
// the entry count is legal. This is the axis that had no bound at all.
func TestResponseHistoryCapEvictsUntilUnderByteBudget(t *testing.T) {
	s := &Server{}
	now := time.Now()
	// 20 entries of ~64 KiB each: well under any entry ceiling, but 1.25 MiB
	// against a 256 KiB budget.
	ids := seedResponseHistory(s, "tenant-a", 20, 64<<10, now.Add(-time.Hour/2))
	budget := int64(256 << 10)

	report := s.enforceResponseHistoryCapsWithLimits(2048, budget, now)

	if report.EvictedCount != 0 {
		t.Fatalf("count ceiling should not have fired: %+v", report)
	}
	if report.EvictedBytes == 0 {
		t.Fatalf("byte ceiling did not evict anything: %+v", report)
	}
	if report.KeptBytes > budget {
		t.Fatalf("kept_bytes = %d, still over budget %d: %+v", report.KeptBytes, budget, report)
	}
	if report.OverBudget {
		t.Fatalf("store should be under budget after the sweep: %+v", report)
	}
	// Eviction stops as soon as the store fits: it must not empty the store.
	remaining := totalStoredResponses(s)
	if remaining == 0 {
		t.Fatal("byte eviction emptied the store instead of trimming to budget")
	}
	// Oldest-first again: the newest entry is the one a client would resume.
	newest := ids[len(ids)-1]
	if _, ok := s.responseMessages["tenant-a"][newest]; !ok {
		t.Fatalf("newest entry %q must survive byte eviction", newest)
	}
	if _, ok := s.responseMessages["tenant-a"][ids[0]]; ok {
		t.Fatal("oldest entry must be the first to go")
	}
}

// A single entry larger than the whole budget must not empty the store:
// dropping the only response breaks previous_response_id while freeing nothing
// that a global cap can protect. It is reported instead.
func TestResponseHistoryCapKeepsLastEntryAndReportsOverBudget(t *testing.T) {
	s := &Server{}
	now := time.Now()
	seedResponseHistory(s, "tenant-a", 1, 1<<20, now.Add(-time.Minute))

	report := s.enforceResponseHistoryCapsWithLimits(2048, 4<<10, now)

	if got := totalStoredResponses(s); got != 1 {
		t.Fatalf("stored entries = %d, want the single oversized entry kept", got)
	}
	if !report.OverBudget {
		t.Fatalf("an entry larger than the budget must be reported: %+v", report)
	}
}

// Expired entries are reclaimed in every bucket, not only in the bucket of the
// tenant currently being written. This is the idle-tenant leak.
func TestResponseHistoryCapSweepsExpiredInIdleBuckets(t *testing.T) {
	s := &Server{}
	now := time.Now()
	s.responseMessages = map[string]map[string]respHistory{
		"idle": {
			"stale": {At: now.Add(-responseHistoryRetention - time.Minute), Messages: []oaiMsg{{Role: "user", Content: "old"}}},
		},
		"busy": {
			"fresh": {At: now.Add(-time.Minute), Messages: []oaiMsg{{Role: "user", Content: "new"}}},
		},
	}

	report := s.enforceResponseHistoryCapsWithLimits(2048, 128<<20, now)

	if report.ExpiredEntries != 1 {
		t.Fatalf("expired_entries = %d, want 1: %+v", report.ExpiredEntries, report)
	}
	if _, ok := s.responseMessages["idle"]; ok {
		t.Fatal("idle tenant bucket must be reclaimed once empty")
	}
	if _, ok := s.responseMessages["busy"]["fresh"]; !ok {
		t.Fatal("a live entry must survive the expiry sweep")
	}
}

// A store that is already far over the ceiling trims to budget on the first
// sweep and keeps the newest entries, rather than being discarded wholesale.
func TestResponseHistoryCapDegradesGracefullyOnOversizedStore(t *testing.T) {
	s := &Server{}
	now := time.Now()
	ids := seedResponseHistory(s, "tenant-a", 500, 4<<10, now.Add(-time.Hour/2))

	report := s.enforceResponseHistoryCapsWithLimits(256, 512<<10, now)

	remaining := totalStoredResponses(s)
	if remaining == 0 || remaining > 256 {
		t.Fatalf("stored entries = %d, want a non-empty store within the 256 ceiling", remaining)
	}
	if report.KeptBytes > 512<<10 {
		t.Fatalf("kept_bytes = %d, want within the byte ceiling", report.KeptBytes)
	}
	newest := ids[len(ids)-1]
	if _, ok := s.responseMessages["tenant-a"][newest]; !ok {
		t.Fatalf("newest entry %q must survive trimming of an oversized store", newest)
	}
	// A second pass on an in-budget store is a no-op.
	again := s.enforceResponseHistoryCapsWithLimits(256, 512<<10, now)
	if again.changed() {
		t.Fatalf("second sweep of an in-budget store must be a no-op: %+v", again)
	}
}

func TestResponseHistoryCapIsConfigurable(t *testing.T) {
	t.Run("entries from env", func(t *testing.T) {
		t.Setenv("M365_RESPONSE_HISTORY_MAX", "512")
		if got := maxResponseHistoryEntries(); got != 512 {
			t.Fatalf("maxResponseHistoryEntries = %d, want 512", got)
		}
	})
	t.Run("bytes from env", func(t *testing.T) {
		t.Setenv("M365_RESPONSE_HISTORY_MAX_BYTES", "16777216")
		if got := maxResponseHistoryBytes(); got != 16<<20 {
			t.Fatalf("maxResponseHistoryBytes = %d, want %d", got, 16<<20)
		}
	})
	t.Run("sweep interval from env", func(t *testing.T) {
		t.Setenv("M365_RESPONSE_HISTORY_SWEEP_MINUTES", "2")
		if got := responseHistorySweepInterval(); got != 2*time.Minute {
			t.Fatalf("responseHistorySweepInterval = %s, want 2m", got)
		}
	})
	t.Run("defaults with no env", func(t *testing.T) {
		if got := maxResponseHistoryEntries(); got != defaultMaxResponseHistoryEntries {
			t.Fatalf("default entries = %d, want %d", got, defaultMaxResponseHistoryEntries)
		}
		if got := maxResponseHistoryBytes(); got != defaultMaxResponseHistoryBytes {
			t.Fatalf("default bytes = %d, want %d", got, defaultMaxResponseHistoryBytes)
		}
		if got := responseHistorySweepInterval(); got != defaultResponseHistorySweepMinutes*time.Minute {
			t.Fatalf("default interval = %s, want %dm", got, defaultResponseHistorySweepMinutes)
		}
	})
}

// A malformed or out-of-range override must fall back to the compiled default
// rather than silently weakening (or breaking) the bound.
func TestResponseHistoryCapMalformedOverrideFallsBackToDefault(t *testing.T) {
	for _, raw := range []string{"", "abc", "0", "-1", "12.5", " ", "1000001", "17"} {
		t.Run("entries="+raw, func(t *testing.T) {
			t.Setenv("M365_RESPONSE_HISTORY_MAX", raw)
			if got := maxResponseHistoryEntries(); got != defaultMaxResponseHistoryEntries {
				t.Fatalf("entries override %q = %d, want default %d", raw, got, defaultMaxResponseHistoryEntries)
			}
		})
	}
	for _, raw := range []string{"", "abc", "0", "-1", "1048576", "9999999999999", "1e9"} {
		t.Run("bytes="+raw, func(t *testing.T) {
			t.Setenv("M365_RESPONSE_HISTORY_MAX_BYTES", raw)
			if got := maxResponseHistoryBytes(); got != defaultMaxResponseHistoryBytes {
				t.Fatalf("bytes override %q = %d, want default %d", raw, got, defaultMaxResponseHistoryBytes)
			}
		})
	}
	for _, raw := range []string{"abc", "0", "-5", "1441"} {
		t.Run("interval="+raw, func(t *testing.T) {
			t.Setenv("M365_RESPONSE_HISTORY_SWEEP_MINUTES", raw)
			if got := responseHistorySweepInterval(); got != defaultResponseHistorySweepMinutes*time.Minute {
				t.Fatalf("interval override %q = %s, want default", raw, got)
			}
		})
	}
	// A below-floor entry count must not make the existing per-tenant
	// allowance unreachable.
	t.Run("entries floor is one per-tenant bucket", func(t *testing.T) {
		t.Setenv("M365_RESPONSE_HISTORY_MAX", fmt.Sprint(maxResponsesPerTenant-1))
		if got := maxResponseHistoryEntries(); got != defaultMaxResponseHistoryEntries {
			t.Fatalf("below-floor override = %d, want default %d", got, defaultMaxResponseHistoryEntries)
		}
		t.Setenv("M365_RESPONSE_HISTORY_MAX", fmt.Sprint(maxResponsesPerTenant))
		if got := maxResponseHistoryEntries(); got != maxResponsesPerTenant {
			t.Fatalf("at-floor override = %d, want %d", got, maxResponsesPerTenant)
		}
	})
}

// The byte estimator must see payloads that contentToString drops, otherwise
// the byte ceiling is blind to the entries that actually make the store large.
func TestResponseHistoryEntryBytesCountsNonTextContent(t *testing.T) {
	image := strings.Repeat("A", 32<<10)
	history := respHistory{Messages: []oaiMsg{{
		Role: "user",
		Content: []any{map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/png;base64," + image},
		}},
	}}}
	if got := responseHistoryEntryBytes(history); got < int64(len(image)) {
		t.Fatalf("entry bytes = %d, want at least the %d-byte image payload", got, len(image))
	}
	// contentToString returns "" for this shape, which is why estimateContextBytes
	// is not reused for the cap.
	if text := contentToString(history.Messages[0].Content); text != "" {
		t.Fatalf("contentToString returned %q; the estimator premise changed", text)
	}
}

// The sweep must not corrupt the store: a surviving entry stays readable
// through the normal accessor.
func TestResponseHistoryCapLeavesSurvivorsReadable(t *testing.T) {
	s := &Server{}
	now := time.Now()
	seedResponseHistory(s, "tenant-a", 10, 128, now.Add(-time.Minute))

	s.enforceResponseHistoryCapsWithLimits(4, 128<<20, now)

	if got := totalStoredResponses(s); got != 4 {
		t.Fatalf("stored entries = %d, want 4", got)
	}
	if _, ok := s.loadResponseHistory("tenant-a", "resp-00009"); !ok {
		t.Fatal("newest survivor must remain loadable after the sweep")
	}
}
