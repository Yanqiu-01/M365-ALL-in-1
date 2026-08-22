package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AccountToken struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"displayName,omitempty"`
	Status       string    `json:"status"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	OID          string    `json:"oid,omitempty"`
	TID          string    `json:"tid,omitempty"`
	ClientID     string    `json:"clientId,omitempty"`
}

type Cache struct {
	Accounts []AccountToken `json:"accounts"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	data     Cache
	nextIdx  int
	inflight map[string]*inflightRefresh
	refresh  func(context.Context, string) (TokenSet, error)
}

// inflightRefresh coalesces concurrent EnsureValid refreshes for the same
// account: AAD refresh tokens can only be redeemed once, so a stampede of
// concurrent requests must not each try Refresh().
type inflightRefresh struct {
	done chan struct{}
	acc  AccountToken
	err  error
}

func CachePath() string {
	if dir := os.Getenv("M365_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "accounts.json")
	}
	if p := os.Getenv("M365_CONFIG"); p != "" {
		return p
	}
	if p := os.Getenv("M365_TOKEN_CACHE"); p != "" {
		return p
	}
	if p := os.Getenv("M365_TOKEN_FILE"); p != "" {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return filepath.Join(".", ".config", "m365-copilot2api", "accounts.json")
	}
	return filepath.Join(h, ".config", "m365-copilot2api", "accounts.json")
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = CachePath()
	}
	s := &Store{path: path, data: Cache{Accounts: []AccountToken{}}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	// Normalize oid/tid for older cache entries.
	for i := range s.data.Accounts {
		a := &s.data.Accounts[i]
		if a.OID == "" {
			a.OID = a.ID
		}
		if a.ID == "" {
			a.ID = a.OID
		}
	}
	return s, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		// /tmp has no nested dir needs usually; ignore if parent is root-ish
		if filepath.Dir(s.path) != "/" && filepath.Dir(s.path) != "." {
			// still try write below
		}
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.path, b, 0o600)
}

func atomicWrite(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) List() []AccountToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountToken, len(s.data.Accounts))
	copy(out, s.data.Accounts)
	return out
}

func (s *Store) UpdateRefreshToken(id, refreshToken string) error {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id {
			s.data.Accounts[i].RefreshToken = refreshToken
			s.data.Accounts[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return errors.New("account not found")
}

func (s *Store) Upsert(tok TokenSet) (AccountToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tok.HomeOID
	if id == "" {
		id = tok.Email
	}
	if id == "" {
		id = "account-" + time.Now().Format("150405")
	}
	acc := AccountToken{
		ID:           id,
		Email:        tok.Email,
		DisplayName:  tok.DisplayName,
		Status:       "online",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		UpdatedAt:    time.Now(),
		OID:          firstNonEmpty(tok.HomeOID, id),
		TID:          tok.TenantID,
		ClientID:     ClientID(),
	}
	found := false
	for i, existing := range s.data.Accounts {
		if existing.ID == acc.ID || (acc.Email != "" && existing.Email == acc.Email) {
			if acc.RefreshToken == "" {
				acc.RefreshToken = existing.RefreshToken
			}
			if acc.TID == "" {
				acc.TID = existing.TID
			}
			if acc.OID == "" {
				acc.OID = existing.OID
			}
			s.data.Accounts[i] = acc
			found = true
			break
		}
	}
	if !found {
		s.data.Accounts = append(s.data.Accounts, acc)
	}
	return acc, s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data.Accounts[:0]
	for _, a := range s.data.Accounts {
		if a.ID != id {
			next = append(next, a)
		}
	}
	s.data.Accounts = next
	return s.saveLocked()
}

func (s *Store) Get(id string) (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.data.Accounts {
		if a.ID == id || a.OID == id || a.Email == id {
			return a, true
		}
	}
	return AccountToken{}, false
}

func (s *Store) First() (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Accounts) == 0 {
		return AccountToken{}, false
	}
	return s.data.Accounts[0], true
}

// Next returns the next account in round-robin order.
func (s *Store) Next() (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.data.Accounts)
	if n == 0 {
		return AccountToken{}, false
	}
	acc := s.data.Accounts[s.nextIdx%n]
	s.nextIdx = (s.nextIdx + 1) % n
	return acc, true
}

func (s *Store) EnsureValid(id string) (AccountToken, error) {
	ctx, cancel := context.WithTimeout(context.Background(), authRequestTimeout)
	defer cancel()
	return s.EnsureValidContext(ctx, id)
}

// EnsureValidContext coalesces refreshes using the latest stored account state.
// Waiters can leave when their context is cancelled instead of blocking forever.
func (s *Store) EnsureValidContext(ctx context.Context, id string) (AccountToken, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	idx, acc, ok := s.findAccountLocked(id)
	if !ok {
		s.mu.Unlock()
		return AccountToken{}, os.ErrNotExist
	}
	if time.Now().Before(acc.ExpiresAt.Add(-30 * time.Second)) {
		s.mu.Unlock()
		return acc, nil
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return acc, err
	}
	if acc.RefreshToken == "" {
		acc.Status = "expired"
		acc.UpdatedAt = time.Now()
		s.data.Accounts[idx] = acc
		saveErr := s.saveLocked()
		s.mu.Unlock()
		if saveErr != nil {
			return acc, errors.Join(fmtExpired(), saveErr)
		}
		return acc, fmtExpired()
	}
	if s.inflight == nil {
		s.inflight = map[string]*inflightRefresh{}
	}
	if f, ok := s.inflight[acc.ID]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.acc, f.err
		case <-ctx.Done():
			return acc, ctx.Err()
		}
	}
	f := &inflightRefresh{done: make(chan struct{})}
	s.inflight[acc.ID] = f
	refresh := s.refresh
	if refresh == nil {
		refresh = RefreshContext
	}
	usedRefreshToken := acc.RefreshToken
	s.mu.Unlock()

	tok, refreshErr := refresh(ctx, usedRefreshToken)

	s.mu.Lock()
	f.acc, f.err = s.applyRefreshResultLocked(acc, usedRefreshToken, tok, refreshErr)
	delete(s.inflight, acc.ID)
	close(f.done)
	s.mu.Unlock()
	return f.acc, f.err
}

func (s *Store) findAccountLocked(id string) (int, AccountToken, bool) {
	for i, acc := range s.data.Accounts {
		if acc.ID == id || acc.OID == id || acc.Email == id {
			return i, acc, true
		}
	}
	return -1, AccountToken{}, false
}

func (s *Store) applyRefreshResultLocked(original AccountToken, usedRefreshToken string, tok TokenSet, refreshErr error) (AccountToken, error) {
	idx, current, ok := s.findAccountLocked(original.ID)
	if !ok {
		return AccountToken{}, os.ErrNotExist
	}
	if refreshErr != nil {
		if isPermanentRefreshError(refreshErr) && current.RefreshToken == usedRefreshToken {
			current.Status = "expired"
			current.UpdatedAt = time.Now()
			s.data.Accounts[idx] = current
			if err := s.saveLocked(); err != nil {
				return current, errors.Join(refreshErr, err)
			}
		}
		return current, refreshErr
	}
	if current.RefreshToken != usedRefreshToken {
		if time.Now().Before(current.ExpiresAt.Add(-30 * time.Second)) {
			return current, nil
		}
		return current, errors.New("token refresh superseded by a concurrent account update")
	}
	if tok.AccessToken == "" {
		return current, errors.New("refresh returned an empty access token")
	}

	updated := current
	updated.Status = "online"
	updated.AccessToken = tok.AccessToken
	updated.RefreshToken = firstNonEmpty(tok.RefreshToken, current.RefreshToken)
	updated.ExpiresAt = tok.ExpiresAt
	updated.UpdatedAt = time.Now()
	updated.Email = firstNonEmpty(tok.Email, current.Email)
	updated.DisplayName = firstNonEmpty(tok.DisplayName, current.DisplayName)
	updated.OID = firstNonEmpty(tok.HomeOID, current.OID, current.ID)
	updated.TID = firstNonEmpty(tok.TenantID, current.TID)
	if updated.ClientID == "" {
		updated.ClientID = ClientID()
	}
	s.data.Accounts[idx] = updated
	if err := s.saveLocked(); err != nil {
		return updated, err
	}
	return updated, nil
}

func isPermanentRefreshError(err error) bool {
	var oauthErr *OAuthError
	return errors.As(err, &oauthErr) && oauthErr.Code == "invalid_grant"
}

func fmtExpired() error {
	return errors.New("token_expired: refresh token missing or expired")
}

func (s *Store) RefreshAllExpired() []TokenRefreshResult {
	s.mu.Lock()
	candidates := make([]AccountToken, 0, len(s.data.Accounts))
	for _, a := range s.data.Accounts {
		if time.Now().After(a.ExpiresAt.Add(-30*time.Second)) && a.RefreshToken != "" {
			candidates = append(candidates, a)
		}
	}
	s.mu.Unlock()
	var results []TokenRefreshResult
	for _, a := range candidates {
		acc, err := s.EnsureValid(a.ID)
		r := TokenRefreshResult{ID: a.ID, Email: a.Email}
		if err != nil {
			r.Success = false
			r.Error = err.Error()
		} else {
			r.Success = true
			r.ExpiresAt = acc.ExpiresAt
		}
		results = append(results, r)
	}
	return results
}

type TokenRefreshResult struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}
