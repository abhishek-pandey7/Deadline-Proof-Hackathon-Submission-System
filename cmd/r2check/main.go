// Command r2check proves, against the real bucket, the property the storage
// cap relies on: a signed part URL accepts only the exact size it was signed
// for. It also reports what the bucket holds. It cleans up after itself.
//
//	go run ./cmd/r2check -env-file .env
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/config"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "r2check:", err)
		os.Exit(1)
	}
}

func put(req storage.SignedRequest, body []byte) (int, error) {
	r, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, v := range req.Headers {
		r.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

func run() error {
	envFile := flag.String("env-file", "", "KEY=VALUE file with the R2 settings")
	flag.Parse()
	if *envFile != "" {
		if err := config.LoadEnvFile(*envFile); err != nil {
			return err
		}
	}
	store, err := storage.New(storage.Config{
		Endpoint:        os.Getenv("R2_ENDPOINT"),
		Region:          os.Getenv("R2_REGION"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("R2_BUCKET"),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	key := "selfcheck/" + hex.EncodeToString(b)

	uploadID, err := store.CreateMultipart(ctx, key)
	if err != nil {
		return fmt.Errorf("credentials or bucket wrong? %w", err)
	}
	defer store.AbortMultipart(context.Background(), key, uploadID) //nolint:errcheck

	req, err := store.PresignPart(ctx, key, uploadID, 1, 1024, time.Minute)
	if err != nil {
		return err
	}
	over, err := put(req, make([]byte, 4096))
	if err != nil {
		return fmt.Errorf("upload to R2 failed: %w", err)
	}
	exact, err := put(req, make([]byte, 1024))
	if err != nil {
		return fmt.Errorf("upload to R2 failed: %w", err)
	}
	parts, err := store.ListParts(ctx, key, uploadID)
	if err != nil {
		return err
	}
	if err := store.AbortMultipart(ctx, key, uploadID); err != nil {
		return err
	}
	bytesUsed, objects, err := store.Measure(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("bucket %q reachable with these keys\n", store.Bucket())
	fmt.Printf("4096 bytes on a URL signed for 1024: HTTP %d (must be refused)\n", over)
	fmt.Printf("1024 bytes on the same URL:          HTTP %d (must be 200)\n", exact)
	fmt.Printf("parts stored: %d, test upload aborted\n", len(parts))
	fmt.Printf("bucket holds %d bytes in %d objects\n", bytesUsed, objects)
	if over < 400 || exact != http.StatusOK || len(parts) != 1 || parts[0].Size != 1024 {
		return fmt.Errorf("R2 does NOT enforce the signed size; do not deploy until this is fixed")
	}
	fmt.Println("PASS: R2 enforces the signed part size")
	return nil
}
