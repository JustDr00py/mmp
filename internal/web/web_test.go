package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// fakeAuthStore is an in-memory auth.Querier.
type fakeAuthStore struct {
	mu       sync.Mutex
	users    map[string]db.User
	sessions map[string]int64 // token hash -> user id
}

func (f *fakeAuthStore) GetUserByUsername(_ context.Context, n string) (db.User, error) {
	if u, ok := f.users[n]; ok {
		return u, nil
	}
	return db.User{}, pgx.ErrNoRows
}

func (f *fakeAuthStore) CreateSession(_ context.Context, a db.CreateSessionParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[string(a.TokenHash)] = a.UserID
	return nil
}

func (f *fakeAuthStore) GetSessionUser(_ context.Context, h []byte) (db.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.sessions[string(h)]
	if !ok {
		return db.User{}, pgx.ErrNoRows
	}
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return db.User{}, pgx.ErrNoRows
}

func (f *fakeAuthStore) DeleteSession(_ context.Context, h []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, string(h))
	return nil
}

func (f *fakeAuthStore) GetAPITokenUser(context.Context, []byte) (db.GetAPITokenUserRow, error) {
	return db.GetAPITokenUserRow{}, pgx.ErrNoRows
}

func (f *fakeAuthStore) TouchAPIToken(context.Context, int64) error { return nil }

type fakeLive []proxy.ChannelStatus

func (f fakeLive) Channels() []proxy.ChannelStatus { return f }

func newUI(t *testing.T) (*httptest.Server, *fakeAuthStore) {
	srv, store, _ := newUIWith(t)
	return srv, store
}

func newUIWith(t *testing.T) (*httptest.Server, *fakeAuthStore, *UI) {
	t.Helper()
	auth.BcryptCost = bcrypt.MinCost
	hash := func(pw string) string {
		h, err := auth.HashPassword(pw)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	store := &fakeAuthStore{sessions: map[string]int64{}, users: map[string]db.User{
		"boss":   {ID: 1, Username: "boss", PasswordHash: hash("boss-pass-1"), Role: auth.RoleAdmin, Enabled: true},
		"viewer": {ID: 2, Username: "viewer", PasswordHash: hash("viewer-pass-1"), Role: auth.RoleCustomer, Enabled: true},
	}}
	live := fakeLive{
		{SessionStatus: proxy.SessionStatus{ChannelID: 1, State: "streaming", StreamID: 7, ProviderID: 1, Since: time.Now()}, Viewers: 3},
		{SessionStatus: proxy.SessionStatus{ChannelID: 2, State: "waiting"}, Viewers: 1},
	}
	ui, err := New(Deps{Auth: auth.NewAuthenticator(store, nil, 0, false), Store: newFakeStore(), Refresh: &fakeRefresher{},
		Live: live, Slots: func() map[int64]int { return map[int64]int{1: 1} }, Log: slog.New(slog.DiscardHandler), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	ui.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store, ui
}

// browser is a cookie-keeping client that does not follow redirects and
// sends same-origin form posts like a browser would.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *browser) get(path string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodGet, b.base+path, nil)
	return b.do(req)
}

func (b *browser) post(path string, form url.Values, origin string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return b.do(req)
}

func TestLoginLockout(t *testing.T) {
	srv, store, ui := newUIWith(t)
	ui.LoginLimit = auth.NewLimiter(3, time.Minute)
	b := newBrowser(t, srv.URL)

	bad := url.Values{"username": {"boss"}, "password": {"wrong-pass-1"}}
	for range 3 {
		if resp, body := b.post("/admin/login", bad, srv.URL); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt = %d\n%s", resp.StatusCode, body)
		}
	}
	// The correct password is refused too: the IP and username are locked.
	resp, body := b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, srv.URL)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many attempts") {
		t.Fatalf("locked login = %d\n%s", resp.StatusCode, body)
	}
	// The lock is per IP: every account from this client is refused.
	if resp, _ := b.post("/admin/login", url.Values{"username": {"viewer"}, "password": {"viewer-pass-1"}}, srv.URL); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("other account from locked IP = %d", resp.StatusCode)
	}
	if len(store.sessions) != 0 { // nothing about the lock creates sessions
		t.Fatal("session created during lockout")
	}
}

