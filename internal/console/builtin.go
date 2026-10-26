package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/time/rate"
	"k8gb-ui/internal/password"
	"sigs.k8s.io/yaml"
)

const builtinLoginCookie = "__Host-k8gb-login"

var builtinUsername = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@-]{0,63}$`)

type builtinUser struct {
	Username     string   `json:"username"`
	PasswordHash string   `json:"passwordHash"`
	Groups       []string `json:"groups"`
	Disabled     bool     `json:"disabled,omitempty"`
}

type builtinAuth struct {
	usersFile string
	dummyHash string
	mu        sync.Mutex
	limits    map[string]*loginLimit
	global    *rate.Limiter
	active    chan struct{}
}

type loginLimit struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type builtinLoginState struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"exp"`
}

func newBuiltinAuthenticator(cfg AuthConfig) (*authenticator, error) {
	public, err := authHTTPSURL(cfg.PublicURL)
	if err != nil || (public.Path != "" && public.Path != "/") || public.RawQuery != "" || public.ForceQuery || cfg.LocalAdmin || cfg.UsersFile == "" || len(cfg.Bindings) == 0 || cfg.IssuerURL != "" || cfg.ClientID != "" || cfg.ClientSecretFile != "" || cfg.RedirectURL != "" {
		return nil, errors.New("invalid builtin authentication configuration")
	}
	key, err := os.ReadFile(cfg.CookieKeyFile)
	if err != nil || len(key) < 32 {
		return nil, errors.New("builtin cookie key must contain at least 32 raw bytes")
	}
	if _, err := readBuiltinUsers(cfg.UsersFile); err != nil {
		return nil, err
	}
	dummy, err := randomAuthValue()
	if err != nil {
		return nil, errors.New("cannot initialize builtin authentication")
	}
	dummyHash, err := password.Hash([]byte(dummy))
	if err != nil {
		return nil, errors.New("cannot initialize builtin authentication")
	}
	return &authenticator{mode: "builtin", key: key, redirectOrigin: authOrigin(public), builtin: &builtinAuth{
		usersFile: cfg.UsersFile, dummyHash: dummyHash, limits: map[string]*loginLimit{},
		global: rate.NewLimiter(5, 10), active: make(chan struct{}, 2),
	}}, nil
}

func readBuiltinUsers(path string) (map[string]builtinUser, error) {
	invalid := errors.New("cannot load builtin users: require a valid users file with unique usernames and Argon2id passwordHash values")
	file, err := os.Open(path)
	if err != nil {
		return nil, invalid
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, invalid
	}
	var data struct {
		Users []builtinUser `json:"users"`
	}
	if yaml.UnmarshalStrict(raw, &data) != nil || len(data.Users) == 0 || len(data.Users) > 1000 {
		return nil, invalid
	}
	users := make(map[string]builtinUser, len(data.Users))
	for _, user := range data.Users {
		if !builtinUsername.MatchString(user.Username) || password.Validate(user.PasswordHash) != nil || len(user.Groups) > 64 {
			return nil, invalid
		}
		if _, exists := users[user.Username]; exists {
			return nil, invalid
		}
		for _, group := range user.Groups {
			if group == "" || len(group) > 256 || strings.TrimSpace(group) != group || !utf8.ValidString(group) || strings.IndexFunc(group, unicode.IsControl) >= 0 {
				return nil, invalid
			}
		}
		users[user.Username] = user
	}
	return users, nil
}

func (a *authenticator) credentialVersion(user builtinUser) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte("builtin-credential\x00" + user.Username + "\x00" + user.PasswordHash))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *authenticator) builtinPrincipal(session authSession) (Principal, bool) {
	if a.builtin == nil || session.AuthMode != "builtin" || session.Version == "" || !strings.HasPrefix(session.Subject, "builtin:") {
		return Principal{}, false
	}
	// Re-read the mounted file so account removal, rotation and group changes
	// take effect on every replica without retaining stale cookie permissions.
	users, err := readBuiltinUsers(a.builtin.usersFile)
	if err != nil {
		return Principal{}, false
	}
	user, found := users[strings.TrimPrefix(session.Subject, "builtin:")]
	if !found || user.Disabled || !hmac.Equal([]byte(session.Version), []byte(a.credentialVersion(user))) {
		return Principal{}, false
	}
	return Principal{Subject: session.Subject, Groups: user.Groups}, true
}

func (b *builtinAuth) allow(remote, username string, now time.Time) bool {
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		ip = "unknown"
	}
	identity := sha256.Sum256([]byte(username))
	keys := []string{"ip:" + ip, "user:" + base64.RawURLEncoding.EncodeToString(identity[:])}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.global.AllowN(now, 1) {
		return false
	}
	if len(b.limits) >= 4096 {
		for key, value := range b.limits {
			if now.Sub(value.lastSeen) > 10*time.Minute {
				delete(b.limits, key)
			}
		}
	}
	for _, key := range keys {
		if b.limits[key] == nil && len(b.limits) >= 4096 {
			return false
		}
		if b.limits[key] == nil {
			b.limits[key] = &loginLimit{limiter: rate.NewLimiter(rate.Every(10*time.Second), 5)}
		}
		b.limits[key].lastSeen = now
		if !b.limits[key].limiter.AllowN(now, 1) {
			return false
		}
	}
	return true
}

