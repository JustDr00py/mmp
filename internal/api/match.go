package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
)

// MountMatch registers matcher routes: regex rules, proposals, apply.
func MountMatch(r chi.Router, env *Env) {
	r.Route("/match", func(r chi.Router) {
		r.Get("/rules", env.listMatchRules)
		r.Post("/rules", env.createMatchRule)
		r.Delete("/rules/{id}", env.deleteMatchRule)
		r.Get("/proposals", env.listProposals)
		r.Post("/apply", env.applyProposals)
	})
}

func (env *Env) listMatchRules(w http.ResponseWriter, r *http.Request) {
	rules, err := env.Store.ListMatchRules(r.Context())
	if err != nil {
		env.Log.Error("api: list match rules", "err", err)
		writeErr(w, http.StatusInternalServerError, "list rules failed")
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func (env *Env) createMatchRule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Pattern     string `json:"pattern"`
		ChannelName string `json:"channel_name"`
		Priority    int32  `json:"priority"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := readJSON(w, r, &in); err != nil || in.Pattern == "" || in.ChannelName == "" {
		writeErr(w, http.StatusBadRequest, "pattern and channel_name are required")
		return
	}
	if _, err := channel.CompileRule(in.Pattern); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid pattern: "+err.Error())
		return
	}
	rule, err := env.Store.CreateMatchRule(r.Context(), db.CreateMatchRuleParams{
		Pattern: in.Pattern, ChannelName: in.ChannelName, Priority: in.Priority,
		Enabled: defBool(in.Enabled, true),
	})
	if err != nil {
		env.Log.Error("api: create match rule", "err", err)
		writeErr(w, http.StatusInternalServerError, "create rule failed")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (env *Env) deleteMatchRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := env.Store.DeleteMatchRule(r.Context(), id); err != nil {
		env.Log.Error("api: delete match rule", "rule_id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "delete rule failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listProposals previews a matching pass without changing anything.
// ?min_providers=N hides groups seen at fewer than N providers.
func (env *Env) listProposals(w http.ResponseWriter, r *http.Request) {
	proposals, err := env.Match.Propose(r.Context())
	if err != nil {
		env.Log.Error("api: match proposals", "err", err)
		writeErr(w, http.StatusInternalServerError, "match failed")
		return
	}
	minProv := clampInt(r.URL.Query().Get("min_providers"), 0, 0, 1000)
	out := make([]channel.Proposal, 0, len(proposals))
	for _, p := range proposals {
		if p.Providers >= minProv {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// applyProposals confirms proposals: {"keys": [...], "min_providers": N}.
// No keys means auto-create for every proposal that passes min_providers.
func (env *Env) applyProposals(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Keys         []string `json:"keys"`
		MinProviders int      `json:"min_providers"`
	}
	if err := readJSON(w, r, &in); err != nil || in.MinProviders < 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	res, err := env.Match.Apply(r.Context(), channel.ApplyOptions{Keys: in.Keys, MinProviders: in.MinProviders})
	if err != nil {
		env.Log.Error("api: match apply", "err", err)
		writeErr(w, http.StatusInternalServerError, "apply failed")
		return
	}
	writeJSON(w, http.StatusOK, res)
}
