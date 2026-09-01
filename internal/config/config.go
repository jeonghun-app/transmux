// Package config loads and validates transmuxd configuration.
//
// Configuration is JSON on purpose: the daemon deliberately keeps its
// third-party dependency surface to the AWS SDK only, so the standard
// library encoding/json is used instead of pulling in a YAML parser.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Duration wraps time.Duration so it can be expressed in JSON as "5s".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"5s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

// Config is the full daemon configuration.
type Config struct {
	// ShardID identifies this process in logs and metrics. It has no effect
	// on channel assignment; assignment comes from the camera provider.
	ShardID string `json:"shard_id"`

	// MaxChannels caps how many channels this process will run. Requests
	// beyond the cap are rejected so a mis-sized provider response cannot
	// silently push the shard past its verified capacity.
	MaxChannels int `json:"max_channels"`

	SpoolDir string `json:"spool_dir"`
	StateDir string `json:"state_dir"`

	HTTPListen string `json:"http_listen"`

	Segment  SegmentConfig  `json:"segment"`
	FFmpeg   FFmpegConfig   `json:"ffmpeg"`
	Storage  StorageConfig  `json:"storage"`
	Upload   UploadConfig   `json:"upload"`
	Reconnect ReconnectConfig `json:"reconnect"`
	Cameras  CameraConfig   `json:"cameras"`
}

type SegmentConfig struct {
	// TargetDuration is the -hls_time value. Actual segment length is
	// governed by the camera's IDR interval because we never transcode.
	TargetDuration Duration `json:"target_duration"`

	// LiveWindow is the number of segments kept in the published manifest.
	LiveWindow int `json:"live_window"`

	// LocalListSize is ffmpeg's -hls_list_size. It defines how long a
	// finished segment survives on the local spool before ffmpeg deletes
	// it, i.e. the upload grace period (LocalListSize * TargetDuration).
	LocalListSize int `json:"local_list_size"`

	// MaxGOPSlack is added to TargetDuration when deciding that a channel
	// has stopped producing segments. Cameras with a long IDR interval
	// legitimately emit segments slower than TargetDuration.
	MaxGOPSlack Duration `json:"max_gop_slack"`
}

type FFmpegConfig struct {
	Binary string `json:"binary"`

	// RTSPTransport is "tcp" or "udp". TCP is the default: it survives
	// container networking and firewalls without an RTP port range.
	RTSPTransport string `json:"rtsp_transport"`

	// InputArgs are extra args placed before -i. Kept configurable because
	// socket timeout flag names differ between ffmpeg releases.
	InputArgs []string `json:"input_args"`

	// StallTimeout kills ffmpeg when no segment has completed for this
	// long. This is a version-independent replacement for ffmpeg's own
	// socket timeout options and is the primary disconnect detector.
	StallTimeout Duration `json:"stall_timeout"`

	// StartupTimeout is the grace period for the very first segment.
	StartupTimeout Duration `json:"startup_timeout"`

	// ShutdownGrace is how long SIGTERM is given before SIGKILL.
	ShutdownGrace Duration `json:"shutdown_grace"`

	// ScanInterval is how often the spool is polled for finished segments.
	// Polling is used rather than inotify: it needs no watch descriptors,
	// cannot drop events, and doubles as crash recovery.
	ScanInterval Duration `json:"scan_interval"`
}

type StorageConfig struct {
	// Backend is "s3" or "filesystem". filesystem exists for tests.
	Backend string `json:"backend"`

	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Endpoint string `json:"endpoint"`
	// ForcePathStyle is required for MinIO.
	ForcePathStyle bool `json:"force_path_style"`

	// KeyPrefix is prepended to every object key.
	KeyPrefix string `json:"key_prefix"`

	// ManifestName is the object name of the live playlist, stored at
	// {prefix}/{center_id}/{camera_id}/{manifest_name}. It deliberately
	// sits above the date directory so the playback URL is stable across
	// midnight; segments still use the dated path from the spec.
	ManifestName string `json:"manifest_name"`

	// Root is the destination directory when Backend is "filesystem".
	Root string `json:"root"`
}

type UploadConfig struct {
	// MaxAttempts is the total number of PutObject tries per object.
	MaxAttempts int      `json:"max_attempts"`
	RetryBase   Duration `json:"retry_base"`
	RetryMax    Duration `json:"retry_max"`

	// MaxConcurrent bounds simultaneous PutObject calls across all
	// channels so a shard cannot exhaust sockets or file descriptors.
	MaxConcurrent int `json:"max_concurrent"`

	// PutTimeout is the per-attempt request timeout.
	PutTimeout Duration `json:"put_timeout"`
}

type ReconnectConfig struct {
	Base   Duration `json:"base"`
	Max    Duration `json:"max"`
	Factor float64  `json:"factor"`

	// ResetAfter is how long a channel must produce segments before the
	// backoff is considered recovered. Reconnecting alone is not success;
	// a camera that accepts RTSP then stalls must keep backing off.
	ResetAfter Duration `json:"reset_after"`
}

type CameraConfig struct {
	// Provider is "static" or "http".
	Provider string `json:"provider"`

	// Static is the inline camera list when Provider is "static".
	Static []StaticCamera `json:"static"`

	// URL is the endpoint returning the camera list when Provider is
	// "http". The response must be a JSON array of StaticCamera.
	URL string `json:"url"`
	// AuthHeader is sent verbatim as the Authorization header.
	AuthHeader string `json:"auth_header"`
	// PollInterval is how often the camera list is refreshed.
	PollInterval Duration `json:"poll_interval"`
	// RequestTimeout bounds a single provider request.
	RequestTimeout Duration `json:"request_timeout"`
}

