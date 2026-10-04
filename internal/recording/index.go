// Package recording builds a persistent time index independently of ingest.
// S3 remains the source of truth; losing this database requires a rebuild,
// but never stops a camera or makes a live upload wait for the index.
package recording

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/jeonghun-app/transmux/internal/config"
	"github.com/jeonghun-app/transmux/internal/hls"
	"github.com/jeonghun-app/transmux/internal/storage"
)

const MaxSegmentDuration = time.Hour

var (
	ErrTooMany     = errors.New("recording range contains too many segments")
	segmentsBucket = []byte("segments-v1")
	scansBucket    = []byte("scans-v1")
	// objectsBucket maps each indexed object name to its segment record, per
	// capture day, in the lexical order an S3 listing uses. It lets a listed
	// page be reconciled against the index without scanning a whole day.
	objectsBucket = []byte("objects-v1")
	// migrationsBucket holds, per partition still being backfilled into
	// objectsBucket, the last segment record key already copied.
	migrationsBucket = []byte("objects-migration-v1")
)

// migrationBatch bounds one backfill write transaction so that upgrading a
// large index never blocks live indexing or session writes for long.
const migrationBatch = 1000

type Segment struct {
	CenterID      string    `json:"center_id"`
	CameraID      string    `json:"camera_id"`
	Key           string    `json:"key"`
	URI           string    `json:"uri"`
	InitURI       string    `json:"init_uri,omitempty"`
	Sequence      uint64    `json:"sequence"`
	Start         time.Time `json:"start"`
	DurationMS    int64     `json:"duration_ms"`
	Discontinuity bool      `json:"discontinuity"`
	Size          int64     `json:"size"`
}

func (s Segment) End() time.Time { return s.Start.Add(time.Duration(s.DurationMS) * time.Millisecond) }

func FromObject(prefix, center, camera string, entry storage.Entry, info storage.ObjectInfo) (Segment, error) {
	base := path.Join(prefix, center, camera) + "/"
	if !strings.HasPrefix(entry.Key, base) {
		return Segment{}, fmt.Errorf("recording is outside its camera")
	}
	uri := strings.TrimPrefix(entry.Key, base)
	seq, pdt, err := hls.SegmentIdentity(uri)
	if err != nil {
		return Segment{}, err
	}
	md := info.Metadata
	if md["center-id"] != center || md["camera-id"] != camera ||
		md["sequence"] != strconv.FormatUint(seq, 10) || md["pdt-ms"] != strconv.FormatInt(pdt.UnixMilli(), 10) {
		return Segment{}, fmt.Errorf("recording metadata disagrees with its key")
	}
	duration, err := strconv.ParseInt(md["duration-ms"], 10, 64)
	if err != nil || duration < 1 || duration > MaxSegmentDuration.Milliseconds() || info.Size <= 0 {
		return Segment{}, fmt.Errorf("recording duration or size is invalid")
	}
	disc, err := strconv.ParseBool(md["discontinuity"])
	if err != nil {
		return Segment{}, fmt.Errorf("recording discontinuity is invalid")
	}
	initURI := md["init-uri"]
	if strings.HasSuffix(uri, ".m4s") {
		if !hls.ValidInitURI(initURI) || path.Dir(initURI) != path.Dir(uri) {
			return Segment{}, fmt.Errorf("recording initialization URI is invalid")
		}
	} else if initURI != "" {
		return Segment{}, fmt.Errorf("MPEG-TS recording carries an initialization URI")
	}
	return Segment{CenterID: center, CameraID: camera, Key: entry.Key, URI: uri,
		InitURI: initURI, Sequence: seq, Start: pdt, DurationMS: duration,
		Discontinuity: disc, Size: info.Size}, nil
}

type Index struct {
	db *bolt.DB
	id string
}

