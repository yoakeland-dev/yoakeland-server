package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Sign-in to a remote server follows the MCP authorization spec: OAuth 2.1
// with PKCE, the server's protected-resource metadata (RFC 9728) to find its
// authorization server, that server's metadata (RFC 8414 or OpenID), dynamic
// client registration (RFC 7591) unless the user registered a client, and a
// resource indicator (RFC 8707) binding the token to the one MCP server.
//
// The browser's redirect goes to a loopback address on the *client's* machine
// (the daemon may be elsewhere), which hands the code back to the daemon; the
// daemon exchanges it, so a token only ever reaches the daemon.

// tokenEntry is what is stored for one MCP server URL.
type tokenEntry struct {
	Issuer        string    `json:"issuer"`
	TokenEndpoint string    `json:"tokenEndpoint"`
	ClientID      string    `json:"clientId"`
	ClientSecret  string    `json:"clientSecret,omitempty"`
	Resource      string    `json:"resource"`
	AccessToken   string    `json:"accessToken,omitempty"`
	RefreshToken  string    `json:"refreshToken,omitempty"`
	ExpiresAt     time.Time `json:"expiresAt,omitempty"`
	Scope         string    `json:"scope,omitempty"`
}

// AuthStore keeps the OAuth tokens in <state-dir>/mcp-auth.json (0600),
// keyed by the server's URL: a token is good for one resource, so it can't be
// sent to a different server that happens to share a name.
type AuthStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]tokenEntry
	hc      *http.Client // tests replace it
	refresh sync.Mutex   // one refresh at a time (refresh tokens rotate)
}

func NewAuthStore(dir string) (*AuthStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &AuthStore{path: filepath.Join(dir, "mcp-auth.json"), entries: map[string]tokenEntry{}}
	data, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &a.entries); err != nil {
		return nil, fmt.Errorf("%s doesn't parse (fix or move it): %w", a.path, err)
	}
	return a, nil
}

func (a *AuthStore) saveLocked() error {
	b, err := json.MarshalIndent(a.entries, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.path, b)
}

// key is the canonical form of a server URL.
func authKey(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	u.Fragment, u.RawQuery, u.User = "", "", nil
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

// SignedIn reports whether there is a token for the server.
func (a *AuthStore) SignedIn(serverURL string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[authKey(serverURL)]
	return ok && (e.AccessToken != "" || e.RefreshToken != "")
}

// Forget signs out of a server.
func (a *AuthStore) Forget(serverURL string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.entries, authKey(serverURL))
	return a.saveLocked()
}

func (a *AuthStore) put(serverURL string, e tokenEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, had := a.entries[authKey(serverURL)]
	a.entries[authKey(serverURL)] = e
	if err := a.saveLocked(); err != nil {
		if had {
			a.entries[authKey(serverURL)] = prev
		} else {
			delete(a.entries, authKey(serverURL))
		}
		return err
	}
	return nil
}

func (a *AuthStore) get(serverURL string) (tokenEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[authKey(serverURL)]
	return e, ok
}

// Bearer returns a function giving the server's current access token, renewed
// with the refresh token when it is about to lapse; "" with no error when the
// user hasn't signed in.
func (a *AuthStore) Bearer(serverURL string) bearerFunc {
	return func(ctx context.Context) (string, error) {
		e, ok := a.get(serverURL)
		if !ok || (e.AccessToken == "" && e.RefreshToken == "") {
			return "", nil
		}
		if e.AccessToken != "" && (e.ExpiresAt.IsZero() || time.Until(e.ExpiresAt) > time.Minute) {
			return e.AccessToken, nil
		}
		if e.RefreshToken == "" {
			return e.AccessToken, nil // expired, and nothing to renew it with: let the server say so
		}
		a.refresh.Lock()
		defer a.refresh.Unlock()
		if cur, ok := a.get(serverURL); ok && cur.AccessToken != e.AccessToken { // someone else renewed it
			return cur.AccessToken, nil
		}
		next, err := a.renew(ctx, serverURL, e)
		if err != nil {
			return "", err
		}
		return next.AccessToken, nil
	}
}

