import { useMemo, useState } from "react";
import { Link } from "react-router";
import { AlertTriangle, GitPullRequest, Hand } from "lucide-react";
import { useBoard, useOverview, type Card as CardT } from "@/api/client";
import { ErrorBox, Loading } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { cn, money, stageName } from "@/lib/utils";

// Stages hidden unless they hold something, so a phone sees what matters.
const quiet = new Set(["epic", "spec", "blocked", "done"]);

export function BoardScreen() {
  const { data, error, isLoading } = useBoard();
  const [filter, setFilter] = useState("");
  const [showDone, setShowDone] = useState(false);
  const [repo, setRepo] = useState("");
  const repos = useOverview().data?.repos ?? [];
  const columns = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return (data?.columns ?? [])
      .map((c) => ({
        ...c,
        cards: c.cards.filter(
          (k) => (!repo || k.repo === repo) && (!q || k.title.toLowerCase().includes(q) || k.ref.toLowerCase().includes(q)),
        ),
      }))
      .filter((c) => (c.stage === "done" ? showDone : !quiet.has(c.stage) || c.cards.length > 0));
  }, [data, filter, showDone, repo]);
  if (isLoading) return <Loading what="Loading the board" />;
  if (error) return <ErrorBox error={error} />;
  const total = data!.columns.reduce((n, c) => n + c.cards.length, 0);
  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <h1 className="text-lg font-semibold">Board</h1>
        <span className="text-sm text-muted-foreground">{total} items</span>
        <input
          aria-label="Filter items"
          placeholder="Filter by title or #issue"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="ml-auto h-8 w-full rounded-md border bg-card px-2.5 text-sm sm:w-64"
        />
        {repos.length > 1 && (
          <select
            aria-label="Repository"
            value={repo}
            onChange={(e) => setRepo(e.target.value)}
            className="h-8 rounded-md border bg-card px-2 text-sm"
          >
            <option value="">All repos</option>
            {repos.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        )}
        <label className="flex items-center gap-1.5 text-sm text-muted-foreground">
          <input type="checkbox" checked={showDone} onChange={(e) => setShowDone(e.target.checked)} /> Done
        </label>
      </div>
      {/* Columns scroll sideways on a desktop and stack on a phone. */}
      <div className="flex flex-col gap-4 md:flex-row md:overflow-x-auto md:pb-2" data-testid="board">
        {columns.map((col) => (
          <section
            key={col.stage}
            aria-label={stageName(col.stage)}
            // On a phone, what needs you comes first.
            className={cn("md:max-w-96 md:min-w-64 md:flex-1", (col.stage === "awaiting_human" || col.stage === "parked") && "max-md:order-first")}
          >
            <h2 className="mb-2 flex items-center gap-2 text-xs font-semibold tracking-wide text-muted-foreground uppercase">
              {stageName(col.stage)}
              <span className="rounded-full bg-muted px-1.5 font-normal normal-case">{col.cards.length}</span>
            </h2>
            <div className="flex flex-col gap-2">
              {col.cards.map((c) => (
                <BoardCard key={c.id} card={c} />
              ))}
              {col.cards.length === 0 && <div className="rounded-lg border border-dashed p-3 text-center text-xs text-muted-foreground">Nothing here</div>}
            </div>
          </section>
        ))}
      </div>
    </div>
  );
}

function BoardCard({ card: c }: { card: CardT }) {
  return (
    <Link
      to={`/items/${c.id}`}
      data-testid="card"
      className={cn(
        "block rounded-lg border bg-card p-3 text-sm shadow-xs transition-colors hover:border-primary/50",
        c.needs_you && "border-warning/60",
        c.stage === "blocked" && "border-destructive/60",
      )}
    >
      <div className="mb-1 flex items-start gap-2">
        <span className="font-mono text-xs text-muted-foreground">{c.ref}</span>
        <span className="line-clamp-2 flex-1 font-medium">{c.title}</span>
        {c.live && (
          <span className="relative mt-1 flex size-2 shrink-0" title="A job is running">
            <span className="absolute size-2 animate-ping rounded-full bg-primary opacity-60" />
            <span className="size-2 rounded-full bg-primary" />
          </span>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
        {c.needs_you && (
          <Badge variant="warning">
            <Hand className="size-3" /> needs you
          </Badge>
        )}
        {c.stage === "blocked" && (
          <Badge variant="destructive">
            <AlertTriangle className="size-3" /> blocked
          </Badge>
        )}
        {c.complexity && <Badge variant="secondary">{c.complexity}</Badge>}
        {c.model && <span>{c.model}</span>}
        {c.cost_usd > 0 && <span>{money(c.cost_usd)}</span>}
        {c.pr_number > 0 && (
          <span className="flex items-center gap-0.5">
            <GitPullRequest className="size-3" />
            {c.pr_number}
          </span>
        )}
      </div>
      {c.blocked_note && <p className="mt-1.5 line-clamp-2 text-xs text-muted-foreground">{c.blocked_note}</p>}
    </Link>
  );
}
