# Deployment

How to install, configure, and operate profgate in a Kubernetes cluster.
The gateway is one stateless Deployment:
it watches Services, Pods, and EndpointSlices, proxies pprof profiles from application Pods,
and, when PGO collection is enabled, coordinates that work through NATS JetStream.
Kubernetes 1.23 is the compatibility baseline.

The image is `ghcr.io/arloliu/profgate`,
built with ko on a distroless static nonroot base and published for `linux/amd64` and `linux/arm64`.
The Helm chart is an OCI artifact at `oci://ghcr.io/arloliu/charts/profgate`.

Two install surfaces ship, and neither is a copy of the other:
the [Helm chart](../deploy/chart/profgate/README.md) templates what an external operator has to change,
and the [kustomize base](../deploy/base) is plain YAML for a repository that already patches with kustomize.
This guide covers the whole lifecycle;
the chart README holds the full values table and the canonical install commands,
which this guide links to rather than repeats.

## Installing

### With Helm

```bash
helm install profgate oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z \
  --namespace profgate --create-namespace
```

An unreleased chart installs from a checkout:

```bash
helm install profgate ./deploy/chart/profgate --namespace profgate --create-namespace
```

Clusters that cannot reach `ghcr.io` directly — a proxy, a one-time transfer, or a standing mirror —
are covered in
[Installing from a private network](../deploy/chart/profgate/README.md#installing-from-a-private-network).

### With kustomize

The base at [`deploy/base`](../deploy/base) applies the same Deployment as plain YAML:

```bash
kubectl apply -k deploy/base
```

It hard-codes the `profgate` namespace, plain HTTP, and PGO off;
a repository using it is expected to patch the ConfigMap and Deployment with overlays of its own.
The base applies only the gateway's own resources —
its ServiceAccount, ClusterRole and binding, ConfigMap, Deployment, Service,
and the gateway NetworkPolicy the chart makes optional.
The application-side NetworkPolicy example ships separately as
[`deploy/networkpolicy-app-example.yaml`](../deploy/networkpolicy-app-example.yaml),
a template to copy and customize for each application's namespace.

### Minimum useful configuration

The shipped defaults run, but three settings decide whether the gateway is useful:

- **Realms** — what each principal may reach.
  The default is one wide-open `developer` realm, a deliberate choice for a gateway reachable only inside the cluster;
  narrow it before exposing the API more widely.
- **`auth.anonymousRealm`** — the realm every request gets while authentication is disabled.
  It must name one of the configured realms.
- **The pprof port** — `discovery.pprof.port` (default 6060) or `discovery.pprof.portName`,
  which is how discovery finds the profiling port on application Pods.

With the chart, realms and `auth.anonymousRealm` are structured values,
and the pprof port goes in the raw `config` block:

```yaml
config:
  discovery:
    pprof:
      port: 6060
```

[configuration.md](configuration.md) documents every key the file accepts.

## RBAC

The chart and the base both create a ClusterRole with fixed, read-only rules:

| Resource | Verbs |
|---|---|
| `services` | list, watch |
| `pods` | get, list, watch |
| `endpointslices` (`discovery.k8s.io`) | list, watch |

That is the permission invariant:
profgate requires no Kubernetes write permissions,
observes Services, Pods, and EndpointSlices, connects only to explicitly permitted pprof ports,
and touches only its own `PROFGATE_*` NATS stores.
The chart offers no value that widens the rules,
and a golden test pins them:
`deploy/deploy_test.go` checks the base's ClusterRole,
and `deploy/chart_test.go` holds the chart's rendered rules equal to it.

Startup runs a preflight that exercises exactly these tuples against the API server.
A 403 on any of them ends the process with a log line naming the resource and verb the ClusterRole lacks;
any other failure retries with a doubling backoff,
so an API server that is briefly away delays startup rather than failing it.

The chart names the ClusterRole and ClusterRoleBinding after the release,
so two releases can share a cluster without taking over each other's RBAC.

## Configuration delivery

The chart renders the gateway's configuration file into a ConfigMap mounted at `/etc/profgate/config.yaml`.
Structured values cover the keys the chart itself depends on
(`server.logLevel`, `auth.anonymousRealm`, `realms`, `tls`, `nats`, `pgo`);
everything else goes in the raw `config:` block, which is merged over the structured keys one key at a time
and wins where both set the same key.
The keys the memory limit is derived from are the exception:
the chart rejects `pgo.enabled` and the four sizing ceilings under `pgo.limits` in the raw block and in `extraEnv`,
because a value arriving through either hatch would leave the rendered limit sized for different ceilings.
The file-path keys the Secret mounts follow — `nats.credsFile` and the `server.tls` certificate paths —
are rejected there too,
because a path arriving through either hatch can name a file nothing mounts.
See [Configuration](../deploy/chart/profgate/README.md#configuration) in the chart README.

The binary reads the file once at startup and has no reload.
The pod template therefore carries a `checksum/config` annotation hashing the rendered ConfigMap,
so a `helm upgrade` that changes only configuration still rolls the Pods.
Setting `configChecksumAnnotation: false` opts out,
and configuration changes then wait for the next unrelated rollout.

`extraEnv` sets `PROFGATE_`-prefixed environment overrides,
which the binary applies on top of the file — the way to override one key without touching the rendered file.
The overrides render into the pod template,
so a change to them rolls the Deployment on upgrade like any other pod-template change;
the checksum annotation exists for a change that touches only the ConfigMap.

## TLS on the API listener

The API port serves plain HTTP by default, with TLS terminating at an Ingress or a mesh.
To serve HTTPS from the gateway itself, enable the chart's `tls` block:

```yaml
tls:
  enabled: true
  existingSecret: profgate-tls
  certKey: tls.crt
  keyKey: tls.key
  mountPath: /etc/profgate/tls
  minVersion: "1.2"
```

The chart never creates the Secret.
It mounts the `kubernetes.io/tls` Secret named by `tls.existingSecret` read-only at mode 0440
and renders `server.tls.certFile` and `server.tls.keyFile` to point inside it.
[`deploy/secret-tls-example.yaml`](../deploy/secret-tls-example.yaml) shows the Secret as a manifest,
and a cert-manager `Certificate` whose `secretName` matches works without any other change.
Unlike the NATS volume, the certificate volume is not optional:
enabling TLS asserts the certificate exists,
so a missing Secret holds the Pod at mount time with an event naming it,
rather than starting a Pod that exits over a file it cannot open.

**Renewals need no rollout.**
The gateway re-reads both files every 30 seconds, compares a hash of their contents,
and swaps the served certificate when they change;
a read or parse failure keeps the pair already loaded and logs a warning.
A certificate renewed into the same Secret is therefore served without restarting a Pod,
and the pod template deliberately carries no checksum annotation for the TLS Secret —
hashing it would roll the Deployment on every renewal and defeat the reload.
`profgate_tls_reloads_total{result}` and `profgate_tls_certificate_expiry_seconds` are how a rotation
that stopped working becomes visible before the certificate expires.

Two details worth knowing:

- `podSecurityContext.fsGroup: 65532` (the default) makes the 0440 key file readable by the distroless image;
  a cluster that assigns its own ranges must arrange that differently.
- The ops listener stays plaintext either way:
  the kubelet's probe and the metrics scraper reach it by Pod address,
  where a certificate could never be verified.

## NATS for PGO collection

PGO collection (`pgo.enabled: true`) keeps its control-plane state in three NATS JetStream stores,
which the operator provisions once, before enabling collection:

| Store | Kind |
|---|---|
| `PROFGATE_CONFIG` | KV bucket |
| `PROFGATE_JOBS` | KV bucket |
| `PROFGATE_ARTIFACTS` | Object Store |

The exact `nats kv add` / `nats object add` commands, the account's permission set,
and the credentials-Secret command live in [`deploy/nats/README.md`](../deploy/nats/README.md)
and [`deploy/nats/account.conf`](../deploy/nats/account.conf).
In short: file storage, no TTL, the default `discard: new`,
and sizes either unlimited — the recommended safe default in those commands —
or bounded at or above the floors preflight checks:
64 MiB per KV bucket, 1 GiB for the Object Store, and 512 KiB per KV value,
owned by an account whose user may reach the three `PROFGATE_` stores and nothing else —
no stream creation, no stream deletion, no bucket outside `PROFGATE_`.

The gateway never creates the stores.
At startup a NATS preflight opens them, checks the bucket contract —
file storage, no TTL, `discard: new`, and minimum size floors —
and live-probes the account's permissions with real operations.
A missing bucket, a contract violation, or a denied probe is fatal:
the process exits with an error naming the bucket and the failing field or operation,
because no amount of waiting turns it into a pass.
Only connection-level failures retry, with a doubling backoff.

With `pgo.enabled`, a Pod is not Ready until that preflight passes.
The Deployment rolls with `maxUnavailable: 0`,
so a NATS outage stalls a rollout instead of replacing working replicas —
the safe failure, but it does mean NATS being down blocks upgrades.

On a NATS in operator mode, JWT credentials come from the Secret named by `nats.existingSecret`
(`profgate-nats-creds` by default), mounted read-only at `/etc/profgate/nats/`;
[`deploy/secret-nats-example.yaml`](../deploy/secret-nats-example.yaml) shows it as a manifest.
Create it before enabling PGO — startup opens the file and exits when it is missing.
The volume itself is optional so the Pod can be created before the Secret exists;
once the Secret appears, the kubelet projects it into the volume and the next restart finds the file.
`nats.credsFile: ""` mounts nothing and sends no JWT credentials;
a username and password carried in `nats.url` still authenticate the gateway,
and on a NATS deployment without authentication a userinfo-free URL connects without credentials —
[`deploy/nats/README.md`](../deploy/nats/README.md) walks the paths.

[pgo.md](pgo.md) covers what collection does once it is running.

## Operations

### Probes and readiness

The ops listener on port 9090 serves `/healthz`, `/readyz`, and `/metrics`.
It is deliberately not in the Service — scrape and probe Pods directly.

`/readyz` answers 200 when all of these hold:
the gateway is not draining, the Kubernetes informer caches have synced,
and, with PGO enabled, the NATS preflight has passed.
A later NATS disconnect does not turn readiness off:
the replica keeps serving interactive requests and answers the PGO routes 503.

There is no livenessProbe, by design.
`/healthz` answers 200 whenever the HTTP server is up and depends on neither Kubernetes nor NATS,
so a liveness probe on it could detect nothing but an HTTP server that has stopped answering entirely.
The tradeoff is deliberate: readiness takes a Pod that cannot serve out of Service traffic,
and nothing restarts a process whose HTTP server has hung —
the accepted cost of a probe that cannot restart a healthy replica over a dependency outage.

### Resources

With `pgo.enabled`, the chart derives the container memory limit from the collection ceilings:

```text
maxActiveCollections x (maxParallel x 8 x maxSampleBytes + 2 x 8 x maxMergedBytes)
```

That is the binary's own sizing rule (`profgate config validate` prints it),
so raising a ceiling raises the limit with it.
The chart rejects `pgo.enabled` and these four ceilings when they arrive through the raw `config:` block or `extraEnv`,
so the derivation holds: no escape hatch can change the ceilings out from under the rendered limit,
and every other key stays overridable through both.
At the shipped ceilings the limit is 4Gi.
With PGO off the limit is the static `memoryLimitWithoutPGO`, 512Mi,
which covers the runtime, the informer caches, and the interactive transfer buffers.
An explicit `resources` block replaces both paths and is rendered verbatim.

### Graceful shutdown

On SIGTERM the gateway drains in this order:

1. `/readyz` turns 503 immediately.
2. The gateway waits `server.drainDelay` (default 5s) with the API listener still open,
   the window the EndpointSlice controllers and every kube-proxy get to stop routing new requests here.
   The image is distroless and runs no preStop hook, so the gateway waits in process.
3. Two drains run in parallel:
   the API drain, bounded by the longer of `limits.cpuSeconds` and `limits.traceSeconds` plus 30 seconds,
   and the Collection drain, which waits up to each running Collection's deadline
   and abandons work still running there —
   merge and write cannot be interrupted once entered — logging what it still waits for every 30 seconds.
4. The informers stop, the ops listener drains, and the process exits 0.

A second signal skips the rest of the drain and exits 1.

`terminationGracePeriodSeconds` defaults to 125,
which covers the drain delay plus the longest interactive profile at the shipped limits.
`profgate config validate` prints two grace-period figures:
the base one covers this interactive drain,
and the PGO one is the period that lets a terminating gateway wait through any running Collection's deadline;
work still running at its deadline is abandoned,
so the figure bounds the wait rather than guaranteeing completion.
A shorter grace period is a supported choice, not a misconfiguration:
the kubelet kills the process and the interrupted attempt's samples are discarded.
Another replica reclaims the Collection and retries from round zero,
but only if the lease expires before the Collection's deadline
and an attempt remains under `pgo.maxAttempts`;
otherwise the Collection ends `failed` as `deadline_exceeded` or `attempts_exhausted`,
whichever bound wins.

### Metrics

All metrics are on the ops port at `/metrics`.

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `profgate_requests_total` | counter | `endpoint`, `profile`, `code` | Completed `/v1` requests |
| `profgate_request_duration_seconds` | histogram | `profile` | Duration of completed `/v1` requests |
| `profgate_confirm_total` | counter | `result` | Pod confirmation attempts |
| `profgate_profiles_in_flight` | gauge | | Profile fetches currently in progress |
| `profgate_discovery_synced` | gauge | | 1 when the discovery cache is synced |
| `profgate_collections_total` | counter | `result` | Collections, by terminal result |
| `profgate_collection_samples_total` | counter | `result` | Collection worker samples |
| `profgate_collection_duration_seconds` | histogram | | Duration of completed Collections |
| `profgate_schedule_slots_total` | counter | `result` | Collection scheduling attempts |
| `profgate_sweeper_deletes_total` | counter | `kind` | Sweeper deletions |
| `profgate_collections_active` | gauge | | Collections currently active |
| `profgate_nats_connected` | gauge | | 1 while the NATS connection is up |
| `profgate_tls_reloads_total` | counter | `result` | Certificate load and reload outcomes, the startup load included |
| `profgate_tls_certificate_expiry_seconds` | gauge | | When the served certificate expires, as a Unix timestamp |

### Audit log

Every `/v1` request emits one JSON log record named `request` at info level on completion.
An interactive request carries
`principal`, `namespace`, `service`, `pod`, `profile`, `seconds`, `status`, `code`, and `duration_ms`;
a PGO request carries
`principal`, `namespace`, `service`, `collection`, `method`, `status`, `code`, and `duration_ms`.
The record names the selected Pod or Collection and never the Pod's IP address.

### Smoke test

After installing, verify the gateway answers for a Service in an authorized namespace.
Port-forward the gateway in its own terminal and leave it running:

```bash
kubectl -n profgate port-forward svc/profgate 8080:8080
```

then ask for the Service's targets:

```bash
curl "http://localhost:8080/v1/namespaces/<ns>/services/<svc>/targets"
```

[api.md](api.md) documents the full API.

### NetworkPolicy, disruption budget, and scheduling

`networkPolicy.enabled` (default `false`) renders an ingress policy for the gateway Pods:
`apiFromNamespaces` names the namespaces allowed to reach the API port (default `[ingress-nginx]`),
and `opsFromNamespaces` the namespaces allowed to reach the ops port (default `[monitoring]`).
It is off by default because those namespaces differ per cluster.
The application side needs a matching policy admitting the gateway to its pprof port;
[`deploy/networkpolicy-app-example.yaml`](../deploy/networkpolicy-app-example.yaml) is that policy,
which belongs in the application's namespace rather than in this release.

A PodDisruptionBudget is on by default with `minAvailable: 1`.
To express the budget the other way around, clear one bound and set the other:
`podDisruptionBudget.minAvailable=""` with `podDisruptionBudget.maxUnavailable=1`.
Both `""` and `null` mean unset, and zero is a real bound —
`maxUnavailable: 0` forbids voluntary disruption entirely.
Setting both bounds fails at render, because they express the same budget twice,
and enabling the budget with neither bound fails too.
`nodeSelector`, `tolerations`, `affinity`, and `topologySpreadConstraints` pass through to the pod spec verbatim.

## Upgrades and versioning

A release tag `vX.Y.Z` publishes chart version `X.Y.Z` with `appVersion: vX.Y.Z`,
which is what `image.tag` defaults to — the chart and image versions differ by exactly the leading `v`.
Setting `image.digest` pins the image by digest and wins over any tag,
which is the reproducible way to pin a build.

The upgrade itself:

```bash
helm upgrade profgate oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z \
  --namespace profgate -f values.yaml
kubectl -n profgate rollout status deploy/profgate
```

`helm rollback profgate REVISION -n profgate` returns to the previous release when the new one misbehaves,
with REVISION taken from `helm history profgate -n profgate`.

Upgrades roll with `maxUnavailable: 0` and `maxSurge: 1`:
no running replica leaves before its replacement is Ready.
Combined with the readiness gate above,
this is what makes a NATS outage stall a PGO-enabled upgrade rather than degrade it.

Two releases can share a cluster —
the ClusterRole and ClusterRoleBinding are named after the release, so their RBAC does not collide.

The design of record for everything this guide describes is
[specs/gateway.md](specs/gateway.md) and [specs/pgo.md](specs/pgo.md).
