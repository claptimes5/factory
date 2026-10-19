import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ExternalLink, Loader2, MessageSquarePlus, Pencil, Send } from "lucide-react";
import { act, APIError, getJSON, postJSON } from "@/api/client";
import type { components } from "@/api/schema";
import { ErrorBox, Loading } from "@/components/common";
import { Markdown } from "@/components/Markdown";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { ago, cn, money } from "@/lib/utils";

type Chats = components["schemas"]["Chats"];
type ChatT = components["schemas"]["Chat"];
type Draft = components["schemas"]["Draft"];
type Sent = components["schemas"]["SentChat"];

const writing = (st?: string) => st === "queued" || st === "running";

const useChats = () => useQuery({ queryKey: ["chats"], queryFn: () => getJSON<Chats>("/chats") });

/** One chat. While its reply is written it polls, since drafts land mid-run. */
const useChat = (id: number) =>
  useQuery({
    queryKey: ["chat", id],
    queryFn: () => getJSON<ChatT>(`/chats/${id}`),
    enabled: id > 0,
    refetchInterval: (q) => (writing(q.state.data?.reply?.state) ? 2_000 : 30_000),
  });

/** The chat with the orchestrator's model: it drafts issues, and a person creates them. */
export function ChatPanel() {
  const [params, setParams] = useSearchParams();
  const chatId = Number(params.get("chat")) || 0;
  const { data, error, isLoading } = useChats();
  const open = (id: number) => {
    const p = new URLSearchParams(params);
    if (id) p.set("chat", String(id));
    else p.delete("chat");
    setParams(p);
  };
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const info = data!;
  return (
    <div className="space-y-4">
      <About info={info} />
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[14rem_minmax(0,1fr)]">
        <nav aria-label="Chats" className="min-w-0 space-y-1">
          <Button variant={chatId ? "outline" : "default"} size="sm" className="w-full justify-start" onClick={() => open(0)}>
            <MessageSquarePlus /> New chat
          </Button>
          <ul className="flex gap-1 overflow-x-auto lg:block lg:space-y-0.5" data-testid="chat-list">
            {info.chats.map((c) => (
              <li key={c.id} className="shrink-0">
                <button
                  onClick={() => open(c.id)}
                  className={cn(
                    "w-full max-w-56 truncate rounded-md px-2 py-1.5 text-left text-sm hover:bg-accent",
                    c.id === chatId && "bg-accent font-medium",
                  )}
                  title={c.title}
                >
                  {c.title}
                  <span className="block text-xs font-normal text-muted-foreground">{ago(c.updated_at)}</span>
                </button>
              </li>
            ))}
          </ul>
        </nav>
        <Conversation key={chatId} chatId={chatId} info={info} onStarted={open} />
      </div>
    </div>
  );
}

/** What chat is, what it costs, and how it relates to the orchestrator running on its own. */
function About({ info }: { info: Chats }) {
  return (
    <Card>
      <CardContent className="space-y-1.5 pt-4 text-sm">
        <p>
          Chat with <span className="font-medium">{modelName(info.model)}</span> about what you want built. It reads your code and the
          factory's work, then drafts issues. Nothing is filed until you press <span className="font-medium">Create issue</span> on a
          draft; the factory then builds it like any issue you label.
        </p>
        <p className="text-muted-foreground">
          {info.orchestrator_on ? (
            <>The orchestrator also runs on its own every {every(info.orchestrator_every)}, splitting epics and unsticking work. </>
          ) : (
            <>
              The orchestrator doesn't run on its own. Turn it on in <Link to="/settings" className="text-primary hover:underline">Settings</Link>{" "}
              (Orchestrator, On) to have it split epics and unstick work on a schedule; chat works either way.{" "}
            </>
          )}
          Each reply may spend up to {money(info.run_budget_usd)}; {money(info.spent_today_usd)} of today's {money(info.daily_budget_usd)} for
          orchestration is spent{info.plan && <> (at API prices: on your plan, its usage windows are what limit it)</>}.
        </p>
        {!info.available && (
          <p className="flex items-start gap-1.5 text-warning" data-testid="chat-unavailable">
            <AlertTriangle className="mt-0.5 size-4 shrink-0" /> {info.unavailable}
          </p>
        )}
      </CardContent>
    </Card>
  );
}

const modelName = (m: string) => (m ? m.charAt(0).toUpperCase() + m.slice(1) : "the orchestrator's model");

