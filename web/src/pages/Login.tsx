import { SignIn } from "@phosphor-icons/react";
import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type Hackathon } from "../api";
import { useServerNow } from "../hooks";
import { Button, Countdown, Field, Panel, Skeleton, TextInput } from "../ui";

export function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [hackathon, setHackathon] = useState<Hackathon | null>(null);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const now = useServerNow(hackathon);

  useEffect(() => {
    api.hackathon().then(setHackathon).catch(() => setHackathon(null));
  }, []);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.login(email, password);
      onSignedIn();
    } catch (err) {
      if (err instanceof ApiError && err.code === "invalid_credentials") setError("That email and password do not match an account.");
      else if (err instanceof ApiError && err.code === "rate_limited") setError("Too many attempts. Wait a minute and try again.");
      else setError("Could not sign in. Check your connection and try again.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="mx-auto grid min-h-[100dvh] w-full max-w-5xl grid-cols-1 items-center gap-10 px-4 py-12 md:grid-cols-[1.1fr_1fr] md:gap-16 md:px-8">
      <div className="flex flex-col gap-8">
        <div className="flex flex-col gap-3">
          <p className="font-mono text-sm font-medium text-accent">Deadline Crush</p>
          <h1 className="text-4xl font-semibold leading-[1.1] tracking-tight text-ink md:text-5xl">
            {hackathon ? hackathon.name : "Hackathon submissions"}
          </h1>
          <p className="max-w-[46ch] text-base leading-relaxed text-ink-2">
            Upload your project before the deadline. Every version is kept, and the server's clock decides what counts.
          </p>
        </div>
        {hackathon ? <Countdown window={hackathon} now={now} /> : <Skeleton className="h-24 w-72" />}
      </div>

      <Panel>
        <form onSubmit={submit} className="flex flex-col gap-5" noValidate>
          <h2 className="text-lg font-semibold tracking-tight">Sign in to your team</h2>
          <Field label="Email">
            {({ id, describedBy, invalid }) => (
              <TextInput id={id} type="email" autoComplete="email" required value={email} onChange={(e) => setEmail(e.target.value)} aria-describedby={describedBy} aria-invalid={invalid} />
            )}
          </Field>
          <Field label="Password" error={error ?? undefined}>
            {({ id, describedBy, invalid }) => (
              <TextInput id={id} type="password" autoComplete="current-password" required value={password} onChange={(e) => setPassword(e.target.value)} aria-describedby={describedBy} aria-invalid={invalid} />
            )}
          </Field>
          <Button type="submit" disabled={busy || !email || !password}>
            <SignIn weight="bold" aria-hidden />
            {busy ? "Signing in" : "Sign in"}
          </Button>
          <details open className="rounded-lg bg-surface-2 px-4 py-3 text-sm text-ink-2">
            <summary className="cursor-pointer font-medium text-ink">Trying the demo?</summary>
            <p className="mt-2 leading-relaxed">
              Team member: <span className="font-mono">team1-member1@demo.test</span>
              <br />
              Organizer: <span className="font-mono">organizer@demo.test</span>
              <br />
              Password for both: <span className="font-mono">demo-pass-2026</span>
            </p>
          </details>
        </form>
      </Panel>
    </main>
  );
}
