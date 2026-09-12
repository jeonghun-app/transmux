package playback

import (
	"net/http"
	"sort"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/config"
)

type CameraView struct {
	CenterID     string     `json:"center_id"`
	CameraID     string     `json:"camera_id"`
	Name         string     `json:"name"`
	Enabled      bool       `json:"enabled"`
	Audio        string     `json:"audio"`
	Format       string     `json:"format"`
	VideoCodec   string     `json:"video_codec"`
	Permissions  []string   `json:"permissions"`
	LastRecorded *time.Time `json:"last_recorded_at,omitempty"`
	Source       string     `json:"source,omitempty"`
	ShardID      string     `json:"shard_id"`
}

func (s *Server) cameraView(cam config.StaticCamera, actor *Claims, admin bool) CameraView {
	out := CameraView{CenterID: cam.CenterID, CameraID: cam.CameraID, Name: cam.Name,
		Enabled: cam.Enabled == nil || *cam.Enabled, Audio: cam.Audio, Format: cam.Format,
		VideoCodec:  cam.VideoCodec,
		Permissions: []string{}}
	out.ShardID = cam.ShardID
	if out.ShardID == "" && len(s.cfg.Shards) == 1 {
		out.ShardID = s.cfg.Shards[0].ID
	}
	if out.Name == "" {
		out.Name = cam.CameraID
	}
	if out.Audio == "" {
		out.Audio = "none"
	}
	if out.Format == "" {
		out.Format = "mpegts"
	}
	if out.VideoCodec == "" {
		out.VideoCodec = "auto"
	}
	for _, p := range []string{"live", "recording", "export", "manage"} {
		if actor.Allows(cam.CenterID, cam.CameraID, p) {
			out.Permissions = append(out.Permissions, p)
		}
	}
	if latest, err := s.index.Latest(cam.CenterID, cam.CameraID, time.Now()); err == nil {
		end := latest.End()
		out.LastRecorded = &end
	}
	if admin {
		out.Source = (camera.Camera{RTSPURL: cam.RTSPURL}).SafeURL()
	}
	return out
}

func visible(actor *Claims, cam config.StaticCamera) bool {
	for _, p := range []string{"live", "recording", "export", "manage"} {
		if actor.Allows(cam.CenterID, cam.CameraID, p) {
			return true
		}
	}
	return false
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request, actor *Claims) {
	center := r.PathValue("center")
	if center != "" && !config.ValidID(center) {
		fail(w, http.StatusBadRequest, "invalid_center", "Invalid center identifier.")
		return
	}
	roster, _, err := s.loadRoster(r.Context())
	if err != nil {
		s.unavailable(w, "load camera catalog", err)
		return
	}
	cameras := []CameraView{}
	for _, cam := range roster {
		if (center == "" || cam.CenterID == center) && visible(actor, cam) {
			cameras = append(cameras, s.cameraView(cam, actor, false))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cameras": cameras})
}

func (s *Server) centers(w http.ResponseWriter, r *http.Request, actor *Claims) {
	roster, _, err := s.loadRoster(r.Context())
	if err != nil {
		s.unavailable(w, "load center catalog", err)
		return
	}
	counts := map[string]int{}
	for _, cam := range roster {
		if visible(actor, cam) {
			counts[cam.CenterID]++
		}
	}
	type centerView struct {
		ID    string `json:"center_id"`
		Count int    `json:"camera_count"`
	}
	centers := []centerView{}
	for id, count := range counts {
		centers = append(centers, centerView{id, count})
	}
	sort.Slice(centers, func(i, j int) bool { return centers[i].ID < centers[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"centers": centers})
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request, actor *Claims) {
	if !actor.GlobalAdmin() {
		fail(w, http.StatusForbidden, "forbidden", "Administrator permission is required.")
		return
	}
	s.retentionMu.Lock()
	retention := s.retention
	s.retentionMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"index": s.scanner.Status(), "retention": retention,
		"active_media_requests": len(s.mediaSlots), "active_exports": len(s.exportSlots),
		"camera_management": s.managed != nil,
	})
}
