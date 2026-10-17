import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export const money = (v: number) => `$${v.toFixed(2)}`;

/** Rates come as 0..1, or -1 when there is no data yet. */
export const pct = (v: number) => (v < 0 ? "–" : `${Math.round(v * 100)}%`);

export function ago(ts: string, now = Date.now()): string {
  const t = Date.parse(ts);
  if (Number.isNaN(t)) return ts;
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export function duration(from: string, to?: string): string {
  const a = Date.parse(from);
  const b = to ? Date.parse(to) : Date.now();
  if (Number.isNaN(a) || Number.isNaN(b)) return "";
  const s = Math.max(0, Math.round((b - a) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

const stageNames: Record<string, string> = {
  epic: "Epics",
  triage: "Triage",
  spec: "Spec",
  ready: "Ready",
  implementing: "Implementing",
  reviewing: "Reviewing",
  awaiting_human: "Needs review",
  parked: "Parked",
  blocked: "Blocked",
  done: "Done",
};
export const stageName = (s: string) => stageNames[s] ?? s;

export const shortSha = (s: string) => s.slice(0, 7);
