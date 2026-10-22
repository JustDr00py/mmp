package web

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/channel"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
)

// matchPageSize is how many suggestions one page shows.
const matchPageSize = 50

func (ui *UI) mountMatch(r chi.Router) {
	r.Get("/match", ui.matchPage)
	r.Post("/match/apply", ui.matchApply)
	r.Post("/match/rules", ui.matchRuleCreate)
	r.Post("/match/rules/{id}/delete", ui.matchRuleDelete)
	r.Post("/match/merge", ui.matchMerge)
	r.Post("/match/merges/delete", ui.matchMergeDelete)
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
	Query    string
	Group    string // provider group title; "" = any
	Show     string // "" = all, "new" = new channels, "existing" = additions to existing ones
	Backbone string // "" = any, source.NoBackbone = providers without one, else the label
	MinProv  int
	Offset   int

	provs map[int64]bool // with Backbone set: its providers (see withBackbone)
}

func parseMatchFilter(v url.Values) matchFilter {
	f := matchFilter{Query: strings.TrimSpace(v.Get("q")), Group: v.Get("group"), Backbone: v.Get("backbone"),
		MinProv: 2, Offset: max(atoi(v.Get("offset")), 0)}
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
	if f.Backbone != "" {
		v.Set("backbone", f.Backbone)
	}
	return v
}

// withBackbone resolves f.Backbone to the providers on it. A backbone with
// fewer providers than f.MinProv would show nothing, so the requirement is
// capped at its provider count.
func (ui *UI) withBackbone(ctx context.Context, f *matchFilter) error {
	if f.Backbone == "" {
		return nil
	}
	ps, err := ui.Store.ListProviders(ctx)
	if err != nil {
		return err
	}
	want := f.Backbone
	if want == source.NoBackbone {
		want = ""
	}
	f.provs = map[int64]bool{}
	for _, p := range ps {
		if p.Backbone == want {
			f.provs[p.ID] = true
		}
	}
	if n := len(f.provs); n > 0 && f.MinProv > n {
		f.MinProv = n
	}
	return nil
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
	Backbones []db.ListBackbonesRow
	Proposals []proposalView
	Matching  int // suggestions passing the whole filter
	AtMin     int // suggestions seen at >= MinProv providers
	Hidden    int // below the provider filter
	MinCapped int // the backbone's provider count when below Filter.MinProv, else 0
	From, To  int
	Prev      int // offset of the previous page, -1 = none
	Next      int // offset of the next page, -1 = none

	ChannelGroups []db.ChannelGroup
	Bouquets      []db.Bouquet
	ApplyGroup    string         // "none", "source" or a channel group id (sticky)
	ApplyBouquets map[int64]bool // sticky

	Rules     []db.MatchRule
	Merges    []channel.Merge
	Channels  []db.ListChannelMatchKeysRow // for "Add to channel"
	RuleError string
	RuleForm  db.CreateMatchRuleParams
	Flash     string
	Applied   *appliedSummary
}

type appliedSummary struct {
	Created, Attached int
	Bouquets          []string
}

var matchedByText = map[string]string{channel.ByTvgID: "same tvg-id", channel.ByName: "same name", channel.ByRule: "your rule"}

