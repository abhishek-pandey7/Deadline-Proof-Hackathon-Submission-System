import { SignOut } from "@phosphor-icons/react";
import type { ReactNode } from "react";
import type { User } from "../api";
import type { LiveState } from "../hooks";
import { Button, cx } from "../ui";

/** Header shared by the team page and the dashboard. */
export function Shell({ user, context, live, onSignOut, children }: { user: User; context: string; live?: LiveState; onSignOut: () => void; children: ReactNode }) {
  return (
    <div className="min-h-[100dvh]">
      <header className="border-b border-line bg-surface">
        <div className="mx-auto flex h-16 w-full max-w-6xl items-center justify-between gap-4 px-4 md:px-8">
          <div className="flex min-w-0 items-baseline gap-3">
            <span className="font-mono text-sm font-semibold text-accent">Deadline Crush</span>
            <span className="truncate text-sm text-ink-2">{context}</span>
          </div>
          <div className="flex items-center gap-3">
            {live && (
              <span className="hidden items-center gap-2 text-sm text-ink-3 sm:inline-flex" aria-live="polite">
                <span aria-hidden className={cx("size-2 rounded-full", live === "live" ? "bg-ok" : "bg-warn")} />
                {live === "live" ? "Live" : live === "connecting" ? "Connecting" : "Reconnecting"}
              </span>
            )}
            <span className="hidden truncate text-sm text-ink-2 md:inline">{user.email}</span>
            <Button variant="quiet" onClick={onSignOut} aria-label="Sign out">
              <SignOut weight="bold" aria-hidden />
              <span className="hidden sm:inline">Sign out</span>
            </Button>
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-6xl px-4 py-8 md:px-8 md:py-10">{children}</main>
    </div>
  );
}
