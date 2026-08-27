# Client-Selected pprof Port Implementation Plan

**Status:** Done
**Outcome:** commit 337b492 on main; CI passed check and all three e2e lanes.

> **For agentic workers:** implement this plan one task at a time, in order;
> each task is written test-first and ends with its own validation block and commit.
> Run a task inline or hand it to a subagent, whichever fits its size.
> Checkboxes (`- [ ]`) track progress.

**Goal:** Let a client name the pprof port for one request,
as the *Port resolution* section of [`docs/specs/gateway.md`](../specs/gateway.md) now defines it:
a `port` or `portName` query parameter on the targets and profile endpoints,
two independent operator allowlists that bound what a client may name,
`400 port_not_allowed` before discovery for a value outside a non-empty list,
an audit field that records the selection as sent,
shipped manifests whose lists are empty,
and the unit and end-to-end layers that prove it.

**Architecture:** No new package.
`internal/config` gains the two allowlists on `PprofConfig` and the rule that decides whether a value passes;
`internal/k8s` gains `PortSelection`, carries it through `Discovery.Targets`, and resolves the port per Pod from it;
`internal/httpapi` parses the two parameters on both endpoints,
checks the allowlist after the realm and before discovery,
and records the selection in the audit line;
`internal/pgo` passes the zero selection on every `Targets` call and changes nothing else;
`deploy/` ships both lists empty in the kustomize ConfigMap and the chart;
`test/e2e/` gives the test app a second listener and adds the `ports-gateway` overlay with two scenarios.

**Tech Stack:** everything already pinned; no new module.
`fuda` already parses a slice field from a comma-separated environment value (`internal/types/converter.go`, `convertSlice`),
and `k8s.io/apimachinery/pkg/util/validation.IsValidPortName` already holds the container-port name rule `internal/config` applies to `portName` today.

**Spec:** [`docs/specs/gateway.md`](../specs/gateway.md),
whose closing *Amendments* section lists every heading the feature touched,
and the matching edits in [`docs/specs/pgo.md`](../specs/pgo.md).
Every behavior table below restates the spec for the task at hand;
where they differ the spec wins, and the plan is the bug.
Spec sections are cited by heading name; unqualified sections are the gateway spec's.
The unit-test cases in the spec's *Layers* section are normative:
each task below names its slice of them, and the task is done only when every bullet for its package passes.
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md),
whose boundary sentence the amendment reworded.

## Global Constraints

- Everything in the gateway, PGO, and authentication plans' constraints still holds.
- No new RBAC tuple, no new `internal/k8s` method, no change to the shipped ClusterRole;
  `Targets` gains a parameter, not a capability (*The seam*).
  Only `internal/k8s` imports `k8s.io/client-go`, unchanged.
- The realm is evaluated before the allowlist, and a refused port never reaches discovery (*Request algorithm*).
- The allowlists are global; no realm widens or narrows them (*Limits are not authorization*).
- An empty list permits any value of its parameter;
  the configured default `port` or `portName` always passes (*Port resolution*).
- Nothing the gateway generates carries the number a `portName` resolved to,
  and the `X-Pprof-Target-*` headers never carry a port (*Non-disclosure*).
  `Target.Port` stays unserialized.
- The port selection is never a metrics label (*Metrics*).
- PGO always uses the configured default:
  every `Targets` call in `internal/pgo` passes `k8s.PortSelection{}` (pgo spec, *Core decisions*).
- Global state stays as the gateway plan lists it.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on what you wrote.

---

## File Structure

```text
internal/config/config.go                   # PprofConfig gains AllowedPorts, AllowedPortNames; AllowsPort, AllowsPortName; validation
internal/config/config_test.go, testdata/allowed-ports*.yaml
internal/k8s/discovery.go                   # PortSelection; Targets takes it
internal/k8s/eligibility.go                 # Targets and eligible carry the selection; pprofPort(pod, sel)
internal/k8s/eligibility_test.go            # selection rows; a second Pod in the fixture
internal/httpapi/server.go                  # port parameters on both endpoints; allowlist step; beforeAllowlist test hook; discover passes the selection
internal/httpapi/profile.go                 # parsePortParams; port grammar
internal/httpapi/audit.go                   # + port field
internal/httpapi/fixtures_test.go           # fakeDiscovery records the selection and answers per name
internal/httpapi/server_test.go, profile_test.go, pgo_collections_test.go
internal/httpapi/export_test.go             # sets the beforeAllowlist hook on a built handler
internal/pgo/rounds.go                      # Targets(..., k8s.PortSelection{})
internal/pgo/fixtures_test.go               # fakeDiscovery records the selection
internal/pgo/rounds_test.go                 # one zero selection per round
deploy/base/configmap.yaml                  # allowedPorts: [], allowedPortNames: []
deploy/chart/profgate/values.yaml           # config.discovery.pprof.allowedPorts: [], allowedPortNames: []; no port
deploy/chart/profgate/README.md             # the raw-block example shows the two lists; the values table row for config
deploy/deploy_test.go, chart_test.go        # rendered keys asserted
test/e2e/testapp/main.go                    # second listener :6061; per-listener /hits
test/e2e/testapp/deployment.yaml            # second container port pprof-alt 6061/TCP
test/e2e/scenarios_test.go                  # the two port-selection scenarios; deployScopedGateway; hits helpers
test/e2e/registry.go, lanes_test.go         # the two scenarios, NeedsPodReach, the degraded-lane skip list
test/e2e/overlays/ports-gateway/            # a gateway whose lists hold only 6060 and pprof
README.md, docs/api.md, docs/configuration.md, docs/deployment.md, docs/README.md, CHANGELOG.md
```

---

## Configuration

**Files:**
- Modify: `internal/config/config.go`, `config_test.go`
- Create: `internal/config/testdata/allowed-ports.yaml`, `allowed-ports-range.yaml`, `allowed-ports-name.yaml`, `allowed-ports-dup.yaml`, `allowed-ports-dup-name.yaml`

**Produces:**

