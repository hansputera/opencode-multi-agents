package websearch

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

var errUnsupportedScheme = errors.New("only http and https URLs are supported")

func errNonOKStatus(status int) error {
	return fmt.Errorf("page returned status %d", status)
}

// attrOf returns the named attribute of a node ("" when absent).
func attrOf(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// classOf returns the class attribute of a node.
func classOf(n *html.Node) string {
	return attrOf(n, "class")
}

// textOf concatenates all descendant text of a node.
func textOf(n *html.Node) string {
	var b []byte
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			b = append(b, c.Data...)
		}
		for ch := c.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return strings.TrimSpace(string(b))
}
