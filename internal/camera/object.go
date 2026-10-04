package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// ObjectProvider lets an operator change a shard's roster without restarting
// ingestion. Replicas read the same roster; the existing per-camera lease
// remains the authority deciding which replica actually connects to RTSP.
type ObjectProvider struct {
	store   storage.ControlClient
	key     string
	seed    []config.StaticCamera
	timeout time.Duration
	shard   string
}

func NewObjectProvider(cfg config.CameraConfig, store storage.ControlClient) (*ObjectProvider, error) {
	if err := config.ValidateRosterKey(cfg.ObjectKey); err != nil {
		return nil, err
	}
	if err := ValidateRoster(cfg.Static); err != nil {
		return nil, err
	}
	return &ObjectProvider{store: store, key: cfg.ObjectKey,
		seed: append([]config.StaticCamera{}, cfg.Static...), timeout: cfg.RequestTimeout.Duration, shard: cfg.ShardFilter}, nil
}

func (p *ObjectProvider) Name() string { return "object" }

func (p *ObjectProvider) Cameras(ctx context.Context) ([]Camera, error) {
	roster, _, err := p.Load(ctx)
	if err != nil {
		return nil, err
	}
	cameras, err := convert(roster)
	return assigned(cameras, p.shard), err
}

func (p *ObjectProvider) Load(ctx context.Context) ([]config.StaticCamera, string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	raw, etag, err := p.store.Get(ctx, p.key)
	if errors.Is(err, storage.ErrNotFound) {
		seed, _ := json.Marshal(p.seed)
		_, writeErr := p.store.Put(ctx, storage.Object{Key: p.key, Body: seed,
			ContentType: "application/json", CacheControl: "no-store",
			Preconditions: storage.Preconditions{IfNoneMatch: true}})
		// An ambiguous create or another replica winning the seed race is
		// resolved by reading the durable roster, never overwriting it.
		raw, etag, err = p.store.Get(ctx, p.key)
		if err != nil {
			return nil, "", fmt.Errorf("initialize camera roster: %v (read: %w)", writeErr, err)
		}
	}
	if err != nil {
		return nil, "", err
	}
	if etag == "" {
		return nil, "", fmt.Errorf("camera roster has no ETag")
	}
	var roster []config.StaticCamera
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&roster); err != nil {
		// JSON type errors can echo a credential value. No raw decoder
		// errors from the shared credential-bearing roster enter logs.
		return nil, "", fmt.Errorf("camera roster is not a valid JSON array")
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF || roster == nil {
		return nil, "", fmt.Errorf("camera roster must be a single JSON array")
	}
	if err := ValidateRoster(roster); err != nil {
		return nil, "", err
	}
	return roster, etag, nil
}

func ValidateRoster(roster []config.StaticCamera) error {
	if len(roster) > 10000 {
		return fmt.Errorf("camera roster exceeds 10000 entries")
	}
	seen := make(map[string]bool, len(roster))
	for _, cam := range roster {
		if err := cam.Validate(); err != nil {
			return err
		}
		key := cam.CenterID + "/" + cam.CameraID
		if seen[key] {
			return fmt.Errorf("duplicate camera %s", key)
		}
		seen[key] = true
	}
	return nil
}

// Save is an explicit compare-and-swap. A caller receiving an error must
// refresh before editing again; replaying an edit blindly can lose changes.
func (p *ObjectProvider) Save(ctx context.Context, roster []config.StaticCamera, etag string) (string, error) {
	if etag == "" {
		return "", fmt.Errorf("camera roster updates require If-Match")
	}
	if err := ValidateRoster(roster); err != nil {
		return "", err
	}
	if roster == nil {
		roster = []config.StaticCamera{}
	}
	body, err := json.Marshal(roster)
	if err != nil {
		return "", err
	}
	if len(body) > 8<<20 {
		return "", fmt.Errorf("camera roster exceeds 8 MiB")
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	next, err := p.store.Put(ctx, storage.Object{Key: p.key, Body: body,
		ContentType: "application/json", CacheControl: "no-store",
		Preconditions: storage.Preconditions{IfMatch: etag}})
	if err != nil && !errors.Is(err, storage.ErrPreconditionFailed) {
		// Resolve a lost success response without performing another write.
		if got, version, readErr := p.store.Get(ctx, p.key); readErr == nil &&
			version != "" && bytes.Equal(got, body) {
			return version, nil
		}
	}
	return next, err
}
