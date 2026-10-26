package console

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type authTestProvider struct {
	server        *httptest.Server
	key           *rsa.PrivateKey
	mu            sync.Mutex
	claims        map[string]any
	metadata      map[string]any
	challenge     string
	tokenRequests int
	lastForm      url.Values
	lastSecret    string
	tokenStatus   int
	omitIDToken   bool
	rawIDToken    string
}

func newAuthTestProvider(t *testing.T) *authTestProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &authTestProvider{key: key, metadata: make(map[string]any)}
	p.server = httptest.NewUnstartedServer(http.HandlerFunc(p.serveHTTP))
	p.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	p.server.StartTLS()
	t.Cleanup(p.server.Close)
	// Trust only the test server's CA, without disabling TLS verification.
	transport := http.DefaultTransport
	http.DefaultTransport = p.server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })
	p.claims = map[string]any{
		"iss": p.server.URL, "aud": "console-client", "sub": "stable-subject",
		"iat": time.Now().Unix(), "exp": time.Now().Add(2 * time.Hour).Unix(),
		"email": "private@example.test", "name": "Private Name",
		"groups": []string{"unbound", "operators", "operators"},
	}
	return p
}

func (p *authTestProvider) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		metadata := map[string]any{
			"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize",
			"token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		for name, value := range p.metadata {
			metadata[name] = value
		}
		_ = json.NewEncoder(w).Encode(metadata)
	case "/keys":
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "kid": "test-key", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(p.key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.PublicKey.E)).Bytes()),
		}}})
	case "/token":
		p.tokenRequests++
		if err := r.ParseForm(); err != nil {
			http.Error(w, "private provider error", http.StatusBadRequest)
			return
		}
		p.lastForm = r.Form
		_, p.lastSecret, _ = r.BasicAuth()
		if p.lastSecret == "" {
			p.lastSecret = r.Form.Get("client_secret")
		}
		if p.tokenStatus != 0 || r.Method != http.MethodPost || r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("code") != "valid-code" || r.Form.Get("redirect_uri") != "https://console.example/auth/callback" ||
			oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != p.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"private provider error valid-code private@example.test"}`)
			return
		}
		token := p.rawIDToken
		if token == "" {
			var err error
			token, err = signAuthTestJWT(p.key, p.claims)
			if err != nil {
				http.Error(w, "cannot sign test token", http.StatusInternalServerError)
				return
			}
		}
		response := map[string]any{
			"access_token": "server-only-access-token", "refresh_token": "server-only-refresh-token",
			"token_type": "Bearer", "expires_in": 7200,
		}
		if !p.omitIDToken {
			response["id_token"] = token
		}
		_ = json.NewEncoder(w).Encode(response)
	default:
		http.NotFound(w, r)
	}
}

func signAuthTestJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (p *authTestProvider) config(t *testing.T) AuthConfig {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "cookie-key")
	if err := os.WriteFile(keyPath, append(bytes.Repeat([]byte{0x91}, 31), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return AuthConfig{
		Mode: "oidc", IssuerURL: p.server.URL, ClientID: "console-client",
		RedirectURL: "https://console.example/auth/callback", CookieKeyFile: keyPath,
		Bindings: []Binding{{Groups: []string{"operators"}, Clusters: []string{"*"}, Namespaces: []string{"*"}}},
	}
}

func (p *authTestProvider) authenticator(t *testing.T) *authenticator {
	t.Helper()
	a, err := newAuthenticator(context.Background(), p.config(t))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func authTestCookie(t *testing.T, rr *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("missing cookie %s", name)
	return nil
}

func tamperAuthTestValue(value string) string {
	if value[0] == 'A' {
		return "B" + value[1:]
	}
	return "A" + value[1:]
}

func (p *authTestProvider) begin(t *testing.T, a *authenticator) (*http.Cookie, loginState) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://console.example/auth/login?return_url=https://evil.example", nil)
	rr := httptest.NewRecorder()
	a.Login(rr, r)
	if rr.Code != http.StatusFound {
		t.Fatalf("login status = %d", rr.Code)
	}
	cookie := authTestCookie(t, rr, loginCookieName)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge <= 0 || cookie.MaxAge > 300 {
		t.Fatalf("unsafe login cookie attributes: %+v", cookie)
	}
	request := httptest.NewRequest(http.MethodGet, "https://console.example", nil)
	request.AddCookie(cookie)
	var state loginState
	if !a.readCookie(request, loginCookieName, &state) {
		t.Fatal("cannot read login cookie")
	}
	location, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	if location.Scheme+"://"+location.Host+location.Path != p.server.URL+"/authorize" ||
		query.Get("state") != state.State || query.Get("nonce") != state.Nonce ||
		query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(state.Verifier) ||
		query.Get("redirect_uri") != a.oauth.RedirectURL || query.Get("response_type") != "code" ||
		query.Get("scope") != "openid" || query.Get("code_verifier") != "" || query.Get("client_secret") != "" {
		t.Fatalf("incorrect authorization request: %s", location)
	}
	if len(state.State) != 43 || len(state.Nonce) != 43 || len(state.Verifier) != 43 || state.State == state.Nonce || state.State == state.Verifier {
		t.Fatal("login secrets must be independent random values")
	}
	if rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("authentication responses must not be cached or leak referrers")
	}
	p.mu.Lock()
	p.claims["nonce"] = state.Nonce
	p.challenge = query.Get("code_challenge")
	p.mu.Unlock()
	return cookie, state
}

func (p *authTestProvider) callback(a *authenticator, cookie *http.Cookie, state string) *httptest.ResponseRecorder {
	query := url.Values{"code": {"valid-code"}, "state": {state}, "return_url": {"https://evil.example"}}
	r := httptest.NewRequest(http.MethodGet, "https://console.example/auth/callback?"+query.Encode(), nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	a.Callback(rr, r)
	return rr
}

func TestAuthenticatorLocalAndInvalidModes(t *testing.T) {
	a, err := newAuthenticator(context.Background(), AuthConfig{Mode: "local", IssuerURL: "invalid"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	r.Header.Set("X-Forwarded-User", "attacker")
	p, ok := a.Principal(r)
	if !ok || p.Subject != "local" || len(p.Groups) != 0 {
		t.Fatalf("unexpected local principal: %+v, %v", p, ok)
	}
	for _, mode := range []string{"", "header", "OIDC"} {
		if _, err := newAuthenticator(context.Background(), AuthConfig{Mode: mode}); err == nil {
			t.Fatalf("accepted mode %q", mode)
		}
		if _, ok := (&authenticator{mode: mode}).Principal(r); ok {
			t.Fatalf("invalid mode %q bypassed authentication", mode)
		}
	}
}

func TestOIDCAuthenticationFlow(t *testing.T) {
	p := newAuthTestProvider(t)
	cfg := p.config(t)
	cfg.GroupsClaim = "roles"
	secretPath := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretPath, []byte("server-only-client-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.ClientSecretFile = secretPath
	a, err := newAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.key) != 32 || a.key[31] != '\n' || a.client.Timeout != 10*time.Second {
		t.Fatal("key must be raw and backend client must have a bounded timeout")
	}
	if a.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("backend redirects must not forward credentials")
	}
	for _, lifetime := range []time.Duration{2 * time.Hour, 2 * time.Minute} {
		t.Run(lifetime.String(), func(t *testing.T) {
			cookie, state := p.begin(t, a)
			groups := []string{"operators", "operators"}
			for i := 0; i < 2000; i++ {
				groups = append(groups, "unreferenced-group-"+strconv.Itoa(i))
			}
			expiry := time.Now().Add(lifetime).Unix()
			p.mu.Lock()
			p.claims["roles"] = groups
			p.claims["groups"] = []string{"wrong-claim"}
			p.claims["exp"] = expiry
			p.mu.Unlock()
			before := time.Now().Unix()
			rr := p.callback(a, cookie, state.State)
			if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/" {
				t.Fatalf("callback status = %d, location = %q", rr.Code, rr.Header().Get("Location"))
			}
			sessionCookie := authTestCookie(t, rr, sessionCookieName)
			if !sessionCookie.Secure || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Path != "/" || sessionCookie.Domain != "" || len(sessionCookie.String()) > 4096 {
				t.Fatal("unsafe session cookie attributes or size")
			}
			if sessionCookie.MaxAge <= 0 || sessionCookie.MaxAge > 3600 || sessionCookie.Expires.Unix() > expiry || sessionCookie.Expires.Unix() > time.Now().Add(time.Hour).Unix() {
				t.Fatal("session outlives its permitted lifetime")
			}
			if lifetime < time.Hour && sessionCookie.Expires.Unix() != expiry {
				t.Fatal("shorter ID token lifetime was not retained")
			}
			if lifetime > time.Hour && sessionCookie.Expires.Unix() < before+3600 {
				t.Fatal("session lifetime was unexpectedly shortened")
			}
			if authTestCookie(t, rr, loginCookieName).MaxAge != -1 {
				t.Fatal("callback did not clear login state")
			}
			r := httptest.NewRequest(http.MethodGet, "https://console.example/api/v1/session", nil)
			r.AddCookie(sessionCookie)
			r.Header.Set("X-Forwarded-User", "spoofed-subject")
			r.Header.Set("X-Auth-Request-Email", "spoofed@example.test")
			principal, ok := a.Principal(r)
			if !ok || principal.Subject != "stable-subject" || !reflect.DeepEqual(principal.Groups, []string{"operators"}) {
				t.Fatalf("unexpected principal: %+v, %v", principal, ok)
			}
			payload, _, _ := strings.Cut(sessionCookie.Value, ".")
			decoded, err := base64.RawURLEncoding.DecodeString(payload)
			if err != nil {
				t.Fatal(err)
			}
			var stored map[string]json.RawMessage
			if err := json.Unmarshal(decoded, &stored); err != nil || len(stored) != 3 || stored["sub"] == nil || stored["groups"] == nil || stored["exp"] == nil {
				t.Fatal("session must contain only subject, bound groups, and expiry")
			}
			for _, forbidden := range []string{"private@example.test", "Private Name", "server-only-", "unreferenced-", "wrong-claim", state.Verifier, state.Nonce} {
				if strings.Contains(string(decoded), forbidden) || strings.Contains(rr.Body.String(), forbidden) || strings.Contains(rr.Header().Get("Location"), forbidden) {
					t.Fatalf("browser response exposed %q", forbidden)
				}
			}
			p.mu.Lock()
			if p.lastForm.Get("code_verifier") != state.Verifier || p.lastSecret != "server-only-client-secret" {
				t.Error("token exchange did not carry the PKCE verifier and client secret")
			}
			p.mu.Unlock()
		})
	}
}

func TestOIDCCallbackRejectsInvalidState(t *testing.T) {
	p := newAuthTestProvider(t)
	a := p.authenticator(t)
	for _, name := range []string{"missing cookie", "tampered cookie", "wrong state", "expired state", "empty nonce", "empty verifier", "duplicate cookie"} {
		t.Run(name, func(t *testing.T) {
			cookie, state := p.begin(t, a)
			stateValue := state.State
			switch name {
			case "missing cookie":
				cookie = nil
			case "tampered cookie":
				cookie.Value = tamperAuthTestValue(cookie.Value)
			case "wrong state":
				stateValue = "forged-state"
			case "expired state":
				state.ExpiresAt = time.Now().Add(-time.Second).Unix()
			case "empty nonce":
				state.Nonce = ""
			case "empty verifier":
				state.Verifier = ""
			}
			if name == "expired state" || name == "empty nonce" || name == "empty verifier" {
				var err error
				cookie.Value, err = a.seal(loginCookieName, state)
				if err != nil {
					t.Fatal(err)
				}
			}
			p.mu.Lock()
			before := p.tokenRequests
			p.mu.Unlock()
			var rr *httptest.ResponseRecorder
			if name == "duplicate cookie" {
				r := httptest.NewRequest(http.MethodGet, "https://console.example/auth/callback?code=valid-code&state="+stateValue, nil)
				r.AddCookie(cookie)
				r.AddCookie(cookie)
				rr = httptest.NewRecorder()
				a.Callback(rr, r)
			} else {
				rr = p.callback(a, cookie, stateValue)
			}
			assertAuthTestFailure(t, rr, http.StatusUnauthorized)
			if authTestCookie(t, rr, loginCookieName).MaxAge != -1 {
				t.Fatal("invalid callback did not discard state")
			}
			p.mu.Lock()
			if p.tokenRequests != before {
				t.Error("invalid state reached the token endpoint")
			}
			p.mu.Unlock()
		})
	}
	for _, query := range []string{"", "state=s", "code=c", "code=c&state=s&state=s", "code=c&code=c&state=s", "error=private-details&state=s", "code=%zz&state=s"} {
		r := httptest.NewRequest(http.MethodGet, "https://console.example/auth/callback?"+query, nil)
		rr := httptest.NewRecorder()
		a.Callback(rr, r)
		assertAuthTestFailure(t, rr, http.StatusUnauthorized)
	}
	_, first := p.begin(t, a)
	_, second := p.begin(t, a)
	if first.State == second.State || first.Nonce == second.Nonce || first.Verifier == second.Verifier {
		t.Fatal("successive logins reused secrets")
	}
}

func assertAuthTestFailure(t *testing.T, rr *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rr.Code != status || rr.Body.String() != "authentication failed\n" || rr.Header().Get("Location") != "" {
		t.Fatalf("unexpected error response: %d %q", rr.Code, rr.Body.String())
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.MaxAge != -1 {
			t.Fatal("failed authentication issued a session")
		}
	}
}

func TestOIDCCallbackVerifiesIDToken(t *testing.T) {
	p := newAuthTestProvider(t)
	a := p.authenticator(t)
	for _, name := range []string{"issuer", "audience", "expired", "missing expiry", "nonce", "missing nonce", "empty subject", "missing subject with email", "signature", "missing token", "groups string", "groups object", "groups null", "groups mixed", "groups null element", "cookie too large"} {
		t.Run(name, func(t *testing.T) {
			cookie, state := p.begin(t, a)
			p.mu.Lock()
			original := make(map[string]any)
			for key, value := range p.claims {
				original[key] = value
			}
			switch name {
			case "issuer":
				p.claims["iss"] = "https://other.example"
			case "audience":
				p.claims["aud"] = "another-client"
			case "expired":
				p.claims["exp"] = time.Now().Add(-time.Second).Unix()
			case "missing expiry":
				delete(p.claims, "exp")
			case "nonce":
				p.claims["nonce"] = "not-the-browser-nonce"
			case "missing nonce":
				delete(p.claims, "nonce")
			case "empty subject":
				p.claims["sub"] = ""
			case "missing subject with email":
				delete(p.claims, "sub")
			case "signature":
				token, err := signAuthTestJWT(p.key, p.claims)
				if err != nil {
					p.mu.Unlock()
					t.Fatal(err)
				}
				parts := strings.Split(token, ".")
				parts[2] = tamperAuthTestValue(parts[2])
				p.rawIDToken = strings.Join(parts, ".")
			case "missing token":
				p.omitIDToken = true
			case "groups string":
				p.claims["groups"] = "operators"
			case "groups object":
				p.claims["groups"] = map[string]any{"name": "operators"}
			case "groups null":
				p.claims["groups"] = nil
			case "groups mixed":
				p.claims["groups"] = []any{"operators", 1}
			case "groups null element":
				p.claims["groups"] = []any{"operators", nil}
			case "cookie too large":
				p.claims["sub"] = strings.Repeat("s", 5000)
			}
			p.mu.Unlock()
			rr := p.callback(a, cookie, state.State)
			assertAuthTestFailure(t, rr, http.StatusUnauthorized)
			p.mu.Lock()
			p.claims, p.rawIDToken, p.omitIDToken = original, "", false
			p.mu.Unlock()
		})
	}
	// A missing group claim is safe: subject bindings can still authorize it.
	cookie, state := p.begin(t, a)
	p.mu.Lock()
	delete(p.claims, "groups")
	p.mu.Unlock()
	rr := p.callback(a, cookie, state.State)
	if rr.Code != http.StatusFound {
		t.Fatalf("subject-only identity was rejected: %d", rr.Code)
	}
}

func TestPrincipalVerifiesSessionEveryRequest(t *testing.T) {
	a := &authenticator{mode: "oidc", key: bytes.Repeat([]byte{0xa5}, 32), allowedGroups: map[string]bool{"operators": true}}
	sign := func(name string, value any) string {
		t.Helper()
		sealed, err := a.seal(name, value)
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	valid := authSession{Subject: "real-subject", Groups: []string{"operators", "unbound", "operators"}, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	good := sign(sessionCookieName, valid)
	other := &authenticator{key: bytes.Repeat([]byte{0xb6}, 32)}
	wrongKey, err := other.seal(sessionCookieName, valid)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	for _, tc := range []struct {
		name, value string
		duplicate   bool
	}{
		{name: "missing"},
		{name: "forged payload", value: tamperAuthTestValue(good)},
		{name: "forged signature", value: parts[0] + "." + tamperAuthTestValue(parts[1])},
		{name: "wrong key", value: wrongKey},
		{name: "expired", value: sign(sessionCookieName, authSession{Subject: "real", ExpiresAt: time.Now().Add(-time.Second).Unix()})},
		{name: "expiry boundary", value: sign(sessionCookieName, authSession{Subject: "real", ExpiresAt: time.Now().Unix()})},
		{name: "no expiry", value: sign(sessionCookieName, map[string]any{"sub": "real"})},
		{name: "empty subject", value: sign(sessionCookieName, authSession{ExpiresAt: valid.ExpiresAt})},
		{name: "whitespace subject", value: sign(sessionCookieName, authSession{Subject: " \t", ExpiresAt: valid.ExpiresAt})},
		{name: "null session", value: sign(sessionCookieName, nil)},
		{name: "unsigned", value: parts[0]},
		{name: "bad base64", value: "%%%.%%%"},
		{name: "oversized", value: strings.Repeat("s", maxCookieValue+1)},
		{name: "wrong purpose", value: sign(loginCookieName, valid)},
		{name: "duplicate", value: good, duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "https://console.example/", nil)
			for _, header := range []string{"X-Forwarded-User", "X-Remote-User", "X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups"} {
				r.Header.Set(header, "operators")
			}
			r.Header.Set("Authorization", "Bearer "+good)
			if tc.value != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tc.value})
			}
			if tc.duplicate {
				r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tc.value})
			}
			if principal, ok := a.Principal(r); ok || principal.Subject != "" || len(principal.Groups) != 0 {
				t.Fatalf("untrusted identity accepted: %+v", principal)
			}
		})
	}
	r := httptest.NewRequest(http.MethodGet, "https://console.example/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: good})
	p, ok := a.Principal(r)
	if !ok || p.Subject != valid.Subject || !reflect.DeepEqual(p.Groups, []string{"operators"}) {
		t.Fatalf("valid session rejected: %+v, %v", p, ok)
	}
	a.allowedGroups = nil
	p, ok = a.Principal(r)
	if !ok || len(p.Groups) != 0 {
		t.Fatal("removed bindings must not leave stale session groups")
	}
}

func TestAuthenticationMethodsAndLogoutCSRF(t *testing.T) {
	a := &authenticator{mode: "oidc", redirectOrigin: "https://console.example:443"}
	for _, endpoint := range []struct {
		name, method string
		handler      func(http.ResponseWriter, *http.Request)
	}{
		{"login", http.MethodGet, a.Login}, {"callback", http.MethodGet, a.Callback}, {"logout", http.MethodPost, a.Logout},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			if method == endpoint.method {
				continue
			}
			rr := httptest.NewRecorder()
			endpoint.handler(rr, httptest.NewRequest(method, "https://console.example/auth/"+endpoint.name, nil))
			if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != endpoint.method || len(rr.Result().Cookies()) != 0 {
				t.Fatalf("%s accepted %s or mutated cookies: %d", endpoint.name, method, rr.Code)
			}
		}
	}
	for _, tc := range []struct {
		name, origin, fetchSite string
		valid                   bool
	}{
		{name: "missing origin"},
		{name: "null origin", origin: "null"},
		{name: "cross origin", origin: "https://evil.example"},
		{name: "same site other origin", origin: "https://other.console.example", fetchSite: "same-site"},
		{name: "http origin", origin: "http://console.example"},
		{name: "wrong port", origin: "https://console.example:444"},
		{name: "origin with path", origin: "https://console.example/auth/logout"},
		{name: "origin with user", origin: "https://evil@console.example"},
		{name: "origin with query", origin: "https://console.example?return_url=/"},
		{name: "origin with fragment", origin: "https://console.example#fragment"},
		{name: "origin with empty fragment", origin: "https://console.example#"},
		{name: "multiple origins", origin: "https://console.example https://evil.example"},
		{name: "fetch cross site", origin: "https://console.example", fetchSite: "cross-site"},
		{name: "fetch cross site with whitespace", origin: "https://console.example", fetchSite: "cross-site "},
		{name: "fetch multiple sites", origin: "https://console.example", fetchSite: "same-origin, cross-site"},
		{name: "same origin", origin: "https://console.example", fetchSite: "same-origin", valid: true},
		{name: "explicit https port", origin: "https://console.example:443", valid: true},
		{name: "case insensitive host", origin: "https://CONSOLE.example", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "https://untrusted-request-host.example/auth/logout", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			r.Header.Set("X-Forwarded-Host", "evil.example")
			r.Header.Set("X-Forwarded-Proto", "http")
			rr := httptest.NewRecorder()
			a.Logout(rr, r)
			if !tc.valid {
				assertAuthTestFailure(t, rr, http.StatusForbidden)
				if len(rr.Result().Cookies()) != 0 {
					t.Fatal("CSRF attempt cleared cookies")
				}
				return
			}
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
				t.Fatalf("logout failed: %d", rr.Code)
			}
			for _, name := range []string{sessionCookieName, loginCookieName} {
				cookie := authTestCookie(t, rr, name)
				if cookie.MaxAge != -1 || cookie.Value != "" || !cookie.Expires.Before(time.Now()) || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
					t.Fatalf("cookie was not safely expired: %+v", cookie)
				}
			}
		})
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		r := httptest.NewRequest(http.MethodPost, "https://console.example/auth/logout", nil)
		r.Header.Set("Origin", "https://console.example")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Add(header, r.Header.Get(header))
		rr := httptest.NewRecorder()
		a.Logout(rr, r)
		assertAuthTestFailure(t, rr, http.StatusForbidden)
	}
}

func TestOIDCConfigurationAndDiscovery(t *testing.T) {
	p := newAuthTestProvider(t)
	cfg := p.config(t)
	for _, name := range []string{"http issuer", "http redirect", "redirect path", "no client", "missing key", "short key", "missing secret", "empty secret"} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			switch name {
			case "http issuer":
				invalid.IssuerURL = "http://issuer.example"
			case "http redirect":
				invalid.RedirectURL = "http://console.example/auth/callback"
			case "redirect path":
				invalid.RedirectURL = "https://console.example/elsewhere"
			case "no client":
				invalid.ClientID = ""
			case "missing key":
				invalid.CookieKeyFile = filepath.Join(t.TempDir(), "missing")
			case "short key":
				invalid.CookieKeyFile = filepath.Join(t.TempDir(), "short")
				if err := os.WriteFile(invalid.CookieKeyFile, bytes.Repeat([]byte{'a'}, 31), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing secret":
				invalid.ClientSecretFile = filepath.Join(t.TempDir(), "missing")
			case "empty secret":
				invalid.ClientSecretFile = filepath.Join(t.TempDir(), "empty")
				if err := os.WriteFile(invalid.ClientSecretFile, []byte("\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := newAuthenticator(context.Background(), invalid); err == nil {
				t.Fatal("invalid authentication configuration accepted")
			}
		})
	}
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri"} {
		t.Run(field, func(t *testing.T) {
			p.mu.Lock()
			p.metadata[field] = "http://insecure.example"
			p.mu.Unlock()
			if _, err := newAuthenticator(context.Background(), cfg); err == nil {
				t.Fatal("insecure or mismatched discovery endpoint accepted")
			}
			p.mu.Lock()
			delete(p.metadata, field)
			p.mu.Unlock()
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newAuthenticator(ctx, cfg); err == nil {
		t.Fatal("discovery ignored cancellation")
	}
	// A valid signature does not make an untrusted HTTPS certificate acceptable.
	trusted := http.DefaultTransport
	http.DefaultTransport = &http.Transport{}
	_, err := newAuthenticator(context.Background(), cfg)
	http.DefaultTransport = trusted
	if err == nil {
		t.Fatal("discovery accepted an untrusted certificate")
	}
}

func TestAuthenticationAuditAndErrorsDoNotExposeIdentity(t *testing.T) {
	p := newAuthTestProvider(t)
	a := p.authenticator(t)
	var audit bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&audit, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })
	cookie, state := p.begin(t, a)
	rr := p.callback(a, cookie, state.State)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback = %d", rr.Code)
	}
	p.mu.Lock()
	p.tokenStatus = http.StatusBadRequest
	p.mu.Unlock()
	cookie, state = p.begin(t, a)
	rr = p.callback(a, cookie, state.State)
	assertAuthTestFailure(t, rr, http.StatusUnauthorized)
	for _, secret := range []string{"private@example.test", "Private Name", "stable-subject", "server-only", "valid-code", "private provider error", state.State, state.Nonce, state.Verifier} {
		if strings.Contains(audit.String(), secret) || strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("audit or HTTP error exposed %q", secret)
		}
	}
	if !strings.Contains(audit.String(), "console authentication succeeded") || !strings.Contains(audit.String(), "console authentication failed") {
		t.Fatal("missing authentication audit events")
	}
}
