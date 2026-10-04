package playback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/recording"
)

// Adversarial and boundary tests for issue #7: session capacity atomicity,
// export disk reservation under concurrency, and the login limits.

func (f *fixture) trustLoopbackProxy(t *testing.T) {
	t.Helper()
	proxies, err := parseTrustedProxies([]string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	f.app.auth.proxies = proxies
}

func (f *fixture) loginFrom(t *testing.T, client, username, password string) int {
	t.Helper()
	resp, _ := f.request(t, "POST", "/v1/login", "", map[string]string{"username": username, "password": password},
		map[string]string{"X-Forwarded-For": client})
	return resp.StatusCode
}

// post is safe to call from goroutines other than the test's: it reports
// transport failures as status 0 instead of calling t.Fatal.
func (f *fixture) post(path, token string, body any, headers map[string]string) (int, []byte) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil
	}
	req, err := http.NewRequest("POST", f.http.URL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := f.http.Client().Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (l *loginLimiter) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[key].Count
}

// Fifty concurrent wrong-password guesses against one account, each from its
// own address, must never verify more than the account limit, and every
// recorded failure must correspond to a 401 the attacker actually received.
func TestQALoginConcurrentGuessesNeverExceedAccountLimit(t *testing.T) {
	f := newFixture(t)
	f.trustLoopbackProxy(t)
	const goroutines = 50
	// Let every attempt verify at once, so the account limit alone has to
	// hold; the two production login slots would otherwise hide a
	// check-then-count race.
	f.app.loginSlots = make(chan struct{}, goroutines)
	statuses := make([]int, goroutines)
	var start, done sync.WaitGroup
	start.Add(1)
	for n := range goroutines {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			client := netip.AddrFrom4([4]byte{198, 51, 100, byte(n + 1)}).String()
			statuses[n], _ = f.post("/v1/login", "", map[string]string{"username": "admin", "password": "wrong-password"},
				map[string]string{"X-Forwarded-For": client})
		}()
	}
	start.Done()
	done.Wait()
	counts := map[int]int{}
	for _, status := range statuses {
		counts[status]++
	}
	if counts[401]+counts[429] != goroutines {
		t.Fatalf("unexpected statuses: %v", counts)
	}
	if counts[401] > loginAccountLimit {
		t.Fatalf("%d concurrent guesses were verified, limit %d", counts[401], loginAccountLimit)
	}
	if got := f.app.loginAccounts.count("admin"); got != counts[401] {
		t.Fatalf("account counter %d does not match %d verified failures", got, counts[401])
	}
	// Keep guessing serially until the limit is reached; the total number of
	// verified guesses must land exactly on the limit, never above.
	verified := counts[401]
	for n := 0; n < 3*loginAccountLimit; n++ {
		status := f.loginFrom(t, fmt.Sprintf("203.0.113.%d", n+1), "admin", "wrong-password")
		if status == 401 {
			verified++
		}
	}
	if verified != loginAccountLimit {
		t.Fatalf("verified %d guesses in one window, want exactly %d", verified, loginAccountLimit)
	}
	if status := f.loginFrom(t, "203.0.113.250", "admin", "test-only-password"); status != 429 {
		t.Fatalf("locked account accepted a sign-in in the same window: %d", status)
	}
}

// The same strictness must hold directly on the limiter, where there is no
// login slot to serialise callers.
func TestQALoginLimiterReserveIsExactUnderContention(t *testing.T) {
	l := newLoginLimiter(loginAccountLimit, loginAccountLimit)
	now := time.Now()
	var granted sync.WaitGroup
	var mu sync.Mutex
	tickets := []loginTicket{}
	for range 50 {
		granted.Add(1)
		go func() {
			defer granted.Done()
			if ticket, ok := l.reserve("admin", now); ok {
				mu.Lock()
				tickets = append(tickets, ticket)
				mu.Unlock()
			}
		}()
	}
	granted.Wait()
	if len(tickets) != loginAccountLimit || l.count("admin") != loginAccountLimit {
		t.Fatalf("granted %d, counted %d, want %d", len(tickets), l.count("admin"), loginAccountLimit)
	}
	// Refunding every ticket concurrently returns the key to empty.
	var refunds sync.WaitGroup
	for _, ticket := range tickets {
		refunds.Add(1)
		go func() { defer refunds.Done(); l.refund(ticket) }()
	}
	refunds.Wait()
	if _, exists := l.entries["admin"]; exists {
		t.Fatalf("refunded key still tracked: %+v", l.entries["admin"])
	}
}

