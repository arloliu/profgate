# The Collector Is Its Own Process

**Status:** Draft

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Every task that lands Go code is test-first,
> and each of those begins by adding the declarations its tests name — types, method stubs, generalized test helpers —
> so its first run fails on assertions rather than on compilation.
> Checkboxes (`- [ ]`) track progress.
> The accepted specs outrank this plan:
> where the two differ the spec wins and the plan is the bug.
> One task below is the exception, and it says so in its own text:
> it repairs the specs first, and every task after it reads them as repaired.

**Goal:** stop making every gateway replica carry the cost of collection.
The scheduler, the worker, and the sweeper move into a `profgate collector` process
that the chart renders only when `pgo.enabled`,
the twelve `pgo.limits` ceilings collapse into `pgo.preset` with per-key overrides,
the chart computes the memory limit and the termination grace period the collector needs,
instead of an operator reading them out of `profgate config validate` and typing them into a Deployment,
sampling stops competing with interactive requests for an admission slot,
and a gateway replica can tell whether a collector exists before it accepts a Collection nobody will run.

**Architecture:** the change is a split, not a rewrite.
`internal/pgo` keeps every loop it has;
what moves is which process starts them.
`Publisher.Run(ctx)` takes over the release pass the scheduler runs today,
because a gateway replica publishes and runs no scheduler.
`internal/pgo` also gains the `collector.<instance>` heartbeat —
a writer in the collector, a cache in a gateway replica —
and its rounds loop loses the admission gate.
`internal/config` gains `pgo.preset` and expands it into the twelve ceilings before validation runs,
gives `PGOMemoryBytes` the collector's base term and an overflow check,
loses `RequiredPGOGracePeriod`,
and drops the one cross-key rule that measured a PGO ceiling against `limits.maxConcurrentProfiles`.
`internal/admit` loses `Acquire` and keeps `TryAcquire`.
`internal/httpapi` gains one refusal and one gauge.
`cmd/profgate` turns `collector` from a reserved operator name into an implemented one,
and `serve` loses the loop wiring.
`deploy/` gains a second Deployment, a second NetworkPolicy, and the arithmetic that sizes the first of them.
`internal/k8s`, `internal/proxy`, `internal/auth`, `internal/natskv`, `internal/ui`, and `internal/client` gain nothing.

**Tech Stack:** everything already pinned in [`mise.toml`](../../mise.toml).
**No Go module is added, and no chart dependency is added.**
The chart's memory arithmetic is Helm template arithmetic,
as `profgate.resources` already is
(`deploy/chart/profgate/templates/_helpers.tpl:200-245`).

**Spec:** [`docs/specs/pgo.md`](../specs/pgo.md), `Accepted`, is the design of record for all of it:
*Architecture* for why the loops moved and why every coordination mechanism stays at one replica,
*Container* for the memory formula and the chart's ownership of it,
*Collector availability* for the heartbeat and the refusal it drives,
*Configuration* and *Presets* for `pgo.preset`,
*Shutdown* for the collector's drain and its grace period,
*Deployment* for the second Deployment, its network flows, and the kustomize placement,
and *Rounds* for sampling without an admission slot.
[`docs/specs/gateway.md`](../specs/gateway.md), `Accepted`, holds the gateway half:
its *CLI* section defines `profgate collector --config <path>`,
its *Build and Deployment* section holds the chart's two-role shape,
and its *Startup and shutdown* section holds a grace period that no longer varies with `pgo.enabled`.
Two rules those documents state are self-contradictory or unsound once the split exists;
*The accepted specs are repaired* closes both, and it runs first.
Sections are cited by heading name, never by number;
an unqualified heading is the PGO spec's.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md), which carries it as its last item.
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- **The permission invariant does not move.**
  The collector runs the same image under the same ServiceAccount, the same ClusterRole,
  the same NATS user, and the same three stores as a gateway replica (*What a compromised gateway can do*).
  No task edits `internal/k8s`, so the seven read tuples stay seven.
  `TestClusterRoleTuples` in `deploy/deploy_test.go`
  and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go` stay green and unedited.
  The one new NATS key prefix, `collector.`, lives in `PROFGATE_JOBS`,
  which the shipped account fragment already grants as `$KV.PROFGATE_JOBS.>` (*Paths that touch each key*),
  so the fragment and the test that pins it do not change.
  The invariant wording in `AGENTS.md`, `README.md`,
  [`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md),
  and the gateway spec's *Permission Boundary* needs no edit.
- **The NATS seam gains no method.**
  `natskv.Client` keeps `Connected`, `Generation`, `Synced`, and `View`;
  `natskv.KV` keeps its five (*The seam*).
  The heartbeat is a `Create`, an `Update` at a held revision, a `Get`, a `Delete`, and a `Watch` prefix —
  every one of them a call the seam already exposes.
- **Coordination is not removed.**
  One collector replica is the default and every mechanism stays:
  the slot key's `Create`, the active key, the lease, the claim race, and the orphan and `not_published` sweeps
  (*Architecture*, "Why the collector is one replica by default").
  A `RollingUpdate` of a one-replica Deployment surges to two, so one replica is not one process.
  No task in this plan deletes a lease, a claim, or a sweep.
- **Every commit leaves an installation that collects.**
  This is the constraint the task order exists to satisfy, and it is stated first because it is easy to break.
  No commit may render a PGO-enabled installation in which no process runs the three loops,
  and no commit may accept a configuration the running sampling path cannot execute at its stated bound.
  The two consequences are that the cutover is additive —
  the collector process exists, is rendered, and is exercised before `serve` gives the loops up —
  and that the admission gate leaves the sampling path before the validation rule that bounded it does.
- **One configuration file serves both processes.**
  `profgate serve` and `profgate collector` load the same ConfigMap, apply the same defaults,
  and run the same validation (*Configuration*).
  No task adds a second file, a second schema, or a role flag inside the file.
- **A preset is a set of defaults, not a mode.**
  Nothing downstream branches on `pgo.preset`;
  after expansion the twelve ceilings are the only thing any code reads (*Presets*).
- **The chart owns the collector's memory arithmetic, and refuses to be told the answer twice.**
  Nothing outside the chart writes the collector's `resources.limits.memory`,
  and the render rejects the environment variables and raw-configuration keys
  that would move a sizing ceiling after the chart had computed it (*Container*).
- **No new metrics label, and no label built from client text.**
  `profgate_pgo_collector_available` is a gauge with no label;
  the existing label sets stay closed (*Metrics*).
- **Nothing a caller can read gains a Pod IP or a resolved port.**
  That holds for `503 collector_unavailable`, which names no instance,
  and for every audit line a collector writes (*Non-disclosure*).
- No jargon: code comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a review round, or a task name.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on every Markdown file a task writes or edits.
- Commit headers are Conventional Commits under 50 characters, with no trailer of any kind.

---

## What This Plan Leaves Alone

| Not in this plan | Why |
|---|---|
| the `artifact.retention >= schedule.every` effective-policy rule | already shipped: `internal/pgo/policy.go:321` holds `retention_under_interval` and `internal/config/config.go:396` defaults `retention` to `24h` |
| removing the lease, the claim, or the orphan sweep | *Architecture* settles it the other way; one replica is not one process |
| a leader election, a sticky session, or a collector Service | nothing routes to a collector, so there is nothing to elect or to select |
| a reload mechanism for the `hot` rows of the *Configuration* table | no reload exists today and this plan adds none; every change still takes a restart |
| `internal/ui` | the console reads the PGO routes over HTTP and cannot tell which process ran a Collection |
| the twelve ceilings' own ranges | a preset value and an override are validated by the ranges already in `internal/config/config.go:353-365`, unchanged except where a task below names one |
| profile rendering, non-Go producers, multi-cluster | [`docs/plans/roadmap.md`](roadmap.md) *Not on This List* |

---

## What This Plan Decides

### The subcommand is `collector`, and the specs already argued for it

`docs/specs/gateway.md:1421` defines `profgate collector --config <path>`,
and `:1437-1438` gives the reason in the spec's own words:
the collector process is `collector`, a noun like `serve`,
so the client verb `collect` keeps its name under the CLI spec's *Reserved names* rule.
The code agrees ahead of the text:
`cmd/profgate/client.go:151` already declares `reservedOperatorNames = [...]string{"collector"}`,
described there as an operator name with no implementation yet,
held so the client half can never take it.
`collect` is taken by `collectVerb()` in the same file's `clientVerbs()`.
What is left is stale text in twelve places, which the first task repairs.

### The heartbeat carries its own validity, and the reader stops deriving one

