package web

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/logos"
)

func (ui *UI) mountLogos(r chi.Router) {
	r.Get("/logos", ui.logosPage)
	r.Post("/logos/update", ui.logosUpdate)
	r.Post("/logos/upload", ui.logosUpload)
	r.Post("/logos/uploads/{name}/delete", ui.logosDelete)
	r.Get("/logos/pick", ui.logosPick)
	r.Get("/logos/suggest", ui.logosSuggest)
	r.Post("/logos/apply", ui.logosApply)
}

// logoHits is the picker grid: logos to choose from.
type logoHits struct {
	Hits  []logos.Hit
	Empty string // shown when there are none
	// Pick marks the tiles as buttons that fill the channel form's logo
	// field; otherwise they link to the file.
	Pick bool
}

type logosData struct {
	State            logos.State
	Library, Uploads int
	Uploaded         []logos.Hit
	Query            string
	Results          logoHits
	Interval         time.Duration
	Flash, Error     string
}

var logoFlashes = map[string]string{
	"updating": "Downloading the logo library. This takes a few minutes; reload the page to see how it is going.",
	"busy":     "The logo library is already being downloaded.",
	"uploaded": "Logo uploaded.",
	"deleted":  "Logo deleted.",
}

func (ui *UI) logosData(r *http.Request) logosData {
	d := logosData{State: ui.Logos.State(), Uploaded: ui.Logos.Uploads(), Interval: ui.LogoUpdateInterval}
	d.Library, d.Uploads = ui.Logos.Counts()
	d.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	if d.Query != "" {
		d.Results = logoHits{Hits: ui.Logos.Search(d.Query, 120), Empty: "No logo matches " + strconv.Quote(d.Query) + "."}
	}
	return d
}

func (ui *UI) logosPage(w http.ResponseWriter, r *http.Request) {
	if ui.Logos == nil {
		http.NotFound(w, r)
		return
	}
	d := ui.logosData(r)
	d.Flash = logoFlashes[r.URL.Query().Get("msg")]
	ui.render(w, r, http.StatusOK, "logos", "Logos", d)
}

func (ui *UI) logosError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	d := ui.logosData(r)
	d.Error = msg
	ui.render(w, r, status, "logos", "Logos", d)
}

func (ui *UI) logosUpdate(w http.ResponseWriter, r *http.Request) {
	if ui.Logos == nil {
		http.NotFound(w, r)
		return
	}
	msg := "updating"
	if !ui.Logos.StartUpdate() {
		msg = "busy"
	}
	ui.redirect(w, r, Prefix+"/logos?msg="+msg)
}

// logosUpload saves an uploaded logo. From the channel form's picker
// (picker=1) it answers with the picker grid holding the new logo, which
// the browser then picks; otherwise it goes back to the Logos page.
func (ui *UI) logosUpload(w http.ResponseWriter, r *http.Request) {
	if ui.Logos == nil {
		http.NotFound(w, r)
		return
	}
	picker := r.URL.Query().Get("picker") == "1"
	refuse := func(msg string) {
		if picker {
			ui.fragment(w, r, "channel", "logo-hits", logoHits{Empty: msg})
			return
		}
		ui.logosError(w, r, http.StatusUnprocessableEntity, msg)
	}
	r.Body = http.MaxBytesReader(w, r.Body, logos.MaxUpload+64<<10)
	if err := r.ParseMultipartForm(logos.MaxUpload + 64<<10); err != nil {
		refuse("Choose an image of at most 5 MB.")
		return
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		refuse("Choose an image to upload.")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, logos.MaxUpload+1))
	if err != nil {
		refuse("Could not read the file.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = fh.Filename
	}
	e, err := ui.Logos.Upload(name, data)
	var bad logos.ErrUpload
	if errors.As(err, &bad) {
		refuse(bad.Error())
		return
	}
	if err != nil {
		ui.fail(w, "upload logo", err)
		return
	}
	ui.Log.Info("web: logo uploaded", "file", e.Name, "bytes", len(data))
	if picker {
		ui.fragment(w, r, "channel", "logo-hits", logoHits{Hits: []logos.Hit{{Entry: e, URL: ui.Logos.URL(e.Path)}}, Pick: true})
		return
	}
	ui.redirect(w, r, Prefix+"/logos?msg=uploaded")
}

