package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/ffmpeg"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/metrics"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func TestUnrecognizedLeaseRecordsAreNeverOverwritten(t *testing.T) {
	for _, body := range []string{"{}", "null", `{"schema":2,"status":"held"}`, `{"schema":1,"camera_key":"c1/cam1"}`} {
		t.Run(body, func(t *testing.T) {
			store := &fakeStore{}
			w := newUnownedWorker(t, store, "owner")
			key := w.objectKey(LeaseObjectName)
			if _, err := store.Put(context.Background(), storage.Object{Key: key, Body: []byte(body)}); err != nil {
				t.Fatal(err)
			}
			before := len(store.keys())
			if err := w.claim(context.Background()); !errors.Is(err, ErrLeaseInvalid) {
				t.Fatalf("unrecognized lease: %v", err)
			}
			if len(store.keys()) != before {
				t.Error("unrecognized lease was overwritten")
			}
		})
	}
}

func TestExpiredOwnerCannotUploadSegments(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("video")},
	})
	w.lease.now = func() time.Time { return time.Now().Add(w.cfg.Lease.TTL.Duration) }
	before := len(store.keys())
	if _, err := w.drain(context.Background(), newDrainState()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired owner must stop: %v", err)
	}
	if got := store.keysSince(before); len(got) != 0 {
		t.Fatalf("expired owner wrote objects: %v", got)
	}
}

func TestLostSegmentsAreCountedOnceAcrossUploadRetries(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	gen := newDrainState()
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("first")},
	})
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000003.ts", duration: 4 * time.Second, pdt: basePDT.Add(12 * time.Second), body: []byte("next")},
	})
	armFailure(store, func(key string) error {
		if strings.HasSuffix(key, ".ts") {
			return errors.New("store unavailable")
		}
		return nil
	})
	for i := 0; i < 2; i++ {
		if _, err := w.drain(context.Background(), gen); err == nil {
			t.Fatal("expected upload failure")
		}
	}
	if got := w.Snapshot().SegmentsLost; got != 2 {
		t.Fatalf("same missing segments counted repeatedly: %d, want 2", got)
	}
	armFailure(store, nil)
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	pub, err := hls.ParsePublished([]byte(store.lastManifest()))
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Window[len(pub.Window)-1].Discontinuity {
		t.Error("playback needs a discontinuity after missing media")
	}
}

func TestInitialSpoolLossIsCounted(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000003.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("video")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	if got := w.Snapshot().SegmentsLost; got != 3 {
		t.Fatalf("segments deleted before the first scan must be counted: %d", got)
	}
}

func TestManifestOutageMakesAReceivingChannelUnhealthy(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	armFailure(store, func(key string) error {
		if strings.HasSuffix(key, ".m3u8") {
			return errors.New("manifest unavailable")
		}
		return nil
	})
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("video")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err == nil {
		t.Fatal("expected manifest failure")
	}
	w.setState(StateReceiving)
	if w.Snapshot().Healthy {
		t.Error("stored segments with no published manifest are not playable")
	}
	if w.mManifestLag.Value() <= 0 {
		t.Error("lag must increase even if the first manifest never succeeds")
	}
	w.mu.Lock()
	w.lastManifestAt = time.Now().Add(-w.gapAllowance() - time.Second)
	w.mu.Unlock()
	if w.Snapshot().Healthy {
		t.Error("a stale manifest must make fresh segment uploads unhealthy")
	}
}

