// Package auth protects the API and console.
//
// Scripts and agents send "Authorization: Bearer <token>" (static tokens).
// People sign in to the console with an email and password (accounts in
// Postgres, argon2id hashes) or with a token, and receive a signed session
// cookie, because EventSource (the console's live streams) cannot send
// headers. Sessions are HMAC-signed values every API replica can check:
//   - token sessions (v1) are signed with a key derived from the token set,
//     so removing a token revokes its sessions;
//   - account sessions (v2) are signed with a key stored in the database and
//     carry the account's session epoch, so a password change or disabling
//     the account revokes them.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	CookieName = "spawner_session"
	// failDelay slows guessing; tokens are long and random, and passwords
	// are also protected by the per-email lockout below.
	failDelay = 500 * time.Millisecond

	maxFailures  = 5                // failed password logins per email...
	failWindow   = 15 * time.Minute // ...within this window lock it for this long
	userCacheFor = 15 * time.Second
)

type Authenticator struct {
	log    *slog.Logger
	tokens map[string]string // token id -> token
	key    []byte
	ttl    time.Duration
	now    func() time.Time

	users      Users
	userKey    []byte
	mu         sync.Mutex
	usersExist bool
	cache      map[uuid.UUID]cachedUser
	failures   map[string]*failure
}

type cachedUser struct {
	user *User
	at   time.Time
}

type failure struct {
	n           int
	first, lock time.Time
}

// New returns an Authenticator for the given tokens. With no tokens it is
// disabled and lets every request through.
func New(log *slog.Logger, tokens []string, ttl time.Duration) *Authenticator {
	a := &Authenticator{log: log, tokens: map[string]string{}, ttl: ttl, now: time.Now,
		cache: map[uuid.UUID]cachedUser{}, failures: map[string]*failure{}}
	var all []string
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			a.tokens[tokenID(t)] = t
			all = append(all, t)
		}
	}
	slices.Sort(all)
	// The signing key changes whenever the token set does, so rotating
	// tokens also invalidates old sessions.
	sum := sha256.Sum256([]byte("spawner-session-v1\x00" + strings.Join(all, "\x00")))
	a.key = sum[:]
	if a.ttl <= 0 {
		a.ttl = 7 * 24 * time.Hour
	}
	return a
}

// WithUsers enables email and password accounts. It loads the shared
// session key and keeps track of whether any account exists (an account
// turns authentication on even without tokens).
func (a *Authenticator) WithUsers(ctx context.Context, users Users) error {
	key, err := users.SessionKey(ctx)
	if err != nil {
		return fmt.Errorf("load session key: %w", err)
	}
	sum := sha256.Sum256(append([]byte("spawner-session-v2\x00"), key...))
	a.users, a.userKey = users, sum[:]
	a.refreshUsersExist(ctx)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.refreshUsersExist(ctx)
			}
		}
	}()
	return nil
}

func (a *Authenticator) refreshUsersExist(ctx context.Context) {
	ok, err := a.users.Any(ctx)
	if err != nil {
		a.log.Warn("check accounts", "err", err)
		return
	}
	a.mu.Lock()
	a.usersExist = ok
	a.mu.Unlock()
}

func (a *Authenticator) Enabled() bool {
	if len(a.tokens) > 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usersExist
}

func tokenID(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:6])
}

// validToken compares in constant time against every configured token.
func (a *Authenticator) validToken(t string) (string, bool) {
	found := ""
	for id, want := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(want)) == 1 {
			found = id
		}
	}
	return found, found != ""
}

func (a *Authenticator) sign(payload string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Authenticator) newSession(id string) (string, time.Time) {
	exp := a.now().Add(a.ttl)
	payload := fmt.Sprintf("v1.%d.%s", exp.Unix(), id)
	return payload + "." + a.sign(payload), exp
}

