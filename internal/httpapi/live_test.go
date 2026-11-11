package httpapi_test

import (
	"bufio"
	"context"
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/organizer"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testfix"
)

type sse struct {
	name, data string
}

// openStream connects to an event stream and returns its events. The stream
// closes when the test ends.
func (c *client) openStream(path string) (*http.Response, <-chan sse) {
	c.a.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.a.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.a.srv.URL+path, nil)
	res, err := c.http.Do(req)
	if err != nil {
		c.a.t.Fatal(err)
	}
	out := make(chan sse, 32)
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		close(out)
		return res, out
	}
	go func() {
		defer res.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(res.Body)
		var ev sse
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				out <- ev
				ev = sse{}
			}
		}
	}()
	return res, out
}

func next(t *testing.T, events <-chan sse, within time.Duration) sse {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(within):
		t.Fatalf("no event within %v", within)
		return sse{}
	}
}

func createVersion(t *testing.T, a api, user, team, key string) string {
	t.Helper()
	f := testfix.NewFile(100)
	res, err := a.svc.Create(context.Background(), user, team, submissions.CreateInput{
		IdempotencyKey: key, Title: "Rocket", FileName: "rocket.zip", FileSize: int64(len(f.Body)), FileSHA256: f.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Version.ID
}

func TestTeamStreamPushesOnlyThatTeamsCommittedChanges(t *testing.T) {
	a := newAPI(t)
	hack := testfix.Hackathon(t, a.pool, testfix.HackathonOpts{})
	alice, aliceEmail := testfix.User(t, a.pool)
	bob, bobEmail := testfix.User(t, a.pool)
	aliceTeam := testfix.Team(t, a.pool, hack, alice)
	bobTeam := testfix.Team(t, a.pool, hack, bob)

	ac := a.client()
	ac.login(aliceEmail)
	_, aliceEvents := ac.openStream("/api/teams/" + aliceTeam + "/events")
	if ev := next(t, aliceEvents, 5*time.Second); ev.name != "ready" {
		t.Fatalf("first event %q, want ready", ev.name)
	}
	bc := a.client()
	bc.login(bobEmail)
	_, bobEvents := bc.openStream("/api/teams/" + bobTeam + "/events")
	next(t, bobEvents, 5*time.Second) // ready

	id := createVersion(t, a, alice, aliceTeam, "live-key-001")
	ev := next(t, aliceEvents, 5*time.Second)
	if ev.name != "version" || !strings.Contains(ev.data, id) || !strings.Contains(ev.data, `"uploading"`) {
		t.Fatalf("event %+v, want version %s uploading", ev, id)
	}
	select {
	case ev := <-bobEvents:
		t.Fatalf("another team's stream got %+v", ev)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestStreamsAreForMembersAndCappedPerUser(t *testing.T) {
	a := newAPI(t)
	hack := testfix.Hackathon(t, a.pool, testfix.HackathonOpts{})
	member, email := testfix.User(t, a.pool)
	stranger, strangerEmail := testfix.User(t, a.pool)
	team := testfix.Team(t, a.pool, hack, member)
	testfix.Team(t, a.pool, hack, stranger)

	s := a.client()
	s.login(strangerEmail)
	if res, _ := s.openStream("/api/teams/" + team + "/events"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("stranger stream: %d, want 404", res.StatusCode)
	}

	c := a.client()
	c.login(email)
	for i := range 4 {
		res, evs := c.openStream("/api/teams/" + team + "/events")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %d", i+1, res.StatusCode)
		}
		next(t, evs, 5*time.Second)
	}
	if res, _ := c.openStream("/api/teams/" + team + "/events"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("fifth stream: %d, want 429", res.StatusCode)
	}
}

// If the LISTEN connection drops, the hub reconnects and tells every stream
// to refetch, because changes during the outage were never delivered.
func TestStreamsResyncAfterTheListenerConnectionDies(t *testing.T) {
	a := newAPI(t)
	user, email := testfix.User(t, a.pool)
	team := testfix.Team(t, a.pool, testfix.Hackathon(t, a.pool, testfix.HackathonOpts{}), user)
	c := a.client()
	c.login(email)
	_, evs := c.openStream("/api/teams/" + team + "/events")
	next(t, evs, 5*time.Second) // ready

	if _, err := a.pool.Exec(context.Background(), `select pg_terminate_backend($1)`, int64(a.hub.ListenerPID())); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, evs, 15*time.Second); ev.name != "resync" {
		t.Fatalf("after the listener died: %+v, want resync", ev)
	}
	id := createVersion(t, a, user, team, "after-resync-1")
	if ev := next(t, evs, 5*time.Second); ev.name != "version" || !strings.Contains(ev.data, id) {
		t.Fatalf("after reconnecting: %+v, want the new version", ev)
	}
}

func TestOrganizerDashboardIsOrganizersOnly(t *testing.T) {
	a := newAPI(t)
	hack := testfix.Hackathon(t, a.pool, testfix.HackathonOpts{})
	user, email := testfix.User(t, a.pool)
	team := testfix.Team(t, a.pool, hack, user)
	testfix.Team(t, a.pool, hack) // a team that never submits
	createVersion(t, a, user, team, "stats-key-01")

	c := a.client()
	c.login(email)
	for _, p := range []string{"/api/organizer/stats", "/api/organizer/export.csv"} {
		if res := c.call("GET", p, nil, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s as a team member: %d, want 404", p, res.StatusCode)
		}
	}

	orgID, orgEmail := testfix.User(t, a.pool)
	if _, err := a.pool.Exec(context.Background(), `update users set is_organizer = true where id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	o := a.client()
	o.login(orgEmail)
	var st organizer.Stats
	if res := o.call("GET", "/api/organizer/stats", nil, nil, &st); res.StatusCode != http.StatusOK {
		t.Fatalf("stats: %d", res.StatusCode)
	}
	if st.Teams != 2 || st.Versions["uploading"] != 1 || st.TeamsSubmitted != 0 {
		t.Fatalf("stats = teams %d, uploading %d, submitted %d; want 2, 1, 0", st.Teams, st.Versions["uploading"], st.TeamsSubmitted)
	}
	_, orgEvents := o.openStream("/api/organizer/events")
	next(t, orgEvents, 5*time.Second) // ready
	createVersion(t, a, user, team, "stats-key-02")
	if ev := next(t, orgEvents, 5*time.Second); ev.name != "version" {
		t.Fatalf("organizer stream: %+v, want a version event", ev)
	}
}

func TestCSVAgreesWithTheTeamPageAndDefusesFormulas(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	hack := testfix.Hackathon(t, a.pool, testfix.HackathonOpts{})
	user, _ := testfix.User(t, a.pool)
	team := testfix.Team(t, a.pool, hack, user)
	testfix.Team(t, a.pool, hack) // no submission

	f := testfix.NewFile(100)
	res, err := a.svc.Create(ctx, user, team, submissions.CreateInput{
		IdempotencyKey: "csv-key-0001", Title: `=HYPERLINK("https://evil.example","click")`,
		FileName: "rocket.zip", FileSize: int64(len(f.Body)), FileSHA256: f.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := a.svc.SignParts(ctx, user, res.Version.ID, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	testfix.Put(t, signed.Parts[0].Method, signed.Parts[0].URL, signed.Parts[0].Headers, f.Body)
	if _, err := a.svc.Complete(ctx, user, res.Version.ID); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	if err := (organizer.Service{DB: a.pool}).WriteCSV(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || strings.Join(records[0], ",") != strings.Join(organizer.CSVHeader, ",") {
		t.Fatalf("csv has %d rows, header %v", len(records), records[0])
	}
	page, err := a.svc.ForTeam(ctx, user, team)
	if err != nil {
		t.Fatal(err)
	}
	var row []string
	for _, r := range records[1:] {
		if r[1] == team {
			row = r
		} else if r[2] != "" {
			t.Errorf("team without a submission shows counted version %q", r[2])
		}
	}
	if row == nil || page.CountedVersionID == nil || row[2] != "1" || row[7] != f.SHA256 {
		t.Fatalf("csv row %v disagrees with the team page (counted %v)", row, page.CountedVersionID)
	}
	if !strings.HasPrefix(row[4], "'=") {
		t.Fatalf("title cell %q was not defused", row[4])
	}
}
