package logos

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// png is the smallest thing http.DetectContentType calls a PNG.
var png = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))

func writeFiles(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, png, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// testLibrary is a small library shaped like tv-logos.
func testLibrary(t *testing.T) *Library {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, filepath.Join(dir, libraryDir),
		"countries/united-states/hgtv-us.png",
		"countries/united-states/history-channel-us.png",
		"countries/united-states/tru-tv-us.png",
		"countries/united-states/usa-us.png",
		"countries/united-states/usa-network-hz-us.png",
		"countries/united-states/abc-us.png",
		"countries/united-states/abc-logo-2013-garnet-us.png",
		"countries/united-states/a-and-e-us.png",
		"countries/united-states/espn-plus-us.png",
		"countries/united-states/espn-us.png",
		"countries/united-states/us-local/cw/cw-5-ktla-us.png",
		"countries/united-states/us-local/cbs-2-kcbs-us.png",
		"countries/united-states/cbs-news-los-angeles-us.png",
		"countries/germany/history-de.png",
		"countries/germany/hgtv-de.png",
		"countries/germany/zdf-de.png",
		"countries/united-kingdom/sky-news-uk.png",
		"countries/united-kingdom/hgtv-uk.png",
		"misc/sports/basketball/los-angeles-lakers.png",
		"README.md",
		".hidden/secret.png",
	)
	l, err := Open(dir, "", "https://tv.example", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSuggest(t *testing.T) {
	l := testLibrary(t)
	for _, tc := range []struct{ name, prefer, want string }{
		{"USA HGTV East", "", "countries/united-states/hgtv-us.png"},
		{"HGTV", "us", "countries/united-states/hgtv-us.png"},
		{"HGTV", "uk", "countries/united-kingdom/hgtv-uk.png"},
		{"UK: HGTV HD", "us", "countries/united-kingdom/hgtv-uk.png"}, // named beats preferred
		{"USA History (SHD)", "", "countries/united-states/history-channel-us.png"},
		{"USA truTV West", "", "countries/united-states/tru-tv-us.png"},                    // joined words
		{"USA A&E", "", "countries/united-states/a-and-e-us.png"},                          // & is "and"
		{"ESPN+", "us", "countries/united-states/espn-plus-us.png"},                        // + is "plus", not ESPN
		{"ESPN", "us", "countries/united-states/espn-us.png"},                              // not ESPN+
		{"ABC East", "us", "countries/united-states/abc-us.png"},                           // plain before its variants
		{"KTLA Los Angeles", "us", "countries/united-states/us-local/cw/cw-5-ktla-us.png"}, // call letters decide
		{"Sky News UK", "", "countries/united-kingdom/sky-news-uk.png"},                    // trailing country
	} {
		hits := l.Suggest(tc.name, tc.prefer, 3)
		if len(hits) == 0 || hits[0].Path != libraryDir+"/"+tc.want {
			t.Errorf("Suggest(%q, %q) = %v, want %s first", tc.name, tc.prefer, paths(hits), tc.want)
		}
	}
	if hits := l.Suggest("USA HGTV East", "", 1); !hits[0].Confident() || hits[0].URL != "https://tv.example/logos/library/countries/united-states/hgtv-us.png" {
		t.Errorf("hit = %+v", hits[0])
	}
	// "USA" names a country, not a logo: usa-us.png is not offered for
	// every "USA …" channel.
	for _, h := range l.Suggest("USA Totally Unknown", "", 5) {
		t.Errorf("unknown channel suggested %s", h.Path)
	}
	if got := l.PreferredCountry([]string{"USA HGTV", "USA TNT", "UK: Sky News", "Random"}); got != "us" {
		t.Errorf("PreferredCountry = %q", got)
	}
}

func paths(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return out
}

func TestSearchAndIndex(t *testing.T) {
	l := testLibrary(t)
	if lib, up := l.Counts(); lib != 19 || up != 0 {
		t.Errorf("Counts = %d, %d (README and hidden files must be skipped)", lib, up)
	}
	got := paths(l.Search("ktla", 10))
	if len(got) != 1 || !strings.HasSuffix(got[0], "cw-5-ktla-us.png") {
		t.Errorf("Search(ktla) = %v", got)
	}
	got = paths(l.Search("hist", 10)) // prefixes
	if len(got) != 2 {
		t.Errorf("Search(hist) = %v", got)
	}
	got = paths(l.Search("hgtv germany", 10)) // folder words narrow
	if len(got) != 1 || !strings.HasSuffix(got[0], "hgtv-de.png") {
		t.Errorf("Search(hgtv germany) = %v", got)
	}
	if got := l.Search("", 10); len(got) != 0 {
		t.Errorf("empty search without uploads = %v", paths(got))
	}
	p, ok := l.PathOf("https://tv.example/logos/library/countries/germany/zdf-de.png")
	if !ok || p != "library/countries/germany/zdf-de.png" {
		t.Errorf("PathOf = %q %v", p, ok)
	}
	if _, ok := l.PathOf("https://elsewhere.example/logos/x.png"); ok {
		t.Error("PathOf accepted another host")
	}
}

func TestUpload(t *testing.T) {
	l := testLibrary(t)
	e, err := l.Upload("KTLA 5 (Los Angeles).PNG", png)
	if err != nil || e.Path != "uploads/ktla-5-los-angeles.png" || !e.Uploaded {
		t.Fatalf("Upload = %+v, %v", e, err)
	}
	e2, err := l.Upload("ktla 5 los angeles", png) // same name: kept apart
	if err != nil || e2.Path != "uploads/ktla-5-los-angeles-2.png" {
		t.Fatalf("second Upload = %+v, %v", e2, err)
	}
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	if e, err := l.Upload("my.svg", svg); err != nil || e.Name != "my.svg" {
		t.Fatalf("svg Upload = %+v, %v", e, err)
	}
	// Content decides, not the name.
	var bad ErrUpload
	if _, err := l.Upload("evil.png", []byte("<html><script>alert(1)</script>")); !errors.As(err, &bad) {
		t.Errorf("html upload err = %v", err)
	}
	if _, err := l.Upload("big.png", append(png, make([]byte, MaxUpload)...)); !errors.As(err, &bad) {
		t.Errorf("big upload err = %v", err)
	}
	// Uploads lead searches and are suggested.
	if got := l.Search("ktla", 10); len(got) != 3 || !got[0].Uploaded {
		t.Errorf("Search after upload = %v", paths(got))
	}
	if got := l.Uploads(); len(got) != 3 {
		t.Errorf("Uploads = %v", paths(got))
	}
	if hits := l.Suggest("KTLA 5 Los Angeles", "us", 1); hits[0].Path != "uploads/ktla-5-los-angeles.png" {
		t.Errorf("Suggest after upload = %v", paths(hits))
	}
	for _, name := range []string{"../library/countries/germany/zdf-de.png", ".upload-x", ""} {
		if err := l.DeleteUpload(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("DeleteUpload(%q) = %v", name, err)
		}
	}
	if err := l.DeleteUpload("my.svg"); err != nil {
		t.Fatal(err)
	}
	if _, up := l.Counts(); up != 2 {
		t.Errorf("uploads after delete = %d", up)
	}
}

func TestHandler(t *testing.T) {
	l := testLibrary(t)
	if _, err := l.Upload("x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Dir, stateFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := l.Handler()
	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		return w
	}
	w := get("/logos/library/countries/united-states/hgtv-us.png")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), png) {
		t.Errorf("png = %d %v", w.Code, w.Header())
	}
	w = get("/logos/uploads/x.svg")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("svg = %d %v", w.Code, w.Header())
	}
	for _, p := range []string{
		"/logos/library/countries/",                 // no listings
		"/logos/library/README.md",                  // only images
		"/logos/library/.hidden/secret.png",         // nothing hidden
		"/logos/.library.json",                      // state file
		"/logos/library/../../../../etc/passwd.png", // no way out
		"/logos/library/countries/nope.png",         // missing
		"/logos/other/x.png",                        // only library/ and uploads/
		"/logos/uploads/%2e%2e/library/x.png",
	} {
		if w := get(p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/logos/uploads/x.svg", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", w.Code)
	}
}

