package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/metrics"
	"github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/hansputera/opencode-multi-agents/internal/upstream"
	"github.com/hansputera/opencode-multi-agents/internal/web"
	"github.com/rs/zerolog"
)

// Handler handles HTTP requests for the OpenAI-compatible API
type Handler struct {
	cfg     *config.Config
	pool    *proxy.PoolManager
	client  upstream.Upstream
	metrics *metrics.Store
	prometheus *metrics.PrometheusExporter
	log     *zerolog.Logger
	mux     *http.ServeMux
}

// New creates a new HTTP handler
func New(cfg *config.Config, pool *proxy.PoolManager, metricsStore *metrics.Store, log *zerolog.Logger) http.Handler {
	var client upstream.Upstream
	switch cfg.UpstreamProvider {
	case "opencode":
		client = upstream.NewOpenCodeClient(cfg, log)
	case "opencode-cli":
		client = upstream.NewOpenCodeCLIClient(cfg, log)
	default:
		client = upstream.NewClient(cfg, log)
	}

	prometheus := metrics.NewPrometheusExporter(metricsStore)

	h := &Handler{
		cfg:     cfg,
		pool:    pool,
		client:  client,
		metrics: metricsStore,
		prometheus: prometheus,
		log:     log,
		mux:     http.NewServeMux(),
	}

	// Register routes
	h.mux.HandleFunc("GET /v1/models", h.handleModels)
	h.mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	h.mux.HandleFunc("GET /health", h.handleHealth)
	h.mux.HandleFunc("GET /stats", h.handleStats)
	h.mux.HandleFunc("GET /api/metrics", h.handleMetrics)
	h.mux.HandleFunc("GET /metrics", prometheus.Handler())

	// Serve the web UI
	h.mux.Handle("/", web.Handler())

	// Middleware
	return h.loggingMiddleware(h.requestIDMiddleware(h.corsMiddleware(h.mux)))
}

// loggingMiddleware logs all requests
func (h *Handler) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create response wrapper for status code capture
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rw, r)

		duration := time.Since(start)
		reqID := GetRequestID(r.Context())

		event := h.log.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rw.statusCode).
			Dur("duration", duration)

		if reqID != "" {
			event = event.Str("request_id", reqID)
		}

		event.Msg("Request")
	})
}

// corsMiddleware adds CORS headers
func (h *Handler) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := h.cfg.CORSOrigin
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware generates a unique request ID and adds it to the context
func (h *Handler) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use existing X-Request-ID header if provided by client
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}

		// Add request ID to context
		ctx := context.WithValue(r.Context(), "request_id", reqID)
		r = r.WithContext(ctx)

		// Add request ID to response header
		w.Header().Set("X-Request-ID", reqID)

		next.ServeHTTP(w, r)
	})
}

// GetRequestID extracts request ID from context
func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value("request_id").(string); ok {
		return id
	}
	return ""
}

// resolveAuth picks the upstream auth header per request:
//  1. client's x-api-key header, if non-empty (empty/whitespace is ignored
//     so a stale "x-api-key:" never reaches the upstream)
//  2. client's Authorization Bearer header (if non-empty)
//  3. a random key from UPSTREAM_API_KEYS (spreads quota/rate limits across
//     multiple accounts when the client sends no key)
//  4. the configured UPSTREAM_API_KEY (as Bearer)
//  5. OpenCode Zen's public access via "x-api-key: public"
func (h *Handler) resolveAuth(r *http.Request) upstream.Auth {
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		return upstream.Auth{Header: "x-api-key", Value: key}
	}
	if key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); key != "" {
		return upstream.Auth{Header: "Authorization", Value: "Bearer " + key}
	}
	if len(h.cfg.UpstreamAPIKeys) > 0 {
		return authForEntry(h.cfg.UpstreamAPIKeys[rand.IntN(len(h.cfg.UpstreamAPIKeys))])
	}
	if h.cfg.UpstreamAPIKey != "" {
		return upstream.Auth{Header: "Authorization", Value: "Bearer " + h.cfg.UpstreamAPIKey}
	}
	return upstream.Auth{Header: "x-api-key", Value: "public"}
}

