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
   This design defines the `disabled` mode and the authorization structure every mode shares;
   [`auth.md`](auth.md) defines `basic` and `oidc` on that structure.
8. **Authorization is static access realms** loaded from process configuration.
9. **Nothing the gateway itself emits reveals a Pod IP, the port number behind a `portName` selection,
   or a name the client's realm denies.**
   Hiding the direct path to the pprof endpoint is part of what the gateway is for;
   what a client can still learn about ports by choosing them is listed in section 7.5.
   Bytes the application sends — a profile body, an upstream error body, an allowlisted upstream header —
   are application-controlled and pass through as they are.
10. **Every dependency is auditable in one sitting.**
    The dependency set is listed in this document; adding to it is a design change.

### 1.2 Non-goals

- Continuous profiling, long-term profile storage, flamegraph UI.
  Grafana Pyroscope and Parca exist for that and are not dependencies of this design.
- Profiling languages other than Go.
- Reaching Pods through `pods/exec`, `pods/portforward`, or a sidecar.
- Hot-reloading configuration is designed for in a later revision of this document;
  PGO collection is designed in [`pgo.md`](pgo.md) and authentication modes in [`auth.md`](auth.md).
  The seams that make them additive are called out where they occur.
  Re-reading the API listener's certificate is not configuration hot-reload:
  the two paths are fixed at startup and only the bytes they point at are read again (section 10).
- Client-certificate authentication, ACME in the gateway process, TLS to application pprof ports,
  and a redirect from plaintext to HTTPS.
  A client certificate identifies a caller and therefore belongs to `auth.mode` (section 7.1),
  not to the transport;
  the certificate the API listener serves is produced by cert-manager or an operator and consumed from a mounted Secret.

---

## 2. Architecture

```text
              Developers / CI
                     |
                HTTP / HTTPS        (TLS terminates at the Ingress by default)
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

The API listener serves plaintext HTTP unless `server.tls` names a certificate and a key (section 10),
in which case it serves HTTPS on the same port under the same name.
Ingress or mesh termination stays the shipped topology;
`server.tls` is for the deployment that has no Ingress in front of the gateway,
or a cluster that requires encryption in transit end to end.
The ops listener is always plaintext, for the reason section 7.5 gives:
its protection is a network property, and the kubelet's probe would skip verification anyway.

---

## 3. Permission Boundary

> Profgate requires no Kubernetes write permissions.
> It observes Services, Pods, and EndpointSlices cluster-wide,
> and serves each caller only the namespaces, Services, and profiles that caller's realm admits.
> It connects to the configured pprof port of a Pod,
> and to any port or port name `discovery.pprof.allowedSelections` admits,
> by an exact entry or by a wildcard, wherever NetworkPolicy permits the connection.
> It manipulates only its dedicated `PROFGATE_*` NATS stores.

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
gateway → `PodIP:pprofPort`;
gateway → OpenID Connect issuer, HTTPS, when `auth.mode` is `oidc` ([`auth.md`](auth.md));
`deploy/` ships a commented egress rule for it.

The first flow is HTTPS when `server.tls` is configured and plaintext otherwise;
the port and its name do not change with the scheme, so nothing else in this section moves.
The other three are plaintext: the ops listener by the decision in section 7.5,
and the pprof connections because application pprof endpoints are HTTP by convention.
The certificate reaches the container through a mounted Secret,
so the kubelet reads it and the gateway's ServiceAccount does not:
serving HTTPS adds no Kubernetes permission and leaves section 3.1's seven tuples as they are.

Application pprof ports must not be routed by application Ingress resources.
Where the cluster enforces NetworkPolicy, the pprof port should admit only the gateway's namespace and Pod selector;
`deploy/` ships an example policy.
A client may name the configured default,
or any selection `discovery.pprof.allowedSelections` admits, exactly or by wildcard (section 5.4);
with a wildcard entry there a client may name any port number, or any port name,
and NetworkPolicy is then the only bound on which Pod ports the gateway can reach.
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
When `auth.basic.usersFile`, `auth.oidc.caFile`, `auth.oidc.browser.clientSecretFile`,
or `auth.oidc.browser.cookieKeyFile` is configured ([`auth.md`](auth.md)),
a second Secret volume at `/etc/profgate/auth/` is mounted the same way, under the same `fsGroup`.
The kubelet mounts each Secret;
the gateway's ServiceAccount needs no Secrets API permission,
and the RBAC table is unchanged.
No host namespaces, host paths, `SYS_PTRACE`, or privileged mode.

### 3.5 What a compromised gateway can do

It can read Service, Pod, and EndpointSlice metadata cluster-wide,
and open HTTP connections to any Pod IP on the configured pprof port that NetworkPolicy admits,
plus any port `discovery.pprof.allowedSelections` admits (section 5.4) —
with a `{port: "*"}` entry, that is every port on every Pod NetworkPolicy admits.
It cannot exec into Pods, read Secrets or logs, port-forward, mutate any Kubernetes object,
or reach the host.
Under `basic` authentication it holds bcrypt hashes, not passwords.
Under `oidc` it holds the issuer's public keys, the cookie key, and, if configured, a client secret;
with those it can mint a session cookie for any principal and realm it already serves,
which is no more than it can already do by ignoring authentication.
It holds no refresh token and cannot obtain a token from the issuer on its own.
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

// PortSelection is the client's port choice for one request (section 5.4);
// the zero value means the configured default.
type PortSelection struct {
    Port     int32
    PortName string
}

// ServiceRef names one Service in the cache.
type ServiceRef struct {
    Namespace, Name string
}

type Discovery interface {
    // Targets returns the currently eligible backends of a Service
    // whose pprof port resolves under port.
    // Order is unspecified.
    Targets(ctx context.Context, namespace, service string, port PortSelection) ([]Target, error)
    // HasSynced reports whether every informer has completed its initial list.
    HasSynced() bool
    // Confirm re-reads the Pod behind t from the API server and reports
    // whether t is still an accurate description of it (section 5.6).
    Confirm(ctx context.Context, t Target) error
    // Catalog lists the Services with a non-empty selector from the cache,
    // sorted by namespace then name.
    // An empty namespace means every namespace; a namespace the cache lacks is an empty list, not an error.
    // It issues no request; an error means the lister could not be read.
    Catalog(ctx context.Context, namespace string) ([]ServiceRef, error)
}
```

`Catalog` serves the listing routes of [`ui.md`](ui.md).
It reads the Service informer cache and issues no request,
so the RBAC table of section 3.1 does not change for it.

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
8. A pprof port resolves for the Pod (section 5.4), from the request's `port` or `portName` when given.

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

Workloads in one cluster expose pprof on fixed but different ports,
and some of their manifests are not the operator's to change,
so one global `port` or `portName` cannot describe every Service.
A client may therefore name the port per request;
the configuration keeps the default and bounds what a client may name.

`discovery.pprof` names the default port in one of two ways (exactly one must be set):

- `port: 6060` — the same numeric port for every Pod.
- `portName: pprof` — the named `containerPort` found in the Pod's `spec.containers[].ports`
  whose `protocol` is `TCP` (or unset, which Kubernetes defaults to TCP).
  A Pod with no TCP port of that name is ineligible; there is no fallback to a number.

A request may carry `port=<1–65535>` or `portName=<name>` (sections 6.2 and 6.3), never both.
Either one replaces the configured default for that request and resolves under the same two rules:
`port` is used for every Pod without checking that the Pod declares it,
and `portName` names the TCP `containerPort`, leaving a Pod without it ineligible.
`Target.Port` is the resolved number either way and is never serialized.

