#!/usr/bin/env python3
"""Verify the repository invariants that are checkable without building anything.

- Every relative Markdown link resolves.
- Every spec and plan records its lifecycle in a **Status:** field on line 3,
  drawn from a closed vocabulary, and a finished plan records what shipped it
  in an **Outcome:** field on line 4
  (.agents/rules/900-design-and-review-loops.md).
- go.mod declares the module language version the project map commits to
  (.agents/rules/100-project-map.md).
- Only internal/k8s imports k8s.io/client-go outside tests and test/
  (.agents/rules/800-security-invariant.md).
- k8s.io/client-go, k8s.io/api, and k8s.io/apimachinery share one minor version.
- Only internal/natskv imports github.com/nats-io/nats.go outside tests and test/.
- Only internal/auth imports github.com/go-jose/go-jose and golang.org/x/crypto outside tests and test/.
- Only cmd/profgate imports golang.org/x/term, which reads a password without echo.
- internal/client imports no Kubernetes package, no NATS package, and none of
  internal/k8s, internal/natskv, or internal/auth; only cmd/profgate imports it.
- The removed discovery.pprof.allowedPorts and allowedPortNames keys, their Go
  fields, and their environment variables appear in no code or manifest;
  internal/config refuses them by name and keeps the fixtures that prove it.

Checks whose subject does not exist yet stay silent rather than failing.
The golden ClusterRole test (.agents/rules/800-security-invariant.md) lives in
deploy/ as a Go test rather than here.

Run via: mise run check

Uses only the Python standard library, against the system interpreter that
mise does not manage.
"""

import glob
import os
import re
import sys
from pathlib import Path

SPEC_STATUS = ("Draft", "Accepted", "Superseded")
PLAN_STATUS = ("Draft", "Approved", "In Progress", "Done", "Abandoned", "Superseded")

STATUS_RE = re.compile(r"^\*\*Status:\*\* (.+?)\s*$")
OUTCOME_RE = re.compile(r"^\*\*Outcome:\*\* \S")
LINK_RE = re.compile(r"\]\(([^)]+)\)")
GO_DIRECTIVE_RE = re.compile(r"^go\s+(\S+)\s*$", re.MULTILINE)
GO_LANGUAGE_VERSION = "1.26.0"
EXTERNAL = ("http://", "https://", "mailto:")


def markdown_files():
    seen = set()
    for pattern in ("*.md", "docs/**/*.md", ".agents/**/*.md"):
        for path in glob.glob(pattern, recursive=True):
            if path not in seen:
                seen.add(path)
                yield path


def read_lines(path):
    with open(path, encoding="utf-8") as handle:
        return handle.read().splitlines()


def check_links(errors):
    for path in sorted(markdown_files()):
        base = os.path.dirname(path)
        with open(path, encoding="utf-8") as handle:
            body = handle.read()
        for match in LINK_RE.finditer(body):
            target = match.group(1).split("#")[0]
            if not target or target.startswith(EXTERNAL):
                continue
            if not os.path.exists(os.path.normpath(os.path.join(base, target))):
                errors.append(f"{path}: link target does not exist: {target}")


def check_status(errors):
    groups = (("docs/specs/*.md", SPEC_STATUS, False), ("docs/plans/*.md", PLAN_STATUS, True))
    for pattern, allowed, is_plan in groups:
        for path in sorted(glob.glob(pattern)):
            if os.path.basename(path) == "README.md":
                continue
            lines = read_lines(path)
            if len(lines) < 3:
                errors.append(f"{path}: too short to carry a Status field")
                continue
            match = STATUS_RE.match(lines[2])
            if not match:
                errors.append(f"{path}:3: line 3 must read '**Status:** <value>', found: {lines[2]!r}")
                continue
            value = match.group(1)
            if value not in allowed:
                errors.append(f"{path}:3: Status {value!r} is not one of {', '.join(allowed)}")
                continue
            if is_plan and value == "Done":
                line4 = lines[3] if len(lines) > 3 else ""
                if not OUTCOME_RE.match(line4):
                    errors.append(f"{path}:4: a Done plan records '**Outcome:** <commit or tag>' on line 4")


