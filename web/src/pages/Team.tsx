import { ArrowSquareOut, CheckCircle, DownloadSimple, Eye, FileArrowUp, WarningCircle, X } from "@phosphor-icons/react";
import { useCallback, useEffect, useRef, useState, type DragEvent, type FormEvent } from "react";
import { api, ApiError, type Preview, type TeamSubmission, type User, type Version } from "../api";
import { checkLabel, formatBytes, formatWhen } from "../format";
import { useLive, useServerNow } from "../hooks";
import { clearPending, loadPending, runUpload, sameFile, type Pending, type Progress } from "../upload";
import { Button, Countdown, cx, deadlinePhase, Field, Panel, ProgressBar, Skeleton, StatusBadge, TextArea, TextInput } from "../ui";
import { Shell } from "./Shell";

const MAX_BYTES = 100 * 1024 * 1024;
const ACCEPT = ".pdf,.zip,.pptx,.png,.jpg,.jpeg,.mp4";

const ruleMessages: Record<string, string> = {
  deadline_passed: "The deadline has passed, so new versions are closed. Your latest finished version still counts.",
  grace_over: "The grace window closed before this upload finished.",
  version_limit: "Your team has used all of its versions.",
  storage_full: "Storage for this hackathon is full. Contact the organizers.",
  uploads_disabled: "Uploads are paused by the organizers. Try again shortly.",
  file_too_large: "The file is larger than 100 MB.",
  idempotency_mismatch: "This upload was started with a different file. Choose the original file or start a new version.",
};

export function TeamPage({ user, teamId, onSignOut }: { user: User; teamId: string; onSignOut: () => void }) {
  const [data, setData] = useState<TeamSubmission | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  const refetch = useCallback(() => {
    api.submission(teamId).then((d) => {
      setData(d);
      setLoadError(null);
    }).catch((err) => setLoadError(err instanceof ApiError && err.status === 404 ? "This team does not exist or you are not a member." : "Could not load your team. Retrying when the connection returns."));
  }, [teamId]);

  useEffect(refetch, [refetch]);
  const live = useLive(`/api/teams/${teamId}/events`, refetch);
  const now = useServerNow(data?.window ?? null);

  const counted = data?.versions.find((v) => v.id === data.counted_version_id) ?? null;
  const phase = data ? deadlinePhase(data.window, now) : "open";

  return (
    <Shell user={user} context={data?.team.name ?? "Your team"} live={live} onSignOut={onSignOut}>
      {loadError && !data && <p className="mb-6 rounded-lg bg-bad-tint px-4 py-3 text-sm font-medium text-bad">{loadError}</p>}
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-[1.4fr_1fr]">
        <Panel title="What counts right now">
          {!data ? <Skeleton className="h-28" /> : counted ? <VersionSummary version={counted} /> : (
            <p className="text-[15px] leading-relaxed text-ink-2">
              Nothing counts yet. Submit a version below; it counts once its upload finishes.
            </p>
          )}
        </Panel>
        <Panel>{data ? <Countdown window={data.window} now={now} /> : <Skeleton className="h-28" />}</Panel>
      </div>

      <div className="mt-6 grid grid-cols-1 gap-6 lg:grid-cols-[1.4fr_1fr]">
        <Panel title="Submit a new version">
          <SubmitPanel teamId={teamId} closed={phase !== "open"} onUploaded={refetch} />
        </Panel>
        <Panel title="How this works">
          <ul className="flex flex-col gap-3 text-sm leading-relaxed text-ink-2">
            <li>Each submission is a new version. Earlier versions are kept, and the latest finished one before the deadline counts.</li>
            <li>The file goes straight to storage in 8 MB parts. If your connection drops, it carries on from the last part.</li>
            <li>After the deadline, an upload that already started has 15 minutes to finish. Nothing new can start.</li>
            <li>Checks run after upload: file type, a fingerprint match, archive safety and whether your links respond.</li>
          </ul>
        </Panel>
      </div>

      <section className="mt-10" aria-labelledby="history">
        <h2 id="history" className="mb-4 text-lg font-semibold tracking-tight">All versions</h2>
        {!data ? <Skeleton className="h-20" /> : data.versions.length === 0 ? (
          <p className="text-sm text-ink-3">No versions yet.</p>
        ) : (
          <ol className="flex flex-col gap-3">
            {data.versions.map((v) => <VersionRow key={v.id} version={v} />)}
          </ol>
        )}
      </section>
    </Shell>
  );
}