func Open(filename string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filename, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open recording index (one process per file): %w", err)
	}
	var id string
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{segmentsBucket, scansBucket, objectsBucket, migrationsBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		bucket := tx.Bucket(scansBucket)
		id = string(bucket.Get([]byte("_index_id")))
		if id == "" {
			raw := make([]byte, 16)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			id = hex.EncodeToString(raw)
			if err := bucket.Put([]byte("_index_id"), []byte(id)); err != nil {
				return err
			}
		}
		if raw, err := hex.DecodeString(id); err != nil || len(raw) != 16 {
			return fmt.Errorf("invalid recording index identity")
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db, id: id}, nil
}

func (i *Index) Close() error     { return i.db.Close() }
func (i *Index) Identity() string { return i.id }

func recordKey(t time.Time, seq uint64) []byte {
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key, uint64(t.UnixMilli()))
	binary.BigEndian.PutUint64(key[8:], seq)
	return key
}

func partition(center, camera string, day time.Time) []byte {
	return []byte(center + "/" + camera + "/" + day.UTC().Format("20060102"))
}

// objectValue is the record key followed by the time the entry was indexed.
func objectValue(record []byte, indexed time.Time) []byte {
	value := make([]byte, 24)
	copy(value, record)
	binary.BigEndian.PutUint64(value[16:], uint64(max(0, indexed.UnixMilli())))
	return value
}

// objectNames returns the object-name bucket of one partition. A partition
// written before that bucket existed is queued for a resumable backfill; see
// migrateNames.
func objectNames(tx *bolt.Tx, name []byte) (*bolt.Bucket, error) {
	root := tx.Bucket(objectsBucket)
	if bucket := root.Bucket(name); bucket != nil {
		return bucket, nil
	}
	bucket, err := root.CreateBucket(name)
	if err != nil {
		return nil, err
	}
	if segments := tx.Bucket(segmentsBucket).Bucket(name); segments != nil {
		if err := tx.Bucket(migrationsBucket).Put(name, []byte{}); err != nil {
			return nil, err
		}
	}
	return bucket, nil
}

// migrateNames copies at most migrationBatch existing segment records of one
// partition into its object-name bucket, with a zero index time so every one
// is eligible to reconcile. It reports whether the partition is complete.
// Names already present were written by a later Put and are kept.
func (i *Index) migrateNames(name []byte) (bool, error) {
	done := false
	err := i.db.Update(func(tx *bolt.Tx) error {
		names, err := objectNames(tx, name)
		if err != nil {
			return err
		}
		progress := tx.Bucket(migrationsBucket).Get(name)
		segments := tx.Bucket(segmentsBucket).Bucket(name)
		if progress == nil || segments == nil {
			done = true
			return tx.Bucket(migrationsBucket).Delete(name)
		}
		c := segments.Cursor()
		k, raw := c.First()
		if len(progress) > 0 {
			k, raw = c.Seek(progress)
			if k != nil && bytes.Equal(k, progress) {
				k, raw = c.Next()
			}
		}
		var last []byte
		for n := 0; k != nil && n < migrationBatch; k, raw = c.Next() {
			var s Segment
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			if names.Get([]byte(s.URI)) == nil {
				if err := names.Put([]byte(s.URI), objectValue(k, time.Time{})); err != nil {
					return err
				}
			}
			last, n = bytes.Clone(k), n+1
		}
		if k == nil {
			done = true
			return tx.Bucket(migrationsBucket).Delete(name)
		}
		return tx.Bucket(migrationsBucket).Put(name, last)
	})
	return done, err
}

