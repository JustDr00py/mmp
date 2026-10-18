package source

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeBackbone(t *testing.T) {
	known := []string{"Strong", "Trex 4K"}
	for in, want := range map[string]string{
		"":               "",
		"  strong  ":     "Strong",
		"TREX   4k":      "Trex 4K",
		"New One":        "New One",
		"  new\tone  ":   "new one",
		NoBackbone:       "",
		" " + NoBackbone: "",
	} {
		if got, err := NormalizeBackbone(in, known); err != nil || got != want {
			t.Errorf("NormalizeBackbone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeBackbone(strings.Repeat("é", MaxBackboneLen), nil); err != nil {
		t.Errorf("%d characters: %v", MaxBackboneLen, err)
	}
	if _, err := NormalizeBackbone(strings.Repeat("é", MaxBackboneLen+1), nil); !errors.Is(err, ErrBackboneTooLong) {
		t.Errorf("too long: %v", err)
	}
}
