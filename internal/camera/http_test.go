package camera

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
)

func TestValidationErrorDoesNotExposeMalformedURL(t *testing.T) {
	c := Camera{CenterID: "c", CameraID: "cam", RTSPURL: "rtsp://admin:secret-password@host/%xx?token=secret-token"}
	err := c.Validate()
	if err == nil {
		t.Fatal("malformed URL was accepted")
	}
	for _, secret := range []string{"secret-password", "secret-token", "admin"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("validation error leaked credentials: %v", err)
		}
	}
}

func TestHTTPProviderRejectsInvalidPayloadWithoutDroppingTheRoster(t *testing.T) {
	for name, payload := range map[string]string{
		"unknown field": `[{"center_id":"c","camera_id":"cam","rtsp_url":"rtsp://host/live","enable":false}]`,
		"oversized":     "[]" + strings.Repeat(" ", 8<<20) + "garbage",
		"null":          "null",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer srv.Close()
			p, err := NewProvider(config.CameraConfig{
				Provider: "http", URL: srv.URL, RequestTimeout: config.Duration{Duration: time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Cameras(context.Background()); err == nil {
				t.Fatal("invalid provider response must not become a successful roster")
			}
		})
	}
}

func TestHTTPProviderAcceptsAnExplicitEmptyRoster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer example" {
			t.Error("provider authorization header missing")
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	p, err := NewProvider(config.CameraConfig{
		Provider: "http", URL: srv.URL, AuthHeader: "Bearer example",
		RequestTimeout: config.Duration{Duration: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	cams, err := p.Cameras(context.Background())
	if err != nil || len(cams) != 0 {
		t.Fatalf("explicit empty array must remove the roster: %v, %v", cams, err)
	}
}
