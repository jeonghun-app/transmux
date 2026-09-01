package channel

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/metrics"
)

// Manager reconciles the roster from the camera provider against the set of
// running workers.
//
// Exactly one worker exists per camera key. Two workers for one camera would
// both overwrite the same manifest object and produce a playlist that flips
// between two sequence timelines, so the invariant is enforced here rather
// than relied upon from the provider.
type Manager struct {
	cfg      config.Config
	provider camera.Provider
	uploader ObjectClient
	reg      *metrics.Registry
	log      *slog.Logger

	mu      sync.RWMutex
	workers map[string]*entry
	// ready flips true after the first successful roster load, which is what
	// the readiness probe reports.
	ready bool
	lastReconcile time.Time
	lastReconcileErr string

	mChannels   *metrics.Metric
	mReconcile  *metrics.Metric
	mReconcileErr *metrics.Metric
	mRejected   *metrics.Metric
}

type entry struct {
	worker *Worker
	cancel context.CancelFunc
	done   chan struct{}
}

func NewManager(cfg config.Config, provider camera.Provider, up ObjectClient, reg *metrics.Registry, log *slog.Logger) *Manager {
	shard := metrics.Label{Name: "shard_id", Value: cfg.ShardID}
	return &Manager{
		cfg:      cfg,
		provider: provider,
		uploader: up,
		reg:      reg,
		log:      log,
		workers:  make(map[string]*entry),
		mChannels: reg.Gauge("transmux_channels_running",
			"Channels currently supervised by this shard.", shard),
		mReconcile: reg.Counter("transmux_roster_reconcile_total",
			"Successful camera roster reconciliations.", shard),
		mReconcileErr: reg.Counter("transmux_roster_reconcile_failures_total",
			"Camera roster loads that failed.", shard),
		mRejected: reg.Counter("transmux_roster_rejected_total",
			"Cameras rejected because the shard channel cap was reached.", shard),
	}
}

// Run reconciles once immediately, then on every poll tick, until ctx is
// cancelled. On return every worker has stopped.
func (m *Manager) Run(ctx context.Context) {
	interval := m.cfg.Cameras.PollInterval.Duration
	if interval <= 0 {
		interval = 30 * time.Second
	}
	m.reconcile(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	loadCtx, cancel := context.WithTimeout(ctx, m.cfg.Cameras.RequestTimeout.Duration)
	desired, err := m.provider.Cameras(loadCtx)
	cancel()
	if err != nil {
		m.mReconcileErr.Inc()
		m.mu.Lock()
		m.lastReconcileErr = err.Error()
		m.mu.Unlock()
		// Deliberately non-fatal: a provider outage must not tear down
		// channels that are streaming fine. The previous roster stands.
		m.log.Error("camera roster load failed, keeping current channels", "error", err)
		return
	}

	// Apply the shard cap deterministically so a repeated partial roster
	// always selects the same subset instead of churning workers.
	sort.Slice(desired, func(i, j int) bool { return desired[i].Key() < desired[j].Key() })
	if len(desired) > m.cfg.MaxChannels {
		m.mRejected.Add(float64(len(desired) - m.cfg.MaxChannels))
		m.log.Error("roster exceeds shard capacity, ignoring the excess",
			"received", len(desired), "max_channels", m.cfg.MaxChannels)
		desired = desired[:m.cfg.MaxChannels]
	}

	want := make(map[string]camera.Camera, len(desired))
	for _, c := range desired {
		want[c.Key()] = c
	}

	m.mu.Lock()
	var toStop []*entry
	for key, e := range m.workers {
		newCam, keep := want[key]
		if !keep {
			toStop = append(toStop, e)
			delete(m.workers, key)
			continue
		}
		// A changed RTSP URL means the source moved; restart the pipeline.
		if newCam.RTSPURL != e.worker.Camera().RTSPURL {
			m.log.Info("camera source changed, restarting channel", "camera", key)
			toStop = append(toStop, e)
			delete(m.workers, key)
			continue
		}
	}
	var toStart []camera.Camera
	for key, c := range want {
		if _, running := m.workers[key]; !running {
			toStart = append(toStart, c)
		}
	}
	m.mu.Unlock()

	for _, e := range toStop {
		m.log.Info("stopping channel", "camera", e.worker.Camera().Key())
		e.cancel()
		<-e.done
		e.worker.Cleanup()
	}

	for _, c := range toStart {
		if ctx.Err() != nil {
			return
		}
		m.start(ctx, c)
	}

	m.mu.Lock()
	m.ready = true
	m.lastReconcile = time.Now().UTC()
	m.lastReconcileErr = ""
	n := len(m.workers)
	m.mu.Unlock()
	m.mChannels.Set(float64(n))
	m.mReconcile.Inc()
}

func (m *Manager) start(parent context.Context, c camera.Camera) {
	w := NewWorker(c, m.cfg, m.uploader, m.reg, m.log)
	ctx, cancel := context.WithCancel(parent)
	e := &entry{worker: w, cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	if _, exists := m.workers[c.Key()]; exists {
		m.mu.Unlock()
		cancel()
		return
	}
	m.workers[c.Key()] = e
	m.mu.Unlock()

	m.log.Info("starting channel", "camera", c.Key(), "source", c.SafeURL())
	go func() {
		defer close(e.done)
		w.Run(ctx)
	}()
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.workers))
	for _, e := range m.workers {
		entries = append(entries, e)
	}
	m.workers = make(map[string]*entry)
	m.mu.Unlock()

	m.log.Info("shutting down channels", "count", len(entries))
	// The parent context is already cancelled, so each Run is unwinding.
	// Wait for all of them so ffmpeg children are reaped before exit.
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			e.cancel()
			<-e.done
		}(e)
	}
	wg.Wait()
	m.mChannels.Set(0)
	m.log.Info("all channels stopped")
}

