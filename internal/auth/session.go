package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// SessionCookie is the admin browser session cookie.
const SessionCookie = "mmp_session"

// ErrUnauthorized is the single failure callers see for any bad login or
// credential, so responses never reveal which part was wrong.
var ErrUnauthorized = errors.New("unauthorized")

// Querier is the persistence the Authenticator needs (satisfied by
// *db.Queries).
type Querier interface {
	GetUserByUsername(ctx context.Context, username string) (db.User, error)
	CreateSession(ctx context.Context, arg db.CreateSessionParams) error
	GetSessionUser(ctx context.Context, tokenHash []byte) (db.User, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error
	GetAPITokenUser(ctx context.Context, tokenHash []byte) (db.GetAPITokenUserRow, error)
	TouchAPIToken(ctx context.Context, id int64) error
}

// Authenticator handles admin logins, sessions and API tokens.
type Authenticator struct {
	q            Querier
	log          *slog.Logger
	sessionTTL   time.Duration
	secureCookie bool
}

// NewAuthenticator builds an Authenticator. secureCookie should be true
// when MMP is served over https (base_url scheme).
func NewAuthenticator(q Querier, log *slog.Logger, sessionTTL time.Duration, secureCookie bool) *Authenticator {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Authenticator{q: q, log: log, sessionTTL: sessionTTL, secureCookie: secureCookie}
}

// HashToken is the stored form of a session or API token secret.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Login verifies an admin's credentials and opens a session, returning the
// session secret for the cookie. Every failure is ErrUnauthorized.
func (a *Authenticator) Login(ctx context.Context, username, password string) (string, db.User, error) {
	u, err := a.q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			CheckPasswordUnknownUser(password) // same cost as a wrong password
			return "", db.User{}, ErrUnauthorized
		}
		return "", db.User{}, fmt.Errorf("login: %w", err)
	}
	if !CheckPassword(u.PasswordHash, password) || u.Role != RoleAdmin || Usable(u, time.Now()) != nil {
		return "", db.User{}, ErrUnauthorized
	}
	token, err := NewStreamToken() // same 256-bit random format
	if err != nil {
		return "", db.User{}, err
	}
	if err := a.q.CreateSession(ctx, db.CreateSessionParams{
		TokenHash: HashToken(token), UserID: u.ID, ExpiresAt: time.Now().Add(a.sessionTTL),
	}); err != nil {
		return "", db.User{}, fmt.Errorf("login: create session: %w", err)
	}
	a.log.Info("admin login", "user_id", u.ID)
	return token, u, nil
}

// Logout ends the session carried by r, if any.
func (a *Authenticator) Logout(ctx context.Context, r *http.Request) error {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	return a.q.DeleteSession(ctx, HashToken(c.Value))
}

// SetSessionCookie writes the session cookie (or clears it when token is "").
func (a *Authenticator) SetSessionCookie(w http.ResponseWriter, token string) {
	c := &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode,
		MaxAge: int(a.sessionTTL / time.Second),
	}
	if token == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// Authenticate resolves the admin behind r: a Bearer API token or the
// session cookie. The account must still be an enabled, unexpired admin.
func (a *Authenticator) Authenticate(r *http.Request) (db.User, error) {
	ctx := r.Context()
	var u db.User
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			return db.User{}, ErrUnauthorized
		}
		row, err := a.q.GetAPITokenUser(ctx, HashToken(tok))
		if err != nil {
			return db.User{}, a.lookupErr(err)
		}
		if err := a.q.TouchAPIToken(ctx, row.TokenID); err != nil {
			a.log.Warn("touch api token", "token_id", row.TokenID, "err", err)
		}
		u = db.User{ID: row.ID, Username: row.Username, PasswordHash: row.PasswordHash, Role: row.Role,
			MaxConnections: row.MaxConnections, ExpiresAt: row.ExpiresAt, Enabled: row.Enabled,
			StreamToken: row.StreamToken, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	} else {
		c, err := r.Cookie(SessionCookie)
		if err != nil || c.Value == "" {
			return db.User{}, ErrUnauthorized
		}
		if u, err = a.q.GetSessionUser(ctx, HashToken(c.Value)); err != nil {
			return db.User{}, a.lookupErr(err)
		}
	}
	if u.Role != RoleAdmin || Usable(u, time.Now()) != nil {
		return db.User{}, ErrUnauthorized
	}
	return u, nil
}

func (a *Authenticator) lookupErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	return fmt.Errorf("authenticate: %w", err)
}

type ctxKey struct{}

// WithUser stores an authenticated admin in ctx, for UserFrom.
func WithUser(ctx context.Context, u db.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the admin stored by RequireAdmin (or WithUser).
func UserFrom(ctx context.Context) (db.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(db.User)
	return u, ok
}

// RequireAdmin rejects requests without a valid admin credential with 401
// (or 500 if the lookup itself failed) and stores the admin in the context.
func (a *Authenticator) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Authenticate(r)
		if err != nil {
			status, msg := http.StatusUnauthorized, "unauthorized"
			if !errors.Is(err, ErrUnauthorized) {
				a.log.Error("auth lookup failed", "err", err)
				status, msg = http.StatusInternalServerError, "auth unavailable"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// BootstrapQuerier is what EnsureAdmin needs (satisfied by *db.Queries).
type BootstrapQuerier interface {
	CountAdmins(ctx context.Context) (int64, error)
	CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error)
}

// EnsureAdmin creates the first admin from configured credentials when no
// enabled admin exists. With none configured it only warns: a fresh
// install is otherwise unmanageable.
func EnsureAdmin(ctx context.Context, q BootstrapQuerier, log *slog.Logger, username, password string) error {
	n, err := q.CountAdmins(ctx)
	if err != nil {
		return fmt.Errorf("ensure admin: %w", err)
	}
	if n > 0 {
		return nil
	}
	if username == "" || password == "" {
		log.Warn("no admin user exists: set MMP_ADMIN_USERNAME and MMP_ADMIN_PASSWORD (or admin: in config) and restart to create one")
		return nil
	}
	if err := ValidateUsername(username); err != nil {
		return fmt.Errorf("ensure admin: %w", err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("ensure admin: %w", err)
	}
	token, err := NewStreamToken()
	if err != nil {
		return err
	}
	u, err := q.CreateUser(ctx, db.CreateUserParams{
		Username: username, PasswordHash: hash, Role: RoleAdmin, MaxConnections: 1,
		Enabled: true, StreamToken: token,
	})
	if err != nil {
		return fmt.Errorf("ensure admin: create: %w", err)
	}
	log.Info("created initial admin user", "user_id", u.ID, "username", u.Username)
	return nil
}
