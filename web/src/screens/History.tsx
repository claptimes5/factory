import { Link, useSearchParams } from "react-router";
import { useBoard, useOverview } from "@/api/client";
import { ActivityFeed, WeekStats } from "@/components/Activity";
import { Empty, ErrorBox, Loading, PageTitle } from "@/components/common";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useRepo } from "@/lib/repo";
import { ago, money, shortRef } from "@/lib/utils";
import { Decisions } from "@/screens/Orchestrator";

// History: what happened, for when you want to look back. Home has what
// needs you now; this has the record: every event, what's done, and what
// the orchestrator decided on its own.

const tabs = ["activity", "done", "decisions"] as const;

export function HistoryScreen() {
  const [params, setParams] = useSearchParams();
  const tab = tabs.find((t) => t === params.get("tab")) ?? "activity";
  const repo = useRepo(useOverview().data?.repos ?? []);
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4" data-testid="history">
      <PageTitle>History</PageTitle>
      <Tabs value={tab} onValueChange={(t) => setParams(t === "activity" ? {} : { tab: t }, { replace: true })}>
        <TabsList>
          <TabsTrigger value="activity">Activity</TabsTrigger>
          <TabsTrigger value="done">Done</TabsTrigger>
          <TabsTrigger value="decisions">Orchestrator's decisions</TabsTrigger>
        </TabsList>
        <TabsContent value="activity" className="flex flex-col gap-5">
          <WeekStats />
          <ActivityFeed repo={repo} />
        </TabsContent>
        <TabsContent value="done">
          <Done repo={repo} />
        </TabsContent>
        <TabsContent value="decisions">
          <Decisions />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function Done({ repo }: { repo: string }) {
  const { data, error, isLoading } = useBoard();
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const done = (data?.columns.find((c) => c.stage === "done")?.cards ?? [])
    .filter((c) => !repo || c.repo === repo)
    .sort((a, b) => b.updated_at.localeCompare(a.updated_at));
  if (done.length === 0) return <Empty>Nothing done yet.</Empty>;
  return (
    <ul className="divide-y rounded-card border bg-surface" aria-label="Done">
      {done.map((c) => (
        <li key={c.id}>
          <Link to={`/items/${c.id}`} className="flex items-center gap-3 px-4 py-3 hover:bg-accent">
            <span className="font-mono text-caption text-success">{shortRef(c.ref)}</span>
            <span className="min-w-0 flex-1 truncate text-body-sm">{c.title}</span>
            {c.cost_usd > 0 && <span className="font-mono text-caption text-muted-foreground">{money(c.cost_usd)}</span>}
            <span className="shrink-0 text-caption text-muted-foreground">{ago(c.updated_at)}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}
