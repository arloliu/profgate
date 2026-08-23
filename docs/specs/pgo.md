# Profgate PGO Collection

**Status:** Draft

Profile-Guided Optimization collection built on the gateway defined in
[`gateway.md`](gateway.md).
Everything there — permission boundary, discovery seam, HTTP API, realms,
configuration, testing — is assumed and not restated;
this document adds scheduling, collection, merge, and the NATS state that
coordinates gateway replicas.
It is a proposal carried over from the superseded draft
[`profgate-design.md`](profgate-design.md) and has not been settled.
Do not implement from it.

Additions this design makes to the gateway, each to be argued here before it
is written:

- `github.com/nats-io/nats.go` enters `go.mod`; the NATS clause of the
  permission boundary becomes active.
- `Target` in `internal/k8s` gains a build-identity field.
- Realms gain a `pgo` block (`read`, `collect`, `configure`).
- `internal/pgo/` and `internal/natskv/` packages appear.
- The container gains an `emptyDir` for ephemeral profile data.
- The `/v1/namespaces/{ns}/services/{svc}/collections` and `.../pgo` resources appear.

## Open questions carried from the draft

### Scheduling

- Exact semantics of `every` and `jitter`.
- Whether cron expressions are needed in addition to fixed intervals.
- How missed schedules are handled after a long outage.

### Replica sampling

- `all` versus fixed-count sampling.
- Behavior when replica count exceeds `maxParallel`.
- Whether node/zone-aware spreading is useful.

### Version identity

- Whether build identity should be mandatory for automated PGO.
- How rolling updates affect scheduled Collections.

### Artifact semantics

- Default retention.
- Maximum local disk usage.
- Whether NATS durable mode should be enabled globally or per service.

### Job retention

- How long completed Collection metadata remains in `PROFGATE_JOBS`.
- Whether old job state is deleted or compacted.

### Access control

- Whether some deployments disable HTTP mutation of `PROFGATE_CONFIG` entirely.
- Exact NATS subject/account ACLs required to constrain the gateway to predefined `PROFGATE_*` stores.
- Whether the internal artifact endpoint requires mTLS in addition to NetworkPolicy.

---

## 9. PGO Design Principles

PGO collection has different semantics from an interactive profile request.

Interactive profiling is:

```text
one request
-> one backend
-> one profile
```

PGO collection is:

```text
logical workload
-> multiple replicas
-> multiple samples
-> potentially multiple points in time
-> merged profile
```

For this reason PGO is modeled as an asynchronous **Collection**.

---

## 10. Short Collections, Long-Lived Scheduling

A key design decision is to avoid long-running Collection jobs.

For example, do not create one Collection that owns a worker for an hour:

```text
10:00 round 1
10:10 round 2
10:20 round 3
...
10:50 round 6
```

Instead prefer short independent Collections:

```text
10:00 Collection A
11:00 Collection B
12:00 Collection C
```

Each Collection may contain one or a small number of rounds lasting only a few minutes.

Temporal diversity is therefore provided by the **runtime scheduler**, not by keeping an individual async job alive for hours.

Benefits:

- gateway ownership leases remain short;
- worker crashes cause little lost work;
- ephemeral disk is practical;
- rolling Deployment updates are easier;
- runtime configuration changes naturally affect future Collections;
- recovery semantics remain simple.

---

## 11. Runtime PGO Configuration

Runtime PGO configuration is stored in a NATS KV bucket.

NATS is deliberately **not** used for authentication configuration, authorization realms, Kubernetes discovery, or the interactive pprof data path.

The configuration bucket is:

```text
PROFGATE_CONFIG
```

Suggested key layout:

```text
defaults

service.<namespace>.<service>
```

For example:

```text
defaults

service.payment.payment-api

service.checkout.checkout-api
```

### 11.1 Example configuration

```json
{
  "enabled": true,

  "schedule": {
    "every": "1h",
    "jitter": "5m"
  },

  "sampling": {
    "profile": "cpu",
    "duration": "30s",
    "rounds": 2,
    "roundInterval": "30s",
    "replicas": "all",
    "maxParallel": 8
  },

  "target": {
    "versionPolicy": "strict",
    "versionLabel": "app.kubernetes.io/version"
  },

  "artifact": {
    "retention": "2h",
    "durability": "ephemeral"
  }
}
```

### 11.2 Defaults and service overrides

