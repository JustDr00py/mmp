// Package web is the embedded admin UI: server-rendered html/template pages
// enhanced with htmx, all under /admin. It authenticates with the same
// admin sessions as the REST API (internal/auth).
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
	"github.com/sysadmin/mmp/internal/redact"
	"github.com/sysadmin/mmp/internal/source"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Prefix is where the UI is mounted.
const Prefix = "/admin"

// LiveStatus reports running channel sessions (satisfied by *proxy.Manager).
type LiveStatus interface {
	Channels() []proxy.ChannelStatus
}

// Store is the persistence the pages need (satisfied by *db.Queries).
type Store interface {
	ListProviders(ctx context.Context) ([]db.Provider, error)
	GetProvider(ctx context.Context, id int64) (db.Provider, error)
	CreateProvider(ctx context.Context, arg db.CreateProviderParams) (db.Provider, error)
	UpdateProvider(ctx context.Context, arg db.UpdateProviderParams) (db.Provider, error)
	DeleteProvider(ctx context.Context, id int64) error
	SetProviderEnabled(ctx context.Context, arg db.SetProviderEnabledParams) error
	ProviderStreamCounts(ctx context.Context) ([]db.ProviderStreamCountsRow, error)
	ListProviderStreams(ctx context.Context, arg db.ListProviderStreamsParams) ([]db.ProviderStream, error)
	SearchProviderStreams(ctx context.Context, arg db.SearchProviderStreamsParams) ([]db.ProviderStream, error)
}

// Refresher refreshes a provider on demand (satisfied by *source.Refresher).
type Refresher interface {
	RefreshProvider(ctx context.Context, p db.Provider) (source.Result, error)
}

// Deps are the UI's collaborators.
type Deps struct {
	Auth    *auth.Authenticator
	Store   Store
	Refresh Refresher
	Live    LiveStatus           // optional
	Slots   func() map[int64]int // provider slots in use; optional
	Log     *slog.Logger
	Version string
}

// UI serves the admin pages.
type UI struct {
	Deps

	pages     map[string]*template.Template
	assetHash string // busts browser caches when the embedded assets change
}

// navItem is one entry of the top navigation. Pages add themselves here
// as they are built.
type navItem struct {
	Path, Label string
}

var nav = []navItem{
	{Prefix + "/", "Dashboard"},
	{Prefix + "/providers", "Providers"},
}

// pageNames are the templates/<name>.html pages, each parsed with the layout.
var pageNames = []string{"login", "dashboard", "providers", "provider_form", "streams"}

// New parses the embedded templates.
func New(d Deps) (*UI, error) {
	ui := &UI{Deps: d, pages: map[string]*template.Template{}}
	h := sha256.New()
	err := fs.WalkDir(staticFS, "static", func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		h.Write(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	ui.assetHash = hex.EncodeToString(h.Sum(nil))[:12]

	funcs := template.FuncMap{
		"asset": func(name string) string {
			return Prefix + "/static/" + name + "?v=" + ui.assetHash
		},
		"redact":     redact.URL,
		"ago":        ago,
		"pct":        func(f float32) int { return int(f*100 + 0.5) },
		"future":     func(t *time.Time) bool { return t != nil && t.After(time.Now()) },
		"streamsURL": streamsQuery,
		"add":        func(a, b int) int { return a + b },
	}
	for _, page := range pageNames {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		ui.pages[page] = t
	}
	return ui, nil
}

// ago renders a past time coarsely ("3m ago"); nil is "never".
func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// Mount registers the UI routes on the root router.
func (ui *UI) Mount(r chi.Router) {
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"/", http.StatusFound)
	})
	r.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { // browsers ask regardless
		http.Redirect(w, r, Prefix+"/static/favicon.svg?v="+ui.assetHash, http.StatusMovedPermanently)
	})
	r.Get(Prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
	})
	r.Route(Prefix, func(r chi.Router) {
		r.Use(securityHeaders, sameOrigin)
		static, _ := fs.Sub(staticFS, "static")
		r.Handle("/static/*", http.StripPrefix(Prefix+"/static/", cacheStatic(http.FileServerFS(static))))
		r.Get("/login", ui.loginPage)
		r.Post("/login", ui.login)
		r.Group(func(r chi.Router) {
			r.Use(ui.requireAdmin)
			r.Post("/logout", ui.logout)
			r.Get("/", ui.dashboard)
			ui.mountProviders(r)
		})
	})
}

// pageData is what every page template receives.
type pageData struct {
	Title   string
	User    *db.User
	Nav     []navItem
	Active  string
	Version string
	Data    any
}

