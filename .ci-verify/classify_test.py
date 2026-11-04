#!/usr/bin/env python3
"""Test harness for the ci.yml changes/classify step.

Extracts the real 'Classify changed paths' run-script from
.github/workflows/ci.yml, substitutes the ${{ ... }} expressions with
per-scenario values, runs it in a temp git repo, and asserts on the
GITHUB_OUTPUT concern flags.
"""
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent  # worktree root
CI = REPO / ".github" / "workflows" / "ci.yml"


def extract_script():
    doc = yaml.safe_load(CI.read_text())
    for step in doc["jobs"]["changes"]["steps"]:
        if step.get("name") == "Classify changed paths":
            return step["run"]
    raise SystemExit("classify step not found")


SCRIPT = extract_script()
EXPR = re.compile(r"\$\{\{ *([^}]+) *\}\}")


def sub_expressions(script, event):
    return EXPR.sub(lambda m: event.get(m.group(1).strip(), ""), script)


def sh(args, cwd=None, env=None):
    return subprocess.run(args, cwd=cwd, capture_output=True, text=True, timeout=60, env=env)


def setup_repo(files):
    d = Path(tempfile.mkdtemp(prefix="classify-"))
    sh(["git", "init", "-q", "-b", "main", str(d)])
    sh(["git", "config", "user.email", "t@t"], cwd=d)
    sh(["git", "config", "user.name", "t"], cwd=d)
    for rel in files:
        p = d / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text("x\n")
    sh(["git", "add", "-A"], cwd=d)
    sh(["git", "commit", "-qm", "base"], cwd=d)
    return d, sh(["git", "rev-parse", "HEAD"], cwd=d).stdout.strip()


def run_scenario(name, event, paths=None, extra=None, expect=None,
                 empty_base=False, corrupt_base=False, tag_ref=False):
    files = [".env.example", "main.go", "main_test.go", "go.mod",
             "Dockerfile", "renovate.json5", "LICENSE", "README.md",
             "CHANGELOG.md", ".github/workflows/ci.yml"] + (extra or [])
    d, base = setup_repo(files)
    head = base
    if paths:
        for rel in paths:
            p = d / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("changed\n")
        sh(["git", "add", "-A"], cwd=d)
        sh(["git", "commit", "-qm", "change"], cwd=d)
        head = sh(["git", "rev-parse", "HEAD"], cwd=d).stdout.strip()

    ev = {
        "github.event_name": event,
        "github.event.pull_request.base.sha": base,
        "github.event.pull_request.head.sha": head,
        "github.event.before": base,
        "github.sha": head,
        "github.ref": "refs/tags/v9.9.9-test" if tag_ref else "refs/heads/main",
    }
    if corrupt_base:
        ev["github.event.before"] = "deadbeef" * 5
    elif empty_base:
        ev["github.event.before"] = ""

    out_file = d / "out.txt"
    script = sub_expressions(SCRIPT, ev)
    # The classifier reads its inputs from step env, not ${{ }} substitution;
    # mirror the ci.yml `env:` block so the extracted script runs the real
    # env-driven code path.
    run_env = dict(os.environ)
    run_env.update({
        "GITHUB_OUTPUT": str(out_file),
        "EVENT_NAME": ev["github.event_name"],
        "EVENT_REF": ev["github.ref"],
        "PR_BASE_SHA": ev["github.event.pull_request.base.sha"],
        "PR_HEAD_SHA": ev["github.event.pull_request.head.sha"],
        "EVENT_BEFORE": ev["github.event.before"],
        "HEAD_SHA": ev["github.sha"],
    })
    try:
        r = sh(["bash", "-c", script], cwd=d, env=run_env)
    except subprocess.TimeoutExpired:
        print(f"FAIL  {name}: script timed out")
        return False
    flags = {}
    if out_file.exists():
        for line in out_file.read_text().splitlines():
            if "=" in line:
                k, v = line.split("=", 1)
                flags[k] = v
    ok = (expect == flags) if expect else True
    if ok and r.returncode != 0:
        ok = False
    status = "PASS" if ok else "FAIL"
    detail = ""
    if expect != flags:
        detail += f" expected={expect} got={flags}"
    if r.returncode != 0:
        detail += f" rc={r.returncode} stderr={r.stderr[:200]!r}"
    print(f"{status}  {name}: {flags}{detail}")
    return ok


ALL = {"workflow": "true", "lint": "true", "build": "true", "test": "true", "docker": "true"}
# code changes: lint/build/test/docker on, workflow untouched
CODE = {"workflow": "false", "lint": "true", "build": "true", "test": "true", "docker": "true"}
NONE = {"workflow": "false", "lint": "false", "build": "false", "test": "false", "docker": "false"}
WF = {"workflow": "true", "lint": "false", "build": "false", "test": "false", "docker": "false"}
TESTS = {"workflow": "false", "lint": "true", "build": "false", "test": "true", "docker": "false"}
WEB = {"workflow": "false", "lint": "false", "build": "true", "test": "false", "docker": "true"}
DOCKER = {"workflow": "false", "lint": "false", "build": "false", "test": "false", "docker": "true"}

