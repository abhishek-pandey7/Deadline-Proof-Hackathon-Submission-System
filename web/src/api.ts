// Typed client for the Go API. Every response is JSON; errors carry a
// stable code the UI switches on.

export interface Window {
  deadline_at: string;
  grace_ends_at: string;
  server_time: string;
}

export interface User {
  id: string;
  email: string;
  display_name: string;
  is_organizer: boolean;
}

export interface TeamRef {
  id: string;
  name: string;
  hackathon_id: string;
  hackathon_name: string;
}

export interface Check {
  name: string;
  passed: boolean;
  detail: Record<string, unknown>;
}

export type VersionState = "uploading" | "checking" | "accepted" | "needs_fix" | "expired";

export interface Version {
  id: string;
  team_id: string;
  number: number;
  title: string;
  description: string;
  links: string[];
  file: { name: string; size: number; sha256: string };
  state: VersionState;
  state_reason: string | null;
  created_at: string;
  completed_at: string | null;
  checks_finished_at: string | null;
  part_size: number;
  part_count: number;
  checks: Check[];
  counts: boolean;
}

export interface TeamSubmission {
  team: { id: string; name: string };
  window: Window;
  versions: Version[];
  counted_version_id: string | null;
}

export interface Hackathon extends Window {
  id: string;
  name: string;
}

export interface SignedPart {
  number: number;
  size: number;
  method: string;
  url: string;
  headers: Record<string, string>;
}

export interface UploadStatus {
  state: VersionState;
  part_count: number;
  part_size: number;
  uploaded: { number: number; size: number }[];
  window: Window;
}

export interface OrganizerStats {
  hackathon_id: string;
  hackathon_name: string;
  window: Window;
  teams: number;
  teams_submitted: number;
  versions: Record<VersionState, number>;
  late_attempts: Record<string, number>;
  failed_checks: Record<string, number>;
  storage: { reserved_bytes: number; budget_bytes: number };
  ledger: {
    cap_bytes: number;
    reserved_bytes: number;
    uploaded_bytes_total: number;
    measured_bytes: number | null;
    measured_objects: number | null;
    measured_at: string | null;
    used_bytes: number;
  };
}

export interface SimulationStatus {
  running: boolean;
  started_at: string | null;
  finished_at: string | null;
  total: number;
  completed: number;
  failed: number;
  can_start_at: string | null;
}

export interface Preview {
  kind: "image" | "text" | "none";
  url?: string;
  path?: string;
  text?: string;
}

/** An error the API answered with. Network failures are plain TypeErrors. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly details: Record<string, unknown> = {},
  ) {
    super(message);
  }

  /** Worth retrying with the same request: overload or a server hiccup. */
  get retryable(): boolean {
    return this.status === 429 || this.status === 408 || this.status >= 500;
  }
}

async function request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const init: RequestInit = { method, headers: { ...headers }, credentials: "same-origin" };
  if (body !== undefined || method !== "GET") {
    (init.headers as Record<string, string>)["Content-Type"] = "application/json";
    init.body = body === undefined ? "{}" : JSON.stringify(body);
  }
  const res = await fetch(path, init);
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    const err = (data as { error?: { code?: string; message?: string; details?: Record<string, unknown> } })?.error;
    throw new ApiError(res.status, err?.code ?? "http_" + res.status, err?.message ?? res.statusText, err?.details ?? {});
  }
  return data as T;
}

export const api = {
  hackathon: () => request<Hackathon>("GET", "/api/hackathon"),
  time: () => request<{ now: string }>("GET", "/api/time"),
  me: () => request<{ user: User; teams: TeamRef[] }>("GET", "/api/me"),
  login: (email: string, password: string) => request<{ user: User }>("POST", "/api/auth/login", { email, password }),
  logout: () => request<void>("POST", "/api/auth/logout"),

  submission: (teamId: string) => request<TeamSubmission>("GET", `/api/teams/${teamId}/submission`),
  createVersion: (
    teamId: string,
    idempotencyKey: string,
    body: { title: string; description: string; links: string[]; file: { name: string; size: number; sha256: string } },
  ) => request<{ version: Version; window: Window }>("POST", `/api/teams/${teamId}/versions`, body, { "Idempotency-Key": idempotencyKey }),
  uploaded: (versionId: string) => request<UploadStatus>("GET", `/api/versions/${versionId}/parts`),
  signParts: (versionId: string, parts: number[]) =>
    request<{ parts: SignedPart[]; expires_at: string; window: Window }>("POST", `/api/versions/${versionId}/parts/sign`, { parts }),
  complete: (versionId: string) => request<{ version: Version }>("POST", `/api/versions/${versionId}/complete`),
  download: (versionId: string) => request<{ url: string; expires_at: string }>("GET", `/api/versions/${versionId}/download`),
  preview: (versionId: string) => request<Preview>("GET", `/api/versions/${versionId}/preview`),

  organizerStats: () => request<OrganizerStats>("GET", "/api/organizer/stats"),
  simulationStatus: () => request<SimulationStatus>("GET", "/api/organizer/simulate"),
  startSimulation: () => request<SimulationStatus>("POST", "/api/organizer/simulate"),
};