def check_go_directive(errors):
    """Hold go.mod to the minimum version the project map commits to.

    `go mod init` and `go mod tidy` both write the running toolchain's version,
    which is deliberately newer, so this drifts on the first careless command.
    """
    if not os.path.exists("go.mod"):
        return
    with open("go.mod", encoding="utf-8") as handle:
        body = handle.read()
    match = GO_DIRECTIVE_RE.search(body)
    if not match:
        errors.append("go.mod: no 'go' directive found")
    elif match.group(1) != GO_LANGUAGE_VERSION:
        errors.append(
            f"go.mod: go directive is {match.group(1)}, expected {GO_LANGUAGE_VERSION} "
            "(see .agents/rules/100-project-map.md)"
        )


def check_clientgo_importers(root):
    bad = []
    for path in root.rglob("*.go"):
        rel = path.relative_to(root).as_posix()
        if rel.endswith("_test.go") or rel.startswith("test/") or rel.startswith("internal/k8s/"):
            continue
        if '"k8s.io/client-go' in path.read_text():
            bad.append(f"{rel}: imports k8s.io/client-go outside internal/k8s")
    return bad


def check_k8s_minor_alignment(root):
    gomod = (root / "go.mod").read_text() if (root / "go.mod").exists() else ""
    minors = {}
    for mod in ("k8s.io/client-go", "k8s.io/api", "k8s.io/apimachinery"):
        m = re.search(rf"^\s*{re.escape(mod)} v0\.(\d+)\.", gomod, re.M)
        if m:
            minors[mod] = m.group(1)
    if len(set(minors.values())) > 1:
        return [f"go.mod: Kubernetes modules on different minors: {minors}"]
    return []


def check_nats_importers(root):
    bad = []
    for path in root.rglob("*.go"):
        rel = path.relative_to(root).as_posix()
        if rel.endswith("_test.go") or rel.startswith("test/") or rel.startswith("internal/natskv/"):
            continue
        if '"github.com/nats-io/nats.go' in path.read_text():
            bad.append(f"{rel}: imports github.com/nats-io/nats.go outside internal/natskv")
    return bad


def check_auth_importers(root):
    bad = []
    for path in root.rglob("*.go"):
        rel = path.relative_to(root).as_posix()
        if rel.endswith("_test.go") or rel.startswith("test/") or rel.startswith("internal/auth/"):
            continue
        text = path.read_text()
        for mod in ('"github.com/go-jose/go-jose', '"golang.org/x/crypto'):
            if mod in text:
                bad.append(f"{rel}: imports {mod[1:]} outside internal/auth")
    return bad


def check_term_importers(root):
    bad = []
    for path in root.rglob("*.go"):
        rel = path.relative_to(root).as_posix()
        if rel.startswith("cmd/profgate/"):
            continue
        if '"golang.org/x/term' in path.read_text():
            bad.append(f"{rel}: imports golang.org/x/term outside cmd/profgate")
    return bad


CLIENT_FORBIDDEN_IMPORTS = (
    '"k8s.io/',
    '"github.com/nats-io/nats.go',
    '"github.com/arloliu/profgate/internal/k8s',
    '"github.com/arloliu/profgate/internal/natskv',
    '"github.com/arloliu/profgate/internal/auth',
)


def check_client_imports(root):
    """Keep internal/client's dependency set an argument about one package.

    The command-line client reaches neither cluster dependency and verifies no signature,
    so it imports no Kubernetes package, no NATS package, and none of internal/k8s, internal/natskv, or internal/auth;
    and only cmd/profgate imports it,
    so the gateway's binary size and dependency set stay an argument about internal/client rather than about the whole tree.
    """
    bad = []
    for path in root.rglob("*.go"):
        rel = path.relative_to(root).as_posix()
        text = path.read_text()
        if rel.startswith("internal/client/"):
            for mod in CLIENT_FORBIDDEN_IMPORTS:
                if mod in text:
                    bad.append(f"{rel}: imports {mod[1:]} inside internal/client")
        elif not rel.startswith("cmd/profgate/") and '"github.com/arloliu/profgate/internal/client' in text:
            bad.append(f"{rel}: imports internal/client outside cmd/profgate")
    return bad


