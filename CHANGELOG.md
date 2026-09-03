# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Every release tag gets a GitHub Release.**
  The release workflow creates one after the image and chart are pushed,
  with that version's changelog section, the image tag and digest, and the chart version as its notes.
  `v0.4.0` and `v0.5.0` have releases written the same way by hand;
  the tags before them already had one.
- **The guides link the changelog, and the README names the client.**
  `README.md`, the deployment guide, and the chart README each link this file where a reader stands when it matters,
  and the deployment guide's upgrade section restates `0.5.0`'s port-selection change there.
  The README also names the `profgate` binary as a client, links its guide,
  and its quickstart lists the namespaces and the Services a caller's realm admits before it asks for a profile.

### Changed

- **BREAKING: the container memory limit follows `pgo.enabled`.**
  `profgate config validate` printed a merge budget for a gateway that never merges,
  three times what the chart renders for the same file;
  with collection off it now prints the gateway's own footprint alone,
  and prints `pgo collection: disabled` in place of the `pgo working set bytes` line,
  so a reader of the second line sees the state rather than a budget.
  The kustomize base drops from `1536Mi` to `512Mi` with it,
  so uncommenting its PGO block now means raising the limit too.
- **BREAKING: the chart validates `memoryLimitWithoutPGO` on both branches.**
  With `pgo.enabled` false the chart printed the value unchecked, so `512` rendered a limit of 512 bytes.
  Both branches now hold it to the one grammar, a whole number of `Mi` or `Gi`,
  so a values file carrying `512`, `1500M`, or `0.5Gi` fails rendering where it rendered before.
  An accepted value renders exactly what it rendered.

### Fixed

- **The chart refuses an authentication mode it can see will not start.**
  `auth.mode: basic` with neither `auth.basic.users` nor `auth.basic.usersFile`,
  and `auth.mode: oidc` with no `auth.oidc.issuer`,
  each rendered a Deployment whose Pod exited at startup;
  both now fail while the chart renders.
  The raw `config:` block is read first for `auth.mode`, `auth.basic.users`, and `auth.oidc.issuer`,
  so a value supplied only there is the one judged and the one named,
  and any `PROFGATE_AUTH_`-prefixed entry in `extraEnv` switches both checks off,
  because the environment can carry a value the chart cannot see.
- **`kubectl apply -k deploy/base` works on a cluster that has never heard of Profgate.**
  The base creates the `profgate` namespace its own resources name,
  so `kubectl delete -k deploy/base` now removes that namespace and everything in it,
  and a repository whose namespaces are managed elsewhere drops the entry from `resources`.
  The base also pins the released image tag where it pinned `latest`.
- **The install notes describe the release that was rendered.**
  Basic mode's notes name where users come from instead of promising a list and printing the realms,
  and an Ingress in front of a TLS-enabled API port is warned about the backend-protocol setting it needs.

## [0.5.0] - 2026-09-03

Adds a first-party command line, target exclusion diagnostics, and an HTTP contract automation can build on,
alongside Collection controls in the console, lower PGO defaults,
and a store-generation barrier that keeps the watched PGO caches honest, with five breaking changes.

### Added

- **The `profgate` binary is also a client.**
  `login`, `logout`, `whoami`, `limits`, `namespaces`, `services`, `targets`, `profile`,
  `collect`, `collections`, `collection get|cancel`, `download`, `pgo policy get|set|delete`,
  and `context list|show|use|delete` talk to a gateway from a terminal, and `docs/cli.md` is the guide.
  Under `oidc`, `login` obtains a token by the device-code grant and caches it under `$XDG_STATE_HOME/profgate/tokens/`;
  under `basic` it verifies a user name and a password it never stores.
  `profile --open` hands the fetched profile to `go tool pprof -http`.
- **A device login a client can discover.**
  `GET /v1/auth`, the one `/v1` route with no authentication step, reports `auth.mode` and,
  where the new optional `auth.oidc.cli` block (`clientID`, `scopes`, `pkce`) is configured,
  the issuer, client identifier, token type, scopes, and whether the device endpoint accepts PKCE.
  The chart renders that block by default through `auth.oidc.cli.enabled: true`.
- **Every response of both listeners carries `X-Request-Id`** — the caller's own when the request sends one,
  generated otherwise — and every audit record names the same value under `requestId`.
- **Every refusal whose code has a vocabulary carries a `details` array of `{field, code, message}` items.**
  `invalid_parameter` has twelve values, `limit_exceeded` five, and `port_not_allowed` its one,
  and every entry of `GET /pgo`'s `violations` carries a `code` from the `limit_exceeded` vocabulary.
