# Deadline-Proof Hackathon Submission System

A project-submission system for a hackathon where 2,000 teams submit in the last 10 minutes before the deadline. It's built so that no submission is lost and the deadline is enforced fairly.

Entry for the Hack4Hack Crew Hiring Challenge 2026 (PS2 · Deadline Crush). The full brief is in [docs/brief.md](docs/brief.md), the design in [docs/design-doc.docx](docs/design-doc.docx) and the load test in [docs/load-test-report.docx](docs/load-test-report.docx).

**Live:** https://deadline-crush-q60y.onrender.com (Render free plan, Singapore; it may take up to a minute to wake). The sign-in page lists the demo accounts: a team member and an organizer, whose dashboard has a **Run simulation** button that makes 100 synthetic teams submit over a minute.

## What it guarantees

- **Nothing lost:** files go straight to storage in signed parts that only accept their exact size; a dropped connection resumes from the parts storage already has; "complete" moves the version to checking and queues exactly one check job in the same transaction.
- **Fair deadline:** every decision uses Postgres's clock. After the deadline nothing new can start; an upload already started has 15 minutes to finish, bytes only (its SHA-256 was fixed before the deadline). The latest version created before the deadline that finished in grace counts; a broken newer one never replaces a good older one.
- **Exactly once:** check results, the version's final state and the job's completion commit in one transaction. `TestKilledWorkerIsRetriedWithoutDuplicateResults` kills a worker process mid-job and proves it.
- **Bounded cost:** an account-wide storage ledger caps the bucket at 9 GB, under R2's free tier, and is checked against the real bucket every 10 minutes.

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

For the UI, run the Vite dev server next to it (it proxies `/api` to :8080, so cookies stay same-origin):

```sh
cd web && npm install && npm run dev    # http://127.0.0.1:5173
```

To build the single production binary with the UI inside:

```sh
cd web && npm run build && cd .. && go build -tags embedui -o bin/server ./cmd/server
```

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
| `GET /api/versions/{id}/download`, `GET /api/versions/{id}/preview` | 60-second signed links to the file and its preview |
| `GET /api/teams/{id}/events` | Server-sent events: each committed state change, `resync` after a reconnect |
| `GET /api/organizer/stats`, `GET /api/organizer/events`, `GET /api/organizer/export.csv` | Organizers only: live counts, their stream, and one CSV row per team |

Live updates come from Postgres `LISTEN/NOTIFY`: every state change notifies inside its own transaction, so a browser never hears about a change that did not commit. `DATABASE_URL` must be Neon's direct host, since the pooler drops `LISTEN`.

## Test

Tests create and drop their own databases on the Docker Postgres:

```sh
TEST_DATABASE_URL='postgres://app:app@127.0.0.1:55433/app?sslmode=disable' go test ./...
```

CI runs the same suite with the race detector on every push (`.github/workflows/ci.yml`).

## Load test

The deadline crush: 2,000 teams submit within 10 minutes, the rate climbing toward the end (60 to 500 per minute), each running create, sign, PUT to storage, complete, and retrying like the browser does.

```sh
DB_DNS_SERVER=1.1.1.1:53 go run ./cmd/seed -env-file .env -teams 2003 -bcrypt-cost 4 -tokens-out loadtest/tokens.csv
go run ./cmd/loadtest -base https://deadline-crush-q60y.onrender.com -tokens loadtest/tokens.csv
```

`loadtest/submit.js` is the same scenario for k6. Results against the live free-tier deployment: **2,000 of 2,000 submissions completed**, 6 requests retried, **API p95 269 ms** measured from Mumbai (about 110 ms of it network) and **200 ms inside the server**, under the 300 ms target in every minute including the 22.5 requests-per-second peak. Full numbers in [docs/load-test-report.docx](docs/load-test-report.docx); raw files in `docs/load-test/`.

Other tools: `go run ./cmd/r2check -env-file .env` proves against the real bucket that signed URLs refuse any other size; `go run ./cmd/cleanup -env-file .env -dry-run` removes test teams and their files once testing is over.

## Configuration

| Variable | Purpose |
| --- | --- |
| `APP_ENV` | `development`, `production` (default) or `test` |
| `DATABASE_URL` | Postgres connection string; use Neon's direct (non-pooler) host |
| `HTTP_ADDR` / `PORT` | Listen address; `PORT` is what Render sets |
| `R2_ENDPOINT`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET` | Object storage |
| `STORAGE_CAP_BYTES` | Account-wide cap on everything in the bucket (default 9 GB, under R2's 10 GB free tier) |

### Storage cap

Postgres keeps one account-wide count of what the bucket holds (`storage_ledger`): every version reserves its declared size before any upload link exists, expiry gives it back, and preview images add their size. Every 10 minutes a job also lists the real bucket, and the limiter uses whichever number is larger, so a count that drifted can only make it stricter. Each environment keeps its own count: locally it counts MinIO, deployed it counts R2. The organizer dashboard shows the count, the cap, the bytes uploaded since launch and the last measurement.

`dev.env` holds local values. Production values go in `.env` (gitignored); see `.env.example`.
