package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/gluetun"
)

// VPN is gluetun behind MMP (docker-compose.vpn.yml): Settings → VPN shows
// the public IP and changes the server location.
type VPN struct {
	Client  *gluetun.Client
	Keeper  *gluetun.Keeper
	Servers *gluetun.ServerList // the location lists; optional
}

func (ui *UI) mountVPN(r chi.Router) {
	r.Get("/vpn", ui.vpnPage)
	r.Get("/vpn/status", ui.vpnStatusFragment)
	r.Post("/vpn/location", ui.vpnLocation)
	r.Post("/vpn/reconnect", ui.vpnReconnect)
	r.Post("/vpn/default", ui.vpnDefault)
}

// vpnStatus is the live part of the page (refreshed every 10 seconds).
type vpnStatus struct {
	IP       gluetun.PublicIP
	State    string // running, starting, stopped, crashed…
	Err      string // gluetun could not be asked
	NoKey    bool   // gluetun wants an API key MMP lacks
	Location string // where gluetun connects now ("" unknown)
}

// StateLabel and StateClass describe the tunnel for people (a pill).
func (s vpnStatus) StateLabel() string {
	switch s.State {
	case "running":
		if s.IP.IP == "" {
			return "Connected, looking up the IP"
		}
		return "Connected"
	case "starting":
		return "Connecting"
	case "stopping":
		return "Disconnecting"
	case "stopped":
		return "Stopped: internet blocked"
	case "crashed":
		return "Down: internet blocked"
	}
	return s.State
}

func (s vpnStatus) StateClass() string {
	switch s.State {
	case "running":
		if s.IP.IP == "" {
			return "warn"
		}
		return "ok"
	case "starting", "stopping":
		return "warn"
	}
	return "bad"
}

type vpnData struct {
	Status   vpnStatus
	Settings gluetun.Settings
	Saved    gluetun.Saved
	Choices  gluetun.Choices
	// Picked fills the form (the saved location, the one gluetun has, or
	// what was just submitted).
	Country, City, Region string
	Flash, Error          string
	// Force: gluetun was still connecting; the form offers to change
	// the location anyway (a location that never connects needs it).
	Force bool
}

// cityGroup is one country's cities, for an <optgroup>.
type cityGroup struct {
	Country string
	Cities  []gluetun.City
}

// CityGroups are the cities by country (the list is sorted by country).
func (d vpnData) CityGroups() []cityGroup {
	var out []cityGroup
	for _, c := range d.Choices.Cities {
		if len(out) == 0 || out[len(out)-1].Country != c.Country {
			out = append(out, cityGroup{Country: c.Country})
		}
		out[len(out)-1].Cities = append(out[len(out)-1].Cities, c)
	}
	return out
}

// TypeLabel is the VPN type as people write it.
func (d vpnData) TypeLabel() string {
	switch strings.ToLower(d.Settings.Type) {
	case "wireguard":
		return "WireGuard"
	case "openvpn":
		return "OpenVPN"
	}
	return d.Settings.Type
}

// status asks gluetun for the public IP, tunnel state and settings at
// once (a few seconds at most: it is local).
func (ui *UI) vpnStatus(ctx context.Context) (vpnStatus, gluetun.Settings, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var (
		st                 vpnStatus
		set                gluetun.Settings
		ipErr, stErr, sErr error
		wg                 sync.WaitGroup
	)
	wg.Add(3)
	go func() { defer wg.Done(); st.IP, ipErr = ui.VPN.Client.PublicIP(ctx) }()
	go func() { defer wg.Done(); st.State, stErr = ui.VPN.Client.Status(ctx) }()
	go func() { defer wg.Done(); set, sErr = ui.VPN.Client.Settings(ctx) }()
	wg.Wait()
	for _, err := range []error{stErr, ipErr, sErr} {
		if errors.Is(err, gluetun.ErrUnauthorized) {
			st.NoKey = true
		} else if err != nil && st.Err == "" {
			st.Err = err.Error()
		}
	}
	if st.NoKey && st.Err == "" && st.State == "" { // only the location needs the key otherwise
		st.Err = gluetun.ErrUnauthorized.Error()
	}
	if sErr == nil {
		st.Location = set.Location.String()
	}
	return st, set, sErr
}