/** "1h0m0s" as "1h", "30m0s" as "30m". */
const every = (d: string) => d.replace(/(\d+[hm])0s$/, "$1").replace(/h0m$/, "h");

function Conversation({ chatId, info, onStarted }: { chatId: number; info: Chats; onStarted: (id: number) => void }) {
  const qc = useQueryClient();
  const { data, error, isLoading } = useChat(chatId);
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const [msg, setMsg] = useState("");
  const end = useRef<HTMLDivElement>(null);
  const turns = data?.messages.length ?? 0;
  useEffect(() => end.current?.scrollIntoView?.({ block: "nearest" }), [turns]);
  const reply = data?.reply;
  const busy = writing(reply?.state);
  const send = async (again = false) => {
    setSending(true);
    setMsg("");
    try {
      const r = await postJSON<Sent>("/chats", { chat_id: chatId, text: again ? "" : text });
      if (!again) setText("");
      await qc.invalidateQueries({ queryKey: ["chats"] });
      await qc.invalidateQueries({ queryKey: ["chat", r.chat_id] });
      if (r.chat_id !== chatId) onStarted(r.chat_id);
    } catch (e) {
      setMsg(e instanceof APIError ? e.message : String(e));
    } finally {
      setSending(false);
    }
  };
  if (chatId && isLoading) return <Loading />;
  if (chatId && error) return <ErrorBox error={error} />;
  const drafts = data?.drafts ?? [];
  return (
    <div className="min-w-0 space-y-4">
      <Card>
        <CardContent className="space-y-3 pt-4" data-testid="conversation">
          {turns === 0 && (
            <div className="space-y-2 text-sm text-muted-foreground">
              <p>Say what you want, as you would to a teammate. For example:</p>
              <ul className="list-disc space-y-1 pl-5">
                <li>Add CSV export to the invoices page, and split it into issues.</li>
                <li>Users say login is slow. Look at the code and draft an issue for the likely cause.</li>
                <li>What's stuck right now, and why?</li>
              </ul>
            </div>
          )}
          {data?.messages.map((m) =>
            m.role === "user" ? (
              <div key={m.id} className="ml-auto max-w-[85%] rounded-lg bg-primary/10 px-3 py-2 text-sm whitespace-pre-wrap" data-testid="you">
                {m.text}
              </div>
            ) : (
              <div key={m.id} className="max-w-[95%] space-y-1" data-testid="reply">
                <Markdown text={m.text} />
                {m.job_id != null && (
                  <Link to={`/jobs/${m.job_id}`} className="text-xs text-muted-foreground hover:underline">
                    How it got there
                  </Link>
                )}
              </div>
            ),
          )}
          {reply && busy && (
            <p className="flex items-center gap-2 text-sm text-muted-foreground" data-testid="thinking">
              <Loader2 className="size-4 animate-spin" />
              {reply.state === "queued" ? "Waiting to start…" : "Reading and writing…"}
              <Link to={`/jobs/${reply.job_id}`} className="text-primary hover:underline">
                Watch it work
              </Link>
            </p>
          )}
          {reply && !busy && (
            <div className="space-y-2 rounded-md border border-destructive/40 p-3 text-sm" data-testid="reply-failed">
              <p>
                {reply.state === "cancelled" ? "The reply was stopped" : "The reply failed"}
                {reply.error && <>: {reply.error}</>}.{" "}
                <Link to={`/jobs/${reply.job_id}`} className="text-primary hover:underline">
                  See what happened
                </Link>
              </p>
              <Button size="sm" variant="outline" disabled={sending || !info.available} onClick={() => void send(true)}>
                Try again
              </Button>
            </div>
          )}
          <div ref={end} />
        </CardContent>
      </Card>
      {drafts.length > 0 && (
        <section className="space-y-2" aria-label="Drafted issues">
          <h2 className="text-sm font-medium">Drafted issues</h2>
          {drafts.map((d) => (
            <DraftCard key={d.id} d={d} chatId={chatId} />
          ))}
        </section>
      )}
      <form
        className="space-y-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (text.trim()) void send();
        }}
      >
        <textarea
          aria-label="Message"
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              if (text.trim() && !busy && !sending) void send();
            }
          }}
          rows={3}
          placeholder={busy ? "Wait for the reply…" : "Describe what you want built, or ask about the factory's work"}
          className="w-full rounded-md border bg-card px-3 py-2 text-sm"
        />
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" size="sm" disabled={!text.trim() || busy || sending || !info.available}>
            {sending ? <Loader2 className="animate-spin" /> : <Send />} Send
          </Button>
          <span className="text-xs text-muted-foreground">Enter sends; Shift+Enter starts a new line.</span>
          {msg && (
            <span role="alert" className="text-sm text-destructive">
              {msg}
            </span>
          )}
        </div>
      </form>
    </div>
  );
}

