package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/hansputera/opencode-multi-agents/internal/upstream"
	"github.com/rs/zerolog"
)

// Handler handles HTTP requests for the OpenAI-compatible API
type Handler struct {
	cfg      *config.Config
	pool     *proxy.PoolManager
	client   *upstream.Client
	log      *zerolog.Logger
	mux      *http.ServeMux
}

// New creates a new HTTP handler
func New(cfg *config.Config, pool *proxy.PoolManager, log *zerolog.Logger) http.Handler {
	h := &Handler{
		cfg:    cfg,
		pool:   pool,
		client: upstream.NewClient(cfg, log),
		log:    log,
		mux:    http.NewServeMux(),
	}

	// Register routes
	h.mux.HandleFunc("GET /v1/models", h.handleModels)
	h.mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	h.mux.HandleFunc("GET /health", h.handleHealth)
	h.mux.HandleFunc("GET /stats", h.handleStats)

	// Middleware
	return h.loggingMiddleware(h.corsMiddleware(h.mux))
}

// loggingMiddleware logs all requests
func (h *Handler) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create response wrapper for status code capture
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rw, r)

		duration := time.Since(start)
		h.log.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rw.statusCode).
			Dur("duration", duration).
			Msg("Request")
	})
}

// corsMiddleware adds CORS headers
func (h *Handler) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleModels returns available models
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// For now, return a static list of free models
	// In production, this could proxy to upstream /v1/models
	models := map[string]interface{}{
		"object": "list",
		"data": []map[string]interface{}{
			{
				"id":       "openrouter/auto",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "openrouter",
			},
			{
				"id":       "meta-llama/llama-3.1-8b-instruct:free",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "meta",
			},
			{
				"id":       "google/gemma-2-9b-it:free",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "google",
			},
			{
				"id":       "mistralai/mistral-7b-instruct:free",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "mistralai",
			},
		},
	}

	h.writeJSON(w, http.StatusOK, models)
}

// handleChatCompletions handles chat completion requests
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}
	r.Body.Close()

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

	// Get API key from Authorization header
	authHeader := r.Header.Get("Authorization")
	apiKey := strings.TrimPrefix(authHeader, "Bearer ")
	if apiKey == "" {
		apiKey = h.cfg.UpstreamAPIKey
	}

	// Get proxy from pool
	ctx := r.Context()
	proxy, err := h.pool.GetProxy(ctx, req.ConversationID)
	if err != nil {
		h.writeError(w, http.StatusServiceUnavailable, "No proxy available")
		return
	}
	defer h.pool.ReleaseProxy(proxy)

	h.log.Debug().
		Str("proxy_id", proxy.ID).
		Str("model", req.Model).
		Bool("stream", req.Stream).
		Msg("Processing request")

	// Forward request with retry
	var lastErr error
	for attempt := 0; attempt <= h.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			// Wait before retry
			delay := h.calculateRetryDelay(attempt)
			h.log.Warn().
				Int("attempt", attempt).
				Dur("delay", delay).
				Msg("Retrying request")

			time.Sleep(delay)

			// Get new proxy for retry
			h.pool.MarkRateLimited(proxy)
			proxy, err = h.pool.GetProxy(ctx, req.ConversationID)
			if err != nil {
				lastErr = err
				continue
			}
		}

		// Make request
		resp, rateLimited, err := h.client.Do(ctx, proxy, body, apiKey, req.Stream)
		if err != nil {
			lastErr = err
			continue
		}

		// Check for rate limit
		if rateLimited {
			lastErr = fmt.Errorf("rate limited")
			continue
		}

		// Stream or return response
		if req.Stream {
			h.handleStreamResponse(w, resp, proxy)
		} else {
			h.handleNormalResponse(w, resp)
		}
		return
	}

	// All retries failed
	h.writeError(w, http.StatusBadGateway, fmt.Sprintf("Request failed: %v", lastErr))
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
				h.log.Debug().Err(err).Msg("Stream read error")
			}
			break
		}

		// Write SSE line
		if _, err := fmt.Fprint(w, line); err != nil {
			h.log.Debug().Err(err).Msg("Stream write error")
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
