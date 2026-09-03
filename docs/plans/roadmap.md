# Roadmap

**Status:** Done
**Outcome:** `v0.5.0`; every item is shipped in it or withdrawn.

> **How to read this document:** it orders the work that follows the console,
> so that each item is taken up in turn and nothing is started out of order.
> It is not an implementation plan:
> an item that changes behavior first revises the spec it names,
> and only then gets a plan of its own under `docs/plans/`.
> Items that change no behavior — a release, a chart template, a docs repair — need no spec and are executed directly.
> A ticked bullet records that the design decision it carries is settled in the spec that item names,
> whatever the bullet's wording:
> a bullet phrased as behavior is ticked once the revision that settles that behavior is in the spec.
> A bullet the item's named spec does not cover is ticked when the work itself is done,
> because there is no revision a tick could record for it instead.
> Whether a settled decision has also shipped is the item's own `Shipped:` line,
> which names the version that carries it, the pull request that landed it, or says it is not built yet.
> A ticked item that has not shipped is an ordinary state rather than a contradiction:
> the design is settled and the code is still to write.
> That line is where the answer lives because a plan under `docs/plans/` is deleted once its work lands,
> so a finished item leaves nothing there to read.

**Goal:** turn what the gateway already does into something a person can install from a release,
reach from a terminal under `oidc`, diagnose when a Service yields no target,
and drive from automation without scraping human-readable text —
and, once that holds, remove the mechanisms whose cost exceeds what they protect.

**Sources:** the investigation that produced this ordering is
[`2026-08-27-service-gaps-and-over-engineering.md`](../investigations/2026-08-27-service-gaps-and-over-engineering.md);
every claim below is restated against the committed file it cites.

## Ordering Principle

Earlier items are cheaper, block more users, or unblock later items.
Within the list: ship what exists, then make the shipped thing reachable, then diagnosable,
then automatable, then lighter.
An item may be reordered only by editing this list, never by starting later work first.

## Items

### 1. Repair the route count and cut a release

- [x] `docs/api.md` states in three places that `/v1` has seven routes;
  eleven exist (`internal/httpapi/server.go`), and the `route_unknown` row of the error table repeats the wrong count.
- [x] Release the `Unreleased` section of `CHANGELOG.md` — authentication, the console, and client-selected ports —
  as the next minor version after `v0.3.0`.

Spec: none.
Shipped: `v0.4.0`.
Why first: nothing under `Unreleased` reaches an operator who installs from the chart registry.

### 2. Chart templates every install needs

- [x] An `Ingress` template, off by default,
  routing `/`, `/ui/`, `/auth/`, and `/v1/` to the API port;
  `values.yaml`, the chart README, and `NOTES.txt` today instruct the operator to write one by hand.
- [x] A `PodMonitor` template, off by default, for the ops port,
  which is deliberately absent from the Service.
- [x] A `PrometheusRule` template, off by default, with the alerts the deployment guide already names as alertable
  (JWKS age, readiness, admission saturation).
- [x] A `resources.requests` default alongside the derived memory limit,
  so a namespace with a `LimitRange` or quota installs without the raw `resources:` escape hatch.

Spec: the deployment section of `docs/specs/gateway.md` names the chart's shape;
confirm it permits these templates before writing them, and revise it if it does not.
Shipped: on `main` after `v0.4.0`, in `a6bbd27`;
the templates are under `deploy/chart/profgate/templates/`.
Tests: extend `deploy/chart_test.go` for each template's on and off rendering.

### 3. Client-selected port becomes default-deny

Today `discovery.pprof.allowedPorts` and `allowedPortNames` are independent lists
where an empty list permits any value,
and no setting forbids `portName` outright (`docs/configuration.md`, section `discovery`).
The console lets a browser type any port under that default.

- [x] Revise `docs/specs/gateway.md`:
  one `discovery.pprof.allowedSelections` list whose entries are `{port: N}` or `{portName: name}`;
  an empty list accepts only the configured default;
  `{port: "*"}` admits any number and `{portName: "*"}` admits any name, each on its own;
  `allowedPorts` and `allowedPortNames` are removed, which is a breaking change.
  `/v1/limits` returns the list so the console can offer a menu or a free field.
