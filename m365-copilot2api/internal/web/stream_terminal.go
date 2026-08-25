package web

import (
	"time"
	"unicode/utf8"
)

// streamProgress records what the client actually received before an upstream
// stream ended. It is attached to an incomplete terminal chunk so clients can
// distinguish a clean stop from a response cut off after partial output.
type streamProgress struct {
	startedAt       time.Time
	textLen         int
	textEvents      int
	reasoningLen    int
	reasoningEvents int
}

func newStreamProgress(startedAt time.Time) *streamProgress {
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return &streamProgress{startedAt: startedAt}
}

func (p *streamProgress) addText(fragment string) {
	if p == nil || fragment == "" {
		return
	}
	p.textLen += utf8.RuneCountInString(fragment)
	p.textEvents++
}

func (p *streamProgress) addReasoning(fragment string) {
	if p == nil || fragment == "" {
		return
	}
	p.reasoningLen += utf8.RuneCountInString(fragment)
	p.reasoningEvents++
}

func (p *streamProgress) truncatedDiagnostics() map[string]any {
	if p == nil {
		p = newStreamProgress(time.Now())
	}
	elapsed := time.Since(p.startedAt).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	return map[string]any{
		"elapsed_ms":      elapsed,
		"textLen":         p.textLen,
		"textEvents":      p.textEvents,
		"reasoningLen":    p.reasoningLen,
		"reasoningEvents": p.reasoningEvents,
		"finished":        false,
		"truncated":       true,
	}
}

// streamTruncatedChunk follows the OpenAI-compatible contract used by the
// Python port: finish_reason=length is emitted before the explanatory error,
// and the m365 marker is present both on the choice and at the top level.
func streamTruncatedChunk(id, model string, progress *streamProgress) map[string]any {
	diagnostics := progress.truncatedDiagnostics()
	choice := map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": "length",
		"m365":          diagnostics,
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
		"m365":    diagnostics,
	}
}
