package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// Extra guide sources: XMLTV URLs not tied to a provider (a community or
// paid guide). They download on the guide schedule and win over provider
// guides by priority (lower wins).

func (ui *UI) mountEPGExtra(r chi.Router) {
	r.Post("/epg/sources", ui.extraCreate)
	r.Get("/epg/sources/{sid}/edit", ui.extraEdit)
	r.Post("/epg/sources/{sid}/edit", ui.extraUpdate)
	r.Post("/epg/sources/{sid}/refresh", ui.extraRefresh)
	r.Post("/epg/sources/{sid}/delete", ui.extraDelete)
}

// extraForm is the add/edit form's state.
type extraForm struct {
	ID                  int64
	Name, URL, Priority string
	Enabled             bool
	Error               string
	ProviderPriorities  string // e.g. "trex-mboard 1, kirby-trex 2", for the hint
}

type extraRow struct {
	db.ListEpgSourceStatsRow
	Refresh *refreshData
}

func (ui *UI) extraRows(r *http.Request) ([]extraRow, error) {
	stats, err := ui.Store.ListEpgSourceStats(r.Context())
	if err != nil {
		return nil, err
	}
	var out []extraRow
	for _, s := range stats {
		if s.ProviderID == nil {
			out = append(out, extraRow{ListEpgSourceStatsRow: s})
		}
	}
	return out, nil
}

// providerPriorities lists the provider guides' priorities, for the hint
// beside a new source's priority.
func (ui *UI) providerPriorities(r *http.Request) string {
	srcs, err := ui.epgSources(r)
	if err != nil {
		return ""
	}
	var parts []string
	for _, s := range srcs {
		parts = append(parts, s.Provider.Name+" "+strconv.Itoa(int(s.GuidePriority())))
	}
	return strings.Join(parts, ", ")
}

func parseExtraForm(r *http.Request) (extraForm, db.UpdateExtraEpgSourceParams, error) {
	f := extraForm{Name: strings.TrimSpace(r.PostForm.Get("name")), URL: strings.TrimSpace(r.PostForm.Get("url")),
		Priority: strings.TrimSpace(r.PostForm.Get("priority")), Enabled: r.PostForm.Get("enabled") != ""}
	p := db.UpdateExtraEpgSourceParams{Name: f.Name, Url: f.URL, Enabled: f.Enabled}
	if f.Name == "" {
		return f, p, formError("Give the guide a name.")
	}
	u, err := url.Parse(f.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return f, p, formError("Guide URL must be an http(s) address of an XMLTV file (.xml or .xml.gz).")
	}
	prio := 0
	if f.Priority != "" {
		if prio, err = strconv.Atoi(f.Priority); err != nil {
			return f, p, formError("Priority must be a whole number.")
		}
	}
	pr := int32(prio)
	p.Priority = &pr
	return f, p, nil
}

func (ui *UI) extraCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f, p, err := parseExtraForm(r)
	if err != nil {
		ui.epgPageWith(w, r, http.StatusUnprocessableEntity, f, err.Error())
		return
	}
	src, err := ui.Store.CreateExtraEpgSource(r.Context(), db.CreateExtraEpgSourceParams{Name: p.Name, Url: p.Url, Priority: p.Priority, Enabled: p.Enabled})
	if err != nil {
		ui.fail(w, "create guide source", err)
		return
	}
	ui.Log.Info("web: guide source added", "epg_source_id", src.ID)
	if src.Enabled {
		ui.downloadInBackground(src)
	}
	ui.redirect(w, r, Prefix+"/epg?msg=extra-added")
}

// downloadInBackground fetches a just-added guide without making the admin
// wait (big guides take a minute); the page shows the result on reload.
func (ui *UI) downloadInBackground(src db.EpgSource) {
	base := ui.Ctx
	if base == nil {
		base = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(base, 15*time.Minute)
		defer cancel()
		if _, err := ui.Refresh.RefreshEPGSource(ctx, src); err != nil {
			ui.Log.Warn("web: guide download", "epg_source_id", src.ID, "err", err)
		}
	}()
}

func (ui *UI) extraSource(w http.ResponseWriter, r *http.Request) (db.EpgSource, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "sid"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return db.EpgSource{}, false
	}
	s, err := ui.Store.GetEpgSource(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && s.ProviderID != nil) {
		http.NotFound(w, r) // provider guides are managed on the provider
		return db.EpgSource{}, false
	}
	if err != nil {
		ui.fail(w, "get guide source", err)
		return db.EpgSource{}, false
	}
	return s, true
}

func (ui *UI) extraEdit(w http.ResponseWriter, r *http.Request) {
	s, ok := ui.extraSource(w, r)
	if !ok {
		return
	}
	f := extraForm{ID: s.ID, Name: s.Name, URL: s.Url, Enabled: s.Enabled, ProviderPriorities: ui.providerPriorities(r)}
	if s.Priority != nil {
		f.Priority = strconv.Itoa(int(*s.Priority))
	}
	ui.render(w, r, http.StatusOK, "epg_source", "Edit guide source", f)
}

func (ui *UI) extraUpdate(w http.ResponseWriter, r *http.Request) {
	s, ok := ui.extraSource(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f, p, err := parseExtraForm(r)
	if err != nil {
		f.ID, f.Error, f.ProviderPriorities = s.ID, err.Error(), ui.providerPriorities(r)
		ui.render(w, r, http.StatusUnprocessableEntity, "epg_source", "Edit guide source", f)
		return
	}
	p.ID = s.ID
	updated, err := ui.Store.UpdateExtraEpgSource(r.Context(), p)
	if err != nil {
		ui.fail(w, "update guide source", err)
		return
	}
	if updated.Enabled && (updated.Url != s.Url || !s.Enabled) {
		ui.downloadInBackground(updated)
	}
	ui.redirect(w, r, Prefix+"/epg?msg=extra-saved")
}

func (ui *UI) extraDelete(w http.ResponseWriter, r *http.Request) {
	s, ok := ui.extraSource(w, r)
	if !ok {
		return
	}
	if _, err := ui.Store.DeleteExtraEpgSource(r.Context(), s.ID); err != nil {
		ui.fail(w, "delete guide source", err)
		return
	}
	ui.redirect(w, r, Prefix+"/epg?msg=extra-deleted")
}

// extraRefresh downloads the guide now and returns its updated row (htmx).
func (ui *UI) extraRefresh(w http.ResponseWriter, r *http.Request) {
	s, ok := ui.extraSource(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextFor(r, 15*time.Minute)
	defer cancel()
	start := time.Now()
	n, err := ui.Refresh.RefreshEPGSource(ctx, s)
	d := refreshData{OK: err == nil, Result: resultOf(n), Elapsed: time.Since(start).Round(100 * time.Millisecond)}
	if err != nil {
		ui.Log.Warn("web: guide download", "epg_source_id", s.ID, "err", err)
		d.Error = err.Error()
	}
	if r.Header.Get("HX-Request") != "true" {
		ui.redirect(w, r, Prefix+"/epg")
		return
	}
	rows, err := ui.extraRows(r)
	if err != nil {
		ui.fail(w, "guide sources", err)
		return
	}
	for _, row := range rows {
		if row.ID == s.ID {
			row.Refresh = &d
			ui.fragment(w, r, "epg", "extra-row", row)
			return
		}
	}
	http.NotFound(w, r)
}
