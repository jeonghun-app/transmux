package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// fakeStore records the order of every Put and emulates the object store's
// conditional-write semantics.
//
// The preconditions are enforced for real rather than accepted and ignored:
// the ownership protocol is built entirely on them, so a fake that waved them
// through would make every fencing test pass for the wrong reason.
type fakeStore struct {
	mu      sync.Mutex
	puts    []storage.Object
	objects map[string]fakeObject
	failKey func(key string) error
	// getErr, when set, is returned by Get instead of consulting objects.
	getErr error
	// ambiguous, when it returns true, stores the object and then reports an
	// error anyway: a write that landed but whose response was lost.
	ambiguous func(key string) bool
}

type fakeObject struct {
	body     []byte
	etag     string
	metadata map[string]string
}

func fakeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (f *fakeStore) Put(_ context.Context, obj storage.Object) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putLocked(obj)
}

func (f *fakeStore) putLocked(obj storage.Object) (string, error) {
	if f.failKey != nil {
		if err := f.failKey(obj.Key); err != nil {
			return "", err
		}
	}
	if f.objects == nil {
		f.objects = map[string]fakeObject{}
	}
	current, exists := f.objects[obj.Key]
	switch {
	case obj.Preconditions.IfNoneMatch && exists:
		return "", fmt.Errorf("%w: %s exists", storage.ErrPreconditionFailed, obj.Key)
	case obj.Preconditions.IfMatch != "":
		if !exists {
			return "", fmt.Errorf("%w: %s absent", storage.ErrPreconditionFailed, obj.Key)
		}
		if current.etag != obj.Preconditions.IfMatch {
			return "", fmt.Errorf("%w: %s etag %s want %s",
				storage.ErrPreconditionFailed, obj.Key, current.etag, obj.Preconditions.IfMatch)
		}
	}
	cp := obj
	cp.Body = append([]byte(nil), obj.Body...)
	f.puts = append(f.puts, cp)
	etag := fakeETag(cp.Body)
	f.objects[obj.Key] = fakeObject{body: cp.Body, etag: etag, metadata: obj.Metadata}
	if f.ambiguous != nil && f.ambiguous(obj.Key) {
		return "", errors.New("connection reset after the write landed")
	}
	return etag, nil
}

// PutFile mirrors the coordinator: the file is read as part of the upload, so
// a vanished segment surfaces as fs.ErrNotExist rather than a store error.
func (f *fakeStore) PutFile(_ context.Context, obj storage.Object, srcPath string) (int64, string, error) {
	body, err := os.ReadFile(srcPath)
	if err != nil {
		return 0, "", err
	}
	obj.Body = body
	f.mu.Lock()
	defer f.mu.Unlock()
	etag, err := f.putLocked(obj)
	if err != nil {
		return 0, "", err
	}
	return int64(len(body)), etag, nil
}

func (f *fakeStore) Get(_ context.Context, key string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, "", f.getErr
	}
	o, ok := f.objects[key]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return o.body, o.etag, nil
}

