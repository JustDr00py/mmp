package web

import (
	"cmp"
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
	"github.com/JustDr00py/mmp/internal/vodmeta"
)

const vodPerPage = 100

func (ui *UI) mountVOD(r chi.Router) {
	r.Get("/vod", ui.vodPage)
	r.Get("/vod/series/{id}", ui.vodSeriesPage)
	r.Post("/vod/{kind}/{id}/match", ui.vodMatch)
	r.Post("/providers/{id}/vod", ui.providerVODRefresh)
	r.Post("/providers/{id}/vod/delete", ui.providerVODDelete)
	r.Post("/bouquets/{id}/vod-categories", ui.bouquetVODAdd)
	r.Post("/bouquets/{id}/vod-categories/{cid}/remove", ui.bouquetVODRemove)
}

// vodRow is one movie or series line of the catalog page.
type vodRow struct {
	ID       int64
	Name     string
	Image    string
	Rating   *float32
	Extra    string // container (movies) or release date (series)
	Disabled bool
	Series   bool
	// The library's view: parsed title, TMDB id and how it was found
	// (panel, tmdb search, name = lent by a copy, manual), and how many
	// active copies on any provider share the title.
	Title, Quality, Lang, TmdbID, ImdbID, MatchSource string
	Year                                              int32
	Locked                                            bool
	Copies                                            int64
	SourceCopies                                      int64 // copies on the picked source's providers
}

// TMDBURL is the title's page on TMDB.
func (r vodRow) TMDBURL() string {
	kind := "movie"
	if r.Series {
		kind = "tv"
	}
	return "https://www.themoviedb.org/" + kind + "/" + r.TmdbID
}

type vodData struct {
	Kind       string // movie | series
	Categories []db.ListVODCategoriesAdminRow
	CategoryID int64
	Languages  []langOption
	Language   string // "" = any, noLanguage = categories without a label
	Sources    []vodSourceGroup
	Source     string // "" = every provider, "b:<backbone>" or "p:<provider id>"
	Backbone   string // the picked backbone, when Source is one
	Query      string
	Unmatched  bool // only titles without a TMDB id
	Rows       []vodRow
	Back       string // this page's URL, for the match forms
	Flash      string
	Error      string
	Offset     int
	PrevOffset int
	NextOffset int
}

// vodSourceGroup is a backbone's providers in the source picker (Backbone
// "" groups the providers without one).
type vodSourceGroup struct {
	Backbone  string
	Providers []vodSourceProvider
}

type vodSourceProvider struct {
	ID   int64
	Name string
}

