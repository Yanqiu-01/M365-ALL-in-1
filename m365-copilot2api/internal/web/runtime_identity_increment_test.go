package web

import (
	"strings"
	"testing"
)

// Session reuse is what made the injected identity look like it worked at first
// and then stopped. Bind stores the injected runtime system message at index 0,
// so on turn 2 the re-injected copy matches as a prefix and HistoryLen slices it
// off: ChatHub then sees only the new user turn, and the cloud identity
// reasserts itself mid-conversation. The increment has to carry the marker
// block again, or the fix only ever holds for one turn.
func TestAttachRuntimeIdentityToIncrementRestoresTheMarker(t *testing.T) {
	increment := "[user]\nnow read E:\\download\\notes.txt"
	got := attachRuntimeIdentityToIncrement(increment)

	if !strings.Contains(got, runtimeWorkspaceMarker) {
		t.Fatalf("increment lost the runtime marker:\n%s", got)
	}
	if !strings.HasSuffix(got, increment) {
		t.Errorf("the caller's increment must survive verbatim at the end:\n%s", got)
	}
	// GOOS-specific host text is asserted in runtime_prompt_test.go; here it is
	// enough that the re-attached block still states where the model is.
	if !strings.Contains(got, "Working directory:") {
		t.Errorf("the re-attached block does not say where execution happens:\n%s", got)
	}
}

// Only the gateway's own marker block goes back on. Re-sending the caller's
// whole harness every turn is exactly what the incremental prompt exists to
// avoid, so a prompt that already carries the marker must be left alone.
func TestAttachRuntimeIdentityToIncrementIsIdempotent(t *testing.T) {
	once := attachRuntimeIdentityToIncrement("[user]\nlist the directory")
	twice := attachRuntimeIdentityToIncrement(once)
	if twice != once {
		t.Error("a prompt that already carries the marker gained a second copy")
	}
	if n := strings.Count(twice, runtimeWorkspaceMarker); n != 1 {
		t.Errorf("marker appears %d times, want 1", n)
	}
}

// An empty increment means the resolver matched the entire message list. There
// is nothing to send, and prepending an identity block would turn that into a
// turn with a system prompt and no request.
func TestAttachRuntimeIdentityToIncrementLeavesEmptyPromptsAlone(t *testing.T) {
	for _, empty := range []string{"", "   \n\t "} {
		if got := attachRuntimeIdentityToIncrement(empty); got != "" {
			t.Errorf("attachRuntimeIdentityToIncrement(%q) = %q, want empty", empty, got)
		}
	}
}
