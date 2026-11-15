// Command cleanup removes test data once testing is over: every team except
// the ones to keep (the demo teams), with their versions, files, previews,
// members and sessions. Then it recounts the storage ledger and re-measures
// the bucket. Run it once, not after every test.
//
//	go run ./cmd/cleanup -env-file .env -keep "Team 1,Team 2,Team 3" -dry-run
//	go run ./cmd/cleanup -env-file .env -keep "Team 1,Team 2,Team 3"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/checks"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/config"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/db"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup:", err)
		os.Exit(1)
	}
}

func run() error {
	envFile := flag.String("env-file", "", "KEY=VALUE file with DATABASE_URL and the R2 settings")
	keepList := flag.String("keep", "Team 1,Team 2,Team 3", "comma-separated team names to keep")
	dryRun := flag.Bool("dry-run", false, "only report what would be removed")
	flag.Parse()
	if *envFile != "" {
		if err := config.LoadEnvFile(*envFile); err != nil {
			return err
		}
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	keep := strings.Split(*keepList, ",")
	for i := range keep {
		keep[i] = strings.TrimSpace(keep[i])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL, 4)
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := storage.New(cfg.Storage)
	if err != nil {
		return err
	}

	var teamIDs []string
	rows, err := pool.Query(ctx, `select id::text from teams where not (name = any($1))`, keep)
	if err != nil {
		return err
	}
	teamIDs, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	type ver struct {
		id, key  string
		uploadID *string
		preview  int64
	}
	rows, err = pool.Query(ctx, `select id::text, object_key, upload_id, preview_bytes from versions where team_id = any($1::uuid[])`, teamIDs)
	if err != nil {
		return err
	}
	var versions []ver
	for rows.Next() {
		var v ver
		if err := rows.Scan(&v.id, &v.key, &v.uploadID, &v.preview); err != nil {
			return err
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var keys []string
	for _, v := range versions {
		keys = append(keys, v.key)
		if v.preview > 0 {
			keys = append(keys, checks.PreviewKey(v.id))
		}
	}
	fmt.Printf("keeping %v; removing %d teams, %d versions, %d stored objects\n", keep, len(teamIDs), len(versions), len(keys))
	if *dryRun {
		fmt.Println("dry run: nothing changed")
		return nil
	}

	for _, v := range versions {
		if v.uploadID != nil {
			_ = store.AbortMultipart(ctx, v.key, *v.uploadID)
		}
	}
	deleted, err := store.DeleteMany(ctx, keys)
	if err != nil {
		return err
	}
	fmt.Printf("deleted %d objects from the bucket\n", deleted)

	versionIDs := make([]string, len(versions))
	for i, v := range versions {
		versionIDs[i] = v.id
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Users who belong only to removed teams (never organizers).
		if _, err := tx.Exec(ctx, `
			create temp table doomed_users on commit drop as
			select distinct m.user_id from team_members m
			where m.team_id = any($1::uuid[])
			  and not exists (select 1 from team_members k where k.user_id = m.user_id and k.team_id <> all($1::uuid[]))
			  and not exists (select 1 from users u where u.id = m.user_id and u.is_organizer)`, teamIDs); err != nil {
			return err
		}
		for _, q := range []string{
			`delete from river_job where kind = 'check_version' and args->>'version_id' = any($1::text[])`,
		} {
			if _, err := tx.Exec(ctx, q, versionIDs); err != nil {
				return err
			}
		}
		for _, q := range []string{
			`delete from late_attempts where team_id = any($1::uuid[])`,
			`delete from versions where team_id = any($1::uuid[])`, // check_results cascade
			`delete from teams where id = any($1::uuid[])`,         // team_members cascade
		} {
			if _, err := tx.Exec(ctx, q, teamIDs); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `delete from users where id in (select user_id from doomed_users)`); err != nil { // sessions cascade
			return err
		}
		// Recount what remains: live versions and their previews.
		if _, err := tx.Exec(ctx, `
			update hackathons h set reserved_bytes = coalesce((
				select sum(v.file_size) from versions v join teams t on t.id = v.team_id
				where t.hackathon_id = h.id and v.state <> 'expired'), 0)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			update storage_ledger set reserved_bytes = (
				select coalesce(sum(file_size) filter (where state <> 'expired'), 0) + coalesce(sum(preview_bytes), 0) from versions),
				updated_at = now()`)
		return err
	})
	if err != nil {
		return fmt.Errorf("database cleanup: %w", err)
	}

	bytes, objects, err := store.Measure(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `update storage_ledger set measured_bytes = $1, measured_objects = $2, measured_at = now()`, bytes, objects); err != nil {
		return err
	}
	fmt.Printf("bucket now holds %d bytes in %d objects; ledger recounted\n", bytes, objects)
	return nil
}