// tarball builds a .tar.gz like GitHub's: everything under one top folder.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "tv-logos-main/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: "tv-logos-main/countries/link.png", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestUpdate(t *testing.T) {
	archive := tarball(t, map[string]string{
		"tv-logos-main/countries/united-states/hgtv-us.png": string(png),
		"tv-logos-main/countries/germany/zdf-de.png":        string(png),
		"tv-logos-main/README.md":                           "readme",
		"tv-logos-main/.github/x.png":                       string(png),
		"tv-logos-main/../escape.png":                       string(png),
	})
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "nope", http.StatusBadGateway)
			return
		}
		w.Write(archive)
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeFiles(t, filepath.Join(dir, libraryDir), "countries/old/gone-xx.png")
	writeFiles(t, dir, ".library-new-123/leftover.png") // from a cut-short update
	l, err := Open(dir, srv.URL, "https://tv.example", srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".library-new-123")); !os.IsNotExist(err) {
		t.Error("leftover update folder kept")
	}
	if err := l.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lib, _ := l.Counts(); lib != 2 {
		t.Errorf("library after update = %d files", lib)
	}
	if _, ok := l.Lookup("library/countries/old/gone-xx.png"); ok {
		t.Error("old library kept")
	}
	for _, p := range []string{"escape.png", "library/README.md", "library/.github", "library/countries/link.png"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s extracted", p)
		}
	}
	st := l.State()
	if st.Files != 2 || st.Updated.IsZero() || st.Running || st.Error != "" {
		t.Errorf("state = %+v", st)
	}
	// The state survives a restart.
	l2, err := Open(dir, srv.URL, "https://tv.example", srv.Client(), nil)
	if err != nil || l2.State().Files != 2 {
		t.Errorf("reopened state = %+v, %v", l2.State(), err)
	}

	// A failed download keeps the library there is.
	fail = true
	if err := l.Update(context.Background()); err == nil {
		t.Error("failed download: no error")
	}
	if lib, _ := l.Counts(); lib != 2 || l.State().Error == "" {
		t.Errorf("after failure: %d files, state %+v", lib, l.State())
	}
}