Configuration is layered:

```text
defaults
   +
service-specific override
   =
effective config
```

Example defaults:

```json
{
  "schedule": {
    "every": "6h",
    "jitter": "10m"
  },
  "sampling": {
    "duration": "30s",
    "rounds": 2,
    "maxParallel": 8
  },
  "artifact": {
    "retention": "2h",
    "durability": "ephemeral"
  }
}
```

Service override:

```json
{
  "enabled": true,
  "sampling": {
    "rounds": 3
  }
}
```

### 11.3 Runtime propagation

Every gateway replica watches `PROFGATE_CONFIG`.

```text
             NATS KV
                |
        +-------+-------+
        |       |       |
        v       v       v
     gateway  gateway  gateway
        A       B       C
```

Each replica maintains a local read-only effective configuration cache.

Gateway restart does not lose configuration because the authoritative state remains in NATS KV.

The implementation should separate **configuration consumption** from **configuration authority**:

- every gateway requires `PROFGATE_CONFIG` read/watch access;
- write access is needed only when the runtime configuration API is enabled;
- high-security deployments may place writes behind a separate administrative gateway identity or an external configuration-management process.

This allows the profiling data path to remain operational without granting every gateway instance fleet-wide PGO policy mutation rights.

---

## 12. PGO Configuration API

Recommended API:

```text
GET    /v1/namespaces/{ns}/services/{svc}/pgo

PUT    /v1/namespaces/{ns}/services/{svc}/pgo

DELETE /v1/namespaces/{ns}/services/{svc}/pgo
```

Example:

```http
PUT /v1/namespaces/payment/services/payment-api/pgo
```

```json
{
  "enabled": true,
  "schedule": {
    "every": "1h"
  },
  "sampling": {
    "duration": "30s",
    "rounds": 2,
    "replicas": "all"
  }
}
```

### 12.1 Revision and optimistic concurrency

NATS KV revisions should be exposed as HTTP ETags.

Example:

```http
HTTP/1.1 200 OK
ETag: "42"
```

Update:

```http
PUT /v1/namespaces/payment/services/payment-api/pgo
If-Match: "42"
```

The gateway performs a revision-conditional KV update.

If another actor has already modified the configuration:

```http
HTTP/1.1 412 Precondition Failed
```

This avoids lost updates and gives the API well-defined concurrency semantics.

---

## 13. Configuration Snapshot Semantics

A Collection must use an immutable snapshot of the configuration that existed when the Collection was created.

Example:

```text
10:00 Collection C1 created using config revision 42

10:01 config updated to revision 43
      duration changed from 30s -> 60s
```

C1 continues to use revision 42.

The next scheduled Collection uses revision 43.

This guarantees reproducibility and makes troubleshooting straightforward.

Setting:

```json
{
  "enabled": false
}
```

means:

> Do not create new scheduled Collections.

It should not implicitly terminate an already running Collection.

Explicit cancellation should use a separate API:

```text
POST /v1/collections/{id}/cancel
```

---

## 14. Distributed Scheduler Without Leader Election

All gateway replicas may run the scheduler.

No permanent scheduler leader is required.

Assume all replicas determine that a service should run at 12:00.

Each attempts to create a deterministic scheduling key in `PROFGATE_JOBS`:

```text
schedule.<namespace>.<service>.<configRevision>.<timeSlot>
```

Example:

```text
schedule.payment.payment-api.42.20260823T120000
```

Using KV create-if-absent semantics:

```text
Profgate A -> create -> success
Profgate B -> create -> key exists
Profgate C -> create -> key exists
```

Only the winner creates the Collection.

This provides distributed de-duplication without:

- leader election;
- Redis;
- database transactions;
- Kubernetes Lease objects;
- CRDs.

### 14.1 Configuration revision as scheduler generation

Including the configuration revision in the scheduling identity also prevents stale timers from accidentally creating jobs using old policy.

A local timer generated from revision 42 may fire after revision 43 is already active.

Before creating the Collection, the scheduler checks that the timer revision is still current.

If it is stale, it is discarded.

---

## 15. Collection API

Create an on-demand Collection:

```text
POST /v1/namespaces/{ns}/services/{svc}/collections
```

Example:

```json
{
  "profile": "cpu",
  "sampling": {
    "duration": "30s",
    "rounds": 2,
    "replicas": "all",
    "maxParallel": 8
  }
}
```

