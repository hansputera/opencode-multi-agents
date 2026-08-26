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