type StaticCamera struct {
	CenterID string `json:"center_id"`
	CameraID string `json:"camera_id"`
	RTSPURL  string `json:"rtsp_url"`
	Enabled  *bool  `json:"enabled"`
}

// Default returns a configuration with production-leaning defaults.
func Default() Config {
	return Config{
		ShardID:     "shard-0",
		MaxChannels: 75,
		SpoolDir:    "/run/transmux/spool",
		StateDir:    "/var/lib/transmux/state",
		HTTPListen:  ":8080",
		Segment: SegmentConfig{
			TargetDuration: Duration{5 * time.Second},
			LiveWindow:     6,
			LocalListSize:  10,
			MaxGOPSlack:    Duration{10 * time.Second},
		},
		FFmpeg: FFmpegConfig{
			Binary:         "ffmpeg",
			RTSPTransport:  "tcp",
			StallTimeout:   Duration{30 * time.Second},
			StartupTimeout: Duration{20 * time.Second},
			ShutdownGrace:  Duration{5 * time.Second},
			ScanInterval:   Duration{500 * time.Millisecond},
		},
		Storage: StorageConfig{
			Backend:      "s3",
			ManifestName: "index.m3u8",
		},
		Upload: UploadConfig{
			MaxAttempts:   3,
			RetryBase:     Duration{200 * time.Millisecond},
			RetryMax:      Duration{5 * time.Second},
			MaxConcurrent: 64,
			PutTimeout:    Duration{15 * time.Second},
		},
		Reconnect: ReconnectConfig{
			Base:       Duration{1 * time.Second},
			Max:        Duration{30 * time.Second},
			Factor:     2.0,
			ResetAfter: Duration{60 * time.Second},
		},
		Cameras: CameraConfig{
			Provider:       "static",
			PollInterval:   Duration{30 * time.Second},
			RequestTimeout: Duration{10 * time.Second},
		},
	}
}

// Load reads a JSON config file on top of Default.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate rejects configurations that would fail at runtime or silently
// produce unplayable output.
func (c *Config) Validate() error {
	if c.ShardID == "" {
		return fmt.Errorf("shard_id must not be empty")
	}
	if c.MaxChannels <= 0 {
		return fmt.Errorf("max_channels must be > 0, got %d", c.MaxChannels)
	}
	if c.SpoolDir == "" {
		return fmt.Errorf("spool_dir must not be empty")
	}
	if c.StateDir == "" {
		return fmt.Errorf("state_dir must not be empty")
	}
	if c.Segment.TargetDuration.Duration < time.Second {
		return fmt.Errorf("segment.target_duration must be >= 1s")
	}
	if c.Segment.LiveWindow < 1 {
		return fmt.Errorf("segment.live_window must be >= 1")
	}
	if c.Segment.LocalListSize <= c.Segment.LiveWindow {
		return fmt.Errorf("segment.local_list_size (%d) must exceed live_window (%d) "+
			"so a segment is not deleted before it is uploaded",
			c.Segment.LocalListSize, c.Segment.LiveWindow)
	}
	switch c.FFmpeg.RTSPTransport {
	case "tcp", "udp":
	default:
		return fmt.Errorf("ffmpeg.rtsp_transport must be tcp or udp, got %q", c.FFmpeg.RTSPTransport)
	}
	if c.FFmpeg.StallTimeout.Duration <= c.Segment.TargetDuration.Duration {
		return fmt.Errorf("ffmpeg.stall_timeout must exceed segment.target_duration")
	}
	if c.FFmpeg.ScanInterval.Duration <= 0 {
		return fmt.Errorf("ffmpeg.scan_interval must be > 0")
	}
	switch c.Storage.Backend {
	case "s3":
		if c.Storage.Bucket == "" {
			return fmt.Errorf("storage.bucket is required for backend s3")
		}
	case "filesystem":
		if c.Storage.Root == "" {
			return fmt.Errorf("storage.root is required for backend filesystem")
		}
	default:
		return fmt.Errorf("storage.backend must be s3 or filesystem, got %q", c.Storage.Backend)
	}
	if c.Storage.ManifestName == "" {
		return fmt.Errorf("storage.manifest_name must not be empty")
	}
	if c.Upload.MaxAttempts < 1 {
		return fmt.Errorf("upload.max_attempts must be >= 1")
	}
	if c.Upload.MaxConcurrent < 1 {
		return fmt.Errorf("upload.max_concurrent must be >= 1")
	}
	if c.Reconnect.Factor < 1 {
		return fmt.Errorf("reconnect.factor must be >= 1")
	}
	if c.Reconnect.Max.Duration < c.Reconnect.Base.Duration {
		return fmt.Errorf("reconnect.max must be >= reconnect.base")
	}
	switch c.Cameras.Provider {
	case "static":
		if len(c.Cameras.Static) == 0 {
			return fmt.Errorf("cameras.static must list at least one camera")
		}
	case "http":
		if c.Cameras.URL == "" {
			return fmt.Errorf("cameras.url is required for provider http")
		}
	default:
		return fmt.Errorf("cameras.provider must be static or http, got %q", c.Cameras.Provider)
	}
	return nil
}
