package channel

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/ffmpeg"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/metrics"
	"github.com/jeonghun-app/transmux/internal/storage"
)

// errStalled means ffmpeg was alive but produced no segments in time. It is
// treated exactly like a disconnect.
var errStalled = errors.New("no segment produced within stall timeout")

// Worker owns one camera end to end: the ffmpeg process, the spool, the
// published playlist and the channel's reported state.
//
// Everything that mutates channel state happens on the single goroutine
// running Run. Snapshot takes a lock only to copy out a consistent view.
type Worker struct {
	cam      camera.Camera
	cfg      config.Config
	log      *slog.Logger
	uploader ObjectClient
	reg      *metrics.Registry

	spoolDir  string
	statePath string

	bo *backoff

	// mu guards the fields below, which Snapshot reads.
	mu   sync.RWMutex
	snap Snapshot

	// runStartedAt is when the current ffmpeg generation was launched. It is
	// used to decide whether a run lasted long enough to reset the backoff.
	runStartedAt time.Time

	// window is the published live playlist, oldest first.
	window []hls.PublishedSegment
	// lastSequence is the highest published media sequence.
	lastSequence uint64
	// discontinuitySequence counts discontinuities that have scrolled out.
	discontinuitySequence uint64
	// pendingDiscontinuity marks the next published segment as the start of
	// a new continuity domain, which is the case after every reconnect.
	pendingDiscontinuity bool
	// needsRecovery is cleared once the starting sequence has been
	// established from the published manifest. It starts true on every
	// worker: the manifest is always consulted, checkpoint or not.
	needsRecovery bool
	// checkpointFloor is the highest sequence a local checkpoint claims was
	// published. It is a lower bound, never an authority: the checkpoint is
	// written after the manifest PUT succeeds and so can lag the durable
	// record, but it can never be ahead of what was actually used.
	checkpointFloor uint64

	// manifestDirty means segments were uploaded and appended to the window
	// but the manifest that references them has not been stored yet. It must
	// be retried on later scans even when no new segment arrives, otherwise
	// stored segments stay invisible forever.
	//
	// Only the Run goroutine touches it, via drain and publishManifest.
	manifestDirty bool
	// manifestRetryAfter rate limits manifest-only retries so a store outage
	// does not turn the scan interval into a log storm.
	manifestRetryAfter time.Time
	// lastManifestAt is when a manifest was last stored successfully. Guarded
	// by mu because the scrape-time collector reads it.
	lastManifestAt time.Time
	// spoolStatsAt rate limits the spool directory scan behind the spool
	// gauges, which would otherwise run on every tick for every channel.
	spoolStatsAt time.Time

	// manifestETag is the version of the published manifest this worker last
	// wrote or fenced. Every manifest write is conditional on it, so a worker
	// that has been taken over discovers it on its next publish instead of
	// clobbering the new owner's timeline.
	manifestETag string
	// lease is this worker's claim on the camera. It gates starting ffmpeg;
	// the manifest ETag is what actually fences writes.
	lease *leaseHolder
	// session identifies this process incarnation for lease ownership.
	session string

	// prevGen carries the previous generation's drain bookkeeping so its
	// spool can be flushed once more before it is discarded.
	prevGen *drainState

	// metrics handles, resolved once.
	mState        *metrics.Metric
	mSegments     *metrics.Metric
	mSegBytes     *metrics.Metric
	mLost         *metrics.Metric
	mReconnects   *metrics.Metric
	mUploadFail   *metrics.Metric
	mSegmentFail  *metrics.Metric
	mManifestFail *metrics.Metric
	mLastSegTS    *metrics.Metric
	mGapSeconds   *metrics.Metric
	mGapAlarm     *metrics.Metric
	mSegDuration  *metrics.Metric
	mLastSeenTS   *metrics.Metric
	mManifestTS   *metrics.Metric
	mManifestLag  *metrics.Metric
	mMissingPDT   *metrics.Metric
	mFailed       *metrics.Metric
	mDrainFail    *metrics.Metric
	mSpoolFiles   *metrics.Metric
	mSpoolBytes   *metrics.Metric
	mSpoolOldest  *metrics.Metric

	mLeaseHeld        *metrics.Metric
	mLeaseContended   *metrics.Metric
	mLeaseAcquireFail *metrics.Metric
	mLeaseRenewFail   *metrics.Metric
	mLeaseLost        *metrics.Metric
	mLeaseReleaseFail *metrics.Metric
	mManifestConflict *metrics.Metric
	mSegmentConflict  *metrics.Metric
}

// ObjectClient is the subset of the upload coordinator the worker needs.
//
// Get returns the ETag as well as the body because the ETag is the token that
// makes the next write conditional, which is how a stale writer is fenced out
// of a channel it no longer owns.
//
// PutFile exists so the segment body is read only once a concurrency slot has
// been granted. Reading in the worker and then queueing would make resident
// segment bytes scale with channel count instead of with upload concurrency.
type ObjectClient interface {
	Put(ctx context.Context, obj storage.Object) (string, error)
	PutFile(ctx context.Context, obj storage.Object, srcPath string) (int64, string, error)
	Get(ctx context.Context, key string) ([]byte, string, error)
	Head(ctx context.Context, key string) (storage.ObjectInfo, error)
}