This is the one place the plan amends an `Accepted` spec rather than implementing it.

*Collector availability* has a gateway replica compute
`writtenAt + 2 × (leaseTTL / 3) + skewMargin > now`
from **its own** `leaseTTL`,
and justifies leaving the interval out of the value:
an operator who lengthens the lease lengthens the interval and the window with it,
and so cannot put the two out of step.
That justification holds inside one process and fails across two Deployments,
which is exactly what this plan creates.
`pgo.leaseTTL` ranges from 30 seconds to 10 minutes (*Configuration*),
and the two Deployments roll independently over one ConfigMap,
so a lease change leaves them disagreeing for the length of a rollout:

| Roll order at the range's extremes | Effect |
|---|---|
| collector reaches `10m` first, gateway still at `60s` | the collector writes every 200s, the gateway calls it absent after 45s, so roughly 155s of false `503 collector_unavailable` per interval |
| gateway reaches `10m` first, collector still at `60s` and then dies | the gateway treats a dead collector's last heartbeat as fresh for about 405s |

Both are silent wrong answers, and only the writer knows its own cadence.
The value therefore carries it:

```json
{"instance": "...", "pod": "...", "writtenAt": "<RFC 3339>", "freshUntil": "<RFC 3339>"}
```

`freshUntil` is `writtenAt + 2 × (leaseTTL / 3)`, computed by the writer from the lease it is running under.
A reader treats a collector as present while `freshUntil + skewMargin > now`,
and treats a value carrying no `freshUntil` as stale —
there is no released version to be compatible with,
so a missing field is a value no shipped writer produces.
The window is unchanged at a single lease: 45 seconds at the shipped 60 seconds.
What changes is which process computed it.

### A refusal reads the store, and only the gauge reads the cache alone

*Collector availability* defines the **gauge** as `1` while the replica's watched `collector.*` cache holds a fresh key,
and that stays exactly as written.
It says nothing about how the refusal decides, and the two are not the same question.
A cache is synced once its initial replay marker has been applied,
but a live entry committed after that marker can be delayed in delivery (*The seam*).
A collector's very first `Create` falls in that window,
so a gateway replica reading the cache alone would answer `503 collector_unavailable`,
though the collector exists and is writing.

`POST /collections` therefore treats the cache as a fast path for the positive answer only.
On a negative or stale cache result it performs one authoritative check —
`Keys("collector.")` and a `Get` of each — through the request's generation-bound view,
and refuses `collector_unavailable` only when that view holds no fresh heartbeat.
If the view is unavailable or has moved under it, the answer is `503 pgo_unavailable`,
which is what a moved generation already means.
A key that disappears between `Keys` and the `Get` for it is an ordinary absent candidate and the scan continues.
The cost is one store read on a path that is about to refuse anyway.

**The check goes after the receipt lookup, never in front of it.**
*Create a Collection* puts the authoritative receipt lookup ahead of every refusal
that could cost a replay its identifier,
and `internal/httpapi/pgo_collections.go:497-510` implements that order:
it answers from the receipt before the ceiling violations and before the token bucket.
A replay creates nothing, so an absent collector is no reason to refuse one.
This is the kind of ordering a later edit breaks silently,
so the task pins it with a test rather than with a comment.

### The `collector.` cache kind lands with the role-specific prefix sets

`internal/pgo/caches.go:154-175` declares exactly four cache kinds with fixed-size generation and sync arrays,
and `Synced` waits on every declared kind (`:479-490`).
So a gateway replica cannot be given four prefixes with `collector.` among them
unless the kind is added in the same change.
The two are one task, and the barrier ranges over the kinds a role selected rather than over every kind declared.

### The cache lifecycle repair belongs to the task that generalizes it

`internal/pgo/caches.go:282-314` opens the watches in sequence under the caller's context.
If a later open fails, the watches already open keep that context while `Run` returns;
if one returned channel closes while the others stay open, `Run` waits forever
and the reopen loop in `cmd/profgate/serve.go:724-742` is never reached.
Today four kinds are always opened together, which hides it.
Making the set role-specific is what turns it into a defect a reviewer can point at,
so the repair lands with the generalization:
one attempt gets a child context that cancels its siblings on a failed open or an unexpected close,
and a watch attempt becomes a reset boundary beside the connection generation.
`internal/pgo/caches.go:333-337` resets a cache only when an entry's generation differs from the one it holds,
so a reopen at the same generation resets nothing at all today.
Before reopening, every selected kind is marked unsynced and cleared — not only the one that failed.
Resetting the triggering kind alone would leave a sibling holding a key deleted during the gap,
and that sibling's fresh marker would lift the role barrier over stale state.

### The chart's agreement test moves rather than being written

`deploy/chart_test.go:395`, `TestChartMemoryLimitIsDerived`,
already renders the chart and compares its memory limit against `config.PGOMemoryBytes()`;
the configuration it computes over is the ConfigMap that same render produced.
It is the mechanism *Container* asks for, pointed at the gateway Deployment.
The chart task moves it to the collector Deployment and widens its target to the base term plus the working set;
it does not invent a comparison the repository lacks.

### The container limit is a second function, because `PGOMemoryBytes` has live consumers

`internal/config/config.go:536` computes the working set alone and multiplies without a check.
The tempting change — give it the 256 MiB base — breaks two agreement tests in the commit that makes it,
long before the tasks that would repair them:
`deploy/deploy_test.go:804` compares `deploy/base/deployment.yaml`'s `4Gi` against `PGOMemoryBytes`
(`deploy/base/deployment.yaml:51-57`),
and `deploy/chart_test.go:388-421` compares the chart's rendered gateway limit against the same function.
Both would fail while the preset task still has to pass `mise run test`,
and the base manifest does not move until the kustomize task.

So `PGOMemoryBytes` keeps its meaning and its callers, and the container limit is a second function:
`CollectorMemoryBytes` is `collectorBaseMemory` plus `PGOMemoryBytes`,
with `collectorBaseMemory` a fixed 256 MiB constant beside `PGODecodeFactor`,
formed under an overflow check.
*Presets* publishes the working set and the container limit as two rows,
so two functions is what the spec describes rather than a workaround.
Nothing existing changes value, and the later chart task points the collector at the new function
and the gateway at the static `gatewayMemoryLimit`.

`RequiredPGOGracePeriod` (`internal/config/config.go:564`) is a different case: it has live callers and no successor.
*Shutdown* replaces the worst-deadline figure with `pgo.leaseTTL + 30s`,
so the function and both callers go in one commit —
`cmd/profgate/main.go:82` prints it and `internal/pgo/record_test.go:382` asserts against it.

### `slot_timeout` leaves the vocabulary rather than becoming unreachable

`internal/pgo/rounds.go:37` declares `ReasonSlotTimeout` and `:455` is its only writer,
reached only from the `Gate.Acquire` at `:452`.
Removing the gate makes the constant dead, and a dead reason in a published vocabulary is worse than no reason:
`docs/api.md` and `docs/pgo.md:287` list it as something an operator may see in a manifest.
The constant, its test, and its two documentation mentions go together.

---

## File Structure

```text
cmd/profgate/
  collector.go          new: the collector subcommand and its lifecycle
  collector_test.go     new
  client.go             modified: collector moves from reserved to implemented, and into usage and runOperator
  client_test.go        modified
  serve.go              modified: the role's watch set, Publisher.Run, and at the cutover no loops and no Collection drain

internal/admit/
  gate.go               modified: Acquire is removed, TryAcquire stays

internal/config/
  config.go             modified: pgo.preset, the changed rules, the base term and the checked product,
                        and RequiredPGOGracePeriod deleted
  presets.go            new: the three presets as data, and the expansion
  presets_test.go       new
  testdata/             modified: the preset fixtures

internal/pgo/
  heartbeat.go          new: the writer, its recovery, the freshness rule, and the reader
  heartbeat_test.go     new
  caches.go             modified: role-specific prefix sets, the collector. kind, and the watch lifecycle repair
  caches_test.go        new
  publisher.go          modified: Run owns the release pass
  rounds.go             modified: no admission gate, no slot_timeout
  scheduler.go          modified: no ReleaseResolved call
  sweeper.go            modified: the stale heartbeat sweep
  worker.go             modified: the drain stops at the lease cutoff

internal/httpapi/
  the create handler    modified: 503 collector_unavailable with its authoritative check
  metrics wiring        modified: the scrape-time collector-available gauge

deploy/
  collector.yaml                  new: outside deploy/base/kustomization.yaml
  collector-networkpolicy.yaml    new: outside the base
  networkpolicy-app-example.yaml  modified: admits both roles
  chart/profgate/
    templates/collector.yaml      new
    templates/_helpers.tpl        modified: the collector's limit, grace period, and rejections
    templates/podmonitor.yaml     modified: selects both roles
    templates/networkpolicy.yaml  modified: one per role
    templates/prometheusrule.yaml modified: the collector-availability alert
    values.yaml, README.md, NOTES.txt  modified
  chart_test.go                   modified: the agreement test moves to the collector

test/e2e/
  registry.go, harness_test.go, scenarios_pgo_test.go  modified: the collector Deployment and three scenarios
```

