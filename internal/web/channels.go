package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sysadmin/mmp/internal/db"
)

const (
	channelsPerPage = 100
	pickerResults   = 30
)

func (ui *UI) mountChannels(r chi.Router) {
	r.Get("/channels", ui.channelsPage)
	r.Get("/channels/new", ui.channelNew)
	r.Post("/channels/new", ui.channelCreate)
	r.Route("/channels/{id}", func(r chi.Router) {
		r.Get("/", ui.channelPage)
		r.Post("/", ui.channelUpdate)
		r.Post("/delete", ui.channelDelete)
		r.Get("/picker", ui.channelPicker)
		r.Post("/streams", ui.channelAddStream)
		r.Post("/streams/order", ui.channelReorder)
		r.Post("/streams/{psid}/move", ui.channelMoveStream)
		r.Post("/streams/{psid}/remove", ui.channelRemoveStream)
	})
}

type channelRow struct {
	db.Channel
	Group   string
	Streams int64
	Playing bool
}

type channelsData struct {
	Rows       []channelRow
	Groups     []db.ChannelGroup
	Query      string
	GroupID    int64
	Offset     int
	NextOffset int
	PrevOffset int
	Flash      string
}

var channelFlashes = map[string]string{
	"created": "Channel created. Add streams to its failover list below.",
	"deleted": "Channel deleted.",
}

func (ui *UI) channelsPage(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	offset := max(atoi(r.URL.Query().Get("offset")), 0)
	gid, _ := strconv.ParseInt(r.URL.Query().Get("group"), 10, 64)
	params := db.ListChannelsParams{Limit: channelsPerPage + 1, Offset: int32(offset)}
	if q != "" {
		pat := "%" + likeEscape(q) + "%"
		params.Search = &pat
	}
	if gid > 0 {
		params.GroupID = &gid
	}
	ctx := r.Context()
	chans, err := ui.Store.ListChannels(ctx, params)
	if err != nil {
		ui.fail(w, "list channels", err)
		return
	}
	groups, err := ui.Store.ListChannelGroups(ctx)
	if err != nil {
		ui.fail(w, "list groups", err)
		return
	}
	counts, err := ui.Store.ChannelStreamCounts(ctx)
	if err != nil {
		ui.fail(w, "channel stream counts", err)
		return
	}
	nStreams := map[int64]int64{}
	for _, c := range counts {
		nStreams[c.ChannelID] = c.Streams
	}
	groupName := map[int64]string{}
	for _, g := range groups {
		groupName[g.ID] = g.Name
	}
	playing := ui.playing()
	d := channelsData{Groups: groups, Query: q, GroupID: gid, Offset: offset, PrevOffset: max(offset-channelsPerPage, 0),
		Flash: channelFlashes[r.URL.Query().Get("msg")]}
	if len(chans) > channelsPerPage {
		chans, d.NextOffset = chans[:channelsPerPage], offset+channelsPerPage
	}
	for _, c := range chans {
		row := channelRow{Channel: c, Streams: nStreams[c.ID], Playing: playing[c.ID] != 0}
		if c.GroupID != nil {
			row.Group = groupName[*c.GroupID]
		}
		d.Rows = append(d.Rows, row)
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "channels" {
		ui.fragment(w, r, "channels", "channels-table", d)
		return
	}
	ui.render(w, r, http.StatusOK, "channels", "Channels", d)
}

// playing maps channel id to the stream its session is playing now.
func (ui *UI) playing() map[int64]int64 {
	out := map[int64]int64{}
	if ui.Live == nil {
		return out
	}
	for _, c := range ui.Live.Channels() {
		if c.State == "streaming" {
			out[c.ChannelID] = c.StreamID
		}
	}
	return out
}

// channelForm is the channel settings form's state.
type channelForm struct {
	ID                        int64
	Number, Name, TvgID, Logo string
	GroupID, ProfileID        int64
	Enabled                   bool
	Error                     string
	Groups                    []db.ChannelGroup
	Profiles                  []db.StreamProfile
}

func (ui *UI) withChoices(r *http.Request, f *channelForm) error {
	var err error
	if f.Groups, err = ui.Store.ListChannelGroups(r.Context()); err != nil {
		return err
	}
	f.Profiles, err = ui.Store.ListStreamProfiles(r.Context())
	return err
}

func parseChannelForm(w http.ResponseWriter, r *http.Request) (channelForm, db.UpdateChannelParams, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return channelForm{}, db.UpdateChannelParams{}, formError("Could not read the form.")
	}
	f := r.PostForm
	trim := func(k string) string { return strings.TrimSpace(f.Get(k)) }
	form := channelForm{Number: trim("number"), Name: trim("name"), TvgID: trim("tvg_id"), Logo: trim("logo_url"),
		Enabled: f.Get("enabled") == "on"}
	form.GroupID, _ = strconv.ParseInt(f.Get("group_id"), 10, 64)
	form.ProfileID, _ = strconv.ParseInt(f.Get("profile_id"), 10, 64)
	p := db.UpdateChannelParams{Name: form.Name, TvgID: form.TvgID, LogoUrl: form.Logo, Enabled: form.Enabled}
	if form.Name == "" {
		return form, p, formError("Name is required.")
	}
	if form.Number != "" {
		n, err := strconv.Atoi(form.Number)
		if err != nil || n < 1 || n > 99999 {
			return form, p, formError("Number must be between 1 and 99999.")
		}
		n32 := int32(n)
		p.Number = &n32
	}
	if form.Logo != "" {
		if u, err := url.Parse(form.Logo); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return form, p, formError("Logo URL must be an absolute http(s) URL.")
		}
	}
	if form.GroupID > 0 {
		p.GroupID = &form.GroupID
	}
	if form.ProfileID > 0 {
		p.StreamProfileID = &form.ProfileID
	}
	return form, p, nil
}

