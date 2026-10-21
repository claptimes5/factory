import { useEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { Link, useSearchParams } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import {
  AlertCircle,
  AlertTriangle,
  ArrowRight,
  Check,
  CheckCircle2,
  ChevronDown,
  Circle,
  Copy,
  ExternalLink,
  Loader2,
  PartyPopper,
  Plus,
  Trash2,
  XCircle,
} from "lucide-react";
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
import { Ago, ErrorBox, Loading, PageTitle } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { SegmentedControl } from "@/components/ui/segmented-control";
import { cn } from "@/lib/utils";

// The get-started page: what `factory setup` asks in the terminal, as a
// list of steps. Each step saves as it goes, to factory.yaml and
// factory.env on the factory's machine. Saved secrets never come back
// here: only whether each is set, and a masked hint.

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

/** A command to run on the factory's machine, with a button that copies it. */
function CopyCommand({ cmd }: { cmd: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(cmd);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* no clipboard, as on a plain http address: the command is there to select */
    }
  };
  return (
    <span className="inline-flex max-w-full min-w-0 items-center gap-1 rounded-md border bg-muted py-0.5 pr-0.5 pl-2 align-middle">
      <code className="min-w-0 font-mono text-xs break-all">{cmd}</code>
      <button
        type="button"
        onClick={copy}
        aria-label={copied ? "Copied" : `Copy ${cmd}`}
        className="inline-flex size-6 shrink-0 cursor-pointer items-center justify-center rounded text-muted-foreground hover:bg-accent hover:text-foreground"
      >
        {copied ? <Check className="size-3.5 text-success" aria-hidden /> : <Copy className="size-3.5" aria-hidden />}
      </button>
    </span>
  );
}

/** Which host a pasted token is for, from how it starts; "" when it doesn't say. */
function tokenHost(t: string): "github" | "gitlab" | "" {
  const v = t.trim();
  if (v.startsWith("glpat-")) return "gitlab";
  if (/^(ghp_|github_pat_|gho_|ghs_|ghu_)/.test(v)) return "github";
  return "";
}

/** "a", "a and b", "a, b and c". */
function joinAnd(s: string[]) {
  return s.length <= 1 ? (s[0] ?? "") : `${s.slice(0, -1).join(", ")} and ${s[s.length - 1]}`;
}

/** A GitLab repo's name starts with its server (gitlab.com/group/project); a GitHub one is owner/name. */
const onGitLab = (repo: string) => repo.split("/")[0].includes(".");

