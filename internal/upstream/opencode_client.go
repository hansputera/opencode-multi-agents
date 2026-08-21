package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	proxypkg "github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/rs/zerolog"
)

// OpenCodeClient drives an OpenCode Server (opencode serve) instance through a
// per-request VPN proxy so each request egresses from a unique IP. It adapts
// OpenCode's session/message API to the OpenAI-compatible shape the handler
// expects from upstream.Upstream.
//
// Single-turn v1 behaviour: one OpenAI chat request creates a fresh OpenCode
// session, sends the conversation messages as `parts`, and relays the
// assistant turn (text, optionally tool calls) as an OpenAI chat response.
// Multi-message continuity / tool-call execution is left for a later iteration.
type OpenCodeClient struct {
	cfg  *config.Config
	log  *zerolog.Logger
	base string

	httpCache map[string]*http.Client
	mu        sync.Mutex
}

// NewOpenCodeClient creates a client against an OpenCode Server.
func NewOpenCodeClient(cfg *config.Config, log *zerolog.Logger) *OpenCodeClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.OpenCodeServerURL), "/")
	return &OpenCodeClient{
		cfg:       cfg,
		log:       log,
		base:      base,
		httpCache: make(map[string]*http.Client),
	}
}

// compile-time assertion: OpenCodeClient implements Upstream.
var _ Upstream = (*OpenCodeClient)(nil)

// ---------------------------------------------------------------------------
// Request bodies sent to OpenCode Server
// ---------------------------------------------------------------------------

// ocPart is a single message part in the OpenCode Server API.
type ocPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ocMessageRequest struct {
	ProviderID string  `json:"providerID"`
	ModelID    string  `json:"modelID,omitempty"`
	Agent      string  `json:"agent,omitempty"`
	Parts      []ocPart `json:"parts"`
}

type ocSessionRequest struct {
	Title    string `json:"title,omitempty"`
	ParentID string `json:"parentID,omitempty"`
}

type ocSessionResponse struct {
	ID string `json:"id"`
}

// ocPartResult mirrors the relevant fields of an OpenCode Part returned by the
// /session/:id/message endpoint.
type ocPartResult struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Error     *struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type ocMessageResponse struct {
	Info struct {
		ID        string `json:"id"`
		Role      string `json:"role"`
		Status    string `json:"status"`
	} `json:"info"`
	Parts []ocPartResult `json:"parts"`
}

// ---------------------------------------------------------------------------
// OpenAI-shaped response writers
// ---------------------------------------------------------------------------

// ocChoice / ocUsage are a small slice of the OpenAI chat response schema.
type ocChoice struct {
	Index   int      `json:"index"`
	Delta   *ocDelta  `json:"delta,omitempty"`
	Message *ocMessage `json:"message,omitempty"`
	Finish  string   `json:"finish_reason,omitempty"`
}

type ocDelta struct {
	Content string `json:"content,omitempty"`
	Role    string `json:"role,omitempty"`
}

type ocMessage struct {
	Role    string      `json:"role"`
	Content string      `json:"content"`
}

type ocChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []ocChoice     `json:"choices"`
}

// ---------------------------------------------------------------------------
// Driver
// ---------------------------------------------------------------------------

// resolveModel splits an OpenAI model field ("provider/model") into the
// OpenCode provider+model pair, falling back to config defaults.
func (c *OpenCodeClient) resolveModel(openaiModel string) (providerID, modelID string) {
	if c.cfg.OpenCodeModel != "" {
		return c.cfg.OpenCodeProviderID, c.cfg.OpenCodeModel
	}
	if i := strings.Index(openaiModel, "/"); i > 0 {
		return openaiModel[:i], openaiModel[i+1:]
	}
	return c.cfg.OpenCodeProviderID, openaiModel
}

// toAuth adapts the gateway Auth into an OpenCode request: bearer key -> PUT
// /auth/{providerID}; password -> http basic auth header on every call.
func (c *OpenCodeClient) toAuth(auth Auth) (basicUser, basicPass string, putAuth bool, putProviderID, putKey string) {
	if c.cfg.OpenCodeServerPassword != "" {
		basicUser, basicPass = "opencode", c.cfg.OpenCodeServerPassword
		return
	}
	if strings.HasPrefix(strings.ToLower(auth.Header), "authorization") && strings.HasPrefix(auth.Value, "Bearer ") {
		// Client supplied a bearer key; hand it to OpenCode for providerID.
		_, putProviderID = c.resolveModel("")
		putAuth = true
		putKey = strings.TrimPrefix(auth.Value, "Bearer ")
	}
	return
}

