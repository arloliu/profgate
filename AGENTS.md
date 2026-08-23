# Profgate Agent Configuration

Authoritative entrypoint for coding agents in this repository.
`CLAUDE.md` points here; other agents read `AGENTS.md` directly.
If instructions here conflict with a default behavior, this file and the rules
it references win — only an explicit user instruction overrides them.

Profgate (module `github.com/arloliu/profgate`) is a standalone
Kubernetes-aware pprof gateway for Go workloads: one HTTP entry point that
resolves a Kubernetes Service to its backend Pods, proxies pprof profiles, and
collects representative CPU profiles for Profile-Guided Optimization.
Kubernetes 1.23 is the compatibility baseline.
NATS JetStream KV holds control-plane state; profile bytes stay ephemeral.

## This Repository Has No Code Yet

`mise.toml` pins the toolchain and defines `mise run check`, which verifies
the repository invariants that need no build.
There is no Go source, no `go.mod`, and no linter configuration.
Every structural claim in
[`100-project-map.md`](.agents/rules/100-project-map.md) describes **planned**
layout drawn from the design draft, not what `ls` shows.
Confirm any path, package, or command against reality before relying on it.

## The Design Is a Draft

[`docs/specs/profgate-design.md`](docs/specs/profgate-design.md) carries
`Status: Draft`.
Its direction has not been settled.
Read it for context, cite it in discussion, and revise it freely — it is not
an implementation source.
A spec becomes the design of record when it reaches `Status: Accepted`.
Full authority order:
[`000-agent-contract.md`](.agents/rules/000-agent-contract.md#document-authority).

## The Permission Invariant

> Profgate requires no Kubernetes write permissions.
> It observes Services, Pods, and EndpointSlices in authorized namespaces,
> connects to explicitly permitted application pprof ports, and manipulates
> only its dedicated `PROFGATE_*` NATS stores.

This boundary is the product.
It erodes through convenience, not malice: one extra client-go call, one extra
verb in the ClusterRole, each reasonable on its own.
Widening it is a design decision that gets argued in a spec, and
[`800-security-invariant.md`](.agents/rules/800-security-invariant.md) holds
the mechanisms that make the boundary checkable rather than aspirational.

## No Jargon, Anywhere

State facts plainly — in agent rules, code comments, commit messages, PR
descriptions, everywhere.
A future reader has no memory of this conversation, this plan, or this review
round; wording that made sense mid-task misleads once that context is gone.

Write the current fact, not the process that produced it.
Off-limits everywhere: sequencing labels (`PR-1`, `Phase 4`), finding or
open-question numbers, review-round references (`round 2`, `v3 findings`),
work-item and session IDs, `tmp/*` paths, and "resolved `<date>`" provenance
trails.
Citing a committed file *path* is always fine; citing its internal section
numbers or question numbering is not.
Commit and PR specifics:
[`600-git-conventions.md`](.agents/rules/600-git-conventions.md).

## Rules

Read [`.agents/rules/AGENTS.md`](.agents/rules/AGENTS.md) first — it maps task
triggers to the rule files that apply, so you load only what the work needs.

[`000-agent-contract.md`](.agents/rules/000-agent-contract.md) is always in
force, including its core rule: **do not guess when source, tests, docs,
`git`, or `grep` can answer.**

Documentation layout and how each kind of document ages:
[`docs/README.md`](docs/README.md).
