# Replay set

Each case is a past issue with a known-good merged fix, frozen at the commit
before the fix:

```
evals/
  add-retry-label/
    case.yaml
    accept/                 # laid over the factory's result, never shown to it
      internal/pipeline/retry_accept_test.go
```

`case.yaml`:

```yaml
title: Unpark items with a factory:retry label
body: |
  The issue text, as it was filed.
base_sha: 0123abcd...        # the commit the fix was built on
accept: go test -count=1 -run TestAccept ./internal/pipeline/
# repo: https://github.com/owner/other.git   # default: the configured repo
```

Pick the acceptance test from the merged fix's own tests, and keep 20 to 50
cases. `factory eval capture PR` writes one from a merged PR: `base_sha` is
where its branch left the base branch, the task is the issue its body
closes (else the PR's own title and body), `accept/` holds the test files
and test data it added or changed, as merged, and `accept` is the repo's
test gate unless you pass `-accept`. Start with the PRs you had to fix by
hand. If a fix came with no tests, add one to `accept/` that fails on the
factory's version; otherwise the case only checks that the suite still
passes. The command lists what it couldn't copy: a test the fix deleted
(`accept/` can only add files), or a symlink or submodule. Run `factory eval -out base.json` on the current config, then
`factory eval -config candidate.yaml -baseline base.json` before changing a
model, prompt, skill or routing threshold. The command fails if the
candidate's pass rate is lower or its cost per pass higher.
