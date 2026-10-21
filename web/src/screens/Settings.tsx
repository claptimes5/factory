import { useState, type ReactNode } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftRight, Check, ChevronRight, ExternalLink, Lock, RotateCw, Save, Sparkles, X } from "lucide-react";
import {
  act,
  APIError,
  applyPreset,
  applyTuning,
  saveSettings,
  useOverview,
  useSettings,
  useTuning,
  type Setting,
  type Settings,
  type SettingValue,
  type TuningChange,
} from "@/api/client";
import { ErrorBox, Loading, PageTitle } from "@/components/common";
import { RemoveFactory } from "@/components/Uninstall";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

// A field's draft is what its input holds: text for most kinds, a boolean
// for a checkbox. Only drafts that differ from the file are sent.
type Draft = string | boolean;

const key = (f: Setting) => f.path.join("\u0000");
const id = (f: Setting) => "set-" + f.path.join("-").replace(/[^\w-]/g, "_");

function initial(f: Setting): Draft {
  if (f.kind === "bool") return f.value === true || (f.value == null && f.default === true);
  if (f.value == null) return "";
  return Array.isArray(f.value) ? f.value.join(", ") : String(f.value);
}

/** The draft as the API takes it: empty is null, which puts the default back. */
function toValue(f: Setting, d: Draft): SettingValue {
  if (typeof d === "boolean") return d;
  const s = d.trim();
  if (s === "") return null;
  if (f.kind === "number" || f.kind === "integer") return Number.isNaN(Number(s)) ? s : Number(s);
  if (f.kind === "list") return s.split(/[\s,]+/).filter(Boolean);
  return s;
}

const show = (v: SettingValue) => (v == null ? "" : Array.isArray(v) ? v.join(", ") : String(v));

const input =
  "h-10 w-full min-w-0 rounded-md border bg-card px-2.5 text-base text-foreground placeholder:text-muted-foreground/70 disabled:opacity-60 md:h-9 md:text-sm";

/** The screens that set the factory up, a tap away: on a phone, Settings is where they live. */
export const settingsLinks = [
  { to: "/trust", label: "Trust", hint: "How much the factory may do on its own" },
  { to: "/notify", label: "When to tell you", hint: "Notifications on your phone, quiet hours" },
  { to: "/get-started", label: "Get started", hint: "Repos, access, model keys" },
  { to: "/setup", label: "Repo setup", hint: "Is each repo ready for work?" },
  { to: "/metrics", label: "Spend", hint: "Costs, plans and how work goes" },
  { to: "/notifications", label: "Inbox", hint: "Everything waiting on you" },
];

function SettingsHub() {
  return (
    <nav aria-label="More settings" className="flex flex-wrap gap-2">
      {settingsLinks.map((l) => (
        <Link
          key={l.to}
          to={l.to}
          title={l.hint}
          className="flex items-center gap-1 rounded-full border bg-surface px-3 py-1.5 text-body-sm hover:border-muted-foreground/40"
        >
          {l.label} <ChevronRight className="size-3.5 text-muted-foreground" aria-hidden />
        </Link>
      ))}
    </nav>
  );
}

/** The screen's sections: the server's groups, gathered so there are a few to scan, each with a line saying where it stands. */
const sections: { key: string; title: string; groups: string[] }[] = [
  { key: "limits", title: "Spending and limits", groups: ["Budgets"] },
  { key: "models", title: "Models", groups: ["Models"] },
  { key: "review", title: "Review and escalation", groups: ["Review", "Escalation"] },
  { key: "automation", title: "Automation", groups: ["Auto-merge", "Orchestrator", "Tuning", "Intake"] },
  { key: "notify", title: "Notifications and asks", groups: ["Quiet hours", "Asks"] },
];

/** The spending fields shown first; the rest wait behind "More limits". */
const firstLimits = ["budgets.daily_total", "budgets.concurrent_jobs", "budgets.awaiting_human_max", "budgets.pause_at_usage"];

const harnessName = (h: string) => (h === "codex" ? "Codex" : h === "claude" ? "Claude" : h);

