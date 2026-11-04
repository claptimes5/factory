#!/usr/bin/env python3
"""check-pr-body.py fails when a pull request's description leaves out how
it was tested, what it risks or how to revert it.

It reads the description on stdin. Those three sections are what a
reviewer needs before merging, and an empty one means nobody wrote it
down: .github/pull_request_template.md asks for them, and Factory fills
them in its own pull requests. A section counts when its heading names it
("Testing", "Risk" or "How to review", "How to revert") and something
other than the template's comments sits under it.
"""

import re
import sys

REQUIRED = {
    "Testing": ("test", "verif"),
    "Risk": ("risk", "how to review"),
    "How to revert": ("revert", "roll back", "rollback"),
}


def sections(body):
    """Return {heading: text} for each Markdown heading in body."""
    out, heading = {}, None
    for line in body.splitlines():
        m = re.match(r"\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$", line)
        if m:
            heading = m.group(1).lower()
            out.setdefault(heading, "")
        elif heading is not None:
            out[heading] += line + "\n"
    return out


def missing(body):
    body = re.sub(r"<!--.*?-->", "", body.replace("\r\n", "\n"), flags=re.S)
    found = sections(body)
    return [name for name, words in REQUIRED.items()
            if not any(any(w in h for w in words) and text.strip() for h, text in found.items())]


def main():
    gaps = missing(sys.stdin.read())
    if gaps:
        print("The description needs these sections filled in: " + ", ".join(gaps) + ".\n"
              "Edit the pull request's description (the template has a heading for each); "
              "this check reruns when you save it.", file=sys.stderr)
        sys.exit(1)
    print("The description says how it was tested, what it risks and how to revert it.")


if __name__ == "__main__":
    main()
