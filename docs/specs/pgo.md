# Profgate PGO Collection

**Status:** Accepted

This document is the design of record for PGO collection,
built on the gateway defined in [`gateway.md`](gateway.md).
Everything there — permission boundary, discovery seam, HTTP API, realms,
configuration, testing — is assumed and not restated;
this document adds scheduling, collection, merge,
and the NATS state that coordinates gateway replicas.
Gateway sections are cited by heading name.
[`profgate-design.md`](profgate-design.md) is the superseded original draft.

---

## 1. Overview

An interactive profile request is one request, one backend, one profile.
PGO needs something else:
a CPU profile that represents a whole workload,
gathered from several replicas at several points in time
and merged into one artifact that `go build -pgo` can consume.

Profgate models that as an asynchronous **Collection**:
a short job that resolves a Service to its current Pods,
fetches one CPU profile from each of a sampled subset in one or more rounds,
merges the samples in memory,
and stores the merged profile in a NATS JetStream Object Store
where any gateway replica can serve it.

Temporal diversity comes from a scheduler that creates a fresh Collection every interval,
never from a job that stays alive for hours.
Collections are minutes long,
so ownership leases stay short,
a crashed worker loses little,
and rolling updates of the gateway are uneventful.

### 1.1 Core decisions

1. **CPU profiles only.**
   PGO consumes CPU profiles; a Collection's `profile` is always `cpu`.
2. **NATS JetStream for coordination and artifacts; nothing else.**
   `PROFGATE_CONFIG` and `PROFGATE_JOBS` are KV buckets;
   `PROFGATE_ARTIFACTS` is an Object Store bucket.
   No database, no PVC, no object storage service, no work queue.
3. **No leader.**
   Every replica runs the scheduler and the worker;
   `create` and revision-conditional `update` on KV keys decide who wins.
4. **No writable filesystem.**
   Samples are merged in memory as they arrive and the merged profile goes straight to the Object Store.
   The container stays exactly as hardened as the gateway spec's *Container* section describes.
5. **The Kubernetes permission boundary does not move.**
   Collections resolve and confirm targets through the same `Discovery` interface as interactive requests,
   fetch samples through the same proxy transport,
   and pass through the same admission gate.
   No new RBAC tuple, no new `internal/k8s` method.
6. **Runtime policy never exceeds static ceilings.**
   Operator defaults and hard limits are process configuration;
   NATS holds only per-Service overrides, each validated against the ceilings when written and when read.
7. **Build identity is the version label.**
   `Target.Version` from the gateway's *Version* section is the identity a Collection merges across;
   two versions never merge.
8. **Opaque Collection identifiers reveal nothing.**
   A realm that may not see a Collection receives `404`, never `403`.

### 1.2 Non-goals

- Continuous profiling, flamegraph UI, or long-term profile retention.
  Artifacts expire after hours, not months.
- Profiles other than `cpu`.
- Cron expressions; catching up on intervals missed during an outage.
- Node- or zone-aware sampling.
- Build identity stronger than a Pod label.
  The manifest records enough (Pod UID, node, version) for a later design to add it.
- Per-Pod exclusion.
  Within one Collection a Pod is sampled at most once per round;
  Collections for different Services that share a Pod, and interactive requests, may overlap on it,
  and only the admission gate bounds the total (section 8.5).

---

## 2. Architecture

```text
              Developers / CI
                     |
                     v
            +-----------------+
            | Ingress / LB    |
            +--------+--------+
                     |
        +------------+------------+
        v                         v
+---------------+         +---------------+
| Profgate A    |         | Profgate B    |
| httpapi       |         | httpapi       |
| scheduler     |         | scheduler     |
| worker        |         | worker        |
| sweeper       |         | sweeper       |
+---+-------+---+         +---+-------+---+
    |       |                 |       |
    |       +--------+--------+       |
    |                |                |
    |                v                |
    |   NATS JetStream                |
    |   KV  PROFGATE_CONFIG           |
    |   KV  PROFGATE_JOBS             |
    |   OBJ PROFGATE_ARTIFACTS        |
    |                                 |
    +----------------+----------------+
                     |
  Kubernetes API (read)  ->  Service -> EndpointSlice -> Pod -> PodIP:pprofPort
```

Each replica adds three long-lived loops to the gateway:
a **scheduler** that turns per-Service policy into Collections,
a **worker** that claims and executes them and revisits stalled ones,
and a **sweeper** that expires artifacts and deletes old records and orphaned objects.
All three are present on every replica; none is elected.
The API listener gains the PGO routes; the ops listener gains metrics.

None of the three acts, and no PGO route reads or writes state,
until the replica's NATS watches have completed their initial replay (section 5.1, "The replay barrier").

When `pgo.enabled` is false, the default, none of this exists at runtime:
no NATS connection is opened and every PGO route answers `501 pgo_disabled`.

---

## 3. Permission Boundary

The boundary in the gateway spec's *Permission Boundary* section is unchanged in its Kubernetes half
and becomes active in its NATS half:

> … and manipulates only its dedicated `PROFGATE_*` NATS stores.

### 3.1 Kubernetes

No change.
Collections read targets from the informer caches and confirm them with the same Pod `get` as interactive requests.
The golden ClusterRole test and the recording-transport test continue to pin seven tuples.

### 3.2 NATS stores

Profgate uses three stores that already exist.
It never creates, configures, or deletes a bucket;
provisioning belongs to whoever operates the NATS cluster.

| Store | Kind | Holds | Written by |
|---|---|---|---|
| `PROFGATE_CONFIG` | KV | per-Service policy overrides | the configuration API |
| `PROFGATE_JOBS` | KV | schedule slots and Collection records | scheduler, worker, sweeper, API |
| `PROFGATE_ARTIFACTS` | Object Store | merged profiles | worker, sweeper |

**Why `PROFGATE_ARTIFACTS` holds the artifact**, in the form
[`800`](../../.agents/rules/800-security-invariant.md) requires of any widening:

1. *Capability needed.*
   A merged profile must be downloadable from any replica after the worker that produced it has been replaced,
   and the container must stay read-only.
2. *What a compromised gateway gains.*
   Write access to one more `PROFGATE_*` store, inside the boundary as already worded.
   The bytes it can store there are bytes it could already fetch from any pprof port;
   the store adds a place to keep them, not a new thing to read.
3. *Narrower alternative considered.*
   An `emptyDir` on each gateway Pod, with the job record naming the owning replica
   and an internal artifact endpoint that other replicas proxy to.
   It was rejected because it needs a writable volume, a third listener, a NetworkPolicy for that listener,
   ownership verification on every internal fetch, an `artifact_lost` state for the case where the owning Pod is gone,
   and disk-pressure eviction — six mechanisms to avoid one store the boundary already names.

**Bucket configuration contract.**
Every coordination rule in this document assumes that a key or object leaves a store only through a Profgate delete:
an active key that the server expired would let a second Collection start while the first runs,
a slot key expired before `retainUntil` would let a slot fire twice,
and an artifact expired before `expiresAt` would break a `completed` record.
A bucket can be provisioned with a server-side TTL, a byte cap with old-message discard, or a value-size limit,
and every probe below would pass under each of them.
Preflight therefore reads each store's status (`natskv.Status`, section 5.1) and requires:

| Field | `PROFGATE_CONFIG`, `PROFGATE_JOBS` | `PROFGATE_ARTIFACTS` |
|---|---|---|
| `TTL` | `0` (none) | `0` (none) |
| `Storage` | file | file |
| `Discard` | new | new |
| `MaxBytes` | unlimited, or ≥ 64 MiB | unlimited, or ≥ 1 GiB |
| `MaxValueSize` | unlimited, or ≥ `maxRecordBytes` | — |

History depth is not constrained.
A violation is fatal: the process logs the bucket name, the field, and the value it found, and exits non-zero.
`Discard: new` turns a full bucket into a failed write (`ErrUnavailable` for the caller, a logged error, nothing lost)
instead of a silently evicted key.
`deploy/` documents the provisioning commands that produce a conforming set:

```text
nats kv add PROFGATE_CONFIG    --storage file --ttl 0 --max-bucket-size -1 --max-value-size -1 --history 1
nats kv add PROFGATE_JOBS      --storage file --ttl 0 --max-bucket-size -1 --max-value-size -1 --history 1
nats object add PROFGATE_ARTIFACTS --storage file --ttl 0 --max-bucket-size -1
```

Reading the status needs `$JS.API.STREAM.INFO.<stream>`, which section 3.3 already grants.

**NATS preflight.**
Before the scheduler, worker, or sweeper starts, the gateway connects, opens all three stores,
checks the configuration contract above,
and exercises every operation it will later need with reversible probes, under one 10-second deadline per bucket:
in each KV bucket, a `Watch` on the key `probe.<instanceID>` is opened first,
then `Create`, `Update` at the returned revision, `Get`, and `Delete` of that key run in order,
and the watch must deliver all three revisions — the create, the update, and the delete — before it is closed;
in the Object Store, `Put`, `Get`, `List` (whose result must contain the probe), and `Delete` of the object `probe-<instanceID>`.
Opening a bucket only looks the stream up;
the probes are what turn a missing publish or subscribe permission into a startup failure
instead of a failure inside the first Collection,
and requiring the watch to deliver is what proves subscription delivery rather than subscription creation.
A missing bucket, a bucket of the wrong kind, a configuration outside the contract, or a permission error on any probe is fatal:
the process logs the bucket name and the operation or field and exits non-zero.
A probe key or object left behind by a crash between its create and its delete is ignored by every reader
and deleted by the sweeper (section 8.9).
A connection failure is transient and is retried with the same backoff as the Kubernetes preflight
(`1s..30s`, forever, logging each attempt);
`/readyz` stays `503` until the NATS preflight has passed when `pgo.enabled` is true.

### 3.3 NATS permissions

The gateway's NATS user needs exactly these subjects.
Everything not listed is denied;
in particular it may not publish to `$JS.API.STREAM.CREATE.>`, `$JS.API.STREAM.DELETE.>`,
`$JS.API.STREAM.UPDATE.>`, or any `$KV.` or `$O.` subject outside the three buckets.

| Permission | Subjects |
|---|---|
| publish | `$JS.API.INFO` |
| publish | `$JS.API.STREAM.INFO.KV_PROFGATE_CONFIG`, `$JS.API.STREAM.INFO.KV_PROFGATE_JOBS`, `$JS.API.STREAM.INFO.OBJ_PROFGATE_ARTIFACTS` |
| publish | `$JS.API.CONSUMER.CREATE.KV_PROFGATE_CONFIG.>`, `$JS.API.CONSUMER.CREATE.KV_PROFGATE_JOBS.>`, `$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>` |
| publish | `$JS.API.CONSUMER.DELETE.KV_PROFGATE_CONFIG.>`, `$JS.API.CONSUMER.DELETE.KV_PROFGATE_JOBS.>`, `$JS.API.CONSUMER.DELETE.OBJ_PROFGATE_ARTIFACTS.>` |
| publish | `$JS.API.CONSUMER.INFO.KV_PROFGATE_CONFIG.>`, `$JS.API.CONSUMER.INFO.KV_PROFGATE_JOBS.>`, `$JS.API.CONSUMER.INFO.OBJ_PROFGATE_ARTIFACTS.>` |
| publish | `$JS.API.CONSUMER.MSG.NEXT.KV_PROFGATE_CONFIG.>`, `$JS.API.CONSUMER.MSG.NEXT.KV_PROFGATE_JOBS.>`, `$JS.API.CONSUMER.MSG.NEXT.OBJ_PROFGATE_ARTIFACTS.>` |
| publish | `$JS.API.DIRECT.GET.KV_PROFGATE_CONFIG.>`, `$JS.API.DIRECT.GET.KV_PROFGATE_JOBS.>`, `$JS.API.DIRECT.GET.OBJ_PROFGATE_ARTIFACTS.>` |
| publish | `$JS.API.STREAM.MSG.GET.KV_PROFGATE_CONFIG`, `$JS.API.STREAM.MSG.GET.KV_PROFGATE_JOBS`, `$JS.API.STREAM.MSG.GET.OBJ_PROFGATE_ARTIFACTS` |
| publish | `$JS.API.STREAM.PURGE.OBJ_PROFGATE_ARTIFACTS` |
| publish | `$KV.PROFGATE_CONFIG.>`, `$KV.PROFGATE_JOBS.>`, `$O.PROFGATE_ARTIFACTS.>` |
| subscribe | `_INBOX.>`, `$KV.PROFGATE_CONFIG.>`, `$KV.PROFGATE_JOBS.>`, `$O.PROFGATE_ARTIFACTS.>` |

The Object Store client reads an object's metadata through the stream's direct-get subject before it fetches chunks,
which is why `DIRECT.GET` is granted on the artifact stream as well as on the two KV streams.
`STREAM.PURGE` on the artifact stream is what the Object Store client uses to delete an object's chunks;
it is scoped to that one stream and is the only destructive stream-level verb.
The `CONSUMER.*` subjects are what `Objects.List` (section 5.1) needs:
it reads the bucket's metadata messages through an ordered consumer.
The pinned client delivers every watch and every object read to an inbox,
so `_INBOX.>` is the subscription those consumers actually use,
and a user without the `$KV.*` and `$O.*` subscriptions still passes preflight.
Both are granted anyway, because the subject a client delivers on is its choice and not a contract.
`deploy/` ships this list as a NATS account configuration fragment beside the provisioning commands of section 3.2,
and a unit test pins the fragment the way the golden test pins the ClusterRole.

### 3.4 Container

Still no writable volume; samples live in memory and artifacts live in NATS.
The one addition is a read-only mount:
when `nats.credsFile` is configured,
the operator-created NATS credentials Secret is mounted at `/etc/profgate/nats/`
with `readOnly: true` and `defaultMode: 0440`,
and the Deployment's pod `securityContext` sets `fsGroup: 65532` —
the nonroot uid/gid the ko-built distroless image runs as
(section 4 carries the amended gateway wording;
mounting a Secret as a volume needs no Secrets API permission).
The pair is what makes the file readable by the non-root gateway:
Secret volumes are written owned by root,
and on the Kubernetes 1.23 baseline the kubelet changes a volume's group ownership only when `fsGroup` is set,
so group 65532 with mode `0440` is the narrowest grant the process can actually read.
`fsGroup` is safe for the existing mounts:
it only widens group ownership,
and the ConfigMap and projected ServiceAccount token stay readable exactly as before.
The PGO-enabled end-to-end scenarios prove the readability end to end,
because NATS preflight (section 3.2) fails startup when the credentials file cannot be read.
The Deployment's memory limit is sized from

```text
maxActiveCollections × (maxParallel × decodeFactor × maxSampleBytes + 2 × decodeFactor × maxMergedBytes)
```

with `decodeFactor = 8`, over the gateway's own footprint, where every term uses the `pgo.limits` ceiling:
per active Collection, every in-flight sample as its compressed body, its decompressed bytes,
and its decoded `*profile.Profile`;
the running merged profile in decoded form;
and the serialized copy written to the store at completion.
The input bytes are bounded exactly (section 8.5);
the decoded representations are not, so `decodeFactor` is an engineering estimate —
two buffers of input plus about six times that in decoded structures —
and the figure is a sizing rule, not a proof.
With the defaults that is 2 × (4 × 8 × 32 MiB + 2 × 8 × 64 MiB) = 4 GiB;
`profgate config validate` prints the figure for the loaded configuration,
the operator sets `resources.limits.memory` from it,
and `deploy/` ships that value for the shipped configuration.

### 3.5 What a compromised gateway can do

