package web

// Account reassignment. The dashboard needs a way to move a conversation off the
// account it is pinned to, without deleting the conversation and without waiting
// for the sticky TTL to lapse.
//
// Two kinds of stickiness exist and both have to be released, otherwise the
// button looks like it did nothing:
//
//   - session -> account, held by sessionResolver (survives restarts via
//     sessions.json), which is what decides who answers the next turn;
//   - account -> proxy exit, held by outbound.Pool.sticky, which is what decides
//     which egress that turn leaves through.
//
// Releasing only the first keeps the new account glued to the same flapping exit,
// which is precisely the 502 case this endpoint is meant to escape.

import (
	"encoding/json"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"strings"
)

// reassignAccount handles POST /api/admin/accounts/reassign.
//
// Body accepts either addressing mode the UI has on hand:
//
//	{"conversationId":"..."}            reassign every session on that conversation
//	{"sessionId":"..."}                 reassign one session
//	{"accountId":"..."}                 optional target; empty means "let the
//	                                    router pick the next healthy account"
//	{"releaseExit":true}                also drop the account -> exit affinity
//	{"all":true}                        reassign every known session
//
// A reassignment intentionally clears the upstream ConversationID: a cloud
// conversation belongs to the account that created it, so replaying it under a
// different account is the CrossID mixing the resolver exists to prevent. Local
// history is kept, so the next turn continues from the same context.
func (s *Server) reassignAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body struct {
		ConversationID string `json:"conversationId"`
		SessionID      string `json:"sessionId"`
		AccountID      string `json:"accountId"`
		ReleaseExit    bool   `json:"releaseExit"`
		All            bool   `json:"all"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if s.sessionResolver == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "会话解析器不可用")
		return
	}

	// Resolve the target account first: failing here must not leave half the
	// sessions moved and half not.
	target := strings.TrimSpace(body.AccountID)
	if target != "" {
		if _, ok := s.tokens.Get(target); !ok {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "指定的账号不在账号池中")
			return
		}
	}

	sessionIDs := []string{}
	switch {
	case body.All:
		for _, sess := range s.sessionResolver.ListSessions() {
			sessionIDs = append(sessionIDs, sess.SessionID)
		}
	case strings.TrimSpace(body.SessionID) != "":
		sessionIDs = append(sessionIDs, strings.TrimSpace(body.SessionID))
	case strings.TrimSpace(body.ConversationID) != "":
		sessionIDs = s.sessionResolver.SessionsForConversation(strings.TrimSpace(body.ConversationID))
		if len(sessionIDs) == 0 {
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "该对话没有本地会话绑定，无法重新分配")
			return
		}
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "需要 conversationId、sessionId 或 all")
		return
	}

	previousAccounts := map[string]struct{}{}
	for _, sess := range s.sessionResolver.ListSessions() {
		for _, want := range sessionIDs {
			if sess.SessionID == want && sess.AccountID != "" {
				previousAccounts[sess.AccountID] = struct{}{}
			}
		}
	}

	reassigned := 0
	failed := []map[string]string{}
	assignments := []map[string]string{}
	for _, sid := range sessionIDs {
		chosen := target
		if chosen == "" {
			// Empty target means "anything but where it is now": ask the router
			// for the next healthy account so the button is useful even when the
			// operator does not know which account to move to.
			acc, err := s.nextHealthyAccount(currentAccountForSession(s, sid))
			if err != nil {
				failed = append(failed, map[string]string{"sessionId": sid, "error": err.Error()})
				continue
			}
			chosen = acc.ID
		}
		if !s.sessionResolver.ReassignAccount(sid, chosen) {
			failed = append(failed, map[string]string{"sessionId": sid, "error": "会话不存在或已过期"})
			continue
		}
		email := chosen
		if acc, ok := s.tokens.Get(chosen); ok && acc.Email != "" {
			email = acc.Email
		}
		assignments = append(assignments, map[string]string{"sessionId": sid, "accountId": chosen, "accountEmail": email})
		reassigned++
	}

	exitsReleased := 0
	if body.ReleaseExit && len(previousAccounts) > 0 {
		ids := make([]string, 0, len(previousAccounts))
		for id := range previousAccounts {
			ids = append(ids, id)
		}
		exitsReleased = outbound.ReleaseStickyExits(ids...)
	}

	jsonOut(w, map[string]any{
		"ok":            len(failed) == 0,
		"reassigned":    reassigned,
		"exitsReleased": exitsReleased,
		"assignments":   assignments,
		"failed":        failed,
	})
}

// currentAccountForSession reports which account a session is pinned to, so the
// auto-pick path can avoid handing back the very account we are moving away from.
func currentAccountForSession(s *Server, sessionID string) string {
	for _, sess := range s.sessionResolver.ListSessions() {
		if sess.SessionID == sessionID {
			return sess.AccountID
		}
	}
	return ""
}
