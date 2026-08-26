package config

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Account represents a ProtonVPN account credential stored in the config DB.
type Account struct {
	ID             int64  `json:"id"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	StorePath      string `json:"store_path"`
	SessionCookies string `json:"session_cookies,omitempty"`
	Enabled        bool   `json:"enabled"`
	CreatedAt      string `json:"created_at,omitempty"`
}

// ProxyConfig represents an external SOCKS5 proxy stored in the config DB.
type ProxyConfig struct {
	ID        int64  `json:"id"`
	Address   string `json:"address"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at,omitempty"`
}

// ConfigStore manages SQLite3 storage for runtime configuration.
type ConfigStore struct {
	db *sql.DB
}

// NewConfigStore opens (or creates) the config database and runs migrations.
func NewConfigStore(path string) (*ConfigStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("failed to open config database: %w", err)
	}
	s := &ConfigStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate config database: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *ConfigStore) Close() error {
	return s.db.Close()
}

func (s *ConfigStore) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL DEFAULT '',
			store_path TEXT NOT NULL DEFAULT '',
			session_cookies TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			address TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	return nil
}

// --- Accounts ---

// GetAccounts returns all accounts ordered by ID.
func (s *ConfigStore) GetAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT id, username, password, store_path, session_cookies, enabled, created_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []Account
	for rows.Next() {
		var a Account
		var enabled int
		if err := rows.Scan(&a.ID, &a.Username, &a.Password, &a.StorePath, &a.SessionCookies, &enabled, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Enabled = enabled == 1
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// GetAccount returns a single account by ID.
func (s *ConfigStore) GetAccount(id int64) (*Account, error) {
	var a Account
	var enabled int
	err := s.db.QueryRow(`SELECT id, username, password, store_path, session_cookies, enabled, created_at FROM accounts WHERE id = ?`, id).
		Scan(&a.ID, &a.Username, &a.Password, &a.StorePath, &a.SessionCookies, &enabled, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	return &a, nil
}

// CreateAccount inserts a new account and returns its ID.
func (s *ConfigStore) CreateAccount(a *Account) error {
	enabled := 0
	if a.Enabled {
		enabled = 1
	}
	res, err := s.db.Exec(`INSERT INTO accounts (username, password, store_path, session_cookies, enabled) VALUES (?, ?, ?, ?, ?)`,
		a.Username, a.Password, a.StorePath, a.SessionCookies, enabled)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// UpdateAccount updates an existing account.
func (s *ConfigStore) UpdateAccount(a *Account) error {
	enabled := 0
	if a.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(`UPDATE accounts SET username = ?, password = ?, store_path = ?, session_cookies = ?, enabled = ? WHERE id = ?`,
		a.Username, a.Password, a.StorePath, a.SessionCookies, enabled, a.ID)
	return err
}

// DeleteAccount removes an account by ID.
func (s *ConfigStore) DeleteAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	return err
}

// --- Proxies ---

// GetProxies returns all external SOCKS5 proxies ordered by ID.
func (s *ConfigStore) GetProxies() ([]ProxyConfig, error) {
	rows, err := s.db.Query(`SELECT id, address, enabled, created_at FROM proxies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var proxies []ProxyConfig
	for rows.Next() {
		var p ProxyConfig
		var enabled int
		if err := rows.Scan(&p.ID, &p.Address, &enabled, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Enabled = enabled == 1
		proxies = append(proxies, p)
	}
	return proxies, rows.Err()
}

// GetProxy returns a single proxy by ID.
func (s *ConfigStore) GetProxy(id int64) (*ProxyConfig, error) {
	var p ProxyConfig
	var enabled int
	err := s.db.QueryRow(`SELECT id, address, enabled, created_at FROM proxies WHERE id = ?`, id).
		Scan(&p.ID, &p.Address, &enabled, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	p.Enabled = enabled == 1
	return &p, nil
}

// CreateProxy inserts a new external proxy and returns its ID.
func (s *ConfigStore) CreateProxy(p *ProxyConfig) error {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	res, err := s.db.Exec(`INSERT INTO proxies (address, enabled) VALUES (?, ?)`, p.Address, enabled)
	if err != nil {
		return err
	}
	p.ID, _ = res.LastInsertId()
	return nil
}

// UpdateProxy updates an existing external proxy.
func (s *ConfigStore) UpdateProxy(p *ProxyConfig) error {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(`UPDATE proxies SET address = ?, enabled = ? WHERE id = ?`, p.Address, enabled, p.ID)
	return err
}

// DeleteProxy removes an external proxy by ID.
func (s *ConfigStore) DeleteProxy(id int64) error {
	_, err := s.db.Exec(`DELETE FROM proxies WHERE id = ?`, id)
	return err
}

// --- Settings ---

// GetSettings returns all settings as a key-value map.
func (s *ConfigStore) GetSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// GetSetting returns a single setting value.
func (s *ConfigStore) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetSetting upserts a setting.
func (s *ConfigStore) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, value)
	return err
}

// SetSettings upserts multiple settings in one transaction.
func (s *ConfigStore) SetSettings(m map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, v := range m {
		if _, err := stmt.Exec(k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SeedFromConfig migrates .env configuration into the store on first boot.
// It only seeds if the accounts table is empty.
func (s *ConfigStore) SeedFromConfig(cfg *Config) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil // already seeded
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Seed accounts from .env
	accounts := cfg.ParseProtonVPNAccounts()
	for i, acct := range accounts {
		storePath := cfg.ProtonVPNStorePath
		if i > 0 {
			storePath = fmt.Sprintf("%s_%d", cfg.ProtonVPNStorePath, i)
		}
		sessionCookies := ""
		if i == 0 {
			sessionCookies = cfg.ProtonVPNSessionCookies
		}
		if _, err := tx.Exec(`INSERT INTO accounts (username, password, store_path, session_cookies, enabled) VALUES (?, ?, ?, ?, 1)`,
			acct.Username, acct.Password, storePath, sessionCookies); err != nil {
			return fmt.Errorf("failed to seed account %d: %w", i, err)
		}
	}

	// Seed external proxies from .env
	for _, addr := range cfg.ParseSOCKS5Addrs() {
		if _, err := tx.Exec(`INSERT INTO proxies (address, enabled) VALUES (?, 1)`, addr); err != nil {
			return fmt.Errorf("failed to seed proxy: %w", err)
		}
	}

	// Seed key settings
	settings := map[string]string{
		"pool_size":            fmt.Sprintf("%d", cfg.ProxyPoolSize),
		"regions":              cfg.ProtonVPNRegions,
		"ip_check_url":         cfg.ProtonVPNIPCheckURL,
		"cooldown_duration":    cfg.CooldownDuration.String(),
		"ip_ban_duration":      cfg.IPBanDuration.String(),
		"health_check_period":  cfg.HealthCheckPeriod.String(),
		"resource_cpu_limit":   cfg.ResourceCPULimit,
		"resource_memory_limit": cfg.ResourceMemoryLimit,
		"max_retries":          fmt.Sprintf("%d", cfg.MaxRetries),
		"request_timeout":      cfg.RequestTimeout.String(),
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, v := range settings {
		if _, err := stmt.Exec(k, v); err != nil {
			return fmt.Errorf("failed to seed setting %s: %w", k, err)
		}
	}

	return tx.Commit()
}

// helper to parse duration from settings with fallback
func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// helper to parse int from settings with fallback
func parseInt(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	var v int
	fmt.Sscanf(s, "%d", &v)
	if v == 0 {
		return fallback
	}
	return v
}
