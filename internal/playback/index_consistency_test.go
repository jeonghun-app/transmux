package playback

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/recording"
)

// TestUpgradedIndexDropsLifecycleDeletionsThroughTheScanner covers the
// upgrade path end to end: an index written by the previous release has no
// object-name buckets, an S3 Lifecycle rule then deletes recordings, and the
// server's own scanner must remove them from /v1/recordings and refuse new
// sessions for them while keeping the recordings that still exist.
func TestUpgradedIndexDropsLifecycleDeletionsThroughTheScanner(t *testing.T) {
	f := newFixture(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	base := day.Add(10 * time.Hour)
	var records []recording.Segment
	for n := 1; n <= 6; n++ {
		records = append(records, f.segment(t, uint64(n), base.Add(time.Duration(n)*4*time.Second), 4*time.Second, []byte("media"), ""))
	}
	// Rewrite the file as the previous release left it: segments only.
	filename := f.app.cfg.IndexPath
	if err := f.index.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filename, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"objects-v1", "objects-migration-v1"} {
			if tx.Bucket([]byte(name)) != nil {
				if err := tx.DeleteBucket([]byte(name)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	index, err := recording.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	f.index, f.app.index, f.app.scanner.Index = index, index, index

	token := f.token(t, allPermissions())
	listedSegments := func() int {
		t.Helper()
		query := url.Values{"center_id": {"c1"}, "camera_id": {"cam1"},
			"start": {base.Format(time.RFC3339Nano)}, "end": {base.Add(time.Minute).Format(time.RFC3339Nano)}}
		resp, body := f.request(t, "GET", "/v1/recordings?"+query.Encode(), token, nil, nil)
		var result struct {
			Periods []struct {
				Segments int `json:"segments"`
			} `json:"periods"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(body, &result) != nil {
			t.Fatalf("recordings: %d %s", resp.StatusCode, body)
		}
		total := 0
		for _, p := range result.Periods {
			total += p.Segments
		}
		return total
	}
	if got := listedSegments(); got != 6 {
		t.Fatalf("upgrade lost recordings: %d of 6 listed", got)
	}
	// Lifecycle expires the middle and the last recording.
	gone := []recording.Segment{records[2], records[5]}
	for _, s := range gone {
		if err := f.store.Delete(context.Background(), s.Key); err != nil {
			t.Fatal(err)
		}
	}
	cam := config.StaticCamera{CenterID: "c1", CameraID: "cam1"}
	if err := f.app.scanner.ScanDay(context.Background(), cam, day); err != nil {
		t.Fatal(err)
	}
	if got := listedSegments(); got != 4 {
		t.Fatalf("deleted recordings still listed after reconciliation: %d", got)
	}
	if status := f.app.scanner.Status(); status.Removed != 2 {
		t.Fatalf("removed counter: %+v", status)
	}
	for _, s := range gone {
		resp, body := f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{CenterID: "c1",
			CameraIDs: []string{"cam1"}, Mode: "recording", Start: s.Start, End: s.End()}, nil)
		if resp.StatusCode != 404 {
			t.Fatalf("session issued for deleted %s: %d %s", s.URI, resp.StatusCode, body)
		}
	}
	kept := records[0]
	resp, body := f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{CenterID: "c1",
		CameraIDs: []string{"cam1"}, Mode: "recording", Start: kept.Start, End: kept.End()}, nil)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("session refused for a recording that still exists: %d %s", resp.StatusCode, body)
	}
}
