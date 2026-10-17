package output

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/activity"
	"github.com/JustDr00py/mmp/internal/db"
)

type activityStore struct {
	mu       sync.Mutex
	networks []db.TouchUserNetworkParams
	watched  []db.InsertViewingSessionParams
	failures []db.InsertAuthFailureParams
}

func (s *activityStore) TouchUserNetwork(_ context.Context, a db.TouchUserNetworkParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.networks = append(s.networks, a)
	return nil
}

func (s *activityStore) InsertViewingSession(_ context.Context, a db.InsertViewingSessionParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watched = append(s.watched, a)
	return nil
}

func (s *activityStore) InsertAuthFailure(_ context.Context, a db.InsertAuthFailureParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, a)
	return nil
}

// TestActivityRecorded: logins note the client network, wrong passwords
// are recorded, and a finished live stream lands in the watch history.
func TestActivityRecorded(t *testing.T) {
	srv, hs, _ := newLiveHandlers(t)
	st := &activityStore{}
	tr := activity.New(st, nil, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	hs.Activity = tr
	hs.Access.OnViewerEnd(func(v Viewer, at time.Time) { tr.Watched(v.UserID, v.ChannelID, v.Addr, v.Output, v.Since, at) })

	if resp, _ := httpGet(t, http.DefaultClient, srv.URL+"/get.php?username=alice&password=wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", resp.StatusCode)
	}
	if resp, _ := httpGet(t, http.DefaultClient, srv.URL+"/playlist/tok-alice.m3u"); resp.StatusCode != 200 {
		t.Fatalf("token playlist = %d", resp.StatusCode)
	}
	code, stop := play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
	if code != 200 {
		t.Fatalf("play = %d", code)
	}
	time.Sleep(200 * time.Millisecond)
	stop()
	deadline := time.Now().Add(3 * time.Second)
	for len(hs.Access.Viewers()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.failures) != 1 || st.failures[0].Username != "alice" || st.failures[0].Ip != "127.0.0.1" {
		t.Errorf("failures = %+v", st.failures)
	}
	if len(st.networks) != 1 || st.networks[0].UserID != 1 || st.networks[0].Network != "127.0.0.1" {
		t.Errorf("networks = %+v, want alice once (repeats within a minute are skipped)", st.networks)
	}
	if len(st.watched) != 1 || st.watched[0].UserID != 1 || st.watched[0].ChannelID != 10 || st.watched[0].Output != "ts" ||
		!st.watched[0].EndedAt.After(st.watched[0].StartedAt) {
		t.Errorf("watched = %+v", st.watched)
	}
}
