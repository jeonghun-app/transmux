// Package upload puts objects into the store with bounded concurrency and
// full-jitter retry.
package upload

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/metrics"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// ErrGaveUp reports that every attempt failed.
var ErrGaveUp = errors.New("upload exhausted all attempts")

// Coordinator serialises access to the object store.
//
// It owns a global semaphore rather than a per-channel one: with several
// hundred channels, unbounded concurrent PutObject calls would exhaust file
// descriptors and sockets long before S3 pushed back.
type Coordinator struct {
	store storage.ObjectStore
	cfg   config.UploadConfig
	sem   chan struct{}
	reg   *metrics.Registry

	putTotal     *metrics.Metric
	putFailures  *metrics.Metric
	putRetries   *metrics.Metric
	putConflicts *metrics.Metric
	putBytes     *metrics.Metric
	inFlight     *metrics.Metric
}

func NewCoordinator(store storage.ObjectStore, cfg config.UploadConfig, reg *metrics.Registry) *Coordinator {
	return &Coordinator{
		store: store,
		cfg:   cfg,
		sem:   make(chan struct{}, cfg.MaxConcurrent),
		reg:   reg,
		putTotal: reg.Counter("transmux_object_put_total",
			"Successful object store PUT operations."),
		putFailures: reg.Counter("transmux_object_put_failures_total",
			"Object store PUT operations that failed after all attempts."),
		putRetries: reg.Counter("transmux_object_put_retries_total",
			"Individual PUT attempts that failed and were retried."),
		putConflicts: reg.Counter("transmux_object_put_conflicts_total",
			"Conditional PUTs refused because another writer held the object. Never retried."),
		putBytes: reg.Counter("transmux_object_put_bytes_total",
			"Total bytes written to the object store."),
		inFlight: reg.Gauge("transmux_object_put_in_flight",
			"Object store PUT operations currently in flight."),
	}
}

// Store exposes the backing store description for health output.
func (c *Coordinator) Store() storage.ObjectStore { return c.store }

// Put uploads one object, retrying with full jitter.
//
// The same key is reused across attempts so a retry after an ambiguous
// failure simply overwrites identical bytes. Generating a fresh key per
// attempt would leave orphaned objects that no manifest references.
func (c *Coordinator) Put(ctx context.Context, obj storage.Object) (string, error) {
	// Check cancellation before the semaphore. A select with both cases ready
	// picks at random, so without this a shutdown could still issue a request
	// that is certain to fail.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.sem }()

	return c.attempts(ctx, obj)
}

// PutFile uploads the contents of srcPath, reading the file only after a
// concurrency slot has been acquired.
//
// That ordering is the whole point. Reading first and then queueing bounds
// resident segment bodies by channel count instead of by max_concurrent: a
// shard with several hundred channels and a slow store would hold one entire
// segment per channel in the Go heap while waiting for a slot, which is
// gigabytes at the channel counts this is sized for.
//
// A missing file is returned as-is so the caller can tell "ffmpeg reclaimed
// it" (data loss, count it) from "the store rejected it" (retry it). The
// bytes are returned because the caller needs the size it actually uploaded.
func (c *Coordinator) PutFile(ctx context.Context, obj storage.Object, srcPath string) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, "", ctx.Err()
	}
	defer func() { <-c.sem }()

	body, err := os.ReadFile(srcPath)
	if err != nil {
		return 0, "", err
	}
	obj.Body = body
	etag, err := c.attempts(ctx, obj)
	if err != nil {
		return 0, "", err
	}
	return int64(len(body)), etag, nil
}

// Head reads an object's metadata under the same concurrency bound.
//
// It exists to resolve an ambiguous conditional write: when a request times
// out, the caller cannot know whether it landed, and a blind retry of a
// conditional write would report a spurious conflict.
func (c *Coordinator) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return storage.ObjectInfo{}, ctx.Err()
	}
	defer func() { <-c.sem }()
	return c.store.Head(ctx, key)
}

// attempts runs the retry loop. The caller must already hold a semaphore slot.
func (c *Coordinator) attempts(ctx context.Context, obj storage.Object) (string, error) {
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.PutTimeout.Duration)
		etag, err := c.store.Put(attemptCtx, obj)
		cancel()
		if err == nil {
			c.putTotal.Inc()
			c.putBytes.Add(float64(len(obj.Body)))
			return etag, nil
		}
		// A refused precondition is an answer, not a fault. Retrying cannot
		// make it true, and burning the retry budget here would bury the one
		// signal that says another writer owns this object.
		if errors.Is(err, storage.ErrPreconditionFailed) {
			c.putConflicts.Inc()
			return "", err
		}
		lastErr = err
		// A cancelled parent context means shutdown, not a transient fault.
		if ctx.Err() != nil {
			c.putFailures.Inc()
			return "", fmt.Errorf("upload %s cancelled: %w", obj.Key, ctx.Err())
		}
		if attempt < c.cfg.MaxAttempts {
			c.putRetries.Inc()
			if !sleepCtx(ctx, c.backoff(attempt)) {
				c.putFailures.Inc()
				return "", fmt.Errorf("upload %s cancelled: %w", obj.Key, ctx.Err())
			}
		}
	}
	c.putFailures.Inc()
	return "", fmt.Errorf("%w after %d attempts for %s: %v",
		ErrGaveUp, c.cfg.MaxAttempts, obj.Key, lastErr)
}

// Get reads an object, retrying transient failures. ErrNotFound is returned
// immediately: a missing key is an answer, not a fault.
func (c *Coordinator) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	defer func() { <-c.sem }()

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.PutTimeout.Duration)
		body, etag, err := c.store.Get(attemptCtx, key)
		cancel()
		if err == nil {
			return body, etag, nil
		}
		if errors.Is(err, storage.ErrNotFound) {
			return nil, "", err
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		if attempt < c.cfg.MaxAttempts {
			if !sleepCtx(ctx, c.backoff(attempt)) {
				return nil, "", ctx.Err()
			}
		}
	}
	return nil, "", fmt.Errorf("%w after %d attempts for %s: %v",
		ErrGaveUp, c.cfg.MaxAttempts, key, lastErr)
}

// backoff returns a full-jitter delay. Full jitter rather than plain
// exponential matters here: several hundred channels failing at the same
// instant would otherwise retry in lockstep.
func (c *Coordinator) backoff(attempt int) time.Duration {
	base := float64(c.cfg.RetryBase.Duration)
	capped := math.Min(base*math.Pow(2, float64(attempt-1)), float64(c.cfg.RetryMax.Duration))
	return time.Duration(rand.Int63n(int64(capped) + 1))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