`discovery.pprof.allowedSelections` bounds what a client may name.
It is a list, and each entry is exactly one of `{port: <1–65535>}` or `{portName: <container-port name>}`;
an entry carrying both keys or neither, and an entry repeating an earlier one, are validation errors.
A value the list does not admit is refused with `400 port_not_allowed` before discovery runs.

**The empty list is the default and admits nothing beyond the configured default.**
A request may then name only `discovery.pprof.port` or `discovery.pprof.portName`, whichever is set,
and every other value is `400 port_not_allowed`.
Because exactly one of those two is set, an empty list also refuses the other kind outright:
under a `portName` default every numeric `port=` is refused,
and under a numeric default every `portName=` is refused.
Naming a port is a capability the operator grants, never one a fresh install already carries.

`{port: "*"}` admits any port number and `{portName: "*"}` admits any container-port name.
Each wildcard covers its own kind and no more:
with `{port: "*"}` alone a `portName=` beyond a named default is still refused,
and with `{portName: "*"}` alone a `port=` beyond a numeric default is still refused.
A wildcard beside a concrete entry of the same kind is a validation error,
because the concrete entry then decides nothing the wildcard has not already decided.

The configured default `port` or `portName` is always permitted, whether or not the list holds it.
Listing it is allowed and changes nothing.
Refusing that redundancy would break a configuration that used to validate
as soon as `discovery.pprof.port` moved to a number the list already holds,
which is a worse trade than a pointless entry.

The status is `400` rather than `403` because the value is invalid under this gateway's configuration,
and the response must not say which ports Pods expose (section 7.5).
The list is global; a realm decides namespaces, Services, and profiles, never ports (section 7.4).

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

All paths are under `/v1` on the API listener,
except the three `/auth/` routes that [`auth.md`](auth.md) adds when its browser flow is configured
and the `/ui/` and `/` routes of [`ui.md`](ui.md) when `ui.enabled`.
The four listing routes of [`ui.md`](ui.md) —
`/v1/namespaces`, `/v1/namespaces/{namespace}/services`, `/v1/whoami`, and `/v1/limits` —
are `/v1` routes defined in that document.
The product name does not appear in any path.
Every response carries `Cache-Control: no-store`.

### 6.1 Request algorithm

Every `/v1` request passes through these steps in order;
the first failing step produces the response.
Steps 8–10 differ by endpoint.

1. **Route.** Unknown path → `404 route_unknown`; unknown `{profile}` → `404 profile_unknown`.
   Path segments for `{namespace}` and `{service}` must be DNS-1123 labels, otherwise `404 route_unknown`.
2. **Method.** A method the route does not accept → `405 method_not_allowed` with `Allow` listing those it does;
   the two routes defined here accept `GET` only.
3. **Readiness.** `HasSynced()` false → `503 not_ready`.
4. **Credential placement.**
   `access_token` as a query parameter → `400 invalid_parameter`.
5. **Authentication.**
   Resolve the principal and its realm per `auth.mode`
   → `401 unauthenticated`, `429 too_many_auth`, `503 auth_unavailable`, or a `302` to login ([`auth.md`](auth.md)).
6. **Realm.** Namespace, then Service, then (profile endpoint only) profile → `403 realm_denied`.
7. **Parameters.**
   Targets endpoint: any query parameter other than `port` or `portName` → `400 invalid_parameter`.
   Profile endpoint: validate every parameter per section 6.3 → `400 invalid_parameter` or `400 seconds_exceeds_limit`.
   Both endpoints: `port` or `portName` outside its grammar, or both given → `400 invalid_parameter`;
   a value `discovery.pprof.allowedSelections` does not admit → `400 port_not_allowed` (section 5.4).
   A refused port never reaches discovery.
8. **Discovery.** `Targets()` with the request's port selection → `404 service_not_found`, `422 service_selectorless`.
9. **Filter and select.**
   Targets endpoint: respond `200` with the full list, sorted (section 6.2).
   Profile endpoint: apply `version`;
   if `pod` is present and no remaining target has that name → `404 pod_not_found`;
   if `pod` is absent and no target remains → `503 no_targets`;
   otherwise pick `pod`, or one target by `strategy` (`random` when absent).
10. **Admit** (profile endpoint only).
   Acquire one of `limits.maxConcurrentProfiles` slots from the shared admission gate (`internal/admit`) without waiting;
   none free → `429 too_many_profiles`.
   The slot is held through confirmation and proxying and released when the response completes.
   The overall request budget (section 6.4) starts here.
11. **Confirm** (profile endpoint only, section 5.6) → `503 target_changed`, `503 discovery_unavailable`.
12. **Proxy** (profile endpoint only, section 6.4).

Realm denial precedes discovery,
so a caller denied a namespace receives the same `403` whether or not the Service exists.

The four listing routes of [`ui.md`](ui.md) run the route, method, readiness, credential-placement,
and authentication steps as written;
the realm step refuses only the Service list, for a namespace the realm does not admit,
while the namespace list is filtered and `whoami` and `limits` describe the caller;
the parameter step refuses any query parameter;
then they read the Service cache, with no discovery, admission, confirmation, or proxy step
([`ui.md`](ui.md) *Request algorithm for the listing endpoints*).
They accept `GET` only, like the two routes defined here.

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

The endpoint takes the optional `port` or `portName` parameter of section 6.3 and no other;
the list then holds the Pods eligible under that port.
`targets` is sorted by `pod` name;
the response `Content-Type` is `application/json`, as it is for every gateway error body.
A Service with no eligible backends returns `200` with an empty array.
`ip` and `port` are never included.
`version` is present and empty when the Pod has no version label.

#### Listing endpoints

Four routes list what the caller's realm admits and describe the caller:
`GET /v1/namespaces`, `GET /v1/namespaces/{namespace}/services`, `GET /v1/whoami`, and `GET /v1/limits`.
Their response shapes are defined in [`ui.md`](ui.md) *Response shapes*.

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
| `port` | decimal integer, 1–65535 | pprof port for every Pod, replacing `discovery.pprof.port`/`portName`; must pass `allowedSelections` (section 5.4) |
| `portName` | IANA service name (the Kubernetes container-port name rule) | named TCP container port, replacing the configured default; must pass `allowedSelections`; excludes `port` |

Rules:

- A parameter given more than once, an empty value, an unknown parameter name,
  or a value outside its grammar → `400 invalid_parameter`.
- `seconds` on a profile that does not take it → `400 invalid_parameter`.
- `port` and `portName` together → `400 invalid_parameter`;
  either one `allowedSelections` does not admit → `400 port_not_allowed`, before discovery.
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
  starts when the request enters the Admit step of section 6.1 and bounds confirmation, dial, header wait,
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
| 400 | `invalid_parameter`, `seconds_exceeds_limit`, `port_not_allowed` |
| 401 | `unauthenticated` |
| 403 | `realm_denied` |
| 404 | `route_unknown`, `service_not_found`, `pod_not_found`, `profile_unknown` |
| 405 | `method_not_allowed` |
| 422 | `service_selectorless` |
| 429 | `too_many_profiles`, `too_many_auth` |
| 502 | `upstream_unreachable`, `upstream_redirect` |
| 503 | `not_ready`, `no_targets`, `target_changed`, `discovery_unavailable`, `auth_unavailable` |
| 504 | `upstream_timeout` |

Upstream non-`2xx` responses are not gateway errors:
they pass through with their own status, body, and `Content-Type`,
and are recorded with code `upstream_<status>`.

`pod_not_found` covers a Pod that does not exist,
one that is not an eligible backend,
and one filtered out by `version`.

`port_not_allowed` names only the value the client sent, never a port a Pod exposes;
receiving it does tell the client that `discovery.pprof.allowedSelections` does not admit the value (section 7.5).

