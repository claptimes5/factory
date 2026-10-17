import { useId } from "react";

type Point = { label: string; value: number | null };

/** A small dependency-free SVG chart: bars or a line, with a y-axis max. */
export function Chart({ points, kind = "line", format = (v: number) => v.toFixed(2), height = 140, max }: {
  points: Point[];
  kind?: "line" | "bar";
  format?: (v: number) => string;
  height?: number;
  max?: number;
}) {
  const id = useId();
  const vals = points.map((p) => p.value).filter((v): v is number => v !== null);
  if (vals.length === 0) return <p className="py-8 text-center text-sm text-muted-foreground">No data yet.</p>;
  const top = max ?? (Math.max(...vals, 0) * 1.15 || 1);
  const w = 600;
  const h = height;
  const pad = { l: 44, r: 8, t: 8, b: 22 };
  const iw = w - pad.l - pad.r;
  const ih = h - pad.t - pad.b;
  const step = iw / Math.max(points.length, 1);
  const x = (i: number) => pad.l + step * i + step / 2;
  const y = (v: number) => pad.t + ih - (v / top) * ih;
  const every = Math.ceil(points.length / 8);
  const line = points
    .map((p, i) => (p.value === null ? null : `${x(i)},${y(p.value)}`))
    .filter(Boolean)
    .join(" ");
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="h-auto w-full text-primary" role="img" aria-labelledby={id}>
      <title id={id}>{points.map((p) => `${p.label}: ${p.value === null ? "none" : format(p.value)}`).join(", ")}</title>
      {[0, 0.5, 1].map((f) => (
        <g key={f}>
          <line x1={pad.l} x2={w - pad.r} y1={y(top * f)} y2={y(top * f)} className="stroke-border" strokeDasharray={f ? "3 3" : undefined} />
          <text x={pad.l - 6} y={y(top * f) + 4} textAnchor="end" className="fill-muted-foreground text-[10px]">
            {format(top * f)}
          </text>
        </g>
      ))}
      {kind === "bar"
        ? points.map((p, i) =>
            p.value === null ? null : (
              <rect key={i} x={x(i) - step * 0.35} width={step * 0.7} y={y(p.value)} height={Math.max(0, pad.t + ih - y(p.value))} rx={2} className="fill-primary/70">
                <title>{`${p.label}: ${format(p.value)}`}</title>
              </rect>
            ),
          )
        : (
          <>
            <polyline points={line} fill="none" stroke="currentColor" strokeWidth={2} />
            {points.map((p, i) =>
              p.value === null ? null : (
                <circle key={i} cx={x(i)} cy={y(p.value)} r={3} className="fill-primary">
                  <title>{`${p.label}: ${format(p.value)}`}</title>
                </circle>
              ),
            )}
          </>
        )}
      {points.map((p, i) =>
        i % every === 0 ? (
          <text key={i} x={x(i)} y={h - 6} textAnchor="middle" className="fill-muted-foreground text-[10px]">
            {p.label}
          </text>
        ) : null,
      )}
    </svg>
  );
}
