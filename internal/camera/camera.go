// Package camera models a source camera and loads the camera roster from
// an external system of record.
package camera

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/jeonghun-app/transmux/internal/config"
)

// idPattern restricts identifiers to characters that are safe in both an
// S3 key and a filesystem path. This is the primary defence against a
// provider injecting "../" or a shell metacharacter into a path.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Camera is one RTSP source assigned to this shard.
type Camera struct {
	CenterID string
	CameraID string
	RTSPURL  string
}

// Key uniquely identifies a camera within a center.
func (c Camera) Key() string { return c.CenterID + "/" + c.CameraID }

// SafeURL returns the RTSP URL with any userinfo replaced by a literal mask.
// Every log line, metric label and API response must use this, never RTSPURL.
//
// The mask is spliced in rather than assigned through url.User, because
// url.URL.String percent-encodes userinfo and would emit "%2A%2A%2A".
func (c Camera) SafeURL() string {
	u, err := url.Parse(c.RTSPURL)
	if err != nil {
		return "rtsp://<unparseable>"
	}
	hadUser := u.User != nil
	u.User = nil
	s := u.String()
	if !hadUser {
		return s
	}
	scheme := u.Scheme + "://"
	return scheme + "***@" + strings.TrimPrefix(s, scheme)
}

// Validate checks the fields that are interpolated into paths and command
// arguments.
func (c Camera) Validate() error {
	if !idPattern.MatchString(c.CenterID) {
		return fmt.Errorf("invalid center_id %q: must match %s", c.CenterID, idPattern)
	}
	if !idPattern.MatchString(c.CameraID) {
		return fmt.Errorf("invalid camera_id %q: must match %s", c.CameraID, idPattern)
	}
	u, err := url.Parse(c.RTSPURL)
	if err != nil {
		return fmt.Errorf("camera %s: invalid rtsp_url: %w", c.Key(), err)
	}
	switch strings.ToLower(u.Scheme) {
	case "rtsp", "rtsps":
	default:
		return fmt.Errorf("camera %s: rtsp_url scheme must be rtsp or rtsps, got %q", c.Key(), u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("camera %s: rtsp_url has no host", c.Key())
	}
	return nil
}

// Provider returns the desired camera roster. Implementations must be safe
// for concurrent use.
type Provider interface {
	Cameras(ctx context.Context) ([]Camera, error)
	Name() string
}

// StaticProvider serves a fixed roster from the config file.
type StaticProvider struct{ cameras []Camera }

func (p *StaticProvider) Name() string { return "static" }

func (p *StaticProvider) Cameras(context.Context) ([]Camera, error) {
	out := make([]Camera, len(p.cameras))
	copy(out, p.cameras)
	return out, nil
}

// HTTPProvider polls an external API or DB-backed service for the roster.
type HTTPProvider struct {
	url        string
	authHeader string
	client     *http.Client
}

func (p *HTTPProvider) Name() string { return "http" }

func (p *HTTPProvider) Cameras(ctx context.Context) ([]Camera, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if p.authHeader != "" {
		req.Header.Set("Authorization", p.authHeader)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("camera provider request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("camera provider returned HTTP %d", resp.StatusCode)
	}
	// Bound the response so a misbehaving provider cannot exhaust memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("camera provider body: %w", err)
	}
	var raw []config.StaticCamera
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("camera provider payload: %w", err)
	}
	return convert(raw)
}

// NewProvider builds the provider selected by configuration.
func NewProvider(cfg config.CameraConfig) (Provider, error) {
	switch cfg.Provider {
	case "static":
		cams, err := convert(cfg.Static)
		if err != nil {
			return nil, err
		}
		return &StaticProvider{cameras: cams}, nil
	case "http":
		return &HTTPProvider{
			url:        cfg.URL,
			authHeader: cfg.AuthHeader,
			client:     &http.Client{Timeout: cfg.RequestTimeout.Duration},
		}, nil
	default:
		return nil, fmt.Errorf("unknown camera provider %q", cfg.Provider)
	}
}

// convert validates and de-duplicates a raw roster. A duplicate camera is
// rejected rather than ignored: two writers for one manifest would corrupt
// the published playlist.
func convert(raw []config.StaticCamera) ([]Camera, error) {
	out := make([]Camera, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, r := range raw {
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		c := Camera{CenterID: r.CenterID, CameraID: r.CameraID, RTSPURL: r.RTSPURL}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("camera entry %d: %w", i, err)
		}
		if _, dup := seen[c.Key()]; dup {
			return nil, fmt.Errorf("duplicate camera %s in roster", c.Key())
		}
		seen[c.Key()] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}