func (a *authenticator) builtinLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// A no-referrer policy turns browser form POST Origin into "null".
	// same-origin preserves the strict Origin check without leaking off-site URLs.
	w.Header().Set("Referrer-Policy", "same-origin")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.builtin == nil {
		authFailure(w, http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		a.renderBuiltinLogin(w, http.StatusOK, "")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !a.sameOrigin(r) || err != nil || media != "application/x-www-form-urlencoded" {
		authFailure(w, http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var state builtinLoginState
	if r.ParseForm() != nil || len(r.PostForm["csrf"]) != 1 || !a.readCookie(r, builtinLoginCookie, &state) ||
		state.ExpiresAt <= time.Now().Unix() || state.Token == "" || !hmac.Equal([]byte(state.Token), []byte(r.PostForm.Get("csrf"))) {
		authFailure(w, http.StatusForbidden)
		return
	}
	username := r.PostForm.Get("username")
	value := r.PostForm.Get("password")
	if len(r.PostForm["username"]) != 1 || len(r.PostForm["password"]) != 1 || !builtinUsername.MatchString(username) || len(value) == 0 || len(value) > password.MaxBytes {
		a.renderBuiltinLogin(w, http.StatusUnauthorized, "Invalid username or password.")
		return
	}
	if !a.builtin.allow(r.RemoteAddr, username, time.Now()) {
		w.Header().Set("Retry-After", "10")
		a.renderBuiltinLogin(w, http.StatusTooManyRequests, "Too many sign-in attempts. Try again shortly.")
		return
	}
	select {
	case a.builtin.active <- struct{}{}:
		defer func() { <-a.builtin.active }()
	default:
		w.Header().Set("Retry-After", "10")
		a.renderBuiltinLogin(w, http.StatusTooManyRequests, "Too many sign-in attempts. Try again shortly.")
		return
	}
	users, err := readBuiltinUsers(a.builtin.usersFile)
	if err != nil {
		a.renderBuiltinLogin(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable.")
		return
	}
	user, found := users[username]
	hash := a.builtin.dummyHash
	if found {
		hash = user.PasswordHash
	}
	if valid := password.Verify(hash, []byte(value)); !valid || !found || user.Disabled {
		slog.Warn("console authentication failed", "mode", "builtin", "status", http.StatusUnauthorized)
		a.renderBuiltinLogin(w, http.StatusUnauthorized, "Invalid username or password.")
		return
	}
	session := authSession{Subject: "builtin:" + username, ExpiresAt: time.Now().Add(sessionLifetime).Unix(), AuthMode: "builtin", Version: a.credentialVersion(user)}
	sealed, err := a.seal(sessionCookieName, session)
	if err != nil {
		authFailure(w, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, authCookie(sessionCookieName, sealed, session.ExpiresAt))
	expireAuthCookie(w, builtinLoginCookie)
	slog.Info("console authentication succeeded", "mode", "builtin")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var builtinLoginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en" data-theme="light"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in - k8gb-ui</title><script src="/static/k8gb-theme.js"></script><link rel="stylesheet" href="/static/k8gb.css"></head>
<body><header class="k8gb-topbar"><a class="k8gb-brand" href="/"><strong>k8gb-ui</strong></a></header><main class="k8gb-main k8gb-login-shell"><section class="k8gb-login-panel" aria-labelledby="loginHeading"><h1 id="loginHeading">Sign in</h1>
{{if .Error}}<p class="k8gb-error" role="alert">{{.Error}}</p>{{end}}
<form action="/auth/login" method="post" class="k8gb-login-form"><input type="hidden" name="csrf" value="{{.Token}}"><div><label class="k8gb-label" for="username">Username</label><input id="username" name="username" required maxlength="64" autocomplete="username" autocapitalize="none" spellcheck="false" autofocus></div><div><label class="k8gb-label" for="password">Password</label><input id="password" name="password" type="password" required maxlength="1024" autocomplete="current-password"></div><button class="k8gb-button k8gb-primary" type="submit">Sign in</button></form></section></main></body></html>`))

func (a *authenticator) renderBuiltinLogin(w http.ResponseWriter, status int, message string) {
	token, err := randomAuthValue()
	if err != nil {
		authFailure(w, http.StatusInternalServerError)
		return
	}
	state := builtinLoginState{Token: token, ExpiresAt: time.Now().Add(loginLifetime).Unix()}
	sealed, err := a.seal(builtinLoginCookie, state)
	if err != nil {
		authFailure(w, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, authCookie(builtinLoginCookie, sealed, state.ExpiresAt))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = builtinLoginPage.Execute(w, struct{ Token, Error string }{token, message})
}
