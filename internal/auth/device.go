package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type DeviceCode struct {
	UserCode        string    `json:"user_code"`
	DeviceCode      string    `json:"device_code"`
	VerificationURI string    `json:"verification_uri"`
	Message         string    `json:"message"`
	ExpiresIn       int       `json:"expires_in"`
	Interval        int       `json:"interval"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type deviceCodeResponse struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	Message         string `json:"message"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

func StartDeviceCode() (DeviceCode, error) {
	return StartDeviceCodeContext(context.Background())
}

func StartDeviceCodeContext(ctx context.Context) (DeviceCode, error) {
	endpoint := DeviceCodeEndpoint()
	if err := ValidateDeviceCodeEndpoint(endpoint); err != nil {
		return DeviceCode{}, err
	}
	form := url.Values{}
	form.Set("client_id", DeviceClientID())
	form.Set("scope", DeviceScope())
	resp, body, err := postAuthForm(ctx, endpoint, form)
	if err != nil {
		return DeviceCode{}, err
	}
	var dr deviceCodeResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return DeviceCode{}, err
	}
	if dr.Error != "" {
		return DeviceCode{}, fmt.Errorf("%s: %s", dr.Error, dr.ErrorDesc)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return DeviceCode{}, fmt.Errorf("device code endpoint HTTP %d", resp.StatusCode)
	}
	if dr.DeviceCode == "" || dr.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("invalid device code response")
	}
	interval := dr.Interval
	if interval <= 0 {
		interval = 5
	}
	return DeviceCode{
		UserCode:        dr.UserCode,
		DeviceCode:      dr.DeviceCode,
		VerificationURI: dr.VerificationURI,
		Message:         dr.Message,
		ExpiresIn:       dr.ExpiresIn,
		Interval:        interval,
		ExpiresAt:       time.Now().Add(time.Duration(dr.ExpiresIn) * time.Second),
	}, nil
}

func PollDeviceCode(deviceCode string) (TokenSet, bool, error) {
	return PollDeviceCodeContext(context.Background(), deviceCode)
}

func PollDeviceCodeContext(ctx context.Context, deviceCode string) (TokenSet, bool, error) {
	endpoint := DeviceTokenEndpoint()
	if err := ValidateTokenEndpoint(endpoint); err != nil {
		return TokenSet{}, false, err
	}
	form := url.Values{}
	form.Set("client_id", DeviceClientID())
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", deviceCode)
	resp, body, err := postAuthForm(ctx, endpoint, form)
	if err != nil {
		return TokenSet{}, false, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return TokenSet{}, false, err
	}
	switch tr.Error {
	case "":
		// success path falls through
	case "authorization_pending", "slow_down":
		return TokenSet{}, false, nil
	case "expired_token", "authorization_declined", "bad_verification_code":
		return TokenSet{}, false, fmt.Errorf("%s: %s", tr.Error, tr.ErrorDesc)
	default:
		if tr.AccessToken == "" {
			return TokenSet{}, false, fmt.Errorf("%s: %s", tr.Error, tr.ErrorDesc)
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return TokenSet{}, false, fmt.Errorf("device token endpoint HTTP %d", resp.StatusCode)
	}
	if tr.AccessToken == "" {
		return TokenSet{}, false, fmt.Errorf("token endpoint returned no access token")
	}
	return tokenSetFromResponse(tr), true, nil
}
