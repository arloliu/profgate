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

The base creates the `profgate` namespace,
so the command above applies on a cluster that has never heard of Profgate.
`kubectl delete -k deploy/base` therefore removes the namespace and everything in it;
a repository whose namespaces are managed elsewhere removes `namespace.yaml` from `kustomization.yaml`'s `resources`.
The base also hard-codes plain HTTP and PGO off;
a repository using it is expected to patch the ConfigMap and Deployment with overlays of its own.
The base pins a released image tag, which a checkout does not update;
set the version this repository runs, or an image digest.
The base applies only the gateway's own resources —
its Namespace, ServiceAccount, ClusterRole and binding, ConfigMap, Deployment, Service,
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
  What a client may name instead is bounded by `discovery.pprof.allowedSelections`,
  and the shipped manifests leave that list empty,
  so a client may name only the configured default until the operator lists more.

With the chart, realms and `auth.anonymousRealm` are structured values,
and the pprof port goes in the raw `config` block:

```yaml
config:
  discovery:
    pprof:
      port: 6060
```

[configuration.md](configuration.md) documents every key the file accepts.

### The console

`ui.enabled` (default `false`) turns on the embedded operator console at `/ui/`;
[console.md](console.md) covers what it shows and how to sign in.
An Ingress that routes only `/v1/` needs `/ui/`, `/auth/`, and `/` added to it once the console is on,
or the console and the sign-in it needs stay unreachable through it;
the chart's own Ingress routes all four by default.
Nothing else about the deployment changes: the console adds no listener, no volume, and no RBAC tuple,
and its assets are compiled into the same binary and served from the same API port.

### Ingress

`ingress.enabled` (default `false`) renders an Ingress for the API port.
It is off by default because the host, the class, and the annotations differ per cluster,
and turning it on with an empty `ingress.hosts` fails rendering rather than producing an Ingress that routes nothing:

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: profgate.example.com
  tls:
    - secretName: profgate-ingress-tls
      hosts:
        - profgate.example.com
```

A host that names no `paths` gets the four prefixes the gateway serves —
`/` and `/ui/` for the console, `/auth/` for the browser login it needs, and `/v1/` for the API —
each with `pathType: Prefix` and the Service's `api` port as its backend.
`/` alone would cover the other three; they are listed so that narrowing the set is a visible choice.
The ops port is not in the Service and is never routed here.
The kustomize base ships no Ingress:
a base that named a host nobody has would apply as a route to nothing.

`ingress.tls` is TLS in front of the Ingress.
With `tls.enabled` the API port serves HTTPS as well,
and a controller reaches it over plain HTTP until it is told otherwise:
ingress-nginx reads `nginx.ingress.kubernetes.io/backend-protocol: HTTPS` from `ingress.annotations`,
and other controllers have their own setting.
Without it the controller speaks HTTP to an HTTPS listener and every request through the Ingress fails.

## RBAC

The chart and the base both create a ClusterRole with fixed, read-only rules:

| Resource | Verbs |
|---|---|
| `services` | list, watch |
| `pods` | get, list, watch |
| `endpointslices` (`discovery.k8s.io`) | list, watch |

That is the permission invariant:

> Profgate requires no Kubernetes write permissions.
> It observes Services, Pods, and EndpointSlices cluster-wide,
> and serves each caller only the namespaces, Services, and profiles that caller's realm admits.
> It connects to the configured pprof port of a Pod,
> and to any port or port name `discovery.pprof.allowedSelections` admits,
> by an exact entry or by a wildcard, wherever NetworkPolicy permits the connection.
> It manipulates only its dedicated `PROFGATE_*` NATS stores.

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
(`server.logLevel`, `auth.anonymousRealm`, `realms`, `tls`, `nats`, `pgo`, `ui`);
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
The expiry threshold is inert until a certificate is served:
an install without `server.tls` reads `NaN`, not an expired-at-the-epoch value that a rule could cross,
and the reload counter has no series at all on such an install.

Two details worth knowing:

- `podSecurityContext.fsGroup: 65532` (the default) makes the 0440 key file readable by the distroless image;
  a cluster that assigns its own ranges must arrange that differently.
- The ops listener stays plaintext either way:
  the kubelet's probe and the metrics scraper reach it by Pod address,
  where a certificate could never be verified.

## Authentication secrets

`auth.mode` (`disabled`, `basic`, or `oidc`) and the two credentialed modes are
[`specs/auth.md`](specs/auth.md)'s subject and [`configuration.md`](configuration.md#auth)'s field reference;
this section covers what the chart mounts and what an operator provisions.

Basic mode's inline `auth.basic.users` needs nothing beyond the ConfigMap the gateway already reads.
A users file, an issuer CA certificate, a browser client secret, or a cookie key each name a Secret data key instead,
so that material never lands in the ConfigMap:

```yaml
auth:
  secret:
    enabled: true
    existingSecret: profgate-auth
    mountPath: /etc/profgate/auth
  basic:
    usersFile: users.yaml       # Secret data key, not a path
  oidc:
    caKey: issuer-ca.crt
    browser:
      clientSecretFile: client-secret
