package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// providerJSON is the wire form of a provider. The password is never
// serialized: an absent password on update means "keep the current one".
type providerJSON struct {
	ID                  int64  `json:"id"`
	Name                string `json:"name"`
	Type                string `json:"type"`
	URL                 string `json:"url"`
	Username            string `json:"username,omitempty"`
	EPGURL              string `json:"epg_url,omitempty"`
	MaxConnections      int32  `json:"max_connections"`
	UserAgent           string `json:"user_agent,omitempty"`
	RefreshIntervalSecs int32  `json:"refresh_interval_secs"`
	Priority            int32  `json:"priority"`
	Enabled             bool   `json:"enabled"`
	// Per-provider override of the refresh disable threshold; nil = default.
	RefreshMaxDisablePct *int32 `json:"refresh_max_disable_pct,omitempty"`
	// VODEnabled imports the provider's movies and series (Xtream only).
	VODEnabled     bool       `json:"vod_enabled"`
	VODRefreshedAt *time.Time `json:"vod_refreshed_at,omitempty"`
	// GroupFilter limits matching and the stream picker to the selected
	// groups (see /providers/{id}/groups).
	GroupFilter bool `json:"group_filter"`
}

func toJSON(p db.Provider) providerJSON {
	return providerJSON{
		ID: p.ID, Name: p.Name, Type: p.Type, URL: p.Url, Username: p.Username,
		EPGURL: p.EpgUrl, MaxConnections: p.MaxConnections, UserAgent: p.UserAgent,
		RefreshIntervalSecs: p.RefreshIntervalSecs, Priority: p.Priority, Enabled: p.Enabled,
		RefreshMaxDisablePct: p.RefreshMaxDisablePct,
		VODEnabled:           p.VodEnabled, VODRefreshedAt: p.VodRefreshedAt,
		GroupFilter: p.GroupFilter,
	}
}

// providerInput is the accepted create/update body.
type providerInput struct {
	Name                string `json:"name"`
	Type                string `json:"type"`
	URL                 string `json:"url"`
	Username            string `json:"username"`
	Password            string `json:"password"`
	EPGURL              string `json:"epg_url"`
	MaxConnections      *int32 `json:"max_connections"`
	UserAgent           string `json:"user_agent"`
	RefreshIntervalSecs *int32 `json:"refresh_interval_secs"`
	Priority            *int32 `json:"priority"`
	Enabled             *bool  `json:"enabled"`
	// 0-100; absent (or null) = the global default threshold.
	RefreshMaxDisablePct *int32 `json:"refresh_max_disable_pct"`
	// Absent keeps the current setting (off for new providers).
	VODEnabled *bool `json:"vod_enabled"`
}

func (in *providerInput) validate() error {
	if in.VODEnabled != nil && *in.VODEnabled && in.Type != "xtream" {
		return errors.New("vod_enabled needs an Xtream provider")
	}
	if err := source.ValidateRefreshMaxDisablePct(in.RefreshMaxDisablePct); err != nil {
		return err
	}
	return source.ValidateProvider(in.Name, in.Type, in.URL, in.EPGURL,
		defInt32(in.MaxConnections, 0), defInt32(in.RefreshIntervalSecs, 1))
}

func defInt32(p *int32, def int32) int32 {
	if p != nil {
		return *p
	}
	return def
}

func defBool(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

// MountProviders registers provider routes.
func MountProviders(r chi.Router, env *Env) {
	r.Route("/providers", func(r chi.Router) {
		r.Get("/", env.listProviders)
		r.Post("/", env.createProvider)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", env.getProvider)
			r.Put("/", env.updateProvider)
			r.Delete("/", env.deleteProvider)
			r.Post("/refresh", env.refreshProvider)
			r.Get("/streams", env.listProviderStreams)
			r.Get("/groups", env.listProviderGroups)
			r.Put("/groups", env.setProviderGroups)
		})
	})
}