Everything in the gateway spec's section of the same name,
plus whatever the NATS account permissions of section 3.3 allow,
which is raw publish and subscribe on every subject of the three stores.
A compromised process is not bound by the code around those permissions:
it can forge or overwrite any job or policy record at any revision,
bypass every in-process ceiling, rate limit, and reservation,
create Collections until the bucket reaches its capacity,
and read or delete every artifact in `PROFGATE_ARTIFACTS`.
The runtime ceilings protect production from a misbehaving *configuration writer* (section 6.3);
the boundary that protects the cluster from a compromised *gateway* is the three stores.
It cannot touch any other NATS stream, bucket, or account.

---

## 4. Changes to the accepted gateway design

Accepting this document amends the following text in the same change.
Nothing else in the gateway design moves.

| File | Current text | New text |
|---|---|---|
| `docs/specs/gateway.md`, *Core decisions* 3 | "**No NATS.** The gateway binary links no NATS client library. `go.mod` not containing `github.com/nats-io/nats.go` is the checkable form of this claim." | "**No NATS on the interactive path.** Discovery, authorization, and proxying never touch NATS; [`pgo.md`](pgo.md) is the only design that uses it, and `internal/natskv` is its only importer." |
| `docs/specs/gateway.md`, *Non-goals* | "Hot-reloading configuration, Basic Auth, OIDC, PGO collection. Each is designed for in [`pgo.md`](pgo.md) or a later revision of this document" | "Hot-reloading configuration, Basic Auth, OIDC. Each is designed for in a later revision of this document; PGO collection is designed in [`pgo.md`](pgo.md)" |
| `docs/specs/gateway.md`, *Permission Boundary* | "The gateway defined here uses no NATS stores at all; the clause exists so the wording stays stable when [`pgo.md`](pgo.md) adds them." | "The gateway defined here uses no NATS stores; [`pgo.md`](pgo.md) names the three it adds." |
| `docs/specs/gateway.md`, *Container* | "The gateway has no writable volume." | "The gateway has no writable volume, with or without PGO collection." |
| `docs/specs/gateway.md`, *Container* | "Its read-only mounts are the configuration ConfigMap (section 10) and the projected ServiceAccount token that Kubernetes injects." | "Its read-only mounts are the configuration ConfigMap (section 10), the projected ServiceAccount token that Kubernetes injects, and, when `pgo.enabled` and `nats.credsFile` are configured ([`pgo.md`](pgo.md)), a Kubernetes Secret volume holding the NATS credentials file at `/etc/profgate/nats/`, mounted `readOnly: true` with `defaultMode: 0440`; the Deployment's pod `securityContext` sets `fsGroup: 65532` so the non-root gateway's group owns the volume and can read the file. The kubelet mounts the Secret; the gateway's ServiceAccount needs no Secrets API permission, and the RBAC table is unchanged." |
| `docs/specs/gateway.md`, *Build and Deployment* | (the `deploy/` bullet) | add: the operator creates the NATS credentials Secret alongside the NATS account; `deploy/` ships a commented example Secret and the Deployment's credentials volume (`defaultMode: 0440`), read-only mount, and pod `fsGroup: 65532`, pinned by a manifest test |
| `docs/specs/gateway.md`, *Dependencies* | (table) | add `github.com/nats-io/nats.go` — "PGO coordination and artifacts (only in `internal/natskv`)", move `github.com/google/pprof` from "tests only" to "profile merge and tests" |
| `docs/specs/gateway.md`, *Request algorithm* step 9 | "Acquire one of `limits.maxConcurrentProfiles` slots without waiting" | "Acquire one of `limits.maxConcurrentProfiles` slots from the shared admission gate (`internal/admit`) without waiting" |
| `docs/specs/gateway.md`, *Package Layout* | (tree) | add `internal/admit/  the admission gate shared by interactive requests and Collections` |
| `internal/httpapi/server.go`, `New` | creates the admission channel inside `New` | takes an `*admit.Gate` in `Deps`; `cmd/profgate` constructs the one instance |
| `docs/specs/gateway.md`, *Request algorithm* step 2 | "Anything but `GET` → `405 method_not_allowed` with `Allow: GET`." | "A method the route does not accept → `405 method_not_allowed` with `Allow` listing those it does; the two routes defined here accept `GET` only." |
| `.agents/rules/800-security-invariant.md`, *NATS* | "`PROFGATE_CONFIG`, `PROFGATE_JOBS`, and optionally `PROFGATE_ARTIFACTS`." | "`PROFGATE_CONFIG`, `PROFGATE_JOBS`, and `PROFGATE_ARTIFACTS`; all three are required when PGO collection is enabled, and the shipped account fragment is pinned by a test." |
| `.agents/rules/800-security-invariant.md`, *Container* | "The gateway has no writable volume at all; if the PGO draft is accepted, its ephemeral profile bytes are confined to an `emptyDir` and nothing else becomes writable." | "The gateway has no writable volume at all; PGO collection merges samples in memory and stores artifacts in NATS, so nothing becomes writable." |
| `.agents/rules/800-security-invariant.md`, *Two Mechanisms* | (add) | "**One importer of nats.go.** `internal/natskv` is the only non-test importer of `github.com/nats-io/nats.go`; `mise run check` runs the same grep shape as for client-go." |
| `.agents/rules/100-project-map.md`, *Runtime dependencies* | "The Kubernetes API only. No NATS until PGO collection lands;" | "The Kubernetes API, and NATS JetStream when PGO collection is enabled;" |
| `.agents/rules/100-project-map.md`, *Planned Structure* | "`internal/pgo/` and `internal/natskv/` arrive with the PGO design … and not before." | list both packages in the tree with one-line purposes |
| `.agents/rules/100-project-map.md`, *External HTTP API* | "`.../collections` and `.../pgo` belong to the PGO draft … and do not exist until it is accepted." | list the five PGO routes |
| `AGENTS.md`, *Two Specs, One Accepted* | the paragraph describing `pgo.md` as `Draft` | both specs `Accepted`; `pgo.md` layered on `gateway.md` |
| `scripts/check-repo.py`, `check_no_nats` | fails when `go.mod` requires `nats.go` | replaced by `check_nats_importers`: every non-test Go file outside `test/` importing `github.com/nats-io/nats.go` is under `internal/natskv/` |

---

## 5. NATS Access

### 5.1 The seam

`internal/natskv` is the only non-test package that imports `github.com/nats-io/nats.go`.
Its exported interface is the complete set of things Profgate can do to NATS:

```go
// Entry is one KV value with the revision that produced it.
type Entry struct {
    Key      string
    Value    []byte
    Revision uint64
    Created  time.Time // server timestamp of this revision, KeyValueEntry.Created()
    Synced   bool      // true on the one marker entry that ends the initial replay; Key is empty
    Generation uint64  // the connection generation this entry was delivered under
}

// KV is one bucket.
type KV interface {
    // Get returns the latest entry; ErrKeyNotFound when absent or deleted.
    Get(ctx context.Context, key string) (Entry, error)
    // Create stores value only when key is absent; ErrKeyExists otherwise.
    Create(ctx context.Context, key string, value []byte) (revision uint64, err error)
    // Update stores value only when the key's current revision equals expected;
    // ErrRevisionMismatch otherwise.
    Update(ctx context.Context, key string, value []byte, expected uint64) (revision uint64, err error)
    // Delete removes the key only when its current revision equals expected.
    Delete(ctx context.Context, key string, expected uint64) error
    // Keys lists live keys under prefix.
    Keys(ctx context.Context, prefix string) ([]string, error)
    // Watch delivers every live entry under prefix, then every later change, until ctx ends.
    // A deleted key arrives as an Entry with a nil Value.
    // The end of the initial replay arrives as one Entry with Synced set and no Key;
    // every Entry after it is a live change.
    Watch(ctx context.Context, prefix string) (<-chan Entry, error)
}

// ObjectInfo describes one stored object.
type ObjectInfo struct {
    Name    string
    Size    uint64
    ModTime time.Time // set by the NATS server when the object was put
}

// Objects is one Object Store bucket.
type Objects interface {
    Put(ctx context.Context, name string, r io.Reader) error
    // Get returns a reader for the object's bytes; ErrObjectNotFound when absent.
    Get(ctx context.Context, name string) (io.ReadCloser, error)
    // Delete removes the object; an absent name is success.
    Delete(ctx context.Context, name string) error
    // List returns every live object in the bucket.
    List(ctx context.Context) ([]ObjectInfo, error)
}

// Status is the server-side configuration of one bucket,
// read from its stream configuration (section 3.2).
type Status struct {
    TTL          time.Duration // 0 when none
    MaxValueSize int64         // -1 when unlimited; KV only
    MaxBytes     int64         // -1 when unlimited
    Storage      string        // "file" or "memory"
    Discard      string        // "old" or "new"
}

// Statused is implemented by every KV and Objects value the seam returns.
type Statused interface {
    Status(ctx context.Context) (Status, error)
}

// Stores is a view of the three buckets bound to one connection generation.
// Every method of its KV and Objects values compares the view's generation
// with the client's current generation before issuing the call and again
// when the result arrives, and returns ErrUnavailable on either mismatch;
// a mismatched mutation is indeterminate, like any other ErrUnavailable.
// There is no unbound accessor: the only way to reach a bucket is through a view.
type Stores struct {
    Config    KV
    Jobs      KV
    Artifacts Objects
}

// Client is the connection; Preflight returns it and the rest of the gateway consumes it.
type Client interface {
    // Connected reports whether the underlying connection is currently up.
    Connected() bool
    // Generation returns the connection generation: a counter the seam increments
    // in the nats.go disconnected callback, never in the reconnected one.
    Generation() uint64
    // Synced reports whether every watch opened by the PGO runtime has delivered
    // its initial-replay marker under generation gen.
    Synced(gen uint64) bool
    // View returns the stores bound to gen.
    // It is ErrUnavailable when gen is not the current generation.
    View(gen uint64) (Stores, error)
}

// Preflight connects, opens the three buckets through View(Generation()),
// checks their Status against the configuration contract of section 3.2,
// and runs the probes of that section.
func Preflight(ctx context.Context, opts Options, instanceID string, log *slog.Logger) (Client, error)
```

Sentinel errors, matched with `errors.Is`:

| Error | Meaning |
|---|---|
| `ErrKeyNotFound` | `Get` on an absent or deleted key |
| `ErrKeyExists` | `Create` lost; another actor wrote the key first |
| `ErrRevisionMismatch` | `Update` or `Delete` lost; the key moved past `expected` |
| `ErrObjectNotFound` | `Get` on an absent object |
| `ErrUnavailable` | the connection is down or the request timed out |

Every call carries a 5-second context deadline in addition to the caller's;
the worker's lease renewals use a shorter one (section 8.4).
`ErrUnavailable` maps to `503 pgo_unavailable` on the API and to "stop and let the lease expire" in the worker.
Every store operation goes through a `Stores` view bound to one generation (`View(gen)`);
the view checks `gen` against the current generation before issuing the call and again when the result arrives,
and reports `ErrUnavailable` on either mismatch, whatever the server answered,
because the caller's view of the bucket is no longer the one the result belongs to.
A disconnect between the check and the call, or between the call and its result,
therefore surfaces to the caller as unavailability rather than as a success it would act on from stale caches.

**The replay barrier.**
nats.go delivers the current value of every key under a watched prefix first,
then a nil entry that marks the end of that initial replay, then live changes;
the seam turns that nil into the `Synced` marker entry.
After preflight, the PGO runtime opens its four watches —
`service.*` in `PROFGATE_CONFIG`, and `job.*`, `active.*`, and `schedule.*` in `PROFGATE_JOBS` —
and the barrier, `pgoSynced`, is defined as `Synced(gen)` for the current generation `gen = Generation()`:
true only once every one of the four watches has delivered its marker under that generation.
Until then the scheduler publishes nothing, the worker claims nothing, the sweeper runs nothing,
and every PGO route that reads or writes store state answers `503 pgo_unavailable`
(`501 pgo_disabled` when PGO is off takes precedence, as always).

The barrier is tied to the connection generation, not to watch re-opening,
because nats.go marks the connection usable before it runs the asynchronous reconnected callback:
store operations can succeed in the gap between the two,
and a flag cleared only by a callback would let a tick or a request decide from caches that missed an outage.
The seam increments `Generation()` in the *disconnected* callback,
which runs before the connection is usable again,
so a disconnect clears the barrier at once and it stays cleared until the watches are re-opened for the new generation
and every one of them has replayed again
(a watcher that was cut off has an unknown gap behind it, so the seam re-opens all four).
Every watched cache is rebuilt from the replay rather than patched,
and marker entries and cache contents carry the generation they were delivered under,
so a cache is either complete as of a point in the stream under the current generation or not consulted at all.

Every scheduler tick, worker scan, sweeper pass, owner loop, and state-touching PGO handler begins with

```text
gen := c.Generation()
if !c.Synced(gen): idle, or 503 pgo_unavailable
stores, err := c.View(gen)                 // ErrUnavailable when gen already moved
```

and uses only that view —
`stores.Jobs`, `stores.Config`, `stores.Artifacts`, written `jobs`, `config`, `artifacts` in the pseudocode below —
for the whole operation;
a call on the view after a disconnect is `ErrUnavailable` to it,
so one tick or request never mixes two views of the bucket.
The barrier is what makes the counts of section 7.2 and the policy reads of section 10.2 trustworthy after a restart:
nothing on a replica decides from a cache that has not yet seen the bucket.
`/readyz` does not depend on the flag (section 12.2).
`ModTime` is the server's clock; the clock-skew assumption of section 8.4 covers it.

`Status` is the one read of stream configuration; nothing writes it.
There is no method that creates, updates, or deletes a bucket, stream, or consumer directly;
the consumers NATS creates for `Watch` are ephemeral and belong to the connection.

### 5.2 Atomicity primitives

Every piece of shared state has one primitive, named here and used nowhere else:

| State | Key | Primitive | Who wins |
|---|---|---|---|
| one Collection per schedule slot | `schedule.<ns>.<svc>.<slot>` | `Create` | the first replica; the rest observe `ErrKeyExists` |
| one live Collection per Service | `active.<ns>.<svc>` | `Create` after the `initializing` record exists and before it becomes `pending`; `Delete` at its revision after the terminal update | the creator whose `Create` succeeds, scheduler or API alike; every other creator observes `ErrKeyExists` |
| live Collections cluster-wide | the set of Services with an `active.*` key or a nonterminal `job.*` record | counted from the watched caches (`cachedLive`) plus the replica's local reservations for publications the caches have not delivered yet, before a creator writes anything, and only behind the replay barrier; no cluster-wide primitive, so the ceiling `pgo.limits.maxLiveCollections` is per replica, giving `replicas × maxLiveCollections` (section 7.2) | — |
| Collection ownership and every state transition | `job.<id>` | `Update` with the revision last read | the replica whose read was most recent; the loser re-reads |
| policy override | `service.<ns>.<svc>` | `Create` for a new key; `Update` with the client's `If-Match` revision | the client whose ETag is current |
| artifact bytes | `<id>-<attempt>.pprof` | `Put` by the owner of that attempt, then named in the `completed` `Update` | the attempt whose `Update` wins; every other attempt's object is unreferenced and is deleted by its writer or by the sweeper |

Nothing takes a snapshot for reassurance, pre-checks before a conditional write, or reads one replica's memory to decide about another.

### 5.3 Paths that touch each key

`service.<ns>.<svc>` in `PROFGATE_CONFIG`:

| Path | Reads | Mutates |
|---|---|---|
| `GET /pgo` | latest entry | — |
| `PUT /pgo` | — | `Create` or `Update` |
| `DELETE /pgo` | `Get` (for the revision and the `404`/`428` distinction) | `Delete` at that revision |
| scheduler | watched cache | — |
| on-demand `POST /collections` | watched cache (for defaults not given in the body), behind the replay barrier | — |

