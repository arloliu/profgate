# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.0] - 2026-08-26

Documents the project for public use, hardens the chart against values it cannot honor,
and corrects guidance the code contradicted.

### Added

- **A documentation set for public use.**
  A root README positions the gateway and walks a Helm quickstart to a first profile;
  four guides cover the API, the configuration file, deployment, and PGO collection end to end;
  this changelog backfills the released versions;
  and an Apache-2.0 license states the terms the published image and chart carry.

- **Chart rendering fails on values it cannot honor.**
  The container memory limit is derived from the PGO ceilings,
  so overriding one of those keys through the raw config block or `extraEnv` now fails rendering,
  naming the supported values key,
  and an explicit `null` at `config.pgo` or `config.pgo.limits` — which would bypass that guard — fails too.
  The same guard covers the file-path keys the Deployment's Secret mounts are built from,
  because a raw or environment override there points the config at files nothing mounts;
  such an override fails rendering rather than shipping a crash-looping Pod,
  a `nats.credsFile` that does not name the file the Secret mount provides fails the same way,
  and a raw `config.nats.url` now satisfies the URL requirement the way the docs promise.
  A PodDisruptionBudget with both bounds set, or with neither, no longer ships a budget the operator did not ask for:
  rendering fails and names the fix,
  and `maxUnavailable: 0` counts as a set bound rather than reading as unset.

- **The informer sync wait logs its progress.**
  A Pod waiting for the Kubernetes informer caches warns every 15 seconds with the elapsed time,
  so each of the readiness waits now names itself in the logs.

### Changed

- **NATS credentials are optional in the chart.**
  The chart mounted the credentials Secret and rendered `credsFile` whenever PGO was enabled,
  though with an empty `credsFile` the binary skips the JWT credentials file.
  Both now render only when `nats.credsFile` is non-empty,
  so `nats.credsFile: ""` skips the Secret mount and authentication, if any, rides in the URL.

### Fixed

- **The application NetworkPolicy example left `deploy/base`.**
  `kubectl apply -k deploy/base` also applied the example into the target namespace,
  touching any workload matching its selector.
  It now lives at `deploy/` beside the Secret examples,
  as a template to customize per application namespace.

- **`config validate` states what a short PGO grace period costs.**
  The output said a shorter grace period loses no work;
  a drain waits through each Collection's deadline and abandons work still running there,
  a cut attempt's samples are dropped,
  and another replica retries only while the deadline and an attempt remain —
  otherwise the Collection fails as `deadline_exceeded` or `attempts_exhausted`.

- **The chart's install notes print API paths that exist.**
  The curl examples used `/v1/targets?namespace=<ns>&service=<svc>`, which matches no route;
  they now use `/v1/namespaces/<ns>/services/<svc>/targets`.

## [0.2.0] - 2026-08-25

Puts HTTPS on the API listener, with a certificate that rotates without a restart,
and makes a shutdown drain everything it promises to drain.

### Added

- **HTTPS on the API listener.**
  `server.tls` names a certificate and a key,
  and when both are set the API listener serves HTTPS on the same port under the same name.
  The two paths are set together or not at all,
  because half a pair would leave a listener that fails every handshake,
  and both files are opened at startup so a path typo names its key instead of surfacing per connection.
  `minVersion` accepts 1.2 or 1.3 and defaults to 1.2.
  Leaving the block unset keeps the listener plaintext for an Ingress or a mesh terminating TLS,
  which remains a supported topology,
  and the ops listener stays plaintext for the kubelet's probe and the metrics scraper.

- **Certificate rotation without a restart.**
  The pair is parsed once and handed to every handshake from an atomic pointer,
  and a goroutine re-reads both files every 30 seconds and swaps the pair only when the contents changed,
  so a cert-manager renewal is served without a rollout.
  A read or parse that fails leaves the pair already loaded in place,
  and re-reading rather than watching survives the kubelet's symlink-rename Secret updates.
  Two metrics make a rotation that quietly stopped working visible before the served certificate expires:
  `profgate_tls_reloads_total` and `profgate_tls_certificate_expiry_seconds`.
  An end-to-end scenario replaces the Secret with a certificate from a second authority
  and verifies the gateway serves it with no Pod restart.

