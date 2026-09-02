package web

import (
	"context"
	"errors"
	"net/http"
)

// routerFailureAction says what to do when the tool router's upstream call
// fails outright (as opposed to returning unparsable text, which
// parseModelToolDecision already handles).
type routerFailureAction int

const (
	// routerFailureAbandon: the client is gone, so there is nobody to answer.
	routerFailureAbandon routerFailureAction = iota
	// routerFailureFatal: the caller demanded a tool call, or retrying would
	// make things worse, so the request has to fail.
	routerFailureFatal
	// routerFailureAnswer: skip tool selection and answer in prose instead.
	routerFailureAnswer
)

// classifyRouterFailure decides how a router upstream error should be handled.
//
// The router is an internal optimization: it spends one upstream turn deciding
// which of the caller's tools to invoke. When that turn fails, the request
// itself is usually still answerable, because the answer prompt is far smaller
// than the router prompt (which carries every tool schema inline). Failing the
// whole request with a 502 throws away a turn the user can still be served.
//
// Measured on live traffic: five turns returned 502 after waiting between 100s
// and the full 600s chatResponseFallbackTimeout, all with tool_intent=true. The
// client got nothing for ten minutes of waiting.
//
// Two failures are not worth degrading for. If the client's context is already
// canceled, the answer attempt would fail the same way, so it is only wasted
// upstream load. If the upstream is rate limiting, an immediate second call
// deepens the limit rather than escaping it.
func classifyRouterFailure(ctx context.Context, err error, toolChoice any) routerFailureAction {
	if err == nil {
		return routerFailureAnswer
	}
	if clientGone(ctx, err) {
		return routerFailureAbandon
	}
	if toolChoiceRequiresToolCall(toolChoice) {
		return routerFailureFatal
	}
	if IsRateLimited(err) {
		return routerFailureFatal
	}
	return routerFailureAnswer
}

// clientGone reports that the caller hung up, so no response can be delivered.
// The gateway sees this as context.Canceled either on the request context or
// wrapped in the upstream error ("upstream request failed (router): context
// canceled" in the log).
func clientGone(ctx context.Context, err error) bool {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	return errors.Is(err, context.Canceled)
}

// writeRouterFatal emits the terminal router error, preserving the existing
// status and message shape so clients that already parse it keep working.
func writeRouterFatal(w http.ResponseWriter, stage string, err error) {
	msg := upstreamStageError(stage, err)
	if IsRateLimited(err) {
		msg = "upstream is rate limiting; try again shortly"
	}
	writeOpenAIError(w, http.StatusBadGateway, "router_error", msg)
}
