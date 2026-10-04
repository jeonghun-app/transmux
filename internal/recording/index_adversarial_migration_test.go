package recording

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/jeonghun-app/transmux/internal/storage"
)

func qaMigrationSegment(day time.Time, seq uint64) Segment {
	start := day.Add(time.Hour + time.Duration(seq)*time.Second)
	uri := fmt.Sprintf("%s/seg-%09d-%d.ts", day.Format("2006/01/02"), seq, start.UnixMilli())
	return Segment{CenterID: "c1", CameraID: "cam1", Key: "archive/c1/cam1/" + uri,
		URI: uri, Sequence: seq, Start: start, DurationMS: 1000, Size: 1}
}

// A legacy database has segment records but no objects-v1 partition. Insert
// those records directly so this fixture does not create media objects.
func qaInsertLegacy(t *testing.T, index *Index, day time.Time, records []Segment) {
	t.Helper()
	err := index.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.Bucket(segmentsBucket).CreateBucketIfNotExists(partition("c1", "cam1", day))
		if err != nil {
			return err
		}
		for _, s := range records {
			raw, err := json.Marshal(s)
			if err != nil {
				return err
			}
			if err := bucket.Put(recordKey(s.Start, s.Sequence), raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func qaMigrationState(t *testing.T, index *Index, day time.Time) (int, []byte) {
	t.Helper()
	var names int
	var progress []byte
	err := index.db.View(func(tx *bolt.Tx) error {
		name := partition("c1", "cam1", day)
		if bucket := tx.Bucket(objectsBucket).Bucket(name); bucket != nil {
			names = bucket.Stats().KeyN
		}
		progress = append([]byte(nil), tx.Bucket(migrationsBucket).Get(name)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names, progress
}

func TestQAAdversarialMigrationRestartAndWritesAcrossProgress(t *testing.T) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	filename := filepath.Join(t.TempDir(), "index.db")
	index, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]Segment, migrationBatch+2)
	for n := range records {
		records[n] = qaMigrationSegment(day, uint64(n+1))
	}
	qaInsertLegacy(t, index, day, records)
	// Exactly one migration batch commits before this context reports cancel.
	ctx := &cancelAfter{Context: context.Background(), allowed: 1}
	if removed, err := index.Reconcile(ctx, "c1", "cam1", day, "", "", nil, time.Now()); removed != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch reconciled partial names: removed=%d err=%v", removed, err)
	}
	if names, progress := qaMigrationState(t, index, day); names != migrationBatch || string(progress) != string(recordKey(records[migrationBatch-1].Start, records[migrationBatch-1].Sequence)) {
		t.Fatalf("first batch: names=%d progress=%x", names, progress)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	index, err = Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	// Delete legacy entries on both sides of progress, and add fresh entries
	// on both sides. Fresh unlisted writes must survive the old listing.
	for _, s := range []Segment{records[299], records[migrationBatch]} {
		if err := index.Remove(s); err != nil {
			t.Fatal(err)
		}
	}
	behind, ahead := qaMigrationSegment(day, 0), qaMigrationSegment(day, migrationBatch+3)
	if err := index.Put([]Segment{behind, ahead}); err != nil {
		t.Fatal(err)
	}
	if names, progress := qaMigrationState(t, index, day); names != migrationBatch+1 || len(progress) == 0 {
		t.Fatalf("live writes changed saved batch unexpectedly: names=%d progress=%x", names, progress)
	}
	// Cancellation at entry must preserve the saved batch and all records.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if removed, err := index.Reconcile(cancelled, "c1", "cam1", day, "", "", nil, time.Now()); removed != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("entry cancellation: removed=%d err=%v", removed, err)
	}
	if names, progress := qaMigrationState(t, index, day); names != migrationBatch+1 || len(progress) == 0 {
		t.Fatalf("entry cancellation changed batch: names=%d progress=%x", names, progress)
	}
	listed := make([]string, 0, len(records))
	for n, s := range records {
		if n != 299 && n != migrationBatch && n != 399 {
			listed = append(listed, s.URI)
		}
	}
	removed, err := index.Reconcile(context.Background(), "c1", "cam1", day, "", "", listed, time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("resumed reconcile: removed=%d err=%v", removed, err)
	}
	if names, progress := qaMigrationState(t, index, day); names != len(records)-3+2 || len(progress) != 0 {
		t.Fatalf("completed migration: names=%d progress=%x", names, progress)
	}
	for _, s := range []Segment{records[299], records[399], records[migrationBatch]} {
		if _, err := index.Get("c1", "cam1", s.URI); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("removed segment %s remains: %v", s.URI, err)
		}
	}
	for _, s := range []Segment{records[0], records[migrationBatch+1], behind, ahead} {
		if _, err := index.Get("c1", "cam1", s.URI); err != nil {
			t.Fatalf("surviving segment %s lost: %v", s.URI, err)
		}
	}
}

func TestQAAdversarialEmptyLegacyPartition(t *testing.T) {
	day := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	index, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	qaInsertLegacy(t, index, day, nil)
	if removed, err := index.Reconcile(context.Background(), "c1", "cam1", day, "", "", nil, time.Now()); err != nil || removed != 0 {
		t.Fatalf("empty legacy partition: removed=%d err=%v", removed, err)
	}
	if names, progress := qaMigrationState(t, index, day); names != 0 || len(progress) != 0 {
		t.Fatalf("empty migration left names or marker: names=%d progress=%x", names, progress)
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		name := partition("c1", "cam1", day)
		if tx.Bucket(objectsBucket).Bucket(name) == nil || tx.Bucket(migrationsBucket).Get(name) != nil {
			return errors.New("empty legacy partition did not finish migration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQAAdversarialPrunePendingMigration(t *testing.T) {
	day := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	index, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	records := make([]Segment, migrationBatch+1)
	for n := range records {
		records[n] = qaMigrationSegment(day, uint64(n+1))
	}
	qaInsertLegacy(t, index, day, records)
	if err := index.Put([]Segment{qaMigrationSegment(day, migrationBatch+2)}); err != nil {
		t.Fatal(err)
	}
	ctx := &cancelAfter{Context: context.Background(), allowed: 1}
	if removed, err := index.Reconcile(ctx, "c1", "cam1", day, "", "", nil, time.Now()); removed != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled migration before prune: removed=%d err=%v", removed, err)
	}
	if names, progress := qaMigrationState(t, index, day); names != migrationBatch+1 || len(progress) == 0 {
		t.Fatalf("pending migration before prune: names=%d progress=%x", names, progress)
	}
	name := partition("c1", "cam1", day)
	if err := index.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(migrationsBucket).Get(name) == nil {
			return errors.New("Put did not queue legacy partition")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := index.Prune(day.Add(48*time.Hour), 1); err != nil || count != 1 {
		t.Fatalf("prune pending migration: count=%d err=%v", count, err)
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(segmentsBucket).Bucket(name) != nil || tx.Bucket(objectsBucket).Bucket(name) != nil || tx.Bucket(migrationsBucket).Get(name) != nil {
			return errors.New("prune left partition or pending marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQAAdversarialSyntheticOrphanMarkerReconcile(t *testing.T) {
	// A marker without either partition cannot arise from Put, migration, or
	// Prune; inject it to exercise recovery from external DB corruption.
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	index, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	name := partition("c1", "cam1", day)
	if err := index.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(migrationsBucket).Put(name, []byte("orphan")) }); err != nil {
		t.Fatal(err)
	}
	if removed, err := index.Reconcile(context.Background(), "c1", "cam1", day, "", "", nil, time.Now()); removed != 0 || err != nil {
		t.Fatalf("orphan marker reconcile: removed=%d err=%v", removed, err)
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(migrationsBucket).Get(name) != nil {
			return errors.New("orphan marker survived reconcile")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
