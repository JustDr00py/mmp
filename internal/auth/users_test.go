package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestMain(m *testing.M) {
	BcryptCost = bcrypt.MinCost
	m.Run()
}

func TestHashAndCheckPassword(t *testing.T) {
	h, err := HashPassword("s3cret-pw")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "s3cret-pw") || !strings.HasPrefix(h, "$2") {
		t.Fatalf("not a bcrypt hash: %q", h)
	}
	if !CheckPassword(h, "s3cret-pw") {
		t.Error("correct password rejected")
	}
	for _, wrong := range []string{"", "s3cret-PW", "s3cret-pw ", "x"} {
		if CheckPassword(h, wrong) {
			t.Errorf("wrong password %q accepted", wrong)
		}
	}
	h2, _ := HashPassword("s3cret-pw")
	if h == h2 {
		t.Error("hashes not salted")
	}
	if CheckPasswordUnknownUser("s3cret-pw") {
		t.Error("unknown-user check must always fail")
	}
}

func TestValidateUsername(t *testing.T) {
	for _, ok := range []string{"alice", "a", "user.name_01-x", strings.Repeat("z", 64)} {
		if err := ValidateUsername(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("z", 65), "a/b", "a b", "a?b", "a%2F", "ünï", "a:b", "a@b"} {
		if ValidateUsername(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	for _, ok := range []string{"abcdef", "P@ss:w0rd!", "ünïcödé", strings.Repeat("x", 72)} {
		if err := ValidatePassword(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("x", 73), "has/slash", "has space", "q?mark", "hash#tag", "pct%41x", "tab\tbed"} {
		if ValidatePassword(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := HashPassword("a/b/cdef"); err == nil {
		t.Error("HashPassword must validate first")
	}
}

func TestNewStreamToken(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := NewStreamToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 43 || strings.ContainsAny(tok, "+/=") {
			t.Fatalf("token %q not 43 url-safe chars", tok)
		}
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
}

func TestUsable(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	tests := []struct {
		name string
		u    db.User
		want error
	}{
		{"active, no expiry", db.User{Enabled: true}, nil},
		{"active, future expiry", db.User{Enabled: true, ExpiresAt: &future}, nil},
		{"expired", db.User{Enabled: true, ExpiresAt: &past}, ErrExpired},
		{"expires exactly now", db.User{Enabled: true, ExpiresAt: &now}, ErrExpired},
		{"disabled beats expiry", db.User{Enabled: false, ExpiresAt: &past}, ErrDisabled},
	}
	for _, tt := range tests {
		if got := Usable(tt.u, now); !errors.Is(got, tt.want) {
			t.Errorf("%s: Usable = %v, want %v", tt.name, got, tt.want)
		}
	}
	if !ValidRole(RoleAdmin) || !ValidRole(RoleCustomer) || ValidRole("root") {
		t.Error("ValidRole wrong")
	}
}
