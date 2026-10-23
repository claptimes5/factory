import { useRef, useState } from "react";
import { FileText, Paperclip, X } from "lucide-react";
import { Button } from "@/components/ui/button";

/** A file picked to send, read as the base64 the API takes. */
export type Picked = { name: string; data: string; size: number; preview?: string };

// The daemon's limits on one message's files, checked here first so a
// person hears at once rather than after the upload.
const maxFiles = 10;
const maxBytes = 20 << 20;

const read = (f: File) =>
  new Promise<string>((ok, fail) => {
    const r = new FileReader();
    r.onload = () => ok(String(r.result).replace(/^data:[^,]*,/, ""));
    r.onerror = () => fail(r.error);
    r.readAsDataURL(f);
  });

/**
 * The files picked for the next message: add from the picker, a paste or
 * a drop; files is what goes in the request.
 */
export function useAttachments() {
  const [picked, setPicked] = useState<Picked[]>([]);
  const [error, setError] = useState("");
  const add = async (list: FileList | File[] | null | undefined) => {
    const fs = Array.from(list ?? []);
    if (fs.length === 0) return;
    const next = [...picked];
    for (const f of fs) {
      if (next.length === maxFiles) {
        setError(`Attach at most ${maxFiles} files to one message.`);
        break;
      }
      if (next.reduce((n, p) => n + p.size, 0) + f.size > maxBytes) {
        setError(`${f.name} is over the 20 MB one message carries.`);
        continue;
      }
      // A pasted screenshot comes as "image.png": name it so several differ.
      const name = f.name && f.name !== "image.png" ? f.name : `pasted-${next.length + 1}.${f.type.split("/")[1] || "png"}`;
      next.push({ name, data: await read(f), size: f.size, preview: f.type.startsWith("image/") ? URL.createObjectURL(f) : undefined });
      setError("");
    }
    setPicked(next);
  };
  const remove = (i: number) => {
    if (picked[i]?.preview) URL.revokeObjectURL(picked[i].preview);
    setPicked(picked.filter((_, j) => j !== i));
  };
  const clear = () => {
    picked.forEach((p) => p.preview && URL.revokeObjectURL(p.preview));
    setPicked([]);
    setError("");
  };
  return { picked, error, add, remove, clear, files: picked.map(({ name, data }) => ({ name, data })) };
}

export type Attachments = ReturnType<typeof useAttachments>;

/** The paperclip that opens the file picker. */
export function AttachButton({ a, disabled }: { a: Attachments; disabled?: boolean }) {
  const input = useRef<HTMLInputElement>(null);
  return (
    <>
      <Button type="button" size="sm" variant="outline" disabled={disabled} onClick={() => input.current?.click()} title="Attach files or images">
        <Paperclip /> <span className="sr-only">Attach files</span>
      </Button>
      <input
        ref={input}
        type="file"
        multiple
        hidden
        aria-label="Attach files"
        onChange={(e) => {
          void a.add(e.target.files);
          e.target.value = "";
        }}
      />
    </>
  );
}

/** The files picked so far, each with a way to take it off, and why one wasn't taken. */
export function AttachedList({ a }: { a: Attachments }) {
  if (a.picked.length === 0 && !a.error) return null;
  return (
    <div className="space-y-1">
      {a.picked.length > 0 && (
        <ul className="flex flex-wrap gap-2" aria-label="Attached files" data-testid="attached">
          {a.picked.map((p, i) => (
            <li key={p.name + i} className="flex items-center gap-1.5 rounded-control border bg-surface py-1 pr-1 pl-1.5 text-meta">
              {p.preview ? <img src={p.preview} alt="" className="size-8 rounded-control object-cover" /> : <FileText className="size-4 text-muted-foreground" />}
              <span className="max-w-40 truncate">{p.name}</span>
              <button type="button" onClick={() => a.remove(i)} className="rounded-control p-0.5 hover:bg-accent" aria-label={`Remove ${p.name}`}>
                <X className="size-3.5" />
              </button>
            </li>
          ))}
        </ul>
      )}
      {a.error && (
        <p role="alert" className="text-xs text-destructive">
          {a.error}
        </p>
      )}
    </div>
  );
}

/** Props that take pasted and dropped files into a, for a text box or form. */
export const takesFiles = (a: Attachments) => ({
  onPaste: (e: React.ClipboardEvent) => {
    if (e.clipboardData.files.length > 0) {
      e.preventDefault();
      void a.add(e.clipboardData.files);
    }
  },
  onDragOver: (e: React.DragEvent) => {
    if (e.dataTransfer.types.includes("Files")) e.preventDefault();
  },
  onDrop: (e: React.DragEvent) => {
    if (e.dataTransfer.files.length > 0) {
      e.preventDefault();
      void a.add(e.dataTransfer.files);
    }
  },
});