```go
package config

// PprofConfig names the default pprof port by number or by container-port name
// and bounds what a request may name instead (spec: Port resolution).
// Port 0 means unset; normalization sets it to 6060 when PortName is also empty.
// Each list bounds one request parameter and an empty list permits any value of it.
type PprofConfig struct {
    Port             int32    `yaml:"port"             env:"PPROF_PORT"               validate:"min=0,max=65535"`
    PortName         string   `yaml:"portName"         env:"PPROF_PORT_NAME"`
    AllowedPorts     []int32  `yaml:"allowedPorts"     env:"PPROF_ALLOWED_PORTS"      validate:"dive,min=1,max=65535"`
    AllowedPortNames []string `yaml:"allowedPortNames" env:"PPROF_ALLOWED_PORT_NAMES"`
}

// AllowsPort reports whether a request may name port n:
// the configured default always, any number when AllowedPorts is empty, otherwise a listed one.
func (p PprofConfig) AllowsPort(n int32) bool

// AllowsPortName reports whether a request may name the container port name:
// the configured default always, any name when AllowedPortNames is empty, otherwise a listed one.
func (p PprofConfig) AllowsPortName(name string) bool
```

The two methods live here so the allowlist rule is written once and `internal/httpapi` only asks the question.
`validate` gains the name grammar and the duplicate check for both lists;
the numeric range is the `dive` tag, which the registered tag-name function reports as `discovery.pprof.allowedPorts`.
A configuration that names neither list loads with both `nil`, which the methods read as empty.
`docs/configuration.md` is not touched here; the *Documentation* task writes its rows.

- [x] **Write the configuration tests**

`config_test.go` gains rows under `TestLoad`,
one fixture or environment variable per row (*Layers*, `internal/config`; *Configuration*).

| Subtest | Configuration | Expect |
|---|---|---|
| good unchanged | `good.yaml` | loads; `AllowedPorts` and `AllowedPortNames` are empty |
| lists from file | `allowed-ports.yaml`: `port: 6060`, `allowedPorts: [6060, 6061]`, `allowedPortNames: [pprof, pprof-alt]` | loads; both slices carry the four values in file order |
| ports from env | `PROFGATE_PPROF_ALLOWED_PORTS=6060,6061` on `good.yaml` | `AllowedPorts == [6060 6061]` |
| names from env | `PROFGATE_PPROF_ALLOWED_PORT_NAMES=pprof,pprof-alt` on `good.yaml` | `AllowedPortNames == [pprof pprof-alt]` |
| env with spaces | `PROFGATE_PPROF_ALLOWED_PORTS=6060, 6061` | `[6060 6061]`: the CSV reader trims leading space |
| port out of range | `allowed-ports-range.yaml`: `allowedPorts: [0]`; and `PROFGATE_PPROF_ALLOWED_PORTS=65536` | error names `discovery.pprof.allowedPorts` each |
| port not a number | `PROFGATE_PPROF_ALLOWED_PORTS=6060,abc` | error names `discovery.pprof.allowedPorts` |
| bad name | `allowed-ports-name.yaml`: `allowedPortNames: [Pprof]`; and `PROFGATE_PPROF_ALLOWED_PORT_NAMES=-x` | error names `discovery.pprof.allowedPortNames` and the offending value each |
| duplicate port | `allowed-ports-dup.yaml`: `allowedPorts: [6060, 6060]` | error names `discovery.pprof.allowedPorts` and `6060` |
| duplicate name | `allowed-ports-dup-name.yaml`: `allowedPortNames: [pprof, pprof]` | error names `discovery.pprof.allowedPortNames` and `pprof` |
| unknown key under pprof | a fixture with `discovery.pprof.allowedPort` | error `field allowedPort not found in type config.PprofConfig`, as `unknown nested` reports today |
| AllowsPort | table over `PprofConfig{Port: 6060, AllowedPorts: nil}`, `{Port: 6060, AllowedPorts: [7070]}`, `{PortName: "pprof", AllowedPorts: [7070]}` | empty list: 6060 and 9999 true; listed: 6060 true (default), 7070 true, 9999 false; name default: 7070 true, 6060 false |
| AllowsPortName | table over `{PortName: "pprof", AllowedPortNames: nil}`, `{PortName: "pprof", AllowedPortNames: ["metrics"]}`, `{Port: 6060, AllowedPortNames: ["metrics"]}` | empty list: `pprof` and `x` true; listed: `pprof` true (default), `metrics` true, `x` false; numeric default: `metrics` true, `pprof` false |
| neither port still 6060 | `neither-port.yaml` with `PROFGATE_PPROF_ALLOWED_PORTS=7070` | `Port == 6060`, `AllowedPorts == [7070]`: the lists do not take part in the default rule |

- [x] **Run the tests and watch them fail**

- [x] **Implement**

Add the two fields and the two methods.
In `validate`, after the existing `portName` check,
walk `AllowedPortNames` through `validation.IsValidPortName` and fail with `discovery.pprof.allowedPortNames %q: <messages>`,
then check both lists for a repeated entry and fail with `discovery.pprof.allowedPorts: duplicate entry 6060` or the name equivalent;
`slices.Contains` over the prefix is enough at these sizes.
`AllowsPort` returns true when `n == p.Port`, when `len(p.AllowedPorts) == 0`, or when the list contains `n`;
`AllowsPortName` mirrors it against `p.PortName` and `p.AllowedPortNames`.
`normalize` is unchanged: the lists have no default and take no part in the 6060 rule.
Two subtests compare `DiscoveryConfig` with `!=`, `good` and `env overrides`,
and both stop compiling once `PprofConfig` holds slices.
Replace each with field-wise assertions:
`VersionLabel`, `Pprof.Port`, and `Pprof.PortName` against the expected values,
and `slices.Equal(cfg.Discovery.Pprof.AllowedPorts, want...)` and `slices.Equal(... AllowedPortNames, ...)` against `nil` in both,
so a stray list in either fixture fails the test rather than slipping past a length check.
`Server`, `Limits`, and `Auth` keep their `!=` comparisons; they hold no slice.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/config/
mise run lint && mise run test && mise run check
git add internal/config/
git commit -m "feat(config): add pprof port allowlists"
```

---

## Discovery seam

**Files:**
- Modify: `internal/k8s/discovery.go`, `eligibility.go`, `eligibility_test.go`, `confirm_test.go`
- Modify: `internal/pgo/rounds_test.go`, for the zero-selection row
- Modify: every `Discovery.Targets` caller and fake in the same change, so the tree compiles at every step
  (`grep -rn 'Targets(' internal cmd test --include='*.go'` lists them:
  `internal/httpapi/server.go`, `internal/httpapi/fixtures_test.go`,
  `internal/pgo/rounds.go`, `internal/pgo/fixtures_test.go`;
  the end-to-end tree calls the HTTP endpoint and implements nothing)

**Produces:**

```go
package k8s

