package health

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/channel"
	"github.com/jeonghun-app/transmux/internal/metrics"
)

type fakeSource struct {
	status channel.Status
	snaps  []channel.Snapshot
}

func (f *fakeSource) Status() channel.Status         { return f.status }
func (f *fakeSource) Snapshots() []channel.Snapshot  { return f.snaps }
func (f *fakeSource) Snapshot(center, camera string) (channel.Snapshot, bool) {
	for _, s := range f.snaps {
		if s.CenterID == center && s.CameraID == camera {
			return s, true
		}
	}
	return channel.Snapshot{}, false
}

func newTestHandler(st channel.Status, snaps ...channel.Snapshot) *Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(&fakeSource{status: st, snaps: snaps}, metrics.NewRegistry(), "store", log)
}

func get(t *testing.T, h *Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s returned invalid JSON: %v\n%s", path, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

// TestLivezIgnoresChannelHealth is the property that keeps a partial fault
// from becoming a total one. If liveness tracked channel health, one
// unreachable camera or a brief store outage would have the orchestrator
// restart the container and drop every other channel on the shard.
func TestLivezIgnoresChannelHealth(t *testing.T) {
	h := newTestHandler(channel.Status{
		Ready: false, ChannelsTotal: 10, ChannelsDegraded: 10,
	})
	code, body := get(t, h, "/livez")
	if code != http.StatusOK {
		t.Fatalf("livez = %d, want 200 even with every channel degraded", code)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
}

func TestReadyzWaitsForTheRoster(t *testing.T) {
	h := newTestHandler(channel.Status{Ready: false})
	code, body := get(t, h, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503 before the roster loads", code)
	}
	if !strings.Contains(body["reason"].(string), "roster") {
		t.Errorf("reason = %v, want it to mention the roster", body["reason"])
	}
}

func TestReadyzRejectsAnEmptyShard(t *testing.T) {
	h := newTestHandler(channel.Status{Ready: true, ChannelsTotal: 0})
	if code, _ := get(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503 with no channels assigned", code)
	}
}

// TestReadyzDegradedBoundary pins the exact threshold, which is the kind of
// off-by-one that silently changes rollout behaviour: the shard stays ready
// while at least half of its channels are healthy.
func TestReadyzDegradedBoundary(t *testing.T) {
	cases := []struct {
		total, degraded, want int
	}{
		{total: 10, degraded: 0, want: http.StatusOK},
		{total: 10, degraded: 5, want: http.StatusOK},  // exactly half healthy
		{total: 10, degraded: 6, want: http.StatusServiceUnavailable},
		{total: 1, degraded: 1, want: http.StatusServiceUnavailable},
		{total: 2, degraded: 1, want: http.StatusOK},
	}
	for _, c := range cases {
		h := newTestHandler(channel.Status{
			Ready: true, ChannelsTotal: c.total, ChannelsDegraded: c.degraded,
			ChannelsHealthy: c.total - c.degraded,
		})
		if code, _ := get(t, h, "/readyz"); code != c.want {
			t.Errorf("total=%d degraded=%d: readyz = %d, want %d",
				c.total, c.degraded, code, c.want)
		}
	}
}

// TestHealthzAlwaysReturns200 is what the Zabbix and WhaTap collectors rely
// on: they alarm on body fields, so a non-200 would be read as "the collector
// cannot reach the shard" rather than "the shard reports a problem".
func TestHealthzAlwaysReturns200(t *testing.T) {
	for _, st := range []channel.Status{
		{Ready: false},
		{Ready: true, ChannelsTotal: 4, ChannelsDegraded: 4},
		{Ready: true, ChannelsTotal: 4, ChannelsHealthy: 4},
	} {
		code, body := get(t, newTestHandler(st), "/healthz")
		if code != http.StatusOK {
			t.Errorf("healthz = %d for %+v, want 200", code, st)
		}
		if _, ok := body["channels_degraded"]; !ok {
			t.Error("healthz body must carry the field the collector alarms on")
		}
	}
}

func TestHealthzStatusWording(t *testing.T) {
	cases := []struct {
		st   channel.Status
		want string
	}{
		{channel.Status{Ready: false}, "starting"},
		{channel.Status{Ready: true, ChannelsTotal: 2, ChannelsDegraded: 0}, "ok"},
		{channel.Status{Ready: true, ChannelsTotal: 2, ChannelsDegraded: 1}, "degraded"},
		{channel.Status{Ready: true, ChannelsTotal: 2, ChannelsDegraded: 2}, "down"},
	}
	for _, c := range cases {
		_, body := get(t, newTestHandler(c.st), "/healthz")
		if body["status"] != c.want {
			t.Errorf("%+v: status = %v, want %q", c.st, body["status"], c.want)
		}
	}
}

func TestChannelsListAndLookup(t *testing.T) {
	snap := channel.Snapshot{
		CenterID: "c1", CameraID: "cam1", State: channel.StateReceiving,
		SourceURL: "rtsp://***@cam.example:554/live", StartedAt: time.Now().UTC(),
	}
	h := newTestHandler(channel.Status{Ready: true, ChannelsTotal: 1}, snap)

	code, body := get(t, h, "/channels")
	if code != http.StatusOK {
		t.Fatalf("/channels = %d", code)
	}
	if list, ok := body["channels"].([]any); !ok || len(list) != 1 {
		t.Fatalf("/channels body = %v", body["channels"])
	}

	code, body = get(t, h, "/channels/c1/cam1")
	if code != http.StatusOK {
		t.Fatalf("/channels/c1/cam1 = %d", code)
	}
	if body["camera_id"] != "cam1" {
		t.Errorf("camera_id = %v", body["camera_id"])
	}

	if code, _ := get(t, h, "/channels/c1/nope"); code != http.StatusNotFound {
		t.Errorf("unknown channel = %d, want 404", code)
	}
	if code, _ := get(t, h, "/channels/c1"); code != http.StatusBadRequest {
		t.Errorf("malformed path = %d, want 400", code)
	}
}

// TestChannelsNeverExposeARawURL is a guard on the endpoint, not just on
// SafeURL: /channels is unauthenticated, so a credential reaching a snapshot
// would be served to anyone who can reach the port.
func TestChannelsNeverExposeARawURL(t *testing.T) {
	snap := channel.Snapshot{
		CenterID: "c1", CameraID: "cam1",
		SourceURL: "rtsp://***@cam.example:554/live?<redacted>",
	}
	h := newTestHandler(channel.Status{Ready: true, ChannelsTotal: 1}, snap)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/channels", nil))
	for _, forbidden := range []string{"password", "admin:", "token="} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("/channels leaked %q", forbidden)
		}
	}
}

func TestMetricsIsPrometheusText(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.Gauge("transmux_test_gauge", "help").Set(2)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(&fakeSource{}, reg, "store", log)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "transmux_test_gauge 2") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// Monitoring responses must not be cached: a probe reading a cached body would
// report a state the shard left minutes ago.
func TestResponsesAreNotCacheable(t *testing.T) {
	h := newTestHandler(channel.Status{Ready: true, ChannelsTotal: 1})
	for _, path := range []string{"/livez", "/readyz", "/healthz", "/channels"} {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
	}
}
