# PGO Implementation Plan

**Status:** Approved

> **For agentic workers:** implement this plan one task at a time, in order;
> each task is written test-first and ends with its own validation block and commit.
> Run a task inline or hand it to a subagent, whichever fits its size.
> Checkboxes (`- [ ]`) track progress.

**Goal:** Build PGO collection as defined in [`docs/specs/pgo.md`](../specs/pgo.md):
the NATS seam with generation-bound views and preflight probes,
the shared admission gate,
policy layering under static ceilings,
the leaderless scheduler and publisher,
the worker with its scan, owner loop, and in-memory merge,
the sweeper,
the seven PGO routes,
and the unit and end-to-end layers that prove it.

**Architecture:** The gateway binary gains three long-lived loops per replica — scheduler, worker, sweeper — none elected.
`internal/natskv` is the only non-test importer of nats.go
and exposes `Client`, generation-bound `Stores` views, and `Preflight`;
`internal/admit` holds the one admission gate shared by interactive requests and Collection samples;
`internal/pgo` layers policy, publishes and claims Collections through KV primitives, merges samples in memory,
and stores artifacts in the Object Store;
`internal/httpapi` gains the PGO routes;
`cmd/profgate` wires it all only when `pgo.enabled`.

**Tech Stack:** everything already pinned, plus
`github.com/nats-io/nats.go v1.53.1` (runtime, only in `internal/natskv` and the e2e harness)
and `github.com/nats-io/nats-server/v2 v2.14.5` (tests only: in-process JetStream).
`github.com/google/pprof` moves from tests-only to runtime (`ParseData`, `Merge`, `Compact`, `Write`).

**Spec:** [`docs/specs/pgo.md`](../specs/pgo.md), layered on [`docs/specs/gateway.md`](../specs/gateway.md).
Every behavior table below restates the spec for the task at hand;
where they differ the spec wins, and the plan is the bug.
Spec sections are cited by heading name; unqualified sections are the PGO spec's.
The unit-test cases in the spec's *Testing* section are normative:
each task below names its slice of them, and the task is done only when every bullet for its package passes.
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- Everything in the gateway plan's constraints still holds, except the retired
  "`go.mod` never contains `github.com/nats-io/nats.go`" line.
- Only `internal/natskv` imports `github.com/nats-io/nats.go` outside `_test.go` files and the `test/` tree;
  `mise run check` already enforces it (`check_nats_importers` in `scripts/check-repo.py`).
- Only `internal/k8s` imports `k8s.io/client-go`, unchanged; no new RBAC tuple, no new `internal/k8s` method.
- One admission gate: `cmd/profgate` constructs exactly one `admit.Gate` from `limits.maxConcurrentProfiles`
  and injects it into both `httpapi` and `pgo`; nothing else creates one.
- Validation enforces `pgo.limits.maxParallel × pgo.limits.maxActiveCollections < limits.maxConcurrentProfiles`
  (the ceilings, not the policy defaults), `maxRounds × maxTargetsPerRound ≤ 256`,
  and `pgo.jobRetention ≥ pgo.limits.maxRetention + 1h`.
- The bucket configuration contract (*Permission Boundary*, "Bucket configuration contract"):
  preflight reads each store's `Status` and exits non-zero on a TTL, memory storage, `Discard: old`,
  or a size limit below the contract.
- Constants, not configuration: `skewMargin` 5s, `maxRecordBytes` 512 KiB, `decodeFactor` 8, orphan age 10m.
- Every store operation goes through a generation-bound `Stores` view; no unbound bucket accessor exists.
- No response, record, manifest, or log line contains a Pod IP or pprof port.
- The container gains no writable volume; samples and merges live in memory.
- Global state stays as the gateway plan lists it; every loop is constructed and injected.
- Module files: a task that imports a module for the first time repeats that module's exact `go get` line
  (idempotent), then runs `go mod tidy` and stages `go.mod` and `go.sum` with its package.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on what you wrote.

---

## File Structure

```text
go.mod, go.sum                          # + nats.go, nats-server (test)
internal/admit/gate.go                  # Gate: TryAcquire, Acquire
internal/admit/gate_test.go
internal/httpapi/server.go              # Deps gains *admit.Gate; the private slots channel goes away
internal/httpapi/pgo.go                 # route table, realm flags, 501/503 steps
internal/httpapi/pgo_policy.go          # GET/PUT/DELETE /pgo, ETag matrix
internal/httpapi/pgo_collections.go     # POST/GET collections, get, download, cancel
internal/httpapi/pgo_test.go, pgo_policy_test.go, pgo_collections_test.go
internal/config/config.go               # + NATSConfig, PGOConfig, PGOLimits, PGODefaults, RealmPGO
internal/config/config_test.go, testdata/pgo-*.yaml
internal/natskv/natskv.go               # Entry, KV, Objects, ObjectInfo, Status, Statused, Stores, Client, errors
internal/natskv/client.go               # connection, generation counter, views, watches with markers
internal/natskv/preflight.go            # bucket contract checks and probes
internal/natskv/*_test.go               # in-process nats-server fixtures
internal/pgo/policy.go                  # Policy types, layering, ceilings, violations
internal/pgo/id.go                      # newID: 20 chars Crockford base32
internal/pgo/record.go                  # Record, Manifest, states, reasons, serialization guard
internal/pgo/publisher.go               # Reserve/Track/Release, release rule, the three publication writes
internal/pgo/scheduler.go               # slots, jitter, tick
internal/pgo/worker.go                  # watch, scan, claim, owner loop
internal/pgo/rounds.go                  # target resolution, sampling, decode, merge, finish
internal/pgo/sweeper.go
internal/pgo/runtime.go                 # Runtime: late-bound handler view of client, publisher, caches
internal/pgo/clock.go                   # clock seam shared by scheduler, worker, sweeper
internal/pgo/*_test.go
internal/metrics/recorder.go            # + PGO methods on Recorder and Noop
internal/metrics/prometheus.go          # + the seven PGO metrics
cmd/profgate/serve.go                   # NATS preflight, loops, gate construction
deploy/base/deployment.yaml             # fsGroup, optional creds volume, memory limit
deploy/secret-nats-example.yaml         # commented example Secret, outside the base
deploy/nats/README.md                   # provisioning commands from the spec
deploy/nats/account.conf                # the NATS account fragment, pinned by a test
deploy/deploy_test.go                   # + fragment and Secret-mount tests
test/e2e/nats.yaml                      # NATS Deployment + Service for kind
test/e2e/harness_test.go                # + bucket provisioning, purge between scenarios
test/e2e/registry.go                    # + PGO scenarios
test/e2e/scenarios_pgo_test.go          # (e2e tag)
```

---

## Admission gate and module pins

**Files:**
- Create: `internal/admit/gate.go`, `internal/admit/gate_test.go`
- Modify: `internal/httpapi/server.go`, `internal/httpapi/server_test.go` (and any test that builds `Deps`),
  `cmd/profgate/serve.go`

