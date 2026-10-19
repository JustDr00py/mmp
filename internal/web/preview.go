package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/output"
	"github.com/JustDr00py/mmp/internal/proxy"
	"github.com/JustDr00py/mmp/internal/redact"
)

// Preview opens channels and provider streams for the admin pages' pop-up
// player. A channel preview joins the channel's shared session like any
// viewer; a stream preview joins the channel already playing that stream,
// or else opens it directly with one of its provider's connection slots.
// Either way the bytes go out as MPEG-TS, through ffmpeg when it is
// available so that the audio is AAC (browsers cannot play AC-3, which
// IPTV uses a lot); the video is copied, so this costs little CPU.
type Preview struct {
	// Attach joins a channel's shared session (*proxy.Manager.Attach).
	Attach func(channelID int64) (*proxy.Viewer, error)
	// Stream loads a provider stream (*db.Queries.GetPreviewStream).
	Stream func(ctx context.Context, id int64) (db.GetPreviewStreamRow, error)
	// Upstream opens provider streams (HTTP-TS or HLS) without a profile.
	Upstream proxy.Opener
	// Capacity holds provider slots, shared with the live sessions.
	Capacity *proxy.Capacity
	// FFmpeg is the ffmpeg binary for the audio conversion; "" sends the
	// stream as it is.
	FFmpeg string
	// Watch lists a running preview with the dashboard's viewers, where it
	// can be kicked (*output.Access.Watch); optional.
	Watch func(v output.Viewer, kick func()) (remove func())
}

// previewMax ends a preview left playing in a forgotten tab, which would
// otherwise hold a provider connection indefinitely.
const previewMax = 15 * time.Minute

// previewFFmpegArgs keeps the first video track as it is and makes the
// first audio track stereo AAC.
const previewFFmpegArgs = "-map 0:v:0? -map 0:a:0? -c:v copy -c:a aac -b:a 128k -ac 2 -sn -dn"

func (ui *UI) mountPreview(r chi.Router) {
	if ui.Preview == nil {
		return
	}
	r.Get("/preview/channel/{id}.ts", ui.previewChannel)
	r.Get("/preview/stream/{id}.ts", ui.previewStream)
}

