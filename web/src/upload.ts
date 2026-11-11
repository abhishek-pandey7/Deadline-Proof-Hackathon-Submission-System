// The browser half of a submission. The file goes straight to storage in
// fixed-size parts through short-lived signed URLs; the API never sees bytes.
// Storage is the ledger: before every round the client asks which parts
// arrived and sends only the missing ones, so a dropped connection, an
// expired URL or a closed laptop just means another round.

import { createSHA256 } from "hash-wasm";
import { api, ApiError, type SignedPart, type Version } from "./api";

export type Phase = "hashing" | "starting" | "uploading" | "finishing" | "offline" | "retrying" | "done";

export interface Progress {
  phase: Phase;
  hashedBytes: number;
  uploadedBytes: number;
  totalBytes: number;
  versionId?: string;
  retryInSeconds?: number;
}

export interface Meta {
  title: string;
  description: string;
  links: string[];
}

/** Parts still to send, given the plan and what storage already has. */
export function missingParts(partCount: number, uploaded: { number: number }[]): number[] {
  const have = new Set(uploaded.map((p) => p.number));
  const out: number[] = [];
  for (let n = 1; n <= partCount; n++) if (!have.has(n)) out.push(n);
  return out;
}

/** Byte range [start, end) of part n (1-based). */
export function partRange(n: number, partSize: number, fileSize: number): [number, number] {
  const start = (n - 1) * partSize;
  return [start, Math.min(start + partSize, fileSize)];
}

/** Split part numbers into sign requests of at most size items. */
export function batches<T>(items: T[], size: number): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < items.length; i += size) out.push(items.slice(i, i + size));
  return out;
}

/** Exponential backoff with jitter: about 1, 2, 4, 8, then 15 s. */
export function backoffMs(attempt: number): number {
  const base = Math.min(15_000, 1000 * 2 ** attempt);
  return Math.round(base * (0.75 + Math.random() * 0.5));
}

// API answers that will not get better by retrying the same request.
const FATAL = new Set(["deadline_passed", "grace_over", "wrong_state", "not_found", "invalid_input", "file_too_large", "version_limit", "storage_full", "uploads_disabled", "idempotency_mismatch", "unauthorized"]);

export function isFatal(err: unknown): boolean {
  return err instanceof ApiError && (FATAL.has(err.code) || (!err.retryable && err.code !== "parts_missing" && err.code !== "upload_mismatch"));
}

/** Stream the file through SHA-256 in 8 MiB slices. */
export async function sha256File(file: Blob, onBytes: (n: number) => void, signal?: AbortSignal): Promise<string> {
  const hasher = await createSHA256();
  hasher.init();
  const slice = 8 << 20;
  for (let off = 0; off < file.size; off += slice) {
    signal?.throwIfAborted();
    const chunk = new Uint8Array(await file.slice(off, off + slice).arrayBuffer());
    hasher.update(chunk);
    onBytes(Math.min(off + slice, file.size));
  }
  return hasher.digest("hex");
}

// An unfinished upload is remembered so a reload can resume it.
export interface Pending {
  teamId: string;
  key: string;
  name: string;
  size: number;
  lastModified: number;
  sha256: string;
  meta: Meta;
}

const pendingKey = (teamId: string) => `h4h.pending.${teamId}`;

export function loadPending(teamId: string): Pending | null {
  try {
    const raw = localStorage.getItem(pendingKey(teamId));
    return raw ? (JSON.parse(raw) as Pending) : null;
  } catch {
    return null;
  }
}

function savePending(p: Pending) {
  try {
    localStorage.setItem(pendingKey(p.teamId), JSON.stringify(p));
  } catch {
    /* storage full or blocked: resume after reload just won't be offered */
  }
}

export function clearPending(teamId: string) {
  try {
    localStorage.removeItem(pendingKey(teamId));
  } catch {
    /* ignore */
  }
}

export function sameFile(p: Pending, f: File): boolean {
  return p.name === f.name && p.size === f.size && p.lastModified === f.lastModified;
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const t = setTimeout(resolve, ms);
    signal?.addEventListener("abort", () => {
      clearTimeout(t);
      reject(signal.reason);
    }, { once: true });
  });
}

function waitOnline(signal?: AbortSignal): Promise<void> {
  if (navigator.onLine) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const done = () => {
      window.removeEventListener("online", done);
      resolve();
    };
    window.addEventListener("online", done);
    signal?.addEventListener("abort", () => {
      window.removeEventListener("online", done);
      reject(signal.reason);
    }, { once: true });
  });
}

/** PUT one part with byte-level progress (fetch cannot report upload progress). */
function putPart(part: SignedPart, body: Blob, onBytes: (sent: number) => void, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open(part.method, part.url);
    for (const [k, v] of Object.entries(part.headers)) xhr.setRequestHeader(k, v);
    xhr.upload.onprogress = (e) => onBytes(e.loaded);
    xhr.onload = () => (xhr.status >= 200 && xhr.status < 300 ? resolve() : reject(new Error(`part ${part.number}: storage answered ${xhr.status}`)));
    xhr.onerror = () => reject(new TypeError(`part ${part.number}: network error`));
    xhr.ontimeout = () => reject(new TypeError(`part ${part.number}: timed out`));
    xhr.timeout = 10 * 60_000;
    signal?.addEventListener("abort", () => xhr.abort(), { once: true });
    xhr.send(body);
  });
}

