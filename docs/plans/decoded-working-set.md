# The Decoded Working Set Is Sized by What It Retains

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.

**Goal:** make the container's memory figure follow what a decoded profile actually retains,
and make the guards that watch for a change in that figure able to fail.
One constant, `config.PGODecodeFactor = 8`, is both the sizing term of `Config.PGOMemoryBytes`
and the comparand of `TestRoundsDecodeHeapDelta`,
and it is a value inside the measured range rather than a ceiling over it:
a decoded CPU profile retains 3.4 to 9.9 times the decompressed bytes it was parsed from,
and a merged one 7.2 to 12.7 times the length of its own uncompressed encoding.
The guard that would have caught this cannot:
it skips under `-race`, which is every test command this repository runs,
and it drops the decompressed input between its two `runtime.ReadMemStats` reads,
so it measures the decoded profile minus one input against a bound written for the decoded profile.
`pgo.limits.maxMergedBytes` bounds the gzipped serialization,
and what a profile compresses to varies from 2.6 to 33 times between profiles,
so a factor over that ceiling sizes nothing.
And no `GOMEMLIMIT` is set anywhere,
so a process can be killed by the transient a parse or a merge allocates
while its live heap is exactly the working set the container was sized for.
After this plan the container is sized by two measured factors,
the merge ceiling bounds the encoding those factors multiply,
a collecting process holds its own soft memory limit,
and two guards band each fixture's own measurement in every build.

**Architecture:** `internal/pgo` counts the running merged profile's uncompressed encoding against `maxMergedBytes`
while `finish` still stores gzip,
and carries two heap guards whose bands are each fixture's own measurement rather than a sizing factor;
`internal/config` splits `PGODecodeFactor` into `PGODecodeRetainFactor` and `PGOMergeRetainFactor`,
takes the new arithmetic into `PGOMemoryBytes` under a validation-time overflow check,
and derives the soft memory limit from the smaller of `GatewayMemoryBytes` and the process's own cgroup limit;
`cmd/profgate` applies that limit at startup through a seam, when `pgo.enabled` is true and never otherwise;
`deploy/chart/profgate` and `deploy/base` follow the binary's arithmetic and stop describing a compressed length;
`docs/configuration.md`, `docs/deployment.md`, and `docs/pgo.md` carry the worked example,
the printed figures, and an upgrade note.
No route, configuration key, chart value, Kubernetes permission, or NATS permission moves.

**Spec:** every behavior here is accepted text in [`pgo.md`](../specs/pgo.md):
*Container* for the formula, the two factors, what each was measured against, and the soft memory limit
(`docs/specs/pgo.md:391-399`, `:412-452`, `:454-464`, `:466-489`, `:491-504`);
*Rounds* for the encoding `maxMergedBytes` bounds and the encoding the store holds (`:1845-1858`);
*Configuration* for the two factors being constants rather than keys (`:2984-2986`);
*Presets* for the working-set and collector-memory rows and the bucket arithmetic (`:3087-3088`, `:3137-3139`);
*Unit* for both guards, their lifetimes, their band, and what the band is not (`:3659-3679`);
and the amendment block that lists every file this plan touches (`:4538-4562`).
Evidence for every figure below:
[`2026-09-08-decoder-footprint.md`](../investigations/2026-09-08-decoder-footprint.md).
This work is ordered by [`roadmap.md`](roadmap.md),
under *Size the decoder against what it actually retains* (`docs/plans/roadmap.md:403-449`),
and it closes the first bullet of *Close the gates that do not run* (`:320-327`),
which that item left open for this one.
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the system, not as the defects that revealed them.

- **A ceiling bounds the encoding a decoder sees.**
  `serializedSize` writes through `r.write`, which is `profile.Write` and gzips (`internal/pgo/rounds.go:99`, `:418-425`),
  `absorb` checks that output against `maxMergedBytes` after every absorbed sample (`:392`),
  and `finish` gzips into a buffer and checks its length again (`:538-541`).
  What a profile compresses to is a property of the profile:
  `cpu-heap.pprof` compresses 33.3 times and a captured busy profile 2.9,
  against deflate's own ceiling near a thousand to one.
- **A sizing factor multiplies the quantity it was measured against.**
  `PGODecodeFactor` is documented as heap against *encoded* length,
  "two buffers of input plus about six times that" (`internal/config/config.go:526-529`);
  `PGOMemoryBytes` spends it once per in-flight sample and twice over `maxMergedBytes` (`:550-555`);
  the chart says a profile costs eight times its *compressed* length
  (`deploy/chart/profgate/values.yaml:495-496`, `templates/_helpers.tpl:228-229`);
  and `rounds_test.go:851-854` says the decompressed body.
  Only the last matches the code, and the value 8 is inside the measured range rather than above it.
- **A guard that watches a figure can fail when the figure moves.**
  `TestRoundsDecodeHeapDelta` skips under `-race` (`internal/pgo/rounds_test.go:849-851`),
  every test command in `mise.toml` passes `-race`,
  and the guard compares a delta that dropped the input against `PGODecodeFactor` — a comparand carrying headroom,
  so it can only fail once the container is already mis-sized.
- **A derived byte count is a byte count.**
  `PGOMemoryBytes` forms `maxActiveCollections × (…)` with no overflow check (`internal/config/config.go:550-555`)
  and `MaxActiveCollections` carries `validate:"min=1"` and no ceiling (`:365`),
  so a configuration this build accepts today can wrap the product to a negative number.
  The chart refuses that case at render time (`deploy/chart/profgate/templates/_helpers.tpl:241-243`);
  the binary does not.
- **The budget the container carries is a live heap, and the transient above it is bounded by something.**
  `grep -rn GOMEMLIMIT .` and `grep -rn SetMemoryLimit .` find nothing,
  so the runtime targets twice the live heap;
  `internal/pgo/rounds.go:373` holds the old merged profile, the incoming one, and the result at once,
  and `:536` compacts into a second merged profile beside the first.

---

## Decisions

Fifteen choices settle how the accepted text is carried, and the Go and pprof facts that shape each one.
The design itself is settled in the spec and is not reopened here;
where a reviewer questions one of these, the reason is written below.

**`serializedSize` counts the uncompressed encoding, and `finish` counts it and then gzips.**
`profile.Write` and `profile.WriteUncompressed` both call the package's `serialize(p)`,
which materializes the whole marshaled protobuf in memory,
and `Write` then streams that through a `gzip.Writer`
(`$(go env GOMODCACHE)/github.com/google/pprof@v0.0.0-20260802141513-ef3492d7dac3/profile/profile.go:336-355`).
So counting the uncompressed length through a `countingWriter` allocates the marshal and retains no output buffer,
which is what *Rounds* means by counting a length rather than keeping it (`docs/specs/pgo.md:1849-1851`).
`Rounds` gains a second seam, `writeUncompressed`, beside `write` (`internal/pgo/rounds.go:80-87`, `:99`);
`serializedSize` writes through the new one,
and `finish` checks through `serializedSize` and stores through `write`.
`finish` therefore marshals twice at completion.
That is one extra marshal per completed Collection.
The alternative — holding the uncompressed buffer live while gzipping from it —
would put a second whole encoding in the heap at the moment *Rounds* already calls the transient (`:1857-1858`).

**`serialize` mutates the profile, and that is why the merge factor is 16.**
`serialize` calls `p.preEncode()` before marshaling (`profile/profile.go:336-341`),
which attaches a `locationIDX` slice to every sample and a `stringTable` to the profile
(`profile/encode.go:80-84`, `:127-131`),
and leaves both there until something replaces them.
`absorb` serializes the running merged profile after every sample that succeeds,
so a Collection holds a merged profile plus one encoding's worth of index for as long as it runs.
Measured over the investigation's own inputs, that raises the retained ratio from 6.4–10.6 to 7.2–12.7.
The accepted spec carries the consequence: `mergeRetainFactor` is 16, standing 26% above 12.7.
Two things follow for this plan.
The merge guard serializes its result *inside* its measurement interval, because that is what the process holds.
And nothing that measures a pprof profile may serialize it early to print a length:
discarding the output and collecting does not undo the attachment.

**The failing-writer test loses its call counter and gains a case that reaches completion.**
`TestRoundsFinishFailures` counts calls so that only the second write fails,
because the size check and the store write are the same seam today (`internal/pgo/rounds_test.go:640-649`).
With two seams the store write needs no counter at all,
and the three failures are separated by where they happen.
`writeUncompressed` is reached first by `absorb`,
whose failure ends the attempt immediately (`internal/pgo/rounds.go:391-397`),
so failing that seam outright proves nothing about `finish`.
The completion case therefore fails `writeUncompressed` only after the last sample has been absorbed —
a seam that counts its calls and fails the one `finish` makes —
and the absorption case fails it on the first call.
A third case fails `r.write`, the gzip write `finish` stores through,
which is the failure *Unit* requires inside `Write` and which neither `writeUncompressed` case reaches.
All three produce `serialize_failed`, from three different sites, and all three are asserted.

