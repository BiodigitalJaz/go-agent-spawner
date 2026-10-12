package auth

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const tok = "s3cret-token-0123456789abcdef"

func server(a *Authenticator) http.Handler {
	mux := http.NewServeMux()
	a.Routes(mux)
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /", ok)
	mux.HandleFunc("GET /v1/trees", ok)
	mux.HandleFunc("POST /v1/trees", ok)
	mux.HandleFunc("GET /debug/backends", ok)
	return a.Middleware(mux)
}

func newAuth(tokens ...string) *Authenticator {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), tokens, time.Hour)
}

func do(h http.Handler, method, path string, mut func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	if mut != nil {
		mut(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, h http.Handler, token string) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/login", strings.NewReader(`{"token":"`+token+`"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("login status %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure {
				t.Fatalf("cookie flags wrong over plain HTTP: %+v", c)
			}
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestDisabledAllowsEverything(t *testing.T) {
	h := server(newAuth())
	if w := do(h, "GET", "/v1/trees", nil); w.Code != 200 {
		t.Fatalf("disabled auth must allow, got %d", w.Code)
	}
	w := do(h, "GET", "/v1/session", nil)
	var s map[string]bool
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	if s["auth"] || !s["authenticated"] {
		t.Fatalf("session should report auth off: %v", s)
	}
}

func TestBearerAndPublicPaths(t *testing.T) {
	h := server(newAuth(tok, "other-token-xyz"))
	for _, p := range []string{"/healthz", "/", "/app.js", "/v1/session"} {
		if w := do(h, "GET", p, nil); w.Code != 200 {
			t.Errorf("%s should be public, got %d", p, w.Code)
		}
	}
	for _, p := range []string{"/v1/trees", "/debug/backends", "/mcp"} {
		if w := do(h, "GET", p, nil); w.Code != 401 {
			t.Errorf("%s without auth: want 401, got %d", p, w.Code)
		}
	}
	bearer := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+v) }
	}
	if w := do(h, "GET", "/v1/trees", bearer(tok)); w.Code != 200 {
		t.Fatalf("valid bearer: %d", w.Code)
	}
	if w := do(h, "GET", "/v1/trees", bearer("other-token-xyz")); w.Code != 200 {
		t.Fatalf("second token: %d", w.Code)
	}
	if w := do(h, "GET", "/v1/trees", bearer("wrong")); w.Code != 401 {
		t.Fatalf("wrong bearer: %d", w.Code)
	}
	// Bearer requests are not subject to the Origin check (no cookie involved).
	if w := do(h, "POST", "/v1/trees", func(r *http.Request) { bearer(tok)(r); r.Header.Set("Origin", "http://evil.example") }); w.Code != 200 {
		t.Fatalf("bearer POST with foreign origin: %d", w.Code)
	}
}

func TestSessionCookie(t *testing.T) {
	a := newAuth(tok)
	h := server(a)
	if w := do(h, "POST", "/v1/login", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{"token":"nope"}`)) }); w.Code != 401 {
		t.Fatalf("bad login: %d", w.Code)
	}
	c := login(t, h, tok)
	withCookie := func(c *http.Cookie) func(*http.Request) { return func(r *http.Request) { r.AddCookie(c) } }
	if w := do(h, "GET", "/v1/trees", withCookie(c)); w.Code != 200 {
		t.Fatalf("cookie GET: %d", w.Code)
	}
	// Same-origin write is fine; a cross-site write with the cookie is refused.
	if w := do(h, "POST", "/v1/trees", func(r *http.Request) { r.AddCookie(c); r.Header.Set("Origin", "http://example.com") }); w.Code != 200 {
		t.Fatalf("same-origin POST: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/trees", func(r *http.Request) { r.AddCookie(c); r.Header.Set("Origin", "http://evil.example") }); w.Code != 403 {
		t.Fatalf("cross-site POST must be refused: %d", w.Code)
	}
	// Tampered cookie.
	bad := *c
	bad.Value = strings.Replace(c.Value, ".", ".9", 1)
	if w := do(h, "GET", "/v1/trees", withCookie(&bad)); w.Code != 401 {
		t.Fatalf("tampered cookie: %d", w.Code)
	}
	// Expired cookie.
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if w := do(h, "GET", "/v1/trees", withCookie(c)); w.Code != 401 {
		t.Fatalf("expired cookie: %d", w.Code)
	}
	// A session from a token that has since been removed is revoked.
	a2 := newAuth("a-different-token-only")
	if w := do(server(a2), "GET", "/v1/trees", withCookie(c)); w.Code != 401 {
		t.Fatalf("revoked token's session must fail: %d", w.Code)
	}
	// Login from another site is refused.
	if w := do(h, "POST", "/v1/login", func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"token":"` + tok + `"}`))
		r.Header.Set("Origin", "http://evil.example")
	}); w.Code != 403 {
		t.Fatalf("cross-site login: %d", w.Code)
	}
}

func TestSecureCookieBehindHTTPS(t *testing.T) {
	h := server(newAuth(tok))
	r := httptest.NewRequest("POST", "/v1/login", strings.NewReader(`{"token":"`+tok+`"}`))
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Fatalf("cookie must be Secure behind HTTPS: %+v", c)
	}
}
