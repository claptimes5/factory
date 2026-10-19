import { useEffect, useState } from "react";
import { Link, NavLink, Outlet } from "react-router";
import { Activity, Bell, LayoutGrid, LogOut, Moon, Radio, Search, SlidersHorizontal, Sun } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { act, useLiveUpdates, useNotifications, useOverview, type Overview } from "@/api/client";
import { Button } from "@/components/ui/button";
import { CommandPalette } from "@/components/CommandPalette";
import { PlanUsageMeter } from "@/components/PlanUsage";
import { SetupBanner } from "@/screens/Setup";
import { useBrowserNotifications } from "@/lib/notify";
import { setRepo, useRepo } from "@/lib/repo";
import { cn, money, repoName } from "@/lib/utils";

function useTheme() {
  const [dark, setDark] = useState(() => {
    try {
      const saved = localStorage.getItem("theme");
      if (saved) return saved === "dark";
    } catch {
      /* storage may be unavailable */
    }
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  });
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    try {
      localStorage.setItem("theme", dark ? "dark" : "light");
    } catch {
      /* ignore */
    }
  }, [dark]);
  return [dark, setDark] as const;
}

/** ⌘K or Ctrl+K anywhere opens the palette. */
function usePalette() {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  return [open, setOpen] as const;
}

export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 28 28" aria-hidden className={className}>
      <rect x="2" y="10" width="7" height="16" rx="1.5" className="fill-warning-fill" />
      <rect x="10.5" y="5" width="7" height="21" rx="1.5" className="fill-primary" />
      <rect x="19" y="14" width="7" height="12" rx="1.5" className="fill-success-fill" />
    </svg>
  );
}

const links = [
  { to: "/", label: "Floor", end: true },
  { to: "/orchestrator", label: "Orchestrator" },
  { to: "/metrics", label: "Spend" },
  { to: "/settings", label: "Settings" },
  { to: "/setup", label: "Setup" },
];

const tabs = [
  { to: "/", label: "Floor", icon: LayoutGrid, end: true },
  { to: "/notifications", label: "Inbox", icon: Bell },
  { to: "/live", label: "Live", icon: Radio },
  { to: "/metrics", label: "Spend", icon: Activity },
  { to: "/settings", label: "Settings", icon: SlidersHorizontal },
];