func (env *Env) listProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := env.Store.ListProviders(r.Context())
	if err != nil {
		env.Log.Error("api: list providers", "err", err)
		writeErr(w, http.StatusInternalServerError, "list providers failed")
		return
	}
	out := make([]providerJSON, 0, len(providers))
	for _, p := range providers {
		out = append(out, toJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (env *Env) createProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := env.Store.CreateProvider(r.Context(), db.CreateProviderParams{
		Name: in.Name, Type: in.Type, Url: in.URL,
		Username: in.Username, Password: in.Password, EpgUrl: in.EPGURL,
		MaxConnections:       defInt32(in.MaxConnections, 1),
		UserAgent:            in.UserAgent,
		RefreshIntervalSecs:  defInt32(in.RefreshIntervalSecs, env.defaultRefreshSecs()),
		Priority:             defInt32(in.Priority, 0),
		Enabled:              defBool(in.Enabled, true),
		RefreshMaxDisablePct: in.RefreshMaxDisablePct,
	})
	if err != nil {
		env.Log.Error("api: create provider", "err", err)
		writeErr(w, http.StatusInternalServerError, "create provider failed")
		return
	}
	if in.VODEnabled != nil && *in.VODEnabled {
		if err := env.Store.SetProviderVODEnabled(r.Context(), db.SetProviderVODEnabledParams{ID: p.ID, VodEnabled: true}); err != nil {
			env.Log.Error("api: enable vod", "provider_id", p.ID, "err", err)
			writeErr(w, http.StatusInternalServerError, "create provider failed")
			return
		}
		p.VodEnabled = true
	}
	writeJSON(w, http.StatusCreated, toJSON(p))
}

func (env *Env) getProvider(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, http.StatusInternalServerError, "get provider failed")
		return
	}
	writeJSON(w, http.StatusOK, toJSON(p))
}

func (env *Env) updateProvider(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in providerInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	current, err := env.Store.GetProvider(r.Context(), id)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		env.Log.Error("api: get provider", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "update provider failed")
		return
	}
	// Empty password keeps the stored one.
	password := in.Password
	if password == "" {
		password = current.Password
	}
	// Empty password keeps the stored one; likewise an absent threshold
	// keeps the stored override (set 100 to disable the guard outright).
	pct := in.RefreshMaxDisablePct
	if pct == nil {
		pct = current.RefreshMaxDisablePct
	}
	p, err := env.Store.UpdateProvider(r.Context(), db.UpdateProviderParams{
		ID:   id,
		Name: in.Name, Type: in.Type, Url: in.URL,
		Username: in.Username, Password: password, EpgUrl: in.EPGURL,
		MaxConnections:       defInt32(in.MaxConnections, current.MaxConnections),
		UserAgent:            in.UserAgent,
		RefreshIntervalSecs:  defInt32(in.RefreshIntervalSecs, current.RefreshIntervalSecs),
		Priority:             defInt32(in.Priority, current.Priority),
		Enabled:              defBool(in.Enabled, current.Enabled),
		RefreshMaxDisablePct: pct,
	})
	if err != nil {
		env.Log.Error("api: update provider", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "update provider failed")
		return
	}
	// Absent vod_enabled keeps the stored flag, except that a non-Xtream
	// provider cannot keep VOD on.
	vod := current.VodEnabled
	if in.VODEnabled != nil {
		vod = *in.VODEnabled
	}
	if p.Type != "xtream" {
		vod = false
	}
	if vod != p.VodEnabled {
		if err := env.Store.SetProviderVODEnabled(r.Context(), db.SetProviderVODEnabledParams{ID: p.ID, VodEnabled: vod}); err != nil {
			env.Log.Error("api: set vod", "provider_id", p.ID, "err", err)
			writeErr(w, http.StatusInternalServerError, "update provider failed")
			return
		}
		p.VodEnabled = vod
	}
	writeJSON(w, http.StatusOK, toJSON(p))
}

