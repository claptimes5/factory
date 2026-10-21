import { useEffect, useRef, useState } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

/**
 * A live shell in the job's container (xterm.js over the terminal WebSocket).
 * It connects only when asked: each connection starts a shell inside the
 * job's container, which lives until the job ends.
 */
export function Terminal({ jobId }: { jobId: number }) {
  const [on, setOn] = useState(false);
  if (!on) {
    return (
      <div className="flex h-full min-h-64 flex-col items-center justify-center gap-2 rounded-md border border-dashed p-4 text-center text-sm text-muted-foreground">
        <p>Open a shell inside this job's container.</p>
        <button onClick={() => setOn(true)} className="cursor-pointer rounded-md border bg-card px-3 py-1.5 font-medium text-foreground hover:bg-accent">
          Connect terminal
        </button>
      </div>
    );
  }
  return <LiveTerminal jobId={jobId} />;
}

function LiveTerminal({ jobId }: { jobId: number }) {
  const el = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState("connecting");
  useEffect(() => {
    const term = new XTerm({ convertEol: false, cursorBlink: true, fontSize: 12, fontFamily: "\"Geist Mono Variable\", ui-monospace, Menlo, monospace", theme: { background: "#0b0b0e" } });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(el.current!);
    fit.fit();
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(`${proto}://${location.host}/api/v1/jobs/${jobId}/terminal`);
    ws.binaryType = "arraybuffer";
    const send = (m: object) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify(m));
    ws.onopen = () => {
      setStatus("connected");
      send({ type: "resize", cols: term.cols, rows: term.rows });
      term.focus();
    };
    ws.onmessage = (e) => term.write(new Uint8Array(e.data as ArrayBuffer));
    ws.onclose = (e) => setStatus(e.reason ? `closed: ${e.reason}` : "closed");
    const input = term.onData((data) => send({ type: "input", data }));
    const ro = new ResizeObserver(() => {
      fit.fit();
      send({ type: "resize", cols: term.cols, rows: term.rows });
    });
    ro.observe(el.current!);
    return () => {
      ro.disconnect();
      input.dispose();
      ws.close();
      term.dispose();
    };
  }, [jobId]);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="mb-1 text-xs text-muted-foreground">Shell in the job's container · {status}</div>
      <div ref={el} data-testid="terminal" className="min-h-64 flex-1 overflow-hidden rounded-md border bg-terminal p-1" />
    </div>
  );
}
