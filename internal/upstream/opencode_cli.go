package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	proxypkg "github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/rs/zerolog"
)

// OpenCodeCLIClient drives the opencode CLI inside each VPN container via
// docker exec (Proxy.ExecIn), so every request is executed by an agent that
// egresses through that container's unique VPN IP. The client's credential is
// injected as a provider env var (default ANTHROPIC_API_KEY) per exec.
//
// `opencode run --format json` emits NDJSON events on stdout; this driver
// relays text/reasoning events as OpenAI-compatible SSE chunks or a non-stream
// JSON completion. Known opencode event-flush bugs can drop text events on
// stdout; when no text event arrives the raw stdout is relayed as plain text.
type OpenCodeCLIClient struct {
	cfg *config.Config
	log *zerolog.Logger

	mu sync.Mutex
	// conversationID -> opencode session id for `opencode run --session`
	sessions map[string]string
}

// NewOpenCodeCLIClient creates a client that execs the opencode CLI inside
// proxy containers.
func NewOpenCodeCLIClient(cfg *config.Config, log *zerolog.Logger) *OpenCodeCLIClient {
	return &OpenCodeCLIClient{
		cfg:      cfg,
		log:      log,
		sessions: make(map[string]string),
	}
}

// compile-time assertion: OpenCodeCLIClient implements Upstream.
var _ Upstream = (*OpenCodeCLIClient)(nil)

// ---------------------------------------------------------------------------
// opencode NDJSON event shapes (subset of `opencode run --format json`)
// ---------------------------------------------------------------------------

type cliEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Text      string `json:"text"`
	Session   struct {
		ID string `json:"id"`
	} `json:"session"`
	// Event payloads are version-dependent: sometimes fields are top-level
	// ({"type":"text","text":...}), sometimes nested ({"type":"text",
	// "part":{"text":...}}). Parse both.
	Part struct {
		Text  string          `json:"text"`
		Error json.RawMessage `json:"error"`
	} `json:"part"`
	Error json.RawMessage `json:"error"`
}

// credential extracts the raw API key from the gateway Auth (strips an
// optional "Bearer " prefix) for injection as a provider env var.
func credential(auth Auth) string {
	v := strings.TrimSpace(auth.Value)
	return strings.TrimPrefix(v, "Bearer ")
}

