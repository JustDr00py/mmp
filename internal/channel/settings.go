package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// AutoMatch is the automatic matching configuration as .env/YAML spells
// it. It only seeds the stored MatchSettings on first start; afterwards the
// Settings page is in charge.
type AutoMatch struct {
	Enabled      bool // run after each provider refresh
	MinProviders int  // new channels only for groups seen at >= this many providers
	NewChannels  bool // false: only attach streams to channels that exist
	// Group is where created channels go: "source" for a group named after
	// the provider group, "none" or "" for no group, anything else a group
	// of that name (created when missing).
	Group string
	// Bouquets decides the bouquets of the channels auto-match creates:
	// empty or "auto" for those holding most of the channel's group,
	// "none" for none, otherwise bouquet names (unknown ones are logged and
	// skipped).
	Bouquets []string
}

// Group and bouquet choices of MatchSettings.
const (
	GroupSource = "source" // a group named after the provider group
	GroupNone   = "none"
	GroupFixed  = "fixed" // GroupID

	BouquetsAuto  = "auto" // the bouquets holding most of the channel's group
	BouquetsNone  = "none"
	BouquetsFixed = "fixed" // BouquetIDs
)

// MatchSettings is automatic matching as stored (edited on the Settings
// page). Groups and bouquets are kept by ID, so renaming them is harmless.
type MatchSettings struct {
	Auto         bool    `json:"auto"`
	NewChannels  bool    `json:"new_channels"`
	MinProviders int     `json:"min_providers"`
	Group        string  `json:"group"`
	GroupID      int64   `json:"group_id,omitempty"`
	Bouquets     string  `json:"bouquets"`
	BouquetIDs   []int64 `json:"bouquet_ids,omitempty"`
}

const matchSettingsKey = "match"

// DefaultMatchSettings are the built-in defaults (as config.Defaults).
func DefaultMatchSettings() MatchSettings {
	return MatchSettings{NewChannels: true, MinProviders: 2, Group: GroupSource, Bouquets: BouquetsAuto}
}

// Validate checks settings from the form.
func (m MatchSettings) Validate() error {
	if m.MinProviders < 1 || m.MinProviders > 100 {
		return errors.New("minimum providers must be between 1 and 100")
	}
	switch m.Group {
	case GroupSource, GroupNone:
	case GroupFixed:
		if m.GroupID <= 0 {
			return errors.New("choose a group")
		}
	default:
		return errors.New("choose where new channels go")
	}
	switch m.Bouquets {
	case BouquetsAuto, BouquetsNone, BouquetsFixed:
	default:
		return errors.New("choose the bouquets of new channels")
	}
	return nil
}

// MatchSettings reads the stored settings (the defaults if never saved).
func (s *Service) MatchSettings(ctx context.Context) (MatchSettings, error) {
	m := DefaultMatchSettings()
	raw, err := db.New(s.pool).GetSetting(ctx, matchSettingsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, fmt.Errorf("match settings: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return DefaultMatchSettings(), fmt.Errorf("match settings: %w", err)
	}
	return m, nil
}

// SaveMatchSettings stores the settings; the next refresh uses them.
func (s *Service) SaveMatchSettings(ctx context.Context, m MatchSettings) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Group != GroupFixed {
		m.GroupID = 0
	}
	if m.Bouquets != BouquetsFixed {
		m.BouquetIDs = nil
	}
	raw, _ := json.Marshal(m)
	if err := db.New(s.pool).SetSetting(ctx, db.SetSettingParams{Key: matchSettingsKey, Value: raw}); err != nil {
		return fmt.Errorf("match settings: %w", err)
	}
	return nil
}

