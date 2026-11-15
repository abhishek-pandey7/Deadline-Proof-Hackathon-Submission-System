// Deadline crush: 2,000 teams submit within 10 minutes, the rate climbing
// toward the end the way real deadlines do. Each team runs the real flow:
// create a version, sign its part, PUT the bytes straight to storage,
// complete. Only API calls count toward the p95 target; the storage PUT is
// measured separately because it is bandwidth, not our server.
//
// Run (PowerShell, from the repo root, Docker running):
//   docker run --rm -i -v "${PWD}/loadtest:/lt" -e BASE_URL=https://deadline-crush.onrender.com `
//     grafana/k6 run --summary-export /lt/results/summary.json /lt/submit.js
import http from "k6/http";
import crypto from "k6/crypto";
import exec from "k6/execution";
import { check, fail } from "k6";
import { SharedArray } from "k6/data";
import { Counter } from "k6/metrics";

const BASE = __ENV.BASE_URL || "http://127.0.0.1:8080";
const SKIP = Number(__ENV.SKIP_TEAMS || 3); // the first teams are the demo accounts

// team_id,token per line, written by: go run ./cmd/seed -teams 2003 -tokens-out loadtest/tokens.csv
const teams = new SharedArray("teams", () =>
  open("./tokens.csv").trim().split("\n").slice(1 + SKIP).map((l) => {
    const [team, token] = l.trim().split(",");
    return { team, token };
  }),
);

const TEAMS = Number(__ENV.TEAMS || 2000);

// File sizes: mostly small decks, some bigger. Kept modest because one home
// connection carries every team's upload; 100 MB uploads are tested on their own.
const SIZES = [256 * 1024, 256 * 1024, 256 * 1024, 512 * 1024, 1024 * 1024, 2 * 1024 * 1024];

const submitted = new Counter("submissions_completed");
const rejected = new Counter("submissions_rejected");

export const options = {
  scenarios: {
    deadline_crush: {
      executor: "ramping-arrival-rate",
      timeUnit: "1m",
      startRate: 60,
      preAllocatedVUs: 50,
      maxVUs: 400,
      // 525 + 675 + 800 = 2,000 submissions in 10 minutes.
      stages: [
        { target: 150, duration: "5m" },
        { target: 300, duration: "3m" },
        { target: 500, duration: "2m" },
      ],
    },
  },
  thresholds: {
    "http_req_duration{kind:api}": ["p(95)<300"],
    "checks": ["rate==1"],
  },
  summaryTrendStats: ["avg", "med", "p(90)", "p(95)", "p(99)", "max"],
};

// A PDF-looking file: it starts with %PDF- so the type check accepts it.
function makeFile(size) {
  const bytes = new Uint8Array(size);
  const head = "%PDF-1.4\n% load test\n";
  for (let i = 0; i < head.length; i++) bytes[i] = head.charCodeAt(i);
  const tail = "\n%%EOF\n";
  for (let i = 0; i < tail.length; i++) bytes[size - tail.length + i] = tail.charCodeAt(i);
  return bytes.buffer;
}

export function setup() {
  const res = http.get(`${BASE}/healthz`, { timeout: "120s" }); // wakes a sleeping free instance
  if (res.status !== 200) fail(`server not healthy: ${res.status}`);
  if (teams.length < TEAMS) fail(`tokens.csv has ${teams.length} load-test teams, need ${TEAMS}`);
  const hashes = {};
  for (const size of new Set(SIZES)) hashes[size] = crypto.sha256(makeFile(size), "hex");
  return { hashes };
}

export default function (data) {
  const i = exec.scenario.iterationInTest;
  if (i >= TEAMS) return;
  const { team, token } = teams[i];
  const size = SIZES[i % SIZES.length];
  const body = makeFile(size);
  const api = (name) => ({
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    tags: { kind: "api", name },
  });

  const created = http.post(
    `${BASE}/api/teams/${team}/versions`,
    JSON.stringify({ title: `Load test team ${i}`, description: "", links: [], file: { name: "deck.pdf", size, sha256: data.hashes[size] } }),
    Object.assign(api("create"), { headers: Object.assign(api("create").headers, { "Idempotency-Key": `loadtest-${team}` }) }),
  );
  if (!check(created, { "create 201": (r) => r.status === 201 || r.status === 200 })) {
    rejected.add(1, { step: "create", status: String(created.status) });
    return;
  }
  const version = created.json("version");

  const signed = http.post(`${BASE}/api/versions/${version.id}/parts/sign`, JSON.stringify({ parts: [1] }), api("sign"));
  if (!check(signed, { "sign 200": (r) => r.status === 200 })) {
    rejected.add(1, { step: "sign", status: String(signed.status) });
    return;
  }
  const part = signed.json("parts.0");
  const put = http.request(part.method, part.url, body, { headers: part.headers, tags: { kind: "storage", name: "put_part" }, timeout: "120s" });
  if (!check(put, { "storage put 200": (r) => r.status === 200 })) {
    rejected.add(1, { step: "put", status: String(put.status) });
    return;
  }

  const done = http.post(`${BASE}/api/versions/${version.id}/complete`, "{}", api("complete"));
  if (!check(done, { "complete 200": (r) => r.status === 200 && r.json("version.state") !== "uploading" })) {
    rejected.add(1, { step: "complete", status: String(done.status) });
    return;
  }
  submitted.add(1);
}
