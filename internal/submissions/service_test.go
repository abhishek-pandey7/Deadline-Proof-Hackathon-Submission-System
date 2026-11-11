package submissions_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testdb"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testfix"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/teststore"
)

// S3 needs every part but the last to be at least 5 MiB.
const partSize = 5 << 20

type env struct {
	pool *pgxpool.Pool
	svc  *submissions.Service
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testdb.New(t)
	store := teststore.New(t)
	svc := submissions.New(pool, store, testfix.Queue(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)),
		submissions.Options{PartSize: partSize})
	return env{pool: pool, svc: svc}
}

func input(key string, f testfix.File) submissions.CreateInput {
	return submissions.CreateInput{
		IdempotencyKey: key,
		Title:          "Rocket",
		Description:    "Ships things",
		Links:          []string{"https://github.com/example/rocket"},
		FileName:       "rocket.zip",
		FileSize:       int64(len(f.Body)),
		FileSHA256:     f.SHA256,
	}
}

func code(err error) string {
	var e *submissions.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return "unexpected: " + err.Error()
	}
	return ""
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// upload sends the given parts of f for version id.
func upload(t *testing.T, e env, userID, id string, f testfix.File, parts ...int) {
	t.Helper()
	signed, err := e.svc.SignParts(context.Background(), userID, id, parts)
	if err != nil {
		t.Fatalf("sign %v: %v", parts, err)
	}
	for _, p := range signed.Parts {
		start := int64(p.Number-1) * partSize
		testfix.Put(t, p.Method, p.URL, p.Headers, f.Body[start:start+p.Size])
	}
}

func TestRetriedCreateReturnsTheSameVersion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := testfix.User(t, e.pool)
	team := testfix.Team(t, e.pool, testfix.Hackathon(t, e.pool, testfix.HackathonOpts{}), user)
	f := testfix.NewFile(1000)

	first, err := e.svc.Create(ctx, user, team, input("click-0001", f))
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Create(ctx, user, team, input("click-0001", f))
	if err != nil {
		t.Fatal(err)
	}
	if again.Version.ID != first.Version.ID || !again.Replayed {
		t.Fatalf("retry made version %s (replayed=%v), want %s", again.Version.ID, again.Replayed, first.Version.ID)
	}

	other := testfix.NewFile(2000)
	if _, err := e.svc.Create(ctx, user, team, input("click-0001", other)); code(err) != submissions.CodeIdempotencyMismatch {
		t.Fatalf("reused key for another file: %v, want idempotency_mismatch", err)
	}
	if n := count(t, e.pool, `select count(*) from versions where team_id = $1`, team); n != 1 {
		t.Fatalf("%d versions, want 1", n)
	}
}

// Fifty simultaneous double-clicks with one idempotency key make one version.
func TestConcurrentRetriesMakeOneVersion(t *testing.T) {
	e := newEnv(t)
	user, _ := testfix.User(t, e.pool)
	team := testfix.Team(t, e.pool, testfix.Hackathon(t, e.pool, testfix.HackathonOpts{}), user)
	f := testfix.NewFile(1000)

	const n = 50
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			res, err := e.svc.Create(context.Background(), user, team, input("same-key-42", f))
			ids[i], errs[i] = res.Version.ID, err
		})
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("call %d got version %s, call 0 got %s", i, ids[i], ids[0])
		}
	}
	if got := count(t, e.pool, `select count(*) from versions where team_id = $1`, team); got != 1 {
		t.Fatalf("%d versions, want 1", got)
	}
	var reserved int64
	if err := e.pool.QueryRow(context.Background(), `select reserved_bytes from hackathons h join teams t on t.hackathon_id = h.id where t.id = $1`, team).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != int64(len(f.Body)) {
		t.Fatalf("reserved %d bytes, want %d (one reservation)", reserved, len(f.Body))
	}
}

