# Profgate PGO Collection

**Status:** Accepted

This document is the design of record for PGO collection,
built on the gateway defined in [`gateway.md`](gateway.md).
Everything there — permission boundary, discovery seam, HTTP API, realms,
configuration, testing — is assumed and not restated;
this document adds scheduling, collection, merge,
and the NATS state that coordinates gateway replicas.
Gateway sections are cited by heading name.
The original draft, `docs/specs/profgate-design.md`,
is superseded by this document and recoverable from git history.

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
and a rolling update of either Deployment is uneventful.

### 1.1 Core decisions

1. **CPU profiles only.**
   PGO consumes CPU profiles; a Collection's `profile` is always `cpu`.
2. **NATS JetStream for coordination and artifacts; nothing else.**
   `PROFGATE_CONFIG` and `PROFGATE_JOBS` are KV buckets;
   `PROFGATE_ARTIFACTS` is an Object Store bucket.
   No database, no PVC, no object storage service, no work queue.
3. **No leader, and two kinds of process.**
   The scheduler, worker, and sweeper run in a collector Deployment,
   started as `profgate collector` and defaulting to one replica.
   Gateway replicas run `profgate serve`, keep every PGO route,
   and publish the Collections `POST /collections` asks for.
   Neither kind is elected:
   `Create` and revision-conditional `Update` on KV keys decide who wins —
   between a gateway and a collector, and between two collectors,
   which a rolling update of a one-replica collector Deployment produces on purpose (section 2).
4. **No writable filesystem.**
   Samples are merged in memory as they arrive and the merged profile goes straight to the Object Store.
   The container stays exactly as hardened as the gateway spec's *Container* section describes.
5. **The Kubernetes permission boundary does not move.**
   Collections resolve and confirm targets through the same `Discovery` interface as interactive requests,
   always with the zero `PortSelection`, so they use the configured pprof port and offer no client selection,
   and fetch samples through the same proxy transport.
   No new RBAC tuple, no new `internal/k8s` method,
   and no Kubernetes permission the collector holds that a gateway replica does not.
6. **Runtime policy never exceeds static ceilings.**
   Operator defaults and hard limits are process configuration;
   NATS holds only per-Service overrides, each validated against the ceilings when written and when read.
7. **Build identity is the version label.**
   `Target.Version` from the gateway's *Version* section is the identity a Collection merges across;
   two versions never merge.
8. **Opaque Collection identifiers reveal nothing.**
   A realm that may not see a Collection receives `404`, never `403`.
9. **An operator picks a size, not twelve numbers.**
   `pgo.preset` is `small`, `standard`, or `large`,
   and each name fixes every ceiling in `pgo.limits` (section 11.1).
   A single `pgo.limits.<key>` overrides one ceiling for the case a preset does not fit.
   The collector's memory limit and termination grace period are computed from the result by the chart,
   never carried from a command's output into a manifest by hand.

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
  and only each process's own concurrency ceilings bound the total,
  which section 8.5 states as a per-Pod figure so an operator can check it against a workload.

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
+---------------+         +---------------+       +-------------------+
| profgate serve|         | profgate serve|       | profgate collector|
| httpapi       |         | httpapi       |       | scheduler         |
| PGO routes    |         | PGO routes    |       | worker            |
| publisher     |         | publisher     |       | sweeper           |
+---+-------+---+         +---+-------+---+       +--+----------+-----+
    |       |                 |       |              |          |
    |       +--------+--------+       |              |          |
    |                |                |              |          |
    |                v                v              v          |
    |            NATS JetStream                                 |
    |            KV  PROFGATE_CONFIG                            |
    |            KV  PROFGATE_JOBS                              |
    |            OBJ PROFGATE_ARTIFACTS                         |
    |                                                           |
    +---------------------------+-------------------------------+
                                |
  Kubernetes API (read)  ->  Service -> EndpointSlice -> Pod -> PodIP:pprofPort
```

The three long-lived loops are a **scheduler** that turns per-Service policy into Collections,
a **worker** that claims and executes them and revisits stalled ones,
and a **sweeper** that expires artifacts and deletes old records and orphaned objects.
All three run in the collector process and nowhere else.
A gateway replica adds to the gateway the PGO routes on its API listener,
the watched caches those routes read — the collector heartbeat of section 7.5 among them,
which is how a replica knows whether anything is there to run what it publishes —
and the **publisher** that `POST /collections` writes a Collection through (section 7.2);
the ops listener of both processes gains metrics.
Neither process is elected and neither knows the other exists:
they meet only in the three NATS stores.

**Why the loops moved out of the gateway.**
A gateway replica exists to answer profile requests,
is sized for streaming a profile through, and is scaled for request load.
A worker holds whole decoded profiles in memory,
which is why the memory figure of section 3.4 is measured in gibibytes,
and it drains on `SIGTERM` on a different timescale from an HTTP request.
Running both in one process multiplies that memory figure by the replica count,
makes every gateway replica's termination grace period a function of the PGO ceilings,
and puts a merge in the same heap as the request path.
Separating them costs one Deployment and buys three things:
the gateway's memory limit and grace period stop depending on `pgo.limits` entirely,
sampling can no longer take an admission slot an interactive request wanted (section 8.5),
and the number of processes that run schedulers and sweepers stops tracking the number of processes that serve requests.
It is not a security change:
the same binary, the same ServiceAccount and its seven read tuples, the same NATS account, and the same three stores
(section 3).

**Why the collector is one replica by default, and why coordination stays.**
The chart value `pgo.collector.replicaCount` — a chart value, not a configuration key — defaults to `1`.
One collector saturates nothing:
a Collection is minutes long, a slot is hours wide, and the sweeper is one pass a minute.
A second replica buys continuity while the first is unschedulable — a drained node, an evicted Pod —
and costs one more `List` and one more scan per pass; it is a supported value, not a recommended one.
None of the coordination this document specifies is removable at one replica,
because one replica is not one process:

- The chart renders the collector with the default `RollingUpdate` strategy,
  where a one-replica Deployment computes `maxSurge: 1` and `maxUnavailable: 0`,
  so an upgrade starts the new collector *before* the old one terminates.
  Two collectors overlap on every `helm upgrade`.
  `Recreate` would avoid the overlap and silently drop whatever slot fell in the gap instead;
  the overlap is the better failure because the mechanisms below already absorb it.
- The slot key's `Create` is what keeps that overlap from firing one slot twice.
- The active key is contended by a gateway and a collector, not by two collectors,
  so it is required at any collector count above zero:
  `POST /collections` on a gateway and the scheduler in the collector are two creators of the same record shape.
- The lease is what makes a restart safe rather than merely survivable.
  A collector that dies mid-Collection leaves a `running` record whose partial merge died with it;
  its successor must reclaim that record as a *new attempt* rather than resume it,
  and must not reclaim one whose owner is still alive on the other side of a rolling update.
  Both are the lease's job.
  The artifact's attempt fencing (section 8.6) does the rest:
  it keeps a drained predecessor's late `Put` out of the successor's committed record.
- The orphan and `not_published` sweeps clean up after publications that died mid-write,
  and those publications happen in gateway replicas, whose count is not one.

What a rule forbidding a second collector would remove is the claim race between two workers over one `pending` record —
and that race is resolved by the same revision-conditional `Update` every other transition already uses,
so forbidding it would delete no mechanism while making a routine upgrade unsafe.
The recommendation is therefore to keep all of it and to run one replica.

None of the loops acts, and no PGO route reads or writes state,
until that process's NATS watches have completed their initial replay (section 5.1, "The replay barrier").

When `pgo.enabled` is false, the default, none of this exists at runtime:
the gateway opens no NATS connection and every PGO route answers `501 pgo_disabled`,
and no collector Deployment is rendered.

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
The contract cuts the other way for the collector heartbeat of section 7.5,
which wants a key that goes stale:
the seam exposes no per-key expiry and the bucket may carry no TTL,
so that key states its own freshness in its value
and every reader judges it by comparing that timestamp with the clock, never by the key's presence.
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
Before a gateway replica serves a PGO route or a collector starts its loops,
the process connects, opens all three stores,
checks the configuration contract above,
and exercises every operation it will later need with reversible probes, under one 30-second deadline per bucket:
in each KV bucket, a `Watch` on the key `probe.<instanceID>` is opened first,
then `Create`, `Update` at the returned revision, `Get`, and `Delete` of that key run in order,
and the watch must deliver all three revisions — the create, the update, and the delete — before it is closed,
each awaited in turn before the next write runs,
because a bucket with a history depth of one drops the previous revision on the next write
and a watch that has not delivered it yet then never will;
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
Both processes run the same image under the same hardened context,
and everything in this section applies to each of them.
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
The collector Deployment's memory limit is the sum of two terms:

```text
collectorBaseMemory + maxActiveCollections × (maxParallel × decodeFactor × maxSampleBytes
                                              + 2 × decodeFactor × maxMergedBytes)
```

`collectorBaseMemory` is a fixed **256 MiB**.
It covers what the process holds before it decodes anything:
the Go runtime and its heap floor, the three Kubernetes informer caches,
the proxy transport's connection pool and buffers, and the NATS client with its four watches.
It is the same figure for every preset, because none of those grows with a PGO ceiling.

The second term is the PGO working set, with `decodeFactor = 8` and every term at its `pgo.limits` ceiling:
per active Collection, every in-flight sample as its compressed body, its decompressed bytes,
and its decoded `*profile.Profile`;
the running merged profile in decoded form;
and the serialized copy written to the store at completion.
The input bytes are bounded exactly (section 8.5);
the decoded representations are not, so `decodeFactor` is an engineering estimate —
two buffers of input plus about six times that in decoded structures —
and the whole limit is a sizing rule, not a proof.
Under `pgo.preset: standard` the working set is 2 × (4 × 8 × 32 MiB + 2 × 8 × 64 MiB) = 4 GiB
and the limit is 256 MiB + 4 GiB = 4352 MiB;
section 11.1 gives both figures for each preset.

**The multiplication is checked, in the binary and in the chart alike.**
Configuration validation forms the product under an overflow check:
an overflow is a validation failure naming the four ceilings that produced it,
never a wrapped or negative byte count.
The chart's own arithmetic already refuses the same case.
At the ranges section 11 publishes the product cannot reach that bound —
`maxActiveCollections` at 64 over the largest admissible ceilings is about 9 TiB,
orders of magnitude inside a 64-bit byte count —
so the check guards a later widening of a range rather than a value an operator can set today,
and the test that exercises it feeds a ceiling the range check itself refuses,
which is why the two layers are asserted separately (section 13.1).

**The chart owns the arithmetic and refuses to be told the answer twice.**
It computes the limit from `pgo.preset` and the `pgo.limits` overrides it was given
and renders it as the collector's `resources.limits.memory`,
so the limit cannot drift from the ceilings it was sized for.
The binary applies `PROFGATE_*` overrides on top of the file,
so a variable naming a sizing ceiling would move it after the chart had computed the limit
and leave the container sized for ceilings it no longer runs under.
The chart therefore rejects, at render time, in the collector's `extraEnv`:
`PROFGATE_PGO_PRESET`, and the four sizing variables
`PROFGATE_PGO_LIMIT_MAX_PARALLEL`, `PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES`,
`PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES`, and `PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS`.
It rejects `config.pgo.preset` and the same four keys under `config.pgo.limits` in the raw configuration block,
for the same reason:
that block is merged after the chart has read the structured values,
so a value there would size the configuration differently from the limit.
Each rejection names the structured value to set instead.
`profgate config validate` prints the same figure for the loaded configuration,
and a test in `deploy/` compares the rendered limit against what the binary computes,
which is what keeps the chart's copy of the arithmetic equal to the binary's.

A gateway replica holds no decoded profile:
its limit is the gateway's own static figure whether `pgo.enabled` is true or false,
and no `pgo.limits` key appears in it.

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
A compromised collector reaches exactly as far and no further:
it is the same image, the same ServiceAccount, and the same NATS user,
so splitting the loops out of the gateway moves the boundary in neither direction.
The collector opens no API listener,
which removes the routes an attacker could reach it through but grants it nothing new.

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
| `docs/specs/gateway.md`, *Request algorithm* step 9 | "Acquire one of `limits.maxConcurrentProfiles` slots without waiting" | "Acquire one of `limits.maxConcurrentProfiles` slots from the admission gate (`internal/admit`) without waiting" |
| `docs/specs/gateway.md`, *Package Layout* | (tree) | add `internal/admit/  the admission gate interactive requests pass through` |
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

// Client is the connection; Preflight returns it and the rest of the process consumes it.
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
After preflight, the PGO runtime opens the watches its process needs.
Each role opens four, and they are not the same four:

| Prefix | Bucket | Collector | Gateway replica |
|---|---|---|---|
| `service.*` | `PROFGATE_CONFIG` | yes | yes |
| `job.*` | `PROFGATE_JOBS` | yes | yes |
| `active.*` | `PROFGATE_JOBS` | yes | yes |
| `schedule.*` | `PROFGATE_JOBS` | yes | no |
| `collector.*` | `PROFGATE_JOBS` | no | yes |

A gateway replica opens no `schedule.*` watch,
because slot keys are read only by the scheduler and the sweeper, and both run in the collector.
A collector opens no `collector.*` watch,
because the heartbeat there is a key it writes for gateway replicas to read (section 7.5)
and nothing in the collector decides from another collector's copy of it.
Three prefixes are common to both roles, and each role adds one of its own.
The barrier, `pgoSynced`, is defined as `Synced(gen)` for the current generation `gen = Generation()`:
true only once every watch this process opened has delivered its marker under that generation.
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
(a watcher that was cut off has an unknown gap behind it, so the seam re-opens every one of them).
Every watched cache is rebuilt from the replay rather than patched,
and marker entries and cache contents carry the generation they were delivered under,
so a cache is either complete as of a point in the stream under the current generation or not consulted at all.

Every scheduler tick, worker scan, sweeper pass, owner loop, publisher pass, and state-touching PGO handler begins with

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
nothing in either process decides from a cache that has not yet seen the bucket.
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
| one live Collection per Service | `active.<ns>.<svc>` | `Create` after the `initializing` record exists and before it becomes `pending`; `Delete` at its revision after the terminal update | the creator whose `Create` succeeds, scheduler or API alike; every other creator observes `ErrKeyExists` and answers `429 collection_in_progress`, or, carrying an idempotency key, resolves through the receipt below (section 10.2) |
| live Collections cluster-wide | the set of Services with an `active.*` key or a nonterminal `job.*` record | counted from the watched caches (`cachedLive`) plus that publisher's local reservations for publications the caches have not delivered yet, before a creator writes anything, and only behind the replay barrier; no cluster-wide primitive, so the ceiling `pgo.limits.maxLiveCollections` is per publisher, giving `publishers × maxLiveCollections` (section 7.2) | — |
| Collection ownership and every state transition | `job.<id>` | `Update` with the revision last read | the replica whose read was most recent; the loser re-reads |
| policy override | `service.<ns>.<svc>` | `Create` for a new key; `Update` with the client's `If-Match` revision | the client whose ETag is current |
| artifact bytes | `<id>-<attempt>.pprof` | `Put` by the owner of that attempt, then named in the `completed` `Update` | the attempt whose `Update` wins; every other attempt's object is unreferenced and is deleted by its writer or by the sweeper |
| one collector's liveness | `collector.<instance>` | `Create`, then `Update` at the revision it last wrote | nobody contends: `<instance>` is unique per process, so the key has exactly one writer for its whole life |
| what one idempotency key created | `idem.<hash>` | `Create` by the winner of `active.<ns>.<svc>`, before that winner's record becomes claimable; `Delete` at the revision the deleter read, once the record it names is gone | three writers that cannot meet: the winner, which holds the active key and is the only creator publishing in that scope, and which deletes a stale receipt at the revision a fresh `Get` returned only after that `Get` of the receipt's record found it absent, then creates its own; a keyed request that found the receipt stale; and the sweeper. The second and third act only on a receipt whose record a fresh `Get` shows absent, and each deletes at the revision it read, so a loser of that `Delete` does nothing (section 10.2) |

Nothing takes a snapshot for reassurance, pre-checks before a conditional write, or reads one replica's memory to decide about another.
The receipt read a keyed `POST /collections` makes before publishing is not such a pre-check:
it decides whether to publish at all rather than predicting whether a conditional write will win,
and a read that misses a receipt written since costs a lost `Create` of the active key,
which the loser then resolves from the receipt (section 10.2).

### 5.3 Paths that touch each key

Each path below runs in one kind of process.
The scheduler, the worker's scan and claim, the owner loop, and the sweeper run in the collector;
every route handler, and the publisher those handlers write through, runs in a gateway replica;
the publisher also runs in the collector, where the scheduler writes through it.
Where a rule below says "the creator", it holds for both.

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
| publisher, each pass | watched cache, then `Get` of the key (the release rule, section 7.2) | nothing |
| `POST /collections` with an `Idempotency-Key`, after a lost `Create` | `Get` of the key, for the id it holds, only while the winner's receipt has not landed (section 10.2) | nothing |
| owner loop finish, `POST /cancel`, worker scan | own last read of the key | `Delete` at its revision after the terminal `Update` of `job.<id>` succeeds, only when the key's `id` is that Collection |
| sweeper | watched cache, then `Get` of the named job | `Delete` at its revision when the job is absent or terminal; an `initializing`, `pending`, or `running` job keeps it |

`job.<id>` in `PROFGATE_JOBS`:

| Path | Reads | Mutates |
|---|---|---|
| scheduler, `POST /collections` | watched caches (`cachedLive` plus local reservations) | `Create` (state `initializing`) before `active.<ns>.<svc>`; then `Update` `initializing` → `pending` after winning it; `Delete` at its revision when the active create loses |
| publisher, each pass | watched cache, then `Get` of `job.<id>` (the release rule, section 7.2) | nothing |
| `POST /collections` with an `Idempotency-Key` | `Get` of the record `idem.<hash>` names, and of the record an active key names while that receipt has not landed (section 10.2) | nothing |
| `GET /collections/{id}?wait=` | `Get`, then the watched cache's signal for that id, then one `Get` per state change (section 10.4) | nothing |
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

`idem.<hash>` in `PROFGATE_JOBS`:

| Path | Reads | Mutates |
|---|---|---|
| `POST /collections` with an `Idempotency-Key` | `Get` of the key, before anything is published | `Create` after winning `active.<ns>.<svc>`; `Delete` at the revision its `Get` returned when the record the receipt names is gone |
| sweeper | the record it is deleting; hourly, `Keys("idem.")` and a `Get` of each name, for the receipts that delete missed | `Delete` right after the record it names is deleted, and `Delete` at its revision for an aged receipt whose record a fresh `Get` finds absent |

No watch is opened on the prefix and no cache holds it:
a receipt decides whether a Collection is created,
so every read of one is authoritative and a replica's watch set stays the four of section 5.1.
The key lives in `PROFGATE_JOBS`, which the account fragment of section 3.3 grants as `$KV.PROFGATE_JOBS.>`,
so the prefix adds no NATS permission.

`collector.<instance>` in `PROFGATE_JOBS`:

| Path | Reads | Mutates |
|---|---|---|
| collector heartbeat | own last revision | `Create`, then `Update` every `leaseTTL / 3`; `Delete` at its revision on a graceful shutdown |
| `POST /collections` on a gateway | watched cache, for the freshest `writtenAt` any key carries | — |
| gateway metric loop | watched cache | — |
| sweeper | watched cache | `Delete` at its revision once `writtenAt + 10m + skewMargin` has passed |

The key belongs to no Service, so it is invisible to `cachedLive`, to the worker's scan,
and to every rule the sweeper applies to `job.*`, `active.*`, and `schedule.*`.
`<instance>` is the owner instance of section 8.2 — a Pod name, a `/`, and the process's random suffix —
whose characters are all in the set NATS accepts (section 7.1).

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
A ceiling bounds what a Collection may ask for; it no longer bounds how long a Deployment takes to drain,
because the collector's drain is bounded by the lease instead (section 12.4).

**One rule judges an effective policy against itself: `artifact.retention ≥ schedule.every`.**
A retention shorter than the interval leaves the Service with no downloadable artifact for the tail of every interval,
which is the state a build asking for the newest profile finds.
Every ceiling in `pgo.limits` bounds one field on its own,
so no combination of them can express this:
a preset admits `schedule.every: 24h` beside `artifact.retention: 1m`
because each value sits inside its own range.
The rule is therefore stated and validated on the effective policy, in five places:

- **configuration-default validation** — `pgo.defaults.artifact.retention ≥ pgo.defaults.schedule.every`,
  checked the way every other cross-field rule in the `pgo` block is,
  whether or not `pgo.enabled` is true, so a file that cannot produce a coherent default fails at startup;
- **`PUT /pgo`** — the effective policy the override produces, `400 limit_exceeded` naming both fields;
- **`POST /collections`** — the snapshot the body produces, the same answer;
- **scheduler evaluation** — a stored override that violates it makes the Service ineligible for scheduling,
  logged once per revision at warning level,
  and appears in `GET /pgo`'s `violations` beside a ceiling violation,
  because an operator whose Service silently stopped being scheduled has nowhere else to read why;
- **worker claim and reclaim** — before any local capacity is reserved,
  a snapshot that violates it ends the Collection `failed limit_exceeded`,
  exactly as a ceiling violation does.

Layering is one level deep per block (section 6.2),
so an override that sets only `schedule.every` is judged against the default `artifact.retention`,
and one that sets only `artifact.retention` against the default `every`;
the rule reads the effective policy, never the override, which is what makes that work.
`pgo.limits.maxRetention ≥ pgo.limits.maxEvery` stays as the ceiling rule beside it (section 11.1):
it is what guarantees a long enough retention is *available* under every preset,
which is a different claim from every effective policy actually using one.

**What `400 limit_exceeded` says in machine form.**
Validation produces one violation per field it refuses,
naming the field, the ceiling crossed, and a human-readable detail;
`GET /pgo` already publishes those as `violations`.
A refusal carries the same violations as the `details` array of the gateway spec's *Errors* section, one item apiece.
`field` is the policy field written as a pointer — `schedule.every` becomes `/schedule/every` — and `code` is one of:

| `code` | The value |
|---|---|
| `above_maximum` | is above the ceiling the message names |
| `below_minimum` | is below the floor the message names |
| `out_of_range` | is outside a range whose two ends are one rule, as `sampling.roundInterval` has |
| `not_permitted` | is not one of the fixed values the field admits, as `target.versionPolicy` has |
| `retention_under_interval` | is an `artifact.retention` under its own policy's `schedule.every`, the rule above |

The last one is reported on `/artifact/retention`,
the field the message asks the writer to raise, with `schedule.every` named in the message beside it.
`message` keeps the text it writes today — the two values and the configuration key of the ceiling —
and stays free to change.
Each `violations` entry of `GET /pgo` carries the same `code` beside its `field`, `ceiling`, and `detail`,
so a refused write and a Service that quietly stopped being scheduled report in one vocabulary rather than two.

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
Two collector versions that follow this contract contend on one key for one slot;
an encoding left to the implementation could let two versions fire the same slot twice,
which is exactly what a rolling update of the collector Deployment puts in front of it (section 2).

Every collector replica computes the same fire time from the same inputs,
so jitter spreads Services across the interval without spreading collectors apart.

A collector considers only the slot containing `now`.
Missed slots are never caught up:
a collector that returns after a day creates at most the Collection for the current slot,
and only if its fire time has passed.

### 7.2 Algorithm

Every 10 seconds, on every collector replica:

```text
gen := c.Generation(); if !c.Synced(gen): return
jobs := c.View(gen).Jobs                                        // ErrUnavailable on a moved generation ends the tick
for each (ns, svc) with an override in the watched PROFGATE_CONFIG cache:
    policy := effective(defaults, override)
    if !policy.Enabled or policy fails effective-policy validation (a ceiling or the retention rule): continue
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
    publish(ns, svc, origin=schedule, claimBy = now + every, key="", principal="")   // section "Publishing a Collection"
