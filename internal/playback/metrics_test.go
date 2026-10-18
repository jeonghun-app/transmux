package playback

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const mediaRoute = `route="GET /media/{token}/{center}/{camera}/{resource...}"`

func (f *fixture) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	f.app.MetricsRoutes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics endpoint: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	return rec.Body.String()
}

func requireLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(out, "\n"+line+"\n") {
			t.Errorf("missing %q in\n%s", line, out)
		}
	}
}

// TestMetricsLabelRoutesByPatternNotPath is the cardinality and disclosure
// guard: the media path carries a signed capability and camera identifiers,
// so a raw-path label would both explode the series count and publish
// working playback URLs to anyone who can scrape.
func TestMetricsLabelRoutesByPatternNotPath(t *testing.T) {
	f := newFixture(t)
	record := f.segment(t, 1, time.Now().Add(-5*time.Second), 4*time.Second, []byte("0123456789"), "")
	f.publish(t, record)
	token := f.token(t, Grant{CenterID: "c1", CameraIDs: []string{"cam1"}, Permissions: []string{"live"}})
	resp, body := f.request(t, "POST", "/v1/playback-sessions", token,
		sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "live"}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("create live: %d %s", resp.StatusCode, body)
	}
	url := streamURL(t, body)
	capability := strings.Split(strings.TrimPrefix(url, f.http.URL), "/")[2]
	if resp, _ := f.request(t, "GET", url, "", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("live playlist: %d", resp.StatusCode)
	}
	if resp, _ := f.request(t, "GET", strings.Replace(url, capability, "not-a-token", 1), "", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("invalid capability: %d", resp.StatusCode)
	}
	for _, raw := range []string{"/no/such/path/private-probe", "/assets"} {
		f.request(t, "GET", raw, "", nil, nil)
	}

	out := f.scrape(t)
	requireLines(t, out,
		`transmux_playback_http_requests_total{`+mediaRoute+`,status_class="2xx"} 1`,
		`transmux_playback_http_requests_total{`+mediaRoute+`,status_class="4xx"} 1`,
		`transmux_playback_http_requests_total{route="POST /v1/playback-sessions",status_class="2xx"} 1`,
		`transmux_playback_http_errors_total{error="invalid_session",`+mediaRoute+`,status="401"} 1`,
		`transmux_playback_http_request_duration_seconds_count{`+mediaRoute+`} 2`,
		`transmux_playback_http_request_duration_seconds_count{route="POST /v1/playback-sessions"} 1`,
		`transmux_playback_http_requests_total{route="unmatched",status_class="4xx"} 1`,
	)
	for _, secret := range []string{capability, token, "not-a-token", "private-probe", "cam1", `"c1"`, "viewer", "index.m3u8"} {
		if strings.Contains(out, secret) {
			t.Errorf("metrics expose %q\n%s", secret, out)
		}
	}
}

// TestPatternMuxNeverReportsARequestPath reproduces the case ServeMux
// documents: for a CONNECT trailing-slash redirect it returns the target
// path, here carrying a capability-like segment, instead of a pattern.
func TestPatternMuxNeverReportsARequestPath(t *testing.T) {
	mux := newPatternMux()
	mux.HandleFunc("/m/{token}/", func(http.ResponseWriter, *http.Request) {})
	req := httptest.NewRequest("CONNECT", "/m/secret-capability", nil)
	if _, raw := mux.Handler(req); raw != "/m/secret-capability/" {
		t.Fatalf("ServeMux no longer returns the redirect path (%q); revisit patternMux", raw)
	}
	if got := mux.route(req); got != unmatchedRoute {
		t.Fatalf("route = %q, want %q", got, unmatchedRoute)
	}
	if got := mux.route(httptest.NewRequest("GET", "/m/secret-capability/x", nil)); got != "/m/{token}/" {
		t.Fatalf("route = %q, want the registered pattern", got)
	}
}