function VersionSummary({ version }: { version: Version }) {
  const failed = version.checks.filter((c) => !c.passed && c.name !== "preview");
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <StatusBadge state={version.state} />
        <span className="text-sm text-ink-3">Version {version.number}</span>
      </div>
      <p className="text-xl font-semibold tracking-tight text-ink">{version.title}</p>
      <p className="text-sm text-ink-2">
        {version.file.name}, {formatBytes(version.file.size)}, finished {version.completed_at ? formatWhen(version.completed_at) : ""}
      </p>
      {version.state === "checking" && <p className="text-sm text-ink-2">Checks are running. This page updates by itself.</p>}
      {failed.length > 0 && (
        <p className="text-sm text-warn">
          {failed.length === 1 ? "One check needs attention" : `${failed.length} checks need attention`}. See the version below for details.
        </p>
      )}
    </div>
  );
}

function SubmitPanel({ teamId, closed, onUploaded }: { teamId: string; closed: boolean; onUploaded: () => void }) {
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [links, setLinks] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [progress, setProgress] = useState<Progress | null>(null);
  const [pending, setPending] = useState<Pending | null>(() => loadPending(teamId));
  const [dragging, setDragging] = useState(false);
  const abort = useRef<AbortController | null>(null);

  useEffect(() => () => abort.current?.abort(), []);

  function pick(f: File | null | undefined) {
    if (!f) return;
    setFile(f);
    setErrors((e) => ({ ...e, file: f.size > MAX_BYTES ? "The file is larger than 100 MB." : "" }));
    if (pending && sameFile(pending, f)) {
      setTitle(pending.meta.title);
      setDescription(pending.meta.description);
      setLinks(pending.meta.links.join("\n"));
    }
  }

  function validate(): { ok: boolean; linkList: string[] } {
    const linkList = links.split(/\s+/).map((l) => l.trim()).filter(Boolean);
    const e: Record<string, string> = {};
    if (!title.trim()) e.title = "Give your project a title.";
    if (linkList.length > 5) e.links = "Up to 5 links.";
    else if (linkList.some((l) => !/^https:\/\/[^\s/$.?#].[^\s]*$/i.test(l))) e.links = "Each link must start with https://";
    if (!file) e.file = "Choose a file to upload.";
    else if (file.size > MAX_BYTES) e.file = "The file is larger than 100 MB.";
    setErrors(e);
    return { ok: Object.keys(e).length === 0, linkList };
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    const { ok, linkList } = validate();
    if (!ok || !file) {
      // Bring the first problem into view: on a long form the error may be
      // scrolled off screen, which makes the button look broken.
      setTimeout(() => {
        const bad = document.querySelector<HTMLElement>('form [aria-invalid="true"]');
        bad?.scrollIntoView({ behavior: "smooth", block: "center" });
        bad?.focus({ preventScroll: true });
      }, 0);
      return;
    }
    setFailure(null);
    const controller = new AbortController();
    abort.current = controller;
    try {
      await runUpload({ teamId, file, meta: { title: title.trim(), description: description.trim(), links: linkList }, onProgress: setProgress, signal: controller.signal });
      setPending(null);
      setTitle("");
      setDescription("");
      setLinks("");
      setFile(null);
      onUploaded();
    } catch (err) {
      if (controller.signal.aborted) {
        setFailure("Upload paused. Choose the same file and submit again to carry on where it stopped.");
        setPending(loadPending(teamId));
      } else if (err instanceof ApiError && err.code === "invalid_input") {
        setFailure("Some fields need fixing.");
        const fields = (err.details.fields ?? {}) as Record<string, string>;
        setErrors(Object.fromEntries(Object.entries(fields).map(([k, v]) => [k.startsWith("file") ? "file" : k, `This ${v}`])));
      } else if (err instanceof ApiError) {
        setFailure(ruleMessages[err.code] ?? err.message);
        if (err.code === "grace_over" || err.code === "deadline_passed") {
          clearPending(teamId);
          setPending(null);
        }
      } else {
        console.error("upload failed", err);
        setFailure("The upload stopped unexpectedly. Submit again with the same file to resume.");
      }
      onUploaded();
    } finally {
      abort.current = null;
      setProgress(null);
    }
  }

  if (progress) return <UploadProgress progress={progress} onCancel={() => abort.current?.abort()} />;

  if (closed && !pending) {
    return <p className="text-[15px] leading-relaxed text-ink-2">The deadline has passed, so new versions are closed. Your latest finished version is what counts.</p>;
  }

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    pick(e.dataTransfer.files[0]);
  };

  return (
    <form onSubmit={submit} className="flex flex-col gap-5" noValidate>
      {pending && (
        <div className="flex items-start justify-between gap-4 rounded-lg bg-accent-tint px-4 py-3 text-sm text-ink" role="status">
          <p>
            The upload of <span className="font-medium">{pending.name}</span> did not finish. Choose the same file and submit to resume it.
          </p>
          <button type="button" className="text-ink-2 hover:text-ink" aria-label="Forget the unfinished upload" onClick={() => { clearPending(teamId); setPending(null); }}>
            <X weight="bold" aria-hidden />
          </button>
        </div>
      )}
      <Field label="Project title" error={errors.title}>
        {({ id, describedBy, invalid }) => <TextInput id={id} value={title} maxLength={120} onChange={(e) => setTitle(e.target.value)} aria-describedby={describedBy} aria-invalid={invalid} />}
      </Field>
      <Field label="Description" help="Optional, up to 5,000 characters.">
        {({ id, describedBy }) => <TextArea id={id} value={description} maxLength={5000} onChange={(e) => setDescription(e.target.value)} aria-describedby={describedBy} />}
      </Field>
      <Field label="Links" help="Repository, demo or video. One per line, up to 5, each starting with https://" error={errors.links}>
        {({ id, describedBy, invalid }) => <TextArea id={id} value={links} rows={3} className="min-h-20 font-mono text-sm" onChange={(e) => setLinks(e.target.value)} aria-describedby={describedBy} aria-invalid={invalid} />}
      </Field>
      <Field label="Project file" help="PDF, ZIP, PPTX, PNG, JPEG or MP4, up to 100 MB." error={errors.file || undefined}>
        {({ id, describedBy, invalid }) => (
          <label
            htmlFor={id}
            onDragOver={(e) => { e.preventDefault(); setDragging(true); }}
            onDragLeave={() => setDragging(false)}
            onDrop={onDrop}
            className={cx(
              "flex cursor-pointer flex-col items-center justify-center gap-2 rounded-lg border border-dashed px-4 py-8 text-center transition-colors",
              dragging ? "border-accent bg-accent-tint" : "border-line bg-surface-2 hover:border-ink-3",
              invalid && "border-bad",
            )}
          >
            <FileArrowUp size={28} weight="regular" className="text-ink-2" aria-hidden />
            {file ? (
              <span className="text-sm text-ink"><span className="font-medium">{file.name}</span>, {formatBytes(file.size)}</span>
            ) : (
              <span className="text-sm text-ink-2">Drop a file here or <span className="font-medium text-accent">choose one</span></span>
            )}
            <input id={id} type="file" accept={ACCEPT} className="sr-only" aria-describedby={describedBy} aria-invalid={invalid} onChange={(e) => pick(e.target.files?.[0])} />
          </label>
        )}
      </Field>
      {failure && <p className="rounded-lg bg-bad-tint px-4 py-3 text-sm font-medium text-bad" role="alert">{failure}</p>}
      <div>
        <Button type="submit">{pending && file && sameFile(pending, file) ? "Resume upload" : "Submit version"}</Button>
      </div>
    </form>
  );
}

const phaseText: Record<Progress["phase"], string> = {
  hashing: "Fingerprinting your file",
  starting: "Starting the upload",
  uploading: "Uploading straight to storage",
  finishing: "Assembling the file",
  offline: "You are offline. The upload will carry on when the connection returns.",
  retrying: "Connection trouble. Retrying",
  done: "Uploaded",
};

function UploadProgress({ progress, onCancel }: { progress: Progress; onCancel: () => void }) {
  const hashing = progress.phase === "hashing";
  const value = hashing ? progress.hashedBytes / Math.max(1, progress.totalBytes) : progress.uploadedBytes / Math.max(1, progress.totalBytes);
  const bytes = hashing ? progress.hashedBytes : progress.uploadedBytes;
  return (
    <div className="flex flex-col gap-4" aria-live="polite">
      <p className={cx("text-[15px] font-medium", progress.phase === "offline" ? "text-warn" : "text-ink")}>
        {phaseText[progress.phase]}
        {progress.phase === "retrying" && progress.retryInSeconds ? ` in ${progress.retryInSeconds}s` : ""}
      </p>
      <ProgressBar value={value} label={hashing ? "Fingerprint progress" : "Upload progress"} />
      <p className="tabular font-mono text-sm text-ink-2">
        {formatBytes(bytes)} of {formatBytes(progress.totalBytes)}
      </p>
      <p className="text-sm text-ink-3">Keep this tab open. If you close it, choose the same file again later to resume.</p>
      <div>
        <Button variant="quiet" type="button" onClick={onCancel}>Pause</Button>
      </div>
    </div>
  );
}

function VersionRow({ version }: { version: Version }) {
  const [preview, setPreview] = useState<Preview | null>(null);
  const [busy, setBusy] = useState(false);
  const stored = version.state === "checking" || version.state === "accepted" || version.state === "needs_fix";

  async function download() {
    setBusy(true);
    try {
      const { url } = await api.download(version.id);
      window.location.assign(url);
    } finally {
      setBusy(false);
    }
  }

  async function showPreview() {
    setPreview(await api.preview(version.id));
  }

  return (
    <li className="rounded-xl border border-line bg-surface">
      <details className="group">
        <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-4 gap-y-2 px-5 py-4">
          <span className="tabular font-mono text-sm text-ink-3">v{version.number}</span>
          <span className="min-w-0 flex-1 truncate font-medium text-ink">{version.title}</span>
          {version.counts && <span className="rounded-full border border-accent px-2.5 py-0.5 text-xs font-semibold text-accent">Counts</span>}
          <StatusBadge state={version.state} />
          <span className="hidden text-sm text-ink-3 md:inline">{formatWhen(version.created_at)}</span>
        </summary>
        <div className="flex flex-col gap-4 border-t border-line px-5 py-4">
          <p className="text-sm text-ink-2">
            {version.file.name}, {formatBytes(version.file.size)}
            <br />
            <span className="break-all font-mono text-xs text-ink-3">SHA-256 {version.file.sha256}</span>
          </p>
          {version.state_reason && <p className="text-sm text-ink-2">{version.state_reason}</p>}
          {version.checks.length > 0 && (
            <ul className="grid grid-cols-1 gap-2 sm:grid-cols-2">
              {version.checks.map((c) => (
                <li key={c.name} className="flex items-start gap-2 text-sm">
                  {c.passed ? <CheckCircle weight="fill" className="mt-0.5 shrink-0 text-ok" aria-hidden /> : <WarningCircle weight="fill" className="mt-0.5 shrink-0 text-warn" aria-hidden />}
                  <span>
                    <span className="text-ink">{checkLabel[c.name] ?? c.name}</span>
                    {!c.passed && typeof c.detail.reason === "string" && <span className="block text-ink-2">{c.detail.reason}</span>}
                    <span className="sr-only">{c.passed ? "passed" : "failed"}</span>
                  </span>
                </li>
              ))}
            </ul>
          )}
          {version.links.length > 0 && (
            <ul className="flex flex-col gap-1">
              {version.links.map((l) => (
                <li key={l}>
                  <a href={l} target="_blank" rel="noreferrer noopener" className="inline-flex items-center gap-1 break-all text-sm text-accent hover:underline">
                    {l}
                    <ArrowSquareOut aria-hidden />
                  </a>
                </li>
              ))}
            </ul>
          )}
          {stored && (
            <div className="flex flex-wrap gap-3">
              <Button variant="quiet" onClick={download} disabled={busy}><DownloadSimple weight="bold" aria-hidden />Download</Button>
              <Button variant="quiet" onClick={showPreview}><Eye weight="bold" aria-hidden />Preview</Button>
            </div>
          )}
          {preview?.kind === "image" && <img src={preview.url} alt={`Preview of ${version.file.name}`} className="max-h-96 w-auto rounded-lg border border-line" />}
          {preview?.kind === "text" && (
            <pre className="max-h-80 overflow-auto whitespace-pre-wrap rounded-lg bg-surface-2 p-4 font-mono text-xs leading-relaxed text-ink">{preview.text}</pre>
          )}
          {preview?.kind === "none" && <p className="text-sm text-ink-3">No preview for this file.</p>}
        </div>
      </details>
    </li>
  );
}
