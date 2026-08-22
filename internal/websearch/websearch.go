// Package websearch implements the gateway's built-in web_search tool:
// pluggable search providers, a page fetcher that extracts readable text,
// and an executor that formats tool results for the model.
//
// All outbound HTTP goes through the caller-supplied http.Client, which the
// handler wires to the assigned proxy container's SOCKS5 tunnel — so search
// queries and page fetches egress from the same rotating VPN IP as model
// traffic.
package websearch

import (
	"context"
	"fmt"
	"strings"
)

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// Provider executes a web search and returns up to maxResults hits.
type Provider interface {
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
}

// FetchFunc downloads a URL and returns its extracted plain text.
type FetchFunc func(ctx context.Context, url string) (string, error)

// Service runs web_search tool calls end to end: query → results → page
// extraction → formatted tool-result content.
type Service struct {
	provider     Provider
	fetch        FetchFunc
	maxResults   int
	maxPages     int
	maxPageChars int
}

// NewService picks a provider based on configuration: a SearXNG instance if
// searxngURL is set, else Brave if apiKey is set, else keyless DuckDuckGo.
func NewService(httpClient FetchHTTPClient, searxngURL, braveAPIKey string, maxResults, maxPages, maxPageChars int) *Service {
	if httpClient == nil {
		return nil
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxPages < 0 {
		maxPages = 2
	}
	if maxPageChars <= 0 {
		maxPageChars = 6000
	}

	var p Provider
	switch {
	case strings.TrimSpace(searxngURL) != "":
		p = &SearXNG{BaseURL: strings.TrimRight(searxngURL, "/"), Client: httpClient}
	case strings.TrimSpace(braveAPIKey) != "":
		p = &Brave{APIKey: braveAPIKey, Client: httpClient}
	default:
		p = &DuckDuckGo{Client: httpClient}
	}

	return &Service{
		provider:     p,
		fetch:        FetchPageText(httpClient, maxPageChars),
		maxResults:   maxResults,
		maxPages:     maxPages,
		maxPageChars: maxPageChars,
	}
}

// RunArgs is the JSON schema of the web_search function parameters.
type RunArgs struct {
	Query string `json:"query"`
}

// Run executes a web_search call and returns the tool-result content string
// handed back to the model.
func (s *Service) Run(ctx context.Context, argumentsJSON string) (string, error) {
	var args RunArgs
	if err := jsonUnmarshalStrict(argumentsJSON, &args); err != nil || strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("invalid web_search arguments: need a non-empty \"query\"")
	}

	results, err := s.provider.Search(ctx, args.Query, s.maxResults)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}
	if len(results) == 0 {
		return "No results found.", nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Web results for %q:\n\n", args.Query))

	for i, r := range results {
		fmt.Fprintf(&b, "[%d] %s\n    %s\n", i+1, r.Title, r.URL)
		if sn := strings.TrimSpace(r.Snippet); sn != "" {
			fmt.Fprintf(&b, "    %s\n", oneLine(sn))
		}
		b.WriteString("\n")
	}

	// Extract full text from the top pages so the model can cite content
	// rather than just snippets.
	for i, r := range results {
		if i >= s.maxPages {
			break
		}
		pageCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
		text, ferr := s.fetch(pageCtx, r.URL)
		cancel()
		if ferr != nil || strings.TrimSpace(text) == "" {
			continue
		}
		fmt.Fprintf(&b, "--- Content of [%d] %s ---\n%s\n---\n", i+1, r.URL, text)
	}

	return b.String(), nil
}

// ToolDefinition returns the OpenAI function-tool schema advertised to the
// model. Shared by the handler when injecting into request bodies.
func ToolDefinition() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "web_search",
			"description": "Search the public web for current information. Returns ranked results with titles, URLs, snippets, and the extracted text of the top pages.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query. Use focused keywords; issue multiple searches for broad topics.",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// oneLine collapses whitespace so snippets render as single lines.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
