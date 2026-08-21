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
		if err := s.Record("meta-llama/llama-3.1-8b-instruct:free", false, true, 200, 120*time.Millisecond, Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, 0.001); err != nil {
			t.Fatalf("failed to record: %v", err)
		}
	}
	if err := s.Record("google/gemma-2-9b-it:free", true, false, 429, 30*time.Millisecond, Usage{}, 0); err != nil {
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

	// Check token totals
	if snap.Summary.TotalTokens != 450 {
		t.Errorf("expected 450 total tokens, got %d", snap.Summary.TotalTokens)
	}
	if snap.Summary.TotalPromptTokens != 300 {
		t.Errorf("expected 300 prompt tokens, got %d", snap.Summary.TotalPromptTokens)
	}
	if snap.Summary.TotalComplTokens != 150 {
		t.Errorf("expected 150 completion tokens, got %d", snap.Summary.TotalComplTokens)
	}
	if snap.Summary.TotalEstimatedCost != 0.003 {
		t.Errorf("expected 0.003 estimated cost, got %f", snap.Summary.TotalEstimatedCost)
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
	if snap.Models[0].PromptTokens != 300 {
		t.Errorf("expected 300 prompt tokens for model, got %d", snap.Models[0].PromptTokens)
	}
	if snap.Models[0].CompletionTokens != 150 {
		t.Errorf("expected 150 completion tokens for model, got %d", snap.Models[0].CompletionTokens)
	}
}

func TestUnknownModelIsTracked(t *testing.T) {
	s := newTestStore(t)
	if err := s.Record("", false, true, 200, 10*time.Millisecond, Usage{}, 0); err != nil {
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
	if err := s.Record("new", false, true, 200, 5*time.Millisecond, Usage{}, 0); err != nil {
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

func TestPricingTable(t *testing.T) {
	pt := NewPricingTable("gpt-4o:2.50,10.00,1.25;gpt-3.5-turbo:0.50,1.50,0.25")

	// Test exact match
	entry, ok := pt.GetEntry("gpt-4o")
	if !ok {
		t.Fatal("expected to find gpt-4o")
	}
	if entry.InputPer1M != 2.50 {
		t.Errorf("expected input rate 2.50, got %f", entry.InputPer1M)
	}
	if entry.OutputPer1M != 10.00 {
		t.Errorf("expected output rate 10.00, got %f", entry.OutputPer1M)
	}
	if entry.CachedPer1M != 1.25 {
		t.Errorf("expected cached rate 1.25, got %f", entry.CachedPer1M)
	}

	// Test substring match
	entry, ok = pt.GetEntry("gpt-4o-2024-01-01")
	if !ok {
		t.Fatal("expected to find gpt-4o via substring match")
	}
	if entry.InputPer1M != 2.50 {
		t.Errorf("expected input rate 2.50, got %f", entry.InputPer1M)
	}

	// Test cost estimation
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500, CachedTokens: 100}
	cost := pt.EstimateCost("gpt-4o", usage)
	expectedCost := (1000*2.50 + 500*10.00 + 100*1.25) / 1_000_000
	if cost != expectedCost {
		t.Errorf("expected cost %f, got %f", expectedCost, cost)
	}

	// Test unknown model
	_, ok = pt.GetEntry("unknown-model")
	if ok {
		t.Error("expected not to find unknown-model")
	}
	cost = pt.EstimateCost("unknown-model", usage)
	if cost != 0 {
		t.Errorf("expected cost 0 for unknown model, got %f", cost)
	}
}

func TestPricingTableParse(t *testing.T) {
	pt := NewPricingTable("")

	// Empty config
	entry, ok := pt.GetEntry("gpt-4o")
	if ok {
		t.Error("expected not to find any entry in empty table")
	}

	// Parse new config
	pt.Parse("gpt-4o:2.50,10.00,1.25")
	entry, ok = pt.GetEntry("gpt-4o")
	if !ok {
		t.Fatal("expected to find gpt-4o after parse")
	}
	if entry.InputPer1M != 2.50 {
		t.Errorf("expected input rate 2.50, got %f", entry.InputPer1M)
	}

	// Parse with default cached rate
	pt.Parse("test-model:1.00,2.00")
	entry, ok = pt.GetEntry("test-model")
	if !ok {
		t.Fatal("expected to find test-model")
	}
	if entry.CachedPer1M != 0.50 {
		t.Errorf("expected cached rate 0.50 (50%% of input), got %f", entry.CachedPer1M)
	}
}