// Snapshots returns every channel view, sorted by camera key.
func (m *Manager) Snapshots() []Snapshot {
	m.mu.RLock()
	workers := make([]*Worker, 0, len(m.workers))
	for _, e := range m.workers {
		workers = append(workers, e.worker)
	}
	m.mu.RUnlock()

	out := make([]Snapshot, 0, len(workers))
	for _, w := range workers {
		out = append(out, w.Snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CenterID != out[j].CenterID {
			return out[i].CenterID < out[j].CenterID
		}
		return out[i].CameraID < out[j].CameraID
	})
	return out
}

// Snapshot returns one channel view.
func (m *Manager) Snapshot(centerID, cameraID string) (Snapshot, bool) {
	m.mu.RLock()
	e, ok := m.workers[centerID+"/"+cameraID]
	m.mu.RUnlock()
	if !ok {
		return Snapshot{}, false
	}
	return e.worker.Snapshot(), true
}

// Status describes the shard as a whole.
type Status struct {
	ShardID          string    `json:"shard_id"`
	Ready            bool      `json:"ready"`
	Provider         string    `json:"provider"`
	ChannelsTotal    int       `json:"channels_total"`
	ChannelsHealthy  int       `json:"channels_healthy"`
	ChannelsDegraded int       `json:"channels_degraded"`
	MaxChannels      int       `json:"max_channels"`
	LastReconcile    time.Time `json:"last_reconcile"`
	LastReconcileErr string    `json:"last_reconcile_error,omitempty"`
}

func (m *Manager) Status() Status {
	snaps := m.Snapshots()
	m.mu.RLock()
	st := Status{
		ShardID:          m.cfg.ShardID,
		Ready:            m.ready,
		Provider:         m.provider.Name(),
		MaxChannels:      m.cfg.MaxChannels,
		LastReconcile:    m.lastReconcile,
		LastReconcileErr: m.lastReconcileErr,
	}
	m.mu.RUnlock()
	st.ChannelsTotal = len(snaps)
	for _, s := range snaps {
		if s.Healthy {
			st.ChannelsHealthy++
		} else {
			st.ChannelsDegraded++
		}
	}
	return st
}
