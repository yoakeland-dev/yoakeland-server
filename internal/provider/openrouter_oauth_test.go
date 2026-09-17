package provider_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"yoakeland/internal/provider"
)

func TestOpenRouterPKCE_ChallengeMatchesVerifier(t *testing.T) {
	pkce, err := provider.NewOpenRouterPKCE()
	if err != nil {
		t.Fatalf("NewOpenRouterPKCE: %v", err)
	}
	if pkce.Verifier == "" || pkce.Challenge == "" {
		t.Fatal("expected non-empty verifier and challenge")
	}

	sum := sha256.Sum256([]byte(pkce.Verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.Challenge != wantChallenge {
		t.Fatalf("challenge = %q, want sha256(verifier) = %q", pkce.Challenge, wantChallenge)
	}

	authURL := pkce.AuthURL("https://app.example.com/callback")
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("AuthURL produced an unparseable URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("callback_url") != "https://app.example.com/callback" {
		t.Fatalf("callback_url = %q", q.Get("callback_url"))
	}
	if q.Get("code_challenge") != pkce.Challenge {
		t.Fatalf("code_challenge in URL = %q, want %q", q.Get("code_challenge"), pkce.Challenge)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
}

func TestExchangeOpenRouterCode_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body["code"] != "auth-code-123" || body["code_verifier"] != "verifier-abc" {
			t.Fatalf("unexpected request body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"or-generated-key"}`))
	}))
	defer srv.Close()

	key, err := provider.ExchangeOpenRouterCode(context.Background(), srv.URL, "auth-code-123", "verifier-abc")
	if err != nil {
		t.Fatalf("ExchangeOpenRouterCode: %v", err)
	}
	if key != "or-generated-key" {
		t.Fatalf("key = %q, want %q", key, "or-generated-key")
	}
}

func TestExchangeOpenRouterCode_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	if _, err := provider.ExchangeOpenRouterCode(context.Background(), srv.URL, "bad-code", "verifier"); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
}

func TestOAuthSessionStore_BeginAndTakeIsSingleUse(t *testing.T) {
	store := provider.NewOAuthSessionStore()

	requestID, pkce, err := store.Begin("my-openrouter")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if requestID == "" || pkce == nil {
		t.Fatal("expected a non-empty requestID and pkce challenge")
	}

	pending, ok := store.Take(requestID)
	if !ok {
		t.Fatal("expected Take to find the pending login")
	}
	if pending.Name != "my-openrouter" || pending.Verifier != pkce.Verifier {
		t.Fatalf("unexpected pending login: %+v", pending)
	}

	if _, ok := store.Take(requestID); ok {
		t.Fatal("expected a second Take of the same requestID to fail (single use)")
	}
}

func TestOAuthSessionStore_TakeUnknownRequestID(t *testing.T) {
	store := provider.NewOAuthSessionStore()
	if _, ok := store.Take("never-issued"); ok {
		t.Fatal("expected Take to fail for an unknown request ID")
	}
}
