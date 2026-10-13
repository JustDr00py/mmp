package channel

import (
	"regexp"
	"slices"
	"testing"
)

// threeProviders is the AC fixture: the same channels named differently at
// three providers (priority A=0, B=1, C=2), plus a decoy.
func threeProviders() []Stream {
	return []Stream{
		// Provider C listed first to prove failover order follows priority.
		{ID: 31, ProviderID: 3, ProviderPriority: 2, Name: "abc-east (us)"},                 // name only
		{ID: 32, ProviderID: 3, ProviderPriority: 2, Name: "ESPN 2 FHD", TvgID: "ESPN2.us"}, // tvg, case differs
		{ID: 33, ProviderID: 3, ProviderPriority: 2, Name: "|EN| Sky Sport Premier League"}, // rule
		{ID: 11, ProviderID: 1, ProviderPriority: 0, Name: "US: ABC East HD", TvgID: "abc.us", LogoURL: "http://logo/abc.png"},
		{ID: 12, ProviderID: 1, ProviderPriority: 0, Name: "ESPN2", TvgID: "espn2.us"},
		{ID: 13, ProviderID: 1, ProviderPriority: 0, Name: "ABC News"},                      // decoy: distinct channel
		{ID: 21, ProviderID: 2, ProviderPriority: 1, Name: "ABC EAST FHD", TvgID: "abc.us"}, // tvg links to A
		{ID: 22, ProviderID: 2, ProviderPriority: 1, Name: "Sky Sports PL HD"},              // rule
		{ID: 23, ProviderID: 2, ProviderPriority: 1, Name: "ESPN 2 SD"},                     // name links to C, tvg-less
	}
}

func skyRule() Rule {
	return Rule{ID: 1, Pattern: regexp.MustCompile(`(?i)sky ?sports? ?(pl|premier league)`), ChannelName: "Sky Sports Premier League"}
}

func byKey(ps []Proposal) map[string]Proposal {
	m := map[string]Proposal{}
	for _, p := range ps {
		m[p.Key] = p
	}
	return m
}

func TestMatchThreeProviders(t *testing.T) {
	got := byKey(Match(threeProviders(), nil, []Rule{skyRule()}))

	tests := []struct {
		key       string
		name      string
		tvg       string
		streams   []int64
		providers int
		matchedBy []string
	}{
		// tvg-id links A+B; normalized name pulls in C's tvg-less stream.
		{"tvg:abc.us", "ABC East", "abc.us", []int64{11, 21, 31}, 3, []string{ByTvgID, ByName}},
		// tvg-id across case links A+C; name links B (no tvg) to C.
		{"tvg:espn2.us", "ESPN2", "espn2.us", []int64{12, 23, 32}, 3, []string{ByTvgID, ByName}},
		// Admin regex groups names the normalizer can't.
		{"name:sky sports premier league", "Sky Sports Premier League", "", []int64{22, 33}, 2, []string{ByRule}},
		// Decoy stays alone.
		{"name:abc news", "ABC News", "", []int64{13}, 1, []string{}},
	}
	if len(got) != len(tests) {
		t.Fatalf("got %d proposals, want %d: %+v", len(got), len(tests), got)
	}
	for _, tt := range tests {
		p, ok := got[tt.key]
		if !ok {
			t.Errorf("missing proposal %q; have %v", tt.key, got)
			continue
		}
		if p.Name != tt.name || p.TvgID != tt.tvg || p.Providers != tt.providers {
			t.Errorf("%s: name=%q tvg=%q providers=%d", tt.key, p.Name, p.TvgID, p.Providers)
		}
		if !slices.Equal(p.StreamIDs, tt.streams) {
			t.Errorf("%s: streams=%v want %v (failover order by provider priority)", tt.key, p.StreamIDs, tt.streams)
		}
		if !slices.Equal(p.MatchedBy, tt.matchedBy) {
			t.Errorf("%s: matched_by=%v want %v", tt.key, p.MatchedBy, tt.matchedBy)
		}
		if p.ChannelID != 0 {
			t.Errorf("%s: unexpected existing channel %d", tt.key, p.ChannelID)
		}
	}
	if got["tvg:abc.us"].LogoURL != "http://logo/abc.png" {
		t.Errorf("logo not carried: %+v", got["tvg:abc.us"])
	}
}

func TestMatchAttachesToExisting(t *testing.T) {
	existing := []Existing{
		{ID: 7, Name: "ABC (East)"},                        // name match only
		{ID: 8, Name: "Something else", TvgID: "ESPN2.US"}, // tvg match
		{ID: 9, Name: "Sky Sports Premier League"},         // rule target
	}
	got := byKey(Match(threeProviders(), existing, []Rule{skyRule()}))
	for key, want := range map[string]int64{
		"tvg:abc.us":                     7,
		"tvg:espn2.us":                   8,
		"name:sky sports premier league": 9,
		"name:abc news":                  0,
	} {
		if got[key].ChannelID != want {
			t.Errorf("%s: channel_id=%d want %d", key, got[key].ChannelID, want)
		}
	}
	if p := got["tvg:espn2.us"]; p.Name != "Something else" || p.TvgID != "ESPN2.US" {
		t.Errorf("existing channel identity not kept: %+v", p)
	}
}

func TestMatchDeterministic(t *testing.T) {
	in := threeProviders()
	first := Match(in, nil, []Rule{skyRule()})
	slices.Reverse(in)
	second := Match(in, nil, []Rule{skyRule()})
	if len(first) != len(second) {
		t.Fatalf("len %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Key != second[i].Key || first[i].Name != second[i].Name ||
			!slices.Equal(first[i].StreamIDs, second[i].StreamIDs) {
			t.Errorf("proposal %d differs on reordered input: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestMatchFirstRuleWins(t *testing.T) {
	streams := []Stream{{ID: 1, ProviderID: 1, Name: "Foo Sports 1"}}
	rules := []Rule{
		{ID: 1, Pattern: regexp.MustCompile(`(?i)foo`), ChannelName: "Foo"},
		{ID: 2, Pattern: regexp.MustCompile(`(?i)sports`), ChannelName: "Sports"},
	}
	got := Match(streams, nil, rules)
	if len(got) != 1 || got[0].Name != "Foo" {
		t.Errorf("got %+v", got)
	}
}

func TestDisplayName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"US: ABC East FHD", "ABC East"},
		{"USA | NBC HD", "NBC"},
		{"USA Network", "USA Network"},
		{"Sky Sports (UK) 4K", "Sky Sports"},
		{"TF1 FR", "TF1 FR"},
		{"HD", "HD"},
		{"ESPN2", "ESPN2"},
	}
	for _, tt := range tests {
		if got := DisplayName(tt.in); got != tt.want {
			t.Errorf("DisplayName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
