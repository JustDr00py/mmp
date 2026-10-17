package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

// mountAuthPublic registers the only unauthenticated admin route.
func mountAuthPublic(r chi.Router, env *Env) {
	if env.Auth != nil {
		r.Post("/auth/login", env.login)
	}
}

// mountAuthPrivate registers session and API token routes (admin only).
func mountAuthPrivate(r chi.Router, env *Env) {
	r.Post("/auth/logout", env.logout)
	r.Get("/auth/me", env.me)
	r.Route("/tokens", func(r chi.Router) {
		r.Get("/", env.listTokens)
		r.Post("/", env.createToken)
		r.Delete("/{id}", env.deleteToken)
	})
}

func (env *Env) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &in); err != nil || in.Username == "" || in.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	attempt := auth.NewAttempt(r, in.Username)
	if ok, retry := env.LoginLimit.Allow(attempt); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too many attempts; retry later")
		return
	}
	token, u, err := env.Auth.Login(r.Context(), in.Username, in.Password)
	switch { // only password verdicts count; infrastructure errors do not
	case err == nil:
		env.LoginLimit.Done(attempt, true)
	case errors.Is(err, auth.ErrUnauthorized):
		env.LoginLimit.Done(attempt, false)
	}
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		env.Log.Error("api: login", "err", err)
		writeErr(w, http.StatusInternalServerError, "login failed")
		return
	}
	env.Auth.SetSessionCookie(w, token)
	v, err := env.viewUser(r.Context(), u)
	if err != nil {
		env.writeUserErr(w, "login", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (env *Env) logout(w http.ResponseWriter, r *http.Request) {
	if env.Auth != nil {
		if err := env.Auth.Logout(r.Context(), r); err != nil {
			env.Log.Warn("api: logout", "err", err)
		}
		env.Auth.SetSessionCookie(w, "")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (env *Env) me(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	v, err := env.viewUser(r.Context(), u)
	if err != nil {
		env.writeUserErr(w, "me", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (env *Env) listTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := env.Store.ListAPITokens(r.Context())
	if err != nil {
		env.Log.Error("api: list tokens", "err", err)
		writeErr(w, http.StatusInternalServerError, "list tokens failed")
		return
	}
	if toks == nil {
		toks = []db.ListAPITokensRow{}
	}
	writeJSON(w, http.StatusOK, toks)
}

// createToken issues an API token for the calling admin. The secret is in
// this response only; MMP keeps just its hash.
func (env *Env) createToken(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &in); err != nil || in.Name == "" || len(in.Name) > 100 {
		writeErr(w, http.StatusBadRequest, "name (1-100 chars) is required")
		return
	}
	secret, err := auth.NewStreamToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create token failed")
		return
	}
	row, err := env.Store.CreateAPIToken(r.Context(), db.CreateAPITokenParams{
		UserID: u.ID, Name: in.Name, TokenHash: auth.HashToken(secret),
	})
	if err != nil {
		env.Log.Error("api: create token", "err", err)
		writeErr(w, http.StatusInternalServerError, "create token failed")
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		ID         int64      `json:"id"`
		Name       string     `json:"name"`
		Token      string     `json:"token"`
		CreatedAt  time.Time  `json:"created_at"`
		LastUsedAt *time.Time `json:"last_used_at"`
	}{row.ID, row.Name, secret, row.CreatedAt, row.LastUsedAt})
}

func (env *Env) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := env.Store.DeleteAPIToken(r.Context(), id)
	if err != nil {
		env.Log.Error("api: delete token", "err", err)
		writeErr(w, http.StatusInternalServerError, "delete token failed")
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
