package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// The audit log lists what admins did: sign-ins (and failed ones), every
// change through the admin UI or API, and backup downloads.

const auditPerPage = 50

func (ui *UI) mountAudit(r chi.Router) {
	r.Get("/audit", ui.auditPage)
}

type auditRow struct {
	When, Full string
	Username   string
	Known      bool // Username is an admin account (not a failed sign-in's guess)
	IP, Via    string
	Failed     bool // a failed sign-in
	Action     string
	Target     string // "provider “Atlas TV”", or "" for a page-level action
	TargetLink string // the target's page, when it still exists
	Path       string
}

// bareActions read as "<action> <type>" when the target has no id: a
// create, or an action on a whole list.
var bareActions = map[string]bool{"created": true, "deleted": true, "updated": true, "uploaded": true}

// targetPages are the admin pages of targets, by type.
var targetPages = map[string]string{
	"provider": "/providers/%s/edit", "channel": "/channels/%s", "bouquet": "/bouquets/%s", "user": "/users/%s",
	"stream profile": "/profiles/%s", "guide source": "/epg/sources/%s/edit",
}

func auditRowOf(e db.AdminAudit, today time.Time) auditRow {
	at := e.Ts.Local()
	row := auditRow{When: at.Format("Jan 2 15:04"), Full: at.Format("2006-01-02 15:04:05 MST"), Username: e.Username,
		Known: e.UserID != nil, IP: e.Ip, Via: e.Via, Failed: e.Action == "failed to sign in", Action: e.Action, Path: e.Path}
	if sameDay(at, today) {
		row.When = at.Format("15:04:05")
	}
	switch {
	case e.TargetID != "":
		name := e.TargetName
		if name == "" {
			name = "#" + e.TargetID
		} else {
			name = "“" + name + "”"
		}
		row.Target = e.TargetType + " " + name
		if page, ok := targetPages[e.TargetType]; ok && e.TargetName != "" && e.Action != "deleted" {
			row.TargetLink = Prefix + strings.Replace(page, "%s", url.PathEscape(e.TargetID), 1)
		}
	case bareActions[e.Action] && e.TargetType != "":
		row.Target = e.TargetType
	}
	return row
}

type auditData struct {
	Rows       []auditRow
	Usernames  []string
	Types      []string
	Username   string
	Type       string
	Offset     int
	PrevOffset int
	NextOffset int
	Query      func(offset int) string
}

func (ui *UI) auditPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := auditData{Username: q.Get("admin"), Type: q.Get("type"), Offset: max(atoi(q.Get("offset")), 0)}
	params := db.ListAuditParams{Limit: auditPerPage + 1, Offset: int32(d.Offset)}
	if d.Username != "" {
		params.Username = &d.Username
	}
	if d.Type != "" {
		params.TargetType = &d.Type
	}
	ctx := r.Context()
	entries, err := ui.Store.ListAudit(ctx, params)
	if err != nil {
		ui.fail(w, "audit log", err)
		return
	}
	if len(entries) > auditPerPage {
		entries, d.NextOffset = entries[:auditPerPage], d.Offset+auditPerPage
	}
	d.PrevOffset = max(d.Offset-auditPerPage, 0)
	today := time.Now().Local()
	for _, e := range entries {
		d.Rows = append(d.Rows, auditRowOf(e, today))
	}
	if d.Usernames, err = ui.Store.AuditUsernames(ctx); err != nil {
		ui.fail(w, "audit log", err)
		return
	}
	if d.Types, err = ui.Store.AuditTargetTypes(ctx); err != nil {
		ui.fail(w, "audit log", err)
		return
	}
	d.Query = func(offset int) string {
		v := url.Values{}
		if d.Username != "" {
			v.Set("admin", d.Username)
		}
		if d.Type != "" {
			v.Set("type", d.Type)
		}
		if offset > 0 {
			v.Set("offset", strconv.Itoa(offset))
		}
		return Prefix + "/audit?" + v.Encode()
	}
	ui.render(w, r, http.StatusOK, "audit", "Audit log", d)
}
