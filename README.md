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

The server applies its migrations on startup.

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