func (c *OpenCodeClient) getClient(p *proxypkg.Proxy) (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.httpCache[p.ID]; ok {
		return client, nil
	}
	dialer, err := NewSOCKS5Dialer(p.SOCKS5Addr)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: &http.Transport{Dial: dialer.Dial},
		Timeout:   c.cfg.RequestTimeout,
	}
	c.httpCache[p.ID] = client
	return client, nil
}

// newRequest builds an OpenCode Server request tunneled through proxy p.
func (c *OpenCodeClient) newRequest(ctx context.Context, p *proxypkg.Proxy, method, path string, body io.Reader, auth Auth) (*http.Request, *http.Client, error) {
	client, err := c.getClient(p)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, nil, err
	}
	if bu, bp, _, _, _ := c.toAuth(auth); bu != "" {
		req.SetBasicAuth(bu, bp)
	}
	return req, client, nil
}

// rateLimitFromResp extracts a *RateLimit from a 429 response (if any).
func rateLimitFromResp(resp *http.Response) *RateLimit {
	if resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	return &RateLimit{RetryAfter: resp.Header.Get("Retry-After")}
}

// DoChatCompletion performs a single OpenCode turn through the proxy and writes
// an OpenAI-shaped response to w. When stream is true the assistant text is
// emitted as a sequence of SSE deltas (one `data: ...` line per chunk).
func (c *OpenCodeClient) DoChatCompletion(w io.Writer, p *proxypkg.Proxy, body []byte, auth Auth, stream bool) (*RateLimit, error) {
	var req struct {
		Model    string        `json:"model"`
		Stream   bool          `json:"stream"`
		Messages []ocPart      `json:"-"`
		Raw      []interface{} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)

	parts := conversationToParts(req.Raw)
	providerID, modelID := c.resolveModel(req.Model)

	// 1) Create a session for this request.
	sessReq, _ := json.Marshal(ocSessionRequest{Title: "gateway-session"})
	createReq, client, err := c.newRequest(context.Background(), p, "POST", "/session", bytes.NewReader(sessReq), auth)
	if err != nil {
		return nil, err
	}
	createResp, err := client.Do(createReq)
	if err != nil {
		return nil, err
	}
	if rl := rateLimitFromResp(createResp); rl != nil {
		createResp.Body.Close()
		return rl, fmt.Errorf("rate limited (429)")
	}
	if createResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opencode /session: %s", createResp.Status)
	}
	var sess ocSessionResponse
	if err := json.NewDecoder(createResp.Body).Decode(&sess); err != nil {
		return nil, fmt.Errorf("decode session: %w", err)
	}
	createResp.Body.Close()
	if sess.ID == "" {
		return nil, fmt.Errorf("opencode: empty session id")
	}

	// 2) Hand the client's bearer key to OpenCode if applicable.
	if _, _, putAuth, putProvider, putKey := c.toAuth(auth); putAuth {
		cred, _ := json.Marshal(map[string]string{"type": "api", "key": putKey})
		authReq, _, err := c.newRequest(context.Background(), p, "PUT", "/auth/"+putProvider, bytes.NewReader(cred), auth)
		if err != nil {
			return nil, err
		}
		ar, err := client.Do(authReq)
		if ar != nil {
			ar.Body.Close()
		}
		_ = err // non-fatal: server may already be authed
	}

	// 3) Send the user message.
	msg := ocMessageRequest{ProviderID: providerID, ModelID: modelID, Parts: parts}
	msgBytes, _ := json.Marshal(msg)
	msgReq, _, err := c.newRequest(context.Background(), p, "POST", "/session/"+sess.ID+"/message", bytes.NewReader(msgBytes), auth)
	if err != nil {
		return nil, err
	}
	msgReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(msgReq)
	if err != nil {
		return nil, err
	}
	if rl := rateLimitFromResp(resp); rl != nil {
		resp.Body.Close()
		return rl, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("opencode message: %s: %s", resp.Status, string(b))
	}

	// 4) Relay the assistant turn as an OpenAI response.
	defer resp.Body.Close()
	var out ocMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}

	text := assistantText(out.Parts)
	if hasErrorPart(out.Parts) {
		return nil, fmt.Errorf("opencode tool error: %s", errorPartMessage(out.Parts))
	}

	model := req.Model
	if modelID != "" {
		model = modelID
	}
	if stream {
		return nil, writeStream(w, sess.ID, model, text)
	}
	return nil, writeNonStream(w, sess.ID, model, text)
}

