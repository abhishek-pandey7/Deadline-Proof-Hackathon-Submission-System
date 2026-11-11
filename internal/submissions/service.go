// Package submissions holds the rules for creating, uploading and completing
// submission versions. Every deadline decision compares against Postgres
// now() inside the transaction that makes the change: one clock for everyone.
package submissions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/jobs"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
)

// Defaults for uploads.
const (
	DefaultPartSize = 8 << 20 // 8 MiB: 13 parts for a 100 MiB file
	DefaultURLTTL   = 10 * time.Minute
	MaxPartsPerSign = 20
)

// EventsChannel is the Postgres NOTIFY channel for version state changes.
const EventsChannel = "version_events"

// ObjectStore is the slice of storage the service uses.
type ObjectStore interface {
	CreateMultipart(ctx context.Context, key string) (string, error)
	PresignPart(ctx context.Context, key, uploadID string, part int32, size int64, ttl time.Duration) (storage.SignedRequest, error)
	ListParts(ctx context.Context, key, uploadID string) ([]storage.Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []storage.Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
	Size(ctx context.Context, key string) (int64, error)
	Delete(ctx context.Context, key string) error
}

// Options tune the service; zero values mean the defaults.
type Options struct {
	PartSize int64
	URLTTL   time.Duration
}

// Service implements the submission rules.
type Service struct {
	db       *pgxpool.Pool
	store    ObjectStore
	queue    *river.Client[pgx.Tx]
	log      *slog.Logger
	partSize int64
	urlTTL   time.Duration
}

// New builds a Service. queue may be an insert-only River client.
func New(pool *pgxpool.Pool, store ObjectStore, queue *river.Client[pgx.Tx], log *slog.Logger, opts Options) *Service {
	if opts.PartSize == 0 {
		opts.PartSize = DefaultPartSize
	}
	if opts.URLTTL == 0 {
		opts.URLTTL = DefaultURLTTL
	}
	return &Service{db: pool, store: store, queue: queue, log: log, partSize: opts.PartSize, urlTTL: opts.URLTTL}
}

// FileInfo describes a version's file.
type FileInfo struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Check is one background check's result.
type Check struct {
	Name   string          `json:"name"`
	Passed bool            `json:"passed"`
	Detail json.RawMessage `json:"detail"`
}

// Version is one immutable submission attempt.
type Version struct {
	ID               string     `json:"id"`
	TeamID           string     `json:"team_id"`
	Number           int        `json:"number"`
	Title            string     `json:"title"`
	Description      string     `json:"description"`
	Links            []string   `json:"links"`
	File             FileInfo   `json:"file"`
	State            string     `json:"state"`
	StateReason      *string    `json:"state_reason"`
	CreatedAt        time.Time  `json:"created_at"`
	CompletedAt      *time.Time `json:"completed_at"`
	ChecksFinishedAt *time.Time `json:"checks_finished_at"`
	PartSize         int64      `json:"part_size"`
	PartCount        int        `json:"part_count"`
	Checks           []Check    `json:"checks"`
	Counts           bool       `json:"counts"`
}

// Window is the deadline as the server sees it right now.
type Window struct {
	DeadlineAt  time.Time `json:"deadline_at"`
	GraceEndsAt time.Time `json:"grace_ends_at"`
	ServerTime  time.Time `json:"server_time"`
}

// versionRow is a Version plus the fields clients never see.
type versionRow struct {
	Version
	objectKey   string
	uploadID    *string
	hackathonID string
	window      Window
}

const versionColumns = `v.id::text, v.team_id::text, v.number, v.title, v.description, v.links,
	v.file_name, v.file_size, v.file_sha256, v.state::text, v.state_reason,
	v.created_at, v.completed_at, v.checks_finished_at, v.part_size, v.object_key, v.upload_id`

func scanVersion(row pgx.Row, extra ...any) (versionRow, error) {
	var v versionRow
	var sha []byte
	dest := []any{&v.ID, &v.TeamID, &v.Number, &v.Title, &v.Description, &v.Links,
		&v.File.Name, &v.File.Size, &sha, &v.State, &v.StateReason,
		&v.CreatedAt, &v.CompletedAt, &v.ChecksFinishedAt, &v.PartSize, &v.objectKey, &v.uploadID}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return versionRow{}, err
	}
	v.File.SHA256 = hex.EncodeToString(sha)
	v.PartCount = PartCount(v.File.Size, v.PartSize)
	if v.Links == nil {
		v.Links = []string{}
	}
	v.Checks = []Check{}
	return v, nil
}