// PortSelection is the client's port choice for one request (spec: Port resolution);
// the zero value means the configured default.
// Port and PortName are never both set: the HTTP layer refuses that before discovery.
type PortSelection struct {
    Port     int32
    PortName string
}

type Discovery interface {
    // Targets returns the currently eligible backends of a Service
    // whose pprof port resolves under port.
    // Order is unspecified.
    Targets(ctx context.Context, namespace, service string, port PortSelection) ([]Target, error)
    HasSynced() bool
    Confirm(ctx context.Context, t Target) error
}
```

`pprofPort(pod, sel)` resolves in this order:
`sel.Port` when set, for every Pod without checking its declarations;
otherwise `sel.PortName` when set, as the TCP container port of that name, leaving a Pod without it ineligible;
otherwise the configured `Options.Port` or `Options.PortName` exactly as today.
`Target.Port` is the resolved number either way.

- [x] **Write the discovery tests**

`TestTargets` in `eligibility_test.go` gains a `sel PortSelection` column and a second Pod;
add a `fixture` helper that appends a Pod with its own name, UID, and address,
plus an endpoint for it in the baseline slice,
so a row can give the two Pods different port declarations.
Every existing row passes `PortSelection{}` and keeps its expectation.
The rows restate *Port resolution*, the *Eligibility* rule that resolves a pprof port for the Pod, and the `internal/k8s` bullet of *Layers*.

| Subtest | Fixture and selection | Expect |
|---|---|---|
| numeric selection ignores declarations | `Options.PortName: pprof`, Pod ports cleared, `sel.Port: 7070` | one target with `Port == 7070` |
| numeric selection over numeric default | `Options.Port: 6060`, `sel.Port: 6061` | `Port == 6061` |
| name selection resolves per Pod | `Options.Port: 6060`, Pod declares `pprof-alt` 6061 TCP, `sel.PortName: pprof-alt` | `Port == 6061` |
| name selection on one Pod of two | second Pod added without `pprof-alt`, `sel.PortName: pprof-alt` | only the first Pod is a target |
| name absent from every Pod | `sel.PortName: nowhere` | no targets, no error |
| name selection requires TCP | Pod's `pprof-alt` is UDP, `sel.PortName: pprof-alt` | no targets |
| name selection protocol unset | Pod's `pprof-alt` protocol `""`, `sel.PortName: pprof-alt` | `Port == 6061` |
| zero selection keeps the default name | `Options.PortName: pprof`, `sel` zero | the baseline target, as today |
| zero selection keeps the default number | `Options.Port: 7070`, `sel` zero | `Port == 7070`, as the `numeric port mode` row reads today |
| selection does not change the recorded requests | any row above | the recording transport still sees only the seven RBAC tuples |

`confirm_test.go` passes `PortSelection{}` at its three `Targets` calls and changes nothing else.

- [x] **Run the tests and watch them fail to compile**

- [x] **Implement**

`Targets` takes `port PortSelection` and hands it to `eligible`, which hands it to `pprofPort`.
`pprofPort(pod *corev1.Pod, sel PortSelection)`:
return `sel.Port` when non-zero;
when `sel.PortName` is non-empty look it up the way the configured name is looked up today;
otherwise run the existing configured-default branches.
Extract the named-port loop into `namedPort(pod, name) (int32, bool)` so the two name paths share one loop.
Update the callers in the same change:
`internal/httpapi/server.go` `discover` passes `k8s.PortSelection{}` for now;
the next task replaces it with the request's selection.
Then:
`internal/httpapi/fixtures_test.go` `fakeDiscovery.Targets` takes the parameter
and stores it in a `selections []k8s.PortSelection` slice under its mutex;
`internal/pgo/rounds.go` `targetsFor` passes `k8s.PortSelection{}`;
`internal/pgo/fixtures_test.go` `fakeDiscovery.Targets` takes the parameter
and appends it to a `selections` slice under its mutex.
Add one row to `internal/pgo/rounds_test.go`, half of the pgo spec's *Unit* case:
`runRounds` drives a Collection through two rounds
and the fake holds exactly two recorded selections, both the zero `PortSelection`,
so a round never names a port.
That harness calls only `Rounds.run`, which resolves targets once per round through `targetsFor`;
the advisory resolution runs in the HTTP collection handler (`pgo_collections.go`), which this harness never reaches.
The other half of the case, the advisory call, is proven in `internal/httpapi/pgo_collections_test.go` under the *HTTP API* task;
here the handler's `discover` passes the zero selection for every route, which covers it until that task parses the parameters.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/k8s/ ./internal/httpapi/ ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/k8s/ internal/httpapi/ internal/pgo/
git commit -m "feat(k8s): resolve the pprof port per request"
```

---

## HTTP API

**Files:**
- Modify: `internal/httpapi/server.go`, `profile.go`, `audit.go`, `fixtures_test.go`, `server_test.go`, `profile_test.go`, `pgo_collections_test.go`
- Create: `internal/httpapi/export_test.go`

**Produces:**

