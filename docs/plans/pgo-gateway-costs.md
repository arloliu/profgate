# What PGO Costs the Gateway It Runs In

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Every task is test-first,
> and each begins by adding the declarations its tests name — types, method stubs, generalized test helpers —
> so its first run fails on assertions rather than on compilation.
> Checkboxes (`- [ ]`) track progress.
> The accepted specs outrank this plan:
> where the two differ the spec wins and the plan is the bug.

**Goal:** stop PGO collection charging a gateway replica for costs it does not have to pay.
A replica draining with a Collection in flight stops waiting for the merge and lets the lease expire,
so `pgo.enabled` no longer implies a termination grace period no operator would set.
Sampling stops taking a slot from the gate interactive requests pass through.
Three of the four ceilings that size the working set drop to values this deployment actually needs,
and the container stops being sized as though the gateway's own footprint were free,
which together take the limit from 4 GiB to 1.5 GiB.

**Architecture:** nothing moves between processes.
`internal/pgo` keeps every loop, and `cmd/profgate/serve.go` keeps starting them.
`internal/pgo/worker.go` gains the drain that stops at the lease cutoff;
`internal/pgo/rounds.go` loses the admission gate and the reason it wrote;
`internal/admit` loses `Acquire` and keeps `TryAcquire`;
`internal/config` loses `RequiredPGOGracePeriod` and one cross-key rule,
lowers three defaults, and gains the gateway's own base term.
`deploy/chart/profgate/templates/_helpers.tpl` adds that term instead of choosing between it and the working set,
and `deploy/base/deployment.yaml` follows the figure down.
Nothing is added under `deploy/`, and nothing is gained by
`internal/k8s`, `internal/proxy`, `internal/auth`, `internal/natskv`, `internal/ui`, or `internal/client`.

**Tech Stack:** everything already pinned in [`mise.toml`](../../mise.toml).
**No Go module is added.**

**Spec:** [`docs/specs/pgo.md`](../specs/pgo.md), `Accepted`, carries the first two changes and not the third.

*Shutdown* bounds a drain at `leaseTTL - skewMargin` from the last renewal
and says nothing about `pgo.enabled` lengthens a gateway replica's grace period.
*Rounds* states the sampling bound as `maxParallel × maxActiveCollections` by construction,
with no shared gate and no `slot_timeout`.
*Configuration* removes the cross-key rule that measured a PGO ceiling against `limits.maxConcurrentProfiles`.
Both of the first two sentences are false against the code today,
so those tasks close a gap between the spec and the tree rather than opening one.

The sizing is different, and an implementer should not read it as a spec requirement.
*Presets* keeps the current ceilings for a configuration that names no preset,
and *Container* gives a gateway replica a static limit **because collection runs elsewhere**.
Neither holds while the loops stay in the gateway.
The lowered defaults and the 512 MiB in-process base come from
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md),
which is where the measurement behind them lives.
Sections are cited by heading name, never by number;
an unqualified heading is the PGO spec's.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md) item 11.
Why the collector Deployment that *Architecture* designs is not built here, and what would revive it:
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- **The permission invariant does not move.**
  No task edits `internal/k8s`, so the seven read tuples stay seven,
  and no task touches the NATS account fragment or adds a key prefix.
  `TestClusterRoleTuples` in `deploy/deploy_test.go`
  and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go` stay green and unedited.
- **Coordination is not removed.**
  The lease, the claim, the reclaim, the slot key, the active key, and the sweeps all stay.
  The drain change *relies* on them: a replica that stops renewing is reclaimed by another,
  which is only safe because that machinery already runs in every replica.
- **No process is added and none is removed.**
  `profgate collector` is not built here; `collector` stays a reserved operator name
  (`cmd/profgate/client.go:151`).
- **Every commit leaves an installation that collects, and none accepts what it cannot execute.**
  The gate leaves the sampling path before the rule that bounded it, in the same commit.
  The derived memory limit and the manifest literal that pins it move together.
- **A Collection interrupted by a rollout costs one attempt, and that is the whole trade.**
  *Shutdown* accepts it and *Recovery* bounds it by `pgo.maxAttempts`.
  No task adds a knob to wait longer.
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
| the collector Deployment, the `collector` subcommand, and role-specific caches | [`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md) |
| the `collector.<instance>` heartbeat, `503 collector_unavailable`, the availability gauge and its alert | they detect a state that cannot occur while the loops run in the gateway |
| `pgo.preset` and the collapse of twelve ceilings into one name | the same record: the twelve are not one axis, so three lower defaults and a sizing table replace it |
| the chart's `pgoEnabled` memory branch | a gateway that collects is still sized for collecting; only the figure gets smaller |
| the `artifact.retention >= schedule.every` rule | already shipped: the code is declared at `internal/pgo/policy.go:321` and the rule applied at `:397-405` |
| the watch lifecycle in `internal/pgo/caches.go:282-314` | a defect independent of everything here, tracked as its own item in [`roadmap.md`](roadmap.md) |