func (ui *UI) render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	ui.renderTemplate(w, r, status, page, "layout", title, data)
}

// fragment renders one named block of a page with data (not wrapped in
// pageData), for htmx swaps.
func (ui *UI) fragment(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	ui.execute(w, http.StatusOK, page, name, data)
}

func (ui *UI) renderTemplate(w http.ResponseWriter, r *http.Request, status int, page, name, title string, data any) {
	pd := pageData{Title: title, Nav: nav, Active: activeNav(r.URL.Path), Version: ui.Version, Data: data}
	if u, ok := auth.UserFrom(r.Context()); ok {
		pd.User = &u
	}
	ui.execute(w, status, page, name, pd)
}

func (ui *UI) execute(w http.ResponseWriter, status int, page, name string, data any) {
	var buf bytes.Buffer // render fully first: a template error must not leave half a page
	if err := ui.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		ui.Log.Error("web: render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// activeNav is the nav entry a path belongs to (its longest prefix).
func activeNav(path string) string {
	best := ""
	for _, n := range nav {
		if (path == n.Path || strings.HasPrefix(path, n.Path+"/")) && len(n.Path) > len(best) {
			best = n.Path
		}
	}
	return best
}

// redirect after a POST: htmx gets HX-Redirect (a full navigation), plain
// forms a 303.
func (ui *UI) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// requireAdmin sends visitors without a valid admin session to the login
// page, remembering where they were going.
func (ui *UI) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := ui.Auth.Authenticate(r)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthorized) {
				ui.Log.Error("web: auth lookup", "err", err)
				http.Error(w, "auth unavailable", http.StatusInternalServerError)
				return
			}
			target := Prefix + "/login"
			if r.Method == http.MethodGet && r.URL.Path != Prefix+"/" {
				target += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			if r.Header.Get("HX-Request") == "true" { // htmx: full-page redirect, not a swapped fragment
				w.Header().Set("HX-Redirect", target)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

type loginData struct {
	Username string
	Next     string
	Error    string
}

func (ui *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := ui.Auth.Authenticate(r); err == nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	ui.render(w, r, http.StatusOK, "login", "Sign in", loginData{Next: safeNext(r.URL.Query().Get("next"))})
}

func (ui *UI) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username, password := strings.TrimSpace(r.PostForm.Get("username")), r.PostForm.Get("password")
	next := safeNext(r.PostForm.Get("next"))
	token, _, err := ui.Auth.Login(r.Context(), username, password)
	if err != nil {
		status, msg := http.StatusUnauthorized, "Invalid username or password."
		if !errors.Is(err, auth.ErrUnauthorized) {
			ui.Log.Error("web: login", "err", err)
			status, msg = http.StatusInternalServerError, "Sign-in is unavailable right now."
		}
		ui.render(w, r, status, "login", "Sign in", loginData{Username: username, Next: next, Error: msg})
		return
	}
	ui.Auth.SetSessionCookie(w, token)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (ui *UI) logout(w http.ResponseWriter, r *http.Request) {
	if err := ui.Auth.Logout(r.Context(), r); err != nil {
		ui.Log.Warn("web: logout", "err", err)
	}
	ui.Auth.SetSessionCookie(w, "")
	http.Redirect(w, r, Prefix+"/login", http.StatusSeeOther)
}

type dashboardData struct {
	Sessions, Streaming, Viewers int
}

func (ui *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	var d dashboardData
	if ui.Live != nil {
		for _, c := range ui.Live.Channels() {
			d.Sessions++
			d.Viewers += c.Viewers
			if c.State == "streaming" {
				d.Streaming++
			}
		}
	}
	ui.render(w, r, http.StatusOK, "dashboard", "Dashboard", d)
}

// safeNext keeps post-login redirects inside the admin UI: anything that
// is not a plain /admin/ path (absolute URLs, //host, backslashes) falls
// back to the dashboard.
func safeNext(next string) string {
	if !strings.HasPrefix(next, Prefix+"/") || strings.ContainsAny(next, "\\\r\n") || strings.HasPrefix(next, "//") {
		return Prefix + "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return Prefix + "/"
	}
	return next
}

// securityHeaders: no framing, no third-party anything, no MIME sniffing.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https: http:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects state-changing requests sent from another site. The
// session cookie is SameSite=Strict already; this also covers the login
// form (login CSRF) and browsers that ignore SameSite.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !originMatches(r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func originMatches(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		// Old browsers and non-browser clients send no Origin; fall back to
		// Referer when present.
		ref := r.Header.Get("Referer")
		if ref == "" {
			return origin == ""
		}
		origin = ref
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// cacheStatic lets browsers keep versioned assets (?v=hash) for a year.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}