```go
package httpapi

// portParams is the client's port selection as parsed:
// the selection discovery receives and the value as the client sent it, for the audit line.
type portParams struct {
    sel  k8s.PortSelection
    sent string // "6061" or "pprof-alt"; empty when neither parameter is present
}

// parsePortParams reads port and portName out of values and removes them,
// so the caller's own loop sees only its own parameters.
// It applies the spec's grammar: each at most once with a value,
// port a decimal integer 1–65535, portName a container-port name, never both.
func parsePortParams(values url.Values) (portParams, *requestError)

// profileParams gains the parsed selection; seconds, pod, and version are unchanged.
type profileParams struct {
    seconds int
    pod     string
    version string
    port    portParams
}

// parseProfileParams takes the decoded query instead of the raw string:
// ServeHTTP decodes once for both endpoints and maps a decoding error to 400 invalid_parameter itself.
// The selection is parsed first, then the profile's own parameters, then the duration limit.
func parseProfileParams(values url.Values, spec profileSpec, limits config.LimitsConfig) (profileParams, *requestError)

// portNotAllowed builds the 400 port_not_allowed error; its message names only the value the client sent.
func portNotAllowed(sent string) *requestError
```

`profileParams` gains one field, `port portParams`, beside `seconds`, `pod`, and `version`;
there is one struct and one return value, and `parseProfileParams` keeps its `*requestError` result.
`request` gains `port portParams`, assigned from the parser's result on both endpoints,
and `auditRecord` gains `port string`, written between `seconds` and `status` on the interactive line.
`discover` passes `q.port.sel`; PGO routes never set it, so they keep the zero selection.
`server` gains an unexported `beforeAllowlist func()` field, nil in production,
which `ServeHTTP` calls, when set, after the realm check and before the allowlist check;
`export_test.go` holds `setBeforeAllowlist(h http.Handler, fn func())`, which asserts `h.(*server)` and sets it.
It is the seam the *Layers* snapshot case needs: a request paused between realm evaluation and discovery while the configuration pointer changes.

- [x] **Write the HTTP API tests**

`fixtures_test.go`: `fakeDiscovery` gains `byName map[string][]k8s.Target`;
when the selection names a port name and the map is set,
`Targets` answers `byName[name]` (nil when absent), otherwise `targets`.
It already records every selection from the previous task; add `selectionsSeen() []k8s.PortSelection`.
The harness gains a `beforeAllowlist func()` field that its handler builder installs through `setBeforeAllowlist`.
`profile_test.go`'s direct `parseProfileParams` table passes `url.Values` for the new signature,
decoding each row's query string with `url.ParseQuery` in the loop;
its `malformed escape` row (`pod=%zz`) moves out, since decoding now fails in `ServeHTTP`,
and the `malformed query` row in `server_test.go` keeps proving the endpoint's answer.
The table gains rows for a valid `port` and a valid `portName` beside `pod`, asserting `profileParams.port`.
The rows restate the `internal/httpapi` bullet of *Layers*,
and *Fetch a profile*, *List targets*, *Errors*, *Non-disclosure*, *Logging*, and *Metrics*.
Unless a row says otherwise the harness runs `testConfig()`, whose `Discovery.Pprof` is zero, so both lists are empty.

Grammar, per parameter and combined;
every row is `400 invalid_parameter` and `expectCounts(t, 0, 0)`.
The audit `port` field is selection-specific (*Logging*):
empty when the selection itself is absent, malformed, repeated, or doubled,
and the value as sent when the selection is valid and some other parameter fails.

| Subtest | Query | Endpoint | Audit `port` |
|---|---|---|---|
| port zero | `port=0` | profile `heap` and targets | empty |
| port over | `port=65536` | both | empty |
| port letters | `port=abc` | both | empty |
| port signed | `port=+80` | both | empty |
| port empty | `port=` | both | empty |
| port repeated | `port=6060&port=6061` | both | empty |
| portName empty | `portName=` | both | empty |
| portName repeated | `portName=a&portName=b` | both | empty |
| portName too long | 16 characters | both | empty |
| portName uppercase | `portName=Pprof` | both | empty |
| portName leading hyphen | `portName=-pprof` | both | empty |
| portName trailing hyphen | `portName=pprof-` | both | empty |
| portName all digits | `portName=6060` | both | empty |
| portName consecutive hyphens | `portName=pp--rof` (the container-port name rule) | both | empty |
| both given | `port=6060&portName=pprof` | both | empty |
| query malformed | `port=%zz`: `url.ParseQuery` fails, mapped to `invalid_parameter` as the profile endpoint maps it today | both | empty |
| valid port, unknown parameter | `port=6060&x=1` | both | `6060`: the selection is valid, only `x` is not |
| valid port, invalid seconds | `cpu?port=6060&seconds=abc` | profile `cpu` | `6060` |
| targets seconds | `seconds=2` | targets: the endpoint takes `port` or `portName` and no other | empty |

Allowlist, realm order, and audit; `configure` sets `Discovery.Pprof` per row:

| Subtest | Configuration and query | Expect |
|---|---|---|
| portName abc is valid | empty lists, `portName=abc` on targets | `200`; the fake saw `PortSelection{PortName: "abc"}` |
| empty lists accept any port | empty lists, `heap?port=9999` | reaches the upstream: `200`, fake saw `{Port: 9999}` |
| disallowed port | `AllowedPorts: [6060]`, `heap?port=6061` | `400 port_not_allowed`; body names `6061` and no other number; `expectCounts(t, 0, 0)`: the fake records no call |
| disallowed name | `AllowedPortNames: [pprof]`, `targets?portName=pprof-alt` | `400 port_not_allowed`; body names `pprof-alt`; no discovery call |
| lists are independent | `AllowedPorts: [6060]` with empty names, `targets?portName=anything`; and `AllowedPortNames: [pprof]` with empty ports, `heap?port=9999` | `200` each: one list never bounds the other parameter |
| default number always passes | `Port: 6060, AllowedPorts: [7070]`, `port=6060` | `200` |
| default name always passes | `PortName: pprof, AllowedPortNames: [metrics]`, `portName=pprof` | `200` |
| no selection under narrowed lists, targets | `Port: 6060, AllowedPorts: [7070], AllowedPortNames: [metrics]`, `targets` with no query | `200`; the fake saw the zero `PortSelection`: a zero selection skips allowlist evaluation and resolves under the configured default |
| no selection under narrowed lists, profile | same configuration, `heap` with no query | `200`, the upstream is reached |
| allowlist reads the request's snapshot | `AllowedPorts: [6060, 7070]`, `heap?port=7070`; `h.beforeAllowlist` stores a fresh `testConfig()` whose `AllowedPorts` is `[6060]` into `h.cfg` | `200`; the fake saw `{Port: 7070}`; the request finishes under the snapshot it loaded. Mutation check: an implementation that reloads `s.deps.Config` after the hook answers `400 port_not_allowed` and fails the row |
| realm before allowlist | realm admitting only namespace `other`, `AllowedPorts: [6060]`, `heap?port=6061` | `403 realm_denied`; no discovery call |
| grammar before allowlist | `AllowedPorts: [6060]`, `port=abc` | `400 invalid_parameter` |
| allowlist before discovery error | `AllowedPorts: [6060]`, `disc.err = ErrServiceNotFound`, `port=6061` | `400 port_not_allowed`, not `404` |
| parameters before allowlist | `AllowedPorts: [6060]`, `cpu?port=6061&seconds=61` | `400 seconds_exceeds_limit`: the parameter step finishes before the allowlist is asked |
| name on one Pod of two | `byName: {"pprof-alt": [pod-1]}`, `targets?portName=pprof-alt` | list holds `pod-1` only |
| name on no Pod | `byName: {}`, `targets?portName=nowhere` | `200` with `targets: []`; `heap?portName=nowhere` is `503 no_targets` |
| audit absent | `heap` | audit `port == ""` |
| audit numeric | `heap?port=6061` | audit `port == "6061"`, status `200`, code `ok` |
| audit name | `heap?portName=pprof-alt` with a target whose `Port` is 6061 | audit `port == "pprof-alt"`; the audit line contains no `6061` |
| audit malformed | `heap?port=abc`; `heap?port=1&port=2`; `heap?port=1&portName=a` | audit `port == ""` with `invalid_parameter` each |
| audit disallowed | `AllowedPorts: [6060]`, `heap?port=6061` | audit `port == "6061"` with `port_not_allowed` |
| no metric label | `heap?port=6061` | `expectMetric(t, metrics.EndpointProfile, "heap")`: the recorder's label set is unchanged |
| no number in the response | `portName=pprof-alt` with a target at `Port` 6061: targets body, profile response headers and the `X-Pprof-Target-*` trio, and every gateway error body | none contains `6061` |
| non-pprof listener passes through | `heap?port=8080` with a `fakeUpstream` outcome `{Code: "upstream_404", Status: 404, Committed: true}` | status `404`, audit code `upstream_404`; `internal/proxy` already maps an upstream 404 this way (`proxy_test.go`, the pass-through row) and needs no change |
| PGO advisory resolution | `pgo_collections_test.go`: `POST .../collections` through the PGO fixture, the other half of the pgo spec's *Unit* case | `selectionsSeen()` holds exactly one entry, the zero `PortSelection`: the advisory resolution never names a port |

- [x] **Run the tests and watch them fail**

- [x] **Implement**

`parsePortParams` in `profile.go`:
for each of `port` and `portName`,
`len(vs) != 1 || vs[0] == ""` is `invalidParameter("parameter %q must appear once with a value")`, the existing message;
`port` reuses the digit-only rule of `parseSeconds` with the bound 1–65535 (`parsePort`),
refusing sign, whitespace, and other bases;
`portName` runs `validation.IsValidPortName`;
both present is `invalidParameter("port and portName exclude each other")`;
the accepted keys are deleted from `values` so the callers' unknown-parameter rule stays as it is.
`parseProfileParams` takes `values url.Values` instead of the raw query,
calls `parsePortParams` first, stores the result in `profileParams.port`,
then runs its existing loop over what remains,
so a malformed `seconds` and a malformed `port` both report `invalid_parameter`
and the effective-duration check still runs last.
`ServeHTTP` runs the following sequence on both interactive endpoints,
after the realm check and before any discovery:

1. Decode the query once: `values, err := url.ParseQuery(r.URL.RawQuery)`;
   an error is `invalidParameter("the query string is malformed")`, the message the profile parser uses today,
   on the targets endpoint as on the profile endpoint.
2. Parse the parameters.
   Targets: `parsePortParams(values)`, then any key that remains is the existing `unknown parameter` message;
   the `takes no parameters` message goes away.
   Profile: `parseProfileParams(values, spec, cfg.Limits)`, which returns the selection inside `profileParams.port`.
   A failure here answers before anything below runs,
   so the audit `port` field stays empty for a selection that is malformed, repeated, or doubled.
3. Assign the selection: `q.port = params.port` (targets: the `portParams` returned directly),
   then `q.audit.port = q.port.sent`,
   right after the parser returns and before the allowlist, so a disallowed value is logged as sent.
   The assignment also runs on the parser's error path:
   `parseProfileParams` fills `profileParams.port` before its own loop and returns that partial struct beside a non-nil error,
   and the targets branch keeps the `portParams` it parsed before the unknown-key check,
   so a valid selection beside an invalid `seconds` or an unknown key is logged as sent,
   while a fault in the selection itself leaves `port` empty because `parsePortParams` returned nothing.
4. Call `s.beforeAllowlist()` when it is non-nil.
   Production leaves it nil.
5. Evaluate the allowlist on `cfg`, the pointer loaded at the top of `ServeHTTP`, never reloaded:
   a zero `q.port.sel` skips this step entirely, since the configured default is always permitted;
   otherwise `cfg.Discovery.Pprof.AllowsPort(sel.Port)` when `sel.Port` is set,
   or `AllowsPortName(sel.PortName)` when the name is set,
   and a `false` fails with `portNotAllowed(q.port.sent)`,
   whose message is `port %q is not allowed by this gateway`;
   it names the client's value and nothing about Pods (*Errors*).
