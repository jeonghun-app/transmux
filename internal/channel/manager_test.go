package channel

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/metrics"
)

type emptyProvider struct{}

func (emptyProvider) Name() string { return "test" }
func (emptyProvider) Cameras(context.Context) ([]camera.Camera, error) {
	return []camera.Camera{}, nil
}

func TestRosterRemovalCancelsAllChannelsBeforeWaiting(t *testing.T) {
	cfg := testConfig(t)
	reg := metrics.NewRegistry()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(cfg, emptyProvider{}, &fakeStore{}, reg, log)
	allCanceled := make(chan struct{})
	allStopped := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(allStopped) })
	canceled := 0
	for i := 0; i < 3; i++ {
		cam := camera.Camera{CenterID: "c", CameraID: strconv.Itoa(i), RTSPURL: "rtsp://host/live"}
		w := NewWorker(cam, cfg, &fakeStore{}, reg, log, "session")
		m.workers[cam.Key()] = &entry{
			worker: w, done: allStopped,
			cancel: func() {
				canceled++
				if canceled == 3 {
					close(allCanceled)
				}
			},
		}
	}
	done := make(chan struct{})
	go func() { m.reconcile(context.Background()); close(done) }()
	select {
	case <-allCanceled:
	case <-time.After(time.Second):
		release.Do(func() { close(allStopped) })
		<-done
		t.Fatal("a draining channel delayed cancellation of the remaining roster")
	}
	release.Do(func() { close(allStopped) })
	<-done
}

type listProvider struct{ cams []camera.Camera }

func (listProvider) Name() string { return "test" }
func (p listProvider) Cameras(context.Context) ([]camera.Camera, error) {
	return append([]camera.Camera(nil), p.cams...), nil
}

// argvRecorder is a stand-in ffmpeg that writes the arguments it was started
// with to dir (one file per process, renamed into place so a reader never sees
// half of it) and then waits to be stopped, as a real ffmpeg would.
func argvRecorder(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + dir + "/argv.$$.tmp && mv " + dir + "/argv.$$.tmp " + dir + "/argv.$$\n" +
		"exec sleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// recordedArgv waits until want ffmpeg processes have recorded their argv
// and returns them, joined by newlines.
func recordedArgv(t *testing.T, dir string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		files, err := filepath.Glob(filepath.Join(dir, "argv.*[0-9]"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) >= want {
			out := make([]string, 0, len(files))
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, string(b))
			}
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d ffmpeg processes recorded their argv, want %d", len(files), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCredentialFreeURLsKeepPasswordsOffTheFFmpegCommandLine covers #43:
// ffmpeg's argv is readable by every user of the host, so with
// cameras.require_credential_free_urls on a camera whose URL carries a
// password must never start ffmpeg, while the rest of the roster still does.
// With the option off the URL reaches ffmpeg exactly as configured.
func TestCredentialFreeURLsKeepPasswordsOffTheFFmpegCommandLine(t *testing.T) {
	const secret = "argv-test-secret"
	clean := camera.Camera{CenterID: "c1", CameraID: "clean", RTSPURL: "rtsp://relay:8554/clean"}
	withPassword := camera.Camera{CenterID: "c1", CameraID: "cred", RTSPURL: "rtsp://operator:" + secret + "@relay:8554/cred"}
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(t)
			cfg.FFmpeg.Binary = argvRecorder(t, dir)
			cfg.Cameras.RequireCredentialFreeURLs = on
			reg := metrics.NewRegistry()
			var logs strings.Builder
			var logMu sync.Mutex
			log := slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &logMu}, nil))
			m := NewManager(cfg, listProvider{cams: []camera.Camera{withPassword, clean}}, &fakeStore{}, reg, log)
			ctx, cancel := context.WithCancel(context.Background())
			m.reconcile(ctx)
			defer func() { cancel(); m.shutdown() }()

			if !on {
				argv := recordedArgv(t, dir, 2)
				if !strings.Contains(strings.Join(argv, "\n"), "\n"+withPassword.RTSPURL+"\n") {
					t.Fatal("with the option off the configured URL did not reach ffmpeg unchanged")
				}
				if st := m.Status(); len(st.CredentialRejected) != 0 {
					t.Fatalf("option off but cameras reported as left out: %v", st.CredentialRejected)
				}
				return
			}
			argv := recordedArgv(t, dir, 1)
			if len(argv) != 1 || !strings.Contains(argv[0], clean.RTSPURL) {
				t.Fatalf("the credential-free camera did not start ffmpeg alone: %q", argv)
			}
			if _, running := m.Snapshot("c1", "cred"); running {
				t.Fatal("a camera whose URL carries a password was started")
			}
			if st := m.Status(); len(st.CredentialRejected) != 1 || st.CredentialRejected[0] != "c1/cred" {
				t.Fatalf("status does not name the camera left out: %v", st.CredentialRejected)
			}
			body := string(reg.Render())
			if !strings.Contains(body, "transmux_roster_credential_rejected{shard_id=\"shard-0\"} 1") {
				t.Fatalf("metric does not count the camera left out:\n%s", body)
			}
			logMu.Lock()
			text := logs.String()
			logMu.Unlock()
			if !strings.Contains(text, "c1/cred") || strings.Contains(text, secret) {
				t.Fatalf("log must name the camera without its password:\n%s", text)
			}
			// A second poll with the same roster does not log the same refusal again.
			before := strings.Count(text, "require_credential_free_urls")
			m.reconcile(ctx)
			logMu.Lock()
			after := strings.Count(logs.String(), "require_credential_free_urls")
			logMu.Unlock()
			if after != before {
				t.Fatalf("an unchanged refusal was logged again on the next poll (%d -> %d)", before, after)
			}
		})
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
