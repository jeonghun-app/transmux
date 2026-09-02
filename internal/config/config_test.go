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

// valid returns a configuration that passes Validate, so each test below can
// break exactly one field.
func valid() Config {
	c := Default()
	c.Storage.Bucket = "b"
	c.Cameras.Static = []StaticCamera{{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://h/s"}}
	return c
}

// TestValidateRejectsStallTimeoutBelowGOPAllowance covers the gap between the
// watchdog and the health check. gapAllowance tolerates
// target_duration + max_gop_slack, so a stall timeout below that kills
// channels the rest of the daemon considers perfectly healthy.
func TestValidateRejectsStallTimeoutBelowGOPAllowance(t *testing.T) {
	c := valid()
	c.Segment.TargetDuration = Duration{4 * time.Second}
	c.Segment.MaxGOPSlack = Duration{10 * time.Second}
	c.FFmpeg.StallTimeout = Duration{5 * time.Second} // > target, < target+slack
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "stall_timeout") {
		t.Fatalf("want a stall_timeout error, got %v", err)
	}
}

func TestValidateRejectsStartupTimeoutBelowGOPAllowance(t *testing.T) {
	c := valid()
	c.FFmpeg.StartupTimeout = Duration{6 * time.Second}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "startup_timeout") {
		t.Fatalf("want a startup_timeout error, got %v", err)
	}
}

// TestValidateRejectsRetryBoundsThatPanic pins the reason this check exists:
// the full-jitter backoff feeds rand.Int63n, which panics on a non-positive
// bound. Configuration is the only place that can prevent it.
func TestValidateRejectsRetryBoundsThatPanic(t *testing.T) {
	cases := map[string]func(*Config){
		"negative retry_max": func(c *Config) { c.Upload.RetryMax = Duration{-1 * time.Second} },
		"negative retry_base": func(c *Config) {
			c.Upload.RetryBase = Duration{-1 * time.Second}
		},
		"retry_max below retry_base": func(c *Config) {
			c.Upload.RetryBase = Duration{5 * time.Second}
			c.Upload.RetryMax = Duration{1 * time.Second}
		},
	}
	for name, break_ := range cases {
		c := valid()
		break_(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestValidateRejectsNonPositiveDurations(t *testing.T) {
	cases := map[string]func(*Config){
		"put_timeout":    func(c *Config) { c.Upload.PutTimeout = Duration{} },
		"shutdown_grace": func(c *Config) { c.FFmpeg.ShutdownGrace = Duration{} },
		"scan_interval":  func(c *Config) { c.FFmpeg.ScanInterval = Duration{} },
		"reconnect.base": func(c *Config) { c.Reconnect.Base = Duration{} },
		"reset_after":    func(c *Config) { c.Reconnect.ResetAfter = Duration{} },
		"poll_interval":  func(c *Config) { c.Cameras.PollInterval = Duration{} },
		// A zero request timeout cancels every roster load before it starts.
		"request_timeout": func(c *Config) { c.Cameras.RequestTimeout = Duration{} },
		"max_gop_slack":   func(c *Config) { c.Segment.MaxGOPSlack = Duration{-time.Second} },
	}
	for name, break_ := range cases {
		c := valid()
		break_(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// TestValidateRejectsScanIntervalAboveTarget catches a config that validates
// on every individual field but cannot keep up with the spool.
func TestValidateRejectsScanIntervalAboveTarget(t *testing.T) {
	c := valid()
	c.FFmpeg.ScanInterval = Duration{10 * time.Second}
	if err := c.Validate(); err == nil {
		t.Fatal("a scan interval longer than a segment must be rejected")
	}
}

func TestValidateRejectsBadListenAddress(t *testing.T) {
	for _, addr := range []string{"", "8080", ":notaport", "1.2.3.4"} {
		c := valid()
		c.HTTPListen = addr
		if err := c.Validate(); err == nil {
			t.Errorf("http_listen %q: expected rejection", addr)
		}
	}
	c := valid()
	c.HTTPListen = "127.0.0.1:8080"
	if err := c.Validate(); err != nil {
		t.Errorf("loopback bind must be accepted: %v", err)
	}
}

// TestValidateRejectsManifestNameEscapingTheChannel guards the object layout:
// the manifest name is joined onto the per-channel prefix, so a separator
// would move the playlist out of its own namespace.
func TestValidateRejectsManifestNameEscapingTheChannel(t *testing.T) {
	for _, name := range []string{"a/b.m3u8", "..", ".", `dir\index.m3u8`} {
		c := valid()
		c.Storage.ManifestName = name
		if err := c.Validate(); err == nil {
			t.Errorf("manifest_name %q: expected rejection", name)
		}
	}
}

// TestLoadRejectsTrailingContent covers a file with a second JSON document
// appended, which the decoder would otherwise ignore entirely.
func TestLoadRejectsTrailingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	body := `{"shard_id":"a","storage":{"bucket":"b"},` +
		`"cameras":{"provider":"static","static":[{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://h/s"}]}}` +
		"\n{\"shard_id\":\"ignored-second-document\"}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected trailing content to be rejected")
	}
}

func TestPocConfigIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "poc.json"))
	if err != nil {
		t.Fatalf("the checked-in PoC config must validate: %v", err)
	}
	if cfg.ShardID == "" {
		t.Error("shard_id should be set by the PoC config")
	}
}

// TestValidateRejectsALeaseThatCannotSurviveAMissedRenewal: if the TTL does not
// leave room for two renewals plus the skew allowance, one lost response hands
// a healthy channel to another shard.
func TestValidateRejectsALeaseThatCannotSurviveAMissedRenewal(t *testing.T) {
	c := valid()
	c.Lease.TTL = Duration{20 * time.Second}
	c.Lease.RenewInterval = Duration{9 * time.Second}
	c.Lease.MaxClockSkew = Duration{2 * time.Second}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "lease.ttl") {
		t.Fatalf("want a lease.ttl error, got %v", err)
	}
}

func TestValidateRejectsALeaseOperationSlowerThanItsInterval(t *testing.T) {
	c := valid()
	c.Lease.OperationTimeout = Duration{30 * time.Second}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "lease.operation_timeout") {
		t.Fatalf("want a lease.operation_timeout error, got %v", err)
	}
}

func TestValidateRejectsNonPositiveLeaseDurations(t *testing.T) {
	cases := map[string]func(*Config){
		"ttl":               func(c *Config) { c.Lease.TTL = Duration{} },
		"renew_interval":    func(c *Config) { c.Lease.RenewInterval = Duration{} },
		"operation_timeout": func(c *Config) { c.Lease.OperationTimeout = Duration{} },
		"negative skew":     func(c *Config) { c.Lease.MaxClockSkew = Duration{-time.Second} },
	}
	for name, break_ := range cases {
		c := valid()
		break_(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("lease.%s: expected rejection", name)
		}
	}
}
