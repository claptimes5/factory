import { useEffect, useState } from "react";
import { NavLink, Outlet } from "react-router";
import { Activity, Bell, Bot, LayoutGrid, Moon, Sun } from "lucide-react";
import { useLiveUpdates, useNotifications, useOverview } from "@/api/client";
import { useBrowserNotifications } from "@/lib/notify";
import { cn, money } from "@/lib/utils";

const nav = [
  { to: "/", label: "Board", icon: LayoutGrid, end: true },
  { to: "/notifications", label: "Inbox", icon: Bell },
  { to: "/orchestrator", label: "Orchestrator", icon: Bot },
  { to: "/metrics", label: "Spend", icon: Activity },
];

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

export function Layout() {
  useLiveUpdates();
  const { data: o } = useOverview();
  const { data: n } = useNotifications();
  useBrowserNotifications(n?.notifications);
  const [dark, setDark] = useTheme();
  const inbox = n?.notifications.length ?? 0;
  const near = o ? o.spent_today_usd >= 0.8 * o.daily_cap_usd : false;
  return (
    <div className="flex min-h-full flex-col">
      <header className="sticky top-0 z-20 border-b bg-background/90 backdrop-blur">
        <div className="mx-auto flex h-12 max-w-[1600px] items-center gap-4 px-4">
          <span className="font-semibold">Factory</span>
          <span className="hidden truncate text-sm text-muted-foreground sm:inline">{o?.repos.join(", ")}</span>
          <nav className="ml-2 hidden gap-1 md:flex" aria-label="Main">
            {nav.map(({ to, label, icon: Icon, end }) => (
              <NavLink
                key={to}
                to={to}
                end={end}
                className={({ isActive }) =>
                  cn("flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm", isActive ? "bg-accent font-medium" : "text-muted-foreground hover:text-foreground")
                }
              >
                <Icon className="size-4" /> {label}
                {to === "/notifications" && inbox > 0 && <Count n={inbox} />}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-3 text-xs text-muted-foreground">
            {o && (
              <>
                <span title="Jobs running / queued">
                  {o.running} running · {o.queued} queued
                </span>
                <span className={cn(near && "font-medium text-warning")} title="Spent today (UTC) of the daily cap">
                  {money(o.spent_today_usd)} / {money(o.daily_cap_usd)}
                </span>
              </>
            )}
            <a href="/" className="hidden hover:text-foreground lg:inline" title="The server-rendered control room">
              Classic view
            </a>
            <button aria-label="Toggle dark mode" className="cursor-pointer rounded p-1 hover:bg-accent" onClick={() => setDark(!dark)}>
              {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
            </button>
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-[1600px] flex-1 px-4 py-4 pb-24 md:pb-6">
        <Outlet />
      </main>
      {/* Phone: the main screens as a bottom tab bar. */}
      <nav aria-label="Tabs" className="fixed inset-x-0 bottom-0 z-20 grid grid-cols-4 border-t bg-background pb-[env(safe-area-inset-bottom)] md:hidden">
        {nav.map(({ to, label, icon: Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            className={({ isActive }) => cn("relative flex flex-col items-center gap-0.5 py-2 text-[11px]", isActive ? "text-primary" : "text-muted-foreground")}
          >
            <Icon className="size-5" />
            {label}
            {to === "/notifications" && inbox > 0 && (
              <span className="absolute top-1 left-1/2 ml-2">
                <Count n={inbox} />
              </span>
            )}
          </NavLink>
        ))}
      </nav>
    </div>
  );
}

function Count({ n }: { n: number }) {
  return <span className="rounded-full bg-destructive px-1.5 text-[10px] leading-4 font-semibold text-white">{n}</span>;
}
