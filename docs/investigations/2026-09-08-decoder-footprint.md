# What a decoded CPU profile costs

Date: 2026-09-08.
Scope: `config.PGODecodeFactor`, the constant that bounds `TestRoundsDecodeHeapDelta`
and sizes `Config.PGOMemoryBytes`.
Toolchain: Go 1.26.7 on linux/amd64, `GOMAXPROCS` 32.

Method: one isolated process per measurement.
`runtime.GC()` and `runtime.ReadMemStats` on both sides of `profile.ParseData` or `profile.Merge`,
reporting the `HeapAlloc` delta as what the call *retains*
and the `TotalAlloc` delta as what it *allocates* on the way.
Every figure is the median of at least three runs;
where a spread is given it is the full observed range.

Reproducing it needs four small programs, which are working notes and not in the tree:
one that parses a profile and reports the delta under a named set of live variables;
one that does the same around `profile.Merge` and divides by the merged result's serialized length;
one that writes a synthetic profile of a chosen sample count, stack depth, and location-pool size;
and one that burns CPU across eight goroutines and writes its own profile.
Each is thirty to sixty lines against `github.com/google/pprof` alone.
What replaces them is the pair of guards this measurement argues for:
once those are committed, running the package's tests is the reproduction.

## What is live decides what the delta means

The decompressed input is allocated before the first `ReadMemStats`, so it is already in the baseline.
Holding it therefore adds nothing to the delta,
and dropping it lets the collection between the two reads subtract its whole length.
The delta with both live is the decoded profile's own retained heap;
the delta with the input dropped is that heap minus one input,
which is not a quantity anything wants.

Over `internal/pgo/testdata/cpu-heap.pprof`, 516,906 bytes decompressed:

| held live across the measurement | delta | against the input |
|---|---|---|
| the decoded profile and the input | 4,466,912 – 4,472,248 | 8.642 – 8.652 |
| the decoded profile alone | 3,942,624 – 3,947,960 | 7.627 – 7.638 |
| the input alone | 10,624 | 0.021 |
| neither | −508,344 – −518,984 | −0.98 – −1.00 |

The last two rows are the control.
The input alone moves the delta by nothing because it predates the first read;
dropping both moves it by exactly minus one input.
So the first row is the decoded profile's footprint,
and the second is the first row less the third row's subject.

`TestRoundsDecodeHeapDelta` takes the second row and compares it against a bound written for the first.
That is what lets it pass: it reads one whole input below the quantity its comparand was chosen for.

A caution for anyone reproducing this.
Naming the input anywhere after the second `ReadMemStats` keeps it live and collapses the two rows into one:
a `len(plain)` in the output line does it,
and so does a `runtime.KeepAlive(plain)` inside a branch that is not taken.
The variable has to be nil before the final `runtime.GC()` for the difference to exist at all,
and the fourth row is the control that proves it does.

## Both sample buffers are live during the decode

`internal/pgo/rounds.go:482-490` reads `body := sink.Bytes()`, calls `decodeSample(body)`,
and reads `len(body)` after it returns.
Inside, `:501-517` decompresses into `plain` through an `io.LimitReader`.
Three things are live while `ParseData` runs:
the compressed body, the decompressed bytes, and the growing decoded profile.
`maxSampleBytes` bounds the first two exactly, one each, and does not bound the third.

## The race detector does not move the delta enough to matter

The guard skips under `-race`, saying the detector's allocator accounting makes the delta meaningless.

| body | without `-race` | under `-race` |
|---|---|---|
| `cpu-heap.pprof`, both lifetimes held | 4,466,912 – 4,472,248 | 4,472,792 – 4,478,368 |
| a synthetic profile of 20,000 single-frame samples | ratio 13.485 | ratio 13.511 – 13.533 |

The ranges do not overlap, so the effect is small and real rather than absent:
on this toolchain and these bodies the detector raises the retained ratio by under half a percent.
That is far inside any band a regression guard would use,
and it is not what keeps the guard from running.

