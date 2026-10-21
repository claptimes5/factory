import { useEffect, useState } from "react";
import { Link } from "react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, BellRing, Moon, Smartphone, Trash2 } from "lucide-react";
import { act, getJSON, postJSON, saveSettings, useSettings, type Settings } from "@/api/client";
import type { components } from "@/api/schema";
import { ErrorBox, Loading, PageTitle } from "@/components/common";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { toast } from "@/components/ui/toaster";
import { ago } from "@/lib/utils";

// When to tell you: push on this device and the others, quiet hours, and
// what always gets through (cmd/factory/deliver.go sends them).

type Push = components["schemas"]["Push"];

const usePush = () => useQuery({ queryKey: ["push"], queryFn: () => getJSON<Push>("/push") });

export function NotifySettingsScreen() {
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-5" data-testid="notify-settings">
      <div>
        <PageTitle>When to tell you</PageTitle>
        <p className="-mt-2 text-body-sm text-text-2">
          The factory tells you when something needs you or goes wrong, on this phone and anywhere else you turn it on. In quiet hours it holds the rest
          for one catch-up.
        </p>
      </div>
      <section aria-label="What gets through" className="flex flex-col divide-y rounded-card border bg-surface">
        <Row title="Something's wrong" detail="Main broken, a revert, the budget or the disk run out" when="Always, even in quiet hours" />
        <Row title="Something needs you" detail="A change to review, a plan or a cost to OK, a question" when="Right away, or in the catch-up" />
        <Row title="Everything else" detail="What merged on its own, splits, lessons, the daily summary" when="Right away, or in the catch-up" />
      </section>
      <ThisDevice />
      <QuietHours />
      <p className="px-1 text-meta text-muted-foreground">
        The same messages go to ntfy, Slack or Discord when that's set up, on the{" "}
        <Link to="/get-started" className="text-primary hover:underline">
          get-started page
        </Link>
        .
      </p>
    </div>
  );
}

function Row({ title, detail, when }: { title: string; detail: string; when: string }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-4 py-3">
      <span className="flex min-w-0 flex-col">
        <span className="text-body-sm font-medium">{title}</span>
        <span className="text-meta text-muted-foreground">{detail}</span>
      </span>
      <span className="text-meta font-medium">{when}</span>
    </div>
  );
}

/** Whether this browser can take push: a service worker, the Push API and notifications. */
const pushSupported = () => "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;

/** On an iPhone or iPad, push works only for the web app added to the home screen. */
const iosBrowser = () => /iPhone|iPad|iPod/.test(navigator.userAgent) && !(navigator as { standalone?: boolean }).standalone;

/** A name for this device, as the list of devices shows it. */
function deviceName() {
  const ua = navigator.userAgent;
  const os = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android" : /Mac/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "a browser";
  return os === "iPhone" || os === "iPad" ? os : `${browser}${os ? ` on ${os}` : ""}`;
}