6. Only then `serveTargets` or `serveProfile`, whose `discover` passes `q.port.sel`.

`writeAudit` adds `"port", rec.port` after `seconds` on the interactive record.
`metrics` is untouched.
`docs/api.md` is not touched here; the *Documentation* task writes it.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "feat(httpapi): accept port and portName"
```

---

## Deployment manifests and chart

**Files:**
- Modify: `deploy/base/configmap.yaml`, `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md`, `deploy/deploy_test.go`, `deploy/chart_test.go`

The chart's structured values render no `discovery` block today (`templates/_helpers.tpl`, `profgate.configStructured`);
`discovery.pprof.port` reaches the file through the raw `config` block, as the chart README's example shows.
The two lists ship the same way:
`values.yaml` sets `config.discovery.pprof.allowedPorts: []` and `allowedPortNames: []` and nothing else under `discovery`,
`profgate.config` merges the block over the structured keys as it does now, and no template changes.
Helm merges a user's `config` values over the chart's defaults key by key,
so `--set config.limits.cpuSeconds=30` keeps the two lists.
The default carries no `discovery.pprof.port`:
`config.Load` accepts exactly one of `port` and `portName` (`internal/config/config.go`, `validate`),
so a `port: 6060` default would combine with a user's `portName` into a file the binary refuses,
while an absent `port` leaves `normalize` to fill 6060 when neither is set.
Helm keeps an empty-list default when the user's values leave the key alone
(`helm template` with only `config.discovery.pprof.portName` set renders `allowedPorts: []` and `allowedPortNames: []` beside it),
so the rendered YAML carries both keys and the tests may assert the text as well as the loaded `config.Config`.
`NOTES.txt` names the API port and the ops port only and needs no change.

- [x] **Write the deployment tests**

| Subtest | File | Expect |
|---|---|---|
| base ConfigMap lists | `deploy_test.go` `TestConfigMap` | the `config.yaml` text contains `allowedPorts: []` and `allowedPortNames: []`; `config.Load` of it returns both slices empty |
| base example still loads | `TestDeploymentGracePeriod`, `TestDeploymentMemoryLimit` | unchanged and still green |
| chart renders the lists | `chart_test.go`, a new `TestChartPortAllowlists` | the rendered ConfigMap text contains `allowedPorts: []` and `allowedPortNames: []`; `loadRenderedConfig(t)` returns both empty |
| chart raw block still merges | `TestChartConfigIsMergedAndParses`, `raw config block` | `--set config.discovery.pprof.port=7070` still yields `Port == 7070` and both lists still empty |
| chart portName alone loads | `TestChartPortAllowlists`, `portName only` | `loadRenderedConfig(t, "--set", "config.discovery.pprof.portName=pprof")` returns `Port == 0`, `PortName == "pprof"`, and both lists empty; the assertion is on the loaded `config.Config`, since `config.Load` is what refuses `port` beside `portName` |
| chart narrows the lists | `--set-json config.discovery.pprof.allowedPorts=[6060]`, `--set-json config.discovery.pprof.allowedPortNames=["pprof"]` | `loadRenderedConfig` returns `[6060]` and `[pprof]` |
| chart lint | `TestChartLint` | still green |

- [x] **Run the tests and watch them fail**

- [x] **Implement**

`deploy/base/configmap.yaml`: under `pprof:` add
`allowedPorts: []` and `allowedPortNames: []`,
each with a one-line comment saying an empty list accepts any value of its request parameter
and that the configured default always passes.
`values.yaml`: replace `config: {}` with `config.discovery.pprof.allowedPorts: []` and `allowedPortNames: []` only, no `port`,
and extend the comment that precedes it with the same two sentences,
plus one saying `port` is left out so a user's `portName` does not meet it.
`deploy/chart/profgate/README.md`:
the raw-block example under *How the configuration file is assembled* shows the two lists beside `port: 6060`,
and the values table row for `config` replaces its `{}` default with the new one,
`discovery.pprof` holding both lists empty and no `port`, so the table matches `values.yaml`.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./deploy/
mise run lint && mise run test && mise run check
git add deploy/
git commit -m "feat(deploy): ship empty pprof port allowlists"
```

---

## Test application and end-to-end scenarios

**Files:**
- Modify: `test/e2e/testapp/main.go`, `test/e2e/testapp/deployment.yaml`, `test/e2e/scenarios_test.go`, `test/e2e/registry.go`, `test/e2e/lanes_test.go`, `test/e2e/harness_test.go`
- Create: `test/e2e/overlays/ports-gateway/kustomization.yaml`, `serviceaccount.yaml`, `clusterrolebinding.yaml`, `configmap.yaml`, `deployment.yaml`

**Produces:**

The test app serves one handler on two `http.Server`s, `:6060` and `:6061`.
`GET /hits` answers `{"pprof": 3, "hits": {":6060": 2, ":6061": 1}}`:
`pprof` is the total the existing scenarios read, `hits` is per listen address.
The `hits` helper in `scenarios_test.go` decodes into a struct with those two fields and keeps returning the total;
`listenerHits(t, app, ":6061")` returns one listener's count.
`test/e2e/testapp/deployment.yaml`, which `deployTestApp` applies into every scenario namespace,
declares a second container port after `pprof`: `name: pprof-alt`, `containerPort: 6061`, `protocol: TCP`;
the readiness probe stays on `pprof` and the Service is unchanged,
since the gateway resolves the name from the Pod, not the Service.
Without this port, `portName=pprof-alt` lists no targets.
`deployErrorsGateway` becomes `deployScopedGateway(t, h, ns, overlay, name string)`,
parameterized on the overlay directory and the resource name every object shares,
so the `errors` scenario calls it with `"errors-gateway", "profgate-errors"`
and the new one with `"ports-gateway", "profgate-ports"`.

- [x] **Register the scenarios and pin them**

