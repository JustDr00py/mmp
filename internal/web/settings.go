package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/eventroute"
	"github.com/JustDr00py/mmp/internal/events"
	"github.com/JustDr00py/mmp/internal/nameguide"
	"github.com/JustDr00py/mmp/internal/output"
)

// The Settings page holds settings that apply while MMP runs (stored in
// the database, so backups keep them). The configuration file and .env
// only provide their values on the first start.

func (ui *UI) mountSettings(r chi.Router) {
	r.Get("/settings", ui.settingsPage)
	r.Post("/settings/match", ui.settingsMatchSave)
	r.Post("/settings/prefixes", ui.settingsPrefixesSave)
	r.Post("/settings/events", ui.settingsEventsSave)
	r.Post("/settings/nameguide", ui.settingsNameGuideSave)
	r.Post("/settings/routes", ui.settingsRoutesSave)
	r.Post("/settings/groupicons", ui.settingsGroupIconsSave)
}

type settingsData struct {
	Match         channel.MatchSettings
	Prefixes      string // ignored name prefixes, one per line
	Events        events.Retention
	NameGuide     nameguide.Settings
	GroupIcons    output.GroupIconSettings
	Routes        eventroute.Settings
	RouteSource   map[int64]bool // ticked game groups (the first rule's)
	RouteTarget   map[int64]bool // ticked team groups
	RouteResult   *eventroute.Result
	Zones         []string // suggestions for the name guide's zone
	ChannelGroups []db.ChannelGroup
	Bouquets      []db.Bouquet
	Picked        map[int64]bool // ticked bouquets
	GroupMissing  bool           // the chosen group was deleted
	Flash         string
	Error         string
}

// GroupValue is the group select's value for the settings.
func (d settingsData) GroupValue() string {
	if d.Match.Group == channel.GroupFixed {
		return strconv.FormatInt(d.Match.GroupID, 10)
	}
	return d.Match.Group
}

func (ui *UI) settingsData(r *http.Request, m *channel.MatchSettings) (settingsData, error) {
	var d settingsData
	var err error
	if m != nil {
		d.Match = *m
	} else if d.Match, err = ui.Match.MatchSettings(r.Context()); err != nil {
		return d, err
	}
	if d.Events, err = events.LoadRetention(r.Context(), ui.Store); err != nil {
		return d, err
	}
	prefixes, err := channel.LoadIgnorePrefixes(r.Context(), ui.Store)
	if err != nil {
		return d, err
	}
	d.Prefixes = strings.Join(prefixes, "\n")
	if d.NameGuide, err = nameguide.Load(r.Context(), ui.Store); err != nil {
		return d, err
	}
	d.Zones = commonZones
	if d.GroupIcons, err = output.LoadGroupIcons(r.Context(), ui.Store); err != nil {
		return d, err
	}
	if d.Routes, err = eventroute.Load(r.Context(), ui.Store); err != nil {
		return d, err
	}
	d.RouteSource, d.RouteTarget = map[int64]bool{}, map[int64]bool{}
	if len(d.Routes.Rules) > 0 {
		for _, id := range d.Routes.Rules[0].SourceGroupIDs {
			d.RouteSource[id] = true
		}
		for _, id := range d.Routes.Rules[0].TargetGroupIDs {
			d.RouteTarget[id] = true
		}
	}
	if d.ChannelGroups, err = ui.Store.ListChannelGroups(r.Context()); err != nil {
		return d, err
	}
	if d.Bouquets, err = ui.Store.ListBouquets(r.Context()); err != nil {
		return d, err
	}
	d.Picked = map[int64]bool{}
	for _, id := range d.Match.BouquetIDs {
		d.Picked[id] = true
	}
	if d.Match.Group == channel.GroupFixed {
		d.GroupMissing = !slices.ContainsFunc(d.ChannelGroups, func(g db.ChannelGroup) bool { return g.ID == d.Match.GroupID })
	}
	return d, nil
}

