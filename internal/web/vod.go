package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source"
)

const vodPerPage = 100

func (ui *UI) mountVOD(r chi.Router) {
	r.Get("/vod", ui.vodPage)
	r.Get("/vod/series/{id}", ui.vodSeriesPage)
	r.Post("/providers/{id}/vod", ui.providerVODRefresh)
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
}

type vodData struct {
	Kind       string // movie | series
	Categories []db.ListVODCategoriesAdminRow
	CategoryID int64
	Query      string
	Rows       []vodRow
	Offset     int
	PrevOffset int
	NextOffset int
}

// vodPage browses the imported catalogs: ?kind=movie|series, &category=,
// &q=, &offset=.
func (ui *UI) vodPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := vodData{Kind: q.Get("kind"), Query: strings.TrimSpace(q.Get("q")), Offset: max(atoi(q.Get("offset")), 0)}
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
	var cid *int64
	if d.CategoryID > 0 {
		cid = &d.CategoryID
	}
	var search *string
	if d.Query != "" {
		s := "%" + likeEscape(d.Query) + "%"
		search = &s
	}
	if d.Kind == "movie" {
		rows, err := ui.Store.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{CategoryID: cid, Search: search, Limit: vodPerPage + 1, Offset: int32(d.Offset)})
		if err != nil {
			ui.fail(w, "list movies", err)
			return
		}
		for _, m := range rows {
			d.Rows = append(d.Rows, vodRow{ID: m.ID, Name: m.Name, Image: m.Icon, Rating: m.Rating, Extra: m.ContainerExtension, Disabled: m.Disabled})
		}
	} else {
		rows, err := ui.Store.ListSeriesAdmin(ctx, db.ListSeriesAdminParams{CategoryID: cid, Search: search, Limit: vodPerPage + 1, Offset: int32(d.Offset)})
		if err != nil {
			ui.fail(w, "list series", err)
			return
		}
		for _, s := range rows {
			d.Rows = append(d.Rows, vodRow{ID: s.ID, Name: s.Name, Image: s.Cover, Rating: s.Rating, Extra: s.ReleaseDate, Disabled: s.Disabled, Series: true})
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

// bouquetVODAdd grants the ticked movie/series categories.
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
	Available []db.ListVODCategoriesAdminRow // not yet granted
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
			if !have[c.ID] && !c.Disabled {
				d.Available = append(d.Available, c)
			}
		}
	}
	return d, nil
}
