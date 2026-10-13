// Package source implements upstream ingestion: refreshing providers
// (M3U and Xtream) into provider_streams, and EPG guides into
// epg_programmes.
package source

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/redact"
	"github.com/sysadmin/mmp/internal/source/epg"
	"github.com/sysadmin/mmp/internal/source/m3u"
	"github.com/sysadmin/mmp/internal/source/xtream"
)

// maxM3USize bounds playlist downloads (100 MiB: big but bounded).
const maxM3USize = 100 << 20

// programmeChunk is the COPY batch size for EPG inserts.
const programmeChunk = 1000

// Refresher refreshes providers on demand and on a schedule.
type Refresher struct {
	log  *slog.Logger
	pool *pgxpool.Pool
	hc   *http.Client
}

// NewRefresher builds a Refresher. The http client carries no global timeout:
// per-refresh deadlines come from the caller's context.
func NewRefresher(log *slog.Logger, pool *pgxpool.Pool) *Refresher {
	return &Refresher{log: log, pool: pool, hc: &http.Client{}}
}

// Result summarises one provider refresh.
type Result struct {
	Seen     int `json:"seen"`     // entries observed from the provider
	Upserted int `json:"upserted"` // rows inserted or updated
	Disabled int `json:"disabled"` // rows soft-disabled for absence
}

// RefreshProvider pulls the provider's current stream list and reconciles
// provider_streams in one transaction.
func (r *Refresher) RefreshProvider(ctx context.Context, p db.Provider) (Result, error) {
	r.log.Info("refreshing provider", "provider_id", p.ID, "type", p.Type)
	var res Result

	entries, err := r.fetchEntries(ctx, p)
	if err != nil {
		return res, fmt.Errorf("refresh provider %d: %w", p.ID, err)
	}
	res.Seen = len(entries)

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("refresh provider %d: begin: %w", p.ID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	q := db.New(tx)

	seenIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		if _, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{
			ProviderID:  p.ID,
			ExternalID:  e.externalID,
			Name:        e.name,
			TvgID:       e.tvgID,
			LogoUrl:     e.logoURL,
			GroupTitle:  e.groupTitle,
			StreamUrl:   e.streamURL,
		}); err != nil {
			return res, fmt.Errorf("refresh provider %d: upsert %s: %w", p.ID, e.externalID, err)
		}
		res.Upserted++
		seenIDs = append(seenIDs, e.externalID)
	}
	disabled, err := q.SoftDisableMissing(ctx, db.SoftDisableMissingParams{
		ProviderID: p.ID, ExternalIds: seenIDs,
	})
	if err != nil {
		return res, fmt.Errorf("refresh provider %d: soft-disable: %w", p.ID, err)
	}
	res.Disabled = int(disabled)
	if err := q.MarkProviderRefreshed(ctx, p.ID); err != nil {
		return res, fmt.Errorf("refresh provider %d: mark refreshed: %w", p.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("refresh provider %d: commit: %w", p.ID, err)
	}

	r.log.Info("provider refreshed",
		"provider_id", p.ID, "seen", res.Seen, "upserted", res.Upserted, "disabled", res.Disabled)
	return res, nil
}

// streamEntry is the provider-agnostic shape both source types map into.
type streamEntry struct {
	externalID, name, tvgID, logoURL, groupTitle, streamURL string
}

// fetchEntries downloads and normalises the provider's live stream list.
func (r *Refresher) fetchEntries(ctx context.Context, p db.Provider) ([]streamEntry, error) {
	switch p.Type {
	case "xtream":
		return r.fetchXtream(ctx, p)
	case "m3u":
		return r.fetchM3U(ctx, p)
	default:
		return nil, fmt.Errorf("unknown provider type %q", p.Type)
	}
}

func (r *Refresher) fetchXtream(ctx context.Context, p db.Provider) ([]streamEntry, error) {
	client, err := xtream.New(p.Url, p.Username, p.Password, p.UserAgent)
	if err != nil {
		return nil, err
	}
	if _, err := client.Login(ctx); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	cats, err := client.LiveCategories(ctx)
	if err != nil {
		return nil, fmt.Errorf("get_live_categories: %w", err)
	}
	catNames := make(map[string]string, len(cats))
	for _, c := range cats {
		catNames[c.CategoryID] = c.CategoryName
	}
	streams, err := client.LiveStreams(ctx)
	if err != nil {
		return nil, fmt.Errorf("get_live_streams: %w", err)
	}
	out := make([]streamEntry, 0, len(streams))
	for _, s := range streams {
		if s.StreamType != "" && s.StreamType != "live" {
			continue // skip movie/series leaks
		}
		out = append(out, streamEntry{
			externalID: strconv.FormatInt(s.StreamID, 10),
			name:       s.Name,
			tvgID:      s.EPGChannel,
			logoURL:    s.StreamIcon,
			groupTitle: catNames[s.CategoryID],
			streamURL:  client.StreamURL(s.StreamID),
		})
	}
	return out, nil
}

