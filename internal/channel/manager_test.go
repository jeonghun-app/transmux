package channel

import (
	"context"
	"io"
	"log/slog"
	"strconv"
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
