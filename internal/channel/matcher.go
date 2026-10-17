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
	TvgID            string
	LogoURL          string
	Group            string // the provider's group title ("US| SPORTS")
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
	ByTvgID = "tvg_id"
	ByName  = "name"
	ByRule  = "rule"
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
	Providers int      `json:"providers"`  // distinct providers in the group
	// Group is the provider group of the first stream; Groups every
	// distinct provider group in the proposal (for filtering).
	Group  string   `json:"group"`
	Groups []string `json:"groups"`
	// Per stream, in failover order: raw name, provider group and provider
	// (for searching and narrowing a proposal to one provider group).
	StreamNames     []string `json:"-"`
	StreamGroups    []string `json:"-"`
	StreamProviders []int64  `json:"-"`
}

// InGroup narrows p to its streams in provider group g. ok is false when
// none of its streams are in g.
func (p Proposal) InGroup(g string) (Proposal, bool) {
	q := p
	q.StreamIDs, q.StreamNames, q.StreamGroups, q.StreamProviders = nil, nil, nil, nil
	provs := map[int64]bool{}
	for i, sg := range p.StreamGroups {
		if sg != g {
			continue
		}
		q.StreamIDs = append(q.StreamIDs, p.StreamIDs[i])
		q.StreamNames = append(q.StreamNames, p.StreamNames[i])
		q.StreamGroups = append(q.StreamGroups, sg)
		q.StreamProviders = append(q.StreamProviders, p.StreamProviders[i])
		provs[p.StreamProviders[i]] = true
	}
	q.Providers, q.Group, q.Groups = len(provs), g, []string{g}
	return q, len(q.StreamIDs) > 0
}

// Match groups streams that are the same channel across providers. Streams
// are linked transitively by a shared tvg-id (case-insensitive), then by a
// shared NormalizeName key, then by the first rule (in slice order) their
// name matches. A tvg-id that one provider gives to differently named
// streams (Spectrum SportsNet's Lakers and Dodgers feeds) covers several
// channels, so under it streams only link when their names match too. Each
// group becomes one Proposal, attached to an existing channel when the
// group shares a tvg-id or name key with it. Output is deterministic for a
// given input.
func Match(streams []Stream, existing []Existing, rules []Rule) []Proposal {
	n := len(streams)
	uf := newUnionFind(n)
	shared := sharedTvgKeys(streams)
	tvgKeyOf := func(tvg, name string) string {
		t := feedTvgKey(tvg, name)
		if t != "" && shared[t] {
			t += "|" + feedlessName(name)
		}
		return t
	}
	tvgKeys := make([]string, n)
	for i, s := range streams {
		tvgKeys[i] = tvgKeyOf(s.TvgID, s.Name)
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
	for i, s := range streams {
		if k := NormalizeName(s.Name); k != "" {
			link(i, "name:"+k)
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

	groups := map[int][]int{}
	for i := range streams {
		root := uf.find(i)
		groups[root] = append(groups[root], i)
	}

	byTvg, byName := map[string]int64{}, map[string]int64{}
	existingByID := make(map[int64]Existing, len(existing))
	for _, c := range existing { // first (lowest ID) wins ties
		existingByID[c.ID] = c
		if t := tvgKeyOf(c.TvgID, c.Name); t != "" {
			if _, ok := byTvg[t]; !ok {
				byTvg[t] = c.ID
			}
		}
		if k := NormalizeName(c.Name); k != "" {
			if _, ok := byName["name:"+k]; !ok {
				byName["name:"+k] = c.ID
			}
		}
	}

	out := make([]Proposal, 0, len(groups))
	for _, idx := range groups {
		out = append(out, buildProposal(streams, tvgKeys, idx, ruleOf, byTvg, byName, existingByID))
	}
	slices.SortFunc(out, func(a, b Proposal) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

func buildProposal(streams []Stream, streamTvgKeys []string, idx []int, ruleOf []*Rule,
	byTvg, byName map[string]int64, existing map[int64]Existing) Proposal {
	// Failover order: preferred providers first, then stream id.
	slices.SortFunc(idx, func(a, b int) int {
		return cmp.Or(
			cmp.Compare(streams[a].ProviderPriority, streams[b].ProviderPriority),
			cmp.Compare(streams[a].ID, streams[b].ID),
		)
	})

	var p Proposal
	var tvgKeys, nameKeys []string
	var rule *Rule
	providers := map[int64]bool{}
	for _, i := range idx {
		s := streams[i]
		p.StreamIDs = append(p.StreamIDs, s.ID)
		p.StreamNames = append(p.StreamNames, s.Name)
		p.StreamGroups = append(p.StreamGroups, s.Group)
		p.StreamProviders = append(p.StreamProviders, s.ProviderID)
		if len(p.StreamIDs) == 1 {
			p.Group = s.Group
		}
		if !slices.Contains(p.Groups, s.Group) {
			p.Groups = append(p.Groups, s.Group)
		}
		providers[s.ProviderID] = true
		if t := streamTvgKeys[i]; t != "" {
			tvgKeys = append(tvgKeys, t)
			if p.TvgID == "" {
				p.TvgID = strings.TrimSpace(s.TvgID)
			}
		}
		if k := NormalizeName(s.Name); k != "" {
			nameKeys = append(nameKeys, "name:"+k)
		}
		if p.LogoURL == "" {
			p.LogoURL = s.LogoURL
		}
		if rule == nil && ruleOf[i] != nil {
			rule = ruleOf[i]
		}
	}
	p.Providers = len(providers)
	slices.Sort(tvgKeys)
	slices.Sort(nameKeys)

	// MatchedBy: methods that actually linked two or more streams.
	p.MatchedBy = []string{}
	if hasDup(tvgKeys) {
		p.MatchedBy = append(p.MatchedBy, ByTvgID)
	}
	if hasDup(nameKeys) {
		p.MatchedBy = append(p.MatchedBy, ByName)
	}
	if rule != nil {
		p.MatchedBy = append(p.MatchedBy, ByRule)
	}

	// Stable key: smallest tvg key, else the rule target, else smallest
	// name key, else first stream.
	switch {
	case len(tvgKeys) > 0:
		p.Key = tvgKeys[0]
	case rule != nil:
		p.Key = "name:" + NormalizeName(rule.ChannelName)
	case len(nameKeys) > 0:
		p.Key = nameKeys[0]
	default:
		p.Key = "stream:" + strconv.FormatInt(p.StreamIDs[0], 10)
	}

	// Existing channel: tvg-id beats rule target beats name.
	for _, t := range tvgKeys {
		if id, ok := byTvg[t]; ok {
			p.ChannelID = id
			break
		}
	}
	if p.ChannelID == 0 && rule != nil {
		p.ChannelID = byName["name:"+NormalizeName(rule.ChannelName)]
	}
	if p.ChannelID == 0 {
		for _, k := range nameKeys {
			if id, ok := byName[k]; ok {
				p.ChannelID = id
				break
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
		p.Name = DisplayName(streams[idx[0]].Name)
	}
	return p
}

// DisplayName tidies a provider stream name for use as a channel name,
// keeping its casing: "US: ABC East FHD" -> "ABC East".
func DisplayName(name string) string {
	s := strings.TrimSpace(name)

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
		t := feedTvgKey(s.TvgID, s.Name)
		if t == "" || shared[t] {
			continue
		}
		k, name := provTvg{s.ProviderID, t}, feedlessName(s.Name)
		if prev, ok := firstName[k]; !ok {
			firstName[k] = name
		} else if prev != name {
			shared[t] = true
		}
	}
	return shared
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