func (ui *UI) logosDelete(w http.ResponseWriter, r *http.Request) {
	if ui.Logos == nil {
		http.NotFound(w, r)
		return
	}
	if err := ui.Logos.DeleteUpload(chi.URLParam(r, "name")); errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		ui.fail(w, "delete logo", err)
		return
	}
	ui.redirect(w, r, Prefix+"/logos?msg=deleted")
}

// logosPick is the channel form's picker grid: logos matching what was
// typed (q), or else suggestions for the channel's name.
func (ui *UI) logosPick(w http.ResponseWriter, r *http.Request) {
	if ui.Logos == nil {
		http.NotFound(w, r)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	d := logoHits{Pick: true}
	switch name := strings.TrimSpace(r.URL.Query().Get("name")); {
	case q != "":
		d.Hits = ui.Logos.Search(q, 48)
		d.Empty = "No logo matches " + strconv.Quote(q) + "."
	case name != "":
		d.Hits = ui.Logos.Suggest(name, ui.preferredCountry(r), 12)
		d.Empty = "No suggestion for this name; search for one above."
	default:
		d.Empty = "Type a channel name or search for a logo."
	}
	if lib, _ := ui.Logos.Counts(); lib == 0 {
		d.Empty += " The logo library has not been downloaded yet (Logos page)."
	}
	ui.fragment(w, r, "channel", "logo-hits", d)
}

// preferredCountry is the country most channel names point at, for logo
// suggestions on names that name none.
func (ui *UI) preferredCountry(r *http.Request) string {
	chans, err := ui.Store.ListChannels(r.Context(), db.ListChannelsParams{Limit: 100000})
	if err != nil {
		return ""
	}
	names := make([]string, len(chans))
	for i, c := range chans {
		names[i] = c.Name
	}
	return ui.Logos.PreferredCountry(names)
}

// logoSuggestion is one channel on the bulk page: its logo now and the
// library's best fits.
type logoSuggestion struct {
	Channel db.Channel
	Hits    []logos.Hit
	// Pick is the hit chosen by default ("" keeps the current logo).
	Pick string
}

type logoSuggestData struct {
	Rows []logoSuggestion
	logoSuggestFilter
	Groups    []db.ChannelGroup
	Total     int // channels the current-logo filter lets through
	Confident int
	Skipped   int // channels hidden because they have a library logo
	Library   int
	Flash     string
}

// logoSuggestFilter narrows the bulk page. Have is which current logos
// show: "" none from the library (the default), "none" no logo at all,
// "all" every channel. Fit is how good the best suggestion is: "sure"
// near-certain, "check" a suggestion to look at, "none" no suggestion.
type logoSuggestFilter struct {
	Query   string
	GroupID int64 // 0 any, -1 no group
	Have    string
	Fit     string
}

func logoSuggestFilterOf(v url.Values) logoSuggestFilter {
	f := logoSuggestFilter{Query: strings.TrimSpace(v.Get("q")), Have: v.Get("have"), Fit: v.Get("fit")}
	f.GroupID, _ = strconv.ParseInt(v.Get("group"), 10, 64)
	if v.Get("all") == "1" { // the old "show all" link
		f.Have = "all"
	}
	if f.Have != "none" && f.Have != "all" {
		f.Have = ""
	}
	if f.Fit != "sure" && f.Fit != "check" && f.Fit != "none" {
		f.Fit = ""
	}
	return f
}

// Values is the filter as query parameters, defaults left out.
func (f logoSuggestFilter) Values() url.Values {
	v := url.Values{}
	for k, s := range map[string]string{"q": f.Query, "have": f.Have, "fit": f.Fit} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if f.GroupID != 0 {
		v.Set("group", strconv.FormatInt(f.GroupID, 10))
	}
	return v
}

// Filtered reports whether anything beyond the current-logo filter is set.
func (f logoSuggestFilter) Filtered() bool { return f.Query != "" || f.GroupID != 0 || f.Fit != "" }

// keeps reports whether the filter lets a channel through, before its
// suggestions are known.
func (f logoSuggestFilter) keeps(c db.Channel) bool {
	switch {
	case f.GroupID > 0 && (c.GroupID == nil || *c.GroupID != f.GroupID):
		return false
	case f.GroupID == -1 && c.GroupID != nil:
		return false
	case f.Have == "none" && c.LogoUrl != "":
		return false
	}
	if f.Query == "" {
		return true
	}
	name := strings.ToLower(c.Name)
	for _, w := range strings.Fields(strings.ToLower(f.Query)) {
		if !strings.Contains(name, w) {
			return false
		}
	}
	return true
}

// fits reports whether a channel's suggestions pass the fit filter.
func (f logoSuggestFilter) fits(hits []logos.Hit) bool {
	switch f.Fit {
	case "sure":
		return len(hits) > 0 && hits[0].Confident()
	case "check":
		return len(hits) > 0 && !hits[0].Confident()
	case "none":
		return len(hits) == 0
	}
	return true
}

// logosSuggest is the bulk page: a suggested logo for every channel that
// has none from the library yet, the near-certain ones ticked.
func (ui *UI) logosSuggest(w http.ResponseWriter, r *http.Request) {
	if ui.Logos == nil {
		http.NotFound(w, r)
		return
	}
	chans, err := ui.Store.ListChannels(r.Context(), db.ListChannelsParams{Limit: 100000})
	if err != nil {
		ui.fail(w, "list channels", err)
		return
	}
	d := logoSuggestData{logoSuggestFilter: logoSuggestFilterOf(r.URL.Query())}
	if d.Groups, err = ui.Store.ListChannelGroups(r.Context()); err != nil {
		ui.fail(w, "list groups", err)
		return
	}
	d.Library, _ = ui.Logos.Counts()
	if n := r.URL.Query().Get("applied"); n != "" {
		d.Flash = "Logos set on " + n + " channels."
	}
	names := make([]string, len(chans))
	for i, c := range chans {
		names[i] = c.Name
	}
	prefer := ui.Logos.PreferredCountry(names)
	for _, c := range chans {
		if _, ours := ui.Logos.PathOf(c.LogoUrl); ours && d.Have == "" {
			d.Skipped++
			continue
		}
		d.Total++
		if !d.keeps(c) {
			continue
		}
		row := logoSuggestion{Channel: c, Hits: ui.Logos.Suggest(c.Name, prefer, 4)}
		if !d.fits(row.Hits) {
			continue
		}
		if len(row.Hits) > 0 && row.Hits[0].Confident() && row.Hits[0].URL != c.LogoUrl {
			row.Pick = row.Hits[0].Path
			d.Confident++
		}
		d.Rows = append(d.Rows, row)
	}
	ui.render(w, r, http.StatusOK, "logo_suggest", "Logo suggestions", d)
}

// logosApply sets the logos chosen on the bulk page: logo_<channel id> =
// a library path ("" leaves the channel alone).
func (ui *UI) logosApply(w http.ResponseWriter, r *http.Request) {
	if ui.Logos == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	n := 0
	for key, vals := range r.PostForm {
		idStr, ok := strings.CutPrefix(key, "logo_")
		if !ok || len(vals) == 0 || vals[0] == "" {
			continue
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		e, found := ui.Logos.Lookup(vals[0])
		if err != nil || !found {
			continue
		}
		err = ui.Store.SetChannelLogo(r.Context(), db.SetChannelLogoParams{ID: id, LogoUrl: ui.Logos.URL(e.Path)})
		if err != nil {
			ui.fail(w, "set channel logo", err)
			return
		}
		n++
	}
	ui.Log.Info("web: channel logos set", "channels", n)
	v := logoSuggestFilterOf(r.PostForm).Values()
	v.Set("applied", strconv.Itoa(n))
	target := Prefix + "/logos/suggest?" + v.Encode()
	ui.redirect(w, r, target)
}