---

## The accepted specs are repaired

**Files:**
- Modify: `docs/specs/pgo.md`, `docs/specs/gateway.md`, `.agents/rules/100-project-map.md`

No code changes.
Two repairs, and they are different in kind:
one is stale wording that contradicts the same documents' own argument,
and one is a rule that was sound before the split and is not sound after it.
Both run first so that every task after this one reads a spec that says one thing.

**The process is named once.**
Every `collect` in either spec is one of four things:
the realm flag `pgo.collect`, the client verb, the ordinary verb "to collect" — all of which stay —
or a stale process name, which becomes `collector`.
`grep -n '\bcollect\b' docs/specs/pgo.md docs/specs/gateway.md` is run over **both files to the end**,
amendment tables included, and every hit is classified before any is changed.
The amendment tables are the easy ones to skip and the ones that matter most:
a row saying the package map lists `collect` preserves the exact contradiction this task exists to remove.

| Kind | Where, in the PGO spec | Action |
|---|---|---|
| the realm flag, the client verb, the ordinary verb | `1141`, `1856`, `1861`, `2155`, `2157`, `2508`, `2635`, `2704`, `3330`, `3651` | unchanged |
| the process, in the body | `3438`, `3439`, `3446`, `3454`, `3463`, `3497`, `3589` | `collector` |
| the process, in the amendment tables | `3708`, `3710`, `3712`, `3713`, and the implementation rows near `3725` | `collector` |

The gateway spec contradicts itself in two places,
carries the same stale name in its own amendment tables at `:2636` and `:2723`,
and settles the question in a third:
`:1431` says "`collect` runs the PGO collection loops" and `:2413` lists `collect` in the package map,
while `:1437-1438` argues that the process is `collector` precisely so the client verb can keep `collect`.
The first two follow the third.
`.agents/rules/100-project-map.md:61` gains `collector` beside the client verb `collect`,
and `:68` becomes the gate interactive requests pass through, which the next task makes true in code.

**A heartbeat carries its own validity.**
The amendment settles three rules, and it is not confined to one section,
because more than one section states the behavior it changes.

| Rule | Where it is stated today, and what it becomes |
|---|---|
| the writer owns the window | *Collector availability* has the reader derive it from its own `leaseTTL`, and *Configuration* repeats that the two cannot drift; the value gains `freshUntil = writtenAt + 2 × (leaseTTL / 3)`, a reader is present while `freshUntil + skewMargin > now`, and a value carrying no `freshUntil` is stale |
| availability is any fresh value, not the newest one | *Paths that touch each key* has the create read the watched cache and choose by `writtenAt`, and *Failure Scenarios* derives staleness the same way; with per-writer cadences the newest `writtenAt` can be the stale one, so a collector is present when **any** value's `freshUntil` is fresh |
| the create refusal reads the store, the gauge does not | *Create a Collection* and *Collector availability* both have the refusal decide from the watched cache alone; the cache stays the positive fast path, a negative or stale result is confirmed against the store, and the gauge stays cache-derived |
| the first write is at the barrier, not at the first tick | the normative heartbeat paragraph and the *Unit* row both have the writer `Create` on its first tick, which leaves an arriving collector invisible for a whole interval; both say immediately after the replay barrier, and the ticker governs every write after it |
| a draining collector deletes its key first | *Shutdown* already says so; the row is repeated here because the amendment reorders the drain around it |

The paragraph claiming an operator cannot put the interval and the window out of step is replaced.
What replaces it is the reason the writer now owns the derivation:
the two processes roll independently over one ConfigMap,
so only the writer knows which lease produced its cadence.
The freshness figure at the shipped lease is unchanged at 45 seconds.
*Sweeper* keeps its heartbeat row, which reads `writtenAt` for an age and not for a comparison between writers.
*Unit*, *End-to-end*, and the HTTP testing rows gain the two roll orders at the range's extremes,
the mixed-cadence reader, and the refusal's authoritative negative.

- [ ] **Repair the naming in the three files**

- [ ] **Amend *Collector availability* and the testing rows it names**

- [ ] **Validate and commit**

```bash
semlf check docs/specs/pgo.md docs/specs/gateway.md .agents/rules/100-project-map.md
mise run check
git add docs/specs/ .agents/rules/
git commit -m "docs(spec): let a heartbeat carry its own validity"
```

---

## Sampling takes no admission slot

**Files:**
- Modify: `internal/admit/gate.go`, `internal/admit/gate_test.go`,
  `internal/pgo/rounds.go`, `internal/pgo/rounds_test.go`,
  `internal/httpapi/server_test.go`,
  `docs/api.md`, `docs/pgo.md`, `CHANGELOG.md`

*Rounds* states the bound after this change: `maxParallel × maxActiveCollections` by construction,
inside one collector process, with no shared gate.
*Rounds* also gives the per-Pod figure that replaces the old one:
`C × maxActiveCollections` from collectors beside `gatewayReplicas × limits.maxConcurrentProfiles` interactive.

**Why this runs before the preset.**
`internal/config/config.go:869-871` refuses a configuration whose
`maxParallel × maxActiveCollections` reaches `limits.maxConcurrentProfiles`,
which defaults to 16 (`config.go:232`).
The `large` preset sets `maxParallel: 8` and `maxActiveCollections: 4`, a product of 32,
so a commit that lands the presets while the rule stands publishes a preset the binary refuses.
Removing the rule first is the other order, and it is worse:
between the two commits `config.Load` would accept `large`
while the sampling path still blocked on `Gate.Acquire` and could still write `slot_timeout`,
so the process would accept a bound it could not execute.
Taking the gate out first leaves the rule standing as a restriction that is merely conservative:
`standard` is `4 × 2 = 8`, comfortably under 16.
Sampling does change behavior in this commit, which is the point of it —
it runs at its construction bound instead of waiting on a gate.
What stays conservative is configuration acceptance, until the next task retires the rule.

**Every caller, listed rather than claimed.**
`grep -rn '\.Acquire(' --include='*.go' internal/ cmd/` finds five, of which one is production:

| Site | Kind | Action |
|---|---|---|
| `internal/pgo/rounds.go:452` | production, the only one | removed with the gate it calls |
| `internal/admit/gate_test.go:48`, `:77`, `:128` | the gate's own tests | removed with the method |
| `internal/httpapi/server_test.go:879` | a test that fills the shared gate to prove an interactive refusal | rewritten against `TryAcquire`, which is what an interactive request uses |

`TryAcquire` (`internal/admit/gate.go:26`) stays and keeps every caller it has.

- [ ] **Write the tests first**

| Case | Expect |
|---|---|
| a round with `maxParallel` samples in flight | a new sample starts only as one finishes, with no gate involved |
| `maxActiveCollections` Collections each at `maxParallel` | the process holds exactly `maxParallel × maxActiveCollections` fetches and no more |
| a saturated interactive gate for the whole round | every sample still starts and finishes; the gate is never consulted |
| `internal/httpapi/server_test.go:879` rewritten | a full gate still refuses an interactive request, proved through `TryAcquire` |
| `grep -rn ReasonSlotTimeout` | no hit outside the changelog |

The saturated-gate case is the one that fails against the current code,
so run it against `rounds.go` as it stands before removing anything.

`TestRoundsSlotTimeout` (`internal/pgo/rounds_test.go:807`) is deleted:
it proves a behavior the spec removes, and keeping it would pin the gate back in.
The `a store that refuses is artifact_store_failed` subtest of `TestRoundsFinishFailures`
and the late-`Put` fencing cases
(`internal/pgo/rounds_test.go:635-668`, `:1209-1292`) stay green and unedited;
they are what proves the rounds loop still ends a Collection correctly without the gate.

- [ ] **Run the tests and watch them fail**

- [ ] **Remove the gate from the rounds loop**

`internal/pgo/rounds.go` loses the `Gate` dependency, the `Acquire` at `:452`,
the `ReasonSlotTimeout` write at `:455`, and the constant at `:37`.
`internal/admit/gate.go:37` loses `Acquire`;
the `context` import goes with it if nothing else in the file needs it.

