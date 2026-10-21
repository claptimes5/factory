import { Link } from "react-router";
import { useActivity, useMetrics, type ActivityEntry } from "@/api/client";
import { Empty, ErrorBox, Loading } from "@/components/common";
import { cn, money, pct, shortRef } from "@/lib/utils";

const toneColor: Record<ActivityEntry["tone"], string> = {
  live: "text-primary-soft",
  needs_you: "text-warning",
  done: "text-success",
  bad: "text-destructive",
  info: "text-primary-soft",
};

/** 14:05 today, or the day before that. */
function clock(at: string) {
  const d = new Date(at);
  if (Number.isNaN(d.getTime())) return at;
  const today = new Date().toDateString() === d.toDateString();
  return today ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }) : d.toLocaleDateString([], { month: "short", day: "numeric" });
}

/** What happened lately, newest first; repo narrows it to one repo's items. */
export function ActivityFeed({ repo, limit = 50, className }: { repo?: string; limit?: number; className?: string }) {
  const { data, error, isLoading } = useActivity(repo);
  if (isLoading) return <Loading what="Loading activity" />;
  if (error) return <ErrorBox error={error} />;
  const entries = data!.entries.slice(0, limit);
  if (entries.length === 0) return <Empty>Nothing has happened yet.</Empty>;
  return (
    <ol data-testid="activity" className={cn("flex flex-col gap-3.5 text-sm leading-snug", className)}>
      {entries.map((e) => {
        const body = (
          <>
            {e.actor === "orchestrator" && <span className="text-muted-foreground">Orchestrator{e.ref ? " on " : ": "}</span>}
            {e.ref && <span className={cn("font-mono text-meta", toneColor[e.tone])}>{shortRef(e.ref)}</span>}
            {e.ref && " "}
            <span className="text-text-2">{e.text}</span>
          </>
        );
        return (
          <li key={e.id} className="flex gap-3">
            <time dateTime={e.at} className="w-11 shrink-0 pt-0.5 font-mono text-xs text-muted-foreground">
              {clock(e.at)}
            </time>
            {e.item_id != null ? (
              <Link to={`/items/${e.item_id}`} className="min-w-0 break-words hover:underline">
                {body}
              </Link>
            ) : (
              <span className="min-w-0 break-words">{body}</span>
            )}
          </li>
        );
      })}
    </ol>
  );
}

/** The week's results, in three numbers. */
export function WeekStats({ className }: { className?: string }) {
  const { data: m } = useMetrics();
  if (!m) return null;
  const week = m.weekly.at(-1);
  const stats = [
    { v: String(week?.merged ?? 0), l: "merged this week" },
    { v: m.merged > 0 ? money(m.cost_per_merged_usd) : "–", l: "per merged PR" },
    { v: pct(m.first_pass_rate), l: "passed the gates first try" },
  ];
  return (
    <dl className={cn("grid grid-cols-3 gap-2 rounded-xl border bg-card p-3.5", className)}>
      {stats.map((s) => (
        <div key={s.l} className="flex flex-col-reverse justify-end">
          <dt className="text-xs leading-tight text-muted-foreground">{s.l}</dt>
          <dd className="font-display text-title font-semibold">{s.v}</dd>
        </div>
      ))}
    </dl>
  );
}

export function LiveHeading({ children = "Live" }: { children?: React.ReactNode }) {
  return (
    <h2 className="label flex items-center gap-2 text-muted-foreground">
      <span className="pulse size-2 rounded-full bg-success-fill" aria-hidden />
      {children}
    </h2>
  );
}