// NewWorker constructs a worker. It does not start anything.
func NewWorker(cam camera.Camera, cfg config.Config, up ObjectClient, reg *metrics.Registry, log *slog.Logger, session string) *Worker {
	labels := []metrics.Label{
		{Name: "center_id", Value: cam.CenterID},
		{Name: "camera_id", Value: cam.CameraID},
	}
	w := &Worker{
		cam:       cam,
		cfg:       cfg,
		session:   session,
		log:       log.With("center_id", cam.CenterID, "camera_id", cam.CameraID),
		uploader:  up,
		reg:       reg,
		spoolDir:  filepath.Join(cfg.SpoolDir, cam.CenterID, cam.CameraID),
		statePath: checkpointPath(cfg.StateDir, cam.CenterID, cam.CameraID),
		bo:        newBackoff(cfg.Reconnect, time.Now().UnixNano()^int64(len(cam.Key()))),

		mState: reg.Gauge("transmux_channel_state",
			"Channel state: 1 starting, 2 receiving, 3 disconnected, 4 reconnecting, 5 stopping, 6 stopped, 7 failed.", labels...),
		mSegments: reg.Counter("transmux_channel_segments_published_total",
			"Segment objects successfully stored. A segment becomes visible to players "+
				"only when transmux_channel_last_manifest_timestamp_seconds advances.", labels...),
		mSegBytes: reg.Counter("transmux_channel_segment_bytes_total",
			"Total segment bytes uploaded for this channel.", labels...),
		mLost: reg.Counter("transmux_channel_segments_lost_total",
			"Segments that ffmpeg produced but that were reclaimed before upload.", labels...),
		mReconnects: reg.Counter("transmux_channel_reconnects_total",
			"Times the ffmpeg pipeline was restarted for this channel.", labels...),
		mUploadFail: reg.Counter("transmux_channel_upload_failures_total",
			"Segment or manifest uploads that failed after all retries.", labels...),
		mSegmentFail: reg.Counter("transmux_channel_segment_upload_failures_total",
			"Segment uploads that failed after all retries.", labels...),
		mManifestFail: reg.Counter("transmux_channel_manifest_upload_failures_total",
			"Manifest uploads that failed after all retries. Segments keep arriving in "+
				"this state but none of them are visible to players.", labels...),
		mLastSegTS: reg.Gauge("transmux_channel_last_segment_timestamp_seconds",
			"Unix timestamp of the most recently published segment.", labels...),
		mSegDuration: reg.Gauge("transmux_channel_last_segment_duration_seconds",
			"Actual EXTINF duration of the most recent segment, which is set by the camera GOP.", labels...),
		mLastSeenTS: reg.Gauge("transmux_channel_last_ffmpeg_segment_timestamp_seconds",
			"Unix timestamp when ffmpeg last finished a segment, before upload. "+
				"Compare with the published timestamp to tell a camera fault from a store fault.", labels...),
		mManifestTS: reg.Gauge("transmux_channel_last_manifest_timestamp_seconds",
			"Unix timestamp of the last successfully stored manifest. A segment is only "+
				"visible to players once this advances past it.", labels...),
		mMissingPDT: reg.Counter("transmux_channel_missing_pdt_total",
			"Segments ffmpeg produced with no EXT-X-PROGRAM-DATE-TIME, which forces the "+
				"date directory to fall back to upload time.", labels...),
		mFailed: reg.Counter("transmux_channel_failed_total",
			"Times this channel entered the terminal failed state. It does not recover "+
				"without operator action.", labels...),
		mDrainFail: reg.Counter("transmux_channel_final_drain_failures_total",
			"Post-exit flushes that did not complete, meaning spooled segments were "+
				"discarded or left behind.", labels...),
		mSpoolFiles: reg.Gauge("transmux_channel_spool_files",
			"Segment files currently waiting on the spool.", labels...),
		mSpoolBytes: reg.Gauge("transmux_channel_spool_bytes",
			"Bytes currently waiting on the spool. Sum across channels to size the tmpfs.", labels...),
		mLeaseHeld: reg.Gauge("transmux_channel_lease_held",
			"1 while this shard owns the camera and may publish.", labels...),
		mLeaseContended: reg.Counter("transmux_channel_lease_contended_total",
			"Times another shard was found holding a live lease. Expected during a "+
				"rolling deploy; sustained growth means two shards want the same camera.", labels...),
		mLeaseAcquireFail: reg.Counter("transmux_channel_lease_acquire_failures_total",
			"Ownership could not be determined because the object store did not answer.", labels...),
		mLeaseRenewFail: reg.Counter("transmux_channel_lease_renew_failures_total",
			"Lease renewals that failed transiently and were retried.", labels...),
		mLeaseLost: reg.Counter("transmux_channel_lease_lost_total",
			"Times this shard lost the camera to another owner, or ran past its own "+
				"lease deadline. The channel stops publishing.", labels...),
		mLeaseReleaseFail: reg.Counter("transmux_channel_lease_release_failures_total",
			"Clean releases that failed; the lease expires on its own instead.", labels...),
		mManifestConflict: reg.Counter("transmux_channel_manifest_conflicts_total",
			"Manifest writes refused because another writer had changed it. Any nonzero "+
				"value means two processes tried to publish this camera.", labels...),
		mSegmentConflict: reg.Counter("transmux_channel_segment_conflicts_total",
			"Segment keys that already held another writer's object. This is the "+
				"cache-poisoning case the ownership protocol exists to prevent.", labels...),
		mSpoolOldest: reg.Gauge("transmux_channel_spool_oldest_seconds",
			"Age of the oldest segment waiting on the spool. It approaches "+
				"local_list_size x segment length when the object store is failing, and "+
				"segments are reclaimed after that.", labels...),
	}
	// Values that decay with wall-clock time are computed at scrape time.
	// Stored gauges for these went stale during reconnect backoff and in the
	// failed state, which is exactly when an operator reads them.
	w.mGapSeconds = reg.GaugeFunc("transmux_channel_seconds_since_segment",
		"Seconds since the most recently published segment.", w.secondsSinceSegment, labels...)
	w.mGapAlarm = reg.GaugeFunc("transmux_channel_segment_gap_alarm",
		"1 when a receiving channel has exceeded its allowed segment gap.", w.gapAlarm, labels...)
	w.mManifestLag = reg.GaugeFunc("transmux_channel_seconds_since_manifest",
		"Seconds since the last successfully stored manifest. Unlike the segment gap "+
			"this covers the publish step, so it rises when only the manifest PUT fails.",
		w.secondsSinceManifest, labels...)
	w.snap = Snapshot{
		CenterID:  cam.CenterID,
		CameraID:  cam.CameraID,
		SourceURL: cam.SafeURL(),
		State:     StateStarting,
		StartedAt: time.Now().UTC(),
	}
	w.mState.Set(stateCode(StateStarting))

	// The published manifest is consulted on every startup, even when a local
	// checkpoint exists. The checkpoint is written only after the manifest
	// PUT succeeds, so it can legitimately lag the durable record; trusting
	// it on its own would rewind EXT-X-MEDIA-SEQUENCE after a crash between
	// those two writes. It is kept as a floor instead, because both sources
	// are lower bounds on what was actually published.
	w.needsRecovery = true
	if cp, err := loadCheckpoint(w.statePath); err != nil {
		// A corrupt checkpoint must not silently restart the sequence at
		// zero: that would rewind EXT-X-MEDIA-SEQUENCE and overwrite object
		// keys that players and CDN edges have already cached. Manifest
		// recovery in Run decides instead.
		w.log.Error("checkpoint unreadable, recovering from the published manifest", "error", err)
		w.setError(err)
	} else if cp.LastSequence > 0 {
		w.checkpointFloor = cp.LastSequence
		w.lastSequence = cp.LastSequence
		w.discontinuitySequence = cp.DiscontinuitySequence
		w.window = cp.toSegments()
		w.snap.LastSequence = cp.LastSequence
		w.log.Info("checkpoint loaded, pending validation against the published manifest",
			"last_sequence", cp.LastSequence)
	}
	w.lease = newLeaseHolder(up, w.objectKey(LeaseObjectName), cam.Key(), cfg.ShardID, session,
		leaseTuning{
			TTL:           cfg.Lease.TTL.Duration,
			RenewInterval: cfg.Lease.RenewInterval.Duration,
			MaxClockSkew:  cfg.Lease.MaxClockSkew.Duration,
		})
	return w
}