- **TLS in the Helm chart.**
  `tls.enabled` mounts a `kubernetes.io/tls` Secret read-only
  and renders `server.tls` to point inside it.
  The port, its name, the Service, and the NetworkPolicy stay as they are;
  only the scheme changes.
  The chart deliberately adds no checksum annotation over the Secret,
  because a renewal must be re-read from disk, not roll the Deployment.
  The kustomize base keeps serving plain HTTP and gains a commented example Secret.

- **Drain visibility.**
  A finished drain now logs how long it took,
  whether the API listener closed on its own or on the deadline,
  how many requests a deadline close cut short,
  and whether the Collection drain finished or left Collections for another replica to reclaim,
  naming them.
  A drain still waiting on a Collection says so every 30 seconds,
  and a shutdown error that is not the deadline is logged rather than discarded.

### Fixed

- **Requests are no longer reset during a rollout.**
  Readiness turned 503 and the API listener closed in the same instant,
  which reset every request the endpoint controllers and the kube-proxies had not yet stopped routing here.
  The new `server.drainDelay` holds the listener open for that window:
  5 seconds by default, 60 at most, zero to turn it off.
  The gateway waits in process because the distroless image has no shell for a preStop hook,
  and the chart and the kustomize base raise `terminationGracePeriodSeconds` to 125 to match.

- **Discovery keeps moving through the drain.**
  The informers descended from the context the stop signal cancels,
  so discovery froze the moment the drain began,
  even though an in-flight Collection re-resolves its targets every round
  and a profile request confirms its Pod before it dials.
  The informers now run under a context of their own,
  cancelled only once the interactive and Collection waits have ended.

- **Claims that land during the drain are drained too.**
  The drain snapshotted the in-flight Collections once,
  so a claim past its capacity check but not yet committed owned nothing the snapshot could see,
  and the process could exit under a Collection still sampling and merging.
  The drain now refuses every later claim,
  waits for the claims already inside that window,
  and looks again after any wait before it returns.

- **A second stop signal cuts the drain short.**
  The second SIGTERM went into a buffer nobody read,
  which left SIGKILL as the only way to end a drain waiting on a merge
  that would outlast the operator's patience.
  The first signal still asks for the graceful drain;
  the second logs that the drain is being cut short and exits non-zero.

- **A fatal listener error restarts fast.**
  A listener failure ended the process through the same shutdown as SIGTERM,
  waiting without bound for in-flight Collections
  and spending the drain delay on an endpoint window that no longer received requests.
  A replica with no listener has nothing left to serve,
  so the fatal path now skips both waits,
  names the Collections it leaves running, and exits 1;
  they stop renewing their leases
  and another replica reclaims each one whose deadline has not passed and that has an attempt left,
  which is the documented recovery.

## [0.1.1] - 2026-08-25

Hardens the distribution: the image base, configuration loading,
the release gate, and installs on a private network.

### Added

- **Chart installation on a private network.**
  The chart README covered only a direct pull from GHCR,
  which a cluster without egress cannot do.
  It now covers three ways in:
  a proxy for the helm client,
  a one-time file transfer with `helm pull` and `skopeo copy --all`,
  and a standing mirror in an internal OCI registry that holds the chart next to the image.
  Each one names the version forms that differ,
  chart `X.Y.Z` against image `vX.Y.Z`,
  and the values an internal registry needs,
  `image.repository` and `imagePullSecrets`.

- **A guarded release task.**
  `mise run release -- vX.Y.Z` refuses a malformed version,
  a dirty tree, a `HEAD` that is not `origin/main`,
  a tag that already exists locally or on the remote,
  and a commit whose check and e2e runs on `main` did not both succeed,
  and only then creates and pushes the annotated tag that cuts a release.

### Changed

