# Hack4Hack Crew Hiring Challenge 2026 — brief

**Chosen problem: PS2 · Deadline Crush.**
Deadline: Sunday 15 Nov 2026, 11:59 PM IST. Solo entry.

Hack4Hack is hiring 2 Full-stack Engineers (paid internship or full-time, remote). The top 5 for each problem statement get an interview. Expect 30 to 50 hours of work. Doing the must-haves properly, with honest notes on what is missing, beats doing everything badly.

## PS2 · Deadline Crush: submissions that survive the last 10 minutes

Every hackathon gets most of its submissions in the final minutes. Nothing may be lost.

**Task:** Build the project-submission system for a hackathon where 2,000 teams submit in the last 10 minutes before the deadline: project details, links and a file of up to 100 MB each. Files go straight to object storage, every submission is checked in the background, and the server enforces the deadline fairly, not the browser.

### Must have

- **Uploads:** files of up to 100 MB go directly to S3-compatible object storage (MinIO, Cloudflare R2 or similar) through short-lived signed URLs, in resumable chunks. A dropped connection continues where it stopped, and the API server never streams the file.
- **Deadline:** enforced on the server clock. Write down a clear grace policy for uploads that started before the deadline, and enforce it. Teams can edit until the deadline; every version is kept and the last one before the deadline counts.
- **Background checks:** a job queue with workers that verify the file type by its content (not the extension), reject zip bombs and oversized archives, check that the submitted links respond, and make a preview (first page of a PDF or the README of a zip).
- **Live status:** each team sees its submission move from uploading to checking to accepted or needs a fix, without refreshing.
- **Exactly once:** a worker that crashes mid-job is retried without duplicate results. Prove it by killing a worker during a test run.
- **Secure:** only members of a team can submit or see its files, signed URLs expire, and every input is validated.
- **Organizer dashboard:** live counts of submissions, late attempts and failed checks, plus a CSV export.
- **Load test:** simulate 2,000 teams submitting in 10 minutes (k6 or similar). Report p95 API latency (target under 300 ms) and show that no submission is lost.
- **Tests:** at least 20 tests running in CI (GitHub Actions).
- **Deployed:** a live URL on any free tier.

### Bonus

An emailed receipt with a hash of the submitted file, uploads that resume after the laptop goes offline, virus scanning, a status page for organizers.

### Submit

- GitHub repo
- Live URL
- The load-test report
- A 1-page design doc: the upload flow, the deadline and grace policy, how jobs stay exactly-once, the trade-offs made and what you would do with one more week
- A walkthrough video of up to 5 minutes

### What they look at

Nothing lost and a fair deadline first, then design decisions and how well you defend them, then code quality, tests and UX.

## Timeline

- Round 1 build task: open now until **Sunday 15 Nov, 11:59 PM IST**. Submit from the Hack4Hack workspace.
- Top 5 per problem statement announced Saturday 21 Nov. The top 50 get written feedback.
- Interviews 23 to 26 Nov: a walkthrough of the work, a small live change and a conversation about the role.
- Offers by Monday 30 Nov.

## Rules

- Individuals only, one problem statement each.
- Free to enter, open worldwide.
- Every complete submission gets a certificate.

## For reference: PS1 (not chosen)

Race-proof team formation at scale. Team service for 10,000 participants with invite codes, join requests, a live "looking for teammates" board, a 50-way race test for the last seat, idempotency, audit log, teammate suggestions, k6 load test (p95 < 200 ms at 100 concurrent users), React UI and deployment.
