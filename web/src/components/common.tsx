import { useState, type ReactNode } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, CheckCircle2, CircleDashed, Loader2, XCircle } from "lucide-react";
import { act, useJob, type Gate } from "@/api/client";
import { Choices } from "@/components/Choices";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { cn, stageName } from "@/lib/utils";

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

export function Empty({ children }: { children: ReactNode }) {
  return <p className="px-1 py-6 text-center text-sm text-muted-foreground">{children}</p>;
}

export function PageTitle({ children, aside }: { children: ReactNode; aside?: ReactNode }) {
  return (
    <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
      <h1 className="font-display text-xl font-semibold tracking-tight">{children}</h1>
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
  if (gates.length === 0) return <Empty>No gate results yet.</Empty>;
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
