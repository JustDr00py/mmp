package channel

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Stream is a provider stream as seen by the matcher.
type Stream struct {
	ID               int64
	ProviderID       int64
	ProviderPriority int32 // lower is preferred; orders the failover list
	Name             string
	// MatchName is Name without the source labels the admin ignores
	// ("AT&T: MTV" -> "MTV"); "" means Name. Matching and suggested
	// channel names use it; rules see the raw Name.
	MatchName string
	TvgID     string
	LogoURL   string
	Group     string // the provider's group title ("US| SPORTS")
	// Backbone is the provider's backbone: providers on one count as one
	// source. "" = the provider is its own.
	Backbone string
}

func (s Stream) matchName() string { return cmp.Or(s.MatchName, s.Name) }

// source identifies where a stream comes from: its backbone, or its
// provider when it has none.
func (s Stream) source() string {
	if s.Backbone != "" {
		return "b:" + s.Backbone
	}
	return "p:" + strconv.FormatInt(s.ProviderID, 10)
}

// Existing is an already-created channel streams can be attached to.
type Existing struct {
	ID    int64
	Name  string
	TvgID string
}

// Rule groups streams whose raw name matches Pattern under ChannelName.
type Rule struct {
	ID          int64
	Pattern     *regexp.Regexp
	ChannelName string
}

// Match methods reported in Proposal.MatchedBy.
const (
	ByTvgID    = "tvg_id"
	ByCallSign = "call_sign"
	ByName     = "name"
	ByRule     = "rule"
)

// Proposal is one proposed channel: a group of equivalent streams, either
// attached to an existing channel or to be created.
type Proposal struct {
	Key       string   `json:"key"`                  // stable id for confirming this proposal
	ChannelID int64    `json:"channel_id,omitempty"` // existing channel; 0 = create new
	Name      string   `json:"name"`
	TvgID     string   `json:"tvg_id"`
	LogoURL   string   `json:"logo_url"`
	MatchedBy []string `json:"matched_by"` // which methods linked the streams
	StreamIDs []int64  `json:"stream_ids"` // failover order
	// Providers counts the distinct sources in the group: a backbone's
	// providers count once (see Stream.Backbone).
	Providers int `json:"providers"`
	// Group is the provider group of the first stream; Groups every
	// distinct provider group in the proposal (for filtering).
	Group  string   `json:"group"`
	Groups []string `json:"groups"`
	// Per stream, in failover order: raw name, provider group and provider
	// (for searching and narrowing a proposal to one provider group).
	StreamNames     []string `json:"-"`
	StreamGroups    []string `json:"-"`
	StreamProviders []int64  `json:"-"`
	StreamSources   []string `json:"-"`
}

// InGroup narrows p to its streams in provider group g. ok is false when
// none of its streams are in g.
func (p Proposal) InGroup(g string) (Proposal, bool) {
	return p.narrow(func(i int) bool { return p.StreamGroups[i] == g }, false)
}

// InProviders narrows p to its streams from the providers in set. ok is
// false when none of its streams are. The set is one backbone's providers,
// so Providers then counts providers, not sources.
func (p Proposal) InProviders(set map[int64]bool) (Proposal, bool) {
	return p.narrow(func(i int) bool { return set[p.StreamProviders[i]] }, true)
}

// narrow keeps the streams i of p that keep(i) accepts, with the source (or
// with byProvider, provider) count and groups recomputed for them.
func (p Proposal) narrow(keep func(i int) bool, byProvider bool) (Proposal, bool) {
	q := p
	q.StreamIDs, q.StreamNames, q.StreamGroups, q.StreamProviders, q.StreamSources, q.Groups = nil, nil, nil, nil, nil, nil
	provs := map[string]bool{}
	for i := range p.StreamIDs {
		if !keep(i) {
			continue
		}
		q.StreamIDs = append(q.StreamIDs, p.StreamIDs[i])
		q.StreamNames = append(q.StreamNames, p.StreamNames[i])
		q.StreamGroups = append(q.StreamGroups, p.StreamGroups[i])
		q.StreamProviders = append(q.StreamProviders, p.StreamProviders[i])
		src := "p:" + strconv.FormatInt(p.StreamProviders[i], 10)
		if i < len(p.StreamSources) {
			q.StreamSources = append(q.StreamSources, p.StreamSources[i])
			if !byProvider {
				src = p.StreamSources[i]
			}
		}
		provs[src] = true
		if !slices.Contains(q.Groups, p.StreamGroups[i]) {
			q.Groups = append(q.Groups, p.StreamGroups[i])
		}
	}
	q.Providers = len(provs)
	if len(q.StreamGroups) > 0 {
		q.Group = q.StreamGroups[0]
	}
	return q, len(q.StreamIDs) > 0
}