**`TestCollectionMergeAndWriteHeldPastTheCutoff` keeps working, and its comment does not.**
It wraps `r.write` and blocks only when the destination is a `*bytes.Buffer`,
because the size check writes to a counter and only the stored serialization writes to the buffer
(`internal/pgo/rounds_test.go:1430-1446`).
The split preserves that barrier — `finish` still gzips into the buffer — and invalidates the comment's reason,
which now belongs to the seam rather than to the destination.
The comment is rewritten in task 1 and the test is otherwise untouched.

**The discriminating case for the moved ceiling is a fixture that compresses well.**
`TestRoundsMergedTooLarge` sets `maxMergedBytes` to 1 KiB (`internal/pgo/rounds_test.go:593`),
which is below both encodings and so is exceeded whichever one the check reads.
`cpu-heap.pprof` is 516,906 bytes decompressed and 15,514 gzipped, a ratio of 33.3.
A ceiling between those two numbers is `merged_too_large` under the uncompressed encoding
and passes under the gzipped one, which is the red state.

**The guards band each fixture's own measurement, and the bands are written here.**
*Unit* requires ±15% of what a fixture measures, failing on a fall as well as a rise,
and forbids `decodeRetainFactor` and `mergeRetainFactor` as comparands (`docs/specs/pgo.md:3659-3679`).
The figures below are `HeapAlloc` deltas on Go 1.26.7, linux/amd64,
measured in this repository's own working copy against the fixtures as committed:

| guard | fixture | quantity | measured | committed centre | band, from that centre |
|---|---|---|---|---|---|
| decode | `cpu-heap.pprof` | 516,906 decompressed bytes, 20,000 samples at 25.8 bytes each | 4,472,248 – 4,472,360 | 4,472,300 | 3,801,455 – 5,143,145 |
| decode | `cpu-large.pprof` | 32,461 decompressed bytes, 400 samples at 81.2 bytes each | 174,432 | 174,432 | 148,267 – 200,597 |
| decode | `cpu-busy.pprof` | 358,386 decompressed bytes, 7,818 samples at 45.8 bytes each | 2,590,648 – 2,590,664 | 2,590,656 | 2,202,058 – 2,979,254 |
| merge | `cpu-busy.pprof` | 358,318 bytes of uncompressed encoding, 7,818 distinct samples | 4,456,192 – 4,461,528 | 4,456,000 | 3,787,600 – 5,124,400 |

The measured column is the observed range over at least three runs;
the centre is one number committed in the source;
the band is that centre times 0.85 and 1.15, computed by the test rather than written as two literals.
`cpu-heap.pprof` and `cpu-large.pprof` differ by a factor of three in bytes per sample,
which is what *Unit* asks for when it says at least two fixtures whose samples differ in encoded size;
`cpu-busy.pprof` is the third because it is committed for the merge guard anyway,
and it is the only captured profile of a busy process among them.
The three small fixtures — `cpu-a.pprof`, `cpu-b.pprof`, `alloc.pprof` — are excluded:
a few hundred bytes of input against a `profile.Profile` that costs several kilobytes empty is fixed overhead,
not a ratio.

**`cpu-busy.pprof` is committed by this plan's own change, not captured by the implementer.**
Every figure in the table above is measured against a specific file,
and "capture a profile of eight goroutines for 25 seconds" does not reproduce it:
a fresh capture is a different fixture with different centres.
So the file is added to `internal/pgo/testdata/` in the commit that adds this plan,
where no test reads it yet, and the tasks below only write the guards that do.
It is a Go CPU profile of a program burning CPU across eight goroutines for 25 seconds —
200 seconds of CPU, 7,818 samples at a mean stack depth of 11.1 —
recompressed with `gzip -9` from the bytes the investigation measured,
which changes nothing the guards read.
Its identity is checkable:

```text
sha256 of the file                   dd66d14af96db30ede3b3bf82c53eb80f201c7eaa124f95b6e1d51b4707ea3f5
sha256 of its decompressed bytes     4a3b6ef99afae25e7f469127adbd4b7917e53e14d1c0667a59eedbf8499ece3f
```

`cpu-heap.pprof` cannot serve for the merge guard,
because its 20,000 samples merge to 400 distinct ones and so measure a repetition.

**Each guard's lifetimes are an ordered sequence, and the order is the measurement.**
The decode guard, in this order and no other:

1. Decompress the fixture.
2. `runtime.GC()`, then read the baseline.
3. `profile.ParseData`.
4. `runtime.GC()`, then read the second `MemStats`.
5. `runtime.KeepAlive` the parsed profile **and** the decompressed input.

The input was allocated before the baseline read, so holding it adds nothing to the delta,
and dropping it lets the collection between the reads subtract its whole length.

The merge guard:

1. Decode the fixture's sources.
   This happens *before* the baseline read,
   so their own heap is in the baseline and contributes nothing to the delta.
2. `runtime.GC()`, then read the baseline.
3. `profile.Merge` the sources.
4. Serialize the merged result once, through a `countingWriter`,
   to learn the length the delta is banded against.
   This is inside the interval on purpose:
   `preEncode` attaches an index that the process then holds,
   and the factor the guard exists beside was measured with it.
5. `runtime.GC()`, then read the second `MemStats`.
6. `runtime.KeepAlive` the merged result **and** the sources.

Moving step 4 after step 5 measures a profile no Collection ever holds,
and reads about 17% low on this fixture.
Dropping the sources before step 5 subtracts their whole heap.
Both produce a number that looks plausible.

**Every row logs its measurement, whether or not it fails.**
A row that passes prints nothing from its failure message,
so a delta sitting just inside its floor is invisible —
which is exactly the state an uncorrected lifetime produces.
Each row therefore calls `t.Logf` with the fixture, the delta, the centre,
and the delta as a percentage of the centre, before it compares.
That is what makes `go test -v` a usable check on the day a band is re-measured.

**The soft memory limit is computed in `internal/config` behind a filesystem root, and applied in `cmd/profgate`.**
*Container* names two terms — the derived figure and the process's own cgroup limit —
and a tenth taken off the smaller of them (`docs/specs/pgo.md:466-478`).
Reading `/proc` and `/sys` directly is untestable,
so the reader takes a root directory and production passes `/`;
tests pass a `t.TempDir()` holding a fixture tree.
`internal/config` computes; `cmd/profgate` calls `runtime/debug.SetMemoryLimit`.
That split is the binary owning the arithmetic while the chart is told the answer once,
and it keeps `internal/config` free of a runtime side effect.

**The cgroup limit is resolved through membership and mountinfo, never through a fixed path.**
*Container* says the usual container paths, and `/proc/self/cgroup` with `/proc/self/mountinfo`
"where the usual container paths do not resolve" (`docs/specs/pgo.md:471-475`).
Reading `/sys/fs/cgroup/memory.max` directly is not that:
a visible mount can expose a *parent* cgroup whose limit is not the process's own,
and the read would then return a number that binds nothing.
The usual container path is what the resolution below produces in the ordinary case,
where a cgroup namespace makes the membership path `/` and the mount point the process's own cgroup.
So there is one algorithm and no shortcut:

1. Read `<root>/proc/self/cgroup`.
   Each line is `hierarchy-ID:controller-list:cgroup-path`.
   A version 1 memory membership is a line whose controller list contains `memory`;
   a version 2 membership is the line whose hierarchy id is `0` and whose controller list is empty.
   Prefer the version 1 memory membership when one exists.
   A kernel may put the memory controller on version 1 while a unified hierarchy also exists,
   so a `0::` line alone does not establish that memory is controlled there.
2. Read `<root>/proc/self/mountinfo`, whose fields are
   `id parent major:minor root mountpoint options… - fstype source superopts`.
   For version 1, take a mount whose fstype is `cgroup` and whose super options contain `memory`;
   for version 2, a mount whose fstype is `cgroup2`.
   Both `root` and `mountpoint` carry octal escapes — `\040`, `\011`, `\012`, `\134` — and both are decoded.
