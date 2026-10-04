// Package recording builds a persistent time index independently of ingest.
// S3 remains the source of truth; losing this database requires a rebuild,
// but never stops a camera or makes a live upload wait for the index.
package recording

import (
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
)

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
		for _, name := range [][]byte{segmentsBucket, scansBucket} {
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

func (i *Index) Put(segments []Segment) error {
	if len(segments) == 0 {
		return nil
	}
	return i.db.Update(func(tx *bolt.Tx) error {
		for _, s := range segments {
			if !config.ValidID(s.CenterID) || !config.ValidID(s.CameraID) || s.Start.UnixMilli() < 0 ||
				s.DurationMS < 1 || s.DurationMS > MaxSegmentDuration.Milliseconds() {
				return fmt.Errorf("invalid recording index entry")
			}
			bucket, err := tx.Bucket(segmentsBucket).CreateBucketIfNotExists(partition(s.CenterID, s.CameraID, s.Start))
			if err != nil {
				return err
			}
			raw, err := json.Marshal(s)
			if err != nil {
				return err
			}
			if err := bucket.Put(recordKey(s.Start, s.Sequence), raw); err != nil {
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
		bucket := tx.Bucket(segmentsBucket).Bucket(partition(s.CenterID, s.CameraID, s.Start))
		if bucket == nil {
			return nil
		}
		return bucket.Delete(recordKey(s.Start, s.Sequence))
	})
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