// buildPrompt flattens the OpenAI messages array into the transcript text
// passed to `opencode run` as the prompt argument.
func buildPrompt(messages []interface{}) string {
	var b strings.Builder
	for _, m := range messages {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		var content string
		switch c := mm["content"].(type) {
		case string:
			content = c
		case []any:
			var parts []string
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			content = strings.Join(parts, "\n")
		}
		if content == "" {
			continue
		}
		switch role {
		case "user":
			b.WriteString("User: ")
		case "assistant":
			b.WriteString("Assistant: ")
		default:
			b.WriteString(role + ": ")
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// runArgs builds the `opencode run` argv: model, extra args, session reuse,
// and the prompt as the final argument.
func (c *OpenCodeCLIClient) runArgs(model, sessionID, prompt string) []string {
	args := []string{"opencode", "run", "--format", "json"}
	if m := c.cfg.OpenCodeCLIModel; m != "" {
		model = m
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, c.cfg.OpenCodeCLIArgs...)
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if prompt != "" {
		args = append(args, prompt)
	}
	return args
}

// ---------------------------------------------------------------------------
// OpenAI-shaped response writers (mirror opencode_client.go)
// ---------------------------------------------------------------------------

type cliChoice struct {
	Index   int         `json:"index"`
	Delta   *cliDelta   `json:"delta,omitempty"`
	Message *cliMessage `json:"message,omitempty"`
	Finish  string      `json:"finish_reason,omitempty"`
}

// cliDelta carries content plus reasoning_content so the chat UI can render
// opencode's thinking blocks. Named fields (not a generic map) keep the chunk
// deterministic for tests.
type cliDelta struct {
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Role             string `json:"role,omitempty"`
}

type cliMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type cliChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []cliChoice `json:"choices"`
	Usage   *Usage      `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// Driver
// ---------------------------------------------------------------------------

type cliRunResult struct {
	text      strings.Builder
	reasoning strings.Builder
	sessionID string
	errMsg    string
	rawStdout string
}

// run executes the opencode CLI inside the proxy container and parses the
// NDJSON event stream.
func (c *OpenCodeCLIClient) run(ctx context.Context, p *proxypkg.Proxy, env, args []string) (*cliRunResult, error) {
	out, err := p.ExecIn(ctx, env, args)
	res := &cliRunResult{rawStdout: string(out)}

	// Robust NDJSON parsing: a line is an event only when it starts with '{'
	// and parses cleanly.
	for _, line := range strings.Split(res.rawStdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var ev cliEvent
		if json.Unmarshal([]byte(trimmed), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "session":
			if ev.Session.ID != "" {
				res.sessionID = ev.Session.ID
			}
			if ev.SessionID != "" {
				res.sessionID = ev.SessionID
			}
		case "text", "reasoning":
			text := ev.Text
			if text == "" {
				text = ev.Part.Text
			}
			if text == "" {
				continue
			}
			if ev.Type == "reasoning" {
				res.reasoning.WriteString(text)
			} else {
				res.text.WriteString(text)
			}
		case "error":
			raw := ev.Error
			if len(raw) == 0 {
				raw = ev.Part.Error
			}
			res.errMsg = cliErrorMessage(raw)
		default:
			// Every event carries the sessionID (older "session" event too);
			// capture it from any event so session continuity works.
			if res.sessionID == "" && ev.SessionID != "" {
				res.sessionID = ev.SessionID
			}
		}
	}

	if res.errMsg != "" {
		return res, fmt.Errorf("opencode: %s", res.errMsg)
	}

	// Fallback for known opencode bugs where text events are missing on
	// stdout: if no text event arrived, relay the raw stdout as plain text.
	if res.text.Len() == 0 {
		res.text.WriteString(res.rawStdout)
	}

	if err != nil && res.text.Len() == 0 {
		return res, err
	}
	return res, nil
}

// Do executes the conversation with the opencode CLI inside the proxy
// container and returns an OpenAI-shaped response.
func (c *OpenCodeCLIClient) Do(ctx context.Context, p *proxypkg.Proxy, body []byte, auth Auth, stream bool) (*http.Response, *RateLimit, error) {
	var req struct {
		Model          string        `json:"model"`
		Messages       []interface{} `json:"messages"`
		ConversationID string        `json:"conversation_id,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errResponse(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err)), nil, nil
	}
	if len(req.Messages) == 0 {
		return errResponse(http.StatusBadRequest, "messages is required"), nil, nil
	}

	// Session continuity: reuse the opencode session mapped to this
	// conversation id (from a previous request in the same conversation).
	sessionID := ""
	if req.ConversationID != "" {
		c.mu.Lock()
		sessionID = c.sessions[req.ConversationID]
		c.mu.Unlock()
	}

	prompt := buildPrompt(req.Messages)
	args := c.runArgs(req.Model, sessionID, prompt)

	env := []string{}
	if key := credential(auth); key != "" {
		env = append(env, c.cfg.OpenCodeCLIProviderEnv+"="+key)
	}

	start := time.Now()
	res, err := c.run(ctx, p, env, args)
	c.log.Info().
		Str("proxy_id", p.ID).
		Str("session_id", res.sessionID).
		Dur("duration", time.Since(start)).
		Bool("stream", stream).
		Msg("opencode CLI run completed")
	if err != nil {
		c.log.Warn().Err(err).Str("proxy_id", p.ID).Msg("opencode CLI run failed")
	}

	// Remember the session id for follow-up turns in the same conversation.
	if req.ConversationID != "" && res.sessionID != "" {
		c.mu.Lock()
		c.sessions[req.ConversationID] = res.sessionID
		c.mu.Unlock()
	}

	model := req.Model
	if m := c.cfg.OpenCodeCLIModel; m != "" {
		model = m
	}

	// Upstream-level failures: rate-limit wording bans the egress IP so the
	// pool rotates; everything else becomes an error response.
	if err != nil {
		body := err.Error() + "\n" + res.rawStdout
		// statusCode 0 intentionally: keyword matching only, so an "invalid
		// api key" error doesn't trip the 429 short-circuit.
		if isRateLimitError(0, body) {
			return nil, &RateLimit{RetryAfter: "60"}, fmt.Errorf("rate limited: %s", body)
		}
		return errResponse(http.StatusBadGateway, err.Error()), nil, nil
	}

	if stream {
		return streamSSE(req.ConversationID, model, res.text.String(), res.reasoning.String()), nil, nil
	}
	return nonStreamJSON(req.ConversationID, model, res.text.String()), nil, nil
}

// DoModels returns the model list: OPENCODE_CLI_MODELS when configured, else
// the live `opencode models` output from inside a proxy container.
func (c *OpenCodeCLIClient) DoModels(ctx context.Context, p *proxypkg.Proxy, auth Auth) (*http.Response, *RateLimit, error) {
	models := c.cfg.OpenCodeCLIModels

	if len(models) == 0 {
		env := []string{}
		if key := credential(auth); key != "" {
			env = append(env, c.cfg.OpenCodeCLIProviderEnv+"="+key)
		}
		out, err := p.ExecIn(ctx, env, []string{"opencode", "models"})
		if err == nil {
			if parsed := parseModelList(string(out)); len(parsed) > 0 {
				models = parsed
			}
		} else {
			c.log.Warn().Err(err).Str("proxy_id", p.ID).Msg("opencode models failed, falling back to defaults")
		}
	}
	if len(models) == 0 {
		models = []string{c.cfg.OpenCodeCLIModel}
	}
	if len(models) == 0 {
		models = []string{"default"}
	}

	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "opencode"})
	}
	b, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// cliErrorMessage unwraps the error event's payload. Observed shapes:
