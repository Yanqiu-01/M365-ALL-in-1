package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImagePolicyRefusalRequiresExplicitReason(t *testing.T) {
	for _, text := range []string{
		"The image request did not pass the safety check.",
		"I cannot generate that image because it violates our content policy.",
		"I can’t help generate sexually explicit imagery.",
		"抱歉，图片请求未能通过安全检查。",
		"抱歉，我不能生成带有性暗示的图像。",
		"content_policy_violation",
	} {
		if !isImagePolicyRefusal(text) {
			t.Errorf("explicit policy refusal not detected: %q", text)
		}
	}
	for _, text := range []string{
		"", "Sorry, no image was returned.", "I cannot generate an image because the image service is unavailable.",
		"抱歉，图片服务暂时不可用。", "抱歉，我无法生成图片，请稍后重试。",
		"抱歉，安全检查服务暂时不可用，无法生成图片。",
		"Your image passed the safety check.",
		"I cannot generate an image right now.\n\nFor reference, the content policy applies to all requests.",
		"Sorry, try again tomorrow.",
	} {
		if isImagePolicyRefusal(text) {
			t.Errorf("ordinary response misclassified: %q", text)
		}
	}
}

func TestWriteImageRefusalSeparatesQuotaPolicyAndMissingImage(t *testing.T) {
	cases := []struct {
		name, text, raw  string
		status           int
		code, typ, retry string
	}{
		{"policy", "The image request did not pass the safety check.", "", http.StatusBadRequest, "content_policy_violation", "invalid_request_error", ""},
		{"quota", "Sorry, try again tomorrow.", "", http.StatusTooManyRequests, "", "rate_limit_error", "86400"},
		{"raw quota", "", `{"message":"image generation quota exhausted"}`, http.StatusTooManyRequests, "", "rate_limit_error", "86400"},
		{"missing image", "Sorry, the image service is unavailable.", "", 0, "", "", ""},
		{"raw prompt is not a refusal", "No image resource was returned.", `{"prompt":"content_policy_violation"}`, 0, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handled := writeImageRefusal(w, tc.text, tc.raw)
			if tc.status == 0 {
				if handled || w.Body.Len() != 0 || len(w.Header()) != 0 {
					t.Fatal("ordinary missing image was intercepted")
				}
				return
			}
			if !handled || w.Code != tc.status {
				t.Fatalf("handled=%t status=%d", handled, w.Code)
			}
			var body struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Type != tc.typ || body.Error.Code != tc.code {
				t.Fatalf("error=%+v", body.Error)
			}
			if got := w.Header().Get("Retry-After"); got != tc.retry {
				t.Fatalf("Retry-After=%q", got)
			}
		})
	}
}
