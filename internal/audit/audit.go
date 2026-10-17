// Package audit records what admins do: sign-ins and failed sign-ins,
// every change made through the admin UI or API, and backup downloads. A
// middleware describes each change from its URL, so new admin routes are
// covered without extra code. Request bodies (which can hold passwords)
// are never stored.
package audit

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

// Store persists entries (satisfied by *db.Queries).
type Store interface {
	InsertAudit(ctx context.Context, arg db.InsertAuditParams) error
}

// Namer names a target ("provider", "7") for the log, or "" when it has
// none or no longer exists.
type Namer func(ctx context.Context, targetType, id string) string

// Recorder writes audit entries. A nil Recorder records nothing.
type Recorder struct {
	Store Store
	Namer Namer // optional
	Log   *slog.Logger
	Now   func() time.Time // tests
}

// Entry is one audit record.
type Entry struct {
	User                             *db.User // nil for a failed sign-in
	Username                         string   // as typed, for a failed sign-in
	IP                               string
	Action                           string
	TargetType, TargetID, TargetName string
	Method, Path, Via                string
}

// Record writes e. A failed write is logged, never returned: auditing must
// not break the admin action it describes.
func (rec *Recorder) Record(ctx context.Context, e Entry) {
	if rec == nil {
		return
	}
	at := time.Now()
	if rec.Now != nil {
		at = rec.Now()
	}
	p := db.InsertAuditParams{Ts: at, Username: e.Username, Ip: e.IP, Action: e.Action, TargetType: e.TargetType,
		TargetID: e.TargetID, TargetName: e.TargetName, Method: e.Method, Path: e.Path, Via: e.Via}
	if e.User != nil {
		p.UserID, p.Username = &e.User.ID, e.User.Username
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := rec.Store.InsertAudit(ctx, p); err != nil && rec.Log != nil {
		rec.Log.Warn("audit record failed", "action", e.Action, "err", err)
	}
}

// SignIn records a sign-in attempt: u is the admin on success, nil on a
// failure (username is what was typed). via is "ui" or "api".
func (rec *Recorder) SignIn(r *http.Request, u *db.User, username, via string) {
	action := "signed in"
	if u == nil {
		action = "failed to sign in"
	}
	rec.Record(r.Context(), Entry{User: u, Username: username, IP: auth.RemoteIP(r), Action: action,
		Method: r.Method, Path: r.URL.Path, Via: via})
}

// Middleware records each successful change (any method but GET, HEAD and
// OPTIONS, plus downloads) made by the admin authenticated earlier in the
// chain. Mount it after the admin check. via is "ui" or "api".
func (rec *Recorder) Middleware(via string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if rec == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !audited(r) {
				next.ServeHTTP(w, r)
				return
			}
			d := Describe(r.Method, r.URL.Path)
			name := ""
			if d.TargetID != "" && rec.Namer != nil { // before the handler: a delete removes it
				name = rec.Namer(r.Context(), d.TargetType, d.TargetID)
			}
			u, _ := auth.UserFrom(r.Context())
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			if sw.status >= 400 {
				return // refused or failed: nothing changed
			}
			if d.TargetID == "" { // a create: the redirect names the new item
				loc := cmp.Or(sw.Header().Get("Location"), sw.Header().Get("HX-Redirect")) // htmx forms redirect by header
				d.TargetID = createdID(loc, d.Resource)
			}
			if d.TargetID != "" && rec.Namer != nil { // its name now (a rename), unless it is gone
				if now := rec.Namer(r.Context(), d.TargetType, d.TargetID); now != "" {
					name = now
				}
			}
			e := Entry{IP: auth.RemoteIP(r), Action: d.Action, TargetType: d.TargetType, TargetID: d.TargetID,
				TargetName: name, Method: r.Method, Path: r.URL.Path, Via: via}
			if u.ID != 0 {
				e.User = &u
			}
			rec.Record(r.Context(), e)
		})
	}
}

