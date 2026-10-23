import { Link } from "react-router";
import { Bell, CircleDollarSign, GitPullRequest, Hand } from "lucide-react";
import { useNotifications } from "@/api/client";
import { Choices } from "@/components/Choices";
import { Empty, ErrorBox, Loading, PageTitle } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { usePermission } from "@/lib/notify";
import { ago } from "@/lib/utils";
import { setWorkspace, useScope } from "@/lib/workspace";

const icon = { needs_input: Hand, ready_for_review: GitPullRequest, budget: CircleDollarSign };

export function NotificationsScreen() {
  const { data, error, isLoading } = useNotifications();
  const [perm, request] = usePermission();
  // The workspace on screen's, and the factory's own; the rest are counted below so none hides.
  const { workspace, workspaces } = useScope();
  const all = data?.notifications ?? [];
  const mine = all.filter((n) => !n.workspace || workspaces.length < 2 || n.workspace === workspace);
  const elsewhere = new Map<string, number>();
  for (const n of all) if (n.workspace && workspaces.length > 1 && n.workspace !== workspace) elsewhere.set(n.workspace, (elsewhere.get(n.workspace) ?? 0) + 1);
  return (
    <div className="max-w-3xl">
      <PageTitle
        aside={
          perm === "default" ? (
            <Button size="sm" variant="outline" onClick={request}>
              <Bell /> Notify me in this browser
            </Button>
          ) : perm === "granted" ? (
            <span className="text-xs text-muted-foreground">Browser notifications are on while a dashboard tab is open.</span>
          ) : perm === "denied" ? (
            <span className="text-xs text-muted-foreground">Browser notifications are blocked for this site.</span>
          ) : null
        }
      >
        Inbox
      </PageTitle>
      {isLoading ? (
        <Loading />
      ) : error ? (
        <ErrorBox error={error} />
      ) : mine.length === 0 ? (
        <Empty>Nothing needs you {elsewhere.size > 0 ? "here" : "right now"}.</Empty>
      ) : (
        <ul className="space-y-2" data-testid="inbox">
          {mine.map((n) => {
            const Icon = icon[n.kind];
            const choose = n.choice_job_id != null && n.choices.some((c) => c.available);
            const title = <span className="font-medium">{n.title}</span>;
            const body = (
              <Card className="flex gap-3 p-3 transition-colors hover:border-primary/50">
                <Icon className={n.kind === "budget" ? "mt-0.5 size-5 shrink-0 text-warning" : "mt-0.5 size-5 shrink-0 text-primary"} />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-baseline gap-2">
                    {choose && n.item_id != null ? (
                      <Link to={`/items/${n.item_id}`} className="hover:underline">
                        {title}
                      </Link>
                    ) : (
                      title
                    )}
                    <span className="text-xs text-muted-foreground">{ago(n.at)}</span>
                  </div>
                  {n.detail && <p className="mt-0.5 text-sm break-words text-muted-foreground">{n.detail}</p>}
                  {choose && (
                    <div className="mt-2">
                      <Choices jobId={n.choice_job_id!} itemId={n.item_id ?? undefined} choices={n.choices} compact />
                    </div>
                  )}
                </div>
              </Card>
            );
            // A card with buttons can't sit inside a link; its title links instead.
            return <li key={n.id}>{n.item_id != null && !choose ? <Link to={`/items/${n.item_id}`}>{body}</Link> : body}</li>;
          })}
        </ul>
      )}
      {elsewhere.size > 0 && (
        <p className="mt-3 flex flex-wrap gap-x-3 gap-y-1 text-sm text-text-2" aria-label="In other workspaces">
          {[...elsewhere].map(([ws, n]) => (
            <span key={ws}>
              In <span className="font-mono">{ws}</span>, {n === 1 ? "1 more needs" : `${n} more need`} you.{" "}
              <button onClick={() => setWorkspace(ws)} className="cursor-pointer font-medium text-primary hover:underline">
                Switch to it
              </button>
            </span>
          ))}
        </p>
      )}
    </div>
  );
}
