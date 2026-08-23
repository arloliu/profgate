# Profgate Gateway

**Status:** Accepted

This document is the design of record for the gateway:
a Kubernetes-aware pprof proxy with no PGO, no NATS, and no durable state.
Profile-Guided Optimization collection is a separate, additive design in
[`pgo.md`](pgo.md); it builds on what is defined here and changes none of it.
[`profgate-design.md`](profgate-design.md) is the superseded original draft
that covered both.

---

## 1. Overview

Production Kubernetes clusters run many Go services.
Exposing `/debug/pprof` through an Ingress for each one creates a large attack surface
and makes access control impossible to standardize.

Profgate is one HTTP entry point for profiling Go workloads.
It resolves a Kubernetes Service to its current backend Pods,
selects one,
and proxies a standard Go pprof profile from that Pod's pprof port.

The central principle:

> A Kubernetes Service is a logical workload identity,
> not the network destination profiles are fetched from.

The gateway never sends profiling traffic to the Service ClusterIP.
It resolves the Service to concrete Pods and connects to a Pod directly,
which is what makes the profiled replica predictable.

### 1.1 Core decisions

1. **No Kubernetes write permissions.**
2. **No Kubernetes CRDs, no operator.**
3. **No NATS.**
   The gateway binary links no NATS client library.
   `go.mod` not containing `github.com/nats-io/nats.go` is the checkable form of this claim.
4. **Stateless.**
   Any replica answers any request; there is no coordination between replicas.
5. **Read-only discovery through cluster-wide shared informers** over Services, Pods, and EndpointSlices.
6. **Kubernetes 1.23 is the compatibility baseline**; only stable API fields available at that release are used.
7. **Authentication is optional and static.**
   This design defines the `disabled` mode and the authorization structure every mode shares.
8. **Authorization is static access realms** loaded from process configuration.
9. **The response never reveals the Pod IP or pprof port.**
   Hiding the direct path to the pprof endpoint is part of what the gateway is for.
10. **Every dependency is auditable in one sitting.**
    The dependency set is listed in this document; adding to it is a design change.

### 1.2 Non-goals

- Continuous profiling, long-term profile storage, flamegraph UI.
  Grafana Pyroscope and Parca exist for that and are not dependencies of this design.
- Profiling languages other than Go.
- Reaching Pods through `pods/exec`, `pods/portforward`, or a sidecar.
- Hot-reloading configuration, Basic Auth, OIDC, PGO collection.
  Each is designed for in [`pgo.md`](pgo.md) or a later revision of this document;
  the seams that make them additive are called out where they occur.

---

## 2. Architecture

```text
              Developers / CI
                     |
                HTTP / HTTPS        (TLS terminates at the Ingress)
                     v
            +-----------------+
            | Ingress / LB    |
            +--------+--------+
                     |
        +------------+------------+
        |                         |
        v                         v
+---------------+         +---------------+
| Profgate A    |         | Profgate B    |
| HTTP API      |         | HTTP API      |
| auth / realm  |         | auth / realm  |
| informers     |         | informers     |
| proxy         |         | proxy         |
+-------+-------+         +-------+-------+
        |                         |
        |  Kubernetes API (read)  |
        +------------+------------+
                     |
  Service -> EndpointSlice -> Pod -> PodIP:pprofPort
```

Each replica holds its own informer cache and serves requests from it.
There is no shared state, so replica count is purely a capacity and availability choice.

---

## 3. Permission Boundary

> Profgate requires no Kubernetes write permissions.
> It observes Services, Pods, and EndpointSlices in authorized namespaces,
> connects to explicitly permitted application pprof ports,
> and manipulates only its dedicated `PROFGATE_*` NATS stores.