// vodSources groups the providers with offered categories by backbone,
// backbones by name and the providers without one last, each group in
// the providers' priority order.
func vodSources(ps []db.Provider, cats []db.ListVODCategoriesAdminRow) []vodSourceGroup {
	has := map[int64]bool{}
	for _, c := range cats {
		has[c.ProviderID] = true
	}
	slices.SortStableFunc(ps, func(a, b db.Provider) int {
		return cmp.Or(cmp.Compare(a.Priority, b.Priority), cmp.Compare(a.Name, b.Name))
	})
	var out []vodSourceGroup
	for _, p := range ps {
		if !has[p.ID] {
			continue
		}
		i := slices.IndexFunc(out, func(g vodSourceGroup) bool { return g.Backbone == p.Backbone })
		if i < 0 {
			out = append(out, vodSourceGroup{Backbone: p.Backbone})
			i = len(out) - 1
		}
		out[i].Providers = append(out[i].Providers, vodSourceProvider{ID: p.ID, Name: p.Name})
	}
	slices.SortStableFunc(out, func(a, b vodSourceGroup) int {
		if (a.Backbone == "") != (b.Backbone == "") {
			return cmp.Compare(boolInt(a.Backbone == ""), boolInt(b.Backbone == ""))
		}
		return cmp.Compare(strings.ToLower(a.Backbone), strings.ToLower(b.Backbone))
	})
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// vodSourceProviders resolves a source ("b:<backbone>" or "p:<id>") to its
// providers among groups, and the backbone when it names one. ok is false
// for a source that is not offered.
func vodSourceProviders(groups []vodSourceGroup, source string) (ids []int64, backbone string, ok bool) {
	kind, val, _ := strings.Cut(source, ":")
	for _, g := range groups {
		for _, p := range g.Providers {
			switch {
			case kind == "b" && val != "" && g.Backbone == val:
				ids, backbone = append(ids, p.ID), val
			case kind == "p" && strconv.FormatInt(p.ID, 10) == val:
				return []int64{p.ID}, "", true
			}
		}
	}
	return ids, backbone, len(ids) > 0
}

// vodPage browses the imported catalogs: ?kind=movie|series, &category=,
// &source=, &q=, &offset=.
func (ui *UI) vodPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := vodData{Kind: q.Get("kind"), Query: strings.TrimSpace(q.Get("q")), Language: q.Get("lang"), Offset: max(atoi(q.Get("offset")), 0),
		Unmatched: q.Get("unmatched") != "", Flash: vodFlash[q.Get("msg")], Error: q.Get("err")}
	back := r.URL.Query()
	back.Del("msg")
	back.Del("err")
	d.Back = Prefix + "/vod?" + back.Encode()
	if d.Kind != "series" {
		d.Kind = "movie"
	}
	d.CategoryID, _ = strconv.ParseInt(q.Get("category"), 10, 64)
	d.PrevOffset = max(d.Offset-vodPerPage, 0)
	ctx := r.Context()
	var err error
	if d.Categories, err = ui.Store.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: d.Kind}); err != nil {
		ui.fail(w, "vod categories", err)
		return
	}
	d.Categories = slices.DeleteFunc(d.Categories, func(c db.ListVODCategoriesAdminRow) bool { return !c.Offered })
	providers, err := ui.Store.ListProviders(ctx)
	if err != nil {
		ui.fail(w, "providers", err)
		return
	}
	d.Sources = vodSources(providers, d.Categories)
	// The source (a backbone or one provider) narrows the categories and
	// the titles; a source no longer offered is dropped.
	var pids []int64 // nil: every provider
	if src := q.Get("source"); src != "" {
		var ok bool
		if pids, d.Backbone, ok = vodSourceProviders(d.Sources, src); ok {
			d.Source = src
			d.Categories = slices.DeleteFunc(d.Categories, func(c db.ListVODCategoriesAdminRow) bool { return !slices.Contains(pids, c.ProviderID) })
		}
	}
	// The language narrows the category list and the titles; a category
	// picked from another language is dropped.
	var names []string
	for _, c := range d.Categories {
		names = append(names, c.Name)
	}
	d.Languages = languagesOf(names)
	if !slices.ContainsFunc(d.Languages, func(l langOption) bool { return l.Value == d.Language }) {
		d.Language = "" // not offered by the picked source
	}
	var cids []int64 // nil: every category
	if d.Language != "" {
		cids = []int64{}
		d.Categories = slices.DeleteFunc(d.Categories, func(c db.ListVODCategoriesAdminRow) bool { return !inLanguage(c.Name, d.Language) })
		for _, c := range d.Categories {
			cids = append(cids, c.ID)
		}
	}
	if d.CategoryID > 0 {
		if !slices.ContainsFunc(d.Categories, func(c db.ListVODCategoriesAdminRow) bool { return c.ID == d.CategoryID }) {
			d.CategoryID = 0
		} else {
			cids = []int64{d.CategoryID}
		}
	}
	var search *string
	if d.Query != "" {
		s := "%" + likeEscape(d.Query) + "%"
		search = &s
	}
	if d.Kind == "movie" {
		rows, err := ui.Store.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{ProviderIds: pids, CategoryIds: cids, Search: search, UnmatchedOnly: d.Unmatched, Limit: vodPerPage + 1, Offset: int32(d.Offset)})
		if err != nil {
			ui.fail(w, "list movies", err)
			return
		}
		for _, m := range rows {
			d.Rows = append(d.Rows, vodRow{ID: m.ID, Name: m.Name, Image: m.Icon, Rating: m.Rating, Extra: m.ContainerExtension, Disabled: m.Disabled,
				Title: m.Title, Year: m.Year, Quality: m.Quality, Lang: m.Lang, TmdbID: m.TmdbID, ImdbID: m.ImdbID,
				MatchSource: m.MatchSource, Locked: m.MatchLocked, Copies: m.Copies, SourceCopies: m.SourceCopies})
		}
	} else {
		rows, err := ui.Store.ListSeriesAdmin(ctx, db.ListSeriesAdminParams{ProviderIds: pids, CategoryIds: cids, Search: search, UnmatchedOnly: d.Unmatched, Limit: vodPerPage + 1, Offset: int32(d.Offset)})
		if err != nil {
			ui.fail(w, "list series", err)
			return
		}
		for _, s := range rows {
			d.Rows = append(d.Rows, vodRow{ID: s.ID, Name: s.Name, Image: s.Cover, Rating: s.Rating, Extra: s.ReleaseDate, Disabled: s.Disabled, Series: true,
				Title: s.Title, Year: s.Year, Quality: s.Quality, Lang: s.Lang, TmdbID: s.TmdbID, ImdbID: s.ImdbID,
				MatchSource: s.MatchSource, Locked: s.MatchLocked, Copies: s.Copies, SourceCopies: s.SourceCopies})
		}
	}
	if len(d.Rows) > vodPerPage {
		d.Rows, d.NextOffset = d.Rows[:vodPerPage], d.Offset+vodPerPage
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "vod-list" {
		ui.fragment(w, r, "vod", "vod-list", d)
		return
	}
	ui.render(w, r, http.StatusOK, "vod", "Movies & series", d)
}

