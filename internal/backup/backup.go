// Package backup writes and restores MMP backups: a gzipped tar holding a
// manifest and one CSV file per table, taken from a single consistent
// snapshot. Guide data and admin login sessions are left out (guides are
// re-downloaded, sessions are just logins).
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Format is the archive layout version written into every manifest.
const Format = 1

const manifestName = "manifest.json"

// Excluded tables never go into a backup: migration bookkeeping (recorded
// as Manifest.Schema instead), login sessions, and guide programs (large,
// and re-downloaded from the providers after a restore).
var Excluded = []string{"goose_db_version", "admin_sessions", "epg_programs"}

// Manifest describes a backup.
type Manifest struct {
	Format    int         `json:"format"`
	App       string      `json:"app"`
	Version   string      `json:"version"` // MMP version that wrote it
	Schema    int64       `json:"schema"`  // migration version of the data
	Kind      string      `json:"kind"`
	CreatedAt time.Time   `json:"created_at"`
	Tables    []TableInfo `json:"tables"`
}

// TableInfo is one table in a backup.
type TableInfo struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
}

// Rows is the total row count.
func (m Manifest) Rows() int64 {
	var n int64
	for _, t := range m.Tables {
		n += t.Rows
	}
	return n
}

// Create writes a backup of the database to w. tmpDir holds the table
// files while the archive is assembled.
func Create(ctx context.Context, pool *pgxpool.Pool, w io.Writer, version, kind, tmpDir string) (Manifest, error) {
	m := Manifest{Format: Format, App: "mmp", Version: version, Kind: kind, CreatedAt: time.Now().UTC()}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return m, fmt.Errorf("backup: connect: %w", err)
	}
	defer conn.Release()
	// One snapshot for every table: rows referencing each other stay consistent.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return m, fmt.Errorf("backup: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	if m.Schema, err = schemaVersion(ctx, tx); err != nil {
		return m, err
	}
	tables, err := listTables(ctx, tx)
	if err != nil {
		return m, err
	}
	work, err := os.MkdirTemp(tmpDir, ".backup-*")
	if err != nil {
		return m, fmt.Errorf("backup: temp dir: %w", err)
	}
	defer os.RemoveAll(work)

	for _, t := range tables {
		if slices.Contains(Excluded, t) {
			continue
		}
		cols, err := copyableColumns(ctx, tx, t)
		if err != nil {
			return m, err
		}
		f, err := os.Create(filepath.Join(work, t+".csv"))
		if err != nil {
			return m, fmt.Errorf("backup: %w", err)
		}
		tag, err := tx.Conn().PgConn().CopyTo(ctx, f, fmt.Sprintf("COPY (SELECT %s FROM %s) TO STDOUT WITH (FORMAT csv)",
			quoteList(cols), pgx.Identifier{t}.Sanitize()))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return m, fmt.Errorf("backup: table %s: %w", t, err)
		}
		m.Tables = append(m.Tables, TableInfo{Name: t, Columns: cols, Rows: tag.RowsAffected()})
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mj, _ := json.MarshalIndent(m, "", "  ")
	if err := writeEntry(tw, manifestName, int64(len(mj)), strings.NewReader(string(mj))); err != nil {
		return m, err
	}
	for _, t := range m.Tables {
		if err := addFile(tw, "data/"+t.Name+".csv", filepath.Join(work, t.Name+".csv")); err != nil {
			return m, err
		}
	}
	if err := tw.Close(); err != nil {
		return m, fmt.Errorf("backup: tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return m, fmt.Errorf("backup: gzip: %w", err)
	}
	return m, nil
}

// ReadManifest reads only the manifest of a backup archive.
func ReadManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, errors.New("not an MMP backup (not gzip)")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != manifestName {
		return m, errors.New("not an MMP backup (no manifest)")
	}
	if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&m); err != nil {
		return m, fmt.Errorf("not an MMP backup (manifest: %w)", err)
	}
	if m.App != "mmp" {
		return m, errors.New("not an MMP backup")
	}
	if m.Format != Format {
		return m, fmt.Errorf("backup format %d is not supported by this MMP (format %d)", m.Format, Format)
	}
	return m, nil
}

