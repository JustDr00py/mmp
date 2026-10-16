package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

const (
	// liveTick is how often the dashboard is re-rendered for SSE clients;
	// only changed snapshots are sent.
	liveTick = time.Second
	// liveHeartbeat keeps idle proxies from dropping the event stream.
	liveHeartbeat = 15 * time.Second
	liveEvents    = 12
)

type liveChannel struct {
	ID        int64
	Number    *int32
	Name      string
	Logo      string
	State     string
	Provider  string
	Stream    string
	StreamID  int64
	For       string // time in the current state
	Uptime    string // since the session started
	Failovers int
	Viewers   int // customers watching (or session readers without a viewer list)
	Closing   bool

	Profile    string // "FFmpeg" or "Passthrough"
	NowPlaying string // the guide's current program
	Streams    []liveStreamOption
	Media      proxy.MediaInfo
	Kbps       int
	AvgKbps    int
	Bytes      uint64
	Watchers   []liveViewer
}

type liveStreamOption struct {
	ID    int64
	Label string
}

type liveViewer struct {
	ID                                  uint64
	Addr, User, Output, Since, Duration string
}

type liveProvider struct {
	ID       int64
	Name     string
	Used     int
	Max      int32 // 0 = unlimited
	Percent  int   // of Max, for the bar
	Disabled bool
}

type liveEvent struct {
	At, Type, Channel, Message string
	ChannelID                  int64
}

// liveSnapshot is everything the dashboard shows.
type liveSnapshot struct {
	Sessions, Streaming, Viewers, Customers int
	Channels                                []liveChannel
	Providers                               []liveProvider
	Events                                  []liveEvent
}

func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func (ui *UI) snapshot(ctx context.Context) (liveSnapshot, error) {
	var s liveSnapshot
	providers, err := ui.Store.ListProviders(ctx)
	if err != nil {
		return s, err
	}
	provName := map[int64]string{}
	for _, p := range providers {
		provName[p.ID] = p.Name
	}

	var chans []liveChannel
	var chanIDs, streamIDs []int64
	streamOf := map[int64]int64{}
	if ui.Live != nil {
		for _, c := range ui.Live.Channels() {
			s.Sessions++
			s.Viewers += c.Viewers
			if c.State == "streaming" {
				s.Streaming++
			}
			chans = append(chans, liveChannel{ID: c.ChannelID, State: c.State, Provider: provName[c.ProviderID],
				StreamID: c.StreamID, For: shortDuration(time.Since(c.Since)), Uptime: shortDuration(time.Since(c.Started)),
				Failovers: c.Failovers, Viewers: c.Viewers, Closing: c.Closing,
				Media: c.Media, Kbps: c.Kbps, AvgKbps: c.AvgKbps, Bytes: c.Bytes})
			chanIDs = append(chanIDs, c.ChannelID)
			if c.StreamID != 0 {
				streamIDs = append(streamIDs, c.StreamID)
				streamOf[c.ChannelID] = c.StreamID
			}
		}
	}

	events, err := ui.Store.ListRecentEvents(ctx, db.ListRecentEventsParams{Limit: liveEvents})
	if err != nil {
		return s, err
	}
	for _, e := range events {
		if e.ChannelID != nil {
			chanIDs = append(chanIDs, *e.ChannelID)
		}
		for _, id := range []*int64{e.FromStreamID, e.ToStreamID} {
			if id != nil {
				streamIDs = append(streamIDs, *id)
			}
		}
	}

	chanName := map[int64]string{}
	if len(chanIDs) > 0 {
		rows, err := ui.Store.ChannelNamesByIDs(ctx, chanIDs)
		if err != nil {
			return s, err
		}
		for _, r := range rows {
			chanName[r.ID] = r.Name
		}
	}
	streamName := map[int64]string{}
	if len(streamIDs) > 0 {
		rows, err := ui.Store.GetStreamsByIDs(ctx, streamIDs)
		if err != nil {
			return s, err
		}
		for _, r := range rows {
			streamName[r.ID] = r.Name + " · " + r.ProviderName
		}
	}

	if err := ui.cardDetails(ctx, chans); err != nil {
		return s, err
	}
	for i := range chans {
		if chans[i].Name == "" {
			chans[i].Name = chanName[chans[i].ID]
		}
		if id, ok := streamOf[chans[i].ID]; ok {
			chans[i].Stream = streamName[id]
		}
	}
	slices.SortFunc(chans, func(a, b liveChannel) int { return strings.Compare(a.Name, b.Name) })
	s.Channels = chans

	var used map[int64]int
	if ui.Slots != nil {
		used = ui.Slots()
	}
	for _, p := range providers {
		if !p.Enabled && used[p.ID] == 0 {
			continue
		}
		lp := liveProvider{ID: p.ID, Name: p.Name, Used: used[p.ID], Max: p.MaxConnections, Disabled: !p.Enabled}
		if lp.Max > 0 {
			lp.Percent = min(100, lp.Used*100/int(lp.Max))
		}
		s.Providers = append(s.Providers, lp)
	}

	if ui.Connections != nil {
		for _, n := range ui.Connections() {
			s.Customers += n
		}
	}

	for _, e := range events {
		le := liveEvent{At: e.Ts.Local().Format("15:04:05"), Type: e.Type, Message: e.Message}
		if e.ChannelID != nil {
			le.ChannelID, le.Channel = *e.ChannelID, chanName[*e.ChannelID]
		}
		if (e.Type == "failover" || e.Type == "switch") && e.FromStreamID != nil && e.ToStreamID != nil {
			if *e.FromStreamID == *e.ToStreamID {
				le.Message = streamName[*e.ToStreamID] + " reconnected"
			} else {
				le.Message = streamName[*e.FromStreamID] + " → " + streamName[*e.ToStreamID]
			}
		}
		s.Events = append(s.Events, le)
	}
	return s, nil
}

