// Command seed creates a hackathon with demo teams. It is safe to run again:
// existing rows are updated, not duplicated.
//
//	go run ./cmd/seed -env-file dev.env                       # 3 demo teams
//	go run ./cmd/seed -env-file dev.env -teams 2000 -tokens-out loadtest/tokens.csv
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/auth"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/config"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	envFile := flag.String("env-file", "", "optional KEY=VALUE file")
	teams := flag.Int("teams", 3, "number of teams")
	members := flag.Int("members", 2, "members per team")
	deadlineIn := flag.Duration("deadline-in", 72*time.Hour, "deadline, from now")
	grace := flag.Duration("grace", 15*time.Minute, "grace for uploads started before the deadline")
	password := flag.String("password", "demo-pass-2026", "password for every seeded account")
	cost := flag.Int("bcrypt-cost", 10, "bcrypt cost (4 is fine for load-test accounts)")
	tokensOut := flag.String("tokens-out", "", "write team_id,session_token per team to this CSV (for the load test)")
	flag.Parse()

	if *envFile != "" {
		if err := config.LoadEnvFile(*envFile); err != nil {
			return err
		}
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url, 4)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	// One hash for every seeded account keeps seeding thousands of users fast.
	hash, err := auth.HashPassword(*password, *cost)
	if err != nil {
		return err
	}

	var hackathonID string
	err = pool.QueryRow(ctx, `
		insert into hackathons (slug, name, deadline_at, grace_seconds)
		values ('hack4hack-2026', 'Hack4Hack 2026', now() + $1::interval, $2)
		on conflict (slug) do update set deadline_at = excluded.deadline_at, grace_seconds = excluded.grace_seconds
		returning id::text`,
		fmt.Sprintf("%d seconds", int(deadlineIn.Seconds())), int(grace.Seconds())).Scan(&hackathonID)
	if err != nil {
		return fmt.Errorf("hackathon: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into users (email, display_name, password_hash, is_organizer)
		values ('organizer@demo.test', 'Organizer', $1, true)
		on conflict (email) do update set password_hash = excluded.password_hash, is_organizer = true`, hash); err != nil {
		return fmt.Errorf("organizer: %w", err)
	}

	type seeded struct{ teamID, firstUserID string }
	out := make([]seeded, 0, *teams)
	for start := 0; start < *teams; start += 200 {
		end := min(start+200, *teams)
		batch := &pgx.Batch{}
		for n := start + 1; n <= end; n++ {
			batch.Queue(`
				with t as (
					insert into teams (hackathon_id, name) values ($1, $2)
					on conflict (hackathon_id, name) do update set name = excluded.name
					returning id
				)
				select id::text from t`, hackathonID, fmt.Sprintf("Team %d", n))
			for m := 1; m <= *members; m++ {
				batch.Queue(`
					insert into users (email, display_name, password_hash) values ($1, $2, $3)
					on conflict (email) do update set password_hash = excluded.password_hash
					returning id::text`,
					fmt.Sprintf("team%d-member%d@demo.test", n, m), fmt.Sprintf("Team %d member %d", n, m), hash)
			}
		}
		res := pool.SendBatch(ctx, batch)
		for n := start + 1; n <= end; n++ {
			var s seeded
			if err := res.QueryRow().Scan(&s.teamID); err != nil {
				res.Close()
				return fmt.Errorf("team %d: %w", n, err)
			}
			for m := 1; m <= *members; m++ {
				var userID string
				if err := res.QueryRow().Scan(&userID); err != nil {
					res.Close()
					return fmt.Errorf("team %d member %d: %w", n, m, err)
				}
				if m == 1 {
					s.firstUserID = userID
				}
				if _, err := pool.Exec(ctx, `insert into team_members (team_id, user_id) values ($1, $2) on conflict do nothing`, s.teamID, userID); err != nil {
					res.Close()
					return fmt.Errorf("membership: %w", err)
				}
			}
			out = append(out, s)
		}
		if err := res.Close(); err != nil {
			return err
		}
	}

	if *tokensOut != "" {
		if err := os.MkdirAll(filepath.Dir(*tokensOut), 0o755); err != nil {
			return err
		}
		f, err := os.Create(*tokensOut)
		if err != nil {
			return err
		}
		w := csv.NewWriter(f)
		_ = w.Write([]string{"team_id", "token"})
		for _, s := range out {
			token, err := auth.CreateSession(ctx, pool, s.firstUserID)
			if err != nil {
				f.Close()
				return err
			}
			_ = w.Write([]string{s.teamID, token})
		}
		w.Flush()
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Printf("wrote %d session tokens to %s (keep this file private)\n", len(out), *tokensOut)
	}

	fmt.Printf("hackathon %s: %d teams x %d members, deadline in %s, grace %s\n", hackathonID, *teams, *members, *deadlineIn, *grace)
	fmt.Printf("sign in as team1-member1@demo.test (or organizer@demo.test) with password %q\n", *password)
	return nil
}
