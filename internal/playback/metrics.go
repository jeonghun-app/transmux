package playback

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jeonghun-app/transmux/internal/metrics"
)

// unmatchedRoute labels every request that did not resolve to a registered
// pattern: unknown paths, wrong methods and CORS preflights.
const unmatchedRoute = "unmatched"

// requestBuckets spans a cached playlist (milliseconds) to a long segment or
// export download, which is timed until the body has been written.
var requestBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// patternMux is a ServeMux that remembers its patterns so the route label is
// always one of them. ServeMux.Handler documents that for its own redirects
// it may return the redirect target path instead of a pattern, and a media
// path carries a playback capability and camera identifiers. Allowing only
// registered patterns bounds the label set by construction.
type patternMux struct {
	*http.ServeMux
	patterns map[string]bool
}

func newPatternMux() *patternMux {
	return &patternMux{ServeMux: http.NewServeMux(), patterns: map[string]bool{}}
}

func (m *patternMux) Handle(pattern string, handler http.Handler) {
	m.patterns[pattern] = true
	m.ServeMux.Handle(pattern, handler)
}

func (m *patternMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.patterns[pattern] = true
	m.ServeMux.HandleFunc(pattern, handler)
}

func (m *patternMux) route(r *http.Request) string {
	if _, pattern := m.Handler(r); m.patterns[pattern] {
		return pattern
	}
	return unmatchedRoute
}

// statusRecorder captures the response status and, through fail, the API
// error code. It is the only writer wrapper on the request path.
type statusRecorder struct {
	http.ResponseWriter
	status    int
	errorCode string
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// ReadFrom keeps the underlying writer's sendfile path for export downloads,
// which http.ServeContent copies with io.Copy.
func (w *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return io.Copy(w.ResponseWriter, src)
}

// Unwrap lets http.ResponseController reach the connection; media and export
// downloads extend their write deadline through it.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// observe counts every request on the public listener, including those that
// headers rejects before routing.
//
// Labels are limited to the route pattern, the status and the API error
// code. All three come from fixed sets in this package, so no capability,
// center or camera identifier, or user name can reach a label.
func (s *Server) observe(routes *patternMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := routes.route(r)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		routeLabel := metrics.Label{Name: "route", Value: route}
		s.metrics.Counter("transmux_playback_http_requests_total",
			"Requests on the public listener by route pattern and status class.",
			routeLabel, metrics.Label{Name: "status_class", Value: strconv.Itoa(status/100) + "xx"}).Inc()
		s.metrics.Histogram("transmux_playback_http_request_duration_seconds",
			"Time until the handler returned, including the response body.",
			requestBuckets, routeLabel).Observe(time.Since(start).Seconds())
		if rec.errorCode != "" {
			s.metrics.Counter("transmux_playback_http_errors_total",
				"API error responses by route pattern, status and error code.",
				routeLabel, metrics.Label{Name: "status", Value: strconv.Itoa(status)},
				metrics.Label{Name: "error", Value: rec.errorCode}).Inc()
		}
	})
}

// registerMetrics exposes the values GET /v1/admin/status reports. They are
// read at scrape time from the same sources, so an alarm and the console
// never disagree.
func (s *Server) registerMetrics() {
	s.metrics.GaugeFunc("transmux_playback_media_requests_active",
		"Media requests holding a max_streams slot.",
		func() float64 { return float64(len(s.mediaSlots)) })
	s.metrics.Gauge("transmux_playback_max_streams",
		"Configured max_streams; media requests beyond it receive 503 playback_capacity.").
		Set(float64(cap(s.mediaSlots)))
	s.metrics.GaugeFunc("transmux_playback_exports_active",
		"Export jobs holding a worker slot.",
		func() float64 { return float64(len(s.exportSlots)) })
	s.metrics.Gauge("transmux_playback_export_workers",
		"Configured export.workers.").Set(float64(cap(s.exportSlots)))
	s.metrics.CounterFunc("transmux_playback_index_failures_total",
		"Recording indexer failures since start.",
		func() float64 { return float64(s.scanner.Status().Failures) })
	s.metrics.CounterFunc("transmux_playback_retention_errors_total",
		"Retention pass errors since start.",
		func() float64 {
			s.retentionMu.Lock()
			defer s.retentionMu.Unlock()
			return float64(s.retention.Errors)
		})
}

// Metrics is the registry served by MetricsRoutes. The command adds build
// metadata to it.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

// MetricsRoutes serves GET /metrics for the internal metrics listener only.
// It is deliberately absent from Routes: that handler is the public,
// authenticated origin, and the scrape endpoint has no authentication.
func (s *Server) MetricsRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.metrics.Render())
	})
	return mux
}
