// Package api implements the admin REST API under /api/v1.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source"
)

// Env carries the API's dependencies.
type Env struct {
	Log     *slog.Logger
	Store   Store
	Refresh Refresher
}

// Store is the persistence surface the API needs (satisfied by *db.Queries).
type Store interface {
	CreateProvider(ctx context.Context, arg db.CreateProviderParams) (db.Provider, error)
	GetProvider(ctx context.Context, id int64) (db.Provider, error)
	ListProviders(ctx context.Context) ([]db.Provider, error)
	UpdateProvider(ctx context.Context, arg db.UpdateProviderParams) (db.Provider, error)
	DeleteProvider(ctx context.Context, id int64) error
	SetProviderEnabled(ctx context.Context, arg db.SetProviderEnabledParams) error
	ListProviderStreams(ctx context.Context, arg db.ListProviderStreamsParams) ([]db.ProviderStream, error)
	SearchProviderStreams(ctx context.Context, arg db.SearchProviderStreamsParams) ([]db.ProviderStream, error)
}

// Refresher is the on-demand refresh surface (satisfied by *source.Refresher).
type Refresher interface {
	RefreshProvider(ctx context.Context, p db.Provider) (source.Result, error)
}

// Mount registers the API routes on r (mounted at /api/v1).
func Mount(r chi.Router, env *Env) {
	r.Route("/providers", func(r chi.Router) {
		r.Get("/", env.listProviders)
		r.Post("/", env.createProvider)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", env.getProvider)
			r.Put("/", env.updateProvider)
			r.Delete("/", env.deleteProvider)
			r.Post("/refresh", env.refreshProvider)
			r.Get("/streams", env.listProviderStreams)
		})
	})
}

// writeJSON writes v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr writes {"error": msg}.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON decodes a bounded JSON body into v.
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return dec.Decode(v)
}

// pathID extracts the {id} URL parameter.
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// isNotFound reports a missing-row error.
func isNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
