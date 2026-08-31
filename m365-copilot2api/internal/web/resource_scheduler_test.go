package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func TestResourceSchedulerBalancesHealthyAccounts(t *testing.T) {
	scheduler := newResourceScheduler()
	accounts := []auth.AccountToken{
		{ID: "account-a", Email: "a@example.test"},
		{ID: "account-b", Email: "b@example.test"},
		{ID: "account-c", Email: "c@example.test"},
	}

	selected := map[string]int{}
	for i := 0; i < 30; i++ {
		account, ok := scheduler.Select(accounts, func(string) bool { return true }, map[string]int{})
		if !ok {
			t.Fatal("expected an available account")
		}
		selected[account.ID]++
	}

	for _, account := range accounts {
		if selected[account.ID] != 10 {
			t.Fatalf("account %s selected %d times, want 10", account.ID, selected[account.ID])
		}
	}
}

func TestResourceSchedulerSkipsUnavailableAndPrefersLeastLoaded(t *testing.T) {
	scheduler := newResourceScheduler()
	accounts := []auth.AccountToken{
		{ID: "unhealthy", Email: "unhealthy@example.test"},
		{ID: "busy", Email: "busy@example.test"},
		{ID: "idle", Email: "idle@example.test"},
	}
	available := func(id string) bool { return id != "unhealthy" }
	inflight := map[string]int{"busy": 4, "idle": 0}

	account, ok := scheduler.Select(accounts, available, inflight)
	if !ok {
		t.Fatal("expected a healthy account")
	}
	if account.ID != "idle" {
		t.Fatalf("selected %q, want idle", account.ID)
	}
}

func TestResourceSchedulerSnapshotIncludesPerAccountState(t *testing.T) {
	scheduler := newResourceScheduler()
	accounts := []auth.AccountToken{
		{ID: "account-b", Email: "b@example.test"},
		{ID: "account-a", Email: "a@example.test"},
	}
	available := func(id string) bool { return id != "account-b" }
	if selected, ok := scheduler.Select(accounts, available, map[string]int{"account-a": 2}); !ok || selected.ID != "account-a" {
		t.Fatalf("selected=%q ok=%v, want account-a", selected.ID, ok)
	}

	snapshot := scheduler.Snapshot(
		accounts,
		available,
		map[string]map[string]any{"account-b": {"cooldownUntil": time.Now().Add(time.Minute)}},
		map[string]any{"limit": 64, "inflight": map[string]int{"account-a": 2}},
	)
	if snapshot.TotalAccounts != 2 || snapshot.AvailableAccounts != 1 {
		t.Fatalf("unexpected totals: %+v", snapshot)
	}
	if len(snapshot.Resources) != 2 {
		t.Fatalf("resources=%d want 2", len(snapshot.Resources))
	}
	first, second := snapshot.Resources[0], snapshot.Resources[1]
	if first.ID != "account-a" || !first.Available || first.Inflight != 2 || first.Selected != 1 || first.LastSelected == "" {
		t.Fatalf("unexpected first resource: %+v", first)
	}
	if second.ID != "account-b" || second.Available || second.Health == nil {
		t.Fatalf("unexpected second resource: %+v", second)
	}
}

func TestContributionLedgerAggregatesUsageByResource(t *testing.T) {
	now := time.Now().UTC()
	usage := &usageLog{records: []UsageRecord{
		{
			Time:         now.Add(-time.Hour),
			AccountEmail: "owner-a@example.test",
			InputTokens:  100,
			OutputTokens: 50,
			CacheTokens:  25,
			DurationMs:   1200,
			Status:       http.StatusOK,
		},
		{
			Time:         now.Add(-30 * time.Minute),
			AccountEmail: "owner-a@example.test",
			InputTokens:  40,
			OutputTokens: 10,
			DurationMs:   800,
			Status:       http.StatusTooManyRequests,
		},
		{
			Time:         now.Add(-15 * time.Minute),
			AccountEmail: "owner-b@example.test",
			InputTokens:  20,
			OutputTokens: 5,
			DurationMs:   200,
			Status:       http.StatusOK,
		},
		{
			Time:         now.AddDate(0, 0, -60),
			AccountEmail: "outside-window@example.test",
			InputTokens:  999,
			Status:       http.StatusOK,
		},
	}}
	server := &Server{usage: usage}

	request := httptest.NewRequest(http.MethodGet, "/api/contributions/ledger?days=30", nil)
	response := httptest.NewRecorder()
	server.contributionLedger(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	var payload struct {
		Days    int `json:"days"`
		Summary struct {
			Resources int   `json:"resources"`
			Requests  int64 `json:"requests"`
			Tokens    int64 `json:"tokens"`
		} `json:"summary"`
		Ledger []contributionEntry `json:"ledger"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Days != 30 {
		t.Fatalf("days=%d want 30", payload.Days)
	}
	if payload.Summary.Resources != 2 || payload.Summary.Requests != 3 || payload.Summary.Tokens != 250 {
		t.Fatalf("unexpected summary: %+v", payload.Summary)
	}
	if len(payload.Ledger) != 2 {
		t.Fatalf("ledger entries=%d want 2", len(payload.Ledger))
	}

	first := payload.Ledger[0]
	if first.AccountEmail != "owner-a@example.test" {
		t.Fatalf("first account=%q want owner-a@example.test", first.AccountEmail)
	}
	if first.Requests != 2 || first.Successes != 1 || first.Failures != 1 || first.TotalTokens != 225 {
		t.Fatalf("unexpected first ledger entry: %+v", first)
	}
}

func TestContributionLedgerRejectsUnsupportedMethod(t *testing.T) {
	server := &Server{usage: &usageLog{}}
	request := httptest.NewRequest(http.MethodPost, "/api/contributions/ledger", nil)
	response := httptest.NewRecorder()

	server.contributionLedger(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want %d", response.Code, http.StatusMethodNotAllowed)
	}
}
