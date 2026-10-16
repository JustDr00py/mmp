package backup

import (
	"testing"
	"time"
)

func TestScheduleSlotsAndDue(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	daily := Schedule{Enabled: true, Every: "daily", At: "03:00", Keep: 7}
	if got := daily.slot(at("2026-10-16 02:59")); !got.Equal(at("2026-10-15 03:00")) {
		t.Errorf("daily slot before 03:00 = %v", got)
	}
	if got := daily.slot(at("2026-10-16 03:00")); !got.Equal(at("2026-10-16 03:00")) {
		t.Errorf("daily slot at 03:00 = %v", got)
	}
	if got := daily.Next(at("2026-10-16 10:00")); !got.Equal(at("2026-10-17 03:00")) {
		t.Errorf("daily next = %v", got)
	}
	six := Schedule{Enabled: true, Every: "6h", At: "01:30"}
	if got := six.slot(at("2026-10-16 00:10")); !got.Equal(at("2026-10-15 19:30")) {
		t.Errorf("6h slot = %v", got)
	}
	if got := six.slot(at("2026-10-16 14:00")); !got.Equal(at("2026-10-16 13:30")) {
		t.Errorf("6h slot = %v", got)
	}
	weekly := Schedule{Enabled: true, Every: "weekly", At: "04:00"} // 2026-10-18 is a Sunday
	if got := weekly.slot(at("2026-10-16 12:00")); !got.Equal(at("2026-10-11 04:00")) {
		t.Errorf("weekly slot = %v", got)
	}
	if got := weekly.Next(at("2026-10-16 12:00")); !got.Equal(at("2026-10-18 04:00")) {
		t.Errorf("weekly next = %v", got)
	}

	// Due: a slot passed since the last backup, and after enabling.
	daily.EnabledAt = at("2026-10-14 12:00")
	if !daily.Due(at("2026-10-16 03:01"), at("2026-10-15 03:00")) {
		t.Error("not due after a new slot")
	}
	if daily.Due(at("2026-10-16 03:01"), at("2026-10-16 03:00")) {
		t.Error("due twice for one slot")
	}
	justOn := daily
	justOn.EnabledAt = at("2026-10-16 10:00")
	if justOn.Due(at("2026-10-16 10:01"), time.Time{}) {
		t.Error("fired for a slot before it was enabled")
	}
	if !justOn.Due(at("2026-10-17 03:00"), time.Time{}) {
		t.Error("not due at the first slot after enabling")
	}
	off := daily
	off.Enabled = false
	if off.Due(at("2026-10-16 03:01"), time.Time{}) || !off.Next(at("2026-10-16 03:01")).IsZero() {
		t.Error("disabled schedule runs")
	}
}

func TestScheduleValidate(t *testing.T) {
	ok := DefaultSchedule()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Schedule{
		{Every: "hourly", At: "03:00", Keep: 7}, {Every: "daily", At: "25:00", Keep: 7},
		{Every: "daily", At: "3", Keep: 7}, {Every: "daily", At: "03:00", Keep: 0}, {Every: "daily", At: "03:00", Keep: 999},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestFileNames(t *testing.T) {
	for _, n := range []string{"mmp-auto-20261016-030000.tar.gz", "mmp-pre-restore-20261016-030000-2.tar.gz", "mmp-upload-20261016-030000.tar.gz"} {
		if !fileRe.MatchString(n) {
			t.Errorf("rejected %q", n)
		}
	}
	for _, n := range []string{"../etc/passwd", "mmp-auto-20261016-030000.tar.gz/..", "x.tar.gz", "mmp-evil-20261016-030000.tar.gz", ".partial-123"} {
		if fileRe.MatchString(n) {
			t.Errorf("accepted %q", n)
		}
	}
}
