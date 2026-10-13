package output

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source/epg"
)

var guideNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func newGuideHandlers(t *testing.T) (*fakeStore, http.Handler) {
	t.Helper()
	hs, h := newTestHandlers(t)
	hs.Now = func() time.Time { return guideNow }
	store := hs.Store.(*fakeStore)
	est := time.FixedZone("EST", -5*3600)
	store.guide = map[string][]db.ListGuideProgrammesRow{
		"news.us": {
			{TvgKey: "news.us", ID: 501, Title: "Morning <News> & Weather", SubTitle: "Ep. 1", Description: "Line one\nline two \x01",
				StartTime: time.Date(2026, 10, 13, 7, 0, 0, 0, est), StopTime: time.Date(2026, 10, 13, 8, 0, 0, 0, est)},
			{TvgKey: "news.us", ID: 502, Title: "Noon", StartTime: guideNow, StopTime: guideNow.Add(30 * time.Minute)},
		},
		"sports.uk": {
			{TvgKey: "sports.uk", Title: "Match", StartTime: guideNow, StopTime: guideNow.Add(2 * time.Hour)},
		},
	}
	// Admin view: News, a channel without tvg-id, and two channels sharing
	// a tvg-id that differs only in case.
	store.all = append(store.all,
		db.ListEnabledChannelsRow{ID: 12, Number: i32(200), Name: "Sports UK", TvgID: "Sports.UK", LogoUrl: "http://logo/s.png?a=1&b=2"},
		db.ListEnabledChannelsRow{ID: 13, Name: "Sports UK Backup", TvgID: "sports.uk"},
	)
	return store, h
}

