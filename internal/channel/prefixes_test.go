package channel

import (
	"slices"
	"testing"
)

func TestStripPrefixes(t *testing.T) {
	prefixes := []string{"AT&T", "SLING", "Verizon Fios"}
	tests := []struct{ in, want string }{
		{"AT&T: MTV ᴿᴬᵂ", "MTV ᴿᴬᵂ"},
		{"at&t | MTV", "MTV"},
		{"AT&T - A&E West", "A&E West"},
		{"SLING | AT&T: ESPN", "ESPN"}, // stacked labels
		{"Verizon Fios/HBO", "HBO"},
		{"AT&T SportsNet", "AT&T SportsNet"}, // no separator: part of the name
		{"AT&T:", "AT&T:"},                   // nothing would be left
		{"NFL: RedZone", "NFL: RedZone"},     // not listed
		{"  MTV  ", "MTV"},
	}
	for _, tt := range tests {
		if got := StripPrefixes(tt.in, prefixes); got != tt.want {
			t.Errorf("StripPrefixes(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := StripPrefixes("AT&T: MTV", nil); got != "AT&T: MTV" {
		t.Errorf("no prefixes: got %q", got)
	}
}

func TestParseIgnorePrefixes(t *testing.T) {
	got := ParseIgnorePrefixes(" AT&T: \nSLING, Directv |\n\nat&t\n sling ")
	want := []string{"AT&T", "SLING", "Directv"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestValidateIgnorePrefixes(t *testing.T) {
	if err := ValidateIgnorePrefixes([]string{"AT&T"}); err != nil {
		t.Errorf("valid list refused: %v", err)
	}
	long := []string{"this prefix is far too long to be a source label"}
	if err := ValidateIgnorePrefixes(long); err == nil {
		t.Error("long prefix accepted")
	}
	if err := ValidateIgnorePrefixes(make([]string, maxIgnorePrefixes+1)); err == nil {
		t.Error("too many prefixes accepted")
	}
}

// A labelled stream joins the plain one, and the suggested name is clean.
func TestMatchIgnoresPrefixesAndTags(t *testing.T) {
	prefixes := []string{"AT&T"}
	stream := func(id, prov int64, name string) Stream {
		return Stream{ID: id, ProviderID: prov, Name: name, MatchName: StripPrefixes(name, prefixes)}
	}
	got := Match([]Stream{
		stream(1, 1, "MTV"),
		stream(2, 2, "AT&T: MTV ᴿᴬᵂ"),
		stream(3, 2, "AT&T: MTV2"),
	}, nil, nil, nil)
	ps := byKey(got)
	p, ok := ps["name:mtv"]
	if !ok {
		t.Fatalf("no MTV proposal: %+v", got)
	}
	if p.Name != "MTV" || !slices.Equal(p.StreamIDs, []int64{1, 2}) {
		t.Errorf("MTV: name=%q streams=%v", p.Name, p.StreamIDs)
	}
	if p2, ok := ps["name:mtv2"]; !ok || p2.Name != "MTV2" {
		t.Errorf("MTV2 should stay its own channel: %+v", ps)
	}
	// Raw names are kept for the Match page.
	if !slices.Contains(p.StreamNames, "AT&T: MTV ᴿᴬᵂ") {
		t.Errorf("stream names = %q", p.StreamNames)
	}
}
