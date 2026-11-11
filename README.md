# Deadline-Proof Hackathon Submission System

A project-submission system for a hackathon where 2,000 teams submit in the last 10 minutes before the deadline. It's built so that no submission is lost and the deadline is enforced fairly.

Entry for the Hack4Hack Crew Hiring Challenge 2026 (PS2 · Deadline Crush). The full brief is in [docs/brief.md](docs/brief.md) and the design in [docs/design-doc.docx](docs/design-doc.docx).

## Stack

Go 1.27 (API and workers in one binary), Postgres 18 with the [River](https://riverqueue.com) job queue, S3-compatible object storage (Cloudflare R2 in production, MinIO locally) and a React + TypeScript UI.

## Run locally

Requires Go 1.27 and Docker.

```sh
docker compose up -d                      # Postgres on 127.0.0.1:55433, MinIO on 127.0.0.1:9000 (console :9001)
go run ./cmd/server -env-file dev.env     # API on http://127.0.0.1:8080
curl http://127.0.0.1:8080/healthz
```

The server applies its migrations on startup and, in development, creates the MinIO bucket.

Seed a hackathon with demo teams (safe to re-run):

```sh
go run ./cmd/seed -env-file dev.env      # sign in as team1-member1@demo.test / demo-pass-2026
```

## API

All writes need `Content-Type: application/json`. Signed-in routes take the `h4h_session` cookie (or `Authorization: Bearer <token>` for the load test).

| Method and path | What it does |
| --- | --- |
| `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/me` | Session cookie (HttpOnly, SameSite=Lax), rate-limited per IP and per email |
| `GET /api/hackathon`, `GET /api/time` | Deadline, grace end and Postgres's clock |
| `GET /api/teams/{id}/submission` | Every version, newest first, and which one counts |
| `POST /api/teams/{id}/versions` | Start a version; needs an `Idempotency-Key` header, a retry returns the same version |
| `POST /api/versions/{id}/parts/sign` | Signed PUT URLs for chunks, each locked to its exact size and never valid past the grace window |
| `GET /api/versions/{id}/parts` | Which chunks storage already has, for resuming |
| `POST /api/versions/{id}/complete` | Assemble the file and queue its checks (exactly one job per version) |

## Test

Tests create and drop their own databases on the Docker Postgres:

```sh
TEST_DATABASE_URL='postgres://app:app@127.0.0.1:55433/app?sslmode=disable' go test ./...
```

CI runs the same suite with the race detector on every push (`.github/workflows/ci.yml`).

## Configuration

| Variable | Purpose |
| --- | --- |
| `APP_ENV` | `development`, `production` (default) or `test` |
| `DATABASE_URL` | Postgres connection string; use Neon's direct (non-pooler) host |
| `HTTP_ADDR` / `PORT` | Listen address; `PORT` is what Render sets |
| `R2_ENDPOINT`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET` | Object storage |

`dev.env` holds local values. Production values go in `.env` (gitignored); see `.env.example`.
