package web

import (
	"fmt"
	"strings"

	"m365-copilot2api/internal/chathub"
)

func parseContent(c any) (string, []chathub.Attachment) {
	var text strings.Builder
	var files []chathub.Attachment
	if s, ok := c.(string); ok {
		return s, nil
	}
	parts, ok := c.([]any)
	if !ok {
		return fmt.Sprint(c), nil
	}
	for _, raw := range parts {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		// Responses API uses input_text and may put image_url directly on
		// the content item rather than nesting it under image_url.
		if v, ok := m["text"].(string); ok && (typ == "text" || typ == "input_text" || typ == "output_text" || typ == "") {
			text.WriteString(v)
		}
		// 只有 switch 不认识的类型才走这条兜底：否则 {"type":"input_image",
		// "image_url":"data:..."} 会在这里和下面的 case 各追加一次，同一张图被
		// 上传两遍（日志里 1 张图显示 attachments=2，两次 UploadFile）。
		if direct, ok := m["image_url"].(string); ok && direct != "" && !imageTypeHandledInSwitch(typ) {
			files = append(files, chathub.Attachment{Type: "image", URL: direct, MimeType: "image/*"})
		}
		switch typ {
		case "text", "input_text", "output_text":
			// handled above
		case "image_url":
			if u, ok := m["image_url"].(map[string]any); ok {
				if v, ok := u["url"].(string); ok {
					a := chathub.Attachment{Type: "image", URL: v, MimeType: "image/*"}
					if d, ok := u["detail"].(string); ok {
						a.Detail = d
					}
					files = append(files, a)
				}
			} else if v, ok := m["image_url"].(string); ok && v != "" {
				files = append(files, chathub.Attachment{Type: "image", URL: v, MimeType: "image/*"})
			}
		case "input_image", "image":
			// Responses API accepts both image_url as a string and image_url
			// as an object containing url. Also accept nested source.url/data.
			u := stringValue(m, "image_url", "url", "source")
			if raw, ok := m["image_url"].(map[string]any); ok {
				u = stringValue(raw, "url", "data", "image_url")
			}
			if raw, ok := m["source"].(map[string]any); ok && u == "" {
				u = sourceToDataURL(raw)
			}
			if u != "" {
				files = append(files, chathub.Attachment{Type: "image", URL: u, MimeType: "image/*"})
			}
		case "input_file", "file":
			u := stringValue(m, "file_data", "file_url", "url")
			if raw, ok := m["source"].(map[string]any); ok && u == "" {
				u = sourceToDataURL(raw)
			}
			if u == "" {
				u = stringValue(m, "source", "file_id")
			}
			if u != "" || stringValue(m, "filename", "name") != "" {
				files = append(files, chathub.Attachment{Type: "file", URL: u, Name: stringValue(m, "filename", "name"), MimeType: stringValue(m, "mime_type", "mimeType", "content_type")})
			}
		case "input_audio", "audio":
			u := stringValue(m, "data", "audio_url", "url", "source")
			if u != "" {
				files = append(files, chathub.Attachment{Type: "audio", URL: u, MimeType: stringValue(m, "mime_type", "mimeType", "format", "content_type")})
			}
		}
	}
	return text.String(), files
}


// imageTypeHandledInSwitch 标出下面 switch 已经会消费 image_url 的类型，
// 避免前置兜底分支与 case 重复追加同一个附件。
func imageTypeHandledInSwitch(typ string) bool {
	switch typ {
	case "image_url", "input_image", "image":
		return true
	}
	return false
}

// sourceToDataURL 归一化 Anthropic 风格的 image/file source 块。
//
// Anthropic 的 base64 source 形如
// {"type":"base64","media_type":"image/png","data":"iVBORw0..."}，
// data 是**裸 base64**，不带 "data:" 前缀。此前这里直接把 data 当成 URL 交给
// 下游，chathub 便按远程地址处理并被 SSRF 校验拒绝，报
// "attachment download requires https" —— 整个请求 502。
//
// 这条路径在 Claude CLI 下必然触发：tool_result 内嵌的 image 块不经过顶层
// anthropicRequest.openAI() 转换，会原样带着 source 落到这里。
func sourceToDataURL(raw map[string]any) string {
	if u := stringValue(raw, "url"); u != "" {
		return u
	}
	data := stringValue(raw, "data")
	if data == "" {
		return ""
	}
	if strings.HasPrefix(data, "data:") {
		return data
	}
	media := stringValue(raw, "media_type", "mediaType", "mime_type", "mimeType", "content_type")
	if media == "" {
		media = "application/octet-stream"
	}
	return "data:" + media + ";base64," + data
}

func stringValue(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
