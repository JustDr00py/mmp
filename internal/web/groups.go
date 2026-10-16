package web

import (
	"net/http"
	"strconv"

	"github.com/JustDr00py/mmp/internal/db"
)

// groupsData is the provider groups page: which of a provider's stream
// groups are offered for matching and in the stream picker.
type groupsData struct {
	Provider db.Provider
	Groups   []db.ListProviderGroupsRow
	Selected int
	Flash    string
}

func (ui *UI) providerGroupsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	gs, err := ui.Store.ListProviderGroups(r.Context(), p.ID)
	if err != nil {
		ui.fail(w, "list groups", err)
		return
	}
	d := groupsData{Provider: p, Groups: gs}
	for _, g := range gs {
		if g.Selected {
			d.Selected++
		}
	}
	if r.URL.Query().Get("msg") == "saved" {
		d.Flash = "Groups saved."
	}
	ui.render(w, r, http.StatusOK, "provider_groups", p.Name+": groups", d)
}

// providerGroupsSave stores the selection: mode=all offers every group
// (the selection is kept for later), mode=selected only the ticked ones.
func (ui *UI) providerGroupsSave(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.provider(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	filter := r.PostForm.Get("mode") == "selected"
	err := ui.inTx(r.Context(), func(q Store) error {
		if filter || len(r.PostForm["group"]) > 0 {
			if err := q.ClearProviderGroups(r.Context(), p.ID); err != nil {
				return err
			}
			if err := q.AddProviderGroups(r.Context(), db.AddProviderGroupsParams{ProviderID: p.ID, GroupTitles: r.PostForm["group"]}); err != nil {
				return err
			}
		}
		return q.SetProviderGroupFilter(r.Context(), db.SetProviderGroupFilterParams{ID: p.ID, GroupFilter: filter})
	})
	if err != nil {
		ui.fail(w, "save groups", err)
		return
	}
	ui.Log.Info("web: provider groups saved", "provider_id", p.ID, "filter", filter, "groups", len(r.PostForm["group"]))
	ui.redirect(w, r, Prefix+"/providers/"+strconv.FormatInt(p.ID, 10)+"/groups?msg=saved")
}