func (ui *UI) previewChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := ui.Store.GetChannel(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		ui.Log.Error("web: preview channel", "channel_id", id, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	ui.previewSession(w, r, id)
}

func (ui *UI) previewStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s, err := ui.Preview.Stream(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		ui.Log.Error("web: preview stream", "stream_id", id, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	// Already playing for a channel: watch that session instead of opening
	// a second connection to the provider.
	if ui.Live != nil {
		for _, c := range ui.Live.Channels() {
			if c.State == "streaming" && c.StreamID == id && !c.Closing {
				ui.previewSession(w, r, c.ChannelID)
				return
			}
		}
	}
	if !s.ProviderEnabled {
		http.Error(w, "the provider is disabled", http.StatusConflict)
		return
	}
	slot, ok := ui.Preview.acquire(r.Context(), s.ProviderID, int(s.MaxConnections))
	if !ok {
		w.Header().Set("Retry-After", "10")
		http.Error(w, "every connection to "+s.ProviderName+" is in use", http.StatusServiceUnavailable)
		return
	}
	defer slot.Release()

	ctx, kick := ui.previewContext(r)
	defer kick()
	src, err := ui.Preview.open(ctx, ui.Preview.Upstream, proxy.Candidate{
		StreamID: s.ID, ProviderID: s.ProviderID, MaxConnections: int(s.MaxConnections),
		URL: s.StreamUrl, UserAgent: s.UserAgent,
	})
	if err != nil {
		ui.Log.Warn("web: preview open", "stream_id", id, "provider_id", s.ProviderID, "err", redact.Err(err))
		http.Error(w, "the provider did not deliver the stream", http.StatusBadGateway)
		return
	}
	ui.servePreview(w, r.WithContext(ctx), kick, src, 0, id)
}

// previewSession previews channel id through its shared session.
func (ui *UI) previewSession(w http.ResponseWriter, r *http.Request, id int64) {
	v, err := ui.Preview.Attach(id)
	if err != nil {
		if !errors.Is(err, proxy.ErrManagerClosed) {
			ui.Log.Error("web: preview attach", "channel_id", id, "err", err)
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer v.Close()
	ctx, kick := ui.previewContext(r)
	defer kick()
	src, err := ui.Preview.open(ctx, viewerOpener{&viewerStream{ctx: ctx, v: v}}, proxy.Candidate{})
	if err != nil {
		ui.Log.Error("web: preview open", "channel_id", id, "err", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	ui.servePreview(w, r.WithContext(ctx), kick, src, id, 0)
}

// previewContext is the request's context, ended after previewMax and at
// server shutdown too.
func (ui *UI) previewContext(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(r.Context(), previewMax)
	if ui.Ctx != nil {
		stop := context.AfterFunc(ui.Ctx, cancel)
		return ctx, func() { stop(); cancel() }
	}
	return ctx, cancel
}

// previewSlotWait is how long a stream preview waits for a provider slot.
// Switching from one preview to the next sends the new request while the
// server is still letting go of the old one's slot; at a provider with one
// connection, that would fail at once.
const previewSlotWait = 3 * time.Second

// acquire takes a slot at the provider, waiting up to previewSlotWait.
func (p *Preview) acquire(ctx context.Context, providerID int64, limit int) (*proxy.Slot, bool) {
	ctx, cancel := context.WithTimeout(ctx, previewSlotWait)
	defer cancel()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		if slot, ok := p.Capacity.TryAcquire(providerID, limit); ok {
			return slot, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-t.C:
		}
	}
}

// open opens c with up, through ffmpeg when there is one.
func (p *Preview) open(ctx context.Context, up proxy.Opener, c proxy.Candidate) (io.ReadCloser, error) {
	if p.FFmpeg == "" {
		return up.Open(ctx, c)
	}
	c.FFmpeg, c.FFmpegArgs = true, previewFFmpegArgs
	return (&proxy.FFmpegOpener{Path: p.FFmpeg, Upstream: up}).Open(ctx, c)
}

// servePreview copies src to the player until either side ends. Exactly one
// of channelID and streamID is set; it names what the dashboard lists.
func (ui *UI) servePreview(w http.ResponseWriter, r *http.Request, kick func(), src io.ReadCloser, channelID, streamID int64) {
	defer src.Close()
	u, _ := auth.UserFrom(r.Context())
	if ui.Preview.Watch != nil {
		defer ui.Preview.Watch(output.Viewer{ChannelID: channelID, UserID: u.ID, Username: u.Username,
			Addr: auth.RemoteIP(r), Output: "preview", Since: time.Now()}, kick)()
	}
	ui.Log.Info("preview started", "user_id", u.ID, "channel_id", channelID, "stream_id", streamID)
	defer ui.Log.Info("preview stopped", "user_id", u.ID, "channel_id", channelID, "stream_id", streamID)

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	buf := make([]byte, 64*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(proxy.WriteTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // player closed
			}
			_ = rc.Flush()
		}
		if err != nil {
			return // stream ended, kicked or timed out
		}
	}
}

// viewerStream reads a channel session's viewer as an io.ReadCloser.
type viewerStream struct {
	ctx context.Context
	v   *proxy.Viewer
}

func (s *viewerStream) Read(p []byte) (int, error) { return s.v.Read(s.ctx, p) }

func (s *viewerStream) Close() error {
	s.v.Close()
	return nil
}

// viewerOpener hands an open stream to proxy.FFmpegOpener as its upstream.
type viewerOpener struct{ rc io.ReadCloser }

func (o viewerOpener) Open(context.Context, proxy.Candidate) (io.ReadCloser, error) { return o.rc, nil }
