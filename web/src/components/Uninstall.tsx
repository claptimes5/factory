import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, Check, ExternalLink, Trash2, X } from "lucide-react";
import { APIError, postJSON, useUninstall, type Uninstall, type UninstallResult } from "@/api/client";
import { ErrorBox, Loading } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

/** Settings' "Remove the factory": a dry run first, then removal once confirmed. */
export function RemoveFactory() {
  const [asked, setAsked] = useState(false);
  const [result, setResult] = useState<UninstallResult | null>(null);
  const { data, error, isFetching, refetch } = useUninstall(asked);
  return (
    <Card role="region" aria-label="Remove the factory" data-testid="uninstall">
      <CardHeader className="flex-col items-start gap-0.5">
        <CardTitle>Remove the factory</CardTitle>
        <p className="text-xs text-muted-foreground">
          Deletes the factory's labels from your repos and the job images it built. Your issues, PRs and code stay. You see everything it would
          remove first. From a terminal: <code className="font-mono">./factory uninstall</code>.
        </p>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        {!asked ? (
          <Button variant="outline" size="sm" onClick={() => setAsked(true)}>
            Show what would be removed
          </Button>
        ) : isFetching && !data ? (
          <Loading what="Looking at your repos and images" />
        ) : error ? (
          <ErrorBox error={error} />
        ) : result ? (
          <Result r={result} />
        ) : (
          data && <Plan p={data} onDone={setResult} onRefresh={() => void refetch()} />
        )}
      </CardContent>
    </Card>
  );
}

