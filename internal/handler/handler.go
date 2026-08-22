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
	cfg       *config.Config
	pool      *proxy.PoolManager
	client    upstream.Upstream
	metrics   *metrics.Store
	prometheus *metrics.PrometheusExporter
	pricing   *metrics.PricingTable
	log       *zerolog.Logger
	mux       *http.ServeMux
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
	pricing := metrics.NewPricingTable(cfg.ModelPricing)

	h := &Handler{
		cfg:       cfg,
		pool:      pool,
		client:    client,
		metrics:   metricsStore,
		prometheus: prometheus,
		pricing:   pricing,
		log:       log,
		mux:       http.NewServeMux(),
	}

	// Register routes
	h.mux.HandleFunc("GET /v1/models", h.handleModels)
	h.mux.HandleFunc("GET /v1/models/{id}", h.handleModelRetrieve)
	h.mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	h.mux.HandleFunc("GET /health", h.handleHealth)
	h.mux.HandleFunc("GET /stats", h.handleStats)
	h.mux.HandleFunc("GET /api/metrics", h.handleMetrics)
	h.mux.HandleFunc("GET /metrics", prometheus.Handler())

	// JSON fallback for unknown /v1/* paths and wrong methods — Go's ServeMux
	// would otherwise answer with a plain-text 404/405, which breaks OpenAI
	// SDK clients that expect the standard JSON error envelope.
	h.mux.HandleFunc("/v1/", h.handleV1Fallback)

	// Serve the web UI
	h.mux.Handle("/", web.Handler())

	// Middleware (outermost first): gateway auth sits inside CORS so 401s
	// carry CORS headers for browser clients.
	return h.loggingMiddleware(h.requestIDMiddleware(h.corsMiddleware(h.gatewayAuthMiddleware(h.mux))))
}

// handleV1Fallback answers unmatched /v1/* requests with OpenAI-style JSON
// errors: 405 when the path exists but the method is wrong, 404 otherwise.
func (h *Handler) handleV1Fallback(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	known := path == "/v1/models" ||
		strings.HasPrefix(path, "/v1/models/") ||
		path == "/v1/chat/completions"
	if known {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		h.writeOpenAIError(w, http.StatusMethodNotAllowed,
			fmt.Sprintf("Method %s not allowed for %s", r.Method, path),
			"", "method_not_allowed")
		return
	}
	h.writeOpenAIError(w, http.StatusNotFound,
		fmt.Sprintf("Invalid URL (%s %s)", r.Method, path),
		"", "unknown_url")
}