func (f *fakeStore) Head(_ context.Context, key string) (storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return storage.ObjectInfo{}, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return storage.ObjectInfo{ETag: o.etag, Size: int64(len(o.body)), Metadata: o.metadata}, nil
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

// keysSince returns the keys written after the first n puts, so a test can
// assert on the writes one operation made without counting the ownership
// fence write that every worker performs before it starts.
func (f *fakeStore) keysSince(n int) []string {
	all := f.keys()
	if n > len(all) {
		return nil
	}
	return all[n:]
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
	w := NewWorker(cam, cfg, store, metrics.NewRegistry(), log, "test-session")
	if err := os.MkdirAll(w.spoolDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Take ownership exactly as Run does: acquire the lease, recover, and
	// fence the manifest. Publishing without this is refused, which is the
	// point of the protocol, so the tests must go through it too.
	if err := w.claim(context.Background()); err != nil {
		t.Fatalf("claim ownership: %v", err)
	}
	return w
}

// armFailure makes the store start failing after setup. Ownership acquisition
// needs a working store, so a test that wants a broken store during
// publication must break it only once the worker owns its camera.
func armFailure(store *fakeStore, fn func(string) error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.failKey = fn
}

func armGetErr(store *fakeStore, err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.getErr = err
}

// releaseOwnership models the clean handover a task performs on SIGTERM, so a
// successor does not have to wait out the lease TTL.
func releaseOwnership(t *testing.T, w *Worker) {
	t.Helper()
	if err := w.lease.Release(context.Background()); err != nil {
		t.Fatalf("release lease: %v", err)
	}
}

// newUnownedWorker builds a worker that has not taken ownership yet, for tests
// that exercise the claim path itself.
func newUnownedWorker(t *testing.T, store ObjectClient, session string) *Worker {
	t.Helper()
	cfg := testConfig(t)
	cam := camera.Camera{CenterID: "c1", CameraID: "cam1", RTSPURL: "rtsp://user:pass@host/stream"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWorker(cam, cfg, store, metrics.NewRegistry(), log, session)
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
	// The ownership fence write happens before any drain; assert on what this
	// drain itself wrote.
	before := len(store.keys())
	res, err := w.drain(context.Background(), gen)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Uploaded != 2 {
		t.Fatalf("uploaded %d segments, want 2", res.Uploaded)
	}

	keys := store.keysSince(before)
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
	before := len(store.keys())
	res, err := w.drain(context.Background(), gen)
	if res.Uploaded != 0 {
		t.Fatalf("uploaded %d, want 0", res.Uploaded)
	}
	if err == nil {
		t.Fatal("expected the segment failure to be reported")
	}
	if got := store.keysSince(before); len(got) != 0 {
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
	w2 := NewWorker(w.cam, w.cfg, store, metrics.NewRegistry(), log, "test-session-2")
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
	store := &fakeStore{}
	w := newTestWorker(t, store)
	armFailure(store, func(string) error { return errors.New("store down") })
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
	releaseOwnership(t, first)
	second := NewWorker(first.cam, cfg, store, metrics.NewRegistry(), log, "test-session-2")

	if !second.needsRecovery {
		t.Fatal("a worker with no checkpoint must ask for recovery")
	}
	if err := second.claim(context.Background()); err != nil {
		t.Fatalf("claim: %v", err)
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
	w := newUnownedWorker(t, store, "test-session")
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
func TestRecoverRetriesWhenTheStoreIsUnreadable(t *testing.T) {
	store := &fakeStore{getErr: errors.New("s3 unreachable")}
	w := newUnownedWorker(t, store, "test-session")
	err := w.recover(context.Background())
	if err == nil {
		t.Fatal("expected recovery to fail rather than assume sequence 0")
	}
	// Retryable, not terminal: nothing has been published, so a transient
	// outage at startup must not permanently disable the channel.
	if !errors.Is(err, ErrLeaseUnavailable) {
		t.Errorf("err = %v, want a retryable ownership error", err)
	}
	if w.lastSequence != 0 || !w.needsRecovery {
		t.Error("a failed recovery must leave the worker unpublishable")
	}
}

func TestRunWaitsWhenOwnershipCannotBeDetermined(t *testing.T) {
	store := &fakeStore{getErr: errors.New("s3 unreachable")}
	w := newUnownedWorker(t, store, "test-session")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	// The worker must refuse to launch ffmpeg. It waits rather than failing,
	// because an unreadable store is a transient condition and nothing has
	// been published: 75 channels must not need an operator after a blip.
	deadline := time.After(3 * time.Second)
	for {
		if w.Snapshot().State == StateWaitingOwnership {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("state = %q, want waiting_ownership", w.Snapshot().State)
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

// restartWorker builds a fresh worker for the same camera, config and state
// directory, which is what a daemon restart produces.
func restartWorker(t *testing.T, prev *Worker, store ObjectClient) *Worker {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWorker(prev.cam, prev.cfg, store, metrics.NewRegistry(), log, "test-session-2")
}

// TestStaleCheckpointDoesNotRewindSequence is the crash window between the
// two writes that publish a segment: the manifest PUT succeeds and the
// checkpoint write does not. The checkpoint is then one publish behind the
// durable record, so trusting it would rewind EXT-X-MEDIA-SEQUENCE and reuse
// object keys that are already cached.
func TestStaleCheckpointDoesNotRewindSequence(t *testing.T) {
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

	// Rewind only the checkpoint, leaving the published manifest at 2.
	if err := saveCheckpoint(first.statePath, checkpoint{LastSequence: 1}); err != nil {
		t.Fatal(err)
	}

	releaseOwnership(t, first)
	second := restartWorker(t, first, store)
	if second.checkpointFloor != 1 {
		t.Fatalf("checkpointFloor = %d, want 1", second.checkpointFloor)
	}
	if !second.needsRecovery {
		t.Fatal("the manifest must be consulted even when a checkpoint exists")
	}
	if err := second.claim(context.Background()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if second.lastSequence != 2 {
		t.Errorf("resumed lastSequence = %d, want 2 from the published manifest", second.lastSequence)
	}

	// Publishing again must not reuse a key that sequence 2 already used.
	writeSpool(t, second.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT.Add(time.Minute), body: []byte("c")},
	})
	if _, err := second.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	if second.lastSequence != 3 {
		t.Errorf("lastSequence = %d, want 3", second.lastSequence)
	}
	seen := map[string]int{}
	for _, k := range store.keys() {
		if strings.HasSuffix(k, ".ts") {
			seen[k]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("segment key %q reused %d times across the restart", k, n)
		}
	}
}

// TestCheckpointAheadOfManifestWins covers the opposite skew: a sequence was
// used but no manifest recorded it. The higher of the two sources must win,
// because both are lower bounds on what was published.
func TestCheckpointAheadOfManifestWins(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)
	writeSpool(t, first.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	if _, err := first.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	if err := saveCheckpoint(first.statePath, checkpoint{LastSequence: 9}); err != nil {
		t.Fatal(err)
	}

	releaseOwnership(t, first)
	second := restartWorker(t, first, store)
	if err := second.claim(context.Background()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if second.lastSequence != 9 {
		t.Errorf("lastSequence = %d, want 9 from the checkpoint floor", second.lastSequence)
	}
	// The recovered window is dropped in this case: mixing it with a higher
	// sequence would leave a numbering gap inside one published playlist.
	if len(second.window) != 0 {
		t.Errorf("window = %d entries, want 0 when the checkpoint is ahead", len(second.window))
	}
}

// TestRecoverFailsClosedEvenWithCheckpoint is the fail-closed property. A
// checkpoint is not evidence that the manifest says the same thing, so an
// unreadable object store must still disable the channel.
func TestRecoverFailsClosedEvenWithCheckpoint(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)
	if err := saveCheckpoint(first.statePath, checkpoint{LastSequence: 4}); err != nil {
		t.Fatal(err)
	}

	broken := &fakeStore{getErr: errors.New("s3 unreachable")}
	second := restartWorker(t, first, broken)
	if second.checkpointFloor != 4 {
		t.Fatalf("checkpointFloor = %d, want 4", second.checkpointFloor)
	}
	if err := second.recover(context.Background()); err == nil {
		t.Fatal("a checkpoint must not license publishing while the store is unreadable")
	}
	if !second.needsRecovery {
		t.Error("a failed recovery must leave the worker unpublishable")
	}
}

// TestRecoverFailsClosedOnForeignManifestWithCheckpoint covers the other
// fail-closed trigger: the manifest exists but belongs to another writer.
func TestRecoverFailsClosedOnForeignManifestWithCheckpoint(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	if err := saveCheckpoint(w.statePath, checkpoint{LastSequence: 4}); err != nil {
		t.Fatal(err)
	}
	foreign := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000,\nsomeone-elses-segment.ts\n"
	if _, err := store.Put(context.Background(), storage.Object{
		Key:  w.objectKey(w.cfg.Storage.ManifestName),
		Body: []byte(foreign),
	}); err != nil {
		t.Fatal(err)
	}

	second := restartWorker(t, w, store)
	if err := second.recover(context.Background()); err == nil {
		t.Fatal("a foreign manifest must not be overwritten just because a checkpoint exists")
	}
}

// TestCheckpointPathsDoNotCollide guards the flat-filename bug: identifiers
// may contain underscores, so joining them with one would let two different
// cameras share a checkpoint and overwrite each other's sequence.
func TestCheckpointPathsDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	a := checkpointPath(dir, "a_b", "c")
	b := checkpointPath(dir, "a", "b_c")
	if a == b {
		t.Fatalf("distinct cameras share a checkpoint path: %q", a)
	}
	if err := saveCheckpoint(a, checkpoint{LastSequence: 7}); err != nil {
		t.Fatal(err)
	}
	if err := saveCheckpoint(b, checkpoint{LastSequence: 3}); err != nil {
		t.Fatal(err)
	}
	got, err := loadCheckpoint(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSequence != 7 {
		t.Errorf("checkpoint for a_b/c = %d, want 7: it was overwritten", got.LastSequence)
	}
}

// TestManifestIsRetriedWithoutANewSegment is the stranded-manifest case. The
// segment is stored, deleted from the spool and counted, but the manifest PUT
// that makes it visible fails. If the retry were gated on a new upload, a
// camera that stops right then would leave those segments referenced by
// nothing, forever.
func TestManifestIsRetriedWithoutANewSegment(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	armFailure(store, func(key string) error {
		if strings.HasSuffix(key, ".m3u8") {
			return errors.New("manifest put failed")
		}
		return nil
	})
	gen := newDrainState()
	before := len(store.keys())
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})

	res, err := w.drain(context.Background(), gen)
	if err == nil {
		t.Fatal("expected the manifest failure to be reported")
	}
	if res.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1: the segment itself stored fine", res.Uploaded)
	}
	if !w.manifestDirty {
		t.Fatal("a failed manifest must be remembered as pending")
	}
	if got := store.keysSince(before); len(got) != 1 || !strings.HasSuffix(got[0], ".ts") {
		t.Fatalf("want only the segment stored, got %v", got)
	}

	// The store recovers. No new segment arrives: the playlist is unchanged
	// and the segment file is already gone from the spool.
	store.mu.Lock()
	store.failKey = nil
	store.mu.Unlock()

	// A manifest-only retry is rate limited, so an immediate rescan must not
	// re-issue it.
	quiet := len(store.keys())
	if _, err := w.drain(context.Background(), gen); err != nil {
		t.Fatalf("rate-limited drain should not surface an error: %v", err)
	}
	if got := store.keysSince(quiet); len(got) != 0 {
		t.Errorf("manifest retry should be rate limited within one target duration, got %v", got)
	}

	// Once the limit has elapsed the retry goes through with no new segment.
	w.manifestRetryAfter = time.Time{}
	res, err = w.drain(context.Background(), gen)
	if err != nil {
		t.Fatalf("retry drain: %v", err)
	}
	if res.Uploaded != 0 {
		t.Fatalf("Uploaded = %d, want 0: nothing new was produced", res.Uploaded)
	}
	if w.manifestDirty {
		t.Error("manifest should be clean after a successful retry")
	}
	if m := store.lastManifest(); !strings.Contains(m, "seg-000000001-") {
		t.Errorf("the retried manifest must reference the stored segment\n%s", m)
	}
}

// TestFFmpegDiscontinuityIsPublished covers a discontinuity ffmpeg reports
// inside a single run, which the supervisor knows nothing about.
func TestFFmpegDiscontinuityIsPublished(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
		{name: "seg-000001.ts", duration: 4 * time.Second, pdt: basePDT.Add(4 * time.Second),
			body: []byte("b"), discontinuity: true},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}

	manifest := store.lastManifest()
	if !strings.Contains(manifest, "#EXT-X-DISCONTINUITY\n") {
		t.Errorf("ffmpeg's discontinuity was dropped\n%s", manifest)
	}
	var second *storage.Object
	for i := range store.puts {
		if strings.Contains(store.puts[i].Key, "seg-000000002-") {
			second = &store.puts[i]
		}
	}
	if second == nil {
		t.Fatal("second segment was not uploaded")
	}
	if got := second.Metadata["discontinuity"]; got != "true" {
		t.Errorf("segment metadata discontinuity = %q, want true", got)
	}
}

// TestDiscardedSpoolIsCountedAsLost makes the spool wipe before each ffmpeg
// generation visible. The wipe itself is correct, but the segments were real
// and must not vanish from the loss counter.
func TestDiscardedSpoolIsCountedAsLost(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	for _, name := range []string{"seg-000000.ts", "seg-000001.ts"} {
		if err := os.WriteFile(filepath.Join(w.spoolDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.accountDiscardedSpool(); err != nil {
		t.Fatal(err)
	}
	if got := w.Snapshot().SegmentsLost; got != 2 {
		t.Errorf("SegmentsLost = %d, want 2", got)
	}
}

// TestCarryOverDrainRescuesPreviousGeneration is the avoidable-loss case: the
// object store was down when ffmpeg died, and the reconnect backoff gives it
// time to come back before the spool is destroyed.
func TestCarryOverDrainRescuesPreviousGeneration(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	armFailure(store, func(string) error { return errors.New("store down") })
	gen := newDrainState()
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	if _, err := w.drain(context.Background(), gen); err == nil {
		t.Fatal("precondition: the drain should have failed")
	}
	w.prevGen = gen

	store.mu.Lock()
	store.failKey = nil
	store.mu.Unlock()

	w.flushPreviousGeneration(context.Background())
	if w.prevGen != nil {
		t.Error("the previous generation must be released after the flush")
	}
	if w.lastSequence != 1 {
		t.Errorf("lastSequence = %d, want 1: the spooled segment should have been rescued", w.lastSequence)
	}
	if store.lastManifest() == "" {
		t.Error("the rescued segment must be published in a manifest")
	}
	if got := w.Snapshot().SegmentsLost; got != 0 {
		t.Errorf("SegmentsLost = %d, want 0: nothing was actually lost", got)
	}
}

// TestSegmentsSpanningMidnightUseTheirCaptureDate pins the date-directory
// rule: the day comes from the segment's own wall clock, not from upload time,
// so a slow upload does not file a segment under the wrong day.
func TestSegmentsSpanningMidnightUseTheirCaptureDate(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	// basePDT is 2026-08-31T23:59:55Z, so the second segment lands on 09-01.
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
		{name: "seg-000001.ts", duration: 4 * time.Second, pdt: basePDT.Add(6 * time.Second), body: []byte("b")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	var before, after bool
	for _, k := range store.keys() {
		if strings.Contains(k, "2026/08/31/") {
			before = true
		}
		if strings.Contains(k, "2026/09/01/") {
			after = true
		}
	}
	if !before || !after {
		t.Errorf("segments must be filed by capture date on both sides of midnight, got %v", store.keys())
	}
}

// TestSegmentReclaimedDuringUploadCountsAsLost covers the window between the
// playlist naming a segment and the upload slot being granted. ffmpeg's
// delete_segments can remove the file in between, and that is data loss, not
// an object-store failure: counting it as the latter would hide a real gap
// behind a retry that can never succeed.
func TestSegmentReclaimedDuringUploadCountsAsLost(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000000.ts", duration: 4 * time.Second, pdt: basePDT, absent: true},
	})

	res, err := w.drain(context.Background(), newDrainState())
	if err != nil {
		t.Fatalf("a vanished segment is loss, not an error to retry: %v", err)
	}
	if res.Uploaded != 0 {
		t.Errorf("Uploaded = %d, want 0", res.Uploaded)
	}
	if got := w.Snapshot().SegmentsLost; got != 1 {
		t.Errorf("SegmentsLost = %d, want 1", got)
	}
	if got := w.Snapshot().UploadFailures; got != 0 {
		t.Errorf("UploadFailures = %d, want 0: the store was never at fault", got)
	}
}

// TestDrainIgnoresAPlaylistEntryThatIsNotASegmentName treats the playlist as
// untrusted input. Its entries are joined onto the spool path, so a crafted
// name must not be able to reach another directory.
func TestDrainIgnoresAPlaylistEntryThatIsNotASegmentName(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	outside := filepath.Join(t.TempDir(), "secret.ts")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000,\n" +
		"../../" + filepath.Base(filepath.Dir(outside)) + "/secret.ts\n"
	if err := os.WriteFile(filepath.Join(w.spoolDir, ffmpeg.LocalPlaylistName),
		[]byte(playlist), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, k := range store.keys() {
		if strings.Contains(k, "secret") {
			t.Fatalf("a playlist entry escaped the spool: %s", k)
		}
	}
	if w.lastSequence != 0 {
		t.Errorf("lastSequence = %d, want 0: nothing legitimate was published", w.lastSequence)
	}
}

// TestMissingProgramDateTimeIsCounted makes the fallback visible. Without
// ffmpeg's timestamp the date directory silently follows upload time, which
// breaks the documented rule and changes the object key between retries.
func TestMissingProgramDateTimeIsCounted(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	// A playlist with EXTINF but no EXT-X-PROGRAM-DATE-TIME.
	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000,\nseg-000000000.ts\n"
	if err := os.WriteFile(filepath.Join(w.spoolDir, "seg-000000000.ts"),
		[]byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.spoolDir, ffmpeg.LocalPlaylistName),
		[]byte(playlist), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	if got := w.mMissingPDT.Value(); got != 1 {
		t.Errorf("missing_pdt_total = %v, want 1", got)
	}
	// It is still published: dropping the segment would be worse than filing
	// it under the wrong second.
	if w.lastSequence != 1 {
		t.Errorf("lastSequence = %d, want 1", w.lastSequence)
	}
}

// TestGapMetricsAreComputedAtScrapeTime is the staleness fix. The gap gauges
// used to be written only while a generation was running, so they froze during
// reconnect backoff and in the failed state, which is exactly when an operator
// looks at them.
func TestGapMetricsAreComputedAtScrapeTime(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	w.setState(StateReceiving)

	first := w.mGapSeconds.Value()
	time.Sleep(20 * time.Millisecond)
	second := w.mGapSeconds.Value()
	if !(second > first) {
		t.Errorf("seconds_since_segment did not advance without a write: %v then %v", first, second)
	}
	if w.mManifestLag.Value() <= 0 {
		t.Error("seconds_since_manifest should be positive after publishing")
	}

	// The alarm only applies to a channel that claims to be receiving.
	w.setState(StateReconnecting)
	if got := w.mGapAlarm.Value(); got != 0 {
		t.Errorf("gap alarm = %v while reconnecting, want 0", got)
	}
}

func TestGapMetricsAreZeroBeforeTheFirstSegment(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	if got := w.mGapSeconds.Value(); got != 0 {
		t.Errorf("seconds_since_segment = %v before any segment, want 0", got)
	}
	if got := w.mManifestLag.Value(); got != 0 {
		t.Errorf("seconds_since_manifest = %v before any manifest, want 0", got)
	}
}

// TestSpoolMetricsReportTheBacklog gives the operator the number that decides
// whether a store outage is about to turn into loss.
func TestSpoolMetricsReportTheBacklog(t *testing.T) {
	w := newTestWorker(t, &fakeStore{})
	for _, name := range []string{"seg-000000000.ts", "seg-000000001.ts"} {
		if err := os.WriteFile(filepath.Join(w.spoolDir, name), []byte("12345"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.refreshSpoolMetrics()
	if got := w.mSpoolFiles.Value(); got != 2 {
		t.Errorf("spool_files = %v, want 2", got)
	}
	if got := w.mSpoolBytes.Value(); got != 10 {
		t.Errorf("spool_bytes = %v, want 10", got)
	}
	if got := w.mSpoolOldest.Value(); got < 0 {
		t.Errorf("spool_oldest_seconds = %v, want >= 0", got)
	}

	// The scan is rate limited, so an immediate second call must not run it.
	if err := os.WriteFile(filepath.Join(w.spoolDir, "seg-000000002.ts"),
		[]byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.refreshSpoolMetrics()
	if got := w.mSpoolFiles.Value(); got != 2 {
		t.Errorf("spool_files = %v, want the rate-limited value 2", got)
	}
}

// TestSegmentAndManifestFailuresAreCountedSeparately is what lets an operator
// tell "nothing is being stored" from "everything is stored but invisible".
func TestSegmentAndManifestFailuresAreCountedSeparately(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	armFailure(store, func(key string) error {
		if strings.HasSuffix(key, ".m3u8") {
			return errors.New("manifest put failed")
		}
		return nil
	})
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err == nil {
		t.Fatal("expected the manifest failure to surface")
	}
	if got := w.mManifestFail.Value(); got != 1 {
		t.Errorf("manifest failures = %v, want 1", got)
	}
	if got := w.mSegmentFail.Value(); got != 0 {
		t.Errorf("segment failures = %v, want 0: the segment stored fine", got)
	}
	if got := w.mUploadFail.Value(); got != 1 {
		t.Errorf("combined failures = %v, want 1 so existing alarms keep working", got)
	}
}

func TestFailedStateIsCounted(t *testing.T) {
	// A manifest that is not ours is terminal: only an operator can decide
	// what it means. An unreadable store, by contrast, is retried.
	store := &fakeStore{}
	w := newUnownedWorker(t, store, "test-session")
	if _, err := store.Put(context.Background(), storage.Object{
		Key:  w.objectKey(w.cfg.Storage.ManifestName),
		Body: []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000,\nchunk_00001.ts\n"),
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	deadline := time.After(3 * time.Second)
	for w.Snapshot().State != StateFailed {
		select {
		case <-deadline:
			t.Fatalf("state = %q, want failed", w.Snapshot().State)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := w.mFailed.Value(); got != 1 {
		t.Errorf("failed_total = %v, want 1", got)
	}
	cancel()
	<-done
}

// TestSecondShardCannotStartWhileTheFirstOwnsTheCamera is the admission check.
// During a rolling deploy the outgoing task still owns the camera and is still
// serving it, so the incoming one must wait rather than start a second ffmpeg.
func TestSecondShardCannotStartWhileTheFirstOwnsTheCamera(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)
	_ = first

	second := newUnownedWorker(t, store, "other-shard-session")
	err := second.claim(context.Background())
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld", err)
	}
}

// TestOwnershipPassesAfterAReleasedLease covers the clean handover.
func TestOwnershipPassesAfterAReleasedLease(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)
	releaseOwnership(t, first)

	second := newUnownedWorker(t, store, "other-shard-session")
	if err := second.claim(context.Background()); err != nil {
		t.Fatalf("a released lease must be claimable: %v", err)
	}
}

// TestExpiredLeaseIsTakenOver covers the crash case: the previous owner never
// released, so the successor waits out the TTL and takes over.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)

	second := newUnownedWorker(t, store, "other-shard-session")
	// Advance the contender's clock past the holder's expiry plus the skew
	// allowance, rather than sleeping for the real TTL.
	skip := first.cfg.Lease.TTL.Duration + 3*first.cfg.Lease.MaxClockSkew.Duration
	second.lease.now = func() time.Time { return time.Now().Add(skip) }

	if err := second.claim(context.Background()); err != nil {
		t.Fatalf("an expired lease must be taken over: %v", err)
	}
}

// TestStaleOwnerIsFencedOutAfterTakeover is the race the lease alone cannot
// close, and the reason the new owner must fence the manifest before it starts.
//
// A froze holding a valid compare-and-swap token. Its lease expired and B took
// over. When A wakes and publishes, its token must already be spent -- if the
// fence write did not happen, A's write would succeed and B's would be the one
// rejected, letting the dead shard evict the live one.
func TestStaleOwnerIsFencedOutAfterTakeover(t *testing.T) {
	store := &fakeStore{}
	stale := newTestWorker(t, store)

	// Ownership moves to another shard while A is frozen.
	fresh := newUnownedWorker(t, store, "other-shard-session")
	skip := stale.cfg.Lease.TTL.Duration + 3*stale.cfg.Lease.MaxClockSkew.Duration
	fresh.lease.now = func() time.Time { return time.Now().Add(skip) }
	if err := fresh.claim(context.Background()); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	// A wakes up and tries to publish. Its manifest token is stale.
	writeSpool(t, stale.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	_, err := stale.drain(context.Background(), newDrainState())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost: the stale owner must be fenced out", err)
	}
	if got := stale.mManifestConflict.Value(); got != 1 {
		t.Errorf("manifest conflicts = %v, want 1", got)
	}

	// The new owner is unaffected and still publishes.
	writeSpool(t, fresh.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT.Add(time.Minute), body: []byte("b")},
	})
	if _, err := fresh.drain(context.Background(), newDrainState()); err != nil {
		t.Fatalf("the live owner must keep publishing: %v", err)
	}
}

// TestFenceWriteChangesTheManifestVersion is what makes the fence effective.
// If the rewritten body were byte-identical the ETag would not move and the
// previous owner's token would still be valid.
func TestFenceWriteChangesTheManifestVersion(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)
	writeSpool(t, first.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	if _, err := first.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	beforeETag := first.manifestETag
	beforeBody := store.lastManifest()

	releaseOwnership(t, first)
	second := newUnownedWorker(t, store, "other-shard-session")
	if err := second.claim(context.Background()); err != nil {
		t.Fatal(err)
	}

	if second.manifestETag == beforeETag {
		t.Error("the fence write must produce a new manifest version")
	}
	after := store.lastManifest()
	if after == beforeBody {
		t.Error("the fence write must change the manifest body, or the ETag will not move")
	}
	// The playlist itself must survive: fencing is not a reset.
	if !strings.Contains(after, "#EXT-X-MEDIA-SEQUENCE:1") {
		t.Errorf("fencing must preserve the recovered window\n%s", after)
	}
	if !strings.Contains(after, hls.WriteIDComment) {
		t.Errorf("fenced manifest must carry a write marker\n%s", after)
	}
}

// TestAmbiguousManifestWriteIsAdoptedNotRetried covers a write that landed
// while its response was lost. Retrying a conditional write would see its own
// precondition already consumed and report a conflict that never happened,
// which would look exactly like being fenced out.
func TestAmbiguousManifestWriteIsAdoptedNotRetried(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})

	// The next manifest write stores its body and then reports a failure.
	var fired bool
	store.mu.Lock()
	store.ambiguous = func(key string) bool {
		if strings.HasSuffix(key, ".m3u8") && !fired {
			fired = true
			return true
		}
		return false
	}
	store.mu.Unlock()

	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatalf("an ambiguous write that landed must be adopted, not surfaced: %v", err)
	}
	if w.manifestDirty {
		t.Error("the manifest is stored, so it must not still be pending")
	}
	if !strings.Contains(store.lastManifest(), "seg-000000001-") {
		t.Errorf("the adopted manifest must reference the segment\n%s", store.lastManifest())
	}
	// A later write must still be accepted, which only works if the adopted
	// ETag was recorded.
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000001.ts", duration: 4 * time.Second, pdt: basePDT.Add(4 * time.Second), body: []byte("b")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatalf("the adopted version must be usable for the next write: %v", err)
	}
}

// TestSegmentKeysAreCreateOnly stops a late writer from overwriting an
// immutable object that is already cached for a year downstream.
func TestSegmentKeysAreCreateOnly(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	if _, err := w.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	var segKey string
	for _, k := range store.keys() {
		if strings.HasSuffix(k, ".ts") {
			segKey = k
		}
	}
	if segKey == "" {
		t.Fatal("no segment was stored")
	}
	// Anyone re-writing that key unconditionally would be poisoning caches;
	// a create-only write is refused instead.
	_, err := store.Put(context.Background(), storage.Object{
		Key:           segKey,
		Body:          []byte("different bytes"),
		Preconditions: storage.Preconditions{IfNoneMatch: true},
	})
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want a refused create", err)
	}
}

// TestSegmentConflictFromAnotherOwnerIsTerminal is the fail-closed case: the
// key exists with somebody else's put-id, so two shards are publishing. The
// worker must stop rather than reference an object it did not produce.
func TestSegmentConflictFromAnotherOwnerIsTerminal(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)

	// Pre-place the segment key this worker is about to use, with foreign
	// provenance.
	pdt := basePDT
	key := w.objectKey(pdt.UTC().Format("2006/01/02") +
		fmt.Sprintf("/seg-%09d-%d.ts", 1, pdt.UTC().UnixMilli()))
	if _, err := store.Put(context.Background(), storage.Object{
		Key:      key,
		Body:     []byte("someone else's segment"),
		Metadata: map[string]string{"put-id": "not-ours"},
	}); err != nil {
		t.Fatal(err)
	}

	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: pdt, body: []byte("ours")},
	})
	_, err := w.drain(context.Background(), newDrainState())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if got := w.mSegmentConflict.Value(); got != 1 {
		t.Errorf("segment conflicts = %v, want 1", got)
	}
	// The foreign object must be intact.
	body, _, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "someone else's segment" {
		t.Error("a conflicting segment must never be overwritten")
	}
}

