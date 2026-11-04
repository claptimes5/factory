#!/usr/bin/env python3
"""ui-seeds.py is the seeded-UI-break check from Step 6 of the playbook.

Each seed breaks what a person meets in the browser: a button hidden, a
route moved, a form field renamed, an API response reshaped, the sign-in
skipped. For each, it applies the seed to a clone, rebuilds the dashboard
(the devserver serves the committed build), runs the whole browser suite,
records whether it went red (caught) or stayed green (MISSED), and puts
the clone back. It never touches the checkout you run it from: clone
first. It needs a Linux Node on the PATH, as verify does.

usage:
  git clone -q . /tmp/ui-seeds-clone
  python3 docs/factory/ui-seeds.py /tmp/ui-seeds-clone [seed id ...]

A seed whose text no longer matches says so; update it with the code.
Results also go to ui-seeds.json beside the clone.
"""
import json
import subprocess
import sys
import time

SEEDS = [
    ("U1", "hidden button", "web/src/screens/NotifySettings.tsx",
     "<Button disabled={busy} onClick={save}>", '<Button disabled={busy} onClick={save} className="hidden">',
     "Save quiet hours is hidden"),
    ("U2", "broken route", "web/src/main.tsx",
     '{ path: "/notify", element: <NotifySettingsScreen /> }', '{ path: "/notifications", element: <NotifySettingsScreen /> }',
     "the notifications screen moved and its links weren't updated"),
    ("U3", "renamed field", "web/src/screens/NotifySettings.tsx",
     'label="From" help="Empty is off"', 'label="Start" help="Empty is off"',
     "quiet hours' From field is now called Start"),
    ("U4", "API shape", "internal/web/api.go",
     'Title      string `json:"title"`', 'Title      string `json:"name"`',
     "an item's title comes back as name"),
    ("U5", "sign-in", "internal/web/auth.go",
     "case l.signedIn(r):", "case true:",
     "every request counts as signed in"),
]


def sh(cmd, cwd):
    return subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)


def run(clone, ids):
    if sh(["npm", "ci", "--no-audit", "--no-fund"], f"{clone}/web").returncode != 0:
        sys.exit("npm ci failed in the clone")
    results = []
    for sid, kind, path, old, new, what in SEEDS:
        if ids and sid not in ids:
            continue
        f = f"{clone}/{path}"
        src = open(f).read()
        if src.count(old) != 1:
            results.append({"id": sid, "kind": kind, "what": what, "result": f"seed text found {src.count(old)} times"})
            continue
        open(f, "w").write(src.replace(old, new))
        t = time.time()
        res, fails = "", []
        if path.startswith("web/") and sh(["npm", "run", "build"], f"{clone}/web").returncode != 0:
            res = "invalid (doesn't build)"
        else:
            r = sh(["npm", "run", "e2e"], f"{clone}/web")
            out = r.stdout + r.stderr
            fails = sorted({l.split("›")[2].strip()[:70] for l in out.splitlines() if "✘" in l and l.count("›") >= 2})
            res = "caught" if r.returncode != 0 else "MISSED"
        # Back to the clone as it was, built dashboard included.
        sh(["git", "checkout", "-q", "--", "."], clone)
        sh(["git", "clean", "-qfd", "--", "internal/web/app"], clone)
        results.append({"id": sid, "kind": kind, "what": what, "result": res, "by": fails[:3], "failed": len(fails), "secs": round(time.time() - t)})
        print(f"{sid:3} {res:8} {what}  ({len(fails)} failed: {'; '.join(fails[:2])})", flush=True)
    return results


if __name__ == "__main__":
    res = run(sys.argv[1], set(sys.argv[2:]))
    json.dump(res, open(sys.argv[1] + "/../ui-seeds.json", "w"), indent=1)
    caught = sum(r["result"] == "caught" for r in res)
    print(f"\ncaught {caught} of {len(res)}")
