import { usePreview, type PreviewShot } from "@/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { shortSha } from "@/lib/utils";

/** The newest build's screenshots, by page; nothing when it took none. */
export function Screenshots({ itemId }: { itemId: number }) {
  const { data } = usePreview(itemId);
  if (!data || (data.shots.length === 0 && data.notes.length === 0)) return null;
  const pages: string[] = [];
  for (const s of data.shots) if (!pages.includes(s.page)) pages.push(s.page);
  return (
    <Card data-testid="screenshots">
      <CardHeader>
        <CardTitle>Screenshots</CardTitle>
        <span className="font-mono text-xs text-muted-foreground">
          {data.base && `${shortSha(data.base)} → `}
          {shortSha(data.head)}
        </span>
      </CardHeader>
      <CardContent className="space-y-4">
        {data.shots.length === 0 && <p className="text-sm text-muted-foreground">No screenshots this time.</p>}
        {pages.map((page) => (
          <div key={page}>
            <div className="mb-1 font-mono text-xs">{page}</div>
            <div className="flex flex-wrap items-start gap-2">
              {data.shots
                .filter((s) => s.page === page)
                .sort((a, b) => (a.side === b.side ? 0 : a.side === "before" ? -1 : 1))
                .map((s) => (
                  <Shot key={s.url} s={s} />
                ))}
            </div>
          </div>
        ))}
        {data.notes.length > 0 && (
          <details className="text-xs">
            <summary className="cursor-pointer text-muted-foreground">What couldn't be captured</summary>
            <pre className="mt-1 overflow-x-auto rounded-md bg-muted p-2 whitespace-pre-wrap">{data.notes.join("\n")}</pre>
          </details>
        )}
      </CardContent>
    </Card>
  );
}

function Shot({ s }: { s: PreviewShot }) {
  const label = `${s.page} at ${s.viewport}, ${s.side === "before" ? "before the change" : "with the change"}`;
  return (
    <a href={s.url} target="_blank" rel="noreferrer" className="group block w-36 shrink-0 sm:w-44" title={label}>
      <img
        src={s.url}
        alt={label}
        loading="lazy"
        className="max-h-56 w-full rounded border object-cover object-top group-hover:border-primary"
      />
      <span className="mt-0.5 block text-caption text-muted-foreground">
        {s.side} · {s.viewport}
      </span>
    </a>
  );
}
