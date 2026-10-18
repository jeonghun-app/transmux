package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jeonghun-app/transmux/internal/config"
)

func testS3Store(t *testing.T, handler http.HandlerFunc) *S3Store {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "test-session-token")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	store, err := NewS3Store(context.Background(), config.StorageConfig{
		Bucket: "bucket", Region: "us-east-1", Endpoint: srv.URL, ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestS3PreservesTemporaryCredentialSessionToken(t *testing.T) {
	store := testS3Store(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Security-Token"); got != "test-session-token" {
			t.Errorf("temporary credential session token = %q", got)
		}
		if r.Header.Get("If-None-Match") != "*" {
			t.Error("create-only precondition was not sent")
		}
		w.Header().Set("ETag", `"version"`)
	})
	if _, err := store.Put(context.Background(), Object{
		Key: "c/cam/segment.ts", Body: []byte("video"), Preconditions: Preconditions{IfNoneMatch: true},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestS3RejectsTruncatedOrUnversionedControlObjects(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) {
			store := testS3Store(t, func(w http.ResponseWriter, r *http.Request) {
				if oversized {
					w.Header().Set("ETag", `"version"`)
					_, _ = w.Write([]byte(strings.Repeat("x", (8<<20)+1)))
					return
				}
				_, _ = w.Write([]byte("object without ETag"))
			})
			if _, _, err := store.Get(context.Background(), "c/cam/index.m3u8"); err == nil {
				t.Fatal("unsafe control object accepted")
			}
		})
	}
}

func TestS3ConditionalConflictIsMapped(t *testing.T) {
	for _, status := range []int{http.StatusPreconditionFailed, http.StatusConflict} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store := testS3Store(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-Match") != `"old"` {
					t.Error("compare-and-swap precondition was not sent")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte("<Error><Code>PreconditionFailed</Code></Error>"))
			})
			_, err := store.Put(context.Background(), Object{
				Key: "c/cam/index.m3u8", Body: []byte("playlist"), Preconditions: Preconditions{IfMatch: `"old"`},
			})
			if !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("conditional conflict was not mapped: %v", err)
			}
		})
	}
}

func TestS3DescriptionRedactsEndpointCredentials(t *testing.T) {
	store, err := NewS3Store(context.Background(), config.StorageConfig{
		Bucket: "bucket", Region: "us-east-1", Endpoint: "https://user:password@example.com?token=secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"user", "password", "secret"} {
		if strings.Contains(store.Describe(), secret) {
			t.Errorf("store description exposes %q", secret)
		}
	}
}

func TestS3LifecycleTagsStayOffControlObjects(t *testing.T) {
	for _, tc := range []struct {
		key  string
		tags map[string]string
		want string
	}{
		{"c/cam/seg.ts", RetentionTags(true, false), "transmux-kind=segment"},
		{"c/cam/init.mp4", RetentionTags(true, true), "transmux-kind=init"},
		{"c/cam/index.m3u8", nil, ""},
		{"c/cam/seg.ts", RetentionTags(false, false), ""},
	} {
		store := testS3Store(t, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-Amz-Tagging"); got != tc.want {
				t.Errorf("tags = %q, want %q", got, tc.want)
			}
			w.Header().Set("ETag", `"version"`)
		})
		if _, err := store.Put(context.Background(), Object{Key: tc.key, Body: []byte("data"), Tags: tc.tags}); err != nil {
			t.Fatal(err)
		}
	}
}

// s3Error answers the way S3 does when it refuses a request: an XML body whose
// Code is what the SDK surfaces as the API error code.
func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>refused</Message></Error>", code)
}

// TestS3CompareAndSwapOnAMissingKeyIsAPreconditionFailure pins the answer S3
// and RustFS give an If-Match write against a key that does not exist: 404
// NoSuchKey rather than 412. The version the caller swapped against is gone,
// which is the same answer as a stale ETag. The bare 404 case covers gateways
// that send no error body.
func TestS3CompareAndSwapOnAMissingKeyIsAPreconditionFailure(t *testing.T) {
	for name, code := range map[string]string{"NoSuchKey": "NoSuchKey", "bare": ""} {
		t.Run(name, func(t *testing.T) {
			store := testS3Store(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-Match") != `"gone"` {
					t.Error("compare-and-swap precondition was not sent")
				}
				if code == "" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				s3Error(w, http.StatusNotFound, code)
			})
			_, err := store.Put(context.Background(), Object{
				Key: "c/cam/_transmux/lease.json", Body: []byte("renewal"),
				Preconditions: Preconditions{IfMatch: `"gone"`},
			})
			if !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("If-Match on a missing key: err = %v, want ErrPreconditionFailed", err)
			}
		})
	}
}

// TestS3NotFoundIsNotAConflictWithoutAMissingKey keeps the mapping narrow. A
// 404 to a write that did not swap against a version says nothing about
// another writer, and a missing bucket is a configuration fault. Reporting
// either as a lost race would stop a channel, or tell an operator to reload a
// roster, for a problem that reloading cannot fix.
func TestS3NotFoundIsNotAConflictWithoutAMissingKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		pre  Preconditions
		code string
	}{
		{"unconditional", Preconditions{}, "NoSuchKey"},
		{"create-only", Preconditions{IfNoneMatch: true}, "NoSuchKey"},
		{"missing-bucket", Preconditions{IfMatch: `"version"`}, "NoSuchBucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testS3Store(t, func(w http.ResponseWriter, r *http.Request) {
				s3Error(w, http.StatusNotFound, tc.code)
			})
			_, err := store.Put(context.Background(), Object{
				Key: "c/cam/index.m3u8", Body: []byte("playlist"), Preconditions: tc.pre,
			})
			if err == nil {
				t.Fatal("a refused write reported success")
			}
			if errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("404 %s was reported as a conditional conflict: %v", tc.code, err)
			}
		})
	}
}
