#!/usr/bin/env python3
"""Actionlint gate with one bounded compatibility for the pinned release.

CI pins actionlint v1.7.12, which predates the workflow `concurrency.queue`
key (upstream rhysd/actionlint issue #657, PR #654 — unreleased). This
checker re-runs that exact pin, then consumes actionlint's machine-readable
diagnostics:

The repo's `.github/actionlint.yml` is OPTIONAL. When it is present the
exact config and its shellcheck input are passed through unchanged; when
it is absent actionlint runs plain, with no `-config-file` and no
`-shellcheck` override (default shellcheck behavior). Both paths keep the
identical pin and machine-readable output:

  * every diagnostic fails the build, exactly as a raw actionlint run;
  * the single `unexpected key "queue"` finding at the top-level
    `concurrency.queue` node of .github/workflows/release.yml is accepted
    only after that node is strictly validated: exactly the keys `group`,
    `cancel-in-progress`, and `queue`; no duplicates, expressions,
    aliases, unknown shapes, or bad types; literal `queue: max` with
    `cancel-in-progress: false` and a non-empty literal `group`; and the
    finding's file/line/column point at that `queue` key.

The validation uses a bounded parser that understands only that canonical
mapping shape; anything it cannot prove fails closed. There is no
whole-file, per-pattern, or per-message ignore: when the known finding is
the only one, the output is one explicit compatibility line and exit 0;
any real finding keeps its full diagnostic and a non-zero exit.

Standard library only, so it runs on the CI runner's Python (3.8) without
adding a dependency.
"""
import argparse
import json
import os
import re
import subprocess
import sys

# The pin must stay identical to the CI workflow-lint invocation; the
# compatibility bound exists only for the one finding below, not as an
# alternate linter configuration. The config file is optional: when it
# exists it (and its shellcheck input) are passed through unchanged; when
# it is absent actionlint runs without them, using default shellcheck
# behavior.
ACTIONLINT = "github.com/rhysd/actionlint/cmd/actionlint@v1.7.12"
CONFIG_RELATIVE = os.path.join(".github", "actionlint.yml")
WORKFLOW_RELATIVE = os.path.join(".github", "workflows", "release.yml")
KNOWN_MESSAGE = (
    'unexpected key "queue" for "concurrency" section. '
    'expected one of "cancel-in-progress", "group"'
)
COMPAT_NOTE = (
    "compat: actionlint v1.7.12 predates the workflow concurrency `queue` key "
    "(upstream rhysd/actionlint#657, unreleased PR #654); accepted the known "
    "false positive at {file}:{line} after full-node validation "
    "(`queue: max`, `cancel-in-progress: false`, literal `group`)."
)


class ConcurrencyShape(Exception):
    """The concurrency node is not the exact validated shape; fail closed."""


def _scalar(raw):
    """Return the value of one bare scalar token, else None.

    Rejects everything except a plain token or a fully quoted string:
    expressions, aliases, anchors, flow collections, comments, and tabs.
    """
    if not raw:
        return None
    if "\t" in raw:
        return None
    head = raw[0]
    if head in "$#{}[]&*!@`":
        return None
    if head == '"' or head == "'":
        if len(raw) < 2 or raw[-1] != head:
            return None
        return raw[1:-1]
    return raw


def validated_concurrency(text):
    """Strictly validate the top-level concurrency node.

    Bounded to exactly this canonical shape:

        concurrency:
          key: scalar
          ...

    Two-space-style consistent indentation, one plain `key: scalar` mapping
    per line, no nesting, no comments as values, no duplicate keys. Returns
    the 1-based line and column of the `queue` key, or raises
    ConcurrencyShape when the node is absent or not exactly validatable.
    """
    lines = text.split("\n")
    start = None
    for index, line in enumerate(lines):
        if line == "concurrency:":
            start = index
            break
    if start is None:
        raise ConcurrencyShape("no top-level concurrency node")

    values = {}
    queue_position = None
    indent = None
    index = start + 1
    while index < len(lines):
        line = lines[index]
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            index += 1
            continue
        if not line[0].isspace():
            break  # next top-level key ends the node
        prefix = line[: len(line) - len(line.lstrip(" "))]
        if "\t" in prefix:
            raise ConcurrencyShape("tab indentation in concurrency node")
        match = re.match(r"([A-Za-z][A-Za-z0-9-]*):(?: (.*))?$", stripped)
        if match is None:
            raise ConcurrencyShape("concurrency entry is not a plain `key: scalar` line")
        current = len(prefix)
        if indent is None:
            indent = current
        elif current != indent:
            raise ConcurrencyShape("nested or misindented entry in concurrency node")
        key = match.group(1)
        if key in values:
            raise ConcurrencyShape("duplicate key %r in concurrency node" % key)
        value = _scalar(match.group(2) or "")
        if value is None:
            raise ConcurrencyShape("non-scalar value for concurrency key %r" % key)
        values[key] = value
        if key == "queue":
            queue_position = (index + 1, current + 1)
        index += 1

    if set(values) != {"group", "cancel-in-progress", "queue"}:
        raise ConcurrencyShape(
            "concurrency keys are %r, expected exactly group, "
            "cancel-in-progress, queue" % sorted(values)
        )
    if values["queue"] != "max":
        raise ConcurrencyShape("concurrency.queue is %r, expected literal max" % values["queue"])
    if values["cancel-in-progress"] != "false":
        raise ConcurrencyShape(
            "concurrency.cancel-in-progress is %r, expected literal false"
            % values["cancel-in-progress"]
        )
    if not values["group"]:
        raise ConcurrencyShape("concurrency.group is empty")
    return {"line": queue_position[0], "column": queue_position[1]}


