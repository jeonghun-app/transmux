package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMediaStreamingRangesAndConfinement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewFilesystemStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, Object{Key: "c1/cam1/clip.ts", Body: []byte("0123456789"),
		Metadata: map[string]string{"duration-ms": "4000"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ rangeValue, body, contentRange string }{
		{"", "0123456789", ""}, {"bytes=2-5", "2345", "bytes 2-5/10"},
		{"bytes=8-", "89", "bytes 8-9/10"}, {"bytes=-3", "789", "bytes 7-9/10"},
		{"bytes=4-99", "456789", "bytes 4-9/10"}, {"bytes=-99", "0123456789", "bytes 0-9/10"},
	} {
		t.Run(tc.rangeValue, func(t *testing.T) {
			stream, err := store.Open(ctx, "c1/cam1/clip.ts", tc.rangeValue)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			body, err := io.ReadAll(stream.Body)
			if err != nil || string(body) != tc.body || stream.ContentRange != tc.contentRange || stream.Size != int64(len(body)) {
				t.Fatalf("wrong range: body=%q header=%q size=%d err=%v", body, stream.ContentRange, stream.Size, err)
			}
		})
	}
	for _, value := range []string{"bytes=10-", "bytes=6-2", "bytes=-0", "bytes=0-1,3-4", "bytes=+1-2", "items=1-2", "bytes=--", "bytes=999999999999999999999999-"} {
		if _, err := store.Open(ctx, "c1/cam1/clip.ts", value); !errors.Is(err, ErrRange) {
			t.Fatalf("range %q must fail: %v", value, err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "c1", "cam1", "leak.ts")); err != nil {
		t.Fatal(err)
	}
	if stream, err := store.Open(ctx, "c1/cam1/leak.ts", ""); err == nil {
		stream.Body.Close()
		t.Fatal("media open followed a symlink outside the store")
	}
	page, err := store.List(ctx, "c1/cam1/", "", 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Key != "c1/cam1/clip.ts" {
		t.Fatalf("listing exposed a sidecar or symlink: %#v, %v", page, err)
	}
	if err := store.Delete(ctx, "c1/cam1/clip.ts"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, "c1/cam1/clip.ts"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete did not remove object: %v", err)
	}
}
