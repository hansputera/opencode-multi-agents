package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hansputera/opencode-multi-agents/internal/config"
)

// wsHarness is a single httptest server playing three roles:
//   - POST /v1/chat/completions  ("upstream provider", scripted via .upstream)
//   - GET  /search               (stub SearXNG)
//   - GET  /page/1               (page the search result points at)
//
// The upstream handler is resolved at request time, so tests can assign
// wh.upstream after construction and still have it take effect.
type wsHarness struct {
	srv    *httptest.Server
	srvURL string

	upstream http.HandlerFunc

	mu       sync.Mutex
	requests []string // raw bodies received by the upstream
}

func newWSHarness(t *testing.T) *wsHarness {
	t.Helper()
	h := &wsHarness{}

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			// SearXNG provider appends "/search" to its base URL.
			w.Write([]byte(`{"results":[{"title":"Weather Report","url":"` + h.srvURL + `/page/1","content":"city weather snippet"}]}`))
		case "/page/1":
			w.Write([]byte(`<html><body><h1>Forecast</h1><p>Sunny with 25 degrees.</p></body></html>`))
		case "/v1/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.requests = append(h.requests, string(raw))
			h.mu.Unlock()
			if h.upstream != nil {
				h.upstream(w, r)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	h.srv = httptest.NewServer(root)
	h.srvURL = h.srv.URL
	t.Cleanup(h.srv.Close)
	return h
}

func (wh *wsHarness) hits() []string {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	return append([]string(nil), wh.requests...)
}

// gateway builds a handler using this harness as both upstream and SearXNG.
// The SearXNG base is the harness root — the provider appends "/search".
func (wh *wsHarness) gateway(t *testing.T, cfgMut ...func(*config.Config)) http.Handler {
	return newTestGateway(t, wh.srvURL, append([]func(*config.Config){
		func(c *config.Config) { c.SearxngURL = wh.srvURL },
	}, cfgMut...)...)
}

// --- non-streaming loop ---

func TestWebSearchToolLoopNonStreaming(t *testing.T) {
	wh := newWSHarness(t)
	round := 0
	wh.upstream = func(w http.ResponseWriter, r *http.Request) {
		round++
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			w.Write([]byte(`{"id":"r1","object":"chat.completion","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"weather today\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			return
		}
		w.Write([]byte(`{"id":"r2","object":"chat.completion","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"It is sunny and 25 degrees."}}],"usage":{"prompt_tokens":40,"completion_tokens":8,"total_tokens":48}}`))
	}

	srv := httptest.NewServer(wh.gateway(t))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"what is the weather?"}]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "sunny and 25 degrees") {
		t.Errorf("final answer missing from response: %s", raw)
	}

	reqs := wh.hits()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 upstream calls (tool round), got %d", len(reqs))
	}

	var first map[string]any
	json.Unmarshal([]byte(reqs[0]), &first)
	injected := false
	if tools, ok := first["tools"].([]any); ok {
		for _, tl := range tools {
			tm, _ := tl.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			if name, _ := fn["name"].(string); name == "web_search" {
				injected = true
			}
		}
	}
	if !injected {
		t.Errorf("web_search tool not injected into first request: %s", reqs[0])
	}

	var second struct {
		Messages []struct {
			Role         string `json:"role"`
			ToolCallID   string `json:"tool_call_id"`
			Content      any    `json:"content"`
			ToolCallsRaw []any  `json:"tool_calls"`
		} `json:"messages"`
	}
	json.Unmarshal([]byte(reqs[1]), &second)
	msgs := second.Messages
	if len(msgs) < 3 {
		t.Fatalf("expected >=3 messages in round-2 request, got %d", len(msgs))
	}
	last := msgs[len(msgs)-1]
	contentStr, _ := last.Content.(string)
	if last.Role != "tool" || last.ToolCallID != "call_1" ||
		!strings.Contains(contentStr, "Sunny with 25 degrees") {
		t.Errorf("bad tool message: %+v (%q)", last, contentStr)
	}
	secondLast := msgs[len(msgs)-2]
	if secondLast.Role != "assistant" || len(secondLast.ToolCallsRaw) != 1 {
		t.Errorf("second-to-last should replay assistant tool_calls, got %+v", secondLast)
	}
}