`discovery_unavailable` also covers a Service cache read that fails on a listing route of [`ui.md`](ui.md);
such a route never answers an empty `200` in its place.
`405 method_not_allowed` under `/ui/` and on `/` carries `Allow: GET, HEAD`,
because those routes serve files and accept `HEAD`.

---

## 7. Authentication and Authorization

Both are static process configuration (section 10), never runtime state;
[`auth.md`](auth.md) keeps that true for its two modes,
whose only runtime-acquired trust state is the issuer's public keys;
users, mappings, secrets, and cookie keys are configuration-derived snapshots.

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

[`auth.md`](auth.md) defines the `basic` and `oidc` modes;
each resolves a principal and a realm and changes nothing below it.

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
`discovery.pprof.allowedSelections` (section 5.4) is global in the same way:
a realm decides namespaces, Services, and profiles, and no realm widens or narrows which ports a client may name.

### 7.5 Non-disclosure

A client's realm bounds everything the gateway generates on the API listener:
the targets response, gateway error bodies, gateway-owned headers, and the transport-error envelopes
name only namespaces, Services, and Pods the realm admits and never a Pod address.
Of ports, the guarantee is narrower:
no response carries the port number a `portName` selection resolved to,
and the `X-Pprof-Target-*` headers never carry a port;
a `400 port_not_allowed` body names only the value the client sent.
Choosing ports, and reading `/v1/limits`, still lets an authorized client observe four things,
listed here so nobody mistakes them for leaks the design closes:

- `portName` on the targets endpoint changes per-Pod eligibility,
  so calling it with different names reveals which admitted Pods declare each named TCP port.
- A numeric `port` on the profile endpoint reaches the Pod without checking its declarations,
  so an authorized caller can combine `pod=` with different numbers,
  and the proxy outcomes of section 6.4 then tell an open pprof port from a refused, silent, redirecting, or non-pprof HTTP port —
  a port-scanning capability over admitted Pods,
  bounded by the realm, `discovery.pprof.allowedSelections`, and NetworkPolicy, and nothing else.
- `400 port_not_allowed` reveals that `discovery.pprof.allowedSelections` does not admit a value.
  It reveals nothing about Pods: realm evaluation precedes it and discovery never runs.
