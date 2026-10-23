# Plan: a review loop that settles

Status: designed with the owner, being built. Follows docs/quality.md,
"Review findings", and docs/using.md, "Trust".

## Why

The factory exists to merge changes with high trust and little of a
person's attention. Its review loop instead rewards "no reviewer
objects": any true finding a reviewer calls major blocks, and each block
costs a full round (a fresh container, an install, the builder relearning
the code, the full suite twice).

Seen on a small high-risk item in a Rails repo: a one-line guard fixed the
issue in round 1, and the gates proved it. Every later block was about the
test the factory itself wrote, and each fix to that test raised the next
finding. Three haiku builds and two sonnet builds later (a rethink at the
round cap), it reached a PR. The builds were about 70% of the tokens and
most of the hour, nearly all of it rerunning the suite.

So a round should only buy what a merge needs, and a round that's needed
should be cheap.

## 1. What can block

**The verifier rules on whether a finding matters to the merge.** Beside
`confirmed` and `refuted`, it can rule `note`: true, but it doesn't
threaten the merge. What threatens one, in its prompt: wrong behavior or a
part of the issue left undone, a regression, a test that doesn't prove the
fix or would break the suite, a security or data-safety problem. Style,
comments, how a test is written and design preferences are notes. A `note`
stops blocking and goes on the PR, as a refuted finding does, under its own
heading. Critical findings and the security pass's findings can't be made
notes: a `note` ruling on one counts as `confirmed`. When the verifier
can't run, a finding blocks, as before.

**Rounds settle.** In a re-review, a new finding blocks only when it's on
lines changed since that reviewer last looked. One on code the reviewer already read and
passed is kept as a note. A regression a fix introduces is on lines that
fix changed, so it still blocks. A whole-change review (the first round, a
workspace change, a rethink) is unaffected.

## 2. Cheaper fix rounds

**A fix round continues the last build.** When review blocks a build, its
checkout and CLI session are kept, as Continue keeps a stopped build's.
The fix round resumes them, with the findings as the next message, so the
builder starts knowing the code and what it tried. When nothing was kept,
or it can't be read, the round starts fresh as before. A rethink always
starts fresh from the base branch: that's what it is for. Nothing is
loosened: the checkout stays on the factory's machine and nothing is pushed
until the gates pass.

**A fix round runs targeted checks.** Its prompt asks for the tests of
what it changed and the linter on the files it changed. The gates still run
the full checks, and a gate failure already earns one more try with their
output.

Fix rounds keep their model; whether they should move up a tier is for the
metrics once rounds resume their session. A dependency cache across jobs is
out of scope.

## 3. Notes and auto-merge

A note is a model's judgment that a true finding doesn't matter, and
nothing has tested that judgment yet. So a PR with open notes waits for a
person, even where `auto_merge` would merge it. (Refuted findings don't
hold it, as today; a refuted security finding does, as today.)

Later, once notes have a record (they start with this change, so there's
none to read yet), the Trust screen earns the rest: beside the levels' practice runs, it
counts the merged PRs that had notes in the window and how many went wrong
(reverted, broke main). After 10 with none gone wrong, it suggests letting
them merge themselves: `auto_merge.with_notes: true`, which a person turns
on. One setting, not one per kind of finding: the kinds are fuzzy and would
each take long to earn. A kind of note that keeps mattering belongs in the
verifier's prompt as merge-relevant.

## 4. Testing and measuring

One test per behavior, beside their siblings: a `note` ruling stops a
block; a critical or security finding stays blocking under a `note`; a
re-review's new finding on unchanged lines is a note and on changed lines
blocks; a fix round resumes a kept build, and starts fresh with none; a
rethink never resumes; a fix round's prompt asks for targeted checks; auto-
merge waits with open notes.

Rounds per item and build tokens per merged PR, before and after, say
whether it worked.

## Not in this plan

Triage's risk rating holds even when the diff is rated lower, so a
high-risk item never merges itself, and its critic runs, however small the
change. Letting the diff lower risk loosens a guard rail: it is its own
decision, on evidence, once this has run.
