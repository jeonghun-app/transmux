package recording

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// slowStore models an object store whose HEAD requests are slow or fail.
type slowStore struct {
	*storage.FilesystemStore
	delay time.Duration
	heads atomic.Int64
	fail  atomic.Int64 // HEAD number that fails once; zero never fails
}

func (s *slowStore) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	n := s.heads.Add(1)
	if n == s.fail.Load() {
		return storage.ObjectInfo{}, errors.New("injected HEAD failure")
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return storage.ObjectInfo{}, ctx.Err()
	}
	return s.FilesystemStore.Head(ctx, key)
}

func scannerFixture(t *testing.T) (*Index, *storage.FilesystemStore) {
	t.Helper()
	index, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	store, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return index, store
}

func countIndexed(t *testing.T, index *Index, day time.Time) int {
	t.Helper()
	got, err := index.Query(context.Background(), "c1", "cam1", day, day.Add(24*time.Hour), 100000)
	if err != nil {
		t.Fatal(err)
	}
	return len(got)
}

func TestScanDayKeepsProgressWhenHeadsOutlastTheBudget(t *testing.T) {
	ctx := context.Background()
	index, fs := scannerFixture(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -5)
	for n := 1; n <= 300; n++ {
		recordingFixture(t, fs, uint64(n), day.Add(time.Hour+time.Duration(n)*4*time.Second), 4*time.Second)
	}
	// The audit case is 256 HEADs at 150ms against a 30s budget. This is the
	// same shape scaled down: one page needs about 2.6s of HEADs but only
	// 250ms is available per call.
	store := &slowStore{FilesystemStore: fs, delay: 10 * time.Millisecond}
	scanner := &Scanner{Index: index, Store: store, Prefix: "archive", Workers: 2,
		ScanTimeout: 250 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cam := config.StaticCamera{CenterID: "c1", CameraID: "cam1"}
	if err := scanner.ScanDay(ctx, cam, day); err != nil {
		t.Fatalf("an exhausted page budget is progress, not a failure: %v", err)
	}
	first := countIndexed(t, index, day)
	state, err := index.ScanState("c1", "cam1", day)
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 || first >= 256 || state.Cursor == "" || state.Complete {
		t.Fatalf("partial page not saved: indexed=%d state=%+v", first, state)
	}
	// A failing HEAD keeps what was confirmed before it and resumes there.
	store.fail.Store(store.heads.Load() + 3)
	if err := scanner.ScanDay(ctx, cam, day); err == nil {
		t.Fatal("store failure was hidden")
	}
	if got := countIndexed(t, index, day); got != first+2 {
		t.Fatalf("progress before the failure lost: before=%d after=%d", first, got)
	}
	for n := 0; n < 200 && countIndexed(t, index, day) < 300; n++ {
		if err := scanner.ScanDay(ctx, cam, day); err != nil {
			t.Fatal(err)
		}
	}
	if got := countIndexed(t, index, day); got != 300 {
		t.Fatalf("slow store stalled the scan at %d of 300", got)
	}
	state, err = index.ScanState("c1", "cam1", day)
	if err != nil || !state.Complete {
		t.Fatalf("scan never completed: %+v %v", state, err)
	}
}

func TestScanDayRemovesRecordingsDeletedOutsideTheIndex(t *testing.T) {
	defer func(grace time.Duration) { reconcileGrace = grace }(reconcileGrace)
	reconcileGrace = 0
	ctx := context.Background()
	index, store := scannerFixture(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	var records []Segment
	for n := 1; n <= 600; n++ {
		records = append(records, recordingFixture(t, store, uint64(n), day.Add(time.Duration(n)*4*time.Second), 4*time.Second))
	}
	scanner := &Scanner{Index: index, Store: store, Prefix: "archive", Workers: 2,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cam := config.StaticCamera{CenterID: "c1", CameraID: "cam1"}
	for range 3 {
		if err := scanner.ScanDay(ctx, cam, day); err != nil {
			t.Fatal(err)
		}
	}
	if got := countIndexed(t, index, day); got != 600 {
		t.Fatalf("setup indexed %d of 600", got)
	}
	// A Lifecycle rule deletes objects across page boundaries, including the
	// last one of the day, without telling the index.
	deleted := map[string]bool{}
	for _, n := range []int{0, 1, 255, 256, 300, 511, 512, 599} {
		if err := store.Delete(ctx, records[n].Key); err != nil {
			t.Fatal(err)
		}
		deleted[records[n].URI] = true
	}
	time.Sleep(5 * time.Millisecond)
	state, err := index.ScanState("c1", "cam1", day)
	if err != nil {
		t.Fatal(err)
	}
	state.CycleStarted, state.ScannedAt = time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour)
	if err := index.SaveScan("c1", "cam1", day, state); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := scanner.ScanDay(ctx, cam, day); err != nil {
			t.Fatal(err)
		}
	}
	if got := countIndexed(t, index, day); got != 600-len(deleted) {
		t.Fatalf("deleted recordings remain indexed: %d", got)
	}
	for _, record := range records {
		_, err := index.Get("c1", "cam1", record.URI)
		if deleted[record.URI] != errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("%s: deleted=%v get=%v", record.URI, deleted[record.URI], err)
		}
	}
	if status := scanner.Status(); status.Removed != uint64(len(deleted)) {
		t.Fatalf("removed counter %d", status.Removed)
	}
}

