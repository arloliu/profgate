# Console Implementation Plan

**Status:** Done
**Outcome:** commit 04324eb on main; CI passed check and all three e2e lanes.

> **For the implementer:** implement this plan one task at a time, in order;
> each task is written test-first and ends with its own validation block and commit.
> The first task repairs the accepted spec, so every later task implements from a document that is true.
> Checkboxes (`- [ ]`) track progress.

**Goal:** Build the operator console defined in [`docs/specs/ui.md`](../specs/ui.md):
four read-only, realm-filtered listing endpoints under `/v1`,
a static page served by the gateway at `/ui/` and off by default,
the `Catalog` method the listing endpoints read the Service cache through,
the `ui.enabled` configuration key and its chart value,
the audit and metrics rows that make the new routes visible,
and the unit and end-to-end layers that prove all of it.

**Architecture:** One new package.
`internal/ui` embeds the page and the vendored browser libraries with `go:embed`,
hashes the tree once, renders the shell, and serves `/ui/` with the response headers of the spec;
`internal/k8s` gains `ServiceRef` and `Catalog`, reading the Service lister and issuing no request;
`internal/httpapi` gains the four listing routes,
dispatches `/ui/` and `/` to a `Console` handler it takes as an `http.Handler`,
and exports its error writer as `WriteError` so the console shares one envelope;
`internal/config` gains the `ui` block and the rule that ties it to the browser flow under `oidc`;
`internal/metrics` gains five `Endpoint` values;
`cmd/profgate` constructs `internal/ui` when `ui.enabled` and hands it to `httpapi`;
`deploy/chart` gains the `ui.enabled` value and its raw-block guard;
`test/e2e` extends the two authentication scenarios with the console's checks.

**Tech Stack:** everything already pinned; no Go module is added (*Dependencies*).
Three browser files are vendored under `internal/ui/static/vendor/` and pinned by a manifest:
Preact 10.29.8 (MIT), htm 3.1.1 (Apache-2.0), and Pico CSS 2.1.1 (MIT),
each read from the npm registry on the day this plan was written and recorded in *Vendor the browser libraries*.

**Spec:** [`docs/specs/ui.md`](../specs/ui.md),
layered on [`docs/specs/gateway.md`](../specs/gateway.md),
[`docs/specs/auth.md`](../specs/auth.md), and [`docs/specs/pgo.md`](../specs/pgo.md).
Every behavior table below restates the spec for the task at hand;
where they differ the spec wins, and the plan is the bug.
Spec sections are cited by heading name; unqualified sections are the console spec's.
The unit-test cases in the spec's *Testing* section are normative:
each task below names its slice of them, and the task is done only when every bullet for its package passes.
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- Everything in the gateway, PGO, and authentication plans' constraints still holds.
- **No Kubernetes write verb, no new RBAC tuple, no change to the shipped ClusterRole.**
  `Catalog` reads the Service informer cache and issues no request;
  `TestClusterRoleTuples` in `deploy/deploy_test.go` and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go` stay green untouched,
  and the `internal/k8s` unit test for `Catalog` asserts the fake clientset saw no action beyond the seven granted tuples
  (*Permission boundary* in the gateway spec).
- Only `internal/k8s` imports `k8s.io/client-go`, unchanged;
  `mise run check` enforces it (`check_clientgo_importers` in `scripts/check-repo.py`).
- **Only `internal/ui` embeds static files.**
  No other package uses `go:embed`, and `internal/ui` embeds exactly its `static` directory.
- **No HTML built from a response.**
  `app.js` and `urls.js` contain none of
  `innerHTML`, `outerHTML`, `dangerouslySetInnerHTML`, `insertAdjacentHTML`, `document.write`, or `DOMParser`;
  the source-scan unit test in `internal/ui` enforces it against the real files and against fixture strings that must fail
  (*Rendering response values*, *Unit*).
- **No new Go module.**
  The spec's *Dependencies* names none;
  `embed`, `crypto/sha256`, `mime`, `io/fs`, and `net/http` are the standard library.
  `go.mod` and `go.sum` do not change in any task.
- **Vendored assets are pinned.**
  Each vendored file is the upstream's published build, byte for byte,
  with its version, license, source URL, and SHA-256 recorded in `internal/ui/static/vendor/MANIFEST`,
  and its license text beside it.
  The exact tarballs, hashes, and file paths are in *Vendor the browser libraries*;
  a task that changes a vendored byte changes the manifest line in the same commit.
- The four listing routes exist whether or not `ui.enabled` is set;
  `/ui/`, every path under it, and `/` exist only when it is (*Routes*).
- Every response under `/ui/` carries the headers of *Response headers and CSP*;
  the shell is `Cache-Control: no-store` and every hashed asset is immutable (*Headers*).
- No listing response carries a Pod IP, a `podIP` field, a node, or a port number a Pod declares (*Response shapes*).
- The interactive request algorithm does not change:
  the targets and profile endpoints run the same steps in the same order with the same outcomes (*Core decisions*).
- Global state stays as the gateway plan lists it:
  `internal/ui` has no `init` function and no package-level mutable state;
  the embedded `embed.FS` is immutable and allowed.
  The same rule holds in the browser: `app.js` and `urls.js` declare no top-level `let` or `var`,
  and every top-level `const` binds an import, a function, a class, or a literal that nothing reassigns;
  state that changes lives on the mounted component.
  The source-scan unit test in `internal/ui` enforces the `let`/`var` half (*Static handler*).
- No jargon: code comments, commit messages, and documentation state the current fact,
  never the task number, the review round, or this plan's sequencing.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on what you wrote.

---

## File Structure

```text
internal/config/config.go                   # UIConfig; Config.UI; validateUI
internal/config/config_test.go, testdata/ui-*.yaml
internal/k8s/discovery.go                   # ServiceRef; Catalog on Discovery
internal/k8s/catalog.go                     # (*Cluster).Catalog over the Service lister
internal/k8s/catalog_test.go
internal/httpapi/fixtures_test.go           # fakeDiscovery.Catalog
internal/pgo/fixtures_test.go               # fakeDiscovery.Catalog
internal/metrics/recorder.go                # + EndpointNamespaces, EndpointServices, EndpointWhoami, EndpointLimits, EndpointUI
internal/httpapi/server.go                  # four route kinds; the listing branch; Deps.Console; /ui/ and / dispatch
internal/httpapi/listing.go                 # listing route regexp, response shapes, the realm filter, the four handlers
internal/httpapi/listing_test.go
internal/httpapi/console.go                 # serveConsole, the status-capturing writer, the ui metric codes
internal/httpapi/console_test.go
internal/httpapi/errors.go                  # writeError becomes WriteError
internal/httpapi/realm.go                   # realmAllows and pgoAllows learn the four kinds
internal/ui/doc.go                          # package comment
internal/ui/ui.go                           # Handler, New, tree hash, shell rendering, routes, headers
internal/ui/ui_test.go
internal/ui/vendor_test.go                  # manifest hashes, relative imports, no inline forms, no eval
internal/ui/scan_test.go                    # the source scan of Rendering response values
internal/ui/static/index.html               # the shell with its two placeholders
internal/ui/static/app.js                   # the console
internal/ui/static/urls.js                  # the URL builders; the only module that spells a /v1 path
internal/ui/static/app.css                  # the console's own rules on top of Pico
internal/ui/static/vendor/MANIFEST
internal/ui/static/vendor/preact/preact.module.js, LICENSE
internal/ui/static/vendor/htm/htm.module.js, LICENSE
internal/ui/static/vendor/pico/pico.classless.min.css, LICENSE
cmd/profgate/serve.go                       # constructs ui.New when cfg.UI.Enabled; Deps.Console
cmd/profgate/serve_test.go                  # gatewayOpts.uiEnabled; the console rows
deploy/base/configmap.yaml                  # a commented ui block
deploy/chart/profgate/values.yaml           # ui.enabled
deploy/chart/profgate/templates/_helpers.tpl # profgate.uiEnabled; the ui block in configStructured; the raw-block guard
deploy/chart/profgate/README.md             # the ui.enabled row and paragraph
deploy/chart_test.go, deploy_test.go        # the console rows
test/e2e/harness_test.go                    # gatewayConfigOptions.UIEnabled
test/e2e/scenarios_auth_test.go             # the console steps in both authentication scenarios
docs/specs/gateway.md, auth.md              # the amendments the console spec lists
.agents/rules/100-project-map.md            # internal/ui/; the new routes
docs/README.md                              # specs/ui.md; the console guide; the plans sentence
docs/api.md, configuration.md, deployment.md, console.md
CHANGELOG.md
```

---

## Amendments to the accepted designs

**Files:**
- Modify: `docs/specs/gateway.md`, `docs/specs/auth.md`, `docs/specs/ui.md`,
  `.agents/rules/100-project-map.md`, `docs/README.md`

The console spec's *Changes to the accepted designs* says its table is applied "in the same change" as acceptance;
the acceptance commit changed only `AGENTS.md` and the spec's own status,
so every other row is still open and lands here.
This task runs before any code changes:
an accepted spec outranks this plan, and a later task must not implement from a document
that still carries the requirements this task corrects.
`AGENTS.md` already reads *Four Specs, All Accepted* and needs nothing.

- [x] **Apply the table**

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *HTTP API* | the `/auth/` exception gains "and the `/ui/` and `/` routes of `ui.md` when `ui.enabled`"; a sentence naming the four listing routes as `/v1` routes defined in `ui.md` |
| `docs/specs/gateway.md` | *Request algorithm* | after the numbered list: the four listing routes run the route, method, readiness, credential-placement, and authentication steps as written, the realm step refuses only the Service list, and then they read the cache with no discovery, admission, confirmation, or proxy step; they accept `GET` only |
| `docs/specs/gateway.md` | *List targets* | a following subsection *Listing endpoints* pointing to `ui.md` *Response shapes* |
| `docs/specs/gateway.md` | *Errors* | `503 discovery_unavailable` also covers a cache read that fails on a listing route; `405` under `/ui/` and on `/` carries `Allow: GET, HEAD` |
| `docs/specs/gateway.md` | *The seam* | `ServiceRef` and `Catalog` in the Go block, with the comment from *Package layout* of `ui.md` |
| `docs/specs/gateway.md` | *Non-disclosure* | a fourth listed observation: `/v1/limits` returns both allowlists and the default to any authenticated caller, with a pointer to `ui.md` *Limits* for the argument |
| `docs/specs/gateway.md` | *Logging* | the listing routes write the record with `namespace` on the Service list only and the other target fields empty; requests under `/ui/` and to `/` write no record |
| `docs/specs/gateway.md` | *Metrics* | `endpoint` gains `namespaces`, `services`, `whoami`, `limits`, `ui`; the `ui` codes: `ok`, `route_unknown`, `method_not_allowed`, and `internal_error` for any other status the console wrote |
| `docs/specs/gateway.md` | *Layers* | unit bullets for `internal/ui`, the listing routes, `Catalog`, and the `ui.enabled` validation, per `ui.md` *Unit* |
| `docs/specs/gateway.md` | *What end-to-end proves* | two new entries at the end of the list: the two console proofs of `ui.md` *End to end*, run inside the two authentication scenarios |
| `docs/specs/gateway.md` | *Configuration* | the `ui.enabled` row |
| `docs/specs/gateway.md` | *Build and Deployment* | the vendored browser files under `internal/ui/static/vendor/` are embedded by `go:embed` and pinned by `MANIFEST`; the chart's `ui.enabled` value and raw-block guard |
| `docs/specs/gateway.md` | *Dependencies* | a closing sentence: no Go module for the console; the vendored browser code is listed in `ui.md` *Dependencies* |
| `docs/specs/gateway.md` | *Package Layout* | `internal/ui/` |
| `docs/specs/gateway.md` | *Failure Scenarios* | rows for a cache read failing on a listing route, a rolling update with two asset hashes, and `ui.enabled` false |
| `docs/specs/gateway.md` | *Amendments* | a second amendment block for the console, in the shape of the first: the rows above, and the documents updated with the implementation |
| `docs/specs/auth.md` | *Non-goals* | "A UI, and the listing endpoints it would need, is a later document" becomes a pointer to `ui.md` |
| `docs/specs/auth.md` | *The `/auth/` routes* | logout's fallback `302` to `/` lands on `/ui/` when `ui.enabled` |
| `docs/specs/auth.md` | *What is redirected* | "a future UI's JSON requests" names `ui.md` |
| `docs/specs/auth.md` | *Testing* | the two end-to-end lanes gain the console steps of `ui.md` *End to end* |
| `docs/specs/ui.md` | *Layout and embedding*, *Dependencies*, *Unit*, *End to end* | the Pico file is `pico.classless.min.css` (the class-less build's published name) wherever `pico.min.css` appears, and its size is about 69 KiB; *End to end* says the two proofs run inside the existing authentication scenarios |
| `docs/specs/ui.md` | *Layout and embedding* | the tree hash frames each file as length-prefixed path and length-prefixed content, and `index.html` is hashed but has no hashed serving route |
| `docs/specs/ui.md` | *Request algorithm for the listing endpoints* | the readiness step is the readiness `internal/httpapi` composes for every `/v1` route (`auth.md` *Request algorithm*: discovery, and the issuer when the browser flow is configured), not `HasSynced()` alone |
| `docs/specs/ui.md` | *Unit*, the `internal/httpapi` bullet | the fake holds no selectorless Service: `ServiceRef` carries no selector, so what the HTTP layer receives is already selector-independent; the two sentences about a selectorless Service and a namespace holding only one move to the `internal/k8s` bullet, which is where selector presence is decided and proven |
| `docs/specs/ui.md` | *Unit*, the `internal/httpapi` bullet | "no listing response contains a string that matches an IP address, a `podIP` field, or a port number" becomes "no Service or namespace listing exposes a Pod-discovered or selected backend port, and no listing response contains a string that matches an IP address or a `podIP` field" — `/v1/limits` returns the configured allowlists and default by design (*Limits*), and the tests keep asserting that the two list responses contain no `6060` |
| `docs/specs/ui.md` | *Audit and metrics*, *Unit* | the `ui` code set gains `internal_error`: any status the console wrote outside `2xx`, `3xx`, `404`, and `405`; the set stays closed |
| `docs/specs/ui.md` | *Changes to the accepted designs* | "Accepting this document amends the following text in the same change" becomes "The following text is amended to match this document", which is the fact once this task lands |
| `.agents/rules/100-project-map.md` | *Planned Structure* | `internal/ui/` — the console: embedded page and vendored browser libraries; sole user of `go:embed` |
| `.agents/rules/100-project-map.md` | *External HTTP API* | the four listing routes; `/ui/`, `/ui/static/{hash}/{file}`, and `/`, present only when `ui.enabled` |
| `docs/README.md` | *Where Contributors Start* | `specs/auth.md` and `specs/ui.md` beside the PGO spec (the authentication spec is not listed there today) |

- [x] **Validate and commit**

```bash
semlf check docs/specs/gateway.md docs/specs/auth.md docs/specs/ui.md .agents/rules/100-project-map.md docs/README.md
mise run check
git add docs/specs/ .agents/rules/100-project-map.md docs/README.md
git commit -m "docs(spec): amend the accepted designs for the console"
```

---

## Configuration

**Files:**
- Modify: `internal/config/config.go`, `config_test.go`
- Create: `internal/config/testdata/ui-*.yaml`

**Produces:**

```go
package config

