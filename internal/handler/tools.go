package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/proxy"
	"github.com/hansputera/opencode-multi-agents/internal/upstream"
	"github.com/hansputera/opencode-multi-agents/internal/websearch"
)

// openAIToolCall is one entry of a message's tool_calls array (typed form).
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// webSearchToolName is the function name reserved by the gateway.
const webSearchToolName = "web_search"

// toolCallAccum assembles a streamed tool call from delta fragments.
type toolCallAccum struct {
	id   string
	typ  string
	name string
	args strings.Builder
}

// assembleToolCalls converts accumulated fragments into raw tool_call objects
// ordered by their index.
func assembleToolCalls(accs map[int]*toolCallAccum, order []int) []map[string]any {
	if len(order) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(order))
	for _, idx := range order {
		a := accs[idx]
		if a == nil || a.name == "" {
			continue
		}
		typ := a.typ
		if typ == "" {
			typ = "function"
		}
		out = append(out, map[string]any{
			"id":   a.id,
			"type": typ,
			"function": map[string]any{
				"name":      a.name,
				"arguments": a.args.String(),
			},
		})
	}
	return out
}

// injectWebSearchTool adds the gateway's web_search function to a request
// body's tools array. Returns the (possibly re-marshalled) body and whether
// injection happened. If the client already defines a web_search tool, their
// definition wins and nothing is changed.
func injectWebSearchTool(body []byte) ([]byte, bool) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	tools, _ := m["tools"].([]any)
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		if name, _ := fn["name"].(string); name == webSearchToolName {
			return body, false
		}
	}
	m["tools"] = append(tools, websearch.ToolDefinition())
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// messageToolCalls extracts the tool_calls array from a chat completion
// choice message, preserving raw JSON so it can be replayed verbatim.
func messageToolCalls(message map[string]any) []map[string]any {
	calls, _ := message["tool_calls"].([]any)
	if len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		if cm, ok := c.(map[string]any); ok {
			out = append(out, cm)
		}
	}
	return out
}

// toolCallName reads the function name from a raw tool_call object.
func toolCallName(call map[string]any) string {
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

// allWebSearch reports whether every call targets the gateway's tool.
func allWebSearch(calls []map[string]any) bool {
	for _, c := range calls {
		if toolCallName(c) != webSearchToolName {
			return false
		}
	}
	return len(calls) > 0
}

// wsFor returns the web search service bound to the given proxy's egress
// tunnel (searches and page fetches use the same rotating VPN IP as model
// traffic), or nil when the tool is unavailable for this request.
func (h *Handler) wsFor(p *proxy.Proxy) *websearch.Service {
	if p == nil || h.wsDriver != "zen" || !h.cfg.WebSearchEnabled {
		return nil
	}
	h.wsMu.Lock()
	client, ok := h.wsClients[p.ID]
	if !ok {
		dialer, err := upstream.NewSOCKS5Dialer(p.SOCKS5Addr)
		if err != nil {
			h.wsMu.Unlock()
			return nil
		}
		client = &http.Client{
			Transport: &http.Transport{Dial: dialer.Dial},
			Timeout:   30 * time.Second,
		}
		h.wsClients[p.ID] = client
	}
	h.wsMu.Unlock()

	return websearch.NewService(client,
		h.cfg.SearxngURL, h.cfg.BraveAPIKey,
		h.cfg.WebSearchMaxResults, h.cfg.WebSearchMaxPages, h.cfg.WebSearchMaxPageChar)
}

// execWebSearchTools runs every web_search call through the search service
// and returns parallel tool-result contents keyed by index of the call.
// Failures become error strings in the tool result — models recover better
// from an explicit error message than from a missing tool message.
func (h *Handler) execWebSearchTools(ctx context.Context, svc *websearch.Service, calls []map[string]any) []string {
	results := make([]string, len(calls))
	for i, c := range calls {
		fn, _ := c["function"].(map[string]any)
		args, _ := fn["arguments"].(string)
		content, err := svc.Run(ctx, args)
		if err != nil {
			h.log.Warn().Err(err).Msg("web_search tool failed")
			results[i] = fmt.Sprintf("web_search failed: %v", err)
			continue
		}
		results[i] = content
	}
	return results
}

// appendToolResults replays an assistant tool-call turn plus tool outputs
// onto the conversation, producing the next request body.
func appendToolResults(origBody []byte, calls []map[string]any, results []string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(origBody, &m); err != nil {
		return nil, err
	}
	msgs, _ := m["messages"].([]any)

	asst := map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": calls,
	}
	msgs = append(msgs, asst)

	for i, c := range calls {
		id, _ := c["id"].(string)
		msgs = append(msgs, map[string]any{
			"role":         "tool",
			"tool_call_id": id,
			"name":         toolCallName(c),
			"content":      results[i],
		})
	}

	m["messages"] = msgs
	return json.Marshal(m)
}

// streamResult captures what a streamed turn produced.
type streamResult struct {
	usage     upstream.Usage
	toolCalls []map[string]any // set only when our tool calls were intercepted
}

