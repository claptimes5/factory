// Factory's service worker: it shows the factory's push notifications,
// and a tap opens the dashboard (docs/plans/design-system.md, phase 5).
// It caches nothing: the dashboard is live data.

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

self.addEventListener("push", (e) => {
  let m = { title: "Factory", body: "", url: "/app/" };
  try {
    m = { ...m, ...e.data.json() };
  } catch {
    if (e.data) m.body = e.data.text();
  }
  e.waitUntil(
    self.registration.showNotification(m.title, {
      body: m.body,
      icon: "/app/icon-192.png",
      badge: "/app/icon-192.png",
      tag: m.tag || undefined,
      data: { url: m.url || "/app/" },
    }),
  );
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = new URL(e.notification.data?.url || "/app/", self.location.origin).href;
  e.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
      const open = list.find((c) => c.url.startsWith(self.location.origin + "/app/"));
      if (open) return open.focus().then((c) => c.navigate(url));
      return self.clients.openWindow(url);
    }),
  );
});