export function SettingsScreen() {
  const { data, error, isLoading } = useSettings();
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  return <SettingsForm s={data!} />;
}

function SettingsForm({ s }: { s: Settings }) {
  const qc = useQueryClient();
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState("");
  const [restartMsg, setRestartMsg] = useState("");
  const [saved, setSaved] = useState(false);
  const fields = s.groups.flatMap((g) => g.fields);
  const changed = fields.filter((f) => key(f) in drafts && drafts[key(f)] !== initial(f));
  const ro = !s.writable;

  const set = (f: Setting, d: Draft) => {
    setErr("");
    setDrafts((ds) => ({ ...ds, [key(f)]: d }));
  };
  const save = async () => {
    setSaving(true);
    setErr("");
    try {
      const next = await saveSettings(
        changed.map((f) => ({ path: f.path, value: toValue(f, drafts[key(f)]) })),
        s.version,
      );
      qc.setQueryData(["settings"], next);
      setDrafts({});
      setSaved(true);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      // Changed on disk since: load it, keeping what was typed here.
      if (e instanceof APIError && e.status === 409) void qc.invalidateQueries({ queryKey: ["settings"] });
    } finally {
      setSaving(false);
    }
  };
  const press = async (path: string) => {
    try {
      const r = await act(path);
      setRestartMsg(r.ok ? "" : r.message);
    } catch (e) {
      setRestartMsg(e instanceof Error ? e.message : String(e));
    }
    void qc.invalidateQueries({ queryKey: ["settings"] });
  };
  const restart = () => press("/settings/restart");
  const cancelRestart = () => press("/settings/restart/cancel");
  const draft = (f: Setting) => (key(f) in drafts ? drafts[key(f)] : initial(f));
  // On plans only, no job costs dollars: the dollar caps matter only with API keys.
  const plansOnly = !!useOverview().data?.plans_only;

  return (
    <div className="max-w-4xl space-y-4" data-testid="settings">
      <PageTitle aside={<span className="font-mono text-xs break-all text-muted-foreground">{s.path}</span>}>Settings</PageTitle>
      <p className="-mt-2 text-sm text-text-2">
        Repos, GitHub or GitLab access, model keys and notifications are on the{" "}
        <Link to="/get-started" className="text-primary hover:underline">
          get-started page
        </Link>
        .
      </p>
      <SettingsHub />

      {ro && (
        <div role="note" className="flex gap-2 rounded-md border border-warning-border bg-warning-bg p-3 text-sm text-warning">
          <Lock className="mt-0.5 size-4 shrink-0" aria-hidden />
          <span>{s.reason}</span>
        </div>
      )}
      <TuningCard
        s={s}
        ro={ro}
        onApplied={(next) => {
          qc.setQueryData(["settings"], next);
          setSaved(true);
        }}
      />
      {s.restart_pending ? (
        <Notice>
          <span className="min-w-0 flex-1">
            {s.running_jobs > 0 || s.restart_waits_for
              ? `Restart pending, waiting for ${[
                  s.running_jobs > 0 ? `${s.running_jobs} running ${s.running_jobs === 1 ? "job" : "jobs"}` : "",
                  s.restart_waits_for,
                ]
                  .filter(Boolean)
                  .join(" and ")}. No new jobs start until then.`
              : "Restarting…"}
          </span>
          {!ro && (s.running_jobs > 0 || !!s.restart_waits_for) && (
            <Button size="sm" variant="outline" onClick={cancelRestart}>
              Cancel restart
            </Button>
          )}
        </Notice>
      ) : (
        s.changed && (
          <Notice>
            <span className="min-w-0 flex-1">
              {saved ? "Saved to " : "Changed since the factory started: "}
              <span className="font-mono break-all">{s.path}</span>. Changes apply when the factory restarts.
            </span>
            {!ro && (
              <Button size="sm" variant="outline" onClick={restart}>
                <RotateCw /> Restart when jobs finish
              </Button>
            )}
          </Notice>
        )
      )}
      {restartMsg && <p role="alert" className="text-sm text-destructive">{restartMsg}</p>}

      <PresetCard s={s} ro={ro} pending={changed.length > 0} onApplied={(next) => qc.setQueryData(["settings"], next)} />

      {sections.map((sec) => {
        const groups = s.groups.filter((g) => sec.groups.includes(g.name));
        if (groups.length === 0) return null;
        const edited = groups.some((g) => g.fields.some((f) => changed.includes(f)));
        return (
          <Section key={sec.key} title={sec.title} summary={summary(sec.key, s, plansOnly)} edited={edited}>
            {groups.map((g) => (
              <div key={g.name} role="region" aria-label={g.name} className="flex flex-col gap-2">
                {groups.length > 1 && <h3 className="text-body-sm font-semibold">{g.name}</h3>}
                {g.help && <p className="text-xs text-muted-foreground">{g.help}</p>}
                {g.fields.some((f) => f.row) ? (
                  <Rows fields={g.fields} draft={draft} set={set} ro={ro} />
                ) : g.name === "Budgets" ? (
                  <Limits fields={g.fields} draft={draft} set={set} ro={ro} plansOnly={plansOnly} />
                ) : (
                  <Fields fields={g.fields} draft={draft} set={set} ro={ro} />
                )}
              </div>
            ))}
          </Section>
        );
      })}

      {!ro && changed.length > 0 && (
        <div className="sticky bottom-20 z-10 flex flex-wrap items-center gap-3 rounded-control border bg-surface/95 p-3 shadow-xs backdrop-blur md:bottom-4">
          <Button onClick={save} disabled={saving || changed.length === 0}>
            <Save /> {saving ? "Saving…" : "Save"}
          </Button>
          {changed.length > 0 && (
            <Button
              variant="ghost"
              disabled={saving}
              onClick={() => {
                setDrafts({});
                setErr("");
              }}
            >
              Discard
            </Button>
          )}
          <span className="text-sm text-muted-foreground">
            {changed.length === 0 ? "No changes." : `${changed.length} ${changed.length === 1 ? "change" : "changes"} to save.`}
          </span>
          {err && (
            <p role="alert" className="w-full text-sm break-words text-destructive">
              {err}
            </p>
          )}
        </div>
      )}
      <p className="text-xs text-muted-foreground">Gate commands, repos, the runner and the network are edited in factory.yaml.</p>
      <RemoveFactory />
    </div>
  );
}

