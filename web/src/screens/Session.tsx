import { useState } from "react";
import { Link, useParams } from "react-router";
import { useItem, useJob, useSavedDiff, type Stop } from "@/api/client";
import { Choices } from "@/components/Choices";
import { Actions, Empty, ErrorBox, GateList, JobStateBadge, Loading } from "@/components/common";
import { DiffView } from "@/components/DiffView";
import { Markdown } from "@/components/Markdown";
import { PlanCostNote } from "@/components/PlanUsage";
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
  const { job: j, gates, can_attach, stop, choices, can_steer, steer_note } = data!;
  const running = j.state === "running";
  type Tab = "transcript" | "terminal" | "issue";
  const tabs: [Tab, string][] = [["transcript", "Transcript"], ...(can_attach ? [["terminal", "Terminal"] as [Tab, string]] : []), ["issue", "Issue & spec"]];
  return (
    <div className="flex flex-col gap-4">
      <div>
        <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
          <Link to="/" className="hover:text-foreground">
            Floor
          </Link>
          {itemId > 0 && (
            <>
              <span>/</span>
              <Link to={`/items/${itemId}`} className="hover:text-foreground">
                {item.data?.item.ref ?? `#${itemId}`} {item.data?.item.title}
              </Link>
            </>
          )}
        </div>
        <h1 className="mt-1 flex flex-wrap items-center gap-2 font-display text-xl font-semibold tracking-tight">
          {j.kind} job #{j.id} <JobStateBadge state={stop ? "stopped" : j.state} />
        </h1>
        <p className="text-sm text-muted-foreground">
          {j.harness}/{j.model}
          {j.effort && ` · ${j.effort}`} · attempt {j.attempt} · {money(j.cost_usd)} of {money(j.budget_usd)}
          {j.started_at && ` · ${duration(j.started_at, j.finished_at || undefined)}`}
          {j.skillset && ` · skills: ${j.skillset}`}
        </p>
        <PlanCostNote harnesses={[j.harness]} className="mt-1" />
        {j.error && !stop && <p className="mt-1 text-sm text-destructive">{j.error}</p>}
        {stop && <StopPanel jobId={j.id} stop={stop} />}
        <div className="mt-2">
          {!running && choices.length > 0 ? (
            <Choices jobId={j.id} itemId={itemId || undefined} choices={choices} />
          ) : (
            <Actions jobId={j.id} itemId={itemId || undefined} running={running} />
          )}
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
          <Transcript jobId={j.id} job={j} stop={stop} canSteer={can_steer} steerNote={steer_note} />
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

/** A build that ran out of turns or budget: what happened, its note, and the work it kept. */
function StopPanel({ jobId, stop }: { jobId: number; stop: Stop }) {
  const [open, setOpen] = useState(false);
  const diff = useSavedDiff(jobId, open && stop.saved_diff);
  return (
    <div data-testid="stop" className="mt-2 space-y-2 rounded-md border border-warning/50 bg-warning/10 p-3 text-sm">
      <p className="font-medium">{stop.summary}</p>
      <p className="text-muted-foreground">
        {stop.kept
          ? "Its work is kept on the factory's machine, and nothing was pushed. It doesn't restart on its own: running out again is likely unless something changes. Continue picks up where it stopped, with more turns or budget and a note from you if you like."
          : "Its work couldn't be kept, so it can only start over."}
      </p>
      {stop.note && (
        <div className="rounded-md bg-card p-2">
          <div className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Its note on what's done and what's left</div>
          <Markdown text={stop.note} />
        </div>
      )}
      {stop.saved_diff && (
        <div>
          <button type="button" className="cursor-pointer text-sm text-primary hover:underline" aria-expanded={open} onClick={() => setOpen(!open)}>
            {open ? "Hide the work it did" : "See the work it did (against the base branch)"}
          </button>
          {open &&
            (diff.isLoading ? (
              <Loading what="Loading the saved work" />
            ) : diff.data && diff.data.patch.trim() ? (
              <div className="mt-2 max-h-[60vh] overflow-auto">
                <DiffView patch={diff.data.patch} findings={[]} itemId={0} />
              </div>
            ) : (
              <Empty>It hadn't changed any files yet.</Empty>
            ))}
        </div>
      )}
    </div>
  );
}
