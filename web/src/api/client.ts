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
export type Estimate = S["Estimate"];
export type Plan = S["Plan"];
export type Job = S["Job"];
export type JobDetail = S["JobDetail"];
export type Gate = S["Gate"];
export type Event = S["Event"];
export type PRPanel = S["PRPanel"];
export type Finding = S["Finding"];
export type Diff = S["Diff"];
export type Preview = S["Preview"];
export type PreviewShot = S["PreviewShot"];
export type Feed = S["Feed"];
export type Activity = S["Activity"];
export type ActivityEntry = S["ActivityEntry"];
export type Spend = S["Spend"];
export type Comparison = S["Comparison"];
export type Metrics = S["Metrics"];
export type Notification = S["Notification"];
export type Notifications = S["Notifications"];
export type ActionResult = S["ActionResult"];
export type Settings = S["Settings"];
export type SettingsCheck = S["SettingsCheck"];
export type SettingsGroup = S["SettingsGroup"];
export type Setting = S["Setting"];
export type SettingValue = S["SettingValue"];
export type Tuning = S["Tuning"];
export type TuningProposal = S["TuningProposal"];
export type TuningChange = S["TuningChange"];
export type Choice = S["Choice"];
export type SetupFix = S["SetupFix"];
export type Stop = S["Stop"];
export type TranscriptEntry = S["TranscriptEntry"];
export type Wizard = S["Wizard"];
export type WizardStep = S["WizardStep"];
export type WizardAccess = S["WizardAccess"];
export type WizardChecks = S["WizardChecks"];
export type WizardSecret = S["WizardSecret"];
export type WizardResult = S["WizardResult"];
export type WizardManifest = S["WizardManifest"];
export type Happened = S["Happened"];
export type Split = S["Split"];
export type Uninstall = S["Uninstall"];
export type UninstallResult = S["UninstallResult"];

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

const base = "/api/v1";

/** A signed-out (or expired) session goes to the sign-in page, then back. */
function signIn(r: Response) {
  if (r.status === 401) window.location.assign("/login?next=" + encodeURIComponent(location.pathname + location.search));
}

export async function getJSON<T>(path: string): Promise<T> {
  const r = await fetch(base + path, { headers: { Accept: "application/json" } });
  signIn(r);
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
  signIn(r);
  const res = (await r.json().catch(() => ({ ok: false, message: r.statusText }))) as ActionResult;
  return { ok: r.ok && res.ok, message: res.message ?? (res as unknown as S["Error"]).error ?? r.statusText };
}

/** POSTs JSON and returns the reply, throwing the server's error when it refuses. */
export async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const r = await fetch(base + path, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(body),
  });
  signIn(r);
  const res = await r.json().catch(() => ({ error: r.statusText }));
  if (!r.ok) throw new APIError((res as S["Error"]).error ?? r.statusText, r.status);
  return res as T;
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
export type Home = S["Home"];
/** Home's catch-up: when it was last looked at, and what went live since. */
export const useHome = () => useQuery({ queryKey: ["home"], queryFn: () => getJSON<Home>("/home") });

export type Trust = S["Trust"];
export type WorkspaceTrust = S["WorkspaceTrust"];
/** Each workspace's trust level, and its repos' record and practice runs. */
export const useTrust = () => useQuery({ queryKey: ["trust"], queryFn: () => getJSON<Trust>("/trust") });

export const useBoard = (opts: { enabled?: boolean; refetchInterval?: number | ((q: { state: { data?: Board } }) => number) } = {}) =>
  useQuery({ queryKey: ["board"], queryFn: () => getJSON<Board>("/board"), ...opts });
export const useItem = (id: number) =>
  useQuery({ queryKey: ["item", id], queryFn: () => getJSON<ItemDetail>(`/items/${id}`) });
export const usePRPanel = (id: number) =>
  useQuery({ queryKey: ["pr", id], queryFn: () => getJSON<PRPanel>(`/items/${id}/pr`) });
export const usePreview = (id: number) =>
  useQuery({ queryKey: ["preview", id], queryFn: () => getJSON<Preview>(`/items/${id}/preview`) });
export const useDiff = (id: number) =>
  useQuery({ queryKey: ["diff", id], queryFn: () => getJSON<Diff>(`/items/${id}/diff`) });
export const useJob = (id: number, opts: { enabled?: boolean } = {}) =>
  useQuery({ queryKey: ["job", id], queryFn: () => getJSON<JobDetail>(`/jobs/${id}`), ...opts });
