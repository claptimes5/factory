import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, CheckCircle2, ChevronRight, CircleDashed, ExternalLink, Loader2, XCircle } from "lucide-react";
import { act, useJob, type Gate } from "@/api/client";
import { Choices } from "@/components/Choices";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ago, cn, stageName } from "@/lib/utils";

export function Loading({ what = "Loading" }: { what?: string }) {
  return (
    <div className="flex items-center gap-2 p-6 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin" /> {what}…
    </div>
  );
}

export function ErrorBox({ error }: { error: unknown }) {
  return (
    <div role="alert" className="m-4 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">
      {error instanceof Error ? error.message : String(error)}
    </div>
  );
}

/** Nothing to show: centered in its own space, or inline in a section. */
export function Empty({ children, inline }: { children: ReactNode; inline?: boolean }) {
  if (inline) return <p className="text-body-sm text-muted-foreground">{children}</p>;
  return <p className="px-1 py-6 text-center text-body-sm text-muted-foreground">{children}</p>;
}

export type Crumb = { label: ReactNode; to?: string; href?: string };

/**
 * A detail screen's head: the way back (the line, then its item), the
 * title with its badges, a line of facts, and the actions, wrapping on a
 * phone. Item, Session and Diff share it.
 */
export function PageHeader({
  crumbs,
  title,
  badges,
  meta,
  children,
}: {
  crumbs: Crumb[];
  title: ReactNode;
  badges?: ReactNode;
  meta?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <header className="flex flex-col gap-2">
      <nav aria-label="Breadcrumb" className="flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 text-meta text-muted-foreground">
        {crumbs.map((c, i) => (
          <span key={i} className="flex min-w-0 items-center gap-1.5">
            {i > 0 && <ChevronRight className="size-3.5 shrink-0" aria-hidden />}
            {c.to ? (
              <Link to={c.to} className="min-w-0 truncate hover:text-foreground">
                {c.label}
              </Link>
            ) : c.href ? (
              <a href={c.href} target="_blank" rel="noreferrer" className="inline-flex min-w-0 items-center gap-1 hover:text-foreground">
                <span className="truncate">{c.label}</span> <ExternalLink className="size-3 shrink-0" aria-hidden />
              </a>
            ) : (
              <span className="truncate">{c.label}</span>
            )}
          </span>
        ))}
      </nav>
      <h1 className="flex flex-wrap items-center gap-x-2.5 gap-y-1 text-title font-semibold tracking-tight">
        <span className="min-w-0 break-words">{title}</span>
        {badges}
      </h1>
      {meta && <div className="text-body-sm text-muted-foreground">{meta}</div>}
      {children && <div className="flex flex-wrap items-center gap-2 pt-1">{children}</div>}
    </header>
  );
}

export function PageTitle({ children, aside }: { children: ReactNode; aside?: ReactNode }) {
  return (
    <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
      <h1 className="text-title font-semibold tracking-tight">{children}</h1>
      {aside}
    </div>
  );
}

const stageVariant: Record<string, "default" | "secondary" | "success" | "warning" | "destructive" | "outline"> = {
  awaiting_human: "warning",
  parked: "warning",
  blocked: "destructive",
  done: "success",
  implementing: "default",
  reviewing: "default",
};
export function StageBadge({ stage }: { stage: string }) {
  return <Badge variant={stageVariant[stage] ?? "secondary"}>{stageName(stage)}</Badge>;
}

export function JobStateBadge({ state }: { state: string }) {
  const v =
    state === "succeeded"
      ? "success"
      : state === "running"
        ? "default"
        : state === "queued"
          ? "secondary"
          : state === "stopped"
            ? "warning"
          : state === "cancelled"
            ? "outline"
            : "destructive";
  return (
    <Badge variant={v}>
      {state === "running" && <span className="size-1.5 animate-pulse rounded-full bg-current" />}
      {state}
    </Badge>
  );
}

export function GateIcon({ status }: { status: string }) {
  if (status === "pass") return <CheckCircle2 aria-label="pass" className="size-4 text-success" />;
  if (status === "fail") return <XCircle aria-label="fail" className="size-4 text-destructive" />;
  if (status === "misconfigured") return <AlertTriangle aria-label="misconfigured" className="size-4 text-warning" />;
  return <CircleDashed aria-label={status} className="size-4 text-muted-foreground" />;
}