// Match groups streams that are the same channel across providers. Streams
// are linked transitively by a shared tvg-id (case-insensitive), by a
// local station's call sign (see CallSign), by a shared NormalizeName key,
// and by the first rule (in slice order) their name matches. A tvg-id that
// one provider gives to differently named streams (Spectrum SportsNet's
// Lakers and Dodgers feeds) covers several channels, so under it streams
// only link when their names match too. Likewise a tvg-id or name streams
// of different stations carry only links streams of the same call sign. Each
// group becomes one Proposal, attached to an existing channel when the
// group shares a tvg-id, call sign or name key with it. Merges then join
// groups the admin said are one channel. Output is deterministic for a
// given input.
func Match(streams []Stream, existing []Existing, rules []Rule, merges []Merge) []Proposal {
	n := len(streams)
	uf := newUnionFind(n)
	calls := make([]string, n)
	for i, s := range streams {
		calls[i] = CallSign(s.matchName(), s.TvgID)
	}
	shared := sharedTvgKeys(streams)
	plainTvg, plainName := make([]string, n), make([]string, n)
	for i, s := range streams {
		plainTvg[i] = feedTvgKey(s.TvgID, s.matchName())
		if k := NormalizeName(s.matchName()); k != "" {
			plainName[i] = "name:" + k
		}
	}
	mixed := mixedCallKeys(calls, plainTvg, plainName)
	tvgKeyOf := func(tvg, name, call string) string {
		t := feedTvgKey(tvg, name)
		switch {
		case t == "":
		case mixed[t] && call != "":
			t += "|call:" + call
		case shared[t]:
			t += "|" + feedlessName(name)
		}
		return t
	}
	nameKeyOf := func(k, call string) string { // k: "name:" + NormalizeName
		if k != "" && mixed[k] && call != "" {
			k += "|call:" + call
		}
		return k
	}
	tvgKeys, callKeys, nameKeys := make([]string, n), make([]string, n), make([]string, n)
	for i, s := range streams {
		if calls[i] != "" {
			callKeys[i] = "call:" + calls[i]
		}
		tvgKeys[i] = tvgKeyOf(s.TvgID, s.matchName(), calls[i])
		nameKeys[i] = nameKeyOf(plainName[i], calls[i])
	}
	owner := map[string]int{} // match key -> first stream index carrying it
	link := func(i int, key string) {
		if j, ok := owner[key]; ok {
			uf.union(i, j)
		} else {
			owner[key] = i
		}
	}

	ruleOf := make([]*Rule, n)
	for i, t := range tvgKeys {
		if t != "" {
			link(i, t)
		}
	}
	for i, c := range callKeys {
		if c != "" {
			link(i, c)
		}
	}
	for i, k := range nameKeys {
		if k != "" {
			link(i, k)
		}
	}
	for i, s := range streams {
		for r := range rules {
			if rules[r].Pattern.MatchString(s.Name) {
				ruleOf[i] = &rules[r]
				// A rule's target joins the name key space, so rule hits group
				// with streams already named like the target.
				link(i, "name:"+NormalizeName(rules[r].ChannelName))
				break
			}
		}
	}
	// A group with no other key is keyed by its first stream.
	for i, s := range streams {
		owner[streamKey(s.ID)] = i
	}
	partners := map[string][]string{} // merged keys, both ways
	for _, m := range merges {
		a, okA := owner[m.A]
		b, okB := owner[m.B]
		if okA && okB {
			uf.union(a, b)
		}
		partners[m.A] = append(partners[m.A], m.B)
		partners[m.B] = append(partners[m.B], m.A)
	}

	groups := map[int][]int{}
	for i := range streams {
		root := uf.find(i)
		groups[root] = append(groups[root], i)
	}

	byTvg, byCall, byName := map[string]int64{}, map[string]int64{}, map[string]int64{}
	existingByID := make(map[int64]Existing, len(existing))
	for _, c := range existing { // first (lowest ID) wins ties
		existingByID[c.ID] = c
		call := CallSign(c.Name, c.TvgID)
		if call != "" {
			if _, ok := byCall["call:"+call]; !ok {
				byCall["call:"+call] = c.ID
			}
		}
		if t := tvgKeyOf(c.TvgID, c.Name, call); t != "" {
			if _, ok := byTvg[t]; !ok {
				byTvg[t] = c.ID
			}
		}
		var plain string
		if k := NormalizeName(c.Name); k != "" {
			plain = "name:" + k
		}
		if k := nameKeyOf(plain, call); k != "" {
			if _, ok := byName[k]; !ok {
				byName[k] = c.ID
			}
		}
	}

	out := make([]Proposal, 0, len(groups))
	for _, idx := range groups {
		out = append(out, buildProposal(streams, tvgKeys, callKeys, nameKeys, idx, ruleOf, byTvg, byCall, byName, existingByID, partners))
	}
	slices.SortFunc(out, func(a, b Proposal) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

func buildProposal(streams []Stream, streamTvgKeys, streamCallKeys, streamNameKeys []string, idx []int, ruleOf []*Rule,
	byTvg, byCall, byName map[string]int64, existing map[int64]Existing, partners map[string][]string) Proposal {
	// Failover order: preferred providers first, then stream id.
	slices.SortFunc(idx, func(a, b int) int {
		return cmp.Or(
			cmp.Compare(streams[a].ProviderPriority, streams[b].ProviderPriority),
			cmp.Compare(streams[a].ID, streams[b].ID),
		)
	})

	var p Proposal
	var tvgKeys, callKeys, nameKeys []string
	var rule *Rule
	sources := map[string]bool{}
	for _, i := range idx {
		s := streams[i]
		p.StreamIDs = append(p.StreamIDs, s.ID)
		p.StreamNames = append(p.StreamNames, s.Name)
		p.StreamGroups = append(p.StreamGroups, s.Group)
		p.StreamProviders = append(p.StreamProviders, s.ProviderID)
		p.StreamSources = append(p.StreamSources, s.source())
		if len(p.StreamIDs) == 1 {
			p.Group = s.Group
		}
		if !slices.Contains(p.Groups, s.Group) {
			p.Groups = append(p.Groups, s.Group)
		}
		sources[s.source()] = true
		if c := streamCallKeys[i]; c != "" {
			callKeys = append(callKeys, c)
		}
		if t := streamTvgKeys[i]; t != "" {
			tvgKeys = append(tvgKeys, t)
			if p.TvgID == "" {
				p.TvgID = strings.TrimSpace(s.TvgID)
			}
		}
		if k := streamNameKeys[i]; k != "" {
			nameKeys = append(nameKeys, k)
		}
		if p.LogoURL == "" {
			p.LogoURL = s.LogoURL
		}
		if rule == nil && ruleOf[i] != nil {
			rule = ruleOf[i]
		}
	}
	p.Providers = len(sources)
	slices.Sort(tvgKeys)
	slices.Sort(callKeys)
	slices.Sort(nameKeys)

	// MatchedBy: methods that actually linked two or more streams.
	p.MatchedBy = []string{}
	if hasDup(tvgKeys) {
		p.MatchedBy = append(p.MatchedBy, ByTvgID)
	}
	if hasDup(callKeys) {
		p.MatchedBy = append(p.MatchedBy, ByCallSign)
	}
	if hasDup(nameKeys) {
		p.MatchedBy = append(p.MatchedBy, ByName)
	}
	if rule != nil {
		p.MatchedBy = append(p.MatchedBy, ByRule)
	}

	// Stable key: smallest tvg key, else smallest call sign, else the rule
	// target, else smallest name key, else first stream.
	switch {
	case len(tvgKeys) > 0:
		p.Key = tvgKeys[0]
	case len(callKeys) > 0:
		p.Key = callKeys[0]
	case rule != nil:
		p.Key = "name:" + NormalizeName(rule.ChannelName)
	case len(nameKeys) > 0:
		p.Key = nameKeys[0]
	default:
		p.Key = streamKey(p.StreamIDs[0])
	}

	// Every key the group carries, for merges.
	keys := slices.Concat(tvgKeys, callKeys, nameKeys)
	if rule != nil {
		keys = append(keys, "name:"+NormalizeName(rule.ChannelName))
	}
	for _, id := range p.StreamIDs {
		keys = append(keys, streamKey(id))
	}

	// Existing channel: a channel the admin merged the group into, then
	// tvg-id, rule target, call sign and name, then a channel matching a
	// key merged with the group's (its other side was applied).
	for _, k := range keys {
		for _, m := range partners[k] {
			if id, ok := ChannelOfKey(m); ok && p.ChannelID == 0 {
				if _, exists := existing[id]; exists {
					p.ChannelID = id
				}
			}
		}
	}
	for _, t := range tvgKeys {
		if p.ChannelID != 0 {
			break
		}
		if id, ok := byTvg[t]; ok {
			p.ChannelID = id
			break
		}
	}
	if p.ChannelID == 0 && rule != nil {
		p.ChannelID = byName["name:"+NormalizeName(rule.ChannelName)]
	}
	for _, c := range callKeys {
		if p.ChannelID != 0 {
			break
		}
		p.ChannelID = byCall[c]
	}
	if p.ChannelID == 0 {
		for _, k := range nameKeys {
			if id, ok := byName[k]; ok {
				p.ChannelID = id
				break
			}
		}
	}
	for _, k := range keys {
		for _, m := range partners[k] {
			if p.ChannelID != 0 {
				break
			}
			if id, ok := byTvg[m]; ok {
				p.ChannelID = id
			} else if id, ok := byCall[m]; ok {
				p.ChannelID = id
			} else if id, ok := byName[m]; ok {
				p.ChannelID = id
			}
		}
	}

	switch {
	case p.ChannelID != 0:
		c := existing[p.ChannelID]
		p.Name = c.Name
		if c.TvgID != "" {
			p.TvgID = c.TvgID
		}
	case rule != nil:
		p.Name = rule.ChannelName
	default:
		// An event slot is named by its label: the event will change.
		name := streams[idx[0]].matchName()
		if label, _, ok := SplitSlot(name); ok {
			name = label
		}
		p.Name = DisplayName(name)
	}
	return p
}

// DisplayName tidies a provider stream name for use as a channel name,
// keeping its casing: "US: ABC East FHD" -> "ABC East", "MTV ᴿᴬᵂ" -> "MTV".
func DisplayName(name string) string {
	s := strings.TrimSpace(strings.Map(func(r rune) rune {
		if isSuperscript(r) { // "MTV ᴿᴬᵂ" -> "MTV"
			return ' '
		}
		return r
	}, name))

	// Leading country prefix glued to a separator: "US: ", "USA | ", "UK-".
	if i := strings.IndexAny(s, ":|-/"); i > 0 {
		head := strings.ToLower(strings.TrimSpace(s[:i]))
		if countryTokens[head] || slices.Contains(longCountryPrefixes, head) {
			s = strings.TrimSpace(s[i+1:])
		}
	}

	words := strings.Fields(s)
	kept := make([]string, 0, len(words))
	for _, w := range words {
		bare := strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
			return unicode.IsPunct(r) || unicode.IsSymbol(r)
		}))
		if qualityTokens[bare] {
			continue
		}
		// Country codes only when decorated: "Sky (UK)" drops the tag, a
		// bare word is kept since it may be part of the name.
		if countryTokens[bare] && bare != strings.ToLower(w) {
			continue
		}
		kept = append(kept, w)
	}
	out := strings.Trim(strings.Join(kept, " "), " -|:/")
	if out == "" {
		return strings.TrimSpace(name)
	}
	return out
}