/** How the factory is connected to a host, with a button that removes it. */
function Connection({ children, remove, onRemove, ro }: { children: ReactNode; remove: string; onRemove: () => void; ro: boolean }) {
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-control border bg-muted/40 px-3 py-2 text-sm">
      <CheckCircle2 aria-hidden className="size-4 shrink-0 text-success" />
      <span className="min-w-0 flex-1 break-words">{children}</span>
      <Button size="sm" variant="ghost" disabled={ro} onClick={onRemove}>
        {remove}
      </Button>
    </div>
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
  hint,
}: {
  id: string;
  label: string;
  saved?: WizardSecret;
  value: string;
  set: (v: string) => void;
  disabled?: boolean;
  placeholder?: string;
  /** A warning about what was typed, such as another host's token. */
  hint?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <label htmlFor={id} className="text-body-sm font-medium">
        {label}
      </label>
      <Input
        id={id}
        type="password"
        autoComplete="off"
        spellCheck={false}
        value={value}
        disabled={disabled}
        placeholder={saved?.set ? `Saved: ${saved.masked}. Paste a new one to replace it.` : placeholder}
        aria-describedby={hint ? `${id}-hint` : undefined}
        onChange={(e) => set(e.target.value)}
      />
      {hint ? (
        <span id={`${id}-hint`} className="text-xs text-warning">
          {hint}
        </span>
      ) : (
        <span className="text-xs text-muted-foreground">Saved to factory.env on the factory's machine. This page never shows it again.</span>
      )}
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
      <label htmlFor={id} className="text-body-sm font-medium">
        {label}
      </label>
      <Input id={id} value={value} disabled={disabled} placeholder={placeholder} spellCheck={false} onChange={(e) => set(e.target.value)} />
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

function StepCard({
  step,
  n,
  open,
  toggle,
  next,
  children,
}: {
  step: WizardStep;
  n: number;
  open: boolean;
  toggle: () => void;
  /** The next step that needs something, offered once this one is done. */
  next?: { n: number; step: WizardStep; go: () => void };
  children: ReactNode;
}) {
  const [label, variant] = stateText[step.state] ?? [step.state, "secondary"];
  return (
    <li
      id={`card-${step.key}`}
      data-testid="wizard-step"
      data-key={step.key}
      data-state={step.state}
      className={cn("scroll-mt-4 rounded-card border bg-card transition-colors", open && step.state === "attention" && "border-primary/40")}
    >
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
              <span className="text-body font-semibold">
                {n}. {step.title}
              </span>
              <Badge variant={variant}>{label}</Badge>
            </span>
            <span className="text-meta break-words text-text-2">{step.summary}</span>
          </span>
          <ChevronDown aria-hidden className={cn("mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-180")} />
        </button>
      </h2>
      {open && (
        <div id={`step-${step.key}`} className="flex min-w-0 flex-col gap-3 border-t px-4 pt-3 pb-4">
          {children}
          {step.state !== "attention" && next && (
            <div className="flex justify-end border-t pt-3">
              <Button size="sm" variant="outline" onClick={next.go}>
                Next: {next.n}. {next.step.title} <ArrowRight aria-hidden />
              </Button>
            </div>
          )}
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

function MachineFinding({ f, w, ro, busy, run }: { f: Wizard["machine"][number]; w: Wizard; ro: boolean; busy: string; run: (a: string) => void }) {
  return (
    <li data-testid="machine-finding" className="flex min-w-0 gap-2 text-sm">
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
  );
}

function MachineStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const { busy, msg, run } = useAction();
  const todo = w.machine.filter((f) => f.status !== "pass");
  const fine = w.machine.filter((f) => f.status === "pass");
  const fails = todo.filter((f) => f.status === "fail").length;
  return (
    <>
      <p className="text-sm text-text-2">
        The factory runs its jobs on this machine, so this checks what's installed here.{" "}
        {fails > 0 ? "Fix the red lines, then check again." : todo.length > 0 ? "The yellow lines are optional." : ""}
      </p>
      {w.machine_checked_at === "" && machineCheckStalled() ? (
        <p role="alert" className="text-sm text-destructive">The machine check didn't finish. Docker may be hung; check again.</p>
      ) : w.machine_checked_at === "" ? (
        <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" aria-hidden /> Checking…
        </p>
      ) : (
        <div className="flex flex-col gap-2" aria-label="Checks of this machine" role="group">
          {todo.length > 0 && (
            <ul className="flex flex-col gap-2">
              {todo.map((f) => (
                <MachineFinding key={f.what} f={f} w={w} ro={ro} busy={busy} run={run} />
              ))}
            </ul>
          )}
          {fine.length > 0 && (
            <details open={todo.length === 0 || undefined}>
              <summary className="flex cursor-pointer items-center gap-2 text-sm">
                {findingIcon.pass} {fine.length === 1 ? "1 check passed" : `${fine.length} checks passed`}
              </summary>
              <ul className="mt-2 flex flex-col gap-2 pl-6">
                {fine.map((f) => (
                  <MachineFinding key={f.what} f={f} w={w} ro={ro} busy={busy} run={run} />
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
      <div>
        <Button size="sm" variant="outline" disabled={ro || !!busy} onClick={() => run("machine/check")}>
          {busy === "machine/check" ? "Checking…" : "Check again"}
        </Button>
      </div>
      <Message msg={msg} />
    </>
  );
}

function ReposStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const { busy, msg, run } = useAction();
  const [host, setHost] = useState<"github" | "gitlab">(w.repos.length > 0 && w.repos.every(onGitLab) ? "gitlab" : "github");
  const [name, setName] = useState("");
  const [server, setServer] = useState(() => w.repos.find(onGitLab)?.split("/")[0] ?? "gitlab.com");
  const save = (repos: string[]) => run("repos", { repos: repos.join(", ") });
  const add = async () => {
    let v = name.trim();
    if (!v) return;
    // A bare group/project is on the GitLab server named beside it; a link or clone address says its own.
    if (host === "gitlab" && !/[:@]/.test(v) && !onGitLab(v)) v = `${server.trim().replace(/^https?:\/\//, "").replace(/\/+$/, "") || "gitlab.com"}/${v}`;
    if (await save([...w.repos, v])) setName("");
  };
  const hosts = [
    { key: "github", label: "GitHub" },
    { key: "gitlab", label: "GitLab" },
  ] as const;
  return (
    <>
      <p className="text-sm text-text-2">
        Which repositories should the factory work on? It clones each one from GitHub or GitLab into a workspace of its own, makes its changes on a
        branch, and opens a pull request for you to review. It never uses or changes a folder on this computer, including the one you started it from.
      </p>
      {w.repos.length > 0 && (
        <ul aria-label="Chosen repositories" className="flex flex-col divide-y rounded-control border">
          {w.repos.map((r) => (
            <li key={r} data-testid="repo" className="flex min-w-0 items-center gap-2.5 py-1.5 pr-1.5 pl-3">
              <Badge variant="secondary" className="w-14 justify-center">
                {onGitLab(r) ? "GitLab" : "GitHub"}
              </Badge>
              <span className="min-w-0 flex-1 font-mono text-sm break-all">{r}</span>
              <Button
                size="icon"
                variant="ghost"
                className="size-8"
                aria-label={`Remove ${r}`}
                title={w.repos.length === 1 ? "Add another repository first: the factory needs at least one" : `Remove ${r}`}
                disabled={ro || !!busy || w.repos.length === 1}
                onClick={() => save(w.repos.filter((x) => x !== r))}
              >
                <Trash2 aria-hidden />
              </Button>
            </li>
          ))}
        </ul>
      )}
      <form
        className="flex flex-col gap-3 rounded-control border p-3"
        aria-label="Add a repository"
        onSubmit={(e) => {
          e.preventDefault();
          void add();
        }}
      >
        <div className="flex flex-col gap-2">
          <span className="text-body-sm font-medium">{w.repos.length === 0 ? "Add your first repository. Where is it hosted?" : "Add another. Where is it hosted?"}</span>
          <SegmentedControl
            label="Where is it hosted?"
            value={host}
            onValueChange={(v) => setHost(v as typeof host)}
            options={hosts.map((h) => ({ value: h.key, label: h.label }))}
            disabled={ro}
          />
        </div>
        <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-end">
          {host === "gitlab" && (
            <div className="sm:w-44">
              <TextField id="wizard-gitlab-server" label="Server" value={server} set={setServer} disabled={ro} placeholder="gitlab.com" />
            </div>
          )}
          <div className="min-w-0 flex-1">
            <TextField
              id="wizard-repos"
              label={host === "github" ? "Repository" : "Project"}
              value={name}
              set={setName}
              disabled={ro}
              placeholder={host === "github" ? "octocat/hello-world" : "group/project"}
            />
          </div>
          <Button type="submit" disabled={ro || !!busy || name.trim() === ""}>
            {busy ? <Loader2 className="animate-spin" aria-hidden /> : <Plus aria-hidden />} Add
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          {host === "github"
            ? "owner/name, as in github.com/owner/name. Or paste the repo's link or clone address."
            : "The project's path, subgroups included. Or paste its link or clone address. For a self-hosted GitLab, change the server."}
        </p>
      </form>
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
  const off = useAction();
  const installed = g.method === "app" && g.installation_id !== "" && g.missing.length === 0;
  const repos = w.access.filter((a) => a.host === "github").map((a) => a.repo);
  const disconnect = () => {
    if (g.method === "app" && !window.confirm(`Disconnect the app ${g.app_slug || g.app_id}? It stays on GitHub, and its key file stays on this machine, so you can use it again under Other ways to connect.`)) return;
    void off.run("github/disconnect");
  };
  return (
    <>
      <p className="text-sm text-text-2">
        The factory needs to reach {joinAnd(repos)} on GitHub: to read the code, push its branches, open pull requests and read the checks they run.
      </p>
      {g.method === "app" ? (
        <Connection remove="Disconnect" onRemove={disconnect} ro={ro || !!off.busy}>
          Connected through the GitHub App <span className="font-mono break-all">{g.app_slug || g.app_id}</span>
          {g.key_path && (
            <>
              , with the key <span className="font-mono break-all">{g.key_path}</span>
            </>
          )}
          .
        </Connection>
      ) : g.method === "token" ? (
        <Connection remove="Remove token" onRemove={() => off.run("github/disconnect")} ro={ro || !!off.busy}>
          Connected with a personal access token <Code>{g.token.masked}</Code>. Fine for a try; an app is safer for everyday use.
        </Connection>
      ) : null}
      <Message msg={off.msg} />
      {g.method !== "app" && (
        <section aria-label="Create a GitHub App" className="flex flex-col gap-2 rounded-control border border-primary/40 bg-primary/5 p-3">
          <h3 className="flex flex-wrap items-center gap-2 text-sm font-semibold">
            Create a GitHub App <Badge>Recommended</Badge>
          </h3>
          <p className="text-sm text-text-2">
            It takes about a minute, and the factory gets only the repos you install it on, nothing else. GitHub shows a form already filled in for{" "}
            {g.app_owner}: click "Create GitHub App" at the bottom, and GitHub brings you back here to install it.
          </p>
          <div>
            <Button disabled={ro || starting} onClick={create}>
              {starting ? "Opening GitHub…" : "Create an app for me"}
            </Button>
          </div>
          <Message msg={msg} />
        </section>
      )}
      {g.method === "app" && !installed && (
        <section aria-label="Install the app" className="flex flex-col gap-2 rounded-control border border-warning-border bg-warning-bg p-3 text-sm">
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
  const wrongHost = tokenHost(token) === "gitlab";
  return (
    <details className="rounded-control border p-3" open={g.method === "token" || undefined}>
      <summary className="cursor-pointer text-sm font-medium">Other ways to connect: an app you already made, or a personal access token</summary>
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
          <h3 className="text-sm font-semibold">Use a GitHub personal access token (quickest, for a try)</h3>
          <p className="text-sm text-text-2">
            Make a fine-grained token at <A href={g.token_url}>{g.token_url}</A>. Under Repository access, choose Only select repositories and pick yours;
            under Permissions, set Contents, Issues and Pull requests to Read and write.
          </p>
          <SecretField
            id="wizard-github-token"
            label="GitHub token"
            saved={g.token}
            value={token}
            set={setToken}
            disabled={ro}
            placeholder="github_pat_…"
            hint={wrongHost && "That's a GitLab token. Add your GitLab project under Repositories, and its GitLab step takes this token."}
          />
          <div>
            <Button
              size="sm"
              variant="outline"
              disabled={ro || !!tok.busy || token.trim() === "" || wrongHost}
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
  const repos = w.access.filter((a) => a.host === "gitlab").map((a) => a.repo);
  const wrongHost = tokenHost(token) === "github";
  return (
    <>
      <p className="text-sm text-text-2">
        The factory reaches {joinAnd(repos)} with a GitLab access token. Whoever owns the token (you, or a bot account) needs the Developer role on each
        project, or Maintainer if the base branch is protected from Developers.
      </p>
      {g.token.set && (
        <Connection remove="Remove token" onRemove={() => run("gitlab/disconnect")} ro={ro || !!busy}>
          Connected with the token <Code>{g.token.masked}</Code>.
        </Connection>
      )}
      <div className="flex flex-col gap-2">
        <span id="gitlab-kind" className="text-body-sm font-medium">
          Which kind of token will you make?
        </span>
        <RadioGroup aria-labelledby="gitlab-kind" value={kind} onValueChange={(v) => setKind(v as typeof kind)}>
          <label className="flex items-start gap-2.5 text-body-sm">
            <RadioGroupItem value="legacy" className="mt-0.5" />
            <span>Legacy, the quick one: one link opens the form with its scopes already ticked.</span>
          </label>
          <label className="flex items-start gap-2.5 text-body-sm">
            <RadioGroupItem value="fine" className="mt-0.5" />
            <span>Fine-grained: limited to these projects, but about 20 boxes to tick by hand.</span>
          </label>
        </RadioGroup>
      </div>
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
      <SecretField
        id="wizard-gitlab-token"
        label="GitLab token"
        saved={g.token}
        value={token}
        set={setToken}
        disabled={ro}
        placeholder="glpat-…"
        hint={wrongHost && "That's a GitHub token. It goes in the GitHub step, which shows once a GitHub repo is under Repositories."}
      />
      <div>
        <Button
          size="sm"
          disabled={ro || !!busy || token.trim() === "" || wrongHost}
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
        These are the commands every change must pass before you're asked to review it: an install, then types, lint, test and build. Leave one empty to
        skip it. The factory suggested them from each repo's files, and from its AGENTS.md or CLAUDE.md where that names one.
      </p>
      <p className="text-sm text-text-2">
        Try them before you save: each must pass on the base branch as it is today, or every change would fail. And check the test command really runs
        the tests: a wrong one can pass without running any.
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
  // Saved services stay on; suggested ones start on while none are saved.
  const [svcs, setSvcs] = useState<string[]>(() => {
    const saved = c.services.filter((s) => s.saved);
    return (saved.length > 0 ? saved : c.services).map((s) => s.name);
  });
  const [env, setEnv] = useState<Record<string, string>>(() => ({ ...c.env }));
  const body = { repo: c.repo, install, image_from: image, commands: cmds, services: svcs, env };
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
            <Input id={id(g)} className="font-mono" value={cmds[g] ?? ""} disabled={ro} placeholder="none" spellCheck={false}
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
      {c.services.length > 0 && (
        <fieldset className="flex flex-col gap-1.5">
          <legend className="text-sm font-medium">Services the checks start</legend>
          {c.services.map((s) => (
            <label key={s.name} className="flex items-center gap-2 text-sm">
              <Checkbox
                disabled={ro}
                checked={svcs.includes(s.name)}
                onCheckedChange={(on) => setSvcs(on === true ? [...svcs, s.name] : svcs.filter((n) => n !== s.name))}
              />
              <span className="font-mono">{s.name}</span>
              <span className="text-xs text-muted-foreground">
                {s.image}
                {s.saved ? "" : ", suggested: its tests look like they need it"}
              </span>
            </label>
          ))}
        </fieldset>
      )}
      {Object.keys(env).length > 0 && (
        <div className="flex flex-col gap-1.5">
          <span className="text-sm font-medium">Environment</span>
          {Object.keys(env)
            .sort()
            .map((k) => (
              <label key={k} className="flex min-w-0 flex-col gap-1 text-xs sm:flex-row sm:items-center sm:gap-2">
                <span className="w-32 shrink-0 font-mono">{k}</span>
                <Input className="font-mono" value={env[k]} disabled={ro} spellCheck={false} onChange={(e) => setEnv({ ...env, [k]: e.target.value })} />
              </label>
            ))}
          <span className="text-xs text-muted-foreground">Each service is reached by its name as the host, such as postgres.</span>
        </div>
      )}
      <TextField id={id("image")} label="Job image built on" value={image} set={setImage} disabled={ro} placeholder="the factory's default image" help="A Docker image with the repo's language, such as ruby:3.4-bookworm." />
      {c.notes.map((n) => (
        <p key={n} className="text-xs text-muted-foreground">
          {n}
        </p>
      ))}
      <div className="flex flex-wrap gap-2">
        <Button size="sm" variant="outline" disabled={ro || !!busy || !!c.try?.running} onClick={() => run("checks/try", body)}>
          {busy === "checks/try" ? "Starting…" : "Try these checks"}
        </Button>
        <Button size="sm" disabled={ro || !!busy} onClick={() => run("checks/save", body)}>
          {busy === "checks/save" ? "Saving…" : c.saved ? "Save" : "Use these checks"}
        </Button>
      </div>
      <Message msg={msg} />
      {c.try && <TryResult t={c.try} />}
    </div>
  );
}

const tryIcon: Record<string, ReactNode> = {
  pass: <CheckCircle2 className="size-4 text-success" aria-label="passed" />,
  fail: <XCircle className="size-4 text-destructive" aria-label="failed" />,
  running: <Loader2 className="size-4 animate-spin text-primary-soft" aria-label="running" />,
  waiting: <Circle className="size-4 text-muted-foreground" aria-label="waiting" />,
  skipped: <Circle className="size-4 text-muted-foreground" aria-label="skipped" />,
};

/** The last try of a repo's checks: each command, how it went, and what a failing one printed. */
function TryResult({ t }: { t: NonNullable<WizardChecks["try"]> }) {
  const failed = t.steps.filter((s) => s.state === "fail").length;
  return (
    <div className="flex flex-col gap-2 rounded-md border p-3 text-sm" data-testid="try-result">
      <p className="font-medium">
        {t.running ? (
          <>
            Trying them, started <Ago ts={t.since} />…
          </>
        ) : t.error ? (
          "The try couldn't run them:"
        ) : failed > 0 ? (
          `${failed} of ${t.steps.length} failed. Fix the command, a service or the environment above, then try again.`
        ) : (
          "Every one passed on the base branch. Save them."
        )}
      </p>
      {t.error && <p className="text-destructive">{t.error}</p>}
      <ul className="flex flex-col gap-1.5">
        {t.steps.map((s) => (
          <li key={s.gate} className="flex flex-col gap-1">
            <span className="flex flex-wrap items-center gap-2">
              {tryIcon[s.state]}
              <span className="font-medium">{s.gate}</span>
              <Code>{s.command}</Code>
              {s.seconds > 0 && <span className="text-xs text-muted-foreground">{s.seconds}s</span>}
              {s.state === "skipped" && <span className="text-xs text-muted-foreground">not run</span>}
            </span>
            {s.output && (
              <details open={s.state === "fail"}>
                <summary className="cursor-pointer text-xs text-muted-foreground">Output</summary>
                <pre className="mt-1 max-h-72 overflow-auto rounded-md bg-muted p-2 font-mono text-xs whitespace-pre-wrap">{s.output}</pre>
              </details>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * What a harness does, or would: Claude and Codex are equals, either one is
 * enough, and with both one builds and the other reviews what it builds.
 */
function RoleLine({ name, other, role }: { name: string; other: string; role: string }) {
  const text =
    role === "builds and reviews"
      ? `${name} builds and reviews. Connect ${other} too, and it reviews what ${name} builds: another vendor's model catches more.`
      : role === "builds"
        ? `${name} builds, and ${other} reviews what it builds.`
        : role === "reviews"
          ? `${name} reviews what ${other} builds.`
          : `The factory runs on Claude, Codex or both: either one is enough. With both, one builds and the other reviews what it builds, which catches more.`;
  return (
    <p className="text-sm text-text-2">
      {text}
      {(role === "builds" || role === "reviews") && " Settings can swap them."}
    </p>
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
      <RoleLine name="Claude" other="Codex" role={c.role} />
      <p className="text-sm text-text-2">Claude can use either of these:</p>
      <ul className="flex flex-col gap-2 text-sm">
        <li className="rounded-control border p-3">
          <span className="font-medium">A Claude subscription</span> (Pro or Max), using the plan's limits. Run <CopyCommand cmd="claude setup-token" /> in a
          terminal and paste the token it prints.
        </li>
        <li className="rounded-control border p-3">
          <span className="font-medium">An API key</span>, billed per use. Make one at{" "}
          <A href="https://console.anthropic.com/settings/keys">console.anthropic.com</A>.
        </li>
      </ul>
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
      <RoleLine name="Codex" other="Claude" role={c.role} />
      <p className="text-sm text-text-2">Codex can sign in with:</p>
      <RadioGroup aria-label="How Codex signs in" value={mode} onValueChange={setMode}>
        {codexModes.map((m) => (
          <label key={m.mode} className="flex items-start gap-2.5 text-body-sm">
            <RadioGroupItem value={m.mode} className="mt-0.5" />
            <span>{m.label}</span>
          </label>
        ))}
      </RadioGroup>
      {mode === "api-key" && (
        <>
          <p className="text-sm text-text-2">
            Paste an API key from <A href="https://platform.openai.com/api-keys">platform.openai.com</A>.
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

/** A topic on ntfy.sh nobody would guess: anyone who knows a topic's name can read it. */
function newTopic() {
  const b = new Uint8Array(8);
  crypto.getRandomValues(b);
  return "https://ntfy.sh/factory-" + Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

function NotifyStep({ w, ro }: { w: Wizard; ro: boolean }) {
  const [url, setUrl] = useState("");
  const save = useAction();
  const test = useAction();
  const topic = url.startsWith("https://ntfy.sh/factory-") ? url : "";
  return (
    <>
      <p className="text-sm text-text-2">
        Get a message on your phone when the factory needs you: a question about an issue, a pull request ready for review, a budget running out. Use an
        ntfy topic, or a Slack or Discord webhook address.
      </p>
      {!w.notify.set && (
        <div className="flex flex-col gap-2 rounded-control border p-3 text-sm">
          <p>
            <span className="font-medium">Quickest: ntfy</span>, free and with no account. Make a private topic, install the ntfy app (
            <A href="https://ntfy.sh/docs/subscribe/phone/">iPhone or Android</A>) and subscribe to it there.
          </p>
          <div>
            <Button size="sm" variant="outline" disabled={ro} onClick={() => setUrl(newTopic())}>
              Make a private topic
            </Button>
          </div>
          {topic && (
            <p className="text-text-2">
              In the app, subscribe to <CopyCommand cmd={topic.replace("https://ntfy.sh/", "")} />, then save and send a test.
            </p>
          )}
        </div>
      )}
      <SecretField id="wizard-notify" label="Notification address" saved={w.notify} value={url} set={setUrl} disabled={ro} placeholder="https://ntfy.sh/… or a webhook address" />
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
      <p className="text-sm text-text-2">
        Each job runs in a Docker image with the tools it needs, built on this machine from deploy/job-image. The first build takes a few minutes per
        image; it runs in the background, so carry on with the other steps meanwhile.
      </p>
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
  const label = w.label_names[0] ?? "factory:ready";
  return (
    <>
      {w.repos.length === 0 ? (
        <p className="text-sm text-text-2">Finish once you've chosen your repositories and the factory can reach them.</p>
      ) : !w.can_finish ? (
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
        <p className="flex items-center gap-2 text-sm font-medium text-success">
          <PartyPopper className="size-4" aria-hidden /> Everything is in place.
        </p>
      )}
      {w.can_finish && (
        <section aria-label="What happens next" className="flex flex-col gap-2 rounded-control border bg-muted/40 p-3 text-sm">
          <h3 className="font-semibold">What happens next</h3>
          <ol className="flex list-decimal flex-col gap-1.5 pl-5 text-text-2">
            <li>
              On the Setup screen, check each repo: the factory clones it, installs it and runs its checks twice, to be sure they give the same answer
              every time. It says what's in the way, and can fix most of it itself.
            </li>
            <li>
              Add the <Code>{label}</Code> label to an issue. The factory picks it up within a minute or two, writes the change and runs the checks on it.
            </li>
            <li>Once a reviewer model has checked the change, you get a pull request to review and merge. The Floor shows each job as it goes.</li>
          </ol>
          <p className="text-xs text-muted-foreground">Start with a small, clear issue: a bug with steps to reproduce, or a change to copy or config.</p>
        </section>
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
      {!setup && !started && w.can_finish && (
        <p className="text-sm text-text-2">
          <Link to="/setup" className="text-primary hover:underline">
            See how ready each repo is
          </Link>
          .
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
  // The first step that needs something is open; toggled are the ones the person opened or closed.
  const [toggled, setToggled] = useState<Record<string, boolean>>(back || backErr ? { github: true } : {});
  const first = data ? (data.steps.find((s) => s.state === "attention")?.key ?? "finish") : "";
  // A step finished while open stays open, so what it did stays in view, and the next one opens below it.
  const prevFirst = useRef("");
  useEffect(() => {
    if (!first) return;
    const p = prevFirst.current;
    if (p && p !== first) setToggled((t) => (p in t ? t : { ...t, [p]: true }));
    prevFirst.current = first;
  }, [first]);
  if (isLoading) return <Loading what="Reading the setup" />;
  if (error) return <ErrorBox error={error} />;
  if (!data) return null;
  const w = data;
  const ro = !w.writable;
  const isOpen = (key: string) => toggled[key] ?? key === first;
  const goTo = (key: string) => {
    setToggled((t) => ({ ...t, [key]: true }));
    requestAnimationFrame(() => document.getElementById(`card-${key}`)?.scrollIntoView({ behavior: "smooth", block: "start" }));
  };
  const required = w.steps.filter((s) => s.state !== "optional" && s.key !== "finish");
  const doneCount = required.filter((s) => s.state === "done").length;
  return (
    <div className="mx-auto flex max-w-3xl min-w-0 flex-col gap-4" data-testid="get-started">
      <div>
        <PageTitle>Get started</PageTitle>
        <div className="-mt-2 flex flex-col gap-2 text-sm text-text-2">
          {w.mode === "setup" ? (
            <p>
              The factory needs a few things before it can start. Give it your repos and a way to reach them, and it turns the issues you label into pull
              requests, each one checked and reviewed before it reaches you.
            </p>
          ) : (
            <p>Change what setup saved. The factory uses the changes once it restarts.</p>
          )}
          <p>
            Each step saves as you go, so you can stop and come back. Optional steps can wait. Prefer a terminal? Run <CopyCommand cmd="./factory setup" />.
          </p>
        </div>
      </div>
      <div className="flex flex-col gap-1.5" data-testid="wizard-progress">
        <div className="flex items-baseline justify-between gap-2 text-sm">
          <span className="font-medium">
            {doneCount === required.length ? "Every required step is done" : `${doneCount} of ${required.length} required steps done`}
          </span>
          <details className="text-xs text-muted-foreground">
            <summary className="cursor-pointer">Where it's saved</summary>
            <p className="mt-1 break-all">
              Settings: <span className="font-mono">{w.config_path}</span>
              <br />
              Keys and tokens: <span className="font-mono">{w.env_path}</span>
            </p>
          </details>
        </div>
        <div
          className="h-1.5 rounded-full bg-border"
          role="progressbar"
          aria-label="Setup progress"
          aria-valuenow={doneCount}
          aria-valuemin={0}
          aria-valuemax={required.length}
        >
          <div
            className={cn("h-1.5 rounded-full transition-all", doneCount === required.length ? "bg-success-fill" : "bg-primary")}
            style={{ width: `${required.length ? (doneCount / required.length) * 100 : 0}%` }}
          />
        </div>
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
        {w.steps.map((s, i) => {
          const j = w.steps.findIndex((x, k) => k > i && x.state === "attention");
          return (
            <StepCard
              key={s.key}
              step={s}
              n={i + 1}
              open={isOpen(s.key)}
              toggle={() => setToggled((t) => ({ ...t, [s.key]: !isOpen(s.key) }))}
              next={j >= 0 ? { n: j + 1, step: w.steps[j], go: () => goTo(w.steps[j].key) } : undefined}
            >
              {bodies[s.key]?.({ w, ro })}
            </StepCard>
          );
        })}
      </ol>
    </div>
  );
}
