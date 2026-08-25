package outbound

import "context"

type accountAffinityKey struct{}

// WithAccountAffinity lets the shared proxy pool pin all transport attempts in
// one account-scoped request to the same healthy exit. The identifier is an
// internal account token ID and never leaves the process.
func WithAccountAffinity(ctx context.Context, accountID string) context.Context {
	if ctx == nil || accountID == "" {
		return ctx
	}
	return context.WithValue(ctx, accountAffinityKey{}, accountID)
}
