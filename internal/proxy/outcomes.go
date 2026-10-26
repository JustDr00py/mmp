package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

// Outcome names one result for the provider health scores. They are stored
// in provider_outcome_buckets and weighed by internal/health: keep the two
// sets of names in step.
const (
	OutcomeOpenOK         = "open_ok"
	OutcomeOpenFailed     = "open_failed"
	OutcomeMidPlayFailure = "mid_play_failure"
	OutcomeProbeOK        = "probe_ok"
	OutcomeProbeFail      = "probe_fail"
)

// OutcomeStore persists outcome counts (satisfied by *db.Queries).
type OutcomeStore interface {
	UpsertOutcomeBucket(ctx context.Context, arg db.UpsertOutcomeBucketParams) error
}

// outcomeKey is one row of provider_outcome_buckets before it is flushed.
type outcomeKey struct {
	minute     time.Time
	providerID int64
	streamID   int64
	backbone   string
	outcome    string
}

// Outcomes counts upstream outcomes in memory and upserts them once a
// minute, so a busy channel costs one write per provider, backbone, outcome
// and minute rather than one per attempt. A nil *Outcomes records nothing.
type Outcomes struct {
	store OutcomeStore
	log   *slog.Logger

	mu     sync.Mutex
	counts map[outcomeKey]int32
}

// NewOutcomes returns a recorder that writes to store.
func NewOutcomes(store OutcomeStore, log *slog.Logger) *Outcomes {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Outcomes{store: store, log: log, counts: map[outcomeKey]int32{}}
}

// Record counts one outcome for providerID and streamID, under the backbone
// it had, now. streamID 0 means the outcome is not about one stream (probes
// count per provider only). Candidates with no provider (0) are not counted.
func (o *Outcomes) Record(providerID, streamID int64, backbone, outcome string) {
	if o == nil || providerID == 0 {
		return
	}
	k := outcomeKey{minute: time.Now().Truncate(time.Minute), providerID: providerID, streamID: streamID, backbone: backbone, outcome: outcome}
	o.mu.Lock()
	o.counts[k]++
	o.mu.Unlock()
}

// Run flushes the counts every minute until ctx ends, then once more with a
// short deadline so the last minute is not lost on shutdown.
func (o *Outcomes) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			o.flush(ctx)
		case <-ctx.Done():
			drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			o.flush(drain)
			return
		}
	}
}

// flush upserts the counts gathered so far. A row that fails to write is
// logged and dropped: the counts are advisory, and the next minute goes on.
func (o *Outcomes) flush(ctx context.Context) {
	o.mu.Lock()
	pending := o.counts
	o.counts = map[outcomeKey]int32{}
	o.mu.Unlock()
	for k, n := range pending {
		err := o.store.UpsertOutcomeBucket(ctx, db.UpsertOutcomeBucketParams{
			BucketStart: k.minute, ProviderID: k.providerID, StreamID: k.streamID, Backbone: k.backbone,
			Outcome: k.outcome, Count: n,
		})
		if err != nil && ctx.Err() == nil {
			o.log.Warn("outcomes: flush", "provider_id", k.providerID, "outcome", k.outcome, "err", err)
		}
	}
}
