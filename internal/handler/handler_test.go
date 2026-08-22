package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/rs/zerolog"
)

func testLogger() *zerolog.Logger {
	log := zerolog.Nop()
	return &log
}

// fakeManager returns prebuilt proxies backed by local SOCKS5 servers. When the
// queued proxies run out it synthesizes fresh ones (reusing the SOCKS5 addr
// with unique IDs), so pool.Start and post-429 replacement goroutines stay fast
// instead of exhausting into createProxy's backoff retries.
type fakeManager struct {
	proxies []*proxy.Proxy
	addr    string
	counter int
}

func (f *fakeManager) Create(ctx context.Context) (*proxy.Proxy, error) {
	return f.CreateEx(ctx, nil, nil)
}

func (f *fakeManager) CreateEx(ctx context.Context, bannedRegions, avoidServers map[string]bool) (*proxy.Proxy, error) {
	if len(f.proxies) > 0 {
		p := f.proxies[0]
		f.proxies = f.proxies[1:]
		f.addr = p.SOCKS5Addr
		return p, nil
	}
	f.counter++
	return &proxy.Proxy{
		ID:         fmt.Sprintf("synthetic-%d", f.counter),
		SOCKS5Addr: f.addr,
		State:      proxy.StateIdle,
		Region:     "test-region",
	}, nil
}

func (f *fakeManager) Remove(ctx context.Context, id string) error { return nil }
func (f *fakeManager) HealthCheck(ctx context.Context, p *proxy.Proxy) (bool, error) {
	return true, nil
}
func (f *fakeManager) Close() error { return nil }
func (f *fakeManager) Exec(ctx context.Context, id string, env, args []string) ([]byte, error) {
	return []byte("fake-exec"), nil
}

// startSocks5 launches a minimal no-auth SOCKS5 server (RFC 1928)
func startSocks5(t testing.TB) (addr string, cancel func()) {
	ln, err := netListen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go socks5Serve(conn)
		}
	}()

	return ln.Addr().String(), func() { ln.Close() }
}

// TestResolveAuth verifies the upstream auth chain: client's Authorization
// header wins, then the configured key, then "x-api-key: public".
func TestResolveAuth(t *testing.T) {
	h := &Handler{cfg: config.DefaultConfig()}
	h.cfg.UpstreamAPIKey = "zent-configured"

	tests := []struct {
		name        string
		apiKey      string
		auth        string
		wantHeader  string
		wantValue   string
	}{
		{name: "client x-api-key wins", apiKey: "public", wantHeader: "x-api-key", wantValue: "public"},
		{name: "empty x-api-key ignored, bearer wins", apiKey: "", auth: "Bearer zent-client", wantHeader: "Authorization", wantValue: "Bearer zent-client"},
		{name: "whitespace x-api-key ignored, config used", apiKey: "   ", wantHeader: "Authorization", wantValue: "Bearer zent-configured"},
		{name: "x-api-key beats bearer", apiKey: "public", auth: "Bearer zent-client", wantHeader: "x-api-key", wantValue: "public"},
		{name: "client bearer wins", auth: "Bearer zent-client", wantHeader: "Authorization", wantValue: "Bearer zent-client"},
		{name: "empty bearer falls back to config", auth: "Bearer ", wantHeader: "Authorization", wantValue: "Bearer zent-configured"},
		{name: "no headers uses configured key", wantHeader: "Authorization", wantValue: "Bearer zent-configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			if tt.apiKey != "" {
				r.Header.Set("x-api-key", tt.apiKey)
			}
			if tt.auth != "" {
				r.Header.Set("Authorization", tt.auth)
			}
			got := h.resolveAuth(r)
			if got.Header != tt.wantHeader || got.Value != tt.wantValue {
				t.Errorf("resolveAuth() = {%s: %s}, want {%s: %s}", got.Header, got.Value, tt.wantHeader, tt.wantValue)
			}
		})
	}

	h.cfg.UpstreamAPIKey = ""
	r := httptest.NewRequest("GET", "/v1/models", nil)
	got := h.resolveAuth(r)
	if got.Header != "x-api-key" || got.Value != "public" {
		t.Errorf("resolveAuth() without config = {%s: %s}, want {x-api-key: public}", got.Header, got.Value)
	}
}

