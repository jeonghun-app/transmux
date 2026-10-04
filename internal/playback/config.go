package playback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
)

type Config struct {
	HTTPListen     string          `json:"http_listen"`
	PublicURL      string          `json:"public_url"`
	IngestConfig   string          `json:"ingest_config"`
	IndexPath      string          `json:"index_path"`
	AllowedOrigins []string        `json:"allowed_origins"`
	Auth           AuthConfig      `json:"auth"`
	Index          IndexConfig     `json:"index"`
	Export         ExportConfig    `json:"export"`
	Retention      RetentionConfig `json:"retention"`
	MaxStreams     int             `json:"max_streams"`
	MaxSessions    int             `json:"max_recording_sessions"`
	Shards         []ShardConfig   `json:"shards"`
}

type ShardConfig struct {
	ID          string `json:"id"`
	MaxChannels int    `json:"max_channels"`
}

type AuthConfig struct {
	SecretEnv  string          `json:"secret_env"`
	Issuer     string          `json:"issuer"`
	Audience   string          `json:"audience"`
	AccessTTL  config.Duration `json:"access_ttl"`
	SessionTTL config.Duration `json:"session_ttl"`
	Users      []UserConfig    `json:"users"`
	// TrustedProxies lists the CIDRs of reverse proxies whose X-Forwarded-For
	// header identifies the login client. Empty means only RemoteAddr counts.
	TrustedProxies []string `json:"trusted_proxies"`
}

type UserConfig struct {
	Username    string  `json:"username"`
	PasswordEnv string  `json:"password_env"`
	Grants      []Grant `json:"grants"`
}

type Grant struct {
	CenterID    string   `json:"center_id"`
	CameraIDs   []string `json:"camera_ids"`
	Permissions []string `json:"permissions"`
}

type IndexConfig struct {
	Days         int             `json:"days"`
	Workers      int             `json:"workers"`
	PollInterval config.Duration `json:"poll_interval"`
}

type ExportConfig struct {
	TempDir     string          `json:"temp_dir"`
	MaxDuration config.Duration `json:"max_duration"`
	Timeout     config.Duration `json:"timeout"`
	MaxBytes    int64           `json:"max_bytes"`
	Workers     int             `json:"workers"`
	// MinFreeBytes stays unreserved on the export volume, which also holds
	// the recording index and session database in the reference deployment.
	MinFreeBytes int64 `json:"min_free_bytes"`
}

type RetentionConfig struct {
	Enabled  bool            `json:"enabled"`
	Days     int             `json:"days"`
	Interval config.Duration `json:"interval"`
	Batch    int             `json:"batch"`
}

func DefaultConfig() Config {
	return Config{
		HTTPListen: ":8090", PublicURL: "http://localhost:8090",
		IngestConfig: "/etc/transmux/config.json",
		IndexPath:    "/var/lib/transmux/playback/recordings.db",
		Auth: AuthConfig{
			SecretEnv: "TRANSMUX_AUTH_SECRET", Issuer: "transmux", Audience: "transmux-api",
			AccessTTL:  config.Duration{Duration: 8 * time.Hour},
			SessionTTL: config.Duration{Duration: 15 * time.Minute},
		},
		Index: IndexConfig{Days: 30, Workers: 8, PollInterval: config.Duration{Duration: 5 * time.Second}},
		Export: ExportConfig{
			TempDir:     "/var/lib/transmux/playback/exports",
			MaxDuration: config.Duration{Duration: 2 * time.Hour},
			Timeout:     config.Duration{Duration: 10 * time.Minute},
			MaxBytes:    4 << 30, Workers: 2, MinFreeBytes: 1 << 30,
		},
		Retention:  RetentionConfig{Days: 30, Interval: config.Duration{Duration: time.Hour}, Batch: 1000},
		MaxStreams: 128, MaxSessions: 2000,
	}
}