func (a *AuthStore) renew(ctx context.Context, serverURL string, e tokenEntry) (tokenEntry, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {e.RefreshToken}, "client_id": {e.ClientID}, "resource": {e.Resource}}
	tok, err := a.tokenRequest(ctx, serverURL, e.TokenEndpoint, e.ClientID, e.ClientSecret, form)
	if err != nil {
		// A refresh the server refuses means the grant is gone: sign in again.
		var oe *oauthError
		if errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Code == "invalid_token") {
			e.AccessToken, e.RefreshToken = "", ""
			_ = a.put(serverURL, e)
			return e, &errNeedsAuth{Detail: "the sign-in expired"}
		}
		return e, err
	}
	e.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		e.RefreshToken = tok.RefreshToken
	}
	e.ExpiresAt = tok.expiry()
	if tok.Scope != "" {
		e.Scope = tok.Scope
	}
	return e, a.put(serverURL, e)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (t tokenResponse) expiry() time.Time {
	if t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

// oauthError is an error response from an authorization server.
type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

func (a *AuthStore) client(serverURL string) *http.Client {
	if a.hc != nil {
		return a.hc
	}
	return oauthClient(serverURL)
}

func (a *AuthStore) tokenRequest(ctx context.Context, serverURL, endpoint, clientID, secret string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if secret != "" {
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(secret))
	}
	resp, err := a.client(serverURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var oe oauthError
		if json.Unmarshal(body, &oe) == nil && oe.Code != "" {
			return nil, &oe
		}
		return nil, fmt.Errorf("the token endpoint answered HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("the token endpoint's answer has no access token: %s", snippet(body))
	}
	if t := strings.ToLower(tok.TokenType); t != "" && t != "bearer" {
		return nil, fmt.Errorf("the token endpoint issued a %q token, not a bearer one", tok.TokenType)
	}
	return &tok, nil
}

// oauthClient fetches discovery documents and tokens. Everything it is told
// to fetch came from the remote server's own metadata, so unless that server is
// itself on a private network it may not send us to one (a metadata document
// naming a token endpoint at 169.254.169.254 or localhost is an attack, not a
// configuration).
func oauthClient(serverURL string) *http.Client {
	allowPrivate := false
	if u, err := url.Parse(serverURL); err == nil {
		allowPrivate = isPrivateHost(u.Hostname())
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	if !allowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
				return fmt.Errorf("refusing to connect to a private address (%s) named by a remote server", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: d.DialContext, TLSHandshakeTimeout: 15 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !allowPrivate && req.URL.Scheme != "https" {
				return errors.New("refusing a redirect to plain http")
			}
			return nil
		},
	}
}

func isPrivateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.ParseIP("100.100.100.200"))
}

// --- the sign-in flow ---

// Pending is a sign-in begun and awaiting the browser's answer.
type Pending struct {
	ID            string
	ServerURL     string
	Verifier      string
	State         string
	Redirect      string
	Issuer        string
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
	Resource      string
	Scope         string
	Expires       time.Time
}

// Flow runs sign-ins, holding the ones in progress.
type Flow struct {
	auth *AuthStore

	mu      sync.Mutex
	pending map[string]*Pending
}

func NewFlow(auth *AuthStore) *Flow { return &Flow{auth: auth, pending: map[string]*Pending{}} }

type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	ScopesSupported       []string `json:"scopes_supported"`
	IssParam              bool     `json:"authorization_response_iss_parameter_supported"`
}

