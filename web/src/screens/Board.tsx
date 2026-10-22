import { useMemo, useState } from "react";
import { Link } from "react-router";
import { ExternalLink } from "lucide-react";
import { useBoard, useOverview, type Card as CardT } from "@/api/client";
import { ActivityFeed, LiveHeading, WeekStats } from "@/components/Activity";
import { ErrorBox, Loading, WaitsFor } from "@/components/common";
import { Choices } from "@/components/Choices";
import { FoundBy } from "@/components/FoundBy";
import { Badge } from "@/components/ui/badge";
import { useRepo } from "@/lib/repo";
import { brief, cn, duration, money, repoName, shortRef, stageName } from "@/lib/utils";
import { useReadiness } from "@/screens/Setup";
import type { components } from "@/api/schema";

type RepoReadiness = components["schemas"]["RepoReadiness"];

// The line's stations, in the order work moves through them. The quiet
// ones show only when they hold something.
const floor = ["epic", "triage", "spec", "ready", "implementing", "reviewing", "blocked"];
const quiet = new Set(["epic", "spec", "blocked"]);
const live = new Set(["implementing", "reviewing"]);

export function BoardScreen() {
  // While something runs, its card's activity line and clock move: refresh often.
  const { data, error, isLoading } = useBoard({
    refetchInterval: (q) => (q.state.data?.columns.some((c) => c.cards.some((k) => k.live)) ? 10_000 : 30_000),
  });
  const repos = useOverview().data?.repos ?? [];
  const repo = useRepo(repos);
  const setupRun = useReadiness().data?.repos.find((r) => r.checking && (!repo || r.repo === repo));
  const [open, setOpen] = useState<string>(""); // phone: the quiet stage opened below
  const [showDone, setShowDone] = useState(false);
  const [cols, held] = useMemo(() => {
    const m = new Map<string, CardT[]>();
    for (const c of data?.columns ?? []) m.set(c.stage, c.cards.filter((k) => !repo || k.repo === repo));
    // An item waiting for your OK on its cost or its plan needs you, not its column.
    const blocked = m.get("blocked") ?? [];
    const ok = (k: CardT) => k.estimate_waiting || k.plan_waiting;
    m.set("blocked", blocked.filter((k) => !ok(k)));
    return [m, blocked.filter(ok)] as const;
  }, [data, repo]);
  if (isLoading) return <Loading what="Loading the floor" />;
  if (error) return <ErrorBox error={error} />;

  const needs = [...(cols.get("awaiting_human") ?? []), ...held, ...(cols.get("parked") ?? [])];
  const shown = floor.filter((s) => !quiet.has(s) || (cols.get(s)?.length ?? 0) > 0);
  const done = cols.get("done") ?? [];
  const moving = shown.filter((s) => live.has(s)).flatMap((s) => cols.get(s) ?? []);
  const waiting = shown.filter((s) => !live.has(s));

  return (
    <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_340px]" data-testid="board">
      <div className="flex min-w-0 flex-col gap-6">
        <section aria-label="Needs you">
          <h2 className="label mb-3 flex items-center gap-2 text-foreground">
            Needs you
            <span className="rounded-full bg-ink px-2 font-mono text-caption text-ink-foreground">{needs.length}</span>
          </h2>
          {needs.length === 0 ? (
            <p className="rounded-card border border-dashed px-4 py-5 text-body-sm text-muted-foreground">Nothing needs you. The line is running on its own.</p>
          ) : (
            <div className="grid gap-3.5 lg:grid-cols-2">
              {needs.map((c) => (
                <NeedsYouCard key={c.id} card={c} />
              ))}
            </div>
          )}
        </section>

        {/* Laptop and tablet: a column per stage. */}
        <section aria-label="The floor" className="grid gap-3.5 max-md:hidden md:grid-cols-2 lg:grid-cols-[repeat(auto-fit,minmax(220px,1fr))]">
          {shown.map((s) => (
            <Column key={s} stage={s} cards={cols.get(s) ?? []} setup={s === "implementing" ? setupRun : undefined} />
          ))}
        </section>

        {/* Phone: what's moving as cards, the rest a tap away. */}
        <section aria-label="On the floor" className="flex flex-col gap-2.5 md:hidden">
          <h2 className="label text-muted-foreground">On the floor</h2>
          {setupRun && <SetupRunCard r={setupRun} />}
          {moving.map((c) => (
            <FloorCard key={c.id} card={c} showStage />
          ))}
          {moving.length === 0 && !setupRun && <p className="text-sm text-muted-foreground">Nothing is building or in review.</p>}
          <div className="grid grid-cols-3 gap-2">
            {[...waiting, "done"].map((s) => {
              const n = (s === "done" ? done : (cols.get(s) ?? [])).length;
              return (
                <button
                  key={s}
                  aria-expanded={open === s}
                  onClick={() => setOpen(open === s ? "" : s)}
                  className={cn(
                    "flex h-11 cursor-pointer items-center justify-center gap-1.5 rounded-control border bg-card text-meta",
                    open === s && "border-foreground",
                  )}
                >
                  {stageName(s)} <span className="font-mono text-muted-foreground">{n}</span>
                </button>
              );
            })}
          </div>
          {open && (
            <div role="region" aria-label={stageName(open)} className="flex flex-col gap-2.5">
              {(open === "done" ? done : (cols.get(open) ?? [])).map((c) => (
                <FloorCard key={c.id} card={c} />
              ))}
              {(open === "done" ? done : (cols.get(open) ?? [])).length === 0 && <p className="text-sm text-muted-foreground">Nothing here.</p>}
            </div>
          )}
        </section>

        <section aria-label="Done" className="max-md:hidden">
          <button onClick={() => setShowDone(!showDone)} aria-expanded={showDone} className="label cursor-pointer text-muted-foreground hover:text-foreground">
            Done <span className="font-mono tracking-normal">{done.length}</span> {showDone ? "▴" : "▾"}
          </button>
          {showDone && (
            <ul className="mt-3 divide-y rounded-card border bg-card">
              {done.map((c) => (
                <li key={c.id}>
                  <Link to={`/items/${c.id}`} className="flex items-center gap-3 px-4 py-2.5 text-sm hover:bg-accent">
                    <span className="font-mono text-xs text-success">{shortRef(c.ref)}</span>
                    <span className="min-w-0 flex-1 truncate">{c.title}</span>
                    {c.cost_usd > 0 && <span className="font-mono text-xs text-muted-foreground">{money(c.cost_usd)}</span>}
                  </Link>
                </li>
              ))}
              {done.length === 0 && <li className="px-4 py-3 text-sm text-muted-foreground">Nothing done yet.</li>}
            </ul>
          )}
        </section>
      </div>

      <aside aria-label="Live activity" className="flex flex-col gap-4 max-md:hidden xl:sticky xl:top-[88px] xl:max-h-[calc(100vh-112px)] xl:self-start">
        <div className="flex items-center justify-between">
          <LiveHeading />
          <Link to="/history" className="text-sm text-primary hover:underline">
            All activity
          </Link>
        </div>
        <div className="min-h-0 overflow-y-auto xl:flex-1">
          <ActivityFeed repo={repo} limit={12} />
        </div>
        <WeekStats />
      </aside>
    </div>
  );
}

