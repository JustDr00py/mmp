// Package source implements upstream ingestion: refreshing providers
// (M3U and Xtream) into provider_streams, and EPG guides into
// epg_programs.
package source

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/netguard"
	"github.com/JustDr00py/mmp/internal/redact"
	"github.com/JustDr00py/mmp/internal/source/epg"
	"github.com/JustDr00py/mmp/internal/source/m3u"
	"github.com/JustDr00py/mmp/internal/source/xtream"
)

// maxM3USize bounds playlist downloads (100 MiB: big but bounded).
const maxM3USize = 100 << 20

// Guide downloads are bounded twice: as fetched, and after gzip, so a small
// gzip bomb cannot inflate into an endless guide.
const (
	maxEPGSize         = 512 << 20 // 512 MiB as downloaded
	maxEPGInflatedSize = 4 << 30   // 4 GiB once decompressed
)

// errGuideTooLarge: a guide past maxEPGSize or maxEPGInflatedSize.
var errGuideTooLarge = errors.New("guide too large")

// cappedReader fails with errGuideTooLarge once more than n bytes were read,
// instead of pretending the input ended (a cut guide would load as a short
// one).
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	if c.n -= int64(k); c.n < 0 {
		return k, errGuideTooLarge
	}
	return k, err
}

// programChunk is the COPY batch size for EPG inserts.
const programChunk = 1000

// fillerTitles are program titles (lower-cased) that providers put on
// channels they have no schedule for. Stored, they would count as a
// schedule and hide a lower-priority guide that has the real one.
var fillerTitles = map[string]bool{
	"channel no longer available": true,
	"no information":              true,
	"no information available":    true,
	"tv guide is not available":   true,
}

func isFiller(title string) bool {
	return fillerTitles[strings.ToLower(strings.TrimSpace(title))]
}

// Refresher refreshes providers on demand and on a schedule.
type Refresher struct {
	log  *slog.Logger
	pool *pgxpool.Pool
	hc   *http.Client

	// MaxDisablePct is the default refresh disable threshold (0-100); a
	// provider may override it per row. 100 disables the guard.
	MaxDisablePct int

	// Timeout bounds each scheduled refresh of one provider or guide; 0
	// means no bound. On-demand refreshes keep their caller's deadline.
	Timeout time.Duration

	// AfterRefresh, if set, runs after each successful provider refresh
	// (scheduled or on demand), e.g. to auto-match new streams. Set it
	// before Start.
	AfterRefresh func(ctx context.Context, p db.Provider)
}

// NewRefresher builds a Refresher with the disable threshold maxDisablePct.
// The http client carries no global timeout: per-refresh deadlines come from
// the caller's context.
func NewRefresher(log *slog.Logger, pool *pgxpool.Pool, maxDisablePct int) *Refresher {
	return &Refresher{log: log, pool: pool, hc: netguard.Client(), MaxDisablePct: maxDisablePct}
}

// ErrNoStreams means a provider answered with no streams at all; the
// refresh is refused so existing streams stay enabled.
var ErrNoStreams = errors.New("provider returned no streams (existing streams kept)")

// ErrGuideOff: the provider's guide is turned off, so it is not downloaded.
var ErrGuideOff = errors.New("this provider's guide is turned off")

// ErrGuideBusy: the guide is already downloading; this download is skipped.
var ErrGuideBusy = errors.New("this guide is already downloading; try again when it finishes")

// DisableRefusedError means a refresh would soft-disable more of the
// provider's active streams than the configured threshold allows — almost
// always a truncated provider answer (one category, a half-loaded panel).
// Nothing is changed; retry with force=true to apply it anyway.
type DisableRefusedError struct {
	ProviderID, WouldDisable, Active int
	MaxPct                           int
}

func (e *DisableRefusedError) Error() string {
	return fmt.Sprintf("refused: would disable %d of %d active streams (%d%% > %d%% threshold); retry with force", e.WouldDisable, e.Active, e.Pct(), e.MaxPct)
}

// Pct is the share of active streams this refresh would disable.
func (e *DisableRefusedError) Pct() int {
	if e.Active <= 0 {
		return 0
	}
	return e.WouldDisable * 100 / e.Active
}

// Result summarises one provider refresh.
type Result struct {
	Seen     int `json:"seen"`     // entries observed from the provider
	Upserted int `json:"upserted"` // rows inserted or updated
	Disabled int `json:"disabled"` // rows soft-disabled for absence
}