SCENARIOS = [
    # --- docs / inert ---
    ("README-only (md) is inert", dict(event="pull_request", paths=["README.md"]), NONE),
    ("CHANGELOG.md is inert", dict(event="pull_request", paths=["CHANGELOG.md"]), NONE),
    ("docs/ nested md is inert", dict(event="pull_request", paths=["docs/api.md"]), NONE),
    ("docs/ non-md is inert", dict(event="pull_request", paths=["docs/diagram.png"]), NONE),
    ("renovate.json5 is inert", dict(event="pull_request", paths=["renovate.json5"]), NONE),
    ("pre-commit config is inert", dict(event="pull_request", paths=[".pre-commit-config.yaml"]), NONE),
    ("LICENSE is inert", dict(event="pull_request", paths=["LICENSE"]), NONE),
    ("inert combo md+renovate", dict(event="pull_request", paths=["README.md", "renovate.json5", "docs/x.md"]), NONE),
    # --- workflow ---
    ("ci.yml change", dict(event="pull_request", paths=[".github/workflows/ci.yml"]), WF),
    ("two workflow ymls", dict(event="pull_request", paths=[".github/workflows/release.yml", ".github/workflows/pr-cleanup.yml"]), WF),
    ("workflow yaml variant", dict(event="pull_request", paths=[".github/workflows/thing.yaml"]), WF),
    ("workflow + main.go combo", dict(event="pull_request", paths=[".github/workflows/ci.yml", "main.go"]), ALL),
    ("workflow md is inert", dict(event="pull_request", paths=[".github/workflows/README.md"]), NONE),
    ("renovate + ci.yml combo", dict(event="pull_request", paths=["renovate.json5", ".github/workflows/ci.yml"]), WF),
    # --- go sources ---
    ("main.go change", dict(event="pull_request", paths=["main.go"]), CODE),
    ("main_test.go change", dict(event="pull_request", paths=["main_test.go"]), TESTS),
    ("go.mod change", dict(event="pull_request", paths=["go.mod"]), CODE),
    ("go.sum change", dict(event="pull_request", paths=["go.sum"]), CODE),
    # --- docker ---
    ("Dockerfile change", dict(event="pull_request", paths=["Dockerfile"]), DOCKER),
    ("dockerignore change", dict(event="pull_request", paths=[".dockerignore"]), DOCKER),
    # --- web arms (dormant in current repo; future-proofing) ---
    ("web/embed.go change", dict(event="pull_request", paths=["web/embed.go"], extra=["web/embed.go"]), CODE),
    ("web/embed_stub.go change", dict(event="pull_request", paths=["web/embed_stub.go"], extra=["web/embed_stub.go"]), CODE),
    ("web/src file change", dict(event="pull_request", paths=["web/src/lib/x.ts"], extra=["web/src/lib/x.ts"]), WEB),
    ("web/package.json change", dict(event="pull_request", paths=["web/package.json"], extra=["web/package.json"]), WEB),
    ("web/tsconfig.json change", dict(event="pull_request", paths=["web/tsconfig.json"], extra=["web/tsconfig.json"]), WEB),
    # --- unknown paths: conservative ---
    ("unknown .svelte-kit file", dict(event="pull_request", paths=[".svelte-kit/generated/x.ts"]), CODE),
    ("unknown root config", dict(event="pull_request", paths=["Makefile"]), CODE),
    # --- inert catch-all arm (markdown under dotdir) ---
    (".impeccable/notes.md is inert", dict(event="pull_request", paths=[".impeccable/notes.md"]), NONE),
    # --- multi-path priority ---
    ("test + go.mod: build+docker from go.mod", dict(event="pull_request", paths=["main_test.go", "go.mod"]), CODE),
    # --- push / dispatch / tag edges ---
    ("push normal", dict(event="push", paths=["main.go"]), CODE),
    ("push docs-only", dict(event="push", paths=["README.md"]), NONE),
    ("push first-branch empty before", dict(event="push", paths=["main.go"], empty_base=True), ALL),
    ("push corrupt before", dict(event="push", paths=["main.go"], corrupt_base=True), ALL),
    ("push no-op (before==head)", dict(event="push"), ALL),
    ("tag push", dict(event="push", paths=["README.md"], tag_ref=True), ALL),
    ("workflow_dispatch", dict(event="workflow_dispatch"), ALL),
    # --- PR edges ---
    ("PR empty diff (no changes)", dict(event="pull_request"), ALL),
]

failed = 0
for name, d, expect in SCENARIOS:
    if not run_scenario(name, d["event"], d.get("paths"), d.get("extra"),
                        expect=expect, empty_base=d.get("empty_base", False),
                        corrupt_base=d.get("corrupt_base", False),
                        tag_ref=d.get("tag_ref", False)):
        failed += 1

print(f"\n{len(SCENARIOS) - failed}/{len(SCENARIOS)} passed")
sys.exit(1 if failed else 0)