// recover establishes the starting media sequence before anything is
// published.
//
// The published manifest in the object store is the durable record. The local
// checkpoint is only a floor; it lives on the instance, does not survive an
// ECS or EKS task replacement, and is written after the manifest so it can be
// one publish behind. Four outcomes:
//
//	manifest found      resume from max(manifest, checkpoint), plus a
//	                    discontinuity
//	manifest absent     new channel; if a checkpoint exists its sequence
//	                    still stands, because those keys were used
//	read failed         nothing published yet, so retry; the channel waits
//	                    rather than failing permanently
//	manifest foreign    fail closed; it belongs to another writer and only an
//	                    operator can decide what that means
func (w *Worker) recover(ctx context.Context) error {
	key := w.objectKey(w.cfg.Storage.ManifestName)
	body, etag, err := w.uploader.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			w.mu.Lock()
			w.manifestETag = ""
			floor := w.checkpointFloor
			// The window came from the checkpoint and no manifest references
			// it any more. Republishing it could point players at objects a
			// lifecycle rule has already removed, so start the window empty
			// and keep only the sequence floor.
			w.window = nil
			w.pendingDiscontinuity = floor > 0
			w.needsRecovery = false
			w.mu.Unlock()
			if floor > 0 {
				w.log.Warn("no published manifest but a local checkpoint exists; "+
					"resuming above the checkpoint so used object keys are not reissued",
					"key", key, "last_sequence", floor)
			} else {
				w.log.Info("no published manifest, starting a new channel at sequence 0", "key", key)
			}
			return nil
		}
		// The store did not answer. Ownership and history are both unknown,
		// but nothing has been published, so this is safe to retry: a
		// transient outage at startup must not permanently disable every
		// channel on the shard and require an operator.
		return fmt.Errorf("%w: recover sequence from %s: %v", ErrLeaseUnavailable, key, err)
	}

	pub, err := hls.ParsePublished(body)
	if err != nil {
		// The object exists but is not one of ours. Overwriting it blindly
		// could clobber another writer's stream, so refuse.
		return fmt.Errorf("published manifest %s is not usable for recovery: %w", key, err)
	}

	w.mu.Lock()
	w.manifestETag = etag
	w.lastSequence = pub.MaxSequence
	w.discontinuitySequence = pub.DiscontinuitySequence
	w.window = pub.Window
	if w.checkpointFloor > pub.MaxSequence {
		// The checkpoint is ahead of the manifest, so a sequence was used
		// that the manifest never recorded. Never publish at or below it.
		// The recovered window is dropped as well: keeping it would leave a
		// numbering gap inside a single published playlist.
		w.log.Warn("local checkpoint is ahead of the published manifest, resuming from the checkpoint",
			"manifest_sequence", pub.MaxSequence, "checkpoint_sequence", w.checkpointFloor)
		w.lastSequence = w.checkpointFloor
		w.window = nil
	}
	w.snap.LastSequence = w.lastSequence
	// Everything before the restart belongs to a different continuity domain.
	w.pendingDiscontinuity = true
	w.needsRecovery = false
	resumed := w.lastSequence
	w.mu.Unlock()

	w.log.Info("recovered sequence from published manifest",
		"key", key,
		"last_sequence", resumed,
		"manifest_sequence", pub.MaxSequence,
		"discontinuity_sequence", pub.DiscontinuitySequence,
		"window", len(pub.Window))
	return nil
}

// secondsSinceSegment, gapAlarm and secondsSinceManifest are the scrape-time
// collectors. They take the read lock themselves and must never be called
// while it is already held.
func (w *Worker) secondsSinceSegment() float64 {
	w.mu.RLock()
	last := w.snap.LastSegmentAt
	w.mu.RUnlock()
	if last == nil {
		return 0
	}
	return time.Since(*last).Seconds()
}

func (w *Worker) gapAlarm() float64 {
	w.mu.RLock()
	state := w.snap.State
	last := w.snap.LastSegmentAt
	w.mu.RUnlock()
	if last == nil || state != StateReceiving {
		return 0
	}
	if time.Since(*last) > w.gapAllowance() {
		return 1
	}
	return 0
}

func (w *Worker) secondsSinceManifest() float64 {
	w.mu.RLock()
	last := w.lastManifestAt
	w.mu.RUnlock()
	if last.IsZero() {
		return 0
	}
	return time.Since(last).Seconds()
}

func (w *Worker) Camera() camera.Camera { return w.cam }

// Snapshot returns the current channel view for the health API.
func (w *Worker) Snapshot() Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	s := w.snap
	if s.LastSegmentAt != nil {
		gap := time.Since(*s.LastSegmentAt).Seconds()
		s.SecondsSinceSegment = &gap
	}
	s.Healthy = w.healthyLocked()
	return s
}

// healthyLocked reports whether the channel is doing its job. It is not the
// same as "ffmpeg is running": a channel that connects and then stops
// delivering segments is unhealthy.
func (w *Worker) healthyLocked() bool {
	if w.snap.State != StateReceiving {
		return false
	}
	if w.snap.LastSegmentAt == nil {
		return false
	}
	return time.Since(*w.snap.LastSegmentAt) <= w.gapAllowance()
}

// gapAllowance is how long a receiving channel may go without a segment
// before it is considered broken.
//
// The threshold is not the target duration. Because we never transcode, a
// camera with a long IDR interval legitimately emits segments slower than
// the target, so MaxGOPSlack plus one upload budget is added. Alarming at
// the target duration alone would fire constantly on long-GOP cameras.
func (w *Worker) gapAllowance() time.Duration {
	return w.cfg.Segment.TargetDuration.Duration +
		w.cfg.Segment.MaxGOPSlack.Duration +
		w.cfg.Upload.PutTimeout.Duration
}

// Run supervises the channel until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	defer func() {
		w.setState(StateStopped)
		w.log.Info("channel stopped")
	}()

	// Ownership first, then sequence, then ffmpeg. Each step is a gate:
	//
	//	1. Take the lease, so a second shard does not start a second ffmpeg
	//	   for this camera.
	//	2. Read the manifest to learn where to resume.
	//	3. Fence: rewrite the manifest conditionally on the version just read.
	//	   This is the step that actually revokes a previous owner's ability to
	//	   write, and it is why the lease alone is not enough -- acquiring a
	//	   lease cannot stop a frozen process from waking up and writing.
	//	4. Only now start ffmpeg.
	if !w.takeOwnership(ctx) {
		return
	}

	// The renewal goroutine outlives individual ffmpeg generations: ownership
	// belongs to the worker, not to a single pipeline run. Losing it cancels
	// ownedCtx, which unwinds everything below.
	ownedCtx, ownershipLost := context.WithCancel(ctx)
	defer ownershipLost()
	go w.runLeaseRenewal(ownedCtx, ownershipLost)
	defer w.releaseLease()

	generation := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if ownedCtx.Err() != nil {
			// Ownership is gone. Do not reconnect, do not drain: another shard
			// is publishing this camera now and our spool belongs to a
			// timeline that is no longer authoritative.
			w.log.Error("channel disabled: ownership of the published stream was lost")
			w.setState(StateFailed)
			w.mFailed.Inc()
			<-ctx.Done()
			return
		}
		generation++
		if generation > 1 {
			w.mReconnects.Inc()
			w.mu.Lock()
			w.snap.Reconnects++
			// Every restart begins a new continuity domain: timestamps and
			// parameter sets may not line up with the previous generation.
			w.pendingDiscontinuity = true
			w.mu.Unlock()
		}

		err := w.runGeneration(ownedCtx, generation)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrLeaseLost) {
			w.log.Error("channel disabled: another shard owns the published stream", "error", err)
			w.setError(err)
			w.setState(StateFailed)
			w.mFailed.Inc()
			<-ctx.Done()
			return
		}
		if err != nil {
			w.log.Warn("pipeline ended", "generation", generation, "error", err)
			w.setError(err)
		} else {
			w.log.Warn("pipeline ended without error", "generation", generation)
		}

		w.setState(StateDisconnected)

		// Reset the backoff only when the channel ran healthily for long
		// enough. Reconnecting successfully is not by itself success.
		if w.lastRunProducedFor(w.cfg.Reconnect.ResetAfter.Duration) {
			w.bo.Reset()
		}
		delay := w.bo.Next()
		w.setState(StateReconnecting)
		w.log.Info("reconnecting", "delay", delay.String(), "attempt", w.bo.Attempt())
		if !sleepCtx(ctx, delay) {
			return
		}
	}
}

