// Package auth holds credential handling for admins and customers:
// password hashing, stream tokens, account validity, and (later) sessions.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/sysadmin/mmp/internal/db"
)

// Roles stored in users.role.
const (
	RoleAdmin    = "admin"
	RoleCustomer = "customer"
)

// Account state errors, returned by Usable. Callers map all of them to the
// same client-facing "unauthorized" so they do not reveal which applies.
var (
	ErrDisabled = errors.New("account disabled")
	ErrExpired  = errors.New("account expired")
)

// BcryptCost balances login latency against offline cracking. Xtream
// players re-send credentials often; 5.3 caches verifications. Tests lower
// it (bcrypt.MinCost) to stay fast under -race.
var BcryptCost = 11

// HashPassword bcrypts a validated password.
func HashPassword(pw string) (string, error) {
	if err := ValidatePassword(pw); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword reports whether pw matches hash, in constant time.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// dummyHash lets callers spend the same time on unknown usernames as on
// wrong passwords, so response timing does not reveal which users exist.
// It is built on first use, at the cost in effect then.
var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("mmp-timing-equaliser"), BcryptCost)
	return h
})

// CheckPasswordUnknownUser burns one bcrypt comparison and returns false.
func CheckPasswordUnknownUser(pw string) bool {
	_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(pw))
	return false
}

// ValidateUsername enforces names that are safe inside Xtream URL paths
// (/live/<user>/<pass>/<id>.ts) and query strings: 1-64 of [A-Za-z0-9._-].
func ValidateUsername(u string) error {
	if len(u) == 0 || len(u) > 64 {
		return errors.New("username must be 1-64 characters")
	}
	for _, r := range u {
		ok := r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-')
		if !ok {
			return errors.New("username may only contain letters, digits, '.', '_' and '-'")
		}
	}
	return nil
}

// ValidatePassword enforces 6-72 bytes (bcrypt's limit) with no characters
// that break an Xtream URL path segment: '/', '?', '#', '%', whitespace or
// control characters.
func ValidatePassword(pw string) error {
	if len(pw) < 6 || len(pw) > 72 {
		return errors.New("password must be 6-72 bytes")
	}
	for _, r := range pw {
		if r == '/' || r == '?' || r == '#' || r == '%' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("password may not contain '/', '?', '#', '%', spaces or control characters")
		}
	}
	return nil
}

// NewStreamToken returns a random URL-safe token (256 bits) for
// /playlist/{token}.m3u and /epg/{token}.xml.
func NewStreamToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("stream token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool { return r == RoleAdmin || r == RoleCustomer }

// Usable reports whether u may log in or stream at now.
func Usable(u db.User, now time.Time) error {
	if !u.Enabled {
		return ErrDisabled
	}
	if u.ExpiresAt != nil && !now.Before(*u.ExpiresAt) {
		return ErrExpired
	}
	return nil
}