const b64 = (b: ArrayBuffer | null) => (b ? btoa(String.fromCharCode(...new Uint8Array(b))) : "");
function fromB64url(s: string) {
  const raw = atob(s.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

function ThisDevice() {
  const qc = useQueryClient();
  const { data, error, isLoading } = usePush();
  const [mine, setMine] = useState<PushSubscription | null | undefined>(undefined);
  const [busy, setBusy] = useState("");
  useEffect(() => {
    if (!pushSupported()) return setMine(null);
    void navigator.serviceWorker.ready.then((r) => r.pushManager.getSubscription()).then(setMine, () => setMine(null));
  }, []);
  if (isLoading) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const p = data!;
  const refresh = () => qc.invalidateQueries({ queryKey: ["push"] });
  const turnOn = async () => {
    setBusy("on");
    try {
      if ((await Notification.requestPermission()) !== "granted") {
        toast.error("Notifications are blocked for this site. Allow them in the browser's settings, then try again.");
        return;
      }
      const reg = await navigator.serviceWorker.ready;
      const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: fromB64url(p.public_key) });
      const json = sub.toJSON();
      await postJSON("/push/subscribe", { ...json, keys: json.keys ?? { p256dh: b64(sub.getKey("p256dh")), auth: b64(sub.getKey("auth")) }, agent: deviceName() });
      setMine(sub);
      toast.success("Notifications are on for this device.");
      await refresh();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy("");
    }
  };
  const turnOff = async () => {
    if (!mine) return;
    setBusy("off");
    await act("/push/unsubscribe", { endpoint: mine.endpoint });
    await mine.unsubscribe().catch(() => false);
    setMine(null);
    setBusy("");
    await refresh();
  };
  const test = async () => {
    setBusy("test");
    const r = await act("/push/test");
    setBusy("");
    if (r.ok) toast.success(r.message);
    else toast.error(r.message);
    await refresh();
  };
  const remove = async (id: string) => {
    await act("/push/unsubscribe", { id });
    await refresh();
  };
  return (
    <section aria-label="Devices" className="flex flex-col gap-3 rounded-card border bg-surface p-4">
      <h2 className="flex items-center gap-2 text-body-sm font-semibold">
        <Smartphone className="size-4" aria-hidden /> This device
      </h2>
      {!p.available ? (
        <p className="text-body-sm text-text-2">This factory can't send push notifications: its push key couldn't be read. See its log.</p>
      ) : !pushSupported() ? (
        <Alert variant="attention">
          <AlertTriangle aria-hidden />
          <span>
            {iosBrowser()
              ? "On an iPhone or iPad, add Factory to your home screen first: tap Share, then Add to Home Screen. Open it from there and turn notifications on."
              : "This browser can't take push notifications."}
          </span>
        </Alert>
      ) : mine ? (
        <div className="flex flex-wrap items-center gap-2">
          <span className="flex flex-1 items-center gap-2 text-body-sm text-success">
            <BellRing className="size-4" aria-hidden /> Notifications are on for this device.
          </span>
          <Button size="sm" variant="outline" disabled={!!busy} onClick={test}>
            Send a test
          </Button>
          <Button size="sm" variant="ghost" disabled={!!busy} onClick={turnOff}>
            Turn off
          </Button>
        </div>
      ) : mine === null ? (
        <div className="flex flex-col gap-2">
          <p className="text-body-sm text-text-2">Get a notification here when the factory needs you, even with the dashboard closed.</p>
          <div>
            <Button disabled={!!busy} onClick={turnOn}>
              <BellRing aria-hidden /> Turn on notifications
            </Button>
          </div>
        </div>
      ) : (
        <Loading />
      )}
      {p.devices.length > 0 && (
        <ul aria-label="Devices with notifications on" className="flex flex-col divide-y border-t">
          {p.devices.map((d) => (
            <li key={d.id} className="flex items-center gap-3 py-2.5">
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="text-body-sm">{d.agent || "A browser"}</span>
                <span className="text-meta text-muted-foreground">
                  On since {ago(d.created_at)}
                  {d.failed_at && <span className="text-warning">, last push failed {ago(d.failed_at)}</span>}
                </span>
              </span>
              <Button size="icon" variant="ghost" aria-label={`Turn off ${d.agent || "this device"}`} onClick={() => remove(d.id)}>
                <Trash2 />
              </Button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

const path = (k: string) => ["notify", "quiet_hours", k];

function QuietHours() {
  const qc = useQueryClient();
  const { data, error, isLoading } = useSettings();
  const [form, setForm] = useState<{ from: string; until: string; weekends: boolean; timezone: string } | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!data || form) return;
    const v = (k: string) => data.groups.flatMap((g) => g.fields).find((f) => f.path.join(".") === path(k).join("."))?.value;
    setForm({ from: (v("from") as string) ?? "", until: (v("until") as string) ?? "", weekends: (v("weekends") as boolean) ?? false, timezone: (v("timezone") as string) ?? "" });
  }, [data, form]);
  if (isLoading || !form) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const s = data!;
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const save = async () => {
    setBusy(true);
    try {
      const next = await saveSettings(
        [
          { path: path("from"), value: form.from || null },
          { path: path("until"), value: form.until || null },
          { path: path("weekends"), value: form.weekends },
          { path: path("timezone"), value: form.timezone || null },
        ],
        s.version,
      );
      qc.setQueryData<Settings>(["settings"], next);
      toast.success("Saved. Quiet hours apply once the factory restarts.");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section aria-label="Quiet hours" className="flex flex-col gap-4 rounded-card border bg-surface p-4">
      <h2 className="flex items-center gap-2 text-body-sm font-semibold">
        <Moon className="size-4" aria-hidden /> Quiet hours
      </h2>
      <div className="grid grid-cols-2 gap-3">
        <Field id="quiet-from" label="From" help="Empty is off">
          <Input id="quiet-from" type="time" value={form.from} disabled={!s.writable} onChange={(e) => setForm({ ...form, from: e.target.value })} />
        </Field>
        <Field id="quiet-until" label="Until">
          <Input id="quiet-until" type="time" value={form.until} disabled={!s.writable} onChange={(e) => setForm({ ...form, until: e.target.value })} />
        </Field>
      </div>
      <label className="flex items-center justify-between gap-3 text-body-sm">
        All weekend
        <Switch checked={form.weekends} disabled={!s.writable} onCheckedChange={(v) => setForm({ ...form, weekends: v })} />
      </label>
      <Field id="quiet-tz" label="Time zone" help={`Empty is the factory machine's. This browser's is ${tz}.`}>
        <Input id="quiet-tz" value={form.timezone} placeholder={tz} disabled={!s.writable} onChange={(e) => setForm({ ...form, timezone: e.target.value })} />
      </Field>
      {s.writable ? (
        <div>
          <Button disabled={busy} onClick={save}>
            Save quiet hours
          </Button>
        </div>
      ) : (
        <p className="text-meta text-muted-foreground">{s.reason}</p>
      )}
      {s.changed && <p className="text-meta text-muted-foreground">Saved changes apply once the factory restarts, from Settings.</p>}
    </section>
  );
}
