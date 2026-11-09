//go:build integration

package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/logos"
)

var testPNG = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))

func TestLogosPages(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"countries/united-states/hgtv-us.png", "countries/united-states/tnt-us.png", "countries/germany/zdf-de.png"} {
		p := filepath.Join(dir, "library", filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, testPNG, 0o644)
	}
	lib, err := logos.Open(dir, "", "http://mmp.example:8080", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, q := pgUI(t, func(d *Deps, _ *pgxpool.Pool) { d.Logos = lib })
	ctx := context.Background()
	mk := func(name, logo string) db.Channel {
		c, err := q.CreateChannel(ctx, db.CreateChannelParams{Name: name, LogoUrl: logo})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	hgtv := mk("USA HGTV East", "http://provider.example/hgtv.jpg")
	tnt := mk("USA TNT West", "")
	odd := mk("Something Unknown", "")
	zdf := mk("ZDF", lib.URL("library/countries/germany/zdf-de.png")) // already from the library

	// The Logos page and its search.
	_, body := b.get("/admin/logos")
	if !strings.Contains(body, "<strong>3</strong> logos") || !strings.Contains(body, `aria-current="page">Logos`) || !strings.Contains(body, "No uploaded logos yet") {
		t.Fatalf("logos page:\n%s", body)
	}
	if _, body = b.get("/admin/logos?q=hgtv"); !strings.Contains(body, "/logos/library/countries/united-states/hgtv-us.png") || strings.Contains(body, "tnt-us.png") {
		t.Errorf("search:\n%s", body)
	}

	// Upload from the Logos page.
	upload := func(path, name string, content []byte) (*http.Response, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "file.png")
		fw.Write(content)
		mw.WriteField("name", name)
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, b.base+path, &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", b.base)
		return b.do(req)
	}
	if resp, _ := upload("/admin/logos/upload", "My Station", testPNG); resp.Header.Get("Location") != "/admin/logos?msg=uploaded" {
		t.Fatalf("upload = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, body = b.get("/admin/logos?msg=uploaded"); !strings.Contains(body, "Logo uploaded.") || !strings.Contains(body, "my-station.png") {
		t.Errorf("after upload:\n%s", body)
	}
	if resp, body := upload("/admin/logos/upload", "x", []byte("not an image")); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "not a PNG") {
		t.Errorf("bad upload = %d", resp.StatusCode)
	}
	// From the channel form's picker: the grid with the new logo, to pick.
	resp, body := upload("/admin/logos/upload?picker=1", "USA TNT West", testPNG)
	if resp.StatusCode != 200 || !strings.Contains(body, `data-logo-pick="http://mmp.example:8080/logos/uploads/usa-tnt-west.png"`) {
		t.Errorf("picker upload = %d\n%s", resp.StatusCode, body)
	}
	if err := lib.DeleteUpload("usa-tnt-west.png"); err != nil {
		t.Fatal(err)
	}

	// The picker: suggestions for the name, or a search.
	if _, body = b.get("/admin/logos/pick?name=" + url.QueryEscape("USA HGTV East")); !strings.Contains(body, `data-logo-pick="http://mmp.example:8080/logos/library/countries/united-states/hgtv-us.png"`) {
		t.Errorf("pick by name:\n%s", body)
	}
	if _, body = b.get("/admin/logos/pick?q=zdf&name=HGTV"); !strings.Contains(body, "zdf-de.png") || strings.Contains(body, "hgtv-us.png") {
		t.Errorf("pick by search:\n%s", body)
	}
	if _, body = b.get("/admin/channels/" + strconv.FormatInt(hgtv.ID, 10)); !strings.Contains(body, `id="logo-hits"`) || !strings.Contains(body, `data-logo-preview src="http://provider.example/hgtv.jpg"`) {
		t.Errorf("channel form has no picker:\n%s", body)
	}

	// Bulk suggestions: the confident ones selected, library logos left out.
	_, body = b.get("/admin/logos/suggest")
	hgtvPick := `name="logo_` + strconv.FormatInt(hgtv.ID, 10) + `" value="library/countries/united-states/hgtv-us.png" checked`
	if !strings.Contains(body, hgtvPick) || !strings.Contains(body, "3 of 3 channels shown (1 with a library logo hidden)") || strings.Contains(body, ">ZDF<") ||
		!strings.Contains(body, `name="logo_`+strconv.FormatInt(odd.ID, 10)+`" value="" checked`) {
		t.Fatalf("suggest page:\n%s", body)
	}
	if _, body = b.get("/admin/logos/suggest?all=1"); !strings.Contains(body, ">ZDF<") {
		t.Errorf("suggest all lacks ZDF")
	}
	if _, body = b.get("/admin/logos/suggest?have=all"); !strings.Contains(body, ">ZDF<") || !strings.Contains(body, "4 of 4 channels shown") {
		t.Errorf("suggest have=all lacks ZDF")
	}
	// Filters: name words, current logo, best fit.
	for query, want := range map[string][]string{
		"q=usa+west":        {">USA TNT West<"},
		"have=none":         {">USA TNT West<", ">Something Unknown<"},
		"fit=none":          {">Something Unknown<"},
		"fit=sure&have=all": {">USA HGTV East<", ">USA TNT West<", ">ZDF<"},
		"q=nothing":         {"No channel matches this filter."},
	} {
		_, body := b.get("/admin/logos/suggest?" + query)
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("suggest?%s lacks %s", query, w)
			}
		}
		for _, name := range []string{">USA HGTV East<", ">USA TNT West<", ">Something Unknown<", ">ZDF<"} {
			if !slices.Contains(want, name) && strings.Contains(body, name) {
				t.Errorf("suggest?%s shows %s", query, name)
			}
		}
	}
	// Applying keeps the filter.
	if resp, _ := b.post("/admin/logos/apply", url.Values{"q": {"usa"}, "fit": {"sure"}}, b.base); resp.Header.Get("Location") != "/admin/logos/suggest?applied=0&fit=sure&q=usa" {
		t.Errorf("filtered apply = %s", resp.Header.Get("Location"))
	}
	form := url.Values{
		"logo_" + strconv.FormatInt(hgtv.ID, 10): {"library/countries/united-states/hgtv-us.png"},
		"logo_" + strconv.FormatInt(tnt.ID, 10):  {"library/../../etc/passwd"}, // not in the library: ignored
		"logo_" + strconv.FormatInt(odd.ID, 10):  {""},
	}
	if resp, _ := b.post("/admin/logos/apply", form, b.base); resp.Header.Get("Location") != "/admin/logos/suggest?applied=1" {
		t.Fatalf("apply = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for id, want := range map[int64]string{
		hgtv.ID: "http://mmp.example:8080/logos/library/countries/united-states/hgtv-us.png",
		tnt.ID:  "", odd.ID: "", zdf.ID: zdf.LogoUrl,
	} {
		if c, _ := q.GetChannel(ctx, id); c.LogoUrl != want {
			t.Errorf("channel %d logo = %q, want %q", id, c.LogoUrl, want)
		}
	}

	// Update: started in the background (it fails here: no archive server).
	if resp, _ := b.post("/admin/logos/update", url.Values{}, b.base); !strings.HasPrefix(resp.Header.Get("Location"), "/admin/logos?msg=") {
		t.Errorf("update = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestGroupIconPicker: with a logo library the Groups page offers the
// library picker for each group's icon; without one it does not.
func TestGroupIconPicker(t *testing.T) {
	lib, err := logos.Open(t.TempDir(), "", "http://mmp.example:8080", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, q := pgUI(t, func(d *Deps, _ *pgxpool.Pool) { d.Logos = lib })
	g, _ := q.CreateChannelGroup(context.Background(), db.CreateChannelGroupParams{Name: "Sports"})
	_, body := b.get("/admin/groups")
	for _, want := range []string{`data-icon-picker="group-` + strconv.FormatInt(g.ID, 10) + `"`, `data-group-name="Sports"`, `id="icon-picker"`} {
		if !strings.Contains(body, want) {
			t.Errorf("groups page lacks %q", want)
		}
	}

	b, q = pgUI(t)
	q.CreateChannelGroup(context.Background(), db.CreateChannelGroupParams{Name: "Sports"})
	if _, body := b.get("/admin/groups"); strings.Contains(body, "data-icon-picker") {
		t.Error("picker shown without a logo library")
	}
}