// authForEntry maps an UPSTREAM_API_KEYS entry to an upstream auth header:
//   - "header:value" → sent as-is on that header
//   - "public..."    → sent as "x-api-key: <value>"
//   - anything else  → sent as "Authorization: Bearer <value>"
func authForEntry(entry string) upstream.Auth {
	if i := strings.Index(entry, ":"); i > 0 {
		return upstream.Auth{Header: entry[:i], Value: entry[i+1:]}
	}
	if strings.HasPrefix(entry, "public") {
		return upstream.Auth{Header: "x-api-key", Value: entry}
	}
	return upstream.Auth{Header: "Authorization", Value: "Bearer " + entry}
}

// isPublicTier reports whether auth selects OpenCode Zen's shared public tier
// (the "public" / "public..." account). The public tier is rate limited by
// account identity, not by egress IP, so rotating VPN containers cannot reset
// a 429 — callers should short-circuit and return 429+Retry-After instead of
// churning fresh proxies in a retry loop.
func isPublicTier(auth upstream.Auth) bool {
	if auth.Header != "x-api-key" {
		return false
	}
	return strings.TrimSpace(auth.Value) == "public" || strings.HasPrefix(auth.Value, "public")
}

// retryAfterHeader chooses the Retry-After value to report to clients on an
// upstream rate limit: prefer the upstream's Retry-After, fall back to the
// configured static value.
func retryAfterHeader(rl *upstream.RateLimit, fallback string) string {
	if rl != nil && rl.RetryAfter != "" {
		return rl.RetryAfter
	}
	return fallback
}

// handleModels returns the model list from the upstream provider
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	auth := h.resolveAuth(r)

	// Get proxy from pool, bounded by RateLimitFreshIPWait so a fully-banned
	// pool returns 429 + Retry-After instead of hanging the client.
	ctx := r.Context()
	acquireCtx, cancel := context.WithTimeout(ctx, h.cfg.RateLimitFreshIPWait)
	proxy, err := h.pool.GetProxy(acquireCtx, "")
	cancel()
	if err != nil {
		h.writeError(w, http.StatusTooManyRequests, "Rate limited by upstream provider, try again later")
		w.Header().Set("Retry-After", h.cfg.RateLimitRetryAfter)
		h.log.Warn().Msg("No unbanned proxy available within wait window")
		return
	}

	// Forward with retry. On an upstream rate limit we ban the proxy's egress
	// IP and wait (bounded by RateLimitFreshIPWait) for a fresh, unbanned
	// proxy whose new VPN container has booted, rather than immediately
	// returning 429 to the client.
	var lastErr error
	var rateLimited bool
	freshDeadline := time.Time{}

	for attempt := 0; attempt <= h.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if rateLimited {
				if freshDeadline.IsZero() {
					freshDeadline = time.Now().Add(h.cfg.RateLimitFreshIPWait)
				}
				left := time.Until(freshDeadline)
				if left <= 0 {
					rateLimited = false
					lastErr = fmt.Errorf("rate limited: timed out waiting for a fresh proxy")
					break
				}
				waitCtx, cancel := context.WithTimeout(ctx, left)
				proxy, err = h.pool.GetProxy(waitCtx, "")
				cancel()
				if err != nil {
					lastErr = err
					rateLimited = false
					break
				}
				// Fresh proxy acquired; use it for this attempt directly.
				rateLimited = false
				continue
			}
			delay := h.calculateRetryDelay(attempt)
			h.log.Warn().Int("attempt", attempt).Dur("delay", delay).Msg("Retrying models request")
			time.Sleep(delay)
		}

		resp, rl, err := h.client.DoModels(ctx, proxy, auth)

		if rl != nil {
			h.pool.MarkRateLimited(proxy, rl.RetryAfter, isPublicTier(auth))
			// Public tier is rate limited by account identity, not by egress IP:
			// spinning a fresh VPN container cannot reset it, so return an
			// honest 429+Retry-After instead of churning the pool.
			if isPublicTier(auth) {
				w.Header().Set("Retry-After", retryAfterHeader(rl, h.cfg.RateLimitRetryAfter))
				h.writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
					"Upstream rate limited (public tier); retry after %s second(s)",
					retryAfterHeader(rl, h.cfg.RateLimitRetryAfter)))
				h.log.Warn().
					Str("path", r.URL.Path).
					Msg("Public-tier 429: not rotating egress IP (identity shared across IPs); returning 429")
				return
			}
			proxy = nil
			rateLimited = true
			lastErr = fmt.Errorf("rate limited by upstream")
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}

		h.handleModelList(w, resp)
		h.pool.ReleaseProxy(proxy)
		return
	}

	if rateLimited {
		w.Header().Set("Retry-After", h.cfg.RateLimitRetryAfter)
		h.writeError(w, http.StatusTooManyRequests, "Rate limited by upstream provider, try again later")
		h.log.Warn().Msg("Models request rate limited by upstream, all fresh IPs exhausted")
		return
	}

	if lastErr != nil && proxy != nil {
		h.pool.ReleaseProxy(proxy)
	}
	h.writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to fetch models: %v", lastErr))
}

