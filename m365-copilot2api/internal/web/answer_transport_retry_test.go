package web

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// The exact error a dropped ChatHub socket produces, as it appeared in the live
// log for the 502s this retry exists to prevent:
//
//	upstream request failed: ws read before completion: websocket:
//	close 1006 (abnormal closure): unexpected EOF
var liveSocketDrop = fmt.Errorf("ws read before completion: %w",
	errors.New("websocket: close 1006 (abnormal closure): unexpected EOF"))

// A transport drop must be retried. The streaming answer path already retried it
// through streamChatWithRecovery; the non-streaming path did not, so one abnormal
// closure became a hard 502 with zero failover.
func TestRetryTransportOnlyRetriesASocketDrop(t *testing.T) {
	attempts := 0
	err := retryTransportOnly(context.Background(), "answer", func(attempt int) error {
		attempts++
		if attempt < 3 {
			return liveSocketDrop
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a drop that later succeeded still returned an error: %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected the drop to be retried until it succeeded, got attempts=%d", attempts)
	}
}

// The one behaviour that separates this helper from retryUpstream. Rate-limit and
// auth failures need a DIFFERENT account, and the caller has an account-failover
// branch immediately after. Burning the backoff on the same account first would
// only delay reaching the thing that actually fixes it.
//
// The DialError rows are the ones that matter most: DialError.Error() renders as
// "ws dial: upstream 429", and "ws dial" is itself a retryable marker in
// isRetryableUpstream. So these errors match BOTH classifiers at once, and only
// the order of the checks inside retryTransportOnly keeps them from being retried
// on an account that cannot serve them.
func TestRetryTransportOnlyDoesNotRetryRateLimitOrAuth(t *testing.T) {
	for _, tc := range []struct {
		label string
		err   error
	}{
		{"http 429", &UpstreamHTTPError{Status: 429}},
		{"http 503", &UpstreamHTTPError{Status: 503}},
		{"http 401", &UpstreamHTTPError{Status: 401}},
		{"http 403", &UpstreamHTTPError{Status: 403}},
		{"ws dial 429 (also matches the retryable marker)", &chathub.DialError{Status: 429}},
		{"ws dial 401 (also matches the retryable marker)", &chathub.DialError{Status: 401}},
		{"wrapped ws dial 429", fmt.Errorf("upstream: %w", &chathub.DialError{Status: 429})},
	} {
		t.Run(tc.label, func(t *testing.T) {
			attempts := 0
			err := retryTransportOnly(context.Background(), "answer", func(int) error {
				attempts++
				return tc.err
			})
			if err == nil {
				t.Fatal("expected the error to be returned, not swallowed")
			}
			if attempts != 1 {
				t.Errorf("%s must fall through to the account failover on the first attempt, got attempts=%d", tc.label, attempts)
			}
		})
	}
}

// A failure that is not a transport failure at all must not be retried either.
func TestRetryTransportOnlyDoesNotRetryANonTransportError(t *testing.T) {
	attempts := 0
	err := retryTransportOnly(context.Background(), "answer", func(int) error {
		attempts++
		return errors.New("chathub completion error: content filtered")
	})
	if err == nil {
		t.Fatal("expected the error to be returned")
	}
	if attempts != 1 {
		t.Errorf("a non-transport error must not be retried, got attempts=%d", attempts)
	}
}

// A cancelled parent context is the one authority on "stop trying".
func TestRetryTransportOnlyHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	if err := retryTransportOnly(ctx, "answer", func(int) error {
		attempts++
		return liveSocketDrop
	}); err == nil {
		t.Fatal("expected the cancelled context to be reported")
	}
	if attempts != 0 {
		t.Errorf("a cancelled context must not run the operation at all, got attempts=%d", attempts)
	}
}

func TestRetryTransportOnlyRejectsANilOperation(t *testing.T) {
	if err := retryTransportOnly(context.Background(), "answer", nil); err == nil {
		t.Fatal("a nil operation must be reported, not silently treated as success")
	}
}
