# A Watch Cut Under a Live Connection Moves the Store Generation

**Status:** Done
**Outcome:** pull request #18 on `fix/pgo-store-generation`.

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit, and checkboxes (`- [ ]`) track progress.
> Every task is test-first, and each begins by adding the declarations its tests name,
> so its first run fails on assertions rather than on compilation.
> The accepted specs outrank this plan: where the two differ the spec wins and the plan is the bug.

**Goal:** close the barrier lie in the watched PGO caches.
A watch whose subscription closes while the NATS connection stays up moves the store generation,
so every watch re-opens under the new one and every watched cache is rebuilt from its replay.
A session's cache reads take the generation it bound and answer `503 pgo_unavailable` when the caches moved past it,
`Caches.Run` starts each attempt from cleared flags, and `profgate_pgo_synced` makes a shut barrier visible.

**Architecture:** no process, loop, or key moves.
`internal/natskv` gains the second move and the callback that reports it,
`internal/pgo` a generation on three cache reads and a per-attempt flag clear,
`internal/httpapi` the refusal those reads now produce,
`internal/metrics` one gauge a scrape reads through a function `cmd/profgate` registers, and the chart one more alert.
Every other package gains nothing; no Go module is added and no permission moves.

**Spec:** [`docs/specs/pgo.md`](../specs/pgo.md), `Accepted`.
*The seam* is the design of record for the move, the session-bound read, and the per-attempt clear;
*Logging* gives the two record kinds, *Health* what stays green, *Metrics* the gauge and the alert,
and *Unit* and *Failure Scenarios* the cases and the row for a bucket deleted or recreated while running.
Ordered by [`docs/plans/roadmap.md`](roadmap.md) item 12, second bullet;
rules in force under [`.agents/rules/`](../../.agents/rules/).

**Global constraints.**
The permission invariant does not move: no task edits `internal/k8s` or the NATS account fragment,
and the store generation stays the only axis — no cache epoch, no second counter, no flag cleared by re-opening.
A signature change and its call sites land in one commit, and every message states the current fact.
Markdown prose uses semantic line breaks, every task validates before it commits,
and commit headers are Conventional Commits under 50 characters with no trailer of any kind.

## A closed watcher moves the generation

**Files:** `internal/natskv/`: `natskv.go`, `client.go`, `preflight.go`, and the three test files beside them.

**Declare first.**
| Declaration | Where | What it is |
|---|---|---|
| `Options.OnGenerationMove func()`, mirrored by `connectConfig.onGenerationMove` and `client.onGenerationMove` | `natskv.go:122-132`, `client.go:46-49` and `:72-73`, carried through `preflight.go:46` | reports either move; `OnConnectionChange` beside it keeps connection state alone |
| `func (c *client) moveGeneration(reason string, attrs ...any)` | `client.go:231` | the body of `bumpGeneration`, which becomes one of its two callers |
| `func (c *client) markSynced(ws *watchState, gen uint64) bool` | beside `Synced` (`client.go:251`) | sets the marker under `c.mu`, then reports whether this marker made `Synced(gen)` true |
| `client.reopening` and `client.reopenFailing`, two bools guarded by `c.mu` | beside `watches` (`client.go:75-78`) | the re-open state of the whole client, so one cut that took down several watches writes one record |
| `testWatchOpened func(prefix string, w jetstream.KeyWatcher)` and `testHoldReopen func(prefix string)` | `client.go:86-98`, following `testDelayPostCheck` | one hands a test the live watcher, whose `Stop()` drives `removeSub`, the closed handler, and `!ok` in `consumeWatcher`; the other blocks each re-open so a test can observe the gap |