```

**Publishing a Collection.**
The scheduler and `POST /collections` publish a record through a `publisher`,
one per process in both kinds of process,
which holds the reservation counter of the live-Collection ceiling (below)
and performs the same writes for both — three, and a fourth for a request that carried a key:

```text
publish(ns, svc, origin, claimBy, key, principal):          // caller holds one reservation; key is empty for a schedule
  id := newID()
  rev, err := jobs.Create("job.<id>", record with state = initializing, origin, configRevision,
                          the policy snapshot, snapshotHash, claimBy, key, principal)
  if err is ErrUnavailable: publisher.Track(ns, svc, id); log or 503; return   // indeterminate; resolved later
  if err != nil: publisher.Release(); log or 503; return    // nothing exists yet
  err = jobs.Create("active.<ns>.<svc>", {"id": id, "createdAt": now})
  if err is ErrKeyExists:
      derr := jobs.Delete("job.<id>", rev)                  // own record, own revision
      if derr is ErrUnavailable: publisher.Track(ns, svc, id)   // indeterminate: the record may still be initializing
      else: publisher.Release()                             // deleted, or already gone
      record busy / answer 429 collection_in_progress,
      or, for a request that carried a key, resolve it as a loser does (section 10.2); return
  publisher.Track(ns, svc, id)                              // released only by the release rule, section "The live-Collection ceiling"
  if err is ErrUnavailable: log or 503; return              // indeterminate: the key may exist; the scan fails the record later
  if key != "":                                             // the receipt, before the record becomes claimable
      rerr := jobs.Create("idem.<hash>", {"id": id, "snapshotHash": snapshotHash, "createdAt": now})
      if rerr is ErrUnavailable: rerr = the same Create, once more   // one retry behind the same generation
      if rerr is ErrKeyExists:
          existing := jobs.Get("idem.<hash>")
          if existing.id == id: rerr = nil                  // the earlier attempt landed
          else if jobs.Get("job." + existing.id) is absent:  // a stale receipt: its record is gone
              jobs.Delete("idem.<hash>", existing.revision), then Create again
          else: rerr stays ErrKeyExists                     // a live receipt for another create; withdraw below
      if rerr != nil:                                       // withdraw: nothing keyed becomes claimable unbound
          jobs.Delete("job.<id>", rev); jobs.Delete("active.<ns>.<svc>", the revision Create returned)
          answer 503 pgo_unavailable; return
  record.state = pending
  _, err = jobs.Update("job.<id>", record, rev)
  if err != nil: log or 503; return                         // the key exists; the release rule resolves the reservation
  record won / answer 202
```

An `initializing` record is never claimable; the worker ignores it.
The receipt is written before the record becomes claimable,
so a Collection a caller can poll is a Collection whose key is already durable.
The order is what closes the publication race:
the active key never exists without its job record already existing,
so a sweeper that reads the job named by an active key finds `initializing` or later, never nothing,
and keeps the key.
A creator that dies between the writes leaves an `initializing` record alone,
that record plus an active key naming it,
or those two plus the receipt of section 10.2;
the worker scan fails any `initializing` record on its first pass after `createdAt + 1m + skewMargin`
once that worker's watched cache holds the record, with reason `not_published`
(no bound holds while NATS is unavailable or a watch is replaying),
and then runs `releaseActive` for it, which deletes the active key only if it names that id.
A creator whose active create loses deletes its own `initializing` record at the revision it holds;
if that delete returns `ErrUnavailable` the reservation stays tracked
and the scan fails the record the same way if it still exists.
A loser writes no receipt.
A receipt outlives its record only until the next request carrying that key cleans it,
or until the sweeper's own pass does (section 8.9),
so a receipt never names a Collection that does not exist for longer than one of those passes.
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
a collector still on the old `every` and one on the new compute different slot keys,
and both slot creates can win,
but only one of them wins the active key;
the other records `busy` and its slot key is consumed.
The Collection records the revision it was created from (section 8.2),
and the next slot uses whatever is current then.

**The live-Collection ceiling.**
The active key bounds one Service to one live Collection;
`pgo.limits.maxLiveCollections` bounds the cluster.
Every process that can publish runs one `publisher` that keeps a local reservation counter `reserved`:
in a collector it is the scheduler's,
in a gateway replica it is the `POST /collections` handler's.
**Publishers** below means the two counts added — gateway replicas plus collector replicas —
because the ceiling is enforced against one publisher's own view.
`Reserve()` computes `cachedLive + reserved`, where
`cachedLive` is the number of Services that have, in this process's watched caches,
an active key or a nonterminal job record (`initializing`, `pending`, or `running`),
and `reserved` counts this process's publications for which neither cache has delivered anything yet.
A Service the caches already show as live is refused without a reservation
(the scheduler records `busy`, the API answers `429 collection_in_progress`).
At or above the ceiling `Reserve()` refuses — the scheduler records `capacity` and the API answers `429 capacity_exhausted`, writing nothing —
otherwise it increments `reserved` and the publication proceeds.

**The release rule.**
A reservation for publication `(ns, svc, id)` is released, checked on every publisher pass, as soon as either:

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
The publisher keeps its unresolved reservations in a list
and runs its own **pass** over that list every 10 seconds, behind the replay barrier and through one `View(gen)`:
it evaluates each reservation, cache first, then the fresh reads,
and leaves the reservation in place while neither observation has been made.

**`Publisher.Run(ctx)` is that timer, and both subcommands start it.**
The loop ticks every 10 seconds until `ctx` ends and performs one pass per tick;
`profgate serve` starts it whenever `pgo.enabled`, and `profgate collector` starts it beside the three loops.
The scheduler does not own the pass and does not call it from its tick:
a gateway replica publishes and runs no scheduler,
so a reservation held there by an indeterminate write would never be re-evaluated,
and a handful of them would leave that replica answering `429 capacity_exhausted` for the life of the process.
Making the pass a step of the scheduler tick would be indistinguishable from this in a collector
and broken in a gateway replica,
which is why the wiring is asserted per subcommand rather than inferred from the algorithm (section 13.1).

The invariant the rule keeps:
a process publishes nothing before its watches have completed their initial replay (section 5.1),
so after any restart every nonterminal record and every active key in the bucket is in `cachedLive` before the first publication;
a record or key written after that is counted by `cachedLive` once delivered
and, in the window before delivery, by the reservation of the publisher that wrote it —
and if that process dies inside the window, its replacement replays the record before publishing;
and a publisher never publishes for a Service it counts as live.
Each publisher therefore contributes at most `maxLiveCollections` Services that nobody has yet seen,
live Services never exceed `publishers × maxLiveCollections`,
and records that are not terminal never exceed `publishers × maxLiveCollections`
plus at most one further record per Service per publisher left by an indeterminate publication —
at most `2 × publishers × maxLiveCollections` —
under any combination of committed and uncommitted creates, a frozen watch, and a restarted publisher,
because a restarted publisher is behind the barrier until the record its predecessor left has replayed.
Stale `initializing` records are drained by the scan (`not_published`) and are counted until then.
The counter survives passes and covers scheduler ticks and API requests alike; there is no per-tick count.
`claimBy` bounds how long a `pending` record waits and the `not_published` rule bounds an `initializing` one,
so a backlog that forms when creation outpaces workers drains into `not_claimed` failures instead of growing.
The arrival rate is bounded separately:
the scheduler creates at most one record per Service per `every`,
and on-demand creation is rate-limited per gateway replica (section 7.3),
so creations per minute are at most `gatewayReplicas × onDemandPerMinute` plus the scheduled ones.
The worker scan of section 8.3 visits at most `2 × publishers × maxLiveCollections` records per pass,
and the sweeper's `Get` per active key is at most `publishers × maxLiveCollections` per pass.

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
so a caller with `pgo.collect` across many Services cannot create records faster than `gatewayReplicas × onDemandPerMinute` per minute.
It takes no slot and is unaffected by `enabled`;
the scheduler and the API are two creators of the same record shape through the same active key.
It answers `429 capacity_exhausted` when the publisher's `Reserve()` refuses (section 7.2), writing nothing,
and `429 collection_in_progress` when the active create loses,
whether the live Collection came from the scheduler or from another request;
the watched cache is consulted first only to answer without a write when the answer is already known.
Concurrent requests for one Service therefore yield exactly one `202`,
and concurrent requests carrying one idempotency key yield one `202` and a `200` naming that same Collection
(section 10.2).

### 7.4 Slot retention

Every slot key carries `retainUntil = slot + every + 24h`, computed from the `every` that created it,
and the sweeper deletes the key only after `retainUntil` has passed.
A slot can be attempted only while `now` lies inside it,
and `every` is at most `pgo.limits.maxEvery` (24 hours),
so by the time a key is deleted its slot ended at least a day earlier
and no replica, whatever its policy now says, can attempt that slot again.
The value, not the current policy, decides the key's lifetime:
lowering `every` after the fact cannot shorten the retention of a key created under a longer one.

### 7.5 Collector availability

A gateway replica accepts a Collection that only a collector can run.
With `pgo.enabled` and no collector, every acceptance is a record nobody will ever claim:
it sits `pending` until `claimBy`, and even the failure that follows is a worker's write,
so nothing fails it either — the record simply waits, the API keeps answering `202`,
and readiness stays green on both sides.
The gateway has to be able to tell that state from a healthy one, and a heartbeat is what tells it.

**The key.**
Each collector writes `collector.<instance>` in `PROFGATE_JOBS`,
`Create` on the first write and `Update` at the revision it last wrote thereafter,
every `leaseTTL / 3` — 20 seconds at the shipped lease, the interval an owner loop already renews on.
The value is `{"instance": ..., "pod": ..., "writtenAt": <RFC 3339>}`.
The write runs behind the replay barrier and through the pass's `View(gen)` like every other store call;
a failed write is logged and retried on the next tick, and nothing else in the collector waits on it.

**Freshness is read from the value, not from the key's presence.**
The seam exposes no per-key expiry, and section 3.2 forbids a bucket-wide TTL,
so a key does not disappear when its writer does.
A gateway replica therefore treats a collector as present only while

```text
writtenAt + 2 × (leaseTTL / 3) + skewMargin > now
```

which is 45 seconds at the shipped 60-second lease:
two write intervals, so one missed write does not report an outage, plus the clock-skew margin.
An absent key and a stale one are the same answer, which is what keeps the design off any cleanup path:
a collector that dies writes nothing further and goes stale on its own, whatever happens to its key afterwards.
`profgate_pgo_collector_available` is `1` while that replica's watched `collector.*` cache holds such a key,
and `0` otherwise.
The chart's `PrometheusRule` alerts on that gauge reading `0` for five minutes with `pgo.enabled`,
which is the shape of "collection has silently stopped" an operator would otherwise learn from a stale artifact.

**What refuses, and what does not.**
`POST /collections` answers `503 collector_unavailable` when no key is fresh, writing nothing:
the request asks for work, and there is nobody to do it.
`PUT /pgo` and `DELETE /pgo` still succeed, and the scheduled Collections they describe start arriving
as soon as a collector does; policy is data, and storing it needs no collector.
`GET /pgo`, the two listing and record routes, `GET .../profile`, and `POST .../cancel` are unaffected too:
each reads or writes state a gateway replica owns end to end.
The check runs after the replay barrier:
a replica whose `collector.*` watch is still replaying answers `503 pgo_unavailable` for the barrier's own reason,
and never reports a false absence.

**Cleanup, which nothing depends on.**
A collector deletes its own key at its revision during a graceful shutdown, best effort.
The sweeper deletes any `collector.*` key whose `writtenAt + 10m + skewMargin` has passed,
the same shape and the same age as the probe sweep (section 8.9).
Both are tidiness: a stale key already reads as absent, so neither delete changes an answer.

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
    "artifact": {"retention": "24h"}
  },
  "state": "running",
  "attempt": 1,
  "owner": {"instance": "profgate-collector-7f88fdf79-xabcd/q2w3e4r5", "pod": "profgate-collector-7f88fdf79-xabcd"},
  "claimBy": "2026-08-23T13:00:00Z",
  "leaseUntil": "2026-08-23T12:06:12Z",
  "deadline": "2026-08-23T12:36:43Z",
  "reason": "",
  "resolvedVersion": "1.42.3",
  "progress": {"round": 1, "rounds": 2, "samplesOK": 5, "samplesFailed": 0},
  "manifest": null,
  "artifact": null,
  "idempotencyKey": "",
  "snapshotHash": "9c1e5b0a4d7f2836a0b91c4e6d8f0a2b3c5d7e9f1a2b3c4d5e6f708192a3b4c5",
  "createdBy": "schedule",
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
| `owner` | the claiming collector: `instance` is the Pod name plus a per-process random suffix, `pod` the Pod name |
| `claimBy` | a `pending` record not claimed by this time is failed `not_claimed`; `createdAt + every` for `schedule`, `createdAt + 1h` for `api` |
| `leaseUntil` | the owner's claim is valid until this time |
| `deadline` | set at first claim from the policy snapshot and the ceilings, never from the live target count: `startedAt + rounds × batches × (duration + 30s) + (rounds − 1) × roundInterval + 60s`, where `batches = ceil(min(replicas, maxTargetsPerRound) / maxParallel)` with `all` read as `maxTargetsPerRound`; no term for waiting on admission, because a sample never waits (section 8.5) |
| `reason` | why a `failed` or `cancelled` Collection ended (section 8.6) |
| `resolvedVersion` | the version the first round settled on |
| `progress` | the owner's last renewal snapshot; informational |
| `manifest` | section 9 |
| `artifact` | `{"object": "<id>-<attempt>.pprof", "bytes": 123456}`; set only by the `completed` update, so it names exactly the object that update committed |
| `idempotencyKey` | the `Idempotency-Key` the creating request sent; empty for a scheduled Collection and for a request that sent none (section 10.2) |
| `snapshotHash` | SHA-256, as 64 lowercase hexadecimal characters, of the canonical encoding of `policy`; what an idempotent replay compares (section 10.2) |
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
the fixed fields, the policy snapshot, and the manifest's own scalars are under 8 KiB with every name at its limit,
the 128-byte idempotency key and the 64-character snapshot hash included;
the whole record stays under 210 KiB.
`maxRecordBytes` is a fixed 512 KiB, leaving that arithmetic a margin of more than two:
the owner loop serializes the record before every `Update`
and fails the Collection with `record_too_large` instead of sending a value the 1 MiB default NATS message limit could reject,
which leaves a reader a terminal record rather than a wedged one.
The `record_too_large` terminal record omits `manifest.samples` and keeps the manifest's counts,
so it is itself small and its `Update` cannot fail for the same reason.

### 8.3 Claim

The worker in every collector replica watches `job.*`.
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
and the ceilings that validated it then are not the ceilings of whichever collector claims it now —
a preset changed under a rolling update, or a restart with a smaller one, can leave `pending` records
whose `maxParallel`, `duration`, `rounds`, or `maxTargetsPerRound` exceed what this collector was sized for,
including records a gateway replica published from ceilings the collector no longer holds.
Such a record is failed `limit_exceeded` by the first worker that meets it and its active key released;
it never reserves a local slot and never samples,
so the bound of section 8.5 holds for the ceilings actually in force, not the ones at creation.
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
so another collector may start the Service's next Collection while this one's work drains;
that is within the memory figure of section 3.4, which is per collector replica.

Cancellation (section 8.7) reaches the owner the same way:
the cancel handler's conditional update advances the revision,
the owner's next renewal or final update fails with `ErrRevisionMismatch`,
and the owner re-reads the record once to log the reason before it stops.
The owner never re-reads during a successful renewal and the work goroutine never touches KV,
so the worst-case latency from cancellation to the owner stopping is one renewal interval, `leaseTTL / 3`.

### 8.5 Rounds

```text
for round in 0..rounds-1:
    targets := discovery.Targets(ns, svc, PortSelection{})  // gateway eligibility rules, from the cache, on the configured port
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
Section 3.4 budgets the collector with an engineering factor, stated as an estimate and not a proof.
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
`upstream_<status>`, `sample_too_large`, `sample_malformed`, `parse_failed`, `incompatible_profile`.
A failed sample is recorded and skipped; only a round with zero successes fails the Collection.