// UIConfig is the console block.
// Enabled is restart-only: it decides which routes the handler registers.
type UIConfig struct {
    Enabled bool `yaml:"enabled" env:"UI_ENABLED" default:"false"`
}

// Config gains, after PGO and before Realms:
    UI UIConfig `yaml:"ui"`

// validateUI holds ui.enabled to the browser flow under oidc:
// a console that cannot log a browser in serves nobody.
func validateUI(cfg *Config) error
```

`validate` calls `validateUI` after `validateAuth` and before `validatePGO`,
so the rule sees an `auth` block that has already been judged.
The rule is `cfg.UI.Enabled && cfg.Auth.Mode == ModeOIDC && cfg.Auth.OIDC.Browser == nil`
→ `ui.enabled requires auth.oidc.browser when auth.mode is oidc`;
`validateAuth` has already required `cfg.Auth.OIDC` under `oidc`, so the pointer is safe to read.
`basic` and `disabled` need nothing (*Configuration*).

- [x] **Write the configuration tests**

`config_test.go` gains rows over `testdata/ui-*.yaml` fixtures, one per row,
in the table that drives `Load`.
The rows restate *Configuration* and the `internal/config` bullet of *Unit*.

| Subtest | Configuration | Expect |
|---|---|---|
| ui absent | the disabled-mode fixture with no `ui` key | loads; `UI.Enabled == false` |
| ui from file | `ui: {enabled: true}` under `mode: disabled` | loads; `UI.Enabled == true` |
| ui from env | no `ui` key, `PROFGATE_UI_ENABLED=true` in the environment | `UI.Enabled == true` |
| ui env false wins over file | `ui: {enabled: true}`, `PROFGATE_UI_ENABLED=false` | `UI.Enabled == false`: the environment overrides the file, as every other key |
| ui not boolean | `ui: {enabled: yes-please}` | error names `ui.enabled` |
| ui unknown key | `ui: {path: /console}` | error names the key (yaml `KnownFields`) |
| ui under basic | `ui: {enabled: true}` with the `basic` fixture over TLS | loads |
| ui under oidc without browser | `ui: {enabled: true}` with the `oidc` fixture and no `browser` block | error text is `ui.enabled requires auth.oidc.browser when auth.mode is oidc` |
| ui under oidc with browser | the same with the browser fixture's block | loads |
| ui off under oidc without browser | `ui: {enabled: false}` with the `oidc` fixture and no `browser` block | loads: the rule reads only an enabled console |

- [x] **Run the tests and watch them fail**

- [x] **Implement**

Add `UIConfig`, the `UI` field, and `validateUI` as written above.
Error text follows the existing style: the key path and the rule.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/config/
mise run lint && mise run test && mise run check
git add internal/config/
git commit -m "feat(config): add ui.enabled"
```

---

## Catalog on the seam

**Files:**
- Modify: `internal/k8s/discovery.go`
- Create: `internal/k8s/catalog.go`, `catalog_test.go`
- Modify: `internal/httpapi/fixtures_test.go`, `internal/pgo/fixtures_test.go`
  (the two fake `Discovery` implementations;
  `grep -rn 'HasSynced() bool' --include='*_test.go' .` lists them)

**Produces:**

```go
package k8s

// ServiceRef names one Service in the cache.
type ServiceRef struct {
    Namespace, Name string
}

// Discovery gains:
    // Catalog lists the Services with a non-empty selector from the cache,
    // sorted by namespace then name.
    // An empty namespace means every namespace; a namespace the cache lacks is an empty list, not an error.
    // It issues no request; an error means the lister could not be read.
    Catalog(ctx context.Context, namespace string) ([]ServiceRef, error)

// Catalog implements Discovery over the Service lister and nothing else.
func (c *Cluster) Catalog(_ context.Context, namespace string) ([]ServiceRef, error)
```

`Catalog` calls `c.services.Services(namespace).List(labels.Everything())` when a namespace is given
and `c.services.List(labels.Everything())` otherwise,
keeps every Service whose `spec.selector` is non-empty,
sorts by namespace then name, and returns an empty non-nil slice when nothing qualifies.
`spec.type` is not read (*The realm filter*).
A lister error is returned wrapped: `fmt.Errorf("read services from cache: %w", err)`;
the HTTP layer maps every error from `Catalog` to `503 discovery_unavailable`,
so no new sentinel is needed.
The context is ignored for the reason `Targets` ignores it: the call reads memory and cannot block.

The two fake `Discovery` implementations gain `Catalog`.
The `internal/httpapi` fake gains a `catalog []k8s.ServiceRef` field, a `catalogErr error` field,
a `catalogCalls atomic.Int32` counter, and a `catalogNamespaces` record of the namespace argument of every call,
because the listing tests must prove `Catalog` was not called on a denied namespace (*Request algorithm for the listing endpoints*).
The `internal/pgo` fake returns `nil, nil`; PGO never lists the catalog.

- [x] **Write the seam tests**

`catalog_test.go` uses `startFixture` from `export_test.go`
and restates the `internal/k8s` bullet of *Unit*.

| Subtest | Fixture | Expect |
|---|---|---|
| every namespace | Services `a/one` (selector), `a/two` (selector), `b/three` (selector), `b/four` (no selector), `c/five` (no selector, `type: ExternalName`) | `Catalog(ctx, "")` is `[a/one a/two b/three]` in that order |
| one namespace | the same | `Catalog(ctx, "b")` is `[b/three]` |
| absent namespace | the same | `Catalog(ctx, "zzz")` is `[]` (non-nil, empty) with no error |
| selectorless whatever the type | `b/four` as `ClusterIP`, `c/five` as `ExternalName`, and a `NodePort` Service without a selector | none listed |
| reads only the cache | after `startFixture`, `cs.ClearActions()`, then `Catalog` with and without a namespace | `cs.Actions()` is empty: the lister answered without a request |
| stays within the tuples | the actions recorded from fixture start through the `Catalog` calls | every action passes `isGranted` (the helper `eligibility_test.go` already uses) |
| reflects an added Service | create `a/six` with a selector through the fake clientset after start, then `waitCache` until `Catalog(ctx, "a")` holds three entries | it holds `[a/one a/six a/two]` |
| reflects a deleted Service | delete `a/one` through the fake clientset, then `waitCache` | `Catalog(ctx, "a")` no longer names it |
| sorted | Services created in the order `b/z`, `a/y`, `a/x` | `Catalog(ctx, "")` is `[a/x a/y b/z]` |

- [x] **Run the tests and watch them fail to compile**

- [x] **Implement**

`catalog.go` holds `Catalog` as described.
The interface method is added to `discovery.go` with the comment above,
and the package comment on `discovery.go` gains "and list the Services it holds".

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/k8s/ ./internal/httpapi/ ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/k8s/ internal/httpapi/fixtures_test.go internal/pgo/fixtures_test.go
git commit -m "feat(k8s): list selector-bearing services"
```

---

## Listing endpoints

**Files:**
- Modify: `internal/metrics/recorder.go`
- Modify: `internal/httpapi/server.go`, `realm.go`, `fixtures_test.go`, `server_test.go`, `realm_test.go`
- Create: `internal/httpapi/listing.go`, `listing_test.go`

**Produces:**

```go
package metrics

// The route families of the four listing endpoints and the console.
const (
    EndpointNamespaces Endpoint = "namespaces"
    EndpointServices   Endpoint = "services"
    EndpointWhoami     Endpoint = "whoami"
    EndpointLimits     Endpoint = "limits"
    // EndpointUI covers /ui/, every path under it, and /; profile is fixed to "none".
    EndpointUI Endpoint = "ui"
)
```

```go
package httpapi

// The routes the gateway serves: the two interactive ones, the four listing ones, and the five PGO ones.
// routeKind gains four values, appended after kindCollectionCancel:
    kindNamespaces
    kindServices
    kindWhoami
    kindLimits

