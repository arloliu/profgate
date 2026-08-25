# Documentation

Organized by how each document ages, because what decides whether a document
can still be trusted is its lifecycle, not its topic.

| Path | Holds | How it ages |
|---|---|---|
| [`specs/`](specs/) | What the system is and why it is shaped that way | Living; a `Status:` field says whether it is settled |
| `plans/` | Executable work orders | Living while `Approved` or `In Progress`; frozen history once `Done` |
| `decisions/` | One decision that is expensive to revisit, per file | Immutable once accepted; superseded rather than edited |

`plans/` does not exist yet; it is created along with its first document.

Review reports and working notes stay in `tmp/`, which is not tracked.
They are byproducts of producing the documents above, not documents in their
own right.

## The Status Field

Specs and plans each live at one stable path, and record their lifecycle in a
`Status:` field on line 3 rather than by moving between directories.

- **Specs:** `Draft` | `Accepted` | `Superseded`
- **Plans:** `Draft` | `Approved` | `In Progress` | `Done` | `Abandoned` | `Superseded`

A spec is the design of record only at `Accepted`.
A plan may be executed only at `Approved` or `In Progress`.

The change that lands a plan's final task also flips its `Status:` to `Done`
and records an `Outcome:` line naming what shipped.
`mise run check` verifies both fields and every relative link here — see
[`.agents/rules/900-design-and-review-loops.md`](../.agents/rules/900-design-and-review-loops.md).

## Where to Start

- **Understanding what Profgate is:** [`specs/gateway.md`](specs/gateway.md),
  the accepted gateway design.
  [`specs/pgo.md`](specs/pgo.md) is the accepted PGO collection design layered on it;
  [`specs/profgate-design.md`](specs/profgate-design.md) is the superseded combined original, kept for history.
- **Why a choice was made:** [`decisions/`](decisions/), one file per topic.
- **Changing anything:** [`.agents/rules/`](../.agents/rules/) holds the rules
  in force, indexed by [`.agents/rules/AGENTS.md`](../.agents/rules/AGENTS.md).
