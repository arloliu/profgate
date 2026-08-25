# profgate Helm chart

The gateway as a Helm release:
ServiceAccount, ClusterRole, ClusterRoleBinding, ConfigMap, Deployment, Service,
a PodDisruptionBudget, and an optional NetworkPolicy.

[`../../base`](../../base) is the same deployment as plain YAML with a kustomize base.
Both are shipped surfaces and neither is a copy of the other:
the chart templates what an external operator has to change,
the base is what a repository already using kustomize patches.

Releases go to GHCR as an OCI artifact,
with `image.tag` defaulting to the release the chart was cut from:

```bash
helm install profgate oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z \
  --namespace profgate --create-namespace
```

An unreleased chart installs from a checkout:

```bash
helm install profgate ./deploy/chart/profgate --namespace profgate --create-namespace
```

## What the chart guarantees

**A configuration change restarts the Pods.**
The binary reads its configuration once at startup and has no reload,
so the pod template carries `checksum/config`, a hash of the rendered ConfigMap.
Without it a `helm upgrade` that changes only configuration updates the ConfigMap and rolls nothing out.
Set `configChecksumAnnotation: false` to opt out;
configuration changes then take effect at the next unrelated rollout.

**The memory limit is derived, not written down.**
With `pgo.enabled`, `limits.memory` is computed from `pgo.limits`:

```text
maxActiveCollections x (maxParallel x 8 x maxSampleBytes + 2 x 8 x maxMergedBytes)
```

That is the gateway's own sizing rule
(`internal/config.Config.PGOMemoryBytes`, which `profgate config validate` prints),
so raising a ceiling raises the limit with it and the two cannot drift apart.
The ceilings answer to each other as well:
`maxParallel` times `maxActiveCollections` has to stay below `limits.maxConcurrentProfiles`,
which the chart does not render and which defaults to 16,
so raising either one far enough means raising `config.limits.maxConcurrentProfiles` in the raw block with it.
That check lives in the binary, so getting it wrong is a startup failure rather than a render failure.
A test renders the chart, loads the rendered ConfigMap through `internal/config`,
and compares the rendered limit against the formula applied to that same configuration.

The formula reads `pgo.limits` and never `pgo.enabled`,
so applying it with collection off would ask for the memory a merge needs on a gateway that never merges.
With `pgo.enabled: false` the limit is the static `memoryLimitWithoutPGO`, 512Mi,
which covers the runtime, the informer caches, and the transfer buffers of the interactive path.
`resources` overrides both paths and is rendered verbatim,
for a cluster whose quota or LimitRange demands requests or a CPU limit.

**`fsGroup` can be omitted entirely.**
`podSecurityContext.fsGroup` defaults to 65532, the uid and gid the distroless image runs as,
which is what makes the NATS credentials Secret readable.
Setting it to `null` renders no pod `securityContext` key at all,
which is what a cluster that assigns its own ranges through a security context constraint needs.
The container `securityContext` is not a knob:
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
`server.logLevel`, `auth.anonymousRealm`, `realms`,
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

`server.listen` and `server.opsListen` are rendered by the chart to match the container ports it declares;
overriding them through the raw block moves the listener without moving the readiness probe or the Service.

The binary also applies `PROFGATE_`-prefixed environment overrides on top of the file,
so `extraEnv` changes one key without re-rendering:

```yaml
extraEnv:
  - name: PROFGATE_LOG_LEVEL
    value: debug
```

An override set that way is invisible to the ConfigMap checksum,
so changing it does not restart Pods on its own.

## Readiness and shutdown

There is no `livenessProbe`, by design.
`/healthz` answers 200 whenever the HTTP server is up and depends on neither Kubernetes nor NATS,
so probing it can only restart a Pod that has stopped answering entirely.
Readiness is the mechanism:
`/readyz` turns 200 once the informer caches have synced,
and, with `pgo.enabled`, once the NATS stores have opened.

A new Pod therefore cannot become Ready while NATS is down and `pgo.enabled` is set.
The rollout stalls at `maxUnavailable: 0` instead of replacing a working replica with one that cannot collect,
which is the safe failure and does mean a NATS outage blocks upgrades.

