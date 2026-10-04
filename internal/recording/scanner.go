package recording

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/storage"
)

type RosterFunc func(context.Context) ([]config.StaticCamera, error)

type Scanner struct {
	Index        *Index
	Store        storage.MediaStore
	Roster       RosterFunc
	Prefix       string
	ManifestName string
	Days         int
	Workers      int
	Interval     time.Duration
	// ScanTimeout bounds one camera-day page. Zero means 30 seconds.
	ScanTimeout time.Duration
	Log         *slog.Logger

	indexed  atomic.Uint64
	removed  atomic.Uint64
	rejected atomic.Uint64
	failures atomic.Uint64
	lastPass atomic.Int64
	lastLive atomic.Int64
	active   atomic.Bool
}

type Status struct {
	Indexed  uint64    `json:"indexed_since_start"`
	Removed  uint64    `json:"removed_since_start"`
	Rejected uint64    `json:"rejected_since_start"`
	Failures uint64    `json:"failures_since_start"`
	LastPass time.Time `json:"last_pass"`
	Scanning bool      `json:"scanning"`
	LastLive time.Time `json:"last_live_scan"`
}

func (s *Scanner) Status() Status {
	last := time.Time{}
	if ms := s.lastPass.Load(); ms != 0 {
		last = time.UnixMilli(ms).UTC()
	}
	live := time.Time{}
	if ms := s.lastLive.Load(); ms != 0 {
		live = time.UnixMilli(ms).UTC()
	}
	return Status{s.indexed.Load(), s.removed.Load(), s.rejected.Load(), s.failures.Load(), last, s.active.Load(), live}
}