// TestOwnAmbiguousSegmentWriteIsAdopted is the other side of a refused create:
// the object is already there because our own earlier attempt landed.
func TestOwnAmbiguousSegmentWriteIsAdopted(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("payload")},
	})

	var fired bool
	store.mu.Lock()
	store.ambiguous = func(key string) bool {
		if strings.HasSuffix(key, ".ts") && !fired {
			fired = true
			return true
		}
		return false
	}
	store.mu.Unlock()

	// The write lands, the response is lost, and the worker resolves it in the
	// same pass by asking what is at the key: its own put-id is there.
	res, err := w.drain(context.Background(), newDrainState())
	if err != nil {
		t.Fatalf("our own landed write must be adopted: %v", err)
	}
	if res.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1", res.Uploaded)
	}
	if got := w.mSegmentConflict.Value(); got != 0 {
		t.Errorf("segment conflicts = %v, want 0: it was our own write", got)
	}
	// Exactly one object, and the manifest references it.
	var segs int
	for _, k := range store.keys() {
		if strings.HasSuffix(k, ".ts") {
			segs++
		}
	}
	if segs != 1 {
		t.Errorf("%d segment writes, want 1: an adopted write must not be duplicated", segs)
	}
	if !strings.Contains(store.lastManifest(), "seg-000000001-") {
		t.Errorf("the adopted segment must be referenced\n%s", store.lastManifest())
	}
}

