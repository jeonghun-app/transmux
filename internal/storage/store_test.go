package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if err := s.Put(context.Background(), Object{Key: key, Body: []byte("payload")}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), key)
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
	_, err = s.Get(context.Background(), "c1/cam1/index.m3u8")
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
	if err := s.Put(context.Background(),
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
	if err := s.Put(context.Background(), Object{Key: key, Body: []byte("first")}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Put(context.Background(), Object{Key: key, Body: []byte("second-body")})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := s.Get(context.Background(), key)
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
