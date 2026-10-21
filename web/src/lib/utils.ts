import { clsx, type ClassValue } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

// tailwind-merge has to know the design system's type scale and radii
// (index.css): else it reads text-body as a color and drops the text
// color beside it.
const twMerge = extendTailwindMerge({
  extend: {
    theme: {
      text: ["caption", "meta", "body-sm", "body", "title-sm", "title", "display"],
      radius: ["control", "card"],
    },
  },
});

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
  implementing: "Building",
  reviewing: "Reviewing",
  awaiting_human: "PR ready",
  parked: "Parked",
  blocked: "Blocked",
  done: "Done",
};
export const stageName = (s: string) => stageNames[s] ?? s;

/** "acme/web#101" as "web#101": the owner is the same everywhere. */
export const shortRef = (ref: string) => ref.replace(/^[^/#]+\//, "");

/** A repo's name without its owner. */
export const repoName = (repo: string) => repo.slice(repo.indexOf("/") + 1);

export const shortSha = (s: string) => s.slice(0, 7);

/**
 * A note as a line or two for a card: the part before any log or code it
 * quotes ("Last failure: ```…"), in whole sentences up to about max
 * characters. The first sentence shows whole unless it runs past twice that.
 */
export function brief(note: string, max = 220): string {
  let s = note.split("```")[0];
  const cut = s.search(/\b(Last failure|Output|Log):/);
  if (cut > 0) s = s.slice(0, cut);
  // Who parked it is noise on a card: the card already says it needs you.
  s = s.replace(/^Parked by [^:]{1,40}:\s*/, "").replace(/\s+/g, " ").trim();
  if (s.length <= max) return s;
  const sentences = s.split(/(?<=[.!?])\s+/);
  let out = sentences[0];
  for (const n of sentences.slice(1)) {
    if (out.length + 1 + n.length > max) break;
    out += " " + n;
  }
  if (out.length > max * 2) out = out.slice(0, max * 2).replace(/\s+\S*$/, "") + "…";
  return out;
}
