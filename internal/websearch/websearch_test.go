package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubClient returns a FetchHTTPClient hitting an httptest server.
func stubClient(s *httptest.Server) FetchHTTPClient {
	return s.Client()
}

const ddgFixture = `
<html><body>
<div class="result">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fweather&amp;rut=abc">Weather today</a>
  <a class="result__snippet">Sunny skies with a high of 25 degrees.</a>
</div>
<div class="result">
  <a class="result__a" href="https://news.example.com/storm">Storm warning</a>
  <a class="result__snippet">Heavy rain expected tomorrow morning.</a>
</div>
</body></html>`

func TestDuckDuckGoParse(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("q") != "weather" {
			t.Errorf("unexpected query form: %v", r.Form)
		}
		w.Write([]byte(ddgFixture))
	}))
	defer s.Close()

	d := &DuckDuckGo{BaseURL: s.URL, Client: stubClient(s)}
	results, err := d.Search(context.Background(), "weather", 5)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(results), results)
	}
	if results[0].Title != "Weather today" {
		t.Errorf("title = %q", results[0].Title)
	}
	// The redirect wrapper must be unwrapped to the real URL.
	if results[0].URL != "https://example.com/weather" {
		t.Errorf("url = %q, want unwrapped https://example.com/weather", results[0].URL)
	}
	if !strings.Contains(results[0].Snippet, "Sunny") {
		t.Errorf("snippet = %q", results[0].Snippet)
	}
}

func TestSearXNGParse(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("missing format=json")
		}
		w.Write([]byte(`{"results":[{"title":"A","url":"https://a.example","content":"first"},{"title":"B","url":"https://b.example","content":"second"}]}`))
	}))
	defer s.Close()

	p := &SearXNG{BaseURL: s.URL, Client: stubClient(s)}
	results, err := p.Search(context.Background(), "q", 1)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 || results[0].Title != "A" {
		t.Errorf("results = %+v", results)
	}
}

func TestBraveParse(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "key-123" {
			t.Errorf("missing subscription token")
		}
		w.Write([]byte(`{"web":{"results":[{"title":"R","url":"https://r.example","description":"desc"}]}}`))
	}))
	defer s.Close()

	p := &Brave{BaseURL: s.URL, APIKey: "key-123", Client: stubClient(s)}
	results, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 || results[0].URL != "https://r.example" {
		t.Errorf("results = %+v", results)
	}
}

func TestUnwrapDDGURL(t *testing.T) {
	cases := map[string]string{
		"https://direct.example.com/x":                   "https://direct.example.com/x",
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fa.b%2Fc": "https://a.b/c",
	}
	for in, want := range cases {
		if got := unwrapDDGURL(in); got != want {
			t.Errorf("unwrapDDGURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServiceRun(t *testing.T) {
	fetched := false
	svc := &Service{
		provider: &fakeProvider{results: []Result{
			{Title: "Result One", URL: "https://one.example/page", Snippet: "snip one"},
			{Title: "Result Two", URL: "https://two.example/page", Snippet: "snip two"},
		}},
		fetch: func(ctx context.Context, url string) (string, error) {
			fetched = true
			return "Full page content here.", nil
		},
		maxResults:   5,
		maxPages:     2,
		maxPageChars: 100,
	}

	out, err := svc.Run(context.Background(), `{"query":"test query"}`)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for _, want := range []string{"test query", "[1] Result One", "https://one.example/page", "snip one", "Content of [1]", "Full page content here."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !fetched {
		t.Error("page fetch never happened")
	}

	// Bad arguments produce an error, not a panic.
	if _, err := svc.Run(context.Background(), `{}`); err == nil {
		t.Error("expected error for empty query")
	}
}

func TestExtractText(t *testing.T) {
	html := `<html><head><style>.x{color:red}</style></head>
	<body><script>evil()</script><h1>Hello</h1><p>World   of   text</p></body></html>`
	got := ExtractText(strings.NewReader(html))
	if strings.Contains(got, "evil") || strings.Contains(got, "color") {
		t.Errorf("script/style leaked: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World of text") {
		t.Errorf("text missing: %q", got)
	}
}

type fakeProvider struct{ results []Result }

func (f *fakeProvider) Search(ctx context.Context, q string, max int) ([]Result, error) {
	return f.results, nil
}
