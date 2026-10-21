import { useState } from "react";
import { useParams } from "react-router";
import { useItem, useJob, useSavedDiff, type Stop } from "@/api/client";
import { Choices } from "@/components/Choices";
import { Actions, Empty, ErrorBox, GateList, JobStateBadge, Loading, PageHeader } from "@/components/common";
import { DiffView } from "@/components/DiffView";
import { Markdown } from "@/components/Markdown";
import { PlanCostNote } from "@/components/PlanUsage";
import { Terminal } from "@/components/Terminal";
import { Transcript } from "@/components/Transcript";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { duration, money } from "@/lib/utils";

const kindName = (k: string) => k.charAt(0).toUpperCase() + k.slice(1);

export function SessionScreen() {
  const id = Number(useParams().id);
  const { data, error, isLoading } = useJob(id);
  const itemId = data?.job.item_id ?? 0;
  const item = useItem(itemId);
  const [tab, setTab] = useState<string>("transcript");
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const { job: j, gates, can_attach, stop, choices, can_steer, steer_note } = data!;
  const running = j.state === "running";
  const it = item.data?.item;
  // The transcript has the room; the terminal is a tab beside it, and on a
  // phone the issue and its gates are one too. On a desk they sit at the side.
  const side = (
    <>
      <Card>
        <CardHeader>
          <CardTitle>Issue</CardTitle>
        </CardHeader>
        <CardContent className="max-h-[50vh] overflow-auto">
          {item.data?.issue_body ? <p className="text-body-sm whitespace-pre-wrap">{item.data.issue_body}</p> : <Empty inline>No issue text.</Empty>}
          {item.data?.spec && (
            <div className="mt-3 rounded-control bg-muted p-3 text-body-sm">
              <div className="label mb-1">Spec (triage)</div>
              <p className="whitespace-pre-wrap">{item.data.spec}</p>
            </div>
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Gates</CardTitle>
        </CardHeader>
        <CardContent>
          <GateList gates={gates} />
        </CardContent>
      </Card>
    </>
  );
  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        crumbs={[{ label: "The line", to: "/line" }, ...(itemId > 0 ? [{ label: it ? `${it.ref} ${it.title}` : `Item ${itemId}`, to: `/items/${itemId}` }] : [])]}
        title={`${kindName(j.kind)} job #${j.id}`}
        badges={<JobStateBadge state={stop ? "stopped" : j.state} />}
        meta={
          <>
            {j.harness}/{j.model}
            {j.effort && ` · ${j.effort}`} · attempt {j.attempt} · {money(j.cost_usd)} of {money(j.budget_usd)}
            {j.started_at && ` · ${duration(j.started_at, j.finished_at || undefined)}`}
            {j.skillset && ` · skills: ${j.skillset}`}
            <PlanCostNote harnesses={[j.harness]} className="mt-1" />
          </>
        }
      />
      {j.error && !stop && <p className="text-body-sm text-destructive">{j.error}</p>}
      {stop && <StopPanel jobId={j.id} stop={stop} />}
      <div>
        {!running && choices.length > 0 ? (
          <Choices jobId={j.id} itemId={itemId || undefined} choices={choices} />
        ) : (
          <Actions jobId={j.id} itemId={itemId || undefined} running={running} />
        )}
      </div>
      <div className="grid gap-5 xl:grid-cols-[minmax(0,1fr)_360px]">
        <Tabs value={tab} onValueChange={setTab} className="min-w-0">
          <TabsList>
            <TabsTrigger value="transcript">Transcript</TabsTrigger>
            {can_attach && <TabsTrigger value="terminal">Terminal</TabsTrigger>}
            <TabsTrigger value="issue" className="xl:hidden">
              Issue & spec
            </TabsTrigger>
          </TabsList>
          {/* Kept mounted, so following the log and a connected terminal survive a switch. */}
          <TabsContent value="transcript" forceMount className="flex h-[65dvh] min-w-0 flex-col data-[state=inactive]:hidden xl:h-[72vh]">
            <Transcript jobId={j.id} job={j} stop={stop} canSteer={can_steer} steerNote={steer_note} />
          </TabsContent>
          {can_attach && (
            <TabsContent value="terminal" forceMount className="flex h-[65dvh] min-w-0 flex-col data-[state=inactive]:hidden xl:h-[72vh]">
              <Terminal jobId={j.id} />
            </TabsContent>
          )}
          <TabsContent value="issue" className="flex flex-col gap-4 xl:hidden">
            {side}
          </TabsContent>
        </Tabs>
        <aside className="hidden min-w-0 flex-col gap-4 xl:flex">{side}</aside>
      </div>
    </div>
  );
}

/** A build that ran out of turns or budget: what happened, its note, and the work it kept. */
function StopPanel({ jobId, stop }: { jobId: number; stop: Stop }) {
  const [open, setOpen] = useState(false);
  const diff = useSavedDiff(jobId, open && stop.saved_diff);
  return (
    <div data-testid="stop" className="space-y-2 rounded-card border border-warning-border bg-warning-bg p-4 text-body-sm">
      <p className="font-medium">{stop.summary}</p>
      <p className="text-muted-foreground">
        {stop.kept
          ? "Its work is kept on the factory's machine, and nothing was pushed. It doesn't restart on its own: running out again is likely unless something changes. Continue picks up where it stopped, with more turns or budget and a note from you if you like."
          : "Its work couldn't be kept, so it can only start over."}
      </p>
      {stop.note && (
        <div className="rounded-control bg-surface p-3">
          <div className="label mb-1">Its note on what's done and what's left</div>
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
