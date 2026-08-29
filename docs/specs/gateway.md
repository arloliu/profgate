# Profgate Gateway

**Status:** Accepted

This document is the design of record for the gateway:
a Kubernetes-aware pprof proxy with no PGO, no NATS, and no durable state.
Profile-Guided Optimization collection is a separate, additive design in
[`pgo.md`](pgo.md); it builds on what is defined here and changes none of it.
The original draft that covered both, `docs/specs/profgate-design.md`,
is superseded by this document and recoverable from git history.

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

// Exclusion counts the Pods one reason kept out of a Service's target list.
type Exclusion struct {
    Reason string // one of the eligibility reason names
    Count  int
}

// Explanation is what Explain reports about one Service.
type Explanation struct {
    Targets         []Target    // what Targets returns for the same arguments
    SelectorMatched int         // Pods in the namespace whose labels match spec.selector
    Excluded        []Exclusion // the reasons with a non-zero count, in vocabulary order
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
    // Explain returns the targets of a Service beside the reasons its other selected Pods were dropped,
    // from one captured list of the namespace's selected Pods and the EndpointSlice pass Targets makes.
    // It counts Pods and names none.
    Explain(ctx context.Context, namespace, service string, port PortSelection) (Explanation, error)
}
```

`Catalog` serves the listing routes of [`ui.md`](ui.md).
It reads the Service informer cache and issues no request,
so the RBAC table of section 3.1 does not change for it.

`Explain` serves `explain=true` on the targets endpoint (*List targets*)
and returns `ErrServiceNotFound` and `ErrServiceSelectorless` where `Targets` returns them.
It reads the Pod, Service, and EndpointSlice informer caches and issues no request,
so the seven RBAC tuples stay seven, the golden ClusterRole test stays green,
and the recording transport of *Layers* sees nothing new.
What it costs is one namespace-wide read of the Pod cache that `Targets` does not pay,
bounded by the number of Pods in the namespace;
`internal/httpapi` cannot do that read instead, because reading a lister means importing client-go,
which the import check keeps out of every package but this one.
That read happens once.
`Explain` lists the namespace's Pods under the Service's selector and indexes them by name,
then resolves every endpoint against that one captured map rather than reading the Pod cache again,
so the population it counts and the Pods it resolves endpoints against are the same snapshot.
A failure of that read is an error and never a zero count:
`Explain` wraps it and returns it, and the HTTP layer answers `503 discovery_unavailable`,
the answer any discovery error that is neither sentinel already gets.
It returns the target list beside the counts because both come from one pass:
a caller that asked for the list and the counts separately could read a cache that moved between the two,
and a Pod would then be counted twice or not at all.
The interface is one method longer, which is the visible cost
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) asks a new capability to carry.

Sentinel errors, matched with `errors.Is`:

| Error | Meaning | HTTP mapping |
|---|---|---|
| `ErrServiceNotFound` | no Service with that name in the namespace | `404 service_not_found` |
| `ErrServiceSelectorless` | Service has no selector, so backend membership cannot be verified | `422 service_selectorless` |
| `ErrTargetChanged` | `Confirm` found the Pod gone, replaced, not ready, terminating, or at a different address | `503 target_changed` |
| `ErrDiscoveryUnavailable` | `Confirm` could not reach the API server within its timeout | `503 discovery_unavailable` |

There is no `GetPod` method.
A `?pod=` request is answered from the `Targets` result,
so a Pod that is not a backend of the Service is disposed of without an additional API call,
and the interface stays one method narrower.
On the profile endpoint that disposal is a rejection, `404 pod_not_found`;
on the targets endpoint, which reports rather than selects, it is an empty list (*List targets*).

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

**Why a Pod is not a target.**
`explain=true` on the targets endpoint (*List targets*) counts the Pods the rules above dropped.

The counted population is the Pods of the Service's namespace whose labels match `spec.selector`,
read from the Pod cache in one list.
A slice entry naming a Pod the selector does not match (rule 4) is counted nowhere:
it is not a Pod of the Service.

A **trusted endpoint** of such a Pod is an endpoint that carries that Pod's identity:
it sits in an EndpointSlice of the Service whose `addressType` is the family the Service is read under,
its `targetRef` is `kind: Pod` in the Service's namespace,
and its `targetRef.name` and `targetRef.uid` are the Pod's name and `metadata.uid`.
Rules 2 and 3 are that definition.
An entry naming another kind, another namespace, a name no selected Pod carries,
or a UID a recreated Pod no longer holds is nobody's trusted endpoint:
the gateway will not follow it, and it says nothing about the Pod it names.
A trusted endpoint is **eligible** when rules 5, 7, and 8 also hold for it and its Pod.

Attribution runs per Pod, over that Pod's trusted endpoints as a group:

- A Pod with at least one eligible trusted endpoint is a target,
  unless two of its trusted endpoints that each satisfy rules 5 and 7 name different addresses it holds.
  That is the conflict the deduplication above excludes, and it is counted as `endpoint_address_conflict`.
  The conflict is decided over the Pod's trusted endpoints satisfying rules 5 and 7, not over its eligible ones,
  so a Pod whose slices disagree about its address carries that reason whether or not a pprof port resolves for it.
  This changes no target list — a portless Pod and a conflicted Pod are both excluded either way —
  and decides only which reason a Pod that is both conflicted and portless carries.
- Every other counted Pod is attributed to the first reason in this table that holds for it.
  A reason describing an endpoint holds when at least one of the Pod's trusted endpoints satisfies it;
  a reason describing the Pod holds from the Pod's own object.

| Reason | A Pod the Service selects, and | Rule |
|---|---|---|
| `pod_terminating` | `metadata.deletionTimestamp` is set | 6 |
| `pod_not_running` | `status.phase` is not `Running` | 6 |
| `pod_not_ready` | the Pod's `Ready` condition is not `True` | 6 |
| `endpoint_missing` | it has no trusted endpoint at all | 2, 3 |
| `endpoint_not_ready` | a trusted endpoint of it carries `conditions.ready: false` | 5 |
| `endpoint_address_mismatch` | a trusted endpoint of it carries no address, or a first address that is not one of the Pod's `status.podIPs` | 7 |
| `endpoint_address_conflict` | two trusted endpoints that each satisfy rules 5 and 7 name it with different addresses it holds, whether or not a pprof port resolves | the deduplication above |
| `port_name_not_declared` | the effective pprof port name matches no TCP container port of the Pod | 8 |
| `version_mismatch` | the request carried `version=` and the Pod's version label holds another value | the filter step of *Request algorithm* |
| `pod_name_mismatch` | the request carried `pod=` and the Pod has another name | the filter step of *Request algorithm* |

Every counted Pod is either a target or attributed to exactly one reason,
so the number of targets plus the counts add up to the number of selected Pods.
The table's order attributes; it decides no eligibility,
which stays the rules above evaluated as they are written.
Nothing in it depends on the order the caches hand things over:
reversing the slices of a Service, the endpoints within a slice, or the Pod list changes no attribution,
and two replicas holding identical cached objects answer identically.
Two replicas holding different snapshots may differ while the newer one is still arriving,
which is what a cache-derived answer means everywhere else in this document.

The table reads Pod state before endpoint state
because a terminating or unready Pod explains its own endpoint,
while the endpoint explains nothing about the Pod.
`endpoint_address_conflict` has a population of its own and needs no tie broken:
an eligible endpoint requires a running, ready, not-terminating Pod,
so a conflicted Pod satisfies none of the three Pod-state reasons,
and a conflicted Pod with an eligible endpoint is reported as the conflict
even when another of its endpoints is unready or mismatched.

`endpoint_missing` covers a Pod no slice names,
a Pod named by a stale UID (rule 3),
and a Pod named by an entry whose kind or namespace is wrong.
One reason covers all of them because the answer is the same:
no endpoint the gateway trusts names this Pod.
In the last two cases the entry exists and is untrusted, which is what the count says.

`endpoint_address_conflict` needs one Pod holding more than one address of the family the Service is read under,
because two entries that both pass rule 7 must both name an address the Pod's `status.podIPs` lists.
Ordinary dual-stack operation does not produce it:
a Service with any IPv4 slice is read as IPv4 and its IPv6 slices are never read,
so entries of the two families never meet.
The reason is defensive.
It covers a Pod whose networking gives it several addresses of one family,
and slices that disagree about which of them is current,
and the gateway excludes such a Pod rather than guess which address is its.

The last two rows are the filter step of *Request algorithm* rather than eligibility.
`Explain` never produces them: it is passed no `version` and no `pod`.
`internal/httpapi` applies those two filters to the targets `Explain` returned
and moves each target they drop into the matching count,
so a Pod stays attributed once and the sum still holds.
A request carrying neither parameter can never reach either row.

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
`GET /v1/openapi.json` describes every route to a machine (*The OpenAPI document*).
`GET /v1/auth` reports the authentication mode to a caller with no credential,
and the values a command-line login needs where one is configured;
it is a `/v1` route with no authentication step, defined in [`cli.md`](cli.md) *Gateway discovery*.
The product name does not appear in any path.
Every response on both listeners carries `X-Request-Id` (*Request identifier*).
Cache policy is per surface rather than one rule:
the `/v1` routes, the console shell, the `/auth/` routes of [`auth.md`](auth.md),
and the three ops paths answer `Cache-Control: no-store`,
while the console's asset routes answer `Cache-Control: no-cache` with a per-file `ETag`
([`ui.md`](ui.md) *Headers*), which is what lets a browser revalidate a file instead of refetching it.

### 6.1 Request algorithm

Before step 1 the request is given its identifier (*Request identifier*),
which every response the steps below produce carries.
Every `/v1` request then passes through these steps in order;
the first failing step produces the response.
Steps 8–12 differ by endpoint.

1. **Route.** Unknown path → `404 route_unknown`; unknown `{profile}` → `404 profile_unknown`.
   Path segments for `{namespace}` and `{service}` must be DNS-1123 labels, otherwise `404 route_unknown`.
2. **Method.** A method the route does not accept → `405 method_not_allowed` with `Allow` listing those it does;
   the three routes defined here accept `GET` only.
3. **Readiness.** `HasSynced()` false → `503 not_ready`.
4. **Credential placement.**
   `access_token` as a query parameter → `400 invalid_parameter`.
5. **Authentication.**
   Resolve the principal and its realm per `auth.mode`
   → `401 unauthenticated`, `429 too_many_auth`, `503 auth_unavailable`, or a `302` to login ([`auth.md`](auth.md)).
6. **Realm.** Namespace, then Service, then (profile endpoint only) profile → `403 realm_denied`.
7. **Parameters.**
   Targets endpoint: `port` or `portName`, `version`, `pod`, and `explain`, validated per *List targets*;
   any other name → `400 invalid_parameter`.
   Profile endpoint: validate every parameter per section 6.3 → `400 invalid_parameter` or `400 seconds_exceeds_limit`.
   Both endpoints: `port` or `portName` outside its grammar, or both given → `400 invalid_parameter`;
   a value `discovery.pprof.allowedSelections` does not admit → `400 port_not_allowed` (section 5.4).
   A refused port never reaches discovery.
8. **Discovery.** `Targets()` with the request's port selection,
   or `Explain()` when the targets endpoint was sent `explain=true` (*The seam*)
   → `404 service_not_found`, `422 service_selectorless`, `503 discovery_unavailable` for a cache read that fails.
9. **Filter and select.**
   Targets endpoint: apply `version`, then `pod`, and respond `200` with the list that remains,
   sorted (*List targets*);
   a `pod` no eligible target carries leaves an empty array, never `404 pod_not_found`.
   Profile endpoint: apply `version`;
   if `pod` is present and no remaining target has that name → `404 pod_not_found`;
   if `pod` is absent and no target remains → `503 no_targets`;
   otherwise pick `pod`, or one target by `strategy` (`random` when absent).
10. **Admit** (profile endpoint only).
   Acquire one of `limits.maxConcurrentProfiles` slots from the admission gate (`internal/admit`) without waiting;
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
They accept `GET` only, like the three routes defined here.

A route may also require the request to declare a media type.
No route defined here does;
the two PGO `POST` routes do, in a step [`pgo.md`](pgo.md) adds immediately after the method step,
so a request another origin could have produced is refused before anything else runs —
before readiness, before PGO availability, before a credential is read, and before any store call.
That document also adds the PGO availability step between readiness and credential placement.
All four accepted designs therefore state one order:
route, method, JSON media type, readiness, PGO availability, credential placement, authentication, realm.
The media type is parsed with `mime.ParseMediaType`, must have an essence of `application/json`,
and every parameter that parse returns is accepted and ignored.

`GET /v1/openapi.json` runs the route, method, and readiness steps,
and the parameter step in the form that refuses every query parameter,
and then answers.
It has no credential-placement, authentication, or realm step,
because it describes the route grammar and names nothing a realm bounds (*The OpenAPI document*).

`GET /v1/auth` runs those same four steps and then answers.
It too has no credential-placement, authentication, or realm step:
it is the route a client reads before it holds a credential,
so requiring one would leave it answering only callers who no longer need it,
and it names nothing a realm bounds ([`cli.md`](cli.md) *Gateway discovery*).

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

The endpoint takes these parameters and no others:

| Parameter | Grammar | Meaning |
|---|---|---|
| `port` | as in *Fetch a profile* | eligibility resolves the pprof port under it, in place of the configured default |
| `portName` | as in *Fetch a profile* | the same, by container-port name; excludes `port` |
| `version` | non-empty string | keep only the targets carrying this version |
| `pod` | DNS-1123 subdomain | keep only the target of this name |
| `explain` | `true` or `false` | `true` adds the exclusion counts below; `false` is accepted and adds nothing |

`version` and `pod` mean what they mean on the profile endpoint
and are applied in that order at the filter step of *Request algorithm*,
with one difference:
this endpoint reports where the profile endpoint selects,
so a `pod` no eligible target carries is `200` with an empty array rather than `404 pod_not_found`.
An endpoint that answered `404` here would make "that Pod is not a target" indistinguishable from a typo,
which is the confusion `explain` exists to end.
Neither filter is a new audit field.
`pod` in the audit record keeps its meaning — the upstream Pod a profile request selected —
and a targets request writes it empty however its query filtered;
`version` has never been a field of that record (*Logging*).
The parameter set is a fixed query grammar rather than operator configuration,
so `/v1/limits` neither reports it nor changes.
Parameters are validated in name order, so a query with several faults reports the same one every time.
A parameter given more than once, one with an empty value, one outside its grammar,
and a name the table does not hold are each `400 invalid_parameter`;
`explain` with any value but `true` or `false` is that answer too.
`port` and `portName` together are `400 invalid_parameter` as they are on the profile endpoint,
and either one `discovery.pprof.allowedSelections` does not admit is `400 port_not_allowed`, before discovery.

**Exclusion counts.**
With `explain=true` the response keeps `targets` exactly as it is above and adds two fields:

```json
{
  "namespace": "payment",
  "service": "payment-api",
  "targets": [
    {"pod": "payment-api-7c8f8c9b9-xabcd", "node": "worker-07", "version": "1.42.3"}
  ],
  "selectorMatched": 6,
  "excluded": [
    {"reason": "pod_not_ready", "count": 2},
    {"reason": "endpoint_missing", "count": 1},
    {"reason": "port_name_not_declared", "count": 2}
  ]
}
```

`selectorMatched` is the number of Pods in the namespace whose labels match the Service's `spec.selector`,
counted before eligibility runs.
`0` is how the response says the selector matches no Pod, which no reason describes and no count could.

`excluded` holds one entry per reason with a non-zero count, in the vocabulary order of *Eligibility*,
and is `[]` — never `null` — when every selected Pod is a target.
`reason` is one of that section's names and nothing else:
a closed set the gateway writes from its own vocabulary, never a value the client sent and never a Pod's own text.
`count` is a number.
The two rows a request produces by filtering, `version_mismatch` and `pod_name_mismatch`,
are added here rather than by the seam:
`internal/httpapi` applies `version` and then `pod` to the targets `Explain` returned
and counts each target it drops under the matching reason,
in the same vocabulary order as the rest.

`selectorMatched` always equals the length of `targets` plus the sum of the counts,
because every selected Pod is a target or carries exactly one reason.
A client can rely on that; `internal/httpapi` asserts it in the unit cases of *Layers*.

There is no field for a Service that was found or a cache that has synced.
A body proves both by existing:
a Service the cache does not hold is `404 service_not_found` at the discovery step,
a Service without a selector is `422 service_selectorless` there too,
and caches that have not synced are `503 not_ready` at the readiness step.
A `cacheSynced: true` would also invite the reading it cannot support —
that the cache is current — when initial sync is all it could ever report,
and a later API outage leaves a synced cache serving data as old as the outage (*Informers*).

`explain` changes no status code, no other field, and no eligibility decision;
it reports what the same request without it already did.
The gateway pays for it with the Pod-cache read of *The seam*, which a request without it does not make.
The response names no Pod, address, node, or version beyond what `targets` already lists:
what *Non-disclosure* says about the plain listing holds for the counts,
and the counts are what its fifth observation records.

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

**Structured details.**
An error body may carry a third field, `details`, naming the inputs the caller has to change:

```json
{
  "error": "port 7000 is not allowed",
  "code": "port_not_allowed",
  "details": [{"field": "port", "code": "not_admitted", "message": "7000 is not an admitted selection"}]
}
```

Each item has three fields:

- `field` names the input at fault:
  a JSON-pointer-like path into the request body (`/schedule/every`),
  a query parameter name (`seconds`),
  a header name (`Idempotency-Key`),
  or empty when no single input is at fault.
  The item's `code` says which of the four it is, so a client never has to guess from the string.
- `code` is a closed vocabulary, one per error code, and is the stable contract the way the envelope's own `code` is.
- `message` is free text and may change, exactly as `error` may.

A `field` is empty only where the item's `code` says it can be:
`body_not_allowed` and `body_malformed` name no input,
and `malformed_parameter` names none when the raw query string is what failed to parse.
`header_malformed` covers every header the request actually carried and the route refuses,
whether that header was required or optional;
`header_required` covers only a header the route requires and the request omitted.
`Idempotency-Key` ([`pgo.md`](pgo.md) *Create a Collection*) is the optional case:
it earns `header_malformed` when it is present and wrong, and no item at all when it is absent.

`details` is omitted entirely — never `null`, never `[]` — when an error has none,
so its presence is the claim that the gateway attributed the failure to named inputs.
That is the opposite of `excluded` in *List targets*, which is `[]` when nothing was excluded,
because there an empty array is an answer and here an empty array would be a promise the gateway did not keep.
An error code no vocabulary below covers carries no `details` at all;
giving one a vocabulary, or adding a value to a vocabulary, is a change to this document.
Every vocabulary is an enumeration in the OpenAPI document,
so the check of *The OpenAPI document* holds the sets closed rather than prose alone.
Items appear in the order the parameters are validated, which is name order (*List targets*).

`invalid_parameter`:

| `code` | `field` | Raised by |
|---|---|---|
| `unknown_parameter` | the query parameter | a name the route does not take |
| `repeated_parameter` | the query parameter | a parameter given more than once |
| `empty_parameter` | the query parameter | a parameter present with an empty value |
| `malformed_parameter` | the query parameter, or empty | a value outside the parameter's grammar; empty when the raw query string does not parse and no one name is at fault |
| `parameter_not_applicable` | the query parameter | a parameter the route takes but not for this request, such as `seconds` on `heap` |
| `mutually_exclusive` | each of the two query parameters, one item apiece in name order | `port` with `portName` |
| `header_required` | the header | a header the route requires and the request omitted |
| `header_malformed` | the header | a header the request carried and the route does not accept: repeated, unparseable, carrying a parameter or a value the route refuses |
| `unknown_field` | a pointer into the body | a body field the route does not accept |
| `field_not_applicable` | a pointer into the body | a body field the route accepts elsewhere but not here |
| `body_not_allowed` | empty | a body sent to a route that accepts none |
| `body_malformed` | empty | a body that is not JSON, or over the route's size limit |

`port_not_allowed` has one item and one value:
`code` is `not_admitted` and `field` is `port` or `portName`, whichever the client sent.
The item names the parameter and the message names the value the client sent,
which is what *Non-disclosure* already allows and no more.

[`pgo.md`](pgo.md) *Ceilings* defines the vocabulary of `limit_exceeded` on the same rules.

`pod_not_found` covers a Pod that does not exist,
one that is not an eligible backend,
and one filtered out by `version`.

`port_not_allowed` names only the value the client sent, never a port a Pod exposes;
receiving it does tell the client that `discovery.pprof.allowedSelections` does not admit the value (section 7.5).

`discovery_unavailable` also covers a Service cache read that fails on a listing route of [`ui.md`](ui.md);
such a route never answers an empty `200` in its place.
`405 method_not_allowed` under `/ui/` and on `/` carries `Allow: GET, HEAD`,
because those routes serve files and accept `HEAD`.

### 6.6 Request identifier

Every response the gateway writes carries `X-Request-Id`,
so a client, an operator reading the audit log, and a bug report name one request the same way.

The value comes from the client when the request carries exactly one `X-Request-Id` of 1 to 128 bytes,
drawn from `[A-Za-z0-9._-]`.
Every other request — one that omits the header, sends it empty, sends it twice,
sends more than 128 bytes, or sends a byte outside that set — is given a generated one:
16 bytes from `crypto/rand` as 32 lowercase hexadecimal characters.

**A value the gateway will not take is replaced, never refused.**
The identifier decides nothing:
no step of *Request algorithm* reads it, no realm is evaluated against it, and no cache is keyed by it.
Refusing a request over it would turn a diagnostic convenience into a failure mode,
and a client whose identifier the gateway declined would lose the answer it came for.

The byte set is what makes echoing client text safe.
It excludes `CR`, `LF`, and every character that could split or forge a header,
and the 128-byte bound caps what one request can reflect.
The value is echoed into that one response header and written to that one audit record and nowhere else.

The header is set on every response on both listeners:
the three routes defined here, the four listing routes and the console routes of [`ui.md`](ui.md)
(their `304` and `405` answers included),
the `/auth/` routes of [`auth.md`](auth.md) and the `302`s they write,
every gateway error envelope,
and `/healthz`, `/readyz`, and `/metrics` on the ops listener (*Health*).
A forwarded upstream response carries it too:
it is a gateway-owned header, set the way `Cache-Control` is and overwriting whatever the upstream sent.

The identifier does not travel upstream.
The upstream request carries no headers from the client (*Proxy behavior*),
and a proxied fetch is the gateway's own request to an application, not the client's;
correlation stops at the gateway's answer and its audit line.

Every audit record carries it as `requestId` (*Logging*),
and a request that writes a record writes exactly one.
Not every response has a record behind it:
requests under `/ui/` and to `/`, and every request on the ops listener,
carry the header and write none (*Logging*, *Health*).
A diagnostic line a handler writes beside a record may name the identifier,
which is what having one identifier is for.
It is not a metric label:
it is client-controlled and different on every request,
so a label would mint one series per request (*Metrics*).

### 6.7 The OpenAPI document

```http
GET /v1/openapi.json
```

`200` with `Content-Type: application/json` and an OpenAPI 3.1 document describing every route the API listener serves;
the ops listener's three paths are not in it, for the reason below.
It carries paths, methods, parameters and their grammars, request and response shapes,
the `X-Request-Id` header, the error envelope, and the `details` schema with every vocabulary of *Errors*.

**It is hand-maintained and served byte for byte.**
The document is `internal/httpapi/openapi.json`, embedded with `go:embed`;
the route answers with those bytes and transforms nothing,
so the file a reviewer reads in a diff is the file a client parses.
Nothing generates it, and no build step stands between the two.
What keeps it true is not authorship but the check below.

**No credential and no realm.**
The document names namespaces and Services as path templates and never as values from a cluster,
so a realm has nothing to bound and there is nothing for authentication to protect.
What it publishes is the route grammar.
`404 route_unknown` and the `Allow` header of a `405` already publish that grammar to an unauthenticated caller,
one request at a time.
It runs the readiness step like every other `/v1` route rather than earning an exception:
one fewer exception in this algorithm is worth more than an answer during startup.

**It does not vary with configuration.**
It describes the PGO routes whether or not `pgo.enabled` is set — they answer `501 pgo_disabled`, which it says —
and the console and `/auth/` routes whether or not those are configured.
A document assembled from the running configuration would be a second configuration surface,
and nothing could check it before the process started.
It carries `Cache-Control: no-store` like every other `/v1` response:
the bytes are in the binary and cost nothing to serve again.

**The route table.**
`internal/httpapi` holds one declaration per route the API listener serves:
its path template and the methods it accepts.
The package holds no such table today — its matching is three expressions and three exact paths —
and writing one is part of this change.

**Every API-listener route consumes one declaration**, with no dispatch beside it:
the two routes of *List targets* and *Fetch a profile*, this document route,
the four listing routes and the `/ui/` and `/` routes of [`ui.md`](ui.md),
the three `/auth/` routes of [`auth.md`](auth.md),
and the seven PGO routes of [`pgo.md`](pgo.md).
The console and `/auth/` routes are dispatched outside the `/v1` parser today,
which is exactly why a table that covered only `/v1` could prove nothing about "every route".
Three things read that one declaration and nothing else:
the router, which matches a request against it;
the `Allow` header of a `405`, which is the methods the matched declaration lists;
and the check below.
A route present when a declaration is absent is a route the router cannot reach,
so a declaration is not a list kept beside the code.

The ops listener is outside the table and outside the document.
`/healthz`, `/readyz`, and `/metrics` answer `text/plain`, carry no error envelope,
are reached only by the kubelet and the metrics scraper, and are never routed by an Ingress (*Health*);
"every route the binary serves" above means every route the API listener serves, and says so here.

**The error-code registry.**
`internal/httpapi` also holds one static registry of every code the gateway can write into an envelope:
a declared set of constants, one per code in *Errors* and in [`pgo.md`](pgo.md) *Errors*.
The registry is the comparison source, and it is what makes the codes checkable
where reading the source could not:
the package writes envelopes through one central `WriteError`, whose `code` argument is a value at that call site,
and the proxy and store transports map their own failures onto codes through their own tables.
A test that demanded a string literal at every call would fail on the central writer first.

Instead, every constructor of a gateway error and every transport mapping takes its code from the registry,
and each mapping is an exhaustive switch over its own closed input,
so a new failure mode does not compile until it names a registry constant.
That discipline is held by review, not by a test that reads the source:
what the check automates is the registry against the document, which is the comparison that can be mechanical.

**The check.**
A Go test in `internal/httpapi` compares the document with the code, never with this document's prose:

1. it walks the route table.
   Every path-and-method pair must appear in the document,
   and the document must declare no pair the table does not hold;
2. it compares the registry with the codes the document enumerates, and the two sets must be equal;
3. it requires every `details` vocabulary of *Errors* to appear as an enumeration in the document;
4. it re-encodes the parsed document and requires the file to equal that encoding,
   so a hand edit cannot leave the file formatted one way and read another;
5. it collects every `$ref` the document holds and requires each to resolve inside it,
   so a pointer at a component the document does not carry cannot ship.

What the check does not catch is a code a constructor names and no route can answer with,
which is a document that over-promises rather than one that lies.
The two version refusals of [`pgo.md`](pgo.md) *Create a Collection*,
which today build one error from a computed value, become two errors naming two registry constants.
Upstream statuses passed through under `upstream_<status>` are not in the document
and not in the registry:
they carry the application's body, not a gateway envelope.

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
the targets response, gateway error bodies and every `details` item inside one,
gateway-owned headers, and the transport-error envelopes
name only namespaces, Services, and Pods the realm admits and never a Pod address.
A `details` item names an input the request itself carried —
a query parameter, a header, or a path into its own body —
and never a value the gateway read from the cluster.
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
- `explain=true` on the targets endpoint reports how many Pods of an admitted Service are not targets, and why,
  which is fleet size inside a realm that already admits the Service.
  The plain listing names every eligible Pod of that Service with its node and version,
  so what the counts add is the size of the ineligible set —
  the Pods a caller who polled the endpoint across a rollout would watch become eligible one by one anyway,
  and, unlike that poll, a number rather than a name.
  What is genuinely new is the Service's Pod count when some of its Pods never become eligible:
  a workload whose replicas crash-loop reports them as a count where the listing reports nothing.
  That is accepted: the count is the size of a workload the realm already admits profiling,
  it names no Pod, no address, and no node,
  and a realm that should not disclose a Service's size is a realm that should not admit the Service.
  A caller the realm denies learns none of it: the realm step precedes discovery, `explain` included.
- `X-Request-Id` reflects up to 128 bytes of the client's own text back to that client,
  and writes them to that request's audit line (*Request identifier*).
  It reaches no other caller and names nothing of the cluster;
  the byte set is what keeps the reflection from forging a header.
- `/v1/openapi.json` answers every caller that can reach the API listener, with no credential
  (*The OpenAPI document*).
  It publishes paths, methods, parameter grammars, status codes, and the error vocabulary —
  the route grammar collected into one answer:
  `404 route_unknown` and the `Allow` header of a `405` already give the same answer one request at a time.
  It carries no namespace, Service, Pod, node, version, port, realm, or principal —
  path templates only, the same text this document holds.
- `/v1/auth` answers every caller that can reach the API listener, with no credential
  ([`cli.md`](cli.md) *Gateway discovery*).
  It publishes `auth.mode`, which the `WWW-Authenticate` header on every `401` already names,
  and, where `auth.oidc.cli` is configured, the issuer URL, the client identifier, the token type,
  the scopes, and whether the client must use PKCE.
  All five are values an OpenID Connect deployment publishes by design:
  the issuer serves its own discovery document to the world,
  and a public client identifier cannot by definition be kept secret.
  It carries no namespace, Service, Pod, node, version, port, realm, or principal,
  and an operator who configures no `auth.oidc.cli` block publishes the mode alone.

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
profgate collector --config <path>
profgate config validate --config <path>
profgate auth hash
profgate version
```