// doRound performs one upstream request for a tool round. On transport
// failure (e.g. the proxy's VPN tunnel died), it retries the same request on
// up to two fresh proxies from the pool — safe to do mid-conversation since
// intercepted rounds never reached the client.
func (h *Handler) doRound(ctx context.Context, p *proxy.Proxy, auth upstream.Auth, body []byte, stream bool) (*http.Response, error) {
	resp, _, err := h.client.Do(ctx, p, body, auth, stream)
	if err == nil && resp != nil {
		return resp, nil
	}
	h.log.Warn().Err(err).Str("proxy_id", p.ID).Msg("web_search round failed on proxy, trying fresh proxies")

	for i := 0; i < 2; i++ {
		actx, cancel := context.WithTimeout(ctx, h.cfg.RateLimitFreshIPWait)
		np, gerr := h.pool.GetProxy(actx, "")
		cancel()
		if gerr != nil || np == nil {
			break
		}
		resp, _, err = h.client.Do(ctx, np, body, auth, stream)
		if err == nil && resp != nil {
			return resp, nil
		}
	}
	return nil, fmt.Errorf("web_search round failed on all available proxies")
}

// streamWithToolRounds streams a completion, intercepting the gateway's
// web_search tool calls and running further rounds until the model produces
// a final answer (or WebSearchMaxRounds is hit). The client sees one
// seamless stream; intercepted tool-call chunks never reach it.
//
// firstResp is the already-acquired response for round 0 (the retry loop in
// handleChatCompletions made that call); later rounds are requested here.
func (h *Handler) streamWithToolRounds(w http.ResponseWriter, ctx context.Context, p *proxy.Proxy, auth upstream.Auth, body []byte, model string, includeUsage bool, firstResp *http.Response) upstream.Usage {
	svc := h.wsFor(p)
	cur := body
	var totalUsage upstream.Usage

	resp := firstResp
	for round := 0; ; round++ {
		if resp == nil {
			var err error
			resp, err = h.doRound(ctx, p, auth, cur, true)
			if err != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
				status := http.StatusBadGateway
				if resp != nil {
					status = resp.StatusCode
				}
				h.writeSSEErrorEvent(w, "The gateway could not complete the web search request.")
				h.log.Error().Err(err).Int("status", status).Int("round", round).Msg("web_search stream round failed")
				return totalUsage
			}
		}

		out := h.handleStreamResponse(w, resp, model, includeUsage, svc != nil)
		totalUsage.PromptTokens += out.usage.PromptTokens
		totalUsage.CompletionTokens += out.usage.CompletionTokens
		totalUsage.TotalTokens += out.usage.TotalTokens

		if len(out.toolCalls) == 0 || round >= h.cfg.WebSearchMaxRounds {
			return totalUsage
		}
		h.log.Info().Str("model", model).Int("round", round+1).Int("calls", len(out.toolCalls)).Msg("Executing web_search tool calls")

		results := h.execWebSearchTools(ctx, svc, out.toolCalls)
		next, err := appendToolResults(cur, out.toolCalls, results)
		if err != nil {
			h.writeSSEErrorEvent(w, "The gateway failed to continue after the web search.")
			return totalUsage
		}
		cur = next
		resp = nil
	}
}

// completeWithToolRounds is the non-streaming counterpart: intercepts
// web_search tool calls, executes them, and loops until a final answer,
// which is written once at the end. firstResp is the already-acquired
// response for round 0.
func (h *Handler) completeWithToolRounds(w http.ResponseWriter, ctx context.Context, p *proxy.Proxy, auth upstream.Auth, body []byte, firstResp *http.Response) upstream.Usage {
	svc := h.wsFor(p)
	cur := body
	var totalUsage upstream.Usage

	resp := firstResp
	for round := 0; ; round++ {
		if resp == nil {
			var err error
			resp, err = h.doRound(ctx, p, auth, cur, false)
			if err != nil || resp == nil {
				h.writeError(w, http.StatusBadGateway, "Request failed during web search")
				h.log.Error().Err(err).Msg("web_search non-stream round failed")
				return totalUsage
			}
		}
		raw, err := io.ReadAll(resp.Body)
		statusCode := resp.StatusCode
		headers := resp.Header
		resp.Body.Close()
		if err != nil {
			h.writeError(w, http.StatusBadGateway, "Failed to read upstream response")
			return totalUsage
		}

		var parsed struct {
			Choices []struct {
				Message map[string]any `json:"message"`
			} `json:"choices"`
			Usage *upstream.Usage `json:"usage"`
		}
		json.Unmarshal(raw, &parsed)

		var toolCalls []map[string]any
		if len(parsed.Choices) > 0 && parsed.Choices[0].Message != nil {
			toolCalls = messageToolCalls(parsed.Choices[0].Message)
		}
		interceptable := svc != nil && allWebSearch(toolCalls) && round < h.cfg.WebSearchMaxRounds &&
			statusCode >= 200 && statusCode < 300

		if parsed.Usage != nil {
			totalUsage.PromptTokens += parsed.Usage.PromptTokens
			totalUsage.CompletionTokens += parsed.Usage.CompletionTokens
			totalUsage.TotalTokens += parsed.Usage.TotalTokens
		}

		if !interceptable {
			for k, v := range headers {
				if len(v) > 0 && !shouldSkipHeader(k) {
					w.Header().Set(k, v[0])
				}
			}
			w.WriteHeader(statusCode)
			w.Write(raw)
			return totalUsage
		}

		h.log.Info().Int("round", round+1).Int("calls", len(toolCalls)).Msg("Executing web_search tool calls")
		results := h.execWebSearchTools(ctx, svc, toolCalls)
		next, aerr := appendToolResults(cur, toolCalls, results)
		if aerr != nil {
			for k, v := range headers {
				if len(v) > 0 && !shouldSkipHeader(k) {
					w.Header().Set(k, v[0])
				}
			}
			w.WriteHeader(statusCode)
			w.Write(raw)
			return totalUsage
		}
		cur = next
		resp = nil
	}
}
