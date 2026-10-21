package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

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
	if q := r.URL.Query(); q.Get("msg") == "saved" {
		d.Flash = "Groups saved."
		if q.Has("created") {
			d.Flash += fmt.Sprintf(" Matching created %d channels and added %d streams to channels.",
				atoi(q.Get("created")), atoi(q.Get("attached")))
		} else if q.Get("match") == "failed" {
			d.Flash += " Matching failed; try again from the Match page."
		}
	}
	ui.render(w, r, http.StatusOK, "provider_groups", p.Name+": groups", d)
}

// autoMatchTimeout bounds the match run of "Save and match".
const autoMatchTimeout = 2 * time.Minute

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
	back := url.Values{"msg": {"saved"}}
	// "Save and match" matches the chosen groups now with the Settings
	// page's options, even when automatic matching is off; a plain save
	// leaves matching to the Match page or the next refresh.
	if r.PostForm.Get("action") == "match" {
		ctx, cancel := contextFor(r, autoMatchTimeout)
		defer cancel()
		if res, err := ui.Match.MatchNow(ctx, p.ID); err != nil {
			ui.Log.Error("web: match after saving groups", "provider_id", p.ID, "err", err)
			back.Set("match", "failed")
		} else {
			back.Set("created", strconv.Itoa(res.Created))
			back.Set("attached", strconv.Itoa(res.Attached))
		}
	}
	ui.redirect(w, r, Prefix+"/providers/"+strconv.FormatInt(p.ID, 10)+"/groups?"+back.Encode())
}
