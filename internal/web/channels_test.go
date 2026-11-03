package web

import (
	"slices"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestSortChannelsByName(t *testing.T) {
	for _, tt := range []struct {
		name     string
		in, want []string
	}{
		{"ignores case", []string{"bravo", "Alpha", "CHARLIE"}, []string{"Alpha", "bravo", "CHARLIE"}},
		{"numbers by value", []string{"ESPN 10", "ESPN 2", "ESPN 1"}, []string{"ESPN 1", "ESPN 2", "ESPN 10"}},
		{"accents beside plain letters", []string{"Zulu", "Écran", "Delta", "Foxtrot"}, []string{"Delta", "Écran", "Foxtrot", "Zulu"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chans := make([]db.Channel, len(tt.in))
			for i, n := range tt.in {
				chans[i].Name = n
			}
			sortChannelsByName(chans)
			got := make([]string, len(chans))
			for i, c := range chans {
				got[i] = c.Name
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	// Same name: the current order stands.
	chans := []db.Channel{{ID: 2, Name: "News"}, {ID: 1, Name: "news"}, {ID: 3, Name: "NEWS"}}
	sortChannelsByName(chans)
	if chans[0].ID != 2 || chans[1].ID != 1 || chans[2].ID != 3 {
		t.Errorf("ties moved: %+v", chans)
	}
}
