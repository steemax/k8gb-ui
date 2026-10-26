package console

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookieName = "__Host-k8gb-session"
	loginCookieName   = "__Host-k8gb-oidc"
	loginLifetime     = 5 * time.Minute
	sessionLifetime   = time.Hour
	authHTTPTimeout   = 10 * time.Second
	maxCookieValue    = 3800
)

type authenticator struct {
	mode           string
	key            []byte
	issuer         string
	redirectOrigin string
	groupsClaim    string
	allowedGroups  map[string]bool
	oauth          oauth2.Config
	verifier       *oidc.IDTokenVerifier
	client         *http.Client
	builtin        *builtinAuth
}

type loginState struct {
	State     string `json:"state"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	ExpiresAt int64  `json:"exp"`
}

type authSession struct {
	Subject   string   `json:"sub"`
	Groups    []string `json:"groups,omitempty"`
	ExpiresAt int64    `json:"exp"`
	AuthMode  string   `json:"auth,omitempty"`
	Version   string   `json:"cv,omitempty"`
}

func newAuthenticator(ctx context.Context, cfg AuthConfig) (*authenticator, error) {
	a := &authenticator{mode: cfg.Mode}
	if cfg.Mode == "local" {
		// Config.Validate enforces the literal loopback bind for this mode.
		return a, nil
	}
	if cfg.Mode == "builtin" {
		return newBuiltinAuthenticator(cfg)
	}
	if cfg.Mode != "oidc" {
		return nil, errors.New("authentication mode must be local, builtin or oidc")
	}
	issuer, err := authHTTPSURL(cfg.IssuerURL)
	if err != nil || issuer.RawQuery != "" || issuer.ForceQuery {
		return nil, errors.New("invalid OIDC issuer URL")
	}
	redirect, err := authHTTPSURL(cfg.RedirectURL)
	if err != nil || redirect.Path != "/auth/callback" || redirect.RawQuery != "" || redirect.ForceQuery {
		return nil, errors.New("invalid OIDC redirect URL")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("OIDC client ID is required")
	}
	key, err := os.ReadFile(cfg.CookieKeyFile)
	if err != nil || len(key) < 32 {
		return nil, errors.New("OIDC cookie key must contain at least 32 raw bytes")
	}
	secret := ""
	if cfg.ClientSecretFile != "" {
		raw, err := os.ReadFile(cfg.ClientSecretFile)
		if err != nil {
			return nil, errors.New("cannot read OIDC client secret")
		}
		secret = strings.TrimSpace(string(raw))
		if secret == "" {
			return nil, errors.New("OIDC client secret is empty")
		}
	}
	a.key, a.issuer, a.redirectOrigin = key, cfg.IssuerURL, authOrigin(redirect)
	a.groupsClaim = cfg.GroupsClaim
	if a.groupsClaim == "" {
		a.groupsClaim = "groups"
	}
	a.allowedGroups = make(map[string]bool)
	for _, binding := range cfg.Bindings {
		for _, group := range binding.Groups {
			if group != "" {
				a.allowedGroups[group] = true
			}
		}
	}
	// Use normal certificate verification and do not follow backend redirects,
	// which could otherwise forward a code or client secret to another endpoint.
	a.client = &http.Client{
		Timeout:       authHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if cfg.IssuerCAFile != "" {
		pem, err := os.ReadFile(cfg.IssuerCAFile)
		if err != nil {
			return nil, errors.New("cannot read OIDC issuer CA file")
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, errors.New("cannot load system certificate pool for OIDC")
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("OIDC issuer CA file contains no certificates")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		a.client.Transport = transport
	}
	clientCtx := oidc.ClientContext(ctx, a.client)
	provider, err := oidc.NewProvider(clientCtx, cfg.IssuerURL)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	endpoint := provider.Endpoint()
	var metadata struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, errors.New("invalid OIDC discovery metadata")
	}
	for _, rawURL := range []string{endpoint.AuthURL, endpoint.TokenURL, metadata.JWKSURL} {
		if _, err := authHTTPSURL(rawURL); err != nil {
			return nil, errors.New("OIDC endpoints must use HTTPS")
		}
	}
	a.oauth = oauth2.Config{
		ClientID: cfg.ClientID, ClientSecret: secret, RedirectURL: cfg.RedirectURL,
		Endpoint: endpoint, Scopes: []string{oidc.ScopeOpenID},
	}
	a.verifier = provider.VerifierContext(clientCtx, &oidc.Config{ClientID: cfg.ClientID})
	return a, nil
}

func authHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") || u.Opaque != "" {
		return nil, errors.New("invalid HTTPS URL")
	}
	return u, nil
}

func authOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return "https://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (a *authenticator) Principal(r *http.Request) (Principal, bool) {
	if a.mode == "local" {
		return Principal{Subject: "local"}, true
	}
	if a.mode != "oidc" && a.mode != "builtin" {
		return Principal{}, false
	}
	var session authSession
	if !a.readCookie(r, sessionCookieName, &session) || session.ExpiresAt <= time.Now().Unix() || strings.TrimSpace(session.Subject) == "" {
		return Principal{}, false
	}
	if a.mode == "builtin" {
		return a.builtinPrincipal(session)
	}
	if session.AuthMode != "" && session.AuthMode != "oidc" {
		return Principal{}, false
	}
	return Principal{Subject: session.Subject, Groups: a.boundGroups(session.Groups)}, true
}

func (a *authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if a.mode == "builtin" {
		a.builtinLogin(w, r)
		return
	}
	if !authMethod(w, r, http.MethodGet) {
		return
	}
	if a.mode == "local" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if a.mode != "oidc" || a.verifier == nil {
		authFailure(w, http.StatusServiceUnavailable)
		return
	}
	state, err := randomAuthValue()
	if err != nil {
		authFailure(w, http.StatusInternalServerError)
		return
	}
	nonce, err := randomAuthValue()
	if err != nil {
		authFailure(w, http.StatusInternalServerError)
		return
	}
	verifier, err := randomAuthValue()
	if err != nil {
		authFailure(w, http.StatusInternalServerError)
		return
	}
	expiry := time.Now().Add(loginLifetime).Unix()
	value, err := a.seal(loginCookieName, loginState{State: state, Nonce: nonce, Verifier: verifier, ExpiresAt: expiry})
	if err != nil {
		authFailure(w, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, authCookie(loginCookieName, value, expiry))
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (a *authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	if !authMethod(w, r, http.MethodGet) {
		return
	}
	if a.mode == "local" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	expireAuthCookie(w, loginCookieName)
	query, err := url.ParseQuery(r.URL.RawQuery)
	var state loginState
	if a.mode != "oidc" || a.verifier == nil || err != nil || query.Has("error") ||
		len(query["state"]) != 1 || len(query["code"]) != 1 || query.Get("code") == "" ||
		!a.readCookie(r, loginCookieName, &state) || state.ExpiresAt <= time.Now().Unix() ||
		state.State == "" || state.Nonce == "" || state.Verifier == "" ||
		!hmac.Equal([]byte(query.Get("state")), []byte(state.State)) {
		authFailure(w, http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), a.client), authHTTPTimeout)
	defer cancel()
	token, err := a.oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(state.Verifier))
	if err != nil {
		authFailure(w, http.StatusUnauthorized)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		authFailure(w, http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil || idToken.Issuer != a.issuer || strings.TrimSpace(idToken.Subject) == "" ||
		!hmac.Equal([]byte(idToken.Nonce), []byte(state.Nonce)) {
		authFailure(w, http.StatusUnauthorized)
		return
	}
	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		authFailure(w, http.StatusUnauthorized)
		return
	}
	var groups []string
	if raw, exists := claims[a.groupsClaim]; exists {
		// Only a top-level JSON array of strings is accepted, never paths,
		// distributed claims, headers, or a comma-separated string.
		var values []any
		if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &values) != nil {
			authFailure(w, http.StatusUnauthorized)
			return
		}
		for _, value := range values {
			group, ok := value.(string)
			if !ok {
				authFailure(w, http.StatusUnauthorized)
				return
			}
			groups = append(groups, group)
		}
	}
	now := time.Now()
	expiry := now.Add(sessionLifetime)
	if idToken.Expiry.Before(expiry) {
		expiry = idToken.Expiry
	}
	if expiry.Unix() <= now.Unix() {
		authFailure(w, http.StatusUnauthorized)
		return
	}
	session := authSession{Subject: idToken.Subject, Groups: a.boundGroups(groups), ExpiresAt: expiry.Unix()}
	value, err := a.seal(sessionCookieName, session)
	if err != nil {
		authFailure(w, http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, authCookie(sessionCookieName, value, session.ExpiresAt))
	slog.Info("console authentication succeeded")
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	if !authMethod(w, r, http.MethodPost) {
		return
	}
	if a.mode != "local" {
		if !a.sameOrigin(r) {
			authFailure(w, http.StatusForbidden)
			return
		}
	}
	sites := r.Header.Values("Sec-Fetch-Site")
	site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if len(sites) > 1 || (site != "" && site != "same-origin" && site != "same-site" && site != "none") {
		authFailure(w, http.StatusForbidden)
		return
	}
	expireAuthCookie(w, sessionCookieName)
	expireAuthCookie(w, loginCookieName)
	if a.mode == "builtin" {
		expireAuthCookie(w, builtinLoginCookie)
	}
	slog.Info("console logout succeeded")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *authenticator) sameOrigin(r *http.Request) bool {
	if a.mode != "oidc" && a.mode != "builtin" {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return false
	}
	origin, err := authHTTPSURL(origins[0])
	if err != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || authOrigin(origin) != a.redirectOrigin {
		return false
	}
	sites := r.Header.Values("Sec-Fetch-Site")
	site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	return len(sites) <= 1 && (site == "" || site == "same-origin" || site == "same-site" || site == "none")
}

func (a *authenticator) boundGroups(groups []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, group := range groups {
		if a.allowedGroups[group] && !seen[group] {
			result = append(result, group)
			seen[group] = true
		}
	}
	return result
}

func randomAuthValue() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (a *authenticator) seal(name string, value any) (string, error) {
	if len(a.key) < 32 {
		return "", errors.New("invalid cookie key")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(name + "\x00" + payload))
	sealed := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(sealed) > maxCookieValue {
		return "", errors.New("session exceeds cookie limit")
	}
	return sealed, nil
}

func (a *authenticator) readCookie(r *http.Request, name string, value any) bool {
	var raw string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			raw = cookie.Value
			count++
		}
	}
	if count != 1 || len(raw) > maxCookieValue || len(a.key) < 32 {
		return false
	}
	payload, signature, ok := strings.Cut(raw, ".")
	if !ok {
		return false
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(sig) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(name + "\x00" + payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	return err == nil && json.Unmarshal(decoded, value) == nil
}

func authCookie(name, value string, expiry int64) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Expires: time.Unix(expiry, 0).UTC(),
		MaxAge: int(expiry - time.Now().Unix()),
	}
}

func expireAuthCookie(w http.ResponseWriter, name string) {
	cookie := authCookie(name, "", 1)
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

func authMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func authFailure(w http.ResponseWriter, status int) {
	// Never log provider errors, request URLs, claims, codes, or cookies.
	slog.Warn("console authentication failed", "status", status)
	http.Error(w, "authentication failed", status)
}
