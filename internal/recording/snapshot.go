package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/storage"
)

var (
	snapshotsBucket    = []byte("playback-snapshots-v1")
	ErrSessionCapacity = errors.New("recording session capacity reached")
)

// SnapshotEntry is one playback session's immutable recording view.
type SnapshotEntry struct {
	ID       string
	Expires  time.Time
	Segments []hls.PublishedSegment
}

// Snapshot makes VOD immutable for a playback session. Backfill or a delayed
// upload may improve the index while a viewer is watching; it must not change
// that viewer's ENDLIST or give the token access to newly discovered objects.
func (i *Index) Snapshot(id string, expires time.Time, segments []hls.PublishedSegment, capacity int) error {
	return i.Snapshots([]SnapshotEntry{{ID: id, Expires: expires, Segments: segments}}, capacity)
}

// Snapshots stores every entry in one transaction. A multi-camera request
// either receives all of its sessions or consumes no session capacity, so a
// partial failure cannot leave unusable snapshots holding slots until expiry.
func (i *Index) Snapshots(entries []SnapshotEntry, capacity int) error {
	bodies := make([][]byte, len(entries))
	for n, entry := range entries {
		bodies[n] = hls.RenderRecording(entry.Segments)
	}
	return i.db.Update(func(tx *bolt.Tx) error {
		root, err := tx.CreateBucketIfNotExists(snapshotsBucket)
		if err != nil {
			return err
		}
		if root.Sequence()+uint64(len(entries)) > uint64(capacity) {
			return ErrSessionCapacity
		}
		for n, entry := range entries {
			if err := putSnapshot(root, entry, bodies[n]); err != nil {
				return err
			}
		}
		return root.SetSequence(root.Sequence() + uint64(len(entries)))
	})
}

func putSnapshot(root *bolt.Bucket, entry SnapshotEntry, body []byte) error {
	bucket, err := root.CreateBucket([]byte(entry.ID))
	if err != nil {
		return err
	}
	stamp := make([]byte, 8)
	binary.BigEndian.PutUint64(stamp, uint64(entry.Expires.Unix()))
	if err := bucket.Put([]byte("_expires"), stamp); err != nil {
		return err
	}
	if err := bucket.Put([]byte("_manifest"), body); err != nil {
		return err
	}
	for _, s := range entry.Segments {
		if err := bucket.Put([]byte(s.URI), []byte{1}); err != nil {
			return err
		}
		if s.InitURI != "" {
			if err := bucket.Put([]byte(s.InitURI), []byte{1}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *Index) SnapshotManifest(id string) ([]byte, error) {
	var body []byte
	err := i.withSnapshot(id, func(bucket *bolt.Bucket) error {
		body = append([]byte(nil), bucket.Get([]byte("_manifest"))...)
		if len(body) == 0 {
			return fmt.Errorf("invalid recording snapshot")
		}
		return nil
	})
	return body, err
}

func (i *Index) SnapshotAllows(id, uri string) bool {
	return i.withSnapshot(id, func(bucket *bolt.Bucket) error {
		if string(bucket.Get([]byte(uri))) != "\x01" {
			return storage.ErrNotFound
		}
		return nil
	}) == nil
}

func (i *Index) withSnapshot(id string, fn func(*bolt.Bucket) error) error {
	return i.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(snapshotsBucket)
		if root == nil {
			return storage.ErrNotFound
		}
		bucket := root.Bucket([]byte(id))
		if bucket == nil {
			return storage.ErrNotFound
		}
		exp := bucket.Get([]byte("_expires"))
		if len(exp) != 8 || time.Now().Unix() >= int64(binary.BigEndian.Uint64(exp)) {
			return storage.ErrNotFound
		}
		return fn(bucket)
	})
}

func (i *Index) PruneSnapshots(now time.Time) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(snapshotsBucket)
		if root == nil {
			return nil
		}
		var expired [][]byte
		err := root.ForEach(func(key, value []byte) error {
			if value != nil {
				return nil
			}
			exp := root.Bucket(key).Get([]byte("_expires"))
			if len(exp) != 8 || int64(binary.BigEndian.Uint64(exp)) <= now.Unix() {
				expired = append(expired, append([]byte(nil), key...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range expired {
			if err := root.DeleteBucket(key); err != nil {
				return err
			}
		}
		if len(expired) > 0 {
			count := root.Sequence()
			if uint64(len(expired)) > count {
				return fmt.Errorf("invalid snapshot count")
			}
			return root.SetSequence(count - uint64(len(expired)))
		}
		return nil
	})
}

func (i *Index) ExtendSnapshot(id string, expires time.Time) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(snapshotsBucket)
		if root == nil {
			return storage.ErrNotFound
		}
		bucket := root.Bucket([]byte(id))
		if bucket == nil {
			return storage.ErrNotFound
		}
		current := bucket.Get([]byte("_expires"))
		if len(current) != 8 || int64(binary.BigEndian.Uint64(current)) <= time.Now().Unix() {
			return storage.ErrNotFound
		}
		if expires.Unix() > int64(binary.BigEndian.Uint64(current)) {
			stamp := make([]byte, 8)
			binary.BigEndian.PutUint64(stamp, uint64(expires.Unix()))
			return bucket.Put([]byte("_expires"), stamp)
		}
		return nil
	})
}