// TestPublishRefusedPastTheLeaseDeadline covers a store that stops answering
// renewals. Rather than keep publishing on an expired claim, the worker stops:
// another shard may already have taken the camera over.
func TestPublishRefusedPastTheLeaseDeadline(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	w.lease.now = func() time.Time {
		return time.Now().Add(w.cfg.Lease.TTL.Duration + time.Second)
	}
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	_, err := w.drain(context.Background(), newDrainState())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
}

// TestFinalDrainSkippedWithoutOwnership: a fenced worker must not flush its
// spool, because those segments belong to a timeline that is no longer
// authoritative.
func TestFinalDrainSkippedWithoutOwnership(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	writeSpool(t, w.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	w.lease.held = false

	before := len(store.keys())
	w.finalDrain(newDrainState())
	if got := store.keysSince(before); len(got) != 0 {
		t.Errorf("a worker without ownership must not publish, got %v", got)
	}
}

// TestLeaseRenewalRejectionIsTerminal: the renewal is refused because another
// shard now holds the lease.
func TestLeaseRenewalRejectionIsTerminal(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)

	// Another shard takes over after the TTL.
	second := newUnownedWorker(t, store, "other-shard-session")
	skip := first.cfg.Lease.TTL.Duration + 3*first.cfg.Lease.MaxClockSkew.Duration
	second.lease.now = func() time.Time { return time.Now().Add(skip) }
	if err := second.claim(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := first.lease.Renew(context.Background())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if first.lease.held {
		t.Error("a refused renewal must clear ownership")
	}
}

// TestLeaseRecordCarriesNoSecrets: the lease object sits in the media bucket
// and inherits whatever read scope that has.
func TestLeaseRecordCarriesNoSecrets(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	body, _, err := store.Get(context.Background(), w.objectKey(LeaseObjectName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"pass", "user:", "rtsp://"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("lease record leaked %q: %s", secret, body)
		}
	}
}

// TestSequenceFloorSurvivesAnEmptyFenceManifest is the regression the review
// found. Recovery drops the window when a checkpoint floor is ahead of the
// manifest, so the ownership fence writes an empty playlist. If that playlist
// did not record the sequence, a later process recovering from it would restart
// at zero and reissue object keys that are already live and cached for a year.
func TestSequenceFloorSurvivesAnEmptyFenceManifest(t *testing.T) {
	store := &fakeStore{}
	first := newTestWorker(t, store)
	writeSpool(t, first.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT, body: []byte("a")},
	})
	if _, err := first.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	// A checkpoint far ahead of the manifest: sequences were used that no
	// manifest ever recorded.
	if err := saveCheckpoint(first.statePath, checkpoint{LastSequence: 9}); err != nil {
		t.Fatal(err)
	}
	releaseOwnership(t, first)

	// The successor resumes from the floor and fences with an empty window.
	second := restartWorker(t, first, store)
	if err := second.claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second.lastSequence != 9 {
		t.Fatalf("lastSequence = %d, want the checkpoint floor 9", second.lastSequence)
	}
	if len(second.window) != 0 {
		t.Fatalf("window = %d entries, want it dropped", len(second.window))
	}
	fenced := store.lastManifest()
	if !strings.Contains(fenced, hls.LastSequenceComment+"9") {
		t.Fatalf("the fence manifest must record the sequence floor\n%s", fenced)
	}

	// Now lose the checkpoint entirely, as a replaced container does. The
	// manifest is the only surviving record.
	releaseOwnership(t, second)
	cfg := second.cfg
	cfg.StateDir = t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	third := NewWorker(second.cam, cfg, store, metrics.NewRegistry(), log, "test-session-3")
	if third.checkpointFloor != 0 {
		t.Fatalf("precondition: the checkpoint must be gone, got floor %d", third.checkpointFloor)
	}
	if err := third.claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if third.lastSequence != 9 {
		t.Errorf("lastSequence = %d, want 9 recovered from the empty manifest", third.lastSequence)
	}

	// And the next segment must not reuse a key.
	writeSpool(t, third.spoolDir, []spoolEntry{
		{name: "seg-000000.ts", duration: 4 * time.Second, pdt: basePDT.Add(time.Hour), body: []byte("c")},
	})
	if _, err := third.drain(context.Background(), newDrainState()); err != nil {
		t.Fatal(err)
	}
	if third.lastSequence != 10 {
		t.Errorf("lastSequence = %d, want 10", third.lastSequence)
	}
	seen := map[string]int{}
	for _, k := range store.keys() {
		if strings.HasSuffix(k, ".ts") {
			seen[k]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("segment key %q written %d times", k, n)
		}
	}
}

