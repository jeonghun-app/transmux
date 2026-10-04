package channel

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jeonghun-app/transmux/internal/config"
)

// Playback, indexing, retention and export all build keys with
// path.Join(loaded key_prefix, center, camera, rel). Ingest must produce the
// identical key for every accepted spelling of key_prefix.
func TestObjectKeyMatchesPlaybackLayoutForEveryPrefixShape(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		base   string
	}{
		{"/a/b/", "a/b/c1/cam1/"},
		{"a/b", "a/b/c1/cam1/"},
		{"/a/b", "a/b/c1/cam1/"},
		{"a/b/", "a/b/c1/cam1/"},
		{"", "c1/cam1/"},
		{"/", "c1/cam1/"},
		{"//tenant//", "tenant/c1/cam1/"},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.json")
			body := `{"storage":{"bucket":"b","key_prefix":` + strconv.Quote(tc.prefix) + `},"cameras":{` +
				`"static":[{"center_id":"c1","camera_id":"cam1","rtsp_url":"rtsp://h/s"}]}}`
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(file)
			if err != nil {
				t.Fatal(err)
			}
			w := newTestWorker(t, &fakeStore{})
			w.cfg.Storage.KeyPrefix = cfg.Storage.KeyPrefix
			for _, rel := range []string{"2026/08/31/seg-000000001-1.ts", "2026/08/31/init-x.mp4", "index.m3u8", LeaseObjectName} {
				got := w.objectKey(rel)
				if got != tc.base+rel {
					t.Errorf("objectKey(%q) = %q, want %q", rel, got, tc.base+rel)
				}
				if want := path.Join(cfg.Storage.KeyPrefix, "c1", "cam1", rel); got != want {
					t.Errorf("ingest key %q differs from playback key %q", got, want)
				}
				if strings.HasPrefix(got, "/") || strings.Contains(got, "//") {
					t.Errorf("objectKey(%q) = %q is not a canonical object key", rel, got)
				}
			}
		})
	}
}
