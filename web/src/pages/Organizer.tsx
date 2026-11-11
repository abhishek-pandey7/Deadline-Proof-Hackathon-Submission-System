import { DownloadSimple } from "@phosphor-icons/react";
import { useCallback, useEffect, useState } from "react";
import { api, type OrganizerStats, type User } from "../api";
import { checkLabel, formatBytes, stateLabel } from "../format";
import { useLive, useServerNow } from "../hooks";
import { Countdown, Panel, Skeleton } from "../ui";
import { Shell } from "./Shell";

const lateLabel: Record<string, string> = {
  create_after_deadline: "New version after the deadline",
  sign_after_grace: "Upload after grace ended",
  complete_after_grace: "Finish after grace ended",
};

function Num({ value, className = "" }: { value: number | string; className?: string }) {
  return <span className={`tabular font-mono font-semibold tracking-tight text-ink ${className}`}>{value}</span>;
}

function Breakdown({ rows }: { rows: [string, number][] }) {
  return (
    <dl className="flex flex-col gap-2">
      {rows.map(([label, n]) => (
        <div key={label} className="flex items-baseline justify-between gap-4 text-sm">
          <dt className="text-ink-2">{label}</dt>
          <dd><Num value={n} /></dd>
        </div>
      ))}
    </dl>
  );
}

export function OrganizerPage({ user, onSignOut }: { user: User; onSignOut: () => void }) {
  const [stats, setStats] = useState<OrganizerStats | null>(null);
  const refetch = useCallback(() => {
    api.organizerStats().then(setStats).catch(() => undefined);
  }, []);
  useEffect(refetch, [refetch]);
  const live = useLive("/api/organizer/events", refetch);
  const now = useServerNow(stats?.window ?? null);

  const lateTotal = stats ? Object.values(stats.late_attempts).reduce((a, b) => a + b, 0) : 0;
  const failedTotal = stats ? Object.values(stats.failed_checks).reduce((a, b) => a + b, 0) : 0;
  const storagePct = stats ? Math.round((stats.storage.reserved_bytes / stats.storage.budget_bytes) * 100) : 0;

  return (
    <Shell user={user} context={stats ? `${stats.hackathon_name}, organizer view` : "Organizer view"} live={live} onSignOut={onSignOut}>
      <div className="flex flex-col justify-between gap-6 md:flex-row md:items-end">
        <div className="flex flex-col gap-2">
          <h1 className="text-3xl font-semibold tracking-tight">Submissions</h1>
          <p className="text-ink-2">Counts update live as teams upload and checks finish.</p>
        </div>
        <a href="/api/organizer/export.csv" download className="inline-flex h-10 items-center gap-2 self-start rounded-lg bg-accent px-4 text-sm font-medium text-accent-ink hover:bg-accent-hover md:self-auto">
          <DownloadSimple weight="bold" aria-hidden />
          Export CSV
        </a>
      </div>

      {!stats ? (
        <div className="mt-8 grid grid-cols-1 gap-6 md:grid-cols-3"><Skeleton className="h-40" /><Skeleton className="h-40" /><Skeleton className="h-40" /></div>
      ) : (
        <div className="mt-8 grid grid-cols-1 gap-6 lg:grid-cols-[1.3fr_1fr]">
          <Panel>
            <p className="text-sm text-ink-2">Teams with a submission that counts</p>
            <p className="mt-2 flex items-baseline gap-3">
              <Num value={stats.teams_submitted} className="text-5xl" />
              <span className="text-ink-3">of {stats.teams} teams</span>
            </p>
            <div className="mt-6 grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-5">
              {(["uploading", "checking", "accepted", "needs_fix", "expired"] as const).map((s) => (
                <div key={s} className="flex flex-col gap-1">
                  <span className="text-xs text-ink-3">{stateLabel[s]}</span>
                  <Num value={stats.versions[s] ?? 0} className="text-2xl" />
                </div>
              ))}
            </div>
          </Panel>
          <Panel><Countdown window={stats.window} now={now} size="md" /></Panel>
          <Panel title={`Late attempts: ${lateTotal}`}>
            <Breakdown rows={Object.entries(stats.late_attempts).map(([k, n]) => [lateLabel[k] ?? k, n])} />
          </Panel>
          <Panel title={`Failed checks: ${failedTotal}`}>
            <Breakdown rows={Object.entries(stats.failed_checks).map(([k, n]) => [checkLabel[k] ?? k, n])} />
          </Panel>
          <Panel title="Storage" className="lg:col-span-2">
            <p className="text-sm text-ink-2">
              <Num value={formatBytes(stats.storage.reserved_bytes)} /> reserved of {formatBytes(stats.storage.budget_bytes)} ({storagePct}%). New uploads are refused before the limit, so storage never passes it.
            </p>
          </Panel>
        </div>
      )}
    </Shell>
  );
}