// RefreshProvider pulls the provider's current stream list and reconciles
// provider_streams in one transaction. A refresh that would soft-disable
// more than maxDisablePct (per-provider override, else the Refresher's
// default) of the provider's active streams is refused with a
// *DisableRefusedError and nothing is changed, unless force is set.
func (r *Refresher) RefreshProvider(ctx context.Context, p db.Provider, force bool) (Result, error) {
	r.log.Info("refreshing provider", "provider_id", p.ID, "type", p.Type, "force", force)
	var res Result

	entries, err := r.fetchEntries(ctx, p)
	if err == nil && len(entries) == 0 {
		// An empty list is almost always a provider fault (an error page,
		// an empty body, a panel hiccup), and reconciling against it would
		// soft-disable every stream the provider has. Keep what we have.
		err = ErrNoStreams
	}
	if err != nil {
		r.markFailing(ctx, p.ID, err)
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
			ProviderID: p.ID,
			ExternalID: e.externalID,
			Name:       e.name,
			TvgID:      e.tvgID,
			LogoUrl:    e.logoURL,
			GroupTitle: e.groupTitle,
			StreamUrl:  e.streamURL,
		}); err != nil {
			return res, fmt.Errorf("refresh provider %d: upsert %s: %w", p.ID, e.externalID, err)
		}
		res.Upserted++
		seenIDs = append(seenIDs, e.externalID)
	}

	// Guard against truncated answers before disabling anything: if this
	// list would switch off a large share of the provider's active streams,
	// roll back and keep the current state (unless forced).
	if !force {
		maxPct := r.MaxDisablePct
		if p.RefreshMaxDisablePct != nil {
			maxPct = int(*p.RefreshMaxDisablePct)
		}
		if maxPct < 100 {
			wouldDisable, err := q.CountActiveProviderStreamsNotIn(ctx,
				db.CountActiveProviderStreamsNotInParams{ProviderID: p.ID, ExternalIds: seenIDs})
			if err != nil {
				return res, fmt.Errorf("refresh provider %d: count would-disable: %w", p.ID, err)
			}
			if wouldDisable > 0 {
				active, err := q.CountActiveProviderStreams(ctx, p.ID)
				if err != nil {
					return res, fmt.Errorf("refresh provider %d: count active: %w", p.ID, err)
				}
				refused := &DisableRefusedError{ProviderID: int(p.ID), WouldDisable: int(wouldDisable), Active: int(active), MaxPct: maxPct}
				if refused.Pct() > maxPct {
					return res, fmt.Errorf("refresh provider %d: %w", p.ID, refused)
				}
			}
		}
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
	if r.AfterRefresh != nil {
		r.AfterRefresh(ctx, p)
	}
	return res, nil
}

// healthEvery is how often a provider is checked between refreshes.
const healthEvery = 5 * time.Minute

// healthTimeout bounds one health check.
const healthTimeout = 30 * time.Second

// checkRetryAfter is how long a failed health check waits before asking
// once more. Panels that throttle logins answer a second login soon after
// another with an error (404 here), so one refusal is not "not answering".
var checkRetryAfter = 5 * time.Second

// sameHostGap spaces out health checks of providers on one server, so the
// accounts of one panel don't log in back to back.
var sameHostGap = 3 * time.Second

