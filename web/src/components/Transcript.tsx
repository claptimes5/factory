import { useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, CheckCircle2, ChevronRight, Download, Loader2, OctagonX, Send } from "lucide-react";
import { act, useOverview, type Job, type Stop, type TranscriptEntry } from "@/api/client";
import { Markdown } from "@/components/Markdown";
import { Button } from "@/components/ui/button";
import { cn, money } from "@/lib/utils";

type Call = { id: string; tool: string; summary: string; input: string; done: boolean; output: string; truncated: boolean; error: boolean };
type Block =
  | { kind: "init"; e: TranscriptEntry }
  | { kind: "text" | "thinking" | "you" | "error"; e: TranscriptEntry }
  | { kind: "tools"; calls: Call[] }
  | { kind: "outcome"; e: TranscriptEntry };

/** Folds the stream's entries into what the page shows: narration, with tool calls grouped between. */
function fold(entries: TranscriptEntry[]): Block[] {
  const out: Block[] = [];
  const calls = new Map<string, Call>();
  let inited = false;
  const group = () => {
    const last = out[out.length - 1];
    if (last?.kind === "tools") return last.calls;
    const b: Block = { kind: "tools", calls: [] };
    out.push(b);
    return b.calls;
  };
  for (const e of entries) {
    switch (e.kind) {
      case "init":
        if (!inited) out.push({ kind: "init", e });
        inited = true;
        break;
      case "tool": {
        const c: Call = { id: e.tool_id, tool: e.tool, summary: e.summary, input: e.input, done: false, output: "", truncated: false, error: false };
        if (e.tool_id) calls.set(e.tool_id, c);
        group().push(c);
        break;
      }
      case "tool_result": {
        let c = e.tool_id ? calls.get(e.tool_id) : undefined;
        if (!c) {
          // A result whose call the log didn't show (Codex can report both at once).
          c = { id: e.tool_id, tool: e.tool, summary: e.summary || "Tool output", input: e.input, done: false, output: "", truncated: false, error: false };
          group().push(c);
        }
        Object.assign(c, { done: true, output: e.output, truncated: e.truncated, error: e.is_error });
        break;
      }
      case "outcome": {
        // Claude's final message is usually its last words, already shown.
        const said = out.some((b) => b.kind === "text" && b.e.text.trim() === e.text.trim());
        out.push({ kind: "outcome", e: said ? { ...e, text: "" } : e });
        break;
      }
      case "text":
      case "thinking":
      case "you":
      case "error":
        out.push({ kind: e.kind, e });
    }
  }
  return out;
}

const verbs: [RegExp, string, string][] = [
  [/^Bash$/, "ran a command", "ran %d commands"],
  [/^Read$/, "read a file", "read %d files"],
  [/^(Edit|MultiEdit|Write|NotebookEdit)$/, "edited a file", "edited %d files"],
  [/^(Grep|Glob|WebSearch)$/, "searched once", "searched %d times"],
];

/** "Ran 3 commands, read 2 files" for a group of calls. */
function groupLine(calls: Call[]): string {
  const counts = new Map<number, number>();
  for (const c of calls) {
    const i = verbs.findIndex(([re]) => re.test(c.tool));
    counts.set(i, (counts.get(i) ?? 0) + 1);
  }
  const parts = [...counts.entries()]
    .sort(([a], [b]) => (a < 0 ? 99 : a) - (b < 0 ? 99 : b))
    .map(([i, n]) => (i < 0 ? (n === 1 ? "used a tool" : `used ${n} tools`) : n === 1 ? verbs[i][1] : verbs[i][2].replace("%d", String(n))));
  const s = parts.join(", ");
  const failed = calls.filter((c) => c.error).length;
  return s.charAt(0).toUpperCase() + s.slice(1) + (failed ? ` (${failed} failed)` : "");
}

const clip = 1200;