Response:

```http
HTTP/1.1 202 Accepted
Location: /v1/collections/01K3XYZ
```

```json
{
  "id": "01K3XYZ",
  "state": "pending"
}
```

Query:

```text
GET /v1/collections/{id}
```

Download:

```text
GET /v1/collections/{id}/profile
```

Cancel:

```text
POST /v1/collections/{id}/cancel
```

---

## 16. Durable Collection State with NATS KV

Collection state is stored in:

```text
PROFGATE_JOBS
```

Suggested key:

```text
job.<collectionID>
```

Example value:

```json
{
  "id": "01K3XYZ",

  "namespace": "payment",
  "service": "payment-api",

  "configRevision": 42,

  "state": "pending",

  "createdAt": "2026-08-23T10:00:00+08:00",

  "attempt": 0
}
```

The collection record is the durable source of truth for asynchronous execution.

Gateway memory is only a cache.

---

## 17. Distributed Worker Ownership

Every gateway replica may watch the jobs bucket.

When a replica sees:

```text
state = pending
```

it may attempt to claim the Collection.

Suppose the current KV revision is `100`.

Profgate A attempts:

```text
revision 100:
pending
  ->
running
owner = profgate-a
```

Profgate B attempts the same update.

Only one conditional update succeeds.

The resulting state might be:

```json
{
  "state": "running",

  "owner": {
    "instance": "profgate-a/01K3INSTANCE",
    "pod": "profgate-7f88fdf79-xabcd"
  },

  "leaseUntil": "2026-08-23T10:10:30+08:00",

  "attempt": 1
}
```

The worker renews ownership periodically using revision-conditional updates.

---

## 18. Worker Failure and Recovery

Example:

```text
Profgate A owns Collection C1

round 1 complete
round 2 executing

Profgate A crashes
```

Because artifacts are stored on ephemeral local disk, another gateway cannot resume from A's local intermediate profile files.

Therefore the recommended recovery model is intentionally simple:

```text
attempt 1 lost
     |
     v
lease expires
     |
     v
Profgate B acquires job
     |
     v
attempt 2 starts Collection from the beginning
```

This provides:

```text
at-least-once Collection execution
```

without requiring distributed raw profile storage.

Because Collections are intentionally short, the amount of repeated profiling should remain small.

---

## 19. Collection Sampling Strategy

For a single Collection, representative replica sampling should be used.

Assume:

```text
payment-api
  pod A
  pod B
  pod C
  pod D
  pod E
```

For:

```json
{
  "rounds": 2,
  "replicas": "all"
}
```

a possible plan is:

```text
Round 0
A B C D E

Round 1
C E A D B
```

The gateway should shuffle target order between rounds.

Profiles within a round may be collected concurrently subject to:

```text
maxParallel
```

This avoids repeatedly profiling only one replica.

---

## 20. Target Resolution During Collection

A Collection stores its logical target:

```text
namespace
service
version/build constraints
```

It should not permanently pin the exact Pod list when the Collection is created.

Each round may resolve the current ready endpoints again.

Example:

```text
Round 0:
A B C

B is terminated during rollout

Round 1:
A C D
```

This allows the collection mechanism to tolerate ordinary Kubernetes lifecycle events.

---

## 21. Version and Build Identity

Profiles from incompatible binaries should not be merged into a single PGO profile.

A simple first implementation may use a configurable Pod label:

```text
app.kubernetes.io/version
```

Example:

```yaml
target:
  versionPolicy: strict
  versionLabel: app.kubernetes.io/version
```

If a Service currently routes to:

```text
v1.41:
  pod A
  pod B

v1.42:
  pod C
  pod D
```

a strict collection should not silently combine all four.

Possible behavior:

```http
HTTP/1.1 409 Conflict
```

unless a version is explicitly selected.

### 21.1 Stronger future identity

Human-readable versions are useful but imperfect.

A stronger binary identity may later use:

- Git commit SHA;
- build ID;
- image digest;
- application-provided profiling metadata.

Example Pod annotation:

```yaml
profiling.example.io/build-id: "sha256:abc..."
```

The Collection records the resolved build identity and rejects incompatible targets.

---

## 22. Local Artifact Storage

By default, raw and merged profiles are stored on a Pod `emptyDir`.

Example:

