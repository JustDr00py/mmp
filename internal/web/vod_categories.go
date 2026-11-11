package web

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// vodCategoriesData is the provider's movie & series categories page:
// which of its categories are imported and offered (like its live groups).
type vodCategoriesData struct {
	Provider   db.Provider
	Categories []vodCategory // movies first, then series
	Languages  []langOption
	Selected   int
	// Pool lists the other providers on this one's backbone, with how
	// many of this provider's movies they hold under the same id and name.
	Pool  []db.VODPoolOverlapRow
	Flash string
	Error string
}

func (ui *UI) providerVODCategoriesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	d := vodCategoriesData{Provider: p}
	var names []string
	for _, kind := range []string{"movie", "series"} {
		cats, err := ui.Store.ListVODCategoriesAdmin(r.Context(), db.ListVODCategoriesAdminParams{Kind: kind, ProviderID: &p.ID})
		if err != nil {
			ui.fail(w, "vod categories", err)
			return
		}
		slices.SortStableFunc(cats, func(a, b db.ListVODCategoriesAdminRow) int { return cmp.Compare(a.Name, b.Name) })
		for _, c := range cats {
			d.Categories = append(d.Categories, vodCategory{ListVODCategoriesAdminRow: c, Language: categoryLanguage(c.Name)})
			names = append(names, c.Name)
			if c.Selected {
				d.Selected++
			}
		}
	}
	d.Languages = languagesOf(names)
	if p.Backbone != "" {
		var err error
		if d.Pool, err = ui.Store.VODPoolOverlap(r.Context(), db.VODPoolOverlapParams{ProviderID: p.ID, Backbone: p.Backbone}); err != nil {
			ui.fail(w, "vod pool", err)
			return
		}
	}
	q := r.URL.Query()
	switch q.Get("msg") {
	case "saved":
		d.Flash = "Categories saved."
		if n := atoi(q.Get("disabled")); n > 0 {
			d.Flash += fmt.Sprintf(" %d titles outside the chosen categories are hidden now.", n)
		}
		if q.Get("mode") == "all" {
			d.Flash += " Titles hidden by an earlier choice come back with the next import."
		}
	case "imported":
		d.Flash = fmt.Sprintf("Categories saved and catalog imported: %d movies and %d series kept, %d movies and %d series in other categories skipped.",
			atoi(q.Get("movies")), atoi(q.Get("series")), atoi(q.Get("movies_skipped")), atoi(q.Get("series_skipped")))
	case "pooled":
		d.Flash = "Pooling saved."
	case "loaded":
		d.Flash = fmt.Sprintf("Loaded %d categories from the provider. Tick the ones you want, then save and import.", atoi(q.Get("n")))
	}
	switch q.Get("err") {
	case "load":
		d.Error = "Could not load the categories from the provider; check the provider and try again."
	case "import":
		d.Error = "Categories saved, but the catalog import failed; see the providers page or try again."
	case "refused":
		d.Error = "Categories saved, but the import was refused: the provider's answer would disable too much of the catalog. Nothing was imported."
	}
	ui.render(w, r, http.StatusOK, "provider_vod_categories", p.Name+": movie & series categories", d)
}

// providerVODCategoriesSave handles the page's buttons: action=load
// fetches the category lists only; otherwise the selection is saved
// (mode=all offers every category, mode=selected only the ticked ones,
// hiding stored titles outside them at once), and action=import then
// imports the catalog.
func (ui *UI) providerVODCategoriesSave(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	page := Prefix + "/providers/" + strconv.FormatInt(p.ID, 10) + "/vod-categories"
	if r.PostForm.Get("action") == "pool" {
		pooled := r.PostForm.Get("pooled") == "on" && p.Backbone != ""
		if err := ui.Store.SetProviderVODPooled(r.Context(), db.SetProviderVODPooledParams{ID: p.ID, VodPooled: pooled}); err != nil {
			ui.fail(w, "save vod pool", err)
			return
		}
		ui.Log.Info("web: provider vod pooling saved", "provider_id", p.ID, "backbone", p.Backbone, "pooled", pooled)
		ui.redirect(w, r, page+"?msg=pooled#pool")
		return
	}
	if r.PostForm.Get("action") == "load" {
		ctx, cancel := contextFor(r, refreshTimeout)
		defer cancel()
		n, err := ui.Refresh.RefreshVODCategories(ctx, p)
		if err != nil {
			ui.Log.Warn("web: vod categories load", "provider_id", p.ID, "err", err)
			ui.redirect(w, r, page+"?err=load")
			return
		}
		ui.redirect(w, r, page+"?msg=loaded&n="+strconv.Itoa(n))
		return
	}

	filter := r.PostForm.Get("mode") == "selected"
	ids := []int64{} // never nil: an empty selection clears every tick
	for _, v := range r.PostForm["category"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	var disabled int64
	err := ui.inTx(r.Context(), func(q Store) error {
		if filter || len(ids) > 0 {
			if err := q.SetVODCategorySelection(r.Context(), db.SetVODCategorySelectionParams{ProviderID: p.ID, Ids: ids}); err != nil {
				return err
			}
		}
		if err := q.SetProviderVODCategoryFilter(r.Context(), db.SetProviderVODCategoryFilterParams{ID: p.ID, VodCategoryFilter: filter}); err != nil {
			return err
		}
		if !filter {
			return nil
		}
		n, err := q.DisableUnselectedVODItems(r.Context(), p.ID)
		if err != nil {
			return err
		}
		m, err := q.DisableUnselectedSeries(r.Context(), p.ID)
		disabled = n + m
		return err
	})
	if err != nil {
		ui.fail(w, "save vod categories", err)
		return
	}
	ui.Log.Info("web: provider vod categories saved", "provider_id", p.ID, "filter", filter, "categories", len(ids), "disabled", disabled)
	back := url.Values{"msg": {"saved"}, "disabled": {strconv.FormatInt(disabled, 10)}}
	if !filter {
		back.Set("mode", "all")
	}
	if r.PostForm.Get("action") == "import" {
		p.VodCategoryFilter = filter
		ctx, cancel := contextFor(r, refreshTimeout)
		defer cancel()
		res, err := ui.Refresh.RefreshVOD(ctx, p, false)
		var refused *source.DisableRefusedError
		switch {
		case errors.As(err, &refused):
			back = url.Values{"err": {"refused"}}
		case err != nil:
			ui.Log.Warn("web: vod import after categories", "provider_id", p.ID, "err", err)
			back = url.Values{"err": {"import"}}
		default:
			back = url.Values{"msg": {"imported"}, "movies": {strconv.Itoa(res.Movies)}, "series": {strconv.Itoa(res.Series)},
				"movies_skipped": {strconv.Itoa(res.MoviesSkipped)}, "series_skipped": {strconv.Itoa(res.SeriesSkipped)}}
		}
	}
	ui.redirect(w, r, page+"?"+back.Encode())
}