// Do (satisfies Upstream) adapts the OpenAI chat body to the OpenCode flow. It
// ignores the standard non-stream Do body and is used by the handler in place
// of DoChatCompletion for the streaming/non-streaming paths.
func (c *OpenCodeClient) Do(ctx context.Context, p *proxypkg.Proxy, body []byte, auth Auth, stream bool) (*http.Response, *RateLimit, error) {
	// The OpenCode response is synthesized in-memory; we still return a
	// synthetic *http.Response-compatible object is not useful to the handler
	// (which expects to stream a real body). Instead we write directly to a
	// buffered body and return it.
	return c.doAdapt(ctx, p, body, auth, stream)
}

// DoModels returns a tiny synthetic model list in OpenCode mode (the model is
// whatever was configured / requested).
func (c *OpenCodeClient) DoModels(ctx context.Context, p *proxypkg.Proxy, auth Auth) (*http.Response, *RateLimit, error) {
	mdl := c.cfg.OpenCodeModel
	if mdl == "" {
		mdl = "default"
	}
	list := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": mdl, "object": "model", "owned_by": "opencode"},
		},
	}
	b, _ := json.Marshal(list)
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil, nil
}

// conversationToParts turns an OpenAI messages array into OpenCode text parts.
// The last user message is the prompt; earlier messages are folded in as
// context text. (Multi-turn tool state is not modelled in v1.)
func conversationToParts(messages []interface{}) []ocPart {
	var parts []ocPart
	for _, m := range messages {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		content, _ := mm["content"].(string)
		if content == "" {
			continue
		}
		var prefix string
		switch role {
		case "assistant":
			prefix = "Assistant: "
		case "user":
			prefix = ""
		default:
			prefix = fmt.Sprintf("%s: ", role)
		}
		parts = append(parts, ocPart{Type: "text", Text: prefix + content})
	}
	return parts
}

func assistantText(parts []ocPartResult) string {
	var b strings.Builder
	for _, pt := range parts {
		if pt.Type == "text" {
			b.WriteString(pt.Text)
		}
	}
	return b.String()
}

func hasErrorPart(parts []ocPartResult) bool {
	for _, pt := range parts {
		if pt.Error != nil {
			return true
		}
	}
	return false
}

func errorPartMessage(parts []ocPartResult) string {
	for _, pt := range parts {
		if pt.Error != nil {
			return pt.Error.Message
		}
	}
	return ""
}

func writeStream(w io.Writer, id, model, text string) error {
	now := time.Now().Unix()
	emit := func(chunk ocChatResponse) error {
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	if err := emit(ocChatResponse{
		ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
		Choices: []ocChoice{{Index: 0, Delta: &ocDelta{Role: "assistant"}}},
	}); err != nil {
		return err
	}
	// Emit text in word-sized chunks to mimic streaming.
	words := strings.Fields(text)
	for i := 0; i < len(words); i++ {
		chunk := words[i]
		if i > 0 {
			chunk = " " + chunk
		}
		if err := emit(ocChatResponse{
			ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
			Choices: []ocChoice{{Index: 0, Delta: &ocDelta{Content: chunk}}},
		}); err != nil {
			return err
		}
		// Yield so the handler's flusher can push chunks to the client.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Millisecond)
	}
	return emit(ocChatResponse{
		ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
		Choices: []ocChoice{{Index: 0, Finish: "stop"}},
	})
}

func writeNonStream(w io.Writer, id, model, text string) error {
	b, err := json.Marshal(ocChatResponse{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []ocChoice{{Index: 0, Message: &ocMessage{Role: "assistant", Content: text}}},
	})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// writeSSE is a thin io.Writer that also implements http.Flusher so the SSE
// streaming writer above can flush the pipe's buffered chunks.
type sseWriter struct {
	w io.Writer
	f http.Flusher
}

func (s sseWriter) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s sseWriter) Flush()                        { if s.f != nil { s.f.Flush() } }

func (c *OpenCodeClient) doAdapt(ctx context.Context, p *proxypkg.Proxy, body []byte, auth Auth, stream bool) (*http.Response, *RateLimit, error) {
	if stream {
		pr, pw := io.Pipe()
		go func() {
			sw := sseWriter{w: pw}
			rl, err := c.DoChatCompletion(sw, p, body, auth, true)
			if err != nil {
				fmt.Fprintf(pw, "data: %s\n\n", err.Error())
			}
			_ = rl
			pw.Close()
		}()
		_ = ctx
		return &http.Response{
			StatusCode: 200,
			Body:       pr,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}, nil, nil
	}
	buf := &bytes.Buffer{}
	rl, err := c.DoChatCompletion(buf, p, body, auth, false)
	if err != nil || rl != nil {
		return nil, rl, err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(buf),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil, nil
}
