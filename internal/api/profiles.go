package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// MountProfiles registers stream profile routes. A profile in ffmpeg mode
// pipes its channels through ffmpeg with ffmpeg_args (e.g. "-c copy");
// changes apply to sessions started afterwards.
func MountProfiles(r chi.Router, env *Env) {
	r.Route("/stream-profiles", func(r chi.Router) {
		r.Get("/", env.listProfiles)
		r.Post("/", env.createProfile)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", env.getProfile)
			r.Put("/", env.updateProfile)
			r.Delete("/", env.deleteProfile)
		})
	})
}

type profileInput struct {
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	FFmpegArgs string `json:"ffmpeg_args"`
	UserAgent  string `json:"user_agent"`
}

func (in *profileInput) validate() error {
	if in.Name == "" {
		return errors.New("name is required")
	}
	if in.Mode == "" {
		in.Mode = "passthrough"
	}
	return proxy.ValidateProfile(in.Mode, in.FFmpegArgs)
}

func (env *Env) listProfiles(w http.ResponseWriter, r *http.Request) {
	ps, err := env.Store.ListStreamProfiles(r.Context())
	if err != nil {
		env.Log.Error("api: list profiles", "err", err)
		writeErr(w, http.StatusInternalServerError, "list profiles failed")
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (env *Env) createProfile(w http.ResponseWriter, r *http.Request) {
	var in profileInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := env.Store.CreateStreamProfile(r.Context(), db.CreateStreamProfileParams{
		Name: in.Name, Mode: in.Mode, FfmpegArgs: in.FFmpegArgs, UserAgent: in.UserAgent,
	})
	if err != nil {
		env.writeProfileErr(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (env *Env) getProfile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := env.Store.GetStreamProfile(r.Context(), id)
	if err != nil {
		env.writeProfileErr(w, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (env *Env) updateProfile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in profileInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := env.Store.UpdateStreamProfile(r.Context(), db.UpdateStreamProfileParams{
		ID: id, Name: in.Name, Mode: in.Mode, FfmpegArgs: in.FFmpegArgs, UserAgent: in.UserAgent,
	})
	if err != nil {
		env.writeProfileErr(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// deleteProfile removes a profile; its channels fall back to passthrough
// (ON DELETE SET NULL).
func (env *Env) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := env.Store.DeleteStreamProfile(r.Context(), id)
	if err != nil {
		env.writeProfileErr(w, "delete", err)
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "profile not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (env *Env) writeProfileErr(w http.ResponseWriter, op string, err error) {
	switch {
	case isNotFound(err):
		writeErr(w, http.StatusNotFound, "profile not found")
	case pgCode(err) == pgUniqueViolation:
		writeErr(w, http.StatusConflict, "profile name already exists")
	default:
		env.Log.Error("api: "+op+" profile", "err", err)
		writeErr(w, http.StatusInternalServerError, op+" profile failed")
	}
}
