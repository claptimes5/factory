#!/usr/bin/env python3
"""seeds.py is the seeded-regression check from Step 5 of the playbook.

Each seed is a realistic one-line bug in a critical flow's code. For
each, it applies the seed to a clone, runs that package's tests, records
whether they failed (caught) or passed (MISSED), and reverts the file.
It never touches the checkout you run it from: clone first.

usage:
  git clone -q . /tmp/seeds-clone
  python3 docs/factory/seeds.py /tmp/seeds-clone [seed id ...]

A seed whose text no longer matches says so; update it with the code.
Results also go to seeds.json beside the clone.
"""
import json
import subprocess
import sys
import time

SEEDS = [
    # Flow 5: dashboard login and request guard
    ("W1", "login", "internal/web/auth.go", "if err != nil || now.Unix() >= n {", "if err != nil {", "expired session token still accepted"),
    ("W2", "login", "internal/web/auth.go", ' || strings.HasPrefix(s, "//")', "", "open redirect via //host after sign-in"),
    ("W3", "login", "internal/web/auth.go", "return len(recent) >= maxFailures ||", "return len(recent) > maxFailures ||", "one extra password guess allowed (off-by-one)"),
    ("W4", "login", "internal/web/web.go", 'site != "same-origin" && site != "none"', 'site != "same-origin" && site != "same-site" && site != "none"', "same-site cross-origin POST accepted"),
    ("W5", "login", "internal/web/web.go", "if r.Method == http.MethodPost {\n\t\t\tif o := r.Header", "if r.Method == http.MethodPut {\n\t\t\tif o := r.Header", "Origin check no longer applies to POST"),
    # Flow 4: containment
    ("E1", "containment", "internal/egress/egress.go", 'return strings.HasSuffix(host, "."+r.Host)', "return strings.HasSuffix(host, r.Host)", "*.github.com matches evilgithub.com"),
    ("E2", "containment", "internal/egress/egress.go", "a.IsLinkLocalMulticast() || a.IsPrivate() ||", "a.IsLinkLocalMulticast() ||", "private networks reachable"),
    ("E3", "containment", "internal/egress/egress.go", "func Blocked(a netip.Addr) bool {\n\ta = a.Unmap()", "func Blocked(a netip.Addr) bool {", "IPv4-mapped IPv6 bypasses the block"),
    ("E4", "containment", "internal/egress/egress.go", "if p.Blocked(ip) && (!rule.Private || !privateOnly(ip)) {", "if p.Blocked(ip) && !rule.Private {", "a private rule reaches loopback and link-local"),
    ("R1", "containment", "internal/runner/runner.go", '"--cap-drop", "ALL", "--pids-limit", "1024",\n\t\t"--label"', '"--cap-drop", "NET_RAW", "--pids-limit", "1024",\n\t\t"--label"', "job container keeps most capabilities"),
    ("R2", "containment", "internal/runner/runner.go", "\tcase s.NoNetwork:\n", "\tcase s.NoNetwork && d.Egress == nil:\n", "a no-network job gets egress when egress is on"),
    ("R3", "containment", "internal/runner/runner.go", 'for _, k := range sortedKeys(s.Env) {\n\t\ta = append(a, "-e", k)\n\t}\n\ta = append(a, s.Image)', 'for _, k := range sortedKeys(s.Env) {\n\t\ta = append(a, "-e", k+"="+s.Env[k])\n\t}\n\ta = append(a, s.Image)', "secret env values on the docker command line"),
    # Flow 2: gates block bad changes
    ("G1", "gates", "internal/gates/gates.go", "if len(rs) == 0 {\n\t\treturn false", "if len(rs) == 0 {\n\t\treturn true", "no gate results counts as passed"),
    ("G2", "gates", "internal/gates/gates.go", '\t\t\tif req[g] {\n\t\t\t\tout = append(out, Result{Gate: g, Status: Misconfigured, Detail: "required gate has no command in factory.yaml"})\n\t\t\t}\n', "", "a required gate with no command is silently skipped"),
    ("G3", "gates", "internal/gates/gates.go", "if path.Base(p) == pp {", "if p == pp {", "a bare protected name only protects the root copy"),
    ("G4", "gates", "internal/gates/gates.go", 'if strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C") {', 'if strings.HasPrefix(code, "R") {', "copies misparsed, so a copy into a protected path slips"),
    ("G5", "gates", "internal/gates/gates.go", 'if strings.Contains(line, "t.Skip(") || strings.Contains(line, "t.SkipNow(") {', 'if strings.Contains(line, "t.SkipNow(") {', "an added t.Skip( isn't flagged"),
    ("P1", "gates", "internal/pipeline/stages.go", "\tif !gates.Passed(results) {\n\t\tif f := setupFault(results)", "\tif !gates.Passed(results[1:]) {\n\t\tif f := setupFault(results)", "a failed diff guard no longer stops the build"),
    # Flow 3: review rounds settle; auto-merge guard rails
    ("V1", "review", "internal/pipeline/stages.go", "if blocks > p.Cfg.Escalation.MaxReviewRounds {", "if blocks > p.Cfg.Escalation.MaxReviewRounds+1 {", "one review round more than the cap"),
    ("V2", "review", "internal/pipeline/stages.go", 'n := 0\n\tfor _, j := range jobs { // newest first\n\t\tif j.Kind == "triage" || isRethink(j) {', 'n := 0\n\tfor _, j := range jobs { // newest first\n\t\tif j.Kind == "triage" {', "a rethink doesn't get rounds of its own"),
    ("A1", "review", "internal/pipeline/guard.go", '\t\t\tp.warn(err, "Guard")\n\t\t\treturn true', '\t\t\tp.warn(err, "Guard")\n\t\t\treturn false', "breaker fails open when its state can't be read"),
    ("A2", "review", "internal/pipeline/guard.go", "for _, r := range p.sameWorkspace(repo) {\n\t\tg, err := p.coolOff", "for _, r := range []string{repo} {\n\t\tg, err := p.coolOff", "a workspace sibling's tripped breaker is ignored"),
    # Flow 1: issue to PR
    ("S1", "issue-to-pr", "internal/state/state.go", "Implementing:  {Reviewing, Parked, Blocked},", "Implementing:  {Reviewing, AwaitingHuman, Parked, Blocked},", "a build can skip review straight to a human"),
    ("S2", "issue-to-pr", "internal/pipeline/stages.go", 'case kind == "review" && it.Risk == "high" &&', 'case kind == "review" && it.Risk == "High" &&', "high-risk items never get the second critic"),
]


