# Configuration Reference

Profgate reads one YAML file, named by the `--config` flag — the only flag the binary takes.
The file is loaded once at startup; there is no hot reload.
The Helm chart gets the same effect by default:
it annotates the Pod template with a checksum of the rendered configuration,
so a configuration change rolls the Deployment,
and `configChecksumAnnotation: false` opts out (see [`deployment.md`](deployment.md)).
The one exception is the TLS certificate pair:
the files named by `server.tls` are re-read from disk, so rotating a certificate needs no restart.

A key the schema does not define is rejected at any nesting depth,
and the process fails at startup naming the file.
A typo therefore surfaces as a startup failure, never as a silently ignored setting.
Validation failures behave the same way: every error names the offending key.

The design rationale behind these settings lives in [`specs/gateway.md`](specs/gateway.md) and,
for the `pgo` and `nats` sections, [`specs/pgo.md`](specs/pgo.md).

## Environment Overrides

Most scalar keys can be overridden by an environment variable:
`PROFGATE_` followed by the name listed in each table below.
An environment variable beats the file.
The names are flat — nesting does not add underscores beyond what the table shows —
so `server.logLevel` is `PROFGATE_LOG_LEVEL` and `discovery.pprof.port` is `PROFGATE_PPROF_PORT`.

Two sections deliberately have no environment overrides: `realms` and `pgo.defaults`.
They are policy, and policy is reviewed in the file, not injected around it.

## `server`

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `listen` | `PROFGATE_LISTEN` | `:8080` | `host:port`; must differ from `opsListen` |
| `opsListen` | `PROFGATE_OPS_LISTEN` | `:9090` | `host:port` |
| `logLevel` | `PROFGATE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `drainDelay` | `PROFGATE_DRAIN_DELAY` | `5s` | `0` to `60s` |

`listen` is the API listener, the one clients profile through;
`opsListen` serves health, readiness, and metrics and the two must be different addresses.

`drainDelay` is the window between `/readyz` turning 503 and the API listener closing,
so the EndpointSlice controllers and every kube-proxy have time to stop routing to a terminating replica.
Zero turns the wait off, which suits local runs and tests.
The delay is part of the required grace period that
[`config validate`](#profgate-config-validate) prints.

### `server.tls`

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `certFile` | `PROFGATE_TLS_CERT_FILE` | unset | set together with `keyFile`; readable at startup |
| `keyFile` | `PROFGATE_TLS_KEY_FILE` | unset | set together with `certFile`; readable at startup |
| `minVersion` | `PROFGATE_TLS_MIN_VERSION` | `1.2` | `1.2` or `1.3` |

There is no `enabled` flag:
naming both files is what turns the API listener into an HTTPS listener,
and a block naming neither is the plaintext default.
Naming one file without the other is a validation error,
and both files are opened at startup
so a path typo fails there rather than surfacing as failed handshakes.
The pair is re-read from disk after startup, so certificate rotation needs no restart.
The ops listener has no TLS block and is always plaintext.

## `discovery`

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `versionLabel` | `PROFGATE_VERSION_LABEL` | `app.kubernetes.io/version` | a valid Kubernetes label key |
| `pprof.port` | `PROFGATE_PPROF_PORT` | see below | `1` to `65535` |
| `pprof.portName` | `PROFGATE_PPROF_PORT_NAME` | unset | a valid container-port name |

Exactly one of `pprof.port` and `pprof.portName` may be set;
naming both is a validation error.
When the file sets neither, `pprof.port` defaults to `6060` —
the default exists only while `portName` is empty.

`port` connects to the same numbered port on every eligible Pod.
`portName` instead resolves a named TCP container port per Pod,
which suits fleets where the pprof port number varies by workload;
a Pod whose spec does not carry the named port is silently ineligible rather than an error.
There is no port allowlist beyond this single setting:
the one configured port, by number or by name, is the only port the gateway connects to.

## `limits`

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `cpuSeconds` | `PROFGATE_LIMIT_CPU_SECONDS` | `60` | `1` to `86400` |
| `traceSeconds` | `PROFGATE_LIMIT_TRACE_SECONDS` | `60` | `1` to `86400` |
| `maxConcurrentProfiles` | `PROFGATE_LIMIT_MAX_CONCURRENT_PROFILES` | `16` | `1` to `1024` |

`cpuSeconds` and `traceSeconds` cap the `seconds` parameter of CPU and trace profile requests.
`maxConcurrentProfiles` is the admission gate on in-flight profile fetches per replica,
and PGO sampling passes through the same gate as interactive requests.
Like every other key, these change only by restart.

## `auth`

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `mode` | `PROFGATE_AUTH_MODE` | `disabled` | `disabled` is the only value |
| `anonymousRealm` | `PROFGATE_ANONYMOUS_REALM` | — | required; must name a key under `realms` |

`disabled` is the only authentication mode: every request is attributed to the anonymous principal
and evaluated against the realm `anonymousRealm` names.
Startup logs a warning that access is controlled only by the network boundary and static realm policy —
reaching the listener at all is the credential,
so put a NetworkPolicy or equivalent in front of it.

## `nats`

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `url` | `PROFGATE_NATS_URL` | unset | comma-separated `nats://` or `tls://` URLs |
| `credsFile` | `PROFGATE_NATS_CREDS_FILE` | unset | readable at startup when set |
| `connectTimeout` | `PROFGATE_NATS_CONNECT_TIMEOUT` | `5s` | `1s` to `60s` |

