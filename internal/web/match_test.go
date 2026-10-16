package web

import (
	"slices"
	"testing"

	"github.com/sysadmin/mmp/internal/channel"
)

func TestNaturalCompare(t *testing.T) {
	got := []string{"sport 10", "sport 2", "sport 1", "sport 02b", "abc", "sport", "sport 100"}
	slices.SortFunc(got, naturalCompare)
	want := []string{"abc", "sport", "sport 1", "sport 2", "sport 02b", "sport 10", "sport 100"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFilterProposals(t *testing.T) {
	news := channel.Proposal{Key: "name:news 5", Name: "News 5", Providers: 2,
		StreamIDs: []int64{1, 2, 3, 4}, StreamNames: []string{"US: News 5", "UK: News 5", "US: News 5", "UK: News 5"},
		StreamGroups: []string{"US| NEWS", "UK| NEWS", "US| NEWS", "UK| NEWS"}, StreamProviders: []int64{1, 1, 2, 2},
		Groups: []string{"US| NEWS", "UK| NEWS"}}
	solo := channel.Proposal{Key: "name:solo", Name: "Solo 10", Providers: 2,
		StreamIDs: []int64{5, 6}, StreamNames: []string{"US: Solo", "UK: Solo"},
		StreamGroups: []string{"US| NEWS", "UK| NEWS"}, StreamProviders: []int64{1, 2}, Groups: []string{"US| NEWS", "UK| NEWS"}}
	single := channel.Proposal{Key: "name:one", Name: "One", Providers: 1, StreamIDs: []int64{7}, StreamNames: []string{"One"},
		StreamGroups: []string{"US| NEWS"}, StreamProviders: []int64{1}, Groups: []string{"US| NEWS"}}
	props := []channel.Proposal{solo, news, single}

	out, groups, atMin, hidden := filterProposals(props, matchFilter{MinProv: 2})
	if len(out) != 2 || out[0].Name != "News 5" || atMin != 2 || hidden != 1 || len(groups) != 2 || groups[1].Name != "US| NEWS" || groups[1].Count != 2 {
		t.Fatalf("no filter: out=%v groups=%v atMin=%d hidden=%d", out, groups, atMin, hidden)
	}
	// The group narrows each suggestion to its streams: Solo's only US
	// stream is at provider 1, so it drops below 2 providers.
	out, _, _, _ = filterProposals(props, matchFilter{MinProv: 2, Group: "US| NEWS"})
	if len(out) != 1 || !slices.Equal(out[0].StreamIDs, []int64{1, 3}) || out[0].Group != "US| NEWS" {
		t.Errorf("group filter: %+v", out)
	}
	out, _, _, _ = filterProposals(props, matchFilter{MinProv: 1, Query: "solo us"})
	if len(out) != 1 || out[0].Key != "name:solo" {
		t.Errorf("search: %+v", out)
	}
}