- [ ] **Take `slot_timeout` out of the published vocabulary**

| File | Change |
|---|---|
| `docs/api.md` | `slot_timeout` leaves the sample-result values |
| `docs/pgo.md:287` | the sample-reason examples name a reason that still exists |
| `CHANGELOG.md` | under `### Removed`, `slot_timeout` no longer appears in a manifest, because sampling no longer waits for an admission slot |

- [ ] **Validate and commit**

```bash
semlf check docs/api.md docs/pgo.md CHANGELOG.md
mise exec -- go test -race ./internal/admit/ ./internal/pgo/ ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/admit/ internal/pgo/ internal/httpapi/ docs/api.md docs/pgo.md CHANGELOG.md
git commit -m "feat(pgo): sample without an admission slot"
```

---

## One preset answers twelve ceilings

**Files:**
- Add: `internal/config/presets.go`, `internal/config/presets_test.go`,
  `internal/config/testdata/pgo-preset-small.yaml`,
  `internal/config/testdata/pgo-preset-override.yaml`,
  `internal/config/testdata/pgo-preset-unknown.yaml`
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `internal/config/testdata/pgo-full.yaml`,
  `cmd/profgate/main.go` and `cmd/profgate/main_test.go`, which are where `config validate` prints,
  `internal/config/config_test.go` and `internal/pgo/record_test.go`,
  which both assert against the grace-period figure being deleted,
  `docs/configuration.md`, `docs/pgo.md`, `docs/deployment.md`, `deploy/base/configmap.yaml`, `CHANGELOG.md`

*Presets* gives the three presets, their twelve values, the five derived figures, and the override rule.
*Configuration* gives `pgo.preset` its row, its environment variable, and its closed vocabulary.
*Container* gives the base term and the overflow check.

**What actually moves, checked against the tree rather than assumed.**
`internal/config/config.go:353-365` holds the twelve ceilings with their current defaults.
Comparing them with the `standard` column of *Presets*:

| Ceiling | Today | `standard` | Action |
|---|---|---|---|
| `maxDuration` | `60s` | `60s` | unchanged |
| `maxRounds` | `5` | `5` | unchanged |
| `maxParallel` | `4` | `4` | unchanged |
| `minEvery` | `15m` | `15m` | unchanged |
| `maxEvery` | `24h` | `24h` | unchanged |
| `maxRetention` | `24h` (`:359`) | `72h` | **moves**, and it is the only one that does |
| `maxSampleBytes` | `33554432` | `33554432` | unchanged |
| `maxMergedBytes` | `67108864` | `67108864` | unchanged |
| `maxTargetsPerRound` | `32` | `32` | unchanged |
| `maxActiveCollections` | `2`, `validate:"min=1"` (`:364`) | `2` | value unchanged; the range **gains `max=64`** |
| `onDemandPerMinute` | `10` | `10` | unchanged |
| `maxLiveCollections` | `64` | `64` | unchanged |

So an existing file that names no preset keeps eleven of its twelve ceilings exactly,
which is what *Presets* claims and what the changelog entry has to say about the twelfth.

**Expansion happens before validation, and the struct tags stop carrying the defaults.**
The twelve `default:` tags are what a preset now supplies,
so leaving them in place would make a preset's value indistinguishable from an operator's override:
both would arrive as a set field.
`PGOLimits` therefore keeps its `yaml`, `env`, and `validate` tags and loses its `default:` tags,
and the loader records which of the twelve the file or the environment actually set.
`applyPreset` then fills every unset ceiling from the named preset, before any `validate` rule runs,
so an override is validated by exactly the rules a preset value is (*Presets*, "Overrides").

**One function is added and one goes; none changes value.**
`CollectorMemoryBytes` joins `PGOMemoryBytes` (`internal/config/config.go:536`) rather than replacing it,
for the reason *What This Plan Decides* gives:
`PGOMemoryBytes` has two live agreement tests reading it,
and this commit cannot move the manifests that would keep them green.
It returns `collectorBaseMemory` plus the working set, under a check whose failure names the four ceilings.
`deploy/deploy_test.go:804` and `deploy/chart_test.go:388-421` therefore stay green and unedited here.
`RequiredPGOGracePeriod` (`:564`) is deleted in this same commit,
and `grep -rn RequiredPGOGracePeriod --include='*.go' .` finds three callers, not two:
`cmd/profgate/main.go:82` prints it,
`internal/config/config_test.go:912` asserts it equals `122465s`,
and `internal/pgo/record_test.go:382` asserts the deadline arithmetic against it.
The deadline those tests cover is unchanged;
what goes is the grace period derived from it, which *Shutdown* replaces with `pgo.leaseTTL + 30s`.

Four places tell an operator that figure exists, and they move in the same commit,
because a document promising output the binary no longer prints is worse than no document:

| Place | What it says today |
|---|---|
| `docs/configuration.md:560-561` | the sample output carries `required terminationGracePeriodSeconds for pgo: 122465` |
| `docs/pgo.md:321-325` | the PGO figure `config validate` prints is the period that lets the drain wait through any admissible deadline |
| `docs/deployment.md:420-424` | `config validate` prints *two* grace-period figures |
| `deploy/base/configmap.yaml:63-72` | a comment telling the operator an enabled gateway asks for `122465` |

The last of these is the one an operator acts on, and after the split it is wrong twice over:
a gateway replica holds no Collection, so it asks for nothing beyond its own drain.

**The rule that goes with it.**
`internal/config/config.go:869-871` is removed, for the reason the previous task gives.
The rule at `:874`, `pgo.limits.maxDuration <= limits.cpuSeconds`, stays and keeps its `pgo.enabled` gate.

- [ ] **Write the preset tests**

`internal/config/presets_test.go`, restating the preset rows of *Unit*:

| Configuration | Expect |
|---|---|
| no `pgo.preset` | `standard`, and the twelve resolved ceilings equal the `standard` column |
| `preset: small`, `preset: standard`, `preset: large` | the twelve ceilings equal that column exactly, asserted field by field |
| each of the three presets | passes every field range and every cross-field rule with `pgo.enabled: true` |
| `preset: medium` | rejected, naming the three admissible values |
| `PROFGATE_PGO_PRESET=large` over a file naming `small` | `large`, because the environment is applied over the file |
| `preset: small` with `maxTargetsPerRound: 24` | eleven ceilings from `small` and `maxTargetsPerRound` at `24` |
| `preset: large` with `maxTargetsPerRound: 33` | rejected: `maxRounds 8 × 33 > 256`, the record bound, with both keys named |
| `preset: small` with `maxActiveCollections: 65` | rejected by the new `max=64` |
| `preset: standard` with `maxRetention: 20h` | rejected: `maxRetention >= maxEvery` and `maxEvery` is `24h` |
| `PROFGATE_PGO_LIMIT_MAX_ROUNDS=1` over `preset: large` | one ceiling replaced, eleven from `large` |
| an unset ceiling under a preset | indistinguishable in the result from the same value written out by hand |
| a single-key override of each of the four sizing ceilings in turn | accepted, and the memory figure moves with it |
| each of the four sizing ceilings set outside its own range | rejected by that range, at every preset |

The derived figures, from the second table of *Presets*:

| Preset | working set | collector memory | fetches held open | per-Pod PGO fetches | live Collections |
|---|---|---|---|---|---|
| `small` | 1 GiB | 1280 MiB | 4 | 1 | 16 |
| `standard` | 4 GiB | 4352 MiB | 8 | 2 | 64 |
| `large` | 12 GiB | 12544 MiB | 32 | 4 | 256 |

Each row is a test case against the exported functions that compute them,
so the chart's comparison has one place to compare against.

The overflow check, from *Container*:

| Input | Expect |
|---|---|
| every preset | the limit is the table's figure, computed without overflow |
| ceilings whose product exceeds an `int64` | a failure naming all four ceilings, never a wrapped or negative count |

*Container* says the overflowing case is one the published ranges cannot reach,
so this case calls the computation directly rather than going through `Load`,
and the range check is asserted separately — the two layers are two tests,
which is only possible because validation and multiplication are separate functions.

- [ ] **Run the tests and watch them fail**

- [ ] **Land the preset, `CollectorMemoryBytes`, and the check**

- [ ] **Delete `RequiredPGOGracePeriod`, its three callers, and the four places that describe it**

- [ ] **`config validate` prints what an override produced**

*Presets* says it prints the preset name, the twelve resolved ceilings, the five figures,
and the collector grace period.
It stops printing the worst-deadline figure.
The test asserts the preset name, the moved `maxRetention`,
the collector memory figure, and `90s` at the shipped lease.

