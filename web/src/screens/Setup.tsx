import { useEffect, useState, type ReactNode } from "react";
import { Link, useLocation } from "react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, CircleDashed, ExternalLink, Loader2, XCircle } from "lucide-react";
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
function Finding({ f, label, repo, canFile }: { f: FindingT; label: string; repo: RepoReadiness; canFile: boolean }) {
  const [open, setOpen] = useState(false);
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
        ? canFile && repo.unfiled > 0 && "No issue yet: File and start work files one."
        : f.status === "not_queued"
          ? `Add the ${label} label on the issue so the factory works on it.`
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
        {f.level !== "good" && <Badge variant={s.variant}>{s.label}</Badge>}
        {f.issue > 0 && (
          <a href={f.issue_url} target="_blank" rel="noreferrer" className="flex items-center gap-1 font-mono text-xs text-primary hover:underline">
            #{f.issue} <ExternalLink className="size-3" aria-hidden />
          </a>
        )}
        {f.item_id != null && (
          <Link to={`/items/${f.item_id}`} className="text-xs text-primary hover:underline">
            See the work
          </Link>
        )}
      </div>
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
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

function StepRow({ step, n, label, repo, canFile }: { step: Step; n: number; label: string; repo: RepoReadiness; canFile: boolean }) {
  return (
    <li data-testid="setup-step" className="flex gap-3 py-3">
      <StepIcon state={step.state} />
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        <h4 className="text-body font-semibold">
          {n}. {step.title}
        </h4>
        <p className="text-meta text-text-2">{step.why}</p>
        {step.state === "done" && step.passed && <p className="text-meta text-success">{step.passed}</p>}
        {step.state === "unchecked" && <p className="text-meta text-muted-foreground">Not checked yet: the check stopped before this step.</p>}
        {step.blockers.length > 0 && (
          <ul className="divide-y">
            {step.blockers.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={repo} canFile={canFile} />
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}

/** The one thing to do next on a repo, with its button when the factory can do it. */
function NextStep({
  r,
  data,
  busy,
  run,
}: {
  r: RepoReadiness;
  data: Readiness;
  busy: boolean;
  run: (path: string, body: Record<string, unknown>) => Promise<void>;
}) {
  const label = data.intake_label;
  const blockers = r.steps.flatMap((s) => s.blockers);
  const needsYou = [...blockers, ...r.extras].filter((f) => f.status === "needs_you").length;
  const yours = blockers.filter((f) => f.status === "none").length;
  const recommended = r.extras.filter((f) => f.level === "gap" && f.status !== "done").length;
  const canStart = data.can_file && !r.checking && r.can_queue;
  const start = (
    <div className="flex flex-wrap gap-2">
      <Button size="sm" disabled={busy} onClick={() => run("/readiness/file", { queue: true })}>
        File and start work
      </Button>
      {r.unfiled > 0 && (
        <Button size="sm" variant="outline" disabled={busy} onClick={() => run("/readiness/file", { queue: false })}>
          Only file the issues
        </Button>
      )}
    </div>
  );
  let tone: "info" | "success" | "attention" = "info";
  let title: string;
  let body: ReactNode;
  let action: ReactNode = null;
  if (r.state === "unchecked") {
    title = `Next: check ${repoName(r.repo) || r.repo}`;
    body = `The check clones ${r.repo}, installs it and runs its checks twice to catch flaky tests. It takes a few minutes and changes nothing.`;
  } else if (r.state === "incomplete") {
    tone = "attention";
    title = "Next: fix what stopped the check, then check again";
    body =
      "The check stopped early, so this says nothing about the repo past that point. It's usually the factory's setup or a command in factory.yaml, not the repo's code.";
  } else if (needsYou > 0) {
    tone = "attention";
    title = `Next: ${needsYou === 1 ? "one of these needs" : `${needsYou} of these need`} you`;
    body = "The factory stopped on it and is waiting for your call. See the work to answer it.";
  } else if (r.state === "not_ready" && canStart) {
    title = "Next: let the factory work through it";
    body = `File and start work files an issue for each blocker and labels them ${label}, so the factory fixes them. Once every step passes it starts on the ${recommended > 0 ? `${recommended} recommended ${recommended === 1 ? "item" : "items"}` : "recommended work"} below by itself, checking again whenever ${r.branch} changes. You review every pull request; nothing merges without you.`;
    action = start;
  } else if (r.state === "not_ready" && r.can_queue && !data.can_file) {
    title = "Next: hand the blockers to the factory";
    body = `Filing their issues from this page needs leave to change settings (see below). Once filed with the ${label} label, the factory fixes them.`;
  } else if (r.state === "not_ready" && r.baseline_pr) {
    tone = "attention";
    title = "Next: merge the lint baseline";
    body = `Lint passes on ${r.branch} once it's merged; only new offenses fail a change. The factory checks again by itself when ${r.branch} changes.`;
    action = (
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" asChild>
          <a href={r.baseline_pr} target="_blank" rel="noreferrer">
            Open the pull request <ExternalLink className="size-3.5" aria-hidden />
          </a>
        </Button>
        {data.can_check && !r.checking && (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => run("/readiness/baseline", {})}>
            Make it again
          </Button>
        )}
      </div>
    );
  } else if (r.state === "not_ready" && yours > 0) {
    tone = "attention";
    title = `Next: ${yours === 1 ? "one blocker is" : `${yours} blockers are`} yours to fix`;
    body = `The factory can't change ${yours === 1 ? "it" : "them"}: the fix is in factory.yaml or the factory's setup. The factory is working on the rest${r.following ? `, and checks again by itself when ${r.branch} changes` : ""}.`;
  } else if (r.state === "not_ready") {
    title = "Nothing for you right now: the factory is fixing the blockers";
    body = r.following
      ? `It checks again by itself whenever ${r.branch} changes, and starts the recommended work once every step passes. You'll review each fix as a pull request.`
      : `You'll review each fix as a pull request. Once they're merged, check again.`;
  } else if (canStart) {
    tone = "success";
    title = "Ready. Next: start the recommended work";
    body = `Every setup step passes, so you can add the ${label} label to real issues now. File and start work also hands the recommended ${recommended === 1 ? "item" : "items"} below to the factory, so it can prove more of what it changes.`;
    action = start;
  } else {
    tone = "success";
    title = `Ready: add the ${label} label to issues to start`;
    body = "Every setup step passes, so the factory's changes can be checked. It picks up each issue with that label.";
  }
  return (
    <div
      data-testid="next-step"
      className={cn(
        "flex flex-col gap-2 rounded-control border p-3 text-sm",
        tone === "success" ? "border-success/40 bg-success/10" : tone === "attention" ? "border-warning/40 bg-warning/10" : "border-primary/40 bg-primary/5",
      )}
    >
      <p className={cn("font-semibold", tone === "success" && "text-success")}>{title}</p>
      <p className="text-text-2">{body}</p>
      {action}
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
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const run = async (path: string, body: Record<string, unknown>) => {
    setBusy(true);
    const res = await act(path, { repo: r.repo, ...body });
    setBusy(false);
    setMsg({ ok: res.ok, text: res.message });
    void qc.invalidateQueries({ queryKey: ["readiness"] });
  };
  const check = () => run("/readiness/check", {});
  // What a run started here said goes once it's done: its result shows instead.
  useEffect(() => {
    if (!r.checking) setMsg(null);
  }, [r.checking]);
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
              <Loader2 className="size-4 animate-spin" aria-hidden /> {r.action === "check" ? "Checking now" : r.action === "baseline" ? "Making the lint baseline" : "Checking, then filing issues"}, started{" "}
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

      <NextStep r={r} data={data} busy={busy} run={run} />
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
      {msg && (
        <p role="status" className={cn("text-sm", msg.ok ? "text-success" : "text-destructive")}>
          {msg.text}
        </p>
      )}

      {r.can_baseline && r.baseline_pr && (
        <div data-testid="baseline" className="flex flex-wrap items-center gap-2 rounded-control border p-3 text-sm">
          <p className="min-w-0 flex-1">The lint baseline is waiting for you to merge it. The factory checks again by itself once you have.</p>
          <a href={r.baseline_pr} target="_blank" rel="noreferrer" className="flex items-center gap-1 font-medium text-primary hover:underline">
            Open the pull request <ExternalLink className="size-3.5" aria-hidden />
          </a>
        </div>
      )}
      {r.can_baseline && data.can_check && !r.checking && !r.baseline_pr && (
        <div data-testid="baseline" className="flex flex-col gap-2 rounded-control border border-primary/40 bg-primary/5 p-3 text-sm">
          <p className="font-semibold">Lint fails on {r.branch} today. Accept today's offenses, and block new ones?</p>
          <p className="text-text-2">
            The factory runs RuboCop's own baseline (<code className="font-mono text-[0.9em]">rubocop --auto-gen-config</code>) on {r.branch}, checks
            that lint passes with it, and opens a pull request that changes only <code className="font-mono text-[0.9em]">.rubocop_todo.yml</code>{" "}
            (and <code className="font-mono text-[0.9em]">.rubocop.yml</code>, to include it). Once you merge it, lint passes and only new offenses fail a
            change; fix the old ones later, a cop at a time. The factory checks again by itself once it's merged.
          </p>
          <div>
            <Button size="sm" disabled={busy} onClick={() => run("/readiness/baseline", {})}>
              Accept today's offenses
            </Button>
          </div>
        </div>
      )}

      {r.steps.length > 0 && (
        <ol aria-label="Setup steps" className="divide-y border-y">
          {r.steps.map((s, i) => (
            <StepRow key={s.key} step={s} n={i + 1} label={label} repo={r} canFile={data.can_file} />
          ))}
        </ol>
      )}

      {gaps.length > 0 && (
        <section aria-label="Recommended" className="flex flex-col gap-1">
          <h3 className="text-body font-semibold">Recommended ({gaps.length})</h3>
          <p className="text-meta text-text-2">
            Not needed to start, but each lets the factory prove more of what it changes, so fewer of its mistakes reach you.{" "}
            {r.following && r.state !== "ready"
              ? "The factory starts on these by itself once every step above passes."
              : !data.can_file
                ? ""
                : r.state === "ready"
                ? "File and start work hands them to the factory."
                : "File and start work hands them to the factory once every step above passes."}
          </p>
          <ul className="divide-y">
            {gaps.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={r} canFile={data.can_file} />
            ))}
          </ul>
        </section>
      )}
      {tips.length > 0 && (
        <details>
          <summary className="cursor-pointer text-sm font-medium">Nice to have ({tips.length})</summary>
          <ul className="divide-y">
            {tips.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={r} canFile={data.can_file} />
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

const actionText: Record<string, string> = {
  check: "Checking",
  file: "Checking, then filing issues for",
  queue: "Checking, then filing and starting work on",
  baseline: "Making the lint baseline for",
};

/**
 * The setup notice: while a repo isn't ready, or once all are and nothing
 * else has started, or while setup's own check runs (not a job, so the
 * Floor doesn't show it). None on the setup screen itself.
 */
export function useSetupNotice(): Notice | null {
  const { data } = useReadiness();
  const here = useLocation().pathname === "/setup";
  if (!data || here) return null;
  const running = data.repos.find((r) => r.checking);
  const doing = running && (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <Loader2 className="size-3.5 animate-spin text-primary" aria-hidden />
      {actionText[running.action] ?? "Checking"} {repoName(running.repo) || running.repo}, started <Ago ts={running.checking_since} />.
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
  return {
    key: "setup",
    label: "Setup",
    tone: "attention",
    body: (
      <span className="flex flex-col gap-1">
        <span className="min-w-0 break-words">
          <span className="font-medium text-warning">Setup isn't finished.</span>{" "}
          {behind
            .map((r) =>
              r.state === "incomplete"
                ? `${data.repos.length > 1 ? repoName(r.repo) + ": the" : "The"} check couldn't finish`
                : `${data.repos.length > 1 ? repoName(r.repo) + ": " : ""}${r.done} of ${r.total} setup steps done`,
            )
            .join("; ")}
          .{" "}
          {behind.every((r) => r.following && r.state === "not_ready")
            ? "The factory is working through it; add real tasks once it's done."
            : "Fix these before adding real tasks."}{" "}
          <Link to="/setup" className="font-medium underline underline-offset-2">
            See what's missing
          </Link>
        </span>
        {doing}
      </span>
    ),
  };
}
