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
}

// Match groups streams that are the same channel across providers. Streams
// are linked transitively by a shared tvg-id (case-insensitive), then by a
// shared NormalizeName key, then by the first rule (in slice order) their
// name matches. Each group becomes one Proposal, attached to an existing
// channel when the group shares a tvg-id or name key with it. Output is
// deterministic for a given input.
func Match(streams []Stream, existing []Existing, rules []Rule) []Proposal {
	n := len(streams)
	uf := newUnionFind(n)
	owner := map[string]int{} // match key -> first stream index carrying it
	link := func(i int, key string) {
		if j, ok := owner[key]; ok {
			uf.union(i, j)
		} else {
			owner[key] = i
		}
	}

	ruleOf := make([]*Rule, n)
	for i, s := range streams {
		if t := tvgKey(s.TvgID); t != "" {
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
		if t := tvgKey(c.TvgID); t != "" {
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
		out = append(out, buildProposal(streams, idx, ruleOf, byTvg, byName, existingByID))
	}
	slices.SortFunc(out, func(a, b Proposal) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

func buildProposal(streams []Stream, idx []int, ruleOf []*Rule,
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
		providers[s.ProviderID] = true
		if t := tvgKey(s.TvgID); t != "" {
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
