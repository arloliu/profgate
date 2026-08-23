# 000 - Agent Contract

Always-on operating contract for this repo.
Supersedes habit; only an explicit user instruction overrides it.

## Document Authority

Rank sources by how much reality has already tested them:

```
source code  >  docs/specs/*.md (Status: Accepted)  >  docs/plans/*.md
```

Code is what runs.
An accepted spec is the design of record.
A plan is a work order, and a completed one is history.

- Execute a plan only while its `Status:` is `Approved` or `In Progress`.
  Every other value makes it history — read it for context, never as an
  instruction.
- A spec at `Status: Draft` is a proposal under discussion.
  Revise it, argue with it, cite it in design conversation.
  It is not an implementation source.
- When code and a document disagree, the code is the fact and the document is
  a bug. Say so instead of writing code that matches the document.

Lifecycle details and the full `Status:` vocabulary:
[900-design-and-review-loops.md](900-design-and-review-loops.md).

## Don't Guess

- **Do not guess when source, tests, docs, `git`, or `grep` can answer.**
- State assumptions explicitly. If uncertain, ask.
- Present multiple interpretations when ambiguity exists; name the one you
  picked and why, rather than silently choosing.
- Push back when a simpler approach exists.
- Stop when confused. Name what is unclear before proceeding.
- Say what is unverified and why, when verification is impossible or too
  expensive.
- This repository is small, which makes guessing cheap and wrong.
  A path, package, command, or Makefile target you have not confirmed with
  `ls`, `cat`, or `grep` does not exist.

## Keep Changes Small

- Make the minimum change that solves the problem.
- Touch only what you must; clean up only the orphans your own change created.
- Leave adjacent code, comments, and formatting as they are, and match
  existing style even where you would do it differently.
- Mention unrelated dead code you notice rather than deleting it.
- Test: every changed line should trace to the user's request.

## Surface Conflicts

- When two patterns contradict, pick one explicitly and explain why.
- Prefer the more recent, more tested, or more local convention.
- Keep the chosen pattern intact rather than blending two into a compromise
  that matches neither.
- Surface a convention that looks harmful instead of silently forking it.

## Test Intent

- Tests encode why behavior matters, not just what happens.
- A test that cannot fail when business logic changes is wrong.

## Fail Loud

- Define success criteria and loop until verified.
- Checkpoint after significant steps: what changed, what is verified, what
  remains.
- Report what was skipped or left unverified, in the same breath as what
  passed.
- Default to surfacing uncertainty.

## No Jargon

State facts plainly in rules, code, commits, everywhere — see the root
[`AGENTS.md`](../../AGENTS.md#no-jargon-anywhere) for the full rule.
