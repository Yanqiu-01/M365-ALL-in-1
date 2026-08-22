package auth

import (
	"fmt"
	"net/url"
	"strings"
)

const microsoftLoginHost = "login.microsoftonline.com"

// ValidateAuthorizationTarget ensures that browser PKCE can only launch the
// Microsoft authorization endpoint and use the native-client callback expected
// by the Android wrapper. Environment overrides are configuration, not trust
// boundaries, so they are validated immediately before state is created.
func ValidateAuthorizationTarget(authorizeEndpoint, redirectURI string) error {
	if err := validateMicrosoftOAuthEndpoint(authorizeEndpoint, "authorize"); err != nil {
		return fmt.Errorf("invalid authorization endpoint: %w", err)
	}
	return ValidateNativeClientRedirectURI(redirectURI)
}

// ValidateNativeClientRedirectURI accepts only the Microsoft native-client
// redirect URI used by this application. Keeping this separate lets callback
// handling revalidate state created by an older or malformed process.
func ValidateNativeClientRedirectURI(redirectURI string) error {
	u, err := parseOAuthHTTPSURL(redirectURI)
	if err != nil || !isMicrosoftLoginURL(u) || !hasOAuthPath(u, "oauth2", "nativeclient") {
		return fmt.Errorf("redirect URI must be the Microsoft nativeclient callback")
	}
	return nil
}

// ValidateDeviceCodeEndpoint prevents configuration from directing the
// device-code request to an arbitrary host that could return a phishing
// verification URL or capture the issued device code.
func ValidateDeviceCodeEndpoint(endpoint string) error {
	if err := validateMicrosoftOAuthEndpoint(endpoint, "devicecode"); err != nil {
		return fmt.Errorf("invalid device code endpoint: %w", err)
	}
	return nil
}

// ValidateTokenEndpoint prevents authorization codes, PKCE verifiers, device
// codes, and refresh tokens from being sent to an endpoint supplied by
// configuration.
func ValidateTokenEndpoint(endpoint string) error {
	if err := validateMicrosoftOAuthEndpoint(endpoint, "token"); err != nil {
		// ROPC requires /organizations/ prefix which the strict path check rejects.
		// Accept it as a known Microsoft OAuth variant.
		if err2 := validateMicrosoftROPCTokenEndpoint(endpoint); err2 != nil {
			return fmt.Errorf("invalid token endpoint: %w", err)
		}
	}
	return nil
}

func validateMicrosoftROPCTokenEndpoint(endpoint string) error {
	u, err := parseOAuthHTTPSURL(endpoint)
	if err != nil || !isMicrosoftLoginURL(u) {
		return fmt.Errorf("must be an HTTPS Microsoft OAuth endpoint")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	// Accept /organizations/oauth2/v2.0/token or /<tenant>/oauth2/v2.0/token
	if len(parts) == 4 {
		if parts[1] != "oauth2" || parts[2] != "v2.0" || parts[3] != "token" {
			return fmt.Errorf("must be a Microsoft OAuth token endpoint")
		}
	} else if len(parts) == 5 {
		if parts[1] != "organizations" || parts[2] != "oauth2" || parts[3] != "v2.0" || parts[4] != "token" {
			return fmt.Errorf("must be a Microsoft OAuth token endpoint")
		}
	} else {
		return fmt.Errorf("must be a Microsoft OAuth token endpoint")
	}
	return nil
}

func validateMicrosoftOAuthEndpoint(endpoint, operation string) error {
	u, err := parseOAuthHTTPSURL(endpoint)
	if err != nil || !isMicrosoftLoginURL(u) || !hasOAuthPath(u, "oauth2", "v2.0", operation) {
		return fmt.Errorf("must be an HTTPS Microsoft OAuth %s endpoint", operation)
	}
	return nil
}

func parseOAuthHTTPSURL(raw string) (*url.URL, error) {
	return parseOAuthHTTPSURLWithQuery(raw, false)
}

func parseOAuthHTTPSURLWithQuery(raw string, allowQuery bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil || u.Port() != "" || u.Fragment != "" || u.ForceQuery || u.RawPath != "" {
		return nil, fmt.Errorf("expected an HTTPS URL without userinfo, port, fragment, or escaped path")
	}
	if !allowQuery && u.RawQuery != "" {
		return nil, fmt.Errorf("expected a plain HTTPS URL without a query")
	}
	return u, nil
}

// NativeClientCallback is the minimal OAuth result accepted from a pasted
// browser callback. It deliberately carries no token material.
type NativeClientCallback struct {
	State string
	Code  string
	Error string
}

// ParseNativeClientCallbackURL rejects pasted URLs unless they are an exact
// Microsoft native-client callback. This prevents a local UI or wrapper from
// turning an arbitrary external URL into an authorization result.
func ParseNativeClientCallbackURL(raw string) (NativeClientCallback, error) {
	u, err := parseOAuthHTTPSURLWithQuery(raw, true)
	if err != nil {
		return NativeClientCallback{}, err
	}
	if !isMicrosoftLoginURL(u) || !hasOAuthPath(u, "oauth2", "nativeclient") {
		return NativeClientCallback{}, fmt.Errorf("callback URL must be the Microsoft nativeclient callback")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return NativeClientCallback{}, fmt.Errorf("invalid callback query: %w", err)
	}
	return ParseOAuthCallbackParameters(query)
}

// ParseOAuthCallbackParameters accepts exactly one state and exactly one OAuth
// result. It is shared by direct loopback delivery and pasted native callbacks
// so duplicate parameters cannot select different values in different flows.
func ParseOAuthCallbackParameters(query url.Values) (NativeClientCallback, error) {
	state, hasState, err := callbackQueryValue(query, "state")
	if err != nil {
		return NativeClientCallback{}, err
	}
	code, hasCode, err := callbackQueryValue(query, "code")
	if err != nil {
		return NativeClientCallback{}, err
	}
	oauthError, hasError, err := callbackQueryValue(query, "error")
	if err != nil {
		return NativeClientCallback{}, err
	}
	if !hasState || (!hasCode && !hasError) || (hasCode && hasError) {
		return NativeClientCallback{}, fmt.Errorf("callback must contain one authorization result and state")
	}
	return NativeClientCallback{State: state, Code: code, Error: oauthError}, nil
}

func callbackQueryValue(query url.Values, name string) (string, bool, error) {
	values, present := query[name]
	if !present {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", false, fmt.Errorf("callback contains duplicate %s", name)
	}
	if values[0] == "" {
		return "", false, fmt.Errorf("callback contains empty %s", name)
	}
	return values[0], true, nil
}

func isMicrosoftLoginURL(u *url.URL) bool {
	return u != nil && strings.EqualFold(u.Hostname(), microsoftLoginHost)
}

func hasOAuthPath(u *url.URL, suffix ...string) bool {
	if u == nil || !strings.HasPrefix(u.Path, "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != len(suffix)+1 || parts[0] == "" || parts[0] == "." || parts[0] == ".." {
		return false
	}
	for i, part := range suffix {
		if parts[i+1] != part {
			return false
		}
	}
	return true
}
