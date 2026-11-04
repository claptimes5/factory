#!/usr/bin/env python3
"""guard.py is the Claude Code hook that enforces what AGENTS.md asks.

Advice in AGENTS.md can be missed; these checks can't. They run on every
interactive Claude Code session in this repository (Factory's own builders
start Claude with --setting-sources user, so they never load them):

  pre-edit   Before Edit, Write or NotebookEdit: an edit to a protected
             path needs a person to approve it.
  pre-bash   Before a shell command: force pushes, --no-verify and rm -rf
             outside the checkout are refused; shell writes to a protected
             path need approval; a commit first runs scripts/verify.sh
             --quick and is refused if it fails.
  post-edit  After an edit: gofmt -w on the Go file.

It reads the hook's JSON on stdin and uses only the standard library, so
it runs on any machine with Python 3. settings.json turns a crash into a
refusal ("|| exit 2"): a guard that fails open is no guard.
"""

import json
import os
import shlex
import subprocess
import sys

# PROTECTED mirrors docs/factory/BASELINE.md and AGENTS.md "Take care
# with". A trailing slash protects a directory. internal/web/web.go is
# protected whole because its request guard lives there.
PROTECTED = (
    "internal/runner/",
    "internal/egress/",
    "internal/web/auth.go",
    "internal/web/web.go",
    "internal/pipeline/guard.go",
    ".github/",
    ".claude/",
    "prompts/",
    "factory.example.yaml",
    ".golangci.yml",
    "deploy/",
    "docs/factory/",
)

# Commands whose arguments are all files they write, and those whose last
# argument is the one they write.
WRITES_ALL = {"rm", "tee", "touch", "truncate", "chmod", "unlink"}
WRITES_LAST = {"cp", "mv", "install", "ln", "rsync"}


def repo_root(path):
    """Return the work tree containing path, or None.

    It walks up to the nearest .git (a directory, or a file in a worktree)
    rather than asking git, because path may not exist yet. A worktree
    under .claude/worktrees is its own root, so its files aren't seen as
    the main checkout's .claude/.
    """
    d = path if os.path.isdir(path) else os.path.dirname(path)
    while True:
        if os.path.exists(os.path.join(d, ".git")):
            return d
        parent = os.path.dirname(d)
        if parent == d:
            return None
        d = parent


def protected(path, cwd):
    """Return the protected entry path falls under, or None."""
    path = os.path.realpath(os.path.join(cwd, os.path.expanduser(path)))
    root = repo_root(path)
    if root is None:
        return None
    rel = os.path.relpath(path, os.path.realpath(root))
    if os.path.isdir(path):
        rel += "/"
    for p in PROTECTED:
        if rel == p or (p.endswith("/") and rel.startswith(p)):
            return p
    return None


def ask(reason):
    json.dump({"hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "ask",
        "permissionDecisionReason": reason,
    }}, sys.stdout)
    sys.exit(0)


def refuse(reason):
    print(reason, file=sys.stderr)
    sys.exit(2)


def ask_protected(p):
    ask(f"{p} is a protected path (AGENTS.md, \"Take care with\"). "
        "Change it only when the task is about it, and only once a person "
        "approves this edit.")


def segments(command):
    """Split a shell command into simple commands, as lists of words.

    It understands quoting and ;, &&, || and |, which covers what agents
    write. Redirections stay in as their own words (">", ">>").
    """
    lex = shlex.shlex(command, posix=True, punctuation_chars=";&|<>()")
    lex.whitespace_split = True
    lex.commenters = ""
    seg = []
    try:
        for w in lex:
            if w and set(w) <= set(";&|()"):
                if seg:
                    yield seg
                seg = []
            else:
                seg.append(w)
    except ValueError:
        # Unbalanced quotes: let the shell report it.
        pass
    if seg:
        yield seg


def git_subcommand(seg):
    """Return (subcommand, its args, -C directory) for a git command."""
    i, where = 1, None
    while i < len(seg) and seg[i].startswith("-"):
        if seg[i] in ("-C", "-c") and i + 1 < len(seg):
            if seg[i] == "-C":
                where = seg[i + 1]
            i += 2
        else:
            i += 1
    if i >= len(seg):
        return None, [], where
    return seg[i], seg[i + 1:], where


def short_flags(args):
    """Return the letters of every short-flag cluster, like -fn."""
    return {c for a in args if a.startswith("-") and not a.startswith("--") for c in a[1:]}


