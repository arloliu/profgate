# Documentation

Start with the guide for your task:

- **Pull a profile:** [`api.md`](api.md) — the HTTP API: routes, parameters, errors.
- **Deploy and operate the gateway:** [`deployment.md`](deployment.md).
- **Configure the gateway:** [`configuration.md`](configuration.md).
- **Authenticate users:** [`authentication.md`](authentication.md).
- **Enable PGO collection:** [`pgo.md`](pgo.md).
- **Use the console:** [`console.md`](console.md).

These guides live at the top of `docs/` and carry no `Status:` field;
they age by being kept true against the code.

The rest of `docs/` serves contributors,
organized by how each document ages,
because what decides whether a document can still be trusted is its lifecycle, not its topic.

| Path | Holds | How it ages |
|---|---|---|
| [`specs/`](specs/) | What the system is and why it is shaped that way | Living; a `Status:` field says whether it is settled, and a spec that reaches `Superseded` is deleted |
| `plans/` | Executable work orders | Living while `Approved` or `In Progress`; deleted in the change after `Done` or `Abandoned` is recorded, with git history as the record |
| `decisions/` | One decision that is expensive to revisit, per file | Immutable once accepted; superseded rather than edited |
| `investigations/` | An investigation frozen as of the day it ran | Immutable; superseded by a newer investigation or by a spec |

`plans/` holds [`plans/roadmap.md`](plans/roadmap.md), which orders the work that comes next.

Working notes stay in `tmp/`, which is not tracked.
They are byproducts of producing the documents above,
not documents in their own right;
an investigation that a committed document cites lives under `investigations/` instead.

## The Status Field

Specs and plans each live at one stable path, and record their lifecycle in a
`Status:` field on line 3 rather than by moving between directories.

- **Specs:** `Draft` | `Accepted` | `Superseded`
- **Plans:** `Draft` | `Approved` | `In Progress` | `Done` | `Abandoned` | `Superseded`

A spec is the design of record only at `Accepted`.
A plan may be executed only at `Approved` or `In Progress`.

The change that lands a plan's final task also flips its `Status:` to `Done`
and records an `Outcome:` line naming what shipped;
the next change that touches the plan deletes it —
[`decisions/finished-documents-leave-the-tree.md`](decisions/finished-documents-leave-the-tree.md)
gives the reasons and says how to read a deleted document back out of git.
`mise run check` verifies both fields and every relative link here — see
[`.agents/rules/900-design-and-review-loops.md`](../.agents/rules/900-design-and-review-loops.md).

## Where Contributors Start

- **Understanding what Profgate is:** [`specs/gateway.md`](specs/gateway.md),
  the accepted gateway design.
  [`specs/pgo.md`](specs/pgo.md) is the accepted PGO collection design layered on it,
  [`specs/auth.md`](specs/auth.md) the accepted authentication design,
  [`specs/ui.md`](specs/ui.md) the accepted console design,
  and [`specs/cli.md`](specs/cli.md) the accepted command-line client design.
- **Why a choice was made:** [`decisions/`](decisions/), one file per topic.
- **Changing anything:** [`.agents/rules/`](../.agents/rules/) holds the rules in force,
  indexed by [`.agents/rules/AGENTS.md`](../.agents/rules/AGENTS.md).
