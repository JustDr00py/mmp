package web

import (
	"net/http"
	"strings"

	"github.com/JustDr00py/mmp/internal/db"
)

// Guide channel lookup: the channels the downloaded guides list, so the
// admin finds a channel's guide id (East or West, say) without reading
// the XMLTV file.

const (
	guideChannelRows  = 50 // EPG page search
	guideChannelPicks = 8  // suggestions under a channel's tvg-id field
)

type guideChannelsData struct {
	Query string
	Rows  []db.SearchGuideChannelsRow
}

// searchGuideChannels finds guide channels whose id or display names hold
// every word of q ("hbo west" finds HBO.West.us). An empty q finds none.
func (ui *UI) searchGuideChannels(r *http.Request, q string, limit int32) ([]db.SearchGuideChannelsRow, error) {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return nil, nil
	}
	words := make([]string, len(fields))
	for i, f := range fields {
		words[i] = "%" + likeEscape(f) + "%"
	}
	return ui.Store.SearchGuideChannels(r.Context(), db.SearchGuideChannelsParams{Words: words, Exact: q, Limit: limit})
}

// epgChannels is the EPG page's guide channel search (its results block
// for htmx, else the whole page).
func (ui *UI) epgChannels(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") != "true" {
		ui.epgPage(w, r)
		return
	}
	d, err := ui.guideChannels(r)
	if err != nil {
		ui.fail(w, "guide channels", err)
		return
	}
	ui.fragment(w, r, "epg", "guide-channels", d)
}

func (ui *UI) guideChannels(r *http.Request) (guideChannelsData, error) {
	d := guideChannelsData{Query: strings.TrimSpace(r.URL.Query().Get("gq"))}
	var err error
	d.Rows, err = ui.searchGuideChannels(r, d.Query, guideChannelRows)
	return d, err
}

// tvgSuggest lists guide channels matching what is typed in a channel's
// tvg-id field, to pick one from.
func (ui *UI) tvgSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("tvg_id"))
	rows, err := ui.searchGuideChannels(r, q, guideChannelPicks)
	if err != nil {
		ui.fail(w, "guide channel suggestions", err)
		return
	}
	// Nothing to suggest when the field already holds the only match.
	if len(rows) == 1 && strings.EqualFold(rows[0].TvgID, q) {
		rows = nil
	}
	ui.fragment(w, r, "channel", "tvg-suggestions", guideChannelsData{Query: q, Rows: rows})
}
