import { Fragment, type ReactNode } from "react";

/**
 * Renders the little Markdown agents write (paragraphs, headings, lists,
 * code blocks, inline code, bold, italics and links) as React elements.
 * The text comes from an untrusted agent, so nothing is ever set as HTML,
 * and only http(s) links become links.
 */
export function Markdown({ text }: { text: string }) {
  return <div className="space-y-2 text-sm leading-relaxed break-words">{blocks(text)}</div>;
}

function blocks(text: string): ReactNode[] {
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  const out: ReactNode[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const fence = /^\s*(`{3,}|~{3,})/.exec(line);
    if (fence) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith(fence[1])) body.push(lines[i++]);
      i++;
      out.push(
        <pre key={out.length} className="overflow-x-auto rounded-md bg-muted p-2 font-mono text-xs">
          {body.join("\n")}
        </pre>,
      );
      continue;
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) {
      out.push(
        <p key={out.length} className={h[1].length <= 2 ? "font-semibold" : "font-medium"}>
          {inline(h[2])}
        </p>,
      );
      i++;
      continue;
    }
    if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) {
      const ordered = /^\s*\d/.test(line);
      const items: string[] = [];
      while (i < lines.length && /^\s*([-*+]|\d+[.)])\s+/.test(lines[i])) {
        let item = lines[i].replace(/^\s*([-*+]|\d+[.)])\s+/, "");
        i++;
        while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !/^\s*([-*+]|\d+[.)])\s+/.test(lines[i])) item += " " + lines[i++].trim();
        items.push(item);
      }
      const L = ordered ? "ol" : "ul";
      out.push(
        <L key={out.length} className={ordered ? "list-decimal space-y-0.5 pl-5" : "list-disc space-y-0.5 pl-5"}>
          {items.map((t, k) => (
            <li key={k}>{inline(t)}</li>
          ))}
        </L>,
      );
      continue;
    }
    if (line.trim() === "") {
      i++;
      continue;
    }
    const para: string[] = [lines[i++]];
    while (i < lines.length && lines[i].trim() !== "" && !/^\s*(`{3,}|~{3,}|#{1,6}\s|([-*+]|\d+[.)])\s+)/.test(lines[i])) para.push(lines[i++]);
    out.push(
      <p key={out.length} className="whitespace-pre-line">
        {inline(para.join("\n"))}
      </p>,
    );
  }
  return out;
}

const token = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(\*[^*\s][^*\n]*\*|_[^_\s][^_\n]*_)|(\[[^\]\n]+\]\([^)\s]+\))/g;

function inline(s: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  for (const m of s.matchAll(token)) {
    const at = m.index ?? 0;
    if (at > last) out.push(<Fragment key={out.length}>{s.slice(last, at)}</Fragment>);
    const t = m[0];
    if (m[1]) {
      out.push(
        <code key={out.length} className="rounded bg-muted px-1 font-mono text-[0.85em]">
          {t.slice(1, -1)}
        </code>,
      );
    } else if (m[2]) {
      out.push(<strong key={out.length}>{t.slice(2, -2)}</strong>);
    } else if (m[3]) {
      out.push(<em key={out.length}>{t.slice(1, -1)}</em>);
    } else {
      const [, label, href] = /^\[([^\]]+)\]\(([^)]+)\)$/.exec(t) ?? [];
      out.push(
        /^https?:\/\//.test(href ?? "") ? (
          <a key={out.length} href={href} target="_blank" rel="noreferrer noopener" className="text-primary underline">
            {label}
          </a>
        ) : (
          <Fragment key={out.length}>{label}</Fragment>
        ),
      );
    }
    last = at + t.length;
  }
  if (last < s.length) out.push(<Fragment key={out.length}>{s.slice(last)}</Fragment>);
  return out;
}
