package audit

import (
	"context"
	"strconv"

	"github.com/JustDr00py/mmp/internal/db"
)

// NameStore looks targets up by id (satisfied by *db.Queries).
type NameStore interface {
	GetProvider(ctx context.Context, id int64) (db.Provider, error)
	GetChannel(ctx context.Context, id int64) (db.Channel, error)
	GetChannelGroup(ctx context.Context, id int64) (db.ChannelGroup, error)
	GetBouquet(ctx context.Context, id int64) (db.Bouquet, error)
	GetUser(ctx context.Context, id int64) (db.User, error)
	GetStreamProfile(ctx context.Context, id int64) (db.StreamProfile, error)
	GetEpgSource(ctx context.Context, id int64) (db.EpgSource, error)
}

// StoreNamer names targets from the database; a backup is its file name.
func StoreNamer(q NameStore) Namer {
	return func(ctx context.Context, targetType, id string) string {
		if targetType == "backup" {
			return id
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return ""
		}
		var name string
		switch targetType {
		case "provider":
			v, e := q.GetProvider(ctx, n)
			name, err = v.Name, e
		case "channel":
			v, e := q.GetChannel(ctx, n)
			name, err = v.Name, e
		case "channel group":
			v, e := q.GetChannelGroup(ctx, n)
			name, err = v.Name, e
		case "bouquet":
			v, e := q.GetBouquet(ctx, n)
			name, err = v.Name, e
		case "user":
			v, e := q.GetUser(ctx, n)
			name, err = v.Username, e
		case "stream profile":
			v, e := q.GetStreamProfile(ctx, n)
			name, err = v.Name, e
		case "guide source":
			v, e := q.GetEpgSource(ctx, n)
			name, err = v.Name, e
		}
		if err != nil {
			return ""
		}
		return name
	}
}
