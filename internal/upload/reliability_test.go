package upload

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/metrics"
	"github.com/jeonghun-app/transmux/internal/storage"
)

type congestedStore struct {
	stubStore
	started chan struct{}
	release chan struct{}
}

func (s *congestedStore) Put(ctx context.Context, obj storage.Object) (string, error) {
	if obj.Key == "segment.ts" {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.stubStore.Put(ctx, obj)
}

func TestLeaseTrafficSurvivesFullSegmentUploadQueue(t *testing.T) {
	store := &congestedStore{started: make(chan struct{}), release: make(chan struct{})}
	cfg := testCfg()
	cfg.MaxConcurrent = 1
	c := NewCoordinator(store, cfg, metrics.NewRegistry())
	done := make(chan error, 1)
	go func() {
		_, err := c.Put(context.Background(), storage.Object{Key: "segment.ts"})
		done <- err
	}()
	<-store.started
	defer func() { close(store.release); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	control := c.LeaseClient()
	if _, _, err := control.Get(ctx, "lease.json"); err != nil {
		t.Fatalf("lease read queued behind a segment upload: %v", err)
	}
	if _, err := control.Put(ctx, storage.Object{
		Key: "lease.json", Preconditions: storage.Preconditions{IfMatch: `"lease"`},
	}); err != nil {
		t.Fatalf("lease renewal queued behind a segment upload: %v", err)
	}
}

func TestBackoffSupportsTheFullDurationRange(t *testing.T) {
	cfg := testCfg()
	cfg.RetryBase = config.Duration{Duration: math.MaxInt64}
	cfg.RetryMax = cfg.RetryBase
	c := NewCoordinator(&stubStore{}, cfg, metrics.NewRegistry())
	for i := 1; i < 100; i++ {
		if delay := c.backoff(i); delay < 0 {
			t.Fatalf("duration overflow: %v", delay)
		}
	}
	c.cfg.RetryBase = config.Duration{}
	if got := c.backoff(2048); got != 0 {
		t.Fatalf("zero backoff overflowed at large attempt count: %v", got)
	}
}
