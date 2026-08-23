# Profgate — Agent Rules Index

Trigger map for repository rules.
Read `000-agent-contract.md` for every task, then the files whose triggers
match the work.
If in doubt, read the file rather than guess its contents.

## Default Load

- Discussing or revising the design: `000`, `900`.
- Writing or executing an implementation plan: `000`, `100`, `900`.
- Writing Go code or tests: add `200`, `300`, `500`.
- Anything touching Kubernetes access, NATS access, RBAC, or authentication:
  add `800`.
- Commits, branches, PRs: add `600`.

## Always

- **[000-agent-contract.md](000-agent-contract.md)** — don't guess, keep
  changes small, surface conflicts, fail loud, and the authority order that
  decides which documents an agent may act on.

## Before Code Changes

- **[100-project-map.md](100-project-map.md)** — identity, module path,
  planned package layout, and the single Kubernetes seam that carries both the
  1.23 compatibility baseline and the permission boundary.

- **[200-coding-standards.md](200-coding-standards.md)** —
  formatting, `log/slog`, error wrapping, no `init()`, context first, and the only package-level mutable state allowed.

## Before Writing or Running Tests

- **[300-testing.md](300-testing.md)** —
  the unit and end-to-end layers, `-race`, table tests, `httptest`, the fake clientset, and a fresh fixture per subtest.

## Before Every Commit or PR

- **[500-validation-and-workflow.md](500-validation-and-workflow.md)** —
  the validation block, `semlf check` on prose, and when the end-to-end suite must run before a PR.

## For Kubernetes Access, NATS Access, RBAC, or Auth

- **[800-security-invariant.md](800-security-invariant.md)** — the permission
  boundary, the two mechanisms that keep it checkable, and what a request to
  widen it must contain.

## Before Crafting Commits or PRs

- **[600-git-conventions.md](600-git-conventions.md)** — branch naming,
  Conventional Commits, jargon and attribution prohibitions.

## Before Design, Plan, or Review Work

- **[900-design-and-review-loops.md](900-design-and-review-loops.md)** — spec
  and plan lifecycle, the `Status:` contract, how a plan is finished, and
  review-loop discipline.