// takeOwnership runs the acquire-recover-fence sequence, retrying while the
// camera is legitimately held elsewhere. It reports whether the worker may
// proceed to start ffmpeg.
func (w *Worker) takeOwnership(ctx context.Context) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		err := w.claim(ctx)
		switch {
		case err == nil:
			return true

		case errors.Is(err, ErrLeaseHeld):
			// Expected during a rolling deploy: the previous task still owns
			// the camera and is still serving it. Waiting is correct, and this
			// is not an operator's problem, so it must not be StateFailed.
			w.mLeaseContended.Inc()
			w.setState(StateWaitingOwnership)
			w.log.Info("camera owned by another shard, waiting", "error", err)
			if !sleepCtx(ctx, w.cfg.Lease.RenewInterval.Duration) {
				return false
			}

		case ctx.Err() != nil:
			return false

		case errors.Is(err, ErrLeaseUnavailable):
			// The store could not be consulted, so ownership is unknown. No
			// publication has been attempted, so retrying is safe -- unlike a
			// foreign manifest, nothing has been assumed about history.
			w.mLeaseAcquireFail.Inc()
			w.setState(StateWaitingOwnership)
			w.setError(err)
			w.log.Error("cannot establish camera ownership, retrying", "error", err)
			if !sleepCtx(ctx, w.cfg.Lease.RenewInterval.Duration) {
				return false
			}

		default:
			// Fail closed. One silent channel is recoverable by an operator;
			// a rewound sequence or a contested manifest corrupts the stream
			// for every viewer and is not.
			w.log.Error("channel disabled: cannot establish safe ownership of the published stream",
				"error", err)
			w.setError(err)
			w.setState(StateFailed)
			w.mFailed.Inc()
			<-ctx.Done()
			return false
		}
	}
}

// releaseLease hands the camera back so a successor need not wait out the TTL.
// A fresh context is used because the worker's own is already cancelled by the
// time this runs, and failure is tolerable: expiry is the fallback.
func (w *Worker) releaseLease() {
	ctx, cancel := context.WithTimeout(context.Background(), w.cfg.Lease.OperationTimeout.Duration)
	defer cancel()
	if err := w.lease.Release(ctx); err != nil {
		w.mLeaseReleaseFail.Inc()
		w.log.Warn("lease release failed; it will expire on its own", "error", err)
	}
	w.mLeaseHeld.Set(0)
}

// ErrLeaseUnavailable reports that ownership could not be determined because
// the object store did not answer. It is retryable; not knowing is different
// from knowing someone else owns the camera.
var ErrLeaseUnavailable = errors.New("camera ownership could not be determined")

// claim acquires the lease, recovers the sequence, and fences the manifest.
func (w *Worker) claim(ctx context.Context) error {
	leaseCtx, cancel := context.WithTimeout(ctx, w.cfg.Lease.OperationTimeout.Duration)
	err := w.lease.Acquire(leaseCtx)
	cancel()
	switch {
	case errors.Is(err, ErrLeaseHeld):
		return err
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrLeaseUnavailable, err)
	}
	w.mLeaseHeld.Set(1)
	w.log.Info("camera lease acquired", "key", w.objectKey(LeaseObjectName))

	w.mu.RLock()
	need := w.needsRecovery
	w.mu.RUnlock()
	if need {
		if err := w.recover(ctx); err != nil {
			return err
		}
	}
	return w.fenceManifest(ctx)
}

// fenceManifest is what actually transfers publication ownership.
//
// A lease says who should be publishing; it cannot stop a process that froze
// with a valid compare-and-swap token from waking up and using it. Consider
// two shards that both read manifest version E: A stalls, its lease expires,
// B acquires the lease and reads E. If B simply started publishing, A could
// wake and write conditionally on E successfully, and B's own write would then
// be the one rejected -- the stale writer would have fenced the live one.
//
// So the new owner rewrites the manifest first, conditional on the exact
// version it recovered from. The body carries a fresh write ID, which
// guarantees a different ETag even when the playlist entries are unchanged.
// Once that write lands, E is spent: every write A still has queued fails.
// ffmpeg does not start until this succeeds.
func (w *Worker) fenceManifest(ctx context.Context) error {
	w.mu.RLock()
	window := make([]hls.PublishedSegment, len(w.window))
	copy(window, w.window)
	discSeq := w.discontinuitySequence
	expected := w.manifestETag
	w.mu.RUnlock()

	etag, err := w.writeManifest(ctx, window, discSeq, manifestPrecondition(expected))
	if err != nil {
		if errors.Is(err, storage.ErrPreconditionFailed) {
			// Someone published between our read and our fence. Their write is
			// now the authority: re-read and fence again rather than assuming
			// anything about the sequence.
			w.mManifestConflict.Inc()
			w.log.Warn("manifest changed under us while fencing, re-reading")
			w.mu.Lock()
			w.needsRecovery = true
			w.mu.Unlock()
			return fmt.Errorf("%w: manifest changed while fencing", ErrLeaseUnavailable)
		}
		return fmt.Errorf("fence manifest: %w", err)
	}
	w.mu.Lock()
	w.manifestETag = etag
	w.mu.Unlock()
	w.log.Info("published stream fenced, this shard is now the writer")
	return nil
}

// manifestPrecondition never yields an unconditional write.
//
// An empty expected ETag means "no manifest existed when we looked", so the
// write must claim the key rather than overwrite whatever appeared since. An
// unconditional manifest write is precisely the bug this protocol removes, so
// there is no code path that produces one.
func manifestPrecondition(expected string) storage.Preconditions {
	if expected == "" {
		return storage.Preconditions{IfNoneMatch: true}
	}
	return storage.Preconditions{IfMatch: expected}
}

