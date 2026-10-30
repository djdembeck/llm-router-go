#!/usr/bin/env python3
"""Reserve exact-source release identities and publish native llm-router-go outputs."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


class ReleaseError(Exception):
    pass


VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
OID = re.compile(r"[0-9a-f]{40}\Z")
REPOSITORY = re.compile(r"(?:git@github\.com:|https://github\.com/)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?\Z")


def semver(value):
    match = VERSION.fullmatch(value)
    if not match:
        raise ReleaseError(f"Invalid release version: {value!r}; expected X.Y.Z")
    return tuple(map(int, match.groups()))


def require(condition, message):
    if not condition:
        raise ReleaseError(message)


def isolated_git_environment():
    return {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}


class Repository:
    def __init__(self, root="."):
        self.root = Path(root).resolve()

    @staticmethod
    def proc(args, cwd):
        return subprocess.run(args, cwd=cwd, env=isolated_git_environment(), text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE)

    def run(self, *args, ok=(0,)):
        result = self.proc(args, self.root)
        require(result.returncode in ok, f"{' '.join(args)}: {result.stderr.strip() or result.stdout.strip()}")
        return result.stdout.strip()

    def git(self, *args):
        return self.run("git", *args)

    def repository(self):
        match = REPOSITORY.fullmatch(self.git("remote", "get-url", "origin"))
        push = REPOSITORY.fullmatch(self.git("remote", "get-url", "--push", "origin"))
        require(match and push and match.group(1).lower() == push.group(1).lower(),
                "origin fetch/push must identify the same GitHub repository")
        return match.group(1)

    def api(self, endpoint, method="GET", fields=None):
        args = ["gh", "api", "--hostname", "github.com", endpoint]
        if method != "GET":
            args += ["--method", method]
        for key, value in (fields or {}).items():
            args += ["-f", f"{key}={value}"]
        value = self.run(*args)
        return json.loads(value) if value else None

    def oid(self, ref):
        require(isinstance(ref, str) and not ref.startswith("-"), "Invalid git reference")
        value = self.git("rev-parse", "--verify", f"{ref}^{{commit}}")
        require(OID.fullmatch(value), "Expected a full commit object ID")
        return value

    def exact_oid(self, value):
        require(isinstance(value, str) and OID.fullmatch(value), "Expected a full commit object ID")
        require(self.oid(value) == value, "Commit object ID did not resolve exactly")
        return value

    def ancestor(self, older, newer):
        result = self.proc(["git", "merge-base", "--is-ancestor", older, newer], self.root)
        require(result.returncode in (0, 1), result.stderr.strip() or "Cannot inspect ancestry")
        return result.returncode == 0

    def fetch(self):
        self.repository()
        self.git("fetch", "--prune", "--no-prune-tags", "--no-tags", "origin",
                 "+refs/heads/main:refs/remotes/origin/main",
                 "+refs/heads/develop:refs/remotes/origin/develop",
                 "+refs/tags/*:refs/llm-router-go-release/tags/*")

    def tags(self, ref):
        tags = []
        for tag in self.git("for-each-ref", f"--merged={ref}", "--format=%(refname:strip=3)",
                            "refs/llm-router-go-release/tags/v*").splitlines():
            if VERSION.fullmatch(tag[1:]):
                tags.append((semver(tag[1:]), tag))
        return sorted(tags)

    def tag_target(self, tag):
        ref = f"refs/llm-router-go-release/tags/{tag}"
        result = self.proc(["git", "show-ref", "--verify", "--quiet", ref], self.root)
        require(result.returncode in (0, 1), "Cannot inspect release tag")
        return self.oid(ref) if result.returncode == 0 else None

    def api_tag_target(self, tag):
        """Resolve a remote tag to its commit, peeling annotated tag objects."""
        repository = self.repository()
        try:
            target = self.api(f"repos/{repository}/git/ref/tags/{tag}")["object"]
        except ReleaseError as error:
            require("HTTP 404" in str(error), str(error))
            return None
        for _ in range(10):
            if target["type"] != "tag":
                break
            target = self.api(f"repos/{repository}/git/tags/{target['sha']}")["object"]
        require(target["type"] == "commit", f"Tag {tag} does not resolve to a commit")
        return target["sha"]

    def select(self, sha, version=None, bump="auto", automatic=False):
        sha = self.exact_oid(sha)
        main = self.oid("origin/main")
        require(self.ancestor(sha, main), "Publication source must belong to main history")
        tags = self.tags(sha)
        exact = [tag for _, tag in tags if self.tag_target(tag) == sha]
        require(len(exact) <= 1, "Ambiguous exact-source release reservations")
        if version:
            version = "v" + version.removeprefix("v")
            semver(version[1:])
            require(not exact or exact == [version], "Source already reserved as a different version")
            target = self.tag_target(version)
            require(target is None or target == sha, "Version is reserved by another source")
            if target:
                return version
        elif exact:
            return exact[0]
        if automatic:
            for _, tag in self.tags(main):
                target = self.tag_target(tag)
                if target != sha and self.ancestor(sha, target):
                    return None  # Superseded automatic candidate; no reservation yet.
        baseline, previous = tags[-1] if tags else ((0, 0, 0), None)
        if version:
            require(semver(version[1:]) > baseline, "New version must exceed the reachable reservation baseline")
            return version
        if not previous:
            return "v0.1.0"
        if bump == "auto":
            messages = self.git("log", "--format=%B", f"refs/llm-router-go-release/tags/{previous}..{sha}")
            if re.search(r"(?m)^BREAKING[ -]CHANGE:|^[a-zA-Z][\w-]*(?:\([^\n)]*\))?!:", messages):
                bump = "major"
            elif re.search(r"(?m)^feat(?:\([^\n)]*\))?:", messages):
                bump = "minor"
            else:
                bump = "patch"
        major, minor, patch = baseline
        require(bump in ("major", "minor", "patch"), "Invalid bump level")
        parts = {"major": (major + 1, 0, 0), "minor": (major, minor + 1, 0), "patch": (major, minor, patch + 1)}[bump]
        selected = "v" + ".".join(map(str, parts))
        require(self.tag_target(selected) in (None, sha), "Version is reserved by another source")
        return selected

    def reserve(self, sha, version=None, bump="auto", automatic=False):
        self.fetch()
        tag = self.select(sha, version, bump, automatic)
        if tag is None:
            return None
        repository = self.repository()
        try:
            self.api(f"repos/{repository}/git/refs", "POST", {"ref": f"refs/tags/{tag}", "sha": sha})
        except ReleaseError:
            # Create-only REST ref writes cannot move a concurrent reservation.
            target = self.api(f"repos/{repository}/git/ref/tags/{tag}")["object"]
            while target["type"] == "tag":
                target = self.api(f"repos/{repository}/git/tags/{target['sha']}")["object"]
            require(target["type"] == "commit" and target["sha"] == sha,
                    "Concurrent reservation points at a different source")
        return tag

    def release(self, tag, source, publish=False):
        semver(tag.removeprefix("v"))
        require(isinstance(source, str) and OID.fullmatch(source), "Expected a full commit object ID")
        repository = self.repository()
        require(self.api_tag_target(tag) == source, f"Tag {tag} does not point at the frozen source")
        try:
            existing = self.api(f"repos/{repository}/releases/tags/{tag}")
        except ReleaseError as error:
            require("HTTP 404" in str(error), str(error))
            require(not publish, "Cannot finalize a missing draft release")
            self.run("gh", "release", "create", tag, "--repo", repository, "--verify-tag", "--draft", "--title", f"Release {tag}", "--notes", "")
            return
        require(existing["tag_name"] == tag, "Release identity mismatch")
        if publish and existing.get("draft"):
            self.run("gh", "release", "edit", tag, "--repo", repository, "--draft=false", "--latest=false")


DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def registry_run(*args):
    return subprocess.run(args, check=True, capture_output=True).stdout


def inspect(ref, field="Manifest", missing=False):
    try:
        return json.loads(registry_run("docker", "buildx", "imagetools", "inspect", ref,
                              "--format", "{{json ." + field + "}}"))
    except subprocess.CalledProcessError as error:
        message = error.stderr.decode(errors="replace").lower()
        # Only an explicit registry manifest-not-found response permits creation.
        # Authentication, transport, throttling and malformed responses fail closed.
        absent = "manifest unknown" in message or f"{ref.lower()}: not found" in message
        if missing and absent and not any(word in message for word in
                                         ("unauthorized", "denied", "timeout", "forbidden")):
            return None
        raise


def digest(value):
    if not isinstance(value, str) or not DIGEST.fullmatch(value):
        raise ValueError(f"Invalid registry digest: {value!r}")
    return value


def image(image_name, ref, sha, version, arch, missing=False):
    manifest = inspect(ref, missing=missing)
    if manifest is None:
        return None
    if "manifests" in manifest:
        raise ValueError(f"Expected one native image at {ref}")
    value = digest(manifest["digest"])
    config = inspect(f"{image_name}@{value}", "Image")
    labels = config.get("config", {}).get("Labels", {})
    if (config.get("os"), config.get("architecture"),
        labels.get("org.opencontainers.image.revision"),
        labels.get("org.opencontainers.image.version")) != ("linux", arch, sha, version):
        raise ValueError(f"Image provenance mismatch at {ref}")
    return value


def publish_image(repository, source, version):
    require(OID.fullmatch(source), "Image source must be a full SHA")
    semver(version.removeprefix("v"))
    image_name = "ghcr.io/" + repository.lower()
    staging = f"{image_name}:sha-{source}-amd64"
    reserved = image(image_name, f"{image_name}:{version}", source, version, "amd64", missing=True)
    existing = image(image_name, staging, source, version, "amd64", missing=True)
    if existing is None and reserved is not None:
        existing = reserved
    if existing is None:
        registry_run("docker", "buildx", "build", "--platform", "linux/amd64", "--provenance=false", "--sbom=false", "--build-arg", f"VERSION={version}", "--build-arg", f"GIT_COMMIT={source}", "--tag", staging, "--push", ".")
        existing = image(image_name, staging, source, version, "amd64")
    require(reserved is None or reserved == existing, "Existing version image has conflicting bytes")
    if reserved is None:
        registry_run("docker", "buildx", "imagetools", "create", "--prefer-index=false", "--tag", f"{image_name}:{version}", f"{image_name}@{existing}")
    require(image(image_name, f"{image_name}:{version}", source, version, "amd64") == existing, "Version image read-back differs")
    print(f"Published immutable image: {image_name}@{existing}")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    reserve = commands.add_parser("reserve")
    reserve.add_argument("--sha", required=True)
    reserve.add_argument("--version")
    reserve.add_argument("--bump", choices=("auto", "patch", "minor", "major"), default="auto")
    reserve.add_argument("--automatic", action="store_true")
    image_command = commands.add_parser("image")
    image_command.add_argument("--source", required=True)
    image_command.add_argument("--version", required=True)
    finish = commands.add_parser("release")
    finish.add_argument("--tag", required=True)
    finish.add_argument("--source", required=True)
    finish.add_argument("--publish", action="store_true")
    args = parser.parse_args(argv)
    repo = Repository()
    try:
        repository = repo.repository()
        if args.command == "reserve":
            print(repo.reserve(args.sha, args.version, args.bump, args.automatic) or "")
        elif args.command == "release":
            repo.release(args.tag, args.source, args.publish)
        elif args.command == "image":
            publish_image(repository, args.source, args.version)
        return 0
    except (ReleaseError, OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        print(f"release: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
