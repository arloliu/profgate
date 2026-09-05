# NATS Holds Nothing the PGO Path Did Not Bound

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> On this machine `mise run lint` runs a golangci-lint 2.1.6 that shadows the pinned 2.12.2,
> so every validation block below runs the linter as `mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`
> and never as `mise run lint`.

**Goal:** make every wait on the PGO path end at a bound the gateway set, and never at one it did not mean.
The seam's five-second call deadline bounds a whole artifact transfer today,
so a download a client cannot drain in five seconds is cut mid-body and an upload longer than five seconds never lands.
A watch the seam cannot re-open is retried every 50 milliseconds without bound or jitter,
and the process-level preflights back off without jitter, so replicas that lost one dependency retry in step.
The collector's drain reads each owner's cutoff once,
so a renewal that lands under the drain extends a lease the drain does not wait for,
and a replica that reclaims a Collection it aborted itself has the second owner's drain entry deleted by the first owner's exit.
A publication runs under the request that asked for it,
so a client that leaves between its first write and its last leaves an `initializing` record that holds the Service for a minute.
A `completed` to `expired` update that fails for any reason but a lost race is dropped without a record,
and so is a probe listing that fails.
The worker scan reads every nonterminal record fresh on every delivery,
and a one-Service listing walks every record the bucket holds.
After this plan each wait has the bound the accepted design gives it,
one counter exists that did not, no configuration key is added, and no NATS or Kubernetes permission moves.

**Architecture:** `internal/natskv` reads an object's chunks through an ephemeral pull consumer of its own under the caller's context,
bounds establishment and each wait on the store by its call deadline, passes a `Put` the caller's context untouched,
and re-opens a cut watch on a capped, jittered backoff that resets once the re-opened watch has replayed;
`cmd/profgate` draws the waits of its four retry loops from one jittered backoff;
`internal/metrics` gains `profgate_pgo_store_failures_total`;
`internal/pgo` counts and logs an expired flip that fails for any reason but a lost race and a probe listing that fails,
carries the lease, the claim deadline, and the deadline in its job cache so a scan reads fresh only what is due,
indexes its job cache per Service so a listing costs what the Service holds,
runs a publication under its own bounded context once its first write has landed,
re-reads an owner's cutoff when the drain timer fires and waits for the work to be cancelled there,
and lets an owner remove only the drain entry it registered;
`internal/httpapi` audits `client_gone` for a create whose client left mid-publication
and counts the flip failure of its download path through the session;
`docs/deployment.md` documents the counter.
No route, chart value, configuration key, or permission moves.

**Spec:** every behavior here is accepted text in [`pgo.md`](../specs/pgo.md):
*NATS stores* for the preflight retry's jitter (`docs/specs/pgo.md:299-301`);
*The seam* for the call deadline per wait, the transfers, and the re-open backoff (`:586-588`, `:597-641`, `:698-717`);
*Algorithm* for the context a publication's writes run under (`:1128-1148`);
*Claim* for the scan's `due` check and the cache fields (`:1444-1463`, `:1497-1511`);
*Sweeper* for the counted flip and probe-listing failures (`:1917-1927`);
*Create a Collection* for what a client that leaves mid-publication sees (`:2193-2203`);
*List Collections* for the per-Service index (`:2462-2467`);
*Download* for the stream bounded by the request and a flip that fails (`:2620-2628`);
*Logging* for the re-open's records and the two warn records (`:3012-3021`);
*Metrics* for the counter and why it is a series of its own (`:3058-3068`);
*Shutdown* for the one cutoff, the drain timer's re-read, and the entry an owner removes (`:3146-3169`);
*Unit* for the cases each task writes (`:3306-3325`, `:3415-3421`, `:3497-3511`, `:3549-3554`, `:3575-3580`, `:3620-3629`, `:3793-3796`);
*Failure Scenarios* for the eight rows (`:4009-4016`);
and the amendment block that lists them (`:4255-4292`).
[`gateway.md`](../specs/gateway.md) *Startup and shutdown* carries the preflight backoff's jitter (`docs/specs/gateway.md:1694`).
This work is ordered by [`roadmap.md`](roadmap.md),
under *Bound what NATS can hold on the PGO path* (`docs/plans/roadmap.md:238-278`).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the system, not as the defects that revealed them.

- **The call deadline bounds each wait on the store and never a transfer.**
  `objView.Get` wraps the caller's context in `callTimeout` and hands that context to nats.go for the whole stream
  (`internal/natskv/client.go:837-838`, `:854`),
  and `objView.Put` does the same for the whole upload (`:819-821`);
  nats.go reads every chunk and answers every `Read` against that one context.
- **A wait that failed is retried on a schedule that spreads out.**
  The re-open loop sleeps `watchReopenDelay`, 50 milliseconds, after every failed open (`:28`, `:713`),
  and the four process-level loops double from one second to thirty with no jitter
  (`cmd/profgate/serve.go:35-38`, `:592-596`, `:620-624`, `:674-678`, `:785-789`).
- **A write that did not land is seen, and a lost race is silent.**
  `flipExpired` returns on any `Update` error (`internal/pgo/sweeper.go:255-257`),
  `expireGoneArtifact` and `expireCollection` do the same
  (`internal/pgo/runtime.go:456-458`, `internal/httpapi/pgo_collections.go:1083-1085`),
  and a probe listing that fails is skipped without a record (`internal/pgo/sweeper.go:421-423`).
- **A scan costs the store what is due, not what is stored.**
  `scan` visits every nonterminal record the cache holds and `visit` reads each one fresh
  (`internal/pgo/worker.go:296-298`, `:306`),
  because `cachedJob` carries neither the lease nor the claim deadline nor the deadline (`internal/pgo/caches.go:108-124`).
- **A listing costs what the Service holds.**
  `Collections` walks every record in the job cache under the lock, allocating for all of them (`internal/pgo/caches.go:743-747`).
- **A publication finishes once it has begun.**
  Every write of `Publish` runs under the caller's context (`internal/pgo/publisher.go:254-315`),
  and the on-demand caller's context is the request's (`internal/httpapi/pgo_collections.go:572`).
- **One cutoff per owner, read wherever an owner's authority ends.**
  `waitCollection` reads the cutoff once and arms one timer from it (`internal/pgo/worker.go:232-236`),
  while the work's cancellation is a separate timer reset through a channel with a relative duration
  (`:559`, `:717-759`).
- **An owner removes only the drain entry it registered.**
  `startOwner` deletes whatever entry its identifier names on exit (`:476-479`),
  and a reclaim by the same replica overwrote that entry with the successor's (`:471`).

---

## Decisions

Fourteen choices settle how the spec's text is carried, and the nats.go and Go facts that shape each test.

