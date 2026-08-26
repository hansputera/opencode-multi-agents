package handler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/pow"
)

// powHarness builds a gateway with the PoW gate enabled, wired to a stub
// upstream, using a temp SQLite store.
func powHarness(t *testing.T, cfgMut ...func(*config.Config)) (http.Handler, *config.Config) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.1","object":"model"}]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := config.DefaultConfig()
	cfg.PowEnabled = true
	cfg.PowStorePath = t.TempDir() + "/pow.db"
	cfg.PowBaseDifficulty = 8
	cfg.PowMinDifficulty = 1 // let tests solve instantly
	for _, f := range cfgMut {
		f(cfg)
	}
	return newTestGateway(t, upstream.URL, func(c *config.Config) { *c = *cfg }), cfg
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return res.StatusCode, m
}

func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return res.StatusCode, m
}

// solveChallenge finds a valid counter for the challenge (test-speed).
func solveChallenge(t *testing.T, ch map[string]any) string {
	t.Helper()
	difficulty := int(ch["difficulty"].(float64))
	prefix := fmt.Sprintf("pow-v%d|%s|%s|%s|%d|%s|%s|",
		int(ch["version"].(float64)),
		ch["id"].(string), ch["resource"].(string), ch["algo"].(string),
		difficulty, ch["salt"].(string), ch["bind"].(string))
	for i := 0; i < 10_000_000; i++ {
		counter := fmt.Sprintf("%010d", i)
		d := sha256.Sum256([]byte(prefix + counter))
		if pow.LeadingZeroBits(d) >= difficulty {
			return counter
		}
	}
	t.Fatal("no solution found")
	return ""
}

func TestPoWEndToEndIssueAndUse(t *testing.T) {
	h, _ := powHarness(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 1. /v1/* is gated: no key -> 401.
	code, body := getJSON(t, srv.URL+"/v1/models")
	if code != http.StatusUnauthorized || body["error"] == nil {
		t.Fatalf("expected 401 without key, got %d %v", code, body)
	}

	// 2. Get a challenge.
	code, chalWrap := getJSON(t, srv.URL+"/api/pow/challenge?plan=basic")
	if code != http.StatusOK {
		t.Fatalf("challenge status %d: %v", code, chalWrap)
	}
	ch := chalWrap["challenge"].(map[string]any)
	if ch["plan"].(string) != "basic" {
		t.Errorf("plan = %v", ch["plan"])
	}

	// 3. Solve it. The too-fast guard rejects redemptions <500ms after
	// issuance, so wait that out first.
	time.Sleep(600 * time.Millisecond)
	solution := solveChallenge(t, ch)

	code, resp := postJSON(t, srv.URL+"/api/pow/redeem", fmt.Sprintf(
		`{"challenge_id":%q,"counters":[%q]}`, ch["id"], solution))
	if code != http.StatusOK {
		t.Fatalf("redeem status %d: %v", code, resp)
	}
	apiKey, _ := resp["api_key"].(string)
	if apiKey == "" || resp["plan"] != "basic" {
		t.Fatalf("bad redemption response: %v", resp)
	}

	// 4. Key works against /v1/*.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with issued key, got %d", res.StatusCode)
	}

	// 5. Challenge is single-use.
	code, rerr := postJSON(t, srv.URL+"/api/pow/redeem", fmt.Sprintf(
		`{"challenge_id":%q,"counters":[%q]}`, ch["id"], solution))
	if code != http.StatusConflict || rerr["error"] == nil {
		t.Fatalf("replay should be 409 already_used, got %d %v", code, rerr)
	}
	errMap := rerr["error"].(map[string]any)
	if errMap["code"] != "already_used" {
		t.Errorf("replay code = %v", errMap["code"])
	}
}

func TestPoWBurstCooldown(t *testing.T) {
	var h *Handler
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := config.DefaultConfig()
	cfg.PowEnabled = true
	cfg.PowStorePath = t.TempDir() + "/pow.db"
	cfg.PowBaseDifficulty = 8
	cfg.PowMinDifficulty = 1
	cfg.PowBurstRPS = 3
	cfg.PowBurstCooldown = 5 * time.Minute
	h = newHandler(cfg, nil, nil, nil, testLogger())

	// Issue a key through the real flow.
	srv := httptest.NewServer(h.loggingMiddleware(h.requestIDMiddleware(h.corsMiddleware(h.gatewayAuthMiddleware(h.mux)))))
	defer srv.Close()
	apiKey := issueKey(t, srv, cfg)
	svc := h.PowService()
	if svc == nil {
		t.Fatal("PoW service should be running")
	}
	kh := pow.KeyHash(apiKey)

	// Trip the burst window at unit speed: BurstRPS+1 instant calls.
	var code string
	var retry time.Duration
	for i := 0; i < cfg.PowBurstRPS+1; i++ {
		retry, code = svc.checkKeyLimits(kh, 100)
	}
	if code != "key_cooldown" || retry <= 0 {
		t.Fatalf("expected burst trip (key_cooldown), got %q after %s", code, retry)
	}

	// While cooling down, HTTP requests are rejected even after >1s.
	time.Sleep(1200 * time.Millisecond)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("key should stay in cooldown for 5m, got %d (%s)", res.StatusCode, raw)
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	e := m["error"].(map[string]any)
	if e["code"] != "key_cooldown" {
		t.Errorf("cooldown code = %v", e["code"])
	}
	if ra := res.Header.Get("Retry-After"); ra == "" {
		t.Error("missing Retry-After header on cooldown")
	}
}

func TestPowPlanRPMLimit(t *testing.T) {
	h, cfg := powHarness(t, func(c *config.Config) {
		c.PowPlan1RPM = 2 // tiny bucket
		c.PowBurstRPS = 0 // disable burst so we test RPM specifically
		c.PowBurstCooldown = 0
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	apiKey := issueKey(t, srv, cfg)

	codes := []int{}
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		codes = append(codes, res.StatusCode)
		if res.StatusCode == http.StatusTooManyRequests {
			var m map[string]any
			json.Unmarshal(raw, &m)
			e := m["error"].(map[string]any)
			if e["code"] != "rate_limited" {
				t.Errorf("RPM code = %v", e["code"])
			}
		}
	}
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("first two requests should pass, got %v", codes)
	}
	for _, c := range codes[2:] {
		if c != http.StatusTooManyRequests {
			t.Errorf("requests beyond RPM should be limited, got %v", codes)
			break
		}
	}
}

// issueKey drives challenge->solve->redeem and returns the key.
func issueKey(t *testing.T, srv *httptest.Server, cfg *config.Config) string {
	t.Helper()
	code, chalWrap := getJSON(t, srv.URL+"/api/pow/challenge?plan=basic")
	if code != http.StatusOK {
		t.Fatalf("challenge %d", code)
	}
	ch := chalWrap["challenge"].(map[string]any)
	time.Sleep(600 * time.Millisecond) // too-fast guard
	solution := solveChallenge(t, ch)
	code, resp := postJSON(t, srv.URL+"/api/pow/redeem", fmt.Sprintf(
		`{"challenge_id":%q,"counters":[%q]}`, ch["id"], solution))
	if code != http.StatusOK {
		t.Fatalf("redeem %d: %v", code, resp)
	}
	key, _ := resp["api_key"].(string)
	if key == "" {
		t.Fatalf("no key in %v", resp)
	}
	t.Logf("issued key rpm=%v plan=%v", resp["rpm"], resp["plan"])
	return key
}
