package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/ffmpeg"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/metrics"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// fakeStore records the order of every Put and can be made to fail.
type fakeStore struct {
	mu      sync.Mutex
	puts    []storage.Object
	objects map[string][]byte
	failKey func(key string) error
	// getErr, when set, is returned by Get instead of consulting objects.
	getErr error
}

func (f *fakeStore) Put(_ context.Context, obj storage.Object) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failKey != nil {
		if err := f.failKey(obj.Key); err != nil {
			return err
		}
	}
	cp := obj
	cp.Body = append([]byte(nil), obj.Body...)
	f.puts = append(f.puts, cp)
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[obj.Key] = cp.Body
	return nil
}

func (f *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return body, nil
}

func (f *fakeStore) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.puts))
	for i, p := range f.puts {
		out[i] = p.Key
	}
	return out
}

func (f *fakeStore) lastManifest() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.puts) - 1; i >= 0; i-- {
		if strings.HasSuffix(f.puts[i].Key, ".m3u8") {
			return string(f.puts[i].Body)
		}
	}
	return ""
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.SpoolDir = filepath.Join(t.TempDir(), "spool")
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	cfg.Storage.Backend = "filesystem"
	cfg.Storage.Root = t.TempDir()
	cfg.Storage.Bucket = "test"
	cfg.Segment.LiveWindow = 3
	cfg.Segment.LocalListSize = 6
	cfg.Cameras.Static = []config.StaticCamera{{CenterID: "c1", CameraID: "cam1", RTSPURL: "rtsp://h/s"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

func newTestWorker(t *testing.T, store ObjectClient) *Worker {
	t.Helper()
	cfg := testConfig(t)
	cam := camera.Camera{CenterID: "c1", CameraID: "cam1", RTSPURL: "rtsp://user:pass@host/stream"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWorker(cam, cfg, store, metrics.NewRegistry(), log)
	if err := os.MkdirAll(w.spoolDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return w
}

// writeSpool lays down a playlist and its segment files the way ffmpeg would.
func writeSpool(t *testing.T, dir string, entries []spoolEntry) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for _, e := range entries {
		if e.discontinuity {
			sb.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&sb, "#EXTINF:%.6f,\n", e.duration.Seconds())
		fmt.Fprintf(&sb, "#EXT-X-PROGRAM-DATE-TIME:%s\n", e.pdt.UTC().Format("2006-01-02T15:04:05.000-0700"))
		sb.WriteString(e.name + "\n")
		if !e.absent {
			if err := os.WriteFile(filepath.Join(dir, e.name), e.body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ffmpeg.LocalPlaylistName), []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

type spoolEntry struct {
	name          string
	duration      time.Duration
	pdt           time.Time
	body          []byte
	absent        bool
	discontinuity bool
}

var basePDT = time.Date(2026, 8, 31, 23, 59, 55, 0, time.UTC)

func TestDrainUploadsSegmentsBeforeManifest(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)

	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("aaa")},
		{name: "seg-000001.ts", duration: 5 * time.Second, pdt: basePDT.Add(5 * time.Second), body: []byte("bbbb")},
	})

	gen := newDrainState()
	res, err := w.drain(context.Background(), gen)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Uploaded != 2 {
		t.Fatalf("uploaded %d segments, want 2", res.Uploaded)
	}

	keys := store.keys()
	if len(keys) != 3 {
		t.Fatalf("want 2 segments + 1 manifest, got %v", keys)
	}
	// The manifest must be last. A manifest published before its segments
	// would point players at objects that do not exist yet.
	if !strings.HasSuffix(keys[2], "index.m3u8") {
		t.Errorf("manifest must be the final PUT, got order %v", keys)
	}
	for _, k := range keys[:2] {
		if !strings.HasSuffix(k, ".ts") {
			t.Errorf("expected segment PUTs first, got %v", keys)
		}
	}

	// The date directory must come from the segment's own wall clock, which
	// is what keeps a segment captured at 23:59:55 out of the next day.
	if !strings.Contains(keys[0], "c1/cam1/2026/08/31/") {
		t.Errorf("segment key %q does not use the captured date path", keys[0])
	}

	manifest := store.lastManifest()
	for _, k := range keys[:2] {
		rel := k[strings.Index(k, "2026/"):]
		if !strings.Contains(manifest, rel) {
			t.Errorf("manifest does not reference uploaded segment %q\n%s", rel, manifest)
		}
	}
	if !strings.Contains(manifest, "#EXT-X-MEDIA-SEQUENCE:1") {
		t.Errorf("media sequence should start at our first published sequence\n%s", manifest)
	}

	// Local copies are removed once stored, so the spool stays bounded.
	for _, name := range []string{"seg-000000.ts", "seg-000001.ts"} {
		if _, err := os.Stat(filepath.Join(w.spoolDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted after upload", name)
		}
	}
}

func TestDrainDoesNotPublishManifestWhenSegmentUploadFails(t *testing.T) {
	failErr := errors.New("s3 unavailable")
	store := &fakeStore{failKey: func(key string) error {
		if strings.HasSuffix(key, ".ts") {
			return failErr
		}
		return nil
	}}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("aaa")},
	})

	gen := newDrainState()
	res, err := w.drain(context.Background(), gen)
	if res.Uploaded != 0 {
		t.Fatalf("uploaded %d, want 0", res.Uploaded)
	}
	if err == nil {
		t.Fatal("expected the segment failure to be reported")
	}
	if got := store.keys(); len(got) != 0 {
		t.Fatalf("nothing should have been published, got %v", got)
	}
	// The segment must stay on the spool so the next tick can retry it.
	if _, statErr := os.Stat(filepath.Join(w.spoolDir, "seg-000000.ts")); statErr != nil {
		t.Errorf("failed segment must remain on the spool for retry: %v", statErr)
	}
	// And it must not consume a sequence number.
	if w.lastSequence != 0 {
		t.Errorf("lastSequence = %d, want 0 after a failed upload", w.lastSequence)
	}
}