// Concurrent correct sign-ins mixed with guesses: successes and slot refusals
// are refunded, so the counter equals the verified failures.
func TestQALoginMixedConcurrentAttemptsCountOnlyFailures(t *testing.T) {
	f := newFixture(t)
	f.trustLoopbackProxy(t)
	const goroutines = 50
	statuses := make([]int, goroutines)
	var start, done sync.WaitGroup
	start.Add(1)
	for n := range goroutines {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			password := "wrong-password"
			if n%2 == 0 {
				password = "test-only-password"
			}
			statuses[n], _ = f.post("/v1/login", "", map[string]string{"username": "admin", "password": password},
				map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", n+1)})
		}()
	}
	start.Done()
	done.Wait()
	failures := 0
	for _, status := range statuses {
		switch status {
		case 401:
			failures++
		case 200, 429:
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if failures > loginAccountLimit {
		t.Fatalf("%d failures verified, limit %d", failures, loginAccountLimit)
	}
	if got := f.app.loginAccounts.count("admin"); got != failures {
		t.Fatalf("account counter %d, verified failures %d", got, failures)
	}
}

func TestQALoginWindowBoundaries(t *testing.T) {
	l := newLoginLimiter(loginAccountLimit, loginAccountLimit)
	start := time.Now()
	for n := range loginAccountLimit {
		// Spread inside the window: a fixed window starts at the first try.
		if !l.allow("admin", start.Add(time.Duration(n)*time.Second)) {
			t.Fatalf("attempt %d refused", n)
		}
	}
	if l.allow("admin", start.Add(loginWindow-time.Nanosecond)) {
		t.Fatal("limit lifted before the window ended")
	}
	if l.count("admin") != loginAccountLimit {
		t.Fatalf("refused attempt changed the count: %d", l.count("admin"))
	}
	if !l.allow("admin", start.Add(loginWindow)) {
		t.Fatal("limit still applied once the window ended")
	}
	if l.count("admin") != 1 || !l.entries["admin"].At.Equal(start.Add(loginWindow)) {
		t.Fatalf("new window not started at the first attempt: %+v", l.entries["admin"])
	}
	// A refused attempt at the very end of a window does not extend it.
	for range loginAccountLimit - 1 {
		l.allow("admin", start.Add(loginWindow))
	}
	for range 5 {
		l.allow("admin", start.Add(2*loginWindow-time.Nanosecond))
	}
	if !l.allow("admin", start.Add(2*loginWindow)) {
		t.Fatal("refused attempts extended the lockout")
	}
	// A ticket from a window that expired is a no-op even before any new
	// attempt re-creates the key.
	m := newLoginLimiter(2, 2)
	old, _ := m.reserve("k", start)
	m.refund(loginTicket{key: "k", at: start.Add(time.Nanosecond)})
	if m.count("k") != 1 {
		t.Fatal("refund with a mismatched window start was applied")
	}
	m.refund(old)
	m.refund(old)
	if _, exists := m.entries["k"]; exists || m.count("k") != 0 {
		t.Fatal("double refund went below zero or kept the key")
	}
}

func TestQALoginClientLimitAndOverflowBoundaries(t *testing.T) {
	l := newLoginLimiter(loginClientLimit, loginOverflowLimit)
	now := time.Now()
	for n := range loginClientLimit {
		if !l.allow("client", now) {
			t.Fatalf("client attempt %d refused", n)
		}
	}
	if l.allow("client", now) {
		t.Fatal("client limit exceeded")
	}
	for n := range loginTrackedKeys - 1 {
		l.allow(fmt.Sprintf("k%d", n), now)
	}
	if len(l.entries) != loginTrackedKeys {
		t.Fatalf("table holds %d keys", len(l.entries))
	}
	for n := range loginOverflowLimit {
		if !l.allow(fmt.Sprintf("new-%d", n), now) {
			t.Fatalf("overflow attempt %d refused", n)
		}
	}
	if l.allow("new-last", now) {
		t.Fatal("overflow bucket exceeded its limit")
	}
	if len(l.entries) != loginTrackedKeys+1 {
		t.Fatalf("overflow grew the table: %d", len(l.entries))
	}
	if status := l.status(); status.Used != loginOverflowLimit+1 || status.Refused != 1 {
		t.Fatalf("overflow status: %+v", status)
	}
}

func TestQAAdminStatusReportsLoginOverflow(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	for n := range loginTrackedKeys {
		f.app.loginClients.allow(fmt.Sprintf("flood-%d", n), now)
	}
	if status := f.loginFrom(t, "", "admin", "wrong-password"); status != 401 {
		t.Fatalf("overflow login: %d", status)
	}
	resp, raw := f.request(t, "GET", "/v1/admin/status", f.token(t, allPermissions()), nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %s", resp.StatusCode, raw)
	}
	var body struct {
		Login map[string]uint64 `json:"login"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Login["overflow_attempts"] != 1 || body.Login["overflow_refused"] != 0 {
		t.Fatalf("login status: %s", raw)
	}
}

func TestQAClientKeyForwardedForVariants(t *testing.T) {
	proxies, err := parseTrustedProxies([]string{"10.0.0.0/8", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	trusted := &Auth{proxies: proxies}
	untrusting := &Auth{}
	for _, tc := range []struct {
		name    string
		auth    *Auth
		remote  string
		headers []string
		want    string
	}{
		{"multiple headers use the right-most line", trusted, "10.1.2.3:4000",
			[]string{"198.51.100.1", "203.0.113.9"}, "203.0.113.9"},
		{"multiple headers with spoofed trusted hop", trusted, "10.1.2.3:4000",
			[]string{"198.51.100.1, 10.5.5.5", "203.0.113.9"}, "203.0.113.9"},
		{"surrounding whitespace and tabs", trusted, "10.1.2.3:4000",
			[]string{" 198.51.100.1 ,\t203.0.113.9\t"}, "203.0.113.9"},
		{"port in the right-most hop is stripped, not trusted further", trusted, "10.1.2.3:4000",
			[]string{"198.51.100.1, 203.0.113.9:5555"}, "203.0.113.9"},
		{"bracketed IPv6 with port is stripped and grouped by /64", trusted, "10.1.2.3:4000",
			[]string{"198.51.100.1, [2001:db8::1]:443"}, "2001:db8::/64"},
		{"empty trailing element does not reach a spoofed hop", trusted, "10.1.2.3:4000",
			[]string{"198.51.100.1,"}, "10.1.2.3"},
		{"IPv6 zone in forwarded hop is dropped", trusted, "10.1.2.3:4000",
			[]string{"2001:db8:1:2::1%eth0"}, "2001:db8:1:2::/64"},
		{"IPv6 zone in remote address is dropped", untrusting, "[fe80::1%eth0]:4000",
			nil, "fe80::/64"},
		{"IPv4-mapped trusted proxy", trusted, "[::ffff:10.1.2.3]:4000",
			[]string{"203.0.113.9"}, "203.0.113.9"},
		{"trusted IPv6 proxy", trusted, "[fd00::7]:4000",
			[]string{"203.0.113.9"}, "203.0.113.9"},
		{"empty trusted list ignores the header", untrusting, "127.0.0.1:4000",
			[]string{"203.0.113.9"}, "127.0.0.1"},
		{"empty trusted list ignores a private hop", untrusting, "10.1.2.3:4000",
			[]string{"203.0.113.9"}, "10.1.2.3"},
		{"remote address without a port", trusted, "203.0.113.4",
			[]string{"198.51.100.1"}, "203.0.113.4"},
		{"all hops trusted resolves to the left-most proxy", trusted, "10.1.2.3:4000",
			[]string{"10.9.9.9, 10.8.8.8"}, "10.9.9.9"},
	} {
		r, _ := http.NewRequest("POST", "/v1/login", nil)
		r.RemoteAddr = tc.remote
		for _, value := range tc.headers {
			r.Header.Add("X-Forwarded-For", value)
		}
		if got := tc.auth.ClientKey(r); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	// Zone and interface-identifier variations of one /64 share a key, so
	// they cannot multiply an attacker's client budget.
	keys := map[string]bool{}
	for _, hop := range []string{"2001:db8:1:2::1", "2001:db8:1:2::1%a", "2001:db8:1:2::1%b", "2001:db8:1:2:ffff::9"} {
		r, _ := http.NewRequest("POST", "/v1/login", nil)
		r.RemoteAddr = "10.1.2.3:4000"
		r.Header.Set("X-Forwarded-For", hop)
		keys[trusted.ClientKey(r)] = true
	}
	if len(keys) != 1 {
		t.Fatalf("one /64 produced %d keys: %v", len(keys), keys)
	}
}

// A client that is not a configured proxy cannot escape its own budget by
// rotating X-Forwarded-For values.
func TestQAUntrustedClientCannotRotateForwardedFor(t *testing.T) {
	f := newFixture(t)
	for n := range loginClientLimit {
		if status := f.loginFrom(t, fmt.Sprintf("198.51.100.%d", n+1), "admin", "test-only-password"); status != 200 {
			t.Fatalf("attempt %d: %d", n, status)
		}
	}
	if status := f.loginFrom(t, "203.0.113.77", "admin", "test-only-password"); status != 429 {
		t.Fatalf("rotated X-Forwarded-For escaped the client limit: %d", status)
	}
}

// N exports submitted at once: the admitted reservations never exceed the
// free space above the floor, and the rest are refused with 507.
func TestQAConcurrentExportsReservationTotalStaysWithinFreeSpace(t *testing.T) {
	f := newFixture(t)
	body := bytes.Repeat([]byte{0x47}, 1<<20)
	record := f.segment(t, 1, time.Now().Add(-time.Hour), 4*time.Second, body, "")
	f.app.cfg.Export.Workers = 16
	f.app.exportSlots = make(chan struct{}, 16)
	f.app.cfg.Export.MinFreeBytes = 128 << 20
	plan, err := f.app.planExport(t.Context(), exportRequest{CenterID: "c1", CameraID: "cam1", Start: record.Start, End: record.End()})
	if err != nil {
		t.Fatal(err)
	}
	room := uint64(3*plan.reserve + plan.reserve/2)
	available := uint64(f.app.cfg.Export.MinFreeBytes) + room
	original := diskAvailable
	diskAvailable = func(string) (uint64, error) { return available, nil }
	t.Cleanup(func() { diskAvailable = original })
	release := make(chan struct{})
	f.app.store = blockingStore{f.app.store, release}
	token := f.token(t, allPermissions())
	req := exportRequest{CenterID: "c1", CameraID: "cam1", Start: record.Start, End: record.End()}
	const exports = 12
	statuses := make([]int, exports)
	var start, done sync.WaitGroup
	start.Add(1)
	for n := range exports {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			status, raw := f.post("/v1/exports", token, req, nil)
			statuses[n] = status
			if status == http.StatusInsufficientStorage && !bytes.Contains(raw, []byte("export_storage_full")) {
				t.Errorf("507 without export_storage_full: %s", raw)
			}
		}()
	}
	start.Done()
	done.Wait()
	admitted, refused := 0, 0
	for _, status := range statuses {
		switch status {
		case 202:
			admitted++
		case http.StatusInsufficientStorage:
			refused++
		default:
			t.Errorf("unexpected status %d", status)
		}
	}
	f.app.jobsMu.Lock()
	var total uint64
	for space := range f.app.exportSpace {
		total += uint64(space.outstanding())
	}
	held := len(f.app.exportSpace)
	f.app.jobsMu.Unlock()
	if total > room {
		t.Fatalf("reserved %d bytes, only %d above the floor", total, room)
	}
	if admitted != 3 || refused != exports-3 || held != admitted {
		t.Fatalf("admitted %d, refused %d, held %d reservations", admitted, refused, held)
	}
	close(release)
	f.waitReservations(t, 0)
}

// A request that exactly fits is admitted; one byte less is refused.
func TestQAExportReservationExactBoundary(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Export.MinFreeBytes = 128 << 20
	const reserve = 10 << 20
	var available uint64
	original := diskAvailable
	diskAvailable = func(string) (uint64, error) { return available, nil }
	t.Cleanup(func() { diskAvailable = original })
	f.app.jobsMu.Lock()
	defer f.app.jobsMu.Unlock()
	available = uint64(f.app.cfg.Export.MinFreeBytes) + reserve - 1
	if _, err := f.app.reserveExportSpace(reserve); err == nil {
		t.Fatal("reservation one byte short of the floor was admitted")
	}
	if len(f.app.exportSpace) != 0 {
		t.Fatal("refused reservation was recorded")
	}
	available++
	if _, err := f.app.reserveExportSpace(reserve); err != nil {
		t.Fatalf("exactly fitting reservation refused: %v", err)
	}
}

func (f *fixture) holdSnapshots(t *testing.T, n int) {
	t.Helper()
	base := time.Now().Add(-2 * time.Hour)
	segments := []hls.PublishedSegment{{URI: base.UTC().Format("2006/01/02") + "/held.ts", Duration: time.Second, ProgramDateTime: base}}
	entries := make([]recording.SnapshotEntry, n)
	for i := range entries {
		entries[i] = recording.SnapshotEntry{ID: fmt.Sprintf("held-%d", i), Expires: time.Now().Add(time.Hour), Segments: segments}
	}
	if err := f.index.Snapshots(entries, n); err != nil {
		t.Fatal(err)
	}
}

// The literal acceptance scenario: 1999 of 2000 slots are held.
func TestQASessionCapacityBoundaryAt2000(t *testing.T) {
	for _, tc := range []struct {
		held    int
		cameras []string
		want    int
		// slot that remains free afterwards: a one-camera request decides it.
		freeAfter bool
	}{
		{1999, []string{"cam1", "cam3"}, 429, true},
		{1998, []string{"cam1", "cam3"}, 201, false},
		{2000, []string{"cam1"}, 429, false},
	} {
		t.Run(fmt.Sprintf("held-%d-cameras-%d", tc.held, len(tc.cameras)), func(t *testing.T) {
			f := newFixture(t)
			token := f.token(t, allPermissions())
			resp, _ := f.request(t, "GET", "/v1/admin/cameras", token, nil, nil)
			camera := config.StaticCamera{CenterID: "c1", CameraID: "cam3", RTSPURL: "rtsp://camera3/live", Name: "Dock"}
			if resp, body := f.request(t, "POST", "/v1/admin/cameras", token, camera, map[string]string{"If-Match": resp.Header.Get("ETag")}); resp.StatusCode != 201 {
				t.Fatalf("camera create: %d %s", resp.StatusCode, body)
			}
			base := time.Now().Add(-time.Hour).Truncate(time.Second)
			f.cameraSegment(t, "cam1", 1, base, 4*time.Second, []byte("one"), "")
			f.cameraSegment(t, "cam3", 1, base, 4*time.Second, []byte("three"), "")
			f.app.cfg.MaxSessions = 2000
			f.holdSnapshots(t, tc.held)
			req := sessionRequest{CenterID: "c1", CameraIDs: tc.cameras, Mode: "recording", Start: base, End: base.Add(4 * time.Second)}
			resp, body := f.request(t, "POST", "/v1/playback-sessions", token, req, nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("request: %d %s", resp.StatusCode, body)
			}
			if tc.want == 201 {
				var created struct {
					Streams []json.RawMessage `json:"streams"`
				}
				if err := json.Unmarshal(body, &created); err != nil || len(created.Streams) != len(tc.cameras) {
					t.Fatalf("streams: %v %s", err, body)
				}
			}
			req.CameraIDs = []string{"cam3"}
			resp, body = f.request(t, "POST", "/v1/playback-sessions", token, req, nil)
			if got := resp.StatusCode == 201; got != tc.freeAfter {
				t.Fatalf("follow-up single-camera request: %d %s", resp.StatusCode, body)
			}
			if tc.freeAfter {
				if resp, body = f.request(t, "POST", "/v1/playback-sessions", token, req, nil); resp.StatusCode != 429 {
					t.Fatalf("capacity exceeded after the last slot: %d %s", resp.StatusCode, body)
				}
			}
		})
	}
}
