package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func TestMultiCameraSessionFailureLeavesNoSnapshot(t *testing.T) {
	f := newFixture(t)
	token := f.token(t, allPermissions())
	resp, _ := f.request(t, "GET", "/v1/admin/cameras", token, nil, nil)
	second := config.StaticCamera{CenterID: "c1", CameraID: "cam3", RTSPURL: "rtsp://camera3/live", Name: "Dock"}
	resp, body := f.request(t, "POST", "/v1/admin/cameras", token, second, map[string]string{"If-Match": resp.Header.Get("ETag")})
	if resp.StatusCode != 201 {
		t.Fatalf("camera create: %d %s", resp.StatusCode, body)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	f.cameraSegment(t, "cam1", 1, base, 4*time.Second, []byte("one"), "")
	f.cameraSegment(t, "cam3", 1, base, 4*time.Second, []byte("three"), "")
	// One slot left, as in 1999 of 2000: a two-camera request must fail
	// without consuming it.
	f.app.cfg.MaxSessions = 2
	held := []hls.PublishedSegment{{URI: "held.ts", Duration: time.Second, ProgramDateTime: base}}
	if err := f.index.Snapshot("held", time.Now().Add(time.Minute), held, 2); err != nil {
		t.Fatal(err)
	}
	req := sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1", "cam3"}, Mode: "recording",
		Start: base, End: base.Add(4 * time.Second)}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", token, req, nil)
	if resp.StatusCode != 429 || !bytes.Contains(body, []byte("session_capacity")) {
		t.Fatalf("over-capacity request: %d %s", resp.StatusCode, body)
	}
	req.CameraIDs = []string{"cam3"}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", token, req, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("failed multi-camera request kept a session slot: %d %s", resp.StatusCode, body)
	}
}

// blockingStore holds every media download until released, so an export job
// stays running while the next one is admitted.
type blockingStore struct {
	storage.MediaStore
	release chan struct{}
}

func (b blockingStore) Open(ctx context.Context, key, byteRange string) (storage.Stream, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return storage.Stream{}, ctx.Err()
	}
	return b.MediaStore.Open(ctx, key, byteRange)
}