function Plan({ p, onDone, onRefresh }: { p: Uninstall; onDone: (r: UninstallResult) => void; onRefresh: () => void }) {
  const qc = useQueryClient();
  const [data, setData] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = dialog.current;
    if (!d) return;
    if (confirming && !d.open) d.showModal?.();
    else if (!confirming && d.open) d.close();
  }, [confirming]);
  const withData = data && p.data.exists && !p.data.blocked;
  const nothing = p.labels.length === 0 && p.images.length === 0 && !withData;
  const remove = async () => {
    setBusy(true);
    setErr("");
    try {
      const r = await postJSON<UninstallResult>("/settings/uninstall", { confirm: "remove", data: withData });
      setConfirming(false);
      onDone(r);
      void qc.invalidateQueries({ queryKey: ["uninstall"] });
    } catch (e) {
      setErr(e instanceof APIError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const repos = [...new Set(p.labels.map((l) => l.repo))];
  return (
    <div className="space-y-4">
      <section aria-label="Labels">
        <h3 className="font-medium">Labels</h3>
        <p className="mb-1 text-xs text-warning">Deleting a label takes it off every issue and PR that has it.</p>
        {p.labels.length === 0 && p.repos.length === 0 && <p className="text-muted-foreground">None.</p>}
        {repos.map((r) => (
          <div key={r} className="mt-1">
            <div className="text-xs font-semibold text-muted-foreground">{r}</div>
            <ul className="flex flex-wrap gap-1.5" data-testid="uninstall-labels">
              {p.labels
                .filter((l) => l.repo === r)
                .map((l) => (
                  <li key={l.name} className="rounded border px-1.5 py-0.5 font-mono text-xs">
                    {l.name} <span className="text-muted-foreground">{openCount(l)} open</span>
                  </li>
                ))}
            </ul>
          </div>
        ))}
        {p.repos.map((r) => (
          <p key={r.repo} className="text-xs text-destructive">
            {r.repo}: couldn't read its labels: {r.error}
          </p>
        ))}
      </section>
      <section aria-label="Job images">
        <h3 className="font-medium">Job images</h3>
        {p.images_note ? (
          <p className="text-muted-foreground">{p.images_note}</p>
        ) : p.images.length === 0 ? (
          <p className="text-muted-foreground">None.</p>
        ) : (
          <ul className="space-y-0.5 font-mono text-xs" data-testid="uninstall-images">
            {p.images.map((im) => (
              <li key={im.ref}>
                {im.ref} <span className="text-muted-foreground">{im.size}</span>
                {im.dangling && <span className="text-muted-foreground"> (an old copy)</span>}
              </li>
            ))}
          </ul>
        )}
      </section>
      {p.kept.length > 0 && (
        <section aria-label="Left alone">
          <h3 className="font-medium">Left alone</h3>
          <ul className="mt-1 list-disc space-y-0.5 pl-5" data-testid="uninstall-kept">
            {p.kept.map((k) => (
              <li key={k.what}>
                <span className="font-mono text-xs">{k.what}</span>: {k.why}
              </li>
            ))}
          </ul>
        </section>
      )}
      <section aria-label="Data">
        <h3 className="font-medium">Data on this machine</h3>
        {p.data.exists && p.data.blocked ? (
          <p className="text-muted-foreground" data-testid="uninstall-data-blocked">
            {p.data.blocked}
          </p>
        ) : p.data.exists ? (
          <label className="mt-1 flex items-start gap-2">
            <input type="checkbox" className="mt-1" checked={data} onChange={(e) => setData(e.target.checked)} />
            <span>
              Also remove the job data ({p.data.size}): {p.data.what}
              {p.data.note && <span className="block text-xs text-muted-foreground">{p.data.note}</span>}
            </span>
          </label>
        ) : (
          <p className="text-muted-foreground">{p.data.note}</p>
        )}
      </section>
      <Manual steps={p.manual} />
      {p.refused && (
        <p role="note" className="flex items-start gap-1.5 text-warning" data-testid="uninstall-refused">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" /> {p.refused}
        </p>
      )}
      {!p.writable && <p className="text-xs text-muted-foreground">{p.reason}</p>}
      <div className="flex flex-wrap gap-2">
        <Button variant="destructive" size="sm" disabled={!p.writable || !!p.refused || nothing} onClick={() => setConfirming(true)}>
          <Trash2 /> Remove…
        </Button>
        <Button variant="ghost" size="sm" onClick={onRefresh}>
          Look again
        </Button>
      </div>
      <dialog
        ref={dialog}
        aria-label="Remove the factory?"
        onClose={() => setConfirming(false)}
        className="mx-auto mt-[10vh] w-[min(560px,calc(100vw-32px))] rounded-card border bg-surface p-4 text-foreground shadow-2xl backdrop:bg-black/50"
      >
        <h2 className="text-title-sm font-semibold">Remove the factory?</h2>
        <div className="mt-2 max-h-[55vh] space-y-2 overflow-y-auto text-sm">
          {p.labels.length > 0 && (
            <p>
              Delete {p.labels.length} {p.labels.length === 1 ? "label" : "labels"} from {repos.join(", ")}:{" "}
              {repos.map((r, i) => (
                <span key={r}>
                  {i > 0 && "; "}
                  {r}{" "}
                  <span className="font-mono text-xs">
                    {p.labels
                      .filter((l) => l.repo === r)
                      .map((l) => l.name)
                      .join(", ")}
                  </span>
                </span>
              ))}
              . Each comes off every issue and PR that has it (
              {p.labels.reduce((n, l) => n + Math.max(0, l.open), 0)}
              {p.labels.some((l) => l.open_capped) ? "+" : ""} open now).
            </p>
          )}
          {p.images.length > 0 && (
            <p>
              Remove {p.images.length} job {p.images.length === 1 ? "image" : "images"}:{" "}
              <span className="font-mono text-xs">{p.images.map((im) => im.ref).join(", ")}</span>.
            </p>
          )}
          {withData && <p>Remove the job data: {p.data.what}</p>}
          <p className="text-muted-foreground">
            The GitHub App, your tokens, factory.yaml and factory.env stay; the result says where to remove them. This can't be undone.
          </p>
          {err && (
            <p role="alert" className="text-destructive">
              {err}
            </p>
          )}
        </div>
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" onClick={() => setConfirming(false)} disabled={busy}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={remove} disabled={busy}>
            {busy ? "Removing…" : "Remove"}
          </Button>
        </div>
      </dialog>
    </div>
  );
}

function Result({ r }: { r: UninstallResult }) {
  const failed = r.done.filter((d) => !d.ok).length;
  return (
    <div className="space-y-3" data-testid="uninstall-result">
      <p role="status" className={failed ? "text-warning" : "text-success"}>
        {failed ? `${failed} of ${r.done.length} couldn't be removed.` : `Removed ${r.done.length}.`}
      </p>
      <ul className="space-y-0.5">
        {r.done.map((d) => (
          <li key={d.what} className="flex items-start gap-1.5">
            {d.ok ? <Check className="mt-0.5 size-4 shrink-0 text-success" aria-label="removed" /> : <X className="mt-0.5 size-4 shrink-0 text-destructive" aria-label="failed" />}
            <span>
              {d.what}
              {d.error && <span className="text-destructive">: {d.error}</span>}
            </span>
          </li>
        ))}
      </ul>
      <Manual steps={r.manual} />
    </div>
  );
}

function Manual({ steps }: { steps: Uninstall["manual"] }) {
  if (steps.length === 0) return null;
  return (
    <section aria-label="Left for you to do">
      <h3 className="font-medium">Left for you to do</h3>
      <ul className="mt-1 list-disc space-y-1 pl-5">
        {steps.map((s) => (
          <li key={s.text + s.url}>
            {s.text}
            {s.url && (
              <>
                {" "}
                <a href={s.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 break-all text-primary hover:underline">
                  {s.url.replace(/^https:\/\//, "")} <ExternalLink className="size-3" />
                </a>
              </>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}

const openCount = (l: Uninstall["labels"][number]) => (l.open < 0 ? "?" : l.open_capped ? `${l.open}+` : String(l.open));
