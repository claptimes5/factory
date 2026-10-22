import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, ExternalLink, MessageCircle, ShieldCheck } from "lucide-react";
import { postJSON, useBoard, useHome, useOverview, useTrust, type Card as CardT } from "@/api/client";
import { ErrorBox, Inline, Loading, WaitsFor } from "@/components/common";
import { Choices } from "@/components/Choices";
import { FoundBy } from "@/components/FoundBy";
import { Button } from "@/components/ui/button";
import { useRepo } from "@/lib/repo";
import { brief, cn, duration, money, repoName, shortRef } from "@/lib/utils";
import { NextActions, nextStep, RunProgress, setupActivity, useReadiness, useReadinessRun, yourPartLeft } from "@/screens/Setup";
import type { components } from "@/api/schema";

type Readiness = components["schemas"]["Readiness"];
type RepoReadiness = components["schemas"]["RepoReadiness"];

// Home: what needs you first, one decision at a time; the line for work
// that's moving; what went live since you looked; and Ask. Everything
// else is a screen away (docs/plans/design-system.md).

/** Ask opens from anywhere: the frame listens for this. */
export const openAsk = () => window.dispatchEvent(new Event("factory:ask"));

type Kind = "review" | "plan" | "cost" | "choice" | "question";

const kindOf = (c: CardT): Kind =>
  c.stage === "awaiting_human"
    ? "review"
    : c.plan_waiting
      ? "plan"
      : c.estimate_waiting
        ? "cost"
        : c.choice_job_id != null && c.choices.some((x) => x.available)
          ? "choice"
          : "question";

const kindLabel: Record<Kind, string> = { review: "Review", plan: "Approve a plan", cost: "OK the cost", choice: "Your call", question: "A question" };

/** Reviews first (they're finished work), then plans and costs (work waiting to start), then questions. */
const kindOrder: Record<Kind, number> = { review: 0, plan: 1, cost: 2, choice: 3, question: 4 };

/**
 * What fixes setup comes first; what waits for setup or for the base
 * branch's checks comes last, since it can't pass before them.
 */
const setupOrder = (c: CardT) => (c.setup === "blocks" ? 0 : c.setup === "held" || c.setup === "base" ? 2 : 1);

/** Whether the order matters: something waits on another, or on setup. */
const ordered = (cs: CardT[]) => cs.some((c) => c.unblocks.length > 0 || c.setup === "blocks" || c.setup === "held" || c.setup === "base");

const setupLabel: Record<string, string> = { blocks: "Blocks setup", held: "Waits for setup", image: "Waits for its image", base: "Waits for the base branch" };

/** "#20 waits for it" for an item others wait for, or "". */
const holdsUp = (c: CardT) =>
  c.unblocks.length === 0 ? "" : `${c.unblocks.map((u) => shortRef(u.ref)).join(", ")} ${c.unblocks.length === 1 ? "waits" : "wait"} for it`;

/** "Do first: #20 waits for it" for an item others wait for, each a link: what waits may not be on the list. */
function HoldsUp({ c }: { c: CardT }) {
  if (c.unblocks.length === 0) return null;
  return (
    <span className="rounded-full bg-warning/15 px-2 py-0.5 text-meta font-medium text-warning">
      Do first:{" "}
      {c.unblocks.map((u, i) => (
        <span key={u.id}>
          {i > 0 && ", "}
          <Link to={`/items/${u.id}`} className="underline">
            {shortRef(u.ref)}
          </Link>
        </span>
      ))}{" "}
      {c.unblocks.length === 1 ? "waits" : "wait"} for it
    </span>
  );
}

/**
 * What waits for the base branch waits for the item fixing it: the setup
 * check filed that, so it goes first and says who waits.
 */