---

## What This Plan Decides

### The drain stops renewing rather than waiting, and reclaim does the rest

*Shutdown* specifies this for a collector process.
The mechanism does not depend on which process runs the loops:
an owner that stops renewing its lease is reclaimed by the next scan,
and `internal/pgo/worker.go` already implements that path as `lease_lost`.
A gateway replica therefore drains the same way,
entering that existing path by a signal rather than by a slow renewal.
No state, no reason, and no rule is added.

What this costs is stated where it lands:
a Collection interrupted by a rollout restarts from round 0 as a new attempt.
`deploy/base/configmap.yaml:63-72` already tells an operator that this is what a 125-second grace period buys,
so the behavior stops being a documented deviation and becomes the design.

### `RequiredPGOGracePeriod` is deleted rather than corrected

It expands the ceilings into the deadline of the slowest Collection they admit,
and returns 122465 seconds — a little over 34 hours — for the shipped ones
(`internal/config/config_test.go:912`).
Once the drain stops waiting for a Collection, no figure derived from a Collection's deadline means anything:
the answer is the gateway's own drain, which `deploy/chart/profgate/values.yaml:163` already ships as 125.
`grep -rn RequiredPGOGracePeriod --include='*.go' .` finds three callers,
and four documents describe the figure; all seven move in one commit.

### `slot_timeout` leaves the vocabulary rather than becoming unreachable

`internal/pgo/rounds.go:37` declares `ReasonSlotTimeout` and `:455` is its only writer,
reached only from the `Gate.Acquire` at `:452`.
Removing the gate makes the constant dead, and a dead reason in a published vocabulary is worse than no reason:
`docs/api.md` and `docs/pgo.md:287` list it as something an operator may see in a manifest.

### The cross-key rule goes in the same commit as the gate

`internal/config/config.go:869-871` refuses a configuration whose
`maxParallel × maxActiveCollections` reaches `limits.maxConcurrentProfiles`.
It guards a shared pool, and after the gate is gone there is no shared pool to guard.
The two are one concept and land together;
keeping the rule would leave a restriction whose stated reason had been deleted.
The rule at `:874`, `pgo.limits.maxDuration <= limits.cpuSeconds`, is unrelated and stays.

### The limit gains a base term, three defaults fall, and the manifest moves with both

`PGOMemoryBytes` (`internal/config/config.go:536`) keeps its formula and its meaning:
it is the PGO working set, and nothing else.
What the container needs is that plus the gateway's own footprint,
and the chart chooses between the two today rather than adding them
(`deploy/chart/profgate/templates/_helpers.tpl:264-269`).
Three of the four inputs also get smaller defaults;
`maxParallel` keeps the `4` it already has.
Together those take the working set from 4 GiB to 1 GiB and the container limit to 1536Mi.
Two places compare the container against a manifest:
`deploy/deploy_test.go:804` against the literal in `deploy/base/deployment.yaml:51-57`,
and `deploy/chart_test.go:388-421` against the chart's own arithmetic.
The chart derives its number and follows automatically;
the base manifest carries `4Gi` as text and must be edited in the same commit or that commit is red.

---

## The drain stops waiting for a merge

**Files:**
- Modify: `internal/pgo/worker.go`, `internal/pgo/worker_test.go`,
  `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`,
  `internal/config/config.go`, `internal/config/config_test.go`,
  `internal/pgo/record_test.go`,
  `cmd/profgate/main.go`, `cmd/profgate/main_test.go`,
  `docs/pgo.md`, `docs/deployment.md`, `docs/configuration.md`,
  `deploy/base/configmap.yaml`, `CHANGELOG.md`

