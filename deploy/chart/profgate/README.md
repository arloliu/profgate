# profgate Helm chart

The gateway as a Helm release:
ServiceAccount, ClusterRole, ClusterRoleBinding, ConfigMap, Deployment, Service,
a PodDisruptionBudget, and an optional NetworkPolicy.

[`../../base`](../../base) is the same deployment as plain YAML with a kustomize base.
Both are shipped surfaces and neither is a copy of the other:
the chart templates what an external operator has to change,
the base is what a repository already using kustomize patches,
and the base's memory limit reserves the PGO-enabled figure with collection off,
where the chart's follows the branch it renders.

Releases go to GHCR as an OCI artifact,
with `image.tag` defaulting to the release the chart was cut from;
[`../../../CHANGELOG.md`](../../../CHANGELOG.md) names what each chart version changed.

```bash
helm install profgate oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z \
  --namespace profgate --create-namespace
```

An unreleased chart installs from a checkout:

```bash
helm install profgate ./deploy/chart/profgate --namespace profgate --create-namespace
```

## Installing from a private network

Two artifacts have to reach the cluster:
the chart, an OCI artifact at `oci://ghcr.io/arloliu/charts/profgate`,
and the image at `ghcr.io/arloliu/profgate`.
Their versions differ by one character.
The release tag `vX.Y.Z` publishes chart version `X.Y.Z`, the tag without its leading `v`,
with `appVersion: vX.Y.Z`, which is what `image.tag` defaults to.

**Egress through a proxy.**
`helm` reads `HTTPS_PROXY` and `NO_PROXY` from the environment for its registry client:

```bash
export HTTPS_PROXY=http://proxy.internal.example:3128
export NO_PROXY=.internal.example,.svc,.cluster.local
helm install profgate oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z \
  --namespace profgate --create-namespace
```

The nodes still pull the image themselves and never see that proxy,
so this is enough only where the container runtime has its own route to `ghcr.io`.

**One transfer, by hand.**
On a connected machine, pull the chart as a file and the image as an archive:

```bash
helm pull oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z
skopeo copy --all docker://ghcr.io/arloliu/profgate:vX.Y.Z \
  oci-archive:profgate-vX.Y.Z.tar
```

That leaves `profgate-X.Y.Z.tgz` and `profgate-vX.Y.Z.tar` to carry across.
`--all` copies the whole index rather than the one manifest matching the machine doing the copy;
the published image is an index over `linux/amd64` and `linux/arm64`,
and without it a cluster on the other architecture gets an image its nodes cannot run.

Inside, push the archive to a registry the cluster can reach and install from the file:

```bash
skopeo copy --all oci-archive:profgate-vX.Y.Z.tar \
  docker://registry.internal.example/profgate:vX.Y.Z
helm install profgate ./profgate-X.Y.Z.tgz \
  --namespace profgate --create-namespace \
  --set image.repository=registry.internal.example/profgate \
  --set imagePullSecrets[0].name=internal-registry
```

Keep the image tag's leading `v` through the copy, or set `image.tag` to whatever it became:
the chart falls back to `appVersion`, which carries the `v`.
The Secret that `imagePullSecrets` names has to exist in the release namespace first;
the chart never creates it, and a registry that allows anonymous pulls needs no `imagePullSecrets` at all.

**A standing mirror.**
Where an internal registry is already the cluster's only source,
mirror both artifacts into it on each release instead of moving files:

```bash
crane copy ghcr.io/arloliu/profgate:vX.Y.Z registry.internal.example/profgate:vX.Y.Z
helm pull oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z
helm push profgate-X.Y.Z.tgz oci://registry.internal.example/charts
```

`crane copy` carries every platform of an index across the way `skopeo copy --all` does.
The chart is an OCI artifact like the image,
so Harbor, GitLab, or any other OCI registry holds it next to the image,
and the install names the mirror on both sides:

```bash
helm install profgate oci://registry.internal.example/charts/profgate --version X.Y.Z \
  --namespace profgate --create-namespace \
  --set image.repository=registry.internal.example/profgate
```

## What the chart guarantees

**A configuration change restarts the Pods.**
The binary reads its configuration once at startup and has no reload,
so the pod template carries `checksum/config`, a hash of the rendered ConfigMap.
Without it a `helm upgrade` that changes only configuration updates the ConfigMap and rolls nothing out.
Set `configChecksumAnnotation: false` to opt out;
configuration changes then take effect at the next unrelated rollout.

