package playback

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	claims, err := s.auth.Parse(r.PathValue("token"), true)
	if err != nil {
		fail(w, http.StatusUnauthorized, "invalid_session", "Playback session has expired.")
		return
	}
	center, camera, resource := r.PathValue("center"), r.PathValue("camera"), r.PathValue("resource")
	if !config.ValidID(center) || !config.ValidID(camera) || claims.Center != center || claims.Camera != camera ||
		(claims.Mode != "live" && claims.Mode != "recording") {
		fail(w, http.StatusForbidden, "forbidden", "This playback session does not allow this resource.")
		return
	}
	if !take(s.mediaSlots) {
		w.Header().Set("Retry-After", "2")
		fail(w, http.StatusServiceUnavailable, "playback_capacity", "Playback capacity reached. Try again shortly.")
		return
	}
	defer release(s.mediaSlots)
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if resource == "index.m3u8" {
		var body []byte
		if claims.Mode == "live" {
			live, err := s.live(ctx, center, camera)
			if err != nil {
				s.mediaError(w, err)
				return
			}
			var window []hls.PublishedSegment
			for _, seg := range live.Window {
				if seg.ProgramDateTime.UnixMilli() >= claims.Start {
					window = append(window, seg)
				} else if seg.Discontinuity {
					live.DiscontinuitySequence++
				}
			}
			if len(window) == 0 {
				s.mediaError(w, errOffline)
				return
			}
			body = hls.RenderLive(hls.Live{Segments: window, DiscontinuitySequence: live.DiscontinuitySequence})
		} else {
			body, err = s.index.SnapshotManifest(claims.ID)
			if err != nil {
				s.mediaError(w, err)
				return
			}
		}
		w.Header().Set("Content-Type", hls.ContentTypeManifest)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}
	isInit := hls.ValidInitURI(resource)
	_, pdt, identityErr := hls.SegmentIdentity(resource)
	if identityErr != nil && !isInit {
		fail(w, http.StatusNotFound, "media_not_found", "Media not found.")
		return
	}
	if claims.Mode == "recording" {
		if !s.index.SnapshotAllows(claims.ID, resource) {
			fail(w, http.StatusForbidden, "outside_recording", "This object is outside the selected recording.")
			return
		}
	} else if !isInit && (pdt.UnixMilli() < claims.Start || pdt.After(time.Now().Add(time.Minute))) {
		fail(w, http.StatusForbidden, "outside_live_window", "This object is outside the live session.")
		return
	}
	key := path.Join(s.ingest.Storage.KeyPrefix, center, camera, resource)
	byteRange := r.Header.Get("Range")
	if r.Method == http.MethodHead {
		byteRange = "" // Range is defined for GET, not HEAD.
	}
	stream, err := s.store.Open(ctx, key, byteRange)
	if err != nil {
		s.mediaError(w, err)
		return
	}
	defer stream.Body.Close()
	contentType := hls.ContentTypeSegment
	if isInit || strings.HasSuffix(resource, ".m4s") {
		contentType = "video/mp4"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(stream.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	// Authorization must run on every request, including cached byte ranges.
	// A shared cache is never allowed to outlive or bypass the URL capability.
	w.Header().Set("Cache-Control", "private, no-store")
	if stream.ETag != "" {
		w.Header().Set("ETag", stream.ETag)
	}
	if stream.ContentRange != "" {
		w.Header().Set("Content-Range", stream.ContentRange)
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method != http.MethodHead {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
		_, _ = io.Copy(w, stream.Body)
	}
}

func (s *Server) mediaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		fail(w, http.StatusNotFound, "media_not_found", "Media not found or its retention period has ended.")
	case errors.Is(err, storage.ErrRange):
		fail(w, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "The byte range cannot be served.")
	case errors.Is(err, errOffline):
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "camera_offline", "No recent live video is available.")
	default:
		s.unavailable(w, "serve media", err)
	}
}
