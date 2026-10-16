package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// authFake is an MCP server and its authorization server in one: the MCP
// endpoint wants a bearer token, and the OAuth endpoints issue one.
type authFake struct {
	srv       *httptest.Server
	registers atomic.Int32
	refreshes atomic.Int32
	challenge string // the code_challenge the last authorize carried
	token     string
	expiresIn int
	issuer    string
}

func newAuthFake(t *testing.T) *authFake {
	t.Helper()
	f := &authFake{token: "tok-1", expiresIn: 3600}
	mcpSrv := &fakeServer{era: "legacy"}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": f.srv.URL + "/mcp", "authorization_servers": []string{f.issuer}, "scopes_supported": []string{"read", "write"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.issuer, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"registration_endpoint": f.srv.URL + "/register", "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.registers.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["token_endpoint_auth_method"] != "none" || body["application_type"] != "native" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "client-xyz"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || r.Form.Get("resource") != f.srv.URL+"/mcp" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code or verifier"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.token, "token_type": "Bearer", "refresh_token": "ref-1", "expires_in": f.expiresIn})
		case "refresh_token":
			f.refreshes.Add(1)
			if r.Form.Get("refresh_token") != "ref-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			f.token = "tok-2"
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-2", "token_type": "Bearer", "expires_in": 3600})
		}
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.srv.URL+`/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var m message
		_ = json.NewDecoder(r.Body).Decode(&m)
		resp := mcpSrv.handle(&m)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return f
}

func TestOAuthFullFlow(t *testing.T) {
	f := newAuthFake(t)
	auth, _ := NewAuthStore(t.TempDir(), "")
	flow := NewFlow(auth)
	cfg := ServerConfig{URL: f.srv.URL + "/mcp"}

	// 1. Unauthenticated: the server says to sign in, and where.
	_, err := Dial(ctx5(t), Options{Config: cfg, Bearer: auth.Bearer(cfg.URL)})
	var ch *errNeedsAuth
	if !asNeedsAuth(err, &ch) {
		t.Fatalf("err = %v, want a sign-in challenge", err)
	}

	// 2. Begin: discovery, registration, and the page to open.
	redirect := "http://localhost:54321/callback"
	p, authURL, err := flow.Begin(ctx5(t), cfg, ch, redirect)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	f.challenge = q.Get("code_challenge")
	if u.Path != "/authorize" || q.Get("client_id") != "client-xyz" || q.Get("redirect_uri") != redirect || q.Get("code_challenge_method") != "S256" ||
		q.Get("resource") != f.srv.URL+"/mcp" || q.Get("response_type") != "code" || q.Get("state") == "" || q.Get("scope") != "read write" {
		t.Fatalf("authorization URL = %s", authURL)
	}
	if f.registers.Load() != 1 {
		t.Fatal("expected one dynamic registration")
	}

	// 3. A wrong state, or an answer from another issuer, is refused.
	taken, ok := flow.Take(p.ID)
	if !ok {
		t.Fatal("pending sign-in not found")
	}
	if err := flow.Complete(ctx5(t), taken, "the-code", "forged", ""); err == nil {
		t.Fatal("a wrong state must be refused")
	}
	if err := flow.Complete(ctx5(t), taken, "the-code", taken.State, "https://evil.example"); err == nil {
		t.Fatal("a different issuer (RFC 9207) must be refused")
	}
	if _, ok := flow.Take(p.ID); ok {
		t.Fatal("a sign-in can be taken only once")
	}

	// 4. The real answer stores tokens, and the server connects.
	if err := flow.Complete(ctx5(t), taken, "the-code", taken.State, f.issuer); err != nil {
		t.Fatal(err)
	}
	if !auth.SignedIn(cfg.URL) {
		t.Fatal("not signed in after a good answer")
	}
	c, err := Dial(ctx5(t), Options{Config: cfg, Bearer: auth.Bearer(cfg.URL)})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	// 5. Tokens are for this server's address only.
	if auth.SignedIn(f.srv.URL + "/other") {
		t.Fatal("a token must not apply to a different resource")
	}

	// 6. An expired access token is renewed, once, with the refresh token.
	e, _ := auth.get(cfg.URL)
	e.ExpiresAt = time.Now().Add(-time.Minute)
	_ = auth.put(cfg.URL, e)
	tok, err := auth.Bearer(cfg.URL)(ctx5(t))
	if err != nil || tok != "tok-2" || f.refreshes.Load() != 1 {
		t.Fatalf("renewed token = %q, %v (refreshes %d)", tok, err, f.refreshes.Load())
	}
	if tok2, _ := auth.Bearer(cfg.URL)(ctx5(t)); tok2 != "tok-2" || f.refreshes.Load() != 1 {
		t.Fatal("a fresh token must not be renewed again")
	}

	// 7. Sign out forgets everything.
	if err := auth.Forget(cfg.URL); err != nil || auth.SignedIn(cfg.URL) {
		t.Fatal("sign out failed")
	}
}

func asNeedsAuth(err error, out **errNeedsAuth) bool {
	for err != nil {
		if a, ok := err.(*errNeedsAuth); ok {
			*out = a
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestOAuthRevokedRefreshMeansSignInAgain(t *testing.T) {
	f := newAuthFake(t)
	auth, _ := NewAuthStore(t.TempDir(), "")
	cfg := ServerConfig{URL: f.srv.URL + "/mcp"}
	_ = auth.put(cfg.URL, tokenEntry{TokenEndpoint: f.srv.URL + "/token", ClientID: "client-xyz", Resource: authKey(cfg.URL),
		AccessToken: "old", RefreshToken: "revoked", ExpiresAt: time.Now().Add(-time.Hour)})
	_, err := auth.Bearer(cfg.URL)(ctx5(t))
	var ch *errNeedsAuth
	if !asNeedsAuth(err, &ch) {
		t.Fatalf("err = %v, want a sign-in challenge", err)
	}
	if auth.SignedIn(cfg.URL) {
		t.Fatal("dead tokens should be dropped")
	}
}

func TestOAuthRefusesRemoteMetadataAimedAtPrivateAddresses(t *testing.T) {
	for _, tc := range []struct {
		server, target string
		ok             bool
	}{
		{"https://mcp.example.com", "https://auth.example.com/token", true},
		{"https://mcp.example.com", "http://auth.example.com/token", false},      // plain http
		{"https://mcp.example.com", "https://user:pw@auth.example.com/t", false}, // credentials in the URL
		{"http://localhost:8080/mcp", "http://localhost:8080/token", true},       // a local dev server may talk http
		{"https://mcp.example.com", "http://169.254.169.254/latest", false},
		{"http://localhost:8080/mcp", "http://example.com/token", false},
	} {
		if got := validEndpoint(tc.server, tc.target); got != tc.ok {
			t.Errorf("validEndpoint(%s, %s) = %v, want %v", tc.server, tc.target, got, tc.ok)
		}
	}
	for ip, private := range map[string]bool{"127.0.0.1": true, "10.1.2.3": true, "192.168.0.9": true, "169.254.169.254": true, "::1": true, "8.8.8.8": false, "100.100.100.200": true} {
		if got := isPrivateIP(netIP(ip)); got != private {
			t.Errorf("isPrivateIP(%s) = %v, want %v", ip, got, private)
		}
	}
	// A public-looking server's own client refuses to dial loopback.
	hc := oauthClient("https://mcp.example.com/mcp")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("dialed a private address") }))
	defer srv.Close()
	if resp, err := hc.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("the OAuth client dialed a loopback address on a public server's say-so")
	} else if !strings.Contains(err.Error(), "private address") {
		t.Fatalf("err = %v", err)
	}
}

func TestOAuthRedirectMustBeLocal(t *testing.T) {
	f := newAuthFake(t)
	auth, _ := NewAuthStore(t.TempDir(), "")
	flow := NewFlow(auth)
	for _, bad := range []string{"https://evil.example/cb", "http://evil.example:80/cb", "javascript:alert(1)", "http://localhost.evil.example/cb", ""} {
		if _, _, err := flow.Begin(context.Background(), ServerConfig{URL: f.srv.URL + "/mcp"}, nil, bad); err == nil {
			t.Errorf("redirect %q must be refused", bad)
		}
	}
}

func TestOAuthPreRegisteredClient(t *testing.T) {
	f := newAuthFake(t)
	auth, _ := NewAuthStore(t.TempDir(), "")
	flow := NewFlow(auth)
	cfg := ServerConfig{URL: f.srv.URL + "/mcp", OAuth: &OAuthConfig{ClientID: "mine", Scope: "repo"}}
	p, authURL, err := flow.Begin(ctx5(t), cfg, nil, "http://127.0.0.1:1/callback")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(authURL)
	if p.ClientID != "mine" || q.Query().Get("client_id") != "mine" || q.Query().Get("scope") != "repo" || f.registers.Load() != 0 {
		t.Fatalf("a configured client must be used without registering: %s", authURL)
	}
}

func TestAuthStoreIsPrivateAndPersistent(t *testing.T) {
	dir := t.TempDir()
	a, _ := NewAuthStore(dir, "")
	_ = a.put("https://x.example/mcp/", tokenEntry{AccessToken: "t", RefreshToken: "r"})
	b, err := NewAuthStore(dir, "")
	if err != nil || !b.SignedIn("https://X.example/mcp") {
		t.Fatalf("reload: %v, signed in = %v (the key is the canonical URL)", err, b.SignedIn("https://X.example/mcp"))
	}
}

func netIP(s string) net.IP { return net.ParseIP(s) }

// A server that says its answers name the issuer (RFC 9207) must name it.
func TestOAuthAnswerMustNameIssuerWhenServerSaysSo(t *testing.T) {
	p := &Pending{State: "s", Issuer: "https://as.example", IssRequired: true}
	if err := (&Flow{}).Complete(context.Background(), p, "code", "s", ""); err == nil || !strings.Contains(err.Error(), "RFC 9207") {
		t.Fatalf("an answer without iss must be refused, got %v", err)
	}
	if err := (&Flow{}).Complete(context.Background(), p, "code", "s", "https://elsewhere.example"); err == nil || strings.Contains(err.Error(), "RFC 9207") {
		t.Fatalf("an answer from another issuer must be refused, got %v", err)
	}
}
