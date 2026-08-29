# PGO Collection

How to collect representative CPU profiles from a Service's Pods and feed them to `go build -pgo=`.
This guide covers the workflow end to end: provisioning, policy, collecting, and consuming the merged profile.
The full route reference lives in [`api.md`](api.md) and every configuration key in
[`configuration.md`](configuration.md); this page links to them instead of restating them.

## What PGO mode does

A single interactive pprof request profiles one Pod at one moment,
which is a poor input for Profile-Guided Optimization:
`go build -pgo=` wants a profile that represents the whole workload.
PGO mode models that as a **Collection** —
a short asynchronous job that resolves a Service to the Pods its informer caches hold at each round,
fetches a CPU profile from each of a sampled subset in one or more rounds spread over time,
merges the samples in memory, and stores one merged `.pprof` artifact that any gateway replica can serve.
PGO collects CPU profiles only, is off by default (`pgo.enabled: false`),
and changes none of the gateway's interactive behavior:
discovery, authorization, and proxying never touch NATS,
and a gateway with PGO disabled opens no NATS connection at all.

## Prerequisites

PGO coordination lives in three pre-provisioned NATS JetStream stores.
The gateway never creates them — startup preflight opens each one,
checks its configuration and the permissions of the gateway's NATS user with reversible probes,
and exits non-zero when anything is missing, of the wrong kind, misconfigured, or denied.
`/readyz` stays `503` until that preflight has passed, so a misprovisioned replica never joins the Service.

Before enabling PGO, check off:

1. **A NATS JetStream cluster the gateway can reach.**
   [`deployment.md`](deployment.md) covers where it runs relative to the gateway.
2. **The three stores, provisioned once** —
   see [`../deploy/nats/README.md`](../deploy/nats/README.md) for the contract preflight checks;
   these commands are the recommended safe defaults:

   ```text
   nats kv add PROFGATE_CONFIG    --storage file --ttl 0 --max-bucket-size -1 --max-value-size -1 --history 1
   nats kv add PROFGATE_JOBS      --storage file --ttl 0 --max-bucket-size -1 --max-value-size -1 --history 1
   nats object add PROFGATE_ARTIFACTS --storage file --ttl 0 --max-bucket-size -1
   ```

   The `-1` sizes mean unlimited, which preflight accepts;
   a bounded size also passes at or above the floors preflight checks:
   64 MiB per KV bucket, 1 GiB for the Object Store, and 512 KiB per KV value.

   `PROFGATE_CONFIG` (KV) holds per-Service policy overrides,
   `PROFGATE_JOBS` (KV) holds schedule slots and Collection records,
   `PROFGATE_ARTIFACTS` (Object Store) holds the merged profiles.
3. **On an authenticated NATS, a user with exactly the permissions in
   [`account.conf`](../deploy/nats/account.conf)**.
   How the credentials reach the gateway depends on the server —
   [`deploy/nats/README.md`](../deploy/nats/README.md) walks both paths.
   On a server in operator mode they ride in a JWT credentials file,
   held in a Secret the Deployment mounts read-only at `/etc/profgate/nats/`:

   ```text
   kubectl -n profgate create secret generic profgate-nats-creds --from-file=nats.creds=profgate.creds
   ```

   A server whose accounts live in its configuration file produces no credentials file:
   set `nats.credsFile: ""` and carry the username and password in `nats.url`.
   A NATS deployment without authentication needs no user and no Secret either:
   set `nats.credsFile: ""`, keep the URL free of userinfo, and the gateway connects without credentials.

4. **Configuration**: `pgo.enabled: true`, `nats.url`,
   and `nats.credsFile` when the server hands the gateway a credentials file —
   see [`configuration.md`](configuration.md) for the whole `nats` and `pgo` blocks.
5. **A realm with `pgo` flags.**
   All PGO routes are realm-checked; a realm without a `pgo` block has every flag false.
   `pgo.read` covers policy reads, listing, records, and downloads;
   `pgo.collect` covers creating and cancelling Collections;
   `pgo.configure` covers writing and deleting policy overrides.

## Policy

Every Service gets its effective policy by layering a per-Service override over the operator's
`pgo.defaults` block, one level deep: an override of `{"sampling": {"rounds": 3}}` changes `rounds` and nothing else.
Overrides are written through the API and stored in `PROFGATE_CONFIG`;
the write is optimistic-concurrency-controlled with an `ETag`/`If-Match` revision pair
(`GET` returns the `ETag`, `PUT` sends it back as `If-Match`, a stale value is `412`) —
[`api.md`](api.md) has the full flow.
`enabled` defaults to `false` and has no operator default:
putting a Service on the schedule is always an explicit override.