**The seam reads an object's chunks itself, through one ephemeral pull consumer it creates and fetches from one chunk at a time.**
nats.go's `ObjectStore.Get` cannot carry two bounds.
It wraps the one context it was given (`$(go env GOMODCACHE)/github.com/nats-io/nats.go@v1.53.1/jetstream/object.go:837`),
reads the metadata under it (`:851`),
subscribes to the chunk subject with a legacy push ordered consumer bound to it (`:938-946`),
answers every chunk under it (`:895-905`),
and every `Read` on the result takes its read deadline from that context's deadline
and falls back to nats.go's five-second default without one (`:1446-1470`);
the bytes cross a synchronous `net.Pipe` (`:888`).
A `Read` that waits for a chunk therefore returns only at that context's deadline,
so no single context can bound establishment at five seconds and let the bytes follow a request.
nats.go's new-API ordered consumer cannot carry them either:
its `Fetch` calls `reset` before every request (`ordered.go:420-441`),
and `reset` deletes the previous consumer on a background ten-second context
and recreates it with retries on ten-second contexts of its own, unlimited by default (`:558-601`, `:626-627`),
so a `Next` under `FetchMaxWait` can still sit in a reset nothing of the seam's can interrupt,
and it creates one consumer per chunk.
The seam does what the spec says instead (`docs/specs/pgo.md:597-619`):
`GetInfo` under the call deadline, which is one direct get of the object's last meta message
(`object.go:1160-1196`, through `stream.go:560-586` on the `DIRECT.GET.<stream>.<subject>` subject of `api.go:103`,
because the object store's stream is created with `AllowDirect` at `object.go:589`),
then one `js.CreateConsumer` (`jetstream.go:881`) over the stream `OBJ_PROFGATE_ARTIFACTS`,
also under the call deadline,
with the configuration an ordered consumer sends the server (`ordered.go:629-645`):
a name of the seam's own, `FilterSubject` `$O.PROFGATE_ARTIFACTS.C.<nuid>`,
`DeliverAllPolicy`, `AckNonePolicy`, `MemoryStorage`, one replica, and the five-minute inactive threshold;
the templates are `object.go:480-484`.
The handle it returns is a pull consumer bound to that one name, which never resets (`consumer.go:368-387` is the shape `js.Consumer` returns too).
Each chunk is one `Fetch(1, jetstream.FetchMaxWait(callTimeout))` on it (`pull.go:821-835`):
the server ends the request at that expiry, the client's own fallback ends it a second later (`:987`),
a batch that ends empty without an error is `nats.ErrTimeout`,
and the pump selects on the batch's `Messages()` and its own cancellation rather than blocking in `Next`.
A fetch publishes to `CONSUMER.MSG.NEXT.<stream>.<consumer>` and subscribes an inbox it unsubscribes when the request ends
(`pull.go:904-914`, `:933`),
the create publishes to `CONSUMER.CREATE.<stream>.<name>.<filter>` (`api.go:55`, `consumer.go:321`),
and `js.DeleteConsumer` publishes to `CONSUMER.DELETE.<stream>.<name>` (`jetstream.go:208`, `consumer.go:446`);
a deleted consumer ends a pull request still pending on it with `409 Consumer Deleted`, which the fetch handles (`pull.go:722`).
Every one of those subjects is in the fragment of section 3.3 (`docs/specs/pgo.md:312-322`)
and in `fragmentPermissions` (`internal/natskv/fixtures_test.go:260-283`);
`TestPublishedSubjects` (`internal/natskv/preflight_test.go:557-607`) is what holds that.
A chunk message carries its bytes in `Data()` and the pending count in `Metadata().NumPending` (`message.go:40`, `:101`, `:321`);
the object's digest is in the metadata, `ObjectInfo.Digest`, as `SHA-256=` and a base64 sum (`object.go:386-412`, `:487-488`, `:817-829`),
so the seam hashes the chunks through `sha256.New()` as they pass and compares at the last one,
which is the chunk at which `NumPending` is zero.
The bytes cross an `io.Pipe` a pump goroutine writes and the caller drains:
the pump fetches the next chunk only after the previous chunk's write has returned,
so time spent handing bytes to the caller is outside every wait on the store.
The reader ends with the caller's context or with `Close`:
either closes the pipe's write side with the cause, which returns a pending `Read` at once,
the pump issues no further fetch,
and, whatever ended it, the pump deletes the consumer under a fresh call deadline before it exits,
which ends the one fetch that may still be pending and its inbox subscription with it.
A store that stops delivering fails the pending `Read` one call deadline into the wait,
wrapped `ErrUnavailable` by `failure` (`internal/natskv/client.go:427-440`).
An object of size zero has no chunks and returns an empty reader without a consumer.
A 40 MiB artifact is three hundred and twenty chunks of 128 KiB (`object.go:486`) and as many fetches on one consumer;
a batch fetch would save the round trips and is not written,
because the client buffers a whole batch in memory (`pull.go:912-914`) and a wait inside one is bounded by nothing per chunk.

**A `Put` gets the caller's context and nothing of the seam's.**
With no deadline on its context, nats.go's `Put` bounds each chunk's acknowledgement by its own default
(`WithPublishAsyncTimeout(obs.js.opts.DefaultTimeout)` at `object.go:687-689`; the default is five seconds, `jetstream.go:457`;
the timer fires `ErrAsyncPublishTimeout` into the error handler, `publish.go:373-401`),
reads any metadata already under the name through the same wrapping (`object.go:661`, `stream.go:565`),
consults the context before every chunk (`:712-724`),
publishes the meta message before it waits for the last acknowledgements (`:752-770`),
and on any failure purges the partial object after waiting for the acknowledgements or the context,
whichever first (`:697-704`).
A cancelled context returns `context.Canceled` (`:717`),
which `failure` maps to `ErrUnavailable` (`internal/natskv/client.go:430`).
`failure` does not map `jetstream.ErrAsyncPublishTimeout`, the error the acknowledgement timer produces (`publish.go:394-401`),
because no `Put` could reach that timer under the seam's own deadline;
it gains that case, so a store that stops acknowledging is `ErrUnavailable` to the worker as every other stall is.
The work goroutine's context carries no deadline
(`context.WithCancel(context.Background())` at `internal/pgo/worker.go:499`)
and the `Put` runs under it (`internal/pgo/rounds.go:548`),
so removing the seam's `WithTimeout` at `internal/natskv/client.go:819-821` is the whole change,
and a caller that does carry a deadline —
the preflight's probe under its thirty seconds (`internal/natskv/preflight.go:238-242`) —
keeps it,
under which nats.go sets no acknowledgement timeout of its own.

**The call deadline becomes a field a test shortens, and the constant stays.**
Every view reads `callTimeout` as a constant (`internal/natskv/client.go:25`, `:496`, `:515`, `:534`, `:553`, `:572`, `:597`, `:657`, `:819`, `:837`, `:861`, `:880`, `:907`).
The transfer tests prove the deadline bounds establishment and each wait alone,
which a ten-second drain under a five-second deadline proves as well as under one second,
while the stalled-store cases need to observe one deadline pass and then a second not fail the reader;
one second is the difference between a test that runs and one that does not.
`client` gains `callTimeout time.Duration`, set from the constant in `connect` the way `probeDeadline` is (`:71-74`, `:149`),
and every site above reads the field.

**Two backoffs of one shape, one per package.**
`internal/natskv` cannot import `cmd/profgate`,
and the seam is the complete set of things Profgate can do to NATS (`docs/specs/pgo.md:475-476`);
a timer type exported from it would be neither.
A package for a ten-line type that two packages share is more structure than the type,
so each package holds its own:
`reopenBackoff` in `internal/natskv/client.go` from 50 milliseconds,
and `backoff` in `cmd/profgate/backoff.go` from one second, both capped at thirty seconds,
each drawing every wait uniformly from the upper half of its schedule and doubling afterwards (`docs/specs/pgo.md:698-717`).
Both draw from `math/rand/v2`, which the repository already uses for a shuffle and a choice
(`internal/pgo/rounds.go:103-114`, `internal/httpapi/server.go:96`),
seeded from `crypto/rand` with the shape and the `//nolint:gosec // G404` note the shuffle carries.
A `rand.Rand` is for one goroutine at a time (`$(go env GOROOT)/src/math/rand/v2/rand.go:8-10`),
and every watch runs its re-open loop on a goroutine of its own (`internal/natskv/client.go:625`),
so the seam holds no shared generator:
`client` carries a factory, `newReopenRand func() *rand.Rand`, production seeding one generator per watch from `crypto/rand`,
and `runWatch` builds its backoff from one call to it;
a test installs a factory that seeds each watch by its prefix.
The four process-level loops each build one backoff on the goroutine that runs the loop, with the same rule.
The seam's re-open has no clock seam and needs none:
its waits are counted and skipped by a hook, the shape `testHoldReopen` and `testReopenFailed` already have (`:127-136`).
The process-level loops sleep on `time.After` today (`cmd/profgate/serve.go:592`, `:620`, `:674`, `:785`);
the backoff owns that sleep through a `wait(ctx)` method whose sleeper a test replaces,
and `serveDeps` gains a factory the four loops build their backoff from,
production nil, the shape `pgoWorker` and `listen` have (`:92-93`).

**The backoff resets at the replay, and a cut before the replay advances it.**
`consumeWatcher` marks the watch synced when it forwards the marker (`internal/natskv/client.go:757-760`),
and `watchState.syncedUnder(gen)` says whether the marker arrived under the generation the watcher was opened for (`:419-423`).
So when `consumeWatcher` returns because of a cut, `runWatch` reads that flag for the generation it consumed under:
true means the watch replayed, the backoff resets and the re-open is at once;
false means the re-open succeeded and was cut before its marker, and the next attempt waits on the advanced schedule,
exactly as a failed open does.
Reading the flag after the cut is the same as resetting at the marker,
and it is what makes a reset at the re-open detectable: that reset would forget the schedule before the marker arrived.

**The counter is a `CounterVec` on `op`, declared, registered, and recorded as `profgate_sweeper_deletes_total` is.**
That series is declared at `internal/metrics/prometheus.go:87-90`, registered at `:160-166`, recorded at `:234-237`,
documented on the `Recorder` at `internal/metrics/recorder.go:91-93`, given its no-op at `:155-156`,
and tested at `internal/metrics/prometheus_test.go:214-228`;
`profgate_pgo_store_failures_total` follows every step.
A `CounterVec` with no child exposes no series, so a process that never records one exports nothing,
which is how every collector-only counter already exists only where its recorder runs (`docs/specs/pgo.md:3084-3090`);
the constructor registers it, and no `PGOSyncedFrom`-style late registration is needed.
Two implementers outside `internal/metrics` spell every method out and gain the new one:
`countingRecorder` (`internal/pgo/fixtures_test.go:1122-1275`) and `recorder` (`internal/httpapi/fixtures_test.go:290-384`);
the others embed `metrics.Noop`.

**The two read paths share one flip, in the session.**
`expireGoneArtifact` (`internal/pgo/runtime.go:453-460`) and `expireCollection` (`internal/httpapi/pgo_collections.go:1080-1088`)
are the same six lines: set `expired`, `WriteRecord`, record the transition.
The warn record and the count belong in one place,
so the session's method becomes `ExpireGoneArtifact`,
returns whether the flip landed, and does the logging and counting;
`expireCollection` calls it and sets the audit collection when it did.
`ErrRevisionMismatch` is the lost race and stays silent (`docs/specs/pgo.md:1916`, `:1927`);
every other error is logged at warn with the Collection and counted under `op="expire"`,
and the sweeper's `flipExpired` does the same with its own recorder.
The `op` values are constants in `internal/pgo` beside the sweeper's kinds (`internal/pgo/sweeper.go:46-53`).

**A cached lease is never later than the store's.**
`applyJob` copies the record a delivered revision carries (`internal/pgo/caches.go:450-465`);
a renewal only extends the lease (`internal/pgo/worker.go:598-601`) and only a delivered revision reaches the cache,
so the cache holds a lease the store held at some earlier revision, never one it has not.
Lag can therefore make `due` true for a record whose fresh lease is still valid,
which costs one `Get` and claims nothing,
because `visit` reads fresh and `claimable` decides from the fresh record (`:306`, `:328-329`, `:333-345`);
it can never make `due` false for a record whose store lease has lapsed.
`due` is a function of the cached record, the clock, and whether this replica has a free slot,
which is the worker's own state (`reserveLocalSlot`, `:446-456`),
so the worker applies it and the cache only carries the fields.

**The per-Service index is a set of identifiers beside the job map.**
`Collections` needs every field of the entry it lists, which the job map already holds keyed by `job.<id>`,
so the index maps a `serviceRef` to the set of identifiers the Service holds,
maintained in `applyJob` as an entry lands or leaves and cleared with the map in `reset` (`internal/pgo/caches.go:397-410`).
The listing walks the Service's set and looks each entry up, so it visits the Service's `k` records and sorts them.
What the test measures is the two costs the spec names (`docs/specs/pgo.md:2462-2467`):
entries visited, through a hook nil in production beside `applyGate` (`:198-201`),
and bytes allocated per listing, because the allocation *count* is one today and one afterwards —
the `make` at `:743` sizes its capacity from every record in the bucket, and the capacity is what grows.
`Live` walks the whole map too (`:582-593`) and is left as it is:
the spec names the listing, and `Live` is a separate question.

**Thirty seconds, from the clock the publisher already holds.**
The budget is the spec's (`docs/specs/pgo.md:1128-1148`):
half the minute after which the scan fails an unfinished record `not_published`,
a bound and not a promise, and shutdown waits for no publication.
`context.WithTimeout` reads the wall clock, and every time-based decision in `internal/pgo` runs on the injected `Clock`
(`internal/pgo/clock.go:5-11`, `internal/pgo/fixtures_test.go:954-1060`),
so the detached context is `context.WithoutCancel` of the caller's plus a cancellation a `Clock` timer fires,
with `errPublishBudget` as its cause, stopped when `Publish` returns.
The switch happens after the record's `Create` has returned without error (`internal/pgo/publisher.go:268-279`):
an `ErrUnavailable` there is already the indeterminate case, tracked and returned, and needs no continuation.
The scheduler's tick context ends only with the process (`internal/pgo/scheduler.go:125-133`, `:237`),
so the bound is the only thing it gains.
The on-demand handler reads `r.Context().Err()` after `Publish` returns and, when the client has left,
audits `client_gone` and writes nothing, the shape the wait already takes (`internal/httpapi/pgo_collections.go:898-902`).

**One cutoff per owner, one transition under one mutex, and the drain waits for the cancellation.**
`inFlight.cutoff` is already the one value a renewal moves (`internal/pgo/worker.go:76-89`, `:560`),
but the work's cancellation runs on a `leaseCutoff` timer fed a relative duration through a channel (`:559`, `:717-759`),
the drain arms its own timer from one read (`:232-236`),
`cutoffAt` releases its mutex before its caller acts (`:84-89`),
the owner keeps a `committed` value of its own beside the entry (`:496`, `:558-560`),
and `finish` gates the final update on that value rather than on the entry (`:624-628`).
A timer could therefore read an old cutoff, a renewal could install a newer one, and the timer could then cancel on its stale read;
or a renewal's result could still be awaiting installation when the cancellation is declared.
The entry becomes the one place both decisions are made, under its own mutex `mu`:
`cutoff time.Time` and `cancelled bool` move under it, and `cancelledCh chan struct{}` is closed once, by the transition that sets `cancelled`.
Two methods make every change:
`install(cutoff) bool` sets the cutoff and reports true, or reports false without touching it when `cancelled` is already set;
`cancel(now) bool` sets `cancelled` when `now` is not before the cutoff and reports whether it did, so a timer that finds the cutoff moved re-arms.
No store call runs under `mu`: `renew`'s `Update` runs first, and `install` runs with its result.
A renewal whose `install` is refused is a renewal the work was cancelled under;
the owner takes the abort path for it and writes nothing more, as it does for a lapsed lease,
although the store may now hold a longer lease, which another replica reclaims after it lapses.
The owner's timer calls `cancel` when it fires and, when it reports true, cancels the work context;
the owner's other abort paths — the lost record and the lapsed lease (`:561-570`) — go through `cancel` too,
so `cancelledCh` means one thing.
`finish` reads `cutoff` and `cancelled` from the entry under `mu` and discards the result when the work was cancelled or the cutoff has passed,
beside the deadline check it already makes;
`committed` stays as the lease `renew` computes its call deadline from (`:593`), installed in the same step as the cutoff.
The drain's timer re-reads the cutoff when it fires and re-arms while it is ahead;
once it has passed, the drain waits for `done` or `cancelledCh`,
so it returns only once the work has been cancelled at the cutoff the entry then holds or the owner has committed
(`docs/specs/pgo.md:3152-3158`).
The drain keeps a timer of its own rather than waiting on `cancelledCh` alone
because that timer is what a test can see:
`fakeClock.armedTimers` (`internal/pgo/fixtures_test.go:1039-1052`) counts it.
The cutoff moves at most once after the drain begins
because an owner that has observed the drain issues no renewal (`:546-551`)
and only a renewal already inside `renew` when the drain began can still land (`:552`);
a failed renewal leaves `committed` where it was (`:571-574`), so nothing else moves the value.
`leaseCutoff.reset` and its channel go; the cutoff goroutine takes the entry instead of a duration.

**An owner removes only its own entry.**
`inFlight` is keyed by identifier (`internal/pgo/worker.go:120`, `:471`),
and a replica can hold two owners for one identifier in turn:
one that aborted at its cutoff and is still draining its work, and the one its own scan started by reclaiming the record
(`docs/specs/pgo.md:3164-3169`).
The deferred delete at `:478` compares the map's entry with its own before it deletes,
which is the whole change; the successor's entry is what the drain then waits for.

**The recording-transport test is extended, not duplicated.**
`TestPublishedSubjects` runs the preflight's probes
and the one operation they do not (`internal/natskv/preflight_test.go:576-591`);
the probe's `Get` now reads a one-chunk object through the seam's consumer,
and one further `Put` and `Get` of a three-chunk object makes the filtered create, three fetches, and the delete all appear in the tap.
The subjects the seam's read adds are
`$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.<name>.$O.PROFGATE_ARTIFACTS.C.<nuid>`,
`$JS.API.CONSUMER.MSG.NEXT.OBJ_PROFGATE_ARTIFACTS.<name>`,
`$JS.API.CONSUMER.DELETE.OBJ_PROFGATE_ARTIFACTS.<name>`,
and `$JS.API.DIRECT.GET.OBJ_PROFGATE_ARTIFACTS.$O.PROFGATE_ARTIFACTS.M.<encoded name>` for the metadata,
each under a fragment entry (`docs/specs/pgo.md:314-318`);
the legacy push read the seam stops making published a consumer create and an inbox delivery subject instead.
The assertion is the inventory it already is, and it is green on the unchanged code as on the changed;
it is the fence, not the reproducer.

**No changelog entry is marked breaking.**
Each entry describes a bound a running install gains; no query, label, or code changes what it matches.
The counter is a new series under `### Added`; everything else goes under `### Fixed`.

---

## Global Constraints

- **No new configuration key, route, chart value, or Kubernetes or NATS permission.**
  Every figure here is a constant the spec fixes,
  and the seam's own consumer uses only the subjects the account fragment already grants.
- **Every implementation task shows a red test before its change, and says what the red run observes.**
  Tasks 1 through 10 each name the test, the exact command, and the losing state the test forces:
  a reader slower than the deadline, a store that stops delivering, a bucket that stays absent,
  a loop that retries at its bare schedule, a flip that fails, a scan that reads what is not due,
  a listing that walks every record, a client that leaves mid-publication, a renewal landing under the drain,
  and a self-reclaim under the drain.
  The rows that are inventories rather than behaviors — the pinned subject list, the cached fields, the counter's gather —
  say so where they are added.
  Task 11 changes no behavior; the document-lifecycle checks of `mise run check` govern it.
- **An embedded server where the behavior is the server's.**
  [`300-testing.md`](../../.agents/rules/300-testing.md) drives NATS behavior against the in-process server the fixtures start
  (`internal/natskv/fixtures_test.go:58-124`, `internal/pgo/fixtures_test.go:67-123`);
  a chunk that does not arrive, an acknowledgement that does not come, and a bucket that is absent are server behavior,
  so those tests run there, and the handler tests keep their fakes.
- **Several cases of one shape are named table cases.**
  [`300-testing.md`](../../.agents/rules/300-testing.md) asks for `t.Run(tc.name, ...)`;
  the band checks, the counter's operations, the two flip outcomes, and the two cancellation orderings below are tables,
  and a row that names one case is one case of such a table.
- **`-race` and `-count=1` on every red run.**
  The commands below carry both.
- **No jargon:** comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a task name, or a review round.
- Markdown prose uses semantic line breaks;
  run `semlf check` on every Markdown file and every Go file with doc comments a task writes or edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters — the hook refuses 50 or more —
  with a body that says what changed and why, one sentence per line under 120 characters, and no trailer of any kind
  ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
  A commit is finished when `git log --oneline -1` shows it and `git status --short` is clean,
  because the hook can refuse a message after `git commit` has already run ([500](../../.agents/rules/500-validation-and-workflow.md));
  every validation block below ends with both.
- Every task ends with the same validation block before its commit:

```bash
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
```

---

## File Structure

```text
internal/natskv/client.go                    # callTimeout field; the seam's own chunk read; no deadline on Put; ErrAsyncPublishTimeout in failure; reopenBackoff and the reset at the replay
internal/natskv/natskv.go                    # Objects doc comments: what bounds a Get and a Put
internal/natskv/client_test.go               # the transfer cases; the backoff cases
internal/natskv/fixtures_test.go             # a gated reader; the consumer count of the artifact stream; a watcher cut before its marker
internal/natskv/preflight_test.go            # the recording-transport test reads a three-chunk object; the tap drops inbox frames on demand
cmd/profgate/backoff.go                      # new: the jittered backoff the four loops draw from
cmd/profgate/backoff_test.go                 # new: the schedule over twenty draws
cmd/profgate/serve.go                        # the four loops take a backoff; serveDeps.backoff
cmd/profgate/serve_test.go                   # each loop's draws under a recording sleeper
internal/metrics/recorder.go                 # StoreFailure on the Recorder and the Noop
internal/metrics/prometheus.go               # profgate_pgo_store_failures_total
internal/metrics/prometheus_test.go          # the gather
internal/pgo/runtime.go                      # ExpireGoneArtifact: the flip, its warn record, its count
internal/pgo/sweeper.go                      # the counted flip and probe-listing failures
internal/pgo/caches.go                       # LeaseUntil, ClaimBy, Deadline on cachedJob; nonterminalJobs; the per-Service index; the visit hook
internal/pgo/worker.go                       # due ahead of visit; the entry's install and cancel transition; both timers re-read the cutoff; the owner's own entry
internal/pgo/publisher.go                    # the detached, clock-bounded continuation
internal/pgo/fixtures_test.go                # StoreFailure on countingRecorder; store-failure rows
internal/pgo/sweeper_test.go                 # an upload that outlived its acknowledgement is swept; the counted failures
internal/pgo/runtime_test.go                 # the latest path's counted flip
internal/pgo/worker_test.go                  # the due check; the drain's re-read; the self-reclaim
internal/pgo/caches_test.go                  # the index; the cached fields
internal/pgo/publisher_test.go               # the continuation and its budget
internal/httpapi/pgo_collections.go          # client_gone after a publication; expireCollection through the session
internal/httpapi/fixtures_test.go            # StoreFailure on recorder; fakeKV honours a cancelled context; lost creates
internal/httpapi/pgo_collections_test.go     # the slow download under backpressure; the client that leaves mid-publication; the 410 whose flip fails
docs/deployment.md                           # the counter in the metrics table
docs/plans/roadmap.md                        # the item's Shipped line, in the closing task
CHANGELOG.md                                 # one entry per behavior
docs/plans/pgo-nats-bounds.md                # this file
```

---

## 1. A download follows its request, and each wait on the store has the call deadline

Closes the first half of the roadmap bullet beginning *An artifact transfer inherits the five-second call deadline*:
the download.

**Files:**
- Modify: `internal/natskv/client.go`, `internal/natskv/natskv.go`,
  `internal/natskv/client_test.go`, `internal/natskv/fixtures_test.go`, `internal/natskv/preflight_test.go`,
  `internal/httpapi/pgo_collections_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the consumer, the fetch, the pipe, the digest, the stop, and the deadline field.

`client` gains `callTimeout time.Duration`, documented beside `probeDeadline` (`internal/natskv/client.go:71-74`)
as the spec's five seconds that tests shorten, set in `connect` (`:149`),
and every `context.WithTimeout(ctx, callTimeout)` in the views and the timer in `openWatcher` read it.
It also gains two hooks beside the others (`:107-136`), both nil in production:
`testChunkWritten func(name string, n int)`, run after chunk `n` of an object has been written into the pipe,
and `testBeforeFetch func(name string, n int)`, run before the fetch of chunk `n`;
they are the barriers the stalled-store, cancellation, and close rows stand on.

`objView.Get` (`:831-855`) becomes:

```go
func (v *objView) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := v.pre(); err != nil {
		return nil, err
	}
	// Establishment runs under the call deadline: the metadata read and the consumer's creation.
	// The bytes then follow ctx, one chunk at a time, each awaited under the same deadline,
	// so the deadline bounds every wait on the store and never the transfer.
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	info, err := v.obs.GetInfo(cctx, name)
	if err == nil && info.Size > 0 {
		cons, err = v.c.js.CreateConsumer(cctx, artifactsStream, chunkConsumerConfig(info.NUID))
	}
	cancel()
	if perr := v.post(); perr != nil { ... delete the consumer if one was created; return perr }
	if err != nil { ... ErrObjectNotFound for jetstream.ErrObjectNotFound, failure(err) otherwise }
	return v.c.openChunks(ctx, name, cons, info), nil
}
```

`artifactsStream` is `"OBJ_" + artifactsBucket`,
`chunkConsumerConfig(nuid)` is the `jetstream.ConsumerConfig` of *Decisions* with `FilterSubject` `"$O." + artifactsBucket + ".C." + nuid`
and a name drawn from a `nuid`, the two templates at `object.go:480-484` spelled for the one bucket the seam reads.
`openChunks` returns the read side of an `io.Pipe` wrapped in `objectReader`
and starts one goroutine, the pump, that for each chunk in turn runs `testBeforeFetch`, calls `cons.Fetch(1, jetstream.FetchMaxWait(v.c.callTimeout))`,
selects on the batch's `Messages()` and `rctx.Done()`,
writes `Data()` into the pipe, feeds a `sha256.New()` hash, runs `testChunkWritten`,
and after the message whose `Metadata().NumPending` is zero compares the sum with `info.Digest` through `jetstream.DecodeObjectDigest`;
a batch that ends with no message and no error is `nats.ErrTimeout`.
It closes the pipe with `nil` on a match, with an error naming the mismatch otherwise,
with `failure(err)` when a fetch fails or times out, and with `context.Cause(rctx)` when `rctx` ends,
where `rctx` is `context.WithCancelCause(ctx)` and `objectReader.Close` cancels it with `errReaderClosed`.
A second goroutine waits on `rctx.Done()` and closes the pipe's write side with the cause,
which is what returns a pending `Read` at once whatever the pump is waiting on.
Before the pump exits, for any reason,
it deletes the consumer through `v.c.js.DeleteConsumer` under a fresh `callTimeout` context, by the name it gave it,
which ends a fetch still pending on the server side with it;
a reader that was cancelled, closed, or drained therefore leaves nothing on the stream.
`objectReader` (`:923-934`) becomes the pipe reader plus the cancel; `Close` cancels and closes the pipe reader.
`Objects.Get`'s doc comment in `internal/natskv/natskv.go:71-72` says the reader follows ctx
and the call deadline covers each wait on the store.

- [x] **Write the test**

`TestObjects` (`internal/natskv/client_test.go:713-793`) gains the subtests below,
each on `startFixture` with `f.c.callTimeout = time.Second`.
`fixtures_test.go` gains `artifactConsumers(t) int`,
the count of `f.js.Stream(ctx, "OBJ_PROFGATE_ARTIFACTS").ConsumerNames(ctx)`
(`stream.go:770`), and `putBytes(t, name, n)` that stores `n` bytes of a known pattern through the admin connection.
Three rows share one setup, `stalledAt(t, f, chunk)`:
a 512 KiB object, four chunks; `testBeforeFetch` blocks on a channel before the fetch of chunk `chunk`
and `testChunkWritten` signals a channel after chunk `chunk - 1` is written;
a reader that has drained chunk `chunk - 1` then has a `Read` pending on a pump that has not fetched,
and the test knows both.

| Test | What it asserts, and how it fails today |
|---|---|
| `a 2 MiB object drained over ten seconds returns every byte` | `putBytes` of 2 MiB; `Get` under `t.Context()`; a loop that reads 32 KiB and sleeps 150 milliseconds between reads, so the drain takes about ten seconds; the bytes equal the pattern and `Close` returns nil. Today the read fails one second in with nats.go's raw `nats: timeout`, which `objectReader` passes through unmapped because it embeds the `ReadCloser` (`client.go:925-927`): nats.go's `Read` fails once the call's context passes (`object.go:1454-1461`), which is the 64 KiB then `nats: timeout` the roadmap reproduced |
| `a reader that holds one chunk for ten seconds is not failed` | `stalledAt` for chunk 2; read chunk 1, sleep ten seconds, release the fetch, read the rest; every byte arrives and `Close` is nil. Today the second read fails at one second |
| `a store that stops delivering fails the pending read one call deadline into the wait` | `stalledAt` for chunk 3; read chunks 1 and 2; purge the chunk subject through the admin connection (`f.js.Stream` then `Purge(ctx, jetstream.WithPurgeSubject(chunkSubj))`); note the time; release the fetch; the pending `Read` fails `ErrUnavailable` no sooner than one call deadline after the release and no later than two. Today the fetch is nats.go's push subscription and the read fails at the deadline whether or not the store stalled; the row above is what tells the two apart, and this one pins where the wait is measured from |
| `a reader whose context ends mid-stream returns the pending read with the cause` | `stalledAt` for chunk 2; `Get` under a cancellable context; read chunk 1, then cancel with `context.WithCancelCause`'s own cause while the second `Read` is pending; that `Read` returns within 100 milliseconds with an error `errors.Is` that cause; release the fetch; `artifactConsumers` reaches zero within `fixtureTimeout`. Today a cancellation returns nats.go's `context.Canceled` rather than the cause, and a pending `Read` holds nats.go's result mutex, which a concurrent `Close` then waits on (`object.go:1447`, `:1506`); the row is the contract of the new reader |
| `a reader closed mid-stream returns the pending read and leaves no consumer` | as above, with `Close` from another goroutine while the `Read` is pending; the `Read` returns within 100 milliseconds, `Close` has returned by then, and the consumer count reaches zero. Today `Close` blocks on the mutex the pending `Read` holds |
| `a consumer creation that is never answered fails the get within the call deadline and leaves nothing` | `startFixture(t, withUsers(fragmentWithout(t, "publish", "$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>")))`, the shape `internal/natskv/preflight_test.go:264-282` uses; `putBytes` of one chunk through the admin connection; `Get` returns an error within two call deadlines, and `artifactConsumers` is zero. The denied publish is answered by an asynchronous permission error and never by a response, so the create waits out its context. Today the legacy push subscribe waits out the same context; the row pins the bound on the new establishment |
| `chunks whose digest is not the metadata's fail the last read rather than EOF` | `putBytes` of 300 KiB; through the admin connection read the last meta message with `GetLastMsgForSubject` on `$O.PROFGATE_ARTIFACTS.M.<encoded name>`, where the name is `base64.URLEncoding` of the object name as `encodeName` writes it (`object.go:633-635`, `:1174`); unmarshal it into `jetstream.ObjectInfo`, replace `Digest` with the digest of other bytes, and publish it back on the subject that message carried with `Nats-Rollup: sub`, the shape `publishMeta` writes (`object.go:979-1002`, `message.go:214`); `io.ReadAll` of the seam's reader returns every byte and an error that is not `io.EOF` and names the digest. nats.go verifies the digest itself at `EOF` (`object.go:1483-1494`), so this row is green today; it fences the seam's own check |
| `get of an absent object is ErrObjectNotFound` (`:743-748`) and the 40 MiB round trip (`:714-741`) | unchanged and green: the first is the metadata read's answer, the second fetches three hundred and twenty chunks through the pump |

