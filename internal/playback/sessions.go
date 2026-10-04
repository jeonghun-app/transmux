package playback

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

const (
	maxLiveAge          = 5 * time.Minute
	maxPlaybackRange    = 6 * time.Hour
	maxPlaybackSegments = 12000
)

type sessionRequest struct {
	CenterID  string    `json:"center_id"`
	CameraIDs []string  `json:"camera_ids"`
	Mode      string    `json:"mode"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

type sessionView struct {
	CameraID  string     `json:"camera_id"`
	URL       string     `json:"url"`
	ExpiresAt time.Time  `json:"expires_at"`
	Start     *time.Time `json:"actual_start,omitempty"`
	End       *time.Time `json:"actual_end,omitempty"`
	Segments  int        `json:"segments,omitempty"`
}

func validRange(start, end time.Time, maximum time.Duration) bool {
	return !start.IsZero() && start.UnixMilli() >= 0 && start.Before(end) &&
		end.Sub(start) <= maximum && !end.After(time.Now().Add(time.Minute))
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request, actor *Claims) {
	var req sessionRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !config.ValidID(req.CenterID) || len(req.CameraIDs) == 0 || len(req.CameraIDs) > 16 ||
		(req.Mode != "live" && req.Mode != "recording") {
		fail(w, http.StatusBadRequest, "invalid_session", "Select 1 to 16 cameras and mode live or recording.")
		return
	}
	if req.Mode == "recording" && !validRange(req.Start, req.End, maxPlaybackRange) {
		fail(w, http.StatusBadRequest, "invalid_range", "Select a recording range of up to six hours.")
		return
	}
	seen := map[string]bool{}
	for _, id := range req.CameraIDs {
		if !config.ValidID(id) || seen[id] {
			fail(w, http.StatusBadRequest, "invalid_camera", "Camera identifiers must be valid and unique.")
			return
		}
		seen[id] = true
		if !actor.Allows(req.CenterID, id, req.Mode) {
			fail(w, http.StatusForbidden, "forbidden", "You do not have permission to play these cameras.")
			return
		}
	}
	if !take(s.querySlots) {
		fail(w, http.StatusTooManyRequests, "query_capacity", "Too many requests. Try again shortly.")
		return
	}
	defer release(s.querySlots)
	roster, _, err := s.loadRoster(r.Context())
	if err != nil {
		s.unavailable(w, "load playback roster", err)
		return
	}
	for _, id := range req.CameraIDs {
		found := false
		for _, cam := range roster {
			if cam.CenterID == req.CenterID && cam.CameraID == id {
				if req.Mode == "live" && cam.Enabled != nil && !*cam.Enabled {
					fail(w, http.StatusConflict, "camera_disabled", "This camera is disabled.")
					return
				}
				found = true
				break
			}
		}
		if !found {
			fail(w, http.StatusNotFound, "camera_not_found", "Camera not found.")
			return
		}
	}
	// Validate every camera before creating any durable recording snapshot.
	type prepared struct {
		id       string
		start    time.Time
		segments []hls.PublishedSegment
	}
	ready := make([]prepared, 0, len(req.CameraIDs))
	for _, id := range req.CameraIDs {
		entry := prepared{id: id, start: time.Now().Add(-maxLiveAge)}
		if req.Mode == "live" {
			if _, err := s.live(r.Context(), req.CenterID, id); err != nil {
				if errors.Is(err, storage.ErrNotFound) || errors.Is(err, errOffline) {
					fail(w, http.StatusConflict, "camera_offline", "This camera has no recent live video.")
					return
				}
				s.unavailable(w, "load live playlist", err)
				return
			}
		} else {
			records, err := s.index.Query(r.Context(), req.CenterID, id, req.Start, req.End, maxPlaybackSegments)
			if err != nil {
				s.queryError(w, err)
				return
			}
			if len(records) == 0 {
				fail(w, http.StatusNotFound, "recording_not_found", "No recording is indexed for this time range.")
				return
			}
			entry.segments, err = recording.Playlist(records)
			if err != nil {
				fail(w, http.StatusConflict, "recording_format_changed", err.Error())
				return
			}
			entry.start = req.Start
		}
		ready = append(ready, entry)
	}
	// Sign every session first and persist all recording snapshots in one
	// transaction, so a later failure cannot strand snapshots for streams the
	// caller never receives.
	streams := make([]sessionView, 0, len(ready))
	snapshots := make([]recording.SnapshotEntry, 0, len(ready))
	for _, entry := range ready {
		token, claims, err := s.auth.Media(actor, req.CenterID, entry.id, req.Mode, entry.start, req.End)
		if err != nil {
			s.unavailable(w, "issue playback session", err)
			return
		}
		view := sessionView{CameraID: entry.id, ExpiresAt: claims.ExpiresAt.Time,
			URL: strings.TrimSuffix(s.cfg.PublicURL, "/") + "/media/" + token + "/" +
				req.CenterID + "/" + entry.id + "/index.m3u8"}
		if req.Mode == "recording" {
			snapshots = append(snapshots, recording.SnapshotEntry{ID: claims.ID,
				Expires: claims.ExpiresAt.Time, Segments: entry.segments})
			start := entry.segments[0].ProgramDateTime
			last := entry.segments[len(entry.segments)-1]
			end := last.ProgramDateTime.Add(last.Duration)
			view.Start, view.End, view.Segments = &start, &end, len(entry.segments)
		}
		streams = append(streams, view)
	}
	if len(snapshots) > 0 {
		if err := s.index.Snapshots(snapshots, s.cfg.MaxSessions); err != nil {
			if errors.Is(err, recording.ErrSessionCapacity) {
				fail(w, http.StatusTooManyRequests, "session_capacity", "Recording session capacity reached. Try again later.")
			} else {
				s.unavailable(w, "store recording session", err)
			}
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"mode": req.Mode, "streams": streams})
}

var errOffline = errors.New("no recent live video")

func (s *Server) live(ctx context.Context, center, camera string) (hls.Published, error) {
	raw, _, err := s.store.Get(ctx, path.Join(s.ingest.Storage.KeyPrefix, center, camera, s.ingest.Storage.ManifestName))
	if err != nil {
		return hls.Published{}, err
	}
	live, err := hls.ParsePublished(raw)
	if err != nil {
		return hls.Published{}, err
	}
	if len(live.Window) == 0 {
		return hls.Published{}, errOffline
	}
	now := time.Now()
	last := live.Window[len(live.Window)-1]
	staleAfter := min(maxLiveAge, max(30*time.Second, s.ingest.FFmpeg.StallTimeout.Duration+s.ingest.Upload.PutTimeout.Duration))
	if last.ProgramDateTime.IsZero() || now.Sub(last.ProgramDateTime.Add(last.Duration)) > staleAfter {
		return hls.Published{}, errOffline
	}
	for _, segment := range live.Window {
		_, pdt, err := hls.SegmentIdentity(segment.URI)
		if err != nil || pdt.After(now.Add(time.Minute)) {
			return hls.Published{}, fmt.Errorf("invalid live segment identity")
		}
	}
	return live, nil
}

func (s *Server) queryError(w http.ResponseWriter, err error) {
	if errors.Is(err, recording.ErrTooMany) {
		fail(w, http.StatusUnprocessableEntity, "range_too_large", "This range contains too many segments. Select a shorter range.")
	} else {
		s.unavailable(w, "query recording index", err)
	}
}

func (s *Server) renewSession(w http.ResponseWriter, r *http.Request, actor *Claims) {
	var req struct {
		Token string `json:"session_token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	old, err := s.auth.Parse(req.Token, true)
	if err != nil || old.Subject != actor.Subject || (old.Mode != "live" && old.Mode != "recording") ||
		!actor.Allows(old.Center, old.Camera, old.Mode) {
		fail(w, http.StatusForbidden, "invalid_session", "This session cannot be renewed.")
		return
	}
	start, end := time.UnixMilli(old.Start), time.Time{}
	if old.Mode == "live" {
		start = time.Now().Add(-maxLiveAge)
	} else {
		end = time.UnixMilli(old.End)
	}
	_, claims, err := s.auth.Media(actor, old.Center, old.Camera, old.Mode, start, end)
	if err != nil {
		s.unavailable(w, "renew playback session", err)
		return
	}
	if old.Mode == "recording" {
		claims.ID = old.ID
		if err := s.index.ExtendSnapshot(old.ID, claims.ExpiresAt.Time); err != nil {
			fail(w, http.StatusNotFound, "recording_session_expired", "Start a new recording session.")
			return
		}
	}
	token, err := s.auth.sign(claims)
	if err != nil {
		s.unavailable(w, "sign renewed playback session", err)
		return
	}
	writeJSON(w, http.StatusOK, sessionView{CameraID: old.Camera, ExpiresAt: claims.ExpiresAt.Time,
		URL: strings.TrimSuffix(s.cfg.PublicURL, "/") + "/media/" + token + "/" +
			old.Center + "/" + old.Camera + "/index.m3u8"})
}