func TestTokenGuideCustomer(t *testing.T) {
	store, h := newGuideHandlers(t)
	code, hdr, body := get(t, h, "/epg/tok-alice.xml")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/xml") {
		t.Fatalf("guide = %d %s", code, hdr.Get("Content-Type"))
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE tv SYSTEM "xmltv.dtd">
<tv generator-info-name="MMP">
 <channel id="news.us">
  <display-name>News &#34;24&#34;</display-name>
  <display-name>101</display-name>
  <icon src="http://logo/n.png"></icon>
 </channel>
 <programme start="20261013120000 +0000" stop="20261013130000 +0000" channel="news.us">
  <title>Morning &lt;News&gt; &amp; Weather</title>
  <sub-title>Ep. 1</sub-title>
  <desc>Line one&#xA;line two ` + "�" + `</desc>
 </programme>
 <programme start="20261013120000 +0000" stop="20261013123000 +0000" channel="news.us">
  <title>Noon</title>
 </programme>
</tv>
`
	if body != want {
		t.Errorf("guide mismatch\n got: %s\nwant: %s", body, want)
	}
	// Bouquet filtering: only alice's channel was ever queried.
	if len(store.fetchs) != 1 || fmt.Sprint(store.fetchs[0].TvgKeys) != "[news.us]" {
		t.Errorf("fetches = %+v", store.fetchs)
	}
	if p := store.fetchs[0]; !p.Since.Equal(guideNow.Add(-guideBehind)) || !p.Until.Equal(guideNow.Add(guideAhead)) {
		t.Errorf("window = %v .. %v", p.Since, p.Until)
	}
}

func TestXMLTVPHPAdminDedupesAndParses(t *testing.T) {
	store, h := newGuideHandlers(t)
	code, _, body := get(t, h, "/xmltv.php?username=root&password=root-pw1")
	if code != 200 {
		t.Fatalf("xmltv.php = %d", code)
	}
	if n := strings.Count(body, "<channel "); n != 2 {
		t.Errorf("channels = %d, want 2 (no tvg-id dropped, case-duplicate merged):\n%s", n, body)
	}
	if !strings.Contains(body, `<channel id="Sports.UK">`) || !strings.Contains(body, `channel="Sports.UK"`) {
		t.Errorf("programme must reference the listed channel id:\n%s", body)
	}
	if !strings.Contains(body, `src="http://logo/s.png?a=1&amp;b=2"`) {
		t.Errorf("icon not escaped:\n%s", body)
	}
	if got := fmt.Sprint(store.fetchs[0].TvgKeys); got != "[news.us sports.uk]" {
		t.Errorf("keys = %s", got)
	}

	// MMP's own XMLTV ingest reads the output back losslessly.
	var progs []epg.Programme
	if err := epg.Parse(strings.NewReader(body), func(p epg.Programme) error {
		progs = append(progs, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(progs) != 3 || progs[0].Channel != "news.us" || progs[0].Title != "Morning <News> & Weather" ||
		!progs[0].Start.Equal(guideNow) || !progs[2].Stop.Equal(guideNow.Add(2*time.Hour)) {
		t.Errorf("round trip = %+v", progs)
	}
}

func TestGuideBatches(t *testing.T) {
	store := &fakeStore{guide: map[string][]db.ListGuideProgrammesRow{}}
	var chans []Channel
	for i := range 250 {
		id := fmt.Sprintf("ch%03d", i)
		chans = append(chans, Channel{ID: int64(i), Name: id, TvgID: id})
		store.guide[id] = []db.ListGuideProgrammesRow{{TvgKey: id, Title: "x", StartTime: guideNow, StopTime: guideNow.Add(time.Hour)}}
	}
	var buf bytes.Buffer
	fetch := func(ctx context.Context, keys []string) ([]db.ListGuideProgrammesRow, error) {
		return store.ListGuideProgrammes(ctx, db.ListGuideProgrammesParams{TvgKeys: keys})
	}
	if err := WriteXMLTV(context.Background(), &buf, chans, fetch); err != nil {
		t.Fatal(err)
	}
	if len(store.fetchs) != 3 || len(store.fetchs[0].TvgKeys) != guideBatch || len(store.fetchs[2].TvgKeys) != 50 {
		t.Errorf("batches = %d", len(store.fetchs))
	}
	if n := strings.Count(buf.String(), "<programme "); n != 250 {
		t.Errorf("programmes = %d", n)
	}
	// No channels at all is still a valid, empty guide.
	buf.Reset()
	if err := WriteXMLTV(context.Background(), &buf, nil, fetch); err != nil || !strings.Contains(buf.String(), "</tv>") {
		t.Errorf("empty guide = %q %v", buf.String(), err)
	}
}

func TestGuideAuthFailures(t *testing.T) {
	_, h := newGuideHandlers(t)
	for _, p := range []string{
		"/xmltv.php",
		"/xmltv.php?username=alice&password=wrong-pw1",
		"/xmltv.php?username=old&password=old-pw1",
		"/epg/nope.xml",
		"/epg/tok-off.xml",
	} {
		if code, _, body := get(t, h, p); code != http.StatusUnauthorized || strings.Contains(body, "<tv") {
			t.Errorf("%s = %d", p, code)
		}
	}
}

// TestGuideValidatesAgainstDTD is the 5.5 AC: the output validates against
// the XMLTV DTD (the subset covering the elements MMP writes).
func TestGuideValidatesAgainstDTD(t *testing.T) {
	xmllint, err := exec.LookPath("xmllint")
	if err != nil {
		t.Skip("xmllint not installed")
	}
	_, h := newGuideHandlers(t)
	dtd, err := filepath.Abs("testdata/xmltv-subset.dtd")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/xmltv.php?username=root&password=root-pw1", "/epg/tok-alice.xml"} {
		_, _, body := get(t, h, p)
		f := filepath.Join(t.TempDir(), "guide.xml")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(xmllint, "--noout", "--nonet", "--dtdvalid", dtd, f).CombinedOutput()
		if err != nil {
			t.Errorf("%s does not validate: %v\n%s", p, err, out)
		}
	}
}
