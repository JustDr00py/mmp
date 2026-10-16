package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JustDr00py/mmp/internal/auth"
	"github.com/JustDr00py/mmp/internal/db"
)

// MountUsers registers admin user management routes.
func MountUsers(r chi.Router, env *Env) {
	r.Route("/users", func(r chi.Router) {
		r.Get("/", env.listUsers)
		r.Post("/", env.createUser)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", env.getUser)
			r.Put("/", env.updateUser)
			r.Delete("/", env.deleteUser)
			r.Post("/rotate-token", env.rotateUserToken)
		})
	})
}

// userView is the API shape of a user: never the password hash.
type userView struct {
	ID             int64      `json:"id"`
	Username       string     `json:"username"`
	Role           string     `json:"role"`
	MaxConnections int32      `json:"max_connections"`
	ExpiresAt      *time.Time `json:"expires_at"`
	Enabled        bool       `json:"enabled"`
	StreamToken    string     `json:"stream_token"`
	BouquetIDs     []int64    `json:"bouquet_ids"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (env *Env) viewUser(ctx context.Context, u db.User) (userView, error) {
	ids, err := env.Store.ListUserBouquetIDs(ctx, u.ID)
	if err != nil {
		return userView{}, err
	}
	if ids == nil {
		ids = []int64{}
	}
	return userView{
		ID: u.ID, Username: u.Username, Role: u.Role, MaxConnections: u.MaxConnections,
		ExpiresAt: u.ExpiresAt, Enabled: u.Enabled, StreamToken: u.StreamToken,
		BouquetIDs: ids, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}, nil
}

// userInput is the create/update body. On update, an empty password keeps
// the current one and omitted fields keep their values.
type userInput struct {
	Username       string     `json:"username"`
	Password       string     `json:"password"`
	Role           string     `json:"role"`
	MaxConnections *int32     `json:"max_connections"`
	ExpiresAt      *time.Time `json:"expires_at"`
	ClearExpiry    bool       `json:"clear_expiry"` // update only: remove expires_at
	Enabled        *bool      `json:"enabled"`
	BouquetIDs     *[]int64   `json:"bouquet_ids"` // replaces the set when present
}

func (in *userInput) validate(creating bool) error {
	if err := auth.ValidateUsername(in.Username); err != nil {
		return err
	}
	if creating || in.Password != "" {
		if err := auth.ValidatePassword(in.Password); err != nil {
			return err
		}
	}
	if in.Role != "" && !auth.ValidRole(in.Role) {
		return errors.New("role must be admin or customer")
	}
	if in.MaxConnections != nil && *in.MaxConnections < 0 {
		return errors.New("max_connections must be >= 0")
	}
	return nil
}

// Postgres error codes the API maps to client errors.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

func pgCode(err error) string {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pe.Code
	}
	return ""
}

// writeUserErr maps store errors from user operations to responses.
func (env *Env) writeUserErr(w http.ResponseWriter, op string, err error) {
	switch {
	case isNotFound(err):
		writeErr(w, http.StatusNotFound, "user not found")
	case pgCode(err) == pgUniqueViolation:
		writeErr(w, http.StatusConflict, "username already exists")
	case pgCode(err) == pgForeignKeyViolation:
		writeErr(w, http.StatusBadRequest, "unknown bouquet id")
	default:
		env.Log.Error("api: "+op, "err", err)
		writeErr(w, http.StatusInternalServerError, op+" failed")
	}
}

// setBouquets replaces userID's bouquet grants when ids is present. Run it
// in the same transaction as the user write so a bad id rolls back both.
func setBouquets(ctx context.Context, st Store, userID int64, ids *[]int64) error {
	if ids == nil {
		return nil
	}
	if err := st.ClearUserBouquets(ctx, userID); err != nil {
		return err
	}
	if len(*ids) == 0 {
		return nil
	}
	return st.AddUserBouquets(ctx, db.AddUserBouquetsParams{UserID: userID, BouquetIds: *ids})
}

func (env *Env) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := env.Store.ListUsers(r.Context())
	if err != nil {
		env.writeUserErr(w, "list users", err)
		return
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		v, err := env.viewUser(r.Context(), u)
		if err != nil {
			env.writeUserErr(w, "list users", err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (env *Env) createUser(w http.ResponseWriter, r *http.Request) {
	var in userInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := in.validate(true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	token, err := auth.NewStreamToken()
	if err != nil {
		env.writeUserErr(w, "create user", err)
		return
	}
	role := in.Role
	if role == "" {
		role = auth.RoleCustomer
	}
	var u db.User
	err = env.inTx(r.Context(), func(st Store) error {
		var err error
		u, err = st.CreateUser(r.Context(), db.CreateUserParams{
			Username: in.Username, PasswordHash: hash, Role: role,
			MaxConnections: defInt32(in.MaxConnections, 1), ExpiresAt: in.ExpiresAt,
			Enabled: defBool(in.Enabled, true), StreamToken: token,
		})
		if err != nil {
			return err
		}
		return setBouquets(r.Context(), st, u.ID, in.BouquetIDs)
	})
	if err != nil {
		env.writeUserErr(w, "create user", err)
		return
	}
	v, err := env.viewUser(r.Context(), u)
	if err != nil {
		env.writeUserErr(w, "create user", err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (env *Env) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := env.Store.GetUser(r.Context(), id)
	if err != nil {
		env.writeUserErr(w, "get user", err)
		return
	}
	v, err := env.viewUser(r.Context(), u)
	if err != nil {
		env.writeUserErr(w, "get user", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// isLastAdmin reports whether u is the only enabled admin.
func (env *Env) isLastAdmin(ctx context.Context, u db.User) (bool, error) {
	if u.Role != auth.RoleAdmin || !u.Enabled {
		return false, nil
	}
	n, err := env.Store.CountAdmins(ctx)
	return n <= 1, err
}

func (env *Env) updateUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in userInput
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	cur, err := env.Store.GetUser(r.Context(), id)
	if err != nil {
		env.writeUserErr(w, "update user", err)
		return
	}
	if in.Username == "" {
		in.Username = cur.Username
	}
	if err := in.validate(false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := db.UpdateUserParams{
		ID: id, Username: in.Username, PasswordHash: cur.PasswordHash, Role: cur.Role,
		MaxConnections: defInt32(in.MaxConnections, cur.MaxConnections),
		ExpiresAt:      cur.ExpiresAt, Enabled: defBool(in.Enabled, cur.Enabled),
	}
	if in.Role != "" {
		p.Role = in.Role
	}
	if in.ClearExpiry {
		p.ExpiresAt = nil
	} else if in.ExpiresAt != nil {
		p.ExpiresAt = in.ExpiresAt
	}
	if in.Password != "" {
		if p.PasswordHash, err = auth.HashPassword(in.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if p.Role != auth.RoleAdmin || !p.Enabled {
		last, err := env.isLastAdmin(r.Context(), cur)
		if err != nil {
			env.writeUserErr(w, "update user", err)
			return
		}
		if last {
			writeErr(w, http.StatusConflict, "cannot demote or disable the last enabled admin")
			return
		}
	}
	var u db.User
	err = env.inTx(r.Context(), func(st Store) error {
		var err error
		if u, err = st.UpdateUser(r.Context(), p); err != nil {
			return err
		}
		return setBouquets(r.Context(), st, id, in.BouquetIDs)
	})
	if err != nil {
		env.writeUserErr(w, "update user", err)
		return
	}
	v, err := env.viewUser(r.Context(), u)
	if err != nil {
		env.writeUserErr(w, "update user", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (env *Env) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cur, err := env.Store.GetUser(r.Context(), id)
	if err != nil {
		env.writeUserErr(w, "delete user", err)
		return
	}
	last, err := env.isLastAdmin(r.Context(), cur)
	if err != nil {
		env.writeUserErr(w, "delete user", err)
		return
	}
	if last {
		writeErr(w, http.StatusConflict, "cannot delete the last enabled admin")
		return
	}
	if err := env.Store.DeleteUser(r.Context(), id); err != nil {
		env.writeUserErr(w, "delete user", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateUserToken issues a new stream token, invalidating the user's
// existing /playlist and /epg URLs.
func (env *Env) rotateUserToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	token, err := auth.NewStreamToken()
	if err != nil {
		env.writeUserErr(w, "rotate token", err)
		return
	}
	u, err := env.Store.SetUserStreamToken(r.Context(), db.SetUserStreamTokenParams{ID: id, StreamToken: token})
	if err != nil {
		env.writeUserErr(w, "rotate token", err)
		return
	}
	v, err := env.viewUser(r.Context(), u)
	if err != nil {
		env.writeUserErr(w, "rotate token", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
