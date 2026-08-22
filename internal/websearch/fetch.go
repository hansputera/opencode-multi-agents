package websearch

import (
	"context"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/html"
)

// FetchPageText returns a FetchFunc that downloads url and extracts readable
// plain text (scripts/styles/nav stripped), truncated to maxChars.
func FetchPageText(client FetchHTTPClient, maxChars int) FetchFunc {
	return func(ctx context.Context, pageURL string) (string, error) {
		if !strings.HasPrefix(pageURL, "http://") && !strings.HasPrefix(pageURL, "https://") {
			return "", errUnsupportedScheme
		}
		req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")

		res, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return "", errNonOKStatus(res.StatusCode)
		}

		// Cap the read: huge pages shouldn't blow memory for a tool result.
		body := io.LimitReader(res.Body, 4<<20)
		text := ExtractText(body)
		if len(text) > maxChars {
			text = text[:maxChars] + "\n[...truncated...]"
		}
		return text, nil
	}
}

// ExtractText pulls visible-ish text out of an HTML document: script/style
// subtrees are dropped and runs of whitespace collapsed. Non-HTML input
// (plain text, JSON) passes through mostly unchanged.
func ExtractText(r io.Reader) string {
	doc, err := html.Parse(r)
	if err != nil {
		return ""
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "svg", "head":
				return
			case "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "section", "article":
				b.WriteString("\n")
			}
		}
		if n.Type == html.TextNode {
			s := strings.Join(strings.Fields(n.Data), " ")
			if s != "" {
				b.WriteString(s)
				b.WriteString(" ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return strings.TrimSpace(b.String())
}
