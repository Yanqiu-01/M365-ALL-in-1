package web

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// isRetryableUpstream classifies a failed upstream attempt.
//
// The parent context is the only authority on "stop trying": once it is Done
// the downstream client is gone or the whole request budget is spent, so
// nothing may be retried. Everything else is a per-attempt transport failure.
//
// This distinction is the fix for the router 502. An inner single-handshake
// timeout reaches this classifier wrapped as "ws dial: context deadline
// exceeded" (chathub/client.go wraps it with %w), so errors.Is(err,
// context.DeadlineExceeded) matched it and returned false. The "ws dial" marker
// below was therefore unreachable and one 40s handshake timeout became a hard
// 502 with zero failover.
func isRetryableUpstream(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "downstream stream write") || strings.Contains(message, "chathub completion error") {
		return false
	}
	for _, marker := range []string{
		"ws read before completion",
		"unexpected eof",
		"close 1006",
		"close 1011",
		"close 1012",
		"close 1013",
		"connection reset",
		"broken pipe",
		"ws dial",
		"handshake",
		"chat send",
		"eof",
		"i/o timeout",
		"connection refused",
		"software caused connection abort",
		"response deadline exceeded before completion",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// routerRetryAttempts returns total attempts, not additional retries. The APK
// uses M365_UPSTREAM_RETRIES: 0..5 becomes 1..6; invalid or unset defaults to 4.
func routerRetryAttempts() int {
	value := strings.TrimSpace(os.Getenv("M365_UPSTREAM_RETRIES"))
	if value != "" {
		if configured, err := strconv.Atoi(value); err == nil && configured >= 0 && configured <= 5 {
			return configured + 1
		}
	}
	return 4
}

// retryUpstream runs operation up to routerRetryAttempts times. The callback is
// given a 1-based attempt number so recovery callers can select a replacement
// account or a continuation prompt after the first failure.
func retryUpstream(ctx context.Context, stage string, operation func(attempt int) error) error {
	if operation == nil {
		return fmt.Errorf("%s: nil upstream operation", stage)
	}
	attempts := routerRetryAttempts()
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = operation(attempt)
		if last == nil || (!isRetryableUpstream(ctx, last) && !IsRateLimited(last) && !IsAuthFailure(last)) || attempt == attempts {
			return last
		}

		// APK uses an attempt-scaled 750ms delay and caps it before sleeping.
		delay := time.Duration(attempt) * 750 * time.Millisecond
		if delay > 12*time.Second {
			delay = 12 * time.Second
		}
		if err := sleepUnlessDone(ctx, delay); err != nil {
			return err
		}
	}
	return last
}

// retryTransportOnly retries a failed upstream attempt when, and only when, the
// failure is a transport-level one: an abnormal websocket close, a read that
// ended before the completion frame, a reset connection.
//
// It deliberately does NOT retry rate-limit or auth failures, which is the one
// thing that separates it from retryUpstream. Those two need a DIFFERENT account,
// not another attempt on the same one, and the callers of this helper already have
// an account-failover branch sitting right after them. Sending a rate-limited
// account through four more attempts here would just spend the backoff before
// reaching the failover that actually fixes it.
//
// The account and conversation binding are left untouched between attempts: a
// dropped socket says nothing about the account's health, and re-running the same
// turn on the same conversation is what makes the retry invisible to the caller.
func retryTransportOnly(ctx context.Context, stage string, operation func(attempt int) error) error {
	if operation == nil {
		return fmt.Errorf("%s: nil upstream operation", stage)
	}
	attempts := routerRetryAttempts()
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = operation(attempt)
		if last == nil || attempt == attempts {
			return last
		}
		if !isRetryableUpstream(ctx, last) || IsRateLimited(last) || IsAuthFailure(last) {
			return last
		}
		delay := time.Duration(attempt) * 750 * time.Millisecond
		if delay > 12*time.Second {
			delay = 12 * time.Second
		}
		if err := sleepUnlessDone(ctx, delay); err != nil {
			return err
		}
	}
	return last
}

// sleepUnlessDone performs cancellation-aware waiting. A 100ms ticker mirrors
// the APK helper's periodic context check while avoiding a goroutine leak.
func sleepUnlessDone(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
}
