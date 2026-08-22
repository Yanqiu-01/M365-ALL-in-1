package web

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "errors"
    "net/http"
    "strconv"
    "strings"
)

type requestOwnerContextKey struct{}
type requestAdminContextKey struct{}

// privilegedOwnerNamespace is installed only after validAdminSession succeeds.
// It is an authorization principal, not a client-controlled tenant value.
const privilegedOwnerNamespace = "admin:privileged"

func withRequestOwner(r *http.Request, owner string, admin bool) *http.Request {
    ctx := context.WithValue(r.Context(), requestOwnerContextKey{}, owner)
    ctx = context.WithValue(ctx, requestAdminContextKey{}, admin)
    return r.WithContext(ctx)
}

func requestOwner(r *http.Request) string {
    if r == nil {
        return ""
    }
    owner, _ := r.Context().Value(requestOwnerContextKey{}).(string)
    return owner
}

func requestIsAdmin(r *http.Request) bool {
    if r == nil {
        return false
    }
    admin, _ := r.Context().Value(requestAdminContextKey{}).(bool)
    return admin && requestOwner(r) == privilegedOwnerNamespace
}

func requireRequestOwner(r *http.Request) (string, error) {
    owner := requestOwner(r)
    if owner == "" {
        return "", errors.New("authenticated owner required")
    }
    return owner, nil
}

func requireAdminRequest(r *http.Request) error {
    if !requestIsAdmin(r) {
        return errors.New("administrator authorization required")
    }
    return nil
}

// stateOwner reads only the owner explicitly supplied by an authenticated
// request path. No state API is allowed to invent a fallback/shared owner.
func stateOwner(values ...string) string {
    if len(values) == 0 {
        return ""
    }
    return strings.TrimSpace(values[0])
}

// scopedStateKey is a length-prefixed composite key. It cannot collide when
// owners, identifiers, or model names contain separators.
func scopedStateKey(owner, identifier string) string {
    return strconv.Itoa(len(owner)) + ":" + owner + ":" + strconv.Itoa(len(identifier)) + ":" + identifier
}

func credentialParts(r *http.Request) (kind, raw string) {
    if r == nil {
        return "", ""
    }
    raw = strings.TrimSpace(r.Header.Get("X-API-Key"))
    if raw != "" {
        return "api-key", raw
    }
    auth := strings.TrimSpace(r.Header.Get("Authorization"))
    if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
        raw = strings.TrimSpace(auth[7:])
        if raw != "" {
            return "bearer", raw
        }
    }
    return "", ""
}

func stableOwnerNamespace(kind, raw string) string {
    if kind == "" || raw == "" {
        return ""
    }
    digest := sha256.Sum256([]byte(kind + "\x00" + raw))
    return kind + ":" + hex.EncodeToString(digest[:])
}

func (s *Server) authenticatedOwner(r *http.Request) (string, bool) {
    kind, raw := credentialParts(r)
    if kind == "" || raw == "" {
        return "", false
    }
    // A registered API key is the same authenticated principal regardless of
    // whether a client presents it via X-API-Key or Bearer.
    if s != nil && s.apiKeys != nil && s.apiKeys.valid(raw) {
        return stableOwnerNamespace("api-key", raw), true
    }
    // Preserve the existing JWT-shaped Bearer compatibility path, but scope
    // it by the complete credential rather than a client-supplied identity.
    if strings.HasPrefix(raw, "eyJ") {
        return stableOwnerNamespace("bearer", raw), true
    }
    return "", false
}

func credentialDisplayID(r *http.Request) string {
    kind, raw := credentialParts(r)
    if kind == "" || raw == "" {
        return ""
    }
    if s := stableOwnerNamespace(kind, raw); s != "" && len(s) > 20 {
        return s[:20]
    }
    return stableOwnerNamespace(kind, raw)
}