// runLeaseRenewal keeps the lease alive for as long as the worker owns the
// camera. It signals loss by cancelling lost.
func (w *Worker) runLeaseRenewal(ctx context.Context, lost context.CancelFunc) {
	t := time.NewTicker(w.cfg.Lease.RenewInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			opCtx, cancel := context.WithTimeout(ctx, w.cfg.Lease.OperationTimeout.Duration)
			err := w.lease.Renew(opCtx)
			cancel()
			switch {
			case err == nil:
				continue
			case errors.Is(err, ErrLeaseLost):
				// Another shard took over. Stop immediately; the manifest
				// fence would reject our writes anyway, but ffmpeg must not
				// keep running and spooling for a camera we do not own.
				w.mLeaseLost.Inc()
				w.mLeaseHeld.Set(0)
				w.setError(err)
				w.log.Error("camera lease lost to another owner, stopping", "error", err)
				lost()
				return
			case ctx.Err() != nil:
				return
			default:
				// Transient. Keep trying, but only while we are still inside
				// our own safety deadline: publishing past it would risk two
				// live owners.
				w.mLeaseRenewFail.Inc()
				w.log.Warn("lease renewal failed, will retry", "error", err)
				if w.lease.Expired() {
					w.mLeaseLost.Inc()
					w.mLeaseHeld.Set(0)
					w.log.Error("lease deadline passed without a successful renewal, stopping")
					lost()
					return
				}
			}
		}
	}
}

// lastRunProducedFor reports whether the channel published segments over a
// span of at least d during the run that just ended.
func (w *Worker) lastRunProducedFor(d time.Duration) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.snap.LastSegmentAt == nil || w.runStartedAt.IsZero() {
		return false
	}
	return w.snap.LastSegmentAt.Sub(w.runStartedAt) >= d
}

// runGeneration runs one ffmpeg process to completion.
func (w *Worker) runGeneration(ctx context.Context, generation int) error {
	// The previous generation's spool is about to be destroyed. Try once more
	// to flush it: the reconnect backoff has just elapsed, which is enough
	// time for a brief object-store outage to have cleared.
	w.flushPreviousGeneration(ctx)
	if err := w.accountDiscardedSpool(); err != nil {
		w.log.Warn("could not account for discarded spool files", "error", err)
	}
	if err := ffmpeg.EnsureSpool(w.spoolDir); err != nil {
		return err
	}
	args, err := ffmpeg.BuildArgs(ffmpeg.Spec{
		RTSPURL:  w.cam.RTSPURL,
		SpoolDir: w.spoolDir,
		Cfg:      w.cfg.FFmpeg,
		Segment:  w.cfg.Segment,
	})
	if err != nil {
		return err
	}

	proc, err := ffmpeg.Start(w.cfg.FFmpeg.Binary, args, w.log, w.cam.StderrRedactor())
	if err != nil {
		return err
	}
	// Stop is idempotent and safe after a natural exit, so this covers both
	// the cancellation path and the error paths below.
	defer func() {
		proc.Stop(w.cfg.FFmpeg.ShutdownGrace.Duration)
		w.mu.Lock()
		w.snap.PID = 0
		w.mu.Unlock()
	}()

	w.mu.Lock()
	w.snap.State = StateStarting
	w.snap.PID = proc.PID()
	w.runStartedAt = time.Now()
	w.mu.Unlock()
	w.mState.Set(stateCode(StateStarting))
	w.log.Info("ffmpeg started", "pid", proc.PID(), "generation", generation,
		"source", w.cam.SafeURL(), "transport", w.cfg.FFmpeg.RTSPTransport)

	// gen tracks per-generation drain state. ffmpeg restarts its local
	// numbering at zero, so this must not persist across generations. It is
	// handed to the next generation only so leftovers can be flushed before
	// the spool is cleared.
	gen := newDrainState()
	w.prevGen = gen

	ticker := time.NewTicker(w.cfg.FFmpeg.ScanInterval.Duration)
	defer ticker.Stop()

	lastProgress := time.Now()
	sawSegment := false

	for {
		select {
		case <-ctx.Done():
			w.setState(StateStopping)
			proc.Stop(w.cfg.FFmpeg.ShutdownGrace.Duration)
			w.finalDrain(gen)
			return ctx.Err()

		case <-proc.Done():
			// ffmpeg exited on its own. Flush whatever finished before it
			// died so a clean camera shutdown does not lose a segment.
			w.finalDrain(gen)
			if err := proc.Err(); err != nil {
				return fmt.Errorf("ffmpeg exited: %w", err)
			}
			return errors.New("ffmpeg exited with status 0 (source ended)")

		case <-ticker.C:
			res, err := w.drain(ctx, gen)
			if err != nil {
				if errors.Is(err, ErrLeaseLost) {
					// Not a transient upload fault: someone else is the
					// writer. Stop the pipeline instead of retrying forever.
					proc.Stop(w.cfg.FFmpeg.ShutdownGrace.Duration)
					return err
				}
				w.log.Warn("drain error", "error", err)
				w.setError(err)
			}
			// The watchdog is driven by Seen, not Uploaded. An object store
			// outage must not cause an ffmpeg restart: it would discard the
			// spooled segments and cannot fix the store.
			if res.Seen > 0 {
				lastProgress = time.Now()
				sawSegment = true
				w.mLastSeenTS.Set(float64(time.Now().Unix()))
				// "receiving" describes the RTSP leg. Whether segments are
				// reaching the object store is reported separately by
				// Healthy and by the segment-gap alarm.
				w.setState(StateReceiving)
			}

			// Watchdog. This, not an ffmpeg socket option, is the primary
			// disconnect detector: the flag names for RTSP socket timeouts
			// vary between ffmpeg releases, while "no segment completed for
			// N seconds" behaves identically everywhere.
			limit := w.cfg.FFmpeg.StallTimeout.Duration
			if !sawSegment {
				limit = w.cfg.FFmpeg.StartupTimeout.Duration
			}
			if time.Since(lastProgress) > limit {
				w.log.Warn("stall detected, terminating ffmpeg",
					"pid", proc.PID(), "limit", limit.String(), "saw_segment", sawSegment)
				proc.Stop(w.cfg.FFmpeg.ShutdownGrace.Duration)
				w.finalDrain(gen)
				return errStalled
			}
			w.refreshSpoolMetrics()
		}
	}
}

// finalDrain flushes the spool after ffmpeg has exited, on a bounded context
// so shutdown cannot hang on a slow object store.
func (w *Worker) finalDrain(gen *drainState) {
	if w.lease.Expired() {
		// Publishing here would race whoever took the camera over.
		w.log.Warn("skipping final drain: this shard no longer owns the channel")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), finalDrainTimeout)
	defer cancel()
	// This is the last chance to publish, so the manifest retry rate limit
	// does not apply.
	w.manifestRetryAfter = time.Time{}
	if res, err := w.drain(ctx, gen); err != nil {
		w.mDrainFail.Inc()
		w.log.Warn("final drain incomplete", "uploaded", res.Uploaded,
			"manifest_pending", w.manifestDirty, "error", err)
	} else if res.Uploaded > 0 {
		w.log.Info("final drain flushed segments", "count", res.Uploaded)
	}
}

// finalDrainTimeout bounds the post-exit flush. A fresh context is used
// rather than the parent because on SIGTERM the parent is already cancelled
// and every upload would fail instantly.
const finalDrainTimeout = 15 * time.Second

