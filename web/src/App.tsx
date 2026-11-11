import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type TeamRef, type User } from "./api";
import { Login } from "./pages/Login";
import { OrganizerPage } from "./pages/Organizer";
import { TeamPage } from "./pages/Team";
import { Skeleton } from "./ui";

type Session = { user: User; teams: TeamRef[] } | null | "loading";

function usePath(): [string, (p: string) => void] {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => {
    const onPop = () => setPath(window.location.pathname);
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const go = useCallback((p: string) => {
    if (p !== window.location.pathname) window.history.pushState(null, "", p);
    setPath(p);
  }, []);
  return [path, go];
}

export function App() {
  const [session, setSession] = useState<Session>("loading");
  const [path, go] = usePath();

  const load = useCallback(() => {
    api.me().then(setSession).catch((err) => setSession(err instanceof ApiError && err.status === 401 ? null : null));
  }, []);
  useEffect(load, [load]);

  const signOut = useCallback(async () => {
    await api.logout().catch(() => undefined);
    setSession(null);
    go("/");
  }, [go]);

  if (session === "loading") {
    return <div className="mx-auto max-w-5xl px-4 py-16"><Skeleton className="h-10 w-64" /><Skeleton className="mt-6 h-40" /></div>;
  }
  if (session === null) return <Login onSignedIn={load} />;

  const teamMatch = path.match(/^\/team\/([0-9a-f-]{36})$/i);
  if (path === "/organizer" && session.user.is_organizer) return <OrganizerPage user={session.user} onSignOut={signOut} />;
  if (teamMatch) return <TeamPage user={session.user} teamId={teamMatch[1]} onSignOut={signOut} />;

  // Default landing: organizers go to the dashboard, members to their team.
  if (session.user.is_organizer) {
    queueMicrotask(() => go("/organizer"));
    return null;
  }
  if (session.teams.length > 0) {
    queueMicrotask(() => go(`/team/${session.teams[0].id}`));
    return null;
  }
  return (
    <main className="mx-auto max-w-xl px-4 py-16">
      <h1 className="text-2xl font-semibold tracking-tight">You are not on a team yet</h1>
      <p className="mt-3 text-ink-2">Ask the organizers to add {session.user.email} to your team, then sign in again.</p>
    </main>
  );
}
