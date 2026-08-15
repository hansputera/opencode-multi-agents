package upstream

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	proxypkg "github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/rs/zerolog"
)

// startSocks5Server starts a minimal no-auth SOCKS5 proxy (RFC 1928)
func startSocks5Server(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks5(conn)
		}
	}()

	return ln.Addr().String()
}

func handleSocks5(conn net.Conn) {
	defer conn.Close()

	// Greeting: check no-auth
	greet := make([]byte, 2)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return
	}
	methods := make([]byte, int(greet[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	// No-auth selected
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// CONNECT request
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	if hdr[1] != 0x01 { // CONNECT
		return
	}
	var host string
	switch hdr[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 0x03: // domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}
		dom := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(conn, dom); err != nil {
			return
		}
		host = string(dom)
	default:
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf)

	target, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		return
	}
	defer target.Close()

	// Reply success
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
	}
	go pipe(target, conn)
	go pipe(conn, target)
	wg.Wait()
}

func testLogger() *zerolog.Logger {
	log := zerolog.Nop()
	return &log
}

func TestNormalizeBase(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://opencode.ai/zen/v1", "https://opencode.ai/zen/v1"},
		{"https://opencode.ai/zen/v1/", "https://opencode.ai/zen/v1"},
		{"https://opencode.ai/zen/go/v1", "https://opencode.ai/zen/go/v1"},
		{"https://openrouter.ai/api", "https://openrouter.ai/api/v1"},
		{"https://openrouter.ai/api/", "https://openrouter.ai/api/v1"},
	}
	for _, c := range cases {
		if got := normalizeBase(c.in); got != c.want {
			t.Errorf("normalizeBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDoModelsThroughProxy(t *testing.T) {
	socksAddr := startSocks5Server(t)

	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4-flash"}]}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL + "/zen/v1" // ensure /v1 not double-appended
	cfg.RequestTimeout = 5 * time.Second

	c := NewClient(cfg, testLogger())
	p := &proxypkg.Proxy{
		ID:         "test-proxy",
		SOCKS5Addr: "socks5://" + socksAddr,
	}

	resp, rateLimited, err := c.DoModels(context.Background(), p, Auth{Header: "Authorization", Value: "Bearer zent-test-key"})
	if err != nil {
		t.Fatalf("DoModels failed: %v", err)
	}
	defer resp.Body.Close()

	if rateLimited != nil {
		t.Fatal("unexpected rate limit")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/zen/v1/models" {
		t.Errorf("expected path /zen/v1/models, got %q", gotPath)
	}
	if gotAuth != "Bearer zent-test-key" {
		t.Errorf("expected auth header, got %q", gotAuth)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("expected response body")
	}
}

func TestNormalizeBaseUsedByDo(t *testing.T) {
	socksAddr := startSocks5Server(t)

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.UpstreamBaseURL = upstream.URL // no /v1 in base
	cfg.RequestTimeout = 5 * time.Second

	c := NewClient(cfg, testLogger())
	p := &proxypkg.Proxy{
		ID:         "test-proxy-2",
		SOCKS5Addr: "socks5://" + socksAddr,
	}

	resp, _, err := c.Do(context.Background(), p, []byte(`{"model":"x"}`), Auth{Header: "x-api-key", Value: "public"}, false)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/v1/chat/completions" {
		t.Errorf("expected /v1/chat/completions, got %q", gotPath)
	}
}