func TestConcurrentExportsReserveDiskSpace(t *testing.T) {
	f := newFixture(t)
	body := bytes.Repeat([]byte{0x47}, 1<<20)
	record := f.segment(t, 1, time.Now().Add(-time.Hour), 4*time.Second, body, "")
	reserve := uint64(3 * len(body))
	f.app.cfg.Export.MinFreeBytes = 128 << 20
	// Room for one job's reservation above the floor, but not two.
	original := diskAvailable
	diskAvailable = func(string) (uint64, error) {
		return uint64(f.app.cfg.Export.MinFreeBytes) + reserve + reserve/2, nil
	}
	t.Cleanup(func() { diskAvailable = original })
	release := make(chan struct{})
	f.app.store = blockingStore{f.app.store, release}
	token := f.token(t, allPermissions())
	req := exportRequest{CenterID: "c1", CameraID: "cam1", Start: record.Start, End: record.End()}
	resp, raw := f.request(t, "POST", "/v1/exports", token, req, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("first export: %d %s", resp.StatusCode, raw)
	}
	var first struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	resp, raw = f.request(t, "POST", "/v1/exports", token, req, nil)
	if resp.StatusCode != http.StatusInsufficientStorage || !bytes.Contains(raw, []byte("export_storage_full")) {
		t.Fatalf("second export was admitted into reserved space: %d %s", resp.StatusCode, raw)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, raw = f.request(t, "GET", "/v1/exports/"+first.ID, token, nil, nil)
		if !bytes.Contains(raw, []byte(`"processing"`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first export never finished: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.reservations(); n != 0 {
		t.Fatalf("finished export kept %d reservations", n)
	}
	resp, raw = f.request(t, "POST", "/v1/exports", token, req, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("reservation was not released: %d %s", resp.StatusCode, raw)
	}
}

func TestExportRejectsMissingRecordingBeforeStarting(t *testing.T) {
	f := newFixture(t)
	token := f.token(t, allPermissions())
	start := time.Now().Add(-time.Hour)
	req := exportRequest{CenterID: "c1", CameraID: "cam1", Start: start, End: start.Add(time.Minute)}
	resp, raw := f.request(t, "POST", "/v1/exports", token, req, nil)
	if resp.StatusCode != 404 || !bytes.Contains(raw, []byte("recording_not_found")) {
		t.Fatalf("empty export: %d %s", resp.StatusCode, raw)
	}
	if len(f.app.exportSlots) != 0 {
		t.Fatal("rejected export kept a worker slot")
	}
}

func TestLoginLimitsAccountsAndClientsSeparately(t *testing.T) {
	f := newFixture(t)
	proxies, err := parseTrustedProxies([]string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	f.app.auth.proxies = proxies
	login := func(client, username, password string) int {
		t.Helper()
		resp, _ := f.request(t, "POST", "/v1/login", "", map[string]string{"username": username, "password": password},
			map[string]string{"X-Forwarded-For": client})
		return resp.StatusCode
	}
	// A noisy client behind the shared proxy exhausts only its own budget.
	for n := 0; n < loginClientLimit; n++ {
		resp, _ := f.request(t, "POST", "/v1/login", "", "malformed", map[string]string{"X-Forwarded-For": "203.0.113.1"})
		if resp.StatusCode != 400 {
			t.Fatalf("attempt %d: %d", n, resp.StatusCode)
		}
	}
	if status := login("203.0.113.1", "admin", "test-only-password"); status != 429 {
		t.Fatalf("client limit not applied: %d", status)
	}
	if status := login("203.0.113.2", "admin", "test-only-password"); status != 200 {
		t.Fatalf("another user behind the proxy was blocked: %d", status)
	}
	// Guessing one account from many addresses is still bounded.
	for n := 1; n <= loginAccountLimit; n++ {
		if status := login(netip.AddrFrom4([4]byte{198, 51, 100, byte(n)}).String(), "admin", "wrong-password"); status != 401 {
			t.Fatalf("guess %d: %d", n, status)
		}
	}
	if status := login("198.51.100.200", "admin", "test-only-password"); status != 429 {
		t.Fatalf("account limit not applied across clients: %d", status)
	}
	if status := login("198.51.100.200", "viewer", "wrong-password"); status != 401 {
		t.Fatalf("account limit leaked to another account: %d", status)
	}
}

func TestLoginClientKeyTrustsForwardedForOnlyFromProxies(t *testing.T) {
	proxies, err := parseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	a := &Auth{proxies: proxies}
	for _, tc := range []struct{ remote, forwarded, want string }{
		{"192.0.2.7:4000", "203.0.113.9", "192.0.2.7"},                 // untrusted peer cannot choose a key
		{"10.1.2.3:4000", "203.0.113.9", "203.0.113.9"},                // trusted proxy
		{"10.1.2.3:4000", "198.51.100.1, 203.0.113.9", "203.0.113.9"},  // spoofed left-most hop ignored
		{"10.1.2.3:4000", "203.0.113.9, 10.9.9.9", "203.0.113.9"},      // chained trusted proxies
		{"10.1.2.3:4000", "", "10.1.2.3"},                              // no header
		{"10.1.2.3:4000", "not-an-ip", "10.1.2.3"},                     // malformed header
		{"[2001:db8:1:2:3:4:5:6]:4000", "", "2001:db8:1:2::/64"},       // IPv6 grouped by /64
		{"10.1.2.3:4000", "::ffff:203.0.113.9", "203.0.113.9"},         // IPv4-mapped
		{"10.1.2.3:4000", "2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"}, // forwarded IPv6
	} {
		r, _ := http.NewRequest("POST", "/v1/login", nil)
		r.RemoteAddr = tc.remote
		if tc.forwarded != "" {
			r.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		if got := a.ClientKey(r); got != tc.want {
			t.Errorf("%s via %s %q: got %s, want %s", tc.remote, tc.forwarded, tc.forwarded, got, tc.want)
		}
	}
}

func TestPlaybackConfigValidatesProxiesAndFreeSpace(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "configs", "solution-playback.json"))
	if err != nil {
		t.Fatalf("example configuration is invalid: %v", err)
	}
	if cfg.Export.MinFreeBytes != 1<<30 || len(cfg.Auth.TrustedProxies) != 0 {
		t.Fatalf("example defaults changed: %+v %v", cfg.Export, cfg.Auth.TrustedProxies)
	}
	for name, change := range map[string]func(*Config){
		"bare address":  func(c *Config) { c.Auth.TrustedProxies = []string{"10.0.0.1"} },
		"hostname":      func(c *Config) { c.Auth.TrustedProxies = []string{"proxy.internal/32"} },
		"free too low":  func(c *Config) { c.Export.MinFreeBytes = 1 << 20 },
		"free disabled": func(c *Config) { c.Export.MinFreeBytes = 0 },
	} {
		bad := cfg
		change(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	good := cfg
	good.Auth.TrustedProxies = []string{"10.0.0.0/8", "fd00::/8"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) reservations() int {
	f.app.jobsMu.Lock()
	defer f.app.jobsMu.Unlock()
	return len(f.app.exportSpace)
}

func (f *fixture) waitReservations(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.reservations() != want {
		if time.Now().After(deadline) {
			t.Fatalf("reservations: got %d, want %d", f.reservations(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExportReservationIncludesInitializationFiles(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Add(-time.Hour)
	var last time.Time
	for n := range 4 {
		// Two segments per distinct initialization file.
		initURI := fmt.Sprintf("%s/init-%064d.mp4", base.UTC().Format("2006/01/02"), n/2)
		record := f.segment(t, uint64(n+1), base.Add(time.Duration(n)*4*time.Second), 4*time.Second, []byte("fragment"), initURI)
		last = record.End()
	}
	plan, err := f.app.planExport(context.Background(), exportRequest{CenterID: "c1", CameraID: "cam1", Start: base, End: last})
	if err != nil {
		t.Fatal(err)
	}
	inputs := int64(4*len("fragment") + 2*maxInitBytes)
	if want := 3*inputs + (4+2+2)*exportFileOverhead; plan.reserve != want {
		t.Fatalf("reserve %d, want %d", plan.reserve, want)
	}
}

func TestExportReservationCountsOnlyUnwrittenBytes(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Export.MinFreeBytes = 128 << 20
	var available uint64
	original := diskAvailable
	diskAvailable = func(string) (uint64, error) { return available, nil }
	t.Cleanup(func() { diskAvailable = original })
	const reserve = 300 << 20
	available = uint64(f.app.cfg.Export.MinFreeBytes) + reserve
	f.app.jobsMu.Lock()
	defer f.app.jobsMu.Unlock()
	running, err := f.app.reserveExportSpace(reserve)
	if err != nil {
		t.Fatal(err)
	}
	// The running job copied a third of its reservation, so Statfs now
	// reports that much less; the same job must still fit alongside it.
	running.written.Store(reserve / 3)
	available = uint64(f.app.cfg.Export.MinFreeBytes) + reserve + reserve - reserve/3 - reserve/3
	if _, err := f.app.reserveExportSpace(reserve); !errors.Is(err, errExportSpace) {
		t.Fatalf("over-committed volume admitted: %v", err)
	}
	available += reserve / 3
	if _, err := f.app.reserveExportSpace(reserve); err != nil {
		t.Fatalf("written bytes were counted twice: %v", err)
	}
}

func TestExportReservationIsReleasedOnCancelAndTimeout(t *testing.T) {
	f := newFixture(t)
	record := f.segment(t, 1, time.Now().Add(-time.Hour), 4*time.Second, []byte("media"), "")
	f.app.store = blockingStore{f.app.store, make(chan struct{})}
	token := f.token(t, allPermissions())
	req := exportRequest{CenterID: "c1", CameraID: "cam1", Start: record.Start, End: record.End()}
	resp, raw := f.request(t, "POST", "/v1/exports", token, req, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("export: %d %s", resp.StatusCode, raw)
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatal(err)
	}
	f.waitReservations(t, 1)
	if resp, _ := f.request(t, "DELETE", "/v1/exports/"+started.ID, token, nil, nil); resp.StatusCode != 200 {
		t.Fatal("cancel failed")
	}
	f.waitReservations(t, 0)
	f.app.cfg.Export.Timeout = config.Duration{Duration: 50 * time.Millisecond}
	resp, raw = f.request(t, "POST", "/v1/exports", token, req, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("export: %d %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatal(err)
	}
	f.waitReservations(t, 0)
	if _, raw = f.request(t, "GET", "/v1/exports/"+started.ID, token, nil, nil); !bytes.Contains(raw, []byte("export_timeout")) {
		t.Fatalf("export did not time out: %s", raw)
	}
}

func TestLoginAccountLimitIgnoresUnknownNamesAndSuccesses(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	// An attacker fills the client table with fresh addresses.
	for n := range loginTrackedKeys {
		f.app.loginClients.allow(fmt.Sprintf("flood-%d", n), now)
	}
	login := func(username, password string) int {
		t.Helper()
		resp, _ := f.request(t, "POST", "/v1/login", "", map[string]string{"username": username, "password": password}, nil)
		return resp.StatusCode
	}
	if status := login("admin", "test-only-password"); status != 200 {
		t.Fatalf("full client table refused a new client: %d", status)
	}
	// Unknown names are bounded by the client limit only.
	if status := login("no-such-user", strings.Repeat("x", 2000)); status != 401 {
		t.Fatalf("unknown account: %d", status)
	}
	f.app.loginAccounts.mu.Lock()
	tracked := len(f.app.loginAccounts.entries)
	f.app.loginAccounts.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("unknown names or successes entered the account table: %d", tracked)
	}
	for n := 0; n < loginAccountLimit; n++ {
		if status := login("admin", "test-only-password"); status != 200 {
			t.Fatalf("successful sign-in %d was counted: %d", n, status)
		}
	}
	f.app.loginClients.mu.Lock()
	overflow := f.app.loginClients.entries[loginOverflowKey].Count
	f.app.loginClients.mu.Unlock()
	if overflow != 2+loginAccountLimit {
		t.Fatalf("overflow bucket counted %d attempts", overflow)
	}
}

func TestLoginOverflowBucketHasItsOwnLimit(t *testing.T) {
	l := newLoginLimiter(1, 2)
	now := time.Now()
	for n := range loginTrackedKeys {
		if !l.allow(fmt.Sprintf("k%d", n), now) {
			t.Fatal("first attempt refused")
		}
	}
	if !l.allow("new-1", now) || !l.allow("new-2", now) || l.allow("new-3", now) {
		t.Fatal("overflow bucket limit not applied")
	}
	if l.allow("k1", now) {
		t.Fatal("tracked key escaped its limit")
	}
	if !l.allow("k1", now.Add(loginWindow)) {
		t.Fatal("expired entry was not reset")
	}
	if !l.allow("new-4", now.Add(loginWindow)) {
		t.Fatal("new key refused after the window")
	}
	if _, tracked := l.entries["new-4"]; !tracked {
		t.Fatal("expired entries were not reclaimed for a new key")
	}
}