```text
/data/profgate/jobs/
  01K3XYZ/
    samples/
      round-000/
        pod-a.pprof
        pod-b.pprof

      round-001/
        pod-a.pprof
        pod-b.pprof

    merged.pprof
    manifest.json
```

Example Kubernetes configuration:

```yaml
volumes:
  - name: profgate-data
    emptyDir:
      sizeLimit: 2Gi
```

The Deployment should define appropriate ephemeral-storage requests and limits.

The rest of the container filesystem should be read-only. The gateway requires no host filesystem mount and no privileged filesystem access.

---

## 23. Artifact Ownership

Because artifacts are local, the job record must identify which gateway instance owns the completed artifact.

Example:

```json
{
  "state": "completed",

  "artifact": {
    "ownerInstance": "profgate-b/01K3INSTANCE",
    "podIP": "10.42.5.17",
    "file": "merged.pprof",
    "expiresAt": "2026-08-23T14:00:00+08:00"
  }
}
```

A request for:

```text
GET /v1/collections/01K3XYZ/profile
```

may arrive at Profgate A.

Profgate A reads the job record and sees that the artifact belongs to Profgate B.

It then internally proxies:

```text
client
  |
  v
Profgate A
  |
  | internal HTTP
  v
Profgate B
  |
  v
local emptyDir
```

The external client never needs to know where the artifact physically resides.

---

## 24. Internal Artifact Endpoint

Each gateway exposes an internal-only endpoint such as:

```text
GET /internal/v1/artifacts/{collectionID}/profile
```

This port should:

- not be exposed through public Ingress;
- be accessible only between gateway Pods;
- be protected with NetworkPolicy;
- validate Collection ownership.

The request should include the expected worker instance identity and Collection identity.

The serving gateway must verify that:

```text
job.artifact.ownerInstance == self.instanceID
collectionID == requested collection
artifact has not expired
```

This prevents accidentally retrieving an unrelated file if a Pod IP is recycled.

The internal port must not share the public Ingress route. NetworkPolicy should restrict it to gateway Pods only. A cluster-internal authentication mechanism such as mTLS or a short-lived signed internal token may be added if the network trust model requires defense in depth.

---

## 25. Artifact Lifecycle States

A completed Collection and its artifact lifecycle are separate concepts.

Recommended states include:

```text
pending
running
completed
failed
cancelled
artifact_lost
expired
```

### `completed`

Collection succeeded and the artifact is currently available.

### `artifact_lost`

Collection succeeded, but the Pod containing the ephemeral artifact disappeared before normal expiration.

### `expired`

The configured artifact retention period elapsed normally.

### `failed`

Profiling itself failed.

This distinction improves diagnostics and avoids incorrectly labeling ephemeral storage loss as a profiling failure.

---

## 26. Local Artifact Garbage Collection

Each gateway manages its own artifact cache.

Example policy:

```text
artifact retention = 2 hours
```

Additionally, define a disk high-water mark:

```text
disk > 80%
    ->
delete oldest completed artifacts
```

When deleting an artifact early because of disk pressure, the job record should be updated accordingly.

Raw samples may be deleted immediately after successful merge if they are not needed for debugging.

---

## 27. Optional Durable Artifact Mode

The default mode remains:

```json
{
  "artifact": {
    "durability": "ephemeral"
  }
}
```

For environments that need the final PGO profile to survive gateway Pod replacement:

```json
{
  "artifact": {
    "durability": "nats",
    "retention": "24h"
  }
}
```

Only the final artifacts need to be persisted:

```text
merged.pprof
manifest.json
```

Raw samples remain ephemeral.

An existing NATS JetStream Object Store may be used:

```text
PROFGATE_ARTIFACTS

<collectionID>/profile.pprof
<collectionID>/manifest.json
```

This avoids introducing:

- S3;
- MinIO;
- PVCs;
- an additional database.

The HTTP API does not expose which storage mode is being used.

---

## 28. PGO Manifest

Every merged profile should have a manifest describing how it was produced.

Example:

```json
{
  "collection": "01K3XYZ",

  "namespace": "payment",
  "service": "payment-api",

  "configRevision": 42,

  "profile": "cpu",

  "target": {
    "version": "1.42.3",
    "buildID": "sha256:abc..."
  },

  "sampling": {
    "duration": "30s",
    "rounds": 2,
    "requestedReplicas": "all"
  },

  "samples": [
    {
      "round": 0,
      "pod": "payment-api-7c8f8c9b9-a",
      "podUID": "...",
      "startedAt": "...",
      "duration": "30s"
    },
    {
      "round": 0,
      "pod": "payment-api-7c8f8c9b9-b",
      "podUID": "...",
      "startedAt": "...",
      "duration": "30s"
    }
  ]
}
```