function withBaseFixers(cs: CardT[], readiness?: Readiness): CardT[] {
  return cs.map((c) => {
    const fixes = (readiness?.repos ?? [])
      .filter((r) => r.repo === c.repo)
      .flatMap((r) => r.work)
      .some((f) => f.item_id === c.id && f.level === "blocker" && f.area === "checks" && f.status !== "done");
    if (!fixes) return c;
    const waiting = cs.filter((o) => o.repo === c.repo && o.setup === "base" && !c.unblocks.some((u) => u.id === o.id));
    return { ...c, unblocks: [...c.unblocks, ...waiting.map((o) => ({ id: o.id, ref: o.ref }))] };
  });
}

/** How the list is ordered, naming only what's on it. */
function orderLine(cs: CardT[]): string {
  const parts = [
    cs.some((c) => kindOf(c) === "review") && "changes to review",
    cs.some((c) => c.unblocks.length > 0) && "what other work waits for",
    cs.some((c) => kindOf(c) !== "review" && c.unblocks.length === 0 && c.setup !== "held" && c.setup !== "base") && "the rest",
    cs.some((c) => c.setup === "held" || c.setup === "base") && "what waits for setup or the base branch",
  ].filter(Boolean);
  return `In this order: ${parts.join(", then ")}.`;
}

/** A decision's one-line ask, for the "next" list. */
function ask(c: CardT): string {
  switch (kindOf(c)) {
    case "review":
      return `review ${c.title}`;
    case "plan":
      return `approve a plan for ${c.title}`;
    case "cost":
      return `OK the cost of ${c.title}`;
    default:
      return `answer about ${c.title}`;
  }
}

// The stations of the line, in the order work moves along it.
const stations = [
  { key: "queue", label: "Queue", stages: ["epic", "triage", "spec", "ready"] },
  { key: "build", label: "Build", stages: ["implementing"] },
  { key: "review", label: "Review", stages: ["reviewing"] },
] as const;

