package playback

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/storage"
)

func expiredObjects(t *testing.T, f *fixture, center, camera string, count int, start time.Time) {
	t.Helper()
	start = time.UnixMilli(start.UnixMilli()).UTC()
	for n := 1; n <= count; n++ {
		pdt := start.Add(time.Duration(n) * 4 * time.Second)
		uri := path.Join(pdt.Format("2006/01/02"), fmt.Sprintf("seg-%09d-%d.ts", n, pdt.UnixMilli()))
		md := map[string]string{"center-id": center, "camera-id": camera, "sequence": strconv.Itoa(n),
			"pdt-ms": strconv.FormatInt(pdt.UnixMilli(), 10), "duration-ms": "4000", "discontinuity": "false"}
		key := path.Join("archive", center, camera, uri)
		if _, err := f.store.Put(context.Background(), storage.Object{Key: key, Body: []byte("media"), Metadata: md}); err != nil {
			t.Fatal(err)
		}
	}
}

func remaining(t *testing.T, f *fixture, center, camera string) int {
	t.Helper()
	page, err := f.store.List(context.Background(), path.Join("archive", center, camera)+"/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Entries)
}

func TestRetentionSpendsTheWholeBatchFairlyAcrossCameras(t *testing.T) {
	f := newFixture(t)
	f.app.cfg.Retention.Days = 2
	old := time.Now().AddDate(0, 0, -8)
	expiredObjects(t, f, "c1", "cam1", 200, old)
	expiredObjects(t, f, "c2", "cam2", 200, old)
	// One sweep of 64 per camera used to end the pass with 172 of the 300
	// batch unspent.
	f.app.cfg.Retention.Batch = 300
	f.app.retentionPass(context.Background(), 0)
	cam1, cam2 := 200-remaining(t, f, "c1", "cam1"), 200-remaining(t, f, "c2", "cam2")
	if cam1+cam2 != 300 || cam1 < 128 || cam2 < 128 {
		t.Fatalf("batch not spent fairly: cam1=%d cam2=%d", cam1, cam2)
	}
	// A large batch stops once every camera reaches unexpired media.
	f.app.cfg.Retention.Batch = 10000
	f.app.retentionPass(context.Background(), 0)
	if remaining(t, f, "c1", "cam1") != 0 || remaining(t, f, "c2", "cam2") != 0 {
		t.Fatal("expired media left behind with batch to spare")
	}
	if status := f.app.retention; status.Deleted != 400 || status.Errors != 0 {
		t.Fatalf("retention status: %+v", status)
	}
}

func TestRecordingsDeletedFromStorageDisappearFromTheIndex(t *testing.T) {
	f := newFixture(t)
	// Both recordings share a capture day and lie wholly in the past.
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(24 * time.Hour).Add(time.Minute)
	kept := f.segment(t, 1, base, 4*time.Second, []byte("kept"), "")
	gone := f.segment(t, 2, base.Add(time.Minute), 4*time.Second, []byte("gone"), "")
	if err := f.store.Delete(context.Background(), gone.Key); err != nil {
		t.Fatal(err)
	} // e.g. an S3 Lifecycle expiration
	day := gone.Start.UTC().Truncate(24 * time.Hour)
	// A listing that begins after both entries were indexed shows only kept.
	page, err := f.store.List(context.Background(), "archive/c1/cam1/"+day.Format("2006/01/02")+"/", "", 1000)
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("listing: %+v %v", page, err)
	}
	if n, err := f.index.Reconcile("c1", "cam1", day, "", "", []string{kept.URI}, time.Now().Add(2*time.Minute)); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	token := f.token(t, allPermissions())
	query := url.Values{"center_id": {"c1"}, "camera_id": {"cam1"},
		"start": {base.Add(-time.Minute).UTC().Format(time.RFC3339Nano)}, "end": {base.Add(3 * time.Minute).Format(time.RFC3339Nano)}}
	resp, body := f.request(t, "GET", "/v1/recordings?"+query.Encode(), token, nil, nil)
	var result struct {
		Periods []struct {
			Segments int `json:"segments"`
		} `json:"periods"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &result) != nil || len(result.Periods) != 1 {
		t.Fatalf("deleted recording still listed: %d %s", resp.StatusCode, body)
	}
	resp, body = f.request(t, "POST", "/v1/playback-sessions", token, sessionRequest{CenterID: "c1", CameraIDs: []string{"cam1"},
		Mode: "recording", Start: gone.Start, End: gone.End()}, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("session issued for a deleted recording: %d %s", resp.StatusCode, body)
	}
}
