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
