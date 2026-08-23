package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/metrics"
	"github.com/hansputera/opencode-multi-agents/internal/proxy"
)

func TestNormalizeRoute(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": "/v1/chat/completions",
		"/v1/models":           "/v1/models",
		"/v1/models/gpt-5.1":   "/v1/models/{id}",
		"/api/pow/challenge":   "/api/pow/challenge",
		"/api/pow/redeem":      "/api/pow/redeem",
		"/stats":               "/stats",
		"/":                    "/",
		"/dashboard.js":        "/ui/{file}",
	}
	for in, want := range cases {
		if got := normalizeRoute("GET", in); got != want {
			t.Errorf("normalizeRoute(%q) = %q, want %q", in, got, want)
		}
	}
	// Dynamic segments collapse.
	if got := normalizeRoute("GET", "/some/path/01JZXQ8K7M6P2ABCD"); !strings.HasSuffix(got, "/{id}") {
		t.Errorf("dynamic segment not collapsed: %q", got)
	}
}

func TestResponseWriterCountsBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	rw.WriteHeader(http.StatusOK)
	rw.Write([]byte("hello "))
	rw.Write([]byte("world"))
	if rw.bytesWritten != 11 {
		t.Errorf("bytesWritten = %d, want 11", rw.bytesWritten)
	}
	if rw.statusCode != http.StatusOK {
		t.Errorf("statusCode = %d", rw.statusCode)
	}
}

// TestTrafficRecordedAcrossEndpoints drives several real routes and verifies
// /api/metrics reports server-wide usage for each of them (excluding the
// untracked infra paths).
func TestTrafficRecordedAcrossEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model"}]}`))
	}))
	defer upstream.Close()

	socksAddr, stop := startSocks5(t)
	t.Cleanup(stop)

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL
	cfg.UpstreamAPIKey = "zent-test"
	cfg.MaxRetries = 0
	log := testLogger()
	mgr := &fakeManager{proxies: []*proxy.Proxy{
		{ID: "p1", SOCKS5Addr: "socks5://" + socksAddr, State: proxy.StateIdle},
	}}
	pool := proxy.NewPoolManagerWithManager(mgr, cfg, log)
	pool.Start(context.Background())
	store, err := metrics.New(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := newHandler(cfg, pool, store, log)
	srv := httptest.NewServer(h.loggingMiddleware(h.requestIDMiddleware(h.corsMiddleware(h.gatewayAuthMiddleware(h.mux)))))
	defer srv.Close()

	// Drive a few routes.
	getJSON(t, srv.URL+"/v1/models")                  // 200 via stub upstream
	getJSON(t, srv.URL+"/v1/models/nope")             // 404
	postJSON(t, srv.URL+"/v1/chat/completions", `{}`) // 400 validation error
	getJSON(t, srv.URL+"/health")                     // excluded from tracking

	code, m := getJSON(t, srv.URL+"/api/metrics")
	if code != 200 {
		t.Fatalf("metrics status %d", code)
	}
	server, _ := m["server"].(map[string]any)
	if server == nil {
		t.Fatalf("missing server section: %v", m)
	}
	total, _ := server["total_requests"].(float64)
	if total < 3 {
		t.Errorf("server totals.requests = %v, want >=3", total)
	}
	endpoints, _ := server["endpoints"].([]any)
	if len(endpoints) == 0 {
		t.Fatal("no endpoint rows returned")
	}

	// Untracked paths must not appear.
	for _, e := range endpoints {
		em := e.(map[string]any)
		route, _ := em["route"].(string)
		switch route {
		case "/health", "/metrics":
			t.Errorf("infra path %s should not be tracked", route)
		case "/api/metrics":
			t.Error("/api/metrics self-polling should not be tracked")
		}
	}

	// The system specs section exists with sane basics.
	sys := m["system"].(map[string]any)
	if sys == nil || sys["num_cpu"] == nil || sys["go_version"] == "" {
		t.Errorf("system section incomplete: %v", sys)
	}
	if sys["num_cpu"].(float64) < 1 {
		t.Error("num_cpu should be >= 1")
	}
}
