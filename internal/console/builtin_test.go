package console

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"k8gb-ui/internal/password"
)

const builtinTestPassword = "only-a-test-passphrase"

func builtinConfig(t *testing.T) (AuthConfig, []builtinUser) {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "session-key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{0xa5}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := password.Hash([]byte(builtinTestPassword))
	if err != nil {
		t.Fatal(err)
	}
	users := []builtinUser{{Username: "operator", PasswordHash: hash, Groups: []string{"platform-admins"}}, {Username: "reader", PasswordHash: hash, Groups: []string{"team-catalog"}}, {Username: "unbound", PasswordHash: hash}, {Username: "disabled", PasswordHash: hash, Groups: []string{"platform-admins"}, Disabled: true}}
	cfg := AuthConfig{Mode: "builtin", PublicURL: "https://console.example", UsersFile: filepath.Join(dir, "users.yaml"), CookieKeyFile: keyFile, Bindings: []Binding{
		{Role: "admin", Groups: []string{"platform-admins"}, Clusters: []string{"*"}, Namespaces: []string{"*"}},
		{Role: "viewer", Groups: []string{"team-catalog"}, Clusters: []string{"east"}, Namespaces: []string{"demo"}},
	}}
	writeBuiltinTestUsers(t, cfg.UsersFile, users)
	return cfg, users
}

func writeBuiltinTestUsers(t *testing.T, path string, users []builtinUser) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"users": users})
	if err != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("cannot write test users")
	}
}

func builtinBegin(t *testing.T, a *authenticator) (*http.Cookie, builtinLoginState) {
	t.Helper()
	rr := httptest.NewRecorder()
	a.Login(rr, httptest.NewRequest("GET", "https://console.example/auth/login", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `autocomplete="current-password"`) || !strings.Contains(rr.Body.String(), `name="csrf"`) || rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("login page unavailable: %d", rr.Code)
	}
	cookie := authTestCookie(t, rr, builtinLoginCookie)
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Domain != "" || cookie.Path != "/" {
		t.Fatal("unsafe login cookie")
	}
	r := httptest.NewRequest("GET", "https://console.example", nil)
	r.AddCookie(cookie)
	var state builtinLoginState
	if !a.readCookie(r, builtinLoginCookie, &state) || state.Token == "" {
		t.Fatal("invalid login state")
	}
	return cookie, state
}

func builtinPost(a *authenticator, cookie *http.Cookie, values url.Values, change func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://console.example/auth/login", strings.NewReader(values.Encode()))
	r.RemoteAddr = "192.0.2.1:12345"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://console.example")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if change != nil {
		change(r)
	}
	rr := httptest.NewRecorder()
	a.Login(rr, r)
	return rr
}

func builtinSignIn(t *testing.T, a *authenticator, username string) *http.Cookie {
	t.Helper()
	cookie, state := builtinBegin(t, a)
	rr := builtinPost(a, cookie, url.Values{"csrf": {state.Token}, "username": {username}, "password": {builtinTestPassword}}, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("sign in failed: %d %s", rr.Code, rr.Body.String())
	}
	return authTestCookie(t, rr, sessionCookieName)
}

func TestBuiltinLoginAndLiveAccountChanges(t *testing.T) {
	cfg, users := builtinConfig(t)
	a, err := newAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cookie := builtinSignIn(t, a, "operator")
	r := httptest.NewRequest("GET", "https://console.example/api/v1/session", nil)
	r.AddCookie(cookie)
	p, ok := a.Principal(r)
	if !ok || p.Subject != "builtin:operator" || !contains(p.Groups, "platform-admins") {
		t.Fatal("authenticated identity not loaded")
	}
	replica, err := newAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := replica.Principal(r); !ok {
		t.Fatal("replica with the same configuration rejected session")
	}
	users[0].Groups = []string{"team-catalog"}
	writeBuiltinTestUsers(t, cfg.UsersFile, users)
	p, ok = replica.Principal(r)
	if !ok || contains(p.Groups, "platform-admins") || !contains(p.Groups, "team-catalog") {
		t.Fatal("group change left stale permissions in session")
	}
	users[0].Disabled = true
	writeBuiltinTestUsers(t, cfg.UsersFile, users)
	if _, ok := a.Principal(r); ok {
		t.Fatal("disabled user retained session")
	}
	users[0].Disabled = false
	users[0].PasswordHash, _ = password.Hash([]byte("a rotated test passphrase"))
	writeBuiltinTestUsers(t, cfg.UsersFile, users)
	if _, ok := a.Principal(r); ok {
		t.Fatal("password rotation did not revoke session")
	}
	writeBuiltinTestUsers(t, cfg.UsersFile, users[1:])
	if _, ok := a.Principal(r); ok {
		t.Fatal("deleted user retained session")
	}
	if err := os.Remove(cfg.UsersFile); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Principal(r); ok {
		t.Fatal("missing users file did not fail closed")
	}
}

