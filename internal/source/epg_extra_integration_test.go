//go:build integration

package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestExtraGuideSource: a third-party .xml.gz guide downloads, and priority
// (lower wins, the extra source's own vs its provider's) picks the schedule.
func TestExtraGuideSource(t *testing.T) {
	w, q := newTestDB(t)
	ctx := context.Background()
	start := time.Now().Add(-30 * time.Minute).UTC()
	xmltv := func(title string) string {
		return `<?xml version="1.0"?><tv><programme start="` + start.Format("20060102150405 -0700") + `" stop="` +
			start.Add(2*time.Hour).Format("20060102150405 -0700") + `" channel="AandE.us"><title>` + title + `</title></programme></tv>`
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, xmltv("From the community guide"))
	zw.Close()
	extraSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/octet-stream") // no Content-Encoding: a plain .gz file
		rw.Write(gz.Bytes())
	}))
	t.Cleanup(extraSrv.Close)
	provSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		io.WriteString(rw, xmltv("From the provider"))
	}))
	t.Cleanup(provSrv.Close)

	ref := NewRefresher(slog.New(slog.DiscardHandler), w.pool, 80)
	p, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "P", Type: "m3u", Url: provSrv.URL + "/list.m3u", EpgUrl: provSrv.URL,
		MaxConnections: 1, RefreshIntervalSecs: 3600, Priority: 1, Enabled: true})
	if _, err := ref.RefreshEPG(ctx, p); err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	src, err := q.CreateExtraEpgSource(ctx, db.CreateExtraEpgSourceParams{Name: "Community", Url: extraSrv.URL + "/guide.xml.gz", Priority: &zero, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// Due before its first download, not after.
	if due, _ := q.ListExtraEpgSourcesDue(ctx, 3600); len(due) != 1 {
		t.Fatalf("due = %v", due)
	}
	if n, err := ref.RefreshEPGSource(ctx, src); err != nil || n != 1 {
		t.Fatalf("refresh extra = %d %v", n, err)
	}
	if due, _ := q.ListExtraEpgSourcesDue(ctx, 3600); len(due) != 0 {
		t.Errorf("still due after download: %v", due)
	}
	title := func() string {
		rows, err := q.ListGuidePrograms(ctx, db.ListGuideProgramsParams{TvgKeys: []string{"aande.us"}, Since: time.Now(), Until: time.Now().Add(time.Hour)})
		if err != nil || len(rows) == 0 {
			t.Fatalf("guide rows = %v %v", rows, err)
		}
		return rows[0].Title
	}
	if got := title(); got != "From the community guide" {
		t.Errorf("priority 0 vs provider 1: got %q", got)
	}
	five := int32(5)
	if _, err := q.UpdateExtraEpgSource(ctx, db.UpdateExtraEpgSourceParams{ID: src.ID, Name: src.Name, Url: src.Url, Priority: &five, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "From the provider" {
		t.Errorf("priority 5 vs provider 1: got %q", got)
	}
	// Provider sources cannot be refreshed, edited or deleted as extras.
	provSrc, _ := q.GetEpgSourceByProvider(ctx, &p.ID)
	if _, err := ref.RefreshEPGSource(ctx, provSrc); err == nil {
		t.Error("provider source refreshed as an extra")
	}
	if n, _ := q.DeleteExtraEpgSource(ctx, provSrc.ID); n != 0 {
		t.Error("provider source deleted as an extra")
	}
}
