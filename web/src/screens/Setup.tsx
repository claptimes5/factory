import { useState, type ReactNode } from "react";
import { Link, useLocation } from "react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, CircleDashed, ExternalLink, Loader2, XCircle } from "lucide-react";
import { act, getJSON } from "@/api/client";
import type { components } from "@/api/schema";
import { ErrorBox, Loading, PageTitle } from "@/components/common";
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

/** "`lint` fails on main" with the command in code. */
function Inline({ text }: { text: string }) {
  return (
    <>
      {text.split(/(`[^`]+`)/).map((part, i) =>
        part.startsWith("`") && part.endsWith("`") && part.length > 1 ? (
          <code key={i} className="rounded bg-muted px-1 font-mono text-[0.9em]">
            {part.slice(1, -1)}
          </code>
        ) : (
          part
        ),
      )}
    </>
  );
}

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
    <div className="flex flex-col gap-2 text-[13px] leading-relaxed text-text-2">
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
  done: { label: "Done", variant: "success" },
};

/** One blocker or gap, with its issue and the factory's work on it. */
function Finding({ f, label, repo }: { f: FindingT; label: string; repo: RepoReadiness }) {
  const [open, setOpen] = useState(false);
  const s = statusText[f.status] ?? { label: f.status, variant: "secondary" as const };
  const hint =
    f.status === "none"
      ? "The factory can't change this one: its fix is in factory.yaml or the factory's setup."
      : f.status === "not_filed"
        ? repo.queue_command && `Run ${repo.queue_command} to file it.`
        : f.status === "not_queued"
          ? `Add the ${label} label on the issue so the factory works on it.`
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

function StepRow({ step, n, label, repo }: { step: Step; n: number; label: string; repo: RepoReadiness }) {
  return (
    <li data-testid="setup-step" className="flex gap-3 py-3">
      <StepIcon state={step.state} />
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        <h4 className="text-[15px] font-semibold">
          {n}. {step.title}
        </h4>
        <p className="text-[13px] text-text-2">{step.why}</p>
        {step.state === "done" && step.passed && <p className="text-[13px] text-success">{step.passed}</p>}
        {step.state === "unchecked" && <p className="text-[13px] text-muted-foreground">Not checked yet: the check stopped before this step.</p>}
        {step.blockers.length > 0 && (
          <ul className="divide-y">
            {step.blockers.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={repo} />
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}

function Command({ children }: { children: ReactNode }) {
  return <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs break-all">{children}</code>;
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
  const check = async () => {
    setBusy(true);
    const res = await act("/readiness/check", { repo: r.repo });
    setBusy(false);
    setMsg({ ok: res.ok, text: res.message });
    void qc.invalidateQueries({ queryKey: ["readiness"] });
  };
  const [badge, variant] = stateBadge[r.state] ?? [r.state, "secondary"];
  const label = data.intake_label;
  const gaps = r.extras.filter((f) => f.level !== "good");
  const good = r.extras.filter((f) => f.level === "good");
  return (
    <section aria-label={`Setup of ${r.repo}`} data-testid="setup-repo" className="flex flex-col gap-4 rounded-xl border bg-card p-4 md:p-5">
      <div className="flex flex-wrap items-start gap-3">
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <h2 className="flex flex-wrap items-center gap-2.5 font-display text-lg font-semibold">
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
              <Loader2 className="size-4 animate-spin" aria-hidden /> Checking now, started {ago(r.checking_since)}
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

      {r.state === "ready" && (
        <div className="rounded-lg border border-success/40 bg-success/10 p-3 text-sm">
          <p className="font-semibold text-success">Ready: add the {label} label to issues to start.</p>
          <p className="mt-1 text-text-2">Every setup step passes, so the factory's changes can be checked. It picks up each issue with that label.</p>
          {r.unqueued > 0 && (
            <p className="mt-1 text-text-2">
              {r.unqueued === 1 ? "1 issue" : `${r.unqueued} issues`} the check filed for later {r.unqueued === 1 ? "isn't" : "aren't"} labeled yet. Label{" "}
              {r.unqueued === 1 ? "it" : "them"} {r.queue_command ? <>or run <Command>{r.queue_command}</Command></> : null} when you want the factory to work on{" "}
              {r.unqueued === 1 ? "it" : "them"}.
            </p>
          )}
        </div>
      )}
      {r.state === "not_ready" && (
        <p className="text-sm text-text-2">
          Every job would fail until these are fixed, so add real tasks once they are. The factory can fix most of them itself; this page follows its work.
          {r.queue_command && (
            <>
              {" "}
              Some have no issue yet: run <Command>{r.queue_command}</Command> to file them and hand the blockers to the factory.
            </>
          )}
        </p>
      )}
      {r.state === "incomplete" && (
        <p className="text-sm text-text-2">
          The check stopped early, so this says nothing about the repo past that point. It's usually the factory's setup or a command in factory.yaml, not the
          repo's code. Fix it, then check again.
        </p>
      )}
      {r.state === "unchecked" && (
        <p className="text-sm text-text-2">
          The check clones {r.repo}, installs it and runs its checks twice to catch flaky tests. It takes a few minutes and changes nothing.
        </p>
      )}
      {!data.can_check && !r.checking && (
        <p className="text-xs text-muted-foreground">
          {r.state === "unchecked" ? "Check it" : "Check again"} with <Command>{r.command}</Command>
          {data.check_note && !data.check_note.includes("can't run") ? `. ${data.check_note}` : "."}
        </p>
      )}
      {r.check_error && (
        <p role="alert" className="text-sm text-destructive">
          The last check failed: {r.check_error}
        </p>
      )}
      {msg && (
        <p role="status" className={cn("text-sm", msg.ok ? "text-success" : "text-destructive")}>
          {msg.text}
        </p>
      )}

      {r.steps.length > 0 && (
        <ol aria-label="Setup steps" className="divide-y border-y">
          {r.steps.map((s, i) => (
            <StepRow key={s.key} step={s} n={i + 1} label={label} repo={r} />
          ))}
        </ol>
      )}

      {gaps.length > 0 && (
        <details className="group">
          <summary className="cursor-pointer text-sm font-medium">Worth doing later ({gaps.length})</summary>
          <p className="mt-1 text-xs text-muted-foreground">Not needed to start. Each lets the factory verify more of what it changes.</p>
          <ul className="divide-y">
            {gaps.map((f) => (
              <Finding key={f.title} f={f} label={label} repo={r} />
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
      </div>
      {data.repos.map((r) => (
        <RepoCard key={r.repo} r={r} data={data} many={data.repos.length > 1} />
      ))}
    </div>
  );
}

/** A line above every screen while a repo isn't ready, or once all are and nothing else has started. */
export function SetupBanner() {
  const { data } = useReadiness();
  const here = useLocation().pathname === "/setup";
  if (!data || !data.banner || here) return null;
  if (data.banner === "ready") {
    return (
      <section aria-label="Setup" className="border-b border-success/30 bg-success/10 px-4 py-2.5 text-sm md:px-6">
        <div className="mx-auto flex max-w-[1680px] flex-wrap items-center gap-x-3 gap-y-1">
          <span className="font-medium text-success">Ready: add the {data.intake_label} label to issues to start.</span>
          <Link to="/setup" className="underline underline-offset-2">
            See the setup
          </Link>
        </div>
      </section>
    );
  }
  const behind = data.repos.filter((r) => r.state === "not_ready" || r.state === "incomplete");
  return (
    <section aria-label="Setup" className="border-b border-warning-border bg-warning-bg px-4 py-2.5 text-sm md:px-6">
      <div className="mx-auto flex max-w-[1680px] flex-wrap items-center gap-x-3 gap-y-1">
        <span className="min-w-0 break-words">
          <span className="font-medium text-warning">Setup isn't finished.</span>{" "}
          {behind
            .map((r) =>
              r.state === "incomplete"
                ? `${data.repos.length > 1 ? repoName(r.repo) + ": the" : "The"} check couldn't finish`
                : `${data.repos.length > 1 ? repoName(r.repo) + ": " : ""}${r.done} of ${r.total} setup steps done`,
            )
            .join("; ")}
          . Fix these before adding real tasks.
        </span>
        <Link to="/setup" className="font-medium underline underline-offset-2">
          See what's missing
        </Link>
      </div>
    </section>
  );
}
