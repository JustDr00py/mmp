package output

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/activity"
	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/netguard"
	"github.com/JustDr00py/mmp/internal/proxy"
	"github.com/JustDr00py/mmp/internal/redact"
	"github.com/JustDr00py/mmp/internal/source"
	"github.com/JustDr00py/mmp/internal/source/xtream"
)

// vodWriteTimeout bounds one write to a VOD client (a paused player that
// stops reading keeps the provider slot only this long).
const vodWriteTimeout = 60 * time.Second

// mountVOD registers movie and episode playback (Xtream and token forms).
func (h *Handlers) mountVOD(r chi.Router) {
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		r.Method(m, "/movie/{user}/{pass}/{id}", http.HandlerFunc(h.vodXtream("movie")))
		r.Method(m, "/series/{user}/{pass}/{id}", http.HandlerFunc(h.vodXtream("series")))
		r.Method(m, "/t/{token}/movie/{id}", http.HandlerFunc(h.vodToken("movie")))
		r.Method(m, "/t/{token}/series/{id}", http.HandlerFunc(h.vodToken("series")))
	}
}

func (h *Handlers) vodXtream(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := h.authPassword(r, chi.URLParam(r, "user"), chi.URLParam(r, "pass"))
		if err != nil {
			h.authFail(w, err)
			return
		}
		h.serveVODFile(w, r, u, kind)
	}
}

func (h *Handlers) vodToken(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := h.authToken(r, chi.URLParam(r, "token"))
		if err != nil {
			h.authFail(w, err)
			return
		}
		h.serveVODFile(w, r, u, kind)
	}
}

// vodID parses "{id}" or "{id}.{ext}"; the extension is ignored (the
// stored container decides what is fetched).
func vodID(s string) (int64, bool) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// vodTarget is what one playback fetches.
type vodTarget struct {
	providerID int64
	externalID string
	ext        string
	title      string // for the dashboard
}

func (h *Handlers) vodTarget(ctx context.Context, u db.User, kind string, id int64) (vodTarget, error) {
	admin := u.Role == auth.RoleAdmin
	if kind == "movie" {
		m, err := h.VOD.GetUserVODItem(ctx, db.GetUserVODItemParams{ID: id, UserID: u.ID, IsAdmin: admin})
		return vodTarget{m.ProviderID, m.ExternalID, m.ContainerExtension, m.Name}, err
	}
	e, err := h.VOD.GetUserEpisode(ctx, db.GetUserEpisodeParams{ID: id, UserID: u.ID, IsAdmin: admin})
	return vodTarget{e.ProviderID, e.ExternalID, e.ContainerExtension, episodeTitle(e)}, err
}

// episodeTitle is "Series · S01E02 · Title" (parts it lacks left out).
func episodeTitle(e db.GetUserEpisodeRow) string {
	parts := []string{}
	if e.SeriesName != "" {
		parts = append(parts, e.SeriesName)
	}
	if e.Season > 0 || e.EpisodeNum > 0 {
		parts = append(parts, fmt.Sprintf("S%02dE%02d", e.Season, e.EpisodeNum))
	}
	if e.Title != "" && e.Title != e.SeriesName {
		parts = append(parts, e.Title)
	}
	return strings.Join(parts, " · ")
}

var vodContentTypes = map[string]string{
	"mp4": "video/mp4", "m4v": "video/x-m4v", "mkv": "video/x-matroska", "webm": "video/webm",
	"avi": "video/x-msvideo", "ts": "video/mp2t", "mov": "video/quicktime", "flv": "video/x-flv",
}

// passHeaders are the upstream response headers a player needs for
// seeking; anything else (Location, cookies, server banners) stays behind.
var passHeaders = []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"}

