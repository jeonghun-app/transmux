package playback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/storage"
)

// qaRetentionStore uses the real lexical-listing store and injects only
// ordinary store errors. The call cap makes a looping regression fail promptly.
type qaRetentionStore struct {
	storage.MediaStore
	lists   []string
	heads   []string
	deletes []string
	maxList int
	failOp  string
	failCam string
	cancel  context.CancelFunc
}

func (s *qaRetentionStore) List(ctx context.Context, prefix, after string, limit int) (storage.Page, error) {
	s.lists = append(s.lists, prefix)
	if len(s.lists) > s.maxList {
		return storage.Page{}, fmt.Errorf("retention exceeded %d list calls", s.maxList)
	}
	if s.failOp == "list" && strings.Contains(prefix, "/"+s.failCam+"/") {
		return storage.Page{}, errors.New("injected list failure")
	}
	return s.MediaStore.List(ctx, prefix, after, limit)
}

func (s *qaRetentionStore) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.heads = append(s.heads, key)
	if s.failOp == "head" && strings.Contains(key, "/"+s.failCam+"/") {
		return storage.ObjectInfo{}, errors.New("injected head failure")
	}
	return s.MediaStore.Head(ctx, key)
}

func (s *qaRetentionStore) Delete(ctx context.Context, key string) error {
	s.deletes = append(s.deletes, key)
	if s.failOp == "delete" && strings.Contains(key, "/"+s.failCam+"/") {
		return errors.New("injected delete failure")
	}
	err := s.MediaStore.Delete(ctx, key)
	if s.cancel != nil {
		s.cancel()
	}
	return err
}

func qaWatchRetention(f *fixture, cap int) *qaRetentionStore {
	s := &qaRetentionStore{MediaStore: f.store, maxList: cap}
	f.app.store = s
	return s
}

func TestQAAdversarialRetentionStoreFailuresDoNotStarveOtherCamera(t *testing.T) {
	for _, op := range []string{"list", "head", "delete"} {
		t.Run(op, func(t *testing.T) {
			f := newFixture(t)
			f.app.cfg.Retention.Days = 2
			f.app.cfg.Retention.Batch = 2
			old := time.Now().UTC().AddDate(0, 0, -8)
			expiredObjects(t, f, "c1", "cam1", 1, old)
			expiredObjects(t, f, "c2", "cam2", 1, old)
			s := qaWatchRetention(f, 3)
			s.failOp, s.failCam = op, "cam1"
			f.app.retentionPass(context.Background(), 0)
			if got := remaining(t, f, "c2", "cam2"); got != 0 {
				t.Fatalf("healthy camera has %d expired objects; lists=%v", got, s.lists)
			}
			if got := remaining(t, f, "c1", "cam1"); got != 1 {
				t.Fatalf("failed camera unexpectedly changed: %d", got)
			}
			if f.app.retention.Errors != 1 || f.app.retention.Deleted != 1 {
				t.Fatalf("status=%+v, lists=%v", f.app.retention, s.lists)
			}
			if len(s.lists) != 2 {
				t.Fatalf("expected one bounded turn per camera, lists=%v", s.lists)
			}
		})
	}
}

func TestQAAdversarialRetentionBatchOneRotatesAcrossPasses(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Retention.Days = 2
	f.app.cfg.Retention.Batch = 1
	old := time.Now().UTC().AddDate(0, 0, -8)
	expiredObjects(t, f, "c1", "cam1", 3, old)
	expiredObjects(t, f, "c2", "cam2", 3, old)
	s := qaWatchRetention(f, 7)
	next := 0
	for pass := 0; pass < 6; pass++ {
		next = f.app.retentionPass(context.Background(), next)
		want := "archive/c1/cam1/"
		if pass%2 == 1 {
			want = "archive/c2/cam2/"
		}
		if len(s.lists) != pass+1 || s.lists[pass] != want {
			t.Fatalf("pass %d: lists=%v, want %s", pass, s.lists, want)
		}
	}
	if remaining(t, f, "c1", "cam1") != 0 || remaining(t, f, "c2", "cam2") != 0 || f.app.retention.Deleted != 6 {
		t.Fatalf("unfair rotation: status=%+v, lists=%v", f.app.retention, s.lists)
	}
}

