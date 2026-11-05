package gluetun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

const settingKey = "vpn.location"

// Store holds the saved location (satisfied by *db.Queries).
type Store interface {
	GetSetting(ctx context.Context, key string) ([]byte, error)
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
}

// Saved is the location chosen in the admin UI. Gluetun forgets locations
// set through its API when it restarts, so the Keeper sets it again.
type Saved struct {
	// Active: MMP is in charge of the location. Off, gluetun uses .env's
	// SERVER_COUNTRIES and friends.
	Active   bool     `json:"active"`
	Location Location `json:"location"`
	// Default is gluetun's location before MMP first changed it (what
	// .env set); going back to .env restores it without a restart.
	Default *Location `json:"default,omitempty"`
}

// Keeper changes gluetun's location and keeps it there.
type Keeper struct {
	Client *Client
	Store  Store
	Log    *slog.Logger
	// Interval between checks that gluetun still has the saved location
	// (it loses it on a restart); 0 means a minute.
	Interval time.Duration

	mu sync.Mutex // one change at a time (admin UI and the check)
}

// Load reads the saved location (inactive if never saved).
func (k *Keeper) Load(ctx context.Context) (Saved, error) {
	var s Saved
	raw, err := k.Store.GetSetting(ctx, settingKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("vpn location: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Saved{}, fmt.Errorf("vpn location: %w", err)
	}
	return s, nil
}

func (k *Keeper) save(ctx context.Context, s Saved) error {
	raw, _ := json.Marshal(s)
	if err := k.Store.SetSetting(ctx, db.SetSettingParams{Key: settingKey, Value: raw}); err != nil {
		return fmt.Errorf("vpn location: %w", err)
	}
	return nil
}

// Set moves gluetun to l and saves it. Nothing is saved if gluetun
// refuses it (an unknown city, say); its message is the error.
func (k *Keeper) Set(ctx context.Context, l Location) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, err := k.Load(ctx)
	if err != nil {
		return "", err
	}
	if s.Default == nil {
		cur, err := k.Client.Settings(ctx)
		if err != nil {
			return "", err
		}
		s.Default = &cur.Location
	}
	outcome, err := k.Client.SetLocation(ctx, l)
	if err != nil {
		return "", err
	}
	s.Active, s.Location = true, l
	return outcome, k.save(ctx, s)
}

// UseDefault hands the location back to .env: gluetun goes back to the
// location it had before MMP changed it.
func (k *Keeper) UseDefault(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, err := k.Load(ctx)
	if err != nil || !s.Active {
		return "", err
	}
	var outcome string
	if s.Default != nil {
		if outcome, err = k.Client.SetLocation(ctx, *s.Default); err != nil {
			return "", err
		}
	}
	return outcome, k.save(ctx, Saved{})
}

// Sync sets the saved location again if gluetun lost it. It reports
// whether it had to.
func (k *Keeper) Sync(ctx context.Context) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, err := k.Load(ctx)
	if err != nil || !s.Active {
		return false, err
	}
	cur, err := k.Client.Settings(ctx)
	if err != nil {
		return false, err
	}
	if cur.Location.Equal(s.Location) {
		return false, nil
	}
	if _, err := k.Client.SetLocation(ctx, s.Location); err != nil {
		return false, err
	}
	return true, nil
}

// Run checks every Interval until ctx ends, starting now.
func (k *Keeper) Run(ctx context.Context) {
	every := k.Interval
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var lastErr string
	for {
		applied, err := k.Sync(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			if err.Error() != lastErr { // once per kind of failure, not every minute
				k.Log.Warn("vpn: cannot check gluetun's location", "err", err)
			}
			lastErr = err.Error()
		case err == nil:
			if lastErr != "" {
				k.Log.Info("vpn: gluetun reachable again")
			}
			lastErr = ""
			if applied {
				k.Log.Info("vpn: gluetun had lost the location chosen in MMP (restarted?); set it again")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
