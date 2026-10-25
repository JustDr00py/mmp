package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// streamsPerPage is the stream browser's page size.
const streamsPerPage = 50

// refreshTimeout bounds an on-demand refresh started from the UI.
const refreshTimeout = 5 * time.Minute

func (ui *UI) mountProviders(r chi.Router) {
	r.Get("/providers", ui.providersPage)
	r.Get("/providers/new", ui.providerNew)
	r.Post("/providers/new", ui.providerCreate)
	r.Post("/providers/bulk", ui.providersBulk)
	r.Route("/providers/{id}", func(r chi.Router) {
		r.Get("/edit", ui.providerEdit)
		r.Post("/edit", ui.providerUpdate)
		r.Post("/delete", ui.providerDelete)
		r.Post("/toggle", ui.providerToggle)
		r.Post("/refresh", ui.providerRefresh)
		r.Get("/streams", ui.streamsPage)
		r.Get("/groups", ui.providerGroupsPage)
		r.Post("/groups", ui.providerGroupsSave)
	})
}

// providerRow is one line of the providers table.
type providerRow struct {
	db.Provider
	Streams, Active, Cooling int64
	SlotsInUse               int
	Groups, GroupsSelected   int64           // stream groups, and how many are selected
	Refresh                  *refreshData    // outcome of a refresh just run from this row
	VODRefresh               *vodRefreshData // outcome of a catalog import just run from this row
}

// flashes are the fixed messages ?msg= may select: never reflected text.
var flashes = map[string]string{
	"created": "Provider added. Refresh it to import its streams, then choose its groups: none of its streams are matched until you do.",
	"saved":   "Provider saved.",
	"deleted": "Provider deleted.",
}

// providersPerPage is the providers table's page size.
const providersPerPage = 100

type providersData struct {
	Rows       []providerRow
	Flash      string
	Filter     providerFilter
	Backbones  []db.ListBackbonesRow
	Unlabeled  int // providers without a backbone
	Total      int // providers before filtering
	Matching   int // providers passing the filter
	From, To   int
	Prev       int  // offset of the previous page, -1 = none
	Next       int  // offset of the next page, -1 = none
	Refreshing bool // a bulk refresh is running
}

func (ui *UI) providerRows(r *http.Request, only int64) ([]providerRow, error) {
	ps, err := ui.Store.ListProviders(r.Context())
	if err != nil {
		return nil, err
	}
	counts, err := ui.Store.ProviderStreamCounts(r.Context())
	if err != nil {
		return nil, err
	}
	byID := map[int64]db.ProviderStreamCountsRow{}
	for _, c := range counts {
		byID[c.ProviderID] = c
	}
	groups, err := ui.Store.ProviderGroupSummary(r.Context())
	if err != nil {
		return nil, err
	}
	groupsByID := map[int64]db.ProviderGroupSummaryRow{}
	for _, g := range groups {
		groupsByID[g.ProviderID] = g
	}
	var slots map[int64]int
	if ui.Slots != nil {
		slots = ui.Slots()
	}
	var rows []providerRow
	for _, p := range ps {
		if only != 0 && p.ID != only {
			continue
		}
		c := byID[p.ID]
		g := groupsByID[p.ID]
		rows = append(rows, providerRow{Provider: p, Streams: c.Total, Active: c.Active, Cooling: c.Cooling, SlotsInUse: slots[p.ID],
			Groups: g.Total, GroupsSelected: g.Selected})
	}
	return rows, nil
}