// TestMetricsCountServiceRejectionsByReason covers the 503s that were
// invisible outside the proxy: a full max_streams budget and a camera that
// stopped publishing must be distinguishable from a storage failure.
func TestMetricsCountServiceRejectionsByReason(t *testing.T) {
	f := newFixture(t)
	record := f.segment(t, 1, time.Now().Add(-5*time.Second), 4*time.Second, []byte("0123456789"), "")
	f.publish(t, record)
	token := f.token(t, Grant{CenterID: "c1", CameraIDs: []string{"cam1"}, Permissions: []string{"live"}})
	_, body := f.request(t, "POST", "/v1/playback-sessions", token,
		sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "live"}, nil)
	url := streamURL(t, body)

	for range cap(f.app.mediaSlots) {
		f.app.mediaSlots <- struct{}{}
	}
	if resp, _ := f.request(t, "GET", url, "", nil, nil); resp.StatusCode != 503 {
		t.Fatalf("full capacity: %d", resp.StatusCode)
	}
	requireLines(t, f.scrape(t),
		`transmux_playback_media_requests_active 128`,
		`transmux_playback_max_streams 128`)
	for range cap(f.app.mediaSlots) {
		<-f.app.mediaSlots
	}

	f.publish(t, f.segment(t, 2, time.Now().Add(-10*time.Minute), 4*time.Second, []byte("stale"), ""))
	if resp, _ := f.request(t, "GET", url, "", nil, nil); resp.StatusCode != 503 {
		t.Fatalf("offline camera: %d", resp.StatusCode)
	}

	requireLines(t, f.scrape(t),
		`transmux_playback_http_errors_total{error="playback_capacity",`+mediaRoute+`,status="503"} 1`,
		`transmux_playback_http_errors_total{error="camera_offline",`+mediaRoute+`,status="503"} 1`,
		`transmux_playback_http_requests_total{`+mediaRoute+`,status_class="5xx"} 2`,
		`transmux_playback_media_requests_active 0`,
	)
}

// TestMetricsMatchAdminStatus keeps the gauges and counters equal to the
// values GET /v1/admin/status reports, so an alarm and the console never
// disagree about the same quantity.
func TestMetricsMatchAdminStatus(t *testing.T) {
	f := newFixture(t)
	f.app.exportSlots <- struct{}{}
	f.app.retentionMu.Lock()
	f.app.retention.Errors = 3
	f.app.retentionMu.Unlock()
	requireLines(t, f.scrape(t),
		`transmux_playback_exports_active 1`,
		`transmux_playback_export_workers 2`,
		`transmux_playback_retention_errors_total 3`,
		`transmux_playback_index_failures_total 0`,
	)
}

// TestMetricsAreNotServedOnThePublicListener pins the exposure decision: the
// public listener is the authenticated origin, and an unauthenticated scrape
// endpoint there would reveal capacity and error rates to the internet.
func TestMetricsAreNotServedOnThePublicListener(t *testing.T) {
	f := newFixture(t)
	admin := f.token(t, allPermissions())
	for _, token := range []string{"", admin} {
		resp, body := f.request(t, "GET", "/metrics", token, nil, nil)
		if resp.StatusCode != 404 || strings.Contains(string(body), "transmux_playback") {
			t.Fatalf("public listener served metrics: %d %s", resp.StatusCode, body)
		}
	}
	rec := httptest.NewRecorder()
	f.app.MetricsRoutes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/me", nil))
	if rec.Code != 404 {
		t.Fatalf("metrics listener served the API: %d", rec.Code)
	}
}

// TestMetricsWrapperKeepsResponseControl matters because media and export
// downloads extend their write deadline through http.ResponseController; a
// wrapper without Unwrap would silently cut long downloads at the server's
// default deadline.
func TestMetricsWrapperKeepsResponseControl(t *testing.T) {
	f := newFixture(t)
	mux := newPatternMux()
	errs := make(chan error, 1)
	mux.HandleFunc("GET /deadline", func(w http.ResponseWriter, r *http.Request) {
		errs <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
		fail(w, http.StatusTeapot, "test_code", "test")
	})
	server := httptest.NewServer(f.app.observe(mux, mux))
	defer server.Close()
	resp, err := http.Get(server.URL + "/deadline")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-errs; err != nil {
		t.Fatalf("write deadline through the metrics wrapper: %v", err)
	}
	requireLines(t, f.scrape(t),
		`transmux_playback_http_errors_total{error="test_code",route="GET /deadline",status="418"} 1`)
}

func TestMetricsListenIsValidated(t *testing.T) {
	if got := DefaultConfig().MetricsListen; got != "127.0.0.1:8091" {
		t.Fatalf("metrics_listen default = %q, want loopback", got)
	}
	for _, addr := range []string{"", "8091", "127.0.0.1:", "127.0.0.1:no-such-port", ":8090", "0.0.0.0:8090"} {
		cfg := DefaultConfig()
		cfg.MetricsListen = addr
		if cfg.Validate() == nil {
			t.Errorf("metrics_listen %q accepted with http_listen %q", addr, cfg.HTTPListen)
		}
	}
	cfg := DefaultConfig()
	cfg.HTTPListen, cfg.MetricsListen = "10.0.0.5:8090", "127.0.0.1:8090"
	if err := cfg.Validate(); err != nil {
		t.Errorf("distinct interfaces on one port rejected: %v", err)
	}
}
