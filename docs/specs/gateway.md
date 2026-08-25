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
   The gateway's ServiceAccount can read three resources —
   `list` and `watch` on each, plus `get` on Pods — and nothing else.
2. **No Kubernetes CRDs, no operator.**
3. **No NATS on the interactive path.**
   Discovery, authorization, and proxying never touch NATS;
   [`pgo.md`](pgo.md) is the only design that uses it,
   and `internal/natskv` is its only importer.
4. **Stateless.**
   Any replica answers any request; there is no coordination between replicas.
5. **Read-only discovery through cluster-wide shared informers** over Services, Pods, and EndpointSlices.
6. **Kubernetes 1.23 is the compatibility baseline**;
   only API fields present in the 1.23 `discovery.k8s.io/v1` schema are read.
7. **Authentication is optional and static.**
   This design defines the `disabled` mode and the authorization structure every mode shares.
8. **Authorization is static access realms** loaded from process configuration.
9. **Nothing the gateway itself emits reveals a Pod IP, a pprof port, or a name the client's realm denies.**
   Hiding the direct path to the pprof endpoint is part of what the gateway is for.
   Bytes the application sends — a profile body, an upstream error body, an allowlisted upstream header —
   are application-controlled and pass through as they are.
10. **Every dependency is auditable in one sitting.**
    The dependency set is listed in this document; adding to it is a design change.

### 1.2 Non-goals

- Continuous profiling, long-term profile storage, flamegraph UI.
  Grafana Pyroscope and Parca exist for that and are not dependencies of this design.
- Profiling languages other than Go.
- Reaching Pods through `pods/exec`, `pods/portforward`, or a sidecar.
- Hot-reloading configuration, Basic Auth, OIDC.
  Each is designed for in a later revision of this document;
  PGO collection is designed in [`pgo.md`](pgo.md).
  The seams that make them additive are called out where they occur.

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
| :8080 API     |         | :8080 API     |
| :9090 ops     |         | :9090 ops     |
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

Two listeners:
the **API listener** (`server.listen`, default `:8080`) serves `/v1` and is what the Ingress routes to;
the **ops listener** (`server.opsListen`, default `:9090`) serves `/healthz`, `/readyz`, and `/metrics`,
is reached only by the kubelet and the metrics scraper, and is never routed by the Ingress.

---

## 3. Permission Boundary

> Profgate requires no Kubernetes write permissions.
> It observes Services, Pods, and EndpointSlices in authorized namespaces,
> connects to explicitly permitted application pprof ports,
> and manipulates only its dedicated `PROFGATE_*` NATS stores.

