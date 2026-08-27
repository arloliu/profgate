# API Guide

Profgate serves one HTTP API for pulling pprof profiles from the Pods behind a Kubernetes Service
and for running PGO CPU-profile collections.
This guide shows how to call it.
The normative behavior lives in the specs:
[specs/gateway.md](specs/gateway.md) for the interactive endpoints and
[specs/pgo.md](specs/pgo.md) for collection.
Configuration keys mentioned here are described in [configuration.md](configuration.md);
how to deploy the gateway is in [deployment.md](deployment.md).

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

The examples in the rest of this guide stay HTTP;
on an HTTPS listener this same shape applies to every one of them.

There is no index route and no OpenAPI document; the seven routes are:

| Route | Methods |
|---|---|
| `/v1/namespaces/{ns}/services/{svc}/targets` | GET |
| `/v1/namespaces/{ns}/services/{svc}/profiles/{profile}` | GET |
| `/v1/namespaces/{ns}/services/{svc}/pgo` | GET, PUT, DELETE |
| `/v1/namespaces/{ns}/services/{svc}/collections` | GET, POST |
| `/v1/collections/{id}` | GET |
| `/v1/collections/{id}/profile` | GET |
| `/v1/collections/{id}/cancel` | POST |

`{ns}` and `{svc}` must be DNS-1123 labels,
and `{id}` is a Collection identifier: exactly 20 lowercase Crockford base32 characters
(the digits and the lowercase letters except `i`, `l`, `o`, and `u`).
A path whose segments do not fit those grammars is not a route at all and answers `404 route_unknown`,
never a 400.
A known route called with the wrong method answers `405 method_not_allowed`,
and its `Allow` header lists what the route accepts.

## How a request is processed

A profile request runs this full sequence, and the first failing step answers.
A targets request runs steps 1 through 7 — parameter validation, the port allowlist, and discovery included —
and answers from what discovery found;
it never reaches single-target selection, admission, confirmation, or the proxy.
A policy or Collection request runs only steps 1 through 5 and then dispatches to its handler.

1. **Route** — the path must match one of the seven routes (`404 route_unknown`),
   and a profile route must name a known profile (`404 profile_unknown`).
2. **Method** — `405 method_not_allowed` plus `Allow` otherwise.
3. **Readiness** — until discovery has synced its caches, everything answers `503 not_ready`.
4. **PGO gate** — a PGO route answers `501 pgo_disabled` when `pgo.enabled` is false,
   and `503 pgo_unavailable` while the NATS stores cannot be decided from.
5. **Realm** — the request's realm must allow the namespace, Service, profile, and PGO action
   (`403 realm_denied`; see [Realms](#realms)).
6. **Parameters** — query string, request body, and preconditions are validated
   (`400 invalid_parameter` and friends).
   `port` and `portName` (never both) are checked here too:
   malformed or repeated is `400 invalid_parameter`,
   and a well-formed value a non-empty allowlist excludes is `400 port_not_allowed` —
   both answered before discovery runs.
7. **Discovery** — the Service is resolved to its Ready Pods
   (`404 service_not_found`, `422 service_selectorless`, `503 discovery_unavailable`).
8. **Select** — filters are applied and one target is chosen
   (`404 pod_not_found`, `503 no_targets`).
9. **Admission** — a concurrency slot is taken or the request is refused now (`429 too_many_profiles`).
10. **Confirm** — the chosen Pod is re-checked against the API server just before dialing
    (`503 target_changed`, which is safe to retry).
11. **Proxy** — the profile is fetched from the Pod and streamed back.

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
| `port` | A decimal integer from 1 to 65535: the pprof port for every Pod, replacing `discovery.pprof.port` or `portName` for this request. Must pass `discovery.pprof.allowedPorts` (`400 port_not_allowed` otherwise); excludes `portName`. |
| `portName` | A container-port name (the Kubernetes rule for `containerPort` names): the named TCP container port, replacing the configured default for this request. Must pass `discovery.pprof.allowedPortNames` (`400 port_not_allowed` otherwise); excludes `port`. |

`port` and `portName` exclude each other; sending both is `400 invalid_parameter`.
The configured default `discovery.pprof.port` or `discovery.pprof.portName` always passes its allowlist,
whatever the allowlist holds.

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
  bounded by the realm, `allowedPorts`, and NetworkPolicy, and nothing else.
- `400 port_not_allowed` reveals that a value is outside the allowlist.
  It reveals nothing about Pods: the realm is evaluated before it and discovery never runs.

### Examples

A 30-second CPU profile, saved and opened:

```sh
curl -sf -o cpu.pprof \
  "http://localhost:8080/v1/namespaces/payments/services/checkout/profiles/cpu?seconds=30"
go tool pprof cpu.pprof
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

The downloaded profile feeds straight into a PGO build:

```sh
go build -pgo=merged.pprof ./cmd/yourapp
```

(or commit it as `default.pgo` in the main package's directory,
which `go build -pgo=auto` picks up automatically).

## Realms

Authorization is a static ACL from the gateway's configuration.
Each realm lists the namespaces, Services, and profiles it may reach
(exact strings or `*`; there is no glob or prefix matching)
plus three PGO flags.
Authentication currently has one mode, `disabled`,
under which every request is the `anonymous` principal
and maps to the realm named by `auth.anonymousRealm`.

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
When the target Pod itself answers an HTTP error,
the gateway forwards that status and body verbatim instead of wrapping it.

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_parameter` | A query parameter, request body, or precondition header is malformed or not accepted here. |
| 400 | `seconds_exceeds_limit` | The effective duration exceeds `limits.cpuSeconds` or `limits.traceSeconds`. |
| 400 | `port_not_allowed` | The `port` or `portName` value is outside its configured allowlist; names only the value sent. |
| 400 | `limit_exceeded` | The effective PGO policy exceeds a `pgo.limits` ceiling; the message names the fields. |
| 403 | `realm_denied` | The realm does not allow this namespace, Service, profile, or PGO action. |
| 403 | `config_api_disabled` | `pgo.configAPI` is `disabled`; policy reads still work. |
| 404 | `route_unknown` | The path is not one of the seven routes (malformed names and identifiers included). |
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
| 429 | `too_many_profiles` | All `limits.maxConcurrentProfiles` slots are busy; the gateway never queues. |
| 429 | `rate_limited` | The on-demand Collection token bucket is empty; retry shortly. |
| 429 | `collection_in_progress` | The Service already has a live Collection. |
| 429 | `capacity_exhausted` | Live Collections reached `pgo.limits.maxLiveCollections` on this replica's view. |
| 501 | `pgo_disabled` | `pgo.enabled` is false; the PGO routes are recognized but unavailable on this gateway. |
| 502 | `upstream_unreachable` | The Pod could not be dialed or reset the connection before headers. |
| 502 | `upstream_redirect` | The Pod answered with a redirect, which the gateway never follows. |
| 503 | `not_ready` | Discovery has not synced yet; the gateway is starting up. |
| 503 | `discovery_unavailable` | Discovery cannot resolve or confirm right now. |
| 503 | `no_targets` | The Service has no eligible target (after any `version` filter). |
| 503 | `target_changed` | The chosen Pod changed between selection and dialing; retry. |
| 503 | `pgo_unavailable` | The PGO stores cannot be decided from right now; retry. |
| 504 | `upstream_timeout` | The Pod did not answer with headers in time. |

A failure after the response has started cannot change the status line;
the gateway then truncates the connection, so an incomplete body means the transfer failed —
check what you saved before trusting it.
