// Package testfix creates hackathons, users and teams for tests.
package testfix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"golang.org/x/crypto/bcrypt"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/auth"
)

// Password is every fixture user's password.
const Password = "correct horse battery"

var (
	seq          atomic.Int64
	passwordHash = mustHash()
)

func mustHash() string {
	h, err := auth.HashPassword(Password, bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return h
}

// HackathonOpts overrides hackathon defaults. Zero values keep the defaults.
type HackathonOpts struct {
	DeadlineIn      time.Duration // from now; default one hour
	GraceSeconds    int
	MaxFileBytes    int64
	MaxVersions     int
	StorageBudget   int64
	UploadsDisabled bool
}

// Hackathon inserts a hackathon and returns its id.
func Hackathon(t testing.TB, pool *pgxpool.Pool, o HackathonOpts) string {
	t.Helper()
	if o.DeadlineIn == 0 {
		o.DeadlineIn = time.Hour
	}
	if o.GraceSeconds == 0 {
		o.GraceSeconds = 900
	}
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = 100 << 20
	}
	if o.MaxVersions == 0 {
		o.MaxVersions = 10
	}
	if o.StorageBudget == 0 {
		o.StorageBudget = 8 << 30
	}
	var id string
	err := pool.QueryRow(context.Background(), `
		insert into hackathons (slug, name, deadline_at, grace_seconds, max_file_bytes, max_versions_per_team, storage_budget_bytes, uploads_enabled)
		values ($1, 'Test hackathon', now() + $2::interval, $3, $4, $5, $6, $7) returning id::text`,
		fmt.Sprintf("hack-%d", seq.Add(1)), fmt.Sprintf("%d milliseconds", o.DeadlineIn.Milliseconds()),
		o.GraceSeconds, o.MaxFileBytes, o.MaxVersions, o.StorageBudget, !o.UploadsDisabled).Scan(&id)
	if err != nil {
		t.Fatalf("testfix: hackathon: %v", err)
	}
	return id
}

// Advance simulates d passing for one hackathon: its deadline and every
// timestamp its versions recorded move d into the past, exactly as if the
// clock had moved forward.
func Advance(t testing.TB, pool *pgxpool.Pool, hackathonID string, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	shift := fmt.Sprintf("%d milliseconds", d.Milliseconds())
	if _, err := pool.Exec(ctx, `update hackathons set deadline_at = deadline_at - $2::interval where id = $1`, hackathonID, shift); err != nil {
		t.Fatalf("testfix: advance deadline: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		update versions v set created_at = v.created_at - $2::interval,
		                      completed_at = v.completed_at - $2::interval,
		                      checks_finished_at = v.checks_finished_at - $2::interval
		from teams t where t.id = v.team_id and t.hackathon_id = $1`, hackathonID, shift); err != nil {
		t.Fatalf("testfix: advance versions: %v", err)
	}
}

// User inserts a user with Password and returns its id and email.
func User(t testing.TB, pool *pgxpool.Pool) (id, email string) {
	t.Helper()
	email = fmt.Sprintf("user%d@example.test", seq.Add(1))
	err := pool.QueryRow(context.Background(),
		`insert into users (email, display_name, password_hash) values ($1, $2, $3) returning id::text`,
		email, "User "+email, passwordHash).Scan(&id)
	if err != nil {
		t.Fatalf("testfix: user: %v", err)
	}
	return id, email
}

// Team inserts a team with the given members and returns its id.
func Team(t testing.TB, pool *pgxpool.Pool, hackathonID string, memberIDs ...string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `insert into teams (hackathon_id, name) values ($1, $2) returning id::text`,
		hackathonID, fmt.Sprintf("Team %d", seq.Add(1))).Scan(&id); err != nil {
		t.Fatalf("testfix: team: %v", err)
	}
	for _, m := range memberIDs {
		if _, err := pool.Exec(ctx, `insert into team_members (team_id, user_id) values ($1, $2)`, id, m); err != nil {
			t.Fatalf("testfix: member: %v", err)
		}
	}
	return id
}

// Queue returns an insert-only River client.
func Queue(t testing.TB, pool *pgxpool.Pool) *river.Client[pgx.Tx] {
	t.Helper()
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatalf("testfix: river client: %v", err)
	}
	return c
}

// File is test content with its hash.
type File struct {
	Body   []byte
	SHA256 string
}

// NewFile returns size bytes of deterministic content.
func NewFile(size int) File {
	b := bytes.Repeat([]byte("deadline-crush "), size/15+1)[:size]
	sum := sha256.Sum256(b)
	return File{Body: b, SHA256: hex.EncodeToString(sum[:])}
}

// Put sends one presigned part upload and fails the test unless it succeeds.
func Put(t testing.TB, method, url string, headers map[string]string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("testfix: part upload status %d", res.StatusCode)
	}
}
