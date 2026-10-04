// Package camera models a source camera and loads the camera roster from
// an external system of record.
package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// Camera is one RTSP source assigned to this shard.
type Camera struct {
	CenterID   string
	CameraID   string
	ShardID    string
	RTSPURL    string
	Name       string
	Audio      string
	Format     string
	VideoCodec string
}

// Key uniquely identifies a camera within a center.
func (c Camera) Key() string { return c.CenterID + "/" + c.CameraID }

// SafeURL returns the RTSP URL stripped of everything that can carry a
// secret. Every log line, metric label and API response must use this, never
// RTSPURL.
//
// Userinfo is the obvious case, but cameras and NVRs also accept credentials
// as query parameters, so the query and fragment are dropped wholesale rather
// than filtered: a value that looks harmless today is not worth publishing
// through an unauthenticated status endpoint.
//
// The mask is spliced in rather than assigned through url.User, because
// url.URL.String percent-encodes userinfo and would emit "%2A%2A%2A".
func (c Camera) SafeURL() string {
	u, err := url.Parse(c.RTSPURL)
	if err != nil {
		return "rtsp://<unparseable>"
	}
	hadUser := u.User != nil
	hadQuery := u.RawQuery != "" || u.ForceQuery
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	s := u.String()
	if hadQuery {
		s += "?<redacted>"
	}
	if !hadUser {
		return s
	}
	scheme := u.Scheme + "://"
	return scheme + "***@" + strings.TrimPrefix(s, scheme)
}

// redactURL removes credentials from a URL that may appear in an error
// message or a log line. Unparseable input is dropped entirely rather than
// echoed, since the point is to avoid printing a secret by accident.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery = "<redacted>"
		u.ForceQuery = false
	}
	u.Fragment = ""
	return u.String()
}

// StderrRedactor returns a replacer that removes this camera's credentials
// from arbitrary text, for use on ffmpeg's stderr.
//
// ffmpeg echoes the input URL in many of its own messages, so the raw URL and
// the password on its own are both replaced. A very short password is left
// alone: replacing a two-character string everywhere would corrupt unrelated
// log content for no real gain, and such a password is not protectable here
// anyway.
func (c Camera) StderrRedactor() func(string) string {
	var pairs []string
	if c.RTSPURL != "" {
		pairs = append(pairs, c.RTSPURL, c.SafeURL())
	}
	if u, err := url.Parse(c.RTSPURL); err == nil && u.User != nil {
		// Longest first: the full userinfo before the bare password.
		if ui := u.User.String(); ui != "" {
			pairs = append(pairs, ui+"@", "***@")
		}
		if pass, ok := u.User.Password(); ok && len(pass) >= minRedactablePassword {
			pairs = append(pairs, pass, "***")
		}
	}
	if len(pairs) == 0 {
		return func(s string) string { return s }
	}
	return strings.NewReplacer(pairs...).Replace
}

// minRedactablePassword is the shortest password worth substituting out of
// free-form text.
const minRedactablePassword = 4

// Validate checks the fields that are interpolated into paths and command
// arguments.
func (c Camera) Validate() error {
	return (config.StaticCamera{
		CenterID: c.CenterID, CameraID: c.CameraID, RTSPURL: c.RTSPURL,
		Name: c.Name, Audio: c.Audio, Format: c.Format, VideoCodec: c.VideoCodec,
		ShardID: c.ShardID,
	}).Validate()
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
	shard      string
}

func (p *HTTPProvider) Name() string { return "http" }

func (p *HTTPProvider) Cameras(ctx context.Context) ([]Camera, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create camera provider request: invalid URL or context")
	}
	req.Header.Set("Accept", "application/json")
	if p.authHeader != "" {
		req.Header.Set("Authorization", p.authHeader)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// A *url.Error carries the full request URL, which may contain an
		// access token in its query. The manager stores this string and the
		// unauthenticated status endpoint serves it, so redact it here.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, fmt.Errorf("camera provider request to %s: %w",
				redactURL(uerr.URL), uerr.Err)
		}
		return nil, fmt.Errorf("camera provider request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("camera provider returned HTTP %d", resp.StatusCode)
	}
	// Bound the response so a misbehaving provider cannot exhaust memory.
	const maxRosterBytes = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRosterBytes+1))
	if err != nil {
		return nil, fmt.Errorf("camera provider body: %w", err)
	}
	if len(body) > maxRosterBytes {
		return nil, fmt.Errorf("camera provider payload exceeds %d bytes", maxRosterBytes)
	}
	var raw []config.StaticCamera
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("camera provider payload: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("camera provider payload has trailing content")
	}
	if raw == nil {
		return nil, fmt.Errorf("camera provider payload must be an array; use [] for an empty roster")
	}
	cameras, err := convert(raw)
	return assigned(cameras, p.shard), err
}

// NewProvider builds the provider selected by configuration.
func NewProvider(cfg config.CameraConfig, stores ...storage.ControlClient) (Provider, error) {
	switch cfg.Provider {
	case "static":
		cams, err := convert(cfg.Static)
		if err != nil {
			return nil, err
		}
		return &StaticProvider{cameras: assigned(cams, cfg.ShardFilter)}, nil
	case "http":
		if err := config.ValidateHTTPURL(cfg.URL, "cameras.url"); err != nil {
			return nil, err
		}
		return &HTTPProvider{
			url:        cfg.URL,
			authHeader: cfg.AuthHeader,
			client:     &http.Client{Timeout: cfg.RequestTimeout.Duration},
			shard:      cfg.ShardFilter,
		}, nil
	case "object":
		if len(stores) != 1 {
			return nil, fmt.Errorf("object camera provider requires a store")
		}
		return NewObjectProvider(cfg, stores[0])
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
		c := Camera{CenterID: r.CenterID, CameraID: r.CameraID, RTSPURL: r.RTSPURL,
			Name: r.Name, Audio: r.Audio, Format: r.Format, VideoCodec: r.VideoCodec, ShardID: r.ShardID}
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

func assigned(cameras []Camera, shard string) []Camera {
	if shard == "" {
		return cameras
	}
	out := make([]Camera, 0, len(cameras))
	for _, cam := range cameras {
		if cam.ShardID == shard {
			out = append(out, cam)
		}
	}
	return out
}
