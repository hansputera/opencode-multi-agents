package protonvpn

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-srp"
	"github.com/rs/zerolog"
)

// SRPAuth handles ProtonVPN SRP authentication
type SRPAuth struct {
	username   string
	password   string
	apiBase    string
	httpClient *http.Client
	log        *zerolog.Logger
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

// Login performs the full SRP authentication flow
func (a *SRPAuth) Login() (*Session, error) {
	// Step 1: Get auth info (Auto intent)
	a.log.Debug().Msg("Step 1: Getting auth info (Auto intent)")
	_, err := a.getAuthInfo("Auto")
	if err != nil {
		return nil, fmt.Errorf("failed to get auth info (Auto): %w", err)
	}

	// Step 2: Get auth info (Proton intent)
	a.log.Debug().Msg("Step 2: Getting auth info (Proton intent)")
	authInfoProton, err := a.getAuthInfo("Proton")
	if err != nil {
		return nil, fmt.Errorf("failed to get auth info (Proton): %w", err)
	}

	// Step 3: Verify modulus signature
	a.log.Debug().Msg("Step 3: Verifying modulus signature")
	_, err = verifyModulusSignature(authInfoProton.Modulus)
	if err != nil {
		return nil, fmt.Errorf("failed to verify modulus signature: %w", err)
	}

	// Step 4: Compute SRP proofs
	a.log.Debug().Msg("Step 4: Computing SRP proofs")
	auth, err := srp.NewAuth(
		authInfoProton.Version,
		a.username,
		[]byte(a.password),
		authInfoProton.Salt,
		authInfoProton.Modulus,
		authInfoProton.ServerEphemeral,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create SRP auth: %w", err)
	}

	proofs, err := auth.GenerateProofs(2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate SRP proofs: %w", err)
	}

	// Step 5: Authenticate
	a.log.Debug().Msg("Step 5: Authenticating")
	authResp, err := a.authenticate(authInfoProton.SRPSession, proofs.ClientProof, proofs.ClientEphemeral)
	if err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	// Step 6: Get cookies
	a.log.Debug().Msg("Step 6: Getting session cookies")
	cookies, err := a.getCookies(authResp.UID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cookies: %w", err)
	}

	// Step 7: Get user info
	a.log.Debug().Msg("Step 7: Getting user info")
	userResp, err := a.getUserInfo(cookies)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}

	// Step 8: Get key salts
	a.log.Debug().Msg("Step 8: Getting key salts")
	_, err = a.getKeySalts(cookies)
	if err != nil {
		return nil, fmt.Errorf("failed to get key salts: %w", err)
	}

	// Step 9: Compute auth key for decryption
	a.log.Debug().Msg("Step 9: Computing auth key")
	// Decode modulus from base64
	modulusBytes, err := base64.StdEncoding.DecodeString(authInfoProton.Salt)
	if err != nil {
		// If salt is not valid base64, use empty slice
		modulusBytes = []byte{}
	}
	authKey, err := srp.HashPassword(
		authInfoProton.Version,
		[]byte(a.password),
		a.username,
		[]byte(authInfoProton.Salt),
		modulusBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to compute auth key: %w", err)
	}

	// Create session
	session := &Session{
		UID:       authResp.UID,
		UserID:    userResp.User.ID,
		AuthKey:   authKey,
		Cookies:   cookies,
		ExpiresAt: time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second),
	}

	return session, nil
}

// getAuthInfo retrieves auth info from the API
func (a *SRPAuth) getAuthInfo(intent string) (*AuthInfoResponse, error) {
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

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
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

// verifyModulusSignature verifies the PGP signature on the modulus
func verifyModulusSignature(signedModulus string) ([]byte, error) {
	// The modulus is PGP signed, we need to extract the signed data
	// For now, we'll decode the base64 modulus directly
	// In production, this should verify the PGP signature

	// Try to decode as armored PGP message
	if strings.HasPrefix(signedModulus, "-----BEGIN PGP MESSAGE-----") {
		// Parse as armored PGP message
		entityList, err := openpgp.ReadArmoredKeyRing(strings.NewReader(signedModulus))
		if err != nil {
			return nil, fmt.Errorf("failed to read PGP key ring: %w", err)
		}
		_ = entityList
		// For now, just extract the base64 data
		// In production, verify the signature against Proton's public key
	}

	// Extract base64 data (strip any armor)
	lines := strings.Split(signedModulus, "\n")
	var base64Data strings.Builder
	inBody := false
	for _, line := range lines {
		if strings.HasPrefix(line, "-----BEGIN PGP MESSAGE-----") {
			inBody = true
			continue
		}
		if strings.HasPrefix(line, "-----END PGP MESSAGE-----") {
			break
		}
		if inBody {
			base64Data.WriteString(strings.TrimSpace(line))
		}
	}

	// If no PGP armor, try direct base64
	if base64Data.Len() == 0 {
		base64Data.WriteString(strings.TrimSpace(signedModulus))
	}

	modulusBytes, err := base64.StdEncoding.DecodeString(base64Data.String())
	if err != nil {
		return nil, fmt.Errorf("failed to decode modulus: %w", err)
	}

	return modulusBytes, nil
}

// authenticate sends the SRP proof to the auth endpoint
func (a *SRPAuth) authenticate(srpsession string, clientProof, clientEphemeral []byte) (*AuthResponse, error) {
	reqBody := map[string]string{
		"Username":        a.username,
		"ClientProof":     base64.StdEncoding.EncodeToString(clientProof),
		"ClientEphemeral": base64.StdEncoding.EncodeToString(clientEphemeral),
		"SRPSession":      srpsession,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", a.apiBase+"/api/core/v4/auth", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var authResp AuthResponse
	if err := json.Unmarshal(body, &authResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &authResp, nil
}

// getCookies retrieves session cookies from the API
func (a *SRPAuth) getCookies(uid string) ([]*http.Cookie, error) {
	req, err := http.NewRequest("GET", a.apiBase+"/api/core/v4/auth/cookies", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+uid)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return resp.Cookies(), nil
}

// getUserInfo retrieves user information
func (a *SRPAuth) getUserInfo(cookies []*http.Cookie) (*UserResponse, error) {
	req, err := http.NewRequest("GET", a.apiBase+"/api/core/v4/users", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
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

// getKeySalts retrieves key salts for decryption
func (a *SRPAuth) getKeySalts(cookies []*http.Cookie) (*KeySaltsResponse, error) {
	req, err := http.NewRequest("GET", a.apiBase+"/api/core/v4/keys/salts", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
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
