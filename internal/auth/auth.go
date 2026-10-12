// Package auth protects the API and console with static bearer tokens.
//
// Scripts and agents send "Authorization: Bearer <token>". The browser logs
// in once with a token and receives a signed session cookie, because
// EventSource (the console's live streams) cannot send headers. Sessions are
// stateless HMAC-signed values, so every API replica accepts them without a
// shared store, and removing a token from the configuration revokes every
// session created with it.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	CookieName = "spawner_session"
	// failDelay slows token guessing; tokens are long and random, so this is
	// politeness rather than the defense.
	failDelay = 500 * time.Millisecond
)

type Authenticator struct {
	log    *slog.Logger
	tokens map[string]string // token id -> token
	key    []byte
	ttl    time.Duration
	now    func() time.Time
}

// New returns an Authenticator for the given tokens. With no tokens it is
// disabled and lets every request through.
func New(log *slog.Logger, tokens []string, ttl time.Duration) *Authenticator {
	a := &Authenticator{log: log, tokens: map[string]string{}, ttl: ttl, now: time.Now}
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

func (a *Authenticator) Enabled() bool { return len(a.tokens) > 0 }

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
	if c, err := r.Cookie(CookieName); err == nil && a.validSession(c.Value) {
		return true, true
	}
	return false, false
}

// Public reports paths that never need authentication: health checks, the
// login endpoints and the console's static shell (it holds no data).
func Public(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/v1/login", "/v1/logout", "/v1/session":
		return true
	}
	return !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/debug/")
}

// Middleware enforces authentication on every non-public path.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Public(r.URL.Path) {
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
		next.ServeHTTP(w, r)
	})
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

// Routes registers /v1/login, /v1/logout and /v1/session.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, r *http.Request) {
		ok := true
		if a.Enabled() {
			ok, _ = a.Authenticated(r)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"auth": a.Enabled(), "authenticated": ok})
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
			Token string `json:"token"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		id, ok := a.validToken(strings.TrimSpace(body.Token))
		if !ok {
			time.Sleep(failDelay)
			a.log.Warn("login failed", "remote", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		value, exp := a.newSession(id)
		http.SetCookie(w, &http.Cookie{
			Name: CookieName, Value: value, Path: "/", Expires: exp, MaxAge: int(a.ttl.Seconds()),
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r),
		})
		a.log.Info("login", "token", id, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]bool{"auth": true, "authenticated": true})
	})
	mux.HandleFunc("POST /v1/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r)})
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
	})
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
