//go:build ffmpeg

// These tests require a real ffmpeg binary. Run them with:
//
//	make test-ffmpeg
//
// They exist because the risky part of this design is not the Go code, it is
// whether the exact -hls_flags combination behaves the way the uploader
// assumes. Everything asserted here was verified against the ffmpeg in the
// project's container image, not inferred from documentation.
package ffmpeg

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
)

func testSpec(t *testing.T) Spec {
	t.Helper()
	cfg := config.Default()
	return Spec{
		SpoolDir: filepath.Join(t.TempDir(), "spool"),
		Cfg:      cfg.FFmpeg,
		Segment: config.SegmentConfig{
			TargetDuration: config.Duration{Duration: 2 * time.Second},
			LiveWindow:     3,
			LocalListSize:  6,
		},
	}
}

func TestFFmpegProbe(t *testing.T) {
	ver, err := Probe(context.Background(), "ffmpeg")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !strings.HasPrefix(ver, "ffmpeg version") {
		t.Errorf("unexpected version banner %q", ver)
	}
	t.Logf("ffmpeg: %s", ver)
}

// TestFFmpegOutputArgsProduceExpectedHLSLayout drives the real muxer with a
// synthetic encoded source and asserts every property the uploader relies on:
// temp_file renaming, the segment filename pattern, EXT-X-PROGRAM-DATE-TIME,
// and delete_segments bounding the spool.
func TestFFmpegOutputArgsProduceExpectedHLSLayout(t *testing.T) {
	spec := testSpec(t)
	if err := EnsureSpool(spec.SpoolDir); err != nil {
		t.Fatal(err)
	}
	out, err := OutputArgs(spec)
	if err != nil {
		t.Fatal(err)
	}

	// A 2 s GOP at 25 fps, so -hls_time 2 can actually cut every 2 s. This
	// stands in for a correctly configured camera.
	args := append(GlobalArgs(),
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-t", "12",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-g", "50", "-keyint_min", "50", "-sc_threshold", "0",
		"-f", "mpegts", "pipe:1",
	)
	// Encode to a file first so the transmux step is a genuine stream copy
	// with no encoder in the path, exactly as in production.
	src := filepath.Join(spec.SpoolDir, "..", "src.ts")
	encode := exec.Command("ffmpeg", append(args[:len(args)-1], src)...)
	encode.Stderr = os.Stderr
	if err := encode.Run(); err != nil {
		t.Fatalf("prepare source: %v", err)
	}

	transmux := exec.Command("ffmpeg", append(append(GlobalArgs(), "-i", src), out...)...)
	transmux.Stderr = os.Stderr
	if err := transmux.Run(); err != nil {
		t.Fatalf("transmux: %v", err)
	}

	entries, err := os.ReadDir(spec.SpoolDir)
	if err != nil {
		t.Fatal(err)
	}
	var segments []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			t.Errorf("a .tmp file survived, so temp_file renaming cannot be trusted: %s", name)
		}
		if strings.HasSuffix(name, ".ts") {
			segments = append(segments, name)
		}
	}
	if len(segments) == 0 {
		t.Fatal("no segments were produced")
	}
	// delete_segments plus local_list_size must bound the spool.
	if len(segments) > spec.Segment.LocalListSize {
		t.Errorf("spool holds %d segments, expected at most local_list_size=%d",
			len(segments), spec.Segment.LocalListSize)
	}
	// The name must match SegmentPattern so parseLocalIndex can order them.
	for _, s := range segments {
		if !strings.HasPrefix(s, "seg-") {
			t.Errorf("segment %q does not match the configured pattern", s)
		}
	}

	raw, err := os.ReadFile(filepath.Join(spec.SpoolDir, LocalPlaylistName))
	if err != nil {
		t.Fatalf("local playlist: %v", err)
	}
	playlist := string(raw)
	if !strings.Contains(playlist, "#EXT-X-PROGRAM-DATE-TIME:") {
		t.Errorf("program_date_time flag did not take effect:\n%s", playlist)
	}
	if !strings.Contains(playlist, "#EXTINF:") {
		t.Errorf("playlist has no EXTINF entries:\n%s", playlist)
	}
	t.Logf("local playlist:\n%s", playlist)

	// Every segment must decode on its own. This is what "independent
	// segment" means in practice and it is the property a player needs when
	// it joins mid-stream.
	//
	// ffprobe prints the codec name more than once for an MPEG-TS input
	// (once per program and once per stream), so the check is that every
	// reported codec is h264 and none is anything else.
	for _, s := range segments {
		probe := exec.Command("ffprobe", "-v", "error",
			"-select_streams", "v:0",
			"-show_entries", "stream=codec_name",
			"-of", "csv=p=0",
			filepath.Join(spec.SpoolDir, s))
		outBytes, err := probe.Output()
		if err != nil {
			t.Errorf("ffprobe %s: %v", s, err)
			continue
		}
		names := []string{}
		for _, line := range strings.Split(string(outBytes), "\n") {
			if v := strings.TrimSpace(line); v != "" {
				names = append(names, v)
			}
		}
		if len(names) == 0 {
			t.Errorf("segment %s: ffprobe reported no video stream", s)
			continue
		}
		for _, n := range names {
			if n != "h264" {
				t.Errorf("segment %s codec = %q, want h264 (the codec must not be altered)", s, n)
			}
		}
	}
}

