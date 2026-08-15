// Package storage is the object store behind an interface.
//
// The interface exists so MinIO locally and S3 in production are a
// configuration change rather than a code change. It is also the seam where a
// CDN would go -- and that is not free: an S3 presigned URL addresses S3
// directly and therefore bypasses CloudFront, so fronting attachments with a
// CDN means switching to CloudFront signed URLs or cookies, with a different
// key and policy format. Real work, not a config flag.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/config"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type ObjectInfo struct {
	Size        int64
	ContentType string
	ETag        string
}

// Store is the object storage contract.
type Store interface {
	// PresignPut returns a URL the client uploads to directly.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	// PresignGet returns a short-lived download URL.
	PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (string, error)
	// Stat reports what was actually uploaded, which is how the declared size
	// gets verified.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

type s3Store struct {
	client *minio.Client
	bucket string
}

// New builds a client for either MinIO or S3.
func New(cfg config.StorageConfig) (Store, error) {
	endpoint := cfg.Endpoint
	secure := true

	if endpoint == "" {
		// No override means real AWS.
		endpoint = fmt.Sprintf("s3.%s.amazonaws.com", cfg.Region)
	} else {
		// MinIO locally, over plain HTTP.
		trimmed, isHTTP := trimScheme(endpoint)
		endpoint, secure = trimmed, !isHTTP
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
		// MinIO addresses buckets by path; real S3 uses virtual-host style.
		BucketLookup: bucketLookup(cfg.UsePathStyle),
	})
	if err != nil {
		return nil, fmt.Errorf("object storage client: %w", err)
	}
	return &s3Store{client: client, bucket: cfg.Bucket}, nil
}

func bucketLookup(pathStyle bool) minio.BucketLookupType {
	if pathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupDNS
}

func trimScheme(endpoint string) (string, bool) {
	switch {
	case len(endpoint) > 7 && endpoint[:7] == "http://":
		return endpoint[7:], true
	case len(endpoint) > 8 && endpoint[:8] == "https://":
		return endpoint[8:], false
	default:
		return endpoint, false
	}
}

func (s *s3Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	// A presigned PUT signs the method, key and expiry -- NOT the body length.
	// A client holding this URL can upload any number of bytes, which is why
	// the size cap is enforced by verifying the object at /complete rather
	// than here. See the note in the attachments handler.
	u, err := s.client.PresignedPutObject(ctx, s.bucket, key, ttl)
	if err != nil {
		return "", fmt.Errorf("presigning upload: %w", err)
	}
	return u.String(), nil
}

func (s *s3Store) PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (string, error) {
	params := make(map[string][]string)
	if filename != "" {
		// Makes the browser save under the original name rather than the
		// opaque object key.
		params["response-content-disposition"] = []string{
			fmt.Sprintf("attachment; filename=%q", filename),
		}
	}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, params)
	if err != nil {
		return "", fmt.Errorf("presigning download: %w", err)
	}
	return u.String(), nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		Size:        info.Size,
		ContentType: info.ContentType,
		ETag:        info.ETag,
	}, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// IsNotFound reports whether err means the object is absent, which is how an
// abandoned upload is detected at /complete.
func IsNotFound(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey"
}