**What changes.**
`consumeWatcher` (`client.go:618`) takes the `prefix` its `runWatch` caller holds,
and its `!ok` branch calls `moveGeneration` before returning `false`,
so `genState()` (`client.go:280`) reads the new generation and `runWatch` re-opens under it.
`moveGeneration` runs `onGenerationMove` after releasing `c.mu`, and nothing clears a marker:
`watchState.syncedUnder` (`client.go:323`) already goes false when the generation moves.
The marker branch calls `markSynced` rather than `ws.setMarker` directly,
which keeps the existing `c.mu` before `ws.mu` order and fires the completion record once.
Logging is on a state change only, and the two re-open records read client-wide state rather than one watch's:
`moveGeneration` writes one record naming what moved it and sets `reopening`,
the first failed open after that sets `reopenFailing`,
and its one record carries the error and the prefix that reached it, whichever watch that was.
`markSynced` writes the record that every watch is open and has replayed again — clearing both bools —
when the marker it just set makes `Synced(gen)` true while `reopening` stands.
The retries at `watchReopenDelay` (`:28`) write nothing, a second watch failing its open writes nothing,
and a process that has never re-opened writes nothing on its first replay.

- [x] **Write the tests, run them, and record which subtests were red**

`client_test.go`, as subtests of `TestGeneration` (`:312`), beside the restart subtest of `TestWatch` (`:272`):

| Subtest | Proves | Wrong implementation it catches |
|---|---|---|
| a watch cut under a live connection moves the generation | with the re-open held: `Generation()` moved, `Synced(oldGen)` is false, `Connected()` is still true | today's re-open under the generation the cut watcher was already holding |
| the re-opened watch replays under the new generation | releasing the hold: the replay and a fresh marker carry the new generation, and `Synced(newGen)` becomes true | a re-open tagging its replay with the generation captured before the move |
| a key deleted through the fixture's admin connection during the gap | is absent from the replay that follows | a cache the replay patches rather than rebuilds |
| a re-open that keeps failing, with `PROFGATE_JOBS` deleted, which cuts the three watches over it at once | exactly one warn record over several retry intervals, and one record when the bucket returns and the watches have replayed | a record per 50 ms retry, and a record per cut watch from a `failing bool` held by each `runWatch` |

That last subtest is the one that proves the two re-open records are client-wide:
three watches go down together,
and a per-watch flag would write three failure records where re-opening began failing once.
The move record is per move and stays that way, so that same cut writes three of those,
one for each closed subscription, which is what *Logging* asks for.
`fixtures_test.go:144` gains `captureLogger()`, beside the discarding `testLogger()`,
because `internal/natskv` has no log capture today and every test there discards through `testLogger()`;
`logCapture` and its `slog.Handler` in `internal/pgo/fixtures_test.go:1260-1276` are the shape to follow.
In `preflight_test.go`, beside `OnConnectionChange sees connect, outage, reconnect` (`:331`),
`OnGenerationMove` runs for a disconnect and for a watch cut while `OnConnectionChange` records the outage alone.

- [x] **Land the move, the callback, and the two records, then validate and commit**

```bash
mise exec -- go test -race ./internal/natskv/ && mise run lint && mise run test && mise run check
git add internal/natskv/ && git commit -m "fix(natskv): move the generation on a watch cut"
```

Body: a watch whose subscription closes under a live connection has the same unknown gap behind it as a disconnect,
so it moves the store generation, every watch re-opens under the new one, and `OnGenerationMove` reports either move.

## An attempt starts from cleared flags

**Files:** `internal/pgo/caches.go`, `internal/pgo/caches_test.go`.

**Declare first:** `func (c *Caches) startAttempt(client natskv.Client)`,
which records the connection this attempt fills the caches from and sets all four `c.synced` entries false under `c.mu`.
`Caches.Run` (`caches.go:287`) calls it on the line between `gen := client.Generation()` (`:288`)
and `stores, err := client.View(gen)` (`:289`),
so an attempt that never opens a watch starts from cleared flags too —
the view failure, and every open failure after it.
`c.gen` is left alone because `apply` (`:343-351`) still resets a cache when its generation differs.
An attempt that opens `overrides` and `jobs` and then fails on `active` (`:305-312`) leaves those two flags set,
and the next attempt overlays a replay `apply` never resets,
so `Caches.Synced(gen)` turns true as the `active` and `slots` markers land, with two replays queued.

