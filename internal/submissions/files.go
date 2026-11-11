package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/jobs"
)

// DownloadTTL is how long a signed download link works.
const DownloadTTL = 60 * time.Second

// Link is a short-lived signed URL.
type Link struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DownloadURL signs a 60-second download of a version's file for a member.
func (s *Service) DownloadURL(ctx context.Context, userID, versionID string) (Link, error) {
	v, err := s.load(ctx, userID, versionID)
	if err != nil {
		return Link{}, err
	}
	switch v.State {
	case "checking", "accepted", "needs_fix":
	default:
		return Link{}, &Error{Code: CodeWrongState, Message: "this version has no stored file", Details: map[string]any{"state": v.State}}
	}
	url, err := s.store.PresignGet(ctx, v.objectKey, v.File.Name, "", DownloadTTL)
	if err != nil {
		return Link{}, fmt.Errorf("sign download: %w", err)
	}
	return Link{URL: url, ExpiresAt: time.Now().Add(DownloadTTL)}, nil
}

// PreviewCheck is the name of the check that records a version's preview.
const PreviewCheck = "preview"

// Preview kinds, as the worker records them.
const (
	PreviewImage         = "image"          // a rendered page stored at Key
	PreviewOriginalImage = "original_image" // the uploaded file is an image
	PreviewText          = "text"           // a README found in an archive
	PreviewNone          = "none"
)

// PreviewDetail is what the worker stores in the preview check's detail.
type PreviewDetail struct {
	Kind        string `json:"kind"`
	Key         string `json:"key,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Path        string `json:"path,omitempty"`
	Text        string `json:"text,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// Bytes is the size of a stored preview image, counted in the ledger.
	Bytes int64 `json:"bytes,omitempty"`
}

// PreviewView is a preview ready for the browser.
type PreviewView struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
	Text string `json:"text,omitempty"`
}

// Preview returns a version's preview: a signed image URL, README text, or none.
func (s *Service) Preview(ctx context.Context, userID, versionID string) (PreviewView, error) {
	v, err := s.load(ctx, userID, versionID)
	if err != nil {
		return PreviewView{}, err
	}
	var raw []byte
	err = s.db.QueryRow(ctx, `select detail from check_results where version_id = $1 and check_name = $2`, v.ID, PreviewCheck).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return PreviewView{Kind: PreviewNone}, nil
	}
	if err != nil {
		return PreviewView{}, fmt.Errorf("load preview: %w", err)
	}
	var d PreviewDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return PreviewView{}, fmt.Errorf("decode preview: %w", err)
	}
	switch d.Kind {
	case PreviewImage, PreviewOriginalImage:
		key := d.Key
		if d.Kind == PreviewOriginalImage {
			key = v.objectKey
		}
		url, err := s.store.PresignGet(ctx, key, "", d.ContentType, DownloadTTL)
		if err != nil {
			return PreviewView{}, fmt.Errorf("sign preview: %w", err)
		}
		return PreviewView{Kind: PreviewImage, URL: url}, nil
	case PreviewText:
		return PreviewView{Kind: PreviewText, Path: d.Path, Text: d.Text}, nil
	default:
		return PreviewView{Kind: PreviewNone}, nil
	}
}

// ExpireStale expires every version still uploading after its grace window,
// releasing its reserved storage. It returns how many it expired.
func (s *Service) ExpireStale(ctx context.Context) (int, error) {
	rows, err := s.db.Query(ctx, `
		select `+versionColumns+`, h.id::text, h.deadline_at, h.grace_seconds, now()
		from versions v join teams t on t.id = v.team_id join hackathons h on h.id = t.hackathon_id
		where v.state = 'uploading' and now() > h.deadline_at + make_interval(secs => h.grace_seconds)
		limit 500`)
	if err != nil {
		return 0, fmt.Errorf("find stale uploads: %w", err)
	}
	var stale []versionRow
	for rows.Next() {
		var hackathonID string
		var w Window
		var grace int
		v, err := scanVersion(rows, &hackathonID, &w.DeadlineAt, &grace, &w.ServerTime)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stale upload: %w", err)
		}
		w.GraceEndsAt = w.DeadlineAt.Add(time.Duration(grace) * time.Second)
		v.hackathonID, v.window = hackathonID, w
		stale = append(stale, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("find stale uploads: %w", err)
	}
	for _, v := range stale {
		s.expire(ctx, v, "", "the grace window closed before the upload finished", "")
	}
	return len(stale), nil
}

// SweepWorker runs ExpireStale from River's periodic job.
type SweepWorker struct {
	river.WorkerDefaults[jobs.ExpireStaleUploads]
	Service *Service
}

// Work expires stale uploads.
func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[jobs.ExpireStaleUploads]) error {
	n, err := w.Service.ExpireStale(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Service.log.InfoContext(ctx, "expired stale uploads", "count", n)
	}
	return nil
}
