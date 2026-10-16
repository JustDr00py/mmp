package web

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
)

// matchPageSize is how many suggestions one page shows.
const matchPageSize = 50

func (ui *UI) mountMatch(r chi.Router) {
	r.Get("/match", ui.matchPage)
	r.Post("/match/apply", ui.matchApply)
	r.Post("/match/rules", ui.matchRuleCreate)
	r.Post("/match/rules/{id}/delete", ui.matchRuleDelete)
}

type proposalStream struct {
	ID       int64
	Name     string
	Provider string
	Group    string
}

type proposalView struct {
	channel.Proposal
	Streams  []proposalStream
	Existing string // name of the channel the streams would join
	Why      string // plain-language MatchedBy
}

// matchFilter is the page's filter state, carried through apply.
type matchFilter struct {
	Query   string
	Group   string // provider group title; "" = any
	Show    string // "" = all, "new" = new channels, "existing" = additions to existing ones
	MinProv int
	Offset  int
}

func parseMatchFilter(v url.Values) matchFilter {
	f := matchFilter{Query: strings.TrimSpace(v.Get("q")), Group: v.Get("group"), MinProv: 2, Offset: max(atoi(v.Get("offset")), 0)}
	if s := v.Get("show"); s == "new" || s == "existing" {
		f.Show = s
	}
	if m := v.Get("min"); m != "" {
		f.MinProv = max(atoi(m), 1)
	}
	return f
}

// Values encodes the filter (without the offset) for links and redirects.
func (f matchFilter) Values() url.Values {
	v := url.Values{"min": {strconv.Itoa(f.MinProv)}}
	if f.Query != "" {
		v.Set("q", f.Query)
	}
	if f.Group != "" {
		v.Set("group", f.Group)
	}
	if f.Show != "" {
		v.Set("show", f.Show)
	}
	return v
}

// PageURL links to the page at offset with the same filter.
func (f matchFilter) PageURL(offset int) string {
	v := f.Values()
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	return Prefix + "/match?" + v.Encode()
}

type groupOption struct {
	Name  string
	Count int
}

type matchData struct {
	Filter    matchFilter
	Groups    []groupOption // provider groups among suggestions at MinProv
	Proposals []proposalView
	Matching  int // suggestions passing the whole filter
	AtMin     int // suggestions seen at >= MinProv providers
	Hidden    int // below the provider filter
	From, To  int
	Prev      int // offset of the previous page, -1 = none
	Next      int // offset of the next page, -1 = none

	ChannelGroups []db.ChannelGroup
	Bouquets      []db.Bouquet
	ApplyGroup    string // "", "source" or a channel group id (sticky)
	ApplyBouquet  int64

	Rules     []db.MatchRule
	RuleError string
	RuleForm  db.CreateMatchRuleParams
	Flash     string
	Applied   *appliedSummary
}

type appliedSummary struct {
	Created, Attached int
	Bouquet           string
}

var matchedByText = map[string]string{channel.ByTvgID: "same tvg-id", channel.ByName: "same name", channel.ByRule: "your rule"}