`schedule.<ns>.<svc>.<slot>` in `PROFGATE_JOBS`:

| Path | Reads | Mutates |
|---|---|---|
| scheduler | watched cache (skip the write when a live Collection is already cached) | `Create` |
| sweeper | watched cache | `Delete` after `retainUntil` (section 7.4) |

`active.<ns>.<svc>` in `PROFGATE_JOBS`:

| Path | Reads | Mutates |
|---|---|---|
| scheduler, `POST /collections` | watched caches (`cachedLive` — Services with an active key or a nonterminal record — plus local reservations against `maxLiveCollections`; skip the write when the Service is already live) | `Create` with the new Collection's id, after `job.<id>` exists as `initializing` |
| scheduler, `POST /collections` | own `Create` result | `Delete` of nothing here; a lost `Create` deletes the creator's own `initializing` record instead |
| publisher, each scheduler tick | watched cache, then `Get` of the key (the release rule, section 7.2) | nothing |
| owner loop finish, `POST /cancel`, worker scan | own last read of the key | `Delete` at its revision after the terminal `Update` of `job.<id>` succeeds, only when the key's `id` is that Collection |
| sweeper | watched cache, then `Get` of the named job | `Delete` at its revision when the job is absent or terminal; an `initializing`, `pending`, or `running` job keeps it |

`job.<id>` in `PROFGATE_JOBS`:

| Path | Reads | Mutates |
|---|---|---|
| scheduler, `POST /collections` | watched caches (`cachedLive` plus local reservations) | `Create` (state `initializing`) before `active.<ns>.<svc>`; then `Update` `initializing` → `pending` after winning it; `Delete` at its revision when the active create loses |
| publisher, each scheduler tick | watched cache, then `Get` of `job.<id>` (the release rule, section 7.2) | nothing |
| worker claim | watched cache, then `Get`; the record's `policy` checked against this replica's `pgo.limits` | `Update` `pending`/expired-lease `running` → `running`; `Update` → `failed limit_exceeded` when the snapshot exceeds a local ceiling |
| worker scan | watched cache, then `Get` | `Update` `initializing` past `createdAt + 1m + skewMargin` → `failed`; `pending` past `claimBy` → `failed`; `running` past `deadline` → `failed`; attempts exhausted → `failed` |
| owner loop renew | own last revision | `Update` `running` → `running` (new `leaseUntil`) |
| owner loop finish | own last revision | `Update` `running` → `completed` or `failed` |
| sweeper orphan rule | `Get` | — |
| `POST /cancel` | `Get` | `Update` `pending`/`running` → `cancelled` |
| sweeper | watched cache | `Update` `completed` → `expired`; `Delete` `expired`, `failed`, `cancelled` records past `jobRetention` |
| `GET /collections/{id}`, `GET .../profile`, list | `Get` / watched cache | `Update` `completed` → `expired` when the object is missing |

`<id>-<attempt>.pprof` in `PROFGATE_ARTIFACTS`:

| Path | Reads | Mutates |
|---|---|---|
| owner loop finish | — | `Put`; `Delete` of its own object when the `completed` `Update` loses |
| `GET .../profile` | `Get` | — |
| sweeper | `List`, then `Get` of the job each object's name encodes | `Delete` at expiry; `Delete` of an object older than 10 minutes whose job, read fresh, is absent or terminal without naming it |

`probe.<instanceID>` in both KV buckets and `probe-<instanceID>` in `PROFGATE_ARTIFACTS` are written and deleted only by preflight;
the watched caches skip them and the sweeper deletes any older than 10 minutes.

---

## 6. Policy

### 6.1 Shape

```go
// Policy is the effective PGO policy of one Service.
type Policy struct {
    Enabled  bool     `json:"enabled"`
    Schedule Schedule `json:"schedule"`
    Sampling Sampling `json:"sampling"`
    Target   TargetPolicy `json:"target"`
    Artifact Artifact `json:"artifact"`
}

type Schedule struct {
    Every  Duration `json:"every"`  // pgo.limits.minEvery <= every <= pgo.limits.maxEvery
    Jitter Duration `json:"jitter"` // 0 <= jitter <= every/2
}

type Sampling struct {
    Duration      Duration `json:"duration"`      // 1s <= duration <= pgo.limits.maxDuration
    Rounds        int      `json:"rounds"`        // 1..pgo.limits.maxRounds
    RoundInterval Duration `json:"roundInterval"` // 0..10m
    Replicas      Replicas `json:"replicas"`      // "all" or 1..pgo.limits.maxTargetsPerRound
    MaxParallel   int      `json:"maxParallel"`   // 1..pgo.limits.maxParallel
}

type TargetPolicy struct {
    VersionPolicy string `json:"versionPolicy"` // "strict"; the only value
    Version       string `json:"version"`       // optional explicit pin; "" means "whatever the targets agree on"
}

type Artifact struct {
    Retention Duration `json:"retention"` // 1m <= retention <= pgo.limits.maxRetention
}
```

`Duration` is a Go duration string (`"30s"`, `"1h"`) in JSON and YAML.
`Replicas` is the string `"all"` or a JSON integer.
`profile` does not appear: it is `cpu`.

### 6.2 Layering

Operator defaults are process configuration (`pgo.defaults`, section 11).
A Service override is the value at `service.<ns>.<svc>` in `PROFGATE_CONFIG`.
The effective policy is the defaults with every field the override sets replacing the default,
block by block, one level deep:
an override `{"sampling": {"rounds": 3}}` changes `rounds` and nothing else.
A field the override sets to `null` is treated as unset.

`enabled` defaults to `false` and is the only field with no operator default:
scheduling a Service is always an explicit override.

### 6.3 Ceilings

Every effective policy is validated against `pgo.limits` (section 11) in three places:

- when an override is written (`PUT /pgo`) or an on-demand Collection is created —
  a violation is `400 limit_exceeded` naming the field and the ceiling;
- when the scheduler reads it —
  a stored override that violates a ceiling lowered after it was written makes the Service ineligible for scheduling,
  logged once per revision at warning level,
  and `GET /pgo` reports it in `violations`;
- when a worker claims or reclaims a Collection (section 8.3) —
  a stored snapshot that violates the claiming replica's ceilings ends the Collection as `failed` with reason `limit_exceeded`
  before any local capacity is reserved.

Client-supplied values never bypass a ceiling.
The Deployment's grace period must cover a Collection's `deadline` (section 8.2) at the configured ceilings,
because drain waits for in-flight work (section 12.4);
`profgate config validate` prints the required value.

### 6.4 Stored value

```json
{
  "policy": {
    "enabled": true,
    "schedule": {"every": "1h", "jitter": "5m"},
    "sampling": {"rounds": 3}
  },
  "updatedBy": "anonymous",
  "updatedAt": "2026-08-23T10:00:00Z"
}
```

`updatedBy` is the principal of the request that wrote it; `updatedAt` is set by the gateway.
KV bucket history depth is the operator's choice and not relied upon.

---

## 7. Scheduling

### 7.1 Slots

Time is divided into slots of `every`, aligned to the Unix epoch in UTC:

```text
slot = floor(now / every) × every
```

`<slot>` in every key and hash input is the slot's start as decimal Unix seconds in UTC, with no padding:
the slot beginning at 2026-08-24T00:00:00Z is `1787529600`,
and its key is `schedule.payment.payment-api.1787529600`.
This is the only encoding; a record shows the same instant as RFC 3339 in its `slot` field for display.
Every KV key Profgate writes uses only `[-/_=.a-zA-Z0-9]`, the set NATS accepts;
namespace and Service names are DNS-1123 labels and the slot is digits, so every key qualifies by construction.

A slot fires at `slot + offset`, where

```text
offset = fnv1a64("<ns>/<svc>/<slotUnixSeconds>") mod jitter   (0 when jitter is 0)
```

with the same decimal encoding in the hash input, for example `payment/payment-api/1787529600`.
Two gateway versions that follow this contract contend on one key for one slot;
an encoding left to the implementation could let two versions fire the same slot twice.

Every replica computes the same fire time from the same inputs,
so jitter spreads Services across the interval without spreading replicas apart.

A replica considers only the slot containing `now`.
Missed slots are never caught up:
a gateway that returns after a day creates at most the Collection for the current slot,
and only if its fire time has passed.

### 7.2 Algorithm

Every 10 seconds, on every replica:

```text
gen := c.Generation(); if !c.Synced(gen): return
jobs := c.View(gen).Jobs                                        // ErrUnavailable on a moved generation ends the tick
for each (ns, svc) with an override in the watched PROFGATE_CONFIG cache:
    policy := effective(defaults, override)
    if !policy.Enabled or policy violates a ceiling: continue
    slot := floor(now / every) × every
    if now < slot + offset(ns, svc, slot): continue
    if the watched caches show (ns, svc) as live:               // an active key or a nonterminal job record
        record schedule_slots_total{result="busy"}; continue     // saves a write; not the decision
    if !publisher.Reserve():                                    // cachedLive + reserved >= maxLiveCollections
        record schedule_slots_total{result="capacity"}; continue
    err := jobs.Create("schedule.<ns>.<svc>.<slot>",
                       {"retainUntil": slot + every + 24h})
    if err is ErrKeyExists: publisher.Release(); record schedule_slots_total{result="lost"}; continue
    if err is ErrUnavailable: publisher.Release(); log; continue    // no active key can exist yet
    publish(ns, svc, origin=schedule, claimBy = now + every)   // section "Publishing a Collection"
```

**Publishing a Collection.**
The scheduler and `POST /collections` publish a record through one per-replica `publisher`,
which holds the reservation counter of the live-Collection ceiling (below)
and performs the same three writes for both:

```text
publish(ns, svc, origin, claimBy):                          // caller holds one reservation
  id := newID()
  rev, err := jobs.Create("job.<id>", record with state = initializing, origin, configRevision,
                          the policy snapshot, claimBy)
  if err is ErrUnavailable: publisher.Track(ns, svc, id); log or 503; return   // indeterminate; resolved later
  if err != nil: publisher.Release(); log or 503; return    // nothing exists yet
  err = jobs.Create("active.<ns>.<svc>", {"id": id, "createdAt": now})
  if err is ErrKeyExists:
      derr := jobs.Delete("job.<id>", rev)                  // own record, own revision
      if derr is ErrUnavailable: publisher.Track(ns, svc, id)   // indeterminate: the record may still be initializing
      else: publisher.Release()                             // deleted, or already gone
      record busy / answer 429 collection_in_progress; return
  publisher.Track(ns, svc, id)                              // released only by the release rule, section "The live-Collection ceiling"
  if err is ErrUnavailable: log or 503; return              // indeterminate: the key may exist; the scan fails the record later
  record.state = pending
  _, err = jobs.Update("job.<id>", record, rev)
  if err != nil: log or 503; return                         // the key exists; the release rule resolves the reservation
  record won / answer 202
```

An `initializing` record is never claimable; the worker ignores it.
The order is what closes the publication race:
the active key never exists without its job record already existing,
so a sweeper that reads the job named by an active key finds `initializing` or later, never nothing,
and keeps the key.
A creator that dies between the writes leaves either an `initializing` record alone,
or an `initializing` record plus an active key naming it;
the worker scan fails any `initializing` record on its first pass after `createdAt + 1m + skewMargin`
once that worker's watched cache holds the record, with reason `not_published`
(no bound holds while NATS is unavailable or a watch is replaying),
and then runs `releaseActive` for it, which deletes the active key only if it names that id.
A creator whose active create loses deletes its own `initializing` record at the revision it holds;
if that delete returns `ErrUnavailable` the reservation stays tracked
and the scan fails the record the same way if it still exists.
Nothing therefore depends on the sweeper telling "not created yet" from "failed to create".

Two creates decide, and the cache decides nothing:
the slot key is one Collection per slot,
the `active.<ns>.<svc>` key is one live Collection per Service, whatever its origin.
The cached check in front of the active create only spares a write that would lose;
a replica whose cache lags simply loses the create instead.

The slot key does not contain the configuration revision,
and the active key does not contain the slot.
A policy changed mid-slot therefore cannot fire the slot twice,
and a change to `every` cannot run two Collections at once either:
a replica still on the old `every` and one on the new compute different slot keys,
and both slot creates can win,
but only one of them wins the active key;
the other records `busy` and its slot key is consumed.
The Collection records the revision it was created from (section 8.2),
and the next slot uses whatever is current then.

**The live-Collection ceiling.**
The active key bounds one Service to one live Collection;
`pgo.limits.maxLiveCollections` bounds the cluster.
Each replica runs one `publisher`, shared by its scheduler and its `POST /collections` handler,
that keeps a local reservation counter `reserved`.
`Reserve()` computes `cachedLive + reserved`, where
`cachedLive` is the number of Services that have, in this replica's watched caches,
an active key or a nonterminal job record (`initializing`, `pending`, or `running`),
and `reserved` counts this replica's publications for which neither cache has delivered anything yet.
A Service the caches already show as live is refused without a reservation
(the scheduler records `busy`, the API answers `429 collection_in_progress`).
At or above the ceiling `Reserve()` refuses — the scheduler records `capacity` and the API answers `429 capacity_exhausted`, writing nothing —
otherwise it increments `reserved` and the publication proceeds.

**The release rule.**
A reservation for publication `(ns, svc, id)` is released, checked on every scheduler tick, as soon as either:

- a watched cache delivers this publication's `job.<id>` in any state,
  or its `active.<ns>.<svc>` key holding `id` —
  from then on the Service is counted by `cachedLive`; or
- authoritative reads show `job.<id>` absent or terminal
  **and** `active.<ns>.<svc>` absent or holding a different id —
  nothing of this publication exists to count.

`ErrUnavailable` on either read leaves the reservation held.
A publication that fails before any write that could have created state
(a refused reservation, a job create that returned a definite error,
a lost active create whose own record it deleted with a definite result)
releases immediately, because nothing can exist.
`ErrUnavailable` on either publication `Create`, or on the creator's delete of its own `initializing` record,
is indeterminate —
the write may have committed and lost its acknowledgement —
so the reservation stays tracked and the creator does not retry the write;
it resolves by one of the two observations above.
Nothing else releases a reservation, and in particular not the passing of `claimBy`.
The publisher keeps its unresolved reservations in a list;
on every scheduler tick it evaluates each one, cache first, then the fresh reads,
and leaves the reservation in place while neither observation has been made.

The invariant the rule keeps:
a replica publishes nothing before its watches have completed their initial replay (section 5.1),
so after any restart every nonterminal record and every active key in the bucket is in `cachedLive` before the first publication;
a record or key written after that is counted by `cachedLive` once delivered
and, in the window before delivery, by the reservation of the replica that wrote it —
and if that replica dies inside the window, its replacement replays the record before publishing;
and a replica never publishes for a Service it counts as live.
Each replica therefore contributes at most `maxLiveCollections` Services that nobody has yet seen,
live Services never exceed `replicas × maxLiveCollections`,
and records that are not terminal never exceed `replicas × maxLiveCollections`
plus at most one further record per Service per replica left by an indeterminate publication —
at most `2 × replicas × maxLiveCollections` —
under any combination of committed and uncommitted creates, a frozen watch, and a restarted publisher,
because a restarted publisher is behind the barrier until the record its predecessor left has replayed.
Stale `initializing` records are drained by the scan (`not_published`) and are counted until then.
The counter survives scheduler ticks and covers API requests alike; there is no per-tick count.
`claimBy` bounds how long a `pending` record waits and the `not_published` rule bounds an `initializing` one,
so a backlog that forms when creation outpaces workers drains into `not_claimed` failures instead of growing.
The arrival rate is bounded separately:
the scheduler creates at most one record per Service per `every`,
and on-demand creation is rate-limited per replica (section 7.3),
so creations per minute are at most `replicas × onDemandPerMinute` plus the scheduled ones.
The worker scan of section 8.3 visits at most `2 × replicas × maxLiveCollections` records per pass,
and the sweeper's `Get` per active key is at most `replicas × maxLiveCollections` per pass.

