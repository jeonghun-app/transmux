package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	bolt "go.etcd.io/bbolt"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func qaRecording(seq uint64, start time.Time, duration time.Duration) Segment {
	uri := path.Join(start.UTC().Format("2006/01/02"),
		fmt.Sprintf("seg-%09d-%d.ts", seq, start.UnixMilli()))
	return Segment{CenterID: "c1", CameraID: "cam1", URI: uri, Key: "archive/c1/cam1/" + uri,
		Sequence: seq, Start: start, DurationMS: duration.Milliseconds(), Size: 15}
}

// Set the persisted wall time directly to exercise exact grace boundaries
// without changing the process clock or sleeping for a minute.
func qaIndexedAt(t *testing.T, index *Index, record Segment, at time.Time) {
	t.Helper()
	if err := index.db.Update(func(tx *bolt.Tx) error {
		names := tx.Bucket(objectsBucket).Bucket(partition(record.CenterID, record.CameraID, record.Start))
		return names.Put([]byte(record.URI), objectValue(recordKey(record.Start, record.Sequence), at))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQAReconcileUsesExactListingRange(t *testing.T) {
	day := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	listedAt := day.Add(12 * time.Hour)
	records := make([]Segment, 7)
	for n := range records {
		records[n] = qaRecording(uint64(n+1), day.Add(time.Duration(n+1)*time.Second), time.Second)
	}
	for _, tc := range []struct {
		name         string
		after, upto  string
		listed, gone []int
	}{
		{"middle-page", records[0].URI, records[4].URI, []int{2, 4}, []int{1, 3}},
		{"first-page", "", records[2].URI, []int{0, 2}, []int{1}},
		{"last-page", records[3].URI, "", []int{5}, []int{4, 6}},
		{"empty-last-page", records[5].URI, "", nil, []int{6}},
		{"empty-complete-day", "", "", nil, []int{0, 1, 2, 3, 4, 5, 6}},
		{"exclusive-after-and-inclusive-upto", records[1].URI, records[4].URI, nil, []int{2, 3, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index, _ := scannerFixture(t)
			if err := index.Put(records); err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				qaIndexedAt(t, index, record, listedAt.Add(-2*time.Minute))
			}
			var listed []string
			for _, n := range tc.listed {
				listed = append(listed, records[n].URI)
			}
			removed, err := index.Reconcile(context.Background(), "c1", "cam1", day, tc.after, tc.upto, listed, listedAt)
			if err != nil || removed != len(tc.gone) {
				t.Fatalf("reconcile removed=%d want=%d err=%v", removed, len(tc.gone), err)
			}
			gone := make(map[int]bool)
			for _, n := range tc.gone {
				gone[n] = true
			}
			for n, record := range records {
				_, err := index.Get("c1", "cam1", record.URI)
				if gone[n] {
					if !errors.Is(err, storage.ErrNotFound) {
						t.Errorf("missing object %d survived: %v", n, err)
					}
				} else if err != nil {
					t.Errorf("object outside the missing range %d was removed: %v", n, err)
				}
			}
		})
	}
}

func TestQAReconcileGraceAtMillisecondBoundary(t *testing.T) {
	index, _ := scannerFixture(t)
	listedAt := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for n, tc := range []struct {
		name   string
		offset time.Duration
		gone   bool
	}{
		{"older-than-grace", -time.Minute - time.Millisecond, true},
		{"exactly-one-minute", -time.Minute, false},
		{"backward-clock-within-grace", -time.Minute + time.Millisecond, false},
		{"same-millisecond", 0, false},
		{"indexed-after-list-start", time.Millisecond, false},
		{"future-index-clock", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := qaRecording(uint64(n+1), listedAt.Add(time.Duration(n)*time.Second), time.Second)
			if err := index.Put([]Segment{record}); err != nil {
				t.Fatal(err)
			}
			qaIndexedAt(t, index, record, listedAt.Add(tc.offset))
			removed, err := index.Reconcile(context.Background(), "c1", "cam1", record.Start,
				"", record.URI, nil, listedAt)
			want := 0
			if tc.gone {
				want = 1
			}
			if err != nil || removed != want {
				t.Fatalf("offset=%v removed=%d want=%d err=%v", tc.offset, removed, want, err)
			}
			_, err = index.Get("c1", "cam1", record.URI)
			if tc.gone && !errors.Is(err, storage.ErrNotFound) || !tc.gone && err != nil {
				t.Fatalf("offset=%v gone=%v get=%v", tc.offset, tc.gone, err)
			}
		})
	}
}

func TestQAReconcileOldAliasDoesNotDeleteReplacement(t *testing.T) {
	index, _ := scannerFixture(t)
	listedAt := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	old := qaRecording(1, listedAt.Add(-time.Hour), time.Second)
	replacement := old
	replacement.URI = strings.TrimSuffix(old.URI, ".ts") + ".m4s"
	replacement.Key = "archive/c1/cam1/" + replacement.URI
	replacement.InitURI = path.Join(path.Dir(replacement.URI), "init-"+strings.Repeat("a", 64)+".mp4")
	if err := index.Put([]Segment{old, replacement}); err != nil {
		t.Fatal(err)
	}
	for _, record := range []Segment{old, replacement} {
		qaIndexedAt(t, index, record, listedAt.Add(-2*time.Minute))
	}
	if n, err := index.Reconcile(context.Background(), "c1", "cam1", old.Start,
		"", "", []string{replacement.URI}, listedAt); err != nil || n != 0 {
		t.Fatalf("stale alias deleted its replacement: removed=%d err=%v", n, err)
	}
	got, err := index.Get("c1", "cam1", replacement.URI)
	if err != nil || got.InitURI != replacement.InitURI {
		t.Fatalf("replacement lost: %+v %v", got, err)
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		names := tx.Bucket(objectsBucket).Bucket(partition("c1", "cam1", old.Start))
		if names.Get([]byte(old.URI)) != nil || names.Get([]byte(replacement.URI)) == nil {
			return errors.New("stale alias or replacement name is inconsistent")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type qaScanStore struct {
	storage.MediaStore
	list func(context.Context, string, string, int) (storage.Page, error)
	head func(context.Context, string) (storage.ObjectInfo, error)
}

func (s *qaScanStore) List(ctx context.Context, prefix, after string, limit int) (storage.Page, error) {
	if s.list != nil {
		return s.list(ctx, prefix, after, limit)
	}
	return s.MediaStore.List(ctx, prefix, after, limit)
}

func (s *qaScanStore) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if s.head != nil {
		return s.head(ctx, key)
	}
	return s.MediaStore.Head(ctx, key)
}

func TestQAReconcileKeepsWritesCommittedDuringListing(t *testing.T) {
	for _, reupload := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-name-reupload=%t", reupload), func(t *testing.T) {
			index, fs := scannerFixture(t)
			day := midnight(time.Now())
			record := qaRecording(1, day.Add(time.Hour), time.Second)
			if reupload {
				record = recordingFixture(t, fs, 1, record.Start, time.Second)
				if err := index.Put([]Segment{record}); err != nil {
					t.Fatal(err)
				}
				qaIndexedAt(t, index, record, time.Now().Add(-2*time.Minute))
				if err := fs.Delete(context.Background(), record.Key); err != nil {
					t.Fatal(err)
				}
			}
			listed, release := make(chan struct{}), make(chan struct{})
			store := &qaScanStore{MediaStore: fs}
			store.list = func(ctx context.Context, prefix, after string, limit int) (storage.Page, error) {
				page, err := fs.List(ctx, prefix, after, limit)
				if err != nil {
					return page, err
				}
				close(listed)
				select {
				case <-release:
					return page, nil
				case <-ctx.Done():
					return storage.Page{}, ctx.Err()
				}
			}
			scanner := &Scanner{Index: index, Store: store, Prefix: "archive", Days: 1,
				Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- scanner.ScanDay(ctx, config.StaticCamera{CenterID: "c1", CameraID: "cam1"}, day)
			}()
			select {
			case <-listed:
			case <-ctx.Done():
				t.Fatal("scanner did not reach LIST")
			}
			record = recordingFixture(t, fs, 1, record.Start, time.Second)
			if err := index.Put([]Segment{record}); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, err := index.Get("c1", "cam1", record.URI); err != nil {
				t.Fatalf("record committed after LIST was removed: %v", err)
			}
			if status := scanner.Status(); status.Removed != 0 {
				t.Fatalf("live write counted as removed: %+v", status)
			}
		})
	}
}