// Migrator moves the database schema to a migration version (DownTo) or
// to the newest one (Up).
type Migrator interface {
	DownTo(ctx context.Context, v int64) error
	Up(ctx context.Context) error
}

// Restore replaces every table's contents with the backup in r. latest is
// the newest migration this MMP knows: newer backups are refused, older
// ones are restored into their own schema version and then migrated up.
func Restore(ctx context.Context, pool *pgxpool.Pool, r io.Reader, mig Migrator, latest int64, tmpDir string) (Manifest, error) {
	work, err := os.MkdirTemp(tmpDir, ".restore-*")
	if err != nil {
		return Manifest{}, fmt.Errorf("restore: temp dir: %w", err)
	}
	defer os.RemoveAll(work)
	m, err := extract(r, work)
	if err != nil {
		return m, err
	}
	if m.Schema > latest {
		return m, fmt.Errorf("this backup was made by a newer MMP (%s, schema %d); upgrade MMP to restore it", m.Version, m.Schema)
	}
	current, err := poolSchemaVersion(ctx, pool)
	if err != nil {
		return m, err
	}
	if m.Schema < current {
		if err := mig.DownTo(ctx, m.Schema); err != nil {
			return m, fmt.Errorf("restore: match the backup's schema: %w", err)
		}
	}
	if err := load(ctx, pool, m, work); err != nil {
		return m, err
	}
	if err := mig.Up(ctx); err != nil {
		return m, fmt.Errorf("restore: upgrade the restored data: %w", err)
	}
	return m, nil
}

