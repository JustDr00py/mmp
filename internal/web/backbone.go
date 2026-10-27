package web

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/health"
)

// Backbone detail: why a backbone scores as it does. Its outcomes are the
// ones recorded under its label (the backbone each provider had at the
// time), so the page agrees with the backbone health table.

// worstStreamRows caps the backbone page's failing streams.
const worstStreamRows = 10

type backboneData struct {
	Name      string
	Health    healthBadge
	Providers []backboneProvider
	Outcomes  []outcomeShare
	Days      []backboneDay
	Streams   []backboneStream
}

// backboneProvider is a provider that is, or was, on the backbone.
type backboneProvider struct {
	ID      int64
	Name    string // "" once deleted
	Enabled bool
	NowOn   string // its backbone now, when it moved off this one
	Gone    bool   // deleted since
	Health  healthBadge
}

// outcomeShare is how many attempts of one kind the backbone had.
type outcomeShare struct {
	Label   string
	Failure bool
	Count   int64
	Percent int // of all attempts
}

// backboneDay is one day of attempts.
type backboneDay struct {
	Day      time.Time
	Attempts int64
	Failures int64
	Percent  int // failed, of the day's attempts
}

// backboneStream is a stream that fails often on the backbone.
type backboneStream struct {
	ID           int64
	Name         string
	ProviderID   int64
	ProviderName string
	Health       healthBadge
}

// outcomeLabels names the outcomes, in the order the page lists them.
var outcomeLabels = []struct{ outcome, label string }{
	{health.OutcomeOpenOK, "Opened"},
	{health.OutcomeOpenFailed, "Failed to open"},
	{health.OutcomeMidPlayFailure, "Failed while playing"},
	{health.OutcomeProbeOK, "Probe worked"},
	{health.OutcomeProbeFail, "Probe failed"},
}

func (ui *UI) backbonePage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	all, err := ui.outcomeBuckets(r.Context(), now)
	if err != nil {
		ui.fail(w, "outcomes", err)
		return
	}
	buckets := slices.DeleteFunc(all, func(b health.Bucket) bool { return b.Backbone != name })
	providers, err := ui.Store.ListProviders(r.Context())
	if err != nil {
		ui.fail(w, "list providers", err)
		return
	}
	onIt := slices.ContainsFunc(providers, func(p db.Provider) bool { return p.Backbone == name })
	if !onIt && len(buckets) == 0 {
		http.NotFound(w, r)
		return
	}
	d := backboneData{Name: name, Health: badge(health.Compute(buckets, now))}

	// Providers: those on it now, and those with outcomes under it.
	scores := health.ByKey(buckets, func(b health.Bucket) int64 { return b.ProviderID }, now)
	seen := map[int64]bool{}
	for _, p := range providers {
		_, scored := scores[p.ID]
		if p.Backbone != name && !scored {
			continue
		}
		bp := backboneProvider{ID: p.ID, Name: p.Name, Enabled: p.Enabled, Health: badge(scores[p.ID])}
		if p.Backbone != name {
			bp.NowOn = cmp.Or(p.Backbone, "no backbone")
		}
		d.Providers = append(d.Providers, bp)
		seen[p.ID] = true
	}
	for id, s := range scores {
		if !seen[id] {
			d.Providers = append(d.Providers, backboneProvider{ID: id, Gone: true, Health: badge(s)})
		}
	}
	// Worst first; those without enough attempts to score last.
	slices.SortFunc(d.Providers, func(a, b backboneProvider) int {
		if a.Health.Enough != b.Health.Enough {
			if a.Health.Enough {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.Health.Value, b.Health.Value), cmp.Compare(b.Health.Failures, a.Health.Failures),
			strings.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	// Attempts by kind, and by day.
	counts := map[string]int64{}
	days := map[time.Time]*backboneDay{}
	for _, b := range buckets {
		counts[b.Outcome] += b.Count
		y, m, dd := b.Start.In(time.Local).Date()
		day := time.Date(y, m, dd, 0, 0, 0, 0, time.Local)
		bd := days[day]
		if bd == nil {
			bd = &backboneDay{Day: day}
			days[day] = bd
		}
		bd.Attempts += b.Count
		if health.IsFailure(b.Outcome) {
			bd.Failures += b.Count
		}
	}
	for _, o := range outcomeLabels {
		if n := counts[o.outcome]; n > 0 {
			d.Outcomes = append(d.Outcomes, outcomeShare{Label: o.label, Failure: health.IsFailure(o.outcome), Count: n,
				Percent: percent(n, d.Health.Attempts)})
		}
	}
	for _, bd := range days {
		bd.Percent = percent(bd.Failures, bd.Attempts)
		d.Days = append(d.Days, *bd)
	}
	slices.SortFunc(d.Days, func(a, b backboneDay) int { return b.Day.Compare(a.Day) }) // newest first

	// The streams that fail most, among those with enough attempts to score.
	var worst []backboneStream
	for id, s := range health.ByKey(buckets, func(b health.Bucket) int64 { return b.StreamID }, now) {
		if id != 0 && s.Enough && s.Failures > 0 {
			worst = append(worst, backboneStream{ID: id, Health: badge(s)})
		}
	}
	slices.SortFunc(worst, func(a, b backboneStream) int {
		return cmp.Or(cmp.Compare(a.Health.Value, b.Health.Value), cmp.Compare(b.Health.Failures, a.Health.Failures), cmp.Compare(a.ID, b.ID))
	})
	worst = worst[:min(len(worst), worstStreamRows)]
	if len(worst) > 0 {
		ids := make([]int64, len(worst))
		for i, s := range worst {
			ids[i] = s.ID
		}
		rows, err := ui.Store.GetStreamsByIDs(r.Context(), ids)
		if err != nil {
			ui.fail(w, "failing streams", err)
			return
		}
		for i := range worst {
			for _, row := range rows {
				if row.ID == worst[i].ID {
					worst[i].Name, worst[i].ProviderID, worst[i].ProviderName = row.Name, row.ProviderID, row.ProviderName
				}
			}
		}
		// Streams since removed from their provider have no name to show.
		worst = slices.DeleteFunc(worst, func(s backboneStream) bool { return s.Name == "" })
	}
	d.Streams = worst
	ui.render(w, r, http.StatusOK, "backbone", name+" backbone", d)
}

// percent is n of total, rounded down (0 when total is 0).
func percent(n, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(n * 100 / total)
}