// cardDetails fills in what the channel cards show beyond the session:
// logo, profile, the current program, the stream picker and the viewers.
func (ui *UI) cardDetails(ctx context.Context, chans []liveChannel) error {
	if len(chans) == 0 {
		return nil
	}
	ids := make([]int64, len(chans))
	byID := map[int64]*liveChannel{}
	for i := range chans {
		ids[i] = chans[i].ID
		byID[chans[i].ID] = &chans[i]
	}
	info, err := ui.Store.LiveChannelInfo(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range info {
		c := byID[r.ID]
		c.Number, c.Name, c.Logo, c.NowPlaying = r.Number, r.Name, r.LogoUrl, r.NowTitle
		c.Profile = "Passthrough"
		if r.ProfileMode == "ffmpeg" {
			c.Profile = "FFmpeg"
		}
		if r.ProfileName != "" {
			c.Profile += " · " + r.ProfileName
		}
	}
	streams, err := ui.Store.LiveChannelStreams(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range streams {
		c := byID[r.ChannelID]
		c.Streams = append(c.Streams, liveStreamOption{ID: r.ProviderStreamID, Label: r.StreamName + " [" + r.ProviderName + "]"})
	}
	if ui.Viewers == nil {
		return nil
	}
	for i := range chans {
		chans[i].Viewers = 0 // counted from the viewer list below
	}
	now := time.Now()
	for _, v := range ui.Viewers.Viewers() {
		c, ok := byID[v.ChannelID]
		if !ok {
			continue
		}
		c.Viewers++
		c.Watchers = append(c.Watchers, liveViewer{ID: v.ID, Addr: v.Addr, User: v.Username, Output: strings.ToUpper(v.Output),
			Since: clockTime(v.Since, now), Duration: shortDuration(now.Sub(v.Since))})
	}
	return nil
}

// clockTime shows a time of day, with the date when it is not today.
func clockTime(t, now time.Time) string {
	t = t.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 2 15:04")
}

// mbps renders a bitrate: "2.48 Mbps", or kbps below one megabit.
func mbps(kbps int) string {
	if kbps < 1000 {
		return fmt.Sprintf("%d kbps", kbps)
	}
	return fmt.Sprintf("%.2f Mbps", float64(kbps)/1000)
}

// byteSize renders a byte count: "17.71 MB".
func byteSize(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

// fps renders a frame rate: "29.97", "25".
func fps(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", f), "0"), ".")
}

// mountLive registers the dashboard's controls for running channels.
func (ui *UI) mountLive(r chi.Router) {
	r.Post("/live/channels/{id}/stop", ui.liveStop)
	r.Post("/live/channels/{id}/switch", ui.liveSwitch)
	r.Post("/live/viewers/{id}/kick", ui.liveKick)
}

var liveFlashes = map[string]string{
	"stopped":  "Channel stopped. Its viewers were disconnected; players that reconnect start it again.",
	"switched": "Switching streams. The card shows the new stream once it plays.",
	"kicked":   "Viewer disconnected. Their player may reconnect.",
	"gone":     "That channel or viewer is no longer live.",
}

func (ui *UI) liveDone(w http.ResponseWriter, r *http.Request, ok bool, msg string) {
	if !ok {
		msg = "gone"
	}
	ui.redirect(w, r, Prefix+"/?msg="+msg)
}

func adminName(r *http.Request) string {
	if u, ok := auth.UserFrom(r.Context()); ok {
		return u.Username
	}
	return ""
}

func (ui *UI) liveStop(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || ui.Live == nil {
		http.NotFound(w, r)
		return
	}
	ok := ui.Live.Stop(id)
	ui.Log.Info("web: channel stopped", "channel_id", id, "by", adminName(r), "was_running", ok)
	ui.liveDone(w, r, ok, "stopped")
}

func (ui *UI) liveSwitch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	stream, serr := strconv.ParseInt(r.FormValue("stream"), 10, 64)
	if err != nil || serr != nil || stream <= 0 || ui.Live == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Only a stream on the channel's own failover list.
	rows, err := ui.Store.ListChannelStreams(r.Context(), id)
	if err != nil {
		ui.fail(w, "channel streams", err)
		return
	}
	if !slices.ContainsFunc(rows, func(s db.ListChannelStreamsRow) bool { return s.ProviderStreamID == stream }) {
		http.Error(w, "that stream is not on this channel", http.StatusConflict)
		return
	}
	ok := ui.Live.Switch(id, stream)
	ui.Log.Info("web: channel switched", "channel_id", id, "stream_id", stream, "by", adminName(r), "was_running", ok)
	ui.liveDone(w, r, ok, "switched")
}

func (ui *UI) liveKick(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil || ui.Viewers == nil {
		http.NotFound(w, r)
		return
	}
	ok := ui.Viewers.Kick(id)
	ui.Log.Info("web: viewer kicked", "viewer_id", id, "by", adminName(r), "found", ok)
	ui.liveDone(w, r, ok, "kicked")
}

// liveStream is the dashboard's server-sent event stream: a "live" event
// carrying the re-rendered dashboard whenever it changes.
func (ui *UI) liveStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	w.WriteHeader(http.StatusOK)

	done := r.Context().Done()
	var stop <-chan struct{}
	if ui.Ctx != nil {
		stop = ui.Ctx.Done() // server shutdown ends the stream
	}
	tick := time.NewTicker(liveTick)
	defer tick.Stop()
	var last []byte
	lastSent := time.Now()
	send := func(b []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := w.Write(b); err != nil {
			return false
		}
		lastSent = time.Now()
		return rc.Flush() == nil
	}
	for {
		snap, err := ui.snapshot(r.Context())
		if err != nil {
			if r.Context().Err() == nil {
				ui.Log.Warn("web: live snapshot", "err", err)
			}
		} else {
			var buf bytes.Buffer
			if err := ui.pages["dashboard"].ExecuteTemplate(&buf, "live", snap); err != nil {
				ui.Log.Error("web: render live", "err", err)
				return
			}
			if !bytes.Equal(buf.Bytes(), last) {
				last = bytes.Clone(buf.Bytes())
				if !send(sseEvent("live", buf.Bytes())) {
					return
				}
			} else if time.Since(lastSent) >= liveHeartbeat {
				if !send([]byte(": ping\n\n")) {
					return
				}
			}
		}
		select {
		case <-done:
			return
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// sseEvent frames data as one server-sent event (each line "data: ").
func sseEvent(name string, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("event: " + name + "\n")
	for _, line := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		b.WriteString("data: ")
		b.Write(bytes.TrimRight(line, "\r"))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}
