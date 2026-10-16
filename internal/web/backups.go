package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/backup"
)

// maxUpload bounds an uploaded backup.
const maxUpload = 4 << 30

func (ui *UI) mountBackups(r chi.Router) {
	r.Get("/backups", ui.backupsPage)
	r.Post("/backups/create", ui.backupCreate)
	r.Post("/backups/schedule", ui.backupSchedule)
	r.Post("/backups/upload", ui.backupUpload)
	r.Get("/backups/{name}/download", ui.backupDownload)
	r.Post("/backups/{name}/restore", ui.backupRestore)
	r.Post("/backups/{name}/delete", ui.backupDelete)
}

type backupsData struct {
	Dir      string
	Backups  []backup.Info
	Schedule backup.Schedule
	Every    []string
	Next     time.Time
	Status   backup.Status
	Flash    string
	Error    string
	Zone     string
}

var backupFlashes = map[string]string{
	"created":  "Backup created.",
	"saved":    "Automatic backup settings saved.",
	"uploaded": "Backup uploaded. Restore it from the list when you are ready.",
	"deleted":  "Backup deleted.",
}

func (ui *UI) backupsData(r *http.Request) (backupsData, error) {
	d := backupsData{Dir: ui.Backups.Dir, Every: backup.Frequencies, Status: ui.Backups.Scheduler.Status()}
	d.Zone, _ = time.Now().Zone()
	var err error
	if d.Backups, err = ui.Backups.List(); err != nil {
		return d, err
	}
	if d.Schedule, err = backup.LoadSchedule(r.Context(), ui.Backups.Pool); err != nil {
		return d, err
	}
	d.Next = d.Schedule.Next(time.Now())
	return d, nil
}

func (ui *UI) backupsPage(w http.ResponseWriter, r *http.Request) {
	if ui.Backups == nil {
		http.NotFound(w, r)
		return
	}
	d, err := ui.backupsData(r)
	if err != nil {
		ui.fail(w, "backups", err)
		return
	}
	d.Flash = backupFlashes[r.URL.Query().Get("msg")]
	ui.render(w, r, http.StatusOK, "backups", "Backups", d)
}

// backupError re-renders the page with a message.
func (ui *UI) backupError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	d, err := ui.backupsData(r)
	if err != nil {
		ui.fail(w, "backups", err)
		return
	}
	d.Error = msg
	ui.render(w, r, status, "backups", "Backups", d)
}

func (ui *UI) backupCreate(w http.ResponseWriter, r *http.Request) {
	if ui.Backups == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := contextFor(r, 30*time.Minute)
	defer cancel()
	info, err := ui.Backups.Create(ctx, backup.KindManual)
	if err != nil {
		ui.Log.Error("web: backup", "err", err)
		ui.backupError(w, r, http.StatusInternalServerError, "Backup failed: "+err.Error())
		return
	}
	ui.Log.Info("web: backup created", "file", info.Name, "bytes", info.Size)
	ui.redirect(w, r, Prefix+"/backups?msg=created")
}

func (ui *UI) backupSchedule(w http.ResponseWriter, r *http.Request) {
	if ui.Backups == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cur, err := backup.LoadSchedule(r.Context(), ui.Backups.Pool)
	if err != nil {
		ui.fail(w, "backup schedule", err)
		return
	}
	keep, _ := strconv.Atoi(strings.TrimSpace(r.PostForm.Get("keep")))
	s := backup.Schedule{Enabled: r.PostForm.Get("enabled") != "", Every: r.PostForm.Get("every"),
		At: strings.TrimSpace(r.PostForm.Get("at")), Keep: keep, EnabledAt: cur.EnabledAt}
	if err := s.Validate(); err != nil {
		ui.backupError(w, r, http.StatusUnprocessableEntity, "Automatic backups: "+err.Error()+".")
		return
	}
	if s.Enabled && !cur.Enabled {
		s.EnabledAt = time.Now()
	}
	if err := backup.SaveSchedule(r.Context(), ui.Backups.Pool, s); err != nil {
		ui.fail(w, "backup schedule", err)
		return
	}
	ui.Log.Info("web: backup schedule saved", "enabled", s.Enabled, "every", s.Every, "at", s.At, "keep", s.Keep)
	ui.redirect(w, r, Prefix+"/backups?msg=saved")
}

func (ui *UI) backupUpload(w http.ResponseWriter, r *http.Request) {
	if ui.Backups == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		ui.backupError(w, r, http.StatusBadRequest, "Choose a backup file to upload.")
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			ui.backupError(w, r, http.StatusBadRequest, "Choose a backup file to upload.")
			return
		}
		if part.FormName() != "file" {
			continue
		}
		info, err := ui.Backups.Import(part)
		if err != nil {
			ui.backupError(w, r, http.StatusUnprocessableEntity, "Upload refused: "+err.Error()+".")
			return
		}
		ui.Log.Info("web: backup uploaded", "file", info.Name, "bytes", info.Size)
		ui.redirect(w, r, Prefix+"/backups?msg=uploaded")
		return
	}
}

func (ui *UI) backupDownload(w http.ResponseWriter, r *http.Request) {
	if ui.Backups == nil {
		http.NotFound(w, r)
		return
	}
	name := chi.URLParam(r, "name")
	path, err := ui.Backups.Path(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeFile(w, r, path)
}

func (ui *UI) backupDelete(w http.ResponseWriter, r *http.Request) {
	if ui.Backups == nil {
		http.NotFound(w, r)
		return
	}
	if err := ui.Backups.Delete(chi.URLParam(r, "name")); errors.Is(err, backup.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		ui.fail(w, "delete backup", err)
		return
	}
	ui.redirect(w, r, Prefix+"/backups?msg=deleted")
}

type restoredData struct {
	Name, Safety string
	Manifest     backup.Manifest
}

// backupRestore replaces all data with a backup. The admin must type
// RESTORE; a safety backup is taken first, and MMP restarts afterwards so
// nothing stale stays in memory.
func (ui *UI) backupRestore(w http.ResponseWriter, r *http.Request) {
	if ui.Backups == nil {
		http.NotFound(w, r)
		return
	}
	name := chi.URLParam(r, "name")
	if _, err := ui.Backups.Path(name); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(r.PostForm.Get("confirm")) != "RESTORE" {
		ui.backupError(w, r, http.StatusUnprocessableEntity, "Type RESTORE to confirm restoring "+name+".")
		return
	}
	ctx, cancel := contextFor(r, 60*time.Minute)
	defer cancel()
	m, safety, err := ui.Backups.Restore(ctx, name)
	if err != nil {
		ui.Log.Error("web: restore failed", "file", name, "safety", safety, "err", err)
		msg := "Restore failed: " + err.Error() + "."
		if safety != "" {
			msg += " Your data from before the attempt is in " + safety + "."
		}
		ui.backupError(w, r, http.StatusInternalServerError, msg)
		return
	}
	ui.Log.Info("web: restored backup", "file", name, "rows", m.Rows(), "safety", safety)
	ui.render(w, r, http.StatusOK, "backup_restored", "Restored", restoredData{Name: name, Safety: safety, Manifest: m})
	if ui.Restart != nil {
		go func() {
			time.Sleep(time.Second) // let the page reach the browser
			ui.Restart()
		}()
	}
}
