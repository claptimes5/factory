import { useEffect, useState } from "react";
import { Link, Navigate, NavLink, Outlet, useLocation } from "react-router";
import { Activity, History, House, ShieldCheck, LayoutGrid, LogOut, MessageCircle, Moon, Search, SlidersHorizontal, Sun } from "lucide-react";
import { useLiveUpdates, useNotifications, useOverview, useWizardMode, type Overview } from "@/api/client";
import { Button } from "@/components/ui/button";
import { CommandPalette } from "@/components/CommandPalette";
import { PlanUsageMeter } from "@/components/PlanUsage";
import { Ask } from "@/components/Ask";
import { NoticeArea } from "@/components/Notices";
import { Dialog, DialogTitle, SheetContent } from "@/components/ui/dialog";
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

/**
 * Factory's mark: the line, with its stations on a track. The last one is
 * lit, the moment work reaches you or ships. The tile is ink, so it
 * inverts with the theme like the decision cards (public/icon.svg too).
 */
export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 28 28" aria-hidden className={className}>
      <rect width="28" height="28" rx="7" className="fill-ink" />
      <path d="M7 14h14" strokeWidth="1.6" strokeLinecap="round" className="stroke-ink-foreground opacity-45" />
      <circle cx="7" cy="14" r="2.1" className="fill-ink-foreground" />
      <circle cx="14" cy="14" r="2.1" className="fill-ink-foreground" />
      <circle cx="21" cy="14" r="3.2" className="fill-ink-attention" />
    </svg>
  );
}

const links = [
  { to: "/", label: "Home", icon: House, end: true },
  { to: "/line", label: "The line", icon: LayoutGrid },
  { to: "/history", label: "History", icon: History },
  { to: "/trust", label: "Trust", icon: ShieldCheck },
  { to: "/metrics", label: "Spend", icon: Activity },
  { to: "/settings", label: "Settings", icon: SlidersHorizontal },
];

// On a phone, Spend and the rest are reached from Settings.
const tabs = [
  { to: "/", label: "Home", icon: House, end: true },
  { to: "/line", label: "Line", icon: LayoutGrid },
  { to: "/history", label: "History", icon: History },
  { to: "/settings", label: "Settings", icon: SlidersHorizontal },
];

/**
 * Until the factory is set up, the dashboard serves only the get-started
 * page, so every route goes there. The wizard says which mode this is.
 */
export function Layout() {
  // A dashboard without the page answers 404: normal mode.
  const { data, isPending } = useWizardMode();
  if (isPending) return null;
  if (data?.mode === "setup") return <SetupShell />;
  return <FullLayout />;
}

function ThemeButton() {
  const [dark, setDark] = useTheme();
  return (
    <Button variant="ghost" size="icon" aria-label={dark ? "Switch to light mode" : "Switch to dark mode"} onClick={() => setDark(!dark)}>
      {dark ? <Sun /> : <Moon />}
    </Button>
  );
}

function SetupShell() {
  const here = useLocation().pathname === "/get-started";
  return (
    <div className="flex min-h-full flex-col">
      <header className="sticky top-0 z-20 border-b bg-surface/95 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-5xl items-center gap-4 px-4 md:px-6">
          <span className="flex items-center gap-2.5">
            <Logo className="size-6" />
            <span className="text-body font-semibold">Factory</span>
          </span>
          <span className="ml-auto" />
          <ThemeButton />
        </div>
      </header>
      <main className="mx-auto w-full max-w-5xl flex-1 px-4 py-6 pb-12 md:px-6">{here ? <Outlet /> : <Navigate to="/get-started" replace />}</main>
    </div>
  );
}