// isPGO, isCollectionScoped, and isListing are each an exhaustive switch over routeKind that names every kind
// in its true or its false case; none compares kinds by order, so declaration order carries no meaning.
// isPGO: the five PGO kinds. isCollectionScoped: kindCollection, kindCollectionProfile, kindCollectionCancel.
func (k routeKind) isPGO() bool
func (k routeKind) isCollectionScoped() bool

// isListing reports whether the route is one of the four listing endpoints,
// which run the algorithm up to the realm step and then read the cache or the configuration.
func (k routeKind) isListing() bool

// listingRouteRE matches /v1/namespaces/{namespace}/services; the namespace is validated as a DNS-1123 label.
var listingRouteRE = regexp.MustCompile(`^/v1/namespaces/([^/]+)/services$`)

// The response shapes of Response shapes, field for field.
type namespacesBody struct {
    Namespaces []string `json:"namespaces"`
}
type servicesBody struct {
    Namespace string   `json:"namespace"`
    Services  []string `json:"services"`
}
type whoamiBody struct {
    Principal string    `json:"principal"`
    Realm     realmView `json:"realm"`
    Auth      authView  `json:"auth"`
}
type realmView struct {
    Name       string   `json:"name"`
    Namespaces []string `json:"namespaces"`
    Services   []string `json:"services"`
    Profiles   []string `json:"profiles"`
    PGO        pgoFlags `json:"pgo"`
}
type pgoFlags struct {
    Read      bool `json:"read"`
    Collect   bool `json:"collect"`
    Configure bool `json:"configure"`
}
type authView struct {
    Mode   string `json:"mode"`
    Logout string `json:"logout,omitempty"`
}
type limitsBody struct {
    CPUSeconds   int       `json:"cpuSeconds"`
    TraceSeconds int       `json:"traceSeconds"`
    Profiles     []string  `json:"profiles"`
    Pprof        pprofView `json:"pprof"`
    PGO          pgoView   `json:"pgo"`
}
type pprofView struct {
    Default          portDefault `json:"default"`
    AllowedPorts     []int32     `json:"allowedPorts"`
    AllowedPortNames []string    `json:"allowedPortNames"`
}
type portDefault struct {
    Port     int32  `json:"port,omitempty"`
    PortName string `json:"portName,omitempty"`
}
type pgoView struct {
    Enabled bool `json:"enabled"`
}

// filterCatalog applies the realm's namespaces and services lists to what Catalog returned (The realm filter).
func filterCatalog(realm config.Realm, refs []k8s.ServiceRef) []k8s.ServiceRef

// namespacesOf is the sorted distinct namespaces of a filtered catalog.
func namespacesOf(refs []k8s.ServiceRef) []string

// serveListing answers one of the four listing routes after the realm step.
func (s *server) serveListing(w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config, p auth.Principal, realm config.Realm)
```

`isPGO` and `isCollectionScoped` are today ordered comparisons (`k >= kindPGOPolicy`, `k >= kindCollection`);
this task rewrites both as exhaustive switches so that a kind added later is classified by name, never by position,
and `exhaustive` makes the compiler refuse a switch that forgets one.
The classification, which the route-kind test restates row for row:

| Kind | `isPGO` | `isCollectionScoped` | `isListing` | `methods()` |
|---|---|---|---|---|
| `kindTargets` | false | false | false | `GET` |
| `kindProfile` | false | false | false | `GET` |
| `kindPGOPolicy` | true | false | false | `GET, PUT, DELETE` |
| `kindCollections` | true | false | false | `GET, POST` |
| `kindCollection` | true | true | false | `GET` |
| `kindCollectionProfile` | true | true | false | `GET` |
| `kindCollectionCancel` | true | true | false | `POST` |
| `kindNamespaces` | false | false | true | `GET` |
| `kindServices` | false | false | true | `GET` |
| `kindWhoami` | false | false | true | `GET` |
| `kindLimits` | false | false | true | `GET` |

The comments that count routes are refreshed in the same change:
the `routeKind` constant block's comment in `server.go` ("the two interactive ones and the five PGO ones")
and the `parseRoute` comment ("the seven routes") name eleven routes in three families,
and the `Recorder.Request` comment in `internal/metrics/recorder.go` gains the four listing endpoints
(`("namespaces","none")` and so on) and `ui`.
`parseRoute` tries the exact path `/v1/namespaces`, then `/v1/whoami`, then `/v1/limits`,
then `listingRouteRE`, before the two regular expressions it has today;
a `{namespace}` that is not a DNS-1123 label is `404 route_unknown`, as for the Service-scoped routes.
`methods()` answers `GET` for the four kinds.
`labels()` maps them to the four new `Endpoint` values with `labelNone`.
`realmAllows` admits `kindNamespaces`, `kindWhoami`, and `kindLimits` unconditionally
and `kindServices` when `listAllows(r.Namespaces, rt.namespace)`;
`pgoAllows` lists the four kinds beside `kindTargets` and `kindProfile`
(`exhaustive` is enabled in `.golangci.yml`, so every `switch` over `routeKind` names them).
In `ServeHTTP`, after the realm check and before the PGO branch,
`rt.kind.isListing()` dispatches to `serveListing`.
`serveListing` first refuses any query: `r.URL.RawQuery != ""` → `invalidParameter("this route takes no query parameter")`.
Then by kind:
`whoami` answers from `cfg` and `p`:
`Realm.Name` is `p.Realm`, the three lists are the configured slices copied,
`Auth.Mode` is `cfg.Auth.Mode`,
and `Auth.Logout` is `/auth/logout` exactly when `cfg.Auth.Mode == config.ModeOIDC && cfg.Auth.OIDC.Browser != nil`;
`limits` answers `cfg.Limits.CPUSeconds`, `cfg.Limits.TraceSeconds`, `config.Profiles()`,
`cfg.Discovery.Pprof` as the view (`Default` from whichever of `Port` and `PortName` is set; each allowlist copied, `[]` when empty),
and `cfg.PGO.Enabled`;
`namespaces` calls `Catalog(ctx, "")`, filters, and answers `namespacesOf`;
`services` calls `Catalog(ctx, rt.namespace)`, filters, and answers the names sorted.
A `Catalog` error → `503 discovery_unavailable` with the message `discovery cannot list services`,
never an empty `200`.
Success writes through the existing `writeJSON(w, http.StatusOK, body)` in `pgo.go`
(which sets `Content-Type: application/json`; `Cache-Control: no-store` is set at entry),
and records `status 200`, `code ok`.
Every slice in a body is built with `make([]T, 0, n)` so an empty list encodes as `[]`.
The audit record for the four routes is the default branch of `writeAudit`:
`namespace` set only for the Service list, every other target field empty, `seconds` zero.

- [x] **Write the listing tests**

`listing_test.go` uses the existing `harness` with `fakeDiscovery.catalog` set,
and a `fakeAuth` (from `auth_test.go`) where a principal other than the disabled default is needed.
Rows restate *Request algorithm for the listing endpoints*, *The realm filter*, *Response shapes*, *Errors*,
*Audit and metrics*, and the `internal/httpapi` bullet of *Unit*.
The fake holds, unless a row says otherwise,
`payments/checkout` and `payments/ledger` with selectors,
`orders/api` with a selector,
`orders/legacy` and `staging/only` with selectors,
and Targets carrying `PodIP` and `Port` so the non-disclosure row has something to catch;
the fake holds no selectorless Service because `ServiceRef` carries no selector and `Catalog` omits them upstream;
selectorless coverage belongs to `internal/k8s` (*Catalog on the seam*), as the amended spec's *Unit* section says.

| Subtest | Request | Expect |
|---|---|---|
| route table | each of the four paths | `200`; `/v1/namespaces/` (trailing slash), `/v1/namespaces/x/services/`, `/v1/whoami/x`, `/v1/limit` are `404 route_unknown` |
| namespace label | `/v1/namespaces/Bad_NS/services`, a 64-character label | `404 route_unknown` each |
| method | `POST`, `PUT`, `DELETE`, `HEAD` on each of the four | `405 method_not_allowed`, `Allow: GET` |
| not ready | `synced=false` on each of the four | `503 not_ready`; `Catalog` not called |
| readiness is the closure | `synced=true`, `ready=false` | `503 not_ready` |
| access_token | `?access_token=x` on each | `400 invalid_parameter`; `Authenticate` not called |
| unauthenticated | `fakeAuth` failing `401` on each | `401 unauthenticated`; `Catalog` not called |
| oidc fetch is a 401 | `authHarness("oidc")` with `fakeAuth` refusing `Failure{Status: 401, Reason: ReasonMissing}` (no `Redirect`), `GET` on each of the four with `Sec-Fetch-Mode: cors`, `Sec-Fetch-Site: same-origin` | `401 unauthenticated`, `WWW-Authenticate: Bearer realm="profgate"`, the JSON envelope; never `302` |
| oidc navigation is redirected | the same harness with `fakeAuth` refusing `Failure{Status: 401, Reason: ReasonMissing, Redirect: "/auth/login?return=<path>"}`, `GET` on each of the four with `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document` | `302`, `Location` equal to the `Redirect`, empty body, audit code `auth_redirect`; `Catalog` not called (the redirect is decided by the authenticator, as on every `/v1` route) |
| basic challenge | `authHarness("basic")` with `fakeAuth` refusing `401` | `WWW-Authenticate: Basic realm="profgate"` |
| unknown query | `?x=1`, `?port=6060`, `?` followed by nothing on each | `400 invalid_parameter` for the first two; a bare `?` is `200` (the raw query is empty) |
| query after realm | `/v1/namespaces/staging/services?x=1` under a realm denying `staging` | `403 realm_denied`: the realm step precedes the parameter step |
| filter both wildcards | realm `["*"]`/`["*"]` | namespaces `[orders payments staging]`; `payments` services `[checkout ledger]` |
| filter named services | `["*"]`/`[checkout api]` | namespaces `[orders payments]`; `payments` services `[checkout]`; `staging` services `[]` with `200` |
| filter named namespaces | `[payments]`/`["*"]` | namespaces `[payments]`; `payments` services `[checkout ledger]`; `orders` services `403 realm_denied` |
| filter both named | `[payments orders]`/`[ledger]` | namespaces `[payments]`; `orders` services `[]` with `200`: admitted, holds no named Service |
| named namespace the cache lacks | `[payments missing]`/`["*"]` | namespaces `[payments]`; `missing` services `200` with `[]` |
| denied namespace, present or absent | `[payments]`/`["*"]`; `GET /v1/namespaces/orders/services` and `/v1/namespaces/nowhere/services` | `403 realm_denied` with byte-identical bodies; `fakeDiscovery.catalogCalls` is `0` after both |
| catalog error | `catalogErr = errors.New("boom")` on the namespace list and on `payments` services | `503 discovery_unavailable` each; the body never says `boom` |
| empty catalog | `catalog = []` | namespaces `{"namespaces":[]}` byte for byte; `payments` services `{"namespace":"payments","services":[]}` |
| sorted | the fake holding `payments/ledger` before `payments/checkout` | both lists sorted ascending |
| catalog namespace argument | `payments` services | `catalogNamespaces` is `[payments]`; the namespace list called with `""` |
| whoami verbatim | realm `developer` with `namespaces: ["*"]`, `services: [checkout]`, `profiles: [cpu heap]`, `pgo: {read: true}` | body equals the *Who am I* shape with those values, `pgo.collect` and `pgo.configure` `false`, `auth.mode` `disabled`, no `logout` key, `principal` `anonymous` |
| whoami logout | `cfg.Auth.Mode = oidc` with a `Browser` block, `fakeAuth` admitting `alice`/`developer` | `auth.logout` is `/auth/logout`, `principal` `alice` |
| whoami no logout under oidc without browser | `Mode = oidc`, `Browser = nil` | no `logout` key |
| whoami under basic | `Mode = basic` | `auth.mode` `basic`, no `logout` key |
| limits numeric default | `Pprof.Port = 6060`, `AllowedPorts = [6060 6061]`, `AllowedPortNames = [pprof pprof-alt]`, `CPUSeconds 60`, `TraceSeconds 30`, `PGO.Enabled true` | body equals the *Limits* shape with those values and `profiles` in the order `config.Profiles()` returns |
| limits named default | `Pprof.Port = 0`, `PortName = pprof` | `pprof.default` is `{"portName":"pprof"}` and carries no `port` key |
| limits empty allowlists | both lists nil | `"allowedPorts":[]` and `"allowedPortNames":[]`, never `null` |
| limits pgo off | `PGO.Enabled false` | `"pgo":{"enabled":false}` |
| no address leaks | every `200` body above with the fake's Targets carrying `PodIP 10.1.2.3` and `Port 6060` | no body matches `\b\d{1,3}(\.\d{1,3}){3}\b`, contains `podIP`, or, for the two list routes, contains `6060` |
| hostile names | `fakeAuth` admitting principal `<b>"alice"&'`, realm lists holding `<x>` entries in `namespaces`, `services`, and `profiles`, and the catalog holding `payments/<svc>` and `<ns>&'/checkout` (bypassing label validation, which the fake does not apply, so the hostile namespace reaches the namespace list, the realm view, and the `namespace` field of the Service list, which is read from the route and so is always a label) | each body — `whoami`, `namespaces`, and the `payments` Service list — decodes with `encoding/json` to the configured strings unchanged; the raw bytes hold `\u003c`, `\u003e`, `\u0026`, and `\"` (what `encoding/json` emits), never a bare `<`, `>`, or `&` |
| no-store | every response above | `Cache-Control: no-store`, `Content-Type: application/json` |
| no CORS | every response above, the `401` and `403` envelopes included | no header whose name starts with `Access-Control-` |
| route kinds | every `routeKind` value, the eleven in the classification table | `isPGO`, `isCollectionScoped`, `isListing`, and `methods()` answer the table's row, and `len(table) == int(kindLimits)+1` so a kind added without a row fails |
| audit namespaces | `/v1/namespaces` | one record with `principal anonymous`, empty `namespace`, `service`, `pod`, `profile`, `port`, `seconds 0`, `status 200`, `code ok` |
| audit services | `/v1/namespaces/payments/services` | the same with `namespace payments` |
| audit whoami and limits | each | as the namespaces row |
| metrics | each of the four | `Request(EndpointNamespaces|EndpointServices|EndpointWhoami|EndpointLimits, "none", "ok", _)` once; a `403` row records `realm_denied` on `EndpointServices` |