// CheckProvider asks the provider whether it answers, without fetching its
// stream list: an Xtream login, or the start of an M3U playlist. A failure
// is retried once after checkRetryAfter, unless the login was rejected.
// The result is stored on the provider (see Describe).
func (r *Refresher) CheckProvider(ctx context.Context, p db.Provider) error {
	err := r.checkOnce(ctx, p)
	if err != nil && ctx.Err() == nil && !errors.Is(err, xtream.ErrUnauthorized) {
		if sleepCtx(ctx, checkRetryAfter) == nil {
			err = r.checkOnce(ctx, p)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err() // shutting down: not the provider's fault
	}
	if err != nil {
		r.markFailing(ctx, p.ID, err)
		return err
	}
	if err := db.New(r.pool).MarkProviderHealthy(ctx, p.ID); err != nil {
		r.log.Error("refresher: mark provider healthy", "provider_id", p.ID, "err", err)
	}
	return nil
}

func (r *Refresher) checkOnce(ctx context.Context, p db.Provider) error {
	cctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	return r.check(cctx, p)
}

// sleepCtx waits d, or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// hostPacer spaces out requests to one server by at least gap.
type hostPacer struct {
	gap  time.Duration
	last map[string]time.Time // hostname -> when it was last asked
}

// wait blocks until rawURL's server was last asked gap ago, then notes it
// as asked now. A URL without a host is not paced.
func (h *hostPacer) wait(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if last, ok := h.last[host]; ok {
		if err := sleepCtx(ctx, h.gap-time.Since(last)); err != nil {
			return err
		}
	}
	if h.last == nil {
		h.last = make(map[string]time.Time)
	}
	h.last[host] = time.Now()
	return nil
}

func (r *Refresher) check(ctx context.Context, p db.Provider) error {
	switch p.Type {
	case "xtream":
		client, err := xtream.New(p.Url, p.Username, p.Password, p.UserAgent)
		if err != nil {
			return err
		}
		if _, err := client.Login(ctx); err != nil {
			return fmt.Errorf("login: %w", err)
		}
		return nil
	case "m3u":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Url, nil)
		if err != nil {
			return fmt.Errorf("download playlist %s: invalid url", redact.URL(p.Url))
		}
		req.Header.Set("User-Agent", userAgentOr(p.UserAgent))
		resp, err := r.hc.Do(req)
		if err != nil {
			return fmt.Errorf("download playlist %s: %w", redact.URL(p.Url), redact.Err(err))
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download playlist %s: status %d", redact.URL(p.Url), resp.StatusCode)
		}
		return nil
	default:
		return fmt.Errorf("unknown provider type %q", p.Type)
	}
}

// markFailing stores why the provider did not answer.
func (r *Refresher) markFailing(ctx context.Context, id int64, cause error) {
	msg := redact.Text(Describe(cause))
	if err := db.New(r.pool).MarkProviderFailing(context.WithoutCancel(ctx), db.MarkProviderFailingParams{ID: id, Error: msg}); err != nil {
		r.log.Error("refresher: mark provider failing", "provider_id", id, "err", err)
	}
}