- [ ] **Update the two documents an operator reads**

| File | Change |
|---|---|
| `docs/configuration.md` | `pgo.preset` with the preset table; `pgo.limits` presented as overrides; `maxActiveCollections` bounded at 64; the removed cross-key rule; what `config validate` now prints |
| `CHANGELOG.md` | under `### Changed`, `pgo.limits` keys become overrides on `pgo.preset`, `pgo.limits.maxRetention` moves from `24h` to `72h` under `standard`, `maxActiveCollections` is bounded at 64, and the rule against `limits.maxConcurrentProfiles` is gone |

- [ ] **Validate and commit**

```bash
semlf check docs/configuration.md CHANGELOG.md
mise exec -- go test -race ./internal/config/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/config/ internal/pgo/ cmd/profgate/ deploy/base/ docs/ CHANGELOG.md
git commit -m "feat(config): answer twelve ceilings with one preset"
```

---

## The collector process exists

**Files:**
- Add: `cmd/profgate/collector.go`, `cmd/profgate/collector_test.go`,
  `internal/pgo/heartbeat.go`, `internal/pgo/heartbeat_test.go`,
  `internal/pgo/caches_test.go`, which does not exist today
- Modify: `cmd/profgate/client.go`, `cmd/profgate/client_test.go`, `cmd/profgate/serve.go`,
  `internal/pgo/publisher.go`, `internal/pgo/publisher_test.go`,
  `internal/pgo/scheduler.go`, `internal/pgo/scheduler_test.go`,
  `internal/pgo/caches.go`, `internal/pgo/worker.go`, `internal/pgo/worker_test.go`

**This task is additive: `serve` keeps running the loops.**
Nothing here removes anything from a running installation.
A cluster at this commit that also deploys a collector runs the loops in two places,
which is a two-collector overlap — the state a `RollingUpdate` of a one-replica collector already produces,
and which every mechanism in *Architecture* is there to absorb.
The cutover comes last, when the chart and the end-to-end harness both supply a collector.

**What `collector` wires.**
The Kubernetes preflight and the informers, because `Confirm` reads Pods;
the NATS preflight; the caches over `service.*`, `job.*`, `active.*`, and `schedule.*`;
`Publisher.Run`, because the scheduler publishes through the same publisher a gateway replica does;
the heartbeat writer; and the scheduler, the worker, and the sweeper.
It opens no API listener and no `auth`, `realms`, or `ui` machinery,
though it loads and validates every one of those keys (*Configuration*).
Its ops listener serves `/metrics`, `/healthz`, and `/readyz`,
and its readiness is the informer caches plus the NATS preflight (*Health*).

**Its drain.**
On `SIGTERM` it deletes its heartbeat key first, best effort and at its own revision,
because *Shutdown* has a draining collector reach gateway replicas sooner than staleness would;
a collector that is draining accepts no new work, so the key should go before the wait, not after it.
It then stops all three loops at once, stops every owner loop renewing its lease,
waits at most `leaseTTL - skewMargin` from the last renewal, and exits —
whether or not a work goroutine is still inside `Merge`, `Compact`, `Write`, or a `Put` (*Shutdown*).
It does not wait for the merge, and the plan does not add a knob that would let it.

**Where the subcommand is registered.**
`cmd/profgate/main.go:35-44` delegates to `dispatch`,
which lives with the verb namespace in `cmd/profgate/client.go:187-225`.
`collector` moves from `reservedOperatorNames` (`client.go:151`) into `operatorVerbs` (`:150`),
joins the usage line (`:180-186`), and gains its arm in `runOperator`.

**The writer's state machine, because a mutation can be indeterminate.**
A generation mismatch and `ErrUnavailable` both leave a write's outcome unknown (*The seam*).
The writer therefore holds either no revision or a revision it believes current:

| State and outcome | Next |
|---|---|
| no revision, `Create` succeeds | hold the returned revision |
| no revision, `Create` reports the key exists | `Get`, adopt the observed revision |
| no revision, `Create` indeterminate | reacquire a current-generation view, wait for its barrier, `Get`; adopt if present, else `Create` |
| held revision, `Update` succeeds | hold the returned revision |
| held revision, revision mismatch | `Get`, adopt if present, else drop to no revision |
| held revision, `Update` indeterminate | as for the indeterminate `Create` |
| the key deleted under it | drop to no revision and `Create` on the next tick |

The first write happens immediately after the replay barrier lifts, not on the first tick,
because *Shutdown* has an arriving collector reach gateway replicas as soon as its preflight passes.
A failed write is logged and retried on the next tick, and no loop waits on it.

- [ ] **Write the wiring tests**

`internal/pgo/publisher_test.go`:

| Case | Expect |
|---|---|
| `Run` on a context that ends | it returns, having stopped the pass |
| `Run` over several ticks | a tracked reservation is observably released, on the clock seam |
| the scheduler's tick | it no longer calls `ReleaseResolved`, proved by a publisher that records calls |

`internal/pgo/caches_test.go`, covering the kind, the roles, and the lifecycle repair together:

| Case | Expect |
|---|---|
| the gateway's set | four watches, `collector.` among them and `schedule.` absent |
| the collector's set | four watches, `schedule.` among them and `collector.` absent |
| either role | the barrier lifts after exactly its four markers and never waits on the inactive fifth kind |
| an open failing at each position in turn | the siblings already open are cancelled, `Run` returns, and the reopen loop is reached |
| an active watch closing unexpectedly | the barrier drops at once, the siblings stop, and the whole role set reopens once |
| a reopen at the same generation | every selected kind is unsynced and cleared before it reopens, not only the one that failed |
| a key deleted from a *sibling* prefix between the cancellation and the reopen | it is gone after the replay, at the same generation |
| the barrier across a reopen | it drops before any loop or handler can read the retained maps, and lifts only once all four new markers arrive |
| an empty prefix set | refused at construction rather than silently idle |

`internal/pgo/heartbeat_test.go`:

| Case | Expect |
|---|---|
| the first write after the barrier | `Create`, immediately, not on the first tick |
| the second | `Update` at the revision the first returned |
| `freshUntil` | `writtenAt + 2 × (leaseTTL / 3)`, at the shipped lease and at both ends of the 30s–10m range |
| each row of the writer's state machine | the next call is the one the table names |
| a generation moving during recovery | the writer waits for the new view's barrier before it reads |
| a failed write | logged, retried next tick, and no loop stalls |
| a value 44s and 46s old at the shipped lease | present, then absent |
| a value carrying no `freshUntil` | stale |
| a newer value whose short window has expired beside an older one whose long window has not | present, because availability is any fresh window and not the newest `writtenAt` |
| a graceful shutdown | the key is deleted at its revision, and a failure to delete changes no answer |
| two instances writing at once | two keys, and deleting one leaves the other fresh |

`cmd/profgate/client_test.go`:

| Case | Expect |
|---|---|
| `collector` | dispatched through `runOperator`, exactly once in the operator namespace |
| `collect` | still parsed as the client verb, with its flags unchanged |
| the usage line | both names, distinctly |
| a global client flag before `collector` | refused, without changing how `collect` parses one |

`cmd/profgate/collector_test.go`:

| Case | Expect |
|---|---|
| a valid configuration with `pgo.enabled` | the three loops, `Publisher.Run`, and the heartbeat writer start after both preflights |
| a tracked reservation left indeterminate, a scheduler injected that never ticks, the clock advanced | `Publisher.Run` resolves and releases it, which is what proves the pass runs without a scheduler tick |
| `pgo.enabled: false` | a non-zero exit naming `pgo.enabled` |
| the API port | nothing binds it; the ops port binds |
| a NATS preflight that fails a permission | a non-zero exit naming the bucket and the operation |
| `SIGTERM` with an owner loop past its cutoff | the process exits within the window and writes no terminal record |
| `SIGTERM` with an owner loop inside its cutoff | the Collection commits normally before the process exits |
| `SIGTERM` with an owner still inside the drain window | the heartbeat key is already absent while the process is still waiting |
| `SIGTERM` with the delete failing | the drain proceeds unchanged |
| a `Put` landing after the process returned | no terminal record names it, and the sweeper removes it |
| `--config` missing | the same usage failure `serve` gives |

- [ ] **Run the tests and watch them fail**

- [ ] **Give the publisher its own loop**

- [ ] **Give the cache its role-specific prefix set, the `collector.` kind, and the lifecycle repair**

- [ ] **Land the heartbeat writer**

- [ ] **Add the subcommand and register it**

