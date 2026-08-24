package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	proxypkg "github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/rs/zerolog"
)

// cliExecManager records exec invocations and returns scripted output.
type cliExecManager struct {
	output []byte
	err    error
	calls  []struct {
		env  []string
		args []string
	}
}

func (m *cliExecManager) Create(ctx context.Context) (*proxypkg.Proxy, error) {
	return m.CreateEx(ctx, nil, nil)
}

func (m *cliExecManager) CreateEx(ctx context.Context, bannedRegions, avoidServers map[string]bool) (*proxypkg.Proxy, error) {
	return nil, nil
}

func (m *cliExecManager) Remove(ctx context.Context, id string) error { return nil }
func (m *cliExecManager) HealthCheck(ctx context.Context, p *proxypkg.Proxy) (bool, error) {
	return true, nil
}
func (m *cliExecManager) Close() error { return nil }
func (m *cliExecManager) Exec(ctx context.Context, id string, env, args []string) ([]byte, error) {
	m.calls = append(m.calls, struct {
		env  []string
		args []string
	}{env, args})
	return m.output, m.err
}

func newCLIClient() (*OpenCodeCLIClient, *proxypkg.Proxy, *cliExecManager) {
	cfg := config.DefaultConfig()
	log := zerolog.Nop()
	client := NewOpenCodeCLIClient(cfg, &log)
	mgr := &cliExecManager{}
	proxy := &proxypkg.Proxy{ID: "cli-test", ContainerID: "abc123", State: proxypkg.StateIdle}
	proxy.SetManager(mgr)
	return client, proxy, mgr
}

func chatBody(model, convID string, stream bool) []byte {
	req := map[string]any{
		"model":           model,
		"stream":          stream,
		"messages":        []map[string]any{{"role": "user", "content": "hello"}},
		"conversation_id": convID,
	}
	b, _ := json.Marshal(req)
	return b
}

func ndjsonEvents(events ...string) []byte {
	return []byte(strings.Join(events, "\n") + "\n")
}

func TestOpenCodeCLIBuildPrompt(t *testing.T) {
	messages := []interface{}{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello!"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"text": "part1"},
			map[string]any{"text": "part2"},
		}},
		map[string]any{"role": "system", "content": "ignore-system"},
	}
	prompt := buildPrompt(messages)
	for _, want := range []string{"User: hi", "Assistant: hello!", "User: part1\npart2", "ignore-system"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %q", want, prompt)
		}
	}
}

func TestOpenCodeCLIRunArgs(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.OpenCodeCLIArgs = []string{"--agent", "build"}
	c := &OpenCodeCLIClient{cfg: cfg}

	if got := c.runArgs("anthropic/claude", "sess-1", "do it"); !strings.Contains(strings.Join(got, " "), "--model anthropic/claude --agent build --session sess-1 do it") {
		t.Errorf("args: %v", got)
	}

	// cfg OpenCodeCLIModel wins over the request model
	cfg.OpenCodeCLIModel = "claude-cfg"
	got := c.runArgs("anthropic/claude", "", "do it")
	if !strings.Contains(strings.Join(got, " "), "--model claude-cfg") || strings.Contains(strings.Join(got, " "), "anthropic/claude") {
		t.Errorf("cfg model should win: %v", got)
	}
}

func TestOpenCodeCLIDoNonStream(t *testing.T) {
	client, proxy, mgr := newCLIClient()
	mgr.output = ndjsonEvents(
		`{"type":"step_start","sessionID":"sess-42","part":{"type":"step-start"}}`,
		`{"type":"text","sessionID":"sess-42","part":{"type":"text","text":"answer one"}}`,
		`{"type":"text","sessionID":"sess-42","part":{"type":"text","text":" answer two"}}`,
		`{"type":"step_finish","sessionID":"sess-42","part":{"type":"step-finish","reason":"stop"}}`,
	)

	resp, rl, err := client.Do(context.Background(), proxy, chatBody("anthropic/claude", "conv-1", false), Auth{Header: "Authorization", Value: "Bearer sk-ant-xyz"}, false)
	if err != nil || rl != nil {
		t.Fatalf("Do: err=%v rl=%v", err, rl)
	}
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, b)
	}
	if got := out.Choices[0].Message.Content; got != "answer one answer two" {
		t.Errorf("content: %q", got)
	}
	if out.Model != "anthropic/claude" {
		t.Errorf("model: %q", out.Model)
	}

	// Session id must be remembered for conversation continuity.
	client.mu.Lock()
	defer client.mu.Unlock()
	if got := client.sessions["conv-1"]; got != "sess-42" {
		t.Errorf("sessions[conv-1] = %q", got)
	}

	// Auth must land in the provider env var.
	if len(mgr.calls) != 1 || len(mgr.calls[0].env) != 1 {
		t.Fatalf("calls: %+v", mgr.calls)
	}
	if got := mgr.calls[0].env[0]; got != "ANTHROPIC_API_KEY=sk-ant-xyz" {
		t.Errorf("env: %q", got)
	}
	if j := strings.Join(mgr.calls[0].args, " "); !strings.Contains(j, "opencode run --format json") || !strings.HasSuffix(j, "User: hello") {
		t.Errorf("args: %q", j)
	}
}

func TestOpenCodeCLIDoStream(t *testing.T) {
	client, proxy, mgr := newCLIClient()
	mgr.output = ndjsonEvents(
		`{"type":"reasoning","sessionID":"sess-r","part":{"type":"reasoning","text":"think step 1"}}`,
		`{"type":"text","sessionID":"sess-r","part":{"type":"text","text":"final answer"}}`,
	)

	resp, rl, err := client.Do(context.Background(), proxy, chatBody("m", "conv-s", true), Auth{}, true)
	if err != nil || rl != nil {
		t.Fatalf("Do: err=%v rl=%v", err, rl)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
	var foundReasoning, foundContent bool
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk parse: %v (%s)", err, line)
		}
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.ReasoningContent != "" {
				foundReasoning = true
			}
			if chunk.Choices[0].Delta.Content != "" {
				foundContent = true
			}
		}
	}
	if !foundReasoning {
		t.Error("no reasoning_content delta emitted")
	}
	if !foundContent {
		t.Error("no content delta emitted")
	}
}

