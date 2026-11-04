#!/usr/bin/env python3
"""Tests for guard.py: each runs the hook as Claude Code does, on a scratch repo."""

import json
import os
import subprocess
import sys
import tempfile
import unittest

GUARD = os.path.join(os.path.dirname(os.path.abspath(__file__)), "guard.py")


class Guard(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.repo = os.path.join(self.tmp.name, "repo")
        os.makedirs(os.path.join(self.repo, "scripts"))
        subprocess.run(["git", "init", "-q", self.repo], check=True)
        self.verify(0)

    def tearDown(self):
        self.tmp.cleanup()

    def verify(self, code):
        """Stand in for scripts/verify.sh, exiting with code."""
        p = os.path.join(self.repo, "scripts", "verify.sh")
        with open(p, "w") as f:
            f.write(f"#!/bin/sh\necho verify ran\nexit {code}\n")
        os.chmod(p, 0o755)

    def run_guard(self, mode, tool_input, factory_config=False):
        event = {"cwd": self.repo, "tool_input": tool_input}
        env = dict(os.environ, CLAUDE_PROJECT_DIR=self.repo, TMPDIR=self.tmp.name)
        env.pop("FACTORY_CONFIG", None)
        if factory_config:
            env["FACTORY_CONFIG"] = "1"
        return subprocess.run([sys.executable, GUARD, mode], input=json.dumps(event),
                              capture_output=True, text=True, env=env)

    def bash(self, command, factory_config=False):
        return self.run_guard("pre-bash", {"command": command}, factory_config)

    def edit(self, path, factory_config=False):
        return self.run_guard("pre-edit", {"file_path": os.path.join(self.repo, path)}, factory_config)

    def assertAsks(self, r):
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout)["hookSpecificOutput"]["permissionDecision"], "ask")

    def assertRefused(self, r):
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn("Refused", r.stderr)

    def assertAllowed(self, r):
        self.assertEqual((r.returncode, r.stdout), (0, ""), r.stderr)

    HARNESS = (".github/workflows/ci.yml", ".github/CODEOWNERS", ".claude/settings.json",
               ".claude/hooks/guard.py", "docs/factory/PROGRESS.md", "AGENTS.md", "CLAUDE.md",
               "REVIEW.md", "RISK.md")

    def test_edit_to_protected_path_asks(self):
        for p in ("internal/runner/docker.go", "internal/web/auth.go", "prompts/x.md", "deploy/upgrade.sh"):
            with self.subTest(p):
                self.assertAsks(self.edit(p))

    def test_edit_to_factory_file_is_refused_without_factory_config(self):
        for p in self.HARNESS:
            with self.subTest(p):
                r = self.edit(p)
                self.assertRefused(r)
                self.assertIn("FACTORY_CONFIG=1", r.stderr)

    def test_edit_to_factory_file_asks_with_factory_config(self):
        for p in self.HARNESS:
            with self.subTest(p):
                self.assertAsks(self.edit(p, factory_config=True))

    def test_edit_elsewhere_is_allowed(self):
        for p in ("internal/web/api.go", "internal/pipeline/pipeline.go", "docs/using.md", "docs/factory.md"):
            with self.subTest(p):
                self.assertAllowed(self.edit(p))

    def test_worktree_under_claude_dir_is_its_own_repo(self):
        wt = os.path.join(self.repo, ".claude", "worktrees", "w")
        os.makedirs(wt)
        open(os.path.join(wt, ".git"), "w").close()
        self.assertAllowed(self.run_guard("pre-edit", {"file_path": os.path.join(wt, "internal/web/api.go")}))
        self.assertAsks(self.run_guard("pre-edit", {"file_path": os.path.join(wt, "deploy/upgrade.sh")}))

    def test_git_directory_without_head_is_not_a_repo(self):
        # Like a home directory holding a tool's stray .git folder.
        home = os.path.join(self.tmp.name, "home")
        os.makedirs(os.path.join(home, ".git", "gk"))
        os.makedirs(os.path.join(home, ".claude", "memory"))
        self.assertAllowed(self.edit(os.path.join(home, ".claude", "memory", "note.md")))

    def test_shell_write_to_protected_path_asks(self):
        for c in ("echo x > deploy/x.sh", "sed -i 's/a/b/' prompts/review.md",
                  "cp /tmp/x .golangci.yml", "rm deploy/upgrade.sh", "cd internal && tee runner/x.go",
                  "git checkout -- internal/egress/egress.go"):
            with self.subTest(c):
                self.assertAsks(self.bash(c))

    def test_shell_write_to_factory_file_is_refused_without_factory_config(self):
        for c in ("echo x > .github/CODEOWNERS", "sed -i 's/a/b/' .github/workflows/ci.yml",
                  "bash -c 'echo x > .claude/hooks/guard.py'", "sh -c \"tee docs/factory/RISK.md\"",
                  "cd docs && bash -c 'cp /tmp/x factory/PROGRESS.md'",
                  # A variable set on the command line is not the session's.
                  "FACTORY_CONFIG=1 sed -i 's/a/b/' AGENTS.md"):
            with self.subTest(c):
                self.assertRefused(self.bash(c))
        self.assertAsks(self.bash("sed -i 's/a/b/' AGENTS.md", factory_config=True))

    def test_shell_c_runs_the_same_checks(self):
        self.assertAsks(self.bash("bash -c 'rm deploy/upgrade.sh'"))
        self.assertRefused(self.bash("sh -c 'git push --force'"))
        self.assertAllowed(self.bash("bash -c 'echo hi > out.txt'"))

    def test_shell_read_of_protected_path_is_allowed(self):
        for c in ("cat .github/workflows/ci.yml", "sed -n 1,5p prompts/review.md",
                  "cp .golangci.yml /tmp/x", "grep -r x internal/runner"):
            with self.subTest(c):
                self.assertAllowed(self.bash(c))

    def test_force_push_is_refused(self):
        for c in ("git push --force", "git push -f origin main", "git push -uf origin x",
                  "git push --force-with-lease", "git push origin +main", "git -C . push --force"):
            with self.subTest(c):
                self.assertRefused(self.bash(c))

    def test_plain_push_is_allowed(self):
        self.assertAllowed(self.bash("git push -u origin my-branch"))

    def test_no_verify_is_refused(self):
        for c in ("git commit --no-verify -m x", "git commit -nm x", "git push --no-verify"):
            with self.subTest(c):
                self.assertRefused(self.bash(c))

    def test_quoted_flag_text_is_not_a_flag(self):
        self.assertAllowed(self.bash("git commit -m 'explain --no-verify and -f'"))

    def test_rm_rf_outside_workspace_is_refused(self):
        for c in ("rm -rf /", "rm -rf ~", "rm -rf ..", "rm -fr /etc", "rm -r -f $HOME/x",
                  "rm -rf .", "cd / && rm -rf home", "rm --recursive --force /var"):
            with self.subTest(c):
                self.assertRefused(self.bash(c))

    def test_rm_rf_inside_workspace_is_allowed(self):
        for c in ("rm -rf web/node_modules", "rm -rf build/out", f"rm -rf {self.tmp.name}/scratch",
                  "rm -rf /tmp/x"):
            with self.subTest(c):
                self.assertAllowed(self.bash(c))

    def test_commit_is_refused_when_verify_fails(self):
        self.verify(1)
        r = self.bash("git add . && git commit -m 'x'")
        self.assertRefused(r)
        self.assertIn("verify ran", r.stderr)

    def test_commit_is_allowed_when_verify_passes(self):
        self.assertAllowed(self.bash("git commit -m 'x'"))
        self.assertAllowed(self.bash("cd . && git add -A && git commit -m 'x'"))

    def test_commit_after_a_change_in_the_same_command_is_refused(self):
        for c in ("echo x > a.go && git commit -am x", "gofmt -w . && git add -A && git commit -m x"):
            with self.subTest(c):
                r = self.bash(c)
                self.assertRefused(r)
                self.assertIn("its own command", r.stderr)

    def test_commit_after_a_change_is_allowed_in_a_repo_without_verify(self):
        os.remove(os.path.join(self.repo, "scripts", "verify.sh"))
        self.assertAllowed(self.bash("echo x > a.go && git commit -am x"))

    def test_post_edit_formats_go(self):
        p = os.path.join(self.repo, "x.go")
        with open(p, "w") as f:
            f.write("package x\nfunc  F( ) {}\n")
        self.assertAllowed(self.run_guard("post-edit", {"file_path": p}))
        with open(p) as f:
            self.assertEqual(f.read(), "package x\n\nfunc F() {}\n")

    def test_post_edit_reports_unparsable_go(self):
        p = os.path.join(self.repo, "x.go")
        with open(p, "w") as f:
            f.write("package x\nfunc {\n")
        r = self.run_guard("post-edit", {"file_path": p})
        self.assertEqual(r.returncode, 2)
        self.assertIn("gofmt could not format", r.stderr)


if __name__ == "__main__":
    unittest.main()