Standard-library `flag` with hand-written subcommand dispatch.

`serve` is the gateway this document describes.
`collect` runs the PGO collection loops of [`pgo.md`](pgo.md) and opens no API listener;
it reads the same configuration file, so `config validate` answers for both.

The same binary carries the client verbs of [`cli.md`](cli.md) *The verbs* —
`login`, `logout`, `whoami`, `limits`, `namespaces`, `services`, `targets`, `profile`,
`collections`, `collection get` and `cancel`, `download`, `pgo policy`, and `context` —
which talk to a gateway over HTTP rather than run one,
share this dispatcher and this flag idiom, and add no Kubernetes access.
The collector process is `collector`, a noun like `serve`,
so the client verb `collect` of [`cli.md`](cli.md) *The verbs* keeps its name under that document's *Reserved names* rule.

### 8.2 Logging

`log/slog`, JSON to stdout, at the level `server.logLevel` names.
Every `/v1` request emits one record on completion:

```text
requestId, principal, namespace, service, pod, profile, seconds, port, status, code, duration_ms
```

`auth_reason` is added on authentication failures and login redirects,
with the values [`auth.md`](auth.md) lists;
one of them, `internal`, marks an authenticator error the gateway could not classify, answered `503 auth_unavailable`.
The `/auth/` routes write a line with no namespace or Service ([`auth.md`](auth.md)).
The four listing routes of [`ui.md`](ui.md) write the record with `namespace` set on the Service list only
and `service`, `pod`, `profile`, `port`, and `seconds` empty;
requests under `/ui/` and to `/` write no record — they carry no principal and name nothing a realm bounds.
`/v1/auth` writes no record for the same reason ([`cli.md`](cli.md) *Gateway discovery*).

