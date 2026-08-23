package pow

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists PoW challenges and issued API keys in SQLite. Challenge
// issuance is deliberately low-QPS (that is the point of the gate), so WAL
// SQLite is more than sufficient; the queries below are all O(1) indexed
// lookups so verification stays in the microsecond range.
type Store struct {
	db *sql.DB
}

// Open creates/opens the SQLite store and applies the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create pow data directory: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open pow database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect pow database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS pow_challenges (
			id          TEXT PRIMARY KEY,
			bind        TEXT NOT NULL,
			plan        TEXT NOT NULL,
			algo        TEXT NOT NULL,
			difficulty  INTEGER NOT NULL,
			salt        TEXT NOT NULL,
			issued_at   INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL,
			used        INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_pow_challenges_expiry ON pow_challenges (expires_at);

		CREATE TABLE IF NOT EXISTS api_keys (
			key_hash   TEXT PRIMARY KEY,
			prefix     TEXT NOT NULL,
			plan       TEXT NOT NULL,
			rpm        INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			disabled   INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_api_keys_expiry ON api_keys (expires_at);

		CREATE TABLE IF NOT EXISTS pow_ip_bonus (
			ip_hash    TEXT PRIMARY KEY,
			bonus_bits INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
	`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// StoredChallenge is a challenge as persisted server-side.
type StoredChallenge struct {
	ID         string
	Bind       string
	Plan       string
	Algo       string
	Difficulty int
	Salt       string
	IssuedAt   int64
	ExpiresAt  int64
	Used       bool
}

// ToChallenge materializes the client-facing challenge object.
func (sc *StoredChallenge) ToChallenge() *Challenge {
	return &Challenge{
		Version:    Version,
		ID:         sc.ID,
		Resource:   ResourceAPIKey,
		Algo:       sc.Algo,
		Difficulty: sc.Difficulty,
		Salt:       sc.Salt,
		Bind:       sc.Bind,
		IssuedAt:   sc.IssuedAt,
		ExpiresAt:  sc.ExpiresAt,
		Plan:       sc.Plan,
	}
}

// InsertChallenge persists a freshly minted challenge.
func (s *Store) InsertChallenge(sc *StoredChallenge) error {
	_, err := s.db.Exec(`INSERT INTO pow_challenges
		(id, bind, plan, algo, difficulty, salt, issued_at, expires_at, used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		sc.ID, sc.Bind, sc.Plan, sc.Algo, sc.Difficulty, sc.Salt, sc.IssuedAt, sc.ExpiresAt)
	return err
}

// GetChallenge loads an unconsumed challenge. Returns nil when unknown.
func (s *Store) GetChallenge(id string) (*StoredChallenge, error) {
	var sc StoredChallenge
	var used int
	err := s.db.QueryRow(`SELECT id, bind, plan, algo, difficulty, salt, issued_at, expires_at, used
		FROM pow_challenges WHERE id = ?`, id).
		Scan(&sc.ID, &sc.Bind, &sc.Plan, &sc.Algo, &sc.Difficulty, &sc.Salt, &sc.IssuedAt, &sc.ExpiresAt, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sc.Used = used != 0
	return &sc, nil
}

// ConsumeChallenge atomically marks a challenge used. It returns false when
// the challenge was already consumed (or vanished) — the single-use guard.
func (s *Store) ConsumeChallenge(id string) (bool, error) {
	res, err := s.db.Exec(`UPDATE pow_challenges SET used = 1
		WHERE id = ? AND used = 0 AND expires_at > ?`, id, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// CleanupExpired removes stale challenges (older than ttl*2) and expired keys
// rows. Safe to run periodically.
func (s *Store) CleanupExpired() error {
	cutoff := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := s.db.Exec(`DELETE FROM pow_challenges WHERE expires_at < ?`, cutoff); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM api_keys WHERE expires_at < ? OR disabled = 1`, time.Now().Unix()); err != nil {
		return err
	}
	return nil
}

// APIKey is an issued key as seen by the server (never the key itself).
type APIKey struct {
	KeyHash   string
	Prefix    string
	Plan      string
	RPM       int
	CreatedAt int64
	ExpiresAt int64
}

// InsertAPIKey stores a newly issued key (hashed).
func (s *Store) InsertAPIKey(k APIKey) error {
	_, err := s.db.Exec(`INSERT INTO api_keys (key_hash, prefix, plan, rpm, created_at, expires_at, disabled)
		VALUES (?, ?, ?, ?, ?, ?, 0)`,
		k.KeyHash, k.Prefix, k.Plan, k.RPM, k.CreatedAt, k.ExpiresAt)
	return err
}

// GetAPIKey returns a live (unexpired, enabled) key by hash. Returns nil when
// unknown/expired/disabled.
func (s *Store) GetAPIKey(keyHash string) (*APIKey, error) {
	var k APIKey
	err := s.db.QueryRow(`SELECT key_hash, prefix, plan, rpm, created_at, expires_at
		FROM api_keys WHERE key_hash = ? AND disabled = 0 AND expires_at > ?`,
		keyHash, time.Now().Unix()).
		Scan(&k.KeyHash, &k.Prefix, &k.Plan, &k.RPM, &k.CreatedAt, &k.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// ListAPIKeys loads all live keys — used at startup to warm the auth cache.
func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT key_hash, prefix, plan, rpm, created_at, expires_at
		FROM api_keys WHERE disabled = 0 AND expires_at > ?`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.KeyHash, &k.Prefix, &k.Plan, &k.RPM, &k.CreatedAt, &k.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// BumpIPBonus raises the per-IP difficulty bonus by one bit (capped at max)
// after a successful redemption — farming many keys from one IP gets
// exponentially harder. Returns the NEW bonus for that IP hash.
func (s *Store) BumpIPBonus(ipHash string, max int) (int, error) {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO pow_ip_bonus (ip_hash, bonus_bits, updated_at)
		VALUES (?, 1, ?)
		ON CONFLICT(ip_hash) DO UPDATE SET
			bonus_bits = MIN(bonus_bits + 1, ?),
			updated_at = excluded.updated_at`,
		ipHash, now, max)
	if err != nil {
		return 0, err
	}
	var bits int
	err = s.db.QueryRow(`SELECT bonus_bits FROM pow_ip_bonus WHERE ip_hash = ?`, ipHash).Scan(&bits)
	return bits, err
}

// IPBonus returns the current difficulty bonus for an IP hash (0 if none).
func (s *Store) IPBonus(ipHash string) int {
	var bits int
	// Bonuses decay after 24h of inactivity via updated_at filter.
	err := s.db.QueryRow(`SELECT bonus_bits FROM pow_ip_bonus
		WHERE ip_hash = ? AND updated_at > ?`,
		ipHash, time.Now().Add(-24*time.Hour).Unix()).Scan(&bits)
	if err != nil {
		return 0
	}
	return bits
}
