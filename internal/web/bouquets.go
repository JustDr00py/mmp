package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sysadmin/mmp/internal/db"
)

// candidateLimit caps the "add channels" list; narrow it with search.
const candidateLimit = 200

func (ui *UI) mountBouquets(r chi.Router) {
	r.Get("/bouquets", ui.bouquetsPage)
	r.Post("/bouquets", ui.bouquetCreate)
	r.Route("/bouquets/{id}", func(r chi.Router) {
		r.Get("/", ui.bouquetPage)
		r.Post("/", ui.bouquetUpdate)
		r.Post("/delete", ui.bouquetDelete)
		r.Get("/editor", ui.bouquetEditorGet)
		r.Post("/add", ui.bouquetAdd)
		r.Post("/add-group", ui.bouquetAddGroup)
		r.Post("/remove", ui.bouquetRemove)
	})
}

type bouquetRow struct {
	db.Bouquet
	Channels, Users int64
}

type bouquetsData struct {
	Rows  []bouquetRow
	Flash string
	Error string
	Name  string
	Sort  string
}

var bouquetFlashes = map[string]string{"deleted": "Bouquet deleted."}

func (ui *UI) bouquetsData(r *http.Request) (bouquetsData, error) {
	var d bouquetsData
	bs, err := ui.Store.ListBouquets(r.Context())
	if err != nil {
		return d, err
	}
	counts, err := ui.Store.BouquetCounts(r.Context())
	if err != nil {
		return d, err
	}
	byID := map[int64]db.BouquetCountsRow{}
	for _, c := range counts {
		byID[c.ID] = c
	}
	for _, b := range bs {
		c := byID[b.ID]
		d.Rows = append(d.Rows, bouquetRow{Bouquet: b, Channels: c.Channels, Users: c.Users})
	}
	return d, nil
}

func (ui *UI) bouquetsPage(w http.ResponseWriter, r *http.Request) {
	d, err := ui.bouquetsData(r)
	if err != nil {
		ui.fail(w, "list bouquets", err)
		return
	}
	d.Flash = bouquetFlashes[r.URL.Query().Get("msg")]
	ui.render(w, r, http.StatusOK, "bouquets", "Bouquets", d)
}

// parseBouquetForm reads name and sort order.
func parseBouquetForm(w http.ResponseWriter, r *http.Request) (name string, sort int32, raw string, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return "", 0, "", formError("Could not read the form.")
	}
	name = strings.TrimSpace(r.PostForm.Get("name"))
	raw = strings.TrimSpace(r.PostForm.Get("sort_order"))
	if name == "" || len(name) > 100 {
		return name, 0, raw, formError("Name is required (up to 100 characters).")
	}
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < -100000 || n > 100000 {
			return name, 0, raw, formError("Sort order must be a whole number.")
		}
		sort = int32(n)
	}
	return name, sort, raw, nil
}

func bouquetSaveErr(err error) (string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return "A bouquet with that name exists.", true
	}
	return "", false
}

func (ui *UI) bouquetCreate(w http.ResponseWriter, r *http.Request) {
	name, sort, raw, err := parseBouquetForm(w, r)
	if err == nil {
		var b db.Bouquet
		if b, err = ui.Store.CreateBouquet(r.Context(), db.CreateBouquetParams{Name: name, SortOrder: sort}); err == nil {
			ui.Log.Info("web: bouquet created", "bouquet_id", b.ID)
			ui.redirect(w, r, Prefix+"/bouquets/"+strconv.FormatInt(b.ID, 10)+"?msg=created")
			return
		}
		msg, ok := bouquetSaveErr(err)
		if !ok {
			ui.fail(w, "create bouquet", err)
			return
		}
		err = formError(msg)
	}
	d, derr := ui.bouquetsData(r)
	if derr != nil {
		ui.fail(w, "list bouquets", derr)
		return
	}
	d.Error, d.Name, d.Sort = err.Error(), name, raw
	ui.render(w, r, http.StatusUnprocessableEntity, "bouquets", "Bouquets", d)
}

