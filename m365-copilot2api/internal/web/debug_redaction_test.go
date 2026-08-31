package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// camelCase 的敏感字段以前永远不会被打码：查表用的是 strings.ToLower(k)，
// 而表里存的是 "accessToken"/"refreshToken"/"clientSecret" 原样的 camelCase，
// 两边永不相等。上游的令牌响应正是 camelCase，于是明文进了 debug-logs.jsonl。
func TestRedactBodyRedactsCamelCaseSecretFields(t *testing.T) {
	body := []byte(`{
		"accessToken":"AT-SECRET-VALUE",
		"refreshToken":"RT-SECRET-VALUE",
		"clientSecret":"CS-SECRET-VALUE",
		"apiKey":"AK-SECRET-VALUE",
		"access_token":"AT-SNAKE-SECRET",
		"model":"gpt-5.6-sol"
	}`)

	out, err := json.Marshal(redactBody(body))
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(out)
	for _, secret := range []string{
		"AT-SECRET-VALUE", "RT-SECRET-VALUE", "CS-SECRET-VALUE",
		"AK-SECRET-VALUE", "AT-SNAKE-SECRET",
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("debug record leaks %s: %s", secret, rendered)
		}
	}
	// 非敏感字段必须原样保留，否则调试记录就没用了。
	if !strings.Contains(rendered, "gpt-5.6-sol") {
		t.Errorf("non-sensitive field was redacted: %s", rendered)
	}
}

// 打码判定与书写风格无关：驼峰、下划线、连字符、大小写、空格都归一化到同一个键。
func TestSensitiveKeyMatchingIsNormalized(t *testing.T) {
	for _, key := range []string{
		"accessToken", "access_token", "Access-Token", "ACCESS_TOKEN", "AccessToken",
		"refreshToken", "refresh_token", "RefreshToken",
		"clientSecret", "client_secret", "CLIENT-SECRET",
		"apiKey", "api_key", "APIKEY",
		"Authorization", "authorization",
	} {
		if !sensitiveKey(key) {
			t.Errorf("sensitiveKey(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"model", "messages", "tokens_used", "stream", ""} {
		if sensitiveKey(key) {
			t.Errorf("sensitiveKey(%q) = true, want false", key)
		}
	}
}

// 表里每一条都必须真的可被命中。以前 4 条 camelCase 是死条目，
// 其中 3 条没有对应的 snake_case 兜底，等于配置了却没生效。
func TestEverySensitiveKeyNameIsReachable(t *testing.T) {
	for _, name := range sensitiveKeyNames {
		if !sensitiveKey(name) {
			t.Errorf("sensitiveKeyNames entry %q can never match", name)
		}
	}
}

// 嵌套结构与数组里的 camelCase 字段同样要打码。
func TestRedactBodyRedactsNestedAndArrayedSecrets(t *testing.T) {
	body := []byte(`{"data":[{"credentials":{"refreshToken":"NESTED-SECRET"}}]}`)
	out, err := json.Marshal(redactBody(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "NESTED-SECRET") {
		t.Fatalf("nested camelCase secret leaked: %s", out)
	}
}