/** A screen wide enough to keep Ask beside the content (Tailwind's 2xl). */
function useWide() {
  const q = "(min-width: 1536px)";
  const [wide, setWide] = useState(() => window.matchMedia(q).matches);
  useEffect(() => {
    const m = window.matchMedia(q);
    const on = () => setWide(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return wide;
}

/** Whether the Ask panel is open beside the screen, on a wide desk; kept per browser. */
function useAskOpen() {
  const [open, setOpen] = useState(() => {
    try {
      return localStorage.getItem("ask-open") === "1";
    } catch {
      return false;
    }
  });
  const set = (v: boolean) => {
    setOpen(v);
    try {
      localStorage.setItem("ask-open", v ? "1" : "0");
    } catch {
      /* it stays as set until a reload */
    }
  };
  return [open, set] as const;
}

/**
 * The frame around every screen. On a desk: a sidebar with the screens,
 * the repo filter, search, what's running and the day's spend; the Ask
 * panel on the right when open. On a phone: a slim top bar, tabs at the
 * bottom, and Ask as a sheet. Warnings for every screen sit in one notice
 * area above the screen's own content.
 */
function FullLayout() {
  useLiveUpdates();
  const { data: o } = useOverview();
  const { data: n } = useNotifications();
  useBrowserNotifications(n?.notifications);
  const [palette, setPalette] = usePalette();
  // Beside the content on a wide screen; narrower, a sheet over it (from
  // the right on a laptop, the bottom on a phone), so the screen keeps its room.
  const wide = useWide();
  const [askDocked, setAskDocked] = useAskOpen();
  const [askSheet, setAskSheet] = useState(false);
  const askOpen = wide ? askDocked : askSheet;
  const setAskOpen = wide ? setAskDocked : setAskSheet;
  // Home's "Ask" box, and anything else, opens it with this event.
  useEffect(() => {
    const on = () => setAskOpen(true);
    window.addEventListener("factory:ask", on);
    return () => window.removeEventListener("factory:ask", on);
  });
  // Following a link out of the sheet (to the full chat, a job) closes it.
  const path = useLocation().pathname;
  useEffect(() => setAskSheet(false), [path]);
  const inbox = n?.notifications.length ?? 0;
  const repos = o?.repos ?? [];
  const repo = useRepo(repos);
  return (
    <div className="flex min-h-full">
      <aside className="sticky top-0 hidden h-dvh w-60 shrink-0 flex-col gap-5 overflow-y-auto border-r bg-surface px-3 py-5 lg:flex">
        <Link to="/" className="flex items-center gap-2.5 px-2.5">
          <Logo className="size-6" />
          <span className="text-body font-semibold">Factory</span>
        </Link>
        <button
          onClick={() => setPalette(true)}
          className="flex h-9 cursor-pointer items-center gap-2 rounded-control border bg-background px-2.5 text-left text-body-sm text-muted-foreground hover:border-muted-foreground/40"
        >
          <Search className="size-4 shrink-0" aria-hidden />
          <span className="flex-1 truncate">Jump to an issue or screen</span>
          <kbd className="rounded border px-1 font-mono text-caption">⌘K</kbd>
        </button>
        <nav aria-label="Main" className="flex flex-col gap-0.5">
          {links.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                cn(
                  "flex h-9 items-center gap-2.5 rounded-control px-2.5 text-body-sm",
                  isActive ? "bg-muted font-medium text-foreground" : "text-text-2 hover:bg-muted/60 hover:text-foreground",
                )
              }
            >
              <Icon className="size-4 shrink-0" aria-hidden />
              <span className="flex-1">{label}</span>
              {to === "/notifications" && inbox > 0 && <Count n={inbox} label={`${inbox} need you`} />}
            </NavLink>
          ))}
          <button
            onClick={() => setAskOpen(!askOpen)}
            aria-pressed={askOpen}
            className={cn(
              "flex h-9 cursor-pointer items-center gap-2.5 rounded-control px-2.5 text-left text-body-sm",
              askOpen ? "bg-muted font-medium text-foreground" : "text-text-2 hover:bg-muted/60 hover:text-foreground",
            )}
          >
            <MessageCircle className="size-4 shrink-0" aria-hidden />
            Ask the factory
          </button>
        </nav>
        {repos.length > 1 && <RepoChips repos={repos} repo={repo} wrap />}
        <div className="mt-auto flex flex-col gap-4 border-t px-2.5 pt-4">
          {o && <Running o={o} />}
          {o && <SpendMeter o={o} />}
          <PlanUsageMeter stacked />
          <div className="-mx-2.5 flex items-center gap-1">
            <ThemeButton />
            {o?.signed_in && <SignOut />}
          </div>
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-20 border-b bg-surface/95 backdrop-blur lg:hidden">
          <div className="flex h-14 items-center gap-2 px-4">
            <Link to="/" className="flex items-center gap-2.5">
              <Logo className="size-6" />
              <span className="text-body font-semibold">Factory</span>
            </Link>
            <span className="flex-1" />
            {o && <Running o={o} compact />}
            <Button variant="ghost" size="icon" aria-label="Search" onClick={() => setPalette(true)}>
              <Search />
            </Button>
            <ThemeButton />
            {o?.signed_in && <SignOut />}
          </div>
          {repos.length > 1 && (
            <div className="overflow-x-auto px-4 pb-2.5">
              <RepoChips repos={repos} repo={repo} />
            </div>
          )}
        </header>
        <main className="mx-auto w-full max-w-[1400px] flex-1 px-4 py-5 pb-28 md:px-6 lg:px-8 lg:py-7 lg:pb-10">
          <NoticeArea o={o} />
          <Outlet />
        </main>
      </div>

      {wide && askDocked && (
        <aside aria-label="Ask the factory" className="sticky top-0 flex h-dvh w-[380px] shrink-0 border-l bg-surface p-4">
          <Ask className="flex-1" />
        </aside>
      )}

      <nav aria-label="Tabs" className="fixed inset-x-0 bottom-0 z-20 grid grid-cols-5 border-t bg-surface pb-[env(safe-area-inset-bottom)] lg:hidden">
        {tabs.slice(0, 2).map((t) => (
          <Tab key={t.to} {...t} inbox={inbox} />
        ))}
        <button onClick={() => setAskSheet(true)} className="flex h-16 cursor-pointer flex-col items-center justify-center gap-1 text-caption text-muted-foreground">
          <MessageCircle className="size-5" aria-hidden />
          Ask
        </button>
        {tabs.slice(2).map((t) => (
          <Tab key={t.to} {...t} inbox={inbox} />
        ))}
      </nav>
      <Dialog open={!wide && askSheet} onOpenChange={setAskSheet}>
        <AskSheet />
      </Dialog>
      <CommandPalette open={palette} onClose={() => setPalette(false)} />
    </div>
  );
}

