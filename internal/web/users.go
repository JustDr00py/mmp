package web

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

func (ui *UI) mountUsers(r chi.Router) {
	r.Get("/users", ui.usersPage)
	r.Get("/users/new", ui.userNew)
	r.Post("/users/new", ui.userCreate)
	r.Route("/users/{id}", func(r chi.Router) {
		r.Get("/", ui.userPage)
		r.Post("/", ui.userUpdate)
		r.Post("/delete", ui.userDelete)
		r.Post("/password", ui.userResetPassword)
		r.Post("/token", ui.userRotateToken)
	})
}

// userRow is one line of the users table.
type userRow struct {
	db.User
	Active   int
	Status   string // active | disabled | expired
	LastSeen *time.Time
	LastIP   string
}

func userStatus(u db.User, now time.Time) string {
	switch {
	case !u.Enabled:
		return "disabled"
	case u.ExpiresAt != nil && !u.ExpiresAt.After(now):
		return "expired"
	}
	return "active"
}

type usersData struct {
	Rows  []userRow
	Query string
	Flash string
}

var userFlashes = map[string]string{
	"saved":   "User saved.",
	"deleted": "User deleted.",
	"token":   "Stream token rotated: the old playlist and guide links stopped working.",
}

func (ui *UI) usersPage(w http.ResponseWriter, r *http.Request) {
	users, err := ui.Store.ListUsers(r.Context())
	if err != nil {
		ui.fail(w, "list users", err)
		return
	}
	var active map[int64]int
	if ui.Connections != nil {
		active = ui.Connections()
	}
	seen, err := ui.Store.UsersLastSeen(r.Context())
	if err != nil {
		ui.fail(w, "users last seen", err)
		return
	}
	lastSeen := make(map[int64]db.UsersLastSeenRow, len(seen))
	for _, s := range seen {
		lastSeen[s.UserID] = s
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	d := usersData{Query: q, Flash: userFlashes[r.URL.Query().Get("msg")]}
	now := time.Now()
	for _, u := range users {
		if q != "" && !strings.Contains(strings.ToLower(u.Username), q) {
			continue
		}
		row := userRow{User: u, Active: active[u.ID], Status: userStatus(u, now)}
		if s, ok := lastSeen[u.ID]; ok {
			row.LastSeen, row.LastIP = &s.LastSeen, s.LastIp
		}
		d.Rows = append(d.Rows, row)
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "users" {
		ui.fragment(w, r, "users", "users-table", d)
		return
	}
	ui.render(w, r, http.StatusOK, "users", "Users", d)
}

// userForm is the create/edit form's state. Passwords are never rendered.
type userForm struct {
	ID             int64
	Username       string
	Role           string
	MaxConnections string
	Expires        string // YYYY-MM-DD, empty = never
	Enabled        bool
	BouquetIDs     map[int64]bool
	Bouquets       []db.Bouquet
	Error          string
}

// credentials is what a customer's player needs. Password is set only
// right after it was chosen (create / reset): MMP stores just its hash.
type credentials struct {
	Server, Username, Password string
	TokenPlaylist, TokenGuide  string
	TokenGuideGz               string
	M3U, XMLTV                 string // with the password; only when Password is set
	HLS                        bool   // whether .m3u8 output is offered
}

func (ui *UI) credentialsFor(u db.User, password string) credentials {
	base := ui.BaseURL
	c := credentials{Server: base, Username: u.Username, Password: password,
		TokenPlaylist: base + "/playlist/" + url.PathEscape(u.StreamToken) + ".m3u",
		TokenGuide:    base + "/epg/" + url.PathEscape(u.StreamToken) + ".xml",
		TokenGuideGz:  base + "/epg/" + url.PathEscape(u.StreamToken) + ".xml.gz"}
	if password != "" {
		q := url.Values{"username": {u.Username}, "password": {password}}
		c.XMLTV = base + "/xmltv.php?" + q.Encode()
		q.Set("type", "m3u_plus")
		q.Set("output", "ts")
		c.M3U = base + "/get.php?" + q.Encode()
	}
	return c
}

func (ui *UI) withBouquets(r *http.Request, f *userForm) error {
	var err error
	f.Bouquets, err = ui.Store.ListBouquets(r.Context())
	return err
}

// parsedUser is a validated form; Password is "" when none was typed.
type parsedUser struct {
	username, password, role string
	maxConns                 int32
	expires                  *time.Time
	enabled                  bool
	bouquets                 []int64
}

// expiryLayout is the date input's format. A date means "through the end
// of that day" in UTC.
const expiryLayout = "2006-01-02"

func parseUserForm(w http.ResponseWriter, r *http.Request) (userForm, parsedUser, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return userForm{}, parsedUser{}, formError("Could not read the form.")
	}
	f := r.PostForm
	form := userForm{Username: strings.TrimSpace(f.Get("username")), Role: f.Get("role"),
		MaxConnections: strings.TrimSpace(f.Get("max_connections")), Expires: strings.TrimSpace(f.Get("expires")),
		Enabled: f.Get("enabled") == "on", BouquetIDs: map[int64]bool{}}
	p := parsedUser{username: form.Username, password: f.Get("password"), role: form.Role, enabled: form.Enabled}
	for _, v := range f["bouquet"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && !form.BouquetIDs[id] {
			form.BouquetIDs[id] = true
			p.bouquets = append(p.bouquets, id)
		}
	}
	if err := auth.ValidateUsername(p.username); err != nil {
		return form, p, sentence(err)
	}
	if !auth.ValidRole(p.role) {
		return form, p, formError("Choose a role.")
	}
	if p.password != "" {
		if err := auth.ValidatePassword(p.password); err != nil {
			return form, p, sentence(err)
		}
	}
	n := 1
	if form.MaxConnections != "" {
		var err error
		if n, err = strconv.Atoi(form.MaxConnections); err != nil || n < 0 || n > 10000 {
			return form, p, formError("Max connections must be a whole number from 0 (unlimited) to 10000.")
		}
	}
	p.maxConns = int32(n)
	if form.Expires != "" {
		d, err := time.Parse(expiryLayout, form.Expires)
		if err != nil {
			return form, p, formError("Expiry must be a date.")
		}
		end := d.Add(24*time.Hour - time.Second)
		p.expires = &end
	}
	return form, p, nil
}

// sentence turns a validator error into a form message.
func sentence(err error) error {
	msg := err.Error()
	return formError(strings.ToUpper(msg[:1]) + msg[1:] + ".")
}

// newPassword returns 16 random characters from an alphabet without
// look-alikes, valid for ValidatePassword and safe in Xtream URLs.
func newPassword() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}

