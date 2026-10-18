import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { Search } from "lucide-react";
import { useBoard } from "@/api/client";
import { cn, shortRef, stageName } from "@/lib/utils";

type Entry = { key: string; label: string; hint: string; go: () => void };

/** ⌘K: jump to any item or screen by typing part of its name or number. */
export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const navigate = useNavigate();
  const { data } = useBoard();
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);

  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) {
      setQ("");
      setSel(0);
      d.showModal();
    } else if (!open && d.open) d.close();
  }, [open]);

  const entries = useMemo(() => {
    const go = (to: string) => () => {
      onClose();
      void navigate(to);
    };
    const screens: Entry[] = [
      { key: "s:/", label: "The floor", hint: "Screen", go: go("/") },
      { key: "s:/notifications", label: "Inbox", hint: "Screen", go: go("/notifications") },
      { key: "s:/live", label: "Live activity", hint: "Screen", go: go("/live") },
      { key: "s:/orchestrator", label: "Orchestrator decisions", hint: "Screen", go: go("/orchestrator") },
      { key: "s:/metrics", label: "Spend and metrics", hint: "Screen", go: go("/metrics") },
      {
        key: "s:classic",
        label: "Classic control room",
        hint: "Screen",
        go: () => {
          onClose();
          window.location.href = "/";
        },
      },
    ];
    const items: Entry[] = (data?.columns ?? []).flatMap((c) =>
      c.cards.map((k) => ({
        key: `i:${k.id}`,
        label: `${shortRef(k.ref)} ${k.title}`,
        hint: stageName(c.stage),
        go: go(`/items/${k.id}`),
      })),
    );
    const t = q.trim().toLowerCase();
    const all = [...items, ...screens];
    return t ? all.filter((e) => e.label.toLowerCase().includes(t)) : [...screens, ...items];
  }, [data, q, navigate, onClose]);

  const pick = Math.min(sel, Math.max(0, entries.length - 1));
  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setSel((pick + 1) % Math.max(1, entries.length));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSel((pick - 1 + entries.length) % Math.max(1, entries.length));
    } else if (e.key === "Enter") {
      e.preventDefault();
      entries[pick]?.go();
    }
  };

  return (
    <dialog
      ref={ref}
      aria-label="Jump to"
      onClose={onClose}
      onClick={(e) => e.target === ref.current && onClose()}
      className="mx-auto mt-[12vh] w-[min(640px,calc(100vw-32px))] rounded-xl border bg-surface p-0 text-foreground shadow-2xl backdrop:bg-black/50"
    >
      <div className="flex items-center gap-2 border-b px-3">
        <Search className="size-4 text-muted-foreground" aria-hidden />
        <input
          autoFocus
          aria-label="Jump to an item or screen"
          aria-controls="palette-list"
          aria-activedescendant={entries[pick] ? `pal-${entries[pick].key}` : undefined}
          placeholder="Jump to an issue or screen"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setSel(0);
          }}
          onKeyDown={onKey}
          className="h-12 flex-1 bg-transparent text-base outline-none placeholder:text-muted-foreground"
        />
        <kbd className="rounded border px-1.5 font-mono text-xs text-muted-foreground">esc</kbd>
      </div>
      <ul id="palette-list" role="listbox" aria-label="Matches" className="max-h-[50vh] overflow-y-auto p-1.5">
        {entries.length === 0 && <li className="px-3 py-6 text-center text-sm text-muted-foreground">Nothing matches.</li>}
        {entries.map((e, i) => (
          <li
            key={e.key}
            id={`pal-${e.key}`}
            role="option"
            aria-selected={i === pick}
            onMouseMove={() => setSel(i)}
            onClick={e.go}
            className={cn("flex cursor-pointer items-center gap-3 rounded-md px-3 py-2.5 text-sm", i === pick && "bg-accent")}
          >
            <span className="min-w-0 flex-1 truncate">{e.label}</span>
            <span className="shrink-0 text-xs text-muted-foreground">{e.hint}</span>
          </li>
        ))}
      </ul>
    </dialog>
  );
}
