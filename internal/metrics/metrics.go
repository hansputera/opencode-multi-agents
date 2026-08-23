package metrics

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// PruneAge is how long request rows are kept before being deleted.
	PruneAge = 7 * 24 * time.Hour

	// DefaultTrafficWindow is the default time window (in minutes) for the traffic series.
	DefaultTrafficWindow = 30

	// currentSchemaVersion is the latest schema version.
	currentSchemaVersion = 3
)

// UntrackedPaths are infrastructure endpoints excluded from traffic
// accounting: monitoring self-polling would drown out real usage data
// (Prometheus scrapes, docker healthchecks, the dashboard's own refresh).
var UntrackedPaths = map[string]bool{
	"/metrics":     true,
	"/health":      true,
	"/api/metrics": true,
}

// Usage holds token counts for a single request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"prompt_tokens_cached,omitempty"`
}

// Store tracks request metrics in a SQLite database.
type Store struct {
	db     *sql.DB
	mu     sync.Mutex
	start  time.Time
	window int
}

// New opens (or creates) the SQLite database at path and initializes the schema.
func New(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create metrics directory: %w", err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open metrics database: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to metrics database: %w", err)
	}

	s := &Store{db: db, start: time.Now(), window: DefaultTrafficWindow}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

// migrate creates the schema and applies migrations.
func (s *Store) migrate() error {
	// Create schema version table
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("failed to create schema_version table: %w", err)
	}

	// Get current version
	var version int
	err = s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version)
	if err != nil {
		// Table might be empty, try inserting version 0
		_, err = s.db.Exec(`INSERT OR IGNORE INTO schema_version (version) VALUES (0)`)
		if err != nil {
			return fmt.Errorf("failed to initialize schema version: %w", err)
		}
		version = 0
	}

	// Apply migrations
	if version < 1 {
		if err := s.migrateV1(); err != nil {
			return fmt.Errorf("migration v1 failed: %w", err)
		}
		version = 1
	}

	if version < 2 {
		if err := s.migrateV2(); err != nil {
			return fmt.Errorf("migration v2 failed: %w", err)
		}
		version = 2
	}

	if version < 3 {
		if err := s.migrateV3(); err != nil {
			return fmt.Errorf("migration v3 failed: %w", err)
		}
		version = 3
	}

	return nil
}

// migrateV1 creates the initial requests table.
func (s *Store) migrateV1() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS requests (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp  TEXT NOT NULL,
			model      TEXT NOT NULL,
			stream     INTEGER NOT NULL DEFAULT 0,
			success    INTEGER NOT NULL DEFAULT 1,
			status     INTEGER NOT NULL DEFAULT 200,
			latency_ms INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests (timestamp);
		CREATE INDEX IF NOT EXISTS idx_requests_model ON requests (model);
		INSERT OR REPLACE INTO schema_version (version) VALUES (1);
	`)
	return err
}

// migrateV2 adds token and cost columns.
func (s *Store) migrateV2() error {
	// Check if columns already exist (for safety)
	var colCount int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name='total_tokens'`).Scan(&colCount)
	if err != nil || colCount > 0 {
		// Column already exists or error, skip migration
		_, _ = s.db.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (2)`)
		return nil
	}

	_, err = s.db.Exec(`
		ALTER TABLE requests ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE requests ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE requests ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE requests ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE requests ADD COLUMN estimated_cost REAL NOT NULL DEFAULT 0;
		INSERT OR REPLACE INTO schema_version (version) VALUES (2);
	`)
	return err
}

// migrateV3 adds the generic endpoint-traffic table (all server requests,
// not just chat completions).
func (s *Store) migrateV3() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS endpoint_traffic (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			ts          INTEGER NOT NULL,
			method      TEXT NOT NULL,
			route       TEXT NOT NULL,
			status      INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			bytes_out   INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_endpoint_traffic_ts ON endpoint_traffic (ts);
		CREATE INDEX IF NOT EXISTS idx_endpoint_traffic_route ON endpoint_traffic (route);
	`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (3)`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Prune removes request rows older than PruneAge. Safe to call on an interval.
func (s *Store) Prune() error {
	cutoff := time.Now().Add(-PruneAge).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM requests WHERE timestamp < ?`, cutoff); err != nil {
		return err
	}
	tsCutoff := time.Now().Add(-PruneAge).Unix()
	_, err := s.db.Exec(`DELETE FROM endpoint_traffic WHERE ts < ?`, tsCutoff)
	return err
}

