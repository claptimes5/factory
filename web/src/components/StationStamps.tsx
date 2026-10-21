import type { Item, Job } from "@/api/client";
import { cn } from "@/lib/utils";

// A job's way along the line as stamps: one for each station it has
// passed, a dashed ring where it is now, and an empty one ahead. The
// last before Live is yours (docs/plans/design-system.md).

const stations = [
  { key: "plan", label: "Plan" },
  { key: "build", label: "Build" },
  { key: "review", label: "Review" },
  { key: "you", label: "You" },
  { key: "live", label: "Live" },
] as const;

const at: Record<string, number> = { epic: 0, triage: 0, spec: 0, ready: 0, implementing: 1, reviewing: 2, awaiting_human: 3, done: 5 };

/** How far it got: by its stage, or, parked or blocked, by the jobs that ran for it. */
function reached(it: Item, jobs: Job[]): number {
  if (it.stage in at) return at[it.stage];
  const ok = (k: string) => jobs.some((j) => j.kind === k && j.state === "succeeded");
  return ok("review") ? 3 : ok("implement") ? 2 : ok("triage") ? 1 : 0;
}

// A stamp's tilt, so a row doesn't look printed.
const tilt = ["-rotate-6", "rotate-3", "-rotate-2", "rotate-6", "-rotate-3"];

export function StationStamps({ it, jobs }: { it: Item; jobs: Job[] }) {
  const n = reached(it, jobs);
  const stuck = it.stage === "parked" || it.stage === "blocked";
  return (
    <ol aria-label="Along the line" className="flex flex-wrap gap-2.5" data-testid="stamps">
      {stations.map((s, i) => {
        const done = i < n;
        const here = i === n && !stuck;
        const you = s.key === "you" && here;
        return (
          <li
            key={s.key}
            aria-label={`${s.label}: ${done ? "done" : here ? "now" : stuck && i === n ? "stopped here" : "ahead"}`}
            className={cn(
              "flex size-14 items-center justify-center rounded-full border-2 font-mono text-caption font-semibold uppercase",
              done && cn("border-primary text-primary", tilt[i]),
              here && !you && "border-dashed border-primary text-primary",
              you && "border-dashed border-warning bg-warning-bg text-warning",
              stuck && i === n && "border-dashed border-destructive text-destructive",
              !done && !here && !(stuck && i === n) && "border-dashed border-border text-muted-foreground",
              s.key === "live" && done && "border-success text-success",
            )}
          >
            {s.label}
          </li>
        );
      })}
    </ol>
  );
}