type seriesPageData struct {
	Series  db.Series
	Seasons []seasonView
	Error   string
	Fetched string
}

type seasonView struct {
	Number   int32
	Episodes []db.SeriesEpisode
}

// vodSeriesPage shows one series with its episodes, loading them from
// the panel if they were never fetched (or are stale).
func (ui *UI) vodSeriesPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := contextFor(r, time.Minute)
	defer cancel()
	s, eps, err := ui.Refresh.SeriesEpisodesByID(ctx, id, ui.episodeTTL())
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	d := seriesPageData{Series: s}
	if err != nil {
		if s.ID == 0 {
			ui.fail(w, "get series", err)
			return
		}
		ui.Log.Warn("web: series episodes", "series_id", id, "err", err) // redacted by the refresher
		d.Error = "Could not load episodes from the provider: " + err.Error()
	}
	for _, e := range eps {
		if n := len(d.Seasons); n == 0 || d.Seasons[n-1].Number != e.Season {
			d.Seasons = append(d.Seasons, seasonView{Number: e.Season})
		}
		d.Seasons[len(d.Seasons)-1].Episodes = append(d.Seasons[len(d.Seasons)-1].Episodes, e)
	}
	if s.EpisodesFetchedAt != nil {
		d.Fetched = ago(s.EpisodesFetchedAt)
	}
	ui.render(w, r, http.StatusOK, "vod_series", s.Name, d)
}

func (ui *UI) episodeTTL() time.Duration {
	if ui.EpisodeTTL > 0 {
		return ui.EpisodeTTL
	}
	return 6 * time.Hour
}

type vodRefreshData struct {
	OK      bool
	Result  source.VODResult
	Error   string
	Elapsed time.Duration
}

// providerVODRefresh imports a provider's catalogs now and re-renders its
// row with the outcome (force=1 applies a refused import).
func (ui *UI) providerVODRefresh(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextFor(r, refreshTimeout)
	defer cancel()
	start := time.Now()
	res, err := ui.Refresh.RefreshVOD(ctx, p, r.FormValue("force") == "1")
	d := vodRefreshData{OK: err == nil, Result: res, Elapsed: time.Since(start).Round(100 * time.Millisecond)}
	if err != nil {
		ui.Log.Warn("web: vod refresh", "provider_id", p.ID, "err", err)
		d.Error = err.Error()
		var refused *source.DisableRefusedError
		if errors.As(err, &refused) {
			d.Error = fmt.Sprintf("would disable %d of %d active titles (%d%%); nothing was changed. Use the API with ?force=1 if the change is real.",
				refused.WouldDisable, refused.Active, refused.Pct())
		}
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
	rows[0].VODRefresh = &d
	ui.fragment(w, r, "providers", "provider-row", rows[0])
}

// providerVODDelete removes a provider's imported movies, series and
// categories, with their episodes and bouquet grants; its live streams
// stay. Only with importing off, or the next import would bring it back.
func (ui *UI) providerVODDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	edit := Prefix + "/providers/" + strconv.FormatInt(p.ID, 10) + "/edit"
	if p.VodEnabled {
		http.Error(w, "turn off “Import movies & series” first", http.StatusConflict)
		return
	}
	res, err := ui.Store.DeleteProviderVOD(r.Context(), p.ID)
	if err != nil {
		ui.fail(w, "delete vod catalog", err)
		return
	}
	ui.Log.Info("web: provider catalog deleted", "provider_id", p.ID,
		"movies", res.Movies, "series", res.Series, "categories", res.Categories)
	ui.redirect(w, r, edit+"?vod_deleted="+strconv.FormatInt(res.Movies, 10)+"-"+strconv.FormatInt(res.Series, 10)+"#catalog")
}

// bouquetVODAdd grants the ticked movie/series categories, or with lang
// every category in that language (kind: "movie", "series" or both).
func (ui *UI) bouquetVODAdd(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var ids []int64
	if lang := r.PostForm.Get("lang"); lang != "" {
		d, err := ui.bouquetVODData(r, b.ID)
		if err != nil {
			ui.fail(w, "vod categories", err)
			return
		}
		kind := r.PostForm.Get("kind")
		for _, c := range d.Available {
			if (kind == "" || c.Kind == kind) && inLanguage(c.Name, lang) {
				ids = append(ids, c.ID)
			}
		}
	}
	for _, v := range r.PostForm["category"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		if _, err := ui.Store.AddVODCategoriesToBouquet(r.Context(), db.AddVODCategoriesToBouquetParams{BouquetID: b.ID, CategoryIds: ids}); err != nil {
			ui.fail(w, "add vod categories", err)
			return
		}
	}
	ui.redirect(w, r, Prefix+"/bouquets/"+strconv.FormatInt(b.ID, 10)+"#vod")
}