`replicas: all` means every eligible Pod up to `pgo.limits.maxTargetsPerRound`;
a Service with more Pods than that is sampled from a different shuffled subset each round,
and the manifest records `truncated: true` so a reader knows the artifact is a sample of the fleet.
The cap is what bounds the manifest, the running merge, the deadline, and the memory figure of section 3.4.
The running profile is checked after every merge against `pgo.limits.maxMergedBytes`
(the serialized size, which is also the size of the object that would be stored);
a Collection that outgrows it fails `merged_too_large` rather than exhausting the collector.

**Sampling takes no admission slot.**
A collector runs at most `pgo.limits.maxActiveCollections` Collections,
each with at most `maxParallel` samples in flight,
so the number of profile fetches one collector has open is at most `maxParallel × maxActiveCollections` —
bounded by construction, with nothing to wait on and no slot to run out of.
`internal/admit` stays what it was for the gateway,
the semaphore that holds interactive requests to `limits.maxConcurrentProfiles` and fails them fast at capacity;
the collector does not use it,
and the waiting `Acquire` the gate grew for samples goes away with them, leaving `TryAcquire` alone.

The alternative is one gate the two share,
sized by a cross-key rule
(`pgo.limits.maxParallel × pgo.limits.maxActiveCollections < limits.maxConcurrentProfiles`)
whose whole job is to stop sampling from holding every slot an interactive request wanted,
with a sample that loses the race waiting and failing `slot_timeout` when the wait runs out.
That is what a process serving requests while it collects profiles needs,
and a collector is not one:
sampling cannot take a slot from a request the process never receives,
so the guarantee is structural rather than arithmetic.
A gate that cannot block prevents no failure,
so there is none — and with it go the cross-key rule, the `slot_timeout` sample reason,
and any deadline term for waiting on admission.

The bound is on how many profiles run at once, not on which Pods they hit.
Within one Collection a Pod is sampled at most once per round, because a round's target list is deduplicated by Pod UID;
two Collections for different Services that select the same Pod, on one collector or on two,
and an interactive request against it, may all profile it at the same time.
There is no per-Pod exclusion, and nothing here claims one.

**What one Pod can therefore receive at once.**
The per-Pod figure is smaller than the per-collector one, because a Collection counts once against it:
a round samples each Pod at most once, so a Collection contributes at most one open fetch to a given Pod,
not `maxParallel`.
With `C` collector replicas overlapping — one in steady state, two through a rolling update —
PGO directs at most `C × maxActiveCollections` simultaneous fetches at one Pod,
which is 2 in steady state under `standard` and 4 while an upgrade overlaps.
Interactive traffic adds at most `gatewayReplicas × limits.maxConcurrentProfiles` on top,
because each gateway replica's admission gate is its own.
Both terms are ceilings a Pod hosting several Services can meet at once,
and neither is a reservation a Service holds:
a Pod selected by ten Services still receives at most `C × maxActiveCollections` PGO fetches,
because that is how many Collections can be live on collectors at all.
The figures are the operator's to check against what a workload tolerates;
the design bounds them and does not arbitrate between Services (section 1.2).

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
Collector A owns C1, round 2 of 3
Collector A dies
leaseTTL + skewMargin passes; no KV write happens
Collector B's scan revisits the running record, reads it fresh, sees leaseUntil + skewMargin < now
Collector B claims: attempt 2, from round 0, object name <id>-2.pprof
```

At-least-once, from the beginning, because the partial merge lived in A's memory.
`pgo.maxAttempts` bounds it;
the claim that would exceed it marks the record `failed` with `attempts_exhausted` instead.
Because Collections are minutes long, the repeated profiling is small.

B is the other collector during a rolling update, or the restarted collector afterwards;
the path is the same either way,
which is why a one-replica collector Deployment needs the lease as much as a two-replica one.
An upgrade that interrupts a Collection therefore costs it one attempt,
and two upgrades inside one Collection reach `attempts_exhausted` at the shipped `maxAttempts: 3`.

A `running` record past its `deadline` and a `pending` record past its `claimBy` are failed by the scan (section 8.3),
not reclaimed.

### 8.9 Sweeper

Every 60 seconds, in every collector replica, behind the barrier and through one `View(gen)` for the pass,
over the watched `job.*` and `schedule.*` caches and one `artifacts.List`:

| Condition | Action | Primitive |
|---|---|---|
| `completed` and `expiresAt + skewMargin < now` | `artifacts.Delete(artifact.object)` (absent is success), then `state = expired` | `Update` at the cached revision |
| `completed` and `artifacts.Get(artifact.object)` is `ErrObjectNotFound` | `state = expired` | `Update` at the cached revision |
| `expired`, `failed`, or `cancelled` and `finishedAt + pgo.jobRetention + skewMargin < now` | delete the record, then delete `idem.<hash>` when the record carried a key | `Delete` at the cached revision, then `Delete` at the receipt's revision |
| slot key with `retainUntil + skewMargin < now` | delete the key | `Delete` at its revision |
| object `<id>-<attempt>.pprof` not named by any `completed` record in the cache, `ModTime + 10m + skewMargin < now` | `jobs.Get("job.<id>")`; delete the object only when the record is absent, or terminal with `artifact.object` naming something else | `Get`, then `artifacts.Delete` |
| `active.<ns>.<svc>` key | `jobs.Get` of the job it names; delete the key when that job is absent or terminal; `initializing`, `pending`, and `running` keep it | `Get`, then `Delete` at the key's revision |
| `idem.*` key, on the reconciliation pass below, whose value's `createdAt + pgo.jobRetention + skewMargin` is past | `jobs.Get` of the record it names; delete the receipt only when that record is absent | `Get` of the receipt, `Get` of the record, then `Delete` at the receipt's revision |
| `collector.*` key whose `writtenAt + 10m + skewMargin` is past | delete, no lookup | `Delete` at the cached revision |
| `probe.*` key whose `Entry.Created`, or `probe-*` object whose `ModTime`, plus `10m + skewMargin` is past | delete, no lookup | `Delete` |

A `completed` record is never deleted directly:
it becomes `expired` first, which deletes its object, and is deleted `jobRetention` after it finished.
Configuration validation requires `pgo.jobRetention ≥ pgo.limits.maxRetention + 1h`,
so a record always outlives its artifact:
an object is unreferenced only when its attempt lost or its record has been deleted,
and a record is deleted only after its object.
The 10-minute age lets a slow `Put` finish before its `completed` update names it.

**The receipt reconciliation runs hourly, not every sweep.**
Receipts are removed by the record-deletion rule above, which knows the record and computes its receipt key from it.
The reconciliation exists for the receipts that rule did not reach —
a delete that returned `ErrUnavailable`, or a process that stopped between the two —
and it is the one condition here with no watched cache behind it:
no watch is opened on `idem.*` (section 5.3),
so the pass must `Keys("idem.")` and then `Get` each name to see the `createdAt` its value carries.
That is a read per receipt in the bucket, which is why it runs on one sweep in sixty rather than on every one.
A receipt whose record is still there is left alone, and a young one is left alone without a record lookup.
Nothing depends on the timing: a receipt outliving its record is invisible to every other rule,
and the next request carrying its key cleans it in passing.

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
Every condition but the receipt reconciliation is evaluated against the watched caches
and one `List` of the artifact bucket;
the NATS calls an ordinary sweep makes are that `List`,
one `Get` per orphan candidate and per active key,
one `Get` of each record crossing `pgo.jobRetention` and one of its receipt when it names a key,
and one `Delete` per matching key or object.
The two record-retention reads are forced:
the receipt key is computed from the record's own `createdBy` and `idempotencyKey`,
and nothing indexes an idempotency key.
Orphan candidates are objects whose attempt lost or whose record is gone, a handful at most;
and active keys are at most `publishers × maxLiveCollections` (section 7.2).
The hourly reconciliation adds one `Keys("idem.")` and one `Get` per receipt in the bucket,
which is the number of keyed Collections inside `pgo.jobRetention`,
plus one record `Get` for each receipt already past that retention.
Per collector replica per minute that is at most one list, one read per Service with a live Collection,
and the number of records, slot keys, and objects that crossed their threshold in that minute,
which under steady load is the number of Collections finishing per minute,
not the number stored.
The cost grows with collector replicas only in the lists and the per-Service reads, once per replica per minute.

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
  "gateway": "profgate-collector-7f88fdf79-xabcd/q2w3e4r5",
  "samples": [
    {"round": 0, "pod": "payment-api-7c8f8c9b9-a", "podUID": "3c1e…", "node": "worker-07", "startedAt": "2026-08-23T12:03:13Z", "result": "ok", "bytes": 48211},
    {"round": 0, "pod": "payment-api-7c8f8c9b9-b", "podUID": "9a0f…", "node": "worker-02", "startedAt": "2026-08-23T12:03:13Z", "result": "upstream_timeout", "bytes": 0}
  ]
}
```

The `gateway` field holds the instance that ran the Collection, which is a collector Pod name plus its random suffix;
the field keeps its name, because renaming it would change a `completed` record every client already reads.
No Pod IP or port appears in it.
It answers "is this profile safe for build X" (version, label, per-sample identity),
"why is it smaller than expected" (failed samples),
and "is this the whole fleet" (`truncated`, set when a round had more eligible Pods than it sampled).

---

## 10. HTTP API

All routes are on a gateway replica's API listener, under `/v1`, realm-checked, with `Cache-Control: no-store`.
The collector opens no API listener and serves none of them;
every route below reads or writes NATS directly, from the gateway process, and reaches no collector.
The gateway spec's *Request algorithm* applies with these additions:
the method step accepts the methods listed per route,
the realm step evaluates the realm's `pgo` flags after namespace and Service,
a **JSON media type** step immediately after the method step answers `400 invalid_parameter`
when a `POST` to a write route declares no JSON media type (*Request media type*),
and a **PGO availability** step between readiness and credential placement answers,
in this order,
`501 pgo_disabled` when `pgo.enabled` is false,
and `503 pgo_unavailable` when the NATS connection is down.
The full order every route here runs is the one all four accepted designs state identically:
route, method, JSON media type, readiness, PGO availability, credential placement, authentication, realm.

| Route | Methods | Realm flag |
|---|---|---|
| `/v1/namespaces/{ns}/services/{svc}/pgo` | `GET` | `pgo.read` |
| `/v1/namespaces/{ns}/services/{svc}/pgo` | `PUT`, `DELETE` | `pgo.configure` |
| `/v1/namespaces/{ns}/services/{svc}/collections` | `GET` | `pgo.read` |
| `/v1/namespaces/{ns}/services/{svc}/collections` | `POST` | `pgo.collect` |
| `/v1/namespaces/{ns}/services/{svc}/collections/latest` | `GET` | `pgo.read` |
| `/v1/namespaces/{ns}/services/{svc}/collections/latest/profile` | `GET` | `pgo.read` |
| `/v1/collections/{id}` | `GET` | `pgo.read` |
| `/v1/collections/{id}/profile` | `GET` | `pgo.read` |
| `/v1/collections/{id}/cancel` | `POST` | `pgo.collect` |

For the three `/v1/collections/{id}` routes the record is read first
and the realm is evaluated against the record's namespace and Service.
A record the realm denies, and a record that does not exist, both answer `404 collection_not_found`.
The identifier is opaque, so this leaks nothing the realm would hide.

[`cli.md`](cli.md) *Collections* is the first-party client that drives these routes,
which changes nothing above: it sends what any client sends.

`latest` is a path segment under a Service, never an identifier.
The two routes that carry it name a Service and are realm-checked against it like every other Service-scoped route,
so the identifier grammar of section 8.1 is untouched
and `/v1/collections/latest` stays `404 route_unknown`.

