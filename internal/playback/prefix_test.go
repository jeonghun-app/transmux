package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func TestLoadedKeyPrefixSharesPlaybackIndexAndRetentionNamespace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ingest := f.app.ingest
	ingest.Storage.KeyPrefix = "/tenant/video/"
	raw, err := json.Marshal(ingest)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "ingest.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	f.app.ingest = loaded
	f.app.scanner.Prefix = loaded.Storage.KeyPrefix
	f.app.cfg.Retention.Days = 2

	recent := f.segment(t, 2, time.Now().Add(-5*time.Second), 4*time.Second, []byte("recent media"), "")
	old := f.segment(t, 1, time.Now().AddDate(0, 0, -8), 4*time.Second, []byte("expired media"), "")
	const base = "tenant/video/c1/cam1/"
	for _, record := range []recording.Segment{recent, old} {
		body, _, err := f.store.Get(ctx, record.Key)
		if err != nil {
			t.Fatal(err)
		}
		info, err := f.store.Head(ctx, record.Key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.Put(ctx, storage.Object{Key: base + record.URI, Body: body, Metadata: info.Metadata}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Delete(ctx, record.Key); err != nil {
			t.Fatal(err)
		}
		if err := f.index.Remove(record); err != nil {
			t.Fatal(err)
		}
		if err := f.app.scanner.ScanDay(ctx, loaded.Cameras.Static[0], record.Start); err != nil {
			t.Fatal(err)
		}
		indexed, err := f.index.Get("c1", "cam1", record.URI)
		if err != nil || indexed.Key != base+record.URI {
			t.Fatalf("index did not find media under the loaded prefix: %+v, %v", indexed, err)
		}
	}
	manifest := hls.RenderLive(hls.Live{Segments: []hls.PublishedSegment{{
		Sequence: recent.Sequence, URI: recent.URI, ProgramDateTime: recent.Start, Duration: 4 * time.Second,
	}}})
	if _, err := f.store.Put(ctx, storage.Object{Key: base + "index.m3u8", Body: manifest}); err != nil {
		t.Fatal(err)
	}
	token := f.token(t, allPermissions())
	for _, mode := range []string{"live", "recording"} {
		resp, body := f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{
			CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: mode, Start: recent.Start, End: recent.End(),
		}, nil)
		if resp.StatusCode != 201 {
			t.Fatalf("%s session did not use the loaded prefix: %d %s", mode, resp.StatusCode, body)
		}
		url := streamURL(t, body)
		resp, body = f.request(t, "GET", url, "", nil, nil)
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte(recent.URI)) {
			t.Fatalf("%s manifest: %d %s", mode, resp.StatusCode, body)
		}
		resp, body = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+recent.URI, "", nil, nil)
		if resp.StatusCode != 200 || string(body) != "recent media" {
			t.Fatalf("%s media: %d %s", mode, resp.StatusCode, body)
		}
	}
	f.app.retentionPass(ctx, 0)
	if _, err := f.store.Head(ctx, base+old.URI); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("retention did not delete expired media under the loaded prefix: %v", err)
	}
	if _, err := f.store.Head(ctx, base+recent.URI); err != nil {
		t.Fatalf("retention deleted recent media: %v", err)
	}
}
