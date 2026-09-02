package web

import (
	"context"
	"errors"
	"testing"
)

func TestClassifyRouterFailureDegradesAutoChoice(t *testing.T) {
	if got := classifyRouterFailure(context.Background(), errors.New("upstream timeout"), "auto"); got != routerFailureAnswer {
		t.Fatalf("auto router failure = %v, want answer fallback", got)
	}
}

func TestClassifyRouterFailureKeepsRequiredAndRateLimitFatal(t *testing.T) {
	if got := classifyRouterFailure(context.Background(), errors.New("upstream timeout"), "required"); got != routerFailureFatal {
		t.Fatalf("required router failure = %v, want fatal", got)
	}
	if got := classifyRouterFailure(context.Background(), errors.New("429 too many requests"), "auto"); got != routerFailureFatal {
		t.Fatalf("rate-limited router failure = %v, want fatal", got)
	}
}

func TestClassifyRouterFailureAbandonsCanceledClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := classifyRouterFailure(ctx, errors.New("upstream timeout"), "auto"); got != routerFailureAbandon {
		t.Fatalf("canceled client router failure = %v, want abandon", got)
	}
}