func TestReconcileKeepsEntriesIndexedAfterTheListingAndMigratesOldPartitions(t *testing.T) {
	defer func(grace time.Duration) { reconcileGrace = grace }(reconcileGrace)
	reconcileGrace = 0
	index, store := scannerFixture(t)
	day := time.Now().UTC().Truncate(24 * time.Hour)
	listedAt := time.Now().Add(-time.Second)
	live := recordingFixture(t, store, 1, day.Add(time.Minute), 4*time.Second)
	if err := index.Put([]Segment{live}); err != nil {
		t.Fatal(err)
	}
	if n, err := index.Reconcile("c1", "cam1", day, "", "", nil, listedAt); err != nil || n != 0 {
		t.Fatalf("an object written after the listing was removed: %d %v", n, err)
	}
	// An index written before object names were tracked has no name bucket.
	if err := index.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(objectsBucket).DeleteBucket(partition("c1", "cam1", day))
	}); err != nil {
		t.Fatal(err)
	}
	kept := recordingFixture(t, store, 2, day.Add(2*time.Minute), 4*time.Second)
	if err := index.Put([]Segment{kept}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	n, err := index.Reconcile("c1", "cam1", day, "", "", []string{kept.URI}, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("migrated partition not reconciled: %d %v", n, err)
	}
	if _, err := index.Get("c1", "cam1", live.URI); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unlisted legacy entry kept: %v", err)
	}
	if _, err := index.Get("c1", "cam1", kept.URI); err != nil {
		t.Fatalf("listed entry removed: %v", err)
	}
	// Removal by retention also drops the object name.
	if err := index.Remove(kept); err != nil {
		t.Fatal(err)
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(objectsBucket).Bucket(partition("c1", "cam1", day)).Get([]byte(kept.URI)) != nil {
			return errors.New("object name survived removal")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPassPrunesDaysOutsideTheIndexWindow(t *testing.T) {
	ctx := context.Background()
	index, store := scannerFixture(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	oldDay, keptDay := today.AddDate(0, 0, -3), today.AddDate(0, 0, -2)
	for n, day := range []time.Time{oldDay, keptDay} {
		if err := index.Put([]Segment{recordingFixture(t, store, uint64(n+1), day.Add(time.Hour), 4*time.Second)}); err != nil {
			t.Fatal(err)
		}
		if err := index.SaveScan("c1", "cam1", day, ScanState{Complete: true, ScannedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// A scanned day with no recordings still has scan state to prune.
	empty := today.AddDate(0, 0, -40)
	if err := index.SaveScan("c2", "cam2", empty, ScanState{Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err := index.SaveCursor("retention/c1/cam1", "archive/c1/cam1/x"); err != nil {
		t.Fatal(err)
	}
	identity := index.Identity()
	scanner := &Scanner{Index: index, Store: store, Prefix: "archive", Days: 3, Workers: 2,
		Roster: func(context.Context) ([]config.StaticCamera, error) { return nil, nil },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil))}
	scanner.Pass(ctx)
	if countIndexed(t, index, oldDay) != 0 || countIndexed(t, index, keptDay) != 1 {
		t.Fatal("prune did not follow the index window")
	}
	for _, check := range []struct {
		center, camera string
		day            time.Time
		want           bool
	}{{"c1", "cam1", oldDay, false}, {"c1", "cam1", keptDay, true}, {"c2", "cam2", empty, false}} {
		state, err := index.ScanState(check.center, check.camera, check.day)
		if err != nil || state.Complete != check.want {
			t.Fatalf("scan state %v: %+v %v", check.day, state, err)
		}
	}
	if cursor, err := index.Cursor("retention/c1/cam1"); err != nil || cursor == "" {
		t.Fatalf("prune removed a retention cursor: %q %v", cursor, err)
	}
	filename := index.db.Path()
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Identity() != identity {
		t.Fatal("prune removed the index identity")
	}
}
