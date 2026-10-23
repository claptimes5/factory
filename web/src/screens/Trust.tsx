import { useState } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, RotateCcw, ShieldCheck } from "lucide-react";
import { act, postJSON, useTrust, type Trust, type WorkspaceTrust } from "@/api/client";
import { ErrorBox, Loading, PageTitle } from "@/components/common";
import { sourceName } from "@/components/FoundBy";
import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { toast } from "@/components/ui/toaster";
import { cn, repoName } from "@/lib/utils";
import { useScope } from "@/lib/workspace";

// Trust: how much the factory may do on its own, for each workspace, on a
// ladder toward full automation. It earns a level from its repos' record,
// a person raises it, and it steps back on its own (internal/web/trust.go).

export function TrustScreen() {
  const { data, error, isLoading } = useTrust();
  const { workspace, workspaces } = useScope();
  if (isLoading) return <Loading what="Reading the factory's record" />;
  if (error) return <ErrorBox error={error} />;
  const t = data!;
  const w = t.workspaces.find((x) => x.workspace === workspace) ?? t.workspaces[0];
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-5" data-testid="trust">
      <div>
        <PageTitle>Trust{workspaces.length > 1 && w ? ` · ${w.workspace}` : ""}</PageTitle>
        <p className="-mt-2 text-body-sm text-text-2">
          How much the factory may do on its own{w && w.repos.length > 1 ? ", with one level for every repo of the workspace" : ""}. It earns more as its
          record holds up, and steps back by itself when something goes wrong. Only you raise it.
        </p>
      </div>
      {t.pending && <PendingRestart t={t} />}
      {w ? <WorkspaceTrustView t={t} w={w} /> : <p className="text-body-sm text-muted-foreground">No repositories yet.</p>}
    </div>
  );
}

function PendingRestart({ t }: { t: Trust }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const restart = async () => {
    setBusy(true);
    const r = await act("/settings/restart");
    setBusy(false);
    if (r.ok) toast.success(r.message);
    else toast.error(r.message);
    void qc.invalidateQueries();
  };
  return (
    <Alert variant="info">
      <RotateCcw aria-hidden />
      <div className="flex flex-1 flex-wrap items-center gap-x-3 gap-y-2">
        <span className="flex-1">Saved changes apply once the factory restarts. It waits for running jobs to finish first.</span>
        {t.can_change && (
          <Button size="sm" variant="outline" disabled={busy} onClick={restart}>
            Restart when jobs finish
          </Button>
        )}
      </div>
    </Alert>
  );
}