`port` is the client's port selection as sent, a number or a name, empty when absent;
for a numeric selection that is also the resolved port,
for a name it is the name and never the number it resolved to.
When the selection is malformed, repeated, or both parameters are present,
the field is empty and the request fails `invalid_parameter`;
a disallowed value is recorded as sent with `port_not_allowed`.
`explain` is added to the record of a targets request that carried `explain=true`, with the value `true`,
the way `auth_reason` is added rather than always present.
A request that omitted the parameter, sent `false`, or was refused for a value outside the grammar writes no `explain`.
The targets endpoint's `version` and `pod` filters add no field and overload none.
`pod` keeps the one meaning it has, the upstream Pod a profile request selected,
so a targets request writes it empty whatever it filtered on,
and `version` is a field of no record on either endpoint.
`code` is `ok` for a successful proxy, the gateway error code, or the upstream code from section 6.4.
`requestId` is the identifier of *Request identifier*, client-sent or generated,
and is on every record this section defines:
the interactive one above, the listing ones, the PGO one ([`pgo.md`](pgo.md) *Logging*), and the `/auth/` one.
It is the field a report from a client joins on, which is why it is first.
Requests under `/ui/` and to `/` still write no record, so a console request's identifier lives in its response alone.
This is the audit trail.
Records never contain a Pod IP.

