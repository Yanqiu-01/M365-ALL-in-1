package web

import (
	"fmt"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestParseContentCarriesUnsafeImageToOutboundValidator(t *testing.T) {
	_, files := parseContent([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://127.0.0.1/a.png"}}})
	if err := chathub.ValidateAttachments(files); err == nil {
		t.Fatal("loopback image URL was accepted by the outbound validator")
	}
}

func TestFlattenPromptMessagesCarriesAttachmentLimitSentinel(t *testing.T) {
	parts := make([]any, 0, chathub.MaxAttachments+1)
	for i := 0; i < chathub.MaxAttachments+1; i++ {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": fmt.Sprintf("https://example.com/%d.png", i)}})
	}
	_, files := flattenPromptMessages([]oaiMsg{{Role: "user", Content: parts}}, nil)
	err := chathub.ValidateAttachments(files)
	if err == nil || !strings.Contains(err.Error(), "too many attachments") {
		t.Fatalf("err=%v attachments=%d", err, len(files))
	}
}