func TestBuiltinRejectsLoginCSRFAndInvalidCredentials(t *testing.T) {
	cfg, _ := builtinConfig(t)
	a, err := newAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cookie, state := builtinBegin(t, a)
	form := func() url.Values {
		return url.Values{"csrf": {state.Token}, "username": {"operator"}, "password": {builtinTestPassword}}
	}
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"no origin", func(r *http.Request) { r.Header.Del("Origin") }},
		{"cross origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"forwarded origin", func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("X-Forwarded-Host", "console.example")
		}},
		{"duplicate origin", func(r *http.Request) { r.Header.Add("Origin", "https://console.example") }},
		{"fetch cross site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
		{"no cookie", func(r *http.Request) { r.Header.Del("Cookie") }},
		{"wrong type", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := builtinPost(a, cookie, form(), tc.change)
			if rr.Code != 403 {
				t.Fatalf("CSRF accepted: %d", rr.Code)
			}
		})
	}
	for _, csrf := range []string{"", "forged"} {
		values := form()
		values.Set("csrf", csrf)
		if rr := builtinPost(a, cookie, values, nil); rr.Code != 403 {
			t.Fatal("invalid CSRF token accepted")
		}
	}
	for _, bad := range []builtinLoginState{{Token: state.Token, ExpiresAt: time.Now().Add(-time.Minute).Unix()}, {Token: "wrong token", ExpiresAt: state.ExpiresAt}} {
		value, _ := a.seal(builtinLoginCookie, bad)
		if rr := builtinPost(a, &http.Cookie{Name: builtinLoginCookie, Value: value}, form(), nil); rr.Code != 403 {
			t.Fatal("expired or mismatched login state accepted")
		}
	}
	for _, username := range []string{"unknown", "disabled", "operator"} {
		values := form()
		values.Set("username", username)
		if username == "operator" {
			values.Set("password", "incorrect passphrase")
		}
		rr := builtinPost(a, cookie, values, nil)
		if rr.Code != 401 || !strings.Contains(rr.Body.String(), "Invalid username or password.") || strings.Contains(rr.Body.String(), builtinTestPassword) {
			t.Fatal("invalid credentials accepted or exposed")
		}
		for _, c := range rr.Result().Cookies() {
			if c.Name == sessionCookieName {
				t.Fatal("failed login issued a session")
			}
		}
	}
	for _, method := range []string{"HEAD", "PUT", "DELETE"} {
		rr := httptest.NewRecorder()
		a.Login(rr, httptest.NewRequest(method, "https://console.example/auth/login", nil))
		if rr.Code != 405 || rr.Header().Get("Allow") != "GET, POST" {
			t.Fatal("invalid login method accepted")
		}
	}
}

func TestBuiltinRateLimitAndBoundedState(t *testing.T) {
	cfg, _ := builtinConfig(t)
	a, err := newAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cookie, state := builtinBegin(t, a)
	for i := 0; i < 6; i++ {
		rr := builtinPost(a, cookie, url.Values{"csrf": {state.Token}, "username": {"operator"}, "password": {"wrong passphrase"}}, nil)
		if i < 5 && rr.Code != 401 || i == 5 && (rr.Code != 429 || rr.Header().Get("Retry-After") == "") {
			t.Fatalf("rate limit status %d at %d", rr.Code, i)
		}
	}
	now := time.Now()
	a.builtin.limits = make(map[string]*loginLimit)
	for i := 0; i < 4096; i++ {
		a.builtin.limits[string(rune(i))] = &loginLimit{lastSeen: now}
	}
	if a.builtin.allow("198.51.100.1:1234", "new-user", now.Add(time.Second)) || len(a.builtin.limits) > 4096 {
		t.Fatal("unbounded rate limit state")
	}
	if !a.builtin.allow("198.51.100.1:1234", "new-user", now.Add(11*time.Minute)) {
		t.Fatal("expired rate limit entries were not reclaimed")
	}
}

