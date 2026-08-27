# A finished plan and a superseded spec are deleted, not kept

**Decision:** a plan whose `Status:` becomes `Done` or `Abandoned`,
and a spec whose `Status:` becomes `Superseded`,
are deleted from the tree, in two commits.
The first commit records the status with the document still in place, exactly as
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) requires,
so history holds one commit in which the document stands finished under the repository's checks.
The next commit that touches the file deletes it and rewrites every link that cited it.
One commit cannot do both:
the tree it writes either holds the finished document or does not.

Recording this decision is prospective, in the manner of that first commit —
it states the policy and deletes nothing.
The six documents already in a terminal state,
the superseded combined design and the five `Done` plans,
leave in the change that applies it.
Git history is their record.

## Context

`docs/specs/profgate-design.md` carries `Status: Superseded` across 2,590 lines.
The five `Done` plans — gateway, PGO, client-selected port, authentication, console — carry 6,596 more.
Together those 9,186 lines outweigh the 6,368 lines of the four accepted specs,
so most of the design Markdown in the repository describes work that has shipped or been replaced.

The cost is paid on every read.
A grep over `docs/` returns the finished lines beside the living ones, ranked the same;
a superseded spec answers in the same voice as an accepted one,
and a plan describes an intended shape that the code has since moved past.
The `Status:` field separates them for a reader who checks line 3,
but a search hit carries no line 3,
and a contributor who opens `docs/specs/` in order meets the superseded combined design first.

Deleting costs nothing that git does not already keep.
`git log --all --diff-filter=D --format=%H -- docs/plans/<name>.md` names the commit that deleted the path;
`git show <that-commit>^:docs/plans/<name>.md`
prints the document as its last living commit held it —
`Status:` set, `Outcome:` naming what shipped.
The commit named on that `Outcome:` line remains the pointer from the plan to the code.

## Alternatives considered

- **Move them to an `archive/` directory.**
  Rejected: a move breaks every link that cites the file just as a deletion does,
  and truncates `git log` for anyone who omits `--follow` —
  `900-design-and-review-loops.md` gives those same two reasons for recording lifecycle in a field,
  rather than by moving a file between directories.
  The search noise survives the move, so the cost that motivates this decision is not paid down at all.
- **Keep them where they are, and rely on the `Status:` field.**
  Rejected: the field is read by a person who opens the file at line 3,
  and the reads that go wrong are the ones that never do that —
  a grep result, an editor's fuzzy file open, an agent gathering context by keyword.
  The volume is the problem, and a field cannot reduce volume.

## Consequences

- A link to a deleted document is removed, or rewritten to name the commit that last held it.
  `check_links` in [`scripts/check-repo.py`](../../scripts/check-repo.py) fails otherwise,
  which makes this a required edit rather than a remembered one.
  Three files link to `docs/specs/profgate-design.md` today:
  [`docs/README.md`](../README.md), [`docs/specs/gateway.md`](../specs/gateway.md),
  and [`docs/specs/pgo.md`](../specs/pgo.md).
- An `Outcome:` line loses its file.
  It is written and committed before the deletion, so it survives in history and in that commit's diff,
  and the change that removes the plan states in its commit message what shipped.
- The `Status:` and `Outcome:` checks in `scripts/check-repo.py` are unchanged,
  and they inspect only the files present under `docs/specs/` and `docs/plans/`.
  A deleted document is therefore no longer checked at all.
  The commit that recorded the status is where the evidence lives:
  its tree holds the finished document under those checks, and its diff shows the flip.
  No check reads history, and this decision adds none.
- `docs/README.md` no longer describes a finished plan as frozen history kept in the tree.
- [`CHANGELOG.md`](../../CHANGELOG.md) is unaffected and remains the user-facing record of what each release changed.
  Plans and specs were never that record.
- A reader who wants a deleted document runs
  `git log --all --diff-filter=D --format=%H -- docs/plans/<name>.md`
  to name the commit that deleted it, and then
  `git show <that-commit>^:docs/plans/<name>.md`.
  The `^` is required,
  because the path no longer exists in the tree that deletion commit wrote.
