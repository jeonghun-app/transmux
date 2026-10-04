// Package playback is the authenticated viewer and camera management API.
// It runs separately from transmuxd's internal, unauthenticated monitoring
// listener and never hands a viewer an S3 credential or an RTSP password.
package playback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jeonghun-app/transmux/internal/camera"
	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

type Server struct {
	cfg      Config
	ingest   config.Config
	store    storage.MediaStore
	index    *recording.Index
	auth     *Auth
	log      *slog.Logger
	provider camera.Provider
	managed  *camera.ObjectProvider
	scanner  *recording.Scanner

	mediaSlots     chan struct{}
	exportSlots    chan struct{}
	loginSlots     chan struct{}
	querySlots     chan struct{}
	rosterMu       sync.Mutex
	roster         []config.StaticCamera
	rosterETag     string
	rosterAt       time.Time
	loginClients   *loginLimiter
	loginAccounts  *loginLimiter
	retentionMu    sync.Mutex
	retention      RetentionStatus
	background     context.Context
	stopBackground context.CancelFunc
	jobsMu         sync.Mutex
	jobs           map[string]*exportJob
	exportReserved int64 // bytes held by running export jobs, guarded by jobsMu
	jobsWG         sync.WaitGroup
}

func NewServer(cfg Config, ingest config.Config, store storage.MediaStore, index *recording.Index,
	log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(cfg.Shards) == 0 {
		id := ingest.Cameras.ShardFilter
		if id == "" {
			id = ingest.ShardID
		}
		if !config.ValidID(id) {
			id = "primary"
		}
		cfg.Shards = []ShardConfig{{ID: id, MaxChannels: ingest.MaxChannels}}
	}
	if len(cfg.Shards) > 1 && (ingest.Cameras.Provider != "object" || ingest.Cameras.ShardFilter == "") {
		return nil, fmt.Errorf("multiple managed shards require a shared object roster and cameras.shard_filter")
	}
	if ingest.Cameras.ShardFilter != "" {
		found := false
		for _, shard := range cfg.Shards {
			if shard.ID == ingest.Cameras.ShardFilter {
				found = true
				if shard.MaxChannels > ingest.MaxChannels {
					return nil, fmt.Errorf("configured shard limit exceeds ingest max_channels")
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("cameras.shard_filter is absent from playback shards")
		}
	}
	auth, err := NewAuth(cfg.Auth)
	if err != nil {
		return nil, err
	}
	provider, err := camera.NewProvider(ingest.Cameras, store)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, ingest: ingest, store: store, index: index, auth: auth, log: log,
		provider: provider, mediaSlots: make(chan struct{}, cfg.MaxStreams),
		exportSlots: make(chan struct{}, cfg.Export.Workers), loginSlots: make(chan struct{}, 2),
		querySlots: make(chan struct{}, 8), loginClients: newLoginLimiter(loginClientLimit),
		loginAccounts: newLoginLimiter(loginAccountLimit)}
	s.background, s.stopBackground = context.WithCancel(context.Background())
	s.jobs = make(map[string]*exportJob)
	if err := s.cleanupOrphanedExports(); err != nil {
		s.stopBackground()
		return nil, err
	}
	s.managed, _ = provider.(*camera.ObjectProvider)
	s.scanner = &recording.Scanner{Index: index, Store: store, Roster: s.loadRosterOnly,
		Prefix: ingest.Storage.KeyPrefix, ManifestName: ingest.Storage.ManifestName,
		Days: cfg.Index.Days, Workers: cfg.Index.Workers,
		Interval: cfg.Index.PollInterval.Duration, Log: log}
	return s, nil
}

func (s *Server) RunBackground(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { s.scanner.Run(ctx) })
	wg.Go(func() { s.runRetention(ctx) })
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	defer wg.Wait()
	defer func() {
		s.stopBackground()
		s.pruneExports(time.Now(), true)
		s.jobsWG.Wait()
	}()
	for {
		if err := s.index.PruneSnapshots(time.Now()); err != nil {
			s.log.Error("recording session cleanup failed", "error", err)
		}
		s.pruneExports(time.Now(), false)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := s.loadRoster(r.Context()); err != nil {
			fail(w, http.StatusServiceUnavailable, "catalog_unavailable", "Camera catalog is unavailable.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /v1/login", s.login)
	mux.HandleFunc("GET /v1/me", s.protect(func(w http.ResponseWriter, r *http.Request, c *Claims) {
		writeJSON(w, http.StatusOK, map[string]any{
			"username": c.Subject, "grants": c.Grants, "expires_at": c.ExpiresAt.Time,
			"camera_management": s.managed != nil,
		})
	}))
	mux.HandleFunc("GET /v1/cameras", s.protect(s.catalog))
	mux.HandleFunc("GET /v1/centers", s.protect(s.centers))
	mux.HandleFunc("GET /v1/centers/{center}/cameras", s.protect(s.catalog))
	mux.HandleFunc("POST /v1/playback-sessions", s.protect(s.createSession))
	mux.HandleFunc("POST /v1/playback-sessions/renew", s.protect(s.renewSession))
	mux.HandleFunc("GET /v1/recordings", s.protect(s.recordings))
	mux.HandleFunc("POST /v1/exports", s.protect(s.export))
	mux.HandleFunc("GET /v1/exports", s.protect(s.listExports))
	mux.HandleFunc("GET /v1/exports/{id}", s.protect(s.exportStatus))
	mux.HandleFunc("DELETE /v1/exports/{id}", s.protect(s.cancelExport))
	mux.HandleFunc("GET /exports/{token}/clip.mp4", s.downloadExport)
	mux.HandleFunc("GET /v1/admin/cameras", s.protect(s.adminCameras))
	mux.HandleFunc("POST /v1/admin/cameras", s.protect(s.createCamera))
	mux.HandleFunc("PUT /v1/admin/cameras/{center}/{camera}", s.protect(s.updateCamera))
	mux.HandleFunc("DELETE /v1/admin/cameras/{center}/{camera}", s.protect(s.disableCamera))
	mux.HandleFunc("GET /v1/admin/status", s.protect(s.adminStatus))
	mux.HandleFunc("GET /v1/admin/retention", s.protect(s.retentionPreview))
	mux.HandleFunc("GET /media/{token}/{center}/{camera}/{resource...}", s.media)
	s.console(mux)
	return s.headers(mux)
}

type actorHandler func(http.ResponseWriter, *http.Request, *Claims)

func (s *Server) protect(next actorHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="transmux"`)
			fail(w, http.StatusUnauthorized, "authentication_required", "Sign in to continue.")
			return
		}
		claims, err := s.auth.Parse(raw, false)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			fail(w, http.StatusUnauthorized, "invalid_token", "Your session has expired. Sign in again.")
			return
		}
		if r.URL.Path != "/v1/exports" {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			r = r.WithContext(ctx)
		}
		next(w, r, claims)
	}
}

func (s *Server) headers(next http.Handler) http.Handler {
	origins := make(map[string]bool, len(s.cfg.AllowedOrigins)+1)
	origins[strings.TrimSuffix(s.cfg.PublicURL, "/")] = true
	for _, origin := range s.cfg.AllowedOrigins {
		origins[origin] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self' blob:; worker-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Add("Vary", "Origin")
			if !origins[origin] {
				fail(w, http.StatusForbidden, "origin_not_allowed", "This origin is not allowed.")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Content-Range, Content-Disposition, X-Recording-Start, X-Recording-End")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match, Range")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	limited := func() {
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "login_rate_limit", "Wait a minute before signing in again.")
	}
	if !s.loginClients.allow(s.auth.ClientKey(r), time.Now()) {
		limited()
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	// Unknown names are counted too, so the limit does not reveal which
	// accounts exist. Login rejects over-long names before hashing.
	if len(body.Username) <= 64 && !s.loginAccounts.allow(body.Username, time.Now()) {
		limited()
		return
	}
	if !take(s.loginSlots) {
		limited()
		return
	}
	defer release(s.loginSlots)
	token, claims, err := s.auth.Login(body.Username, body.Password)
	if err != nil {
		fail(w, http.StatusUnauthorized, "invalid_credentials", "Username or password is incorrect.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token, "token_type": "Bearer", "expires_at": claims.ExpiresAt.Time,
	})
}

func (s *Server) loadRosterOnly(ctx context.Context) ([]config.StaticCamera, error) {
	roster, _, err := s.loadRoster(ctx)
	return roster, err
}

func (s *Server) loadRoster(ctx context.Context) ([]config.StaticCamera, string, error) {
	s.rosterMu.Lock()
	defer s.rosterMu.Unlock()
	if !s.rosterAt.IsZero() && time.Since(s.rosterAt) < 2*time.Second {
		return append([]config.StaticCamera(nil), s.roster...), s.rosterETag, nil
	}
	var roster []config.StaticCamera
	var etag string
	var err error
	switch provider := s.provider.(type) {
	case *camera.ObjectProvider:
		roster, etag, err = provider.Load(ctx)
	case *camera.HTTPProvider:
		roster, err = provider.Load(ctx)
	default:
		roster = append([]config.StaticCamera{}, s.ingest.Cameras.Static...)
	}
	if err != nil {
		return nil, "", err
	}
	sort.Slice(roster, func(i, j int) bool {
		return roster[i].CenterID+"/"+roster[i].CameraID < roster[j].CenterID+"/"+roster[j].CameraID
	})
	s.roster, s.rosterETag, s.rosterAt = roster, etag, time.Now()
	return append([]config.StaticCamera(nil), roster...), etag, nil
}

func (s *Server) invalidateRoster() {
	s.rosterMu.Lock()
	s.rosterAt = time.Time{}
	s.rosterMu.Unlock()
}

func take(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func release(slots chan struct{}) { <-slots }

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if mediaType := strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]; mediaType != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "json_required", "Use Content-Type: application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			fail(w, http.StatusRequestEntityTooLarge, "body_too_large", "The request is too large.")
		} else {
			fail(w, http.StatusBadRequest, "invalid_json", "The request must be valid JSON with supported fields.")
		}
		return false
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		fail(w, http.StatusBadRequest, "invalid_json", "The request must contain one JSON document.")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (s *Server) unavailable(w http.ResponseWriter, operation string, err error) {
	s.log.Warn(operation, "error", err)
	fail(w, http.StatusServiceUnavailable, "temporarily_unavailable", "The service is temporarily unavailable. Try again.")
}
