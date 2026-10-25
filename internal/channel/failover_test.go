package channel

import (
	"slices"
	"testing"
)

func TestSlotByProvider(t *testing.T) {
	e := func(id int64, prio int32) FailoverEntry { return FailoverEntry{StreamID: id, ProviderPriority: prio} }
	tests := []struct {
		name        string
		list, added []FailoverEntry
		want        []int64
	}{
		{"empty list", nil, []FailoverEntry{e(1, 0), e(2, 1)}, []int64{1, 2}},
		{"nothing added", []FailoverEntry{e(1, 1), e(2, 0)}, nil, []int64{1, 2}},
		{"best level goes first", []FailoverEntry{e(1, 1), e(2, 2)}, []FailoverEntry{e(9, 0)}, []int64{9, 1, 2}},
		{"worst level goes last", []FailoverEntry{e(1, 0), e(2, 1)}, []FailoverEntry{e(9, 5)}, []int64{1, 2, 9}},
		{"tie joins the end of its level", []FailoverEntry{e(1, 0), e(2, 0), e(3, 1)}, []FailoverEntry{e(9, 0)}, []int64{1, 2, 9, 3}},
		{"added keep their order", []FailoverEntry{e(1, 0), e(3, 2)}, []FailoverEntry{e(8, 1), e(9, 1)}, []int64{1, 8, 9, 3}},
		// A hand-made order (worse provider on top) is kept; the new stream
		// goes after the last stream at its level or better.
		{"hand order kept", []FailoverEntry{e(1, 1), e(2, 0), e(3, 2)}, []FailoverEntry{e(9, 1)}, []int64{1, 2, 9, 3}},
	}
	for _, tt := range tests {
		if got := SlotByProvider(tt.list, tt.added); !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