function CallRow({ c }: { c: Call }) {
  const [open, setOpen] = useState(false);
  const [all, setAll] = useState(false);
  const long = c.output.length > clip;
  return (
    <li>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className={cn("flex w-full cursor-pointer items-start gap-1.5 text-left hover:text-foreground", c.error ? "text-destructive" : "text-muted-foreground")}
      >
        <ChevronRight className={cn("mt-0.5 size-3.5 shrink-0 transition-transform", open && "rotate-90")} aria-hidden />
        <span className="min-w-0 font-mono text-xs break-all">
          {c.summary}
          {c.error && " (failed)"}
          {!c.done && <Loader2 className="ml-1 inline size-3 animate-spin" aria-label="running" />}
        </span>
      </button>
      {open && (
        <div className="mt-1 ml-5 space-y-1">
          {c.input && <pre className="max-h-60 overflow-auto rounded bg-muted p-2 font-mono text-xs whitespace-pre-wrap">{c.input}</pre>}
          {c.done && (
            <pre className={cn("max-h-96 overflow-auto rounded border p-2 font-mono text-xs whitespace-pre-wrap", c.error && "border-destructive/50 text-destructive")}>
              {c.output === "" ? "(no output)" : all || !long ? c.output : c.output.slice(0, clip) + "…"}
              {c.truncated && (all || !long) && "\n… (cut here: the raw log has all of it)"}
            </pre>
          )}
          {c.done && long && (
            <button type="button" className="cursor-pointer text-xs text-primary hover:underline" onClick={() => setAll(!all)}>
              {all ? "Show less" : `Show all ${c.output.length.toLocaleString()} characters`}
            </button>
          )}
        </div>
      )}
    </li>
  );
}

function ToolGroup({ calls, live }: { calls: Call[]; live: boolean }) {
  const [open, setOpen] = useState(false);
  const failed = calls.filter((c) => c.error);
  if (calls.length === 1) {
    return (
      <ul data-testid="tool-group" className="text-sm">
        <CallRow c={calls[0]} />
      </ul>
    );
  }
  return (
    <div data-testid="tool-group" className="text-sm">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex cursor-pointer items-center gap-1.5 text-muted-foreground hover:text-foreground"
      >
        <ChevronRight className={cn("size-3.5 transition-transform", open && "rotate-90")} aria-hidden />
        <span className="text-xs">{groupLine(calls)}</span>
        {live && calls.some((c) => !c.done) && <Loader2 className="size-3 animate-spin" aria-label="running" />}
      </button>
      {open ? (
        <ul className="mt-1 ml-4 space-y-1 border-l pl-2">
          {calls.map((c, i) => (
            <CallRow key={c.id || i} c={c} />
          ))}
        </ul>
      ) : (
        // Errors stay in sight while the rest is folded away.
        failed.length > 0 && (
          <ul className="mt-1 ml-4 space-y-1 border-l border-destructive/40 pl-2">
            {failed.map((c, i) => (
              <CallRow key={c.id || i} c={c} />
            ))}
          </ul>
        )
      )}
    </div>
  );
}

function Thinking({ text }: { text: string }) {
  return (
    <details className="text-xs text-muted-foreground">
      <summary className="cursor-pointer select-none">Thinking</summary>
      <p className="mt-1 ml-3 border-l pl-2 whitespace-pre-wrap italic">{text}</p>
    </details>
  );
}