func (a *Authenticator) validSession(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	payload := strings.Join(parts[:3], ".")
	if !hmac.Equal([]byte(parts[3]), []byte(a.sign(payload))) {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || a.now().Unix() >= exp {
		return false
	}
	_, ok := a.tokens[parts[2]] // revoked when its token is removed
	return ok
}

// Authenticated reports whether r carries a valid bearer token or session,
// and whether it came by cookie (cookies need the cross-site check).
func (a *Authenticator) Authenticated(r *http.Request) (ok, viaCookie bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		_, ok := a.validToken(strings.TrimPrefix(h, "Bearer "))
		return ok, false
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if strings.HasPrefix(c.Value, "v2.") {
			return a.sessionUser(r.Context(), c.Value) != nil, true
		}
		if a.validSession(c.Value) {
			return true, true
		}
	}
	return false, false
}

// ---- account sessions (v2.<exp>.<user id>.<epoch>.<mac>) ----

func (a *Authenticator) signUser(payload string) string {
	m := hmac.New(sha256.New, a.userKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Authenticator) newUserSession(u *User) (string, time.Time) {
	exp := a.now().Add(a.ttl)
	payload := fmt.Sprintf("v2.%d.%s.%d", exp.Unix(), u.ID, u.Epoch)
	return payload + "." + a.signUser(payload), exp
}

// sessionUser returns the account behind a valid v2 session, or nil.
func (a *Authenticator) sessionUser(ctx context.Context, v string) *User {
	if a.users == nil {
		return nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 5 || parts[0] != "v2" {
		return nil
	}
	payload := strings.Join(parts[:4], ".")
	if !hmac.Equal([]byte(parts[4]), []byte(a.signUser(payload))) {
		return nil
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || a.now().Unix() >= exp {
		return nil
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return nil
	}
	epoch, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil
	}
	u := a.cachedUser(ctx, id)
	if u == nil || u.Disabled || u.Epoch != epoch {
		return nil // password changed, account disabled or deleted
	}
	return u
}

func (a *Authenticator) cachedUser(ctx context.Context, id uuid.UUID) *User {
	a.mu.Lock()
	c, ok := a.cache[id]
	a.mu.Unlock()
	if ok && a.now().Sub(c.at) < userCacheFor {
		return c.user
	}
	u, err := a.users.ByID(ctx, id)
	if err != nil && !errors.Is(err, ErrNoUser) {
		a.log.Warn("load account", "err", err)
		return nil
	}
	a.mu.Lock()
	a.cache[id] = cachedUser{user: u, at: a.now()}
	a.mu.Unlock()
	return u
}

func (a *Authenticator) forget(id uuid.UUID) {
	a.mu.Lock()
	delete(a.cache, id)
	a.mu.Unlock()
}

// locked reports whether an email is locked out after repeated failures.
func (a *Authenticator) locked(email string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[email]
	return f != nil && a.now().Before(f.lock)
}

func (a *Authenticator) recordFailure(email string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	f := a.failures[email]
	if f == nil || now.Sub(f.first) > failWindow {
		f = &failure{first: now}
		a.failures[email] = f
	}
	f.n++
	if f.n >= maxFailures {
		f.lock = now.Add(failWindow)
	}
}

func (a *Authenticator) clearFailures(email string) {
	a.mu.Lock()
	delete(a.failures, email)
	a.mu.Unlock()
}

func (a *Authenticator) setSession(w http.ResponseWriter, r *http.Request, value string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: value, Path: "/", Expires: exp, MaxAge: int(a.ttl.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r),
	})
}

// Public reports paths that never need authentication: health checks, the
// login endpoints and the console's static shell (it holds no data).
func Public(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/v1/login", "/v1/logout", "/v1/session":
		return true
	}
	return !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/debug/") && path != "/mcp" && !strings.HasPrefix(path, "/mcp/")
}

// Middleware enforces authentication on every non-public path, and records
// who is asking (Principal). Whether authentication is on is checked per
// request: creating the first account turns it on without a restart.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() || Public(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ok, viaCookie := a.Authenticated(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		if viaCookie && unsafeMethod(r.Method) && crossSite(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, a.principal(r))))
	})
}

type principalKey struct{}

// Principal identifies who made an authenticated request: the account's
// email, "token:<id>" for a token, or "" when authentication is off.
func Principal(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(string)
	return p
}