func audited(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return strings.HasSuffix(r.URL.Path, "/download")
	case http.MethodOptions:
		return false
	}
	return true
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (backup downloads) working.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// resources are the URL words that name a kind of target, with its label.
var resources = map[string]string{
	"providers": "provider", "channels": "channel", "groups": "channel group", "channel-groups": "channel group",
	"bouquets": "bouquet", "users": "user", "profiles": "stream profile", "stream-profiles": "stream profile",
	"sources": "guide source", "rules": "match rule", "backups": "backup", "viewers": "viewer",
	"tokens": "API token", "settings": "settings", "vod-categories": "movie category",
}

// verbs describe the URL word after a target.
var verbs = map[string]string{
	"delete": "deleted", "remove": "removed from", "edit": "updated", "refresh": "refreshed",
	"toggle": "turned on or off", "password": "reset the password of", "token": "rotated the stream token of",
	"rotate-token": "rotated the stream token of", "restore": "restored", "download": "downloaded",
	"stop": "stopped", "switch": "switched the stream of", "kick": "disconnected", "add": "added channels to",
	"add-group": "added a group of channels to", "groups": "changed the groups of", "streams": "added a stream to",
	"order": "reordered the streams of", "move": "moved a stream of", "vod": "refreshed movies and series of",
	"vod-categories": "added movie categories to", "schedule": "changed the backup schedule",
	"upload": "uploaded", "fill": "filled channel groups", "group": "moved channels to a group",
	"apply": "applied matches", "create": "created", "new": "created", "logout": "signed out",
	"match": "changed automatic matching", "events": "changed the history setting",
}

// Description is what Describe makes of a request.
type Description struct {
	Action, TargetType, TargetID string
	Resource                     string // the URL word of the target ("providers")
}

var idSegment = regexp.MustCompile(`^[0-9]+$|^mmp-[A-Za-z0-9._-]+$`) // row ids, backup names

// Describe reads an admin request's path: its target (the first resource
// word followed by an id) and what was done to it (the word after, or the
// method).
func Describe(method, path string) Description {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) > 0 && segs[0] == "admin" {
		segs = segs[1:]
	} else if len(segs) > 1 && segs[0] == "api" {
		segs = segs[2:] // api/v1
	}
	var d Description
	rest := segs
	for i := 0; i+1 < len(segs); i++ {
		if label, ok := resources[segs[i]]; ok && idSegment.MatchString(segs[i+1]) {
			d.Resource, d.TargetType, d.TargetID = segs[i], label, segs[i+1]
			rest = segs[i+2:]
			break
		}
	}
	if d.TargetID == "" { // no id: a create or a page-level action
		for i := len(segs) - 1; i >= 0; i-- {
			if label, ok := resources[segs[i]]; ok {
				d.Resource, d.TargetType = segs[i], label
				rest = segs[i+1:]
				break
			}
		}
		if d.Resource == "" && len(segs) > 0 {
			rest = segs[len(segs)-1:]
		}
	}
	// The last word that means something says what happened.
	for i := len(rest) - 1; i >= 0; i-- {
		if v, ok := verbs[rest[i]]; ok {
			d.Action = v
			return d
		}
	}
	switch {
	case method == http.MethodDelete:
		d.Action = "deleted"
	case d.TargetID != "":
		d.Action = "updated"
	case d.Resource != "":
		d.Action = "created"
	default:
		d.Action = strings.ToLower(method) + " " + strings.Join(segs, "/")
	}
	return d
}

// createdID finds the new item's id in a create's redirect
// ("/admin/channels/12?msg=created").
func createdID(location, resource string) string {
	if location == "" || resource == "" {
		return ""
	}
	path, _, _ := strings.Cut(location, "?")
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == resource && idSegment.MatchString(segs[i+1]) {
			return segs[i+1]
		}
	}
	return ""
}