NATS holds the PGO control-plane stores and nothing on the interactive path.
`url` and `credsFile` are validated only when `pgo.enabled` is true —
a gateway that never collects reaches no NATS cluster and needs none configured —
and `url` is then required.
`connectTimeout` bounds the initial connection attempt and its range holds either way.

## `pgo`

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `enabled` | `PROFGATE_PGO_ENABLED` | `false` | |
| `configAPI` | `PROFGATE_PGO_CONFIG_API` | `enabled` | `enabled` or `disabled` |
| `leaseTTL` | `PROFGATE_PGO_LEASE_TTL` | `60s` | `30s` to `10m` |
| `maxAttempts` | `PROFGATE_PGO_MAX_ATTEMPTS` | `3` | `1` to `10` |
| `jobRetention` | `PROFGATE_PGO_JOB_RETENTION` | `168h` | at most `2160h`; see cross-key rules |

`enabled: false` — the shipped state — runs the gateway with no PGO subsystem at all.
`configAPI: disabled` keeps collection running but rejects policy writes through the API,
freezing policy at what `pgo.defaults` and the stores already hold.
`leaseTTL` is how long a replica may hold a Collection before another may reclaim it,
`maxAttempts` caps a Collection's total attempts (the first plus at most `maxAttempts - 1` retries),
and `jobRetention` is how long finished Collection records stay readable.
How Collections behave is [`pgo.md`](pgo.md)'s subject.

The `pgo.limits` and `pgo.defaults` blocks below are always validated, even with `enabled: false`,
so a block that contradicts itself fails at startup rather than on the day collection is turned on.

### `pgo.limits`

Ceilings every effective policy is measured against,
whether the policy comes from `pgo.defaults` or from a write through the Collection API —
the same value is judged the same way from either source.

| Key | Environment variable | Default | Constraints |
|---|---|---|---|
| `maxDuration` | `PROFGATE_PGO_LIMIT_MAX_DURATION` | `60s` | at least `1s` |
| `maxRounds` | `PROFGATE_PGO_LIMIT_MAX_ROUNDS` | `5` | `1` to `20` |
| `maxParallel` | `PROFGATE_PGO_LIMIT_MAX_PARALLEL` | `4` | `1` to `64` |
| `minEvery` | `PROFGATE_PGO_LIMIT_MIN_EVERY` | `15m` | at least `1m` |
| `maxEvery` | `PROFGATE_PGO_LIMIT_MAX_EVERY` | `24h` | at most `24h` |
| `maxRetention` | `PROFGATE_PGO_LIMIT_MAX_RETENTION` | `24h` | `1m` to `720h` |
| `maxSampleBytes` | `PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES` | `33554432` | `1048576` to `268435456` |
| `maxMergedBytes` | `PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES` | `67108864` | at most `1073741824` |
| `maxTargetsPerRound` | `PROFGATE_PGO_LIMIT_MAX_TARGETS_PER_ROUND` | `32` | `1` to `256` |
| `maxActiveCollections` | `PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS` | `2` | at least `1` |
| `onDemandPerMinute` | `PROFGATE_PGO_LIMIT_ON_DEMAND_PER_MINUTE` | `10` | `1` to `600` |
| `maxLiveCollections` | `PROFGATE_PGO_LIMIT_MAX_LIVE_COLLECTIONS` | `64` | `1` to `1024` |

