package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
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
	// Static credentials are only used for MinIO/LocalStack in development.
	// In production the task role (ECS) or IRSA (EKS) supplies credentials
	// through the default chain and these variables are unset.
	if id, secret := devCredentials(); id != "" && secret != "" {
		loadOpts = append(loadOpts, awscfg.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(id, secret, "")))
	}
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
		describe += " endpoint=" + cfg.Endpoint
	}
	return &S3Store{
		client:   s3.NewFromConfig(awsCfg, s3Opts...),
		bucket:   cfg.Bucket,
		describe: describe,
	}, nil
}

func (s *S3Store) Describe() string { return s.describe }

func (s *S3Store) Put(ctx context.Context, obj Object) error {
	if err := validateKey(obj.Key); err != nil {
		return err
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
	if _, err := s.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3 put %s: %w", obj.Key, redactRequestID(err))
	}
	return nil
}

// Get reads an object, mapping a missing key to ErrNotFound.
//
// This exists so a restarted channel can recover its published media
// sequence from the manifest already in the store. Without it, a container
// replacement that loses the local checkpoint would restart the sequence at
// zero, rewinding EXT-X-MEDIA-SEQUENCE and breaking every player.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		var nf *s3types.NotFound
		if errors.As(err, &nsk) || errors.As(err, &nf) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		// MinIO and some gateways answer a missing key with a bare 404
		// rather than a typed error.
		var respErr *awshttp.ResponseError
		if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404 {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, redactRequestID(err))
	}
	defer out.Body.Close()
	// Manifests are small; cap the read so a wrong key cannot exhaust memory.
	body, err := io.ReadAll(io.LimitReader(out.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("s3 get %s body: %w", key, err)
	}
	return body, nil
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

// devCredentials reads the static keys used to talk to MinIO or LocalStack.
// Production deployments leave these unset and rely on the ECS task role or
// EKS IRSA credentials resolved by the default chain.
func devCredentials() (string, string) {
	return os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
}