/** How the run ended, in plain words. */
function Outcome({ job, stop, outcome, state }: { job: Job; stop?: Stop | null; outcome?: TranscriptEntry; state: string }) {
  const plansOnly = !!useOverview().data?.plans_only;
  let tone: "ok" | "stop" | "bad" = state === "succeeded" ? "ok" : "bad";
  let head = outcome?.summary || `The job ${state.replace("_", " ")}.`;
  let more = "";
  if (stop) {
    tone = "stop";
    head = stop.summary;
    more = stop.kept
      ? "Its work is kept on the factory's machine (nothing was pushed). Continue picks up from here."
      : "Its work couldn't be kept, so it can only start over.";
  } else if (state !== "succeeded" && job.error) {
    more = job.error;
  }
  const turns = stop?.turns || outcome?.turns || 0;
  const Icon = tone === "ok" ? CheckCircle2 : tone === "stop" ? AlertTriangle : OctagonX;
  return (
    <div
      data-testid="outcome"
      className={cn(
        "flex gap-2 rounded-md border p-3 text-sm",
        tone === "ok" && "border-success/40 bg-success/10",
        tone === "stop" && "border-warning/50 bg-warning/10",
        tone === "bad" && "border-destructive/40 bg-destructive/10",
      )}
    >
      <Icon className={cn("mt-0.5 size-4 shrink-0", tone === "ok" ? "text-success" : tone === "stop" ? "text-warning" : "text-destructive")} />
      <div className="min-w-0">
        <p className="font-medium">{head}</p>
        {more && <p className="mt-0.5 break-words whitespace-pre-wrap text-muted-foreground">{more}</p>}
        <p className="mt-0.5 text-xs text-muted-foreground">
          {[turns > 0 && `${turns} turns`, !plansOnly && `${money(job.cost_usd)} of ${money(job.budget_usd)}`].filter(Boolean).join(" · ")}
        </p>
      </div>
    </div>
  );
}

/** A box to message the running agent, which reads it at its next step. */
function SteerBox({ jobId, can, note }: { jobId: number; can: boolean; note: string }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  return (
    <form
      className="mt-2 space-y-1"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        const r = await act(`/jobs/${jobId}/steer`, { text });
        setBusy(false);
        setMsg({ ok: r.ok, text: r.message });
        if (r.ok) setText("");
      }}
    >
      <div className="flex gap-2">
        <input
          aria-label="Message the agent"
          value={text}
          disabled={!can || busy}
          onChange={(e) => setText(e.target.value)}
          placeholder={can ? "Message the agent: it reads this at its next step" : "Messages can't reach this agent"}
          className="min-w-0 flex-1 rounded-md border bg-card px-2.5 py-1.5 text-base disabled:opacity-60 md:text-sm"
        />
        <Button size="sm" type="submit" disabled={!can || busy || text.trim() === ""}>
          <Send /> Send
        </Button>
      </div>
      {!can && note && <p className="text-xs text-muted-foreground">{note}</p>}
      {msg && (
        <p role="status" className={cn("text-xs", msg.ok ? "text-success" : "text-destructive")}>
          {msg.text}
        </p>
      )}
    </form>
  );
}

/**
 * Follows a job's transcript over SSE (it replays from the start on open)
 * and shows it the way Claude Code does: the agent's words as the thread,
 * tool calls folded into a line each, thinking tucked away, and how it
 * ended.
 */