func (s *Scanner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	// Full reconciliation has its own small worker budget. A multi-day
	// rebuild cannot delay indexing new live segments.
	wg.Go(func() {
		ticker := time.NewTicker(s.Interval)
		defer ticker.Stop()
		for {
			s.Pass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	defer wg.Wait()
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		s.LivePass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Scanner) Pass(ctx context.Context) {
	if !s.active.CompareAndSwap(false, true) {
		return
	}
	defer s.active.Store(false)
	cameras, err := s.Roster(ctx)
	if err != nil {
		s.failures.Add(1)
		s.Log.Warn("recording index roster unavailable", "error", err)
		return
	}
	// Days outside the scan window are never reconciled again. Dropping
	// them bounds the file; S3 stays the source of truth for older media.
	now := time.Now().UTC()
	if s.Days > 0 {
		if _, err := s.Index.Prune(midnight(now).AddDate(0, 0, 1-s.Days), 64); err != nil {
			s.failures.Add(1)
			s.Log.Warn("recording index prune failed", "error", err)
		}
	}
	type job struct {
		camera config.StaticCamera
		day    time.Time
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	for range max(1, s.Workers/4) {
		wg.Go(func() {
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if err := s.ScanDay(ctx, j.camera, j.day); err != nil && ctx.Err() == nil {
					s.failures.Add(1)
					s.Log.Warn("recording index scan failed", "center_id", j.camera.CenterID,
						"camera_id", j.camera.CameraID, "day", j.day.Format("2006-01-02"), "error", err)
				}
			}
		})
	}
	// Recent recordings are indexed first, including yesterday's boundary
	// segment. Backfill gets a bounded page per camera/day on every pass.
send:
	for offset := 0; offset < s.Days; offset++ {
		day := midnight(now).AddDate(0, 0, -offset)
		for _, cam := range cameras {
			select {
			case jobs <- job{cam, day}:
			case <-ctx.Done():
				break send
			}
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() == nil {
		s.lastPass.Store(time.Now().UnixMilli())
	}
}

func (s *Scanner) LivePass(ctx context.Context) {
	cameras, err := s.Roster(ctx)
	if err != nil {
		s.failures.Add(1)
		return
	}
	jobs := make(chan config.StaticCamera)
	var wg sync.WaitGroup
	for range max(1, s.Workers-max(1, s.Workers/4)) {
		wg.Go(func() {
			for cam := range jobs {
				if ctx.Err() != nil {
					continue
				}
				err := s.scanLive(ctx, cam)
				if err != nil && !errors.Is(err, storage.ErrNotFound) && ctx.Err() == nil {
					s.failures.Add(1)
				}
			}
		})
	}
send:
	for _, cam := range cameras {
		select {
		case jobs <- cam:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() == nil {
		s.lastLive.Store(time.Now().UnixMilli())
	}
}

func (s *Scanner) scanLive(ctx context.Context, cam config.StaticCamera) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base := path.Join(s.Prefix, cam.CenterID, cam.CameraID)
	name := s.ManifestName
	if name == "" {
		name = "index.m3u8"
	}
	raw, _, err := s.Store.Get(ctx, path.Join(base, name))
	if err != nil {
		return err
	}
	live, err := hls.ParsePublished(raw)
	if err != nil {
		return err
	}
	var pending []Segment
	for _, segment := range live.Window {
		if _, err := s.Index.Get(cam.CenterID, cam.CameraID, segment.URI); err == nil {
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		entry := storage.Entry{Key: path.Join(base, segment.URI)}
		info, err := s.Store.Head(ctx, entry.Key)
		if err != nil {
			return err
		}
		record, err := FromObject(s.Prefix, cam.CenterID, cam.CameraID, entry, info)
		if err != nil {
			return err
		}
		pending = append(pending, record)
	}
	if err := s.Index.Put(pending); err != nil {
		return err
	}
	s.indexed.Add(uint64(len(pending)))
	return nil
}

// ScanDay indexes one listed page of a camera-day and removes index entries
// whose objects the page shows are gone. Progress is saved even when the time
// budget or a store error ends the page early, so a slow store still advances.
func (s *Scanner) ScanDay(ctx context.Context, cam config.StaticCamera, day time.Time) error {
	parent := ctx
	timeout := s.ScanTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	state, err := s.Index.ScanState(cam.CenterID, cam.CameraID, day)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	old := day.Before(midnight(now).AddDate(0, 0, -1))
	if state.Complete && old && now.Sub(state.ScannedAt) < 24*time.Hour {
		return nil
	}
	// A daily full reconciliation catches delayed writes inserted before
	// the lexical cursor and deletions anywhere in the day. Frequent passes
	// use an overlap at the live tail.
	if state.CycleStarted.IsZero() || now.Sub(state.CycleStarted) >= 24*time.Hour && state.Complete {
		state.Cursor, state.CycleStarted, state.Complete = "", now, false
	}
	base := path.Join(s.Prefix, cam.CenterID, cam.CameraID) + "/"
	prefix := base + day.Format("2006/01/02") + "/"
	listedAt := time.Now()
	page, err := s.Store.List(ctx, prefix, state.Cursor, 256)
	if err != nil {
		return err
	}
	// Deletions are reconciled before any HEAD so that a page cut short by
	// the time budget still drops objects a Lifecycle rule has removed.
	listed := make([]string, len(page.Entries))
	for n, entry := range page.Entries {
		listed[n] = strings.TrimPrefix(entry.Key, base)
	}
	upto := ""
	if page.More && len(listed) > 0 {
		upto = listed[len(listed)-1]
	}
	removed, err := s.Index.Reconcile(cam.CenterID, cam.CameraID, day,
		strings.TrimPrefix(state.Cursor, base), upto, listed, listedAt)
	if err != nil {
		return err
	}
	s.removed.Add(uint64(removed))
	var pending []Segment
	done := state.Cursor
	var scanErr error
	for n, entry := range page.Entries {
		record, ok, err := s.indexEntry(ctx, cam, entry, listed[n])
		if err != nil {
			scanErr = err
			break
		}
		if ok {
			pending = append(pending, record)
		}
		done = entry.Key
	}
	if err := s.Index.Put(pending); err != nil {
		return err
	}
	s.indexed.Add(uint64(len(pending)))
	if scanErr != nil {
		if done == state.Cursor {
			return scanErr
		}
		// Resume after the last object handled instead of repeating the
		// page. Running out of the page budget is progress, not a failure.
		state.Cursor, state.Complete = done, false
		if err := s.Index.SaveScan(cam.CenterID, cam.CameraID, day, state); err != nil {
			return err
		}
		if errors.Is(scanErr, context.DeadlineExceeded) && parent.Err() == nil {
			return nil
		}
		return scanErr
	}
	if len(page.Entries) > 0 {
		if page.More || old {
			state.Cursor = page.Entries[len(page.Entries)-1].Key
		} else if len(page.Entries) > 64 {
			state.Cursor = page.Entries[len(page.Entries)-65].Key
		}
	}
	state.Complete, state.ScannedAt = !page.More, now
	return s.Index.SaveScan(cam.CenterID, cam.CameraID, day, state)
}

// indexEntry returns the record of one listed object that is not yet indexed.
// ok is false for skipped objects: other names, already indexed, deleted after
// the listing or carrying invalid metadata.
func (s *Scanner) indexEntry(ctx context.Context, cam config.StaticCamera, entry storage.Entry,
	uri string) (Segment, bool, error) {
	if _, _, err := hls.SegmentIdentity(uri); err != nil {
		return Segment{}, false, nil
	}
	if _, err := s.Index.Get(cam.CenterID, cam.CameraID, uri); err == nil {
		return Segment{}, false, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Segment{}, false, err
	}
	info, err := s.Store.Head(ctx, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return Segment{}, false, nil // retention may remove an object after LIST
	}
	if err != nil {
		return Segment{}, false, err
	}
	record, err := FromObject(s.Prefix, cam.CenterID, cam.CameraID, entry, info)
	if err != nil {
		s.rejected.Add(1)
		s.Log.Warn("recording metadata rejected", "object", entry.Key, "error", err)
		return Segment{}, false, nil
	}
	return record, true, nil
}
