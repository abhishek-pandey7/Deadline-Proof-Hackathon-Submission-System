package checks_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/checks"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/db"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/jobs"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testdb"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testfix"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/teststore"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func pngFile(t *testing.T) testfix.File {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b.Bytes())
	return testfix.File{Body: b.Bytes(), SHA256: hex.EncodeToString(sum[:])}
}

func newWorker(pool *pgxpool.Pool, store *storage.Store) *checks.Worker {
	return &checks.Worker{
		DB:  pool,
		Log: quiet,
		Runner: &checks.Runner{
			Store:   store,
			Links:   checks.NewLinkChecker(checks.LinkOptions{}),
			Archive: checks.DefaultArchiveLimits,
		},
		JobTimeout: 5 * time.Second,
	}
}

// startWorkers runs a River client that works check jobs until the test ends.
func startWorkers(t *testing.T, pool *pgxpool.Pool, w *checks.Worker, rescueAfter time.Duration) *river.Client[pgx.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	client, err := river.NewClient(riverpgxv5.New(pool), checks.ClientConfig(workers, 2, 5*time.Second, rescueAfter, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(ctx)
	})
	return client
}

// submit creates, uploads and completes one version whose upload has the
// given body but declares declared's hash.
func submit(t *testing.T, svc *submissions.Service, pool *pgxpool.Pool, name string, body []byte, declared string) (user, team, versionID string) {
	t.Helper()
	ctx := context.Background()
	user, _ = testfix.User(t, pool)
	team = testfix.Team(t, pool, testfix.Hackathon(t, pool, testfix.HackathonOpts{}), user)
	res, err := svc.Create(ctx, user, team, submissions.CreateInput{
		IdempotencyKey: "worker-key-01", Title: "Logo", FileName: name,
		FileSize: int64(len(body)), FileSHA256: declared,
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := svc.SignParts(ctx, user, res.Version.ID, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	testfix.Put(t, signed.Parts[0].Method, signed.Parts[0].URL, signed.Parts[0].Headers, body)
	if _, err := svc.Complete(ctx, user, res.Version.ID); err != nil {
		t.Fatal(err)
	}
	return user, team, res.Version.ID
}

func waitForState(t *testing.T, pool *pgxpool.Pool, id string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var state string
		if err := pool.QueryRow(context.Background(), `select state::text from versions where id = $1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "checking" {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("version %s still checking after %v", id, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func checkRows(t *testing.T, pool *pgxpool.Pool, id string) (rows, distinctTimes int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`select count(*), count(distinct checked_at) from check_results where version_id = $1`, id).Scan(&rows, &distinctTimes); err != nil {
		t.Fatal(err)
	}
	return rows, distinctTimes
}

func TestGoodVersionIsAcceptedAndRecordedOnce(t *testing.T) {
	pool := testdb.New(t)
	store := teststore.New(t)
	worker := newWorker(pool, store)
	client := startWorkers(t, pool, worker, time.Hour)
	svc := submissions.New(pool, store, client, quiet, submissions.Options{})
	f := pngFile(t)
	user, _, id := submit(t, svc, pool, "logo.png", f.Body, f.SHA256)

	if state := waitForState(t, pool, id, 30*time.Second); state != "accepted" {
		t.Fatalf("state = %s, want accepted", state)
	}
	rows, _ := checkRows(t, pool, id)
	if rows != 4 { // integrity, file_type, links, preview
		t.Fatalf("%d check rows, want 4", rows)
	}
	preview, err := svc.Preview(context.Background(), user, id)
	if err != nil || preview.Kind != submissions.PreviewImage || preview.URL == "" {
		t.Fatalf("preview = %+v, %v; want a signed image URL", preview, err)
	}

	// A slow duplicate attempt that wakes up after the first one finished
	// must change nothing.
	var before time.Time
	pool.QueryRow(context.Background(), `select max(checked_at) from check_results where version_id = $1`, id).Scan(&before)
	late := &river.Job[jobs.CheckVersion]{JobRow: &rivertype.JobRow{ID: -1, Attempt: 2}, Args: jobs.CheckVersion{VersionID: id}}
	if err := worker.Work(context.Background(), late); err != nil {
		t.Fatalf("late duplicate attempt: %v", err)
	}
	var after time.Time
	pool.QueryRow(context.Background(), `select max(checked_at) from check_results where version_id = $1`, id).Scan(&after)
	if rows2, _ := checkRows(t, pool, id); rows2 != 4 || !after.Equal(before) {
		t.Fatalf("late attempt rewrote results: %d rows, checked_at %v -> %v", rows2, before, after)
	}
}

func TestFileThatDoesNotMatchItsHashNeedsFixAndDoesNotCount(t *testing.T) {
	pool := testdb.New(t)
	store := teststore.New(t)
	client := startWorkers(t, pool, newWorker(pool, store), time.Hour)
	svc := submissions.New(pool, store, client, quiet, submissions.Options{})

	declared := pngFile(t)
	swapped := append([]byte(nil), declared.Body...)
	swapped[len(swapped)-1] ^= 0xff // same size, different bytes
	user, team, id := submit(t, svc, pool, "logo.png", swapped, declared.SHA256)

	if state := waitForState(t, pool, id, 30*time.Second); state != "needs_fix" {
		t.Fatalf("state = %s, want needs_fix", state)
	}
	var reason string
	pool.QueryRow(context.Background(), `select state_reason from versions where id = $1`, id).Scan(&reason)
	if !strings.Contains(reason, checks.CheckIntegrity) {
		t.Fatalf("reason %q does not name the integrity check", reason)
	}
	page, err := svc.ForTeam(context.Background(), user, team)
	if err != nil {
		t.Fatal(err)
	}
	if page.CountedVersionID != nil {
		t.Fatalf("a file that failed its hash check counts: %s", *page.CountedVersionID)
	}
}

// The crash test. A worker process runs the checks and is killed with
// SIGKILL (TerminateProcess on Windows) after the checks ran but before it
// committed. Nothing it did may be visible; River must retry the job on
// another worker; the results must be written exactly once.
func TestKilledWorkerIsRetriedWithoutDuplicateResults(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about a minute: waits for River's stuck-job rescuer")
	}
	pool := testdb.New(t)
	store := teststore.New(t)
	svc := submissions.New(pool, store, testfix.Queue(t, pool), quiet, submissions.Options{}) // enqueue only
	f := pngFile(t)
	_, _, id := submit(t, svc, pool, "logo.png", f.Body, f.SHA256)

	helper := exec.Command(os.Args[0], "-test.run=^TestHelperWorkerProcess$", "-test.count=1")
	helper.Env = append(os.Environ(),
		"H4H_HELPER_WORKER=1",
		"H4H_HELPER_DB="+pool.Config().ConnString(),
		"H4H_HELPER_BUCKET="+store.Bucket())
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "READY "); ok {
				ready <- v
			}
		}
	}()
	select {
	case got := <-ready:
		if got != id {
			t.Fatalf("helper worked %s, want %s", got, id)
		}
	case <-time.After(90 * time.Second):
		_ = helper.Process.Kill()
		t.Fatal("helper worker never reached the commit")
	}

	// The worker has finished its checks and is about to commit. Kill it.
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()

	var state, jobState string
	var attempt int
	pool.QueryRow(context.Background(), `select state::text from versions where id = $1`, id).Scan(&state)
	pool.QueryRow(context.Background(), `select state::text, attempt from river_job where kind = 'check_version'`).Scan(&jobState, &attempt)
	if rows, _ := checkRows(t, pool, id); rows != 0 || state != "checking" || jobState != "running" || attempt != 1 {
		t.Fatalf("after the kill: %d result rows, version %s, job %s attempt %d; want 0, checking, running, 1", rows, state, jobState, attempt)
	}

	// A healthy worker comes up. River's rescuer notices the dead attempt
	// and retries the job here.
	startWorkers(t, pool, newWorker(pool, store), 10*time.Second)
	if final := waitForState(t, pool, id, 150*time.Second); final != "accepted" {
		t.Fatalf("state after retry = %s, want accepted", final)
	}
	rows, times := checkRows(t, pool, id)
	pool.QueryRow(context.Background(), `select state::text, attempt from river_job where kind = 'check_version'`).Scan(&jobState, &attempt)
	if rows != 4 || times != 1 || jobState != "completed" || attempt != 2 {
		t.Fatalf("after retry: %d rows written at %d moments, job %s attempt %d; want 4 rows in 1 commit, completed, attempt 2", rows, times, jobState, attempt)
	}
}

// TestHelperWorkerProcess is the worker process the crash test kills. It
// only runs when the crash test starts it.
func TestHelperWorkerProcess(t *testing.T) {
	if os.Getenv("H4H_HELPER_WORKER") != "1" {
		t.Skip("started by TestKilledWorkerIsRetriedWithoutDuplicateResults")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, os.Getenv("H4H_HELPER_DB"), 4)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.New(storage.Config{
		Endpoint:        os.Getenv("R2_ENDPOINT"),
		Region:          os.Getenv("R2_REGION"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("H4H_HELPER_BUCKET"),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := newWorker(pool, store)
	w.JobTimeout = time.Hour
	w.BeforeCommit = func(versionID string) {
		fmt.Println("READY " + versionID)
		time.Sleep(time.Hour) // the test kills the process here
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	client, err := river.NewClient(riverpgxv5.New(pool), checks.ClientConfig(workers, 1, time.Hour, 2*time.Hour, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {}
}