REMOVED_PORT_KEYS = (
    "AllowedPorts",
    "AllowedPortNames",
    "allowedPorts",
    "allowedPortNames",
    "PROFGATE_PPROF_ALLOWED_PORTS",
    "PROFGATE_PPROF_ALLOWED_PORT_NAMES",
)


def check_removed_port_keys(root):
    """Fail on any code or manifest line naming the two removed port allowlists.

    discovery.pprof.allowedSelections replaced them, and a manifest still
    carrying one would stop the gateway at startup rather than fail a build.
    internal/config holds the refusal and its fixtures; CHANGELOG.md and
    docs/configuration.md name the old keys on purpose and are never read.
    """
    bad = []
    paths = [p for pattern in ("*.go", "*.yaml", "*.yml", "*.tpl") for p in root.rglob(pattern)]
    paths.append(root / "deploy/chart/profgate/README.md")
    for path in sorted(paths):
        rel = path.relative_to(root).as_posix()
        if rel.startswith("internal/config/") or not path.is_file():
            continue
        for number, line in enumerate(path.read_text().splitlines(), 1):
            hit = next((key for key in REMOVED_PORT_KEYS if key in line), None)
            if hit:
                bad.append(f"{rel}:{number}: names the removed {hit}; use discovery.pprof.allowedSelections")
    return bad


PKCE_OVERRIDE_NAME = "PROFGATE_E2E_PKCE_VERIFIER_OVERRIDE"
PKCE_OVERRIDE_ALLOWED = (
    "internal/client/pkce_override_e2e.go",
    "internal/client/pkce_override_test.go",
)


def check_pkce_override_name(root):
    """Fail on any code or manifest line naming the PKCE verifier override.

    The end-to-end lanes prove PKCE enforcement by polling with a verifier that does not match the challenge,
    and the substitution lives in the client binary behind the e2e build tag, read from that variable.
    The override must never reach a manifest, the chart, or an untagged Go file:
    only the e2e-tagged file, the test proving the default build ignores the variable, and the end-to-end scenarios may name it.
    """
    bad = []
    paths = [p for pattern in ("*.go", "*.yaml", "*.yml", "*.tpl") for p in root.rglob(pattern)]
    for path in sorted(paths):
        rel = path.relative_to(root).as_posix()
        if not path.is_file() or rel in PKCE_OVERRIDE_ALLOWED:
            continue
        if rel.startswith("test/e2e/") and rel.endswith("_test.go") and "/" not in rel[len("test/e2e/"):]:
            continue
        for number, line in enumerate(path.read_text().splitlines(), 1):
            if PKCE_OVERRIDE_NAME in line:
                bad.append(f"{rel}:{number}: names {PKCE_OVERRIDE_NAME} outside the e2e-tagged client file, its test, and test/e2e")
    return bad


def check_hooks(root):
    """Fail when a repository git hook is missing or not executable.

    Rule 500 tells every clone to point core.hooksPath at .githooks; a hook
    that git cannot execute is skipped without a word, which reads as a
    commit that passed the check.
    """
    bad = []
    for name in ("pre-commit", "commit-msg"):
        path = root / ".githooks" / name
        if not path.is_file():
            bad.append(f".githooks/{name}: missing")
        elif not os.access(path, os.X_OK):
            bad.append(f".githooks/{name}: not executable (chmod +x)")
    return bad


def main():
    errors = []
    check_links(errors)
    check_status(errors)
    check_go_directive(errors)
    root = Path(".")
    errors.extend(check_clientgo_importers(root))
    errors.extend(check_k8s_minor_alignment(root))
    errors.extend(check_nats_importers(root))
    errors.extend(check_auth_importers(root))
    errors.extend(check_term_importers(root))
    errors.extend(check_client_imports(root))
    errors.extend(check_removed_port_keys(root))
    errors.extend(check_pkce_override_name(root))
    errors.extend(check_hooks(root))
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        print(f"\n{len(errors)} problem(s) found", file=sys.stderr)
        return 1
    print("repository invariants hold")
    return 0


if __name__ == "__main__":
    sys.exit(main())
