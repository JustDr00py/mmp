// Package events keeps the events table (failovers, open failures, outages),
// customer activity and the admin audit log to a set age.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// Retention is how long events are kept, edited on the Settings page.
type Retention struct {
	KeepDays int `json:"keep_days"`
}

const retentionKey = "events"

// DefaultRetention keeps 30 days.
func DefaultRetention() Retention { return Retention{KeepDays: 30} }

// Validate checks a retention from the form.
func (r Retention) Validate() error {
	if r.KeepDays < 1 || r.KeepDays > 3650 {
		return errors.New("keep events between 1 and 3650 days")
	}
	return nil
}

// SettingsStore reads and writes settings (satisfied by *db.Queries).
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) ([]byte, error)
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
}

// LoadRetention reads the retention (the default if never saved).
func LoadRetention(ctx context.Context, q SettingsStore) (Retention, error) {
	r := DefaultRetention()
	raw, err := q.GetSetting(ctx, retentionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("event retention: %w", err)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return DefaultRetention(), fmt.Errorf("event retention: %w", err)
	}
	return r, nil
}

// SaveRetention stores the retention; the next prune uses it.
func SaveRetention(ctx context.Context, q SettingsStore, r Retention) error {
	if err := r.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(r)
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: retentionKey, Value: raw}); err != nil {
		return fmt.Errorf("event retention: %w", err)
	}
	return nil
}

// PruneStore is what pruning needs (satisfied by *db.Queries).
type PruneStore interface {
	SettingsStore
	PruneEvents(ctx context.Context, before time.Time) (int64, error)
	PruneViewingSessions(ctx context.Context, before time.Time) (int64, error)
	PruneUserNetworks(ctx context.Context, before time.Time) (int64, error)
	PruneAuthFailures(ctx context.Context, before time.Time) (int64, error)
	PruneAudit(ctx context.Context, before time.Time) (int64, error)
	PruneOutcomeBuckets(ctx context.Context, before time.Time) (int64, error)
}

// Prune deletes the events, customer activity and audit entries older than the
// retention, in batches so a large first prune does not hold one long
// transaction. It returns how many rows went.
func Prune(ctx context.Context, q PruneStore, now time.Time) (int64, error) {
	r, err := LoadRetention(ctx, q)
	if err != nil {
		return 0, err
	}
	before := now.AddDate(0, 0, -r.KeepDays)
	var total int64
	for _, t := range []struct {
		name  string
		prune func(context.Context, time.Time) (int64, error)
	}{
		{"events", q.PruneEvents},
		{"provider outcomes", q.PruneOutcomeBuckets},
		{"viewing sessions", q.PruneViewingSessions},
		{"user networks", q.PruneUserNetworks},
		{"failed logins", q.PruneAuthFailures},
		{"audit log", q.PruneAudit},
	} {
		for {
			n, err := t.prune(ctx, before)
			total += n
			if err != nil {
				return total, fmt.Errorf("prune %s: %w", t.name, err)
			}
			if n == 0 {
				break
			}
		}
	}
	return total, nil
}

// RunPruner prunes at once and then every interval until ctx ends.
func RunPruner(ctx context.Context, q PruneStore, log *slog.Logger, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := Prune(ctx, q, time.Now()); err != nil {
			if ctx.Err() == nil {
				log.Warn("prune events", "err", err)
			}
		} else if n > 0 {
			log.Info("pruned old events and activity", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