**The memory limit is derived, not written down.**
With `pgo.enabled`, `limits.memory` is `memoryLimitWithoutPGO` plus the working set `pgo.limits` sizes:

```text
memoryLimitWithoutPGO + maxActiveCollections x (maxParallel x 8 x maxSampleBytes + 2 x 8 x maxMergedBytes)
```

That is the gateway's own sizing rule
(`internal/config.Config.GatewayMemoryBytes`, which `profgate config validate` prints beside the working set),
so raising a ceiling raises the limit with it and the two cannot drift apart.
What each term buys, and what raising one costs, is the sizing table in
[`docs/configuration.md`](https://github.com/arloliu/profgate/blob/main/docs/configuration.md).
A test renders the chart, loads the rendered ConfigMap through `internal/config`,
and compares the rendered limit against the formula applied to that same configuration.

The working set reads `pgo.limits` and never `pgo.enabled`,
so applying it with collection off would ask for the memory a merge needs on a gateway that never merges.
With `pgo.enabled: false` the limit is `memoryLimitWithoutPGO` on its own, 512Mi,
which covers the runtime, the informer caches, and the transfer buffers of the interactive path.
That term is in the enabled figure too: the gateway does not stop costing it when collection is on.
`resources.limits` overrides both paths and is rendered verbatim,
for a cluster that needs a CPU limit or a memory limit the derivation does not produce.
`resources.requests` is a separate half, rendered as written,
and ships a CPU request of `100m`:
a container with no CPU request is refused outright by a namespace whose `ResourceQuota` counts `requests.cpu`.
There is deliberately no memory request.
Kubernetes copies an unset memory request from the limit, so the Pod already reserves the derived figure,
and a smaller number would let the scheduler place a gateway where the merge that limit is sized for has no room.
Setting `resources.requests.cpu` to `null` removes the request,
the way `podSecurityContext.fsGroup: null` removes that key.
The kustomize base declares no CPU request at all:
no chart value patches it, so a repository installing from the base adds one where its namespace needs it.

**`fsGroup` can be omitted entirely.**
`podSecurityContext.fsGroup` defaults to 65532, the uid and gid the distroless image runs as,
which is what makes the mounted Secrets -- NATS credentials, the TLS key, and authentication files -- readable.
Setting it to `null` renders no pod `securityContext` key at all,
which is what a cluster that assigns its own ranges through a security context constraint needs.
The container `securityContext` is not configurable:
non-root, no privilege escalation, read-only root filesystem, all capabilities dropped.

**Two releases can share a cluster.**
The ClusterRole and ClusterRoleBinding are named after the release,
so a second install does not take over the first one's RBAC.
Their rules are fixed and read-only —
`services` (list, watch), `pods` (get, list, watch), `endpointslices` (list, watch) —
and the chart offers no way to widen them.

## Configuration

The gateway's configuration file is assembled from two places.

Structured values cover the keys the chart itself depends on:
`server.logLevel`, `server.tls`, `auth.anonymousRealm`, `realms`, `ui.enabled`,
`nats.url` and `nats.credsFile`, and `pgo.enabled`, `pgo.configAPI`, and `pgo.limits`.
The chart reads `pgo.limits` for the memory arithmetic and `nats` for the volume it mounts,
so these have to be values rather than opaque text.

Everything else goes in the raw `config` block,
which is merged over the structured keys one key at a time:

```yaml
config:
  discovery:
    pprof:
      port: 6060
      allowedSelections: []
  limits:
    cpuSeconds: 60
    maxConcurrentProfiles: 16
  pgo:
    defaults:
      sampling:
        duration: 30s
```

A key set in both places takes the raw block's value.
A key set in neither takes the binary's own default,
which is why the rendered file is short.

Two families of keys are the exception,
because the Deployment couples them to something else it renders.
The keys the memory limit is derived from —
`pgo.enabled` and the four sizing ceilings under `pgo.limits`
(`maxParallel`, `maxSampleBytes`, `maxMergedBytes`, `maxActiveCollections`) —
are rejected in the raw block and as `PROFGATE_` overrides in `extraEnv`,
because the chart reads them from the `pgo` values before either hatch applies,
and a value arriving there would leave the rendered `limits.memory` sized for different ceilings.
The file-path keys the Secret mounts follow —
`nats.credsFile`, `server.tls.certFile`, and `server.tls.keyFile` —
are rejected in both hatches too,
because the credentials mount follows `nats.credsFile` and the certificate mount follows `tls.enabled`,
so a path arriving there can name a file nothing mounts
and startup validation would end the Pod over it.
Set the `pgo`, `nats`, and `tls` values instead;
everything else, `pgo.configAPI` and `server.tls.minVersion` included,
stays free to override through both hatches.

`server.listen` and `server.opsListen` are rendered by the chart to match the container ports it declares;
overriding them through the raw block moves the listener without moving the readiness probe or the Service.

The binary also applies `PROFGATE_`-prefixed environment overrides on top of the file,
so `extraEnv` changes one key without putting it in the raw config block or the rendered ConfigMap:

```yaml
extraEnv:
  - name: PROFGATE_LOG_LEVEL
    value: debug
```

The overrides land in the pod template,
so changing `extraEnv` rolls the Deployment on upgrade the way any other pod-template change does.

`ui.enabled` turns on the embedded operator console, served at `/ui/`.
It is off by default and restart-class, since it decides which routes the handler registers.
Under `auth.mode: oidc` it needs `auth.oidc.browser`, because a console that cannot log a browser in serves nobody;
`config.Load` refuses the combination without it.
An Ingress that routes only `/v1` has to add `/ui/`, `/auth/`, and `/` once the console is on,
or the console and the sign-in it needs stay unreachable through it.

`ingress.enabled` renders that Ingress rather than leaving it to be written by hand.
It is off by default, because the host, the class, and the annotations differ per cluster,
and turning it on with an empty `ingress.hosts` fails rendering rather than producing an Ingress that routes nothing.
A host that names no `paths` gets all four prefixes the gateway serves —
`/` and `/ui/` for the console, `/auth/` for the browser login it needs, and `/v1/` for the API —
each with `pathType: Prefix` and the Service's `api` port as its backend.
`/` alone would cover the other three; they are listed so that narrowing the set is a visible choice.
The ops port is not in the Service and is never routed here.
`ingress.tls` takes `networking.k8s.io/v1` `IngressTLS` entries as written,
and the chart no more creates their Secret than it creates the one `tls.enabled` mounts.

`ingress.tls` is TLS in front of the Ingress.
With `tls.enabled` the API port serves HTTPS as well,
and a controller reaches it over plain HTTP until it is told otherwise:
ingress-nginx reads `nginx.ingress.kubernetes.io/backend-protocol: HTTPS` from `ingress.annotations`,
and other controllers have their own setting.
Without it the controller speaks HTTP to an HTTPS listener and every request through the Ingress fails.

## Readiness and shutdown

There is no `livenessProbe`, by design.
`/healthz` answers 200 whenever the HTTP server is up and depends on neither Kubernetes nor NATS,
so probing it can only restart a Pod that has stopped answering entirely.
Readiness is the mechanism:
`/readyz` turns 200 once the informer caches have synced,
and, with `pgo.enabled`, once the NATS preflight has passed.

A new Pod therefore cannot become Ready while NATS is down and `pgo.enabled` is set.
The rollout stalls at `maxUnavailable: 0` instead of replacing a working replica with one that cannot collect,
which is the safe failure and does mean a NATS outage blocks upgrades.

`terminationGracePeriodSeconds` defaults to 125,
which covers `server.drainDelay` and then the drain of in-flight profile requests:
the longest of `limits.cpuSeconds` and `limits.traceSeconds`, plus 60 seconds of slack.

`server.drainDelay` is the window between `/readyz` turning 503 and the API listener closing.
The gateway waits it out in process because the image is distroless and has no shell for a `preStop` hook,
and the `sleep` lifecycle action is newer than the Kubernetes baseline the gateway supports.
The ops listener keeps answering `/readyz`, `/healthz`, and `/metrics` for the whole drain.
A Collection can run far longer than that,
and `profgate config validate` prints the period that lets a drain wait through any admissible Collection's deadline;
work still running at that deadline is abandoned,
so the figure bounds the wait rather than guaranteeing completion.
Raising the grace period to that number is a tradeoff rather than a requirement:
a shorter period discards the interrupted attempt's samples;
another replica reclaims the Collection and retries from round zero,
but only if the lease expires before the Collection's deadline
and an attempt remains under `pgo.maxAttempts`;
otherwise the Collection ends `failed` as `deadline_exceeded` or `attempts_exhausted`,
whichever bound wins.

## HTTPS on the API port

`tls.enabled` makes the API port serve HTTPS.
The port and its name do not change, only the scheme:
the Service, the NetworkPolicy, and the container port stay as they are.
The ops port stays plain HTTP either way,
because the readiness probe and the metrics scraper reach it by Pod address,
where a certificate could never be verified.
Off is the shipped default, and means TLS terminating at an Ingress or a mesh.

The chart mounts the `kubernetes.io/tls` Secret named by `tls.existingSecret`,
read-only at `tls.mountPath` with mode 0440,
and renders `server.tls.certFile` and `server.tls.keyFile` to point inside it.
It never creates the Secret.
Unlike the NATS volume the certificate volume is not optional:
`tls.enabled` asserts that the certificate exists,
so a missing Secret holds the Pod at mount time with an event naming it,
rather than starting a Pod that exits over a file it cannot open.

```yaml
tls:
  enabled: true
  existingSecret: profgate-tls
  minVersion: "1.2"
```

**Renewals need no rollout.**
The gateway re-reads both files while it runs and swaps the certificate it serves when their contents change,
so a certificate renewed into the same Secret is served without restarting a Pod.
The pod template deliberately carries no `checksum/tls-secret` beside `checksum/config`:
hashing the Secret would roll the Deployment on every renewal and defeat that.
`profgate_tls_reloads_total` and `profgate_tls_certificate_expiry_seconds` are on the ops port,
and are how a rotation that stopped working becomes visible before the certificate expires.

**With cert-manager**, point a `Certificate` at the same Secret and change nothing else:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: profgate
  namespace: profgate
spec:
  secretName: profgate-tls
  issuerRef:
    name: my-issuer
    kind: ClusterIssuer
  dnsNames:
    - profgate.example.com
```

cert-manager writes `tls.crt` and `tls.key`, which are the keys `tls.certKey` and `tls.keyKey` name,
and renews in place.
The chart renders neither the `Certificate` nor an issuer annotation,
the same posture it takes toward the NATS account and its Secret.

Certificate contents are the only part of *this* configuration that changes without a restart —
*Authentication* below covers the two other files the gateway re-reads while running.
Both TLS paths and `tls.minVersion` are read once at startup,
so changing them rolls the Pods through `checksum/config`, as any other configuration change does.

## Authentication

`auth.mode` selects `disabled` (the shipped default), `basic`, or `oidc`,
each defined in [`../../../docs/specs/auth.md`](../../../docs/specs/auth.md).
The chart renders only the block the mode selects,
and `auth.anonymousRealm` is rejected outside `disabled`,
so a mode change cannot leave a wide-open realm dormant in the rendered configuration.

**Basic mode** compares a request's HTTP Basic credentials against `auth.basic.users`,
a bcrypt hash per user from `profgate auth hash`, never a plaintext password:

```yaml
auth:
  mode: basic
  basic:
    users:
      - name: alice
        passwordHash: "$2a$12$..."
        realm: developer
```

`auth.basic.allowPlaintext` has to be `true` to run Basic without `tls.enabled`,
because Basic sends the password on every request.
A users file, instead of or beside the inline list,
is a Secret data key named by `auth.basic.usersFile`, mounted from `auth.secret`;
see below.

**oidc mode** verifies a bearer token against an issuer:

```yaml
auth:
  mode: oidc
  oidc:
    issuer: https://issuer.example.com/realms/profgate
    audience: profgate
    mapping:
      defaultRealm: developer
```

The optional `auth.oidc.browser` block turns a browser's top-level navigation with no credential into a redirect to a login page,
instead of `401`,
and a successful login into a session cookie;
`curl`, `go tool pprof`, and `fetch`-shaped requests without a credential still receive `401`:

```yaml
auth:
  oidc:
    browser:
      clientID: profgate
      redirectURL: https://profgate.example.com/auth/callback
tls:
  enabled: true
```

`clientID` has to equal `auth.oidc.audience`, and `tls.enabled` is required:
the session cookie carries `Secure` and a `__Host-` prefix, which a plaintext listener cannot set.
Every replica needs the same cookie key, so `auth.oidc.browser` also always needs `auth.secret`; see below.

**The authentication Secret.**
The chart mounts the Secret named by `auth.secret.existingSecret`, `profgate-auth` by default,
read-only at `auth.secret.mountPath` with mode 0440.
`auth.basic.usersFile`, `auth.oidc.caKey`, and `auth.oidc.browser.clientSecretFile`
each name the data key their file appears under inside it.
A cookie key at the fixed data key `cookie.key` is mounted the same way whenever `auth.oidc.browser` is set,
because `auth.oidc.browser.cookieKeyFile` is always required and every replica has to share one file.
Naming a file without turning `auth.secret.enabled` on fails at render time,
because the config would then name a file nothing mounts:

```yaml
auth:
  secret:
    enabled: true
```

The chart never creates this Secret;
[`../../secret-auth-example.yaml`](../../secret-auth-example.yaml) is a commented example, with the
`kubectl create secret generic` command and the `openssl rand -base64 32` line that generates a cookie key.
Unlike the TLS certificate, this Secret is not optional once `auth.secret.enabled` is `true`:
a missing Secret holds the Pod at mount time with an event naming it,
rather than starting a Pod that exits over a file it cannot open.
`basic` mode with neither `auth.basic.users` nor `auth.basic.usersFile` set,
and `oidc` mode with no `auth.oidc.issuer`,
both fail at render time rather than rendering a Pod that exits at startup.
The raw `config:` block is read first for `auth.mode`, `auth.basic.users`, and `auth.oidc.issuer`,
so a value supplied only there is the one judged,
and a value of the wrong type there fails rendering rather than reaching a gateway that cannot decode it.
`auth.basic.usersFile` is not one of them:
the chart refuses `config.auth.basic.usersFile` outright,
because the Secret mount carries the file the structured key names.
Any `PROFGATE_AUTH_`-prefixed entry in `extraEnv` switches both checks off,
because the environment can carry a value the chart cannot see, and startup validation judges it instead.

## NATS credentials

The chart never creates credentials, the NATS account, or the JetStream stores.
With `pgo.enabled` and a non-empty `nats.credsFile` it mounts the Secret named by `nats.existingSecret`,
`profgate-nats-creds` by default,
read-only at `nats.mountPath` with mode 0440.

When `nats.credsFile` is set, create the Secret before turning `pgo.enabled` on.
Startup validation opens the file `nats.credsFile` names,
so a gateway that cannot find it exits and the Pod restarts in a loop.
The volume itself is optional so the Pod can be created before the Secret exists;
once the Secret appears, the kubelet projects it into the volume and the next restart finds the file.
That is a different failure from an unreachable NATS,
where the Pod runs and never becomes Ready.

The credentials file and its Secret belong to a NATS in operator mode.
With server-configuration accounts, the username and password ride in `nats.url` instead,
and a NATS deployment without authentication needs neither.
On both of those paths:

```yaml
nats:
  credsFile: ""
```

mounts nothing and renders no `credsFile` key, so the gateway sends no JWT credentials;
authentication, if any, rides in the URL.

[`../../nats/README.md`](../../nats/README.md) holds the commands
that provision the buckets, the account, and the Secret.

## Scraping

`podMonitor.enabled` renders a prometheus-operator `PodMonitor` for the ops port, which carries `/metrics`.
It is off by default:
it needs the `monitoring.coreos.com` custom resource definitions installed
and a Prometheus whose `podMonitorSelector` matches it,
and in a cluster without them it is an object nothing reads.
`podMonitor.labels` is how that selector finds it —
`release: kube-prometheus-stack` is what the kube-prometheus-stack chart looks for.

It selects Pods rather than a Service because the ops port is deliberately absent from the Service:
nothing routes to it, and the kubelet and the scraper reach it by Pod address.
The endpoint therefore names the container port the Deployment declares rather than a Service port.
With `networkPolicy.enabled`, `networkPolicy.opsFromNamespaces` has to name the namespace the scraper runs in,
or the scrape is refused before it reaches the port.

`prometheusRule.enabled` renders a `PrometheusRule` with alerts over metrics the gateway already exports,
three always and a fourth only when `pgo.enabled`:

| Alert | Expression | Fires when |
|---|---|---|
| `ProfgateNotReady` | `profgate_discovery_synced == 0` | A replica's discovery cache has been unsynced for ten minutes, so `/readyz` answers 503 |
| `ProfgateAdmissionSaturated` | `sum(rate(profgate_requests_total{code="too_many_profiles"}[5m])) > 0` | The admission gate is at `limits.maxConcurrentProfiles` and answering 429 |
| `ProfgateOIDCKeysStale` | `profgate_oidc_jwks_age_seconds > 43200` | Signing keys have not been fetched for 12 hours |
| `ProfgatePGONotSynced` | `profgate_pgo_synced == 0` | The watched PGO caches have been unsynced for ten minutes, so the process decides nothing from them and every PGO route on a gateway replica is refusing |

Every one reads the ops port, so they need something scraping it — `podMonitor.enabled`, or a scrape configured by hand.
The stale-keys threshold is half of the binary's own `auth.oidc.jwksMaxStale` default of 24 hours,
which is when verification starts failing as `keys_stale`,
so the alert arrives while tokens still verify.
The chart does not render that key and cannot follow a deployment that lowers it;
`prometheusRule.rules` replaces the shipped set outright for that case,
and for any other rule set an operator would rather ship.

## Values

| Key | Default | What it does |
|---|---|---|
| `image.repository`, `image.tag`, `image.digest`, `image.pullPolicy` | `ghcr.io/arloliu/profgate`, appVersion, none, `IfNotPresent` | The image. A digest wins over a tag. |
| `imagePullSecrets` | `[]` | Registry credentials. |
| `nameOverride`, `fullnameOverride` | `""` | The generated resource names. |
| `replicaCount` | `2` | Replicas, primarily for availability; more replicas also add aggregate capacity, since the admission gate and the PGO limits are per replica. |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, generated, `{}` | The ServiceAccount. |
| `rbac.create` | `true` | The ClusterRole and ClusterRoleBinding. |
| `service.type`, `.port`, `.annotations` | `ClusterIP`, `8080`, `{}` | The Service. The ops port stays out of it. |
| `ingress.enabled`, `.className`, `.annotations`, `.hosts`, `.tls` | `false`, `""`, `{}`, `[]`, `[]` | An Ingress for the API port. A host that names no `paths` gets `/`, `/ui/`, `/auth/`, and `/v1/`, each `pathType: Prefix`. Enabling it with no hosts fails rendering. |
| `podDisruptionBudget.enabled`, `.minAvailable`, `.maxUnavailable` | `true`, `1`, unset | Voluntary disruption budget. Set exactly one of the two bounds and clear the other (`""` and `null` both mean unset); zero is a real bound (`maxUnavailable: 0` forbids voluntary disruption). Setting both fails rendering, as does enabling the budget with neither set. |
| `configChecksumAnnotation` | `true` | The `checksum/config` annotation. |
| `podSecurityContext` | `{fsGroup: 65532}` | Pod security context; `fsGroup: null` renders no key. |
| `securityContext` | hardened | Container security context. |
| `resources.limits`, `.requests` | unset, `{cpu: 100m}` | `limits` replaces the derived memory limit; `requests` is rendered as written. No memory request, so it keeps tracking the limit. |
| `memoryLimitWithoutPGO` | `512Mi` | The memory limit while `pgo.enabled` is false. A whole number of `Mi` or `Gi`; anything else fails rendering, on both branches. |
| `terminationGracePeriodSeconds` | `125` | Drain time before SIGKILL. |
| `readinessProbe` | 10s period | Probe timings. There is no liveness probe. |
| `extraEnv` | `[]` | `PROFGATE_`-prefixed overrides and anything else. |
| `podAnnotations`, `podLabels` | `{}` | Extra pod metadata. |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | empty | Scheduling. |
| `networkPolicy.enabled`, `.apiFromNamespaces`, `.opsFromNamespaces` | `false`, `[ingress-nginx]`, `[monitoring]` | Ingress policy for the gateway Pods. |
| `podMonitor.enabled`, `.interval`, `.labels` | `false`, `30s`, `{}` | A prometheus-operator PodMonitor for the ops port. |
| `prometheusRule.enabled`, `.labels`, `.rules` | `false`, `{}`, `[]` | A prometheus-operator PrometheusRule. Empty `rules` keeps the shipped alerts, a fourth of them only when `pgo.enabled`; a non-empty list replaces them. |
| `server.logLevel` | `info` | `debug`, `info`, `warn`, or `error`. |
| `server.drainDelay` | `5s` | The wait between `/readyz` turning 503 and the API listener closing. |
| `auth.mode` | `disabled` | `disabled`, `basic`, or `oidc`. |
| `auth.anonymousRealm` | `developer` | The realm every request gets while `auth.mode` is `disabled`. |
| `auth.basic.users`, `.usersFile`, `.allowPlaintext`, `.maxConcurrent` | `[]`, `""`, `false`, `16` | Basic mode's user set; `usersFile` names a Secret data key. Basic mode with both empty fails rendering. |
| `auth.oidc.issuer`, `.audience`, `.tokenType`, `.usernameClaim`, `.groupsClaim`, `.caKey`, `.httpProxy` | empty, empty, `id`, `sub`, `groups`, `""`, `""` | oidc mode's issuer and how it reads a token; `caKey` names a Secret data key. oidc mode with no issuer fails rendering. |
| `auth.oidc.mapping.users`, `.groups`, `.defaultRealm` | `[]`, `[]`, `""` | How a verified token maps to a realm. |
| `auth.oidc.browser` | `{}` | The relying-party block that turns a login into a session cookie; empty renders no `auth.oidc.browser` block. See *Authentication*. |
| `auth.oidc.cli.enabled` | `true` | Renders the `auth.oidc.cli` block, whose presence makes `GET /v1/auth` report a device login. Omitted, with a notice in the install notes, when `auth.oidc.browser.clientSecretFile` is set under `tokenType: id`, the pair the binary refuses. |
| `auth.oidc.cli.clientID` | `""` | The client identifier a command-line client sends; rendered only when set. The gateway defaults it to `auth.oidc.audience`, which it must equal under `tokenType: id`. |
| `auth.oidc.cli.scopes` | `[]` | The scopes the client requests; rendered only when set. The gateway defaults them to `openid, offline_access`. |
| `auth.oidc.cli.pkce` | `null` | The operator's assertion that the issuer's device endpoint accepts a PKCE challenge, which discovery does not answer; rendered only when set, and read as `false` when it is not. |
| `auth.secret.enabled`, `.existingSecret`, `.mountPath` | `false`, `profgate-auth`, `/etc/profgate/auth` | The Secret the files above are read from. |
| `realms` | one wide-open realm | What each principal may reach. |
| `ui.enabled` | `false` | The embedded operator console, served at `/ui/`. Restart-class; under `auth.mode: oidc` it requires `auth.oidc.browser`. |
| `tls.enabled`, `.existingSecret`, `.certKey`, `.keyKey`, `.mountPath`, `.minVersion` | `false`, `profgate-tls`, `tls.crt`, `tls.key`, `/etc/profgate/tls`, `1.2` | HTTPS on the API port, from a Secret the operator creates. |
| `nats.url`, `.credsFile`, `.existingSecret`, `.secretKey`, `.mountPath` | empty, `/etc/profgate/nats/nats.creds`, `profgate-nats-creds`, `nats.creds`, `/etc/profgate/nats` | NATS, used only with `pgo.enabled`. |
| `pgo.enabled`, `.configAPI`, `.limits` | `false`, `enabled`, shipped ceilings | PGO collection and the ceilings the memory limit is derived from. |
| `config` | `discovery.pprof` holding `allowedSelections: []` | Raw configuration merged over everything above. `allowedSelections` is default-deny: the empty list admits only the configured `port` or `portName`, and a user's list replaces it entry for entry, `{port: N}`, `{portName: name}`, or a `"*"` wildcard of either kind. |

The NetworkPolicy is off by default because the namespaces that reach the two ports differ per cluster.
[`../../networkpolicy-app-example.yaml`](../../networkpolicy-app-example.yaml)
is the matching policy an application namespace needs to admit the gateway to its pprof port;
the chart does not render it, because it belongs to the application's namespace rather than to this release.
