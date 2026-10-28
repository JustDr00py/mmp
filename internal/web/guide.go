package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/nameguide"
	"github.com/JustDr00py/mmp/internal/output"
)

// The guide grid shows guideSpan from a half-hour boundary; earlier/later
// move it by guideStep, within the window the customer guide serves.
const (
	guideSpan        = 4 * time.Hour
	guideStep        = 2 * time.Hour
	guideSlot        = 30 * time.Minute
	guideBehind      = 24 * time.Hour
	guideAhead       = 7 * 24 * time.Hour
	guideChannels    = 50  // rows per page
	guideFetchBatch  = 100 // channels whose programs are loaded at once
	guideMinBlockPct = 0.5 // narrower programs still get a sliver
)

func (ui *UI) mountGuide(r chi.Router) {
	r.Get("/guide", ui.guidePage)
}

// guideProgram is one program block, placed as a percentage of the window.
type guideProgram struct {
	ID              string
	Title, SubTitle string
	Description     string
	Start, Stop     time.Time
	Left, Width     float64
	Now             bool // on air
	CutStart        bool // began before the window
	CutStop         bool // runs past it
}

// guideRow is one channel and its programs in the window.
type guideRow struct {
	Channel  output.Channel
	Programs []guideProgram
	FromName bool // the programs come from the stream name, not an XMLTV source
	Playable bool // it has a stream to preview
}

// guideSlotLabel is a time-axis tick.
type guideSlotLabel struct {
	At   time.Time
	Left float64
}

type guideData struct {
	Rows        []guideRow
	Slots       []guideSlotLabel
	Groups      []db.ChannelGroup
	Query       string
	GroupID     int64
	MissingOnly bool
	Start       time.Time
	NowLeft     float64 // the now line; <0 or >100 when outside the window
	Earlier     int64   // start of the earlier window, 0 when out of range
	Later       int64
	Offset      int
	NextOffset  int
	PrevOffset  int
}

// NowShown reports whether the now line falls inside the window.
func (d guideData) NowShown() bool { return d.NowLeft >= 0 && d.NowLeft <= 100 }

// Link is the page URL with the current filters and the given window start
// and channel offset.
func (d guideData) Link(start int64, offset int) string {
	v := url.Values{}
	if d.Query != "" {
		v.Set("q", d.Query)
	}
	if d.GroupID != 0 {
		v.Set("group", strconv.FormatInt(d.GroupID, 10))
	}
	if d.MissingOnly {
		v.Set("missing", "1")
	}
	if start != 0 {
		v.Set("start", strconv.FormatInt(start, 10))
	}
	if offset != 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	if len(v) == 0 {
		return Prefix + "/guide"
	}
	return Prefix + "/guide?" + v.Encode()
}

// StartUnix is the window's start, for links that keep it.
func (d guideData) StartUnix() int64 { return d.Start.Unix() }

// guideStart is the window's start: start (unix seconds) if given and in
// range, else the half hour now falls in.
func guideStart(start string, now time.Time) time.Time {
	first := now.Add(-guideBehind).Truncate(guideSlot)
	last := now.Add(guideAhead - guideSpan).Truncate(guideSlot)
	if s, err := strconv.ParseInt(start, 10, 64); err == nil {
		switch t := time.Unix(s, 0).Truncate(guideSlot); {
		case t.Before(first):
			return first
		case t.After(last):
			return last
		default:
			return t
		}
	}
	return now.Truncate(guideSlot)
}