func (ui *UI) providersPage(w http.ResponseWriter, r *http.Request) {
	rows, err := ui.providerRows(r, 0)
	if err != nil {
		ui.fail(w, "list providers", err)
		return
	}
	backbones, err := ui.Store.ListBackbones(r.Context())
	if err != nil {
		ui.fail(w, "list backbones", err)
		return
	}
	q := r.URL.Query()
	f := parseProviderFilter(q)
	d := providersData{Filter: f, Backbones: backbones, Total: len(rows), Prev: -1, Next: -1,
		Flash: flashes[q.Get("msg")], Refreshing: ui.bulkRefreshing.Load()}
	if msg, ok := bulkFlash(q); ok {
		d.Flash = msg
	}
	for _, p := range rows {
		if p.Backbone == "" {
			d.Unlabeled++
		}
	}
	shown := slices.DeleteFunc(rows, func(p providerRow) bool { return !f.keep(p.Provider) })
	d.Matching = len(shown)
	if f.Offset >= len(shown) {
		f.Offset = max(0, (len(shown)-1)/providersPerPage*providersPerPage)
		d.Filter.Offset = f.Offset
	}
	d.Rows = shown[f.Offset:min(f.Offset+providersPerPage, len(shown))]
	if len(d.Rows) > 0 {
		d.From, d.To = f.Offset+1, f.Offset+len(d.Rows)
	}
	if f.Offset > 0 {
		d.Prev = max(f.Offset-providersPerPage, 0)
	}
	if d.To < len(shown) {
		d.Next = d.To
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "providers" {
		ui.fragment(w, r, "providers", "providers-table", d)
		return
	}
	ui.render(w, r, http.StatusOK, "providers", "Providers", d)
}

// providerForm is the create/edit form's state. The password is never
// rendered back: on edit, blank keeps the stored one.
type providerForm struct {
	ID                                int64
	Name, Type, URL, Username, EPGURL string
	UserAgent, Backbone               string
	Backbones                         []db.ListBackbonesRow // the picker's choices
	NewBackbone                       bool                  // Backbone is not one of them
	MaxConnections, RefreshMinutes    string
	Priority                          string
	MaxDisablePct                     string // blank = server default
	VODEnabled                        bool
	Enabled                           bool
	ResortChannels                    bool // on edit: sort the channels using it after saving
	HasPassword                       bool
	Error                             string

	// On edit: the imported movie/series catalog, and what a delete just
	// removed.
	Catalog       db.ProviderVODSizeRow
	VODDeleted    bool
	DeletedMovies int
	DeletedSeries int
}

func formFromProvider(p db.Provider) providerForm {
	f := providerForm{
		ID: p.ID, Name: p.Name, Type: p.Type, URL: p.Url, Username: p.Username, EPGURL: p.EpgUrl,
		UserAgent: p.UserAgent, Backbone: p.Backbone, MaxConnections: strconv.Itoa(int(p.MaxConnections)),
		RefreshMinutes: strconv.Itoa(int(p.RefreshIntervalSecs / 60)), Priority: strconv.Itoa(int(p.Priority)),
		Enabled: p.Enabled, HasPassword: p.Password != "", VODEnabled: p.VodEnabled,
	}
	if p.RefreshMaxDisablePct != nil {
		f.MaxDisablePct = strconv.Itoa(int(*p.RefreshMaxDisablePct))
	}
	return f
}

// defaultRefreshMinutes is refresh.interval in whole minutes (at least 1).
func (ui *UI) defaultRefreshMinutes() int {
	if ui.DefaultRefresh <= 0 {
		return 360
	}
	return max(int(ui.DefaultRefresh/time.Minute), 1)
}

// parsed is a validated form.
type parsedProvider struct {
	name, typ, url, username, password, epgURL, userAgent, backbone string
	maxConns, refreshSecs, priority                                 int32
	maxDisablePct                                                   *int32
	enabled                                                         bool
}

func parseProviderForm(w http.ResponseWriter, r *http.Request, defaultMinutes int) (providerForm, parsedProvider, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return providerForm{}, parsedProvider{}, errors.New("could not read the form")
	}
	f := r.PostForm
	trim := func(k string) string { return strings.TrimSpace(f.Get(k)) }
	form := providerForm{
		Name: trim("name"), Type: trim("type"), URL: trim("url"), Username: trim("username"),
		EPGURL: trim("epg_url"), UserAgent: trim("user_agent"), MaxConnections: trim("max_connections"),
		RefreshMinutes: trim("refresh_minutes"), Priority: trim("priority"), MaxDisablePct: trim("max_disable_pct"),
		Enabled: f.Get("enabled") == "on", VODEnabled: f.Get("vod_enabled") == "on",
		ResortChannels: f.Get("resort_channels") == "on",
	}
	var berr error
	form.Backbone, berr = pickedBackbone(f, "backbone")
	form.NewBackbone = berr != nil
	num := func(s, field string, def int) (int32, error) {
		if s == "" {
			return int32(def), nil
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < -1_000_000 || n > 1_000_000 {
			return 0, formError(field + " must be a whole number.")
		}
		return int32(n), nil
	}
	p := parsedProvider{name: form.Name, typ: form.Type, url: form.URL, username: form.Username,
		password: f.Get("password"), epgURL: form.EPGURL, userAgent: form.UserAgent, backbone: form.Backbone, enabled: form.Enabled}
	if berr != nil {
		return form, p, berr
	}
	var err error
	if p.maxConns, err = num(form.MaxConnections, "Max connections", 1); err != nil {
		return form, p, err
	}
	mins, err := num(form.RefreshMinutes, "Refresh interval", defaultMinutes)
	if err != nil {
		return form, p, err
	}
	p.refreshSecs = mins * 60
	if p.priority, err = num(form.Priority, "Priority", 0); err != nil {
		return form, p, err
	}
	if form.MaxDisablePct != "" { // blank keeps the server default
		n, err := strconv.Atoi(form.MaxDisablePct)
		if err != nil {
			return form, p, formError("Max % disabled must be a whole number.")
		}
		pct := int32(n)
		if err := source.ValidateRefreshMaxDisablePct(&pct); err != nil {
			return form, p, formError("Max % disabled must be within 0-100.")
		}
		p.maxDisablePct = &pct
	}
	if err := source.ValidateProvider(p.name, p.typ, p.url, p.epgURL, p.maxConns, p.refreshSecs); err != nil {
		return form, p, errors.New(humanize(err.Error()))
	}
	return form, p, nil
}

// humanize turns the shared validator's API wording into form wording.
func humanize(msg string) string {
	r := strings.NewReplacer("epg_url", "EPG URL", "url", "URL", "max_connections", "Max connections",
		"refresh_interval_secs must be > 0", "Refresh interval must be at least 1 minute", `"m3u" or "xtream"`, "M3U or Xtream")
	msg = r.Replace(msg)
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

func (ui *UI) providerNew(w http.ResponseWriter, r *http.Request) {
	f := providerForm{Type: "xtream", MaxConnections: "1", RefreshMinutes: strconv.Itoa(ui.defaultRefreshMinutes()), Priority: "0", Enabled: true}
	if !ui.withBackbones(w, r, &f) {
		return
	}
	ui.render(w, r, http.StatusOK, "provider_form", "Add provider", f)
}

// withBackbones loads the backbone picker's choices into f, answering 500
// itself on failure. A label that is not among them (one being added)
// shows in the "Add new backbone" field.
func (ui *UI) withBackbones(w http.ResponseWriter, r *http.Request, f *providerForm) bool {
	var err error
	if f.Backbones, err = ui.Store.ListBackbones(r.Context()); err != nil {
		ui.fail(w, "list backbones", err)
		return false
	}
	if f.Backbone != "" && !slices.ContainsFunc(f.Backbones, func(b db.ListBackbonesRow) bool { return b.Backbone == f.Backbone }) {
		f.NewBackbone = true
	}
	return true
}

// newBackbone is the backbone pickers' "Add new backbone…" choice; the
// label is then typed into the field named after the picker plus "_new".
const newBackbone = "+new"

// pickedBackbone is the backbone the picker called name chose: its value,
// or the typed label when "Add new backbone…" was chosen.
func pickedBackbone(f url.Values, name string) (string, error) {
	v := strings.TrimSpace(f.Get(name))
	if v != newBackbone {
		return v, nil
	}
	if v = strings.TrimSpace(f.Get(name + "_new")); v == "" {
		return "", formError("Enter a name for the new backbone.")
	}
	return v, nil
}

// backboneLabel normalizes a backbone label against the ones in use.
func (ui *UI) backboneLabel(ctx context.Context, s string) (string, error) {
	rows, err := ui.Store.ListBackbones(ctx)
	if err != nil {
		return "", err
	}
	known := make([]string, len(rows))
	for i, b := range rows {
		known[i] = b.Backbone
	}
	return source.NormalizeBackbone(s, known)
}

// parseProviderFormFull is parseProviderForm plus the backbone, spelled
// like a label already in use. ok=false: a store failure, already
// answered with a 500.
func (ui *UI) parseProviderFormFull(w http.ResponseWriter, r *http.Request) (form providerForm, p parsedProvider, ok bool, err error) {
	if form, p, err = parseProviderForm(w, r, ui.defaultRefreshMinutes()); err != nil {
		return form, p, true, err
	}
	p.backbone, err = ui.backboneLabel(r.Context(), p.backbone)
	switch {
	case errors.Is(err, source.ErrBackboneTooLong):
		return form, p, true, errors.New(humanize(err.Error()))
	case err != nil:
		ui.fail(w, "list backbones", err)
		return form, p, false, nil
	}
	return form, p, true, nil
}

func (ui *UI) providerCreate(w http.ResponseWriter, r *http.Request) {
	form, p, ok, err := ui.parseProviderFormFull(w, r)
	if !ok {
		return
	}
	if err != nil {
		form.Error = err.Error()
		if !ui.withBackbones(w, r, &form) {
			return
		}
		ui.render(w, r, http.StatusUnprocessableEntity, "provider_form", "Add provider", form)
		return
	}
	created, err := ui.Store.CreateProvider(r.Context(), db.CreateProviderParams{
		Name: p.name, Type: p.typ, Url: p.url, Username: p.username, Password: p.password, EpgUrl: p.epgURL,
		MaxConnections: p.maxConns, UserAgent: p.userAgent, RefreshIntervalSecs: p.refreshSecs,
		Priority: p.priority, Enabled: p.enabled, RefreshMaxDisablePct: p.maxDisablePct, Backbone: p.backbone,
		// None of its groups until they are chosen, so the first refresh
		// (and its auto-match) imports streams without matching them.
		GroupFilter: true,
	})
	if err != nil {
		ui.fail(w, "create provider", err)
		return
	}
	if form.VODEnabled && created.Type == "xtream" {
		if err := ui.Store.SetProviderVODEnabled(r.Context(), db.SetProviderVODEnabledParams{ID: created.ID, VodEnabled: true}); err != nil {
			ui.fail(w, "enable vod", err)
			return
		}
	}
	ui.Log.Info("web: provider created", "provider_id", created.ID)
	ui.redirect(w, r, Prefix+"/providers?msg=created")
}

// provider loads {id}, answering 404 itself when it does not exist.
func (ui *UI) provider(w http.ResponseWriter, r *http.Request) (db.Provider, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return db.Provider{}, false
	}
	p, err := ui.Store.GetProvider(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return db.Provider{}, false
	}
	if err != nil {
		ui.fail(w, "get provider", err)
		return db.Provider{}, false
	}
	return p, true
}

func (ui *UI) providerEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	f := formFromProvider(p)
	if !ui.withBackbones(w, r, &f) {
		return
	}
	var err error
	if f.Catalog, err = ui.Store.ProviderVODSize(r.Context(), p.ID); err != nil {
		ui.fail(w, "vod catalog size", err)
		return
	}
	if v := r.URL.Query().Get("vod_deleted"); v != "" {
		movies, series, _ := strings.Cut(v, "-")
		f.VODDeleted, f.DeletedMovies, f.DeletedSeries = true, atoi(movies), atoi(series)
	}
	ui.render(w, r, http.StatusOK, "provider_form", "Edit "+p.Name, f)
}