- **`POST .../collections` takes an `Idempotency-Key`.**
  A repeat under one key answers with the Collection the first request created rather than starting a second,
  and a repeat asking for something else answers `409 idempotency_mismatch`.
- **`GET /v1/collections/{id}` takes `wait=`, a duration from `1s` to `60s`,**
  and holds the request open until the Collection's state moves, the deadline passes,
  the replica drains, or the client leaves.
- **The collections listing filters and pages, and `latest` answers without an identifier.**
  `GET .../collections` takes `state`, `origin`, `since`, `limit`, and `cursor`, and its body carries `nextCursor`;
  `GET .../collections/latest` and `.../latest/profile` answer for the newest Collection still holding a profile.
- **`GET /v1/openapi.json` serves a hand-maintained OpenAPI 3.1 document.**
  It describes every route the listener serves, whatever the configuration enables.
- **`explain=true` on the targets endpoint, and the `version` and `pod` filters it now accepts.**
  `GET .../targets?explain=true` keeps the plain listing and adds `selectorMatched` and `excluded`,
  one entry per exclusion reason with a non-zero count, so an empty answer says why it is empty.
  The console shows those reasons where a Service has no target, and `profgate targets --explain` prints them too.
- **The console starts and cancels a Collection.**
  A **Start collection** control, and a **Cancel** on every `pending` or `running` row,
  appear when `pgo.enabled` is true and the caller's realm carries `pgo.collect`.
  Each takes two presses in place, and the page still edits no Service's PGO policy.
- **Optional chart templates for the pieces every install needed to write by hand.**
  `ingress.enabled` renders an Ingress routing `/`, `/ui/`, `/auth/`, and `/v1/` to the API port;
  `podMonitor.enabled` renders a PodMonitor for the ops port, which the Service deliberately omits;
  `prometheusRule.enabled` renders alerts for stale JWKS keys, discovery not synced, and admission saturation.
  All three are off by default.
- **A default CPU request.**
  The container ships `resources.requests.cpu: 100m`, so a namespace whose quota counts CPU requests admits it.
- **`profgate_pgo_synced`, and the chart's `ProfgatePGONotSynced` alert.**
  The gauge is `1` only when the watched PGO caches have replayed and applied under the current store generation.
  The alert fires when it has read `0` for ten minutes, and is off by default like the other three.
- **The end-to-end suite drives the console in a headless Chromium.**
  Two scenarios execute the page's own JavaScript, which nothing else does, and a machine with no Chromium skips them.

### Changed

- **BREAKING: client-selected ports are default-deny.**
  `discovery.pprof.allowedPorts` and `allowedPortNames` are removed, with their `PROFGATE_*` variables,
  and replaced by the one list `discovery.pprof.allowedSelections`
  (`PROFGATE_PPROF_ALLOWED_SELECTIONS`, comma-separated `port:N`, `portName:name`, `port:*`, `portName:*`).
  `{port: "*"}` admits any port number and `{portName: "*"}` admits any port name, each on its own.
  An empty list now admits only the configured default, where an empty allowlist used to admit anything,
  and a configuration that still sets a removed name fails validation with a message naming the replacement.
  `/v1/limits` reports `pprof.allowedSelections` in place of the two arrays, and each old list converts on its own:

  | Old value | New entry |
  |---|---|
  | `allowedPorts: []` | `- port: "*"` |
  | `allowedPortNames: []` | `- portName: "*"` |
  | `allowedPorts: [6061, 6062]` | `- port: 6061` and `- port: 6062` |
  | `allowedPortNames: [pprof-alt]` | `- portName: pprof-alt` |

- **BREAKING: `pgo.defaults.target.versionPolicy` is removed.**
  It was the only key under `pgo.defaults.target`, so delete the whole block; nothing replaces it.
  A configuration file that still sets it fails validation, a request body carrying the field is refused,
  and `effective.target.versionPolicy` is gone from `GET /pgo` and from `profgate pgo policy`.
  Removing it also moves the policy hash an `Idempotency-Key` is bound to,
  so retry with a fresh key: one minted before the upgrade answers `409 idempotency_mismatch`
  until its receipt expires, which takes `pgo.jobRetention`, a week by default.
- **BREAKING: an artifact is kept for at least the interval that produces it.**
  `pgo.defaults.artifact.retention` moves from `2h` to `24h`,
  and every effective policy must now hold `artifact.retention` at least `schedule.every`.
  A policy that breaks the rule is refused with `400 limit_exceeded` and a `retention_under_interval` detail,
  a stored override that breaks it makes the Service ineligible for scheduling, and a file pinning it no longer starts.
  Raise the retention, or lower the interval, until one covers the other.
