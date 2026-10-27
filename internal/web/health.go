package web

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/health"
)

// healthScores holds the provider and backbone health over the window.
type healthScores struct {
	Providers map[int64]health.Score
	Streams   map[int64]health.Score
	Backbones map[string]health.Score
}

// outcomeBuckets is the scoring window's outcomes as of now, per hour.
func (ui *UI) outcomeBuckets(ctx context.Context, now time.Time) ([]health.Bucket, error) {
	rows, err := ui.Store.ListOutcomeHoursSince(ctx, now.Add(-health.Window))
	if err != nil {
		return nil, err
	}
	buckets := make([]health.Bucket, 0, len(rows))
	for _, r := range rows {
		buckets = append(buckets, health.Bucket{Start: r.HourStart, ProviderID: r.ProviderID, StreamID: r.StreamID,
			Backbone: r.Backbone, Outcome: r.Outcome, Count: r.Count})
	}
	return buckets, nil
}

// healthScores scores every provider and backbone as of now.
func (ui *UI) healthScores(ctx context.Context) (healthScores, error) {
	now := time.Now()
	buckets, err := ui.outcomeBuckets(ctx, now)
	if err != nil {
		return healthScores{}, err
	}
	return healthScores{
		Providers: health.ByKey(buckets, func(b health.Bucket) int64 { return b.ProviderID }, now),
		Streams:   health.ByKey(buckets, func(b health.Bucket) int64 { return b.StreamID }, now),
		Backbones: health.ByKey(buckets, func(b health.Bucket) string { return b.Backbone }, now),
	}, nil
}

// healthBadge is a score as the pages show it.
type healthBadge struct {
	health.Score
	Tone string // ok, warn or bad: the pill's colour
}

// badge styles a score: 90 and up is ok, 70 and up warn, anything less bad.
func badge(s health.Score) healthBadge {
	tone := "bad"
	switch {
	case s.Value >= 90:
		tone = "ok"
	case s.Value >= 70:
		tone = "warn"
	}
	return healthBadge{Score: s, Tone: tone}
}

// backboneRow is one line of the backbone health table.
type backboneRow struct {
	Backbone  string
	Providers int64
	Health    healthBadge
}

// backboneRows pairs each current backbone with its health, by label.
func backboneRows(backbones []db.ListBackbonesRow, scores map[string]health.Score) []backboneRow {
	var out []backboneRow
	for _, b := range backbones {
		if b.Backbone == "" {
			continue
		}
		out = append(out, backboneRow{Backbone: b.Backbone, Providers: b.Providers, Health: badge(scores[b.Backbone])})
	}
	slices.SortFunc(out, func(a, b backboneRow) int { return cmp.Compare(a.Backbone, b.Backbone) })
	return out
}