Every override is validated against the `pgo.limits` ceilings when written and again when read,
so a client can never exceed what the operator sized the gateway for.

What each setting buys you in profile quality:

| Setting | Effect on the merged profile |
|---|---|
| `schedule.every` | how fresh the newest artifact is; each slot is an opportunity for at most one new Collection |
| `schedule.jitter` | spreads different Services' Collections inside the interval, so they do not all sample at once |
| `sampling.duration` | seconds of CPU profiling per sample; longer samples smooth out short-lived spikes |
| `sampling.rounds` | how many passes over the Pods; more rounds capture more distinct moments in time |
| `sampling.roundInterval` | the gap between rounds; a wider gap makes the rounds more independent |
| `sampling.replicas` | Pods sampled per round (`"all"` or a count); more Pods average out per-replica skew |
| `sampling.maxParallel` | concurrent samples; affects throughput, not quality |
| `target.version` | optional pin to one version label value; unset means "whatever the Pods agree on" |
| `artifact.retention` | how long the merged profile stays downloadable (default `24h`, and at least `schedule.every`) |

Rounds spread over time are the point:
one artifact merges several rounds across several Pods,
and represents the workload in a way a single snapshot cannot.
Collections are version-pinned (`target.versionPolicy: strict`):
every sampled Pod must carry the same version label value,
so two builds never mix in one artifact.

All examples in this guide go through a port-forward to the gateway Service,
kept running in its own terminal:

```text
kubectl -n profgate port-forward svc/profgate 8080:8080
```

To schedule a Service, write an override with `enabled: true`
(first write of a new override sends no `If-Match` and returns `201`):

```text
curl -sS -X PUT http://localhost:8080/v1/namespaces/payment/services/payment-api/pgo \
  -H 'Content-Type: application/json' \
  -d '{"enabled": true, "schedule": {"every": "1h", "jitter": "5m"}, "sampling": {"rounds": 3}}'
```

## Collecting

A Collection has one of two origins.

### Scheduled

With an override at `enabled: true`, every gateway replica divides time into slots of `schedule.every`
and races to create the slot's key in `PROFGATE_JOBS`; at most one replica wins each slot
and publishes a Collection with `origin: "schedule"`.
A slot can also pass without a Collection:
a replica stands aside while the Service already has a live Collection
or live Collections have reached `pgo.limits.maxLiveCollections` on its view,
and a publication can still fail after the slot is won —
the slot is then consumed and the next is at most `schedule.every` away.
The guarantee is at most one Collection per Service per slot.
There is nothing to trigger and no manual action needed.
What you observe:

- `GET /v1/namespaces/{ns}/services/{svc}/collections` shows at most one Collection per slot,
  newest first, 100 records to a page,
  with `state`, `origin`, and `since` to narrow it and `nextCursor` to page through the rest —
  a record that falls off the page stays readable at `GET /v1/collections/{id}`
  until `pgo.jobRetention` expires it,
  so a client that needs a Collection later should keep the `id` (or the `Location` header) from creation;
  a record moves through `pending` and `running` to `completed` when all goes well,
  and otherwise ends `failed` or `cancelled`;
  it can also show an `initializing` record — a publication caught mid-flight —
  which becomes `pending`,
  disappears when a competing publication wins the Service's active slot,
  or ends `failed` with reason `not_published` when its publication or cleanup never completed;
- the `profgate_schedule_slots_total` metric counts slots `won`, `lost`, `busy`, and `capacity`;
- missed slots are never caught up —
  a gateway that returns after an outage creates at most one Collection per Service, for the current slot.

### On demand

`POST /collections` creates a Collection with `origin: "api"` immediately,
regardless of `enabled`, subject to a per-replica token bucket of `pgo.limits.onDemandPerMinute`.
The worked sequence, end to end:

```text
$ curl -sS -i -X POST http://localhost:8080/v1/namespaces/payment/services/payment-api/collections \
    -H 'Content-Type: application/json' -d '{}'
HTTP/1.1 202 Accepted
Location: /v1/collections/7h2k9m4p6r8t0v1w3x5y

{"id": "7h2k9m4p6r8t0v1w3x5y", "state": "pending"}
```

An empty body `{}` runs the Service's effective policy;
the body may override `sampling`, `target`, and `artifact` for this one Collection
(never `enabled` or `schedule`).

