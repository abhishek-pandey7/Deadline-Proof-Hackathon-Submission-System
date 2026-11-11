// Package storage wraps the S3 API used for submission files. Production
// talks to Cloudflare R2, development and tests to MinIO.
package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Config locates the bucket and the credentials for it.
type Config struct {
	Endpoint        string
	Region          string // R2 wants "auto"
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
}

// Part is one uploaded chunk of a multipart upload, as storage reports it.
type Part struct {
	Number int32
	Size   int64
	ETag   string
}

// SignedRequest is a presigned request the browser sends as-is.
type SignedRequest struct {
	Method string
	URL    string
	// Headers the browser must send. Host and Content-Length are left out:
	// browsers set those themselves.
	Headers map[string]string
}

// ErrNoSuchUpload means the multipart upload is gone (completed or aborted).
var ErrNoSuchUpload = errors.New("storage: no such multipart upload")

// ErrNotFound means the object does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Store talks to one bucket.
type Store struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// New builds a Store. It does not contact the server.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" || cfg.Bucket == "" {
		return nil, errors.New("storage: endpoint, access key, secret and bucket are required")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		// Newer SDKs add CRC checksums to every upload by default. Browsers
		// uploading through presigned URLs cannot compute them, and R2 does not
		// need them, so only send checksums when an operation requires one.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 &http.Client{Timeout: 60 * time.Second},
	})
	return &Store{client: client, presign: s3.NewPresignClient(client), bucket: cfg.Bucket}, nil
}

// Bucket returns the bucket name.
func (s *Store) Bucket() string { return s.bucket }

// EnsureBucket creates the bucket if it is missing. Development and tests only;
// production buckets are made in the R2 dashboard.
func (s *Store) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if err == nil {
		return nil
	}
	if _, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket}); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		if errors.As(err, &owned) {
			return nil
		}
		return fmt.Errorf("storage: create bucket: %w", err)
	}
	return nil
}

// CreateMultipart starts a multipart upload and returns its id.
func (s *Store) CreateMultipart(ctx context.Context, key string) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      &s.bucket,
		Key:         &key,
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return "", fmt.Errorf("storage: create multipart: %w", err)
	}
	return aws.ToString(out.UploadId), nil
}

// PresignPart signs a PUT for one part. Content-Length is part of the
// signature, so storage refuses a body of any other size.
func (s *Store) PresignPart(ctx context.Context, key, uploadID string, part int32, size int64, ttl time.Duration) (SignedRequest, error) {
	req, err := s.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:        &s.bucket,
		Key:           &key,
		UploadId:      &uploadID,
		PartNumber:    aws.Int32(part),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return SignedRequest{}, fmt.Errorf("storage: presign part %d: %w", part, err)
	}
	headers := map[string]string{}
	for name, values := range req.SignedHeader {
		switch http.CanonicalHeaderKey(name) {
		case "Host", "Content-Length":
			continue
		}
		if len(values) > 0 {
			headers[name] = values[0]
		}
	}
	return SignedRequest{Method: req.Method, URL: req.URL, Headers: headers}, nil
}

// ListParts returns the parts storage has received, sorted by number.
func (s *Store) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	var parts []Part
	p := s3.NewListPartsPaginator(s.client, &s3.ListPartsInput{Bucket: &s.bucket, Key: &key, UploadId: &uploadID})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if isCode(err, "NoSuchUpload") {
				return nil, ErrNoSuchUpload
			}
			return nil, fmt.Errorf("storage: list parts: %w", err)
		}
		for _, pt := range page.Parts {
			parts = append(parts, Part{Number: aws.ToInt32(pt.PartNumber), Size: aws.ToInt64(pt.Size), ETag: aws.ToString(pt.ETag)})
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// CompleteMultipart assembles the parts into the final object.
func (s *Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	completed := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: aws.String(p.ETag)}
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          &s.bucket,
		Key:             &key,
		UploadId:        &uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		if isCode(err, "NoSuchUpload") {
			return ErrNoSuchUpload
		}
		return fmt.Errorf("storage: complete multipart: %w", err)
	}
	return nil
}

// AbortMultipart discards an upload and the parts stored for it.
func (s *Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &key, UploadId: &uploadID})
	if err != nil && !isCode(err, "NoSuchUpload") {
		return fmt.Errorf("storage: abort multipart: %w", err)
	}
	return nil
}

// Size returns an object's size.
func (s *Store) Size(ctx context.Context, key string) (int64, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if isCode(err, "NotFound") || isCode(err, "NoSuchKey") {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("storage: head object: %w", err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// Delete removes an object. Deleting a missing object is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key}); err != nil && !isCode(err, "NoSuchKey") {
		return fmt.Errorf("storage: delete: %w", err)
	}
	return nil
}

// PresignGet signs a short-lived download that names the file for the browser.
func (s *Store) PresignGet(ctx context.Context, key, fileName string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     &s.bucket,
		Key:                        &key,
		ResponseContentDisposition: aws.String(fmt.Sprintf("attachment; filename=%q", fileName)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage: presign get: %w", err)
	}
	return req.URL, nil
}

// Client exposes the raw S3 client for workers that stream objects.
func (s *Store) Client() *s3.Client { return s.client }

func isCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}
