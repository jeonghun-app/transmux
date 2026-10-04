// Package storage abstracts the object store so the daemon can run against
// real S3, MinIO, or a local directory in tests without code changes.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// ErrNotFound reports that the key does not exist. Callers distinguish this
// from a transport failure: a missing manifest means a brand new channel,
// whereas a failed read means the channel's history is unknown and must not
// be assumed empty.
var ErrNotFound = errors.New("object not found")

// ErrPreconditionFailed reports that a conditional write was refused because
// the object was not in the state the caller expected.
//
// This is a semantic answer, not a fault. It means another writer got there
// first, so it must never be retried: the precondition cannot become true by
// waiting. Callers treat it as loss of ownership.
var ErrPreconditionFailed = errors.New("object precondition failed")

// Preconditions makes a write conditional on the current state of the key.
//
// The two are mutually exclusive. IfNoneMatch is create-only semantics, used
// for immutable segments and for claiming a control object. IfMatch is
// compare-and-swap, used for the manifest and for lease renewal.
type Preconditions struct {
	// IfMatch requires the stored object to have exactly this ETag.
	IfMatch string
	// IfNoneMatch requires the key to be absent.
	IfNoneMatch bool
}

// Conditional reports whether the write carries a precondition.
//
// It matters to the retry policy: a conditional write must never be retried
// after an ambiguous failure. If the first attempt landed and only its
// response was lost, the retry sees its own precondition already consumed and
// returns ErrPreconditionFailed -- indistinguishable from being overtaken by
// another writer. Only the caller, which knows the write ID it used, can
// resolve that.
func (p Preconditions) Conditional() bool { return p.IfMatch != "" || p.IfNoneMatch }

func (p Preconditions) validate() error {
	if p.IfMatch != "" && p.IfNoneMatch {
		return fmt.Errorf("If-Match and If-None-Match are mutually exclusive")
	}
	return nil
}

// ControlClient is the small-object interface used for ownership leases.
// A coordinator can supply it with independent concurrency so slow segment
// uploads cannot consume every slot needed to renew ownership.
type ControlClient interface {
	Put(context.Context, Object) (string, error)
	Get(context.Context, string) ([]byte, string, error)
}

// ObjectInfo is what the store knows about a key without its body.
type ObjectInfo struct {
	// ETag is an opaque version token. It must never be interpreted as a
	// checksum: encryption and gateways are both free to change its form.
	ETag     string
	Size     int64
	Metadata map[string]string
}

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
	// Tags are S3 object tags (distinct from user metadata).
	Tags map[string]string
	// Preconditions, when set, make the write conditional.
	Preconditions Preconditions
}

// ObjectStore writes immutable segments and mutable manifests.
//
// Put is idempotent for a given key when unconditional: the retry path
// re-issues the same key rather than generating a new one, so a duplicate
// write is harmless. A conditional Put is NOT idempotent, because a retry
// after an ambiguous success will see the precondition already consumed and
// return ErrPreconditionFailed. Callers of conditional writes must resolve
// ambiguity by reading back, never by retrying blindly.
type ObjectStore interface {
	// Put stores the object and returns its new ETag. It returns
	// ErrPreconditionFailed when Preconditions are not met.
	Put(ctx context.Context, obj Object) (string, error)
	// Get returns the object body and its ETag, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, string, error)
	// Head returns what is known about a key without transferring the body.
	// It is how an ambiguous conditional write is resolved.
	Head(ctx context.Context, key string) (ObjectInfo, error)
	// Describe returns a loggable description with no credentials in it.
	Describe() string
}

// FilesystemStore writes objects under a root directory. Used by tests and
// by the offline development path.
//
// It emulates the object store's conditional-write semantics rather than
// merely accepting the fields, because the ownership protocol is built on
// them: a backend that silently ignored a precondition would make the tests
// pass for the wrong reason. The ETag is the SHA-256 of the body, and the
// check-and-write is serialised with a lock file so that two store instances
// sharing one root behave like two processes against one bucket, which is
// exactly the case being modelled.
//
// Metadata is persisted in a sidecar so Head can resolve an ambiguous write
// after a restart.
type FilesystemStore struct {
	root string
	gate chan struct{}
}

func NewFilesystemStore(root string) (*FilesystemStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &FilesystemStore{root: root, gate: make(chan struct{}, 1)}, nil
}