function Column({ stage, cards, setup }: { stage: string; cards: CardT[]; setup?: RepoReadiness }) {
  return (
    <section aria-label={stageName(stage)} className="flex min-w-0 flex-col gap-2.5">
      <h3 className="label flex justify-between text-muted-foreground">
        {stageName(stage)} <span className="font-mono tracking-normal">{cards.length + (setup ? 1 : 0)}</span>
      </h3>
      {setup && <SetupRunCard r={setup} />}
      {cards.map((c, i) => (
        <FloorCard key={c.id} card={c} next={stage === "ready" && i === 0} />
      ))}
      {cards.length === 0 && !setup && <div className="rounded-control border border-dashed p-3 text-center text-xs text-muted-foreground">Nothing here</div>}
    </section>
  );
}

/** A title that is the card's link, stretched over the card so the whole card opens it. */
function CardLink({ card, className }: { card: CardT; className?: string }) {
  return (
    <Link to={`/items/${card.id}`} className={cn("after:absolute after:inset-0 after:rounded-[inherit] focus-visible:outline-none", className)}>
      {card.title}
    </Link>
  );
}

const doing: Record<string, string> = {
  implement: "building",
  review: "reviewing the diff",
  critic: "a second critic is reading",
  triage: "sizing it up",
};

function FloorCard({ card: c, next, showStage }: { card: CardT; next?: boolean; showStage?: boolean }) {
  const kind = c.live_kind;
  return (
    <article
      data-testid="card"
      className={cn(
        "relative flex flex-col gap-2.5 rounded-control border bg-card p-3.5 transition-colors focus-within:ring-2 focus-within:ring-ring hover:border-primary/60",
        c.live && "border-live-border bg-live-bg",
        c.stage === "blocked" && "border-destructive/50",
      )}
    >
      <div className="flex items-center justify-between gap-2 font-mono text-xs">
        <span className="text-muted-foreground">
          {shortRef(c.ref)}
          {showStage && ` · ${stageName(c.stage).toLowerCase()}`}
        </span>
        {c.cost_usd > 0 && <span>{money(c.cost_usd)}</span>}
      </div>
      <CardLink card={c} className="text-body leading-snug font-semibold" />
      <FoundBy source={c.self_started} className="text-muted-foreground" />
      {c.live && (
        <div className={cn("flex min-w-0 items-center gap-2 text-xs text-primary-soft", c.activity && "font-mono")}>
          <span title="A job is running" className="pulse size-[7px] shrink-0 rounded-full bg-primary" />
          <span className="truncate">{c.activity || doing[kind] || kind}</span>
        </div>
      )}
      {c.live && kind === "implement" && <div className="working h-[3px] rounded-full" aria-hidden />}
      {c.gates_total > 0 && c.stage !== "implementing" && <GateBar passed={c.gates_passed} total={c.gates_total} />}
      {c.stage === "blocked" && c.blocked_note && <p className="line-clamp-2 text-xs text-muted-foreground">{brief(c.blocked_note)}</p>}
      <WaitsFor items={c.waits_for} />
      <Footer card={c} next={next} />
    </article>
  );
}

