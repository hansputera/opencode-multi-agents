package protonvpn

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	_ "modernc.org/sqlite"
)

// Store manages SQLite3 storage for ProtonVPN data
type Store struct {
	db *sql.DB
}

// NewStore creates a new SQLite3 store
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	return s, nil
}

// Close closes the database
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate creates the schema
func (s *Store) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS credentials (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			username TEXT NOT NULL,
			password TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS session (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			uid TEXT NOT NULL,
			user_id TEXT NOT NULL,
			auth_key BLOB,
			cookies TEXT,
			expires_at DATETIME NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS certificates (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			certificate TEXT NOT NULL,
			client_key TEXT NOT NULL,
			server_public_key TEXT NOT NULL,
			server_ip TEXT NOT NULL,
			vpn_ip TEXT NOT NULL,
			expires_at DATETIME NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS server_cache (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			server_list_json TEXT NOT NULL,
			fetched_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS wireguard_keys (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			private_key TEXT NOT NULL,
			public_key TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS ed25519_keys (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			private_key BLOB NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("failed to execute migration: %w", err)
		}
	}

	return nil
}

// GetCredentials retrieves stored credentials
func (s *Store) GetCredentials() (username, password string, err error) {
	var u, p string
	err = s.db.QueryRow("SELECT username, password FROM credentials WHERE id = 1").Scan(&u, &p)
	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("no credentials stored")
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to get credentials: %w", err)
	}
	return u, p, nil
}

// SetCredentials stores credentials
func (s *Store) SetCredentials(username, password string) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO credentials (id, username, password) VALUES (1, ?, ?)",
		username, password,
	)
	if err != nil {
		return fmt.Errorf("failed to set credentials: %w", err)
	}
	return nil
}

// GetSession retrieves the stored session
func (s *Store) GetSession() (*Session, error) {
	var uid, userID string
	var cookiesJSON string
	var expiresAt time.Time

	err := s.db.QueryRow(
		"SELECT uid, user_id, cookies, expires_at FROM session WHERE id = 1",
	).Scan(&uid, &userID, &cookiesJSON, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no session stored")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	var cookies []*http.Cookie
	if cookiesJSON != "" {
		if err := json.Unmarshal([]byte(cookiesJSON), &cookies); err != nil {
			return nil, fmt.Errorf("failed to unmarshal cookies: %w", err)
		}
	}

	return &Session{
		UID:       uid,
		UserID:    userID,
		Cookies:   cookies,
		ExpiresAt: expiresAt,
	}, nil
}

// SetSession stores the session
func (s *Store) SetSession(session *Session) error {
	cookiesJSON, err := json.Marshal(session.Cookies)
	if err != nil {
		return fmt.Errorf("failed to marshal cookies: %w", err)
	}

	_, err = s.db.Exec(
		"INSERT OR REPLACE INTO session (id, uid, user_id, cookies, expires_at) VALUES (1, ?, ?, ?, ?)",
		session.UID, session.UserID, string(cookiesJSON), session.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("failed to set session: %w", err)
	}
	return nil
}

// GetCertificate retrieves the most recent valid certificate
func (s *Store) GetCertificate() (*CertificateResponse, error) {
	var cert CertificateResponse
	var expiresAt time.Time

	err := s.db.QueryRow(
		"SELECT certificate, client_key, server_public_key, server_ip, vpn_ip, expires_at FROM certificates WHERE expires_at > ? ORDER BY id DESC LIMIT 1",
		time.Now(),
	).Scan(&cert.Certificate, &cert.ClientKey, &cert.ServerPublicKey,
		&cert.Features.PeerIP, &cert.Features.PeerIP, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no valid certificate stored")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get certificate: %w", err)
	}

	cert.ExpirationTime = expiresAt.Unix()
	return &cert, nil
}

// SetCertificate stores a certificate
func (s *Store) SetCertificate(cert *CertificateResponse) error {
	_, err := s.db.Exec(
		"INSERT INTO certificates (certificate, client_key, server_public_key, server_ip, vpn_ip, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
		cert.Certificate, cert.ClientKey, cert.ServerPublicKey,
		cert.Features.PeerIP, cert.Features.PeerIP,
		time.Unix(cert.ExpirationTime, 0),
	)
	if err != nil {
		return fmt.Errorf("failed to set certificate: %w", err)
	}
	return nil
}

// GetServerList retrieves the cached server list
func (s *Store) GetServerList() (*ServerListResponse, error) {
	var jsonStr string
	var fetchedAt time.Time

	err := s.db.QueryRow(
		"SELECT server_list_json, fetched_at FROM server_cache WHERE id = 1",
	).Scan(&jsonStr, &fetchedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no server list cached")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get server list: %w", err)
	}

	// Cache for 1 hour
	if time.Since(fetchedAt) > time.Hour {
		return nil, fmt.Errorf("server list cache expired")
	}

	var list ServerListResponse
	if err := json.Unmarshal([]byte(jsonStr), &list); err != nil {
		return nil, fmt.Errorf("failed to unmarshal server list: %w", err)
	}

	return &list, nil
}

// SetServerList caches the server list
func (s *Store) SetServerList(list *ServerListResponse) error {
	jsonBytes, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("failed to marshal server list: %w", err)
	}

	_, err = s.db.Exec(
		"INSERT OR REPLACE INTO server_cache (id, server_list_json, fetched_at) VALUES (1, ?, ?)",
		string(jsonBytes), time.Now(),
	)
	if err != nil {
		return fmt.Errorf("failed to set server list: %w", err)
	}
	return nil
}

// GetWireGuardKey retrieves stored WireGuard keys
func (s *Store) GetWireGuardKey() (privateKey, publicKey string, err error) {
	var priv, pub string
	err = s.db.QueryRow("SELECT private_key, public_key FROM wireguard_keys WHERE id = 1").Scan(&priv, &pub)
	if err == sql.ErrNoRows {
		return "", "", fmt.Errorf("no wireguard keys stored")
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to get wireguard keys: %w", err)
	}
	return priv, pub, nil
}

// SetWireGuardKey stores WireGuard keys
func (s *Store) SetWireGuardKey(privateKey, publicKey string) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO wireguard_keys (id, private_key, public_key) VALUES (1, ?, ?)",
		privateKey, publicKey,
	)
	if err != nil {
		return fmt.Errorf("failed to set wireguard keys: %w", err)
	}
	return nil
}

// GetEd25519Key retrieves the stored Ed25519 private key
func (s *Store) GetEd25519Key() ([]byte, error) {
	var privKeyBytes []byte
	err := s.db.QueryRow(
		"SELECT private_key FROM ed25519_keys WHERE id = 1",
	).Scan(&privKeyBytes)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no Ed25519 keys stored")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get Ed25519 keys: %w", err)
	}
	return privKeyBytes, nil
}

// SetEd25519Key stores an Ed25519 private key
func (s *Store) SetEd25519Key(privKeyBytes []byte) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO ed25519_keys (id, private_key) VALUES (1, ?)",
		privKeyBytes,
	)
	if err != nil {
		return fmt.Errorf("failed to set Ed25519 key: %w", err)
	}
	return nil
}

// DeleteEd25519Key deletes the stored Ed25519 key
func (s *Store) DeleteEd25519Key() error {
	_, err := s.db.Exec("DELETE FROM ed25519_keys WHERE id = 1")
	if err != nil {
		return fmt.Errorf("failed to delete Ed25519 key: %w", err)
	}
	return nil
}