These ceilings size the container:
the memory a replica needs at the configured ceilings is

```text
maxActiveCollections × (maxParallel × 8 × maxSampleBytes + 2 × 8 × maxMergedBytes)
```

which is 4 GiB at the shipped defaults — the figure the Helm chart renders as the memory limit,
and the third line `config validate` prints.
The factor 8 estimates how much heap a decoded profile occupies against its encoded length;
it is a sizing rule, not a proof.

### `pgo.defaults`

The policy a Service gets before any override, with no environment overrides.
Each value must obey the matching `pgo.limits` ceiling — the cross-key rules below.

| Key | Default | Constraints |
|---|---|---|
| `schedule.every` | `6h` | between `minEvery` and `maxEvery` |
| `schedule.jitter` | `10m` | at most half of `schedule.every` |
| `sampling.duration` | `30s` | at least `1s`; at most `maxDuration` |
| `sampling.rounds` | `2` | at least `1`; at most `maxRounds` |
| `sampling.roundInterval` | `30s` | `0` to `10m` |
| `sampling.replicas` | `all` | `all`, or a count from `1` to `maxTargetsPerRound` |
| `sampling.maxParallel` | `4` | at least `1`; at most `pgo.limits.maxParallel` |
| `target.versionPolicy` | `strict` | `strict` is the only value |
| `artifact.retention` | `2h` | at least `1m`; at most `maxRetention` |

`replicas: all` samples every eligible Pod, up to `maxTargetsPerRound` per round.
`versionPolicy: strict` requires every sampled Pod to carry the same value of
`discovery.versionLabel`, so a merged profile never mixes binary versions.

## `realms`

`realms` is a required map with at least one entry;
each key is a realm name,
and each realm says what the requests attributed to it may reach.

| Key | Default | Constraints |
|---|---|---|
| `namespaces` | — | required, at least one entry; each a DNS-1123 label or `"*"` |
| `services` | — | required, at least one entry; each a DNS-1123 label or `"*"` |
| `profiles` | — | required, at least one entry; each a profile name or `"*"` |
| `pgo.read` | `false` | |
| `pgo.collect` | `false` | |
| `pgo.configure` | `false` | |

The eight profile names are
`cpu`, `trace`, `heap`, `allocs`, `goroutine`, `mutex`, `block`, and `threadcreate`.

A request is evaluated in order: namespace, then Service, then the route's own check —
the `profiles` list for a profile fetch, the matching `pgo` flag for a PGO route.
Every list matches by the exact string or by `"*"`; there is no prefix or glob matching.
A realm name the configuration does not hold denies, so a bad lookup fails closed rather than open.

The `pgo` flags gate the PGO routes of [`api.md`](api.md):
`read` for reading policies, Collections, and their profiles;
`collect` for creating or cancelling a Collection;
`configure` for writing a policy.
A realm without a `pgo` block has every flag false and reaches no PGO route.

Writing a realm well means starting narrow:
name the namespaces and Services a team actually owns, list only the profiles they use,
and grant `pgo.configure` to few realms —
it is the flag that changes standing behavior rather than reading or triggering it.
`"*"` in `namespaces` or `services` grants everything the gateway's own Kubernetes permissions can see,
so a wildcard realm is as broad as the ServiceAccount behind the gateway.

## Cross-Key Validation

Rules the per-key ranges cannot express, checked at startup.
Always, whatever `pgo.enabled` says:

- `server.opsListen` must differ from `server.listen`.
- `server.tls.certFile` and `server.tls.keyFile` are set together or not at all,
  and both files must be readable.
- Exactly one of `discovery.pprof.port` and `discovery.pprof.portName` is set.
- `auth.anonymousRealm` must name a key under `realms`.
- `pgo.limits.maxRounds × pgo.limits.maxTargetsPerRound` must be at most `256`.
- `pgo.jobRetention` must be at least `pgo.limits.maxRetention + 1h`.
- `pgo.limits.minEvery` must be at most `pgo.limits.maxEvery`.
- `pgo.limits.maxSampleBytes` must be at most `pgo.limits.maxMergedBytes`.
- Every `pgo.defaults` value must obey its ceiling, as the `pgo.defaults` table lists.

Only when `pgo.enabled` is true:

- `nats.url` is required, and every comma-separated entry must begin with `nats://` or `tls://`;
  `nats.credsFile`, when set, must be readable.
