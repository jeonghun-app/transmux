package recording

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// TestRunPrunesWhileAFullPassIsStuck proves pruning does not wait for a full
// pass: with every HEAD blocked, Run still drops expired days on its own
// schedule, and cancellation still stops the blocked pass.
func TestRunPrunesWhileAFullPassIsStuck(t *testing.T) {
	index, fs := scannerFixture(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	// One unindexed object today keeps the pass waiting on its HEAD.
	recordingFixture(t, fs, 1, today.Add(time.Minute), 4*time.Second)
	var expired []Segment
	for n := range 150 {
		expired = append(expired, recordingFixture(t, fs, uint64(100+n), today.AddDate(0, 0, -20-n).Add(time.Hour), 4*time.Second))
	}
	kept := recordingFixture(t, fs, 2, today.AddDate(0, 0, -1).Add(time.Hour), 4*time.Second)
	if err := index.Put(append([]Segment{kept}, expired...)); err != nil {
		t.Fatal(err)
	}
	store := &slowStore{FilesystemStore: fs, delay: time.Hour}
	cam := config.StaticCamera{CenterID: "c1", CameraID: "cam1"}
	scanner := &Scanner{Index: index, Store: store, Prefix: "archive", Days: 3, Workers: 2,
		Interval: 20 * time.Millisecond, ScanTimeout: time.Hour,
		Roster: func(context.Context) ([]config.StaticCamera, error) { return []config.StaticCamera{cam}, nil },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { scanner.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop after cancellation")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := 0
		for _, s := range expired {
			if _, err := index.Get("c1", "cam1", s.URI); err == nil {
				left++
			} else if !errors.Is(err, storage.ErrNotFound) {
				t.Fatal(err)
			}
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d expired days left while the pass was stuck (scanning=%v)", left, scanner.Status().Scanning)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !scanner.Status().Scanning || !scanner.Status().LastPass.IsZero() {
		t.Fatalf("pass was expected to still be blocked: %+v", scanner.Status())
	}
	if _, err := index.Get("c1", "cam1", kept.URI); err != nil {
		t.Fatalf("day inside the window pruned: %v", err)
	}
}