func (ui *UI) providerUpdate(w http.ResponseWriter, r *http.Request) {
	cur, ok := ui.provider(w, r)
	if !ok {
		return
	}
	form, p, ok, err := ui.parseProviderFormFull(w, r)
	if !ok {
		return
	}
	form.ID, form.HasPassword = cur.ID, cur.Password != ""
	if err != nil {
		form.Error = err.Error()
		if !ui.withBackbones(w, r, &form) {
			return
		}
		ui.render(w, r, http.StatusUnprocessableEntity, "provider_form", "Edit "+cur.Name, form)
		return
	}
	if p.password == "" {
		p.password = cur.Password // blank keeps the stored password
	}
	if p.maxDisablePct == nil {
		p.maxDisablePct = cur.RefreshMaxDisablePct // blank keeps the stored override
	}
	if _, err := ui.Store.UpdateProvider(r.Context(), db.UpdateProviderParams{
		ID: cur.ID, Name: p.name, Type: p.typ, Url: p.url, Username: p.username, Password: p.password,
		EpgUrl: p.epgURL, MaxConnections: p.maxConns, UserAgent: p.userAgent,
		RefreshIntervalSecs: p.refreshSecs, Priority: p.priority, Enabled: p.enabled,
		RefreshMaxDisablePct: p.maxDisablePct, Backbone: p.backbone,
	}); err != nil {
		ui.fail(w, "update provider", err)
		return
	}
	// Movies & series only exist on Xtream panels.
	if vod := form.VODEnabled && p.typ == "xtream"; vod != cur.VodEnabled {
		if err := ui.Store.SetProviderVODEnabled(r.Context(), db.SetProviderVODEnabledParams{ID: cur.ID, VodEnabled: vod}); err != nil {
			ui.fail(w, "set vod", err)
			return
		}
	}
	if form.ResortChannels {
		n, err := ui.Store.SortChannelStreamsByProvider(r.Context(), db.SortChannelStreamsByProviderParams{ProviderIds: []int64{cur.ID}})
		if err != nil {
			ui.fail(w, "sort channel streams", err)
			return
		}
		ui.Log.Info("web: channel streams sorted", "provider_id", cur.ID, "rows", n)
	}
	ui.Log.Info("web: provider updated", "provider_id", cur.ID)
	ui.redirect(w, r, Prefix+"/providers?msg=saved")
}

