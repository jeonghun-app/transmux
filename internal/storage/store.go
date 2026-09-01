// Package storage abstracts the object store so the daemon can run against
// real S3, MinIO, or a local directory in tests without code changes.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound reports that the key does not exist. Callers distinguish this
// from a transport failure: a missing manifest means a brand new channel,
// whereas a failed read means the channel's history is unknown and must not
// be assumed empty.
var ErrNotFound = errors.New("object not found")

// Object is one upload request.
type Object struct {
	Key          string
	Body         []byte
	ContentType  string
	CacheControl string
	// Metadata is stored as S3 user metadata. It carries the per-segment
	// facts a future recording index needs and that cannot be recovered from
	// the object key alone, most importantly the real duration.
	Metadata map[string]string
}

// ObjectStore writes immutable segments and mutable manifests.
//
// Put must be idempotent for a given key: the retry path re-issues the same
// key rather than generating a new one, so a duplicate write is harmless.
type ObjectStore interface {
	Put(ctx context.Context, obj Object) error
	// Get returns the object body, or ErrNotFound if the key is absent.
	Get(ctx context.Context, key string) ([]byte, error)
	// Describe returns a loggable description with no credentials in it.
	Describe() string
}

// FilesystemStore writes objects under a root directory. Used by tests and
// by the offline development path.
//
// Metadata is not persisted by this backend; it exists to exercise the S3
// code path and is asserted on in unit tests through a fake store instead.
type FilesystemStore struct {
	root string
	mu   sync.Mutex
}

func NewFilesystemStore(root string) (*FilesystemStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &FilesystemStore{root: root}, nil
}

func (s *FilesystemStore) Describe() string { return "filesystem:" + s.root }

func (s *FilesystemStore) Put(_ context.Context, obj Object) error {
	if err := validateKey(obj.Key); err != nil {
		return err
	}
	dest := filepath.Join(s.root, filepath.FromSlash(obj.Key))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	// Write to a temp file and rename so a reader never observes a partial
	// manifest, mirroring the atomic-overwrite behaviour of S3.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(obj.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func (s *FilesystemStore) Get(_ context.Context, key string) ([]byte, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(key)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, err
	}
	return body, nil
}

// validateKey rejects keys that could escape the destination prefix.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty object key")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("object key %q must be relative", key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("object key %q contains an unsafe path element", key)
		}
	}
	return nil
}

// MetadataKeys returns the metadata field names in sorted order. Used for
// deterministic logging and test assertions.
func MetadataKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