`registry.go` appends `{Name: "port selection", NeedsPodReach: true}`
and `{Name: "port selection refused", NeedsPodReach: true}` after `"api outage"`,
in the order *What end-to-end proves* lists them.
`lanes_test.go` `TestScenariosRegistry` adds the two names to the Pod-reach check,
since both read the test app's counter through a port-forward and the first completes a proxy through the gateway.
`TestScenarioSkips` holds an exact expected list for the degraded lane;
append `"port selection"` and `"port selection refused"` to it, after `"api outage"`,
in the position the registry gives them, or the test fails on the first run.
`runners()` in `harness_test.go` maps the two names to `scenarioPortSelection` and `scenarioPortSelectionRefused`.

- [x] **Write the test app change**

`main.go`: `listenAddrs = [...]string{":6060", ":6061"}`;
`app` gains `hits sync.Map` or a mutex-guarded `map[string]*atomic.Int64` keyed by listen address;
`counted` reads the listener from `r.Context().Value(http.LocalAddrContextKey)`,
keeps its port as `":" + port`,
and increments both the total and that key;
`serve` starts one `http.Server` per address with the shared handler,
fails on the first listen error,
and drains both on stop.
`hits` writes the two-field JSON above.
The image is built by ko from this directory as before.

- [x] **Write the overlay**

`overlays/ports-gateway/` is `overlays/errors-gateway/` with every `profgate-errors` renamed `profgate-ports`,
the same `namespaces: ["placeholder"]` line the scenario patches,
and `discovery.pprof` reading `port: 6060`, `allowedPorts: [6060]`, `allowedPortNames: [pprof]`.
Its `kustomization.yaml` comment says why it exists:
configuration is loaded once per process,
and one gateway cannot show both the accepted and the refused outcome (*Harness*).

- [x] **Write the scenarios**

The default gateway's configuration comes from `gatewayConfig` in `harness_test.go`,
which names neither list,
so both are empty and the test app's second port and name are accepted.

| Scenario | Steps | Expect |
|---|---|---|
| port selection | `deployTestApp`; `app := h.ForwardTestApp(t, ns, pods[0].Name)`; read `listenerHits(app, ":6061")`; `GET profiles/heap?port=6061&pod=<pod>` on `h.Gateways[0]`; then `GET profiles/heap?portName=pprof-alt&pod=<pod>` | each is `200` and `profile.ParseData` parses the body; the `:6061` count rises by one after each; the `X-Pprof-Target-*` headers of both responses contain no `6061`; `GET targets?portName=pprof-alt` lists every test-app Pod |
| port selection refused | `deployTestApp`; `c := deployScopedGateway(t, h, ns, "ports-gateway", "profgate-ports")`; forward the test app; read the `:6061` count; `GET profiles/heap?port=6061` and `GET profiles/heap?portName=pprof-alt` on `c` | each is `400 port_not_allowed` and the body names the value sent; the `:6061` count has not moved; `GET profiles/heap?port=6060` on `c` is `200`, the configured default passing whatever the lists hold |

- [x] **Run the suite**

```bash
mise run test:e2e
```

- [x] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add test/e2e/
git commit -m "test(e2e): prove client-selected pprof ports"
```

---

## Documentation

**Files:**
- Modify: `README.md`, `docs/api.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/README.md`, `CHANGELOG.md`, `deploy/chart/profgate/README.md`

The spec's *Amendments* section lists `docs/api.md`, `docs/configuration.md`, and the two manifests
as the documents updated with the implementation;
the manifests landed in the *Deployment manifests and chart* task.
`.agents/rules/100-project-map.md` lists packages and routes, and this feature adds neither, so it is unchanged.
`README.md` states that application Pods must serve pprof on the configured port or port name;
with client selection a request may name any other port the allowlists permit, so that sentence changes.

- [x] **Update the guides**

| File | Change |
|---|---|
| `README.md` | the sentence under *The one requirement on the application* becomes: the Pods must serve Go's `net/http/pprof` handlers on the configured default, `discovery.pprof.port` (6060 by default) or `discovery.pprof.portName`, and on any other port a client selects with `port` or `portName` that `discovery.pprof.allowedPorts` and `allowedPortNames` permit |
| `docs/api.md` | the *Parameters* step of *How a request is processed* names `port`/`portName` grammar and the allowlist (`400 port_not_allowed`), ahead of discovery; *Listing targets* replaces "takes no query parameters" with the two parameters it now takes; *Query parameters* gains the `port` and `portName` rows in the spec's *Fetch a profile* wording, with the never-both rule; the *Errors* table gains `400 port_not_allowed`; a short *What choosing ports reveals* subsection restates the three observations of *Non-disclosure* |
| `docs/configuration.md` | the `discovery` table gains the `pprof.allowedPorts` and `pprof.allowedPortNames` rows from the spec's *Configuration* table (environment variable, empty default, comma-separated); the paragraph ending "the only port the gateway connects to" is replaced by the allowlist rule: independent lists, an empty list permits any value, the default always passes, and "no `portName`" is expressed by listing only the default name under a named default, or one well-formed name no Pod declares under a numeric default; *Cross-Key Validation* gains the range, name, and duplicate rules; the *Examples* file shows both lists empty |
| `docs/deployment.md` | the pprof-port bullet under the configuration section names the two lists and that the shipped manifests leave them empty; the NetworkPolicy sentence notes that with empty lists NetworkPolicy is the only bound on which Pod ports the gateway reaches (*Network*); the *Audit log* field list for an interactive request gains `port` between `seconds` and `status`, with one sentence from *Logging*: it is the selection as sent, a number or a name, empty when absent, and for a name it is never the number the name resolved to |
| `deploy/chart/profgate/README.md` | the `config` row of the values table, whose default the chart task already set, gains the sentence that the two lists merge key by key with a user's `config` values and that an empty list accepts any value of its parameter |
| `docs/README.md` | the `plans/` sentence counts this plan among the plans it names |
| `CHANGELOG.md` | under `## [Unreleased]` (create it above `0.3.0` if the authentication work has not), *Added*: the two parameters, the two allowlists, `400 port_not_allowed`, the audit `port` field, the test app's second listener; *Changed*: the targets endpoint accepts `port` and `portName`, and the permission invariant's wording |