/** A section of settings, folded to a line that says where it stands; one with edits opens. */
function Section({ title, summary, edited, children }: { title: string; summary: string; edited: boolean; children: ReactNode }) {
  return (
    <details open={edited || undefined} className="group rounded-card border bg-card" data-testid="settings-section">
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-3 gap-y-1 p-4 md:px-5">
        <ChevronRight className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-90" aria-hidden />
        <span className="font-semibold">{title}</span>
        <span className="min-w-0 flex-1 text-sm text-muted-foreground">{summary}</span>
        {edited && <span className="text-xs font-medium text-primary">Edited</span>}
      </summary>
      <div className="flex flex-col gap-5 border-t p-4 md:px-5">{children}</div>
    </details>
  );
}

/** A field's value as it stands: the file's, or the default. */
function current(s: Settings, path: string): SettingValue {
  for (const g of s.groups) for (const f of g.fields) if (f.path.join(".") === path) return f.value ?? f.default;
  return null;
}

/** A section's line: its few settings that matter most, as they stand. */
function summary(sec: string, s: Settings, plansOnly: boolean): string {
  const v = (p: string) => current(s, p);
  switch (sec) {
    case "limits": {
      const jobs = `${v("budgets.concurrent_jobs") ?? 3} jobs at once`;
      const pause = `plans pause at ${v("budgets.pause_at_usage") ?? 95}% of a usage window`;
      return plansOnly ? `${jobs} · ${pause}` : `$${v("budgets.daily_total") ?? "?"} a day · ${jobs} · ${pause}`;
    }
    case "models": {
      const p = s.presets.find((x) => x.key === s.preset);
      const who = s.builder === s.reviewer ? `${harnessName(s.builder)} builds and reviews` : `${harnessName(s.builder)} builds, ${harnessName(s.reviewer)} reviews`;
      return `${p ? `As ${p.name} sets them` : "Set by hand"} · ${who}`;
    }
    case "review":
      return `Findings ${v("review.verify") === false ? "not double-checked" : "double-checked"} · ${v("escalation.max_review_rounds") ?? 2} review rounds`;
    case "automation": {
      const up = v("auto_merge.up_to");
      return [
        !up || up === "none" ? "Auto-merge off" : `Auto-merge up to ${up} risk`,
        v("orchestrator.enabled") === true ? "orchestrator on" : "orchestrator off",
        `tuning every ${v("tuning.every") ?? "720h"}`,
      ].join(" · ");
    }
    case "notify": {
      const from = v("notify.quiet_hours.from");
      return [from ? `Quiet ${from}–${v("notify.quiet_hours.until") ?? "?"}` : "No quiet hours", v("concise_asks") === false ? "full asks" : "concise asks"].join(" · ");
    }
  }
  return "";
}

