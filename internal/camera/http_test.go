package camera

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
)

func TestValidationErrorDoesNotExposeMalformedURL(t *testing.T) {
	c := Camera{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://admin:secret-password@host/%xx?token=secret-token"}
	err := c.Validate()
	if err == nil {
		t.Fatal("malformed URL was accepted")
	}
	for _, secret := range []string{"secret-password", "secret-token", "admin"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("validation error leaked credentials: %v", err)
		}
	}
}

func TestHTTPProviderRejectsInvalidPayloadWithoutDroppingTheRoster(t *testing.T) {
	for name, payload := range map[string]string{
		"unknown field":          `[{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/live","enable":false}]`,
		"oversized":              "[]" + strings.Repeat(" ", 8<<20) + "garbage",
		"null":                   "null",
		"trailing content":       "[] {}",
		"invalid enabled camera": `[{"center_id":"c","camera_id":"cam"}]`,
		"invalid enabled shard":  `[{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/live","shard_id":"../shard"}]`,
		"duplicate enabled camera": `[{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/live"},` +
			`{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/other"}]`,
		"disabled then duplicate enabled camera": `[{"center_id":"c","camera_id":"cam","enabled":false},` +
			`{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/live"},` +
			`{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/other"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer srv.Close()
			p, err := NewProvider(config.CameraConfig{
				Provider: "http", URL: srv.URL, RequestTimeout: config.Duration{Duration: time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Cameras(context.Background()); err == nil {
				t.Fatal("invalid provider response must not become a successful roster")
			}
			if _, err := p.(*HTTPProvider).Load(context.Background()); err == nil {
				t.Fatal("invalid provider response must not become a successful catalog")
			}
		})
	}
}

func TestHTTPProviderAcceptsAnExplicitEmptyRoster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer example" {
			t.Error("provider authorization header missing")
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	p, err := NewProvider(config.CameraConfig{
		Provider: "http", URL: srv.URL, AuthHeader: "Bearer example",
		RequestTimeout: config.Duration{Duration: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	cams, err := p.Cameras(context.Background())
	if err != nil || len(cams) != 0 {
		t.Fatalf("explicit empty array must remove the roster: %v, %v", cams, err)
	}
	roster, err := p.(*HTTPProvider).Load(context.Background())
	if err != nil || len(roster) != 0 {
		t.Fatalf("explicit empty array must remove the catalog: %v, %v", roster, err)
	}
}

func TestHTTPProviderCatalogKeepsDisabledCamerasInAssignedShard(t *testing.T) {
	enabled, disabled := true, false
	roster := []config.StaticCamera{
		{CenterID: "c1", CameraID: "cam1", ShardID: "one", RTSPURL: "rtsp://camera/live"},
		{CenterID: "c1", CameraID: "cam2", ShardID: "one", RTSPURL: "rtsp://camera2/live",
			Name: "Archive", Enabled: &disabled, Audio: "copy", Format: "fmp4", VideoCodec: "hevc"},
		{CenterID: "c2", CameraID: "cam3", ShardID: "two", RTSPURL: "rtsp://camera3/live", Enabled: &enabled},
		{CenterID: "c2", CameraID: "cam4", ShardID: "two", Enabled: &disabled},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Error("provider must request JSON")
		}
		_ = json.NewEncoder(w).Encode(roster)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		shard   string
		keys    []string
		catalog []config.StaticCamera
	}{
		{"", []string{"c1/cam1", "c2/cam3"}, roster},
		{"one", []string{"c1/cam1"}, roster[:2]},
		{"two", []string{"c2/cam3"}, roster[2:]},
		{"missing", nil, []config.StaticCamera{}},
	} {
		t.Run(tc.shard, func(t *testing.T) {
			p, err := NewProvider(config.CameraConfig{Provider: "http", URL: srv.URL,
				RequestTimeout: config.Duration{Duration: time.Second}, ShardFilter: tc.shard})
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := p.(*HTTPProvider).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(catalog, tc.catalog) {
				t.Fatalf("catalog = %#v, want %#v", catalog, tc.catalog)
			}
			cams, err := p.Cameras(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, cam := range cams {
				keys = append(keys, cam.Key())
			}
			if !reflect.DeepEqual(keys, tc.keys) {
				t.Errorf("ingest cameras = %v, want %v", keys, tc.keys)
			}
		})
	}
}

func TestHTTPProviderKeepsIngestCompatibleWithDisabledEntries(t *testing.T) {
	disabled := false
	active := config.StaticCamera{CenterID: "c1", CameraID: "a", RTSPURL: "rtsp://h/a"}
	archived := config.StaticCamera{CenterID: "c1", CameraID: "b", Enabled: &disabled}
	removedSource := archived
	removedSource.RTSPURL, removedSource.Audio = "removed", "old-audio"
	removedSource.Format, removedSource.VideoCodec = "old-format", "old-codec"
	const activeJSON = `{"center_id":"c1","camera_id":"a","rtsp_url":"rtsp://h/a"}`
	const archivedJSON = `{"center_id":"c1","camera_id":"b","enabled":false}`
	const duplicateJSON = `{"center_id":"c1","camera_id":"a","rtsp_url":"rtsp://h/old","enabled":false}`
	for _, tc := range []struct {
		name    string
		payload string
		catalog []config.StaticCamera
	}{
		{"disabled without URL", "[" + activeJSON + "," + archivedJSON + "]", []config.StaticCamera{active, archived}},
		{"active before disabled duplicate", "[" + activeJSON + "," + duplicateJSON + "]", []config.StaticCamera{active}},
		{"active after disabled duplicate", "[" + duplicateJSON + "," + activeJSON + "]", []config.StaticCamera{active}},
		{"first disabled duplicate wins", "[" + activeJSON + "," + archivedJSON + "," +
			`{"center_id":"c1","camera_id":"b","name":"later","enabled":false}]`, []config.StaticCamera{active, archived}},
		{"invalid disabled identities", "[" + activeJSON + "," +
			`{"center_id":"../c1","camera_id":"b","enabled":false},` +
			`{"center_id":"c1","camera_id":"../b","enabled":false},` +
			`{"center_id":"c1","camera_id":"b","shard_id":"../shard","enabled":false},` +
			`{"center_id":"","camera_id":"b","enabled":false},` +
			`{"center_id":"c1","camera_id":"","enabled":false}]`, []config.StaticCamera{active}},
		{"disabled source metadata is not validated", "[" + activeJSON + "," +
			`{"center_id":"c1","camera_id":"b","enabled":false,"rtsp_url":"removed",` +
			`"audio":"old-audio","format":"old-format","video_codec":"old-codec"}]`, []config.StaticCamera{active, removedSource}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer srv.Close()
			p, err := NewProvider(config.CameraConfig{Provider: "http", URL: srv.URL,
				RequestTimeout: config.Duration{Duration: time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			cams, err := p.Cameras(context.Background())
			if err != nil || len(cams) != 1 || cams[0].Key() != "c1/a" || cams[0].RTSPURL != active.RTSPURL {
				t.Fatalf("disabled entries changed ingest: %+v, %v", cams, err)
			}
			catalog, err := p.(*HTTPProvider).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(catalog, tc.catalog) {
				t.Errorf("catalog = %#v, want %#v", catalog, tc.catalog)
			}
		})
	}
}

func TestHTTPProviderCatalogResolvesDuplicatesBeforeShardFiltering(t *testing.T) {
	disabled := false
	old := config.StaticCamera{CenterID: "c1", CameraID: "cam1", ShardID: "one", Enabled: &disabled}
	current := config.StaticCamera{CenterID: "c1", CameraID: "cam1", ShardID: "two", RTSPURL: "rtsp://camera/live"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]config.StaticCamera{old, current})
	}))
	defer srv.Close()
	for _, tc := range []struct {
		shard   string
		catalog []config.StaticCamera
	}{
		{"one", []config.StaticCamera{}},
		{"two", []config.StaticCamera{current}},
	} {
		t.Run(tc.shard, func(t *testing.T) {
			p, err := NewProvider(config.CameraConfig{Provider: "http", URL: srv.URL, ShardFilter: tc.shard,
				RequestTimeout: config.Duration{Duration: time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := p.(*HTTPProvider).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(catalog, tc.catalog) {
				t.Errorf("catalog after shard move = %#v, want %#v", catalog, tc.catalog)
			}
			cams, err := p.Cameras(context.Background())
			if err != nil || len(cams) != len(tc.catalog) {
				t.Fatalf("ingest assignment after shard move = %+v, %v", cams, err)
			}
		})
	}
}