```

The chart mounts that Secret read-only at `auth.secret.mountPath` with mode 0440
and derives each file path by joining the mount path with the data key:
`auth.basic.usersFile`, `auth.oidc.caFile`, and `auth.oidc.browser.clientSecretFile` this way,
and `auth.oidc.browser.cookieKeyFile` the same way at the fixed key `cookie.key`
whenever `auth.oidc.browser` is set, because every replica has to share one cookie key file.
The chart never creates this Secret;
[`deploy/secret-auth-example.yaml`](../deploy/secret-auth-example.yaml) is a commented manifest with the
`kubectl create secret generic` command and the `openssl rand -base64 32` line that generates a cookie key.
Like the TLS certificate and unlike the NATS credentials, it is not optional once `auth.secret.enabled` is
`true`: a missing Secret holds the Pod at mount time with an event naming it,
rather than starting a Pod that exits over a file it cannot open.

**oidc mode reaches the issuer.**
With `auth.mode: oidc`, the gateway makes outbound HTTPS requests to the issuer:
for discovery, its signing keys, and, with the browser flow, the token endpoint.
A NetworkPolicy that restricts the gateway's egress needs a rule for it,
alongside DNS, the Kubernetes API, the application pprof ports, and NATS when `pgo.enabled`;
`deploy/chart/profgate/values.yaml`'s `networkPolicy` block carries the commented example:

```yaml
egress:
  - to:
      - ipBlock:
          cidr: <the issuer's IP address or CIDR>/32
    ports:
      - protocol: TCP
        port: 443