func streamKey(id int64) string { return "stream:" + strconv.FormatInt(id, 10) }

// ChannelKey is the merge key of an existing channel: a group merged with
// it is offered as additions to that channel.
func ChannelKey(id int64) string { return "channel:" + strconv.FormatInt(id, 10) }

// ChannelOfKey is the channel id in a ChannelKey.
func ChannelOfKey(k string) (int64, bool) {
	rest, ok := strings.CutPrefix(k, "channel:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil
}

// sharedTvgKeys finds the tvg-id keys that some provider gives to streams
// with different names. Copies of one channel (HD, FHD) normalize to the
// same name and do not count.
func sharedTvgKeys(streams []Stream) map[string]bool {
	type provTvg struct {
		provider int64
		tvg      string
	}
	firstName := map[provTvg]string{}
	shared := map[string]bool{}
	for _, s := range streams {
		t := feedTvgKey(s.TvgID, s.matchName())
		if t == "" || shared[t] {
			continue
		}
		k, name := provTvg{s.ProviderID, t}, feedlessName(s.matchName())
		if prev, ok := firstName[k]; !ok {
			firstName[k] = name
		} else if prev != name {
			shared[t] = true
		}
	}
	return shared
}

// mixedCallKeys finds the match keys (per stream, in each of keySets)
// streams of different stations carry. Providers copy tvg-ids between
// stations ("NBC 3 LAS VEGAS (KSNV)" and "NBC 20 FORT MYERS (WBBH)" both as
// nbcksnv.us), and a stream whose name gives no call sign takes a copied
// tvg-id's, so its name can link stations too. Under such a key, streams
// with a call sign only link to the same station.
func mixedCallKeys(calls []string, keySets ...[]string) map[string]bool {
	first := map[string]string{}
	mixed := map[string]bool{}
	for _, keys := range keySets {
		for i, k := range keys {
			if k == "" || calls[i] == "" || mixed[k] {
				continue
			}
			if prev, ok := first[k]; !ok {
				first[k] = calls[i]
			} else if prev != calls[i] {
				mixed[k] = true
			}
		}
	}
	return mixed
}

// feedlessName is NormalizeName without an "east" label: East is the
// default feed, so "A&E East" and "A&E" are one channel. Other feeds are
// told apart by feedTvgKey already.
func feedlessName(name string) string {
	words := strings.Fields(NormalizeName(name))
	return strings.Join(slices.DeleteFunc(words, func(w string) bool { return w == "east" }), " ")
}

// feedTvgKey is the tvg-id match key, split by feed: providers give the
// East and West (or +1) feeds of a channel the same tvg-id, but they air
// hours apart, so they must not become one channel or fail over into each
// other. East and unlabelled feeds share the plain key.
func feedTvgKey(tvg, name string) string {
	t := tvgKey(tvg)
	if t == "" {
		return ""
	}
	if v := FeedVariant(name); v != "" {
		return t + "|" + v
	}
	return t
}

var timeshiftRe = regexp.MustCompile(`\+\s?([1-9])(\s|$|h\b)`)

// FeedVariant names a channel's time-shifted feed from its name: "west"
// (also Pacific), "mountain", or "+1".."+9"; "" for East or no label.
func FeedVariant(name string) string {
	low := strings.ToLower(name)
	if m := timeshiftRe.FindStringSubmatch(low); m != nil {
		return "+" + m[1]
	}
	for _, w := range strings.Fields(NormalizeName(name)) {
		switch w {
		case "west", "pacific":
			return "west"
		case "mountain":
			return "mountain"
		}
	}
	return ""
}

func tvgKey(tvg string) string {
	t := strings.ToLower(strings.TrimSpace(tvg))
	if t == "" {
		return ""
	}
	return "tvg:" + t
}

// hasDup reports whether a sorted slice has a repeated element.
func hasDup(sorted []string) bool {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return true
		}
	}
	return false
}

// unionFind is a path-halving disjoint set over stream indexes.
type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &unionFind{parent: p}
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if rb < ra { // smaller root wins: grouping independent of union order
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
}
