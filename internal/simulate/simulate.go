// Package simulate lets an organizer watch a deadline rush on the live site:
// 100 synthetic teams submit over about a minute, through the same service
// calls and the same signed uploads to storage as real teams, so the dashboard
// climbs live. It is a demonstration, not a benchmark: the load comes from
// inside the server, so it says nothing about network latency.
package simulate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
)

// Limits that keep a public demo cheap: about 6 MB of storage per run.
const (
	TeamsPerRun  = 100
	FileBytes    = 64 << 10
	RunDuration  = time.Minute
	Cooldown     = 2 * time.Minute
	MaxSimTeams  = 2000
	TeamPrefix   = "Sim team "
	emailDomain  = "@sim.invalid"
	parallelPuts = 20
)

// ErrBusy means a run is in progress or the cooldown has not passed.
var ErrBusy = errors.New("a simulation ran recently; wait for it to finish and cool down")

// ErrLimit means the demo has created as many simulated teams as it may.
var ErrLimit = errors.New("the simulated-team limit is reached; ask the maintainer to clean up")

// Status is what the dashboard shows.
type Status struct {
	Running    bool       `json:"running"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Total      int        `json:"total"`
	Completed  int        `json:"completed"`
	Failed     int        `json:"failed"`
	CanStartAt *time.Time `json:"can_start_at"`
}

// Simulator runs at most one rush at a time.
type Simulator struct {
	DB   *pgxpool.Pool
	Subs *submissions.Service
	Log  *slog.Logger
	// HTTP sends the file bytes to storage, as a browser would.
	HTTP *http.Client
	// Teams and Over override the defaults (tests only).
	Teams int
	Over  time.Duration

	mu     sync.Mutex
	status Status
}

// Status returns the current run's progress.
func (s *Simulator) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Start begins a run in the background and returns immediately.
func (s *Simulator) Start(ctx context.Context) (Status, error) {
	teams, over := s.Teams, s.Over
	if teams == 0 {
		teams = TeamsPerRun
	}
	if over == 0 {
		over = RunDuration
	}

	s.mu.Lock()
	if s.status.Running || (s.status.CanStartAt != nil && time.Now().Before(*s.status.CanStartAt)) {
		s.mu.Unlock()
		return s.Status(), ErrBusy
	}
	now := time.Now()
	s.status = Status{Running: true, StartedAt: &now, Total: teams}
	s.mu.Unlock()

	var existing int
	if err := s.DB.QueryRow(ctx, `select count(*) from teams where name like $1`, TeamPrefix+"%").Scan(&existing); err != nil {
		s.finish()
		return s.Status(), err
	}
	if existing+teams > MaxSimTeams {
		s.finish()
		return s.Status(), ErrLimit
	}
	members, err := s.createTeams(ctx, teams)
	if err != nil {
		s.finish()
		return s.Status(), err
	}
	go s.run(members, over)
	return s.Status(), nil
}

type member struct{ user, team string }

// createTeams adds teams with one member each. The members cannot sign in:
// their password hash matches no password.
func (s *Simulator) createTeams(ctx context.Context, n int) ([]member, error) {
	var hackathonID string
	if err := s.DB.QueryRow(ctx, `select id::text from hackathons order by created_at desc limit 1`).Scan(&hackathonID); err != nil {
		return nil, fmt.Errorf("simulate: no hackathon: %w", err)
	}
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	tag := hex.EncodeToString(run)

	batch := &pgx.Batch{}
	for i := range n {
		batch.Queue(`
			with u as (insert into users (email, display_name, password_hash) values ($1, $2, '!') returning id),
			     t as (insert into teams (hackathon_id, name) values ($3, $4) returning id),
			     m as (insert into team_members (team_id, user_id) select t.id, u.id from t, u returning team_id, user_id)
			select user_id::text, team_id::text from m`,
			fmt.Sprintf("sim-%s-%d%s", tag, i, emailDomain), fmt.Sprintf("Simulated member %d", i),
			hackathonID, fmt.Sprintf("%s%s-%03d", TeamPrefix, tag, i))
	}
	res := s.DB.SendBatch(ctx, batch)
	defer res.Close()
	out := make([]member, n)
	for i := range n {
		if err := res.QueryRow().Scan(&out[i].user, &out[i].team); err != nil {
			return nil, fmt.Errorf("simulate: create team %d: %w", i, err)
		}
	}
	return out, nil
}

func (s *Simulator) run(members []member, over time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), over+3*time.Minute)
	defer cancel()
	defer s.finish()

	body := make([]byte, FileBytes)
	copy(body, "%PDF-1.4\n% simulated submission\n")
	copy(body[FileBytes-7:], "\n%%EOF\n")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	// Arrivals speed up toward the end, like a real deadline: the i-th of n
	// starts at over * sqrt(i/n), so half arrive in the last ~30% of the run.
	start := time.Now()
	sem := make(chan struct{}, parallelPuts)
	var wg sync.WaitGroup
	for i, m := range members {
		at := time.Duration(float64(over) * sqrtFrac(i, len(members)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(start.Add(at))):
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			s.record(s.submit(ctx, m, i, body, sha) == nil)
		})
	}
	wg.Wait()
}

func sqrtFrac(i, n int) float64 { return math.Sqrt(float64(i) / float64(n)) }

func (s *Simulator) submit(ctx context.Context, m member, i int, body []byte, sha string) error {
	res, err := s.Subs.Create(ctx, m.user, m.team, submissions.CreateInput{
		IdempotencyKey: "sim-" + m.team,
		Title:          fmt.Sprintf("Simulated project %d", i),
		FileName:       "deck.pdf",
		FileSize:       int64(len(body)),
		FileSHA256:     sha,
	})
	if err != nil {
		return err
	}
	signed, err := s.Subs.SignParts(ctx, m.user, res.Version.ID, []int{1})
	if err != nil {
		return err
	}
	p := signed.Parts[0]
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	put, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		return fmt.Errorf("storage answered %d", put.StatusCode)
	}
	_, err = s.Subs.Complete(ctx, m.user, res.Version.ID)
	return err
}

func (s *Simulator) record(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.status.Completed++
	} else {
		s.status.Failed++
	}
}

func (s *Simulator) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	next := now.Add(Cooldown)
	s.status.Running = false
	s.status.FinishedAt = &now
	s.status.CanStartAt = &next
	if s.status.Failed > 0 {
		s.Log.Warn("simulation finished with failures", "completed", s.status.Completed, "failed", s.status.Failed)
	}
}
