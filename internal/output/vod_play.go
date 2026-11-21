package output

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
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

// vodTarget is what one playback fetches: the title and the providers that
// hold it (one, or each pooled account with a copy), in the order to try.
type vodTarget struct {
	copies []vodCopy
	title  string // for the dashboard
}

// vodCopy is one provider's copy of a title.
type vodCopy struct {
	providerID int64
	externalID string
	ext        string
}

// vodTarget looks the title up; pgx.ErrNoRows if the user may not play it.
func (h *Handlers) vodTarget(ctx context.Context, u db.User, kind string, id int64) (vodTarget, error) {
	admin := u.Role == auth.RoleAdmin
	var t vodTarget
	if kind == "movie" {
		rows, err := h.VOD.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: id, UserID: u.ID, IsAdmin: admin})
		if err != nil {
			return t, err
		}
		for _, m := range rows {
			t.copies = append(t.copies, vodCopy{m.ProviderID, m.ExternalID, m.ContainerExtension})
		}
		if len(rows) > 0 {
			t.title = rows[0].Name
		}
	} else {
		rows, err := h.VOD.ListUserEpisodeCopies(ctx, db.ListUserEpisodeCopiesParams{ID: id, UserID: u.ID, IsAdmin: admin})
		if err != nil {
			return t, err
		}
		for _, e := range rows {
			t.copies = append(t.copies, vodCopy{e.ProviderID, e.ExternalID, e.ContainerExtension})
		}
		if len(rows) > 0 {
			t.title = episodeTitle(rows[0])
		}
	}
	if len(t.copies) == 0 {
		return t, pgx.ErrNoRows
	}
	return t, nil
}

// episodeTitle is "Series · S01E02 · Title" (parts it lacks left out).
func episodeTitle(e db.ListUserEpisodeCopiesRow) string {
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
	// A fresh start that fails upstream before anything was sent moves on
	// to the title's next copy (another panel's file may well be there).
	exclude := map[int64]bool{}
	for h.serveVODCopy(w, r, u, kind, exclude) {
	}
}

