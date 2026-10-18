import { useSyncExternalStore } from "react";

// The repo the header's chips picked, shared by every screen and kept
// across visits. "" is every repo.
let current = (() => {
  try {
    return localStorage.getItem("repo") ?? "";
  } catch {
    return "";
  }
})();
const listeners = new Set<() => void>();

export function setRepo(repo: string) {
  current = repo;
  try {
    localStorage.setItem("repo", repo);
  } catch {
    /* storage may be unavailable */
  }
  listeners.forEach((l) => l());
}

export function useRepo(repos: string[]): string {
  const r = useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => current,
  );
  // A repo since dropped from the config shows everything.
  return repos.includes(r) ? r : "";
}