func TestDrainStopsAtFirstFailureToPreserveOrder(t *testing.T) {
	store := &fakeStore{failKey: func(key string) error {
		if strings.Contains(key, "seg-000000002") {
			return errors.New("transient")
		}
		return nil
	}}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("a")},
		{name: "seg-000001.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("b")},
		{name: "seg-000002.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("c")},
	})

	gen := newDrainState()
	res, _ := w.drain(context.Background(), gen)
	// Segment 2 fails, so segment 3 must not be published ahead of it: HLS
	// requires the playlist to be gapless and in order.
	if res.Uploaded != 1 {
		t.Fatalf("uploaded %d, want 1 (stop at the first failure)", res.Uploaded)
	}
	manifest := store.lastManifest()
	if strings.Count(manifest, "#EXTINF") != 1 {
		t.Errorf("manifest should list exactly the one published segment\n%s", manifest)
	}
}

func TestDrainDetectsReclaimedSegments(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	// ffmpeg listed index 0, then 3: indexes 1 and 2 were deleted before we
	// read them. That is real loss and has to be counted, not skipped.
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("a")},
		{name: "seg-000003.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("d")},
	})
	gen := newDrainState()
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := w.Snapshot().SegmentsLost; got != 2 {
		t.Errorf("SegmentsLost = %d, want 2", got)
	}
}

func TestDrainCountsPlaylistEntryWithMissingFileAsLost(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, absent: true},
		{name: "seg-000001.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("b")},
	})
	gen := newDrainState()
	res, err := w.drain(context.Background(), gen)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("uploaded %d, want 1", res.Uploaded)
	}
	if got := w.Snapshot().SegmentsLost; got != 1 {
		t.Errorf("SegmentsLost = %d, want 1", got)
	}
}

func TestDrainIsIdempotentAcrossTicks(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	entries := []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("a")},
	}
	writeSpool(t, w.spoolDir, entries)

	gen := newDrainState()
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	// The playlist still lists the segment on the next scan. It must not be
	// uploaded again or consume a second sequence number.
	res, err := w.drain(context.Background(), gen)
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 0 {
		t.Fatalf("second drain uploaded %d, want 0", res.Uploaded)
	}
	if w.lastSequence != 1 {
		t.Errorf("lastSequence = %d, want 1", w.lastSequence)
	}
}

