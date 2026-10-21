import { useEffect, useState } from "react";
import { Link, useLocation } from "react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, CircleDashed, ExternalLink, Loader2, UserRound, XCircle } from "lucide-react";
import { act, getJSON } from "@/api/client";
import type { components } from "@/api/schema";
import { Ago, ErrorBox, Inline, Loading, PageTitle } from "@/components/common";
import type { Notice } from "@/components/Notices";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ago, cn, repoName, shortSha } from "@/lib/utils";

type Readiness = components["schemas"]["Readiness"];
type RepoReadiness = components["schemas"]["RepoReadiness"];
type Step = components["schemas"]["ReadinessStep"];
type FindingT = components["schemas"]["ReadinessFinding"];

/** Every repo's readiness. While a check runs, it polls until it's done. */
export const useReadiness = () =>
  useQuery({
    queryKey: ["readiness"],
    queryFn: () => getJSON<Readiness>("/readiness"),
    refetchInterval: (q) => (q.state.data?.repos.some((r) => r.checking) ? 3_000 : 30_000),
  });

/** A finding's detail: paragraphs, with command output in fenced blocks shown as is. */
function Detail({ text }: { text: string }) {
  const blocks: { code: boolean; text: string }[] = [];
  let fence = "";
  let cur: string[] = [];
  const flush = (code: boolean) => {
    const t = cur.join("\n").trim();
    if (t) blocks.push({ code, text: t });
    cur = [];
  };
  for (const line of text.split("\n")) {
    const m = /^\s*(`{3,}|~{3,})/.exec(line);
    if (!fence && m) {
      flush(false);
      fence = m[1];
    } else if (fence && line.trim() === fence) {
      flush(true);
      fence = "";
    } else {
      cur.push(line);
    }
  }
  flush(fence !== "");
  return (
    <div className="flex flex-col gap-2 text-meta leading-relaxed text-text-2">
      {blocks.map((b, i) =>
        b.code ? (
          <pre key={i} className="max-h-60 overflow-auto rounded-md bg-muted p-2.5 font-mono text-xs whitespace-pre-wrap">
            {b.text}
          </pre>
        ) : (
          <p key={i}>
            <Inline text={b.text} />
          </p>
        ),
      )}
    </div>
  );
}

const statusText: Record<string, { label: string; variant: "default" | "secondary" | "success" | "warning" | "destructive" | "outline" }> = {
  none: { label: "Yours to fix", variant: "outline" },
  not_filed: { label: "No issue yet", variant: "secondary" },
  not_queued: { label: "Filed, not queued", variant: "secondary" },
  closed: { label: "Issue closed", variant: "secondary" },
  waiting: { label: "Waiting its turn", variant: "secondary" },
  building: { label: "Being built", variant: "default" },
  needs_you: { label: "Needs you", variant: "warning" },
  split: { label: "Split", variant: "default" },
  done: { label: "Done", variant: "success" },
};

/** One blocker or gap, with its issue and the factory's work on it. */
/** The next step, for the finding it's about to show on its row. */
type Placed = { next: Next; busy: boolean; run: (path: string, body: Record<string, unknown>) => void; id: string };

function Finding({
  f,
  label,
  repo,
  placed,
}: {
  f: FindingT;
  label: string;
  repo: RepoReadiness;
  placed?: Placed;
}) {
  const [open, setOpen] = useState(false);
  const here = placed && isAt(placed.next, f) ? placed : undefined;
  const s =
    f.status === "split"
      ? { label: `Split: ${f.split_done} of ${f.split_total} done`, variant: "default" as const }
      : (statusText[f.status] ?? { label: f.status, variant: "secondary" as const });
  const hint =
    f.status === "none"
      ? f.level === "blocker"
        ? "The factory can't change this one: its fix is in factory.yaml or the factory's setup."
        : "The factory doesn't file an issue for this one: the details say what to add."
      : f.status === "not_filed"
        ? "No issue yet: the factory files one on its next check."
        : f.status === "not_queued"
          ? f.level === "blocker"
            ? `Filed without the ${label} label: the factory adds it on its next check.`
            : "Filed for later: the factory starts on it once its setup fixes are merged."
          : f.status === "split"
            ? `Split into ${f.split_total} smaller issues: See the work for each one's progress. ${f.level === "blocker" ? "Once all are merged, Check again to confirm it." : ""}`
            : f.status === "done" && f.level === "blocker"
            ? "The factory's fix is merged. Check again to confirm it."
            : "";
  return (
    <li data-testid="readiness-finding" className="flex flex-col gap-1.5 py-2.5">
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
        <span className="min-w-0 flex-1 text-sm font-medium break-words">
          <Inline text={f.title} />
        </span>
        {/* The next step on this row says all there is to do; its status and links would only compete. */}
        {!here && f.level !== "good" && (
          <Badge variant={s.variant}>{s.label}</Badge>
        )}
        {!here && f.issue > 0 && (
          <a href={f.issue_url} target="_blank" rel="noreferrer" className="flex items-center gap-1 font-mono text-xs text-primary hover:underline">
            #{f.issue} <ExternalLink className="size-3" aria-hidden />
          </a>
        )}
        {!here && f.item_id != null && (
          <Link to={`/items/${f.item_id}`} className="text-xs text-primary hover:underline">
            See the work
          </Link>
        )}
      </div>
      {here ? (
        <NextBox next={here.next} r={repo} busy={here.busy} run={here.run} id={here.id} />
      ) : (
        hint && <p className="text-xs text-muted-foreground">{hint}</p>
      )}
      {f.detail && (
        <div>
          <button onClick={() => setOpen(!open)} aria-expanded={open} className="cursor-pointer text-xs text-muted-foreground hover:text-foreground">
            {open ? "Hide details ▴" : "Details ▾"}
          </button>
          {open && (
            <div className="mt-1.5">
              <Detail text={f.detail} />
            </div>
          )}
        </div>
      )}
    </li>
  );
}

function StepIcon({ state }: { state: string }) {
  if (state === "done") return <CheckCircle2 aria-label="Done" className="size-5 shrink-0 text-success" />;
  if (state === "failing") return <XCircle aria-label="Not done" className="size-5 shrink-0 text-destructive" />;
  return <CircleDashed aria-label="Not checked yet" className="size-5 shrink-0 text-muted-foreground" />;
}

function StepRow({
  step,
  n,
  label,
  repo,
  placed,
}: {
  step: Step;
  n: number;
  label: string;
  repo: RepoReadiness;
  placed: Placed;
}) {
  // A step that's done is a line; what it checked opens from it.
  if (step.state === "done") {
    return (
      <li data-testid="setup-step" className="py-2">
        <details className="group">
          <summary className="flex cursor-pointer list-none items-center gap-3 text-sm">
            <StepIcon state={step.state} />
            <span className="min-w-0 flex-1">
              {n}. {step.title}
            </span>
            <span className="text-xs text-muted-foreground group-open:hidden">Details ▾</span>
          </summary>
          <div className="mt-1 flex flex-col gap-1 pl-8">
            <p className="text-meta text-text-2">{step.why}</p>
            {step.passed && <p className="text-meta text-success">{step.passed}</p>}
          </div>
        </details>
      </li>
    );
  }
  return (
    <li data-testid="setup-step" className="flex gap-3 py-3">
      <StepIcon state={step.state} />
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        <h4 className="text-body font-semibold">
          {n}. {step.title}
        </h4>
        <p className="text-meta text-text-2">{step.why}</p>
        {isAtStep(placed.next, step) && <NextBox next={placed.next} r={repo} busy={placed.busy} run={placed.run} id={placed.id} />}
        {step.state === "unchecked" && <p className="text-meta text-muted-foreground">Not checked yet: the check stopped before this step.</p>}
        {step.blockers.length > 0 && (
          <ul className="divide-y">
            {step.blockers.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={repo} placed={placed} />
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}

/** One button of a next step: it runs a readiness action, opens a page, or opens a link. */
type NextAction = { label: string; primary?: boolean; run?: [string, Record<string, unknown>]; to?: string; href?: string };

/** A repo's next step: the one thing to do now, and its buttons. */
export type Next = {
  tone: "info" | "success" | "attention";
  title: string;
  body: string;
  actions: NextAction[];
  running?: boolean;
  /** What it's about, by finding key, "area:name" or "step:key": Setup shows it there. */
  at?: string;
  /** Only a person can do it: the factory waits on them. */
  human?: boolean;
};

/** Whether next step n is about finding f. */
const isAt = (n: Next, f: FindingT) => !!n.at && (n.at.startsWith("area:") ? f.area === n.at.slice(5) : f.key === n.at);

/** Whether next step n is about a whole setup step: a check running again, say. */
const isAtStep = (n: Next, s: Step) => n.at === `step:${s.key}`;

/** What a pull request is called on the repo's host: a GitLab repo is named host/path. */
export const prWord = (r: RepoReadiness) => (r.repo.split("/").length > 2 ? "merge request" : "pull request");

/** Marks what only a person can do. */
export function NeedsYou({ className }: { className?: string }) {
  return (
    <Badge variant="warning" className={className}>
      <UserRound className="size-3" aria-hidden />
      Needs you
    </Badge>
  );
}

/** The one thing to do next on a repo, most pressing first: the person's part (the setup steps), then the factory's (its work). */
export function nextStep(r: RepoReadiness, data: Readiness): Next {
  const label = data.intake_label;
  const yours = r.steps.flatMap((s) => s.blockers);
  const settled = (f: FindingT) => f.status === "done" || (f.status === "split" && f.split_total > 0 && f.split_done === f.split_total);
  const merged = r.work.filter(settled);
  const check: NextAction[] = data.can_check && !r.checking ? [{ label: r.state === "unchecked" ? "Check now" : "Check again", primary: true, run: ["/readiness/check", {}] }] : [];
  if (r.checking) {
    const failing = r.steps.find((st) => st.state === "failing");
    return {
      tone: "info",
      title: `Working on it: checking ${repoName(r.repo) || r.repo}`,
      body: `It installs ${r.repo} on a fresh copy of ${r.branch} and runs its checks, then files what a change can fix for the factory. It takes a few minutes; this updates by itself.`,
      actions: [],
      running: true,
      // A check is about what's failing, which it looks at again.
      at: failing ? `step:${failing.key}` : undefined,
    };
  }
  if (r.state === "unchecked") {
    return {
      tone: "info",
      title: `Next: check ${repoName(r.repo) || r.repo}`,
      body: `The check clones ${r.repo}, installs it and runs its checks, to see what the factory needs first. It takes a few minutes and changes nothing in the repo.`,
      actions: check,
    };
  }
  if (r.state === "incomplete") {
    return {
      tone: "attention",
      title: "Next: fix what stopped the check, then check again",
      body: "The check stopped early, so this says nothing about the repo past that point. It's usually the factory's setup or a command in factory.yaml, not the repo's code.",
      actions: check,
      human: true,
    };
  }
  const access = yours.find((f) => f.area === "access");
  if (access) {
    // A GitLab repo is named host/path; its tokens are in the user's settings there.
    const host = r.repo.split("/").length > 2 ? r.repo.split("/")[0] : "";
    return {
      tone: "attention",
      title: "Next: give the factory's token the permissions it needs",
      body: `${access.title.replace(/^The factory's token/, "Its token")}. Every change ends with the factory opening a pull request, so until it can, every job would fail at its end and nothing else starts. Details says what's missing; once the new token is in place and the factory restarted, check again.`,
      actions: [
        ...(host ? [{ label: "Open your access tokens", primary: true, href: `https://${host}/-/user_settings/personal_access_tokens` }] : []),
        ...check.map((a) => ({ ...a, primary: !host })),
      ],
      at: "area:access",
      human: true,
    };
  }
  const files = yours.find((f) => f.area === "files");
  if (files) {
    return {
      tone: "attention",
      title: "Next: let the factory copy the files git doesn't track",
      body: `${files.title.replace(/`/g, "")}. A fresh copy of the repo lacks what its .worktreeinclude lists, and no change can add it, so every job would fail. Point factory.yaml's files setting at a checkout of the repo on this machine (Details says exactly what to add), restart the factory, then check again.`,
      actions: check,
      at: "area:files",
      human: true,
    };
  }
  if (yours.length > 0) {
    return {
      tone: "attention",
      title: `Next: ${yours[0].title.replace(/`/g, "")}`,
      body: "The factory can't change this one: the fix is in factory.yaml or the factory's setup. Details says what it saw; fix it, then check again.",
      actions: check,
      at: yours[0].key ? yours[0].key : `area:${yours[0].area}`,
      human: true,
    };
  }
  const waiting = r.work.find((f) => f.status === "needs_you");
  if (waiting) {
    const review = waiting.item_stage === "awaiting_human";
    return {
      tone: "attention",
      title: review ? `Next: review the factory's fix: ${waiting.title.replace(/`/g, "")}` : `Next: the factory needs your call on ${waiting.title.replace(/`/g, "")}`,
      body: review
        ? `Its pull request is ready. Merge it, and the factory checks ${r.branch} again by itself; new tasks start once its fixes are in.`
        : "It stopped and is waiting for you. See the work to answer it.",
      actions: waiting.item_id != null ? [{ label: review ? "Review it" : "See the work", primary: true, to: `/items/${waiting.item_id}` }] : [],
      at: waiting.key || undefined,
      human: true,
    };
  }
  if (r.state === "not_ready" && merged.length > 0) {
    return {
      tone: "info",
      title: "Next: check again to confirm the fix",
      body: `The factory's fix for ${merged.length === 1 ? "a blocker is" : `${merged.length} blockers are`} merged, but only a fresh check proves it. The factory checks again by itself within a few minutes of ${r.branch} changing.`,
      actions: check,
      at: merged.length === 1 && merged[0].key ? merged[0].key : undefined,
    };
  }
  if (r.state === "not_ready") {
    const n = r.work.length;
    return {
      tone: "info",
      title: `Your part is done. The factory is fixing ${n === 1 ? "one thing" : `${n} things`} first`,
      body: `What fails on ${r.branch} today would fail every change, so new tasks on ${repoName(r.repo) || r.repo} wait until ${n === 1 ? "this fix is" : "these fixes are"} merged. You'll review each as a pull request, like any of the factory's work.`,
      actions: [{ label: "See them on the line", to: "/line" }],
    };
  }
  return {
    tone: "success",
    title: `Ready: add the ${label} label to issues to start`,
    body: "Every check passes, so the factory's changes can be proved. It picks up each issue with that label, and the recommended work below is queued for it too.",
    actions: [],
  };
}

/** Runs a repo's readiness actions, saying what each started. */
export function useReadinessRun(r: RepoReadiness) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  // What a run started here said goes once it's done: its result shows instead.
  useEffect(() => {
    if (!r.checking) setMsg(null);
  }, [r.checking, r.result, r.check_error]);
  const run = async (path: string, body: Record<string, unknown>) => {
    setBusy(true);
    const res = await act(path, { repo: r.repo, ...body });
    setBusy(false);
    // What started shows on the page itself; only a refusal needs saying.
    setMsg(res.ok ? null : { ok: false, text: res.message });
    void qc.invalidateQueries({ queryKey: ["readiness"] });
  };
  return { busy, msg, run };
}

/** A next step's buttons; inverse on an ink card. */
export function NextActions({ next, busy, run, inverse }: { next: Next; busy: boolean; run: (path: string, body: Record<string, unknown>) => void; inverse?: boolean }) {
  if (next.actions.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-2">
      {next.actions.map((a) => {
        const variant = inverse ? (a.primary ? "inverse" : "inverse-outline") : a.primary ? "default" : "outline";
        if (a.href)
          return (
            <Button key={a.label} size="sm" variant={variant} asChild>
              <a href={a.href} target="_blank" rel="noreferrer">
                {a.label} <ExternalLink className="size-3.5" aria-hidden />
              </a>
            </Button>
          );
        if (a.to)
          return (
            <Button key={a.label} size="sm" variant={variant} asChild>
              <Link to={a.to}>{a.label}</Link>
            </Button>
          );
        return (
          <Button key={a.label} size="sm" variant={variant} disabled={busy} onClick={() => a.run && run(...a.run)}>
            {a.label}
          </Button>
        );
      })}
    </div>
  );
}

/** A setup run's steps so far: each command's outcome and time, and what it's doing now. */
export function RunProgress({ r, last }: { r: RepoReadiness; last?: boolean }) {
  // A command that finished says so; its "running" line goes.
  const steps = r.progress.filter((p, i) => !p.text.startsWith("running ") || i === r.progress.length - 1);
  if (steps.length === 0) return null;
  return (
    <ol aria-label="What it has done" className="flex flex-col gap-1 text-meta">
      {(last ? steps.slice(-1) : steps).map((p, i, all) => {
        const now = i === all.length - 1 && r.checking;
        const bad = p.text.startsWith("failed ") || p.text.startsWith("couldn't ");
        return (
          <li key={i} className="flex items-start gap-2">
            {now ? (
              <Loader2 className="mt-0.5 size-3.5 shrink-0 animate-spin text-primary" aria-label="Doing now" />
            ) : bad ? (
              <XCircle className="mt-0.5 size-3.5 shrink-0 text-destructive" aria-label="Failed" />
            ) : (
              <CheckCircle2 className="mt-0.5 size-3.5 shrink-0 text-success" aria-label="Done" />
            )}
            <span className="min-w-0 break-words">
              <Inline text={p.text.charAt(0).toUpperCase() + p.text.slice(1)} />
              {now && (
                <span className="text-muted-foreground">
                  {" "}
                  · since <Ago ts={p.at} />
                </span>
              )}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

function NextBox({ next, r, busy, run, id }: { next: Next; r: RepoReadiness; busy: boolean; run: (path: string, body: Record<string, unknown>) => void; id?: string }) {
  return (
    <div
      id={id}
      data-testid="next-step"
      className={cn(
        "flex flex-col gap-2 rounded-control border p-3 text-sm",
        next.tone === "success" ? "border-success/40 bg-success/10" : next.tone === "attention" ? "border-warning/40 bg-warning/10" : "border-primary/40 bg-primary/5",
      )}
    >
      <p className={cn("flex scroll-mt-24 flex-wrap items-center gap-2 font-semibold", next.tone === "success" && "text-success")}>
        {next.running && <Loader2 className="size-4 shrink-0 animate-spin text-primary" aria-hidden />}
        {next.title}
        {next.human && <NeedsYou />}
      </p>
      {next.running && (
        <p className="text-meta text-muted-foreground">
          Started <Ago ts={r.checking_since} />
          {r.checking_why && `. ${r.checking_why.replace(/^Started by itself: /, "The factory started it: ")}`}
        </p>
      )}
      <p className="text-text-2">{next.body}</p>
      {next.running && <RunProgress r={r} />}
      <NextActions next={next} busy={busy} run={run} />
    </div>
  );
}

const stateBadge: Record<string, [string, "success" | "warning" | "destructive" | "secondary"]> = {
  ready: ["Ready", "success"],
  not_ready: ["Not ready yet", "warning"],
  incomplete: ["The check couldn't finish", "destructive"],
  unchecked: ["Not checked yet", "secondary"],
};

function RepoCard({ r, data, many }: { r: RepoReadiness; data: Readiness; many: boolean }) {
  const { busy, msg, run } = useReadinessRun(r);
  const check = () => run("/readiness/check", {});
  const next = nextStep(r, data);
  // A next step about one finding sits on that finding's row; the top
  // only points there, so the action is next to what it fixes.
  const stepAt = r.steps.findIndex((st) => isAtStep(next, st) || st.blockers.some((f) => isAt(next, f)));
  const inWork = r.work.some((f) => isAt(next, f));
  const placed = stepAt >= 0 || inWork || r.extras.some((f) => isAt(next, f));
  const boxId = `next-${r.repo.replace(/[^a-z0-9]+/gi, "-")}`;
  const placedNext = { next, busy, run, id: boxId };
  const [badge, variant] = stateBadge[r.state] ?? [r.state, "secondary"];
  const label = data.intake_label;
  const gaps = r.extras.filter((f) => f.level === "gap");
  const tips = r.extras.filter((f) => f.level === "tip");
  const good = r.extras.filter((f) => f.level === "good");
  return (
    <section aria-label={`Setup of ${r.repo}`} data-testid="setup-repo" className="flex flex-col gap-4 rounded-card border bg-card p-4 md:p-5">
      <div className="flex flex-wrap items-start gap-3">
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <h2 className="flex flex-wrap items-center gap-2.5 text-title-sm font-semibold">
            <span className="font-mono break-all">{many ? r.repo : repoName(r.repo) || r.repo}</span>
            <Badge variant={variant}>{badge}</Badge>
          </h2>
          {r.state !== "unchecked" && (
            <p className="text-xs text-muted-foreground">
              Checked {ago(r.checked_at)} on {r.branch}
              {r.sha && ` at ${shortSha(r.sha)}`}
            </p>
          )}
        </div>
        <div className="flex flex-col items-end gap-1.5">
          {r.checking ? (
            <span role="status" className="flex items-center gap-2 text-sm text-primary-soft">
              <Loader2 className="size-4 animate-spin" aria-hidden /> Checking now, started{" "}
              <Ago ts={r.checking_since} />
            </span>
          ) : data.can_check ? (
            <Button size="sm" variant="outline" disabled={busy} onClick={check}>
              {r.state === "unchecked" ? "Check now" : "Check again"}
            </Button>
          ) : null}
        </div>
      </div>

      {r.total > 0 && (
        <div className="flex flex-col gap-1.5">
          <p className="text-sm font-medium">
            {r.done} of {r.total} setup steps done
          </p>
          <div
            className="h-1.5 rounded-full bg-border"
            role="progressbar"
            aria-label={`Setup of ${r.repo}`}
            aria-valuenow={r.done}
            aria-valuemin={0}
            aria-valuemax={r.total}
          >
            <div className={cn("h-1.5 rounded-full", r.done === r.total ? "bg-success-fill" : "bg-warning-fill")} style={{ width: `${(r.done / r.total) * 100}%` }} />
          </div>
        </div>
      )}

      {placed ? (
        <button
          data-testid="next-pointer"
          onClick={() => document.getElementById(boxId)?.scrollIntoView({ behavior: "smooth", block: "center" })}
          className="flex w-fit cursor-pointer flex-wrap items-center gap-x-2 gap-y-1 text-left text-xs text-muted-foreground hover:text-foreground"
        >
          {next.running && <Loader2 className="size-3.5 shrink-0 animate-spin text-primary" aria-hidden />}
          <span className="font-medium text-foreground">{next.title}</span>
          {next.human && <NeedsYou />}
          <span className="text-muted-foreground">{stepAt >= 0 ? `in step ${stepAt + 1} ↓` : inWork ? "in the factory's work ↓" : "below ↓"}</span>
        </button>
      ) : (
        <NextBox next={next} r={r} busy={busy} run={run} />
      )}
      {!data.can_check && !r.checking && data.check_note && <p className="text-xs text-muted-foreground">{data.check_note}</p>}
      {r.result && !r.checking && (
        <p role="status" className="text-sm text-success">
          {r.result}
        </p>
      )}
      {r.check_error && (
        <p role="alert" className="text-sm text-destructive">
          {r.check_error}
        </p>
      )}
      {!r.checking && r.progress.length > 0 && (
        <details>
          <summary className="cursor-pointer text-xs text-muted-foreground hover:text-foreground">What the last run did</summary>
          <div className="mt-1.5">
            <RunProgress r={r} />
          </div>
        </details>
      )}
      {msg && (
        <p role="status" className={cn("text-sm", msg.ok ? "text-success" : "text-destructive")}>
          {msg.text}
        </p>
      )}

      {r.steps.length > 0 && (
        <ol aria-label="Setup steps" className="divide-y border-y">
          {r.steps.map((s, i) => (
            <StepRow key={s.key} step={s} n={i + 1} label={label} repo={r} placed={placedNext} />
          ))}
        </ol>
      )}

      {r.work.length > 0 && (
        <section aria-label="The factory's work" className="flex flex-col gap-1">
          <h3 className="text-body font-semibold">The factory is fixing these first ({r.work.length})</h3>
          <p className="text-meta text-text-2">
            Each fails on {r.branch} today, so it would fail every change. The factory fixes them as issues, like any of its work, and you review each as a pull
            request. New tasks on {repoName(r.repo) || r.repo} wait until they're merged.
          </p>
          <ul className="divide-y">
            {r.work.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={r} placed={placedNext} />
            ))}
          </ul>
        </section>
      )}
      {gaps.length > 0 && (
        <section aria-label="Recommended" className="flex flex-col gap-1">
          <h3 className="text-body font-semibold">Recommended ({gaps.length})</h3>
          <p className="text-meta text-text-2">
            Not needed to start, but each lets the factory prove more of what it changes, so fewer of its mistakes reach you.{" "}
            {r.state === "ready" ? "The factory works on them like any issue." : "The factory files them, and starts on them once its fixes above are merged."}
          </p>
          <ul className="divide-y">
            {gaps.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={r} placed={placedNext} />
            ))}
          </ul>
        </section>
      )}
      {tips.length > 0 && (
        <details>
          <summary className="cursor-pointer text-sm font-medium">Nice to have ({tips.length})</summary>
          <ul className="divide-y">
            {tips.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={r} placed={placedNext} />
            ))}
          </ul>
        </details>
      )}
      {good.length > 0 && (
        <details>
          <summary className="cursor-pointer text-sm font-medium">Already in place ({good.length})</summary>
          <ul className="mt-1 flex flex-col gap-1 text-sm">
            {good.map((f) => (
              <li key={f.title} className="flex items-center gap-2">
                <CheckCircle2 className="size-4 shrink-0 text-success" aria-hidden />
                <Inline text={f.title} />
              </li>
            ))}
          </ul>
        </details>
      )}
    </section>
  );
}

export function SetupScreen() {
  const { data, error, isLoading } = useReadiness();
  if (isLoading) return <Loading what="Reading the readiness checks" />;
  if (error) return <ErrorBox error={error} />;
  if (!data) return null;
  const checked = data.repos.filter((r) => r.state !== "unchecked");
  const ready = checked.filter((r) => r.state === "ready").length;
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-5" data-testid="setup">
      <div>
        <PageTitle>Setup</PageTitle>
        <p className="-mt-2 text-sm text-text-2">
          Before the factory can work on real tasks, each repo's install and checks must pass on its base branch, and its tests must give the same answer
          every run. This is where each repo stands.
          {checked.length > 0 && ` ${ready} of ${data.repos.length} ${data.repos.length === 1 ? "repo is" : "repos are"} ready.`}
        </p>
        <p className="mt-1 text-sm text-text-2">
          Repos, access and keys are on the{" "}
          <Link to="/get-started" className="text-primary hover:underline">
            get-started page
          </Link>
          .
        </p>
      </div>
      {data.repos.map((r) => (
        <RepoCard key={r.repo} r={r} data={data} many={data.repos.length > 1} />
      ))}
    </div>
  );
}


