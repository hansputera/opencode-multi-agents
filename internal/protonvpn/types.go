package protonvpn

import (
	"net/http"
	"time"
)

// AuthInfoResponse represents the response from /api/core/v4/auth/info
type AuthInfoResponse struct {
	Code            int    `json:"Code"`
	Modulus         string `json:"Modulus"`
	ServerEphemeral string `json:"ServerEphemeral"`
	Version         int    `json:"Version"`
	Salt            string `json:"Salt"`
	SRPSession      string `json:"SRPSession"`
}

// AuthResponse represents the response from /api/core/v4/auth
type AuthResponse struct {
	Code      int      `json:"Code"`
	ExpiresIn int      `json:"ExpiresIn"`
	UID       string   `json:"UID"`
	UserID    string   `json:"UserID"`
	Scopes    []string `json:"Scopes"`
}

// UserResponse represents the response from /api/core/v4/users
type UserResponse struct {
	Code int  `json:"Code"`
	User User `json:"User"`
}

// User represents a Proton user
type User struct {
	ID    string `json:"ID"`
	Email string `json:"Email"`
	Keys  []Key  `json:"Keys"`
}

// Key represents a PGP key
type Key struct {
	ID         string `json:"ID"`
	Primary    int    `json:"Primary"`
	PrivateKey string `json:"PrivateKey"`
}

// KeySaltsResponse represents the response from /api/core/v4/keys/salts
type KeySaltsResponse struct {
	Code     int       `json:"Code"`
	KeySalts []KeySalt `json:"KeySalts"`
}

// KeySalt represents a key salt for decryption
type KeySalt struct {
	ID      string `json:"ID"`
	KeySalt string `json:"KeySalt"`
}

// CertificateResponse represents the response from /api/vpn/v1/certificate
type CertificateResponse struct {
	Code                int       `json:"Code"`
	SerialNumber        string    `json:"SerialNumber"`
	ClientKey           string    `json:"ClientKey"`
	Certificate         string    `json:"Certificate"`
	ExpirationTime      int64     `json:"ExpirationTime"`
	RefreshTime         int64     `json:"RefreshTime"`
	ServerPublicKey     string    `json:"ServerPublicKey"`
	ServerPublicKeyMode string    `json:"ServerPublicKeyMode"`
	IPv4                string    `json:"IPv4"`
	IPv6                string    `json:"IPv6"`
	DNS                 []string  `json:"DNS"`
	Features            Features  `json:"Features"`
}

// ServerListResponse represents the response from /api/vpn/v2/logicals
type ServerListResponse struct {
	LogicalServers []LogicalServer `json:"LogicalServers"`
}

// LogicalServer represents a logical VPN server
type LogicalServer struct {
	Name         string   `json:"Name"`
	EntryCountry string   `json:"EntryCountry"`
	ExitCountry  string   `json:"ExitCountry"`
	Domain       string   `json:"Domain"`
	Tier         int      `json:"Tier"`
	Features     int      `json:"Features"`
	Score        float64  `json:"Score"`
	Servers      []Server `json:"Servers"`
	Status       int      `json:"Status"`
	Load         int      `json:"Load"`
}

// Server represents a physical VPN server
type Server struct {
	EntryIP         string `json:"EntryIP"`
	ExitIP          string `json:"ExitIP"`
	Domain          string `json:"Domain"`
	X25519PublicKey string `json:"X25519PublicKey"`
	Status          int    `json:"Status"`
}

// Features represents VPN server features
type Features struct {
	Bouncing       string `json:"Bouncing"`
	PortForwarding bool   `json:"PortForwarding"`
	SplitTCP       bool   `json:"SplitTCP"`
	PeerName       string `json:"peerName"`
	PeerIP         string `json:"peerIp"`
	PeerPublicKey  string `json:"peerPublicKey"`
	Platform       string `json:"platform"`
}

// Session holds authentication state
type Session struct {
	UID       string
	UserID    string
	Cookies   []*http.Cookie
	ExpiresAt time.Time
}

// SessionResponse represents the response from /api/auth/v4/sessions
type SessionResponse struct {
	Code         int      `json:"Code"`
	AccessToken  string   `json:"AccessToken"`
	RefreshToken string   `json:"RefreshToken"`
	TokenType    string   `json:"TokenType"`
	Scopes       []string `json:"Scopes"`
	UID          string   `json:"UID"`
	LocalID      int      `json:"LocalID"`
}
