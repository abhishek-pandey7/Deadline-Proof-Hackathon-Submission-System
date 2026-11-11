package submissions_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testfix"
)

func ledger(t *testing.T, e env) submissions.Ledger {
	t.Helper()
	l, err := submissions.ReadLedger(context.Background(), e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// R2's free tier is per account, so the cap must hold across hackathons
// even when each hackathon's own budget would allow more.
func TestAccountWideCapHoldsAcrossHackathons(t *testing.T) {
	e := newEnv(t)
	const size, fits, teams = 1000, 5, 30
	if err := submissions.SetStorageCap(context.Background(), e.pool, size*fits); err != nil {
		t.Fatal(err)
	}
	hacks := []string{testfix.Hackathon(t, e.pool, testfix.HackathonOpts{}), testfix.Hackathon(t, e.pool, testfix.HackathonOpts{})}
	f := testfix.NewFile(size)

	type member struct{ user, team string }
	members := make([]member, teams)
	for i := range members {
		u, _ := testfix.User(t, e.pool)
		members[i] = member{u, testfix.Team(t, e.pool, hacks[i%2], u)}
	}
	results := make([]string, teams)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, m := range members {
		wg.Go(func() {
			<-start
			_, err := e.svc.Create(context.Background(), m.user, m.team, input(fmt.Sprintf("cap-key-%03d", i), f))
			results[i] = code(err)
		})
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, c := range results {
		switch c {
		case "":
			ok++
		case submissions.CodeStorageFull:
		default:
			t.Fatalf("unexpected result %q", c)
		}
	}
	if l := ledger(t, e); ok != fits || l.ReservedBytes != size*fits {
		t.Fatalf("%d created, %d bytes reserved; want %d and %d", ok, l.ReservedBytes, fits, size*fits)
	}
}

func TestLedgerCountsUploadsAndGivesBackExpiredSpace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := testfix.User(t, e.pool)
	hack := testfix.Hackathon(t, e.pool, testfix.HackathonOpts{})
	team := testfix.Team(t, e.pool, hack, user)
	kept, dropped := testfix.NewFile(1000), testfix.NewFile(3000)

	v, err := e.svc.Create(ctx, user, team, input("ledger-key-1", kept))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, user, team, input("ledger-key-2", dropped)); err != nil {
		t.Fatal(err)
	}
	if l := ledger(t, e); l.ReservedBytes != 4000 || l.UploadedBytesTotal != 0 {
		t.Fatalf("after two creates: reserved %d uploaded %d, want 4000 and 0", l.ReservedBytes, l.UploadedBytesTotal)
	}

	upload(t, e, user, v.Version.ID, kept, 1)
	if _, err := e.svc.Complete(ctx, user, v.Version.ID); err != nil {
		t.Fatal(err)
	}
	if l := ledger(t, e); l.UploadedBytesTotal != 1000 {
		t.Fatalf("uploaded total %d after one finished upload, want 1000", l.UploadedBytesTotal)
	}

	testfix.Advance(t, e.pool, hack, time.Hour+20*time.Minute)
	if _, err := e.svc.ExpireStale(ctx); err != nil {
		t.Fatal(err)
	}
	if l := ledger(t, e); l.ReservedBytes != 1000 || l.UploadedBytesTotal != 1000 {
		t.Fatalf("after expiry: reserved %d uploaded %d, want 1000 and 1000", l.ReservedBytes, l.UploadedBytesTotal)
	}
}

// If the bucket holds more than the app counted (a file put there by hand,
// a bug), the measurement wins and the limiter gets stricter.
func TestMeasuredBucketMakesTheLimiterStricter(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := submissions.SetStorageCap(ctx, e.pool, 5000); err != nil {
		t.Fatal(err)
	}
	user, _ := testfix.User(t, e.pool)
	team := testfix.Team(t, e.pool, testfix.Hackathon(t, e.pool, testfix.HackathonOpts{}), user)

	if err := e.store.Put(ctx, "outside-the-app.bin", "application/octet-stream", make([]byte, 4000)); err != nil {
		t.Fatal(err)
	}
	l, err := e.svc.MeasureStorage(ctx, e.store)
	if err != nil {
		t.Fatal(err)
	}
	if l.MeasuredBytes == nil || *l.MeasuredBytes != 4000 || l.UsedBytes != 4000 {
		t.Fatalf("measured %v, used %d; want 4000 for both", l.MeasuredBytes, l.UsedBytes)
	}
	if _, err := e.svc.Create(ctx, user, team, input("too-much-01", testfix.NewFile(2000))); code(err) != submissions.CodeStorageFull {
		t.Fatalf("2000 bytes on top of a measured 4000 under a 5000 cap: %v, want storage_full", err)
	}
	if _, err := e.svc.Create(ctx, user, team, input("just-fits-01", testfix.NewFile(1000))); err != nil {
		t.Fatalf("1000 bytes still fit: %v", err)
	}
}
