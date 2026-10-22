package web

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/nameguide"
)

const guideRowsPerPage = 100

// fromStreamName marks coverage rows whose guide is made from the stream
// name.
const fromStreamName = "stream name"

func (ui *UI) mountEPG(r chi.Router) {
	r.Get("/epg", ui.epgPage)
	r.Post("/epg/providers/{id}/refresh", ui.epgRefresh)
	r.Post("/epg/providers/{id}/priority", ui.epgPriority)
	ui.mountEPGExtra(r)
}

// epgSource is one guide-capable provider and its source, if fetched yet.
type epgSource struct {
	Provider db.Provider
	Stats    *db.ListEpgSourceStatsRow
	Refresh  *refreshData
}

type epgData struct {
	Sources     []epgSource
	Extras      []extraRow
	ExtraForm   extraForm
	Flash       string
	Guide       []db.ChannelGuideStatusRow
	Query       string
	MissingOnly bool
	Offset      int
	NextOffset  int
	PrevOffset  int
}

// GuidePriority is the priority the provider's guide ranks at: its own,
// else the provider's.
func (s epgSource) GuidePriority() int32 {
	if s.Stats != nil && s.Stats.Priority != nil {
		return *s.Stats.Priority
	}
	return s.Provider.Priority
}

// OwnPriority is the guide's own priority as the form shows it ("" when it
// follows the provider's).
func (s epgSource) OwnPriority() string {
	if s.Stats != nil && s.Stats.Priority != nil {
		return strconv.Itoa(int(*s.Stats.Priority))
	}
	return ""
}

// guideCapable: Xtream panels serve XMLTV themselves; others need an EPG URL.
func guideCapable(p db.Provider) bool { return p.Type == "xtream" || p.EpgUrl != "" }

func (ui *UI) epgSources(r *http.Request) ([]epgSource, error) {
	ps, err := ui.Store.ListProviders(r.Context())
	if err != nil {
		return nil, err
	}
	stats, err := ui.Store.ListEpgSourceStats(r.Context())
	if err != nil {
		return nil, err
	}
	byProvider := map[int64]db.ListEpgSourceStatsRow{}
	for _, s := range stats {
		if s.ProviderID != nil {
			byProvider[*s.ProviderID] = s
		}
	}
	var out []epgSource
	for _, p := range ps {
		if !guideCapable(p) {
			continue
		}
		src := epgSource{Provider: p}
		if s, ok := byProvider[p.ID]; ok {
			src.Stats = &s
		}
		out = append(out, src)
	}
	slices.SortStableFunc(out, func(a, b epgSource) int { return cmp.Compare(a.GuidePriority(), b.GuidePriority()) })
	return out, nil
}

var epgFlashes = map[string]string{
	"extra-added":   "Guide source added. It is downloading now; reload in a minute to see its programs.",
	"extra-saved":   "Guide source saved.",
	"extra-deleted": "Guide source deleted.",
	"priority":      "Guide priority saved. Channels take their guide from the new order at once.",
}

func (ui *UI) epgPage(w http.ResponseWriter, r *http.Request) {
	ui.epgPageWith(w, r, http.StatusOK, extraForm{Enabled: true}, "")
}

// epgPageWith renders the Guide page, with the add-source form's state.
func (ui *UI) epgPageWith(w http.ResponseWriter, r *http.Request, status int, form extraForm, formErr string) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	missing := r.URL.Query().Get("missing") == "1"
	offset := max(atoi(r.URL.Query().Get("offset")), 0)
	params := db.ChannelGuideStatusParams{MissingOnly: missing, Limit: guideRowsPerPage + 1, Offset: int32(offset)}
	if q != "" {
		pat := "%" + likeEscape(q) + "%"
		params.Search = &pat
	}
	guide, err := ui.Store.ChannelGuideStatus(r.Context(), params)
	if err != nil {
		ui.fail(w, "channel guide status", err)
		return
	}
	d := epgData{Query: q, MissingOnly: missing, Offset: offset, PrevOffset: max(offset-guideRowsPerPage, 0)}
	if len(guide) > guideRowsPerPage {
		guide, d.NextOffset = guide[:guideRowsPerPage], offset+guideRowsPerPage
	}
	// What the stream-name guide gives channels without a guide, and
	// those set to take it first (as the customer outputs pick).
	if names, err := nameguide.Load(r.Context(), ui.Store); err == nil {
		now := time.Now()
		for i := range guide {
			g := &guide[i]
			s := names
			switch {
			case g.GuideMode == "xmltv":
				continue
			case g.GuideMode == "name":
				s.Enabled = true
			case g.SourceName != "":
				continue
			}
			if t := s.NowTitle(g.Name, g.StreamName, now); t != "" {
				g.NowTitle, g.SourceName = t, fromStreamName
			}
		}
	}
	d.Guide = guide
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "guide" {
		ui.fragment(w, r, "epg", "guide-table", d)
		return
	}
	if d.Sources, err = ui.epgSources(r); err != nil {
		ui.fail(w, "epg sources", err)
		return
	}
	if d.Extras, err = ui.extraRows(r); err != nil {
		ui.fail(w, "guide sources", err)
		return
	}
	form.Error, form.ProviderPriorities = formErr, ui.providerPriorities(r)
	d.ExtraForm = form
	d.Flash = epgFlashes[r.URL.Query().Get("msg")]
	ui.render(w, r, status, "epg", "Guide (EPG)", d)
}

// epgRefresh downloads a provider's guide now and returns its updated row.
func (ui *UI) epgRefresh(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	if !guideCapable(p) {
		http.Error(w, "this provider has no guide", http.StatusBadRequest)
		return
	}
	ctx, cancel := contextFor(r, refreshTimeout)
	defer cancel()
	start := time.Now()
	n, err := ui.Refresh.RefreshEPG(ctx, p)
	d := refreshData{OK: err == nil, Result: resultOf(n), Elapsed: time.Since(start).Round(100 * time.Millisecond)}
	if err != nil {
		ui.Log.Warn("web: refresh epg", "provider_id", p.ID, "err", err) // refresher errors are redacted
		d.Error = err.Error()
	}
	if r.Header.Get("HX-Request") != "true" {
		ui.redirect(w, r, Prefix+"/epg")
		return
	}
	srcs, err := ui.epgSources(r)
	if err != nil {
		ui.fail(w, "epg sources", err)
		return
	}
	for _, s := range srcs {
		if s.Provider.ID == p.ID {
			s.Refresh = &d
			ui.fragment(w, r, "epg", "source-row", s)
			return
		}
	}
	http.NotFound(w, r)
}

// epgPriority sets a provider guide's own priority (blank: the provider's
// stream priority), independent of the provider's failover order.
func (ui *UI) epgPriority(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var prio *int32
	if v := strings.TrimSpace(r.PostForm.Get("priority")); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			http.Error(w, "priority must be a whole number", http.StatusBadRequest)
			return
		}
		n32 := int32(n)
		prio = &n32
	}
	n, err := ui.Store.SetProviderGuidePriority(r.Context(), db.SetProviderGuidePriorityParams{Priority: prio, ProviderID: &p.ID})
	if err != nil {
		ui.fail(w, "set guide priority", err)
		return
	}
	if n == 0 {
		http.Error(w, "this provider's guide has not been downloaded yet", http.StatusConflict)
		return
	}
	ui.Log.Info("web: guide priority saved", "provider_id", p.ID)
	ui.redirect(w, r, Prefix+"/epg?msg=priority")
}
