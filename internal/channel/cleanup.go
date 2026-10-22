package channel

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// slotCleanupKey marks the one-time repair of what matching did before it
// knew event slots and told ESPN+ from ESPN.
const slotCleanupKey = "cleanup_slot_names_v1"

// CleanupResult says what CleanupSlotNames changed.
type CleanupResult struct {
	Ran      bool     // false when it already ran
	Unlinked int      // streams taken off a channel they only joined by a dropped '+'
	Renamed  []string // "old -> new" channel names
}

// CleanupSlotNames runs once, the first start after matching changed:
// it takes streams like "ESPN+ 2" off channels like "ESPN 2" that they
// joined only because '+' was ignored (not by tvg-id or an admin merge),
// and renames channels named after an event a slot carried when matched
// ("Serie A: Venezia vs Napoli @ Oct 17 8:50 AM :Paramount+ 04") to the
// slot's label ("Paramount+ 04"). Unlinked streams show on the Match page
// again (and auto-match picks them up after the next refresh).
func (s *Service) CleanupSlotNames(ctx context.Context) (CleanupResult, error) {
	var res CleanupResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.SeedSetting(ctx, db.SeedSettingParams{Key: slotCleanupKey, Value: []byte("true")})
		if err != nil || n == 0 {
			return err
		}
		res.Ran = true
		prefixes, err := LoadIgnorePrefixes(ctx, q)
		if err != nil {
			return err
		}
		merges, err := LoadMerges(ctx, q)
		if err != nil {
			return err
		}
		merged := map[string]bool{}
		for _, m := range merges {
			merged[m.A], merged[m.B] = true, true
		}
		links, err := q.ListChannelLinks(ctx)
		if err != nil {
			return err
		}
		for _, l := range links {
			name := StripPrefixes(l.StreamName, prefixes)
			oldKey := normalizeName(name, false)
			if oldKey != normalizeName(l.ChannelName, false) || NormalizeName(name) == NormalizeName(l.ChannelName) ||
				(tvgKey(l.StreamTvgID) != "" && tvgKey(l.StreamTvgID) == tvgKey(l.ChannelTvgID)) ||
				merged["name:"+oldKey] || merged[streamKey(l.ProviderStreamID)] {
				continue
			}
			if err := q.RemoveStreamFromChannel(ctx, db.RemoveStreamFromChannelParams{
				ChannelID: l.ChannelID, ProviderStreamID: l.ProviderStreamID}); err != nil {
				return err
			}
			res.Unlinked++
		}
		chans, err := q.ListChannelMatchKeys(ctx)
		if err != nil {
			return err
		}
		for _, c := range chans {
			label, event, ok := SplitSlot(c.Name)
			if !ok || event == "" {
				continue
			}
			if name := DisplayName(label); name != c.Name {
				if err := q.RenameChannel(ctx, db.RenameChannelParams{ID: c.ID, Name: name}); err != nil {
					return err
				}
				res.Renamed = append(res.Renamed, c.Name+" -> "+name)
			}
		}
		return nil
	})
	if err != nil {
		return CleanupResult{}, fmt.Errorf("slot name cleanup: %w", err)
	}
	return res, nil
}
