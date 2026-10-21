import { Fragment, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { ExternalLink, Scissors } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { act, APIError, getJSON, postJSON, useItem, type Estimate, type Event, type Happened, type Item, type Plan, type Split } from "@/api/client";
import type { components } from "@/api/schema";
import { Empty, ErrorBox, Inline, JobStateBadge, Loading, StageBadge, WaitsFor } from "@/components/common";
import { Choices } from "@/components/Choices";
import { PlanCostNote } from "@/components/PlanUsage";
import { PRPanel } from "@/components/PRPanel";
import { Screenshots } from "@/components/Screenshots";
import { StationStamps } from "@/components/StationStamps";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ago, cn, duration, money } from "@/lib/utils";

export function ItemScreen() {
  const id = Number(useParams().id);
  const { data, error, isLoading } = useItem(id);
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const { item: it, jobs, events, depends_on, issue_body, spec, estimate, plan, risk_reasons, needs_person, happened, split, children } = data!;
  const live = jobs.find((j) => j.state === "running");
  // A plan or cost that waits says so on its own card.
  const note = it.plan_waiting || it.estimate_waiting ? "" : it.blocked_note;
  const choices = it.choice_job_id != null && it.choices.length > 0;
  return (
    <div className="space-y-4">
      <div>
        <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
          <Link to="/line" className="hover:text-foreground">
            The line
          </Link>
          <span>/</span>
          <a href={it.issue_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 hover:text-foreground">
            {it.ref} <ExternalLink className="size-3" />
          </a>
          {it.parent_id != null && (
            <>
              <span>/</span>
              <Link to={`/items/${it.parent_id}`} className="hover:text-foreground">
                Part of item {it.parent_id}
              </Link>
            </>
          )}
        </div>
        <h1 className="mt-1 flex flex-wrap items-center gap-2 text-title font-semibold tracking-tight">
          {it.title} <StageBadge stage={it.stage} />
        </h1>
        <div className="mt-3">
          <StationStamps it={it} jobs={jobs} />
        </div>
        <WaitsFor items={it.waits_for} plan={it.plan_waiting} className="mt-1 text-sm" />
        {/* One card for where it stands and what you can do: what happened, then the buttons, with the raw error out of the way. */}
        {(happened || note || choices) && (
          <Card className="mt-3 border-warning/60" data-testid="status">
            <CardContent className="space-y-3 pt-4">
              {happened ? <HappenedCard h={happened} /> : note && <p className="text-sm">{note}</p>}
              {split && !choices && <SplitButton split={split} />}
              {happened && note && (
                <details>
                  <summary className="cursor-pointer text-xs text-muted-foreground">Details</summary>
                  <pre className="mt-1 max-h-72 overflow-auto rounded-md bg-muted p-2 font-mono text-xs whitespace-pre-wrap">{note}</pre>
                </details>
              )}
              {choices && (
                <div className="space-y-2 border-t pt-3" data-testid="next">
                  <div className="flex flex-wrap items-baseline gap-2">
                    <span className="text-sm font-semibold">What you can do</span>
                    <Link to={`/jobs/${it.choice_job_id}`} className="text-xs text-primary hover:underline">
                      about job #{it.choice_job_id}
                    </Link>
                  </div>
                  <Choices jobId={it.choice_job_id!} itemId={it.id} choices={it.choices} />
                  {split && <SplitButton split={split} />}
                  <p className="text-xs text-muted-foreground">
                    Or on{" "}
                    <a href={it.issue_url} target="_blank" rel="noreferrer" className="text-primary hover:underline">
                      the issue
                    </a>
                    : add the factory:retry label to send it back through triage.
                  </p>
                </div>
              )}
            </CardContent>
          </Card>
        )}
        {children.length > 0 && <SplitInto items={children} />}
        {live && (
          <Link to={`/jobs/${live.id}`} className="mt-2 inline-flex items-center gap-2 text-sm text-primary hover:underline">
            <span className="size-2 animate-pulse rounded-full bg-primary" /> Watch the running {live.kind} job
          </Link>
        )}
      </div>
      <div className="grid gap-4 lg:grid-cols-[1fr_420px]">
        <div className="min-w-0 space-y-4">
          {/* What waits for you first, then the issue; a plan already approved is folded away. */}
          {plan?.waiting && <PlanCard item={it} plan={plan} estimate={it.estimate_waiting ? estimate : null} />}
          {estimate && it.estimate_waiting && <EstimateCard itemId={it.id} e={estimate} waiting planWaiting={it.plan_waiting} />}
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
              {(risk_reasons.length > 0 || needs_person.length > 0) && <DiffRisk reasons={risk_reasons} person={needs_person} />}
            </CardContent>
          </Card>
          {plan && !plan.waiting && (
            <details className="rounded-card border bg-card" data-testid="plan-folded">
              <summary className="cursor-pointer px-4 py-3 text-sm font-semibold">
                Plan
                {plan.approved_by && <span className="font-normal text-muted-foreground"> · approved {plan.approved_by}</span>}
              </summary>
              <div className="border-t">
                <PlanCard item={it} plan={plan} estimate={null} flat />
              </div>
            </details>
          )}
          <Screenshots itemId={it.id} />
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
                  <PlanCostNote harnesses={jobs.map((j) => j.harness)} className="mt-2" />
                </div>
              )}
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Audit trail</CardTitle>
              <Link to={`/orchestrator?tab=decisions&item=${it.id}`} className="text-xs text-primary hover:underline">
                Orchestrator decisions
              </Link>
            </CardHeader>
            <CardContent>
              <Trail events={events} />
            </CardContent>
          </Card>
        </div>
        <div className="min-w-0">
          <div className="space-y-4">
            <PRPanel itemId={it.id} needsYou={it.stage === "awaiting_human"} noActions={choices} />
            {estimate && !it.estimate_waiting && <EstimateCard itemId={it.id} e={estimate} waiting={false} planWaiting={it.plan_waiting} />}
          </div>
        </div>
      </div>
    </div>
  );
}