// TestRunReleasesALeaseItCouldNotUse: claim can take the lease and then fail on
// a foreign manifest. Holding it until the TTL blocks a legitimate successor
// for no reason.
func TestRunReleasesALeaseItCouldNotUse(t *testing.T) {
	store := &fakeStore{}
	w := newUnownedWorker(t, store, "test-session")
	if _, err := store.Put(context.Background(), storage.Object{
		Key:  w.objectKey(w.cfg.Storage.ManifestName),
		Body: []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000,\nchunk_00001.ts\n"),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	deadline := time.After(3 * time.Second)
	for w.Snapshot().State != StateFailed {
		select {
		case <-deadline:
			t.Fatalf("state = %q, want failed", w.Snapshot().State)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	// A successor must be able to claim immediately, without waiting out the TTL.
	other := newUnownedWorker(t, store, "other-session")
	err := other.lease.Acquire(context.Background())
	if errors.Is(err, ErrLeaseHeld) {
		t.Fatal("the failed worker kept its lease, blocking a successor for a full TTL")
	}
}

// TestInvalidLeaseRecordIsTerminal: a lease object that is not a lease record
// cannot be fixed by waiting, so it must not be retried forever.
func TestInvalidLeaseRecordIsTerminal(t *testing.T) {
	store := &fakeStore{}
	w := newUnownedWorker(t, store, "test-session")
	if _, err := store.Put(context.Background(), storage.Object{
		Key:  w.objectKey(LeaseObjectName),
		Body: []byte("this is not json"),
	}); err != nil {
		t.Fatal(err)
	}
	err := w.claim(context.Background())
	if !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("err = %v, want ErrLeaseInvalid", err)
	}
	if errors.Is(err, ErrLeaseUnavailable) {
		t.Error("an unreadable record must not be classified as a transient outage")
	}
}

func TestLeaseForAnotherCameraIsTerminal(t *testing.T) {
	store := &fakeStore{}
	w := newUnownedWorker(t, store, "test-session")
	rec := `{"schema":1,"camera_key":"other/cam","owner_session":"x","status":"held",` +
		`"expires_at":"2099-01-01T00:00:00Z"}`
	if _, err := store.Put(context.Background(), storage.Object{
		Key:  w.objectKey(LeaseObjectName),
		Body: []byte(rec),
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.claim(context.Background()); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("err = %v, want ErrLeaseInvalid", err)
	}
}

// TestFenceOutageIsRetryableNotTerminal: a store that will not answer during
// the fence must leave the channel waiting, not permanently disabled. Nothing
// has been published at that point.
func TestFenceOutageIsRetryableNotTerminal(t *testing.T) {
	store := &fakeStore{}
	w := newUnownedWorker(t, store, "test-session")
	armFailure(store, func(key string) error {
		if strings.HasSuffix(key, ".m3u8") {
			return errors.New("store unreachable")
		}
		return nil
	})
	err := w.claim(context.Background())
	if !errors.Is(err, ErrLeaseUnavailable) {
		t.Fatalf("err = %v, want a retryable ownership error", err)
	}
}

// TestRenewalAfterTheDeadlineDoesNotReviveOwnership: a renewal whose response
// arrives after the local deadline has passed must not extend the claim. By
// then a contender is entitled to take over.
func TestRenewalAfterTheDeadlineDoesNotReviveOwnership(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)

	// Freeze the clock past the deadline established at acquire time, then
	// renew: the store will accept it, but the holder must not.
	w.lease.now = func() time.Time {
		return time.Now().Add(w.cfg.Lease.TTL.Duration + time.Second)
	}
	err := w.lease.Renew(context.Background())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if !w.lease.Expired() {
		t.Error("a lapsed holder must stay expired")
	}
}

// TestLeaseIsSafeUnderConcurrentRenewalAndPublication exercises the goroutine
// pairing the original implementation got wrong: the renewal loop mutating
// lease state while the publish path reads it. It exists to be run under the
// race detector, which never saw this path before because no test ran Run all
// the way through ownership.
func TestLeaseIsSafeUnderConcurrentRenewalAndPublication(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	// Renew far faster than production so many renewals overlap the drains.
	w.cfg.Lease.RenewInterval = config.Duration{Duration: time.Millisecond}
	w.cfg.Lease.OperationTimeout = config.Duration{Duration: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		w.runLeaseRenewal(ctx, cancel)
	}()

	deadline := time.After(300 * time.Millisecond)
	i := 0
loop:
	for {
		select {
		case <-deadline:
			break loop
		default:
		}
		i++
		writeSpool(t, w.spoolDir, []spoolEntry{
			{name: fmt.Sprintf("seg-%06d.ts", i), duration: 4 * time.Second,
				pdt: basePDT.Add(time.Duration(i) * 4 * time.Second), body: []byte("x")},
		})
		if _, err := w.drain(ctx, newDrainState()); err != nil && ctx.Err() == nil {
			t.Fatalf("drain: %v", err)
		}
		_ = w.lease.Expired()
		_ = w.leaseSecondsRemaining()
	}
	cancel()
	<-renewalDone

	if err := w.lease.Release(context.Background()); err != nil && !errors.Is(err, ErrLeaseLost) {
		t.Logf("release after cancellation: %v", err)
	}
}

// TestRenewalIsJoinedBeforeRelease pins the shutdown ordering. Releasing while
// a renewal is in flight has the two race their compare-and-swaps, and a
// renewal landing after the release leaves the lease held until the TTL.
func TestRenewalIsJoinedBeforeRelease(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	leaseKey := w.objectKey(LeaseObjectName)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	// Let it reach the generation loop; ffmpeg will fail to start (no binary in
	// the unit-test image), which is fine: ownership is already established.
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	// The final lease write must be the release tombstone, not a renewal.
	var last string
	store.mu.Lock()
	for _, p := range store.puts {
		if p.Key == leaseKey {
			last = string(p.Body)
		}
	}
	store.mu.Unlock()
	if last == "" {
		t.Fatal("no lease writes recorded")
	}
	if !strings.Contains(last, `"status":"released"`) {
		t.Errorf("the last lease write must be the release, got %s", last)
	}
}
