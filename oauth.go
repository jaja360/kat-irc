package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const issuer = "https://auth.openai.com"
const tokenURL = issuer + "/api/accounts/oauth/token"
const planScope = "chatgpt.tokens.use.direct"

var authHTTP = &http.Client{Timeout: 30 * time.Second}

type Credentials struct {
	ClientID     string    `json:"client_id"`
	HostID       string    `json:"ext_agent_host_id"`
	Subject      string    `json:"subject"`
	Email        string    `json:"email"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	Scope        string    `json:"scope"`
	ExpiresAt    time.Time `json:"expires_at"`
}
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
}

func hasScope(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}
func readCredentials(path string) (*Credentials, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	var c Credentials
	if e = json.Unmarshal(b, &c); e != nil {
		return nil, e
	}
	if c.ClientID == "" || c.ClientID == "dynamic_agent_client" || c.Subject == "" || c.AccessToken == "" || !hasScope(c.Scope, planScope) {
		return nil, fmt.Errorf("invalid OAuth credentials or ChatGPT plan permission missing")
	}
	return &c, nil
}
func exchange(ctx context.Context, values url.Values) (tokenResponse, error) {
	var t tokenResponse
	req, e := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(values.Encode()))
	if e != nil {
		return t, e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, e := authHTTP.Do(req)
	if e != nil {
		return t, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return t, fmt.Errorf("OAuth HTTP %d; renew login if session revoked or expired", res.StatusCode)
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&t); e != nil {
		return t, e
	}
	if t.AccessToken == "" || !strings.EqualFold(t.TokenType, "Bearer") || t.ExpiresIn <= 0 {
		return t, fmt.Errorf("invalid OAuth token response")
	}
	return t, nil
}
func oauthToken(ctx context.Context, path string) (string, error) {
	// Reload after locking: login/models/serve must never refresh stale rotating tokens.
	unlock, e := waitLock(ctx, path)
	if e != nil {
		return "", e
	}
	defer unlock()
	c, e := readCredentials(path)
	if e != nil {
		return "", e
	}
	if time.Until(c.ExpiresAt) > 2*time.Minute {
		return c.AccessToken, nil
	}
	if c.RefreshToken == "" {
		return "", fmt.Errorf("OAuth refresh token absent: run login")
	}
	t, e := exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.RefreshToken}, "resource": {apiBase}})
	if e != nil {
		return "", e
	}
	if t.Scope == "" {
		t.Scope = c.Scope
	}
	if !hasScope(t.Scope, planScope) {
		return "", fmt.Errorf("ChatGPT plan permission revoked")
	}
	c.AccessToken = t.AccessToken
	c.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	c.Scope = t.Scope
	if t.RefreshToken != "" {
		c.RefreshToken = t.RefreshToken
	}
	if t.IDToken != "" {
		c.IDToken = t.IDToken
	}
	if e = atomicJSON(path, c); e != nil {
		return "", fmt.Errorf("persist refreshed OAuth session: %w", e)
	}
	return c.AccessToken, nil
}
func randomString() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func hostID(path string) (string, error) {
	b, e := os.ReadFile(path)
	if e == nil {
		var id string
		e = json.Unmarshal(b, &id)
		if e != nil || !strings.HasPrefix(id, "urn:uuid:") {
			return "", fmt.Errorf("invalid host ID")
		}
		return id, nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	var u [16]byte
	if _, e = rand.Read(u[:]); e != nil {
		return "", e
	}
	u[6] = (u[6] & 15) | 64
	u[8] = (u[8] & 63) | 128
	id := fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	return id, atomicJSON(path, id)
}
func login(ctx context.Context, path string, port int) error {
	// Login is intentionally a local, operator-only CLI command, never an IRC command.
	host, e := hostID(filepath.Join(filepath.Dir(path), "host-id.json"))
	if e != nil {
		return e
	}
	old, e := readCredentials(path)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	clientID := "dynamic_agent_client"
	if old != nil {
		clientID = old.ClientID
	}
	ln, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return e
	}
	redirect := "http://" + ln.Addr().String() + "/auth/callback"
	state, nonce, verifier := randomString(), randomString(), randomString()
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {clientID}, "ext_agent_host_id": {host}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {"openid profile email offline_access resource.invoke " + planScope}, "resource": {apiBase}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}}
	if old == nil {
		q.Set("agent_name_hint", "kat-irc")
	}
	// Do not print id_token_hint or any existing token in the login URL.
	result := make(chan url.Values, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		if r.Method != "GET" || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid login state", 400)
			return
		}
		accepted := false
		once.Do(func() { result <- r.URL.Query(); accepted = true })
		if !accepted {
			http.Error(w, "Login already consumed", 400)
			return
		}
		fmt.Fprintln(w, "Callback received. Return to the terminal for the verification result.")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer srv.Close()
	go srv.Serve(ln)
	fmt.Println("Continue with ChatGPT — open this URL in your local browser:")
	fmt.Println(issuer + "/api/accounts/authorize?" + q.Encode())
	fmt.Println("This grants kat-irc access to your plan's shared usage. Manage access and limits in ChatGPT Settings. No API billing fallback.")
	var cb url.Values
	select {
	case cb = <-result:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("login timed out")
	}
	if cb.Get("error") != "" {
		return fmt.Errorf("OAuth consent declined or sign-in failed")
	}
	if cb.Get("code") == "" {
		return fmt.Errorf("callback missing code")
	}
	if old == nil {
		clientID = cb.Get("client_id")
		if clientID == "" || clientID == "dynamic_agent_client" {
			return fmt.Errorf("callback missing issued client ID")
		}
	} else if id := cb.Get("client_id"); id != "" && id != clientID {
		return fmt.Errorf("callback client ID mismatch")
	}
	ctx = oidc.ClientContext(ctx, authHTTP)
	t, e := exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {cb.Get("code")}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {apiBase}})
	if e != nil {
		return e
	}
	provider, e := oidc.NewProvider(ctx, issuer)
	if e != nil {
		return fmt.Errorf("OIDC discovery: %w", e)
	}
	id, e := provider.Verifier(&oidc.Config{ClientID: clientID}).Verify(ctx, t.IDToken)
	if e != nil {
		return fmt.Errorf("ID token verification failed")
	}
	if id.Nonce != nonce || id.Subject == "" {
		return fmt.Errorf("ID token nonce or subject invalid")
	}
	if old != nil && id.Subject != old.Subject {
		return fmt.Errorf("selected account does not match existing credentials; use a separate credential path for another account")
	}
	if !hasScope(t.Scope, planScope) {
		return fmt.Errorf("ChatGPT plan permission was not granted")
	}
	var claims struct {
		Email string `json:"email"`
	}
	if e = id.Claims(&claims); e != nil {
		return e
	}
	c := &Credentials{ClientID: clientID, HostID: host, Subject: id.Subject, Email: claims.Email, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, IDToken: t.IDToken, Scope: t.Scope, ExpiresAt: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)}
	if e = atomicJSON(path, c); e != nil {
		return e
	}
	fmt.Printf("ChatGPT connected: %s. Credentials saved. Use models to list available models.\n", c.Email)
	return nil
}
