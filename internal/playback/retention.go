package playback

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

type RetentionStatus struct {
	Enabled bool      `json:"enabled"`
	Days    int       `json:"days"`
	Running bool      `json:"running"`
	LastRun time.Time `json:"last_run"`
	Deleted uint64    `json:"deleted_since_start"`
	Errors  uint64    `json:"errors_since_start"`
}

func (s *Server) runRetention(ctx context.Context) {
	s.retentionMu.Lock()
	s.retention.Enabled, s.retention.Days = s.cfg.Retention.Enabled, s.cfg.Retention.Days
	s.retentionMu.Unlock()
	ticker := time.NewTicker(s.cfg.Retention.Interval.Duration)
	defer ticker.Stop()
	nextCamera := 0
	for {
		if s.cfg.Retention.Enabled {
			nextCamera = s.retentionPass(ctx, nextCamera)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) retentionPass(ctx context.Context, first int) int {
	s.retentionMu.Lock()
	s.retention.Running = true
	s.retentionMu.Unlock()
	defer func() {
		s.retentionMu.Lock()
		s.retention.Running, s.retention.LastRun = false, time.Now().UTC()
		s.retentionMu.Unlock()
	}()
	roster, _, err := s.loadRoster(ctx)
	if err != nil {
		s.retentionError(err)
		return first
	}
	if len(roster) == 0 {
		return 0
	}
	// Deletion uses complete UTC capture days. A few extra hours of
	// retention avoid deleting an init object used by a midnight fragment.
	cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -s.cfg.Retention.Days)
	budget := s.cfg.Retention.Batch
	next := first % len(roster)
	for n := 0; n < len(roster) && budget > 0 && ctx.Err() == nil; n++ {
		cam := roster[next]
		next = (next + 1) % len(roster)
		cursorName := "retention/" + cam.CenterID + "/" + cam.CameraID
		cursor, err := s.index.Cursor(cursorName)
		if err != nil {
			s.retentionError(err)
			continue
		}
		count, after, err := s.retainCamera(ctx, cam, cutoff, cursor, min(64, budget))
		budget -= count
		if err != nil {
			s.retentionError(err)
			continue
		}
		if err := s.index.SaveCursor(cursorName, after); err != nil {
			s.retentionError(err)
		}
	}
	return next
}

// Retention lists the source of truth rather than relying on index coverage:
// it must still work after losing the index or importing an existing bucket.
func (s *Server) retainCamera(ctx context.Context, cam config.StaticCamera, cutoff time.Time,
	after string, limit int) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	base := path.Join(s.ingest.Storage.KeyPrefix, cam.CenterID, cam.CameraID) + "/"
	page, err := s.store.List(ctx, base, after, limit)
	if err != nil {
		return 0, after, err
	}
	processed := 0
	for _, entry := range page.Entries {
		processed++
		uri := strings.TrimPrefix(entry.Key, base)
		seq, start, segmentErr := hls.SegmentIdentity(uri)
		_ = seq
		isInit := hls.ValidInitURI(uri)
		if segmentErr != nil && !isInit {
			after = entry.Key
			continue // lease, live manifest and unrelated objects are excluded
		}
		day, _ := time.Parse("2006/01/02", uri[:10])
		if !day.Add(24*time.Hour + recording.MaxSegmentDuration).Before(cutoff) {
			// Date prefixes sort before the control namespace. This is the
			// end of the expired portion; begin a fresh sweep next time.
			return processed, "", nil
		}
		info, err := s.store.Head(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			after = entry.Key
			continue
		}
		if err != nil {
			return processed, after, err
		}
		var record recording.Segment
		if isInit {
			hash := strings.TrimSuffix(strings.TrimPrefix(path.Base(uri), "init-"), ".mp4")
			if info.Metadata["center-id"] != cam.CenterID || info.Metadata["camera-id"] != cam.CameraID ||
				info.Metadata["put-id"] != "init:"+hash {
				after = entry.Key
				continue
			}
		} else {
			record, err = recording.FromObject(s.ingest.Storage.KeyPrefix, cam.CenterID, cam.CameraID, entry, info)
			if err != nil || !start.Before(cutoff) || record.End().After(cutoff) {
				after = entry.Key
				continue
			}
		}
		if err := s.store.Delete(ctx, entry.Key); err != nil {
			return processed, after, err
		}
		if !isInit {
			if err := s.index.Remove(record); err != nil {
				return processed, after, err
			}
		}
		s.retentionMu.Lock()
		s.retention.Deleted++
		s.retentionMu.Unlock()
		after = entry.Key
	}
	if !page.More {
		after = ""
	}
	return processed, after, nil
}

func (s *Server) retentionError(err error) {
	s.retentionMu.Lock()
	s.retention.Errors++
	s.retentionMu.Unlock()
	s.log.Warn("recording retention pass failed", "error", err)
}

func (s *Server) retentionPreview(w http.ResponseWriter, r *http.Request, actor *Claims) {
	if !actor.GlobalAdmin() {
		fail(w, http.StatusForbidden, "forbidden", "Administrator permission is required.")
		return
	}
	cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -s.cfg.Retention.Days)
	roster, _, err := s.loadRoster(r.Context())
	if err != nil {
		s.unavailable(w, "load retention roster", err)
		return
	}
	type candidate struct {
		CenterID string    `json:"center_id"`
		CameraID string    `json:"camera_id"`
		Start    time.Time `json:"start"`
		Bytes    int64     `json:"bytes"`
	}
	sample := []candidate{}
	for _, cam := range roster {
		if len(sample) >= 100 {
			break
		}
		base := path.Join(s.ingest.Storage.KeyPrefix, cam.CenterID, cam.CameraID) + "/"
		page, err := s.store.List(r.Context(), base, "", 100-len(sample))
		if err != nil {
			s.unavailable(w, "preview retention", err)
			return
		}
		for _, entry := range page.Entries {
			uri := strings.TrimPrefix(entry.Key, base)
			_, start, err := hls.SegmentIdentity(uri)
			if err != nil || !start.UTC().Truncate(24*time.Hour).Add(24*time.Hour+recording.MaxSegmentDuration).Before(cutoff) {
				continue
			}
			sample = append(sample, candidate{cam.CenterID, cam.CameraID, start, entry.Size})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.cfg.Retention.Enabled, "days": s.cfg.Retention.Days,
		"cutoff": cutoff, "candidates_sample": sample, "sample_limit": 100,
		"note": "Dry run only. Deletion additionally validates object metadata. Policy changes require deployment configuration."})
}