export function HomeScreen() {
  const { data, error, isLoading } = useBoard({
    refetchInterval: (q) => (q.state.data?.columns.some((c) => c.cards.some((k) => k.live)) ? 10_000 : 30_000),
  });
  const repos = useOverview().data?.repos ?? [];
  const repo = useRepo(repos);
  const readiness = useReadiness().data;
  // Home leads with setup while a person's part is left; the factory's own fixes are work like any other.
  const unready = (readiness?.repos ?? []).filter((r) => r.state !== "unchecked" && yourPartLeft(r) && (!repo || r.repo === repo));
  const [later, setLater] = useState<number[]>([]);
  const cols = useMemo(() => {
    const m = new Map<string, CardT[]>();
    for (const c of data?.columns ?? []) m.set(c.stage, c.cards.filter((k) => !repo || k.repo === repo));
    return m;
  }, [data, repo]);
  if (isLoading) return <Loading what="Loading Home" />;
  if (error) return <ErrorBox error={error} />;

  const held = (cols.get("blocked") ?? []).filter((k) => k.estimate_waiting || k.plan_waiting);
  const needs = withBaseFixers([...(cols.get("awaiting_human") ?? []), ...held, ...(cols.get("parked") ?? [])], readiness).sort(
    // Reviews first (finished work), then what other work waits for: answering it lets the factory move again.
    (a, b) =>
      setupOrder(a) - setupOrder(b) ||
      Number(kindOf(b) === "review") - Number(kindOf(a) === "review") ||
      b.unblocks.length - a.unblocks.length ||
      kindOrder[kindOf(a)] - kindOrder[kindOf(b)],
  );
  // "Later" sends a decision to the back of the queue, for this visit.
  const queue = [...needs.filter((c) => !later.includes(c.id)), ...later.flatMap((id) => needs.filter((c) => c.id === id))];
  const top = queue[0];
  const moving = stations.map((s) => ({ ...s, cards: s.stages.flatMap((st) => cols.get(st) ?? []) }));
  const stuck = (cols.get("blocked") ?? []).filter((k) => !k.estimate_waiting && !k.plan_waiting);

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-6" data-testid="home">
      {unready.length > 0 && readiness ? (
        <>
          <SetupLead r={unready[0]} data={readiness} more={unready.length - 1} />
          {needs.length > 0 && (
            // Folded: setup is the one thing to do now, and these wait their turn.
            <section aria-label="Needs you">
            <details className="group rounded-card border bg-surface">
              <summary className="flex cursor-pointer items-center justify-between gap-3 p-3 text-body-sm text-muted-foreground md:px-4">
                <span>
                  After that, {needs.length === 1 ? "1 more thing needs" : `${needs.length} more things need`} you
                </span>
                <span className="text-meta group-open:hidden">Show</span>
                <span className="hidden text-meta group-open:inline">Hide</span>
              </summary>
              {ordered(needs) && <p className="border-t px-3 pt-3 text-body-sm text-muted-foreground md:px-4">{orderLine(needs)}</p>}
              <ol className="flex flex-col divide-y">
                {needs.map((c, i) => (
                  <NeedRow key={c.id} c={c} n={ordered(needs) ? i + 1 : undefined} />
                ))}
              </ol>
            </details>
            </section>
          )}
        </>
      ) : top ? (
        <section aria-label="Needs you" className="flex flex-col gap-3">
          <div className="flex items-baseline justify-between gap-3">
            <h1 className="text-display font-semibold tracking-tight">{needs.length === 1 ? "1 thing needs you" : `${needs.length} things need you`}</h1>
            {needs.length > 1 && <span className="text-body-sm text-muted-foreground">1 of {needs.length}</span>}
          </div>
          <Decision key={top.id} c={top} stacked={queue.length > 1} onLater={queue.length > 1 ? () => setLater([...later.filter((x) => x !== top.id), top.id]) : undefined} />
          {queue.length > 1 && (
            <ol className="flex flex-col gap-1 px-1 text-body-sm text-muted-foreground" aria-label="After this">
              {queue.slice(1, 3).map((c, i) => (
                <li key={c.id} className="truncate">
                  {i === 0 ? "Next" : "Then"}: <Link to={`/items/${c.id}`} className="hover:text-foreground hover:underline">{ask(c)}</Link>
                  {c.setup === "base" || c.setup === "held" ? ` (${setupLabel[c.setup].toLowerCase()})` : holdsUp(c) && ` (${holdsUp(c)})`}
                </li>
              ))}
              {queue.length > 3 && <li>and {queue.length - 3} more</li>}
            </ol>
          )}
        </section>
      ) : (
        <section aria-label="Needs you" className="flex flex-col gap-2 py-4">
          <h1 className="text-display font-semibold tracking-tight">All humming along.</h1>
          <p className="text-body text-text-2">Nothing needs you. You'll hear from the factory when a change is ready for your OK.</p>
        </section>
      )}

      <CatchUp />

      <LineStrip moving={moving} you={needs.length} stuck={stuck.length} setup={readiness?.repos.some((r) => setupActivity(r)) ? 1 : 0} />

      <Earned />

      <button
        onClick={openAsk}
        className="flex h-12 cursor-pointer items-center gap-3 rounded-card border bg-surface px-4 text-left text-body text-muted-foreground hover:border-muted-foreground/40"
      >
        <MessageCircle className="size-4 shrink-0" aria-hidden />
        <span className="flex-1">Ask, or hand over a task…</span>
      </button>
    </div>
  );
}

/** A repo whose record supports more trust, to raise on the trust screen. */
function Earned() {
  const r = useTrust().data?.repos.find((x) => x.suggest > 0);
  if (!r) return null;
  return (
    <Link
      to={`/trust?repo=${encodeURIComponent(r.repo)}`}
      aria-label="Earned more trust"
      className="flex items-start gap-2.5 rounded-card bg-live-bg px-4 py-3 text-body-sm hover:ring-1 hover:ring-live-border"
    >
      <ShieldCheck className="mt-0.5 size-4 shrink-0 text-primary" aria-hidden />
      <span className="flex-1">
        <span className="font-mono">{r.repo}</span> has earned more trust. {r.suggest_why}{" "}
        <span className="font-medium text-primary">Move it to level {r.suggest}?</span>
      </span>
    </Link>
  );
}

