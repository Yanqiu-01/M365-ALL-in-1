package web

import (
	"context"
	"crypto/sha256"
	"strings"
	"unicode/utf8"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

const streamRecoveryTailBytes = 24 << 10

// streamRecoveryEventLedgerLimit bounds the state retained while a broken
// stream is retried. The retry policy allows only a handful of attempts, so
// this is deliberately generous without allowing an unbounded event map.
const streamRecoveryEventLedgerLimit = 2048

// streamRecoveryChatWithEvents is a narrow seam for recovery regression tests.
// Production delegates directly to the account-aware ChatHub event path.
var streamRecoveryChatWithEvents = func(ctx context.Context, s *Server, accountID string, account chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
	return s.chatWithAccountEvents(ctx, accountID, account, request, onEvent)
}

type streamRecoveryEventLedger struct {
	seen  map[string]struct{}
	order []string
}

func (l *streamRecoveryEventLedger) contains(event chathub.StreamEvent) bool {
	if l == nil || len(l.seen) == 0 {
		return false
	}
	_, ok := l.seen[streamRecoveryEventKey(event)]
	return ok
}

func (l *streamRecoveryEventLedger) record(event chathub.StreamEvent) {
	if l == nil {
		return
	}
	key := streamRecoveryEventKey(event)
	if l.seen == nil {
		l.seen = make(map[string]struct{})
	}
	if _, exists := l.seen[key]; exists {
		return
	}
	if len(l.order) >= streamRecoveryEventLedgerLimit {
		oldest := l.order[0]
		delete(l.seen, oldest)
		l.order = l.order[1:]
	}
	l.seen[key] = struct{}{}
	l.order = append(l.order, key)
}

// streamRecoveryEventKey deliberately excludes Raw. Raw is an enclosing
// SignalR frame and changes between attempts even when it contains the same
// semantic event. A stable identity lets recovery suppress replayed tool,
// progress, reasoning, and empty-text frames while forwarding newly produced
// events from the continuation attempt.
func streamRecoveryEventKey(event chathub.StreamEvent) string {
	material := event.Kind + "\x00" + event.Text + "\x00" + event.MessageType + "\x00" + event.ContentType + "\x00" + event.ToolName + "\x00" + string(event.Arguments)
	sum := sha256.Sum256([]byte(material))
	return string(sum[:])
}

const streamRecoveryPrefix = "The previous response stream was interrupted. The following suffix has already been delivered to the user:\n\n<delivered>\n"
const streamRecoverySuffix = "\n</delivered>\n\nContinue immediately after the final delivered character. Do not repeat, restart, summarize, or wrap the delivered text. Preserve the existing Markdown/code-fence structure and finish the original task."

// retryTextReconciler removes text that an upstream continuation repeats after
// a partially delivered response. Its structure and 2 KiB overlap window are
// recovered from the APK's stream_recovery.go implementation.
type retryTextReconciler struct {
	delivered   string
	buffer      strings.Builder
	passthrough bool
}

// push accepts one recovery fragment. If final is true, a short unmatched
// prefix is emitted instead of waiting for more data.
func (r *retryTextReconciler) push(fragment string, final bool) string {
	if r == nil {
		return fragment
	}
	if r.passthrough {
		return fragment
	}
	if fragment != "" {
		r.buffer.WriteString(fragment)
	}
	candidate := r.buffer.String()
	if candidate == "" {
		return ""
	}
	if r.delivered == "" {
		r.passthrough = true
		r.buffer.Reset()
		return candidate
	}

	// Some upstream retries replay the entire previously delivered text.
	if strings.HasPrefix(r.delivered, candidate) {
		if !final {
			return ""
		}
		return ""
	}
	if strings.HasPrefix(candidate, r.delivered) {
		r.passthrough = true
		r.buffer.Reset()
		return candidate[len(r.delivered):]
	}

	// Otherwise find the longest suffix of delivered that matches a prefix of
	// the recovery output. APK limits this comparison to 0x800 bytes and does
	// not trust tiny overlaps (less than 16 bytes).
	max := len(candidate)
	if max > len(r.delivered) {
		max = len(r.delivered)
	}
	if max > 0x800 {
		max = 0x800
	}
	for overlap := max; overlap >= 16; overlap-- {
		if !utf8Boundary(r.delivered, len(r.delivered)-overlap) || !utf8Boundary(candidate, overlap) {
			continue
		}
		if strings.HasSuffix(r.delivered, candidate[:overlap]) {
			r.passthrough = true
			r.buffer.Reset()
			return candidate[overlap:]
		}
	}

	// Wait for enough characters to make overlap matching meaningful unless the
	// continuation completed. The APK uses a 16-byte minimum before declaring
	// there is no usable overlap.
	if !final && len(candidate) < 16 {
		return ""
	}
	r.passthrough = true
	r.buffer.Reset()
	return candidate
}

func utf8Boundary(text string, index int) bool {
	return index >= 0 && index <= len(text) && (index == len(text) || utf8.RuneStart(text[index]))
}

func trimTrailingIncompleteRune(text string) string {
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

// streamRecoveryPrompt exactly follows the APK layout: trim whitespace, retain
// at most the last 24KiB while preserving UTF-8 boundaries, then surround that
// delivered suffix with a strict continuation instruction.
func streamRecoveryPrompt(delivered string) string {
	delivered = strings.TrimSpace(delivered)
	if len(delivered) > streamRecoveryTailBytes {
		delivered = delivered[len(delivered)-streamRecoveryTailBytes:]
		for len(delivered) > 0 && !utf8.RuneStart(delivered[0]) {
			delivered = delivered[1:]
		}
	}
	delivered = trimTrailingIncompleteRune(delivered)
	return streamRecoveryPrefix + delivered + streamRecoverySuffix
}

// streamChatWithRecovery retries only a broken pre-completion stream. It uses
// a fresh ChatHub conversation on the next healthy account, prompts it to
// continue from the delivered suffix, and reconciles text before forwarding it
// to the client so no previously emitted content is duplicated.
func (s *Server) streamChatWithRecovery(ctx context.Context, account auth.AccountToken, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, auth.AccountToken, error) {
	current := account
	original := request
	var delivered strings.Builder
	var result chathub.Result
	var reconciler *retryTextReconciler
	var deliveredEvents streamRecoveryEventLedger

	err := retryUpstream(ctx, "stream-recovery", func(attempt int) error {
		recovery := attempt > 1
		if recovery {
			next, err := s.nextHealthyAccount(current.ID)
			if err != nil {
				return err
			}
			current = next
			request = original
			request.ConversationID = ""
			request.SessionID = ""
			request.Started = true
			request.Text = original.Text + "\n\n" + streamRecoveryPrompt(delivered.String())
			reconciler = &retryTextReconciler{delivered: delivered.String()}
		}

		chatAccount := chathub.Account{AccessToken: current.AccessToken, OID: current.OID, TID: current.TID}
		res, err := streamRecoveryChatWithEvents(ctx, s, current.ID, chatAccount, request, func(event chathub.StreamEvent) error {
			if event.Kind != "text" || event.Text == "" {
				// A continuation can produce genuinely new tool/progress/reasoning
				// events. Suppress only identities that were already delivered on an
				// earlier attempt; dropping every non-text event loses valid work.
				if recovery && deliveredEvents.contains(event) {
					return nil
				}
				if onEvent != nil {
					if err := onEvent(event); err != nil {
						return err
					}
					deliveredEvents.record(event)
				}
				return nil
			}
			text := event.Text
			if recovery {
				text = reconciler.push(text, false)
				if text == "" {
					return nil
				}
			}
			delivered.WriteString(text)
			event.Text = text
			if onEvent != nil {
				return onEvent(event)
			}
			return nil
		})
		result = res
		if err != nil {
			return err
		}
		if recovery && reconciler != nil {
			if tail := reconciler.push("", true); tail != "" {
				delivered.WriteString(tail)
				if onEvent != nil {
					if err := onEvent(chathub.StreamEvent{Kind: "text", Text: tail}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	return result, current, err
}