func (ui *UI) settingsPage(w http.ResponseWriter, r *http.Request) {
	d, err := ui.settingsData(r, nil)
	if err != nil {
		ui.fail(w, "settings", err)
		return
	}
	switch r.URL.Query().Get("msg") {
	case "match":
		d.Flash = "Matching settings saved. They apply from the next provider refresh."
	case "prefixes":
		d.Flash = "Name prefixes saved. Match uses them now; automatic matching from the next provider refresh."
	case "events":
		d.Flash = "History setting saved. Older events and activity are deleted within the hour."
	case "nameguide":
		d.Flash = "Stream-name guide saved. Players get it the next time they load the guide."
	case "routes":
		d.Flash = "Game routing saved."
	case "groupicons":
		d.Flash = "Group icon setting saved. Players get it the next time they load their categories."
	}
	ui.render(w, r, http.StatusOK, "settings", "Settings", d)
}

func (ui *UI) settingsMatchSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	m := channel.MatchSettings{Auto: f.Get("auto") != "", NewChannels: f.Get("new_channels") != "",
		Group: f.Get("group"), Bouquets: f.Get("bouquets")}
	m.MinProviders, _ = strconv.Atoi(f.Get("min_providers"))
	if id, err := strconv.ParseInt(m.Group, 10, 64); err == nil {
		m.Group, m.GroupID = channel.GroupFixed, id
	}
	for _, v := range f["bq"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && !slices.Contains(m.BouquetIDs, id) {
			m.BouquetIDs = append(m.BouquetIDs, id)
		}
	}
	d, err := ui.settingsData(r, &m)
	if err != nil {
		ui.fail(w, "settings", err)
		return
	}
	if err = m.Validate(); err == nil && d.GroupMissing {
		err = formError("That group no longer exists. Choose another.")
	}
	if err != nil {
		d.Error = err.Error()
		ui.render(w, r, http.StatusUnprocessableEntity, "settings", "Settings", d)
		return
	}
	if err := ui.Match.SaveMatchSettings(r.Context(), m); err != nil {
		ui.fail(w, "save match settings", err)
		return
	}
	ui.Log.Info("web: match settings saved", "auto", m.Auto, "new_channels", m.NewChannels)
	ui.redirect(w, r, Prefix+"/settings?msg=match")
}

func (ui *UI) settingsPrefixesSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	prefixes := channel.ParseIgnorePrefixes(r.PostForm.Get("prefixes"))
	if err := channel.ValidateIgnorePrefixes(prefixes); err != nil {
		d, derr := ui.settingsData(r, nil)
		if derr != nil {
			ui.fail(w, "settings", derr)
			return
		}
		d.Prefixes, d.Error = r.PostForm.Get("prefixes"), "Name prefixes: "+err.Error()+"."
		ui.render(w, r, http.StatusUnprocessableEntity, "settings", "Settings", d)
		return
	}
	if err := channel.SaveIgnorePrefixes(r.Context(), ui.Store, prefixes); err != nil {
		ui.fail(w, "save name prefixes", err)
		return
	}
	ui.Log.Info("web: ignored name prefixes saved", "count", len(prefixes))
	ui.redirect(w, r, Prefix+"/settings?msg=prefixes")
}

func (ui *UI) settingsEventsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ret := events.Retention{KeepDays: atoi(strings.TrimSpace(r.PostForm.Get("keep_days")))}
	if err := ret.Validate(); err != nil {
		d, derr := ui.settingsData(r, nil)
		if derr != nil {
			ui.fail(w, "settings", derr)
			return
		}
		d.Events, d.Error = ret, err.Error()
		ui.render(w, r, http.StatusUnprocessableEntity, "settings", "Settings", d)
		return
	}
	if err := events.SaveRetention(r.Context(), ui.Store, ret); err != nil {
		ui.fail(w, "save event retention", err)
		return
	}
	ui.Log.Info("web: event retention saved", "keep_days", ret.KeepDays)
	ui.redirect(w, r, Prefix+"/settings?msg=events")
}

