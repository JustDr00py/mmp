package output

import (
	"context"
	"strings"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/nameguide"
)

// NameFallback gives a channel without a guide its stream-name guide for
// [since, until), or nothing.
type NameFallback func(c Channel, since, until time.Time) []db.ListGuideProgramsRow

// nameGuideFor reads the stream-name guide settings once and returns the
// fallback for one request.
func (h *Handlers) nameGuideFor(ctx context.Context, now time.Time) NameFallback {
	if h.NameGuide == nil {
		return func(Channel, time.Time, time.Time) []db.ListGuideProgramsRow { return nil }
	}
	s := h.NameGuide(ctx)
	return func(c Channel, since, until time.Time) []db.ListGuideProgramsRow {
		ev, ok := s.For(c.Name, c.StreamName, now)
		if !ok {
			return nil
		}
		key := strings.ToLower(c.GuideID())
		var out []db.ListGuideProgramsRow
		for _, p := range nameguide.Programs(ev, since, until) {
			out = append(out, db.ListGuideProgramsRow{TvgKey: key, ID: p.Start.Unix(), Title: p.Title,
				Description: p.Desc, StartTime: p.Start, StopTime: p.Stop})
		}
		return out
	}
}
