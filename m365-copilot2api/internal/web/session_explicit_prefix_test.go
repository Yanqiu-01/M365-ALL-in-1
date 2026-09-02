package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// tenantTestRequest builds a resolver request that carries an explicit tenant
// identity. requestTenantKey reads X-API-Key verbatim, so the key doubles as the
// readable tenant name in these tests.
func tenantTestRequest(apiKey, ip, ua string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = ip + ":12345"
	r.Header.Set("User-Agent", ua)
	if apiKey != "" {
		r.Header.Set("X-API-Key", apiKey)
	}
	return r
}

// victimHistory is deliberately disjoint in vocabulary from probeMessages below,
// so neither the strict-prefix nor the similarity fallback can match it. That
// isolates the explicit X-M365-Session-Id path as the ONLY path that could
// resolve these probes, which is what makes the assertions below meaningful.
func victimHistory() []oaiMsg {
	return []oaiMsg{
		{Role: "user", Content: "alpha beta gamma delta epsilon"},
		{Role: "assistant", Content: "zeta eta theta iota kappa"},
	}
}

func probeMessages() []oaiMsg {
	return []oaiMsg{{Role: "user", Content: "quuz corge grault garply waldo"}}
}

// bindVictimSession stores one session owned by tenant "victim-key" under a
// known session id, and returns the resolver plus the stored history length.
func bindVictimSession(t *testing.T) (*sessionResolver, int) {
	t.Helper()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))

	sr := openSessionResolver()
	sr.Bind("sess-victim", "conv-victim", "acc-victim",
		&oaiReq{Messages: victimHistory()}, "lambda mu nu",
		tenantTestRequest("victim-key", "203.0.113.10", "client-victim"))

	sess, ok := sr.GetSession("sess-victim")
	if !ok {
		t.Fatal("precondition: victim session was not stored")
	}
	if sess.TenantKey != "victim-key" {
		t.Fatalf("precondition: TenantKey = %q, want victim-key", sess.TenantKey)
	}
	return sr, len(sess.ContextHistory)
}

// The owning tenant naming its OWN session id must keep working exactly as
// before: the explicit header is the highest-priority continuation signal, and
// it returns the stored conversation, account and incremental boundary. This is
// the backward-compatibility half — a fix that rejects foreign prefixes must not
// also break legitimate explicit continuation.
func TestExplicitSessionHeaderAcceptedForOwningTenant(t *testing.T) {
	sr, historyLen := bindVictimSession(t)

	// A different IP/UA on purpose: explicit continuation is defined to survive a
	// network change, so only the tenant must match, not the fingerprint.
	r := tenantTestRequest("victim-key", "198.51.100.7", "client-victim-roaming")
	r.Header.Set(sessionHeaderName, "sess-victim")

	res := sr.Resolve(r, &oaiReq{Messages: probeMessages()})
	if res.IsNew {
		t.Fatal("owning tenant's explicit session id must resolve, got IsNew=true")
	}
	if res.MatchedBy != "explicit" {
		t.Fatalf("MatchedBy = %q, want explicit (the probe cannot match by content, so any other value means the explicit path did not run)", res.MatchedBy)
	}
	if res.SessionID != "sess-victim" {
		t.Fatalf("SessionID = %q, want sess-victim", res.SessionID)
	}
	if res.ConversationID != "conv-victim" {
		t.Fatalf("ConversationID = %q, want conv-victim", res.ConversationID)
	}
	if res.AccountID != "acc-victim" {
		t.Fatalf("AccountID = %q, want acc-victim", res.AccountID)
	}
	if res.HistoryLen != historyLen {
		t.Fatalf("HistoryLen = %d, want %d", res.HistoryLen, historyLen)
	}
}