def check_git(seg, cwd):
    sub, args, where = git_subcommand(seg)
    if "--no-verify" in args or (sub == "commit" and "n" in short_flags(args)):
        refuse("Refused: --no-verify skips the checks this repository relies on. "
               "Fix what the hook or check reports instead.")
    if sub == "push":
        forced = any(a in ("-f", "--force", "--force-with-lease", "--force-if-includes")
                     or a.startswith("--force-with-lease=") or a.startswith("+")
                     for a in args) or "f" in short_flags(args)
        if forced:
            refuse("Refused: force pushes rewrite history others may have. "
                   "Push a new commit instead, or ask a person to do it.")
    if sub in ("checkout", "restore") and "--" in args:
        for a in args[args.index("--") + 1:]:
            if p := protected(a, cwd):
                ask_protected(p)
    if sub == "commit" and "--dry-run" not in args:
        verify_before_commit(os.path.join(cwd, where) if where else cwd)


def verify_before_commit(cwd):
    root = repo_root(os.path.realpath(cwd))
    script = root and os.path.join(root, "scripts", "verify.sh")
    if not script or not os.path.isfile(script):
        return
    r = subprocess.run([script, "--quick"], cwd=root, text=True,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if r.returncode != 0:
        tail = "\n".join(r.stdout.strip().splitlines()[-30:])
        refuse("Refused: scripts/verify.sh --quick fails, so this commit would "
               f"break the build. Fix it, then commit again.\n\n{tail}")


def check_rm(seg, cwd, workspace):
    args = seg[1:]
    longs = set(a for a in args if a.startswith("--"))
    shorts = short_flags(args)
    recursive = "r" in shorts or "R" in shorts or "--recursive" in longs
    force = "f" in shorts or "--force" in longs
    if not (recursive and force):
        return
    for a in args:
        if a.startswith("-"):
            continue
        if "$" in a or "`" in a:
            refuse(f"Refused: rm -rf on {a}, which the guard can't resolve. "
                   "Name the path directly.")
        t = os.path.realpath(os.path.join(cwd, os.path.expanduser(a)))
        inside = any(t.startswith(base + os.sep) for base in workspace)
        # The checkout itself, or a folder holding it, is never fair game,
        # even when it lives under the temporary directory.
        holds_checkout = any(c == t or c.startswith(t.rstrip(os.sep) + os.sep) for c in workspace[:2])
        if not inside or holds_checkout:
            refuse(f"Refused: rm -rf {a} reaches outside this checkout and the "
                   "temporary directory. Delete only inside them, or ask a person.")


def check_writes(seg, cwd):
    """Ask before common shell writes to a protected path.

    This is best effort: a script can write anywhere. It catches the
    everyday ways (redirection, sed -i, cp, mv, rm, tee), and code review
    and CODEOWNERS catch the rest.
    """
    targets = [seg[i + 1] for i, w in enumerate(seg[:-1]) if w in (">", ">>")]
    name, args = os.path.basename(seg[0]), [a for a in seg[1:] if not a.startswith("-")]
    if name in WRITES_ALL:
        targets += args
    elif name in WRITES_LAST and args:
        targets.append(args[-1])
    elif name == "sed" and any(a == "-i" or a.startswith("-i") or a.startswith("--in-place")
                               for a in seg[1:]):
        targets += args[1:]
    for t in targets:
        if p := protected(t, cwd):
            ask_protected(p)


def pre_bash(event):
    cwd = event.get("cwd") or os.getcwd()
    project = os.environ.get("CLAUDE_PROJECT_DIR") or cwd
    tmp = os.environ.get("TMPDIR") or "/tmp"
    workspace = [os.path.realpath(d) for d in (project, repo_root(os.path.realpath(cwd)) or project, tmp, "/tmp")]
    for seg in segments(event.get("tool_input", {}).get("command", "")):
        while seg and "=" in seg[0] and not seg[0].startswith("="):
            seg = seg[1:]  # FOO=bar cmd
        if not seg:
            continue
        name = os.path.basename(seg[0])
        if name == "cd" and len(seg) > 1:
            cwd = os.path.join(cwd, os.path.expanduser(seg[1]))
            continue
        check_writes(seg, cwd)
        if name == "git":
            check_git(seg, cwd)
        elif name == "rm":
            check_rm(seg, cwd, workspace)


def edited_path(event):
    ti = event.get("tool_input", {})
    return ti.get("file_path") or ti.get("notebook_path") or ""


def pre_edit(event):
    path = edited_path(event)
    if path and (p := protected(path, event.get("cwd") or os.getcwd())):
        ask_protected(p)


def post_edit(event):
    path = edited_path(event)
    if not path.endswith(".go") or not os.path.isfile(path):
        return
    r = subprocess.run(["gofmt", "-w", path], capture_output=True, text=True)
    if r.returncode != 0:
        # Exit 2 shows the error to Claude; the edit already happened.
        refuse(f"gofmt could not format {path}:\n{r.stderr.strip()}")


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else ""
    handler = {"pre-edit": pre_edit, "pre-bash": pre_bash, "post-edit": post_edit}.get(mode)
    if handler is None:
        refuse(f"guard.py: unknown mode {mode!r}")
    handler(json.load(sys.stdin))


if __name__ == "__main__":
    main()