func (ui *UI) bouquetVODRemove(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil || cid <= 0 {
		http.NotFound(w, r)
		return
	}
	if _, err := ui.Store.RemoveVODCategoryFromBouquet(r.Context(), db.RemoveVODCategoryFromBouquetParams{BouquetID: b.ID, VodCategoryID: cid}); err != nil {
		ui.fail(w, "remove vod category", err)
		return
	}
	ui.redirect(w, r, Prefix+"/bouquets/"+strconv.FormatInt(b.ID, 10)+"#vod")
}

// bouquetVOD is the bouquet page's movie/series section.
type bouquetVOD struct {
	Granted   []db.ListBouquetVODCategoriesRow
	Available []vodCategory // not yet granted
	Languages []langOption  // of Available
}

// vodCategory is a category with the language its name is labelled with.
type vodCategory struct {
	db.ListVODCategoriesAdminRow
	Language string
}

func (ui *UI) bouquetVODData(r *http.Request, bouquetID int64) (bouquetVOD, error) {
	var d bouquetVOD
	var err error
	if d.Granted, err = ui.Store.ListBouquetVODCategories(r.Context(), bouquetID); err != nil {
		return d, err
	}
	have := map[int64]bool{}
	for _, c := range d.Granted {
		have[c.ID] = true
	}
	for _, kind := range []string{"movie", "series"} {
		cats, err := ui.Store.ListVODCategoriesAdmin(r.Context(), db.ListVODCategoriesAdminParams{Kind: kind})
		if err != nil {
			return d, err
		}
		for _, c := range cats {
			if !have[c.ID] && !c.Disabled && c.Offered {
				d.Available = append(d.Available, vodCategory{ListVODCategoriesAdminRow: c, Language: categoryLanguage(c.Name)})
			}
		}
	}
	names := make([]string, len(d.Available))
	for i, c := range d.Available {
		names[i] = c.Name
	}
	d.Languages = languagesOf(names)
	return d, nil
}

var vodFlash = map[string]string{
	"matched": "Match saved and locked: imports and TMDB searches leave it as it is.",
	"split":   "Title split from its copies and locked.",
	"reset":   "Match reset: the next VOD refresh brings the panel's id back, and TMDB may be searched again.",
}

// vodMatch fixes one title's TMDB match: action=set (tmdb_id), split (no
// id: its own title) or reset (back to the panel's id and searches).
func (ui *UI) vodMatch(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 || kind != "movie" && kind != "series" {
		http.NotFound(w, r)
		return
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, Prefix+"/vod?") {
		back = Prefix + "/vod?kind=" + kind
	}
	done := func(msg, errText string) {
		v := "&msg=" + msg
		if errText != "" {
			v = "&err=" + url.QueryEscape(errText)
		}
		ui.redirect(w, r, back+v)
	}
	ctx := r.Context()
	tmdbID := ""
	switch r.FormValue("action") {
	case "reset":
		if kind == "movie" {
			err = ui.Store.ResetVODItemMatch(ctx, id)
		} else {
			err = ui.Store.ResetSeriesMatch(ctx, id)
		}
		if err != nil {
			ui.fail(w, "reset match", err)
			return
		}
		ui.Log.Info("web: vod match reset", "kind", kind, "id", id)
		done("reset", "")
		return
	case "set":
		tmdbID = strings.TrimSpace(r.FormValue("tmdb_id"))
		if _, err := strconv.ParseUint(tmdbID, 10, 64); err != nil || strings.Trim(tmdbID, "0") == "" {
			done("", "A TMDB id is a number, as in themoviedb.org/movie/603.")
			return
		}
		if ui.VODLibrary != nil {
			fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := ui.VODLibrary.Fetch(fctx, kind, tmdbID)
			cancel()
			if errors.Is(err, vodmeta.ErrTMDBNotFound) {
				done("", "TMDB has no "+kind+" with id "+tmdbID+".")
				return
			} else if err != nil {
				ui.Log.Warn("web: could not load TMDB details; match saved without them", "kind", kind, "tmdb_id", tmdbID, "err", err)
			}
		}
	case "split":
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if kind == "movie" {
		err = ui.Store.SetVODItemMatch(ctx, db.SetVODItemMatchParams{ID: id, TmdbID: tmdbID})
	} else {
		err = ui.Store.SetSeriesMatch(ctx, db.SetSeriesMatchParams{ID: id, TmdbID: tmdbID})
	}
	if err != nil {
		ui.fail(w, "save match", err)
		return
	}
	ui.Log.Info("web: vod match fixed", "kind", kind, "id", id, "tmdb_id", tmdbID)
	if tmdbID == "" {
		done("split", "")
		return
	}
	done("matched", "")
}