Request bodies are JSON, at most 64 KiB, decoded with unknown fields rejected (`400 invalid_parameter`).
An unknown field carries a `details` item with code `unknown_field` and a pointer to it,
a body over the limit or one that is not JSON carries `body_malformed`,
and a body sent to a route that accepts none carries `body_not_allowed`
(the gateway spec's *Errors* section defines the vocabulary).

**Request media type.**
A `POST` to `.../collections` or to `/v1/collections/{id}/cancel` must declare `Content-Type: application/json`,
with or without a body.
An absent header is `400 invalid_parameter` with a `details` item naming it under `header_required`;
every other refusal is the same status under `header_malformed`.
The header is parsed with `mime.ParseMediaType`:
the essence must be `application/json`, and every parameter it returns is accepted and ignored,
`charset` among them, so no client is refused over a parameter the route does not read.
A header that is repeated, that does not parse, or whose essence is anything else is refused.

The check is its own step, immediately after the method step and before readiness:
before `pgo_disabled`, before the replay barrier, and so before authentication, the realm, and every store call.
Ordering it there leaks nothing.
The answer is decided by the request's own headers and by the route grammar the client already holds,
and it is the same answer for every caller —
authenticated or not, admitted by a realm or denied,
on a gateway with collection enabled or disabled, and on one whose caches have not synced.
What it buys is that a request another origin could have produced dies before anything reads a credential:
an HTML form can send only `application/x-www-form-urlencoded`, `multipart/form-data`, or `text/plain`,
and a cross-origin `fetch` that sets the header earns a preflight the gateway answers no CORS header to
([`ui.md`](ui.md) *Starting and cancelling a Collection*).
`PUT /pgo` needs no such rule:
no form can issue a `PUT` at all, and the `fetch` that could is preflighted the same way.

`state` is a closed set for this release:
the values listed in section 8.2 are exhaustive, and adding one is a spec change.
The `state=` filter of section 10.3 takes exactly those values and refuses every other.
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
    "artifact": {"retention": "24h"}
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

`If-Match` is a quoted decimal revision; any other form, including `*`, is `400 invalid_parameter`,
with a `details` item naming the header under `header_malformed`.

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
The snapshot is validated as an effective policy — every ceiling and the retention rule (`400 limit_exceeded`).
With no fresh heartbeat in the watched `collector.*` cache,
the handler answers `503 collector_unavailable` and writes nothing (section 7.5):
the record would sit `pending` with nobody to claim it, and no worker would even fail it.
That check runs before the on-demand token bucket,
so a caller who is both rate-limited and collector-less reads the durable problem rather than the transient one;
a replica whose bucket is then empty answers `429 rate_limited`, also before any write (section 7.3).
When the publisher's `Reserve()` refuses — Services the watched caches show as live plus this replica's reservations at `maxLiveCollections` — the handler answers `429 capacity_exhausted` and writes nothing (section 7.2).
A Service that already has a live Collection answers `429 collection_in_progress`:
the handler's `Create` of `active.<ns>.<svc>` loses (section 7.3),
or the watched cache already shows the key and the write is skipped.

**Which refusal carries `Retry-After`.**
One does: the keyed create that loses the active key and finds no receipt yet,
which is answered `429 collection_in_progress` with `Retry-After: 1`
because a retry one second on reads the receipt or creates, as **How a loser resolves** below has it.
`429 rate_limited` and `429 capacity_exhausted` carry no `Retry-After`,
and neither does the `429 collection_in_progress` a replica writes
when its watched cache already shows the Service live.
A client reads the header where it is present and assumes a delay of its own where it is not:
the console disables the control for five seconds
([`ui.md`](ui.md) *Starting and cancelling a Collection*).

Before creating the record the handler resolves targets once, as round 0 would, with the zero `PortSelection`,
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

**The `Idempotency-Key` header.**
The create commits before it answers,
so a caller whose response never arrived holds no identifier for a Collection that may be running.
A blind retry is answered `429 collection_in_progress`,
which says neither whether that live Collection is the one this caller created nor what to poll.
The header closes that:

```http
POST /v1/namespaces/payment/services/payment-api/collections
Content-Type: application/json
Idempotency-Key: 3f0a1c7e-8b52-4d6a-9f11-2c4e6a8b0d31
```

It is optional.
When present it is 1 to 128 bytes drawn from `[A-Za-z0-9._-]`,
the grammar the gateway spec's *Request identifier* section uses;
anything else — empty, longer, a byte outside the set, or the header sent twice —
is `400 invalid_parameter` with a `details` item naming it under `header_malformed`.
A key the gateway cannot read is refused rather than replaced, unlike a request identifier:
this header decides whether a Collection is created,
so guessing at it would be guessing at that.

**It is scoped to the principal, the namespace, and the Service.**
Two principals sending one key are asking for two different things,
and a key used on one Service says nothing about another.
The scope decides a lookup and never a message:
no answer tells a caller that another principal used its key,
and two principals sending one key each get their own Collection without either learning of the other.

**Where the key is answered from.**
One key in one scope names one Collection, and that binding is a key of its own in `PROFGATE_JOBS`:

```text
idem.<first 32 hexadecimal characters of SHA-256(scope)>
scope = len(principal):principal len(namespace):namespace len(service):service len(key):key
```

The value is `{"id": ..., "snapshotHash": ..., "createdAt": ...}` and nothing else.
Each field is written as its byte length, a colon, and its bytes,
so no value can be read as part of the one beside it
and a principal carrying any separator character cannot borrow another's scope;
32 hexadecimal characters are 128 bits, which no caller can collide by choosing keys.
The key is hashed rather than spelled out because a principal is arbitrary text
and a NATS subject token is not, and because a bucket key is not the place to publish a caller's name.
The receipt is what a later request reads.
The record still carries `idempotencyKey` and `createdBy` (section 8.2) —
a reader of a record sees what created it — but no lookup scans records for them,
and the `active.<ns>.<svc>` value carries neither: nothing reads them there.
The scheduler writes no receipt: a scheduled Collection carries no key, so no replay can match one.

**Every keyed request reads the receipt authoritatively.**
Behind the replay barrier, the handler issues one `Get` of `idem.<hash>` before it publishes anything.
A cache miss is never proof of absence:
no watch is opened on the prefix, nothing indexes it, and the read is the store's answer or `ErrUnavailable`.
The receipt names an identifier, so the answer costs at most one further `Get` of `job.<id>`:

- **A receipt whose record exists** is a replay when its `snapshotHash` equals this request's,
  and `409 idempotency_mismatch` when it differs —
  unless that record is `failed` with reason `not_published`,
  which never became claimable and never ran.
  A receipt is written before the `pending` update,
  so a creator that dies in that window leaves a receipt naming an `initializing` record
  that the scan then fails `not_published`.
  Such a receipt is stale in the same sense as one whose record is gone:
  the handler deletes it at the revision its own `Get` returned,
  and then creates as a request with no history does.
- **A receipt whose record is absent** is stale:
  the record reached its retention and was deleted before its receipt followed it.
  The handler deletes the receipt at the revision its own `Get` returned,
  and then creates as a request with no history does.
  Losing that `Delete` means another request cleaned it,
  or a create has already written a newer one, and the handler re-reads once.
- **No receipt** is a request with no history, which creates.

**A keyed record never becomes claimable without its receipt.**
The winner's receipt `Create` is the write that releases the Collection:
the `initializing → pending` update that makes the record claimable follows a receipt the store acknowledged, never precedes it.
A receipt `Create` that returns `ErrUnavailable` is retried once behind the same generation;
a `Create` that finds the key already holding this identifier is the earlier attempt having landed, and counts as success.
When the receipt still has not landed, the winner withdraws:
it deletes its `initializing` record at the revision it holds, deletes the active key at the revision its `Create` returned,
and answers `503 pgo_unavailable`, which promises the caller that nothing runs.
A creator that dies between the active key and the receipt leaves an `initializing` record and an active key that names it;
the loser's fallback below and the `not_published` scan both read that state as the unfinished create it is,
and nothing claims it.
A withdraw that loses its record `Delete` has been overtaken by the scan,
which failed the record `not_published` first; the winner still deletes the active key and still answers `503`.
The only records this window leaves behind are therefore an `initializing` one
and a `failed` one whose reason is `not_published`,
and neither ever ran: no worker claimed it and no sample was taken.
A retry that finds no receipt creates anew, and so does one whose receipt names the `not_published` record,
which is a first Collection for that key rather than a duplicate.
No `pending` or later non-terminal record carries a key its receipt does not bind.

**Where the lookup sits in the handler.**
The handler decodes the body and builds the snapshot it would produce, and reads the receipt next:
before the ceiling refusal, the token bucket, the collector check, the advisory target resolution,
and the reservation.
A replay creates nothing, so none of the bounds those steps hold apply to it.
Those steps would answer a retry `429 rate_limited`, `429 capacity_exhausted`, or `503 collector_unavailable`,
withholding an identifier the caller already owns,
which is the failure this header exists to prevent;
or `400 limit_exceeded` because a ceiling moved in between,
refusing a Collection that is already running.
A ceiling bounds a request that would create something, and a replay would not.
`409 idempotency_mismatch` is answered from the same place and for the same reason: neither answer writes.
The snapshot is built before the lookup because the comparison needs it,
and a violation in it is reported only once the lookup has found no replay to answer with.

**What a replay answers.**
`200`, with the create acknowledgement the first answer carried —
`{"id": ..., "state": ...}`, the state read from the record — and the same `Location` header.
Not `202`: this Collection was accepted earlier, and the answer reports it rather than accepting it again.
Not the stored record either.
`POST .../collections` is a `pgo.collect` route and `GET /v1/collections/{id}` is a `pgo.read` one,
and the two flags are independent ([`ui.md`](ui.md) *Starting and cancelling a Collection*),
so answering a replay with the full record would hand a collect-only principal the manifest and the placement
that its realm denies on the route that serves them.
Principal equality is not authorization.
A caller that holds `pgo.read` fetches the record by the identifier this answer carries,
and one that does not gets the identifier, which is what it came back for.

**What is not a replay.**

- The same key with a request that means something else is `409 idempotency_mismatch`, and nothing is written.
  "Something else" is the canonical effective-policy snapshot:
  the handler encodes the snapshot this body would produce in the canonical form of *Record*,
  hashes it, and compares that hash with the receipt's `snapshotHash`.
  Bytes, whitespace, and field order in the request therefore decide nothing,
  and identical JSON can still mismatch:
  a retry sent after the Service's stored override changed, or after the operator defaults moved under it,
  produces a different effective policy and so a different hash.
  That is the right answer rather than an unlucky one:
  the key would otherwise stand for two Collections that differ.
- A different key while a live Collection holds the Service is `429 collection_in_progress`, exactly as today.
- No key at all is today's behavior, unchanged.

**The conditional write is the one that was already there.**
The `Create` of `active.<ns>.<svc>` (section 5.2) still decides which of two creators publishes;
this contract adds one key and no lease, no lock, and no second decision.
The winner writes the receipt immediately after winning and before its record becomes claimable
(section 7.2), so the binding is durable before anything can act on the Collection.
A receipt `Create` that finds the key already there is looking at one the winner's own read did not see,
which only a stale receipt can be:
the winner deletes it at the revision it reads and writes its own.

**How a loser resolves.**
The loser deletes its own `initializing` record at the revision it holds, as section 7.2 already has it,
and reads `idem.<hash>` again.
A receipt is the answer, under the rules above.
No receipt means the winner has not written one yet —
it holds the active key and its record is not yet claimable —
or will never write one, because it carried a different key or none.
The loser therefore reads the active key it lost to and the record that key names, once each,
and answers `429 collection_in_progress` with `Retry-After: 1` whatever that record carries:
an `initializing` record is a create still deciding whether it will be released or withdrawn,
and an identifier handed out from it could name a Collection the winner deletes a moment later.
The caller's retry, one second on, finds the receipt and is answered from it,
or finds nothing and creates, because the winner withdrew.
Those two reads cover the publication window and nothing else, and they never answer `200`.
Every later retry — a new request, seconds or hours afterwards, on any replica — reads the receipt,
which outlives the active key and depends on no cache.
That is what makes the promise below hold where reading the active key alone could not:
a winner that becomes terminal and deletes its active key takes nothing with it.

**What the guarantee is.**
A retry carries the key of a create whose answer was lost,
and it is answered with that Collection's identifier for as long as the record exists.
The receipt is deleted after the record it names, by the sweeper (section 8.9),
so there is no moment at which the record is present and its key resolves to nothing.
No watch, no cache, and no replica's replay lag stands between the retry and the answer:
the read is authoritative on every request.
What ends the promise is the record's own retention, which is when there is nothing left to name.

### 10.3 List Collections

```http
GET /v1/namespaces/payment/services/payment-api/collections
```

```json
{
  "namespace": "payment",
  "service": "payment-api",
  "collections": [
    {"id": "7h2k9m4p6r8t0v1w3x5y", "origin": "schedule", "state": "completed", "attempt": 1, "resolvedVersion": "1.42.3", "createdAt": "2026-08-23T12:03:12Z", "finishedAt": "2026-08-23T12:05:40Z", "expiresAt": "2026-08-24T12:05:40Z"}
  ]
}
```

Read from the watched cache, newest first:
by `createdAt` descending, and by `id` descending where two records share an instant.
The identifier is random rather than time-ordered (section 8.1),
so that second key is what makes the order total;
without it two records created in the same instant could swap places between two reads,
and a cursor would skip one or repeat it.
Each entry is the shape above and gains no field.

The endpoint takes these parameters and no others:

| Parameter | Grammar | Meaning |
|---|---|---|
| `state` | one of the states of section 8.2, repeatable | keep records in any of the named states; absent keeps every state |
| `since` | RFC 3339 timestamp | keep records whose `createdAt` is at or after it |
| `origin` | `schedule` or `api` | keep records of that origin |
| `limit` | decimal integer, 1 to 100 | return at most this many entries; absent means 100 |
| `cursor` | an opaque token a previous response carried | continue after the entry that token names |

`origin` takes the record's own two values rather than prettier ones.
Every entry serializes the field as `schedule` or `api` (section 8.2),
and a filter whose vocabulary differed from the field's would trap the client that reads both.

Filters are applied first, then the cursor, then `limit`.
Parameters are validated in name order, so a query with several faults reports the same one every time.
A name the table does not hold, a repeated `since`, `origin`, `limit`, or `cursor`, an empty value,
a `state` outside the closed set, a `since` that is not RFC 3339, a `limit` outside 1 to 100,
a cursor that does not decode,
and a cursor whose filters differ from the request's,
are each `400 invalid_parameter`,
carrying the `details` item the gateway spec's *Errors* section defines.

**The cursor.**
A response with more entries to give carries `nextCursor` beside `collections`;
one that reached the end of the listing omits the field rather than sending it empty.
The token encodes the `createdAt` and the `id` of the last entry the response carried,
and the `state`, `since`, and `origin` filters the request that produced it carried.
It is opaque: a client that parses it reads an encoding this document does not promise.

A page is the entries that sort after that pair, compared by value rather than by position.

**The cursor carries its filters, and a page must repeat them.**
A cursor is `400 invalid_parameter`, with a `malformed_parameter` item naming `cursor`,
when it is presented beside a `state`, `since`, or `origin` set other than the one it was minted under.
The position is a point in one filtered listing.
Reading it against another would skip records the second listing holds before that point,
and repeat none of them —
a silent wrong answer where a refusal costs the client one corrected request.
`limit` is not part of the token: it bounds a page and not the order.

**What the order promises.**
For the records present when paging started, `(createdAt, id)` is a total order,
so a client walking the pages sees each of them once.
A record created while the client pages carries a fresh `createdAt` and is normally newer than any cursor it holds,
but two records can share an instant,
and a random identifier that sorts below the cursor's puts such a record on a page the client has passed.
The promise is therefore the one the mechanism keeps:
stable order over the records the listing already held, and no guarantee about records inserted mid-walk.
Records the sweeper deletes stop appearing,
and a cursor naming a record that has since been deleted still works,
because nothing looks that record up — the pair is the position.

This replaces the flat cap of at most 100 entries with nothing behind them.
For a Service collected hourly that cap was under five days of a `jobRetention` of a week,
and a client had no way to ask for the rest.
`limit` still stops at 100:
a page is built from a cache in memory and returned in one response,
and a client that wants more asks again with the cursor.

### 10.4 Get a Collection

```http
GET /v1/collections/7h2k9m4p6r8t0v1w3x5y
```

`200` with the full record of section 8.2, as stored.
It contains no Pod IP;
the owner instance is a collector Pod name plus a random suffix,
which a realm that may read PGO state may know.

The endpoint takes one parameter and no others:

| Parameter | Grammar | Meaning |
|---|---|---|
| `wait` | a duration, `1s` to `60s` | hold the request open until the record moves, or until the wait ends |

**Long polling.**
Without `wait` the answer is the record as read, which is what a client on a timer already gets.
A wait turns on two reads of the record, made in this order:
a Collection-scoped route has already read the record to evaluate the realm against its namespace and Service,
and that read is the state a wait compares against.
With `wait` the handler then registers first and reads second:
it registers its channel under the record's identifier,
issues one authoritative `Get`,
and answers at once when that read is terminal
or shows a `state` other than the one the realm read returned.
Registering before the read is what closes the lost-wakeup window —
a transition that lands between the two is delivered to a channel that already exists,
where a handler that read first would park until its deadline over a change that had already happened.
Comparing against the realm read is what keeps a transition inside that same window visible:
against the handler's own first read it would not be,
because that read has already absorbed it and the transition would show as the state the client already had.
Otherwise it holds the request until the first of these:

- an authoritative read after a pulse shows a `state` other than the one the realm read returned:
  a `pending` that became `running` answers, as does any terminal state;
- that read finds the record gone,
  which answers `404 collection_not_found` exactly as a plain `GET` of a deleted record does;
- `wait` elapses, which brings the handler back to one authoritative `Get` exactly as a pulse does,
  and that read is the answer;
- the client disconnects, and nothing is answered (audit `client_gone`);
- the replica begins draining (below);
- the connection generation moves, which ends the wait with `503 pgo_unavailable`.

**Every move a wait reports comes from a read taken after it.**
That is one rule and not two: a pulse ends the wait with a read, and so does the deadline,
so the endings above and the pulse rules below say the same thing.
A deadline answered from an earlier read would turn a dropped pulse into a wrong answer rather than a delay —
a terminal transition whose pulse a full buffer coalesced away,
or one that becomes ready in the same instant as the timer,
would be reported as the state before it.
The two endings that answer without a read are the two where a read cannot be taken:
a draining replica answers with the record it last read (below),
and a connection-generation move answers `503 pgo_unavailable`.

Only a change of `state` ends a wait.
An owner renews its lease every `leaseTTL / 3` and writes `progress` with it,
so a wait that answered on any write would answer every twenty seconds with a record that had not moved.
A client that wants progress reads it from the record each answer carries.

Each of these is `400 invalid_parameter`,
with a `malformed_parameter` item naming `wait`,
or, for a repetition, the item the gateway spec's *Errors* section defines:
a value that is not a duration, one at or below zero, one above `60s`, an empty one, and a repeated `wait`.
A value above the grammar is refused rather than clamped.
The route table above states the grammar as `1s` to `60s`,
and a parameter that silently becomes another value teaches a client that its input was accepted as sent.

**Where the wake-up comes from.**
The handler subscribes to the `job.*` cache this replica already watches:
one channel registered under the record's identifier,
signalled for each entry applied for it, and removed when the request ends.
No NATS request is issued for a waiting client, no consumer is created, and the seam of section 5.1 gains no method —
the fan-out is over entries one watch already delivers,
so a hundred waiting clients cost a hundred channels and no additional traffic to the store.
**A pulse is a hint and never the answer.**
Every pulse is followed by one authoritative `Get`, and the handler decides from that read alone,
never from what the pulse carried and never from the cached entry.
A pulse therefore needs no payload, and coalescing costs nothing:
a channel whose buffer is full drops the pulse rather than blocking `apply`,
and the next pulse — or the wait's own deadline — brings the handler back to a read
that sees whatever the bucket holds by then, including a terminal state two writes arrived for at once.
The one thing a dropped pulse can cost is latency inside the wait, never a wrong answer.

**The generation is a channel too.**
A connection-generation change is broadcast on a channel of its own,
which the handler selects on beside the drain signal and the request's own cancellation.
The seam of section 5.1 exposes `Generation()` as a value, which a parked handler cannot read again on its own;
without the broadcast a wait would sit out an entire outage and answer from a view the store had moved past.
The broadcast carries no state: it ends the wait with `503 pgo_unavailable`, below.

**The response.**
Identical to the plain `GET` — the same statuses and the same record,
with `404 collection_not_found` for a record the store lacks or the realm denies — plus one header:

```http
X-Wait-Elapsed: 12.480
```

Decimal seconds with millisecond precision,
on every answer to a request that carried a `wait` the gateway accepted, `0.000` included.
A client can tell a wait that ran from one that never started without timing the request itself.

**A wait holds nothing.**
It takes no admission slot: it fetches no profile, merges nothing, and reaches no Pod.
What it holds is one parked goroutine, one registered channel, and its own connection,
for at most the minute the ceiling allows.
That ceiling is the bound: no client parks a connection here for longer,
and every wait ends on its own without a second request.

**Draining ends a wait.**
When the replica begins draining — the moment `/readyz` turns 503, before `server.drainDelay` —
every waiting request answers with the record it last read,
`X-Wait-Elapsed` naming how long it had waited.
The gateway spec's *Startup and shutdown* section states it from the drain's side:
a poll that could otherwise outlast the whole drain window ends with the window, so the drain bound does not move.

**A wait that loses the store ends `503 pgo_unavailable`.**
The record the handler read was what the bucket held,
but what `wait` promises is that the answer is current when it arrives,
and a replica whose watches are replaying under a new generation cannot keep that promise.
The client retries, as it does for every other `pgo_unavailable`.

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

### 10.6 The latest completed Collection

```http
GET /v1/namespaces/payment/services/payment-api/collections/latest
```

`200` with the full record of section 8.2,
written exactly as `GET /v1/collections/{id}` writes it,
for the newest `completed` Collection of that Service whose artifact is still there.
`404 collection_not_found` when the Service has none.

```http
GET /v1/namespaces/payment/services/payment-api/collections/latest/profile
```

The artifact of that same record, streamed under the rules, the headers, and the table of section 10.5,
minus the two rows the selection below has already ruled out:
a record this route answers for is `completed` with its object present.

**Only a `completed` record with its bytes is the latest one.**
A `running` Collection has no artifact and an `expired` one no longer has its bytes,
so neither is what a caller asking for the latest wants:
the endpoint exists to hand a build the newest profile it can actually download.
A Service whose newest Collection failed answers with the completed one before it when there is one,
and `404 collection_not_found` when there is not.
That one `404` covers a Service that has never collected and a Service whose artifacts have all expired,
because a build has the same thing to do about either.

**Selection walks candidates rather than picking one.**
A record can be `completed` and its object gone —
the sweeper deletes on schedule and section 10.5 answers `410 artifact_gone` for exactly that —
so choosing the newest `completed` entry from the cache and answering with it would hand a build a `410`
while an older, intact artifact sat behind it.
Both routes therefore run one selection:

1. take the Service's records from the watched cache,
   `completed` ones only, newest first under the order of section 10.3;
2. for each candidate in turn, `Get job.<id>` authoritatively —
   a cache is a candidate filter here as it is for the sweeper —
   and drop it when the fresh read shows any state but `completed`;
3. confirm the object the record names is in the store;
   when it is absent, flip that record to `expired` with an `Update` at the revision the fresh read returned,
   the same conditional update `GET /v1/collections/{id}/profile` performs, and continue with the next candidate;
4. answer from the first candidate that survives all three.

`404 collection_not_found` is the answer when no candidate does.
`ErrUnavailable` at any point is `503 pgo_unavailable`, not a fall-through to an older Collection:
a store the gateway cannot read says nothing about which artifact is newest.
The walk costs one `Get` per candidate it discards, which is the number of artifacts
that expired since the cache last moved — normally none, and bounded by the page cap of section 10.3.
`latest` and `latest/profile` share the selection,
so the record one answers with is the record whose bytes the other streams,
and neither can answer `410 artifact_gone` while an intact artifact exists.

**Retention is what makes the endpoint useful.**
For a scheduled Service it answers with the previous interval's profile until the next one completes,
which holds only while an artifact outlives the interval that produced it.
That is the effective-policy rule of section 6.3, `artifact.retention` at least `schedule.every`,
and the shipped defaults sit well inside it: a 24-hour retention against a six-hour interval.
Without the rule, a Service could collect hourly and retain for a minute,
and then answer `404` for fifty-nine minutes of every hour —
the state that rule exists to prevent, now with an endpoint that would have shown it.

**What it saves, and what it does not change.**
A build that wanted the newest profile listed the Service's Collections, picked the newest `completed` entry,
and then downloaded it; these two routes are that walk, done by the gateway from the cache it already holds.
Both are Service-scoped routes, realm-checked on namespace, Service, and `pgo.read` before anything is read,
so a caller learns nothing it could not have learned from the listing it replaces.
The audit record names the identifier that answered, so a reader still sees which Collection a build took.

### 10.7 Cancel

```http
POST /v1/collections/7h2k9m4p6r8t0v1w3x5y/cancel
```

`200` with the updated record;
`409 collection_initializing` while the record is still being published (nonterminal; the client retries after a second);
`409 collection_terminal` when already terminal.
No body is accepted (`400 invalid_parameter` if one is sent).

### 10.8 Errors

Added to the gateway's table; same envelope, same rule that `code` is the contract.

| Status | `code` |
|---|---|
| 400 | `limit_exceeded` |
| 403 | `config_api_disabled` |
| 404 | `collection_not_found`, `pgo_override_not_found` |
| 409 | `version_conflict`, `version_missing`, `collection_not_completed`, `collection_initializing`, `collection_terminal`, `idempotency_mismatch` |
| 410 | `artifact_gone` |
| 412 | `precondition_failed` |
| 428 | `precondition_required` |
| 429 | `collection_in_progress`, `rate_limited`, `capacity_exhausted` |
| 501 | `pgo_disabled` |
| 503 | `pgo_unavailable`, `collector_unavailable` |

Of the codes in this table, only `429 collection_in_progress` ever carries `Retry-After`,
and only on the one path section 10.2 names; every other code here carries none,
`429 rate_limited`, `429 capacity_exhausted`, `503 pgo_unavailable`, and `503 collector_unavailable` included.

`collector_unavailable` is a code of its own rather than a second meaning for `pgo_unavailable`,
because the two say different things to a caller and to an alert:
`pgo_unavailable` means this gateway replica cannot reach the store or has not finished replaying it,
which a retry usually resolves,
while `collector_unavailable` means the store is fine and nothing is running Collections,
which no retry resolves until an operator or a scheduler brings a collector back.
It is answered by `POST /collections` alone.

`idempotency_mismatch` is `409` rather than `400` because the request is well formed
and the conflict is with a Collection that already exists (section 10.2).
It is answered by `POST /collections` alone, and it carries no `details`:
the fault is not in one input, it is that this key already stands for something else.

`limit_exceeded` carries a `details` item per violating field, with the vocabulary section 6.3 defines.
Every other code in this table carries no `details`;
the rule and the vocabularies belong to the gateway spec's *Errors* section.

`invalid_parameter`, `realm_denied`, `route_unknown`, `method_not_allowed`, `not_ready`,
`service_not_found`, and `service_selectorless` are reused with their gateway meanings.
Audit-only codes, never an HTTP status of their own: `cas_contended`, `artifact_stream_failed`, `client_gone`.

### 10.9 Non-disclosure

The gateway spec's *Non-disclosure* section holds.
Records, manifests, and error bodies name namespaces, Services, Pods, nodes, versions, and gateway Pod names;
never a Pod IP or pprof port.
A record also carries the idempotency key its creator sent, which is that caller's own text
and names nothing of the cluster;
a realm that may read the record reads it, and reading it grants nothing.
A key resolves only through the receipt of section 10.2,
whose name is a hash over the principal that sent it,
so a reader of one principal's record cannot form another principal's receipt key,
and a replay is answered from the scope that created the Collection and no other.
The receipt itself is reachable through no route.
The `details` of a `limit_exceeded` name policy fields and the ceilings they crossed,
which are the operator's configuration and the caller's own request rather than cluster state.
The merged profile is application data and passes through as the interactive profile body does.

---

## 11. Configuration

New top-level blocks `nats` and `pgo`, and a `pgo` block in each realm.
One file configures both processes.
`profgate serve` and `profgate collector` load the same ConfigMap, apply the same defaults,
and run the same validation, so a file cannot be valid for one role and invalid for the other,
and `profgate config validate` answers for both at once.
The collector reads no `realms`, `auth`, `ui`, or `server.listen` value — it opens no API listener —
but they are still loaded and validated there,
because a second file would be a second thing to keep in step for no gain.
Loading, strict unknown-key handling, environment prefix, and the `atomic.Pointer` snapshot are as in the gateway spec's *Configuration* section.

| Key | Env | Default | Reload | Validation |
|---|---|---|---|---|
| `pgo.enabled` | `PROFGATE_PGO_ENABLED` | `false` | restart | bool |
| `pgo.preset` | `PROFGATE_PGO_PRESET` | `standard` | restart | `small`, `standard`, or `large` (section 11.1) |
| `pgo.configAPI` | `PROFGATE_PGO_CONFIG_API` | `enabled` | hot | `enabled` or `disabled` |
| `pgo.leaseTTL` | `PROFGATE_PGO_LEASE_TTL` | `60s` | restart | 30s–10m |
| `pgo.maxAttempts` | `PROFGATE_PGO_MAX_ATTEMPTS` | `3` | restart | 1–10 |
| `pgo.jobRetention` | `PROFGATE_PGO_JOB_RETENTION` | `168h` | restart | ≥ `pgo.limits.maxRetention + 1h`; ≤ 2160h |
| `pgo.limits.maxDuration` | `PROFGATE_PGO_LIMIT_MAX_DURATION` | preset | restart | 1s–`limits.cpuSeconds` |
| `pgo.limits.maxRounds` | `PROFGATE_PGO_LIMIT_MAX_ROUNDS` | preset | restart | 1–20 |
| `pgo.limits.maxParallel` | `PROFGATE_PGO_LIMIT_MAX_PARALLEL` | preset | restart | 1–64 |
| `pgo.limits.minEvery` | `PROFGATE_PGO_LIMIT_MIN_EVERY` | preset | restart | 1m–`maxEvery` |
| `pgo.limits.maxEvery` | `PROFGATE_PGO_LIMIT_MAX_EVERY` | preset | restart | `minEvery`–24h |
| `pgo.limits.maxRetention` | `PROFGATE_PGO_LIMIT_MAX_RETENTION` | preset | restart | 1m–720h; ≥ `maxEvery` |
| `pgo.limits.maxSampleBytes` | `PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES` | preset | restart | 1 MiB–256 MiB |
| `pgo.limits.maxMergedBytes` | `PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES` | preset | restart | `maxSampleBytes`–1 GiB |
| `pgo.limits.maxTargetsPerRound` | `PROFGATE_PGO_LIMIT_MAX_TARGETS_PER_ROUND` | preset | restart | 1–256; `maxRounds × maxTargetsPerRound ≤ 256` |
| `pgo.limits.maxActiveCollections` | `PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS` | preset | restart | 1–64 |
| `pgo.limits.onDemandPerMinute` | `PROFGATE_PGO_LIMIT_ON_DEMAND_PER_MINUTE` | preset | restart | 1–600 |
| `pgo.limits.maxLiveCollections` | `PROFGATE_PGO_LIMIT_MAX_LIVE_COLLECTIONS` | preset | restart | 1–1024 |
| `pgo.defaults.schedule.every` | — | `6h` | hot | `minEvery`–`maxEvery` |
| `pgo.defaults.schedule.jitter` | — | `10m` | hot | ≤ `every / 2` |
| `pgo.defaults.sampling.duration` | — | `30s` | hot | 1s–`maxDuration` |
| `pgo.defaults.sampling.rounds` | — | `2` | hot | 1–`maxRounds` |
| `pgo.defaults.sampling.roundInterval` | — | `30s` | hot | 0–10m |
| `pgo.defaults.sampling.replicas` | — | `all` | hot | `all` or 1–`maxTargetsPerRound` |
| `pgo.defaults.sampling.maxParallel` | — | `4` | hot | 1–`limits.maxParallel` |
| `pgo.defaults.target.versionPolicy` | — | `strict` | hot | `strict` |
| `pgo.defaults.artifact.retention` | — | `24h` | hot | 1m–`maxRetention`; ≥ `pgo.defaults.schedule.every` |
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
`pgo.limits` is restart because the memory figure in section 3.4 and the collector's sizing depend on it.
`skewMargin` (section 8.4), `maxRecordBytes` (section 8.2),
`decodeFactor` and `collectorBaseMemory` (section 3.4),
and the 10-minute orphan age (section 8.9) are constants, not configuration:
a knob would invite tuning them past the assumptions they encode.
The heartbeat interval and its freshness window (section 7.5) are not configuration either:
both are derived from `pgo.leaseTTL`,
so an operator who lengthens the lease lengthens the interval and the window with it
and cannot put the two out of step.

`maxActiveCollections` carries an upper bound of 64 rather than only a floor,
because it is the outer multiplier of the memory limit (section 3.4)
and an unbounded ceiling lets a typo render a limit no node can satisfy —
or, before the checked multiplication, one that is not a byte count at all.
Every preset sits far inside it: 1, 2, and 4.
Every preset satisfies every cross-field rule as published (section 11.1),
and so must a preset with overrides applied.
Every cross-field rule that judges the `pgo` block against itself —
between limits, and between a default and its ceiling —
runs whether or not `pgo.enabled` is true,
so a file carrying an inconsistent `pgo` block fails at startup as written rather than on the day the flag flips.
The one rule that measures a PGO ceiling against `limits` (`maxDuration ≤ limits.cpuSeconds`)
waits for `pgo.enabled`:
a gateway that never collects is free to set a `limits.cpuSeconds` under the shipped `maxDuration`,
a value no preset can satisfy.
The `nats` requirements wait for the same flag:
a disabled gateway reaches no NATS cluster and needs none configured.

```yaml
nats:
  url: nats://nats.profgate.svc:4222
  credsFile: /etc/profgate/nats/nats.creds
  connectTimeout: 5s
pgo:
  enabled: true
  preset: standard
  configAPI: enabled
  leaseTTL: 60s
  maxAttempts: 3
  jobRetention: 168h
  limits:
    # Every ceiling comes from the preset; an entry here replaces that one ceiling.
    maxTargetsPerRound: 24
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
      retention: 24h
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

### 11.1 Presets

Twelve ceilings are twelve decisions an operator has to make before collecting anything,
and eleven of them have no local answer:
nothing about a particular cluster says whether `maxMergedBytes` should be 64 MiB.
What an operator does know is how much of their fleet they intend to profile at once.
`pgo.preset` asks that question once and answers the twelve.

| `pgo.limits` key | `small` | `standard` | `large` |
|---|---|---|---|
| `maxDuration` | `30s` | `60s` | `60s` |
| `maxRounds` | `3` | `5` | `8` |
| `maxParallel` | `4` | `4` | `8` |
| `minEvery` | `15m` | `15m` | `5m` |
| `maxEvery` | `24h` | `24h` | `24h` |
| `maxRetention` | `48h` | `72h` | `120h` |
| `maxSampleBytes` | `16777216` (16 MiB) | `33554432` (32 MiB) | `33554432` (32 MiB) |
| `maxMergedBytes` | `33554432` (32 MiB) | `67108864` (64 MiB) | `67108864` (64 MiB) |
| `maxTargetsPerRound` | `16` | `32` | `32` |
| `maxActiveCollections` | `1` | `2` | `4` |
| `onDemandPerMinute` | `5` | `10` | `30` |
| `maxLiveCollections` | `16` | `64` | `256` |

`standard` is what this document shipped as its twelve defaults, unchanged except for `maxRetention` (below),
so an existing configuration that names no preset keeps the ceilings it already had.
`small` is a collector that runs one Collection at a time;
`large` is one that runs four, profiles a Service every five minutes if a policy asks,
and sits exactly on the `maxRounds × maxTargetsPerRound ≤ 256` record bound,
which is what that rule is for.

Each preset fixes three figures nobody types:

| Figure | `small` | `standard` | `large` |
|---|---|---|---|
| PGO working set, from the second term of section 3.4 | 1 GiB | 4 GiB | 12 GiB |
| collector memory, that working set plus the 256 MiB base | 1280 MiB | 4352 MiB | 12544 MiB |
| profile fetches one collector holds open, `maxParallel × maxActiveCollections` | 4 | 8 | 32 |
| PGO fetches one Pod can receive from one collector, `maxActiveCollections` | 1 | 2 | 4 |
| live Collections per publisher, `maxLiveCollections` | 16 | 64 | 256 |

The two memory rows are listed separately because only the second is a container limit:
the working set is what the ceilings buy, and the base is what the process costs before it decodes anything.
The collector's `resources.limits.memory` is the second row, and the chart computes it (section 3.4).
The per-Pod row is per collector replica;
`C` overlapping collectors make it `C × maxActiveCollections`,
and interactive traffic adds `gatewayReplicas × limits.maxConcurrentProfiles` beside it (section 8.5).
The collector's `terminationGracePeriodSeconds` does not vary with the preset:
it is `pgo.leaseTTL + 30s`, 90 seconds at the shipped lease, for the reason section 12.4 gives.
A gateway replica's memory limit and grace period do not vary with the preset either,
because nothing in this section describes a gateway replica's work.

**Overrides.**
`pgo.limits.<key>` sets one ceiling and leaves the other eleven at the preset's value;
so does the key's environment variable.
An override is validated exactly as a preset value is —
its own range, and every cross-field rule — so no override can produce a configuration a preset could not.
`profgate config validate` prints the preset name, the twelve resolved ceilings,
the figures above, and the collector grace period,
which is how an operator reads what an override actually produced.
A preset is a set of defaults, not a mode: nothing downstream branches on its name.

**Why `maxRetention` moved, and where `artifact.retention` sits against it.**
`pgo.defaults.artifact.retention` shipped at `2h` against a default `schedule.every` of `6h`,
which left a Service without a downloadable artifact for two thirds of every interval —
a build asking for the newest profile usually found none.
Retention shorter than `every` is the incoherent case in general,
so the shipped default is now `24h`,
covering the `6h` default four times over and the `maxEvery` ceiling once.
Two rules hold it there, and they claim different things.
`pgo.limits.maxRetention ≥ pgo.limits.maxEvery` is a feasibility rule about the preset:
it guarantees that a retention long enough for the longest admissible interval is *available*.
On its own it guarantees nothing about any particular policy —
every preset would still admit `schedule.every: 24h` beside `artifact.retention: 1m`,
because each field satisfies its own range and no ceiling relates them.
What rules out that policy is the effective-policy rule of section 6.3,
`artifact.retention ≥ schedule.every`,
validated where a policy is written, published, scheduled, and claimed.
`pgo.jobRetention` stays `168h` and keeps its rule, `≥ maxRetention + 1h`:
a record is metadata in KV and a artifact is bytes in an Object Store,
so records outliving artifacts several times over is the right asymmetry, not an accident.
The cost of the longer default is store size:
a Service holds about `ceil(retention / every)` artifacts at once, each at most `maxMergedBytes`,
which at `every: 15m` and `retention: 24h` is 96 of them.
The 1 GiB floor section 3.2 requires of `PROFGATE_ARTIFACTS` is a floor, not a sizing recommendation;
sizing the bucket is that arithmetic over the Services actually scheduled,
and a bucket that fills fails the write as `artifact_store_failed` under `Discard: new` rather than evicting anything.

---

## 12. Operations

### 12.1 Logging

Every PGO request emits one record on completion:

```text
requestId, principal, namespace, service, collection, method, status, code, duration_ms
```

`requestId` is the gateway spec's *Request identifier*, on this record as on every other request record.
`wait` is added to the record of a `GET /v1/collections/{id}` that carried a `wait` the gateway accepted,
carrying the duration the request asked for, which is the only one it can now be,
the way `explain` is added to a targets record rather than always present.
A request that carried none writes no `wait`.
A `latest` request names in `collection` the identifier that answered it,
so a reader sees which Collection a build took although the request named none.

Every Collection transition emits one record, from whichever process performed the terminal update —
the collector for every transition its worker, owner loop, or sweeper makes,
a gateway replica for a cancel and for the expiry a download discovers:

```text
collection, namespace, service, state, attempt, reason, instance
```

Every sample emits one record at debug level with `pod`, `round`, `result`, `bytes`; never an IP.

### 12.2 Health

A gateway replica's `/readyz` additionally requires the NATS preflight to have passed when `pgo.enabled` is true.
It does not wait for the replay barrier:
a replica whose watches are still replaying serves interactive requests and answers PGO routes `503 pgo_unavailable`,
which is correct behavior for it, not a reason to be removed from the Service.
The collector serves `/healthz` and `/readyz` on its ops listener and nothing else.
Its readiness is the Kubernetes informer caches synced and the NATS preflight passed —
the same two conditions, minus everything about serving requests —
and no Service selects it, so readiness there gates only the rollout:
a collector that cannot reach NATS holds the new Pod unready
and leaves the previous one running until it can.
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
| `profgate_pgo_collector_available` (gauge) | — |
| `profgate_nats_connected` (gauge) | — |

`profgate_requests_total` gains `endpoint` values
`pgo_policy`, `collections`, `collection`, `collection_profile`, `collection_cancel`,
with `profile` fixed to `cpu` for the last three and `none` otherwise,
and `code` gains the values of section 10.8 including the audit-only ones.
The two `latest` routes are counted under `collection` and `collection_profile`:
they answer those two shapes and differ only in how the record was chosen,
and a label per route would split a series to record a path the audit line already names.
No namespace, Service, or Collection identifier becomes a label.
No parameter becomes one either, `wait` least of all.
A long poll's duration lands in `profgate_request_duration_seconds`,
whose only label is `profile` and which the Collection routes fix to `cpu`,
so a minute spent waiting is indistinguishable there from a minute spent fetching a CPU profile.
An operator reading that histogram is reading `wait` in it,
and the alternative is the label per request parameter the gateway spec's *Metrics* section refuses.
`profgate_collections_total` is emitted by whichever process wrote the terminal update,
so its `cancelled` and `expired` counts come from gateway replicas and the rest from the collector.
`profgate_collection_samples_total`, `profgate_collection_duration_seconds`, `profgate_schedule_slots_total`,
`profgate_sweeper_deletes_total`, and `profgate_collections_active` are the collector's alone;
`profgate_requests_total` and `profgate_pgo_collector_available` are a gateway replica's alone;
`profgate_nats_connected` is on both.
`profgate_pgo_collector_available` is a gateway metric on purpose:
a collector cannot report its own absence, and the replicas that refuse work for it can.
It is `1` while that replica sees a fresh heartbeat and `0` otherwise (section 7.5),
and it exists only when `pgo.enabled`.

Every one of them is on an ops listener, which no Service selects,
so a scrape configuration has to reach both Deployments to see the whole picture —
which is why the chart's `PodMonitor` selects the common labels and no `component`
(the gateway spec's *Build and Deployment* section).

The chart's `PrometheusRule` carries one PGO alert, rendered only when `pgo.enabled`:
`profgate_pgo_collector_available == 0` for 5 minutes, at warning severity,
whose text says that policy writes still succeed and that no Collection will run until a collector returns.
Five minutes is long enough to sit out a collector rolling update,
whose new Pod writes its first heartbeat as soon as its preflight passes,
and short enough that a collection outage does not first surface as a stale artifact hours later.

### 12.4 Shutdown

A gateway replica drains exactly as the gateway spec's *Startup and shutdown* section says.
Its PGO routes are ordinary requests; nothing about `pgo.enabled` lengthens its grace period.

On `SIGTERM` the collector stops all three loops at once —
the scheduler creates nothing further, the sweeper ends its pass, and the worker claims nothing —
**and every owner loop stops renewing its lease.**
An owner that finishes before `committedLeaseUntil - skewMargin` commits its Collection normally,
because its lease is still valid and its final update is the same conditional write it always was.
An owner that has not finished by then aborts on the cutoff of section 8.4, writes nothing,
and its record is reclaimed as a new attempt by the next collector to scan it.
That is the existing `lease_lost` path, entered by a signal instead of by a slow renewal;
shutdown introduces no state, no reason, and no rule of its own.

The window this bounds is at most `leaseTTL - skewMargin` measured from the owner's last successful renewal,
which at the shipped 60-second lease and its `leaseTTL / 3` renewal interval is between about 35 and 55 seconds.
The collector therefore waits up to that window for its owner loops and then exits,
whether or not a work goroutine is still inside `Merge`, `Compact`, `Write`, or a `Put`:
those calls take no context (section 8.4), so waiting for them is unbounded and buys nothing,
now that whatever they produce can no longer be committed.
A `Put` that lands after the process exits leaves an object no record names,
which is what the sweeper's 10-minute orphan rule already removes (section 8.9).
`terminationGracePeriodSeconds` for the collector is therefore `pgo.leaseTTL + 30s` —
90 seconds at the shipped lease — and the chart renders it;
`profgate config validate` prints it.

Waiting for the work instead of the lease asks for a grace period covering the worst deadline
`pgo.limits` admits, which runs to tens of hours — a number no operator sets.
Waiting past the lease also buys nothing that reclaim does not already provide sooner:
once the owner stops renewing, the record is claimable within `leaseTTL + skewMargin` anyway.
The cost is stated where it lands: a Collection interrupted by a rollout restarts from round 0 as a new attempt,
and the attempts it has left bound how often that can happen (section 8.8).

While the collector drains, its Kubernetes credentials stay valid:
it authenticates with a projected ServiceAccount token bound to its own Pod object,
and a graceful delete leaves that Pod object in place until termination completes.
A force deletion (`GracePeriodSeconds: 0`) removes the Pod object immediately instead:
every `Confirm` in the next round fails `discovery_unavailable`, the round has zero successful samples,
and the worker ends the Collection `failed no_samples` rather than finishing it;
the next schedule slot collects the Service again.
A collector that dies without draining stops renewing by dying;
the next collector's scan reclaims its Collections once `leaseTTL + skewMargin` has passed.
It also stops writing its heartbeat, so gateway replicas see it go stale within the window of section 7.5;
a draining collector deletes its key first, which reaches them sooner and changes no answer.

A Collection's samples are bounded by the collector's own work context and the lease cutoff,
not by the gateway spec's HTTP-server drain,
which bounds requests a gateway replica is serving and reaches nothing the collector fetches.
An outbound sample is an ordinary proxied request built from the same transport and the same budget rules,
issued under a context the owner loop cancels at `committedLeaseUntil - skewMargin` (section 8.4);
that cancellation, not a drain, is what stops it.

### 12.5 Deployment

The gateway spec's *Build and Deployment* section holds what the chart guarantees for both roles:
the common `app.kubernetes.io/name` and instance labels beside `app.kubernetes.io/component`,
the conditional collector Deployment, the gateway's static memory limit,
the `PodMonitor` that selects both roles, and one NetworkPolicy per role.
What follows is the PGO half of those objects — what the collector needs that the gateway does not.

**The collector Deployment.**
It is rendered only when `pgo.enabled`, carries `app.kubernetes.io/component: collector`,
and runs `profgate collector` over the same ConfigMap the gateway mounts (section 11).
`pgo.collector.replicaCount` defaults to `1` (section 2).
It declares the ops port and no API port, no Service selects it, and it carries no PodDisruptionBudget:
nothing routes to it, so there is nothing for a budget to protect.
Its `resources.limits.memory` is the figure of section 3.4, computed by the chart from the resolved ceilings;
its `terminationGracePeriodSeconds` is `pgo.leaseTTL + 30s`, 90 seconds at the shipped lease (section 12.4).
Neither figure appears on the gateway Deployment, and neither moves when the other role's does.
It mounts the NATS credentials Secret exactly as the gateway does (section 3.4).

**Network.**
The collector's required flows are not the gateway's:
it opens no API listener, so nothing routes to it,
and it reaches NATS, which a gateway replica reaches only when `pgo.enabled`.

| Direction | Peer | Why |
|---|---|---|
| ingress | monitoring namespace → ops port | `/metrics`, `/healthz`, `/readyz` |
| egress | Kubernetes API server | the informers and the Pod `get` of every `Confirm` |
| egress | NATS | the three stores |
| egress | DNS | resolving both of the above |
| egress | `PodIP:pprofPort` across application namespaces | fetching samples |

The gateway's own ingress is unchanged by this document:
the API port from the Ingress controller's namespace and the ops port from the monitoring namespace.
The collector's policy admits the ops port and nothing else,
because no request reaches it from anywhere.

**The application-side policy admits the collector.**
`deploy/networkpolicy-app-example.yaml` admits the gateway's pprof connections to an application Pod.
A collector fetches samples over the same port from the same namespace,
so in a cluster that enforces policy,
a rule naming only the gateway would leave every Collection failing `no_samples`
while the interactive path kept working —
the split's most likely deployment mistake.
The example's Pod selector is therefore the common `app.kubernetes.io/name: profgate` with no `component`,
which admits both roles from one rule,
and its comment says that narrowing it to one role breaks the other.

**The kustomize tree.**
The collector Deployment and its NetworkPolicy ship in `deploy/` and are not named by `deploy/base/kustomization.yaml`,
for the reason the gateway spec's *Build and Deployment* section gives:
a base has no conditional, `pgo.enabled` is false by default,
and an unconditional collector would give every plain-kustomize install a Pod for a disabled feature.
An operator enabling collection adds both to an overlay beside the `pgo` keys.

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
  The effective-policy retention rule (section 6.3) over the three ways a policy reaches it:
  operator defaults alone, where `retention` below `every` is rejected;
  a stored override setting only `schedule.every` above the default `retention`, rejected at `PUT /pgo`
  and, when written past the API, making the Service ineligible for scheduling
  and appearing in `GET /pgo`'s `violations`;
  a stored override setting only `artifact.retention` below the default `every`, likewise;
  and a `POST /collections` body whose snapshot violates it answering `400 limit_exceeded` naming both fields.
  `every` equal to `retention` passes at each of them.
  Before any local slot is reserved,
  a worker claiming a stored snapshot that violates the rule fails it `limit_exceeded`,
  the way a ceiling violation does;
  the test fails when the rule is checked only at write time,
  which is the state a ceiling-to-ceiling rule alone would leave.
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
  and with both publishers' active-key watches frozen before a barrier
  and scheduled plus concurrent on-demand creations across distinct Services released together,
  each publisher publishes at most its headroom and the cluster stays at or below 16,
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
  at or below `2 × publishers × maxLiveCollections`;
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
- `internal/pgo` publisher, driven without a scheduler, as a gateway replica's is:
  its own pass releases a reservation left by an indeterminate create once a cache delivers the record,
  and by the fresh reads when nothing committed,
  with no scheduler tick anywhere in the test
  (the test fails when the release rule is driven from the scheduler);
  `maxLiveCollections` reservations held by indeterminate creates,
  all resolved by later passes, leave the publisher accepting again,
  which is the regression that would otherwise answer `429 capacity_exhausted` for the life of the process;
  `Publisher.Run(ctx)` performs one pass per 10-second tick of the injected clock and stops when `ctx` ends.
- `internal/pgo` collector heartbeat, with a fake clock:
  the writer `Create`s `collector.<instance>` on its first tick and `Update`s at its own revision thereafter,
  once every `leaseTTL / 3`, and writes nothing before the replay barrier clears;
  a reader holds available while a key is within `2 × leaseTTL/3 + skewMargin` of `writtenAt`,
  survives exactly one missed write, and reports unavailable on the second;
  a key left behind by a stopped writer reads unavailable at the same moment
  whether or not it is ever deleted (the test fails when availability is derived from the key's presence);
  two collectors leave the reader available while either one is fresh;
  a graceful shutdown deletes the key at its revision, and the sweeper deletes one
  `10m + skewMargin` past `writtenAt` and keeps a younger one;
  neither delete changes what a reader reports;
  the key is counted by no `cachedLive` figure, visited by no worker scan,
  and matched by no `job.*`, `active.*`, or `schedule.*` sweeper rule.
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
  the fake `Discovery` records the zero `PortSelection` on the advisory resolution and on every round,
  so a Collection never names a port;
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
  a multi-batch Collection (`maxParallel` 2 over six Pods, three rounds) never has more than two samples in flight,
  counted by a fake transport, and needs no admission gate to hold that;
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
  the worker takes no `admit.Gate` at all:
  a compile-time check that `pgo.NewRounds` has no gate parameter,
  and a fake transport counting concurrent fetches at `maxParallel × maxActiveCollections` while two Collections run;
  three Services whose selectors all resolve to one Pod, with `maxActiveCollections: 2`,
  release Collections together against a fake transport that counts fetches per Pod:
  the third waits for a local slot, that Pod never has more than two PGO fetches open at once,
  and a Collection contributes one of them rather than `maxParallel`,
  which is the per-Pod bound of section 8.5 measured where nothing excludes a Pod.
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
- `internal/pgo` caches:
  a per-record subscription receives a pulse for every entry applied for that record,
  is registered before the handler's first read and removed when its request ends,
  and a subscriber that never reads blocks no `apply`, proven by a second subscriber that still receives;
  a subscriber whose buffer is full has its pulse dropped rather than `apply` blocked,
  and the next entry applied for the record pulses it again;
  no cache indexes an idempotency key, asserted by a scan of the package's cache types,
  because every read of one is authoritative;
  a connection-generation change broadcasts on the channel a parked handler selects on,
  reaching every subscriber once.
- `internal/pgo` idempotency receipts, over the in-process server:
  the receipt key is `idem.` followed by 32 hexadecimal characters,
  the head of the SHA-256 of the four length-prefixed scope fields;
  two scopes that differ only in where a `|` falls inside a principal produce different keys;
  a keyed publication writes the receipt after winning `active.<ns>.<svc>` and before the `pending` update,
  proven by a barrier between the two that finds the receipt already in the bucket;
  a publication killed after its record and before its receipt leaves a record no key resolves;
  a publication whose receipt `Create` returns `ErrUnavailable` retries once,
  and one that fails twice withdraws — its record and its active key are gone and the answer is `503 pgo_unavailable`;
  a same-key loser that finds no receipt and an `initializing` record answers `429 collection_in_progress` with `Retry-After: 1`
  and never `200`, with a barrier proving the winner can still withdraw at that moment;
  a keyed record the scan failed `not_published` answers no replay, and a retry with its key creates anew;
  a scheduled publication writes no receipt;
  a keyed publication whose active create loses writes none either;
  the sweeper deletes the record before its receipt and never the other way round,
  and a receipt left behind by a record deletion that did not reach it is deleted by the reconciliation pass
  once its value's `createdAt + jobRetention + skewMargin` has passed,
  while a receipt whose record is still there, and a receipt younger than that, both survive it;
  the reconciliation runs on one sweep in sixty and reads no `idem.*` key on the other fifty-nine.
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
  `POST /collections` with the `collector.*` cache empty, and with it holding only a stale key,
  each answering `503 collector_unavailable` with no write to either bucket,
  and answering `202` again once a fresh key is delivered;
  under the same empty cache, `PUT /pgo`, `DELETE /pgo`, `GET /pgo`,
  the two record routes, the download, and cancel all answer as they do with a collector present,
  which is what "policy is data" means as an assertion;
  `collector_unavailable` and `pgo_unavailable` distinguished:
  a replica behind the replay barrier answers the latter for every state-touching route,
  never the former, whatever its `collector.*` cache holds;
  `profgate_pgo_collector_available` reads `0` and `1` across those cases;
  body size and unknown-field rejection, each carrying its `details` item;
  the media type of section 10:
  a `POST` to `.../collections` and to `.../cancel` is `400 invalid_parameter` under four headers —
  none at all, `text/plain`, `application/x-www-form-urlencoded`, and `multipart/form-data` —
  each naming the header under `header_required` or `header_malformed`,
  while `application/json`, `application/json; charset=utf-8`, and `application/json; profile=x` each pass,
  which is `mime.ParseMediaType` returning parameters the route ignores;
  a repeated `Content-Type` and one that does not parse are refused under `header_malformed`;
  that refusal is the answer with `pgo.enabled` false, with the replay barrier not yet cleared,
  with the caches unsynced so readiness would have refused, with no credential under `basic`,
  and for a realm-denied Service,
  and the fake store records no call in any of the four,
  which is the ordering claim stated as a test;
  `PUT /pgo` without a `Content-Type` is unchanged, because the rule names the two `POST` routes;
  the `Idempotency-Key` contract of section 10.2:
  a key of one byte and of 128 bytes is accepted,
  and an empty one, 129 bytes, a byte outside the set, and the header sent twice are each
  `400 invalid_parameter` naming it under `header_malformed`, with nothing written;
  a second `POST` carrying the key of the first answers `200` with `{id, state}` and the same `Location`,
  the state read from the record rather than fixed at `pending`,
  and carries no other field, asserted against the encoded body;
  it answers so for a record that is `initializing`, one that is `running`,
  and one that is terminal with its active key already deleted;
  a principal holding `pgo.collect` and not `pgo.read` receives that same `200`
  while `GET /v1/collections/{id}` for the identifier it names is `404 collection_not_found`,
  which is the disclosure the thin answer exists to prevent;
  a replay whose principal's realm no longer admits the Service is `403 realm_denied` at the realm step,
  before the receipt is read;
  the same key from another principal and the same key on another Service each publish a new Collection,
  and the two receipts are distinct keys in the bucket;
  the same key with a body whose snapshot hash differs is `409 idempotency_mismatch` with nothing written,
  including the case where the body is identical and the Service's stored override changed between the two requests,
  and the case where the operator defaults moved instead;
  a different key while a Collection is live is `429 collection_in_progress`;
  two concurrent requests carrying one key, released from a barrier against a frozen cache,
  yield one `202` and one `200` naming the same identifier,
  one `job.*` key, one `active.*` key, and one `idem.*` key in the bucket,
  and no `initializing` record left behind;
  the same pair, with the winner held at a barrier between its active create and its receipt create,
  yields one `202` and one `429 collection_in_progress` carrying `Retry-After: 1`,
  and the loser's retry after the barrier lifts is answered `200` from the receipt;
  with every `job.*` cache in the replica frozen from the moment the first create returned,
  a replay still answers `200` with the original identifier,
  which is the test that fails when the lookup reads a cache instead of the receipt;
  a replay whose receipt names a record the sweeper has deleted deletes the receipt and publishes,
  leaving one receipt naming the new Collection;
  a record written without a receipt is never replayed;
  a scheduled Collection's record carries no key, writes no receipt, and no replay matches it;
  the `wait` parameter of section 10.4:
  an absent `wait` answers as it does today and writes no `wait` audit field;
  `wait=0`, `wait=-1s`, `wait=abc`, `wait=61s`, `wait=120s`, an empty value, and a repeated `wait` are each
  `400 invalid_parameter`, carrying the item each earns, and register no subscription;
  no answer to any of them carries `X-Wait-Elapsed`;
  a terminal record answers at once with `X-Wait-Elapsed: 0.000`;
  a record answers at once rather than at the deadline
  when it moves from `pending` to `running` between the handler's registration and its first read,
  which is the test that fails when the handler reads before it registers;
  a `pending` record that becomes `running` answers with the record read after the pulse, not with the cached entry;
  a subscriber whose pulse buffer is filled before the record is written terminal still answers terminal,
  because the read and not the pulse decides;
  a renewal that writes only `progress` does not answer, proven by a wait that outlives two renewals;
  a record deleted mid-wait answers `404 collection_not_found`;
  a wait that expires answers the record its final read returned,
  with an elapsed value at least the duration asked for;
  a terminal transition whose pulse was dropped is answered terminal at the deadline,
  rather than reported as the state before it;
  a client that disconnects mid-wait is audited `client_gone`,
  and after the handler returns no subscription remains registered;
  the generation moving mid-wait answers `503 pgo_unavailable`,
  driven by the broadcast rather than by a timer,
  and the test fails when the handler reads `Generation()` once and never learns of the move;
  the drain signal answers every waiting request at once with the record it last read;
  fifty concurrent waits on one record are woken by one applied entry,
  and the fake store records one `Get` per woken request and opens no watch beyond the caches' own;
  the two `latest` routes of section 10.6:
  the newest `completed` record answers while a newer `failed`, a newer `running`, and a newer `expired` record exist,
  and the body equals `GET /v1/collections/{id}` for that identifier;
  three Services answer `404 collection_not_found`:
  one with no `completed` record, one with no records at all, and one whose only completed record expired;
  both routes answer with the completed record behind the newest one
  when that newest one has lost its object,
  and that newest record is `expired` in the bucket afterwards —
  a test that fails when the selection takes the newest cached entry without confirming the object;
  a Service whose two newest completed records have both lost their objects walks past both;
  a record the cache shows `completed` and a fresh read shows `expired` is skipped;
  `ErrUnavailable` during the walk is `503 pgo_unavailable` and never an older Collection;
  the two routes select the same record for the same fixture, asserted by comparing the identifier
  `latest` returns with the one `latest/profile` names in `X-Pprof-Collection`;
  `latest/profile` streams the bytes and the headers the identifier route streams;
  a denied namespace and a denied Service are `403 realm_denied` before the cache is read;
  `/v1/collections/latest` stays `404 route_unknown`;
  the audit record names the identifier that answered;
  the filters and the cursor of section 10.3:
  `state=` once and repeated, `since=`, `origin=schedule`, and `origin=api` each filter as specified
  and intersect when combined;
  an unknown name, an empty value, `state=running,pending` as one value, `state=nonsense`, `origin=on-demand`,
  `since=yesterday`, `limit=0`, `limit=101`, `limit=abc`, a repeated `limit`,
  a cursor that does not decode,
  and a cursor minted under one `state`, `since`, or `origin` and presented beside another,
  are each `400 invalid_parameter` with the item they earn;
  the same cursor presented beside the filters it was minted under, and beside a different `limit`, both work;
  a query with several faults reports the first fault in name order;
  250 records including duplicate `createdAt` values page through in three requests with none repeated and none skipped,
  and the same walk holds when records are inserted at the head and deleted at the tail between pages;
  a cursor naming a record the sweeper has since deleted still returns the entries after it;
  the last page carries no `nextCursor` key at all;
  no response, header, or manifest containing a Pod IP or port.
- `internal/config`:
  every new environment variable lands on its field;
  `nats.url` required only when `pgo.enabled`;
  the complete example of section 11 loads and validates as written;
  each of the three presets expands to the twelve ceilings of section 11.1 exactly,
  and each satisfies every cross-field rule on its own;
  an absent `pgo.preset` expands to `standard`, and an unknown name is rejected naming the key;
  one `pgo.limits` key overrides its preset value and leaves the other eleven;
  the same through the key's environment variable;
  an override outside its own range, and one that breaks a cross-field rule, are both rejected
  (`maxRounds × maxTargetsPerRound` above 256, `maxRetention` below `maxEvery`,
  `jobRetention` below `maxRetention + 1h`, `maxEvery` below `minEvery`,
  `onDemandPerMinute` outside 1–600, `maxDuration` above `limits.cpuSeconds`);
  defaults violating limits;
  a `pgo` block that contradicts itself rejected with `enabled: false`,
  and the rule against `limits` not applied to a disabled block;
  `maxActiveCollections` at `0` and at `65` rejected naming the key and the range, at `1` and at `64` accepted;
  the memory figure and the collector grace period computed for each preset match section 11.1,
  base term included, so a figure that dropped `collectorBaseMemory` fails;
  the multiplication rejects an overflowing product rather than returning a wrapped or negative count,
  exercised by handing the arithmetic ceilings the range check would refuse,
  which is the only way to reach it now that every range is bounded;
  a realm without `pgo` has all flags false.
- `cmd/profgate`:
  `collect` and `serve` load one file the same way and reach the same resolved ceilings;
  `collect` starts the scheduler, worker, sweeper, and heartbeat writer and opens no API listener;
  `serve` with `pgo.enabled` starts none of those, opens its four watches of section 5.1, and serves the PGO routes,
  and each role opens exactly the four prefixes its row of that table names and no fifth;
  each subcommand starts `Publisher.Run`, asserted per role rather than inferred from the algorithm:
  a publication left indeterminate before the process starts,
  the injected clock advanced past one 10-second pass interval,
  and the reservation resolved with no scheduler tick having run —
  under `collect` the test injects a scheduler that never ticks,
  and under `serve` there is no scheduler to inject,
  so a build that drove the pass from `Scheduler.tick` fails both halves;
  `config validate` prints the preset name, the resolved ceilings, the collector memory figure,
  and the collector grace period, for a configuration of each preset;
  `serve` closes the drain signal when `/readyz` turns 503 and before `server.drainDelay`,
  and a request parked in `wait=` answers at that moment rather than at its own deadline,
  which is what keeps the drain bound of the gateway spec's *Startup and shutdown* section where it is;
  `SIGTERM` to `collect` stops the loops and stops lease renewal,
  an owner that can finish inside the remaining lease commits,
  one that cannot writes nothing and leaves a reclaimable record,
  and the process exits without waiting for a work goroutine held at a barrier inside `Write`.
- `deploy/`: the NATS account fragment equals the section 3.3 list exactly.
  A manifest test pins the NATS credentials Secret volume on both Deployments:
  volume name, Secret source with `defaultMode: 0440`,
  mount path `/etc/profgate/nats/`, `readOnly: true` on the mount,
  and `fsGroup: 65532` in the pod `securityContext`.
  The collector Deployment renders only with `pgo.enabled`, runs `collect`, declares one replica,
  exposes the ops port and no API port, is selected by no Service, and carries no PodDisruptionBudget.
  Its `resources.limits.memory` equals what the binary computes for the same values, preset by preset and with an override,
  and its `terminationGracePeriodSeconds` equals `pgo.leaseTTL + 30s`;
  the gateway Deployment's memory limit and grace period are the same with `pgo.enabled` true and false.
  The chart and the binary agree on three outcomes, over the same values each time:
  **success**, where the rendered limit equals `internal/config`'s figure for the rendered ConfigMap,
  base term included, for each preset and for a single-key override of each sizing ceiling;
  **rejection**, where a value outside a sizing ceiling's range fails the render and fails `config.Load`,
  each naming the key, with `maxActiveCollections` at `0` and at `65` among them;
  and **overflow**, where a product past a 64-bit byte count is refused by both rather than rendered,
  asserted at the layer each check occupies (section 3.4).
  The collector's `extraEnv` refuses `PROFGATE_PGO_PRESET`
  and the four `PROFGATE_PGO_LIMIT_MAX_*` sizing variables, each with a message naming the structured value;
  the raw `config` block refuses `config.pgo.preset` and the four sizing keys under `config.pgo.limits` the same way;
  a non-sizing `pgo.limits` key and an unrelated `extraEnv` entry both still render.
  Every Pod of both Deployments carries the common labels and its own `app.kubernetes.io/component`;
  the `PodMonitor` selector matches both roles' Pods and names the ops container port;
  each role's NetworkPolicy selects its own `component`,
  the gateway's ingress ports are unchanged, the collector's ingress is the ops port alone,
  and the collector's egress covers the Kubernetes API server, NATS, DNS, and the Pod pprof port (section 12.5).
  `deploy/networkpolicy-app-example.yaml` admits both roles from one rule,
  asserted on a selector that carries no `component`.
- `internal/admit`: `TryAcquire` fails without blocking at capacity.
- Repository checks: the nats.go import check; the existing client-go check still passes.

The zero `PortSelection` on every `Targets` call is the gateway spec's client-selected-port amendment applied here;
its *Amendments* section lists this document's edits.

### 13.2 End-to-end

`mise run test:e2e` gains a NATS JetStream Deployment in the kind cluster
(`nats:2.11-alpine`, one replica, `--jetstream` with a file store directory on an `emptyDir`, a ClusterIP Service),
applied by `TestMain` with the gateway overlay,
and a collector Deployment of one replica running `collect` against the same ConfigMap.
The harness provisions the three buckets with `nats.go` through a port-forward before either Deployment starts,
with the configuration of section 3.2 (file storage, no TTL, `Discard: new`, no size limits).
Between scenarios it purges every key and object so each starts empty;
it never deletes or recreates a bucket while a gateway or a collector runs,
because both roles' watches and consumers belong to the original streams
and a recreated stream would leave them watching nothing.
Both Deployments run with `pgo.enabled: true`, and the gateway with a realm whose `pgo` flags are all true.
Scenarios that need a proxy to a test-app Pod to complete declare `needsPodReach`.

1. An on-demand Collection with `rounds: 2, replicas: all` against a three-replica test app
   reaches `completed` on either gateway,
   the downloaded artifact parses,
   the manifest lists six `ok` samples across three distinct Pod UIDs,
   and both gateways return the same record and the same bytes (`needsPodReach`).
   The create carries an `Idempotency-Key`,
   and a second `POST` with that key answers `200` with the same identifier rather than starting a second Collection.
   The scenario waits with `GET /v1/collections/{id}?wait=60s` rather than a poll loop:
   each call returns when the state moves, carries `X-Wait-Elapsed`,
   and the walk reaches `completed` inside the scenario's deadline.
   Once it has, `GET .../collections/latest` answers with that record
   and `GET .../collections/latest/profile` with the same bytes the identifier route served.
2. A `PUT /pgo` with `every` equal to `minEvery` and `jitter: 0`
   yields exactly one Collection for the slot;
   the harness watches `PROFGATE_JOBS` directly and counts `schedule.*` and `job.*` keys,
   and reads them through either gateway, neither of which schedules (`needsPodReach`).
3. A Collection with `rounds: 3, roundInterval: 20s` cancelled after round 1 ends `cancelled`,
   with no object in `PROFGATE_ARTIFACTS` (`needsPodReach`).
4. Two test-app Deployments with different version labels behind one Service:
   `POST /collections` answers `409 version_conflict`;
   with `target.version` pinned it completes with samples from one Deployment only (`needsPodReach`).
5. The collector Pod is deleted after its first renewal;
   its replacement reclaims with `attempt: 2` and the Collection completes (`needsPodReach`).
6. Realm without `pgo.configure`: `PUT /pgo` is `403`;
   without `pgo.read`: `GET /collections/{id}` of an existing record is `404`.
7. A gateway started with `pgo.enabled: false` answers `501 pgo_disabled` on every PGO route and links no NATS connection
   (asserted from the NATS server's connection count), and no collector Deployment exists.
8. The gateway ClusterRole is unchanged:
   the golden test and the missing-verb variants of the gateway suite still hold with PGO enabled.
9. With `PROFGATE_JOBS` re-provisioned with a 1-minute TTL
   (both Deployments restarted for this scenario only, since the bucket is recreated),
   the gateway and the collector each exit non-zero naming the bucket and `TTL`.
   With `nats.credsFile` pointing at a user that can open every bucket but lacks, in turn,
   publish on `$KV.PROFGATE_JOBS.>`,
   publish on `$O.PROFGATE_ARTIFACTS.>`,
   and `$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>`,
   the process exits non-zero naming the bucket and the probe operation that failed,
   and no probe key or object remains.

10. A `helm upgrade` of the collector while a Collection runs:
    the two collectors overlap, the slot fires once,
    and the Collection either completes on the outgoing Pod inside its remaining lease
    or is reclaimed by the incoming one as `attempt: 2` and completes there;
    exactly one artifact is downloadable afterwards and no orphan object survives the sweeper (`needsPodReach`).

11. With `pgo.enabled: true` on both Deployments and the collector scaled to zero,
    both gateways stay `Ready` and keep serving interactive requests;
    once the last heartbeat has gone stale, `profgate_pgo_collector_available` reads `0` on each of them,
    `POST /collections` answers `503 collector_unavailable` and writes no `job.*` or `active.*` key,
    while `PUT /pgo` still answers `200` and the override is readable back through `GET /pgo`.
    The scenario reaches no application Pod: nothing it asserts requires a sample.
12. Scaling that collector back to one makes the gauge read `1` on both gateways
    and the next `POST /collections` answer `202`,
    and the Collection the stored policy describes completes from the following slot (`needsPodReach`).
    It is a scenario of its own rather than the tail of 11,
    so a degraded lane skips the recovery and still proves the refusal.

Scenarios 1–5 and 10–12 run on every lane; the kind lanes do not need NetworkPolicy for NATS.

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
internal/admit/      the admission gate interactive requests pass through
internal/pgo/        policy layering, presets and ceilings, identifiers, publisher (reservation counter, Run and its pass, and the publication writes: the record, the active key, the idempotency receipt, and the update that makes the record claimable), scheduler, worker scan and owner loop, merge, sweeper, collector heartbeat writer and reader, clock seam
internal/httpapi/    gains the seven PGO routes and their realm flags
internal/config/     gains nats, pgo, and realm pgo blocks, and expands pgo.preset into pgo.limits
internal/metrics/    gains the PGO metrics
cmd/profgate/        serve wires NATS preflight, the caches, and Publisher.Run when pgo.enabled; collect wires preflight, the caches, Publisher.Run, the heartbeat writer, and the three loops
deploy/              gains the NATS account fragment, the bucket provisioning commands, the collector Deployment with its derived memory limit and grace period and its NetworkPolicy (both outside the kustomize base), and the example creds mount
test/e2e/            gains the NATS manifest, the collector Deployment, bucket provisioning, and scenarios 1–12
```

`internal/pgo` depends on `internal/natskv`, `internal/k8s`, `internal/proxy`, and `internal/metrics`;
nothing depends on it except `httpapi` and `cmd`.
`internal/admit` depends on nothing in the module and is used by `httpapi` alone.

---

## 16. Failure Scenarios

| Event | Behavior |
|---|---|
| NATS unreachable at startup with `pgo.enabled` | preflight retries forever in both processes; `/readyz` 503; interactive `/v1` routes serve once the Kubernetes side is ready; PGO routes `503 pgo_unavailable`; a collector rollout stalls with the previous collector still running |
| a collector's watches still replaying after preflight or after a reconnect | scheduler, worker, and sweeper idle until every watch has delivered its replay marker under the current connection generation |
| a gateway replica's watches still replaying | `/readyz` 200; PGO routes `503 pgo_unavailable` under the same condition |
| a record's policy snapshot exceeds the claiming replica's ceilings | `failed limit_exceeded` on claim or reclaim, before any local slot is reserved; active key released |
| a bucket missing or of the wrong kind | process exits naming the bucket |
| a bucket with a TTL, memory storage, `Discard: old`, or a size limit below the contract | preflight reads the status; process exits naming the bucket and the field |
| a bucket reaches `MaxBytes` | the write fails `ErrUnavailable` under `Discard: new`; nothing already stored is evicted |
| on-demand creation faster than `onDemandPerMinute` | `429 rate_limited` before any write |
| NATS user lacks a permission | a preflight probe fails; process exits naming the bucket and the operation |
| NATS unreachable while running | PGO routes `503 pgo_unavailable`; scheduler creates nothing; an owner aborts once `leaseUntil - skewMargin` passes without a renewal; the disconnect moves the connection generation and clears the barrier before the connection is usable again; after reconnect the watches replay behind the barrier, then a scan reclaims |
| collector crashes mid-Collection | lease expires; the next collector's scan reclaims from round 0 with `attempt + 1` under a new object name |
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
| collector rolled while a Collection runs | its owner stops renewing, commits if it finishes inside the remaining lease, and otherwise leaves the record to the incoming collector as a new attempt; the Collection costs one attempt per interruption |
| two rollouts inside one Collection | the third claim would exceed `pgo.maxAttempts`; the record is failed `attempts_exhausted` and the next slot collects the Service again |
| collector down or absent, while it is absent | its heartbeat goes stale within `2 × leaseTTL/3 + skewMargin`; `profgate_pgo_collector_available` reads `0` on every gateway replica and the chart's alert fires after 5 minutes; `POST /collections` answers `503 collector_unavailable` and writes nothing; policy writes and every read route still succeed; records already `pending` stay `pending`, because writing `not_claimed` is a worker's job; slot keys and expired artifacts accumulate, because sweeping is too |
| collector down or absent, when a collector returns | its first heartbeat makes the gauge `1` and `POST /collections` accepted again; its first scan fails every `pending` record past `claimBy` as `not_claimed` and its first sweep clears the accumulated slot keys, records, and artifacts; the current slot of each scheduled Service collects normally |
| `pgo.enabled` set on a hand-written manifest with no collector Deployment | the two rows above, indefinitely: the gauge stays `0`, the alert stays firing, and `POST /collections` keeps refusing; the chart renders the collector whenever `pgo.enabled` is true, so this is a hand-rolled install's failure and the gauge is where it shows |
| a heartbeat key left by a collector that died | it reads stale from `writtenAt`, so it reports absence exactly as no key would; the sweeper deletes it `10m + skewMargin` later, which changes no answer |
| every collector down over several slots | one Collection per Service when one returns, for the current slot only |
| policy changed while a Collection runs | the Collection keeps its snapshot; the next slot uses the new policy |
| rollout during a Collection | Pods of the new version are excluded; all rolled → `failed no_targets` |
| Kubernetes API unreachable during a round | confirmations fail `discovery_unavailable`; all-failed round → `failed no_samples` |
| object missing at download | `410 artifact_gone`; record flipped to `expired` |
| object expires while a download streams | connection closed; audit `artifact_stream_failed`; the client retries and gets `410` |
| a configuration writer raises every knob | ceilings reject the write; a ceiling lowered later makes the Service ineligible and visible in `violations` |
| an effective policy whose `artifact.retention` is under its `schedule.every` | `400 limit_exceeded` at `PUT /pgo` and `POST /collections`; a stored one makes the Service ineligible for scheduling and appears in `violations`; a claim fails it `limit_exceeded` before reserving a slot |
| two Collections on one collector, both at `maxParallel` | at most `maxParallel × maxActiveCollections` profile fetches open, in a process that serves no interactive request |
| several Services selecting one Pod, all collecting | that Pod receives at most `C × maxActiveCollections` PGO fetches at once over `C` overlapping collectors, plus up to `gatewayReplicas × limits.maxConcurrentProfiles` interactive ones; there is no per-Pod exclusion |
| a retry carrying the `Idempotency-Key` of a create whose answer was lost | `200` with the identifier and state of the Collection that key created, from one authoritative read of `idem.<hash>`, for as long as that record exists; no cache and no replica's replay lag stands in the way |
| a create that wrote its record and died before its receipt | the record never becomes claimable: the scan fails it `not_published` and releases the active key; a retry in the window is `429 collection_in_progress` with `Retry-After: 1`, and one after it finds no receipt and creates, a first Collection for that key and not a duplicate, because nothing ran |
| a create whose receipt `Create` fails twice | the winner withdraws: it deletes its `initializing` record and the active key and answers `503 pgo_unavailable`; a retry creates anew |
| a retry whose receipt names a record the sweeper has already deleted | the receipt is deleted at the revision the read returned and the create proceeds, so the key binds the new Collection |
| a replay reaching a principal with `pgo.collect` and not `pgo.read` | `200` with `{id, state}` and `Location`; the record itself stays behind `GET /v1/collections/{id}`, which that realm is denied |
| a replay whose caller's realm no longer admits the Service | `403 realm_denied` at the realm step, before the receipt is read; the key is not a way past a realm |
| the same `Idempotency-Key` with a request that means something else | `409 idempotency_mismatch`; nothing is written and the first Collection is untouched |
| a `POST` to a write route with no JSON media type | `400 invalid_parameter` naming `Content-Type`, before authentication, the realm, and every store call |
| a `wait=` request whose record never moves | it answers at its deadline with the record as last read, `X-Wait-Elapsed` naming how long it waited |
| a `wait=` request when the NATS connection drops | `503 pgo_unavailable`, the answer every state-touching route gives under a moved generation |
| a `wait=` request when its replica starts draining | it answers at once with the record it last read; the drain bound does not move |
| `latest` for a Service whose artifacts have all expired | `404 collection_not_found`, the same answer a Service that never collected gets, because a build does the same thing about either |
| a listing cursor naming a record the sweeper has deleted | the entries after it are still returned; the cursor is a position, not a lookup |
| a listing cursor presented with a different `state`, `since`, or `origin` | `400 invalid_parameter` naming `cursor`; a position in one filtered listing is not a position in another |
| a record created mid-walk that shares an instant with the cursor | it may fall on a page the client has passed; the order is stable for the records the listing held when paging started |
| `wait` above `60s` | `400 invalid_parameter` naming `wait`; the value is outside the route's grammar and is not clamped to it |
| `latest` for a Service whose newest completed record lost its object | that record is flipped to `expired` and the walk continues to the next completed one; `410 artifact_gone` is never the answer while an intact artifact exists |
| a Service whose effective retention is under its interval, asked for `latest` | `404` for the tail of every interval; the rule of section 6.3 refuses that policy at every write, which is what makes the endpoint dependable |

---

## 17. Amendments

Moving the collection loops into their own Deployment,
collapsing the twelve `pgo.limits` ceilings into `pgo.preset`,
moving the memory and grace-period arithmetic into the chart,
making a collector's presence observable to the gateways that publish for it,
and requiring an effective policy to retain an artifact for at least one interval amend the following text.
The first table lists the edits made in the same change as this section;
the second lists the documents that describe shipped behavior and are updated when the implementation lands.

Amended now:

| File | Section | Change |
|---|---|---|
| `docs/specs/pgo.md` | *Overview* | a rolling update of either Deployment is uneventful |
| `docs/specs/pgo.md` | *Core decisions* | decision 3 names the two processes and the `profgate collector` subcommand; decision 5 drops the shared admission gate and states that the collector holds no Kubernetes permission a gateway replica does not; decision 9 adds the preset |
| `docs/specs/pgo.md` | *Non-goals* | overlapping Collections on one Pod are bounded by each process's own ceilings, not by an admission gate |
| `docs/specs/pgo.md` | *Architecture* | the diagram carries `profgate serve` replicas and one `profgate collector`; why the loops moved out of the gateway; why one collector replica is the default and why every coordination mechanism stays, starting from the two collectors a `RollingUpdate` of a one-replica Deployment produces |
| `docs/specs/pgo.md` | *Container* | the collector's limit is `collectorBaseMemory` plus a checked PGO working set, the chart computes it and refuses the preset and sizing overrides that would move it out from under the render, a `deploy/` test compares the rendered limit with what the binary computes, and a gateway replica's limit no longer depends on `pgo.limits` |
| `docs/specs/pgo.md` | *What a compromised gateway can do* | a compromised collector reaches exactly as far: same image, ServiceAccount, NATS user, and stores |
| `docs/specs/pgo.md` | *The seam* | the replay barrier covers the watches a process opens — four in each role, three of them common, a collector adding `schedule.*` and a gateway replica `collector.*` |
| `docs/specs/pgo.md` | *Atomicity primitives*, *Paths that touch each key* | `maxLiveCollections` is per publisher, giving `publishers × maxLiveCollections`; a lead paragraph maps each path to the process it runs in |
| `docs/specs/pgo.md` | *Ceilings* | a ceiling no longer bounds how long a Deployment takes to drain; the effective-policy rule `artifact.retention ≥ schedule.every`, validated at configuration-default validation, `PUT /pgo`, `POST /collections`, scheduler evaluation, and claim and reclaim |
| `docs/specs/pgo.md` | *Slots*, *Collector availability* | slot arithmetic names collectors where it named gateways; a new *Collector availability* section defines the `collector.<instance>` heartbeat, its cadence and freshness window, the gauge, the alert, and the `503 collector_unavailable` refusal |
| `docs/specs/pgo.md` | *Atomicity primitives*, *Paths that touch each key*, *Sweeper*, *NATS stores* | the heartbeat key: one writer, its four paths, its sweep, and why its freshness is a timestamp rather than a server expiry |
| `docs/specs/pgo.md` | *Rounds* | the per-Pod bound: `C × maxActiveCollections` from collectors beside `gatewayReplicas × limits.maxConcurrentProfiles` interactive |
| `docs/specs/pgo.md` | *Errors*, *Create a Collection* | `503 collector_unavailable`, answered by `POST /collections` alone, and why it is not a second meaning for `pgo_unavailable` |
| `docs/specs/pgo.md` | *Deployment* | a new section under *Operations*: the collector Deployment's own resources and grace period, its network flows, the application-side policy admitting both roles, and the kustomize placement |
| `docs/specs/pgo.md` | *Algorithm*, *On-demand Collections* | the scheduler runs in collector replicas; `Publisher.Run(ctx)` owns the release pass and both subcommands start it, because a gateway replica publishes and runs no scheduler; the bounds read `publishers ×` and `gatewayReplicas ×` |
| `docs/specs/pgo.md` | *Record* | the deadline formula loses its term for waiting on admission |
| `docs/specs/pgo.md` | *Claim* | the worker runs in collector replicas; a claim can meet a snapshot published under a preset the collector no longer holds |
| `docs/specs/pgo.md` | *Rounds* | sampling takes no admission slot: the bound is `maxParallel × maxActiveCollections` by construction, the cross-key rule and the `slot_timeout` reason are removed, and `internal/admit` keeps `TryAcquire` alone |
| `docs/specs/pgo.md` | *Recovery* | the reclaiming process is the other collector during a rolling update or the restarted one afterwards; an interrupted Collection costs one attempt |
| `docs/specs/pgo.md` | *Sweeper* | the sweeper runs in collector replicas, and its cost is per collector replica |
| `docs/specs/pgo.md` | *HTTP API* | every route is served by a gateway replica and reaches no collector |
| `docs/specs/pgo.md` | *Configuration*, *Presets* | one file configures both roles; `pgo.preset` with the three presets, their derived figures, and single-key overrides; `maxActiveCollections` bounded at 64; `pgo.limits.maxRetention ≥ pgo.limits.maxEvery` kept as the feasibility rule beside the effective-policy one; `pgo.defaults.artifact.retention` is `24h` and must be at least the default `every`; the preset table separates the working set from the container limit; the cross-key rule against `limits.maxConcurrentProfiles` is removed |
| `docs/specs/pgo.md` | *Manifest*, *Get a Collection*, *Record* | the owner instance and the manifest's `gateway` field hold a collector Pod name; the field keeps its name |
| `docs/specs/pgo.md` | *Logging*, *Health*, *Metrics* | a transition record comes from whichever process wrote the terminal update; the collector's readiness is the informer caches and the NATS preflight, on its ops listener, selected by no Service; each metric is named with the process that emits it, `profgate_collections_total` comes from both, and `profgate_pgo_collector_available` with its alert is a gateway replica's |
| `docs/specs/pgo.md` | *Shutdown* | `SIGTERM` stops the loops and stops lease renewal; the drain window is at most `leaseTTL - skewMargin` from the last renewal; the collector exits without waiting for an uninterruptible merge or `Put`; a draining collector deletes its heartbeat key; a sample is bounded by the collector's work context and lease cutoff, not by the gateway's HTTP-server drain; `terminationGracePeriodSeconds` is `pgo.leaseTTL + 30s`; the worst-deadline figure is gone |
| `docs/specs/pgo.md` | *Testing* | preset expansion and override validation, the effective-policy retention rule, the two subcommands' wiring including `Publisher.Run`, the heartbeat and the refusal it drives, chart-and-binary agreement on success, rejection, and overflow, the per-Pod concurrency bound, the rendered manifests and their selectors and ports, a collector rolling update as scenario 10, and an enabled installation with no collector as scenarios 11 and 12 |
| `docs/specs/pgo.md` | *Package Layout*, *Failure Scenarios* | `collect` and `serve` wiring; rows for a rolled collector, an absent collector split into while it is absent and when one returns, a stale heartbeat key, an incoherent retention policy, several Services on one Pod, and attempts exhausted by repeated rollouts |
| `docs/specs/gateway.md` | *CLI* | `profgate collector --config <path>`, which runs the PGO loops and opens no API listener |
| `docs/specs/gateway.md` | *Request algorithm*, *Package Layout* | the admission gate is the one interactive requests pass through; `cmd/profgate/` lists `collect` |
| `docs/specs/gateway.md` | *Startup and shutdown* | no Collection wait beside the request drain and none on the listener-failure path; the grace period does not vary with `pgo.enabled` |
| `docs/specs/gateway.md` | *Build and Deployment*, *Layers* | the gateway's static memory limit; the common and `component` labels; a second Deployment when `pgo.enabled`, running `collect` with one replica, the ops port only, no Service, a derived memory limit, and a grace period of `pgo.leaseTTL + 30s`; the `PodMonitor` selecting both roles; one NetworkPolicy per role; the collector alert in the `PrometheusRule`; the collector objects outside the kustomize base; the manifest and chart tests that pin all of it |
| `.agents/rules/100-project-map.md` | *Planned Structure* | `cmd/profgate/` lists `collect`; `internal/admit/` is the gate for interactive requests |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/pgo.md` | the collector Deployment and what runs where; `pgo.preset` in place of the twelve ceilings; the retention default and its relationship to `schedule.every`; the drain paragraph under *Multiple gateway replicas*, which becomes what a rollout of either Deployment does; `slot_timeout` leaves the sample reasons; the memory formula sizes the collector |
| `docs/configuration.md` | `pgo.preset` and the preset table; `pgo.limits` as overrides and `maxActiveCollections` bounded at 64; the removed cross-key rule, the kept `maxRetention ≥ maxEvery`, and the added `artifact.retention ≥ schedule.every`; the `artifact.retention` default; `profgate config validate` prints the preset, the resolved ceilings, the collector memory figure, and the collector grace period instead of the worst-deadline figure; `profgate collector` in the subcommand list |
| `docs/deployment.md` | the collector Deployment: what it needs, what it does not serve, how it is scraped, the egress it needs, and the application-side policy that must admit both roles |
| `docs/api.md` | `slot_timeout` leaves the sample-result values; `503 collector_unavailable` on `POST /collections` |
| `deploy/chart/profgate/values.yaml`, `templates/`, `README.md` | a collector Deployment template with `pgo.collector.replicaCount: 1`; `pgo.preset` and `pgo.limits` overrides; the derived memory limit and grace period on the collector; `memoryLimitWithoutPGO` becomes the gateway's memory limit under both states and is renamed; the `extraEnv` and raw-config rejection lists gain `PROFGATE_PGO_PRESET` and `config.pgo.preset` and follow the sizing keys onto the collector; the `component` label on both roles; a PodMonitor selecting both and a NetworkPolicy per role; the collector-availability alert in the PrometheusRule |
| `deploy/` | the collector Deployment and its NetworkPolicy as files outside `deploy/base/kustomization.yaml`, for an overlay to add; the application policy example admitting both roles |
| `internal/config` | `pgo.preset` expansion, the changed cross-key rules, the checked memory multiplication, and the figures `config validate` prints |
| `internal/pgo` | `Publisher.Run` and its pass; the rounds loop without `admit`; the worker's drain on `SIGTERM`; the collector heartbeat writer and the reader gateway replicas use |
| `internal/httpapi` | `503 collector_unavailable` on `POST /collections` and the `profgate_pgo_collector_available` gauge |
| `internal/admit` | `Acquire` is removed; `TryAcquire` stays |
| `cmd/profgate` | the `collect` subcommand and the `serve` wiring that starts no loops |
| `test/e2e` | the collector Deployment and scenarios 10 to 12 |
| `CHANGELOG.md` | the client-visible moves: `pgo.defaults.artifact.retention` from `2h` to `24h`, the new rule that an effective policy's retention covers its interval, `slot_timeout` leaving the sample results, `503 collector_unavailable` joining the `POST /collections` answers, `pgo.limits` keys becoming preset overrides, and the chart's renamed memory value |

A contract a program can build on —
an `Idempotency-Key` on the create, a `wait=` long poll on the record route,
the newest completed Collection and its artifact under two routes,
filters and a cursor on the listing,
structured details inside `limit_exceeded`,
and the media type the two write routes require —
amends the following text.
The first table lists the edits made in the same change as this block;
the second lists the documents that describe shipped behavior and are updated when the implementation lands;
the third names the documents that read these routes and are revised on their own.

Amended now:

| File | Section | Change |
|---|---|---|
| `docs/specs/pgo.md` | *HTTP API* | the two `latest` routes; `latest` as a path segment and never an identifier; the media-type rule and its place in the step this section adds, with the argument that its ordering leaks nothing; the `details` items the body refusals carry; the `state=` filter's vocabulary |
| `docs/specs/pgo.md` | *Ceilings* | the machine form of `limit_exceeded`: the field as a pointer, the five codes, and the same code beside every `violations` entry |
| `docs/specs/pgo.md` | *Atomicity primitives*, *Paths that touch each key* | the active key's value carries the idempotency key and the principal, and a loser reads it to replay; rows for the key index and for a waiting request |
| `docs/specs/pgo.md` | *Algorithm*, *On-demand Collections* | `publish` carries the key and the principal, empty for a schedule; the losing branch replays instead of refusing; concurrent requests under one key yield one `202` and one `200` |
| `docs/specs/pgo.md` | *Record* | `idempotencyKey`, and the size arithmetic that still holds with it |
| `docs/specs/pgo.md` | *Create a Collection* | the `Idempotency-Key` contract: grammar, scope, where the key is stored, the lookup through the watched index, the `200` replay of the stored record, `409 idempotency_mismatch`, the conditional write that was already there, and the bound the guarantee carries |
| `docs/specs/pgo.md` | *List Collections* | the total order and why it needs two keys; `state`, `since`, `origin`, `limit`, and `cursor`; `nextCursor`; and the flat cap they replace |
| `docs/specs/pgo.md` | *Get a Collection* | `wait=`: what ends a wait, the clamp, `X-Wait-Elapsed`, the subscription over the watch the replica already holds, and what a wait costs |
| `docs/specs/pgo.md` | *The latest completed Collection* | a new subsection: the two routes, why only a `completed` record is the latest one, and the retention rule that makes the answer dependable |
| `docs/specs/pgo.md` | *Cancel*, *Errors*, *Non-disclosure* | renumbered to 10.7, 10.8, and 10.9; `409 idempotency_mismatch` and which codes carry `details`; the idempotency key inside a record a realm may read |
| `docs/specs/pgo.md` | *Logging* | `requestId` on the PGO record, the `wait` field, and a `latest` request naming the identifier that answered it |
| `docs/specs/pgo.md` | *Metrics* | the `latest` routes counted under `collection` and `collection_profile`; no label for `wait`, and what that costs the duration histogram |
| `docs/specs/pgo.md` | *Unit*, *End-to-end* | cases for the media type, the key, the wait, the two `latest` routes, the filters and the cursor, the cache's index and subscriptions, and the drain signal; scenario 1 gains the key, the wait, and the latest assertions |
| `docs/specs/pgo.md` | *Failure Scenarios* | rows for a replay and its bound, a mismatch, a missing media type, the three ways a wait ends, an expired latest, a cursor whose record is gone, and a policy that retains less than it collects |
| `docs/specs/gateway.md` | *HTTP API*, *Errors*, *Request identifier*, *The OpenAPI document*, *Logging*, *Health*, *Metrics*, *Startup and shutdown* | the gateway half of the same contract, listed in that document's own amendment block |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/api.md` | `Idempotency-Key`, `wait=`, the two `latest` routes, the listing filters and the cursor, the media type the two `POST` routes require, and `409 idempotency_mismatch` |
| `docs/pgo.md` | fetching the newest profile in one request, and what an idempotent create buys a script that loses its answer |
| `internal/pgo` | `idempotencyKey` and `snapshotHash` on the record, the `idem.<hash>` receipt its publisher writes, the caches' per-record subscriptions and generation broadcast, and `code` on `Violation` |
| `internal/httpapi` | the media-type check, the replay lookup, the wait handler, the two `latest` routes, and the listing's filters and cursor |
| `cmd/profgate` | the drain signal a waiting request selects on |
| `CHANGELOG.md` | the client-visible additions: the header, the parameter, the two routes, the filters and the cursor, the new `409`, and a `code` on every `violations` entry of `GET /pgo` |

Read these routes and are amended by the block below rather than by this one:
[`docs/specs/cli.md`](cli.md), [`docs/specs/ui.md`](ui.md),
and [`.agents/rules/100-project-map.md`](../../.agents/rules/100-project-map.md).

Making an idempotent create durable, and tightening the rest of that contract —
a receipt key that answers a replay from the store rather than from a watch,
a thin replay that discloses no more than the create it repeats,
one request-step order across the accepted designs,
a `wait=` that cannot lose a wake-up,
a cursor bound to its filters,
and a `latest` that confirms the artifact it names —
amends the following text.

| File | Section | Change |
|---|---|---|
| `docs/specs/pgo.md` | *Atomicity primitives*, *Paths that touch each key* | `idem.<hash>`: its one writer, its four paths, the sweep that removes it after the record, and that no watch or cache holds it; the `active.<ns>.<svc>` value loses the key and the principal, which nothing read |
| `docs/specs/pgo.md` | *Algorithm* | `publish` writes the receipt after winning the active key and before the record becomes claimable, and carries `snapshotHash` on the record |
| `docs/specs/pgo.md` | *Record* | `snapshotHash`, the canonical hash a replay compares, and the size arithmetic that still holds with it |
| `docs/specs/pgo.md` | *Sweeper* | the receipt is deleted after the record it names, with a `Keys("idem.")` pass for what a record deletion missed, and the cost that adds |
| `docs/specs/pgo.md` | *HTTP API* | the JSON media type is its own step immediately after the method step; `mime.ParseMediaType`, an essence of `application/json`, and every parameter accepted; the step order all four designs state identically |
| `docs/specs/pgo.md` | *Create a Collection* | the receipt key and value, the authoritative read every keyed request makes, the two indeterminate-write recoveries, the thin `{id, state}` replay and why a full record would disclose what `pgo.collect` alone does not admit, the canonical snapshot hash, how a loser resolves, and a guarantee that runs for the record's whole life |
| `docs/specs/pgo.md` | *List Collections* | the cursor carries `state`, `since`, and `origin` and is refused beside another set; stable order is promised for the records the listing already held |
| `docs/specs/pgo.md` | *Get a Collection* | registration before the first read, every pulse a hint followed by an authoritative read, a dropped pulse costing latency and not correctness, the generation broadcast a parked handler selects on, a deleted record answering `404`, and a `wait` above `60s` refused rather than clamped |
| `docs/specs/pgo.md` | *The latest completed Collection* | both routes walk completed candidates newest first, confirm each record and its object, expire a record whose object is gone, and share one selection |
| `docs/specs/pgo.md` | *Logging* | the `wait` audit field carries the duration asked for, there being no other |
| `docs/specs/pgo.md` | *Non-disclosure* | a replay is bounded by the receipt's hashed scope rather than by comparing principals, and the receipt is reachable through no route |
| `docs/specs/pgo.md` | *Unit*, *Failure Scenarios* | receipt cases in `internal/pgo` and `internal/httpapi`, the wait's lost-wakeup and generation cases, the cursor's filter binding, the latest walk, the collect-only replay; rows for a record written without a receipt, a stale receipt, a thin replay, a refused wait, a mismatched cursor, and a latest whose object is gone |
| `docs/specs/gateway.md` | *HTTP API*, *Request algorithm*, *Errors*, *Request identifier*, *The OpenAPI document*, *Logging* | the gateway half of the same tightening, listed in that document's own amendment block |
| `docs/specs/ui.md` | *Starting and cancelling a Collection*, *Required by this revision and not yet made* | a replay answers `{id, state}` rather than the record, and mismatch is on the effective snapshot; the rows this change satisfies leave that table |
| `docs/specs/cli.md` | *Collections*, *Testing* | `collect` reads the identifier out of a thin replay and polls the record only with `pgo.read`; mismatch is on the effective snapshot |
| `docs/specs/auth.md` | *Request algorithm* | the composed order gains the JSON media type step between method and readiness |
| `.agents/rules/100-project-map.md` | *Planned Structure*, *External HTTP API* | `internal/httpapi` embeds the OpenAPI document beside `internal/ui`'s console tree; the route list gains the two `latest` routes and `/v1/openapi.json`, and the console assets move to stable paths |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/api.md` | a replay answering `{id, state}` with `Location`, the cursor's filter binding, a `wait` above `60s` refused, and the artifact `latest` confirms |
| `internal/pgo` | the receipt writer and reader, the canonical snapshot hash, the sweeper's receipt rules, and the generation broadcast |
| `internal/httpapi` | the media-type step ahead of readiness, the receipt lookup and the thin replay, the register-then-read wait, the cursor's filters, and the latest walk |
| `CHANGELOG.md` | the client-visible moves: a replay's body, a `wait` above the grammar refused rather than clamped, and a cursor refused beside filters it was not minted under |

Accepting the command line of [`cli.md`](cli.md) amends the following text.

| File | Section | Change |
|---|---|---|
| `docs/specs/pgo.md` | *HTTP API* | a sentence naming [`cli.md`](cli.md) *Collections* as the first-party client that drives these routes, which changes no behavior: it sends what any client sends |
