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
	guideChannelFilter
	Guides []db.ListGuideSourceNamesRow // the guide filter's choices
	Rows   []db.SearchGuideChannelsRow
	More   bool // the search found more than Rows
}

// guideChannelFilter narrows a guide channel search.
type guideChannelFilter struct {
	Guide        int64 // one guide's channels (0: every guide's)
	WithPrograms bool  // only ids the guide has programs for
	Unused       bool  // only ids no channel uses yet
}

// searchGuideChannels finds guide channels whose id or display names hold
// every word of q ("hbo west" finds HBO.West.us). An empty q finds none,
// unless one guide is chosen: then it lists that guide.
func (ui *UI) searchGuideChannels(r *http.Request, q string, f guideChannelFilter, limit int32) ([]db.SearchGuideChannelsRow, error) {
	fields := strings.Fields(q)
	if len(fields) == 0 && f.Guide == 0 {
		return nil, nil
	}
	words := make([]string, len(fields))
	for i, f := range fields {
		words[i] = "%" + likeEscape(f) + "%"
	}
	p := db.SearchGuideChannelsParams{Words: words, Exact: q, WithPrograms: f.WithPrograms, Unused: f.Unused, Limit: limit}
	if f.Guide > 0 {
		p.SourceID = &f.Guide
	}
	return ui.Store.SearchGuideChannels(r.Context(), p)
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
	v := r.URL.Query()
	d := guideChannelsData{Query: strings.TrimSpace(v.Get("gq")), guideChannelFilter: guideChannelFilter{
		Guide: int64(max(atoi(v.Get("guide")), 0)), WithPrograms: v.Get("programs") == "1", Unused: v.Get("unused") == "1"}}
	var err error
	if d.Guides, err = ui.Store.ListGuideSourceNames(r.Context()); err != nil {
		return d, err
	}
	// One more than shown, to tell whether there are more.
	if d.Rows, err = ui.searchGuideChannels(r, d.Query, d.guideChannelFilter, guideChannelRows+1); err != nil {
		return d, err
	}
	if len(d.Rows) > guideChannelRows {
		d.Rows, d.More = d.Rows[:guideChannelRows], true
	}
	return d, nil
}

// tvgSuggest lists guide channels matching what is typed in a channel's
// tvg-id field, to pick one from.
func (ui *UI) tvgSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("tvg_id"))
	rows, err := ui.searchGuideChannels(r, q, guideChannelFilter{}, guideChannelPicks)
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