- [x] Revise the permission invariant text in `AGENTS.md`, `README.md`, and `.agents/rules/800-security-invariant.md`,
  which today states that an empty allowlist admits any port a client names,
  to say the gateway connects to the configured pprof port and to any other the operator lists.
- [x] Write the implementation plan; migrate `docs/configuration.md`, the chart values, and the console's port control.

Spec: `docs/specs/gateway.md` (revision required).
Shipped: pull request #2, in `v0.5.0`.
Why here: it narrows what a compromised client can probe and shrinks the configuration surface,
and the CLI in item 4 should be written against the final model.

### 4. A first-party command line

`profgate` today has `version`, `config validate`, `auth hash`, and `serve` (`cmd/profgate/main.go`).
Under `oidc`, `go tool pprof` cannot attach a bearer token,
and `docs/authentication.md` sends the user to another tool for one.

- [x] Write `docs/specs/cli.md`:
  `login` by OIDC device code with a local token cache,
  `namespaces`, `services`, `targets`, `profile` (with `--open` running `go tool pprof -http`),
  `collect --wait`, `collections`, and `download`;
  the same binary or a second `cmd/`.
- [x] Write the implementation plan once the spec is `Accepted`.

Spec: new (`docs/specs/cli.md`), layered on `docs/specs/auth.md`,
which already defers token acquisition to this document.
Shipped: pull request #3, in `v0.5.0`.

### 5. Target exclusion diagnostics

`/targets` returns only eligible Pods;
an empty answer cannot distinguish no Ready Pod, a selector that matches nothing,
a named port no Pod declares, a terminating Pod, or a cache not yet synced.

- [x] Revise `docs/specs/gateway.md`:
  `GET .../targets?explain=true` adds aggregate exclusion reasons with counts,
  never Pod identity beyond what the plain listing shows,
  guarded by the same realm check.
- [x] Revise `docs/specs/ui.md` so the console's empty state shows those reasons.
- [x] The CLI's `targets` prints them.

Spec: `docs/specs/gateway.md` and `docs/specs/ui.md` (revisions required).
Shipped: pull request #4, in `v0.5.0`.

### 6. A machine contract automation can build on

- [x] `X-Request-Id`: accepted from the client or generated, echoed on every response,
  and carried in the audit record (`internal/httpapi/audit.go`).
- [x] Structured error details:
  `limit_exceeded` today names the violating fields only in its message (`internal/httpapi/pgo_policy.go`);
  add a `details` array to the error envelope (`internal/httpapi/errors.go`), leaving `message` free to change.
- [x] `Idempotency-Key` on `POST .../collections`.
- [x] `GET /v1/collections/{id}?wait=<duration>` long-poll that returns at completion or at the deadline.
- [x] `GET .../services/{svc}/collections/latest` — the newest completed record and its artifact —
  and `state`, `since`, and `origin` filters plus a cursor on the listing.
- [x] Revisit the `pgo.artifact.retention` default (2h) against `jobRetention` (168h),
  which leaves records that outlive their artifact by a wide margin.
- [x] An OpenAPI document generated from the routes, served at a fixed path, and checked in CI against the router.

Spec: `docs/specs/gateway.md` and `docs/specs/pgo.md` (revisions required).
Shipped: pull request #5, in `v0.5.0`.

### 7. Console: write paths, browser tests, stable asset paths

- [x] Start and cancel a Collection from the console;
  the policy editor stays out until conflict handling is designed.
- [x] A small browser-driven test layer that executes `app.js`:
  the login-to-profile happy path, the `oidc` 401 redirect and return,
  Collection list and download, and hostile strings rendered as text only.
- [x] Replace the content-hashed asset tree (`internal/ui/ui.go`, `treeHash`) with stable paths,
  an `ETag`, and `Cache-Control: no-cache`;
  this removes the rolling-update failure `docs/console.md` documents.

Spec: `docs/specs/ui.md` (revision required).
Shipped: pull request #10, in `v0.5.0`.

### 8. Small removals

Each is one commit.
Most change nothing a client sees;
removing `pgo.versionPolicy` changed what `GET /pgo` publishes and what the two write routes accept,
and revised [`pgo.md`](../specs/pgo.md) to say so.

