# Profgate — Agent Rules Index

Trigger map for repository rules.
Read `000-agent-contract.md` for every task, then the files whose triggers
match the work.
If in doubt, read the file rather than guess its contents.

Five rules exist because five things are true today.
Coding standards, testing, and validation rules arrive when `go.mod`, the test
framework, and `.golangci.yml` land.
`mise.toml` pins Go and golangci-lint, but the linter has no configuration and
there is nothing yet to lint.

## Default Load

- Discussing or revising the design: `000`, `900`.
- Writing or executing an implementation plan: `000`, `100`, `900`.
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