/** Fields in a grid: label, input, what it does. */
function Fields({ fields, draft, set, ro }: { fields: Setting[] } & FieldProps) {
  return (
    <div className="grid gap-x-4 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
      {fields.map((f) => (
        <div key={key(f)} className="flex min-w-0 flex-col gap-1">
          <label htmlFor={id(f)} className="text-sm font-medium">
            {f.label}
          </label>
          <Input f={f} d={draft(f)} set={set} ro={ro} />
          <span id={id(f) + "-help"} className="text-xs text-muted-foreground">
            {f.help}
          </span>
        </div>
      ))}
    </div>
  );
}

/** Spending: the limits that matter first; per-job caps behind a fold, and on plans only, the dollar caps too. */
function Limits({ fields, plansOnly, ...p }: { fields: Setting[]; plansOnly: boolean } & FieldProps) {
  const dollars = (f: Setting) => f.path[1] !== "concurrent_jobs" && f.path[1] !== "awaiting_human_max" && f.path[1] !== "pause_at_usage" && f.path[1] !== "max_turns";
  const first = fields.filter((f) => firstLimits.includes(f.path.join(".")) && !(plansOnly && dollars(f)));
  const rest = fields.filter((f) => !first.includes(f));
  return (
    <>
      <Fields fields={first} {...p} />
      {rest.length > 0 && (
        <details open={rest.some((f) => p.draft(f) !== initial(f)) || undefined}>
          <summary className="cursor-pointer text-sm font-medium">{plansOnly ? "Dollar caps: only for work on API keys" : "More limits"}</summary>
          <div className="mt-3">
            <Fields fields={rest} {...p} />
          </div>
        </details>
      )}
    </>
  );
}

/**
 * How hard the factory works: a ladder of presets, each setting the model
 * for every step and how strictly review checks itself, for the harnesses
 * there are. Applied at once, like a save.
 */