export function Transcript({
  jobId,
  job,
  stop,
  canSteer = false,
  steerNote = "",
}: {
  jobId: number;
  job: Job;
  stop?: Stop | null;
  canSteer?: boolean;
  steerNote?: string;
}) {
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  const [done, setDone] = useState("");
  const [follow, setFollow] = useState(true);
  const box = useRef<HTMLDivElement>(null);
  const qc = useQueryClient();
  useEffect(() => {
    setEntries([]);
    setDone("");
    const es = new EventSource(`/api/v1/jobs/${jobId}/log`);
    let buf: TranscriptEntry[] = [];
    const flush = setInterval(() => {
      if (buf.length) {
        const add = buf;
        buf = [];
        setEntries((l) => (l.length + add.length > 5000 ? [...l, ...add].slice(-5000) : [...l, ...add]));
      }
    }, 150);
    // The server replays the log from the start on every (re)connect.
    es.onopen = () => {
      buf = [];
      setEntries([]);
    };
    es.addEventListener("line", (e) => {
      const d = JSON.parse((e as MessageEvent).data) as { entries?: TranscriptEntry[] };
      if (d.entries) buf.push(...d.entries);
    });
    es.addEventListener("done", (e) => {
      setDone((JSON.parse((e as MessageEvent).data) as { state: string }).state);
      es.close();
      void qc.invalidateQueries();
    });
    return () => {
      es.close();
      clearInterval(flush);
    };
  }, [jobId, qc]);
  const blocks = useMemo(() => fold(entries), [entries]);
  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [blocks, follow]);
  const live = !done;
  const outcome = [...blocks].reverse().find((b) => b.kind === "outcome") as { e: TranscriptEntry } | undefined;
  // What it's doing now: the last call still waiting on its result.
  let now = "";
  const last = blocks[blocks.length - 1];
  if (live && last?.kind === "tools") now = last.calls.filter((c) => !c.done).pop()?.summary ?? "";
  const steps = blocks.reduce((n, b) => n + (b.kind === "tools" ? b.calls.length : 0), 0);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="mb-1 flex items-center gap-2 text-xs text-muted-foreground">
        <span>{steps} steps</span>
        {done ? <span>· job {done.replace("_", " ")}</span> : <span className="animate-pulse">· live</span>}
        <a href={`/api/v1/jobs/${jobId}/log.jsonl`} download className="ml-auto inline-flex items-center gap-1 hover:text-foreground">
          <Download className="size-3" /> Raw log
        </a>
        <label className="flex items-center gap-1">
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
        </label>
      </div>
      <div
        ref={box}
        data-testid="transcript"
        className="min-h-64 flex-1 space-y-3 overflow-auto rounded-md border bg-card p-3"
        onScroll={(e) => {
          const el = e.currentTarget;
          setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 40);
        }}
      >
        {blocks.length === 0 && <p className="text-sm text-muted-foreground">{done ? "No transcript." : "Waiting for output…"}</p>}
        {blocks.map((b, i) => {
          switch (b.kind) {
            case "init":
              return (
                <p key={i} data-testid="session-header" className="font-mono text-xs text-muted-foreground">
                  {b.e.model === "codex" ? "Codex session" : `Session · ${b.e.model}`}
                  {b.e.tools > 0 && ` · ${b.e.tools} tools`}
                </p>
              );
            case "text":
              return (
                <div key={i} data-testid="narration">
                  <Markdown text={b.e.text} />
                </div>
              );
            case "thinking":
              return <Thinking key={i} text={b.e.text} />;
            case "you":
              return (
                <div key={i} data-testid="you" className="rounded-md bg-primary/10 p-2 text-sm">
                  <span className="font-medium">You: </span>
                  <span className="whitespace-pre-wrap">{b.e.text}</span>
                  {!b.e.delivered && <span className="text-xs text-muted-foreground"> (not delivered: it had finished)</span>}
                </div>
              );
            case "error":
              return (
                <p key={i} className="flex gap-1.5 text-sm text-destructive">
                  <OctagonX className="mt-0.5 size-4 shrink-0" /> {b.e.text}
                </p>
              );
            case "tools":
              return <ToolGroup key={i} calls={b.calls} live={live} />;
            case "outcome":
              // The banner below says how it ended; its last words are worth keeping.
              return b.e.text && !b.e.is_error ? (
                <div key={i} data-testid="final-message" className="border-l-2 border-success/50 pl-2">
                  <Markdown text={b.e.text} />
                </div>
              ) : null;
          }
          return null;
        })}
        {live && (
          <p data-testid="now" className="flex items-center gap-1.5 text-xs text-primary">
            <Loader2 className="size-3 animate-spin" /> {now ? `Now: ${now}` : "Working…"}
          </p>
        )}
        {done && <Outcome job={job} stop={stop} outcome={outcome?.e} state={done} />}
      </div>
      {live && (canSteer || steerNote) && <SteerBox jobId={jobId} can={canSteer} note={steerNote} />}
    </div>
  );
}
