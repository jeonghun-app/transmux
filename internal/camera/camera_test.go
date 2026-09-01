package camera

import (
	"strings"
	"testing"

	"github.com/jeonghun-app/transmux/internal/config"
)

// staticPair builds a two-entry roster for convert.
func staticPair(centerA, cameraA, centerB, cameraB string) []config.StaticCamera {
	return []config.StaticCamera{
		{CenterID: centerA, CameraID: cameraA, RTSPURL: "rtsp://h/a"},
		{CenterID: centerB, CameraID: cameraB, RTSPURL: "rtsp://h/b"},
	}
}

func TestSafeURLRedactsUserinfo(t *testing.T) {
	c := Camera{RTSPURL: "rtsp://admin:s3cret@10.0.0.5:554/Streaming/Channels/101"}
	got := c.SafeURL()
	if strings.Contains(got, "s3cret") || strings.Contains(got, "admin") {
		t.Fatalf("SafeURL leaked credentials: %q", got)
	}
	if !strings.Contains(got, "***@") {
		t.Errorf("SafeURL = %q, want a masked userinfo", got)
	}
	if !strings.Contains(got, "10.0.0.5:554") {
		t.Errorf("SafeURL = %q, host should survive redaction", got)
	}
}

// TestSafeURLRedactsQueryCredentials covers the case userinfo masking misses.
// Cameras and NVRs commonly take credentials or a signed token as query
// parameters, and SafeURL is what the unauthenticated /channels endpoint
// serves.
func TestSafeURLRedactsQueryCredentials(t *testing.T) {
	c := Camera{RTSPURL: "rtsp://cam.example:554/live?user=admin&password=hunter2&token=abc123"}
	got := c.SafeURL()
	for _, secret := range []string{"hunter2", "abc123", "password="} {
		if strings.Contains(got, secret) {
			t.Errorf("SafeURL leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("SafeURL = %q, want the query marked as redacted", got)
	}
}

func TestSafeURLLeavesACleanURLIntact(t *testing.T) {
	c := Camera{RTSPURL: "rtsp://cam.example:554/live"}
	if got := c.SafeURL(); got != "rtsp://cam.example:554/live" {
		t.Errorf("SafeURL = %q, want it unchanged", got)
	}
}

// TestStderrRedactorRemovesCredentials covers ffmpeg's own error messages,
// which echo the input URL.
func TestStderrRedactorRemovesCredentials(t *testing.T) {
	c := Camera{RTSPURL: "rtsp://admin:s3cretpass@10.0.0.5:554/live"}
	redact := c.StderrRedactor()
	line := "[rtsp @ 0x7f] Could not connect to rtsp://admin:s3cretpass@10.0.0.5:554/live: timeout"
	got := redact(line)
	if strings.Contains(got, "s3cretpass") {
		t.Fatalf("redactor left the password in place: %q", got)
	}
	if !strings.Contains(got, "10.0.0.5:554") {
		t.Errorf("redactor should keep the diagnostic content: %q", got)
	}
}

func TestStderrRedactorHandlesABarePassword(t *testing.T) {
	c := Camera{RTSPURL: "rtsp://admin:longenoughpass@host/live"}
	redact := c.StderrRedactor()
	if got := redact("auth failed for longenoughpass"); strings.Contains(got, "longenoughpass") {
		t.Errorf("bare password should be redacted too: %q", got)
	}
}

// A very short password is deliberately left alone: substituting a two-letter
// string everywhere would corrupt unrelated log text.
func TestStderrRedactorIgnoresVeryShortPasswords(t *testing.T) {
	c := Camera{RTSPURL: "rtsp://admin:ab@host/live"}
	redact := c.StderrRedactor()
	if got := redact("stream is stable"); got != "stream is stable" {
		t.Errorf("unrelated text was rewritten: %q", got)
	}
}

func TestValidateRejectsUnsafeIdentifiers(t *testing.T) {
	cases := []Camera{
		{CenterID: "../etc", CameraID: "cam", RTSPURL: "rtsp://h/s"},
		{CenterID: "c", CameraID: "cam;rm -rf /", RTSPURL: "rtsp://h/s"},
		{CenterID: "", CameraID: "cam", RTSPURL: "rtsp://h/s"},
		{CenterID: "c", CameraID: "cam", RTSPURL: "http://h/s"},
		{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp:///nohost"},
	}
	for _, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted %+v", c)
		}
	}
	ok := Camera{CenterID: "center-01", CameraID: "cam_1.a", RTSPURL: "rtsp://h:554/s"}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate rejected a legitimate camera: %v", err)
	}
}

func TestConvertRejectsDuplicates(t *testing.T) {
	// Two workers for one camera would both overwrite the same manifest.
	_, err := convert(staticPair("c1", "cam1", "c1", "cam1"))
	if err == nil {
		t.Fatal("expected a duplicate camera to be rejected")
	}
	cams, err := convert(staticPair("c1", "cam1", "c1", "cam2"))
	if err != nil {
		t.Fatalf("distinct cameras must be accepted: %v", err)
	}
	if len(cams) != 2 {
		t.Errorf("got %d cameras, want 2", len(cams))
	}
}