`TestPublishedSubjects` (`internal/natskv/preflight_test.go:557-607`)
puts and gets a 300 KiB object after the `Keys` calls,
so the tap holds the filtered create, the fetches, and the delete of the seam's consumer;
the subset assertion is unchanged.
`TestUnavailable`'s `object get` row (`:838-841`) stays green: the metadata read fails within the deadline.

`internal/httpapi/pgo_collections_test.go` gains `TestCollectionDownloadFollowsASlowClient`, beside `TestCollectionDownloadClientGone` (`:1246-1278`):
a 256 KiB object in the fake store, an `httptest.NewServer` over the handler,
a client that reads 4 KiB and sleeps 20 milliseconds between reads, so the socket's buffers fill and the handler's copy waits on the client;
every byte arrives, the fake reader was read under a context with no deadline (`fakeReader.ctx.Deadline()` reports none),
and the reader is closed afterwards.
The handler passes the request's context to the store and nothing shorter (`internal/httpapi/pgo_collections.go:989`, `:1039`),
so the row is green today;
it is the fence that keeps a deadline out of the copy, and the seam rows above are the reproducer.

The red state:

```bash
go test -race -count=1 ./internal/natskv/ -run 'TestObjects/a_2_MiB_object|TestObjects/a_reader_that_holds|TestObjects/a_store_that_stops|TestObjects/a_reader_whose_context|TestObjects/a_reader_closed'
```

The first two report a raw `nats: timeout` from a read that had one second;
the third fails at the deadline before the release;
the last two wait on the result mutex.
`a consumer creation that is never answered`, the digest row, and the two existing rows are green before and after;
so is the httpapi row.

- [x] **Read the chunks and say so**