func (f *Flow) getJSON(ctx context.Context, serverURL, target string, out any) error {
	if !validEndpoint(serverURL, target) {
		return fmt.Errorf("refusing to fetch %s", target)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.auth.client(serverURL).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpError{Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// validEndpoint: https, or plain http only when the MCP server is itself local.
func validEndpoint(serverURL, target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	s, err := url.Parse(serverURL)
	return err == nil && u.Scheme == "http" && isPrivateHost(s.Hostname()) && isPrivateHost(u.Hostname())
}

// discover finds the authorization server for serverURL.
func (f *Flow) discover(ctx context.Context, serverURL string, ch *errNeedsAuth) (*asMetadata, []string, error) {
	su, err := url.Parse(serverURL)
	if err != nil {
		return nil, nil, err
	}
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	candidates := []string{}
	if ch != nil && ch.Resource != "" {
		candidates = append(candidates, ch.Resource)
	}
	root := su.Scheme + "://" + su.Host
	if p := strings.Trim(su.Path, "/"); p != "" {
		candidates = append(candidates, root+"/.well-known/oauth-protected-resource/"+p)
	}
	candidates = append(candidates, root+"/.well-known/oauth-protected-resource")
	issuer := root
	var scopes []string
	for _, c := range candidates {
		if f.getJSON(ctx, serverURL, c, &prm) == nil && len(prm.AuthorizationServers) > 0 {
			issuer, scopes = prm.AuthorizationServers[0], prm.ScopesSupported
			break
		}
	}
	iu, err := url.Parse(issuer)
	if err != nil || iu.Host == "" {
		return nil, nil, fmt.Errorf("the server named an invalid authorization server %q", issuer)
	}
	base := iu.Scheme + "://" + iu.Host
	path := strings.Trim(iu.Path, "/")
	var urls []string
	if path != "" {
		urls = []string{base + "/.well-known/oauth-authorization-server/" + path, base + "/.well-known/openid-configuration/" + path, base + "/" + path + "/.well-known/openid-configuration"}
	} else {
		urls = []string{base + "/.well-known/oauth-authorization-server", base + "/.well-known/openid-configuration"}
	}
	var md asMetadata
	var found bool
	for _, u := range urls {
		md = asMetadata{}
		if f.getJSON(ctx, serverURL, u, &md) == nil && md.AuthorizationEndpoint != "" && md.TokenEndpoint != "" {
			found = true
			break
		}
	}
	if !found {
		// Servers from before metadata discovery existed use fixed paths.
		if len(prm.AuthorizationServers) > 0 {
			return nil, nil, fmt.Errorf("couldn't read the authorization server's metadata at %s", issuer)
		}
		md = asMetadata{Issuer: root, AuthorizationEndpoint: root + "/authorize", TokenEndpoint: root + "/token", RegistrationEndpoint: root + "/register"}
	}
	if md.Issuer == "" {
		md.Issuer = issuer
	}
	if len(md.CodeChallengeMethods) > 0 {
		ok := false
		for _, m := range md.CodeChallengeMethods {
			ok = ok || m == "S256"
		}
		if !ok {
			return nil, nil, errors.New("the authorization server doesn't support PKCE with S256, which is required")
		}
	}
	for _, e := range []string{md.AuthorizationEndpoint, md.TokenEndpoint} {
		if !validEndpoint(serverURL, e) {
			return nil, nil, fmt.Errorf("the authorization server named an endpoint I won't use: %s", e)
		}
	}
	return &md, scopes, nil
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Begin starts a sign-in to serverURL and returns the page to open. redirect is
// where the browser will send the code: a loopback http address.
func (f *Flow) Begin(ctx context.Context, cfg ServerConfig, ch *errNeedsAuth, redirect string) (*Pending, string, error) {
	ru, err := url.Parse(redirect)
	if err != nil || ru.Scheme != "http" || !(ru.Hostname() == "localhost" || ru.Hostname() == "127.0.0.1" || ru.Hostname() == "::1" || ru.Hostname() == "[::1]") {
		return nil, "", errors.New("the sign-in must come back to a local address")
	}
	md, scopes, err := f.discover(ctx, cfg.URL, ch)
	if err != nil {
		return nil, "", err
	}
	scope := ""
	switch {
	case cfg.OAuth != nil && cfg.OAuth.Scope != "":
		scope = cfg.OAuth.Scope
	case ch != nil && ch.Scope != "":
		scope = ch.Scope
	case len(scopes) > 0:
		scope = strings.Join(scopes, " ")
	}
	p := &Pending{ID: randToken(12), ServerURL: cfg.URL, Verifier: randToken(32), State: randToken(16), Redirect: redirect,
		Issuer: md.Issuer, TokenEndpoint: md.TokenEndpoint, Resource: authKey(cfg.URL), Scope: scope, Expires: time.Now().Add(10 * time.Minute)}
	if cfg.OAuth != nil {
		p.ClientID, p.ClientSecret = cfg.OAuth.ClientID, cfg.OAuth.ClientSecret
	} else if prev, ok := f.auth.get(cfg.URL); ok && prev.ClientID != "" && prev.Issuer == md.Issuer {
		p.ClientID, p.ClientSecret = prev.ClientID, prev.ClientSecret
	}
	if p.ClientID == "" {
		if md.RegistrationEndpoint == "" {
			return nil, "", errors.New("this server's sign-in has no dynamic client registration; register a client with it and give its clientId in the server's oauth settings")
		}
		if !validEndpoint(cfg.URL, md.RegistrationEndpoint) {
			return nil, "", fmt.Errorf("the authorization server named a registration endpoint I won't use: %s", md.RegistrationEndpoint)
		}
		if p.ClientID, p.ClientSecret, err = f.register(ctx, cfg.URL, md.RegistrationEndpoint, redirect); err != nil {
			return nil, "", err
		}
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	au, _ := url.Parse(md.AuthorizationEndpoint)
	q := au.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", p.State)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("resource", p.Resource)
	if scope != "" {
		q.Set("scope", scope)
	}
	au.RawQuery = q.Encode()

	f.mu.Lock()
	for id, old := range f.pending {
		if time.Now().After(old.Expires) {
			delete(f.pending, id)
		}
	}
	f.pending[p.ID] = p
	f.mu.Unlock()
	return p, au.String(), nil
}

func (f *Flow) register(ctx context.Context, serverURL, endpoint, redirect string) (id, secret string, err error) {
	body, _ := json.Marshal(map[string]any{
		"client_name": "Yoakeland", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none", "application_type": "native",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := f.auth.client(serverURL).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("client registration was refused (HTTP %d): %s", resp.StatusCode, snippet(data))
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.ClientID == "" {
		return "", "", fmt.Errorf("client registration returned no client id: %s", snippet(data))
	}
	return out.ClientID, out.ClientSecret, nil
}

// Take removes and returns a pending sign-in, so a code is only ever redeemed once.
func (f *Flow) Take(id string) (*Pending, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pending[id]
	delete(f.pending, id)
	if ok && time.Now().After(p.Expires) {
		return nil, false
	}
	return p, ok
}

// Complete redeems the code the browser brought back and stores the tokens.
// iss is the issuer the redirect named, if it named one (RFC 9207).
func (f *Flow) Complete(ctx context.Context, p *Pending, code, state, iss string) error {
	if state != p.State {
		return errors.New("the sign-in answer doesn't match the request (state)")
	}
	if iss != "" && iss != p.Issuer {
		return errors.New("the sign-in answer came from a different authorization server than the one asked")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {p.Redirect},
		"client_id": {p.ClientID}, "code_verifier": {p.Verifier}, "resource": {p.Resource}}
	tok, err := f.auth.tokenRequest(ctx, p.ServerURL, p.TokenEndpoint, p.ClientID, p.ClientSecret, form)
	if err != nil {
		return err
	}
	scope := tok.Scope
	if scope == "" {
		scope = p.Scope
	}
	return f.auth.put(p.ServerURL, tokenEntry{Issuer: p.Issuer, TokenEndpoint: p.TokenEndpoint, ClientID: p.ClientID, ClientSecret: p.ClientSecret,
		Resource: p.Resource, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: tok.expiry(), Scope: scope})
}