// channelSaveErr maps constraint violations to form messages; ok=false
// means an unexpected error.
func channelSaveErr(err error) (string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505":
			return "That channel number is already in use.", true
		case "23503":
			return "The selected group or profile no longer exists.", true
		}
	}
	return "", false
}

func (ui *UI) channelNew(w http.ResponseWriter, r *http.Request) {
	f := channelForm{Enabled: true}
	if n, err := ui.Store.NextChannelNumber(r.Context()); err == nil {
		f.Number = strconv.Itoa(int(n))
	}
	if err := ui.withChoices(r, &f); err != nil {
		ui.fail(w, "channel choices", err)
		return
	}
	ui.render(w, r, http.StatusOK, "channel_new", "New channel", f)
}

func (ui *UI) channelCreate(w http.ResponseWriter, r *http.Request) {
	form, p, err := parseChannelForm(w, r)
	if err == nil {
		var c db.Channel
		c, err = ui.Store.CreateChannel(r.Context(), db.CreateChannelParams{Number: p.Number, Name: p.Name, TvgID: p.TvgID,
			LogoUrl: p.LogoUrl, GroupID: p.GroupID, StreamProfileID: p.StreamProfileID})
		if err == nil {
			if !p.Enabled { // created enabled by default
				p.ID = c.ID
				_, err = ui.Store.UpdateChannel(r.Context(), p)
			}
			if err == nil {
				ui.Log.Info("web: channel created", "channel_id", c.ID)
				ui.redirect(w, r, Prefix+"/channels/"+strconv.FormatInt(c.ID, 10)+"?msg=created")
				return
			}
		}
		msg, ok := channelSaveErr(err)
		if !ok {
			ui.fail(w, "create channel", err)
			return
		}
		err = errors.New(msg)
	}
	form.Error = err.Error()
	if cerr := ui.withChoices(r, &form); cerr != nil {
		ui.fail(w, "channel choices", cerr)
		return
	}
	ui.render(w, r, http.StatusUnprocessableEntity, "channel_new", "New channel", form)
}

// channel loads {id}, answering 404 itself when it does not exist.
func (ui *UI) channel(w http.ResponseWriter, r *http.Request) (db.Channel, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return db.Channel{}, false
	}
	c, err := ui.Store.GetChannel(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return db.Channel{}, false
	}
	if err != nil {
		ui.fail(w, "get channel", err)
		return db.Channel{}, false
	}
	return c, true
}

func formFromChannel(c db.Channel) channelForm {
	f := channelForm{ID: c.ID, Name: c.Name, TvgID: c.TvgID, Logo: c.LogoUrl, Enabled: c.Enabled}
	if c.Number != nil {
		f.Number = strconv.Itoa(int(*c.Number))
	}
	if c.GroupID != nil {
		f.GroupID = *c.GroupID
	}
	if c.StreamProfileID != nil {
		f.ProfileID = *c.StreamProfileID
	}
	return f
}

