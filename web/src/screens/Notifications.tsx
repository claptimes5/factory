import { Link } from "react-router-dom";
import { Bell, CircleDollarSign, GitPullRequest, Hand } from "lucide-react";
import { useNotifications } from "@/api/client";
import { Empty, ErrorBox, Loading, PageTitle } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { usePermission } from "@/lib/notify";
import { ago } from "@/lib/utils";

const icon = { needs_input: Hand, ready_for_review: GitPullRequest, budget: CircleDollarSign };

export function NotificationsScreen() {
  const { data, error, isLoading } = useNotifications();
  const [perm, request] = usePermission();
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
      ) : data!.notifications.length === 0 ? (
        <Empty>Nothing needs you right now.</Empty>
      ) : (
        <ul className="space-y-2" data-testid="inbox">
          {data!.notifications.map((n) => {
            const Icon = icon[n.kind];
            const body = (
              <Card className="flex gap-3 p-3 transition-colors hover:border-primary/50">
                <Icon className={n.kind === "budget" ? "mt-0.5 size-5 shrink-0 text-warning" : "mt-0.5 size-5 shrink-0 text-primary"} />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-baseline gap-2">
                    <span className="font-medium">{n.title}</span>
                    <span className="text-xs text-muted-foreground">{ago(n.at)}</span>
                  </div>
                  {n.detail && <p className="mt-0.5 text-sm break-words text-muted-foreground">{n.detail}</p>}
                </div>
              </Card>
            );
            return <li key={n.id}>{n.item_id != null ? <Link to={`/items/${n.item_id}`}>{body}</Link> : body}</li>;
          })}
        </ul>
      )}
    </div>
  );
}