3. The file's directory is the mount point,
   joined to the membership path with the mount's root prefix removed.
   The prefix match is on path components, not on characters,
   so a mount rooted at `/tenant` does not match a membership of `/tenantry`.
   A membership that is not under the mount root means this mount does not expose the process's cgroup:
   try the next matching mount, and give up when none is left.
   A membership equal to the mount root — commonly `/` under a cgroup namespace,
   but only when the mount's own root is `/` too — leaves the mount point itself.
   Then reject what remains if it escapes:
   a suffix containing a `..` component can walk out of the mounted hierarchy
   and name an unrelated part of the filesystem, so it is refused rather than cleaned.
   A rejected suffix rejects **that mount**, not the search:
   move to the next matching mount, exactly as a failed prefix test does.
   A membership of `/../../init` leaves an escaping suffix under a mount rooted at `/`
   and a usable `/init` under one rooted at `/../..`,
   so a resolution that stopped at the first candidate would miss the limit that binds.
   The Go runtime's own reader continues for this reason.
4. Read `memory.max` there under version 2, or `memory.limit_in_bytes` under version 1.
5. The outcomes, each its own test row:
   no memory membership; no mount exposing it; every candidate mount leaving an escaping suffix;
   a file that is absent;
   a file that cannot be read; a value that does not parse; a value that is negative or zero;
   `max` under version 2; a value at or above `1 << 62` under version 1,
   which is how it spells unlimited as a page-aligned near-maximum;
   a value at or above the derived figure;
   and a positive value below the derived figure, which is the one case that binds.
   Every other case means no cgroup limit, and the derived figure stands alone.
   Negative and zero are refused rather than treated as small:
   a negative reading would derive a negative soft limit,
   and `debug.SetMemoryLimit` reads a negative argument as a query and silently sets nothing,
   so the process would log that it set a limit it did not set.
   The `1 << 62` sentinel is a version 1 rule and is not applied to a version 2 reading,
   which spells unlimited as `max`.
   A read error is not a startup failure:
   an unreadable file is a fact about the sandbox, not about the ceiling.

The tenth is a named constant with its reason on it,
and the arithmetic is `figure / 10 * 9` so no product of two large numbers is formed.
At the shipped container figure that is `1842138315`, which a test pins,
because integer division makes the rounding a decision rather than an accident.

**No test moves the real process limit, and no test reads the real cgroup.**
`debug.SetMemoryLimit` has no scope:
a test that sets a 1.8 GiB soft limit changes how hard the runtime collects,
for the rest of that binary's run.
`serveDeps` gains two fields in the shape the fifteen already there have (`cmd/profgate/serve.go:83-99`):

```go
	setMemoryLimit func(int64) int64                       // production: nil, so serve calls debug.SetMemoryLimit
	softMemoryLimit func(*config.Config) (int64, bool)     // production: nil, so serve calls cfg.SoftMemoryLimit
```

each with an accessor beside `retryBackoff` (`:101-105`) returning the production function when the field is nil.
Two fields rather than one, because the setter alone does not isolate the test:
`SoftMemoryLimit` reads the real cgroup through `/`,
so a runner inside a small cgroup computes a smaller figure,
and an assertion about the derived one then fails for a reason that has nothing to do with the code.
The filesystem resolution stays proved in `internal/config`, against fixture trees;
`cmd/profgate` proves only the wiring.
The figure the setter receives is the **soft** limit — `1842138315` at the shipped ceilings —
not the container figure `SoftMemoryLimit` derives it from.
**`startGatewayWith` installs both fakes for every test that goes through it**
(`cmd/profgate/serve_test.go:493-544`), not only the new one:
existing tests already start a gateway with collection enabled (`:1914-1918`)
and would otherwise reach the real `debug.SetMemoryLimit` and the runner's real cgroup.

**A limit already in force is never raised.**
The spec names two terms and says nothing about `GOMEMLIMIT` in the environment,
which the runtime has already applied by the time `main` runs.
Two readings are available: replace it, or take the smaller of it and the derived figure.
This plan takes the smaller.
A soft limit is a ceiling,
and lowering a ceiling an operator set is a surprise that costs them memory they asked to keep;
`debug.SetMemoryLimit(-1)` returns `math.MaxInt64` when nothing set one,
so the extra term costs nothing when the variable is absent and the derived figure wins as the spec describes.
The chart sets no `GOMEMLIMIT` of its own,
so the only way an existing limit binds is an operator's own `extraEnv`;
the branch is reachable rather than theoretical, and `docs/deployment.md` says what it does.

**The multiplication is checked before its result reaches the runtime.**
*Container* requires it in the binary as well as the chart (`docs/specs/pgo.md:506-516`),
and until now nothing needed it:
a wrapped figure was a number in a Deployment an operator could see.
It is now the argument to `debug.SetMemoryLimit`,
where a negative value is read as a query and silently sets nothing.
So configuration validation forms the product with overflow checks —
the per-collection term, the multiplication by `maxActiveCollections`, and the addition of the base —
and refuses the configuration, naming the four ceilings that produced it.
`PGOMemoryBytes` and `GatewayMemoryBytes` keep their signatures,
because a validated configuration is the only kind either is called on.
This is not the `maxActiveCollections` ceiling of *Presets*:
that bound arrives with `pgo.preset`, which is not built,
and the check has to hold without it because `validate:"min=1"` is the whole of today's range
(`internal/config/config.go:365`).

**The chart, the manifests, and the printed figures move in one commit with the arithmetic.**
`TestChartMemoryLimitIsDerived` loads the rendered ConfigMap through `internal/config`,
and compares the rendered limit against `Config.GatewayMemoryBytes` (`deploy/chart_test.go:411-445`, `:544`, `:740`);
`TestChartBaseTermIsTheSameFigureBothWays` holds `memoryLimitWithoutPGO` against the binary's base term (`:632-647`).
Neither pins a byte count of its own —
the raised-ceilings case moves all four inputs and still compares two formulas —
so nothing in `deploy/` needs recomputing,
but everything goes red the moment only one side moves.
The e2e harness pins the same figure as a literal (`test/e2e/harness_config_test.go:206-210`)
and moves in that commit too, though `mise run test` does not build it.

---

## What the figures become

One table, so no task has to recompute them.
The shipped ceilings are `maxParallel: 4`, `maxSampleBytes: 16777216` (16 MiB),
`maxMergedBytes: 33554432` (32 MiB), `maxActiveCollections: 1`.

| Figure | Today | After |
|---|---|---|
| the per-sample term | `maxParallel × 8 × maxSampleBytes` | `maxParallel × (2 + 12) × maxSampleBytes` |
| the merged term | `2 × 8 × maxMergedBytes` | `(1 + 16) × maxMergedBytes` |
| PGO working set at the shipped ceilings | `1073741824` (1 GiB) | `1509949440` (1440 MiB) |
| container memory at the shipped ceilings | `1610612736` (1536 MiB) | `2046820352` (1952 MiB) |
| soft memory limit at the shipped ceilings | none | `1842138315` |
| container memory with `pgo.enabled: false` | `536870912` (512 MiB) | unchanged, and still no soft limit |

The gateway's own base term, `PGOGatewayBaseMemory`, stays **512 MiB**.
The 256 MiB `collectorBaseMemory` of *Container* is the collector Deployment's base,
and no collector Deployment is built (`docs/specs/pgo.md:4504-4536`),
so the preset table's collector-memory row (1696, 6016, 18944 MiB) is not what `config validate` prints,
and no figure in this plan is derived from it.
The working-set row of that table — 1440, 5760, 18688 MiB — is the second term alone,
and it is what the new arithmetic produces.

**Every derived limit rises where collection is enabled.**
The difference is `maxActiveCollections × (6 × maxParallel × maxSampleBytes + maxMergedBytes)`,
which no admissible ceiling makes negative,
so no configuration's working set falls and none is unchanged.
Two kinds of installation still keep the limit they had, and the changelog says so.
One with `pgo.enabled` false carries the base term alone,
which `GatewayMemoryBytes` returns unchanged (`internal/config/config.go:561-566`).
One that writes an explicit `resources.limits` in the chart is rendered as written
(`deploy/chart/profgate/templates/_helpers.tpl:295-308`),
because that value replaces the derivation rather than adding to it.

---

## Global Constraints

- **No new configuration key, route, chart value, or Kubernetes or NATS permission.**
  Both factors are constants the spec fixes (`docs/specs/pgo.md:2984-2986`),
  and the soft memory limit is derived from figures the process already holds.
- **Every implementation task shows a red test before its change, and says what the red run observes.**
  Tasks 1 through 5 each name the test, the exact command, and the losing state the test forces.
  A red run here always means a test that *fails*.
  Where the starting state is a test that skips rather than fails,
  the task says so and names what it does about it.