func (a *Authenticator) principal(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if id, ok := a.validToken(strings.TrimPrefix(h, "Bearer ")); ok {
			return "token:" + id
		}
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if u := a.sessionUser(r.Context(), c.Value); u != nil {
			return u.Email
		}
		if parts := strings.Split(c.Value, "."); len(parts) == 4 && parts[0] == "v1" {
			return "token:" + parts[2]
		}
	}
	return ""
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// crossSite is defense in depth beside SameSite=Strict: a cookie-authenticated
// write whose Origin names another host is refused.
func crossSite(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || !strings.EqualFold(u.Host, r.Host)
}

// Routes registers /v1/login, /v1/logout, /v1/session and
// /v1/account/password.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"auth": a.Enabled(), "authenticated": true, "accounts": a.users != nil}
		if a.Enabled() {
			ok, _ := a.Authenticated(r)
			resp["authenticated"] = ok
		}
		if c, err := r.Cookie(CookieName); err == nil {
			if u := a.sessionUser(r.Context(), c.Value); u != nil {
				resp["user"] = u.Email
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() {
			writeJSON(w, http.StatusOK, map[string]bool{"auth": false})
			return
		}
		if crossSite(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
			return
		}
		var body struct {
			Token    string `json:"token"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		if body.Email != "" {
			a.loginAccount(w, r, NormalizeEmail(body.Email), body.Password)
			return
		}
		id, ok := a.validToken(strings.TrimSpace(body.Token))
		if !ok {
			time.Sleep(failDelay)
			a.log.Warn("login failed", "method", "token", "remote", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		value, exp := a.newSession(id)
		a.setSession(w, r, value, exp)
		a.log.Info("login", "method", "token", "token", id, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]bool{"auth": true, "authenticated": true})
	})
	mux.HandleFunc("POST /v1/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r)})
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
	})
	// Changing the password ends every other session of the account; this
	// one gets a fresh cookie.
	mux.HandleFunc("POST /v1/account/password", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		var u *User
		if err == nil {
			u = a.sessionUser(r.Context(), c.Value)
		}
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in with an account to change its password"})
			return
		}
		if crossSite(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
			return
		}
		var body struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		if ok, _ := VerifyPassword(u.Hash, body.Current); !ok {
			time.Sleep(failDelay)
			a.log.Warn("password change refused: wrong current password", "account", u.Email, "remote", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the current password is wrong"})
			return
		}
		hash, err := HashPassword(body.New)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		epoch, err := a.users.SetPassword(r.Context(), u.ID, hash)
		if err != nil {
			a.log.Error("set password", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not change the password"})
			return
		}
		a.forget(u.ID)
		nu := *u
		nu.Hash, nu.Epoch = hash, epoch
		value, exp := a.newUserSession(&nu)
		a.setSession(w, r, value, exp)
		a.log.Info("password changed", "account", u.Email, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]bool{"changed": true})
	})
}

func (a *Authenticator) loginAccount(w http.ResponseWriter, r *http.Request, email, password string) {
	fail := func(status int, msg, reason string) {
		time.Sleep(failDelay)
		a.log.Warn("login failed", "method", "password", "account", email, "reason", reason, "remote", r.RemoteAddr)
		writeJSON(w, status, map[string]string{"error": msg})
	}
	if a.users == nil {
		fail(http.StatusUnauthorized, "invalid email or password", "accounts not enabled")
		return
	}
	if a.locked(email) {
		fail(http.StatusTooManyRequests, "too many failed attempts; try again in 15 minutes", "locked")
		return
	}
	u, err := a.users.ByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, ErrNoUser) {
		a.log.Error("load account", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sign-in failed"})
		return
	}
	hash := dummyHash // same work whether or not the account exists
	if u != nil {
		hash = u.Hash
	}
	ok, _ := VerifyPassword(hash, password)
	if u == nil || !ok || u.Disabled {
		a.recordFailure(email)
		fail(http.StatusUnauthorized, "invalid email or password", map[bool]string{true: "disabled", false: "wrong email or password"}[u != nil && ok])
		return
	}
	a.clearFailures(email)
	value, exp := a.newUserSession(u)
	a.setSession(w, r, value, exp)
	a.log.Info("login", "method", "password", "account", u.Email, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"auth": true, "authenticated": true, "user": u.Email})
}

// secure marks the cookie Secure only over HTTPS: the console is also served
// over plain HTTP on a NodePort, where a Secure cookie would never be sent.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