// bouquet loads {id}, answering 404 itself when it does not exist.
func (ui *UI) bouquet(w http.ResponseWriter, r *http.Request) (db.Bouquet, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return db.Bouquet{}, false
	}
	b, err := ui.Store.GetBouquet(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return db.Bouquet{}, false
	}
	if err != nil {
		ui.fail(w, "get bouquet", err)
		return db.Bouquet{}, false
	}
	return b, true
}

// bouquetEditor is the two-pane channel chooser: members on one side,
// matching non-members on the other.
type bouquetEditor struct {
	BouquetID  int64
	Members    []db.Channel
	Candidates []db.Channel
	More       bool // more candidates than shown: narrow the search
	Query      string
	GroupID    int64
	Groups     []db.ChannelGroup
	Note       string // outcome of the last bulk action
}

func (ui *UI) editor(r *http.Request, bouquetID int64, q string, gid int64) (bouquetEditor, error) {
	ed := bouquetEditor{BouquetID: bouquetID, Query: q, GroupID: gid}
	ctx := r.Context()
	var err error
	if ed.Members, err = ui.Store.ListBouquetChannels(ctx, bouquetID); err != nil {
		return ed, err
	}
	if ed.Groups, err = ui.Store.ListChannelGroups(ctx); err != nil {
		return ed, err
	}
	member := map[int64]bool{}
	for _, c := range ed.Members {
		member[c.ID] = true
	}
	// Page through channels until enough non-members are found.
	params := db.ListChannelsParams{Limit: 500}
	if q != "" {
		pat := "%" + likeEscape(q) + "%"
		params.Search = &pat
	}
	if gid > 0 {
		params.GroupID = &gid
	}
	for {
		chans, err := ui.Store.ListChannels(ctx, params)
		if err != nil {
			return ed, err
		}
		for _, c := range chans {
			if member[c.ID] {
				continue
			}
			if len(ed.Candidates) == candidateLimit {
				ed.More = true
				return ed, nil
			}
			ed.Candidates = append(ed.Candidates, c)
		}
		if len(chans) < int(params.Limit) {
			return ed, nil
		}
		params.Offset += params.Limit
	}
}

type bouquetPageData struct {
	VOD     bouquetVOD
	Bouquet db.Bouquet
	Editor  bouquetEditor
	Users   int64
	Flash   string
	Error   string
	Name    string
	Sort    string
}

func (ui *UI) renderBouquet(w http.ResponseWriter, r *http.Request, status int, b db.Bouquet, d bouquetPageData) {
	ed, err := ui.editor(r, b.ID, "", 0)
	if err != nil {
		ui.fail(w, "bouquet editor", err)
		return
	}
	counts, err := ui.Store.BouquetCounts(r.Context())
	if err != nil {
		ui.fail(w, "bouquet counts", err)
		return
	}
	for _, c := range counts {
		if c.ID == b.ID {
			d.Users = c.Users
		}
	}
	d.Bouquet, d.Editor = b, ed
	if d.VOD, err = ui.bouquetVODData(r, b.ID); err != nil {
		ui.fail(w, "bouquet vod", err)
		return
	}
	if d.Name == "" && d.Error == "" {
		d.Name, d.Sort = b.Name, strconv.Itoa(int(b.SortOrder))
	}
	if d.Flash == "" {
		switch r.URL.Query().Get("msg") {
		case "created":
			d.Flash = "Bouquet created. Add channels, then grant it to users on the Users page."
		case "saved":
			d.Flash = "Bouquet saved."
		}
	}
	ui.render(w, r, status, "bouquet", b.Name, d)
}

func (ui *UI) bouquetPage(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	ui.renderBouquet(w, r, http.StatusOK, b, bouquetPageData{})
}

