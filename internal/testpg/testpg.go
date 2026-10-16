// Package testpg locates the Postgres server integration tests use.
package testpg

import (
	"os"
	"testing"
)

// defaultBase matches a plain `make up` with the example credentials.
const defaultBase = "postgres://mmp:mmp@localhost:5432"

// DSN is the connection string for database name on the test server:
// MMP_TEST_PG (e.g. postgres://mmp:secret@localhost:5432; `make` sets it
// from .env) or the default.
func DSN(name string) string {
	base := os.Getenv("MMP_TEST_PG")
	if base == "" {
		base = defaultBase
	}
	return base + "/" + name + "?sslmode=disable"
}

// Unreachable reports a failed connection: a skip when no server was
// configured, but a failure when MMP_TEST_PG names one, so a wrong
// password never turns the integration suite into a silent pass.
func Unreachable(t testing.TB, err error) {
	t.Helper()
	if os.Getenv("MMP_TEST_PG") != "" {
		t.Fatalf("postgres at MMP_TEST_PG not usable: %v", err)
	}
	t.Skipf("postgres not reachable (%v); start it with `make up`", err)
}
