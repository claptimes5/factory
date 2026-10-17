import { useEffect, useState } from "react";
import type { Notification as Note } from "@/api/client";

// Browser notifications for new inbox entries. They show while a dashboard
// tab is open (the daemon has no push service to reach a closed one); the
// configured Slack or ntfy channel covers that case.

const seenKey = "notified";

/** The ids already shown, or null in a browser that has never loaded the inbox. */
function loadSeen(): Set<string> | null {
  try {
    const raw = localStorage.getItem(seenKey);
    return raw === null ? null : new Set(JSON.parse(raw) as string[]);
  } catch {
    return null;
  }
}

export function notificationsSupported() {
  return typeof window !== "undefined" && "Notification" in window;
}

export function usePermission() {
  const [perm, setPerm] = useState<NotificationPermission | "unsupported">(
    notificationsSupported() ? Notification.permission : "unsupported",
  );
  const request = async () => {
    if (!notificationsSupported()) return;
    setPerm(await Notification.requestPermission());
  };
  return [perm, request] as const;
}

export function useBrowserNotifications(notes: Note[] | undefined) {
  useEffect(() => {
    if (!notes) return;
    const seen = loadSeen();
    // Do not replay the whole inbox on the first load of a new browser.
    const fresh = seen === null ? [] : notes.filter((n) => !seen.has(n.id));
    if (notificationsSupported() && Notification.permission === "granted") {
      for (const n of fresh.slice(0, 3)) {
        const note = new Notification(n.title, { body: n.detail, tag: n.id });
        note.onclick = () => {
          window.focus();
          window.location.assign(n.item_id ? `/app/items/${n.item_id}` : "/app/notifications");
        };
      }
    }
    try {
      localStorage.setItem(seenKey, JSON.stringify(notes.map((n) => n.id)));
    } catch {
      /* ignore */
    }
  }, [notes]);
}
