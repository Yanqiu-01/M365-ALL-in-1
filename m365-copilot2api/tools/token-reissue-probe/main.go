package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type accountFile struct {
	Accounts []struct {
		Email        string `json:"email"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
	} `json:"accounts"`
}

func main() {
	raw, err := os.ReadFile(`C:\Users\ad\.config\m365-copilot2api\accounts.json`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read cache: %v\n", err)
		os.Exit(1)
	}
	var file accountFile
	if err := json.Unmarshal(raw, &file); err != nil {
		fmt.Fprintf(os.Stderr, "cache is not plaintext json: %v\n", err)
		os.Exit(2)
	}
	if len(file.Accounts) == 0 || strings.TrimSpace(file.Accounts[0].RefreshToken) == "" {
		fmt.Fprintln(os.Stderr, "no plaintext refresh token available")
		os.Exit(3)
	}
	acc := file.Accounts[0]
	form := url.Values{}
	form.Set("client_id", "c0ab8ce9-e9a0-42e7-b064-33d422df41f1")
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", acc.RefreshToken)
	form.Set("scope", "openid profile offline_access https://substrate.office.com/sydney/M365Chat.Read https://substrate.office.com/sydney/sydney.readwrite")
	req, err := http.NewRequest(http.MethodPost, "https://login.microsoftonline.com/common/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "request: %v\n", err)
		os.Exit(4)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "do: %v\n", err)
		os.Exit(5)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	preview := strings.ReplaceAll(string(body), "\n", " ")
	if len(preview) > 240 {
		preview = preview[:240]
	}
	fmt.Printf("email=%s stored_client=%s http=%d preview=%s\n", acc.Email, acc.ClientID, resp.StatusCode, preview)
}
