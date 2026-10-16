package backup

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Service is what the admin UI and the command line use.
type Service struct {
	*Store
	Scheduler *Scheduler
	Migrator  Migrator
	Latest    int64 // newest migration this MMP has
}

// Create makes a backup now (waits for any running backup or restore).
func (s *Service) Create(ctx context.Context, kind string) (Info, error) {
	defer s.Scheduler.Lock()()
	return s.Store.Create(ctx, kind)
}

// Restore restores a stored backup by name, taking a safety backup first.
// It returns the safety backup's name too, so a bad restore can be undone.
func (s *Service) Restore(ctx context.Context, name string) (Manifest, string, error) {
	path, err := s.Path(name)
	if err != nil {
		return Manifest{}, "", err
	}
	return s.RestoreFile(ctx, path)
}

// RestoreFile restores a backup file from any path (mmp -restore).
func (s *Service) RestoreFile(ctx context.Context, path string) (Manifest, string, error) {
	defer s.Scheduler.Lock()()
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("restore: %w", err)
	}
	defer f.Close()
	// Refuse unusable files before touching anything.
	m, err := ReadManifest(f)
	if err != nil {
		return m, "", err
	}
	if m.Schema > s.Latest {
		return m, "", fmt.Errorf("this backup was made by a newer MMP (%s, schema %d); upgrade MMP to restore it", m.Version, m.Schema)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return m, "", err
	}
	safety, err := s.Store.Create(ctx, KindPreRestore)
	if err != nil {
		return m, "", fmt.Errorf("restore: safety backup first: %w", err)
	}
	// The backup schedule belongs to this machine, not to the data: keep it.
	sched, schedErr := LoadSchedule(ctx, s.Pool)
	m, err = Restore(ctx, s.Pool, f, s.Migrator, s.Latest, s.Dir)
	if err == nil && schedErr == nil {
		if serr := SaveSchedule(ctx, s.Pool, sched); serr != nil {
			return m, safety.Name, fmt.Errorf("restored, but the backup schedule could not be kept: %w", serr)
		}
	}
	return m, safety.Name, err
}