// The storage budget holds when many teams create versions at the same instant.
func TestStorageBudgetHoldsUnderConcurrentTeams(t *testing.T) {
	e := newEnv(t)
	const size, fits, teams = 1000, 5, 30
	hack := testfix.Hackathon(t, e.pool, testfix.HackathonOpts{StorageBudget: size * fits})
	f := testfix.NewFile(size)

	type member struct{ user, team string }
	members := make([]member, teams)
	for i := range members {
		u, _ := testfix.User(t, e.pool)
		members[i] = member{u, testfix.Team(t, e.pool, hack, u)}
	}

	results := make([]string, teams)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, m := range members {
		wg.Go(func() {
			<-start
			_, err := e.svc.Create(context.Background(), m.user, m.team, input(fmt.Sprintf("team-key-%03d", i), f))
			results[i] = code(err)
		})
	}
	close(start)
	wg.Wait()

	ok, full := 0, 0
	for _, c := range results {
		switch c {
		case "":
			ok++
		case submissions.CodeStorageFull:
			full++
		default:
			t.Fatalf("unexpected result %q", c)
		}
	}
	if ok != fits || full != teams-fits {
		t.Fatalf("%d created, %d refused; want %d and %d", ok, full, fits, teams-fits)
	}
	if got := count(t, e.pool, `select reserved_bytes::int from hackathons where id = $1`, hack); got != size*fits {
		t.Fatalf("reserved %d, want %d", got, size*fits)
	}
}

func TestCreateAfterDeadlineIsRefusedAndLogged(t *testing.T) {
	e := newEnv(t)
	user, _ := testfix.User(t, e.pool)
	hack := testfix.Hackathon(t, e.pool, testfix.HackathonOpts{DeadlineIn: -time.Second})
	team := testfix.Team(t, e.pool, hack, user)

	_, err := e.svc.Create(context.Background(), user, team, input("late-key-01", testfix.NewFile(10)))
	if code(err) != submissions.CodeDeadlinePassed {
		t.Fatalf("err = %v, want deadline_passed", err)
	}
	if n := count(t, e.pool, `select count(*) from late_attempts where team_id = $1 and kind = 'create_after_deadline'`, team); n != 1 {
		t.Fatalf("%d late attempts logged, want 1", n)
	}
	if n := count(t, e.pool, `select count(*) from versions where team_id = $1`, team); n != 0 {
		t.Fatalf("%d versions created after the deadline", n)
	}
}

