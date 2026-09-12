package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/jeonghun-app/transmux/internal/config"
)

// S3Store uploads with PutObject.
//
// Multipart upload is deliberately not used. A 5 second segment at 2-8 Mbps
// is roughly 1.25-5 MB, which is at or below the 5 MiB minimum size for a
// non-final multipart part, so multipart would add an extra CreateMultipart
// and CompleteMultipart round trip per segment plus abandoned-upload cleanup
// for no benefit.
type S3Store struct {
	client   *s3.Client
	bucket   string
	describe string
}

func NewS3Store(ctx context.Context, cfg config.StorageConfig) (*S3Store, error) {
	loadOpts := []func(*awscfg.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awscfg.WithRegion(cfg.Region))
	}
	// The SDK chain supports ECS/IRSA as well as development environment
	// credentials, including AWS_SESSION_TOKEN for temporary credentials.
	// Rebuilding a static provider from just key/secret discards that token.
	// Retries are handled by the upload coordinator so that attempts,
	// backoff and metrics stay in one place.
	loadOpts = append(loadOpts, awscfg.WithRetryMaxAttempts(1))

	awsCfg, err := awscfg.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) { o.BaseEndpoint = aws.String(cfg.Endpoint) })
	}
	if cfg.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) { o.UsePathStyle = true })
	}

	describe := fmt.Sprintf("s3://%s region=%s", cfg.Bucket, awsCfg.Region)
	if cfg.Endpoint != "" {
		if u, err := url.Parse(cfg.Endpoint); err == nil {
			u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
			u.ForceQuery = false
			describe += " endpoint=" + u.String()
		} else {
			describe += " endpoint=<invalid>"
		}
	}
	return &S3Store{
		client:   s3.NewFromConfig(awsCfg, s3Opts...),
		bucket:   cfg.Bucket,
		describe: describe,
	}, nil
}

func (s *S3Store) Describe() string { return s.describe }

func (s *S3Store) Put(ctx context.Context, obj Object) (string, error) {
	if err := validateKey(obj.Key); err != nil {
		return "", err
	}
	if err := obj.Preconditions.validate(); err != nil {
		return "", err
	}
	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(obj.Key),
		Body:          bytes.NewReader(obj.Body),
		ContentLength: aws.Int64(int64(len(obj.Body))),
	}
	if obj.ContentType != "" {
		in.ContentType = aws.String(obj.ContentType)
	}
	if obj.CacheControl != "" {
		in.CacheControl = aws.String(obj.CacheControl)
	}
	if len(obj.Metadata) > 0 {
		in.Metadata = obj.Metadata
	}
	if len(obj.Tags) > 0 {
		tags := url.Values{}
		for key, value := range obj.Tags {
			tags.Set(key, value)
		}
		in.Tagging = aws.String(tags.Encode())
	}
	// Conditional writes are what make single-writer ownership enforceable.
	// If-None-Match claims a key that must not already exist; If-Match is a
	// compare-and-swap against the version this caller last observed.
	if obj.Preconditions.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	} else if obj.Preconditions.IfMatch != "" {
		in.IfMatch = aws.String(obj.Preconditions.IfMatch)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		if isPreconditionFailed(err) {
			return "", fmt.Errorf("%w: %s", ErrPreconditionFailed, obj.Key)
		}
		return "", fmt.Errorf("s3 put %s: %w", obj.Key, redactRequestID(err))
	}
	if etag := aws.ToString(out.ETag); etag != "" {
		return etag, nil
	}
	return "", fmt.Errorf("s3 put %s returned no ETag", obj.Key)
}

// isPreconditionFailed recognises a refused conditional write.
//
// The typed error differs between S3 and S3-compatible gateways, so the HTTP
// status is the reliable signal. 409 is included because S3 answers a race
// between two conditional creates with ConditionalRequestConflict rather than
// PreconditionFailed; both mean the same thing to this daemon, which is that
// another writer got there first.
func isPreconditionFailed(err error) bool {
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.HTTPStatusCode() {
		case 412, 409:
			return true
		}
	}
	return false
}

// Get reads an object, mapping a missing key to ErrNotFound.
//
// This exists so a restarted channel can recover its published media
// sequence from the manifest already in the store. Without it, a container
// replacement that loses the local checkpoint would restart the sequence at
// zero, rewinding EXT-X-MEDIA-SEQUENCE and breaking every player.
//
// The ETag is returned because it is the token the caller needs to write the
// next version conditionally, which is how a stale writer is fenced out.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := validateKey(key); err != nil {
		return nil, "", err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, "", fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, "", fmt.Errorf("s3 get %s: %w", key, redactRequestID(err))
	}
	defer out.Body.Close()
	// Manifests are small; cap the read so a wrong key cannot exhaust memory.
	body, err := readControlBody(out.Body)
	if err != nil {
		return nil, "", fmt.Errorf("s3 get %s body: %w", key, err)
	}
	if aws.ToString(out.ETag) == "" {
		return nil, "", fmt.Errorf("s3 get %s returned no ETag", key)
	}
	return body, aws.ToString(out.ETag), nil
}

// Head resolves an ambiguous conditional write: it says what is at the key
// now, and whose write put it there, without transferring the body.
func (s *S3Store) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return ObjectInfo{}, fmt.Errorf("s3 head %s: %w", key, redactRequestID(err))
	}
	return ObjectInfo{
		ETag:     aws.ToString(out.ETag),
		Size:     aws.ToInt64(out.ContentLength),
		Metadata: out.Metadata,
	}, nil
}

// isNotFound covers both the typed errors and the bare 404 that MinIO and
// some gateways answer a missing key with.
func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	var nf *s3types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var respErr *awshttp.ResponseError
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404
}

// redactRequestID keeps the useful part of an SDK error without dumping the
// full signed request into logs.
//
// A transport failure carries no HTTP status, so the status-code branch is
// skipped in that case; reporting "http 0" would send an operator looking for
// a server-side cause that does not exist.
func redactRequestID(err error) error {
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() != 0 {
		return fmt.Errorf("http %d (request-id %s)", respErr.HTTPStatusCode(), respErr.ServiceRequestID())
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Strips the presigned query string, which can contain the signature.
		return fmt.Errorf("%s failed: %v", urlErr.Op, urlErr.Err)
	}
	return err
}
