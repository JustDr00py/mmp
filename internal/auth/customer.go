package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/mmp/internal/db"
)

// CustomerQuerier is what customer authentication reads (satisfied by
// *db.Queries).
type CustomerQuerier interface {
	GetUserByUsername(ctx context.Context, username string) (db.User, error)
	GetUserByStreamToken(ctx context.Context, token string) (db.User, error)
}

// Customers authenticates playback clients: Xtream username/password (sent
// on every request by players) or a stream token. Both admins and
// customers may use the customer outputs.
type Customers struct {
	q   CustomerQuerier
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]verified
}

// verified records that a username/password pair matched hash at some time.
type verified struct {
	hash string
	at   time.Time
}

// maxCacheEntries bounds the verification cache; when full it is reset.
const maxCacheEntries = 10000

// NewCustomers builds an authenticator that caches successful password
// verifications for ttl (bcrypt per player request would be ~100ms CPU).
func NewCustomers(q CustomerQuerier, ttl time.Duration) *Customers {
	return &Customers{q: q, ttl: ttl, now: time.Now, cache: map[[32]byte]verified{}}
}

// ByPassword authenticates Xtream-style credentials. The user row is read
// on every call, so a password change, disable or expiry applies at once;
// only the bcrypt comparison is cached. All failures are ErrUnauthorized.
func (c *Customers) ByPassword(ctx context.Context, username, password string) (db.User, error) {
	u, err := c.q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			CheckPasswordUnknownUser(password)
			return db.User{}, ErrUnauthorized
		}
		return db.User{}, fmt.Errorf("customer auth: %w", err)
	}
	if !c.passwordOK(u, username, password) || Usable(u, c.now()) != nil {
		return db.User{}, ErrUnauthorized
	}
	return u, nil
}

// keyFor is the cache key for a credential pair; the plaintext is not kept.
func (c *Customers) keyFor(username, password string) [32]byte {
	return sha256.Sum256([]byte(username + "\x00" + password))
}

func (c *Customers) passwordOK(u db.User, username, password string) bool {
	key := c.keyFor(username, password)
	now := c.now()
	c.mu.Lock()
	v, hit := c.cache[key]
	c.mu.Unlock()
	if hit && v.hash == u.PasswordHash && now.Sub(v.at) < c.ttl {
		return true
	}
	if !CheckPassword(u.PasswordHash, password) {
		return false
	}
	c.mu.Lock()
	if len(c.cache) >= maxCacheEntries {
		clear(c.cache)
	}
	c.cache[key] = verified{hash: u.PasswordHash, at: now}
	c.mu.Unlock()
	return true
}

// ByToken authenticates a stream token (playlist and EPG URLs).
func (c *Customers) ByToken(ctx context.Context, token string) (db.User, error) {
	if token == "" {
		return db.User{}, ErrUnauthorized
	}
	u, err := c.q.GetUserByStreamToken(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.User{}, ErrUnauthorized
		}
		return db.User{}, fmt.Errorf("customer auth: %w", err)
	}
	if Usable(u, c.now()) != nil {
		return db.User{}, ErrUnauthorized
	}
	return u, nil
}