- [x] `pgo.versionPolicy` is a one-valued enum (`internal/config/config.go`, `oneof=strict`); remove the key.
- [x] `internal/pgo/id.go` hand-packs Crockford base32 for an identifier nobody transcribes;
  keep the identifier grammar the API documents, or revise the spec to plain hex, and delete the packer.
- [x] `internal/auth/cookie.go` frames session and transaction fields with hand-written length prefixes;
  serialize with `encoding/json` under the same seal.
- The startup probes stay — withdrawn.
  `internal/natskv/preflight.go` writes, watches, and deletes a probe key in each KV bucket
  and a probe object in the artifact store,
  and `internal/pgo/sweeper.go` removes what a preflight that died between a probe's create and its delete left behind.
  Dropping them and letting the first real operation fail loudly was the idea.
  The probes are the only thing that verifies NATS *write* permission, per bucket and per verb, and watch delivery.
  The bucket-contract check reads stream information and writes nothing;
  the watches the PGO runtime opens afterwards prove subscribe and never publish;
  the sweeper's artifact listing warns and carries on when it fails.
  "The first real operation" has no bound either:
  `DefaultPolicy` sets `enabled` false for every Service,
  and the scheduler passes only over Services that carry a stored override,
  so a gateway with `pgo.enabled: true` and no override anywhere never writes to NATS at all.
  Today a denied probe is a non-zero exit naming the bucket and the operation,
  so the new Pod crash-loops and the rollout stalls with the old Pod still serving.
  Without the probes the process would connect, pass the contract check, answer `/readyz` 200,
  join the Service, and fail later as a 5xx or a Collection that dies.
  [`pgo.md`](../specs/pgo.md) promises that exit —
  a NATS user lacking a permission makes the process exit naming the bucket and the operation —
  and nothing else in the process would keep it.
  The trade is about 230 lines of production code against about 340 lines of tests and 30 of spec,
  and the sweeper would stop reading `PROFGATE_CONFIG` at all, `sweepProbes` being its only reader of that bucket.
- `deploy/chart_test.go` keeps its per-field assertions — withdrawn.
  `helm template` golden files cannot carry most of what the file asserts.
  It calls `renderFailure` 110 times,
  and that helper asserts `helm template` *fails* and reads the reason out of stderr;
  a refused render writes no stdout, so a golden file has nothing to record.
  Those cases are about 720 lines of table rows and loop bodies,
  in eleven functions running about 1,500 lines together,
  and they exercise the 42 `fail` sites in `deploy/chart/profgate/templates/_helpers.tpl`,
  one site refusing many bad values.
  Another 178 lines assert agreement between the render and something else in the repository —
  `TestChartClusterRoleMatchesBase` against `deploy/base/clusterrole.yaml`,
  `TestChartPrometheusRule` against the series `internal/metrics/prometheus.go` exports
  and the code `internal/httpapi/codes.go` registers,
  `TestChartGuardedEnvNamesMatchTheBinary` against the `env` struct tags `config.Load` reads —
  or about 240 counting `TestChartSecurityContexts` against `deploy/base/deployment.yaml`
  and `TestChartReadmeValues` against the chart README.
  A golden file records one side of such a pair;
  the agreement is the assertion, and both sides moving together would leave the golden green.
  About 110 more lines assert a relationship between two renders that no single recording holds:
  `TestChartConfigChecksum` requires the checksum to move on a configuration change
  and to stay put on a `replicaCount` change,
  and `TestChartClusterResourcesAreReleaseScoped` requires two releases to render different cluster-scoped names.
  What is left is a thin band of static shape assertions,
  and the file renders the chart at 72 points, each with its own values,
  so recording even part of it would cost dozens of golden files, and this repository has none.
  Its three `testdata/` directories hold input fixtures fed to `config.Load`, not recorded output.
  `deploy/chart/profgate/templates/deployment.yaml:34` puts `checksum/config`,
  a sha256 over the whole rendered ConfigMap, into the pod template,
  so a golden file holding the pod template reddens on every `values.yaml` default change,
  with a diff that reads as one hex string becoming another.
  The premise was wrong as well:
  3,177 lines of test against a 1,887-line chart —
  1,327 of templates, 541 of `values.yaml`, 19 of `Chart.yaml` —
  is 1.68 times, not the multiple "larger than the chart it tests" suggests.
  741 of those template lines are `_helpers.tpl`,
  whose 568 lines from `profgate.pgoCeiling` on are derivation and validation.
  A validation program's suite is one case per bad value it must refuse.
