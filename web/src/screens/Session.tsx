import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useItem, useJob } from "@/api/client";
import { Actions, Empty, ErrorBox, GateList, JobStateBadge, Loading } from "@/components/common";
import { Terminal } from "@/components/Terminal";
import { Transcript } from "@/components/Transcript";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn, duration, money } from "@/lib/utils";

export function SessionScreen() {
  const id = Number(useParams().id);
  const { data, error, isLoading } = useJob(id);
  const itemId = data?.job.item_id ?? 0;
  const item = useItem(itemId);
  const [tab, setTab] = useState<"transcript" | "terminal" | "issue">("transcript");
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const { job: j, gates, can_attach } = data!;
  const running = j.state === "running";
  type Tab = "transcript" | "terminal" | "issue";
  const tabs: [Tab, string][] = [["transcript", "Transcript"], ...(can_attach ? [["terminal", "Terminal"] as [Tab, string]] : []), ["issue", "Issue & spec"]];
  return (
    <div className="flex flex-col gap-4">
      <div>
        <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
          <Link to="/" className="hover:text-foreground">
            Board
          </Link>
          {itemId > 0 && (
            <>
              <span>/</span>
              <Link to={`/items/${itemId}`} className="hover:text-foreground">
                #{item.data?.item.issue ?? itemId} {item.data?.item.title}
              </Link>
            </>
          )}
        </div>
        <h1 className="mt-1 flex flex-wrap items-center gap-2 text-lg font-semibold">
          {j.kind} job #{j.id} <JobStateBadge state={j.state} />
        </h1>
        <p className="text-sm text-muted-foreground">
          {j.harness}/{j.model}
          {j.effort && ` · ${j.effort}`} · attempt {j.attempt} · {money(j.cost_usd)} of {money(j.budget_usd)}
          {j.started_at && ` · ${duration(j.started_at, j.finished_at || undefined)}`}
          {j.skillset && ` · skills: ${j.skillset}`}
        </p>
        {j.error && <p className="mt-1 text-sm text-destructive">{j.error}</p>}
        <div className="mt-2">
          <Actions jobId={j.id} itemId={itemId || undefined} running={running} />
        </div>
      </div>
      {/* Phone: one pane at a time. */}
      <div role="tablist" className="flex gap-1 xl:hidden">
        {tabs.map(([k, label]) => (
          <button
            key={k}
            role="tab"
            aria-selected={tab === k}
            onClick={() => setTab(k)}
            className={cn("cursor-pointer rounded-md px-3 py-1.5 text-sm", tab === k ? "bg-accent font-medium" : "text-muted-foreground")}
          >
            {label}
          </button>
        ))}
      </div>
      <div className={cn("grid gap-4", can_attach ? "xl:grid-cols-[320px_1fr_1fr]" : "xl:grid-cols-[360px_1fr]")}>
        <div className={cn("min-w-0 space-y-4", tab !== "issue" && "hidden xl:block")}>
          <Card>
            <CardHeader>
              <CardTitle>Issue</CardTitle>
            </CardHeader>
            <CardContent className="max-h-[50vh] overflow-auto">
              {item.data?.issue_body ? <p className="text-sm whitespace-pre-wrap">{item.data.issue_body}</p> : <Empty>No issue text.</Empty>}
            </CardContent>
          </Card>
          {item.data?.spec && (
            <Card>
              <CardHeader>
                <CardTitle>Spec</CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-sm whitespace-pre-wrap">{item.data.spec}</p>
              </CardContent>
            </Card>
          )}
          <Card>
            <CardHeader>
              <CardTitle>Gates</CardTitle>
            </CardHeader>
            <CardContent>
              <GateList gates={gates} />
            </CardContent>
          </Card>
        </div>
        <div className={cn("flex h-[70vh] min-w-0 flex-col", tab !== "transcript" && "hidden xl:flex")}>
          <Transcript jobId={j.id} />
        </div>
        {can_attach && (
          <div className={cn("flex h-[70vh] min-w-0 flex-col", tab !== "terminal" && "hidden xl:flex")}>
            <Terminal jobId={j.id} />
          </div>
        )}
      </div>
    </div>
  );
}
