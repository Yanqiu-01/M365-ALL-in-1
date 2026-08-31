package chathub

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

func imageURLs(raw []json.RawMessage) []string {
	seen := map[string]bool{}
	out := []string{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				lk := strings.ToLower(k)
				if s, ok := e.(string); ok && (lk == "url" || lk == "imageurl" || lk == "thumbnailurl" || lk == "downloadurl" || lk == "src" || lk == "value" || lk == "data") {
					if isImageURL(s) && !seen[s] {
						seen[s] = true
						out = append(out, s)
					}
					continue
				}
				// ImageReferenceUrls and similar fields hold image URLs as a
				// flat string array keyed by a *Urls name.
				if arr, ok := e.([]any); ok && strings.Contains(lk, "url") {
					for _, item := range arr {
						if s, ok := item.(string); ok && isImageURL(s) && !seen[s] {
							seen[s] = true
							out = append(out, s)
						}
					}
					continue
				}
				walk(e)
			}
		}
	}
	for _, r := range raw {
		var v any
		if json.Unmarshal(r, &v) == nil {
			walk(v)
		}
	}
	return out
}

func isImageURL(s string) bool {
	if strings.HasPrefix(s, "data:image/") {
		// SplitN 只在存在分隔符时才返回两段。少了这个检查，一个没有逗号的
		// "data:image/png;base64" 会让索引 [1] 越界 panic —— 而这段代码跑在
		// imageURLs 里，扫的是上游 SignalR 帧中任何 url/src/value/data 字段，
		// 输入完全由上游决定。
		parts := strings.SplitN(s, ",", 2)
		if len(parts) < 2 || parts[1] == "" {
			return false
		}
		_, err := base64.StdEncoding.DecodeString(parts[1])
		return err == nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" {
		return false
	}
	// Designer-hosted generation URLs carry the extension in the query
	// (?path=.../dalle-xxx.png&dcHint=...); check path and query for markers.
	p := strings.ToLower(u.Path + "?" + u.RawQuery)
	if strings.Contains(p, "image") {
		return true
	}
	re := regexp.MustCompile(`\.(png|jpe?g|webp|gif)(&|$)`)
	return re.MatchString(p)
}