func (i *Index) Put(segments []Segment) error {
	if len(segments) == 0 {
		return nil
	}
	now := time.Now()
	return i.db.Update(func(tx *bolt.Tx) error {
		for _, s := range segments {
			if !config.ValidID(s.CenterID) || !config.ValidID(s.CameraID) || s.Start.UnixMilli() < 0 ||
				s.DurationMS < 1 || s.DurationMS > MaxSegmentDuration.Milliseconds() {
				return fmt.Errorf("invalid recording index entry")
			}
			name := partition(s.CenterID, s.CameraID, s.Start)
			names, err := objectNames(tx, name)
			if err != nil {
				return err
			}
			bucket, err := tx.Bucket(segmentsBucket).CreateBucketIfNotExists(name)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(s)
			if err != nil {
				return err
			}
			key := recordKey(s.Start, s.Sequence)
			if err := bucket.Put(key, raw); err != nil {
				return err
			}
			if err := names.Put([]byte(s.URI), objectValue(key, now)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (i *Index) Get(center, camera, uri string) (Segment, error) {
	seq, pdt, err := hls.SegmentIdentity(uri)
	if err != nil {
		return Segment{}, storage.ErrNotFound
	}
	var out Segment
	err = i.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(segmentsBucket).Bucket(partition(center, camera, pdt))
		if bucket == nil {
			return storage.ErrNotFound
		}
		raw := bucket.Get(recordKey(pdt, seq))
		if raw == nil {
			return storage.ErrNotFound
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		if out.URI != uri {
			return storage.ErrNotFound
		}
		return nil
	})
	return out, err
}

func midnight(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// Query includes overlapping boundary segments and scans a bounded predecessor
// interval. It never lists S3 or HEADs a media object on a viewer request.
func (i *Index) Query(ctx context.Context, center, camera string, start, end time.Time, limit int) ([]Segment, error) {
	if !config.ValidID(center) || !config.ValidID(camera) || !start.Before(end) ||
		start.UnixMilli() < 0 || end.Sub(start) > 31*24*time.Hour || limit < 1 {
		return nil, fmt.Errorf("invalid recording range")
	}
	from := start.Add(-MaxSegmentDuration)
	var out []Segment
	err := i.db.View(func(tx *bolt.Tx) error {
		for day := midnight(from); day.Before(end); day = day.AddDate(0, 0, 1) {
			if err := ctx.Err(); err != nil {
				return err
			}
			bucket := tx.Bucket(segmentsBucket).Bucket(partition(center, camera, day))
			if bucket == nil {
				continue
			}
			c := bucket.Cursor()
			for key, raw := c.Seek(recordKey(from, 0)); key != nil; key, raw = c.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(key) != 16 || raw == nil {
					return fmt.Errorf("corrupt recording index")
				}
				timestamp := int64(binary.BigEndian.Uint64(key))
				if timestamp >= end.UnixMilli() {
					break
				}
				var s Segment
				if err := json.Unmarshal(raw, &s); err != nil {
					return err
				}
				if !s.End().After(start) {
					continue
				}
				if len(out) == limit {
					return ErrTooMany
				}
				out = append(out, s)
			}
		}
		return nil
	})
	return out, err
}

func (i *Index) Remove(s Segment) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		name := partition(s.CenterID, s.CameraID, s.Start)
		if names := tx.Bucket(objectsBucket).Bucket(name); names != nil {
			if err := names.Delete([]byte(s.URI)); err != nil {
				return err
			}
		}
		bucket := tx.Bucket(segmentsBucket).Bucket(name)
		if bucket == nil {
			return nil
		}
		return bucket.Delete(recordKey(s.Start, s.Sequence))
	})
}

// reconcileGrace keeps entries indexed shortly before a listing began. It
// absorbs wall-clock adjustments between the index write and the listing.
var reconcileGrace = time.Minute

// Reconcile removes the indexed segments of one capture day whose objects are
// absent from a listing of the object-name range (after, upto]. An empty upto
// means the listing reached the end of the day. S3 listings are strongly
// consistent, so an object indexed before the listing began and missing from
// it has been deleted, typically by a Lifecycle rule. An entry indexed after
// the listing began may name an object written later and is kept. When ctx
// ends before a pending backfill finishes, nothing is removed and ctx's error
// is returned; the backfill resumes from its saved position.
func (i *Index) Reconcile(ctx context.Context, center, camera string, day time.Time, after, upto string,
	listed []string, listedAt time.Time) (int, error) {
	present := make(map[string]bool, len(listed))
	for _, name := range listed {
		present[name] = true
	}
	before := uint64(max(0, listedAt.Add(-reconcileGrace).UnixMilli()))
	name := partition(center, camera, day)
	// Finish any backfill first, one short transaction per batch. Each
	// batch saves its position, so a cancelled call resumes next time and
	// nothing is reconciled against a partial name set.
	for done := false; !done; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var err error
		if done, err = i.migrateNames(name); err != nil {
			return 0, err
		}
	}
	removed := 0
	err := i.db.Update(func(tx *bolt.Tx) error {
		segments := tx.Bucket(segmentsBucket).Bucket(name)
		names := tx.Bucket(objectsBucket).Bucket(name)
		if segments == nil || names == nil || tx.Bucket(migrationsBucket).Get(name) != nil {
			return nil
		}
		type entry struct{ name, record []byte }
		var stale []entry
		c := names.Cursor()
		k, v := c.Seek([]byte(after))
		if k != nil && string(k) == after {
			k, v = c.Next()
		}
		for ; k != nil; k, v = c.Next() {
			if upto != "" && string(k) > upto {
				break
			}
			if present[string(k)] || len(v) != 24 || binary.BigEndian.Uint64(v[16:]) >= before {
				continue
			}
			stale = append(stale, entry{bytes.Clone(k), bytes.Clone(v[:16])})
		}
		for _, e := range stale {
			// A later object with the same capture identity may own the
			// record; then only the stale name is dropped.
			if raw := segments.Get(e.record); raw != nil {
				var s Segment
				if err := json.Unmarshal(raw, &s); err != nil {
					return err
				}
				if s.URI == string(e.name) {
					if err := segments.Delete(e.record); err != nil {
						return err
					}
					removed++
				}
			}
			if err := names.Delete(e.name); err != nil {
				return err
			}
		}
		return nil
	})
	return removed, err
}

