package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchTool_Execute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subscription-Token"); got != "test-key" {
			t.Errorf("missing/wrong API key header: %q", got)
		}
		if got := r.URL.Query().Get("q"); got != "yoakeland" {
			t.Errorf("query = %q, want %q", got, "yoakeland")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"web":{"results":[{"title":"Yoakeland","url":"https://example.com","description":"a thing"}]}}`))
	}))
	defer srv.Close()

	wt := NewWebSearchTool("test-key")
	wt.baseURL = srv.URL

	out, err := wt.Execute(context.Background(), ExecutionContext{}, []byte(`{"query":"yoakeland"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Yoakeland") || !strings.Contains(out, "https://example.com") {
		t.Errorf("output missing expected result: %q", out)
	}
}

func TestWebSearchTool_Execute_EmptyQuery(t *testing.T) {
	wt := NewWebSearchTool("test-key")
	if _, err := wt.Execute(context.Background(), ExecutionContext{}, []byte(`{"query":"  "}`)); err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestWebSearchTool_Execute_DDGFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "yoakeland" {
			t.Errorf("query = %q, want %q", got, "yoakeland")
		}
		w.Write([]byte(`<html><body><div class="results">
			<div class="web-result">
				<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa">Result A</a>
				<a class="result__snippet">snippet a</a>
			</div>
			<div class="web-result">
				<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fb">Result B</a>
				<a class="result__snippet">snippet b</a>
			</div>
		</div></body></html>`))
	}))
	defer srv.Close()

	wt := NewWebSearchTool("") // no key -> DDG fallback
	wt.ddgBaseURL = srv.URL

	out, err := wt.Execute(context.Background(), ExecutionContext{}, []byte(`{"query":"yoakeland"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Result A") || !strings.Contains(out, "https://example.com/a") || !strings.Contains(out, "snippet b") {
		t.Errorf("output missing expected result: %q", out)
	}
}

func TestWebSearchTool_Execute_DDGBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><div class="anomaly-modal">are you a robot?</div></body></html>`))
	}))
	defer srv.Close()

	wt := NewWebSearchTool("")
	wt.ddgBaseURL = srv.URL

	if _, err := wt.Execute(context.Background(), ExecutionContext{}, []byte(`{"query":"yoakeland"}`)); err == nil {
		t.Fatal("expected error when DuckDuckGo returns a bot-check page")
	}
}
