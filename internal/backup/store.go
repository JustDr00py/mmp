package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Kinds of backup, part of the file name.
const (
	KindAuto       = "auto"        // made by the schedule; pruned to the "keep" setting
	KindManual     = "manual"      // "Back up now" or mmp -backup
	KindPreRestore = "pre-restore" // safety copy taken before every restore
	KindUpload     = "upload"      // uploaded through the admin UI
)

// fileRe is the only file name the store reads, serves or deletes: the
// kind, a UTC timestamp and an optional counter.
var fileRe = regexp.MustCompile(`^mmp-(auto|manual|pre-restore|upload)-(\d{8}-\d{6})(-\d+)?\.tar\.gz$`)

// ErrNotFound is returned for a name that is not a backup in the store.
var ErrNotFound = errors.New("backup not found")

// Store is the backup directory.
type Store struct {
	Dir     string
	Pool    *pgxpool.Pool
	Version string // MMP version written into new backups
}

// Info is one backup file.
type Info struct {
	Name     string
	Kind     string
	Size     int64
	Created  time.Time
	Manifest Manifest
	Err      string // why the manifest could not be read
}

// List returns the backups, newest first.
func (s *Store) List() ([]Info, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backups: %w", err)
	}
	var out []Info
	for _, e := range entries {
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		info := Info{Name: e.Name(), Kind: m[1]}
		if t, err := time.Parse("20060102-150405", m[2]); err == nil {
			info.Created = t
		}
		if st, err := e.Info(); err == nil {
			info.Size = st.Size()
		}
		if f, err := os.Open(filepath.Join(s.Dir, e.Name())); err == nil {
			if info.Manifest, err = ReadManifest(f); err != nil {
				info.Err = err.Error()
			} else {
				info.Created = info.Manifest.CreatedAt
			}
			f.Close()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Path is the file of a backup name, or ErrNotFound.
func (s *Store) Path(name string) (string, error) {
	if !fileRe.MatchString(name) {
		return "", ErrNotFound
	}
	p := filepath.Join(s.Dir, name)
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		return "", ErrNotFound
	}
	return p, nil
}

// Create makes a backup of the given kind and returns its name.
func (s *Store) Create(ctx context.Context, kind string) (Info, error) {
	return s.save(kind, func(w io.Writer) (Manifest, error) {
		return Create(ctx, s.Pool, w, s.Version, kind, s.Dir)
	})
}

// Import stores an uploaded backup after checking it is one.
func (s *Store) Import(r io.Reader) (Info, error) {
	return s.save(KindUpload, func(w io.Writer) (Manifest, error) {
		tmp, ok := w.(*os.File)
		if !ok {
			return Manifest{}, errors.New("backups: import needs a file")
		}
		if _, err := io.Copy(tmp, r); err != nil {
			return Manifest{}, fmt.Errorf("upload: %w", err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return Manifest{}, err
		}
		m, err := ReadManifest(tmp)
		if err != nil {
			return m, err
		}
		_, err = tmp.Seek(0, io.SeekEnd)
		return m, err
	})
}

// save writes a new backup through write into a temporary file and moves
// it into place only when complete.
func (s *Store) save(kind string, write func(io.Writer) (Manifest, error)) (Info, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Info{}, fmt.Errorf("backups: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".partial-*")
	if err != nil {
		return Info{}, fmt.Errorf("backups: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	m, err := write(tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Info{}, err
	}
	name := s.freeName(kind, time.Now().UTC())
	if err := os.Rename(tmp.Name(), filepath.Join(s.Dir, name)); err != nil {
		return Info{}, fmt.Errorf("backups: %w", err)
	}
	st, _ := os.Stat(filepath.Join(s.Dir, name))
	info := Info{Name: name, Kind: kind, Manifest: m, Created: time.Now().UTC()}
	if st != nil {
		info.Size = st.Size()
	}
	return info, nil
}

func (s *Store) freeName(kind string, t time.Time) string {
	base := fmt.Sprintf("mmp-%s-%s", kind, t.Format("20060102-150405"))
	name := base + ".tar.gz"
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(s.Dir, name)); errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s-%d.tar.gz", base, i)
	}
}

// Delete removes a backup.
func (s *Store) Delete(name string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// Prune deletes the oldest automatic backups beyond keep. Other kinds are
// only ever deleted by hand.
func (s *Store) Prune(keep int) (int, error) {
	all, err := s.List()
	if err != nil {
		return 0, err
	}
	var auto []Info
	for _, b := range all {
		if b.Kind == KindAuto {
			auto = append(auto, b)
		}
	}
	removed := 0
	for _, b := range auto[min(len(auto), max(keep, 1)):] {
		if err := s.Delete(b.Name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// LastAuto is the time of the newest automatic backup (zero if none).
func (s *Store) LastAuto() time.Time {
	all, _ := s.List()
	i := slices.IndexFunc(all, func(b Info) bool { return b.Kind == KindAuto })
	if i < 0 {
		return time.Time{}
	}
	return all[i].Created
}