`serve` gains the `collector.` prefix in the set it passes and `Publisher.Run`,
and keeps the three loops until the cutover.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/pgo/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/pgo/ cmd/profgate/
git commit -m "feat(pgo): add the collector process"
```

---

## The chart renders a collector and sizes it

**Files:**
- Add: `deploy/chart/profgate/templates/collector.yaml`
- Modify: `deploy/chart/profgate/templates/_helpers.tpl`,
  `deploy/chart/profgate/templates/deployment.yaml`,
  `deploy/chart/profgate/templates/podmonitor.yaml`,
  `deploy/chart/profgate/templates/networkpolicy.yaml`,
  `deploy/chart/profgate/templates/prometheusrule.yaml`,
  `deploy/chart/profgate/templates/NOTES.txt`,
  `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md`,
  `deploy/chart_test.go`, `docs/deployment.md`, `CHANGELOG.md`

*Deployment* gives the collector Deployment's shape and its network flows;
*Container* gives the arithmetic and the two rejection lists;
the gateway spec's *Build and Deployment* gives the labels, the `PodMonitor`, and the per-role policies.

**The rename, with every site listed.**
`grep -rn memoryLimitWithoutPGO deploy/` finds six.
The value is the gateway's limit while PGO is off;
after the split it is the gateway's limit under both states, so the name is wrong in the one case it distinguished.
It becomes `gatewayMemoryLimit`:

| Site | Action |
|---|---|
| `deploy/chart/profgate/values.yaml:147` | the key is renamed, value `512Mi` unchanged |
| `deploy/chart/profgate/templates/_helpers.tpl:269` | renamed in place; the `pgoEnabled` branch above it at `:264-269` **stays** until the cutover, for the reason below |
| `deploy/chart/profgate/README.md:128`, `:503` | the prose and the values table |
| `deploy/chart/profgate/templates/NOTES.txt:142` | the sentence contrasting it with a derived figure becomes the collector's note |
| `deploy/chart_test.go:591` | the assertion and its message |

**The gateway keeps the derived figure until it stops collecting.**
`profgate.resources` (`deploy/chart/profgate/templates/_helpers.tpl:257-270`) branches today:
a PGO-enabled gateway gets `profgate.pgoMemoryBytes`, and a disabled one gets the static value.
Collapsing that branch here would render every gateway replica at 512Mi
while `serve` still runs the scheduler, the worker, and the sweeper —
three tasks before the cutover takes them away.
An installation upgraded at this commit would merge whole decoded profiles in a container sized for not merging,
which is an out-of-memory kill rather than a test failure.
So this task renames the value and leaves the branch alone.
The cutover commit collapses it, because that is the commit where "the gateway holds no decoded profile" becomes true.

**The chart gains a second helper, mirroring the binary's second function.**
`profgate.pgoMemoryBytes` (`_helpers.tpl:200-245`) computes the working set and stays exactly as it is,
so the gateway's existing branch and its existing agreement test keep their value.
A second helper adds the 256 MiB base to it and is what the collector Deployment renders,
matching `CollectorMemoryBytes` on the Go side.
Two helpers and two functions is the shape *Presets* describes,
and it is what lets one commit size two roles differently.

**The agreement test is copied, not moved.**
`deploy/chart_test.go:395`, `TestChartMemoryLimitIsDerived`,
already compares the rendered limit against `config.PGOMemoryBytes()` over the ConfigMap the same render produced,
through `loadRenderedConfig`.
A second case joins it, pointed at the collector Deployment and at `CollectorMemoryBytes`,
keeping `loadRenderedConfig` exactly as the existing one does.
The existing gateway case stays green and unedited, because the gateway's limit has not moved yet.
The cutover commit is where the gateway case goes, together with the branch it was pinning.
Its cases widen from two to the matrix *Unit* names:
each preset, a single-key override of each of the four sizing ceilings,
and a rejection outside each of those four ranges in Helm and in `config.Load` alike.

**Bounding `maxActiveCollections` closes the chart's only route to its own overflow branch.**
`deploy/chart_test.go:479` reaches it today by setting that ceiling to `9007199254740992`,
a value the new `max=64` refuses before the multiplication is ever formed.
*Container* requires both arithmetic layers to refuse an overflowing product, so the coverage cannot simply go.
The chart's checked multiplication moves into a helper that a test-only template renders directly,
which is the same separation the preset task makes in the binary,
and for the same reason: the case is unreachable through the public range check.

- [ ] **Write the chart tests first**

| Values | Expect |
|---|---|
| `pgo.enabled: false` | no collector Deployment, no collector NetworkPolicy, no collector alert |
| `pgo.enabled: true` | one collector Deployment, `component: collector`, ops port only, no API port |
| `pgo.enabled: true` | no Service selects the collector, and no PodDisruptionBudget names it |
| `pgo.collector.replicaCount` unset, and set to `2` | `1` and `2` respectively |
| each of the three presets | `resources.limits.memory` is the *Presets* figure — 1280Mi, 4352Mi, 12544Mi |
| a single-key override of each sizing ceiling | the rendered limit equals `CollectorMemoryBytes` over the rendered ConfigMap |
| each sizing ceiling outside its range | the render fails, and `config.Load` refuses the same value |
| `pgo.limits.maxActiveCollections` at `0` and at `65` | the render fails on the range, at both ends |
| the chart's multiplication helper rendered directly with an overflowing product | it refuses, naming the four ceilings, rather than rendering a wrapped number |
| `pgo.leaseTTL: 60s` and `120s` | `terminationGracePeriodSeconds` is `90` and `150` |
| the gateway Deployment with `pgo.enabled: true` | still the derived figure, because it is still collecting |
| the gateway Deployment with `pgo.enabled: false` | the static `gatewayMemoryLimit`, unchanged |
| the gateway Deployment under both states | the same `terminationGracePeriodSeconds: 125` |
| `extraEnv` naming any of `PROFGATE_PGO_PRESET`, `PROFGATE_PGO_LIMIT_MAX_PARALLEL`, `PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES`, `PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES`, `PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS` | the render fails, naming the structured value to set instead |
| `config.pgo.preset` or any of the four keys under `config.pgo.limits` | the render fails the same way |
| `pgo.enabled: true` | the `PodMonitor` selects both roles by the common name label with no `component` |
| `pgo.enabled: true` | two NetworkPolicies, the collector's admitting the ops port and nothing else |
| `pgo.enabled: true` | the `PrometheusRule` carries the collector-availability alert, on the gauge at `0` for five minutes |
| both roles | `app.kubernetes.io/component` is present and differs |
| the collector's command | exactly `profgate collector --config <the mounted path>` |
| the collector's volumes | the same ConfigMap at the same path the gateway mounts |
| the collector's NATS credentials | the Secret mounted at `/etc/profgate/nats/` with `readOnly: true` and `defaultMode: 0440`, and pod `fsGroup: 65532` |
| the collector's container context | the same hardened context the gateway carries |
| an `extraEnv` entry naming no sizing variable | rendered onto both roles, so the rejection does not refuse the whole surface |
| a non-sizing key under `config.pgo.limits` | rendered, for the same reason |

- [ ] **Run the tests and watch them fail**

- [ ] **Land the template, the collector's helper, and the rejections**

The collector Deployment renders the new helper,
`profgate.pgoMemoryBytes` plus the 256 MiB base, under the same overflow check.
The gateway Deployment is not touched.

- [ ] **Rename the value in the six places above**

- [ ] **Update the chart README, `NOTES.txt`, and the deployment guide**

`docs/deployment.md` gains the collector Deployment:
what it needs, what it does not serve, how it is scraped, the egress it needs,
and the application-side policy that must admit both roles.
`CHANGELOG.md` records the renamed chart value and the new Deployment.

- [ ] **Validate and commit**

```bash
semlf check deploy/chart/profgate/README.md docs/deployment.md CHANGELOG.md
mise exec -- go test ./deploy/
mise run lint && mise run test && mise run check
git add deploy/ docs/deployment.md CHANGELOG.md
git commit -m "feat(chart): render and size the collector"
```

---

## The kustomize tree and the policy that admits both roles

**Files:**
- Add: `deploy/collector.yaml`, `deploy/collector-networkpolicy.yaml`
- Modify: `deploy/networkpolicy-app-example.yaml`, `deploy/deploy_test.go`,
  `docs/deployment.md`

*Deployment*, "The kustomize tree", puts both new files in `deploy/` and **outside**
`deploy/base/kustomization.yaml`:
a base has no conditional, `pgo.enabled` is false by default,
and an unconditional collector would give every plain-kustomize install a Pod for a disabled feature.

The application-side change is the split's most likely deployment mistake and gets its own attention.
`deploy/networkpolicy-app-example.yaml` admits the gateway's pprof connections to an application Pod.
A rule naming only the gateway would leave every Collection failing `no_samples`
while the interactive path kept working.
Its peer selector becomes the common `app.kubernetes.io/name: profgate` with no `component`,
and its comment says that narrowing it to one role breaks the other.

- [ ] **Write the manifest tests**

| Case | Expect |
|---|---|
| `deploy/base/kustomization.yaml` | names neither new file |
| `kustomize build deploy/base` | renders no collector, exactly as today |
| `deploy/collector.yaml` | `component: collector`, ops port only, the same ServiceAccount as the gateway |
| `deploy/collector.yaml` | `profgate collector --config`, the shared ConfigMap volume, the credentials Secret at `0440` and read-only, and `fsGroup: 65532` |
| `deploy/collector-networkpolicy.yaml` | ingress on the ops port from the monitoring namespace and nothing else; egress to the API server, NATS, DNS, and application Pod ports |
| `deploy/networkpolicy-app-example.yaml` | its peer selector carries no `component` key |
| `TestClusterRoleTuples` | unchanged and green |

- [ ] **Run the tests and watch them fail**

- [ ] **Add the two files and widen the example**

- [ ] **Validate and commit**

```bash
semlf check docs/deployment.md
mise exec -- go test ./deploy/
mise run lint && mise run test && mise run check
git add deploy/ docs/deployment.md
git commit -m "feat(deploy): ship the collector outside the base"
```

---

## The end-to-end suite runs a collector

**Files:**
- Modify: `test/e2e/registry.go`, `test/e2e/harness_test.go`

Nine PGO scenarios exist (`test/e2e/registry.go:32-40`),
and every one of them runs against a gateway that also collects.
This task gives them a collector as well, and changes no assertion:
the two together are the overlap the coordination mechanisms already absorb,
and it happens here for one reason:
the suite proves the collector runs before the next task takes the loops out of `serve`.

- [ ] **Give the harness a collector**

The PGO overlay renders the collector Deployment beside the gateway
and waits for it to become Ready before a PGO scenario starts.

- [ ] **Run the PGO scenarios**

```bash
PROFGATE_E2E_KEEP=1 mise exec -- go test -tags e2e -count=1 -timeout 30m -run 'TestScenarios/pgo' ./test/e2e/
```

- [ ] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add test/e2e/
git commit -m "test(e2e): deploy a collector beside the gateway"
```