- Every caller the configured `auth.mode` admits — under `disabled`, that is every caller —
  reads `discovery.pprof.allowedSelections` and the configured default from `/v1/limits`.
  The values are global operator configuration, not cluster state:
  they say which values any client may name, not which port any Pod exposes,
  and the number a `portName` resolves to on a Pod stays hidden;
  the argument is in [`ui.md`](ui.md) *Limits*.

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
profgate auth hash
profgate version
```

Standard-library `flag` with hand-written subcommand dispatch.

### 8.2 Logging

`log/slog`, JSON to stdout, at the level `server.logLevel` names.
Every `/v1` request emits one record on completion:

```text
principal, namespace, service, pod, profile, seconds, port, status, code, duration_ms
```

`auth_reason` is added on authentication failures and login redirects,
with the values [`auth.md`](auth.md) lists;
one of them, `internal`, marks an authenticator error the gateway could not classify, answered `503 auth_unavailable`.
The `/auth/` routes write a line with no namespace or Service ([`auth.md`](auth.md)).
The four listing routes of [`ui.md`](ui.md) write the record with `namespace` set on the Service list only
and `service`, `pod`, `profile`, `port`, and `seconds` empty;
requests under `/ui/` and to `/` write no record — they carry no principal and name nothing a realm bounds.

`port` is the client's port selection as sent, a number or a name, empty when absent;
for a numeric selection that is also the resolved port,
for a name it is the name and never the number it resolved to.
When the selection is malformed, repeated, or both parameters are present,
the field is empty and the request fails `invalid_parameter`;
a disallowed value is recorded as sent with `port_not_allowed`.
`code` is `ok` for a successful proxy, the gateway error code, or the upstream code from section 6.4.
This is the audit trail.
Records never contain a Pod IP.

### 8.3 Health

Both paths are on the ops listener and have no authentication or realm check.

| Path | `200` when |
|---|---|
| `/healthz` | the process is serving HTTP |
| `/readyz` | issuer discovery and the initial key fetch have succeeded when `auth.mode` is `oidc` ([`auth.md`](auth.md)), preflight has passed, and `HasSynced()` is true |

`/readyz` reflects the initial sync of the informers as a whole.
It does not track API reachability afterwards:
a gateway that cannot reach the API server still answers the targets endpoint from its cache
and refuses to proxy (section 5.6), which is the correct behavior, not a reason to be removed from the Service.

### 8.4 Metrics

`/metrics` on the ops listener exposes Prometheus text format via `prometheus/client_golang`:

| Metric | Labels |
|---|---|
| `profgate_requests_total` (counter) | `endpoint` (`targets`/`profile`/`namespaces`/`services`/`whoami`/`limits`/`ui`), `profile`, `code` |
| `profgate_request_duration_seconds` (histogram) | `profile` |
| `profgate_confirm_total` (counter) | `result` (`ok`/`changed`/`unavailable`) |
| `profgate_profiles_in_flight` (gauge) | — |
| `profgate_discovery_synced` (gauge) | — |
| `profgate_tls_reloads_total` (counter) | `result` (`applied`/`unchanged`/`failed`) |
| `profgate_tls_certificate_expiry_seconds` (gauge) | — |
| `profgate_auth_failures_total` (counter) | `mode`, `reason` ([`auth.md`](auth.md)) |
| `profgate_auth_sessions_issued_total` (counter) | — |
| `profgate_oidc_jwks_refresh_total` (counter) | `result` (`ok`/`failed`) |
| `profgate_oidc_jwks_keys` (gauge) | — |
| `profgate_oidc_jwks_age_seconds` (gauge) | — |
| `profgate_auth_file_reload_total` (counter) | `file` (`users`/`cookie_key`), `result` (`ok`/`failed`) |
| `profgate_auth_cookie_key_info` (gauge) | `fingerprint`, `role` (`current`/`previous`) |

The two TLS metrics exist only while `server.tls` is configured.
`profgate_tls_certificate_expiry_seconds` holds the served leaf's `notAfter` as a Unix timestamp.
A rotation that quietly stopped working is alertable from it,
while the certificate the gateway still serves is valid.

`profgate_request_duration_seconds` uses buckets `0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300` seconds,
wide enough for the durations `limits.cpuSeconds` and `limits.traceSeconds` allow (section 6.3)
and the header deadline and overall budget built from them (section 6.4).

Every label has a fixed value set:
`profile` is the eight names or `none`, `code` takes the values in sections 6.4 and 6.5
with upstream statuses bucketed as `upstream_<status>`.
The `endpoint` values `namespaces`, `services`, `whoami`, `limits`, and `ui` belong to [`ui.md`](ui.md),
with `profile` fixed to `none`;
`ui` covers `/ui/`, every path under it, and `/`,
and its `code` is `ok` for a `200` or the `302`, `route_unknown`, `method_not_allowed`,
or `internal_error` for any other status the console wrote.
The client's port selection is not a label either;
it is client-controlled and would add a series per value.
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
[discovering] only when auth.mode is oidc: fetch issuer discovery and the JWKS (auth.md)
  |   failure: retry with backoff for auth.oidc.discoveryTimeout, then exit 1
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
A second `SIGTERM` or `SIGINT` during the drain ends the process at once with a non-zero exit,
logging that it did not finish:
the drain's own waits are the ones the work legitimately needs,
so only the operator can say it has gone on long enough.

A listener that fails is fatal:
the process logs the failure, waits out the in-flight requests, and exits 1.
It skips `server.drainDelay`, because a listener that has failed receives nothing that window protects,
and it never waits for the Collection drain of [`pgo.md`](pgo.md) section 12.4,
because a replica with no listener has nothing left to serve
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
  plus dual-stack and IPv6-only Services,
  and a request `portName` that one Pod declares and another does not, leaving only the first eligible.
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
  an allowed numeric port that is not a pprof listener (a plain HTTP server answering `404`)
  passing through as `upstream_404`,
  client cancellation propagated,
  upstream `404`/`429`/`500` pass-through, reset before headers, reset after partial body,
  and concurrent requests with different header deadlines against delayed upstreams proving the deadlines are independent.
- `internal/httpapi` against a fake `Discovery`:
  a table over every route, method, parameter combination, and error code;
  realm denial identical for an existing and a missing Service;
  no response, header, or error string containing a Pod IP.
  For `port`: `0`, `65536`, `abc`, empty, and repeated are `400 invalid_parameter`.
  For `portName`, each of these is `400 invalid_parameter`:
  empty, repeated, 16 characters or longer, uppercase, a leading or trailing hyphen, and all digits;
  `abc` is valid.
  `port` and `portName` together are `400 invalid_parameter`.
  A value `allowedSelections` does not admit is `400 port_not_allowed` and the fake `Discovery` records no call;
  an empty list refuses every value but the configured default,
  including every value of the kind the default is not;
  a listed entry passes;
  `{port: "*"}` passes any number while a `portName=` beyond the default is still refused,
  and `{portName: "*"}` passes any name while a `port=` beyond the default is still refused;
  the configured default passes whether or not the list holds it;
  a `portName` default with only `{port: N}` entries admits that name and those numbers
  and refuses every other name and number,
  and a `port` default with only `{portName: name}` entries admits that number and those names
  and refuses every other number and name,
  each refusal reaching no `Discovery` call;
  a realm denial with a refused port is `403 realm_denied`, proving realm evaluation precedes `allowedSelections`;
  a `portName` present on one fake Pod and absent on another lists only the first on the targets endpoint,
  and one absent from every Pod lists none and profiles `503 no_targets`.
  The audit `port` field is empty for an absent selection,
  the value as sent for a single valid `port` or `portName`,
  empty with `invalid_parameter` for a repeated, malformed, or doubled selection,
  and the value as sent with `port_not_allowed` for a refused one;
  a `portName` selection never logs the resolved number,
  and no response or header carries the number a `portName` resolved to.
- `internal/config`: every environment override, unknown keys at each nesting level,
  numeric-only, name-only, both, and neither of `port`/`portName` (neither normalizes to 6060),
  `allowedSelections` from file and from its comma-separated environment variable,
  with an out-of-range number, an invalid name, an entry carrying both keys or neither,
  a duplicate entry,
  and a wildcard beside a concrete entry of its own kind, each rejected from each source;
  `PROFGATE_PPROF_ALLOWED_SELECTIONS` absent leaving a file list that holds `port: "*"` in place,
  set to the empty string replacing that same file list with `[]`,
  set to one token and to several,
  and set with a leading, a trailing, or a doubled comma rejected as an empty token;
  a file still setting `allowedPorts` or `allowedPortNames`,
  and an environment still setting `PROFGATE_PPROF_ALLOWED_PORTS` or `PROFGATE_PPROF_ALLOWED_PORT_NAMES`,
  each rejected with a message naming the replacement,
  asserted on the message for all four names;
  invalid limits, unknown realm reference,
  and a request paused between realm evaluation and discovery while the config pointer is swapped,
  proving the request uses one snapshot.
- `internal/tlscert` against files in a temporary directory:
  the first load serves the leaf the files hold and `GetCertificate` reads no file afterwards;
  rewriting both files with a second authority's pair and refreshing once serves the new leaf;
  a rotation performed the way the kubelet performs one —
  writing `..data_a` and `..data_b` directories and renaming a `..data` symlink over the old one —
  is followed;
  a mismatched or truncated pair on disk leaves the previous certificate in place and counts a `failed` reload;
  and a refresh over unchanged files re-parses nothing.
- `internal/config`: `server.tls` with both keys, neither, and one of the two;
  a `certFile` naming a path that does not exist is fatal;
  a `minVersion` outside `1.2` and `1.3` is rejected.
- A handshake test on the API listener as `serve` builds it,
  proving a client that pins the certificate's authority completes a handshake
  and that replacing the files on disk changes which authority is accepted.
  The `cmd/profgate` tests drive the process's own listeners over loopback,
  because the listener the process builds is what they are about;
  the handshake is the case that cannot be written against a stand-in
  ([`300-testing.md`](../../.agents/rules/300-testing.md)).
- The golden ClusterRole test (section 3.1) parses `deploy/` and compares rule tuples.
- A manifest test pins the gateway NetworkPolicy's selectors and ports and the Service's port list;
  the kind lanes cannot prove NetworkPolicy enforcement, only that the manifest is shaped as specified.
- Chart tests render `deploy/chart/profgate` with the `helm` binary mise pins, and assert on the objects:
  the derived memory limit equals `PGOMemoryBytes` applied to the rendered ConfigMap loaded through `internal/config`,
  which also proves the rendered configuration parses;
  `checksum/config` moves with a configuration change, with `tls.enabled`, and stays put for an unrelated one;
  `tls.enabled` renders the certificate volume, its read-only mount, `defaultMode: 0440`, and a non-optional Secret;
  a null `fsGroup` renders no key;
  the ClusterRole rules match the base's;
  and `helm lint` passes.
  They skip when `helm` is absent rather than failing.
- A configuration test sets every `PROFGATE_*` variable and proves each lands on its field,
  guarding against a doubled prefix in a tag.
- `internal/ui` against its embedded tree, per [`ui.md`](ui.md) *Unit*:
  the tree hash, the shell and asset headers, the manifest hashes, relative imports only,
  no inline script or style, and the source scan of *Rendering response values*.
- `internal/ui` against the console's port-control model, per [`ui.md`](ui.md) *Unit*:
  a table over deriving the menu and the free-form fields from `pprof.default` and `allowedSelections`,
  and over serializing the control's state,
  evaluated in the ECMAScript interpreter section 11.1 lists.
- The listing routes in `internal/httpapi`, per [`ui.md`](ui.md) *Unit*:
  the request algorithm table, realm filtering over the four combinations,
  `whoami` and `limits` contents — `limits` carrying `allowedSelections` as an array of one-key objects,
  `[]` when the list is empty and never `null` —
  no Pod IP or Pod-declared port in a list, hostile names,
  the `/ui/` and `/` dispatch, and the audit and metrics rows.
- `Catalog` in `internal/k8s`, per [`ui.md`](ui.md) *Unit*:
  it reads only the Service lister and the recording transport sees nothing beyond the seven tuples;
  a selectorless Service is not listed.
- `internal/config`: `ui.enabled` from file and environment, a non-boolean rejected,
  and `ui.enabled` under `auth.mode: oidc` rejected without a `browser` block and accepted with one.
- The client-go import check:
  every non-test Go file outside `test/` that imports `k8s.io/client-go` is under `internal/k8s/`.
- The Kubernetes module check: `k8s.io/client-go`, `k8s.io/api`, and `k8s.io/apimachinery` share one minor in `go.mod`.

**End-to-end**, run by `mise run test:e2e`, minutes.
Plain `go test` under `//go:build e2e` in `test/e2e/`, inside the main module.
No end-to-end framework; the reasoning is recorded in
[`../decisions/e2e-without-framework.md`](../decisions/e2e-without-framework.md).
`controller-runtime/envtest` is not used: it runs no controller-manager and no kubelet,
so it cannot produce real EndpointSlices or reachable Pods.

