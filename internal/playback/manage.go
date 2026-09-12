package playback

import (
	"errors"
	"net/http"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func (s *Server) adminCameras(w http.ResponseWriter, r *http.Request, actor *Claims) {
	canManage := false
	for _, grant := range actor.Grants {
		for _, permission := range grant.Permissions {
			if permission == "manage" {
				canManage = true
			}
		}
	}
	if !canManage {
		fail(w, http.StatusForbidden, "forbidden", "Camera management permission is required.")
		return
	}
	roster, etag, err := s.loadRoster(r.Context())
	if err != nil {
		s.unavailable(w, "load managed roster", err)
		return
	}
	out := []CameraView{}
	for _, cam := range roster {
		if actor.Allows(cam.CenterID, cam.CameraID, "manage") {
			out = append(out, s.cameraView(cam, actor, true))
		}
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cameras": out, "editable": s.managed != nil,
		"max_active_cameras": s.ingest.MaxChannels, "shards": s.cfg.Shards})
}

func (s *Server) editRoster(w http.ResponseWriter, r *http.Request) ([]config.StaticCamera, bool) {
	if s.managed == nil {
		fail(w, http.StatusConflict, "external_camera_provider", "This deployment manages cameras through its external catalog.")
		return nil, false
	}
	if r.Header.Get("If-Match") == "" {
		fail(w, http.StatusPreconditionRequired, "version_required", "Reload the camera list and send its ETag as If-Match.")
		return nil, false
	}
	roster, etag, err := s.loadRoster(r.Context())
	if err != nil {
		s.unavailable(w, "load roster for edit", err)
		return nil, false
	}
	if etag != r.Header.Get("If-Match") {
		s.invalidateRoster()
		fail(w, http.StatusPreconditionFailed, "camera_list_changed", "The camera list changed. Reload it before saving.")
		return nil, false
	}
	return roster, true
}

func (s *Server) saveRoster(w http.ResponseWriter, r *http.Request, actor *Claims,
	roster []config.StaticCamera, cam config.StaticCamera, status int, action string) {
	active := map[string]int{}
	limits := map[string]int{}
	for _, shard := range s.cfg.Shards {
		limits[shard.ID] = shard.MaxChannels
	}
	for _, c := range roster {
		shard := c.ShardID
		if shard == "" && len(s.cfg.Shards) == 1 {
			shard = s.cfg.Shards[0].ID
		}
		if c.Enabled == nil || *c.Enabled {
			if limits[shard] == 0 {
				fail(w, http.StatusBadRequest, "invalid_shard", "Assign the camera to a configured ingest shard.")
				return
			}
			active[shard]++
		}
	}
	for id, count := range active {
		if count > limits[id] {
			fail(w, http.StatusConflict, "shard_capacity", "This shard has reached its active camera limit.")
			return
		}
	}
	etag, err := s.managed.Save(r.Context(), roster, r.Header.Get("If-Match"))
	s.invalidateRoster()
	if errors.Is(err, storage.ErrPreconditionFailed) {
		fail(w, http.StatusPreconditionFailed, "camera_list_changed", "The camera list changed. Reload it before saving.")
		return
	}
	if err != nil {
		s.unavailable(w, "save camera roster", err)
		return
	}
	s.log.Info("camera configuration changed", "actor", actor.Subject, "action", action,
		"center_id", cam.CenterID, "camera_id", cam.CameraID)
	w.Header().Set("ETag", etag)
	writeJSON(w, status, map[string]any{"camera": s.cameraView(cam, actor, true),
		"apply_within_seconds": s.ingest.Cameras.PollInterval.Duration.Seconds()})
}

func (s *Server) createCamera(w http.ResponseWriter, r *http.Request, actor *Claims) {
	var cam config.StaticCamera
	if !readJSON(w, r, &cam) {
		return
	}
	if !actor.Allows(cam.CenterID, cam.CameraID, "manage") {
		fail(w, http.StatusForbidden, "forbidden", "Camera management permission is required.")
		return
	}
	if cam.ShardID == "" && len(s.cfg.Shards) == 1 {
		cam.ShardID = s.cfg.Shards[0].ID
	}
	if err := cam.Validate(); err != nil {
		fail(w, http.StatusBadRequest, "invalid_camera", err.Error())
		return
	}
	roster, ok := s.editRoster(w, r)
	if !ok {
		return
	}
	for _, existing := range roster {
		if existing.CenterID == cam.CenterID && existing.CameraID == cam.CameraID {
			fail(w, http.StatusConflict, "camera_exists", "This camera already exists. Edit it to re-enable or change it.")
			return
		}
	}
	roster = append(roster, cam)
	s.saveRoster(w, r, actor, roster, cam, http.StatusCreated, "create")
}

func (s *Server) updateCamera(w http.ResponseWriter, r *http.Request, actor *Claims) {
	s.changeCamera(w, r, actor, false)
}

func (s *Server) disableCamera(w http.ResponseWriter, r *http.Request, actor *Claims) {
	s.changeCamera(w, r, actor, true)
}

func (s *Server) changeCamera(w http.ResponseWriter, r *http.Request, actor *Claims, disable bool) {
	center, id := r.PathValue("center"), r.PathValue("camera")
	if !config.ValidID(center) || !config.ValidID(id) {
		fail(w, http.StatusBadRequest, "invalid_camera", "Invalid camera identifier.")
		return
	}
	if !actor.Allows(center, id, "manage") {
		fail(w, http.StatusForbidden, "forbidden", "Camera management permission is required.")
		return
	}
	var update config.StaticCamera
	if !disable && !readJSON(w, r, &update) {
		return
	}
	if update.CenterID != "" && update.CenterID != center || update.CameraID != "" && update.CameraID != id {
		fail(w, http.StatusBadRequest, "identity_mismatch", "Camera identifiers must match the URL.")
		return
	}
	roster, ok := s.editRoster(w, r)
	if !ok {
		return
	}
	for i, cam := range roster {
		if cam.CenterID != center || cam.CameraID != id {
			continue
		}
		action := "update"
		if disable {
			// Keep the catalog entry and recordings discoverable. Stopping
			// ingestion must not make a retained recording unreachable.
			enabled := false
			cam.Enabled = &enabled
			action = "disable"
		} else {
			update.CenterID, update.CameraID = center, id
			if update.RTSPURL == "" {
				update.RTSPURL = cam.RTSPURL
			}
			if update.Enabled == nil {
				update.Enabled = cam.Enabled
			}
			if update.ShardID == "" {
				update.ShardID = cam.ShardID
				if update.ShardID == "" && len(s.cfg.Shards) == 1 {
					update.ShardID = s.cfg.Shards[0].ID
				}
			}
			cam = update
		}
		if err := cam.Validate(); err != nil {
			fail(w, http.StatusBadRequest, "invalid_camera", err.Error())
			return
		}
		roster[i] = cam
		s.saveRoster(w, r, actor, roster, cam, http.StatusOK, action)
		return
	}
	fail(w, http.StatusNotFound, "camera_not_found", "Camera not found.")
}
