package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// GroupIconSettings say whether Xtream categories carry their group's icon
// (category_icon), edited on the Settings page. Off by default: the field
// is MMP's own, so it is opt-in in case some player rejects it.
type GroupIconSettings struct {
	Enabled bool `json:"enabled"`
}

const groupIconsKey = "xtream_group_icons"

// LoadGroupIcons reads the settings (off if never saved).
func LoadGroupIcons(ctx context.Context, q interface {
	GetSetting(ctx context.Context, key string) ([]byte, error)
}) (GroupIconSettings, error) {
	var s GroupIconSettings
	raw, err := q.GetSetting(ctx, groupIconsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("group icon settings: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return GroupIconSettings{}, fmt.Errorf("group icon settings: %w", err)
	}
	return s, nil
}

// SaveGroupIcons stores the settings.
func SaveGroupIcons(ctx context.Context, q interface {
	SetSetting(ctx context.Context, arg db.SetSettingParams) error
}, s GroupIconSettings) error {
	raw, _ := json.Marshal(s)
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: groupIconsKey, Value: raw}); err != nil {
		return fmt.Errorf("group icon settings: %w", err)
	}
	return nil
}