**Produces** (signatures per the spec's *Collections*, "Admission is one gate"):

```go
package admit

type Gate struct{ /* unexported */ }

func New(capacity int) *Gate
// TryAcquire takes a slot without waiting; interactive requests use it (429 when ok is false).
func (g *Gate) TryAcquire() (release func(), ok bool)
// Acquire waits for a slot until ctx ends; Collection samples use it.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error)
```

`httpapi.Deps` gains `Gate *admit.Gate` and `New` stops creating its `slots` channel;
the profile handler's admission step becomes `release, ok := d.Gate.TryAcquire()` with `defer release()`.
`cmd/profgate` constructs the one gate from the loaded `limits.maxConcurrentProfiles` and passes it in.
This realizes two amendment rows in the spec's *Changes to the accepted gateway design*:
*Request algorithm* step 9, and `internal/httpapi/server.go`.

- [ ] **Add the modules** (pins only; nothing imports them until later tasks):

```bash
mise exec -- go get github.com/nats-io/nats.go@v1.53.1
```

Do not run `go mod tidy` here; the natskv task repeats the line and tidies.

- [ ] **Write the failing tests** (`internal/admit`, per the spec's *Testing* bullet for the package):

| Subtest | Expect |
|---|---|
| try at capacity | `New(1)`; first `TryAcquire` ok; second `ok == false` without blocking (assert < 100ms) |
| release frees | release the first; `TryAcquire` ok again |
| acquire waits | `New(1)`, slot held; `Acquire(ctx)` blocks, returns after release |
| acquire context end | slot held; `Acquire` with a 50ms-deadline ctx returns its error |
| release idempotent-ish | calling the same `release` twice does not free two slots (a second `TryAcquire` still fails); guard with `sync.Once` |
| race | 100 goroutines mixing both paths under `-race`; net slots return to capacity |

- [ ] **Implement** `Gate` over a buffered channel; `release` wraps the receive in `sync.Once`.

- [ ] **Rewire `httpapi`**: `Deps` gains `Gate *admit.Gate` and every constructor caller passes one;
  no nil-check path is added.
  Update the `server_test.go` fixtures to build `admit.New(cfg.Limits.MaxConcurrentProfiles)`;
  the admission subtests (429, in-flight gauge) must still pass unchanged.

- [ ] **Rewire `cmd/profgate/serve.go`**: construct the gate next to the recorder and pass it into `httpapi.Deps`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/admit/ ./internal/httpapi/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/admit/ internal/httpapi/ cmd/profgate/ go.mod go.sum
git commit -m "feat(admit): share one admission gate"
```

---

## Configuration

**Files:**
- Create: `internal/config/testdata/pgo-full.yaml`
  (the complete example from the spec's *Configuration* section, verbatim),
  plus one bad fixture per rejected row below
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  and `cmd/profgate/` (the `config validate` output gains the PGO memory figure and grace period)

**Produces:** new blocks on `Config`, mirroring the spec's configuration table exactly
(keys, env names with prefix `PROFGATE_`, defaults, validation ranges):

```go
type Config struct {
    // ... existing fields ...
    NATS NATSConfig `yaml:"nats"`
    PGO  PGOConfig  `yaml:"pgo"`
}
type NATSConfig struct {
    URL            string        `yaml:"url"            env:"NATS_URL"`
    CredsFile      string        `yaml:"credsFile"      env:"NATS_CREDS_FILE"`
    ConnectTimeout time.Duration `yaml:"connectTimeout" env:"NATS_CONNECT_TIMEOUT" default:"5s"`
}
type PGOConfig struct {
    Enabled      bool          `yaml:"enabled"      env:"PGO_ENABLED"      default:"false"`
    ConfigAPI    string        `yaml:"configAPI"    env:"PGO_CONFIG_API"   default:"enabled"`
    LeaseTTL     time.Duration `yaml:"leaseTTL"     env:"PGO_LEASE_TTL"    default:"60s"`
    MaxAttempts  int           `yaml:"maxAttempts"  env:"PGO_MAX_ATTEMPTS" default:"3"`
    JobRetention time.Duration `yaml:"jobRetention" env:"PGO_JOB_RETENTION" default:"168h"`
    Limits       PGOLimits     `yaml:"limits"`
    Defaults     PGODefaults   `yaml:"defaults"`
}
type Realm struct {
    // ... existing fields ...
    PGO RealmPGO `yaml:"pgo"`
}
type RealmPGO struct {
    Read      bool `yaml:"read"`
    Collect   bool `yaml:"collect"`
    Configure bool `yaml:"configure"`
}
```

`PGOLimits` carries every `pgo.limits.*` key and `PGODefaults` every `pgo.defaults.*` key from the spec's table,
with the spec's env names, defaults, and ranges;
`pgo.defaults.sampling.replicas` is a string field validated as `all` or an integer in range
(the typed `Replicas` union lives in `internal/pgo`, not here — config stores what the operator wrote).
Cross-field validation joins the existing hand checks:
`nats.url` required when `pgo.enabled`;
`nats.credsFile`, when set, names a readable file;
`maxParallel × maxActiveCollections < limits.maxConcurrentProfiles`;
`maxRounds × maxTargetsPerRound ≤ 256`;
`jobRetention ≥ maxRetention + 1h`;
`minEvery ≤ maxEvery ≤ 24h`;
`maxDuration ≤ limits.cpuSeconds` (seconds);
`maxSampleBytes ≤ maxMergedBytes`;
every `pgo.defaults` value within its ceiling;
`jitter ≤ every/2`.
`config validate` additionally prints the memory figure from the spec's *Container* formula
(`decodeFactor` 8) and the grace period covering the deadline formula at the ceilings
(*Ceilings* and *Shutdown*).

- [ ] **Write the failing tests**, one subtest per row of the spec's `internal/config` testing bullet:
  the full example loads and validates as written (fixture `pgo-full.yaml`);
  every new env var lands on its field (extend the existing env-override table);
  `nats.url` missing with `pgo.enabled: true` rejected, accepted when disabled;
  `limits.maxParallel: 8` with `maxActiveCollections: 2` and `maxConcurrentProfiles: 16` rejected, `4` accepted;
  `maxRounds: 20` with `maxTargetsPerRound: 32` rejected (`> 256`);
  `onDemandPerMinute` 0 and 601 rejected;
  `jobRetention: 24h` with `maxRetention: 24h` rejected (`< +1h`);
  `maxEvery` below `minEvery` rejected;
  `maxDuration: 120s` with `cpuSeconds: 60` rejected;
  defaults violating limits (e.g. `defaults.sampling.rounds: 6` with `maxRounds: 5`) rejected;
  `credsFile` naming a missing file rejected;
  a realm without `pgo` loads with all three flags false;
  `replicas: all` and `replicas: 3` accepted, `replicas: many` and `replicas: 300` (over the ceiling) rejected;
  `config validate` output contains the memory figure and the required grace period.

- [ ] **Implement**, run the tests, then validate and commit

```bash
mise exec -- go test -race ./internal/config/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/config/ cmd/profgate/
git commit -m "feat(config): add nats, pgo, and realm pgo blocks"
```

---

## NATS seam: client, views, and watches

**Files:**
- Create: `internal/natskv/natskv.go`, `internal/natskv/client.go`, `internal/natskv/client_test.go`,
  `internal/natskv/fixtures_test.go` (in-process server helper)

**Produces:** the exact interface of the spec's *NATS Access*, "The seam" — copy it verbatim:
`Entry` (with `Created`, `Synced`, `Generation`), `KV`, `ObjectInfo`, `Objects` (with `List`),
`Status`, `Statused`, `Stores`, `Client`
(`Connected`, `Generation`, `Synced`, `View`),
and the sentinel errors `ErrKeyNotFound`, `ErrKeyExists`, `ErrRevisionMismatch`, `ErrObjectNotFound`, `ErrUnavailable`.
`Preflight` lands in the next task and stays the only exported production entry point;
this task builds the unexported connection constructor it will call,
reached by tests through a hook exported in `export_test.go` (the pattern `internal/k8s` already uses).

Mechanics the implementation must follow (spec: "The seam" and "The replay barrier"):

- Every call carries a 5-second deadline in addition to the caller's.
- The generation counter increments in the nats.go **disconnected** callback (`nats.DisconnectErrHandler`),
  never in the reconnected one.
- `View(gen)` returns `ErrUnavailable` when `gen` is not current;
  every method of a view compares generations before the call and again on the result,
  returning `ErrUnavailable` on either mismatch.
- `Watch` maps nats.go's initial-values-then-nil protocol to entries and one `Synced` marker;
  deletes arrive as nil-`Value` entries; entries carry the generation they were delivered under;
  on reconnect the seam re-opens the watch for the new generation and a fresh replay precedes a fresh marker.
- Errors map: `jetstream.ErrKeyExists` → `ErrKeyExists`, wrong-last-revision → `ErrRevisionMismatch`,
  `jetstream.ErrKeyNotFound` **and** `jetstream.ErrKeyDeleted` → `ErrKeyNotFound`
  (v1.53.1 returns them separately; the seam's `Get` contract folds absent and deleted together),
  `jetstream.ErrObjectNotFound` → `ErrObjectNotFound`,
  timeouts and connection errors → `ErrUnavailable`; everything wraps with `%w`.
- `Objects.Delete` of an absent name is success.
- `Status` reads the bucket's stream configuration (TTL, MaxValueSize, MaxBytes, Storage, Discard).

- [ ] **Write the in-process fixture** (`fixtures_test.go`):
  one `nats-server` per subtest with JetStream on `t.TempDir()`, random port,
  provisioned with the three buckets per the contract, returning a connected `Client` and a raw admin `nats.Conn`;
  helpers to stop and restart the server in place (same store directory, same port),
  and a variant that runs the server with per-user permissions for the permission tests of the next task.

- [ ] **Add the modules, write the failing tests, and watch them fail to compile**

```bash
mise exec -- go get github.com/nats-io/nats.go@v1.53.1 github.com/nats-io/nats-server/v2@v2.14.5
```

The cases are the first half of the spec's `internal/natskv` testing bullet:
create-exists, stale update, stale delete (value unchanged after each);
`Get` of a key that was deleted is `ErrKeyNotFound`, same as one that never existed;
watch delivers existing keys, one `Synced` marker with an empty key, later puts, deletes as nil values,
stops on context end, and the marker arrives for an empty prefix;
server restarted → watch re-opened, fresh marker after full replay;
the disconnected callback alone moves `Generation()` and makes `Synced(gen)` false before the connection is usable,
with no watch re-opened yet (drive the callback via server stop; assert before restart completes);
a `View(gen)` issued after the generation moved is `ErrUnavailable` before reaching the server;
a call issued before the move whose result arrives after it is `ErrUnavailable` whatever the server answered
(block the server response with a `nats.Conn` proxy or by pausing the server between request and reply —
a test-only hook on the view that delays the post-call check deterministically is acceptable,
and must be named in the code);
`View` for a non-current generation is `ErrUnavailable`;
`Objects.Delete` absent name is nil;
`List` returns every object with `ModTime`, empty for an empty bucket;
every call against a stopped server returns `ErrUnavailable` within its deadline;
an Object `Put`/`Get` round-trips 40 MiB byte for byte; `Get` absent is `ErrObjectNotFound`.

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/natskv/ && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add internal/natskv/ go.mod go.sum
git commit -m "feat(natskv): add the NATS seam with views"
```

---

## NATS seam: preflight, contract, and permissions

**Files:**
- Create: `internal/natskv/preflight.go`, `internal/natskv/preflight_test.go`
- Modify: `internal/natskv/natskv.go` (the `Preflight` signature), `internal/natskv/fixtures_test.go`

**Produces:**

```go
type Options struct {
    URL            string
    CredsFile      string
    ConnectTimeout time.Duration
    // OnConnectionChange, when set, runs once with true after the initial
    // connection succeeds inside Preflight, then in the disconnected callback
    // with false and the reconnected callback with true; the caller wires it to
    // metrics.Recorder.NATSConnected so natskv never imports internal/metrics.
    // Without the initial call the gauge would read zero until the first reconnect.
    OnConnectionChange func(up bool)
}

// Preflight connects, opens the three buckets through View(Generation()),
// checks their Status against the configuration contract, and runs the probes.
func Preflight(ctx context.Context, opts Options, instanceID string, log *slog.Logger) (Client, error)
```

Per the spec's *Permission Boundary*, "NATS preflight" and "Bucket configuration contract":
open `PROFGATE_CONFIG`, `PROFGATE_JOBS`, `PROFGATE_ARTIFACTS`;
verify kind and `Status` (TTL 0, file storage, `Discard: new`, `MaxBytes` unlimited or ≥ 64 MiB / ≥ 1 GiB,
KV `MaxValueSize` unlimited or ≥ 512 KiB);
then per KV bucket, under one 10s deadline:
open a `Watch` on `probe.<instanceID>` first, run `Create`, `Update` at the returned revision, `Get`, `Delete`,
and require the watch to deliver all three revisions before closing it;
per Object Store: `Put`, `Get`, `List` (must contain the probe), `Delete` of `probe-<instanceID>`.
Any violation or permission error returns an error naming the bucket and the operation or field;
connection failures are transient (`ErrUnavailable`) and the caller retries.
A probe left by a crash is cleaned by the sweeper, not here.

- [ ] **Write the failing tests** — the second half of the spec's `internal/natskv` bullet:
  missing bucket → error naming it; `PROFGATE_ARTIFACTS` created as KV → error;
  each contract field violated one at a time (1-minute TTL, memory storage, `Discard: old`,
  1 MiB `MaxBytes`, `MaxValueSize` below 512 KiB) → error naming bucket and field;
  the provisioning commands' configuration passes;
  users lacking, in turn, publish on `$KV.PROFGATE_JOBS.>`, subscribe on `$KV.PROFGATE_JOBS.>`
  (watch never delivers the probe revisions), publish on `$O.PROFGATE_ARTIFACTS.>`,
  subscribe on `$O.PROFGATE_ARTIFACTS.>`, and `$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>`
  → error naming bucket and operation, and no probe key or object remains;
  success only after the watch delivered the create, update, and delete revisions,
  proven deterministically and separately from the permission cases:
  a named test-only interceptor on the watch channel swallows deliveries while the probe writes succeed,
  preflight blocks until its 10s deadline and fails naming the bucket and the watch,
  and the same run with the interceptor released succeeds;
  a recording wrapper on the connection collects every published subject across all seam operations
  and asserts the set is a subset of the spec's *NATS permissions* list;
  `OnConnectionChange` fires true once when preflight's initial connection succeeds,
  false on server stop, and true again on restart (a recording callback asserts the exact sequence).

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/natskv/
mise run lint && mise run test && mise run check
git add internal/natskv/
git commit -m "feat(natskv): preflight probes and contract"
```

---

## Metrics for PGO

**Files:**
- Modify: `internal/metrics/recorder.go`, `internal/metrics/prometheus.go`, `internal/metrics/prometheus_test.go`,
  `internal/httpapi/fixtures_test.go`

**Produces:** `Recorder` gains (and `Noop` stubs):

```go
Collection(result string)                 // completed | failed | cancelled | expired
CollectionSample(result string)           // ok | failed
CollectionDuration(d time.Duration)
ScheduleSlot(result string)               // won | lost | busy | capacity
SweeperDelete(kind string)                // artifact | record | slot | active | orphan | probe
CollectionsActive(delta int)
NATSConnected(up bool)
```

backed by the spec's *Metrics* table:
`profgate_collections_total{result}`, `profgate_collection_samples_total{result}`,
`profgate_collection_duration_seconds` (buckets `10, 30, 60, 120, 300, 600, 1200`),
`profgate_schedule_slots_total{result}`, `profgate_sweeper_deletes_total{kind}`,
`profgate_collections_active`, `profgate_nats_connected`.
`metrics.Endpoint` gains `pgo_policy`, `collections`, `collection`, `collection_profile`, `collection_cancel`
as constants; `Request`'s contract line documents `profile` fixed to `cpu` for the last three and `none` otherwise.

- [ ] **Write the failing tests**: extend the pedantic-registry comparison with each new metric; label sets pinned.
- [ ] **Implement**,
  giving the test `recorder` in `internal/httpapi/fixtures_test.go` the seven new methods as no-op accumulators,
  because the interface expansion otherwise breaks that package's build and this task's validation block.
  Then validate and commit

```bash
mise exec -- go test -race ./internal/metrics/
mise run lint && mise run test && mise run check
git add internal/metrics/ internal/httpapi/fixtures_test.go
git commit -m "feat(metrics): add PGO recorders"
```

---

## PGO policy, identifiers, records, and clock

**Files:**
- Create: `internal/pgo/policy.go`, `internal/pgo/id.go`, `internal/pgo/record.go`, `internal/pgo/clock.go`,
  `internal/pgo/policy_test.go`, `internal/pgo/id_test.go`, `internal/pgo/record_test.go`

**Produces:**

```go
package pgo

// Policy and its blocks exactly as the spec's *Policy*, "Shape" defines them,
// with Duration marshaling as a Go duration string and Replicas as "all" or an integer.
type Policy struct { ... }

// Effective layers override onto defaults, block by block, one level deep; null fields are unset.
func Effective(defaults Policy, override *PolicyOverride) Policy
// Validate checks p against the ceilings; each violation names the field and the ceiling.
func Validate(p Policy, lim config.PGOLimits) []Violation
// StoredOverride is the KV value shape: {"policy": ..., "updatedBy": ..., "updatedAt": ...}.

// newID returns 20 lowercase Crockford base32 characters from 100 bits of crypto/rand.
func newID() string

// Record is the job.<id> value of the spec's *Collections*, "Record", every field JSON-tagged as shown there.
// Manifest is the spec's *Manifest* shape.
// MarshalBounded serializes and errors past maxRecordBytes (512 KiB), so every Update is size-checked.
// Deadline computes the spec's formula from the snapshot and the ceilings, never the live target count.

// Clock is the seam every loop uses: Now(), NewTimer, NewTicker; a fake drives every time-based test.
type Clock interface { ... }
```

- [ ] **Write the failing tests** — the spec's `internal/pgo` policy bullet, plus record arithmetic:
  layering one level deep (`{"sampling":{"rounds":3}}` changes `rounds` only);
  `null` as unset;
  every ceiling violated one field at a time at write validation;
  every ceiling violated one field at a time at read —
  a stored override exceeding a since-lowered ceiling reports the violation naming the field and the ceiling
  (the scheduler task proves the Service is then ineligible and the violation logged,
  the HTTP task that `GET /pgo` reports it in `violations`);
  `replicas` as `"all"`, integer, integer above `maxTargetsPerRound`, and rejected strings;
  `every` above `maxEvery`; `enabled` has no default (absent → false);
  `newID`: 20 chars, alphabet `0-9a-hjkmnp-tv-z`, 10k draws distinct;
  the record example of the spec round-trips JSON unchanged;
  a record at the ceiling (256 samples, every field at maximum length: 253-char pod and node names,
  36-char UIDs, nanosecond offset timestamps, 32-char reasons) stays under 512 KiB;
  `MarshalBounded` past the constant errors;
  `Deadline` with `replicas: all` uses `maxTargetsPerRound`, not a live count
  (`batches = ceil(min(replicas, maxTargetsPerRound) / maxParallel)`, `admissionWait = duration + roundInterval`).

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/pgo/
git commit -m "feat(pgo): policy, records, and identifiers"
```

---

## Publisher and scheduler

**Files:**
- Create: `internal/pgo/publisher.go`, `internal/pgo/scheduler.go`,
  `internal/pgo/publisher_test.go`, `internal/pgo/scheduler_test.go`,
  `internal/pgo/fixtures_test.go` (in-process server + fake clock + two-instance harness)

**Produces:** the spec's *Scheduling* in full:

- `slotFor(now, every)` and `offset(ns, svc, slot, jitter)` with the exact encodings
  (decimal Unix seconds UTC; FNV-1a 64 over `<ns>/<svc>/<slotUnixSeconds>`).
- `Publisher` per replica, shared by scheduler and API:
  `Reserve()` counting `cachedLive + reserved` against `maxLiveCollections`
  (`cachedLive` = Services with an active key or a nonterminal record in the watched caches),
  `Track(ns, svc, id)`, `Release()`,
  the release rule evaluated every tick (cache delivery of `job.<id>` in any state or the active key with this id;
  or authoritative reads showing job absent/terminal **and** key absent/other id; `ErrUnavailable` holds),
  and `Publish(view, ns, svc, origin, claimBy, snapshot)` doing the three writes in order —
  `Create job.<id>` as `initializing`, `Create active.<ns>.<svc>`, `Update` → `pending` —
  with the exact indeterminate-create and lost-create handling of the spec's "Publishing a Collection" pseudocode.
- The publisher owns the transition logs its writes produce (spec: *Operations*, logging):
  one record for the `initializing` create and one for the `initializing → pending` update,
  each carrying the spec's transition fields (`collection, namespace, service, state, attempt, reason, instance`),
  plus `trigger` (`schedule` or `api`) as extra context, never a Pod IP.
  Every other transition is logged by whichever component commits it —
  the worker for claims and its terminations, the cancel handler for `cancelled`,
  the sweeper for its flips — so no transition is recorded twice.
- `Scheduler` ticking every 10s behind the barrier:
  effective policy per override in the watched `PROFGATE_CONFIG` cache,
  ceiling and enabled checks, fire time, cached-live skip (`busy`),
  `Reserve` (`capacity`), slot `Create` (`lost` on `ErrKeyExists`; release and log on `ErrUnavailable`),
  then `Publish` with `claimBy = now + every`;
  every considered Service records `ScheduleSlot(result)` (`won`/`lost`/`busy`/`capacity`)
  through an injected `metrics.Recorder`.
- Watched caches for `service.*`, `job.*`, `active.*`, and `schedule.*`,
  built from `natskv.Watch` with a generation-tagged rebuild on re-replay,
  and the `pgoSynced` barrier (`Synced(gen)` for `gen = Generation()`).

- [ ] **Write the failing tests** — every case of the spec's `internal/pgo` scheduler bullet, verbatim.
  The harness runs two scheduler+publisher instances over one in-process server with one fake clock;
  barriers (channels) pause creators between writes; "frozen watch" holds a cache's delivery;
  "killed creator" abandons a publication between writes;
  counting assertions read the authoritative bucket, not the caches.
  Highlights that shape the fixture:
  exactly one record per slot across 100 interleaved ticks;
  a ceiling-violating stored override never creates a slot
  and the violation is logged once per revision at warning level;
  clock jumped 3 days → one Collection;
  `every: 24h` slot-key retention across `retainUntil`;
  differing `every` revisions → two slot keys, one record;
  the ceiling suite (100 Services / `maxLiveCollections: 8` / two instances ≤ 16 live;
  frozen watches with concurrent scheduled and on-demand creations;
  `claimBy` passing does not release; indeterminate creates tracked and resolved;
  the restarted-publisher replay barrier;
  nonterminal records ≤ `2 × replicas × maxLiveCollections` across ticks and restarts);
  slot-create `ErrUnavailable` in both outcomes;
  the exact slot key `schedule.payment.payment-api.1787529600` and hash input `payment/payment-api/1787529600`;
  a tick over a won, a busy, a capacity-refused, and a lost Service leaves exactly those
  `ScheduleSlot` counter rows;
  a successful publication leaves exactly two transition log records
  (`initializing`, then `pending`, with `trigger: schedule`) and a refused one leaves none;
  the generation tests (state changed during outage, watch re-opening held → no operation;
  tick paused after `Synced(g)`/`View(g)`, disconnect+reconnect, released call → `ErrUnavailable`, nothing written).
  Two spec cases need components later tasks build and land there instead:
  the publication race against a sweeper pass (*Sweeper* task)
  and the reclaimed Collection's merge content (*Worker: rounds…* task).
  Each test the spec marks with "the test fails when X" must be verified to fail under mutation X before it ships.

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/pgo/
git commit -m "feat(pgo): leaderless scheduler and publisher"
```

---

## Worker: claim, scan, and owner loop

**Files:**
- Create: `internal/pgo/worker.go`, `internal/pgo/worker_test.go`
- Modify: `internal/pgo/fixtures_test.go`

**Produces:** the spec's *Collections* sections "Claim", "The owner loop", "Recovery":

- The worker watches `job.*` (through the shared caches)
  and attempts claims on delivery and on the **scan** every `leaseTTL / 2`,
  with the spec's scan pseudocode verbatim:
  fresh `Get` per candidate; `initializing` past `createdAt + 1m + skewMargin` → `failed not_published`;
  `pending` past `claimBy + skewMargin` → `failed not_claimed`;
  `running` past `deadline + skewMargin` → `failed deadline_exceeded`; claimable → claim.
- `claim`: ceiling check first (→ `failed limit_exceeded`, active key released, no slot reserved);
  `reserveLocalSlot` against `maxActiveCollections`; attempts check;
  `Update` to `running` with lease and (first claim) `startedAt`/`deadline`;
  run only on a successful `Update` with its revision; any error releases the slot and profiles nothing.
- `terminate` and `releaseActive` helpers exactly as specified
  (release deletes the key only when it names this id; failures left to the sweeper).
- The **owner loop**: one goroutine holding `rev` and `committedLeaseUntil`;
  renewal every `leaseTTL / 3` with call deadline `min(5s, committedLeaseUntil - now - skewMargin)`;
  proposed lease committed only on success;
  work context cut off at `committedLeaseUntil - skewMargin`, reset on renewal;
  immediate cancel on `ErrRevisionMismatch` (one re-read for logging);
  abort when the committed lease lapses;
  the final update gated on committed lease, `deadline - skewMargin`, and the cancellation flag;
  the local slot held until the work goroutine exits.
- The view is taken once at claim time; a disconnect makes later renewals `ErrUnavailable` (the abort path).
- The lifecycle seam `cmd/profgate` drains through (spec: *Shutdown*):

```go
// Run watches, scans, and claims until ctx ends; ending ctx stops new claims.
func (w *Worker) Run(ctx context.Context)
// Drain blocks until every owner loop and work goroutine has exited,
// waiting per Collection no longer than its deadline;
// a work goroutine still running at its Collection's deadline is abandoned,
// logged by Collection id, and Drain returns without it.
func (w *Worker) Drain(ctx context.Context) error
```

  The scheduler and sweeper need no drain call: cancelling their run context stops them at the next tick.
- The worker owns its slice of the *Operations* observability contract, injected as `metrics.Recorder` + `*slog.Logger`:
  every state transition it performs (claim, every `terminate`, the final update) emits the spec's transition record
  (`collection, namespace, service, state, attempt, reason, instance`);
  `CollectionsActive(+1)` on a successful claim and `(-1)` when the local slot is released;
  `Collection(result)` at every terminal transition this replica commits;
  `CollectionDuration` observed once, at the `completed` update, as `finishedAt − startedAt`.

The work goroutine's body (rounds, sampling, finish) is a `run func(ctx, workInput) workResult` seam in this task,
implemented by the next one; tests here drive it with stubs (blocking, cancelling-ignoring, failing).

- [ ] **Write the failing tests** — the claim/lease/owner-loop slice of the spec's worker bullet:
  two workers racing one `pending` record;
  fake clock past `leaseUntil + skewMargin` with no KV write → scan reclaims, attempt 2
  (fails if the scan is removed);
  `pending` past `claimBy` → `not_claimed`;
  records under `maxParallel: 8` claimed by a worker limited to 4 → `limit_exceeded`,
  keys released, no slot reserved, trap server untouched
  (fails when the check is removed or moved after `reserveLocalSlot`);
  nothing claimed or scanned while the replay marker is held, both start on delivery
  (the sweeper's half of this case lands with the *Sweeper* task);
  reclaimed owner with a `skewMargin`-fast clock issues no final update past `deadline - skewMargin`
  and deletes its object;
  `running` past `deadline` failed, not claimed;
  `attempts_exhausted`;
  blocked renewal past `leaseUntil - skewMargin` aborts, trap proves no later fetch;
  `ErrUnavailable` renewal leaves the committed lease and cutoff unchanged;
  work stub held at a barrier past the cutoff, with and without a reclaiming scan
  (the *Worker: rounds…* task repeats this with barriers inside the real `Merge`, `Write`, and `Put`);
  a cancellation-ignoring work stub past the cutoff: no final update, local slot held,
  a second claimable record not claimed until it returns;
  renewal `ErrRevisionMismatch` cancels the work context immediately;
  renew and finish serialized (never a mismatch against the owner's own write);
  a `skewMargin`-fast claimer does not reclaim a valid lease;
  claim `Update` `ErrUnavailable` → nothing profiled, slot free (trap + counting `Discovery`);
  every terminal transition deletes the active key, a key renamed to another id is left alone;
  observability: a completed and a failed Collection leave exactly those
  `profgate_collections_total` rows, one duration observation (for the completed one),
  a `profgate_collections_active` gauge that returns to zero,
  and one transition log record per state change the worker commits,
  carrying `collection, state, attempt, reason, instance` and no Pod IP;
  a Collection cancelled under the worker leaves no `cancelled` row from the worker —
  that row belongs to the cancel handler whose CAS wins (the *HTTP API* task) —
  and the worker's lost final update records nothing.

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/pgo/
git commit -m "feat(pgo): worker claim, scan, and owner loop"
```

---

## Worker: rounds, sampling, merge, and finish

**Files:**
- Create: `internal/pgo/rounds.go`, `internal/pgo/rounds_test.go`, `internal/pgo/testdata/*.pprof` fixtures
- Modify: `internal/pgo/worker.go` (wire the real run function)
  and `internal/httpapi/` (the shared-gate contention test against the real handler)

**Produces:** the spec's *Collections* sections "Rounds", "Finish", "Cancel" (owner side):

- Round loop exactly as the spec's pseudocode: re-resolve through `k8s.Discovery.Targets`,
  version filtering and round-0 resolution (`version_missing`, `version_conflict`),
  `resolvedVersion` filter, `no_targets`, injected `Shuffle`,
  `want` capped by `maxTargetsPerRound` with `manifest.truncated`,
  fan-out with `maxParallel` goroutines, `gate.Acquire` waiting `duration + roundInterval` (`slot_timeout`),
  `defer release()` after the body is consumed and closed on every path,
  gateway `Confirm` + the interactive proxy transport with `seconds = duration`,
  decode per "Decoding a sample" (double `io.LimitReader`, gzip magic re-check → `sample_malformed`,
  `profile.ParseData` only on uncompressed bytes, decoder as an injectable function),
  first sample assigned without `Merge`, later `profile.Merge([]{merged, sample})` with
  `incompatible_profile` leaving `merged` unchanged,
  `maxMergedBytes` checked after every merge (`merged_too_large`),
  `no_samples` per round, `roundInterval` sleeps on the fake clock.
- Finish: `Compact`, `Write` (`serialize_failed`), `Put` of `<id>-<attempt>.pprof` (`artifact_store_failed`),
  hand-off to the owner loop; the owner's final-update gate and loser cleanup per the spec,
  `record_too_large` dropping `manifest.samples`.
- Sample results recorded in the manifest with the spec's reason vocabulary;
  `CollectionSample(result)` recorded per sample (`ok`/`failed`);
  one debug-level sample log per sample with `pod`, `round`, `result`, `bytes`, never an IP.

- [ ] **Write the failing tests** — the remaining worker bullet cases:
  stale owner at a barrier vs a completed reclaimer, both `Put` orders, winner's bytes intact;
  a worker crashed mid-round and reclaimed: the reclaimer's merge contains only the second attempt's samples,
  never the first attempt's (fails if attempt-one state leaks into attempt two);
  the work goroutine held at a barrier inside the real `Merge`, inside the real `Write`, and inside the real `Put`,
  in turn, past `committedLeaseUntil - skewMargin`:
  without a reclaiming scan the owner issues no final update and stores nothing;
  with one, the reclaimer completes and the stale owner's late object is deleted and never named;
  renewal `ErrRevisionMismatch` stops the worker before its next sample is fetched (a trap server asserts no dial),
  and `ErrUnavailable` on every renewal until `leaseUntil - skewMargin` passes stops it likewise;
  round-0 `version_conflict` / `version_missing`; new-version Pod excluded in round 1; all-rolled → `no_targets`;
  `replicas: 2` over five Pods, fixed `Shuffle`: two distinct Pods per round, 20-round union covers five;
  two production-seeded workers diverge within 20 rounds;
  first-sample-no-Merge (a `Merge` seam fails on nil);
  `replicas: all` over `maxTargetsPerRound + 3` Pods → exactly the cap, `truncated: true`;
  counting decoder: input bytes held ≤ `maxParallel × 2 × maxSampleBytes`;
  4 KiB gzip expanding past the limit → `sample_too_large` before `ParseData` (fails without the limit);
  nested gzip → `sample_malformed`, `ParseData` never called;
  heap-delta regression guard under `decodeFactor × len(fixture)`, skipped under `-race`;
  multi-batch Collection leaves the gate at capacity (fails when any path omits `release`);
  `merged_too_large` before the next merge;
  differing sample types → `incompatible_profile`, running profile unchanged, round continues;
  failing writer → `serialize_failed`, nothing stored;
  ceiling record fits; forced-oversize → `record_too_large`, object deleted, terminal record without samples updates;
  deadline computed from the cap;
  oversized sample `sample_too_large`; unparseable `parse_failed`; all-failed round `no_samples`;
  merged object parses and sample counts sum;
  a round with two `ok` and one failed sample leaves those `profgate_collection_samples_total` rows
  and three debug sample logs;
  `deadline_exceeded`; `Put` failure → `artifact_store_failed`, no record flip;
  cancel between rounds and mid-sample → no object;
  slot pressure: real `httpapi` handler and real worker share one `admit.Gate` capacity 3, `maxParallel` 2 —
  an interactive request always finds a slot (fails when either side gets its own gate).

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/pgo/ ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/pgo/ internal/httpapi/
git commit -m "feat(pgo): rounds, in-memory merge, and finish"
```

---

## Sweeper

**Files:**
- Create: `internal/pgo/sweeper.go`, `internal/pgo/sweeper_test.go`

**Produces:** the spec's *Collections*, "Sweeper": a 60s pass per replica, behind the barrier, one view per pass,
implementing the seven-row condition table verbatim —
expiry (delete object then flip, absent object is success), missing-object flip,
record retention, slot retention by stored `retainUntil`,
the orphan rule with its fresh `Get` (cache is a candidate filter; `ErrUnavailable` keeps the object),
active-key release after a fresh job read, and probe cleanup by `Entry.Created` / `ModTime`;
every threshold carries `skewMargin` toward later deletion;
`SweeperDelete(kind)` recorded per delete,
`Collection("expired")` recorded on every `completed → expired` flip this replica wins,
and each flip logged as a transition record.

- [ ] **Write the failing tests** — the spec's sweeper bullet, complete:
  expiry order and lost-update tolerance; retention never deletes `completed` directly;
  slot keys after `retainUntil` only; orphan age boundary both sides;
  a named object never deleted before `expiresAt`;
  frozen cache vs authoritative `completed` → object survives (fails without the fresh `Get`);
  `ErrUnavailable` keeps the object;
  active keys per job state; probe key and object age boundaries;
  an expiry pass leaves one `expired` collections-counter row and the matching `SweeperDelete` kinds;
  and the two cases deferred from earlier tasks:
  a creator paused between its active-key create and its `pending` CAS while a sweeper pass
  and a second creator run — exactly one live Collection results, the active key survives the pass;
  nothing is swept while the replay marker is held, and the first pass runs once it is delivered.

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/pgo/
git commit -m "feat(pgo): sweeper for artifacts and records"
```

---

## HTTP API: PGO routes

**Files:**
- Create: `internal/httpapi/pgo.go`, `internal/httpapi/pgo_policy.go`, `internal/httpapi/pgo_collections.go`,
  `internal/httpapi/pgo_test.go`, `internal/httpapi/pgo_policy_test.go`, `internal/httpapi/pgo_collections_test.go`,
  `internal/pgo/runtime.go`, `internal/pgo/runtime_test.go`
- Modify: `internal/httpapi/server.go` (route table, `Deps` gains the PGO dependencies), `internal/httpapi/audit.go`

**Produces:** the seven routes of the spec's *HTTP API* with its route/method/realm-flag table,
the amended request algorithm
(`501 pgo_disabled` then `503 pgo_unavailable` between readiness and authentication;
realm `pgo` flags after namespace and Service;
per-route `Allow` lists on 405),
the `/v1/collections/{id}` realm-after-read rule answering `404 collection_not_found` for denied and missing alike,
the policy ETag matrix (`201`/`200`/`412`/`428`, quoted decimal only, `*` rejected),
`DELETE` read-then-conditional-delete (`404 pgo_override_not_found`, lost revision → `412`),
`GET /pgo` with `source`, `override`, `effective`, `violations`,
`POST /collections` behind the barrier,
ordered token bucket → advisory discovery and version pre-check → publisher (`Reserve`, then `Publish`),
so both advisory `409` responses precede every write,
list (newest first, ≤ 100, no query params), get, download with its streaming outcome table,
including download's `completed → expired` flip when the object is missing —
the handler whose conditional update wins owns that transition's log record and `Collection("expired")`,
exactly as the sweeper owns the same transition on its own path (one owner per winning CAS),
cancel with the five-attempt loop (`collection_initializing`, `collection_terminal`, `cas_contended`),
64 KiB JSON bodies with unknown fields rejected,
the error codes of the spec's "Errors" table,
and one audit record per request (`principal, namespace, service, collection, method, status, code, duration_ms`).
The cancel handler whose conditional update wins also owns the `cancelled` transition:
it emits the transition log record and `Collection("cancelled")` —
the worker records only transitions it performs itself, so the result is never counted twice.

`Deps` gains one field for all of it: a `*pgo.Runtime`, the late-binding seam this task defines in `internal/pgo`.
`Runtime` holds an `atomic.Pointer` to an immutable bundle
(`natskv.Client`, publisher, shared caches, effective-policy function, on-demand token bucket);
`Bind(bundle)` is called exactly once, after `natskv.Preflight` succeeds,
and every accessor before that reports unavailable.
Handlers never receive the client, publisher, or caches directly:
each request asks the `Runtime`, answers `503 pgo_unavailable` while unbound,
and otherwise takes `gen`/`Synced`/`View` per request exactly as the loops do.
The seam exists because the HTTP server must start before NATS preflight has succeeded
(interactive routes stay available while NATS is unreachable — spec: *Failure Scenarios*),
so the PGO dependencies cannot be constructor arguments.

- [ ] **Write the failing tests** — the spec's `internal/httpapi` bullet, complete:
  the route × method × realm flag × state table including `501` and `503` variants
  (state-touching routes `503` while `/readyz` stays 200 — drive with a fake `Client`);
  every PGO route on an unbound `Runtime` → `503 pgo_unavailable`, interactive routes unaffected;
  the `If-Match` matrix and the moved-key `DELETE` → `412`;
  cancel racing a renewal at a barrier → `200` on retry, never `409`;
  cancel losing all five CAS attempts against a still-live record (a fake `KV` that always mismatches)
  → `503 pgo_unavailable` with audit code `cas_contended`;
  download with the client disconnecting mid-stream → upstream read stops, audit code `client_gone`;
  cancel on `completed` → `collection_terminal`, on `initializing` → `collection_initializing`;
  cancelling a `pending` record with no owner → `200`, the record `cancelled`, its active key released,
  exactly one `Collection("cancelled")` row and one transition log record from the handler;
  download of `initializing` → `collection_not_completed`;
  live Collection → `429 collection_in_progress`;
  `409 version_conflict` and `409 version_missing` from the advisory pre-check leave no `job.*`, `active.*`,
  or `schedule.*` key (assert against the authoritative bucket);
  eight concurrent `POST` for one Service, frozen cache → one `202`, seven `429`, one `active.*` key;
  50 Services with `onDemandPerMinute: 10` → ten `202`, forty `429 rate_limited`, ten keys, no write for rejected;
  cache at `maxLiveCollections` → `429 capacity_exhausted`, no write;
  download reader failing after headers → connection closed, audit `artifact_stream_failed`
  (the `panic(http.ErrAbortHandler)` pattern the profile handler already uses);
  `404 collection_not_found` identical for missing and denied ids;
  `410` flips the record to `expired`,
  with exactly one `Collection("expired")` row and one transition log record from the handler;
  body size and unknown-field rejection;
  no response, header, or manifest with a Pod IP or port;
  metrics rows for the new endpoints.

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/pgo/runtime.go internal/pgo/runtime_test.go
git commit -m "feat(httpapi): serve the PGO routes"
```

---

## Serve wiring

**Files:**
- Modify: `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`, `internal/ops/ops.go` (only if readiness wiring needs it)

**Produces:** the lifecycle additions of the spec's *Operations*:
when `pgo.enabled`, after the Kubernetes preflight path is set up,
a NATS preflight goroutine retries `natskv.Preflight` with the same 1s..30s backoff,
but only for connection-level `ErrUnavailable`:
a missing bucket, a wrong kind, a contract violation, or a permission error on any probe is fatal,
ending startup with a non-zero exit that names the bucket and the operation or field (spec: *NATS preflight*);
`/readyz` additionally requires it to have passed (never the replay barrier);
on success the scheduler, worker, and sweeper start behind the barrier,
the publisher and caches are constructed once and published to the handlers through one `pgo.Runtime.Bind` call,
and `profgate_nats_connected` tracks the connection.
On `SIGTERM` the scheduler and sweeper contexts are cancelled at once,
then shutdown runs two separately bounded waits:
`apiServer.Shutdown` keeps its existing context (longest interactive profile + 30s),
while `worker.Drain` receives its own context,
bounded by the latest `deadline` among the Collections this replica owns plus `skewMargin` —
a Collection deadline can far exceed the interactive bound, and the shorter context must never cut it off;
SIGTERM never cancels a work context — in-flight Collections finish when they can (spec: *Shutdown*);
`config validate` (the configuration task) already prints the grace period this bound requires.
When `pgo.enabled` is false nothing NATS-related is constructed and the routes answer `501`.

- [ ] **Write the failing tests** (serve tests with an in-process NATS server or a fake `Client` seam in `serveDeps`):
  disabled → `501` on a PGO route, no NATS connection attempted;
  enabled with NATS down → `/readyz` 503;
  interactive routes fine once Kubernetes is ready; PGO routes `503 pgo_unavailable`;
  enabled with NATS up → `/readyz` 200 after both preflights, PGO routes pass the barrier after replay;
  SIGTERM with a barrier-held work stub
  → the stub's work context stays uncancelled and `Drain` stays blocked while the barrier holds;
  releasing the barrier lets the Collection finish and shutdown complete
  (an implementation that cancels work contexts on shutdown must fail this test —
  the spec stops new claims but lets in-flight Collections finish);
  SIGTERM with a cancellation-ignoring work stub and a fake clock advanced past the Collection's `deadline`
  → `Drain` returns at the deadline, the abandoned Collection is logged by id, and the process exits
  (fails if drain waits on the stub itself rather than the deadline);
  SIGTERM with an owned Collection whose `deadline` lies beyond the interactive HTTP bound
  → the HTTP drain context expires on schedule while `Drain` keeps waiting to the Collection deadline
  (fails if `Drain` inherits the HTTP context);
  the contract-violation exit path names the bucket and field (exit non-zero).

- [ ] **Implement**, then validate and commit

```bash
mise exec -- go test -race ./cmd/profgate/ ./internal/ops/
mise run lint && mise run test && mise run check
git add cmd/profgate/ internal/ops/
git commit -m "feat(cli): wire PGO loops and NATS preflight"
```

---

## Deployment

**Files:**
- Create: `deploy/nats/account.conf`, `deploy/nats/README.md`, `deploy/secret-nats-example.yaml`
- Modify: `deploy/base/deployment.yaml`, `deploy/base/configmap.yaml`, `deploy/deploy_test.go`

**Produces:** the spec's *Permission Boundary* and *Container* deployment artifacts:
`account.conf` holds the exact permission list of "NATS permissions";
`README.md` the three provisioning commands of the bucket contract;
the Deployment gains pod `securityContext.fsGroup: 65532`,
a real credentials volume — Secret `profgate-nats-creds`, `defaultMode: 0440`, `optional: true` —
and its `readOnly: true` mount at `/etc/profgate/nats/`,
and `resources.limits.memory` from the shipped configuration's formula (4Gi at the defaults).
The volume and mount are actual parsed fields, because a YAML comment produces nothing a manifest test can pin;
`optional: true` is what keeps the base deployable when the Secret does not exist,
which is the default state since PGO is off and nothing reads the empty directory.
The example Secret file itself is fully commented and never applied,
and it lives at `deploy/secret-nats-example.yaml`, outside `deploy/base/`:
the base's exhaustive resource test requires every base file to appear in `resources`,
and a comment-only file can neither join that list nor be exempted without weakening the test;
`deploy/base/kustomization.yaml` is therefore untouched and that test is unaffected.
The ConfigMap example stays gateway-only (PGO off by default) with a commented PGO block
whose comment also states the `terminationGracePeriodSeconds` an operator must set when enabling PGO —
the value `profgate config validate` prints for that configuration — while the base stays at 120,
which the gateway spec's drain bound already covers with PGO off.

- [ ] **Write the failing tests**: the fragment file equals the spec's permission table exactly
  (parse both, compare subject sets);
  the manifest test pins the credentials volume by name, Secret name, `defaultMode: 0440`, `optional: true`,
  its mount path `/etc/profgate/nats/` with `readOnly: true`, and pod `fsGroup: 65532`;
  the existing Deployment test's exact-count assertions move from one volume and one mount to two of each,
  still naming every element (config plus credentials);
  the golden ClusterRole test still passes untouched.
- [ ] **Write the manifests**, then validate and commit

```bash
mise exec -- go test -race ./deploy/
mise run lint && mise run test && mise run check
git add deploy/
git commit -m "feat(deploy): NATS account, creds mount, memory"
```

---

## End-to-end

**Files:**
- Create: `test/e2e/nats.yaml`, `test/e2e/scenarios_pgo_test.go`
- Modify: `test/e2e/registry.go`, `test/e2e/lanes_test.go`, `test/e2e/harness_test.go`, `test/e2e/overlays/*` as needed

**Produces:** the spec's *End-to-end* section:
`TestMain` additionally applies a NATS JetStream Deployment
(`nats:2.11-alpine`, one replica, `--jetstream`, file store on an `emptyDir`, ClusterIP Service),
provisions the three buckets through a port-forward with nats.go per the contract before the gateways start,
runs the gateways with `pgo.enabled: true` and an all-true `pgo` realm,
and purges keys and objects between scenarios without recreating buckets.
`Scenarios()` gains the nine PGO scenarios of the spec's *End-to-end* section, named here for the registry;
`pgo-preflight-negative` restarts its own gateways because it re-provisions a bucket.

- [ ] **Extend the registry and lane tests** (untagged): the new scenario names, flags, and skip rules;
  the five `needsPodReach` scenarios below run on every lane.
- [ ] **Write the nine runners**, one per spec scenario, under these registry names:
  `pgo-on-demand` — an on-demand Collection across three test-app replicas: completes,
  the artifact parses, six `ok` samples over three Pod UIDs,
  both gateways agree on record and bytes (`needsPodReach`);
  `pgo-scheduled-slot` — `PUT /pgo` with `every = minEvery`, `jitter: 0`
  → one Collection per slot across two gateways
  (the harness watches `PROFGATE_JOBS` directly; `needsPodReach`);
  `pgo-cancel` — cancel after the first round → `cancelled`, no object (`needsPodReach`);
  `pgo-version-conflict` — two versions behind one Service → `409 version_conflict`;
  pinned → one Deployment's samples only (`needsPodReach`);
  `pgo-reclaim` — the owning gateway Pod deleted after its first renewal
  → reclaim with `attempt: 2`, completes (`needsPodReach`);
  `pgo-realm-flags` — no `configure` → `403`; no `read` → `404` on an existing record;
  `pgo-disabled` — a `pgo.enabled: false` gateway → `501` on every PGO route;
  the harness records the NATS server's connection count before this gateway starts
  and proves the count never rises above that baseline while it runs —
  attribution, not an absolute zero, because the suite's shared PGO-enabled gateways stay connected;
  `pgo-clusterrole` — the gateway ClusterRole scenarios still hold with PGO enabled;
  `pgo-preflight-negative` — a recreated `PROFGATE_JOBS` with a 1-minute TTL (own gateways)
  → exit naming the bucket and `TTL`,
  and reduced NATS users (the four permission removals of the spec)
  → exit naming the bucket and operation, no probe left.
- [ ] **Run the suite**

```bash
PROFGATE_E2E_LANE=current mise run test:e2e
PROFGATE_E2E_LANE=1.23 mise run test:e2e
PROFGATE_E2E_LANE=1.24 mise run test:e2e
```

- [ ] **Validate and commit**

```bash
mise exec -- go vet -tags e2e ./test/e2e/... && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add test/e2e/ go.mod go.sum
git commit -m "test(e2e): prove PGO collection on the lanes"
```

---

## Finish the plan

- [ ] Confirm the `main` run passed every lane (the existing workflows need no change:
  `check.yml` covers the new unit tests and `e2e.yml` the lanes).
- [ ] Update `.agents/rules/500-validation-and-workflow.md`
  if review decides that `internal/pgo` or `internal/natskv` changes should also trigger the e2e suite before a PR;
  record the decision either way.
- [ ] In the same change: set line 3 of this file to `**Status:** Done` and add line 4
  `**Outcome:** <tag or commit that shipped PGO collection>`.
- [ ] `mise run lint && mise run test && mise run check`;
  `git add docs/plans/pgo.md .agents/rules/`; `git commit -m "docs: mark the PGO plan done"`.

---

## Self-Review

- Spec coverage: NATS seam, views, and barrier (*NATS seam* tasks);
  bucket contract and permissions (preflight task, deployment task, `pgo-preflight-negative`);
  policy and ceilings (*Configuration*, *PGO policy* tasks);
  scheduling and publication (*Publisher and scheduler*);
  claim, lease, recovery (*Worker: claim…*); rounds, decode, merge, finish (*Worker: rounds…*);
  sweeper (*Sweeper*); the seven routes (*HTTP API*);
  admission sharing (*Admission gate*, the slot-pressure test);
  operations, observability, and shutdown (*Serve wiring* plus the recorder call sites named per task);
  manifests (*Deployment*); the nine e2e scenarios (*End-to-end*);
  every failure-scenario row maps to at least one unit or e2e case above.
- Types: `admit.Gate`, `natskv.Entry/KV/Objects/Status/Stores/Client`, `pgo.Policy/Record/Publisher/Clock`,
  the `metrics.Recorder` additions, and the config blocks are each defined once, in the task that first needs them,
  with signatures copied from the spec.
- Task order compiles at every step:
  admit → config → natskv → metrics → pgo (policy → publisher/scheduler → worker → rounds → sweeper) → httpapi → cmd → deploy → e2e;
  no task imports a package a later task creates.
- Left to the implementer by design: fixture pprof profiles (generate with `runtime/pprof` in a test helper),
  the internal shape of the watched caches, helper names, and the deterministic post-call generation hook's exact form
  (it must be named in the code as a test seam).
- Decided during implementation, recorded here so nobody mistakes them for omissions:
  how the natskv view delays a result deterministically for the cross-generation test
  (a test-only hook, named in the code);
  whether `pgo-preflight-negative` shares the main NATS server or runs its own
  (its bucket re-provisioning must not disturb scenarios that follow, which argues for its own).
