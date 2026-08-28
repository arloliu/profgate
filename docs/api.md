# API Guide

Profgate serves one HTTP API for pulling pprof profiles from the Pods behind a Kubernetes Service
and for running PGO CPU-profile collections.
This guide shows how to call it.
The normative behavior lives in the specs:
[specs/gateway.md](specs/gateway.md) for the interactive endpoints and
[specs/pgo.md](specs/pgo.md) for collection.
Configuration keys mentioned here are described in [configuration.md](configuration.md);
how to deploy the gateway is in [deployment.md](deployment.md).
The `profgate` binary is also a client of this API, one verb per route;
[cli.md](cli.md) covers it, and its form of each example appears beside the `curl` one below.

## Reaching the API

The API listener defaults to port 8080 (`server.listen`),
and serves HTTPS instead of HTTP when a certificate is configured (`server.tls`).
From a workstation, port-forward the gateway's Service and talk to localhost.
The forward lasts only while the command runs, so leave it running in its own terminal:

```sh
kubectl -n profgate port-forward svc/profgate 8080:8080
```

Every example below assumes that port-forward and uses `http://localhost:8080`.
A client running inside the cluster uses the Service DNS name instead,
for example `http://profgate.profgate.svc:8080`.

A first profile, end to end — a heap profile of the Service `checkout` in namespace `payments`,
opened in the pprof viewer:

```sh
curl -sf -o heap.pprof http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/heap
go tool pprof heap.pprof
```

The same profile through the client, which opens the viewer itself:

```sh
profgate --server http://localhost:8080 profile payments/checkout heap --open
```

### When the listener serves HTTPS

When `server.tls` is configured, the certificate is issued for a DNS name,
so a port-forwarded client must present that name rather than `localhost`:
the URL carries the name, and `--resolve` sends it to the forwarded port.
`--cacert` supplies the CA to verify against —
the CA that issued the certificate, for example exported from your issuer.

```sh
curl -sf -o heap.pprof --cacert ca.crt --resolve "<name>:8080:127.0.0.1" \
  "https://<name>:8080/v1/namespaces/payments/services/checkout/profiles/heap"
```

The client keeps the forwarded address in the URL and names the certificate's name and authority separately:

```sh
profgate --server https://localhost:8080 --server-name <name> --ca-file ca.crt \
  profile payments/checkout heap -o heap.pprof
```

The examples in the rest of this guide stay HTTP,
except where authentication requires TLS;
on an HTTPS listener this same shape applies to every one of them.

There is no index route and no OpenAPI document.
Twelve routes live under `/v1`; the seven that take path parameters are:

| Route | Methods |
|---|---|
| `/v1/namespaces/{ns}/services/{svc}/targets` | GET |
| `/v1/namespaces/{ns}/services/{svc}/profiles/{profile}` | GET |
| `/v1/namespaces/{ns}/services/{svc}/pgo` | GET, PUT, DELETE |
| `/v1/namespaces/{ns}/services/{svc}/collections` | GET, POST |
| `/v1/collections/{id}` | GET |
| `/v1/collections/{id}/profile` | GET |
| `/v1/collections/{id}/cancel` | POST |