type channelPageData struct {
	Form    channelForm
	Streams streamListData
	Flash   string
}

// streamListData is the failover list fragment's state.
type streamListData struct {
	ChannelID int64
	Rows      []db.ListChannelStreamsRow
	Playing   int64 // stream id playing now, 0 if none
}

func (ui *UI) streamList(r *http.Request, channelID int64) (streamListData, error) {
	rows, err := ui.Store.ListChannelStreams(r.Context(), channelID)
	return streamListData{ChannelID: channelID, Rows: rows, Playing: ui.playing()[channelID]}, err
}

func (ui *UI) channelPage(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	ui.renderChannel(w, r, http.StatusOK, c, formFromChannel(c), channelFlashes[r.URL.Query().Get("msg")])
}

func (ui *UI) renderChannel(w http.ResponseWriter, r *http.Request, status int, c db.Channel, f channelForm, flash string) {
	if err := ui.withChoices(r, &f); err != nil {
		ui.fail(w, "channel choices", err)
		return
	}
	sl, err := ui.streamList(r, c.ID)
	if err != nil {
		ui.fail(w, "channel streams", err)
		return
	}
	if r.URL.Query().Get("msg") == "saved" {
		flash = "Channel saved."
	}
	ui.render(w, r, status, "channel", c.Name, channelPageData{Form: f, Streams: sl, Flash: flash})
}

func (ui *UI) channelUpdate(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	form, p, err := parseChannelForm(w, r)
	form.ID = c.ID
	if err == nil {
		p.ID = c.ID
		if _, err = ui.Store.UpdateChannel(r.Context(), p); err == nil {
			ui.redirect(w, r, Prefix+"/channels/"+strconv.FormatInt(c.ID, 10)+"?msg=saved")
			return
		}
		msg, ok := channelSaveErr(err)
		if !ok {
			ui.fail(w, "update channel", err)
			return
		}
		err = errors.New(msg)
	}
	form.Error = err.Error()
	ui.renderChannel(w, r, http.StatusUnprocessableEntity, c, form, "")
}

func (ui *UI) channelDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	if err := ui.Store.DeleteChannel(r.Context(), c.ID); err != nil {
		ui.fail(w, "delete channel", err)
		return
	}
	ui.Log.Info("web: channel deleted", "channel_id", c.ID)
	ui.redirect(w, r, Prefix+"/channels?msg=deleted")
}

// renderStreamList answers a failover-list change: the fragment for htmx,
// the channel page otherwise.
func (ui *UI) renderStreamList(w http.ResponseWriter, r *http.Request, channelID int64) {
	if r.Header.Get("HX-Request") != "true" {
		ui.redirect(w, r, Prefix+"/channels/"+strconv.FormatInt(channelID, 10))
		return
	}
	sl, err := ui.streamList(r, channelID)
	if err != nil {
		ui.fail(w, "channel streams", err)
		return
	}
	ui.fragment(w, r, "channel", "stream-list", sl)
}

// saveOrder writes ids as the channel's failover order (priority 1..n).
// ids must be exactly the channel's current streams.
func (ui *UI) saveOrder(r *http.Request, channelID int64, ids []int64) error {
	return ui.Store.ReorderChannelStreams(r.Context(), db.ReorderChannelStreamsParams{ChannelID: channelID, StreamIds: ids})
}

func currentOrder(rows []db.ListChannelStreamsRow) []int64 {
	ids := make([]int64, len(rows))
	for i, s := range rows {
		ids[i] = s.ProviderStreamID
	}
	return ids
}

