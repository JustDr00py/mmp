package logos

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxArchiveFile  = 20 << 20 // one logo in the archive
	maxArchiveTotal = 4 << 30  // everything extracted
	updateTimeout   = 30 * time.Minute
)

// ErrBusy is returned while another update runs.
var ErrBusy = errors.New("logos: an update is already running")

// StartUpdate downloads the library in the background; false means one is
// already running.
func (l *Library) StartUpdate() bool {
	if !l.updating.TryLock() {
		return false
	}
	go func() {
		defer l.updating.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
		defer cancel()
		_ = l.update(ctx)
	}()
	return true
}

// Update downloads the library now, replacing the copy on disk.
func (l *Library) Update(ctx context.Context) error {
	if !l.updating.TryLock() {
		return ErrBusy
	}
	defer l.updating.Unlock()
	return l.update(ctx)
}

func (l *Library) update(ctx context.Context) (err error) {
	started := time.Now()
	l.setState(func(s *State) { s.Running, s.Error, s.Tried = true, "", started })
	l.Log.Info("logos: updating library", "source", l.ArchiveURL)
	defer func() {
		l.setState(func(s *State) {
			s.Running = false
			if err != nil {
				s.Error = err.Error()
			}
		})
		if err != nil {
			l.Log.Warn("logos: update failed", "err", err)
		}
	}()

	tmp, err := os.MkdirTemp(l.Dir, ".library-new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	n, err := l.download(ctx, tmp)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("the archive holds no logos")
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}

	// Swap the new copy in. Between the two renames the library is
	// briefly missing; players retry logos on their next load.
	cur, old := filepath.Join(l.Dir, libraryDir), filepath.Join(l.Dir, ".library-old")
	os.RemoveAll(old)
	if err := os.Rename(cur, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, cur); err != nil {
		os.Rename(old, cur)
		return err
	}
	os.RemoveAll(old)

	st := State{Updated: time.Now(), Files: n, Source: l.ArchiveURL}
	if b, err := json.Marshal(st); err == nil {
		if err := os.WriteFile(filepath.Join(l.Dir, stateFile), b, 0o644); err != nil {
			l.Log.Warn("logos: save state", "err", err)
		}
	}
	l.setState(func(s *State) { s.Updated, s.Files, s.Source = st.Updated, st.Files, st.Source })
	if err := l.Reindex(); err != nil {
		return err
	}
	l.Log.Info("logos: library updated", "files", n, "took", time.Since(started).Round(time.Second))
	return nil
}

func (l *Library) setState(f func(*State)) {
	l.mu.Lock()
	f(&l.state)
	l.mu.Unlock()
}

// download fetches the archive and extracts its images into dir, without
// the archive's top folder ("tv-logos-main/").
func (l *Library) download(ctx context.Context, dir string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.ArchiveURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "mmp-logos")
	resp, err := l.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download: %s", resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("not a .tar.gz archive: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	tr := tar.NewReader(gz)
	n, total := 0, int64(0)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		p, ok := archivePath(h.Name)
		if !ok || h.Size > maxArchiveFile {
			continue
		}
		if total += h.Size; total > maxArchiveTotal {
			return n, errors.New("archive is too large")
		}
		if err := root.MkdirAll(filepath.FromSlash(path.Dir(p)), 0o755); err != nil {
			return n, err
		}
		f, err := root.OpenFile(filepath.FromSlash(p), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return n, err
		}
		_, err = io.Copy(f, io.LimitReader(tr, maxArchiveFile))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return n, err
		}
		n++
	}
}

// archivePath is where an archive member goes: its path without the top
// folder, if it is an image with a sane path.
func archivePath(name string) (string, bool) {
	_, p, ok := strings.Cut(name, "/")
	if !ok || !utf8.ValidString(p) || imageTypes[strings.ToLower(path.Ext(p))] == "" {
		return "", false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || strings.HasPrefix(s, ".") || strings.Contains(s, `\`) {
			return "", false
		}
	}
	return p, true
}

// RunSchedule updates the library every so often, once it has been
// downloaded (an admin starts the first download). It returns when ctx
// ends; every <= 0 does nothing.
func (l *Library) RunSchedule(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st := l.State()
		if st.Updated.IsZero() || time.Since(st.Updated) < every || time.Since(st.Tried) < every/4 {
			continue
		}
		if err := l.Update(ctx); err != nil && !errors.Is(err, ErrBusy) {
			continue // logged; tried again after every/4
		}
	}
}