func TestTeamLimitsAreEnforced(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := testfix.User(t, e.pool)
	hack := testfix.Hackathon(t, e.pool, testfix.HackathonOpts{MaxVersions: 2, MaxFileBytes: 500})
	team := testfix.Team(t, e.pool, hack, user)
	small := testfix.NewFile(100)

	if _, err := e.svc.Create(ctx, user, team, input("too-big-001", testfix.NewFile(501))); code(err) != submissions.CodeFileTooLarge {
		t.Fatalf("oversized file: %v, want file_too_large", err)
	}
	for i := range 2 {
		if _, err := e.svc.Create(ctx, user, team, input(fmt.Sprintf("limit-key-%d", i), small)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.Create(ctx, user, team, input("limit-key-9", small)); code(err) != submissions.CodeVersionLimit {
		t.Fatalf("third version: %v, want version_limit", err)
	}
	if _, err := e.pool.Exec(ctx, `update hackathons set uploads_enabled = false, max_versions_per_team = 10 where id = $1`, hack); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, user, team, input("paused-key-1", small)); code(err) != submissions.CodeUploadsDisabled {
		t.Fatalf("paused uploads: %v, want uploads_disabled", err)
	}
}

func TestOnlyMembersCanTouchATeam(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	member, _ := testfix.User(t, e.pool)
	stranger, _ := testfix.User(t, e.pool)
	team := testfix.Team(t, e.pool, testfix.Hackathon(t, e.pool, testfix.HackathonOpts{}), member)
	f := testfix.NewFile(100)

	if _, err := e.svc.Create(ctx, stranger, team, input("stranger-01", f)); code(err) != submissions.CodeNotFound {
		t.Fatalf("stranger create: %v, want not_found", err)
	}
	res, err := e.svc.Create(ctx, member, team, input("member-0001", f))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Version.ID
	if _, err := e.svc.SignParts(ctx, stranger, id, []int{1}); code(err) != submissions.CodeNotFound {
		t.Errorf("stranger sign: %v", err)
	}
	if _, err := e.svc.Uploaded(ctx, stranger, id); code(err) != submissions.CodeNotFound {
		t.Errorf("stranger list parts: %v", err)
	}
	if _, err := e.svc.Complete(ctx, stranger, id); code(err) != submissions.CodeNotFound {
		t.Errorf("stranger complete: %v", err)
	}
	if _, err := e.svc.ForTeam(ctx, stranger, team); code(err) != submissions.CodeNotFound {
		t.Errorf("stranger team page: %v", err)
	}
}

// A dropped connection resumes: storage reports which parts arrived and the
// client sends only the rest. Completing twice enqueues one check job.
func TestUploadResumesAndCompletesExactlyOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := testfix.User(t, e.pool)
	team := testfix.Team(t, e.pool, testfix.Hackathon(t, e.pool, testfix.HackathonOpts{}), user)
	f := testfix.NewFile(2*partSize + 123) // three parts

	res, err := e.svc.Create(ctx, user, team, input("resume-0001", f))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Version.ID
	if res.Version.PartCount != 3 {
		t.Fatalf("part count %d, want 3", res.Version.PartCount)
	}

	upload(t, e, user, id, f, 1, 3) // the connection drops before part 2

	status, err := e.svc.Uploaded(ctx, user, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Uploaded) != 2 || status.Uploaded[0].Number != 1 || status.Uploaded[1].Number != 3 {
		t.Fatalf("uploaded = %+v, want parts 1 and 3", status.Uploaded)
	}
	_, err = e.svc.Complete(ctx, user, id)
	var serr *submissions.Error
	if !errors.As(err, &serr) || serr.Code != submissions.CodePartsMissing || fmt.Sprint(serr.Details["missing"]) != "[2]" {
		t.Fatalf("complete with a gap: %v, want parts_missing [2]", err)
	}

	upload(t, e, user, id, f, 2)
	v, err := e.svc.Complete(ctx, user, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "checking" || v.CompletedAt == nil {
		t.Fatalf("state %s completed_at %v, want checking with a time", v.State, v.CompletedAt)
	}
	if again, err := e.svc.Complete(ctx, user, id); err != nil || again.ID != id {
		t.Fatalf("second complete: %v", err)
	}
	if n := count(t, e.pool, `select count(*) from river_job where kind = 'check_version' and args->>'version_id' = $1`, id); n != 1 {
		t.Fatalf("%d check jobs, want exactly 1", n)
	}

	page, err := e.svc.ForTeam(ctx, user, team)
	if err != nil {
		t.Fatal(err)
	}
	if page.CountedVersionID == nil || *page.CountedVersionID != id {
		t.Fatalf("counted version %v, want %s", page.CountedVersionID, id)
	}
}

// After the deadline, an upload started before it may still finish inside
// grace; once grace ends it expires and gives its storage back.
func TestGraceWindowIsBytesOnlyAndEnds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := testfix.User(t, e.pool)
	hack := testfix.Hackathon(t, e.pool, testfix.HackathonOpts{})
	team := testfix.Team(t, e.pool, hack, user)
	f := testfix.NewFile(1000)

	inGrace, err := e.svc.Create(ctx, user, team, input("grace-key-1", f))
	if err != nil {
		t.Fatal(err)
	}
	tooLate, err := e.svc.Create(ctx, user, team, input("grace-key-2", f))
	if err != nil {
		t.Fatal(err)
	}

	// The deadline (an hour away) passed a minute ago; 14 minutes of grace remain.
	testfix.Advance(t, e.pool, hack, time.Hour+time.Minute)
	if _, err := e.svc.Create(ctx, user, team, input("grace-key-3", f)); code(err) != submissions.CodeDeadlinePassed {
		t.Fatalf("new version after deadline: %v", err)
	}
	signed, err := e.svc.SignParts(ctx, user, inGrace.Version.ID, []int{1})
	if err != nil {
		t.Fatalf("sign in grace: %v", err)
	}
	if signed.ExpiresAt.After(signed.Window.GraceEndsAt) {
		t.Fatalf("url expires %v, after grace ends %v", signed.ExpiresAt, signed.Window.GraceEndsAt)
	}
	testfix.Put(t, signed.Parts[0].Method, signed.Parts[0].URL, signed.Parts[0].Headers, f.Body)
	if _, err := e.svc.Complete(ctx, user, inGrace.Version.ID); err != nil {
		t.Fatalf("complete in grace: %v", err)
	}

	// Twenty minutes later grace is over too.
	testfix.Advance(t, e.pool, hack, 20*time.Minute)
	if _, err := e.svc.SignParts(ctx, user, tooLate.Version.ID, []int{1}); code(err) != submissions.CodeGraceOver {
		t.Fatalf("sign after grace: %v, want grace_over", err)
	}
	if n := count(t, e.pool, `select count(*) from versions where id = $1 and state = 'expired'`, tooLate.Version.ID); n != 1 {
		t.Fatal("late version not expired")
	}
	if n := count(t, e.pool, `select reserved_bytes::int from hackathons where id = $1`, hack); n != len(f.Body) {
		t.Fatalf("reserved %d bytes, want %d (expired version released)", n, len(f.Body))
	}
	if n := count(t, e.pool, `select count(*) from late_attempts where team_id = $1 and kind = 'sign_after_grace'`, team); n != 1 {
		t.Fatalf("%d sign_after_grace attempts logged, want 1", n)
	}

	page, err := e.svc.ForTeam(ctx, user, team)
	if err != nil {
		t.Fatal(err)
	}
	if page.CountedVersionID == nil || *page.CountedVersionID != inGrace.Version.ID {
		t.Fatalf("counted %v, want the version finished in grace", page.CountedVersionID)
	}
}