- **BREAKING: the container memory limit falls from 4 GiB to `1536Mi` and now counts the gateway's own footprint.**
  `pgo.limits.maxSampleBytes` (`33554432` to `16777216`), `maxMergedBytes` (`67108864` to `33554432`),
  and `maxActiveCollections` (`2` to `1`) take the working set from 4 GiB to 1 GiB; `maxParallel` keeps its `4`.
  An operator who set any of the three keys keeps their own value, and the chart sizes the container from it;
  the chart's `memoryLimitWithoutPGO` is now read as a byte count and must be a whole number of `Mi` or `Gi`.
  A hand-written Deployment carrying `4Gi` still runs, and the kustomize base moves to `1536Mi`.
  `profgate config validate` prints `pgo working set bytes` and `container memory bytes` in place of `pgo memory bytes`.
- **BREAKING: the two Collection writes require `Content-Type: application/json`.**
  `POST .../collections` and `POST /v1/collections/{id}/cancel` refuse anything else with `400 invalid_parameter`,
  so a client that omitted the header declares it.
- **Console assets are served at stable paths.**
  No asset URL carries a content hash, and each asset carries an `ETag` and `Cache-Control: no-cache`,
  so a browser revalidates on every load and is answered `304` while its copy is current.
  This removes the rolling-update failure the hashed tree produced, at the cost of one rollout —
  the one that carries this release — where a console load can fail until the replicas converge.
- **Upgrading invalidates every browser session.**
  Session and transaction cookies now carry a JSON object where they carried length-prefixed fields,
  so a browser holding a session is signed out once and logs in again,
  and a login in flight across the upgrade returns `401` with reason `state` and starts over.
- **A rollout interrupts a running Collection instead of waiting for it.**
  A terminating replica stops renewing the lease on every Collection it owns,
  and returns once each owner has committed or reached the cutoff of the lease it last renewed,
  which is `pgo.leaseTTL` minus five seconds of clock skew.
  An owner still merging at that cutoff commits nothing,
  and the replica that reclaims the record retries it from round zero under `pgo.maxAttempts`.
  `profgate config validate` no longer prints a second grace period for PGO.

### Removed

- **`slot_timeout` no longer appears in a Collection manifest.**
  Sampling takes no slot in the admission gate interactive requests pass through,
  so a sample never waits for one and never fails for want of one.
- **The rule measuring the PGO fan-out against `limits.maxConcurrentProfiles` is gone.**
  A configuration whose `pgo.limits.maxParallel × pgo.limits.maxActiveCollections` reaches that ceiling now loads,
  and every configuration that loaded before still loads.

### Fixed

- **A gateway with `pgo.enabled` no longer gains a duplicate cache consumer when a watch fails to open.**
  A failed open now ends the watches the attempt had already opened,
  so a process that reopens its caches runs on one consumer per prefix.
- **A watch cut under a live connection no longer leaves the watched PGO caches stale.**
  A re-opened watcher used to replay under the generation it already held,
  so nothing rebuilt the cache and a key changed during the gap could stay missing indefinitely.
  Such a cut now moves the store generation the way a disconnect already does, and every watched cache rebuilds.
  A session's cache reads carry the generation they were admitted under,
  so the collections listing, a Collection create, and the two `latest` routes now answer `503 pgo_unavailable`
  where they used to answer over caches that had gone stale.


## [0.4.0] - 2026-08-27

Adds authentication, the embedded operator console, and client-selected pprof ports.

### Added

- **A client can select the pprof port for one request.**
  A `port` (a decimal integer) or `portName` (a container-port name) query parameter,
  on the targets and profile endpoints,
  replaces the configured default for that request, never both together.
  Two independent operator allowlists,
  `discovery.pprof.allowedPorts` and `allowedPortNames`, bound what a client may name;
  an empty list permits any value of its parameter, and the configured default always passes.
  A value a non-empty allowlist excludes is refused with `400 port_not_allowed` before discovery runs.
  The audit log gains a `port` field recording the selection as sent —
  a number or a name, empty when absent, and for a name never the number it resolved to.
  The end-to-end test application now serves pprof on a second listener
  so the two allowlists have a real alternate port and port name to exercise.

