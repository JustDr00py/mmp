package channel

import (
	"regexp"
	"slices"
	"strings"
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
	got := byKey(Match(threeProviders(), nil, []Rule{skyRule()}, nil))

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
	got := byKey(Match(threeProviders(), existing, []Rule{skyRule()}, nil))
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
	first := Match(in, nil, []Rule{skyRule()}, nil)
	slices.Reverse(in)
	second := Match(in, nil, []Rule{skyRule()}, nil)
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
	got := Match(streams, nil, rules, nil)
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
		{"MTV ᴿᴬᵂ", "MTV"},
		{"US: Fox News ᴴᴰ", "Fox News"},
	}
	for _, tt := range tests {
		if got := DisplayName(tt.in); got != tt.want {
			t.Errorf("DisplayName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFeedVariant(t *testing.T) {
	for name, want := range map[string]string{
		"US - A&E EAST HD ◉": "", "US - A&E WEST HD": "west", "A&E": "", "HBO Pacific": "west",
		"Starz Mountain FHD": "mountain", "UK: Channel 4 +1": "+1", "ITV+1 HD": "+1", "E4 +2h": "+2",
		"Westerns Channel": "", "Disney+ HD": "", "Sky Sports+": "", "BBC One West Midlands": "west",
	} {
		if got := FeedVariant(name); got != want {
			t.Errorf("FeedVariant(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestMatchSplitsFeeds: providers tag East and West feeds with the same
// tvg-id; they must stay separate channels (and never fail over into each
// other), while East joins an unlabelled feed and the existing channel.
func TestMatchSplitsFeeds(t *testing.T) {
	streams := []Stream{
		{ID: 1, ProviderID: 1, Name: "US - A&E EAST HD ◉", TvgID: "aande.us"},
		{ID: 2, ProviderID: 1, Name: "US - A&E WEST HD", TvgID: "aande.us"},
		{ID: 3, ProviderID: 2, Name: "US| A&E HD", TvgID: "AandE.us"},
		{ID: 4, ProviderID: 2, Name: "US| A&E WEST", TvgID: "AandE.us"},
		{ID: 5, ProviderID: 1, Name: "UK: Channel 4 +1", TvgID: "channel4.uk"},
		{ID: 6, ProviderID: 1, Name: "UK: Channel 4", TvgID: "channel4.uk"},
	}
	byKey := map[string]Proposal{}
	for _, p := range Match(streams, nil, nil, nil) {
		byKey[p.Key] = p
	}
	if p := byKey["tvg:aande.us"]; !slices.Equal(p.StreamIDs, []int64{1, 3}) {
		t.Errorf("A&E (East) = %v, want streams 1 and 3", p.StreamIDs)
	}
	if p := byKey["tvg:aande.us|west"]; !slices.Equal(p.StreamIDs, []int64{2, 4}) || !strings.Contains(strings.ToLower(p.Name), "west") {
		t.Errorf("A&E West = %+v, want streams 2 and 4", p)
	}
	if p := byKey["tvg:channel4.uk|+1"]; !slices.Equal(p.StreamIDs, []int64{5}) {
		t.Errorf("Channel 4 +1 = %v", p.StreamIDs)
	}

	// An existing "A&E" channel takes the East feeds only; West is new.
	props := Match(streams[:4], []Existing{{ID: 8, Name: "A&E", TvgID: "AandE.us"}}, nil, nil)
	for _, p := range props {
		west := strings.HasSuffix(p.Key, "|west")
		if west && p.ChannelID != 0 {
			t.Errorf("West attached to the existing East channel: %+v", p)
		}
		if !west && p.ChannelID != 8 {
			t.Errorf("East not attached to the existing channel: %+v", p)
		}
	}
}

// TestMatchSharedTvgID: some providers give one tvg-id to different
// channels (Spectrum SportsNet's Lakers and Dodgers feeds). When a provider
// carries differently named streams under one tvg-id, that tvg-id links
// only streams whose names match too; elsewhere tvg-id matching is as
// before (FS1 and Fox Sports 1 are one channel).
func TestMatchSharedTvgID(t *testing.T) {
	streams := []Stream{
		{ID: 1, ProviderID: 1, Name: "US: Spectrum SportsNet Lakers HD", TvgID: "SpectrumSportsNet.us"},
		{ID: 2, ProviderID: 1, Name: "US: Spectrum SportsNet Dodgers HD", TvgID: "SpectrumSportsNet.us"},
		{ID: 3, ProviderID: 2, Name: "Spectrum SportsNet Lakers", TvgID: "spectrumsportsnet.us"},
		{ID: 4, ProviderID: 2, Name: "Spectrum SportsNet Dodgers FHD", TvgID: "spectrumsportsnet.us"},
		{ID: 5, ProviderID: 1, Name: "US: FS1", TvgID: "fs1.us"},
		{ID: 6, ProviderID: 2, Name: "Fox Sports 1", TvgID: "FS1.us"},
		{ID: 7, ProviderID: 1, Name: "US: A&E", TvgID: "aande.us"},      // East is the default feed:
		{ID: 8, ProviderID: 1, Name: "US: A&E East", TvgID: "aande.us"}, // not two channels
		{ID: 9, ProviderID: 2, Name: "A&E HD", TvgID: "aande.us"},
	}
	groups := map[string][]int64{}
	for _, p := range Match(streams, nil, nil, nil) {
		groups[p.Name] = p.StreamIDs
	}
	for name, want := range map[string][]int64{
		"Spectrum SportsNet Lakers":  {1, 3},
		"Spectrum SportsNet Dodgers": {2, 4},
		"FS1":                        {5, 6},
		"A&E":                        {7, 8, 9},
	} {
		if got := groups[name]; !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v (all: %v)", name, got, want, groups)
		}
	}

	// An existing Dodgers channel with the shared tvg-id takes the Dodgers
	// streams only.
	for _, p := range Match(streams[:4], []Existing{{ID: 9, Name: "Spectrum SportsNet Dodgers", TvgID: "SpectrumSportsNet.us"}}, nil, nil) {
		dodgers := slices.Contains(p.StreamIDs, 2)
		if dodgers != (p.ChannelID == 9) {
			t.Errorf("proposal %v attached to channel %d", p.StreamIDs, p.ChannelID)
		}
	}
}

// TestMatchMerges: a merge joins the groups holding its two keys, whether
// a key is a tvg-id or a lone stream's, and a key no stream carries
// changes nothing.
func TestMatchMerges(t *testing.T) {
	streams := []Stream{
		{ID: 1, ProviderID: 1, Name: "KTLA Los Angeles", TvgID: "ca.KTLA"},
		{ID: 2, ProviderID: 2, Name: "KTLA LA", TvgID: "ca.KTLA"},
		{ID: 3, ProviderID: 3, Name: "KTLA 5", TvgID: "ktla.us"},
		{ID: 4, ProviderID: 4, Name: "CW 5 Los Angeles"},
		{ID: 5, ProviderID: 1, Name: "KCAL"},
	}
	if n := len(Match(streams, nil, nil, nil)); n != 4 {
		t.Fatalf("without merges: %d proposals, want 4", n)
	}
	merges := []Merge{
		{A: "tvg:ktla.us", B: "tvg:ca.ktla"},
		{A: "tvg:ca.ktla", B: streamKey(4)},
		{A: "tvg:gone.us", B: streamKey(5)},
	}
	got := byKey(Match(streams, nil, nil, merges))
	if len(got) != 2 {
		t.Fatalf("with merges: %d proposals %v, want 2", len(got), got)
	}
	p, ok := got["tvg:ca.ktla"]
	if !ok || !slices.Equal(p.StreamIDs, []int64{1, 2, 3, 4}) || p.Providers != 4 {
		t.Errorf("merged KTLA = %+v", p)
	}
	if _, ok := got["name:kcal"]; !ok {
		t.Errorf("KCAL changed by a merge with a missing key: %v", got)
	}
}

// TestMatchMergesWithChannels: once one side of a merge is a channel, new
// streams carrying the other side's key are offered to that channel, and
// a group merged straight into a channel joins it.
func TestMatchMergesWithChannels(t *testing.T) {
	streams := []Stream{{ID: 3, ProviderID: 3, Name: "KTLA 5", TvgID: "ktla.us"}}
	existing := []Existing{{ID: 12, Name: "KTLA Los Angeles", TvgID: "ca.KTLA"}, {ID: 13, Name: "KCAL"}}
	for _, tc := range []struct {
		name   string
		merges []Merge
		want   int64
	}{
		{"no merge", nil, 0},
		{"merged with the channel's tvg-id", []Merge{{A: "tvg:ca.ktla", B: "tvg:ktla.us"}}, 12},
		{"merged with the channel's name", []Merge{{A: "name:ktla los angeles", B: "tvg:ktla.us"}}, 12},
		{"merged into the channel", []Merge{{A: ChannelKey(13), B: "tvg:ktla.us"}}, 13},
		{"merged into a deleted channel", []Merge{{A: ChannelKey(99), B: "tvg:ktla.us"}}, 0},
		{"the explicit channel wins", []Merge{{A: "tvg:ca.ktla", B: "tvg:ktla.us"}, {A: ChannelKey(13), B: "name:ktla 5"}}, 13},
	} {
		got := Match(streams, existing, nil, tc.merges)
		if len(got) != 1 || got[0].ChannelID != tc.want {
			t.Errorf("%s: %+v, want channel %d", tc.name, got, tc.want)
		}
	}
}