`server_test.go` and `realm_test.go` keep their rows;
`realm_test.go` gains one row per new kind for `realmAllows`.

- [x] **Run the tests and watch them fail to compile**

- [x] **Implement**

As described under *Produces*.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/metrics/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/metrics/
git commit -m "feat(httpapi): add the listing endpoints"
```

---

## Console dispatch

**Files:**
- Modify: `internal/httpapi/server.go`, `errors.go`, `audit.go`, `fixtures_test.go`
- Create: `internal/httpapi/console.go`, `console_test.go`

**Produces:**

```go
package httpapi

// Deps gains:
    // Console serves /ui/ and /; nil means ui.enabled is false and both are 404 route_unknown.
    Console http.Handler

// WriteError writes the gateway's JSON error envelope; the console calls it for its own 404 and 405.
// It sets only gateway-owned headers and never a target header.
func WriteError(w http.ResponseWriter, status int, code, message string)

// request gains:
    // console marks a request under /ui/ or to /: counted under EndpointUI, never narrated in the audit log.
    console bool

// statusWriter captures the status the console wrote so the metrics row can be derived from it.
type statusWriter struct {
    http.ResponseWriter
    status int
}

func (w *statusWriter) WriteHeader(status int)
func (w *statusWriter) Write(b []byte) (int, error) // marks 200 when nothing was written yet

// consoleCode maps the status the console wrote to the closed set of Audit and metrics:
// 2xx and 3xx → "ok", 404 → "route_unknown", 405 → "method_not_allowed",
// and every other status → "internal_error", so a console that fails is counted as a failure
// and never as a route miss. The set stays closed: no status reaches the label as a number.
func consoleCode(status int) string

// serveConsole dispatches a path under /ui/ or exactly / to Console, or answers 404 route_unknown when it is nil.
func (s *server) serveConsole(w http.ResponseWriter, r *http.Request, q *request)
```

`writeError` is renamed `WriteError` everywhere in the package (`gofmt -r` or `sed`; no other change).
In `ServeHTTP`, before the `/auth/` dispatch,
`r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, uiPrefix)` → `serveConsole`,
where `uiPrefix = "/ui/"`.
The deferred block records the metrics row for every request as it does today
and calls `writeAudit` only when `!q.console` (*Audit and metrics*).
`serveConsole` sets `q.console = true`;
with `Console == nil` it fails `404 route_unknown` through `q.fail`;
otherwise it wraps `w` in a `statusWriter`, calls `Console.ServeHTTP`,
and sets `q.audit.status` and `q.audit.code = consoleCode(status)` from what was written.
`labels()` answers `(EndpointUI, labelNone)` when `q.console`.
The `Cache-Control: no-store` header `ServeHTTP` sets at entry stays set for the shell and for the envelopes;
the console overwrites it for the immutable assets (*Headers*),
which is why the console owns that header (*Package layout*).

- [x] **Write the dispatch tests**

`console_test.go` uses the `harness` with a new `console http.Handler` field passed as `Deps.Console`.
The fake console writes what a row programs: a status, a body, and a `Cache-Control` of its own.
Rows restate *Package layout*, *Audit and metrics*, and the `ui.enabled` bullets of *Unit*.

| Subtest | Request | Expect |
|---|---|---|
| nil console | `GET /ui/`, `GET /ui/static/abc/app.js`, `HEAD /`, `GET /` with `Console == nil` | `404 route_unknown` envelope each; `Request(EndpointUI, "none", "route_unknown", _)`; no audit record |
| dispatch | `GET /ui/?ns=x` with a fake writing `200` | the fake was called with the request unchanged; the response is the fake's; `Request(EndpointUI, "none", "ok", _)`; no audit record |
| root | `GET /` with a fake writing `302` and `Location: /ui/` | passed through; code `ok` |
| deeper paths | `GET /ui/static/h/vendor/preact/preact.module.js` | the fake was called |
| not the prefix | `GET /ui`, `GET /uix`, `GET /v1/ui/` | the fake was not called; `404 route_unknown` through the ordinary path, with an audit record |
| before /auth/ and /v1 | `GET /ui/` with `AuthRoutes` set and a realm that would deny everything | the fake was called; neither `Authenticate` nor `ServeAuth` ran |
| 404 from the console | the fake writing `404` | code `route_unknown` |
| 405 from the console | the fake writing `405` | code `method_not_allowed` |
| other statuses | the fake writing `500`, `503`, `400`, and `418` | code `internal_error` each; the status recorded as written |
| consoleCode table | `consoleCode` over `200`, `204`, `302`, `304`, `404`, `405`, `400`, `500`, `503` | `ok`, `ok`, `ok`, `ok`, `route_unknown`, `method_not_allowed`, `internal_error`, `internal_error`, `internal_error`; the function's result is always one of those four strings |
| body without WriteHeader | the fake calling `Write` only | status recorded as `200`, code `ok` |
| no readiness step | `synced=false` | the fake is still called: the shell has no readiness step |
| cache-control | the fake setting `Cache-Control: public, max-age=31536000, immutable` | the response carries the fake's value, not `no-store` |
| WriteError exported | `WriteError(rec, 404, "route_unknown", "no such route")` on a recorder | the same bytes and headers `q.fail` produces for a `404` |

- [x] **Run the tests and watch them fail to compile**

- [x] **Implement**

As described under *Produces*.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "feat(httpapi): dispatch /ui/ to a console"
```

---

## Vendor the browser libraries

**Files:**
- Create: `internal/ui/doc.go`, `vendor_test.go`,
  `internal/ui/static/vendor/MANIFEST`,
  `internal/ui/static/vendor/preact/{preact.module.js,LICENSE}`,
  `internal/ui/static/vendor/htm/{htm.module.js,LICENSE}`,
  `internal/ui/static/vendor/pico/{pico.classless.min.css,LICENSE}`

**Produces:** the vendored tree and the tests that pin it; `doc.go` holds only the package comment
so the test file has a package to live in.

The three files were read from the npm registry when this plan was written.
Each line below is what the vendoring step must fetch and what the manifest must record;
the SHA-256 is of the file inside the tarball, unchanged, and the task fails if a hash differs.

| Project | Version | License | Tarball | Tarball SHA-256 |
|---|---|---|---|---|
| Preact | 10.29.8 | MIT | `https://registry.npmjs.org/preact/-/preact-10.29.8.tgz` | `b18cb0a457f3d43c7bb30391a74ade7d13e03bc6e77915e061c70c0fe1123299` |
| htm | 3.1.1 | Apache-2.0 | `https://registry.npmjs.org/htm/-/htm-3.1.1.tgz` | `2425b9bee11409177bcabc7f32e319926fc6690c1701c0b257c88bdff2d5ba90` |
| Pico CSS | 2.1.1 | MIT | `https://registry.npmjs.org/@picocss/pico/-/pico-2.1.1.tgz` | `4affbb56f49df6d5325610b06762156f1dcdbc6ef57cea5c136d7e8a00a8cd24` |

| Vendored path (under `vendor/`) | From the tarball | Bytes | SHA-256 |
|---|---|---|---|
| `preact/preact.module.js` | `package/dist/preact.module.js` | 11693 | `c30e721ebfdc6e2ad4c18c14d2dfb82667829c8aec27de1207774e3fc16858a8` |
| `preact/LICENSE` | `package/LICENSE` | | `1fe6958409c8c257a70c587a18b6f7f412b179b456630790d30b2ec9a8e4b7d4` |
| `htm/htm.module.js` | `package/dist/htm.module.js` | 1207 | `ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4` |
| `htm/LICENSE` | `package/LICENSE` | | `740725f7252e750af735d0028cc534970772f513331e9f68150fede8fb3ce00f` |
| `pico/pico.classless.min.css` | `package/css/pico.classless.min.css` | 71040 | `61207a40ffc02a42d1e50143651c121beab70ed413c934c1ff84fa263ba436b0` |
| `pico/LICENSE` | `package/LICENSE.md` | | `afaff063e044233f917b7807dccab022f09dca474f92b642846bee4850655bbf` |

The spec names the Pico file `pico.classless.min.css`, the class-less build's published name
(`css/pico.min.css` in the same package is the class-based build);
the class-less build is what the page needs — every control is a native element and the shell carries no class —
so that file is vendored under its upstream name.
htm ships no `NOTICE`; none is vendored.
Neither module contains an `import` statement, `eval(`, or `new Function(`;
`preact.module.js` ends with a `sourceMappingURL` comment naming `preact.module.js.map`,
which a browser requests only with developer tools open and which answers `404 route_unknown`;
the stylesheet references only `data:` URLs and declares no `@font-face` and no `@import`.

`MANIFEST` holds one line per file under `vendor/` other than itself,
six lines, six space-separated fields each:
`<path> <id> <version> <license> <source URL> <sha256>`,
where the path is relative to `vendor/`, the id is the single token `preact`, `htm`, or `pico`
(never the display name, so no field holds a space), the license is the SPDX identifier,
and the source URL is the tarball the file came from.
Lines starting with `#` are comments; the first says how the file is checked.