*Shutdown* is the whole specification for the behavior;
what this task decides is only that a gateway replica is the process it applies to.

**What `serve` loses, and the half of it that must stay.**
`cmd/profgate/serve.go:540`, `reportDraining`, exists to say what a Collection drain is still waiting for.
Nothing waits any more, so it goes.
The `collectionWorker` interface (`serve.go:80`) keeps only what the new drain needs,
and `stubWorker` (`serve_test.go:1364`) follows it.

`drainAll` and `abandonCollections` carry **two** distinctions, not one,
and only the Collection half collapses.
The same mode also decides whether to spend `server.drainDelay` waiting for endpoint removal
(`cmd/profgate/serve.go:365-389`),
and a failed listener deliberately skips that wait (`serve.go:519-526`)
because there is no longer a listener for an endpoint to point at.
`serve_test.go:1686-1708` pins it: a listener failure exits without waiting through a configured delay,
which `internal/config/config.go:45-52` admits up to 60 seconds of.
So the shutdown path keeps an input saying whether the listener has already failed;
what it loses is the choice between waiting for a Collection and abandoning one,
because the new drain is bounded either way.

**What the worker gains.**
On the drain signal it stops every owner loop renewing its lease and returns
once each owner has passed its cutoff or committed,
which is at most `leaseTTL - skewMargin` from the last renewal —
between about 35 and 55 seconds at the shipped 60-second lease.
An owner that finishes inside its cutoff commits normally, because its lease is still valid.
An owner that does not writes nothing and is reclaimed as a new attempt.

**The figure and its seven consumers.**
`grep -rn RequiredPGOGracePeriod --include='*.go' .`:

| Site | Action |
|---|---|
| `internal/config/config.go:564` and its comment from `:543` | deleted |
| `cmd/profgate/main.go:82` | stops printing `required terminationGracePeriodSeconds for pgo` |
| `internal/config/config_test.go:912` | the `122465s` assertion goes |
| `internal/pgo/record_test.go:382` | the deadline arithmetic it covers is unchanged; only the grace-period comparison goes |
| `docs/configuration.md:560-561` | the sample output loses the PGO line |
| `docs/pgo.md:321-325` | the paragraph describing the printed PGO figure |
| `docs/deployment.md:420-424` | "prints two grace-period figures" becomes one |
| `deploy/base/configmap.yaml:63-72` | the comment telling an operator an enabled gateway asks for `122465` |

The last is the one an operator acts on.
It becomes the plain fact: a rollout interrupts a running Collection, which is retried from round 0,
and 125 seconds is the gateway's own drain whether or not PGO is enabled.

- [ ] **Write the drain tests**

`internal/pgo/worker_test.go`, on the clock seam so no test waits on wall-clock time:

| Case | Expect |
|---|---|
| a drain signal with an owner inside its cutoff | it commits normally, and the drain returns after it |
| a drain signal with an owner past its cutoff | it writes nothing, and the record is reclaimed by the next scan as a new attempt with `lease_lost` |
| a drain signal with a work goroutine still inside a merge | the drain returns at the cutoff without waiting for it |
| a `Put` landing after the drain returned | no terminal record names it, and the sweeper removes it as an orphan |
| the drain's total wait | never more than `leaseTTL - skewMargin` from the last renewal, at the shipped lease and at both ends of the 30s–10m range |
| no Collection in flight | the drain returns immediately |

`cmd/profgate/serve_test.go`:

| Case | Expect |
|---|---|
| `SIGTERM` with a Collection in flight | the process exits within the window; the interactive drain is unaffected |
| the listener failing, with a non-zero `server.drainDelay` | it exits without waiting through the endpoint-removal window, and the worker still receives the bounded drain |
| the listener failing | the same bounded worker drain as an ordinary `SIGTERM`, with no Collection distinction |
| `SIGTERM` with `pgo.enabled: false` | unchanged |

`cmd/profgate/main_test.go`: `config validate` prints one grace-period figure, and it is the gateway's.

Run the drain-bound case against the current code first:
it is the one that fails, because today the drain waits for the merge.

- [ ] **Run the tests and watch them fail**

