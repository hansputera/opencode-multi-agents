package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
)

// --- Accounts ---

func (h *Handler) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.cfgStore.GetAccounts()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to list accounts")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]interface{}{"accounts": accounts})
}

func (h *Handler) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		SessionCookies string `json:"session_cookies"`
		Enabled        *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		h.writeErrorParam(w, http.StatusBadRequest, "username is required", "username")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Generate store path based on next ID
	accounts, _ := h.cfgStore.GetAccounts()
	storePath := fmt.Sprintf("data/protonvpn_%d.db", len(accounts)+1)
	if len(accounts) == 0 {
		storePath = h.cfg.ProtonVPNStorePath
	}

	acct := &config.Account{
		Username:       strings.TrimSpace(req.Username),
		Password:       req.Password,
		StorePath:      storePath,
		SessionCookies: req.SessionCookies,
		Enabled:        enabled,
	}
	if err := h.cfgStore.CreateAccount(acct); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			h.writeErrorParam(w, http.StatusConflict, "account already exists", "username")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to create account")
		return
	}

	h.writeJSON(w, http.StatusCreated, acct)
}

func (h *Handler) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid account ID")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		SessionCookies string `json:"session_cookies"`
		Enabled        *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	acct, err := h.cfgStore.GetAccount(id)
	if err != nil {
		h.writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if req.Username != "" {
		acct.Username = strings.TrimSpace(req.Username)
	}
	if req.Password != "" {
		acct.Password = req.Password
	}
	if req.SessionCookies != "" {
		acct.SessionCookies = req.SessionCookies
	}
	if req.Enabled != nil {
		acct.Enabled = *req.Enabled
	}
	if err := h.cfgStore.UpdateAccount(acct); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to update account")
		return
	}
	h.writeJSON(w, http.StatusOK, acct)
}

func (h *Handler) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid account ID")
		return
	}
	if err := h.cfgStore.DeleteAccount(id); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to delete account")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Proxies ---

func (h *Handler) handleListProxies(w http.ResponseWriter, r *http.Request) {
	proxies, err := h.cfgStore.GetProxies()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to list proxies")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]interface{}{"proxies": proxies})
}

func (h *Handler) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Address) == "" {
		h.writeErrorParam(w, http.StatusBadRequest, "address is required", "address")
		return
	}

	proxyCfg := &config.ProxyConfig{
		Address: strings.TrimSpace(req.Address),
		Enabled: true,
	}
	if err := h.cfgStore.CreateProxy(proxyCfg); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			h.writeErrorParam(w, http.StatusConflict, "proxy already exists", "address")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to create proxy")
		return
	}

	// Add to live pool
	ctx := r.Context()
	if _, err := h.pool.AddExternalProxy(ctx, proxyCfg.Address); err != nil {
		h.log.Warn().Err(err).Str("addr", proxyCfg.Address).Msg("Proxy saved but failed to add to live pool")
	}

	h.writeJSON(w, http.StatusCreated, proxyCfg)
}

func (h *Handler) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid proxy ID")
		return
	}
	if err := h.cfgStore.DeleteProxy(id); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to delete proxy")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Settings ---

func (h *Handler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.cfgStore.GetSettings()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to get settings")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]interface{}{"settings": settings})
}

func (h *Handler) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req map[string]string
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if err := h.cfgStore.SetSettings(req); err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to update settings")
		return
	}

	// Apply pool size change immediately
	if ps, ok := req["pool_size"]; ok {
		if n, err := strconv.Atoi(ps); err == nil && n >= 1 && n <= 20 {
			h.pool.SetPoolSize(n)
		}
	}

	// Apply hot-reloadable settings to the live config pointer
	if v, ok := req["upstream_base_url"]; ok && v != "" {
		h.cfg.UpstreamBaseURL = v
	}
	if v, ok := req["upstream_provider"]; ok && v != "" {
		h.cfg.UpstreamProvider = v
	}
	if v, ok := req["model_filter"]; ok {
		h.cfg.ModelFilter = v
	}
	if v, ok := req["log_level"]; ok && v != "" {
		h.cfg.LogLevel = v
	}
	if v, ok := req["log_format"]; ok && v != "" {
		h.cfg.LogFormat = v
	}
	if v, ok := req["request_timeout"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			h.cfg.RequestTimeout = d
		}
	}
	if v, ok := req["max_retries"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 10 {
			h.cfg.MaxRetries = n
		}
	}
	if v, ok := req["max_concurrent"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			h.cfg.MaxConcurrent = n
		}
	}
	if v, ok := req["cooldown_duration"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			h.cfg.CooldownDuration = d
		}
	}
	if v, ok := req["ip_ban_duration"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			h.cfg.IPBanDuration = d
		}
	}
	if v, ok := req["rate_limit_fresh_ip_wait"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			h.cfg.RateLimitFreshIPWait = d
		}
	}
	if v, ok := req["rate_limit_retry_after"]; ok && v != "" {
		h.cfg.RateLimitRetryAfter = v
	}
	if v, ok := req["health_check_period"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			h.cfg.HealthCheckPeriod = d
		}
	}
	if v, ok := req["sticky_session_ttl"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			h.cfg.StickySessionTTL = d
		}
	}
	if v, ok := req["regions"]; ok {
		h.cfg.ProtonVPNRegions = v
	}
	if v, ok := req["ip_check_url"]; ok {
		h.cfg.ProtonVPNIPCheckURL = v
	}
	if v, ok := req["pow_enabled"]; ok {
		h.cfg.PowEnabled = v == "true"
	}
	if v, ok := req["web_search_enabled"]; ok {
		h.cfg.WebSearchEnabled = v == "true"
	}
	if v, ok := req["web_search_max_results"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			h.cfg.WebSearchMaxResults = n
		}
	}
	if v, ok := req["web_search_max_rounds"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			h.cfg.WebSearchMaxRounds = n
		}
	}

	settings, _ := h.cfgStore.GetSettings()
	h.writeJSON(w, http.StatusOK, map[string]interface{}{"settings": settings})
}

// --- Pool Status ---

func (h *Handler) handleGetPool(w http.ResponseWriter, r *http.Request) {
	stats := h.pool.Stats()
	proxies := h.pool.List()
	snapshots := make([]interface{}, len(proxies))
	for i, p := range proxies {
		snapshots[i] = p.Snapshot()
	}
	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"stats":   stats,
		"proxies": snapshots,
		"time":    time.Now().Format(time.RFC3339),
	})
}

// --- helpers ---

func parseID(r *http.Request, param string) (int64, error) {
	idStr := r.PathValue(param)
	if idStr == "" {
		return 0, fmt.Errorf("missing %s", param)
	}
	return strconv.ParseInt(idStr, 10, 64)
}
