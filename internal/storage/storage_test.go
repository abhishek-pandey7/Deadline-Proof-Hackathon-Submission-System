package storage_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/teststore"
)

func put(t *testing.T, req storage.SignedRequest, body []byte) int {
	t.Helper()
	r, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range req.Headers {
		r.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// The size limit on a signed part URL is what stops anyone from pushing more
// bytes than they declared, so it is tested directly against storage.
func TestSignedPartURLOnlyAcceptsTheSignedSize(t *testing.T) {
	store := teststore.New(t)
	ctx := context.Background()

	uploadID, err := store.CreateMultipart(ctx, "obj")
	if err != nil {
		t.Fatal(err)
	}
	req, err := store.PresignPart(ctx, "obj", uploadID, 1, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if code := put(t, req, make([]byte, 4096)); code < 400 {
		t.Fatalf("oversized body accepted with status %d", code)
	}
	if code := put(t, req, make([]byte, 1024)); code != http.StatusOK {
		t.Fatalf("correctly sized body got status %d", code)
	}

	parts, err := store.ListParts(ctx, "obj", uploadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Size != 1024 {
		t.Fatalf("parts = %+v, want one 1024-byte part", parts)
	}
}

func TestMultipartRoundTrip(t *testing.T) {
	store := teststore.New(t)
	ctx := context.Background()

	const partSize = 5 << 20 // S3's minimum for every part but the last
	first := bytes.Repeat([]byte{'a'}, partSize)
	last := []byte("tail")

	uploadID, err := store.CreateMultipart(ctx, "file")
	if err != nil {
		t.Fatal(err)
	}
	for i, body := range [][]byte{first, last} {
		req, err := store.PresignPart(ctx, "file", uploadID, int32(i+1), int64(len(body)), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if code := put(t, req, body); code != http.StatusOK {
			t.Fatalf("part %d: status %d", i+1, code)
		}
	}
	parts, err := store.ListParts(ctx, "file", uploadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteMultipart(ctx, "file", uploadID, parts); err != nil {
		t.Fatal(err)
	}
	size, err := store.Size(ctx, "file")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(partSize + len(last)); size != want {
		t.Fatalf("size = %d, want %d", size, want)
	}
	if _, err := store.ListParts(ctx, "file", uploadID); err != storage.ErrNoSuchUpload {
		t.Fatalf("list parts after complete: err = %v, want ErrNoSuchUpload", err)
	}
}