func (s *FilesystemStore) Describe() string { return "filesystem:" + s.root }

// etagOf is the content addressing this backend uses as its version token.
func etagOf(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *FilesystemStore) sidecar(dest string) string {
	return filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".meta")
}

// lock serialises conditional writes across every process sharing the root.
// An in-process mutex is not enough: the whole point of the precondition is
// to arbitrate between two daemons.
func (s *FilesystemStore) lock(ctx context.Context) (func(), error) {
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	path := filepath.Join(s.root, ".transmux-cas.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		<-s.gate
		return nil, err
	}
	for {
		err = ctx.Err()
		if err == nil {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		}
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			timer := time.NewTimer(5 * time.Millisecond)
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		f.Close()
		<-s.gate
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		<-s.gate
	}, nil
}

func (s *FilesystemStore) Put(ctx context.Context, obj Object) (string, error) {
	if err := validateKey(obj.Key); err != nil {
		return "", err
	}
	if err := obj.Preconditions.validate(); err != nil {
		return "", err
	}
	dest := filepath.Join(s.root, filepath.FromSlash(obj.Key))

	unlock, err := s.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()

	if obj.Preconditions.Conditional() {
		current, err := os.ReadFile(dest)
		switch {
		case err == nil:
			if obj.Preconditions.IfNoneMatch {
				return "", fmt.Errorf("%w: %s exists", ErrPreconditionFailed, obj.Key)
			}
			if got := etagOf(current); got != obj.Preconditions.IfMatch {
				return "", fmt.Errorf("%w: %s has etag %s, expected %s",
					ErrPreconditionFailed, obj.Key, got, obj.Preconditions.IfMatch)
			}
		case os.IsNotExist(err):
			if obj.Preconditions.IfMatch != "" {
				return "", fmt.Errorf("%w: %s is absent", ErrPreconditionFailed, obj.Key)
			}
		default:
			return "", err
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", err
	}
	// Write to a temp file and rename so a reader never observes a partial
	// manifest, mirroring the atomic-overwrite behaviour of S3.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".put-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(obj.Body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", err
	}
	if len(obj.Metadata) > 0 {
		raw, err := json.Marshal(obj.Metadata)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(s.sidecar(dest), raw, 0o600); err != nil {
			return "", err
		}
	} else if err := os.Remove(s.sidecar(dest)); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return etagOf(obj.Body), nil
}

func (s *FilesystemStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := validateKey(key); err != nil {
		return nil, "", err
	}
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(key)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, "", err
	}
	defer f.Close()
	body, err := readControlBody(f)
	if err != nil {
		return nil, "", err
	}
	return body, etagOf(body), nil
}

func (s *FilesystemStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer unlock()
	dest := filepath.Join(s.root, filepath.FromSlash(key))
	body, err := os.ReadFile(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return ObjectInfo{}, err
	}
	info := ObjectInfo{ETag: etagOf(body), Size: int64(len(body))}
	if raw, err := os.ReadFile(s.sidecar(dest)); err == nil {
		md := map[string]string{}
		if err := json.Unmarshal(raw, &md); err != nil {
			return ObjectInfo{}, fmt.Errorf("read object metadata: %w", err)
		}
		info.Metadata = md
	} else if !os.IsNotExist(err) {
		return ObjectInfo{}, err
	}
	return info, nil
}

// validateKey rejects keys that could escape the destination prefix.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty object key")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("object key %q must be relative", key)
	}
	if strings.Contains(key, `\`) || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return fmt.Errorf("object key contains an unsafe character")
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("object key %q contains an unsafe path element", key)
		}
	}
	return nil
}

// Read one byte beyond the bound so a valid prefix of an oversized object
// can never be mistaken for the complete ownership or recovery record.
func readControlBody(r io.Reader) ([]byte, error) {
	const maxControlBytes = 8 << 20
	body, err := io.ReadAll(io.LimitReader(r, maxControlBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxControlBytes {
		return nil, fmt.Errorf("control object exceeds %d bytes", maxControlBytes)
	}
	return body, nil
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

func RetentionTags(enabled bool, initialization bool) map[string]string {
	if !enabled {
		return nil
	}
	kind := "segment"
	if initialization {
		kind = "init"
	}
	return map[string]string{"transmux-kind": kind}
}
