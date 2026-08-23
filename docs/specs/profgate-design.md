# Profgate — Design

**Status:** Draft

Direction is not settled. This document is a proposal under discussion:
read it for context and revise it freely, but do not implement from it.
It becomes the design of record when its Status reaches `Accepted`.

**Scope:** Profgate — a lightweight standalone pprof gateway for Kubernetes 1.23+ environments  
**Primary use cases:** On-demand production pprof access and lightweight asynchronous PGO collection

---

## 1. Overview

Production Kubernetes environments commonly run many Go-based services across Deployments, StatefulSets, and other workload types. Exposing `/debug/pprof` through an Ingress for every service creates an unnecessarily large attack surface and makes access control difficult to standardize.

This proposal introduces **Profgate**, a **lightweight, standalone Kubernetes-aware pprof gateway** that provides a single HTTP entry point for profiling Go workloads.

Profgate is intentionally designed for environments where deploying a complete continuous-profiling stack such as Grafana Alloy + Pyroscope or Parca would be operationally disproportionate to the problem being solved. Those systems remain appropriate when long-term profiling storage, fleet-wide historical analysis, flamegraph exploration, or continuous profiling are required; they are not dependencies of this design.

Profgate is responsible for:

- Resolving a Kubernetes Service into its current backend Pods.
- Selecting a specific backend or a representative backend sample.
- Proxying standard Go pprof profiles such as CPU, heap, goroutine, mutex, block, allocs, and trace.
- Collecting representative CPU profiles for Profile-Guided Optimization (PGO).
- Supporting asynchronous PGO collection without requiring Kubernetes CRDs.
- Allowing PGO collection policy to be updated at runtime.
- Remaining horizontally scalable and stateless from the Kubernetes workload perspective.
- Avoiding external object storage by default.
- Reusing an existing NATS JetStream deployment for durable control-plane state when available.

The central design principle is:

> Treat a Kubernetes Service as a logical workload identity rather than as the network destination used to fetch profiles.

The gateway resolves the Service to concrete Pod endpoints and connects directly to those Pods.

### 1.1 Core design decisions

The initial design deliberately favors a small operational and security footprint:

1. **No Kubernetes CRDs.**
2. **No Kubernetes write permissions.**
3. **No sticky sessions or stateful gateway Pods.**
4. **NATS KV is the durable coordination and configuration plane.**
5. **Profile bytes remain ephemeral by default.**
6. **Only final merged artifacts may optionally be persisted through NATS Object Store.**
7. **Scheduled PGO collections are short-lived; temporal diversity comes from repeated scheduling rather than hour-long jobs.**
8. **All gateway replicas may schedule and execute work; coordination uses NATS KV revision/CAS semantics rather than permanent leader election.**
9. **Kubernetes Service discovery is read-only and resolves through EndpointSlices to concrete Pods.**
10. **The externally reachable API and the internal artifact-transfer path are separate trust boundaries.**
11. **Kubernetes 1.23 is the compatibility baseline; only stable API fields available at that baseline are required.**
12. **Authentication is optional and static: disabled, HTTP Basic Auth, or OIDC.**
13. **Authorization uses simple static access realms rather than a dynamic IAM subsystem.**
14. **The gateway does not depend on Grafana, Pyroscope, Parca, Prometheus, or another profiling backend.**

### 1.2 Project naming

The project and repository name is:

```text
profgate
```

Recommended Go module and binary naming:

```text
github.com/<org>/profgate
profgate
```

Example commands:

```bash
profgate serve
profgate config validate
profgate version
```

Kubernetes object names should use the same stable prefix where practical:

```text
Deployment/profgate
Service/profgate
ServiceAccount/profgate
ClusterRole/profgate
```

The external HTTP API should remain product-neutral and resource-oriented:

```text
/v1/namespaces/{namespace}/services/{service}/profiles/{profile}
/v1/namespaces/{namespace}/services/{service}/collections
```

The name `Profgate` should not be embedded into versioned API resource paths.

---

## 2. Goals

### 2.1 Security

