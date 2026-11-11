import { CheckCircle, Clock, HourglassMedium, Prohibit, UploadSimple, WarningCircle } from "@phosphor-icons/react";
import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode, TextareaHTMLAttributes } from "react";
import { useId } from "react";
import type { VersionState, Window } from "./api";
import { formatCountdown, formatWhen, stateLabel } from "./format";

export function cx(...parts: (string | false | null | undefined)[]) {
  return parts.filter(Boolean).join(" ");
}

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "primary" | "quiet" | "danger" };

export function Button({ variant = "primary", className, ...rest }: ButtonProps) {
  return (
    <button
      {...rest}
      className={cx(
        "inline-flex h-10 items-center justify-center gap-2 whitespace-nowrap rounded-lg px-4 text-sm font-medium",
        "transition-[background-color,transform] duration-150 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-55",
        variant === "primary" && "bg-accent text-accent-ink hover:bg-accent-hover",
        variant === "quiet" && "border border-line bg-surface text-ink hover:bg-surface-2",
        variant === "danger" && "border border-line bg-surface text-bad hover:bg-bad-tint",
        className,
      )}
    />
  );
}

interface FieldProps {
  label: string;
  help?: ReactNode;
  error?: string;
  children: (ids: { id: string; describedBy?: string; invalid: boolean }) => ReactNode;
}

/** Label above, help and error below, all wired for screen readers. */
export function Field({ label, help, error, children }: FieldProps) {
  const id = useId();
  const helpId = help ? `${id}-help` : undefined;
  const errId = error ? `${id}-err` : undefined;
  const describedBy = [helpId, errId].filter(Boolean).join(" ") || undefined;
  return (
    <div className="flex flex-col gap-2">
      <label htmlFor={id} className="text-sm font-medium text-ink">
        {label}
      </label>
      {children({ id, describedBy, invalid: Boolean(error) })}
      {help && (
        <p id={helpId} className="text-sm text-ink-3">
          {help}
        </p>
      )}
      {error && (
        <p id={errId} className="text-sm font-medium text-bad">
          {error}
        </p>
      )}
    </div>
  );
}

const inputClass =
  "w-full rounded-lg border border-line bg-surface px-3 text-[15px] text-ink placeholder:text-ink-3 aria-[invalid=true]:border-bad";

export function TextInput(props: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={cx(inputClass, "h-10", props.className)} />;
}

export function TextArea(props: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...props} className={cx(inputClass, "min-h-28 py-2 leading-relaxed", props.className)} />;
}

const badgeStyle: Record<VersionState, string> = {
  uploading: "bg-accent-tint text-accent",
  checking: "bg-accent-tint text-accent",
  accepted: "bg-ok-tint text-ok",
  needs_fix: "bg-warn-tint text-warn",
  expired: "bg-surface-2 text-ink-3",
};

const badgeIcon: Record<VersionState, ReactNode> = {
  uploading: <UploadSimple weight="bold" aria-hidden />,
  checking: <HourglassMedium weight="bold" aria-hidden />,
  accepted: <CheckCircle weight="bold" aria-hidden />,
  needs_fix: <WarningCircle weight="bold" aria-hidden />,
  expired: <Prohibit weight="bold" aria-hidden />,
};

export function StatusBadge({ state }: { state: VersionState }) {
  return (
    <span className={cx("inline-flex h-7 items-center gap-1.5 rounded-full px-3 text-sm font-medium", badgeStyle[state])}>
      {badgeIcon[state]}
      {stateLabel[state]}
    </span>
  );
}

export function Panel({ title, action, children, className }: { title?: string; action?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cx("rounded-xl border border-line bg-surface p-5 sm:p-6", className)}>
      {(title || action) && (
        <div className="mb-4 flex items-center justify-between gap-4">
          {title && <h2 className="text-base font-semibold tracking-tight text-ink">{title}</h2>}
          {action}
        </div>
      )}
      {children}
    </section>
  );
}

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={cx("animate-pulse rounded-lg bg-surface-2", className)} />;
}

/** Progress as a bar; transform-only so it stays smooth. */
export function ProgressBar({ value, label }: { value: number; label: string }) {
  const pct = Math.max(0, Math.min(1, value));
  return (
    <div role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct * 100)} className="h-2 w-full overflow-hidden rounded-full bg-surface-2">
      <div className="h-full origin-left rounded-full bg-accent transition-transform duration-300 ease-out" style={{ transform: `scaleX(${pct})` }} />
    </div>
  );
}

/** Where the hackathon is relative to its deadline, by the server's clock. */
export function deadlinePhase(window: Window, now: number): "open" | "grace" | "closed" {
  if (now <= Date.parse(window.deadline_at)) return "open";
  if (now <= Date.parse(window.grace_ends_at)) return "grace";
  return "closed";
}

export function Countdown({ window, now, size = "lg" }: { window: Window; now: number; size?: "lg" | "md" }) {
  const phase = deadlinePhase(window, now);
  const target = phase === "open" ? window.deadline_at : window.grace_ends_at;
  const left = Date.parse(target) - now;
  const urgent = phase === "open" && left < 10 * 60_000;
  return (
    <div className="flex flex-col gap-1">
      <p className="flex items-center gap-2 text-sm text-ink-2">
        <Clock weight="bold" aria-hidden />
        {phase === "open" && "Time left to submit"}
        {phase === "grace" && "Deadline passed. Uploads already started can finish for"}
        {phase === "closed" && "Submissions are closed"}
      </p>
      <p
        className={cx(
          "tabular font-mono font-semibold tracking-tight",
          size === "lg" ? "text-4xl sm:text-5xl" : "text-2xl",
          urgent ? "text-bad" : phase === "closed" ? "text-ink-3" : "text-ink",
        )}
        aria-live="off"
      >
        {phase === "closed" ? "00:00:00" : formatCountdown(left)}
      </p>
      <p className="text-sm text-ink-3">Deadline {formatWhen(window.deadline_at)}, judged by the server clock</p>
    </div>
  );
}