// serveVODCopy is one playback attempt. It returns true, having added the
// failed provider to exclude and written nothing, when the caller should
// try the title's remaining copies.
func (h *Handlers) serveVODCopy(w http.ResponseWriter, r *http.Request, u db.User, kind string, exclude map[int64]bool) (retry bool) {
	if h.VOD == nil {
		http.NotFound(w, r)
		return false
	}
	id, ok := vodID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return false
	}
	ctx := r.Context()
	tgt, err := h.vodTarget(ctx, u, kind, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return false
	}
	if err != nil {
		h.Log.Error("output: vod lookup", "user_id", u.ID, "kind", kind, "id", id, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return false
	}
	// The providers holding a copy, in order: playback takes the first
	// with a free connection (pooled accounts share one title).
	var provs []db.Provider
	copyOf := map[int64]vodCopy{}
	for _, c := range tgt.copies {
		if _, dup := copyOf[c.providerID]; dup {
			continue
		}
		p, err := h.VOD.GetProvider(ctx, c.providerID)
		if err != nil {
			h.Log.Error("output: vod provider", "provider_id", c.providerID, "err", err)
			continue
		}
		provs = append(provs, p)
		copyOf[p.ID] = c
	}
	provs = h.vodCandidates(provs, copyOf, vodKey{u.ID, kind, id}, rangeStart(r), exclude)
	if len(provs) == 0 {
		if len(exclude) > 0 { // every copy failed upstream
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return false
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return false
	}
	// Another copy is worth trying only for a fresh start: a resume must
	// stay on the file it started (vodCandidates already pinned it).
	canRetry := len(provs) > 1 && rangeStart(r) == 0
	provIDs := make([]int64, len(provs))
	for i, p := range provs {
		provIDs[i] = p.ID
	}

	addr := auth.RemoteIP(r)
	ctx, lease, release, err := h.vodLeases.acquire(ctx, vodAcquire{
		key: vodKey{u.ID, kind, id}, providers: provIDs, userMax: int(u.MaxConnections),
		take: func() (*proxy.Slot, *proxy.Slot, error) {
			userSlot, err := h.Access.Acquire(u)
			if err != nil {
				return nil, nil, err
			}
			if h.Slots == nil {
				return userSlot, nil, nil
			}
			for _, p := range provs {
				if provSlot, ok := h.Slots.TryAcquire(p.ID, int(p.MaxConnections)); ok {
					return userSlot, provSlot, nil
				}
			}
			userSlot.Release()
			return nil, nil, errProviderFull
		},
		takeUser: func() (*proxy.Slot, error) { return h.Access.Acquire(u) },
		register: func(ls *vodLease, kick func()) func() {
			_, remove := h.Access.viewers.add(Viewer{UserID: u.ID, Username: u.Username, Addr: addr, Output: kind,
				Title: tgt.title, ItemID: id, ProviderID: ls.providerID, Since: ls.since}, kick,
				&viewerLive{bytes: &ls.bytes, idleSince: &ls.idleSince})
			return remove
		},
		tookOver: func(old *vodLease) {
			h.Log.Info("vod playback taken over", "user_id", u.ID, "username", u.Username, "old_kind", old.key.kind,
				"old_id", old.key.id, "kind", kind, "id", id)
		},
		ended: func(ls *vodLease, reason string, at time.Time) {
			h.vodSessionEnded(u, addr, tgt, ls.providerID, ls, reason, at)
		},
	})
	switch {
	case errors.Is(err, errProviderFull):
		h.Log.Info("output: vod provider busy", "user_id", u.ID, "username", u.Username, "providers", provIDs,
			"kind", kind, "id", id)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "line busy, try again shortly", http.StatusServiceUnavailable)
		return false
	case err != nil:
		h.Log.Info("output: connection limit", "user_id", u.ID, "username", u.Username, "kind", kind, "id", id,
			"max", u.MaxConnections)
		http.Error(w, "connection limit reached", http.StatusForbidden)
		return false
	}
	defer release()
	// The account this playback holds (a seek joins the running one).
	var p db.Provider
	for _, c := range provs {
		if c.ID == lease.providerID {
			p = c
		}
	}
	if p.ID == 0 { // joined a lease on an account that no longer holds the title
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return false
	}
	cp := copyOf[p.ID]

	client, err := xtream.New(source.ProviderURL(p), p.Username, p.Password, p.UserAgent)
	if err != nil {
		h.Log.Error("output: vod provider url", "provider_id", p.ID, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return false
	}
	upURL := client.MovieURL(cp.externalID, cp.ext)
	if kind == "series" {
		upURL = client.EpisodeURL(cp.externalID, cp.ext)
	}
	method := http.MethodGet
	if r.Method == http.MethodHead {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(ctx, method, upURL, nil)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return false
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
			if canRetry {
				exclude[p.ID] = true
				return true
			}
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return false
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent,
		resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
	case resp.StatusCode == http.StatusNotFound:
		h.Log.Warn("output: vod upstream not found", "provider_id", p.ID, "kind", kind, "id", id)
		h.vodLeases.failed(lease, "upstream status 404")
		if canRetry {
			exclude[p.ID] = true
			return true
		}
		http.NotFound(w, r)
		return false
	default:
		h.Log.Warn("output: vod upstream status", "provider_id", p.ID, "kind", kind, "id", id, "status", resp.StatusCode)
		h.vodLeases.failed(lease, "upstream status "+strconv.Itoa(resp.StatusCode))
		if canRetry {
			exclude[p.ID] = true
			return true
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return false
	}
	h.vodSticky.set(vodKey{u.ID, kind, id}, vodStickyCopy{providerID: p.ID, externalID: cp.externalID,
		backbone: p.Backbone, pooled: p.VodPooled})
	for _, k := range passHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	ct := vodContentTypes[strings.ToLower(cp.ext)]
	if ct == "" {
		ct = resp.Header.Get("Content-Type")
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if method == http.MethodHead {
		return false
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
	return false
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
// seeking never counts as a second connection. When the last request ends
// the lease is held for VODHold: players buffer ahead, drop the connection
// and come back minutes later, and the line is still theirs when they do
// (anyone else gets "line busy" meanwhile). Only the provider's line is
// held: the user's own slot is free meanwhile (live TV can have it), and is
// taken back on return. Held leases still count toward the user's limit
// for new titles: at it, the user's next title ends their longest-idle
// hold first, so skipping through episodes holds one line, not one each.
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
	ended              bool                          // slots already released
	idle               *time.Timer                   // set while held with no request open (userSlot is nil)
	idleSince          atomic.Int64                  // while held: when its last request ended (unix ns)
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

// VODHold is how long a playback keeps its slots after its last request
// ends. Reopening the same title within it (a seek that closed the old
// request first, a reconnect after the player buffered ahead, resuming a
// pause) continues the same session, on the same line. Players seen in the
// watch history come back within 4 minutes almost always.
const VODHold = 5 * time.Minute

// vodLeases tracks active playback leases; the zero value is ready.
type vodLeases struct {
	mu      sync.Mutex
	m       map[vodKey]*vodLease
	hold    time.Duration // 0: VODHold
	closing bool          // flushed for shutdown: end at once
}

// vodAcquire describes one playback request to vodLeases.acquire.
type vodAcquire struct {
	key vodKey
	// providers may serve the title (pooled accounts, or just one): with
	// all of them full, the user's oldest lease on any of them is taken
	// over. The first stands for the lease when take returns no provider
	// slot.
	providers []int64
	// take reserves the user's slot and a provider slot (on one of
	// providers) for a new lease.
	take func() (user, prov *proxy.Slot, err error)
	// userMax is the user's connection limit (0: none). Held leases do not
	// keep the user's slot, so without it a user skipping from title to
	// title would hold a provider line per title. Before a new lease, the
	// user's held leases beyond userMax are ended (the longest idle first).
	userMax int
	// takeUser reserves just the user's slot, to resume a held lease.
	// Optional: without it the lease resumes without one.
	takeUser func() (*proxy.Slot, error)
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
	if ok && ls.idle != nil { // back within the hold: the same session, on the same line
		if a.takeUser != nil {
			u, err := a.takeUser()
			for errors.Is(err, ErrTooManyConnections) {
				old := l.oldestLocked(a.key.userID, nil, a.key)
				if old == nil {
					break
				}
				l.endLocked(old, vodTakenOver)
				if a.tookOver != nil {
					a.tookOver(old)
				}
				u, err = a.takeUser()
			}
			if err != nil {
				return nil, nil, nil, err // the line stays held
			}
			ls.userSlot = u
		}
		ls.idle.Stop()
		ls.idle = nil
		ls.idleSince.Store(0)
	}
	if !ok {
		for a.userMax > 0 && l.countLocked(a.key.userID) >= a.userMax {
			old := l.idlestLocked(a.key.userID)
			if old == nil {
				break // all playing: take below decides
			}
			l.endLocked(old, vodTakenOver)
			if a.tookOver != nil {
				a.tookOver(old)
			}
		}
		var u, p *proxy.Slot
		for {
			if u, p, err = a.take(); err == nil {
				break
			}
			var old *vodLease
			switch {
			case errors.Is(err, ErrTooManyConnections):
				old = l.oldestLocked(a.key.userID, nil, a.key)
			case errors.Is(err, errProviderFull):
				old = l.oldestLocked(a.key.userID, a.providers, a.key)
			}
			if old == nil {
				return nil, nil, nil, err
			}
			l.endLocked(old, vodTakenOver)
			if a.tookOver != nil {
				a.tookOver(old)
			}
		}
		ls = &vodLease{key: a.key, since: time.Now(), userSlot: u, provSlot: p,
			cancels: map[uint64]context.CancelFunc{}, onEnd: a.ended}
		switch {
		case p != nil:
			ls.providerID = p.ProviderID()
		case len(a.providers) > 0:
			ls.providerID = a.providers[0]
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

// oldestLocked is userID's oldest lease other than skip's, on one of
// providers unless that is nil.
func (l *vodLeases) oldestLocked(userID int64, providers []int64, skip vodKey) *vodLease {
	var old *vodLease
	for _, ls := range l.m {
		if ls.key.userID != userID || ls.key == skip || (providers != nil && !slices.Contains(providers, ls.providerID)) {
			continue
		}
		if old == nil || ls.since.Before(old.since) {
			old = ls
		}
	}
	return old
}

// countLocked is how many leases userID has, playing or held.
func (l *vodLeases) countLocked(userID int64) int {
	n := 0
	for _, ls := range l.m {
		if ls.key.userID == userID {
			n++
		}
	}
	return n
}

// idlestLocked is userID's held lease idle the longest, or nil.
func (l *vodLeases) idlestLocked(userID int64) *vodLease {
	var old *vodLease
	for _, ls := range l.m {
		if ls.key.userID != userID || ls.idle == nil {
			continue
		}
		if old == nil || ls.idleSince.Load() < old.idleSince.Load() {
			old = ls
		}
	}
	return old
}

// endLocked ends ls now: its slots are free at once and its requests are
// cancelled (they finish unwinding on their own). It is reported at once,
// as ending when its last request did if it was being held.
func (l *vodLeases) endLocked(ls *vodLease, reason string) {
	at := time.Now()
	if ls.idle != nil {
		ls.idle.Stop()
		ls.idle = nil
		at = time.Unix(0, ls.idleSince.Load())
	}
	if l.closeLocked(ls) && ls.onEnd != nil {
		ls.onEnd(ls, reason, at)
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
	if ls.userSlot != nil {
		ls.userSlot.Release()
	}
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
	if ls.ended {
		return
	}
	if ls.bytes.Load() == 0 && ls.lastErr != "" {
		l.endLocked(ls, vodFailed)
		return
	}
	// Nothing played (a HEAD probe, a player that gave up at once), or
	// shutting down: no line worth holding.
	if !ls.started.Load() || l.closing {
		l.endLocked(ls, vodStopped)
		return
	}
	hold := l.hold
	if hold <= 0 {
		hold = VODHold
	}
	ls.idleSince.Store(time.Now().UnixNano())
	if ls.userSlot != nil {
		ls.userSlot.Release()
		ls.userSlot = nil
	}
	var t *time.Timer
	t = time.AfterFunc(hold, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if ls.idle == t {
			l.endLocked(ls, vodStopped)
		}
	})
	ls.idle = t
}

// flush ends every held playback now, and from now on ends playbacks as
// soon as their last request does (for shutdown).
func (l *vodLeases) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closing = true
	for _, ls := range l.m {
		if ls.idle != nil {
			l.endLocked(ls, vodStopped)
		}
	}
}

// FlushVOD records every movie and episode playback still waiting to see
// if the player comes back, and stops waiting from now on. For shutdown.
func (h *Handlers) FlushVOD() { h.vodLeases.flush() }

// rangeStart is the first byte a request asks for ("bytes=N-"), or 0.
func rangeStart(r *http.Request) int64 {
	v, ok := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Range")), "bytes=")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(v, "-", 2)[0]), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// vodCandidates orders the providers holding a title for playback and
// drops the ones to skip:
//   - excluded providers (failed upstream on this request) go;
//   - a resume (start > 0) stays on the copy the playback started on,
//     since byte offsets mean nothing in another panel's file; pooled
//     accounts of that panel serve the same file and stay eligible;
//   - within one priority, providers with more free connections come
//     first, so equal providers share the load instead of one filling up.
func (h *Handlers) vodCandidates(provs []db.Provider, copyOf map[int64]vodCopy, key vodKey, start int64, exclude map[int64]bool) []db.Provider {
	out := make([]db.Provider, 0, len(provs))
	for _, p := range provs {
		if !exclude[p.ID] {
			out = append(out, p)
		}
	}
	if st, ok := h.vodSticky.get(key); ok && start > 0 {
		var pinned []db.Provider
		for _, p := range out {
			if p.ID == st.providerID || st.pooled && p.VodPooled && p.Backbone != "" && p.Backbone == st.backbone &&
				copyOf[p.ID].externalID == st.externalID {
				pinned = append(pinned, p)
			}
		}
		if len(pinned) > 0 { // else the copy is gone: any copy beats none
			out = pinned
		}
	}
	free := func(p db.Provider) int {
		if p.MaxConnections <= 0 || h.Slots == nil {
			return math.MaxInt32
		}
		return int(p.MaxConnections) - h.Slots.InUse(p.ID)
	}
	slices.SortStableFunc(out, func(a, b db.Provider) int {
		return cmp.Or(cmp.Compare(a.Priority, b.Priority), cmp.Compare(free(b), free(a)))
	})
	return out
}

// vodStickyTTL is how long a playback's copy is remembered for resumes.
const vodStickyTTL = 6 * time.Hour

// vodStickyCopy is the copy a playback started on.
type vodStickyCopy struct {
	providerID int64
	externalID string
	backbone   string
	pooled     bool
	at         time.Time
}

// vodSticky remembers which copy each user's playback of a title used;
// the zero value is ready.
type vodSticky struct {
	mu sync.Mutex
	m  map[vodKey]vodStickyCopy
}

func (s *vodSticky) set(k vodKey, c vodStickyCopy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.m == nil {
		s.m = map[vodKey]vodStickyCopy{}
	}
	if len(s.m) >= 1024 {
		for k, v := range s.m {
			if now.Sub(v.at) > vodStickyTTL {
				delete(s.m, k)
			}
		}
	}
	c.at = now
	s.m[k] = c
}

func (s *vodSticky) get(k vodKey) (vodStickyCopy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[k]
	if !ok || time.Since(c.at) > vodStickyTTL {
		return vodStickyCopy{}, false
	}
	return c, true
}
