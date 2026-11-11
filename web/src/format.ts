import type { VersionState } from "./api";

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 10 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

const dateTime = new Intl.DateTimeFormat(undefined, {
  weekday: "short",
  day: "numeric",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  timeZoneName: "short",
});

export function formatWhen(iso: string): string {
  return dateTime.format(new Date(iso));
}

/** "2d 04:12:09" style countdown from milliseconds. */
export function formatCountdown(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  const d = Math.floor(s / 86400);
  const hh = String(Math.floor((s % 86400) / 3600)).padStart(2, "0");
  const mm = String(Math.floor((s % 3600) / 60)).padStart(2, "0");
  const ss = String(s % 60).padStart(2, "0");
  return d > 0 ? `${d}d ${hh}:${mm}:${ss}` : `${hh}:${mm}:${ss}`;
}

export const stateLabel: Record<VersionState, string> = {
  uploading: "Uploading",
  checking: "Checking",
  accepted: "Accepted",
  needs_fix: "Needs a fix",
  expired: "Expired",
};

export const checkLabel: Record<string, string> = {
  integrity: "File matches its fingerprint",
  file_type: "File type",
  archive: "Archive is safe to open",
  links: "Links respond",
  preview: "Preview",
};
