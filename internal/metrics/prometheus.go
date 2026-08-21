package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// PrometheusExporter exposes metrics in Prometheus format
type PrometheusExporter struct {
	store      *Store
	start      time.Time
	requestCount int64
	errorCount   int64
	mu         sync.RWMutex
}

// NewPrometheusExporter creates a new Prometheus exporter
func NewPrometheusExporter(store *Store) *PrometheusExporter {
	return &PrometheusExporter{
		store: store,
		start: time.Now(),
	}
}

// RecordRequest increments the request counter
func (pe *PrometheusExporter) RecordRequest(success bool) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.requestCount++
	if !success {
		pe.errorCount++
	}
}

// Handler returns an HTTP handler that serves Prometheus metrics
func (pe *PrometheusExporter) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pe.mu.RLock()
		defer pe.mu.RUnlock()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintf(w, "# HELP opencode_requests_total Total number of requests\n")
		fmt.Fprintf(w, "# TYPE opencode_requests_total counter\n")
		fmt.Fprintf(w, "opencode_requests_total %d\n", pe.requestCount)

		fmt.Fprintf(w, "# HELP opencode_errors_total Total number of errors\n")
		fmt.Fprintf(w, "# TYPE opencode_errors_total counter\n")
		fmt.Fprintf(w, "opencode_errors_total %d\n", pe.errorCount)

		fmt.Fprintf(w, "# HELP opencode_uptime_seconds Uptime in seconds\n")
		fmt.Fprintf(w, "# TYPE opencode_uptime_seconds gauge\n")
		fmt.Fprintf(w, "opencode_uptime_seconds %f\n", time.Since(pe.start).Seconds())

		// Add snapshot-based metrics if available
		if snap, err := pe.store.Snapshot(); err == nil {
			fmt.Fprintf(w, "# HELP opencode_avg_latency_ms Average latency in milliseconds\n")
			fmt.Fprintf(w, "# TYPE opencode_avg_latency_ms gauge\n")
			fmt.Fprintf(w, "opencode_avg_latency_ms %f\n", snap.Summary.AvgLatencyMS)

			fmt.Fprintf(w, "# HELP opencode_success_rate Success rate percentage\n")
			fmt.Fprintf(w, "# TYPE opencode_success_rate gauge\n")
			fmt.Fprintf(w, "opencode_success_rate %f\n", snap.Summary.SuccessRate)
		}
	}
}