function Footer({ card: c, next }: { card: CardT; next?: boolean }) {
  const parts: string[] = [];
  const building = c.live && c.live_kind === "implement";
  if (building && c.model) parts.push(c.model);
  if (building && c.attempt > 0) parts.push(`attempt ${c.attempt}`);
  if (!building && c.stage === "reviewing" && c.model) parts.push(`${c.model} built it`);
  if (c.live && c.live_since) parts.push(duration(c.live_since));
  if (!c.live && c.gates_total > 0) parts.push(`gates ${c.gates_passed}/${c.gates_total}`);
  if (c.estimate && !c.live && ["ready", "blocked"].includes(c.stage)) parts.push(`est. ${c.estimate}`);
  if (parts.length === 0 && !c.complexity && !next) return null;
  return (
    <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
      {c.complexity && !c.live && <span className="rounded border px-1.5">{c.complexity}</span>}
      {next && <span>next in line</span>}
      {parts.length > 0 && <span>{parts.join(" · ")}</span>}
    </div>
  );
}

function GateBar({ passed, total }: { passed: number; total: number }) {
  return (
    <div className="flex gap-1" role="img" aria-label={`Gates: ${passed} of ${total} passed`}>
      {Array.from({ length: total }, (_, i) => (
        <span key={i} className={cn("h-1 flex-1 rounded-full", i < passed ? "bg-success-fill" : "bg-destructive")} />
      ))}
    </div>
  );
}

function Dot({ ok }: { ok: boolean | null }) {
  return <span className={cn("size-2 shrink-0 rounded-full", ok === null ? "pulse bg-primary" : ok ? "bg-success-fill" : "bg-destructive")} aria-hidden />;
}

/** A setup run (a check, a lint baseline) on the line, like a job: it isn't an item, so it opens Setup. */
function SetupRunCard({ r }: { r: RepoReadiness }) {
  return (
    <article data-testid="setup-run" className="relative flex flex-col gap-1.5 rounded-card border border-primary/40 bg-surface p-3.5">
      <div className="flex items-center gap-2 text-caption text-muted-foreground">
        <span className="pulse inline-block size-1.5 rounded-full bg-primary" aria-hidden />
        Setup · {repoName(r.repo) || r.repo}
      </div>
      <Link to="/setup" className="text-body-sm font-semibold after:absolute after:inset-0 hover:underline">
        Checking {repoName(r.repo) || r.repo}
      </Link>
      <span className="text-meta text-muted-foreground">{duration(r.checking_since)}</span>
    </article>
  );
}