- [x] **Write the tests, run them, and record which subtests were red**

`TestCachesRunClearsSyncedFlagsPerAttempt`, new, over the `watchedCaches` fixture (`caches_test.go:19-44`):
fail the `active` open with `wc.failWatch(activePrefix)` (`:47`),
wait until the first attempt applies the `overrides` and `jobs` markers,
lift the failure (`:59`), install an `applyGate` (`caches.go:192-195`) holding those two prefixes,
then start the second attempt, wait for the `active` and `slots` markers, and assert `Caches.Synced(gen)` is false.
Today it is true, because the first attempt's flags survived, and releasing the gate then turns it true.
`TestCachesRunFailedWatchOpen` (`caches_test.go:97`) gains one assertion,
that `wc.caches.Synced(wc.client.Generation())` is false between the two attempts.
`TestCachesRunViewFailure` (`caches_test.go:153`) gains one that pins the placement:
set the four flags through a first attempt that reaches `Synced(gen)` and cancel it,
then run the attempt whose view fails,
and `wc.caches.Synced(wc.client.Generation())` is false afterwards.
A `startAttempt` placed below the view failure's early return leaves that assertion red.
The cut itself is proved at the seam.
`TestGeneration` in `internal/natskv` cuts a live watcher through `client.testWatchOpened`,
an unexported field of an unexported type no test outside that package reaches,
and watches the generation move, the watches re-open, and their replay carry the new generation.
The rebuild that cut must cause is proved here against the same contract:
`TestCachesRebuildOnAWatchCut` runs `Caches.Run` over a connection that delivers exactly what the seam promises —
the generation moves, `OnGenerationMove` reports it, and every watch replays under the new generation —
so a key deleted while the watches were down is absent once the barrier is open again,
although no tombstone was ever delivered for it.
A bucket deleted or recreated under a running process is still uncovered,
and whether a re-open binds to a stream recreated under the handle the client holds is unverified.

- [x] **Clear the flags, then validate and commit**

```bash
mise exec -- go test -race ./internal/pgo/ && mise run lint && mise run test && mise run check
git add internal/pgo/ && git commit -m "fix(pgo): clear the synced flags per attempt"
```

Body: an attempt that failed partway through the four watches left the flags of the watches it had opened standing,
so the next attempt reported a completed replay over caches that were still filling.

## A cache read carries the generation of the session that took it

**Files:** `internal/pgo/` (`caches.go`, `runtime.go`, `scheduler.go`, `runtime_test.go`) and `internal/httpapi/` (`pgo_collections.go`, `fixtures_test.go`, `pgo_test.go`).

**Declare first.** `Session` (`runtime.go:99-107`) gains a `gen uint64` field, taken from `Session()` (`:112-137`),
and `Caches` gains `func (c *Caches) syncedLocked(gen uint64, kinds ...cacheKind) bool`,
true when every named kind is synced under `gen`; each guarded read takes `gen` first and reports its own `ok`:

| Session read | `Caches` method | Cache kinds the guard covers |
|---|---|---|
| `CachedOverride` (`runtime.go:304`) | `Override` (`caches.go:729`) | `cacheOverrides` |
| `Collections` (`:311`) and `LatestCompleted` (`:363-364`) | `Collections` (`caches.go:677`) | `cacheJobs` |
| `Live` (`:319`) | `Live` (`caches.go:528`) | `cacheActive` **and** `cacheJobs`, because `liveLocked` (`:536-549`) reads both maps |

