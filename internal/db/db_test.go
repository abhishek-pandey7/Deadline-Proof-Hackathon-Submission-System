package db_test

import (
	"context"
	"io/fs"
	"testing"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/db"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/testdb"
)

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	pool := testdb.New(t) // already migrated once
	ctx := context.Background()

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate applied %v, want nothing", applied)
	}

	for _, table := range []string{"hackathons", "users", "teams", "team_members", "sessions", "versions", "check_results", "late_attempts", "river_job"} {
		var exists bool
		if err := pool.QueryRow(ctx, "select to_regclass($1) is not null", table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("table %s missing after migrate", table)
		}
	}
}

func TestMigrationsAreRecordedOncePerFile(t *testing.T) {
	pool := testdb.New(t)
	files, err := fs.Glob(db.MigrationFilesForTest(), "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var recorded int
	if err := pool.QueryRow(context.Background(), "select count(*) from schema_migrations").Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != len(files) {
		t.Fatalf("schema_migrations has %d rows, want one per file (%d)", recorded, len(files))
	}
}

func TestVersionsRejectInvalidRows(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	var hackathonID, userID, teamID string
	if err := pool.QueryRow(ctx, `insert into hackathons (slug, name, deadline_at) values ('h', 'H', now() + interval '1 hour') returning id`).Scan(&hackathonID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `insert into users (email, display_name, password_hash) values ('a@example.com', 'A', 'x') returning id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `insert into teams (hackathon_id, name) values ($1, 'T') returning id`, hackathonID).Scan(&teamID); err != nil {
		t.Fatal(err)
	}

	insert := `insert into versions (team_id, number, idempotency_key, created_by, title, description, file_name, file_size, file_sha256, object_key, part_size)
	           values ($1, $2, $3, $4, 'Title', '', 'deck.pdf', 10, $5, $6, 8388608)`
	good := make([]byte, 32)
	if _, err := pool.Exec(ctx, insert, teamID, 1, "key-00000001", userID, good, "obj-1"); err != nil {
		t.Fatalf("valid insert failed: %v", err)
	}

	cases := map[string][]any{
		"duplicate idempotency key": {teamID, 2, "key-00000001", userID, good, "obj-2"},
		"duplicate version number":  {teamID, 1, "key-00000002", userID, good, "obj-3"},
		"short sha256":              {teamID, 3, "key-00000003", userID, make([]byte, 16), "obj-4"},
	}
	for name, args := range cases {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s: insert succeeded, want a constraint error", name)
		}
	}
}