// serveVODFile proxies one movie or episode: a dedicated upstream request
// per viewer with Range passed through, so players can seek. The viewer
// holds one of the user's connection slots and one provider slot until the
// response ends.
func (h *Handlers) serveVODFile(w http.ResponseWriter, r *http.Request, u db.User, kind string) {
	if h.VOD == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := vodID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	tgt, err := h.vodTarget(ctx, u, kind, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.Log.Error("output: vod lookup", "user_id", u.ID, "kind", kind, "id", id, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	p, err := h.VOD.GetProvider(ctx, tgt.providerID)
	if err != nil {
		h.Log.Error("output: vod provider", "provider_id", tgt.providerID, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	addr := auth.RemoteIP(r)
	ctx, lease, release, err := h.vodLeases.acquire(ctx, vodAcquire{
		key: vodKey{u.ID, kind, id}, providerID: p.ID,
		take: func() (*proxy.Slot, *proxy.Slot, error) {
			userSlot, err := h.Access.Acquire(u)
			if err != nil {
				return nil, nil, err
			}
			if h.Slots == nil {
				return userSlot, nil, nil
			}
			provSlot, ok := h.Slots.TryAcquire(p.ID, int(p.MaxConnections))
			if !ok {
				userSlot.Release()
				return nil, nil, errProviderFull
			}
			return userSlot, provSlot, nil
		},
		register: func(ls *vodLease, kick func()) func() {
			_, remove := h.Access.viewers.add(Viewer{UserID: u.ID, Username: u.Username, Addr: addr, Output: kind,
				Title: tgt.title, ItemID: id, ProviderID: p.ID, Since: ls.since}, kick, &ls.bytes)
			return remove
		},
		tookOver: func(old *vodLease) {
			h.Log.Info("vod playback taken over", "user_id", u.ID, "username", u.Username, "old_kind", old.key.kind,
				"old_id", old.key.id, "kind", kind, "id", id)
		},
		ended: func(ls *vodLease, reason string, at time.Time) {
			h.vodSessionEnded(u, addr, tgt, p.ID, ls, reason, at)
		},
	})
	switch {
	case errors.Is(err, errProviderFull):
		h.Log.Info("output: vod provider busy", "user_id", u.ID, "username", u.Username, "provider_id", p.ID,
			"max", p.MaxConnections, "kind", kind, "id", id)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "provider busy, try again shortly", http.StatusServiceUnavailable)
		return
	case err != nil:
		h.Log.Info("output: connection limit", "user_id", u.ID, "username", u.Username, "kind", kind, "id", id,
			"max", u.MaxConnections)
		http.Error(w, "connection limit reached", http.StatusForbidden)
		return
	}
	defer release()

	client, err := xtream.New(source.ProviderURL(p), p.Username, p.Password, p.UserAgent)
	if err != nil {
		h.Log.Error("output: vod provider url", "provider_id", p.ID, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	upURL := client.MovieURL(tgt.externalID, tgt.ext)
	if kind == "series" {
		upURL = client.EpisodeURL(tgt.externalID, tgt.ext)
	}
	method := http.MethodGet
	if r.Method == http.MethodHead {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(ctx, method, upURL, nil)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, k := range []string{"Range", "If-Range"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	ua := p.UserAgent
	if ua == "" {
		ua = proxy.DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)

	resp, err := h.vodClient().Do(req) // redirects are followed here, never shown to the player
	if err != nil {
		if ctx.Err() == nil {
			h.Log.Warn("output: vod upstream", "provider_id", p.ID, "kind", kind, "id", id, "err", redact.Err(err))
			h.vodLeases.failed(lease, redact.Err(err).Error())
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent,
		resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
	case resp.StatusCode == http.StatusNotFound:
		h.Log.Warn("output: vod upstream not found", "provider_id", p.ID, "kind", kind, "id", id)
		h.vodLeases.failed(lease, "upstream status 404")
		http.NotFound(w, r)
		return
	default:
		h.Log.Warn("output: vod upstream status", "provider_id", p.ID, "kind", kind, "id", id, "status", resp.StatusCode)
		h.vodLeases.failed(lease, "upstream status "+strconv.Itoa(resp.StatusCode))
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	for _, k := range passHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	ct := vodContentTypes[strings.ToLower(tgt.ext)]
	if ct == "" {
		ct = resp.Header.Get("Content-Type")
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if method == http.MethodHead {
		return
	}

	h.Log.Debug("vod request started", "user_id", u.ID, "kind", kind, "id", id, "range", r.Header.Get("Range"),
		"status", resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 128*1024)
	var sent int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(vodWriteTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				break // the player left (or seeks with a new request)
			}
			sent += int64(n)
			lease.bytes.Add(int64(n))
			if lease.started.CompareAndSwap(false, true) {
				h.Log.Info("vod session started", "user_id", u.ID, "username", u.Username, "ip", addr, "kind", kind,
					"id", id, "title", tgt.title, "provider_id", p.ID)
			}
		}
		if rerr != nil {
			break
		}
	}
	h.Log.Debug("vod request ended", "user_id", u.ID, "kind", kind, "id", id, "bytes", sent)
}

// vodSessionEnded logs a finished playback and adds it to the user's watch
// history. A lease that sent nothing and failed nothing (a HEAD probe, a
// player that gave up at once) is not a session worth keeping.
func (h *Handlers) vodSessionEnded(u db.User, addr string, tgt vodTarget, providerID int64, ls *vodLease, reason string, at time.Time) {
	bytes := ls.bytes.Load()
	args := []any{"user_id", u.ID, "username", u.Username, "ip", addr, "kind", ls.key.kind, "id", ls.key.id,
		"title", tgt.title, "provider_id", providerID, "reason", reason,
		"duration", at.Sub(ls.since).Round(time.Second).String(), "bytes", bytes, "requests", ls.requests}
	if ls.lastErr != "" {
		args = append(args, "err", ls.lastErr)
	}
	if bytes == 0 && ls.lastErr == "" {
		h.Log.Debug("vod session ended", args...)
		return
	}
	h.Log.Info("vod session ended", args...)
	h.Activity.WatchedVOD(activity.VODSession{UserID: u.ID, IP: addr, Kind: ls.key.kind, ItemID: ls.key.id,
		Title: tgt.title, Bytes: bytes, EndReason: reason, Start: ls.since, End: at})
}

// End reasons, in logs and the watch history.
const (
	vodStopped   = "stopped"        // the player closed its last request
	vodKicked    = "kicked"         // disconnected from the dashboard
	vodTakenOver = "taken over"     // ended to make room for the user's next title
	vodFailed    = "upstream error" // the provider never delivered
)

var errProviderFull = errors.New("provider has no free connection")

// vodClient is the upstream client: no overall timeout (films are long),
// but a bound on connecting and on response headers.
func (h *Handlers) vodClient() *http.Client {
	if h.VODClient != nil {
		return h.VODClient
	}
	return defaultVODClient
}

var defaultVODClient = &http.Client{
	Transport: vodTransport(),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// vodTransport dials through netguard: a provider picks the film URLs.
func vodTransport() *http.Transport {
	t := netguard.Transport()
	t.ResponseHeaderTimeout = 15 * time.Second
	t.MaxIdleConnsPerHost = 4
	return t
}

// vodLease is the slot pair one user's playback of one item holds. Players
// seek by opening a new ranged request (often before closing the old one),
// so every concurrent request for the same (user, item) shares the lease:
// seeking never counts as a second connection. The slots are released as
// soon as the last request ends (a later seek simply re-acquires them).
//
// Starting another item when the user (or the item's provider) has no free
// slot takes over: the user's oldest playback that frees the needed slot is
// ended (players move on to the next title without closing the last one).
type vodLease struct {
	key                vodKey
	providerID         int64
	since              time.Time
	userSlot, provSlot *proxy.Slot
	refs               int
	nextReq            uint64
	cancels            map[uint64]context.CancelFunc // the requests sharing the lease
	ended              bool                          // kicked or taken over: slots already released
	removeViewer       func()                        // its dashboard entry
	bytes              atomic.Int64                  // sent to the player, all requests
	started            atomic.Bool                   // the first byte reached the player
	requests           int                           // requests served (a seek is one more)
	lastErr            string                        // the last upstream failure, if any
	onEnd              func(ls *vodLease, reason string, at time.Time)
}

type vodKey struct {
	userID int64
	kind   string
	id     int64
}

// vodResume is how long a stopped playback stays open for its report:
// reopening the same title within it (a seek that closed the old request
// first, a reconnect, resuming after a pause) continues the session rather
// than starting a new one in the logs and the watch history.
const vodResume = time.Minute

// vodLeases tracks active playback leases; the zero value is ready.
type vodLeases struct {
	mu sync.Mutex
	m  map[vodKey]*vodLease
	// stopped holds playbacks whose last request ended, unreported until
	// resume passes; slots are already free.
	stopped map[vodKey]*vodPause
	resume  time.Duration // 0: vodResume
	closing bool          // flushed for shutdown: report at once
}

// vodPause is a stopped playback awaiting its report.
type vodPause struct {
	ls    *vodLease
	at    time.Time // when its last request ended
	timer *time.Timer
}

// vodAcquire describes one playback request to vodLeases.acquire.
type vodAcquire struct {
	key        vodKey
	providerID int64
	// take reserves the user's and the provider's slot for a new lease.
	take func() (user, prov *proxy.Slot, err error)
	// register lists a new lease on the dashboard; kick must end it. It
	// returns the entry's remove. Optional.
	register func(ls *vodLease, kick func()) (remove func())
	// tookOver reports a lease ended to make room. Optional.
	tookOver func(old *vodLease)
	// ended reports a new lease's end, with why (vodStopped, ...).
	// It runs with the leases locked, so it must not call back into them.
	// Optional.
	ended func(ls *vodLease, reason string, at time.Time)
}

// acquire joins the lease for a.key or creates one, taking over the
// user's older playbacks when take finds no free slot. The returned context
// is ctx, cancelled if the lease is kicked or taken over.
func (l *vodLeases) acquire(ctx context.Context, a vodAcquire) (_ context.Context, _ *vodLease, release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[vodKey]*vodLease{}
	}
	ls, ok := l.m[a.key]
	if !ok {
		var u, p *proxy.Slot
		for {
			if u, p, err = a.take(); err == nil {
				break
			}
			var old *vodLease
			switch {
			case errors.Is(err, ErrTooManyConnections):
				old = l.oldestLocked(a.key.userID, 0)
			case errors.Is(err, errProviderFull):
				old = l.oldestLocked(a.key.userID, a.providerID)
			}
			if old == nil {
				return nil, nil, nil, err
			}
			l.endLocked(old, vodTakenOver)
			if a.tookOver != nil {
				a.tookOver(old)
			}
		}
		ls = &vodLease{key: a.key, providerID: a.providerID, since: time.Now(), userSlot: u, provSlot: p,
			cancels: map[uint64]context.CancelFunc{}, onEnd: a.ended}
		if st := l.stopped[a.key]; st != nil { // resumed: the same session
			st.timer.Stop()
			delete(l.stopped, a.key)
			ls.since, ls.requests, ls.lastErr = st.ls.since, st.ls.requests, st.ls.lastErr
			ls.bytes.Store(st.ls.bytes.Load())
			ls.started.Store(st.ls.started.Load())
		}
		l.m[a.key] = ls
		if a.register != nil {
			ls.removeViewer = a.register(ls, func() { l.kick(ls) })
		}
	}
	ls.refs++
	ls.requests++
	ls.nextReq++
	req := ls.nextReq
	ctx, cancel := context.WithCancel(ctx)
	ls.cancels[req] = cancel
	var once sync.Once
	return ctx, ls, func() { once.Do(func() { cancel(); l.release(ls, req) }) }, nil
}

// oldestLocked is userID's oldest lease, on providerID unless that is 0.
func (l *vodLeases) oldestLocked(userID, providerID int64) *vodLease {
	var old *vodLease
	for _, ls := range l.m {
		if ls.key.userID != userID || (providerID != 0 && ls.providerID != providerID) {
			continue
		}
		if old == nil || ls.since.Before(old.since) {
			old = ls
		}
	}
	return old
}

// endLocked ends ls now: its slots are free at once and its requests are
// cancelled (they finish unwinding on their own). It is reported at once.
func (l *vodLeases) endLocked(ls *vodLease, reason string) {
	if l.closeLocked(ls) && ls.onEnd != nil {
		ls.onEnd(ls, reason, time.Now())
	}
}

// closeLocked frees ls's slots, cancels its requests and drops it from the
// dashboard; false if it had already ended.
func (l *vodLeases) closeLocked(ls *vodLease) bool {
	if ls.ended {
		return false
	}
	ls.ended = true
	if l.m[ls.key] == ls {
		delete(l.m, ls.key)
	}
	ls.userSlot.Release()
	if ls.provSlot != nil {
		ls.provSlot.Release()
	}
	for _, cancel := range ls.cancels {
		cancel()
	}
	if ls.removeViewer != nil {
		ls.removeViewer()
	}
	return true
}

// kick ends ls (the dashboard's disconnect).
func (l *vodLeases) kick(ls *vodLease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.endLocked(ls, vodKicked)
}

// failed notes an upstream failure on ls, for its end report.
func (l *vodLeases) failed(ls *vodLease, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ls.lastErr = msg
}

func (l *vodLeases) release(ls *vodLease, req uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(ls.cancels, req)
	if ls.refs--; ls.refs > 0 {
		return
	}
	if ls.bytes.Load() == 0 && ls.lastErr != "" {
		l.endLocked(ls, vodFailed)
		return
	}
	if !l.closeLocked(ls) || ls.onEnd == nil {
		return
	}
	at := time.Now()
	if l.closing {
		ls.onEnd(ls, vodStopped, at)
		return
	}
	// Hold the report: a reopen within resume continues this session.
	if l.stopped == nil {
		l.stopped = map[vodKey]*vodPause{}
	}
	if prev := l.stopped[ls.key]; prev != nil { // cannot happen: acquire takes it
		prev.timer.Stop()
		prev.ls.onEnd(prev.ls, vodStopped, prev.at)
	}
	resume := l.resume
	if resume <= 0 {
		resume = vodResume
	}
	st := &vodPause{ls: ls, at: at}
	st.timer = time.AfterFunc(resume, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.stopped[ls.key] == st {
			delete(l.stopped, ls.key)
			ls.onEnd(ls, vodStopped, at)
		}
	})
	l.stopped[ls.key] = st
}

// flush reports every stopped playback now, and from now on reports
// stops at once (for shutdown).
func (l *vodLeases) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closing = true
	for k, st := range l.stopped {
		st.timer.Stop()
		delete(l.stopped, k)
		st.ls.onEnd(st.ls, vodStopped, st.at)
	}
}

// FlushVOD records every movie and episode playback still waiting to see
// if the player comes back, and stops waiting from now on. For shutdown.
func (h *Handlers) FlushVOD() { h.vodLeases.flush() }
