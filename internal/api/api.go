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
func Mount(r chi.Router, env *Env) {
	MountProviders(r, env)
	MountChannels(r, env)
	MountMatch(r, env)
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
