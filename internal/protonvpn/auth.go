package protonvpn

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ProtonMail/go-srp"
	"github.com/rs/zerolog"
)

const maxHTTPBodySize = 10 << 20 // 10 MB

// SRPAuth handles ProtonVPN SRP authentication
type SRPAuth struct {
	username   string
	password   string
	apiBase    string
	httpClient *http.Client
	log        *zerolog.Logger
}

// commonHeaders sets the standard ProtonVPN web headers on a request
func commonHeaders(req *http.Request, uid string) {
	req.Header.Set("Accept", "application/vnd.protonmail.v1+json")
	req.Header.Set("x-pm-appversion", "web-vpn-settings@5.0.353.0")
	req.Header.Set("x-pm-locale", "en_US")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	if uid != "" {
		req.Header.Set("x-pm-uid", uid)
	}
}

// readBody reads an HTTP response body with a size limit to prevent OOM.
func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxHTTPBodySize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxHTTPBodySize {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxHTTPBodySize)
	}
	return body, nil
}

// NewSRPAuth creates a new SRP authenticator
func NewSRPAuth(username, password, apiBase string, log *zerolog.Logger) *SRPAuth {
	return &SRPAuth{
		username: username,
		password: password,
		apiBase:  apiBase,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		log: log,
	}
}

// mergeCookies merges new cookies into existing cookie list, replacing by name
func mergeCookies(existing []*http.Cookie, newCookies []*http.Cookie) []*http.Cookie {
	cookieMap := make(map[string]*http.Cookie)
	for _, c := range existing {
		cookieMap[c.Name] = c
	}
	for _, c := range newCookies {
		cookieMap[c.Name] = c
	}
	var merged []*http.Cookie
	for _, c := range cookieMap {
		merged = append(merged, c)
	}
	return merged
}

// Login performs the full SRP authentication flow
func (a *SRPAuth) Login() (*Session, error) {
	// Step 1: Get session token via /api/auth/v4/sessions
	a.log.Debug().Msg("Step 1: Getting session token")
	sessionResp, err := a.getSession()
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	// Step 2: Get initial cookies via /api/core/v4/auth/cookies
	a.log.Debug().Msg("Step 2: Getting initial cookies")
	cookies, err := a.getCookies(sessionResp.AccessToken, sessionResp.RefreshToken, sessionResp.UID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cookies: %w", err)
	}

	// Step 3: Get auth info for SRP (Intent: Auto)
	a.log.Debug().Msg("Step 3: Getting auth info")
	authInfo, err := a.getAuthInfo(sessionResp.UID, cookies, "Auto")
	if err != nil {
		return nil, fmt.Errorf("failed to get auth info: %w", err)
	}

	// Step 4: Compute SRP proofs
	a.log.Debug().Msg("Step 4: Computing SRP proofs")
	srpAuth, err := srp.NewAuth(
		authInfo.Version,
		a.username,
		[]byte(a.password),
		authInfo.Salt,
		authInfo.Modulus,
		authInfo.ServerEphemeral,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create SRP auth: %w", err)
	}

	proofs, err := srpAuth.GenerateProofs(2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate SRP proofs: %w", err)
	}

	// Step 5: Authenticate with SRP proof — this sets the real AUTH cookie
	a.log.Debug().Msg("Step 5: Authenticating with SRP proof")
	authResp, authCookies, err := a.authenticate(authInfo.SRPSession, proofs.ClientProof, proofs.ClientEphemeral, sessionResp.UID, cookies)
	if err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	// Merge: auth cookies replace the initial AUTH cookie
	cookies = mergeCookies(cookies, authCookies)

	// Step 6: Get user info (using merged cookies + uid header)
	a.log.Debug().Msg("Step 6: Getting user info")
	userResp, err := a.getUserInfo(authResp.UID, cookies)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}

	// Step 7: Get key salts
	a.log.Debug().Msg("Step 7: Getting key salts")
	_, err = a.getKeySalts(authResp.UID, cookies)
	if err != nil {
		return nil, fmt.Errorf("failed to get key salts: %w", err)
	}

	// Create session
	session := &Session{
		UID:       authResp.UID,
		UserID:    userResp.User.ID,
		Cookies:   cookies,
		ExpiresAt: time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second),
	}

	return session, nil
}

// getSession retrieves a session token via /api/auth/v4/sessions
func (a *SRPAuth) getSession() (*SessionResponse, error) {
	req, err := http.NewRequest("POST", a.apiBase+"/api/auth/v4/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	commonHeaders(req, "")
	req.Header.Set("x-enforce-unauthsession", "true")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var sessionResp SessionResponse
	if err := json.Unmarshal(body, &sessionResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &sessionResp, nil
}

// getAuthInfo retrieves SRP auth info from the API
func (a *SRPAuth) getAuthInfo(uid string, cookies []*http.Cookie, intent string) (*AuthInfoResponse, error) {
	reqBody := map[string]string{
		"Username": a.username,
		"Intent":   intent,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", a.apiBase+"/api/core/v4/auth/info", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	commonHeaders(req, uid)
	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var authInfo AuthInfoResponse
	if err := json.Unmarshal(body, &authInfo); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &authInfo, nil
}

// authenticate sends the SRP proof to the auth endpoint and returns the new AUTH cookies
func (a *SRPAuth) authenticate(srpsession string, clientProof, clientEphemeral []byte, uid string, cookies []*http.Cookie) (*AuthResponse, []*http.Cookie, error) {
	reqBody := map[string]string{
		"Username":        a.username,
		"ClientProof":     base64.StdEncoding.EncodeToString(clientProof),
		"ClientEphemeral": base64.StdEncoding.EncodeToString(clientEphemeral),
		"SRPSession":      srpsession,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", a.apiBase+"/api/core/v4/auth", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	commonHeaders(req, uid)
	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var authResp AuthResponse
	if err := json.Unmarshal(body, &authResp); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	// Return the NEW cookies set by this response (the real AUTH cookie)
	return &authResp, resp.Cookies(), nil
}

// getCookies retrieves session cookies via /api/core/v4/auth/cookies
func (a *SRPAuth) getCookies(accessToken, refreshToken, uid string) ([]*http.Cookie, error) {
	reqBody := map[string]string{
		"ResponseType": "token",
		"ClientID":     "WebVPNSettings",
		"GrantType":    "refresh_token",
		"RefreshToken": refreshToken,
		"UID":          uid,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", a.apiBase+"/api/core/v4/auth/cookies", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	commonHeaders(req, uid)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodySize))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return resp.Cookies(), nil
}

// getUserInfo retrieves user information
func (a *SRPAuth) getUserInfo(uid string, cookies []*http.Cookie) (*UserResponse, error) {
	req, err := http.NewRequest("GET", a.apiBase+"/api/core/v4/users", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	commonHeaders(req, uid)
	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var userResp UserResponse
	if err := json.Unmarshal(body, &userResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &userResp, nil
}

// getKeySalts retrieves key salts
func (a *SRPAuth) getKeySalts(uid string, cookies []*http.Cookie) (*KeySaltsResponse, error) {
	req, err := http.NewRequest("GET", a.apiBase+"/api/core/v4/keys/salts", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	commonHeaders(req, uid)
	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var keySalts KeySaltsResponse
	if err := json.Unmarshal(body, &keySalts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &keySalts, nil
}
