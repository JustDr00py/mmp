package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sysadmin/mmp/internal/db"
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
	Name      string
	State     string
	Provider  string
	Stream    string
	For       string // time in the current state
	Failovers int
	Viewers   int
	Closing   bool
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
				For: shortDuration(time.Since(c.Since)), Failovers: c.Failovers, Viewers: c.Viewers, Closing: c.Closing})
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

	for i := range chans {
		chans[i].Name = chanName[chans[i].ID]
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
		if e.Type == "failover" && e.FromStreamID != nil && e.ToStreamID != nil {
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
