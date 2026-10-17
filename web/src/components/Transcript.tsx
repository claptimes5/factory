import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

/** Follows a job's transcript over SSE; replays from the start on open. */
export function Transcript({ jobId }: { jobId: number }) {
  const [lines, setLines] = useState<string[]>([]);
  const [done, setDone] = useState("");
  const [follow, setFollow] = useState(true);
  const box = useRef<HTMLDivElement>(null);
  const qc = useQueryClient();
  useEffect(() => {
    setLines([]);
    setDone("");
    const es = new EventSource(`/api/v1/jobs/${jobId}/log`);
    let buf: string[] = [];
    const flush = setInterval(() => {
      if (buf.length) {
        const add = buf;
        buf = [];
        setLines((l) => (l.length + add.length > 5000 ? [...l, ...add].slice(-5000) : [...l, ...add]));
      }
    }, 150);
    es.addEventListener("line", (e) => buf.push((JSON.parse((e as MessageEvent).data) as { text: string }).text));
    es.addEventListener("done", (e) => {
      setDone((JSON.parse((e as MessageEvent).data) as { state: string }).state);
      es.close();
      void qc.invalidateQueries();
    });
    return () => {
      es.close();
      clearInterval(flush);
    };
  }, [jobId, qc]);
  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [lines, follow]);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="mb-1 flex items-center gap-2 text-xs text-muted-foreground">
        <span>{lines.length} lines</span>
        {done ? <span>· job {done}</span> : <span className="animate-pulse">· live</span>}
        <label className="ml-auto flex items-center gap-1">
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
        </label>
      </div>
      <div
        ref={box}
        data-testid="transcript"
        className="min-h-64 flex-1 overflow-auto rounded-md border bg-card p-2 font-mono text-xs leading-relaxed"
        onScroll={(e) => {
          const el = e.currentTarget;
          setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 40);
        }}
      >
        {lines.length === 0 && <p className="text-muted-foreground">{done ? "No transcript." : "Waiting for output…"}</p>}
        {lines.map((l, i) => (
          <div key={i} className={l.startsWith("→") ? "text-primary" : l.startsWith("result") ? "font-semibold" : ""}>
            {l}
          </div>
        ))}
      </div>
    </div>
  );
}