## The ratio tracks sample density, not length

The committed fixtures disagree with each other, and the disagreement is not noise:

| fixture | decompressed | samples | mean depth | bytes per sample | retained | allocated |
|---|---|---|---|---|---|---|
| `cpu-heap.pprof` | 516,906 | 20,000 | 8.0 | 25.8 | 8.652 | 12.69 |
| `cpu-large.pprof` | 32,461 | 400 | 1.0 | 81.2 | 5.374 | 8.46 |
| `cpu-a.pprof` | 976 | 12 | — | 81.3 | ~11.2 | — |
| `cpu-b.pprof` | 735 | 9 | — | 81.7 | ~13.4 | — |
| `alloc.pprof` | 488 | 5 | — | 97.6 | ~16.6 | — |

The bottom three are fixed overhead rather than signal:
a few hundred bytes of input against a `profile.Profile` that costs several kilobytes empty.
They cannot carry a per-byte ratio, and the two that can differ by 1.6 times.

Synthetic profiles isolate the reason.
Twenty thousand samples over a pool of two thousand distinct locations, sweeping the stack depth:

| stack depth | encoded | bytes per sample | retained | allocated |
|---|---|---|---|---|
| 32 | 1,553,700 | 77.7 | 5.576 | 9.66 |
| 24 | 1,243,860 | 62.2 | 5.942 | 10.01 |
| 16 | 934,020 | 46.7 | 6.539 | 10.59 |
| 8 | 624,180 | 31.2 | 7.737 | 11.74 |
| 4 | 469,260 | 23.5 | 8.941 | 12.91 |
| 3 | 430,530 | 21.5 | 9.365 | 13.32 |
| 2 | 391,800 | 19.6 | 9.893 | 14.24 |

The ratio is flat in the sample count:
50,000 and 200,000 samples of one shape give 14.682 and 14.676.
It moves only with what a sample costs to encode.
A `profile.Sample` costs about the same heap whatever its depth,
so the fewer bytes a sample takes on the wire, the more heap each byte buys.
Shallow stacks are the expensive case, not the cheap one.

A single-frame sweep reaches 13.485 to 14.682.
The Go runtime does not emit that:
a CPU profile records the whole stack, so its shallowest samples are three or four frames,
where the sweep reads 9.365 and 8.941.

## Real profiles, and where they sit

Captured from this repository's own test binaries with `-cpuprofile`,
and from a program that burns CPU for 25 seconds across eight goroutines doing JSON, gzip, sorting, and hashing.
That last is 200 seconds of CPU, which is the shape a busy service's profile has:

| profile | samples | mean depth | bytes per sample | retained | allocated |
|---|---|---|---|---|---|
| CPU, from an input-bound test binary | 68 | 8.2 | 492.9 | 3.393 | 5.72 |
| CPU, from a second test binary | 381 | 9.7 | 241.4 | 3.924 | 6.41 |
| CPU, 200 seconds of a busy process | 7,818 | 11.1 | 45.8 | 7.229 | 11.34 |

Among CPU profiles the measured range is 3.4 to 9.9,
a real busy one sits at 7.2,
and the densest committed fixture at 8.7.
`PGODecodeFactor` is `8`: a value inside that range, not a ceiling over it.

## The merge ceiling and the merge factor measure different encodings

`internal/pgo/rounds.go:99` writes through `profile.Write`, which gzips.
`serializedSize` measures that output (`:418-425`),
`:392` checks it against `maxMergedBytes` after every absorbed sample,
and `:539-545` checks it again at completion and stores the same buffer.
So `maxMergedBytes` bounds the gzipped encoding.

A merged profile's heap against the two candidate denominators:

| merged from | distinct samples | uncompressed | against it | gzipped | against it |
|---|---|---|---|---|---|
| one 200-second busy profile | 7,818 | 358,318 | 10.362 | 125,652 | 29.55 |
| four 8-second busy profiles | 9,155 | 408,527 | 10.557 | 140,290 | 30.74 |
| `cpu-large.pprof` | 400 | 32,461 | 6.642 | 12,589 | 17.13 |
| a synthetic profile at depth 2 | 2,000 | 124,086 | 8.486 | 34,955 | 30.12 |
| a synthetic profile at depth 8 | 2,000 | 147,324 | 7.763 | 38,748 | 29.52 |
| a synthetic profile at depth 32 | 2,000 | 240,276 | 6.358 | 42,859 | 35.64 |

A merged profile is heavier per uncompressed byte than a parsed one,
6.4 to 10.6 against 3.4 to 9.9,
because `profile.Merge` rebuilds the location, function, and mapping tables rather than reusing the ones it was given.

## What a profile compresses to is a property of the profile, so the gzipped ceiling bounds little

The two pairs of columns above differ by the compression ratio, which varies by an order of magnitude:

| body | uncompressed | gzipped | ratio |
|---|---|---|---|
| `cpu-heap.pprof` | 516,906 | 15,514 | 33.3 |
| a synthetic profile at depth 32 | 240,276 | 42,859 | 5.6 |
| CPU, 200 seconds of a busy process | 358,318 | 125,652 | 2.9 |
| `cpu-large.pprof` | 32,461 | 12,589 | 2.6 |

`cpu-heap.pprof` is a repetition — its 20,000 samples merge to 400 distinct ones —
and repetition is what gzip is best at.
Nothing stops a real profile from being repetitive:
a process spending all its time in one loop produces one.

So a ceiling on the gzipped size says little about what the decoded profile costs.
Deflate has a theoretical ceiling of about a thousand to one,
so a multiplier over the gzipped ceiling is a bound in principle,
and two orders of magnitude too loose to be one in practice.
What it is instead is a coincidence that holds for typical profiles and fails for repetitive ones,
which are exactly what a process stuck in one loop produces.
The sample side does not have this problem.
[`pgo.md`](../specs/pgo.md) *Rounds* puts a limit on the compressed body
and a second one on the decompressed bytes,
and refuses nested gzip so the decoder is never handed an unbounded expansion.
`maxSampleBytes` bounds both encodings; `maxMergedBytes` bounds one, and it is the wrong one.

## Retained is not peak, and peak is not measured here

Everything above is what a call retains once a collection has run.
A parse allocates about 1.4 to 1.5 times that on the way, and a merge about twice.
Allocated bytes are an upper bound rather than the peak,
because the collector reclaims some of them while the call is still running,
so the peak sits between the two columns and no measurement here says where.

Three things in the code push it toward the upper end.
`internal/pgo/rounds.go:373` holds the old merged profile, the incoming one,
and the new merged result at the same moment.
`:536` calls `Compact()` before serializing, so a second merged profile is live beside the first.
And `maxParallel` samples decode concurrently.

There is no `GOMEMLIMIT` anywhere in the repository,
so the collector targets twice the live heap by default:
a container sized at a base plus a retained working set can be killed
while the live heap is exactly that working set.

## Three documents disagree about what the factor multiplies

- `deploy/chart/profgate/values.yaml:496-497` and `templates/_helpers.tpl:230-231`:
  "a profile occupies about eight times its compressed length once decoded".
- `internal/config/config.go:526-528`: "against its encoded length:
  two buffers of input plus about six times that in decoded structures".
- `internal/pgo/rounds_test.go:852-854`, contradicting the chart:
  the factor "multiplies `maxSampleBytes`, which bounds the decompressed body …
  and not against the gzipped wire form".

The test's comment is the one that matches the code.
Measured against the decompressed body,
the decoded structures alone are 8.7 times the input on `cpu-heap.pprof` rather than six,
and the two input buffers are one each rather than one term of two.