func (ui *UI) userNew(w http.ResponseWriter, r *http.Request) {
	f := userForm{Role: auth.RoleCustomer, MaxConnections: "1", Enabled: true, BouquetIDs: map[int64]bool{}}
	if err := ui.withBouquets(r, &f); err != nil {
		ui.fail(w, "list bouquets", err)
		return
	}
	ui.render(w, r, http.StatusOK, "user_new", "New user", f)
}

// inTx runs fn in a transaction when the UI has one, else directly.
func (ui *UI) inTx(ctx context.Context, fn func(Store) error) error {
	if ui.InTx == nil {
		return fn(ui.Store)
	}
	return ui.InTx(ctx, fn)
}

func setUserBouquets(ctx context.Context, st Store, userID int64, ids []int64) error {
	if err := st.ClearUserBouquets(ctx, userID); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return st.AddUserBouquets(ctx, db.AddUserBouquetsParams{UserID: userID, BouquetIds: ids})
}

// userSaveErr maps constraint violations to form messages.
func userSaveErr(err error) (string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505":
			return "That username is taken.", true
		case "23503":
			return "A selected bouquet no longer exists; reload the page.", true
		}
	}
	return "", false
}

type userPageData struct {
	Form        userForm
	Creds       credentials
	ShowSecret  bool // Creds.Password is set: this is the only time it is shown
	Active      int
	Flash       string
	ConfirmSelf bool // the admin is looking at their own account
	Activity    userActivity
}

