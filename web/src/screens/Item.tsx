import { Link, useParams } from "react-router";
import { ExternalLink } from "lucide-react";
import { useItem, type Event } from "@/api/client";
import { Empty, ErrorBox, JobStateBadge, Loading, StageBadge } from "@/components/common";
import { PRPanel } from "@/components/PRPanel";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ago, duration, money } from "@/lib/utils";

export function ItemScreen() {
  const id = Number(useParams().id);
  const { data, error, isLoading } = useItem(id);
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const { item: it, jobs, events, depends_on, issue_body, spec } = data!;
  const live = jobs.find((j) => j.state === "running");
  return (
    <div className="space-y-4">
      <div>
        <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
          <Link to="/" className="hover:text-foreground">
            Floor
          </Link>
          <span>/</span>
          <a href={it.issue_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 hover:text-foreground">
            {it.ref} <ExternalLink className="size-3" />
          </a>
        </div>
        <h1 className="mt-1 flex flex-wrap items-center gap-2 font-display text-xl font-semibold tracking-tight">
          {it.title} <StageBadge stage={it.stage} />
        </h1>
        {it.blocked_note && <p className="mt-1 rounded-md bg-warning/10 p-2 text-sm text-warning">{it.blocked_note}</p>}
        {live && (
          <Link to={`/jobs/${live.id}`} className="mt-2 inline-flex items-center gap-2 text-sm text-primary hover:underline">
            <span className="size-2 animate-pulse rounded-full bg-primary" /> Watch the running {live.kind} job
          </Link>
        )}
      </div>
      <div className="grid gap-4 lg:grid-cols-[1fr_420px]">
        <div className="min-w-0 space-y-4">
          <Card>
            <CardHeader>
              <CardTitle>Issue</CardTitle>
            </CardHeader>
            <CardContent>
              {issue_body ? <p className="text-sm whitespace-pre-wrap">{issue_body}</p> : <Empty>The issue text could not be read.</Empty>}
              {spec && (
                <div className="mt-3 rounded-md bg-muted p-3 text-sm">
                  <div className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Spec (triage)</div>
                  <p className="whitespace-pre-wrap">{spec}</p>
                </div>
              )}
              <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-4">
                <Meta k="Size" v={it.complexity || "–"} />
                <Meta k="Risk" v={it.risk || "–"} />
                <Meta k="Priority" v={String(it.priority)} />
                <Meta k="Branch" v={it.branch || "–"} mono />
                {depends_on.length > 0 && (
                  <div className="col-span-2 sm:col-span-4">
                    <dt className="text-muted-foreground">Depends on</dt>
                    <dd className="flex gap-2">
                      {depends_on.map((d) => (
                        <Link key={d} to={`/items/${d}`} className="text-primary hover:underline">
                          item {d}
                        </Link>
                      ))}
                    </dd>
                  </div>
                )}
              </dl>
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Jobs</CardTitle>
            </CardHeader>
            <CardContent>
              {jobs.length === 0 ? (
                <Empty>No jobs yet.</Empty>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead className="text-left text-xs text-muted-foreground">
                      <tr>
                        <th className="py-1 pr-3 font-medium">Job</th>
                        <th className="pr-3 font-medium">State</th>
                        <th className="pr-3 font-medium">Model</th>
                        <th className="pr-3 font-medium">Cost</th>
                        <th className="font-medium">Time</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y">
                      {[...jobs].reverse().map((j) => (
                        <tr key={j.id}>
                          <td className="py-1.5 pr-3">
                            <Link to={`/jobs/${j.id}`} className="text-primary hover:underline">
                              {j.kind} #{j.id}
                            </Link>
                            {j.attempt > 1 && <span className="ml-1 text-xs text-muted-foreground">try {j.attempt}</span>}
                          </td>
                          <td className="pr-3">
                            <JobStateBadge state={j.state} />
                          </td>
                          <td className="pr-3 text-xs">
                            {j.harness}/{j.model}
                          </td>
                          <td className="pr-3">{money(j.cost_usd)}</td>
                          <td className="text-xs text-muted-foreground">{j.started_at ? duration(j.started_at, j.finished_at || undefined) : "queued"}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Audit trail</CardTitle>
              <Link to={`/orchestrator?item=${it.id}`} className="text-xs text-primary hover:underline">
                Orchestrator decisions
              </Link>
            </CardHeader>
            <CardContent>
              <Trail events={events} />
            </CardContent>
          </Card>
        </div>
        <div className="min-w-0">
          <PRPanel itemId={it.id} needsYou={it.stage === "awaiting_human"} />
        </div>
      </div>
    </div>
  );
}

function Meta({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div>
      <dt className="text-muted-foreground">{k}</dt>
      <dd className={mono ? "truncate font-mono" : ""}>{v}</dd>
    </div>
  );
}

export function Trail({ events }: { events: Event[] }) {
  if (events.length === 0) return <Empty>Nothing recorded yet.</Empty>;
  return (
    <ol className="space-y-2 text-sm">
      {[...events].reverse().map((e) => (
        <li key={e.id} className="flex gap-3">
          <span className="w-16 shrink-0 text-xs text-muted-foreground" title={e.at}>
            {ago(e.at)}
          </span>
          <span className="w-24 shrink-0 truncate text-xs font-medium">{e.actor}</span>
          <span className="min-w-0">
            <span className="font-mono text-xs">{e.type}</span>
            {e.reason && <span className="text-muted-foreground"> · {e.reason}</span>}
          </span>
        </li>
      ))}
    </ol>
  );
}