### 8.3 Health

Both paths are on the ops listener and have no authentication or realm check.

| Path | `200` when |
|---|---|
| `/healthz` | the process is serving HTTP |
| `/readyz` | issuer discovery and the initial key fetch have succeeded when `auth.mode` is `oidc` ([`auth.md`](auth.md)), preflight has passed, and `HasSynced()` is true |

**Both answer `text/plain`, when they pass and when they fail.**
Their readers are the kubelet and a probe definition, which decide from the status line and read no body,
so a JSON `{status}` would be a second response shape to keep in step for a reader that never parses one.
The API listener's JSON envelope exists because clients act on `code`; nothing here does.
`X-Request-Id` is set on all three ops paths as it is on the API listener (*Request identifier*),
so a failed probe or scrape can be named in a report;
no path on this listener writes an audit record, so the header is the whole of what it buys.

`/readyz` reflects the initial sync of the informers as a whole.
It does not track API reachability afterwards:
a gateway that cannot reach the API server still answers the targets endpoint from its cache
and refuses to proxy (section 5.6), which is the correct behavior, not a reason to be removed from the Service.

### 8.4 Metrics

`/metrics` on the ops listener exposes Prometheus text format via `prometheus/client_golang`:

| Metric | Labels |
|---|---|
| `profgate_requests_total` (counter) | `endpoint` (`targets`/`profile`/`namespaces`/`services`/`whoami`/`limits`/`ui`/`openapi`/`auth`), `profile`, `code` |
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
`openapi` is the endpoint value of the document route, with `profile` fixed to `none`;
its `code` is `ok`, `not_ready`, `method_not_allowed`, or `invalid_parameter`,
which is every answer it has (*The OpenAPI document*).
It carries no `route_unknown`:
a path the table does not match fails before the request is routed,
so that answer is recorded under `profile` rather than here.
`auth` is the endpoint value of `/v1/auth`, with the same `profile` and those four codes beside `route_unknown`,
which is every answer it has ([`cli.md`](cli.md) *Gateway discovery*).
The client's port selection is not a label either;
it is client-controlled and would add a series per value.
The request identifier is not a label for the stronger form of the same reason:
it differs on every request, so a label would mint a series per request (*Request identifier*).
`explain` is not a label either:
it would double every `targets` series to record a parameter the audit line already carries,
and the label sets stay the closed ones above.
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
              the process exits once that wait has ended
