package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestValidateKeyRejectsEscapes is a security control, not a tidiness check:
// object keys are built from center and camera identifiers and are turned into
// filesystem paths by the filesystem backend.
func TestValidateKeyRejectsEscapes(t *testing.T) {
	bad := []string{
		"",
		"/absolute/key.ts",
		"a//b.ts",
		"../outside.ts",
		"a/../../outside.ts",
		"a/./b.ts",
		"a/..",
	}
	for _, key := range bad {
		if err := validateKey(key); err == nil {
			t.Errorf("validateKey(%q) = nil, want an error", key)
		}
	}
	good := []string{
		"c1/cam1/index.m3u8",
		"prefix/c1/cam1/2026/08/31/seg-000000001-1788198187331.ts",
		"single.ts",
	}
	for _, key := range good {
		if err := validateKey(key); err != nil {
			t.Errorf("validateKey(%q) = %v, want nil", key, err)
		}
	}
}

func TestFilesystemStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := "c1/cam1/2026/08/31/seg-000000001-1.ts"
	if _, err := s.Put(context.Background(), Object{Key: key, Body: []byte("payload")}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("body = %q", got)
	}
	// The nested key must have become a real directory tree under the root.
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key))); err != nil {
		t.Errorf("object not on disk: %v", err)
	}
}

// TestFilesystemStoreGetMissingIsErrNotFound matters for recovery: a missing
// manifest means "new channel", while any other error means the history is
// unknown and the channel must fail closed rather than start from zero.
func TestFilesystemStoreGetMissingIsErrNotFound(t *testing.T) {
	s, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Get(context.Background(), "c1/cam1/index.m3u8")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFilesystemStoreRejectsAnUnsafeKey(t *testing.T) {
	root := t.TempDir()
	s, err := NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(),
		Object{Key: "../escaped.ts", Body: []byte("x")}); err == nil {
		t.Fatal("expected the unsafe key to be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.ts")); err == nil {
		t.Fatal("a write escaped the storage root")
	}
}

// TestFilesystemStorePutIsAtomic covers the manifest overwrite. A reader must
// never observe a half-written playlist, which is why Put renames into place.
func TestFilesystemStorePutIsAtomic(t *testing.T) {
	root := t.TempDir()
	s, err := NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := "c1/cam1/index.m3u8"
	if _, err := s.Put(context.Background(), Object{Key: key, Body: []byte("first")}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Put(context.Background(), Object{Key: key, Body: []byte("second-body")})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _, err := s.Get(context.Background(), key)
			if err != nil {
				return
			}
			if v := string(body); v != "first" && v != "second-body" {
				t.Errorf("observed a torn write: %q", v)
			}
		}()
	}
	wg.Wait()

	// No temporary files may be left behind.
	entries, err := os.ReadDir(filepath.Join(root, "c1", "cam1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".put-") {
			t.Errorf("temporary file left on disk: %s", e.Name())
		}
	}
}

func TestMetadataKeysAreSorted(t *testing.T) {
	got := MetadataKeys(map[string]string{"sequence": "1", "center-id": "c", "pdt-ms": "2"})
	want := []string{"center-id", "pdt-ms", "sequence"}
	if len(got) != len(want) {
		t.Fatalf("MetadataKeys = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MetadataKeys = %v, want %v", got, want)
			break
		}
	}
}

func TestFilesystemStoreDescribeCarriesNoCredentials(t *testing.T) {
	s, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Describe(), "filesystem:") {
		t.Errorf("Describe = %q", s.Describe())
	}
}

// TestConditionalCreateAdmitsOneWriter is the primitive the whole ownership
// protocol rests on. If the backend waved the precondition through, every
// fencing test elsewhere would pass for the wrong reason.
func TestConditionalCreateAdmitsOneWriter(t *testing.T) {
	s, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "c1/cam1/_transmux/lease.json"
	create := Object{Key: key, Body: []byte("owner A"),
		Preconditions: Preconditions{IfNoneMatch: true}}

	etag, err := s.Put(context.Background(), create)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if etag == "" {
		t.Error("a successful write must return a version token")
	}

	create.Body = []byte("owner B")
	if _, err := s.Put(context.Background(), create); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("second create: err = %v, want ErrPreconditionFailed", err)
	}
	body, _, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "owner A" {
		t.Errorf("the loser must not have overwritten the winner, got %q", body)
	}
}

// TestCompareAndSwapRejectsAStaleToken is how a renewal, and a manifest write,
// discover that ownership moved.
func TestCompareAndSwapRejectsAStaleToken(t *testing.T) {
	s, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "c1/cam1/index.m3u8"
	first, err := s.Put(context.Background(), Object{Key: key, Body: []byte("v1")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(context.Background(), Object{Key: key, Body: []byte("v2"),
		Preconditions: Preconditions{IfMatch: first}})
	if err != nil {
		t.Fatalf("swap on the current version: %v", err)
	}
	if second == first {
		t.Error("a changed body must produce a changed version token")
	}
	// The holder of the old token is now fenced out.
	if _, err := s.Put(context.Background(), Object{Key: key, Body: []byte("v3"),
		Preconditions: Preconditions{IfMatch: first}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale swap: err = %v, want ErrPreconditionFailed", err)
	}
	// And swapping against an absent key is refused rather than creating it.
	if _, err := s.Put(context.Background(), Object{Key: "c1/cam1/absent.m3u8",
		Body: []byte("x"), Preconditions: Preconditions{IfMatch: first}}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("swap against an absent key: err = %v, want ErrPreconditionFailed", err)
	}
}

// TestConditionalWritesAreSerialisedAcrossInstances: two store instances on one
// root stand in for two daemons against one bucket. An in-process mutex would
// not arbitrate that, and the test would pass for the wrong reason.
func TestConditionalWritesAreSerialisedAcrossInstances(t *testing.T) {
	root := t.TempDir()
	a, err := NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}

	key := "c1/cam1/_transmux/lease.json"
	var wins int32
	var wg sync.WaitGroup
	for _, s := range []*FilesystemStore{a, b} {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(s *FilesystemStore) {
				defer wg.Done()
				if _, err := s.Put(context.Background(), Object{
					Key: key, Body: []byte("claim"),
					Preconditions: Preconditions{IfNoneMatch: true},
				}); err == nil {
					atomic.AddInt32(&wins, 1)
				}
			}(s)
		}
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d writers won a create-only race, want exactly 1", wins)
	}
}

// TestHeadReportsMetadata is how an ambiguous segment write is resolved: the
// stored put-id says whether the object is ours.
func TestHeadReportsMetadata(t *testing.T) {
	s, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "c1/cam1/2026/08/31/seg-000000001-1.ts"
	if _, err := s.Put(context.Background(), Object{
		Key: key, Body: []byte("payload"),
		Metadata: map[string]string{"put-id": "session:1", "sequence": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	info, err := s.Head(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Metadata["put-id"] != "session:1" {
		t.Errorf("put-id = %q", info.Metadata["put-id"])
	}
	if info.Size != int64(len("payload")) {
		t.Errorf("size = %d", info.Size)
	}
	if info.ETag == "" {
		t.Error("Head must report a version token")
	}
	if _, err := s.Head(context.Background(), "c1/cam1/absent.ts"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Head of an absent key: err = %v, want ErrNotFound", err)
	}
}
