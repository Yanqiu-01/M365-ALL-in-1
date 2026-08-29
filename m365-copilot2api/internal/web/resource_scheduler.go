package web

import (
	"sort"
	"sync"
	"time"

	"m365-copilot2api/internal/auth"
)

// resourceScheduler provides health-aware, least-loaded scheduling for
// accounts owned by or explicitly authorized for this gateway. It does not
// attempt to conceal traffic or bypass provider controls. Provider rate limits
// and Retry-After responses are honored through accountHealth.
type resourceScheduler struct {
	mu       sync.Mutex
	selected map[string]uint64
	last     map[string]time.Time
}

type schedulerCandidate struct {
	account  auth.AccountToken
	inflight int
	selected uint64
	last     time.Time
}

type schedulerResourceSnapshot struct {
	ID           string         `json:"id"`
	Email        string         `json:"email,omitempty"`
	Available    bool           `json:"available"`
	Inflight     int            `json:"inflight"`
	Selected     uint64         `json:"selected"`
	LastSelected string         `json:"last_selected,omitempty"`
	Health       map[string]any `json:"health,omitempty"`
}

type schedulerSnapshot struct {
	TotalAccounts     int                         `json:"total_accounts"`
	AvailableAccounts int                         `json:"available_accounts"`
	Selected          map[string]uint64           `json:"selected"`
	LastSelected      map[string]time.Time        `json:"last_selected"`
	Health            map[string]map[string]any   `json:"health"`
	Concurrency       map[string]any              `json:"concurrency"`
	Resources         []schedulerResourceSnapshot `json:"resources"`
}

func newResourceScheduler() *resourceScheduler {
	return &resourceScheduler{
		selected: make(map[string]uint64),
		last:     make(map[string]time.Time),
	}
}

// Select chooses the least-loaded healthy account. Ties are resolved by the
// number of previous selections and then by least-recent use. This distributes
// normal authorized workload fairly while allowing existing cooldown,
// authentication and concurrency controls to remove unsafe candidates.
func (s *resourceScheduler) Select(accounts []auth.AccountToken, available func(string) bool, inflight map[string]int) (auth.AccountToken, bool) {
	if s == nil || len(accounts) == 0 {
		return auth.AccountToken{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidates := make([]schedulerCandidate, 0, len(accounts))
	for _, account := range accounts {
		if account.ID == "" || (available != nil && !available(account.ID)) {
			continue
		}
		candidates = append(candidates, schedulerCandidate{
			account:  account,
			inflight: inflight[account.ID],
			selected: s.selected[account.ID],
			last:     s.last[account.ID],
		})
	}
	if len(candidates) == 0 {
		return auth.AccountToken{}, false
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.inflight != right.inflight {
			return left.inflight < right.inflight
		}
		if left.selected != right.selected {
			return left.selected < right.selected
		}
		if !left.last.Equal(right.last) {
			return left.last.Before(right.last)
		}
		return left.account.ID < right.account.ID
	})

	chosen := candidates[0].account
	s.selected[chosen.ID]++
	s.last[chosen.ID] = time.Now().UTC()
	return chosen, true
}

func (s *resourceScheduler) Snapshot(accounts []auth.AccountToken, available func(string) bool, health map[string]map[string]any, concurrency map[string]any) schedulerSnapshot {
	out := schedulerSnapshot{
		TotalAccounts: len(accounts),
		Selected:      map[string]uint64{},
		LastSelected:  map[string]time.Time{},
		Health:        health,
		Concurrency:   concurrency,
		Resources:     make([]schedulerResourceSnapshot, 0, len(accounts)),
	}
	inflight := map[string]int{}
	if raw, ok := concurrency["inflight"].(map[string]int); ok {
		inflight = raw
	}
	for _, account := range accounts {
		isAvailable := available == nil || available(account.ID)
		if isAvailable {
			out.AvailableAccounts++
		}
		out.Resources = append(out.Resources, schedulerResourceSnapshot{
			ID:        account.ID,
			Email:     account.Email,
			Available: isAvailable,
			Inflight:  inflight[account.ID],
			Health:    health[account.ID],
		})
	}
	if s != nil {
		s.mu.Lock()
		for id, count := range s.selected {
			out.Selected[id] = count
		}
		for id, selectedAt := range s.last {
			out.LastSelected[id] = selectedAt
		}
		for i := range out.Resources {
			id := out.Resources[i].ID
			out.Resources[i].Selected = s.selected[id]
			if selectedAt, ok := s.last[id]; ok {
				out.Resources[i].LastSelected = selectedAt.UTC().Format(time.RFC3339)
			}
		}
		s.mu.Unlock()
	}
	sort.Slice(out.Resources, func(i, j int) bool {
		left, right := out.Resources[i], out.Resources[j]
		if left.Email != right.Email {
			return left.Email < right.Email
		}
		return left.ID < right.ID
	})
	return out
}