---

## The gateway stops collecting and starts checking

**Files:**
- Modify: `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`,
  `internal/pgo/heartbeat.go` and its test, `internal/pgo/sweeper.go` and its test,
  `internal/httpapi/` create handler and its test, `internal/metrics/`,
  `test/e2e/registry.go`, `test/e2e/scenarios_pgo_test.go`,
  `deploy/chart/profgate/templates/_helpers.tpl` and `deploy/chart_test.go`, for the gateway's memory branch,
  `docs/api.md`, `docs/pgo.md`, `CHANGELOG.md`

**This commit is atomic on purpose, and the reason is worth stating.**
The loops inside `serve` write no heartbeat.
So a gateway that still runs them and already checks for one would refuse `POST /collections`
while it was itself collecting —
a false `503 collector_unavailable` in the only configuration that shipped before this plan.
Removing the loops and adding the check are therefore one change, not two.
By this point the chart renders a collector, `deploy/` carries one, and the end-to-end suite deploys one,
so nothing in the tree loses its collector when `serve` gives the loops up.

**What `serve` loses.**
Its derived memory limit goes with the loops:
the chart's `pgoEnabled` branch in `profgate.resources` collapses to `gatewayMemoryLimit` in this commit,
and the gateway half of the chart's memory agreement test goes with it,
because only now does a gateway replica hold no decoded profile.
`cmd/profgate/serve.go:510-517` starts the three loops once the NATS preflight passes;
the branch keeps `Publisher.Run` alone.
The Collection drain goes with them:
`drainAll` and `abandonCollections` collapse to one shutdown path,
`reportDraining` (`serve.go:540`) is deleted,
the `collectionWorker` interface (`serve.go:80`) and the `pgoWorker` dependency (`serve.go:97`) go,
and `stubWorker` (`serve_test.go:1364`) goes with them.
The gateway's `terminationGracePeriodSeconds` stops depending on `pgo.enabled`.

**What `serve` gains.**
The heartbeat reader over the `collector.*` cache it has held since the collector process landed,
the scrape-time gauge, and the refusal.
`profgate_pgo_collector_available` is evaluated at scrape rather than on a watch event,
because a heartbeat goes stale with no event to announce it;
`internal/metrics/prometheus.go:127-149` already does exactly this for the JWKS-age gauge,
and the collector gauge follows it, over the cache and an injected clock.
The refusal's authoritative negative check is as *What This Plan Decides* sets out.

- [ ] **Write the tests first**

`cmd/profgate/serve_test.go`:

| Case | Expect |
|---|---|
| `pgo.enabled` and the preflight passing | `Publisher.Run` starts and no scheduler, worker, or sweeper does |
| a tracked reservation left indeterminate and the clock advanced | it is resolved and released, with no scheduler in the process at all |
| `SIGTERM` during an in-flight request | the request drains and nothing waits on a Collection |
| the listener failing | one shutdown path, with no Collection distinction |
| `pgo.enabled: false` | unchanged: no NATS connection and `501 pgo_disabled` |
| the rendered gateway Deployment under both `pgo.enabled` states | the same static `gatewayMemoryLimit`, now that no gateway replica merges |

The gauge and the refusal:

| Case | Expect |
|---|---|
| a fresh key in the cache | the gauge is `1` |
| only the fake clock advancing, with no delivery and no request | a scrape moves the gauge from `1` to `0` at the freshness boundary |
| no fresh key in the cache, and none in the store | `503 collector_unavailable`, writing nothing, naming no instance |
| no fresh key in the cache, one committed but not yet delivered | accepted, through the authoritative check |
| a key vanishing between `Keys` and its own `Get` | the scan continues; it is an absent candidate, not an error |
| the view moving under the scan | `503 pgo_unavailable` |
| a keyed create, then every heartbeat removed, then the same key sent again | `200` with the original identifier, having read no heartbeat at all |
| the same key with a moved snapshot, every heartbeat removed | `409 idempotency_mismatch`, also ahead of the collector check |
| a replica whose `collector.*` watch is still replaying | `503 pgo_unavailable`, never a false absence |
| no fresh key | `PUT /pgo`, `DELETE /pgo`, `GET /pgo`, both listings, the record route, `GET .../profile`, and `POST .../cancel` answer as they do today |

The sweep:

| Case | Expect |
|---|---|
| a key whose `writtenAt + 10m + skewMargin` has passed | deleted by the sweeper |
| a key one second inside that age | kept |
| the sweep failing | warned, and the pass continues, as the probe sweep already does |

- [ ] **Run the tests and watch them fail**

- [ ] **Take the loops out of `serve` and add the reader, the gauge, and the refusal**

`collector_unavailable` is answered by `POST /collections` alone.
`internal/httpapi/codes.go:90-92` already declares `CodeCollectorUnavailable`,
registered at `:145` and commented "No route answers it in this build".
This change makes it a code a route does answer, so that comment goes
and the OpenAPI document gains the response.

- [ ] **Write the three end-to-end scenarios**

| Scenario | What it proves |
|---|---|
| a collector rolling update | two collectors overlap, two heartbeat keys coexist, the slot key's `Create` fires one slot once, an interrupted Collection is reclaimed as a new attempt, and draining one leaves the gateway available from the other |
| an enabled installation with no collector | `POST /collections` answers `503 collector_unavailable`, `PUT /pgo` still succeeds, and the gauge reads `0` |
| a collector arriving afterwards | the gauge reaches `1`, the create is accepted, and the policy stored while no collector existed is scheduled |

The second is what a `pgo-disabled`-shaped scenario cannot cover:
`pgo.enabled` is true throughout, and the absence is the collector's, not the feature's.

- [ ] **Run the whole suite**

```bash
PROFGATE_E2E_KEEP=1 mise exec -- go test -tags e2e -count=1 -timeout 30m ./test/e2e/
```

- [ ] **Update what a client reads**

| File | Change |
|---|---|
| `docs/api.md` | `503 collector_unavailable` on `POST /collections`, and what distinguishes it from `pgo_unavailable` |
| `docs/pgo.md` | the heartbeat as the thing that tells an operator collection has silently stopped |
| `CHANGELOG.md` | under `### Added`, `503 collector_unavailable` joins the `POST /collections` answers |

