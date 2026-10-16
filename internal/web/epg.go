package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

const guideRowsPerPage = 100

func (ui *UI) mountEPG(r chi.Router) {
	r.Get("/epg", ui.epgPage)
	r.Post("/epg/providers/{id}/refresh", ui.epgRefresh)
}

// epgSource is one guide-capable provider and its source, if fetched yet.
type epgSource struct {
	Provider db.Provider
	Stats    *db.ListEpgSourceStatsRow
	Refresh  *refreshData
}

type epgData struct {
	Sources     []epgSource
	Guide       []db.ChannelGuideStatusRow
	Query       string
	MissingOnly bool
	Offset      int
	NextOffset  int
	PrevOffset  int
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
	return out, nil
}

func (ui *UI) epgPage(w http.ResponseWriter, r *http.Request) {
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
	d.Guide = guide
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "guide" {
		ui.fragment(w, r, "epg", "guide-table", d)
		return
	}
	if d.Sources, err = ui.epgSources(r); err != nil {
		ui.fail(w, "epg sources", err)
		return
	}
	ui.render(w, r, http.StatusOK, "epg", "Guide (EPG)", d)
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
