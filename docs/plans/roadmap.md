# Roadmap

**Status:** Draft

> **How to read this document:** it orders the work that follows the console,
> so that each item is taken up in turn and nothing is started out of order.
> It is not an implementation plan:
> an item that changes behavior first revises the spec it names,
> and only then gets a plan of its own under `docs/plans/`.
> Items that change no behavior — a release, a chart template, a docs repair — need no spec and are executed directly.
> Checkboxes track which items have started.

**Goal:** turn what the gateway already does into something a person can install from a release,
reach from a terminal under `oidc`, diagnose when a Service yields no target,
and drive from automation without scraping human-readable text —
and, once that holds, remove the mechanisms whose cost exceeds what they protect.

**Sources:** the review under `tmp/` that produced this ordering is not tracked;
every claim below is restated against the committed file it cites.

## Ordering Principle

Earlier items are cheaper, block more users, or unblock later items.
Within the list: ship what exists, then make the shipped thing reachable, then diagnosable,
then automatable, then lighter.
An item may be reordered only by editing this list, never by starting later work first.

## Items

### 1. Repair the route count and cut a release

- [ ] `docs/api.md` states in three places that `/v1` has seven routes;
  eleven exist (`internal/httpapi/server.go`), and the `route_unknown` row of the error table repeats the wrong count.
- [ ] Release the `Unreleased` section of `CHANGELOG.md` — authentication, the console, and client-selected ports —
  as the next minor version after `v0.3.0`.

Spec: none.
Why first: nothing under `Unreleased` reaches an operator who installs from the chart registry.

### 2. Chart templates every install needs

- [ ] An `Ingress` template, off by default,
  routing `/`, `/ui/`, `/auth/`, and `/v1/` to the API port;
  `values.yaml`, the chart README, and `NOTES.txt` today instruct the operator to write one by hand.
- [ ] A `PodMonitor` template, off by default, for the ops port,
  which is deliberately absent from the Service.
- [ ] A `PrometheusRule` template, off by default, with the alerts the deployment guide already names as alertable
  (JWKS age, readiness, admission saturation).
- [ ] A `resources.requests` default alongside the derived memory limit,
  so a namespace with a `LimitRange` or quota installs without the raw `resources:` escape hatch.

Spec: the deployment section of `docs/specs/gateway.md` names the chart's shape;
confirm it permits these templates before writing them, and revise it if it does not.
Tests: extend `deploy/chart_test.go` for each template's on and off rendering.

### 3. Client-selected port becomes default-deny

Today `discovery.pprof.allowedPorts` and `allowedPortNames` are independent lists
where an empty list permits any value,
and no setting forbids `portName` outright (`docs/configuration.md`, section `discovery`).
The console lets a browser type any port under that default.

- [ ] Revise `docs/specs/gateway.md`:
  one `discovery.pprof.allowedSelections` list of `{port}` or `{portName}` entries;
  empty means only the configured default is accepted;
  an explicit `allowAny: true` restores today's behavior.
- [ ] Revise the permission invariant text in `AGENTS.md`, `README.md`, and `.agents/rules/800-security-invariant.md`,
  which today states that an empty allowlist admits any port a client names.
- [ ] Write the implementation plan; migrate `docs/configuration.md`, the chart values, and the console's port control.

Spec: `docs/specs/gateway.md` (revision required).
Why here: it narrows what a compromised client can probe and shrinks the configuration surface,
and the CLI in item 4 should be written against the final model.

### 4. A first-party command line

`profgate` today has `version`, `config validate`, `auth hash`, and `serve` (`cmd/profgate/main.go`).
Under `oidc`, `go tool pprof` cannot attach a bearer token,
and `docs/authentication.md` sends the user to another tool for one.

- [ ] Write `docs/specs/cli.md`:
  `login` by OIDC device code with a local token cache,
  `namespaces`, `services`, `targets`, `profile` (with `--open` running `go tool pprof -http`),
  `collect --wait`, `collections`, and `download`;
  the same binary or a second `cmd/`.
- [ ] Write the implementation plan once the spec is `Accepted`.

Spec: new (`docs/specs/cli.md`), layered on `docs/specs/auth.md`,
which already defers token acquisition to this document.

### 5. Target exclusion diagnostics

`/targets` returns only eligible Pods;
an empty answer cannot distinguish no Ready Pod, a selector that matches nothing,
a named port no Pod declares, a terminating Pod, or a cache not yet synced.

- [ ] Revise `docs/specs/gateway.md`:
  `GET .../targets?explain=true` adds aggregate exclusion reasons with counts,
  never Pod identity beyond what the plain listing shows,
  guarded by the same realm check.
- [ ] Revise `docs/specs/ui.md` so the console's empty state shows those reasons.
- [ ] The CLI's `targets` prints them.

Spec: `docs/specs/gateway.md` and `docs/specs/ui.md` (revisions required).

### 6. A machine contract automation can build on

- [ ] `X-Request-Id`: accepted from the client or generated, echoed on every response,
  and carried in the audit record (`internal/httpapi/audit.go`).
