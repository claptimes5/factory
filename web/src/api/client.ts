import { useEffect } from "react";
import { QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import type { components } from "./schema";

// Types come from internal/web/openapi.yaml (`npm run gen`), so the Go
// server and this app cannot drift without the build failing.
type S = components["schemas"];
export type Overview = S["Overview"];
export type Board = S["Board"];
export type Card = S["Card"];
export type Item = S["Item"];
export type ItemDetail = S["ItemDetail"];
export type Job = S["Job"];
export type JobDetail = S["JobDetail"];
export type Gate = S["Gate"];
export type Event = S["Event"];
export type PRPanel = S["PRPanel"];
export type Finding = S["Finding"];
export type Diff = S["Diff"];
export type Feed = S["Feed"];
export type Activity = S["Activity"];
export type ActivityEntry = S["ActivityEntry"];
export type Spend = S["Spend"];
export type Metrics = S["Metrics"];
export type Notification = S["Notification"];
export type Notifications = S["Notifications"];
export type ActionResult = S["ActionResult"];

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

const base = "/api/v1";

export async function getJSON<T>(path: string): Promise<T> {
  const r = await fetch(base + path, { headers: { Accept: "application/json" } });
  const body = await r.json().catch(() => ({ error: r.statusText }));
  if (!r.ok) throw new APIError((body as S["Error"]).error ?? r.statusText, r.status);
  return body as T;
}

/** POSTs a button press. The daemon may refuse it; that comes back as ok=false. */
export async function act(path: string, body: unknown = {}): Promise<ActionResult> {
  const r = await fetch(base + path, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(body),
  });
  const res = (await r.json().catch(() => ({ ok: false, message: r.statusText }))) as ActionResult;
  return { ok: r.ok && res.ok, message: res.message ?? (res as unknown as S["Error"]).error ?? r.statusText };
}

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The stream invalidates on every change; this is only a fallback
      // for when it is down.
      refetchInterval: 30_000,
      staleTime: 2_000,
      retry: (n, err) => !(err instanceof APIError && err.status === 404) && n < 2,
    },
  },
});

export const useOverview = () => useQuery({ queryKey: ["overview"], queryFn: () => getJSON<Overview>("/overview") });
export const useBoard = () => useQuery({ queryKey: ["board"], queryFn: () => getJSON<Board>("/board") });
export const useItem = (id: number) =>
  useQuery({ queryKey: ["item", id], queryFn: () => getJSON<ItemDetail>(`/items/${id}`) });
export const usePRPanel = (id: number) =>
  useQuery({ queryKey: ["pr", id], queryFn: () => getJSON<PRPanel>(`/items/${id}/pr`) });
export const useDiff = (id: number) =>
  useQuery({ queryKey: ["diff", id], queryFn: () => getJSON<Diff>(`/items/${id}/diff`) });
export const useJob = (id: number) => useQuery({ queryKey: ["job", id], queryFn: () => getJSON<JobDetail>(`/jobs/${id}`) });
export const useFeed = (item?: number) =>
  useQuery({
    queryKey: ["feed", item ?? 0],
    queryFn: () => getJSON<Feed>(item ? `/orchestrator?item=${item}` : "/orchestrator"),
  });
export const useActivity = () => useQuery({ queryKey: ["activity"], queryFn: () => getJSON<Activity>("/activity") });
export const useSpend = () => useQuery({ queryKey: ["spend"], queryFn: () => getJSON<Spend>("/spend") });
export const useMetrics = () => useQuery({ queryKey: ["metrics"], queryFn: () => getJSON<Metrics>("/metrics") });
export const useNotifications = () =>
  useQuery({ queryKey: ["notifications"], queryFn: () => getJSON<Notifications>("/notifications") });

/**
 * Refetches everything on screen whenever the daemon reports a change.
 *
 * Browsers allow six HTTP/1.1 connections per host, and an open stream holds
 * one, so tabs share a single stream: whichever tab holds a Web Lock opens
 * it and relays versions to the others over a BroadcastChannel. When that
 * tab closes, the lock passes to another.
 */
export function useLiveUpdates() {
  const qc = useQueryClient();
  useEffect(() => {
    let last = "";
    const seen = (v: string) => {
      if (last && v !== last) void qc.invalidateQueries();
      last = v;
    };
    const shared = typeof BroadcastChannel !== "undefined" && typeof navigator !== "undefined" && "locks" in navigator;
    const bc = shared ? new BroadcastChannel("factory-version") : null;
    if (bc) bc.onmessage = (e) => seen(e.data as string);
    let stop = () => {};
    const abort = new AbortController();
    const stream = () =>
      new Promise<void>((resolve) => {
        // A grant that lands after cleanup must not open a stream nothing will close.
        if (abort.signal.aborted) return resolve();
        let es: EventSource | null = null;
        let retry: ReturnType<typeof setTimeout> | undefined;
        const open = () => {
          es = new EventSource(base + "/stream");
          es.addEventListener("version", (e) => {
            const v = (JSON.parse((e as MessageEvent).data) as { version: string }).version;
            try {
              bc?.postMessage(v);
            } catch {
              /* channel closed during cleanup */
            }
            seen(v);
          });
          es.onerror = () => {
            es?.close();
            retry = setTimeout(open, 3000);
          };
        };
        open();
        stop = () => {
          es?.close();
          clearTimeout(retry);
          resolve();
        };
      });
    if (shared) {
      navigator.locks.request("factory-version-stream", { signal: abort.signal }, stream).catch(() => {});
    } else {
      void stream();
    }
    return () => {
      abort.abort();
      stop();
      bc?.close();
    };
  }, [qc]);
}
