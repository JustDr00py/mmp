package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// providerFilter is the providers table's filter state.
type providerFilter struct {
	Query    string // in the name, URL or backbone
	Backbone string // "" = any, source.NoBackbone = none, else the label
	Status   string // "" = any, "enabled" or "disabled"
	Offset   int
}

func parseProviderFilter(v url.Values) providerFilter {
	f := providerFilter{Query: strings.TrimSpace(v.Get("q")), Backbone: v.Get("backbone"), Offset: max(atoi(v.Get("offset")), 0)}
	if s := v.Get("status"); s == "enabled" || s == "disabled" {
		f.Status = s
	}
	return f
}

// Active reports whether the filter hides anything.
func (f providerFilter) Active() bool { return f.Query != "" || f.Backbone != "" || f.Status != "" }

func (f providerFilter) keep(p db.Provider) bool {
	switch {
	case f.Backbone == source.NoBackbone && p.Backbone != "",
		f.Backbone != "" && f.Backbone != source.NoBackbone && p.Backbone != f.Backbone,
		f.Status == "enabled" && !p.Enabled,
		f.Status == "disabled" && p.Enabled:
		return false
	}
	if f.Query == "" {
		return true
	}
	hay := strings.ToLower(p.Name + "\n" + p.Url + "\n" + p.Backbone)
	for _, t := range strings.Fields(strings.ToLower(f.Query)) {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// Values encodes the filter (without the offset) for links and redirects.
func (f providerFilter) Values() url.Values {
	v := url.Values{}
	if f.Query != "" {
		v.Set("q", f.Query)
	}
	if f.Backbone != "" {
		v.Set("backbone", f.Backbone)
	}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	return v
}

// PageURL links to the providers page at offset with the same filter.
func (f providerFilter) PageURL(offset int) string {
	v := f.Values()
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	if len(v) == 0 {
		return Prefix + "/providers"
	}
	return Prefix + "/providers?" + v.Encode()
}

// bulkActions are the bulk form's actions and how the flash says them.
var bulkActions = map[string]string{
	"backbone": "Set the backbone of", "enable": "Enabled", "disable": "Disabled",
	"priority": "Set the priority of", "refresh": "Refreshing",
}

// bulkFlash is the message after a bulk action (?msg=bulk&act=&n=), from
// fixed text and numbers only.
func bulkFlash(q url.Values) (string, bool) {
	switch q.Get("msg") {
	case "bulk":
	case "busy":
		return "A bulk refresh is already running; try again when it finishes.", true
	case "none":
		return "Select at least one provider first.", true
	case "aligned":
		if atoi(q.Get("n")) == 0 {
			return "That backbone's providers already share one priority.", true
		}
		return "Gave " + plural(int64(atoi(q.Get("n"))), "provider", "providers") + " their backbone's best priority, " +
			strconv.Itoa(atoi(q.Get("p"))) + ".", true
	default:
		return "", false
	}
	verb, ok := bulkActions[q.Get("act")]
	if !ok {
		return "", false
	}
	msg := verb + " " + plural(int64(atoi(q.Get("n"))), "provider", "providers") + "."
	if q.Get("act") == "refresh" {
		msg = msg[:len(msg)-1] + " one after another in the background. Reload to see their last refresh."
	}
	return msg, true
}

// providersBulk applies one action to the selected providers, or with
// scope=all to every provider passing the posted filter.
func (ui *UI) providersBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	pf := r.PostForm
	f := parseProviderFilter(pf)
	back := func(v url.Values) {
		for k, vs := range f.Values() {
			if !v.Has(k) {
				v[k] = vs
			}
		}
		ui.redirect(w, r, Prefix+"/providers?"+v.Encode())
	}
	act := pf.Get("action")
	if _, ok := bulkActions[act]; !ok {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var ids []int64
	if pf.Get("scope") == "all" {
		ps, err := ui.Store.ListProviders(ctx)
		if err != nil {
			ui.fail(w, "list providers", err)
			return
		}
		for _, p := range ps {
			if f.keep(p) {
				ids = append(ids, p.ID)
			}
		}
	} else {
		seen := map[int64]bool{}
		for _, v := range pf["provider"] {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		back(url.Values{"msg": {"none"}})
		return
	}
	done := url.Values{"msg": {"bulk"}, "act": {act}}
	var n int64
	var err error
	switch act {
	case "backbone":
		label, perr := pickedBackbone(pf, "backbone_to")
		if perr != nil {
			http.Error(w, perr.Error(), http.StatusBadRequest)
			return
		}
		if label, err = ui.backboneLabel(ctx, label); err != nil {
			if errors.Is(err, source.ErrBackboneTooLong) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			break
		}
		n, err = ui.Store.SetProvidersBackbone(ctx, db.SetProvidersBackboneParams{Backbone: label, Ids: ids})
		// Show where they went, unless the list was not filtered by backbone.
		if f.Backbone != "" {
			done.Set("backbone", label)
			if label == "" {
				done.Set("backbone", source.NoBackbone)
			}
		}
	case "enable", "disable":
		n, err = ui.Store.SetProvidersEnabled(ctx, db.SetProvidersEnabledParams{Enabled: act == "enable", Ids: ids})
	case "priority":
		p, perr := strconv.Atoi(strings.TrimSpace(pf.Get("priority_to")))
		if perr != nil || p < -1_000_000 || p > 1_000_000 {
			http.Error(w, "priority must be a whole number", http.StatusBadRequest)
			return
		}
		n, err = ui.Store.SetProvidersPriority(ctx, db.SetProvidersPriorityParams{Priority: int32(p), Ids: ids})
		if err == nil && pf.Get("resort_channels") == "on" {
			var rows int64
			if rows, err = ui.Store.SortChannelStreamsByProvider(ctx, db.SortChannelStreamsByProviderParams{ProviderIds: ids}); err == nil {
				ui.Log.Info("web: channel streams sorted", "providers", len(ids), "rows", rows)
			}
		}
	case "refresh":
		var ps []db.Provider
		if ps, err = ui.Store.GetProvidersByIDs(ctx, ids); err != nil {
			break
		}
		if !ui.bulkRefreshing.CompareAndSwap(false, true) {
			back(url.Values{"msg": {"busy"}})
			return
		}
		n = int64(len(ps))
		go ui.refreshAll(context.WithoutCancel(ctx), ps)
	}
	if err != nil {
		ui.fail(w, "bulk "+act, err)
		return
	}
	ui.Log.Info("web: providers bulk", "action", act, "providers", n)
	done.Set("n", strconv.FormatInt(n, 10))
	back(done)
}

// providersAlignBackbone gives every provider on one backbone that
// backbone's best priority and, if asked, re-sorts the channels using the
// providers it changed.
func (ui *UI) providersAlignBackbone(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.PostForm.Get("align"))
	if label == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var changed []db.AlignBackbonePriorityRow
	err := ui.inTx(ctx, func(q Store) error {
		var err error
		if changed, err = q.AlignBackbonePriority(ctx, label); err != nil || len(changed) == 0 || r.PostForm.Get("resort_channels") != "on" {
			return err
		}
		ids := make([]int64, len(changed))
		for i, c := range changed {
			ids[i] = c.ID
		}
		n, err := q.SortChannelStreamsByProvider(ctx, db.SortChannelStreamsByProviderParams{ProviderIds: ids})
		ui.Log.Info("web: channel streams sorted", "providers", len(ids), "rows", n)
		return err
	})
	if err != nil {
		ui.fail(w, "align backbone", err)
		return
	}
	v := parseProviderFilter(r.PostForm).Values()
	v.Set("msg", "aligned")
	v.Set("n", strconv.Itoa(len(changed)))
	if len(changed) > 0 {
		v.Set("p", strconv.Itoa(int(changed[0].Priority)))
	}
	ui.Log.Info("web: backbone priority aligned", "providers", len(changed))
	ui.redirect(w, r, Prefix+"/providers?"+v.Encode())
}

// refreshAll refreshes ps one at a time (as the scheduler would), so a
// bulk refresh never opens hundreds of provider connections at once.
func (ui *UI) refreshAll(ctx context.Context, ps []db.Provider) {
	defer ui.bulkRefreshing.Store(false)
	start := time.Now()
	var failed int
	for _, p := range ps {
		pctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		if _, err := ui.Refresh.RefreshProvider(pctx, p, false); err != nil {
			failed++
			ui.Log.Warn("web: bulk refresh", "provider_id", p.ID, "err", err) // refresher errors are redacted
		}
		cancel()
	}
	ui.Log.Info("web: bulk refresh done", "providers", len(ps), "failed", failed, "elapsed", time.Since(start).Round(time.Second))
}