A script that can lose its answer sends an `Idempotency-Key` header with the create
and retries under the same key.
The gateway binds the key to the Collection it creates,
so the retry answers `200` with that Collection's identifier and state instead of starting a second one,
and a key that asks for something else than the first request did answers `409 idempotency_mismatch`.
That is what turns a timed-out create from "a Collection may or may not be running,
go and look" into a request the caller simply sends again.
The `profgate` client does this on every `collect` ([`cli.md`](cli.md)).

Poll the record until it is terminal:

```text
$ curl -sS http://localhost:8080/v1/collections/7h2k9m4p6r8t0v1w3x5y
{"id": "7h2k9m4p6r8t0v1w3x5y", "state": "running", "progress": {"round": 1, "rounds": 2, ...}}
...
{"id": "7h2k9m4p6r8t0v1w3x5y", "state": "completed", "artifact": {"object": "...", "bytes": 481920}, ...}
```

Download the merged profile and hand it to the Go toolchain:

```text
$ curl -sS -o default.pgo http://localhost:8080/v1/collections/7h2k9m4p6r8t0v1w3x5y/profile
$ go build -pgo=default.pgo ./cmd/payment-api
```

A build that just wants the freshest profile the Service has does not need an identifier at all:

```text
$ curl -sS -o default.pgo \
    http://localhost:8080/v1/namespaces/payment/services/payment-api/collections/latest/profile
```

That route answers with the newest Collection whose artifact is still stored,
and `.../collections/latest` beside it answers with that same Collection's record,
so a pipeline can log which Collection and which version it built against.
A Service with no stored artifact answers `404 collection_not_found`,
whether it has never collected or its artifacts have all expired.
Both routes need only the realm's `pgo.read` flag.

Saving it as `default.pgo` in the main package's directory also works with no flag at all:
`go build` picks that file up by convention (`-pgo=auto` is the default since Go 1.21).
The record's `manifest` tells you whether the profile is safe for the build you are optimizing:
`resolvedVersion` is the one version every sample came from,
`samples` lists each Pod's result,
and `truncated: true` means a round had more eligible Pods than it sampled
(`sampling.replicas`, itself capped by `pgo.limits.maxTargetsPerRound`)
and the artifact is a sample of the fleet rather than the whole of it.

Sampling shares the gateway's interactive admission gate (`limits.maxConcurrentProfiles`),
and configuration validation guarantees
`pgo.limits.maxParallel × pgo.limits.maxActiveCollections < limits.maxConcurrentProfiles`,
so Collections can never hold every slot and interactive profiling always has headroom.

## Lifecycle reference

### States

```text
initializing -> pending -> running -> completed -> expired
      |            |          |-----> failed
      |            |          `-----> cancelled
      |            |-----> failed
      |            `-----> cancelled
      |-----> failed
      `-----> (deleted)