A scheduler whose publication fails after winning the slot logs the error and moves on;
the slot is consumed and nothing runs for it.
This is a deliberate loss rather than a retry loop:
the next slot is at most `every` away, and a second create cannot know whether the first half-succeeded;
whatever the first left behind, the scan and the sweeper clear it as described above.

### 7.3 On-demand Collections

`POST /collections` publishes a record exactly as the scheduler does (section 7.2), with `origin: api` and `claimBy = now + 1h`.
Before any write, the handler takes a token from a per-replica bucket of `pgo.limits.onDemandPerMinute` tokens
refilled at that rate;
an empty bucket answers `429 rate_limited` and writes nothing,
so a caller with `pgo.collect` across many Services cannot create records faster than `replicas × onDemandPerMinute` per minute.
It takes no slot and is unaffected by `enabled`;
the scheduler and the API are two creators of the same record shape through the same active key.
It answers `429 capacity_exhausted` when the publisher's `Reserve()` refuses (section 7.2), writing nothing,
and `429 collection_in_progress` when the active create loses,
whether the live Collection came from the scheduler or from another request;
the watched cache is consulted first only to answer without a write when the answer is already known.
Concurrent requests for one Service therefore yield exactly one `202`.

### 7.4 Slot retention

Every slot key carries `retainUntil = slot + every + 24h`, computed from the `every` that created it,
and the sweeper deletes the key only after `retainUntil` has passed.
A slot can be attempted only while `now` lies inside it,
and `every` is at most `pgo.limits.maxEvery` (24 hours),
so by the time a key is deleted its slot ended at least a day earlier
and no replica, whatever its policy now says, can attempt that slot again.
The value, not the current policy, decides the key's lifetime:
lowering `every` after the fact cannot shorten the retention of a key created under a longer one.

---

## 8. Collections

### 8.1 Identifier

20 characters from the Crockford base32 alphabet (`0-9a-hjkmnp-tv-z`, lowercase), no padding,
encoding 100 bits from `crypto/rand`.
Opaque; not time-ordered; `createdAt` carries the time.
Route matching accepts exactly this grammar; anything else is `404 route_unknown`.

### 8.2 Record

Key `job.<id>` in `PROFGATE_JOBS`:

```json
{
  "id": "7h2k9m4p6r8t0v1w3x5y",
  "namespace": "payment",
  "service": "payment-api",
  "origin": "schedule",
  "slot": "2026-08-23T12:00:00Z",
  "configRevision": 42,
  "policy": {
    "enabled": true,
    "schedule": {"every": "1h", "jitter": "5m"},
    "sampling": {"duration": "30s", "rounds": 2, "roundInterval": "30s", "replicas": "all", "maxParallel": 4},
    "target": {"versionPolicy": "strict", "version": ""},
    "artifact": {"retention": "2h"}
  },
  "state": "running",
  "attempt": 1,
  "owner": {"instance": "profgate-7f88fdf79-xabcd/q2w3e4r5", "pod": "profgate-7f88fdf79-xabcd"},
  "claimBy": "2026-08-23T13:00:00Z",
  "leaseUntil": "2026-08-23T12:06:12Z",
  "deadline": "2026-08-23T12:36:43Z",
  "reason": "",
  "resolvedVersion": "1.42.3",
  "progress": {"round": 1, "rounds": 2, "samplesOK": 5, "samplesFailed": 0},
  "manifest": null,
  "artifact": null,
  "createdBy": "anonymous",
  "createdAt": "2026-08-23T12:03:12Z",
  "startedAt": "2026-08-23T12:03:13Z",
  "finishedAt": null,
  "expiresAt": null
}
```

| Field | Meaning |
|---|---|
| `origin` | `schedule` or `api` |
| `slot` | the slot that created it, as RFC 3339 for display; the key uses Unix seconds (section 7.1); absent for `api` |
| `configRevision` | revision of `service.<ns>.<svc>` at creation; `0` when no override existed |
| `policy` | the effective policy snapshot the Collection runs with; never re-read |
| `state` | `initializing`, `pending`, `running`, `completed`, `failed`, `cancelled`, `expired`; `initializing` lasts from the record's `Create` until its creator wins the active key and is never claimable |
| `attempt` | claims so far; `0` while `pending` |
| `owner` | the claiming replica: `instance` is the Pod name plus a per-process random suffix, `pod` the Pod name |
| `claimBy` | a `pending` record not claimed by this time is failed `not_claimed`; `createdAt + every` for `schedule`, `createdAt + 1h` for `api` |
| `leaseUntil` | the owner's claim is valid until this time |
| `deadline` | set at first claim from the policy snapshot and the ceilings, never from the live target count: `startedAt + rounds × batches × (duration + 30s + admissionWait) + (rounds − 1) × roundInterval + 60s`, where `batches = ceil(min(replicas, maxTargetsPerRound) / maxParallel)` with `all` read as `maxTargetsPerRound`, and `admissionWait = duration + roundInterval` (section 8.5) |
| `reason` | why a `failed` or `cancelled` Collection ended (section 8.6) |
| `resolvedVersion` | the version the first round settled on |
| `progress` | the owner's last renewal snapshot; informational |
| `manifest` | section 9 |
| `artifact` | `{"object": "<id>-<attempt>.pprof", "bytes": 123456}`; set only by the `completed` update, so it names exactly the object that update committed |
| `expiresAt` | `finishedAt + retention` for `completed` |

The record is the durable source of truth.
Gateway memory is a watched cache of it.
A record is at most `rounds × maxTargetsPerRound` manifest samples plus fixed fields.
Configuration validation requires `maxRounds × maxTargetsPerRound ≤ 256`.
A manifest sample at every field's maximum is:
`pod` 253, `node` 253, `podUID` 36, `startedAt` 35 (RFC 3339 with offset and nanoseconds),
`result` and `reason` 32 each, `bytes` 20, and JSON keys, quotes, and punctuation at most 120 —
781 bytes, rounded to 800.
256 samples are then at most 200 KiB;
the fixed fields, the policy snapshot, and the manifest's own scalars are under 8 KiB with every name at its limit;
the whole record stays under 210 KiB.
`maxRecordBytes` is a fixed 512 KiB, leaving that arithmetic a margin of more than two:
the owner loop serializes the record before every `Update`
and fails the Collection with `record_too_large` instead of sending a value the 1 MiB default NATS message limit could reject,
which leaves a reader a terminal record rather than a wedged one.
The `record_too_large` terminal record omits `manifest.samples` and keeps the manifest's counts,
so it is itself small and its `Update` cannot fail for the same reason.

### 8.3 Claim

The worker on every replica watches `job.*`.
A record is claimable when

```text
state == pending
or (state == running and leaseUntil + skewMargin < now)
```

and the replica has fewer than `pgo.limits.maxActiveCollections` Collections of its own in flight.
`skewMargin` is a fixed 5 seconds (section 8.4).

A claim attempt runs in two situations, both only once the replay barrier has cleared (section 5.1):
when the watch delivers a record that is claimable as delivered,
and on the **scan** that every worker runs every `leaseTTL / 2` over its watched cache.
The scan exists because time passing writes no KV revision:
after an owner dies, the last thing any watch delivered was a valid lease,
and nothing else would ever revisit the record.

```text
scan, every leaseTTL / 2:
  gen := c.Generation(); if !c.Synced(gen): return
  jobs := c.View(gen).Jobs                                // one view for the whole scan
  for each cached record with state in (initializing, pending, running):
      entry := jobs.Get("job.<id>")                      // latest, with its revision
      switch:
      case entry.state == initializing and entry.createdAt + 1m + skewMargin < now:
          terminate(entry, failed, not_published)
      case entry.state == pending and entry.claimBy + skewMargin < now:
          terminate(entry, failed, not_claimed)
      case entry.state == running and entry.deadline + skewMargin < now:
          terminate(entry, failed, deadline_exceeded)
      case claimable(entry):
          claim(entry)

terminate(entry, state, reason):
  entry.state = state; entry.reason = reason; entry.finishedAt = now
  _, err := jobs.Update(key, entry, entry.Revision)     // loser: ErrRevisionMismatch, done
  if err == nil: releaseActive(entry)

releaseActive(entry):
  active := jobs.Get("active.<ns>.<svc>")               // ErrKeyNotFound or ErrUnavailable: done; the sweeper covers it
  if active.id == entry.id: jobs.Delete("active.<ns>.<svc>", active.Revision)   // loser: done

claim(entry):
  if entry.policy violates any ceiling in this replica's pgo.limits:
      terminate(entry, failed, limit_exceeded)         // before any local capacity is reserved
      return
  if !reserveLocalSlot(): return                       // one of maxActiveCollections on this replica
  if entry.attempt >= pgo.maxAttempts:
      releaseLocalSlot()
      terminate(entry, failed, attempts_exhausted)
      return
  entry.state = running
  entry.attempt += 1
  entry.owner = self
  entry.leaseUntil = now + pgo.leaseTTL
  if entry.startedAt == nil: entry.startedAt = now; entry.deadline = per section 8.2
  rev, err := jobs.Update(key, entry, entry.Revision)
  if err != nil:                                       // ErrRevisionMismatch: someone else moved it;
      releaseLocalSlot()                               // ErrUnavailable: the outcome is unknown
      return                                           // either way nothing is profiled; the watch or the scan shows what happened
  run(entry, rev)                                      // only with a revision the server returned
```

The `Get` before the `Update` is what makes the revision the replica compares against most recent;
it is not a pre-check, and the `Update` alone decides.
The ceiling check comes first, on every claim and reclaim alike:
a record carries the policy snapshot it was created under,
and the ceilings that validated it then are not the ceilings of whichever replica claims it now —
a rolling configuration change or a restart with lower limits can leave `pending` records
whose `maxParallel`, `duration`, `rounds`, or `maxTargetsPerRound` exceed what this replica's admission share,
memory figure, and deadline arithmetic were sized for.
Such a record is failed `limit_exceeded` by the first worker that meets it and its active key released;
it never reserves a local slot and never samples,
so the inequality of section 8.5 holds for the ceilings actually in force, not the ones at creation.
Work starts only on a successful `Update` with the revision it returned.
An `ErrUnavailable` claim is indeterminate — the server may have committed it and lost the acknowledgement —
and a worker that ran on it would either profile without owning the record
or own it without knowing the revision every later conditional write needs;
so it releases its local slot and profiles nothing.
If the claim did commit, the record carries a lease this replica will never renew,
and another scan reclaims it after `leaseTTL + skewMargin` as it would after a crash.
`releaseActive` runs after every terminal `Update` that succeeds, here and in sections 8.6 and 8.7:
it frees the Service for the next Collection the moment this one ends,
and deletes only a key that names this Collection, so it can never release a successor's claim.
A release that fails for any reason is not retried; the sweeper releases it on its next pass.
A `running` record past its `deadline` is failed rather than reclaimed:
an owner that has not finished inside a deadline computed from the ceilings is wedged,
and its own next renewal observes the mismatch and stops.

### 8.4 The owner loop

Every write to a Collection's record after the claim is made by one goroutine, the **owner loop**,
which holds the current revision `rev` and the **committed lease** `committedLeaseUntil`,
the value the last successful `Update` stored.
The owner loop does nothing else:
it renews on a timer, receives progress from the work, issues the final update, and releases the active key.
Everything that can block for long — sampling, parsing, merging, compaction, serialization, and the `Put` —
runs in one separate **work goroutine** under a context the owner loop owns.
The work goroutine reports progress and hands over the finished object name over a channel;
it never touches the record, and it never calls KV.
Renewal and finish are therefore serialized by construction,
the owner can never lose a conditional update to its own newer revision,
and a long merge can never hold the renewal timer hostage.

**Clock-skew assumption.**
Gateway Pods run on NTP-disciplined nodes whose clocks agree within `skewMargin = 5s`,
and the NATS server's clock, which stamps `ModTime`, is within the same bound.
Every comparison between a timestamp one replica wrote and the time another replica reads —
lease expiry, `claimBy`, `deadline`, `expiresAt`, orphan age —
carries the margin in the direction that makes the reader more conservative:
a claimer waits `skewMargin` longer, an owner gives up `skewMargin` earlier.
Nodes whose clocks disagree by more than that can reclaim a live Collection early;
the result is a duplicated attempt, never a lost or corrupted artifact (section 8.6).

The owner loop takes its view once, at claim time, from the generation the claiming scan ran under;
a disconnect during the Collection makes every later renewal `ErrUnavailable` on that view,
which is the abort path below.
Every `leaseTTL / 3` the owner loop renews:

```text
callDeadline := min(5s, committedLeaseUntil - now - skewMargin)
if callDeadline <= 0: abort(lease_lost)
proposed := copy of entry
proposed.leaseUntil = now + leaseTTL
proposed.progress = current
newRev, err := jobs.Update(key, proposed, rev)   // with that call deadline
if err == nil:
    rev = newRev; entry = proposed; committedLeaseUntil = proposed.leaseUntil
    reset the work context's cutoff to committedLeaseUntil - skewMargin
```

The proposed lease lives in a local copy until the `Update` succeeds;
a failed renewal leaves `committedLeaseUntil` exactly where the last successful write put it,
so the cutoff the owner enforces is always a lease another replica can also see.
The call deadline shrinks as the committed lease runs out,
so a renewal blocked by a slow NATS can never return success after the lease it was renewing has lapsed.
`ErrRevisionMismatch` means the record changed under the owner —
cancelled by the API, or reclaimed after a stall.
`ErrUnavailable` changes nothing; the next tick tries again with what remains.
Either way, the moment `now > committedLeaseUntil - skewMargin` without a successful renewal,
the owner aborts: it cancels the work context, sets its cancellation flag, and writes nothing.
On `ErrRevisionMismatch` it cancels the work context at once, without waiting for the cutoff.
An owner that cannot prove its lease is not an owner,
and it commits nothing after the moment another replica may lawfully claim.

**What the cutoff guarantees, and what it does not.**
Guaranteed: the owner issues no final `Update` after the cutoff.
The final update (section 8.6) is gated on the committed lease, the deadline, and the cancellation flag,
all read by the one goroutine that can issue it,
so a late result from the work goroutine is rejected whatever state the work was in.
Not guaranteed: that the work has stopped.
Sampling and the store calls observe the context, but
`profile.Merge`, `Compact`, and `Write` take no context and run to completion once entered;
they, and a `Put` already in flight, continue until they return.
The Collection deadline is the bound on how long shutdown waits for the work goroutine (section 12.4),
not a proof that such a call has returned by then.
Until it exits, the local `maxActiveCollections` reservation and the memory it stands for stay held,
so a replacement Collection on this replica waits for the slot.
The active key is released by whoever performed the terminal `Update`, regardless,
so another replica may start the Service's next Collection while this one's work drains;
that is within the memory figure of section 3.4, which is per replica.

Cancellation (section 8.7) reaches the owner the same way:
the cancel handler's conditional update advances the revision,
the owner's next renewal or final update fails with `ErrRevisionMismatch`,
and the owner re-reads the record once to log the reason before it stops.
The owner never re-reads during a successful renewal and the work goroutine never touches KV,
so the worst-case latency from cancellation to the owner stopping is one renewal interval, `leaseTTL / 3`.

