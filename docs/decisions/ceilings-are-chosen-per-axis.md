# A PGO ceiling is chosen on its own axis, not by one preset name

**Decision:** `pgo.limits` carries twelve ceilings and no `pgo.preset`.
The preset [`pgo.md`](../specs/pgo.md) carried as accepted design is abandoned rather than deferred:
the twelve answer three different questions,
and one name over all of them would couple choices an operator makes separately.
Each key keeps its own default.
Four of them have no local answer,
and the working-set arithmetic of [`pgo.md`](../specs/pgo.md) *Container* gives it.
[`configuration.md`](../configuration.md) carries the same arithmetic.

## Context

The preset rested on one premise:
that eleven of the twelve ceilings have no local answer,
because nothing about a particular cluster says whether `maxMergedBytes` should be 64 MiB,
while what an operator does know is how much of their fleet they intend to profile at once.
One name would ask that question once and answer the twelve.

**Measurement inverted the premise.**
Sizing the decoder against what it actually retains
([`2026-09-08-decoder-footprint.md`](../investigations/2026-09-08-decoder-footprint.md))
turned the four memory ceilings into the terms of an arithmetic an operator can solve:
a container budget and any three of `maxActiveCollections`, `maxParallel`,
`maxSampleBytes`, and `maxMergedBytes` fix the fourth.
The four keys with the strongest claim to having no local answer are now the four that have one,
and it is an answer no preset name could give,
because it depends on the memory a node or a quota will admit.

**The shipped ceilings need two presets at once.**
Against the three columns the spec published,
the four memory defaults this build ships are exactly `small`:

| | `maxParallel` | `maxSampleBytes` | `maxMergedBytes` | `maxActiveCollections` | `maxRetention` |
|---|---|---|---|---|---|
| shipped | 4 | 16 MiB | 32 MiB | 1 | 24h |
| `small` | 4 | 16 MiB | 32 MiB | 1 | 48h |
| `standard` | 4 | 32 MiB | 64 MiB | 2 | 72h |

The seven scale defaults are exactly `standard`:
`maxDuration`, `maxRounds`, `minEvery`, `maxEvery`, `maxTargetsPerRound`,
`onDemandPerMinute`, and `maxLiveCollections`,
five of which `small` would lower while leaving `minEvery` and `maxEvery` where they are.
`maxRetention` matches no column at all, because it answers neither question:
it bounds how long an artifact stays downloadable,
which follows from what consumes the profiles.
So the configuration this build actually runs has no name.
Two changes produced it, and both stayed on the memory axis:
lowering three of the four sizing defaults
([`collection-stays-in-the-gateway.md`](collection-stays-in-the-gateway.md)),
and then correcting the factors those four are multiplied by.
Neither touched any of the other eight keys.
Under a preset, each would have forced all three columns to be redrawn.
Needing a redraw whenever one measurement moves is the coupling stated as a cost.

**Two smaller facts point the same way.**
`maxEvery` was `24h` in all three columns, so one of the twelve never varied at all;
its own range caps it there, so a column that moved it could only have moved it down.
And the spec said in its own words that a preset is a set of defaults and not a mode,
because nothing downstream branches on its name —
which makes it documentation carried as a configuration key.
Documentation is what *Container* and [`configuration.md`](../configuration.md) already are.

**What is given up.**
The seven scale ceilings still have no arithmetic, and abandoning the preset leaves them unanswered.
They keep defaults that a fleet of fewer than ten collecting Services has run against,
and an operator changes one when their fleet gives them a reason to.
A table of suggested values by fleet size would close that gap;
a name that also moved the four memory ceilings would reopen the one this record closes.

## Consequences

- `pgo.preset` leaves [`pgo.md`](../specs/pgo.md).
  *Presets* becomes *Choosing the ceilings*, which states the three questions and carries no table of names,
  and the retention text that shared that subsection becomes *Retention* beside it.
- No code changes.
  No Go source and no chart template ever named a preset,
  so this record settles a design and touches nothing that runs.
  A configuration file that writes `pgo.preset` now fails to load under strict unknown-key handling,
  like any other key the schema does not carry.
- The chart's render-time refusals keep the four sizing variables and the four raw-block keys
  and lose the two that named a preset.
- This record takes over the subject of one bullet in
  [`collection-stays-in-the-gateway.md`](collection-stays-in-the-gateway.md),
  the one reading that `pgo.preset` "is not built" because the ceilings it would collapse are not one axis.
  That reason is the one adopted here, with the measurement it lacked when it was written.
  A decision record is not edited once accepted ([`README.md`](../README.md)),
  so that bullet stands as written and this file is where its subject is read.

## What a later proposal has to establish

This is not a list of triggers that bring the preset back.
It is what any proposal to set several ceilings with one name has to show first,
whether the name covers twelve of them or two.

- **The keys share one axis.**
  A reason to move one has to be a reason to move the others,
  demonstrated on a change that actually happened rather than argued from how the names sound.
- **The name survives a measurement.**
  A correction to one term must not force every other value the name carries to be restated,
  because that is the cost this record was written about.
- **A qualifying example exists.**
  One name over the four memory ceilings would qualify:
  they are the four terms of a single expression,
  so each trades against the same container budget and a name can move along that budget coherently.
  It would not follow that the four can only move together —
  raising `maxMergedBytes` alone is a legitimate change,
  and such a name would be a starting point rather than the only way to reach a figure.
  A name reaching past them to retention or to the on-demand rate would not qualify,
  because no budget relates those to the four.