/** "Since you looked": what went live, until you dismiss it. The first visit starts the clock. */
function CatchUp() {
  const qc = useQueryClient();
  const { data } = useHome();
  const seen = async () => {
    const h = await postJSON("/home/seen", {});
    qc.setQueryData(["home"], h);
  };
  const first = data && data.seen_at === "";
  useEffect(() => {
    if (first) void seen();
  }, [first]);
  const live = data?.went_live ?? [];
  if (live.length === 0) return null;
  return (
    <section aria-label="Since you looked" className="flex items-start gap-2.5 rounded-card bg-success-bg px-4 py-3 text-body-sm">
      <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-success" aria-hidden />
      <p className="min-w-0 flex-1">
        Since you looked: <span className="font-semibold">{live.length === 1 ? "1 change went live" : `${live.length} changes went live`}</span>.{" "}
        {live.slice(0, 3).map((it, i) => (
          <span key={it.id}>
            {i > 0 && ", "}
            <Link to={`/items/${it.id}`} className="underline-offset-2 hover:underline">
              {it.title}
            </Link>
          </span>
        ))}
        {live.length > 3 && ` and ${live.length - 3} more`}
      </p>
      <button onClick={() => void seen()} className="cursor-pointer text-meta font-medium text-success hover:underline">
        Dismiss
      </button>
    </section>
  );
}

/** The decision on top: what it is, why it needs you, and its actions, on the lead card. */
function Decision({ c, stacked, onLater }: { c: CardT; stacked: boolean; onLater?: () => void }) {
  const kind = kindOf(c);
  const meta =
    kind === "review"
      ? [c.gates_total > 0 && `${c.gates_passed} of ${c.gates_total} checks passed`, c.model && `built by ${c.model}`, c.cost_usd > 0 && money(c.cost_usd)]
      : [c.complexity && `size ${c.complexity}`, kind === "cost" && c.estimate];
  const why = kind === "review" ? "" : c.blocked_note ? brief(c.blocked_note) : "";
  const later = onLater && (
    <Button variant="inverse-outline" onClick={onLater}>
      Later
    </Button>
  );
  const prs = c.prs.length > 1 ? c.prs : c.pr_url ? [{ url: c.pr_url, ref: `PR #${c.prs[0]?.number ?? c.pr_number}` }] : [];
  return (
    <div className={cn("lead relative", stacked && "pb-4")}>
      {stacked && (
        <>
          <div aria-hidden className="absolute inset-x-6 bottom-0 h-10 rounded-card border border-live-border bg-ink/60" />
          <div aria-hidden className="absolute inset-x-3 bottom-2 h-10 rounded-card border border-live-border bg-ink" />
        </>
      )}
      <article data-testid="decision" className="relative flex flex-col gap-3 rounded-card border border-live-border bg-ink p-5 text-ink-foreground md:p-6">
        <div className="flex flex-wrap items-center gap-x-2 text-meta font-semibold text-ink-attention">
          {kindLabel[kind]} · <span className="font-mono font-medium">{shortRef(c.ref)}</span>
        </div>
        <FoundBy source={c.self_started} className="text-ink-muted" />
        <Link to={`/items/${c.id}`} className="text-title-sm font-semibold hover:underline md:text-title">
          {c.title}
        </Link>
        {why && <p className="text-body-sm text-ink-muted">“{why}”</p>}
        {meta.some(Boolean) && <p className="text-body-sm text-ink-muted">{meta.filter(Boolean).join(" · ")}</p>}
        {c.live && <p className="text-body-sm text-ink-muted">{c.activity || "The factory is still working on it"} · {duration(c.live_since)}</p>}
        <WaitsFor items={c.waits_for} plan={c.plan_waiting} className="text-meta text-ink-muted" />
        {holdsUp(c) && <p className="text-meta text-ink-muted">Do this first: {holdsUp(c)}.</p>}
        {c.setup === "base" && c.setup_note && (
          <p className="text-meta text-ink-muted">
            <Inline text={c.setup_note} />
          </p>
        )}
        {kind === "choice" ? (
          <div className="mt-1">
            <Choices jobId={c.choice_job_id!} itemId={c.id} choices={c.choices} compact inverse>
              {later}
            </Choices>
          </div>
        ) : (
          <div className="mt-1 flex flex-wrap gap-2">
            {kind === "review" &&
              prs.map((p, i) => (
                <Button key={p.url} variant={i === 0 ? "inverse" : "inverse-outline"} asChild>
                  <a href={p.url} target="_blank" rel="noreferrer">
                    {prs.length > 1 ? `${i + 1}. Review ${p.ref}` : "Review the change"} <ExternalLink aria-hidden />
                  </a>
                </Button>
              ))}
            {kind === "review" && (
              <Button variant="inverse-outline" asChild>
                <Link to={`/items/${c.id}/diff`}>See the diff</Link>
              </Button>
            )}
            {(kind === "plan" || kind === "cost") && (
              <Button variant="inverse" asChild>
                <Link to={`/items/${c.id}`}>{kind === "plan" ? "Read the plan" : "See the estimate"}</Link>
              </Button>
            )}
            {kind === "question" && c.issue_url && (
              <Button variant="inverse" asChild>
                <a href={c.issue_url} target="_blank" rel="noreferrer">
                  Answer on the issue <ExternalLink aria-hidden />
                </a>
              </Button>
            )}
            {later}
          </div>
        )}
      </article>
    </div>
  );
}