// gatewayAuthMiddleware enforces the configured gateway API keys on all
// /v1/* endpoints. When no keys are configured the API stays open. Keys are
// compared as exact Bearer token matches.
func (h *Handler) gatewayAuthMiddleware(next http.Handler) http.Handler {
	if len(h.cfg.GatewayAPIKeys) == 0 {
		return next
	}
	keys := make(map[string]bool, len(h.cfg.GatewayAPIKeys))
	for _, k := range h.cfg.GatewayAPIKeys {
		keys[k] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if key == "" || !keys[key] {
				h.writeOpenAIError(w, http.StatusUnauthorized,
					"Incorrect API key provided. You can find your API key in the gateway configuration.",
					"", "invalid_api_key")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
		w.Header().Set("Retry-After", h.cfg.RateLimitRetryAfter)
		h.writeError(w, http.StatusTooManyRequests, "Rate limited by upstream provider, try again later")
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

// handleModelRetrieve implements GET /v1/models/{id}: fetches the upstream
// model list and returns the matching entry, or a 404 with code
// "model_not_found" when absent.
func (h *Handler) handleModelRetrieve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	auth := h.resolveAuth(r)

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.RateLimitFreshIPWait)
	defer cancel()
	proxy, err := h.pool.GetProxy(ctx, "")
	if err != nil {
		w.Header().Set("Retry-After", h.cfg.RateLimitRetryAfter)
		h.writeError(w, http.StatusTooManyRequests, "Rate limited by upstream provider, try again later")
		h.log.Warn().Str("model", id).Msg("No unbanned proxy available for model retrieve")
		return
	}

	resp, rl, err := h.client.DoModels(r.Context(), proxy, auth)
	if rl != nil {
		h.pool.MarkRateLimited(proxy, rl.RetryAfter, isPublicTier(auth))
	}
	if err != nil || rl != nil {
		if proxy != nil {
			h.pool.ReleaseProxy(proxy)
		}
		w.Header().Set("Retry-After", retryAfterHeader(rl, h.cfg.RateLimitRetryAfter))
		h.writeError(w, http.StatusTooManyRequests, "Rate limited by upstream provider, try again later")
		return
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	h.pool.ReleaseProxy(proxy)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "Failed to read upstream response")
		return
	}

	var list struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(body, &list) == nil {
		for _, m := range list.Data {
			if mid, _ := m["id"].(string); mid == id {
				h.writeJSON(w, http.StatusOK, m)
				return
			}
		}
	}
	h.writeOpenAIError(w, http.StatusNotFound,
		fmt.Sprintf("The model %q does not exist", id),
		"", "model_not_found")
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

// chatMessage is one entry of the messages array.
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// validChatRoles are the roles accepted by the OpenAI chat completions API.
var validChatRoles = map[string]bool{
	"system": true, "user": true, "assistant": true,
	"tool": true, "function": true, "developer": true,
}

// chatRequest mirrors the OpenAI chat completion request fields the gateway
// needs to inspect; everything else passes through to the upstream verbatim.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	// ConversationID is a gateway extension for sticky sessions.
	ConversationID string `json:"conversation_id,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// validateChatRequest checks OpenAI-standard required fields. It returns the
// offending field path ("" when valid) and a human-readable message.
func validateChatRequest(req *chatRequest) (param string, message string) {
	if strings.TrimSpace(req.Model) == "" {
		return "model", "you must provide a model parameter"
	}
	if len(req.Messages) == 0 {
		return "messages", "'messages' must contain at least one message"
	}
	for i, m := range req.Messages {
		field := fmt.Sprintf("messages[%d]", i)
		if !validChatRoles[m.Role] {
			return field + ".role",
				fmt.Sprintf("invalid role %q; expected one of system, user, assistant, tool, function, developer", m.Role)
		}
		// Assistant messages may legitimately carry null content (tool calls).
		if len(m.Content) == 0 || string(m.Content) == "null" {
			if m.Role != "assistant" && m.Role != "tool" {
				return field + ".content", "message content is required for role " + m.Role
			}
		}
	}
	return "", ""
}

// handleChatCompletions handles chat completion requests
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Limit request body size to prevent DoS
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	// Enforce JSON bodies like the OpenAI API does (415 otherwise). Requests
	// without an explicit Content-Type are tolerated.
	if ct := r.Header.Get("Content-Type"); ct != "" &&
		!strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
		h.writeOpenAIError(w, http.StatusUnsupportedMediaType,
			"Expected \"Content-Type: application/json\" but got "+ct,
			"", "")
		return
	}

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

	// Parse and validate the request
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeOpenAIError(w, http.StatusBadRequest,
			"We could not parse the JSON body of your request.",
			"", "invalid_json")
		return
	}
	if param, msg := validateChatRequest(&req); param != "" {
		h.writeErrorParam(w, http.StatusBadRequest, msg, param)
		return
	}
	includeUsage := req.Stream && req.StreamOptions != nil && req.StreamOptions.IncludeUsage

	// metrics records the outcome of the request
	record := func(success bool, status int, usage upstream.Usage) {
		if h.metrics == nil {
			return
		}
		cost := h.pricing.EstimateCostFromUpstream(req.Model, usage.PromptTokens, usage.CompletionTokens, usage.CachedTokens)
		mUsage := metrics.Usage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			CachedTokens:     usage.CachedTokens,
		}
		if err := h.metrics.Record(req.Model, req.Stream, success, status, time.Since(start), mUsage, cost); err != nil {
			h.log.Warn().Err(err).Msg("Failed to record metrics")
		}
		// Record to Prometheus exporter
		if h.prometheus != nil {
			h.prometheus.RecordRequest(success, mUsage, cost)
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
		record(false, http.StatusTooManyRequests, upstream.Usage{})
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
				record(false, http.StatusTooManyRequests, upstream.Usage{})
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
		var usage upstream.Usage
		if req.Stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			usage = h.handleStreamResponse(w, resp, req.Model, includeUsage)
		} else {
			usage = h.handleNormalResponse(w, resp)
		}
		h.log.Info().
			Str("model", req.Model).
			Bool("stream", req.Stream).
			Dur("duration", time.Since(start)).
			Int("prompt_tokens", usage.PromptTokens).
			Int("completion_tokens", usage.CompletionTokens).
			Msg("Request completed")
		record(true, resp.StatusCode, usage)
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
		record(false, http.StatusTooManyRequests, upstream.Usage{})
		return
	}

	// Sanitize error message - don't leak internal details to clients
	h.writeError(w, http.StatusBadGateway, "Request failed: upstream error")
	h.log.Error().
		Str("model", req.Model).
		Err(lastErr).
		Msg("Request failed")
	record(false, http.StatusBadGateway, upstream.Usage{})
}

// handleStreamResponse handles streaming (SSE) responses. It relays upstream
// chunks verbatim (including reasoning_content deltas) and adds OpenAI
// streaming compliance:
//   - X-Accel-Buffering: no so reverse proxies don't buffer the stream
//   - a synthesized final usage chunk when the client asked for
//     stream_options.include_usage but the upstream never sent one
//   - an SSE error event if the upstream connection breaks mid-stream,
//     instead of ending silently with no [DONE]
func (h *Handler) handleStreamResponse(w http.ResponseWriter, resp *http.Response, model string, includeUsage bool) upstream.Usage {
	defer resp.Body.Close()

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Transfer-Encoding")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "Streaming not supported")
		return upstream.Usage{}
	}

	var usage upstream.Usage
	sawUsageChunk := false
	interrupted := false

	writeSSEError := func(message string) {
		payload, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": message,
				"type":    "server_error",
				"param":   nil,
				"code":    "stream_interrupted",
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				h.log.Warn().Err(err).Msg("Stream read error")
				interrupted = true
			} else if line != "" {
				// Final unterminated line; fall through to write it below.
			}
			if line == "" {
				break
			}
		}

		// Parse SSE data lines for usage
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if data == "[DONE]" {
				// Synthesize the OpenAI-standard terminal usage chunk when the
				// client opted in via stream_options.include_usage but the
				// upstream never sent one.
				if includeUsage && !sawUsageChunk && usage.TotalTokens > 0 {
					chunk := map[string]interface{}{
						"id":      "chatcmpl-" + uuid.NewString(),
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []interface{}{},
						"usage":   usage,
					}
					if b, jerr := json.Marshal(chunk); jerr == nil {
						fmt.Fprintf(w, "data: %s\n\n", b)
					}
				}
				if _, err := fmt.Fprint(w, line); err != nil {
					h.log.Warn().Err(err).Msg("Stream write error")
					break
				}
				flusher.Flush()
				continue
			}
			var chunk struct {
				Usage *upstream.Usage `json:"usage,omitempty"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err == nil && chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
				usage = *chunk.Usage
				sawUsageChunk = true
			}
		}

		// Write SSE line
		if _, err := fmt.Fprint(w, line); err != nil {
			h.log.Warn().Err(err).Msg("Stream write error")
			return usage
		}
		flusher.Flush()
	}

	// Upstream broke before [DONE]: tell the client in-band so SDKs don't
	// hang or silently treat a truncated answer as complete.
	if interrupted {
		writeSSEError("The upstream connection was interrupted before the response completed.")
	}
	return usage
}

