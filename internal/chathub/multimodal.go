package chathub

import "fmt"

const (
	MaxAttachments     = 10
	MaxAttachmentBytes = int64(10 << 20)
)

type Attachment struct {
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Detail   string `json:"detail,omitempty"`
	DocID    string `json:"-"`
	FileType string `json:"-"`
}

func ValidateAttachment(a Attachment) error {
	if a.Type != "image" {
		return nil
	}
	if err := validateImageSource(a.URL); err != nil {
		return fmt.Errorf("invalid image attachment: %w", err)
	}
	return nil
}

func ValidateAttachments(attachments []Attachment) error {
	if len(attachments) > MaxAttachments {
		return fmt.Errorf("too many attachments: limit is %d", MaxAttachments)
	}
	for i, a := range attachments {
		if err := ValidateAttachment(a); err != nil {
			return fmt.Errorf("attachment %d: %w", i+1, err)
		}
	}
	return nil
}
