package auth

import "os"

// Office web Copilot first-party client (verified working with ChatHub via browser PKCE).
// The default authority is multi-tenant so any supported Microsoft account can sign in.
// Device-code/FOCI client can still be forced via M365_CLIENT_ID.
const DefaultClientID = "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"
const FOCIClientID = "d3590ed6-52b3-4102-aeff-aad2292ab01c"
const DefaultAuthority = "https://login.microsoftonline.com/common"
const DefaultRedirectURI = "https://login.microsoftonline.com/common/oauth2/nativeclient"
const DefaultScope = "openid profile offline_access https://substrate.office.com/sydney/M365Chat.Read https://substrate.office.com/sydney/sydney.readwrite"

func ClientID() string {
	if v := os.Getenv("M365_CLIENT_ID"); v != "" {
		return v
	}
	return DefaultClientID
}

func Authority() string {
	if v := os.Getenv("M365_AUTHORITY"); v != "" {
		return v
	}
	return DefaultAuthority
}

func RedirectURI() string {
	if v := os.Getenv("M365_REDIRECT_URI"); v != "" {
		return v
	}
	return DefaultRedirectURI
}

func Scope() string {
	if v := os.Getenv("M365_SCOPE"); v != "" {
		return v
	}
	return DefaultScope
}

func AuthorizeEndpoint() string {
	if v := os.Getenv("M365_AUTHORIZE_ENDPOINT"); v != "" {
		return v
	}
	return Authority() + "/oauth2/v2.0/authorize"
}

func TokenEndpoint() string {
	if v := os.Getenv("M365_TOKEN_ENDPOINT"); v != "" {
		return v
	}
	return Authority() + "/oauth2/v2.0/token"
}

func DeviceCodeEndpoint() string {
	if v := os.Getenv("M365_DEVICE_ENDPOINT"); v != "" {
		return v
	}
	return Authority() + "/oauth2/v2.0/devicecode"
}

// DefaultPrompt forces a fresh Microsoft login. prompt=select_account still
// auto-continues a single signed-in browser session, so switching accounts
// would immediately hit the nativeclient callback with the previous identity.
const DefaultPrompt = "login"

// Prompt resolves the OAuth prompt parameter. An explicit M365_PROMPT still
// wins (including "none", which AuthorizationURLWithPrompt drops on purpose),
// so existing deployments keep their behaviour.
func Prompt() string {
	if v, ok := os.LookupEnv("M365_PROMPT"); ok {
		return v
	}
	return DefaultPrompt
}

// LogoutURL clears the Microsoft browser session before a new authorization so
// that switching accounts is possible. post_logout_redirect_uri is omitted
// because it must be registered on the app; Microsoft then shows its own
// "signed out" page, which the popup closes on its own.
func LogoutURL() string {
	if v := os.Getenv("M365_LOGOUT_ENDPOINT"); v != "" {
		return v
	}
	return Authority() + "/oauth2/v2.0/logout"
}