// Record stores a single request metric.
func (s *Store) Record(model string, stream, success bool, status int, latency time.Duration, usage Usage, cost float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO requests (timestamp, model, stream, success, status, latency_ms, prompt_tokens, completion_tokens, total_tokens, cached_tokens, estimated_cost) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, orUnknown(model), boolToInt(stream), boolToInt(success), status, latency.Milliseconds(),
		usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.CachedTokens, cost,
	)
	return err
}

// RecordEndpoint stores one generic server request (any route). This is the
// "all traffic" layer — chat completions additionally get a rich row in
// requests via Record.
func (s *Store) RecordEndpoint(method, route string, status int, duration time.Duration, bytesOut int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO endpoint_traffic (ts, method, route, status, duration_ms, bytes_out)
		VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), method, route, status, duration.Milliseconds(), bytesOut)
	return err
}

// TrafficPoint is one bucket in the per-minute traffic series.
type TrafficPoint struct {
	Timestamp string `json:"timestamp"`
	Requests  int    `json:"requests"`
	Errors    int    `json:"errors"`
}

// ModelUsage is the usage count for a single model.
type ModelUsage struct {
	Model            string  `json:"model"`
	Requests         int     `json:"requests"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	EstimatedCost    float64 `json:"estimated_cost"`
}

// Summary holds the aggregated totals.
type Summary struct {
	TotalRequests      int     `json:"total_requests"`
	TotalErrors        int     `json:"total_errors"`
	SuccessRate        float64 `json:"success_rate"`
	AvgLatencyMS       float64 `json:"avg_latency_ms"`
	StreamRequests     int     `json:"stream_requests"`
	UptimeSeconds      int64   `json:"uptime_seconds"`
	TotalTokens        int64   `json:"total_tokens"`
	TotalPromptTokens  int64   `json:"total_prompt_tokens"`
	TotalComplTokens   int64   `json:"total_completion_tokens"`
	TotalCachedTokens  int64   `json:"total_cached_tokens"`
	TotalEstimatedCost float64 `json:"total_estimated_cost"`
}

// Snapshot is the full metrics payload served to the dashboard.
type Snapshot struct {
	Summary Summary        `json:"summary"`
	Traffic []TrafficPoint `json:"traffic"`
	Models  []ModelUsage   `json:"models"`
	Server  ServerStats    `json:"server"`
	Window  int            `json:"window"`
	Started time.Time      `json:"started_at"`
}

// EndpointUsage aggregates one route's traffic over the report window.
type EndpointUsage struct {
	Method       string  `json:"method"`
	Route        string  `json:"route"`
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	BytesOut     int64   `json:"bytes_out"`
}

// ServerStats is the server-wide traffic picture (all endpoints).
type ServerStats struct {
	TotalRequests int64           `json:"total_requests"`
	TotalErrors   int64           `json:"total_errors"`
	BytesOut      int64           `json:"bytes_out"`
	Routes        int64           `json:"routes"`
	Endpoints     []EndpointUsage `json:"endpoints"` // top routes, last 24h
}

// Snapshot returns the current metrics: summary totals, per-minute traffic
// series (ALL server requests) for the last window minutes (zero-filled),
// per-model usage, and server-wide endpoint stats.
func (s *Store) Snapshot() (Snapshot, error) {
	snap := Snapshot{
		Window:  s.window,
		Started: s.start,
	}

	if err := s.summary(&snap); err != nil {
		return snap, err
	}
	if err := s.traffic(&snap); err != nil {
		return snap, err
	}
	if err := s.models(&snap); err != nil {
		return snap, err
	}
	if err := s.server(&snap); err != nil {
		return snap, err
	}
	return snap, nil
}

func (s *Store) summary(snap *Snapshot) error {
	var avg sql.NullFloat64
	err := s.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(latency_ms), 0),
			COALESCE(SUM(CASE WHEN stream = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(estimated_cost), 0)
		FROM requests
	`).Scan(
		&snap.Summary.TotalRequests,
		&snap.Summary.TotalErrors,
		&avg,
		&snap.Summary.StreamRequests,
		&snap.Summary.TotalTokens,
		&snap.Summary.TotalPromptTokens,
		&snap.Summary.TotalComplTokens,
		&snap.Summary.TotalCachedTokens,
		&snap.Summary.TotalEstimatedCost,
	)
	if err != nil {
		return fmt.Errorf("failed to load summary: %w", err)
	}

	snap.Summary.AvgLatencyMS = avg.Float64
	if snap.Summary.TotalRequests > 0 {
		snap.Summary.SuccessRate = float64(snap.Summary.TotalRequests-snap.Summary.TotalErrors) / float64(snap.Summary.TotalRequests) * 100
	}
	snap.Summary.UptimeSeconds = int64(time.Since(s.start).Seconds())
	return nil
}