//   - {"error":"message"}
//   - {"error":{"message":"..."}}
//   - {"error":{"name":"...","data":{"message":"..."}}}  (opencode CLI >= 1.18)
func cliErrorMessage(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Data.Message != "" {
			return obj.Data.Message
		}
		return obj.Message
	}
	return string(raw)
}

var modelToken = regexp.MustCompile(`(?i)[a-z0-9][a-z0-9._-]*(?::|/)[a-z0-9][a-z0-9._-]+`)

// parseModelList extracts model ids from `opencode models` output, which is a
// text table listing provider/model pairs.
func parseModelList(out string) []string {
	seen := map[string]bool{}
	var models []string
	for _, tok := range modelToken.FindAllString(out, -1) {
		if tok = strings.TrimSpace(tok); tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		models = append(models, tok)
	}
	return models
}

// streamSSE builds a streaming SSE body: role chunk, reasoning_content chunks
// (rendered as the assistant's thinking by the chat UI), content chunks and a
// final [DONE].
func streamSSE(id, model, text, reasoning string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		now := time.Now().Unix()
		emit := func(chunk cliChatResponse) error {
			b, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(pw, "data: %s\n\n", b)
			return err
		}
		_ = emit(cliChatResponse{
			ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
			Choices: []cliChoice{{Index: 0, Delta: &cliDelta{Role: "assistant"}}},
		})
		if reasoning != "" {
			_ = emit(cliChatResponse{
				ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
				Choices: []cliChoice{{Index: 0, Delta: &cliDelta{ReasoningContent: reasoning}}},
			})
		}
		if text != "" {
			for _, chunk := range splitChunks(text) {
				if emit(cliChatResponse{
					ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
					Choices: []cliChoice{{Index: 0, Delta: &cliDelta{Content: chunk}}},
				}) != nil {
					break
				}
			}
		}
		_ = emit(cliChatResponse{
			ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
			Choices: []cliChoice{{Index: 0, Finish: "stop"}},
		})
		_, _ = fmt.Fprintf(pw, "data: [DONE]\n\n")
		pw.Close()
	}()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
}

// splitChunks splits text into sentence/word-sized pieces to mimic streaming.
func splitChunks(text string) []string {
	sentences := strings.Split(text, "\n")
	var chunks []string
	for _, s := range sentences {
		for i, w := range strings.Fields(s) {
			chunk := w
			if i > 0 {
				chunk = " " + chunk
			}
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

// nonStreamJSON builds a single-completion JSON response.
func nonStreamJSON(id, model, text string) *http.Response {
	b, _ := json.Marshal(cliChatResponse{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []cliChoice{{Index: 0, Message: &cliMessage{Role: "assistant", Content: text}}},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// errResponse builds an OpenAI-shaped error response.
func errResponse(status int, message string) *http.Response {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "error"},
	})
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}
