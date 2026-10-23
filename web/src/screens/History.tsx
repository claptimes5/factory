import { Link, useSearchParams } from "react-router";
import { type Comparison, useBoard, useCompare, useOverview } from "@/api/client";
import { ActivityFeed, WeekStats } from "@/components/Activity";
import { Empty, ErrorBox, Loading, PageTitle } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { FoundBy } from "@/components/FoundBy";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useRepo } from "@/lib/repo";
import { ago, money, shortRef, shortSha } from "@/lib/utils";
import { Decisions } from "@/screens/Orchestrator";

// History: what happened, for when you want to look back. Home has what
// needs you now; this has the record: every event, what's done, and what
// the orchestrator decided on its own, and the `factory compare` runs.

const tabs = ["activity", "done", "decisions", "compare"] as const;

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
          <TabsTrigger value="compare">Comparisons</TabsTrigger>
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
        <TabsContent value="compare" className="flex flex-col gap-4">
          <Comparisons />
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
            <span className="flex min-w-0 flex-1 flex-col">
              <span className="truncate text-body-sm">{c.title}</span>
              <FoundBy source={c.self_started} className="text-muted-foreground" />
            </span>
            {c.cost_usd > 0 && <span className="font-mono text-caption text-muted-foreground">{money(c.cost_usd)}</span>}
            <span className="shrink-0 text-caption text-muted-foreground">{ago(c.updated_at)}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

function Comparisons() {
  const { data, error, isLoading } = useCompare();
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  if (!data?.runs.length)
    return (
      <Empty>
        No comparisons yet. <code className="font-mono">factory compare ISSUE -skills a,b</code> builds an issue once per
        skill set and puts the results here.
      </Empty>
    );
  return data.runs.map((r) => <ComparisonCard key={r.id} run={r} />);
}

// One run, side by side as `factory compare` writes it: a column per skill set.
function ComparisonCard({ run }: { run: Comparison }) {
  const rows: [string, (a: Comparison["arms"][number]) => React.ReactNode][] = [
    [
      "Outcome",
      (a) =>
        a.stage === "awaiting_human" ? (
          <Badge variant="success">passed review</Badge>
        ) : (
          <Badge variant="secondary">{a.stage}</Badge>
        ),
    ],
    ["Review", (a) => `${a.review || "–"} (${a.findings.length} findings)`],
    [
      "Gates",
      (a) => {
        if (a.gates.length === 0) return "–";
        const bad = a.gates.filter((g) => g.status !== "pass");
        return bad.length ? bad.map((g) => `${g.gate}=${g.status}`).join(", ") : "all pass";
      },
    ],
    ["Implement jobs", (a) => a.implement_jobs],
    ["Cost", (a) => money(a.cost_usd)],
    ["Time", (a) => `${Math.round(a.seconds)}s`],
    ["Diff", (a) => a.diff_stat.trim().split("\n").pop() || "–"],
    ["Branch", (a) => <code className="font-mono">{a.branch || "–"}</code>],
  ];
  const notes = run.arms.filter((a) => a.error || a.findings.length);
  return (
    <section className="rounded-card border bg-surface" aria-label={`Comparison ${run.ref}`}>
      <header className="flex flex-col gap-1 border-b px-4 py-3">
        <h2 className="text-body font-medium">
          <span className="font-mono text-muted-foreground">{shortRef(run.ref)}</span> {run.title}
        </h2>
        <p className="text-caption text-muted-foreground">
          {ago(run.started)} · base <span className="font-mono">{shortSha(run.base_sha)}</span>
          {run.complexity && ` · complexity ${run.complexity}`}
        </p>
      </header>
      <div className="overflow-x-auto">
        <table className="w-full text-body-sm">
          <thead>
            <tr className="border-b">
              <th className="px-4 py-2" />
              {run.arms.map((a) => (
                <th key={a.skillset} scope="col" className="px-4 py-2 text-left font-medium">
                  {a.skillset}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map(([label, cell]) => (
              <tr key={label} className="border-b last:border-0">
                <th scope="row" className="px-4 py-2 text-left font-normal whitespace-nowrap text-muted-foreground">
                  {label}
                </th>
                {run.arms.map((a) => (
                  <td key={a.skillset} className="px-4 py-2 align-top">
                    {cell(a)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {notes.length > 0 && (
        <div className="flex flex-col gap-3 border-t px-4 py-3">
          {notes.map((a) => (
            <div key={a.skillset} className="flex flex-col gap-1 text-body-sm">
              <span className="font-medium">{a.skillset}</span>
              {a.error && <p className="text-destructive">Error: {a.error}</p>}
              {a.findings.length > 0 && (
                <ul className="list-disc pl-5">
                  {a.findings.map((f, i) => (
                    <li key={i}>{f}</li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
