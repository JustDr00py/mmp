package nameguide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// Settings say which channels get a guide from their stream name, edited
// on the Settings page.
type Settings struct {
	Enabled bool `json:"enabled"`
	// All also gives channels whose names say nothing about an event an
	// all-day entry with their name, so no channel shows an empty guide.
	All bool `json:"all"`
	// Zone is the IANA time zone of times a name gives without one.
	Zone string `json:"zone"`
}

const settingsKey = "name_guide"

// DefaultSettings: on for every channel without a guide, zoneless times
// in UTC.
func DefaultSettings() Settings { return Settings{Enabled: true, All: true, Zone: "UTC"} }

// Location is the zone of zoneless times (UTC if unknown).
func (s Settings) Location() *time.Location {
	if loc, err := time.LoadLocation(s.Zone); err == nil {
		return loc
	}
	return time.UTC
}

// Validate checks settings from the form.
func (s Settings) Validate() error {
	if _, err := time.LoadLocation(s.Zone); err != nil || s.Zone == "" || s.Zone == "Local" {
		return fmt.Errorf("unknown time zone %q: use a name like UTC, Europe/London or America/New_York", s.Zone)
	}
	return nil
}

// Store reads and writes settings (satisfied by *db.Queries).
type Store interface {
	GetSetting(ctx context.Context, key string) ([]byte, error)
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
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
		return s, fmt.Errorf("stream-name guide settings: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return DefaultSettings(), fmt.Errorf("stream-name guide settings: %w", err)
	}
	return s, nil
}

// Save stores the settings.
func Save(ctx context.Context, q Store, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(s)
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: settingsKey, Value: raw}); err != nil {
		return fmt.Errorf("stream-name guide settings: %w", err)
	}
	return nil
}

// For is the event a channel's guide is built from under s: its stream
// name when that is in the "a | b" style or names an event, else (with
// All) the event slot title it has ("label : title") or the channel's own
// name. ok is false when the channel gets none.
func (s Settings) For(channelName, streamName string, now time.Time) (Event, bool) {
	if !s.Enabled {
		return Event{}, false
	}
	loc := s.Location()
	if streamName != "" {
		if ev := Parse(streamName, now, loc); ev.Title != "" && (ev.Piped || ev.Start != nil) {
			return ev, true
		}
	}
	if !s.All {
		return Event{}, false
	}
	name := channelName
	if _, _, ok := splitLabel(streamName); ok { // an event slot's current title, not a name it had once
		name = streamName
	}
	ev := Parse(name, now, loc)
	return ev, ev.Title != ""
}