- **Two authentication modes.**
  `auth.mode: basic` checks HTTP Basic credentials against a static, bcrypt-hashed user list;
  `auth.mode: oidc` verifies a bearer JWT against an OpenID Connect issuer
  and maps it to a realm by username, by group, or by a default.
  A credential that fails to resolve to a realm is denied outright —
  neither mode falls through to `auth.anonymousRealm` —
  and every authentication failure answers `401 unauthenticated` with a `WWW-Authenticate` header naming the scheme,
  `429 too_many_auth` when Basic's per-replica bcrypt gate is full,
  or `503 auth_unavailable` when the gateway cannot decide
  (stale signing keys, an unreachable issuer, a failed random read).
  `internal/auth` is the only non-test importer of `github.com/go-jose/go-jose/v4` and `golang.org/x/crypto`.

- **An optional browser login for `oidc` mode.**
  `auth.oidc.browser` turns a browser navigation with no credential into a redirect to the issuer's login page,
  instead of `401`, using the authorization-code flow with PKCE,
  and mints an encrypted, stateless session cookie from the ID token it receives.
  The three `/auth/login`, `/auth/callback`, and `/auth/logout` routes exist only when it is configured.
  A session cannot be revoked before it expires; `sessionTTL` (8 hours by default) bounds that exposure.
  The cookie key rotates across replicas in a staged five-step procedure that never drops a session,
  and `profgate_auth_cookie_key_info` reports which fingerprints every replica has loaded.

- **`profgate auth hash`.**
  Reads a password from the terminal without echo and prints its bcrypt hash at cost 12,
  so an operator never has to find `htpasswd`.

- **Authentication metrics, audit fields, and the authentication Secret.**
  The ops listener gains `profgate_auth_failures_total`, `profgate_auth_sessions_issued_total`,
  `profgate_oidc_jwks_refresh_total`, `profgate_oidc_jwks_keys`, `profgate_oidc_jwks_age_seconds`,
  `profgate_auth_file_reload_total`, and `profgate_auth_cookie_key_info`.
  The audit log gains `auth_reason` on a failure, and the `/auth/` routes write their own record.
  The Helm chart mounts a Secret at `auth.secret.mountPath` for the users file, the issuer CA certificate,
  a browser client secret, and the cookie key, none of which belong in the rendered ConfigMap;
  [`deploy/secret-auth-example.yaml`](deploy/secret-auth-example.yaml) is a commented manifest for it.

- **An embedded operator console, off by default.**
  `ui.enabled` serves a static page at `/ui/`:
  pick a namespace, then a Service, then a profile,
  download it or copy the URL `go tool pprof` fetches it from,
  see who you are and what your realm admits,
  and, when PGO collection is enabled, browse a Service's Collections and download a finished artifact.
  The page renders no profile and stores nothing; every fact it shows came from a `/v1` response a realm bounded.
  Four listing routes back it and are useful from a script too, realm-filtered and read-only:
  `GET /v1/namespaces`, `GET /v1/namespaces/{ns}/services`, `GET /v1/whoami`, and `GET /v1/limits`.
  `Catalog` on the discovery seam reads the Service cache for the two lists and issues no request,
  so the console adds no Kubernetes capability.
  `profgate_requests_total`'s `endpoint` label gains `namespaces`, `services`, `whoami`, `limits`, and `ui`.
  The page vendors Preact, htm, and Pico CSS under `internal/ui/static/vendor/`,
  compiled into the binary with `go:embed`; it loads nothing from a CDN.
  The Helm chart gains a `ui.enabled` value, off by default.

### Changed

- **The targets endpoint now accepts `port` and `portName`.**
  It previously took no query parameters; any other query parameter still answers
  `400 invalid_parameter`.
- **The permission invariant's wording.**
  It now says the gateway connects to application ports the operator permits,
  and that an empty allowlist accepts any port or port name a client names,
  rather than naming one fixed port as the only one reachable.
- **The request algorithm gained two steps.**
  Credential placement rejects an `access_token` query parameter before any credential is read,
  and authentication resolves the principal and its realm; every later step renumbers.
  `401 unauthenticated` now precedes `403 realm_denied`,
  so a client can tell "present a credential" from "your credential does not reach this."
- **`/` now redirects to `/ui/` when the console is enabled.**
  Logout's fallback redirect lands there too, so signing out returns to a signed-out console.

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

[Unreleased]: https://github.com/arloliu/profgate/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/arloliu/profgate/releases/tag/v0.5.0
[0.4.0]: https://github.com/arloliu/profgate/releases/tag/v0.4.0
[0.3.0]: https://github.com/arloliu/profgate/releases/tag/v0.3.0
[0.2.0]: https://github.com/arloliu/profgate/releases/tag/v0.2.0
[0.1.1]: https://github.com/arloliu/profgate/releases/tag/v0.1.1
[0.1.0]: https://github.com/arloliu/profgate/releases/tag/v0.1.0