// commonZones are offered for the stream-name guide's zone (any IANA name
// works).
var commonZones = []string{"UTC", "Europe/London", "Europe/Paris", "Europe/Berlin", "Europe/Stockholm",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles", "Australia/Sydney"}

func (ui *UI) settingsNameGuideSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	s := nameguide.Settings{Enabled: f.Get("enabled") != "", All: f.Get("all") != "", Zone: strings.TrimSpace(f.Get("zone"))}
	if err := s.Validate(); err != nil {
		d, derr := ui.settingsData(r, nil)
		if derr != nil {
			ui.fail(w, "settings", derr)
			return
		}
		d.NameGuide, d.Error = s, err.Error()
		ui.render(w, r, http.StatusUnprocessableEntity, "settings", "Settings", d)
		return
	}
	if err := nameguide.Save(r.Context(), ui.Store, s); err != nil {
		ui.fail(w, "save stream-name guide", err)
		return
	}
	ui.Log.Info("web: stream-name guide saved", "enabled", s.Enabled, "all", s.All, "zone", s.Zone)
	ui.redirect(w, r, Prefix+"/settings?msg=nameguide")
}

func (ui *UI) settingsRoutesSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	s := eventroute.Settings{Enabled: f.Get("enabled") != ""}
	s.BeforeMins, _ = strconv.Atoi(f.Get("before"))
	s.AfterMins, _ = strconv.Atoi(f.Get("after"))
	var rule eventroute.Rule
	for _, v := range f["source"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			rule.SourceGroupIDs = append(rule.SourceGroupIDs, id)
		}
	}
	for _, v := range f["target"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			rule.TargetGroupIDs = append(rule.TargetGroupIDs, id)
		}
	}
	if len(rule.SourceGroupIDs) > 0 || len(rule.TargetGroupIDs) > 0 {
		s.Rules = []eventroute.Rule{rule}
	}
	fail := func(status int, msg string) {
		d, derr := ui.settingsData(r, nil)
		if derr != nil {
			ui.fail(w, "settings", derr)
			return
		}
		d.Routes, d.Error = s, msg
		d.RouteSource, d.RouteTarget = map[int64]bool{}, map[int64]bool{}
		for _, id := range rule.SourceGroupIDs {
			d.RouteSource[id] = true
		}
		for _, id := range rule.TargetGroupIDs {
			d.RouteTarget[id] = true
		}
		ui.render(w, r, status, "settings", "Settings", d)
	}
	if err := s.Validate(); err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := eventroute.Save(r.Context(), ui.Store, s); err != nil {
		ui.fail(w, "save game routing", err)
		return
	}
	ui.Log.Info("web: game routing saved", "enabled", s.Enabled, "rules", s.Rules,
		"before_mins", s.BeforeMins, "after_mins", s.AfterMins)
	if ui.Routes == nil {
		ui.redirect(w, r, Prefix+"/settings?msg=routes")
		return
	}
	res, err := ui.Routes.Run(r.Context(), time.Now()) // apply at once rather than at the next tick
	if err != nil {
		fail(http.StatusInternalServerError, "Saved, but routing failed: "+err.Error())
		return
	}
	d, err := ui.settingsData(r, nil)
	if err != nil {
		ui.fail(w, "settings", err)
		return
	}
	d.RouteResult = &res
	d.Flash = "Game routing saved and run."
	ui.render(w, r, http.StatusOK, "settings", "Settings", d)
}

func (ui *UI) settingsGroupIconsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s := output.GroupIconSettings{Enabled: r.PostForm.Get("enabled") != ""}
	if err := output.SaveGroupIcons(r.Context(), ui.Store, s); err != nil {
		ui.fail(w, "save group icon settings", err)
		return
	}
	ui.Log.Info("web: group icon setting saved", "enabled", s.Enabled)
	ui.redirect(w, r, Prefix+"/settings?msg=groupicons")
}
