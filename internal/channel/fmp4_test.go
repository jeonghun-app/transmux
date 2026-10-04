package channel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/hls"
)

func TestFMP4InitIsStoredBeforeFragmentsAndCopiedAcrossMidnight(t *testing.T) {
	store := &fakeStore{}
	w := newTestWorker(t, store)
	gen := newDrainState()
	if err := os.WriteFile(filepath.Join(w.spoolDir, "init.mp4"), []byte("codec initialization"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.spoolDir, "seg-000000.m4s"), []byte("fragment"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(store.keys())
	seg := hls.LocalSegment{Name: "seg-000000.m4s", InitName: "init.mp4", Duration: 4 * time.Second,
		ProgramDateTime: time.Date(2026, 9, 11, 23, 59, 58, 0, time.UTC)}
	armFailure(store, func(key string) error {
		if strings.HasSuffix(key, ".mp4") {
			return errors.New("init unavailable")
		}
		return nil
	})
	if err := w.publishSegment(context.Background(), gen, seg); err == nil {
		t.Fatal("fragment published without init")
	}
	if len(store.keys()) != before {
		t.Fatal("a fragment landed before its init object")
	}
	armFailure(store, nil)
	if err := w.publishSegment(context.Background(), gen, seg); err != nil {
		t.Fatal(err)
	}
	firstInit := gen.initURI
	seg.ProgramDateTime = seg.ProgramDateTime.Add(4 * time.Second)
	if err := w.publishSegment(context.Background(), gen, seg); err != nil {
		t.Fatal(err)
	}
	if firstInit == gen.initURI || !strings.HasPrefix(gen.initURI, "2026/09/12/") {
		t.Fatal("midnight fragments depend on a previous capture day's init")
	}
	if err := w.publishManifest(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, err := hls.ParsePublished([]byte(store.lastManifest()))
	if err != nil {
		t.Fatal(err)
	}
	if len(published.Window) != 2 || published.Window[0].InitURI != firstInit || published.Window[1].InitURI != gen.initURI {
		t.Fatalf("recovery lost initialization maps: %#v", published)
	}
	for _, segment := range published.Window {
		info, err := store.Head(context.Background(), w.objectKey(segment.URI))
		if err != nil || info.Metadata["init-uri"] != segment.InitURI {
			t.Fatalf("recording metadata lost initialization: %#v %v", info, err)
		}
	}
}