// TestAuthForEntry verifies UPSTREAM_API_KEYS entry mapping.
func TestAuthForEntry(t *testing.T) {
	tests := []struct {
		entry      string
		wantHeader string
		wantValue  string
	}{
		{entry: "public", wantHeader: "x-api-key", wantValue: "public"},
		{entry: "public-abc", wantHeader: "x-api-key", wantValue: "public-abc"},
		{entry: "zent-key-1", wantHeader: "Authorization", wantValue: "Bearer zent-key-1"},
		{entry: "x-api-key:v2-public", wantHeader: "x-api-key", wantValue: "v2-public"},
		{entry: "Authorization:Bearer custom", wantHeader: "Authorization", wantValue: "Bearer custom"},
		{entry: "x-custom-header:abc", wantHeader: "x-custom-header", wantValue: "abc"},
		{entry: "sk-anykey", wantHeader: "Authorization", wantValue: "Bearer sk-anykey"},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			got := authForEntry(tt.entry)
			if got.Header != tt.wantHeader || got.Value != tt.wantValue {
				t.Errorf("authForEntry(%q) = {%s: %s}, want {%s: %s}", tt.entry, got.Header, got.Value, tt.wantHeader, tt.wantValue)
			}
		})
	}
}

// TestResolveAuthRandomKeys verifies a random key from UPSTREAM_API_KEYS is
// picked when the client sends no Authorization header.
func TestResolveAuthRandomKeys(t *testing.T) {
	h := &Handler{cfg: config.DefaultConfig()}
	h.cfg.UpstreamAPIKey = ""
	h.cfg.UpstreamAPIKeys = []string{"public", "zent-a", "zent-b"}

	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		got := h.resolveAuth(r)
		key := got.Header + ":" + got.Value
		switch key {
		case "x-api-key:public", "Authorization:Bearer zent-a", "Authorization:Bearer zent-b":
			seen[key] = true
		default:
			t.Fatalf("resolveAuth() returned unexpected auth {%s: %s}", got.Header, got.Value)
		}
	}
	if len(seen) < 3 {
		t.Errorf("expected to observe all 3 keys over 60 draws, saw %d: %v", len(seen), seen)
	}
}

// TestHandleModelsEndToEnd
func TestHandleModelsEndToEnd(t *testing.T) {
	socksAddr, stop := startSocks5(t)
	defer stop()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer zent-test-key" {
			t.Errorf("unexpected auth %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.1","object":"model"}]}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL // no /v1 — normalization must add it
	cfg.UpstreamAPIKey = "zent-test-key"
	cfg.MaxRetries = 0
	cfg.ProxyPoolSize = 1
	cfg.ModelFilter = ""

	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())

	h := New(cfg, pool, nil, log)
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	body := make([]byte, 1024)
	n, _ := res.Body.Read(body)
	if !strings.Contains(string(body[:n]), "gpt-5.1") {
		t.Errorf("expected model gpt-5.1 in response, got %q", string(body[:n]))
	}
}

// TestStreamingChatCompletions verifies SSE streaming works through the
// logging middleware (regression for "Streaming not supported" 500s caused
// by the responseWriter wrapper hiding http.Flusher).
func TestStreamingChatCompletions(t *testing.T) {
	socksAddr, stop := startSocks5(t)
	defer stop()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("test upstream response writer not a flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`data: {"id":"1","choices":[{"delta":{"content":"hello"}}]}` + "\n\n",
			`data: {"id":"1","choices":[{"delta":{"content":" world"}}]}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			if _, err := w.Write([]byte(chunk)); err != nil {
				t.Fatalf("upstream write failed: %v", err)
			}
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL
	cfg.UpstreamAPIKey = "zent-test-key"
	cfg.MaxRetries = 0
	cfg.ProxyPoolSize = 1

	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())

	h := New(cfg, pool, nil, log)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	data, _ := io.ReadAll(res.Body)
	out := string(data)
	for _, want := range []string{`"content":"hello"`, `"content":" world"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream response missing %q, got %q", want, out)
		}
	}
}

// TestStreamErrorRelay verifies that non-2xx upstream responses are relayed
// with their real status+body even when the client requested streaming.
func TestStreamErrorRelay(t *testing.T) {
	socksAddr, stop := startSocks5(t)
	defer stop()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"type":"AuthError","message":"Invalid API key."}}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL
	cfg.UpstreamAPIKey = "zent-test-key"
	cfg.MaxRetries = 0
	cfg.ProxyPoolSize = 1

	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())

	h := New(cfg, pool, nil, log)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.StatusCode)
	}

	data, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(data), "AuthError") {
		t.Errorf("expected AuthError body from upstream, got %q", string(data))
	}
}

