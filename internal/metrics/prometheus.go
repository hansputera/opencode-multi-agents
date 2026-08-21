package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// PrometheusExporter exposes metrics in Prometheus format
type PrometheusExporter struct {
	store           *Store
	start           time.Time
	requestCount    int64
	errorCount      int64
	totalTokens     int64
	promptTokens    int64
	completionTokens int64
	cachedTokens    int64
	estimatedCost   float64
	mu              sync.RWMutex
}

// NewPrometheusExporter creates a new Prometheus exporter
func NewPrometheusExporter(store *Store) *PrometheusExporter {
	return &PrometheusExporter{
		store: store,
		start: time.Now(),
	}
}

// RecordRequest increments the request counter and token counters
func (pe *PrometheusExporter) RecordRequest(success bool, usage Usage, cost float64) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.requestCount++
	if !success {
		pe.errorCount++
	}
	pe.totalTokens += int64(usage.TotalTokens)
	pe.promptTokens += int64(usage.PromptTokens)
	pe.completionTokens += int64(usage.CompletionTokens)
	pe.cachedTokens += int64(usage.CachedTokens)
	pe.estimatedCost += cost
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

		fmt.Fprintf(w, "# HELP opencode_tokens_total Total number of tokens\n")
		fmt.Fprintf(w, "# TYPE opencode_tokens_total counter\n")
		fmt.Fprintf(w, "opencode_tokens_total{type=\"prompt\"} %d\n", pe.promptTokens)
		fmt.Fprintf(w, "opencode_tokens_total{type=\"completion\"} %d\n", pe.completionTokens)
		fmt.Fprintf(w, "opencode_tokens_total{type=\"cached\"} %d\n", pe.cachedTokens)
		fmt.Fprintf(w, "opencode_tokens_total{type=\"total\"} %d\n", pe.totalTokens)

		fmt.Fprintf(w, "# HELP opencode_cost_estimated_total Estimated cost in dollars\n")
		fmt.Fprintf(w, "# TYPE opencode_cost_estimated_total counter\n")
		fmt.Fprintf(w, "opencode_cost_estimated_total %f\n", pe.estimatedCost)

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