func TestSequenceIsMonotonicAcrossGenerations(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)

	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("a")},
		{name: "seg-000001.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("b")},
	})
	gen1 := newDrainState()
	if _, err := w.drain(context.Background(), gen1); err != nil {
		t.Fatal(err)
	}

	// Simulate a reconnect: ffmpeg restarts and its local numbering resets
	// to zero. Our published sequence must keep climbing.
	if err := ffmpeg.EnsureSpool(w.spoolDir); err != nil {
		t.Fatal(err)
	}
	w.pendingDiscontinuity = true
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT.Add(time.Minute), body: []byte("c")},
	})
	gen2 := newDrainState()
	if _, err := w.drain(context.Background(), gen2); err != nil {
		t.Fatal(err)
	}

	if w.lastSequence != 3 {
		t.Fatalf("lastSequence = %d, want 3", w.lastSequence)
	}
	// No object key may be written twice: an overwrite would corrupt a
	// segment a player may already be fetching.
	seen := map[string]int{}
	for _, k := range store.keys() {
		if strings.HasSuffix(k, ".ts") {
			seen[k]++
		}
	}
	for k, c := range seen {
		if c > 1 {
			t.Errorf("segment key %q written %d times", k, c)
		}
	}
	if len(seen) != 3 {
		t.Errorf("want 3 distinct segment keys, got %d: %v", len(seen), seen)
	}

	manifest := store.lastManifest()
	if !strings.Contains(manifest, "#EXT-X-DISCONTINUITY") {
		t.Errorf("the first segment after a reconnect must be marked discontinuous\n%s", manifest)
	}
}

func TestLiveWindowIsBoundedAndTracksDiscontinuitySequence(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store) // LiveWindow = 3

	// First segment carries a discontinuity; after five more it has scrolled
	// out of the window and must be reflected in EXT-X-DISCONTINUITY-SEQUENCE.
	w.pendingDiscontinuity = true
	entries := make([]spoolEntry, 0, 6)
	for i := 0; i < 6; i++ {
		entries = append(entries, spoolEntry{
			name:     fmt.Sprintf("seg-%06d.ts", i),
			duration: 5 * time.Second,
			pdt:      basePDT.Add(time.Duration(i) * 5 * time.Second),
			body:     []byte{byte('a' + i)},
		})
	}
	writeSpool(t, w.spoolDir, entries)
	gen := newDrainState()
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatal(err)
	}

	if len(w.window) != 3 {
		t.Fatalf("window has %d entries, want 3", len(w.window))
	}
	if w.discontinuitySequence != 1 {
		t.Errorf("discontinuitySequence = %d, want 1", w.discontinuitySequence)
	}
	manifest := store.lastManifest()
	if strings.Count(manifest, "#EXTINF") != 3 {
		t.Errorf("manifest should list only the live window\n%s", manifest)
	}
	if !strings.Contains(manifest, "#EXT-X-DISCONTINUITY-SEQUENCE:1") {
		t.Errorf("missing discontinuity sequence\n%s", manifest)
	}
	if !strings.Contains(manifest, "#EXT-X-MEDIA-SEQUENCE:4") {
		t.Errorf("media sequence should be the first live segment\n%s", manifest)
	}
}

func TestCheckpointRestoresSequence(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	gen := newDrainState()
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatal(err)
	}

	// A fresh worker for the same camera, as after a container restart.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w2 := NewWorker(w.cam, w.cfg, store, metrics.NewRegistry(), log)
	if w2.lastSequence != w.lastSequence {
		t.Errorf("restarted worker lastSequence = %d, want %d", w2.lastSequence, w.lastSequence)
	}
	if len(w2.window) != len(w.window) {
		t.Errorf("restarted worker window = %d entries, want %d", len(w2.window), len(w.window))
	}
}

func TestSnapshotNeverLeaksCredentials(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	snap := w.Snapshot()
	if strings.Contains(snap.SourceURL, "pass") {
		t.Fatalf("snapshot leaked credentials: %q", snap.SourceURL)
	}
	if !strings.Contains(snap.SourceURL, "***") {
		t.Errorf("expected redacted userinfo, got %q", snap.SourceURL)
	}
}

func TestGapAllowanceExceedsLongGOPSegments(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	// A camera whose IDR interval is longer than the target duration must not
	// trip the missing-segment alarm just for being slow.
	if w.gapAllowance() <= w.cfg.Segment.TargetDuration.Duration {
		t.Fatal("gap allowance must exceed the target segment duration")
	}
}

func TestBackoffGrowsAndResets(t *testing.T) {
	cfg := config.Default().Reconnect
	b := newBackoff(cfg, 1)
	var prevMax time.Duration
	for i := 0; i < 6; i++ {
		d := b.Next()
		if d < cfg.Base.Duration/2 {
			t.Fatalf("attempt %d delay %v below the floor", i, d)
		}
		if d > cfg.Max.Duration {
			t.Fatalf("attempt %d delay %v above the cap %v", i, d, cfg.Max.Duration)
		}
		prevMax = d
	}
	_ = prevMax
	if b.Attempt() != 6 {
		t.Errorf("Attempt = %d, want 6", b.Attempt())
	}
	b.Reset()
	if b.Attempt() != 0 {
		t.Errorf("Attempt after Reset = %d, want 0", b.Attempt())
	}
}

