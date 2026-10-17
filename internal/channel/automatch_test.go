package channel

import "testing"

func TestBouquetMode(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "auto": "auto", " Auto ": "auto", "none": "none", "NONE": "none", "Basic": "names"} {
		v := []string{in}
		if in == "" {
			v = nil
		}
		if got := bouquetMode(v); got != want {
			t.Errorf("bouquetMode(%q) = %s, want %s", in, got, want)
		}
	}
	if got := bouquetMode([]string{"none", "Basic"}); got != "names" {
		t.Errorf("two names = %s", got)
	}
}