```

**Cookie key rotation is staged.**
Every replica polls `cookieKeyFile` on its own 30-second cycle,
so writing a single new key straight over the old one loses a session on any replica that has not re-read yet:
it keeps sealing with a key another replica has already stopped accepting.
Rotate in five steps instead, from [`specs/auth.md`](specs/auth.md) *Cookie key*:

1. Write `old,new` — every replica learns to open `new` while still sealing with `old`.
2. Wait until `profgate_auth_cookie_key_info{fingerprint,role}` on every replica reports both fingerprints.
3. Write `new,old` — replicas begin sealing with `new`; `old` still opens.
4. Wait one `sessionTTL` after every replica reports `new` as `role="current"`.
5. Write `new` alone.

`fingerprint` is the first 8 hex digits of `SHA-256(key)`,
so the metric confirms propagation without reading key material off any replica.

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
with `auth.mode: oidc` issuer discovery and the initial signing-key fetch have succeeded,
and, with PGO enabled, the NATS preflight has passed.
A later NATS disconnect does not turn readiness off:
the replica keeps serving interactive requests and answers the PGO routes 503.
It does abort every Collection that replica owns:
each aborted attempt is spent against `pgo.maxAttempts`,
and [Troubleshooting](#troubleshooting) says what that costs.

With `auth.mode: oidc`, startup passes through a `[discovering]` state between opening the listeners
and the Kubernetes preflight:
both listeners are up and `/healthz` is already 200,
but `/readyz` stays 503 until discovery and the first key fetch succeed,
logged as `issuer discovered; starting preflight`.
A gateway that cannot reach its issuer within `auth.oidc.discoveryTimeout` exits,
rather than serve `503` to every request while looking otherwise healthy;
the log line is `issuer discovery failed`.

There is no livenessProbe, by design.
`/healthz` answers 200 whenever the HTTP server is up and depends on neither Kubernetes nor NATS,
so a liveness probe on it could detect nothing but an HTTP server that has stopped answering entirely.
The tradeoff is deliberate: readiness takes a Pod that cannot serve out of Service traffic,
and nothing restarts a process whose HTTP server has hung —
the accepted cost of a probe that cannot restart a healthy replica over a dependency outage.

### Resources

With `pgo.enabled`, the container memory limit is derived from `memoryLimitWithoutPGO` and the ceilings:

```text
memoryLimitWithoutPGO + maxActiveCollections x (maxParallel x 8 x maxSampleBytes + 2 x 8 x maxMergedBytes)
```

That is the binary's own sizing rule (`profgate config validate` prints both halves),
so raising a ceiling raises the limit with it.
The chart rejects `pgo.enabled` and these four ceilings when they arrive through the raw `config:` block or `extraEnv`,
so the derivation holds: no escape hatch can change the ceilings out from under the rendered limit,
and every other key stays overridable through both.
At the shipped ceilings the working set is 1Gi and the limit is 1536Mi;
[`configuration.md`](configuration.md) has the table of what each term buys and what raising one costs.
With PGO off the limit is `memoryLimitWithoutPGO` alone, 512Mi,
which covers the runtime, the informer caches, and the interactive transfer buffers —
the same term the enabled figure carries, because the gateway still costs it while it collects.
The chart derives its limit from the branch it renders, so a chart install with PGO off gets 512Mi.
The kustomize base ships collection off and reserves 512Mi,
so uncommenting the ConfigMap's PGO block means raising the limit too,
to the figure `config validate` prints for the edited file.
An explicit `resources.limits` replaces both paths and is rendered verbatim.

`resources.requests` is rendered as written, and ships a CPU request of `100m`.
A container with no CPU request at all is refused outright by a namespace whose `ResourceQuota` counts `requests.cpu`,
which is what made such a namespace need the escape hatch to install.
There is deliberately no memory request:
Kubernetes copies an unset memory request from the limit,
so the Pod already reserves the derived figure,
and a smaller number would let the scheduler place a gateway where the merge that limit is sized for has no room.

### Graceful shutdown

On SIGTERM the gateway drains in this order:

1. `/readyz` turns 503 immediately.
2. The gateway waits `server.drainDelay` (default 5s) with the API listener still open,
   the window the EndpointSlice controllers and every kube-proxy get to stop routing new requests here.
   The image is distroless and runs no preStop hook, so the gateway waits in process.
3. Two drains run in parallel:
   the API drain, bounded by the longer of `limits.cpuSeconds` and `limits.traceSeconds` plus 30 seconds,
   and the Collection drain, which stops renewing the lease on every Collection this replica owns
   and returns once each owner has committed or reached the cutoff of the lease it last renewed —
   `pgo.leaseTTL` minus five seconds of clock skew.
   Merge and write cannot be interrupted once entered,
   so a work goroutine may outlive the drain; it commits nothing, because its lease has lapsed.
4. The informers stop, the ops listener drains, and the process exits 0.

A second signal skips the rest of the drain and exits 1.

`terminationGracePeriodSeconds` defaults to 125,
which covers the drain delay plus the longest interactive profile at the shipped limits.
`profgate config validate` prints that one grace-period figure, and enabling PGO does not raise it.
A terminating gateway stops renewing the lease on every Collection it owns
and waits only until each owner has committed or reached the cutoff of the lease it last renewed,
which is `pgo.leaseTTL` minus five seconds of clock skew.
An interrupted Collection is reclaimed by another replica and retried from round zero,
as long as an attempt remains under `pgo.maxAttempts`;
otherwise it ends `failed` as `attempts_exhausted`,
or as `deadline_exceeded` when the deadline fixed at the first claim passes first.

### Metrics

All metrics are on the ops port at `/metrics`.

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `profgate_requests_total` | counter | `endpoint`, `profile`, `code` | Completed `/v1` requests |
| `profgate_request_duration_seconds` | histogram | `profile` | Duration of completed `/v1` requests |
| `profgate_confirm_total` | counter | `result` | Pod confirmation attempts, on the interactive profile path |
| `profgate_profiles_in_flight` | gauge | | Profile fetches currently in progress, on the interactive profile path |
| `profgate_discovery_synced` | gauge | | 1 once the initial discovery sync has completed; never back to 0 |
| `profgate_collections_total` | counter | `result` | Collections, by terminal result |
| `profgate_collection_samples_total` | counter | `result` | Collection worker samples |
| `profgate_collection_duration_seconds` | histogram | | Duration of completed Collections |
| `profgate_schedule_slots_total` | counter | `result` | Collection scheduling attempts |
| `profgate_sweeper_deletes_total` | counter | `kind` | Sweeper deletions |
| `profgate_collections_active` | gauge | | Collections currently active |
| `profgate_nats_connected` | gauge | | 1 while the NATS connection is up; `0` under `pgo.enabled`, from the start of the NATS preflight and before the first attempt; `NaN` where the process makes none |
| `profgate_pgo_synced` | gauge | | 1 while every watch has replayed the current store generation and every cache has applied it |
| `profgate_tls_reloads_total` | counter | `result` | Certificate load and reload outcomes, the startup load included |
| `profgate_tls_certificate_expiry_seconds` | gauge | | When the served certificate expires, as a Unix timestamp; `NaN` until one is loaded |
| `profgate_auth_failures_total` | counter | `mode`, `reason` | Authentication failures answered `401`, `429`, or `503`; a redirect is not a failure |
| `profgate_auth_sessions_issued_total` | counter | | Browser sessions minted |
| `profgate_oidc_jwks_refresh_total` | counter | `result` | Signing key fetches |
| `profgate_oidc_jwks_keys` | gauge | | Usable signing keys currently held |
| `profgate_oidc_jwks_age_seconds` | gauge | | Seconds since the last successful key fetch; `NaN` before the first — the alertable form of `keys_stale` |
| `profgate_auth_file_reload_total` | counter | `file` (`users`/`cookie_key`), `result` | Re-reads of the users file or the cookie key file |
| `profgate_auth_cookie_key_info` | gauge | `fingerprint`, `role` (`current`/`previous`) | One series per loaded cookie key, always `1` |

`profgate_requests_total`'s `endpoint` label also carries `namespaces`, `services`, `whoami`, and `limits` for the four listing routes, and `ui` for `/ui/`, every path under it, and `/`;
`profile` is `none` for all five.
`ui`'s `code` is one of `ok`, `route_unknown`, `method_not_allowed`, or `internal_error` —
a closed set derived from the status the console wrote, not the raw HTTP status.

The `code` label's value set is closed apart from one family, so a query can match it on an exact value.
Forty values are registered envelope codes;
[specs/gateway.md](specs/gateway.md) and [specs/pgo.md](specs/pgo.md) *Errors* hold the tables that name them,
and `/v1/openapi.json` serves the same set.
`collector_unavailable` is one of the forty and no route in this build answers it, so it never appears on a series.
Eight more values reach this label and the audit log but never an error envelope:
`ok`, `upstream_stream_failed`, `internal_error`, `auth_redirect`,
`cas_contended`, `artifact_stream_failed`, `client_gone`, and `cancelled`.
`auth_redirect` is the browser navigation sent to login;
it is not a failure and is not counted in `profgate_auth_failures_total`.
The last family, `upstream_<status>`, is minted from any upstream response of `400` and above,
so its members are HTTP statuses rather than a fixed list.
`profgate_confirm_total` and `profgate_profiles_in_flight` count the interactive profile path alone:
a Collection's own confirmation attempts are counted by neither one,
and a Collection's worker samples are counted instead by `profgate_collection_samples_total`.

`podMonitor.enabled` (default `false`) renders a prometheus-operator `PodMonitor` for that port.
It is off by default because it needs the `monitoring.coreos.com` custom resource definitions
and a Prometheus whose `podMonitorSelector` matches it;
`podMonitor.labels` is how that selector finds it,
and `podMonitor.interval` (default `30s`) is the scrape interval.
It selects Pods rather than a Service, because the ops port is absent from the Service by design,
so the endpoint names the container port rather than a Service port.
With `networkPolicy.enabled`, `opsFromNamespaces` has to name the namespace the scraper runs in,
or the scrape is refused before it reaches the port.
The rendered `PodMonitor` drops the `endpoint` target label prometheus-operator writes from that port name,
so the `endpoint` label in the table above reaches a query under that name instead of `exported_endpoint`;
`up` and the other per-scrape series carry no `endpoint` as a result.

`prometheusRule.enabled` (default `false`) renders a `PrometheusRule` over metrics from the table above,
eight rules always and two more when `pgo.enabled`:

| Alert | Expression | Fires when |
|---|---|---|
| `ProfgateNotReady` | `profgate_discovery_synced == 0` | A replica has not completed its initial informer sync for ten minutes, so `/readyz` answers 503. The gauge is also `0` while an unreachable issuer or a failing Kubernetes preflight keeps the informers from starting, and it never returns to `0`, so it does not report a replica that goes unready later |
| `ProfgateAdmissionSaturated` | `sum(rate(profgate_requests_total{code="too_many_profiles"}[5m])) > 0` | The admission gate is at `limits.maxConcurrentProfiles` and answering 429 |
| `ProfgateOIDCKeysStale` | `profgate_oidc_jwks_age_seconds > 43200` | Signing keys have not been fetched for 12 hours |
| `ProfgateAuthLimiterSaturated` | `sum without (endpoint, profile, code) (rate(profgate_requests_total{code="too_many_auth"}[5m])) > 0` | A replica has answered at least one authentication attempt with 429 `too_many_auth` in every five-minute window for ten minutes. Only the bcrypt gate at `auth.basic.maxConcurrent` answers that code |
| `ProfgateAuthUnavailable` | `sum without (endpoint, profile, code) (rate(profgate_requests_total{code="auth_unavailable"}[5m])) > 0` | A replica has answered at least one request with 503 `auth_unavailable` in every five-minute window for ten minutes. `profgate_auth_failures_total` by `reason` says which callers are refused: `keys_stale` refuses every token, `entropy` and `exchange` stop the browser login alone |
| `ProfgateUpstreamsUnreachable` | `sum without (endpoint, profile, code) (rate(profgate_requests_total{code=~"upstream_unreachable\|upstream_timeout"}[5m])) > 0 unless sum without (endpoint, profile, code) (rate(profgate_requests_total{code="ok",profile!="none",endpoint!~"collection.*"}[5m])) > 0` | For fifteen minutes a replica has answered profile requests at the dial or the deadline and served none successfully in the same window, which is a NetworkPolicy that closed the pprof port or a `discovery.pprof.port` no workload listens on rather than one Pod that died |
| `ProfgateTLSReloadFailing` | `sum without (result) (rate(profgate_tls_reloads_total{result="failed"}[15m])) / sum without (result) (rate(profgate_tls_reloads_total[15m])) > 0.9` | More than nine in ten of a replica's certificate re-reads over fifteen minutes failed, so a renewal into the Secret is not reaching it and it still serves the pair it last applied |
| `ProfgateTLSCertificateExpiring` | `profgate_tls_certificate_expiry_seconds - time() < 604800` | The certificate a replica serves stops being valid in under seven days, and the rule keeps firing once it has expired |
| `ProfgateNATSDisconnected` | `profgate_nats_connected == 0` | The NATS transport has been down on a replica for five minutes. At startup that is the only alert that fires, because `profgate_pgo_synced` does not exist until the preflight passes |
| `ProfgatePGONotSynced` | `profgate_pgo_synced == 0` | The watched PGO caches have been unsynced for ten minutes, the window `ProfgateNotReady` already gives discovery, so the process decides nothing from them and every PGO route on a gateway replica is refusing |

Three of them are inert where the feature they watch is off:
the two certificate rules read series only `server.tls` produces,
and `ProfgateAuthLimiterSaturated` reads a code only `auth.mode: basic` answers with.
`ProfgateUpstreamsUnreachable` produces no sample while no profile request is served,
so an outage that starts in a quiet period waits for traffic.
Every one reads the ops port, so they need something scraping it.
The stale-keys threshold is half of the binary's `auth.oidc.jwksMaxStale` default of 24 hours,
which is when verification starts failing as `keys_stale`;
`prometheusRule.rules` replaces the shipped set outright for a deployment that lowers that key,
since the chart does not render it and cannot follow it.

### Example queries

One query per question an operator asks of the series above.
Each is checked in as a rule file `promtool` parses in the test suite,
so a query here has been read as PromQL before a reader copies it.
There is no dashboard file to import:
a query goes into whatever the operator already runs.

**The interactive path.**

| Question | Query |
|---|---|
| Request rate by route family | `sum by (endpoint) (rate(profgate_requests_total[5m]))` |
| Share of requests that are not `ok` | `sum(rate(profgate_requests_total{code!="ok"}[5m])) / sum(rate(profgate_requests_total[5m]))` |
| 95th percentile profile fetch | `histogram_quantile(0.95, sum by (le, profile) (rate(profgate_request_duration_seconds_bucket[5m])))` |
| Profile fetches in flight, fleet-wide | `sum(profgate_profiles_in_flight)` |
| The ten refusals with the most volume | `topk(10, sum by (code) (rate(profgate_requests_total{code!="ok"}[1h])))` |

**The certificate.**

| Question | Query |
|---|---|
| Days left on each replica's served certificate | `(profgate_tls_certificate_expiry_seconds - time()) / 86400` |
| Re-read outcomes by result | `sum by (result) (rate(profgate_tls_reloads_total[15m]))` |

**Authentication.**

| Question | Query |
|---|---|
| Failures by reason | `sum by (reason) (rate(profgate_auth_failures_total[5m]))` |
| Signing key age per replica | `profgate_oidc_jwks_age_seconds` |
| Browser sessions minted | `sum(rate(profgate_auth_sessions_issued_total[1h]))` |

**Readiness.**

| Question | Query |
|---|---|
| Replicas that have completed their initial sync | `count(profgate_discovery_synced == 1) or vector(0)` |
| Replicas exporting the gauge at all, which is the denominator for the row above | `count(profgate_discovery_synced)` |
| Replicas with no NATS connection | `count(profgate_nats_connected == 0) or vector(0)` |
| Replicas whose PGO caches have not replayed | `count(profgate_pgo_synced == 0) or vector(0)` |

**Collection.**

| Question | Query |
|---|---|
| Collections by terminal result | `sum by (result) (rate(profgate_collections_total[1h]))` |
| Scheduling outcomes | `sum by (result) (rate(profgate_schedule_slots_total[1h]))` |
| Collections running now | `sum(profgate_collections_active)` |
| Sample outcomes | `sum by (result) (rate(profgate_collection_samples_total[15m]))` |
| 95th percentile Collection duration | `histogram_quantile(0.95, sum by (le) (rate(profgate_collection_duration_seconds_bucket[1h])))` |

Six things about the queries above, which is what makes them readable rather than only copyable:

- Every series the gateway exports is per replica, because every one is scraped per Pod.
  A query with no aggregation answers per replica, which is what the certificate and key-age rows want;
  `sum(...)` or `sum by (<label>)` answers for the fleet, which is what every rate row wants.
  Drop the `sum` to see which replica a number came from.
- The error-share query reads as a gap wherever there is nothing to divide, never as a healthy `0`.
  A gateway that answered every request as asked exports no series matching `code!="ok"`,
  so the numerator has no sample and neither does the ratio.
  Where the refusal counters exist but neither side moved in the window, the ratio is `0/0`, which is `NaN`.
  Both draw as a break in the line, so read this query beside the request-rate one rather than alone.
- `histogram_quantile` needs the `le` label,
  so a histogram is summed by `le` alongside whatever else the row groups by.
  Aggregating a `_bucket` series without `le` produces a number that means nothing.
- `count(<gauge> == 0)` produces no sample when nothing matches, which is again a gap rather than a zero.
  `or vector(0)` is what turns "nothing is broken" into a zero a panel can draw,
  and it is why the readiness rows carry it.
- `code!="ok"` counts the `upstream_<status>` values,
  which are a target's own answers passed through rather than the gateway refusing anything.
- `profgate_confirm_total` and `profgate_profiles_in_flight` count the interactive profile path alone,
  as the metric table above says.

The `endpoint` label is the one label a scrape can take away,
and the request-rate query is the only one here that reads it.
The chart's `PodMonitor` drops the target label of that name so the gateway's value keeps it,
and that query is written for that install.
A scrape configured by hand that attaches an `endpoint` target label of its own, with `honorLabels` unset,
moves the gateway's value to `exported_endpoint`;
group by `exported_endpoint` there instead.
`ProfgateUpstreamsUnreachable` loses the `endpoint!~"collection.*"` half of its expression on such an install,
so a replica answering Collection routes while every profile fetch fails does not raise it.

### Audit log

Every `/v1` request emits one JSON log record named `request` at info level on completion.
Requests under `/ui/` and to `/` write no audit line:
they carry no principal and name nothing a realm bounds, and one page load is several of them.
Every record opens with `requestId`, the value the response's `X-Request-Id` header carried,
which is what joins a client's report of a request to the gateway's record of it.
An interactive request then carries
`principal`, `namespace`, `service`, `pod`, `profile`, `seconds`, `port`, `status`, `code`, and `duration_ms`;
a PGO request carries
`principal`, `namespace`, `service`, `collection`, `method`, `status`, `code`, and `duration_ms`.
`port` is the client's port selection as sent, a number or a name, empty when absent;
for a name it is never the number the name resolved to.
A targets request that asked for the exclusion counts adds `explain`,
and a Collection read whose wait the gateway accepted adds `wait`, the duration it asked to be held open for.
The record names the selected Pod or Collection and never the Pod's IP address.

An authentication failure adds `auth_reason` — one of the reasons in
[`specs/auth.md`](specs/auth.md#7-audit-and-metrics), such as `bad_credential`, `expired`, or `no_realm` —
and `principal` is `-`, because no principal was resolved.
The three `/auth/` routes write their own record, carrying only
`principal` (the resolved principal on a successful callback, `-` for login and logout),
`route` (`auth_login`, `auth_callback`, or `auth_logout`), `method`, `status`, `code`, and `duration_ms`,
with no namespace, Service, or Pod.
A browser navigation sent to `/auth/login` from `/v1` — no credential, or an expired session — is not counted
as a failure: it carries `code auth_redirect` and status `302`.

### Troubleshooting

Each row is a symptom, the series or the log line that shows it, and the step.
The same behavior is stated by [specs/gateway.md](specs/gateway.md#13-failure-scenarios)
and [specs/pgo.md](specs/pgo.md#122-health).

| Symptom | Where it shows | Step |
|---|---|---|
| `/readyz` 503 from startup and never green | `profgate_discovery_synced` reads `0`, and the log repeats `preflight attempt` with `error` and `retry_in`, or `still waiting for informer caches to sync` with `elapsed` | The Kubernetes API is unreachable, or the informers have not finished their first list. The preflight retries forever and the process stays up, so reaching the API is the only thing that ends this |
| the process exits at startup naming a resource and a verb | one `preflight forbidden; the ClusterRole lacks a tuple` record at error, carrying `resource` and `verb`: the preflight received `403` | Add that tuple to the ClusterRole; [RBAC](#rbac) has the shipped set |
| `/readyz` 503 under `auth.mode: oidc`, with no `issuer discovered; starting preflight` line | issuer discovery is still retrying; both listeners are up and `/healthz` already answers 200 | The process exits with `issuer discovery failed` once `auth.oidc.discoveryTimeout` passes. Check that the issuer is reachable and that `auth.oidc.caFile` trusts its chain |
| `/readyz` 503 under `pgo.enabled`, with `nats preflight attempt` repeating | the connection is unavailable and `profgate_nats_connected` reads `0`. `ProfgateNATSDisconnected` is what fires; `ProfgatePGONotSynced` cannot, because `profgate_pgo_synced` does not exist until the preflight passes | Only a connection failure retries, and it retries for as long as it lasts. Fix the transport |
| the process exits at startup after one `nats preflight failed` record at error | a missing bucket, a bucket of the wrong kind, a configuration outside the contract, or a probe the account is denied; none of those retries | The error names the bucket and the operation or the field. [NATS for PGO collection](#nats-for-pgo-collection) has the buckets and the permissions they need |
| every profile request answers `502` or `504` | `profgate_requests_total{code="upstream_unreachable"}` or `{code="upstream_timeout"}` climbing with no `code="ok"` beside it. `ProfgateUpstreamsUnreachable` | A NetworkPolicy that closed the pprof port, or a `discovery.pprof.port` no workload listens on. One dead Pod raises neither: the rule needs a replica that served no profile request successfully in the same window |
| profile requests answer `429` | `profgate_requests_total{code="too_many_profiles"}`. `ProfgateAdmissionSaturated` | `limits.maxConcurrentProfiles` is the admission gate and it counts per replica; the gateway never queues. Raise it or add replicas |
| a renewed certificate is not being served | `profgate_tls_reloads_total{result="failed"}` climbing while `profgate_tls_certificate_expiry_seconds` does not move. `ProfgateTLSReloadFailing` | The pair last applied stays in use, so no connection drops. The files are unreadable, or the certificate and the key do not match |
| `profgate_tls_certificate_expiry_seconds` reads `NaN` | no certificate has been loaded | `server.tls` is not configured. Expected on a plaintext listener, where both certificate rules are inert by construction |
| PGO routes answer `503 pgo_unavailable` while `/readyz` is green | `profgate_pgo_synced` reads `0`. `ProfgatePGONotSynced` | A store whose watches the seam cannot re-open. Readiness is right to stay green, because interactive profiling is unaffected. A deleted or recreated bucket and a restore from backup are how a store gets there |
| a Collection ends `failed` with reason `attempts_exhausted` around a NATS outage | `profgate_collections_total{result="failed"}`, and the `reason` field of the record | Every disconnect aborts the Collections each replica owns and spends an attempt, so `pgo.maxAttempts` of them inside one Collection's deadline end it — three at the default |

**A bucket deleted or recreated under a running process.**
The process stays up, the seam retries the re-open until the bucket exists again,
`/readyz` stays green, and every PGO route refuses for as long as that lasts
([specs/pgo.md](specs/pgo.md#122-health)).
`profgate_pgo_synced` is where it shows and `ProfgatePGONotSynced` is what fires.
Recreating the bucket under the same name is enough; no restart is needed.
The log writes one record when a re-open starts failing and one when every watch is open and has replayed again,
and nothing per retry,
so a bucket absent for an hour leaves two records rather than hundreds.

**A restore from backup.**
A restore replaces the buckets, which is the deleted-or-recreated case:
the watches on them close, the store generation moves,
and every replica reads `profgate_pgo_synced` `0` until its watches have replayed under the new generation
([specs/pgo.md](specs/pgo.md#122-health)).
Records that come back from a backup carry leases and deadlines stamped before the restore.
A Collection whose lease has lapsed is reclaimed by the ordinary scan as a new attempt and costs one;
a Collection whose deadline has passed is failed `deadline_exceeded` by that scan rather than resumed.

**NATS maintenance.**
Plan it as an outage that costs attempts, not as a pause.
A disconnect of any length aborts the Collections each replica owns.

**What a disconnect costs a running Collection.**
An owner takes its store view once, at claim time.
Any move of the store generation — a disconnect, or a watch cut under a live connection —
makes every later call on that view unavailable, so the owner never renews its lease again
([specs/pgo.md](specs/pgo.md#84-the-owner-loop)).
It stops once `pgo.leaseTTL` minus five seconds of clock skew has passed since its last successful renewal,
and commits nothing.
How long the outage lasts decides how long the owner takes to notice, not whether its attempt survives:
a disconnect of any length ends that attempt.
Another replica reclaims the record and retries from round zero,
so each disconnect spends one attempt against `pgo.maxAttempts`.
That key is what bounds the exhaustion: it runs from `1` to `10` and defaults to `3`,
so `pgo.maxAttempts` disconnects inside one Collection's deadline end it `failed attempts_exhausted`.

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
A client may name the port it wants
([`port` and `portName` on the profile and targets routes](api.md#query-parameters));
a client may name the configured default and any selection `discovery.pprof.allowedSelections` admits,
exactly or by wildcard,
and only under a wildcard is this NetworkPolicy the bound on which Pod ports the gateway reaches.

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

`0.5.0` carries five breaking changes; [`../CHANGELOG.md`](../CHANGELOG.md) has all five.
The one that meets an operator without any other change is port selection:
`discovery.pprof.allowedPorts` and `allowedPortNames` are removed,
with their `PROFGATE_*` variables,
and a configuration that still sets either one does not start,
with a message naming the replacement.
The single list `discovery.pprof.allowedSelections` replaces them.
An empty `allowedSelections` — the shipped default — admits only the configured `port` or `portName`,
where an empty old allowlist admitted anything.
`- port: "*"`, `- portName: "*"`, or both restore that open behavior, one wildcard per old list.

Upgrades roll with `maxUnavailable: 0` and `maxSurge: 1`:
no running replica leaves before its replacement is Ready.
Combined with the readiness gate above,
this is what makes a NATS outage stall a PGO-enabled upgrade rather than degrade it.

Two releases can share a cluster —
the ClusterRole and ClusterRoleBinding are named after the release, so their RBAC does not collide.

The design of record for everything this guide describes is
[specs/gateway.md](specs/gateway.md) and [specs/pgo.md](specs/pgo.md).
