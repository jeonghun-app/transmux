package config

import (
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
