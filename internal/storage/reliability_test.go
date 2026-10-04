package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFilesystemOverwriteClearsOldMetadata(t *testing.T) {
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "object"
	if _, err := store.Put(ctx, Object{Key: key, Body: []byte("old"), Metadata: map[string]string{"put-id": "old"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, Object{Key: key, Body: []byte("new")}); err != nil {
		t.Fatal(err)
	}
	info, err := store.Head(ctx, key)
	if err != nil || len(info.Metadata) != 0 {
		t.Fatalf("HEAD must not attribute a replacement to its old writer: %+v, %v", info, err)
	}
}

func TestFilesystemLockHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".transmux-cas.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, Object{Key: "lease", Body: []byte("owner"), Preconditions: Preconditions{IfNoneMatch: true}})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock wait did not preserve cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("filesystem lock outlived the request deadline")
	}
}

func TestConflictingPreconditionsAreRejected(t *testing.T) {
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Put(context.Background(), Object{
		Key: "object", Preconditions: Preconditions{IfNoneMatch: true, IfMatch: `"version"`},
	})
	if err == nil {
		t.Fatal("mutually exclusive preconditions accepted")
	}
}
