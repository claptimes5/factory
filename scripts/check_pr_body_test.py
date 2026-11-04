#!/usr/bin/env python3
"""Tests for check-pr-body.py, run on descriptions as GitHub sends them."""

import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
CHECK = os.path.join(HERE, "check-pr-body.py")
TEMPLATE = os.path.join(HERE, "..", ".github", "pull_request_template.md")


def check(body):
    return subprocess.run([sys.executable, CHECK], input=body, capture_output=True, text=True)


class CheckPRBody(unittest.TestCase):
    def test_the_untouched_template_fails_on_all_three(self):
        r = check(open(TEMPLATE).read())
        self.assertEqual(r.returncode, 1)
        self.assertIn("Testing, Risk, How to revert", r.stderr)

    def test_an_empty_description_fails(self):
        self.assertEqual(check("").returncode, 1)

    def test_a_filled_template_passes(self):
        body = open(TEMPLATE).read()
        for h, text in (("## Testing", "scripts/verify.sh passed."), ("## Risk", "Only docs."),
                        ("## How to revert", "Revert this PR.")):
            body = body.replace(h, h + "\n\n" + text)
        r = check(body.replace("\n", "\r\n"))
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_one_empty_section_is_named(self):
        r = check("## Testing\n\nRan it.\n\n## Risk\n\n<!-- todo -->\n\n## How to revert\n\nRevert.\n")
        self.assertEqual(r.returncode, 1)
        self.assertIn("filled in: Risk.", r.stderr)

    def test_factorys_own_headings_pass(self):
        # Without a template, Factory titles its risk section "How to review".
        body = ("Closes #1.\n\n## Summary\n\nx\n\n## How to review\n\nLook at a.go.\n\n"
                "## Testing\n\ngo test passed.\n\n## How to revert\n\nRevert the merge.\n")
        r = check(body)
        self.assertEqual(r.returncode, 0, r.stderr)


if __name__ == "__main__":
    unittest.main()
