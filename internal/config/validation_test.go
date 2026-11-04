package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateRejectsInvalidCameraRoster(t *testing.T) {
	for name, entries := range map[string][]StaticCamera{
		"unsafe identifier": {{CenterID: "../c", CameraID: "cam", RTSPURL: "rtsp://host/live"}},
		"invalid URL":       {{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://user:secret@host/%xx"}},
		"empty hostname":    {{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://:554/live"}},
		"wrong scheme":      {{CenterID: "c", CameraID: "cam", RTSPURL: "file:///etc/passwd"}},
		"unknown codec":     {{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://host/live", VideoCodec: "unknown"}},
		"HEVC without fMP4": {{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://host/live", VideoCodec: "hevc"}},
		"duplicate": {
			{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://host/a"},
			{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://host/b"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			c.Cameras.Static = entries
			if err := c.Validate(); err == nil {
				t.Fatal("invalid roster passed configuration validation")
			} else if strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation leaked URL credentials: %v", err)
			}
		})
	}
}

func TestValidateRejectsInvalidRemoteEndpoints(t *testing.T) {
	for _, raw := range []string{"relative/path", "ftp://host/api", "https://:443/api", "https://host/%xx"} {
		t.Run(raw, func(t *testing.T) {
			c := valid()
			c.Cameras.Provider, c.Cameras.URL = "http", raw
			if err := c.Validate(); err == nil {
				t.Error("invalid camera provider endpoint accepted")
			}
			c = valid()
			c.Storage.Endpoint = raw
			if err := c.Validate(); err == nil {
				t.Error("invalid object store endpoint accepted")
			}
		})
	}
}

func TestValidateRejectsUnsafeObjectNamespace(t *testing.T) {
	for _, prefix := range []string{"../outside", "a/../b", "a//b", "a/./b", "a\\b", "a\x00b"} {
		c := valid()
		c.Storage.KeyPrefix = prefix
		if err := c.Validate(); err == nil {
			t.Errorf("unsafe key prefix %q accepted", prefix)
		}
	}
	for _, name := range []string{"_transmux", "index\x00.m3u8", "index\n.m3u8"} {
		c := valid()
		c.Storage.ManifestName = name
		if err := c.Validate(); err == nil {
			t.Errorf("unsafe manifest name %q accepted", name)
		}
	}
	c := valid()
	c.Storage.KeyPrefix = "/tenant/video/"
	if err := c.Validate(); err != nil {
		t.Fatalf("existing support for surrounding slashes must remain: %v", err)
	}
}

func TestValidateRejectsDurationArithmeticOverflow(t *testing.T) {
	c := valid()
	c.Lease.RenewInterval = Duration{1 << 62}
	c.Lease.OperationTimeout = Duration{time.Second}
	if err := c.Validate(); err == nil {
		t.Error("overflowing renewal budget accepted")
	}
	c = valid()
	c.Segment.MaxGOPSlack = Duration{1<<63 - 1}
	if err := c.Validate(); err == nil {
		t.Error("overflowing segment allowance accepted")
	}
}

func TestLeaseBudgetIncludesTheConfirmationRead(t *testing.T) {
	c := valid()
	c.Lease.TTL = Duration{25 * time.Second}
	c.Lease.RenewInterval = Duration{10 * time.Second}
	c.Lease.OperationTimeout = Duration{2 * time.Second}
	c.Lease.MaxClockSkew = Duration{2 * time.Second}
	if err := c.Validate(); err == nil {
		t.Fatal("lease expires before the second renewal's confirmation can finish")
	}
}

// argvTestSecret stands in for a camera password. A credential check must
// refuse every URL that carries it without ever quoting it back.
const argvTestSecret = "argv-test-secret"

func TestCredentialFreeURLCheckRefusesUserinfoAndSecretQueryKeys(t *testing.T) {
	for name, raw := range map[string]string{
		"user and password": "rtsp://operator:" + argvTestSecret + "@relay/cam",
		"user only":         "rtsp://" + argvTestSecret + "@relay/cam",
		"empty userinfo":    "rtsp://@relay/cam",
		"query password":    "rtsp://relay/cam?channel=1&password=" + argvTestSecret,
		"mixed case key":    "rtsp://relay/cam?PassWord=" + argvTestSecret,
		"passwd":            "rtsp://relay/cam?passwd=" + argvTestSecret,
		"pwd":               "rtsp://relay/cam?pwd=" + argvTestSecret,
		"token":             "rtsp://relay/cam?access_token=" + argvTestSecret,
		"key":               "rtsp://relay/cam?ApiKey=" + argvTestSecret,
		"secret":            "rtsp://relay/cam?client_secret=" + argvTestSecret,
		"auth":              "rtsp://relay/cam?auth=" + argvTestSecret,
		"signature":         "rtsp://relay/cam?X-Amz-Signature=" + argvTestSecret,
		"credential":        "rtsp://relay/cam?X-Amz-Credential=" + argvTestSecret,
		// Only the unescaped name matches: "%74oken" is "token".
		"escaped key": "rtsp://relay/cam?%74oken=" + argvTestSecret,
		"semicolon":   "rtsp://relay/cam?channel=1;token=" + argvTestSecret,
		"bare key":    "rtsp://relay/cam?token" + argvTestSecret,
		// A name that cannot be unescaped is refused: it cannot be read.
		"unreadable key": "rtsp://relay/cam?%zzq=" + argvTestSecret,
	} {
		t.Run(name, func(t *testing.T) {
			cam := StaticCamera{CenterID: "c", CameraID: "cam", RTSPURL: raw}
			err := cam.CheckCredentialFreeURL()
			if err == nil {
				t.Fatal("URL with credentials passed the credential-free check")
			}
			if strings.Contains(err.Error(), argvTestSecret) {
				t.Fatalf("credential check quoted the secret: %v", err)
			}
		})
	}
}

func TestCredentialFreeURLCheckKeepsPlainSourceURLs(t *testing.T) {
	for _, raw := range []string{
		"rtsp://relay:8554/cam01",
		"rtsps://relay/cam",
		"rtsp://relay/cam?channel=1&subtype=0",
		"rtsp://relay/cam?",
	} {
		cam := StaticCamera{CenterID: "c", CameraID: "cam", RTSPURL: raw}
		if err := cam.CheckCredentialFreeURL(); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestValidateRequiresCredentialFreeStaticURLsOnlyWhenAsked(t *testing.T) {
	c := valid()
	c.Cameras.Static = []StaticCamera{
		{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://operator:" + argvTestSecret + "@relay/cam"},
	}
	// Off by default: existing deployments that pass credentials keep working.
	if err := c.Validate(); err != nil {
		t.Fatalf("credential URL refused with the option off: %v", err)
	}
	c.Cameras.RequireCredentialFreeURLs = true
	err := c.Validate()
	if err == nil {
		t.Fatal("credential URL accepted with cameras.require_credential_free_urls on")
	}
	if strings.Contains(err.Error(), argvTestSecret) {
		t.Fatalf("validation quoted the secret: %v", err)
	}
	// A disabled camera never starts ffmpeg, so its URL never reaches argv.
	disabled := false
	c.Cameras.Static[0].Enabled = &disabled
	c.Cameras.Static = append(c.Cameras.Static, StaticCamera{CenterID: "c", CameraID: "cam2", RTSPURL: "rtsp://relay/cam2"})
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled credential camera refused: %v", err)
	}
}

// TestCredentialFreeURLCheckNamesTheMatchedPatternOnly covers the review of
// #45: a refusal must say which fixed pattern matched, so an operator can
// tell a false match (Axis videokeyframeinterval) from a real credential, but
// must not quote the camera's own parameter name, which can be the secret.
func TestCredentialFreeURLCheckNamesTheMatchedPatternOnly(t *testing.T) {
	for raw, want := range map[string]struct{ reason, absent string }{
		"rtsp://relay/axis-media/media.amp?videokeyframeinterval=8": {`query parameter matches credential pattern "key"`, "videokeyframeinterval"},
		"rtsp://relay/cam?access_token=1":                           {`query parameter matches credential pattern "token"`, "access_token"},
		"rtsp://relay/cam?bypass=1":                                 {`query parameter matches credential pattern "pass"`, "bypass"},
		"rtsp://relay/cam?%74oken=1":                                {`query parameter matches credential pattern "token"`, "%74oken"},
		"rtsp://relay/cam?%zzq=1":                                   {"query parameter name cannot be read", "%zzq"},
		"rtsp://operator@relay/cam":                                 {"userinfo", "operator"},
	} {
		cam := StaticCamera{CenterID: "c", CameraID: "cam", RTSPURL: raw}
		err := cam.CheckCredentialFreeURL()
		var cerr *CredentialURLError
		if !errors.As(err, &cerr) {
			t.Fatalf("%s: got %v, want a *CredentialURLError", raw, err)
		}
		if !strings.Contains(err.Error(), want.reason) || !strings.Contains(cerr.Reason, strings.TrimPrefix(want.reason, "query parameter ")) {
			t.Errorf("%s: error %q / reason %q does not name %q", raw, err, cerr.Reason, want.reason)
		}
		if strings.Contains(err.Error(), want.absent) || strings.Contains(cerr.Reason, want.absent) {
			t.Errorf("%s: refusal quoted the camera's own text %q: %v", raw, want.absent, err)
		}
	}
}
