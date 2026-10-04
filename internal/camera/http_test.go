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
		"unknown field":           `[{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/live","enable":false}]`,
		"oversized":               "[]" + strings.Repeat(" ", 8<<20) + "garbage",
		"null":                    "null",
		"trailing content":        "[] {}",
		"invalid disabled camera": `[{"center_id":"../c","camera_id":"cam","rtsp_url":"rtsp://host/live","enabled":false}]`,
		"duplicate disabled camera": `[{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/live"},` +
			`{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/other","enabled":false}]`,
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

func TestHTTPProviderCatalogKeepsDisabledCamerasAndOtherShards(t *testing.T) {
	enabled, disabled := true, false
	roster := []config.StaticCamera{
		{CenterID: "c1", CameraID: "cam1", ShardID: "one", RTSPURL: "rtsp://camera/live"},
		{CenterID: "c1", CameraID: "cam2", ShardID: "one", RTSPURL: "rtsp://camera2/live",
			Name: "Archive", Enabled: &disabled, Audio: "copy", Format: "fmp4", VideoCodec: "hevc"},
		{CenterID: "c2", CameraID: "cam3", ShardID: "two", RTSPURL: "rtsp://camera3/live", Enabled: &enabled},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Error("provider must request JSON")
		}
		_ = json.NewEncoder(w).Encode(roster)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		shard string
		keys  []string
	}{
		{"", []string{"c1/cam1", "c2/cam3"}},
		{"one", []string{"c1/cam1"}},
		{"two", []string{"c2/cam3"}},
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
			if !reflect.DeepEqual(catalog, roster) {
				t.Fatalf("catalog lost entries or metadata: %#v", catalog)
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
