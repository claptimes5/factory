import { useState } from "react";
import { Link } from "react-router";
import { AlertTriangle, Maximize2, MessageSquarePlus } from "lucide-react";
import { ErrorBox, Loading } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Conversation, useChats } from "@/screens/Chat";
import { cn } from "@/lib/utils";

// The chat with the factory, on every screen: a panel at the right of a
// desk, a sheet from the bottom of a phone. It keeps the chat you were in
// (per browser); New chat starts another. The orchestrator screen has the
// full list.

const key = "ask-chat";

function savedChat() {
  try {
    return Number(localStorage.getItem(key)) || 0;
  } catch {
    return 0;
  }
}

/** The Ask panel's content. In a sheet, its header leaves room for the sheet's close button. */
export function Ask({ className, inSheet }: { className?: string; inSheet?: boolean }) {
  const { data, error, isLoading } = useChats();
  const [chatId, setChatId] = useState(savedChat);
  const open = (id: number) => {
    setChatId(id);
    try {
      if (id) localStorage.setItem(key, String(id));
      else localStorage.removeItem(key);
    } catch {
      /* the chat stays open until a reload */
    }
  };
  // A chat that's gone (or from another factory) starts a new one.
  const known = data?.chats.some((c) => c.id === chatId) ? chatId : 0;
  return (
    <div className={cn("flex min-h-0 flex-col", className)} data-testid="ask">
      <div className={cn("flex items-center gap-1 pb-3", inSheet && "pr-11 md:pr-9")}>
        <h2 className="flex-1 text-body font-semibold">Ask the factory</h2>
        {known > 0 && (
          <Button size="icon" variant="ghost" aria-label="New chat" title="New chat" onClick={() => open(0)}>
            <MessageSquarePlus />
          </Button>
        )}
        <Button size="icon" variant="ghost" asChild>
          <Link to={known ? `/orchestrator?chat=${known}` : "/orchestrator"} aria-label="Open the full chat" title="Open the full chat">
            <Maximize2 />
          </Link>
        </Button>
      </div>
      {isLoading ? (
        <Loading />
      ) : error ? (
        <ErrorBox error={error} />
      ) : (
        <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto">
          {known === 0 && <p className="text-body-sm text-text-2">Ask about the factory's work, or say what you want built: it drafts the issues.</p>}
          {!data!.available && (
            <p className="flex items-start gap-1.5 text-body-sm text-warning">
              <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden /> {data!.unavailable}
            </p>
          )}
          <Conversation key={known} chatId={known} info={data!} onStarted={open} compact />
        </div>
      )}
    </div>
  );
}