- [x] Collapse the repeated type checks in `TestChartMountPartsAreValidated` (`deploy/chart_test.go`):
  twelve rows prove `profgate.mountPartString` refuses a non-string,
  and the ones that differ only in which values key the message names become one table.
- [x] `docs/decisions/e2e-without-framework.md` set a revisit trigger on harness size;
  `test/e2e/harness_test.go` has passed it, and no revisit is recorded.

Spec: [`docs/specs/pgo.md`](../specs/pgo.md), amended for the removed `versionPolicy` key,
and again for the identifier grammar if that bullet is taken up.
Shipped: pull request #11 landed the identifier, the cookie, and the harness revisit;
`pgo.versionPolicy` is removed, in `v0.5.0`.

### 9. Superseded and finished documents leave the tree

`docs/specs/profgate-design.md` was `Superseded` and five plans were `Done`;
together they were most of the Markdown in the repository and competed with the living specs in every search.
They are deleted; git history is their record.

- [x] Write a decision record:
  a finished plan and a superseded spec are deleted in the commit after the one that records their status,
  and git reads a deleted one back —
  `git log --all --diff-filter=D --format=%H -- <path>`,
  then `git show <that-commit>^:<path>`.
- [x] Apply it.

Spec: none;
the decision record changes `docs/README.md` and `.agents/rules/900-design-and-review-loops.md`.
Shipped: on `main` after `v0.4.0`, in `66d713a`;
the record is `docs/decisions/finished-documents-leave-the-tree.md`.

### 10. OIDC transport from a library — withdrawn

`internal/auth/{discovery,jwks,issuer,verify}.go` were compared against `github.com/coreos/go-oidc/v3` v3.20.0.
The library provides discovery and audience membership,
refetches keys on a miss with no periodic refresh and no cooldown across sequential requests,
tries every key when a token carries no `kid`,
applies no clock skew to `exp` and a fixed five-minute leeway to `nbf`,
and reads response bodies without a bound;
four normative cases in the authentication spec's testing section fail against it.
Replacing the transport would delete nothing, keep the hardening, and add glue and one module.
The dependency argument in `docs/specs/auth.md` stands; nothing changes.
The comparison, function by function, is
[`2026-08-28-oidc-library.md`](../investigations/2026-08-28-oidc-library.md).

Shipped: nothing; the item is withdrawn.

### 11. PGO stops costing what it is not worth

The scheduler, worker, sweeper, and their stores run in every gateway replica whenever `pgo.enabled` is set,
and `profgate config validate` reports a termination grace period and memory budget
that an operator must carry into the Deployment by hand.

- [x] Revise `docs/specs/pgo.md`:
  the collector runs as a separate Deployment or subcommand,
  the twelve `pgo.limits.*` keys collapse into named presets with an escape hatch,
  and the grace-period and memory arithmetic moves inside the chart.
- [x] Decide, in the same revision, whether one collector replica suffices;
  if it does, the lease, claim, and orphan-sweep machinery can be removed with the multi-replica guarantee.
  It does not:
  a `RollingUpdate` of a one-replica Deployment surges to two, so every mechanism stays.
- [x] A gateway replica drains on the lease rather than on a Collection's worst-case deadline.
  That is what makes the spec's own promise true —
  `pgo.enabled` does not lengthen a gateway replica's grace period —
  and `config.RequiredPGOGracePeriod` goes with the 34-hour figure it prints.
- [x] Sampling stops taking a slot from the gate interactive requests pass through,
  and `slot_timeout` leaves the published sample results.
- [x] Three of the four ceilings that size the working set get lower defaults,
  the container limit counts the gateway's own footprint beside that working set,
  and `docs/configuration.md` gains a sizing table.