func TestLoginFlow(t *testing.T) {
	srv, store := newUI(t)
	b := newBrowser(t, srv.URL)

	// Signed out: the dashboard sends you to the login page.
	resp, _ := b.get("/admin/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/login" {
		t.Fatalf("dashboard signed out = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := b.get("/admin/login")
	if resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) || !strings.Contains(body, `action="/admin/login"`) {
		t.Fatalf("login page = %d\n%s", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}

	// Wrong password, unknown user, and a customer account: all refused
	// with the same message and no session.
	for _, f := range []url.Values{
		{"username": {"boss"}, "password": {"wrong-pass-1"}},
		{"username": {"ghost"}, "password": {"boss-pass-1"}},
		{"username": {"viewer"}, "password": {"viewer-pass-1"}},
	} {
		resp, body := b.post("/admin/login", f, srv.URL)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Invalid username or password.") {
			t.Errorf("%s: %d", f.Get("username"), resp.StatusCode)
		}
		if len(resp.Cookies()) != 0 || len(store.sessions) != 0 {
			t.Errorf("%s: session created", f.Get("username"))
		}
		if strings.Contains(body, f.Get("password")) {
			t.Errorf("%s: password echoed back", f.Get("username"))
		}
	}

	// Correct credentials: session cookie, then the dashboard.
	resp, _ = b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, srv.URL)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/" {
		t.Fatalf("login = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", cookie)
	}
	resp, body = b.get("/admin/")
	if resp.StatusCode != 200 || !strings.Contains(body, "boss") || !strings.Contains(body, "Sign out") {
		t.Fatalf("dashboard = %d\n%s", resp.StatusCode, body)
	}
	// 2 sessions, 1 streaming, 4 viewers.
	for _, want := range []string{`<span class="num">1<span class="of"> / 2</span>`, `<span class="num">4</span>`, "Channel 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %s", want)
		}
	}
	if resp, _ := b.get("/admin/login"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("login page when signed in = %d, want redirect", resp.StatusCode)
	}

	// Sign out: the session is gone server-side, not only the cookie.
	resp, _ = b.post("/admin/logout", nil, srv.URL)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/login" || len(store.sessions) != 0 {
		t.Fatalf("logout = %d %q, sessions %d", resp.StatusCode, resp.Header.Get("Location"), len(store.sessions))
	}
	if resp, _ := b.get("/admin/"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("dashboard after logout = %d", resp.StatusCode)
	}
}

func TestLoginNextRedirect(t *testing.T) {
	srv, _ := newUI(t)
	b := newBrowser(t, srv.URL)
	resp, body := b.get("/admin/login?next=" + url.QueryEscape("/admin/channels?q=x"))
	if resp.StatusCode != 200 || !strings.Contains(body, `value="/admin/channels?q=x"`) {
		t.Fatalf("next not carried into the form:\n%s", body)
	}
	resp, _ = b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}, "next": {"/admin/channels?q=x"}}, srv.URL)
	if loc := resp.Header.Get("Location"); loc != "/admin/channels?q=x" {
		t.Errorf("redirect after login = %q", loc)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/admin/",
		"/admin/users":           "/admin/users",
		"/admin/users?id=3":      "/admin/users?id=3",
		"https://evil.example/":  "/admin/",
		"//evil.example/admin/":  "/admin/",
		"/admin/\\evil":          "/admin/",
		"/adminx":                "/admin/",
		"/admin/x\r\nSet-Cookie": "/admin/",
		"/api/v1/users":          "/admin/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCrossOriginPostsRefused(t *testing.T) {
	srv, store := newUI(t)
	b := newBrowser(t, srv.URL)
	good := url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}
	resp, _ := b.post("/admin/login", good, "https://evil.example")
	if resp.StatusCode != http.StatusForbidden || len(store.sessions) != 0 {
		t.Errorf("cross-origin login = %d (sessions %d), want 403", resp.StatusCode, len(store.sessions))
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/login", strings.NewReader(good.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, _ := b.do(req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site cross-site = %d, want 403", resp.StatusCode)
	}
	// No Origin at all (curl, old browsers) is allowed.
	if resp, _ := b.post("/admin/login", good, ""); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("origin-less login = %d", resp.StatusCode)
	}
}

func TestHTMXUnauthenticated(t *testing.T) {
	srv, _ := newUI(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/", nil)
	req.Header.Set("HX-Request", "true")
	resp, _ := newBrowser(t, srv.URL).do(req)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("HX-Redirect") != "/admin/login" {
		t.Errorf("htmx request signed out = %d %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
}

func TestStaticAssets(t *testing.T) {
	srv, _ := newUI(t)
	b := newBrowser(t, srv.URL)
	_, page := b.get("/admin/login")
	for _, name := range []string{"htmx.min.js", "app.css"} {
		i := strings.Index(page, "/admin/static/"+name+"?v=")
		if i < 0 {
			t.Fatalf("login page does not reference %s with a version", name)
		}
		ref := page[i : i+strings.IndexByte(page[i:], '"')]
		resp, body := b.get(ref)
		if resp.StatusCode != 200 || len(body) < 100 {
			t.Errorf("%s = %d (%d bytes)", ref, resp.StatusCode, len(body))
		}
		if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s cache-control = %q", ref, resp.Header.Get("Cache-Control"))
		}
	}
	if resp, body := b.get("/admin/static/htmx.min.js"); !strings.Contains(resp.Header.Get("Content-Type"), "javascript") || !strings.HasPrefix(body, "var htmx") {
		t.Errorf("htmx = %s", resp.Header.Get("Content-Type"))
	}
	if resp, _ := b.get("/"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/" {
		t.Errorf("/ = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