`CHANGELOG.md`, `### Fixed`:
**A download is bounded by its request, and by each wait on the store.**
The seam's five-second call deadline covered the whole transfer,
so a client that could not drain an artifact in five seconds was cut mid-body and nothing was tunable.
The deadline now bounds opening the object and each wait for a chunk;
the bytes follow the request, so a slow client is served to the end,
and a store that stops delivering ends the stream one deadline into the wait,
audited `artifact_stream_failed` as before.

- [x] **Validate and commit**

```bash
semlf check internal/natskv/client.go internal/natskv/natskv.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/natskv/client.go internal/natskv/natskv.go internal/natskv/client_test.go internal/httpapi/pgo_collections_test.go \
  internal/natskv/fixtures_test.go internal/natskv/preflight_test.go CHANGELOG.md
git commit -m "fix(natskv): bound each chunk wait" -m "<body: the call deadline covered a whole download; the seam's own consumer, what the deadline now bounds, and how the reader ends>"
git log --oneline -1 && git status --short
```

---

## 2. An upload follows the work context

Closes the second half of the roadmap bullet beginning *An artifact transfer inherits the five-second call deadline*:
the upload.

**Files:**
- Modify: `internal/natskv/client.go`, `internal/natskv/natskv.go`,
  `internal/natskv/client_test.go`, `internal/natskv/fixtures_test.go`, `internal/natskv/preflight_test.go`,
  `internal/pgo/sweeper_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles what nats.go does with a deadline-less context.
`objView.Put` (`internal/natskv/client.go:815-829`) drops its `context.WithTimeout` and passes `ctx` to `v.obs.Put`;
its comment says that the caller's context is the bound,
that nats.go bounds the metadata read and each acknowledgement by its own five seconds
when that context carries no deadline,
and that a cancelled upload is `ErrUnavailable` with an object that may or may not stand under the name,
which the orphan rule removes.
`Objects.Put`'s doc comment in `internal/natskv/natskv.go:70` says the same in one sentence.
`failure` (`:427-440`) gains `jetstream.ErrAsyncPublishTimeout` among the errors it maps to `ErrUnavailable`.
`pre` and `post` stay as they are.

- [x] **Write the test**

Four subtests of `TestObjects` and one of `TestSweeperOrphanObjects`.
`fixtures_test.go` gains `gatedReader`, an `io.Reader` over a byte slice that delivers one 128 KiB chunk
and then blocks on a channel the test closes before it delivers the rest;
nats.go reads the source synchronously (`object.go:729`) and checks its context only between chunks (`:712-724`),
so a gate is released before any return time is measured.
`subjectTap` (`internal/natskv/preflight_test.go:453-539`) gains a read-side parser mirroring its write-side one
and a `dropInbox` flag under its mutex:
while the flag is set, `MSG` and `HMSG` frames from the server whose subject begins with `_INBOX.` are dropped whole,
payload included, and every other frame passes;
a JetStream acknowledgement is such a frame, addressed to the publisher's inbox.

| Test | What it asserts, and how it fails today |
|---|---|
| `a put that takes six seconds under a context with no deadline lands` | a reader over 512 KiB that pauses two seconds after each of its first three chunks; `Put` under `context.Background()` returns nil, and a `Get` returns every byte. Today `Put` returns `ErrUnavailable` at five seconds, observed when the reader hands over its next chunk: the seam's own deadline ends the upload before the reader is drained (`object.go:712-724`) |
| `a put cancelled mid-upload is ErrUnavailable and the name is usable again` | a `gatedReader`; `Put` on a goroutine under a cancellable context; cancel while the reader is gated, then release the gate and note the time; `Put` returns `ErrUnavailable` within a second of the release; a second `Put` of other bytes under the same name lands and `Get` returns those bytes. Green today, because the seam's child context is cancelled with the parent; the row fences the mapping and the name's reuse |
| `a put whose acknowledgements are withheld fails while the connection stays up` | the client connected through a `subjectTap`, as `TestPublishedSubjects` connects (`:564-574`); a `gatedReader` whose gate sets `dropInbox` before it delivers the rest, so the metadata read has already been answered; `Put` under a context cancelled by a timer a minute out; `Put` returns `ErrUnavailable` within twenty seconds, `f.c.Connected()` is true, the generation is what it was before the `Put`, and the caller's context has not ended; `dropInbox` is cleared afterwards. The acknowledgement timer fires `ErrAsyncPublishTimeout` after five seconds (`publish.go:373-401`) and the purge under the default timeout adds five more (`object.go:697-704`). Today `Put` fails at the seam's own deadline, before any timer, with `nats: timeout`; with the deadline removed and `failure` unchanged it returns the raw `ErrAsyncPublishTimeout`, which the row refuses |
| `a put whose server goes away fails before a caller's cutoff a minute out` | a `gatedReader`; `Put` under the same context; `f.stopServer()` while the reader is gated, then release it; `Put` returns `ErrUnavailable` within twenty seconds, and the caller's context has not ended. nats.go buffers the publishes while the connection is down and the same two timers bound the failure; the disconnect also moves the generation, and either cause answers `ErrUnavailable`. Green today at five seconds; the row pins the bound the removed deadline leaves in place |
| `TestSweeperOrphanObjects/an upload that outlived its acknowledgement is swept`, `internal/pgo/sweeper_test.go` | a replica whose client is a `hookClient` (`internal/pgo/fixtures_test.go:1367-1390`); a claimable record and a worker over the real round loop against one `podServer` (`:1781-1810`), or the run stub with a `Put` of its own; the hook's `after` rewrites `put` on the attempt's object to `ErrUnavailable`, which is the indeterminate result the spec describes for a cancelled upload (`docs/specs/pgo.md:633-638`), the acknowledgement lost after the object landed; the object is in the bucket by `f.readObject`, the record ends `failed artifact_store_failed` naming no artifact, and a sweep with the clock past the object's `ModTime + orphanAge + skewMargin` removes it, counted under `orphan`. A cancellation landing between the meta publish and the last acknowledgement (`object.go:752-770`) cannot be timed from a test; the lost acknowledgement leaves the same state. Green today; the sweeper's rule at `internal/pgo/sweeper.go:355-389` already keeps only what a record names, and the row fences it against an upload that no longer fails at five seconds |

The red state:

```bash
go test -race -count=1 ./internal/natskv/ -run 'TestObjects/a_put_that_takes_six_seconds|TestObjects/a_put_whose_acknowledgements'
```

- [x] **Pass the context through and say so**

`CHANGELOG.md`, `### Fixed`:
**An upload follows the collector's work context.**
The seam bounded the whole `Put` of a merged profile at five seconds,
so an artifact the store could not take in that time failed the Collection `artifact_store_failed`.
The upload now runs under the owner's work context, whose cutoff the committed lease sets;
nats.go bounds each chunk's acknowledgement by its own five seconds,
so a store that stops acknowledging still fails the attempt.

- [x] **Validate and commit**

```bash
semlf check internal/natskv/client.go internal/natskv/natskv.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/natskv/client.go internal/natskv/natskv.go internal/natskv/client_test.go internal/natskv/fixtures_test.go \
  internal/natskv/preflight_test.go internal/pgo/sweeper_test.go CHANGELOG.md
git commit -m "fix(natskv): preserve the upload context" -m "<body: the seam bounded a whole upload at five seconds; the work context is the bound now, what nats.go bounds on its own, and the one error it can now return that failure did not map>"
git log --oneline -1 && git status --short
```

---

## 3. A watch is re-opened on a jittered backoff that resets at the replay

Closes the first half of the roadmap bullet beginning *Watch re-open retries every 50 ms*.

**Files:**
- Modify: `internal/natskv/client.go`, `internal/natskv/client_test.go`, `internal/natskv/fixtures_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the shape, the draw, and where the reset is read.

`internal/natskv/client.go` replaces `watchReopenDelay` (`:27-28`) with:

```go
// reopenFirst and reopenCap bound the wait between two attempts to re-open a cut watch:
// the wait doubles from the first to the cap,
// and each wait is drawn from the upper half of its schedule so the watches of one process,
// and the replicas of one Deployment, spread out.
const (
	reopenFirst = 50 * time.Millisecond
	reopenCap   = 30 * time.Second
)

// reopenBackoff is one watch's place on that schedule.
type reopenBackoff struct {
	next time.Duration
	rng  *rand.Rand
}

// draw returns the wait before the next attempt and advances the schedule.
func (b *reopenBackoff) draw() time.Duration {
	d := b.next
	b.next = min(b.next*2, reopenCap)

	return d/2 + time.Duration(b.rng.Int64N(int64(d/2)+1))
}

// reset returns the schedule to its first wait, once a re-opened watch has replayed.
func (b *reopenBackoff) reset() { b.next = reopenFirst }
```

`client` gains `newReopenRand func() *rand.Rand`,
set in `connect` to a function that seeds one generator from `crypto/rand` with the note `internal/pgo/rounds.go:108-113` carries;
`runWatch` calls it once and keeps the generator with its backoff, so no two watches share one.
It gains `testReopenWait func(prefix string, d time.Duration) (skip bool)` beside `testHoldReopen` (`:127-131`):
it runs with every wait drawn and, when it returns true, the wait is skipped; production never sets it.
`testWatchOpened` (`:121-125`) changes shape to `func(prefix string, w jetstream.KeyWatcher) jetstream.KeyWatcher`
and `openWatcher` (`:665-667`) uses the watcher it returns,
so a test can put a watcher of its own in front of the real one; production never sets it,
and the two tests that record watchers through it return what they were given.
`runWatch` (`:681-717`) keeps one `reopenBackoff` per watch across its outer loop:
after `consumeWatcher` returns false, it reads `ws.syncedUnder(gen)` for the generation it consumed under —
true resets the backoff and re-opens at once, false waits one draw first —
and after every failed `openWatcher` it waits one draw,
through one helper that runs the hook, then selects on `ctx.Done()` and `time.After`.
The doc comment on `runWatch` says which cut waits and which does not.

- [x] **Write the test**

Two subtests of `TestGeneration` (`internal/natskv/client_test.go:321-711`),
each on `startFixture` with a `watcherTap` (`fixtures_test.go:198-245`),
a `newReopenRand` that seeds by prefix,
and a hook that appends every draw to a mutex-guarded slice and returns true.
`band(k)` is the schedule's `k`th figure, `min(50ms << k, 30s)`; a draw is in band when it lies in `[band/2, band]`;
the band checks are a table over `k`.
`fixtures_test.go` gains `cutWatcher`, a `jetstream.KeyWatcher` over a real one
whose `Updates()` channel the test closes without delivering anything and whose `Stop` stops the real one:
nats.go places an empty prefix's replay marker in the real watcher's buffered channel before `WatchFiltered` returns
(`kv.go:1276`, `:1373-1387`),
and stopping it closes the channel without discarding what it holds (`:1235-1239`, `:1364-1367`),
so a watcher stopped at open still replays;
a watcher whose channel closes empty is the one cut before its marker.

| Test | What it asserts, and how it fails today |
|---|---|
| `a watch cut against an absent bucket is re-opened on a doubling, jittered schedule that resets after the replay` | watch `g.` and drain to the marker; delete the jobs bucket through `f.js.DeleteKeyValue`, which cuts the subscription; wait until the hook holds twelve draws; draw `k` is in `band(k)`, at least one of the twelve is strictly below its band's whole, and none exceeds thirty seconds. Then recreate the bucket and wait for `f.c.Synced(f.c.Generation())`; clear the draws; delete the bucket again and wait for one draw; it is in `band(0)`. Today every draw is the fixed 50 milliseconds, which lies in `band(1)`: the first assertion fails at `k = 2` |
| `a re-open that is cut before its marker waits on the same schedule as a failed open` | watch `g.` and drain to the marker; `testWatchOpened` returns a `cutWatcher` for the next four watchers it sees, closing each one's channel at once, and returns the real watcher afterwards, all under one mutex-guarded counter installed before the watch is opened; `tap.cut`; wait for four draws; they are in `band(0)` through `band(3)`, and the watch then replays under the current generation; arm the counter for one more; `tap.cut`; the next draw is in `band(0)`. Today the loop re-opens at once after a successful open whatever happened to it, so no draw is recorded and the first wait times out; a reset made at the re-open would record four draws all in `band(0)`, which the second assertion refuses |

The existing bucket-deletion subtest (`:440-540`) is the concurrent case:
three watches fail their re-opens together, each on its own goroutine and its own generator, under the `-race` the suite always runs;
it stays green, gains a `newReopenRand` seeded by prefix and the draw hook,
and asserts that each of the three prefixes recorded draws in `band(0)` and `band(1)`,
which is what a shared generator would race on.
Its two failed opens per watch now cost about 100 milliseconds together.

The red state:

```bash
go test -race -count=1 ./internal/natskv/ -run 'TestGeneration/a_watch_cut_against_an_absent_bucket|TestGeneration/a_re-open_that_is_cut'
```

- [x] **Back off and say so**

`CHANGELOG.md`, `### Fixed`:
**A watch the seam cannot re-open backs off.**
A cut watch was re-opened every 50 milliseconds without bound,
so a bucket that stayed absent cost the process about sixty failed opens a second for as long as it lasted.
The wait now doubles from 50 milliseconds to thirty seconds, with each wait drawn from the upper half of its schedule,
and resets once the re-opened watch has replayed.

- [x] **Validate and commit**

```bash
semlf check internal/natskv/client.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/natskv/client.go internal/natskv/client_test.go internal/natskv/fixtures_test.go CHANGELOG.md
git commit -m "fix(natskv): back off a watch re-open with jitter" -m "<body: a cut watch was re-opened on a fixed 50 ms interval; the schedule, the draw, and why it resets at the replay and not at the re-open>"
git log --oneline -1 && git status --short
```