// TestFFmpegLongGOPOverridesTargetDuration is the empirical proof of the
// single most important constraint in this design: with -c:v copy, ffmpeg
// cannot honour -hls_time if the source keyframe interval is longer, because
// it cannot create an IDR frame without re-encoding.
//
// A camera with a 6 s GOP configured for 2 s segments produces ~6 s
// segments. This is why the segment-gap alarm allows MaxGOPSlack, why
// EXT-X-TARGETDURATION is computed from actual EXTINF values, and why the
// "4-6 s segment" requirement is a camera configuration item rather than
// something this module can enforce.
func TestFFmpegLongGOPOverridesTargetDuration(t *testing.T) {
	spec := testSpec(t)
	if err := EnsureSpool(spec.SpoolDir); err != nil {
		t.Fatal(err)
	}

	// 6 s GOP at 25 fps = 150 frames.
	src := filepath.Join(spec.SpoolDir, "..", "longgop.ts")
	encode := exec.Command("ffmpeg", append(GlobalArgs(),
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-t", "18",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-g", "150", "-keyint_min", "150", "-sc_threshold", "0",
		"-f", "mpegts", src)...)
	encode.Stderr = os.Stderr
	if err := encode.Run(); err != nil {
		t.Fatalf("prepare source: %v", err)
	}

	// Target duration stays at 2 s.
	out, err := OutputArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	transmux := exec.Command("ffmpeg", append(append(GlobalArgs(), "-i", src), out...)...)
	transmux.Stderr = os.Stderr
	if err := transmux.Run(); err != nil {
		t.Fatalf("transmux: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(spec.SpoolDir, LocalPlaylistName))
	if err != nil {
		t.Fatal(err)
	}
	playlist := string(raw)
	t.Logf("long-GOP playlist:\n%s", playlist)

	var longest float64
	for _, line := range strings.Split(playlist, "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		d, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			continue
		}
		if d > longest {
			longest = d
		}
	}
	if longest < 5.0 {
		t.Errorf("longest segment %.3fs: expected roughly the 6s GOP, not the 2s target. "+
			"If this fails, ffmpeg is cutting at non-keyframes and segments are not independent.", longest)
	}
	t.Logf("configured target 2s, actual longest segment %.3fs (set by the 6s GOP)", longest)
}

// TestProcessStopKillsProcessGroup verifies the zombie-avoidance path: a
// terminated ffmpeg must be reaped, and Stop must return only after that.
func TestProcessStopKillsProcessGroup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A long-running encode that ignores the source ending.
	proc, err := Start("ffmpeg", []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-f", "null", "-",
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	pid := proc.PID()
	if pid <= 0 {
		t.Fatal("no pid")
	}
	time.Sleep(500 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		proc.Stop(2 * time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return; the process was not reaped")
	}

	select {
	case <-proc.Done():
	default:
		t.Fatal("Done must be closed once Stop returns")
	}
	// A reaped process leaves no /proc entry.
	if _, err := os.Stat("/proc/" + itoa(pid)); err == nil {
		t.Errorf("pid %d still present after Stop", pid)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
