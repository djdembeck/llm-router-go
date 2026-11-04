#!/usr/bin/env python3
"""Truth-table test for the ci.yml job-level circuit breakers and inline gates.

Extracts the real `if:` expressions from the YAML (job-level, the inline Gate
step, and the expensive real steps) and evaluates them with a small mock
GitHub-expression evaluator for every relevant combination of
(changes.result, flags, upstream results, gate.enabled).

The required jobs (lint-format, build, test) must never be job-level-skipped:
their job-level `if` is just `!cancelled()`, so they always RUN. The inline
Gate step (its own `if` is also `!cancelled()`) is where the real gating
happens: it fails the job on a failed/malformed classification or a failed
upstream, emits `enabled=true/false`, and the expensive steps only run when
`enabled == 'true'` plus their required upstream successes. `gate_step()`
below models that run script so the step `if:` and the downstream `enabled`
conditions can be exercised together with the real YAML expressions.
"""
import ast
import operator
import re
import sys
import yaml
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
doc = yaml.safe_load((REPO / ".github/workflows/ci.yml").read_text())
J = doc["jobs"]

T = "true"
F = "false"
S = "success"
SK = "skipped"
FL = "failure"
FLAGS = ("workflow", "lint", "build", "test", "docker")
JOB_RESULTS = ("workflow-lint", "lint-format", "build", "test")


def step_if(job, name):
    return next(s["if"] for s in J[job]["steps"] if s.get("name") == name)


EXPRS = {
    "workflow-lint.job": J["workflow-lint"]["if"],
    "lint-format.gate": step_if("lint-format", "Gate"),
    "lint-format.real": step_if("lint-format", "Check formatting"),
    "build.job": J["build"]["if"],
    "build.gate": step_if("build", "Gate"),
    "build.real": step_if("build", "Build Go (stub)"),
    "test.job": J["test"]["if"],
    "test.gate": step_if("test", "Gate"),
    "test.real": step_if("test", "Run tests"),
    "docker.job": J["docker-build"]["if"],
}

BINOPS = {
    ast.Eq: operator.eq,
    ast.NotEq: operator.ne,
    ast.And: lambda a, b: bool(a) and bool(b),
    ast.Or: lambda a, b: bool(a) or bool(b),
    ast.Not: lambda a: not a,
}


def evaluate(expr, changes_result, flags, results, gate_enabled=False):
    """Evaluate a real ci.yml `if:` expression.

    Substitutes the raw values BEFORE AST parsing: needs results and
    classifier outputs as strings, and steps.gate.outputs.enabled as
    `gate_enabled` (already "true"/"false"), so comparisons like
    `== 'true'` are compared against literal strings.
    """
    e = expr.strip()
    if e.startswith("${{") and e.endswith("}}"):
        e = e[3:-2]
    e = e.strip()
    e = e.replace("needs['changes'].result", f"'{changes_result}'")
    for f in FLAGS:
        e = e.replace(f"needs['changes'].outputs.{f}", f"'{flags[f]}'")
    for job in JOB_RESULTS:
        e = e.replace(f"needs['{job}'].result", f"'{results.get(job, SK)}'")
    e = e.replace("steps.gate.outputs.enabled", f"'{gate_enabled}'")
    e = e.replace("!cancelled()", "True")
    e = e.replace("cancelled()", "False")
    # Operators: two-char first so '!' of '!=' is never touched.
    e = e.replace("!=", " !=")
    e = e.replace("==", " ==")
    e = e.replace(" && ", " and ")
    e = e.replace(" || ", " or ")
    e = re.sub(r"!\s*\(", " not (", e)
    e = re.sub(r"!(?=\s)", " not ", e)
    node = ast.parse(e, mode="eval").body
    return _ev(node)


def _ev(node):
    if isinstance(node, ast.Constant):
        return node.value
    if isinstance(node, ast.Compare):
        op = BINOPS[type(node.ops[0])]
        left = _ev(node.left)
        right = _ev(node.comparators[0])
        return op(left, right)
    if isinstance(node, ast.BoolOp):
        vals = [_ev(v) for v in node.values]
        return all(bool(v) for v in vals) if isinstance(node.op, ast.And) else any(bool(v) for v in vals)
    if isinstance(node, ast.UnaryOp):
        return BINOPS[type(node.op)](_ev(node.operand))
    raise ValueError(f"unsupported node: {ast.dump(node)}")


def gate_step(changes_result, flags, own, predecessors):
    """Model the inline Gate run script for a required job.

    Reproduces, in order: classifier-failure trip, failed-upstream trip,
    malformed/missing flag trip, then emits `enabled = (own flag == true)`.
    Returns ('failed', None) on any failure/corruption (the step exits and
    emits no `enabled`), else ('success', enabled_bool). `predecessors` maps
    upstream job name -> result; anything but `failure` is tolerated there
    (success and skipped both pass).
    """
    if changes_result != S:
        return (FL, None)
    for r in predecessors.values():
        if r == FL:
            return (FL, None)
    for f in FLAGS:
        if flags.get(f) not in (T, F):
            return (FL, None)
    return (S, flags[own] == T)


failed = 0


