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

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/channel"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source"
)

// Env carries the API's dependencies.
type Env struct {
	Log     *slog.Logger
	Store   Store
	Refresh Refresher
	Match   Matcher
	Streams StreamAttacher
	// Auth guards every route but login. Nil disables auth (unit tests).
	Auth *auth.Authenticator
	// InTx runs fn against a transactional Store. Nil runs fn directly on
	// Store (unit tests with in-memory fakes).
	InTx func(ctx context.Context, fn func(Store) error) error
}

// inTx runs fn atomically when the env supports transactions.
func (env *Env) inTx(ctx context.Context, fn func(Store) error) error {
	if env.InTx == nil {
		return fn(env.Store)
	}
	return env.InTx(ctx, fn)
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

	CreateChannelGroup(ctx context.Context, arg db.CreateChannelGroupParams) (db.ChannelGroup, error)
	ListChannelGroups(ctx context.Context) ([]db.ChannelGroup, error)
	UpdateChannelGroup(ctx context.Context, arg db.UpdateChannelGroupParams) (db.ChannelGroup, error)
	DeleteChannelGroup(ctx context.Context, id int64) error

	CreateChannel(ctx context.Context, arg db.CreateChannelParams) (db.Channel, error)
	GetChannel(ctx context.Context, id int64) (db.Channel, error)
	ListChannels(ctx context.Context, arg db.ListChannelsParams) ([]db.Channel, error)
	UpdateChannel(ctx context.Context, arg db.UpdateChannelParams) (db.Channel, error)
	DeleteChannel(ctx context.Context, id int64) error
	NextChannelNumber(ctx context.Context) (int32, error)

	AddStreamToChannel(ctx context.Context, arg db.AddStreamToChannelParams) error
	RemoveStreamFromChannel(ctx context.Context, arg db.RemoveStreamFromChannelParams) error
	ReorderChannelStreams(ctx context.Context, arg db.ReorderChannelStreamsParams) error
	ListChannelStreams(ctx context.Context, channelID int64) ([]db.ListChannelStreamsRow, error)

	CreateBouquet(ctx context.Context, arg db.CreateBouquetParams) (db.Bouquet, error)
	ListBouquets(ctx context.Context) ([]db.Bouquet, error)
	GetBouquet(ctx context.Context, id int64) (db.Bouquet, error)
	UpdateBouquet(ctx context.Context, arg db.UpdateBouquetParams) (db.Bouquet, error)
	DeleteBouquet(ctx context.Context, id int64) error
	AddChannelToBouquet(ctx context.Context, arg db.AddChannelToBouquetParams) error
	RemoveChannelFromBouquet(ctx context.Context, arg db.RemoveChannelFromBouquetParams) error
	ListBouquetChannels(ctx context.Context, bouquetID int64) ([]db.Channel, error)

	CreateMatchRule(ctx context.Context, arg db.CreateMatchRuleParams) (db.MatchRule, error)
	ListMatchRules(ctx context.Context) ([]db.MatchRule, error)
	DeleteMatchRule(ctx context.Context, id int64) error

	CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error)
	GetUser(ctx context.Context, id int64) (db.User, error)
	ListUsers(ctx context.Context) ([]db.User, error)
	UpdateUser(ctx context.Context, arg db.UpdateUserParams) (db.User, error)
	SetUserStreamToken(ctx context.Context, arg db.SetUserStreamTokenParams) (db.User, error)
	DeleteUser(ctx context.Context, id int64) error
	ClearUserBouquets(ctx context.Context, userID int64) error
	AddUserBouquets(ctx context.Context, arg db.AddUserBouquetsParams) error
	ListUserBouquetIDs(ctx context.Context, userID int64) ([]int64, error)
	CountAdmins(ctx context.Context) (int64, error)

	CreateAPIToken(ctx context.Context, arg db.CreateAPITokenParams) (db.CreateAPITokenRow, error)
	ListAPITokens(ctx context.Context) ([]db.ListAPITokensRow, error)
	DeleteAPIToken(ctx context.Context, id int64) (int64, error)
}

// Refresher is the on-demand refresh surface (satisfied by *source.Refresher).
type Refresher interface {
	RefreshProvider(ctx context.Context, p db.Provider) (source.Result, error)
}

// Matcher proposes and applies cross-provider channel groupings
// (satisfied by *channel.Service).
type Matcher interface {
	Propose(ctx context.Context) ([]channel.Proposal, error)
	Apply(ctx context.Context, opts channel.ApplyOptions) (channel.ApplyResult, error)
}

// Mount registers the API routes on r (mounted at /api/v1).
// Everything except POST /auth/login requires an admin when env.Auth is set.
func Mount(r chi.Router, env *Env) {
	mountAuthPublic(r, env)
	r.Group(func(r chi.Router) {
		if env.Auth != nil {
			r.Use(env.Auth.RequireAdmin)
		}
		mountAuthPrivate(r, env)
		MountProviders(r, env)
		MountChannels(r, env)
		MountMatch(r, env)
		MountUsers(r, env)
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