/**
 * Home's lead while a repo isn't set up: its one next step, as the main
 * card. Every job on it would fail until then, so nothing else leads.
 */
function SetupLead({ r, data, more }: { r: RepoReadiness; data: Readiness; more: number }) {
  const { busy, msg, run } = useReadinessRun(r);
  const next = nextStep(r, data);
  const title = next.title.replace(/^(Ready\. )?Next: |^Working on it: /, "");
  return (
    <section aria-label="Setup" className="flex flex-col gap-3">
      <h1 className="text-display font-semibold tracking-tight">Finish setting up {repoName(r.repo) || r.repo}</h1>
      <article data-testid="setup-lead" className="lead flex flex-col gap-3 rounded-card border border-live-border bg-ink p-5 text-ink-foreground md:p-6">
        <div className="text-meta font-semibold text-ink-attention">
          {next.running ? "Working on it now" : next.human ? "Do this first: nothing new starts until setup is done" : "Nothing new starts until setup is done"}
        </div>
        <p className="flex items-center gap-2.5 text-title-sm font-semibold md:text-title">
          {next.running && <span className="pulse inline-block size-2.5 shrink-0 rounded-full bg-ink-foreground" aria-hidden />}
          {title.charAt(0).toUpperCase() + title.slice(1)}
        </p>
        <p className="text-body-sm text-ink-muted">{next.body}</p>
        {next.running && (
          <div className="flex flex-col gap-1 text-body-sm text-ink-muted">
            <p>Started {duration(setupActivity(r)?.since ?? r.checking_since)} ago.</p>
            <RunProgress r={r} last />
          </div>
        )}
        {/* One button; the details are a link, so nothing competes with it. */}
        <div className="mt-1 flex flex-wrap items-center gap-4">
          <NextActions next={next} busy={busy} run={run} inverse />
          <Link to="/setup" className="text-body-sm text-ink-muted underline hover:text-ink-foreground">
            {r.state === "incomplete" ? "See the error" : "See every step"}
          </Link>
        </div>
        {msg && <p className="text-body-sm text-ink-muted">{msg.text}</p>}
        {!msg && !r.checking && r.check_error && <p className="text-body-sm text-ink-muted">{r.check_error}</p>}
      </article>
      {more > 0 && (
        <p className="px-1 text-body-sm text-muted-foreground">
          {more === 1 ? "1 more repo isn't" : `${more} more repos aren't`} set up yet.{" "}
          <Link to="/setup" className="font-medium text-primary hover:underline">
            See setup
          </Link>
        </p>
      )}
    </section>
  );
}