```

Nothing about `pgo.enabled` lengthens this drain.
A gateway replica runs no Collection: the loops that do live in the collector process of [`pgo.md`](pgo.md),
which drains on a bound of its own and has its own grace period.
A replica's PGO routes are ordinary requests and finish inside the wait above.

One of them would otherwise sit idle inside it.
A `GET /v1/collections/{id}?wait=` holds its connection open for up to a minute waiting for a record to move
([`pgo.md`](pgo.md) *Get a Collection*),
which a deployment with `limits.cpuSeconds: 1` would find longer than the whole drain it sized.
The drain therefore ends every wait:
each waiting request answers at once with the record it last read,
at the moment `/readyz` turns 503 and before `server.drainDelay`.
The bound above is unchanged, and a client sees a well-formed answer to poll from rather than a reset.

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
The figure is the same whether `pgo.enabled` is true or false;
no PGO ceiling appears in it.
A second `SIGTERM` or `SIGINT` during the drain ends the process at once with a non-zero exit,
logging that it did not finish:
the drain's own waits are the ones the work legitimately needs,
so only the operator can say it has gone on long enough.

A listener that fails is fatal:
the process logs the failure, waits out the in-flight requests, and exits 1.
It skips `server.drainDelay`, because a listener that has failed receives nothing that window protects.

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
- The request identifier, in `internal/httpapi` and against the ops handler:
  a client value of one byte, of 128 bytes, and of every character the set holds is echoed unchanged;
  an absent header, an empty value, 129 bytes, a space, a colon, a `CR`, an `LF`, a non-ASCII byte,
  and two `X-Request-Id` headers on one request each yield a generated 32-character lowercase hexadecimal value,
  and two requests never receive the same generated one;
  the header is present on a `200`, on every error envelope, on a `405` beside `Allow`,
  on the console's `304` and on its file responses, on an `/auth/` `302`,
  and each of those responses carries the cache policy its surface names —
  `no-store` on a `/v1`, shell, `/auth/`, or ops answer,
  `no-cache` with an `ETag` on a console asset and on its `304`;
  on a forwarded upstream response, where an upstream's own `X-Request-Id` is overwritten and never reaches the client,
  and on `/healthz`, `/readyz`, and `/metrics`, whose bodies stay `text/plain` in both their outcomes;
  the audit record carries `requestId`, one record is written per request,
  a request under `/ui/` writes none while its response still carries the header,
  and the recorder sees no label built from it.
- The `details` array, in `internal/httpapi` against the encoded body rather than the struct:
  the parameter tables of *List targets* and *Fetch a profile* each refuse with one item,
  whose `code` is the value *Errors* names for that refusal and whose `field` is the parameter;
  `port` with `portName` carries two items in name order;
  a selection `allowedSelections` refuses carries `not_admitted` with the parameter the client sent;
  an error whose code has no vocabulary carries no `details` key at all, and no body ever carries `"details": []`;
  a raw query string that does not parse is `malformed_parameter` with an empty `field`,
  and an optional header the route refuses — an `Idempotency-Key` outside its grammar — is `header_malformed`
  while an omitted required one is `header_required`;
  no item names a Pod, an address, or the number a `portName` resolved to.
- The OpenAPI document, in `internal/httpapi`:
  the route answers `200` with the embedded bytes byte for byte and `Content-Type: application/json`,
  `503 not_ready` before the caches sync,
  `400 invalid_parameter` for any query parameter, `access_token` included,
  `405` with `Allow: GET`,
  and `200` with no credential under `basic` and under `oidc`, proving no authentication step runs.
  The check of *The OpenAPI document* is exercised against documents that differ from the code in exactly one way —
  a missing route, an extra route, a missing method, a missing code, an extra code, a missing vocabulary value,
  a renamed component that leaves a reference dangling, and a file reindented by hand — and each must fail;
  the shipped document must pass.
  The route table: every route the API listener serves has one declaration and the router reads no other source,
  asserted by driving one request per declaration and by a scan that finds no path matched outside it;
  the `Allow` header of a `405` on each route equals that route's declared methods;
  a `/ui/` route, an `/auth/` route, and a PGO route each appear in the document
  whether or not their configuration enables them,
  and none of `/healthz`, `/readyz`, or `/metrics` appears at all.
  The error-code registry: it holds exactly the codes of *Errors* and of [`pgo.md`](pgo.md) *Errors*,
  a constant removed from it or added to it fails the comparison with the document,
  and each transport mapping is exhaustive over its own input,
  asserted by a table that drives every input value through it.
- Target exclusion diagnostics, split by where each reason is decided:
  the eight cache-derived reasons in `internal/k8s` against the fake clientset,
  the two filter-derived reasons in `internal/httpapi` against the fake `Discovery`.
  `Explain` cannot produce the filter-derived pair, because it is passed no `version` and no `pod`.
  `internal/k8s`, one case per cache-derived reason,
  each built from the fixture that already exercises that eligibility rule,
  asserting the reason, its count, and that no other reason is reported.
  `endpoint_missing` is exercised on every branch that leaves a Pod without a trusted endpoint:
  no slice entry at all, a `nil` `targetRef`, a `targetRef` of another kind,
  a `targetRef` naming another namespace, a name no Pod in the cache carries, and a stale UID.
  `endpoint_address_mismatch` is exercised for an endpoint with no address
  and for one whose first address is not in `status.podIPs`;
  `port_name_not_declared` for a Pod with no port of the name, for one declaring it over UDP,
  and for the configured default name as well as a request `portName`;
  `endpoint_address_conflict` for one Pod carrying two addresses of the read family named by two eligible entries,
  which is the same-family case the reason is defensive against.
  Every existing eligibility rejection fixture asserts the attribution its rejection earns
  and that the target count plus the sum of the counts equals `SelectorMatched`,
  so a rule tested for exclusion is tested for its explanation in the same place.
  A Pod satisfying several reasons is attributed to the first in the vocabulary,
  asserted for a terminating Pod that is also unready
  and for an unready Pod whose endpoint also carries `ready: false`.
  A multi-Pod fixture inserts its reasons in an order the vocabulary does not use,
  and the report comes back in the vocabulary's order regardless.
  Reversing the EndpointSlice list, the endpoints within a slice, or the Pod list changes no count and no attribution,
  and two `Explain` calls over identical cached objects return identical `Excluded` slices.
  `SelectorMatched` is `0` for a Service whose selector matches no Pod;
  a Pod the selector does not match, present in a slice, is counted under no reason;
  a Service the cache lacks is `ErrServiceNotFound` and a selectorless one `ErrServiceSelectorless`, as for `Targets`;
  a Pod lister that fails returns an error rather than an empty count;
  the recording transport sees no request while any of it runs.
  The HTTP layer: `explain=true` adds `selectorMatched` and `excluded`,
  `excluded` encodes as `[]` and never `null`, and no body carries a `serviceFound` or `cacheSynced` field;
  `explain=false` and an absent `explain` produce today's body unchanged;
  `explain=1`, `explain=TRUE`, `explain=yes`, a repeated `explain`, and an empty value are `400 invalid_parameter`;
  `version=` and `pod=` narrow `targets` and add `version_mismatch` and `pod_name_mismatch`,
  leaving the counts the seam reported as they were,
  each added entry appearing in vocabulary order,
  and a `pod=` naming no eligible target is `200` with an empty array rather than `404 pod_not_found`;
  the sum invariant of *List targets* holds across those combinations, including both filters at once;
  a realm-denied request is `403 realm_denied` with the same body whether or not the Service exists,
  and the fake records that `Explain` was not called;
  readiness false is `503 not_ready` and writes no explain body, which is the only answer an unsynced cache has;
  a fake whose `Explain` fails is `503 discovery_unavailable`;
  no response, header, or audit line names a Pod beyond `targets` or carries an address;
  the audit line carries `explain` only for an accepted `explain=true`,
  writes an empty `pod` for a targets request that carried `pod=`, and carries no `version` field,
  and the recorder sees the `targets` endpoint with no label the parameter added.
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
- A manifest test pins each role's NetworkPolicy selectors and ports,
  the `PodMonitor` selector and the container port it names,
  and the Service's port list;
  the kind lanes cannot prove NetworkPolicy enforcement, only that the manifest is shaped as specified.
- Chart tests render `deploy/chart/profgate` with the `helm` binary mise pins, and assert on the objects:
  the gateway Deployment's memory limit and grace period are the same with `pgo.enabled` true and false,
  the collector's memory limit equals what `internal/config` computes for the ConfigMap the same render produced,
  which also proves the rendered configuration parses ([`pgo.md`](pgo.md) *Deployment*);
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
- `internal/client` and `cmd/profgate`, per [`cli.md`](cli.md) *Testing*:
  every deadline and interval read from an injected clock, so no test sleeps;
  cache binding to a canonical gateway origin, and the plaintext refusals that precede any request;
  the token cache's modes, atomic write, expiry by token type, refresh outcomes, and serialization;
  the device grant's polling rules, `login` and `logout`, and the idempotent `collect` retry;
  the dispatcher's positional grammar, output modes, and exit codes.
- `/v1/auth` in `internal/httpapi`, per [`cli.md`](cli.md) *Testing*:
  the four body shapes;
  the `oidc` object present only with an `auth.oidc.cli` block, and never derived from `auth.oidc.browser`;
  no credential read on the route, no audit record, and the `auth` metrics row.
- `internal/config`: `auth.oidc.cli` from file and environment,
  the `clientID` equality rule under `tokenType: id`,
  the refusal of `auth.oidc.browser.clientSecretFile` beside it,
  and every key rejected unless `auth.mode` is `oidc` ([`cli.md`](cli.md) *Configuration*).
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
Both lanes gain the client steps of [`cli.md`](cli.md) *End to end*:
`profgate login` against the lane's issuer, then a client command answered from the cached token.

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
  image substitution, `replicas: 2`,
  and ClusterRole variants missing `watch` and missing `get`.
  The default gateway's configuration, its `allowedSelections` included,
  is composed by the harness in Go and applied as a ConfigMap patch,
  so one function shapes every gateway the suite deploys.
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
   The same request with `?explain=true` counts the NotReady Pod under `pod_not_ready`,
   the first reason it satisfies, and names no Pod outside `targets`.
   Both replicas are polled until each reports that count,
   because until the Pod update reaches a replica that has already seen the endpoint update,
   that replica reports `endpoint_not_ready`, which is the correct answer to the cache it holds.
3. After a Pod is deleted, both replicas converge on the new target set within 10 seconds;
   the same holds after a Pod becomes ready (`needsPodReach`, because readiness is flipped through the test app).
   The two halves are separate scenarios in the registry so a degraded lane skips only the second.
4. Every one of the eight profiles fetched through the gateway parses with `github.com/google/pprof/profile`
   (trace is checked for the `go 1.` header bytes instead),
   and `cpu?seconds=2` completes in no less than 2 and no more than 5 seconds (`needsPodReach`).
5. A namespace outside the realm returns `403` identically for an existing and a missing Service;
   an unknown Service in an allowed namespace `404`;
   a selectorless Service with a manual Pod-backed slice `422`;
   `?pod=` naming a Pod of another Service is `404` on the profile endpoint,
   while the same `?pod=` on the targets endpoint is `200` with an empty array,
   and with `?explain=true` beside it the Service's own eligible Pods are counted under `pod_name_mismatch`.
6. `?version=` on the profile endpoint filters correctly and excludes Pods without the label;
   on the targets endpoint the same value lists only the Pods carrying it,
   and with `?explain=true` the rest are counted under `version_mismatch`.
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
- The collector Deployment of [`pgo.md`](pgo.md) and its NetworkPolicy ship in `deploy/`
  and are not named by `deploy/base/kustomization.yaml`,
  the way the example Secrets and the application policy are not.
  A kustomize base has no conditional and `pgo.enabled` is false by default,
  and a base that listed them would give every plain-kustomize install a collector Pod,
  for a feature its configuration has turned off.
  An operator enabling collection adds both to an overlay beside the `pgo` keys.
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
  - A static `limits.memory` of 512Mi on the gateway Deployment,
    whether `pgo.enabled` is true or false:
    a gateway replica holds no decoded profile, so no PGO ceiling sizes it.
    An explicit `resources.limits` overrides it,
    and `resources.requests` is a separate half rendered as written,
    shipping the CPU request a namespace whose quota counts `requests.cpu` needs.
    The derived limit belongs to the collector Deployment below, and to it alone.
  - Common labels on every Pod of both roles —
    `app.kubernetes.io/name: profgate` and the release instance —
    beside `app.kubernetes.io/component`, whose value is `gateway` or `collector`.
    One selector therefore reaches both roles and another reaches exactly one,
    which is what lets the `PodMonitor` scrape both while each NetworkPolicy binds one.
  - A collector Deployment, rendered only when `pgo.enabled`,
    running `profgate collector` at one replica with the ops port and no API port,
    selected by no Service,
    and carrying its own `resources.limits.memory` and `terminationGracePeriodSeconds`,
    both derived from the PGO ceilings and neither shared with the gateway
    ([`pgo.md`](pgo.md) *Deployment*).
  - `podSecurityContext.fsGroup`, 65532 by default, rendering no key at all when set to null,
    for a cluster that assigns its own ranges through a security context constraint.
  - Release-scoped ClusterRole and ClusterRoleBinding names, so two releases in one cluster do not collide,
    over rules identical to the base's.
  - `discovery.pprof.allowedSelections` rendered as an empty list in `values.yaml`,
    so a chart install accepts only the configured default until the operator lists more;
    the binary's own default is the same empty list.
  - `auth.oidc.cli.enabled`, default `true`.
    It renders an `auth.oidc.cli` block under `oidc` from the `clientID`, `scopes`, and `pkce` values beside it,
    each rendered only when set,
    so a chart install serves a device login until the operator sets it `false`;
    the chart omits the block when `auth.oidc.browser.clientSecretFile` is set under `tokenType: id`,
    the pair the binary refuses, and `NOTES.txt` says so
    ([`cli.md`](cli.md) *Configuration*).
    The binary's own default is no block.
  - An Ingress, off by default, routing `/`, `/ui/`, `/auth/`, and `/v1/` to the Service's API port,
    so an operator reaching the gateway from outside the cluster does not write one by hand.
    It never routes the ops port, which stays reachable only by the kubelet and the metrics scraper.
  - A prometheus-operator `PodMonitor`, off by default, for the ops port.
    It selects Pods and names the container port, because the ops port is absent from the Service by design.
    Its selector carries the common labels and no `component`,
    so it reaches the gateway Pods and the collector Pod alike;
    each role emits part of the PGO metric set ([`pgo.md`](pgo.md) *Metrics*),
    and a selector naming one role would drop the other half.
  - One NetworkPolicy per role, each selecting its own `component` value.
    The gateway's admits the API port from the Ingress controller's namespace
    and the ops port from the monitoring namespace, as the kustomize base does.
    The collector's admits the ops port from the monitoring namespace and nothing else,
    beside the egress [`pgo.md`](pgo.md) *Deployment* lists.
  - A prometheus-operator `PrometheusRule`, off by default,
    over the metrics section's readiness, admission, and signing-key gauges,
    and, when `pgo.enabled`, the collector-availability alert of [`pgo.md`](pgo.md) *Metrics*,
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
The command line adds no Go module either, for the reason in [`cli.md`](cli.md) *Dependencies*.

---

## 12. Package Layout

```text
cmd/profgate/        CLI: serve, collect, config validate, version, and the client verbs of cli.md
internal/k8s/        the seam; sole non-test importer of client-go
internal/proxy/      upstream HTTP to PodIP:Port, transport, budget, error mapping
internal/httpapi/    routing, realm checks, handlers, error bodies, audit log, the embedded OpenAPI document
internal/config/     fuda-loaded Config, strict pre-parse, validation, hot/restart classification
internal/metrics/    Recorder interface and the Prometheus implementation
internal/tlscert/    the API listener's certificate: load, re-read on a ticker, GetCertificate
internal/admit/      the admission gate interactive requests pass through
internal/auth/       Authenticator; basic, oidc, and disabled modes; JWKS cache; browser flow
internal/ui/         the console: embedded page and vendored browser libraries
internal/client/     the command-line client: contexts, transport, token cache, issuer client
deploy/              kustomize base and Helm chart
test/e2e/            harness, versions.yaml, testapp, overlays
```

Two packages embed files and no others do:
`internal/ui` the console tree, and `internal/httpapi` the one OpenAPI document it serves.

`internal/client` is the command line of [`cli.md`](cli.md) *Package layout*;
it is reachable only from `cmd/profgate` and imports no Kubernetes or NATS package.

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
| Client sends `explain=true` on the targets endpoint | the counts of *List targets* are added; the caller learns how many selected Pods are not targets and why, and no Pod name beyond `targets` |
| Service whose selector matches no Pod, with `explain=true` | `targets` is `[]`, `selectorMatched` is `0`, and `excluded` is `[]`: there is nothing to exclude, which no reason would have said |
| Caches not synced when `explain=true` is sent | `503 not_ready` at the readiness step; no explain body is written, and no field of any body reports the condition |
| Console sends `explain=true` to a replica older than this design, mid-rollout | `400 invalid_parameter`, the answer any unknown parameter gets; the console retries the fetch once without it ([`ui.md`](ui.md)) |
| Service cache read fails on a listing route | `503 discovery_unavailable`; never an empty `200` ([`ui.md`](ui.md)) |
| Rolling update with two console asset hashes | each request a page makes may reach either build; an asset the answering replica lacks is `404 route_unknown` and the page does not render until the rollout converges, after which a reload recovers ([`ui.md`](ui.md)) |
| `ui.enabled` false | `/ui/` and `/` are `404 route_unknown`; the four listing routes still answer |
| Client sends no `X-Request-Id`, or one the grammar refuses | one is generated; the request is answered as it would have been, and the response and the audit record carry the generated value |
| Client sends a usable `X-Request-Id` | it is echoed unchanged and written to the audit record; nothing else reads it |
| `/v1/openapi.json` before the caches sync | `503 not_ready`, like every other `/v1` route; the document itself would have been correct, and the exception is not worth its cost |
| `/v1/auth` before the caches sync | `503 not_ready`, like every other `/v1` route; the client reports it and exits ([`cli.md`](cli.md) *Failure scenarios*) |
| Router and OpenAPI document disagree | the check of *The OpenAPI document* fails; no build ships a route, method, or error code the document does not hold |
| `POST` to a PGO write route without `Content-Type: application/json` | `400 invalid_parameter` with a `details` item naming the header, immediately after the method step: before readiness, before authentication, and before any store call ([`pgo.md`](pgo.md)) |
| A route the router serves with no declaration in the route table | the router cannot reach it, so it is `404 route_unknown`; a declaration is the only way a path is matched |
| An envelope code a constructor names and the document does not | the check of *The OpenAPI document* fails against the registry; no build ships a code the document lacks |
| Replica drains while a `wait=` request is parked | the wait ends at the drain signal and answers with the record it last read; the drain bound is unchanged ([`pgo.md`](pgo.md)) |

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

Target exclusion diagnostics —
`explain=true` on the targets endpoint, the `version` and `pod` parameters beside it,
and the closed reason vocabulary they report —
amend the following text.
The first table lists the edits made in the same change as this block;
the second lists the documents that describe shipped behavior and are updated when the implementation lands.

Amended now:

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *The seam* | `Explain`, `Explanation`, and `Exclusion`: one cache pass over one captured Pod list, no request, no new RBAC tuple, and a failed Pod-cache read answered `503 discovery_unavailable` |
| `docs/specs/gateway.md` | *Eligibility* | the trusted endpoint, the closed reason vocabulary and its order, the counted population, and the per-Pod rule that attributes each counted Pod to one reason; the conflict among a Pod's trusted endpoints is decided over those satisfying rules 5 and 7, whether or not a pprof port resolves, so the attribution rule agrees with the reason table |
| `docs/specs/gateway.md` | *Request algorithm* | the parameter step takes `version`, `pod`, and `explain` on the targets endpoint; the discovery step calls `Explain`; the filter step narrows the listing and answers an empty array for a `pod` no target carries |
| `docs/specs/gateway.md` | *List targets* | the parameter table, the `explain=true` response, `selectorMatched`, `excluded`, the sum invariant, and where the two filter-derived reasons are counted |
| `docs/specs/gateway.md` | *Non-disclosure* | a fifth observation: the counts state how many selected Pods are not targets, inside a realm that already admits the Service |
| `docs/specs/gateway.md` | *Logging* | the audit field `explain`, added only for an accepted `explain=true`; the targets filters overload no field, `pod` staying the selected upstream Pod |
| `docs/specs/gateway.md` | *Metrics* | no label for `explain` |
| `docs/specs/gateway.md` | *Layers* | the eight cache-derived reasons in `internal/k8s` and the two filter-derived ones in `internal/httpapi`, attribution on every existing rejection fixture, order independence, the sum invariant, the parameter grammar, realm denial before `Explain`, and readiness before any explain body |
| `docs/specs/gateway.md` | *What end-to-end proves* | the ineligible-Pods proof asserts the `pod_not_ready` count on both replicas; the error and version proofs say which endpoint each assertion is about |
| `docs/specs/gateway.md` | *Failure Scenarios* | rows for an `explain` request, a selector matching no Pod, caches that have not synced, and an older replica refusing the parameter |
| `docs/specs/ui.md` | *Targets, with reasons*, *Controls*, *Failure scenarios*, *Unit*, *What is not proven*, *Layout and embedding*, *Dependencies*, *Package layout* | the console sends `explain=true` on every targets fetch, retries once without it on `400 invalid_parameter`, and turns an empty list into counted reasons in fixed wording, through a `targetmodel.js` the tests execute |
| `docs/specs/cli.md` | *Reading* | `targets --explain` sends `explain=true` and prints the `excluded` rows beside the list; `--output json` copies the body through unchanged |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/api.md` | the targets section: `version`, `pod`, and `explain`, `selectorMatched`, the `excluded` array, and the reason table |
| `docs/console.md` | the empty state showing counted reasons |