def run(clone, ids):
    results = []
    for sid, flow, path, old, new, what in SEEDS:
        if ids and sid not in ids:
            continue
        f = f"{clone}/{path}"
        src = open(f).read()
        if src.count(old) != 1:
            results.append({"id": sid, "flow": flow, "what": what, "result": f"seed text found {src.count(old)} times"})
            continue
        open(f, "w").write(src.replace(old, new))
        pkg = "./" + path.rsplit("/", 1)[0] + "/"
        t = time.time()
        r = subprocess.run(["go", "test", "-count=1", pkg], cwd=clone, capture_output=True, text=True)
        out = r.stdout + r.stderr
        open(f, "w").write(src)
        if "[build failed]" in out or "[setup failed]" in out:
            res = "invalid (doesn't build)"
        elif r.returncode != 0:
            res = "caught"
        else:
            res = "MISSED"
        fails = [l.split()[2] for l in out.splitlines() if l.startswith("--- FAIL")][:3]
        results.append({"id": sid, "flow": flow, "what": what, "result": res, "by": fails, "secs": round(time.time() - t)})
        print(f"{sid:3} {res:8} {what}  {' '.join(fails)}", flush=True)
    return results


if __name__ == "__main__":
    res = run(sys.argv[1], set(sys.argv[2:]))
    json.dump(res, open(sys.argv[1] + "/../seeds.json", "w"), indent=1)
    caught = sum(r["result"] == "caught" for r in res)
    print(f"\ncaught {caught} of {len(res)}")