`terminationGracePeriodSeconds` defaults to 120,
which covers the drain of in-flight profile requests:
the longest of `limits.cpuSeconds` and `limits.traceSeconds`, plus 60 seconds of slack.
A Collection can run far longer than that,
and `profgate config validate` prints the period that lets every Collection the ceilings admit finish in place.
Raising the grace period to that number is a tradeoff rather than a requirement:
a Collection the kubelet kills mid-merge loses no work,
its lease expires and another replica reclaims it.

## NATS credentials

The chart never creates credentials, the NATS account, or the JetStream stores.
With `pgo.enabled` it mounts the Secret named by `nats.existingSecret`, `profgate-nats-creds` by default,
read-only at `nats.mountPath` with mode 0440.

Create the Secret before turning `pgo.enabled` on.
Startup validation opens the file `nats.credsFile` names,
so a gateway that cannot find it exits and the Pod restarts in a loop.
The volume itself is optional, which is what lets the Secret be replaced under a running Pod.
That is a different failure from an unreachable NATS,
where the Pod runs and never becomes Ready.

[`../../nats/README.md`](../../nats/README.md) holds the commands
that provision the buckets, the account, and the Secret.

## Values

| Key | Default | What it does |
|---|---|---|
| `image.repository`, `image.tag`, `image.digest`, `image.pullPolicy` | `ghcr.io/arloliu/profgate`, appVersion, none, `IfNotPresent` | The image. A digest wins over a tag. |
| `imagePullSecrets` | `[]` | Registry credentials. |
| `nameOverride`, `fullnameOverride` | `""` | The generated resource names. |
| `replicaCount` | `2` | Replicas, for availability during a rollout. |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, generated, `{}` | The ServiceAccount. |
| `rbac.create` | `true` | The ClusterRole and ClusterRoleBinding. |
| `service.type`, `.port`, `.annotations` | `ClusterIP`, `8080`, `{}` | The Service. The ops port stays out of it. |
| `podDisruptionBudget.enabled`, `.minAvailable`, `.maxUnavailable` | `true`, `1`, unset | Voluntary disruption budget. |
| `configChecksumAnnotation` | `true` | The `checksum/config` annotation. |
| `podSecurityContext` | `{fsGroup: 65532}` | Pod security context; `fsGroup: null` renders no key. |
| `securityContext` | hardened | Container security context. |
| `resources` | `{}` | Explicit resources, replacing the derived limit. |
| `memoryLimitWithoutPGO` | `512Mi` | The memory limit while `pgo.enabled` is false. |
| `terminationGracePeriodSeconds` | `120` | Drain time before SIGKILL. |
| `readinessProbe` | 10s period | Probe timings. There is no liveness probe. |
| `extraEnv` | `[]` | `PROFGATE_`-prefixed overrides and anything else. |
| `podAnnotations`, `podLabels` | `{}` | Extra pod metadata. |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | empty | Scheduling. |
| `networkPolicy.enabled`, `.apiFromNamespaces`, `.opsFromNamespaces` | `false`, `[ingress-nginx]`, `[monitoring]` | Ingress policy for the gateway Pods. |
| `server.logLevel` | `info` | `debug`, `info`, `warn`, or `error`. |
| `auth.anonymousRealm` | `developer` | The realm every request gets while authentication is disabled. |
| `realms` | one wide-open realm | What each principal may reach. |
| `nats.url`, `.credsFile`, `.existingSecret`, `.secretKey`, `.mountPath` | empty, `/etc/profgate/nats/nats.creds`, `profgate-nats-creds`, `nats.creds`, `/etc/profgate/nats` | NATS, used only with `pgo.enabled`. |
| `pgo.enabled`, `.configAPI`, `.limits` | `false`, `enabled`, shipped ceilings | PGO collection and the ceilings the memory limit is derived from. |
| `config` | `{}` | Raw configuration merged over everything above. |

The NetworkPolicy is off by default because the namespaces that reach the two ports differ per cluster.
[`../../base/networkpolicy-app-example.yaml`](../../base/networkpolicy-app-example.yaml)
is the matching policy an application namespace needs to admit the gateway to its pprof port;
the chart does not render it, because it belongs to the application's namespace rather than to this release.