**Authentication**: `internal/auth` unit tests, the `internal/httpapi` integration tests,
and the two end-to-end lanes — `oidc` with the browser flow against Dex, and `basic` over TLS —
are specified in [`auth.md`](auth.md).

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
  `degraded: true` skips exactly the scenarios that declare it on that lane
  (2, the readiness half of 3, 4, 8, 9, 11, and 12);
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
  image substitution, `replicas: 2`, the default gateway's `allowedSelections`,
  and ClusterRole variants missing `watch` and missing `get`.
- The reduced-ClusterRole scenario runs a second gateway Deployment with its own ServiceAccount
  and ClusterRoleBinding so it cannot disturb the main gateway.
- The default gateway's `allowedSelections` holds `{port: 6061}` and `{portName: pprof-alt}`,
  so it accepts the test app's second port and its name,
  and the `ports-gateway` overlay runs a gateway whose `allowedSelections` is empty,
  so the same two values are refused there and only the configured default is accepted,
  because configuration is loaded once per process and one gateway cannot show both the accepted and the refused outcome.
- The harness reaches individual gateway Pods and test-app Pods through client-go `portforward`
  with the tester's kubeconfig, because Pod IPs are unreachable from outside kind.
- The test application lives in `test/e2e/testapp/`:
  `net/http/pprof` on `:6060` (container port `pprof`)
  and on a second `http.Server` on `:6061` (container port `pprof-alt`) sharing one handler,
  a readiness probe on `/healthz` with `periodSeconds: 1` and `failureThreshold: 1`,
  and `POST /healthz/fail` and `POST /healthz/pass` that flip the probe result for that process.
  `GET /hits` keeps its `pprof` total and adds a per-listener count keyed by listen address:
  `{"pprof": 3, "hits": {":6060": 2, ":6061": 1}}`.
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
10. A gateway configured with `server.tls` serves `/v1` over HTTPS,
    a client pinning a different authority is refused,
    and replacing the Secret's contents with a certificate from a second authority makes the gateway serve the new one,
    while the Pod's UID and restart count stay as they were.
    The scenario reaches no application Pod: it exercises the gateway's own listener.
11. Against the default gateway, whose `allowedSelections` lists the test app's second port and its name,
    `?port=6061` fetches a profile the test app's `/hits` attributes to `:6061`,
    and `?portName=pprof-alt` does the same (`needsPodReach`).
12. Against the `ports-gateway` overlay, whose `allowedSelections` is empty,
    `?port=6061` and `?portName=pprof-alt` are each `400 port_not_allowed`
    while `?port=6060`, the configured default, still fetches a profile,
    and the test app's `:6061` count does not move (`needsPodReach`).
    The two halves are separately registered scenarios because one gateway configuration cannot prove both outcomes.
13. Every scenario runs on every lane whose capabilities it does not exclude;
    a lane skips a scenario only by `degraded` or `networkPolicy`, and the skip is logged by scenario name.
14. Inside the browser-flow scenario against Dex, with `ui.enabled`:
    `/ui/` serves the shell with its security headers to a caller with no cookie,
    a `fetch`-shaped `/v1/whoami` without a cookie is `401` and not `302`,
    the login walk returns to `/ui/?ns=x`,
    the four listing routes answer `200` with the cookie and the Service list holds the test app's Service,
    and logout lands on `/` and then `/ui/` ([`ui.md`](ui.md) *End to end*).
15. Inside the `basic` over TLS scenario, with `ui.enabled`:
    `/ui/` is `200` without a credential,
    `/v1/namespaces` is `401` with `WWW-Authenticate: Basic realm="profgate"` without one and `200` with one,
    and `/v1/limits` reports the lane's configured limits ([`ui.md`](ui.md) *End to end*).

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
| `server.drainDelay` | `PROFGATE_DRAIN_DELAY` | `5s` | restart | 0s–60s |
| `server.tls.certFile` | `PROFGATE_TLS_CERT_FILE` | — | restart (path) | a readable file; set with `keyFile` or not at all |
| `server.tls.keyFile` | `PROFGATE_TLS_KEY_FILE` | — | restart (path) | a readable file; pairs with `certFile` |
| `server.tls.minVersion` | `PROFGATE_TLS_MIN_VERSION` | `1.2` | restart | `1.2`, `1.3` |
| `discovery.versionLabel` | `PROFGATE_VERSION_LABEL` | `app.kubernetes.io/version` | restart | valid label key |
| `discovery.pprof.port` | `PROFGATE_PPROF_PORT` | `6060` when `portName` is also absent | restart | 1–65535; exactly one of `port`/`portName` after normalization |
| `discovery.pprof.portName` | `PROFGATE_PPROF_PORT_NAME` | — | restart | IANA service name (the Kubernetes container-port name rule) |
| `discovery.pprof.allowedSelections` | `PROFGATE_PPROF_ALLOWED_SELECTIONS` (comma-separated) | empty: only the configured default accepted | restart | each entry exactly one of `port` (1–65535 or `"*"`) and `portName` (an IANA service name or `"*"`); no duplicate; no wildcard beside a concrete entry of its own kind |
| `limits.cpuSeconds` | `PROFGATE_LIMIT_CPU_SECONDS` | `60` | restart | 1–86400 |
| `limits.traceSeconds` | `PROFGATE_LIMIT_TRACE_SECONDS` | `60` | restart | 1–86400 |
| `limits.maxConcurrentProfiles` | `PROFGATE_LIMIT_MAX_CONCURRENT_PROFILES` | `16` | restart | 1–1024 |
| `auth.mode` | `PROFGATE_AUTH_MODE` | `disabled` | restart | `disabled`, `basic`, `oidc`; the mode-specific keys are in [`auth.md`](auth.md) |
| `auth.anonymousRealm` | `PROFGATE_ANONYMOUS_REALM` | — | hot | required in `disabled`, forbidden otherwise; names an entry in `realms` |
| `realms` | — | — | hot | at least one; entries per section 7.2 |
| `ui.enabled` | `PROFGATE_UI_ENABLED` | `false` | restart | boolean; under `auth.mode: oidc` requires `auth.oidc.browser` ([`ui.md`](ui.md) *Configuration*) |

`PROFGATE_PPROF_ALLOWED_SELECTIONS` holds comma-separated tokens,
each `port:<number>`, `portName:<name>`, `port:*`, or `portName:*`,
and replaces the file's list rather than adding to it.
Three cases, because the file's list may hold a wildcard and the difference decides what a client may name:

- **Absent** — the file's list stands, whatever it holds.
- **Present and empty** — the list becomes `[]`, and only the configured default is accepted;
  this is how a deployment narrows an inherited wildcard without editing the file it inherited.
- **Present and non-empty** — the tokens are parsed in order into the list.

A parsed list is validated by the rules the file's list obeys and no others:
the same entry grammar, no duplicate, and no wildcard beside a concrete entry of its own kind.
An empty token inside a non-empty value — a leading, a trailing, or a doubled comma —
is a validation error rather than a silently dropped entry,
so a typo cannot quietly widen or narrow what the gateway accepts.

`discovery.pprof.allowedPorts` and `discovery.pprof.allowedPortNames` do not exist,
and neither do `PROFGATE_PPROF_ALLOWED_PORTS` and `PROFGATE_PPROF_ALLOWED_PORT_NAMES`.
Setting any one of the four fails validation with a message naming `allowedSelections`
and `PROFGATE_PPROF_ALLOWED_SELECTIONS`,
so an operator carrying an older deployment forward reads what to write,
instead of watching a key or a variable be ignored into a default-deny gateway.
The two key names are checked for before the strict unknown-key pass,
which would otherwise report only that the key is unknown and leave the operator to guess the replacement.
The two variable names have no such pass to reach them —
an environment variable no field claims is invisible to fuda —
so they are checked for by name, in the same place, before the file is decoded.