func TestFallbackTimestampBookkeepingIsBounded(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	gen := newDrainState()
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("seg-%06d.ts", i)
		if err := os.WriteFile(filepath.Join(w.spoolDir, name), []byte("video"), 0o600); err != nil {
			t.Fatal(err)
		}
		body := "#EXTM3U\n#EXTINF:4.000,\n" + name + "\n"
		if err := os.WriteFile(filepath.Join(w.spoolDir, ffmpeg.LocalPlaylistName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := w.drain(context.Background(), gen); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(gen.fallbackPDT); got > 4*w.cfg.Segment.LocalListSize {
		t.Fatalf("fallback timestamps grow with stream lifetime: %d", got)
	}
}

func TestManifestRecoversWhenTheFirstReadbackAlsoFails(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	gen := newDrainState()
	var fired bool
	store.ambiguous = func(key string) bool {
		if strings.HasSuffix(key, ".m3u8") && !fired {
			fired = true
			store.getErr = errors.New("readback unavailable")
			return true
		}
		return false
	}
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("first")},
	})
	if _, err := w.drain(context.Background(), gen); err == nil {
		t.Fatal("write and readback failure should surface")
	}
	armGetErr(store, nil)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000001.ts", duration: 4 * time.Second, pdt: basePDT.Add(4 * time.Second), body: []byte("second")},
	})
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatalf("our own uncertain write must not fence us out: %v", err)
	}
	pub, err := hls.ParsePublished([]byte(store.lastManifest()))
	if err != nil || pub.MaxSequence != 2 || w.manifestDirty {
		t.Fatalf("recovery must publish the latest window, not just the earlier write: %+v, %v", pub, err)
	}
}

func TestLeaseRecoversWhenTheFirstReadbackAlsoFails(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	var fired bool
	store.ambiguous = func(key string) bool {
		if strings.HasSuffix(key, LeaseObjectName) && !fired {
			fired = true
			store.getErr = errors.New("readback unavailable")
			return true
		}
		return false
	}
	if err := w.lease.Renew(context.Background()); err == nil {
		t.Fatal("write and readback failure should surface")
	}
	armGetErr(store, nil)
	if err := w.lease.Renew(context.Background()); err != nil {
		t.Fatalf("a landed renewal must remain recognizable on the next attempt: %v", err)
	}
	body, _, err := store.Get(context.Background(), w.objectKey(LeaseObjectName))
	if err != nil {
		t.Fatal(err)
	}
	var rec leaseRecord
	if err := json.Unmarshal(body, &rec); err != nil || rec.OwnerSession != w.session {
		t.Fatalf("wrong owner after recovery: %+v, %v", rec, err)
	}
}

func TestWorkerRegistrationCanOverlapMetricScrapes(t *testing.T) {
	reg := metrics.NewRegistry()
	cfg := testConfig(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = reg.Render()
			}
		}
	}()
	defer func() { close(done); wg.Wait() }()
	for i := 0; i < 30; i++ {
		NewWorker(camera.Camera{CenterID: "c", CameraID: fmt.Sprint(i), RTSPURL: "rtsp://host/live"},
			cfg, &fakeStore{}, reg, log, "session")
	}
}

func TestUncertainManifestCannotAdoptAnotherOwnersFence(t *testing.T) {
	store := &fakeStore{}
	stale := newTestWorker(t, store)
	gen := newDrainState()
	var fired bool
	store.ambiguous = func(key string) bool {
		if strings.HasSuffix(key, ".m3u8") && !fired {
			fired = true
			store.getErr = errors.New("readback unavailable")
			return true
		}
		return false
	}
	writeSpool(t, stale.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("first")},
	})
	if _, err := stale.drain(context.Background(), gen); err == nil {
		t.Fatal("expected failed confirmation")
	}
	armGetErr(store, nil)
	fresh := newUnownedWorker(t, store, "successor")
	fresh.lease.now = func() time.Time {
		return time.Now().Add(stale.cfg.Lease.TTL.Duration + 3*stale.cfg.Lease.MaxClockSkew.Duration)
	}
	if err := fresh.claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := store.lastManifest()
	stale.manifestRetryAfter = time.Time{}
	if _, err := stale.drain(context.Background(), gen); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("uncertain stale writer did not recognize takeover: %v", err)
	}
	if store.lastManifest() != before {
		t.Fatal("stale writer modified its successor's manifest")
	}
}

type missingETagStore struct{ *fakeStore }

func (s missingETagStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	body, _, err := s.fakeStore.Get(ctx, key)
	return body, "", err
}

func TestLeaseNeverFallsBackToAnUnconditionalWrite(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)
	first.releaseLease()
	second := newUnownedWorker(t, missingETagStore{store}, "successor")
	before := len(store.keys())
	if err := second.claim(context.Background()); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("missing ETag must stop ownership acquisition: %v", err)
	}
	if len(store.keys()) != before {
		t.Fatal("missing compare-and-swap token caused a write")
	}
}