func TestOpenCodeCLIErrorEvent(t *testing.T) {
	client, proxy, mgr := newCLIClient()
	mgr.output = ndjsonEvents(
		`{"type":"error","sessionID":"sess-e","error":{"name":"AuthError","data":{"message":"invalid api key"}}}`,
	)

	resp, rl, err := client.Do(context.Background(), proxy, chatBody("m", "", false), Auth{}, false)
	if err != nil || rl != nil {
		t.Fatalf("expected error surfaced via response, got err=%v rl=%v", err, rl)
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: %v", resp)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "invalid api key") {
		t.Errorf("error body missing message: %s", b)
	}
}

func TestOpenCodeCLIRateLimitSignalsRotation(t *testing.T) {
	client, proxy, mgr := newCLIClient()
	mgr.output = ndjsonEvents(
		`{"type":"error","error":"rate limit exceeded, try again later"}`,
	)

	_, rl, err := client.Do(context.Background(), proxy, chatBody("m", "", false), Auth{}, false)
	if rl == nil {
		t.Fatalf("expected rate limit signal, err=%v", err)
	}
	if rl.RetryAfter != "60" {
		t.Errorf("RetryAfter: %q", rl.RetryAfter)
	}
}

func TestOpenCodeCLIFallbackPlainText(t *testing.T) {
	client, proxy, mgr := newCLIClient()
	// No NDJSON text events at all (known opencode stdout flush bug).
	mgr.output = []byte("plain fallback answer\non two lines")

	resp, _, err := client.Do(context.Background(), proxy, chatBody("m", "", false), Auth{}, false)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "plain fallback answer") {
		t.Errorf("fallback output missing: %s", b)
	}
}

func TestOpenCodeCLISessionContinuation(t *testing.T) {
	client, proxy, mgr := newCLIClient()
	mgr.output = ndjsonEvents(`{"type":"step_start","sessionID":"sess-1","part":{"type":"step-start"}}`, `{"type":"text","sessionID":"sess-1","part":{"type":"text","text":"first"}}`)

	if _, _, err := client.Do(context.Background(), proxy, chatBody("m", "conv-x", false), Auth{}, false); err != nil {
		t.Fatalf("first Do: %v", err)
	}

	client, proxy, mgr = newCLIClient()
	mgr.output = ndjsonEvents(`{"type":"text","text":"second"}`)
	// New client, no session known yet: no --session flag.
	if _, _, err := client.Do(context.Background(), proxy, chatBody("m", "conv-y", false), Auth{}, false); err != nil {
		t.Fatalf("second Do: %v", err)
	}
	if j := strings.Join(mgr.calls[0].args, " "); strings.Contains(j, "--session") {
		t.Errorf("unexpected --session for fresh conversation: %q", j)
	}

	// Simulate an established mapping (e.g. from the client above).
	client.mu.Lock()
	client.sessions["conv-x"] = "sess-1"
	client.mu.Unlock()
	if _, _, err := client.Do(context.Background(), proxy, chatBody("m", "conv-x", false), Auth{}, false); err != nil {
		t.Fatalf("continuation Do: %v", err)
	}
	j := strings.Join(mgr.calls[1].args, " ")
	if !strings.Contains(j, "--session sess-1") {
		t.Errorf("missing --session on continuation: %q", j)
	}
}

func TestOpenCodeCLIDoModels(t *testing.T) {
	client, proxy, mgr := newCLIClient()
	mgr.output = []byte("anthropic:claude-sonnet-4-20250514\nopenai:gpt-4o\nfoo\n")

	resp, rl, err := client.DoModels(context.Background(), proxy, Auth{})
	if err != nil || rl != nil {
		t.Fatalf("DoModels: err=%v rl=%v", err, rl)
	}
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, b)
	}
	if len(out.Data) != 2 || out.Data[0].ID != "anthropic:claude-sonnet-4-20250514" {
		t.Errorf("models: %+v", out.Data)
	}

	// OPENCODE_CLI_MODELS override skips the exec entirely.
	client2 := NewOpenCodeCLIClient(&config.Config{OpenCodeCLIModels: []string{"cfg-model"}, OpenCodeCLIProviderEnv: "ANTHROPIC_API_KEY"}, &zerolog.Logger{})
	mgr2 := &cliExecManager{}
	proxy2 := &proxypkg.Proxy{ID: "p2"}
	proxy2.SetManager(mgr2)
	resp, _, err = client2.DoModels(context.Background(), proxy2, Auth{})
	if err != nil {
		t.Fatalf("DoModels: %v", err)
	}
	if len(mgr2.calls) != 0 {
		t.Errorf("expected models exec to be skipped for configured list, calls=%d", len(mgr2.calls))
	}
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "cfg-model") {
		t.Errorf("configured models missing: %s", b)
	}
}

func TestOpenCodeCLIParseModelList(t *testing.T) {
	got := parseModelList("NAME\nanthropic:claude-sonnet-4-20250514  claude-sonnet-4\nopenai/gpt-4o\nnot-a-model\n")
	if len(got) != 2 {
		t.Fatalf("parsed: %v", got)
	}
	if got[0] != "anthropic:claude-sonnet-4-20250514" || got[1] != "openai/gpt-4o" {
		t.Errorf("parsed: %v", got)
	}
}