/** Ask over the screen: from the bottom of a phone, the right of a laptop. */
function AskSheet() {
  const lg = window.matchMedia("(min-width: 1024px)").matches;
  return (
    <SheetContent side={lg ? "right" : "bottom"} aria-describedby={undefined} className={lg ? "" : "h-[85dvh]"}>
      <DialogTitle className="sr-only">Ask the factory</DialogTitle>
      <Ask className="flex-1" inSheet />
    </SheetContent>
  );
}

function Tab({ to, label, icon: Icon, end, inbox }: (typeof tabs)[number] & { inbox: number }) {
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        cn("relative flex h-16 flex-col items-center justify-center gap-1 text-caption", isActive ? "font-medium text-foreground" : "text-muted-foreground")
      }
    >
      <Icon className="size-5" aria-hidden />
      {label}
      {to === "/notifications" && inbox > 0 && <Count n={inbox} label={`${inbox} need you`} className="absolute top-2 left-[55%]" />}
    </NavLink>
  );
}

function SignOut() {
  return (
    <form method="post" action="/logout">
      <Button variant="ghost" size="icon" aria-label="Sign out" title="Sign out">
        <LogOut />
      </Button>
    </form>
  );
}

function RepoChips({ repos, repo, wrap }: { repos: string[]; repo: string; wrap?: boolean }) {
  const chip = (value: string, label: string) => (
    <button
      key={value || "all"}
      aria-pressed={repo === value}
      onClick={() => setRepo(value)}
      className={cn(
        "h-9 shrink-0 cursor-pointer rounded-full border px-3 text-meta font-medium whitespace-nowrap lg:h-7 lg:px-2.5",
        value && "font-mono",
        repo === value ? "border-foreground bg-foreground text-background" : "text-text-2 hover:border-muted-foreground",
      )}
    >
      {label}
    </button>
  );
  return (
    <div role="group" aria-label="Repositories" className={cn("flex gap-1.5", wrap && "flex-wrap px-1")}>
      {chip("", "All repos")}
      {repos.map((r) => chip(r, repoName(r)))}
    </div>
  );
}

function Running({ o, className, compact }: { o: Overview; className?: string; compact?: boolean }) {
  return (
    <div
      title="Jobs running and queued"
      className={cn(
        className,
        "flex h-8 w-fit items-center gap-2 rounded-full px-3 text-meta font-medium whitespace-nowrap",
        o.running > 0 ? "bg-live-bg text-primary" : "bg-muted text-muted-foreground",
      )}
    >
      <span className={cn("size-2 rounded-full", o.running > 0 ? "pulse bg-primary" : "bg-muted-foreground")} />
      {compact ? `${o.running} running` : `${o.running} running · ${o.queued} queued`}
    </div>
  );
}

function SpendMeter({ o, className }: { o: Overview; className?: string }) {
  const share = o.daily_cap_usd > 0 ? Math.min(1, o.spent_today_usd / o.daily_cap_usd) : 0;
  const near = share >= 0.8;
  return (
    <div className={cn("flex flex-col gap-1.5", className)} title="Spent today (UTC) of the daily cap">
      <div className="flex justify-between gap-3 text-meta text-muted-foreground">
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

function Count({ n, label, className }: { n: number; label: string; className?: string }) {
  return (
    <span className={cn("flex h-[18px] min-w-[18px] items-center justify-center rounded-full bg-warning-fill px-1 text-caption font-bold text-warning-ink", className)}>
      <span aria-hidden>{n}</span>
      <span className="sr-only">{label}</span>
    </span>
  );
}