/**
 * The setup notice: while a repo isn't ready, or once all are and nothing
 * else has started, or while a check runs (a job with no item, so the
 * line doesn't show it as one). None on the setup screen itself.
 */
export function useSetupNotice(): Notice | null {
  const { data } = useReadiness();
  const path = useLocation().pathname;
  // Home leads with setup itself while a person's part is left.
  const here = path === "/setup" || (path === "/" && !!data?.repos.some(yourPartLeft));
  if (!data || here) return null;
  const running = data.repos.find((r) => r.checking);
  const doing = running && (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <Loader2 className="size-3.5 animate-spin text-primary" aria-hidden />
      Checking {repoName(running.repo) || running.repo}, started <Ago ts={running.checking_since} />.
      <Link to="/setup" className="font-medium underline underline-offset-2">
        Watch it
      </Link>
    </span>
  );
  if (!data.banner) {
    return doing ? { key: "setup", label: "Setup", tone: "info", body: doing } : null;
  }
  if (data.banner === "ready") {
    return {
      key: "setup",
      label: "Setup",
      tone: "success",
      body: (
        <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
          <span className="font-medium text-success">Ready: add the {data.intake_label} label to issues to start.</span>
          <Link to="/setup" className="underline underline-offset-2">
            See the setup
          </Link>
        </span>
      ),
    };
  }
  const behind = data.repos.filter((r) => r.state === "not_ready" || r.state === "incomplete");
  const many = data.repos.length > 1;
  // Only the factory's fixes left: new tasks wait for them, and that's all.
  if (!behind.some(yourPartLeft)) {
    const n = behind.reduce((k, r) => k + r.work.filter((f) => f.status !== "needs_you").length, 0);
    // Fixes waiting on you lead Home already; elsewhere they're said as yours, not the factory's.
    if (n === 0) {
      const w = behind.reduce((k, r) => k + r.work.length, 0);
      if (path === "/" || w === 0) return doing ? { key: "setup", label: "Setup", tone: "info", body: doing } : null;
      return {
        key: "setup",
        label: "Setup",
        tone: "attention",
        body: (
          <span className="flex flex-col gap-1">
            <span className="min-w-0 break-words">
              {w === 1 ? "A fix" : `${w} fixes`} for what fails on{" "}
              {behind.length > 1 ? "the repos' base branches" : `${many ? repoName(behind[0].repo) + "'s " : ""}${behind[0].branch}`}{" "}
              {w === 1 ? "waits" : "wait"} on you; new tasks wait until {w === 1 ? "it's" : "they're"} merged.{" "}
              <Link to="/" className="font-medium underline underline-offset-2">
                See {w === 1 ? "it" : "them"}
              </Link>
            </span>
            {doing}
          </span>
        ),
      };
    }
    return {
      key: "setup",
      label: "Setup",
      tone: "info",
      body: (
        <span className="flex flex-col gap-1">
          <span className="min-w-0 break-words">
            The factory is fixing {n === 1 ? "one thing" : `${n} things`} that fail on{" "}
            {behind.length > 1 ? "the repos' base branches" : `${many ? repoName(behind[0].repo) + "'s " : ""}${behind[0].branch}`} today; new tasks wait until{" "}
            {n === 1 ? "it's" : "they're"} merged.{" "}
            <Link to="/line" className="font-medium underline underline-offset-2">
              See the work
            </Link>
          </span>
          {doing}
        </span>
      ),
    };
  }
  return {
    key: "setup",
    label: "Setup",
    tone: "attention",
    body: (
      <span className="flex flex-col gap-1">
        <span className="min-w-0 break-words">
          <span className="font-medium text-warning">Setup isn't finished.</span>{" "}
          {behind
            .filter(yourPartLeft)
            .map((r) =>
              r.state === "incomplete"
                ? `${many ? repoName(r.repo) + ": the" : "The"} check couldn't finish`
                : `${many ? repoName(r.repo) + ": " : ""}${r.done} of ${r.total} setup steps done`,
            )
            .join("; ")}
          . New tasks wait until it's done.{" "}
          <Link to="/setup" className="font-medium underline underline-offset-2">
            See what's missing
          </Link>
        </span>
        {doing}
      </span>
    ),
  };
}

/** Whether a person's part of a repo's setup is left: its steps, or a check that hasn't run or couldn't finish. */
export const yourPartLeft = (r: RepoReadiness) => r.state === "unchecked" || r.state === "incomplete" || (r.state === "not_ready" && r.done < r.total);
