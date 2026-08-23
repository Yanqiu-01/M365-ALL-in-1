package web

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRouterRetryAttemptsAPKConfig(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{
		{"", 4}, {"bad", 4}, {"-1", 4}, {"6", 4}, {"0", 1}, {"1", 2}, {"5", 6},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("M365_UPSTREAM_RETRIES", test.value)
			if got := routerRetryAttempts(); got != test.want {
				t.Fatalf("routerRetryAttempts()=%d want %d", got, test.want)
			}
		})
	}
}

func TestIsRetryableUpstreamAPKClassifier(t *testing.T) {
	live := context.Background()
	for _, err := range []error{
		fmt.Errorf("ws read before completion: unexpected EOF"),
		fmt.Errorf("websocket: close 1006"),
		fmt.Errorf("connection reset by peer"),
		fmt.Errorf("write: broken pipe"),
		fmt.Errorf("i/o timeout"),
	} {
		if !isRetryableUpstream(live, err) {
			t.Fatalf("expected retryable: %v", err)
		}
	}
	for _, err := range []error{
		nil,
		fmt.Errorf("downstream stream write: client disconnected"),
		fmt.Errorf("chathub completion error: rejected"),
		fmt.Errorf("invalid tool arguments"),
	} {
		if isRetryableUpstream(live, err) {
			t.Fatalf("unexpected retryable: %v", err)
		}
	}
}

// A single inner handshake timeout arrives wrapped by chathub as
// "ws dial: context deadline exceeded". While the parent context is still live
// this is a per-attempt transport failure and must fail over to another
// account/exit. The previous errors.Is(err, context.DeadlineExceeded) early
// return classified it as permanent, so one dial timeout became a hard 502 with
// zero retries.
func TestIsRetryableUpstreamWrappedDialTimeoutWithLiveParentContext(t *testing.T) {
	err := fmt.Errorf("ws dial: %w", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("test premise: the wrapped error must still unwrap to context.DeadlineExceeded")
	}
	if !isRetryableUpstream(context.Background(), err) {
		t.Fatalf("wrapped dial timeout must be retryable while the parent context is live: %v", err)
	}
	for _, inner := range []error{
		fmt.Errorf("upstream handshake failed: %w", context.DeadlineExceeded),
		fmt.Errorf("chat send: %w", context.DeadlineExceeded),
		fmt.Errorf("chathub response deadline exceeded before completion"),
	} {
		if !isRetryableUpstream(context.Background(), inner) {
			t.Fatalf("inner transport timeout must be retryable: %v", inner)
		}
	}
}

// The parent context is the only authority on "stop trying": a cancelled client
// or an exhausted overall request budget must never be retried, whatever the
// error text says.
func TestIsRetryableUpstreamStopsWhenParentContextIsDone(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "expired": expired} {
		for _, err := range []error{
			fmt.Errorf("ws dial: %w", context.DeadlineExceeded),
			fmt.Errorf("ws read before completion: unexpected EOF"),
			fmt.Errorf("i/o timeout"),
		} {
			if isRetryableUpstream(ctx, err) {
				t.Fatalf("%s parent context must stop retries: %v", name, err)
			}
		}
	}
}

func TestRetryUpstreamRetriesTransientFailures(t *testing.T) {
	t.Setenv("M365_UPSTREAM_RETRIES", "2") // three total attempts
	attempts := 0
	err := retryUpstream(context.Background(), "test", func(attempt int) error {
		attempts++
		if attempt < 3 {
			return errors.New("ws read before completion: unexpected EOF")
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestRetryUpstreamStopsForPermanentFailure(t *testing.T) {
	t.Setenv("M365_UPSTREAM_RETRIES", "5")
	attempts := 0
	want := errors.New("invalid request")
	err := retryUpstream(context.Background(), "test", func(int) error {
		attempts++
		return want
	})
	if !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestSleepUnlessDoneHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := sleepUnlessDone(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled sleep took %s", elapsed)
	}
}