func (ui *UI) userCreate(w http.ResponseWriter, r *http.Request) {
	form, p, err := parseUserForm(w, r)
	if err == nil && p.password == "" {
		if p.password, err = newPassword(); err != nil {
			ui.fail(w, "generate password", err)
			return
		}
	}
	var u db.User
	if err == nil {
		var hash, token string
		if hash, err = auth.HashPassword(p.password); err != nil {
			ui.fail(w, "hash password", err)
			return
		}
		if token, err = auth.NewStreamToken(); err != nil {
			ui.fail(w, "stream token", err)
			return
		}
		err = ui.inTx(r.Context(), func(st Store) error {
			var err error
			if u, err = st.CreateUser(r.Context(), db.CreateUserParams{Username: p.username, PasswordHash: hash, Role: p.role,
				MaxConnections: p.maxConns, ExpiresAt: p.expires, Enabled: p.enabled, StreamToken: token}); err != nil {
				return err
			}
			return setUserBouquets(r.Context(), st, u.ID, p.bouquets)
		})
		if err != nil {
			msg, ok := userSaveErr(err)
			if !ok {
				ui.fail(w, "create user", err)
				return
			}
			err = formError(msg)
		}
	}
	if err != nil {
		form.Error = err.Error()
		if berr := ui.withBouquets(r, &form); berr != nil {
			ui.fail(w, "list bouquets", berr)
			return
		}
		ui.render(w, r, http.StatusUnprocessableEntity, "user_new", "New user", form)
		return
	}
	ui.Log.Info("web: user created", "user_id", u.ID, "role", u.Role)
	// Rendered straight from the POST (no redirect) so the password never
	// travels in a URL; the response is no-store.
	ui.renderUser(w, r, http.StatusCreated, u, p.password, "User created. Copy the password now: it is not shown again.", nil)
}

// user loads {id}, answering 404 itself when it does not exist.
func (ui *UI) user(w http.ResponseWriter, r *http.Request) (db.User, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return db.User{}, false
	}
	u, err := ui.Store.GetUser(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return db.User{}, false
	}
	if err != nil {
		ui.fail(w, "get user", err)
		return db.User{}, false
	}
	return u, true
}

func (ui *UI) formFromUser(r *http.Request, u db.User) (userForm, error) {
	f := userForm{ID: u.ID, Username: u.Username, Role: u.Role, MaxConnections: strconv.Itoa(int(u.MaxConnections)),
		Enabled: u.Enabled, BouquetIDs: map[int64]bool{}}
	if u.ExpiresAt != nil {
		f.Expires = u.ExpiresAt.UTC().Format(expiryLayout)
	}
	ids, err := ui.Store.ListUserBouquetIDs(r.Context(), u.ID)
	for _, id := range ids {
		f.BouquetIDs[id] = true
	}
	return f, err
}

// renderUser shows the user page; form, when non-nil, replaces the stored
// values (a rejected edit).
func (ui *UI) renderUser(w http.ResponseWriter, r *http.Request, status int, u db.User, password, flash string, form *userForm) {
	var f userForm
	if form != nil {
		f = *form
	} else {
		var err error
		if f, err = ui.formFromUser(r, u); err != nil {
			ui.fail(w, "user bouquets", err)
			return
		}
	}
	if err := ui.withBouquets(r, &f); err != nil {
		ui.fail(w, "list bouquets", err)
		return
	}
	d := userPageData{Form: f, Creds: ui.credentialsFor(u, password), ShowSecret: password != "", Flash: flash}
	if ui.Connections != nil {
		d.Active = ui.Connections()[u.ID]
	}
	if me, ok := auth.UserFrom(r.Context()); ok && me.ID == u.ID {
		d.ConfirmSelf = true
	}
	if d.Flash == "" {
		d.Flash = userFlashes[r.URL.Query().Get("msg")]
	}
	var err error
	if d.Activity, err = ui.userActivity(r.Context(), u.ID); err != nil {
		ui.fail(w, "user activity", err)
		return
	}
	ui.render(w, r, status, "user", u.Username, d)
}

func (ui *UI) userPage(w http.ResponseWriter, r *http.Request) {
	u, ok := ui.user(w, r)
	if !ok {
		return
	}
	ui.renderUser(w, r, http.StatusOK, u, "", "", nil)
}

// lastAdmin reports whether u is the only enabled admin.
func (ui *UI) lastAdmin(ctx context.Context, u db.User) (bool, error) {
	if u.Role != auth.RoleAdmin || !u.Enabled {
		return false, nil
	}
	n, err := ui.Store.CountAdmins(ctx)
	return n <= 1, err
}