- [ ] Structured error details:
  `limit_exceeded` today names the violating fields only in its message (`internal/httpapi/pgo_policy.go`);
  add a `details` array to the error envelope (`internal/httpapi/errors.go`), leaving `message` free to change.
- [ ] `Idempotency-Key` on `POST .../collections`.
- [ ] `GET /v1/collections/{id}?wait=<duration>` long-poll that returns at completion or at the deadline.
- [ ] `GET .../services/{svc}/collections/latest` — the newest completed record and its artifact —
  and `state`, `since`, and `origin` filters plus a cursor on the listing.
- [ ] Revisit the `pgo.artifact.retention` default (2h) against `jobRetention` (168h),
  which leaves records that outlive their artifact by a wide margin.
- [ ] An OpenAPI document generated from the routes, served at a fixed path, and checked in CI against the router.

Spec: `docs/specs/gateway.md` and `docs/specs/pgo.md` (revisions required).

### 7. Console: write paths, browser tests, stable asset paths

- [ ] Start and cancel a Collection from the console;
  the policy editor stays out until conflict handling is designed.
- [ ] A small browser-driven test layer that executes `app.js`:
  the login-to-profile happy path, the `oidc` 401 redirect and return,
  Collection list and download, and hostile strings rendered as text only.
  The spec records that no test runs `app.js` today.
- [ ] Replace the content-hashed asset tree (`internal/ui/ui.go`, `treeHash`) with stable paths,
  an `ETag`, and `Cache-Control: no-cache`;
  this removes the rolling-update failure `docs/console.md` documents.

Spec: `docs/specs/ui.md` (revision required).

### 8. Small removals

Each is a refactor with no behavior change visible to a client, and each is one commit.

- [ ] `pgo.versionPolicy` is a one-valued enum (`internal/config/config.go`, `oneof=strict`); remove the key.
- [ ] `internal/pgo/id.go` hand-packs Crockford base32 for an identifier nobody transcribes;
  keep the identifier grammar the API documents, or revise the spec to plain hex, and delete the packer.
- [ ] `internal/auth/cookie.go` frames session and transaction fields with hand-written length prefixes;
  serialize with `encoding/json` under the same seal.
- [ ] `internal/natskv/preflight.go` writes, watches, and deletes probe keys and objects at startup
  and `internal/pgo/sweeper.go` then sweeps those probes;
  drop the probes and let the first real operation fail loudly instead.
- [ ] `deploy/chart_test.go` is larger than the chart it tests;
  replace per-field assertions with `helm template` golden files where a golden file reads as well.
- [ ] `docs/decisions/e2e-without-framework.md` set a revisit trigger on harness size;
  `test/e2e/harness_test.go` has passed it, and no revisit is recorded.

Spec: `docs/specs/pgo.md` for the identifier grammar; none otherwise.

### 9. Superseded and finished documents leave the tree

`docs/specs/profgate-design.md` is `Superseded` and five plans are `Done`;
together they are most of the Markdown in the repository and compete with the living specs in every search.
`docs/README.md` today keeps finished plans as frozen history.

- [ ] Write a decision record that finished plans and superseded specs are removed in the change that supersedes them,
  with `git log --follow` as their history.
- [ ] Apply it.

Spec: none; the decision record changes `docs/README.md`.

### 10. OIDC transport from a library

`internal/auth/{discovery,jwks,issuer,verify}.go` implement discovery, key caching, and verification by hand.
Replace the transport, cache, and discovery with a maintained library;
keep the key-hardening rules in `internal/auth/jwks.go` —
minimum RSA size, algorithm-to-curve binding, duplicate `kid` rejection —
which the library does not provide.

Spec: `docs/specs/auth.md` (revision required for the dependency).

### 11. PGO as an optional deployment with presets

The scheduler, worker, sweeper, and their stores run in every gateway replica whenever `pgo.enabled` is set,
and `profgate config validate` reports a termination grace period and memory budget
that an operator must carry into the Deployment by hand.

- [ ] Revise `docs/specs/pgo.md`:
  the collector runs as a separate Deployment or subcommand,
  the twelve `pgo.limits.*` keys collapse into named presets with an escape hatch,
  and the grace-period and memory arithmetic moves inside the chart.
- [ ] Decide, in the same revision, whether one collector replica suffices;
  if it does, the lease, claim, and orphan-sweep machinery can be removed with the multi-replica guarantee.

Spec: `docs/specs/pgo.md` (revision required).
Why last: it is the largest change, it is off by default, and every earlier item is useful without it.

## Not on This List

- Rendering profiles — flamegraphs, diffs, top tables — stays with `go tool pprof`.
- Non-Go pprof producers, multi-cluster, continuous profiling, long-term profile storage.
- Cookie key rotation, the file poller, and the fingerprint gauge stay as they are;
  they cost little and removing them buys little.

## Validation

Every item that lands code ends with:

```bash
mise run lint && mise run test && mise run check
```

Prose uses semantic line breaks; run `semlf check` on what you wrote.
