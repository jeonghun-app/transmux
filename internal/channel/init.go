package channel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/jeonghun-app/transmux/internal/storage"
)

func (w *Worker) publishInit(ctx context.Context, gen *drainState, pdt time.Time) (string, error) {
	day := pdt.UTC().Format("2006/01/02")
	if gen.initURI != "" && path.Dir(gen.initURI) == day {
		return gen.initURI, nil
	}
	raw, err := os.ReadFile(filepath.Join(w.spoolDir, "init.mp4"))
	if err != nil {
		return "", fmt.Errorf("read initialization segment: %w", err)
	}
	if len(raw) == 0 || len(raw) > 1<<20 {
		return "", fmt.Errorf("invalid initialization segment size")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	uri := path.Join(day, "init-"+digest+".mp4")
	key, putID := w.objectKey(uri), "init:"+digest
	_, err = w.uploader.Put(ctx, storage.Object{
		Key: key, Body: raw, ContentType: "video/mp4",
		Tags:          storage.RetentionTags(w.cfg.Storage.TagMedia, true),
		CacheControl:  "public, max-age=31536000, immutable",
		Metadata:      map[string]string{"put-id": putID, "center-id": w.cam.CenterID, "camera-id": w.cam.CameraID},
		Preconditions: storage.Preconditions{IfNoneMatch: true},
	})
	if err != nil {
		if _, resolveErr := w.resolveSegmentConflict(ctx, key, putID); resolveErr != nil {
			return "", fmt.Errorf("store initialization segment: %w", resolveErr)
		}
	}
	gen.initURI = uri
	return uri, nil
}