### 8.5 Rounds

```text
for round in 0..rounds-1:
    targets := discovery.Targets(ns, svc)          // gateway eligibility rules, from the cache
    targets  = filter(version != "")
    if policy.target.version != "": targets = filter(version == pin)
    if round == 0:
        versions := distinct(targets.version)
        if len(versions) == 0: fail(version_missing)
        if len(versions) >  1: fail(version_conflict)
        resolvedVersion = versions[0]; renew
    targets = filter(version == resolvedVersion)
    if len(targets) == 0: fail(no_targets)
    shuffle(len(targets), swap)                     // the injected Shuffle; production: math/rand/v2 seeded from crypto/rand
    want := replicas == all ? maxTargetsPerRound : replicas
    if len(targets) > want: targets = targets[:want]; manifest.truncated = true
    fan out over targets with maxParallel sampling goroutines, each:
        release, err := gate.Acquire(ctx)           // one of limits.maxConcurrentProfiles, waiting up to duration + roundInterval
        if err != nil: record the sample as slot_timeout; return
        defer release()                             // runs after the body is fully consumed and closed, on every path
        confirm(target)                             // gateway confirmation; failure records the sample as failed
        fetch /debug/pprof/profile?seconds=duration through the proxy transport
        decode(body) -> *profile.Profile            // section "Decoding a sample" below; closes the body
        send the decoded profile to the work goroutine
    work goroutine, one sample at a time:
        if merged == nil:
            merged = sample                             // the first success is the running profile; Merge is never given nil
        else:
            next, err := profile.Merge([]*profile.Profile{merged, sample})
            if err != nil: record the sample as incompatible_profile; merged is unchanged; continue
            merged = next
        if serializedSize(merged) > maxMergedBytes: fail(merged_too_large)
        record the sample as ok
    if samplesOK(round) == 0: fail(no_samples)
    if round < rounds-1: sleep roundInterval
```

`version_missing` covers two cases with one reason:
no Pod the round resolves carries a version label,
and every Pod carries one but none matches `target.version` when it is pinned —
the pin filter runs before the distinct-version check, so both leave zero versions to resolve from.

**Decoding a sample.**
The body is read through `io.LimitReader(maxSampleBytes + 1)` into memory;
reaching the extra byte is `sample_too_large`.
When the bytes begin with the gzip magic (`0x1f 0x8b`),
they are decompressed through a second `io.LimitReader(maxSampleBytes + 1)` with the same rule, so a small body cannot expand past the limit.
If the decompressed bytes still begin with the gzip magic the sample fails `sample_malformed`:
nested gzip is never decoded, because `profile.ParseData` would otherwise expand it without a bound.
Only uncompressed bytes reach `ParseData`, which decodes them without another copy.
`profile.Parse` is not used: it reads its input into a second buffer and expands gzip without a bound.
A decode error is `parse_failed`.
Per in-flight sample the input side is bounded exactly — compressed and decompressed bytes, each at most `maxSampleBytes` —
and the decoded `*profile.Profile` is not:
its heap size is slices, strings, and pointed-to objects whose total the encoded length does not bound.
Section 3.4 budgets it with an engineering factor, stated as an estimate and not a proof.
The decoder is a function on the worker, replaced in tests, so the input bound is testable without a real pprof endpoint.

**Merging.**
`profile.Merge` rejects profiles whose sample types or period types differ,
which the version label does not rule out (a binary can serve a different profile shape under the same label);
such a sample fails `incompatible_profile` and the running profile is untouched.
The first successful sample becomes the running profile without a `Merge` call:
`profile.Merge` reads its first argument's header and sample types before anything else,
so it is never handed a nil running profile.

**Shuffling.**
The worker takes a `Shuffle func(n int, swap func(i, j int))` at construction.
Production passes a `math/rand/v2` generator seeded from `crypto/rand`, one per worker;
tests pass a fixed sequence, so a coverage assertion is deterministic and a failure reproducible.

Targets are re-resolved every round, so a Pod that leaves during a rollout drops out
and a Pod that arrives with the same version joins.
A Pod that arrives with a different version is filtered out by `resolvedVersion`;
when every Pod has rolled, the round finds nothing and the Collection fails `no_targets`.
Two versions cannot share an artifact by construction.

Each sample uses the gateway's interactive machinery unchanged:
the same `Confirm`, the same `http.Transport`, the same header deadline and budget rules with `seconds = duration`.
The only difference is the destination of the body:
an in-memory writer that stops at `maxSampleBytes` and records `sample_too_large`.

Sample failure reasons: `target_changed`, `discovery_unavailable`, `upstream_unreachable`, `upstream_timeout`,
`upstream_<status>`, `sample_too_large`, `sample_malformed`, `parse_failed`, `incompatible_profile`, `slot_timeout`.
A failed sample is recorded and skipped; only a round with zero successes fails the Collection.

`replicas: all` means every eligible Pod up to `pgo.limits.maxTargetsPerRound`;
a Service with more Pods than that is sampled from a different shuffled subset each round,
and the manifest records `truncated: true` so a reader knows the artifact is a sample of the fleet.
The cap is what bounds the manifest, the running merge, the deadline, and the memory figure of section 3.4.
The running profile is checked after every merge against `pgo.limits.maxMergedBytes`
(the serialized size, which is also the size of the object that would be stored);
a Collection that outgrows it fails `merged_too_large` rather than exhausting the gateway.

**Admission is one gate, shared with interactive requests.**
`internal/admit` holds it:

```go
type Gate struct{ /* unexported */ }

func New(capacity int) *Gate
// TryAcquire takes a slot without waiting; interactive requests use it (429 when ok is false).
func (g *Gate) TryAcquire() (release func(), ok bool)
// Acquire waits for a slot until ctx ends; Collection samples use it.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error)
```

`cmd/profgate` constructs exactly one `Gate` with `limits.maxConcurrentProfiles`
and injects it into both `httpapi` and `pgo`;
`httpapi.New` no longer creates its own channel (section 4).
A Collection sample waits for a slot for at most `duration + roundInterval`
(`slot_timeout` when it does not get one), where an interactive request fails fast.
Configuration validation requires
`pgo.limits.maxParallel × pgo.limits.maxActiveCollections < limits.maxConcurrentProfiles`,
using the ceiling and not the policy default, because a valid override may run at the ceiling;
so Collections can never hold every slot and interactive profiling always has at least one.
With the published defaults that is `4 × 2 < 16`.
The guarantee holds only because there is one gate and every slot a sampler takes is given back:
each sampler keeps the `release` that `Acquire` returned and calls it once the upstream body is consumed and closed,
on the success path and on every failure path alike.
Two semaphores with the same arithmetic would satisfy every local test and break it;
so would one leaked release per sample, which would block interactive requests after enough rounds.

The gate bounds how many profiles run at once, not which Pods they hit.
Within one Collection a Pod is sampled at most once per round, because a round's target list is deduplicated by Pod UID;
two Collections for different Services that select the same Pod, on one gateway or on two,
and an interactive request against it, may all profile it at the same time.
There is no per-Pod exclusion, and nothing here claims one.

### 8.6 Finish

After the last round, in the work goroutine:

```text
merged = merged.Compact()
var buf bytes.Buffer
if err := merged.Write(&buf); err != nil: fail(serialize_failed)
object := "<id>-<attempt>.pprof"
if err := artifacts.Put(ctx, object, &buf); err != nil: fail(artifact_store_failed)
hand (object, buf.Len(), manifest) to the owner loop
```

Then in the owner loop:

```text
if now >= committedLeaseUntil - skewMargin or now >= entry.deadline - skewMargin:
    artifacts.Delete(object); abort(lease_lost)   // its own object only; deadline was written by the first claimer
proposed := copy of entry
proposed.state = completed; proposed.manifest = m; proposed.artifact = {object, bytes}
proposed.finishedAt = now; proposed.expiresAt = now + retention
if len(json(proposed)) > maxRecordBytes:                          // after deleting the object;
    fail(record_too_large)                                        // the failed record drops manifest.samples
_, err := jobs.Update(key, proposed, rev)
if err == nil: releaseActive(entry)
if err is ErrRevisionMismatch:
    artifacts.Delete(object)                      // its own object only; best effort, the sweeper also covers it
```

The lease and deadline are re-checked at the last moment, against the committed values,
because the `Put` may have taken long enough for a scan elsewhere to have lawfully reclaimed or failed the record;
an owner whose lease has lapsed by then has nothing to commit and removes what it wrote.

Failure writes `state = failed`, `reason`, `finishedAt` with the same conditional update,
serialized and size-checked the same way, followed by `releaseActive` on success.
Reasons: `version_missing`, `version_conflict`, `no_targets`, `no_samples`, `deadline_exceeded`,
`attempts_exhausted`, `artifact_store_failed`, `merged_too_large`, `serialize_failed`, `record_too_large`, `not_claimed`, `not_published`, `limit_exceeded`.

**Artifacts are fenced by attempt.**
The object name carries the attempt number,
the record's `artifact.object` is set only by the `completed` update that wins,
and a loser deletes only the object it wrote.
A stale owner that writes after a reclaimed attempt has completed therefore writes a different name,
cannot overwrite the committed bytes,
and on losing its update deletes its own object and nothing else;
whatever order the two attempts' puts and updates interleave in,
the committed record names an object that exists and that only the winning attempt wrote.
The object is written before the record flips to `completed`,
so a successful `completed` update names an object the winning attempt stored,
and an object no `completed` record names is garbage the sweeper removes.
That is a guarantee about publication, not about the record's whole life:
expiry (section 8.9), loss outside Profgate, or a sweeper that deleted the object and then crashed before flipping the record
can leave a `completed` record whose object is gone.
The download path and the sweeper both treat that as `expired`,
and `Objects.Delete` of an absent name is success, so whichever of them runs next finishes the transition.

### 8.7 Cancel

`POST /collections/{id}/cancel`:

```text
for attempt in 1..5:
    entry := jobs.Get(key)
    if entry.state == initializing: 409 collection_initializing      // nonterminal; retry after a second
    if entry.state not in (pending, running): 409 collection_terminal  // read as terminal
    entry.state = cancelled; entry.reason = cancelled_by_api; entry.finishedAt = now
    _, err := jobs.Update(key, entry, entry.Revision)
    if err == nil: releaseActive(entry); 200 with entry
    if err is ErrRevisionMismatch: continue        // lost to a renewal or a claim; read again
    503 pgo_unavailable
503 pgo_unavailable, audit code cas_contended
```

Losing to a renewal is the common race: the Collection is still live, so the loop reads again and retries.
`409 collection_terminal` is answered only from a read that shows a terminal state, never inferred from a lost update;
`409 collection_initializing` is the one nonterminal `409`.
Five losses in a row means the record is moving faster than the handler can read it,
which does not happen at the renewal rate of section 8.4; the bound exists so the handler cannot spin.

The owner learns of it through `ErrRevisionMismatch` on its next renewal or final update (section 8.4),
at most `leaseTTL / 3` later, and commits no artifact reference;
an `artifacts.Put` already in flight may leave an orphan object that the sweeper removes (section 8.9).
A cancelled Collection never names an artifact.

### 8.8 Recovery

```text
Profgate A owns C1, round 2 of 3
Profgate A dies
leaseTTL + skewMargin passes; no KV write happens
Profgate B's scan revisits the running record, reads it fresh, sees leaseUntil + skewMargin < now
Profgate B claims: attempt 2, from round 0, object name <id>-2.pprof
```

At-least-once, from the beginning, because the partial merge lived in A's memory.
`pgo.maxAttempts` bounds it;
the claim that would exceed it marks the record `failed` with `attempts_exhausted` instead.
Because Collections are minutes long, the repeated profiling is small.

A `running` record past its `deadline` and a `pending` record past its `claimBy` are failed by the scan (section 8.3),
not reclaimed.

### 8.9 Sweeper

Every 60 seconds, on every replica, behind the barrier and through one `View(gen)` for the pass,
over the watched `job.*` and `schedule.*` caches and one `artifacts.List`:

| Condition | Action | Primitive |
|---|---|---|
| `completed` and `expiresAt + skewMargin < now` | `artifacts.Delete(artifact.object)` (absent is success), then `state = expired` | `Update` at the cached revision |
| `completed` and `artifacts.Get(artifact.object)` is `ErrObjectNotFound` | `state = expired` | `Update` at the cached revision |
| `expired`, `failed`, or `cancelled` and `finishedAt + pgo.jobRetention + skewMargin < now` | delete the record | `Delete` at the cached revision |
| slot key with `retainUntil + skewMargin < now` | delete the key | `Delete` at its revision |
| object `<id>-<attempt>.pprof` not named by any `completed` record in the cache, `ModTime + 10m + skewMargin < now` | `jobs.Get("job.<id>")`; delete the object only when the record is absent, or terminal with `artifact.object` naming something else | `Get`, then `artifacts.Delete` |
| `active.<ns>.<svc>` key | `jobs.Get` of the job it names; delete the key when that job is absent or terminal; `initializing`, `pending`, and `running` keep it | `Get`, then `Delete` at the key's revision |
| `probe.*` key whose `Entry.Created`, or `probe-*` object whose `ModTime`, plus `10m + skewMargin` is past | delete, no lookup | `Delete` |

A `completed` record is never deleted directly:
it becomes `expired` first, which deletes its object, and is deleted `jobRetention` after it finished.
Configuration validation requires `pgo.jobRetention ≥ pgo.limits.maxRetention + 1h`,
so a record always outlives its artifact:
an object is unreferenced only when its attempt lost or its record has been deleted,
and a record is deleted only after its object.
The 10-minute age lets a slow `Put` finish before its `completed` update names it.

**The cache is a candidate filter, never the authority for a delete.**
A watched cache has no freshness bound,
so the scan and the sweeper act on a record only once a watch has delivered it,
and no cleanup latency is bounded while NATS is unavailable or a watch is replaying:
after a NATS outage the connection can come back
and serve a `List` before the job watch has replayed a `completed` record,
and a sweeper that trusted its cache would then delete a live artifact.
So every orphan candidate costs one fresh `Get` of the job its name encodes,
the object survives whenever that record names it,
and `ErrUnavailable` on the `Get` keeps the object.
The same rule governs active keys:
a key is released only after a fresh read shows its Collection terminal or gone.
Because the record is created before the key (section 7.2), a fresh read never finds "gone" for a key still being published;
a key left by a creator that died is freed once the scan has failed its `initializing` record.

Losers observe `ErrRevisionMismatch` and do nothing; the watch delivers the winner's write.
The sweeper never touches `initializing`, `pending`, or `running` records; the worker's scan does.

**Cost.**
Every condition is evaluated against the watched caches and one `List` of the artifact bucket;
the NATS calls a sweep makes are that `List`,
one `Get` per orphan candidate and per active key,
and one `Delete` per matching key or object.
Orphan candidates are objects whose attempt lost or whose record is gone, a handful at most;
active keys are at most `replicas × maxLiveCollections` (section 7.2).
Per replica per minute that is at most one list, one read per Service with a live Collection,
and the number of records, slot keys, and objects that crossed their threshold in that minute,
which under steady load is the number of Collections finishing per minute,
not the number stored.
The cost grows with replica count only in the lists and the per-Service reads, once per replica per minute.

---

## 9. Manifest

Stored inside the record as `manifest` once the Collection completes:

```json
{
  "collection": "7h2k9m4p6r8t0v1w3x5y",
  "namespace": "payment",
  "service": "payment-api",
  "profile": "cpu",
  "configRevision": 42,
  "resolvedVersion": "1.42.3",
  "versionLabel": "app.kubernetes.io/version",
  "sampling": {"duration": "30s", "rounds": 2, "roundInterval": "30s", "replicas": "all", "maxParallel": 4},
  "attempt": 1,
  "truncated": false,
  "gateway": "profgate-7f88fdf79-xabcd/q2w3e4r5",
  "samples": [
    {"round": 0, "pod": "payment-api-7c8f8c9b9-a", "podUID": "3c1e…", "node": "worker-07", "startedAt": "2026-08-23T12:03:13Z", "result": "ok", "bytes": 48211},
    {"round": 0, "pod": "payment-api-7c8f8c9b9-b", "podUID": "9a0f…", "node": "worker-02", "startedAt": "2026-08-23T12:03:13Z", "result": "upstream_timeout", "bytes": 0}
  ]
}
```

No Pod IP or port appears in it.
It answers "is this profile safe for build X" (version, label, per-sample identity),
"why is it smaller than expected" (failed samples),
and "is this the whole fleet" (`truncated`, set when a round had more eligible Pods than `maxTargetsPerRound`).

---

## 10. HTTP API

All routes are on the API listener, under `/v1`, realm-checked, with `Cache-Control: no-store`.
The gateway spec's *Request algorithm* applies with these additions:
step 2 accepts the methods listed per route,
step 5 evaluates the realm's `pgo` flags after namespace and Service,
and a new step between readiness and authentication answers `501 pgo_disabled` when `pgo.enabled` is false
and `503 pgo_unavailable` when the NATS connection is down.

| Route | Methods | Realm flag |
|---|---|---|
| `/v1/namespaces/{ns}/services/{svc}/pgo` | `GET` | `pgo.read` |
| `/v1/namespaces/{ns}/services/{svc}/pgo` | `PUT`, `DELETE` | `pgo.configure` |
| `/v1/namespaces/{ns}/services/{svc}/collections` | `GET` | `pgo.read` |
| `/v1/namespaces/{ns}/services/{svc}/collections` | `POST` | `pgo.collect` |
| `/v1/collections/{id}` | `GET` | `pgo.read` |
| `/v1/collections/{id}/profile` | `GET` | `pgo.read` |
| `/v1/collections/{id}/cancel` | `POST` | `pgo.collect` |

For the three `/v1/collections/{id}` routes the record is read first
and the realm is evaluated against the record's namespace and Service.
A record the realm denies, and a record that does not exist, both answer `404 collection_not_found`.
The identifier is opaque, so this leaks nothing the realm would hide.

Request bodies are JSON, at most 64 KiB, decoded with unknown fields rejected (`400 invalid_parameter`).

`state` is a closed set for this release:
the values listed in section 8.2 are exhaustive, and adding one is a spec change.
`origin` and `reason` are open sets:
this release enumerates the values in use today and later work can add more,
so a client that switches on either needs a default arm for a value it does not recognize.

### 10.1 Policy

```http
GET /v1/namespaces/payment/services/payment-api/pgo
```

```http
HTTP/1.1 200 OK
ETag: "42"
```

```json
{
  "namespace": "payment",
  "service": "payment-api",
  "source": "override",
  "override": {"enabled": true, "schedule": {"every": "1h", "jitter": "5m"}, "sampling": {"rounds": 3}},
  "effective": {
    "enabled": true,
    "schedule": {"every": "1h", "jitter": "5m"},
    "sampling": {"duration": "30s", "rounds": 3, "roundInterval": "30s", "replicas": "all", "maxParallel": 4},
    "target": {"versionPolicy": "strict", "version": ""},
    "artifact": {"retention": "2h"}
  },
  "violations": [],
  "updatedBy": "anonymous",
  "updatedAt": "2026-08-23T10:00:00Z"
}
```

`source` is `override` or `defaults`.
With no override, `override` is `null`, `ETag` is absent, and `effective` is the defaults with `enabled: false`.
`violations` lists fields whose stored value exceeds a current ceiling.

```http
PUT /v1/namespaces/payment/services/payment-api/pgo
If-Match: "42"
Content-Type: application/json
```

```json
{"enabled": true, "schedule": {"every": "1h"}, "sampling": {"rounds": 3}}
```

The body is the override, not the effective policy; fields absent from it fall back to the defaults.

| Condition | Response |
|---|---|
| key absent, no `If-Match` | `Create`; `201 Created`, `ETag` of the new revision, body as `GET` |
| key absent, `If-Match` present | `412 precondition_failed` |
| key present, no `If-Match` | `428 precondition_required` |
| key present, `If-Match` equals the current revision | `Update`; `200 OK`, new `ETag` |
| key present, `If-Match` differs | `412 precondition_failed` |
| effective policy violates a ceiling | `400 limit_exceeded` |
| `pgo.configAPI` is `disabled` | `403 config_api_disabled` |

`If-Match` is a quoted decimal revision; any other form, including `*`, is `400 invalid_parameter`.

```http
DELETE /v1/namespaces/payment/services/payment-api/pgo
If-Match: "43"
```

The handler reads the key for its revision and the absent/present distinction, then deletes at that revision.
`204 No Content`; `428` and `412` as for `PUT`; `404 pgo_override_not_found` when the key is absent.
No body is accepted (`400 invalid_parameter` if one is sent).
A `Delete` that loses its revision (the key moved between the read and the delete) is `412`,
the same as a stale `If-Match`: the client re-reads and decides again.
Deleting the override returns the Service to the defaults and stops scheduling it.

### 10.2 Create a Collection

```http
POST /v1/namespaces/payment/services/payment-api/collections
Content-Type: application/json
```

```json
{
  "sampling": {"duration": "30s", "rounds": 2, "replicas": 3},
  "target": {"version": "1.42.3"},
  "artifact": {"retention": "4h"}
}
```

Every field is optional; an empty body `{}` is valid.
The handler runs only behind the replay barrier (section 5.1), answering `503 pgo_unavailable` before it,
so the override it reads from the watched `PROFGATE_CONFIG` cache is the stored one and `configRevision` is never `0` for a Service that has an override.
The Collection's policy snapshot is the effective policy with the body's fields replacing it;
`enabled` and `schedule` in the body are `400 invalid_parameter`.
The snapshot is validated against the ceilings (`400 limit_exceeded`).
A replica whose on-demand token bucket is empty answers `429 rate_limited` before any write (section 7.3).
When the publisher's `Reserve()` refuses — Services the watched caches show as live plus this replica's reservations at `maxLiveCollections` — the handler answers `429 capacity_exhausted` and writes nothing (section 7.2).
A Service that already has a live Collection answers `429 collection_in_progress`:
the handler's `Create` of `active.<ns>.<svc>` loses (section 7.3),
or the watched cache already shows the key and the write is skipped.

Before creating the record the handler resolves targets once, as round 0 would,
and answers `409 version_conflict` or `409 version_missing` on the spot;
this is advisory — the round is authoritative — so a Collection can still fail for the same reason later.
A Service that does not exist or has no selector answers as the gateway spec's *Discovery* step does.

```http
HTTP/1.1 202 Accepted
Location: /v1/collections/7h2k9m4p6r8t0v1w3x5y
```

```json
{"id": "7h2k9m4p6r8t0v1w3x5y", "state": "pending"}
```

### 10.3 List Collections

```http
GET /v1/namespaces/payment/services/payment-api/collections
```

```json
{
  "namespace": "payment",
  "service": "payment-api",
  "collections": [
    {"id": "7h2k9m4p6r8t0v1w3x5y", "origin": "schedule", "state": "completed", "attempt": 1, "resolvedVersion": "1.42.3", "createdAt": "2026-08-23T12:03:12Z", "finishedAt": "2026-08-23T12:05:40Z", "expiresAt": "2026-08-23T14:05:40Z"}
  ]
}
```

Newest `createdAt` first, at most 100 entries, no pagination, read from the watched cache.
Query parameters are `400 invalid_parameter`.

### 10.4 Get a Collection

```http
GET /v1/collections/7h2k9m4p6r8t0v1w3x5y
```

`200` with the full record of section 8.2, as stored.
It contains no Pod IP;
the owner instance is a gateway Pod name plus a random suffix,
which a realm that may read PGO state may know.

### 10.5 Download

```http
GET /v1/collections/7h2k9m4p6r8t0v1w3x5y/profile
```

| State | Response |
|---|---|
| `completed`, object present | `200`, `Content-Type: application/octet-stream`, `Content-Disposition: attachment; filename="7h2k9m4p6r8t0v1w3x5y.pprof"`, `X-Pprof-Collection: 7h2k9m4p6r8t0v1w3x5y`, `X-Pprof-Target-Version: 1.42.3`, body streamed from the store |
| `completed`, object missing | record flipped to `expired` by conditional update; `410 artifact_gone` |
| `expired` | `410 artifact_gone` |
| `initializing`, `pending`, `running` | `409 collection_not_completed` |
| `failed`, `cancelled` | `409 collection_not_completed` |

The body is streamed, not buffered, and the outcome is classified as the gateway spec's *Proxy behavior* section classifies upstream streams:

| Condition | Response | Audit / metrics `code` |
|---|---|---|
| store error before headers (`ErrUnavailable`) | `503 pgo_unavailable` | `pgo_unavailable` |
| store error after headers, including the object expiring mid-stream | connection closed; body truncated | `artifact_stream_failed` |
| client disconnected | store read cancelled | `client_gone` |

A download in progress does not protect its object from expiry;
the sweeper deletes on schedule and the reader sees `artifact_stream_failed`.
Retention is hours and a download is seconds, so the race is rare and the client retries.

### 10.6 Cancel

```http
POST /v1/collections/7h2k9m4p6r8t0v1w3x5y/cancel
```

`200` with the updated record;
`409 collection_initializing` while the record is still being published (nonterminal; the client retries after a second);
`409 collection_terminal` when already terminal.
No body is accepted (`400 invalid_parameter` if one is sent).

### 10.7 Errors

Added to the gateway's table; same envelope, same rule that `code` is the contract.

| Status | `code` |
|---|---|
| 400 | `limit_exceeded` |
| 403 | `config_api_disabled` |
| 404 | `collection_not_found`, `pgo_override_not_found` |
| 409 | `version_conflict`, `version_missing`, `collection_not_completed`, `collection_initializing`, `collection_terminal` |
| 410 | `artifact_gone` |
| 412 | `precondition_failed` |
| 428 | `precondition_required` |
| 429 | `collection_in_progress`, `rate_limited`, `capacity_exhausted` |
| 501 | `pgo_disabled` |
| 503 | `pgo_unavailable` |

`invalid_parameter`, `realm_denied`, `route_unknown`, `method_not_allowed`, `not_ready`,
`service_not_found`, and `service_selectorless` are reused with their gateway meanings.
Audit-only codes, never an HTTP status of their own: `cas_contended`, `artifact_stream_failed`, `client_gone`.

### 10.8 Non-disclosure

The gateway spec's *Non-disclosure* section holds.
Records, manifests, and error bodies name namespaces, Services, Pods, nodes, versions, and gateway Pod names;
never a Pod IP or pprof port.
The merged profile is application data and passes through as the interactive profile body does.

---

## 11. Configuration

New top-level blocks `nats` and `pgo`, and a `pgo` block in each realm.
Loading, strict unknown-key handling, environment prefix, and the `atomic.Pointer` snapshot are as in the gateway spec's *Configuration* section.

| Key | Env | Default | Reload | Validation |
|---|---|---|---|---|
| `pgo.enabled` | `PROFGATE_PGO_ENABLED` | `false` | restart | bool |
| `pgo.configAPI` | `PROFGATE_PGO_CONFIG_API` | `enabled` | hot | `enabled` or `disabled` |
| `pgo.leaseTTL` | `PROFGATE_PGO_LEASE_TTL` | `60s` | restart | 30s–10m |
| `pgo.maxAttempts` | `PROFGATE_PGO_MAX_ATTEMPTS` | `3` | restart | 1–10 |
| `pgo.jobRetention` | `PROFGATE_PGO_JOB_RETENTION` | `168h` | restart | ≥ `pgo.limits.maxRetention + 1h`; ≤ 2160h |
| `pgo.limits.maxDuration` | `PROFGATE_PGO_LIMIT_MAX_DURATION` | `60s` | restart | 1s–`limits.cpuSeconds` |
| `pgo.limits.maxRounds` | `PROFGATE_PGO_LIMIT_MAX_ROUNDS` | `5` | restart | 1–20 |
| `pgo.limits.maxParallel` | `PROFGATE_PGO_LIMIT_MAX_PARALLEL` | `4` | restart | 1–64 |
| `pgo.limits.minEvery` | `PROFGATE_PGO_LIMIT_MIN_EVERY` | `15m` | restart | 1m–`maxEvery` |
| `pgo.limits.maxEvery` | `PROFGATE_PGO_LIMIT_MAX_EVERY` | `24h` | restart | `minEvery`–24h |
| `pgo.limits.maxRetention` | `PROFGATE_PGO_LIMIT_MAX_RETENTION` | `24h` | restart | 1m–720h |
| `pgo.limits.maxSampleBytes` | `PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES` | `33554432` | restart | 1 MiB–256 MiB |
| `pgo.limits.maxMergedBytes` | `PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES` | `67108864` | restart | `maxSampleBytes`–1 GiB |
| `pgo.limits.maxTargetsPerRound` | `PROFGATE_PGO_LIMIT_MAX_TARGETS_PER_ROUND` | `32` | restart | 1–256; `maxRounds × maxTargetsPerRound ≤ 256` |
| `pgo.limits.maxActiveCollections` | `PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS` | `2` | restart | ≥ 1; `maxParallel × maxActiveCollections < limits.maxConcurrentProfiles` |
| `pgo.limits.onDemandPerMinute` | `PROFGATE_PGO_LIMIT_ON_DEMAND_PER_MINUTE` | `10` | restart | 1–600 |
| `pgo.limits.maxLiveCollections` | `PROFGATE_PGO_LIMIT_MAX_LIVE_COLLECTIONS` | `64` | restart | 1–1024 |
| `pgo.defaults.schedule.every` | — | `6h` | hot | `minEvery`–`maxEvery` |
| `pgo.defaults.schedule.jitter` | — | `10m` | hot | ≤ `every / 2` |
| `pgo.defaults.sampling.duration` | — | `30s` | hot | ≤ `maxDuration` |
| `pgo.defaults.sampling.rounds` | — | `2` | hot | ≤ `maxRounds` |
| `pgo.defaults.sampling.roundInterval` | — | `30s` | hot | 0–10m |
| `pgo.defaults.sampling.replicas` | — | `all` | hot | `all` or 1–`maxTargetsPerRound` |
| `pgo.defaults.sampling.maxParallel` | — | `4` | hot | ≤ `limits.maxParallel` |
| `pgo.defaults.target.versionPolicy` | — | `strict` | hot | `strict` |
| `pgo.defaults.artifact.retention` | — | `2h` | hot | ≤ `maxRetention` |
| `nats.url` | `PROFGATE_NATS_URL` | — | restart | required when `pgo.enabled`; `nats://` or `tls://` URL list |
| `nats.credsFile` | `PROFGATE_NATS_CREDS_FILE` | — | restart | readable file when set |
| `nats.connectTimeout` | `PROFGATE_NATS_CONNECT_TIMEOUT` | `5s` | restart | 1s–60s |
| `realms.<name>.pgo.read` | — | `false` | hot | bool |
| `realms.<name>.pgo.collect` | — | `false` | hot | bool |
| `realms.<name>.pgo.configure` | — | `false` | hot | bool |

