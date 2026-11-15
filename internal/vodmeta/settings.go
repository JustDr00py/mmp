package vodmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// Settings are the VOD library's, edited on the Settings page.
type Settings struct {
	// TMDBKey is a TMDB API key (v3) or read access token (v4); empty
	// turns TMDB lookups off.
	TMDBKey string `json:"tmdb_key"`
	// Match searches TMDB for titles panels gave no id, after VOD
	// refreshes.
	Match bool `json:"match"`
	// CleanTitles shows customers "Title (Year)" instead of panel names.
	CleanTitles bool `json:"clean_titles"`
}

const settingsKey = "vod_library"

// Matching reports whether TMDB searches run.
func (s Settings) Matching() bool { return s.Match && s.TMDBKey != "" }

// Normalize trims what a form sent.
func (s *Settings) Normalize() { s.TMDBKey = strings.TrimSpace(s.TMDBKey) }

// LoadSettings reads the settings (all off if never saved).
func LoadSettings(ctx context.Context, q interface {
	GetSetting(ctx context.Context, key string) ([]byte, error)
}) (Settings, error) {
	var s Settings
	raw, err := q.GetSetting(ctx, settingsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("vod library settings: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("vod library settings: %w", err)
	}
	return s, nil
}

// SaveSettings stores the settings.
func SaveSettings(ctx context.Context, q interface {
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
}, s Settings) error {
	s.Normalize()
	raw, _ := json.Marshal(s)
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: settingsKey, Value: raw}); err != nil {
		return fmt.Errorf("vod library settings: %w", err)
	}
	return nil
}
