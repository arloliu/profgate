# PGO collection runs in the gateway process until measurement says otherwise

**Decision:** the scheduler, the worker, and the sweeper keep running in every
`profgate serve` replica that has `pgo.enabled`.
The separate collector Deployment that [`pgo.md`](../specs/pgo.md) designs is accepted,
and deliberately not built yet.
Two changes that document describes are built now, in the gateway process:
a drain bounded by the lease rather than by a Collection's worst-case deadline,
and sampling that takes no interactive admission slot.

## Context

The separation is argued in [`pgo.md`](../specs/pgo.md) *Architecture*,
under "Why the loops moved out of the gateway", on three costs.
Each was measured against the shipped configuration before this record was written.

**The memory cost is real at every sizing, and most of it is recoverable without separating anything.**
`config.PGOMemoryBytes()` returns 4 GiB for the shipped ceilings
(`internal/config/config_test.go:906`).
With `pgo.enabled` the chart installs that figure as the entire container limit
and ignores the 512 MiB it budgets for the gateway's own runtime, informer caches, and transfer buffers
(`deploy/chart/profgate/values.yaml:141-147`,
`deploy/chart/profgate/templates/_helpers.tpl:264-269`).
At the two replicas the chart runs (`deploy/chart/profgate/values.yaml:25`),
that is 8 GiB of configured limit.

Two corrections to that shape are worth separating from each other:

| Shape, at two gateway replicas | Aggregate limit |
|---|---|
| today: the derived figure alone, at the shipped ceilings | 8 GiB |
| the container sized as the gateway's own footprint plus the working set, at the shipped ceilings | 9 GiB |
| the same, at the three lowered ceilings this record adopts | 3 GiB |
| a separated collector at those lowered ceilings: 1280 MiB beside two 512 MiB gateways | 2.25 GiB |

So the separation does save memory — 0.75 GiB at the lowered ceilings,
and 3.75 GiB against a correctly sized pair at the shipped ones.
But the sizing change alone recovers 5 GiB of the 5.75 GiB available,
and the remaining 0.75 GiB is what a second Deployment,
a second NetworkPolicy, and an availability protocol would buy.
**What only the separation buys is isolation:**
a merge cannot exhaust the heap of a replica that is serving profile requests.

**The grace-period cost is a guidance defect rather than a deployment one.**
`config.RequiredPGOGracePeriod()` returns 122465 seconds — a little over 34 hours —
for the shipped ceilings (`internal/config/config_test.go:912`),
because it expands them into the deadline of the slowest Collection they admit.
No manifest deploys that number.
`deploy/chart/profgate/values.yaml:163` and `deploy/base/deployment.yaml` both ship 125 seconds,
and `deploy/base/configmap.yaml:63-72` states the trade an operator makes by leaving it there:
a Collection killed mid-merge loses its samples and is reclaimed by another replica.
So the defect is that `profgate config validate` prints a figure nobody should act on.
Bounding the drain by the lease removes the figure entirely,
and it needs no second process to do it —
the lease, the claim, and the reclaim that make it safe already run in every replica.

**The admission cost is real and is not what the separation fixes.**
`internal/pgo/rounds.go:452` waits on the same gate interactive requests fail fast against,
so sampling can hold 8 of the 16 slots `limits.maxConcurrentProfiles` admits.
[`pgo.md`](../specs/pgo.md) *Rounds* already removes that coupling,
and removing it is independent of which process the rounds loop runs in.

**What the separation costs that nothing costs today.**
An installation with `pgo.enabled` and no collector cannot occur while the loops run in the gateway,
and that state is what [`pgo.md`](../specs/pgo.md) *Collector availability* exists to detect:
the `collector.<instance>` heartbeat and its freshness rule,
`503 collector_unavailable`, the availability gauge and its alert, and the stale-key sweep.
Beside it sit a second Deployment, a second NetworkPolicy,
an application-side policy that must admit both roles or leave every Collection failing `no_samples`,
and the collector's memory arithmetic written once in Helm and once in Go with a test to hold them equal.

**The evidence base is six days old.**
`CHANGELOG.md` records the first release on 2026-08-25 with PGO collection already in it,
and `v0.4.0` two days later.
No installation has yet reported memory pressure, a rollout that lost a Collection,
or a need to scale collection independently of request load.

**This deployment's scale is the low end of the range the separation is for.**
Fewer than ten Services have collection enabled, and they collect at intervals measured in hours.
At that rate a merge overlapping a traffic peak is an infrequent event,
which is the one cost the separation, and only the separation, removes.

## Consequences

- The two changes above are built in the gateway process.
  Both make sentences in [`pgo.md`](../specs/pgo.md) true that are false today:
  *Shutdown* says nothing about `pgo.enabled` lengthens a gateway replica's grace period,
  and *Rounds* says sampling takes no admission slot.
  Building them moves the code toward the accepted design rather than away from it.
- `config.RequiredPGOGracePeriod` is deleted with the figure it produced.
- The container is sized as the gateway's own footprint plus the working set, which is what it holds,
  rather than as the working set alone.
- Three of the four ceilings that size the working set get lower defaults,
  and `docs/configuration.md` carries a sizing table in place of the twelve-key arithmetic an operator does today.
  `pgo.preset` is not built:
  the twelve ceilings it would collapse are not one axis —
  retention, on-demand rate, and live-record count answer different questions from the four memory ones,
  and a single name for all of them would couple choices an operator makes separately.
- A gateway replica that collects stays sized for collecting.
  [`pgo.md`](../specs/pgo.md) *Container* gives a gateway replica a static limit that no
  `pgo.limits` key enters; that holds once the collector Deployment exists and not before.
- Nothing in *Collector availability* is built.
  While the loops run in the gateway there is no absent collector to report,
  so `collector_unavailable` stays a registered code no route answers
  (`internal/httpapi/codes.go:90-92`).
- The design is not discarded.
  [`pgo.md`](../specs/pgo.md) keeps every section that describes the separation,
  and this record is where the deferral lives;
  the roadmap that carried it shipped as `v0.5.0` and left the tree.

## Revisit

Any one of these is enough to build the separation:

- A gateway replica is killed for exhausting memory while a merge is running.
  This is the cost nothing else removes, and one occurrence is evidence rather than a trend.
- More than about ten Services carry an enabled policy,
  or an effective policy collects at an interval under an hour,
  so that a Collection is running during most of the day rather than briefly.
- The ceilings an operator actually needs reach the shipped values or above,
  where the saving in *Context* grows from 0.75 GiB to several gigabytes.
- The gateway's derived memory limit becomes what bounds how many replicas a node or a quota admits.

Rebuilding from this record costs little:
the separation is specified in full, and none of the mechanisms it needs —
the lease, the slot key, the active key, the claim, the reclaim, or the sweeps —
is removed by the decision recorded here.
