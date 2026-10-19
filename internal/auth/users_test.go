package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type memUsers struct {
	mu    sync.Mutex
	users map[uuid.UUID]*User
}

func (m *memUsers) ByEmail(_ context.Context, email string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Email == NormalizeEmail(email) {
			c := *u
			return &c, nil
		}
	}
	return nil, ErrNoUser
}
func (m *memUsers) ByID(_ context.Context, id uuid.UUID) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, ErrNoUser
}
func (m *memUsers) SetPassword(_ context.Context, id uuid.UUID, hash string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.users[id]
	u.Hash, u.Epoch = hash, u.Epoch+1
	return u.Epoch, nil
}
func (m *memUsers) Any(context.Context) (bool, error) { return len(m.users) > 0, nil }
func (m *memUsers) SessionKey(context.Context) ([]byte, error) {
	return []byte("test-session-key"), nil
}

const pw = "correct horse battery staple"

func newAccounts(t *testing.T, tokens ...string) (*Authenticator, http.Handler, *memUsers, *User) {
	t.Helper()
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	u := &User{ID: uuid.New(), Email: "someone@example.com", Hash: hash}
	users := &memUsers{users: map[uuid.UUID]*User{u.ID: u}}
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)), tokens, time.Hour)
	if err := a.WithUsers(context.Background(), users); err != nil {
		t.Fatal(err)
	}
	return a, server(a), users, u
}

func post(h http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func cookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie (status %d: %s)", w.Code, w.Body.String())
	return nil
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword(pw)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash %q %v", h, err)
	}
	if h2, _ := HashPassword(pw); h2 == h {
		t.Fatal("salts must differ")
	}
	if ok, _ := VerifyPassword(h, pw); !ok {
		t.Fatal("right password rejected")
	}
	if ok, _ := VerifyPassword(h, pw+"x"); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short passwords must be refused")
	}
	if _, err := VerifyPassword("$2a$10$bcrypt-style", pw); err == nil {
		t.Fatal("unknown hash formats must error")
	}
}

func TestAccountLogin(t *testing.T) {
	a, h, _, _ := newAccounts(t) // no tokens: accounts alone turn auth on
	if !a.Enabled() {
		t.Fatal("an account must enable authentication")
	}
	if w := do(h, "GET", "/v1/trees", nil); w.Code != 401 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	w := post(h, "/v1/login", `{"email":" Someone@Example.com ","password":"`+pw+`"}`)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	c := cookieOf(t, w)
	if !strings.HasPrefix(c.Value, "v2.") || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", c)
	}
	if w := do(h, "GET", "/v1/trees", func(r *http.Request) { r.AddCookie(c) }); w.Code != 200 {
		t.Fatalf("with session: %d", w.Code)
	}
	var s map[string]any
	_ = json.Unmarshal(do(h, "GET", "/v1/session", func(r *http.Request) { r.AddCookie(c) }).Body.Bytes(), &s)
	if s["user"] != "someone@example.com" || s["authenticated"] != true {
		t.Fatalf("session: %v", s)
	}
	bad := *c
	bad.Value = c.Value[:len(c.Value)-2] + "xx"
	if w := do(h, "GET", "/v1/trees", func(r *http.Request) { r.AddCookie(&bad) }); w.Code != 401 {
		t.Fatalf("tampered session: %d", w.Code)
	}
	for _, body := range []string{`{"email":"someone@example.com","password":"wrong password!!"}`, `{"email":"nobody@example.com","password":"` + pw + `"}`} {
		if w := post(h, "/v1/login", body); w.Code != 401 || !strings.Contains(w.Body.String(), "invalid email or password") {
			t.Fatalf("bad login %s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

func TestAccountLockout(t *testing.T) {
	_, h, _, _ := newAccounts(t, "a-token-for-scripts")
	for i := 0; i < maxFailures; i++ {
		post(h, "/v1/login", `{"email":"someone@example.com","password":"not the password"}`)
	}
	if w := post(h, "/v1/login", `{"email":"someone@example.com","password":"`+pw+`"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked account must refuse even the right password: %d", w.Code)
	}
	// Tokens are unaffected.
	if w := post(h, "/v1/login", `{"token":"a-token-for-scripts"}`); w.Code != 200 {
		t.Fatalf("token login: %d", w.Code)
	}
}

func TestPasswordChangeAndRevocation(t *testing.T) {
	_, h, users, u := newAccounts(t)
	old := cookieOf(t, post(h, "/v1/login", `{"email":"someone@example.com","password":"`+pw+`"}`))
	other := cookieOf(t, post(h, "/v1/login", `{"email":"someone@example.com","password":"`+pw+`"}`))
	if w := post(h, "/v1/account/password", `{"current":"wrong","new":"another long password"}`, old); w.Code != 401 {
		t.Fatalf("wrong current: %d", w.Code)
	}
	if w := post(h, "/v1/account/password", `{"current":"`+pw+`","new":"short"}`, old); w.Code != 400 {
		t.Fatalf("short new: %d", w.Code)
	}
	w := post(h, "/v1/account/password", `{"current":"`+pw+`","new":"another long password"}`, old)
	if w.Code != 200 {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	fresh := cookieOf(t, w)
	if w := do(h, "GET", "/v1/trees", func(r *http.Request) { r.AddCookie(fresh) }); w.Code != 200 {
		t.Fatalf("new session: %d", w.Code)
	}
	time.Sleep(10 * time.Millisecond)
	for _, c := range []*http.Cookie{old, other} {
		if w := do(h, "GET", "/v1/trees", func(r *http.Request) { r.AddCookie(c) }); w.Code != 401 {
			t.Fatalf("sessions from before the change must be revoked: %d", w.Code)
		}
	}
	if w := post(h, "/v1/login", `{"email":"someone@example.com","password":"another long password"}`); w.Code != 200 {
		t.Fatalf("login with the new password: %d", w.Code)
	}
	// Disabling the account ends its sessions (after the short cache).
	users.mu.Lock()
	users.users[u.ID].Disabled = true
	users.mu.Unlock()
	if w := post(h, "/v1/login", `{"email":"someone@example.com","password":"another long password"}`); w.Code != 401 {
		t.Fatalf("disabled account login: %d", w.Code)
	}
}

func TestPrincipalAndLateAccounts(t *testing.T) {
	users := &memUsers{users: map[uuid.UUID]*User{}}
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, time.Hour)
	if err := a.WithUsers(context.Background(), users); err != nil {
		t.Fatal(err)
	}
	var seen string
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = Principal(r.Context()); w.WriteHeader(200) }))
	if w := do(h, "GET", "/v1/trees", nil); w.Code != 200 {
		t.Fatalf("no accounts, no tokens: open, got %d", w.Code)
	}
	// The first account turns authentication on without a restart.
	hash, _ := HashPassword(pw)
	u := &User{ID: uuid.New(), Email: "late@example.com", Hash: hash}
	users.users[u.ID] = u
	a.refreshUsersExist(context.Background())
	if w := do(h, "GET", "/v1/trees", nil); w.Code != 401 {
		t.Fatalf("after the first account: want 401, got %d", w.Code)
	}
	value, exp := a.newUserSession(u)
	if w := do(h, "GET", "/v1/trees", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: value, Expires: exp}) }); w.Code != 200 || seen != "late@example.com" {
		t.Fatalf("account session: %d principal %q", w.Code, seen)
	}
}