function WorkspaceTrustView({ t, w: r }: { t: Trust; w: WorkspaceTrust }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(0);
  const move = async (level: number) => {
    setBusy(level);
    try {
      const res = await postJSON<{ message: string; trust: Trust }>("/trust", { workspace: r.workspace, level });
      qc.setQueryData(["trust"], res.trust);
      toast.success(res.message);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(0);
    }
  };
  const practice = new Map(r.practice.map((p) => [p.level, p]));
  return (
    <>
      {r.repos.length > 1 && (
        <p className="text-body-sm text-text-2" data-testid="trust-repos">
          For {r.repos.map((x, i) => (
            <span key={x}>
              {i > 0 && (i === r.repos.length - 1 ? " and " : ", ")}
              <span className="font-mono">{repoName(x)}</span>
            </span>
          ))}
          .
        </p>
      )}
      {r.mixed && (
        <Alert variant="info">
          <AlertTriangle aria-hidden />
          <span>
            Its repos were set to different levels, so they all merge at the lowest, {r.level}, until you choose one for the workspace. A repo set lower
            than the one you choose keeps its own.
          </span>
        </Alert>
      )}
      {r.stricter.length > 0 && (
        <p className="text-meta text-text-2" data-testid="trust-stricter">
          Set lower by hand in factory.yaml:{" "}
          {r.stricter.map((s, i) => (
            <span key={s.repo}>
              {i > 0 && ", "}
              <span className="font-mono">{repoName(s.repo)}</span> at level {s.level}
            </span>
          ))}
          . A repo stays at its own level when that's stricter.
        </p>
      )}
      {r.stepped_back && (
        <Alert variant="attention">
          <AlertTriangle aria-hidden />
          <span>
            <span className="font-medium">Stepped back to level 1.</span> {r.why}. New pull requests wait for you until auto-merge is reset, from the notice at
            the top.
          </span>
        </Alert>
      )}

      <ol aria-label="Levels" className="flex flex-col rounded-card border bg-surface p-1.5">
        {t.levels.map((l) => {
          const here = l.level === r.level;
          const p = practice.get(l.level);
          return (
            <li
              key={l.level}
              data-testid="level"
              aria-current={here ? "step" : undefined}
              className={cn("flex gap-3 rounded-control p-3", here && "bg-live-bg", !l.available && "opacity-70")}
            >
              <span
                className={cn(
                  "flex size-7 shrink-0 items-center justify-center rounded-full font-mono text-meta font-semibold",
                  here ? "bg-primary text-primary-foreground" : l.level < r.level ? "border-2 border-primary text-primary" : "border-2 border-dashed border-border text-muted-foreground",
                )}
              >
                {l.level}
              </span>
              <div className="flex min-w-0 flex-1 flex-col gap-1">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-body-sm font-semibold">{l.name}</span>
                  {here && <Badge>{r.custom ? "Now (set by hand)" : "Now"}</Badge>}
                  {l.level === r.suggest && <Badge variant="success">Earned</Badge>}
                  {!l.available && <Badge variant="secondary">Not built yet</Badge>}
                </div>
                <p className="text-meta text-text-2">{l.detail}</p>
                {!l.available && <p className="text-meta text-muted-foreground">{l.needs}</p>}
                {p && l.level === 6 ? (
                  <p className="text-meta text-muted-foreground">
                    Practice run: of the {r.proposed} issues it filed from its own findings in {t.window_days} days, you took on {r.accepted}
                    {r.accepted > 0 && (p.went_wrong > 0 ? `; ${p.went_wrong} went wrong once merged` : "; none went wrong once merged")}.
                  </p>
                ) : (
                  p &&
                  l.level !== 1 && (
                    <p className="text-meta text-muted-foreground">
                      Practice run: would have merged {p.would_merge} in {t.window_days} days
                      {p.would_merge > 0 && (p.went_wrong > 0 ? `, ${p.went_wrong} went wrong` : ", none went wrong")}.
                    </p>
                  )
                )}
                {l.level === 6 && here && r.sources.length > 0 && (
                  <p className="text-meta text-text-2">Starts on its own: {r.sources.map(sourceName).join(", ")}.</p>
                )}
              </div>
              {l.available && !here && t.can_change && (
                <Button size="sm" variant={l.level === r.suggest ? "default" : "outline"} disabled={busy > 0} className="self-center" onClick={() => move(l.level)}>
                  {l.level < r.level ? "Step down" : "Move here"}
                </Button>
              )}
            </li>
          );
        })}
      </ol>
      {!t.can_change && t.reason && <p className="text-meta text-muted-foreground">{t.reason}</p>}

      {r.suggest > 0 ? (
        <section aria-label="Earned" className="flex flex-col gap-3 rounded-card bg-ink p-5 text-ink-foreground">
          <span className="text-meta font-semibold text-ink-attention">Earned</span>
          <h2 className="text-title-sm font-semibold">Ready for level {r.suggest}?</h2>
          <p className="text-body-sm text-ink-muted">{r.suggest_why}</p>
          {t.can_change && (
            <div className="flex flex-wrap gap-2">
              <Button variant="inverse" disabled={busy > 0} onClick={() => move(r.suggest)}>
                Move to level {r.suggest}
              </Button>
            </div>
          )}
        </section>
      ) : (
        <p className="flex items-start gap-2 px-1 text-body-sm text-text-2">
          <ShieldCheck className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden /> {r.suggest_why}
        </p>
      )}

      <section aria-label="Its record" className="flex flex-col gap-3 rounded-card border bg-surface p-4">
        <h2 className="text-body-sm font-semibold">Its record, last {t.window_days} days</h2>
        <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          {[
            ["Merged", r.merged],
            ["By itself", r.automatic],
            ["Reverted", r.reverted],
            ["Broke main", r.broke_main],
          ].map(([k, v]) => (
            <div key={k} className="flex flex-col">
              <dd className="font-mono text-title font-semibold">{v}</dd>
              <dt className="text-meta text-muted-foreground">{k}</dt>
            </div>
          ))}
        </dl>
      </section>

      <section aria-label="Guard rails" className="flex flex-col gap-2 px-1 text-body-sm text-text-2">
        <p>
          <span className="font-semibold text-foreground">Always asks you:</span> {t.always_asks.length > 0 ? t.always_asks.map(kind).join(", ") : "nothing listed"},
          and anything high-risk, at every level.
        </p>
        <p>
          <span className="font-semibold text-foreground">Steps back on its own:</span> reverts or a broken main in any of its repos turn auto-merge off
          for the workspace until you reset it, which also stops it starting its own work; the record starts again from there.
        </p>
        <p className="text-meta text-muted-foreground">
          A level is the workspace's auto_merge in factory.yaml; you can set it there too, or set a repo lower.{" "}
          <Link to="/settings" className="text-primary hover:underline">Settings</Link>
        </p>
      </section>
    </>
  );
}

const kinds: Record<string, string> = { migrations: "migrations", new_dependencies: "new dependencies", auth: "auth code", deleted_tests: "deleted tests" };
const kind = (k: string) => kinds[k] ?? k.replace(/_/g, " ");