- `pgo.limits.maxParallel × pgo.limits.maxActiveCollections` must stay strictly below `limits.maxConcurrentProfiles`,
  so scheduled sampling can never fill the admission gate that interactive requests share.
- `pgo.limits.maxDuration` must be at most `limits.cpuSeconds`,
  because a Collection sample is an ordinary CPU profile fetch and passes the same duration cap.

## `profgate config validate`

```console
$ profgate config validate --config /etc/profgate/config.yaml
required terminationGracePeriodSeconds: 125
required terminationGracePeriodSeconds for pgo: 122465
  the worst case over every policy pgo.limits admits: drain waits through each Collection's deadline and abandons work still running there;
  a shorter period discards the interrupted attempt's samples: another replica retries from round zero only if the lease expires before the Collection's deadline and an attempt remains (pgo.maxAttempts); otherwise the Collection fails as deadline_exceeded or attempts_exhausted
pgo memory bytes: 4294967296
```

The command loads the file exactly as `serve` would — defaults, environment overrides,
normalization, every validation rule — and exits `2` with the failing key on any error.
On success it prints three deployment figures:

- The required `terminationGracePeriodSeconds` with PGO off:
  `server.drainDelay`, plus the longer of `limits.cpuSeconds` and `limits.traceSeconds`,
  plus 60 seconds of slack.
- The variant with PGO on: the worst case over every policy `pgo.limits` admits.
  It is large by construction,
  and is the period that lets a drain wait through any admissible Collection's deadline:
  drain waits for running Collection work up to each Collection's deadline
  and abandons what is still running there,
  because the merge and write steps cannot be interrupted —
  the figure bounds the wait, it does not guarantee completion.
  A shorter period is a supported choice with a different outcome:
  the process is killed and the interrupted attempt's samples are discarded.
  Another replica reclaims the Collection and retries from round zero,
  but only if the lease (`pgo.leaseTTL`) expires before the Collection's deadline
  and an attempt remains under `pgo.maxAttempts`;
  otherwise the Collection ends `failed` as `deadline_exceeded` or `attempts_exhausted`,
  whichever bound wins.
- The PGO memory bytes figure from the formula under [`pgo.limits`](#pgolimits).

The other subcommands are `profgate version`, which prints the build version, and
`profgate serve --config <path>`, which runs the gateway.
Exit codes: `2` for a usage error or an invalid configuration, `1` for a fatal runtime error,
`0` for a clean drain after SIGTERM or SIGINT.
A second SIGTERM or SIGINT during the drain exits `1` immediately,
cutting requests and leaving Collections for another replica.

## Examples

The smallest valid configuration — every other key takes its default,
including `discovery.pprof.port: 6060`:

```yaml
auth:
  anonymousRealm: developer
realms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
```

A complete configuration with PGO collection on,
adapted from the shipped [`configmap.yaml`](../deploy/base/configmap.yaml):

```yaml
server:
  listen: ":8080"
  opsListen: ":9090"
  logLevel: info
  drainDelay: 5s
  tls:
    certFile: /etc/profgate/tls/tls.crt
    keyFile: /etc/profgate/tls/tls.key
    minVersion: "1.2"
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
nats:
  url: nats://nats.profgate.svc:4222
  credsFile: /etc/profgate/nats/nats.creds
  connectTimeout: 5s
pgo:
  enabled: true
  configAPI: enabled
  leaseTTL: 60s
  maxAttempts: 3
  jobRetention: 168h
  limits:
    maxDuration: 60s
    maxRounds: 5
    maxParallel: 4
    minEvery: 15m
    maxEvery: 24h
    maxRetention: 24h
    maxSampleBytes: 33554432
    maxMergedBytes: 67108864
    maxTargetsPerRound: 32
    maxActiveCollections: 2
    onDemandPerMinute: 10
    maxLiveCollections: 64
  defaults:
    schedule:
      every: 6h
      jitter: 10m
    sampling:
      duration: 30s
      rounds: 2
      roundInterval: 30s
      replicas: all
      maxParallel: 4
    target:
      versionPolicy: strict
    artifact:
      retention: 2h
realms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
    pgo:
      read: true
      collect: true
  platform:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["cpu", "heap", "goroutine"]
    pgo:
      read: true
      collect: true
      configure: true
```

Every value under `pgo.limits` and `pgo.defaults` above is the shipped default,
written out so the file shows the full shape;
omitting any of them loads the same configuration.