// filterProposals applies f (except paging) to props, sorted by name. It
// also returns the provider-group counts and the provider-filter tallies.
func filterProposals(props []channel.Proposal, f matchFilter) (out []channel.Proposal, groups []groupOption, atMin, hidden int) {
	terms := strings.Fields(strings.ToLower(f.Query))
	counts := map[string]int{}
	for _, p := range props {
		if f.provs != nil {
			// Only the backbone's streams, so "2+ providers" means two of
			// its providers carry the channel.
			var ok bool
			if p, ok = p.InProviders(f.provs); !ok {
				continue
			}
		}
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
	if err := ui.withBackbone(ctx, &f); err != nil {
		return d, err
	}
	if f.MinProv != d.Filter.MinProv {
		d.MinCapped = f.MinProv
	}
	var err error
	if d.Backbones, err = ui.Store.ListBackbones(ctx); err != nil {
		return d, err
	}
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
	if d.Merges, err = channel.LoadMerges(ctx, ui.Store); err != nil {
		return d, err
	}
	if d.Channels, err = ui.Store.ListChannelMatchKeys(ctx); err != nil {
		return d, err
	}
	slices.SortFunc(d.Channels, func(a, b db.ListChannelMatchKeysRow) int { return naturalCompare(a.Name, b.Name) })
	return d, nil
}

var matchFlashes = map[string]string{
	"rule": "Rule added. Its suggestions are in the list.", "ruledel": "Rule deleted.",
	"merged":    "Suggestions merged. They stay together after provider refreshes; undo it under Advanced.",
	"mergeone":  "Tick two or more suggestions to merge them.",
	"mergegone": "Those suggestions changed since the page loaded. Check the list and try again.",
	"mergedel":  "Merge undone.",
	"mergedch":  "Added to the channel. New streams like these will be offered to it too; undo under Advanced.",
	"nochannel": "Pick a channel from the list to add the suggestions to.",
}

func (ui *UI) matchPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d, err := ui.matchData(r, parseMatchFilter(q))
	if err != nil {
		ui.fail(w, "match proposals", err)
		return
	}
	d.Flash = matchFlashes[q.Get("msg")]
	d.ApplyGroup = q.Get("cg")
	if !q.Has("cg") { // new channels keep their provider's categories unless told otherwise
		d.ApplyGroup = "source"
	}
	d.ApplyBouquets = map[int64]bool{}
	for _, v := range q["bq"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			d.ApplyBouquets[id] = true
		}
	}
	if msg := q.Get("applied"); msg != "" {
		parts := strings.SplitN(msg, "-", 3)
		a := &appliedSummary{Created: atoi(parts[0])}
		if len(parts) > 1 {
			a.Attached = atoi(parts[1])
		}
		for _, b := range d.Bouquets {
			if d.ApplyBouquets[b.ID] {
				a.Bouquets = append(a.Bouquets, b.Name)
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
	if err := ui.withBackbone(r.Context(), &f); err != nil {
		ui.fail(w, "match backbone", err)
		return
	}
	narrowed := f.Group != "" || f.Backbone != ""
	if pf.Get("scope") == "all" || narrowed {
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
			// Suggestions narrowed to a provider group or backbone apply
			// only its streams (rows on the page already post just those).
			if _, onPage := opts.Streams[p.Key]; !onPage && narrowed {
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
	for _, v := range pf["bq"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 && !slices.Contains(opts.BouquetIDs, id) {
			opts.BouquetIDs = append(opts.BouquetIDs, id)
		}
	}
	back := f.Values()
	if cg != "" {
		back.Set("cg", cg)
	}
	for _, id := range opts.BouquetIDs {
		back.Add("bq", strconv.FormatInt(id, 10))
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

// matchMerge joins the ticked suggestions into one, for channels automatic
// matching keeps apart. The first ticked suggestion takes in the others;
// the links are saved, so the suggestions stay merged (see channel.Merge).
func (ui *UI) matchMerge(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pf := r.PostForm
	back := parseMatchFilter(pf).Values()
	if o := atoi(pf.Get("offset")); o > 0 {
		back.Set("offset", strconv.Itoa(o))
	}
	var keys []string
	for _, k := range pf["key"] {
		if k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if pf.Get("to") == "channel" {
		ui.mergeIntoChannel(w, r, strings.TrimSpace(pf.Get("channel")), keys, back)
		return
	}
	if len(keys) < 2 {
		back.Set("msg", "mergeone")
		ui.redirect(w, r, Prefix+"/match?"+back.Encode())
		return
	}
	props, err := ui.Match.Propose(r.Context())
	if err != nil {
		ui.fail(w, "match proposals", err)
		return
	}
	names := map[string]string{}
	for _, p := range props {
		names[p.Key] = p.Name
	}
	for _, k := range keys {
		if _, ok := names[k]; !ok {
			back.Set("msg", "mergegone")
			ui.redirect(w, r, Prefix+"/match?"+back.Encode())
			return
		}
	}
	merges, err := channel.LoadMerges(r.Context(), ui.Store)
	if err != nil {
		ui.fail(w, "match merges", err)
		return
	}
	into := keys[0]
	for _, k := range keys[1:] {
		if !slices.ContainsFunc(merges, func(m channel.Merge) bool { return m.Joins(into, k) }) {
			merges = append(merges, channel.Merge{A: into, B: k, AName: names[into], BName: names[k]})
		}
	}
	if err := channel.SaveMerges(r.Context(), ui.Store, merges); err != nil {
		ui.fail(w, "match merges", err)
		return
	}
	ui.Log.Info("web: match suggestions merged", "into", into, "merged", len(keys)-1)
	back.Set("msg", "merged")
	ui.redirect(w, r, Prefix+"/match?"+back.Encode())
}

// channelRefRe reads the channel id from an "Add to channel" pick.
var channelRefRe = regexp.MustCompile(`\(#(\d+)\)\s*$`)

// mergeIntoChannel merges the ticked suggestions into an existing channel
// and applies them: their streams join the channel now, and later streams
// with the same keys are offered to it.
func (ui *UI) mergeIntoChannel(w http.ResponseWriter, r *http.Request, target string, keys []string, back url.Values) {
	ctx := r.Context()
	if len(keys) == 0 {
		back.Set("msg", "mergeone")
		ui.redirect(w, r, Prefix+"/match?"+back.Encode())
		return
	}
	chans, err := ui.Store.ListChannelMatchKeys(ctx)
	if err != nil {
		ui.fail(w, "match channels", err)
		return
	}
	var ch *db.ListChannelMatchKeysRow
	if m := channelRefRe.FindStringSubmatch(target); m != nil {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		if i := slices.IndexFunc(chans, func(c db.ListChannelMatchKeysRow) bool { return c.ID == id }); i >= 0 {
			ch = &chans[i]
		}
	} else { // typed without picking: a name only one channel has
		for i := range chans {
			if strings.EqualFold(chans[i].Name, target) {
				if ch != nil {
					ch = nil
					break
				}
				ch = &chans[i]
			}
		}
	}
	if ch == nil {
		back.Set("msg", "nochannel")
		ui.redirect(w, r, Prefix+"/match?"+back.Encode())
		return
	}
	props, err := ui.Match.Propose(ctx)
	if err != nil {
		ui.fail(w, "match proposals", err)
		return
	}
	names := map[string]string{}
	for _, p := range props {
		names[p.Key] = p.Name
	}
	merges, err := channel.LoadMerges(ctx, ui.Store)
	if err != nil {
		ui.fail(w, "match merges", err)
		return
	}
	into := channel.ChannelKey(ch.ID)
	opts := channel.ApplyOptions{Streams: map[string][]int64{}, ExistingOnly: true}
	pf := r.PostForm
	for _, k := range keys {
		if _, ok := names[k]; !ok {
			back.Set("msg", "mergegone")
			ui.redirect(w, r, Prefix+"/match?"+back.Encode())
			return
		}
		if !slices.ContainsFunc(merges, func(m channel.Merge) bool { return m.Joins(into, k) }) {
			merges = append(merges, channel.Merge{A: into, B: k, AName: ch.Name, BName: names[k]})
		}
		opts.Keys = append(opts.Keys, k)
		if slices.Contains(pf["row"], k) { // the page posts its ticked streams
			ids := []int64{}
			for _, v := range pf["stream|"+k] {
				if id, err := strconv.ParseInt(v, 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
			opts.Streams[k] = ids
		}
	}
	if err := channel.SaveMerges(ctx, ui.Store, merges); err != nil {
		ui.fail(w, "match merges", err)
		return
	}
	res, err := ui.Match.Apply(ctx, opts)
	if err != nil {
		ui.fail(w, "match apply", err)
		return
	}
	ui.Log.Info("web: match suggestions merged into channel", "channel_id", ch.ID, "merged", len(keys), "attached", res.Attached)
	back.Set("msg", "mergedch")
	back.Set("applied", strconv.Itoa(res.Created)+"-"+strconv.Itoa(res.Attached))
	ui.redirect(w, r, Prefix+"/match?"+back.Encode())
}

// matchMergeDelete undoes one merge.
func (ui *UI) matchMergeDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a, b := r.PostForm.Get("a"), r.PostForm.Get("b")
	merges, err := channel.LoadMerges(r.Context(), ui.Store)
	if err != nil {
		ui.fail(w, "match merges", err)
		return
	}
	merges = slices.DeleteFunc(merges, func(m channel.Merge) bool { return m.Joins(a, b) })
	if err := channel.SaveMerges(r.Context(), ui.Store, merges); err != nil {
		ui.fail(w, "match merges", err)
		return
	}
	ui.redirect(w, r, Prefix+"/match?msg=mergedel")
}