def check(name, got, want):
    global failed
    ok = got == want
    if not ok:
        failed += 1
    print(f"{'PASS' if ok else 'FAIL'}  {name}: got={got} want={want}")


FULL = dict(workflow=T, lint=T, build=T, test=T, docker=T)
NONE = dict(workflow=F, lint=F, build=F, test=F, docker=F)
MISSING_LINT = {f: FULL[f] for f in FLAGS if f != "lint"}

# --- Gate step model (pure helper) ---
check("gate fail on classifier failure", gate_step(FL, FULL, "lint", {}), (FL, None))
check("gate fail on malformed flag", gate_step(S, dict(FULL, lint="maybe"), "lint", {}), (FL, None))
check("gate fail on missing flag", gate_step(S, MISSING_LINT, "lint", {}), (FL, None))
check("gate fail on failed upstream", gate_step(S, FULL, "build", {"lint-format": FL, "workflow-lint": S}), (FL, None))
check("gate passes with upstream skipped", gate_step(S, FULL, "build", {"lint-format": S, "workflow-lint": SK}), (S, True))
check("gate own false => success+disabled", gate_step(S, NONE, "lint", {}), (S, False))
check("gate own true => success+enabled", gate_step(S, FULL, "lint", {}), (S, True))

# --- lint-format (required): job & gate always run; real gated on enabled ---
check("lint gate step runs (if !cancelled())", evaluate(EXPRS["lint-format.gate"], S, NONE, {}, "true"), True)
check("lint real off when gate disabled", evaluate(EXPRS["lint-format.real"], S, NONE, {}, "false"), False)
check("lint real on when gate enabled", evaluate(EXPRS["lint-format.real"], S, FULL, {}, "true"), True)
check("lint real off when classifier failed (gate failed, no enabled)", evaluate(EXPRS["lint-format.real"], FL, FULL, {}, "false"), False)

# --- build (required): job runs regardless of upstream; real needs lint-format ---
check("build job not skipped by upstream failure (if !cancelled())", evaluate(EXPRS["build.job"], S, FULL, {"workflow-lint": FL, "lint-format": FL}), True)
check("build gate step runs (if !cancelled())", evaluate(EXPRS["build.gate"], S, NONE, {}, "true"), True)
check("build real off when gate disabled", evaluate(EXPRS["build.real"], S, NONE, {}, "false"), False)
check("build real off when lint-format not success", evaluate(EXPRS["build.real"], S, FULL, {"lint-format": FL, "workflow-lint": S}, "true"), False)
check("build real on when enabled + lint-format success", evaluate(EXPRS["build.real"], S, dict(workflow=F, lint=F, build=T, test=F, docker=F), {"lint-format": S, "workflow-lint": SK}, "true"), True)

# --- test (required): job runs regardless of upstream; real needs lint+build ---
check("test job not skipped by build failure (if !cancelled())", evaluate(EXPRS["test.job"], S, FULL, {"lint-format": S, "build": FL}), True)
check("test gate step runs (if !cancelled())", evaluate(EXPRS["test.gate"], S, NONE, {}, "true"), True)
check("test real off when gate disabled", evaluate(EXPRS["test.real"], S, NONE, {}, "false"), False)
check("test real off when build not success", evaluate(EXPRS["test.real"], S, FULL, {"lint-format": S, "build": FL}, "true"), False)
check("test real off when lint-format not success", evaluate(EXPRS["test.real"], S, FULL, {"lint-format": FL, "build": S}, "true"), False)
check("test real on when enabled + lint-format & build success", evaluate(EXPRS["test.real"], S, dict(workflow=F, lint=F, build=F, test=T, docker=F), {"lint-format": S, "build": S}, "true"), True)

# --- workflow-lint (non-required): job-level gate, can skip freely ---
check("wf-lint on when workflow=true", evaluate(EXPRS["workflow-lint.job"], S, dict(workflow=T, lint=F, build=F, test=F, docker=F), {}, "false"), True)
check("wf-lint skipped when workflow=false", evaluate(EXPRS["workflow-lint.job"], S, NONE, {}, "false"), False)

# --- docker-build (non-required): needs ALL upstream success ---
check("docker runs on full-green", evaluate(EXPRS["docker.job"], S, FULL, {"workflow-lint": SK, "lint-format": S, "build": S, "test": S}, "false"), True)
check("docker runs on docker-only diff (upstreams no-op success)", evaluate(EXPRS["docker.job"], S, dict(workflow=F, lint=F, build=F, test=F, docker=T), {"workflow-lint": SK, "lint-format": S, "build": S, "test": S}, "false"), True)
check("docker skips when build failed", evaluate(EXPRS["docker.job"], S, FULL, {"workflow-lint": S, "lint-format": S, "build": FL, "test": S}, "false"), False)
check("docker skips when docker=false", evaluate(EXPRS["docker.job"], S, dict(workflow=F, lint=T, build=F, test=T, docker=F), {"workflow-lint": SK, "lint-format": S, "build": S, "test": S}, "false"), False)

print(f"\n{'ALL PASS' if failed == 0 else f'{failed} FAILURES'}")
sys.exit(1 if failed else 0)