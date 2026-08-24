package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/proxy"
)

// retryHarness builds a gateway with the given MaxRetries against a stub
// upstream that fails the first `failures` calls with a 503 provider error.
func retryHarness(t *testing.T, failures int32) (srvURL string, calls *int32) {
	t.Helper()
	calls = new(int32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(calls, 1) <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"type":"server_error","message":"Error from provider (Console): Upstream request failed: Endpoint is unavailable."}`))
			return
		}
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"recovered"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	socksAddr, stop := startSocks5(t)
	t.Cleanup(stop)
	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL
	cfg.UpstreamAPIKey = "zent-test"
	cfg.MaxRetries = 3
	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())

	srv := httptest.NewServer(New(cfg, pool, nil, log))
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

func chatPost(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Post(url+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// TestChatRetriesTransientUpstream5xx verifies a provider "Endpoint is
// unavailable" (503) is retried and succeeds once the upstream recovers.
func TestChatRetriesTransientUpstream5xx(t *testing.T) {
	url, calls := retryHarness(t, 2)

	code, body := chatPost(t, url)
	if code != http.StatusOK {
		t.Fatalf("expected eventual 200, got %d: %s", code, body)
	}
	if !strings.Contains(body, "recovered") {
		t.Errorf("expected successful content, got %s", body)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("upstream calls = %d, want 3 (two 503s then success)", got)
	}
}

// TestChatRelaysProviderErrorAfterRetriesExhausted verifies the provider's
// own error text and status are relayed verbatim once retries run out —
// "Endpoint is unavailable" is far more actionable than a generic 502.
func TestChatRelaysProviderErrorAfterRetriesExhausted(t *testing.T) {
	url, calls := retryHarness(t, 99) // always failing

	code, body := chatPost(t, url)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected relayed 503, got %d", code)
	}
	if !strings.Contains(body, "Endpoint is unavailable") {
		t.Errorf("provider error text should be relayed verbatim, got %s", body)
	}

	// Initial attempt + MaxRetries(3) = 4 upstream calls.
	if got := atomic.LoadInt32(calls); got != 4 {
		t.Errorf("upstream calls = %d, want 4", got)
	}

	var m map[string]any
	json.Unmarshal([]byte(body), &m)
	if e, ok := m["error"].(map[string]any); ok && e["type"] != "server_error" {
		t.Errorf("relayed error type = %v", e["type"])
	}
}

// TestModelsRetriesTransientUpstream5xx covers the models endpoint too.
func TestModelsRetriesTransientUpstream5xx(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.1","object":"model"}]}`))
	}))
	defer upstream.Close()

	socksAddr, stop := startSocks5(t)
	t.Cleanup(stop)
	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL
	cfg.UpstreamAPIKey = "zent-test"
	cfg.MaxRetries = 2
	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())
	srv := httptest.NewServer(New(cfg, pool, nil, log))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d: %s", res.StatusCode, raw)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2", got)
	}
}
