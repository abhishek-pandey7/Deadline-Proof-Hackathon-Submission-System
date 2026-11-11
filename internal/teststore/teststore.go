// Package teststore gives each test its own bucket on the local MinIO.
package teststore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
)

// New creates an empty bucket and returns a Store for it. Everything in the
// bucket is removed when the test ends. R2_ENDPOINT, R2_ACCESS_KEY_ID and
// R2_SECRET_ACCESS_KEY must point at a disposable S3 server (MinIO); without
// them the test is skipped locally and fails in CI.
func New(t testing.TB) *storage.Store {
	t.Helper()
	endpoint := os.Getenv("R2_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("R2_ENDPOINT must be set in CI")
		}
		t.Skip("R2_ENDPOINT not set; start MinIO with `docker compose up -d minio`")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	store, err := storage.New(storage.Config{
		Endpoint:        endpoint,
		Region:          os.Getenv("R2_REGION"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          "test-" + hex.EncodeToString(b),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("teststore: %v", err)
	}
	t.Cleanup(func() { empty(t, store) })
	return store
}

func empty(t testing.TB, store *storage.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, bucket := store.Client(), aws.String(store.Bucket())

	if ups, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket}); err == nil {
		for _, u := range ups.Uploads {
			_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: bucket, Key: u.Key, UploadId: u.UploadId})
		}
	}
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: bucket})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			break
		}
		for _, o := range page.Contents {
			_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: o.Key})
		}
	}
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bucket}); err != nil {
		t.Logf("teststore: delete bucket %s: %v", store.Bucket(), err)
	}
}
