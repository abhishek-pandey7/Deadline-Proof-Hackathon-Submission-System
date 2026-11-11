package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/jobs"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
)

// JobTimeout bounds one check run. RescueStuckJobsAfter must exceed it.
const JobTimeout = 2 * time.Minute

// Worker runs the checks for one version.
//
// Jobs run at least once; results land exactly once. Everything the job
// changes in Postgres (the results, the version's final state and the job's
// own completion) commits in one transaction, guarded so only a version still
// in "checking" moves. A worker killed before that commit leaves nothing
// behind and River retries the job; one that wakes up after another attempt
// already committed changes nothing.
type Worker struct {
	river.WorkerDefaults[jobs.CheckVersion]
	DB     *pgxpool.Pool
	Runner *Runner
	Log    *slog.Logger
	// BeforeCommit, if set, runs after the checks and before the commit.
	// The crash test uses it to kill a worker at the worst moment.
	BeforeCommit func(versionID string)
	// JobTimeout overrides the default per-attempt limit (tests only).
	JobTimeout time.Duration
}

// Timeout bounds one attempt.
func (w *Worker) Timeout(*river.Job[jobs.CheckVersion]) time.Duration {
	if w.JobTimeout > 0 {
		return w.JobTimeout
	}
	return JobTimeout
}

// Work runs the checks and records the outcome.
func (w *Worker) Work(ctx context.Context, job *river.Job[jobs.CheckVersion]) error {
	id := job.Args.VersionID
	var t Target
	var state string
	var sha []byte
	err := w.DB.QueryRow(ctx, `
		select v.id::text, v.object_key, v.file_name, v.file_size, v.file_sha256, v.links, v.state::text
		from versions v where v.id = $1`, id).
		Scan(&t.VersionID, &t.ObjectKey, &t.FileName, &t.FileSize, &sha, &t.Links, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		w.Log.WarnContext(ctx, "check job for a missing version", "version_id", id)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load version: %w", err)
	}
	if state != "checking" {
		return nil // an earlier attempt already finished it
	}
	t.SHA256 = fmt.Sprintf("%x", sha)

	results, err := w.Runner.Run(ctx, t)
	if err != nil {
		return err
	}

	final, reason := "accepted", ""
	var failed []string
	for _, r := range results {
		if Blocking(r) {
			failed = append(failed, r.Name)
		}
	}
	if len(failed) > 0 {
		final, reason = "needs_fix", "failed checks: "+strings.Join(failed, ", ")
	}

	if w.BeforeCommit != nil {
		w.BeforeCommit(id)
	}

	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var teamID string
	err = tx.QueryRow(ctx, `
		update versions set state = $2, state_reason = nullif($3, ''), checks_finished_at = now()
		where id = $1 and state = 'checking'
		returning team_id::text`, id, final, reason).Scan(&teamID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Another attempt won; keep its results and just finish this job.
	case err != nil:
		return fmt.Errorf("finish version: %w", err)
	default:
		for _, r := range results {
			detail, err := json.Marshal(r.Detail)
			if err != nil {
				return fmt.Errorf("encode %s detail: %w", r.Name, err)
			}
			if _, err := tx.Exec(ctx, `
				insert into check_results (version_id, check_name, passed, detail) values ($1, $2, $3, $4)
				on conflict (version_id, check_name) do update
				set passed = excluded.passed, detail = excluded.detail, checked_at = now()`,
				id, r.Name, r.Passed, detail); err != nil {
				return fmt.Errorf("store %s: %w", r.Name, err)
			}
		}
		for _, r := range results {
			if d, ok := r.Detail.(submissions.PreviewDetail); ok {
				if err := submissions.RecordPreview(ctx, tx, id, d.Bytes); err != nil {
					return err
				}
			}
		}
		payload, _ := json.Marshal(map[string]any{"version_id": id, "team_id": teamID, "state": final})
		if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, submissions.EventsChannel, string(payload)); err != nil {
			return fmt.Errorf("notify: %w", err)
		}
	}

	if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	w.Log.InfoContext(ctx, "checks finished", "version_id", id, "state", final, "attempt", job.Attempt)
	return nil
}

// ClientConfig is the River setup for a process that runs the workers.
// rescueAfter is how long a job may sit "running" before River assumes its
// worker died and retries it; it must exceed jobTimeout.
func ClientConfig(workers *river.Workers, checkConcurrency int, jobTimeout, rescueAfter time.Duration, periodic []*river.PeriodicJob) *river.Config {
	return &river.Config{
		Queues: map[string]river.QueueConfig{
			jobs.QueueChecks:      {MaxWorkers: checkConcurrency},
			jobs.QueueMaintenance: {MaxWorkers: 1},
		},
		Workers:              workers,
		PeriodicJobs:         periodic,
		JobTimeout:           jobTimeout,
		RescueStuckJobsAfter: rescueAfter,
	}
}
