import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { act, type Choice } from "@/api/client";
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
 * Compact shows the explanations on request; otherwise they are listed.
 */
export function Choices({ jobId, itemId, choices, compact }: { jobId: number; itemId?: number; choices: Choice[]; compact?: boolean }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [continuing, setContinuing] = useState(false);
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
  const shown = choices.filter((c) => c.available || c.action === "continue");
  // The help explains the buttons shown, and lists apart the choices that
  // aren't offered with why, so it never reads as more buttons than there are.
  const hidden = choices.filter((c) => !shown.includes(c));
  const help = (
    <div className="space-y-1.5 text-xs text-muted-foreground" data-testid="choice-help">
      <ul className="space-y-1.5">
        {shown.map((c) => (
          <li key={c.action}>
            <span className="font-medium text-foreground">{c.label}</span>
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
            size="sm"
            variant={c.action === "continue" ? "default" : "outline"}
            disabled={!!busy || !c.available}
            title={(c.available ? c.explain : c.why) + (c.model ? ` Model: ${c.model}.` : "")}
            aria-expanded={c.action === "continue" ? continuing : undefined}
            onClick={() => run(c)}
          >
            {c.label}
          </Button>
        ))}
      </div>
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
      {compact ? (
        <details className="text-xs">
          <summary className="cursor-pointer text-muted-foreground">What do these do?</summary>
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