func TestObjectKeyHonoursPrefix(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	w.cfg.Storage.KeyPrefix = "/live/"
	got := w.objectKey("2026/08/31/seg-1.ts")
	if got != "live/c1/cam1/2026/08/31/seg-1.ts" {
		t.Errorf("objectKey = %q", got)
	}
}

func TestParseLocalIndex(t *testing.T) {
	cases := map[string]int{
		"seg-000000.ts": 0,
		"seg-000123.ts": 123,
	}
	for name, want := range cases {
		got, ok := parseLocalIndex(name)
		if !ok || got != want {
			t.Errorf("parseLocalIndex(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}
	if _, ok := parseLocalIndex("garbage.ts"); ok {
		t.Error("expected failure on a name with no index")
	}
}

// TestDrainReportsSeenSeparatelyFromUploaded covers the distinction that
// keeps an object-store outage from being mistaken for a camera fault.
//
// While the store is down, ffmpeg is still producing segments. The stall
// watchdog is driven by Seen, so it must keep advancing; if it were driven by
// Uploaded, the watchdog would kill and relaunch ffmpeg, which cannot fix the
// store and would discard everything spooled so far.
func TestDrainReportsSeenSeparatelyFromUploaded(t *testing.T) {
	store := &fakeStore{failKey: func(string) error { return errors.New("store down") }}
	w := newTestWorker(t, store)
	gen := newDrainState()

	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	res, _ := w.drain(context.Background(), gen)
	if res.Seen != 1 {
		t.Errorf("Seen = %d, want 1: ffmpeg did produce a segment", res.Seen)
	}
	if res.Uploaded != 0 {
		t.Errorf("Uploaded = %d, want 0: the store is failing", res.Uploaded)
	}

	// A new segment appears while the store is still down.
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("a")},
		{name: "seg-000001.ts", duration: 5 * time.Second, pdt: basePDT, body: []byte("b")},
	})
	res, _ = w.drain(context.Background(), gen)
	if res.Seen != 1 {
		t.Errorf("Seen = %d, want 1 for the one newly appeared segment", res.Seen)
	}

	// Re-scanning without any new segment must report Seen == 0, otherwise a
	// dead camera would keep the watchdog alive forever.
	res, _ = w.drain(context.Background(), gen)
	if res.Seen != 0 {
		t.Errorf("Seen = %d on a rescan with no new segment, want 0", res.Seen)
	}

	// Recovery: the spooled segments upload in order and the manifest follows.
	store.mu.Lock()
	store.failKey = nil
	store.mu.Unlock()
	res, err := w.drain(context.Background(), gen)
	if err != nil {
		t.Fatalf("drain after recovery: %v", err)
	}
	if res.Uploaded != 2 {
		t.Fatalf("Uploaded = %d after recovery, want the 2 spooled segments", res.Uploaded)
	}
	if got := w.Snapshot().SegmentsLost; got != 0 {
		t.Errorf("SegmentsLost = %d, want 0: the segments were still on the spool", got)
	}
	keys := store.keys()
	if !strings.HasSuffix(keys[len(keys)-1], ".m3u8") {
		t.Errorf("manifest must still be published last, got %v", keys)
	}
}