- Expose only one externally reachable profiling endpoint.
- Do not expose application pprof ports through public Ingress resources.
- Allow authentication and authorization to be enforced centrally when enabled.
- Support a deliberately simple authentication model: disabled, HTTP Basic Auth, or OIDC.
- Keep authorization policy static and easy to audit.
- Restrict application pprof ports with Kubernetes NetworkPolicy.
- Use a dedicated Kubernetes ServiceAccount.
- Require only read access to Services, Pods, and EndpointSlices.
- Require no Kubernetes object creation, update, patch, or delete permissions.
- Require no access to Secrets, Pod logs, Pod exec, or Pod port-forward.
- Scope NATS access to dedicated `PROFGATE_*` stores.
- Do not grant the gateway JetStream administrative permissions.
- Run the gateway as a non-root container with no Linux capabilities.
- Avoid CRDs, privileged containers, host networking, host PID access, and host filesystem mounts.

### 2.2 Operational simplicity

- Deploy as a normal Kubernetes Deployment.
- Do not require a database, Redis, Kafka, PVC, S3, or MinIO.
- Reuse NATS JetStream/KV if durable coordination is required.
- Support multiple gateway replicas without sticky sessions.
- Allow gateway Pods to restart or roll independently.
- Support Kubernetes 1.23 and newer clusters without requiring version-specific cluster features.
- Keep interactive profiling usable even when NATS is temporarily unavailable.
- Keep the standalone deployment small: a gateway Deployment plus NATS only for scheduled/asynchronous PGO state.

### 2.3 Profiling

Support standard Go profiling endpoints:

- CPU
- heap
- allocs
- goroutine
- mutex
- block
- threadcreate
- trace

### 2.4 Compatibility

- Use only stable Kubernetes APIs available in Kubernetes 1.23.
- Avoid reliance on recently introduced EndpointSlice fields or topology features.
- Treat Kubernetes 1.23 and 1.24 as first-class integration-test targets.
- Keep Kubernetes client/version-specific logic behind a narrow discovery adapter.
- Preserve compatibility with newer clusters by relying on stable core APIs.

### 2.5 PGO

- Collect CPU profiles from multiple replicas.
- Sample workloads across time.
- Avoid accidentally combining profiles from incompatible application versions or builds.
- Make PGO collection configurable at runtime without restarting the gateway.
- Provide deterministic metadata describing how a PGO profile was produced.

---

## 3. Non-Goals

The initial implementation is not intended to be:

- A continuous profiling platform comparable to Parca or Pyroscope.
- A general-purpose Kubernetes job scheduler.
- A long-term profile archive.
- A replacement for application metrics or tracing.
- A generic TCP reverse proxy.
- A Kubernetes operator.
- A Grafana/Pyroscope/Parca replacement.
- A profile database or historical query engine.
- A general identity-management or policy-management system.
- A requirement to install a broader Grafana observability stack.

Profgate should remain a relatively small production utility rather than becoming a full observability platform.

### 3.1 Relationship to existing profiling systems

Existing continuous-profiling systems already solve much larger problems:

```text
Alloy / Pyroscope / Parca
    continuous collection
    historical storage
    profile query
    visualization
    fleet-wide analysis
```

Profgate intentionally solves a narrower problem:

```text
Profgate
    secure or internal single entry point
    Kubernetes Service -> Pod resolution
    on-demand raw pprof / trace access
    lightweight scheduled PGO sampling
    short-lived local artifacts
```

If an environment later adopts a full continuous-profiling platform, the PGO scheduler may be disabled while the interactive gateway remains useful. The external API should therefore avoid coupling itself to the internal PGO implementation.

---

## 4. High-Level Architecture

```text
                         Developers / CI / SSO
                                  |
                             HTTP / HTTPS
                                  v
                         +-------------------+
                         |  Load Balancer /  |
                         |      Ingress      |
                         +---------+---------+
                                   |
               +-------------------+-------------------+
               |                   |                   |
               v                   v                   v
        +-------------+     +-------------+     +-------------+
        | Profgate A  |     | Profgate B  |     | Profgate C  |
        |             |     |             |     |             |
        | HTTP API    |     | HTTP API    |     | HTTP API    |
        | Auth (opt.) |     | Auth (opt.) |     | Auth (opt.) |
        | Scheduler   |     | Scheduler   |     | Scheduler   |
        | Collector   |     | Collector   |     | Collector   |
        | emptyDir    |     | emptyDir    |     | emptyDir    |
        +------+------+     +------+------+     +------+------+
               |                   |                   |
               +-------------------+-------------------+
                                   |
                            NATS JetStream
                                   |
                     +-------------+-------------+
                     |                           |
                     v                           v
              PROFGATE_CONFIG KV             PROFGATE_JOBS KV
              runtime policy              async job state
              revisions                   ownership
              history                     leases
              scheduling                  progress

                                   |
                                   | Kubernetes API
                                   v

             Service -> EndpointSlice -> Pod -> PodIP:pprofPort
```