func (r *Refresher) fetchM3U(ctx context.Context, p db.Provider) ([]streamEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Url, nil)
	if err != nil {
		return nil, err
	}
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download playlist %s: %w", redact.URL(p.Url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download playlist %s: status %d", redact.URL(p.Url), resp.StatusCode)
	}

	var out []streamEntry
	err = m3u.Parse(io.LimitReader(resp.Body, maxM3USize), func(e m3u.Entry) error {
		out = append(out, streamEntry{
			externalID: e.URL, // M3U has no ids: the URL is the stable key
			name:       e.Name,
			tvgID:      e.TVGID,
			logoURL:    e.Logo,
			groupTitle: e.Group,
			streamURL:  e.URL,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse playlist %s: %w", redact.URL(p.Url), err)
	}
	return out, nil
}

// RefreshEPG fetches the provider's XMLTV guide (panel-provided for Xtream,
// epg_url for M3U) and replaces its stored programmes.
func (r *Refresher) RefreshEPG(ctx context.Context, p db.Provider) (int, error) {
	xmltvURL := p.EpgUrl
	if p.Type == "xtream" && xmltvURL == "" {
		client, err := xtream.New(p.Url, p.Username, p.Password, p.UserAgent)
		if err != nil {
			return 0, err
		}
		xmltvURL = client.XMLTVURL()
	}
	if xmltvURL == "" {
		return 0, fmt.Errorf("refresh epg provider %d: no xmltv url", p.ID)
	}

	source, err := db.New(r.pool).UpsertEpgSource(ctx, db.UpsertEpgSourceParams{
		Name: p.Name + " EPG", Url: xmltvURL, ProviderID: &p.ID,
	})
	if err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: upsert source: %w", p.ID, err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: begin: %w", p.ID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := db.New(tx)

	if _, err := q.ClearEpgSourceProgrammes(ctx, source.ID); err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: clear: %w", p.ID, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xmltvURL, nil)
	if err != nil {
		return 0, err
	}
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: fetch %s: %w", p.ID, redact.URL(xmltvURL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("refresh epg provider %d: fetch: status %d", p.ID, resp.StatusCode)
	}

	total := 0
	batch := make([]db.InsertEpgProgrammesParams, 0, programmeChunk)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := q.InsertEpgProgrammes(ctx, batch); err != nil {
			return fmt.Errorf("insert %d programmes: %w", len(batch), err)
		}
		batch = batch[:0]
		return nil
	}
	err = epg.Parse(resp.Body, func(pr epg.Programme) error {
		batch = append(batch, db.InsertEpgProgrammesParams{
			EpgSourceID:  source.ID,
			ChannelTvgID: pr.Channel,
			Title:        pr.Title,
			SubTitle:     pr.SubTitle,
			Description:  pr.Desc,
			StartTime:    pr.Start,
			StopTime:     pr.Stop,
		})
		total++
		if len(batch) >= programmeChunk {
			return flush()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: parse: %w", p.ID, err)
	}
	if err := flush(); err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: %w", p.ID, err)
	}
	if err := q.MarkEpgSourceRefreshed(ctx, source.ID); err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: mark: %w", p.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: commit: %w", p.ID, err)
	}
	r.log.Info("epg refreshed", "provider_id", p.ID, "programmes", total)
	return total, nil
}

// Start runs the refresh loop until ctx is cancelled: every tick it
// refreshes every enabled provider whose interval elapsed. EPG follows the
// stream refresh at the coarser epgInterval cadence.
func (r *Refresher) Start(ctx context.Context, tick time.Duration, epgInterval time.Duration) error {
	r.log.Info("refresher started", "tick", tick.String(), "epg_interval", epgInterval.String())
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.refreshDue(ctx, epgInterval)
		}
	}
}

func (r *Refresher) refreshDue(ctx context.Context, epgInterval time.Duration) {
	q := db.New(r.pool)
	providers, err := q.ListDueProviders(ctx)
	if err != nil {
		r.log.Error("refresher: list due providers", "err", err)
		return
	}
	for _, p := range providers {
		if _, err := r.RefreshProvider(ctx, p); err != nil {
			r.log.Error("refresher: refresh failed", "provider_id", p.ID, "err", err.Error())
		}
	}
	epgDue, err := q.ListProvidersDueForEpg(ctx, epgInterval.Seconds())
	if err != nil {
		r.log.Error("refresher: list epg due", "err", err)
		return
	}
	for _, p := range epgDue {
		if _, err := r.RefreshEPG(ctx, p); err != nil {
			r.log.Error("refresher: epg refresh failed", "provider_id", p.ID, "err", err.Error())
		}
	}
}