Replacing two fail-open lists with one default-deny list is a breaking change and ships in the next minor version.
Each old list converts on its own, and the two conversions do not interact:

| Old value | New entry |
|---|---|
| `allowedPorts: []` | `- port: "*"` |
| `allowedPortNames: []` | `- portName: "*"` |
| `allowedPorts: [6061, 6062]` | `- port: 6061` and `- port: 6062` |
| `allowedPortNames: [pprof-alt]` | `- portName: pprof-alt` |

A file that set both lists empty therefore converts to both wildcards,
which is the configuration that keeps the old behavior exactly.
Adopting default-deny instead is a separate decision:
a deployment that wants clients to name nothing beyond the configured default writes no entry at all,
and one that wants a fixed set writes that set as one-key entries.

```yaml
server:
  listen: ":8080"
  opsListen: ":9090"
  logLevel: info
  drainDelay: 5s
discovery:
  versionLabel: app.kubernetes.io/version
  pprof:
    port: 6060
    allowedSelections: []
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
no reloader ships yet, and the users file and the cookie key file of [`auth.md`](auth.md)
are the only values re-read while the process runs.
`restart` marks a field whose change requires a process restart.
`restart (path)` marks a field whose path is fixed for the life of the process
while the file's contents are read again: a rotated certificate is served without a restart.
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

**`server.tls` is presence-implied**, like `nats.credsFile` and unlike `pgo.enabled`:
it carries two paths and starts no subsystem, so there is no `enabled` flag to disagree with them.
Neither path set is plaintext, which is the shipped default;
both set is HTTPS on the API listener;
exactly one set is a validation error naming the missing key.
Both files are opened during validation, and the pair they hold is parsed at startup.
A path that does not exist, or a certificate that does not match its key, exits the process,
rather than leaving a listener that fails every handshake.

**The certificate is re-read while the process runs.**
The gateway keeps the parsed pair behind an atomic pointer and serves it from `tls.Config.GetCertificate`,
so a handshake reads no file.
A goroutine re-reads both paths every 30 seconds, hashes their contents,
and parses and swaps only when the hash differs;
a read or parse that fails leaves the previous pair in place, logs at warn, and counts a `failed` reload.
The files are polled rather than watched.
The kubelet replaces a Secret volume by writing a new `..data_<timestamp>` directory,
then renaming the `..data` symlink over the old one,
which leaves a watch armed on the file path pointing at the old inode.
Polling also keeps the dependency set unchanged (section 11.1).
The end-to-end delay after a Secret is updated is dominated by the kubelet's own sync period, not by this interval.

---

## 11. Build and Deployment

- Module `github.com/arloliu/profgate`; binary `profgate`; entrypoint `cmd/profgate`.
- Images are built with `ko`: no Dockerfile, distroless base, non-root.
- `deploy/` holds plain YAML with a kustomize base:
  ServiceAccount, ClusterRole, ClusterRoleBinding,
  ConfigMap with the example configuration mounted read-only at `/etc/profgate/config.yaml`
  (its `discovery.pprof.allowedSelections` is empty,
  so a client may name only the configured default until the operator lists more),
  Deployment (`replicas: 2`, hardened as in section 3.4, `--config /etc/profgate/config.yaml`,
  readiness probe on the ops listener's `/readyz`, `terminationGracePeriodSeconds: 125`),
  a `ClusterIP` Service exposing only the API port,
  a NetworkPolicy for the gateway Pods that admits the API port from the Ingress controller's namespace
  and the ops port from the monitoring namespace (both namespace selectors are kustomize-patched per cluster),
  and an example NetworkPolicy for application pprof ports.
- The kustomize base serves plaintext HTTP.
  `deploy/secret-tls-example.yaml` is a commented example of the `kubernetes.io/tls` Secret an operator creates,
  next to the commented NATS one and outside `deploy/base` for the same reason.
  A base that mounted a Secret nobody had created would make `kubectl apply -k deploy/base` produce a Pod
  that never starts.
- When an authentication mode needs files ([`auth.md`](auth.md)),
  `deploy/` ships a commented example Secret for `/etc/profgate/auth/`,
  the Deployment's volume and mount for it, an egress NetworkPolicy rule to the issuer,
  and Helm values for each, pinned by the manifest test.
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
  Beyond rendering those resources, the chart guarantees:
  - `checksum/config` on the pod template, holding a hash of the rendered ConfigMap,
    because the binary reads its configuration once at startup and has no reload,
    so without it a `helm upgrade` that changes only configuration rolls nothing out.
  - `limits.memory` derived from `pgo.limits` through the sizing rule of `PGOMemoryBytes`,
    so the limit cannot drift from the configuration it is sized for.
    An explicit `resources.limits` overrides it,
    and `resources.requests` is a separate half rendered as written,
    shipping the CPU request a namespace whose quota counts `requests.cpu` needs;
    with PGO off the limit is a static 512Mi, because the formula reads `pgo.limits`
    and never `pgo.enabled` and would otherwise size a merge that never happens.
  - `podSecurityContext.fsGroup`, 65532 by default, rendering no key at all when set to null,
    for a cluster that assigns its own ranges through a security context constraint.
  - Release-scoped ClusterRole and ClusterRoleBinding names, so two releases in one cluster do not collide,
    over rules identical to the base's.
  - `discovery.pprof.allowedSelections` rendered as an empty list in `values.yaml`,
    so a chart install accepts only the configured default until the operator lists more;
    the binary's own default is the same empty list.
  - An Ingress, off by default, routing `/`, `/ui/`, `/auth/`, and `/v1/` to the Service's API port,
    so an operator reaching the gateway from outside the cluster does not write one by hand.
    It never routes the ops port, which stays reachable only by the kubelet and the metrics scraper.
  - A prometheus-operator `PodMonitor`, off by default, for the ops port.
    It selects Pods and names the container port, because the ops port is absent from the Service by design.
  - A prometheus-operator `PrometheusRule`, off by default,
    over the metrics section's readiness, admission, and signing-key gauges,
    replaceable outright by a rule set the operator supplies.

  `tls.enabled` gates the certificate volume, its mount, and the `server.tls` keys in the rendered configuration.
  The chart needs the flag because Helm needs a boolean to render a conditional;
  the configuration file stays presence-implied,
  the same way `pgo.enabled` gates the credentials volume while `nats.credsFile` is presence-implied.
  The Secret is the operator's to create, from cert-manager or by hand, and the chart never creates it.
  The volume is not optional, unlike the credentials one:
  `tls.enabled` asserts that the certificate exists,
  so a missing Secret holds the Pod at mount time with an event naming it,
  rather than starting a Pod that exits over a file it cannot open.
  There is deliberately no `checksum/tls-secret` annotation.
  Adding one for symmetry with `checksum/config` would roll the Deployment on every renewal
  and defeat the re-read the gateway does for exactly that reason.
- The console of [`ui.md`](ui.md) ships inside the binary:
  the vendored browser files under `internal/ui/static/vendor/` are embedded by `go:embed`
  and pinned by `internal/ui/static/vendor/MANIFEST`, so the image gains no layer and the page loads nothing at runtime.
  The chart gains a top-level `ui.enabled` value, default `false`, rendered into the ConfigMap,
  and its raw `config:` block refuses `config.ui.enabled` and a scalar `config.ui`,
  with the same guard it applies to `pgo.enabled` and the `tls` and `auth` file paths.

### 11.1 Dependencies

| Module | Purpose |
|---|---|
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | discovery (only in `internal/k8s`); also the end-to-end harness |
| `github.com/arloliu/fuda` | configuration |
| `gopkg.in/yaml.v3` | strict unknown-key pass before fuda (already a fuda dependency) |
| `github.com/prometheus/client_golang` | metrics |
| `github.com/nats-io/nats.go` | PGO coordination and artifacts (only in `internal/natskv`) |
| `github.com/google/pprof` | profile merge ([`pgo.md`](pgo.md)) and tests: parsing fetched profiles |
| `github.com/go-jose/go-jose/v4` | JWS verification and JWK parsing (only in `internal/auth`; [`auth.md`](auth.md)) |
| `golang.org/x/crypto` | `bcrypt` (only in `internal/auth`) |
| `golang.org/x/term` | reading a password without echo for `profgate auth hash` (only in `cmd/profgate`) |
| `sigs.k8s.io/yaml` | tests only: golden ClusterRole and `versions.yaml` |
| `github.com/dop251/goja` | tests only: evaluating the console's port-control model ([`ui.md`](ui.md) *What is not proven*) |

Everything else is the standard library.
The console adds no Go module to the binary and one to the tests, the interpreter above;
the browser code it vendors is listed in [`ui.md`](ui.md) *Dependencies*.

---

## 12. Package Layout

```text
cmd/profgate/        CLI: serve, config validate, version
internal/k8s/        the seam; sole non-test importer of client-go
internal/proxy/      upstream HTTP to PodIP:Port, transport, budget, error mapping
internal/httpapi/    routing, realm checks, handlers, error bodies, audit log
internal/config/     fuda-loaded Config, strict pre-parse, validation, hot/restart classification
internal/metrics/    Recorder interface and the Prometheus implementation
internal/tlscert/    the API listener's certificate: load, re-read on a ticker, GetCertificate
internal/admit/      the admission gate shared by interactive requests and Collections
internal/auth/       Authenticator; basic, oidc, and disabled modes; JWKS cache; browser flow
internal/ui/         the console: embedded page and vendored browser libraries; sole user of go:embed
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
| TLS certificate rotated in place | the new pair is served within the refresh interval; no restart, no dropped connection |
| TLS files unreadable or mismatched while running | the previous pair stays in use; a warning is logged and a `failed` reload counted |
| Issuer unreachable at startup (`oidc`) | discovery retries for `auth.oidc.discoveryTimeout`, then the process exits |
| JWKS refresh fails while running | the previous keys stay in use; a warning is logged and a `failed` refresh counted; `503 auth_unavailable` after `jwksMaxStale` |
| Issuer rotates its signing keys | tokens under the new key verify within `jwksRefreshMin` of the first one arriving |
| Issuer token endpoint down | browser logins answer `503 auth_unavailable`; existing sessions and bearer tokens unaffected |
| Users file or cookie key file unreadable while running | the previous contents stay in use; a warning is logged and a `failed` reload counted |
| Client names a `portName` on the targets endpoint | the list holds only admitted Pods declaring that TCP port name; the client learns which do (section 7.5) |
| Client probes numeric ports with `pod=` on the profile endpoint | each admitted port answers with the proxy outcome of section 6.4; the client can tell open from refused, silent, redirecting, or non-pprof HTTP ports of admitted Pods, within the realm, `allowedSelections`, and NetworkPolicy |
| Client names a port `allowedSelections` does not admit | `400 port_not_allowed` before discovery; the client learns the value is not admitted and nothing about Pods |
| Client names any value beyond the configured default under an empty `allowedSelections` | `400 port_not_allowed`; a fresh install grants no port selection at all |
| Client denied by realm names a refused port | `403 realm_denied`; `allowedSelections` is never evaluated |
| Client names an admitted numeric port that is not a pprof listener | the dial or the upstream response decides: `502`/`504` from section 6.4, or the upstream's own status passed through as `upstream_<status>` |
| Client names a `portName` some or all Pods lack | Pods without it are ineligible; the targets list shrinks to those that declare it, and a profile request with none left is `503 no_targets` |
| Service cache read fails on a listing route | `503 discovery_unavailable`; never an empty `200` ([`ui.md`](ui.md)) |
| Rolling update with two console asset hashes | each request a page makes may reach either build; an asset the answering replica lacks is `404 route_unknown` and the page does not render until the rollout converges, after which a reload recovers ([`ui.md`](ui.md)) |
| `ui.enabled` false | `/ui/` and `/` are `404 route_unknown`; the four listing routes still answer |