The Reload column records which changes a future reload could apply without a restart;
no reload mechanism exists yet, so today every change, `hot` or `restart`, takes effect only after one.
`pgo.defaults` is hot because it is policy, like realms;
the scheduler reads the config pointer on every tick.
`pgo.limits` is restart because the memory figure in section 3.4 and the admission arithmetic in section 8.5 depend on it.
`skewMargin` (section 8.4), `maxRecordBytes` (section 8.2), `decodeFactor` (section 3.4),
and the 10-minute orphan age (section 8.9) are constants, not configuration:
a knob would invite tuning them past the assumptions they encode.
The defaults satisfy every cross-field rule as published:
`4 × 2 < 16` (the `maxParallel` ceiling times `maxActiveCollections`), `5 × 32 ≤ 256`, `168h ≥ 24h + 1h`, `60s ≤ 60`.
Every cross-field rule above — between limits, and between a default and its ceiling —
runs only when `pgo.enabled` is true:
a file carrying an inconsistent `pgo` block loads without error while disabled
and fails at startup only once `pgo.enabled` flips to true.

```yaml
nats:
  url: nats://nats.profgate.svc:4222
  credsFile: /etc/profgate/nats/nats.creds
  connectTimeout: 5s
pgo:
  enabled: true
  configAPI: enabled
  leaseTTL: 60s
  maxAttempts: 3
  jobRetention: 168h
  limits:
    maxDuration: 60s
    maxRounds: 5
    maxParallel: 4
    minEvery: 15m
    maxEvery: 24h
    maxRetention: 24h
    maxSampleBytes: 33554432
    maxMergedBytes: 67108864
    maxTargetsPerRound: 32
    maxActiveCollections: 2
    onDemandPerMinute: 10
    maxLiveCollections: 64
  defaults:
    schedule:
      every: 6h
      jitter: 10m
    sampling:
      duration: 30s
      rounds: 2
      roundInterval: 30s
      replicas: all
      maxParallel: 4
    target:
      versionPolicy: strict
    artifact:
      retention: 2h
realms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
    pgo:
      read: true
      collect: true
      configure: true
```

The shipped example writes every flag as `true`, so the wide-open default is visible in version control,
as the gateway spec's *Wide-open is explicit* section requires.
A realm without a `pgo` block has every flag false.

---

## 12. Operations

### 12.1 Logging

Every PGO request emits one record on completion:

```text
principal, namespace, service, collection, method, status, code, duration_ms
```

Every Collection transition emits one record:

```text
collection, namespace, service, state, attempt, reason, instance
```

Every sample emits one record at debug level with `pod`, `round`, `result`, `bytes`; never an IP.

### 12.2 Health

`/readyz` additionally requires the NATS preflight to have passed when `pgo.enabled` is true.
It does not wait for the replay barrier:
a replica whose watches are still replaying serves interactive requests and answers PGO routes `503 pgo_unavailable`,
which is correct behavior for it, not a reason to be removed from the Service.
A NATS connection lost afterwards does not change `/readyz` either:
interactive profiling is unaffected, PGO routes answer `503 pgo_unavailable`
(the disconnect moves the connection generation and so clears the barrier,
which stays cleared until every watch has replayed under the new generation),
and the client library reconnects on its own.

### 12.3 Metrics

| Metric | Labels |
|---|---|
| `profgate_collections_total` (counter) | `result` (`completed`/`failed`/`cancelled`/`expired`) |
| `profgate_collection_samples_total` (counter) | `result` (`ok`/`failed`) |
| `profgate_collection_duration_seconds` (histogram, buckets `10, 30, 60, 120, 300, 600, 1200`) | — |
| `profgate_schedule_slots_total` (counter) | `result` (`won`/`lost`/`busy`/`capacity`) |
| `profgate_sweeper_deletes_total` (counter) | `kind` (`artifact`/`record`/`slot`/`active`/`orphan`/`probe`) |
| `profgate_collections_active` (gauge) | — |
| `profgate_nats_connected` (gauge) | — |

`profgate_requests_total` gains `endpoint` values
`pgo_policy`, `collections`, `collection`, `collection_profile`, `collection_cancel`,
with `profile` fixed to `cpu` for the last three and `none` otherwise,
and `code` gains the values of section 10.7 including the audit-only ones.
No namespace, Service, or Collection identifier becomes a label.

### 12.4 Shutdown

On `SIGTERM` the scheduler and sweeper stop at once.
The worker stops claiming and finishes its in-flight Collections inside the grace period,
as long as its Kubernetes credentials stay valid:
the gateway authenticates with a projected ServiceAccount token bound to its own Pod object,
and a graceful delete leaves that Pod object in place until termination completes,
so the token is valid for the whole drain.
A force deletion (`GracePeriodSeconds: 0`) removes the Pod object immediately instead:
every `Confirm` in the next round fails `discovery_unavailable`, the round has zero successful samples,
and the worker ends the Collection `failed no_samples` rather than finishing it;
the next schedule slot collects the Service again.
A Collection that cannot finish because the worker process itself dies stops renewing;
another replica's scan reclaims it once `leaseTTL + skewMargin` has passed.
Drain waits for every work goroutine to exit, up to the Collection's `deadline`,
because `Merge`, `Compact`, and `Write` cannot be interrupted (section 8.4);
the grace period must therefore cover the deadline formula of section 8.2 at the configured ceilings,
and `profgate config validate` prints the required value alongside the gateway's own.
A Collection's samples are ordinary proxied requests inside the gateway spec's drain bound.

---

## 13. Testing

### 13.1 Unit

`mise run test`, seconds, no cluster and no external NATS:
`internal/natskv` and `internal/pgo` run an in-process `github.com/nats-io/nats-server/v2` with JetStream on a temporary directory,
one server per subtest.

- `internal/natskv`:
  `Create` on an existing key is `ErrKeyExists`;
  `Update` with a stale revision is `ErrRevisionMismatch` and leaves the value unchanged;
  `Delete` with a stale revision likewise;
  `Watch` delivers existing keys, then exactly one `Synced` marker with an empty key, then later puts, and deletes as nil values,
  and stops on context end;
  the marker arrives even for an empty prefix;
  after the in-process server is restarted the seam re-opens the watch and delivers a fresh marker after the full replay;
  the disconnected callback alone moves `Generation()` and makes `Synced(gen)` false for the new generation
  before the connection is usable again, with no watch re-opened yet;
  a call on a `View(gen)` issued after the generation moved is `ErrUnavailable` before it reaches the server,
  and one issued before the move whose result arrives after it is `ErrUnavailable` whatever the server answered;
  `View(gen)` for a generation that is not current is `ErrUnavailable`;
  `Preflight` against a server missing any one bucket,
  or with `PROFGATE_ARTIFACTS` created as a KV bucket,
  returns the named error;
  `Preflight` against a bucket provisioned, one field at a time, with a 1-minute TTL, memory storage, `Discard: old`,
  a 1 MiB `MaxBytes`, or a `MaxValueSize` below `maxRecordBytes`,
  fails naming the bucket and the field, and passes with the commands of section 3.2;
  `Objects.Delete` of an absent name returns nil;
  `Preflight` with a user that can open the buckets but lacks publish on `$KV.PROFGATE_JOBS.>`,
  lacks publish on `$O.PROFGATE_ARTIFACTS.>`,
  or lacks `$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>` (`List` cannot open its consumer),
  fails naming the bucket and the operation, and the probe keys and objects are gone afterwards;
  `Preflight` still succeeds for a user that lacks subscribe on `$KV.PROFGATE_JOBS.>` or on `$O.PROFGATE_ARTIFACTS.>`,
  which is what pins the reason those two grants are unexercised today (section 3.3);
  `Preflight` succeeds only after the watch has delivered the create, update, and delete revisions of the probe key,
  proven by a server that accepts publishes but drops deliveries to the probe subscription;
  `List` returns every object with its `ModTime` and nothing for an empty bucket;
  every call against a stopped server returns `ErrUnavailable` within its deadline;
  an Object `Put`/`Get` round-trips 40 MiB byte for byte and `Get` of an absent name is `ErrObjectNotFound`;
  a recording of the subjects the client publishes to during every operation is a subset of the section 3.3 list
  (the NATS analogue of the recording-transport test).
- `internal/pgo` policy:
  layering one level deep, `null` as unset,
  every ceiling violated one field at a time at write and at read,
  `replicas` as `"all"`, integer, an integer above `maxTargetsPerRound`, and rejected strings,
  `every` above `maxEvery`,
  and `enabled` having no default.