---

## 4. The four process-level retries draw from one jittered backoff

Closes the second half of the roadmap bullet beginning *Watch re-open retries every 50 ms*:
the process-level retries that back off in step.

**Files:**
- Modify: `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`, `CHANGELOG.md`
- Add: `cmd/profgate/backoff.go`, `cmd/profgate/backoff_test.go`

**The decision, and why.**
*Decisions* settles one type per package and the sleeper as the test seam.
`cmd/profgate/backoff.go`:

```go
// backoff is the wait between two attempts of a retry loop:
// doubling from preflightBackoffFirst to preflightBackoffCap,
// each wait drawn from the upper half of its schedule so replicas that lost one dependency at one moment do not retry in step.
type backoff struct {
	next  time.Duration
	rng   *rand.Rand
	sleep func(ctx context.Context, d time.Duration) error
}

// draw returns the next wait and advances the schedule.
func (b *backoff) draw() time.Duration

// wait sleeps one draw, or returns ctx.Err() when ctx ends first.
func (b *backoff) wait(ctx context.Context) (time.Duration, error)
```

`newBackoff(rng *rand.Rand, sleep ...)` fills `sleep` with a `time.After` select when nil.
`serveDeps` (`cmd/profgate/serve.go:83-98`) gains
`backoff func() *backoff // production: nil, so each loop doubles from 1 s to 30 s on time.After`,
and `serve` resolves it once into a local factory it hands to the four loops:
`preflight` (`:577-598`), `discoverIssuer` (`:606-626`), `natsPreflight` (`:639-680`), and `runCaches` (`:776-791`)
each take a `*backoff`,
replace their `backoff := preflightBackoffFirst`, `time.After`, and `min(backoff*2, cap)` lines with one `wait` call,
and log the drawn wait in `retry_in` as they log the schedule's today.
The constants at `:35-38` stay and are what the type reads.

- [x] **Write the test**

`backoff_test.go` holds the schedule as a table over `k`; `serve_test.go` holds the four loops.
The loop tests build a factory whose `sleep` appends the wait to a slice and returns nil at once,
seeded with `rand.New(rand.NewPCG(1, 2))`.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestBackoff/twenty draws lie in their bands and reach the cap` | twenty `draw`s from a seeded generator: draw `k` lies in `[min(1s << k, 30s) / 2, min(1s << k, 30s)]`, none exceeds thirty seconds, at least one is strictly below its band's whole; a second backoff seeded `(3, 4)` draws a sequence that differs from the first in at least one place. The type does not exist today; the red run is the compile |
| `TestServe/the kubernetes preflight draws its waits from the backoff` | a reactor on `list services` that fails the first twenty lists; `preflight(ctx, rt, logger, factory())` directly, with a runtime from `k8s.NewRuntimeWithClientset` (`cmd/profgate/serve.go:155` shows the call); it returns nil and the recorded waits are the twenty draws the seeded generator makes, each in its band. Today `preflight` takes no backoff and sleeps on `time.After` at 1s, 2s, 4s: the test does not compile, and with the signature added and the loop unchanged it sleeps through the schedule and records nothing |
| `TestServePGO/the nats preflight draws its waits from the backoff` | `newPreflightStub` with twenty `down` results then a client (`serve_test.go:1815-1831` is the shape); `natsPreflight` directly with the factory; the same assertion on the waits |
| `TestServe/issuer discovery draws its waits from the backoff` | `newTestIssuer` with `discoveryStatus` 500 (`:2567-2568` shows the field); an `*auth.OIDC` from `auth.NewOIDC` over `oidcBlock` (`:137`, `:2570`); the recording sleeper flips `discoveryStatus` to 200 under the issuer's own mutex on the twentieth wait it records, so the loop's next attempt succeeds; `discoverIssuer` with a timeout of an hour and the factory; nil, and the waits |
| `TestServePGO/the watched-cache re-open draws its waits from the backoff` | a `fakeNATS` whose jobs bucket is a KV whose `Watch` fails twenty times then opens, written beside `emptyKV`; `runCaches` with the factory under a context cancelled once the caches report `Synced`; the waits |

`preflight transient then ok` (`:973-997`) and `a connection failure is retried until it passes` (`:1815-1831`) stay green:
they count `preflight attempt` records over two failures,
whose two waits now total at most three seconds under the real sleeper.

The red state:

```bash
go test -race -count=1 ./cmd/profgate/ -run 'TestBackoff|TestServe/the_kubernetes_preflight_draws|TestServePGO/the_nats_preflight_draws|TestServe/issuer_discovery_draws|TestServePGO/the_watched-cache_re-open_draws'
```

- [x] **Draw the waits and say so**

`CHANGELOG.md`, `### Fixed`:
**The startup retries do not retry in step.**
The Kubernetes preflight, the NATS preflight, issuer discovery, and the watched-cache re-open doubled their wait from one second to thirty with no jitter,
so every replica that lost a dependency at one moment retried it at the same moments afterwards.
Each wait is now drawn from the upper half of its schedule.

- [x] **Validate and commit**

```bash
semlf check cmd/profgate/backoff.go cmd/profgate/serve.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add cmd/profgate/backoff.go cmd/profgate/backoff_test.go cmd/profgate/serve.go cmd/profgate/serve_test.go CHANGELOG.md
git commit -m "fix(serve): jitter the startup retry loops" -m "<body: four loops doubled from one second to thirty with no jitter; the one backoff they draw from and how a test replaces its sleeper>"
git log --oneline -1 && git status --short
```

---

## 5. `profgate_pgo_store_failures_total` exists

Carries the counter of *Metrics* (`docs/specs/pgo.md:3058-3068`); the next task records into it.

**Files:**
- Modify: `internal/metrics/recorder.go`, `internal/metrics/prometheus.go`, `internal/metrics/prometheus_test.go`,
  `internal/pgo/fixtures_test.go`, `internal/httpapi/fixtures_test.go`, `docs/deployment.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles that the series follows `profgate_sweeper_deletes_total` step for step.
`Recorder` (`internal/metrics/recorder.go:58-122`) gains, after `SweeperDelete`:

```go
	// StoreFailure records one store operation that returned an error other than a lost race:
	// "expire" is a completed-to-expired update that returned anything but a revision mismatch,
	// on the sweeper's path and on the two read paths that make the same flip;
	// "probe_list" is a probe key listing the sweeper could not take.
	StoreFailure(op string)
```

`Noop` gains its empty method (`:155-156` is the shape).
`Prometheus` gains `storeFailures *prometheus.CounterVec`, declared as `profgate_pgo_store_failures_total` with the label `op`
and the help `Total number of PGO store operations that returned an error other than a lost race, by operation.`,
registered in the `MustRegister` list (`internal/metrics/prometheus.go:160-166`), and recorded by `StoreFailure`.
`countingRecorder` (`internal/pgo/fixtures_test.go:1122-1275`) gains a `storeFailures map[string]int`,
the method, and `storeFailureRows()`;
`recorder` (`internal/httpapi/fixtures_test.go:290-384`) gains a `storeFailures []string`,
the method, and `storeFailureRows()`.
`docs/deployment.md` gains one row after `profgate_pgo_synced` (`docs/deployment.md:471`):
`profgate_pgo_store_failures_total`, counter, `op` (`expire`/`probe_list`),
a store write that returned an error other than a lost race, or a probe listing that returned one;
the durable outcome of an `expire` may be indeterminate.

- [x] **Write the test**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestPrometheus_StoreFailure`, `internal/metrics/prometheus_test.go`, beside `TestPrometheus_SweeperDelete` (`:214-228`) | a table of three named cases: `expire` recorded once gathers the one-row text with `op="expire"`, `probe_list` recorded once gathers its row, and a registry with nothing recorded gathers no `profgate_pgo_store_failures_total` series at all, which is how the counter exists only where PGO runs; each through `testutil.GatherAndCompare`. The method does not exist today: the red run is the compile |
| `TestNoop` (`:505`) | gains the call, so the no-op is exercised |

The red state:

```bash
go test -race -count=1 ./internal/metrics/ -run 'TestPrometheus_StoreFailure'
```

- [x] **Declare the series and say so**

`CHANGELOG.md`, `### Added`:
**`profgate_pgo_store_failures_total` counts a store operation that returned a failure.**
A `completed` to `expired` update that fails for a reason other than a lost race, and a probe key listing that fails,
each add one to the counter under `op`, `expire` or `probe_list`, and write one warn record.
Whether the write landed is then indeterminate: a result can be lost after the server committed it.
No existing counter counted a failed result:
`profgate_collections_total` counts transitions that did, and `profgate_sweeper_deletes_total` counts deletions.
The series exists only where PGO runs, on both roles.

- [x] **Validate and commit**

```bash
semlf check internal/metrics/recorder.go internal/metrics/prometheus.go docs/deployment.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/metrics/recorder.go internal/metrics/prometheus.go internal/metrics/prometheus_test.go \
  internal/pgo/fixtures_test.go internal/httpapi/fixtures_test.go docs/deployment.md CHANGELOG.md
git commit -m "feat(metrics): count PGO store failures" -m "<body: no series counted a store write that did not land; the counter, its two operations, and where it exists>"
git log --oneline -1 && git status --short
```

---

## 6. A flip that fails and a listing that fails are logged and counted

Closes the roadmap bullet beginning *A `completed` to `expired` transition that fails to persist*.

**Files:**
- Modify: `internal/pgo/runtime.go`, `internal/pgo/sweeper.go`, `internal/httpapi/pgo_collections.go`,
  `internal/pgo/sweeper_test.go`, `internal/pgo/runtime_test.go`, `internal/httpapi/pgo_collections_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the one flip in the session and the silent lost race.
`internal/pgo/sweeper.go` gains, beside the kinds (`:46-53`):

```go
// The operations profgate_pgo_store_failures_total carries.
const (
	storeOpExpire    = "expire"
	storeOpProbeList = "probe_list"
)
```

`Session.expireGoneArtifact` (`internal/pgo/runtime.go:453-460`) becomes exported:

```go
// ExpireGoneArtifact flips a completed record whose object is no longer in the store,
// and reports whether the flip landed.
// The conditional update at the revision the fresh read returned is what decides:
// the reader that wins it owns the transition's log record and its metric row,
// exactly as the sweeper owns the same transition on its own path, so one flip is never counted twice.
// A lost update is another reader's flip and needs nothing from this one.
// Any other failure is logged at warn and counted under op="expire";
// whether the update landed is then indeterminate, and the next reader or the next sweep observes what stands.
func (s *Session) ExpireGoneArtifact(ctx context.Context, stored StoredRecord) bool
```

with `errors.Is(err, natskv.ErrRevisionMismatch)` returning false silently,
and every other error writing `s.b.Log.Warn("pgo: expired flip failed", "collection", rec.ID, "error", err)`
and `s.b.Recorder.StoreFailure(storeOpExpire)` before returning false;
`LatestCompleted` calls it at `:427` and `:437` and ignores the result, as the walk continues either way.
`expireCollection` (`internal/httpapi/pgo_collections.go:1076-1088`) becomes
`if sess.ExpireGoneArtifact(r.Context(), stored) { q.audit.collection = stored.Record.ID }`.
`flipExpired` (`internal/pgo/sweeper.go:255-257`) gives the `Update` error the same treatment with `s.log` and `s.recorder`.
`sweepProbes` (`:421-423`) logs `pgo: listing probe keys failed` with the bucket and the error,
records `storeOpProbeList`, and continues to the next bucket.

- [x] **Write the test**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestSweeperExpiry/a flip whose update is unavailable is logged and counted`, `internal/pgo/sweeper_test.go` | the shape of `the object goes before the record flips` (`:20-67`) with a `kvHook` whose `after` answers `ErrUnavailable` for `update` on the record's key (`internal/pgo/fixtures_test.go:1438-1441`, `:1546-1564`); after the pass: one `pgo: expired flip failed` record in `r.logs.with`, `storeFailureRows()[storeOpExpire] == 1`, no `expired` collection row; the store holds `expired`, because the write landed and its result was lost; a second pass counts nothing more, because the cache delivered `expired` and the record is no candidate. Today the warn record is absent and the count is zero |
| `TestSweeperExpiry/a lost update is silent and the next pass flips what is still completed` | `a lost update leaves the record alone` (`:92-122`) extended: no warn record and a zero count after the lost pass; release the freezer, wait for the cache to hold the moved revision, sweep again: the record is `expired`, one `expired` row. Green on the silence today, red on the row order only through the previous subtest; it fences the lost race |
| `TestSweeperProbeCleanup/a probe listing that fails is logged and counted, and the rest of the pass runs` | the stranded-probe row (`:494-549`) with a `kvHook` whose `before` answers `ErrUnavailable` for `keys` under `probe.` on the config bucket alone, told apart by the call's order, since `sweepProbes` lists config first (`:420`); after the pass: one `pgo: listing probe keys failed` record, `storeFailureRows()[storeOpProbeList] == 1`, the jobs probe key and the probe object are gone, the config probe key remains. Today no record and no count |
| `TestLatestCollectionCountsAFlipThatFails`, `internal/pgo/runtime_test.go`, beside the session tests | a replica with `wrapClient` installing a `kvHook` whose `after` answers `ErrUnavailable` on `update`; a `Bundle` built as `newRuntime` builds one (`internal/pgo/runtime_test.go:15-36`) but with `Client: r.loopClient`, because `newRuntime` binds `r.client`, which the hook does not wrap (`internal/pgo/fixtures_test.go:638-655`); two completed records, the newer one's object deleted through `f.deleteObject`; `LatestCompleted` on that session returns the older record and its reader, one warn record, `storeFailureRows()[storeOpExpire] == 1`. Today the walk continues and nothing is counted |
| `TestCollectionDownloadCountsAFlipThatFails`, `internal/httpapi/pgo_collections_test.go`, beside `TestCollectionDownloadFlipsAMissingArtifact` (`:1124-1143`) | `h.nats.jobs.updateErr = natskv.ErrUnavailable`; `GET .../profile`: `410 artifact_gone`, `h.rec.storeFailureRows()` is exactly `["expire"]`, no collection row, no transition record; a mirror with `h.nats.jobs.updateMismatch = true`: `410`, no store-failure row. Today the first records nothing |