function DraftCard({ d, chatId }: { d: Draft; chatId: number }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState(d.title);
  const [body, setBody] = useState(d.body);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");
  const done = () => qc.invalidateQueries({ queryKey: ["chat", chatId] });
  const create = async () => {
    setBusy(true);
    setMsg("");
    try {
      await postJSON<Draft>(`/drafts/${d.id}/create`, editing ? { title, body } : {});
      setEditing(false);
    } catch (e) {
      setMsg(e instanceof APIError ? e.message : String(e));
    } finally {
      setBusy(false);
      await done();
    }
  };
  const dismiss = async () => {
    setBusy(true);
    const r = await act(`/drafts/${d.id}/dismiss`);
    setBusy(false);
    if (!r.ok) setMsg(r.message);
    await done();
  };
  if (d.state === "dismissed") {
    return (
      <p className="px-1 text-sm text-muted-foreground line-through" data-testid="draft-dismissed">
        {d.title}
      </p>
    );
  }
  return (
    <Card data-testid="draft">
      <CardContent className="space-y-2 pt-4">
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="outline">{d.repo}</Badge>
          {d.labels.map((l) => (
            <Badge key={l} variant="secondary">
              {l}
            </Badge>
          ))}
          {d.state === "created" && <Badge variant="success">Created</Badge>}
          {d.state === "creating" && <Badge variant="warning">Creating…</Badge>}
        </div>
        {editing ? (
          <div className="space-y-2">
            <input
              aria-label="Title"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              className="h-9 w-full rounded-md border bg-card px-2 text-sm font-medium"
            />
            <textarea
              aria-label="Body"
              value={body}
              onChange={(e) => setBody(e.target.value)}
              rows={10}
              className="w-full rounded-md border bg-card px-2 py-1.5 font-mono text-xs"
            />
          </div>
        ) : (
          <>
            <p className="font-medium">{d.title}</p>
            <details open={d.state === "draft"}>
              <summary className="cursor-pointer text-xs text-muted-foreground">Description</summary>
              <div className="mt-1">
                <Markdown text={d.body} />
              </div>
            </details>
          </>
        )}
        {(d.depends_on.length > 0 || d.parent != null) && (
          <p className="text-xs text-muted-foreground">
            {d.parent != null && (
              <>
                Part of{" "}
                <Link to={`/items/${d.parent}`} className="text-primary hover:underline">
                  item {d.parent}
                </Link>
                .{" "}
              </>
            )}
            {d.depends_on.length > 0 && (
              <>
                Waits for{" "}
                {d.depends_on.map((id, i) => (
                  <span key={id}>
                    {i > 0 && ", "}
                    <Link to={`/items/${id}`} className="text-primary hover:underline">
                      item {id}
                    </Link>
                  </span>
                ))}
                .
              </>
            )}
          </p>
        )}
        {d.state === "created" ? (
          <p className="flex flex-wrap items-center gap-3 text-sm">
            {d.issue_url ? (
              <a href={d.issue_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">
                Issue #{d.issue_number} <ExternalLink className="size-3.5" />
              </a>
            ) : (
              <span>Issue #{d.issue_number}</span>
            )}
            {d.item_id != null && (
              <Link to={`/items/${d.item_id}`} className="text-primary hover:underline">
                Follow it on the board
              </Link>
            )}
          </p>
        ) : (
          d.state === "draft" && (
            <div className="flex flex-wrap items-center gap-2">
              <Button size="sm" disabled={busy || (editing && (!title.trim() || !body.trim()))} onClick={() => void create()}>
                {busy && <Loader2 className="animate-spin" />} Create issue
              </Button>
              {!editing && (
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => {
                    setTitle(d.title);
                    setBody(d.body);
                    setEditing(true);
                  }}
                >
                  <Pencil /> Edit
                </Button>
              )}
              {editing && (
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy}
                  onClick={() => {
                    setEditing(false);
                    setTitle(d.title);
                    setBody(d.body);
                  }}
                >
                  Cancel edit
                </Button>
              )}
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => void dismiss()}>
                Dismiss
              </Button>
            </div>
          )
        )}
        {(msg || d.error) && (
          <p role="alert" className="text-sm text-destructive">
            {msg || `Creating it failed: ${d.error}`}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