// Describe puts a provider error in words an admin can act on, keeping
// the technical cause after them.
func Describe(err error) string {
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return fmt.Sprintf("Server name %s does not exist (DNS). The provider may have moved to a new address. (%v)", dns.Name, err)
	case errors.As(err, &dns):
		return fmt.Sprintf("Could not look up %s (DNS). (%v)", dns.Name, err)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("The server refused the connection. (%v)", err)
	case errors.Is(err, xtream.ErrUnauthorized):
		return fmt.Sprintf("The panel rejected the login: the account may have expired or the password changed. (%v)", err)
	case errors.Is(err, ErrNoStreams):
		return fmt.Sprintf("The provider answered with no streams. (%v)", err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("The server did not answer in time. (%v)", err)
	}
	return err.Error()
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
		return nil, fmt.Errorf("download playlist %s: invalid url", redact.URL(p.Url)) // err echoes the raw URL
	}
	req.Header.Set("User-Agent", userAgentOr(p.UserAgent))
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download playlist %s: %w", redact.URL(p.Url), redact.Err(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download playlist %s: status %d", redact.URL(p.Url), resp.StatusCode)
	}

	var out []streamEntry
	err = m3u.Parse(io.LimitReader(resp.Body, maxM3USize), func(e m3u.Entry) error {
		if !absHTTP(e.URL) {
			return nil // not http(s): nothing MMP could or should open
		}
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
// epg_url for M3U) and replaces its stored programs. A provider whose guide
// is turned off answers ErrGuideOff.
func (r *Refresher) RefreshEPG(ctx context.Context, p db.Provider) (int, error) {
	if !p.EpgEnabled {
		return 0, ErrGuideOff
	}
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
		Name: p.Name + " EPG", Url: xmltvURL, ProviderID: p.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) { // turned off since p was read
		return 0, ErrGuideOff
	}
	if err != nil {
		return 0, fmt.Errorf("refresh epg provider %d: upsert source: %w", p.ID, err)
	}
	return r.loadGuide(ctx, source.ID, xmltvURL, p.UserAgent, fmt.Sprintf("provider %d", p.ID))
}

// RefreshEPGSource downloads an extra guide source (one not tied to a
// provider) and replaces its stored programs.
func (r *Refresher) RefreshEPGSource(ctx context.Context, src db.EpgSource) (int, error) {
	if src.ProviderID != nil {
		return 0, fmt.Errorf("refresh epg source %d: belongs to a provider", src.ID)
	}
	return r.loadGuide(ctx, src.ID, src.Url, "", fmt.Sprintf("source %d", src.ID))
}

// loadGuide fetches an XMLTV guide (plain or gzipped) and replaces the
// source's programs and channel list in one transaction. label names it in
// errors.
func (r *Refresher) loadGuide(ctx context.Context, sourceID int64, xmltvURL, userAgent, label string) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("refresh epg %s: begin: %w", label, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := db.New(tx)

	// A guide is downloaded on add, on the schedule and on demand: a second
	// download while one runs would insert the same programs and fail.
	locked, err := q.TryLockEpgSource(ctx, int32(sourceID))
	if err != nil {
		return 0, fmt.Errorf("refresh epg %s: lock: %w", label, err)
	}
	if !locked {
		return 0, ErrGuideBusy
	}

	if _, err := q.ClearEpgSourcePrograms(ctx, sourceID); err != nil {
		return 0, fmt.Errorf("refresh epg %s: clear: %w", label, err)
	}
	if err := q.ClearEpgSourceChannels(ctx, sourceID); err != nil {
		return 0, fmt.Errorf("refresh epg %s: clear channels: %w", label, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xmltvURL, nil)
	if err != nil {
		return 0, fmt.Errorf("refresh epg %s: fetch %s: invalid url", label, redact.URL(xmltvURL))
	}
	req.Header.Set("User-Agent", userAgentOr(userAgent))
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("refresh epg %s: fetch %s: %w", label, redact.URL(xmltvURL), redact.Err(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("refresh epg %s: fetch: status %d", label, resp.StatusCode)
	}

	total := 0
	batch := make([]db.InsertEpgProgramsParams, 0, programChunk)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := q.InsertEpgPrograms(ctx, batch); err != nil {
			return fmt.Errorf("insert %d programs: %w", len(batch), err)
		}
		batch = batch[:0]
		return nil
	}
	// Provider guides repeat programs (same channel and start) and carry
	// zero-length ones; either would fail the whole COPY, so skip them.
	type progKey struct {
		channel string
		start   int64
	}
	seen := map[progKey]struct{}{}
	// The guide's channels by lower-cased id (as guide lookups match): its
	// <channel> elements in file order, then ids only its programs name.
	type guideChannel struct {
		id       string
		names    []string
		programs int32
	}
	channels := map[string]*guideChannel{}
	var order []string
	channel := func(id string) *guideChannel {
		k := strings.ToLower(id)
		c, ok := channels[k]
		if !ok {
			c = &guideChannel{id: id, names: []string{}} // NOT NULL column
			channels[k] = c
			order = append(order, k)
		}
		return c
	}
	skipped, fillers := 0, 0
	// Third-party guides are usually .xml.gz: detect gzip by its magic bytes.
	var guide io.Reader = bufio.NewReaderSize(&cappedReader{r: resp.Body, n: maxEPGSize}, 64<<10)
	if head, _ := guide.(*bufio.Reader).Peek(2); len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(guide)
		if err != nil {
			return 0, fmt.Errorf("refresh epg %s: gzip: %w", label, err)
		}
		defer gz.Close()
		guide = &cappedReader{r: gz, n: maxEPGInflatedSize}
	}
	onChannel := func(ec epg.Channel) error {
		c := channel(ec.ID)
		for _, n := range ec.Names {
			if !slices.Contains(c.names, n) {
				c.names = append(c.names, n)
			}
		}
		return nil
	}
	err = epg.ParseGuide(guide, onChannel, func(pr epg.Program) error {
		k := progKey{pr.Channel, pr.Start.UnixNano()}
		if _, dup := seen[k]; dup || !pr.Stop.After(pr.Start) {
			skipped++
			return nil
		}
		seen[k] = struct{}{}
		if isFiller(pr.Title) {
			// The id stays listed, with no programs, so guide lookups
			// fall through to another source.
			channel(pr.Channel)
			fillers++
			return nil
		}
		channel(pr.Channel).programs++
		batch = append(batch, db.InsertEpgProgramsParams{
			EpgSourceID:  sourceID,
			ChannelTvgID: pr.Channel,
			Title:        pr.Title,
			SubTitle:     pr.SubTitle,
			Description:  pr.Desc,
			StartTime:    pr.Start,
			StopTime:     pr.Stop,
		})
		total++
		if len(batch) >= programChunk {
			return flush()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("refresh epg %s: parse: %w", label, err)
	}
	if err := flush(); err != nil {
		return 0, fmt.Errorf("refresh epg %s: %w", label, err)
	}
	if skipped > 0 {
		r.log.Info("epg: skipped duplicate or empty programs", "provider_id", label, "skipped", skipped)
	}
	if fillers > 0 {
		r.log.Info("epg: skipped filler programs", "provider_id", label, "skipped", fillers)
	}
	rows := make([]db.InsertEpgChannelsParams, 0, len(order))
	withPrograms := 0
	for _, k := range order {
		c := channels[k]
		if c.programs > 0 {
			withPrograms++
		}
		rows = append(rows, db.InsertEpgChannelsParams{EpgSourceID: sourceID, TvgID: c.id, DisplayNames: c.names, ProgramCount: c.programs})
	}
	if _, err := q.InsertEpgChannels(ctx, rows); err != nil {
		return 0, fmt.Errorf("refresh epg %s: insert %d channels: %w", label, len(rows), err)
	}
	if err := q.MarkEpgSourceRefreshed(ctx, db.MarkEpgSourceRefreshedParams{
		ID: sourceID, Programs: int64(total), Channels: int64(withPrograms),
	}); err != nil {
		return 0, fmt.Errorf("refresh epg %s: mark: %w", label, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("refresh epg %s: commit: %w", label, err)
	}
	r.log.Info("epg refreshed", "provider_id", label, "programs", total)
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

// bounded runs one scheduled refresh under timeout d (0: no bound), so a
// provider that answers slowly cannot hold up the ones after it.
func bounded[T any](ctx context.Context, d time.Duration, fn func(context.Context) (T, error)) (T, error) {
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	return fn(ctx)
}

func (r *Refresher) refreshDue(ctx context.Context, epgInterval time.Duration) {
	q := db.New(r.pool)
	providers, err := q.ListDueProviders(ctx)
	if err != nil {
		r.log.Error("refresher: list due providers", "err", err)
		return
	}
	for _, p := range providers {
		if _, err := bounded(ctx, r.Timeout, func(ctx context.Context) (Result, error) { return r.RefreshProvider(ctx, p, false) }); err != nil { // scheduled: never force
			r.log.Error("refresher: refresh failed", "provider_id", p.ID, "err", err.Error())
		}
		// The catalogs are independent of the live list: a VOD-only panel
		// (no live channels, so the live refresh fails) still gets its
		// movies and series.
		if p.VodEnabled && p.Type == "xtream" {
			if _, err := bounded(ctx, r.Timeout, func(ctx context.Context) (VODResult, error) { return r.RefreshVOD(ctx, p, false) }); err != nil {
				r.log.Error("refresher: vod refresh failed", "provider_id", p.ID, "err", err.Error())
			}
		}
	}
	// Between refreshes, check that each provider still answers. Those
	// refreshed above were just checked.
	due, err := q.ListProvidersDueForCheck(ctx, healthEvery.Seconds())
	if err != nil {
		r.log.Error("refresher: list providers due for check", "err", err)
	}
	pacer := hostPacer{gap: sameHostGap}
	for _, p := range due {
		if pacer.wait(ctx, p.Url) != nil {
			return
		}
		if err := r.CheckProvider(ctx, p); err != nil && ctx.Err() == nil {
			r.log.Warn("refresher: provider check failed", "provider_id", p.ID, "err", redact.Text(err.Error()))
		}
	}
	epgDue, err := q.ListProvidersDueForEpg(ctx, epgInterval.Seconds())
	if err != nil {
		r.log.Error("refresher: list epg due", "err", err)
		return
	}
	for _, p := range epgDue {
		if _, err := bounded(ctx, r.Timeout, func(ctx context.Context) (int, error) { return r.RefreshEPG(ctx, p) }); err != nil {
			r.log.Error("refresher: epg refresh failed", "provider_id", p.ID, "err", err.Error())
		}
	}
	extra, err := q.ListExtraEpgSourcesDue(ctx, epgInterval.Seconds())
	if err != nil {
		r.log.Error("refresher: list extra guides due", "err", err)
		return
	}
	for _, src := range extra {
		if _, err := bounded(ctx, r.Timeout, func(ctx context.Context) (int, error) { return r.RefreshEPGSource(ctx, src) }); err != nil {
			r.log.Error("refresher: extra guide refresh failed", "epg_source_id", src.ID, "err", err.Error())
		}
	}
}

// userAgentOr returns ua, or the default when the provider sets none.
func userAgentOr(ua string) string {
	if ua == "" {
		return xtream.DefaultUserAgent
	}
	return ua
}