func TestQAScanDaySavesProgressWhenHeadFailureCoincidesWithCancellation(t *testing.T) {
	injected := errors.New("HEAD transport failure at the deadline")
	for _, mode := range []string{"page-deadline", "store-error-at-deadline", "parent-cancel"} {
		t.Run(mode, func(t *testing.T) {
			index, fs := scannerFixture(t)
			day := midnight(time.Now()).AddDate(0, 0, -3)
			records := make([]Segment, 4)
			for n := range records {
				records[n] = recordingFixture(t, fs, uint64(n+1), day.Add(time.Duration(n+1)*time.Second), time.Second)
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			headCalls := make(map[string]int)
			store := &qaScanStore{MediaStore: fs}
			store.head = func(ctx context.Context, key string) (storage.ObjectInfo, error) {
				headCalls[key]++
				if key == records[1].Key {
					if err := fs.Delete(context.Background(), key); err != nil {
						return storage.ObjectInfo{}, err
					}
				}
				if key == records[2].Key && headCalls[key] == 1 {
					if mode == "parent-cancel" {
						cancel()
					}
					<-ctx.Done()
					if mode == "store-error-at-deadline" {
						return storage.ObjectInfo{}, injected
					}
					return storage.ObjectInfo{}, fmt.Errorf("HEAD interrupted: %w", ctx.Err())
				}
				return fs.Head(ctx, key)
			}
			scanner := &Scanner{Index: index, Store: store, Prefix: "archive", ScanTimeout: time.Second,
				Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			cam := config.StaticCamera{CenterID: "c1", CameraID: "cam1"}
			err := scanner.ScanDay(parent, cam, day)
			switch mode {
			case "page-deadline":
				if err != nil {
					t.Fatalf("page deadline discarded successful progress: %v", err)
				}
			case "store-error-at-deadline":
				if !errors.Is(err, injected) {
					t.Fatalf("original store error was hidden: %v", err)
				}
			case "parent-cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("parent cancellation was hidden: %v", err)
				}
			}
			state, err := index.ScanState("c1", "cam1", day)
			if err != nil || state.Cursor != records[1].Key || state.Complete || countIndexed(t, index, day) != 1 {
				t.Fatalf("partial progress: state=%+v err=%v indexed=%d", state, err, countIndexed(t, index, day))
			}
			if err := scanner.ScanDay(context.Background(), cam, day); err != nil {
				t.Fatalf("resume failed: %v", err)
			}
			state, err = index.ScanState("c1", "cam1", day)
			if err != nil || !state.Complete || countIndexed(t, index, day) != 3 ||
				headCalls[records[0].Key] != 1 || headCalls[records[2].Key] != 2 {
				t.Fatalf("resume repeated or lost work: state=%+v err=%v indexed=%d heads=%v",
					state, err, countIndexed(t, index, day), headCalls)
			}
		})
	}
}

func TestQAScanDayForeignNamesAtThePageBoundary(t *testing.T) {
	index, fs := scannerFixture(t)
	day := midnight(time.Now()).AddDate(0, 0, -3)
	first := recordingFixture(t, fs, 1, day.Add(time.Hour), time.Second)
	missing := qaRecording(2, day.Add(2*time.Hour), time.Second)
	last := recordingFixture(t, fs, 3, day.Add(3*time.Hour), time.Second)
	records := []Segment{first, missing, last}
	if err := index.Put(records); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		qaIndexedAt(t, index, record, time.Now().Add(-2*time.Minute))
	}
	base := "archive/c1/cam1/" + day.Format("2006/01/02") + "/"
	for n := range 255 {
		key := base + fmt.Sprintf("000-foreign-%03d", n)
		if _, err := fs.Put(context.Background(), storage.Object{Key: key, Body: []byte("unrelated")}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"한글-녹화-😀.ts", strings.Repeat("x", 230) + ".ts",
		fmt.Sprintf("seg-18446744073709551616-%d.ts", last.Start.UnixMilli())} {
		if _, err := fs.Put(context.Background(), storage.Object{Key: base + name, Body: []byte("unrelated")}); err != nil {
			t.Fatal(err)
		}
	}
	heads := 0
	store := &qaScanStore{MediaStore: fs, head: func(context.Context, string) (storage.ObjectInfo, error) {
		heads++
		return storage.ObjectInfo{}, errors.New("foreign names and already indexed objects must not need HEAD")
	}}
	scanner := &Scanner{Index: index, Store: store, Prefix: "archive",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cam := config.StaticCamera{CenterID: "c1", CameraID: "cam1"}
	if err := scanner.ScanDay(context.Background(), cam, day); err != nil {
		t.Fatal(err)
	}
	state, err := index.ScanState("c1", "cam1", day)
	if err != nil || state.Cursor != first.Key || state.Complete {
		t.Fatalf("foreign keys broke the first page: state=%+v err=%v", state, err)
	}
	if _, err := index.Get("c1", "cam1", missing.URI); err != nil {
		t.Fatalf("unlisted object beyond the first page was removed prematurely: %v", err)
	}
	if err := scanner.ScanDay(context.Background(), cam, day); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Get("c1", "cam1", missing.URI); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing object in the second page's range survived: %v", err)
	}
	for _, record := range []Segment{first, last} {
		if _, err := index.Get("c1", "cam1", record.URI); err != nil {
			t.Fatalf("valid recording lost beside a foreign key: %s: %v", record.URI, err)
		}
	}
	state, err = index.ScanState("c1", "cam1", day)
	if err != nil || !state.Complete || heads != 0 || scanner.Status().Removed != 1 {
		t.Fatalf("foreign keys prevented completion: state=%+v err=%v heads=%d status=%+v",
			state, err, heads, scanner.Status())
	}
}

func TestQAPruneEmptyLegacyPartitionWithPendingMarker(t *testing.T) {
	index, _ := scannerFixture(t)
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	name := partition("c1", "cam1", day)
	// Removing the last recording from a legacy partition leaves an empty
	// bucket. Its first object-name access queues an empty pending backfill.
	if err := index.db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.Bucket(segmentsBucket).CreateBucket(name); err != nil {
			return err
		}
		_, err := objectNames(tx, name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(migrationsBucket).Get(name) == nil {
			return errors.New("fixture lacks a pending migration marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := index.Prune(day.Add(48*time.Hour), 64); err != nil || n != 1 {
		t.Fatalf("empty pending partition prune: count=%d err=%v", n, err)
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(migrationsBucket).Get(name) != nil || tx.Bucket(segmentsBucket).Bucket(name) != nil ||
			tx.Bucket(objectsBucket).Bucket(name) != nil {
			return errors.New("empty pending partition survived prune")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQAPruneOneDayWindowIsIndependentOfTimezone(t *testing.T) {
	for _, raw := range []string{
		"2026-10-04T23:59:59.999Z",
		"2026-10-05T00:00:00Z",
		"2026-10-05T00:00:00.001Z",
		"2026-10-05T00:59:59.999Z",
		"2026-10-05T01:00:00Z",
		"2026-03-09T06:30:00Z",
		"2026-11-02T06:30:00Z",
		"2024-03-01T00:00:00Z",
		"2017-01-01T00:00:00Z",
	} {
		now, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, zone := range []string{"UTC", "Asia/Seoul", "America/New_York", "Pacific/Kiritimati"} {
			t.Run(raw+"/"+zone, func(t *testing.T) {
				location, err := time.LoadLocation(zone)
				if err != nil {
					t.Fatal(err)
				}
				index, _ := scannerFixture(t)
				scanner := &Scanner{Index: index, Days: 1}
				horizon := scanner.horizon(now.In(location))
				wantHorizon := time.UnixMilli(now.UnixMilli() - 86400000).UTC()
				if !horizon.Equal(wantHorizon) {
					t.Fatalf("one day became a local calendar day: got=%v want=%v", horizon, wantHorizon)
				}
				crossing := qaRecording(1, wantHorizon.Add(-time.Millisecond).In(location), 2*time.Millisecond)
				previous := qaRecording(2, midnight(wantHorizon).Add(-time.Millisecond).In(location), time.Hour)
				expired := qaRecording(3, midnight(wantHorizon).Add(-48*time.Hour).In(location), time.Second)
				if err := index.Put([]Segment{crossing, previous, expired}); err != nil {
					t.Fatal(err)
				}
				if _, err := index.Prune(horizon.In(location), 64); err != nil {
					t.Fatal(err)
				}
				for _, record := range []Segment{crossing, previous, expired} {
					_, err := index.Get("c1", "cam1", record.URI)
					dayEnd := time.UnixMilli((record.Start.UnixMilli()/86400000 + 1) * 86400000)
					wantGone := !dayEnd.Add(time.Hour).After(wantHorizon)
					if wantGone && !errors.Is(err, storage.ErrNotFound) || !wantGone && err != nil {
						t.Errorf("UTC partition boundary: start=%v end=%v horizon=%v gone=%v get=%v",
							record.Start, record.End(), wantHorizon, wantGone, err)
					}
					if record.End().After(wantHorizon) && err != nil {
						t.Errorf("segment overlapping the window was removed: %+v %v", record, err)
					}
				}
			})
		}
	}
}