The three `Session` methods return an error wrapping `natskv.ErrUnavailable` in place of an `ok`,
which is what `Session()` itself returns and what the routes already map,
and `LatestCompleted` returns that error when its candidate read is not ok.
`consider` (`scheduler.go:189`) takes the generation its `tick` already read (`:143`, call at `:164`)
and returns on a not-ok `Live`, exactly as the barrier check at `:147` does:
a signature change with no behavior change.
The worker, sweeper, and publisher are untouched, because they read through `jobEntries`, `slotEntries`,
`activeEntries`, `cachedLive`, `hasJob`, and `activeID`,
and every mutation they then make goes through a `View(gen)` that fails on a moved generation.
`internal/httpapi/pgo_collections.go:413` (the listing), `:491` (the override a create layers),
and `:538` (the live check a create makes) answer `q.fail(w, errPGOUnavailable)`;
the two `latest` routes need no new branch, because `serveLatestCollection` (`:910-919`) passes the error to
`storeError` (`internal/httpapi/pgo.go:190-196`), which answers that for everything but `ErrKeyNotFound`.

- [x] **Write the tests, run them, and record which subtests were red**

`runtime_test.go`, beside `TestSessionWaitsForBothHalvesOfTheBarrier` (`:80`):
`TestSessionCacheReadsRefuseAMovedGeneration`, a table over the four session reads.
Take a session, move the generation, let the caches be reset under it, then make the read:
each answers `natskv.ErrUnavailable` where today it answers an empty result.
Six tests in the same file call the re-signed methods directly and take the generation with them:
`TestCachesCollectionsListsNewestFirst`, `OrderIsTotal`, `PagesByValue`, `Filters`, `LimitsThePage`,
and `TestCachesOverrideCarriesItsRevision` (`:251-489`).

`internal/httpapi` needs one harness seam, because `Session()` is taken once at `server.go:453`, before every read:
wrap `Deps.Auth` (`server.go:63-64`) in an authenticator that runs a test hook and delegates,
and give `pgoHarness` (`fixtures_test.go:1480-1490`) a helper that, on the next request, calls `p.disconnect()`
(`:1781`), writes one key under each watched prefix, and blocks on `p.waitCache` (`:1624`) until all four have applied.
`TestPGORoutesRefuseAMovedGeneration` drives the five routes the three paths serve —
the listing, `POST /collections` for the override and for the live check, and the two `latest` routes —
and each answers `503 pgo_unavailable`;
harness reads at `fixtures_test.go:1669`, `:1698`, `:1708`, and `:1719` take the generation with them.

- [x] **Guard the reads, map the refusal, then validate and commit**

```bash
mise exec -- go test -race ./internal/pgo/ ./internal/httpapi/ && mise run lint && mise run test && mise run check
git add internal/pgo/ internal/httpapi/ && git commit -m "fix(pgo): bind a cache read to its session"
```

Body: a session bound its generation and then read the watched caches without it,
so a read arriving after those caches were reset answered an empty listing rather than a refusal.

## The barrier becomes a gauge

**Files:** `internal/metrics/`: `recorder.go`, `prometheus.go`, `prometheus_test.go`;
`cmd/profgate/`: `serve.go`, `serve_test.go`;
and the two test fakes that implement `Recorder` method by method rather than by embedding `Noop`,
`internal/httpapi/fixtures_test.go:311` and `internal/pgo/fixtures_test.go:1194`,
each of which gains an empty `PGOSyncedFrom` in this commit or stops compiling.

**Declare first:** `Recorder.PGOSyncedFrom(read func() bool)` beside `DiscoverySynced` (`recorder.go:71-72`),
whose `Noop` body at `:127-128` does nothing.
`Prometheus` keeps the `prometheus.Registerer` its constructor takes (`prometheus.go:41`),
on a field beside `jwksFetched` (`:35-37`),
because that constructor registers every metric in one list (`:144-150`) and drops the registerer today.
`PGOSyncedFrom` registers `prometheus.NewGaugeFunc` with `GaugeOpts{Name: "profgate_pgo_synced"}` on that field,
built the way `jwksAge` (`:132-142`) already is: after the struct literal, and answered when the scrape asks.
In `serve.go`, `cacheBarrier` is the one-method interface `Synced(gen uint64) bool` that `*pgo.Caches` satisfies,
and `genBarrier` adds `Generation() uint64` to it, which `natskv.Client` satisfies.
`pgoSynced(client genBarrier, caches cacheBarrier) bool` takes `gen := client.Generation()` on every call.
It answers `client.Synced(gen) && caches.Synced(gen)`, both halves under the one generation.