NATS is required only for runtime PGO configuration and asynchronous/scheduled Collection coordination. Interactive profiling remains a direct Kubernetes-discovery + HTTP data path and can continue operating during a NATS outage.

An optional NATS Object Store may be enabled for durable storage of only the final merged PGO profile and manifest.

---


## 5. Required Privileges and Trust Boundaries

The gateway should be deployable with a narrow, auditable permission set.

### 5.1 Kubernetes RBAC

The required Kubernetes API access is:

| API group | Resource | Verbs | Purpose |
|---|---|---|---|
| core (`""`) | `services` | `get`, `list`, `watch` | Validate logical targets and read Service metadata |
| core (`""`) | `pods` | `get`, `list`, `watch` | Resolve Pod identity, labels, annotations, node name, and build/version metadata |
| `discovery.k8s.io` | `endpointslices` | `get`, `list`, `watch` | Discover the current ready Service backends |

Recommended role definition:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: Profgate
rules:
  - apiGroups: [""]
    resources:
      - services
      - pods
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - discovery.k8s.io
    resources:
      - endpointslices
    verbs:
      - get
      - list
      - watch
```

The ClusterRole defines a reusable permission set. It does **not** imply cluster-wide access.

For environments where profiling is allowed only in selected namespaces, bind the ClusterRole through namespace-local `RoleBinding` objects:

```text
ClusterRole/Profgate
        |
        +-- RoleBinding in payment
        +-- RoleBinding in checkout
        +-- RoleBinding in order
```

Use a `ClusterRoleBinding` only when cluster-wide discovery is explicitly required.

Namespace-scoped bindings trade some informer simplicity for a significantly smaller blast radius and are recommended where the number of profiled namespaces is manageable.

### 5.2 Explicitly unnecessary Kubernetes permissions

The gateway does not require access to:

```text
pods/exec
pods/log
pods/portforward

secrets
configmaps
nodes

deployments
replicasets
statefulsets
daemonsets

networkpolicies
events

customresourcedefinitions
```

It also does not require any Kubernetes verbs such as:

```text
create
update
patch
delete
deletecollection
```

The gateway profiles applications over ordinary HTTP connections to their pprof ports; it does not attach to application processes or exec into Pods.

### 5.3 Why Deployment and StatefulSet access is unnecessary

The target model deliberately stops at the Pod boundary:

```text
Service
   |
   v
EndpointSlice
   |
   v
