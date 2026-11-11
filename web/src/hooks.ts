import { useCallback, useEffect, useRef, useState } from "react";
import type { Window } from "./api";

/**
 * The server's clock, as an offset from this device's. The deadline is judged
 * by the server, so every countdown is computed from server time; a wrong
 * laptop clock changes nothing.
 */
export function useServerNow(window: Window | null): number {
  const offset = useRef(0);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (window) offset.current = Date.parse(window.server_time) - Date.now();
  }, [window]);
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now() + offset.current), 250);
    return () => clearInterval(id);
  }, []);
  return now;
}

export type LiveState = "connecting" | "live" | "reconnecting";

/**
 * Subscribe to a server-sent event stream. Every event, including "ready"
 * on (re)connect, means "refetch"; the stream never carries state itself.
 * Refetches are coalesced so a burst of events costs one request.
 */
export function useLive(path: string | null, refetch: () => void): LiveState {
  const [state, setState] = useState<LiveState>("connecting");
  const pending = useRef<ReturnType<typeof setTimeout> | null>(null);
  const latest = useRef(refetch);
  latest.current = refetch;

  const schedule = useCallback(() => {
    if (pending.current) return;
    pending.current = setTimeout(() => {
      pending.current = null;
      latest.current();
    }, 300);
  }, []);

  useEffect(() => {
    if (!path) return;
    const es = new EventSource(path);
    const onEvent = () => {
      setState("live");
      schedule();
    };
    es.addEventListener("ready", onEvent);
    es.addEventListener("version", onEvent);
    es.addEventListener("resync", onEvent);
    es.onerror = () => setState("reconnecting");
    return () => {
      es.close();
      if (pending.current) clearTimeout(pending.current);
      pending.current = null;
    };
  }, [path, schedule]);

  return state;
}