Spec: `docs/specs/pgo.md`, already revised.
Shipped: pull request #17, in `v0.5.0`.
Separating the collector into its own Deployment is **deferred, not dropped**.
The measurements behind that, and the triggers that would revive it, are in
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md);
`docs/specs/pgo.md` keeps every section that designs the separation,
and `pgo.preset` is not built for the reason that record gives.
Why last: it is the largest change, it is off by default, and every earlier item is useful without it.

### 12. Two defects in the watched caches, one of them silent

`Caches.Run` (`internal/pgo/caches.go:282-315`) opens four watches in a loop
and waits on one `sync.WaitGroup` for all four consumers.
Below it, `runWatch` (`internal/natskv/client.go:578-613`) owns the channel `Caches.consume` reads,
and it opens as many underlying watchers as it takes to keep that channel fed.
Each layer has a defect, and they are not the same defect.

- [x] **A failed open cleans up after itself.**
  `Caches.Run` opens every watch under a context of its own,
  and a failed open cancels that context and waits for the consumers it started before returning the error.
  `runCaches` (`cmd/profgate/serve.go`) still answers a failure by calling `Run` again,
  and now reopens from one consumer per prefix rather than from a growing number.
- [x] **A watcher that reopens under an unchanged generation replays into a cache nothing clears.**
  `consumeWatcher` reports a closed underlying watcher as a reconnect rather than as completion
  (`internal/natskv/client.go:636-638`),
  and `runWatch` opens another without returning (`:596-611`),
  so the channel `Caches.consume` reads stays open and `Caches.Run` never learns of the cut.
  When the connection did not drop, the replay carries the generation the watch already held,
  and `Caches.apply` (`internal/pgo/caches.go:333-337`) rebuilds only when a generation differs,
  so nothing resets the cache and every change made in the gap is missing from it.
  `watchState` never clears its marker (`internal/natskv/client.go:309-326`),
  so `Client.Synced` reports the barrier open throughout,
  and the gap is unbounded: the re-open retries forever, and against a deleted stream it fails forever.
  A key deleted during the gap is usually repaired once the replay completes,
  because the replay delivers the newest message on every subject and a delete marker is one.
  It survives only when the stream was recreated or its delete markers were purged,
  which is also the likeliest reason the watcher was cut while the connection stayed up.

The second is settled in the spec.
Clearing the cache alone is not the repair:
`Session` (`internal/pgo/runtime.go:112-137`) checks the barrier once and hands back a bound view,
and a route reads the cache afterwards without rechecking
(`internal/httpapi/pgo_collections.go:413`),
so emptying the maps under a live generation turns an admitted request into a false empty answer.
The invalidation has to reach a session already inside the barrier,
which is what moving the generation already does for a dropped connection.
So a watch whose subscription closes under a live connection moves the store generation too,
every watch re-opens under the new one and every watched cache is reset,
a session's cache reads take the generation it bound and answer `503 pgo_unavailable` on a mismatch,
and `Caches.Run` clears its synced flags at the start of every attempt.
A bucket deleted or recreated under a running process then keeps that process up,
with `/readyz` green and every PGO route refusing until the bucket exists again,
which the gauge `profgate_pgo_synced` and its alert make visible.

Spec: none for the first bullet;
the second is settled in [`pgo.md`](../specs/pgo.md) *The seam*.
*Logging*, *Health*, *Metrics*, and *Failure Scenarios* carry what follows from it.
Shipped: the first bullet in pull request #17 and the second in pull request #18, both in `v0.5.0`.
Why here: neither bullet blocks item 11 nor is blocked by it.
The first is small and self-contained.
The second is the one an operator would never notice,
because everything keeps answering and only the answers are wrong.

## Not on This List

- Rendering profiles — flamegraphs, diffs, top tables — stays with `go tool pprof`.
- Non-Go pprof producers, multi-cluster, continuous profiling, long-term profile storage.
- Cookie key rotation, the file poller, and the fingerprint gauge stay as they are;
  they cost little and removing them buys little.

## Validation

Every item that lands code ends with:

```bash
mise run lint && mise run test && mise run check
```

Prose uses semantic line breaks; run `semlf check` on what you wrote.
