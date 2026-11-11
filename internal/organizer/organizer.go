// Package organizer answers the organizer dashboard: live counts and the
// CSV export of every team's counted submission.
package organizer

import (
	"context"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
)

// countedRule is the SQL form of the rule in submissions.ForTeam: created
// before the deadline, finished within grace, and its file matched its hash.
// TestCSVAgreesWithTheTeamPage keeps the two in step.
const countedRule = `
	v.created_at <= h.deadline_at
	and v.completed_at is not null
	and v.completed_at <= h.deadline_at + make_interval(secs => h.grace_seconds)
	and v.state in ('checking', 'accepted', 'needs_fix')
	and not exists (select 1 from check_results c where c.version_id = v.id and c.check_name = 'integrity' and not c.passed)`

// Stats are the dashboard's live numbers.
type Stats struct {
	HackathonID    string             `json:"hackathon_id"`
	HackathonName  string             `json:"hackathon_name"`
	Window         submissions.Window `json:"window"`
	Teams          int                `json:"teams"`
	TeamsSubmitted int                `json:"teams_submitted"`
	Versions       map[string]int     `json:"versions"`
	LateAttempts   map[string]int     `json:"late_attempts"`
	FailedChecks   map[string]int     `json:"failed_checks"`
	Storage        struct {
		ReservedBytes int64 `json:"reserved_bytes"`
		BudgetBytes   int64 `json:"budget_bytes"`
	} `json:"storage"`
	// Ledger is the account-wide count that keeps the bucket under the cap.
	Ledger submissions.Ledger `json:"ledger"`
}

// Service runs the organizer queries against the current hackathon.
type Service struct{ DB *pgxpool.Pool }

func (s Service) current(ctx context.Context) (id, name string, w submissions.Window, reserved, budget int64, err error) {
	var grace int
	err = s.DB.QueryRow(ctx, `
		select id::text, name, deadline_at, grace_seconds, now(), reserved_bytes, storage_budget_bytes
		from hackathons order by created_at desc limit 1`).
		Scan(&id, &name, &w.DeadlineAt, &grace, &w.ServerTime, &reserved, &budget)
	w.GraceEndsAt = w.DeadlineAt.Add(time.Duration(grace) * time.Second)
	return
}

// Stats counts submissions, late attempts and failed checks.
func (s Service) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	var err error
	st.HackathonID, st.HackathonName, st.Window, st.Storage.ReservedBytes, st.Storage.BudgetBytes, err = s.current(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("load hackathon: %w", err)
	}

	if err := s.DB.QueryRow(ctx, `
		select (select count(*) from teams where hackathon_id = $1),
		       (select count(distinct v.team_id)
		          from versions v join teams t on t.id = v.team_id join hackathons h on h.id = t.hackathon_id
		         where h.id = $1 and `+countedRule+`)`, st.HackathonID).
		Scan(&st.Teams, &st.TeamsSubmitted); err != nil {
		return Stats{}, fmt.Errorf("count teams: %w", err)
	}
	if st.Ledger, err = submissions.ReadLedger(ctx, s.DB); err != nil {
		return Stats{}, err
	}

	st.Versions = map[string]int{"uploading": 0, "checking": 0, "accepted": 0, "needs_fix": 0, "expired": 0}
	if err := s.countInto(ctx, st.Versions, `
		select v.state::text, count(*) from versions v join teams t on t.id = v.team_id
		where t.hackathon_id = $1 group by v.state`, st.HackathonID); err != nil {
		return Stats{}, err
	}
	st.LateAttempts = map[string]int{"create_after_deadline": 0, "sign_after_grace": 0, "complete_after_grace": 0}
	if err := s.countInto(ctx, st.LateAttempts, `
		select kind, count(*) from late_attempts where hackathon_id = $1 group by kind`, st.HackathonID); err != nil {
		return Stats{}, err
	}
	st.FailedChecks = map[string]int{"integrity": 0, "file_type": 0, "archive": 0, "links": 0}
	if err := s.countInto(ctx, st.FailedChecks, `
		select c.check_name, count(*) from check_results c
		join versions v on v.id = c.version_id join teams t on t.id = v.team_id
		where t.hackathon_id = $1 and not c.passed and c.check_name <> 'preview'
		group by c.check_name`, st.HackathonID); err != nil {
		return Stats{}, err
	}
	return st, nil
}

func (s Service) countInto(ctx context.Context, into map[string]int, sql string, args ...any) error {
	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return fmt.Errorf("count: %w", err)
		}
		into[k] = n
	}
	return rows.Err()
}

// CSVHeader is the export's first row.
var CSVHeader = []string{
	"team", "team_id", "counted_version", "state", "title", "file_name", "file_size_bytes", "file_sha256",
	"created_at", "completed_at", "links", "failed_checks", "versions_total", "late_attempts",
}

// WriteCSV streams one row per team with its counted version (blank when
// none counts), so organizers can judge from a spreadsheet.
func (s Service) WriteCSV(ctx context.Context, w io.Writer) error {
	hackathonID, _, _, _, _, err := s.current(ctx)
	if err != nil {
		return fmt.Errorf("load hackathon: %w", err)
	}
	rows, err := s.DB.Query(ctx, `
		with counted as (
			select distinct on (v.team_id) v.id, v.team_id, v.number, v.state::text as state, v.title, v.file_name,
			       v.file_size, v.file_sha256, v.created_at, v.completed_at, v.links
			from versions v join teams t on t.id = v.team_id join hackathons h on h.id = t.hackathon_id
			where h.id = $1 and `+countedRule+`
			order by v.team_id, v.number desc
		)
		select t.name, t.id::text, c.number, c.state, c.title, c.file_name, c.file_size, c.file_sha256,
		       c.created_at, c.completed_at, c.links,
		       (select string_agg(r.check_name, ' ' order by r.check_name) from check_results r
		         where r.version_id = c.id and not r.passed and r.check_name <> 'preview'),
		       (select count(*) from versions v2 where v2.team_id = t.id),
		       (select count(*) from late_attempts l where l.team_id = t.id)
		from teams t left join counted c on c.team_id = t.id
		where t.hackathon_id = $1
		order by t.name`, hackathonID)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer rows.Close()

	cw := csv.NewWriter(w)
	if err := cw.Write(CSVHeader); err != nil {
		return err
	}
	for rows.Next() {
		var (
			name, teamID                string
			number                      *int
			state, title, file, failed  *string
			size                        *int64
			sha                         []byte
			created, completed          *time.Time
			links                       []string
			versionsTotal, lateAttempts int
		)
		if err := rows.Scan(&name, &teamID, &number, &state, &title, &file, &size, &sha, &created, &completed, &links, &failed, &versionsTotal, &lateAttempts); err != nil {
			return fmt.Errorf("export row: %w", err)
		}
		rec := []string{
			cell(name), teamID, intOrBlank(number), deref(state), cell(deref(title)), cell(deref(file)),
			int64OrBlank(size), hex.EncodeToString(sha), timeOrBlank(created), timeOrBlank(completed),
			cell(strings.Join(links, " ")), deref(failed), strconv.Itoa(versionsTotal), strconv.Itoa(lateAttempts),
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	cw.Flush()
	return cw.Error()
}

// cell defuses spreadsheet formulas: a team-supplied title such as
// =HYPERLINK(...) is written as text, not run when an organizer opens it.
func cell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func intOrBlank(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

func int64OrBlank(n *int64) string {
	if n == nil {
		return ""
	}
	return strconv.FormatInt(*n, 10)
}

func timeOrBlank(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