// handleNormalResponse handles non-streaming responses
func (h *Handler) handleNormalResponse(w http.ResponseWriter, resp *http.Response) upstream.Usage {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "Failed to read upstream response")
		return upstream.Usage{}
	}

	// Parse JSON to extract usage
	var result struct {
		Usage *upstream.Usage `json:"usage,omitempty"`
	}
	json.Unmarshal(body, &result)

	// Copy headers
	for k, v := range resp.Header {
		if len(v) > 0 && !shouldSkipHeader(k) {
			w.Header().Set(k, v[0])
		}
	}

	w.WriteHeader(resp.StatusCode)
	w.Write(body)

	if result.Usage != nil {
		return *result.Usage
	}
	return upstream.Usage{}
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

// openaiErrorType maps an HTTP status to the OpenAI-standard error type.
func openaiErrorType(status int) string {
	switch status {
	case http.StatusBadRequest,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		// Matches the OpenAI API: unknown URLs and missing models are
		// reported as invalid_request_error (with a code such as
		// "model_not_found"), not as a distinct not_found type.
		return "invalid_request_error"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "service_unavailable_error"
	default:
		return "server_error"
	}
}

// writeOpenAIError writes the OpenAI-standard error envelope:
//
//	{"error": {"message": "...", "type": "...", "param": "...|null", "code": "...|null"}}
//
// param names the offending request field ("" → null); code is a stable
// machine-readable string ("" → null), e.g. "invalid_api_key".
func (h *Handler) writeOpenAIError(w http.ResponseWriter, status int, message string, param string, code string) {
	err := map[string]interface{}{
		"message": message,
		"type":    openaiErrorType(status),
		"param":   nil,
		"code":    nil,
	}
	if param != "" {
		err["param"] = param
	}
	if code != "" {
		err["code"] = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"error": err})
}

// writeError writes an OpenAI-standard error with no param/code attribution.
func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	h.writeOpenAIError(w, status, message, "", "")
}

// writeErrorParam writes an OpenAI-standard invalid_request_error attributing
// the failure to a specific request field.
func (h *Handler) writeErrorParam(w http.ResponseWriter, status int, message, param string) {
	h.writeOpenAIError(w, status, message, param, "")
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
