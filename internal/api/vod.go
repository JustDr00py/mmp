package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// MountVOD registers movie/series catalog routes: browsing, on-demand
// import, and bouquet category membership.
func MountVOD(r chi.Router, env *Env) {
	r.Route("/vod", func(r chi.Router) {
		r.Get("/categories", env.listVODCategories)
		r.Get("/movies", env.listVODMovies)
		r.Get("/series", env.listVODSeries)
	})
	r.Post("/providers/{id}/vod/refresh", env.refreshProviderVOD)
	r.Route("/bouquets/{id}/vod-categories", func(r chi.Router) {
		r.Get("/", env.listBouquetVODCategories)
		r.Post("/", env.addBouquetVODCategories)
		r.Delete("/{cid}", env.removeBouquetVODCategory)
	})
}

// optInt64 parses an optional positive id query parameter.
func optInt64(r *http.Request, key string) (*int64, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil, false
	}
	return &n, true
}

func (env *Env) listVODCategories(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "movie"
	}
	if kind != "movie" && kind != "series" {
		writeErr(w, http.StatusBadRequest, "kind must be movie or series")
		return
	}
	pid, ok := optInt64(r, "provider_id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid provider_id")
		return
	}
	cats, err := env.Store.ListVODCategoriesAdmin(r.Context(), db.ListVODCategoriesAdminParams{Kind: kind, ProviderID: pid})
	if err != nil {
		env.Log.Error("api: list vod categories", "err", err)
		writeErr(w, http.StatusInternalServerError, "list categories failed")
		return
	}
	if cats == nil {
		cats = []db.ListVODCategoriesAdminRow{}
	}
	writeJSON(w, http.StatusOK, cats)
}

// catalogFilter reads provider_id, category_id, q, limit and offset.
func catalogFilter(w http.ResponseWriter, r *http.Request) (pid, cid *int64, search *string, limit, offset int32, ok bool) {
	var okP, okC bool
	if pid, okP = optInt64(r, "provider_id"); !okP {
		writeErr(w, http.StatusBadRequest, "invalid provider_id")
		return
	}
	if cid, okC = optInt64(r, "category_id"); !okC {
		writeErr(w, http.StatusBadRequest, "invalid category_id")
		return
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		s := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		search = &s
	}
	limit = int32(clampInt(r.URL.Query().Get("limit"), 100, 1, 1000))
	offset = int32(clampInt(r.URL.Query().Get("offset"), 0, 0, 1<<30))
	return pid, cid, search, limit, offset, true
}

func (env *Env) listVODMovies(w http.ResponseWriter, r *http.Request) {
	pid, cid, search, limit, offset, ok := catalogFilter(w, r)
	if !ok {
		return
	}
	rows, err := env.Store.ListVODItemsAdmin(r.Context(), db.ListVODItemsAdminParams{
		ProviderID: pid, CategoryID: cid, Search: search, Limit: limit, Offset: offset})
	if err != nil {
		env.Log.Error("api: list movies", "err", err)
		writeErr(w, http.StatusInternalServerError, "list movies failed")
		return
	}
	if rows == nil {
		rows = []db.ListVODItemsAdminRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (env *Env) listVODSeries(w http.ResponseWriter, r *http.Request) {
	pid, cid, search, limit, offset, ok := catalogFilter(w, r)
	if !ok {
		return
	}
	rows, err := env.Store.ListSeriesAdmin(r.Context(), db.ListSeriesAdminParams{
		ProviderID: pid, CategoryID: cid, Search: search, Limit: limit, Offset: offset})
	if err != nil {
		env.Log.Error("api: list series", "err", err)
		writeErr(w, http.StatusInternalServerError, "list series failed")
		return
	}
	if rows == nil {
		rows = []db.ListSeriesAdminRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// refreshProviderVOD imports the provider's catalogs now (?force=1 applies
// an import the disable threshold refused).
func (env *Env) refreshProviderVOD(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := env.Store.GetProvider(r.Context(), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		env.Log.Error("api: get provider", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "vod refresh failed")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	res, err := env.Refresh.RefreshVOD(r.Context(), p, force)
	if err != nil {
		var refused *source.DisableRefusedError
		switch {
		case errors.Is(err, source.ErrVODUnsupported):
			writeErr(w, http.StatusBadRequest, err.Error())
		case errors.As(err, &refused):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "vod refresh refused: " + refused.Error(), "code": "too_many_disabled",
				"would_disable": refused.WouldDisable, "active": refused.Active, "max_disable_pct": refused.MaxPct,
				"force_url": "/api/v1/providers/" + strconv.FormatInt(id, 10) + "/vod/refresh?force=1",
			})
		default:
			env.Log.Error("api: vod refresh", "provider_id", id, "err", err)
			writeErr(w, http.StatusBadGateway, "vod refresh failed: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (env *Env) listBouquetVODCategories(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cats, err := env.Store.ListBouquetVODCategories(r.Context(), id)
	if err != nil {
		env.Log.Error("api: bouquet vod categories", "err", err)
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	if cats == nil {
		cats = []db.ListBouquetVODCategoriesRow{}
	}
	writeJSON(w, http.StatusOK, cats)
}

func (env *Env) addBouquetVODCategories(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in struct {
		CategoryIDs []int64 `json:"category_ids"`
	}
	if err := readJSON(w, r, &in); err != nil || len(in.CategoryIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "category_ids is required")
		return
	}
	n, err := env.Store.AddVODCategoriesToBouquet(r.Context(), db.AddVODCategoriesToBouquetParams{BouquetID: id, CategoryIds: in.CategoryIDs})
	if pgCode(err) == pgForeignKeyViolation {
		writeErr(w, http.StatusBadRequest, "unknown bouquet or category")
		return
	}
	if err != nil {
		env.Log.Error("api: add bouquet vod categories", "err", err)
		writeErr(w, http.StatusInternalServerError, "add failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"added": n})
}

func (env *Env) removeBouquetVODCategory(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil || cid <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid category id")
		return
	}
	if _, err := env.Store.RemoveVODCategoryFromBouquet(r.Context(), db.RemoveVODCategoryFromBouquetParams{BouquetID: id, VodCategoryID: cid}); err != nil {
		env.Log.Error("api: remove bouquet vod category", "err", err)
		writeErr(w, http.StatusInternalServerError, "remove failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
