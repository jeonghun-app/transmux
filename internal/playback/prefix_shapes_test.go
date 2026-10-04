package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// putAt stores a media segment the way ingest does, under base, without
// indexing it, so the test proves the scanner finds it under the same prefix.
func putAt(t *testing.T, f *fixture, base string, sequence uint64, pdt time.Time, body []byte) recording.Segment {
	t.Helper()
	pdt = time.UnixMilli(pdt.UnixMilli()).UTC()
	uri := path.Join(pdt.Format("2006/01/02"), fmt.Sprintf("seg-%09d-%d.ts", sequence, pdt.UnixMilli()))
	md := map[string]string{"center-id": "c1", "camera-id": "cam1", "sequence": strconv.FormatUint(sequence, 10),
		"pdt-ms": strconv.FormatInt(pdt.UnixMilli(), 10), "duration-ms": "4000", "discontinuity": "false", "init-uri": ""}
	if _, err := f.store.Put(context.Background(), storage.Object{Key: base + uri, Body: body, Metadata: md}); err != nil {
		t.Fatal(err)
	}
	return recording.Segment{URI: uri, Start: pdt, DurationMS: 4000, Sequence: sequence}
}

func loadIngestWithPrefix(t *testing.T, f *fixture, prefix string) config.Config {
	t.Helper()
	ingest := f.app.ingest
	ingest.Storage.KeyPrefix = prefix
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
	return loaded
}

// A stand-in for ffmpeg that concatenates the segments export downloaded, so
// the clip body proves which objects export read.
func fakeRemuxer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nfor out; do :; done\ncat \"$(dirname \"$out\")\"/segment-* > \"$out\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestKeyPrefixShapesShareOneNamespaceAcrossServices(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		base   string
	}{
		{"/a/b/", "a/b/c1/cam1/"},
		{"a/b", "a/b/c1/cam1/"},
		{"", "c1/cam1/"},
		{"/", "c1/cam1/"},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			loaded := loadIngestWithPrefix(t, f, tc.prefix)
			loaded.FFmpeg.Binary = fakeRemuxer(t)
			f.app.ingest = loaded
			f.app.scanner.Prefix = loaded.Storage.KeyPrefix
			f.app.cfg.Retention.Days = 2

			recent := putAt(t, f, tc.base, 2, time.Now().Add(-10*time.Second), []byte("recent media"))
			old := putAt(t, f, tc.base, 1, time.Now().AddDate(0, 0, -8), []byte("expired media"))
			for _, record := range []recording.Segment{recent, old} {
				if err := f.app.scanner.ScanDay(ctx, loaded.Cameras.Static[0], record.Start); err != nil {
					t.Fatal(err)
				}
				indexed, err := f.index.Get("c1", "cam1", record.URI)
				if err != nil || indexed.Key != tc.base+record.URI {
					t.Fatalf("index key = %+v, %v; want %s", indexed, err, tc.base+record.URI)
				}
			}
			manifest := hls.RenderLive(hls.Live{Segments: []hls.PublishedSegment{{
				Sequence: recent.Sequence, URI: recent.URI, ProgramDateTime: recent.Start, Duration: 4 * time.Second,
			}}})
			if _, err := f.store.Put(ctx, storage.Object{Key: tc.base + "index.m3u8", Body: manifest}); err != nil {
				t.Fatal(err)
			}

			token := f.token(t, allPermissions())
			for _, mode := range []string{"live", "recording"} {
				resp, body := f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{
					CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: mode, Start: recent.Start, End: recent.End(),
				}, nil)
				if resp.StatusCode != http.StatusCreated {
					t.Fatalf("%s session: %d %s", mode, resp.StatusCode, body)
				}
				url := streamURL(t, body)
				resp, body = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+recent.URI, "", nil, nil)
				if resp.StatusCode != http.StatusOK || string(body) != "recent media" {
					t.Fatalf("%s media: %d %s", mode, resp.StatusCode, body)
				}
			}

			resp, body := f.request(t, "POST", "/v1/exports", token,
				exportRequest{CenterID: "c1", CameraID: "cam1", Start: recent.Start, End: recent.End()}, nil)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("start export: %d %s", resp.StatusCode, body)
			}
			var started struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(body, &started); err != nil {
				t.Fatal(err)
			}
			var job struct {
				State string `json:"state"`
				URL   string `json:"url"`
				Error string `json:"error"`
			}
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				_, body = f.request(t, "GET", "/v1/exports/"+started.ID, token, nil, nil)
				if err := json.Unmarshal(body, &job); err != nil {
					t.Fatal(err)
				}
				if job.State == "ready" || job.State == "failed" {
					break
				}
			}
			if job.State != "ready" {
				t.Fatalf("export under prefix %q: %s", tc.prefix, body)
			}
			resp, body = f.request(t, "GET", job.URL, "", nil, nil)
			if resp.StatusCode != http.StatusOK || string(body) != "recent media" {
				t.Fatalf("export read the wrong objects: %d %q", resp.StatusCode, body)
			}

			f.app.retentionPass(ctx, 0)
			if _, err := f.store.Head(ctx, tc.base+old.URI); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("retention kept expired media under %s: %v", tc.base, err)
			}
			if _, err := f.store.Head(ctx, tc.base+recent.URI); err != nil {
				t.Fatalf("retention deleted recent media: %v", err)
			}
		})
	}
}

