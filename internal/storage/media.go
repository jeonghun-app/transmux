package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jeonghun-app/transmux/internal/config"
)

var ErrRange = errors.New("unsatisfiable byte range")

// MediaStore extends the ingest interface without buffering video in memory.
// Listing uses a lexical cursor so a background indexer can resume after a
// crash. Only the delivery/index process needs list and delete permissions.
type MediaStore interface {
	ObjectStore
	Open(ctx context.Context, key, byteRange string) (Stream, error)
	List(ctx context.Context, prefix, after string, limit int) (Page, error)
	Delete(ctx context.Context, key string) error
}

type Stream struct {
	Body         io.ReadCloser
	Size         int64
	ETag         string
	ContentRange string
	Modified     time.Time
}

type Entry struct {
	Key      string
	Size     int64
	Modified time.Time
}

type Page struct {
	Entries []Entry
	More    bool
}

func New(ctx context.Context, cfg config.StorageConfig) (MediaStore, error) {
	switch cfg.Backend {
	case "s3":
		return NewS3Store(ctx, cfg)
	case "filesystem":
		return NewFilesystemStore(cfg.Root)
	default:
		return nil, fmt.Errorf("unsupported storage backend %q", cfg.Backend)
	}
}

// CheckRange accepts a single RFC 9110 byte range; multipart ranges are
// deliberately unsupported. S3 applies the range to the actual object size.
func CheckRange(raw string) error {
	if raw == "" {
		return nil
	}
	if !strings.HasPrefix(raw, "bytes=") || len(raw) > 80 {
		return ErrRange
	}
	parts := strings.Split(strings.TrimPrefix(raw, "bytes="), "-")
	if len(parts) != 2 || (parts[0] == "" && parts[1] == "") {
		return ErrRange
	}
	var values [2]int64
	for i, p := range parts {
		if p == "" {
			continue
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return ErrRange
			}
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return ErrRange
		}
		values[i] = n
	}
	if parts[0] == "" && values[1] == 0 ||
		parts[0] != "" && parts[1] != "" && values[0] > values[1] {
		return ErrRange
	}
	return nil
}

func (s *S3Store) Open(ctx context.Context, key, byteRange string) (Stream, error) {
	if err := validateKey(key); err != nil {
		return Stream{}, err
	}
	if err := CheckRange(byteRange); err != nil {
		return Stream{}, err
	}
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if byteRange != "" {
		in.Range = aws.String(byteRange)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		if isNotFound(err) {
			return Stream{}, ErrNotFound
		}
		var status interface{ HTTPStatusCode() int }
		if errors.As(err, &status) && status.HTTPStatusCode() == 416 {
			return Stream{}, ErrRange
		}
		return Stream{}, fmt.Errorf("open media: %w", redactRequestID(err))
	}
	return Stream{Body: out.Body, Size: aws.ToInt64(out.ContentLength),
		ETag: aws.ToString(out.ETag), ContentRange: aws.ToString(out.ContentRange),
		Modified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3Store) List(ctx context.Context, prefix, after string, limit int) (Page, error) {
	if err := validateList(prefix, limit); err != nil {
		return Page{}, err
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix),
		MaxKeys: aws.Int32(int32(limit))}
	if after != "" {
		in.StartAfter = aws.String(after)
	}
	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return Page{}, fmt.Errorf("list media: %w", redactRequestID(err))
	}
	page := Page{More: aws.ToBool(out.IsTruncated)}
	for _, obj := range out.Contents {
		page.Entries = append(page.Entries, Entry{Key: aws.ToString(obj.Key),
			Size: aws.ToInt64(obj.Size), Modified: aws.ToTime(obj.LastModified)})
	}
	return page, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete media: %w", redactRequestID(err))
	}
	return nil
}

type sectionCloser struct {
	io.Reader
	io.Closer
}

func (s *FilesystemStore) Open(ctx context.Context, key, byteRange string) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return Stream{}, err
	}
	if err := validateKey(key); err != nil {
		return Stream{}, err
	}
	if err := CheckRange(byteRange); err != nil {
		return Stream{}, err
	}
	f, err := os.OpenInRoot(s.root, filepath.FromSlash(key))
	if os.IsNotExist(err) {
		return Stream{}, ErrNotFound
	}
	if err != nil {
		return Stream{}, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return Stream{}, err
	}
	if !info.Mode().IsRegular() {
		return Stream{}, ErrNotFound
	}
	out := Stream{Body: f, Size: info.Size(), Modified: info.ModTime()}
	if byteRange != "" {
		parts := strings.Split(strings.TrimPrefix(byteRange, "bytes="), "-")
		start, end := int64(0), info.Size()-1
		if parts[0] == "" {
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			start = max(0, info.Size()-n)
		} else {
			start, _ = strconv.ParseInt(parts[0], 10, 64)
			if parts[1] != "" {
				n, _ := strconv.ParseInt(parts[1], 10, 64)
				end = min(end, n)
			}
		}
		if start > end {
			return Stream{}, ErrRange
		}
		out.Size = end - start + 1
		out.Body = sectionCloser{Reader: io.NewSectionReader(f, start, out.Size), Closer: f}
		out.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size())
	}
	ok = true
	return out, nil
}

func validateList(prefix string, limit int) error {
	if limit < 1 || limit > 1000 {
		return fmt.Errorf("list limit must be between 1 and 1000")
	}
	return validateKey(strings.TrimSuffix(prefix, "/"))
}

func (s *FilesystemStore) List(ctx context.Context, prefix, after string, limit int) (Page, error) {
	if err := validateList(prefix, limit); err != nil {
		return Page{}, err
	}
	var entries []Entry
	base := filepath.Join(s.root, filepath.FromSlash(strings.TrimSuffix(prefix, "/")))
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if key <= after || !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, Entry{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	page := Page{More: len(entries) > limit}
	page.Entries = entries[:min(limit, len(entries))]
	return page, nil
}

func (s *FilesystemStore) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(filepath.FromSlash(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	sidecar, err := filepath.Rel(s.root, s.sidecar(filepath.Join(s.root, filepath.FromSlash(key))))
	if err != nil {
		return err
	}
	if err := root.Remove(sidecar); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