func (ui *UI) providerDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	if err := ui.Store.DeleteProvider(r.Context(), p.ID); err != nil {
		ui.fail(w, "delete provider", err)
		return
	}
	ui.Log.Info("web: provider deleted", "provider_id", p.ID)
	ui.redirect(w, r, Prefix+"/providers?msg=deleted")
}

// providerToggle flips enabled and returns the refreshed table row.
func (ui *UI) providerToggle(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	if err := ui.Store.SetProviderEnabled(r.Context(), db.SetProviderEnabledParams{ID: p.ID, Enabled: !p.Enabled}); err != nil {
		ui.fail(w, "toggle provider", err)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		ui.redirect(w, r, Prefix+"/providers")
		return
	}
	rows, err := ui.providerRows(r, p.ID)
	if err != nil || len(rows) != 1 {
		ui.fail(w, "provider row", err)
		return
	}
	ui.fragment(w, r, "providers", "provider-row", rows[0])
}

type refreshData struct {
	OK     bool
	Result source.Result
	Error  string
	// Refused is set when the disable threshold refused the refresh: the
	// row offers a "disable anyway" button that re-runs it with force.
	Refused    *source.DisableRefusedError
	ProviderID int64
	Elapsed    time.Duration
}

// providerRefresh runs a refresh now and reports the outcome inline.
func (ui *UI) providerRefresh(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	force := r.FormValue("force") == "1"
	// The refresh outlives a closed tab: it is one transaction either way.
	ctx, cancel := contextFor(r, refreshTimeout)
	defer cancel()
	start := time.Now()
	res, err := ui.Refresh.RefreshProvider(ctx, p, force)
	d := refreshData{OK: err == nil, Result: res, ProviderID: p.ID, Elapsed: time.Since(start).Round(100 * time.Millisecond)}
	if err != nil {
		ui.Log.Warn("web: refresh provider", "provider_id", p.ID, "err", err) // refresher errors are redacted
		d.Error = err.Error()
		if errors.As(err, &d.Refused) {
			d.Error = fmt.Sprintf("would disable %d of %d active streams (%d%%) — possibly a truncated provider answer; nothing was changed.",
				d.Refused.WouldDisable, d.Refused.Active, d.Refused.Pct())
		}
	}
	if r.Header.Get("HX-Request") != "true" {
		ui.redirect(w, r, Prefix+"/providers")
		return
	}
	// The whole row: counts and "last refresh" changed too.
	rows, err := ui.providerRows(r, p.ID)
	if err != nil || len(rows) != 1 {
		ui.fail(w, "provider row", err)
		return
	}
	rows[0].Refresh = &d
	ui.fragment(w, r, "providers", "provider-row", rows[0])
}

