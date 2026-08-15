package metrics

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("failed to create metrics store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordAndSnapshot(t *testing.T) {
	s := newTestStore(t)

	for i := 0; i < 3; i++ {
		if err := s.Record("meta-llama/llama-3.1-8b-instruct:free", false, true, 200, 120*time.Millisecond); err != nil {
			t.Fatalf("failed to record: %v", err)
		}
	}
	if err := s.Record("google/gemma-2-9b-it:free", true, false, 429, 30*time.Millisecond); err != nil {
		t.Fatalf("failed to record: %v", err)
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatalf("failed to snapshot: %v", err)
	}

	if snap.Summary.TotalRequests != 4 {
		t.Errorf("expected 4 total requests, got %d", snap.Summary.TotalRequests)
	}
	if snap.Summary.TotalErrors != 1 {
		t.Errorf("expected 1 error, got %d", snap.Summary.TotalErrors)
	}
	if snap.Summary.StreamRequests != 1 {
		t.Errorf("expected 1 stream request, got %d", snap.Summary.StreamRequests)
	}
	if snap.Summary.SuccessRate != 75 {
		t.Errorf("expected 75%% success rate, got %f", snap.Summary.SuccessRate)
	}
	if snap.Summary.AvgLatencyMS != 97.5 {
		t.Errorf("expected 97.5ms avg latency, got %f", snap.Summary.AvgLatencyMS)
	}

	if len(snap.Traffic) != DefaultTrafficWindow {
		t.Errorf("expected %d traffic points, got %d", DefaultTrafficWindow, len(snap.Traffic))
	}
	if snap.Traffic[len(snap.Traffic)-1].Requests != 4 {
		t.Errorf("expected last traffic bucket to have 4 requests, got %d", snap.Traffic[len(snap.Traffic)-1].Requests)
	}

	if len(snap.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(snap.Models))
	}
	if snap.Models[0].Model != "meta-llama/llama-3.1-8b-instruct:free" || snap.Models[0].Requests != 3 {
		t.Errorf("unexpected top model usage: %+v", snap.Models[0])
	}
}

func TestUnknownModelIsTracked(t *testing.T) {
	s := newTestStore(t)
	if err := s.Record("", false, true, 200, 10*time.Millisecond); err != nil {
		t.Fatalf("failed to record: %v", err)
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatalf("failed to snapshot: %v", err)
	}
	if len(snap.Models) != 1 || snap.Models[0].Model != "unknown" {
		t.Errorf("expected unknown model tracked, got %+v", snap.Models)
	}
}

func TestPrune(t *testing.T) {
	s := newTestStore(t)

	// Insert an old row directly.
	if _, err := s.db.Exec(
		`INSERT INTO requests (timestamp, model, stream, success, status, latency_ms) VALUES (?, 'old', 0, 1, 200, 5)`,
		time.Now().Add(-PruneAge-24*time.Hour).UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("failed to insert old row: %v", err)
	}
	if err := s.Record("new", false, true, 200, 5*time.Millisecond); err != nil {
		t.Fatalf("failed to record: %v", err)
	}

	if err := s.Prune(); err != nil {
		t.Fatalf("failed to prune: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil {
		t.Fatalf("failed to count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after prune, got %d", count)
	}
}