The manifest is valuable for:

- reproducibility;
- troubleshooting;
- auditing;
- determining whether a profile is safe to use for a particular build.

---

## 29. Profile Merge

The collector merges the successfully collected CPU profiles into one PGO-compatible pprof profile.

The implementation should preferably use a Go pprof profile library directly rather than spawning an external `go tool pprof` process.

Pseudo-flow:

```text
collect profile A
collect profile B
collect profile C
      |
      v
parse profiles
      |
      v
verify compatible identity
      |
      v
merge
      |
      v
merged.pprof
```

The merged artifact becomes the primary Collection result.

---

## 30. Concurrency and Production Safety

Profiling may add runtime overhead, especially CPU profiling and tracing.

The gateway should therefore provide limits at several levels.

### 30.1 Global collector limit

```text
max active profiling requests per gateway
```

### 30.2 Collection-level limit

```json
{
  "sampling": {
    "maxParallel": 8
  }
}
```

### 30.3 Per-target protection

A Pod should generally not receive multiple expensive profiling operations simultaneously.

The collector should attempt to avoid selecting a Pod already being profiled by another Collection.

For the first implementation, this may be enforced conservatively using a target-level claim in NATS KV if necessary.

### 30.4 Profile-specific limits

Example server policy:

```yaml
profiles:
  cpu:
    enabled: true
    maxDuration: 120s

  trace:
    enabled: true
    maxDuration: 10s

  heap:
    enabled: true

  goroutine:
    enabled: true
    allowDebugText: false
```

Client-supplied durations must never bypass server-side policy.

---

## 33. Why JetStream Work Queues Are Not Required Initially

A JetStream work queue could model:

```text
collection request
   ->
publish work item
   ->
durable consumer
   ->
collector
```

This provides useful features such as:

- redelivery;
- backpressure;
- consumer concurrency.

However, the initial workload is expected to consist of relatively low-frequency, long-running, expensive operations rather than a very high volume of tiny tasks.

NATS KV already provides the essential mechanisms:

- durable state;
- watching;
- revision-based optimistic concurrency;
- create-if-absent de-duplication;
- lease-like ownership records.

A JetStream work queue can be added later without changing the external API or Collection model.

---

## 34. Failure Scenarios

### Gateway crashes during interactive profile

The HTTP request fails.

No durable recovery is required.

### Gateway crashes during Collection

Its ownership lease expires.

Another gateway claims the job and restarts the Collection from the beginning.

### Gateway crashes after Collection completed

If the artifact is ephemeral:

```text
completed -> artifact_lost
```

If NATS artifact durability is enabled, another gateway can continue serving the artifact.

### NATS temporarily unavailable

Interactive profiling may remain available if target resolution and authorization do not depend on NATS.

PGO configuration changes, scheduling, and async Collection transitions should fail closed until NATS is available again.

Workers should not assume ownership when durable coordination cannot be confirmed.

### Unauthorized or compromised configuration writer

A configuration writer can influence profiling frequency and therefore production overhead.

For this reason:

- server-side hard limits override all runtime policy;
- `duration`, `rounds`, `maxParallel`, and schedule frequency must have enforced ceilings;
- config history records the actor and revision;
- environments with stricter controls should separate `PROFGATE_CONFIG` write credentials from normal gateway credentials.

Runtime configuration is policy input, not authority to bypass safety limits.

### Kubernetes API temporarily unavailable

Cached target information may be used for interactive requests only if a clearly defined freshness policy allows it.

New PGO Collection rounds should preferably wait until reliable target resolution is available.

---

## 35. Recommended Initial Data Model

### PGO Policy

```text
PGOPolicy

enabled

schedule
  every
  jitter

sampling
  profile
  duration
  rounds
  roundInterval
  replicas
  maxParallel

target
  versionPolicy
  versionLabel

artifact
  durability
  retention
```

### Collection

```text
Collection

id
namespace
service

configRevision
configSnapshot

state
attempt

owner
leaseUntil

resolvedVersion
resolvedBuildIdentity

progress

artifact

createdAt
startedAt
completedAt
```

---