The gateway defined here uses no NATS stores at all;
the clause exists so the wording stays stable when [`pgo.md`](pgo.md) adds them.
[`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md)
holds the authoritative wording and the mechanisms that keep it checkable.

### 3.1 Kubernetes RBAC

| API group | Resource | Verbs |
|---|---|---|
| core (`""`) | `services` | `get`, `list`, `watch` |
| core (`""`) | `pods` | `get`, `list`, `watch` |
| `discovery.k8s.io` | `endpointslices` | `get`, `list`, `watch` |

The shipped manifest is a `ClusterRole` bound by a `ClusterRoleBinding`.
Discovery is cluster-wide because the informers are cluster-wide (section 5);
which namespaces a caller may reach is decided by realms (section 7), not by RBAC.
Namespace-scoped `RoleBinding` deployments are not supported by this design
and would require namespace-scoped informers.

A golden test pins the manifest:
the set of `(apiGroups, resources, verbs)` tuples must equal exactly the table above.
One tuple more or fewer fails the test.

**Startup access check.**
Before serving, the gateway issues a `SelfSubjectAccessReview` for each of the nine
(resource, verb) pairs in the table.
Any denial is fatal: the process logs which pair was denied and exits non-zero.
`SelfSubjectAccessReview` creation is granted to every authenticated principal by the built-in `system:basic-user` ClusterRole,
so the check needs no rule of its own.
This turns an under-privileged deployment into a crash at startup instead of a `403` discovered on the first request,
and gives the invariant a negative test: a ClusterRole missing `watch` must fail to start.

### 3.2 Explicitly absent

No `pods/exec`, `pods/log`, `pods/portforward`, `secrets`, `configmaps`, `nodes`,
no `apps/*` workload resource, no mutating verb.
The target model stops at the Pod; which controller owns it is irrelevant to profiling.
`spec.nodeName` on the Pod already records the node.

### 3.3 Network

Required flows: Ingress → gateway listen port; gateway → Kubernetes API; gateway → `PodIP:pprofPort`.

Application pprof ports must not be routed by application Ingress resources.
Where the cluster enforces NetworkPolicy, the pprof port should admit only the gateway's namespace and Pod selector;
`deploy/` ships an example policy.

### 3.4 Container

```yaml
securityContext:
  runAsNonRoot: true
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
```

The gateway writes nothing to disk; no volume is required.
No host namespaces, host paths, `SYS_PTRACE`, or privileged mode.

### 3.5 What a compromised gateway can do

It can read Service, Pod, and EndpointSlice metadata cluster-wide,
and open HTTP connections to any Pod IP on the configured pprof port that NetworkPolicy admits.
It cannot exec into Pods, read Secrets or logs, port-forward, mutate any Kubernetes object,
or reach the host.
Profiling output is sensitive production data
(stack traces, package names, request strings) and is treated as such in the audit log.

---

## 4. Kubernetes Compatibility

### 4.1 Baseline

Kubernetes 1.23 is the minimum supported version; 1.23 and 1.24 are production targets.
The gateway uses only `discovery.k8s.io/v1 EndpointSlice` fields stable at 1.23:

```text
metadata.labels["kubernetes.io/service-name"]
addressType
endpoints[].addresses
endpoints[].conditions.ready
endpoints[].targetRef
endpoints[].nodeName
```

Newer fields (`trafficDistribution`, topology hints, `conditions.serving`/`terminating`)
are not read.
There is no `core/v1 Endpoints` fallback; clusters with EndpointSlice disabled are unsupported.

### 4.2 client-go policy

The newest `client-go` that passes the end-to-end matrix (section 9) is used.
client-go's version skew policy covers API servers several minors older,
so a current client against a 1.23 API server is the normal case, not an exception.
Exactly one package imports `client-go`: `internal/k8s` (section 5).

### 4.3 ServiceAccount tokens

The gateway reads the in-cluster projected token through the standard client configuration.
It does not assume a Secret-backed token exists
(Kubernetes 1.24 stopped auto-generating them) and needs no Secret API access.

---

## 5. Target Resolution

### 5.1 The seam

`internal/k8s` is the only importer of `k8s.io/client-go`.
Its exported interface is the complete set of things Profgate can do to Kubernetes:

```go
// Target is one eligible backend of a Service.
type Target struct {
    Namespace string
    Service   string
    Pod       string
    Node      string
    PodIP     string // never serialized to a client
    Port      int32  // resolved pprof port; never serialized to a client
    Version   string // value of the configured version label; "" when absent
}

type Discovery interface {
    // Targets returns the currently eligible backends of a Service.
    // Order is unspecified.
    Targets(ctx context.Context, namespace, service string) ([]Target, error)
    // HasSynced reports whether every informer has completed its initial list.
    HasSynced() bool
}
```

Sentinel errors, matched with `errors.Is`:

| Error | Meaning | HTTP mapping |
|---|---|---|
| `ErrServiceNotFound` | no Service with that name in the namespace | `404 service_not_found` |
| `ErrServiceSelectorless` | Service has no selector; its endpoints are not Pods | `422 service_selectorless` |

There is no `GetPod` method.
A `?pod=` request is validated by searching the `Targets` result,
so naming a Pod that is not a backend of the Service is rejected without an additional API call,
and the interface stays one method narrower.

`Target` carries no `Labels` map.
When [`pgo.md`](pgo.md) needs build identity, the struct grows a named field;
Kubernetes object shapes do not leak into the core data model.

### 5.2 Informers

One cluster-wide shared informer each for Services, Pods, and EndpointSlices,
with a 10-minute resync period.
`Targets` reads only from informer caches and never calls the API server.

Until every informer has synced, `HasSynced` is false,
the HTTP API answers `503 not_ready` to every `/v1` request,
and `/readyz` fails (section 8.3).

### 5.3 Eligibility

A Pod is a target of a Service when all of the following hold:

1. An EndpointSlice labeled `kubernetes.io/service-name=<service>` in the namespace lists the Pod in `endpoints[].targetRef` with `kind: Pod`.
2. That endpoint's `conditions.ready` is not `false` (an unset value counts as ready,
   matching the EndpointSlice API contract).
3. The Pod object exists in the cache, `status.phase` is `Running`,
   and its `Ready` condition is `True`.
   This cross-check excludes Pods that appear only because the Service sets `publishNotReadyAddresses`.
4. `metadata.deletionTimestamp` is unset.
5. A pprof port resolves for the Pod (section 5.4).

Endpoints are aggregated across every EndpointSlice of the Service and deduplicated by Pod UID.
Each EndpointSlice carries one `addressType`;
when a Service has slices of both `IPv4` and `IPv6`, only the `IPv4` slices are used.
The first entry of `endpoints[].addresses` is the Pod IP.

### 5.4 Port resolution

`discovery.pprof` names the port in one of two ways (exactly one must be set):

- `port: 6060` — the same numeric port for every Pod.
- `portName: pprof` — the named `containerPort` found in the Pod's `spec.containers[].ports`.
  A Pod with no port of that name is ineligible; there is no fallback to a number.

The application Service does not need to expose the pprof port.
A per-Service annotation override is deliberately not part of this design.

### 5.5 Version

`discovery.versionLabel` names a Pod label; the default is `app.kubernetes.io/version`.
`Target.Version` is that label's value, or empty when the Pod lacks it.
`?version=` filtering excludes Pods with an empty version.

---

## 6. HTTP API

All paths are under `/v1`.
The product name does not appear in any path.

### 6.1 List targets

```http
GET /v1/namespaces/{namespace}/services/{service}/targets
```

```json
{
  "namespace": "payment",
  "service": "payment-api",
  "targets": [
    {"pod": "payment-api-7c8f8c9b9-xabcd", "node": "worker-07", "version": "1.42.3"}
  ]
}
```

A Service with no eligible backends returns `200` with an empty `targets` array.
`ip` and `port` are never included.

### 6.2 Fetch a profile

```http
GET /v1/namespaces/{namespace}/services/{service}/profiles/{profile}
```

| `{profile}` | Upstream path |
|---|---|
| `cpu` | `/debug/pprof/profile` |
| `heap` | `/debug/pprof/heap` |
| `allocs` | `/debug/pprof/allocs` |
| `goroutine` | `/debug/pprof/goroutine` |
| `mutex` | `/debug/pprof/mutex` |
| `block` | `/debug/pprof/block` |
| `threadcreate` | `/debug/pprof/threadcreate` |
| `trace` | `/debug/pprof/trace` |

Query parameters:

| Parameter | Meaning | Default |
|---|---|---|
| `seconds` | forwarded to `cpu` and `trace`; rejected for other profiles | upstream default (30 for cpu, 1 for trace) |
| `pod` | select this Pod; must be an eligible target | — |
| `version` | restrict selection to targets with this version | — |
| `strategy` | `random` is the only value | `random` |

`seconds` above `limits.cpuSeconds` or `limits.traceSeconds` returns `400 seconds_exceeds_limit`;
the value is never silently clamped.
`pod` and `version` compose: the Pod must also carry the version.
`pod` with `strategy` is accepted; `strategy` is ignored.

Response headers on success:

```http
X-Pprof-Target-Pod: payment-api-7c8f8c9b9-xabcd
X-Pprof-Target-Node: worker-07
X-Pprof-Target-Version: 1.42.3
```

`X-Pprof-Target-Version` is present and empty when the Pod has no version label,
so a client can distinguish "no label" from "gateway predates the header".

### 6.3 Proxy behavior

The gateway opens a plain HTTP connection to `PodIP:Port`, issues the upstream request,
and streams the response body and status code back unchanged.
Upstream `Content-Type` is forwarded.
The upstream request carries no headers from the client;
the client's `Accept-Encoding` is not forwarded and the body is not re-encoded.

| Condition | Response |
|---|---|
| connection refused / reset / timeout | `502 upstream_unreachable` |
| upstream returns non-2xx | that status and body, unchanged |

Timeouts: dial 5s; response header wait `seconds + 10s` for `cpu` and `trace`, 30s otherwise.

### 6.4 Errors

Every error body has the same shape:

```json
{"error": "service payment-api not found in namespace payment", "code": "service_not_found"}
```

`code` is the stable contract; `error` is human-readable and may change.
The complete set of codes:

| Status | `code` |
|---|---|
| 400 | `invalid_parameter`, `seconds_exceeds_limit`, `strategy_unsupported` |
| 403 | `realm_denied` |
| 404 | `service_not_found`, `pod_not_found`, `profile_unknown` |
| 422 | `service_selectorless` |
| 503 | `not_ready`, `no_targets` |
| 502 | `upstream_unreachable` |

`pod_not_found` covers both a Pod that does not exist and one that is not an eligible backend.
`no_targets` is returned by the profile endpoint when selection finds nothing;
the targets endpoint returns an empty list instead.

Realm denial is evaluated before target resolution,
so a caller denied a namespace cannot learn whether a Service exists in it.

---

## 7. Authentication and Authorization

Both are static process configuration (section 10), never runtime state.

### 7.1 Principals and realms

Every request is attributed to a principal; the principal maps to exactly one realm;
the realm decides what the request may do.
This design defines one authentication mode:

```yaml
auth:
  mode: disabled
  anonymousRealm: developer
```

`disabled` attributes every request to the principal `anonymous`
and maps it to `auth.anonymousRealm`.
At startup the gateway logs, at warning level:

```text
authentication disabled; access is controlled only by network boundary and static realm policy
```

Later modes (Basic, OIDC) add a principal → realm mapping step and change nothing below it.

### 7.2 Realm structure

```yaml
realms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
```

Each list matches a request value if the list contains `"*"` or the exact string.
There is no glob, prefix, or regular-expression matching.

Evaluation order: namespace, service, profile.
The first failing check returns `403 realm_denied`.
The targets endpoint checks namespace and service only.

### 7.3 Wide-open is explicit

A configuration with no `realms` entry, or whose `auth.anonymousRealm` names a realm that does not exist,
fails validation and the process exits.
Access to every namespace is expressed by writing `"*"`;
it is never the consequence of leaving something out.
The example configuration under `deploy/` writes `"*"`, so the default experience is unrestricted
and the wildcard is visible in version control.

### 7.4 Limits are not authorization

```yaml
limits:
  cpuSeconds: 60
  traceSeconds: 60
```

Limits cap what any realm may request.
No principal, however privileged, exceeds them.

---

## 8. Operations

### 8.1 CLI

```text
profgate serve --config <path>
profgate config validate --config <path>
profgate version
```

Standard-library `flag` with hand-written subcommand dispatch.

### 8.2 Logging

`log/slog`, JSON to stdout.
Every proxied request emits one record:

```text
principal, namespace, service, pod, profile, seconds, status, duration_ms, code (on error)
```

This is the audit trail.

### 8.3 Health

| Path | Condition for `200` |
|---|---|
| `/healthz` | process is serving HTTP |
| `/readyz` | `Discovery.HasSynced()` is true |

`/readyz` reflects the sync state of the informers as a whole.
Both paths bypass authentication and realm checks.

### 8.4 Metrics

`/metrics` exposes Prometheus text format via `prometheus/client_golang`:

| Metric | Labels |
|---|---|
| `profgate_proxy_requests_total` | `namespace`, `service`, `profile`, `code` |
| `profgate_proxy_duration_seconds` (histogram) | `profile` |
| `profgate_targets` (gauge, sampled on request) | `namespace`, `service` |
| `profgate_informer_synced` (gauge) | — |

Handlers record through an internal `Recorder` interface; the Prometheus implementation is one of its implementations.
`/metrics` bypasses authentication and realm checks.

---

## 9. Testing

### 9.1 Layers

**Unit**, run by `mise run test`, seconds:

- `internal/k8s` against `client-go/kubernetes/fake` with real informers;
  EndpointSlice and Pod fixtures exercise every eligibility rule in section 5.3 and the dedup rule.
- `internal/proxy` against `httptest.Server` stand-ins for pprof endpoints.
- `internal/httpapi` against a fake `Discovery`.
- The golden ClusterRole test (section 3.1) parses `deploy/` and compares rule tuples.
- The client-go import check: every Go file importing `k8s.io/client-go` is under `internal/k8s/`.

**End-to-end**, run by `mise run test:e2e`, minutes.
Plain `go test` under `//go:build e2e` in `test/e2e/`, inside the main module.
No end-to-end framework; the reasoning is recorded in
[`../decisions/e2e-without-framework.md`](../decisions/e2e-without-framework.md).
`controller-runtime/envtest` is not used: it runs no controller-manager and no kubelet,
so it cannot produce real EndpointSlices or reachable Pods.

### 9.2 Cluster matrix

`test/e2e/versions.yaml` is the single source of truth for lanes:

```yaml
- name: "1.23"
  frozen: true
  kind: "0.22.0"
  image: "<mirror>/kindest/node:v1.23.17@sha256:14d0a9a892b943866d7e6be119a06871291c517d279aedb816a4b4bc0ec0a5b3"
- name: "1.24"
  frozen: true
  kind: "0.22.0"
  image: "<mirror>/kindest/node:v1.24.17@sha256:bad10f9b98d54586cba05a7eaa1b61c6b90bfc4ee174fdc43a7b75ca75c95e51"
- name: "current"
  frozen: false
  kind: "0.32.0"
  image: "<mirror>/kindest/node:v1.36.1@sha256:<pinned at first run>"
```

Frozen lanes are never upgraded, only removed.
`current` tracks the kind default image and is bumped in its own commit.
Images are mirrored to a registry under the project's control; the digest is identical after mirroring.
The kind binary is pinned per lane through mise (`mise x kind@<version>`).
Why kind rather than k3s for the old versions:
[`../decisions/kind-frozen-lanes.md`](../decisions/kind-frozen-lanes.md).

### 9.3 Harness

- `PROFGATE_E2E_LANE` selects a lane; unset means `current`.
- One kind cluster per lane per run, shared by the whole suite;
  each test works in its own namespace derived from the test name and deletes it on exit.
- `PROFGATE_E2E_KEEP=1` leaves the cluster running; a cluster whose name and image match is reused.
- Images (gateway and test application) are built with `ko build --local` and loaded with `kind load`.
- Manifests are `deploy/` plus kustomize overlays under `test/e2e/`:
  image substitution, `replicas: 2`, and a ClusterRole variant missing `watch`.
- The reduced-ClusterRole scenario runs a second gateway Deployment with its own ServiceAccount
  and ClusterRoleBinding so it cannot disturb the main gateway.
- Replica-consistency checks reach each gateway Pod through client-go `portforward`,
  because Pod IPs are unreachable from outside kind.
- The test application lives in `test/e2e/testapp/`:
  `net/http/pprof` plus a `/healthz` that can be flipped to failing over HTTP
  so a Pod can be made `NotReady` on demand.

### 9.4 What end-to-end proves

1. A Service backed by several EndpointSlices yields a deduplicated target list.
2. NotReady Pods, terminating Pods, and Pods of a `publishNotReadyAddresses` Service are never targets.
3. After a Pod is deleted or added, `targets` converges within 10 seconds.
4. Every one of the eight profiles fetched through the gateway parses with `github.com/google/pprof/profile`
   (trace is checked for the `go 1.` header bytes instead), and `cpu?seconds=2` takes at least 2 seconds.
5. A namespace outside the realm returns `403`; an unknown Service `404`;
   a selectorless Service `422`; `?pod=` naming a Pod of another Service `404`.
6. `?version=` filters correctly and excludes Pods without the label.
7. The shipped ClusterRole lets the gateway start;
   the variant missing `watch` makes it exit non-zero with the denied pair in its log.
8. Two replicas answer identically (targets list equal, headers consistent).
9. All of the above pass on every lane.

### 9.5 Continuous integration

GitHub Actions in this repository:

- every push: `mise run check`, lint, unit tests;
- pull requests: the `current` lane;
- pushes to `main`: all three lanes.

There is no scheduled run.
Mirror-registry pull credentials live in repository secrets.
Production deployment runs on a private GitLab and is configured separately.

---

## 10. Configuration

Loaded once at startup with `github.com/arloliu/fuda`:
file via `--config`, `default` tags, environment overrides with prefix `PROFGATE_`,
`validate` tags, and `ref:"file://..."` for values that live in mounted files.
The process holds the result in an `atomic.Pointer[Config]`
and every request reads the current pointer,
so a later hot-reload (`fuda/watcher`) is one goroutine and no change to request handling.

```yaml
server:
  listen: ":8080"             # restart
discovery:
  versionLabel: app.kubernetes.io/version   # hot
  pprof:
    port: 6060                # hot
    # portName: pprof         # hot; exactly one of port / portName
limits:
  cpuSeconds: 60              # hot
  traceSeconds: 60            # hot
auth:
  mode: disabled              # restart
  anonymousRealm: developer   # hot
realms:                       # hot
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
```

`hot` marks a field a future reload may change in place;
`restart` marks a field whose change requires a process restart.
The classification is fixed now so a reload cannot later be applied to `server.listen` by accident.

Validation failures are fatal at startup and reported by `profgate config validate`.

---

## 11. Build and Deployment

- Module `github.com/arloliu/profgate`; binary `profgate`; entrypoint `cmd/profgate`.
- Images are built with `ko`: no Dockerfile, distroless base, non-root.
- `deploy/` holds plain YAML with a kustomize base:
  ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment (`replicas: 2`, hardened as in section 3.4),
  Service, example NetworkPolicy, example configuration.
- No Helm chart; there is nothing to template.

### 11.1 Dependencies

| Module | Purpose |
|---|---|
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | discovery (only in `internal/k8s`) |
| `github.com/arloliu/fuda` | configuration |
| `github.com/prometheus/client_golang` | metrics |
| `github.com/google/pprof` | tests only: parsing fetched profiles |
| `sigs.k8s.io/yaml` | tests only: golden ClusterRole and `versions.yaml` |

Everything else is the standard library.

---

## 12. Package Layout

```text
cmd/profgate/        CLI: serve, config validate, version
internal/k8s/        the seam; sole importer of client-go
internal/proxy/      upstream HTTP to PodIP:Port, timeouts, error mapping
internal/httpapi/    routing, realm checks, handlers, error bodies, audit log
internal/config/     fuda-loaded Config, validation, hot/restart classification
internal/metrics/    Recorder interface and the Prometheus implementation
deploy/              kustomize base
test/e2e/            harness, versions.yaml, testapp, overlays
```

---

## 13. Failure Scenarios

| Event | Behavior |
|---|---|
| Kubernetes API unreachable at startup | informers never sync; `/readyz` fails; `/v1` returns `503 not_ready` |
| Kubernetes API unreachable while running | informers serve their last cache; resolution continues on stale data until reconnect |
| Gateway replica crashes mid-profile | the client's connection drops; no state to recover |
| Target Pod dies mid-profile | upstream connection resets; `502` if headers were not yet sent, otherwise a truncated body |
| Configuration invalid | process exits at startup with the validation error |
| RBAC too narrow | process exits at startup naming the denied (resource, verb) |