// channelReorder saves a drag-and-drop order: the full list of stream
// ids, which must be a permutation of the current list.
func (ui *UI) channelReorder(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rows, err := ui.Store.ListChannelStreams(r.Context(), c.ID)
	if err != nil {
		ui.fail(w, "channel streams", err)
		return
	}
	cur := map[int64]bool{}
	for _, id := range currentOrder(rows) {
		cur[id] = true
	}
	var ids []int64
	seen := map[int64]bool{}
	for _, v := range r.PostForm["stream"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || !cur[id] || seen[id] {
			http.Error(w, "the list changed; reload the page", http.StatusConflict)
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) != len(cur) {
		http.Error(w, "the list changed; reload the page", http.StatusConflict)
		return
	}
	if err := ui.saveOrder(r, c.ID, ids); err != nil {
		ui.fail(w, "reorder", err)
		return
	}
	ui.renderStreamList(w, r, c.ID)
}

// channelMoveStream moves one stream up or down (the no-drag path).
func (ui *UI) channelMoveStream(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	psid, _ := strconv.ParseInt(chi.URLParam(r, "psid"), 10, 64)
	rows, err := ui.Store.ListChannelStreams(r.Context(), c.ID)
	if err != nil {
		ui.fail(w, "channel streams", err)
		return
	}
	ids := currentOrder(rows)
	i := -1
	for k, id := range ids {
		if id == psid {
			i = k
		}
	}
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	j := i - 1
	if r.FormValue("dir") == "down" {
		j = i + 1
	}
	if j >= 0 && j < len(ids) {
		ids[i], ids[j] = ids[j], ids[i]
		if err := ui.saveOrder(r, c.ID, ids); err != nil {
			ui.fail(w, "reorder", err)
			return
		}
	}
	ui.renderStreamList(w, r, c.ID)
}

func (ui *UI) channelRemoveStream(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	psid, _ := strconv.ParseInt(chi.URLParam(r, "psid"), 10, 64)
	if err := ui.Store.RemoveStreamFromChannel(r.Context(), db.RemoveStreamFromChannelParams{ChannelID: c.ID, ProviderStreamID: psid}); err != nil {
		ui.fail(w, "remove stream", err)
		return
	}
	ui.renderStreamList(w, r, c.ID)
}

// channelAddStream appends a stream to the end of the failover list.
func (ui *UI) channelAddStream(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	psid, err := strconv.ParseInt(r.FormValue("stream"), 10, 64)
	if err != nil || psid <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rows, err := ui.Store.ListChannelStreams(r.Context(), c.ID)
	if err != nil {
		ui.fail(w, "channel streams", err)
		return
	}
	prio := int32(1)
	for _, s := range rows {
		if s.ProviderStreamID == psid { // already listed: nothing to do
			ui.renderStreamList(w, r, c.ID)
			return
		}
		prio = max(prio, s.Priority+1)
	}
	if err := ui.Store.AddStreamToChannel(r.Context(), db.AddStreamToChannelParams{ChannelID: c.ID, ProviderStreamID: psid, Priority: prio}); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23503" {
			http.Error(w, "that stream no longer exists", http.StatusConflict)
			return
		}
		ui.fail(w, "add stream", err)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		ui.renderStreamList(w, r, c.ID)
		return
	}
	// The list, plus an out-of-band swap marking the picker entry as added.
	sl, err := ui.streamList(r, c.ID)
	if err != nil {
		ui.fail(w, "channel streams", err)
		return
	}
	ui.fragment(w, r, "channel", "stream-list-added", struct {
		List    streamListData
		AddedID int64
	}{sl, psid})
}

type pickerData struct {
	ChannelID int64
	Query     string
	Results   []db.SearchStreamsAllProvidersRow
	Listed    map[int64]bool
}

// channelPicker searches every provider's streams to add to the channel.
func (ui *UI) channelPicker(w http.ResponseWriter, r *http.Request) {
	c, ok := ui.channel(w, r)
	if !ok {
		return
	}
	d := pickerData{ChannelID: c.ID, Query: strings.TrimSpace(r.URL.Query().Get("q")), Listed: map[int64]bool{}}
	if d.Query != "" {
		var err error
		d.Results, err = ui.Store.SearchStreamsAllProviders(r.Context(), db.SearchStreamsAllProvidersParams{
			Pattern: "%" + likeEscape(d.Query) + "%", Limit: pickerResults})
		if err != nil {
			ui.fail(w, "search streams", err)
			return
		}
		rows, err := ui.Store.ListChannelStreams(r.Context(), c.ID)
		if err != nil {
			ui.fail(w, "channel streams", err)
			return
		}
		for _, s := range rows {
			d.Listed[s.ProviderStreamID] = true
		}
	}
	ui.fragment(w, r, "channel", "picker-results", d)
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
