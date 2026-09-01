package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	c := Default()
	c.Storage.Bucket = "b"
	c.Cameras.Static = []StaticCamera{{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://h/s"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate once bucket and cameras are set: %v", err)
	}
}

func TestValidateRejectsSpoolTooSmallForUploadGrace(t *testing.T) {
	c := Default()
	c.Storage.Bucket = "b"
	c.Cameras.Static = []StaticCamera{{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://h/s"}}
	// local_list_size must exceed live_window, otherwise ffmpeg deletes a
	// segment while it is still in the published window and not yet uploaded.
	c.Segment.LiveWindow = 10
	c.Segment.LocalListSize = 10
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "local_list_size") {
		t.Fatalf("want a local_list_size error, got %v", err)
	}
}

func TestValidateRejectsStallTimeoutBelowSegmentLength(t *testing.T) {
	c := Default()
	c.Storage.Bucket = "b"
	c.Cameras.Static = []StaticCamera{{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://h/s"}}
	c.Segment.TargetDuration = Duration{6 * time.Second}
	c.FFmpeg.StallTimeout = Duration{5 * time.Second}
	if err := c.Validate(); err == nil {
		t.Fatal("a stall timeout shorter than one segment would kill healthy channels")
	}
}

func TestValidateRejectsBadTransport(t *testing.T) {
	c := Default()
	c.Storage.Bucket = "b"
	c.Cameras.Static = []StaticCamera{{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://h/s"}}
	c.FFmpeg.RTSPTransport = "sctp"
	if err := c.Validate(); err == nil {
		t.Fatal("expected an rtsp_transport error")
	}
}

func TestLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	body := map[string]any{
		"shard_id":     "shard-7",
		"max_channels": 50,
		"segment":      map[string]any{"target_duration": "6s", "live_window": 4, "local_list_size": 12},
		"storage":      map[string]any{"backend": "filesystem", "root": dir, "manifest_name": "index.m3u8"},
		"cameras": map[string]any{
			"provider": "static",
			"static":   []map[string]any{{"center_id": "c1", "camera_id": "cam1", "rtsp_url": "rtsp://h/s"}},
		},
	}
	raw, _ := json.Marshal(body)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ShardID != "shard-7" {
		t.Errorf("shard_id = %q", cfg.ShardID)
	}
	if cfg.Segment.TargetDuration.Duration != 6*time.Second {
		t.Errorf("target_duration = %v", cfg.Segment.TargetDuration.Duration)
	}
	// Fields absent from the file must keep their defaults.
	if cfg.Upload.MaxAttempts != 3 {
		t.Errorf("upload.max_attempts = %d, want the default 3", cfg.Upload.MaxAttempts)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	// A typo in a config key must fail loudly, not run with a silent default.
	if err := os.WriteFile(path, []byte(`{"shard_idd":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an unknown-field error")
	}
}
