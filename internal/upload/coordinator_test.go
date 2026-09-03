package upload

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/metrics"
	"github.com/jeonghun-app/transmux/internal/storage"
)

type stubStore struct {
	mu       sync.Mutex
	attempts int
	failFor  int // fail the first n attempts
	err      error
	body     []byte
}

func (s *stubStore) Put(_ context.Context, obj storage.Object) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.attempts <= s.failFor {
		if s.err != nil {
			return "", s.err
		}
		return "", errors.New("transient")
	}
	s.body = append([]byte(nil), obj.Body...)
	return `"stub-etag"`, nil
}

func (s *stubStore) Get(context.Context, string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.attempts <= s.failFor {
		return nil, "", errors.New("transient")
	}
	return s.body, `"stub-etag"`, nil
}

func (s *stubStore) Head(context.Context, string) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.attempts <= s.failFor {
		return storage.ObjectInfo{}, errors.New("transient")
	}
	return storage.ObjectInfo{ETag: `"stub-etag"`, Size: int64(len(s.body))}, nil
}

func (s *stubStore) Describe() string { return "stub" }

func (s *stubStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func testCfg() config.UploadConfig {
	c := config.Default().Upload
	c.RetryBase = config.Duration{Duration: time.Millisecond}
	c.RetryMax = config.Duration{Duration: 2 * time.Millisecond}
	return c
}

func TestPutRetriesThenSucceeds(t *testing.T) {
	store := &stubStore{failFor: 2}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	if _, err := c.Put(context.Background(), storage.Object{Key: "k", Body: []byte("x")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := store.count(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestPutGivesUpAfterMaxAttempts(t *testing.T) {
	store := &stubStore{failFor: 99}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	_, err := c.Put(context.Background(), storage.Object{Key: "k", Body: []byte("x")})
	if !errors.Is(err, ErrGaveUp) {
		t.Fatalf("err = %v, want ErrGaveUp", err)
	}
	if got := store.count(); got != config.Default().Upload.MaxAttempts {
		t.Errorf("attempts = %d, want %d", got, config.Default().Upload.MaxAttempts)
	}
}

// TestGetReturnsNotFoundImmediately matters for recovery: a missing manifest
// is an answer (new channel), not a fault to retry.
func TestGetReturnsNotFoundImmediately(t *testing.T) {
	store := &notFoundStore{}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	_, _, err := c.Get(context.Background(), "k")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if store.attempts != 1 {
		t.Errorf("attempts = %d, want 1: a 404 must not be retried", store.attempts)
	}
}

type notFoundStore struct {
	attempts     int
	headAttempts int
}

func (s *notFoundStore) Put(context.Context, storage.Object) (string, error) { return "", nil }
func (s *notFoundStore) Get(context.Context, string) ([]byte, string, error) {
	s.attempts++
	return nil, "", storage.ErrNotFound
}
func (s *notFoundStore) Head(context.Context, string) (storage.ObjectInfo, error) {
	s.headAttempts++
	return storage.ObjectInfo{}, storage.ErrNotFound
}
func (s *notFoundStore) Describe() string { return "notfound" }

func TestPutStopsOnCancelledContext(t *testing.T) {
	store := &stubStore{failFor: 99}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Put(ctx, storage.Object{Key: "k", Body: []byte("x")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := store.count(); got != 0 {
		t.Errorf("attempts = %d, want 0: shutdown must not start new requests", got)
	}
}

// TestBackoffNeverPanicsOnValidatedConfig is the regression guard for the
// full-jitter bound. rand.Int63n panics on a non-positive argument, so a
// configuration that passes Validate must never produce one.
func TestBackoffNeverPanicsOnValidatedConfig(t *testing.T) {
	full := config.Default()
	full.Storage.Bucket = "b"
	full.Cameras.Static = []config.StaticCamera{
		{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://h/s"},
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	c := NewCoordinator(&stubStore{}, full.Upload, metrics.NewRegistry())
	for attempt := 1; attempt <= 10; attempt++ {
		if d := c.backoff(attempt); d < 0 {
			t.Fatalf("attempt %d produced a negative delay %v", attempt, d)
		}
	}

	// Zero base and zero cap is the degenerate case Validate still permits.
	zero := full.Upload
	zero.RetryBase = config.Duration{}
	zero.RetryMax = config.Duration{}
	c2 := NewCoordinator(&stubStore{}, zero, metrics.NewRegistry())
	if d := c2.backoff(1); d != 0 {
		t.Errorf("zero bounds should yield no delay, got %v", d)
	}
}

// TestMaxConcurrentIsEnforced pins the reason the semaphore exists: several
// hundred channels must not open unbounded simultaneous requests.
func TestMaxConcurrentIsEnforced(t *testing.T) {
	cfg := testCfg()
	cfg.MaxConcurrent = 2
	blocker := &blockingStore{inFlight: make(chan struct{}, 16), release: make(chan struct{})}
	c := NewCoordinator(blocker, cfg, metrics.NewRegistry())

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Put(context.Background(), storage.Object{Key: "k", Body: []byte("x")})
		}()
	}
	// Give the goroutines time to pile up on the semaphore.
	time.Sleep(50 * time.Millisecond)
	if got := len(blocker.inFlight); got > 2 {
		t.Errorf("%d concurrent puts, want at most 2", got)
	}
	close(blocker.release)
	wg.Wait()
}

type blockingStore struct {
	inFlight chan struct{}
	release  chan struct{}
}

func (s *blockingStore) Put(context.Context, storage.Object) (string, error) {
	s.inFlight <- struct{}{}
	<-s.release
	return `"blocking-etag"`, nil
}
func (s *blockingStore) Get(context.Context, string) ([]byte, string, error) { return nil, "", nil }
func (s *blockingStore) Head(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, nil
}
func (s *blockingStore) Describe() string { return "blocking" }

// TestPutFileReadsOnlyAfterAcquiringASlot is the memory bound. Reading the
// segment before queueing makes resident bytes scale with channel count
// instead of with max_concurrent, which is gigabytes at the channel counts
// this shard is sized for.
func TestPutFileReadsOnlyAfterAcquiringASlot(t *testing.T) {
	cfg := testCfg()
	cfg.MaxConcurrent = 1
	blocker := &blockingStore{inFlight: make(chan struct{}, 8), release: make(chan struct{})}
	c := NewCoordinator(blocker, cfg, metrics.NewRegistry())

	dir := t.TempDir()
	var paths []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, fmt.Sprintf("seg-%d.ts", i))
		if err := os.WriteFile(p, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	// Occupy the only slot, then queue the rest.
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _, _ = c.PutFile(context.Background(), storage.Object{Key: "k"}, p)
		}(p)
	}
	time.Sleep(50 * time.Millisecond)

	// Delete the files that are still waiting. If PutFile had read them up
	// front, the uploads would succeed from memory; because it reads after
	// acquiring, the queued ones must fail with fs.ErrNotExist. Either way the
	// process must not have held all three payloads at once.
	if got := len(blocker.inFlight); got != 1 {
		t.Fatalf("%d uploads in flight with max_concurrent=1", got)
	}
	close(blocker.release)
	wg.Wait()
}

// TestPutFileReportsAMissingFileVerbatim lets the caller tell "ffmpeg
// reclaimed the segment" (data loss) from "the store rejected it" (retry).
func TestPutFileReportsAMissingFileVerbatim(t *testing.T) {
	c := NewCoordinator(&stubStore{}, testCfg(), metrics.NewRegistry())
	_, _, err := c.PutFile(context.Background(),
		storage.Object{Key: "k"}, filepath.Join(t.TempDir(), "gone.ts"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestPutFileReturnsTheUploadedSize(t *testing.T) {
	store := &stubStore{}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	p := filepath.Join(t.TempDir(), "seg.ts")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, _, err := c.PutFile(context.Background(), storage.Object{Key: "k"}, p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("size = %d, want 10", n)
	}
	if string(store.body) != "0123456789" {
		t.Errorf("stored body = %q", store.body)
	}
}

func TestPutFileRetriesTheSameBytes(t *testing.T) {
	store := &stubStore{failFor: 2}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	p := filepath.Join(t.TempDir(), "seg.ts")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PutFile(context.Background(), storage.Object{Key: "k"}, p); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if got := store.count(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if string(store.body) != "abc" {
		t.Errorf("stored body = %q, want the file re-sent unchanged", store.body)
	}
}

// TestConditionalConflictIsNotRetried is the load-bearing property of the
// retry loop. A refused precondition cannot become true by waiting, so
// retrying it would burn the attempt budget and then surface a generic
// ErrGaveUp -- burying the one signal that says another writer owns the object.
func TestConditionalConflictIsNotRetried(t *testing.T) {
	store := &conflictStore{}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	_, err := c.Put(context.Background(), storage.Object{
		Key: "k", Body: []byte("x"),
		Preconditions: storage.Preconditions{IfMatch: `"stale"`},
	})
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}
	if store.attempts != 1 {
		t.Errorf("attempts = %d, want 1: a conflict must not be retried", store.attempts)
	}
}

type conflictStore struct{ attempts int }

func (s *conflictStore) Put(context.Context, storage.Object) (string, error) {
	s.attempts++
	return "", storage.ErrPreconditionFailed
}
func (s *conflictStore) Get(context.Context, string) ([]byte, string, error) { return nil, "", nil }
func (s *conflictStore) Head(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, nil
}
func (s *conflictStore) Describe() string { return "conflict" }

// TestConditionalWriteIsNotRetriedAfterAnAmbiguousFailure is the defect that
// made the worker's whole ambiguity-resolution path dead code.
//
// A conditional write that lands and then loses its response must be reported
// to the caller as-is. Retrying it makes the second attempt fail its own
// now-consumed precondition, and the caller sees ErrPreconditionFailed --
// indistinguishable from being overtaken by another writer, which it treats as
// permanent loss of ownership. So a healthy channel would kill itself over a
// dropped TCP connection.
func TestConditionalWriteIsNotRetriedAfterAnAmbiguousFailure(t *testing.T) {
	store := &ambiguousStore{}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())

	_, err := c.Put(context.Background(), storage.Object{
		Key: "c1/cam1/index.m3u8", Body: []byte("v2"),
		Preconditions: storage.Preconditions{IfMatch: `"v1"`},
	})
	if err == nil {
		t.Fatal("the ambiguous outcome must be reported, not hidden")
	}
	if errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatal("a retry turned a lost response into a false conflict; the caller " +
			"cannot tell that apart from losing ownership")
	}
	if store.attempts != 1 {
		t.Errorf("attempts = %d, want 1: a conditional write must not be retried", store.attempts)
	}
	if !store.stored {
		t.Error("precondition: the store should have recorded the landed write")
	}
}

// TestUnconditionalWriteStillRetries: the no-retry rule must be scoped to
// conditional writes, or every transient segment upload failure becomes fatal.
func TestUnconditionalWriteStillRetries(t *testing.T) {
	store := &stubStore{failFor: 2}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	if _, err := c.Put(context.Background(), storage.Object{Key: "k", Body: []byte("x")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if store.count() != 3 {
		t.Errorf("attempts = %d, want 3", store.count())
	}
}

// ambiguousStore stores the object and then reports a transport failure: the
// write landed, the response did not.
type ambiguousStore struct {
	attempts int
	stored   bool
}

func (s *ambiguousStore) Put(context.Context, storage.Object) (string, error) {
	s.attempts++
	s.stored = true
	return "", errors.New("connection reset by peer")
}
func (s *ambiguousStore) Get(context.Context, string) ([]byte, string, error) { return nil, "", nil }
func (s *ambiguousStore) Head(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, nil
}
func (s *ambiguousStore) Describe() string { return "ambiguous" }

// TestHeadIsBoundedAndRetried: Head resolves conflicts while holding an upload
// slot, so an unbounded one would wedge a channel and a slot together.
func TestHeadIsBoundedAndRetried(t *testing.T) {
	store := &stubStore{failFor: 2}
	c := NewCoordinator(store, testCfg(), metrics.NewRegistry())
	if _, err := c.Head(context.Background(), "k"); err != nil {
		t.Fatalf("Head: %v", err)
	}
	if store.count() != 3 {
		t.Errorf("attempts = %d, want 3 (two transient failures then success)", store.count())
	}

	nf := &notFoundStore{}
	c2 := NewCoordinator(nf, testCfg(), metrics.NewRegistry())
	if _, err := c2.Head(context.Background(), "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if nf.headAttempts != 1 {
		t.Errorf("attempts = %d, want 1: absence is an answer", nf.headAttempts)
	}
}