function NeedsYouCard({ card: c }: { card: CardT }) {
  const pr = c.stage === "awaiting_human";
  const plan = c.plan_waiting;
  const held = c.estimate_waiting || plan;
  const chooser = !pr && !held && c.choice_job_id != null && c.choices.some((x) => x.available);
  const btn = "relative z-10 flex h-11 items-center gap-1.5 rounded-control px-3.5 text-body-sm font-medium md:h-9";
  const main = "bg-ink text-ink-foreground hover:bg-ink/90";
  const other = "border bg-surface hover:bg-accent";
  return (
    <article
      data-testid="card"
      className="relative flex flex-col gap-3 rounded-card border bg-surface p-4 transition-colors focus-within:ring-2 focus-within:ring-ring hover:border-muted-foreground/40"
    >
      <div className="flex items-center gap-2.5">
        <span className="font-mono text-caption text-muted-foreground">{shortRef(c.ref)}</span>
        <Badge variant="attention">{pr ? "PR ready" : plan ? "Plan check" : held ? "Cost check" : chooser ? "Your call" : "Question"}</Badge>
        {c.setup && <Badge variant={c.setup === "blocks" ? "warning" : "secondary"}>{{ blocks: "Blocks setup", held: "Waits for setup", image: "Waits for its image" }[c.setup]}</Badge>}
        <span className="ml-auto font-mono text-xs text-muted-foreground">
          {pr
            ? [c.model, c.cost_usd > 0 && money(c.cost_usd)].filter(Boolean).join(" · ")
            : [c.complexity && `size ${c.complexity}`, c.estimate_waiting && c.estimate].filter(Boolean).join(" · ")}
        </span>
      </div>
      <CardLink card={c} className="text-title-sm leading-tight font-semibold" />
      <FoundBy source={c.self_started} className="text-muted-foreground" />
      {(c.setup === "blocks" || c.setup === "image") && c.setup_note && <p className="text-sm leading-relaxed text-text-2">{c.setup_note}</p>}
      {pr ? (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-meta text-text-2">
          {c.gates_total > 0 && (
            <span className="flex items-center gap-1.5">
              <Dot ok={c.gates_passed === c.gates_total} />
              {c.gates_passed} of {c.gates_total} gates
            </span>
          )}
          {c.live && (
            <span className="flex items-center gap-1.5">
              <Dot ok={null} />
              {c.activity || doing[c.live_kind] || "working"}
            </span>
          )}
        </div>
      ) : (
        c.blocked_note && <p className="line-clamp-3 text-sm leading-relaxed text-text-2">“{brief(c.blocked_note)}”</p>
      )}
      <WaitsFor items={c.waits_for} plan={plan} className="text-meta" />
      <div className="mt-auto flex flex-wrap gap-2">
        {pr && c.prs.length > 1
          ? // A change across a workspace: one PR per repo, in the order they merge.
            c.prs.map((p, i) => (
              <a
                key={p.url}
                href={p.url}
                target="_blank"
                rel="noreferrer"
                title={`Merge ${i + 1} of ${c.prs.length}`}
                className={cn(btn, main)}
              >
                {i + 1}. Review {p.ref} <ExternalLink className="size-3.5" aria-hidden />
              </a>
            ))
          : pr &&
            c.pr_url && (
              <a href={c.pr_url} target="_blank" rel="noreferrer" className={cn(btn, main)}>
                Review PR #{c.prs[0]?.number ?? c.pr_number} <ExternalLink className="size-3.5" aria-hidden />
              </a>
            )}
        {pr && (
          <Link to={`/items/${c.id}/diff`} className={cn(btn, other)}>
            View diff
          </Link>
        )}
        {held && (
          <Link to={`/items/${c.id}`} className={cn(btn, main)}>
            {plan ? "Read the plan" : "See the estimate"}
          </Link>
        )}
        {plan && c.issue_url && (
          <a href={c.issue_url} target="_blank" rel="noreferrer" className={cn(btn, other)}>
            Open the issue <ExternalLink className="size-3.5" aria-hidden />
          </a>
        )}
        {!pr && !held && c.issue_url && (
          <a
            href={c.issue_url}
            target="_blank"
            rel="noreferrer"
            className={cn(btn, chooser ? other : main)}
          >
            {chooser ? "Open the issue" : "Answer on the issue"} <ExternalLink className="size-3.5" aria-hidden />
          </a>
        )}
      </div>
      {chooser && (
        // What to do next, right here: the issue is only the other way.
        <div className="relative z-10">
          <Choices jobId={c.choice_job_id!} itemId={c.id} choices={c.choices} compact />
        </div>
      )}
    </article>
  );
}