- [ ] **Land the drain, and delete the figure with its seven consumers**

- [ ] **Say plainly what a rollout does**

`CHANGELOG.md`, under `### Changed`:
a gateway replica no longer waits for a running Collection to finish before it exits,
`profgate config validate` no longer prints a PGO grace period,
and a Collection interrupted by a rollout is retried from round 0 by another replica.

- [ ] **Validate and commit**

```bash
semlf check docs/pgo.md docs/deployment.md docs/configuration.md CHANGELOG.md
mise exec -- go test -race ./internal/pgo/ ./internal/config/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/ cmd/profgate/ deploy/base/ docs/ CHANGELOG.md
git commit -m "feat(pgo): drain on the lease, not the deadline"
```

---

## Sampling takes no admission slot

**Files:**
- Modify: `internal/admit/gate.go`, `internal/admit/gate_test.go`,
  `internal/pgo/rounds.go`, `internal/pgo/rounds_test.go`, `internal/pgo/fixtures_test.go`,
  `cmd/profgate/serve.go`,
  `internal/httpapi/server_test.go`,
  `internal/config/config.go`, `internal/config/config_test.go`,
  `docs/api.md`, `docs/pgo.md`, `docs/configuration.md`, `CHANGELOG.md`

*Rounds* states the bound after this change:
`maxParallel × maxActiveCollections` by construction, with no shared gate.
*Configuration* removes the cross-key rule that measured that product against the interactive pool.

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
| a saturated interactive gate for a whole round | every sample still starts and finishes; the gate is never consulted |
| a round with `maxParallel` samples in flight | a new sample starts only as one finishes, with no gate involved |
| `maxActiveCollections` Collections each at `maxParallel` | the process holds exactly `maxParallel × maxActiveCollections` fetches and no more |
| an interactive request against a full gate | still refused, proved through `TryAcquire` |
| `maxParallel × maxActiveCollections` at or above `limits.maxConcurrentProfiles` | accepted by `config.Load`, where it is refused today |
| `grep -rn ReasonSlotTimeout` | no hit outside the changelog |

The saturated-gate case is the one that fails against the current code, so run it first.
`TestRoundsSlotTimeout` (`internal/pgo/rounds_test.go:807`) is deleted:
it proves a behavior the spec removes, and keeping it would pin the gate back in.
The `a store that refuses is artifact_store_failed` subtest of `TestRoundsFinishFailures`
and the late-`Put` fencing cases (`internal/pgo/rounds_test.go:635-668`, `:1209-1292`)
stay green and unedited: they are what proves the rounds loop still ends a Collection correctly.

- [ ] **Run the tests and watch them fail**

- [ ] **Remove the gate, its reason, and the rule that measured it**

`internal/pgo/rounds.go` loses the `Gate` dependency, the `Acquire` at `:452`,
the `ReasonSlotTimeout` write at `:455`, and the constant at `:37`.
Two live sites construct `RoundsDeps` with that field and move with it:
`cmd/profgate/serve.go:689`, inside `startPGO`, and `internal/pgo/fixtures_test.go:1818`.
**`cmd/profgate/serve.go:207` is a different gate and stays** —
it is the one the HTTP server hands interactive requests,
and `internal/admit` exists for it.
`internal/admit/gate.go:37` loses `Acquire`;
the `context` import goes with it if nothing else in the file needs it.
`internal/config/config.go:869-871` goes in the same commit, for the reason
*What This Plan Decides* gives.

- [ ] **Take `slot_timeout` out of the published vocabulary**

| File | Change |
|---|---|
| `docs/api.md` | `slot_timeout` leaves the sample-result values |
| `docs/pgo.md:287` | the sample-reason examples name a reason that still exists |
| `docs/configuration.md` | the removed cross-key rule leaves the `pgo.limits` validation notes |
| `CHANGELOG.md` | under `### Removed`, `slot_timeout` no longer appears in a manifest, because sampling no longer waits for an admission slot; and the rule against `limits.maxConcurrentProfiles` is gone |

- [ ] **Validate and commit**

```bash
semlf check docs/api.md docs/pgo.md docs/configuration.md CHANGELOG.md
mise exec -- go test -race ./internal/admit/ ./internal/pgo/ ./internal/httpapi/ ./internal/config/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/ cmd/profgate/ docs/ CHANGELOG.md
git commit -m "feat(pgo): sample without an admission slot"
```