function PresetCard({ s, ro, pending, onApplied }: { s: Settings; ro: boolean; pending: boolean; onApplied: (s: Settings) => void }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const [err, setErr] = useState("");
  const both = s.harnesses.includes("claude") && s.harnesses.includes("codex");
  const apply = async (body: { key?: string; builder?: string; reviewer?: string }, what: string) => {
    setBusy(what);
    setErr("");
    try {
      onApplied(await applyPreset({ ...body, version: s.version }));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      // Changed on disk since: load it, so the next try starts from it.
      if (e instanceof APIError && e.status === 409) void qc.invalidateQueries({ queryKey: ["settings"] });
    } finally {
      setBusy("");
    }
  };
  const blocked = ro || pending || !!busy;
  return (
    <section aria-label="How hard it works" className="flex flex-col gap-3 rounded-card border bg-card p-4 md:p-5">
      <div>
        <h2 className="font-semibold">How hard it works</h2>
        <p className="text-xs text-muted-foreground">
          Sets the model for each step, how strictly review checks itself and how far a failing build climbs, for the harnesses you have. The rest stays as it is.
        </p>
      </div>
      <ol className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
        {s.presets.map((p) => {
          const now = p.key === s.preset;
          return (
            <li key={p.key}>
              <button
                type="button"
                data-testid="preset"
                aria-pressed={now}
                disabled={blocked || now}
                onClick={() => apply({ key: p.key }, p.key)}
                className={cn(
                  "flex h-full w-full flex-col gap-1 rounded-control border p-3 text-left text-sm disabled:cursor-default",
                  now ? "border-primary bg-primary/5" : "hover:border-muted-foreground/40",
                )}
              >
                <span className="flex items-center gap-2 font-medium">
                  {p.name}
                  {now && <span className="rounded-full bg-primary/15 px-2 text-caption text-primary">Now</span>}
                  {busy === p.key && <span className="text-caption text-muted-foreground">Applying…</span>}
                </span>
                <span className="text-xs text-muted-foreground">{p.summary}</span>
              </button>
            </li>
          );
        })}
      </ol>
      {s.preset === "" && (
        <p className="text-xs text-muted-foreground">
          Set by hand: the models and review settings below match no preset. Choosing one replaces them.
        </p>
      )}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
        <span>
          {s.builder === s.reviewer
            ? `${harnessName(s.builder)} builds and reviews.`
            : `${harnessName(s.builder)} builds; ${harnessName(s.reviewer)} reviews what it builds.`}
        </span>
        {both && (
          <Button
            size="sm"
            variant="outline"
            disabled={blocked}
            onClick={() =>
              apply(
                s.builder === s.reviewer
                  ? { builder: s.builder, reviewer: s.builder === "claude" ? "codex" : "claude" }
                  : { builder: s.reviewer, reviewer: s.builder },
                "swap",
              )
            }
          >
            <ArrowLeftRight /> {s.builder === s.reviewer ? `Have ${harnessName(s.builder === "claude" ? "codex" : "claude")} review` : "Swap"}
          </Button>
        )}
        {!both && s.harnesses.length === 1 && (
          <span className="text-xs text-muted-foreground">
            Connect {harnessName(s.harnesses[0] === "claude" ? "codex" : "claude")} on the{" "}
            <Link to="/get-started" className="text-primary hover:underline">
              get-started page
            </Link>
            , and it reviews what {harnessName(s.harnesses[0])} builds.
          </span>
        )}
      </div>
      {pending && <p className="text-xs text-muted-foreground">Save or discard your edits below first.</p>}
      {err && (
        <p role="alert" className="text-sm text-destructive">
          {err}
        </p>
      )}
    </section>
  );
}

function Notice({ children }: { children: ReactNode }) {
  return (
    <div role="status" className="flex flex-wrap items-center gap-3 rounded-md border bg-primary/10 p-3 text-sm">
      {children}
    </div>
  );
}

type FieldProps = { draft: (f: Setting) => Draft; set: (f: Setting, d: Draft) => void; ro: boolean };

/** Routing entries: one line each, harness, model, effort and fast. */
function Rows({ fields, draft, set, ro }: { fields: Setting[] } & FieldProps) {
  const rows: { name: string; fields: Setting[] }[] = [];
  for (const f of fields) {
    const last = rows[rows.length - 1];
    if (last && last.name === f.row) last.fields.push(f);
    else rows.push({ name: f.row, fields: [f] });
  }
  return (
    <div className="divide-y">
      <div aria-hidden className="hidden gap-2 pb-1.5 text-xs text-muted-foreground md:grid md:grid-cols-[10rem_7rem_1fr_8rem_4rem]">
        <span>Step</span>
        <span>Harness</span>
        <span>Model</span>
        <span>Effort</span>
        <span>Fast</span>
      </div>
      {rows.map((r) => (
        <fieldset key={r.name} className="grid grid-cols-2 items-end gap-2 py-2.5 last:pb-0 md:grid-cols-[10rem_7rem_1fr_8rem_4rem]">
          <legend className="sr-only">{r.name}</legend>
          <span aria-hidden className="col-span-2 self-center font-mono text-sm md:col-span-1">
            {r.name}
          </span>
          {r.fields.map((f) => (
            <label key={key(f)} className="flex min-w-0 flex-col gap-1">
              <span className="text-xs text-muted-foreground md:sr-only">{f.label}</span>
              <Input f={f} d={draft(f)} set={set} ro={ro} label={`${r.name} ${f.label.toLowerCase()}`} />
            </label>
          ))}
        </fieldset>
      ))}
    </div>
  );
}