func (ui *UI) userUpdate(w http.ResponseWriter, r *http.Request) {
	cur, ok := ui.user(w, r)
	if !ok {
		return
	}
	form, p, err := parseUserForm(w, r)
	form.ID = cur.ID
	reject := func(msg string) {
		form.Error = msg
		ui.renderUser(w, r, http.StatusUnprocessableEntity, cur, "", "", &form)
	}
	if err != nil {
		reject(err.Error())
		return
	}
	if p.password != "" {
		reject("Use “Reset password” to change the password.")
		return
	}
	if p.role != auth.RoleAdmin || !p.enabled {
		last, err := ui.lastAdmin(r.Context(), cur)
		if err != nil {
			ui.fail(w, "count admins", err)
			return
		}
		if last {
			reject("This is the last enabled admin: it cannot be demoted or disabled.")
			return
		}
	}
	err = ui.inTx(r.Context(), func(st Store) error {
		if _, err := st.UpdateUser(r.Context(), db.UpdateUserParams{ID: cur.ID, Username: p.username, PasswordHash: cur.PasswordHash,
			Role: p.role, MaxConnections: p.maxConns, ExpiresAt: p.expires, Enabled: p.enabled}); err != nil {
			return err
		}
		return setUserBouquets(r.Context(), st, cur.ID, p.bouquets)
	})
	if err != nil {
		msg, ok := userSaveErr(err)
		if !ok {
			ui.fail(w, "update user", err)
			return
		}
		reject(msg)
		return
	}
	ui.Log.Info("web: user updated", "user_id", cur.ID)
	ui.redirect(w, r, Prefix+"/users/"+strconv.FormatInt(cur.ID, 10)+"?msg=saved")
}

// userResetPassword sets a new random password and shows it once.
func (ui *UI) userResetPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := ui.user(w, r)
	if !ok {
		return
	}
	pw, err := newPassword()
	if err != nil {
		ui.fail(w, "generate password", err)
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		ui.fail(w, "hash password", err)
		return
	}
	if u, err = ui.Store.UpdateUser(r.Context(), db.UpdateUserParams{ID: u.ID, Username: u.Username, PasswordHash: hash,
		Role: u.Role, MaxConnections: u.MaxConnections, ExpiresAt: u.ExpiresAt, Enabled: u.Enabled}); err != nil {
		ui.fail(w, "reset password", err)
		return
	}
	ui.Log.Info("web: password reset", "user_id", u.ID)
	ui.renderUser(w, r, http.StatusOK, u, pw, "New password set. Copy it now: it is not shown again. Players using the old one must be updated.", nil)
}

func (ui *UI) userRotateToken(w http.ResponseWriter, r *http.Request) {
	u, ok := ui.user(w, r)
	if !ok {
		return
	}
	token, err := auth.NewStreamToken()
	if err != nil {
		ui.fail(w, "stream token", err)
		return
	}
	if _, err := ui.Store.SetUserStreamToken(r.Context(), db.SetUserStreamTokenParams{ID: u.ID, StreamToken: token}); err != nil {
		ui.fail(w, "rotate token", err)
		return
	}
	ui.Log.Info("web: stream token rotated", "user_id", u.ID)
	ui.redirect(w, r, Prefix+"/users/"+strconv.FormatInt(u.ID, 10)+"?msg=token")
}

func (ui *UI) userDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := ui.user(w, r)
	if !ok {
		return
	}
	last, err := ui.lastAdmin(r.Context(), u)
	if err != nil {
		ui.fail(w, "count admins", err)
		return
	}
	if last {
		ui.renderUser(w, r, http.StatusConflict, u, "", "", func() *userForm {
			f, _ := ui.formFromUser(r, u)
			f.Error = "This is the last enabled admin: it cannot be deleted."
			return &f
		}())
		return
	}
	if err := ui.Store.DeleteUser(r.Context(), u.ID); err != nil {
		ui.fail(w, "delete user", err)
		return
	}
	ui.Log.Info("web: user deleted", "user_id", u.ID)
	ui.redirect(w, r, Prefix+"/users?msg=deleted")
}
