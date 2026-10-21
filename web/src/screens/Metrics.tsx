import { useMetrics, useSpend } from "@/api/client";
import { ErrorBox, Loading, PageTitle } from "@/components/common";
import { PlanUsageMeter } from "@/components/PlanUsage";
import { Chart } from "@/components/Chart";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { money, pct } from "@/lib/utils";

export function MetricsScreen() {
  const m = useMetrics();
  const s = useSpend();
  if (m.isLoading || s.isLoading) return <Loading />;
  if (m.error || s.error) return <ErrorBox error={m.error ?? s.error} />;
  const r = m.data!;
  const sp = s.data!;
  const weeks = r.weekly.slice(-12);
  const wl = (start: string) => start.slice(5);
  const rate = (v: number) => (v < 0 ? null : v);
  return (
    <div className="space-y-4">
      <PageTitle>Spend and metrics</PageTitle>
      <PlanUsageMeter className="mb-4 rounded-card border bg-surface p-3.5" />
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4 xl:grid-cols-7">
        <Stat label="Cost per merged PR" value={r.merged ? money(r.cost_per_merged_usd) : "–"} big />
        <Stat label="First-pass rate" value={pct(r.first_pass_rate)} />
        <Stat label="Escalation rate" value={pct(r.escalation_rate)} />
        <Stat label="Review escapes" value={pct(r.review_escape_rate)} hint="Reviewer-passed PRs a human changed or closed" />
        <Stat label="Reviewer precision" value={pct(r.review_precision)} hint="Blocking findings that led to a fix" />
        <Stat label="Rounds to pass" value={r.rounds_to_pass < 0 ? "–" : r.rounds_to_pass.toFixed(1)} hint="Review rounds an item took before its review passed" />
        <Stat
          label="Revert rate"
          value={pct(r.revert_rate)}
          hint={`Factory PRs merged in the last ${r.revert_window_days} days that a later commit or PR reverted: ${r.reverted} of ${r.revert_merged}`}
        />
        <Stat label="Raised by the diff" value={String(r.risk_raised_by_diff)} hint="Items whose built diff raised their risk above triage's" />
        <Stat label="Spent this week" value={money(sp.week_usd)} />
        <Stat label="Merged / items" value={`${r.merged} / ${r.items}`} />
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        <ChartCard title="Daily spend (30 days)">
          <Chart kind="bar" format={money} max={Math.max(sp.daily_cap_usd, ...sp.days.map((d) => d.cost_usd)) || undefined}
            points={[...sp.days].reverse().map((d) => ({ label: d.day.slice(5), value: d.cost_usd }))} />
        </ChartCard>
        <ChartCard title="Cost per merged PR, by week">
          <Chart format={money} points={weeks.map((w) => ({ label: wl(w.start), value: w.merged ? w.cost_per_merged_usd : null }))} />
        </ChartCard>
        <ChartCard title="First-pass rate, by week">
          <Chart format={pct} max={1} points={weeks.map((w) => ({ label: wl(w.start), value: rate(w.first_pass_rate) }))} />
        </ChartCard>
        <ChartCard title="Escalation rate, by week">
          <Chart format={pct} max={1} points={weeks.map((w) => ({ label: wl(w.start), value: rate(w.escalation_rate) }))} />
        </ChartCard>
      </div>
      <div className="grid gap-4 lg:grid-cols-3">
        <Table title="Cost per PR by size" rows={r.by_complexity.map((g) => [g.key, String(g.merged), money(g.cost_per_pr_usd)])} head={["Size", "Merged", "Per PR"]} />
        <Table title="Cost per PR by model" rows={r.by_model.map((g) => [g.key, String(g.merged), money(g.cost_per_pr_usd)])} head={["Model", "Merged", "Per PR"]} />
        <Table
          title="Reviewer precision"
          rows={r.precision_by_reviewer.map((x) => [x.key, `${x.fixed} / ${x.findings}`, String(x.refuted), pct(x.precision)])}
          head={["Reviewer", "Fixed", "Refuted", "Precision"]}
        />
        {r.reverts_by_repo.length > 0 && (
          <Table
            title={`Reverts by repo (${r.revert_window_days} days)`}
            rows={r.reverts_by_repo.map((x) => [x.repo, `${x.reverted} / ${x.merged}`, pct(x.revert_rate), String(x.broke_main)])}
            head={["Repo", "Reverted", "Rate", "Broke main"]}
          />
        )}
        <Table title="Spend by model (all time)" rows={sp.by_model.map((x) => [x.model, money(x.cost_usd)])} head={["Model", "Spend"]} />
        {sp.usage.length > 0 && (
          <Table
            title="Usage limits (last reported)"
            rows={sp.usage.map((u) => [u.harness, u.window, `${Math.round(u.used_percent)}%`, u.resets_at ? new Date(u.resets_at).toLocaleString() : "unknown"])}
            head={["Harness", "Window", "Used", "Resets"]}
          />
        )}
      </div>
      <p className="text-xs text-muted-foreground">
        Human time: items sat {r.awaiting_human_hours.toFixed(1)} h awaiting review in total ({r.awaiting_human_mean_hours.toFixed(1)} h on average).
        Reviewer blocks: {r.review_blocks}. The verifier refuted {pct(r.verifier_refute_rate)} of the findings it checked. Security passes:{" "}
        {r.security_passes}, {r.security_blocks} of them blocking.
      </p>
      {sp.estimates.compared > 0 && (
        <p className="text-xs text-muted-foreground" data-testid="estimate-accuracy">
          Cost estimates: {sp.estimates.within} of {sp.estimates.compared} finished builds cost at most their p75; the median build came in{" "}
          {sp.estimates.median_miss_usd >= 0 ? `${money(sp.estimates.median_miss_usd)} over` : `${money(-sp.estimates.median_miss_usd)} under`} its estimate's median.
        </p>
      )}
    </div>
  );
}

function Stat({ label, value, hint, big }: { label: string; value: string; hint?: string; big?: boolean }) {
  return (
    <Card className="p-3" title={hint}>
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={big ? "text-2xl font-semibold" : "text-xl font-semibold"}>{value}</div>
    </Card>
  );
}

function ChartCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  );
}

function Table({ title, head, rows }: { title: string; head: string[]; rows: string[][] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>
        {rows.length === 0 ? (
          <p className="text-sm text-muted-foreground">No data yet.</p>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-left text-xs text-muted-foreground">
              <tr>
                {head.map((h) => (
                  <th key={h} className="pb-1 font-medium">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y">
              {rows.map((r, i) => (
                <tr key={i}>
                  {r.map((c, j) => (
                    <td key={j} className={j === 0 ? "py-1.5 pr-2 break-all" : "py-1.5"}>
                      {c}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </CardContent>
    </Card>
  );
}