func TestBuiltinSharedRBACAndAPIEnforcement(t *testing.T) {
	auth, _ := builtinConfig(t)
	cfg, _ := Demo("127.0.0.1:8080")
	cfg.Auth, cfg.WritesEnabled = auth, true
	for i := range cfg.Clusters {
		cfg.Clusters[i].URL = "https://" + cfg.Clusters[i].ID + ".example"
		cfg.Clusters[i].BearerTokenFile = "unused-test-read-token"
		cfg.Clusters[i].WriteTokenFile = "unused-test-write-token"
	}
	east := &fakeWriter{fakeAgent: fakeAgent{namespaces: []string{"demo", "private"}}}
	west := &fakeWriter{fakeAgent: fakeAgent{namespaces: []string{"demo", "private"}}}
	s, err := New(context.Background(), cfg, fstest.MapFS{"index.html": {Data: []byte("console")}, "static/k8gb.css": {Data: []byte("body{}")}}, map[string]Agent{"east": east, "west": west}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"operator", "reader", "unbound"} {
		cookie := builtinSignIn(t, s.auth, username)
		for _, namespace := range []string{"demo", "private"} {
			pReq := httptest.NewRequest("GET", "https://console.example/api/v1/session", nil)
			pReq.AddCookie(cookie)
			p, ok := s.auth.Principal(pReq)
			if !ok {
				t.Fatal("session rejected")
			}
			for _, cluster := range []string{"east", "west"} {
				read := username == "operator" || username == "reader" && cluster == "east" && namespace == "demo"
				if cfg.Allowed(p, cluster, namespace) != read || cfg.CanWrite(p, cluster, namespace) != (username == "operator") {
					t.Fatal("group bindings did not constrain cluster and namespace")
				}
			}
			input, _ := json.Marshal(testPlanRequest(namespace))
			r := httptest.NewRequest("POST", "https://console.example/api/v1/mutations/plan", bytes.NewReader(input))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-K8gb-Intent", "mutation")
			r.Header.Set("Origin", "https://console.example")
			r.AddCookie(cookie)
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, r)
			if username == "operator" && rr.Code != 200 || username != "operator" && rr.Code != 403 {
				t.Fatalf("API write authorization failed for %s: %d", username, rr.Code)
			}
		}
		r := httptest.NewRequest("GET", "https://console.example/api/v1/namespaces", nil)
		r.AddCookie(cookie)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, r)
		if username != "operator" && strings.Contains(rr.Body.String(), "private") || username == "unbound" && strings.Contains(rr.Body.String(), "demo") {
			t.Fatal("unauthorized namespace leaked")
		}
	}
	if east.actor != "builtin:operator" || east.plans != 2 || west.plans != 0 {
		t.Fatal("mutation actor spoofed or unauthorized request reached agent")
	}
	for _, origin := range []string{"https://console.example", "https://CONSOLE.example:443"} {
		r := httptest.NewRequest("POST", "https://console.example/api/v1/mutations/plan", nil)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-K8gb-Intent", "mutation")
		r.Header.Set("Origin", origin)
		if !s.safeMutationOrigin(r) {
			t.Fatal("equivalent HTTPS origin rejected")
		}
		r.Header.Set("Sec-Fetch-Site", "cross-site ")
		if s.safeMutationOrigin(r) {
			t.Fatal("cross-site mutation accepted")
		}
	}
	for _, tc := range []struct {
		path   string
		status int
	}{{"/api/v1/session", 401}, {"/", 302}, {"/static/k8gb.css", 200}} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest("GET", "https://console.example"+tc.path, nil))
		if rr.Code != tc.status {
			t.Fatalf("unauthenticated route %s: %d", tc.path, rr.Code)
		}
	}
	for _, mode := range []string{"oidc", "builtin"} {
		cfg.Auth.Mode = mode
		if cfg.Allowed(Principal{Subject: "keycloak-admin", Groups: []string{"realm-admin"}}, "east", "demo") {
			t.Fatal("identity-provider admin implicitly received application permissions")
		}
	}
}

