package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// The events page lists what happened to channels' upstreams: failovers,
// admin switches, streams that would not open and channels that went down.

const eventsPerPage = 50

// eventTypes are the event types, in the filter's order, with their labels.
var eventTypes = []struct{ Value, Label string }{
	{"failover", "Failovers"},
	{"switch", "Switches by an admin"},
	{"open_failed", "Streams that would not open"},
	{"exhausted", "Channels that went down"},
}

func (ui *UI) mountEvents(r chi.Router) {
	r.Get("/events", ui.eventsPage)
}

// eventRow is one event as the dashboard and the events page show it.
type eventRow struct {
	When, Full string // short ("15:04:05" today, else "Jan 2 15:04") and full time
	Type       string
	ChannelID  int64
	Channel    string
	Streams    string // "stream · provider", or "from → to" for a switch
	Message    string // why, in words
}

// eventRows names the channels and streams events refer to.
func (ui *UI) eventRows(ctx context.Context, evs []db.Event) ([]eventRow, error) {
	var chanIDs, streamIDs []int64
	for _, e := range evs {
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
			return nil, err
		}
		for _, r := range rows {
			chanName[r.ID] = r.Name
		}
	}
	streamName := map[int64]string{}
	if len(streamIDs) > 0 {
		rows, err := ui.Store.GetStreamsByIDs(ctx, streamIDs)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			streamName[r.ID] = r.Name + " · " + r.ProviderName
		}
	}
	name := func(id *int64) string {
		if id == nil {
			return ""
		}
		if n, ok := streamName[*id]; ok {
			return n
		}
		return "stream #" + strconv.FormatInt(*id, 10) + " (removed)"
	}

	today := time.Now().Local()
	out := make([]eventRow, 0, len(evs))
	for _, e := range evs {
		at := e.Ts.Local()
		row := eventRow{When: at.Format("Jan 2 15:04"), Full: at.Format("2006-01-02 15:04:05 MST"), Type: e.Type, Message: e.Message}
		if sameDay(at, today) {
			row.When = at.Format("15:04:05")
		}
		if e.ChannelID != nil {
			row.ChannelID, row.Channel = *e.ChannelID, chanName[*e.ChannelID]
		}
		switch e.Type {
		case "failover", "switch":
			if e.FromStreamID != nil && e.ToStreamID != nil && *e.FromStreamID == *e.ToStreamID {
				row.Streams = name(e.ToStreamID) + " reconnected"
			} else if e.FromStreamID != nil {
				row.Streams = name(e.FromStreamID) + " → " + name(e.ToStreamID)
			} else {
				row.Streams = name(e.ToStreamID)
			}
			if row.Message == "switched upstream" { // recorded before reasons were
				row.Message = ""
			}
		default:
			row.Streams = name(e.FromStreamID)
		}
		out = append(out, row)
	}
	return out, nil
}

type eventsData struct {
	Rows        []eventRow
	Providers   []db.Provider
	Types       []struct{ Value, Label string }
	ChannelID   int64
	Channel     string
	ProviderID  int64
	Type        string
	Offset      int
	PrevOffset  int
	NextOffset  int
	Query       func(offset int) string // the page's filters at another offset
	AllChannels string                  // the page's filters without the channel
}

func (ui *UI) eventsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := eventsData{Types: eventTypes, Type: q.Get("type"), Offset: max(atoi(q.Get("offset")), 0)}
	d.ChannelID, _ = strconv.ParseInt(q.Get("channel"), 10, 64)
	d.ProviderID, _ = strconv.ParseInt(q.Get("provider"), 10, 64)
	params := db.ListEventsParams{Limit: eventsPerPage + 1, Offset: int32(d.Offset)}
	if d.ChannelID > 0 {
		params.ChannelID = &d.ChannelID
	}
	if d.ProviderID > 0 {
		params.ProviderID = &d.ProviderID
	}
	if d.Type != "" {
		params.Type = &d.Type
	}
	ctx := r.Context()
	evs, err := ui.Store.ListEvents(ctx, params)
	if err != nil {
		ui.fail(w, "list events", err)
		return
	}
	if len(evs) > eventsPerPage {
		evs, d.NextOffset = evs[:eventsPerPage], d.Offset+eventsPerPage
	}
	d.PrevOffset = max(d.Offset-eventsPerPage, 0)
	if d.Rows, err = ui.eventRows(ctx, evs); err != nil {
		ui.fail(w, "list events", err)
		return
	}
	if d.Providers, err = ui.Store.ListProviders(ctx); err != nil {
		ui.fail(w, "list providers", err)
		return
	}
	if d.ChannelID > 0 {
		if c, err := ui.Store.GetChannel(ctx, d.ChannelID); err == nil {
			d.Channel = c.Name
		}
	}
	link := func(channel int64, offset int) string {
		v := url.Values{}
		if channel > 0 {
			v.Set("channel", strconv.FormatInt(channel, 10))
		}
		if d.ProviderID > 0 {
			v.Set("provider", strconv.FormatInt(d.ProviderID, 10))
		}
		if d.Type != "" {
			v.Set("type", d.Type)
		}
		if offset > 0 {
			v.Set("offset", strconv.Itoa(offset))
		}
		return Prefix + "/events?" + v.Encode()
	}
	d.Query = func(offset int) string { return link(d.ChannelID, offset) }
	d.AllChannels = link(0, 0)
	ui.render(w, r, http.StatusOK, "events", "Events", d)
}

func sameDay(a, b time.Time) bool {
	y1, m1, d1 := a.Date()
	y2, m2, d2 := b.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}
