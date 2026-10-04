package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
	"github.com/jeonghun-app/transmux/internal/storage"
)

type fixture struct {
	app   *Server
	http  *httptest.Server
	store *storage.FilesystemStore
	index *recording.Index
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("TRANSMUX_TEST_SECRET", strings.Repeat("test-signing-material-", 3))
	t.Setenv("TRANSMUX_TEST_PASSWORD", "test-only-password")
	cfg := DefaultConfig()
	cfg.Auth.SecretEnv = "TRANSMUX_TEST_SECRET"
	cfg.Auth.Users = []UserConfig{{Username: "admin", PasswordEnv: "TRANSMUX_TEST_PASSWORD",
		Grants: []Grant{{CenterID: "*", CameraIDs: []string{"*"}, Permissions: []string{"live", "recording", "export", "manage"}}}}}
	cfg.AllowedOrigins = []string{"https://operator.example"}
	cfg.IndexPath = filepath.Join(t.TempDir(), "recordings.db")
	cfg.Export.TempDir = t.TempDir()
	ingest := config.Default()
	ingest.Storage.Backend = "filesystem"
	ingest.Storage.Root = t.TempDir()
	ingest.Storage.KeyPrefix = "archive"
	ingest.Cameras.Provider = "object"
	ingest.Cameras.ObjectKey = "_transmux/rosters/test.json"
	ingest.Cameras.Static = []config.StaticCamera{
		{CenterID: "c1", CameraID: "cam1", Name: "Entrance", RTSPURL: "rtsp://admin:private-camera-password@camera/stream?token=private-query"},
		{CenterID: "c2", CameraID: "cam2", Name: "Other center", RTSPURL: "rtsp://camera2/live"},
	}
	ingest.MaxChannels = 3
	store, err := storage.NewFilesystemStore(ingest.Storage.Root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := recording.Open(cfg.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	cfg.PublicURL = "http://" + server.Listener.Addr().String()
	app, err := NewServer(cfg, ingest, store, index, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = app.Routes()
	server.Start()
	t.Cleanup(func() {
		server.Close()
		app.stopBackground()
		app.pruneExports(time.Now(), true)
		app.jobsWG.Wait()
		index.Close()
	})
	return &fixture{app, server, store, index}
}

func (f *fixture) token(t *testing.T, grants ...Grant) string {
	t.Helper()
	claims := &Claims{Type: "access", Grants: grants, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: f.app.cfg.Auth.Issuer, Subject: "viewer", Audience: jwt.ClaimStrings{f.app.cfg.Auth.Audience},
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Second)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	token, err := f.app.auth.sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func allPermissions() Grant {
	return Grant{CenterID: "*", CameraIDs: []string{"*"}, Permissions: []string{"live", "recording", "export", "manage"}}
}

func (f *fixture) request(t *testing.T, method, url, token string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	if strings.HasPrefix(url, "/") {
		url = f.http.URL + url
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func (f *fixture) segment(t *testing.T, sequence uint64, pdt time.Time, duration time.Duration, body []byte, initURI string) recording.Segment {
	t.Helper()
	pdt = time.UnixMilli(pdt.UnixMilli()).UTC()
	extension := ".ts"
	if initURI != "" {
		extension = ".m4s"
	}
	uri := path.Join(pdt.Format("2006/01/02"), fmt.Sprintf("seg-%09d-%d%s", sequence, pdt.UnixMilli(), extension))
	key := "archive/c1/cam1/" + uri
	md := map[string]string{"center-id": "c1", "camera-id": "cam1", "sequence": strconv.FormatUint(sequence, 10),
		"pdt-ms": strconv.FormatInt(pdt.UnixMilli(), 10), "duration-ms": strconv.FormatInt(duration.Milliseconds(), 10),
		"discontinuity": "false", "init-uri": initURI}
	if _, err := f.store.Put(context.Background(), storage.Object{Key: key, Body: body, Metadata: md}); err != nil {
		t.Fatal(err)
	}
	record, err := recording.FromObject("archive", "c1", "cam1", storage.Entry{Key: key}, storage.ObjectInfo{Size: int64(len(body)), Metadata: md})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.index.Put([]recording.Segment{record}); err != nil {
		t.Fatal(err)
	}
	return record
}

func (f *fixture) publish(t *testing.T, records ...recording.Segment) {
	t.Helper()
	var segments []hls.PublishedSegment
	for _, record := range records {
		segments = append(segments, hls.PublishedSegment{Sequence: record.Sequence, URI: record.URI, InitURI: record.InitURI,
			ProgramDateTime: record.Start, Duration: time.Duration(record.DurationMS) * time.Millisecond})
	}
	_, err := f.store.Put(context.Background(), storage.Object{Key: "archive/c1/cam1/index.m3u8", Body: hls.RenderLive(hls.Live{Segments: segments})})
	if err != nil {
		t.Fatal(err)
	}
}

func streamURL(t *testing.T, body []byte) string {
	t.Helper()
	var result struct {
		Streams []sessionView `json:"streams"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Streams) != 1 {
		t.Fatalf("invalid session: %s %v", body, err)
	}
	return result.Streams[0].URL
}

func TestAccessTokensRejectAlgorithmAudienceExpiryAndScopeConfusion(t *testing.T) {
	f := newFixture(t)
	valid := func() *Claims {
		return &Claims{Type: "access", Grants: []Grant{allPermissions()}, RegisteredClaims: jwt.RegisteredClaims{
			Issuer: f.app.cfg.Auth.Issuer, Subject: "viewer", Audience: jwt.ClaimStrings{f.app.cfg.Auth.Audience},
			IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		}}
	}
	tests := []struct {
		name   string
		change func(*Claims)
		method jwt.SigningMethod
	}{
		{"wrong issuer", func(c *Claims) { c.Issuer = "other" }, jwt.SigningMethodHS256},
		{"wrong audience", func(c *Claims) { c.Audience = jwt.ClaimStrings{"transmux-media"} }, jwt.SigningMethodHS256},
		{"expired", func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Second)) }, jwt.SigningMethodHS256},
		{"missing expiry", func(c *Claims) { c.ExpiresAt = nil }, jwt.SigningMethodHS256},
		{"type confusion", func(c *Claims) { c.Type = "media" }, jwt.SigningMethodHS256},
		{"bad grant", func(c *Claims) { c.Grants[0].CenterID = "../c1" }, jwt.SigningMethodHS256},
		{"wrong algorithm", func(c *Claims) {}, jwt.SigningMethodHS512},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := valid()
			tc.change(claims)
			raw, err := jwt.NewWithClaims(tc.method, claims).SignedString(f.app.auth.key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.auth.Parse(raw, false); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	resp, body := f.request(t, "POST", "/v1/login", "", map[string]string{"username": "admin", "password": "test-only-password"}, nil)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("access_token")) {
		t.Fatalf("standalone login failed: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.request(t, "POST", "/v1/login", "", map[string]string{"username": "admin", "password": "wrong"}, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong password accepted: %d", resp.StatusCode)
	}
}

func TestLiveDeliveryIsPrivateScopedAndSupportsRanges(t *testing.T) {
	f := newFixture(t)
	record := f.segment(t, 1, time.Now().Add(-5*time.Second), 4*time.Second, []byte("0123456789"), "")
	f.publish(t, record)
	token := f.token(t, Grant{CenterID: "c1", CameraIDs: []string{"cam1"}, Permissions: []string{"live"}})
	resp, body := f.request(t, "GET", "/v1/cameras", token, nil, nil)
	if resp.StatusCode != 200 || bytes.Contains(body, []byte("cam2")) || bytes.Contains(body, []byte("private-")) || bytes.Contains(body, []byte("rtsp")) {
		t.Fatalf("catalog leaked another tenant or source credential: %d %s", resp.StatusCode, body)
	}
	for _, endpoint := range []string{"/channels", "/metrics"} {
		resp, _ := f.request(t, "GET", endpoint, token, nil, nil)
		if resp.StatusCode != 404 {
			t.Fatalf("internal endpoint exposed: %s %d", endpoint, resp.StatusCode)
		}
	}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "live"}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("create live: %d %s", resp.StatusCode, body)
	}
	url := streamURL(t, body)
	resp, body = f.request(t, "GET", url, "", nil, nil)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(record.URI)) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("live response: %d %s", resp.StatusCode, body)
	}
	segmentURL := strings.TrimSuffix(url, "index.m3u8") + record.URI
	resp, body = f.request(t, "GET", segmentURL, "", nil, map[string]string{"Range": "bytes=2-5"})
	if resp.StatusCode != 206 || string(body) != "2345" || resp.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range response: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.request(t, "GET", strings.Replace(url, "/c1/cam1/", "/c2/cam2/", 1), "", nil, nil)
	if resp.StatusCode != 403 {
		t.Fatal("capability escaped camera scope")
	}
	resp, _ = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+"_transmux/lease.json", "", nil, nil)
	if resp.StatusCode != 404 {
		t.Fatal("capability exposed control object")
	}
	resp, _ = f.request(t, "GET", url, "", nil, map[string]string{"Origin": "https://untrusted.example"})
	if resp.StatusCode != 403 {
		t.Fatal("untrusted origin accepted")
	}
	resp, _ = f.request(t, "GET", url, "", nil, map[string]string{"Origin": "https://operator.example"})
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://operator.example" {
		t.Fatal("allowed origin failed")
	}
	parts := strings.Split(strings.TrimPrefix(url, f.http.URL), "/")
	cap, err := f.app.auth.Parse(parts[2], true)
	if err != nil {
		t.Fatal(err)
	}
	cap.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Second))
	expired, err := f.app.auth.sign(cap)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = f.request(t, "GET", strings.Replace(segmentURL, parts[2], expired, 1), "", nil, map[string]string{"Range": "bytes=0-1"})
	if resp.StatusCode != 401 {
		t.Fatal("expired token could retrieve a byte range")
	}
	resp, _ = f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "recording",
		Start: record.Start, End: record.End()}, nil)
	if resp.StatusCode != 403 {
		t.Fatal("live-only viewer acquired recording access")
	}
}

func TestRecordingSessionSnapshotDoesNotExpandDuringBackfill(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	first := f.segment(t, 1, base, 4*time.Second, []byte("first"), "")
	last := f.segment(t, 3, base.Add(8*time.Second), 4*time.Second, []byte("last"), "")
	token := f.token(t, allPermissions())
	req := sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "recording", Start: base.Add(time.Second), End: last.End().Add(-time.Second)}
	resp, body := f.request(t, "POST", "/v1/playback-sessions", token, req, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("recording session: %d %s", resp.StatusCode, body)
	}
	url := streamURL(t, body)
	resp, manifest := f.request(t, "GET", url, "", nil, nil)
	if resp.StatusCode != 200 || !bytes.Contains(manifest, []byte("#EXT-X-ENDLIST")) || !bytes.Contains(manifest, []byte(first.URI)) {
		t.Fatalf("finite recording failed: %s", manifest)
	}
	late := f.segment(t, 2, base.Add(4*time.Second), 4*time.Second, []byte("late"), "")
	_, after := f.request(t, "GET", url, "", nil, nil)
	if !bytes.Equal(after, manifest) {
		t.Fatal("late backfill changed an active VOD")
	}
	resp, _ = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+late.URI, "", nil, nil)
	if resp.StatusCode != 403 {
		t.Fatal("snapshot token admitted a newly indexed object")
	}
	tokenPart := strings.Split(strings.TrimPrefix(url, f.http.URL), "/")[2]
	resp, body = f.request(t, "POST", "/v1/playback-sessions/renew", token, map[string]string{"session_token": tokenPart}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("renewal failed: %d %s", resp.StatusCode, body)
	}
	var renewed sessionView
	if err := json.Unmarshal(body, &renewed); err != nil {
		t.Fatal(err)
	}
	_, after = f.request(t, "GET", renewed.URL, "", nil, nil)
	if !bytes.Equal(after, manifest) {
		t.Fatal("renewal changed the recording snapshot")
	}
}

func TestFMP4InitializationRequiresSnapshotMembership(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Add(-time.Minute)
	initURI := base.UTC().Format("2006/01/02") + "/init-" + strings.Repeat("a", 64) + ".mp4"
	if _, err := f.store.Put(context.Background(), storage.Object{Key: "archive/c1/cam1/" + initURI, Body: []byte("init")}); err != nil {
		t.Fatal(err)
	}
	record := f.segment(t, 1, base, 4*time.Second, []byte("fragment"), initURI)
	token := f.token(t, allPermissions())
	resp, body := f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"}, Mode: "recording", Start: record.Start, End: record.End()}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("fMP4 session: %s", body)
	}
	url := streamURL(t, body)
	resp, body = f.request(t, "GET", url, "", nil, nil)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("#EXT-X-MAP:URI=\""+initURI+"\"")) {
		t.Fatalf("missing fMP4 map: %s", body)
	}
	resp, _ = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+initURI, "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatal("authorized init denied")
	}
	resp, _ = f.request(t, "GET", strings.TrimSuffix(url, "index.m3u8")+strings.Replace(initURI, strings.Repeat("a", 64), strings.Repeat("b", 64), 1), "", nil, nil)
	if resp.StatusCode != 403 {
		t.Fatal("arbitrary init admitted")
	}
}

func TestCameraManagementChecksVersionScopeAndKeepsDisabledArchive(t *testing.T) {
	f := newFixture(t)
	token := f.token(t, allPermissions())
	resp, body := f.request(t, "GET", "/v1/admin/cameras", token, nil, nil)
	version := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || version == "" || bytes.Contains(body, []byte("private-camera-password")) {
		t.Fatalf("admin roster: %d %s", resp.StatusCode, body)
	}
	newCamera := config.StaticCamera{CenterID: "c1", CameraID: "newcam", RTSPURL: "rtsp://camera/live", Name: "Added"}
	resp, _ = f.request(t, "POST", "/v1/admin/cameras", token, newCamera, nil)
	if resp.StatusCode != 428 {
		t.Fatal("roster mutation lacked If-Match")
	}
	resp, body = f.request(t, "POST", "/v1/admin/cameras", token, newCamera, map[string]string{"If-Match": version})
	if resp.StatusCode != 201 {
		t.Fatalf("camera create: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.request(t, "PUT", "/v1/admin/cameras/c1/newcam", token, newCamera, map[string]string{"If-Match": version})
	if resp.StatusCode != 412 {
		t.Fatal("stale edit overwrote shared roster")
	}
	resp, _ = f.request(t, "GET", "/v1/admin/cameras", token, nil, nil)
	version = resp.Header.Get("ETag")
	restricted := f.token(t, Grant{CenterID: "c2", CameraIDs: []string{"*"}, Permissions: []string{"manage"}})
	resp, _ = f.request(t, "DELETE", "/v1/admin/cameras/c1/newcam", restricted, nil, map[string]string{"If-Match": version})
	if resp.StatusCode != 403 {
		t.Fatal("cross-center administrator changed a camera")
	}
	resp, body = f.request(t, "DELETE", "/v1/admin/cameras/c1/newcam", token, nil, map[string]string{"If-Match": version})
	if resp.StatusCode != 200 {
		t.Fatalf("disable: %d %s", resp.StatusCode, body)
	}
	cams, err := f.app.provider.Cameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cam := range cams {
		if cam.CameraID == "newcam" {
			t.Fatal("disabled camera still assigned to ingest")
		}
	}
	resp, body = f.request(t, "GET", "/v1/cameras", token, nil, nil)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("newcam")) {
		t.Fatal("disabling orphaned the retained recordings")
	}
}

func TestRetentionDiscoversImportedMediaAndNeverDeletesControlObjects(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Retention.Days = 2
	old := f.segment(t, 1, time.Now().AddDate(0, 0, -8), 4*time.Second, []byte("old media"), "")
	if err := f.index.Remove(old); err != nil {
		t.Fatal(err)
	} // imported object with no index entry
	recent := f.segment(t, 2, time.Now().Add(-time.Minute), 4*time.Second, []byte("recent"), "")
	f.publish(t, recent)
	controls := []string{"archive/c1/cam1/_transmux/lease.json", "archive/c1/cam1/operator-notes.txt"}
	for _, key := range controls {
		if _, err := f.store.Put(context.Background(), storage.Object{Key: key, Body: []byte("keep")}); err != nil {
			t.Fatal(err)
		}
	}
	f.app.retentionPass(context.Background(), 0)
	if _, err := f.store.Head(context.Background(), old.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired imported media not deleted: %v", err)
	}
	for _, key := range append(controls, recent.Key, "archive/c1/cam1/index.m3u8", "_transmux/rosters/test.json") {
		if _, err := f.store.Head(context.Background(), key); err != nil {
			t.Fatalf("retention deleted protected or recent object %s: %v", key, err)
		}
	}
}