export function GateList({ gates }: { gates: Gate[] }) {
  if (gates.length === 0) return <Empty inline>No gate results yet.</Empty>;
  return (
    <ul className="divide-y text-sm">
      {gates.map((g) => (
        <li key={g.gate} className="py-2">
          <div className="flex items-center gap-2">
            <GateIcon status={g.status} />
            <span className="font-mono text-xs">{g.gate}</span>
            <span className="text-muted-foreground">{g.status}</span>
          </div>
          {g.detail && g.status !== "pass" && (
            <pre className="mt-1 max-h-40 overflow-auto rounded bg-muted p-2 font-mono text-xs whitespace-pre-wrap">{g.detail}</pre>
          )}
        </li>
      ))}
    </ul>
  );
}

/**
 * What can be done with a job: once it ended, its choices (Continue, Retry,
 * Tier up, Park, each explained); while it runs, cancel or park. Every
 * press goes through the daemon's own checks.
 */
export function Actions({ jobId, itemId, running }: { jobId?: number | null; itemId?: number; running?: boolean }) {
  const job = useJob(jobId ?? 0, { enabled: jobId != null && !running });
  const choices = job.data?.choices ?? [];
  if (jobId != null && !running && choices.length > 0) return <Choices jobId={jobId} itemId={itemId} choices={choices} compact />;
  return <PlainActions jobId={jobId} itemId={itemId} running={running} />;
}

function PlainActions({ jobId, itemId, running }: { jobId?: number | null; itemId?: number; running?: boolean }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const press = async (name: string, path: string, body?: unknown) => {
    setBusy(name);
    const r = await act(path, body);
    setBusy("");
    setMsg({ ok: r.ok, text: r.message });
    void qc.invalidateQueries();
  };
  const park = () => {
    const reason = window.prompt("Why park it? (shown on the issue)", "");
    if (reason !== null) void press("park", `/items/${itemId}/park`, { reason });
  };
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap gap-2">
        {jobId != null && !running && (
          <>
            <Button size="sm" variant="outline" disabled={!!busy} onClick={() => press("retry", `/jobs/${jobId}/retry`, { tier_up: false })}>
              Retry
            </Button>
            <Button size="sm" variant="outline" disabled={!!busy} onClick={() => press("tier", `/jobs/${jobId}/retry`, { tier_up: true })}>
              Retry a tier up
            </Button>
          </>
        )}
        {jobId != null && running && (
          <Button
            size="sm"
            variant="destructive"
            disabled={!!busy}
            title="Stops this job now and parks the item. Nothing it did is pushed. Then retry it from its page, or add the factory:retry label on the issue."
            onClick={() => press("cancel", `/jobs/${jobId}/cancel`)}
          >
            Cancel job
          </Button>
        )}
        {itemId != null && (
          <Button
            size="sm"
            variant="outline"
            disabled={!!busy}
            title="Stops work on the item and takes it off the queue. Nothing is lost. To bring it back, press Retry on its page, or add the factory:retry label on the issue."
            onClick={park}
          >
            Park
          </Button>
        )}
      </div>
      {msg && (
        <p role="status" className={cn("text-sm", msg.ok ? "text-success" : "text-destructive")}>
          {msg.text}
        </p>
      )}
    </div>
  );
}

/** The items one waits for: it isn't built until they're done, whatever is approved. */
export function WaitsFor({ items, plan, className }: { items: { id: number; ref: string }[]; plan?: boolean; className?: string }) {
  if (items.length === 0) return null;
  const refs = items.map((w, i) => (
    <span key={w.id}>
      {i > 0 && (i === items.length - 1 ? " and " : ", ")}
      <Link to={`/items/${w.id}`} className="relative z-10 text-primary hover:underline">
        {w.ref}
      </Link>
    </span>
  ));
  return (
    <p className={cn("text-xs text-muted-foreground", className)} data-testid="waits-for">
      Built after {refs} {items.length === 1 ? "is" : "are"} done{plan ? ": approving its plan now won't start it early." : "."}
    </p>
  );
}

/** "`lint` fails on main" with the command in code. */
export function Inline({ text }: { text: string }) {
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

/** "2m ago" that keeps counting: the page's data doesn't change while something runs, so this redraws itself each second. */
/** Time since ts as m:ss, counting up each second. */
export function Elapsed({ ts }: { ts: string }) {
  const [, tick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, []);
  return <>{clock((Date.now() - Date.parse(ts)) / 1000)}</>;
}

/** Seconds as m:ss, or h:mm:ss. */
export function clock(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  const pad = (n: number) => String(n).padStart(2, "0");
  return s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}` : `${Math.floor(s / 60)}:${pad(s % 60)}`;
}

export function Ago({ ts }: { ts: string }) {
  const [, tick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, []);
  return <>{ago(ts)}</>;
}