- **A heap measurement is a table row, not a bespoke test.**
  [`300-testing.md`](../../.agents/rules/300-testing.md) asks for `t.Run(tc.name, ...)`;
  the decode guard's three fixtures are three rows of one table,
  and the cgroup reader's outcomes are rows of another.
- **A heap measurement runs serially, and nothing in its interval is unrelated work.**
  The investigation measured one observation per process.
  A package test cannot have that, so each row runs without `t.Parallel()`,
  allocates nothing between its two reads but the call under measurement,
  and takes a fresh input rather than one another row has touched.
  The bands are ±15%,
  which is what absorbs the difference between an isolated process and a package run;
  a centre that only holds in isolation is a centre to re-measure, not a band to widen.
- **`-race` and `-count=1` on every red run.**
  The commands below carry both,
  and the guards are expected to pass under `-race` as well as without it,
  the detector moving the ratio by under half a percent on this toolchain (`docs/specs/pgo.md:3665-3666`).
- **Chart tests skip without helm.**
  `helmBin` skips when `helm` is absent from `PATH` (`deploy/chart_test.go:30-39`),
  so a bare `go test ./deploy/` can report success while asserting nothing.
  Every command below that exercises `deploy/` runs through `mise`, so the pinned helm is present,
  and a run whose output says `SKIP` is not a result.
