package submissions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/jobs"
)

// DefaultStorageCap keeps the whole bucket under R2's 10 GB free tier with
// about 1 GB to spare.
const DefaultStorageCap int64 = 9_000_000_000

// Ledger is the account-wide storage count.
type Ledger struct {
	CapBytes           int64      `json:"cap_bytes"`
	ReservedBytes      int64      `json:"reserved_bytes"`
	UploadedBytesTotal int64      `json:"uploaded_bytes_total"`
	MeasuredBytes      *int64     `json:"measured_bytes"`
	MeasuredObjects    *int       `json:"measured_objects"`
	MeasuredAt         *time.Time `json:"measured_at"`
	// UsedBytes is what the limiter compares with the cap: the larger of the
	// app's own count and the last measurement of the real bucket.
	UsedBytes int64 `json:"used_bytes"`
}

// ReadLedger returns the current storage count.
func ReadLedger(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (Ledger, error) {
	var l Ledger
	err := q.QueryRow(ctx, `
		select cap_bytes, reserved_bytes, uploaded_bytes_total, measured_bytes, measured_objects, measured_at,
		       greatest(reserved_bytes, coalesce(measured_bytes, 0))
		from storage_ledger`).
		Scan(&l.CapBytes, &l.ReservedBytes, &l.UploadedBytesTotal, &l.MeasuredBytes, &l.MeasuredObjects, &l.MeasuredAt, &l.UsedBytes)
	if err != nil {
		return Ledger{}, fmt.Errorf("read storage ledger: %w", err)
	}
	return l, nil
}

// SetStorageCap changes the cap, for STORAGE_CAP_BYTES at startup.
func SetStorageCap(ctx context.Context, pool *pgxpool.Pool, capBytes int64) error {
	if capBytes <= 0 {
		return errors.New("storage cap must be positive")
	}
	if _, err := pool.Exec(ctx, `update storage_ledger set cap_bytes = $1, updated_at = now()`, capBytes); err != nil {
		return fmt.Errorf("set storage cap: %w", err)
	}
	return nil
}

// reserveGlobal takes n bytes from the account-wide cap inside tx. The
// single ledger row is locked by the update, so concurrent creates from any
// hackathon line up and the cap cannot be overshot.
func reserveGlobal(ctx context.Context, tx pgx.Tx, n int64) (bool, error) {
	tag, err := tx.Exec(ctx, `
		update storage_ledger set reserved_bytes = reserved_bytes + $1, updated_at = now()
		where greatest(reserved_bytes, coalesce(measured_bytes, 0)) + $1 <= cap_bytes`, n)
	if err != nil {
		return false, fmt.Errorf("reserve storage: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// releaseGlobal gives n bytes back.
func releaseGlobal(ctx context.Context, tx pgx.Tx, n int64) error {
	if _, err := tx.Exec(ctx, `
		update storage_ledger set reserved_bytes = greatest(reserved_bytes - $1, 0), updated_at = now()`, n); err != nil {
		return fmt.Errorf("release storage: %w", err)
	}
	return nil
}

// countUploaded adds finished bytes to the lifetime total.
func countUploaded(ctx context.Context, tx pgx.Tx, n int64) error {
	if _, err := tx.Exec(ctx, `
		update storage_ledger set uploaded_bytes_total = uploaded_bytes_total + $1, updated_at = now()`, n); err != nil {
		return fmt.Errorf("count uploaded bytes: %w", err)
	}
	return nil
}

// RecordPreview sets a version's stored preview size inside tx and moves the
// ledger by the difference, so a retried job never counts a preview twice.
func RecordPreview(ctx context.Context, tx pgx.Tx, versionID string, bytes int64) error {
	var old int64
	if err := tx.QueryRow(ctx, `select preview_bytes from versions where id = $1 for update`, versionID).Scan(&old); err != nil {
		return fmt.Errorf("read preview size: %w", err)
	}
	if _, err := tx.Exec(ctx, `update versions set preview_bytes = $2 where id = $1`, versionID, bytes); err != nil {
		return fmt.Errorf("record preview size: %w", err)
	}
	delta := bytes - old
	if delta == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		update storage_ledger
		set reserved_bytes = greatest(reserved_bytes + $1, 0),
		    uploaded_bytes_total = uploaded_bytes_total + greatest($1, 0),
		    updated_at = now()`, delta); err != nil {
		return fmt.Errorf("count preview: %w", err)
	}
	return nil
}

// BucketMeasurer sums what the bucket really holds.
type BucketMeasurer interface {
	Measure(ctx context.Context) (bytes int64, objects int, err error)
}

// MeasureStorage records the real bucket size next to the app's own count.
func (s *Service) MeasureStorage(ctx context.Context, m BucketMeasurer) (Ledger, error) {
	bytes, objects, err := m.Measure(ctx)
	if err != nil {
		return Ledger{}, err
	}
	if _, err := s.db.Exec(ctx, `
		update storage_ledger set measured_bytes = $1, measured_objects = $2, measured_at = now()`, bytes, objects); err != nil {
		return Ledger{}, fmt.Errorf("save measurement: %w", err)
	}
	l, err := ReadLedger(ctx, s.db)
	if err != nil {
		return Ledger{}, err
	}
	// Completed objects can only be at most what the app reserved; more means
	// something wrote to the bucket without going through the app.
	if bytes > l.ReservedBytes {
		s.log.WarnContext(ctx, "bucket holds more than the app counted", "measured_bytes", bytes, "reserved_bytes", l.ReservedBytes)
	}
	return l, nil
}

// MeasureWorker runs MeasureStorage from River's periodic job.
type MeasureWorker struct {
	river.WorkerDefaults[jobs.MeasureStorage]
	Service *Service
	Bucket  BucketMeasurer
}

// Work measures the bucket.
func (w *MeasureWorker) Work(ctx context.Context, _ *river.Job[jobs.MeasureStorage]) error {
	l, err := w.Service.MeasureStorage(ctx, w.Bucket)
	if err != nil {
		return err
	}
	w.Service.log.InfoContext(ctx, "measured storage", "used_bytes", l.UsedBytes, "cap_bytes", l.CapBytes)
	return nil
}