interface RunOptions {
  teamId: string;
  file: File;
  meta: Meta;
  onProgress: (p: Progress) => void;
  signal?: AbortSignal;
}

/**
 * Submit a new version and upload its file, resuming as needed. Resolves
 * with the completed version (now "checking"); rejects on a rule the server
 * enforces (deadline, grace, limits) or when cancelled.
 */
export async function runUpload({ teamId, file, meta, onProgress, signal }: RunOptions): Promise<Version> {
  const total = file.size;
  let progress: Progress = { phase: "hashing", hashedBytes: 0, uploadedBytes: 0, totalBytes: total };
  const emit = (p: Partial<Progress>) => {
    progress = { ...progress, ...p };
    onProgress(progress);
  };
  emit({});

  const sha256 = await sha256File(file, (n) => emit({ hashedBytes: n }), signal);

  // Reuse the key of an unfinished upload of this same file: the server then
  // returns that version instead of starting a second one.
  const prev = loadPending(teamId);
  const key = prev && sameFile(prev, file) && prev.sha256 === sha256 ? prev.key : crypto.randomUUID();
  savePending({ teamId, key, name: file.name, size: file.size, lastModified: file.lastModified, sha256, meta });

  emit({ phase: "starting" });
  const created = await retrying(
    () => api.createVersion(teamId, key, { ...meta, file: { name: file.name, size: file.size, sha256 } }),
    emit,
    signal,
  );
  const version = created.version;
  emit({ versionId: version.id });

  for (let round = 0; ; round++) {
    signal?.throwIfAborted();
    const status = await retrying(() => api.uploaded(version.id), emit, signal);
    if (status.state !== "uploading") break; // finished by an earlier attempt
    const todo = missingParts(status.part_count, status.uploaded);
    const doneBytes = status.uploaded.reduce((s, p) => s + p.size, 0);
    emit({ phase: "uploading", uploadedBytes: doneBytes });

    if (todo.length === 0) {
      emit({ phase: "finishing" });
      try {
        const done = await retrying(() => api.complete(version.id), emit, signal);
        clearPending(teamId);
        emit({ phase: "done", uploadedBytes: total });
        return done.version;
      } catch (err) {
        if (err instanceof ApiError && (err.code === "parts_missing" || err.code === "upload_mismatch")) continue;
        throw err;
      }
    }

    let failed = false;
    const inFlight = new Map<number, number>();
    const report = () => emit({ uploadedBytes: doneBytes + [...inFlight.values()].reduce((s, n) => s + n, 0) });
    for (const group of batches(todo, 20)) {
      const signed = await retrying(() => api.signParts(version.id, group), emit, signal);
      const queue = [...signed.parts];
      const worker = async () => {
        for (let part = queue.shift(); part; part = queue.shift()) {
          const [start, end] = partRange(part.number, status.part_size, total);
          inFlight.set(part.number, 0);
          try {
            await putPart(part, file.slice(start, end), (sent) => {
              inFlight.set(part!.number, sent);
              report();
            }, signal);
            inFlight.set(part.number, end - start);
          } catch (err) {
            signal?.throwIfAborted();
            inFlight.delete(part.number);
            failed = true;
            void err;
          }
          report();
        }
      };
      await Promise.all([worker(), worker(), worker()]);
    }
    if (failed) {
      // Something did not arrive. Wait for the network, back off, then ask
      // storage again what it has; the next round sends only the gaps.
      if (!navigator.onLine) {
        emit({ phase: "offline" });
        await waitOnline(signal);
      } else {
        const wait = backoffMs(Math.min(round, 4));
        emit({ phase: "retrying", retryInSeconds: Math.ceil(wait / 1000) });
        await sleep(wait, signal);
      }
    }
  }

  const final = await api.submission(teamId);
  clearPending(teamId);
  emit({ phase: "done", uploadedBytes: total });
  return final.versions.find((v) => v.id === version.id) ?? version;
}

/** Run an API call, retrying network failures and overload with backoff. */
async function retrying<T>(fn: () => Promise<T>, emit: (p: Partial<Progress>) => void, signal?: AbortSignal): Promise<T> {
  for (let attempt = 0; ; attempt++) {
    signal?.throwIfAborted();
    try {
      return await fn();
    } catch (err) {
      if (isFatal(err) || (err instanceof ApiError && !err.retryable)) throw err;
      if (signal?.aborted) throw signal.reason;
      if (!navigator.onLine) {
        emit({ phase: "offline" });
        await waitOnline(signal);
        continue;
      }
      const wait = backoffMs(attempt);
      emit({ phase: "retrying", retryInSeconds: Math.ceil(wait / 1000) });
      await sleep(wait, signal);
    }
  }
}