Pod
```

Whether the Pod is controlled by a Deployment, StatefulSet, DaemonSet, or another controller is not relevant to profiling.

This keeps the gateway independent of workload-controller topology and avoids unnecessary `apps/*` RBAC access.

### 5.4 Node access is not required initially

The Pod object already exposes `spec.nodeName`, which is sufficient for recording which node hosted a sample.

Node API access should only be added if a future feature genuinely requires node labels or topology data that cannot be obtained from EndpointSlice and Pod metadata.

### 5.5 NATS permissions

The gateway uses dedicated NATS stores:

```text
PROFGATE_CONFIG
PROFGATE_JOBS
PROFGATE_ARTIFACTS   # optional
```

Required logical access:

| Store | Access | Purpose |
|---|---|---|
| `PROFGATE_CONFIG` | read, watch; optional write | Runtime PGO policy |
| `PROFGATE_JOBS` | read, watch, create, update, cleanup | Async state, ownership, scheduling de-duplication |
| `PROFGATE_ARTIFACTS` | get/put/delete, optional | Final merged profile durability |

The gateway should **not** be allowed to create, delete, or reconfigure arbitrary JetStream streams, KV buckets, or Object Store buckets in production.

Infrastructure provisioning should create the required stores before the gateway starts.

### 5.6 PGO configuration authority

The HTTP API may expose:

```text
GET    .../pgo
PUT    .../pgo
DELETE .../pgo
```

Whether a user may call the write operations is decided by the user's static access realm.

The gateway therefore normally has read/write access to its dedicated `PROFGATE_CONFIG` bucket, while user-level authorization controls access to configuration mutation.

For especially restrictive environments, PGO policy writes may instead be disabled in the gateway and managed by an external process. This is an optional hardening mode rather than a requirement for the base design.

Authentication credentials and authorization realms are **not** stored in `PROFGATE_CONFIG`; they remain static process configuration.

### 5.7 Network trust boundaries

Required flows are:

```text
External Ingress
      |
      v
profgate public port

Profgate
      |
      +--> Kubernetes API :443
      |
      +--> NATS
      |
      +--> application PodIP:pprof-port
      |
      +--> other Profgate Pods on internal artifact port
```

Application pprof ports should not be reachable through normal public application Ingress resources.

A NetworkPolicy should allow the pprof port only from the gateway identity, ideally using both namespace and Pod selectors.

The internal gateway-to-gateway artifact endpoint must also be excluded from public Ingress routing.

### 5.8 Container permissions

The gateway requires only write access to its dedicated ephemeral profile directory.

Recommended container hardening:

```yaml
securityContext:
  runAsNonRoot: true
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop:
      - ALL
```

Use an `emptyDir` for writable profile data:

```yaml
volumes:
  - name: profgate-data
    emptyDir:
      sizeLimit: 2Gi
```

The gateway does not require:

```text
privileged mode
SYS_PTRACE
SYS_ADMIN
hostPID
hostNetwork
hostPath
```

### 5.9 ServiceAccount

Use a dedicated ServiceAccount:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: Profgate
```

Because the gateway reads the Kubernetes API, it requires a projected ServiceAccount credential. The ServiceAccount should not be reused by unrelated workloads.

### 5.10 Security audit summary

A compromised gateway is intentionally limited to the following capabilities.

It **can**:

- read Service, Pod, and EndpointSlice metadata in authorized namespaces;
- discover backend Pod addresses;
- connect to pprof ports explicitly allowed by network policy;
- read and update its dedicated NATS job state;
- read runtime PGO policy;
- modify runtime PGO policy only if that deployment mode explicitly permits it;
- read/write final NATS profiling artifacts only if durable artifact mode is enabled.

It **cannot**:

- exec into application Pods;
- read Kubernetes Secrets;
- read arbitrary Pod logs;
- port-forward through the Kubernetes API;
- create or mutate application Pods;
- modify Services or EndpointSlices;
- modify Deployments or StatefulSets;
- modify NetworkPolicies;
- create CRDs;
- write arbitrary Kubernetes resources;
- administer JetStream;
- access the host filesystem;
- attach to application processes through ptrace.

---


## 6. Kubernetes Compatibility Strategy

### 6.1 Minimum supported version

The initial compatibility baseline is:

```text
Kubernetes >= 1.23
```

Kubernetes 1.23 already provides the stable:

```text
discovery.k8s.io/v1
EndpointSlice
```

API, so the gateway does not need to depend on the older `discovery.k8s.io/v1beta1` API for the supported baseline.

The project should explicitly integration-test at least:

```text
1.23
1.24
one representative modern Kubernetes release
```

and should not infer compatibility solely from the version of `client-go` used to build the binary.

### 6.2 Conservative EndpointSlice field usage

For maximum compatibility, target resolution should depend only on fields that are part of the Kubernetes 1.23 `discovery.k8s.io/v1` API:

```text
metadata.labels["kubernetes.io/service-name"]

addressType
ports

endpoints[].addresses
endpoints[].conditions.ready
endpoints[].targetRef
endpoints[].nodeName
```

The initial implementation should **not require** newer or optional topology/routing behavior such as:

```text
trafficDistribution
advanced topology hints
new endpoint routing policies
newer Service traffic features
```

For eligibility, use conservative semantics:

```text
EndpointSlice condition ready != false
AND
targetRef.kind == "Pod"
AND
Pod.status.phase == Running
AND
Pod Ready condition == True
```

Cross-checking Pod readiness avoids accidentally profiling a backend that appears usable only because a Service is configured with unusual `publishNotReadyAddresses` semantics.

### 6.3 EndpointSlice aggregation

A Service may be represented by multiple EndpointSlices.

The resolver must:

```text
list/watch all EndpointSlices in the namespace
    where kubernetes.io/service-name == service

aggregate all eligible endpoints

deduplicate by Pod UID / targetRef
```

No assumption should be made that one Service maps to exactly one EndpointSlice.

### 6.4 Legacy Endpoints fallback

A `core/v1 Endpoints` fallback is **not required** for the Kubernetes 1.23+ support target.

Avoiding the fallback keeps:

- the implementation simpler;
- RBAC narrower;
- one discovery model across all supported clusters.

If future deployments require Kubernetes older than 1.23 or a vendor cluster with EndpointSlice disabled, legacy `Endpoints` support may be introduced as an explicit compatibility mode rather than silently becoming part of the default path.

Such a mode would require additional RBAC:

```text
core/v1 endpoints: get/list/watch
```

and should therefore remain opt-in.

### 6.5 Client-go compatibility policy

Kubernetes API stability and `client-go` release compatibility are related but not identical.

The implementation should isolate Kubernetes access behind an internal interface such as:

```go
type TargetDiscovery interface {
    ResolveService(ctx context.Context, namespace, service string) ([]Target, error)
    GetPod(ctx context.Context, namespace, name string) (*PodInfo, error)
}
```

This allows the project to:

- upgrade `client-go` without changing profiling logic;
- test the same behavior against 1.23, 1.24, and modern API servers;
- avoid leaking newer Kubernetes fields into the core data model.

The preferred dependency policy is:

> Use the newest practical `client-go` version that passes the project's minimum-version integration suite rather than pinning indefinitely to an unmaintained Kubernetes 1.23-era client library.

### 6.6 ServiceAccount behavior across 1.23 and 1.24

The gateway should rely on normal in-cluster ServiceAccount credentials and must not assume that a permanent Secret-backed ServiceAccount token exists.

This matters because Kubernetes 1.24 changed legacy token auto-generation behavior.

The gateway therefore reads the standard in-cluster projected token through the Kubernetes client configuration and requires no direct Secret API access.

### 6.7 Port discovery

The application Service does not need to expose the pprof port.

EndpointSlice is used primarily to discover backend Pod identity and address.

The pprof port should be resolved from static gateway configuration, with optional per-Service metadata override:

```yaml
profiling:
  defaultPort: 6060
  defaultPortName: pprof
```

A future annotation convention may override the port:

```yaml
profiling.example.io/port: "6060"
```

This preserves the security property that the normal application Service does not have to advertise pprof as a routable Service port.

### 6.8 Non-Pod and selectorless Services

By default, only EndpointSlice entries whose `targetRef` identifies a Pod are eligible.

External, manually managed, or selectorless Service endpoints are rejected unless a future explicit configuration mode enables them.

This keeps version/build validation and authorization tied to Kubernetes workload identity.

---

## 7. Kubernetes Target Resolution

### 7.1 Service as logical identity

A request identifies a workload using:

```text
namespace + service
```

The gateway does not normally send the profiling request to the Service ClusterIP.

The resolver uses `discovery.k8s.io/v1 EndpointSlice` and Pod metadata only; it does not depend on workload-controller APIs.

Instead:

```text
Service
   |
   v
EndpointSlices
   |
   v
Ready endpoints
   |
   v
Pod identities
   |
   v
Pod IP + pprof port
```

This provides control over exactly which replica is profiled.

### 7.2 Why direct Pod selection matters

Using the Service ClusterIP would delegate backend selection to kube-proxy or the cluster networking implementation. That makes the actual profiled instance unpredictable and prevents reliable:

- replica sampling;
- version filtering;
- rollout handling;
- Pod-specific diagnostics;
- distributed PGO sampling.

### 7.3 Workload abstraction

The gateway should not care whether a Pod belongs to:

- Deployment;
- StatefulSet;
- DaemonSet;
- another controller.

The relevant abstraction is simply:

```text
Service -> EndpointSlice -> Pod
```

### 7.4 Kubernetes permissions

The gateway should ideally need only read access:

```yaml
resources:
  - services
  - pods
  - endpointslices
verbs:
  - get
  - list
  - watch
```

No Kubernetes write permissions are required.

Shared informers should be used rather than querying the Kubernetes API for every profiling request.

---

## 8. Interactive Profiling API

Recommended resource structure:

```text
GET /v1/namespaces/{namespace}/services/{service}/targets

GET /v1/namespaces/{namespace}/services/{service}/profiles/{profile}
```

Examples:

```text
GET /v1/namespaces/payment/services/payment-api/profiles/heap

GET /v1/namespaces/payment/services/payment-api/profiles/cpu?seconds=30

GET /v1/namespaces/payment/services/payment-api/profiles/trace?seconds=5

GET /v1/namespaces/payment/services/payment-api/profiles/mutex
```

### 8.1 Target selection

Optional query parameters may include:

```text
?pod=payment-api-7c8f8c9b9-xabcd

?version=1.42.3

?strategy=random
```

The default selection strategy for an ordinary single-target profile may be `random`.

The response should expose the actual selected backend in headers:

```http
X-Pprof-Target-Pod: payment-api-7c8f8c9b9-xabcd
X-Pprof-Target-Version: 1.42.3
X-Pprof-Target-Node: worker-07
```

### 8.2 Gateway profile vocabulary

The external API should not expose Go implementation paths directly.

Example mapping:

```go
var profilePaths = map[string]string{
    "cpu":          "/debug/pprof/profile",
    "heap":         "/debug/pprof/heap",
    "allocs":       "/debug/pprof/allocs",
    "goroutine":    "/debug/pprof/goroutine",
    "mutex":        "/debug/pprof/mutex",
    "block":        "/debug/pprof/block",
    "threadcreate": "/debug/pprof/threadcreate",
    "trace":        "/debug/pprof/trace",
}
```

This decouples the Profgate API from the backend implementation.

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

## 31. Security Model

The gateway supports three authentication modes:

```text
disabled
basic
oidc
```

The default may be `disabled` for trusted developer/internal environments.

Authentication and authorization configuration is static. It is loaded at process startup from the gateway configuration and is not stored in NATS.

### 31.1 Authentication disabled

Example:

```yaml
auth:
  mode: disabled
```

Requests are treated as an anonymous internal developer principal.

A configured anonymous/default access realm still controls which namespaces, services, profile types, and PGO operations are available.

This mode is appropriate when the gateway is reachable only through a trusted development network, VPN, private ingress, or similarly restricted environment.

When authentication is disabled, the gateway should emit a clear startup log warning:

```text
authentication disabled; access is controlled only by network boundary and static realm policy
```

The service should not silently imply that the endpoint is authenticated.

### 31.2 HTTP Basic Auth

Basic Auth provides the smallest useful authentication option.

Example:

```yaml
auth:
  mode: basic

  basic:
    httpRealm: Profgate

    users:
      alice:
        passwordHash: "$2a$..."
        accessRealm: developer

      platform:
        passwordHash: "$2a$..."
        accessRealm: production-read
```

Passwords must not be stored as plaintext in normal configuration.

The gateway should support a strong password hash format such as bcrypt and compare credentials in a timing-safe manner.

Credentials may be supplied through a Kubernetes Secret mounted as a file or environment source by the Pod specification. The gateway does **not** need Kubernetes Secret API permissions to consume a mounted Secret.

Basic Auth is particularly useful for:

```text
curl
go tool pprof
small CI jobs
internal developer use
```

### 31.3 OIDC SSO

OIDC is the production-friendly SSO mode.

Example:

```yaml
auth:
  mode: oidc

  oidc:
    issuerURL: https://sso.example.com
    clientID: Profgate
    redirectURL: https://profgate.example.com/auth/callback

    access:
      groupsClaim: groups

      groupRealms:
        platform-engineering: production
        backend-developers: developer
```

The minimal OIDC implementation should support:

1. Authorization Code flow with PKCE for browser users.
2. Secure, signed/encrypted session cookies.
3. Bearer-token validation for API/CLI integrations where an OIDC access or ID token is already available.
4. Issuer, audience, expiry, and signature validation using the provider metadata/JWKS.

Gateway session state should remain stateless. Session cookies are protected with a shared static signing/encryption key mounted into every gateway replica.

No NATS-backed login session database is required.

OIDC client secrets and cookie keys may be mounted through Kubernetes Secrets without granting the gateway Secret API read permissions.

### 31.4 Static access realms

Authorization is intentionally simple.

The gateway does not implement:

```text
dynamic RBAC objects
policy database
OPA dependency
per-request Kubernetes SubjectAccessReview
NATS-hosted authorization policy
```

Instead, static configuration defines named access realms.

Example:

```yaml
authorization:
  anonymousRealm: developer

  realms:
    developer:
      namespaces:
        - dev
        - staging

      services:
        - "*"

      profiles:
        - cpu
        - heap
        - allocs
        - goroutine
        - mutex
        - block
        - trace

      pgo:
        read: true
        collect: true
        configure: false

    production-read:
      namespaces:
        - payment
        - checkout

      services:
        - "*"

      profiles:
        - cpu
        - heap
        - goroutine

      pgo:
        read: true
        collect: true
        configure: false

    production-admin:
      namespaces:
        - "*"

      services:
        - "*"

      profiles:
        - "*"

      pgo:
        read: true
        collect: true
        configure: true
```

Authorization evaluation is:

```text
authenticated principal
        |
        v
static access realm
        |
        +--> namespace allowed?
        +--> service allowed?
        +--> profile type allowed?
        +--> PGO operation allowed?
```

This is intentionally much simpler than general RBAC.

### 31.5 Realm mapping

For Basic Auth:

```text
username -> accessRealm
```

For OIDC:

```text
group claim -> accessRealm
```

Optionally, exact email/subject mappings may be supported:

```yaml
oidc:
  access:
    subjectRealms:
      "00u123...": production-admin
```

If multiple OIDC groups match, the implementation should use a deterministic rule such as unioning permissions or selecting the explicitly highest-priority realm. The first implementation should choose one rule and document it rather than adding a complex policy language.

### 31.6 Application Pods

Application Pods expose an internal pprof port such as:

```text
:6060
```

The normal application Service does not need to expose this port.

### 31.7 NetworkPolicy

Only the pprof gateway should be allowed to connect to application pprof ports where the cluster networking model supports enforcing that restriction.

Conceptually:

```text
developer
   |
   | HTTP/HTTPS
   v
pprof gateway
   |
   | allowed by NetworkPolicy
   v
PodIP:6060
```

Direct access from unrelated workloads should be denied where practical.

### 31.8 Sensitive output

pprof and trace data may expose:

- internal package names;
- stack traces;
- application behavior;
- request-related strings;
- implementation details.

Profiling data should therefore be treated as sensitive production debugging information even when the gateway itself is deployed primarily for developer use.

### 31.9 Authentication and safety are separate

Authentication must not control production-safety limits.

Even a fully authorized administrator cannot request arbitrary profiling load beyond static server ceilings.

For example:

```yaml
safety:
  cpu:
    maxDuration: 120s

  trace:
    maxDuration: 10s

  collection:
    maxParallel: 16
    minScheduleInterval: 15m
```

Runtime PGO policy and authenticated users may request values only within these limits.

## 32. Auditability

Runtime configuration updates should record the caller identity when authentication is enabled, or `anonymous` when the gateway intentionally runs with authentication disabled.

Example KV value metadata:

```json
{
  "spec": {
    "...": "..."
  },

  "meta": {
    "updatedAt": "2026-08-23T10:01:00+08:00",
    "updatedBy": "user@example.com"
  }
}
```

NATS KV history may be used to inspect previous revisions.

A future HTTP endpoint may expose configuration history:

```text
GET /v1/namespaces/{ns}/services/{svc}/pgo/history
```

This is especially useful in production environments where configuration changes require traceability.

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

## 36. Recommended v1 Deployment Model

Use one binary and one Deployment initially.

Example:

```text
Profgate
replicas: 3
```

Every replica runs:

- HTTP API;
- optional Basic/OIDC authentication;
- static realm authorization;
- Kubernetes informer/cache;
- NATS config watcher when PGO scheduling is enabled;
- scheduler;
- Collection worker;
- local artifact GC.

Interactive profiling requires only Kubernetes API access and network connectivity to target Pod pprof ports. NATS is needed for runtime/scheduled PGO functionality, not for basic proxy operation.

A typical hardened Pod should use:

```yaml
spec:
  serviceAccountName: Profgate

  containers:
    - name: gateway
      securityContext:
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]

      resources:
        requests:
          ephemeral-storage: 256Mi
        limits:
          ephemeral-storage: 2Gi

      volumeMounts:
        - name: profgate-data
          mountPath: /data/profgate

  volumes:
    - name: profgate-data
      emptyDir:
        sizeLimit: 2Gi
```

This provides simple deployment and horizontal availability while retaining a narrow runtime privilege set.

If PGO workload later becomes heavy, the same binary can support role separation:

```text
profgate serve

profgate collector
```

allowing independent API and worker Deployments without redesigning the protocol or state model.

---

## 37. Recommended v1 Architecture

The recommended first implementation is:

```text
Kubernetes
  read-only Service / Pod / EndpointSlice discovery

NATS KV
  PROFGATE_CONFIG
  PROFGATE_JOBS

Gateway replicas
  HTTP API
  scheduler
  collector
  local emptyDir artifacts

Optional
  NATS Object Store for merged.pprof + manifest.json
```

Specifically:

1. **No CRDs.**
2. **No Kubernetes write permissions.**
3. **No sticky sessions.**
4. **No dedicated scheduler leader.**
5. **No external database.**
6. **No external object storage by default.**
7. **NATS KV is the durable control-plane state.**
8. **Collection artifacts are ephemeral by default.**
9. **Short Collections make restart-from-zero recovery practical.**
10. **Long-term temporal sampling is handled by runtime scheduling.**
11. **PGO configuration is hot-reloaded from NATS KV.**
12. **Only the final merged profile is optionally persisted to NATS Object Store.**
13. **The Kubernetes ServiceAccount has read-only discovery permissions.**
14. **The gateway has no access to Secrets, Pod exec, logs, port-forward, or workload mutation APIs.**
15. **NATS permissions are restricted to predefined `PROFGATE_*` stores; JetStream administration remains external.**
16. **Runtime PGO configuration writes may be isolated behind a separate administrative identity.**
17. **The container runs non-root, without Linux capabilities, with a read-only root filesystem.**
18. **Kubernetes 1.23+ is the supported baseline; only stable 1.23-era EndpointSlice fields are required.**
19. **Auth defaults to a lightweight static model: disabled, Basic, or OIDC.**
20. **Authorization realms are static config and do not require another policy service.**
21. **Continuous-profiling stacks are explicitly optional alternatives, not dependencies.**

---

## 38. Open Design Questions

The following details should be resolved before implementation.

### Scheduling

- Exact semantics of `every` and `jitter`.
- Whether cron expressions are needed in addition to fixed intervals.
- How missed schedules are handled after a long outage.

### Replica sampling

- `all` versus fixed-count sampling.
- Behavior when replica count exceeds `maxParallel`.
- Whether node/zone-aware spreading is useful.

### Version identity

- Default version label.
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

- Exact static realm wildcard/pattern syntax.
- Whether OIDC group matches union permissions or select one highest-priority realm.
- Whether namespace-scoped RoleBindings or cluster-wide read discovery is appropriate for the target environment.
- Whether the internal artifact endpoint requires mTLS in addition to NetworkPolicy.
- Whether some deployments disable HTTP mutation of `PROFGATE_CONFIG` entirely.
- Exact NATS subject/account ACLs required to constrain the gateway to predefined `PROFGATE_*` stores.

### Kubernetes compatibility

- Exact `client-go` version policy after validating against real 1.23 and 1.24 API servers.
- Whether optional `core/v1 Endpoints` fallback is needed for any vendor-specific legacy clusters.
- IPv4/IPv6 target preference when a gateway can reach both address families.
- Whether a per-Service pprof port annotation should be supported in v1 or deferred.

---

## 39. Compatibility and Design References

The compatibility baseline and design constraints should be verified against upstream documentation during implementation:

- Kubernetes 1.23 API reference: `discovery.k8s.io/v1 EndpointSlice`
  - https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.23/
- Current EndpointSlice API documentation
  - https://kubernetes.io/docs/reference/kubernetes-api/discovery/endpoint-slice-v1/
- Kubernetes ServiceAccount guidance and projected tokens
  - https://kubernetes.io/docs/concepts/security/service-accounts/
- Kubernetes `client-go` compatibility guidance
  - https://github.com/kubernetes/client-go
- Go `net/http/pprof`
  - https://pkg.go.dev/net/http/pprof
- Go PGO documentation
  - https://go.dev/doc/pgo
- Google pprof profile package
  - https://pkg.go.dev/github.com/google/pprof/profile

Continuous-profiling systems such as Grafana Pyroscope and Parca are useful architectural references and possible future integration targets, but are intentionally not dependencies of this standalone design.

---

## 40. Conclusion

The proposed architecture keeps Profgate operationally lightweight while still providing reliable asynchronous PGO collection in a horizontally scaled production environment.

The critical separation is:

```text
NATS KV
    =
durable control-plane state

Gateway memory
    =
cache and active execution state

emptyDir
    =
short-lived profiling data

NATS Object Store
    =
optional durability for final artifacts only
```

This model avoids the operational and security overhead of CRDs, a full continuous-profiling stack, and Kubernetes write permissions while preserving:

- stateless gateway deployment;
- runtime-updatable PGO policy;
- distributed scheduling;
- worker failover;
- representative replica sampling;
- secure centralized pprof access.

Most importantly, the design intentionally avoids turning the gateway into either a general distributed data platform or a miniature Pyroscope/Parca implementation. NATS is used only where durable coordination is necessary, while large profiling data remains local and disposable unless explicit durability is required.

From a production security-review perspective, the intended permission boundary is concise:

> **Profgate requires no Kubernetes write permissions. It only observes Services, Pods, and EndpointSlices in authorized namespaces, connects to explicitly permitted application pprof ports, and manipulates only dedicated NATS profiling state.**

This boundary should be treated as an architectural invariant. Features that require broader Kubernetes or NATS privileges should be justified explicitly rather than silently expanding the gateway's authority.