func (s *Server) recordings(w http.ResponseWriter, r *http.Request, actor *Claims) {
	query := r.URL.Query()
	center, cam := query.Get("center_id"), query.Get("camera_id")
	start, errStart := time.Parse(time.RFC3339Nano, query.Get("start"))
	end, errEnd := time.Parse(time.RFC3339Nano, query.Get("end"))
	if !config.ValidID(center) || !config.ValidID(cam) || errStart != nil || errEnd != nil ||
		!validRange(start, end, 24*time.Hour) {
		fail(w, http.StatusBadRequest, "invalid_range", "Provide a camera and an RFC3339 time range of up to 24 hours.")
		return
	}
	if !actor.Allows(center, cam, "recording") {
		fail(w, http.StatusForbidden, "forbidden", "Recording permission is required.")
		return
	}
	if !take(s.querySlots) {
		fail(w, http.StatusTooManyRequests, "query_capacity", "Too many requests. Try again shortly.")
		return
	}
	defer release(s.querySlots)
	records, err := s.index.Query(r.Context(), center, cam, start, end, 90000)
	if err != nil {
		s.queryError(w, err)
		return
	}
	type period struct {
		Start    time.Time `json:"start"`
		End      time.Time `json:"end"`
		Segments int       `json:"segments"`
		Bytes    int64     `json:"bytes"`
		Format   string    `json:"format"`
	}
	periods := []period{}
	for _, segment := range records {
		format := "mpegts"
		if segment.InitURI != "" {
			format = "fmp4"
		}
		last := len(periods) - 1
		if last >= 0 && periods[last].Format == format &&
			segment.Start.Sub(periods[last].End) <= 500*time.Millisecond {
			periods[last].End = maxTime(periods[last].End, segment.End())
			periods[last].Segments++
			periods[last].Bytes += segment.Size
		} else {
			periods = append(periods, period{segment.Start, segment.End(), 1, segment.Size, format})
		}
	}
	complete := true
	var indexedAt time.Time
	for day := start.UTC().Truncate(24 * time.Hour); day.Before(end); day = day.AddDate(0, 0, 1) {
		state, err := s.index.ScanState(center, cam, day)
		if err != nil {
			s.unavailable(w, "read recording coverage", err)
			return
		}
		complete = complete && state.Complete
		if indexedAt.IsZero() || state.ScannedAt.Before(indexedAt) {
			indexedAt = state.ScannedAt
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"center_id": center, "camera_id": cam, "periods": periods,
		"segment_aligned": true, "index_complete": complete, "indexed_at": indexedAt,
	})
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
