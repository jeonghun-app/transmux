package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func recordingFixture(t *testing.T, store storage.ObjectStore, seq uint64, start time.Time, duration time.Duration) Segment {
	t.Helper()
	uri := path.Join(start.UTC().Format("2006/01/02"), fmt.Sprintf("seg-%09d-%d.ts", seq, start.UnixMilli()))
	key := "archive/c1/cam1/" + uri
	body := []byte("immutable media")
	md := map[string]string{"center-id": "c1", "camera-id": "cam1", "sequence": strconv.FormatUint(seq, 10),
		"pdt-ms": strconv.FormatInt(start.UnixMilli(), 10), "duration-ms": strconv.FormatInt(duration.Milliseconds(), 10),
		"discontinuity": "false"}
	if _, err := store.Put(context.Background(), storage.Object{Key: key, Body: body, Metadata: md}); err != nil {
		t.Fatal(err)
	}
	record, err := FromObject("archive", "c1", "cam1", storage.Entry{Key: key}, storage.ObjectInfo{Size: int64(len(body)), Metadata: md})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRecordingQueryCrossesMidnightPreservesGapsAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "index.db")
	index, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	midnight := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	records := []Segment{
		recordingFixture(t, store, 1, midnight.Add(-2*time.Second), 4*time.Second),
		recordingFixture(t, store, 2, midnight.Add(2*time.Second), 6*time.Second),
		recordingFixture(t, store, 3, midnight.Add(20*time.Second), 4*time.Second),
	}
	if err := index.Put(records); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	got, err := index.Query(ctx, "c1", "cam1", midnight, midnight.Add(22*time.Second), 10)
	if err != nil || len(got) != 3 || got[0].URI != records[0].URI {
		t.Fatalf("midnight overlap lost: %#v %v", got, err)
	}
	playlist, err := Playlist(got)
	if err != nil {
		t.Fatal(err)
	}
	body := string(hls.RenderRecording(playlist))
	if !playlist[2].Discontinuity || !strings.Contains(body, "#EXTINF:6.000,") || !strings.HasSuffix(body, "#EXT-X-ENDLIST\n") {
		t.Fatalf("VOD lost true timing or gap: %s", body)
	}
	got, err = index.Query(ctx, "c1", "cam1", records[0].End(), records[1].End(), 10)
	if err != nil || len(got) != 1 || got[0].Sequence != 2 {
		t.Fatalf("end-exclusive overlap failed: %#v %v", got, err)
	}
	if _, err := index.Query(ctx, "c1", "cam1", midnight, midnight.Add(time.Minute), 2); !errors.Is(err, ErrTooMany) {
		t.Fatalf("query silently truncated: %v", err)
	}
}

func TestScannerPaginationAndDailyReconciliationFindLateObjects(t *testing.T) {
	ctx := context.Background()
	index, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	store, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	base := day.Add(time.Hour)
	// Reserve an early lexical key, then upload it after the scanner has
	// passed that cursor. It models a delayed stale-owner upload.
	for n := 2; n <= 270; n++ {
		recordingFixture(t, store, uint64(n), base.Add(time.Duration(n)*4*time.Second), 4*time.Second)
	}
	scanner := &Scanner{Index: index, Store: store, Prefix: "archive", Workers: 2,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cam := config.StaticCamera{CenterID: "c1", CameraID: "cam1"}
	for range 2 {
		if err := scanner.ScanDay(ctx, cam, day); err != nil {
			t.Fatal(err)
		}
	}
	late := recordingFixture(t, store, 1, base.Add(4*time.Second), 4*time.Second)
	if err := scanner.ScanDay(ctx, cam, day); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Get("c1", "cam1", late.URI); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("test did not create a late key: %v", err)
	}
	state, err := index.ScanState("c1", "cam1", day)
	if err != nil {
		t.Fatal(err)
	}
	state.CycleStarted = time.Now().Add(-25 * time.Hour)
	if err := index.SaveScan("c1", "cam1", day, state); err != nil {
		t.Fatal(err)
	}
	if err := scanner.ScanDay(ctx, cam, day); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Get("c1", "cam1", late.URI); err != nil {
		t.Fatalf("reconciliation missed late upload: %v", err)
	}
}

func TestRecordingSnapshotIsImmutableAndScopeIsExact(t *testing.T) {
	index, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	day := time.Now().UTC().Format("2006/01/02")
	initURI := day + "/init-" + strings.Repeat("a", 64) + ".mp4"
	uri := fmt.Sprintf("%s/seg-000000001-%d.m4s", day, time.Now().UnixMilli())
	segments := []hls.PublishedSegment{{Sequence: 0, URI: uri, InitURI: initURI, Duration: 4 * time.Second, ProgramDateTime: time.Now()}}
	if err := index.Snapshot("one", time.Now().Add(time.Minute), segments, 1); err != nil {
		t.Fatal(err)
	}
	body, err := index.SnapshotManifest("one")
	if err != nil {
		t.Fatal(err)
	}
	if !index.SnapshotAllows("one", uri) || !index.SnapshotAllows("one", initURI) ||
		index.SnapshotAllows("one", day+"/seg-other.ts") || index.SnapshotAllows("one", "_manifest") {
		t.Fatal("snapshot allowed an object outside its exact manifest")
	}
	if err := index.Snapshot("two", time.Now().Add(time.Minute), segments, 1); !errors.Is(err, ErrSessionCapacity) {
		t.Fatalf("unbounded snapshots: %v", err)
	}
	if err := index.ExtendSnapshot("one", time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := index.SnapshotManifest("one")
	if err != nil || string(body) != string(after) {
		t.Fatal("renewal changed a finite playlist")
	}
	if err := index.PruneSnapshots(time.Now().Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if index.SnapshotAllows("one", uri) {
		t.Fatal("expired snapshot still allows reads")
	}
	if err := index.Snapshot("two", time.Now().Add(time.Minute), segments, 1); err != nil {
		t.Fatalf("cleanup did not release capacity: %v", err)
	}
}
