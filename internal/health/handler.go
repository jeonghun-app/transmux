// Package health exposes the monitoring surface: liveness, readiness, a
// channel inventory for Zabbix/WhaTap, and Prometheus metrics.
//
// SECURITY: these endpoints are unauthenticated. They expose the camera
// inventory and per-channel state (never credentials or RTSP URLs with
// userinfo). Bind them to an internal interface or place them behind the
// service mesh / security group; do not expose the listener to the public
// internet or to the camera network.
package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jeonghun-app/transmux/internal/channel"
	"github.com/jeonghun-app/transmux/internal/metrics"
)

// StatusSource is the view of the channel manager the monitoring endpoints
// need. It is an interface so the handler can be tested against specific
// shard states without running real workers.
type StatusSource interface {
	Status() channel.Status
	Snapshots() []channel.Snapshot
	Snapshot(centerID, cameraID string) (channel.Snapshot, bool)
}

// Handler serves the monitoring endpoints.
type Handler struct {
	mgr   StatusSource
	reg   *metrics.Registry
	log   *slog.Logger
	store string

	// degradedRatio is the share of unhealthy channels above which /readyz
	// reports not-ready. Liveness is never affected by channel health.
	degradedRatio float64
}

func NewHandler(mgr StatusSource, reg *metrics.Registry, storeDesc string, log *slog.Logger) *Handler {
	return &Handler{mgr: mgr, reg: reg, log: log, store: storeDesc, degradedRatio: 0.5}
}

// Routes builds the mux.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", h.livez)
	mux.HandleFunc("/readyz", h.readyz)
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/channels", h.channels)
	mux.HandleFunc("/channels/", h.channel)
	mux.HandleFunc("/metrics", h.metricsHandler)
	return mux
}

// livez reports only that the process is running and its goroutines are
// scheduling.
//
// It deliberately ignores camera and object-store health. If a single
// unreachable camera or a transient S3 outage failed liveness, the
// orchestrator would restart the container and drop every other channel on
// the shard, turning a partial fault into a total one.
func (h *Handler) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC(),
	})
}

// readyz reports whether the shard is doing useful work: the roster has been
// loaded and most channels are receiving.
func (h *Handler) readyz(w http.ResponseWriter, _ *http.Request) {
	st := h.mgr.Status()
	code := http.StatusOK
	reason := ""
	switch {
	case !st.Ready:
		code, reason = http.StatusServiceUnavailable, "camera roster not loaded yet"
	case st.ChannelsTotal == 0:
		code, reason = http.StatusServiceUnavailable, "no channels assigned"
	case float64(st.ChannelsDegraded) > h.degradedRatio*float64(st.ChannelsTotal):
		code, reason = http.StatusServiceUnavailable, "majority of channels degraded"
	}
	writeJSON(w, code, map[string]any{
		"status": statusWord(code),
		"reason": reason,
		"shard":  st,
		"store":  h.store,
	})
}

// healthz is the aggregate view consumed by Zabbix and WhaTap. It always
// returns 200 with a machine-readable body so the collector can alarm on
// fields rather than on the status code.
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	st := h.mgr.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            statusWordFromShard(st),
		"shard":             st,
		"store":             h.store,
		"channels_total":    st.ChannelsTotal,
		"channels_healthy":  st.ChannelsHealthy,
		"channels_degraded": st.ChannelsDegraded,
		"time":              time.Now().UTC(),
	})
}

func (h *Handler) channels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"shard":    h.mgr.Status(),
		"channels": h.mgr.Snapshots(),
	})
}

// channel serves /channels/{center_id}/{camera_id}.
func (h *Handler) channel(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/channels/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "path must be /channels/{center_id}/{camera_id}",
		})
		return
	}
	snap, ok := h.mgr.Snapshot(parts[0], parts[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "channel not found"})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (h *Handler) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.reg.Render())
}

func statusWord(code int) string {
	if code == http.StatusOK {
		return "ok"
	}
	return "unavailable"
}

func statusWordFromShard(st channel.Status) string {
	switch {
	case !st.Ready:
		return "starting"
	case st.ChannelsDegraded == 0:
		return "ok"
	case st.ChannelsDegraded < st.ChannelsTotal:
		return "degraded"
	default:
		return "down"
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
