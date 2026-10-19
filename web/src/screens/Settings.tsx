import { useState, type ReactNode } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Check, ExternalLink, Lock, RotateCw, Save, Sparkles, X } from "lucide-react";
import {
  act,
  APIError,
  applyTuning,
  saveSettings,
  useSettings,
  useTuning,
  type Setting,
  type Settings,
  type SettingValue,
  type TuningChange,
} from "@/api/client";
import { ErrorBox, Loading, PageTitle } from "@/components/common";
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
            {s.running_jobs > 0
              ? `Restart pending, waiting for ${s.running_jobs} running ${s.running_jobs === 1 ? "job" : "jobs"}. No new jobs start until then.`
              : "Restarting…"}
          </span>
          {!ro && s.running_jobs > 0 && (
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

      {s.groups.map((g) => (
        <Card key={g.name} role="region" aria-label={g.name}>
          <CardHeader className="flex-col items-start gap-0.5">
            <CardTitle>{g.name}</CardTitle>
            {g.help && <p className="text-xs text-muted-foreground">{g.help}</p>}
          </CardHeader>
          <CardContent>
            {g.fields.some((f) => f.row) ? (
              <Rows fields={g.fields} draft={draft} set={set} ro={ro} />
            ) : (
              <div className="grid gap-x-4 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
                {g.fields.map((f) => (
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
            )}
          </CardContent>
        </Card>
      ))}

      {!ro && (
        <div className="sticky bottom-20 z-10 flex flex-wrap items-center gap-3 rounded-lg border bg-surface/95 p-3 shadow-xs backdrop-blur md:bottom-4">
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
    </div>
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
