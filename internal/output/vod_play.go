package output

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/auth"
	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
	"github.com/sysadmin/mmp/internal/redact"
	"github.com/sysadmin/mmp/internal/source/xtream"
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
		u, err := h.Access.Customers.ByToken(r.Context(), chi.URLParam(r, "token"))
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
}

func (h *Handlers) vodTarget(ctx context.Context, u db.User, kind string, id int64) (vodTarget, error) {
	admin := u.Role == auth.RoleAdmin
	if kind == "movie" {
		m, err := h.VOD.GetUserVODItem(ctx, db.GetUserVODItemParams{ID: id, UserID: u.ID, IsAdmin: admin})
		return vodTarget{m.ProviderID, m.ExternalID, m.ContainerExtension}, err
	}
	e, err := h.VOD.GetUserEpisode(ctx, db.GetUserEpisodeParams{ID: id, UserID: u.ID, IsAdmin: admin})
	return vodTarget{e.ProviderID, e.ExternalID, e.ContainerExtension}, err
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

	release, err := h.vodLeases.acquire(vodKey{u.ID, kind, id}, func() (*proxy.Slot, *proxy.Slot, error) {
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
	})
	switch {
	case errors.Is(err, errProviderFull):
		w.Header().Set("Retry-After", "10")
		http.Error(w, "provider busy, try again shortly", http.StatusServiceUnavailable)
		return
	case err != nil:
		h.Log.Info("output: connection limit", "user_id", u.ID, "kind", kind, "max", u.MaxConnections)
		http.Error(w, "connection limit reached", http.StatusForbidden)
		return
	}
	defer release()

	client, err := xtream.New(p.Url, p.Username, p.Password, p.UserAgent)
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
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent,
		resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
	case resp.StatusCode == http.StatusNotFound:
		http.NotFound(w, r)
		return
	default:
		h.Log.Warn("output: vod upstream status", "provider_id", p.ID, "kind", kind, "id", id, "status", resp.StatusCode)
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

	h.Log.Info("vod playback started", "user_id", u.ID, "kind", kind, "id", id, "range", r.Header.Get("Range") != "")
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
		}
		if rerr != nil {
			break
		}
	}
	h.Log.Info("vod playback stopped", "user_id", u.ID, "kind", kind, "id", id, "bytes", sent)
}

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
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// vodLease is the slot pair one user's playback of one item holds. Players
// seek by opening a new ranged request (often before closing the old one),
// so every concurrent request for the same (user, item) shares the lease:
// seeking never counts as a second connection. The slots are released as
// soon as the last request ends (a later seek simply re-acquires them), so
// switching to another item right away is never refused.
type vodLease struct {
	userSlot, provSlot *proxy.Slot
	refs               int
}

type vodKey struct {
	userID int64
	kind   string
	id     int64
}

// vodLeases tracks active playback leases; the zero value is ready.
type vodLeases struct {
	mu sync.Mutex
	m  map[vodKey]*vodLease
}

// acquire joins the lease for key or creates one with acquire's slots.
func (l *vodLeases) acquire(key vodKey, take func() (user, prov *proxy.Slot, err error)) (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[vodKey]*vodLease{}
	}
	ls, ok := l.m[key]
	if !ok {
		u, p, err := take()
		if err != nil {
			return nil, err
		}
		ls = &vodLease{userSlot: u, provSlot: p}
		l.m[key] = ls
	}
	ls.refs++
	var once sync.Once
	return func() { once.Do(func() { l.release(key, ls) }) }, nil
}

func (l *vodLeases) release(key vodKey, ls *vodLease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ls.refs--; ls.refs > 0 {
		return
	}
	delete(l.m, key)
	ls.userSlot.Release()
	if ls.provSlot != nil {
		ls.provSlot.Release()
	}
}