Five more routes under `/v1` take no path parameter and answer from configuration or the cache:
[Listing endpoints](#listing-endpoints) below covers four of them,
and [Discovering how to log in](#discovering-how-to-log-in) covers `GET /v1/auth`,
the one `/v1` route that requires no credential.

`{ns}` and `{svc}` must be DNS-1123 labels,
and `{id}` is a Collection identifier: exactly 20 lowercase Crockford base32 characters
(the digits and the lowercase letters except `i`, `l`, `o`, and `u`).
A path whose segments do not fit those grammars is not a route at all and answers `404 route_unknown`,
never a 400.
A known route called with the wrong method answers `405 method_not_allowed`,
and its `Allow` header lists what the route accepts.

## How a request is processed

A profile request runs this full sequence, and the first failing step answers.
A targets request runs steps 1 through 9 — parameter validation, the port allowlist, and discovery included —
and answers from what discovery found;
it never reaches single-target selection, admission, confirmation, or the proxy.
A policy or Collection request runs only steps 1 through 7 and then dispatches to its handler.
A listing request ([Listing endpoints](#listing-endpoints)) runs the same steps 1 through 8 and then stops,
answering from the configuration snapshot or the Service cache with no discovery, selection, admission,
confirmation, or proxy step.
`GET /v1/auth` runs steps 1 through 3 and then step 8 alone:
it has no credential-placement, authentication, or realm step,
because it is the route a client reads before it holds a credential
([Discovering how to log in](#discovering-how-to-log-in)).

1. **Route** — the path must match one of the twelve `/v1` routes (`404 route_unknown`),
   and a profile route must name a known profile (`404 profile_unknown`).
2. **Method** — `405 method_not_allowed` plus `Allow` otherwise.
3. **Readiness** — until discovery has synced its caches, everything answers `503 not_ready`;
   under `auth.mode: oidc` the same answer also covers the time before issuer discovery
   and the initial signing-key fetch have succeeded.
4. **PGO gate** — a PGO route answers `501 pgo_disabled` when `pgo.enabled` is false,
   and `503 pgo_unavailable` while the NATS stores cannot be decided from.
5. **Credential placement** — an `access_token` query parameter is refused with `400 invalid_parameter`,
   in every authentication mode, before any credential is read.
6. **Authentication** — the principal and its realm are resolved per `auth.mode`
   (`401 unauthenticated`, `429 too_many_auth`, `503 auth_unavailable`, or, for a browser navigation, `302`;
   see [Authentication](#authentication)).
7. **Realm** — the request's realm must allow the namespace, Service, profile, and PGO action
   (`403 realm_denied`; see [Realms](#realms)).
8. **Parameters** — query string, request body, and preconditions are validated
   (`400 invalid_parameter` and friends).
   `port` and `portName` (never both) are checked here too:
   malformed or repeated is `400 invalid_parameter`,
   and a well-formed value `discovery.pprof.allowedSelections` does not admit is `400 port_not_allowed` —
   both answered before discovery runs.
9. **Discovery** — the Service is resolved to its Ready Pods
   (`404 service_not_found`, `422 service_selectorless`, `503 discovery_unavailable`).
10. **Select** — filters are applied and one target is chosen
    (`404 pod_not_found`, `503 no_targets`).
11. **Admission** — a concurrency slot is taken or the request is refused now (`429 too_many_profiles`).
12. **Confirm** — the chosen Pod is re-checked against the API server just before dialing
    (`503 target_changed`, which is safe to retry).
13. **Proxy** — the profile is fetched from the Pod and streamed back.

Every response, success or failure, carries `Cache-Control: no-store`.
Failures use the [error envelope](#errors) described at the end of this guide.

## Listing targets

```
GET /v1/namespaces/{ns}/services/{svc}/targets
```

Answers the Pods the gateway would profile right now, as its informer caches see them.
The endpoint takes the optional `port` or `portName` parameter described under
[Fetching a profile](#fetching-a-profile) and no other; any other query parameter is `400 invalid_parameter`.
The list holds the Pods eligible under that port:
`portName` excludes a Pod that has no TCP container port of that name,
while `port` never excludes a Pod, since a numeric port is used without checking that the Pod declares it.

```sh
curl http://localhost:8080/v1/namespaces/payments/services/checkout/targets
profgate targets payments/checkout
```

```json
{
  "namespace": "payments",
  "service": "checkout",
  "targets": [
    {"pod": "checkout-5f7c9d8b6-abcde", "node": "worker-1", "version": "1.42.0"},
    {"pod": "checkout-5f7c9d8b6-fghij", "node": "worker-2", "version": "1.42.0"}
  ]
}
```

Targets are sorted by Pod name, and `targets` is `[]` (never `null`) for a Service with no eligible backend.
`version` is the value of the Pod label named by `discovery.versionLabel`
(default `app.kubernetes.io/version`), and is empty when the Pod has no such label.
The response never discloses a Pod IP or port.

## Listing endpoints

```
GET /v1/namespaces
GET /v1/namespaces/{ns}/services
GET /v1/whoami
GET /v1/limits
```

Four routes let a script, or [the console](console.md), discover what a realm can reach without already knowing a namespace or a Service name.
They read informer caches and configuration, never the API server, take no query parameter
(any parameter is `400 invalid_parameter`), and answer from the realm the caller's credential resolves to.
Every array in these responses is `[]`, never `null`, when empty, and lists are sorted by name.
The client's `namespaces`, `services <namespace>`, `whoami`, and `limits` verbs call these four routes
and print the bodies below as tables, or verbatim under `--output json`.

```sh
curl http://localhost:8080/v1/namespaces
profgate namespaces
```

```json
{"namespaces": ["orders", "payments"]}
```

```sh
curl http://localhost:8080/v1/namespaces/payments/services
```

```json
{"namespace": "payments", "services": ["checkout", "ledger"]}
```

The namespace and Service lists apply the caller's realm the way every other route does:
`realm.namespaces` and `realm.services` each admit a value by exact match or by holding `"*"`,
and only a Service with a non-empty selector is ever listed.
A namespace the realm does not admit is absent from the namespace list,
and its Service list is `403 realm_denied` whether or not the namespace holds a Service —
a namespace whose only Services fall outside `realm.services` is absent from the namespace list the same way,
because listing it would disclose that it exists.

```sh
curl http://localhost:8080/v1/whoami
```

```json
{
  "principal": "alice",
  "realm": {
    "name": "payments-dev",
    "namespaces": ["payments"],
    "services": ["*"],
    "profiles": ["cpu", "heap", "goroutine"],
    "pgo": {"read": true, "collect": false, "configure": false}
  },
  "auth": {"mode": "oidc", "logout": "/auth/logout"}
}
```

`realm` is the caller's own realm exactly as configured, the wildcard included.
`auth.logout` is present only when the browser flow is configured, and is always `/auth/logout`.
Under `disabled` the principal is `anonymous`, as everywhere else.

```sh
curl http://localhost:8080/v1/limits
```

```json
{
  "cpuSeconds": 60,
  "traceSeconds": 60,
  "profiles": ["cpu", "trace", "heap", "allocs", "goroutine", "mutex", "block", "threadcreate"],
  "pprof": {
    "default": {"port": 6060},
    "allowedSelections": [{"port": 6061}, {"portName": "pprof-alt"}]
  },
  "pgo": {"enabled": true}
}
```

`pprof.default` is `{"port": N}` or `{"portName": "name"}`, whichever `discovery.pprof` is configured with,
and `allowedSelections` is `discovery.pprof.allowedSelections` as configured:
an array of one-key objects, each `{"port": N}`, `{"portName": "name"}`, `{"port": "*"}`, or `{"portName": "*"}`,
and `[]` when the list is empty.
`pgo.enabled` is `pgo.enabled`, and is how a caller learns whether PGO collection is on at all.

**`/v1/limits` deliberately discloses `allowedSelections` and the configured default to any authenticated caller.**
That is a decision, not an oversight:
the values are global operator configuration, not cluster state,
and every profile request the values let a caller build is one the caller could already build without them,
bounded by the same realm, the same list, and the same NetworkPolicy.
An unauthenticated probe still learns nothing but `401`.

## Fetching a profile

```
GET /v1/namespaces/{ns}/services/{svc}/profiles/{profile}
```

The gateway picks one Ready Pod of the Service (uniformly at random by default),
fetches the profile from its pprof port, and streams the bytes back.

### Profile types

| Profile | Upstream path | Takes `seconds` | Default seconds |
|---|---|---|---|
| `cpu` | `/debug/pprof/profile` | yes | 30 |
| `trace` | `/debug/pprof/trace` | yes | 1 |
| `heap` | `/debug/pprof/heap` | no | — |
| `allocs` | `/debug/pprof/allocs` | no | — |
| `goroutine` | `/debug/pprof/goroutine` | no | — |
| `mutex` | `/debug/pprof/mutex` | no | — |
| `block` | `/debug/pprof/block` | no | — |
| `threadcreate` | `/debug/pprof/threadcreate` | no | — |

### Query parameters

Each parameter may appear at most once, with a value; anything unknown is `400 invalid_parameter`.

| Parameter | Meaning |
|---|---|
| `seconds` | Duration for `cpu` and `trace` only: a decimal integer from 1 to 86400. The effective duration (given or default) must also fit the configured limit — `limits.cpuSeconds` or `limits.traceSeconds`, both 60 by default — or the request is `400 seconds_exceeds_limit`. |
| `pod` | Pin the exact Pod to profile. A Pod that is not currently an eligible target answers `404 pod_not_found`. |
| `version` | Keep only Pods whose version label equals this value, then select among them. The filter runs before the `pod` pin. |
| `strategy` | Selection strategy; `random` is the only value and the default. |
| `port` | A decimal integer from 1 to 65535: the pprof port for every Pod, replacing `discovery.pprof.port` or `portName` for this request. Must be admitted by `discovery.pprof.allowedSelections` (`400 port_not_allowed` otherwise); excludes `portName`. |
| `portName` | A container-port name (the Kubernetes rule for `containerPort` names): the named TCP container port, replacing the configured default for this request. Must be admitted by `discovery.pprof.allowedSelections` (`400 port_not_allowed` otherwise); excludes `port`. |

`port` and `portName` exclude each other; sending both is `400 invalid_parameter`.
`discovery.pprof.allowedSelections` is default-deny:
an empty list admits only the configured default `discovery.pprof.port` or `discovery.pprof.portName`,
a `{port: N}` or `{portName: name}` entry admits that one value,
and `{port: "*"}` or `{portName: "*"}` admits every value of its own kind.
The configured default is always admitted, whether or not the list names it
([`configuration.md`](configuration.md#discovery)).

### Response

On success the body is the upstream pprof response, streamed as it arrives.
Only these upstream headers are forwarded:
`Content-Type`, `Content-Length`, `Content-Encoding`, `Content-Disposition`, and `X-Content-Type-Options`.
The gateway adds three headers of its own, on forwarded responses only, saying who was profiled:

```
X-Pprof-Target-Pod: checkout-5f7c9d8b6-abcde
X-Pprof-Target-Node: worker-1
X-Pprof-Target-Version: 1.42.0
```

Error envelopes never carry target headers.
If the Pod itself answers a 4xx or 5xx, the gateway forwards that status and body verbatim.

### Admission and confirmation

The gateway never queues a profile request:
when all `limits.maxConcurrentProfiles` slots (16 by default) are busy,
the request is refused immediately with `429 too_many_profiles` — back off and retry.
Just before dialing, the chosen Pod is confirmed against the API server:
same UID, still running and ready, still holding the selected address.
Confirmation catches a Pod that was replaced, terminated, or lost readiness —
it checks that one Pod, not whether the Service's membership has moved,
which still comes from the informer caches.
`503 target_changed` is safe to retry,
but a retry reruns the same cached discovery and random selection,
so it can land on the same stale target until the caches advance,
and a request pinned with `pod=` keeps its pin.

### What choosing ports reveals

Choosing ports lets an authorized client observe three things that this design does not try to close:

- `portName` on the targets endpoint changes per-Pod eligibility,
  so calling it with different names reveals which admitted Pods declare each named TCP port.
- A numeric `port` on the profile endpoint reaches the Pod without checking its declarations,
  so an authorized caller can combine `pod=` with different numbers,
  and the outcome then tells an open pprof port from a refused, silent, redirecting, or non-pprof HTTP port —
  a port-scanning capability over admitted Pods,
  bounded by the realm, `discovery.pprof.allowedSelections`, and NetworkPolicy, and nothing else.
- `400 port_not_allowed` reveals that `discovery.pprof.allowedSelections` does not admit a value.
  It reveals nothing about Pods: the realm is evaluated before it and discovery never runs.

### Examples

A 30-second CPU profile, saved and opened:

```sh
curl -sf -o cpu.pprof \
  "http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/cpu?seconds=30"
go tool pprof cpu.pprof
```

```sh
profgate profile payments/checkout cpu --seconds 30 -o cpu.pprof
```

`go tool pprof` can also fetch through the gateway directly:

```sh
go tool pprof "http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/cpu?seconds=30"
```

A heap profile pinned to one Pod:

```sh
curl -sf -o heap.pprof \
  "http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/heap?pod=checkout-5f7c9d8b6-abcde"
go tool pprof heap.pprof
```

```sh
profgate profile payments/checkout heap --pod checkout-5f7c9d8b6-abcde --open
```

A CPU profile from whatever Pods run a specific release:

```sh
curl -sf -o cpu.pprof \
  "http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/cpu?seconds=30&version=1.42.0"
```

A 5-second execution trace:

```sh
curl -sf -o trace.out \
  "http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/trace?seconds=5"
go tool trace trace.out
```

## PGO collection

The five PGO routes are always recognized but unavailable until the operator has enabled collection:
with `pgo.enabled` false they all answer `501 pgo_disabled`,
and while the gateway cannot reach its NATS stores they answer `503 pgo_unavailable`.
What a Collection actually does — rounds, merging, artifacts — is described in [pgo.md](pgo.md);
this section covers the API surface.
None of the PGO routes take query parameters.

### Policy override

```
GET    /v1/namespaces/{ns}/services/{svc}/pgo
PUT    /v1/namespaces/{ns}/services/{svc}/pgo
DELETE /v1/namespaces/{ns}/services/{svc}/pgo
```

Each Service's PGO policy is the operator's `pgo.defaults` with an optional stored override layered on top,
one level deep: an override of `{"sampling": {"rounds": 3}}` changes `rounds` and nothing else.
Every response body has the same shape:

```json
{
  "namespace": "payments",
  "service": "checkout",
  "source": "override",
  "override": {"enabled": true, "sampling": {"rounds": 3}},
  "effective": {
    "enabled": true,
    "schedule": {"every": "6h", "jitter": "10m"},
    "sampling": {"duration": "30s", "rounds": 3, "roundInterval": "30s", "replicas": "all", "maxParallel": 4},
    "target": {"versionPolicy": "strict", "version": ""},
    "artifact": {"retention": "2h"}
  },
  "violations": [],
  "updatedBy": "anonymous",
  "updatedAt": "2026-08-26T09:00:00Z"
}
```

`source` is `"override"` when an override is stored and `"defaults"` when the Service runs on defaults alone
(in which case `override` is `null`, and `updatedBy`, `updatedAt`, and the `ETag` header are absent).
`violations` lists any effective fields a current ceiling would refuse; it is `[]` when the policy fits.
Durations are Go duration strings (`"30s"`, `"6h"`),
and `sampling.replicas` is either the string `"all"` or an integer.

Writes are guarded by ETags so two clients cannot silently overwrite each other.
Every response for a stored override carries an `ETag` header holding a quoted decimal revision,
and that exact form is the only `If-Match` value accepted — `If-Match: *` is refused as `400 invalid_parameter`.

- The first `PUT` for a Service, sent without `If-Match`, creates the override and answers `201` with the new ETag.
- A later `PUT` must send `If-Match` with the ETag of the last read;
  without it the write is `428 precondition_required`, and with a stale one it is `412 precondition_failed` —
  re-read, reconcile, and try again.
- `DELETE` always requires `If-Match` and answers `204` on success,
  returning the Service to the operator's defaults;
  deleting a Service with no override is `404 pgo_override_not_found`.

Request bodies must be a single JSON value of at most 64 KiB,
and a field the policy schema does not declare is rejected (`400 invalid_parameter`).
A policy whose effective values exceed a `pgo.limits` ceiling is refused with `400 limit_exceeded`,
naming the offending fields.
When the operator has set `pgo.configAPI: disabled`,
`PUT` and `DELETE` answer `403 config_api_disabled` while `GET` keeps working.

```sh
# Create the override (first write: no If-Match).
curl -sf -i -X PUT \
  -d '{"enabled": true, "schedule": {"every": "6h"}, "sampling": {"rounds": 3}}' \
  http://localhost:8080/v1/namespaces/payments/services/checkout/pgo

# Update it later, conditional on the ETag the last response carried.
curl -sf -i -X PUT -H 'If-Match: "42"' \
  -d '{"enabled": true, "sampling": {"rounds": 5}}' \
  http://localhost:8080/v1/namespaces/payments/services/checkout/pgo
```

Note that `PUT` replaces the whole override, not individual fields:
send the full override you want stored.

### Collections

```
GET  /v1/namespaces/{ns}/services/{svc}/collections
POST /v1/namespaces/{ns}/services/{svc}/collections
GET  /v1/collections/{id}
GET  /v1/collections/{id}/profile
POST /v1/collections/{id}/cancel
```

`GET .../collections` lists the Service's Collections newest first,
at most the newest 100 records, with no pagination:

```json
{
  "namespace": "payments",
  "service": "checkout",
  "collections": [
    {
      "id": "3g7hk2m9p4qr8s1tvw5x",
      "origin": "api",
      "state": "completed",
      "attempt": 1,
      "resolvedVersion": "1.42.0",
      "createdAt": "2026-08-26T09:00:00Z",
      "finishedAt": "2026-08-26T09:04:12Z",
      "expiresAt": "2026-08-26T11:04:12Z"
    }
  ]
}
```

A Collection that falls off the listing stays readable at `GET /v1/collections/{id}`
until `pgo.jobRetention` expires it,
so a client that needs a Collection later should keep the `id`
(or the `Location` header) from creation.

`POST .../collections` starts an on-demand Collection.
The body is optional — an empty body means "use the Service's effective policy as it stands" —
and otherwise it is a one-shot policy override for this Collection only.
It has the same shape and layering as the stored override,
except that `enabled` and `schedule` may not appear (they are rejected as `400 invalid_parameter`).
On success the answer is `202` with a `Location` header pointing at the record:

```json
{"id": "3g7hk2m9p4qr8s1tvw5x", "state": "pending"}
```

A create can be refused with `429`:
`rate_limited` when the on-demand token bucket (`pgo.limits.onDemandPerMinute`) is empty,
`collection_in_progress` when the Service already has a live Collection,
and `capacity_exhausted` when live Collections have reached `pgo.limits.maxLiveCollections` on this replica's view.
It can also be refused with `409 version_missing` or `409 version_conflict`:
a Collection profiles exactly one binary version,
so it cannot start when no Pod carries a usable version label,
when no eligible Pod matches a pinned `target.version`,
or when the Pods disagree on their version.

`GET /v1/collections/{id}` answers the full stored record.
The interesting fields:

- `state` — one of `initializing`, `pending`, `running`, `completed`, `failed`, `cancelled`, `expired`.
- `attempt` — how many times the Collection has been claimed; `reason` says why a terminal state was reached.
- `resolvedVersion` — the binary version the Collection profiles.
- `policy` — the effective policy snapshot it runs with, fixed at creation.
- `progress` — the owner's last checkpoint: `round`, `rounds`, `samplesOK`, `samplesFailed`.
- `manifest` — which Pods contributed which samples, once sampling has run.
- `artifact` — the stored merged profile's object name and size, set when the Collection completes.
- `createdAt`, `startedAt`, `finishedAt`, `expiresAt` — the timestamps of its life;
  `expiresAt` is when a completed profile's retention runs out.

`GET /v1/collections/{id}/profile` downloads the merged profile:
`application/octet-stream` with `Content-Disposition: attachment; filename="{id}.pprof"`,
plus `X-Pprof-Collection` and `X-Pprof-Target-Version` headers.
Only a `completed` Collection has one:
an `expired` Collection answers `410 artifact_gone`,
and every other state answers `409 collection_not_completed`.

`POST /v1/collections/{id}/cancel` (no body) ends a live Collection
and answers `200` with the updated record.
A Collection that already finished answers `409 collection_terminal`,
and one still `initializing` answers `409 collection_initializing` — retry shortly.

### End to end

```sh
# 1. Give the service a PGO policy (first write, so no If-Match).
curl -sf -X PUT \
  -d '{"enabled": true, "sampling": {"rounds": 3, "replicas": "all"}}' \
  http://localhost:8080/v1/namespaces/payments/services/checkout/pgo

# 2. Start an on-demand collection.
curl -sf -X POST \
  http://localhost:8080/v1/namespaces/payments/services/checkout/collections
# -> 202 {"id": "3g7hk2m9p4qr8s1tvw5x", "state": "pending"}

# 3. Poll the record until it completes.
curl -sf http://localhost:8080/v1/collections/3g7hk2m9p4qr8s1tvw5x
# -> {"state": "running", "progress": {"round": 2, "rounds": 3, ...}, ...}
# -> {"state": "completed", "artifact": {...}, ...}

# 4. Download the merged profile before its retention runs out.
curl -sf -o merged.pprof \
  http://localhost:8080/v1/collections/3g7hk2m9p4qr8s1tvw5x/profile
```

The same four steps through the client, which reads the `ETag` and waits on the record itself:

```sh
profgate pgo policy set payments/checkout --enabled --rounds 3 --replicas all
profgate collect payments/checkout --wait
profgate download 3g7hk2m9p4qr8s1tvw5x -o merged.pprof
```

The downloaded profile feeds straight into a PGO build:

```sh
go build -pgo=merged.pprof ./cmd/yourapp
```

(or commit it as `default.pgo` in the main package's directory,
which `go build -pgo=auto` picks up automatically).

## Authentication

`auth.mode` picks how a request is attributed to a principal: `disabled` (every request is `anonymous`),
`basic` (an HTTP Basic credential against a static user list), or `oidc` (a bearer JWT from an issuer,
plus an optional browser login).
The full design, including the failure reasons the audit log records, is
[specs/auth.md](specs/auth.md); this section covers what a client sends and gets back.
A request that fails authentication answers `401 unauthenticated`.
The `WWW-Authenticate` header names the scheme the mode accepts,
`Basic realm="profgate"` or `Bearer realm="profgate"`,
and the body and message are the identical `authentication required` whatever the reason:
the response never says which check failed.

### `basic` mode

```sh
curl -u alice -sf -o cpu.pprof \
  "https://profgate.example/v1/namespaces/payments/services/checkout/profiles/cpu?seconds=30"
profgate --server https://profgate.example -u alice profile payments/checkout cpu --seconds 30 -o cpu.pprof
```

The client prompts for the password without echo, or reads `PROFGATE_USER` and `PROFGATE_PASSWORD`.
`basic` mode requires `server.tls`, because the password crosses the network on every request;
a gateway configured with `basic` and no server certificate refuses to start
unless `auth.basic.allowPlaintext: true` is set, the escape hatch for a lab behind a TLS-terminating Ingress,
and it then logs a warning that passwords cross the network in the clear
(see [specs/auth.md](specs/auth.md), *Transport*).

`go tool pprof` cannot set a header, but Go's HTTP client turns URL userinfo into a Basic header,
so a password in the URL still works:

```sh
go tool pprof "https://alice:PASSWORD@profgate.example/v1/namespaces/payments/services/checkout/profiles/cpu"
```

That form puts the password in shell history and process listings.
A browser that receives `401` with `WWW-Authenticate: Basic` shows its own login dialog and retries,
so `basic` mode needs nothing else to serve one.

### `oidc` mode

A command-line client sends the token as a bearer credential:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/cpu?seconds=30"
```

`go tool pprof <url>` fetches with `http.Client.Get` and offers no way to add a header,
so it cannot present a bearer token; use `curl` to save the profile, then open the file.
The client obtains the token itself, caches it, and refreshes it:

```sh
profgate login --context prod --server https://profgate.example
profgate --context prod profile payments/checkout cpu --seconds 30 --open
```

### Discovering how to log in

```
GET /v1/auth
```

The one `/v1` route that requires no credential:
it is what a client reads before it holds one, so requiring one would make it answer only callers who no longer need it.
It runs the route, method, readiness, and parameter steps of [How a request is processed](#how-a-request-is-processed)
and none of the others: `GET` only, `503 not_ready` before the gateway is ready,
`400 invalid_parameter` for any query parameter (an `access_token` parameter included), and `Cache-Control: no-store`.
It writes no audit record.

```sh
curl http://localhost:8080/v1/auth
```

The body has four shapes.
Under `basic` it is `{"mode": "basic"}` and under `disabled` `{"mode": "disabled"}`.
Under `oidc` it is `{"mode": "oidc"}`, carrying an `oidc` object only when `auth.oidc.cli` is configured,
the operator's statement that this issuer admits a device login:

```json
{
  "mode": "oidc",
  "oidc": {
    "issuer": "https://keycloak.example/realms/engineering",
    "clientID": "profgate",
    "tokenType": "id",
    "scopes": ["openid", "offline_access"],
    "pkce": true
  }
}
```

`clientID` is `auth.oidc.cli.clientID`, `auth.oidc.audience` by default;
`scopes` is `auth.oidc.cli.scopes`, `["openid", "offline_access"]` by default;
`pkce` is `auth.oidc.cli.pkce`.
None of the three is derived from `auth.oidc.browser`.

**What it discloses.**
Under `basic` and `disabled` it publishes the mode, which the `WWW-Authenticate` header on every `401` already names.
Under `oidc` with the `cli` block it publishes four more values to an unauthenticated caller:
an issuer URL, a public client identifier, a token type, and scope names.
A deployment running the browser flow already hands all four out through the `302` that `/auth/login` answers;
a bearer-only deployment does not, and for it this is a new disclosure.
It is accepted because an issuer publishes its discovery document by design,
a public client identifier cannot be secret,
and no namespace, Service, Pod, realm, principal, or credential appears in the body.
An operator who declines it configures no `auth.oidc.cli` block, and the route reports the mode alone.

### The browser flow

With `auth.oidc.browser` configured, a browser navigation under `/v1` that carries no credential
and no session cookie is answered `302` to `/auth/login?return=<path>` instead of `401`.
From a user's view: following a gateway link, or opening one directly, lands on the issuer's own login page;
after signing in, the browser is sent to a same-origin landing page that reads "Signed in"
and then to the original path, now carrying a session cookie that lasts `sessionTTL` (8 hours by default).
The three routes exist only when the browser block is configured (`404 route_unknown` otherwise),
serve `GET` only, and are not under `/v1`:

| Route | What it does |
|---|---|
| `GET /auth/login?return=<path>` | starts a login; redirects to the issuer |
| `GET /auth/callback?code=...&state=...` | completes a login; mints the session cookie |
| `GET /auth/logout` | clears the session cookie and redirects to the issuer's logout, or to `/` |

Without the browser block, `oidc` mode is bearer-only and a browser gets `401` like any other client.
A terminal acquires its token with `profgate login` ([cli.md](cli.md#the-first-login)),
which needs `auth.oidc.cli` rather than the browser block;
see [authentication.md](authentication.md) for the issuers this has been run against.

## Realms

Authorization is a static ACL from the gateway's configuration.
Each realm lists the namespaces, Services, and profiles it may reach
(exact strings or `*`; there is no glob or prefix matching)
plus three PGO flags.
The realm a request is evaluated against comes from authentication (see
[Authentication](#authentication)): the resolved principal's realm under `basic` or `oidc`,
or the realm named by `auth.anonymousRealm` while `auth.mode` is `disabled`.

```yaml
realms:
  developer:
    namespaces: ["payments", "orders"]
    services: ["*"]
    profiles: ["cpu", "heap", "goroutine"]
    pgo:
      read: true
      collect: true
      configure: false
auth:
  mode: disabled
  anonymousRealm: developer
```

The PGO flags gate the PGO routes by what they do:

| Flag | Grants |
|---|---|
| `read` | `GET .../pgo`, `GET .../collections`, `GET /v1/collections/{id}`, `GET /v1/collections/{id}/profile` |
| `collect` | `POST .../collections`, `POST /v1/collections/{id}/cancel` |
| `configure` | `PUT .../pgo`, `DELETE .../pgo` |

A denied request answers `403 realm_denied` with an identical body
whether or not the Service exists, so a denial discloses nothing.
A Collection-scoped route evaluates the realm against the record's own namespace and Service,
and a denied Collection answers `404 collection_not_found`,
exactly as if the identifier did not exist.

## Errors

Every gateway-generated failure is a JSON envelope with a stable machine-readable code:

```json
{"error": "pod checkout-5f7c9d8b6-abcde changed since it was selected; retry", "code": "target_changed"}
```

`error` is human-readable and may change; `code` is stable and is what clients should switch on.
The message names at most a namespace, a Service, a Pod, or the `port`/`portName` value the client sent —
never a Pod address, and never the port number a `portName` selection resolved to.

`400 port_not_allowed` also carries a `details` array with exactly one item,
naming the query parameter the client sent and the value it sent, and nothing else:

```json
{
  "error": "port \"6062\" is not allowed by this gateway",
  "code": "port_not_allowed",
  "details": [{"field": "port", "code": "not_admitted", "message": "6062 is not an admitted selection"}]
}
```

`field` is `port` or `portName`, whichever the request carried, and `code` is always `not_admitted`.
Every other error omits the `details` key entirely — never `null`, never `[]`.
When the target Pod itself answers an HTTP error,
the gateway forwards that status and body verbatim instead of wrapping it.

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_parameter` | A query parameter, request body, or precondition header is malformed or not accepted here — including `access_token` as a query parameter, refused in every authentication mode before any credential is read. |
| 400 | `seconds_exceeds_limit` | The effective duration exceeds `limits.cpuSeconds` or `limits.traceSeconds`. |
| 400 | `port_not_allowed` | `discovery.pprof.allowedSelections` does not admit the `port` or `portName` value; names only the value sent, in the message and in its one `details` item. |
| 400 | `limit_exceeded` | The effective PGO policy exceeds a `pgo.limits` ceiling; the message names the fields. |
| 401 | `unauthenticated` | No credential, a wrong or expired one, or one that maps to no realm; see [Authentication](#authentication). `WWW-Authenticate` names the scheme. |
| 403 | `realm_denied` | The realm does not allow this namespace, Service, profile, or PGO action. |
| 403 | `config_api_disabled` | `pgo.configAPI` is `disabled`; policy reads still work. |
| 404 | `route_unknown` | The path is not one of the twelve `/v1` routes (malformed names and identifiers included), or, under `/auth/`, not one of the three routes, or the browser block is not configured. |
| 404 | `profile_unknown` | The profile name is not in the profile table. |
| 404 | `service_not_found` | The Service does not exist in that namespace. |
| 404 | `pod_not_found` | The pinned Pod is not currently an eligible target. |
| 404 | `pgo_override_not_found` | `DELETE .../pgo` on a Service with no stored override. |
| 404 | `collection_not_found` | No such Collection — or one the realm denies; the two answer alike. |
| 405 | `method_not_allowed` | Wrong method for the route; `Allow` lists the accepted ones. |
| 409 | `version_missing` | No Pod carries a usable version label, or none matches a pinned `target.version`, so a Collection cannot start. |
| 409 | `version_conflict` | The Service's Pods carry more than one version, so a Collection cannot start. |
| 409 | `collection_not_completed` | The Collection has no stored profile (it is not `completed`). |
| 409 | `collection_initializing` | The Collection is still being published; retry shortly. |
| 409 | `collection_terminal` | The Collection already finished, so it cannot be cancelled. |
| 410 | `artifact_gone` | The Collection's profile has expired and is no longer stored. |
| 412 | `precondition_failed` | `If-Match` names a revision the policy has moved past; re-read and retry. |
| 422 | `service_selectorless` | The Service has no selector, so it has no Pods to profile. |
| 428 | `precondition_required` | The Service already has an override; the write must send `If-Match`. |
| 429 | `too_many_auth` | `basic` mode's per-replica bcrypt comparison gate is full; carries `Retry-After: 1`. |
| 429 | `too_many_profiles` | All `limits.maxConcurrentProfiles` slots are busy; the gateway never queues. |
| 429 | `rate_limited` | The on-demand Collection token bucket is empty; retry shortly. |
| 429 | `collection_in_progress` | The Service already has a live Collection. |
| 429 | `capacity_exhausted` | Live Collections reached `pgo.limits.maxLiveCollections` on this replica's view. |
| 501 | `pgo_disabled` | `pgo.enabled` is false; the PGO routes are recognized but unavailable on this gateway. |
| 502 | `upstream_unreachable` | The Pod could not be dialed or reset the connection before headers. |
| 502 | `upstream_redirect` | The Pod answered with a redirect, which the gateway never follows. |
| 503 | `not_ready` | Discovery has not synced yet, or, under `oidc`, issuer discovery and the initial signing-key fetch have not succeeded yet; the gateway is starting up. |
| 503 | `auth_unavailable` | The gateway cannot decide: stale signing keys, a failed random read, or an unreachable issuer during a browser login; carries `Retry-After: 5`. |
| 503 | `discovery_unavailable` | Discovery cannot resolve or confirm right now. |
| 503 | `no_targets` | The Service has no eligible target (after any `version` filter). |
| 503 | `target_changed` | The chosen Pod changed between selection and dialing; retry. |
| 503 | `pgo_unavailable` | The PGO stores cannot be decided from right now; retry. |
| 504 | `upstream_timeout` | The Pod did not answer with headers in time. |

A failure after the response has started cannot change the status line;
the gateway then truncates the connection, so an incomplete body means the transfer failed —
check what you saved before trusting it.
