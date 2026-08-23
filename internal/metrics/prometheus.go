package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// endpointKey groups Prometheus counters by route + status class.
type endpointKey struct {
	route  string
	status int // status class: 2, 3, 4, 5 (hundreds)
}

// PrometheusExporter exposes metrics in Prometheus format
type PrometheusExporter struct {
	store            *Store
	start            time.Time
	requestCount     int64
	errorCount       int64
	totalTokens      int64
	promptTokens     int64
	completionTokens int64
	cachedTokens     int64
	estimatedCost    float64
	mu               sync.RWMutex

	epRequests map[endpointKey]int64
	epBytes    map[string]int64 // route -> bytes served
}

func statusClass(status int) int { return status / 100 }

// NewPrometheusExporter creates a new Prometheus exporter
func NewPrometheusExporter(store *Store) *PrometheusExporter {
	return &PrometheusExporter{
		store:      store,
		start:      time.Now(),
		epRequests: make(map[endpointKey]int64),
		epBytes:    make(map[string]int64),
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

// RecordEndpointTraffic counts one server request by route and status class,
// plus bytes served. Route labels are bounded (normalized templates), so the
// label cardinality stays small.
func (pe *PrometheusExporter) RecordEndpointTraffic(route string, status int, duration time.Duration, bytesOut int64) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	k := endpointKey{route: route, status: statusClass(status)}
	pe.epRequests[k]++
	if bytesOut > 0 {
		pe.epBytes[route] += bytesOut
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

		// Per-endpoint server traffic.
		keys := make([]endpointKey, 0, len(pe.epRequests))
		for k := range pe.epRequests {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].route != keys[j].route {
				return keys[i].route < keys[j].route
			}
			return keys[i].status < keys[j].status
		})
		fmt.Fprintf(w, "# HELP opencode_endpoint_requests_total Requests per route and status class\n")
		fmt.Fprintf(w, "# TYPE opencode_endpoint_requests_total counter\n")
		for _, k := range keys {
			fmt.Fprintf(w, "opencode_endpoint_requests_total{route=%q,status=%q} %d\n",
				k.route, strconv.Itoa(k.status)+"xx", pe.epRequests[k])
		}
		routes := make([]string, 0, len(pe.epBytes))
		for r := range pe.epBytes {
			routes = append(routes, r)
		}
		sort.Strings(routes)
		fmt.Fprintf(w, "# HELP opencode_bytes_served_total Response bytes served per route\n")
		fmt.Fprintf(w, "# TYPE opencode_bytes_served_total counter\n")
		for _, r := range routes {
			fmt.Fprintf(w, "opencode_bytes_served_total{route=%q} %d\n", r, pe.epBytes[r])
		}
	}
}