`vendor_test.go` holds its own copy of the two tables above as a Go map,
`wantVendored map[string]vendoredFile` keyed by path with fields `id`, `version`, `license`, `licensePath`, `sourceURL`, and `sha256`,
written out in the test file and not read from `MANIFEST`;
the test checks the files against the map and the manifest against the map,
so a change that edits a file and its manifest line together still fails until the test's own table is edited too.

- [x] **Write the vendor tests**

`vendor_test.go` reads the embedded tree (the embed directive lands in the next task;
until then the test opens `static/` with `os.DirFS`, and the next task switches it to the `embed.FS`).
Rows restate *Vendoring rule*, *Response headers and CSP*, and the `internal/ui` bullets of *Unit*.

| Subtest | Assertion |
|---|---|
| manifest covers the tree | every file under `vendor/` except `MANIFEST` has exactly one manifest line, and every manifest line names a file that exists |
| manifest hashes | each file's SHA-256 equals the `sha256` of `wantVendored`, and its manifest line carries the same value |
| manifest fields | each line has exactly six fields, and its id, version, license, and source URL equal `wantVendored` field for field (an exact string, not a prefix) |
| expected map covers the tree | the key set of `wantVendored` equals the set of files under `vendor/` except `MANIFEST`; each entry's `licensePath` names a file that exists |
| relative imports | no `.js` file under `vendor/` contains a static `import` or a dynamic `import(` whose specifier does not start with `./` or `../`; the check matches every `import` statement's quoted specifier and every `import(` call's quoted argument |
| no inline forms | no vendored `.js` or `.css` file contains `<script`, `<style`, ` style=`, or `\bon[a-z]+=`, and no `.js` file contains `eval(` or `new Function(` |
| license texts | `preact/LICENSE` and `pico/LICENSE` contain `MIT License`; `htm/LICENSE` contains `Apache License` and `Version 2.0` |
| fixture scans fail | the relative-import and inline-form checks, run on fixture strings holding `import x from "preact"`, `import("htm")`, `<script>1</script>`, `onclick=`, and `eval(`, each report a finding |

- [x] **Run the tests and watch them fail**

- [x] **Implement**

Fetch each tarball, verify it, and stream each member straight to its repository path;
the license files are renamed on the way (`package/LICENSE` and `package/LICENSE.md` both become `LICENSE`),
and the archive layout is the one the second table records:

```bash
cd internal/ui/static/vendor
curl -sSLO https://registry.npmjs.org/preact/-/preact-10.29.8.tgz
curl -sSLO https://registry.npmjs.org/htm/-/htm-3.1.1.tgz
curl -sSLO https://registry.npmjs.org/@picocss/pico/-/pico-2.1.1.tgz
sha256sum -c <<'EOF'
b18cb0a457f3d43c7bb30391a74ade7d13e03bc6e77915e061c70c0fe1123299  preact-10.29.8.tgz
2425b9bee11409177bcabc7f32e319926fc6690c1701c0b257c88bdff2d5ba90  htm-3.1.1.tgz
4affbb56f49df6d5325610b06762156f1dcdbc6ef57cea5c136d7e8a00a8cd24  pico-2.1.1.tgz
EOF
mkdir -p preact htm pico
tar -xzOf preact-10.29.8.tgz package/dist/preact.module.js      > preact/preact.module.js
tar -xzOf preact-10.29.8.tgz package/LICENSE                     > preact/LICENSE
tar -xzOf htm-3.1.1.tgz      package/dist/htm.module.js         > htm/htm.module.js
tar -xzOf htm-3.1.1.tgz      package/LICENSE                     > htm/LICENSE
tar -xzOf pico-2.1.1.tgz     package/css/pico.classless.min.css > pico/pico.classless.min.css
tar -xzOf pico-2.1.1.tgz     package/LICENSE.md                  > pico/LICENSE
rm preact-10.29.8.tgz htm-3.1.1.tgz pico-2.1.1.tgz
sha256sum preact/* htm/* pico/*   # must equal the second table, line for line
```

Then write `MANIFEST` from the second table.
Nothing in a vendored file is edited, and no tarball is committed.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/
git commit -m "build: vendor preact, htm, and pico"
```

---

## Static handler

**Files:**
- Create: `internal/ui/ui.go`, `ui_test.go`, `scan_test.go`,
  `internal/ui/static/index.html`, `app.css`, and first versions of `app.js` and `urls.js`
- Modify: `internal/ui/vendor_test.go` (switch to the embedded tree)

**Produces:**

```go
package ui

//go:embed static
var static embed.FS

const (
    // Prefix is where the console lives; the shell is served at exactly this path.
    Prefix = "/ui/"
    // assetPrefix is where the hashed tree is served: assetPrefix + hash + "/" + path.
    assetPrefix = "/ui/static/"
    // The two placeholders index.html carries, replaced by the hashed asset paths when the shell is rendered.
    stylesheetPlaceholder = "__STYLESHEET__"
    scriptPlaceholder     = "__SCRIPT__"
)

// Handler serves the console: the rendered shell at /ui/, the hashed tree under /ui/static/<hash>/, and the 302 from /.
type Handler struct {
    hash  string
    shell []byte
    files fs.FS // rooted at the static directory
}

// New builds the handler over the embedded tree.
func New() (*Handler, error)

// newFromFS builds the handler over any tree rooted like the static directory; tests hand it a modified copy.
func newFromFS(fsys fs.FS) (*Handler, error)

// Hash is the tree hash every asset is served under.
func (h *Handler) Hash() string

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request)

// treeHash is SHA-256 over every regular file of the tree in path order, index.html and vendor/MANIFEST included,
// truncated to 16 hex digits (Layout and embedding).
// Each file is framed as: the path length as an 8-byte big-endian integer, the path bytes,
// the content length as an 8-byte big-endian integer, the content bytes;
// the framing keeps two trees whose path/content boundaries differ from concatenating to the same input.
func treeHash(fsys fs.FS) (string, error)

// renderShell replaces the two placeholders; a placeholder that is absent or repeated is an error.
func renderShell(index []byte, hash string) ([]byte, error)

// contentType is the Content-Type of Headers: text/javascript, text/css, text/html, else mime.TypeByExtension, else application/octet-stream.
func contentType(name string) string

// setSecurityHeaders writes every header of Response headers and CSP except Cache-Control and Content-Type.
func setSecurityHeaders(h http.Header)
```

`ServeHTTP` runs, in order:
`setSecurityHeaders`;
method: anything but `GET` and `HEAD` → `Allow: GET, HEAD` and `httpapi.WriteError(w, 405, "method_not_allowed", "method <M> not allowed")`;
path `/` → `Location: /ui/`, `Cache-Control: no-store`, `302`, no body;
path `/ui/` (the query is ignored) → the shell with `Content-Type: text/html; charset=utf-8`, `Cache-Control: no-store`, `Content-Length`;
path `assetPrefix + h.hash + "/" + rest` where `rest` is non-empty, contains no `..` segment, no leading `/`, no `\`,
is not `index.html`, and names a regular file in `files` → the file with its `Content-Type`,
`Cache-Control: public, max-age=31536000, immutable`, `Content-Length`;
everything else → `httpapi.WriteError(w, 404, "route_unknown", "no such route")`.
`HEAD` writes the headers and status of the `GET` and no body:
the handler itself branches on `r.Method == http.MethodHead` after the headers are set and writes nothing,
for the shell, an asset, the `302`, the `404`, and the `405` alike
(for the two envelopes it sets `Content-Type: application/json` and `Cache-Control: no-store` as `WriteError` would, then the status).
`net/http` also drops a `HEAD` body on the wire, but the tests run the handler against `httptest.NewRecorder`,
which records what the handler wrote, so a body written on `HEAD` is a test failure and not something the wire hides.
No response sets `ETag` or `Last-Modified`, and none sets a CORS header (*Headers*, *Response headers and CSP*).
`index.html` is part of the tree hash like every other file but has no hashed serving route:
it is the shell's template, and serving it raw would hand out a page with unreplaced placeholders.
The `Content-Security-Policy` value is the exact string of *Response headers and CSP*, held in one constant.
`internal/ui` imports `internal/httpapi` for `WriteError` and nothing else from it;
`internal/httpapi` imports nothing of `internal/ui` (*Package layout*).

`index.html` is the shell of *Layout and embedding*:
`<!doctype html>`, `<html lang="en">`, a `<meta charset>`, a `<meta name="viewport">`, a `<title>`,
`<link rel="stylesheet" href="__STYLESHEET__">`, `<script type="module" src="__SCRIPT__"></script>`,
and `<main id="app"></main>`; no inline script, style, `style=`, or `on*=` attribute.
The placeholders render to `/ui/static/<hash>/app.css` and `/ui/static/<hash>/app.js`.
The shell references exactly two assets (*Layout and embedding*),
so `app.css` begins with `@import url("./vendor/pico/pico.classless.min.css");` and the console's own rules follow;
a relative `@import` resolves under the hashed prefix and needs no CSP source beyond `style-src 'self'`.
The first `app.js` imports `./vendor/preact/preact.module.js`, `./vendor/htm/htm.module.js`, and `./urls.js`,
binds htm to `h`, and renders one line of text into `#app`;
the first `urls.js` exports `apiURL(segments, params)` built with `new URL`, `encodeURIComponent`, and `URLSearchParams`.
Both are replaced by the *Console page* task and exist here so the scan tests have real files to run against
and the *Serve wiring* task has a shell and a script to serve.

- [x] **Write the handler tests**

`ui_test.go` builds handlers with `New()` and with `newFromFS` over an `fstest.MapFS` copy of the embedded tree.
Rows restate *Layout and embedding*, *Headers*, *Response headers and CSP*, and the `internal/ui` bullets of *Unit*.

| Subtest | Request | Expect |
|---|---|---|
| hash stable | `New()` twice | equal `Hash()`, 16 lowercase hex digits |
| hash moves | `newFromFS` over a copy with one byte of `app.css` changed; over a copy with a file added | a different hash each time |
| hash covers paths | a copy with two files' contents swapped | a different hash: the path is hashed with the bytes |
| hash covers index.html | a copy with one byte of `index.html` changed and nothing else | a different hash: the template is in the tree even though it has no hashed route |
| hash is framed | `treeHash` over `fstest.MapFS{"ab": "c"}` and over `fstest.MapFS{"a": "bc"}` | different hashes: the length prefixes separate path from content |
| shell | `GET /ui/` and `GET /ui/?ns=x&svc=y` | `200`, `text/html; charset=utf-8`, `Cache-Control: no-store`, `Content-Length` equal to the body, the body holds `/ui/static/<hash>/app.css` and `/ui/static/<hash>/app.js` and no `__` placeholder |
| security headers | every response of this table, envelopes included | `Content-Security-Policy` equal to the spec's string, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin` |
| no validators | every response of this table | no `ETag` and no `Last-Modified` header |
| no CORS | every response of this table, envelopes included | no header whose name starts with `Access-Control-` |
| assets | `GET /ui/static/<hash>/app.js`, `app.css`, `urls.js`, `vendor/preact/preact.module.js`, `vendor/htm/htm.module.js`, `vendor/pico/pico.classless.min.css`, `vendor/MANIFEST` | `200`, the embedded bytes, `Content-Length`, `Cache-Control: public, max-age=31536000, immutable`; `text/javascript; charset=utf-8` for `.js`, `text/css; charset=utf-8` for `.css`, `application/octet-stream` for `MANIFEST` |
| wrong hash | the same paths under `0000000000000000` | `404 route_unknown` envelope with `Cache-Control: no-store` |
| traversal | `/ui/static/<hash>/../app.js`, `/ui/static/<hash>//app.js`, `/ui/static/<hash>/vendor/..%2Fapp.js`, `/ui/static/<hash>/vendor\preact\LICENSE`, `/ui/static/<hash>/` | `404 route_unknown` each |
| index is not an asset | `/ui/static/<hash>/index.html` | `404 route_unknown` |
| directory | `/ui/static/<hash>/vendor`, `/ui/static/<hash>/vendor/` | `404 route_unknown` |
| other paths | `/ui`, `/ui/x`, `/ui/static/`, `/ui/static/<hash>` | `404 route_unknown` |
| root | `GET /` and `HEAD /` | `302`, `Location: /ui/`, `Cache-Control: no-store`, empty body |
| head | `HEAD /ui/` and `HEAD /ui/static/<hash>/app.js` | the headers of the `GET`, `Content-Length` included, and an empty body |
| head on a miss | `HEAD /ui/static/0000000000000000/app.js`, `HEAD /ui/x`, `HEAD /ui/static/<hash>/index.html` | `404`, `Content-Type: application/json`, `Cache-Control: no-store`, the security headers, and an empty body on the recorder |
| head is never 405 | `HEAD` on every path of this table | no `405`: `HEAD` is accepted wherever `GET` is, so the `405` rows are for other methods only |
| method | `POST /ui/`, `PUT /`, `DELETE /ui/static/<hash>/app.js` | `405 method_not_allowed`, `Allow: GET, HEAD`, the envelope |
| placeholders required | `newFromFS` over a copy whose `index.html` lacks `__SCRIPT__`; over one that repeats it | an error naming the placeholder |
| shell inline forms | the embedded `index.html` | contains no `<script>` with a body, no `<style`, no ` style=`, no `on[a-z]+=`; exactly one `<link rel="stylesheet"` and one `<script type="module"` |

