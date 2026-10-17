import { Fragment, useEffect, useMemo, useState } from "react";
import { parse } from "diff2html/lib-esm/diff-parser";
import type { DiffFile, DiffLine } from "diff2html/lib-esm/types";
import type { Finding } from "@/api/client";
import { FindingRow, findingAnchor } from "@/components/PRPanel";
import { cn } from "@/lib/utils";

type Row = { left?: DiffLine; right?: DiffLine };

/** Pairs deleted and inserted runs so a side-by-side view lines them up. */
function sideBySide(lines: DiffLine[]): Row[] {
  const rows: Row[] = [];
  let del: DiffLine[] = [];
  let ins: DiffLine[] = [];
  const flush = () => {
    for (let i = 0; i < Math.max(del.length, ins.length); i++) rows.push({ left: del[i], right: ins[i] });
    del = [];
    ins = [];
  };
  for (const l of lines) {
    if (l.type === "delete") del.push(l);
    else if (l.type === "insert") ins.push(l);
    else {
      flush();
      rows.push({ left: l, right: l });
    }
  }
  flush();
  return rows;
}

const fileName = (f: DiffFile) => (f.newName === "/dev/null" ? f.oldName : f.newName);
const text = (l?: DiffLine) => (l ? l.content.slice(1) : "");

function Cell({ line, side }: { line?: DiffLine; side: "left" | "right" | "unified" }) {
  const n = side === "left" ? line?.oldNumber : side === "right" ? line?.newNumber : (line?.newNumber ?? line?.oldNumber);
  const kind = line?.type;
  return (
    <>
      <td className="w-10 border-r px-1 text-right align-top text-muted-foreground select-none">{n ?? ""}</td>
      <td
        className={cn(
          "px-2 align-top break-all whitespace-pre-wrap",
          !line && "bg-muted/50",
          kind === "insert" && "bg-success/12",
          kind === "delete" && "bg-destructive/12",
        )}
      >
        {side === "unified" && line && <span className="mr-1 text-muted-foreground select-none">{line.content[0]}</span>}
        {text(line)}
      </td>
    </>
  );
}

export function DiffView({ patch, findings, itemId }: { patch: string; findings: Finding[]; itemId: number }) {
  const files = useMemo(() => parse(patch), [patch]);
  const [mode, setMode] = useState<"split" | "unified">(() => (window.innerWidth < 900 ? "unified" : "split"));
  // Findings attach to the new-side line they name; the rest list above the file.
  const byFile = useMemo(() => {
    const m = new Map<string, Finding[]>();
    for (const f of findings) m.set(f.file, [...(m.get(f.file) ?? []), f]);
    return m;
  }, [findings]);
  useEffect(() => {
    const id = decodeURIComponent(location.hash.slice(1));
    if (id) document.getElementById(id)?.scrollIntoView({ block: "center" });
  }, [files]);
  const unplaced = findings.filter((f) => !files.some((d) => fileName(d) === f.file));
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-sm">
        <span className="text-muted-foreground">
          {files.length} files · {findings.length} findings
        </span>
        <div className="ml-auto flex rounded-md border p-0.5">
          {(["split", "unified"] as const).map((m) => (
            <button
              key={m}
              onClick={() => setMode(m)}
              className={cn("cursor-pointer rounded px-2 py-0.5 text-xs", mode === m ? "bg-accent font-medium" : "text-muted-foreground")}
            >
              {m === "split" ? "Side by side" : "Unified"}
            </button>
          ))}
        </div>
      </div>
      {unplaced.length > 0 && (
        <ul className="divide-y rounded-lg border bg-card px-3">
          {unplaced.map((f, i) => (
            <FindingRow key={i} f={f} itemId={itemId} />
          ))}
        </ul>
      )}
      {files.map((f) => {
        const name = fileName(f);
        const fs = byFile.get(name) ?? [];
        const lines = new Set(f.blocks.flatMap((b) => b.lines.map((l) => l.newNumber ?? -1)));
        const loose = fs.filter((x) => !lines.has(x.line));
        const at = (l?: DiffLine) => (l?.newNumber !== undefined ? fs.filter((x) => x.line === l.newNumber) : []);
        const cols = mode === "split" ? 4 : 2;
        return (
          <section key={name} className="overflow-hidden rounded-lg border bg-card" data-testid="diff-file">
            <header className="flex items-center gap-2 border-b bg-muted/50 px-3 py-2 font-mono text-xs">
              <span className="truncate">{name}</span>
              {f.isNew && <span className="text-success">new</span>}
              {f.isDeleted && <span className="text-destructive">deleted</span>}
              <span className="ml-auto text-success">+{f.addedLines}</span>
              <span className="text-destructive">−{f.deletedLines}</span>
            </header>
            {loose.length > 0 && (
              <ul className="divide-y border-b px-3">
                {loose.map((x, i) => (
                  <li key={i} id={findingAnchor(x)}>
                    <FindingRow f={x} itemId={itemId} />
                  </li>
                ))}
              </ul>
            )}
            {f.isBinary ? (
              <p className="p-3 text-sm text-muted-foreground">Binary file.</p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full table-fixed border-collapse font-mono text-xs leading-5">
                  <colgroup>
                    {mode === "split" ? (
                      <>
                        <col className="w-10" />
                        <col />
                        <col className="w-10" />
                        <col />
                      </>
                    ) : (
                      <>
                        <col className="w-10" />
                        <col />
                      </>
                    )}
                  </colgroup>
                  <tbody>
                    {f.blocks.map((b, bi) => (
                      <Fragment key={bi}>
                        <tr>
                          <td colSpan={cols} className="bg-primary/8 px-2 py-0.5 text-muted-foreground">
                            {b.header}
                          </td>
                        </tr>
                        {(mode === "split" ? sideBySide(b.lines) : b.lines.map((l) => ({ left: l, right: l }))).map((r, ri) => {
                          const notes = at(r.right?.type === "delete" ? undefined : r.right);
                          return (
                            <Fragment key={ri}>
                              <tr>
                                {mode === "split" ? (
                                  <>
                                    <Cell line={r.left} side="left" />
                                    <Cell line={r.right} side="right" />
                                  </>
                                ) : (
                                  <Cell line={r.left} side="unified" />
                                )}
                              </tr>
                              {notes.map((x, i) => (
                                <tr key={i} id={findingAnchor(x)} data-testid="inline-finding">
                                  <td colSpan={cols} className="border-y border-warning/40 bg-warning/8 px-3 font-sans">
                                    <ul>
                                      <FindingRow f={x} itemId={itemId} />
                                    </ul>
                                  </td>
                                </tr>
                              ))}
                            </Fragment>
                          );
                        })}
                      </Fragment>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        );
      })}
    </div>
  );
}
