package web

import (
	"net/http/httptest"
	"testing"
)

// validAPIKey 曾经有一句 strings.HasPrefix(raw, "eyJ") 就直接放行。
//
// "eyJ" 只是 `{"` 的 base64 前缀，所以「Authorization: Bearer eyJ」这三个字符即可通过
// 认证 —— 不验签名、不验过期、不验签发者。实测活网关对 `Bearer eyJ` 返回 200，对
// `Bearer eyJhaha-not-a-real-token` 同样返回 200。任何第三方租户的合法 JWT，乃至任何
// 以 eyJ 开头的垃圾串，都是有效凭据。
//
// 形状检查（三段点分 base64url）也不够：一个网关既没签发也无法验签的 JWT 不能证明任何
// 事。唯一可核实的判据是与本地已授权账号的 access token 逐字匹配。

func bearerRequest(token string) *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func withBearer(t *testing.T, s *Server, token string) bool {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return s.validAPIKey(r)
}

func TestJWTShapedTokensAreNotCredentials(t *testing.T) {
	s := &Server{tokens: testAccountFiles(t)}

	// 每一条都曾经通过。
	for _, token := range []string{
		"eyJ",
		"eyJhaha-not-a-real-token",
		"eyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJhdHRhY2tlciJ9.signature",
		"eyJ" + "A",
	} {
		if withBearer(t, s, token) {
			t.Errorf("bearer %q was accepted; a JWT the gateway cannot verify is not a credential", token)
		}
	}
}

func TestNoCredentialIsRejected(t *testing.T) {
	s := &Server{tokens: testAccountFiles(t)}
	if withBearer(t, s, "") {
		t.Error("a request with no Authorization header must not authenticate")
	}
	if withBearer(t, s, "totally-invalid") {
		t.Error("an arbitrary bearer must not authenticate")
	}
}

// 正方向：网关自己持有的 access token 仍然可用，否则这个修复会打断合法用法。
func TestKnownAccountAccessTokenStillAuthenticates(t *testing.T) {
	store := testAccountFiles(t)
	accounts := store.List()
	if len(accounts) == 0 {
		t.Skip("fixture has no accounts")
	}
	var token string
	for _, a := range accounts {
		if a.AccessToken != "" {
			token = a.AccessToken
			break
		}
	}
	if token == "" {
		t.Skip("fixture accounts carry no access token")
	}
	s := &Server{tokens: store}
	if !withBearer(t, s, token) {
		t.Error("an access token the gateway itself holds must authenticate")
	}
	// 但差一个字符就不行。
	if withBearer(t, s, token+"x") {
		t.Error("a near-miss token must not authenticate")
	}
	if withBearer(t, s, token[:len(token)-1]) {
		t.Error("a truncated token must not authenticate")
	}
}

// 没有账号存储时不得因为「无法比对」而放行。
func TestNilTokenStoreDeniesRatherThanAllows(t *testing.T) {
	s := &Server{}
	if withBearer(t, s, "eyJ0eXAiOiJKV1QifQ.e30.sig") {
		t.Error("with no token store the gateway must deny, not fall open")
	}
}