// assertDisabledArchive checks that a disabled cam1 keeps its recordings
// playable and retained while live viewing is refused.
func assertDisabledArchive(t *testing.T, f *fixture, recent, old recording.Segment) {
	t.Helper()
	ctx := context.Background()
	token := f.token(t, allPermissions())
	resp, body := f.request(t, "GET", "/v1/cameras", token, nil, nil)
	var catalog struct {
		Cameras []CameraView `json:"cameras"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog: %d %s %v", resp.StatusCode, body, err)
	}
	found := false
	for _, cam := range catalog.Cameras {
		if cam.CenterID == "c1" && cam.CameraID == "cam1" {
			found = true
			if cam.Enabled {
				t.Fatalf("disabled camera reported as enabled: %+v", cam)
			}
		}
	}
	if !found {
		t.Fatalf("disabled camera left the catalog: %s", body)
	}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{
		CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "recording", Start: recent.Start, End: recent.End(),
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("recording session for disabled camera: %d %s", resp.StatusCode, body)
	}
	url := streamURL(t, body)
	resp, body = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+recent.URI, "", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "retained media" {
		t.Fatalf("disabled camera media: %d %s", resp.StatusCode, body)
	}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", token,
		sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "live"}, nil)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte(`"camera_disabled"`)) {
		t.Fatalf("live session for disabled camera: %d %s", resp.StatusCode, body)
	}
	f.app.cfg.Retention.Days = 2
	f.app.retentionPass(ctx, 0)
	if _, err := f.store.Head(ctx, old.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("retention skipped the disabled camera: %v", err)
	}
	if _, err := f.store.Head(ctx, recent.Key); err != nil {
		t.Fatalf("retention deleted recent disabled-camera media: %v", err)
	}
}

func TestStaticDisabledCameraKeepsArchive(t *testing.T) {
	f := newFixture(t)
	disabled := false
	cfg := config.CameraConfig{Provider: "static", Static: []config.StaticCamera{
		{CenterID: "c1", CameraID: "cam1", RTSPURL: "rtsp://camera/live", Enabled: &disabled},
		{CenterID: "c2", CameraID: "cam2", RTSPURL: "rtsp://camera2/live"},
	}}
	provider, err := camera.NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cams, err := provider.Cameras(context.Background())
	if err != nil || len(cams) != 1 || cams[0].Key() != "c2/cam2" {
		t.Fatalf("static ingest assignment = %+v, %v", cams, err)
	}
	f.app.provider, f.app.managed, f.app.ingest.Cameras = provider, nil, cfg
	f.app.invalidateRoster()
	recent := f.segment(t, 2, time.Now().Add(-time.Minute), 4*time.Second, []byte("retained media"), "")
	old := f.segment(t, 1, time.Now().AddDate(0, 0, -8), 4*time.Second, []byte("expired media"), "")
	assertDisabledArchive(t, f, recent, old)
}

func TestObjectDisabledCameraKeepsArchive(t *testing.T) {
	f := newFixture(t)
	token := f.token(t, allPermissions())
	resp, _ := f.request(t, "GET", "/v1/admin/cameras", token, nil, nil)
	resp, body := f.request(t, "DELETE", "/v1/admin/cameras/c1/cam1", token, nil,
		map[string]string{"If-Match": resp.Header.Get("ETag")})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable: %d %s", resp.StatusCode, body)
	}
	cams, err := f.app.provider.Cameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cam := range cams {
		if cam.Key() == "c1/cam1" {
			t.Fatal("disabled object-roster camera still assigned to ingest")
		}
	}
	recent := f.segment(t, 2, time.Now().Add(-time.Minute), 4*time.Second, []byte("retained media"), "")
	old := f.segment(t, 1, time.Now().AddDate(0, 0, -8), 4*time.Second, []byte("expired media"), "")
	assertDisabledArchive(t, f, recent, old)
}
