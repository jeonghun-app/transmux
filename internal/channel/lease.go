package channel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jeonghun-app/transmux/internal/storage"
)

// LeaseObjectName is the control object that records which process owns a
// camera. It is deliberately not configurable: two shards using different
// lock namespaces would reintroduce the very bug the lease exists to prevent.
//
// It sits under the camera's own prefix, so the IAM scope a shard already
// needs covers it. It is not under a date directory and does not match the
// segment URI pattern, so no manifest parser will ever mistake it for media.
const LeaseObjectName = "_transmux/lease.json"

// ErrLeaseHeld reports that another process holds a live lease. It is an
// expected condition during a rolling deploy, not a fault.
var ErrLeaseHeld = errors.New("camera lease held by another owner")

// ErrLeaseLost reports that this process no longer owns the camera. It is
// terminal for the worker: something else is publishing now.
var ErrLeaseLost = errors.New("camera lease lost")

// leaseRecord is the body of the lease object.
//
// It carries no RTSP URL, no credentials and no host identity beyond the
// operator-chosen shard label, because this object sits in the same bucket as
// the media and inherits whatever read scope that has.
type leaseRecord struct {
	Schema int `json:"schema"`
	// CameraKey guards against a misconfigured prefix pointing two cameras at
	// one lease object.
	CameraKey string `json:"camera_key"`
	// OwnerSession identifies one process incarnation. It must not survive a
	// restart: a restarted task is a new owner, and a renewal still in flight
	// from before the restart must not be able to extend a lease the new
	// process did not acquire.
	OwnerSession string `json:"owner_session"`
	ShardID      string `json:"shard_id"`
	// Status is "held" or "released". A clean release leaves a tombstone
	// rather than deleting the object, so the successor's compare-and-swap
	// has something to swap against and no DeleteObject permission is needed.
	Status    string    `json:"status"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// OperationID identifies the individual write, so an ambiguous timeout
	// can be resolved by reading the object back.
	OperationID string `json:"operation_id"`
}

func (r leaseRecord) held(now time.Time, skew time.Duration) bool {
	return r.Status == "held" && now.Before(r.ExpiresAt.Add(skew))
}

// newID returns a 128-bit random identifier. Randomness rather than a counter
// is what keeps identity unique across an operator deleting the lease object.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a time-based fallback is
		// still better than a fixed value, which would break exclusion.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewSessionID generates the per-process owner identity. Called once at
// startup and passed to every worker.
func NewSessionID() string { return newID() }

// leaseHolder owns one camera's lease object.
//
// The lease is an admission and liveness mechanism: it stops a second shard
// from starting ffmpeg for a camera that is already being served. It is not
// by itself the write fence. Acquiring a lease cannot revoke another
// process's ability to write the manifest key, so the actual fence is the
// conditional manifest write in the worker; see Worker.fenceManifest.
type leaseHolder struct {
	client  ObjectClient
	key     string
	camera  string
	shardID string
	session string
	cfg     leaseTuning

	// etag is the version of the lease object this holder last wrote. It is
	// the token every renewal swaps against.
	etag string
	// deadline is the monotonic instant after which this holder must stop
	// publishing, whatever the store says. It is derived from the wall clock
	// at the moment a write succeeded, so a renewal response that arrives
	// after the deadline cannot revive ownership.
	deadline time.Time
	held     bool

	now func() time.Time
}

type leaseTuning struct {
	TTL           time.Duration
	RenewInterval time.Duration
	MaxClockSkew  time.Duration
}

func newLeaseHolder(client ObjectClient, key, camera, shardID, session string, cfg leaseTuning) *leaseHolder {
	return &leaseHolder{
		client: client, key: key, camera: camera,
		shardID: shardID, session: session, cfg: cfg,
		now: time.Now,
	}
}

// Acquire claims the camera.
//
// Outcomes:
//
//	nil            this process now owns the camera
//	ErrLeaseHeld   someone else owns it and their lease is still live
//	other error    the store could not be consulted; ownership is unknown and
//	               the caller must not start publishing
func (l *leaseHolder) Acquire(ctx context.Context) error {
	body, etag, err := l.client.Get(ctx, l.key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		// No lease object at all: claim it with a create-only write so that
		// exactly one of several simultaneous claimants wins.
		return l.write(ctx, storage.Preconditions{IfNoneMatch: true})
	case err != nil:
		return fmt.Errorf("read lease %s: %w", l.key, err)
	}

	var rec leaseRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		// Not a lease record. Refuse rather than overwrite: it may belong to
		// another system, and guessing here is how you get two publishers.
		return fmt.Errorf("lease %s is not readable as a lease record: %w", l.key, err)
	}
	if rec.CameraKey != "" && rec.CameraKey != l.camera {
		return fmt.Errorf("lease %s belongs to camera %q, not %q", l.key, rec.CameraKey, l.camera)
	}
	// A contender waits out the full TTL plus the skew allowance on both
	// sides: the holder's clock may be slow while ours is fast.
	if rec.held(l.now(), 2*l.cfg.MaxClockSkew) && rec.OwnerSession != l.session {
		return fmt.Errorf("%w: %s until %s", ErrLeaseHeld, rec.ShardID, rec.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return l.write(ctx, storage.Preconditions{IfMatch: etag})
}

// Renew extends the lease. A refused precondition means another process took
// over, which is terminal.
func (l *leaseHolder) Renew(ctx context.Context) error {
	if !l.held {
		return ErrLeaseLost
	}
	err := l.write(ctx, storage.Preconditions{IfMatch: l.etag})
	if errors.Is(err, storage.ErrPreconditionFailed) {
		l.held = false
		return fmt.Errorf("%w: renewal refused, another owner holds %s", ErrLeaseLost, l.key)
	}
	return err
}

// Release writes a tombstone so a successor can take over immediately instead
// of waiting out the TTL. Failure is not fatal: expiry is the fallback.
func (l *leaseHolder) Release(ctx context.Context) error {
	if !l.held {
		return nil
	}
	rec := l.record("released", l.now())
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = l.client.Put(ctx, storage.Object{
		Key:           l.key,
		Body:          raw,
		ContentType:   "application/json",
		CacheControl:  "no-store",
		Preconditions: storage.Preconditions{IfMatch: l.etag},
	})
	l.held = false
	return err
}

// Expired reports whether this holder has passed its own safety deadline. The
// worker checks it before every publish so a store it cannot reach does not
// leave it publishing past its ownership window.
func (l *leaseHolder) Expired() bool {
	if !l.held {
		return true
	}
	return !l.now().Before(l.deadline)
}

func (l *leaseHolder) record(status string, issued time.Time) leaseRecord {
	return leaseRecord{
		Schema:       1,
		CameraKey:    l.camera,
		OwnerSession: l.session,
		ShardID:      l.shardID,
		Status:       status,
		IssuedAt:     issued,
		ExpiresAt:    issued.Add(l.cfg.TTL),
		OperationID:  newID(),
	}
}

// write stores a held record under the given precondition and, on an
// ambiguous outcome, reads back to find out whether it landed.
func (l *leaseHolder) write(ctx context.Context, pre storage.Preconditions) error {
	// The deadline is derived from the clock reading taken before the request,
	// never after: if the request takes ten seconds, those ten seconds are
	// already gone from the lease's life.
	issued := l.now()
	rec := l.record("held", issued)
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	etag, err := l.client.Put(ctx, storage.Object{
		Key:           l.key,
		Body:          raw,
		ContentType:   "application/json",
		CacheControl:  "no-store",
		Preconditions: pre,
	})
	if err == nil {
		l.etag, l.held = etag, true
		l.deadline = issued.Add(l.cfg.TTL - l.cfg.MaxClockSkew)
		return nil
	}
	if errors.Is(err, storage.ErrPreconditionFailed) {
		return err
	}
	// Ambiguous: the request may have landed and the response been lost.
	// Reading back by operation ID is the only way to tell, and guessing
	// either way is unsafe -- assuming failure risks two owners, assuming
	// success risks publishing without a lease.
	if landed, lerr := l.confirm(ctx, rec.OperationID); lerr == nil && landed {
		l.held = true
		l.deadline = issued.Add(l.cfg.TTL - l.cfg.MaxClockSkew)
		return nil
	}
	return err
}

// confirm reports whether the given operation is the one stored at the key.
func (l *leaseHolder) confirm(ctx context.Context, operationID string) (bool, error) {
	body, etag, err := l.client.Get(ctx, l.key)
	if err != nil {
		return false, err
	}
	var rec leaseRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return false, err
	}
	if rec.OperationID != operationID {
		return false, nil
	}
	l.etag = etag
	return true, nil
}
