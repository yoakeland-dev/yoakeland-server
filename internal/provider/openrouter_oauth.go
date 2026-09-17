package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
)

const (
	openRouterAuthURL     = "https://openrouter.ai/auth"
	openRouterKeyExchange = "https://openrouter.ai/api/v1/auth/keys"
)

// OpenRouterPKCE is one PKCE challenge/verifier pair for OpenRouter's OAuth
// flow (https://openrouter.ai/docs/use-cases/oauth-pkce): no client secret,
// the user authorizes on openrouter.ai and the exchange hands back a usable
// API key scoped to their account.
type OpenRouterPKCE struct {
	Verifier  string
	Challenge string
}

func NewOpenRouterPKCE() (*OpenRouterPKCE, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return &OpenRouterPKCE{Verifier: verifier, Challenge: challenge}, nil
}

// AuthURL is the page to send the user's browser to; OpenRouter redirects
// back to callbackURL with a `code` query parameter on approval.
func (p *OpenRouterPKCE) AuthURL(callbackURL string) string {
	v := url.Values{}
	v.Set("callback_url", callbackURL)
	v.Set("code_challenge", p.Challenge)
	v.Set("code_challenge_method", "S256")
	return openRouterAuthURL + "?" + v.Encode()
}

// ExchangeOpenRouterCode swaps an authorization code for a usable OpenRouter
// API key. baseURL overrides the exchange endpoint for tests; production
// callers pass "".
func ExchangeOpenRouterCode(ctx context.Context, baseURL, code, verifier string) (string, error) {
	if baseURL == "" {
		baseURL = openRouterKeyExchange
	}

	body, err := json.Marshal(map[string]string{
		"code":                  code,
		"code_verifier":         verifier,
		"code_challenge_method": "S256",
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter key exchange failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("malformed openrouter key exchange response: %w", err)
	}
	if result.Key == "" {
		return "", fmt.Errorf("openrouter key exchange response carried no key")
	}
	return result.Key, nil
}

// PendingOAuth is one in-flight login: the verifier needed to complete the
// exchange, and the provider name to register once it does.
type PendingOAuth struct {
	Verifier string
	Name     string
}

// OAuthSessionStore tracks in-flight OAuth logins between the `start` and
// `complete` IPC calls. Deliberately in-memory only (not persisted): a
// pending login that outlives a process restart is just started over.
type OAuthSessionStore struct {
	mu      sync.Mutex
	pending map[string]PendingOAuth
}

func NewOAuthSessionStore() *OAuthSessionStore {
	return &OAuthSessionStore{pending: make(map[string]PendingOAuth)}
}

// Begin starts a new login for the given provider name, returning a request
// ID the client must echo back in the `complete` step alongside the code.
func (s *OAuthSessionStore) Begin(name string) (requestID string, pkce *OpenRouterPKCE, err error) {
	pkce, err = NewOpenRouterPKCE()
	if err != nil {
		return "", nil, err
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, err
	}
	requestID = hex.EncodeToString(idBytes)

	s.mu.Lock()
	s.pending[requestID] = PendingOAuth{Verifier: pkce.Verifier, Name: name}
	s.mu.Unlock()
	return requestID, pkce, nil
}

// Take retrieves and removes a pending login by request ID (single use).
func (s *OAuthSessionStore) Take(requestID string) (PendingOAuth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
	}
	return p, ok
}