function Input({ f, d, set, ro, label }: { f: Setting; d: Draft; set: (f: Setting, d: Draft) => void; ro: boolean; label?: string }) {
  const dirty = d !== initial(f);
  const cls = cn(input, dirty && "border-primary");
  // A row's inputs are named by the row; the others by their label, with the help line describing them.
  const a11y = label ? { "aria-label": label, title: f.help } : { id: id(f), "aria-describedby": id(f) + "-help" };
  if (f.kind === "bool") {
    return (
      <span className="flex h-10 items-center md:h-9">
        <input
          type="checkbox"
          {...a11y}
          checked={d === true}
          disabled={ro}
          onChange={(e) => set(f, e.target.checked)}
          className="size-5 accent-primary md:size-4"
        />
      </span>
    );
  }
  if (f.kind === "select") {
    const def = f.default == null ? "Not set" : show(f.default) === "" ? "Default" : `Default (${show(f.default)})`;
    return (
      <select {...a11y} value={d as string} disabled={ro} onChange={(e) => set(f, e.target.value)} className={cls}>
        {f.options.map((o) => (
          <option key={o} value={o}>
            {o === "" ? def : o}
          </option>
        ))}
      </select>
    );
  }
  const numeric = f.kind === "number" || f.kind === "integer";
  return (
    <input
      {...a11y}
      value={d as string}
      disabled={ro}
      inputMode={numeric ? "decimal" : undefined}
      placeholder={f.default != null ? show(f.default) : f.kind === "list" ? "haiku, sonnet, opus" : ""}
      onChange={(e) => set(f, e.target.value)}
      className={cn(cls, (numeric || f.kind === "duration") && "font-mono")}
      spellCheck={false}
      autoCapitalize="off"
      autoComplete="off"
    />
  );
}

const dotted = (c: TuningChange) => c.path.join(".");

