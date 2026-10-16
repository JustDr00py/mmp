package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JustDr00py/mmp/internal/db"
)

// Channel groups are the categories customers' players show (Xtream
// categories, M3U group-title). Channels without one show as
// "Uncategorized".

func (ui *UI) mountChannelGroups(r chi.Router) {
	r.Get("/groups", ui.channelGroupsPage)
	r.Post("/groups", ui.channelGroupCreate)
	r.Post("/groups/fill", ui.channelGroupsFill)
	r.Post("/groups/{id}", ui.channelGroupUpdate)
	r.Post("/groups/{id}/delete", ui.channelGroupDelete)
}

type channelGroupRow struct {
	db.ChannelGroup
	Channels int64
}

type channelGroupsData struct {
	Rows      []channelGroupRow
	Ungrouped int64 // channels without a group
	Flash     string
	Error     string
	Name      string // new group form
	Sort      string
}

func (ui *UI) channelGroupsData(r *http.Request) (channelGroupsData, error) {
	var d channelGroupsData
	groups, err := ui.Store.ListChannelGroups(r.Context())
	if err != nil {
		return d, err
	}
	counts, err := ui.Store.ChannelGroupCounts(r.Context())
	if err != nil {
		return d, err
	}
	n := map[int64]int64{}
	for _, c := range counts {
		n[c.GroupID] = c.Channels
	}
	for _, g := range groups {
		d.Rows = append(d.Rows, channelGroupRow{ChannelGroup: g, Channels: n[g.ID]})
	}
	d.Ungrouped = n[0]
	return d, nil
}

func (ui *UI) channelGroupsPage(w http.ResponseWriter, r *http.Request) {
	d, err := ui.channelGroupsData(r)
	if err != nil {
		ui.fail(w, "list channel groups", err)
		return
	}
	q := r.URL.Query()
	switch q.Get("msg") {
	case "created":
		d.Flash = "Group created. Put channels in it from the Channels page or a channel's settings."
	case "saved":
		d.Flash = "Group saved."
	case "deleted":
		d.Flash = "Group deleted. Its channels now have no group."
	case "filled":
		d.Flash = "Put " + plural(int64(atoi(q.Get("n"))), "channel", "channels") + " in the group named after their provider group."
	}
	ui.render(w, r, http.StatusOK, "channel_groups", "Groups", d)
}

func channelGroupSaveErr(err error) (string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return "A group with that name exists.", true
	}
	return "", false
}

// renderChannelGroupsErr re-renders the page with a form error.
func (ui *UI) renderChannelGroupsErr(w http.ResponseWriter, r *http.Request, msg, name, sort string) {
	d, err := ui.channelGroupsData(r)
	if err != nil {
		ui.fail(w, "list channel groups", err)
		return
	}
	d.Error, d.Name, d.Sort = msg, name, sort
	ui.render(w, r, http.StatusUnprocessableEntity, "channel_groups", "Groups", d)
}

func (ui *UI) channelGroupCreate(w http.ResponseWriter, r *http.Request) {
	name, sort, raw, err := parseNameSortForm(w, r)
	if err == nil {
		var g db.ChannelGroup
		if g, err = ui.Store.CreateChannelGroup(r.Context(), db.CreateChannelGroupParams{Name: name, SortOrder: sort}); err == nil {
			ui.Log.Info("web: channel group created", "group_id", g.ID)
			ui.redirect(w, r, Prefix+"/groups?msg=created")
			return
		}
		msg, ok := channelGroupSaveErr(err)
		if !ok {
			ui.fail(w, "create channel group", err)
			return
		}
		err = formError(msg)
	}
	ui.renderChannelGroupsErr(w, r, err.Error(), name, raw)
}

// channelGroup loads {id}, answering 404 itself when it does not exist.
func (ui *UI) channelGroup(w http.ResponseWriter, r *http.Request) (db.ChannelGroup, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return db.ChannelGroup{}, false
	}
	g, err := ui.Store.GetChannelGroup(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return db.ChannelGroup{}, false
	}
	if err != nil {
		ui.fail(w, "get channel group", err)
		return db.ChannelGroup{}, false
	}
	return g, true
}

func (ui *UI) channelGroupUpdate(w http.ResponseWriter, r *http.Request) {
	g, ok := ui.channelGroup(w, r)
	if !ok {
		return
	}
	name, sort, _, err := parseNameSortForm(w, r)
	if err == nil {
		if _, err = ui.Store.UpdateChannelGroup(r.Context(), db.UpdateChannelGroupParams{ID: g.ID, Name: name, SortOrder: sort}); err == nil {
			ui.redirect(w, r, Prefix+"/groups?msg=saved")
			return
		}
		msg, ok := channelGroupSaveErr(err)
		if !ok {
			ui.fail(w, "update channel group", err)
			return
		}
		err = formError(msg)
	}
	ui.renderChannelGroupsErr(w, r, "Could not save “"+g.Name+"”: "+err.Error(), "", "")
}

func (ui *UI) channelGroupDelete(w http.ResponseWriter, r *http.Request) {
	g, ok := ui.channelGroup(w, r)
	if !ok {
		return
	}
	if err := ui.Store.DeleteChannelGroup(r.Context(), g.ID); err != nil {
		ui.fail(w, "delete channel group", err)
		return
	}
	ui.Log.Info("web: channel group deleted", "group_id", g.ID)
	ui.redirect(w, r, Prefix+"/groups?msg=deleted")
}

// channelGroupsFill puts every channel without a group into the group
// named after its provider group.
func (ui *UI) channelGroupsFill(w http.ResponseWriter, r *http.Request) {
	var n int64
	err := ui.inTx(r.Context(), func(q Store) error {
		var err error
		n, err = assignSourceGroups(r.Context(), q, nil)
		return err
	})
	if err != nil {
		ui.fail(w, "fill channel groups", err)
		return
	}
	ui.Log.Info("web: channel groups filled", "channels", n)
	ui.redirect(w, r, Prefix+"/groups?msg=filled&n="+strconv.FormatInt(n, 10))
}

// assignSourceGroups moves channels into the channel group named after the
// provider group of their first failover stream, creating groups as
// needed: the listed channels, or every channel without a group when ids
// is nil. Channels whose streams have no provider group are left as they
// are. It returns how many channels changed group.
func assignSourceGroups(ctx context.Context, q Store, ids []int64) (int64, error) {
	rows, err := q.ChannelSourceGroups(ctx, ids)
	if err != nil {
		return 0, err
	}
	byTitle := map[string][]int64{}
	for _, row := range rows {
		byTitle[row.GroupTitle] = append(byTitle[row.GroupTitle], row.ChannelID)
	}
	titles := make([]string, 0, len(byTitle))
	for t := range byTitle {
		titles = append(titles, t)
	}
	slices.Sort(titles) // new groups are numbered in name order
	var n int64
	for _, t := range titles {
		g, err := q.EnsureChannelGroup(ctx, t)
		if err != nil {
			return n, err
		}
		moved, err := q.SetChannelsGroup(ctx, db.SetChannelsGroupParams{GroupID: &g.ID, ChannelIds: byTitle[t]})
		if err != nil {
			return n, err
		}
		n += moved
	}
	return n, nil
}