// flushPreviousGeneration retries the previous generation's spool one last
// time before it is cleared.
//
// The final drain after ffmpeg exited may have failed because the object
// store was unavailable. By the time the next generation starts, the
// reconnect backoff has elapsed, so a transient outage has had time to clear
// and those segments can still be saved.
func (w *Worker) flushPreviousGeneration(ctx context.Context) {
	if w.prevGen == nil {
		return
	}
	gen := w.prevGen
	w.prevGen = nil
	if ctx.Err() != nil || w.lease.Expired() {
		return
	}
	drainCtx, cancel := context.WithTimeout(ctx, finalDrainTimeout)
	defer cancel()
	res, err := w.drain(drainCtx, gen)
	if err != nil {
		w.log.Warn("carry-over drain incomplete, spooled segments will be discarded",
			"uploaded", res.Uploaded, "error", err)
		return
	}
	if res.Uploaded > 0 {
		w.log.Info("carry-over drain rescued segments from the previous generation",
			"count", res.Uploaded)
	}
}

// accountDiscardedSpool counts segments that are about to be destroyed with
// the spool so the loss is visible instead of silent.
//
// EnsureSpool wipes the directory before every generation because a leftover
// segment's sequence belongs to a dead generation and its duration lives in a
// playlist that is being replaced. That is the right call, but the segments
// were real and must show up in segments_lost_total.
func (w *Worker) accountDiscardedSpool() error {
	entries, err := os.ReadDir(w.spoolDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	discarded := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		discarded++
	}
	if discarded > 0 {
		w.recordLost(discarded)
		w.log.Error("discarding spooled segments that were never stored",
			"count", discarded, "spool", w.spoolDir)
	}
	return nil
}

// drainState is per-generation bookkeeping for the spool scanner.
type drainState struct {
	// lastLocalIndex is the highest ffmpeg segment index already handled,
	// meaning uploaded or accounted for as lost.
	lastLocalIndex int
	// maxSeenIndex is the highest index ever observed in the local playlist.
	// It advances only when ffmpeg actually finishes a new segment, which is
	// what makes it a trustworthy RTSP liveness signal.
	maxSeenIndex int
	// processed guards against re-uploading a segment that is still listed
	// in the local playlist on the next scan tick.
	processed map[string]bool
}

// drainResult separates the two things a scan can observe, because they
// belong to different failure domains and demand different responses.
//
//	Seen == 0     ffmpeg is not producing. The camera or the RTSP session is
//	              broken, so restarting ffmpeg is the correct remedy.
//	Uploaded == 0 while Seen > 0: the object store is failing. Restarting
//	              ffmpeg would not help and would discard the spooled
//	              segments, so the pipeline is left running to retry.
type drainResult struct {
	Seen     int
	Uploaded int
}

func newDrainState() *drainState {
	return &drainState{lastLocalIndex: -1, maxSeenIndex: -1, processed: make(map[string]bool)}
}

