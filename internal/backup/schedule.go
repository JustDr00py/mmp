package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schedule is the automatic backup setting, edited in the admin UI.
type Schedule struct {
	Enabled bool   `json:"enabled"`
	Every   string `json:"every"` // 6h, 12h, daily, weekly
	At      string `json:"at"`    // HH:MM, server time: daily/weekly time, and the anchor for 6h/12h
	Keep    int    `json:"keep"`  // automatic backups to keep
	// EnabledAt keeps a just-enabled schedule from firing for a slot that
	// passed before it was turned on.
	EnabledAt time.Time `json:"enabled_at"`
}

// Frequencies offered in the UI.
var Frequencies = []string{"6h", "12h", "daily", "weekly"}

// DefaultSchedule: off; when turned on, daily at 03:00 keeping 7.
func DefaultSchedule() Schedule { return Schedule{Every: "daily", At: "03:00", Keep: 7} }

// Validate checks a schedule from the form.
func (s Schedule) Validate() error {
	switch s.Every {
	case "6h", "12h", "daily", "weekly":
	default:
		return errors.New("choose how often to back up")
	}
	if _, _, err := parseAt(s.At); err != nil {
		return err
	}
	if s.Keep < 1 || s.Keep > 365 {
		return errors.New("keep between 1 and 365 automatic backups")
	}
	return nil
}

func (s Schedule) period() time.Duration {
	switch s.Every {
	case "6h":
		return 6 * time.Hour
	case "12h":
		return 12 * time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

func parseAt(at string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(at, ":")
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if !ok || err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, errors.New("time must be HH:MM")
	}
	return h, m, nil
}

// slot is the latest scheduled time at or before now. Slots are At plus
// whole periods; weekly slots fall on Sundays.
func (s Schedule) slot(now time.Time) time.Time {
	h, m, err := parseAt(s.At)
	if err != nil {
		h, m = 3, 0
	}
	anchor := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if s.Every == "weekly" {
		anchor = anchor.AddDate(0, 0, -int(anchor.Weekday()))
	}
	p := s.period()
	k := math.Floor(float64(now.Sub(anchor)) / float64(p))
	return anchor.Add(time.Duration(k) * p)
}

// Next is when the next automatic backup runs (zero when disabled).
func (s Schedule) Next(now time.Time) time.Time {
	if !s.Enabled {
		return time.Time{}
	}
	return s.slot(now).Add(s.period())
}

// Due reports whether a backup should run now, given the last automatic one.
func (s Schedule) Due(now, last time.Time) bool {
	if !s.Enabled {
		return false
	}
	slot := s.slot(now)
	return slot.After(last) && !slot.Before(s.EnabledAt)
}

const scheduleKey = "backup.schedule"

// LoadSchedule reads the schedule (the default if never saved).
func LoadSchedule(ctx context.Context, pool *pgxpool.Pool) (Schedule, error) {
	s := DefaultSchedule()
	var raw []byte
	err := pool.QueryRow(ctx, "SELECT value FROM settings WHERE key = $1", scheduleKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("backup schedule: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return DefaultSchedule(), fmt.Errorf("backup schedule: %w", err)
	}
	return s, nil
}

// SaveSchedule stores the schedule.
func SaveSchedule(ctx context.Context, pool *pgxpool.Pool, s Schedule) error {
	raw, _ := json.Marshal(s)
	_, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, scheduleKey, raw)
	if err != nil {
		return fmt.Errorf("backup schedule: %w", err)
	}
	return nil
}

// Status is the outcome of the latest automatic backup.
type Status struct {
	At    time.Time
	Name  string
	Error string
}

// Scheduler runs automatic backups.
type Scheduler struct {
	Store *Store
	Log   *slog.Logger
	Now   func() time.Time // tests

	mu     sync.Mutex
	status Status
	busy   sync.Mutex // one backup or restore at a time
}

// Lock serialises backups and restores (also taken by the admin UI).
func (s *Scheduler) Lock() func() {
	s.busy.Lock()
	return s.busy.Unlock
}

// Status is the latest automatic run.
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Run checks the schedule every minute until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs an automatic backup if one is due.
func (s *Scheduler) Tick(ctx context.Context) {
	sched, err := LoadSchedule(ctx, s.Store.Pool)
	if err != nil {
		s.Log.Error("backup: load schedule", "err", err)
		return
	}
	if !sched.Due(s.now(), s.Store.LastAuto()) {
		return
	}
	// After a failure, retry every 15 minutes rather than every tick.
	if st := s.Status(); st.Error != "" && s.now().Sub(st.At) < 15*time.Minute {
		return
	}
	unlock := s.Lock()
	defer unlock()
	info, err := s.Store.Create(ctx, KindAuto)
	st := Status{At: s.now(), Name: info.Name}
	if err != nil {
		st.Error = err.Error()
		s.Log.Error("backup: automatic backup failed", "err", err)
	} else {
		removed, perr := s.Store.Prune(sched.Keep)
		if perr != nil {
			s.Log.Warn("backup: prune", "err", perr)
		}
		s.Log.Info("backup: automatic backup done", "file", info.Name, "bytes", info.Size, "pruned", removed)
	}
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}