- `internal/pgo` scheduler, with a fake clock and two scheduler instances over one server:
  exactly one `job.*` record per slot across 100 ticks with interleaved clocks;
  the same offset from both instances for the same inputs;
  a disabled, ceiling-violating, or overridden-to-`null` Service never creates a slot;
  a clock jumped 3 days forward creates one Collection, not 72;
  a policy change between two ticks of one slot does not create a second record;
  with `every: 24h` and the clock advanced across the slot boundary and then past the first key's `retainUntil`,
  the first slot's key is deleted only after `retainUntil` and the first slot is never re-created;
  a Service whose previous Collection is still `pending` or `running` gets `busy`, not a second record,
  and the same holds when the cache is frozen so the check falls through to a losing `Create` of the active key;
  two scheduler instances reading different `every` revisions for one Service, both past their fire times,
  create two slot keys and exactly one `job.*` record;
  a scheduled tick and a `POST /collections` released from one barrier yield one record and one `busy` or `429`;
  the slot key for the slot starting 2026-08-24T00:00:00Z is exactly `schedule.payment.payment-api.1787529600`
  and the offset hash input is exactly `payment/payment-api/1787529600`;
  a creator paused at a barrier after winning the active key, while a sweeper pass runs and a second creator tries,
  leaves the active key in place, gives the second creator `busy`, and yields exactly one live Collection
  once the first creator resumes;
  a creator killed after the `initializing` create, and another killed after the active create,
  each leave state the scan fails `not_published` on its first pass after `createdAt + 1m + skewMargin`
  once the record is in the scanning worker's cache, and releases, after which the next slot runs;
  a lost active create deletes the creator's own `initializing` record and leaves the bucket with one `job.*` record;
  100 Services with `maxLiveCollections: 8` and two scheduler instances leave at most 16 live Collections,
  and with both replicas' active-key watches frozen before a barrier
  and scheduled plus concurrent on-demand creations across distinct Services released together,
  each replica publishes at most its headroom and the cluster stays at or below 16,
  a test that fails when the reservation counter is replaced by the cached count alone;
  with `maxLiveCollections: 1` and both watches frozen, the first Collection claimed and `running`,
  the fake clock advanced past `claimBy`, and scheduler ticks for a second Service,
  no second active key is written (the test fails when passing `claimBy` releases the reservation);
  with both watches frozen, a job create or an active create that committed but returned `ErrUnavailable`
  keeps the reservation across ticks and releases it only when a cache delivers the record or the key,
  after which the Service is counted as live and refused with `busy`;
  an uncommitted indeterminate create, and a lost active create whose own-record delete returned `ErrUnavailable`,
  release by the fresh reads finding nothing when nothing committed,
  and are held until a cache delivers the `initializing` record when it did;
  the publisher replaced between indeterminate publications, as a restarted gateway's would be,
  with the replay of its watches held by the test before the marker:
  every scheduler tick publishes nothing, `POST /collections` answers `503 pgo_unavailable`,
  and the authoritative bucket gains no record
  (the test fails when the barrier is removed and the replacement reserves against an empty cache);
  once the replay marker is delivered it counts the replayed `initializing` record as live, refuses the Service with `busy`,
  and publishes again only after the scan fails it with `not_published`
  (the test fails when `cachedLive` counts active keys alone);
  each of these, repeated across ticks and restarts with `maxLiveCollections: 1`,
  keeps the count of nonterminal `job.*` records in the authoritative bucket
  at or below `2 × replicas × maxLiveCollections`;
  record `capacity` for the rest, and write neither a slot key, an active key, nor a record for them;
  `ErrUnavailable` on the slot create publishes nothing from that attempt in either outcome:
  no committed slot, or a committed slot with a lost acknowledgement that leaves the key present;
  authoritative state changed during an outage (another replica's active key and record, and a changed override),
  the connection then restored while the test holds watch re-opening:
  every scheduler tick publishes nothing, `POST /collections` and `PUT /pgo` answer `503 pgo_unavailable`,
  and no store operation is issued,
  because the barrier is false for the new generation
  (the test fails when the barrier is cleared by watch re-opening instead of by the generation);
  a tick paused after `Synced(g)` succeeded and `View(g)` was taken,
  the connection then disconnected and restored while the test holds replay,
  and the tick released into its first store call:
  that call returns `ErrUnavailable`, the tick ends, and no slot key, active key, or record is written from its stale caches
  (the test fails when the view does not re-check the generation around the call).
- `internal/pgo` worker, with a fake `Discovery`, `httptest` pprof servers serving fixture profiles, and a fake clock:
  two workers racing one `pending` record — one runs, the other's `Update` is `ErrRevisionMismatch`;
  a worker stopped mid-round, the fake clock advanced past `leaseUntil + skewMargin` with no KV write,
  and the other worker's scan reclaims with `attempt` 2 and the merge contains only the second run's samples
  (the test fails if the scan is removed and only the watch remains);
  a `pending` record past `claimBy` is failed `not_claimed` by the scan;
  two `pending` records created under `maxParallel: 8`, claimed by a worker whose `pgo.limits.maxParallel` is `4`,
  are failed `limit_exceeded`;
  their active keys are released, no local slot is reserved, and the trap pprof server receives nothing
  (the test fails when the ceiling check is removed or moved after `reserveLocalSlot`);
  nothing is claimed, scanned, or swept while the test holds the replay marker back, and all three start once it is delivered;
  a reclaimed owner whose clock runs `skewMargin` ahead of the first claimer's issues no final update
  once `now >= deadline - skewMargin`, and deletes its object;
  a `running` record past `deadline` is failed `deadline_exceeded` by the scan, not claimed;
  `maxAttempts` reached marks `failed` `attempts_exhausted`;
  a stale owner held at a barrier until the reclaiming owner has completed,
  then released to `Put` and lose its update,
  leaves the winner's object downloadable byte for byte and its own object deleted,
  in both orders of the two `Put` calls;
  a renewal whose NATS call is blocked until after `leaseUntil - skewMargin` aborts the Collection,
  and a trap server proves no sample is fetched after that moment;
  a renewal that returns `ErrUnavailable` leaves the committed lease unchanged
  and the work context's cutoff at the previous value;
  the work goroutine held at a barrier inside merge, inside `Write`, and inside `Put`,
  in turn, past `committedLeaseUntil - skewMargin`:
  without a reclaiming scan the owner issues no final update and stores nothing;
  with one, the reclaimer completes and the stale owner's late object is deleted and never named;
  a work function that ignores cancellation until a barrier releases it, past the cutoff:
  no final `Update` is issued, the replica's local slot stays taken until the function returns,
  and a second claimable record on that replica is not claimed before then;
  renewal `ErrRevisionMismatch` cancels the work context immediately, not at the cutoff;
  the final `completed` update is never issued
  when the committed lease or the deadline has passed by the time the object is stored;
  renewal and finish racing in the owner loop never produce `ErrRevisionMismatch` against the owner's own write;
  a claimer whose clock runs `skewMargin` ahead does not reclaim a lease that is valid on the owner's clock;
  cancel between rounds and cancel mid-sample both end with no object in the store;
  renewal `ErrRevisionMismatch` stops the worker before its next sample is fetched (a trap server asserts no dial);
  `ErrUnavailable` on every renewal until `leaseUntil - skewMargin` passes stops it likewise;
  round 0 with two versions fails `version_conflict`; with no labels, `version_missing`;
  a Pod of a new version appearing in round 1 is excluded and the manifest shows only `resolvedVersion`;
  every Pod rolled by round 1 fails `no_targets`;
  `replicas: 2` over five Pods with a fixed `Shuffle` sequence samples two distinct Pods per round
  and the union over 20 rounds covers all five;
  two production-seeded workers over the same five Pods produce different orders within 20 rounds
  (identical orders have probability 120^-20; the test fails if the seed is made constant);
  a first valid sample becomes the running profile without a `Merge` call
  (a `Merge` seam records its arguments and fails the test on a nil first argument),
  and the second sample reaches `Merge` with both profiles;
  `replicas: all` over `maxTargetsPerRound + 3` Pods samples exactly `maxTargetsPerRound` per round with `truncated: true` in the manifest;
  a counting decoder seam proves input bytes held for samples never exceed `maxParallel × 2 × maxSampleBytes`;
  a gzip body of 4 KiB that expands past `maxSampleBytes` is `sample_too_large` before `ParseData` is called,
  and the test fails when the decompression limit is removed;
  a gzip body whose decompressed bytes are again gzip is `sample_malformed` and `ParseData` is never called;
  a regression guard, skipped under `-race`, parses a fixture profile and asserts the heap delta
  (`runtime.ReadMemStats` before and after) is under `decodeFactor × len(fixture)`;
  a multi-batch Collection (`maxParallel` 2 over six Pods, three rounds) leaves the gate at full capacity afterwards,
  and the test fails when any sampler path omits its `release`;
  a claim whose `Update` returns `ErrUnavailable` profiles nothing,
  a trap pprof server and a counting `Discovery` prove no `Confirm` and no dial,
  and the replica's local slot is free again;
  a running merge whose serialized size crosses `maxMergedBytes` fails `merged_too_large` before the next sample is merged;
  two valid profiles of one version with different sample types: the second is `incompatible_profile`,
  the running profile is unchanged, and the round continues;
  a writer that fails inside `Write` ends `serialize_failed` with nothing stored;
  a record serialized at the ceiling (`maxRounds × maxTargetsPerRound = 256` samples with every field at its maximum length:
  253-character Pod and node names, 36-character UIDs, nanosecond timestamps with offsets, 32-character reasons)
  stays under `maxRecordBytes`,
  and a fixture forced past it ends `record_too_large` with its object deleted
  and a terminal record that carries the manifest counts and no `samples`, whose `Update` succeeds;
  the deadline with `all` is computed from `maxTargetsPerRound`, not from the five live Pods;
  a sample over `maxSampleBytes` is `sample_too_large`, an unparseable body `parse_failed`, and the round continues;
  a round of all-failed samples is `no_samples`;
  the merged object parses with `profile.Parse` and its sample count equals the fixtures' sum;
  the deadline passing fails `deadline_exceeded`;
  `Put` failing fails `artifact_store_failed` with no record flip;
  every terminal transition — completed, each failure reason, cancel, and the scan's failures — deletes the active key,
  and a key renamed to another Collection's id in between is left alone;
  slot pressure: the real `httpapi` handler and the real worker share one `admit.Gate` with capacity 3 and `maxParallel` 2;
  an interactive request always finds a slot while a Collection runs,
  and the test fails when either side is given its own gate.
- `internal/pgo` sweeper:
  expiry deletes the object then flips the record; a lost `Update` leaves the record alone;
  job retention deletes `expired`, `failed`, and `cancelled` records and never a `completed` one;
  slot keys are deleted after `retainUntil` and not before;
  an object no `completed` record names, older than 10 minutes, is deleted, a younger one kept,
  and an object a `completed` record names is never deleted before `expiresAt`;
  the sweeper's cache frozen before a `completed` record lands while the authoritative KV holds it:
  the object survives, and the test fails when the fresh `Get` is removed;
  `ErrUnavailable` on that `Get` keeps the object;
  an active key whose job is terminal, and one whose job is absent, are deleted; one whose job is `running` is kept;
  a probe key created a minute ago by `Entry.Created` is kept and one created eleven minutes ago is deleted,
  and the same for a probe object by `ModTime`.
- `internal/httpapi`:
  a table over every PGO route × method × realm flag × state → status and code, including `501` with `pgo.enabled` false
  and `503` with a fake `Client` whose `Connected()` reports false or with the replay barrier not yet cleared
  (every state-reading and state-writing PGO route, while `/readyz` stays `200`);
  `If-Match` matrix of section 10.1, and `DELETE` against a key moved between its read and its delete answering `412`;
  cancel racing a renewal at a barrier: the cancel wins on its retry with `200`, never `409`;
  cancel against a record already `completed` answering `409 collection_terminal`,
  against an `initializing` record answering `409 collection_initializing`,
  and download of an `initializing` record answering `409 collection_not_completed`;
  `POST /collections` while a Collection is live answering `429 collection_in_progress`;
  eight concurrent `POST /collections` for one Service released from a barrier against a frozen cache:
  exactly one `202` and seven `429`, and one `active.*` key in the bucket;
  `POST /collections` across 50 distinct Services with `onDemandPerMinute: 10`:
  ten `202` and forty `429 rate_limited`, ten `active.*` keys, and no write for the rejected ones;
  `POST /collections` with the cache holding `maxLiveCollections` active keys
  answering `429 capacity_exhausted` with no write;
  a download whose store reader fails after headers closing the connection with audit code `artifact_stream_failed`;
  `404 collection_not_found` identical for a missing id and a realm-denied one;
  `410` on a `completed` record whose object is gone, and the record observed `expired` afterwards;
  body size and unknown-field rejection;
  no response, header, or manifest containing a Pod IP or port.
- `internal/config`:
  every new environment variable lands on its field;
  `nats.url` required only when `pgo.enabled`;
  the complete example of section 11 loads and validates as written;
  the admission inequality using the `maxParallel` ceiling
  (`limits.maxParallel: 8` with `maxActiveCollections: 2` and `maxConcurrentProfiles: 16` rejected, `4` accepted),
  and `maxRounds × maxTargetsPerRound` above 256;
  `onDemandPerMinute` outside 1–600;
  `jobRetention` below `maxRetention + 1h` rejected;
  `maxEvery` below `minEvery` rejected;
  `maxDuration` above `limits.cpuSeconds`;
  defaults violating limits;
  a realm without `pgo` has all flags false.
- `deploy/`: the NATS account fragment equals the section 3.3 list exactly.
  A manifest test pins the NATS credentials Secret volume:
  volume name, Secret source with `defaultMode: 0440`,
  mount path `/etc/profgate/nats/`, `readOnly: true` on the mount,
  and `fsGroup: 65532` in the pod `securityContext`.
- `internal/admit`: `TryAcquire` fails without blocking at capacity;
  `Acquire` waits and returns on release or on context end.
- Repository checks: the nats.go import check; the existing client-go check still passes.

### 13.2 End-to-end

`mise run test:e2e` gains a NATS JetStream Deployment in the kind cluster
(`nats:2.11-alpine`, one replica, `--jetstream` with a file store directory on an `emptyDir`, a ClusterIP Service),
applied by `TestMain` with the gateway overlay.
The harness provisions the three buckets with `nats.go` through a port-forward before the gateway starts,
with the configuration of section 3.2 (file storage, no TTL, `Discard: new`, no size limits).
Between scenarios it purges every key and object so each starts empty;
it never deletes or recreates a bucket while a gateway runs,
because the gateways' watches and consumers belong to the original streams
and a recreated stream would leave them watching nothing.
The gateway runs with `pgo.enabled: true` and a realm whose `pgo` flags are all true.
Scenarios that need the gateway to complete a proxy to a test-app Pod declare `needsPodReach`.

1. An on-demand Collection with `rounds: 2, replicas: all` against a three-replica test app
   reaches `completed` on either gateway,
   the downloaded artifact parses,
   the manifest lists six `ok` samples across three distinct Pod UIDs,
   and both gateways return the same record and the same bytes (`needsPodReach`).
2. A `PUT /pgo` with `every` equal to `minEvery` and `jitter: 0`
   yields exactly one Collection for the slot across two gateways;
   the harness watches `PROFGATE_JOBS` directly and counts `schedule.*` and `job.*` keys (`needsPodReach`).
3. A Collection with `rounds: 3, roundInterval: 20s` cancelled after round 1 ends `cancelled`,
   with no object in `PROFGATE_ARTIFACTS` (`needsPodReach`).
4. Two test-app Deployments with different version labels behind one Service:
   `POST /collections` answers `409 version_conflict`;
   with `target.version` pinned it completes with samples from one Deployment only (`needsPodReach`).
5. The owning gateway Pod is deleted after its first renewal;
   the other gateway reclaims with `attempt: 2` and the Collection completes (`needsPodReach`).
6. Realm without `pgo.configure`: `PUT /pgo` is `403`;
   without `pgo.read`: `GET /collections/{id}` of an existing record is `404`.
7. A gateway started with `pgo.enabled: false` answers `501 pgo_disabled` on every PGO route and links no NATS connection
   (asserted from the NATS server's connection count).
8. The gateway ClusterRole is unchanged:
   the golden test and the missing-verb variants of the gateway suite still hold with PGO enabled.
9. With `PROFGATE_JOBS` re-provisioned with a 1-minute TTL (the gateways restarted for this scenario only,
   since the bucket is recreated),
   the gateway exits non-zero naming the bucket and `TTL`.
   With `nats.credsFile` pointing at a user that can open every bucket but lacks, in turn,
   publish on `$KV.PROFGATE_JOBS.>`,
   publish on `$O.PROFGATE_ARTIFACTS.>`,
   and `$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>`,
   the gateway exits non-zero naming the bucket and the probe operation that failed,
   and no probe key or object remains.

Scenarios 1–5 run on every lane; the kind lanes do not need NetworkPolicy for NATS.

---

## 14. Dependencies

| Module | Purpose |
|---|---|
| `github.com/nats-io/nats.go` | KV and Object Store (only in `internal/natskv`); also the end-to-end harness |
| `github.com/google/pprof` | `profile.ParseData`, `profile.Merge`, `Compact`, and `Write` in `internal/pgo`; tests |
| `github.com/nats-io/nats-server/v2` | tests only: in-process JetStream |

Everything else is already in the gateway's table or the standard library.

---

## 15. Package Layout

```text
internal/natskv/     the NATS seam; sole non-test importer of nats.go; preflight and probes, KV, Objects
internal/admit/      the admission gate shared by interactive requests and Collection samples
internal/pgo/        policy layering and ceilings, identifiers, publisher (reservation counter and the three publication writes), scheduler, worker scan and owner loop, merge, sweeper, clock seam
internal/httpapi/    gains the seven PGO routes and their realm flags
internal/config/     gains nats, pgo, and realm pgo blocks
internal/metrics/    gains the PGO metrics
cmd/profgate/        wires NATS preflight, scheduler, worker, and sweeper when pgo.enabled
deploy/              gains the NATS account fragment, the bucket provisioning commands, the memory limit, and the example creds mount
test/e2e/            gains the NATS manifest, bucket provisioning, and scenarios 1–9
```

`internal/pgo` depends on `internal/natskv`, `internal/k8s`, `internal/proxy`, `internal/admit`, and `internal/metrics`;
nothing depends on it except `httpapi` and `cmd`.
`internal/admit` depends on nothing in the module.

---

## 16. Failure Scenarios

| Event | Behavior |
|---|---|
| NATS unreachable at startup with `pgo.enabled` | preflight retries forever; `/readyz` 503; interactive `/v1` routes serve once the Kubernetes side is ready; PGO routes `503 pgo_unavailable` |
| watches still replaying after preflight or after a reconnect | `/readyz` 200; scheduler, worker, and sweeper idle; PGO routes `503 pgo_unavailable` until every watch has delivered its replay marker under the current connection generation |
| a record's policy snapshot exceeds the claiming replica's ceilings | `failed limit_exceeded` on claim or reclaim, before any local slot is reserved; active key released |
| a bucket missing or of the wrong kind | process exits naming the bucket |
| a bucket with a TTL, memory storage, `Discard: old`, or a size limit below the contract | preflight reads the status; process exits naming the bucket and the field |
| a bucket reaches `MaxBytes` | the write fails `ErrUnavailable` under `Discard: new`; nothing already stored is evicted |
| on-demand creation faster than `onDemandPerMinute` | `429 rate_limited` before any write |
| NATS user lacks a permission | a preflight probe fails; process exits naming the bucket and the operation |
| NATS unreachable while running | PGO routes `503 pgo_unavailable`; scheduler creates nothing; an owner aborts once `leaseUntil - skewMargin` passes without a renewal; the disconnect moves the connection generation and clears the barrier before the connection is usable again; after reconnect the watches replay behind the barrier, then a scan reclaims |
| gateway crashes mid-Collection | lease expires; another replica's scan reclaims from round 0 with `attempt + 1` under a new object name |
| stale owner finishes after a reclaim completed | its update loses; it deletes only its own object; the committed artifact is untouched |
| owner's merge, serialization, or `Put` outlasts its committed lease | the owner issues no final update; the work finishes its current `Merge`/`Compact`/`Write`/`Put` and exits; its local slot is held until then; a scan reclaims |
| sweeper's cache lags a `completed` record after a NATS outage | the orphan rule reads the job fresh and keeps the named object |
| two creators for one Service at once | one wins `active.<ns>.<svc>`; the loser deletes its own `initializing` record; the scheduler records `busy`, the API answers `429 collection_in_progress` |
| creator dies between its three publication writes | the scan fails the `initializing` record `not_published` on its first pass after `createdAt + 1m + skewMargin` once a worker's cache holds the record, and releases the active key it names |
| Services the watched caches show as live plus local reservations at `maxLiveCollections` | the scheduler records `capacity`; `POST /collections` answers `429 capacity_exhausted`; no write |
| a record outgrows `maxRecordBytes` | `failed record_too_large` with the manifest counts and no samples; its object deleted |
| a sample body is gzip inside gzip | `sample_malformed`; the round continues |
| a claim's `Update` returns `ErrUnavailable` | nothing is profiled; if it committed, the lease lapses and a scan reclaims |
| two samples of one version with different sample types | the second is `incompatible_profile`; the round continues |
| owner wedged past `deadline` | the scan fails the record `deadline_exceeded`; the owner's next renewal stops it |
| no worker has capacity before `claimBy` | the scan fails the record `not_claimed`; the next slot tries again |
| a Service with more Pods than `maxTargetsPerRound` | each round samples a shuffled subset of that size; `truncated: true` in the manifest |
| the merge outgrows `maxMergedBytes` | `failed merged_too_large`; nothing stored |
| clocks disagree by more than `skewMargin` | a live Collection may be reclaimed early and run twice; the artifact is one attempt's bytes, never a mix |
| gateway crashes after `completed` | nothing is lost; any replica serves the artifact |
| every gateway down over several slots | one Collection per Service when they return, for the current slot only |
| policy changed while a Collection runs | the Collection keeps its snapshot; the next slot uses the new policy |
| rollout during a Collection | Pods of the new version are excluded; all rolled → `failed no_targets` |
| Kubernetes API unreachable during a round | confirmations fail `discovery_unavailable`; all-failed round → `failed no_samples` |
| object missing at download | `410 artifact_gone`; record flipped to `expired` |
| object expires while a download streams | connection closed; audit `artifact_stream_failed`; the client retries and gets `410` |
| a configuration writer raises every knob | ceilings reject the write; a ceiling lowered later makes the Service ineligible and visible in `violations` |
| two Collections on one gateway, both at `maxParallel` | at most `maxParallel × maxActiveCollections` slots held; interactive requests keep the rest |
