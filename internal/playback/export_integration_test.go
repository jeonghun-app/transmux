//go:build ffmpeg

package playback

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/ffmpeg"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func TestExportDecodesRealH264HEVCAndConvertedAudio(t *testing.T) {
	for _, tc := range []struct{ name, encoder, expectedVideo, sourceAudio, mode, format string }{
		{"h264-aac-ts", "libx264", "h264", "aac", "copy", "mpegts"},
		{"h264-g711-fmp4", "libx264", "h264", "pcm_mulaw", "aac", "fmp4"},
		{"hevc-g711-fmp4", "libx265", "hevc", "pcm_mulaw", "aac", "fmp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			source := filepath.Join(dir, "source.nut")
			args := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc2=size=160x96:rate=10",
				"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=8000",
				"-t", "8", "-c:v", tc.encoder, "-preset", "ultrafast", "-g", "20",
				"-c:a", tc.sourceAudio}
			if tc.encoder == "libx265" {
				args = append(args, "-x265-params", "pools=1:frame-threads=1:log-level=error")
			}
			args = append(args, "-f", "nut", source)
			if body, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("generate camera fixture: %v %s", err, body)
			}
			spool := filepath.Join(dir, "spool")
			if err := ffmpeg.EnsureSpool(spool); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Segment.TargetDuration = config.Duration{Duration: 2 * time.Second}
			cfg.Segment.LocalListSize = 10
			out, err := ffmpeg.OutputArgs(ffmpeg.Spec{SpoolDir: spool, Cfg: cfg.FFmpeg, Segment: cfg.Segment, Audio: tc.mode, Format: tc.format, VideoCodec: tc.expectedVideo})
			if err != nil {
				t.Fatal(err)
			}
			args = append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", source}, out...)
			if body, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("transmux camera fixture: %v %s", err, body)
			}
			raw, err := os.ReadFile(filepath.Join(spool, ffmpeg.LocalPlaylistName))
			if err != nil {
				t.Fatal(err)
			}
			local, err := hls.ParseLocal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(local) < 3 {
				t.Fatalf("too few segments: %s", raw)
			}
			var records []recording.Segment
			for n, seg := range local {
				initURI := ""
				if seg.InitName != "" {
					init, err := os.ReadFile(filepath.Join(spool, seg.InitName))
					if err != nil {
						t.Fatal(err)
					}
					initURI = path.Join(seg.ProgramDateTime.UTC().Format("2006/01/02"), fmt.Sprintf("init-%x.mp4", sha256.Sum256(init)))
					if _, err := f.store.Put(t.Context(), storage.Object{Key: "archive/c1/cam1/" + initURI, Body: init}); err != nil {
						t.Fatal(err)
					}
				}
				body, err := os.ReadFile(filepath.Join(spool, seg.Name))
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, f.segment(t, uint64(n+1), seg.ProgramDateTime, seg.Duration, body, initURI))
			}
			token := f.token(t, allPermissions())
			request := exportRequest{CenterID: "c1", CameraID: "cam1", Start: records[0].Start, End: records[len(records)-1].End()}
			resp, body := f.request(t, "POST", "/v1/exports", token, request, nil)
			if resp.StatusCode != 202 {
				t.Fatalf("start export: %d %s", resp.StatusCode, body)
			}
			var started struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(body, &started); err != nil {
				t.Fatal(err)
			}
			var job struct {
				State string `json:"state"`
				URL   string `json:"url"`
				Error string `json:"error"`
			}
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				resp, body = f.request(t, "GET", "/v1/exports/"+started.ID, token, nil, nil)
				if err := json.Unmarshal(body, &job); err != nil {
					t.Fatal(err)
				}
				if job.State == "ready" || job.State == "failed" {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if job.State != "ready" {
				t.Fatalf("export never became ready: %d %s", resp.StatusCode, body)
			}
			resp, body = f.request(t, "GET", job.URL, "", nil, nil)
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp4" || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
				t.Fatalf("download failed: %d %s", resp.StatusCode, body)
			}
			clip := filepath.Join(dir, "download.mp4")
			if err := os.WriteFile(clip, body, 0o600); err != nil {
				t.Fatal(err)
			}
			probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,codec_tag_string", "-of", "csv=p=0", clip).CombinedOutput()
			if err != nil || !strings.Contains(string(probe), tc.expectedVideo) || !strings.Contains(string(probe), "aac") {
				t.Fatalf("download lost video or audio: %s %v", probe, err)
			}
			if tc.expectedVideo == "hevc" && !strings.Contains(string(probe), "hvc1") {
				t.Fatalf("HEVC export is missing its hvc1 sample entry: %s", probe)
			}
			if decoded, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-i", clip, "-frames:v", "2", "-f", "null", "-").CombinedOutput(); err != nil {
				t.Fatalf("download is not decodable: %s %v", decoded, err)
			}
			if resp.Header.Get("X-Recording-Start") == "" || resp.Header.Get("X-Recording-End") == "" {
				t.Fatal("export did not report segment-aligned bounds")
			}
			t.Logf("%s exported %d segments, %d bytes; video=%s audio=aac decoded", tc.name, len(records), len(body), tc.expectedVideo)
			resp, _ = f.request(t, "DELETE", "/v1/exports/"+started.ID, token, nil, nil)
			if resp.StatusCode != 200 {
				t.Fatal("export removal failed")
			}
			resp, _ = f.request(t, "GET", job.URL, "", nil, nil)
			if resp.StatusCode != 404 {
				t.Fatal("deleted export still downloadable")
			}
		})
	}
}