func (env *Env) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := env.Store.DeleteProvider(r.Context(), id); err != nil {
		env.Log.Error("api: delete provider", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "delete provider failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (env *Env) refreshProvider(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, http.StatusInternalServerError, "refresh failed")
		return
	}
	// ?force=1 applies a refresh that the disable threshold would refuse.
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	res, err := env.Refresh.RefreshProvider(r.Context(), p, force)
	if err != nil {
		var refused *source.DisableRefusedError
		if errors.As(err, &refused) {
			env.Log.Warn("api: refresh provider refused", "provider_id", id, "would_disable", refused.WouldDisable, "active", refused.Active)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           "refresh refused: " + refused.Error(),
				"code":            "too_many_disabled",
				"would_disable":   refused.WouldDisable,
				"active":          refused.Active,
				"max_disable_pct": refused.MaxPct,
				"force_url":       "/api/v1/providers/" + strconv.FormatInt(id, 10) + "/refresh?force=1",
			})
			return
		}
		env.Log.Error("api: refresh provider", "provider_id", id, "err", err)
		writeErr(w, http.StatusBadGateway, "refresh failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (env *Env) listProviderStreams(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampInt(r.URL.Query().Get("limit"), 50, 1, 500)
	offset := clampInt(r.URL.Query().Get("offset"), 0, 0, 1<<30)
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	var streams []db.ProviderStream
	if q != "" {
		streams, err = env.Store.SearchProviderStreams(r.Context(), db.SearchProviderStreamsParams{
			ProviderID: id, Name: "%" + q + "%", Limit: int32(limit), Offset: int32(offset),
		})
	} else {
		streams, err = env.Store.ListProviderStreams(r.Context(), db.ListProviderStreamsParams{
			ProviderID: id, Limit: int32(limit), Offset: int32(offset),
		})
	}
	if err != nil {
		env.Log.Error("api: list provider streams", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "list streams failed")
		return
	}
	// stream URLs are provider-side secrets for customers, but the admin may
	// see them; passwords inside Xtream URLs stay, this is the admin API.
	type streamJSON struct {
		ID         int64  `json:"id"`
		ExternalID string `json:"external_id"`
		Name       string `json:"name"`
		TvgID      string `json:"tvg_id"`
		LogoURL    string `json:"logo_url"`
		Group      string `json:"group_title"`
		StreamURL  string `json:"stream_url"`
		Disabled   bool   `json:"disabled"`
	}
	out := make([]streamJSON, 0, len(streams))
	for _, s := range streams {
		out = append(out, streamJSON{
			ID: s.ID, ExternalID: s.ExternalID, Name: s.Name, TvgID: s.TvgID,
			LogoURL: s.LogoUrl, Group: s.GroupTitle, StreamURL: s.StreamUrl, Disabled: s.Disabled,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// clampInt parses v as int, defaulting to def and clamping to [min, max].
func clampInt(v string, def, min, max int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

type providerGroupJSON struct {
	Name     string `json:"name"`
	Streams  int64  `json:"streams"`
	Selected bool   `json:"selected"`
}

type providerGroupsJSON struct {
	Filter bool                `json:"filter"`
	Groups []providerGroupJSON `json:"groups"`
}

// listProviderGroups lists the provider's stream groups and the selection.
func (env *Env) listProviderGroups(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, http.StatusInternalServerError, "get provider failed")
		return
	}
	gs, err := env.Store.ListProviderGroups(r.Context(), id)
	if err != nil {
		env.Log.Error("api: list groups", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "list groups failed")
		return
	}
	out := providerGroupsJSON{Filter: p.GroupFilter, Groups: make([]providerGroupJSON, 0, len(gs))}
	for _, g := range gs {
		out.Groups = append(out.Groups, providerGroupJSON{Name: g.GroupTitle, Streams: g.Streams, Selected: g.Selected})
	}
	writeJSON(w, http.StatusOK, out)
}

// setProviderGroups replaces the selection: {"filter": true, "groups": [...]}
// offers only those groups; {"filter": false} offers all (groups, if given,
// are stored for later).
func (env *Env) setProviderGroups(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in struct {
		Filter bool      `json:"filter"`
		Groups *[]string `json:"groups"`
	}
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := env.Store.GetProvider(r.Context(), id); isNotFound(err) {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	} else if err != nil {
		env.Log.Error("api: get provider", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "get provider failed")
		return
	}
	err = env.inTx(r.Context(), func(st Store) error {
		if in.Groups != nil {
			if err := st.ClearProviderGroups(r.Context(), id); err != nil {
				return err
			}
			if err := st.AddProviderGroups(r.Context(), db.AddProviderGroupsParams{ProviderID: id, GroupTitles: *in.Groups}); err != nil {
				return err
			}
		}
		return st.SetProviderGroupFilter(r.Context(), db.SetProviderGroupFilterParams{ID: id, GroupFilter: in.Filter})
	})
	if err != nil {
		env.Log.Error("api: set groups", "provider_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "set groups failed")
		return
	}
	env.listProviderGroups(w, r)
}