const statusText: Record<string, string> = {
  off: "No limit is set for this repo.",
  under: "Under the limit, so it went ahead.",
  exempt: "Over the limit, but a fix for a red main goes ahead.",
  waiting: "Over the limit: waiting for your OK.",
  parked: "Over the limit, and parked while it waited: it still needs an OK. Add factory:retry to triage it again.",
  approved: "Over the limit; approved",
};

/** Triage's cost estimate, what it's based on, and the OK when it waits for one. */
function EstimateCard({ itemId, e, waiting, planWaiting }: { itemId: number; e: Estimate; waiting: boolean; planWaiting: boolean }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const approve = async () => {
    setBusy(true);
    const r = await act(`/items/${itemId}/approve-estimate`);
    setBusy(false);
    setMsg({ ok: r.ok, text: r.message });
    void qc.invalidateQueries();
  };
  const status = statusText[e.status] + (e.status === "approved" && e.approved_by ? ` by ${e.approved_by}.` : e.status === "approved" ? "." : "");
  return (
    <Card data-testid="estimate" className={cn(waiting && "border-warning/60")}>
      <CardHeader>
        <CardTitle>Cost estimate</CardTitle>
        {e.threshold_usd > 0 && <span className="text-xs text-muted-foreground">limit {money(e.threshold_usd)}</span>}
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        <p className="font-mono text-title-sm font-semibold">{e.range}</p>
        <p className="text-muted-foreground">{e.detail}</p>
        <p>{status}</p>
        {e.finished && (
          <p className="text-xs text-muted-foreground">
            It cost {money(e.actual_usd)} to build, against a median of {money(e.total_median_usd)} and a p75 of {money(e.total_p75_usd)}.
          </p>
        )}
        {waiting && planWaiting && <p className="text-xs text-muted-foreground">Build it, on the plan above, approves the cost and the plan at once.</p>}
        {waiting && !planWaiting && (
          <div className="flex flex-col gap-2">
            {e.can_approve ? (
              <>
                <div>
                  <Button size="sm" disabled={busy} onClick={approve}>
                    Approve estimate
                  </Button>
                </div>
                <p className="text-xs text-muted-foreground">Or add the factory:go label on the issue.</p>
              </>
            ) : (
              <p className="text-xs text-muted-foreground">
                To approve, add the factory:go label on the issue. Approving here needs a dashboard password (FACTORY_WEB_PASSWORD), or opening the
                dashboard on this machine.
              </p>
            )}
          </div>
        )}
        {msg && (
          <p role="status" className={cn("text-sm", msg.ok ? "text-success" : "text-destructive")}>
            {msg.text}
          </p>
        )}
      </CardContent>
    </Card>
  );
}