func (ui *UI) vpnData(r *http.Request) (vpnData, error) {
	var d vpnData
	var setErr error
	d.Status, d.Settings, setErr = ui.vpnStatus(r.Context())
	var err error
	if d.Saved, err = ui.VPN.Keeper.Load(r.Context()); err != nil {
		return d, err
	}
	if setErr == nil {
		if d.Choices, err = ui.VPN.Servers.Choices(d.Settings.Provider, d.Settings.Type); err != nil {
			ui.Log.Warn("web: gluetun server list", "err", err)
		}
	}
	loc := d.Settings.Location
	if d.Saved.Active {
		loc = d.Saved.Location
	}
	d.Country, d.City, d.Region = first(loc.Countries), first(loc.Cities), first(loc.Regions)
	return d, nil
}

func first(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

var vpnFlashes = map[string]string{
	"location":  "Location changed. Gluetun has reconnected; the public IP below updates in a few seconds.",
	"reconnect": "Reconnected. The public IP below updates in a few seconds.",
	"default":   "The location is back to what .env sets.",
}

func (ui *UI) vpnPage(w http.ResponseWriter, r *http.Request) {
	if ui.VPN == nil {
		http.NotFound(w, r)
		return
	}
	d, err := ui.vpnData(r)
	if err != nil {
		ui.fail(w, "vpn", err)
		return
	}
	d.Flash = vpnFlashes[r.URL.Query().Get("msg")]
	ui.render(w, r, http.StatusOK, "vpn", "VPN", d)
}

func (ui *UI) vpnStatusFragment(w http.ResponseWriter, r *http.Request) {
	if ui.VPN == nil {
		http.NotFound(w, r)
		return
	}
	st, _, _ := ui.vpnStatus(r.Context())
	ui.fragment(w, r, "vpn", "vpn-status", st)
}

// vpnError shows the page again with a message (and the submitted form).
func (ui *UI) vpnError(w http.ResponseWriter, r *http.Request, msg string, keepForm bool) {
	d, err := ui.vpnData(r)
	if err != nil {
		ui.fail(w, "vpn", err)
		return
	}
	if keepForm {
		f := r.PostForm
		d.Country, d.City, d.Region = f.Get("country"), f.Get("city"), f.Get("region")
	}
	d.Error = msg
	ui.render(w, r, http.StatusUnprocessableEntity, "vpn", "VPN", d)
}

func (ui *UI) vpnLocation(w http.ResponseWriter, r *http.Request) {
	if ui.VPN == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	country, city, region := strings.TrimSpace(f.Get("country")), strings.TrimSpace(f.Get("city")), strings.TrimSpace(f.Get("region"))
	// The city list names each city's country: a city alone takes it, and
	// one from another country than the one chosen would match no server.
	if city != "" {
		set, err := ui.VPN.Client.Settings(r.Context())
		if err == nil {
			c, _ := ui.VPN.Servers.Choices(set.Provider, set.Type)
			if cc, ok := cityCountry(c, city, country); ok && country == "" {
				country = cc
			} else if !ok {
				msg := "Gluetun has no " + set.Provider + " servers in " + city + "."
				if country != "" {
					msg = city + " is not in " + country + ". Choose a city in that country, or any city."
				}
				ui.vpnError(w, r, msg, true)
				return
			}
		}
	}
	var loc gluetun.Location
	for _, p := range []struct {
		v   string
		dst *[]string
	}{{country, &loc.Countries}, {city, &loc.Cities}, {region, &loc.Regions}} {
		if p.v != "" {
			*p.dst = []string{p.v}
		}
	}
	if msg, forceable := ui.vpnBusy(r.Context()); msg != "" && (!forceable || f.Get("force") != "1") {
		ui.vpnBusyError(w, r, msg, forceable, true)
		return
	}
	outcome, err := ui.VPN.Keeper.Set(r.Context(), loc)
	if err != nil {
		ui.Log.Warn("web: vpn location refused", "location", loc.String(), "err", err)
		ui.vpnError(w, r, "Gluetun did not take that location: "+err.Error(), true)
		return
	}
	ui.Log.Info("web: vpn location changed", "location", loc.String(), "gluetun", outcome)
	ui.redirect(w, r, Prefix+"/vpn?msg=location")
}

// vpnBusy says why gluetun should not get a new location now ("" when
// it can). Mid-change (starting, stopping) it would hang or wedge gluetun;
// while gluetun is still trying servers (running but unhealthy) a change
// can wedge it too, but it may never connect, so that one can be forced.
func (ui *UI) vpnBusy(ctx context.Context) (msg string, forceable bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := ui.VPN.Client.Status(ctx)
	if err != nil {
		return "", false // the change itself reports gluetun's error
	}
	if gluetun.Transitional(st) {
		return "Gluetun is " + st + " the tunnel. Try again in a few seconds. If it stays " + st +
			", recreate it: docker compose -f docker-compose.vpn.yml up -d --force-recreate gluetun mmp", false
	}
	if ok, err := ui.VPN.Client.Healthy(ctx); err == nil && !ok {
		return "Gluetun is still looking for a server that answers (its health check fails). " +
			"Changing the location now can leave it stuck, so wait a moment, or change it anyway.", true
	}
	return "", false
}

func (ui *UI) vpnBusyError(w http.ResponseWriter, r *http.Request, msg string, forceable, keepForm bool) {
	ui.Log.Info("web: vpn change held back", "reason", msg)
	d, err := ui.vpnData(r)
	if err != nil {
		ui.fail(w, "vpn", err)
		return
	}
	if keepForm {
		f := r.PostForm
		d.Country, d.City, d.Region = f.Get("country"), f.Get("city"), f.Get("region")
	}
	d.Error, d.Force = msg, forceable
	ui.render(w, r, http.StatusConflict, "vpn", "VPN", d)
}

// cityCountry finds city in the choices (in country, when one is given)
// and returns its country. With no city list, anything passes.
func cityCountry(c gluetun.Choices, city, country string) (string, bool) {
	if len(c.Cities) == 0 {
		return "", true
	}
	for _, x := range c.Cities {
		if strings.EqualFold(x.Name, city) && (country == "" || strings.EqualFold(x.Country, country)) {
			return x.Country, true
		}
	}
	return "", false
}

func (ui *UI) vpnReconnect(w http.ResponseWriter, r *http.Request) {
	if ui.VPN == nil {
		http.NotFound(w, r)
		return
	}
	// Reconnecting is the way out of a failing location, so only a
	// change already under way stops it.
	if msg, forceable := ui.vpnBusy(r.Context()); msg != "" && !forceable {
		ui.vpnBusyError(w, r, msg, false, false)
		return
	}
	if err := ui.VPN.Client.Reconnect(r.Context()); err != nil {
		ui.vpnError(w, r, "Reconnecting failed: "+err.Error(), false)
		return
	}
	ui.Log.Info("web: vpn reconnected")
	ui.redirect(w, r, Prefix+"/vpn?msg=reconnect")
}

func (ui *UI) vpnDefault(w http.ResponseWriter, r *http.Request) {
	if ui.VPN == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if msg, forceable := ui.vpnBusy(r.Context()); msg != "" && (!forceable || r.PostForm.Get("force") != "1") {
		ui.vpnBusyError(w, r, msg, forceable, false)
		return
	}
	outcome, err := ui.VPN.Keeper.UseDefault(r.Context())
	if err != nil {
		ui.vpnError(w, r, "Going back to the .env location failed: "+err.Error(), false)
		return
	}
	ui.Log.Info("web: vpn location back to .env", "gluetun", outcome)
	ui.redirect(w, r, Prefix+"/vpn?msg=default")
}