func (ui *UI) guidePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	qv := r.URL.Query()
	d := guideData{
		Query:       strings.TrimSpace(qv.Get("q")),
		GroupID:     int64(max(atoi(qv.Get("group")), 0)),
		MissingOnly: qv.Get("missing") == "1",
		Offset:      max(atoi(qv.Get("offset")), 0),
		Start:       guideStart(qv.Get("start"), now),
	}
	d.PrevOffset = max(d.Offset-guideChannels, 0)
	since, until := d.Start, d.Start.Add(guideSpan)
	if e := since.Add(-guideStep); !e.Before(now.Add(-guideBehind).Truncate(guideSlot)) {
		d.Earlier = e.Unix()
	}
	if l := since.Add(guideStep); !l.Add(guideSpan).After(now.Add(guideAhead)) {
		d.Later = l.Unix()
	}
	pct := func(t time.Time) float64 { return 100 * float64(t.Sub(since)) / float64(guideSpan) }
	d.NowLeft = pct(now)
	for t := since; t.Before(until); t = t.Add(guideSlot) {
		d.Slots = append(d.Slots, guideSlotLabel{At: t, Left: pct(t)})
	}

	rows, err := ui.Store.ListEnabledChannels(ctx)
	if err != nil {
		ui.fail(w, "list channels", err)
		return
	}
	query := strings.ToLower(d.Query)
	var chans []output.Channel
	for _, c := range rows {
		if d.GroupID != 0 && c.GroupID != d.GroupID {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(c.Name), query) && !strings.Contains(strings.ToLower(c.TvgID), query) {
			continue
		}
		chans = append(chans, output.Channel{ID: c.ID, Number: c.Number, Name: c.Name, TvgID: c.TvgID, Logo: c.LogoUrl,
			GroupID: c.GroupID, GroupName: c.GroupName, StreamName: c.StreamName, GuideMode: c.GuideMode})
	}

	names, err := nameguide.Load(ctx, ui.Store)
	if err != nil {
		ui.fail(w, "stream-name guide settings", err)
		return
	}
	fallback := output.NameFallbackFor(names, now)
	// Channels are taken a batch at a time until the page is full, so
	// "only channels without a guide" pages over the matching ones.
	skip := d.Offset
	for len(chans) > 0 && len(d.Rows) <= guideChannels {
		n := min(guideFetchBatch, len(chans))
		batch := chans[:n]
		chans = chans[n:]
		keys := make([]string, 0, n)
		for _, c := range batch {
			keys = append(keys, strings.ToLower(c.GuideID()))
		}
		progs, err := ui.Store.ListGuidePrograms(ctx, db.ListGuideProgramsParams{TvgKeys: keys, Since: since, Until: until, Now: now})
		if err != nil {
			ui.fail(w, "guide programs", err)
			return
		}
		byKey := map[string][]db.ListGuideProgramsRow{}
		for _, p := range progs {
			byKey[p.TvgKey] = append(byKey[p.TvgKey], p)
		}
		for i, c := range batch {
			xmltv := byKey[keys[i]]
			got := output.GuideRows(c, xmltv, fallback, since, until)
			// GuideRows hands back the XMLTV slice itself unless it chose the stream-name guide.
			fromName := len(got) > 0 && (len(xmltv) == 0 || &got[0] != &xmltv[0])
			// As on the EPG page: no XMLTV guide, even if the stream name fills in.
			if d.MissingOnly && len(got) > 0 && !fromName {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			row := guideRow{Channel: c, Playable: c.StreamName != "", FromName: fromName}
			for _, p := range got {
				gp := guideProgram{ID: "gp-" + strconv.FormatInt(c.ID, 10) + "-" + strconv.FormatInt(p.StartTime.Unix(), 10),
					Title: p.Title, SubTitle: p.SubTitle, Description: p.Description, Start: p.StartTime, Stop: p.StopTime,
					Now:      !p.StartTime.After(now) && p.StopTime.After(now),
					CutStart: p.StartTime.Before(since), CutStop: p.StopTime.After(until)}
				gp.Left = max(pct(p.StartTime), 0)
				gp.Width = max(min(pct(p.StopTime), 100)-gp.Left, guideMinBlockPct)
				row.Programs = append(row.Programs, gp)
			}
			d.Rows = append(d.Rows, row)
			if len(d.Rows) > guideChannels {
				break
			}
		}
	}
	if len(d.Rows) > guideChannels {
		d.Rows, d.NextOffset = d.Rows[:guideChannels], d.Offset+guideChannels
	}

	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "guide-grid" {
		ui.fragment(w, r, "guide", "guide-grid", d)
		return
	}
	if d.Groups, err = ui.Store.ListChannelGroups(ctx); err != nil {
		ui.fail(w, "channel groups", err)
		return
	}
	ui.render(w, r, http.StatusOK, "guide", "Guide", d)
}