// handleModelList relays the upstream /v1/models response, optionally keeping
// only models whose name matches the configured MODEL_FILTER substring.
func (h *Handler) handleModelList(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "Failed to read upstream response")
		return
	}

	out := body
	if h.cfg.ModelFilter != "" {
		if filtered, ok := filterModels(body, h.cfg.ModelFilter); ok {
			out = filtered
		} else {
			h.log.Debug().Str("filter", h.cfg.ModelFilter).Msg("Model filter could not be applied, relaying original list")
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

// filterModels keeps only models whose id (or name) contains the pattern
// (case-insensitive). Returns ok=false when the payload can't be parsed.
func filterModels(body []byte, pattern string) ([]byte, bool) {
	var list map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, false
	}

	data, ok := list["data"].([]any)
	if !ok {
		return nil, false
	}

	pattern = strings.ToLower(pattern)
	kept := make([]any, 0, len(data))
	for _, item := range data {
		entry, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		name, _ := entry["id"].(string)
		if name == "" {
			name, _ = entry["name"].(string)
		}
		if strings.Contains(strings.ToLower(name), pattern) {
			kept = append(kept, item)
		}
	}

	list["data"] = kept
	out, err := json.Marshal(list)
	if err != nil {
		return nil, false
	}
	return out, true
}

const (
	// maxRequestBodySize limits the maximum size of incoming request bodies
	// to prevent DoS attacks via large payloads. Default is 10MB.
	maxRequestBodySize = 10 * 1024 * 1024
)

// handleChatCompletions handles chat completion requests
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Limit request body size to prevent DoS
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			h.writeError(w, http.StatusRequestEntityTooLarge, "Request body too large")
		} else {
			h.writeError(w, http.StatusBadRequest, "Failed to read request body")
		}
		return
	}
	defer r.Body.Close()

	// Parse request to check for streaming
	var req struct {
		Stream         bool                   `json:"stream"`
		Model          string                 `json:"model"`
		Messages       []interface{}          `json:"messages"`
		ConversationID string                 `json:"conversation_id,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if len(req.Messages) == 0 {
		h.writeError(w, http.StatusBadRequest, "messages is required")
		return
	}

	// metrics records the outcome of the request
	record := func(success bool, status int) {
		if h.metrics == nil {
			return
		}
		if err := h.metrics.Record(req.Model, req.Stream, success, status, time.Since(start)); err != nil {
			h.log.Warn().Err(err).Msg("Failed to record metrics")
		}
		// Record to Prometheus exporter
		if h.prometheus != nil {
			h.prometheus.RecordRequest(success)
		}
	}

	// Upstream auth: client's Authorization header, configured key, or
	// public access via x-api-key
	auth := h.resolveAuth(r)

	// Get proxy from pool, bounded by RateLimitFreshIPWait so a fully-banned
	// pool returns 429 + Retry-After instead of hanging the client.
	ctx := r.Context()
	acquireCtx, cancel := context.WithTimeout(ctx, h.cfg.RateLimitFreshIPWait)
	proxy, err := h.pool.GetProxy(acquireCtx, req.ConversationID)
	cancel()
	if err != nil {
		w.Header().Set("Retry-After", h.cfg.RateLimitRetryAfter)
		h.writeError(w, http.StatusTooManyRequests, "Rate limited by upstream provider, try again later")
		h.log.Warn().Msg("No unbanned proxy available within wait window")
		record(false, http.StatusTooManyRequests)
		return
	}

	h.log.Debug().
		Str("proxy_id", proxy.ID).
		Str("model", req.Model).
		Bool("stream", req.Stream).
		Msg("Processing request")

	// Forward request with retry. On upstream rate limit, ban the egress IP
	// and wait (bounded by RateLimitFreshIPWait) for a fresh proxy.
	var lastErr error
	var rateLimited bool
	freshDeadline := time.Time{}
	ownProxy := proxy

	for attempt := 0; attempt <= h.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if rateLimited {
				if freshDeadline.IsZero() {
					freshDeadline = time.Now().Add(h.cfg.RateLimitFreshIPWait)
				}
				left := time.Until(freshDeadline)
				if left <= 0 {
					rateLimited = false
					lastErr = fmt.Errorf("rate limited: timed out waiting for a fresh proxy")
					break
				}
				waitCtx, cancel := context.WithTimeout(ctx, left)
				ownProxy, err = h.pool.GetProxy(waitCtx, req.ConversationID)
				cancel()
				if err != nil {
					lastErr = err
					rateLimited = false
					break
				}
				proxy = ownProxy
				rateLimited = false
				continue
			}
			delay := h.calculateRetryDelay(attempt)
			h.log.Warn().Int("attempt", attempt).Dur("delay", delay).Msg("Retrying request")
			time.Sleep(delay)
		}

		// Make request
		resp, rl, err := h.client.Do(ctx, proxy, body, auth, req.Stream)

		// Rate limit check first: Do reports rate limits with a non-nil rl
		// value (and a non-nil error too) — never treat them as transport
		// failures.
		if rl != nil {
			h.pool.MarkRateLimited(proxy, rl.RetryAfter, isPublicTier(auth))
			// Public tier is rate limited by account identity, not by egress IP:
			// spinning a fresh VPN container cannot reset it, so return an
			// honest 429+Retry-After immediately instead of churning the pool.
			if isPublicTier(auth) {
				w.Header().Set("Retry-After", retryAfterHeader(rl, h.cfg.RateLimitRetryAfter))
				h.writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
					"Upstream rate limited (public tier); retry after %s second(s)",
					retryAfterHeader(rl, h.cfg.RateLimitRetryAfter)))
				h.log.Warn().
					Str("model", req.Model).
					Bool("stream", req.Stream).
					Msg("Public-tier 429: not rotating egress IP (identity shared across IPs); returning 429")
				h.pool.ReleaseProxy(ownProxy)
				record(false, http.StatusTooManyRequests)
				return
			}
			rateLimited = true
			lastErr = fmt.Errorf("rate limited by upstream")
			// Don't release ownProxy here: the rate-limit retry path below
			// replaces it with a fresh proxy via MarkRateLimited's replacement.
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}

		// Stream or return response. Non-2xx upstream responses (e.g. auth
		// errors) are relayed as-is with their real status even when the
		// client requested streaming, so errors stay visible to the caller.
		if req.Stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			h.handleStreamResponse(w, resp, proxy)
		} else {
			h.handleNormalResponse(w, resp)
		}
		h.log.Info().
			Str("model", req.Model).
			Bool("stream", req.Stream).
			Dur("duration", time.Since(start)).
			Msg("Request completed")
		record(true, resp.StatusCode)
		h.pool.ReleaseProxy(ownProxy)
		return
	}

	h.pool.ReleaseProxy(ownProxy)

	// If the upstream is throttling us, say so honestly (429 + Retry-After)
	// instead of masking it as a 502 server error.
	if rateLimited {
		w.Header().Set("Retry-After", h.cfg.RateLimitRetryAfter)
		h.writeError(w, http.StatusTooManyRequests, "Rate limited by upstream provider, try again later")
		h.log.Warn().
			Str("model", req.Model).
			Msg("Request rate limited by upstream, all fresh IPs exhausted")
		record(false, http.StatusTooManyRequests)
		return
	}

	// Sanitize error message - don't leak internal details to clients
	h.writeError(w, http.StatusBadGateway, "Request failed: upstream error")
	h.log.Error().
		Str("model", req.Model).
		Err(lastErr).
		Msg("Request failed")
	record(false, http.StatusBadGateway)
}

// handleStreamResponse handles streaming (SSE) responses
func (h *Handler) handleStreamResponse(w http.ResponseWriter, resp *http.Response, proxy *proxy.Proxy) {
	defer resp.Body.Close()

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				h.log.Warn().Err(err).Msg("Stream read error")
			}
			break
		}

		// Write SSE line
		if _, err := fmt.Fprint(w, line); err != nil {
			h.log.Warn().Err(err).Msg("Stream write error")
			break
		}
		flusher.Flush()
	}
}

// handleNormalResponse handles non-streaming responses
func (h *Handler) handleNormalResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "Failed to read upstream response")
		return
	}

	// Copy headers
	for k, v := range resp.Header {
		if len(v) > 0 && !shouldSkipHeader(k) {
			w.Header().Set(k, v[0])
		}
	}

	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// handleHealth returns health status
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	stats := h.pool.Stats()
	healthy := stats.Total > 0 && (stats.Idle > 0 || stats.Active > 0)

	status := map[string]interface{}{
		"healthy": healthy,
		"pool":    stats,
	}

	if healthy {
		h.writeJSON(w, http.StatusOK, status)
	} else {
		h.writeJSON(w, http.StatusServiceUnavailable, status)
	}
}

// handleStats returns pool statistics
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := h.pool.Stats()
	h.writeJSON(w, http.StatusOK, stats)
}

// handleMetrics returns the pooled metrics snapshot plus proxy pool statistics
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	proxies := h.pool.List()
	snapshots := make([]proxy.ProxySnapshot, len(proxies))
	for i, p := range proxies {
		snapshots[i] = p.Snapshot()
	}

	resp := map[string]interface{}{
		"pool":    h.pool.Stats(),
		"proxies": snapshots,
	}

	if h.metrics != nil {
		snap, err := h.metrics.Snapshot()
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "Failed to load metrics")
			return
		}
		resp["metrics"] = snap
	}

	h.writeJSON(w, http.StatusOK, resp)
}

// writeJSON writes JSON response
func (h *Handler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes error response
func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "api_error",
			"code":    status,
		},
	})
}

// calculateRetryDelay calculates delay for retry with exponential backoff
func (h *Handler) calculateRetryDelay(attempt int) time.Duration {
	delay := h.cfg.RetryBaseDelay * time.Duration(1<<uint(attempt-1))
	if delay > h.cfg.RetryMaxDelay {
		delay = h.cfg.RetryMaxDelay
	}
	return delay
}

// shouldSkipHeader returns true if header should not be forwarded
func shouldSkipHeader(key string) bool {
	switch strings.ToLower(key) {
	case "content-length", "transfer-encoding", "connection":
		return true
	default:
		return false
	}
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher so SSE streaming works through the
// logging middleware (net/http's ResponseWriter otherwise hides Flush).
func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
