//go:build integration

package web

import (
	"bytes"
	"context"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/backup"
	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/testpg"
)

func TestBackupsPage(t *testing.T) {
	dir := t.TempDir()
	var restarts atomic.Int32
	var svc *backup.Service
	b, q := pgUI(t, func(d *Deps, pool *pgxpool.Pool) {
		latest, _ := db.LatestMigration()
		store := &backup.Store{Dir: dir, Pool: pool, Version: "v-test"}
		svc = &backup.Service{Store: store, Scheduler: &backup.Scheduler{Store: store, Log: slog.New(slog.DiscardHandler)},
			Migrator: db.Migrator{DSN: testpg.DSN("mmp_web_test")}, Latest: latest}
		d.Backups = svc
		d.Restart = func() { restarts.Add(1) }
	})
	ctx := context.Background()
	if _, err := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Keep me"}); err != nil {
		t.Fatal(err)
	}

	// Empty page with the schedule off by default.
	_, body := b.get("/admin/backups")
	if !strings.Contains(body, "No backups yet") || !strings.Contains(body, "Automatic backups are off") ||
		strings.Contains(body, `name="enabled" checked`) || !strings.Contains(body, `aria-current="page">Backups`) {
		t.Fatalf("empty page:\n%s", body)
	}

	// Back up now.
	if resp, _ := b.post("/admin/backups/create", url.Values{}, b.base); resp.Header.Get("Location") != "/admin/backups?msg=created" {
		t.Fatalf("create = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body = b.get("/admin/backups?msg=created")
	name := regexp.MustCompile(`mmp-manual-\d{8}-\d{6}\.tar\.gz`).FindString(body)
	if name == "" || !strings.Contains(body, "Backup created.") || !strings.Contains(body, "manual") {
		t.Fatalf("after create:\n%s", body)
	}

	// Download.
	resp, data := b.get("/admin/backups/" + name + "/download")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), name) {
		t.Fatalf("download = %d %v", resp.StatusCode, resp.Header)
	}
	if m, err := backup.ReadManifest(strings.NewReader(data)); err != nil || m.Version != "v-test" {
		t.Errorf("downloaded manifest = %+v %v", m, err)
	}
	for _, bad := range []string{"..%2F..%2Fetc%2Fpasswd", "mmp-manual-1.tar.gz", "notes.txt"} {
		if resp, _ := b.get("/admin/backups/" + bad + "/download"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("download %q = %d", bad, resp.StatusCode)
		}
	}

	// Schedule: validation, then on.
	if resp, body := b.post("/admin/backups/schedule", url.Values{"enabled": {"on"}, "every": {"daily"}, "at": {"27:00"}, "keep": {"7"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "HH:MM") {
		t.Errorf("bad schedule = %d", resp.StatusCode)
	}
	b.post("/admin/backups/schedule", url.Values{"enabled": {"on"}, "every": {"12h"}, "at": {"02:30"}, "keep": {"3"}}, b.base)
	_, body = b.get("/admin/backups")
	if !strings.Contains(body, `name="enabled" checked`) || !strings.Contains(body, `value="12h" selected`) || !strings.Contains(body, "Next backup:") {
		t.Errorf("schedule not saved:\n%s", body)
	}

	// Restore needs the typed confirmation.
	if resp, body := b.post("/admin/backups/"+name+"/restore", url.Values{"confirm": {"yes"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Type RESTORE") {
		t.Errorf("unconfirmed restore = %d", resp.StatusCode)
	}
	if _, err := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Added after the backup"}); err != nil {
		t.Fatal(err)
	}
	resp, body = b.post("/admin/backups/"+name+"/restore", url.Values{"confirm": {"RESTORE"}}, b.base)
	if resp.StatusCode != 200 || !strings.Contains(body, "Backup restored") || !strings.Contains(body, "mmp-pre-restore-") {
		t.Fatalf("restore = %d\n%s", resp.StatusCode, body)
	}
	bqs, _ := q.ListBouquets(ctx)
	if len(bqs) != 1 || bqs[0].Name != "Keep me" {
		t.Errorf("bouquets after restore = %+v", bqs)
	}
	// The schedule belongs to the machine: a restore keeps the current one.
	if s, _ := backup.LoadSchedule(ctx, svc.Pool); !s.Enabled || s.Every != "12h" || s.Keep != 3 {
		t.Errorf("schedule after restore = %+v, want the one set before", s)
	}

	// Signed out by the restore (sessions are not in backups).
	if resp, _ := b.get("/admin/backups"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("still signed in after restore: %d", resp.StatusCode)
	}
	if resp, _ := b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login after restore = %d", resp.StatusCode)
	}

	// Upload: a non-backup is refused, a real one is listed.
	upload := func(filename string, content []byte) (*http.Response, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", filename)
		fw.Write(content)
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, b.base+"/admin/backups/upload", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", b.base)
		return b.do(req)
	}
	if resp, body := upload("x.tar.gz", []byte("not a backup")); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Upload refused") {
		t.Errorf("garbage upload = %d", resp.StatusCode)
	}
	if resp, _ := upload("mine.tar.gz", []byte(data)); resp.Header.Get("Location") != "/admin/backups?msg=uploaded" {
		t.Errorf("upload = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body = b.get("/admin/backups")
	up := regexp.MustCompile(`mmp-upload-\d{8}-\d{6}(-\d+)?\.tar\.gz`).FindString(body)
	if up == "" || !strings.Contains(body, "uploaded") {
		t.Errorf("uploaded backup not listed:\n%s", body)
	}

	// Delete.
	b.post("/admin/backups/"+up+"/delete", url.Values{}, b.base)
	if _, body = b.get("/admin/backups"); strings.Contains(body, up) {
		t.Error("deleted backup still listed")
	}
	// Restart is requested once per restore (asynchronously).
	for i := 0; i < 50 && restarts.Load() == 0; i++ {
		b.get("/admin/backups")
	}
}