// A foreign tenant naming someone else's session id must not inherit it. The
// explicit path has to enforce the same TenantKey constraint the derived paths
// enforce (session_resolver.go: strict-prefix filter and similarity filter), or
// a caller can name another tenant's session and inherit its upstream
// conversation binding, and with it that session's account and history.
func TestExplicitSessionHeaderRejectedForForeignTenant(t *testing.T) {
	sr, _ := bindVictimSession(t)

	// Same IP/UA as the victim, so the ONLY thing separating this caller from the
	// stored session is the tenant key.
	r := tenantTestRequest("attacker-key", "203.0.113.10", "client-victim")
	r.Header.Set(sessionHeaderName, "sess-victim")

	res := sr.Resolve(r, &oaiReq{Messages: probeMessages()})
	if !res.IsNew {
		t.Fatalf("foreign tenant inherited session %q via the explicit header (matched=%s conversation=%s account=%s)",
			"sess-victim", res.MatchedBy, res.ConversationID, res.AccountID)
	}
	// IsNew alone is not enough: nothing from the victim's binding may leak.
	if res.SessionID != "" || res.ConversationID != "" || res.AccountID != "" {
		t.Fatalf("rejected result still leaked victim binding: session=%q conversation=%q account=%q",
			res.SessionID, res.ConversationID, res.AccountID)
	}

	// The victim's session must be untouched and still usable by its owner.
	sess, ok := sr.GetSession("sess-victim")
	if !ok {
		t.Fatal("victim session disappeared after the foreign request")
	}
	if sess.TenantKey != "victim-key" || sess.ConversationID != "conv-victim" || sess.AccountID != "acc-victim" {
		t.Fatalf("victim session was mutated by the foreign request: %+v", sess)
	}
}

// The derived paths must enforce the same constraint, so the two halves cannot
// drift apart. A foreign tenant replaying a genuine strict-prefix continuation
// of the victim's history gets a new session, not the victim's.
func TestDerivedSessionMatchRejectedForForeignTenant(t *testing.T) {
	sr, _ := bindVictimSession(t)

	// A real continuation of the victim's stored history from the victim's own
	// fingerprint: this WOULD match by strict prefix if the tenant were the same.
	continuation := append(victimHistory(),
		oaiMsg{Role: "assistant", Content: "lambda mu nu"},
		oaiMsg{Role: "user", Content: "and then?"})

	own := sr.Resolve(tenantTestRequest("victim-key", "203.0.113.10", "client-victim"),
		&oaiReq{Messages: continuation})
	if own.IsNew || own.ConversationID != "conv-victim" {
		t.Fatalf("precondition: owning tenant's continuation must match by content, got %#v", own)
	}

	foreign := sr.Resolve(tenantTestRequest("attacker-key", "203.0.113.10", "client-victim"),
		&oaiReq{Messages: continuation})
	if !foreign.IsNew {
		t.Fatalf("foreign tenant matched the victim's session by content (matched=%s conversation=%s account=%s)",
			foreign.MatchedBy, foreign.ConversationID, foreign.AccountID)
	}
}

// Bind's explicit-header override only applies when the caller passes no session
// id of its own (session_resolver.go: `explicitID != "" && sessionID == ""`).
// Every production call site passes a non-empty session id precisely to stay out
// of that branch — server.go's tool-round bind generates a fresh UUID with an
// explanatory comment saying so. That guard is load-bearing: Bind does NOT
// tenant-check the explicit id before overwriting an existing binding, so if the
// guard were relaxed a client-supplied header could clobber another tenant's
// session. Pin it here so the invariant cannot be "simplified" away silently.
func TestBindIgnoresExplicitHeaderWhenCallerSuppliesSessionID(t *testing.T) {
	sr, _ := bindVictimSession(t)

	r := tenantTestRequest("attacker-key", "203.0.113.10", "client-attacker")
	r.Header.Set(sessionHeaderName, "sess-victim")

	// The caller supplies its own session id, exactly as every production call
	// site does. The explicit header must be ignored.
	sr.Bind("sess-attacker", "conv-attacker", "acc-attacker",
		&oaiReq{Messages: probeMessages()}, "reply", r)

	victim, ok := sr.GetSession("sess-victim")
	if !ok {
		t.Fatal("victim session disappeared after the attacker's Bind")
	}
	if victim.TenantKey != "victim-key" {
		t.Fatalf("victim session TenantKey was taken over: got %q want victim-key", victim.TenantKey)
	}
	if victim.ConversationID != "conv-victim" || victim.AccountID != "acc-victim" {
		t.Fatalf("victim binding was overwritten via the explicit header: conversation=%q account=%q",
			victim.ConversationID, victim.AccountID)
	}
	if _, ok := sr.GetSession("sess-attacker"); !ok {
		t.Fatal("attacker's own session id was not stored")
	}
}