The gateway defined here uses no NATS stores;
[`pgo.md`](pgo.md) names the three it adds.
[`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md)
holds the authoritative wording and the mechanisms that keep it checkable.

### 3.1 Kubernetes RBAC

| API group | Resource | Verbs |
|---|---|---|
| core (`""`) | `services` | `list`, `watch` |
| core (`""`) | `pods` | `get`, `list`, `watch` |
| `discovery.k8s.io` | `endpointslices` | `list`, `watch` |

Seven tuples.
`list` and `watch` feed the shared informers that every selection reads from.
`get` on Pods is used in two places, both reading one named Pod:
the preflight below, and the confirmation read immediately before a proxy connection (section 5.6).
No other `get` exists because nothing else needs one.

**Why `get` on Pods is in the boundary**, in the form
[`800`](../../.agents/rules/800-security-invariant.md) requires of any widening:

1. *Capability needed.*
   A most-recent read of one Pod by name, so the proxy can check the selected Pod's identity and address
   against the API server rather than against a cache that may lag.
2. *What a compromised gateway gains.*
   Nothing beyond what `list` on Pods already grants:
   `list` returns every Pod cluster-wide, and `get` returns one of them by name.
   The attacker's reachable information set is unchanged; only the access pattern differs.
3. *Narrower alternative considered.*
   A `list` restricted by `fieldSelector=metadata.name=<pod>` in the Pod's namespace is also a most-recent read
   and would keep the verb set at `list`/`watch`.
   It was rejected because it expresses a single-object read as a filtered collection read,
   which hides the intent from anyone auditing the API calls and makes the preflight harder to state;
   the capability is identical either way.

The shipped manifest is a `ClusterRole` bound by a `ClusterRoleBinding`.
Discovery is cluster-wide because the informers are cluster-wide (section 5);
which namespaces a caller may reach is decided by realms (section 7), not by RBAC.
Namespace-scoped `RoleBinding` deployments are not supported by this design
and would require namespace-scoped informers.

A golden test pins the manifest:
the set of `(apiGroup, resource, verb)` tuples must equal exactly the seven in the table,
and every rule must consist of `apiGroups`, `resources`, and `verbs` only —
no `nonResourceURLs`, `resourceNames`, or wildcard in any field.
One tuple more or fewer, or any other rule shape, fails the test.

**Startup preflight.**
Before the informers start, the gateway performs, for each of the three resources,
one `list` with `limit=1` and one `watch` that it closes as soon as the server accepts it,
plus one `get` of a Pod named `profgate-preflight` in the gateway's own namespace
(read from the ServiceAccount namespace file), where a `404` counts as success
because authorization is decided before the lookup.
These calls exercise every tuple in the table and nothing else.
A `403 Forbidden` on any of the seven is fatal:
the process logs the `(resource, verb)` pair and exits non-zero.
Every preflight call carries its own 10-second context deadline;
expiry is a transient error like any other.
Any other error is transient and is retried (section 8.5).
This turns an under-privileged deployment into a crash at startup instead of an informer that retries forever,
and gives the invariant a negative test: a ClusterRole missing `watch` must fail to start.

A recording transport in unit tests asserts that startup, steady state, and proxying
address only the seven tuples above.

### 3.2 Explicitly absent

No `get` on Services or EndpointSlices;
no `pods/exec`, `pods/log`, `pods/portforward`, `secrets`, `configmaps`, `nodes`,
no `apps/*` workload resource, no `authorization.k8s.io` review resource, no mutating verb.
The target model stops at the Pod; which controller owns it is irrelevant to profiling.
`spec.nodeName` on the Pod already records the node.

### 3.3 Network

Required flows:
Ingress → API listener;
kubelet and metrics scraper → ops listener;
gateway → Kubernetes API;
gateway → `PodIP:pprofPort`.

Application pprof ports must not be routed by application Ingress resources.
Where the cluster enforces NetworkPolicy, the pprof port should admit only the gateway's namespace and Pod selector;
`deploy/` ships an example policy.
The ops listener is not exposed by the gateway's own Ingress or Service of type other than `ClusterIP`.

### 3.4 Container

```yaml
securityContext:
  runAsNonRoot: true
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
```

The gateway has no writable volume, with or without PGO collection.
Its read-only mounts are the configuration ConfigMap (section 10),
the projected ServiceAccount token that Kubernetes injects,
and, when `pgo.enabled` and `nats.credsFile` are configured ([`pgo.md`](pgo.md)),
a Kubernetes Secret volume holding the NATS credentials file at `/etc/profgate/nats/`,
mounted `readOnly: true` with `defaultMode: 0440`;
the Deployment's pod `securityContext` sets `fsGroup: 65532`
so the non-root gateway's group owns the volume and can read the file.
The kubelet mounts the Secret;
the gateway's ServiceAccount needs no Secrets API permission,
and the RBAC table is unchanged.
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
The gateway reads only these `discovery.k8s.io/v1 EndpointSlice` fields:

```text
metadata.labels["kubernetes.io/service-name"]
addressType
endpoints[].addresses
endpoints[].conditions.ready
endpoints[].targetRef
```

The 1.23 schema also carries `endpoints[].conditions.serving`, `endpoints[].conditions.terminating`,
`endpoints[].hints`, and `endpoints[].nodeName`; they are present but unused.
Node identity comes from the Pod (section 5.3).
There is no `core/v1 Endpoints` fallback; clusters with EndpointSlice disabled are unsupported.

### 4.2 client-go policy

`k8s.io/client-go`, `k8s.io/api`, and `k8s.io/apimachinery` are pinned to one exact minor in `go.mod`,
and a repository check fails if the three drift apart.
client-go's published compatibility guarantee runs from an older client to a newer cluster;
support for 1.23 and 1.24 with a current client is established by the end-to-end matrix (section 9), not by policy.
A bump of the Kubernetes module minor is a matrix-tested change: it runs every non-degraded lane.
Exactly one non-test package imports `client-go`: `internal/k8s` (section 5).

### 4.3 ServiceAccount tokens

The gateway reads the in-cluster projected token through the standard client configuration.
It does not assume a Secret-backed token exists
(Kubernetes 1.24 stopped auto-generating them) and needs no Secret API access.

---

## 5. Target Resolution

### 5.1 The seam

`internal/k8s` is the only non-test package that imports `k8s.io/client-go`.
Its exported interface is the complete set of things the gateway can do to Kubernetes:

```go
// Target is one eligible backend of a Service.
type Target struct {
    Namespace string
    Service   string
    Pod       string
    Node      string // pod.spec.nodeName
    PodIP     string // never serialized to a client
    Port      int32  // resolved pprof port; never serialized to a client
    Version   string // value of the configured version label; "" when absent
    UID       string // pod.metadata.uid at selection; used by Confirm, never serialized
}

type Discovery interface {
    // Targets returns the currently eligible backends of a Service.
    // Order is unspecified.
    Targets(ctx context.Context, namespace, service string) ([]Target, error)
    // HasSynced reports whether every informer has completed its initial list.
    HasSynced() bool
    // Confirm re-reads the Pod behind t from the API server and reports
    // whether t is still an accurate description of it (section 5.6).
    Confirm(ctx context.Context, t Target) error
}
```

Sentinel errors, matched with `errors.Is`:

| Error | Meaning | HTTP mapping |
|---|---|---|
| `ErrServiceNotFound` | no Service with that name in the namespace | `404 service_not_found` |
| `ErrServiceSelectorless` | Service has no selector, so backend membership cannot be verified | `422 service_selectorless` |
| `ErrTargetChanged` | `Confirm` found the Pod gone, replaced, not ready, terminating, or at a different address | `503 target_changed` |
| `ErrDiscoveryUnavailable` | `Confirm` could not reach the API server within its timeout | `503 discovery_unavailable` |

There is no `GetPod` method.
A `?pod=` request is validated by searching the `Targets` result,
so naming a Pod that is not a backend of the Service is rejected without an additional API call,
and the interface stays one method narrower.

`Target` carries no `Labels` map.
When [`pgo.md`](pgo.md) needs build identity, the struct grows a named field;
Kubernetes object shapes do not leak into the core data model.

The end-to-end harness under `test/` also imports `client-go`, with the tester's kubeconfig credentials,
to drive the cluster.
That is test tooling, not gateway capability;
the import check in section 9.1 excludes the `test/` tree and every `_test.go` file.

### 5.2 Informers

One cluster-wide shared informer each for Services, Pods, and EndpointSlices,
with a 10-minute resync period.
`Targets` reads only from informer caches and never calls the API server.

Until every informer has synced, `HasSynced` is false,
the API listener answers `503 not_ready` to every `/v1` request,
and `/readyz` fails (section 8.3).

**Stale caches are expected, and the guarantee is stated accordingly.**
An informer cache lags the API server by design:
events queue, watches reconnect, and during an API outage the cache stops moving entirely.
The gateway does not try to measure that lag.
Instead it splits what a profile request relies on into two facts with different sources:

- **Membership** — that the Pod is a backend of the requested Service, carries the requested version, and is ready —
  is decided from the cache (section 5.3).
  It is current to within the informer lag, normally well under a second;
  during an API outage it is as old as the outage.
- **Identity** — that the address the gateway is about to dial belongs to that Pod, by UID, right now —
  is decided by a live read from the API server (section 5.6).

The gateway therefore promises:
*it connects only to an address that the API server, moments before the dial, reported as belonging to the
selected Pod by UID, and that Pod was a backend of the requested Service as of the gateway's cache.*
It does not promise that the Pod is still a backend at the instant of the dial.
Cached membership is as old as the cache:
normally under a second, during an API outage as long as the outage.
Until the cache advances, every request selects from the same cached membership,
so a Pod whose Service selector or labels changed can be profiled repeatedly under its old membership,
not just once.
**Realm admission is defined on that cached association:**
a Service-restricted realm admits a Pod
when the cached eligibility rules (section 5.3) associate the Pod with an allowed Service.
That is the authorization contract, stated in terms the implementation can meet.
After an API outage the informers reconnect and relist on their own; nothing in the gateway needs to notice.

### 5.3 Eligibility

A Pod is a target of a Service when all of the following hold:

1. The Service exists in the cache and has a non-empty `spec.selector`.
2. An EndpointSlice in the same namespace labeled `kubernetes.io/service-name=<service>`
   has an endpoint whose `targetRef` is `kind: Pod` with the Service's namespace.
3. A Pod with that `targetRef.name` exists in the cache and its `metadata.uid` equals `targetRef.uid`.
   A slice that names a Pod by a stale UID (a recreated Pod of the same name) does not qualify.
4. The Pod's labels match `spec.selector`.
   A slice entry for a Pod the Service would not select is ignored.
5. The endpoint's `conditions.ready` is not `false`
   (an unset value counts as ready, matching the EndpointSlice API contract).
6. `status.phase` is `Running`, the Pod's `Ready` condition is `True`, and `metadata.deletionTimestamp` is unset.
   This excludes Pods that appear only because the Service sets `publishNotReadyAddresses`,
   and Pods that are terminating.
7. The endpoint's first address is one of the Pod's `status.podIPs`.
   A slice whose address disagrees with the Pod it claims to represent does not qualify.
8. A pprof port resolves for the Pod (section 5.4).

Endpoints are aggregated across every EndpointSlice of the Service in this order:
each endpoint is validated against rules 2–7 on its own and invalid entries are discarded;
the valid entries are then deduplicated by Pod UID.
Two valid entries for one Pod UID that disagree on address are a conflict:
that Pod is excluded and the conflict is logged.
An invalid entry (for example a wrong address) never causes a conflict; it is simply dropped.

Address family:
when the Service has slices with `addressType: IPv4`, only those are read;
when it has only `IPv6` slices, those are read.
`PodIP` is the endpoint's first address; `Node` is the Pod's `spec.nodeName`.

### 5.4 Port resolution

`discovery.pprof` names the port in one of two ways (exactly one must be set):

- `port: 6060` — the same numeric port for every Pod.
- `portName: pprof` — the named `containerPort` found in the Pod's `spec.containers[].ports`
  whose `protocol` is `TCP` (or unset, which Kubernetes defaults to TCP).
  A Pod with no TCP port of that name is ineligible; there is no fallback to a number.

The application Service does not need to expose the pprof port.
A per-Service annotation override is deliberately not part of this design.

### 5.5 Version

`discovery.versionLabel` names a Pod label; the default is `app.kubernetes.io/version`.
`Target.Version` is that label's value, or empty when the Pod lacks it.
`?version=` filtering excludes Pods with an empty version.

### 5.6 Confirmation before connecting

Between selecting a target and opening the upstream connection,
the profile endpoint calls `Confirm`, which issues one `get` of the Pod by namespace and name
with a 5-second timeout and checks the live object against the `Target`:

1. `metadata.uid` equals `Target.UID` (the Pod was not deleted and recreated under the same name);
2. `metadata.deletionTimestamp` is unset;
3. `status.phase` is `Running` and the `Ready` condition is `True`;
4. `Target.PodIP` is one of `status.podIPs`.

Any mismatch is `ErrTargetChanged` and the request ends with `503 target_changed`;
the client retries and selection runs again against a cache that has usually caught up by then.
A `404` from the `get` is the same outcome.
Confirmation shares the gateway's single Kubernetes client with the informers,
with client-side rate limiting at QPS 20, burst 50.
The admission slot cap bounds confirmations in flight to `limits.maxConcurrentProfiles`,
so profile traffic can never generate more than that many concurrent API reads;
time spent waiting on the rate limiter counts toward the confirmation timeout.
A transport error, timeout, or any other API error is `ErrDiscoveryUnavailable`
and ends with `503 discovery_unavailable`:
when the API server cannot vouch for the target, the gateway does not connect.

`Confirm` uses `Target.UID` captured at selection, never a second cache lookup,
so a replacement Pod that has since entered the cache under the same name cannot satisfy it.

**Residual window.**
Between the read and the dial, milliseconds pass.
Any deletion that completes inside that window, followed by the CNI reusing the address, defeats identity:
a force-deleted Pod (`--grace-period=0`) loses its API object at once,
and a normally deleted Pod whose process exits immediately on `SIGTERM` can release its sandbox address
long before its grace period expires — the grace period is a deadline for stopping containers, not a minimum lifetime.
No read can close that gap: only an identity handshake with the application could,
and Go's pprof handler offers none.
The gateway accepts this as a residual risk and does not claim otherwise.
Forbidding forced deletion of profiled workloads narrows the window but does not remove it.
A unit test with a seam between confirmation and dial documents the gap rather than pretending to close it.

The targets endpoint does not confirm: it is informational, reads only the cache,
and its response contains nothing a client could connect to.

---

## 6. HTTP API

All paths are under `/v1` on the API listener.
The product name does not appear in any path.
Every response carries `Cache-Control: no-store`.

### 6.1 Request algorithm

Every `/v1` request passes through these steps in order;
the first failing step produces the response.
Steps 7–9 differ by endpoint.

1. **Route.** Unknown path → `404 route_unknown`; unknown `{profile}` → `404 profile_unknown`.
   Path segments for `{namespace}` and `{service}` must be DNS-1123 labels, otherwise `404 route_unknown`.
2. **Method.** A method the route does not accept → `405 method_not_allowed` with `Allow` listing those it does;
   the two routes defined here accept `GET` only.
3. **Readiness.** `HasSynced()` false → `503 not_ready`.
4. **Authentication.** Resolve the principal (section 7.1).
5. **Realm.** Namespace, then Service, then (profile endpoint only) profile → `403 realm_denied`.
6. **Parameters.**
   Targets endpoint: any query parameter → `400 invalid_parameter`.
   Profile endpoint: validate every parameter per section 6.3 → `400 invalid_parameter` or `400 seconds_exceeds_limit`.
7. **Discovery.** `Targets()` → `404 service_not_found`, `422 service_selectorless`.
8. **Filter and select.**
   Targets endpoint: respond `200` with the full list, sorted (section 6.2).
   Profile endpoint: apply `version`;
   if `pod` is present and no remaining target has that name → `404 pod_not_found`;
   if `pod` is absent and no target remains → `503 no_targets`;
   otherwise pick `pod`, or one target by `strategy` (`random` when absent).
9. **Admit** (profile endpoint only).
   Acquire one of `limits.maxConcurrentProfiles` slots from the shared admission gate (`internal/admit`) without waiting;
   none free → `429 too_many_profiles`.
   The slot is held through confirmation and proxying and released when the response completes.
   The overall request budget (section 6.4) starts here.
10. **Confirm** (profile endpoint only, section 5.6) → `503 target_changed`, `503 discovery_unavailable`.
11. **Proxy** (profile endpoint only, section 6.4).

Realm denial precedes discovery,
so a caller denied a namespace receives the same `403` whether or not the Service exists.

### 6.2 List targets

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

`targets` is sorted by `pod` name;
the response `Content-Type` is `application/json`, as it is for every gateway error body.
A Service with no eligible backends returns `200` with an empty array.
`ip` and `port` are never included.
`version` is present and empty when the Pod has no version label.

### 6.3 Fetch a profile

```http
GET /v1/namespaces/{namespace}/services/{service}/profiles/{profile}
```

| `{profile}` | Upstream path | Takes `seconds` | Upstream default |
|---|---|---|---|
| `cpu` | `/debug/pprof/profile` | yes | 30 |
| `trace` | `/debug/pprof/trace` | yes | 1 |
| `heap` | `/debug/pprof/heap` | no | — |
| `allocs` | `/debug/pprof/allocs` | no | — |
| `goroutine` | `/debug/pprof/goroutine` | no | — |
| `mutex` | `/debug/pprof/mutex` | no | — |
| `block` | `/debug/pprof/block` | no | — |
| `threadcreate` | `/debug/pprof/threadcreate` | no | — |

Query parameters:

| Parameter | Grammar | Meaning |
|---|---|---|
| `seconds` | decimal integer, 1–86400 | duration for `cpu` and `trace`; rejected for other profiles |
| `pod` | DNS-1123 subdomain (the Kubernetes Pod-name rule) | select this Pod; it must be an eligible target |
| `version` | non-empty string | restrict selection to targets with this version |
| `strategy` | `random` | selection strategy; the only value, and the default when absent |

Rules:

- A parameter given more than once, an empty value, an unknown parameter name,
  or a value outside its grammar → `400 invalid_parameter`.
- `seconds` on a profile that does not take it → `400 invalid_parameter`.
- **Effective duration** for `cpu` and `trace` is the explicit `seconds` or the upstream default.
  The effective duration, explicit or not, must not exceed `limits.cpuSeconds` / `limits.traceSeconds`,
  otherwise `400 seconds_exceeds_limit`.
  The effective duration is always sent upstream explicitly,
  so a lowered limit cannot be bypassed by omitting the parameter.
- `version` is applied before `pod`.
  A `pod` that is eligible but carries a different version → `404 pod_not_found`.
- `pod` with `strategy` is accepted; `strategy` has nothing to choose from and is ignored.

Response headers on success:

```http
X-Pprof-Target-Pod: payment-api-7c8f8c9b9-xabcd
X-Pprof-Target-Node: worker-07
X-Pprof-Target-Version: 1.42.3
```

`X-Pprof-Target-Version` is present and empty when the Pod has no version label,
so a client can distinguish "no label" from "gateway predates the header".
These headers are also set on pass-through upstream errors (section 6.4), never on gateway-generated errors.

### 6.4 Proxy behavior

The gateway connects to `net.JoinHostPort(PodIP, Port)` over plain HTTP
through a dedicated `http.Transport`:

- `Proxy: nil` — environment proxy variables are never consulted.
- `DisableCompression: true` — the upstream body is forwarded byte for byte.
- `CheckRedirect` returns `http.ErrUseLastResponse` —
  a redirect is returned to the client as an upstream status, never followed.
- Dial timeout 5s on the transport's dialer.
- The transport is immutable and shared; `ResponseHeaderTimeout` is left unset.
  Per-request deadlines are enforced with context timers instead:
  a **header deadline** of effective duration + 10s for `cpu` and `trace`, 30s otherwise,
  cancels the upstream request if response headers have not arrived;
  an **overall budget** of effective duration + 30s (30s for profiles without a duration)
  starts when the request enters step 9 of section 6.1 and bounds confirmation, dial, header wait,
  and body streaming together;
  the confirmation read's own 5-second timeout is the lesser of 5s and what remains of the budget.
- The upstream request context is derived from the client request context,
  so a client disconnect cancels the upstream request.

The upstream request carries no headers from the client.
The upstream status code and body are forwarded unchanged; response bodies are streamed, not buffered.
Upstream response headers pass through an allowlist:
`Content-Type`, `Content-Length`, `Content-Encoding`, `Content-Disposition`, and `X-Content-Type-Options`.
Every other upstream header is dropped, including hop-by-hop headers, `Set-Cookie`, `Server`, `Location`,
and any `X-Pprof-*` or `Cache-Control` the upstream sends;
gateway-owned headers (`X-Pprof-Target-*`, `Cache-Control`) are always set by the gateway and overwrite upstream values.
`Content-Encoding` passes through because the body is forwarded as sent;
a gzip body therefore reaches the client with the header that lets it decode.

Outcomes are classified by cause, not by which timer fired first:

| Condition | Response | Audit / metrics `code` |
|---|---|---|
| dial refused or reset, connection reset or EOF before headers, any non-deadline transport error before headers | `502 upstream_unreachable` (JSON envelope) | `upstream_unreachable` |
| header deadline or overall budget expired before headers | `504 upstream_timeout` (JSON envelope) | `upstream_timeout` |
| upstream returned `3xx` | `502 upstream_redirect` (JSON envelope); pprof handlers never redirect, and forwarding `Location` could reveal a Pod address | `upstream_redirect` |
| upstream returned `2xx` | forwarded | `ok` |
| upstream returned `4xx` or `5xx` | forwarded unchanged, with target headers | `upstream_<status>` |
| failure or budget expiry after the client response was committed | connection closed; body truncated | `upstream_stream_failed` |
| client disconnected | upstream cancelled | `client_gone` |

The two deadlines are distinct context causes so the classification is deterministic even when they coincide;
a deadline cause always maps to `504`, any other error before headers to `502`.

No transport error text reaches the client;
the JSON `error` string for `upstream_unreachable` and `upstream_timeout` is fixed and names the Pod,
never its address.

### 6.5 Errors

Every gateway-generated error body has the same shape:

```json
{"error": "service payment-api not found in namespace payment", "code": "service_not_found"}
```

`code` is the stable contract; `error` is human-readable and may change.
The complete set of gateway-generated codes:

| Status | `code` |
|---|---|
| 400 | `invalid_parameter`, `seconds_exceeds_limit` |
| 403 | `realm_denied` |
| 404 | `route_unknown`, `service_not_found`, `pod_not_found`, `profile_unknown` |
| 405 | `method_not_allowed` |
| 422 | `service_selectorless` |
| 429 | `too_many_profiles` |
| 502 | `upstream_unreachable`, `upstream_redirect` |
| 503 | `not_ready`, `no_targets`, `target_changed`, `discovery_unavailable` |
| 504 | `upstream_timeout` |

Upstream non-`2xx` responses are not gateway errors:
they pass through with their own status, body, and `Content-Type`,
and are recorded with code `upstream_<status>`.

`pod_not_found` covers a Pod that does not exist,
one that is not an eligible backend,
and one filtered out by `version`.

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
An entry other than `"*"` must be a valid DNS-1123 label (namespaces, services) or a known profile name.

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

Limits cap the effective duration any realm may request (section 6.3).
No principal, however privileged, exceeds them.

### 7.5 Non-disclosure

A client's realm bounds everything the gateway generates on the API listener:
the targets response, gateway error bodies, gateway-owned headers, and the transport-error envelopes
name only namespaces, Services, and Pods the realm admits and never a Pod address.
What an authorized upstream response carries — the profile bytes, a pass-through error body,
an allowlisted upstream header such as `Content-Disposition` — is the application's to control;
the gateway forwards it unchanged and makes no claim about its content.

The ops listener (section 8) carries namespace and Service names in metric labels with no realm check.
Its reachability is a network property, not an application one:
the Ingress never routes it, the gateway's Service does not expose it,
and the shipped gateway NetworkPolicy admits it only from the monitoring namespace.
Where the cluster's CNI does not enforce NetworkPolicy, any Pod that can reach the gateway Pod IP can scrape it;
deployments that need non-disclosure against in-cluster callers must run an enforcing CNI.

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

`log/slog`, JSON to stdout, at the level `server.logLevel` names.
Every `/v1` request emits one record on completion:

```text
principal, namespace, service, pod, profile, seconds, status, code, duration_ms
```

`code` is `ok` for a successful proxy, the gateway error code, or the upstream code from section 6.4.
This is the audit trail.
Records never contain a Pod IP.

### 8.3 Health

Both paths are on the ops listener and have no authentication or realm check.

| Path | `200` when |
|---|---|
| `/healthz` | the process is serving HTTP |
| `/readyz` | preflight has passed and `HasSynced()` is true |

`/readyz` reflects the initial sync of the informers as a whole.
It does not track API reachability afterwards:
a gateway that cannot reach the API server still answers the targets endpoint from its cache
and refuses to proxy (section 5.6), which is the correct behavior, not a reason to be removed from the Service.

### 8.4 Metrics

`/metrics` on the ops listener exposes Prometheus text format via `prometheus/client_golang`:

| Metric | Labels |
|---|---|
| `profgate_requests_total` (counter) | `endpoint` (`targets`/`profile`), `profile`, `code` |
| `profgate_request_duration_seconds` (histogram) | `profile` |
| `profgate_confirm_total` (counter) | `result` (`ok`/`changed`/`unavailable`) |
| `profgate_profiles_in_flight` (gauge) | — |
| `profgate_discovery_synced` (gauge) | — |

`profgate_request_duration_seconds` uses buckets `0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300` seconds,
wide enough for the durations `limits.cpuSeconds` and `limits.traceSeconds` allow (section 6.3)
and the header deadline and overall budget built from them (section 6.4).

Every label has a fixed value set:
`profile` is the eight names or `none`, `code` takes the values in sections 6.4 and 6.5
with upstream statuses bucketed as `upstream_<status>`.
Namespace and Service names never become labels:
they are client-controlled path segments, and under a wildcard realm a caller could mint one series per request.
Those names live in the audit log, where they cost nothing after the line is written.
Handlers record through an internal `Recorder` interface; the Prometheus implementation is one of its implementations.

### 8.5 Startup and shutdown

```text
start
  |
  v
[listening]   both listeners open; /healthz 200; /readyz 503; /v1 -> 503 not_ready
  |
  v
[preflight]   list+watch each resource (section 3.1)
  |   403 on any tuple ------------------------------> log pair, exit 1
  |   other error, including a 10s per-call deadline: retry with backoff 1s..30s,
  |   forever, logging resource, verb, and error on each attempt
  v
[syncing]     informers start; wait for HasSynced
  |   list or watch error: informer's own retry; stays [syncing]
  v
[ready]       /readyz 200; /v1 served
  |   API unreachable: targets served from cache; profile requests end
  |   with 503 discovery_unavailable at Confirm; informers reconnect on their own
  v
SIGTERM
  |
  v
[draining]    /readyz 503; after server.drainDelay the API listener stops accepting;
              in-flight requests finish up to max(limits.cpuSeconds, limits.traceSeconds) + 30s;
              the ops listener answers /readyz, /healthz, and /metrics for the whole drain;
              with pgo.enabled the Collection drain of pgo.md section 12.4 runs beside it,
              and the process exits once both waits have ended
```

`server.drainDelay` is the window between `/readyz` turning 503 and the API listener closing:
without it the listener closes before the EndpointSlice controllers and the kube-proxies have removed this replica,
and every request already in flight towards it is reset.
A `preStop` hook is where a deployment usually buys that window.
The image is distroless and has no shell to run one,
and the `sleep` lifecycle action arrived after the Kubernetes 1.23 baseline,
so the gateway waits in process instead.
It defaults to 5 seconds and accepts anything up to 60;
zero turns it off for a local run.

The Deployment sets `terminationGracePeriodSeconds: 125`,
which covers the drain delay and the default limits (60s) with margin.
An operator who raises either limit must raise the grace period with it:
at least `server.drainDelay` plus the larger limit plus 60s,
which `profgate config validate` prints.
A listener that fails is fatal:
the process logs the failure, drains the interactive requests, and exits 1.
It never waits for the Collection drain of [`pgo.md`](pgo.md) section 12.4,
because a replica with no listener has nothing left to serve,
and a Collection left running stops renewing its lease for another replica to reclaim.

An unreachable API server is never fatal after preflight and does not change `/readyz`:
the targets endpoint keeps serving the cache, confirmation fails closed, and the failure table in section 13 applies.
Because the overall request budget already includes confirmation, the drain bound above covers it.

---

## 9. Testing

### 9.1 Layers

**Unit**, run by `mise run test`, seconds:

- `internal/k8s` against `client-go/kubernetes/fake` with real informers;
  fixtures exercise every eligibility rule in section 5.3 one mutation at a time
  (selector mismatch, stale UID, wrong namespace in `targetRef`, address not in `podIPs`,
  duplicate with conflicting address, `ready: false`, terminating, not `Running`, missing named port),
  plus dual-stack and IPv6-only Services.
  A recording transport asserts the request set during preflight, steady state, and confirmation
  is exactly the seven RBAC tuples.
  Confirmation tests: freeze the informer (stop delivering events), delete or recreate the Pod in the fake API,
  change its IP, mark it not ready, or make the API return an error,
  and prove each case ends in `target_changed` or `discovery_unavailable` before a trap server on the old IP sees a dial;
  remove the confirmation step in a mutation and require the trap test to fail.
  Recreate the Pod with the same name and IP, let the informer observe the replacement, then call `Confirm`
  and require `target_changed`; replace the captured UID with a cache lookup and require that test to fail.
  A seam test between confirmation and dial swaps the address to a trap server and documents that the dial proceeds:
  it pins the residual window described in section 5.6 so a reader cannot mistake it for a closed gap.
  A concurrency test issues more profile requests than `maxConcurrentProfiles` through a fake rate-limited client
  and asserts the API call count never exceeds the cap while an informer relist still completes.
- `internal/proxy` against `httptest.Server` stand-ins:
  redirect to a trap server that must receive nothing, `HTTP_PROXY` set in the environment,
  gzip-encoded upstream body preserved byte for byte with its `Content-Encoding`,
  `Content-Disposition` from a standard pprof handler preserved,
  forged `X-Pprof-Target-*`, `Cache-Control`, `Set-Cookie`, and hop-by-hop headers dropped,
  relative and absolute redirects turned into `502 upstream_redirect` with no Pod address in the body,
  connection refused versus accepted-but-silent upstream yielding `502` versus `504` on every run,
  client cancellation propagated,
  upstream `404`/`429`/`500` pass-through, reset before headers, reset after partial body,
  and concurrent requests with different header deadlines against delayed upstreams proving the deadlines are independent.
- `internal/httpapi` against a fake `Discovery`:
  a table over every route, method, parameter combination, and error code;
  realm denial identical for an existing and a missing Service;
  no response, header, or error string containing a Pod IP.
- `internal/config`: every environment override, unknown keys at each nesting level,
  numeric-only, name-only, both, and neither of `port`/`portName` (neither normalizes to 6060),
  invalid limits, unknown realm reference,
  and a request paused between realm evaluation and discovery while the config pointer is swapped,
  proving the request uses one snapshot.
- The golden ClusterRole test (section 3.1) parses `deploy/` and compares rule tuples.
- A manifest test pins the gateway NetworkPolicy's selectors and ports and the Service's port list;
  the kind lanes cannot prove NetworkPolicy enforcement, only that the manifest is shaped as specified.
- Chart tests render `deploy/chart/profgate` with the `helm` binary mise pins, and assert on the objects:
  the derived memory limit equals `PGOMemoryBytes` applied to the rendered ConfigMap loaded through `internal/config`,
  which also proves the rendered configuration parses;
  `checksum/config` moves with a configuration change and stays put for an unrelated one;
  a null `fsGroup` renders no key;
  the ClusterRole rules match the base's;
  and `helm lint` passes.
  They skip when `helm` is absent rather than failing.
- A configuration test sets every `PROFGATE_*` variable and proves each lands on its field,
  guarding against a doubled prefix in a tag.
- The client-go import check:
  every non-test Go file outside `test/` that imports `k8s.io/client-go` is under `internal/k8s/`.
- The Kubernetes module check: `k8s.io/client-go`, `k8s.io/api`, and `k8s.io/apimachinery` share one minor in `go.mod`.

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
  degraded: false
  networkPolicy: false
  kind: "0.22.0"
  image: "kindest/node:v1.23.17@sha256:14d0a9a892b943866d7e6be119a06871291c517d279aedb816a4b4bc0ec0a5b3"
- name: "1.24"
  frozen: true
  degraded: false
  networkPolicy: false
  kind: "0.22.0"
  image: "kindest/node:v1.24.17@sha256:bad10f9b98d54586cba05a7eaa1b61c6b90bfc4ee174fdc43a7b75ca75c95e51"
- name: "current"
  frozen: false
  degraded: false
  networkPolicy: true
  kind: "0.32.0"
  image: "kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
```

- `image` is a repository path with a digest; the registry prefix comes from `PROFGATE_E2E_REGISTRY`
  (default `docker.io`).
  CI sets it to the project's GHCR mirror, `ghcr.io/arloliu`, where the images are copied digest-for-digest.
- `frozen: true` lanes are never upgraded, only removed.
  `current` tracks the kind default image and is bumped in its own commit.
- The harness has two kinds of Pod access.
  **Gateway control path:** `TestMain` opens one port-forward to each gateway replica and keeps them for the whole run;
  every scenario talks to the gateway through these and they are never accounted per scenario.
  **Test-app reach:** a scenario that opens a port-forward to a test-app Pod (to flip readiness)
  or that needs the gateway to complete a proxy to a test-app Pod declares `needsPodReach`.
  `degraded: true` skips exactly the scenarios that declare it (2, the readiness half of 3, 4, 8, and 9) on that lane;
  the harness logs every skipped scenario by name, the remaining scenarios must still pass,
  and CI reports the lane as passed with a warning annotation.
  A scenario that opens a test-app port-forward without declaring `needsPodReach` fails on every lane.
  Only a commit whose message names the breakage may set `degraded`.
- The kind binary is pinned per lane through mise (`mise x kind@<version>`).
- `networkPolicy` records whether the lane's CNI enforces NetworkPolicy.
  kind gained a built-in enforcer in 0.24.0, so the frozen lanes on 0.22.0 carry `false`.
  Scenarios that need enforcement declare `needsNetworkPolicy` and are skipped, with a logged reason,
  on lanes without it; CI fails if no lane runs them.
- A unit test validates the file: registry-free path, 64-hex digest, known kind version,
  `degraded` only on a `frozen` lane, and at least one lane with `networkPolicy: true`.
  The Go harness and the CI matrix both read this file; neither holds its own lane list.

Why kind rather than k3s for the old versions:
[`../decisions/kind-frozen-lanes.md`](../decisions/kind-frozen-lanes.md).

### 9.3 Harness

- `PROFGATE_E2E_LANE` selects a lane; unset means `current`.
- One kind cluster per lane per run, shared by the whole suite;
  each test works in its own namespace derived from the test name and deletes it on exit.
- `PROFGATE_E2E_KEEP=1` leaves the cluster running; a cluster whose name and image match is reused.
- Images (gateway and test application) are built with `ko build --local` and loaded with `kind load`.
- Manifests are `deploy/` plus kustomize overlays under `test/e2e/`:
  image substitution, `replicas: 2`, and ClusterRole variants missing `watch` and missing `get`.
- The reduced-ClusterRole scenario runs a second gateway Deployment with its own ServiceAccount
  and ClusterRoleBinding so it cannot disturb the main gateway.
- The harness reaches individual gateway Pods and test-app Pods through client-go `portforward`
  with the tester's kubeconfig, because Pod IPs are unreachable from outside kind.
- The test application lives in `test/e2e/testapp/`:
  `net/http/pprof`, a readiness probe on `/healthz` with `periodSeconds: 1` and `failureThreshold: 1`,
  and `POST /healthz/fail` and `POST /healthz/pass` that flip the probe result for that process.
  After either call the harness waits until its own watch sees the Pod's `Ready` condition change
  before it starts the convergence clock.
  Its Deployment sets `terminationGracePeriodSeconds: 60` and a `preStop` sleep of 30s,
  so a deleted Pod stays `Terminating` long enough to observe.
- Overlapping EndpointSlices are created by the harness as manually managed slices
  (`endpointslice.kubernetes.io/managed-by: profgate-e2e`) listing the same Pod as the controller's slice;
  a second manual slice lists the Pod with a wrong address to exercise rule 7 of section 5.3.
- Convergence is measured against an expected set the harness computes before the mutation
  (the current set minus the deleted Pod, or plus the Pod that becomes ready).
  Timing starts when the harness observes the triggering change through its own watch,
  and it polls both gateway replicas every 500ms
  until both return exactly the expected set for three consecutive polls;
  the whole sequence, including the three stable polls, must complete within the 10-second deadline.
  A replica that still returns the pre-mutation set never satisfies the check.

### 9.4 What end-to-end proves

1. A Service backed by several EndpointSlices yields a deduplicated target list;
   a manual slice with a wrong address does not add or alter a target.
2. NotReady Pods, terminating Pods, and Pods of a `publishNotReadyAddresses` Service are never targets
   (`needsPodReach`, for the readiness flip).
3. After a Pod is deleted, both replicas converge on the new target set within 10 seconds;
   the same holds after a Pod becomes ready (`needsPodReach`, because readiness is flipped through the test app).
   The two halves are separate scenarios in the registry so a degraded lane skips only the second.
4. Every one of the eight profiles fetched through the gateway parses with `github.com/google/pprof/profile`
   (trace is checked for the `go 1.` header bytes instead),
   and `cpu?seconds=2` completes in no less than 2 and no more than 5 seconds (`needsPodReach`).
5. A namespace outside the realm returns `403` identically for an existing and a missing Service;
   an unknown Service in an allowed namespace `404`;
   a selectorless Service with a manual Pod-backed slice `422`;
   `?pod=` naming a Pod of another Service `404`.
6. `?version=` filters correctly and excludes Pods without the label.
7. On the 1.24 lane the gateway runs with no Secret-backed ServiceAccount token in the namespace,
   proving the projected token is sufficient.
   The shipped ClusterRole lets the gateway start;
   the variants missing `watch` and missing `get` on Pods each make it exit non-zero with the denied pair in its log.
8. Two replicas return equal sorted target lists,
   and `?pod=` requests to each return identical target headers (`needsPodReach`).
9. With the gateway's egress to the API server blocked by a NetworkPolicy (`needsNetworkPolicy`),
   `/readyz` on both replicas stays `200`, the targets endpoint still answers from cache,
   and a profile request returns `503 discovery_unavailable` without the test app receiving a connection (`needsPodReach`, to read the test app's request counter).
10. Every scenario runs on every lane whose capabilities it does not exclude;
    a lane skips a scenario only by `degraded` or `networkPolicy`, and the skip is logged by scenario name.

### 9.5 Continuous integration

GitHub Actions in this repository:

- every push: `mise run check`, lint, unit tests;
- pull requests: the `current` lane;
- pushes to `main`: all three lanes, except documentation-only changes, which skip the lanes;
- `v*` tags: the same unit gates and the `current` lane, then publication to GHCR.

A tag publishes two artifacts, both gated on those runs passing.
The image goes to `ghcr.io/arloliu/profgate` for `linux/amd64` and `linux/arm64`,
tagged with the tag and with `latest`, and its version is stamped into the binary from the tag.
The chart goes to `oci://ghcr.io/arloliu/charts` with the tag as its `appVersion`
and the tag without its leading `v` as its chart version.
Consuming both from a network without egress to GHCR — through a proxy, as files, or from an internal mirror —
is [`deploy/chart/profgate/README.md`](../../deploy/chart/profgate/README.md).

There is no scheduled run.
The GHCR mirror is public, so no pull credentials are needed;
the workflow logs in with the `GITHUB_TOKEN` GitHub Actions provides automatically,
only in case the packages ever go private.
Production deployment runs on a private GitLab and is configured separately.

---

## 10. Configuration

Loaded once at startup with `github.com/arloliu/fuda`:
file via `--config`, `default` tags, environment overrides through per-field `env` tags with prefix `PROFGATE_`,
`validate` tags, and `ref:"file://..."` for values that live in mounted files.
Before fuda decodes the file, the gateway decodes it once with `yaml.v3` and `KnownFields(true)`
so an unknown key at any nesting level is a validation error.
The process holds the result in an `atomic.Pointer[Config]`;
each request loads the pointer once and uses that snapshot throughout,
so a later hot-reload (`fuda/watcher`) is one goroutine and no change to request handling.

| Key | Env | Default | Reload | Validation |
|---|---|---|---|---|
| `server.listen` | `PROFGATE_LISTEN` | `:8080` | restart | host:port |
| `server.opsListen` | `PROFGATE_OPS_LISTEN` | `:9090` | restart | host:port, distinct from `listen` |
| `server.logLevel` | `PROFGATE_LOG_LEVEL` | `info` | restart | `debug`, `info`, `warn`, `error` |
| `discovery.versionLabel` | `PROFGATE_VERSION_LABEL` | `app.kubernetes.io/version` | restart | valid label key |
| `discovery.pprof.port` | `PROFGATE_PPROF_PORT` | `6060` when `portName` is also absent | restart | 1–65535; exactly one of `port`/`portName` after normalization |
| `discovery.pprof.portName` | `PROFGATE_PPROF_PORT_NAME` | — | restart | IANA service name (the Kubernetes container-port name rule) |
| `limits.cpuSeconds` | `PROFGATE_LIMIT_CPU_SECONDS` | `60` | restart | 1–86400 |
| `limits.traceSeconds` | `PROFGATE_LIMIT_TRACE_SECONDS` | `60` | restart | 1–86400 |
| `limits.maxConcurrentProfiles` | `PROFGATE_LIMIT_MAX_CONCURRENT_PROFILES` | `16` | restart | 1–1024 |
| `auth.mode` | `PROFGATE_AUTH_MODE` | `disabled` | restart | `disabled` |
| `auth.anonymousRealm` | `PROFGATE_ANONYMOUS_REALM` | — | hot | names an entry in `realms` |
| `realms` | — | — | hot | at least one; entries per section 7.2 |

```yaml
server:
  listen: ":8080"
  opsListen: ":9090"
  logLevel: info
discovery:
  versionLabel: app.kubernetes.io/version
  pprof:
    port: 6060
limits:
  cpuSeconds: 60
  traceSeconds: 60
  maxConcurrentProfiles: 16
auth:
  mode: disabled
  anonymousRealm: developer
realms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
```

`hot` marks a field a future reload may change in place;
`restart` marks a field whose change requires a process restart.
Only access policy (`realms`, `auth.anonymousRealm`) is hot:
discovery fields are read inside `Targets()` and would otherwise need a snapshot threaded through the seam,
the duration limits are coupled to the Deployment's grace period,
and the log level is fixed in the handler the process builds at startup.
The classification is fixed now so a reload cannot later be applied to `server.listen` by accident.

**Normalization** runs before validation:
when neither `discovery.pprof.port` nor `discovery.pprof.portName` is set, `port` becomes 6060.
The `default` tag is not used for `port`,
because a file that names the pprof port through `portName` leaves `port` absent,
and fuda fills an absent field from its `default` tag.

Validation failures are fatal at startup and reported by `profgate config validate`.

---

## 11. Build and Deployment

- Module `github.com/arloliu/profgate`; binary `profgate`; entrypoint `cmd/profgate`.
- Images are built with `ko`: no Dockerfile, distroless base, non-root.
- `deploy/` holds plain YAML with a kustomize base:
  ServiceAccount, ClusterRole, ClusterRoleBinding,
  ConfigMap with the example configuration mounted read-only at `/etc/profgate/config.yaml`,
  Deployment (`replicas: 2`, hardened as in section 3.4, `--config /etc/profgate/config.yaml`,
  readiness probe on the ops listener's `/readyz`, `terminationGracePeriodSeconds: 125`),
  a `ClusterIP` Service exposing only the API port,
  a NetworkPolicy for the gateway Pods that admits the API port from the Ingress controller's namespace
  and the ops port from the monitoring namespace (both namespace selectors are kustomize-patched per cluster),
  and an example NetworkPolicy for application pprof ports.
- When PGO collection is enabled ([`pgo.md`](pgo.md)),
  the operator creates the NATS credentials Secret alongside the NATS account;
  `deploy/` ships a commented example Secret
  and the Deployment's credentials volume (`defaultMode: 0440`),
  read-only mount, and pod `fsGroup: 65532`, pinned by a manifest test.
- [`deploy/chart/profgate/`](../../deploy/chart/profgate/README.md) holds a Helm chart over the same resources,
  for an operator installing Profgate from outside this repository.
  The two surfaces are both shipped and neither is generated from the other:
  the chart templates what an external operator changes,
  the kustomize base is what a repository already using kustomize patches.
  The chart guarantees four things beyond rendering those resources:
  - `checksum/config` on the pod template, holding a hash of the rendered ConfigMap,
    because the binary reads its configuration once at startup and has no reload,
    so without it a `helm upgrade` that changes only configuration rolls nothing out.
  - `limits.memory` derived from `pgo.limits` through the sizing rule of `PGOMemoryBytes`,
    so the limit cannot drift from the configuration it is sized for.
    An explicit `resources` block overrides it;
    with PGO off the limit is a static 512Mi, because the formula reads `pgo.limits`
    and never `pgo.enabled` and would otherwise size a merge that never happens.
  - `podSecurityContext.fsGroup`, 65532 by default, rendering no key at all when set to null,
    for a cluster that assigns its own ranges through a security context constraint.
  - Release-scoped ClusterRole and ClusterRoleBinding names, so two releases in one cluster do not collide,
    over rules identical to the base's.

### 11.1 Dependencies

| Module | Purpose |
|---|---|
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | discovery (only in `internal/k8s`); also the end-to-end harness |
| `github.com/arloliu/fuda` | configuration |
| `gopkg.in/yaml.v3` | strict unknown-key pass before fuda (already a fuda dependency) |
| `github.com/prometheus/client_golang` | metrics |
| `github.com/nats-io/nats.go` | PGO coordination and artifacts (only in `internal/natskv`) |
| `github.com/google/pprof` | profile merge ([`pgo.md`](pgo.md)) and tests: parsing fetched profiles |
| `sigs.k8s.io/yaml` | tests only: golden ClusterRole and `versions.yaml` |

Everything else is the standard library.

---

## 12. Package Layout

```text
cmd/profgate/        CLI: serve, config validate, version
internal/k8s/        the seam; sole non-test importer of client-go
internal/proxy/      upstream HTTP to PodIP:Port, transport, budget, error mapping
internal/httpapi/    routing, realm checks, handlers, error bodies, audit log
internal/config/     fuda-loaded Config, strict pre-parse, validation, hot/restart classification
internal/metrics/    Recorder interface and the Prometheus implementation
internal/admit/      the admission gate shared by interactive requests and Collections
deploy/              kustomize base and Helm chart
test/e2e/            harness, versions.yaml, testapp, overlays
```

---

## 13. Failure Scenarios

| Event | Behavior |
|---|---|
| Kubernetes API unreachable at startup | preflight retries forever; `/healthz` 200; `/readyz` 503; `/v1` returns `503 not_ready` |
| RBAC too narrow | preflight receives `403`; process exits naming the denied (resource, verb) |
| Kubernetes API unreachable while running | targets endpoint serves the cache; profile requests fail at confirmation with `503 discovery_unavailable`; informers reconnect and relist on their own |
| Selected Pod deleted before confirmation | confirmation returns `503 target_changed`; the client retries |
| Selected Pod deleted after confirmation, before the dial | the residual window of section 5.6: a reset or `502` in practice, and, if the address was already reused, a connection to the wrong Pod |
| Gateway replica receives SIGTERM mid-profile | `/readyz` 503; the API listener closes after `server.drainDelay`; the profile completes within the grace period; then exit |
| Gateway replica crashes mid-profile | the client's connection drops; no state to recover |
| Target Pod dies mid-profile | `502` if headers were not yet sent; otherwise a truncated body and `upstream_stream_failed` in the audit log |
| Configuration invalid | process exits at startup with the validation error |
