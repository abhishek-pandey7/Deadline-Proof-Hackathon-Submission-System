// Package checks runs the background checks on a completed version: the
// file matches its declared hash, its type comes from its content, archives
// are not bombs, links respond, and a preview exists where one can be made.
package checks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
)

// Check names, as stored in check_results.
const (
	CheckIntegrity = "integrity"
	CheckFileType  = "file_type"
	CheckArchive   = "archive"
	CheckLinks     = "links"
	CheckPreview   = "preview"
)

// Target is the version being checked.
type Target struct {
	VersionID string
	ObjectKey string
	FileName  string
	FileSize  int64
	SHA256    string // hex
	Links     []string
}

// Result is one check's outcome. Detail is stored as JSON.
type Result struct {
	Name   string
	Passed bool
	Detail any
}

// Store is the storage the checks read from and write previews to.
type Store interface {
	Download(ctx context.Context, key string, w io.Writer) (int64, error)
	Put(ctx context.Context, key, contentType string, body []byte) error
}

// Runner runs every check on a target.
type Runner struct {
	Store   Store
	Links   *LinkChecker
	PDF     PDFRenderer // nil when no renderer is installed
	Archive ArchiveLimits
	TempDir string // "" means the OS default
}

// Run downloads the file once (hashing it on the way) and runs every check.
// It returns an error only when the checks could not run at all, such as
// storage being unreachable; River then retries the job.
func (r *Runner) Run(ctx context.Context, t Target) ([]Result, error) {
	f, err := os.CreateTemp(r.TempDir, "check-*")
	if err != nil {
		return nil, fmt.Errorf("checks: temp file: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	h := sha256.New()
	n, err := r.Store.Download(ctx, t.ObjectKey, io.MultiWriter(f, h))
	if errors.Is(err, storage.ErrNotFound) {
		return []Result{{Name: CheckIntegrity, Passed: false, Detail: map[string]any{"reason": "the stored file is missing"}}}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("checks: flush temp file: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	results := []Result{{
		Name:   CheckIntegrity,
		Passed: n == t.FileSize && sum == t.SHA256,
		Detail: map[string]any{"expected_sha256": t.SHA256, "actual_sha256": sum, "expected_size": t.FileSize, "actual_size": n},
	}}

	kind, typeResult := checkFileType(f.Name(), t.FileName)
	results = append(results, typeResult)

	var readme *Readme
	if kind.isZip {
		archiveResult, found := inspectZip(f.Name(), r.Archive)
		results = append(results, archiveResult)
		readme = found
	}

	results = append(results, r.Links.CheckAll(ctx, t.Links))
	results = append(results, r.preview(ctx, t, f.Name(), kind, readme))
	return results, nil
}

// Blocking reports whether a failed result should send the version to
// "needs fix". A missing preview never does.
func Blocking(res Result) bool { return !res.Passed && res.Name != CheckPreview }