---

## The container is sized for what it actually holds

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `internal/config/testdata/pgo-full.yaml`,
  `internal/pgo/policy_test.go`, `internal/httpapi/fixtures_test.go`,
  `cmd/profgate/main.go`, `cmd/profgate/main_test.go`,
  `deploy/base/deployment.yaml`, `deploy/base/configmap.yaml`,
  `deploy/deploy_test.go`, `deploy/chart_test.go`,
  `deploy/chart/profgate/templates/_helpers.tpl`,
  `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md`,
  `docs/configuration.md`, `docs/pgo.md`, `CHANGELOG.md`

Two changes, and they have to land together because each is unsafe alone.

**The limit stops pretending the gateway's own footprint is free.**
`deploy/chart/profgate/templates/_helpers.tpl:264-269` chooses:
with `pgo.enabled` the container limit is the PGO working set, and otherwise it is `memoryLimitWithoutPGO`.
`deploy/chart/profgate/values.yaml:141-147` says so in as many words —
"With PGO on this value is ignored" —
and describes what that 512 MiB is for:
the Go runtime, the informer caches, and `limits.maxConcurrentProfiles` transfer buffers.
None of those stop existing when PGO is switched on.
At 4 GiB the omission is invisible, because the working set's own estimate carries enough slack to absorb it.
At the lowered ceilings below it is not, and a gateway serving requests would have no budget of its own.
So the enabled limit becomes the sum, not the choice:
`gatewayBaseMemory + PGOMemoryBytes`, mirroring the shape *Container* already gives the collector.
`GatewayMemoryBytes` is the Go side of it, over a `gatewayBaseMemory` constant of 512 MiB.

**Three of the four inputs to the working set get smaller defaults.**
The other eight ceilings do not enter the formula and are not touched.
The values are the smallest column of *Presets*,
which is what a deployment collecting a handful of Services at hourly intervals needs:

| Ceiling | Today | Becomes | In the formula |
|---|---|---|---|
| `maxParallel` (`config.go:356`) | `4` | `4`, unchanged | per in-flight sample |
| `maxSampleBytes` (`:360`) | `33554432` (32 MiB) | `16777216` (16 MiB) | per in-flight sample |
| `maxMergedBytes` (`:361`) | `67108864` (64 MiB) | `33554432` (32 MiB) | twice, for the running merge and the serialized copy |
| `maxActiveCollections` (`:363`) | `2` | `1` | the outer multiplier |

`1 × (4 × 8 × 16 MiB + 2 × 8 × 32 MiB)` is `1 GiB`, against `4 GiB` today,
and the container limit is `512 MiB + 1 GiB`, which is `1536Mi`.
`maxActiveCollections: 1` means one Collection at a time per replica,
so the two replicas the chart runs still collect two at once.
None of the values leaves its own range, and every cross-field rule still holds:
`maxRounds × maxTargetsPerRound` is untouched at `5 × 32`,
and `maxMergedBytes >= maxSampleBytes` holds at 32 MiB against 16 MiB.

**Every consumer of the old figures, listed rather than assumed.**

| Site | Today | Action |
|---|---|---|
| `deploy/base/deployment.yaml:51-57` | the literal `memory: 4Gi` | `1536Mi`, in this commit or the commit is red |
| `deploy/deploy_test.go:804` | compares that literal against `PGOMemoryBytes` | compares against `GatewayMemoryBytes` |
| `deploy/chart_test.go:417`, `:521`, `:656` | three enabled-chart assertions comparing the rendered limit against `PGOMemoryBytes` | all three compare against `GatewayMemoryBytes`; the chart derives its own number and follows |
| `cmd/profgate/main.go:85` | prints `pgo memory bytes: <PGOMemoryBytes>` | prints both figures, below |
| `cmd/profgate/main_test.go:36` | expects `pgo memory bytes: 4294967296` | expects the two lines below |
| `internal/config/config_test.go:906` | asserts `PGOMemoryBytes()` is `4 GiB` | asserts `1 GiB`, beside a new assertion that `GatewayMemoryBytes()` is `1536Mi` |
| `internal/pgo/policy_test.go:14-29` | a helper calling itself the shipped ceilings, pinning `maxActiveCollections: 2` | moved to `1`, or renamed to say it is deliberately not the shipped set |
| `internal/httpapi/fixtures_test.go:1502-1517` | the same | the same |
| `internal/config/testdata/pgo-full.yaml` | checked for any of the three written out by hand | updated only where it pins a value this task moves |
| `deploy/chart/profgate/values.yaml:141-147` | "With PGO on this value is ignored" | it is added to the derived figure instead |
| `deploy/base/configmap.yaml` | the sizing comment | the new arithmetic |