/** The tuning loop's proposal, when one waits: what it would change and why, applied through the same save as the form. */
function TuningCard({ s, ro, onApplied }: { s: Settings; ro: boolean; onApplied: (next: Settings) => void }) {
  const qc = useQueryClient();
  const { data } = useTuning();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  // The changes you unticked; the rest that still apply are applied.
  const [skip, setSkip] = useState<ReadonlySet<number>>(new Set());
  const p = data?.proposal;
  if (!p) return null;
  const picked = p.changes.flatMap((c, i) => (c.now === "" && !skip.has(i) ? [i] : []));
  const toggle = (i: number, on: boolean) =>
    setSkip((was) => {
      const next = new Set(was);
      if (on) next.delete(i);
      else next.add(i);
      return next;
    });
  const apply = async () => {
    setBusy(true);
    setErr("");
    try {
      onApplied(await applyTuning(p.id, s.version, picked));
      void qc.invalidateQueries({ queryKey: ["tuning"] });
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      if (e instanceof APIError && e.status === 409) void qc.invalidateQueries({ queryKey: ["settings"] });
    } finally {
      setBusy(false);
    }
  };
  const dismiss = async () => {
    setBusy(true);
    setErr("");
    try {
      const r = await act(`/settings/tuning/${p.id}/dismiss`);
      if (!r.ok) setErr(r.message);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
      void qc.invalidateQueries({ queryKey: ["tuning"] });
    }
  };
  return (
    <Card role="region" aria-label="Tuning proposal" className="border-primary/50">
      <CardHeader className="flex-col items-start gap-0.5">
        <CardTitle className="flex items-center gap-2">
          <Sparkles className="size-4 text-primary" aria-hidden /> Tuning proposal
        </CardTitle>
        <p className="text-xs text-muted-foreground">
          From the metrics for {p.period_from.slice(0, 10)} to {p.period_to.slice(0, 10)}. Nothing changes until you apply it.
        </p>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        <p className="whitespace-pre-wrap">{p.summary}</p>
        {p.changes.length > 0 ? (
          <ul className="space-y-2" aria-label="Proposed changes">
            {p.changes.map((c, i) => (
              <li key={dotted(c)} className={cn("flex gap-2.5 rounded-md border p-2.5", c.now !== "" && "opacity-70")}>
                <input
                  type="checkbox"
                  aria-label={`Apply ${dotted(c)}`}
                  checked={c.now === "" && !skip.has(i)}
                  disabled={ro || busy || c.now !== ""}
                  onChange={(e) => toggle(i, e.target.checked)}
                  className="mt-0.5 size-5 shrink-0 accent-primary md:size-4"
                />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-baseline gap-x-2 font-mono text-sm">
                    <span className="break-all">{dotted(c)}</span>
                    <span className="text-muted-foreground">→</span>
                    <span className="font-medium break-all">{c.value == null ? "default" : show(c.value)}</span>
                  </div>
                  <p className="mt-1 text-muted-foreground">{c.why}</p>
                  {c.now !== "" && (
                    <p className="mt-1 text-xs text-warning">
                      {c.now === "factory.yaml already says that" ? "Already applied: factory.yaml says this now." : `Won't apply now: ${c.now}`}
                    </p>
                  )}
                </div>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-muted-foreground">No setting changes.</p>
        )}
        {p.ideas.length > 0 && (
          <section aria-label="Ideas">
            <h3 className="text-xs font-medium text-muted-foreground">Ideas for prompts and skills</h3>
            <ul className="mt-1 list-disc space-y-1 pl-5">
              {p.ideas.map((idea, i) => (
                <li key={i}>{idea}</li>
              ))}
            </ul>
          </section>
        )}
        {p.dropped.length > 0 && (
          <details>
            <summary className="cursor-pointer text-muted-foreground">
              {p.dropped.length} dropped {p.dropped.length === 1 ? "change" : "changes"}
            </summary>
            <ul className="mt-1 list-disc space-y-1 pl-5">
              {p.dropped.map((c, i) => (
                <li key={i}>
                  <span className="font-mono break-all">{dotted(c)}</span>: {c.reason}
                </li>
              ))}
            </ul>
          </details>
        )}
        {p.diff && (
          <details open>
            <summary className="cursor-pointer text-muted-foreground">Diff of factory.yaml</summary>
            <pre aria-label="Diff" className="mt-1 max-h-72 overflow-auto rounded bg-muted p-2 font-mono text-xs">
              {p.diff.trimEnd().split("\n").map((l, i) => (
                <div
                  key={i}
                  className={cn(
                    "whitespace-pre-wrap break-all",
                    l.startsWith("+") && !l.startsWith("+++") && "bg-success/12",
                    l.startsWith("-") && !l.startsWith("---") && "bg-destructive/12",
                    l.startsWith("@@") && "text-muted-foreground",
                  )}
                >
                  {l || "\u00a0"}
                </div>
              ))}
            </pre>
          </details>
        )}
        {p.pr_url && (
          <p>
            <a href={p.pr_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary underline-offset-2 hover:underline">
              Draft PR with these changes <ExternalLink className="size-3.5" aria-hidden />
            </a>{" "}
            <span className="text-muted-foreground">Applying it here is fine too; close the PR afterwards.</span>
          </p>
        )}
        {!ro && (
          <div className="flex flex-wrap items-center gap-2">
            <Button onClick={apply} disabled={busy || picked.length === 0}>
              <Check /> {picked.length === p.changes.length || picked.length === 0 ? "Apply" : `Apply ${picked.length} of ${p.changes.length}`}
            </Button>
            <Button variant="outline" onClick={dismiss} disabled={busy}>
              <X /> Dismiss
            </Button>
            <span className="text-xs text-muted-foreground">Check model changes with factory eval first.</span>
          </div>
        )}
        {err && (
          <p role="alert" className="text-sm break-words text-destructive">
            {err}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
