# 900 - Design, Plans, and Review Loops

Apply when drafting or revising a design, writing an implementation plan,
executing one, or working through a review round.

## The Status Contract

Specs and plans each live at one stable path while they are living documents.
Their lifecycle is a `Status:` field in the document, never a directory —
moving a file to record a state change breaks every link that cites it and
truncates its history for anyone who forgets `git log --follow`.
A document that reaches a terminal state is deleted rather than moved:
a move breaks those same links and leaves the finished text in every search,
while a deletion breaks them and takes the text out of the tree,
with a lookup that reads it back.
[Deleting a Finished Document](#deleting-a-finished-document) gives the protocol.

Line 3 of every `docs/specs/*.md` and `docs/plans/*.md` is exactly:

```
**Status:** <value>
```

Specs use `Draft`, `Accepted`, or `Superseded`.
Plans use `Draft`, `Approved`, `In Progress`, `Done`, `Abandoned`, or
`Superseded`.
Nothing else — a closed vocabulary is what lets a check verify the field
mechanically.
`mise run check` verifies it, together with the `Outcome:` rule below and
every relative link in the repository's Markdown.
Run it after editing a spec or a plan.
No CI invokes it yet, so the check is currently as unforced as the field it
guards — treat that as a gap to close, not a reason to skip it.
The two mechanisms in [800](800-security-invariant.md) join the same task once
Go code exists.

What each value licenses: [000](000-agent-contract.md#document-authority).

### Finishing a Plan

**The same change that lands a plan's last task flips its `Status:` to `Done`
and adds an `Outcome:` line naming the commit or tag that shipped it.**

`Outcome:` occupies line 4, directly below `Status:`, so a check can find it
in the same place every time.

This is the rule the whole contract rests on.
A `Status:` field that nothing forces rots: a plan reading `In Progress`
months after its work shipped will send an arriving agent to seek approval for
something already in production, and the agent has no way to tell.
Binding the flip to the completing change turns staleness into a missing diff
hunk that review catches, rather than a memory failure nobody notices.

The same applies to a plan that dies: `Abandoned` plus a line saying what
replaced it, in the change that walks away from it.

### Deleting a Finished Document

**A plan that reaches `Done` or `Abandoned`,
and a spec that reaches `Superseded`,
are then deleted from the tree, in two commits.**

The first commit is the change described above, with the document still in place:
it flips `Status:` and adds the line that says where the work went —
`Outcome:` naming the commit or tag for a `Done` plan,
the replacement for an `Abandoned` one,
and for a `Superseded` spec the specs that replace it.
`check_status` validates that tree exactly as it always has.
The second commit is the next commit that touches the file:
it deletes the document and rewrites every link that cited it,
which `check_links` enforces.
One commit cannot do both,
because the tree a commit writes either holds the finished document or does not.

`scripts/check-repo.py` inspects the files present in the tree,
so a deleted plan is no longer checked at all.
The first commit is where the evidence lives:
its tree holds the finished `Status:` and the line below it under the checks,
and its diff shows both.
No check reads history, and this protocol adds none.

Nothing enforces the line an `Abandoned` plan carries,
or the one a `Superseded` spec carries:
`check_status` requires `Outcome:` on line 4 of a `Done` plan and nothing more.
Review is what holds those two cases.

Why deletion rather than an archive directory,
and how to read a deleted document back out of git:
[`finished-documents-leave-the-tree.md`](../../docs/decisions/finished-documents-leave-the-tree.md).

### Naming

Name a document for what it is about.
Living documents — specs, plans — carry no date prefix; a date makes an
evolving document look like a snapshot of the day it was born.
Decision records are named by topic, not numbered:
`docs/decisions/replica-sampling.md`.
An investigation frozen as of the day it ran is the one case where a date
belongs in the filename.

## Before Drafting

1. **State the invariant, not the symptom.**
   Not "PGO merge produced a bad profile" but "profiles from different build
   identities must never merge into one artifact."
   The invariant tells you which paths have to maintain it.
2. **Grep, don't claim.**
   Every "X is the only caller of Y" is a claim you run before you write it,
   in production code and tests both.
3. **Enumerate paths, then design.**
   List every path that observes the state and every path that mutates it,
   identify where two operations must coordinate, then pick the smallest
   mechanism that holds across all of them.
   Designing the mechanism first produces patches on patches.

## During Design

4. **Name the atomicity primitive before writing code.**
   Profgate coordinates across gateway replicas through NATS KV revisions and
   compare-and-swap, with no leader and no sticky sessions.
   Distributed state has no locks: the shape is usually "commit conditionally,
   then let the loser observe it lost," not "prevent the race."
   Signs of bolting it on afterwards: a snapshot taken for reassurance, a
   redundant pre-check, a check that cannot actually observe the other
   replica's current state.
5. **A tightly-coupled issue is not deferrable.**
   If a "later" issue shares a key, lease, revision counter, or path with the
   change in front of you, pull it in or show the separation that prevents
   cross-effects.
6. **Test plans compile against current source.**
   The reproducer must be writable against code that exists today.
   A missing clock seam, fake, or harness is part of the plan, not something
   discovered during implementation.

## During Review Loops

7. **"Approve with changes" means required.**
   Every listed change is a required edit, not a suggestion.
8. **Patching past two or three rounds means the design is wrong.**
   Reset to step 1.
   Signals: a second coordination mechanism for the same state, test timing
   that keeps getting more precise, a finding about the interaction between
   two earlier fixes.
9. **A scope shift moves the design.**
   When the goal expands mid-loop, re-examine every previously deferred item
   against the new goal.
10. **The reviewer sees what you wrote, not what you meant.**
    A "refuted" finding usually means the text was ambiguous.
    Tighten it and cite `file:line` for load-bearing claims.