The two test helpers are the reason this task lists files no arithmetic mentions:
both name themselves after the shipped ceilings and would stay green while exercising values
that stopped being shipped, which is the failure that outlives the commit that caused it.

- [ ] **Write the tests first**

| Case | Expect |
|---|---|
| the shipped defaults | `PGOMemoryBytes()` is `1 GiB` and `GatewayMemoryBytes()` is `1536Mi` |
| `config validate` on the shipped configuration | both lines, with the container figure at `1610612736` |
| the shipped defaults | every field range and every cross-field rule passes with `pgo.enabled: true` |
| `deploy/base/` | the rendered limit equals `GatewayMemoryBytes` over the base's own ConfigMap |
| the chart at its defaults, `pgo.enabled: true` | the rendered limit equals `GatewayMemoryBytes` over the rendered ConfigMap |
| the chart with `pgo.enabled: false` | still `memoryLimitWithoutPGO` alone, unchanged |
| the chart value against the binary | `memoryLimitWithoutPGO` equals `gatewayBaseMemory`, so the two copies of 512 MiB cannot drift |
| each of the three lowered ceilings raised one at a time | both derived limits follow it up, and the base term stays fixed |
| `maxSampleBytes` above `maxMergedBytes` | still refused |

- [ ] **Run the tests and watch them fail**

**What `config validate` reports, decided rather than left implicit.**
One number cannot answer both questions an operator has.
*Presets* publishes the working set and the container limit as separate rows, and gives the reason:
the working set is what the ceilings buy,
and the base is what the process costs before it decodes anything.
`config validate` prints both, each saying which it is:

```text
pgo working set bytes: 1073741824
container memory bytes: 1610612736
```

The second is the number that goes on the Deployment, and it is the one the manifests are compared against.

- [ ] **Add the base term, move the three defaults, and move every consumer above**

- [ ] **Write the sizing table**

`docs/configuration.md` gains a short table under `pgo.limits`:
the four ceilings that enter the formula, what each multiplies, the base term beside them,
the shipped result, and one worked row showing what raising `maxActiveCollections` to `2` costs.
That is the arithmetic an operator does by hand today,
and it replaces the derivation the guide currently leaves to them.
`docs/pgo.md` and the chart README point at it rather than repeating it.

- [ ] **Validate and commit**

```bash
semlf check docs/configuration.md docs/pgo.md deploy/chart/profgate/README.md CHANGELOG.md
mise exec -- go test -race ./internal/config/ ./internal/pgo/ ./internal/httpapi/ ./cmd/profgate/ && mise exec -- go test ./deploy/
mise run lint && mise run test && mise run check
git add internal/ cmd/profgate/ deploy/ docs/ CHANGELOG.md
git commit -m "feat(pgo): size the container for what it holds"
```

---

## The guides, the changelog, and this plan's own status

**Files:**
- Modify: `docs/pgo.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/api.md`,
  `CHANGELOG.md`, `docs/plans/roadmap.md`, this plan

- [ ] **Read the four guides end to end against the shipped behavior**

The earlier tasks each moved the documents their own change touched;
this is the pass that catches what only reads right once all three have landed.
In particular `docs/pgo.md`'s *Multiple gateway replicas* section describes a drain that no longer happens.

- [ ] **Consolidate the changelog**

An operator upgrading reads the behavior changes in one place:
a rollout now interrupts a running Collection instead of waiting for it,
`config validate` prints one grace-period figure,
`slot_timeout` is gone from the sample results,
and the memory limit falls from 4 GiB to 1536Mi,
which is the first time it has counted the gateway's own footprint at all.
The last is the one that changes a rendered manifest,
so it says plainly that an operator who raised the four ceilings keeps their own values.