/** A stopped build's work against the base branch, as kept on the factory's machine. */
export const useSavedDiff = (id: number, enabled: boolean) =>
  useQuery({ queryKey: ["saved-diff", id], queryFn: () => getJSON<Diff>(`/jobs/${id}/saved-diff`), enabled });
/** A query string of the params that are set. */
const query = (params: Record<string, string | number | undefined>) => {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v) q.set(k, String(v));
  const s = q.toString();
  return s ? `?${s}` : "";
};
/** The orchestrator's decisions: one item's, or a workspace's. */
export const useFeed = (item?: number, workspace = "") =>
  useQuery({
    queryKey: ["feed", item ?? 0, workspace],
    queryFn: () => getJSON<Feed>(`/orchestrator${query({ item, workspace })}`),
  });
export const useActivity = (repo = "", workspace = "") =>
  useQuery({
    queryKey: ["activity", repo, workspace],
    queryFn: () => getJSON<Activity>(`/activity${query({ repo, workspace })}`),
  });
export const useSpend = () => useQuery({ queryKey: ["spend"], queryFn: () => getJSON<Spend>("/spend") });
export const useCompare = () =>
  useQuery({ queryKey: ["compare"], queryFn: () => getJSON<S["Comparisons"]>("/compare") });
export const useMetrics = () => useQuery({ queryKey: ["metrics"], queryFn: () => getJSON<Metrics>("/metrics") });
export const useNotifications = () =>
  useQuery({ queryKey: ["notifications"], queryFn: () => getJSON<Notifications>("/notifications") });
export const useSettings = () => useQuery({ queryKey: ["settings"], queryFn: () => getJSON<Settings>("/settings") });
/** What removing the factory would remove. It asks every repo's host, so it loads only when asked for. */
export const useUninstall = (enabled: boolean) =>
  useQuery({ queryKey: ["uninstall"], queryFn: () => getJSON<Uninstall>("/settings/uninstall"), enabled, refetchInterval: false, staleTime: Infinity });

/** Writes changed settings to factory.yaml. An invalid result is refused with the reason, and nothing is written. */
export const saveSettings = (changes: { path: string[]; value: SettingValue }[], version: string) =>
  postJSON<Settings>("/settings", { changes, version });

/** What saving these changes would say, without writing them. */
export const checkSettings = (changes: { path: string[]; value: SettingValue }[]) => postJSON<SettingsCheck>("/settings/check", { changes });

/** Sets how hard the factory works, for the harnesses named (empty: as they are). */
export const applyPreset = (body: { key?: string; builder?: string; reviewer?: string; version: string }) => postJSON<Settings>("/settings/preset", body);

/**
 * The get-started page. It polls while a check or an image build runs.
 */
// When the page first saw the machine check still running, so it can stop
// polling for it after a minute and say it didn't finish.
let machineWaitSince: number | null = null;
const machineWait = 60_000;

/** The machine check has run longer than the page waits for it. */
export const machineCheckStalled = () => machineWaitSince !== null && Date.now() - machineWaitSince > machineWait;

export const useWizard = () =>
  useQuery({
    queryKey: ["wizard"],
    // live: the server checks the forges again (at most every 30s), so a
    // revoked token, an uninstalled app or a deleted label shows here.
    queryFn: () => getJSON<Wizard>("/setup?live=1"),
    refetchInterval: (q) => {
      const w = q.state.data;
      if (!w) return 30_000;
      if (w.machine_checked_at !== "") machineWaitSince = null;
      else if (machineWaitSince === null) machineWaitSince = Date.now();
      const checking = w.machine_checked_at === "" && !machineCheckStalled();
      return checking || w.images?.building || w.codex?.sign_in.running || w.checks.some((c) => c.try?.running) ? 2_000 : 30_000;
    },
  });

/** Which mode the dashboard runs in, without asking any forge. */
export const useWizardMode = () =>
  useQuery({ queryKey: ["wizard-mode"], queryFn: () => getJSON<Wizard>("/setup"), retry: false, refetchInterval: false, staleTime: 60_000 });

/** Runs one of the get-started page's actions; a refusal throws with the reason. */
export const wizardDo = (action: string, body: Record<string, unknown> = {}) => postJSON<WizardResult>(`/setup/${action}`, body);

export const useTuning = () => useQuery({ queryKey: ["tuning"], queryFn: () => getJSON<Tuning>("/settings/tuning") });

/** Applies the picked changes of a tuning proposal (by index) to factory.yaml as it is now, through the same checks as a save. */
export const applyTuning = (id: number, version: string, changes: number[]) =>
  postJSON<Settings>(`/settings/tuning/${id}/apply`, { version, changes });

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