// load truncates every table and copies the backup in, in one transaction.
func load(ctx context.Context, pool *pgxpool.Pool, m Manifest, work string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("restore: connect: %w", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("restore: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // after commit, a no-op

	tables, err := listTables(ctx, tx)
	if err != nil {
		return err
	}
	var wipe []string
	for _, t := range tables {
		if t != "goose_db_version" {
			wipe = append(wipe, pgx.Identifier{t}.Sanitize())
		}
	}
	if len(wipe) > 0 {
		if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(wipe, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
			return fmt.Errorf("restore: clear tables: %w", err)
		}
	}
	order, err := insertOrder(ctx, tx, tables)
	if err != nil {
		return err
	}
	byName := map[string]TableInfo{}
	for _, t := range m.Tables {
		byName[t.Name] = t
	}
	for _, t := range order {
		info, ok := byName[t]
		if !ok || info.Rows == 0 {
			continue
		}
		f, err := os.Open(filepath.Join(work, t+".csv"))
		if err != nil {
			return fmt.Errorf("restore: table %s missing from the backup", t)
		}
		_, err = tx.Conn().PgConn().CopyFrom(ctx, f, fmt.Sprintf("COPY %s (%s) FROM STDIN WITH (FORMAT csv)",
			pgx.Identifier{t}.Sanitize(), quoteList(info.Columns)))
		f.Close()
		if err != nil {
			return fmt.Errorf("restore: table %s: %w", t, err)
		}
	}
	// Identity and serial sequences continue after the restored ids.
	rows, err := tx.Query(ctx, `
		SELECT c.table_name, c.column_name, pg_get_serial_sequence(quote_ident(c.table_name), c.column_name)
		FROM information_schema.columns c
		WHERE c.table_schema = 'public' AND pg_get_serial_sequence(quote_ident(c.table_name), c.column_name) IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("restore: sequences: %w", err)
	}
	type seq struct{ table, column, name string }
	var seqs []seq
	for rows.Next() {
		var s seq
		if err := rows.Scan(&s.table, &s.column, &s.name); err != nil {
			rows.Close()
			return fmt.Errorf("restore: sequences: %w", err)
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	for _, s := range seqs {
		q := fmt.Sprintf("SELECT setval($1, COALESCE((SELECT max(%s) FROM %s), 0) + 1, false)",
			pgx.Identifier{s.column}.Sanitize(), pgx.Identifier{s.table}.Sanitize())
		if _, err := tx.Exec(ctx, q, s.name); err != nil {
			return fmt.Errorf("restore: sequence %s: %w", s.name, err)
		}
	}
	// Guides were not in the backup: download them again soon.
	if slices.Contains(tables, "epg_sources") {
		if _, err := tx.Exec(ctx, "UPDATE epg_sources SET last_refreshed_at = NULL"); err != nil {
			return fmt.Errorf("restore: reset guides: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("restore: commit: %w", err)
	}
	return nil
}

// insertOrder sorts tables so that every table comes after the tables its
// foreign keys point to.
func insertOrder(ctx context.Context, tx pgx.Tx, tables []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT cl.relname, ref.relname
		FROM pg_constraint c
		JOIN pg_class cl ON cl.oid = c.conrelid
		JOIN pg_class ref ON ref.oid = c.confrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE c.contype = 'f' AND n.nspname = 'public' AND c.conrelid <> c.confrelid`)
	if err != nil {
		return nil, fmt.Errorf("restore: foreign keys: %w", err)
	}
	deps := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		deps[child] = append(deps[child], parent)
	}
	rows.Close()
	var order []string
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string) error
	visit = func(t string) error {
		switch state[t] {
		case 1:
			return fmt.Errorf("restore: foreign key cycle at %s", t)
		case 2:
			return nil
		}
		state[t] = 1
		for _, p := range deps[t] {
			if err := visit(p); err != nil {
				return err
			}
		}
		state[t] = 2
		order = append(order, t)
		return nil
	}
	sorted := slices.Clone(tables)
	slices.Sort(sorted)
	for _, t := range sorted {
		if err := visit(t); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// extract unpacks a backup into dir and returns its manifest. Only the
// manifest and data/<table>.csv entries are accepted.
func extract(r io.Reader, dir string) (Manifest, error) {
	var m Manifest
	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, errors.New("not an MMP backup (not gzip)")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	first := true
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, fmt.Errorf("backup is damaged: %w", err)
		}
		if first {
			first = false
			if h.Name != manifestName {
				return m, errors.New("not an MMP backup (no manifest)")
			}
			if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&m); err != nil || m.App != "mmp" {
				return m, errors.New("not an MMP backup (bad manifest)")
			}
			if m.Format != Format {
				return m, fmt.Errorf("backup format %d is not supported by this MMP (format %d)", m.Format, Format)
			}
			continue
		}
		name, ok := strings.CutPrefix(h.Name, "data/")
		name, isCSV := strings.CutSuffix(name, ".csv")
		if !ok || !isCSV || !validTable(name) || h.Typeflag != tar.TypeReg {
			return m, fmt.Errorf("backup has an unexpected entry %q", h.Name)
		}
		f, err := os.OpenFile(filepath.Join(dir, name+".csv"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return m, fmt.Errorf("restore: %w", err)
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return m, fmt.Errorf("backup is damaged: %w", err)
		}
	}
	if first {
		return m, errors.New("not an MMP backup (empty)")
	}
	for _, t := range m.Tables {
		if !validTable(t.Name) {
			return m, fmt.Errorf("backup lists an invalid table %q", t.Name)
		}
		if _, err := os.Stat(filepath.Join(dir, t.Name+".csv")); err != nil && t.Rows > 0 {
			return m, fmt.Errorf("backup is missing table %s", t.Name)
		}
	}
	return m, nil
}

func validTable(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func schemaVersion(ctx context.Context, q querier) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, "SELECT COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied").Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("backup: schema version: %w", err)
	}
	return v, nil
}

func poolSchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	return schemaVersion(ctx, pool)
}

func listTables(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("backup: list tables: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// copyableColumns are a table's columns minus generated ones (which COPY
// FROM cannot write).
func copyableColumns(ctx context.Context, q querier, table string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND is_generated = 'NEVER'
		ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("backup: columns of %s: %w", table, err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func quoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = pgx.Identifier{c}.Sanitize()
	}
	return strings.Join(q, ", ")
}

func writeEntry(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return fmt.Errorf("backup: tar: %w", err)
	}
	if _, err := io.Copy(tw, r); err != nil {
		return fmt.Errorf("backup: tar: %w", err)
	}
	return nil
}

func addFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return writeEntry(tw, name, st.Size(), f)
}
