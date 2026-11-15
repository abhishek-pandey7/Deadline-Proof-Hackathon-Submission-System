package simulate_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/simulate"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testdb"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testfix"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/teststore"
)

func TestSimulationSubmitsEveryTeamAndCoolsDown(t *testing.T) {
	pool := testdb.New(t)
	store := teststore.New(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	subs := submissions.New(pool, store, testfix.Queue(t, pool), quiet, submissions.Options{})
	testfix.Hackathon(t, pool, testfix.HackathonOpts{})
	sim := &simulate.Simulator{DB: pool, Subs: subs, Log: quiet, HTTP: http.DefaultClient, Teams: 8, Over: time.Second}

	if _, err := sim.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.Start(context.Background()); !errors.Is(err, simulate.ErrBusy) {
		t.Fatalf("second start while running: %v, want ErrBusy", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for sim.Status().Running {
		if time.Now().After(deadline) {
			t.Fatal("simulation never finished")
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := sim.Status()
	if st.Completed != 8 || st.Failed != 0 {
		t.Fatalf("completed %d failed %d, want 8 and 0", st.Completed, st.Failed)
	}
	var checking int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from versions v join teams t on t.id = v.team_id
		where t.name like 'Sim team %' and v.state = 'checking'`).Scan(&checking); err != nil {
		t.Fatal(err)
	}
	if checking != 8 {
		t.Fatalf("%d simulated versions waiting for checks, want 8", checking)
	}
	if _, err := sim.Start(context.Background()); !errors.Is(err, simulate.ErrBusy) {
		t.Fatalf("start during cooldown: %v, want ErrBusy", err)
	}
}