// TestWebSearchDisabledByEnv verifies WEB_SEARCH_ENABLED=false skips injection.
func TestWebSearchDisabledByEnv(t *testing.T) {
	wh := newWSHarness(t)
	wh.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"r1","object":"chat.completion","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"no tools"}}]}`))
	}

	srv := httptest.NewServer(wh.gateway(t, func(c *config.Config) { c.WebSearchEnabled = false }))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	reqs := wh.hits()
	var first map[string]any
	json.Unmarshal([]byte(reqs[0]), &first)
	if _, has := first["tools"]; has {
		t.Error("tools must not be injected when web search is disabled")
	}
}

// --- streaming loop ---

func TestWebSearchToolLoopStreaming(t *testing.T) {
	wh := newWSHarness(t)
	round := 0
	wh.upstream = func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		round++
		if round == 1 {
			for _, line := range []string{
				`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"web_search","arguments":""}}]}}]}`,
				`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"weather\"}"}}]}}]}`,
				`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				"data: [DONE]",
			} {
				w.Write([]byte(line + "\n\n"))
				flusher.Flush()
			}
			return
		}
		for _, line := range []string{
			`data: {"id":"2","choices":[{"index":0,"delta":{"content":"It is sunny."}}]}`,
			`data: {"id":"2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			"data: [DONE]",
		} {
			w.Write([]byte(line + "\n\n"))
			flusher.Flush()
		}
	}

	srv := httptest.NewServer(wh.gateway(t))
	defer srv.Close()

	body := `{"model":"m1","messages":[{"role":"user","content":"weather?"}],"stream":true}`
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	stream, _ := io.ReadAll(res.Body)

	if !strings.Contains(string(stream), "It is sunny.") {
		t.Errorf("final round content missing from stream:\n%s", stream)
	}
	if strings.Contains(string(stream), `"web_search"`) || strings.Contains(string(stream), `"tool_calls"`) {
		t.Errorf("intercepted tool-call chunks leaked to client:\n%s", stream)
	}
	if n := strings.Count(string(stream), "data: [DONE]"); n != 1 {
		t.Errorf("expected exactly 1 [DONE], got %d:\n%s", n, stream)
	}
	if len(wh.hits()) != 2 {
		t.Errorf("expected 2 upstream calls, got %d", len(wh.hits()))
	}
}

// TestClientToolsNotIntercepted verifies model calls to CLIENT tools are
// relayed verbatim so the client can drive its own function-calling flow.
func TestClientToolsNotIntercepted(t *testing.T) {
	wh := newWSHarness(t)
	wh.upstream = func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_c","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"X\"}"}}]}}]}`,
			`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			"data: [DONE]",
		} {
			w.Write([]byte(line + "\n\n"))
			flusher.Flush()
		}
	}

	srv := httptest.NewServer(wh.gateway(t))
	defer srv.Close()

	body := `{"model":"m1","messages":[{"role":"user","content":"weather?"}],"stream":true,"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	if !strings.Contains(string(raw), `"get_weather"`) {
		t.Errorf("client tool-call chunks must be relayed verbatim:\n%s", raw)
	}
}

// TestMixedToolsRelayedVerbatim: a turn containing BOTH a client tool call
// and web_search is relayed untouched — the gateway only executes rounds
// where every call is its own web_search.
func TestMixedToolsRelayedVerbatim(t *testing.T) {
	wh := newWSHarness(t)
	callsSeen := 0
	wh.upstream = func(w http.ResponseWriter, r *http.Request) {
		callsSeen++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}},` +
			`{"id":"c2","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q\"}"}}]}}]}`))
	}

	srv := httptest.NewServer(wh.gateway(t))
	defer srv.Close()

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	if !strings.Contains(string(raw), `"c1"`) || !strings.Contains(string(raw), `"c2"`) {
		t.Errorf("mixed tool-call turn must be relayed untouched:\n%s", raw)
	}
	if callsSeen != 1 {
		t.Errorf("mixed turn must not trigger extra rounds, upstream hits=%d", callsSeen)
	}
}
