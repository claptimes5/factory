import { useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, CheckCircle2, ChevronDown, Info, XCircle } from "lucide-react";
import { act, type Overview } from "@/api/client";
import { Button } from "@/components/ui/button";
import { toast } from "@/components/ui/toaster";
import { useSetupNotice } from "@/screens/Setup";
import { cn, repoName } from "@/lib/utils";

/**
 * One place for what applies to every screen: main is red, the disk is
 * full, auto-merge is off, setup isn't finished. The most serious shows;
 * the rest wait behind "more", so they never push the screen down a row
 * at a time.
 */
export type Notice = {
  key: string;
  /** The region's name, for screen readers and tests. */
  label: string;
  tone: "danger" | "attention" | "info" | "success";
  body: ReactNode;
};

const order = { danger: 0, attention: 1, info: 2, success: 3 };

const toneStyle = {
  danger: "border-destructive/30 bg-destructive/5",
  attention: "border-warning-border bg-warning-bg",
  info: "border-live-border bg-live-bg",
  success: "border-transparent bg-success-bg",
};

const toneIcon = {
  danger: <XCircle className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden />,
  attention: <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />,
  info: <Info className="mt-0.5 size-4 shrink-0 text-primary" aria-hidden />,
  success: <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-success" aria-hidden />,
};

export function NoticeArea({ o }: { o?: Overview }) {
  const [all, setAll] = useState(false);
  const setup = useSetupNotice();
  const notices = [...(o ? overviewNotices(o) : []), ...(setup ? [setup] : [])].sort((a, b) => order[a.tone] - order[b.tone]);
  if (notices.length === 0) return null;
  const shown = all ? notices : notices.slice(0, 1);
  const more = notices.length - shown.length;
  return (
    <div aria-label="Notices" role="group" className="mb-5 flex flex-col gap-2">
      {shown.map((n) => (
        <section key={n.key} aria-label={n.label} className={cn("flex gap-2.5 rounded-card border px-3.5 py-3 text-body-sm", toneStyle[n.tone])}>
          {toneIcon[n.tone]}
          <div className="min-w-0 flex-1 break-words">{n.body}</div>
        </section>
      ))}
      {(more > 0 || (all && notices.length > 1)) && (
        <button
          onClick={() => setAll(!all)}
          aria-expanded={all}
          className="flex w-fit cursor-pointer items-center gap-1.5 px-1 text-meta font-medium text-muted-foreground hover:text-foreground"
        >
          {all ? "Show fewer" : more === 1 ? "1 more notice" : `${more} more notices`}
          <ChevronDown className={cn("size-3.5 transition-transform", all && "rotate-180")} aria-hidden />
        </button>
      )}
    </div>
  );
}

function overviewNotices(o: Overview): Notice[] {
  const out: Notice[] = [];
  const bad = o.main_health.filter((h) => h.state === "red" || h.state === "flaky");
  if (bad.length > 0) {
    out.push({
      key: "main",
      label: "Main health",
      tone: bad.some((h) => h.state === "red") ? "danger" : "attention",
      body: <MainHealth o={o} bad={bad} />,
    });
  }
  if (o.disk_low) out.push({ key: "disk", label: "Disk", tone: "danger", body: o.disk_low });
  if (o.auto_merge.some((b) => b.tripped)) out.push({ key: "auto-merge", label: "Auto-merge breaker", tone: "attention", body: <AutoMergeBreaker o={o} /> });
  return out;
}

/** A line per repo whose base branch is red or flaky, with the fix issue. */
function MainHealth({ o, bad }: { o: Overview; bad: Overview["main_health"] }) {
  return (
    <ul className="flex flex-col gap-1">
      {bad.map((h) => (
        <li key={h.repo} className="min-w-0 break-words">
          <span className={cn("font-medium", h.state === "red" ? "text-destructive" : "text-warning")}>
            {o.repos.length > 1 ? `${repoName(h.repo)} ` : ""}
            {h.branch} is {h.state === "red" ? "red" : "flaky"}
          </span>
          {": "}
          {h.checks.length === 0
            ? h.detail
            : h.checks.map((c, i) => (
                <span key={c.check}>
                  {i > 0 && ", "}
                  <span className="font-mono">{c.check}</span>
                  {c.kind === "flaky" && h.state === "red" ? " (flaky)" : ""}{" "}
                  {c.issue > 0 ? (
                    <a href={c.issue_url} target="_blank" rel="noreferrer" className="underline underline-offset-2">
                      #{c.issue}
                    </a>
                  ) : (
                    <span className="text-muted-foreground">(not filed yet)</span>
                  )}
                </span>
              ))}
        </li>
      ))}
    </ul>
  );
}

/** A line per repo whose auto-merge breaker tripped, with its Reset button. */
function AutoMergeBreaker({ o }: { o: Overview }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const tripped = o.auto_merge.filter((b) => b.tripped);
  // A toast, since a reset that works takes this notice away.
  const reset = async (repo: string) => {
    setBusy(repo);
    const r = await act("/auto-merge/reset", { repo });
    setBusy("");
    if (r.ok) toast.success(r.message);
    else toast.error(r.message);
    void qc.invalidateQueries();
  };
  return (
    <ul className="flex flex-col gap-2">
      {tripped.map((b) => (
        <li key={b.repo} className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1.5 break-words">
          <span className="min-w-0">
            <span className="font-medium text-warning">Auto-merge is off{o.repos.length > 1 ? ` for ${repoName(b.repo)}` : ""}</span>
            {": "}
            {b.reason}. New PRs wait for you.
          </span>
          {b.can_reset ? (
            <Button size="sm" variant="outline" disabled={busy === b.repo} onClick={() => reset(b.repo)}>
              Reset
            </Button>
          ) : (
            <span className="text-meta text-muted-foreground">Resetting it here needs a dashboard password: set FACTORY_WEB_PASSWORD.</span>
          )}
        </li>
      ))}
    </ul>
  );
}