- **The linter is the pinned one, and the implementer checks which one resolves.**
  [`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
  gives the validation block as `mise run lint`.
  On the machine this plan was written on it does not run the pinned linter.
  `mise exec -- which golangci-lint` resolves
  `~/.local/share/mise/installs/go/1.26.7/bin/golangci-lint`,
  a binary installed by the Go toolchain that shadows the `golangci-lint@2.12.2` tool,
  and `mise exec -- golangci-lint --version` reports `v2.1.6`.
  Those two commands are the check, because they read the environment a `mise` task runs in;
  comparing a bare shell's `golangci-lint` against an explicitly versioned `mise exec` does not.
  Run them once before the first task.
  If they agree, use `mise run lint` as the rules say.
  If they disagree, use `mise exec golangci-lint@2.12.2 -- golangci-lint run ./...` in its place,
  and say so in the pull request, because a task linted by the wrong version is not linted.
- **No jargon:** comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a task name, or a review round.
- Markdown prose uses semantic line breaks;
  run `semlf check` on every Markdown file and every Go file with doc comments a task writes or edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters — the hook refuses 50 or more —
  with a body that says what changed and why, one sentence per line under 120 characters,
  and no trailer of any kind
  ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
  A commit is finished when `git log --oneline -1` shows it and `git status --short` is clean,
  because the hook can refuse a message after `git commit` has already run
  ([500](../../.agents/rules/500-validation-and-workflow.md));
  every validation block below ends with both.
- Every task ends with the same validation block before its commit,
  with the linter written as the check above decided:

```bash
mise run lint && mise run test && mise run check && mise run prose
```

---

## File Structure

```text
internal/pgo/rounds.go                        # the writeUncompressed seam; serializedSize counts it; finish checks it and stores gzip
internal/pgo/rounds_test.go                   # the uncompressed-ceiling case; three serialization failures; the banded decode guard; the merge guard
internal/pgo/race_on_test.go                  # deleted with the skip, if nothing else reads raceEnabled
internal/pgo/race_off_test.go                 # deleted with the skip, if nothing else reads raceEnabled
internal/pgo/testdata/cpu-busy.pprof          # added by this plan's own change; read first by the guards of tasks 2 and 3
internal/config/config.go                     # PGODecodeRetainFactor and PGOMergeRetainFactor; the new arithmetic; the checked product
internal/config/memlimit.go                   # new: the cgroup read behind a filesystem root, and the derived soft limit
internal/config/config_test.go                # the working-set and container figures, and the refused overflow
internal/config/memlimit_test.go              # new: the cgroup outcomes, the rounding, and the disabled case
cmd/profgate/serve.go                         # the setMemoryLimit and softMemoryLimit seams, and the startup call
cmd/profgate/serve_test.go                    # startGatewayWith installs both fakes; the wiring cases
cmd/profgate/main_test.go                     # the figures config validate prints
deploy/chart/profgate/templates/_helpers.tpl  # the two factors in the rendered arithmetic and in the comment above it
deploy/chart/profgate/values.yaml             # the formula comment, the factors, and the worked figure
deploy/chart/profgate/README.md               # the formula and the figure it quotes
deploy/chart_test.go                          # the container sum's overflow boundary, which the formula comparisons do not reach
deploy/base/deployment.yaml                   # the figure the comment tells an operator to raise the limit to
deploy/base/configmap.yaml                    # the same figure in the enablement comment
test/e2e/harness_config_test.go               # the literal the PGO harness patches the Deployment with
docs/configuration.md                         # the sizing table, the worked example, and the upgrade note
docs/deployment.md                            # the formula, the figure, and the soft memory limit
docs/pgo.md                                   # the container figure the guide quotes
docs/plans/roadmap.md                         # this item's Shipped line and item 8's first bullet, in the closing task
CHANGELOG.md                                  # one entry per client-visible move
docs/plans/decoded-working-set.md             # this file
```

`deploy/chart_test.go`'s existing comparisons need no change:
they already read the binary's own arithmetic on both sides.
It appears above only for the overflow boundary they cannot reach, which task 4 adds.
`deploy/base/deployment.yaml`'s `512Mi` literal is deliberately unchanged:
that manifest ships collection disabled,
and `deploy/deploy_test.go` compares it with the binary's own disabled figure.
Only its comment, which names the figure an operator raises the limit to, moves.

---

## 1. The merge ceiling bounds the encoding a decoder sees

Closes the roadmap bullet beginning *`pgo.limits.maxMergedBytes` bounds the gzipped serialized size*.

**Files:**
- Modify: `internal/pgo/rounds.go`, `internal/pgo/rounds_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the second seam, the two marshals at completion, and where each failure is forced.
`Rounds` (`internal/pgo/rounds.go:80-87`) gains one field beside `write`:

```go
	writeUncompressed func(p *profile.Profile, w io.Writer) error
```

`NewRounds` (`:89-101`) sets it to
`func(p *profile.Profile, w io.Writer) error { return p.WriteUncompressed(w) }`.
`serializedSize` (`:418-425`) writes through the new seam,
and its doc comment says the uncompressed encoding is what `maxMergedBytes` bounds
and that the store holds the gzipped one.
`finish` (`:531-560`) checks through `serializedSize(merged)` after `Compact()` and before it writes;
the `int64(buf.Len()) > MaxMergedBytes` check at `:541` goes,
because the length of the stored object is recorded rather than compared.
`absorb`'s check at `:392` is unchanged in shape and changes meaning with the seam under it.

- [ ] **Write the test**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestRoundsMergedTooLarge`, `internal/pgo/rounds_test.go:580-613` | gains a subtest, "inside the gzipped ceiling and outside the uncompressed one": one Pod serving `cpu-heap.pprof`, `maxMergedBytes` at `262144` — above its 15,514 gzipped and far below its 516,906 uncompressed — and the result is `merged_too_large` with no object stored. Today the check reads the gzipped length and the Collection completes, so the red run is a completion where the test wants a refusal. The existing 1 KiB subtest keeps its assertion, and its comment says it is below both encodings and therefore says nothing about which is read |
| `TestRoundsFinishFailures`, `:631-658` | three serialization failures where there was one. "a writer that fails during absorption" fails `writeUncompressed` on its first call. "a writer that fails at completion" fails `writeUncompressed` only on the call `finish` makes — the seam counts, and lets every absorption through. "a gzip writer that fails" fails `r.write` outright, which is now the store write alone and needs no counter. All three assert `serialize_failed` with no object stored. The second is what proves the completion check exists. The third keeps the coverage *Unit* asks for of a failure inside `Write` (`docs/specs/pgo.md:3703`), which the two `writeUncompressed` cases do not reach |
| `TestRoundsMergesEverySample`, `:60-101` | gains one assertion: the stored object begins `0x1f 0x8b`. It parses the object today (`:95`) but never asserts the encoding, so nothing in the suite would notice a `finish` that stopped compressing |
| `TestCollectionMergeAndWriteHeldPastTheCutoff`, `:1430-1446` | unchanged behavior, corrected comment: the barrier holds because `finish` still gzips into the buffer, not because the size check writes to a counter through the same seam |

The red state comes in two steps, because the seam-dependent tests do not compile before the field exists,
and a package that does not compile demonstrates nothing about a ceiling.

First, write only the `TestRoundsMergedTooLarge` subtest, which needs no new seam:

```bash
mise exec -- go test -race -count=1 ./internal/pgo/ -run 'TestRoundsMergedTooLarge' -v
```

It must report a completed Collection where the subtest wants `merged_too_large`.
Then add the seam, the two `finish` changes, and the three `TestRoundsFinishFailures` subtests together:

```bash
mise exec -- go test -race -count=1 ./internal/pgo/ -run 'TestRoundsMergedTooLarge|TestRoundsFinishFailures|TestRoundsMergesEverySample' -v
```

- [ ] **Move the ceiling and say so**

`CHANGELOG.md`, `### Changed`:
**`pgo.limits.maxMergedBytes` bounds the merged profile's encoding before compression.**
It bounded the gzipped serialization, and what a profile compresses to is a property of the profile:
a repetitive one compresses 33 times where a captured one compresses under 3.
The key keeps its name and its value, and the stored artifact is still gzipped —
the size a download reports, and the size the console and `profgate collect` print, is still the stored length.
What changes is which Collections are refused as `merged_too_large`:
a Collection that compresses well and would have completed now fails at a ceiling it passed before.
An operator whose Collections approach the ceiling raises `maxMergedBytes` for the encoding it now reads.

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/rounds.go CHANGELOG.md
mise run lint && mise run test && mise run check && mise run prose
git add internal/pgo/rounds.go internal/pgo/rounds_test.go CHANGELOG.md
git commit -m "fix(pgo): bound the merge before compression" -m "<body: what a profile compresses to is a property of the profile, so the gzipped ceiling bounded nothing; the count is uncompressed and the store still holds gzip>"
git log --oneline -1 && git status --short
```

---

## 2. The decode guard measures what it claims, in every build

Closes the roadmap bullets beginning *`TestRoundsDecodeHeapDelta` keeps only `parsed` alive*
and *The guard borrows the whole sizing constant*,
and the first bullet of *Close the gates that do not run*.

**Files:**
- Modify: `internal/pgo/rounds_test.go`
- Delete: `internal/pgo/race_on_test.go`, `internal/pgo/race_off_test.go`

**The decision, and why.**
*Decisions* settles the bands, the three fixtures, the lifetimes, and the logging.
`TestRoundsDecodeHeapDelta` (`internal/pgo/rounds_test.go:843-877`) becomes a table over three fixtures,
each row naming the fixture and the centre measured for it.
The skip on `raceEnabled` goes (`:849-851`).
`grep -rn raceEnabled internal/` finds `rounds_test.go:849` and the two helper files and nothing else,
so both helpers are deleted in this task and both deletions are staged with it.
The comment about `config.PGODecodeFactor` multiplying `maxSampleBytes` goes with the comparand.
Each row measures in the order *Decisions* gives,
and after the second read `plain` appears only in its `KeepAlive`:

```go
plain := gunzipBytes(t, fixtureProfile(t, tc.fixture))

runtime.GC()
var before, after runtime.MemStats
runtime.ReadMemStats(&before)

parsed, err := profile.ParseData(plain)
// ...
runtime.GC()
runtime.ReadMemStats(&after)
runtime.KeepAlive(parsed)
runtime.KeepAlive(plain)
```

The band is written as the measured centre and one fraction:

```go
// heapBandFraction is how far a fixture's measured delta may move before a
// guard fails. A rise means the decoder retains more than the container was
// sized for; a fall means it retains less, which is when to take the memory
// back.
const heapBandFraction = 0.15
```

- [ ] **Write the test**

| Test | What it asserts |
|---|---|
| `TestRoundsDecodeHeapDelta`, rewritten in place | three rows — `cpu-heap.pprof` at 4,472,300, `cpu-large.pprof` at 174,432, `cpu-busy.pprof` at 2,590,656 — each logging the fixture, the delta, the centre, and the delta as a percentage of the centre, then asserting the delta is inside ±15% of that centre. It fails on a fall as well as a rise. The failure message names the fixture, the delta, the centre, and both ends of the band |

**The starting state is a skip, not a failure.**
`go test -race -count=1 ./internal/pgo/ -run TestRoundsDecodeHeapDelta -v` prints `SKIP` today
(`internal/pgo/rounds_test.go:849-851`),
which is the condition this task removes rather than a red run.
Treat a `SKIP` in that output as a failing result for the purposes of this task,
and record what it printed before the change.

**The red run for the lifetime is a mutant, and only one row carries it.**
Dropping the input subtracts its whole decompressed length,
so the uncorrected delta is the corrected one less that length.
Measured on this machine, three runs each:

| fixture | corrected | uncorrected | band floor | uncorrected verdict |
|---|---|---|---|---|
| `cpu-heap.pprof` | 4,472,248 – 4,472,360 | 3,942,624 – 3,947,960 | 3,801,455 | inside the band, 11.8% below the centre |
| `cpu-large.pprof` | 174,432 | 136,344 – 141,664 | 148,267 | **below the floor by 4.5% to 8.0%, and fails** |
| `cpu-busy.pprof` | 2,590,648 – 2,590,664 | 2,230,216 – 2,235,520 | 2,202,058 | inside the band, 1.3% to 1.5% above the floor |

So the demonstration is exact.
With the table written and the band in place, make two edits and no others:
delete the `runtime.KeepAlive(plain)` line,
and add `plain = nil` before the second `runtime.GC()` so the collection can actually reclaim it.
Name `plain` nowhere else afterwards —
a `len(plain)` in a log line keeps it live and hides the whole effect
(`docs/investigations/2026-09-08-decoder-footprint.md:51-56`),
so the row's `t.Logf` takes the fixture's length from the table rather than from the variable.
`cpu-large.pprof` must fail;
the other two must log a delta visibly below their centres.
Then **undo both edits** — remove the `plain = nil` assignment as well as restoring the `KeepAlive` —
and run again.
Restoring only the `KeepAlive` keeps a nil slice alive and leaves the mutant in place.
Record both runs' output in the pull request: a red run nobody watched is not evidence.
If `cpu-large.pprof` passes uncorrected on the implementation machine,
stop and measure a fourth fixture that fails — do not widen the band.

The commands:

```bash
mise exec -- go test -race -count=1 ./internal/pgo/ -run 'TestRoundsDecodeHeapDelta' -v
mise exec -- go test -count=1 ./internal/pgo/ -run 'TestRoundsDecodeHeapDelta' -v
```

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/rounds_test.go
mise run lint && mise run test && mise run check && mise run prose
git add internal/pgo/rounds_test.go
git rm internal/pgo/race_on_test.go internal/pgo/race_off_test.go
git commit -m "test(pgo): band the decoder's heap per fixture" -m "<body: the guard dropped the input between its reads and borrowed a sizing constant, and skipped in every build this repository runs; it holds both lifetimes, bands each fixture's own measurement, and fails on a fall>"
git log --oneline -1 && git status --short
```

---

## 3. A merged profile is banded against its uncompressed encoding

Carries the second guard of *Unit* (`docs/specs/pgo.md:3669-3677`),
which is what watches the factor task 4 spends.

**Files:**
- Modify: `internal/pgo/rounds_test.go`

**The decision, and why.**
*Decisions* settles the fixture, its identity, and the six-step lifetime order —
including that the serialization happens *inside* the measurement interval,
because `preEncode` attaches an index the process then holds
and `mergeRetainFactor` was measured with it.
The fixture is already in the tree; this task adds no file.
Its properties are asserted by the guard itself,
so a fixture that is not the one this plan measured is a test failure rather than a silent drift.

- [ ] **Write the test**

| Test | What it asserts |
|---|---|
| `TestRoundsMergeHeapDelta`, new, beside `TestRoundsDecodeHeapDelta` | decodes `cpu-busy.pprof` before the baseline read; `runtime.GC()`; baseline `MemStats`; `profile.Merge` of the one source; `WriteUncompressed` into a `countingWriter`; `runtime.GC()`; second `MemStats`; `KeepAlive` on the merged result and on the sources. It logs the delta, the centre, and the percentage, then asserts the delta is inside ±15% of 4,456,000. It asserts the two properties the centre depends on, each with its own message: exactly 7,818 distinct samples, and an uncompressed encoding of 358,318 bytes within 1% |

The guard does not exist today, so its first run is its own first result —
its absence is not a compile failure,
because it uses only APIs and helpers the package already has.
What makes it meaningful is the mutant:
move the `WriteUncompressed` call after the second `ReadMemStats`
and the delta falls to about 3,710,000,
roughly 17% low and below the floor of 3,787,600.
Run that mutant once, record it, and put the call back.

```bash
mise exec -- go test -race -count=1 ./internal/pgo/ -run 'TestRoundsMergeHeapDelta' -v
```

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/rounds_test.go
mise run lint && mise run test && mise run check && mise run prose
git add internal/pgo/rounds_test.go
git commit -m "test(pgo): band a merged profile's heap" -m "<body: nothing watched what a merge retains, and the running profile carries the index serialization attaches to it; the guard measures the merge and one serialization together, banded against the merged result's uncompressed encoding>"
git log --oneline -1 && git status --short
```

---

## 4. The working set is sized by two measured factors

Closes the roadmap bullets beginning *`PGODecodeFactor` describes itself*
and *Three documents disagree about what the factor multiplies*.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `cmd/profgate/main_test.go`,
  `deploy/chart/profgate/templates/_helpers.tpl`, `deploy/chart/profgate/values.yaml`,
  `deploy/chart/profgate/README.md`, `deploy/chart_test.go`,
  `deploy/base/deployment.yaml`, `deploy/base/configmap.yaml`,
  `test/e2e/harness_config_test.go`,
  `docs/configuration.md`, `docs/deployment.md`, `docs/pgo.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles that the chart and the manifests move in this commit,
and that the product is checked before the next task hands it to the runtime.
`internal/config/config.go:526-529` becomes two constants,
each documented as what it was measured against and citing the investigation:

```go
	// PGODecodeRetainFactor is the heap a decoded *profile.Profile holds,
	// against the decompressed bytes it was parsed from. Measured over
	// captured Go CPU profiles and over synthetic profiles swept across stack
	// depth: 3.4 to 9.9, a busy process at 7.2, the densest committed fixture
	// at 8.7. Shallow stacks are the expensive case, because a sample costs
	// about the same heap whatever its depth.
	PGODecodeRetainFactor = 12
	// PGOMergeRetainFactor is the heap the running merged profile holds,
	// against the length of its uncompressed encoding, which is what
	// maxMergedBytes bounds. Measured at 7.2 to 12.7. It is the larger of the
	// two because profile.Merge rebuilds the location, function, and mapping
	// tables rather than reusing the ones it was given, and because
	// serializing a profile attaches an index to it that the Collection then
	// holds for as long as it runs.
	PGOMergeRetainFactor = 16
```

`PGOMemoryBytes` (`:545-556`) becomes the formula of *Container* (`docs/specs/pgo.md:394-399`),
with the exactly bounded buffers written as literals rather than folded into a factor:

```go
	perCollection := int64(l.MaxParallel)*(2+PGODecodeRetainFactor)*l.MaxSampleBytes +
		(1+PGOMergeRetainFactor)*l.MaxMergedBytes
```

Its doc comment says which term is bounded and which is estimated:
the `2` is a sample's compressed body and its decompressed bytes, both bounded by `maxSampleBytes`;
the `1` is the stored copy,
whose length is the ceiling's own quantity give or take what gzip adds to input it cannot compress;
the two factors are what estimates the decoded forms no limit covers.

**The checked product.**
PGO validation (`internal/config/config.go:1189-1219`) gains a rule that forms the same product under overflow checks:
the per-collection term, the multiplication by `maxActiveCollections`, and the addition of `PGOGatewayBaseMemory`.
It returns a validation error when any step wraps,
naming `maxParallel`, `maxSampleBytes`, `maxMergedBytes`, and `maxActiveCollections` with their values.
`PGOMemoryBytes` and `GatewayMemoryBytes` keep their signatures.
The chart needs one more check than it has.
`profgate.pgoMemoryBytes` guards its multiplication (`deploy/chart/profgate/templates/_helpers.tpl:241-243`)
and keeps that shape with the new multipliers,
but `profgate.gatewayMemoryBytes` then adds the base term without checking the sum (`:281-282`),
and `maxActiveCollections` has no upper bound of its own in the chart either (`:179-197`, `:216`).
So a working set that fits can still produce a container sum that does not:
at the shipped sample ceilings, `maxActiveCollections` of `6108397932` gives a working set of
`9223372036720558080`, which passes the product guard,
and a sum with the 512 MiB base of `9223372037257428992`, which is past a signed 64-bit maximum.
The base conversion is a third gap of the same kind.
`profgate.gatewayBaseMemoryBytes` accepts a digit string with `Mi` or `Gi`
and multiplies it by the unit without checking either step (`:257-271`),
so `memoryLimitWithoutPGO: 17179869184Gi` converts to `2^64` and wraps to `0`.
An addition check alone cannot see that: it would add a zero base to a working set that fits.
So the chart gains three checks rather than one, each before the operation it guards:
the digit string is refused when it does not fit an `int64`;
the component is checked against `MaxInt64 / unit` before the multiplication;
and the working set is checked against `MaxInt64 - baseBytes` before the addition.
`deploy/chart_test.go` gains a distinct case for each, because one input cannot exercise two of them:

| input | which check refuses it |
|---|---|
| `memoryLimitWithoutPGO: 99999999999999999999Mi` | the digit string, which is larger than an `int64` holds, before any conversion |
| `memoryLimitWithoutPGO: 17179869184Gi` | the multiplication: the digits fit an `int64` and the product is `2^64` |
| `pgo.limits.maxActiveCollections: 6108397932` at the shipped sample ceilings | the addition: the working set fits and the sum with the base does not |

`6108397931` renders, which is what makes the third row a boundary rather than a bare refusal,
and `6108397932` is refused by `Load` as well as by the render.
Each message names the value and the key it came from, the way the existing refusals do.
The existing base-term test covers invalid units and the explicit-resources bypass
(`deploy/chart_test.go:649-677`), neither of which is an overflow;
the bypass keeps skipping the base derivation entirely, and that is unchanged.

The chart's arithmetic at `_helpers.tpl:240` becomes
`add (mul $parallel 14 $sample) (mul 17 $merged)`,
its overflow message names the same terms with the new multipliers,
and the comment above it (`:227-229`) stops saying eight times a compressed length.
It says instead that 14 is two bounded input buffers plus a decoded profile at 12 times its decompressed bytes,
and 17 is the stored copy plus the running merged profile at 16 times its uncompressed encoding.
`values.yaml:488-497` and `README.md:112-119` restate the same formula and figure, and follow.

- [ ] **Write the test**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestPGOSizing`, `TestGatewayMemoryWithCollectionOff`, and `TestGatewayMemoryFollowsEveryCeiling`, `internal/config/config_test.go:1122-1173` | the working set at the shipped ceilings is `1509949440` and the container `2046820352`; the disabled case's container stays `536870912` while its working set moves with the ceilings, so the independently pinned `1<<30` at `:1137-1144` becomes `1509949440` too; the arithmetic still does not read `pgo.enabled`; raising any of the four ceilings still raises the working set. Three pinned figures are wrong today. `TestPGOSizing` uses `Fatalf`, so it reports its first mismatch and stops — the run is red three times over, not in one report |
| `TestPGOSizingRefusesAnOverflow`, `internal/config/config_test.go`, new | `maxActiveCollections` at a value today's range admits and the new product cannot hold is a `Load` failure naming the four ceilings; a value one step inside the boundary loads and produces a positive figure. `7000000000` is such a value: today's formula gives `7516192768000000000`, which fits, and the new one gives `10569646080000000000`, which wraps to `-7877097993709551616`. So the red run is a `Load` that succeeds where the test wants a refusal, not a negative figure today. The chart is exercised at its own boundary in the same test file's rendering case: `6108397932` overflows the container sum while passing the product guard, and `6108397931` does not |
| `TestRun`'s `validate good with collection on` case, `cmd/profgate/main_test.go:40-49` | the printed lines carry the same two figures |
| `TestChartMemoryLimitIsDerived` and the two other renders, `deploy/chart_test.go:411-445`, `:544`, `:740` | unchanged assertions: they read `cfg.GatewayMemoryBytes()`, so they compare two formulas and are green both before this task and after it. They go red only in the intermediate state where the Go arithmetic has moved and the template has not, which is why the two move in one commit. Neither case pins a byte count of its own, so no figure in `deploy/` needs recomputing |
| `TestChartBaseTermIsTheSameFigureBothWays`, `:632-647` | unchanged and expected to stay green: `memoryLimitWithoutPGO` is 512Mi and the base term does not move |

The red state.
The first command names every sizing test, because `TestPGOSizing` alone leaves two of them unrun.
The second runs the chart tests with the pinned helm;
a run whose output says `SKIP` proves nothing and is not a result.

```bash
mise exec -- go test -race -count=1 ./internal/config/ ./cmd/profgate/ \
  -run 'TestPGOSizing|TestGatewayMemory|TestRun'
mise exec -- go test -race -count=1 ./deploy/ -run 'TestChartMemoryLimitIsDerived' -v
```

- [ ] **Follow the figure everywhere it is written down**

- `deploy/base/deployment.yaml:53-59`: the comment's `1536Mi` becomes `1952Mi`.
  The `512Mi` literal does not move.
- `deploy/base/configmap.yaml:60-63`: the same figure, and `1Gi working set` becomes `1440Mi working set`.
- `test/e2e/harness_config_test.go:206-210`: the literal becomes `"1952Mi"`,
  and its comment's `1Gi` becomes `1440Mi`.
- `docs/configuration.md:439-470`: the formula; the sentence explaining the factor, which becomes two;
  the `What it multiplies` column — `maxMergedBytes` is no longer "twice"
  but once for the stored copy and sixteen times for the running merged profile;
  the worked example; and the `maxActiveCollections: 2` example,
  which becomes 2880 MiB of working set and `3392Mi` of container.
  It gains the upgrade note: `maxMergedBytes` now bounds the encoding before compression,
  and every derived limit rises wherever collection is enabled,
  while a disabled installation and an explicit `resources.limits` keep what they had.
- `docs/deployment.md:390-400`: the formula and `the working set is 1Gi and the limit is 1536Mi`.
- `docs/pgo.md:361-362`: the container figure the guide quotes.

- [ ] **Say what moved**

`CHANGELOG.md`, `### Changed`:
**Derived container memory limits rise, and an operator recalculates for their own ceilings.**
The working set was sized by one constant standing for a decoded profile against its encoded length,
and that constant was a value inside the measured range rather than a ceiling over it.
It is now two: 12 for a decoded profile against the decompressed bytes it was parsed from,
and 16 for the running merged profile against the length of its uncompressed encoding —
which is larger partly because serializing a profile attaches an index the Collection then holds.
At the shipped defaults the limit rises from 1536 MiB to 1952 MiB.
Every derived limit rises where collection is enabled,
by `maxActiveCollections × (6 × maxParallel × maxSampleBytes + maxMergedBytes)`.
An installation with `pgo.enabled` false, and one that writes an explicit `resources.limits`,
keep the figure they had.
A configuration whose ceilings multiply out past a 64-bit byte count is now refused at startup, naming them.
The Helm chart renders the new figure and the kustomize base's comments name it;
an explicit `resources.limits` in the chart still overrides the derivation, while the implicit request follows it.

- [ ] **Validate and commit**

```bash
semlf check internal/config/config.go docs/configuration.md docs/deployment.md docs/pgo.md \
  deploy/chart/profgate/README.md CHANGELOG.md
mise run lint && mise run test && mise run check && mise run prose
git add internal/config/config.go internal/config/config_test.go cmd/profgate/main_test.go \
  deploy/chart/profgate/templates/_helpers.tpl deploy/chart/profgate/values.yaml deploy/chart/profgate/README.md \
  deploy/chart_test.go deploy/base/deployment.yaml deploy/base/configmap.yaml test/e2e/harness_config_test.go \
  docs/configuration.md docs/deployment.md docs/pgo.md CHANGELOG.md
git commit -m "fix(config): size the working set by measurement" -m "<body: one constant stood for both decoded forms and sat inside the measured range; two measured factors replace it, each against the quantity it multiplies, the product is checked, and the chart, the manifests, and the guides follow>"
git log --oneline -1 && git status --short
```

---

## 5. A collecting process holds its own soft memory limit

Closes the roadmap bullet beginning *The container is sized from what a decode retains*.

**Files:**
- Add: `internal/config/memlimit.go`, `internal/config/memlimit_test.go`
- Modify: `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`,
  `docs/deployment.md`, `docs/configuration.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the filesystem root, the full resolution algorithm and its outcomes,
the split between the two packages, the rounding,
that no test touches the real limit or the real cgroup,
and that a limit already in force is never raised.
`internal/config/memlimit.go` carries:

```go
// SoftMemoryLimit is the GOMEMLIMIT a collecting process sets: 90% of the
// smaller of the container figure this configuration derives and the memory
// limit of the process's own cgroup. Reading the cgroup is what makes an
// explicitly lowered container limit count. A process that collects nothing
// sets no limit: the base term is asserted rather than measured, and a soft
// limit over an unmeasured figure would change how an installation behaves
// that decodes nothing.
func (c *Config) SoftMemoryLimit() (int64, bool)
```

calling an unexported `softMemoryLimit(root string)` so a test can pass a `t.TempDir()`;
production passes `"/"`.
The cgroup read is `cgroupMemoryLimit(root string) (int64, bool)`,
implementing the numbered resolution of *Decisions* exactly:
membership first, then a matching mount, then the mount root subtracted from the membership path,
octal escapes decoded, and every failure meaning no cgroup limit rather than a startup error.

`cmd/profgate/serve.go` applies it once at startup, beside the other startup records
(`cmd/profgate/serve.go:207` is where the configuration is already being spent),
through the seam so no test reaches the runtime:

```go
	if limit, ok := deps.softLimit()(cfg); ok {
		setLimit := deps.memoryLimitSetter()
		if current := setLimit(-1); limit < current {
			setLimit(limit)
			logger.Info("soft memory limit set", "bytes", limit)
		}
	}
```

`debug.SetMemoryLimit(-1)` returns the limit in force without changing it,
and `math.MaxInt64` when nothing set one,
so the comparison costs nothing in the ordinary case and never raises a `GOMEMLIMIT` an operator set.

- [ ] **Write the test**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestSoftMemoryLimit`, `internal/config/memlimit_test.go`, new | a table against fixture trees under `t.TempDir()`: cgroup v2 whose `memory.max` is below the derived figure returns 90% of the cgroup value; v2 `max`, v2 above the derived figure, a tree with no cgroup file, a file that cannot be read, and a file holding `not a number` all return 90% of the derived figure; v1 `memory.limit_in_bytes` below it returns 90% of that; v1 at `1 << 62` returns 90% of the derived figure, that sentinel meaning unlimited; a v2 reading of `1 << 62` is a real limit and binds; a negative or zero reading returns 90% of the derived figure; and `pgo.enabled: false` returns `ok` false whatever the tree holds. One row pins the rounding: at the shipped container figure the answer is exactly `1842138315`. It does not exist today: the first run is the compile |
| `TestCgroupMemoryLimitResolution`, same file, new | the resolution itself: a mount whose root is `/tenant` and whose membership is `/tenant/container` reads `<mountpoint>/container/memory.max`, not `<mountpoint>/tenant/container/memory.max`; a mount rooted at `/tenant` does not match a membership of `/tenantry`; a membership equal to the mount root, both being `/`, reads the mount point itself, while a membership of `/` under a mount rooted at `/child` does not match at all; a membership of `/../../init` skips the mount rooted at `/` and resolves against the one rooted at `/../..`; a parent cgroup visible at the conventional path is *not* read when the membership names a child; a hybrid tree with a `0::` line and a version 1 `memory` line takes the version 1 one; a mount point carrying `\040` is decoded; and a membership no candidate mount admits yields no limit |
| `TestServeSetsTheSoftMemoryLimit`, `cmd/profgate/serve_test.go`, new | through both fakes, so neither the runtime limit nor the runner's cgroup is touched: the fake `softMemoryLimit` returns `1842138315` and `true`, and the fake setter records what it was called with. With collection on the setter is called with `1842138315`, the soft figure and not the container figure it is derived from; with collection off the fake returns `false` and the setter is not called at all; where the recorded current limit is already below `1842138315` it is read and not written |
| `startGatewayWith`, `cmd/profgate/serve_test.go:493-544` | installs **both** fakes for every test that goes through it, so no existing case reaches `debug.SetMemoryLimit` or the runner's own cgroup. Without this change the tests at `:1914-1918` already would |

The red state:

```bash
mise exec -- go test -race -count=1 ./internal/config/ ./cmd/profgate/ -run 'SoftMemoryLimit|CgroupMemoryLimit'
```

- [ ] **Document what it buys, and what it does not**

`docs/deployment.md`, in the sizing section after the derived limit:
a collecting process sets `GOMEMLIMIT` at startup to 90% of the smaller of the derived figure
and the memory limit of its own cgroup,
so an explicitly lowered `resources.limits.memory` counts;
a process with `pgo.enabled: false` sets none;
and a `GOMEMLIMIT` already set in the environment is never raised by it.
It says plainly what the limit buys:
the runtime collects more often as the heap approaches it,
which trades CPU for a smaller transient
and can slow both collection and the interactive path, because collection shares the process.
It does not prevent an out-of-memory kill —
Go may exceed a soft limit rather than collect without end,
and a live heap larger than the container is not reclaimable at any collection rate.
`docs/configuration.md` gains one sentence in the sizing section pointing at it.

`CHANGELOG.md`, `### Added`:
**A collecting process sets its own soft memory limit.**
The container was sized by what a decode retains, and the peak is higher:
a parse allocates about half as much again on the way and a merge about twice,
and nothing set `GOMEMLIMIT`, so the runtime targeted twice the live heap.
A process with `pgo.enabled` true now sets it at startup to 90% of the smaller of two figures,
the derived container figure and its own cgroup limit;
a process that collects nothing sets none,
and a `GOMEMLIMIT` already in the environment is never raised.
This narrows the window in which a transient peak kills a correctly configured process.
It does not make an undersized one safe.

- [ ] **Validate and commit**

```bash
semlf check internal/config/memlimit.go cmd/profgate/serve.go docs/deployment.md docs/configuration.md CHANGELOG.md
mise run lint && mise run test && mise run check && mise run prose
git add internal/config/memlimit.go internal/config/memlimit_test.go cmd/profgate/serve.go cmd/profgate/serve_test.go \
  docs/deployment.md docs/configuration.md CHANGELOG.md
git commit -m "feat(pgo): set a soft limit when collecting" -m "<body: the container was sized by a live heap and nothing bounded the transient above it; a collecting process sets GOMEMLIMIT from the smaller of the derived figure and the limit of its own cgroup>"
git log --oneline -1 && git status --short
```

---

## 6. Close the plan

**Files:**
- Modify: `docs/plans/decoded-working-set.md`, `docs/plans/roadmap.md`

Line 3 becomes `**Status:** Done` and line 4 `**Outcome:** pull request #<n> …`,
naming the pull request that carries the five tasks above.
In the same commit the roadmap's *Size the decoder against what it actually retains* `Shipped:` line
(`docs/plans/roadmap.md:447`) names that pull request in place of `not built yet`,
and the first bullet of *Close the gates that do not run* (`:320-327`) is ticked,
that item's `Shipped:` line naming this pull request beside the one it already carries —
that item's own text says the lifetime and the bound are an item of their own
and the skip comes out with them, and it has.
The pull request is named rather than a commit because the merge rebases this branch onto `main`
and rewrites every hash on it, while the number is the same before and after;
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
admits a pull request there for that reason,
and `check_status` in [`check-repo.py`](../../scripts/check-repo.py)
requires `**Outcome:** ` followed by text on line 4.
This commit does not delete the plan.
The deletion is the next commit that touches the file, after the merge,
the protocol [`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records;
it deletes this file and rewrites every link that cited it, which `check_links` enforces,
and changes nothing else.
`grep -rn decoded-working-set --include='*.md' .` finds the links.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/decoded-working-set.md docs/plans/roadmap.md
mise run lint && mise run test && mise run check && mise run prose
git add docs/plans/decoded-working-set.md docs/plans/roadmap.md
git commit -m "docs: close the decoded working set plan" -m "<body: the item's six bullets are done and its Shipped line names the pull request; the unrun heap guard now runs, so the gate item's first bullet is ticked too>"
git log --oneline -1 && git status --short
```

---

## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite:

```bash
mise run test:e2e
```

It is required.
[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
lists `internal/pgo` and `deploy/` among the eight packages
that need the suite on the `current` lane before a pull request,
and this plan changes both.
What the suite proves here is narrow:
a PGO-enabled gateway starts and completes a Collection under the new container limit
and the new soft memory limit,
which is the one place `SoftMemoryLimit` reads a real cgroup rather than a fixture tree,
and the one place the moved `maxMergedBytes` ceiling meets a real profile.
It proves nothing about the bands, the cgroup reader's other outcomes, or the chart's arithmetic;
the unit tests above and `deploy/chart_test.go` are the evidence for those.
`PROFGATE_E2E_KEEP=1` saves about four minutes of setup between runs.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **A heap band is a measurement, and a measurement can move with the toolchain.**
  The figures above are Go 1.26.7 on linux/amd64,
  taken in this repository's working copy against the fixtures as committed.
  A toolchain bump that changes what a `profile.Sample` costs moves every band at once,
  and the guards will say so by failing — which is the point,
  and also the maintenance cost the design accepted.
  A band that fails after a toolchain bump is re-measured
  and the new centre committed with a reason, never widened.
- **The soft memory limit does not prevent an out-of-memory kill.**
  Go may exceed a soft limit rather than collect without end,
  and a live heap larger than the container is not reclaimable at any collection rate.
  What the limit narrows is the window in which the transient above the working set kills a process,
  where that process was configured correctly.
  The peak itself remains unmeasured;
  *Container* says so in those words (`docs/specs/pgo.md:466-468`),
  and the investigation says the same and leaves it unmeasured.
  Nothing in this plan establishes a peak figure.
- **The end-to-end node's headroom is checked, not proved.**
  `test/e2e` patches the gateway Deployment to the figure `config validate` prints,
  which rises from `1536Mi` to `1952Mi`,
  and a Pod the node cannot fit stays `Pending`,
  failing every PGO scenario for a reason that looks nothing like this change.
  The kind node this plan was written against reports 65403156Ki allocatable,
  so the raise is not close to a bound there.
  That is one node on one machine.
  A CI runner or a smaller workstation is where this would be met first,
  and the implementer confirms it there rather than assuming it.
- **The container figure rises by a quarter at the shipped defaults.**
  [`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md)
  reopens the collector separation
  when the derived limit becomes what bounds how many replicas a node or a quota admits.
  1536 MiB to 1952 MiB does not reach that,
  and *Container* is where the next move is checked (`docs/specs/pgo.md:502-504`).
  This plan does not reopen it.
- **A Collection that used to complete can now fail `merged_too_large`.**
  The ceiling reads an encoding two to thirty-three times larger than the one it read before,
  at the same numeric value.
  How many real Collections that affects is not measured here:
  the investigation measures fixtures, not fleets.
  The changelog carries it and no configuration is migrated automatically,
  because the case nothing has counted is the one such a migration would be for,
  and renaming or rescaling the key would fail every existing configuration over it.
- **`cmd/profgate` gains a process-global side effect, and no unit test exercises it.**
  `debug.SetMemoryLimit` has no scope, and `go test` runs a package's tests in one process.
  A test that set a real limit would change how hard the runtime collects,
  for the rest of that binary's run.
  The seam is what keeps that from happening,
  which means nothing in the unit suite proves the real call works —
  the end-to-end suite is the only place it runs,
  and it observes the effect only as a gateway that starts.
- **The cgroup reader is specified against documented layouts, not against every kernel.**
  The resolution above follows what the Go runtime's own reader does.
  Its rows are fixture trees,
  so no test proves the reader against a real hybrid or namespaced host.
  Every failure it can hit degrades to the derived figure rather than to a startup error,
  so an unproven layout keeps the process starting.
  That is not the same as safe:
  falling back means missing a cgroup limit smaller than the derived figure,
  which is the case the cgroup term exists for,
  and *Container* already says a soft limit does not make an undersized installation safe.

---

## Self-Review

- Every figure in this plan is either quoted from the accepted spec or measured,
  and each measured one names the fixture and the quantity it is against.
  The 1440 MiB working set is the arithmetic of *Container* at the shipped ceilings,
  and matches what *Presets* publishes for `small`, which is what the shipped defaults are.
  The 1952 MiB container is that working set over the gateway's own 512 MiB base,
  and matches no preset row, because the preset's collector limit is 1696 MiB over a 256 MiB base.
- The 256 MiB `collectorBaseMemory` is not used anywhere in this plan.
- Every task leaves the tree green.
  Task 4 moves the Go arithmetic and the chart's in one commit because two committed tests compare them;
  splitting them would leave a red `deploy/` package between two commits.
- The fixture the guards read is committed by the change that adds this plan,
  so no task references a file a later task adds,
  and every centre above is checkable against a file in the tree.
- No task adds a configuration key, a route, a chart value, or a permission,
  and `grep -rn PGODecodeFactor .` after task 4 should find only the changelog's description of what it was.
- The plan does not reopen a settled design decision.
  Where a figure could have been chosen differently — the factors at 12 and 16,
  `maxMergedBytes` keeping its name, the guards banding their own measurement —
  the spec settles it and *Decisions* points at the text rather than re-arguing it.
