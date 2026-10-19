package web

import (
	"context"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/proxy"
)

func TestPreviewAcquireWaitsForSlot(t *testing.T) {
	p := &Preview{Capacity: proxy.NewCapacity()}
	held, ok := p.Capacity.TryAcquire(1, 1)
	if !ok {
		t.Fatal("first slot refused")
	}
	// The previous preview lets go shortly after the next one asks.
	time.AfterFunc(250*time.Millisecond, held.Release)
	slot, ok := p.acquire(context.Background(), 1, 1)
	if !ok {
		t.Fatal("acquire did not wait for the released slot")
	}
	slot.Release()
}

func TestPreviewAcquireGivesUp(t *testing.T) {
	p := &Preview{Capacity: proxy.NewCapacity()}
	held, _ := p.Capacity.TryAcquire(1, 1)
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, ok := p.acquire(ctx, 1, 1); ok {
		t.Fatal("acquire went over the provider's limit")
	}
	if n := p.Capacity.InUse(1); n != 1 {
		t.Fatalf("slots in use = %d, want 1", n)
	}
}