Running PGO collection in its own process —
the `profgate collector` subcommand, the collector Deployment the chart renders when `pgo.enabled`,
and the gateway's release from every PGO ceiling —
amends the following text.
The first table lists the edits made in the same change as this block;
the second lists the documents that describe shipped behavior and are updated when the implementation lands.

Amended now:

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *CLI* | `profgate collector --config <path>`, which runs the collection loops and opens no API listener, over the configuration file `serve` reads |
| `docs/specs/gateway.md` | *Request algorithm* | step 10 acquires from the admission gate, which interactive requests alone pass through |
| `docs/specs/gateway.md` | *Startup and shutdown* | the drain waits only for in-flight requests; no Collection wait beside it, and none on the listener-failure path; the grace period does not vary with `pgo.enabled` |
| `docs/specs/gateway.md` | *Layers* | the manifest test pins both roles' NetworkPolicy selectors and the `PodMonitor`; the chart test compares the collector's derived limit with the binary's and pins the gateway's static one |
| `docs/specs/gateway.md` | *Build and Deployment* | the gateway's static 512Mi limit; the common and `component` labels; the conditional collector Deployment with its own resources and grace period; the `PodMonitor` selecting both roles; one NetworkPolicy per role; the collector alert in the `PrometheusRule`; the collector Deployment and its policy ship outside `deploy/base/kustomization.yaml` |
| `docs/specs/gateway.md` | *Package Layout* | `cmd/profgate/` lists `collect`; `internal/admit/` is the gate interactive requests pass through |
| `docs/specs/pgo.md` | *Changes to the accepted gateway design* | the two `internal/admit` rows carry the wording the gateway spec now holds |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/deployment.md` | the collector Deployment: what the chart renders, what the kustomize tree leaves to an overlay, and how both roles are scraped |
| `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md` | the `component` labels, the collector block, and the gateway's static memory limit |

A contract a program can build on —
an identifier on every request, structured details inside the error envelope,
an OpenAPI document served and checked against the router,
and the media type the two PGO write routes require —
amends the following text.
The first table lists the edits made in the same change as this block;
the second lists the documents that describe shipped behavior and are updated when the implementation lands;
the third names the documents that read these routes and are revised on their own.

Amended now:

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *HTTP API* | the route inventory names `/v1/openapi.json`; every response carries `X-Request-Id` beside `Cache-Control: no-store` |
| `docs/specs/gateway.md` | *Request algorithm* | the identifier is assigned before step 1; a route may require a media type, which only the two PGO `POST` routes do; the document route's shorter path |
| `docs/specs/gateway.md` | *Errors* | the optional `details` array, its item shape, the rule that an error with no vocabulary carries no key at all, and the vocabularies of `invalid_parameter` and `port_not_allowed` |
| `docs/specs/gateway.md` | *Request identifier* | a new subsection: the grammar a client value must meet, the generated value, why a bad one is replaced rather than refused, where the header is set, and that it never travels upstream |
| `docs/specs/gateway.md` | *The OpenAPI document* | a new subsection: `GET /v1/openapi.json`, hand-maintained and served byte for byte, no credential and no realm, static across configuration, and the four comparisons its check makes against the router |
| `docs/specs/gateway.md` | *Non-disclosure* | `details` items name only inputs the request carried; two observations, the reflected identifier and the published route grammar |
| `docs/specs/gateway.md` | *Logging* | the `requestId` field, first in every record shape, and one record per request |
| `docs/specs/gateway.md` | *Health* | `/healthz` and `/readyz` answer `text/plain` in both outcomes; the identifier is echoed on all three ops paths |
| `docs/specs/gateway.md` | *Metrics* | the `openapi` endpoint value and its codes; no label built from the identifier |
| `docs/specs/gateway.md` | *Startup and shutdown* | a parked `wait=` request ends at the drain signal, so the drain bound does not move |
| `docs/specs/gateway.md` | *Layers* | unit rows for the identifier on both listeners, the `details` array against the encoded body, and the document route and its check against one-way-wrong documents |
| `docs/specs/gateway.md` | *Package Layout* | `internal/httpapi` holds the embedded document; the two packages that embed files are named below the tree |
| `docs/specs/gateway.md` | *Failure Scenarios* | rows for a refused and an accepted identifier, the document route before sync, a router the document disagrees with, a PGO write without the media type, and a drain during a wait |
| `docs/specs/pgo.md` | *HTTP API*, *Create a Collection*, *List Collections*, *Get a Collection*, *The latest completed Collection*, *Ceilings*, *Record*, *Errors*, *Logging*, *Metrics*, *Testing*, *Failure Scenarios* | the PGO half of the same contract, listed in that document's own amendment block |

Updated with the implementation:

| File | Change |
|---|---|
| `docs/api.md` | `X-Request-Id` on every response, the `details` array with its vocabularies, `/v1/openapi.json`, and the route count the new routes move |
| `internal/httpapi` | the route table the router and the check share, the embedded document, the check itself, and the two version refusals rewritten with literal codes so that check can read them |
| `internal/metrics` | the `openapi` endpoint value |

Carries a claim this change moves, and is revised on its own, not here:

| File | Section | Change |
|---|---|---|
| `.agents/rules/100-project-map.md` | *Planned Structure* | `internal/ui/` is no longer the sole user of `go:embed`: `internal/httpapi` embeds the document it serves |
| `.agents/rules/100-project-map.md` | *External HTTP API* | the route list gains `/v1/openapi.json`, and the two `latest` routes of [`pgo.md`](pgo.md) |

Tightening that contract —
one request-step order across the accepted designs,
a route table every API-listener route consumes,
a static registry of envelope codes for the document to be checked against,
a cache policy stated per surface,
and an idempotent create made durable in [`pgo.md`](pgo.md) —
amends the following text.

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *HTTP API* | every response carries `X-Request-Id`, and cache policy is per surface: `no-store` on `/v1`, the shell, `/auth/`, and ops, `no-cache` with an `ETag` on console assets |
| `docs/specs/gateway.md` | *Request algorithm* | the JSON media type step sits immediately after the method step and the PGO availability step between readiness and credential placement, in the order all four designs now state identically; `mime.ParseMediaType`, an essence of `application/json`, and every parameter accepted; steps 8 to 12 differ by endpoint; three routes are defined here |
| `docs/specs/gateway.md` | *Errors* | `header_malformed` covers any header the request carried and the route refuses, required or optional; `malformed_parameter` carries an empty field for a raw query string no name can be blamed for |
| `docs/specs/gateway.md` | *Request identifier* | every audit record carries `requestId`, while console and ops responses carry the header and write no record |
| `docs/specs/gateway.md` | *The OpenAPI document* | the route table is what every API-listener route is dispatched from, and what `405 Allow` and the check both read; the ops listener is outside it and says so; a static registry of envelope codes replaces reading the source for string literals |
| `docs/specs/gateway.md` | *Layers*, *Failure Scenarios* | unit rows for the table's coverage, the `Allow` header, the registry, the two `details` codes, and the per-surface cache policy; rows for a route with no declaration and a code the document lacks |
| `docs/specs/pgo.md` | *Create a Collection*, *List Collections*, *Get a Collection*, *The latest completed Collection*, *Atomicity primitives*, *Paths that touch each key*, *Algorithm*, *Record*, *Sweeper*, *Non-disclosure*, *Unit*, *Failure Scenarios* | the PGO half of the same tightening, listed in that document's own amendment block |

Read these routes and are amended by that block rather than by this one:
[`docs/specs/auth.md`](auth.md), whose composed order gains the media type step;
[`docs/specs/ui.md`](ui.md), whose satisfied rows leave its pending table;
and [`docs/specs/cli.md`](cli.md), whose `collect` reads a thin replay.

A first-party command line —
`/v1/auth` for a caller who holds no credential,
the client verbs of [`cli.md`](cli.md) in this binary,
and the `auth.oidc.cli` keys that make a device login discoverable —
amends the following text.

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *HTTP API* | the route inventory names `/v1/auth` |
| `docs/specs/gateway.md` | *Request algorithm* | `/v1/auth` runs route, method, readiness, and the parameter step that refuses every query parameter, and then answers, the shorter path `/v1/openapi.json` already takes |
| `docs/specs/gateway.md` | *Non-disclosure* | a further observation: `/v1/auth` publishes `auth.mode` to any caller, and the issuer, client identifier, token type, scopes, and PKCE assertion where `auth.oidc.cli` is configured |
| `docs/specs/gateway.md` | *CLI* | the client verbs of [`cli.md`](cli.md) *The verbs*, and the `collect` name that both halves of the binary now claim |
| `docs/specs/gateway.md` | *Logging* | `/v1/auth` writes no audit record, as `/ui/` writes none |
| `docs/specs/gateway.md` | *Metrics* | `endpoint` gains `auth`, with the codes the route has |
| `docs/specs/gateway.md` | *Layers* | unit rows for `internal/client`, for `/v1/auth`, and for `auth.oidc.cli` validation; the two authentication lanes gain the client's login and a command answered from the cached token |
| `docs/specs/gateway.md` | *Dependencies* | a closing sentence: the command line adds no Go module |
| `docs/specs/gateway.md` | *Package Layout* | `internal/client/`, and `cmd/profgate/` carrying the client verbs |
| `docs/specs/gateway.md` | *Failure Scenarios* | a row for `/v1/auth` before the caches sync |
| `docs/specs/auth.md` | *Request algorithm*, *Non-goals*, *Configuration*, *Issuer notes* | the authentication half of the same change, listed in that document's own amendments |
| `docs/specs/pgo.md` | *HTTP API* | the client that drives the PGO routes, listed in that document's own amendment block |
| `.agents/rules/100-project-map.md` | *Planned Structure*, *External HTTP API* | `internal/client/` and `/v1/auth` |
| `AGENTS.md` | *Four Specs, All Accepted* | five, adding [`cli.md`](cli.md) |
| `docs/README.md` | *Where Contributors Start* | [`specs/cli.md`](cli.md) beside the other specs |
| *Build and Deployment* | the chart's `auth.oidc.cli.enabled` value, default `true`, and the case in which the block is omitted ([`cli.md`](cli.md) *Configuration*) |
| *Harness* | the default gateway's configuration is composed by the harness in Go and applied as a ConfigMap patch, not carried by an overlay, as the suite already does |

Updated with the implementation:
[`docs/api.md`](../api.md), [`docs/authentication.md`](../authentication.md),
[`docs/configuration.md`](../configuration.md), [`docs/keycloak-realm.json`](../keycloak-realm.json),
and a client guide of its own, as [`cli.md`](cli.md) lists.
