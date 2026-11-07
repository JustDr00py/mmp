// Package eventroute routes games to team channels. Event slot channels
// ("NBA 01"…"NBA 20") carry each night's games and their stream names say
// which ("Milwaukee Bucks vs Toronto Raptors @ Nov 6 8:00 PM :NBA  06").
// While a game is on, each playing team's channel plays the slot channel
// first and its own streams after (see channel_event_overrides), then goes
// back to its own list.
package eventroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// Rule routes games found in the slot channels of SourceGroupIDs to the
// team channels of TargetGroupIDs.
type Rule struct {
	SourceGroupIDs []int64 `json:"source_group_ids"`
	TargetGroupIDs []int64 `json:"target_group_ids"`
}

// Settings are edited on the Settings page.
type Settings struct {
	Enabled bool   `json:"enabled"`
	Rules   []Rule `json:"rules"`
	// BeforeMins and AfterMins bound when a team plays its game's slot:
	// from BeforeMins before the start a name gives to AfterMins after.
	BeforeMins int `json:"before_mins"`
	AfterMins  int `json:"after_mins"`
}

const settingsKey = "event_routes"

// DefaultSettings: off, no rules, from 30 minutes before to 4 hours after
// the start.
func DefaultSettings() Settings { return Settings{BeforeMins: 30, AfterMins: 240} }

// Before and After are the window as durations.
func (s Settings) Before() time.Duration { return time.Duration(s.BeforeMins) * time.Minute }
func (s Settings) After() time.Duration  { return time.Duration(s.AfterMins) * time.Minute }

// Validate checks settings from the form.
func (s Settings) Validate() error {
	if s.BeforeMins < 0 || s.BeforeMins > 24*60 {
		return fmt.Errorf("minutes before the start must be 0 to %d", 24*60)
	}
	if s.AfterMins < 30 || s.AfterMins > 24*60 {
		return fmt.Errorf("minutes after the start must be 30 to %d", 24*60)
	}
	for _, r := range s.Rules {
		if len(r.SourceGroupIDs) == 0 || len(r.TargetGroupIDs) == 0 {
			return errors.New("pick a game group and at least one team group")
		}
		for _, id := range r.SourceGroupIDs {
			if slices.Contains(r.TargetGroupIDs, id) {
				return errors.New("a group can't be both the game group and a team group")
			}
		}
	}
	if s.Enabled && len(s.Rules) == 0 {
		return errors.New("pick a game group and team groups to turn game routing on")
	}
	return nil
}

// Load reads the settings (the defaults if never saved).
func Load(ctx context.Context, q interface {
	GetSetting(ctx context.Context, key string) ([]byte, error)
}) (Settings, error) {
	s := DefaultSettings()
	raw, err := q.GetSetting(ctx, settingsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("game routing settings: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return DefaultSettings(), fmt.Errorf("game routing settings: %w", err)
	}
	return s, nil
}

// Save stores the settings.
func Save(ctx context.Context, q interface {
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
}, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(s)
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: settingsKey, Value: raw}); err != nil {
		return fmt.Errorf("game routing settings: %w", err)
	}
	return nil
}