// filterProposals applies f (except paging) to props, sorted by name. It
// also returns the provider-group counts and the provider-filter tallies.
func filterProposals(props []channel.Proposal, f matchFilter) (out []channel.Proposal, groups []groupOption, atMin, hidden int) {
	terms := strings.Fields(strings.ToLower(f.Query))
	counts := map[string]int{}
	for _, p := range props {
		if p.Providers < f.MinProv {
			hidden++
			continue
		}
		atMin++
		for _, g := range p.Groups {
			counts[g]++
		}
		if (f.Show == "new" && p.ChannelID != 0) || (f.Show == "existing" && p.ChannelID == 0) {
			continue
		}
		if f.Group != "" {
			// Only the group's own streams: "US: News 5" and "UK: News 5"
			// match by name, but filtering by US| NEWS means the US one.
			var ok bool
			if p, ok = p.InGroup(f.Group); !ok || p.Providers < f.MinProv {
				continue
			}
		}
		if len(terms) > 0 {
			hay := strings.ToLower(p.Name + "\n" + strings.Join(p.StreamNames, "\n") + "\n" + p.TvgID)
			ok := true
			for _, t := range terms {
				if !strings.Contains(hay, t) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b channel.Proposal) int {
		return cmp.Or(naturalCompare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Key, b.Key))
	})
	for g, n := range counts {
		groups = append(groups, groupOption{g, n})
	}
	slices.SortFunc(groups, func(a, b groupOption) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, groups, atMin, hidden
}

func (ui *UI) matchData(r *http.Request, f matchFilter) (matchData, error) {
	ctx := r.Context()
	d := matchData{Filter: f, Prev: -1, Next: -1}
	props, err := ui.Match.Propose(ctx)
	if err != nil {
		return d, err
	}
	shown, groups, atMin, hidden := filterProposals(props, f)
	d.Groups, d.AtMin, d.Hidden, d.Matching = groups, atMin, hidden, len(shown)
	if f.Offset >= len(shown) {
		f.Offset = max(0, (len(shown)-1)/matchPageSize*matchPageSize)
		d.Filter.Offset = f.Offset
	}
	page := shown[f.Offset:min(f.Offset+matchPageSize, len(shown))]
	if len(page) > 0 {
		d.From, d.To = f.Offset+1, f.Offset+len(page)
	}
	if f.Offset > 0 {
		d.Prev = max(f.Offset-matchPageSize, 0)
	}
	if d.To < len(shown) {
		d.Next = d.To
	}

	var ids, chanIDs []int64
	for _, p := range page {
		ids = append(ids, p.StreamIDs...)
		if p.ChannelID != 0 {
			chanIDs = append(chanIDs, p.ChannelID)
		}
	}
	streams := map[int64]db.GetStreamsByIDsRow{}
	if len(ids) > 0 {
		rows, err := ui.Store.GetStreamsByIDs(ctx, ids)
		if err != nil {
			return d, err
		}
		for _, s := range rows {
			streams[s.ID] = s
		}
	}
	chanNames := map[int64]string{}
	if len(chanIDs) > 0 {
		rows, err := ui.Store.ChannelNamesByIDs(ctx, chanIDs)
		if err != nil {
			return d, err
		}
		for _, c := range rows {
			chanNames[c.ID] = c.Name
		}
	}
	for _, p := range page {
		v := proposalView{Proposal: p, Existing: chanNames[p.ChannelID]}
		var why []string
		for _, m := range p.MatchedBy {
			why = append(why, matchedByText[m])
		}
		v.Why = strings.Join(why, ", ")
		for i, id := range p.StreamIDs {
			s := streams[id]
			v.Streams = append(v.Streams, proposalStream{ID: id, Name: p.StreamNames[i], Provider: s.ProviderName, Group: s.GroupTitle})
		}
		d.Proposals = append(d.Proposals, v)
	}
	if d.ChannelGroups, err = ui.Store.ListChannelGroups(ctx); err != nil {
		return d, err
	}
	if d.Bouquets, err = ui.Store.ListBouquets(ctx); err != nil {
		return d, err
	}
	if d.Rules, err = ui.Store.ListMatchRules(ctx); err != nil {
		return d, err
	}
	return d, nil
}

var matchFlashes = map[string]string{"rule": "Rule added. Its suggestions are in the list.", "ruledel": "Rule deleted."}

func (ui *UI) matchPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d, err := ui.matchData(r, parseMatchFilter(q))
	if err != nil {
		ui.fail(w, "match proposals", err)
		return
	}
	d.Flash = matchFlashes[q.Get("msg")]
	d.ApplyGroup = q.Get("cg")
	d.ApplyBouquet, _ = strconv.ParseInt(q.Get("bq"), 10, 64)
	if msg := q.Get("applied"); msg != "" {
		parts := strings.SplitN(msg, "-", 3)
		a := &appliedSummary{Created: atoi(parts[0])}
		if len(parts) > 1 {
			a.Attached = atoi(parts[1])
		}
		for _, b := range d.Bouquets {
			if b.ID == d.ApplyBouquet {
				a.Bouquet = b.Name
			}
		}
		d.Applied = a
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "match-results" {
		ui.fragment(w, r, "match", "match-results", d)
		return
	}
	ui.render(w, r, http.StatusOK, "match", "Match", d)
}

// matchApply commits suggestions: the ticked ones (scope=selected) or every
// one passing the posted filter (scope=all). Keys are recomputed server
// side by the matcher, so a stale page only applies what still matches.
// Rows shown on the page carry their (possibly edited) name and the ticked
// streams; suggestions applied by scope=all from other pages apply as
// suggested.
func (ui *UI) matchApply(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pf := r.PostForm
	f := parseMatchFilter(pf)
	opts := channel.ApplyOptions{Names: map[string]string{}, Streams: map[string][]int64{}}
	for _, key := range pf["row"] {
		opts.Names[key] = pf.Get("name|" + key)
		ids := []int64{}
		for _, v := range pf["stream|"+key] {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		opts.Streams[key] = ids
	}
	if pf.Get("scope") == "all" || f.Group != "" {
		props, err := ui.Match.Propose(r.Context())
		if err != nil {
			ui.fail(w, "match proposals", err)
			return
		}
		shown, _, _, _ := filterProposals(props, f)
		for _, p := range shown {
			if pf.Get("scope") == "all" {
				opts.Keys = append(opts.Keys, p.Key)
			}
			// Suggestions narrowed to a provider group apply only its
			// streams (rows on the page already post just those).
			if _, onPage := opts.Streams[p.Key]; !onPage && f.Group != "" {
				opts.Streams[p.Key] = p.StreamIDs
			}
		}
	}
	if pf.Get("scope") != "all" {
		opts.Keys = pf["key"]
	}
	cg := pf.Get("cg")
	switch {
	case cg == "source":
		opts.GroupFromSource = true
	case cg != "":
		if id, err := strconv.ParseInt(cg, 10, 64); err == nil && id > 0 {
			opts.ChannelGroupID = &id
		}
	}
	if id, err := strconv.ParseInt(pf.Get("bq"), 10, 64); err == nil && id > 0 {
		opts.BouquetID = &id
	}
	back := f.Values()
	if cg != "" {
		back.Set("cg", cg)
	}
	if opts.BouquetID != nil {
		back.Set("bq", strconv.FormatInt(*opts.BouquetID, 10))
	}
	if f.Offset > 0 {
		back.Set("offset", strconv.Itoa(f.Offset))
	}
	if len(opts.Keys) == 0 {
		ui.redirect(w, r, Prefix+"/match?"+back.Encode())
		return
	}
	res, err := ui.Match.Apply(r.Context(), opts)
	if err != nil {
		ui.fail(w, "match apply", err)
		return
	}
	ui.Log.Info("web: match applied", "keys", len(opts.Keys), "created", res.Created, "attached", res.Attached)
	back.Set("applied", strconv.Itoa(res.Created)+"-"+strconv.Itoa(res.Attached))
	ui.redirect(w, r, Prefix+"/match?"+back.Encode())
}

func (ui *UI) matchRuleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f := db.CreateMatchRuleParams{
		// The pattern is kept verbatim: surrounding spaces can be part of a regex.
		Pattern: r.PostForm.Get("pattern"), ChannelName: strings.TrimSpace(r.PostForm.Get("channel_name")),
		Priority: int32(atoi(r.PostForm.Get("priority"))), Enabled: true,
	}
	var msg string
	switch {
	case strings.TrimSpace(f.Pattern) == "" || f.ChannelName == "":
		msg = "Pattern and channel name are required."
	default:
		if _, err := channel.CompileRule(f.Pattern); err != nil {
			msg = "Invalid pattern: " + err.Error()
		}
	}
	if msg == "" {
		if _, err := ui.Store.CreateMatchRule(r.Context(), f); err != nil {
			ui.fail(w, "create match rule", err)
			return
		}
		ui.redirect(w, r, Prefix+"/match?msg=rule")
		return
	}
	d, err := ui.matchData(r, parseMatchFilter(nil))
	if err != nil {
		ui.fail(w, "match proposals", err)
		return
	}
	d.RuleError, d.RuleForm = msg, f
	ui.render(w, r, http.StatusUnprocessableEntity, "match", "Match", d)
}

func (ui *UI) matchRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := ui.Store.DeleteMatchRule(r.Context(), id); err != nil {
		ui.fail(w, "delete match rule", err)
		return
	}
	ui.redirect(w, r, Prefix+"/match?msg=ruledel")
}

// naturalCompare orders strings with digit runs compared as numbers:
// "Sport 2" < "Sport 10".
func naturalCompare(a, b string) int {
	for a != "" && b != "" {
		da, db := digitPrefix(a), digitPrefix(b)
		if da != "" && db != "" {
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if c := cmp.Or(cmp.Compare(len(na), len(nb)), cmp.Compare(na, nb)); c != 0 {
				return c
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return cmp.Compare(a[0], b[0])
		}
		a, b = a[1:], b[1:]
	}
	return cmp.Compare(len(a), len(b))
}

func digitPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
