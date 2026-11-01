"""Consumer-visible gate behavior for scripts/check_workflow_lint.py.

Each test builds a throwaway repo root and runs the REAL pinned actionlint
(no mocked diagnostics): the exact validated `queue: max` node is the only
finding actionlint v1.7.12 can emit for it, so exit 0 plus the explicit
compat line is the only acceptable pass. This repo has no
.github/actionlint.yml, so the no-config path (plain actionlint, default
shellcheck behavior) is the primary shape; one case keeps the config-present
invocation working. Bad queue values, a cancelled release run, a duplicate
key, and a mixed real error must all fail with the real diagnostic retained.
"""
import io
import os
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

import check_workflow_lint

CHECKER = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_workflow_lint.py")

CONFIG = "self-hosted-runner:\n  labels:\n    - fast-runners\n"
BODY = "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi"
HEADER = "name: Fixture\non:\n  workflow_dispatch: {}\nconcurrency:"


def run_checker(repo_root):
    out, err = io.StringIO(), io.StringIO()
    with redirect_stdout(out), redirect_stderr(err):
        code = check_workflow_lint.main(["--repo-root", str(repo_root)])
    return code, out.getvalue(), err.getvalue()


def make_case(root, body, config=None):
    directory = os.path.join(root, ".github")
    os.makedirs(os.path.join(directory, "workflows"))
    if config is not None:
        with open(os.path.join(directory, "actionlint.yml"), "w", encoding="utf-8") as fh:
            fh.write(config)
    with open(os.path.join(directory, "workflows", "release.yml"), "w", encoding="utf-8") as fh:
        fh.write(body)


class WorkflowLintGate(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="llm-router-workflow-lint-")
        self.root = os.path.join(self.temp.name)
        subprocess.run(["git", "init", "-q", self.root], check=True)

    def tearDown(self):
        self.temp.cleanup()

    def write(self, body, config=None):
        make_case(self.root, body, config)

    def test_exact_validated_queue_node_is_the_only_pass_without_config(self):
        body = (
            HEADER
            + "\n  group: release\n  cancel-in-progress: false\n  queue: max\n"
            + BODY
        )
        self.write(body)
        code, out, err = run_checker(self.root)
        self.assertEqual(code, 0)
        self.assertIn("compat:", out)
        self.assertNotIn("workflow-lint:", out)
        self.assertEqual(err, "")

    def test_exact_validated_queue_node_passes_with_config(self):
        # Config-present path: the original exact invocation
        # (-config-file + -shellcheck "") must stay equivalent.
        body = (
            HEADER
            + "\n  group: release\n  cancel-in-progress: false\n  queue: max\n"
            "jobs:\n  a:\n    runs-on: fast-runners\n    steps:\n      - run: echo hi"
        )
        self.write(body, config=CONFIG)
        code, out, err = run_checker(self.root)
        self.assertEqual(code, 0)
        self.assertIn("compat:", out)
        self.assertEqual(err, "")

    def test_bad_queue_value_fails_with_real_diagnostic(self):
        body = (
            HEADER
            + "\n  group: release\n  cancel-in-progress: false\n  queue: aggressive\n"
            + BODY
        )
        self.write(body)
        code, out, err = run_checker(self.root)
        self.assertNotEqual(code, 0)
        self.assertNotIn("compat:", out)
        self.assertIn('unexpected key "queue"', err)

    def test_cancel_in_progress_true_fails_validation(self):
        body = (
            HEADER
            + "\n  group: release\n  cancel-in-progress: true\n  queue: max\n" + BODY
        )
        self.write(body)
        code, out, err = run_checker(self.root)
        self.assertNotEqual(code, 0)
        self.assertNotIn("compat:", out)
        self.assertIn('unexpected key "queue"', err)

    def test_duplicate_queue_key_fails_not_ignored(self):
        body = (
            HEADER
            + "\n  group: release\n  cancel-in-progress: false\n  queue: max\n  queue: max\n"
            + BODY
        )
        self.write(body)
        code, out, err = run_checker(self.root)
        self.assertNotEqual(code, 0)
        self.assertNotIn("compat:", out)
        self.assertIn('unexpected key "queue"', err)
        self.assertIn("key \"queue\" is duplicated", err)

    def test_real_error_is_kept_alongside_the_known_finding(self):
        body = (
            HEADER
            + "\n  group: release\n  cancel-in-progress: false\n  queue: max\n"
            "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n"
            "      - uses: no-such-action@v1\n"
        )
        self.write(body)
        code, out, err = run_checker(self.root)
        self.assertNotEqual(code, 0)
        self.assertIn("compat:", out)
        self.assertIn("specifying action", err)

    def test_expression_queue_is_never_accepted(self):
        # An expression value cannot be proven to be the literal max, so
        # the known finding must stay fatal even though actionlint's
        # message is identical to the valid case.
        body = (
            HEADER
            + "\n  group: release\n  cancel-in-progress: false\n"
            "  queue: ${{ github.ref }}\n"
            + BODY
        )
        self.write(body)
        code, out, err = run_checker(self.root)
        self.assertNotEqual(code, 0)
        self.assertNotIn("compat:", out)
        self.assertIn('unexpected key "queue"', err)


if __name__ == "__main__":
    unittest.main()