// drain uploads every newly finished segment, then republishes the manifest.
//
// The function runs two passes on purpose.
//
// Pass 1 observes. It walks the whole playlist and records the highest
// segment index ffmpeg has produced. This has to happen before any upload is
// attempted, because pass 2 stops at the first upload failure; if observation
// were interleaved, a store outage would hide the fact that the camera is
// still delivering and the stall watchdog would kill a perfectly healthy
// ffmpeg.
//
// Pass 2 uploads, in playlist order, and stops at the first failure. Ordering
// is the whole point: the manifest is published only after every segment it
// references is confirmed stored. A manifest referencing a missing segment
// breaks playback for every viewer at once, whereas a stored segment that is
// not yet listed is invisible and harmless.
func (w *Worker) drain(ctx context.Context, gen *drainState) (drainResult, error) {
	playlistPath := filepath.Join(w.spoolDir, ffmpeg.LocalPlaylistName)
	raw, err := os.ReadFile(playlistPath)
	if err != nil {
		if os.IsNotExist(err) {
			// ffmpeg has not written the playlist yet.
			return drainResult{}, nil
		}
		return drainResult{}, fmt.Errorf("read local playlist: %w", err)
	}
	local, err := hls.ParseLocal(raw)
	if err != nil {
		// A torn read of the playlist is expected occasionally; ffmpeg
		// rewrites it in place. The next tick will pick it up.
		return drainResult{}, nil
	}

	var res drainResult

	// Pass 1: observation only.
	for _, seg := range local {
		idx, ok := parseLocalIndex(seg.Name)
		if !ok {
			continue
		}
		if idx > gen.maxSeenIndex {
			gen.maxSeenIndex = idx
			res.Seen++
		}
	}

	// Pass 2: upload.
	var firstErr error
	for _, seg := range local {
		if gen.processed[seg.Name] {
			continue
		}
		idx, ok := parseLocalIndex(seg.Name)
		if !ok {
			continue
		}
		// The playlist is a file on disk; treat its contents as untrusted
		// input rather than joining an arbitrary string onto the spool path.
		if !ffmpeg.ValidSegmentName(seg.Name) {
			w.log.Error("ignoring a playlist entry that is not a segment name",
				"name", seg.Name)
			gen.processed[seg.Name] = true
			continue
		}
		// Detect segments that ffmpeg created and then reclaimed before we
		// got to them. This is real data loss and must be visible.
		if gen.lastLocalIndex >= 0 && idx > gen.lastLocalIndex+1 {
			missed := idx - gen.lastLocalIndex - 1
			w.recordLost(missed)
			w.log.Error("segments reclaimed before upload",
				"count", missed, "from_index", gen.lastLocalIndex+1, "to_index", idx-1)
		}

		if err := w.publishSegment(ctx, seg); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Already reclaimed by ffmpeg's delete_segments between the
				// playlist being written and the upload slot being granted.
				gen.processed[seg.Name] = true
				if idx > gen.lastLocalIndex {
					gen.lastLocalIndex = idx
				}
				w.recordLost(1)
				continue
			}
			// Leave it unprocessed so the next tick retries while the file
			// still exists. The manifest is not advanced past it.
			if firstErr == nil {
				firstErr = err
			}
			break
		}

		gen.processed[seg.Name] = true
		if idx > gen.lastLocalIndex {
			gen.lastLocalIndex = idx
		}
		res.Uploaded++
		// The local copy is no longer needed once it is in the object store.
		_ = os.Remove(filepath.Join(w.spoolDir, seg.Name))
	}

	// Publish whenever the window has segments the stored manifest does not
	// reference yet. Keying this off res.Uploaded would strand a failed
	// manifest forever: the segments are already uploaded and deleted from
	// the spool, so a later scan sees no new upload and would never retry.
	//
	// A pure retry is rate limited to one target duration. Scanning runs
	// every few hundred milliseconds, and several hundred channels retrying
	// a manifest at that rate through a store outage is a log and metric
	// storm that tells an operator nothing new.
	if w.manifestDirty && (res.Uploaded > 0 || !time.Now().Before(w.manifestRetryAfter)) {
		if err := w.publishManifest(ctx); err != nil {
			w.manifestRetryAfter = time.Now().Add(w.cfg.Segment.TargetDuration.Duration)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	// Bound the processed set: entries for segments that have scrolled out
	// of ffmpeg's playlist can never reappear within this generation.
	if len(gen.processed) > 4*w.cfg.Segment.LocalListSize {
		pruneProcessed(gen, local)
	}
	return res, firstErr
}

// publishSegment uploads one segment and appends it to the live window.
//
// The body is not read here: PutFile reads it after taking an upload slot, so
// the number of segments resident in memory is bounded by upload concurrency
// rather than by channel count.
func (w *Worker) publishSegment(ctx context.Context, seg hls.LocalSegment) error {
	w.mu.Lock()
	seqNo := w.lastSequence + 1
	// A discontinuity comes from either side: the supervisor knows about
	// reconnects and restarts, and ffmpeg reports the ones it saw inside a
	// single run. Dropping ffmpeg's would leave players without a timeline
	// reset across a mid-run parameter-set or timestamp change.
	disc := w.pendingDiscontinuity || seg.Discontinuity
	w.mu.Unlock()

	pdt := seg.ProgramDateTime
	if pdt.IsZero() {
		// ffmpeg is asked for program_date_time, so this should not happen.
		// Falling back to now breaks the documented rule that the date
		// directory follows capture time, and makes the object key change
		// between retries, so it is recorded rather than passed over.
		pdt = time.Now().UTC()
		w.mMissingPDT.Inc()
		w.log.Warn("segment has no EXT-X-PROGRAM-DATE-TIME, filing it under the current time",
			"segment", seg.Name, "sequence", seqNo)
	}
	// The date directory comes from the segment's own wall-clock anchor, not
	// from time.Now at upload time, so a slow upload near midnight still
	// files the segment under the day it was captured.
	relURI := path.Join(pdt.UTC().Format("2006/01/02"),
		fmt.Sprintf("seg-%09d-%d.ts", seqNo, pdt.UTC().UnixMilli()))
	key := w.objectKey(relURI)

	// The write identity must be stable across retries of the same segment,
	// or a store hiccup would make our own landed write look like a foreign
	// one and fail the channel closed. It is derived from the owner session
	// and the sequence, both of which are fixed for a given segment: a retry
	// recomputes the same sequence because a failed publish does not advance
	// it, while a different owner necessarily has a different session.
	putID := w.session + ":" + strconv.FormatUint(seqNo, 10)
	size, _, err := w.uploader.PutFile(ctx, storage.Object{
		Key:         key,
		ContentType: hls.ContentTypeSegment,
		// Segments are immutable, so they can be cached indefinitely.
		CacheControl: "public, max-age=31536000, immutable",
		// Written now so a recording index can be built later without
		// re-reading every object. The duration in particular cannot be
		// recovered from the key: inferring it from the next segment's
		// timestamp is wrong across a camera disconnect.
		//
		// put-id identifies the writer and sequence, so an ambiguous timeout
		// can be resolved without overwriting a segment somebody else stored.
		Metadata: map[string]string{
			"sequence":      strconv.FormatUint(seqNo, 10),
			"pdt-ms":        strconv.FormatInt(pdt.UTC().UnixMilli(), 10),
			"duration-ms":   strconv.FormatInt(seg.Duration.Milliseconds(), 10),
			"discontinuity": strconv.FormatBool(disc),
			"center-id":     w.cam.CenterID,
			"camera-id":     w.cam.CameraID,
			"put-id":        putID,
		},
		// Create-only. A segment key is immutable and cached for a year, so
		// overwriting one is cache poisoning rather than a correction. If the
		// key already exists, either this is our own retry after an ambiguous
		// success, or another writer produced a different body under the same
		// key -- and those must not be confused.
		Preconditions: storage.Preconditions{IfNoneMatch: true},
	}, filepath.Join(w.spoolDir, seg.Name))
	if err != nil {
		// A vanished file is loss, not an upload failure; the caller counts it.
		if errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if errors.Is(err, storage.ErrPreconditionFailed) {
			resolved, rerr := w.resolveSegmentConflict(ctx, key, putID)
			if rerr != nil {
				w.recordSegmentFailure(rerr)
				return rerr
			}
			size = resolved
		} else {
			w.recordSegmentFailure(err)
			return err
		}
	}

	published := hls.PublishedSegment{
		Sequence:        seqNo,
		URI:             relURI,
		Duration:        seg.Duration,
		ProgramDateTime: pdt,
		Discontinuity:   disc,
		Bytes:           size,
	}

	now := time.Now().UTC()
	w.mu.Lock()
	w.lastSequence = seqNo
	w.pendingDiscontinuity = false
	w.window = append(w.window, published)
	for len(w.window) > w.cfg.Segment.LiveWindow {
		// A discontinuity that scrolls out of the window must be accounted
		// for in EXT-X-DISCONTINUITY-SEQUENCE or players lose their
		// timeline across a reconnect.
		if w.window[0].Discontinuity {
			w.discontinuitySequence++
		}
		w.window = w.window[1:]
	}
	w.snap.SegmentsPublished++
	w.snap.BytesPublished += size
	w.snap.LastSequence = seqNo
	w.snap.LastSegmentAt = &now
	w.mu.Unlock()

	// The segment is stored but no manifest references it yet.
	w.manifestDirty = true

	w.mSegments.Inc()
	w.mSegBytes.Add(float64(size))
	w.mLastSegTS.Set(float64(now.Unix()))
	w.mSegDuration.Set(seg.Duration.Seconds())

	w.log.Debug("segment published",
		"sequence", seqNo, "key", key, "bytes", size,
		"duration_s", seg.Duration.Seconds(), "discontinuity", disc)
	return nil
}

// resolveSegmentConflict decides what a refused create means.
//
// Two cases produce it and they demand opposite responses. If the stored
// object carries our own put-id, our earlier attempt landed and only the
// response was lost: adopt it. If it carries anything else, a different writer
// put different bytes under a key we consider ours, which means two shards are
// publishing the same camera. That is the failure the whole ownership protocol
// exists to prevent, and continuing would put a segment in the manifest that
// is not the one we produced.
func (w *Worker) resolveSegmentConflict(ctx context.Context, key, putID string) (int64, error) {
	info, err := w.uploader.Head(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("segment %s already exists and could not be inspected: %w", key, err)
	}
	if info.Metadata["put-id"] == putID {
		w.log.Warn("segment upload reported a conflict but our own write had landed",
			"key", key, "bytes", info.Size)
		return info.Size, nil
	}
	w.mSegmentConflict.Inc()
	return 0, fmt.Errorf("%w: segment %s was written by another owner (put-id %q)",
		ErrLeaseLost, key, info.Metadata["put-id"])
}

// publishManifest renders and uploads the live playlist, then checkpoints.
//
// The write is conditional on the manifest version this worker last wrote or
// fenced. That condition is the ownership check on the hot path: if another
// shard has taken the channel over, this write is refused and the worker knows
// to stop rather than overwrite the new owner's timeline.
func (w *Worker) publishManifest(ctx context.Context) error {
	w.mu.RLock()
	window := make([]hls.PublishedSegment, len(w.window))
	copy(window, w.window)
	discSeq := w.discontinuitySequence
	lastSeq := w.lastSequence
	expected := w.manifestETag
	w.mu.RUnlock()

	// Refuse to publish past our own ownership deadline. If the store has
	// stopped answering renewals, another shard may already have taken over.
	if w.lease.Expired() {
		err := fmt.Errorf("%w: refusing to publish past the lease deadline", ErrLeaseLost)
		w.recordManifestFailure(err)
		return err
	}

	etag, err := w.writeManifest(ctx, window, discSeq, manifestPrecondition(expected))
	if err != nil {
		if errors.Is(err, storage.ErrPreconditionFailed) {
			w.mManifestConflict.Inc()
			w.recordManifestFailure(err)
			return fmt.Errorf("%w: manifest was overwritten by another writer", ErrLeaseLost)
		}
		w.recordManifestFailure(err)
		return err
	}
	// Every segment currently in the window is now referenced by a stored
	// manifest.
	w.manifestDirty = false
	now := time.Now()
	w.mu.Lock()
	w.manifestETag = etag
	w.lastManifestAt = now
	w.mu.Unlock()
	w.mManifestTS.Set(float64(now.Unix()))

	if err := saveCheckpoint(w.statePath, checkpoint{
		LastSequence:          lastSeq,
		DiscontinuitySequence: discSeq,
		Window:                windowToCheckpoint(window),
	}); err != nil {
		// Genuinely non-fatal: the published manifest is the authority on
		// startup and the checkpoint is only a floor, so losing it costs a
		// manifest GET at the next start and nothing else.
		w.log.Warn("checkpoint write failed", "error", err)
	}
	return nil
}

// writeManifest renders and stores one manifest version, resolving an
// ambiguous outcome by reading back rather than retrying.
//
// A conditional write cannot be retried blindly: if the first attempt landed
// and only the response was lost, the retry sees its own precondition already
// consumed and reports a conflict that never happened. The write ID in the
// body is what distinguishes "my write landed" from "someone else wrote".
func (w *Worker) writeManifest(ctx context.Context, window []hls.PublishedSegment,
	discSeq uint64, pre storage.Preconditions) (string, error) {

	writeID := newID()
	body := hls.RenderLive(window, discSeq, writeID)
	key := w.objectKey(w.cfg.Storage.ManifestName)

	etag, err := w.uploader.Put(ctx, storage.Object{
		Key:         key,
		Body:        body,
		ContentType: hls.ContentTypeManifest,
		// The manifest is mutable and must never be cached by a CDN or
		// browser, otherwise viewers stall on a stale window.
		CacheControl:  "no-cache, max-age=0",
		Preconditions: pre,
	})
	if err == nil {
		return etag, nil
	}
	if errors.Is(err, storage.ErrPreconditionFailed) || ctx.Err() != nil {
		return "", err
	}
	// Ambiguous. Read back: if the stored manifest carries our write ID, the
	// write landed and we own the resulting version.
	stored, storedETag, gerr := w.uploader.Get(ctx, key)
	if gerr != nil {
		return "", err
	}
	if pub, perr := hls.ParsePublished(stored); perr == nil && pub.WriteID == writeID {
		w.log.Warn("manifest write reported an error but landed; adopting it", "error", err)
		return storedETag, nil
	}
	return "", err
}

// objectKey builds the full key from the configured prefix and the spec's
// {center_id}/{camera_id}/... layout.
func (w *Worker) objectKey(rel string) string {
	parts := make([]string, 0, 4)
	if p := strings.Trim(w.cfg.Storage.KeyPrefix, "/"); p != "" {
		parts = append(parts, p)
	}
	parts = append(parts, w.cam.CenterID, w.cam.CameraID, rel)
	return path.Join(parts...)
}

func (w *Worker) setState(s State) {
	w.mu.Lock()
	w.snap.State = s
	w.mu.Unlock()
	w.mState.Set(stateCode(s))
}

func (w *Worker) setError(err error) {
	now := time.Now().UTC()
	w.mu.Lock()
	w.snap.LastErrorAt = &now
	w.snap.LastError = err.Error()
	w.mu.Unlock()
}

func (w *Worker) recordLost(n int) {
	if n <= 0 {
		return
	}
	w.mLost.Add(float64(n))
	w.mu.Lock()
	w.snap.SegmentsLost += uint64(n)
	w.mu.Unlock()
}

// recordSegmentFailure and recordManifestFailure both feed the combined
// counter so existing alarms keep working, and a specific one so an operator
// can tell a media backlog from a channel whose segments are stored but
// invisible.
func (w *Worker) recordSegmentFailure(err error) {
	w.mSegmentFail.Inc()
	w.recordUploadFailure(err)
}

func (w *Worker) recordManifestFailure(err error) {
	w.mManifestFail.Inc()
	w.recordUploadFailure(err)
}

func (w *Worker) recordUploadFailure(err error) {
	w.mUploadFail.Inc()
	w.mu.Lock()
	w.snap.UploadFailures++
	w.mu.Unlock()
	w.setError(err)
}

// refreshSpoolMetrics reports what is waiting on the spool.
//
// Rate limited to one scan per target duration: a readdir plus a stat per file
// on every tick, for every channel, is real work for a number that cannot
// change faster than segments are produced.
func (w *Worker) refreshSpoolMetrics() {
	if time.Since(w.spoolStatsAt) < w.cfg.Segment.TargetDuration.Duration {
		return
	}
	w.spoolStatsAt = time.Now()

	entries, err := os.ReadDir(w.spoolDir)
	if err != nil {
		return
	}
	var files, bytes int64
	oldest := time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files++
		bytes += info.Size()
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	w.mSpoolFiles.Set(float64(files))
	w.mSpoolBytes.Set(float64(bytes))
	if oldest.IsZero() {
		w.mSpoolOldest.Set(0)
	} else {
		w.mSpoolOldest.Set(time.Since(oldest).Seconds())
	}
}

// Cleanup removes the channel's spool directory and metric series. Called
// when a camera is removed from the roster.
func (w *Worker) Cleanup() {
	if err := os.RemoveAll(w.spoolDir); err != nil {
		w.log.Warn("spool cleanup failed", "error", err)
	}
	w.reg.DropSeries(
		metrics.Label{Name: "center_id", Value: w.cam.CenterID},
		metrics.Label{Name: "camera_id", Value: w.cam.CameraID},
	)
}

func parseLocalIndex(name string) (int, bool) {
	base := strings.TrimSuffix(name, ".ts")
	i := strings.LastIndexByte(base, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(base[i+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// pruneProcessed drops bookkeeping for segments no longer in the playlist.
func pruneProcessed(gen *drainState, local []hls.LocalSegment) {
	live := make(map[string]bool, len(local))
	for _, s := range local {
		live[s.Name] = true
	}
	for name := range gen.processed {
		if !live[name] {
			delete(gen.processed, name)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
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
