import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { act, getJSON, type Choice, type SetupFix } from "@/api/client";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

const extras: [number, string][] = [
  [0, "No more"],
  [0.5, "+50%"],
  [1, "+100%"],
  [2, "+200%"],
];

/**
 * What a person can do next with a job that ended: Continue (when its work
 * was kept), Retry, Tier up and Park, each saying what it does and which
 * model it runs on. Every press goes through the daemon's own checks.
 * Compact shows the explanations on request, or right away for one button;
 * otherwise they are listed. Inverse is for the lead card; children go at
 * the end of the button row.
 */
export function Choices({ jobId, itemId, choices, compact, inverse, children }: { jobId: number; itemId?: number; choices: Choice[]; compact?: boolean; inverse?: boolean; children?: React.ReactNode }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [continuing, setContinuing] = useState(false);
  const [fixing, setFixing] = useState(false);
  const [extra, setExtra] = useState(0);
  const [note, setNote] = useState("");
  const press = async (name: string, path: string, body?: unknown) => {
    setBusy(name);
    const r = await act(path, body);
    setBusy("");
    setMsg({ ok: r.ok, text: r.message });
    if (r.ok) setContinuing(false);
    void qc.invalidateQueries();
  };
  const run = (c: Choice) => {
    switch (c.action) {
      case "fix_setup":
        setFixing(!fixing);
        return;
      case "continue":
        setContinuing(!continuing);
        return;
      case "retry":
        return press("retry", `/jobs/${jobId}/retry`, { tier_up: false });
      case "tier_up":
        return press("tier_up", `/jobs/${jobId}/retry`, { tier_up: true });
      case "park": {
        const reason = window.prompt("Why park it? (shown on the issue)", "");
        if (reason !== null && itemId) void press("park", `/items/${itemId}/park`, { reason });
      }
    }
  };
  const shown = choices.filter((c) => (c.available || c.action === "continue") && (c.action !== "fix_setup" || itemId));
  const lead = (c: Choice) => c.action === "continue" || c.action === "fix_setup";
  // The help explains the buttons shown, and lists apart the choices that
  // aren't offered with why, so it never reads as more buttons than there are.
  const hidden = choices.filter((c) => !shown.includes(c));
  const help = (
    <div className={cn("space-y-1.5 text-xs", inverse ? "text-ink-muted" : "text-muted-foreground")} data-testid="choice-help">
      <ul className="space-y-1.5">
        {shown.map((c) => (
          <li key={c.action}>
            <span className={cn("font-medium", inverse ? "text-ink-foreground" : "text-foreground")}>{c.label}</span>
            {c.model && <span className="font-mono"> ({c.model})</span>}: {c.available ? c.explain : c.why || c.explain}
          </li>
        ))}
      </ul>
      {hidden.length > 0 && (
        <details data-testid="choice-unavailable">
          <summary className="cursor-pointer">Not offered now ({hidden.length})</summary>
          <ul className="mt-1 space-y-1 pl-3">
            {hidden.map((c) => (
              <li key={c.action}>
                {c.label}
                {c.why && <>: {c.why}</>}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
  return (
    <div className="flex flex-col gap-2" data-testid="choices">
      <div className="flex flex-wrap gap-2">
        {shown.map((c) => (
          <Button
            key={c.action}
            size={inverse ? "default" : "sm"}
            variant={lead(c) ? (inverse ? "inverse" : "default") : inverse ? "inverse-outline" : "outline"}
            disabled={!!busy || !c.available}
            title={(c.available ? c.explain : c.why) + (c.model ? ` Model: ${c.model}.` : "")}
            aria-expanded={c.action === "continue" ? continuing : c.action === "fix_setup" ? fixing : undefined}
            onClick={() => run(c)}
          >
            {c.label}
            {c.model && c.available && <span className="font-normal opacity-75">on {c.model.split("/").pop()}</span>}
          </Button>
        ))}
        {children}
      </div>
      {fixing && itemId && <FixSetup itemId={itemId} press={press} busy={!!busy} />}
      {continuing && (
        <form
          className="space-y-2 rounded-md border bg-card p-3"
          onSubmit={(e) => {
            e.preventDefault();
            void press("continue", `/jobs/${jobId}/continue`, { extra, note });
          }}
        >
          <fieldset className="flex flex-wrap items-center gap-2 text-xs">
            <legend className="mb-1 font-medium">More turns and budget</legend>
            {extras.map(([v, label]) => (
              <label key={v} className={cn("cursor-pointer rounded-md border px-2 py-1", extra === v && "border-primary bg-primary/10")}>
                <input type="radio" name="extra" className="sr-only" checked={extra === v} onChange={() => setExtra(v)} />
                {label}
              </label>
            ))}
          </fieldset>
          <label htmlFor={`note-${jobId}`} className="block text-xs font-medium">
            A note for the builder (optional). It reads this first: say what to finish, or what to leave out.
          </label>
          <textarea
            id={`note-${jobId}`}
            value={note}
            onChange={(e) => setNote(e.target.value)}
            rows={3}
            className="w-full min-w-0 rounded-md border bg-card p-2 text-base md:text-sm"
            placeholder="For example: finish the model and its tests; leave the admin screen for a separate issue."
          />
          {extra > 0 && <p className="text-xs text-muted-foreground">Adding budget needs a dashboard password, or the dashboard opened on the factory's machine.</p>}
          <Button size="sm" type="submit" disabled={!!busy}>
            Continue from here
          </Button>
        </form>
      )}
      {compact && shown.length > 1 ? (
        <details className="text-xs">
          <summary className={cn("cursor-pointer", inverse ? "text-ink-muted" : "text-muted-foreground")}>What do these do?</summary>
          <div className="mt-1.5">{help}</div>
        </details>
      ) : (
        help
      )}
      {msg && (
        <p role="status" className={cn("text-sm", msg.ok ? "text-success" : "text-destructive")}>
          {msg.text}
        </p>
      )}
    </div>
  );
}

/**
 * Fix setup: what the job's sandbox lacked, and the change to factory.yaml
 * the repo's own files suggest, for a person to apply.
 */
function FixSetup({ itemId, press, busy }: { itemId: number; press: (name: string, path: string, body?: unknown) => Promise<void>; busy: boolean }) {
  const { data: fix, error, isLoading } = useQuery({ queryKey: ["setup-fix", itemId], queryFn: () => getJSON<SetupFix>(`/items/${itemId}/setup-fix`) });
  if (isLoading) return <p className="text-sm text-muted-foreground">Reading the repo's files…</p>;
  if (error || !fix) return <p className="text-sm text-destructive">{error instanceof Error ? error.message : "Couldn't read the repo's files."}</p>;
  return (
    <div className="space-y-3 rounded-md border bg-card p-3 text-sm" data-testid="fix-setup">
      <div className="space-y-1">
        <p>The checks can't run in the factory's sandbox: {fix.problem}.</p>
        <pre className="overflow-x-auto rounded-md bg-muted p-2 font-mono text-xs whitespace-pre-wrap">{fix.line}</pre>
      </div>
      {fix.changes.length > 0 ? (
        <div className="space-y-1">
          <p className="font-medium">From the repo's files, for {fix.repo} in factory.yaml:</p>
          <ul className="list-disc space-y-0.5 pl-5">
            {fix.changes.map((c) => (
              <li key={c}>{c}</li>
            ))}
          </ul>
        </div>
      ) : (
        <p>
          The repo's Dockerfile and compose file don't say what's missing, so there's nothing to change from here. Add it to {fix.repo} in factory.yaml (a service
          under <code>services:</code>, or a job image that has it), then press Retry.
        </p>
      )}
      {fix.notes.map((n) => (
        <p key={n} className="text-xs text-muted-foreground">
          {n}
        </p>
      ))}
      {fix.changes.length > 0 && (
        <>
          <details className="text-xs">
            <summary className="cursor-pointer text-muted-foreground">factory.yaml before and after</summary>
            <div className="mt-1 grid gap-2 md:grid-cols-2">
              <pre className="min-w-0 overflow-x-auto rounded-md bg-muted p-2 font-mono">{fix.before}</pre>
              <pre className="min-w-0 overflow-x-auto rounded-md bg-muted p-2 font-mono">{fix.after}</pre>
            </div>
          </details>
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" disabled={busy || !fix.writable} onClick={() => void press("fix_setup", `/items/${itemId}/setup-fix`, { version: fix.version })}>
              Apply and retry
            </Button>
            <span className="text-xs text-muted-foreground">
              {fix.writable ? "Saves it, restarts the factory once running jobs finish (rebuilding the job image if it changed), then retries." : fix.reason}
            </span>
          </div>
        </>
      )}
    </div>
  );
}