The red state:

```bash
go test -race -count=1 ./internal/pgo/ -run 'TestSweeperExpiry/a_flip_whose_update|TestSweeperProbeCleanup/a_probe_listing|TestLatestCollectionCountsAFlipThatFails'
go test -race -count=1 ./internal/httpapi/ -run 'TestCollectionDownloadCountsAFlipThatFails'
```

- [x] **Count the failure and say so**

`CHANGELOG.md`, `### Fixed`:
**An expired flip that fails, and a probe listing that fails, are seen.**
A `completed` to `expired` update that returned anything but a lost race was dropped without a record,
on the sweeper's path and on the two download paths that make the same flip,
and a probe key listing that failed skipped that bucket silently.
Each now writes one warn record and counts once under `profgate_pgo_store_failures_total`;
the next pass or the next reader flips a record still `completed` and leaves one already `expired`.

- [x] **Validate and commit**

```bash
semlf check internal/pgo/runtime.go internal/pgo/sweeper.go internal/httpapi/pgo_collections.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/pgo/runtime.go internal/pgo/sweeper.go internal/httpapi/pgo_collections.go \
  internal/pgo/sweeper_test.go internal/pgo/runtime_test.go internal/httpapi/pgo_collections_test.go CHANGELOG.md
git commit -m "fix(pgo): log and count a flip that did not land" -m "<body: a failed expired flip and a failed probe listing left no trace; the warn record, the counter, and why the lost race stays silent>"
git log --oneline -1 && git status --short
```

---

## 7. The scan reads fresh only what is due

Closes the first half of the roadmap bullet beginning *The worker scan re-reads every nonterminal record*:
the cache fields and the due check.

**Files:**
- Modify: `internal/pgo/caches.go`, `internal/pgo/worker.go`, `internal/pgo/caches_test.go`, `internal/pgo/worker_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles why the cache carries the fields and the worker applies the rule.
`cachedJob` (`internal/pgo/caches.go:108-124`) gains `LeaseUntil`, `ClaimBy`, and `Deadline`,
with a comment that the scan decides from them which records it reads fresh
and that the cache is a candidate filter and never the authority;
`applyJob` (`:450-465`) copies them from the record.
`nonterminalJobIDs` (`:624-639`) becomes `nonterminalJobs() []scanCandidate`,
where `scanCandidate` is the identifier and the cached record, sorted by identifier as today;
its one existing caller outside the worker, `internal/pgo/worker_test.go:440`, reads the length of the new result instead.
`Worker.scan` (`internal/pgo/worker.go:287-299`) reads `now` once and visits only candidates for which `due` holds:

```go
// due reports whether a cached record may need acting on now,
// so a pass reads fresh only the records it may act on and one delivery costs the store what is due, not what is stored.
// A cached lease is never later than the store's, because a renewal only extends it and the cache lags the store,
// so lag can make a pass read a record it need not and never skip one whose lease has lapsed;
// the fresh read still precedes every write, and the write alone decides.
func due(c cachedJob, now time.Time, slotFree bool) bool {
	switch c.State {
	case StateInitializing:
		return c.CreatedAt.Add(publishGrace + skewMargin).Before(now)
	case StatePending:
		return slotFree || c.ClaimBy.Add(skewMargin).Before(now)
	case StateRunning:
		return (c.LeaseUntil != nil && c.LeaseUntil.Add(skewMargin).Before(now)) ||
			(c.Deadline != nil && c.Deadline.Add(skewMargin).Before(now))
	default:
		return false
	}
}
```

`slotFree` is `w.active < w.maxActive` under `w.mu`, read once per pass and not per candidate:
a claim inside the pass changes it, and the claim's own `reserveLocalSlot` is what refuses the next.
`visit` and everything after it are unchanged.
The comment on `Run` (`:159-166`) keeps saying the pass runs on the timer and on every delivery;
that is already the code.

- [ ] **Write the test**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestWorkerScanReadsOnlyWhatIsDue/fifty running records with valid leases cost no read`, `internal/pgo/worker_test.go` | fifty `seedClaimable` records mutated to `running` with a lease an hour out, a `kvHook` on the replica's client (`internal/pgo/fixtures_test.go:1367-1390`), `waitCache` until the cache holds all fifty, `hook.reset()`, `scanNow`; `hook.callsFor("get", ...)` over the fifty keys totals zero. Today it is fifty |
| `.../lapsed leases cost one read each` | the same fifty; `r.clock.Set` past the lease plus `skewMargin`; `scanNow` with a `trapRun`-free stub and `maxActiveCollections` at one, so one claim lands and the rest are read and refused for want of a slot; the reads total fifty, the update on `job.*` totals one |
| `.../a replica at its ceiling reads no pending record until claimBy passes` | `maxActiveCollections` at one, one `running` record this replica owns through a held stub, and one `pending` record with `claimBy` an hour out; `scanNow` reads nothing of the pending record; `r.clock.Set` past `claimBy` plus `skewMargin`; `scanNow` reads it once and fails it `not_claimed`. Today the first scan reads it |
| `.../a cached lease that lags the store reads fresh and claims nothing` | a `running` record with a lapsed lease in the cache, a freezer on `cacheJobs` (`:1658-1720`), then a renewed lease written through `f.putJSON` while the cache is frozen; `scanNow`: one read, no update, the record's attempt unchanged. Green today; it is the candidate-filter rule and stays as the fence for the rows above |
| `TestCachesCarryTheScanFields`, `internal/pgo/caches_test.go` | a `seamClient` (`:319-478`) delivering one `running` record with all four fields set; `jobEntries()` holds them as written. An inventory; the fields do not exist today, so the red run is the compile |

The red state:

```bash
go test -race -count=1 ./internal/pgo/ -run 'TestWorkerScanReadsOnlyWhatIsDue|TestCachesCarryTheScanFields'
```

- [ ] **Skip what is not due and say so**

`CHANGELOG.md`, `### Fixed`:
**The worker scan reads only what is due.**
Every scan, on its timer and after every record delivery, read every nonterminal record fresh,
quadratic in live Collections.
The cache now carries each record's lease, claim deadline, and deadline,
so a pass reads fresh only a `pending` record it could claim or that has outlived its claim deadline,
a `running` record whose lease or deadline has lapsed, and an `initializing` one past its grace.

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/caches.go internal/pgo/worker.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/pgo/caches.go internal/pgo/worker.go internal/pgo/caches_test.go internal/pgo/worker_test.go CHANGELOG.md
git commit -m "fix(pgo): scan only the records that are due" -m "<body: every delivery re-read every nonterminal record; the cached fields, the due rule, and why a lagging lease costs a read and never a skip>"
git log --oneline -1 && git status --short
```

---

## 8. A listing costs what the Service holds

Closes the second half of the roadmap bullet beginning *The worker scan re-reads every nonterminal record*:
the per-Service index.

**Files:**
- Modify: `internal/pgo/caches.go`, `internal/pgo/caches_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the set beside the map and the two costs the test reads.
`Caches` (`internal/pgo/caches.go:161-202`) gains `byService map[serviceRef]map[string]struct{}`,
keyed by the Service and holding `job.<id>` keys,
and `listVisited func(key string)`, nil in production,
run for every entry a listing examines, documented beside `applyGate`.
`applyJob` adds the key to its Service's set when it stores an entry
and removes it, deleting an emptied set, when it deletes one;
a record that could not be unmarshalled is removed the same way (`:444-448`).
`reset` clears the index with the map (`:402-403`).
`Collections` (`:731-781`) walks `c.byService[serviceRef{ns, svc}]`,
looks each key up in `c.jobs`, and sizes `out` from that set.

- [ ] **Write the test**

Both on a `seamClient` (`internal/pgo/caches_test.go:319-478`),
because ten thousand records over the embedded server would be the slow part of the suite,
and because the index is a property of `apply` alone.
`seamClient` gains a `buffer int` the watch channel is sized from, `seamWatchBuffer` when zero (`:314`, `:463`):
`Watch` replays the bucket into that channel under the client's mutex before it returns (`:460-477`),
so a replay of ten thousand and four entries into a channel of sixty-four blocks before `Run` can read it;
the listing test sets the buffer to twenty thousand.
The red run of the listing test has one step before the assertion:
`listVisited` is added to `Collections` as it is today, run for every record the loop examines ahead of the Service filter (`:744-747`),
so the old tree reports what it visits;
the index and the walk over it then replace that loop.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestCachesCollectionsCostTheServiceAlone` | a `seamClient` with `buffer` twenty thousand; three completed records of `payment-api` and ten thousand of `other-api`, delivered through `client.put` before `Run`; wait for `Synced`; install `listVisited` counting under a mutex; one listing of `payment-api` visits exactly three entries; `testing.Benchmark` of the same listing reports `AllocedBytesPerOp` under 4 KiB. Today, with the hook in the old loop, the listing visits ten thousand and three and allocates a slice sized for all of them (`:743`), about a megabyte |
| `TestCachesServiceIndexFollowsTheRecord` | one record delivered: the index holds its key under its Service; `client.remove` plus a tombstone entry delivered by hand through the watch's channel, the shape `seamClient.put` uses with a nil value: the index loses it and the Service's set is gone; `client.cut` and a replay without the record: the index is empty under the new generation. The index does not exist today; the red run is the compile |

The red state:

```bash
go test -race -count=1 ./internal/pgo/ -run 'TestCachesCollectionsCostTheServiceAlone|TestCachesServiceIndexFollowsTheRecord'
```

- [ ] **Index the cache and say so**

`CHANGELOG.md`, `### Fixed`:
**A Collection listing costs what the Service holds.**
A page of one Service's Collections walked every record in the job cache under its lock and allocated for all of them,
about 30 milliseconds and 10 MiB per listing with a week of records at the default on-demand ceiling.
The cache now indexes its records per Service, so a listing visits and sorts that Service's records alone.

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/caches.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/pgo/caches.go internal/pgo/caches_test.go CHANGELOG.md
git commit -m "fix(pgo): index the job cache per service" -m "<body: a one-Service listing walked every record under the lock; the index, what maintains it, and what a listing now costs>"
git log --oneline -1 && git status --short
```

---

## 9. A publication finishes once it has begun

Closes the roadmap bullet beginning *Publication runs under the request context*.

**Files:**
- Modify: `internal/pgo/publisher.go`, `internal/httpapi/pgo_collections.go`,
  `internal/pgo/publisher_test.go`, `internal/httpapi/fixtures_test.go`, `internal/httpapi/pgo_collections_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the budget, its clock, and the switch point.
`internal/pgo/publisher.go` gains:

```go
// publishBudget bounds the writes of a publication that follow its record's Create:
// once that write has been acknowledged the rest run under a context of their own,
// so a caller that leaves — a client gone from POST /collections — leaves no initializing record behind.
// It is a bound and not a promise: it is half the minute after which the scan fails an unfinished record not_published,
// and a publication it cuts leaves the state a creator that died leaves, which that scan already resolves.
// A constant, not configuration.
const publishBudget = 30 * time.Second

// errPublishBudget is the cause a continuation's context carries when the budget passed.
var errPublishBudget = errors.New("the publication's continuation budget passed")
```

and `Publish` (`:254-315`), immediately after the `Create` switch at `:268-279` has passed without returning:

```go
	// From here the writes finish whether or not the caller is still there.
	ctx, stop := p.continuation(ctx)
	defer stop()
```

where `continuation` derives `context.WithCancelCause(context.WithoutCancel(ctx))`,
arms `p.clock.NewTimer(publishBudget)`, and runs one goroutine that cancels with `errPublishBudget` when the timer fires
and returns when `stop` closes its channel; `stop` also stops the timer.
The doc comment on `Publish` (`:238-253`) gains one sentence naming the switch.
`serveCollectionCreate` (`internal/httpapi/pgo_collections.go:572-595`)
reads `r.Context().Err()` right after `Publish` returns:
when it is non-nil, the request is audited `codeClientGone` (`internal/httpapi/pgo.go:35`) with nothing written,
the shape `waitForCollection` takes at `:898-902`, whatever the outcome was;
the three branches below run only for a client still there.

- [ ] **Write the test**