- [x] **Validate and commit**

```bash
semlf check README.md docs/api.md docs/configuration.md docs/deployment.md docs/README.md CHANGELOG.md deploy/chart/profgate/README.md
mise run lint && mise run test && mise run check
git add README.md docs/ CHANGELOG.md deploy/chart/profgate/README.md
git commit -m "docs: describe client-selected pprof ports"
```

---

## Finish the plan

- [x] Confirm the `main` run passed every lane (the existing workflows need no change:
  `check.yml` covers the new unit tests and `e2e.yml` the lanes).
- [x] In the same change: set line 3 of this file to `**Status:** Done` and add line 4
  `**Outcome:** <tag or commit that shipped client-selected pprof ports>`.
- [x] `mise run lint && mise run test && mise run check`;
  `git add docs/plans/port-selection.md`; `git commit -m "docs: mark the port selection plan done"`.

---

## Self-Review

- Spec coverage, by the headings the *Amendments* table lists:
  *Core decisions*, *Permission Boundary*, *Network*, *What a compromised gateway can do*: prose already amended, restated in *Global Constraints* and the deployment guide;
  *The seam*, *Eligibility*, *Port resolution*: *Discovery seam* and *Configuration*;
  *Request algorithm*, *List targets*, *Fetch a profile*, *Errors*, *Limits are not authorization*, *Non-disclosure*, *Logging*, *Metrics*: *HTTP API*;
  *Proxy behavior*: the budget sentence was a wording change and needs no code;
  *Layers*: the `internal/config`, `internal/k8s`, and `internal/httpapi` bullets are the three test tables,
  and the proxy row is the `non-pprof listener passes through` subtest;
  *Cluster matrix*, *Harness*, *What end-to-end proves*: *Test application and end-to-end scenarios*;
  *Configuration*, *Build and Deployment*: *Configuration* and *Deployment manifests and chart*;
  *Failure Scenarios*: each row maps to a unit row above
  (`portName` inference: `name on one Pod of two`;
  numeric probing: `non-pprof listener passes through`;
  allowlist disclosure: `disallowed port`;
  realm first: `realm before allowlist`;
  a name some or all Pods lack: `name on no Pod`);
  pgo spec *Core decisions*, *On-demand Collections*, *Rounds*, *Unit*:
  one zero selection per round in *Discovery seam* and the advisory call in *HTTP API*.
- Types: `k8s.PortSelection`, `config.PprofConfig` (extended), `config.PprofConfig.AllowsPort`, `AllowsPortName`,
  and the unexported `portParams`, `parsePortParams`, `parsePort`, `portNotAllowed`, `namedPort`, `beforeAllowlist`, `setBeforeAllowlist`
  are each defined once, in the task that first needs them, and consumed by those names afterwards.
- Task order compiles at every step:
  config → discovery seam (interface, every caller, every fake) → httpapi → deploy → e2e → docs;
  the seam task updates `internal/httpapi` and `internal/pgo` callers and fakes in the same commit as the interface,
  the httpapi task changes only `internal/httpapi`,
  and `deploy/` tests import `internal/config`, which is finished first.
- Current source against the spec:
  `docs/configuration.md` states "There is no port allowlist beyond this single setting";
  the *Documentation* task replaces it.
  `docs/api.md` states the targets endpoint takes no query parameters; same task.
  The chart renders no structured `discovery` block,
  so the lists ship through the raw `config` values rather than a template change.
  The end-to-end `hits` helper decodes `/hits` into `map[string]int64`,
  which the nested `hits` object would break;
  the end-to-end task changes the helper with the shape.
- Decided here because the spec leaves them to the implementer:
  the allowlist rule lives in `internal/config` as two methods, so the HTTP layer holds no port policy of its own;
  `parsePortParams` removes its two keys from the query values so each endpoint's existing unknown-parameter rule stays intact,
  and the port parameters are parsed before the endpoint's own,
  so a malformed `seconds` and a malformed `port` both report `invalid_parameter`
  and `seconds_exceeds_limit` still comes before `port_not_allowed`;
  the `port_not_allowed` message is `port "<value>" is not allowed by this gateway`;
  a zero selection skips allowlist evaluation rather than asking `AllowsPort(0)`,
  which a numeric default beside a non-empty list would refuse;
  the snapshot seam is an unexported hook on `server` rather than a fake `Discovery`,
  because the existing `onTargets` hook fires inside discovery, after the allowlist has already read its configuration;
  the test app attributes a hit to a listener through `http.LocalAddrContextKey`;
  the `hits` map is keyed `":6060"` and `":6061"` exactly as the spec shows;
  the `ports-gateway` overlay is a renamed copy of `errors-gateway`, deployed by the generalized `deployScopedGateway`;
  the refused-port scenario adds `?port=6060` answering `200` as its positive control,
  which *Port resolution* ("the configured default is always permitted") supports
  though the scenario text names only the two refusals;
  the chart ships the lists as raw `config` defaults, matching how `discovery.pprof.port` already reaches the file.
- Where the spec is silent, left as stated:
  the consecutive-hyphen refusal is the container-port name rule the spec cites, not a grammar row it lists;
  an environment variable set to the empty string (`PROFGATE_PPROF_ALLOWED_PORTS=`) is not a spec case,
  and `fuda`'s CSV reader may refuse it rather than read an empty list —
  the implementer adds the row with whatever `config.Load` does and reports it,
  since the spec names no outcome;
  the spec does not say whether `allowedPorts` may list the configured default number redundantly;
  the plan allows it, because the default passes anyway.
- Left to the implementer by design: helper names inside test files,
  the exact second-Pod fixture helper in `internal/k8s`,
  and whether `deployScopedGateway` keeps a thin `deployErrorsGateway` wrapper or the `errors` scenario calls it directly.