- [ ] **Validate and commit**

```bash
semlf check docs/api.md docs/pgo.md CHANGELOG.md
mise exec -- go test -race ./internal/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/ cmd/profgate/ test/e2e/ docs/api.md docs/pgo.md CHANGELOG.md
git commit -m "feat(pgo): move collection out of the gateway"
```

---

## The guides, the changelog, and this plan's own status

**Files:**
- Modify: `docs/pgo.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/api.md`,
  `CHANGELOG.md`, `docs/plans/roadmap.md`, this plan

The earlier tasks each moved the documents their own change touched.
This task is the sweep that catches what only reads right once every task has landed,
and the status flip [`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) requires.

- [ ] **Read the four guides end to end against the shipped behavior**

| File | What is left to reconcile |
|---|---|
| `docs/pgo.md` | what runs where; the *Multiple gateway replicas* drain paragraph becomes what a rollout of either Deployment does; the memory formula sizes the collector |
| `docs/configuration.md` | `profgate collector` joins the subcommand list |
| `docs/deployment.md` | the two Deployments read as one installation rather than two additions |
| `docs/api.md` | the changes made in earlier tasks read consistently in one pass |

- [ ] **Consolidate the changelog**

Every entry the earlier tasks added is already under `## [Unreleased]`;
this step checks that an operator upgrading can read the breaking half in one place:
the renamed chart value, the `pgo.limits` keys becoming overrides,
`maxRetention` moving under `standard`, and `slot_timeout` leaving the manifest.

- [ ] **Flip the roadmap and this plan**

`docs/plans/roadmap.md` item 11's `Shipped:` line names what carried it.
This plan's line 3 becomes `**Status:** Done` and line 4 becomes an `Outcome:` naming the same thing.
The plan is deleted in the next commit that touches it, per
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md);
one commit cannot do both.

- [ ] **Validate and commit**

```bash
semlf check docs/pgo.md docs/configuration.md docs/deployment.md docs/api.md CHANGELOG.md docs/plans/
mise run lint && mise run test && mise run check
git add docs/ CHANGELOG.md
git commit -m "docs: describe the two-process installation"
```

---

## Risks and What This Plan Does Not Cover

**An operator upgrades and collection stops silently.**
This is the change's real hazard, and it has three edges.
In a cluster enforcing NetworkPolicy,
an application-side rule that names the gateway leaves every Collection failing `no_samples`;
the widened example and the guide paragraph are the mitigation, and the heartbeat is not —
a collector that cannot reach a Pod is present and failing, not absent.
An operator who pinned a sizing ceiling through `extraEnv` finds the render refusing;
the message names the structured value, which is the whole reason the rejection is at render time.
`profgate_pgo_collector_available` and its alert report a collector that never scheduled at all.

**The memory arithmetic lives in two languages.**
The chart computes the limit and the binary computes the same figure,
and a Helm template and Go can drift.
The `deploy/` agreement test is the only thing that stops them,
which is why it compares at every preset and at every single-key sizing override rather than at one point.

**`decodeFactor` is an estimate, not a measurement.**
*Container* says so plainly.
A collector that is OOM-killed at a preset's limit is evidence about the factor, not about this plan,
and the response is a spec revision rather than a chart edit.

**A rolling update costs an in-flight Collection an attempt.**
*Shutdown* accepts this and *Recovery* bounds it by `pgo.maxAttempts`.
An operator who rolls the collector more often than a Collection takes will exhaust attempts,
and *Failure Scenarios* carries the row.
Nothing in this plan changes it.

**One intermediate commit runs the loops in two places.**
Between the task that lands the collector process and the task that takes the loops out of `serve`,
an installation deploying both runs `gatewayReplicas + collectorReplicas` loop sets.
That is an overlap the coordination mechanisms absorb by design, and it is deliberate:
the alternative is a commit where nothing collects.
It is not a state any release ships, because the tasks land together.

**Not covered:** any change to the interactive request path,
any change to the realm model or to authentication,
a collector that serves HTTP,
a second collector Deployment per realm or per namespace,
and any widening of the Kubernetes or NATS permission the gateway holds today.

---

## Self-Review

- Every claim about the tree was run, not remembered:
  `internal/config/config.go:353-365` holds the twelve ceilings and `:359` is the one whose default moves;
  `:364` carries `validate:"min=1"` with no maximum;
  `:869-871` is the rule against `limits.maxConcurrentProfiles` and `:874` is the one that stays;
  `:232` sets `limits.maxConcurrentProfiles` to `16`, which is what makes `large` incompatible with `:869`;
  `:536` is `PGOMemoryBytes`, with no base term and no overflow check,
  and `deploy/deploy_test.go:804` and `deploy/chart_test.go:388-421` both read it,
  which is why a second function is added rather than that one changed;
  `:564` is `RequiredPGOGracePeriod`, the figure *Shutdown* deletes,
  called from `cmd/profgate/main.go:82`, `internal/config/config_test.go:912`, and `internal/pgo/record_test.go:382`,
  and described in `docs/configuration.md:560-561`, `docs/pgo.md:321-325`, `docs/deployment.md:420-424`,
  and `deploy/base/configmap.yaml:63-72`;
  `internal/pgo/rounds.go:452` is the only production `Acquire` and `:37` and `:455` are the reason it writes;
  `internal/httpapi/server_test.go:879` is the only other non-gate caller;
  `internal/pgo/scheduler.go:156` is the only caller of `ReleaseResolved`;
  `internal/pgo/caches.go:154-175` declares four kinds with fixed-size arrays and `:479-490` waits on all of them,
  and `:282-314` is the watch lifecycle the role split exposes;
  `internal/pgo/clock.go` already gives `Now` and `NewTicker`, so no seam is added;
  `cmd/profgate/client.go:150-151` holds `operatorVerbs` and `reservedOperatorNames = [...]string{"collector"}`,
  and `collectVerb()` in the same file is the client verb, so `collect` is taken;
  `cmd/profgate/main.go:35-44` delegates to `dispatch`, which lives in `client.go`, not in `main.go`;
  `deploy/chart_test.go:395` is `TestChartMemoryLimitIsDerived`, which already compares against `PGOMemoryBytes`;
  `deploy/chart/profgate/values.yaml:147` and five other sites name `memoryLimitWithoutPGO`;
  `values.yaml:163` sets the gateway's grace period to `125`;
  `internal/metrics/prometheus.go:127-149` is the scrape-time gauge the collector gauge copies;
  `internal/httpapi/codes.go:90-92` declares `CodeCollectorUnavailable` and `:145` registers it,
  with a comment saying no route answers it;
  `test/e2e/registry.go:32-40` holds nine PGO scenarios, and `:41` is `tls-rotation`;
  `internal/pgo/policy.go:321` and `internal/config/config.go:396` show the retention rule already shipped,
  which is why it is in *What This Plan Leaves Alone* rather than in a task.
- One task amends an `Accepted` spec, and it says so where it happens:
  *Collector availability*'s freshness rule is unsound across two independently rolling Deployments,
  which is a defect this plan creates rather than one it inherits,
  so repairing it is part of the plan rather than a note in *Risks*.
- Decided here because the spec leaves it to the implementer:
  the refusal performs one authoritative store read on a negative cache result while the gauge stays cache-derived;
  the heartbeat writer holds an explicit revision state machine over indeterminate mutations;
  the first heartbeat is written at the barrier rather than at the first tick;
  the cache takes its prefix set rather than deriving it from a role flag;
  a collector started with `pgo.enabled: false` exits non-zero rather than idling;
  `ReleaseResolved` keeps its name and `Run` is the loop around it, so its tests keep their subject;
  the chart value is renamed `gatewayMemoryLimit`;
  the preset table is expressed as data rather than as twelve conditionals;
  the twelve `default:` struct tags are dropped so that an override is distinguishable from a preset value;
  validation and multiplication are separate functions, so the overflow case can be reached without `Load`.
- Left to the implementer by design: helper and fixture names,
  the exact wording of every message beyond the values it must name,
  the layout of `internal/pgo/heartbeat.go`,
  and the internal split of `cmd/profgate/collector.go`.
- The order is a dependency order, and every step of it answers one constraint —
  every commit leaves an installation that collects:
  the specs are repaired before anything reads them;
  the gate leaves the sampling path before the rule that bounded it,
  because the other order accepts a bound the running code cannot execute;
  the preset lands before the chart, because the agreement test needs both sides;
  the collector process, the chart, and the harness all supply a collector before `serve` gives the loops up;
  that is why the last change is one commit and not three.