func (ui *UI) bouquetUpdate(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	name, sort, raw, err := parseBouquetForm(w, r)
	if err == nil {
		if _, err = ui.Store.UpdateBouquet(r.Context(), db.UpdateBouquetParams{ID: b.ID, Name: name, SortOrder: sort}); err == nil {
			ui.redirect(w, r, Prefix+"/bouquets/"+strconv.FormatInt(b.ID, 10)+"?msg=saved")
			return
		}
		msg, ok := bouquetSaveErr(err)
		if !ok {
			ui.fail(w, "update bouquet", err)
			return
		}
		err = formError(msg)
	}
	ui.renderBouquet(w, r, http.StatusUnprocessableEntity, b, bouquetPageData{Error: err.Error(), Name: name, Sort: raw})
}

func (ui *UI) bouquetDelete(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	if err := ui.Store.DeleteBouquet(r.Context(), b.ID); err != nil {
		ui.fail(w, "delete bouquet", err)
		return
	}
	ui.Log.Info("web: bouquet deleted", "bouquet_id", b.ID)
	ui.redirect(w, r, Prefix+"/bouquets?msg=deleted")
}

// editorFilter reads the picker's search and group from a request.
func editorFilter(r *http.Request) (string, int64) {
	gid, _ := strconv.ParseInt(r.FormValue("group"), 10, 64)
	return strings.TrimSpace(r.FormValue("q")), gid
}

func (ui *UI) renderEditor(w http.ResponseWriter, r *http.Request, b db.Bouquet, note string) {
	if r.Header.Get("HX-Request") != "true" {
		ui.redirect(w, r, Prefix+"/bouquets/"+strconv.FormatInt(b.ID, 10))
		return
	}
	q, gid := editorFilter(r)
	ed, err := ui.editor(r, b.ID, q, gid)
	if err != nil {
		ui.fail(w, "bouquet editor", err)
		return
	}
	ed.Note = note
	ui.fragment(w, r, "bouquet", "bouquet-editor", ed)
}

// bouquetEditorGet re-renders the editor for a new search or group.
func (ui *UI) bouquetEditorGet(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	ui.renderEditor(w, r, b, "")
}

func channelIDs(r *http.Request) []int64 {
	var ids []int64
	for _, v := range r.PostForm["channel"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatInt(n, 10) + " " + many
}

func (ui *UI) bouquetAdd(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ids := channelIDs(r)
	var n int64
	if len(ids) > 0 {
		var err error
		if n, err = ui.Store.AddChannelsToBouquet(r.Context(), db.AddChannelsToBouquetParams{BouquetID: b.ID, ChannelIds: ids}); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23503" {
				http.Error(w, "a selected channel no longer exists; reload the page", http.StatusConflict)
				return
			}
			ui.fail(w, "add channels", err)
			return
		}
	}
	ui.renderEditor(w, r, b, "Added "+plural(n, "channel", "channels")+".")
}

// bouquetAddGroup adds every channel of a group ("0" = channels without a
// group), including ones not shown in the picker.
func (ui *UI) bouquetAddGroup(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	gid, err := strconv.ParseInt(r.FormValue("add_group"), 10, 64)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p := db.AddGroupToBouquetParams{BouquetID: b.ID}
	if gid > 0 {
		p.GroupID = &gid
	}
	n, err := ui.Store.AddGroupToBouquet(r.Context(), p)
	if err != nil {
		ui.fail(w, "add group", err)
		return
	}
	ui.renderEditor(w, r, b, "Added "+plural(n, "channel", "channels")+" from the group.")
}

func (ui *UI) bouquetRemove(w http.ResponseWriter, r *http.Request) {
	b, ok := ui.bouquet(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var n int64
	if ids := channelIDs(r); len(ids) > 0 {
		var err error
		if n, err = ui.Store.RemoveChannelsFromBouquet(r.Context(), db.RemoveChannelsFromBouquetParams{BouquetID: b.ID, ChannelIds: ids}); err != nil {
			ui.fail(w, "remove channels", err)
			return
		}
	}
	ui.renderEditor(w, r, b, "Removed "+plural(n, "channel", "channels")+".")
}