// partitionExpired reports whether every segment a capture day can hold ended
// by horizon. A day holds segments that start before its end, and a segment
// lasts at most MaxSegmentDuration.
func partitionExpired(day, horizon time.Time) bool {
	return !midnight(day).AddDate(0, 0, 1).Add(MaxSegmentDuration).After(horizon)
}

// Prune drops whole capture days whose segments all ended by horizon, with
// their scan state. The scanner no longer reconciles those days against S3,
// so keeping them would list recordings that may be gone and grow the file
// without bound. A segment ending after horizon is never dropped. At most
// limit days are dropped per call to keep each write transaction small.
func (i *Index) Prune(horizon time.Time, limit int) (int, error) {
	expired := func(name []byte) bool {
		parts := strings.Split(string(name), "/")
		if len(parts) != 3 || !config.ValidID(parts[0]) || !config.ValidID(parts[1]) {
			return false
		}
		day, err := time.Parse("20060102", parts[2])
		return err == nil && partitionExpired(day, horizon)
	}
	var names [][]byte
	err := i.db.View(func(tx *bolt.Tx) error {
		seen := map[string]bool{}
		for _, root := range [][]byte{segmentsBucket, objectsBucket, scansBucket} {
			c := tx.Bucket(root).Cursor()
			for k, _ := c.First(); k != nil && len(names) < limit; k, _ = c.Next() {
				if expired(k) && !seen[string(k)] {
					seen[string(k)] = true
					names = append(names, bytes.Clone(k))
				}
			}
		}
		return nil
	})
	if err != nil || len(names) == 0 {
		return 0, err
	}
	err = i.db.Update(func(tx *bolt.Tx) error {
		for _, name := range names {
			for _, root := range [][]byte{segmentsBucket, objectsBucket} {
				if tx.Bucket(root).Bucket(name) != nil {
					if err := tx.Bucket(root).DeleteBucket(name); err != nil {
						return err
					}
				}
			}
			for _, root := range [][]byte{scansBucket, migrationsBucket} {
				if err := tx.Bucket(root).Delete(name); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(names), nil
}

func (i *Index) Latest(center, camera string, now time.Time) (Segment, error) {
	var latest Segment
	err := i.db.View(func(tx *bolt.Tx) error {
		for offset := 0; offset < 2; offset++ {
			bucket := tx.Bucket(segmentsBucket).Bucket(partition(center, camera, now.AddDate(0, 0, -offset)))
			if bucket == nil {
				continue
			}
			_, raw := bucket.Cursor().Last()
			if raw != nil {
				return json.Unmarshal(raw, &latest)
			}
		}
		return storage.ErrNotFound
	})
	return latest, err
}

// Playlist preserves true durations and makes source gaps explicit. A
// container-format change requires a separate recording request because HLS
// cannot unset an EXT-X-MAP once a fragmented MP4 map has been introduced.
func Playlist(records []Segment) ([]hls.PublishedSegment, error) {
	var out []hls.PublishedSegment
	for _, r := range records {
		if len(out) > 0 {
			prev := out[len(out)-1]
			if (prev.InitURI != "") != (r.InitURI != "") {
				return nil, fmt.Errorf("recording range crosses a container change; select one format period")
			}
			// A late stale-owner object can be wholly covered by a segment
			// already selected. Do not play the same capture interval twice.
			if !r.End().After(prev.ProgramDateTime.Add(prev.Duration)) {
				continue
			}
		}
		s := hls.PublishedSegment{Sequence: uint64(len(out)), URI: r.URI, InitURI: r.InitURI,
			ProgramDateTime: r.Start, Duration: time.Duration(r.DurationMS) * time.Millisecond,
			Discontinuity: r.Discontinuity, Bytes: r.Size}
		if len(out) > 0 {
			prev := out[len(out)-1]
			delta := r.Start.Sub(prev.ProgramDateTime.Add(prev.Duration))
			if delta > 250*time.Millisecond || delta < -250*time.Millisecond || s.InitURI != prev.InitURI {
				s.Discontinuity = true
			}
		}
		out = append(out, s)
	}
	return out, nil
}

type ScanState struct {
	Cursor       string    `json:"cursor"`
	CycleStarted time.Time `json:"cycle_started"`
	ScannedAt    time.Time `json:"scanned_at"`
	Complete     bool      `json:"complete"`
}

func (i *Index) ScanState(center, camera string, day time.Time) (ScanState, error) {
	var state ScanState
	err := i.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(scansBucket).Get(partition(center, camera, day))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &state)
	})
	return state, err
}

func (i *Index) SaveScan(center, camera string, day time.Time, state ScanState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return i.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(scansBucket).Put(partition(center, camera, day), raw)
	})
}

func (i *Index) Cursor(name string) (string, error) {
	var cursor string
	err := i.db.View(func(tx *bolt.Tx) error {
		cursor = string(tx.Bucket(scansBucket).Get([]byte("cursor/" + name)))
		return nil
	})
	return cursor, err
}

func (i *Index) SaveCursor(name, value string) error {
	return i.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(scansBucket).Put([]byte("cursor/"+name), []byte(value))
	})
}

// Expired visits only indexed media from complete capture days before cutoff.
// Control objects and the live manifest are never candidates for deletion.
func (i *Index) Expired(ctx context.Context, cutoff time.Time, limit int) ([]Segment, error) {
	var out []Segment
	err := i.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(segmentsBucket).ForEach(func(name, value []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(out) >= limit || value != nil {
				return nil
			}
			bucket := tx.Bucket(segmentsBucket).Bucket(name)
			return bucket.ForEach(func(_, raw []byte) error {
				if len(out) >= limit {
					return nil
				}
				var s Segment
				if err := json.Unmarshal(raw, &s); err != nil {
					return err
				}
				if !s.End().After(cutoff) {
					out = append(out, s)
				}
				return nil
			})
		})
	})
	return out, err
}
