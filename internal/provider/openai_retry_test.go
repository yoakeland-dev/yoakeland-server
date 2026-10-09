package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPostRetriesOn429(t *testing.T) {
	old := rateLimitBackoff
	rateLimitBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { rateLimitBackoff = old }()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()

	p := &OpenAICompatibleProvider{BaseURL: srv.URL, Client: srv.Client()}
	resp, err := p.post(t.Context(), []byte(`{}`))
	if err != nil || resp.StatusCode != 200 || calls != 3 {
		t.Fatalf("status=%v err=%v calls=%d, want 200 after 3 calls", resp, err, calls)
	}

	// Out of retries: the last 429 is returned for the caller to report.
	calls = -100
	resp, err = p.post(t.Context(), []byte(`{}`))
	if err != nil || resp.StatusCode != 429 || calls != -97 {
		t.Fatalf("status=%v err=%v calls=%d, want 429 after 3 attempts", resp, err, calls)
	}
}
