// Package config loads and validates transmuxd configuration.
//
// Configuration is JSON on purpose: the daemon deliberately keeps its
// third-party dependency surface to the AWS SDK only, so the standard
// library encoding/json is used instead of pulling in a YAML parser.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
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

	Segment   SegmentConfig   `json:"segment"`
	FFmpeg    FFmpegConfig    `json:"ffmpeg"`
	Storage   StorageConfig   `json:"storage"`
	Upload    UploadConfig    `json:"upload"`
	Reconnect ReconnectConfig `json:"reconnect"`
	Lease     LeaseConfig     `json:"lease"`
	Cameras   CameraConfig    `json:"cameras"`
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

// LeaseConfig tunes the camera ownership lease.
//
// The lease stops two shards from ingesting the same camera, which would make
// the published playlist flip between two sequence timelines. Because
// segments are immutable and cached for a year, a reused key is a CDN cache
// poisoning event rather than a harmless overwrite.
//
// There is deliberately no switch to turn it off and no way to change the
// object name: either would let two shards use different lock namespaces and
// reintroduce the bug.
type LeaseConfig struct {
	// TTL is how long a lease stays valid without renewal. It is also the
	// worst-case gap before another shard may take over a dead one.
	TTL Duration `json:"ttl"`

	// RenewInterval is how often the owner extends the lease.
	RenewInterval Duration `json:"renew_interval"`

	// MaxClockSkew is the assumed bound on wall-clock disagreement between
	// shards. A contender waits this long past expiry on top of the TTL, and
	// the owner stops this long before it. No TTL lease can be safe without
	// such a bound; hosts must run NTP.
	MaxClockSkew Duration `json:"max_clock_skew"`

	// OperationTimeout bounds one lease request. It is separate from
	// upload.put_timeout because a lease record is a few hundred bytes while a
	// segment is megabytes: sizing the two together would either make renewal
	// hang far longer than its own interval or make segment uploads give up
	// too early.
	OperationTimeout Duration `json:"operation_timeout"`
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
		Lease: LeaseConfig{
			TTL:              Duration{45 * time.Second},
			RenewInterval:    Duration{10 * time.Second},
			MaxClockSkew:     Duration{2 * time.Second},
			OperationTimeout: Duration{5 * time.Second},
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
	// Anything after the first JSON document is a mistake that would
	// otherwise be ignored, for instance a second copy of the config
	// appended to the file.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return cfg, fmt.Errorf("parse config %s: unexpected trailing content after the "+
			"JSON object", path)
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
	if c.Segment.MaxGOPSlack.Duration < 0 {
		return fmt.Errorf("segment.max_gop_slack must not be negative")
	}
	switch c.FFmpeg.RTSPTransport {
	case "tcp", "udp":
	default:
		return fmt.Errorf("ffmpeg.rtsp_transport must be tcp or udp, got %q", c.FFmpeg.RTSPTransport)
	}
	if c.FFmpeg.Binary == "" {
		return fmt.Errorf("ffmpeg.binary must not be empty")
	}
	// The watchdog threshold has to clear the same allowance the health check
	// uses, or a camera whose long GOP is explicitly tolerated by
	// max_gop_slack would still be killed and relaunched forever.
	gopAllowance := c.Segment.TargetDuration.Duration + c.Segment.MaxGOPSlack.Duration
	if c.FFmpeg.StallTimeout.Duration <= gopAllowance {
		return fmt.Errorf("ffmpeg.stall_timeout (%s) must exceed segment.target_duration + "+
			"segment.max_gop_slack (%s), otherwise a camera whose GOP is within the "+
			"configured slack is still killed by the watchdog",
			c.FFmpeg.StallTimeout.Duration, gopAllowance)
	}
	if c.FFmpeg.StartupTimeout.Duration <= gopAllowance {
		return fmt.Errorf("ffmpeg.startup_timeout (%s) must exceed segment.target_duration + "+
			"segment.max_gop_slack (%s), otherwise the first segment never arrives in time",
			c.FFmpeg.StartupTimeout.Duration, gopAllowance)
	}
	if c.FFmpeg.ShutdownGrace.Duration <= 0 {
		return fmt.Errorf("ffmpeg.shutdown_grace must be > 0, otherwise SIGTERM is followed " +
			"immediately by SIGKILL and the last segment is truncated")
	}
	if c.FFmpeg.ScanInterval.Duration <= 0 {
		return fmt.Errorf("ffmpeg.scan_interval must be > 0")
	}
	if c.FFmpeg.ScanInterval.Duration >= c.Segment.TargetDuration.Duration {
		return fmt.Errorf("ffmpeg.scan_interval (%s) must be shorter than "+
			"segment.target_duration (%s) or finished segments sit in the spool",
			c.FFmpeg.ScanInterval.Duration, c.Segment.TargetDuration.Duration)
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
	// The manifest name is joined onto the channel's key prefix, so it has to
	// be a single path element: a separator or ".." would move the playlist
	// out of the channel's own namespace.
	if strings.ContainsAny(c.Storage.ManifestName, `/\`) || c.Storage.ManifestName == ".." ||
		c.Storage.ManifestName == "." {
		return fmt.Errorf("storage.manifest_name %q must be a single path element",
			c.Storage.ManifestName)
	}
	if c.Upload.MaxAttempts < 1 {
		return fmt.Errorf("upload.max_attempts must be >= 1")
	}
	if c.Upload.MaxConcurrent < 1 {
		return fmt.Errorf("upload.max_concurrent must be >= 1")
	}
	if c.Upload.PutTimeout.Duration <= 0 {
		return fmt.Errorf("upload.put_timeout must be > 0, otherwise every request is " +
			"cancelled before it is sent")
	}
	if c.Upload.RetryBase.Duration < 0 {
		return fmt.Errorf("upload.retry_base must not be negative")
	}
	// A negative or inverted cap makes the full-jitter computation feed a
	// non-positive bound to rand.Int63n, which panics.
	if c.Upload.RetryMax.Duration < c.Upload.RetryBase.Duration {
		return fmt.Errorf("upload.retry_max (%s) must be >= upload.retry_base (%s)",
			c.Upload.RetryMax.Duration, c.Upload.RetryBase.Duration)
	}
	if c.Reconnect.Base.Duration <= 0 {
		return fmt.Errorf("reconnect.base must be > 0, otherwise a failing camera is " +
			"retried in a tight loop")
	}
	if c.Reconnect.Factor < 1 {
		return fmt.Errorf("reconnect.factor must be >= 1")
	}
	if c.Reconnect.Max.Duration < c.Reconnect.Base.Duration {
		return fmt.Errorf("reconnect.max must be >= reconnect.base")
	}
	if c.Reconnect.ResetAfter.Duration <= 0 {
		return fmt.Errorf("reconnect.reset_after must be > 0, otherwise the backoff resets " +
			"on a camera that connects and immediately stalls")
	}
	if c.Lease.TTL.Duration <= 0 {
		return fmt.Errorf("lease.ttl must be > 0")
	}
	if c.Lease.RenewInterval.Duration <= 0 {
		return fmt.Errorf("lease.renew_interval must be > 0")
	}
	if c.Lease.MaxClockSkew.Duration < 0 {
		return fmt.Errorf("lease.max_clock_skew must not be negative")
	}
	if c.Lease.OperationTimeout.Duration <= 0 {
		return fmt.Errorf("lease.operation_timeout must be > 0")
	}
	// A missed renewal must still leave room for the next one to complete
	// before the owner's own deadline at ttl - skew. The second attempt starts
	// at 2 x renew_interval and can take a full operation_timeout, so the whole
	// of that must fit:
	//
	//	2*renew_interval + operation_timeout + max_clock_skew < ttl
	//
	// Checking only 2*renew + 2*skew is not enough: ttl 25s, renew 10s, skew
	// 1s, operation_timeout 9s passes that but cannot recover before its own
	// deadline at 24s, because the second attempt may not finish until 29s.
	if need := 2*c.Lease.RenewInterval.Duration + c.Lease.OperationTimeout.Duration +
		c.Lease.MaxClockSkew.Duration; need >= c.Lease.TTL.Duration {
		return fmt.Errorf("lease.ttl (%s) must exceed 2 x lease.renew_interval + "+
			"lease.operation_timeout + lease.max_clock_skew (%s), otherwise one missed "+
			"renewal cannot be recovered before the owner's own deadline",
			c.Lease.TTL.Duration, need)
	}
	if c.Lease.OperationTimeout.Duration >= c.Lease.RenewInterval.Duration {
		return fmt.Errorf("lease.operation_timeout (%s) must be shorter than "+
			"lease.renew_interval (%s), or a renewal cannot finish before the next is due",
			c.Lease.OperationTimeout.Duration, c.Lease.RenewInterval.Duration)
	}
	if c.Cameras.PollInterval.Duration <= 0 {
		return fmt.Errorf("cameras.poll_interval must be > 0")
	}
	if c.Cameras.RequestTimeout.Duration <= 0 {
		return fmt.Errorf("cameras.request_timeout must be > 0, otherwise every roster " +
			"load is cancelled before it starts")
	}
	if err := validateListenAddr(c.HTTPListen); err != nil {
		return err
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

// validateListenAddr rejects an address net.Listen would either refuse or,
// worse, silently accept as a wildcard on a default port.
func validateListenAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("http_listen must not be empty; use \"127.0.0.1:8080\" to keep " +
			"the unauthenticated monitoring API on the loopback interface")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("http_listen %q must be host:port: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("http_listen %q has no port", addr)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("http_listen %q has an invalid port: %w", addr, err)
	}
	_ = host // an empty host is a legitimate wildcard bind
	return nil
}