`scan_test.go` restates the source scan of *Rendering response values*:

| Subtest | Assertion |
|---|---|
| no HTML interfaces | `app.js` and `urls.js` contain none of `innerHTML`, `outerHTML`, `dangerouslySetInnerHTML`, `insertAdjacentHTML`, `document.write`, `DOMParser` |
| paths live in urls.js | `app.js` contains no string literal (single, double, or backtick) beginning with `/v1`, `/ui`, or `/auth` |
| urls.js builds | `urls.js` contains `encodeURIComponent` and `URLSearchParams`, and no `+` whose left operand is a string literal (`['"][^'"]*['"]\s*\+`) |
| relative imports | `app.js` and `urls.js` pass the relative-import check of `vendor_test.go` |
| no inline forms | `app.js` and `urls.js` pass the inline-form and `eval` check of `vendor_test.go` |
| every JS file is scanned | the set of `.js` files outside `vendor/` is exactly `{app.js, urls.js}`, so a third file cannot escape the scan |
| no mutable top-level state | no line of `app.js` or `urls.js` matches `^(let|var)\b`: state lives on the component, not at module level (*Global Constraints*) |
| fixtures fail | each check above, run on a fixture string that violates it (`el.innerHTML = x`, `fetch("/v1/namespaces")`, `"/v1/" + ns`, `import h from "htm"`, `<div onclick=…>`, `let navigated = false;` at column zero), reports a finding |

- [x] **Run the tests and watch them fail to compile**

- [x] **Implement**

As described under *Produces*.
`vendor_test.go` now opens the tree with `fs.Sub(static, "static")` instead of `os.DirFS`.
The relative-import and inline-form helpers move to a shared unexported test helper both scan files call.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/
git commit -m "feat(ui): serve the embedded console shell"
```

---

## Serve wiring

**Files:**
- Modify: `cmd/profgate/serve.go`, `serve_test.go`

**Produces:** `serve` constructs `internal/ui` when `cfg.UI.Enabled` and passes it as `Deps.Console`.
The page at this point is the one-line `app.js` of the *Static handler* task;
the *Console page* task replaces it and needs this wiring to review the page in a browser.

```go
// serve, after apiDeps is built and before httpapi.New:
    if cfg.UI.Enabled {
        console, err := ui.New()
        if err != nil {
            logger.Error("console", "error", err)

            return 1
        }
        apiDeps.Console = console
        logger.Info("console enabled", "path", ui.Prefix)
    }

// gatewayOpts gains:
    // uiEnabled writes ui.enabled: true into the configuration.
    uiEnabled bool
```

`writeConfig` appends `ui:\n  enabled: true\n` when `o.uiEnabled`.
A construction error ends startup with exit 1 before anything binds, as an authenticator error does.

- [x] **Write the serve tests**

Rows extend `TestServe`.

| Subtest | Configuration | Expect |
|---|---|---|
| console off | the defaults | `GET /ui/` and `GET /` on the API listener are `404 route_unknown`; `GET /v1/whoami` is `200` with `principal anonymous`; the log holds no `console enabled` record |
| console on | `uiEnabled: true` | `GET /ui/` is `200 text/html` with the `Content-Security-Policy` header; the shell names `/ui/static/<hash>/app.js` and `GET` of that path is `200 text/javascript`; `GET /` is `302` to `/ui/`; the log holds `console enabled` |
| console on the ops listener | `uiEnabled: true` | `GET /ui/` on the ops listener is `404`: the console lives on the API listener only |
| listing before sync | `uiEnabled: true` with the preflight held | `GET /v1/namespaces` is `503 not_ready` while `GET /ui/` is `200` |
| oidc requires the browser flow | `mode: oidc` without a `browser` block and `uiEnabled: true` | `config.Load` refuses it: exit 2 and `ui.enabled requires auth.oidc.browser` in stderr |

- [x] **Run the tests and watch them fail**

- [x] **Implement**

As described under *Produces*; `serve.go` imports `github.com/arloliu/profgate/internal/ui`.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./cmd/profgate/
mise run lint && mise run test && mise run check
git add cmd/profgate/
git commit -m "feat(serve): mount the console when enabled"
```

---

## Console page

**Files:**
- Modify: `internal/ui/static/app.js`, `urls.js`, `app.css`, `index.html` (the `<title>` only, if needed)

**Produces:** the console of *The page*, as an ES module using Preact class components and htm bound to `h`.
No Go code changes; the scan and handler tests of the *Static handler* task run unchanged against the new files,
and the gateway already serves whatever is in `static/` (*Serve wiring*).

`urls.js` exports, and is the only module that spells a `/v1`, `/ui`, or `/auth` path:

```js
// Every function builds with new URL(path, location.origin); segments pass through encodeURIComponent;
// queries are URLSearchParams. Nothing here concatenates a response value into a path.
export function namespacesURL()
export function servicesURL(ns)
export function targetsURL(ns, svc, port)            // port: {port} | {portName} | null
export function collectionsURL(ns, svc)
export function collectionURL(id)                     // id must match /^[0-9a-hjkmnp-tv-z]{20}$/ (pgo.md Identifier); throws otherwise
export function collectionProfileURL(id)
export function profileURL(ns, svc, profile, params)  // params: {seconds, pod, version, port, portName}, each optional
export function whoamiURL()
export function limitsURL()
export function loginURL(returnPath)                  // /auth/login?return=<returnPath>
export function logoutURL()
export function pageURL(ns, svc)                      // /ui/?ns=&svc= ; the return path the page sends
```

`app.js` holds:
`fetchJSON(url)` — `fetch(url, {credentials: 'same-origin'})`,
resolving to `{status, headers, body, error}`:
`body` is the decoded JSON when `Content-Type` starts with `application/json`,
`error` is the envelope only when that body is an object with string `error` and `code` (*Errors*),
and otherwise `error` is `HTTP <status> <statusText>`;
a rejected `fetch` resolves to `error` `request failed` with `status` `0`, and the caller offers a retry;
`fetchJSON` decides nothing about `401`: the caller does, because what a `401` means depends on the mode;
an `App` class component holding the state of *Controls* plus the bootstrap state below
and rendering the panels: identity, selection, request, Collections, and errors;
every response-derived value reaches the template as `${value}` in child or attribute position (*Rendering response values*);
no `style` prop, no `dangerouslySetInnerHTML`, no `ref` that touches `innerHTML`.

**Bootstrap.** The page does not know `auth.mode` until `/v1/whoami` answers,
so `componentDidMount` runs one request and nothing else until it succeeds:

