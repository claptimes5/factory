import { useSearchParams, Link } from "react-router";
import { useFeed } from "@/api/client";
import { Empty, ErrorBox, Loading, PageTitle } from "@/components/common";
import { Card, CardContent } from "@/components/ui/card";
import { ago, cn } from "@/lib/utils";
import { ChatPanel } from "@/screens/Chat";

/** The orchestrator: a chat with its model, and the decisions it made on its own. */
export function OrchestratorScreen() {
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "decisions" ? "decisions" : "chat";
  const pick = (t: string) => {
    const p = new URLSearchParams();
    if (t === "decisions") p.set("tab", t);
    setParams(p);
  };
  return (
    <div>
      <PageTitle>Orchestrator</PageTitle>
      <div role="tablist" className="mb-4 flex gap-1 border-b">
        {[
          ["chat", "Chat"],
          ["decisions", "Decisions"],
        ].map(([t, label]) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => pick(t)}
            className={cn(
              "-mb-px border-b-2 px-3 py-1.5 text-sm",
              tab === t ? "border-primary font-medium" : "border-transparent text-muted-foreground hover:text-foreground",
            )}
          >
            {label}
          </button>
        ))}
      </div>
      {tab === "chat" ? <ChatPanel /> : <Decisions />}
    </div>
  );
}

export function Decisions() {
  const [params, setParams] = useSearchParams();
  const item = Number(params.get("item")) || undefined;
  const { data, error, isLoading } = useFeed(item);
  const types = [...new Set((data?.events ?? []).map((e) => e.type))].sort();
  const type = params.get("type") ?? "";
  const events = (data?.events ?? []).filter((e) => !type || e.type === type);
  const set = (k: string, v: string) => {
    const p = new URLSearchParams(params);
    if (v) p.set(k, v);
    else p.delete(k);
    setParams(p, { replace: true });
  };
  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <p className="text-sm text-muted-foreground">What the orchestrator did on its own, and why.</p>
        <div className="flex flex-wrap items-center gap-2 text-sm">
            <input
              aria-label="Item id"
              inputMode="numeric"
              placeholder="Item id"
              defaultValue={item ?? ""}
              onChange={(e) => set("item", e.target.value.replace(/\D/g, ""))}
              className="h-8 w-24 rounded-md border bg-card px-2"
            />
            <select aria-label="Decision type" value={type} onChange={(e) => set("type", e.target.value)} className="h-8 rounded-md border bg-card px-2">
              <option value="">All decisions</option>
              {types.map((t) => (
                <option key={t}>{t}</option>
              ))}
            </select>
        </div>
      </div>
      {isLoading ? (
        <Loading />
      ) : error ? (
        <ErrorBox error={error} />
      ) : events.length === 0 ? (
        <Empty>No orchestrator decisions{item ? ` for item ${item}` : ""} yet.</Empty>
      ) : (
        <Card>
          <CardContent className="pt-3">
            <ol className="divide-y" data-testid="feed">
              {events.map((e) => (
                <li key={e.id} className="flex flex-col gap-0.5 py-2.5 sm:flex-row sm:gap-4">
                  <span className="w-20 shrink-0 text-xs text-muted-foreground" title={e.at}>
                    {ago(e.at)}
                  </span>
                  <span className="w-32 shrink-0 font-mono text-xs">{e.type}</span>
                  <span className="min-w-0 flex-1 text-sm">
                    {e.reason || <span className="text-muted-foreground">(no reason given)</span>}
                    {e.data && e.data !== "{}" && <code className="ml-2 text-xs break-all text-muted-foreground">{e.data}</code>}
                  </span>
                  {e.item_id != null && (
                    <Link to={`/items/${e.item_id}`} className="shrink-0 text-xs text-primary hover:underline">
                      item {e.item_id}
                    </Link>
                  )}
                </li>
              ))}
            </ol>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
