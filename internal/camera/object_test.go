package camera

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/storage"
)

func TestSharedRosterSeedsOnceAndRejectsLostUpdates(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.CameraConfig{Provider: "object", ObjectKey: "_transmux/rosters/test.json",
		RequestTimeout: config.Duration{Duration: time.Second},
		Static:         []config.StaticCamera{{CenterID: "c1", CameraID: "cam1", RTSPURL: "rtsp://camera/live"}}}
	one, err := NewObjectProvider(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Static[0].RTSPURL = "rtsp://different/seed"
	two, err := NewObjectProvider(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	first, version, err := one.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, otherVersion, err := two.Load(ctx)
	if err != nil || version != otherVersion || first[0].RTSPURL != other[0].RTSPURL {
		t.Fatalf("second seed replaced roster: %#v %v", other, err)
	}
	first[0].Name = "first editor"
	other[0].Name = "second editor"
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, roster := range [][]config.StaticCamera{first, other} {
		wg.Go(func() { _, err := one.Save(ctx, roster, version); errs <- err })
	}
	wg.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, storage.ErrPreconditionFailed) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent editors: success=%d conflicts=%d", successes, conflicts)
	}
	_, current, err := one.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.Save(ctx, []config.StaticCamera{}, current); err != nil {
		t.Fatal(err)
	}
	cams, err := two.Cameras(ctx)
	if err != nil || len(cams) != 0 {
		t.Fatalf("explicit empty roster must stop ingestion: %v %v", cams, err)
	}
}

func TestSharedRosterAssignmentsMoveBetweenShards(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.CameraConfig{Provider: "object", ObjectKey: "_transmux/rosters/global.json",
		RequestTimeout: config.Duration{Duration: time.Second}, ShardFilter: "one",
		Static: []config.StaticCamera{{CenterID: "c1", CameraID: "cam1", ShardID: "one", RTSPURL: "rtsp://camera/live"}}}
	one, err := NewObjectProvider(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ShardFilter = "two"
	two, err := NewObjectProvider(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	before, err := one.Cameras(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("initial assignment: %v %v", before, err)
	}
	before, err = two.Cameras(ctx)
	if err != nil || len(before) != 0 {
		t.Fatalf("camera assigned twice: %v %v", before, err)
	}
	roster, version, err := one.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roster[0].ShardID = "two"
	if _, err := one.Save(ctx, roster, version); err != nil {
		t.Fatal(err)
	}
	after, err := one.Cameras(ctx)
	if err != nil || len(after) != 0 {
		t.Fatalf("old shard kept removed assignment: %v %v", after, err)
	}
	after, err = two.Cameras(ctx)
	if err != nil || len(after) != 1 {
		t.Fatalf("new shard did not receive assignment: %v %v", after, err)
	}
}
