package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/proxy"
)

func (ui *UI) mountProfiles(r chi.Router) {
	r.Get("/profiles", ui.profilesPage)
	r.Get("/profiles/new", ui.profileNew)
	r.Post("/profiles/new", ui.profileCreate)
	r.Get("/profiles/{id}", ui.profileEdit)
	r.Post("/profiles/{id}", ui.profileUpdate)
	r.Post("/profiles/{id}/delete", ui.profileDelete)
}

type profileRow struct {
	db.StreamProfile
	Channels int64
}

var profileFlashes = map[string]string{
	"created": "Profile created. Choose it on a channel's settings to use it.",
	"saved":   "Profile saved. It applies to channel sessions started from now on.",
	"deleted": "Profile deleted; its channels are back to passthrough.",
}

func (ui *UI) profilesPage(w http.ResponseWriter, r *http.Request) {
	ps, err := ui.Store.ListStreamProfiles(r.Context())
	if err != nil {
		ui.fail(w, "list profiles", err)
		return
	}
	counts, err := ui.Store.ProfileChannelCounts(r.Context())
	if err != nil {
		ui.fail(w, "profile counts", err)
		return
	}
	n := map[int64]int64{}
	for _, c := range counts {
		if c.ProfileID != nil {
			n[*c.ProfileID] = c.Channels
		}
	}
	rows := make([]profileRow, 0, len(ps))
	for _, p := range ps {
		rows = append(rows, profileRow{StreamProfile: p, Channels: n[p.ID]})
	}
	ui.render(w, r, http.StatusOK, "profiles", "Stream profiles", struct {
		Rows  []profileRow
		Flash string
	}{rows, profileFlashes[r.URL.Query().Get("msg")]})
}

type profileForm struct {
	ID                                int64
	Name, Mode, FFmpegArgs, UserAgent string
	Error                             string
}

func parseProfileForm(w http.ResponseWriter, r *http.Request) (profileForm, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return profileForm{}, formError("Could not read the form.")
	}
	f := profileForm{Name: strings.TrimSpace(r.PostForm.Get("name")), Mode: r.PostForm.Get("mode"),
		FFmpegArgs: strings.TrimSpace(r.PostForm.Get("ffmpeg_args")), UserAgent: strings.TrimSpace(r.PostForm.Get("user_agent"))}
	if f.Name == "" || len(f.Name) > 100 {
		return f, formError("Name is required (up to 100 characters).")
	}
	if err := proxy.ValidateProfile(f.Mode, f.FFmpegArgs); err != nil {
		// The validator speaks API field names; "ffmpeg" stays lower-case.
		msg := strings.ReplaceAll(err.Error(), "ffmpeg_args", "ffmpeg arguments")
		if !strings.HasPrefix(msg, "ffmpeg") {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		return f, formError(msg + ".")
	}
	return f, nil
}

func profileSaveErr(err error) (string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return "A profile with that name exists.", true
	}
	return "", false
}

func (ui *UI) profileNew(w http.ResponseWriter, r *http.Request) {
	ui.render(w, r, http.StatusOK, "profile_form", "New profile", profileForm{Mode: "ffmpeg", FFmpegArgs: "-c copy"})
}

func (ui *UI) profileCreate(w http.ResponseWriter, r *http.Request) {
	f, err := parseProfileForm(w, r)
	if err == nil {
		if _, err = ui.Store.CreateStreamProfile(r.Context(), db.CreateStreamProfileParams{Name: f.Name, Mode: f.Mode,
			FfmpegArgs: f.FFmpegArgs, UserAgent: f.UserAgent}); err == nil {
			ui.redirect(w, r, Prefix+"/profiles?msg=created")
			return
		}
		msg, ok := profileSaveErr(err)
		if !ok {
			ui.fail(w, "create profile", err)
			return
		}
		err = formError(msg)
	}
	f.Error = err.Error()
	ui.render(w, r, http.StatusUnprocessableEntity, "profile_form", "New profile", f)
}

func (ui *UI) profile(w http.ResponseWriter, r *http.Request) (db.StreamProfile, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return db.StreamProfile{}, false
	}
	p, err := ui.Store.GetStreamProfile(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return db.StreamProfile{}, false
	}
	if err != nil {
		ui.fail(w, "get profile", err)
		return db.StreamProfile{}, false
	}
	return p, true
}

func (ui *UI) profileEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.profile(w, r)
	if !ok {
		return
	}
	ui.render(w, r, http.StatusOK, "profile_form", "Edit "+p.Name,
		profileForm{ID: p.ID, Name: p.Name, Mode: p.Mode, FFmpegArgs: p.FfmpegArgs, UserAgent: p.UserAgent})
}

func (ui *UI) profileUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.profile(w, r)
	if !ok {
		return
	}
	f, err := parseProfileForm(w, r)
	f.ID = p.ID
	if err == nil {
		if _, err = ui.Store.UpdateStreamProfile(r.Context(), db.UpdateStreamProfileParams{ID: p.ID, Name: f.Name, Mode: f.Mode,
			FfmpegArgs: f.FFmpegArgs, UserAgent: f.UserAgent}); err == nil {
			ui.redirect(w, r, Prefix+"/profiles?msg=saved")
			return
		}
		msg, ok := profileSaveErr(err)
		if !ok {
			ui.fail(w, "update profile", err)
			return
		}
		err = formError(msg)
	}
	f.Error = err.Error()
	ui.render(w, r, http.StatusUnprocessableEntity, "profile_form", "Edit "+p.Name, f)
}

func (ui *UI) profileDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.profile(w, r)
	if !ok {
		return
	}
	if _, err := ui.Store.DeleteStreamProfile(r.Context(), p.ID); err != nil {
		ui.fail(w, "delete profile", err)
		return
	}
	ui.redirect(w, r, Prefix+"/profiles?msg=deleted")
}