// traffic builds the per-minute request series from endpoint_traffic —
// ALL server requests (API, PoW, UI), not just chat completions.
func (s *Store) traffic(snap *Snapshot) error {
	start := time.Now().Add(-time.Duration(s.window) * time.Minute)
	rows, err := s.db.Query(`
		SELECT strftime('%Y-%m-%dT%H:%M', ts, 'unixepoch') AS bucket,
		       COUNT(*) AS total,
		       COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0) AS errors
		FROM endpoint_traffic
		WHERE ts >= ?
		GROUP BY bucket
		ORDER BY bucket
	`, start.Unix())
	if err != nil {
		return fmt.Errorf("failed to load traffic: %w", err)
	}
	defer rows.Close()

	counts := make(map[string][2]int)
	for rows.Next() {
		var bucket string
		var total, errors int
		if err := rows.Scan(&bucket, &total, &errors); err != nil {
			return fmt.Errorf("failed to scan traffic row: %w", err)
		}
		counts[bucket] = [2]int{total, errors}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("traffic rows iteration error: %w", err)
	}

	// Zero-fill missing buckets.
	now := time.Now()
	for i := s.window - 1; i >= 0; i-- {
		t := now.Add(-time.Duration(i) * time.Minute)
		bucket := t.Format("2006-01-02T15:04")
		c := counts[bucket]
		snap.Traffic = append(snap.Traffic, TrafficPoint{
			Timestamp: t.Truncate(time.Minute).Format(time.RFC3339),
			Requests:  c[0],
			Errors:    c[1],
		})
	}
	return nil
}

func (s *Store) models(snap *Snapshot) error {
	rows, err := s.db.Query(`
		SELECT model, COUNT(*) AS total,
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(estimated_cost), 0)
		FROM requests
		GROUP BY model
		ORDER BY total DESC
	`)
	if err != nil {
		return fmt.Errorf("failed to load model usage: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests, &m.PromptTokens, &m.CompletionTokens, &m.TotalTokens, &m.EstimatedCost); err != nil {
			return fmt.Errorf("failed to scan model row: %w", err)
		}
		snap.Models = append(snap.Models, m)
	}
	return rows.Err()
}

// server aggregates server-wide totals and the top endpoints (last 24h).
func (s *Store) server(snap *Snapshot) error {
	st := ServerStats{}
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(bytes_out), 0),
		       COUNT(DISTINCT route)
		FROM endpoint_traffic
	`).Scan(&st.TotalRequests, &st.TotalErrors, &st.BytesOut, &st.Routes)
	if err != nil {
		return fmt.Errorf("failed to load server traffic totals: %w", err)
	}

	rows, err := s.db.Query(`
		SELECT method, route, COUNT(*),
		       COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(AVG(duration_ms), 0),
		       COALESCE(SUM(bytes_out), 0)
		FROM endpoint_traffic
		WHERE ts >= ?
		GROUP BY method, route
		ORDER BY COUNT(*) DESC
		LIMIT 15
	`, time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		return fmt.Errorf("failed to load endpoint usage: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e EndpointUsage
		var avg sql.NullFloat64
		if err := rows.Scan(&e.Method, &e.Route, &e.Requests, &e.Errors, &avg, &e.BytesOut); err != nil {
			return fmt.Errorf("failed to scan endpoint row: %w", err)
		}
		e.AvgLatencyMS = avg.Float64
		st.Endpoints = append(st.Endpoints, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	snap.Server = st
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