// CreateResult is a new or replayed version and the window it was made in.
type CreateResult struct {
	Version  Version `json:"version"`
	Window   Window  `json:"window"`
	Replayed bool    `json:"-"`
}

// Create starts a new version for a team. A retry with the same idempotency
// key returns the original version instead of making a second one.
func (s *Service) Create(ctx context.Context, userID, teamID string, in CreateInput) (CreateResult, error) {
	in.Normalize()
	if bad := in.Validate(); bad != nil {
		return CreateResult{}, &Error{Code: CodeInvalidInput, Message: "some fields are invalid", Details: map[string]any{"fields": bad}}
	}
	sha, _ := hex.DecodeString(in.FileSHA256)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Lock the team row: version numbers and idempotency keys are per team,
	// so two concurrent creates for one team run one after the other.
	var (
		hackathonID    string
		w              Window
		graceSeconds   int
		maxFileBytes   int64
		maxVersions    int
		uploadsEnabled bool
		member         bool
	)
	err = tx.QueryRow(ctx, `
		select h.id::text, h.deadline_at, h.grace_seconds, h.max_file_bytes, h.max_versions_per_team,
		       h.uploads_enabled, now(),
		       exists (select 1 from team_members m where m.team_id = t.id and m.user_id = $2)
		from teams t join hackathons h on h.id = t.hackathon_id
		where t.id = $1
		for update of t`, teamID, userID).
		Scan(&hackathonID, &w.DeadlineAt, &graceSeconds, &maxFileBytes, &maxVersions, &uploadsEnabled, &w.ServerTime, &member)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !member) {
		return CreateResult{}, errNotFound
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("create: load team: %w", err)
	}
	w.GraceEndsAt = w.DeadlineAt.Add(time.Duration(graceSeconds) * time.Second)

	existing, err := scanVersion(tx.QueryRow(ctx,
		`select `+versionColumns+` from versions v where v.team_id = $1 and v.idempotency_key = $2`, teamID, in.IdempotencyKey))
	switch {
	case err == nil:
		if existing.File.Size != in.FileSize || existing.File.SHA256 != in.FileSHA256 {
			return CreateResult{}, errf(CodeIdempotencyMismatch, "this idempotency key was already used for a different file")
		}
		if err := tx.Commit(ctx); err != nil {
			return CreateResult{}, fmt.Errorf("create: commit replay: %w", err)
		}
		existing.hackathonID, existing.window = hackathonID, w
		if err := s.ensureMultipart(ctx, &existing); err != nil {
			return CreateResult{}, err
		}
		return CreateResult{Version: existing.Version, Window: w, Replayed: true}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return CreateResult{}, fmt.Errorf("create: idempotency lookup: %w", err)
	}

	if w.ServerTime.After(w.DeadlineAt) {
		if _, err := tx.Exec(ctx, `insert into late_attempts (hackathon_id, team_id, user_id, kind) values ($1, $2, $3, 'create_after_deadline')`,
			hackathonID, teamID, userID); err != nil {
			return CreateResult{}, fmt.Errorf("create: log late attempt: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return CreateResult{}, fmt.Errorf("create: commit late attempt: %w", err)
		}
		return CreateResult{}, &Error{Code: CodeDeadlinePassed, Message: "the deadline has passed", Details: map[string]any{"deadline_at": w.DeadlineAt, "server_time": w.ServerTime}}
	}
	if !uploadsEnabled {
		return CreateResult{}, errf(CodeUploadsDisabled, "uploads are paused by the organizers")
	}
	if in.FileSize > maxFileBytes {
		return CreateResult{}, &Error{Code: CodeFileTooLarge, Message: "the file is too large", Details: map[string]any{"max_bytes": maxFileBytes}}
	}

	var count, lastNumber int
	if err := tx.QueryRow(ctx, `select count(*), coalesce(max(number), 0) from versions where team_id = $1`, teamID).Scan(&count, &lastNumber); err != nil {
		return CreateResult{}, fmt.Errorf("create: count versions: %w", err)
	}
	if count >= maxVersions {
		return CreateResult{}, &Error{Code: CodeVersionLimit, Message: "this team has used all its versions", Details: map[string]any{"max_versions": maxVersions}}
	}

	// Reserve the declared size against the hackathon's storage budget before
	// any upload link exists. The conditional update is atomic, so concurrent
	// creates from different teams can never overshoot the budget.
	tag, err := tx.Exec(ctx, `update hackathons set reserved_bytes = reserved_bytes + $2
		where id = $1 and reserved_bytes + $2 <= storage_budget_bytes`, hackathonID, in.FileSize)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create: reserve storage: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return CreateResult{}, errf(CodeStorageFull, "storage for this hackathon is full; contact the organizers")
	}

	id := newUUID()
	v, err := scanVersion(tx.QueryRow(ctx, `
		insert into versions as v (id, team_id, number, idempotency_key, created_by, title, description, links,
		                           file_name, file_size, file_sha256, object_key, part_size)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		returning `+versionColumns,
		id, teamID, lastNumber+1, in.IdempotencyKey, userID, in.Title, in.Description, in.Links,
		in.FileName, in.FileSize, sha, fmt.Sprintf("submissions/%s/%s/%s", hackathonID, teamID, id), s.partSize))
	if err != nil {
		return CreateResult{}, fmt.Errorf("create: insert version: %w", err)
	}
	if err := notify(ctx, tx, v.Version); err != nil {
		return CreateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, fmt.Errorf("create: commit: %w", err)
	}

	// Start the multipart upload after commit, so no storage call happens
	// while the team row is locked. If storage fails, a retry with the same
	// idempotency key comes back through ensureMultipart.
	v.hackathonID, v.window = hackathonID, w
	if err := s.ensureMultipart(ctx, &v); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Version: v.Version, Window: w}, nil
}