func TestQAAdversarialRetentionControlObjectsConsumeListedBudget(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Retention.Days = 2
	f.app.cfg.Retention.Batch = 1
	for _, key := range []string{"archive/c1/cam1/000-control", "archive/c1/cam1/001-live.m3u8"} {
		if _, err := f.store.Put(context.Background(), storage.Object{Key: key, Body: []byte("control")}); err != nil {
			t.Fatal(err)
		}
	}
	expiredObjects(t, f, "c2", "cam2", 1, time.Now().UTC().AddDate(0, 0, -8))
	s := qaWatchRetention(f, 4)
	next := f.app.retentionPass(context.Background(), 0)
	if len(s.lists) != 1 || len(s.heads) != 0 || len(s.deletes) != 0 || next != 1 {
		t.Fatalf("control object failed to spend one listed-object budget: lists=%v heads=%v deletes=%v next=%d", s.lists, s.heads, s.deletes, next)
	}
	next = f.app.retentionPass(context.Background(), next)
	if len(s.lists) != 2 || s.lists[1] != "archive/c2/cam2/" || remaining(t, f, "c2", "cam2") != 0 {
		t.Fatalf("second camera starved after control object: lists=%v next=%d", s.lists, next)
	}
}

func TestQAAdversarialRetentionCursorResetStopsUntilNextPass(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Retention.Days = 2
	f.app.cfg.Retention.Batch = 100
	expiredObjects(t, f, "c1", "cam1", 1, time.Now().UTC().AddDate(0, 0, -8))
	expiredObjects(t, f, "c2", "cam2", 1, time.Now().UTC().AddDate(0, 0, -8))
	// A valid current-day object ends cam1's expired prefix and resets its cursor.
	expiredObjects(t, f, "c1", "cam1", 1, time.Now().UTC().Truncate(24*time.Hour).Add(time.Hour))
	s := qaWatchRetention(f, 5)
	f.app.retentionPass(context.Background(), 0)
	if len(s.lists) != 2 || remaining(t, f, "c1", "cam1") != 1 || f.app.retention.Deleted != 2 || f.app.retention.Errors != 0 {
		t.Fatalf("cursor reset repeated sweep in one pass: status=%+v lists=%v", f.app.retention, s.lists)
	}
	for _, cam := range []string{"c1/cam1", "c2/cam2"} {
		cursor, err := f.index.Cursor("retention/" + cam)
		if err != nil || cursor != "" {
			t.Fatalf("cursor for %s = %q, %v", cam, cursor, err)
		}
	}
	f.app.retentionPass(context.Background(), 0)
	if len(s.lists) != 4 || f.app.retention.Deleted != 2 || f.app.retention.Errors != 0 {
		t.Fatalf("empty sweep did not terminate: status=%+v lists=%v", f.app.retention, s.lists)
	}
}

func TestQAAdversarialRetentionParentCancellationStopsNextCamera(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Retention.Days = 2
	f.app.cfg.Retention.Batch = 100
	old := time.Now().UTC().AddDate(0, 0, -8)
	expiredObjects(t, f, "c1", "cam1", 2, old)
	expiredObjects(t, f, "c2", "cam2", 1, old)
	s := qaWatchRetention(f, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel = cancel
	f.app.retentionPass(ctx, 0)
	if len(s.lists) != 1 || s.lists[0] != "archive/c1/cam1/" || remaining(t, f, "c2", "cam2") != 1 {
		t.Fatalf("parent cancellation did not stop next camera: lists=%v", s.lists)
	}
	if f.app.retention.Running {
		t.Fatal("retention remained marked running after cancellation")
	}
}