**What changes.**
`startPGO` (`serve.go:625-680`) calls `deps.recorder.PGOSyncedFrom(func() bool { return pgoSynced(client, caches) })`,
beside `go runCaches` (`:642`).
Every scrape evaluates that function, so the gauge reads `0` from the moment the generation moves:
there is no push site to reach and no sampling interval to lag by,
and one function covers both halves, which neither `Caches.apply` nor the move callback can do alone.
The registration is late and happens once.
`main.go:126` builds the recorder over the registry long before any loop starts,
and `startPGO` is reached only from the `natsCh` branch (`serve.go:490`),
whose channel one goroutine writes once (`:351-357`), so `MustRegister` sees this gauge a single time.
That whole path is under `cfg.PGO.Enabled`,
which is what constructs `natsCh` and the preflight goroutine at all (`:351`),
so with PGO off nothing registers the series and a scrape carries none —
which is the spec's *Metrics* requirement that it exists only when `pgo.enabled`.
`natsPreflight` (`serve.go:583-596`) keeps `OnConnectionChange` driving `profgate_nats_connected`
and moves `runtime.MoveGeneration` onto the new `OnGenerationMove`, which the seam calls for both causes.

*Metrics* puts `profgate_pgo_synced` on both roles, the gateway and the collector.
The collector as a separate process is not built, and
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md) records why,
so this task registers the gauge from the PGO start path rather than from anything gateway-specific:
a collector role that starts its loops through that path gets the series with them.
No collector test appears below because no collector code exists to run one against.

- [x] **Write the tests, run them, and record which subtests were red**

`prometheus_test.go`, beside `TestPrometheus_DiscoverySynced` (`:89`):
a registry gathers no `profgate_pgo_synced` until `PGOSyncedFrom` is called,
and after it `testutil.GatherAndCompare` (`:10`) reads whatever the function answers at that moment, `1` then `0`,
with no recorder call between the two gathers.
`serve_test.go`, over `fakeNATS` (`:1271`) and the `recorder` fake (`:122`), which keeps the function it is handed:
`TestServePGO`'s disabled row (`:1464`) is handed none, and an enabled row (`:1483`) exactly one.
`pgoSynced`, over a `genBarrier` stub whose generation moves on demand and a `cacheBarrier` stub:
true only when both halves hold, over their four combinations;
false on the first call after a disconnect moves the generation and on the first call after a watch cut moves it,
because it re-reads `Generation()` every time and holds nothing between calls;
and false while the caches are still filling under a new generation,
which catches a gauge fed from `Client.Synced` alone.
`OnConnectionChange` still reports the connection and `OnGenerationMove` reaches `Runtime.MoveGeneration`.

- [x] **Add the gauge, rewire the two callbacks, then validate and commit**

```bash
mise exec -- go test -race ./internal/metrics/ ./cmd/profgate/ ./internal/httpapi/ ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/metrics/ cmd/profgate/ internal/httpapi/ internal/pgo/
git commit -m "feat(metrics): add profgate_pgo_synced"
```

Body: the replay barrier had no series, so a store a watch cannot re-open was invisible.
`profgate_pgo_synced` is `1` only when the watches have replayed and the caches have applied that replay.

## The chart alerts on a shut barrier

**Files:** `deploy/chart/profgate/`: `templates/prometheusrule.yaml`, `values.yaml`, `README.md`; `deploy/chart_test.go`; `docs/deployment.md`.

`ProfgatePGONotSynced` is `profgate_pgo_synced == 0` for `10m` at warning severity,
rendered after the shipped three and only when `pgo.enabled`, with the whole rule off by default as it is today;
its description says the process decides nothing from its caches and every PGO route refuses.

