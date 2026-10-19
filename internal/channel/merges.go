package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// An admin can merge two Match suggestions that automatic matching keeps
// apart (one provider's "ktla.us" tvg-id beside everyone else's
// "ca.KTLA"). A merge links the two suggestions' keys, not their streams:
// a key comes from a stream's tvg-id, name or rule, so the link survives
// provider refreshes and also catches new streams carrying either key.

const mergesKey = "match_merges"

// maxMerges bounds the list, which every match pass reads.
const maxMerges = 1000

// Merge links two proposal keys; the names are for showing it.
type Merge struct {
	A     string `json:"a"`
	B     string `json:"b"`
	AName string `json:"a_name"`
	BName string `json:"b_name"`
}

// Joins reports whether m links keys a and b, in either order.
func (m Merge) Joins(a, b string) bool {
	return (m.A == a && m.B == b) || (m.A == b && m.B == a)
}

// LoadMerges reads the merge list (empty if never saved).
func LoadMerges(ctx context.Context, q SettingsStore) ([]Merge, error) {
	raw, err := q.GetSetting(ctx, mergesKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("match merges: %w", err)
	}
	var m []Merge
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("match merges: %w", err)
	}
	return m, nil
}

// SaveMerges stores the list; the next match uses it.
func SaveMerges(ctx context.Context, q SettingsStore, merges []Merge) error {
	if len(merges) > maxMerges {
		return fmt.Errorf("at most %d merges", maxMerges)
	}
	if merges == nil {
		merges = []Merge{}
	}
	raw, _ := json.Marshal(merges)
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: mergesKey, Value: raw}); err != nil {
		return fmt.Errorf("match merges: %w", err)
	}
	return nil
}