| State | Trigger | Action |
|---|---|---|
| `booting` | mount, or the retry button | `fetchJSON(whoamiURL())`; no other request is in flight |
| `ready` | `whoami` `200` | keep `auth.mode`, `auth.logout`, `principal`, and `realm` from the body; then fetch `/v1/limits` and `/v1/namespaces`, and the Service list when the page's `ns` is listed |
| navigating | `whoami` `401` whose `WWW-Authenticate` starts with `Bearer`, and `this.navigatedToLogin` is `false` | set `this.navigatedToLogin = true`, `location.assign(loginURL(pageURL(ns, svc)))`; render nothing further |
| `signInRequired` | `whoami` `401` whose `WWW-Authenticate` starts with `Basic` (the browser's dialog was cancelled), or a `Bearer` `401` after `navigatedToLogin` is already `true` | show `sign in required` and a retry button that returns to `booting` |
| `error` | `whoami` `401` with no `WWW-Authenticate` or an unrecognized scheme; any other non-`200`; a rejected `fetch` | show the error as text (*Errors*) with a retry button that returns to `booting`; nothing is guessed about the mode |

After `ready`, a later `401` follows the mode the page now knows:
under `oidc` it navigates once, by the same `navigatedToLogin` rule, and is shown as an error afterwards;
under `basic` it shows `sign in required` with a retry that repeats the failed request;
under `disabled` it is an error like any other, because `disabled` never answers `401`.
The login query parameter is `return`, as [`docs/specs/auth.md`](../specs/auth.md) *The `/auth/` routes* defines it.

The *Controls* contract, restated so the implementation is checked against it in review:

| Control | Choices | Default | Sent as | On change |
|---|---|---|---|---|
| namespace | `/v1/namespaces` | the page's `ns` when listed; else none | page query `ns` | fetch the Service list; clear Service, Pod, version, Collections |
| Service | `/v1/namespaces/{ns}/services` | the page's `svc` when listed; else none | page query `svc` | fetch targets and, when offered, Collections; clear Pod and version |
| profile | `limits.profiles` filtered by `realm.profiles` | `cpu` when offered, else the first offered | path segment | show or hide the duration input |
| seconds | integer, `1` to the profile's limit | `30` for `cpu`, `1` for `trace`, or the limit when it is lower | `seconds=`, always sent for `cpu` and `trace` | none |
| port | `default`, then `allowedPorts`, then `allowedPortNames`; a free-form number field when `allowedPorts` is empty and a free-form name field when `allowedPortNames` is empty | `default` | `port=` or `portName=`, nothing for `default`; a non-empty free-form field wins; typing in one clears the other | refetch targets; clear Pod and version |
| Pod | `targets[].pod` | `any` | `pod=`, nothing for `any` | none |
| version | distinct `targets[].version` | `any` | `version=`, nothing for `any` | none |

And the rest of *The page*, each a checkbox for review:

- [x] The selection is `?ns=&svc=` in the page's query, written with `history.replaceState` on change; nothing else is remembered.
- [x] A bookmarked `ns` or `svc` not in the fetched list leaves the control unselected and shows `<value> is not listed` as text; the query keeps it.
- [x] A `seconds` outside `1..limit` disables the download link and names the bound as text beside the input.
- [x] The download is `<a href=URL download>`; the URL is also shown in a read-only `<input>`; the copy button renders only when `navigator.clipboard?.writeText` exists; the sentence beside it follows `auth.mode` (*Flow*).
- [x] `/v1/whoami` is the first and only request until it answers `200` (*Bootstrap*); the state table is followed row for row.
- [x] Under `oidc`, the first `401` navigates to `loginURL(pageURL(ns, svc))`;
  the page does this at most once per load and shows every later `401` as an error.
- [x] Under `basic`, a `401` shows `sign in required` with a retry button that repeats the request; where the logout link would be, the page says a Basic credential is the browser's to keep.
- [x] Under `disabled`, the principal and realm show with no sign-in or sign-out control.
- [x] The logout link renders exactly when `/v1/whoami` returned `auth.logout`.
- [x] `not_ready` retries every 2 seconds until the first `200`; the hints table of *Errors* is rendered as text next to `code` and `error`; every other code shows as is.
- [x] `service_not_found` from a download cannot be observed by the page (it is a navigation); the hint applies when a targets or Collections fetch answers it, and the page then refetches the Service list.
- [x] The Collections view renders only when `limits.pgo.enabled && whoami.realm.pgo.read`; the table columns and the detail fields are those of *Controls*; the download link comes only from the detail record, only when `state === 'completed' && artifact !== null`, and only for an `id` that `collectionURL` accepts; `reason` shows beside `failed` and `cancelled`; unknown `origin` and `reason` show verbatim.
- [x] `app.css` is a few dozen lines on top of Pico: the panel grid, the URL field, the error box; no `url()` to anything but a relative path, no `@font-face`.

The once-per-load rule is `this.navigatedToLogin`, a field of the mounted `App` instance and not a module-level variable
(*Global Constraints*: no top-level `let` or `var`); once set, every later `401` is shown as an error.
A reload mounts a fresh instance and so resets it, which is what a user does after fixing a login,
and a loop between a page and a failing login is what the rule exists to prevent (*Signing in and out*).

- [x] **Read the scan tests**

There is no test to write first: `scan_test.go` and `ui_test.go` already run against these files.
Read them before writing the page so the constraints are in view.

- [x] **Implement**

Write `urls.js`, then `app.js`, then `app.css`, in that order, and keep each small enough to read in one sitting
(*What is not proven*).
Run `mise exec -- go test -race ./internal/ui/` after each file.

- [x] **Review by hand**

The spec accepts that no test runs `app.js`.
Before committing, open the checklist above and confirm each box against the code, not from memory;
for every place a response value reaches the template, confirm it is a child or an attribute interpolation.
Load the page once in a browser against a gateway started from this branch with `ui.enabled` under `auth.mode: disabled`
(the *Serve wiring* task has already landed, so `profgate serve` with `ui: {enabled: true}` serves the page),
and record in the commit body which browser and which of the checklist items were seen working.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/static/
git commit -m "feat(ui): build the console page"
```

---

## Deployment manifests and chart

**Files:**
- Modify: `deploy/base/configmap.yaml`, `deploy/deploy_test.go`
- Modify: `deploy/chart/profgate/values.yaml`, `templates/_helpers.tpl`, `README.md`, `deploy/chart_test.go`

The ClusterRole, the ClusterRoleBinding, the ServiceAccount, the Service, the NetworkPolicy,
the Deployment, and both security contexts do not change (*Configuration*);
`TestClusterRoleTuples` and `TestChartClusterRoleMatchesBase` stay green untouched.

- [x] **Write the manifest tests**

| Subtest | Assertion |
|---|---|
| base configmap comment | `TestConfigMap` gains: the base ConfigMap's `config.yaml` still loads through `config.Load` with `UI.Enabled == false`, and the file's text contains a commented `ui:` block naming `enabled: true` |
| chart default | `TestChartUI`: `loadRenderedConfig(t)` has `UI.Enabled == false`; the rendered `config.yaml` text contains `ui:` and `enabled: false`, so the key is visible in the ConfigMap |
| chart on | `loadRenderedConfig(t, "--set", "ui.enabled=true")` has `UI.Enabled == true` |
| chart on under basic | `authBasicValues(t)` plus `ui.enabled=true` | loads with `UI.Enabled == true` |
| chart on under oidc with the browser flow | `authBrowserValues(t)` plus `ui.enabled=true` | renders and loads with `UI.Enabled == true` and `Auth.OIDC.Browser != nil`: the supported combination, with no chart guard of its own, because `config.Load` at startup refuses the unsupported one |
| chart checksum | `TestChartConfigChecksum` gains "turning the console on moves it": the annotation differs with `--set ui.enabled=true` |
| chart boolean guard | `TestChartBooleanTogglesAreValidated` gains `ui.enabled` |
| chart raw guards | `TestChartRejectsDerivedKeyOverrides` gains `config.ui.enabled` (wants `set ui.enabled instead`), `config.ui` null and `config.ui` scalar (want `config.ui must be a mapping`) |
| chart raw mapping ok | `--set-json 'config={"ui":{}}'` renders |
| chart README | `TestChartReadmeValues` (or the existing README assertion, if one exists; else a new one) asserts the *Values* table has a `ui.enabled` row |

- [x] **Run the tests and watch them fail**

- [x] **Implement**

`deploy/base/configmap.yaml` gains, after the `auth` block, a comment:
the console is off; to serve it at `/ui/`, uncomment `ui:` / `enabled: true`,
and under `auth.mode: oidc` configure `auth.oidc.browser` first.
`values.yaml` gains a top-level `ui:` block beside `tls`, `pgo`, and `auth`:
`enabled: false` with a comment saying what it serves, that it is restart-class,
that the console needs `auth.oidc.browser` under `oidc`,
and that an Ingress routing only `/v1` must add `/ui/`, `/auth/`, and `/`.
`_helpers.tpl` gains `profgate.uiEnabled` in the shape of `profgate.pgoEnabled` (through `profgate.boolValue`),
`profgate.configStructured` renders `ui:\n  enabled: <true|false>` after the `realms` block and before the PGO block,
and `profgate.validateNoDerivedOverrides` gains the two `config.ui` checks in the shape of the `config.pgo` ones:
a non-mapping `config.ui` fails with
`config.ui must be a mapping: null or a scalar replaces the whole ui block in the rendered configuration, so set ui.enabled instead`,
and `config.ui.enabled` fails with
`config.ui.enabled is not supported: the console is a documented value, so set ui.enabled instead`.
No `extraEnv` guard is added for `PROFGATE_UI_ENABLED`:
the env guards exist for keys the memory limit is derived from, and `ui.enabled` derives nothing.
`README.md` gains a `ui.enabled` row in *Values*,
and a short paragraph under *Configuration* saying what the console is, that it is off, and the Ingress paths.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./deploy/
mise run lint && mise run test && mise run check
git add deploy/
git commit -m "feat(deploy): add the chart ui.enabled value"
```

---

## End-to-end scenarios

**Files:**
- Modify: `test/e2e/harness_test.go`, `scenarios_auth_test.go`

The spec's *End to end* adds two scenarios "to the lanes auth.md defines";
in this repository those are the `auth-oidc-browser` and `auth-basic` scenarios,
each deploying one gateway whose ClusterRoleBinding is cluster-scoped and so cannot run twice at once.
The console checks therefore extend those two scenarios rather than register two more:
both gateways gain `ui.enabled`, and the steps below run inside them.
The registry, `lanes_test.go`, and `runners()` do not change;
`NeedsPodReach` stays true because the scenarios still pull a profile.

- [x] **Write the harness piece**

`gatewayConfigOptions` gains `UIEnabled bool`; `gatewayConfig` writes `ui:\n  enabled: true\n` when set.
Both authentication scenarios pass `UIEnabled: true`.

- [x] **Write the console steps**

In `scenarioAuthOIDCBrowser`, after the existing logout step (the jar is then empty):

| Step | Action | Assert |
|---|---|---|
| shell without a cookie | `GET https://gateway/ui/` with `navigationHeaders()` | `200`, `Content-Type` starts `text/html`, every header of *Response headers and CSP* present with its value, `Cache-Control: no-store` |
| head | `HEAD /ui/` | `200`, the same headers, empty body |
| asset | the `app.js` path read from the shell body | `200`, `text/javascript; charset=utf-8`, `Cache-Control: public, max-age=31536000, immutable` |
| fetch is not redirected | `GET /v1/whoami` with `Sec-Fetch-Mode: cors`, `Sec-Fetch-Site: same-origin`, no cookie | `401`, not `302`; `WWW-Authenticate: Bearer realm="profgate"` |
| login from the page | `GET /auth/login?return=%2Fui%2F%3Fns%3Dx` with `navigationHeaders()`, then the Dex walk the scenario already performs (`followToGateway` and the callback with `Sec-Fetch-Site: cross-site`) | the callback is `200 text/html` and its body names `/ui/?ns=x` twice: in the `<meta http-equiv="refresh">` and in the `Continue` link |
| shell with the cookie | `GET /ui/?ns=x` with the jar and `Sec-Fetch-Site: cross-site` | `200`: the shell has no authentication step |
| listings with the cookie | `GET /v1/whoami`, `/v1/limits`, `/v1/namespaces`, `/v1/namespaces/<ns>/services`, each with the jar and `Sec-Fetch-Site: same-origin` | `200` each; `whoami.principal` is `alice@example.com` and `whoami.realm.name` is `developer`; `namespaces` contains `<ns>`; `services` contains `testapp`; `limits.cpuSeconds` is `60`; no body contains an IPv4 address |
| logout lands on the console | `GET /auth/logout` with the jar; then `GET /` | `302` to `/`; then `302` to `/ui/` |
| audit | the gateway log | one `request` record per listing route with `principal alice@example.com`, the Service-list record with `namespace <ns>`; no record whose path is under `/ui/` (the audit line carries no path, so the assertion is: the count of records did not grow across the shell and asset requests) |

In `scenarioAuthBasic`, after the users-file rotation:

| Step | Action | Assert |
|---|---|---|
| shell | `GET /ui/` without a credential | `200`, `text/html`, the CSP header |
| challenge | `GET /v1/namespaces` without a credential | `401`, `WWW-Authenticate: Basic realm="profgate"` |
| listing | `GET /v1/namespaces` as `alice` | `200`; `namespaces` contains `<ns>` |
| limits | `GET /v1/limits` as `alice` | `200`; `cpuSeconds 60`, `traceSeconds 60`, `pprof.default` is `{"port":6060}`, both allowlists `[]`, `pgo.enabled false` |

- [x] **Run the suite on the current lane**

```bash
PROFGATE_E2E_LANE=current mise run test:e2e
```

- [x] **Validate and commit**

```bash
mise exec -- go vet -tags e2e ./test/e2e/...
mise run lint && mise run test && mise run check
git add test/e2e/
git commit -m "test(e2e): prove the console over the wire"
```

---

## Documentation

**Files:**
- Modify: `docs/api.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/README.md`, `CHANGELOG.md`,
  `deploy/chart/profgate/README.md` (if the deployment task left anything)
- Create: `docs/console.md`

- [x] **Update the guides**

| File | Change |
|---|---|
| `docs/api.md` | a section *Listing endpoints* after *Listing targets*: the four routes, their shapes from *Response shapes*, the realm filter in one paragraph, `curl` examples, and the sentence that `/v1/limits` discloses the allowlists to any authenticated caller; *How a request is processed* gains one sentence saying the listing routes stop after the parameter step and read the cache; the *Errors* table needs no new code |
| `docs/configuration.md` | a `## ui` section between `pgo` and `realms`: the table row for `enabled`, restart-class, and the `oidc` rule; *Cross-Key Validation* gains the rule; *Examples* gain a `ui: {enabled: true}` line in the `oidc` example |
| `docs/deployment.md` | under *Installing*, a paragraph *The console*: `ui.enabled`, the Ingress paths `/ui/`, `/auth/`, and `/` when an Ingress routes only `/v1`, and that nothing else in the deployment changes; *Metrics* lists the five `endpoint` values and the four `ui` codes; *Audit log* says `/ui/` writes no line |
| `docs/console.md` | the user guide: enabling the console, what it shows, signing in under each mode from the user's view, the download and the copied URL and which mode each works in, the Collections view and when it appears, the rolling-update caveat, and what the page never does (render profiles, store anything, write PGO state) |
| `docs/README.md` | the guide list gains *Use the console:* `console.md`; the `plans/` sentence names five plans |
| `deploy/chart/profgate/README.md` | confirm the `ui.enabled` row and paragraph landed in the deployment task |
| `CHANGELOG.md` | under `## [Unreleased]` *Added*: the console and `ui.enabled`, the four listing endpoints, `Catalog` on the seam, the five `endpoint` label values, the chart value; *Changed*: `/` now redirects to `/ui/` when the console is enabled and logout lands there |

- [x] **Validate and commit**

```bash
semlf check docs/api.md docs/configuration.md docs/deployment.md docs/console.md docs/README.md CHANGELOG.md deploy/chart/profgate/README.md
mise run lint && mise run test && mise run check
git add docs/ CHANGELOG.md deploy/chart/profgate/README.md
git commit -m "docs: describe the web console"
```

---

## Finish the plan

- [x] Confirm the `main` run passed every lane
  (the existing workflows need no change: `check.yml` covers the new unit tests and `e2e.yml` the lanes;
  `e2e.yml` ignores `docs/**` and `**/*.md`, so the amendment and documentation commits run only `check.yml`).
- [x] Decide whether `internal/ui` changes should trigger the end-to-end suite before a PR
  and record the decision in `.agents/rules/500-validation-and-workflow.md` either way
  (the shell and headers are proven there; the page itself is not).
- [x] In the same change: set line 3 of this file to `**Status:** Done` and add line 4
  `**Outcome:** <tag or commit that shipped the console>`.
- [x] `mise run lint && mise run test && mise run check`;
  `git add docs/plans/ui.md .agents/rules/`; `git commit -m "docs: mark the console plan done"`.

---

## Self-Review

- Spec coverage:
  routes and the request algorithm for the listing endpoints (*Listing endpoints*, *Console dispatch*);
  the realm filter and its four combinations (*Listing endpoints*);
  response shapes, non-disclosure of `/v1/limits`, Collections read-only (*Listing endpoints*, *Console page*);
  the page, its flow, controls, rendering rules, sign-in and sign-out, errors (*Console page*);
  static assets, layout, headers, vendoring rule (*Vendor the browser libraries*, *Static handler*);
  response headers and CSP (*Static handler*);
  errors (no new code; *Listing endpoints* and *Static handler*);
  audit and metrics (*Listing endpoints*, *Console dispatch*);
  failure scenarios (each row is a test row in *Listing endpoints*, *Static handler*, or *Serve wiring*,
  except the rolling-update row, which is documented and not testable in one process);
  configuration (*Configuration*, *Deployment manifests and chart*);
  testing (every task names its slice; the two end-to-end proofs live in the two authentication scenarios);
  dependencies (no `go get`; the manifest);
  package layout (`internal/ui` as listed; `Catalog`; `Deps.Console`; `WriteError`);
  the amendments the spec's table lists (*Amendments to the accepted designs*), and the documents it says are updated with the implementation (*Documentation*).
- Types: `config.UIConfig`, `k8s.ServiceRef`, the four `routeKind` values, `listingRouteRE`,
  `namespacesBody`, `servicesBody`, `whoamiBody`, `realmView`, `pgoFlags`, `authView`, `limitsBody`, `pprofView`, `portDefault`, `pgoView`,
  `statusWriter`, `httpapi.WriteError`, `Deps.Console`, `request.console`,
  the five `metrics.Endpoint` values, `ui.Handler`, `ui.New`, `ui.Prefix`, and the unexported `treeHash`, `renderShell`, `contentType`, `setSecurityHeaders`
  are each defined once, in the task that first needs them, and consumed by those names afterwards.
- Task order compiles at every step:
  amendments → config → k8s → listing → dispatch → vendor → static handler → serve → page → deploy → e2e → docs.
  The amendments come first so no task implements from a spec sentence this plan knows to be false.
  The serve wiring precedes the page so the page task can load its own result in a browser and commit on its own;
  the one-line `app.js` of the static-handler task is what the serve tests see,
  and they read only the shell's asset path and the script's `Content-Type`.
  The k8s task extends both fake `Discovery` implementations in the same commit as the interface,
  so `internal/httpapi` and `internal/pgo` keep compiling
  (`internal/httpapi/fixtures_test.go:75-120`, `internal/pgo/fixtures_test.go:1624`).
  The dispatch task lands `WriteError` before `internal/ui` imports it;
  `internal/ui` imports `internal/httpapi` and `internal/httpapi` imports nothing of `internal/ui`,
  so the import runs one way, and `cmd/profgate` imports both.
  The vendor task's test opens the tree with `os.DirFS` because the embed directive arrives with the handler;
  the handler task switches it.
  The chart task runs after the config task because `TestChartConfigIsMergedAndParses` loads the rendered file through `config.Load`
  (`deploy/chart_test.go:247-261`), which must already know the `ui` key.
- Current-source facts the plan rests on:
  `ServeHTTP` records the metrics row and writes the audit line in one deferred block for every request
  (`internal/httpapi/server.go:273-278`), which is why the dispatch task adds `request.console` rather than a second path;
  the `/auth/` dispatch precedes `parseRoute` (`server.go:280-284`), and the console dispatch goes before it;
  `routeKind` is an `iota` whose predicates `isPGO` and `isCollectionScoped` are ordered comparisons
  (`server.go:136-141`), which the listing task replaces with exhaustive switches
  so the four new kinds can be appended without a position carrying meaning;
  the comments counting routes are at `server.go:123` and `server.go:167`,
  and the `Recorder.Request` comment at `internal/metrics/recorder.go:45-49`;
  `exhaustive` is enabled (`.golangci.yml:4`), so every `switch` over `routeKind` names the new kinds;
  the `/v1` readiness step is `s.ready()` (`server.go:317`, `auth.go:44-50`), the closure `cmd/profgate` passes,
  not `HasSynced()` alone as the console spec's readiness step words it —
  the amendments task corrects the spec,
  and the plan follows the code, which follows the authentication spec's composition;
  a browser navigation is redirected by the authenticator returning a `Failure` with `Redirect` set,
  which `internal/httpapi` answers as `302` with code `auth_redirect` (`internal/auth/auth.go:29-36`, `internal/httpapi/auth.go:110-114`),
  and the `WWW-Authenticate` value is `auth.Challenge(mode)` (`internal/auth/auth.go:96-107`, `internal/httpapi/auth.go:121-122`);
  `writeError` is unexported with the signature the spec asks `WriteError` to keep (`errors.go:30`);
  `writeJSON` already exists for `200` bodies (`internal/httpapi/pgo.go:200`);
  `listAllows` is the one predicate (`realm.go:59-61`);
  the seam interface has three methods and the Service lister is `c.services` (`internal/k8s/discovery.go:41-49`, `cluster.go:27`);
  `Targets` ignores its context for the reason `Catalog` does (`eligibility.go:17-21`);
  `isGranted` and `cs.Actions()` are how the cache-only property is already asserted (`eligibility_test.go:428-434`, `462-480`);
  `config.Profiles()` returns the eight names in the spec's order (`internal/config/config.go:338-348`)
  and `profileSpecs` carries the upstream defaults `30` and `1` (`internal/httpapi/profile.go:35-44`), which the page restates;
  `normalize` guarantees exactly one of `Port` and `PortName` is set (`config.go:477-485`, `487-490`);
  `validate` calls `validateAuth` then `validatePGO` (`config.go:539-543`), and `validateUI` goes between them;
  `Recorder.Request` applies no label validation (`internal/metrics/prometheus.go:156-159`), so the five values need only the constants;
  `apiDeps` is built at `cmd/profgate/serve.go:197-211` and `Console` is set beside `AuthRoutes`;
  `writeConfig` composes the test configuration from blocks (`cmd/profgate/serve_test.go:340-380`) and gains the `ui` block the same way;
  the chart renders the configuration from `profgate.configStructured` (`_helpers.tpl:574`) merged with the raw block (`:659-662`),
  guards booleans through `profgate.boolValue` (`:74`, `:87-88`), and refuses raw-block hatches in `profgate.validateNoDerivedOverrides` (`:294-328`);
  the base ConfigMap is parsed by `TestConfigMap` (`deploy/deploy_test.go:475`), so a comment is safe there;
  the end-to-end configuration is composed by `gatewayConfig` over `gatewayConfigOptions` (`test/e2e/harness_test.go:746-790`) with `cpuSeconds: 60` and `traceSeconds: 60`;
  the oidc scenario ends with a logout that empties the jar (`test/e2e/scenarios_auth_test.go:635-641`), which is where the console walk begins;
  `navigationHeaders()` and `followToGateway` are the walk's helpers (`scenarios_auth_test.go:288-290`, `705-725`);
  `docs/README.md` *Where Contributors Start* lists the gateway and PGO specs and not the authentication spec (`docs/README.md:46-52`);
  `AGENTS.md` already says *Four Specs, All Accepted* (`AGENTS.md:16`);
  the acceptance commit `fd96b57` touched only `AGENTS.md` and `docs/specs/ui.md`, so the spec's amendment table is still open;
  the vendored files' hashes, sizes, license texts, and archive member paths were computed from the tarballs named in *Vendor the browser libraries*;
  the chart's `authBrowserValues` helper (`deploy/chart_test.go:202-214`)
  is what the positive `oidc`-with-browser chart row builds on.
- Decided here because the spec leaves them to the implementer, recorded so nobody mistakes them for omissions:
  the Pico file is the class-less build under its upstream name `pico.classless.min.css`,
  because the spec names the class-less build and `pico.min.css` is not it;
  `index.html` is the shell's template, hashed with the rest of the tree and not served as an asset;
  the tree hash frames each file with length-prefixed path and content, which the spec leaves to the implementation;
  the manifest ids are the single tokens `preact`, `htm`, and `pico`,
  and `vendor_test.go` carries its own expected table so the manifest is checked, not trusted;
  the shell's placeholders are `__STYLESHEET__` and `__SCRIPT__`;
  `app.css` imports Pico with a relative `@import`, so the shell references exactly two assets;
  the `ui` metric code for a status outside `{2xx, 3xx, 404, 405}` is `internal_error`,
  a fourth bounded value the amendments task adds to the spec,
  so a failing console is counted as a failure and not as a route miss;
  `HEAD` bodies are suppressed by the handler and proven on `httptest.NewRecorder`, not left to `net/http`;
  a request under `/ui/` with `Console == nil` is counted under `EndpointUI` and writes no audit line, like one with a console;
  `/ui` without the trailing slash is `404 route_unknown`, as the spec's dispatch rule words it;
  `auth.logout` in `/v1/whoami` is decided from the configuration snapshot (`oidc` with a `browser` block),
  not from `Deps.AuthRoutes`;
  a listing route refuses a query by testing the raw query for emptiness, so a malformed query is refused the same way;
  the `oidc` once-per-load rule is a field of the mounted `App` instance,
  and the page's first request is `/v1/whoami` alone;
  the two end-to-end proofs extend the two authentication scenarios rather than register two more,
  because each needs a gateway with its own cluster-scoped ClusterRoleBinding;
  no `extraEnv` guard for `PROFGATE_UI_ENABLED`, because the env guards exist for keys the memory limit is derived from;
  the base ConfigMap gains only a comment;
  `docs/console.md` is a new guide, which the spec does not name but `docs/README.md`'s per-task guide list calls for.
- Left to the implementer by design: helper names inside test files,
  the exact wording of the page's texts,
  the `app.css` rules,
  and whether `serveListing` is one function with a `switch` or four small ones.
