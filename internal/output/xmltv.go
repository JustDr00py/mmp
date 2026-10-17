package output

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// Guide window served to players: a day of catch-up context, a week ahead.
const (
	guideBehind = 24 * time.Hour
	guideAhead  = 7 * 24 * time.Hour
)

// guideBatch bounds how many channels' programs are loaded at once, so a
// full guide streams out without being held in memory.
const guideBatch = 100

// xmltvTime is the XMLTV timestamp layout; times are always written in UTC.
const xmltvTime = "20060102150405 -0700"

// GuideFetcher loads programs for a batch of lower-cased tvg-ids.
type GuideFetcher func(ctx context.Context, keys []string) ([]db.ListGuideProgramsRow, error)

type xmltvChannel struct {
	XMLName      xml.Name   `xml:"channel"`
	ID           string     `xml:"id,attr"`
	DisplayNames []string   `xml:"display-name"`
	Icon         *xmltvIcon `xml:"icon,omitempty"`
}

type xmltvIcon struct {
	Src string `xml:"src,attr"`
}

type xmltvProgram struct {
	XMLName  xml.Name `xml:"programme"`
	Start    string   `xml:"start,attr"`
	Stop     string   `xml:"stop,attr"`
	Channel  string   `xml:"channel,attr"`
	Title    string   `xml:"title"`
	SubTitle string   `xml:"sub-title,omitempty"`
	Desc     string   `xml:"desc,omitempty"`
}

// WriteXMLTV renders the guide for chans. Channel ids are the channels'
// tvg-ids, the same value the playlists carry as tvg-id, so players link
// the two; channels without a tvg-id have no guide and are left out, and a
// tvg-id shared by several channels is listed once. Programs are fetched
// guideBatch channels at a time.
func WriteXMLTV(ctx context.Context, w io.Writer, chans []Channel, fetch GuideFetcher) error {
	if _, err := io.WriteString(w, xml.Header+`<!DOCTYPE tv SYSTEM "xmltv.dtd">`+"\n"); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", " ")
	tv := xml.StartElement{Name: xml.Name{Local: "tv"}, Attr: []xml.Attr{
		{Name: xml.Name{Local: "generator-info-name"}, Value: "MMP"},
	}}
	if err := enc.EncodeToken(tv); err != nil {
		return err
	}

	var keys []string
	outID := map[string]string{} // lower-cased tvg-id -> id as first listed
	for _, c := range chans {
		id := strings.TrimSpace(c.TvgID)
		key := strings.ToLower(id)
		if id == "" || outID[key] != "" {
			continue
		}
		outID[key] = id
		keys = append(keys, key)
		ch := xmltvChannel{ID: id, DisplayNames: []string{c.Name}}
		if c.Number != nil {
			ch.DisplayNames = append(ch.DisplayNames, strconv.Itoa(int(*c.Number)))
		}
		if c.Logo != "" {
			ch.Icon = &xmltvIcon{Src: c.Logo}
		}
		if err := enc.Encode(ch); err != nil {
			return err
		}
	}

	for len(keys) > 0 {
		n := min(guideBatch, len(keys))
		rows, err := fetch(ctx, keys[:n])
		if err != nil {
			return err
		}
		keys = keys[n:]
		for _, r := range rows {
			if err := enc.Encode(xmltvProgram{
				Start:    r.StartTime.UTC().Format(xmltvTime),
				Stop:     r.StopTime.UTC().Format(xmltvTime),
				Channel:  outID[r.TvgKey],
				Title:    r.Title,
				SubTitle: r.SubTitle,
				Desc:     r.Description,
			}); err != nil {
				return err
			}
		}
		if err := enc.Flush(); err != nil { // one batch at a time onto the wire
			return err
		}
	}

	if err := enc.EncodeToken(tv.End()); err != nil {
		return err
	}
	if err := enc.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// xmltvPHP is the Xtream guide: /xmltv.php?username=&password=.
func (h *Handlers) xmltvPHP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u, err := h.authPassword(r, q.Get("username"), q.Get("password"))
	if err != nil {
		h.authFail(w, err)
		return
	}
	h.serveGuide(w, r, u)
}

// tokenGuide is /epg/{token}.xml.
func (h *Handlers) tokenGuide(w http.ResponseWriter, r *http.Request) {
	u, err := h.authToken(r, chi.URLParam(r, "token"))
	if err != nil {
		h.authFail(w, err)
		return
	}
	h.serveGuide(w, r, u)
}

func (h *Handlers) serveGuide(w http.ResponseWriter, r *http.Request, u db.User) {
	chans, err := h.channelsFor(r.Context(), u)
	if err != nil {
		h.Log.Error("output: list channels", "user_id", u.ID, "err", err)
		http.Error(w, "guide unavailable", http.StatusInternalServerError)
		return
	}
	now := h.now()
	fetch := func(ctx context.Context, keys []string) ([]db.ListGuideProgramsRow, error) {
		return h.Store.ListGuidePrograms(ctx, db.ListGuideProgramsParams{
			TvgKeys: keys, Since: now.Add(-guideBehind), Until: now.Add(guideAhead),
		})
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="epg.xml"`)
	w.Header().Set("Cache-Control", "no-store")
	// Headers are gone once the first byte is written, so a failure past
	// this point can only cut the document short; log it.
	if err := WriteXMLTV(r.Context(), w, chans, fetch); err != nil {
		h.Log.Warn("output: write guide", "user_id", u.ID, "err", err)
	}
}

func (h *Handlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
