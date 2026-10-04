package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
)

type exportRequest struct {
	CenterID string    `json:"center_id"`
	CameraID string    `json:"camera_id"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
}

type clip struct {
	Directory string
	Filename  string
	Start     time.Time
	End       time.Time
	Size      int64
}

type exportJob struct {
	ID      string
	Actor   string
	Request exportRequest
	State   string
	Error   string
	Expires time.Time
	Clip    clip
	Cancel  context.CancelFunc
}

// export starts a bounded background job. The browser later downloads a
// regular GET response, so a multi-gigabyte MP4 never becomes a JS Blob.
func (s *Server) export(w http.ResponseWriter, r *http.Request, actor *Claims) {
	var req exportRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !config.ValidID(req.CenterID) || !config.ValidID(req.CameraID) ||
		!validRange(req.Start, req.End, s.cfg.Export.MaxDuration.Duration) {
		fail(w, http.StatusBadRequest, "invalid_export_range", "Select a recording range within the export duration limit.")
		return
	}
	if !actor.Allows(req.CenterID, req.CameraID, "export") {
		fail(w, http.StatusForbidden, "forbidden", "Export permission is required.")
		return
	}
	if !take(s.exportSlots) {
		w.Header().Set("Retry-After", "10")
		fail(w, http.StatusTooManyRequests, "export_capacity", "Other exports are running. Try again shortly.")
		return
	}
	_, claims, err := s.auth.Media(actor, req.CenterID, req.CameraID, "export", req.Start, req.End)
	if err != nil {
		release(s.exportSlots)
		s.unavailable(w, "create export job", err)
		return
	}
	planCtx, planCancel := context.WithTimeout(r.Context(), 20*time.Second)
	plan, err := s.planExport(planCtx, req)
	planCancel()
	if err != nil {
		release(s.exportSlots)
		switch {
		case errors.Is(err, recording.ErrTooMany) || errors.Is(err, errClipSize):
			fail(w, http.StatusUnprocessableEntity, "export_too_large", "This range exceeds the export size limit. Select a shorter range.")
		case errors.Is(err, errNoRecording):
			fail(w, http.StatusNotFound, "recording_not_found", "No recording is indexed for this time range.")
		case errors.Is(err, errFormatChanged):
			fail(w, http.StatusConflict, "recording_format_changed", "The recording format changes within this range. Select a range with one format.")
		default:
			s.unavailable(w, "plan export", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(s.background, s.cfg.Export.Timeout.Duration)
	job := &exportJob{ID: claims.ID, Actor: actor.Subject, Request: req, State: "processing",
		Expires: time.Now().Add(s.cfg.Export.Timeout.Duration + time.Hour), Cancel: cancel}
	s.jobsMu.Lock()
	// Finished clips also consume disk. Requiring deletion or expiry bounds
	// retained artifacts as well as concurrent encoders.
	if len(s.jobs) >= 20 || s.background.Err() != nil {
		s.jobsMu.Unlock()
		cancel()
		release(s.exportSlots)
		fail(w, http.StatusTooManyRequests, "export_capacity", "Export storage capacity reached. Remove old exports or try later.")
		return
	}
	if err := s.reserveExportSpace(plan.reserve); err != nil {
		s.jobsMu.Unlock()
		cancel()
		release(s.exportSlots)
		if errors.Is(err, errExportSpace) {
			w.Header().Set("Retry-After", "60")
			fail(w, http.StatusInsufficientStorage, "export_storage_full", "Not enough disk space for this export. Wait for other exports to finish or remove old exports.")
		} else {
			s.unavailable(w, "check export disk space", err)
		}
		return
	}
	s.jobs[job.ID] = job
	s.jobsWG.Add(1)
	s.jobsMu.Unlock()
	go func() {
		defer s.jobsWG.Done()
		defer cancel()
		defer release(s.exportSlots)
		result, err := s.buildClip(ctx, req, plan)
		s.jobsMu.Lock()
		// The finished clip is now visible to Statfs; scratch input is gone.
		s.exportReserved -= plan.reserve
		if job.State == "cancelled" {
			s.jobsMu.Unlock()
			if result.Directory != "" {
				_ = os.RemoveAll(result.Directory)
			}
			return
		}
		if err != nil {
			job.State = "failed"
			job.Error = "export_failed"
			if errors.Is(err, errClipSize) {
				job.Error = "export_too_large"
			} else if ctx.Err() != nil {
				job.Error = "export_timeout"
			}
			s.jobsMu.Unlock()
			s.log.Warn("recording export failed", "actor", actor.Subject, "job_id", job.ID, "error", err)
			return
		}
		job.Clip, job.State, job.Expires = result, "ready", time.Now().Add(time.Hour)
		s.jobsMu.Unlock()
		s.log.Info("recording export ready", "actor", actor.Subject, "job_id", job.ID,
			"center_id", req.CenterID, "camera_id", req.CameraID, "bytes", result.Size)
	}()
	w.Header().Set("Location", "/v1/exports/"+job.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": job.ID, "state": "processing"})
}

func (s *Server) exportStatus(w http.ResponseWriter, r *http.Request, actor *Claims) {
	s.jobsMu.Lock()
	job := s.jobs[r.PathValue("id")]
	if job == nil || job.Actor != actor.Subject ||
		!actor.Allows(job.Request.CenterID, job.Request.CameraID, "export") ||
		!job.Expires.After(time.Now()) {
		s.jobsMu.Unlock()
		fail(w, http.StatusNotFound, "export_not_found", "Export not found or expired.")
		return
	}
	copy := *job
	s.jobsMu.Unlock()
	result := map[string]any{"id": copy.ID, "state": copy.State, "expires_at": copy.Expires}
	if copy.Error != "" {
		result["error"] = copy.Error
	}
	if copy.State == "ready" {
		_, cap, err := s.auth.Media(actor, copy.Request.CenterID, copy.Request.CameraID, "export", copy.Request.Start, copy.Request.End)
		if err != nil {
			s.unavailable(w, "issue export download", err)
			return
		}
		cap.ID = copy.ID
		if cap.ExpiresAt.Time.After(copy.Expires) {
			cap.ExpiresAt.Time = copy.Expires
		}
		token, err := s.auth.sign(cap)
		if err != nil {
			s.unavailable(w, "sign export download", err)
			return
		}
		result["url"] = strings.TrimSuffix(s.cfg.PublicURL, "/") + "/exports/" + token + "/clip.mp4"
		result["actual_start"], result["actual_end"], result["bytes"] = copy.Clip.Start, copy.Clip.End, copy.Clip.Size
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listExports(w http.ResponseWriter, r *http.Request, actor *Claims) {
	type entry struct {
		ID      string    `json:"id"`
		Center  string    `json:"center_id"`
		Camera  string    `json:"camera_id"`
		State   string    `json:"state"`
		Expires time.Time `json:"expires_at"`
	}
	out := []entry{}
	s.jobsMu.Lock()
	for _, job := range s.jobs {
		if job.Actor == actor.Subject && job.Expires.After(time.Now()) &&
			actor.Allows(job.Request.CenterID, job.Request.CameraID, "export") {
			out = append(out, entry{job.ID, job.Request.CenterID, job.Request.CameraID, job.State, job.Expires})
		}
	}
	s.jobsMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Expires.After(out[j].Expires) })
	writeJSON(w, http.StatusOK, map[string]any{"exports": out})
}

func (s *Server) cancelExport(w http.ResponseWriter, r *http.Request, actor *Claims) {
	s.jobsMu.Lock()
	job := s.jobs[r.PathValue("id")]
	if job == nil || job.Actor != actor.Subject {
		s.jobsMu.Unlock()
		fail(w, http.StatusNotFound, "export_not_found", "Export not found.")
		return
	}
	job.State = "cancelled"
	job.Cancel()
	directory := job.Clip.Directory
	delete(s.jobs, job.ID)
	s.jobsMu.Unlock()
	if directory != "" {
		_ = os.RemoveAll(directory)
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "cancelled"})
}

func (s *Server) downloadExport(w http.ResponseWriter, r *http.Request) {
	cap, err := s.auth.Parse(r.PathValue("token"), true)
	if err != nil || cap.Mode != "export" {
		fail(w, http.StatusUnauthorized, "invalid_session", "Download session has expired.")
		return
	}
	s.jobsMu.Lock()
	job := s.jobs[cap.ID]
	if job == nil || job.State != "ready" || !job.Expires.After(time.Now()) ||
		job.Actor != cap.Subject || job.Request.CenterID != cap.Center || job.Request.CameraID != cap.Camera {
		s.jobsMu.Unlock()
		fail(w, http.StatusNotFound, "export_not_found", "Export not found or expired.")
		return
	}
	artifact := job.Clip
	// Open while holding the job lock, so expiry/cancellation cannot unlink
	// it between authorization and open. POSIX keeps an open download valid.
	file, err := os.Open(filepath.Join(artifact.Directory, "clip.mp4"))
	s.jobsMu.Unlock()
	if err != nil {
		s.unavailable(w, "open export", err)
		return
	}
	defer file.Close()
	if !take(s.mediaSlots) {
		fail(w, http.StatusServiceUnavailable, "download_capacity", "Download capacity reached. Try again shortly.")
		return
	}
	defer release(s.mediaSlots)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.Filename}))
	w.Header().Set("X-Recording-Start", artifact.Start.UTC().Format(time.RFC3339Nano))
	w.Header().Set("X-Recording-End", artifact.End.UTC().Format(time.RFC3339Nano))
	w.Header().Set("ETag", `"`+cap.ID+`"`)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Hour))
	http.ServeContent(w, r, artifact.Filename, time.Time{}, file)
}

func (s *Server) pruneExports(now time.Time, all bool) {
	s.jobsMu.Lock()
	var directories []string
	for id, job := range s.jobs {
		if all || !job.Expires.After(now) {
			job.Cancel()
			job.State = "cancelled"
			directories = append(directories, job.Clip.Directory)
			delete(s.jobs, id)
		}
	}
	s.jobsMu.Unlock()
	for _, directory := range directories {
		if directory != "" {
			_ = os.RemoveAll(directory)
		}
	}
}

var (
	errClipSize      = errors.New("export exceeds size limit")
	errNoRecording   = errors.New("no recording indexed for this range")
	errFormatChanged = errors.New("recording format changed within the range")
	errExportSpace   = errors.New("insufficient export disk space")
)

type exportPlan struct {
	segments []hls.PublishedSegment
	// reserve is the peak scratch space: the copied inputs and the remuxed
	// output together, with headroom for the +faststart pass.
	reserve int64
}

func (s *Server) planExport(ctx context.Context, req exportRequest) (exportPlan, error) {
	records, err := s.index.Query(ctx, req.CenterID, req.CameraID, req.Start, req.End, maxPlaybackSegments)
	if err != nil {
		return exportPlan{}, err
	}
	if len(records) == 0 {
		return exportPlan{}, errNoRecording
	}
	segments, err := recording.Playlist(records)
	if err != nil {
		return exportPlan{}, fmt.Errorf("%w: %s", errFormatChanged, err.Error())
	}
	var total int64
	for _, segment := range segments {
		if segment.Bytes <= 0 || segment.Bytes > s.cfg.Export.MaxBytes-total {
			return exportPlan{}, errClipSize
		}
		total += segment.Bytes
	}
	return exportPlan{segments: segments, reserve: 3 * total}, nil
}

// reserveExportSpace admits a job only if the volume can hold it together
// with every running job's reservation and the configured free-space floor.
// Statfs alone is not enough: concurrent jobs see the same free space before
// any of them has written. The caller holds jobsMu.
func (s *Server) reserveExportSpace(reserve int64) error {
	if err := os.MkdirAll(s.cfg.Export.TempDir, 0o750); err != nil {
		return err
	}
	available, err := diskAvailable(s.cfg.Export.TempDir)
	if err != nil {
		return err
	}
	if available < uint64(s.exportReserved)+uint64(reserve)+uint64(s.cfg.Export.MinFreeBytes) {
		return errExportSpace
	}
	s.exportReserved += reserve
	return nil
}

// diskAvailable is a variable so tests can model a nearly full volume.
var diskAvailable = func(dir string) (uint64, error) {
	var disk syscall.Statfs_t
	if err := syscall.Statfs(dir, &disk); err != nil {
		return 0, err
	}
	return uint64(disk.Bavail) * uint64(disk.Bsize), nil
}

func (s *Server) buildClip(ctx context.Context, req exportRequest, plan exportPlan) (clip, error) {
	segments := plan.segments
	dir, err := os.MkdirTemp(s.cfg.Export.TempDir, "clip-"+s.index.Identity()+"-")
	if err != nil {
		return clip{}, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	local := make([]hls.PublishedSegment, 0, len(segments))
	inits := map[string]string{}
	remaining := s.cfg.Export.MaxBytes
	for n, segment := range segments {
		name := fmt.Sprintf("segment-%06d%s", n, path.Ext(segment.URI))
		key := path.Join(s.ingest.Storage.KeyPrefix, req.CenterID, req.CameraID, segment.URI)
		size, err := s.download(ctx, key, filepath.Join(dir, name), remaining)
		if err != nil {
			return clip{}, err
		}
		remaining -= size
		if segment.InitURI != "" {
			initName, exists := inits[segment.InitURI]
			if !exists {
				initName = fmt.Sprintf("init-%06d.mp4", len(inits))
				key := path.Join(s.ingest.Storage.KeyPrefix, req.CenterID, req.CameraID, segment.InitURI)
				size, err := s.download(ctx, key, filepath.Join(dir, initName), min(remaining, 1<<20))
				if err != nil {
					return clip{}, err
				}
				remaining -= size
				inits[segment.InitURI] = initName
			}
			segment.InitURI = initName
		}
		segment.URI = name
		local = append(local, segment)
	}
	playlist := filepath.Join(dir, "clip.m3u8")
	if err := os.WriteFile(playlist, hls.RenderRecording(local), 0o600); err != nil {
		return clip{}, err
	}
	output := filepath.Join(dir, "clip.mp4")
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "file", "-allowed_extensions", "ALL",
		"-i", playlist, "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy",
		"-avoid_negative_ts", "make_zero", "-movflags", "+faststart", "-y", output}
	cmd := exec.CommandContext(ctx, s.ingest.FFmpeg.Binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	stderr := &boundedLog{remaining: 4096}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return clip{}, fmt.Errorf("remux recording: %w: %s", err, stderr.text.String())
	}
	info, err := os.Stat(output)
	if err != nil {
		return clip{}, err
	}
	if info.Size() <= 0 || info.Size() > s.cfg.Export.MaxBytes+s.cfg.Export.MaxBytes/10 {
		return clip{}, errClipSize
	}
	// Only retain the downloadable result, not its copied source segments.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return clip{}, err
	}
	for _, entry := range entries {
		if entry.Name() != "clip.mp4" {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return clip{}, err
			}
		}
	}
	first, last := segments[0], segments[len(segments)-1]
	complete = true
	return clip{Directory: dir, Size: info.Size(), Start: first.ProgramDateTime,
		End: last.ProgramDateTime.Add(last.Duration),
		Filename: fmt.Sprintf("%s-%s-%s.mp4", req.CenterID, req.CameraID,
			first.ProgramDateTime.UTC().Format("20060102T150405Z"))}, nil
}

func (s *Server) cleanupOrphanedExports() error {
	entries, err := os.ReadDir(s.cfg.Export.TempDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// The index's exclusive process lock is already held. Only directories
	// belonging to this durable index identity can be left by a dead job;
	// another playback instance's temporary files have a different prefix.
	prefix := "clip-" + s.index.Identity() + "-"
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			if err := os.RemoveAll(filepath.Join(s.cfg.Export.TempDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) download(ctx context.Context, key, filename string, limit int64) (int64, error) {
	stream, err := s.store.Open(ctx, key, "")
	if err != nil {
		return 0, err
	}
	defer stream.Body.Close()
	if limit <= 0 || stream.Size <= 0 || stream.Size > limit {
		return 0, errClipSize
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var written int64
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := stream.Body.Read(buffer)
		if int64(n) > limit-written {
			return written, errClipSize
		}
		if n > 0 {
			count, err := file.Write(buffer[:n])
			written += int64(count)
			if err != nil {
				return written, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return written, readErr
		}
	}
	if written != stream.Size {
		return written, fmt.Errorf("media object was truncated")
	}
	return written, file.Close()
}

type boundedLog struct {
	text      strings.Builder
	remaining int
}

func (b *boundedLog) Write(p []byte) (int, error) {
	n := min(len(p), b.remaining)
	b.text.Write(p[:n])
	b.remaining -= n
	return len(p), nil
}