The `internal/pgo` rows run through `publishOne` (`internal/pgo/publisher_test.go:19-42`)
with a caller's context the row owns,
so `publishOne` gains a context parameter, or a sibling that takes one.
The budget row needs a store call that ends with the continuation:
`ctxKV`, a `natskv.KV` over the replica's jobs view whose `Update` on one key waits on its context and answers `ErrUnavailable` with the context's error,
because a `kvHook.before` runs ahead of the real call (`internal/pgo/fixtures_test.go:1553-1557`)
and a create released after the cancellation runs under a cancelled context, which is not an acknowledged key.
The `internal/httpapi` rows run over `httptest.NewServer` with a raw client that closes its connection, the shape `TestCollectionDownloadClientGone` uses (`:1246-1278`),
because `fail` answers a client that left by recording `client_gone` and panicking `http.ErrAbortHandler` (`internal/httpapi/server.go:370-380`),
which `hold` (`internal/httpapi/fixtures_test.go:2053-2075`) calls `ServeHTTP` under without recovering.
They need two fixture changes, both in `fakeKV` (`internal/httpapi/fixtures_test.go:971-1007`):
`Create`, `Update`, and `Delete` return `ctx.Err()` wrapped `natskv.ErrUnavailable` when their context has already ended at entry,
which is what the seam answers (`internal/natskv/client.go:429-436`)
and what today's fake ignores (`:1060`, `:1076`, `:1093`),
and `loseCreateOnCancel bool`,
under which a `Create` stores its value, closes a started channel, waits for its context to end, and answers `natskv.ErrUnavailable`:
a write the server committed whose acknowledgement the client's leaving lost.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestPublishFinishesOnceBegun/a caller cancelled after the first write leaves a pending record, its key, and its receipt`, `internal/pgo/publisher_test.go` | a keyed publication; a `kvHook` whose `after` cancels the caller's context once `create` on `job.*` has returned; `Publish` returns `OutcomeWon`; the store holds the record `pending`, the active key naming it, and the receipt. Today the active create runs under the cancelled context, the seam answers `ErrUnavailable`, and the record stays `initializing` |
| `.../a caller cancelled before the first write creates nothing` | cancel before `Publish`; `ErrUnavailable`; no `job.*` key; the reservation is tracked and the next `releaseResolved` releases it, because the fresh reads find nothing. Green today; it fences the switch point |
| `.../a caller cancelled while the first write is in flight keeps its reservation` | a `before` hook that cancels the context and lets the create proceed; `ErrUnavailable`; `r.pub.Reserved()` is one; whatever the store holds under the key is `initializing` or nothing. Green today; the indeterminate case is unchanged |
| `.../a continuation the budget cuts leaves an initializing record the scan fails` | a keyed publication over `ctxKV` holding the `pending` update of the record; wait for the hold to be entered; `r.clock.Advance(publishBudget)`; `Publish` returns `ErrUnavailable` once the continuation's cancellation has reached the held update, which is the return itself; the record is `initializing`, the active key names it, the receipt names it; `r.clock` past `createdAt + publishGrace + skewMargin`, `scanNow` with a worker over the same replica: the record is `failed not_published` and the active key is gone. Today the caller's context is the test's and never ends, so the held update never returns and the row times out |
| `TestCollectionCreateClientGone/after the record's create`, `internal/httpapi/pgo_collections_test.go` | a keyed `POST` written by hand over a raw connection to an `httptest.NewServer`; `h.nats.jobs.setBefore` closes that connection on `create` of the active key and waits for the request's cancellation to be visible, which the fake's own context check makes it; `waitForAudit` for `client_gone`; the record is `pending`, one active key, the receipt exists; a second keyed `POST` with the same key answers `200` naming the identifier. Today the active create runs under the cancelled context, the fake answers `ErrUnavailable`, `fail` records `client_gone` and writes nothing, and the record stays `initializing`: the audit is already right, and the `pending` record, the receipt, and the replay are what the row is for |
| `TestCollectionCreateClientGone/during the record's create` | `loseCreateOnCancel` on; the same raw `POST`; close the connection once the fake's started channel has been read; `waitForAudit` for `client_gone`; the record is `initializing`, `h.pub.Reserved()` is one. Green today: the publisher tracks the reservation and `fail` records `client_gone`; the row fences the indeterminate case |

The red state:

```bash
go test -race -count=1 ./internal/pgo/ -run 'TestPublishFinishesOnceBegun/a_caller_cancelled_after|TestPublishFinishesOnceBegun/a_continuation'
go test -race -count=1 ./internal/httpapi/ -run 'TestCollectionCreateClientGone/after_the_record'
```

- [ ] **Detach the continuation and say so**

`CHANGELOG.md`, `### Fixed`:
**A publication finishes once its first write has landed.**
`POST /collections` ran every write under the request,
so a client that left between the record's first write and the last left an `initializing` record that held the Service for about a minute.
The writes after the first now run under a context of their own, bounded at thirty seconds,
and a request whose client left is audited `client_gone` with nothing written, whether or not the publication won;
a client that leaves during the first write, or a continuation the bound cuts, leaves what a creator that died leaves,
which the scan already fails `not_published`.

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/publisher.go internal/httpapi/pgo_collections.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/pgo/publisher.go internal/httpapi/pgo_collections.go internal/pgo/publisher_test.go \
  internal/httpapi/fixtures_test.go internal/httpapi/pgo_collections_test.go CHANGELOG.md
git commit -m "fix(pgo): finish a publication once it has begun" -m "<body: every write ran under the caller's context; the thirty-second continuation, its clock, and what a client that left is audited>"
git log --oneline -1 && git status --short
```

---

## 10. The drain follows a renewed lease, and an owner removes only its own entry

Closes the two roadmap bullets beginning *`Drain` reads each owner's cutoff once*
and *A replica that reclaims its own aborted Collection*.

**Files:**
- Modify: `internal/pgo/worker.go`, `internal/pgo/worker_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the one transition, the two timers, the wait for the cancellation, and the compare before the delete.
`inFlight` (`internal/pgo/worker.go:65-89`) becomes:

```go
// inFlight is one Collection this replica owns, for Drain to wait on.
// cutoff is the one absolute time an owner's authority ends at, committedLeaseUntil - skewMargin of the lease it last committed:
// a renewal moves it, the work is cancelled at it, the final update is gated on it, and the drain waits to it,
// so no two of them can disagree.
// install and cancel are the only writes, both under mu, so a renewal's result and a cancellation are ordered one way:
// a result installed first moves the cutoff the timer then re-reads,
// and a cancellation declared first refuses the result, which the owner then treats as a lease it cannot use.
type inFlight struct {
	id          string
	done        chan struct{} // closed when the owner loop exits
	cancelledCh chan struct{} // closed once cancel has set cancelled

	mu        sync.Mutex
	cutoff    time.Time
	cancelled bool
}

// install records the cutoff of a lease the owner has just committed, unless the work has been cancelled already.
func (fl *inFlight) install(cutoff time.Time) bool

// cancel marks the work cancelled when now is not before the cutoff, and reports whether this call did so.
func (fl *inFlight) cancel(now time.Time) bool

// abort marks the work cancelled whatever the cutoff, for the lost record and the lapsed lease.
func (fl *inFlight) abort() bool

// state is the cutoff and whether the work was cancelled, read together.
func (fl *inFlight) state() (time.Time, bool)
```

`cancel` and `abort` close `cancelledCh` once, under `mu`, when they set `cancelled`; `cutoffAt` and `setCutoff` go.
`leaseCutoff` and `startCutoff` (`:717-759`) become:

```go
// runCutoff cancels the work when the committed lease is about to lapse, whatever the owner loop is doing.
// It reads the entry's cutoff when its timer fires rather than being told a new one:
// a renewal moves the value, and a timer that finds it moved re-arms for the new one,
// so the work is cancelled at the cutoff the entry holds then and never at an older one.
func (w *Worker) runCutoff(fl *inFlight, cancelWork context.CancelFunc, done <-chan struct{})
```

started by `ownerLoop` in place of `startCutoff` (`:520-521`) with a channel closed when the loop returns;
on every fire it calls `fl.cancel(w.clock.Now())`, re-arms at the cutoff `fl.state()` reports when that is false,
and calls `cancelWork` when it is true.
The renewal's success branch (`:553-560`) installs through `fl.install(lease.Add(-skewMargin))` before it touches `rev`, `entry`, or `committed`;
a refused install takes the lapsed-lease branch (`:567-570`) instead, with a warn record that names the renewal the work was cancelled under.
The two abort branches (`:561-570`) call `fl.abort()` beside `cancelWork`.
`finish` (`:624-634`) reads `fl.state()` and discards the result when the work was cancelled or `now` is not before the cutoff,
beside the deadline check it makes today, so its gate and the drain's read one value;
`ownerLoop` passes `fl` to it.
`waitCollection` (`:224-248`) loops:
a timer at the cutoff `fl.state()` reports; when it fires, a cutoff still ahead re-arms, and one passed falls through to
`select` on `fl.done`, `fl.cancelledCh`, and `ctx.Done()`, logging the warn record of `:242` when `cancelledCh` wins;
`fl.done` wins at any point.
`startOwner`'s deferred delete (`:476-479`) becomes `if w.inFlight[rec.ID] == fl { delete(w.inFlight, rec.ID) }`,
with a comment that a replica can hold two owners for one identifier in turn
and a successor's entry is the drain's to wait for.

- [ ] **Write the test**

All in `TestWorkerDrain` (`internal/pgo/worker_test.go:1029-1292`).
Three barriers serve every row:
the owner's cutoff timer is armed once `r.clock.armedTimers()` reads one after the claim,
because `startCutoff` arms it on a goroutine of its own (`:729-730`);
the drain has read the cutoff and armed its timer once `armedTimers` reads two after `Drain` began;
and a renewal's result is installed once the entry's cutoff, read through `fl.state()` under the test's access to `w.inFlight`, has moved,
because the store shows the new lease before `ownerLoop` installs it (`:610-615`, `:554-560`).
The work context is captured by a run function that records the `ctx` it was given before it defers to the stub,
so cancellation is read from the context itself rather than from the stub's goroutine (`internal/pgo/fixtures_test.go:867-869`).
The two orderings of a renewal and the cutoff are a table of two named cases.

| Test | What it asserts, and how it fails today |
|---|---|
| `follows a renewal that lands after it began/installed before the timer fires` | the blocked-renewal shape of `TestWorkerBlockedRenewalAborts` (`:615-667`): a claim, the cutoff-timer barrier, then a `before` hook that holds the first renewal's `update`; `r.clock.Advance(testLeaseTTL / 3)` and wait on `blocked`; start `Drain` on a goroutine and wait for the drain-timer barrier; release the renewal and wait for the installation barrier; `r.clock.Set` one second past the first lease's cutoff: `Drain` has not returned after 50 milliseconds and the captured context is not cancelled; `r.clock.Set` one second past the new lease's cutoff: `Drain` returns within `fixtureTimeout`, the captured context is cancelled when it does, and the stub is released only afterwards. Today `Drain` returns at the first cutoff |
| `follows a renewal that lands after it began/fired while the result is pending installation` | the same, with an `after` hook that holds the renewal's result once the store has answered; start `Drain` and wait for the drain-timer barrier; `r.clock.Set` one second past the first lease's cutoff: the captured context is cancelled, and `Drain` returns; release the result: the entry's cutoff has not moved, the owner takes the abort path and commits nothing when the stub is released, `w.activeSlots()` reads zero, and the store holds the renewed lease under attempt one. Today the result is installed on a cancelled owner and `fl.cutoff` moves after the drain returned |
| `waits for the second owner of a collection this replica reclaimed` | `maxActiveCollections` at two; a run function that dispatches by `in.Record.Attempt` to two stubs, both ignoring cancellation and held; a hook whose `before` answers `ErrUnavailable` for `update` on the record after the claim, so no renewal lands; `r.clock.Set` past the lease's cutoff and wait for the first stub's context to be cancelled; lift the hook; `r.clock.Set` past `leaseUntil + skewMargin`, `scanNow`: the record is attempt two, owned by the same replica, the second stub entered; wait until `w.inFlight[id]`, read under `w.mu`, is an entry other than the first owner's; release the first stub and wait for `w.activeSlots()` to read one; `Drain` on a goroutine has not returned after 50 milliseconds, and `w.inFlight[id]` is still the second owner's entry; release the second stub: `Drain` returns. Today the first owner's exit deletes the second owner's entry and `Drain` returns at once with the second owner still holding its lease |

The existing rows stay green:
`returns at the lease cutoff, whatever the lease` (`:1124-1169`) waits on `cancelled`,
which the cutoff timer closes though the stub ignores it,
and `leaves a collection past its cutoff` (`:1171-1220`) the same.

The red state:

```bash
go test -race -count=1 ./internal/pgo/ -run 'TestWorkerDrain/follows_a_renewal|TestWorkerDrain/waits_for_the_second_owner'
```

- [ ] **Re-read the cutoff and say so**

