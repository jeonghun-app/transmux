package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func TestHTTPCameraDisablePreservesArchiveAuthorizationAndRetention(t *testing.T) {
	f := newFixture(t)
	var disabled atomic.Bool
	providerHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enabled := !disabled.Load()
		cam1 := map[string]any{"center_id": "c1", "camera_id": "cam1", "shard_id": "shard-0",
			"name": "Entrance", "enabled": enabled, "format": "fmp4", "video_codec": "hevc"}
		if enabled {
			cam1["rtsp_url"] = "rtsp://camera/live"
		}
		_ = json.NewEncoder(w).Encode([]any{
			cam1,
			config.StaticCamera{CenterID: "c1", CameraID: "cam3", ShardID: "shard-0", RTSPURL: "rtsp://camera3/live"},
			config.StaticCamera{CenterID: "c2", CameraID: "cam2", ShardID: "other", RTSPURL: "rtsp://camera2/live"},
		})
	}))
	defer providerHTTP.Close()
	cfg := config.CameraConfig{Provider: "http", URL: providerHTTP.URL, ShardFilter: "shard-0",
		RequestTimeout: config.Duration{Duration: time.Second}}
	provider, err := camera.NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.app.provider = provider
	f.app.managed = nil
	f.app.ingest.Cameras = cfg
	f.app.invalidateRoster()

	admin := f.token(t, allPermissions())
	resp, body := f.request(t, "GET", "/v1/cameras", admin, nil, nil)
	var initial struct {
		Cameras []CameraView `json:"cameras"`
	}
	if err := json.Unmarshal(body, &initial); err != nil || resp.StatusCode != http.StatusOK ||
		len(initial.Cameras) != 2 || initial.Cameras[0].CameraID != "cam1" || !initial.Cameras[0].Enabled ||
		initial.Cameras[1].CameraID != "cam3" {
		t.Fatalf("initial HTTP catalog: %d %s: %v", resp.StatusCode, body, err)
	}
	cams, err := provider.Cameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 2 || !reflect.DeepEqual([]string{cams[0].Key(), cams[1].Key()}, []string{"c1/cam1", "c1/cam3"}) {
		t.Fatalf("initial ingest assignment = %+v", cams)
	}

	now := time.Now().UTC()
	recentTime := now.Add(-time.Minute)
	initURI := recentTime.Format("2006/01/02") + "/init-" + strings.Repeat("a", 64) + ".mp4"
	if _, err := f.store.Put(context.Background(), storage.Object{Key: "archive/c1/cam1/" + initURI, Body: []byte("init")}); err != nil {
		t.Fatal(err)
	}
	recent := f.segment(t, 2, recentTime, 4*time.Second, []byte("retained fragment"), initURI)
	old := f.segment(t, 1, now.AddDate(0, 0, -8), 4*time.Second, []byte("expired fragment"), "")
	if err := f.index.Remove(old); err != nil {
		t.Fatal(err)
	}
	oldBody, _, err := f.store.Get(context.Background(), old.Key)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := f.store.Head(context.Background(), old.Key)
	if err != nil {
		t.Fatal(err)
	}
	otherKey := "archive/c2/cam2/" + old.URI
	otherMetadata := make(map[string]string, len(oldInfo.Metadata))
	for key, value := range oldInfo.Metadata {
		otherMetadata[key] = value
	}
	otherMetadata["center-id"], otherMetadata["camera-id"] = "c2", "cam2"
	if _, err := f.store.Put(context.Background(), storage.Object{Key: otherKey, Body: oldBody, Metadata: otherMetadata}); err != nil {
		t.Fatal(err)
	}

	disabled.Store(true)
	f.app.invalidateRoster()
	cams, err = provider.Cameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 1 || cams[0].Key() != "c1/cam3" {
		t.Fatalf("disabled or other-shard camera assigned to ingest: %+v", cams)
	}
	resp, body = f.request(t, "GET", "/v1/cameras", admin, nil, nil)
	var catalog struct {
		Cameras []CameraView `json:"cameras"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("disabled HTTP catalog: %d %s: %v", resp.StatusCode, body, err)
	}
	if len(catalog.Cameras) != 2 || catalog.Cameras[0].CameraID != "cam1" || catalog.Cameras[0].Enabled ||
		catalog.Cameras[0].Format != "fmp4" || catalog.Cameras[0].VideoCodec != "hevc" ||
		catalog.Cameras[0].LastRecorded == nil || catalog.Cameras[1].CameraID != "cam3" {
		t.Fatalf("disabled camera lost archive metadata: %+v", catalog.Cameras)
	}

	restricted := f.token(t, Grant{CenterID: "c2", CameraIDs: []string{"cam2"}, Permissions: []string{"recording"}})
	resp, body = f.request(t, "GET", "/v1/cameras", restricted, nil, nil)
	if resp.StatusCode != http.StatusOK || bytes.Contains(body, []byte(`"camera_id":"cam1"`)) {
		t.Fatalf("restricted catalog exposed disabled camera: %d %s", resp.StatusCode, body)
	}
	recordingReq := sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "recording",
		Start: recent.Start, End: recent.End()}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", restricted, recordingReq, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restricted user accessed disabled archive: %d %s", resp.StatusCode, body)
	}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", admin, recordingReq, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("new recording session after disable: %d %s", resp.StatusCode, body)
	}
	url := streamURL(t, body)
	resp, body = f.request(t, "GET", url, "", nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(recent.URI)) ||
		!bytes.Contains(body, []byte("#EXT-X-ENDLIST")) {
		t.Fatalf("disabled camera recording manifest: %d %s", resp.StatusCode, body)
	}
	resp, body = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+recent.URI, "", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "retained fragment" {
		t.Fatalf("disabled camera media: %d %s", resp.StatusCode, body)
	}
	resp, body = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+initURI, "", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "init" {
		t.Fatalf("disabled camera initialization: %d %s", resp.StatusCode, body)
	}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", admin,
		sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "live"}, nil)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte(`"camera_disabled"`)) {
		t.Fatalf("disabled camera live session: %d %s", resp.StatusCode, body)
	}

	f.app.cfg.Retention.Days = 2
	f.app.retentionPass(context.Background(), 0)
	if _, err := f.store.Head(context.Background(), old.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired disabled-camera media retained: %v", err)
	}
	if _, err := f.store.Head(context.Background(), otherKey); err != nil {
		t.Fatalf("expired other-shard media deleted: %v", err)
	}
	for _, key := range []string{recent.Key, "archive/c1/cam1/" + initURI} {
		if _, err := f.store.Head(context.Background(), key); err != nil {
			t.Fatalf("recent disabled-camera media deleted (%s): %v", key, err)
		}
	}
}