---

## 14. Amendments

Client-selected pprof ports —
the `port` and `portName` query parameters bounded by `discovery.pprof.allowedSelections` —
amend the following text.
The first table lists the edits made in the same change as this section;
the second lists the documents that describe shipped behavior and are updated when the implementation lands.

Amended now:

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *Core decisions* | decision 9 narrowed: a Pod IP, the number behind a `portName` selection, and realm-denied names stay hidden; what choosing ports reveals is listed under *Non-disclosure* |
| `docs/specs/gateway.md` | *Permission Boundary* | invariant sentence reworded: cluster-wide observation, realm-bounded service, and connections to the configured pprof port or any selection `allowedSelections` admits, exactly or by wildcard |
| `docs/specs/gateway.md` | *Network* | a wildcard entry accepts every port or every name a client names and leaves NetworkPolicy as the only bound on reachable Pod ports |
| `docs/specs/gateway.md` | *What a compromised gateway can do* | every port `allowedSelections` admits, and every port at all under `{port: "*"}` |
| `docs/specs/gateway.md` | *The seam* | `PortSelection` and the `Targets` parameter that carries it |
| `docs/specs/gateway.md` | *Cluster matrix* | scenarios 11 and 12 in the `degraded` skip list |
| `docs/specs/gateway.md` | *Eligibility* | rule 8 resolves from the request's port selection when given |
| `docs/specs/gateway.md` | *Port resolution* | motivation, the two parameters, `discovery.pprof.allowedSelections` and its entry shape, `400 port_not_allowed`, an empty list admitting only the configured default and refusing the other kind outright, the two wildcards and their same-kind redundancy error, the default always permitted and listable, shipped manifests ship the list empty |
| `docs/specs/gateway.md` | *Request algorithm* | parameter step validates the port and checks it against `allowedSelections` before discovery; discovery takes the selection |
| `docs/specs/gateway.md` | *List targets* | accepts `port` or `portName` |
| `docs/specs/gateway.md` | *Fetch a profile* | parameter table rows and rules for `port` and `portName` |
| `docs/specs/gateway.md` | *Proxy behavior* | the overall budget starts at the Admit step, cited by name |
| `docs/specs/gateway.md` | *Errors* | `400 port_not_allowed`, naming only the client's value and revealing whether `allowedSelections` admits it |
| `docs/specs/gateway.md` | *Limits are not authorization* | `allowedSelections` is global, not per realm |
| `docs/specs/gateway.md` | *Non-disclosure* | the guarantee narrowed to no Pod IP, no number behind a `portName`, no port in `X-Pprof-Target-*`; the observations a client can make by choosing ports |
| `docs/specs/gateway.md` | *Logging* | audit field `port`: the selection as sent, empty when absent or invalid, the client value with `port_not_allowed` |
| `docs/specs/gateway.md` | *Metrics* | no label for the port selection |
| `docs/specs/gateway.md` | *Layers* | unit rows per parameter for grammar, `allowedSelections` empty, listed, and wildcarded, realm before `allowedSelections`, per-Pod `portName` eligibility, the audit field per input, no discovery on refusal, a non-pprof listener through the proxy, config validation including the removed keys |
| `docs/specs/gateway.md` | *Harness* | the test app's second listener `:6061` named `pprof-alt`, the per-listener `/hits` shape, the default gateway listing both in `allowedSelections`, and the `ports-gateway` overlay leaving the list empty |
| `docs/specs/gateway.md` | *What end-to-end proves* | scenario 11: second port reached through `?port=` and `?portName=` on the default gateway; scenario 12: both refused by the empty-list `ports-gateway` overlay, whose configured default still fetches |
| `docs/specs/gateway.md` | *Configuration* | the `allowedSelections` row and its environment token grammar; `allowedPorts` and `allowedPortNames` removed and refused by validation; the migration to the two wildcards; the example ships the list empty |
| `docs/specs/gateway.md` | *Build and Deployment* | kustomize ConfigMap and chart `values.yaml` ship `allowedSelections` empty |
| `docs/specs/gateway.md` | *Failure Scenarios* | rows for `portName` target-set inference, numeric probing through proxy outcomes, membership disclosure, an empty list refusing everything but the configured default, realm denial before `allowedSelections`, an admitted port that is not a pprof listener, and a name some or all Pods lack |
| `docs/specs/pgo.md` | *Core decisions*, *On-demand Collections*, *Rounds*, *Unit* | every `Targets` call passes the zero `PortSelection`: PGO always uses the configured default and offers no client selection; a unit test records the zero value |
| `.agents/rules/800-security-invariant.md` | *The Boundary* | invariant sentence in the wording above |
| `AGENTS.md` | *The Permission Invariant* | invariant sentence in the wording above |
| `README.md` | opening paragraph and *The one requirement on the application* | invariant sentence in the wording above; the application serves pprof on the configured default and on whatever `allowedSelections` lists |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/api.md` | the `port` and `portName` parameters, `400 port_not_allowed`, the `/v1/limits` shape, and what choosing ports reveals |
| `docs/configuration.md` | `allowedSelections`, the empty list admitting only the configured default, the two wildcards, and the removal of `allowedPorts` and `allowedPortNames` |
| `docs/deployment.md` | the invariant sentence in the wording above, the pprof-port prose, and the NetworkPolicy sentence |
| `deploy/base/configmap.yaml`, `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md` | `allowedSelections` shipped empty |

The console ([`ui.md`](ui.md)) — four listing routes, a static page under `/ui/`, and `Catalog` on the seam —
amends the following text.
The first table lists the edits made in the same change as this block;
the second lists the documents updated when the implementation lands.

Amended now:

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *HTTP API* | the `/auth/` exception also names the `/ui/` and `/` routes when `ui.enabled`; the four listing routes are `/v1` routes defined in [`ui.md`](ui.md) |
| `docs/specs/gateway.md` | *Request algorithm* | the four listing routes run the route, method, readiness, credential-placement, and authentication steps as written, the realm step refuses only the Service list, and then they read the cache with no discovery, admission, confirmation, or proxy step; `GET` only |
| `docs/specs/gateway.md` | *List targets* | the *Listing endpoints* subsection pointing to [`ui.md`](ui.md) *Response shapes* |
| `docs/specs/gateway.md` | *Errors* | `503 discovery_unavailable` also covers a cache read that fails on a listing route; `405` under `/ui/` and on `/` carries `Allow: GET, HEAD` |
| `docs/specs/gateway.md` | *The seam* | `ServiceRef` and `Catalog`, reading the Service cache and issuing no request |
| `docs/specs/gateway.md` | *Non-disclosure* | a fourth listed observation: `/v1/limits` returns `allowedSelections` and the default to every request `auth.mode` admits, anonymous requests under `disabled` included |
| `docs/specs/gateway.md` | *Logging* | the listing routes write the record with `namespace` on the Service list only and the other target fields empty; requests under `/ui/` and to `/` write no record |
| `docs/specs/gateway.md` | *Metrics* | `endpoint` gains `namespaces`, `services`, `whoami`, `limits`, `ui`; the `ui` codes `ok`, `route_unknown`, `method_not_allowed`, `internal_error` |
| `docs/specs/gateway.md` | *Layers* | unit bullets for `internal/ui`, the listing routes, `Catalog`, and the `ui.enabled` validation |
| `docs/specs/gateway.md` | *What end-to-end proves* | scenarios 14 and 15: the console proofs run inside the two authentication scenarios |
| `docs/specs/gateway.md` | *Configuration* | the `ui.enabled` row |
| `docs/specs/gateway.md` | *Build and Deployment* | the vendored browser files embedded by `go:embed` and pinned by `MANIFEST`; the chart's `ui.enabled` value and raw-block guard |
| `docs/specs/gateway.md` | *Dependencies* | no Go module for the console; the vendored browser code is listed in [`ui.md`](ui.md) *Dependencies* |
| `docs/specs/gateway.md` | *Package Layout* | `internal/ui/` |
| `docs/specs/gateway.md` | *Failure Scenarios* | rows for a cache read failing on a listing route, a rolling update with two asset hashes, and `ui.enabled` false |
| `docs/specs/auth.md` | *Non-goals*, *The `/auth/` routes*, *What is redirected*, *Testing* | the UI non-goal points to [`ui.md`](ui.md); logout's fallback `302` to `/` lands on `/ui/` when `ui.enabled`; the `fetch` sentence names the console; the two end-to-end lanes gain the console steps |
| `.agents/rules/100-project-map.md` | *Planned Structure*, *External HTTP API* | `internal/ui/`; the four listing routes and the three console routes |
| `docs/README.md` | *Where Contributors Start* | `specs/auth.md` and `specs/ui.md` beside the PGO spec |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/api.md` | the four listing endpoints |
| `docs/configuration.md` | `ui.enabled` |
| `docs/deployment.md` | the Ingress paths `/ui/`, `/auth/`, and `/` |
| `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md` | the `ui.enabled` value and the raw-block guard |

