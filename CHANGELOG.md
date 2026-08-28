# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **BREAKING: client-selected ports are default-deny.**
  `discovery.pprof.allowedPorts` and `discovery.pprof.allowedPortNames` are removed,
  together with `PROFGATE_PPROF_ALLOWED_PORTS` and `PROFGATE_PPROF_ALLOWED_PORT_NAMES`,
  and replaced by the one list `discovery.pprof.allowedSelections`
  (`PROFGATE_PPROF_ALLOWED_SELECTIONS`, comma-separated `port:N`, `portName:name`, `port:*`, `portName:*`).
  Each entry is `{port: N}` or `{portName: name}`;
  an empty list now admits only the configured default,
  where an empty allowlist used to admit any value of its parameter.
  `{port: "*"}` admits any port number and `{portName: "*"}` admits any port name, each on its own.
  A configuration that still sets a removed key or variable fails validation with a message naming the replacement.
  Each old list converts on its own:

  | Old value | New entry |
  |---|---|
  | `allowedPorts: []` | `- port: "*"` |
  | `allowedPortNames: []` | `- portName: "*"` |
  | `allowedPorts: [6061, 6062]` | `- port: 6061` and `- port: 6062` |
  | `allowedPortNames: [pprof-alt]` | `- portName: pprof-alt` |

  `/v1/limits` reports `pprof.allowedSelections`, an array of one-key objects, in place of the two arrays.
  `400 port_not_allowed` carries a `details` array with one item,
  `field` `port` or `portName` and `code` `not_admitted`, naming only the value the client sent.
  The console's port control is a menu of the configured default and every listed entry,
  with a free-form field only where the matching wildcard is configured.

### Added

- **The `profgate` binary is also a client.**
  `login`, `logout`, `whoami`, `limits`, `namespaces`, `services`, `targets`, `profile`,
  `collect`, `collections`, `collection get|cancel`, `download`, `pgo policy get|set|delete`,
  and `context list|show|use|delete` talk to a gateway from a terminal,
  each calling one route of the HTTP API;
  `docs/cli.md` is the guide.
  Under `oidc`, `login` obtains a token by the device-code grant, with a PKCE challenge where the gateway asserts one,
  caches it under `$XDG_STATE_HOME/profgate/tokens/` with `0600` permissions,
  and refreshes it before it expires;
  under `basic` it verifies a user name and a password it never stores;
  `profile --open` hands the fetched profile to `go tool pprof -http`.
  A credential travels only over `https://` or to a loopback address, and no flag skips certificate verification.
  `collect` sends an `Idempotency-Key` on every create;
  the gateway does not read the header yet,
  so the client retries no create until it does and reports a lost response once.
  No Go module was added.
- **`GET /v1/auth`, the one `/v1` route with no authentication step.**
  It reports `auth.mode` to an unauthenticated caller and, where `auth.oidc.cli` is configured,
  the issuer, the client identifier, the token type, the scopes, and whether the device endpoint accepts PKCE,
  which is what a device login needs before it holds a credential.
  It writes no audit record and is counted under `endpoint="auth"`.
- **The optional `auth.oidc.cli` block.**
  `clientID` (default `auth.oidc.audience`), `scopes` (default `openid, offline_access`), and `pkce` (default `false`),
  under `PROFGATE_AUTH_OIDC_CLI_CLIENT_ID` and `PROFGATE_AUTH_OIDC_CLI_PKCE`.
  The block's presence is what makes `GET /v1/auth` report a device login; an empty block enables it with every default.
  `clientID` must equal `auth.oidc.audience` under `tokenType: id`,
  and the block is refused beside `auth.oidc.browser.clientSecretFile` under that token type,
  because the shared registration must stay a public client.
  The chart renders the block by default through `auth.oidc.cli.enabled: true`,
  and omits it, saying so in `NOTES.txt`, when a browser client secret under `tokenType: id` forbids it.
- **Optional chart templates for the pieces every install needed to write by hand.**
  `ingress.enabled` renders an Ingress routing `/`, `/ui/`, `/auth/`, and `/v1/` to the API port;
  `podMonitor.enabled` renders a PodMonitor for the ops port, which the Service deliberately omits;
  `prometheusRule.enabled` renders alerts for stale JWKS keys, discovery not synced, and admission saturation.
  All three are off by default.
- **A default CPU request.**
  The container ships `resources.requests.cpu: 100m`
  so a namespace whose quota counts CPU requests admits the gateway;
  `resources.requests` now merges with the shipped value and `resources.limits` still replaces the derived memory limit.

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

[Unreleased]: https://github.com/arloliu/profgate/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/arloliu/profgate/releases/tag/v0.4.0
[0.3.0]: https://github.com/arloliu/profgate/releases/tag/v0.3.0
[0.2.0]: https://github.com/arloliu/profgate/releases/tag/v0.2.0
[0.1.1]: https://github.com/arloliu/profgate/releases/tag/v0.1.1
[0.1.0]: https://github.com/arloliu/profgate/releases/tag/v0.1.0