- [ ] **Flip the roadmap and this plan**

`docs/plans/roadmap.md` item 11's three unticked bullets are ticked,
and its `Shipped:` line names what carried them while keeping the deferral it records.
This plan's line 3 becomes `**Status:** Done` and line 4 an `Outcome:` naming the same thing.
The plan is deleted in the next commit that touches it, per
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md);
one commit cannot do both.

- [ ] **Validate and commit**

```bash
semlf check docs/ CHANGELOG.md
mise run lint && mise run test && mise run check
git add docs/ CHANGELOG.md
git commit -m "docs: describe collection that yields to a rollout"
```

---

## Risks and What This Plan Does Not Cover

**A rollout now costs a Collection its progress, where it used to cost 34 hours of grace period nobody set.**
This is the trade, and it is not new — `deploy/base/configmap.yaml:63-72` documents it today
as what a 125-second grace period buys.
What changes is that it becomes the design rather than a deviation from one.
*Recovery* bounds how often it can happen by `pgo.maxAttempts`,
and an operator who rolls more often than a Collection takes will exhaust attempts.

**A merge still shares a heap with the request path.**
Lowering the working set to 1 GiB shrinks the blast radius; it does not remove it.
This is the cost the collector Deployment exists to remove, and it is deliberately still here —
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md)
names the trigger that would change the answer.

**`decodeFactor` is an estimate, not a measurement.**
*Container* says so.
Lowering the ceilings lowers the figure the estimate produces without making the estimate better.

**Not covered:** any change to the interactive request path beyond removing a queue nothing should have joined,
any change to the realm model or authentication,
and any widening of the Kubernetes or NATS permission the gateway holds today.

---

## Self-Review

- Every claim about the tree was run, not remembered:
  `internal/pgo/rounds.go:452` is the only production `Acquire`, and `:37` and `:455` are the reason it writes;
  `internal/httpapi/server_test.go:879` is the only other non-gate caller;
  `internal/config/config.go:536` is `PGOMemoryBytes`, `:564` is `RequiredPGOGracePeriod`,
  and `:869-871` is the rule against `limits.maxConcurrentProfiles` while `:874` is the one that stays;
  the grace-period figure has three callers —
  `cmd/profgate/main.go:82`, `internal/config/config_test.go:912`, `internal/pgo/record_test.go:382` —
  and four documents describe it:
  `docs/configuration.md:560-561`, `docs/pgo.md:321-325`, `docs/deployment.md:420-424`,
  and `deploy/base/configmap.yaml:63-72`;
  `internal/config/config_test.go:906` asserts `PGOMemoryBytes()` is `4 GiB`;
  `deploy/base/deployment.yaml:51-57` carries `4Gi` as a literal and `deploy/deploy_test.go:804` compares it;
  `deploy/chart_test.go:388-421` compares the chart's own arithmetic against the same function;
  `cmd/profgate/serve.go:540` is `reportDraining` and `:80` and `:97` are the worker seam it reports through;
  `internal/pgo/clock.go` already gives `Now` and `NewTimer`, so no seam is added.
- Decided here because the spec leaves it to the implementer:
  the three sizing defaults that move take the values of the smallest column of *Presets*, without the `preset` key,
  and they are this plan's choice rather than the spec's, which keeps the current ceilings absent a preset;
  `config validate` prints the working set and the container figure as two lines rather than one;
  the enabled container limit becomes the gateway's base plus the working set rather than a choice between them,
  and `gatewayBaseMemory` keeps the 512 MiB the chart already budgets, pinned equal by a test;
  the cross-key rule leaves in the same commit as the gate rather than in its own;
  `record_test.go` keeps its deadline arithmetic and loses only the grace-period comparison;
  `docs/configuration.md` carries the sizing table, and the other guides point at it.
- Left to the implementer by design: helper and fixture names,
  the exact wording of every message beyond the values it must name,
  and the layout of the sizing table beyond the four rows it must carry.
- The order is a dependency order:
  the drain lands first, so the grace-period figure is deleted before the sizing task would restate it;
  the gate leaves before nothing, because the rule that bounded it leaves in the same commit;
  the sizing change lands last because it is the only task that edits a rendered manifest.
