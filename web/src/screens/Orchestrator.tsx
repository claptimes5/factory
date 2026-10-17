import { useSearchParams, Link } from "react-router";
import { useFeed } from "@/api/client";
import { Empty, ErrorBox, Loading, PageTitle } from "@/components/common";
import { Card, CardContent } from "@/components/ui/card";
import { ago } from "@/lib/utils";

export function OrchestratorScreen() {
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
      <PageTitle
        aside={
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
        }
      >
        Orchestrator decisions
      </PageTitle>
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