// TestFilterModels verifies the MODEL_FILTER filtering of the /v1/models list.
func TestFilterModels(t *testing.T) {
	raw := []byte(`{"object":"list","data":[
		{"id":"deepseek-v4-flash-free","object":"model"},
		{"id":"claude-pro","object":"model"},
		{"id":"deepseek-v4-flash","object":"model"},
		{"id":"GHOST-FREE-v1","object":"model"}
	]}`)

	out, ok := filterModels(raw, "-free")
	if !ok {
		t.Fatal("filterModels() = ok=false, want true")
	}

	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("filtered output is not valid JSON: %v", err)
	}

	want := []string{"deepseek-v4-flash-free", "GHOST-FREE-v1"}
	if len(list.Data) != len(want) {
		t.Fatalf("got %d models %v, want %d %v", len(list.Data), list.Data, len(want), want)
	}
	for i, m := range list.Data {
		if m.ID != want[i] {
			t.Errorf("model[%d] = %q, want %q", i, m.ID, want[i])
		}
	}
}

func TestFilterModelsInvalidJSON(t *testing.T) {
	if _, ok := filterModels([]byte("not json"), "-free"); ok {
		t.Error("filterModels() = ok=true on invalid JSON, want false")
	}
}

func TestFilterModelsNoData(t *testing.T) {
	if _, ok := filterModels([]byte(`{"object":"list"}`), "-free"); ok {
		t.Error("filterModels() = ok=true without data array, want false")
	}
}

// TestRateLimitedReturns429 verifies that when the upstream keeps returning
// 429, the gateway responds 429 + Retry-After (not a misleading 502).
func TestRateLimitedReturns429(t *testing.T) {
	socksAddr, stop := startSocks5(t)
	defer stop()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL
	cfg.UpstreamAPIKey = "zent-test-key"
	cfg.MaxRetries = 0
	cfg.ProxyPoolSize = 1
	cfg.RateLimitRetryAfter = "120"

	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())

	h := New(cfg, pool, nil, log)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/v1/chat/completions", "/v1/models"} {
		t.Run(path, func(t *testing.T) {
			var res *http.Response
			var err error
			if path == "/v1/chat/completions" {
				body := `{"model":"gpt-5.1","messages":[{"role":"user","content":"hi"}]}`
				res, err = http.Post(srv.URL+path, "application/json", strings.NewReader(body))
			} else {
				res, err = http.Get(srv.URL + path)
			}
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusTooManyRequests {
				raw, _ := io.ReadAll(res.Body)
				t.Fatalf("expected 429, got %d: %s", res.StatusCode, raw)
			}
			if got := res.Header.Get("Retry-After"); got != "120" {
				t.Errorf("Retry-After = %q, want 120", got)
			}
		})
	}
}

