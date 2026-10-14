// Package web is the embedded admin UI: server-rendered html/template pages
// enhanced with htmx, all under /admin. It authenticates with the same
// admin sessions as the REST API (internal/auth).
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
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

// UI serves the admin pages.
type UI struct {
	Auth    *auth.Authenticator
	Live    LiveStatus // optional
	Log     *slog.Logger
	Version string

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
}

// New parses the embedded templates.
func New(a *auth.Authenticator, live LiveStatus, log *slog.Logger, version string) (*UI, error) {
	ui := &UI{Auth: a, Live: live, Log: log, Version: version, pages: map[string]*template.Template{}}
	h := sha256.New()
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
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

	funcs := template.FuncMap{"asset": func(name string) string {
		return Prefix + "/static/" + name + "?v=" + ui.assetHash
	}}
	for _, page := range []string{"login", "dashboard"} {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		ui.pages[page] = t
	}
	return ui, nil
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
	pd := pageData{Title: title, Nav: nav, Active: r.URL.Path, Version: ui.Version, Data: data}
	if u, ok := auth.UserFrom(r.Context()); ok {
		pd.User = &u
	}
	var buf bytes.Buffer // render fully first: a template error must not leave half a page
	if err := ui.pages[page].ExecuteTemplate(&buf, "layout", pd); err != nil {
		ui.Log.Error("web: render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
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