// An upload abandoned mid-way gives its storage back once grace ends, even
// though no browser ever calls again.
func TestSweepExpiresAbandonedUploads(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := testfix.User(t, e.pool)
	hack := testfix.Hackathon(t, e.pool, testfix.HackathonOpts{})
	team := testfix.Team(t, e.pool, hack, user)
	f := testfix.NewFile(1000)

	abandoned, err := e.svc.Create(ctx, user, team, input("abandon-key1", f))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.ExpireStale(ctx); err != nil || n != 0 {
		t.Fatalf("sweep before the deadline expired %d (%v), want 0", n, err)
	}
	testfix.Advance(t, e.pool, hack, time.Hour+20*time.Minute)
	if n, err := e.svc.ExpireStale(ctx); err != nil || n != 1 {
		t.Fatalf("sweep after grace expired %d (%v), want 1", n, err)
	}
	if n := count(t, e.pool, `select count(*) from versions where id = $1 and state = 'expired'`, abandoned.Version.ID); n != 1 {
		t.Fatal("abandoned upload not expired")
	}
	if n := count(t, e.pool, `select reserved_bytes::int from hackathons where id = $1`, hack); n != 0 {
		t.Fatalf("%d bytes still reserved, want 0", n)
	}
	if n := count(t, e.pool, `select count(*) from late_attempts where team_id = $1`, team); n != 0 {
		t.Fatalf("the sweep logged %d late attempts; it is not a team's attempt", n)
	}
}

// A broken last-second re-upload never replaces a good earlier version.
func TestUnfinishedNewerVersionKeepsTheOlderOneCounting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := testfix.User(t, e.pool)
	hack := testfix.Hackathon(t, e.pool, testfix.HackathonOpts{})
	team := testfix.Team(t, e.pool, hack, user)
	f := testfix.NewFile(1000)

	good, err := e.svc.Create(ctx, user, team, input("good-key-01", f))
	if err != nil {
		t.Fatal(err)
	}
	upload(t, e, user, good.Version.ID, f, 1)
	if _, err := e.svc.Complete(ctx, user, good.Version.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, user, team, input("broken-key-1", testfix.NewFile(2000))); err != nil {
		t.Fatal(err)
	}

	testfix.Advance(t, e.pool, hack, time.Hour+20*time.Minute) // past deadline and grace
	page, err := e.svc.ForTeam(ctx, user, team)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Versions) != 2 || page.CountedVersionID == nil || *page.CountedVersionID != good.Version.ID {
		t.Fatalf("counted %v of %d versions, want the finished older one", page.CountedVersionID, len(page.Versions))
	}
}
