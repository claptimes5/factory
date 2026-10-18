import { Link } from "react-router";
import { CheckCircle2, CircleDashed, ExternalLink, GitPullRequest, XCircle } from "lucide-react";
import { usePRPanel, type Finding, type PRPanel as PRPanelT } from "@/api/client";
import { Actions, Empty, ErrorBox, GateList, Loading } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { shortSha } from "@/lib/utils";

export const findingAnchor = (f: Finding) => `f-${f.file}-${f.line}`.replace(/[^A-Za-z0-9_-]/g, "_");

const sevVariant = { critical: "destructive", major: "warning", minor: "secondary" } as const;

export function FindingRow({ f, itemId }: { f: Finding; itemId: number }) {
  return (
    <li className="py-2 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        {f.id && <span className="font-mono text-xs text-muted-foreground">{f.id}</span>}
        <Badge variant={sevVariant[f.severity as keyof typeof sevVariant] ?? "secondary"}>{f.severity}</Badge>
        {f.blocks ? (
          <Badge variant="destructive">blocks</Badge>
        ) : (
          <Badge variant="secondary" title={f.check === "refuted" ? `Refuted: ${f.check_reason}` : undefined}>
            {f.check === "refuted" ? "refuted: a note" : "note"}
          </Badge>
        )}
        {f.by && <span className="text-xs text-muted-foreground">by the {f.by}</span>}
        {f.file && (
          <Link to={`/items/${itemId}/diff#${findingAnchor(f)}`} className="font-mono text-xs text-primary hover:underline">
            {f.file}
            {f.line > 0 && `:${f.line}`}
          </Link>
        )}
      </div>
      <p className="mt-1">{f.claim}</p>
      {f.evidence && <p className="mt-0.5 text-xs whitespace-pre-wrap text-muted-foreground">{f.evidence}</p>}
      {f.check && (
        <p className="mt-0.5 text-xs text-muted-foreground" data-testid="finding-check">
          Verifier: {f.check}
          {f.check_reason && `: ${f.check_reason}`}
        </p>
      )}
    </li>
  );
}

function Checks({ p }: { p: PRPanelT }) {
  if (p.checks_error) return <p className="text-sm text-destructive">GitHub: {p.checks_error}</p>;
  if (!p.head_sha) return <Empty>No commit yet.</Empty>;
  if (p.checks.length === 0) return <Empty>No CI checks on {shortSha(p.head_sha)}.</Empty>;
  return (
    <ul className="divide-y text-sm">
      {p.checks.map((c) => (
        <li key={c.name + c.url} className="flex items-center gap-2 py-1.5">
          {c.status !== "completed" ? (
            <CircleDashed className="size-4 animate-spin text-muted-foreground" aria-label={c.status} />
          ) : c.conclusion === "success" || c.conclusion === "skipped" || c.conclusion === "neutral" ? (
            <CheckCircle2 className="size-4 text-success" aria-label={c.conclusion} />
          ) : (
            <XCircle className="size-4 text-destructive" aria-label={c.conclusion} />
          )}
          <a href={c.url} target="_blank" rel="noreferrer" className="truncate hover:underline">
            {c.name}
          </a>
          <span className="ml-auto text-xs text-muted-foreground">{c.conclusion || c.status}</span>
        </li>
      ))}
    </ul>
  );
}

export function PRPanel({ itemId, needsYou }: { itemId: number; needsYou: boolean }) {
  const { data: p, error, isLoading } = usePRPanel(itemId);
  if (isLoading) return <Loading what="Loading the PR" />;
  if (error) return <ErrorBox error={error} />;
  if (!p) return null;
  const decision = p.review?.decision;
  return (
    <Card data-testid="pr-panel">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <GitPullRequest className="size-4" />
          {p.prs.length > 1 ? (
            `${p.prs.length} linked PRs`
          ) : p.pr_number > 0 ? (
            <a href={p.pr_url} target="_blank" rel="noreferrer" className="hover:underline">
              PR #{p.prs[0]?.number ?? p.pr_number}
            </a>
          ) : (
            "No PR yet"
          )}
          {p.head_sha && <span className="font-mono text-xs font-normal text-muted-foreground">{shortSha(p.head_sha)}</span>}
        </CardTitle>
        {decision && <Badge variant={decision === "pass" ? "success" : decision === "block" ? "destructive" : "warning"}>review: {decision}</Badge>}
      </CardHeader>
      <CardContent className="space-y-4">
        {p.prs.length > 1 && (
          <section>
            <h4 className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Merge in this order</h4>
            <ol data-testid="linked-prs" className="space-y-1 text-sm">
              {p.prs.map((x, i) => (
                <li key={x.url} className="flex items-center gap-2">
                  <span className="w-4 font-mono text-xs text-muted-foreground">{i + 1}.</span>
                  <a href={x.url} target="_blank" rel="noreferrer" className="font-mono hover:underline">
                    {x.ref}
                  </a>
                  {needsYou && (
                    <a href={`${x.url}/files`} target="_blank" rel="noreferrer" className="ml-auto text-xs text-primary-soft hover:underline">
                      Approve on GitHub
                    </a>
                  )}
                </li>
              ))}
            </ol>
          </section>
        )}
        <div className="flex flex-wrap gap-2">
          {p.pr_number > 0 && needsYou && p.prs.length <= 1 && (
            // Approving is a GitHub review in your own name; the factory never approves its own work.
            <a
              href={`${p.pr_url}/files`}
              target="_blank"
              rel="noreferrer"
              className="inline-flex h-8 items-center gap-1.5 rounded-md bg-primary px-2.5 text-xs font-medium text-primary-foreground hover:bg-primary/90"
            >
              Approve on GitHub <ExternalLink className="size-3.5" />
            </a>
          )}
          <Link to={`/items/${itemId}/diff`} className="inline-flex h-8 items-center rounded-md border bg-card px-2.5 text-xs font-medium hover:bg-accent">
            View diff
          </Link>
        </div>
        <Actions jobId={p.last_job_id} itemId={itemId} />
        <section>
          <h4 className="mb-1 text-xs font-semibold text-muted-foreground uppercase">CI</h4>
          <Checks p={p} />
        </section>
        <section>
          <h4 className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Gates</h4>
          <GateList gates={p.gates} />
        </section>
        <section>
          <h4 className="mb-1 text-xs font-semibold text-muted-foreground uppercase">Reviewer</h4>
          {!p.review ? (
            <Empty>No review yet.</Empty>
          ) : p.review.findings.length === 0 ? (
            <Empty>No findings.</Empty>
          ) : (
            <ul className="divide-y">
              {p.review.findings.map((f, i) => (
                <FindingRow key={i} f={f} itemId={itemId} />
              ))}
            </ul>
          )}
          {p.review && p.review.challenged_assumptions.length > 0 && (
            <details className="mt-2 text-sm">
              <summary className="cursor-pointer text-muted-foreground">Challenged assumptions</summary>
              <ul className="mt-1 list-disc pl-5">
                {p.review.challenged_assumptions.map((a, i) => (
                  <li key={i}>{a}</li>
                ))}
              </ul>
            </details>
          )}
        </section>
      </CardContent>
    </Card>
  );
}
