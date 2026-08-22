package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// fetchTimeout bounds each individual page fetch.
const fetchTimeout = 15 * time.Second

// FetchHTTPClient is the minimal http.Client surface the providers need.
type FetchHTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// jsonUnmarshalStrict decodes JSON into v (helper kept tiny for testability).
func jsonUnmarshalStrict(data string, v any) error {
	return json.Unmarshal([]byte(data), v)
}

// userAgent mimics a browser so search engines don't block the default client.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"

// --- DuckDuckGo (keyless) ---

// DuckDuckGo scrapes html.duckduckgo.com results — no API key required.
// BaseURL is overridable for tests.
type DuckDuckGo struct {
	BaseURL string
	Client  FetchHTTPClient
}

func (d *DuckDuckGo) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	base := d.BaseURL
	if base == "" {
		base = "https://html.duckduckgo.com/html/"
	}
	form := url.Values{"q": {query}, "kl": {"wt-wt"}}
	req, err := http.NewRequestWithContext(ctx, "POST", base, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	res, err := d.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo returned status %d", res.StatusCode)
	}

	doc, err := html.Parse(res.Body)
	if err != nil {
		return nil, err
	}

	var results []Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			cls := classOf(n)
			href := attrOf(n, "href")
			switch {
			case strings.Contains(cls, "result__a"):
				title := textOf(n)
				if title != "" && href != "" {
					results = append(results, Result{Title: title, URL: unwrapDDGURL(href)})
				}
			case strings.Contains(cls, "result__snippet"):
				if len(results) > 0 && results[len(results)-1].Snippet == "" {
					results[len(results)-1].Snippet = textOf(n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return results, nil
}

// unwrapDDGURL resolves DDG's redirect links ("//duckduckgo.com/l/?uddg=...")
// to the real target URL.
func unwrapDDGURL(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if uddg := u.Query().Get("uddg"); uddg != "" {
		if real, err := url.QueryUnescape(uddg); err == nil {
			return real
		}
	}
	return href
}

// --- SearXNG (self-hosted) ---

// SearXNG queries a user-provided SearXNG instance via its JSON API.
type SearXNG struct {
	BaseURL string
	Client  FetchHTTPClient
}

func (s *SearXNG) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	u := fmt.Sprintf("%s/search?q=%s&format=json", s.BaseURL, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	res, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng returned status %d", res.StatusCode)
	}

	var body struct {
		Results []Result `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Results) > maxResults {
		body.Results = body.Results[:maxResults]
	}
	return body.Results, nil
}

// --- Brave Search API ---

// Brave uses the Brave Search API (requires an API key).
// BaseURL is overridable for tests.
type Brave struct {
	BaseURL string
	APIKey  string
	Client  FetchHTTPClient
}

func (b *Brave) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	base := b.BaseURL
	if base == "" {
		base = "https://api.search.brave.com/res/v1/web/search"
	}
	u := fmt.Sprintf("%s?q=%s&count=%d", base, url.QueryEscape(query), maxResults)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.APIKey)

	res, err := b.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave returned status %d", res.StatusCode)
	}

	var body struct {
		Web struct {
			Results []Result `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Web.Results, nil
}