| Site | Change |
|---|---|
| `templates/prometheusrule.yaml`, after the `ProfgateOIDCKeysStale` block | the alert, behind a `pgo.enabled` conditional read through `profgate.boolValue` as the file's own guard is (`:1`) |
| `values.yaml:229-236` | the comment listing the shipped alerts names the fourth and the condition it renders under; `rules: []` at `:244` keeps its meaning, that a non-empty list replaces the whole set |
| `README.md:468-476` and `:509` | the table gains a row, and both "three alerts" sentences become what the chart renders |
| `docs/deployment.md:450`, `:476-480` | a `profgate_pgo_synced` row beside `profgate_nats_connected`, and an alert list naming the fourth |
| `deploy/chart_test.go:1033` | the pinned alert names, now per `pgo.enabled` |

- [x] **Write the tests, run them, and record which subtests were red**

`deploy/chart_test.go`: with `pgo.enabled=true` the rendered alerts are the four in order, three at the defaults;
and the loop at `:1020-1032` already checks each for a `for`, a `severity`, a `summary`, and a `description`.
The series-name check at `:1039-1049` greps the bytes of `internal/metrics/prometheus.go` for `"profgate_pgo_synced"`,
which the `GaugeOpts{Name: ...}` inside `PGOSyncedFrom` satisfies wherever in that file it stands,
so this task lands after the one that writes it.

- [x] **Render the alert, move the four documents, then validate and commit**

```bash
semlf check deploy/chart/profgate/README.md docs/deployment.md
mise exec -- go test ./deploy/ && mise run lint && mise run test && mise run check
git add deploy/ docs/deployment.md && git commit -m "feat(chart): alert on an unsynced pgo store"
```

Body: a bucket deleted or recreated under a running process keeps `/readyz` green while every PGO route refuses,
and `ProfgatePGONotSynced` fires after ten minutes, the window `ProfgateNotReady` already gives discovery.

## The changelog, the roadmap, and this plan

**Files:** `CHANGELOG.md`, `docs/plans/roadmap.md`, this plan.

- [x] **Write the changelog entries**

Under `### Fixed`: a watch cut while the NATS connection stayed up re-opened under the generation it already held,
so the watched PGO caches missed every change made in the gap while the routes kept answering from them;
that cut now moves the store generation, and a session whose caches were reset under it answers `503 pgo_unavailable`.
Under `### Added`: `profgate_pgo_synced`, and `ProfgatePGONotSynced` in the chart's `PrometheusRule` when `pgo.enabled`.
No `### Changed` entry for `Options.OnGenerationMove`:
every `Unreleased` entry describes a configuration key, an HTTP contract, or a rendered manifest,
and `internal/natskv` has no consumer.

- [x] **Flip the roadmap and this plan, then validate and commit**

Item 12's second bullet is ticked, its "has no plan yet" close goes, its `Shipped:` line names what carried it,
this plan's line 3 becomes `**Status:** Done` and line 4 an `Outcome:` naming the same commits,
and the plan is deleted in the next commit that touches it, per
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md).

```bash
semlf check CHANGELOG.md docs/plans/roadmap.md docs/plans/store-generation.md
mise run lint && mise run test && mise run check
git add CHANGELOG.md docs/plans/ && git commit -m "docs: name the barrier defect and its gauge"
```

Body: the changelog names the silent staleness a watch cut used to leave in the PGO caches,
and the gauge and the alert an operator now watches for it through.

## Risks and What This Plan Does Not Cover

**One flaky watcher costs four cache rebuilds and every store call in flight,** which is what *The seam* accepts:
the cut means the bucket's stream is gone, where a process should be refusing to decide anyway.

**A deleted stream keeps the barrier shut for as long as it is absent,** with `runWatch` retrying forever,
`/readyz` green, and every PGO route refusing until the bucket returns;
that is the designed answer, and the gauge, which a scrape reads live, is what makes it visible.

**Not covered:** the collector role, which no code declares —
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md) records why it is not built —
and `profgate_pgo_collector_available` with its alert;
the interactive request path, the realm model, the permission boundary,
and an end-to-end scenario for a bucket recreated under a running process.