// TestPublicTierRateLimitShortCircuits verifies that when the upstream
// rate-limits the shared "public" account (x-api-key: public), the gateway
// returns 429 + Retry-After immediately and does NOT churn the proxy pool:
// no replacement container is spawned, so the pool stays at its configured
// size. Rotating egress IPs can't reset an identity-based public-tier 429.
func TestPublicTierRateLimitShortCircuits(t *testing.T) {
	socksAddr, stop := startSocks5(t)
	defer stop()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL
	cfg.UpstreamAPIKey = "" // no key -> public tier
	cfg.MaxRetries = 3      // must NOT be exercised on public-tier 429
	cfg.ProxyPoolSize = 1
	cfg.RateLimitRetryAfter = "60"

	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())

	h := New(cfg, pool, nil, log)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"model":"gpt-5.1","messages":[{"role":"user","content":"hi"}]}`
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusTooManyRequests {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("expected 429, got %d: %s", res.StatusCode, raw)
	}
	// Upstream Retry-After must be passed through (37), not the fallback (60).
	if got := res.Header.Get("Retry-After"); got != "37" {
		t.Errorf("Retry-After = %q, want 37 (upstream pass-through)", got)
	}

	// The pool self-heals in the background: the banned proxy is replaced by
	// a fresh one and the dead container pruned, so the pool never grows past
	// ProxyPoolSize and always returns to full usable capacity.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := pool.Stats()
		if stats.Total == 1 && stats.Idle == 1 && stats.Cooldown == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats := pool.Stats()
	if stats.Total != 1 {
		t.Errorf("pool grew to %d proxies after public-tier 429, want 1 (capped at ProxyPoolSize)", stats.Total)
	}
	if stats.Cooldown != 0 || stats.Idle != 1 {
		t.Errorf("after self-heal pool = idle=%d cooldown=%d, want idle=1 cooldown=0 (banned proxy replaced and pruned)", stats.Idle, stats.Cooldown)
	}
}
// Run with: ZEN_LIVE=1 go test ./internal/handler/ -run TestZenLiveModels -v
// Uses a local SOCKS5 proxy (no VPN containers required).
func TestZenLiveModels(t *testing.T) {
	if os.Getenv("ZEN_LIVE") != "1" {
		t.Skip("set ZEN_LIVE=1 to run the live Zen integration test")
	}

	for _, base := range []string{
		"https://opencode.ai/zen/v1",
		"https://opencode.ai/zen/go/v1",
	} {
		t.Run(base, func(t *testing.T) {
			socksAddr, stop := startSocks5(t)
			defer stop()

			cfg := config.DefaultConfig()
			cfg.UpstreamBaseURL = base
			cfg.MaxRetries = 1
			cfg.RequestTimeout = 10 * 1e9
			cfg.ModelFilter = ""

			log := testLogger()
			mgr := &fakeManager{proxies: []*proxy.Proxy{
				{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
			}}
			pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
			pool.Start(context.Background())

			h := New(cfg, pool, nil, log)
			srv := httptest.NewServer(h)
			defer srv.Close()

			res, err := http.Get(srv.URL + "/v1/models")
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusOK {
				body := make([]byte, 512)
				n, _ := res.Body.Read(body)
				t.Fatalf("expected 200 from %s, got %d: %s", base, res.StatusCode, string(body[:n]))
			}

			body := make([]byte, 4096)
			n, _ := res.Body.Read(body)
			if !strings.Contains(string(body[:n]), `"object":"list"`) {
				t.Errorf("unexpected models response: %q", string(body[:n]))
			}
			t.Logf("%s -> %d bytes: %s...", base, n, string(body[:80]))
		})
	}
}

// TestOpenCodeModeRelay verifies that in "opencode" upstream mode a chat
// request is proxied through a VPN container to an OpenCode Server, which
// returns a session id + an assistant message that the gateway re-emits as an
// OpenAI-shaped stream (SSE) / non-stream response.
func TestOpenCodeModeRelay(t *testing.T) {
	socksAddr, stop := startSocks5(t)
	defer stop()

	var hitSession, hitMessage bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session":
			hitSession = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sess-123"}`))
		case strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/message"):
			hitMessage = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"info":{"id":"msg-1","role":"assistant","status":"complete"},"parts":[{"type":"text","text":"hello from opencode"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamProvider = "opencode"
	cfg.OpenCodeServerURL = upstream.URL
	cfg.OpenCodeProviderID = "test"
	cfg.OpenCodeModel = "gpt-5.1-codex"
	cfg.MaxRetries = 0
	cfg.ProxyPoolSize = 1

	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())

	h := New(cfg, pool, nil, log)
	srv := httptest.NewServer(h)
	defer srv.Close()

	tests := []struct {
		name   string
		stream bool
		want   string
	}{
		{"streaming", true, "data: "},
		{"non-streaming", false, `"object":"chat.completion"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hitSession, hitMessage = false, false
			body := `{"model":"gpt-5.1-codex","stream":` + fmt.Sprintf("%v", tt.stream) + `,"messages":[{"role":"user","content":"hi"}]}`
			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", res.StatusCode)
			}
			raw, _ := io.ReadAll(res.Body)
			if !strings.Contains(string(raw), tt.want) {
				t.Errorf("missing %q in response: %q", tt.want, string(raw))
			}
			if !tt.stream && !strings.Contains(string(raw), `"content":"hello from opencode"`) {
				t.Errorf("missing assistant content in response: %q", string(raw))
			}
			if tt.stream {
				if !strings.Contains(res.Header.Get("Content-Type"), "event-stream") {
					t.Errorf("stream should be text/event-stream, got %q", res.Header.Get("Content-Type"))
				}
			}
		})
	}

	if !hitSession {
		t.Error("OpenCode Server was never hit on POST /session — proxy tunnel unused")
	}
	if !hitMessage {
		t.Error("OpenCode Server was never hit on POST /session/:id/message — proxy tunnel unused")
	}
}
