package output

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// HLSConfig tunes the HLS output.
type HLSConfig struct {
	Segment time.Duration // target segment length
	Window  int           // segments listed in the playlist
	Idle    time.Duration // a client silent this long has left
}

// HLS serves channels as live HLS. Each channel has one segmenter fed by a
// single viewer of the shared session, however many HLS clients watch it.
// A client starts at /live/u/p/{id}.m3u8 (or /t/{token}/{id}.m3u8), which
// checks access, holds one of the user's connection slots in a lease and
// redirects to /hls/{lease}/index.m3u8. Playlist reloads and segments are
// addressed by the unguessable lease key, so they need no password check
// and carry no credentials. A lease (and its slot) ends after Idle without
// requests; a channel's segmenter stops when no lease uses it.
type HLS struct {
	cfg     HLSConfig
	streams StreamAttacher
	access  *Access
	log     *slog.Logger
	ctx     context.Context

	mu     sync.Mutex
	chans  map[int64]*hlsChannel
	leases map[string]*hlsLease
}

type hlsChannel struct {
	id      int64
	seg     *proxy.HLSSegmenter
	viewer  *proxy.Viewer
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool // the session ended; a new lease restarts the channel
}

type hlsLease struct {
	key      string
	userID   int64
	channel  *hlsChannel
	slot     *proxy.Slot
	lastSeen time.Time
}

// NewHLS starts the HLS output; it stops serving and frees everything when
// ctx ends.
func NewHLS(ctx context.Context, cfg HLSConfig, streams StreamAttacher, access *Access, log *slog.Logger) *HLS {
	h := &HLS{cfg: cfg, streams: streams, access: access, log: log, ctx: ctx,
		chans: map[int64]*hlsChannel{}, leases: map[string]*hlsLease{}}
	go h.reap()
	return h
}

// errHLSUnavailable means the channel's session could not be joined.
var errHLSUnavailable = errors.New("hls: channel unavailable")

// lease returns the key of u's lease on channel id, creating it (and
// taking a connection slot) if u has none.
func (h *HLS) lease(u db.User, id int64) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.leases {
		if l.userID == u.ID && l.channel.id == id && !l.channel.stopped {
			l.lastSeen = time.Now()
			return l.key, nil
		}
	}
	slot, err := h.access.Acquire(u)
	if err != nil {
		return "", err
	}
	ch, err := h.channelLocked(id)
	if err != nil {
		slot.Release()
		return "", err
	}
	var b [16]byte
	rand.Read(b[:])
	l := &hlsLease{key: hex.EncodeToString(b[:]), userID: u.ID, channel: ch, slot: slot, lastSeen: time.Now()}
	h.leases[l.key] = l
	return l.key, nil
}

// channelLocked returns the running channel id, starting it if needed.
func (h *HLS) channelLocked(id int64) (*hlsChannel, error) {
	if ch, ok := h.chans[id]; ok && !ch.stopped {
		return ch, nil
	}
	v, err := h.streams.Attach(id)
	if err != nil {
		if !errors.Is(err, proxy.ErrManagerClosed) {
			h.log.Error("hls: attach", "channel_id", id, "err", err)
		}
		return nil, errHLSUnavailable
	}
	ctx, cancel := context.WithCancel(h.ctx)
	ch := &hlsChannel{id: id, seg: proxy.NewHLSSegmenter(h.cfg.Segment, h.cfg.Window),
		viewer: v, cancel: cancel, done: make(chan struct{})}
	h.chans[id] = ch
	go h.pump(ctx, ch)
	h.log.Info("hls: channel started", "channel_id", id)
	return ch, nil
}

// pump feeds the channel's viewer into its segmenter until the session
// ends or the channel is stopped.
func (h *HLS) pump(ctx context.Context, ch *hlsChannel) {
	defer close(ch.done)
	defer ch.viewer.Close()
	buf := make([]byte, 64*1024)
	for {
		n, err := ch.viewer.Read(ctx, buf)
		if n > 0 {
			ch.seg.Write(buf[:n], time.Now())
		}
		if err != nil {
			break
		}
	}
	ch.seg.End()
	h.mu.Lock()
	ch.stopped = true
	h.mu.Unlock()
}

// reap ends idle leases and stops channels no lease uses.
func (h *HLS) reap() {
	t := time.NewTicker(max(h.cfg.Idle/4, 100*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			h.mu.Lock()
			for k, l := range h.leases {
				l.slot.Release()
				delete(h.leases, k)
			}
			h.mu.Unlock()
			return
		case <-t.C:
		}
		h.reapOnce(time.Now())
	}
}

func (h *HLS) reapOnce(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	used := map[*hlsChannel]bool{}
	for k, l := range h.leases {
		if now.Sub(l.lastSeen) > h.cfg.Idle {
			l.slot.Release()
			delete(h.leases, k)
			h.log.Info("viewer stopped", "user_id", l.userID, "channel_id", l.channel.id, "output", "hls")
			continue
		}
		used[l.channel] = true
	}
	for id, ch := range h.chans {
		if !used[ch] {
			ch.cancel()
			delete(h.chans, id)
			h.log.Info("hls: channel stopped", "channel_id", id)
		}
	}
}

// touch finds a lease by key and marks it alive.
func (h *HLS) touch(key string) (*hlsLease, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.leases[key]
	if ok {
		l.lastSeen = time.Now()
	}
	return l, ok
}

// Mount registers the lease-addressed routes.
func (h *HLS) Mount(r chi.Router) {
	r.Get("/hls/{key}/index.m3u8", h.playlist)
	r.Get("/hls/{key}/{seq}.ts", h.segment)
}

// playlistWait bounds how long a first playlist request waits for the
// opening segments of a channel that just started.
const playlistWait = 20 * time.Second

// allowCORS lets browser players (hls.js) on other origins fetch the
// playlist and segments; they are addressed by unguessable lease keys.
func allowCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func (h *HLS) playlist(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	l, ok := h.touch(chi.URLParam(r, "key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Two segments before the first playlist, so players do not start on
	// a lone segment and stall at once.
	ctx, cancel := context.WithTimeout(r.Context(), playlistWait)
	defer cancel()
	// Past the wait (or for an ended stream), one segment will do; Wait on
	// the expired ctx only checks what is there.
	if !l.channel.seg.Wait(ctx, 2) && !l.channel.seg.Wait(ctx, 1) {
		http.Error(w, "channel unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	// Relative segment URIs resolve against /hls/{key}/: they stay on MMP.
	w.Write([]byte(l.channel.seg.Playlist(func(seq int64) string { return strconv.FormatInt(seq, 10) + ".ts" })))
}

func (h *HLS) segment(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	l, ok := h.touch(chi.URLParam(r, "key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	seq, err := strconv.ParseInt(chi.URLParam(r, "seq"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	seg, ok := l.channel.seg.Segment(seq)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(seg.Data)))
	w.Header().Set("Cache-Control", "max-age=60")
	w.Write(seg.Data)
}

// Leases is the number of active HLS clients (for tests and the dashboard).
func (h *HLS) Leases() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.leases)
}

// isHLSRequest reports whether a stream path asks for HLS ("{id}.m3u8").
func isHLSRequest(s string) bool { return strings.HasSuffix(s, ".m3u8") }
