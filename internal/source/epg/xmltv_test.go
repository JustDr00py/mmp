package epg

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func parseFixture(t *testing.T) []Program {
	t.Helper()
	f, err := os.Open("testdata/guide.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Program
	if err := Parse(f, func(p Program) error { out = append(out, p); return nil }); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return out
}

func TestParseFixture(t *testing.T) {
	progs := parseFixture(t)
	if len(progs) != 3 {
		t.Fatalf("got %d programs, want 3 (junk skipped): %+v", len(progs), progs)
	}

	// +0300 offset applied correctly.
	p := progs[0]
	if p.Channel != "us.abc.east" || p.Title != "Evening News" || p.SubTitle != "Special Report" {
		t.Errorf("prog 0 = %+v", p)
	}
	if want := time.Date(2026, 10, 13, 18, 0, 0, 0, time.FixedZone("", 3*3600)); !p.Start.Equal(want) {
		t.Errorf("start = %v, want %v", p.Start, want)
	}
	if p.Stop.Sub(p.Start) != time.Hour {
		t.Errorf("duration = %v, want 1h", p.Stop.Sub(p.Start))
	}
	// Entity decoding.
	if p.Desc != "The day's headlines with & analysis." {
		t.Errorf("desc = %q", p.Desc)
	}

	// -0500 offset; first non-empty title wins across languages.
	p = progs[1]
	if p.Title != "Abend Nachrichten" || p.Desc != "Nachrichten" {
		t.Errorf("prog 1 = %+v", p)
	}
	if _, off := p.Start.Zone(); off != -5*3600 {
		t.Errorf("offset = %d, want -18000", off)
	}

	// "20261013200000+0200" (no space) parses; missing sub-title/desc fine.
	p = progs[2]
	if p.Channel != "de.das.erste" || p.Title != "Tagesschau" || p.SubTitle != "" {
		t.Errorf("prog 2 = %+v", p)
	}
	if _, off := p.Start.Zone(); off != 2*3600 {
		t.Errorf("offset = %d, want 7200", off)
	}
}

func TestParseTime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"20261013060000 +0000", time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)},
		{"20261013060000", time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := ParseTime(c.in)
		if err != nil {
			t.Errorf("ParseTime(%q): %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("ParseTime(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseTime("not a time"); err == nil {
		t.Error("want error for garbage")
	}
}

func TestParseEmitError(t *testing.T) {
	sentinel := errors.New("halt")
	doc := `<?xml version="1.0"?><tv>
<programme start="20261013180000 +0000" stop="20261013190000 +0000" channel="c"><title>A</title></programme>
<programme start="20261013190000 +0000" stop="20261013200000 +0000" channel="c"><title>B</title></programme>
</tv>`
	n := 0
	err := Parse(strings.NewReader(doc), func(Program) error {
		n++
		return sentinel
	})
	if !errors.Is(err, sentinel) || n != 1 {
		t.Errorf("err=%v n=%d", err, n)
	}
}

func TestParseLatin1(t *testing.T) {
	// encoding="ISO-8859-1" with a mutated vowel in the title.
	doc := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><tv>" +
		"<programme start=\"20261013180000 +0100\" stop=\"20261013190000 +0100\" channel=\"c\">" +
		"<title>K\xf6che</title></programme></tv>"
	var got string
	err := Parse(strings.NewReader(doc), func(p Program) error { got = p.Title; return nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != "Köche" {
		t.Errorf("title = %q, want Köche", got)
	}
}