Tightening the client port selection model above — what the permission invariant claims about reach,
the environment grammar, and the environment variables the change removed —
amends the following text.

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *Permission Boundary* | invariant sentence: observation is cluster-wide and a realm bounds what each caller reaches; connections go to the configured pprof port or to any port or name `allowedSelections` admits, exactly or by wildcard, wherever NetworkPolicy permits |
| `docs/specs/gateway.md` | *Network* | a client may name the configured default as well as a selection `allowedSelections` admits |
| `docs/specs/gateway.md` | *Non-disclosure* | `/v1/limits` is read by every caller the configured `auth.mode` admits, which under `disabled` is every caller |
| `docs/specs/gateway.md` | *Layers* | a named default with only numeric entries and a numeric default with only named entries; the three environment cases and the empty token; the removed environment variables asserted on the message; the console's port-control model test |
| `docs/specs/gateway.md` | *Configuration* | `PROFGATE_PPROF_ALLOWED_SELECTIONS` absent, present and empty, and present and non-empty, with an empty token a validation error; `PROFGATE_PPROF_ALLOWED_PORTS` and `PROFGATE_PPROF_ALLOWED_PORT_NAMES` refused by name; the table converting each old list on its own |
| `docs/specs/gateway.md` | *Dependencies* | the ECMAScript interpreter that evaluates the console's port-control model, tests only |
| `docs/specs/ui.md` | *Core decisions*, *Request algorithm for the listing endpoints*, *Limits*, *Controls*, *Unit*, *What is not proven* | which listing endpoints a realm bounds; the control a value came from decides its parameter; a menu entry equal to the default is suppressed; `portmodel.js` and the test that evaluates it |
| `.agents/rules/800-security-invariant.md`, `AGENTS.md`, `README.md` | *The Boundary*, *The Permission Invariant*, opening paragraph | the invariant sentence in the wording above; `README.md` adds that the gateway itself uses no NATS store |
| `README.md` | *The one requirement on the application* | the application serves pprof on the configured default and on whatever `allowedSelections` admits, by an exact entry or by a wildcard |

Updated with the implementation: `docs/deployment.md`,
whose invariant paragraph and pprof-port prose still describe the two fail-open lists that ship today.