// SeedMatchSettings stores am as the starting settings unless settings
// were saved before (then am is ignored). Group and bouquet names are
// resolved here, once, case-insensitively; a missing group is created.
func (s *Service) SeedMatchSettings(ctx context.Context, am AutoMatch) error {
	q := db.New(s.pool)
	if _, err := q.GetSetting(ctx, matchSettingsKey); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("match settings: %w", err)
	}
	m := MatchSettings{Auto: am.Enabled, NewChannels: am.NewChannels, MinProviders: am.MinProviders,
		Group: GroupNone, Bouquets: bouquetMode(am.Bouquets)}
	switch g := strings.TrimSpace(am.Group); strings.ToLower(g) {
	case GroupSource:
		m.Group = GroupSource
	case GroupNone, "":
	default:
		id, err := groupID(ctx, q, g)
		if err != nil {
			return fmt.Errorf("match settings: group %q: %w", g, err)
		}
		m.Group, m.GroupID = GroupFixed, id
	}
	if m.Bouquets == BouquetsFixed {
		ids, err := s.bouquetIDs(ctx, am.Bouquets)
		if err != nil {
			return err
		}
		m.BouquetIDs = ids
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("match settings from the configuration: %w", err)
	}
	raw, _ := json.Marshal(m)
	if _, err := q.SeedSetting(ctx, db.SeedSettingParams{Key: matchSettingsKey, Value: raw}); err != nil {
		return fmt.Errorf("match settings: %w", err)
	}
	return nil
}

// groupID finds the channel group named name (case-insensitively),
// creating it when there is none.
func groupID(ctx context.Context, q *db.Queries, name string) (int64, error) {
	groups, err := q.ListChannelGroups(ctx)
	if err != nil {
		return 0, err
	}
	if i := slices.IndexFunc(groups, func(g db.ChannelGroup) bool { return strings.EqualFold(g.Name, name) }); i >= 0 {
		return groups[i].ID, nil
	}
	cg, err := q.EnsureChannelGroup(ctx, name)
	return cg.ID, err
}

// AutoMatchHook returns a source.Refresher AfterRefresh hook: when the
// stored settings turn automatic matching on, after a provider refresh it
// attaches new streams to matching existing channels and, with
// NewChannels, creates channels for groups seen at >= MinProviders
// providers, in the chosen group and bouquets. Existing channels that only
// gain streams keep their group and bouquets. Settings are read on each
// run, so changes apply without a restart. Failures are logged; the
// refresh itself already succeeded.
func (s *Service) AutoMatchHook() func(context.Context, db.Provider) {
	return func(ctx context.Context, p db.Provider) {
		m, err := s.MatchSettings(ctx)
		if err != nil {
			s.log.Error("auto-match failed", "provider_id", p.ID, "err", err)
			return
		}
		if !m.Auto {
			return
		}
		opts, err := s.autoMatchOptions(ctx, m)
		if err != nil {
			s.log.Error("auto-match failed", "provider_id", p.ID, "err", err)
			return
		}
		res, err := s.Apply(ctx, opts)
		if err != nil {
			s.log.Error("auto-match failed", "provider_id", p.ID, "err", err)
			return
		}
		s.log.Info("auto-match", "provider_id", p.ID, "created", res.Created, "attached", res.Attached)
	}
}

// autoMatchOptions turns settings into an Apply. A chosen group or bouquet
// deleted since is skipped with a warning.
func (s *Service) autoMatchOptions(ctx context.Context, m MatchSettings) (ApplyOptions, error) {
	opts := ApplyOptions{MinProviders: m.MinProviders, AttachAlways: true, ExistingOnly: !m.NewChannels}
	if !m.NewChannels {
		return opts, nil
	}
	q := db.New(s.pool)
	switch m.Group {
	case GroupSource:
		opts.GroupFromSource = true
	case GroupFixed:
		if _, err := q.GetChannelGroup(ctx, m.GroupID); errors.Is(err, pgx.ErrNoRows) {
			s.log.Warn("auto-match: the group for new channels was deleted; choose another on the Settings page")
		} else if err != nil {
			return opts, fmt.Errorf("group: %w", err)
		} else {
			opts.ChannelGroupID = &m.GroupID
		}
	}
	switch m.Bouquets {
	case BouquetsAuto:
		opts.NewChannelBouquetsFromGroup = true
	case BouquetsFixed:
		all, err := q.ListBouquets(ctx)
		if err != nil {
			return opts, fmt.Errorf("list bouquets: %w", err)
		}
		for _, id := range m.BouquetIDs {
			if slices.ContainsFunc(all, func(b db.Bouquet) bool { return b.ID == id }) {
				opts.NewChannelBouquetIDs = append(opts.NewChannelBouquetIDs, id)
			}
		}
	}
	return opts, nil
}