export function Layout() {
  useLiveUpdates();
  const { data: o } = useOverview();
  const { data: n } = useNotifications();
  useBrowserNotifications(n?.notifications);
  const [dark, setDark] = useTheme();
  const [palette, setPalette] = usePalette();
  const inbox = n?.notifications.length ?? 0;
  const repos = o?.repos ?? [];
  const repo = useRepo(repos);
  const theme = (
    <button
      aria-label={dark ? "Switch to light mode" : "Switch to dark mode"}
      className="flex size-11 shrink-0 cursor-pointer items-center justify-center rounded-lg border bg-card text-foreground hover:bg-accent md:size-9"
      onClick={() => setDark(!dark)}
    >
      {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
    </button>
  );
  return (
    <div className="flex min-h-full flex-col">
      <header className="sticky top-0 z-20 border-b bg-surface/95 backdrop-blur">
        <div className="mx-auto flex max-w-[1680px] flex-wrap items-center gap-x-4 gap-y-3 px-4 py-3 md:px-6 lg:h-16 lg:flex-nowrap lg:py-0">
          <Link to="/" className="flex items-center gap-2.5">
            <Logo className="size-6 md:size-7" />
            <span className="font-display text-lg font-bold tracking-tight">Factory</span>
          </Link>

          {/* Repos, the main screens and search: a row of their own until a laptop has room. */}
          <div className="order-last flex w-full min-w-0 items-center gap-2 max-md:hidden lg:order-none lg:w-auto lg:flex-1">
            {repos.length > 1 && (
              <div className="min-w-0 shrink overflow-x-auto">
                <RepoChips repos={repos} repo={repo} />
              </div>
            )}
            <nav aria-label="Main" className="flex shrink-0 gap-1">
              {links.map((l) => (
                <NavLink
                  key={l.to}
                  to={l.to}
                  end={l.end}
                  className={({ isActive }) =>
                    cn("rounded-md px-2.5 py-1.5 text-sm", isActive ? "bg-accent font-medium text-foreground" : "text-muted-foreground hover:text-foreground")
                  }
                >
                  {l.label}
                </NavLink>
              ))}
            </nav>
            <button
              onClick={() => setPalette(true)}
              className="ml-auto flex h-10 min-w-40 flex-1 cursor-pointer items-center gap-2.5 rounded-lg border bg-card px-3 text-left text-sm text-muted-foreground hover:border-primary/50 lg:h-9 lg:max-w-96"
            >
              <Search className="size-4 shrink-0" aria-hidden />
              <span className="flex-1 truncate">Jump to an issue or screen</span>
              <kbd className="rounded border px-1.5 font-mono text-xs">⌘K</kbd>
            </button>
          </div>

          <div className="ml-auto flex items-center gap-3 lg:ml-0">
            {o && <Running o={o} className="max-md:hidden" />}
            {o && <SpendMeter o={o} className="max-md:hidden" />}
            <button
              aria-label="Search"
              onClick={() => setPalette(true)}
              className="flex size-11 cursor-pointer items-center justify-center rounded-lg border bg-card md:hidden"
            >
              <Search className="size-4" />
            </button>
            <Link
              to="/notifications"
              aria-label={inbox ? `Inbox, ${inbox} need you` : "Inbox"}
              className="relative flex size-11 items-center justify-center rounded-lg border bg-card hover:bg-accent max-md:hidden md:size-9"
            >
              <Bell className="size-[18px]" />
              {inbox > 0 && <Count n={inbox} className="absolute -top-1.5 -right-1.5" />}
            </Link>
            {theme}
            {o?.signed_in && (
              <form method="post" action="/logout">
                <button
                  aria-label="Sign out"
                  title="Sign out"
                  className="flex size-11 shrink-0 cursor-pointer items-center justify-center rounded-lg border bg-card text-foreground hover:bg-accent md:size-9"
                >
                  <LogOut className="size-4" />
                </button>
              </form>
            )}
          </div>

          {/* Phone: what's running and the day's spend under the logo. */}
          {o && (
            <div className="flex w-full items-center gap-3 md:hidden">
              <Running o={o} />
              <SpendMeter o={o} className="flex-1" />
            </div>
          )}
          {repos.length > 1 && (
            <div className="-mx-4 w-[calc(100%+2rem)] overflow-x-auto px-4 md:hidden">
              <RepoChips repos={repos} repo={repo} />
            </div>
          )}
        </div>
      </header>
      <SetupBanner />
      <PlanUsageMeter className="border-b px-4 py-2 md:px-6" />
      {o && <MainHealth o={o} />}
      {o && <AutoMergeBreaker o={o} />}
      {o?.disk_low && (
        <div role="alert" className="border-b border-warning-border bg-warning-bg px-4 py-2.5 text-sm text-warning md:px-6">
          <div className="mx-auto max-w-[1680px]">{o.disk_low}</div>
        </div>
      )}
      <main className="mx-auto w-full max-w-[1680px] flex-1 px-4 py-5 pb-28 md:px-6 md:pb-8">
        <Outlet />
      </main>
      <nav
        aria-label="Tabs"
        className="fixed inset-x-0 bottom-0 z-20 grid grid-cols-5 border-t bg-surface pb-[env(safe-area-inset-bottom)] md:hidden"
      >
        {tabs.map(({ to, label, icon: Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            className={({ isActive }) =>
              cn("relative flex h-16 flex-col items-center justify-center gap-1 text-[11px]", isActive ? "text-foreground" : "text-muted-foreground")
            }
          >
            <Icon className="size-5" aria-hidden />
            {label}
            {to === "/notifications" && inbox > 0 && <Count n={inbox} className="absolute top-2 left-[55%]" />}
          </NavLink>
        ))}
      </nav>
      <CommandPalette open={palette} onClose={() => setPalette(false)} />
    </div>
  );
}

function RepoChips({ repos, repo }: { repos: string[]; repo: string }) {
  const chip = (value: string, label: string) => (
    <button
      key={value || "all"}
      aria-pressed={repo === value}
      onClick={() => setRepo(value)}
      className={cn(
        "h-9 shrink-0 cursor-pointer rounded-full border px-3 text-[13px] font-medium whitespace-nowrap lg:h-8",
        value && "font-mono",
        repo === value ? "border-foreground bg-foreground text-background" : "text-text-2 hover:border-muted-foreground",
      )}
    >
      {label}
    </button>
  );
  return (
    <div role="group" aria-label="Repositories" className="flex gap-1.5">
      {chip("", "All repos")}
      {repos.map((r) => chip(r, repoName(r)))}
    </div>
  );
}

function Running({ o, className }: { o: Overview; className?: string }) {
  return (
    <div
      title="Jobs running and queued"
      className={cn(
        className,
        "flex h-8 items-center gap-2 rounded-full px-3 text-[13px] font-medium whitespace-nowrap",
        o.running > 0 ? "bg-primary/10 text-primary-soft" : "bg-muted text-muted-foreground",
      )}
    >
      <span className={cn("size-2 rounded-full", o.running > 0 ? "pulse bg-primary" : "bg-muted-foreground")} />
      {o.running} running · {o.queued} queued
    </div>
  );
}

function SpendMeter({ o, className }: { o: Overview; className?: string }) {
  const share = o.daily_cap_usd > 0 ? Math.min(1, o.spent_today_usd / o.daily_cap_usd) : 0;
  const near = share >= 0.8;
  return (
    <div className={cn("flex flex-col gap-1.5 md:w-40", className)} title="Spent today (UTC) of the daily cap">
      <div className="flex justify-between gap-3 text-xs text-muted-foreground">
        <span>Today</span>
        <span className={cn("font-mono", near ? "font-semibold text-warning" : "text-foreground")}>
          {money(o.spent_today_usd)} / {money(o.daily_cap_usd)}
        </span>
      </div>
      <div className="h-1 rounded-full bg-border" role="meter" aria-label="Spent today" aria-valuenow={Math.round(share * 100)} aria-valuemin={0} aria-valuemax={100}>
        <div className={cn("h-1 rounded-full", near ? "bg-warning-fill" : "bg-success-fill")} style={{ width: `${share * 100}%` }} />
      </div>
    </div>
  );
}

function Count({ n, className }: { n: number; className?: string }) {
  return (
    <span className={cn("flex h-[18px] min-w-[18px] items-center justify-center rounded-full bg-warning-fill px-1 text-[11px] font-bold text-warning-ink", className)}>
      {n}
    </span>
  );
}

/** A line per repo whose base branch is red or flaky, with the fix issue. */
function MainHealth({ o }: { o: Overview }) {
  const bad = o.main_health.filter((h) => h.state === "red" || h.state === "flaky");
  if (bad.length === 0) return null;
  return (
    <section aria-label="Main health" className="border-b border-destructive/30 bg-destructive/10 px-4 py-2.5 text-sm md:px-6">
      <ul className="mx-auto max-w-[1680px] space-y-1">
        {bad.map((h) => (
          <li key={h.repo} className="min-w-0 break-words">
            <span className="font-medium text-destructive">
              {o.repos.length > 1 ? `${repoName(h.repo)} ` : ""}
              {h.branch} is {h.state === "red" ? "red" : "flaky"}
            </span>
            {": "}
            {h.checks.length === 0
              ? h.detail
              : h.checks.map((c, i) => (
                  <span key={c.check}>
                    {i > 0 && ", "}
                    <span className="font-mono">{c.check}</span>
                    {c.kind === "flaky" && h.state === "red" ? " (flaky)" : ""}{" "}
                    {c.issue > 0 ? (
                      <a href={c.issue_url} target="_blank" rel="noreferrer" className="underline underline-offset-2">
                        #{c.issue}
                      </a>
                    ) : (
                      <span className="text-muted-foreground">(not filed yet)</span>
                    )}
                  </span>
                ))}
          </li>
        ))}
      </ul>
    </section>
  );
}

/** A line per repo whose auto-merge breaker tripped, with its Reset button. */
function AutoMergeBreaker({ o }: { o: Overview }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const tripped = o.auto_merge.filter((b) => b.tripped);
  if (tripped.length === 0 && !msg) return null;
  const reset = async (repo: string) => {
    setBusy(repo);
    const r = await act("/auto-merge/reset", { repo });
    setBusy("");
    setMsg({ ok: r.ok, text: r.message });
    void qc.invalidateQueries();
  };
  return (
    <section aria-label="Auto-merge breaker" className="border-b border-warning-border bg-warning-bg px-4 py-2.5 text-sm md:px-6">
      <ul className="mx-auto max-w-[1680px] space-y-1">
        {tripped.map((b) => (
          <li key={b.repo} className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 break-words">
            <span className="min-w-0">
              <span className="font-medium text-warning">
                Auto-merge is off{o.repos.length > 1 ? ` for ${repoName(b.repo)}` : ""}
              </span>
              {": "}
              {b.reason}. New PRs wait for you.
            </span>
            {b.can_reset ? (
              <Button size="sm" variant="outline" disabled={busy === b.repo} onClick={() => reset(b.repo)}>
                Reset
              </Button>
            ) : (
              <span className="text-xs text-muted-foreground">
                Reset it with <span className="font-mono">factory automerge reset {b.repo}</span>, or here behind a dashboard password.
              </span>
            )}
          </li>
        ))}
        {msg && (
          <li role="status" className={cn(msg.ok ? "text-success" : "text-destructive")}>
            {msg.text}
          </li>
        )}
      </ul>
    </section>
  );
}