```

`initializing` is the moment between the record's creation and its publication; it is never claimable.
It ends one of three ways:
publication moves it to `pending`,
the publisher deletes it when a competing publication wins the Service's active slot,
or a worker fails it with reason `not_published` once the publish grace has passed.
A `pending` record waits for a worker to claim it (until `claimBy`: one `schedule.every` for a scheduled
Collection, one hour for an on-demand one);
unclaimed past `claimBy` it fails with reason `not_claimed`,
and a cancel through the API ends it before any worker runs.
A `running` record is owned by one replica, which renews a lease on it while the rounds run.
`completed` names a downloadable artifact until the artifact's retention passes;
the sweeper then deletes the object and flips the record to `expired`.
Terminal records are kept for `pgo.jobRetention` (default `168h`) and then deleted.
Until then a record stays readable at `GET /v1/collections/{id}`,
even after newer records push it off the Service's listing,
which holds at most the newest 100 records and offers no pagination —
a client that needs a Collection later keeps the `id` (or the `Location` header) from creation.

### Failure reasons

The `reason` field of a `failed` record:

| Reason | Meaning |
|---|---|
| `version_missing` | no sampled Pod carried the version label, or none matched a pinned `target.version` |
| `version_conflict` | the first round saw more than one version label value |
| `no_targets` | no eligible Pod matched the resolved version |
| `no_samples` | a round ended with zero successful samples |
| `deadline_exceeded` | the Collection was still running past its per-Collection deadline |
| `attempts_exhausted` | a claim would have exceeded `pgo.maxAttempts` (default 3) |
| `artifact_store_failed` | storing the merged profile in `PROFGATE_ARTIFACTS` failed |
| `merged_too_large` | the merged profile outgrew `pgo.limits.maxMergedBytes` |
| `serialize_failed` | the merged profile could not be serialized |
| `record_too_large` | the record with its manifest exceeded the record size bound; per-sample entries are dropped |
| `not_claimed` | no worker claimed the record before `claimBy` |
| `not_published` | an `initializing` record whose publication or cleanup never completed |
| `limit_exceeded` | the stored policy snapshot exceeds the claiming replica's current ceilings |

A cancelled record carries `cancelled_by_api`.
Individual failed samples do not fail a Collection —
they are recorded in the manifest (`upstream_timeout`, `sample_too_large`, `slot_timeout`, and so on)
and only a round with zero successes does.

### Retention, expiry, cancel

`GET .../profile` on an `expired` record — or on a `completed` one whose object is already gone —
answers `410 artifact_gone`;
the next scheduled Collection that completes produces a fresh artifact.
`POST /v1/collections/{id}/cancel` ends a `pending` or `running` Collection
(`200` with the updated record, `409 collection_terminal` when it already ended);
the owning replica notices within a third of the lease TTL and stops,
and a cancelled Collection never names an artifact.

## Multiple gateway replicas

Every replica runs the same three loops — scheduler, worker, sweeper — and none is a standing leader.
Coordination is compare-and-swap on KV keys:

- **At most one Collection per slot.**
  All replicas race a `Create` on the slot key; the first wins, the rest move on.
- **One live Collection per Service.** A second creator — scheduled or on-demand — loses the `active` key
  and the API answers `429 collection_in_progress`.
- **Ownership is a lease.**
  The owning replica renews `leaseUntil` (`pgo.leaseTTL`, default `60s`) while it works.
  If the replica dies, another replica's periodic scan reclaims the record after the lease lapses —
  provided the Collection's deadline has not passed and an attempt remains —
  and restarts sampling from round 0 as a new attempt under a new object name;
  `pgo.maxAttempts` (default 3) bounds total attempts:
  the default permits the first attempt and at most two retries.
- **Rollouts drain.**
  On `SIGTERM` a replica stops claiming and waits for its in-flight Collections,
  up to each Collection's own deadline
  (the merge and store steps cannot be interrupted mid-call);
  work still running at its deadline is abandoned.
  The PGO figure `profgate config validate` prints is the termination grace period
  that lets the drain wait through any admissible Collection's deadline at the configured ceilings —
  it bounds the wait, it does not guarantee completion.
  A shorter grace period is a supported choice with a different outcome:
  the process is killed and the interrupted attempt's samples are discarded.
  Another replica reclaims the record and retries from round zero,
  but only if the lease (`pgo.leaseTTL`) expires before the Collection's deadline
  and an attempt remains under `pgo.maxAttempts`;
  otherwise the Collection ends `failed` as `deadline_exceeded` or `attempts_exhausted`,
  whichever bound wins.
  Deleting a gateway Pod ends the same way, by reclaim on another replica under the same bounds.
  A process that keeps running after its Pod object is gone loses its Kubernetes credentials with it,
  fails every confirmation in the next round,
  and can end the Collection itself as `failed no_samples`;
  either way, the Service becomes eligible again
  and a later successful slot may publish another Collection.
- **The sweeper cleans up everywhere.**
  Every replica expires artifacts past retention,
  deletes terminal records past `jobRetention`, consumed slot keys, orphaned objects,
  and active keys whose Collection is gone.

Completed artifacts survive any of this: once a record is `completed`, any replica serves the download.

## Sizing and limits

The `pgo.limits` block is the operator's ceiling on everything a policy can ask for —
per-setting ranges and defaults are in [`configuration.md`](configuration.md).
The Deployment's memory limit is sized from

```text
maxActiveCollections × (maxParallel × 8 × maxSampleBytes + 2 × 8 × maxMergedBytes)
```

over the gateway's own footprint — 4 GiB at the shipped defaults;
`profgate config validate` prints the figure for the loaded configuration.

Refusals you can hit as a client, all `429`:

| Code | Cause |
|---|---|
| `collection_in_progress` | the Service already has a live Collection |
| `rate_limited` | this replica's `pgo.limits.onDemandPerMinute` token bucket is empty |
| `capacity_exhausted` | live Collections reached `pgo.limits.maxLiveCollections` on this replica's view |

None of the three refusals leaves a new live Collection,
though the loser of a `collection_in_progress` race may briefly create and remove an `initializing` record;
retry after the current Collection ends or the bucket refills.