type streamsData struct {
	Provider   db.Provider
	Streams    []db.ProviderStream
	Query      string
	Offset     int
	PrevOffset int
	NextOffset int // 0 when there is no next page
	First      int // 1-based index of the first row shown
}

func (ui *UI) streamsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	var streams []db.ProviderStream
	var err error
	// One extra row tells whether a next page exists.
	if q != "" {
		streams, err = ui.Store.SearchProviderStreams(r.Context(), db.SearchProviderStreamsParams{
			ProviderID: p.ID, Name: "%" + likeEscape(q) + "%", Limit: streamsPerPage + 1, Offset: int32(offset)})
	} else {
		streams, err = ui.Store.ListProviderStreams(r.Context(), db.ListProviderStreamsParams{
			ProviderID: p.ID, Limit: streamsPerPage + 1, Offset: int32(offset)})
	}
	if err != nil {
		ui.fail(w, "list streams", err)
		return
	}
	d := streamsData{Provider: p, Query: q, Offset: offset, PrevOffset: max(offset-streamsPerPage, 0), First: offset + 1}
	if len(streams) > streamsPerPage {
		streams, d.NextOffset = streams[:streamsPerPage], offset+streamsPerPage
	}
	d.Streams = streams
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "streams" {
		ui.fragment(w, r, "streams", "streams-table", d)
		return
	}
	ui.render(w, r, http.StatusOK, "streams", p.Name+" streams", d)
}

// contextFor detaches a long admin action from the request: it finishes
// (within d) even if the tab is closed, since it is one transaction anyway.
func contextFor(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), d)
}

// resultOf adapts a program count to the refresh result display.
func resultOf(programs int) source.Result { return source.Result{Seen: programs} }

// likeEscape makes user input literal inside an ILIKE pattern.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// streamsQuery builds a streams page URL (for pagination links).
func streamsQuery(id int64, q string, offset int) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	u := Prefix + "/providers/" + strconv.FormatInt(id, 10) + "/streams"
	if len(v) > 0 {
		u += "?" + v.Encode()
	}
	return u
}

// fail logs an internal error and answers 500 without details.
func (ui *UI) fail(w http.ResponseWriter, op string, err error) {
	ui.Log.Error("web: "+op, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