func TestBuiltinRejectsUnsafeConfigurationAndUserFiles(t *testing.T) {
	auth, users := builtinConfig(t)
	for _, change := range []func(*AuthConfig){
		func(c *AuthConfig) { c.PublicURL = "http://console.example" },
		func(c *AuthConfig) { c.PublicURL += "/path" },
		func(c *AuthConfig) { c.PublicURL += "?" },
		func(c *AuthConfig) { c.PublicURL += "#fragment" },
		func(c *AuthConfig) { c.CookieKeyFile = "" },
		func(c *AuthConfig) { c.UsersFile = "" },
		func(c *AuthConfig) { c.LocalAdmin = true },
		func(c *AuthConfig) { c.Bindings = nil },
		func(c *AuthConfig) { c.IssuerURL = "https://issuer.example" },
	} {
		cfg, _ := Demo(":8080")
		cfg.Auth = auth
		for i := range cfg.Clusters {
			cfg.Clusters[i].URL = "https://agent.example"
			cfg.Clusters[i].BearerTokenFile = "unused"
		}
		change(&cfg.Auth)
		if cfg.Validate() == nil {
			t.Fatal("unsafe builtin configuration accepted")
		}
	}
	for _, raw := range []string{`users: []`, `users: [{username: admin, password: secret}]`, `users: [{username: admin, passwordHash: plaintext}]`, strings.Repeat(" ", (1<<20)+1)} {
		if err := os.WriteFile(auth.UsersFile, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newAuthenticator(context.Background(), auth); err == nil {
			t.Fatal("invalid users file accepted")
		}
	}
	writeBuiltinTestUsers(t, auth.UsersFile, append(users, users[0]))
	if _, err := readBuiltinUsers(auth.UsersFile); err == nil {
		t.Fatal("duplicate users accepted")
	}
}

func TestBuiltinExampleAndRelativeSecretPaths(t *testing.T) {
	if _, err := LoadConfig("../../examples/console.builtin.yaml"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "console.yaml")
	raw := []byte("listen: ':8080'\nauth:\n  mode: builtin\n  publicURL: https://console.example\n  cookieKeyFile: session-key\n  usersFile: users.yaml\n  bindings:\n    - subjects: [builtin:reader]\n      clusters: [east]\n      namespaces: [demo]\nclusters:\n  - id: east\n    url: https://agent.example\n    zones: [apps.example.test]\n    bearerTokenFile: agent-token\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Auth.UsersFile != filepath.Join(dir, "users.yaml") || cfg.Auth.CookieKeyFile != filepath.Join(dir, "session-key") {
		t.Fatalf("relative secret paths not resolved: %v", err)
	}
}

func TestBuiltinSessionsCannotCrossModesAndLogsContainNoCredentials(t *testing.T) {
	cfg, _ := builtinConfig(t)
	a, err := newAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	cookie := builtinSignIn(t, a, "operator")
	r := httptest.NewRequest("GET", "https://console.example/api/v1/session", nil)
	r.AddCookie(cookie)
	oidcAuth := &authenticator{mode: "oidc", key: a.key}
	if _, ok := oidcAuth.Principal(r); ok {
		t.Fatal("builtin session accepted after switching to OIDC")
	}
	oidcSession, _ := a.seal(sessionCookieName, authSession{Subject: "builtin:operator", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	r.Header.Set("Cookie", (&http.Cookie{Name: sessionCookieName, Value: oidcSession}).String())
	if _, ok := a.Principal(r); ok {
		t.Fatal("OIDC session accepted in builtin mode")
	}
	for _, secret := range []string{builtinTestPassword, cookie.Value, "builtin:operator", "argon2id"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("authentication log exposed identity or credentials")
		}
	}
	rr := httptest.NewRecorder()
	r = httptest.NewRequest("POST", "https://console.example/auth/logout", nil)
	r.Header.Set("Origin", "https://console.example")
	a.Logout(rr, r)
	if rr.Code != 303 || authTestCookie(t, rr, sessionCookieName).MaxAge != -1 || authTestCookie(t, rr, builtinLoginCookie).MaxAge != -1 {
		t.Fatal("builtin logout did not clear session and login cookies")
	}
}