// TestRecoverFromPublishedManifest is the container-replacement case. The
// local checkpoint is gone, but the manifest in the object store still records
// where the channel got to. Resuming from zero would rewind
// EXT-X-MEDIA-SEQUENCE and overwrite already-cached segment keys.
func TestRecoverFromPublishedManifest(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)

	writeSpool(t, first.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
		{name: "seg-000001.ts", duration: 4 * time.Second, pdt: basePDT.Add(4 * time.Second), body: []byte("b")},
	})
	if _, err := first.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	if first.lastSequence != 2 {
		t.Fatalf("precondition: lastSequence = %d, want 2", first.lastSequence)
	}

	// Simulate a task replacement: same camera and config, but the state
	// directory is empty because it was never persisted.
	cfg := first.cfg
	cfg.StateDir = t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	second := NewWorker(first.cam, cfg, store, metrics.NewRegistry(), log)

	if !second.needsRecovery {
		t.Fatal("a worker with no checkpoint must ask for recovery")
	}
	if err := second.recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if second.lastSequence != 2 {
		t.Errorf("recovered lastSequence = %d, want 2", second.lastSequence)
	}
	if len(second.window) != 2 {
		t.Errorf("recovered window = %d entries, want 2", len(second.window))
	}
	if !second.pendingDiscontinuity {
		t.Error("the first segment after a restart must be marked discontinuous")
	}

	// Publishing again must move forward, never reuse a key.
	if err := ffmpeg.EnsureSpool(second.spoolDir); err != nil {
		t.Fatal(err)
	}
	writeSpool(t, second.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT.Add(time.Minute), body: []byte("c")},
	})
	if _, err := second.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	if second.lastSequence != 3 {
		t.Errorf("lastSequence after restart = %d, want 3", second.lastSequence)
	}
	seen := map[string]int{}
	for _, k := range store.keys() {
		if strings.HasSuffix(k, ".ts") {
			seen[k]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("segment key %q written %d times across the restart", k, n)
		}
	}
	if len(seen) != 3 {
		t.Errorf("want 3 distinct segment keys, got %d", len(seen))
	}
}

func TestRecoverTreatsMissingManifestAsNewChannel(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	if !w.needsRecovery {
		t.Fatal("expected recovery to be requested")
	}
	if err := w.recover(context.Background()); err != nil {
		t.Fatalf("a missing manifest is a normal new channel, not an error: %v", err)
	}
	if w.lastSequence != 0 {
		t.Errorf("lastSequence = %d, want 0 for a new channel", w.lastSequence)
	}
	if w.pendingDiscontinuity {
		t.Error("a brand new channel needs no discontinuity")
	}
}

// TestRecoverFailsClosedOnReadError is the safety property. If the object
// store cannot be read, the channel's history is unknown. Starting at zero
// could overwrite segments that viewers are watching, so the correct
// behaviour is to refuse rather than guess.
func TestRecoverFailsClosedOnReadError(t *testing.T) {
	store := &fakeStore{getErr: errors.New("s3 unreachable")}
	w := newTestWorker(t, store)
	err := w.recover(context.Background())
	if err == nil {
		t.Fatal("expected recovery to fail rather than assume sequence 0")
	}
	if w.lastSequence != 0 || !w.needsRecovery {
		t.Error("a failed recovery must leave the worker unpublishable")
	}
}

func TestRunFailsClosedWhenRecoveryFails(t *testing.T) {
	store := &fakeStore{getErr: errors.New("s3 unreachable")}
	w := newTestWorker(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	// The worker must reach StateFailed and must not launch ffmpeg.
	deadline := time.After(3 * time.Second)
	for {
		if w.Snapshot().State == StateFailed {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("state = %q, want failed", w.Snapshot().State)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := w.Snapshot().PID; got != 0 {
		t.Errorf("ffmpeg must not start when the sequence is unknown, pid = %d", got)
	}
	if w.Snapshot().Healthy {
		t.Error("a failed channel must not report healthy")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestSegmentCarriesIndexingMetadata guards the data a recording index will
// need. Duration in particular cannot be derived from the object key, and
// inferring it from the next segment's timestamp is wrong across a camera
// disconnect, so it has to be written at upload time.
func TestSegmentCarriesIndexingMetadata(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 6500 * time.Millisecond, pdt: basePDT, body: []byte("abc")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	var seg *storage.Object
	for i := range store.puts {
		if strings.HasSuffix(store.puts[i].Key, ".ts") {
			seg = &store.puts[i]
			break
		}
	}
	if seg == nil {
		t.Fatal("no segment was uploaded")
	}
	want := map[string]string{
		"sequence":      "1",
		"duration-ms":   "6500",
		"discontinuity": "false",
		"center-id":     "c1",
		"camera-id":     "cam1",
		"pdt-ms":        strconv.FormatInt(basePDT.UnixMilli(), 10),
	}
	for k, v := range want {
		if got := seg.Metadata[k]; got != v {
			t.Errorf("metadata[%q] = %q, want %q", k, got, v)
		}
	}
	// The manifest is mutable and needs no indexing metadata.
	for _, p := range store.puts {
		if strings.HasSuffix(p.Key, ".m3u8") && len(p.Metadata) != 0 {
			t.Errorf("manifest should carry no user metadata, got %v", storage.MetadataKeys(p.Metadata))
		}
	}
}

// verify the fake satisfies the interface the worker depends on
var _ ObjectClient = (*fakeStore)(nil)
var _ = hls.ContentTypeSegment