/** The plan the builder wrote before a big build, and what to do while it waits for an OK. */
function PlanCard({ item: it, plan: p, estimate, flat }: { item: Item; plan: Plan; estimate: Estimate | null | undefined; flat?: boolean }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [asking, setAsking] = useState(false);
  const [ask, setAsk] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const run = async (path: string, body: unknown = {}) => {
    setBusy(true);
    const r = await act(path, body);
    setBusy(false);
    setMsg({ ok: r.ok, text: r.message });
    if (r.ok) setAsking(false);
    void qc.invalidateQueries();
  };
  const issue = (
    <a href={it.issue_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">
      the issue {it.ref} <ExternalLink className="size-3" />
    </a>
  );
  return (
    <Card data-testid="plan" className={cn(p.waiting && "border-warning/60", flat && "rounded-none border-0 shadow-none")}>
      <CardHeader>
        <CardTitle>{p.waiting ? "Plan: waiting for your OK" : "Plan"}</CardTitle>
        {p.replan_of && <span className="text-xs text-muted-foreground">the new plan {p.replan_of} asked for</span>}
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        {p.waiting && <PlanAsk size={it.complexity} ask={p.ask} />}
        {p.waiting && <div className="text-xs font-semibold text-muted-foreground uppercase">The plan</div>}
        <p className="whitespace-pre-wrap">{p.approach}</p>
        {p.files.length > 0 && (
          <div>
            <div className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Files</div>
            <ul className="list-disc space-y-0.5 pl-4 font-mono text-xs">
              {p.files.map((f) => (
                <li key={f}>{f}</li>
              ))}
            </ul>
          </div>
        )}
        {p.risks.length > 0 && (
          <div>
            <div className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Risks</div>
            <ul className="list-disc space-y-0.5 pl-4">
              {p.risks.map((r) => (
                <li key={r}>{r}</li>
              ))}
            </ul>
          </div>
        )}
        <div>
          <div className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Tests</div>
          <p className="whitespace-pre-wrap">{p.tests}</p>
        </div>
        {!p.waiting && p.approved_by && <p className="text-muted-foreground">Approved {p.approved_by}.</p>}
        {p.waiting && (
          <div className="space-y-3 rounded-md bg-warning/10 p-3">
            <p className="font-medium">Nothing is built until someone approves this plan.</p>
            {estimate && (
              <p>
                Its cost estimate, <span className="font-mono">{estimate.range}</span>, is over the {money(estimate.threshold_usd)} limit too. Build it approves
                both.
              </p>
            )}
            {p.replan_of && (
              <p className="text-xs text-muted-foreground">
                The factory plans again only once. Asking for changes to this plan stops the item; to start over, edit the issue and add the factory:retry label.
              </p>
            )}
            {p.can_approve ? (
              <>
                <div className="flex flex-wrap gap-2">
                  <Button size="sm" disabled={busy} onClick={() => run(`/items/${it.id}/approve-estimate`, { plan_job_id: p.job_id })}>
                    Build it
                  </Button>
                  <Button size="sm" variant="outline" disabled={busy} aria-expanded={asking} onClick={() => setAsking(!asking)}>
                    Ask for another plan
                  </Button>
                </div>
                {asking && (
                  <form
                    className="space-y-2"
                    onSubmit={(e) => {
                      e.preventDefault();
                      void run(`/items/${it.id}/replan`, { plan_job_id: p.job_id, ask });
                    }}
                  >
                    <label htmlFor="plan-ask" className="block text-xs font-medium">
                      What should change? The builder writes a new plan with this, and it goes on the issue too.
                    </label>
                    <textarea
                      id="plan-ask"
                      value={ask}
                      onChange={(e) => setAsk(e.target.value)}
                      rows={4}
                      className="w-full min-w-0 rounded-md border bg-card p-2.5 text-base text-foreground placeholder:text-muted-foreground/70 md:text-sm"
                      placeholder="For example: keep the database tables in the monolith, and add a feature flag for the switch-over."
                    />
                    <Button size="sm" type="submit" disabled={busy || ask.trim() === ""}>
                      Send and plan again
                    </Button>
                  </form>
                )}
                <p className="text-xs text-muted-foreground">
                  Or on {issue}: add the factory:go label to build it, or comment what to change and then add the factory:replan label.
                </p>
              </>
            ) : (
              <>
                <ol className="list-decimal space-y-1 pl-5">
                  <li>Open {issue}.</li>
                  <li>To build it, add the factory:go label.</li>
                  <li>For another plan, comment what to change, then add the factory:replan label.</li>
                </ol>
                <p className="text-xs text-muted-foreground">
                  Doing this here needs a dashboard password (FACTORY_WEB_PASSWORD), or opening the dashboard on this machine.
                </p>
              </>
            )}
          </div>
        )}
        {msg && (
          <p role="status" className={cn("text-sm", msg.ok ? "text-success" : "text-destructive")}>
            {msg.text}
          </p>
        )}
      </CardContent>
    </Card>
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

// DiffRisk is the latest build's rating from its diff: why it raised the
// risk, and what keeps it waiting for you whatever its risk.
function DiffRisk({ reasons, person }: { reasons: string[]; person: string[] }) {
  return (
    <div className="mt-3 space-y-2 text-xs">
      {reasons.length > 0 && (
        <div>
          <div className="mb-1 font-semibold text-muted-foreground uppercase">Risk from the diff</div>
          <ul className="list-disc space-y-0.5 pl-4">
            {reasons.map((r) => (
              <li key={r}>{r}</li>
            ))}
          </ul>
        </div>
      )}
      {person.length > 0 && (
        <p className="rounded-md bg-warning/10 p-2 text-warning">
          Needs you whatever its risk, so it won't merge itself: it {person.join("; ")}.
        </p>
      )}
    </div>
  );
}

/** A last step that ends in its own punctuation (a parked question) needs no full stop. */
const endsSentence = (h: Happened) => {
  const last = h.steps[h.steps.length - 1];
  return !!last && last.job_id == null && /[.?!]$/.test(last.text);
};

/** How a stopped item got there, each step linked to its job, and the offer to split it. */
function HappenedCard({ h }: { h: Happened }) {
  return (
    <div className="text-sm" data-testid="happened">
      <div className="mb-1 text-xs font-semibold text-muted-foreground uppercase">What happened</div>
      <p>
        {h.steps.map((st, i) => (
          <Fragment key={i}>
            {i > 0 && ", then "}
            {st.text}
            {st.job_id != null && (
              <>
                {" ("}
                <Link to={`/jobs/${st.job_id}`} className="text-primary hover:underline">
                  job #{st.job_id}
                </Link>
                {")"}
              </>
            )}
          </Fragment>
        ))}
        {endsSentence(h) ? "" : "."}
      </p>
      {h.note && (
        <p className="mt-2 font-medium" data-testid="happened-note">
          <Inline text={h.note} />
        </p>
      )}
    </div>
  );
}

/** Split it: asks chat to split an item too big for one build into smaller issues. */
function SplitButton({ split }: { split: Split }) {
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const splitIt = async () => {
    setBusy(true);
    setErr("");
    try {
      // Chat may have become unavailable since the page loaded.
      const chats = await getJSON<components["schemas"]["Chats"]>("/chats");
      if (!chats.available) throw new Error(chats.unavailable);
      const r = await postJSON<components["schemas"]["SentChat"]>("/chats", { chat_id: 0, text: split.prompt });
      void navigate(`/orchestrator?chat=${r.chat_id}`);
    } catch (e) {
      setErr(e instanceof APIError || e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };
  return (
    <div className="flex flex-wrap items-center gap-2">
      <Button size="sm" variant="outline" disabled={busy || !!split.unavailable} onClick={splitIt} title={split.unavailable || undefined}>
        <Scissors /> {busy ? "Starting a chat…" : "Split it"}
      </Button>
      <span className="text-xs text-muted-foreground">
        {split.unavailable || "Asks the orchestrator's model, in a new chat, to split it into smaller issues and close this one in favor of them."}
      </span>
      {err && (
        <p role="alert" className="w-full text-destructive">
          {err}
        </p>
      )}
    </div>
  );
}

/** The issues an item was split into, and how far each has got. Once all are done, so is the item. */
function SplitInto({ items }: { items: Item[] }) {
  const done = items.filter((c) => c.stage === "done").length;
  return (
    <Card className="mt-2" data-testid="split-into">
      <CardHeader>
        <CardTitle>
          Split into {items.length} {items.length === 1 ? "issue" : "issues"}: {done} done
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-1.5">
        <ul className="space-y-1 text-sm">
          {items.map((c) => (
            <li key={c.id} className="flex flex-wrap items-center gap-2">
              <StageBadge stage={c.stage} />
              <Link to={`/items/${c.id}`} className="min-w-0 break-words text-primary hover:underline">
                {c.ref} {c.title}
              </Link>
              {c.needs_you && <span className="text-xs text-warning">needs you</span>}
              {c.waits_for.length > 0 && <span className="text-xs text-muted-foreground">after {c.waits_for.map((w) => w.ref).join(", ")}</span>}
            </li>
          ))}
        </ul>
        <p className="text-xs text-muted-foreground">
          {done === items.length ? "All done, so this item is too." : "This item is done once each of these is."}
        </p>
      </CardContent>
    </Card>
  );
}

const sizeName: Record<string, string> = { XS: "extra small", S: "small", M: "medium", L: "large" };

/** Why a plan waits for a person, and what they are deciding, ahead of the plan itself. */
function PlanAsk({ size, ask }: { size: string; ask: string }) {
  return (
    <div className="space-y-2 rounded-md bg-warning/10 p-3" data-testid="plan-ask">
      <p>
        <span className="font-medium">Why you're asked:</span>{" "}
        {size ? `triage sized this ${sizeName[size] ?? size} (${size}), and` : ""} a big build's plan waits for a person, so a wrong approach is
        caught before the build is paid for. Nothing is wrong with it.
      </p>
      {ask ? (
        <>
          <div data-testid="plan-decide">
            <p className="font-medium">What you're deciding</p>
            <p className="mt-0.5 whitespace-pre-wrap">{ask}</p>
          </div>
          <p>
            <span className="font-medium">Build it</span> to go ahead as it recommends. <span className="font-medium">Ask for another plan</span> to answer
            differently, or to change anything else below.
          </p>
        </>
      ) : (
        <>
          <div>
            <p className="font-medium">What you're deciding: is this the right change, done the right way?</p>
            <ul className="mt-1 list-disc space-y-0.5 pl-5">
              <li>The approach does what the issue asks, and no more.</li>
              <li>The files are the ones you expect it to change.</li>
              <li>You accept the risks it lists, and any assumption it makes.</li>
            </ul>
          </div>
          <p>
            <span className="font-medium">Build it</span> if all three hold. <span className="font-medium">Ask for another plan</span> if one doesn't,
            and say what to change.
          </p>
        </>
      )}
      <p className="text-xs text-muted-foreground">
        To skip this for {size || "big"} items, take {size || "the size"} out of <span className="font-mono">plan.sizes</span> in factory.yaml.
      </p>
    </div>
  );
}
