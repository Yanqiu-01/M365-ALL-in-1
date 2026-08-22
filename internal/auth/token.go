package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	authRequestTimeout   = 30 * time.Second
	maxAuthResponseBytes = int64(1 << 20)
)

type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresIn    int       `json:"expires_in,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Email        string    `json:"email,omitempty"`
	DisplayName  string    `json:"display_name,omitempty"`
	HomeOID      string    `json:"home_oid,omitempty"`
	TenantID     string    `json:"tenant_id,omitempty"`
}

type tokenResponse struct {
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	IDToken       string `json:"id_token"`
	TokenType     string `json:"token_type"`
	Scope         string `json:"scope"`
	ExpiresIn     int    `json:"expires_in"`
	Error         string `json:"error"`
	ErrorDesc     string `json:"error_description"`
	CorrelationID string `json:"correlation_id"`
	TraceID       string `json:"trace_id"`
}

type OAuthError struct {
	Code          string
	AADSTS        string
	HTTPStatus    int
	CorrelationID string
	TraceID       string
}

func (e *OAuthError) Error() string {
	parts := []string{e.Code}
	if e.AADSTS != "" {
		parts = append(parts, e.AADSTS)
	}
	if e.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.HTTPStatus))
	}
	return strings.Join(parts, ": ")
}

func (t TokenSet) Valid() bool {
	return t.AccessToken != "" && time.Now().Before(t.ExpiresAt.Add(-30*time.Second))
}

func ExchangeCode(code, verifier, redirect string) (TokenSet, error) {
	return ExchangeCodeContext(context.Background(), code, verifier, redirect)
}

func ExchangeCodeContext(ctx context.Context, code, verifier, redirect string) (TokenSet, error) {
	form := url.Values{}
	form.Set("client_id", ClientID())
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)
	form.Set("scope", Scope())
	return requestTokenContext(ctx, form)
}

func Refresh(refreshToken string) (TokenSet, error) {
	return RefreshContext(context.Background(), refreshToken)
}

func RefreshContext(ctx context.Context, refreshToken string) (TokenSet, error) {
	form := url.Values{}
	form.Set("client_id", ClientID())
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", Scope())
	return requestTokenContext(ctx, form)
}

// RefreshWithScope redeems the same account refresh token for a separately
// consented Microsoft resource, such as the Designer App Service used to
// download generated images. The caller must persist a rotated refresh token.
func RefreshWithScope(refreshToken, clientID, scope string) (TokenSet, error) {
	return RefreshWithScopeContext(context.Background(), refreshToken, clientID, scope)
}

func RefreshWithScopeContext(ctx context.Context, refreshToken, clientID, scope string) (TokenSet, error) {
	form := url.Values{}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = ClientID()
	}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", scope)
	return requestTokenContext(ctx, form)
}

func ROPC(username, password string) (TokenSet, error) {
	return ROPCContext(context.Background(), username, password)
}

func ROPCContext(ctx context.Context, username, password string) (TokenSet, error) {
	form := url.Values{}
	form.Set("client_id", ClientID())
	form.Set("grant_type", "password")
	form.Set("username", username)
	form.Set("password", password)
	form.Set("scope", Scope())
	return requestTokenTenantContext(ctx, form, "https://login.microsoftonline.com/organizations/oauth2/v2.0/token")
}

func requestTokenTenant(form url.Values, endpoint string) (TokenSet, error) {
	return requestTokenTenantContext(context.Background(), form, endpoint)
}

func requestTokenTenantContext(ctx context.Context, form url.Values, endpoint string) (TokenSet, error) {
	if err := ValidateTokenEndpoint(endpoint); err != nil {
		return TokenSet{}, err
	}
	resp, body, err := postAuthForm(ctx, endpoint, form)
	if err != nil {
		return TokenSet{}, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
return TokenSet{}, fmt.Errorf("decode token response (HTTP %d, body=%q): %w", resp.StatusCode, string(body), err)
	}
	if tr.Error != "" {
		return TokenSet{}, fmt.Errorf("ROPC %s: %s", tr.Error, tr.ErrorDesc)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return TokenSet{}, fmt.Errorf("ROPC HTTP %d", resp.StatusCode)
	}
	if tr.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("ROPC HTTP %d: empty access token", resp.StatusCode)
	}
	return tokenSetFromResponse(tr), nil
}

func requestToken(form url.Values) (TokenSet, error) {
	return requestTokenContext(context.Background(), form)
}

func requestTokenContext(ctx context.Context, form url.Values) (TokenSet, error) {
	endpoint := TokenEndpoint()
	if err := ValidateTokenEndpoint(endpoint); err != nil {
		return TokenSet{}, err
	}
	resp, body, err := postAuthForm(ctx, endpoint, form)
	if err != nil {
		return TokenSet{}, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return TokenSet{}, fmt.Errorf("decode token response: %w", err)
	}
	if tr.Error != "" {
		return TokenSet{}, &OAuthError{
			Code:          tr.Error,
			AADSTS:        aadstsCode(tr.ErrorDesc),
			HTTPStatus:    resp.StatusCode,
			CorrelationID: firstNonEmpty(tr.CorrelationID, resp.Header.Get("client-request-id")),
			TraceID:       firstNonEmpty(tr.TraceID, resp.Header.Get("x-ms-request-id")),
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return TokenSet{}, fmt.Errorf("token endpoint HTTP %d", resp.StatusCode)
	}
	if tr.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("token endpoint HTTP %d: empty access token", resp.StatusCode)
	}
	return tokenSetFromResponse(tr), nil
}

func postAuthForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := outbound.HTTPClient().Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, err
	}
	if resp == nil || resp.Body == nil {
		return nil, nil, fmt.Errorf("authentication endpoint returned no response body")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maxAuthResponseBytes {
		return nil, nil, fmt.Errorf("authentication response exceeds %d bytes", maxAuthResponseBytes)
	}
	return resp, body, nil
}

func tokenSetFromResponse(tr tokenResponse) TokenSet {
	set := TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		ExpiresIn:    tr.ExpiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	if claims, err := decodeJWTClaims(tr.AccessToken); err == nil {
		set.Email = firstNonEmpty(claims["unique_name"], claims["upn"], claims["preferred_username"], claims["email"])
		set.DisplayName = firstNonEmpty(claims["name"], set.Email)
		set.HomeOID = firstNonEmpty(claims["oid"], claims["sub"])
		set.TenantID = firstNonEmpty(claims["tid"], claims["tenant_id"])
	}
	if tr.IDToken != "" {
		if claims, err := decodeJWTClaims(tr.IDToken); err == nil {
			if set.Email == "" {
				set.Email = firstNonEmpty(claims["preferred_username"], claims["email"], claims["upn"])
				set.DisplayName = firstNonEmpty(claims["name"], set.Email)
				set.HomeOID = firstNonEmpty(claims["oid"], claims["sub"], set.HomeOID)
			}
			set.TenantID = firstNonEmpty(set.TenantID, claims["tid"], claims["tenant_id"])
		}
	}
	return set
}

func aadstsCode(description string) string {
	const prefix = "AADSTS"
	start := strings.Index(description, prefix)
	if start < 0 {
		return ""
	}
	end := start + len(prefix)
	for end < len(description) && description[end] >= '0' && description[end] <= '9' {
		end++
	}
	if end == start+len(prefix) {
		return ""
	}
	return description[start:end]
}

func decodeJWTClaims(token string) (map[string]string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid jwt")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range m {
		if value, ok := v.(string); ok {
			out[k] = value
		}
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
