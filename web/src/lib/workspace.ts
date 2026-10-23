import { useSyncExternalStore } from "react";
import { useOverview, type Overview } from "@/api/client";
import { setRepo, useRepo } from "@/lib/repo";

// The workspace the switcher picked, shared by every screen: kept across
// visits, and in the address as ?ws= so a link opens the right one. Every
// screen shows one workspace; "" until the factory says which it has.

const read = () => {
  const fromURL = new URLSearchParams(window.location.search).get("ws");
  if (fromURL) return fromURL;
  try {
    return localStorage.getItem("workspace") ?? "";
  } catch {
    return "";
  }
};
let current = read();
const listeners = new Set<() => void>();

export function setWorkspace(ws: string) {
  if (ws === current) return;
  current = ws;
  try {
    localStorage.setItem("workspace", ws);
  } catch {
    /* storage may be unavailable */
  }
  const url = new URL(window.location.href);
  if (url.searchParams.has("ws")) {
    url.searchParams.set("ws", ws);
    window.history.replaceState(window.history.state, "", url);
  }
  setRepo(""); // a repo picked in one workspace isn't in the next
  listeners.forEach((l) => l());
}

export type Workspace = Overview["workspaces"][number];

/** What a screen shows: the chosen workspace, its repos, and the repo picked within it ("" for all of them). */
export type Scope = {
  workspace: string;
  workspaces: Workspace[];
  repos: string[];
  repo: string;
  /** Whether a repo's work belongs on the screen. */
  inScope: (repo: string) => boolean;
  /** The workspace a repo is in, or "". */
  workspaceOf: (repo: string) => string;
};

export function useScope(): Scope {
  const workspaces = useOverview().data?.workspaces ?? [];
  const chosen = useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => current,
  );
  // One since dropped from the config, or none chosen yet, is the first.
  const ws = workspaces.find((w) => w.name === chosen) ?? workspaces[0];
  const repos = ws?.repos ?? [];
  const repo = useRepo(repos);
  const lower = repos.map((r) => r.toLowerCase());
  const workspaceOf = (r: string) => workspaces.find((w) => w.repos.some((x) => x.toLowerCase() === r.toLowerCase()))?.name ?? "";
  return {
    workspace: ws?.name ?? "",
    workspaces,
    repos,
    repo,
    // Before the overview loads, nothing is left out.
    inScope: (r: string) => !ws || (repo ? r === repo : lower.includes(r.toLowerCase())),
    workspaceOf,
  };
}