/** One decision in the list under the setup lead: what it asks, and how it stands to setup. */
function NeedRow({ c, n }: { c: CardT; n?: number }) {
  return (
    <li className="flex flex-col gap-1 p-3 md:px-4">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        {n != null && <span className="text-meta font-semibold tabular-nums">{n}.</span>}
        <span className="text-meta font-semibold text-muted-foreground">{kindLabel[kindOf(c)]}</span>
        <span className="font-mono text-meta text-muted-foreground">{shortRef(c.ref)}</span>
        {c.setup && (
          <span
            className={cn(
              "rounded-full px-2 py-0.5 text-meta font-medium",
              c.setup === "blocks" ? "bg-warning/15 text-warning" : "bg-muted text-muted-foreground",
            )}
          >
            {setupLabel[c.setup]}
          </span>
        )}
        <HoldsUp c={c} />
      </div>
      <Link to={`/items/${c.id}`} className="text-body font-medium hover:underline">
        {c.title}
      </Link>
      {(c.setup === "blocks" || c.setup === "image" || c.setup === "base") && c.setup_note ? (
        <p className="text-body-sm text-text-2">
          <Inline text={c.setup_note} />
        </p>
      ) : (
        // Why it stopped: "A question" alone doesn't say a review couldn't run.
        c.blocked_note && kindOf(c) !== "review" && <p className="text-body-sm text-text-2">{brief(c.blocked_note, 160)}</p>
      )}
    </li>
  );
}

/** The line at a glance: how many jobs are at each station, you being the last. */
function LineStrip({ moving, you, stuck, setup }: { moving: { key: string; label: string; cards: CardT[] }[]; you: number; stuck: number; setup: number }) {
  // A setup run (a check, a lint baseline) is work too.
  const working = moving.reduce((n, s) => n + s.cards.filter((c) => c.live).length, 0) + setup;
  return (
    <section aria-label="The line" className="flex flex-col gap-4 rounded-card border bg-surface p-4 md:p-5">
      <div className="flex items-baseline justify-between gap-3">
        <h2 className="text-body-sm font-semibold">On the line</h2>
        <span className="text-meta text-muted-foreground">
          {working === 0 ? "Nothing running" : `${working} working`}
          {stuck > 0 ? `, ${stuck} stuck` : you > 0 ? `, ${you} waiting on you` : ", nothing stuck"}
        </span>
      </div>
      <ol className="grid grid-cols-4 gap-2">
        {[...moving.map((s) => ({ key: s.key, label: s.label, n: s.cards.length, cards: s.cards })), { key: "you", label: "You", n: you, cards: [] as CardT[] }].map((s) => (
          <li key={s.key} className="flex min-w-0 flex-col gap-2" aria-label={`${s.label}: ${s.n}`}>
            <span className={cn("h-1 rounded-full", s.key === "you" ? (s.n > 0 ? "bg-ink" : "bg-border") : s.n > 0 ? "bg-primary" : "bg-border")} />
            <span className={cn("text-meta", s.key === "you" ? "font-semibold" : "text-muted-foreground")}>
              {s.label} <span className="font-mono">{s.n}</span>
            </span>
            <ul className="hidden flex-col gap-1.5 md:flex">
              {s.cards.slice(0, 3).map((c) => (
                <li key={c.id} className="min-w-0">
                  <Link to={`/items/${c.id}`} className="line-clamp-2 text-body-sm hover:underline">
                    {c.live && <span className="pulse mr-1.5 inline-block size-1.5 rounded-full bg-primary align-middle" aria-hidden />}
                    {c.title}
                  </Link>
                </li>
              ))}
              {s.cards.length > 3 && <li className="text-meta text-muted-foreground">and {s.cards.length - 3} more</li>}
            </ul>
          </li>
        ))}
      </ol>
      <Link to="/line" className="text-body-sm font-medium text-primary hover:underline">
        See the whole line
      </Link>
    </section>
  );
}