def known_queue_finding(diagnostic, repo_root):
    """Return the validated queue position when this finding is the known
    false positive, else None (the caller keeps it fatal)."""
    if diagnostic.get("kind") != "syntax-check":
        return None
    if diagnostic.get("message") != KNOWN_MESSAGE:
        return None
    if os.path.normpath(diagnostic.get("filepath", "")) != WORKFLOW_RELATIVE:
        return None
    path = os.path.join(repo_root, WORKFLOW_RELATIVE)
    if not os.path.isfile(path):
        return None
    with open(path, "r", encoding="utf-8-sig") as source:
        text = source.read()
    try:
        node = validated_concurrency(text)
    except (ConcurrencyShape, UnicodeDecodeError, OSError):
        return None
    if node["line"] != diagnostic.get("line") or node["column"] != diagnostic.get("column"):
        return None
    return node


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", default=os.getcwd())
    args = parser.parse_args(argv)
    repo_root = os.path.abspath(args.repo_root)
    command = ["go", "run", ACTIONLINT]
    if os.path.isfile(os.path.join(repo_root, CONFIG_RELATIVE)):
        # Repo config present: keep the original exact invocation.
        # Without it, actionlint runs plain with default shellcheck
        # behavior (no -config-file, no -shellcheck).
        command += ["-config-file", CONFIG_RELATIVE, "-shellcheck", ""]
    command += ["-format", "{{json .}}"]
    proc = subprocess.run(
        command,
        cwd=repo_root,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        universal_newlines=True,
    )
    if proc.returncode not in (0, 1):
        sys.stderr.write(
            "workflow-lint: actionlint did not complete a lint (exit %d)\n" % proc.returncode
        )
        for stream in (proc.stderr, proc.stdout):
            if stream.strip():
                sys.stderr.write(stream.rstrip() + "\n")
        return 1

    diagnostics = []
    if proc.stdout.strip():
        try:
            parsed = json.loads(proc.stdout)
        except ValueError:
            sys.stderr.write(
                "workflow-lint: actionlint emitted non-JSON diagnostics; refusing to continue\n"
                + proc.stdout.rstrip()
                + "\n"
            )
            return 1
        if not isinstance(parsed, list) or any(
            not isinstance(d, dict)
            or not isinstance(d.get("message"), str)
            or not isinstance(d.get("line"), int)
            for d in parsed
        ):
            sys.stderr.write(
                "workflow-lint: actionlint emitted unparseable diagnostics; refusing to continue\n"
                + proc.stdout.rstrip()
                + "\n"
            )
            return 1
        diagnostics = parsed
    if proc.returncode == 1 and not diagnostics:
        sys.stderr.write(
            "workflow-lint: actionlint reported findings without parseable diagnostics\n"
        )
        if proc.stderr.strip():
            sys.stderr.write(proc.stderr.rstrip() + "\n")
        return 1

    retained = []
    for diagnostic in diagnostics:
        node = known_queue_finding(diagnostic, repo_root)
        if node is not None:
            sys.stdout.write(COMPAT_NOTE.format(file=WORKFLOW_RELATIVE, line=node["line"]) + "\n")
        else:
            retained.append(diagnostic)
    for diagnostic in retained:
        sys.stderr.write(
            "workflow-lint: %s:%s: %s\n"
            % (diagnostic.get("filepath"), diagnostic.get("line"), diagnostic.get("message"))
        )
    return 1 if retained else 0


if __name__ == "__main__":
    sys.exit(main())