func LoadConfig(filename string) (Config, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(filename)
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid playback configuration JSON")
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return cfg, fmt.Errorf("playback configuration has trailing content")
	}
	if publicURL := os.Getenv("TRANSMUX_PUBLIC_URL"); publicURL != "" {
		cfg.PublicURL = publicURL
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	_, port, err := net.SplitHostPort(c.HTTPListen)
	if err != nil || port == "" {
		return fmt.Errorf("http_listen must be host:port")
	}
	if err := config.ValidateHTTPURL(c.PublicURL, "public_url"); err != nil {
		return err
	}
	u, _ := url.Parse(c.PublicURL)
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("public_url must be an origin without credentials, path or query")
	}
	if c.IndexPath == "" || c.IngestConfig == "" || c.Export.TempDir == "" {
		return fmt.Errorf("ingest_config, index_path and export.temp_dir are required")
	}
	if c.Auth.SecretEnv == "" || c.Auth.Issuer == "" || c.Auth.Audience == "" ||
		c.Auth.Audience == "transmux-media" {
		return fmt.Errorf("auth requires a secret environment variable, issuer and distinct API audience")
	}
	if c.Auth.AccessTTL.Duration < time.Minute || c.Auth.AccessTTL.Duration > 24*time.Hour ||
		c.Auth.SessionTTL.Duration < time.Minute || c.Auth.SessionTTL.Duration > c.Auth.AccessTTL.Duration {
		return fmt.Errorf("auth TTLs must satisfy 1m <= session_ttl <= access_ttl <= 24h")
	}
	if len(c.Auth.Users) > 100 {
		return fmt.Errorf("auth.users exceeds 100; use the external JWT contract for larger user directories")
	}
	seen := map[string]bool{}
	for _, u := range c.Auth.Users {
		if !config.ValidID(u.Username) || seen[u.Username] || u.PasswordEnv == "" {
			return fmt.Errorf("auth user requires a unique username and password_env")
		}
		seen[u.Username] = true
		if err := validateGrants(u.Grants); err != nil {
			return err
		}
	}
	if len(c.Auth.TrustedProxies) > 64 {
		return fmt.Errorf("auth.trusted_proxies exceeds 64 entries")
	}
	if _, err := parseTrustedProxies(c.Auth.TrustedProxies); err != nil {
		return err
	}
	for _, origin := range c.AllowedOrigins {
		if err := config.ValidateHTTPURL(origin, "allowed_origins"); err != nil {
			return err
		}
		u, _ := url.Parse(origin)
		if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("allowed_origins entries must be exact origins without a trailing slash")
		}
	}
	if c.Index.Days < 2 || c.Index.Days > 365 || c.Index.Workers < 2 || c.Index.Workers > 64 ||
		c.Index.PollInterval.Duration < time.Second || c.Index.PollInterval.Duration > time.Minute {
		return fmt.Errorf("index requires days 2..365, workers 2..64 and poll_interval 1s..1m")
	}
	if c.Export.MaxDuration.Duration < time.Minute || c.Export.MaxDuration.Duration > 6*time.Hour ||
		c.Export.Timeout.Duration < time.Second || c.Export.Timeout.Duration > time.Hour ||
		c.Export.MaxBytes < 1<<20 || c.Export.MaxBytes > 100<<30 ||
		c.Export.Workers < 1 || c.Export.Workers > 16 {
		return fmt.Errorf("invalid export duration, timeout, byte limit or concurrency")
	}
	if c.Export.MinFreeBytes < 128<<20 || c.Export.MinFreeBytes > 1<<40 {
		return fmt.Errorf("export.min_free_bytes must be 128MiB..1TiB")
	}
	if c.Retention.Days < 1 || c.Retention.Days > 365 ||
		c.Retention.Interval.Duration < time.Minute || c.Retention.Interval.Duration > 24*time.Hour ||
		c.Retention.Batch < 1 || c.Retention.Batch > 10000 {
		return fmt.Errorf("invalid retention days, interval or batch")
	}
	if c.MaxStreams < 1 || c.MaxStreams > 10000 || c.MaxSessions < 1 || c.MaxSessions > 10000 {
		return fmt.Errorf("max_streams and max_recording_sessions must be 1..10000")
	}
	shards := map[string]bool{}
	for _, shard := range c.Shards {
		if !config.ValidID(shard.ID) || shard.MaxChannels < 1 || shards[shard.ID] {
			return fmt.Errorf("shards must have unique valid IDs and positive channel limits")
		}
		shards[shard.ID] = true
	}
	return nil
}

func validateGrants(grants []Grant) error {
	if len(grants) == 0 || len(grants) > 100 {
		return fmt.Errorf("grants must contain 1..100 entries")
	}
	for _, g := range grants {
		if g.CenterID != "*" && !config.ValidID(g.CenterID) ||
			len(g.CameraIDs) == 0 || len(g.CameraIDs) > 1000 || len(g.Permissions) == 0 {
			return fmt.Errorf("invalid grant scope")
		}
		for _, id := range g.CameraIDs {
			if id != "*" && !config.ValidID(id) {
				return fmt.Errorf("invalid camera scope")
			}
		}
		for _, p := range g.Permissions {
			switch p {
			case "live", "recording", "export", "manage":
			default:
				return fmt.Errorf("unknown permission")
			}
		}
	}
	return nil
}
