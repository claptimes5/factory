import { useEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { Link, useSearchParams } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { AlertCircle, AlertTriangle, CheckCircle2, ChevronDown, Circle, ExternalLink, Loader2, XCircle } from "lucide-react";
import {
  getJSON,
  machineCheckStalled,
  postJSON,
  useWizard,
  wizardDo,
  type Wizard,
  type WizardAccess,
  type WizardChecks,
  type WizardManifest,
  type WizardSecret,
  type WizardStep,
} from "@/api/client";
import { ErrorBox, Loading, PageTitle } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// The get-started page: what `factory setup` asks in the terminal, as a
// list of steps. Each step saves as it goes, to factory.yaml and
// factory.env on the factory's machine. Saved secrets never come back
// here: only whether each is set, and a masked hint.

const input =
  "h-10 w-full min-w-0 rounded-md border bg-card px-2.5 text-base text-foreground placeholder:text-muted-foreground/70 disabled:opacity-60 md:h-9 md:text-sm";

type Msg = { ok: boolean; text: string } | null;

/** Runs a wizard action, putting the page it returns in place, and keeps its message. */
function useAction() {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const [msg, setMsg] = useState<Msg>(null);
  const run = async (action: string, body: Record<string, unknown> = {}) => {
    setBusy(action);
    setMsg(null);
    try {
      const r = await wizardDo(action, body);
      qc.setQueryData(["wizard"], r.wizard);
      setMsg({ ok: true, text: r.message });
      return r;
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
      return null;
    } finally {
      setBusy("");
    }
  };
  return { busy, msg, setMsg, run };
}

function Message({ msg }: { msg: Msg }) {
  if (!msg) return null;
  return (
    <p role={msg.ok ? "status" : "alert"} className={cn("text-sm break-words", msg.ok ? "text-success" : "text-destructive")}>
      {msg.text}
    </p>
  );
}

function Code({ children }: { children: ReactNode }) {
  return <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs break-all">{children}</code>;
}

function A({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a href={href} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 break-all text-primary underline-offset-2 hover:underline">
      {children}
      <ExternalLink className="size-3 shrink-0" aria-hidden />
    </a>
  );
}

/** A secret's field: what was typed goes up once and never comes back, so it shows only whether one is saved. */
function SecretField({
  id,
  label,
  saved,
  value,
  set,
  disabled,
  placeholder,
}: {
  id: string;
  label: string;
  saved?: WizardSecret;
  value: string;
  set: (v: string) => void;
  disabled?: boolean;
  placeholder?: string;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      <input
        id={id}
        type="password"
        autoComplete="off"
        spellCheck={false}
        className={input}
        value={value}
        disabled={disabled}
        placeholder={saved?.set ? `Saved: ${saved.masked}. Paste a new one to replace it.` : placeholder}
        onChange={(e) => set(e.target.value)}
      />
    </div>
  );
}

function TextField({
  id,
  label,
  value,
  set,
  disabled,
  placeholder,
  help,
}: {
  id: string;
  label: string;
  value: string;
  set: (v: string) => void;
  disabled?: boolean;
  placeholder?: string;
  help?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      <input id={id} className={input} value={value} disabled={disabled} placeholder={placeholder} spellCheck={false} onChange={(e) => set(e.target.value)} />
      {help && <span className="text-xs text-muted-foreground">{help}</span>}
    </div>
  );
}

const stateText: Record<string, [string, "success" | "warning" | "secondary"]> = {
  done: ["Done", "success"],
  attention: ["Needs attention", "warning"],
  optional: ["Optional", "secondary"],
};

function StepIcon({ state }: { state: string }) {
  if (state === "done") return <CheckCircle2 aria-hidden className="size-5 shrink-0 text-success" />;
  if (state === "attention") return <AlertCircle aria-hidden className="size-5 shrink-0 text-warning" />;
  return <Circle aria-hidden className="size-5 shrink-0 text-muted-foreground" />;
}

function StepCard({ step, n, open, toggle, children }: { step: WizardStep; n: number; open: boolean; toggle: () => void; children: ReactNode }) {
  const [label, variant] = stateText[step.state] ?? [step.state, "secondary"];
  return (
    <li data-testid="wizard-step" data-key={step.key} data-state={step.state} className="rounded-xl border bg-card">
      <h2>
        <button
          aria-expanded={open}
          aria-controls={`step-${step.key}`}
          onClick={toggle}
          className="flex w-full min-w-0 cursor-pointer items-start gap-3 p-4 text-left"
        >
          <StepIcon state={step.state} />
          <span className="flex min-w-0 flex-1 flex-col gap-0.5">
            <span className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
              <span className="font-display text-[15px] font-semibold">
                {n}. {step.title}
              </span>
              <Badge variant={variant}>{label}</Badge>
            </span>
            <span className="text-[13px] break-words text-text-2">{step.summary}</span>
          </span>
          <ChevronDown aria-hidden className={cn("mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-180")} />
        </button>
      </h2>
      {open && (
        <div id={`step-${step.key}`} className="flex min-w-0 flex-col gap-3 border-t px-4 pt-3 pb-4">
          {children}
        </div>
      )}
    </li>
  );
}

const findingIcon = {
  pass: <CheckCircle2 aria-label="OK" className="size-4 shrink-0 text-success" />,
  warn: <AlertTriangle aria-label="Worth knowing" className="size-4 shrink-0 text-warning" />,
  fail: <XCircle aria-label="To fix" className="size-4 shrink-0 text-destructive" />,
};

function MachineStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const { busy, msg, run } = useAction();
  return (
    <>
      <p className="text-sm text-text-2">What the factory needs on this machine. Fix the red lines, then check again.</p>
      {w.machine_checked_at === "" && machineCheckStalled() ? (
        <p role="alert" className="text-sm text-destructive">The machine check didn't finish. Docker may be hung; check again.</p>
      ) : w.machine_checked_at === "" ? (
        <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" aria-hidden /> Checking…
        </p>
      ) : (
        <ul className="flex flex-col gap-2" aria-label="Checks of this machine">
          {w.machine.map((f) => (
            <li key={f.what} data-testid="machine-finding" className="flex min-w-0 gap-2 text-sm">
              <span className="mt-0.5">{findingIcon[f.status]}</span>
              <span className="flex min-w-0 flex-1 flex-col gap-1">
                <span className="break-words">{f.what}</span>
                {f.status !== "pass" && f.fix && (
                  <pre className="overflow-x-auto rounded-md bg-muted p-2 font-mono text-xs whitespace-pre-wrap break-all">{f.fix}</pre>
                )}
                {f.status !== "pass" && f.action === "build_images" && (
                  <span>
                    <Button size="sm" variant="outline" disabled={ro || !!busy || !!w.images?.building} onClick={() => run("images/build")}>
                      {w.images?.building ? "Building…" : "Build"}
                    </Button>
                  </span>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
      <div>
        <Button size="sm" variant="outline" disabled={ro || !!busy} onClick={() => run("machine/check")}>
          {busy === "machine/check" ? "Checking…" : "Re-check"}
        </Button>
      </div>
      <Message msg={msg} />
    </>
  );
}

function ReposStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const [v, setV] = useState(w.repos.join(", "));
  const { busy, msg, run } = useAction();
  return (
    <>
      <p className="text-sm text-text-2">The factory can work on several repos, each on its own. They are saved in factory.yaml.</p>
      <TextField
        id="wizard-repos"
        label="Repositories"
        value={v}
        set={setV}
        disabled={ro}
        placeholder="octocat/hello-world"
        help="owner/name for GitHub, and the project's URL for GitLab (https://gitlab.com/group/project). Separate them with commas."
      />
      <div>
        <Button size="sm" disabled={ro || !!busy} onClick={() => run("repos", { repos: v })}>
          {busy ? "Saving…" : "Save"}
        </Button>
      </div>
      <Message msg={msg} />
    </>
  );
}

/** Each repo on one host: whether the factory reaches it, and what to try when not. */
function AccessList({ w, host, ro }: { w: Wizard; host: "github" | "gitlab"; ro: boolean }) {
  const { busy, msg, run } = useAction();
  const repos = w.access.filter((a) => a.host === host);
  return (
    <section aria-label="Access to each repo" className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold">Access to each repo</h3>
      <ul className="flex flex-col gap-2">
        {repos.map((a) => (
          <AccessRow key={a.repo} a={a} ro={ro} />
        ))}
      </ul>
      <div>
        <Button size="sm" variant="outline" disabled={ro || !!busy} onClick={() => run("access/check")}>
          {busy ? "Checking…" : "Check access"}
        </Button>
      </div>
      <Message msg={msg} />
    </section>
  );
}

function AccessRow({ a, ro }: { a: WizardAccess; ro: boolean }) {
  const { busy, msg, run } = useAction();
  return (
    <li data-testid="access" className="flex min-w-0 flex-col gap-1 text-sm">
      <span className="flex min-w-0 items-start gap-2">
        <span className="mt-0.5">
          {a.state === "ok" ? findingIcon.pass : a.state === "failed" ? findingIcon.fail : <Circle aria-label="Not checked" className="size-4 text-muted-foreground" />}
        </span>
        <span className="min-w-0 flex-1 break-words">
          <span className="font-mono break-all">{a.repo}</span>
          {a.state === "ok" && ": the factory can reach it."}
          {a.state === "unchecked" && ": not checked yet."}
          {a.state === "failed" && `: ${a.error}`}
        </span>
      </span>
      {a.missing_labels.length > 0 && (
        <p className="pl-6 text-xs text-warning">
          Missing {a.missing_labels.length === 1 ? "label" : "labels"}: <span className="font-mono break-all">{a.missing_labels.join(", ")}</span>
        </p>
      )}
      {a.labels_error && <p className="pl-6 text-xs text-destructive">Couldn't list its labels: {a.labels_error}</p>}
      {a.hints.map((h) => (
        <p key={h} className="pl-6 text-xs text-text-2">
          {h}
        </p>
      ))}
      {a.default_branch && (
        <div className="flex flex-wrap items-center gap-2 pl-6 text-xs text-text-2">
          <span>
            Its default branch is <Code>{a.default_branch}</Code>, but the factory will use <Code>{a.base_branch}</Code>.
            {!a.can_use_default && " To change it, give that repo its own base_branch under repos: in factory.yaml."}
          </span>
          {a.can_use_default && (
            <Button size="sm" variant="outline" disabled={ro || !!busy} onClick={() => run("base-branch", { repo: a.repo, branch: a.default_branch })}>
              Use {a.default_branch}
            </Button>
          )}
        </div>
      )}
      <div className="pl-6">
        <Message msg={msg} />
      </div>
    </li>
  );
}

function Labels({ w, host, ro }: { w: Wizard; host: "github" | "gitlab"; ro: boolean }) {
  const { busy, msg, run } = useAction();
  const repos = new Set(w.access.filter((a) => a.host === host).map((a) => a.repo));
  const done = w.labels.filter((l) => repos.has(l.repo));
  const reach = w.access.some((a) => a.host === host && a.state === "ok");
  const missing = w.access.filter((a) => a.host === host).reduce((n, a) => n + a.missing_labels.length, 0);
  return (
    <section aria-label="Labels" className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold">Labels</h3>
      <p className="text-sm text-text-2">
        The factory uses {w.label_names.length} labels on each repo, such as <Code>{w.label_names[0]}</Code>.{" "}
        {missing > 0 ? `${missing} ${missing === 1 ? "is" : "are"} missing. Creating them leaves the others as they are.` : "Every one is there."}
      </p>
      <div>
        <Button size="sm" variant={missing > 0 ? "default" : "outline"} disabled={ro || !reach || missing === 0 || !!busy} onClick={() => run("labels")}>
          {busy ? "Creating…" : "Create missing labels"}
        </Button>
      </div>
      <Message msg={msg} />
      {done.length > 0 && (
        <ul className="flex flex-col gap-1 text-xs" aria-label="What happened to each label">
          {done.map((l) => (
            <li key={l.repo + l.name} data-testid="label" className="flex min-w-0 flex-wrap gap-x-2">
              <span className="font-mono break-all">{repos.size > 1 ? `${l.repo} ${l.name}` : l.name}</span>
              <span className={cn(l.result === "failed" ? "text-destructive" : l.result === "created" ? "text-success" : "text-muted-foreground")}>
                {l.result === "created" ? "created" : l.result === "exists" ? "already there" : l.error}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** Posts GitHub's new-app form, with the manifest the server prepared, from this page. */
function postToGitHub(url: string, manifest: string) {
  const f = document.createElement("form");
  f.method = "post";
  f.action = url;
  const i = document.createElement("input");
  i.type = "hidden";
  i.name = "manifest";
  i.value = manifest;
  f.appendChild(i);
  document.body.appendChild(f);
  f.submit();
}

function GitHubStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const g = w.github!;
  const [msg, setMsg] = useState<Msg>(null);
  const [starting, setStarting] = useState(false);
  const check = useAction();
  const create = async () => {
    setStarting(true);
    setMsg(null);
    try {
      const m = await postJSON<WizardManifest>("/setup/github/app", {});
      postToGitHub(m.url, m.manifest);
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
      setStarting(false);
    }
  };
  const installed = g.method === "app" && g.installation_id !== "" && g.missing.length === 0;
  return (
    <>
      <p className="text-sm text-text-2">
        The factory acts on GitHub through a GitHub App limited to {w.access.filter((a) => a.host === "github").length > 1 ? "your repos" : "your repo"}. It
        can read and write code, issues and pull requests, and read checks and workflow runs. Nothing else.
      </p>
      {g.method === "app" ? (
        <p className="text-sm">
          Using the app <span className="font-mono break-all">{g.app_slug || g.app_id}</span>
          {g.key_path && (
            <>
              {" "}
              with the key <span className="font-mono break-all">{g.key_path}</span>
            </>
          )}
          .
        </p>
      ) : g.method === "token" ? (
        <p className="text-sm">Using a personal access token ({g.token.masked}). An app is safer for everyday use.</p>
      ) : null}
      {g.method !== "app" && (
        <div className="flex flex-col gap-1.5">
          <div>
            <Button disabled={ro || starting} onClick={create}>
              {starting ? "Opening GitHub…" : "Create an app for me"}
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            GitHub shows a form with everything filled in, for the account {g.app_owner}. Click "Create GitHub App" at the bottom, and GitHub brings you back here.
          </p>
          <Message msg={msg} />
        </div>
      )}
      {g.method === "app" && !installed && (
        <section aria-label="Install the app" className="flex flex-col gap-2 rounded-lg border border-warning-border bg-warning-bg p-3 text-sm">
          <h3 className="font-semibold">Install the app</h3>
          {g.install_url ? (
            <p>
              Open <A href={g.install_url}>{g.install_url}</A>
            </p>
          ) : (
            <p>Open the app's settings on GitHub and choose Install App.</p>
          )}
          {g.install_steps.length > 0 ? (
            g.install_steps.map((s) => <p key={s}>{s}</p>)
          ) : (
            <p>Choose "Only select repositories", select your repos, and click Install.</p>
          )}
          {g.install_error && <p className="text-destructive">Couldn't check: {g.install_error}</p>}
          <div>
            <Button size="sm" disabled={ro || !!check.busy} onClick={() => check.run("github/install-check")}>
              {check.busy ? "Checking…" : "Check"}
            </Button>
          </div>
          <Message msg={check.msg} />
        </section>
      )}
      {g.method !== "none" && <AccessList w={w} host="github" ro={ro} />}
      {w.access.some((a) => a.host === "github" && a.state === "ok") && <Labels w={w} host="github" ro={ro} />}
      <Alternatives w={w} ro={ro} />
    </>
  );
}

function Alternatives({ w, ro }: { w: Wizard; ro: boolean }) {
  const g = w.github!;
  const [appID, setAppID] = useState(g.app_id);
  const [keyPath, setKeyPath] = useState(g.key_path);
  const [token, setToken] = useState("");
  const app = useAction();
  const tok = useAction();
  return (
    <details className="rounded-lg border p-3" open={g.method === "token" || undefined}>
      <summary className="cursor-pointer text-sm font-medium">Other ways to connect</summary>
      <div className="mt-3 flex flex-col gap-4">
        <section aria-label="Use an app I already made" className="flex flex-col gap-2">
          <h3 className="text-sm font-semibold">Use an app I already made</h3>
          <div className="grid gap-3 sm:grid-cols-2">
            <TextField id="wizard-app-id" label="App ID" value={appID} set={setAppID} disabled={ro} help="Under About on the app's settings page." />
            <TextField
              id="wizard-key-path"
              label="Private key file (.pem)"
              value={keyPath}
              set={setKeyPath}
              disabled={ro}
              placeholder="/home/you/factory/app.pem"
              help="Its path on the factory's machine."
            />
          </div>
          <div>
            <Button size="sm" variant="outline" disabled={ro || !!app.busy} onClick={() => app.run("github/existing-app", { app_id: appID, key_path: keyPath })}>
              {app.busy ? "Checking…" : "Use this app"}
            </Button>
          </div>
          <Message msg={app.msg} />
        </section>
        <section aria-label="Use a personal access token" className="flex flex-col gap-2">
          <h3 className="text-sm font-semibold">Use a personal access token (only for a quick try)</h3>
          <p className="text-sm text-text-2">
            Make a fine-grained token at <A href={g.token_url}>{g.token_url}</A> for your repos only, with Contents, Issues and Pull requests set to Read and
            write.
          </p>
          <SecretField id="wizard-github-token" label="Token" saved={g.token} value={token} set={setToken} disabled={ro} placeholder="github_pat_…" />
          <div>
            <Button
              size="sm"
              variant="outline"
              disabled={ro || !!tok.busy || token.trim() === ""}
              onClick={async () => {
                if (await tok.run("github/token", { token })) setToken("");
              }}
            >
              {tok.busy ? "Checking…" : "Use this token"}
            </Button>
          </div>
          <Message msg={tok.msg} />
        </section>
      </div>
    </details>
  );
}

function GitLabStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const g = w.gitlab!;
  const [kind, setKind] = useState<"legacy" | "fine">("legacy");
  const [token, setToken] = useState("");
  const { busy, msg, run } = useAction();
  return (
    <>
      <p className="text-sm text-text-2">
        The factory acts on GitLab with an access token. Its owner needs the Developer role on each project (Maintainer if the base branch is protected from
        Developers).
      </p>
      <fieldset className="flex flex-col gap-2">
        <legend className="mb-1 text-sm font-medium">Which kind of token will you make?</legend>
        <label className="flex items-start gap-2 text-sm">
          <input type="radio" name="gitlab-kind" className="mt-1" checked={kind === "legacy"} onChange={() => setKind("legacy")} />
          <span>Legacy, the quick one: one link opens the form with its scopes already ticked.</span>
        </label>
        <label className="flex items-start gap-2 text-sm">
          <input type="radio" name="gitlab-kind" className="mt-1" checked={kind === "fine"} onChange={() => setKind("fine")} />
          <span>Fine-grained: limited to these projects, but about 20 boxes to tick by hand.</span>
        </label>
      </fieldset>
      {kind === "legacy" ? (
        <div className="flex flex-col gap-1.5 text-sm">
          {g.hosts.map((h) => (
            <p key={h.host}>
              Open <A href={h.legacy_url}>{h.host}'s token form</A>, set an expiry date and click Generate token.
            </p>
          ))}
          <p className="text-text-2">It makes a personal token with the api and write_repository scopes. A project, group or bot account token with those scopes works too.</p>
        </div>
      ) : (
        <div className="flex flex-col gap-1.5 text-sm">
          {g.hosts.map((h) => (
            <p key={h.host}>
              Open <A href={h.tokens_url}>{h.tokens_url}</A> and choose Generate token, then Fine-grained token.
            </p>
          ))}
          <p>On the Group and project tab, add each project and give it:</p>
          <ul className="list-disc pl-5 text-text-2">
            {g.fine_grained.map((l) => (
              <li key={l}>{l}</li>
            ))}
          </ul>
          <p>On the User tab, User: Read. On the Global tab, Metadata: Read.</p>
        </div>
      )}
      <SecretField id="wizard-gitlab-token" label="GitLab token" saved={g.token} value={token} set={setToken} disabled={ro} placeholder="glpat-…" />
      <div>
        <Button
          size="sm"
          disabled={ro || !!busy || token.trim() === ""}
          onClick={async () => {
            if (await run("gitlab/token", { token })) setToken("");
          }}
        >
          {busy ? "Checking…" : "Save and check"}
        </Button>
      </div>
      <Message msg={msg} />
      {g.token.set && <AccessList w={w} host="gitlab" ro={ro} />}
      {w.access.some((a) => a.host === "gitlab" && a.state === "ok") && <Labels w={w} host="gitlab" ro={ro} />}
    </>
  );
}

/** Each repo's checks: what every change must pass. Suggested from its files and its AGENTS.md or CLAUDE.md, confirmed here. */
function ChecksStep({ w, ro }: { w: Wizard; ro: boolean }) {
  return (
    <>
      <p className="text-sm text-text-2">
        Every change must pass these checks before review, so each must pass on the base branch first. The factory suggests them from each repo's
        files, and takes the test and lint commands from its AGENTS.md or CLAUDE.md when they name one. Check them: a wrong test command can pass
        without running a test.
      </p>
      {w.checks.map((c) => (
        // Remounted once its files are read, so the fields take the suggestion; not on each save, which would drop its message.
        <RepoChecks key={c.repo + (c.stack ? ":read" : "")} c={c} ro={ro} several={w.checks.length > 1} />
      ))}
    </>
  );
}

const standardGates = ["types", "lint", "test", "build"];

function RepoChecks({ c, ro, several }: { c: WizardChecks; ro: boolean; several: boolean }) {
  const { busy, msg, run } = useAction();
  const [install, setInstall] = useState(c.install);
  const [image, setImage] = useState(c.image_from);
  const [cmds, setCmds] = useState<Record<string, string>>(() => Object.fromEntries(c.gates.map((g) => [g.gate, g.command])));
  const names = [...c.gates.map((g) => g.gate), ...standardGates.filter((g) => !c.gates.some((x) => x.gate === g))];
  const id = (s: string) => `checks-${c.repo.replace(/[^a-z0-9]/gi, "-")}-${s}`;
  if (!c.stack && !c.saved) {
    return <p className="text-sm text-muted-foreground">{c.repo}: connect the factory to it above, so it can read the repo's files and suggest its checks.</p>;
  }
  return (
    <div className="flex flex-col gap-3 rounded-md border p-3" data-testid="repo-checks">
      <p className="text-sm">
        {several && <span className="font-medium">{c.repo}: </span>}
        {c.stack ? `Looks like ${c.stack}.` : ""} {c.saved ? "Saved in factory.yaml." : "Suggested, not saved yet."}
      </p>
      <TextField id={id("install")} label="Install" value={install} set={setInstall} disabled={ro} placeholder="bundle install" help={c.suggested_install && `Suggested: ${c.suggested_install}`} />
      {names.map((g) => {
        const row = c.gates.find((x) => x.gate === g);
        const from = row?.source ? `from ${row.source}` : row && c.stack ? "guessed from the repo's files" : "";
        return (
          <div key={g} className="flex min-w-0 flex-col gap-1">
            <label htmlFor={id(g)} className="text-sm font-medium">
              {g}
            </label>
            <input id={id(g)} className={cn(input, "font-mono")} value={cmds[g] ?? ""} disabled={ro} placeholder="none" spellCheck={false}
              onChange={(e) => setCmds({ ...cmds, [g]: e.target.value })} />
            {row?.suggested ? (
              <span className="flex flex-wrap items-center gap-2 text-xs text-warning">
                {row.source || "The repo's files"} suggest <Code>{row.suggested}</Code>
                <Button size="sm" variant="outline" className="h-7" disabled={ro} onClick={() => setCmds({ ...cmds, [g]: row.suggested })}>
                  Use it
                </Button>
              </span>
            ) : (
              from && <span className="text-xs text-muted-foreground">{from}</span>
            )}
          </div>
        );
      })}
      <TextField id={id("image")} label="Job image built on" value={image} set={setImage} disabled={ro} placeholder="the factory's default image" help="A Docker image with the repo's language, such as ruby:3.4-bookworm." />
      {c.notes.map((n) => (
        <p key={n} className="text-xs text-muted-foreground">
          {n}
        </p>
      ))}
      <div>
        <Button size="sm" disabled={ro || !!busy} onClick={() => run("checks/save", { repo: c.repo, install, image_from: image, commands: cmds })}>
          {busy ? "Saving…" : c.saved ? "Save" : "Use these checks"}
        </Button>
      </div>
      <Message msg={msg} />
    </div>
  );
}

function ClaudeStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const c = w.claude!;
  const [key, setKey] = useState("");
  const [odd, setOdd] = useState(false);
  const { busy, msg, run } = useAction();
  const save = async (anyway: boolean) => {
    const r = await run("claude", { key, anyway });
    setOdd(!r && !anyway && key.trim() !== "" && !key.trim().startsWith("sk-ant-"));
    if (r) setKey("");
  };
  return (
    <>
      <p className="text-sm text-text-2">
        Claude writes the code. Paste an API key from <A href="https://console.anthropic.com/settings/keys">console.anthropic.com</A>, or, to use a Claude
        subscription, run <Code>claude setup-token</Code> and paste the token it prints.
      </p>
      <SecretField id="wizard-claude" label="API key or subscription token" saved={c.key} value={key} set={setKey} disabled={ro} placeholder="sk-ant-…" />
      <div className="flex flex-wrap gap-2">
        <Button size="sm" disabled={ro || !!busy || key.trim() === ""} onClick={() => save(false)}>
          {busy ? "Saving…" : "Save"}
        </Button>
        {odd && (
          <Button size="sm" variant="outline" disabled={ro || !!busy} onClick={() => save(true)}>
            Use it anyway
          </Button>
        )}
      </div>
      <Message msg={msg} />
    </>
  );
}

const codexModes = [
  { mode: "api-key", label: "An OpenAI API key, billed per use. OpenAI recommends this for automation." },
  { mode: "chatgpt", label: "A ChatGPT plan (Plus, Pro, Business or Enterprise), using the plan's limits." },
  { mode: "access-token", label: "A Codex access token from a ChatGPT Business or Enterprise workspace." },
] as const;

function CodexStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const c = w.codex!;
  const [mode, setMode] = useState<string>(c.mode);
  const [key, setKey] = useState("");
  const { busy, msg, run } = useAction();
  return (
    <>
      <p className="text-sm text-text-2">
        {c.review_only ? "factory.yaml has Codex review Claude's work." : "factory.yaml runs every job on Codex."} Codex can sign in with:
      </p>
      <fieldset className="flex flex-col gap-2">
        <legend className="sr-only">How Codex signs in</legend>
        {codexModes.map((m) => (
          <label key={m.mode} className="flex items-start gap-2 text-sm">
            <input type="radio" name="codex-mode" className="mt-1" checked={mode === m.mode} onChange={() => setMode(m.mode)} />
            <span>{m.label}</span>
          </label>
        ))}
      </fieldset>
      {mode === "api-key" && (
        <>
          <p className="text-sm text-text-2">
            Paste an API key from <A href="https://platform.openai.com/api-keys">platform.openai.com</A>. No OpenAI account? See Claude-only review in
            docs/configuration.md.
          </p>
          <SecretField id="wizard-openai" label="OpenAI API key" saved={c.api_key} value={key} set={setKey} disabled={ro} placeholder="sk-…" />
        </>
      )}
      {mode === "chatgpt" && (
        <div className="flex flex-col gap-2 text-sm">
          <ul className="list-disc pl-5 text-text-2">
            <li>Don't use it for public repos, where anyone can put text in front of Codex.</li>
            <li>Codex runs one job at a time, since two jobs can't share one sign-in.</li>
            <li>Codex refreshes the sign-in about every 8 days, and the factory keeps the new one.</li>
            <li>Jobs cost $0 in the factory's budget; the plan's limits show on the Spend page.</li>
          </ul>
          <CodexSignIn c={c} ro={ro} />
        </div>
      )}
      {mode === "access-token" && (
        <>
          <p className="text-sm text-text-2">
            See <A href="https://learn.chatgpt.com/codex/enterprise/access-tokens">how to make one</A>. When it expires, make a new one and save it here.
          </p>
          <SecretField id="wizard-codex-token" label="Codex access token" saved={c.access_token} value={key} set={setKey} disabled={ro} />
        </>
      )}
      <div>
        <Button
          size="sm"
          disabled={ro || !!busy}
          onClick={async () => {
            if (await run("codex", { mode, key })) setKey("");
          }}
        >
          {busy ? "Saving…" : "Save"}
        </Button>
      </div>
      <Message msg={msg} />
    </>
  );
}

/** Codex's device-code sign-in, run on the factory's machine: it shows a link and a code to enter there. */
function CodexSignIn({ c, ro }: { c: NonNullable<Wizard["codex"]>; ro: boolean }) {
  const { busy, msg, run } = useAction();
  const si = c.sign_in;
  return (
    <div className="flex flex-col gap-2" aria-label="ChatGPT sign-in" role="group">
      <p>
        {c.mode === "chatgpt" && c.signed_in
          ? "Codex is signed in. Sign in again if it was signed out."
          : "Sign in with your ChatGPT account. Signing in also switches Codex to the plan."}{" "}
        If Codex says device code sign-in is off, turn it on in ChatGPT's security settings (or your workspace's) and try again.
      </p>
      {!si.running && (
        <div>
          <Button size="sm" variant="outline" disabled={ro || !!busy} onClick={() => run("codex/sign-in")}>
            {busy === "codex/sign-in" ? "Starting…" : c.signed_in ? "Sign in again" : "Sign in"}
          </Button>
        </div>
      )}
      <Message msg={msg} />
      {si.running && ro && (
        <p role="status" className="flex items-center gap-2 text-muted-foreground">
          <Loader2 className="size-4 animate-spin" aria-hidden /> A sign-in is running on the factory's machine.
        </p>
      )}
      {si.running && !ro && (
        <div role="status" className="flex flex-col gap-2 rounded-md border bg-muted/50 p-3">
          {si.url && si.code ? (
            <>
              <p>
                1. Open <A href={si.url}>{si.url}</A> and sign in to ChatGPT.
              </p>
              <p className="flex flex-wrap items-center gap-2">
                2. Enter this code:{" "}
                <code data-testid="device-code" className="rounded bg-card px-2 py-1 font-mono text-base font-semibold tracking-wider">
                  {si.code}
                </code>
              </p>
              <p className="flex items-center gap-2 text-muted-foreground">
                <Loader2 className="size-4 animate-spin" aria-hidden /> Waiting for you to enter it. The code expires in 15 minutes.
              </p>
            </>
          ) : (
            <p className="flex items-center gap-2 text-muted-foreground">
              <Loader2 className="size-4 animate-spin" aria-hidden /> Starting Codex…
            </p>
          )}
          <div>
            <Button size="sm" variant="ghost" disabled={!!busy} onClick={() => run("codex/sign-in/cancel")}>
              {busy === "codex/sign-in/cancel" ? "Cancelling…" : "Cancel"}
            </Button>
          </div>
        </div>
      )}
      {!si.running && si.result && (
        <p role={si.failed ? "alert" : "status"} className={cn("break-words", si.failed ? "text-destructive" : "text-success")}>
          {si.result}
        </p>
      )}
      {si.log && (si.failed || (si.running && !si.code)) && (
        <pre className="max-h-48 overflow-auto rounded-md bg-muted p-2.5 font-mono text-xs whitespace-pre-wrap break-all">{si.log}</pre>
      )}
    </div>
  );
}

function NotifyStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const [url, setUrl] = useState("");
  const save = useAction();
  const test = useAction();
  return (
    <>
      <p className="text-sm text-text-2">
        Get a message on your phone when something needs you. Use an ntfy topic address (https://ntfy.sh/your-topic), or a Slack or Discord webhook address.
      </p>
      <SecretField id="wizard-notify" label="Notification address" saved={w.notify} value={url} set={setUrl} disabled={ro} placeholder="https://ntfy.sh/…" />
      <div className="flex flex-wrap gap-2">
        <Button
          size="sm"
          disabled={ro || !!save.busy || url.trim() === ""}
          onClick={async () => {
            if (await save.run("notify", { url })) setUrl("");
          }}
        >
          Save
        </Button>
        {w.notify.set && (
          <>
            <Button size="sm" variant="outline" disabled={ro || !!test.busy} onClick={() => test.run("notify/test")}>
              {test.busy ? "Sending…" : "Send a test"}
            </Button>
            <Button size="sm" variant="ghost" disabled={ro || !!save.busy} onClick={() => save.run("notify", { url: "" })}>
              Turn off
            </Button>
          </>
        )}
      </div>
      <Message msg={save.msg} />
      <Message msg={test.msg} />
    </>
  );
}

function SentryStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const [token, setToken] = useState("");
  const { busy, msg, run } = useAction();
  return (
    <>
      <p className="text-sm text-text-2">
        sources.sentry files an issue for each unresolved Sentry error. It reads Sentry with an auth token with the project:read and event:read scopes
        (Settings, then Developer Settings on your Sentry).
      </p>
      <SecretField id="wizard-sentry" label="Sentry auth token" saved={w.sentry!} value={token} set={setToken} disabled={ro} />
      <div className="flex flex-wrap gap-2">
        <Button
          size="sm"
          disabled={ro || !!busy || token.trim() === ""}
          onClick={async () => {
            if (await run("sentry", { token })) setToken("");
          }}
        >
          Save
        </Button>
        {w.sentry!.set && (
          <Button size="sm" variant="ghost" disabled={ro || !!busy} onClick={() => run("sentry", { token: "" })}>
            Remove
          </Button>
        )}
      </div>
      <Message msg={msg} />
    </>
  );
}

function ImagesStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const im = w.images!;
  const { busy, msg, run } = useAction();
  const missing = im.images.filter((i) => !i.built).length;
  return (
    <>
      <p className="text-sm text-text-2">Jobs run in Docker images built from deploy/job-image. Building takes a few minutes per image.</p>
      <ul className="flex flex-col gap-1.5 text-sm" aria-label="Job images">
        {im.images.map((i) => (
          <li key={i.tag} data-testid="job-image" className="flex min-w-0 items-start gap-2">
            <span className="mt-0.5">{i.built ? findingIcon.pass : findingIcon.fail}</span>
            <span className="min-w-0 flex-1 break-words">
              <span className="font-mono break-all">{i.tag}</span> for {i.for}
              {i.built ? "" : ", not built yet"}
            </span>
          </li>
        ))}
      </ul>
      <div>
        <Button size="sm" disabled={ro || im.building || !!busy || missing === 0} onClick={() => run("images/build")}>
          {im.building ? "Building…" : missing === 0 ? "All built" : "Build"}
        </Button>
      </div>
      <Message msg={msg} />
      {im.building && (
        <p role="status" className="flex items-center gap-2 text-sm text-primary-soft">
          <Loader2 className="size-4 animate-spin" aria-hidden /> Building. You can leave this page; the build keeps going.
        </p>
      )}
      {!im.building && im.result && (
        <p role={im.failed ? "alert" : "status"} className={cn("text-sm break-words", im.failed ? "text-destructive" : "text-success")}>
          {im.result}
        </p>
      )}
      {im.log && (
        <pre data-testid="build-log" className="max-h-64 overflow-auto rounded-md bg-muted p-2.5 font-mono text-xs whitespace-pre-wrap break-all">
          {im.log}
        </pre>
      )}
    </>
  );
}

/** Once Finish starts the factory, waits for the dashboard to answer, then opens it. */
/** How long Finish waits for the factory to come back before saying so. */
const restartWait = 120_000;

/**
 * Once Finish starts the factory, waits for the dashboard to answer, then
 * opens it. It stops when a new process answers in setup mode again (the
 * factory still can't start), or after restartWait, and says why.
 */
function useWaitForFactory(on: boolean, startedAt: RefObject<string>) {
  const [failed, setFailed] = useState("");
  useEffect(() => {
    if (!on) {
      return;
    }
    let stop = false;
    const since = Date.now();
    const poll = async () => {
      while (!stop) {
        await new Promise((r) => setTimeout(r, 1500));
        if (stop) return;
        try {
          await getJSON("/overview");
          window.location.assign("/app/");
          return;
        } catch {
          /* not up yet, or up in setup mode */
        }
        try {
          const w = await getJSON<Wizard>("/setup");
          if (w.mode === "setup" && w.started_at !== startedAt.current) {
            setFailed(`The factory restarted but still can't start: ${w.setup_reason || "see its log"}.`);
            return;
          }
        } catch {
          /* between processes */
        }
        if (Date.now() - since > restartWait) {
          setFailed("The factory didn't come back within 2 minutes. Check its log on the factory's machine, then reload this page.");
          return;
        }
      }
    };
    void poll();
    return () => {
      stop = true;
    };
  }, [on, startedAt]);
  return failed;
}

function FinishStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const { busy, msg, run } = useAction();
  const [started, setStarted] = useState(false);
  // The process that was asked to restart, fixed when Finish is pressed:
  // a refetch reaching the new one mustn't change what the wait looks for.
  const oldStart = useRef("");
  const failed = useWaitForFactory(started, oldStart);
  const setup = w.mode === "setup";
  const finish = async () => {
    oldStart.current = w.started_at;
    const r = await run("finish");
    if (r && setup) setStarted(true);
  };
  return (
    <>
      {!w.can_finish ? (
        <p className="text-sm text-text-2">Finish once the factory can reach every repo.</p>
      ) : w.warnings.length > 0 ? (
        <div className="flex flex-col gap-1.5 text-sm">
          <p>You can finish now, but some things won't work yet:</p>
          <ul className="list-disc pl-5 text-warning">
            {w.warnings.map((x) => (
              <li key={x}>{x}</li>
            ))}
          </ul>
        </div>
      ) : (
        <p className="text-sm text-text-2">Everything is in place.</p>
      )}
      {!setup && w.restart_pending ? (
        <p role="status" className="text-sm text-text-2">
          A restart is pending. It happens once running jobs finish{w.restart_waits_for ? ` and ${w.restart_waits_for} is done` : ""}.
        </p>
      ) : (
        <div>
          <Button disabled={ro || !w.can_finish || !!busy || started} onClick={finish}>
            {setup ? "Finish and start the factory" : w.changed ? "Finish and restart" : "Finish"}
          </Button>
        </div>
      )}
      {!setup && w.changed && !w.restart_pending && (
        <p className="text-xs text-muted-foreground">Saved changes apply when the factory restarts, once running jobs finish.</p>
      )}
      <Message msg={msg} />
      {started && !failed && (
        <p role="status" className="flex items-center gap-2 text-sm text-primary-soft">
          <Loader2 className="size-4 animate-spin" aria-hidden /> Waiting for the factory to start…
        </p>
      )}
      {failed && (
        <p role="alert" className="text-sm break-words text-destructive">
          {failed}
        </p>
      )}
      {!setup && !started && (
        <p className="text-sm text-text-2">
          Then add the label to an issue to hand it to the factory. <Link to="/setup" className="text-primary hover:underline">See how ready each repo is</Link>.
        </p>
      )}
    </>
  );
}

const bodies: Record<string, (p: { w: Wizard; ro: boolean }) => ReactNode> = {
  machine: (p) => <MachineStep {...p} />,
  repos: (p) => <ReposStep {...p} />,
  github: (p) => <GitHubStep {...p} />,
  gitlab: (p) => <GitLabStep {...p} />,
  checks: (p) => <ChecksStep {...p} />,
  claude: (p) => <ClaudeStep {...p} />,
  codex: (p) => <CodexStep {...p} />,
  notify: (p) => <NotifyStep {...p} />,
  sentry: (p) => <SentryStep {...p} />,
  images: (p) => <ImagesStep {...p} />,
  finish: (p) => <FinishStep {...p} />,
};

export function GetStartedScreen() {
  const { data, error, isLoading } = useWizard();
  const [params, setParams] = useSearchParams();
  // GitHub's answer shows once: kept here, and taken out of the address so
  // a reload or a shared link doesn't show it again.
  const [back] = useState(params.get("github") ?? "");
  const [backErr] = useState(params.get("github_error") ?? "");
  useEffect(() => {
    if (params.has("github") || params.has("github_error")) setParams({}, { replace: true });
  }, [params, setParams]);
  // null follows the first step that needs attention; a key is one the person opened.
  const [open, setOpen] = useState<string | null>(back || backErr ? "github" : null);
  if (isLoading) return <Loading what="Reading the setup" />;
  if (error) return <ErrorBox error={error} />;
  if (!data) return null;
  const w = data;
  const ro = !w.writable;
  const first = w.steps.find((s) => s.state === "attention")?.key ?? "finish";
  const current = open ?? first;
  return (
    <div className="mx-auto flex max-w-3xl min-w-0 flex-col gap-4" data-testid="get-started">
      <div>
        <PageTitle>Get started</PageTitle>
        <p className="-mt-2 text-sm text-text-2">
          {w.mode === "setup"
            ? "The factory needs a few things before it can start. Each step is saved as you go, to factory.yaml and factory.env on the factory's machine."
            : "Change what setup saved. Each step is saved as you go; the factory uses the changes once it restarts."}{" "}
          The terminal version is <Code>./factory setup</Code>.
        </p>
      </div>
      {ro && (
        <div role="note" className="rounded-md border border-warning-border bg-warning-bg p-3 text-sm text-warning">
          {w.reason}
        </div>
      )}
      {back && (
        <p role="status" className="rounded-md border border-success/40 bg-success/10 p-3 text-sm break-words text-success">
          {back}
        </p>
      )}
      {backErr && (
        <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm break-words text-destructive">
          GitHub didn't finish making the app: {backErr}
        </p>
      )}
      <ol className="flex flex-col gap-3" aria-label="Setup steps">
        {w.steps.map((s, i) => (
          <StepCard key={s.key} step={s} n={i + 1} open={current === s.key} toggle={() => setOpen(current === s.key ? "" : s.key)}>
            {bodies[s.key]?.({ w, ro })}
          </StepCard>
        ))}
      </ol>
    </div>
  );
}