// ensureMultipart makes sure the version has a multipart upload. Concurrent
// callers may both create one; only the first is kept and the other aborted.
func (s *Service) ensureMultipart(ctx context.Context, v *versionRow) error {
	if v.uploadID != nil || v.State != "uploading" {
		return nil
	}
	uploadID, err := s.store.CreateMultipart(ctx, v.objectKey)
	if err != nil {
		s.log.ErrorContext(ctx, "create multipart upload", "version_id", v.ID, "err", err)
		return errf(CodeStorageUnavailable, "storage is unavailable; retry the same request")
	}
	tag, err := s.db.Exec(ctx, `update versions set upload_id = $2 where id = $1 and upload_id is null`, v.ID, uploadID)
	if err != nil {
		return fmt.Errorf("save upload id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		_ = s.store.AbortMultipart(ctx, v.objectKey, uploadID)
		if err := s.db.QueryRow(ctx, `select upload_id from versions where id = $1`, v.ID).Scan(&v.uploadID); err != nil {
			return fmt.Errorf("reload upload id: %w", err)
		}
		return nil
	}
	v.uploadID = &uploadID
	return nil
}

// load fetches a version the user may act on, with the hackathon's window.
func (s *Service) load(ctx context.Context, userID, versionID string) (versionRow, error) {
	var (
		graceSeconds int
		hackathonID  string
		w            Window
	)
	v, err := scanVersion(s.db.QueryRow(ctx, `
		select `+versionColumns+`, h.id::text, h.deadline_at, h.grace_seconds, now()
		from versions v
		join teams t on t.id = v.team_id
		join hackathons h on h.id = t.hackathon_id
		where v.id = $1
		  and exists (select 1 from team_members m where m.team_id = v.team_id and m.user_id = $2)`,
		versionID, userID), &hackathonID, &w.DeadlineAt, &graceSeconds, &w.ServerTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return versionRow{}, errNotFound
	}
	if err != nil {
		return versionRow{}, fmt.Errorf("load version: %w", err)
	}
	w.GraceEndsAt = w.DeadlineAt.Add(time.Duration(graceSeconds) * time.Second)
	v.hackathonID, v.window = hackathonID, w
	return v, nil
}

// SignedPart is one part's upload request.
type SignedPart struct {
	Number  int               `json:"number"`
	Size    int64             `json:"size"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// SignResult carries signed part URLs and when they stop working.
type SignResult struct {
	Parts     []SignedPart `json:"parts"`
	ExpiresAt time.Time    `json:"expires_at"`
	Window    Window       `json:"window"`
}

// SignParts returns presigned PUT URLs for the given part numbers. No URL
// ever outlives the end of the grace window.
func (s *Service) SignParts(ctx context.Context, userID, versionID string, numbers []int) (SignResult, error) {
	v, err := s.load(ctx, userID, versionID)
	if err != nil {
		return SignResult{}, err
	}
	if v.State != "uploading" {
		return SignResult{}, &Error{Code: CodeWrongState, Message: "this version is no longer uploading", Details: map[string]any{"state": v.State}}
	}
	now := v.window.ServerTime
	if !now.Before(v.window.GraceEndsAt) {
		s.expire(ctx, v, userID, "upload did not finish before the grace window closed", "sign_after_grace")
		return SignResult{}, errf(CodeGraceOver, "the grace window for unfinished uploads has closed")
	}
	ttl := min(s.urlTTL, v.window.GraceEndsAt.Sub(now))
	if ttl < time.Second {
		s.expire(ctx, v, userID, "upload did not finish before the grace window closed", "sign_after_grace")
		return SignResult{}, errf(CodeGraceOver, "the grace window for unfinished uploads has closed")
	}

	if len(numbers) == 0 || len(numbers) > MaxPartsPerSign {
		return SignResult{}, errf(CodeInvalidInput, "ask for 1-%d parts at a time", MaxPartsPerSign)
	}
	seen := map[int]bool{}
	for _, n := range numbers {
		if n < 1 || n > v.PartCount || seen[n] {
			return SignResult{}, &Error{Code: CodeInvalidInput, Message: "invalid part number", Details: map[string]any{"part": n, "part_count": v.PartCount}}
		}
		seen[n] = true
	}

	if err := s.ensureMultipart(ctx, &v); err != nil {
		return SignResult{}, err
	}
	out := SignResult{ExpiresAt: now.Add(ttl), Window: v.window, Parts: make([]SignedPart, 0, len(numbers))}
	for _, n := range numbers {
		size := PartLength(v.File.Size, v.PartSize, n)
		req, err := s.store.PresignPart(ctx, v.objectKey, *v.uploadID, int32(n), size, ttl)
		if err != nil {
			return SignResult{}, fmt.Errorf("sign part %d: %w", n, err)
		}
		out.Parts = append(out.Parts, SignedPart{Number: n, Size: size, Method: req.Method, URL: req.URL, Headers: req.Headers})
	}
	return out, nil
}

// PartInfo is an uploaded part as storage reports it.
type PartInfo struct {
	Number int   `json:"number"`
	Size   int64 `json:"size"`
}

// UploadStatus tells a resuming client what storage already has.
type UploadStatus struct {
	State     string     `json:"state"`
	PartCount int        `json:"part_count"`
	PartSize  int64      `json:"part_size"`
	Uploaded  []PartInfo `json:"uploaded"`
	Window    Window     `json:"window"`
}

// Uploaded lists the parts storage holds, so a client can resume by
// sending only the missing ones. Storage, not the database, is the ledger.
func (s *Service) Uploaded(ctx context.Context, userID, versionID string) (UploadStatus, error) {
	v, err := s.load(ctx, userID, versionID)
	if err != nil {
		return UploadStatus{}, err
	}
	out := UploadStatus{State: v.State, PartCount: v.PartCount, PartSize: v.PartSize, Uploaded: []PartInfo{}, Window: v.window}
	if v.State != "uploading" || v.uploadID == nil {
		return out, nil
	}
	parts, err := s.store.ListParts(ctx, v.objectKey, *v.uploadID)
	if errors.Is(err, storage.ErrNoSuchUpload) {
		return out, nil
	}
	if err != nil {
		return UploadStatus{}, errf(CodeStorageUnavailable, "storage is unavailable; retry")
	}
	for _, p := range parts {
		out.Uploaded = append(out.Uploaded, PartInfo{Number: int(p.Number), Size: p.Size})
	}
	return out, nil
}

// Complete assembles the uploaded parts and hands the version to the
// background checks. Calling it again after success returns the same version.
func (s *Service) Complete(ctx context.Context, userID, versionID string) (Version, error) {
	v, err := s.load(ctx, userID, versionID)
	if err != nil {
		return Version{}, err
	}
	switch v.State {
	case "checking", "accepted", "needs_fix":
		return s.withChecks(ctx, v.Version)
	case "expired":
		return Version{}, &Error{Code: CodeWrongState, Message: "this version expired", Details: map[string]any{"state": v.State}}
	}
	if v.window.ServerTime.After(v.window.GraceEndsAt) {
		s.expire(ctx, v, userID, "upload did not finish before the grace window closed", "complete_after_grace")
		return Version{}, errf(CodeGraceOver, "the grace window for unfinished uploads has closed")
	}
	if v.uploadID == nil {
		return Version{}, &Error{Code: CodePartsMissing, Message: "no parts uploaded yet", Details: map[string]any{"missing": seq(v.PartCount)}}
	}

	parts, err := s.store.ListParts(ctx, v.objectKey, *v.uploadID)
	switch {
	case errors.Is(err, storage.ErrNoSuchUpload):
		// A concurrent complete may have assembled it already.
		if size, serr := s.store.Size(ctx, v.objectKey); serr != nil || size != v.File.Size {
			return Version{}, errf(CodeUploadMismatch, "the upload is gone; start a new version")
		}
	case err != nil:
		return Version{}, errf(CodeStorageUnavailable, "storage is unavailable; retry")
	default:
		if perr := checkParts(parts, v); perr != nil {
			return Version{}, perr
		}
		if err := s.store.CompleteMultipart(ctx, v.objectKey, *v.uploadID, parts); err != nil && !errors.Is(err, storage.ErrNoSuchUpload) {
			s.log.ErrorContext(ctx, "complete multipart", "version_id", v.ID, "err", err)
			return Version{}, errf(CodeStorageUnavailable, "storage is unavailable; retry")
		}
		if size, err := s.store.Size(ctx, v.objectKey); err != nil || size != v.File.Size {
			return Version{}, errf(CodeUploadMismatch, "the stored file does not match the declared size")
		}
	}

	done, err := s.finish(ctx, v)
	if err != nil {
		return Version{}, err
	}
	if done != nil {
		return s.withChecks(ctx, *done)
	}

	// Nothing changed: another call finished it, or the grace window closed
	// between the checks above and the commit.
	again, err := s.load(ctx, userID, versionID)
	if err != nil {
		return Version{}, err
	}
	if again.State == "uploading" {
		s.expire(ctx, again, userID, "upload did not finish before the grace window closed", "complete_after_grace")
		_ = s.store.Delete(ctx, again.objectKey)
		return Version{}, errf(CodeGraceOver, "the grace window for unfinished uploads has closed")
	}
	return s.withChecks(ctx, again.Version)
}

// finish moves the version to checking and enqueues its job in one
// transaction. It returns nil when the version was not in a finishable state.
func (s *Service) finish(ctx context.Context, v versionRow) (*Version, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("finish: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	done, err := scanVersion(tx.QueryRow(ctx, `
		update versions as v set state = 'checking', completed_at = now(), upload_id = null
		from teams t join hackathons h on h.id = t.hackathon_id
		where v.id = $1 and t.id = v.team_id and v.state = 'uploading'
		  and now() <= h.deadline_at + make_interval(secs => h.grace_seconds)
		returning `+versionColumns, v.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finish: update: %w", err)
	}
	if _, err := s.queue.InsertTx(ctx, tx, jobs.CheckVersion{VersionID: v.ID}, nil); err != nil {
		return nil, fmt.Errorf("finish: enqueue checks: %w", err)
	}
	if err := notify(ctx, tx, done.Version); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("finish: commit: %w", err)
	}
	return &done.Version, nil
}

// checkParts compares storage's part list with the plan for this version.
func checkParts(parts []storage.Part, v versionRow) *Error {
	have := map[int]int64{}
	for _, p := range parts {
		have[int(p.Number)] = p.Size
	}
	var missing []int
	var wrong []map[string]any
	for n := 1; n <= v.PartCount; n++ {
		size, ok := have[n]
		want := PartLength(v.File.Size, v.PartSize, n)
		switch {
		case !ok:
			missing = append(missing, n)
		case size != want:
			wrong = append(wrong, map[string]any{"part": n, "size": size, "want": want})
		}
	}
	if len(missing) > 0 {
		return &Error{Code: CodePartsMissing, Message: "some parts have not been uploaded", Details: map[string]any{"missing": missing}}
	}
	if len(wrong) > 0 {
		return &Error{Code: CodeUploadMismatch, Message: "some parts have the wrong size; upload them again", Details: map[string]any{"parts": wrong}}
	}
	return nil
}

// expire marks an unfinished version expired, gives its bytes back to the
// storage budget, logs the late attempt and discards the partial upload.
func (s *Service) expire(ctx context.Context, v versionRow, userID, reason, lateKind string) {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		row, err := scanVersion(tx.QueryRow(ctx, `
			update versions as v set state = 'expired', state_reason = $2
			where v.id = $1 and v.state = 'uploading'
			returning `+versionColumns, v.ID, reason))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if _, err := tx.Exec(ctx, `update hackathons set reserved_bytes = reserved_bytes - $2 where id = $1`, v.hackathonID, v.File.Size); err != nil {
				return err
			}
			if err := notify(ctx, tx, row.Version); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `insert into late_attempts (hackathon_id, team_id, user_id, kind) values ($1, $2, $3, $4)`,
			v.hackathonID, v.TeamID, userID, lateKind)
		return err
	})
	if err != nil {
		s.log.ErrorContext(ctx, "expire version", "version_id", v.ID, "err", err)
		return
	}
	if v.uploadID != nil {
		if err := s.store.AbortMultipart(ctx, v.objectKey, *v.uploadID); err != nil {
			s.log.WarnContext(ctx, "abort multipart after expiry", "version_id", v.ID, "err", err)
		}
	}
}

// withChecks attaches stored check results to a version.
func (s *Service) withChecks(ctx context.Context, v Version) (Version, error) {
	rows, err := s.db.Query(ctx, `select check_name, passed, detail from check_results where version_id = $1 order by check_name`, v.ID)
	if err != nil {
		return Version{}, fmt.Errorf("load checks: %w", err)
	}
	checks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Check, error) {
		var c Check
		err := r.Scan(&c.Name, &c.Passed, &c.Detail)
		return c, err
	})
	if err != nil {
		return Version{}, fmt.Errorf("scan checks: %w", err)
	}
	v.Checks = checks
	if v.Checks == nil {
		v.Checks = []Check{}
	}
	return v, nil
}

// Team is a team as its members see it.
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TeamSubmission is everything a team page shows.
type TeamSubmission struct {
	Team             Team      `json:"team"`
	Window           Window    `json:"window"`
	Versions         []Version `json:"versions"`
	CountedVersionID *string   `json:"counted_version_id"`
}

// ForTeam lists a team's versions, newest first, and marks the one that
// counts: the latest version created before the deadline that finished
// uploading within grace and whose file matched its declared hash.
func (s *Service) ForTeam(ctx context.Context, userID, teamID string) (TeamSubmission, error) {
	var out TeamSubmission
	var graceSeconds int
	var member bool
	err := s.db.QueryRow(ctx, `
		select t.id::text, t.name, h.deadline_at, h.grace_seconds, now(),
		       exists (select 1 from team_members m where m.team_id = t.id and m.user_id = $2)
		from teams t join hackathons h on h.id = t.hackathon_id where t.id = $1`, teamID, userID).
		Scan(&out.Team.ID, &out.Team.Name, &out.Window.DeadlineAt, &graceSeconds, &out.Window.ServerTime, &member)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !member) {
		return TeamSubmission{}, errNotFound
	}
	if err != nil {
		return TeamSubmission{}, fmt.Errorf("load team: %w", err)
	}
	out.Window.GraceEndsAt = out.Window.DeadlineAt.Add(time.Duration(graceSeconds) * time.Second)

	rows, err := s.db.Query(ctx, `select `+versionColumns+` from versions v where v.team_id = $1 order by v.number desc`, teamID)
	if err != nil {
		return TeamSubmission{}, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()
	byID := map[string]int{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return TeamSubmission{}, fmt.Errorf("scan version: %w", err)
		}
		byID[v.ID] = len(out.Versions)
		out.Versions = append(out.Versions, v.Version)
	}
	if err := rows.Err(); err != nil {
		return TeamSubmission{}, fmt.Errorf("list versions: %w", err)
	}
	if out.Versions == nil {
		out.Versions = []Version{}
	}

	crow, err := s.db.Query(ctx, `
		select c.version_id::text, c.check_name, c.passed, c.detail
		from check_results c join versions v on v.id = c.version_id
		where v.team_id = $1 order by c.check_name`, teamID)
	if err != nil {
		return TeamSubmission{}, fmt.Errorf("list checks: %w", err)
	}
	defer crow.Close()
	for crow.Next() {
		var id string
		var c Check
		if err := crow.Scan(&id, &c.Name, &c.Passed, &c.Detail); err != nil {
			return TeamSubmission{}, fmt.Errorf("scan check: %w", err)
		}
		if i, ok := byID[id]; ok {
			out.Versions[i].Checks = append(out.Versions[i].Checks, c)
		}
	}
	if err := crow.Err(); err != nil {
		return TeamSubmission{}, fmt.Errorf("list checks: %w", err)
	}

	for i := range out.Versions { // newest first, so the first match counts
		if counts(out.Versions[i], out.Window) {
			out.Versions[i].Counts = true
			out.CountedVersionID = &out.Versions[i].ID
			break
		}
	}
	return out, nil
}

// IntegrityCheck is the check that compares the stored file with its hash.
const IntegrityCheck = "integrity"

func counts(v Version, w Window) bool {
	if v.CreatedAt.After(w.DeadlineAt) || v.CompletedAt == nil || v.CompletedAt.After(w.GraceEndsAt) {
		return false
	}
	switch v.State {
	case "checking", "accepted", "needs_fix":
	default:
		return false
	}
	for _, c := range v.Checks {
		if c.Name == IntegrityCheck && !c.Passed {
			return false
		}
	}
	return true
}

// notify publishes a state change; Postgres delivers it only on commit.
func notify(ctx context.Context, tx pgx.Tx, v Version) error {
	payload, _ := json.Marshal(map[string]any{"version_id": v.ID, "team_id": v.TeamID, "state": v.State})
	if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, EventsChannel, string(payload)); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