- **The image is based on distroless static.**
  `gcr.io/distroless/static-debian12:nonroot` replaces the Chainguard static image.
  Both bases carry the CA bundle profgate needs when `nats.url` uses `tls://`,
  but distroless offers a pinned tag that still receives security updates,
  where the Chainguard free tier tracks only `latest`.
  The runtime UID stays 65532, so no manifest changes.

- **Configuration loading adopts fuda v1.7.0.**
  The loader now tracks which keys the YAML document supplied,
  so a declared default no longer overwrites a value the operator wrote as the field's zero.
  Validation failures render as `<key>: <plain statement>`,
  for example `discovery.pprof.port: must be at most 65535`.

### Fixed

- **The PGO sampling defaults gained floors.**
  Four keys had a default but no lower bound:
  `pgo.defaults.sampling.duration`, `rounds`, `maxParallel`, and `pgo.defaults.artifact.retention`.
  A zero duration samples nothing,
  and zero rounds or zero `maxParallel` describes a Collection that does no work.
  Each field now refuses values below the floor an operator override was already held to:
  1s, 1, 1, and 1m.
  `pgo.defaults.sampling.roundInterval` keeps accepting zero,
  which is a setting an operator can mean.

## [0.1.0] - 2026-08-25

The initial release: a Kubernetes-aware pprof gateway for Go workloads,
and PGO CPU-profile collection layered on top of it.

### Added

- **The pprof gateway.**
  One HTTP entry point resolves a Kubernetes Service to its backend Pods,
  using EndpointSlice discovery with strict eligibility rules,
  and proxies eight profile types (`cpu`, `trace`, `heap`, `allocs`, `goroutine`, `mutex`, `block`, `threadcreate`)
  over a pinned HTTP client that confirms the Pod before it dials.
  The `/v1` API lists a Service's eligible targets and fetches a profile from a named or randomly selected Pod,
  with version restriction and a bounded duration for `cpu` and `trace`.

- **Access realms.**
  Authorization is static realms loaded from process configuration:
  which namespaces a caller may reach is decided per realm,
  and nothing the gateway emits reveals a Pod IP, a pprof port,
  or a name the caller's realm denies.

- **A read-only Kubernetes footprint.**
  Profgate requires no Kubernetes write permissions:
  it observes Services, Pods, and EndpointSlices in authorized namespaces
  and connects only to explicitly permitted application pprof ports.
  The deployment manifests pin the matching read-only RBAC,
  and a startup preflight confirms the granted permissions before serving.

- **PGO CPU-profile collection.**
  Scheduled and on-demand Collections gather representative CPU profiles for Profile-Guided Optimization:
  multi-round sampling across a Service's Pods,
  an in-memory merge, and retained artifacts.
  Replicas coordinate through dedicated NATS JetStream KV and Object stores,
  with leases, reclaim of Collections whose owner died,
  and a sweeper for expired state,
  while profile bytes stay ephemeral.
  Interactive profiling and PGO sampling share one admission gate,
  and the `/v1` API manages per-Service PGO policies and Collections end to end.

- **Deployment surfaces.**
  A Helm chart and a kustomize base with pinned RBAC,
  NATS account provisioning and credentials mounting for PGO,
  and a release workflow that publishes the image and the chart to GHCR on every tag.

- **Observability and operations.**
  Prometheus metrics for the gateway and the PGO loops,
  a JSON audit record for every `/v1` request,
  a configurable server log level,
  and a CLI with `version` and `config validate`.

- **End-to-end proof on real clusters.**
  kind-based e2e lanes exercise the gateway and PGO collection against real clusters,
  frozen Kubernetes 1.23 and 1.24 images and the current Kubernetes release,
  matching the 1.23 compatibility baseline.

[0.3.0]: https://github.com/arloliu/profgate/releases/tag/v0.3.0
[0.2.0]: https://github.com/arloliu/profgate/releases/tag/v0.2.0
[0.1.1]: https://github.com/arloliu/profgate/releases/tag/v0.1.1
[0.1.0]: https://github.com/arloliu/profgate/releases/tag/v0.1.0