`CHANGELOG.md`, `### Fixed`:
**The collector's drain follows a lease renewed under it, and waits for every owner it holds.**
The drain read each owner's cutoff once,
so a renewal already in flight when the drain began extended a lease the drain did not wait for,
and the work could still be running with a lease other replicas honoured when the process exited.
The drain now re-reads the cutoff when its timer fires
and returns only once the work has been cancelled or committed there.
A replica that reclaimed a Collection it had aborted itself lost the second owner's entry when the first owner exited,
and the drain returned at once; an owner now removes only the entry it registered.

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/worker.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/pgo/worker.go internal/pgo/worker_test.go CHANGELOG.md
git commit -m "fix(pgo): drain to the cutoff a renewal moved" -m "<body: the drain read each cutoff once and an owner's exit deleted its successor's entry; the one value both timers read and the compare before the delete>"
git log --oneline -1 && git status --short
```

---

## 11. Close the plan

**Files:**
- Modify: `docs/plans/pgo-nats-bounds.md`, `docs/plans/roadmap.md`, `docs/specs/pgo.md`

Line 3 becomes `**Status:** Done` and line 4 `**Outcome:** pull request #<n> ...`,
naming the pull request that carries the ten tasks above,
and in the same commit the roadmap item's `Shipped:` line (`docs/plans/roadmap.md:277`) names that pull request,
the shape the previous plan's closing commit gave it.
The pull request is named rather than a commit because the merge rebases this branch onto `main`
and rewrites every hash on it, while the number is the same before and after;
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) admits a pull request there for that reason,
and `check_status` in [`check-repo.py`](../../scripts/check-repo.py) requires `**Outcome:** ` followed by text on line 4.
This commit does not delete the plan.
The deletion is the next commit that touches the file, after the merge,
the protocol [`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records;
it deletes this file and rewrites every link that cited it, which `check_links` enforces, and changes nothing else.
`grep -rn pgo-nats-bounds --include='*.md' .` finds the links.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/pgo-nats-bounds.md docs/plans/roadmap.md docs/specs/pgo.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add docs/plans/pgo-nats-bounds.md docs/plans/roadmap.md docs/specs/pgo.md
git commit -m "docs: close the PGO NATS bounds plan" -m "<body: the item's seven bullets are done and its Shipped line names the pull request; the plan is Done>"
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
lists `internal/pgo` and `internal/natskv` among the eight packages that need the suite on the `current` lane before a pull request,
and this plan changes both.
What the suite proves here is narrow:
the seam's own consumer meets a real NATS cluster with the shipped account fragment,
so a download through the gateway is the one place the seam's subjects meet a real permission set,
and a Collection's `Put` lands through a real collector under its work context.
It proves nothing about a slow client, a stalled store, a bucket that stays absent, or a drain under a renewal;
the unit tests above are the evidence for each of those, against the embedded server and the fakes.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **The seam's consumer is a plain pull consumer, and nats.go's own is a push one.**
  nats.go's `Get` subscribes a legacy push ordered consumer (`object.go:938-946`);
  the seam creates one ephemeral pull consumer and fetches one chunk per round trip on it, never resetting it.
  A 40 MiB artifact is three hundred and twenty round trips,
  which the round-trip test at `internal/natskv/client_test.go:714-741` times as it always has;
  a batch fetch would save them and is not written, because the client buffers a batch whole and a wait inside one is bounded per batch, not per chunk.
- **A consumer whose sequence the server loses is not recreated.**
  An ordered consumer would reset on a gap; the seam's consumer reads one subject whose messages a purge alone removes,
  and a purge mid-read is the store stopping delivery, which the fetch reports one call deadline later.
- **A consumer the pump could not delete expires on its own.**
  The delete runs under a fresh call deadline;
  a store unreachable then leaves an ephemeral consumer with a five-minute inactive threshold (`ordered.go:635`),
  which the server removes.
- **Two tests take ten seconds each by design.**
  The slow drain and the held chunk are what the spec asks for and what the bound is about;
  the package's suite grows by about twenty seconds.
- **A `Put` under a context with a deadline gets no acknowledgement timeout from nats.go.**
  The preflight's probe runs under thirty seconds (`internal/natskv/preflight.go:238-242`)
  and would wait that long for a withheld acknowledgement;
  the work context, the one production upload, carries none.
- **The withheld-acknowledgement test filters the wire.**
  The tap drops the acknowledgement frames a JetStream publish is answered with;
  a nats.go release that answered them on a subject outside `_INBOX.` would make the row pass for the wrong reason,
  and the stopped-server row beside it holds the bound either way.
- **Two backoff types of one shape.**
  A change to the draw has two places to land; each package's test pins its own.
- **Jitter cannot be told from the bare schedule on one draw.**
  A draw equal to the schedule's whole is one outcome in half a billion at nanosecond resolution;
  the tests ask for at least one draw below the whole over twelve or twenty, which never happens on the bare schedule.
- **`Live` still walks the job map.**
  The scheduler calls it per Service per tick and the create route once per request (`internal/pgo/caches.go:582-593`);
  the index would answer it in one lookup, and the spec names the listing alone.
  It is noticed, not changed.
- **The continuation's budget runs on the injected clock.**
  In production `SystemClock` is the wall clock;
  a test that forgets to advance its fake clock leaves a continuation unbounded,
  which its own `stop` ends when `Publish` returns.
- **The self-reclaim test drives a renewal that never lands, then lets the reclaim land.**
  A `before` hook answering `ErrUnavailable` is how an owner reaches its cutoff without a second replica, lifted before the reclaim, which is the same `Update` (`internal/pgo/worker.go:426`);
  the reclaim then happens on the same replica's scan, which is the case the bullet names.
- **A refused install leaves a longer lease in the store than the owner honours.**
  The renewal landed and the work was cancelled under it; the owner writes nothing more,
  and the record is reclaimed once that lease lapses, one renewal interval later than it would have been.
- **The spec's deadline-less upload case is carried by six seconds.**
  The accepted text asks for an upload that outlasts the call deadline; five is the bound the change removes, and six outlasts it.
- **The plan's deletion is not one of its tasks.**
  The closing task leaves the finished document in the tree under the lifecycle checks;
  the commit that deletes it and rewrites its links follows the merge, as the previous plan's did.

---

## Self-Review

- Bullet coverage, one line each:
  the transfer deadline, download and upload (tasks 1 and 2);
  the re-open backoff and the process-level jitter (tasks 3 and 4);
  the counted flip and probe-listing failures (tasks 5 and 6);
  the scan's due check and the per-Service index (tasks 7 and 8);
  the publication context (task 9);
  the drain's re-read and the owner's own entry (task 10).
- Current-source facts this plan rests on, each confirmed by reading the file:
  `callTimeout` is `internal/natskv/client.go:25` and is read at `:496`, `:515`, `:534`, `:553`, `:572`, `:597`, `:657`, `:819`, `:837`, `:861`, `:880`, `:907`;
  `watchReopenDelay` is `:27-28` and is slept at `:713`; `runWatch` is `:681-717` and `consumeWatcher` is `:721-771`;
  `markSynced` is `:348-359`, `watchState.syncedUnder` is `:419-423`, `failure` is `:427-440`;
  `objView.Put` is `:815-829`, `objView.Get` is `:831-855`, `objectReader` is `:923-934` and embeds nats.go's reader unmapped at `:925-927`;
  the test hooks are `:107-136`, `testWatchOpened` is run at `:665-667`, `probeDeadline` is `:71-74`, set at `:149`, and every watch pumps on its own goroutine from `:625`;
  `client.js` is `:64`; the fixture is `internal/natskv/fixtures_test.go:58-124`, `fragmentPermissions` is `:260-283`, `watcherTap` is `:198-245`;
  `TestPublishedSubjects` is `internal/natskv/preflight_test.go:557-607`, `probeObjects` is `internal/natskv/preflight.go:236-282`;
  the four loops are `cmd/profgate/serve.go:577-598`, `:606-626`, `:639-680`, `:776-791`, the constants are `:35-38`, `serveDeps` is `:83-98`;
  the counter's model is `internal/metrics/prometheus.go:87-90`, `:160-166`, `:234-237`, `internal/metrics/recorder.go:91-93`, `:155-156`, `internal/metrics/prometheus_test.go:214-228`;
  `flipExpired` is `internal/pgo/sweeper.go:233-260` and fails silently at `:255-257`, `sweepProbes` is `:417-456` and skips at `:421-423`, the kinds are `:46-53`;
  `expireGoneArtifact` is `internal/pgo/runtime.go:453-460`, `LatestCompleted` calls it at `:427` and `:437`, `WriteRecord` is `:205-215`;
  `expireCollection` is `internal/httpapi/pgo_collections.go:1076-1088`, `serveCollectionCreate`'s publication is `:572-595`, the wait's `client_gone` is `:898-902`;
  `cachedJob` is `internal/pgo/caches.go:108-124`, `applyJob` is `:433-466`, `reset` is `:397-410`, `nonterminalJobIDs` is `:620-639`, `Collections` is `:731-781` with the `make` at `:743`, `liveLocked` is `:582-593`, `applyGate` is `:198-201`;
  `Worker.Run` is `internal/pgo/worker.go:167-181`, `Drain` is `:197-220`, `waitCollection` is `:224-248`, `scan` is `:287-299`, `visit` is `:304-331`, `claimable` is `:334-345`, `reserveLocalSlot` is `:446-456`, `startOwner` is `:468-487` with the delete at `:478`, `ownerLoop` is `:494-578` with `committed` at `:496`, the drain check at `:546-551`, the renewal at `:552`, the install at `:553-560`, and the abort branches at `:561-570`, `renew` is `:589-616` with its call deadline at `:593`, `finish` gates at `:624-634`, `leaseCutoff` is `:717-759` and arms on a goroutine at `:729-730`, `inFlight.cutoffAt` is `:84-89`, the work context is `:499`;
  `Publish` is `internal/pgo/publisher.go:254-315` with the record's `Create` at `:268-279`;
  the scheduler's tick runs under `Run`'s context at `internal/pgo/scheduler.go:125-133` and publishes at `:237`;
  the `Put` is `internal/pgo/rounds.go:548`, the seeded shuffle is `:103-114`;
  the pgo fixtures: `startPGO` at `internal/pgo/fixtures_test.go:67-123`, `replica` and `newReplica` at `:589-676`, `runStub` at `:827-937`, `fakeClock` at `:954-1120` with `armedTimers` at `:1039-1052`, `countingRecorder` at `:1122-1275`, `hookClient` and `kvHook` at `:1367-1656`, `cacheFreezer` at `:1658-1720`, `memoryObjects` at `:1973-2053`;
  `TestWorkerBlockedRenewalAborts` is `internal/pgo/worker_test.go:615-667`, `TestWorkerDrain` is `:1029-1292`, `waitClaimed` is `:22-32`, and `nonterminalJobIDs` is called at `:440`; `newRuntime` binds `r.client` at `internal/pgo/runtime_test.go:15-36`;
  `seamClient` is `internal/pgo/caches_test.go:319-478` with its buffer at `:314` and `:463` and its replay under the mutex at `:425-441` and `:460-477`; `publishOne` is `internal/pgo/publisher_test.go:19-42`;
  the httpapi fixtures: `recorder` at `internal/httpapi/fixtures_test.go:290-384`, `fakeKV` at `:971-1307` whose writes ignore their context at `:1060`, `:1076`, `:1093`, `fakeObjects` at `:1310-1418`, `newPGOHarness` at `:1720-1787`, `hold` at `:2053-2075` calling `ServeHTTP` on a goroutine without a recover, and `fail` recording `client_gone` and panicking `http.ErrAbortHandler` for a client that left at `internal/httpapi/server.go:370-380`;
  `TestCollectionDownloadFlipsAMissingArtifact` is `internal/httpapi/pgo_collections_test.go:1124-1143`;
  the metrics table is `docs/deployment.md:457-478` with `profgate_pgo_synced` at `:471`;
  in nats.go v1.53.1, `ObjectStore.Get` is `object.go:836-955` and `Put` is `:638-815` reading its source synchronously at `:729`, `GetInfo` is `:1160-1196`, `objResult.Read` is `:1446-1503` and `Close` takes the same mutex at `:1506`, the templates are `:480-488`, `encodeName` is `:633-635`, the chunk size is `:486`, `ObjectInfo` is `:386-412`, `AllowDirect` is set at `:589`, `publishMeta` is `:979-1002`;
  `wrapContextWithoutDeadline` is `jetstream.go:1270-1275` and `defaultAPITimeout` is `:457`, `CreateConsumer` is `:881`, `OrderedConsumer` is `:902-926`, `DeleteConsumer` is `:208`, `Stream` is `:809`, `getConsumer` is `consumer.go:368-387`;
  the ordered consumer's `Fetch` resets at `ordered.go:420-441`, `reset` is `:558-601` with the unlimited default at `:626-627`, its config is `:609-650` with the inactive threshold at `:635`; `pullConsumer.Fetch` is `pull.go:821-900`, its fallback is `:987`, its deleted-consumer handling is `:722`, and a fetch's subjects are `:904-933`;
  `FetchMaxWait` is `jetstream_options.go:527` and `WithPublishAsyncTimeout` is `:70-76`; the acknowledgement timer is `publish.go:373-401`;
  `Msg.Data` and `Metadata` are `message.go:37-40`, `NumPending` is `:101` and `:321`, `MsgRollup` is `:214`; `ConsumerNames` is `stream.go:770` and `getMsg` is `:564-586`; a KV watcher buffers its marker before `WatchFiltered` returns at `kv.go:1276` and `:1373-1387`, and `Stop` closes without discarding at `:1235-1239` and `:1364-1367`; a `rand.Rand` is for one goroutine at a time (`math/rand/v2/rand.go:8-10`);
  the subject templates are `api.go:51-64`, `:91-103`;
  `context.WithoutCancel` and `math/rand/v2` are in the Go the module pins (`go.mod` declares `go 1.26.0`);
  every commit header above is under 50 characters.
- Decided here, with the reason stated where it is carried:
  the seam's own pull consumer, created once and fetched from one chunk at a time, and the pipe for a `Get`, with the digest checked at the last chunk;
  a `Put` that gets the caller's context untouched, what nats.go bounds on its own, and the acknowledgement timeout `failure` maps;
  the call deadline as a field the seam's tests shorten;
  two backoff types of one shape, one per package, drawing from the upper half of the schedule, one generator per loop;
  the reset read from the watch's marker flag after the cut;
  the counter declared as the sweeper's counter is;
  the flip's logging and counting in the session, with the download route calling it;
  the due rule in the worker over fields the cache carries;
  a set of identifiers per Service beside the job map, with a visit hook and allocated bytes as the test's measure;
  the thirty-second continuation on the publisher's clock, switched after the record's `Create`;
  one cutoff both timers re-read, installed and cancelled under one mutex, with the final update gated on it and the drain waiting for the cancellation;
  the compare before the delete;
  no changelog entry marked breaking;
  the plan closed naming the pull request in both the `Outcome:` and the roadmap's `Shipped:` line,
  and deleted by the commit after the merge.
- Left to the implementer: the exact shapes of `gatedReader`, `artifactConsumers`, `cutWatcher`, `ctxKV`, the tap's read-side parser, and the meta message the digest row rewrites;
  the signature `publishOne` grows or the sibling beside it;
  the failing-watch KV the cache re-open row uses;
  and the wording of every commit body.
