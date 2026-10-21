import { useQuery } from "@tanstack/react-query";
import { getJSON } from "@/api/client";
import type { components } from "@/api/schema";
import { cn } from "@/lib/utils";

type PlanUsage = components["schemas"]["PlanUsage"];
type UsagePlan = components["schemas"]["UsagePlan"];
type Usage = components["schemas"]["Usage"];

/** The plans the factory's harnesses run on, with their latest usage readings. Empty with API keys. */
export const usePlanUsage = () => useQuery({ queryKey: ["usage"], queryFn: () => getJSON<PlanUsage>("/usage") });

export const windowName = (w: string) => (w === "5h" ? "5-hour" : w === "weekly" ? "Weekly" : w.replace(/_/g, " "));

/** "3:10pm" today, "Tue 3:10pm" within the week, else the date. */
export function resetTime(ts: string, now = new Date()): string {
  const t = new Date(ts);
  if (Number.isNaN(t.getTime())) return "";
  const clock = t.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }).replace(/\s?([AP])M/i, (_, p: string) => p.toLowerCase() + "m");
  if (t.toDateString() === now.toDateString()) return clock;
  if (t.getTime() - now.getTime() < 6 * 86400_000) return `${t.toLocaleDateString([], { weekday: "short" })} ${clock}`;
  return `${t.toLocaleDateString([], { month: "short", day: "numeric" })} ${clock}`;
}

/** "5-hour: 42% used, resets 3:10pm". */
export function windowLine(u: Usage): string {
  const reset = u.resets_at ? resetTime(u.resets_at) : "";
  return `${windowName(u.window)}: ${Math.round(u.used_percent)}% used${reset ? `, resets ${reset}` : ""}`;
}

/**
 * Each plan's usage windows, for the sidebar and the Spend screen: on a subscription the windows
 * are what limit the work, not dollars. Nothing with API keys.
 */
export function PlanUsageMeter({ className, stacked }: { className?: string; stacked?: boolean }) {
  const plans = usePlanUsage().data?.plans ?? [];
  if (plans.length === 0) return null;
  return (
    <div role="group" aria-label="Plan usage" className={className}>
      <div className={cn("flex min-w-0 gap-x-5 gap-y-1", stacked ? "flex-col gap-y-2" : "flex-wrap items-center")}>
        {plans.map((p) => (
          <PlanLine key={p.harness} p={p} />
        ))}
      </div>
    </div>
  );
}

function PlanLine({ p }: { p: UsagePlan }) {
  // The 5-hour and weekly windows are the ones that pause jobs; show those.
  const main = p.windows.filter((w) => w.window === "5h" || w.window === "weekly");
  const shown = main.length > 0 ? main : p.windows.slice(0, 1);
  const all = p.windows.map(windowLine).join("\n");
  return (
    <div
      data-testid="plan-usage"
      title={p.no_reading || `${p.name} on your ${p.plan}. Jobs wait once a window reaches ${p.pause_at}%.\n${all}`}
      className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-xs"
    >
      <span className="font-medium text-foreground">{p.name}</span>
      {shown.length === 0 ? (
        <span className="text-muted-foreground">no reading yet</span>
      ) : (
        shown.map((w) => {
          const near = w.used_percent >= p.pause_at * 0.8;
          return (
            <span key={w.window} className="flex min-w-0 items-center gap-1.5">
              <span
                className="h-1 w-8 shrink-0 rounded-full bg-border"
                role="meter"
                aria-label={`${p.name} ${windowName(w.window)} usage`}
                aria-valuenow={Math.round(w.used_percent)}
                aria-valuemin={0}
                aria-valuemax={100}
              >
                <span className={cn("block h-1 rounded-full", near ? "bg-warning-fill" : "bg-success-fill")} style={{ width: `${Math.min(100, w.used_percent)}%` }} />
              </span>
              <span className={cn("font-mono", near ? "font-semibold text-warning" : "text-muted-foreground")}>{windowLine(w)}</span>
            </span>
          );
        })
      )}
    </div>
  );
}

/**
 * A note beside dollar costs for jobs that ran on a plan: they cost no
 * dollars, and the plan's usage is what counts. Nothing when none of the
 * harnesses is on a plan.
 */
export function PlanCostNote({ harnesses, className }: { harnesses: string[]; className?: string }) {
  const plans = (usePlanUsage().data?.plans ?? []).filter((p) => harnesses.includes(p.harness));
  if (plans.length === 0) return null;
  return (
    <p data-testid="plan-cost-note" className={cn("text-xs text-muted-foreground", className)}>
      {plans.map((p) => (
        <span key={p.harness} className="block">
          {`${p.name} runs on your ${p.plan}, so its jobs cost no dollars here: the plan's usage is what counts.`}{" "}
          {p.windows.length > 0 ? p.windows.map(windowLine).join(". ") + "." : p.no_reading}
        </span>
      ))}
    </p>
  );
}
