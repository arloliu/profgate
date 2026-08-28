# Client-Selected Port Becomes Default-Deny

**Status:** Draft

> **For the implementer:** implement this plan one task at a time, in order;
> each task is written test-first and ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> The accepted spec outranks this plan:
> where the two differ the spec wins and the plan is the bug.

**Goal:** Replace the two fail-open allowlists,
`discovery.pprof.allowedPorts` and `discovery.pprof.allowedPortNames`,
with the single default-deny list `discovery.pprof.allowedSelections`
that [`docs/specs/gateway.md`](../specs/gateway.md) *Port resolution* and *Configuration* define:
entries are `{port: N}` or `{portName: name}`,
an empty list admits only the configured default,
`{port: "*"}` and `{portName: "*"}` each open their own kind,
and the two old keys and their two environment variables are removed and refused by validation.
`/v1/limits` reports the list so the console can offer a menu or a free field,
and the end-to-end suite proves both outcomes against a real cluster.

**Architecture:** No new package, no new seam, and no change to the Kubernetes interface.
`internal/config` gains the `Selection` value type, the `allowedSelections` field,
and the two predicates the HTTP layer already calls;
the admission point in `internal/httpapi` keeps its shape and changes only what the predicates answer;
`internal/httpapi/listing.go` renders the list into `/v1/limits`;
`internal/ui` gains `static/portmodel.js`, two pure functions that turn `/v1/limits` into the port control
and the control's state into the query parameter it sends;
`deploy/` and `test/e2e/` ship the new key;
`internal/k8s` is untouched — `PortSelection`, `Targets`, and eligibility already carry a client's choice
([`docs/specs/gateway.md`](../specs/gateway.md) *The seam*, *Eligibility*).

**Tech Stack:** everything already pinned in [`mise.toml`](../../mise.toml).
One test-only Go module is added:
`github.com/dop251/goja`, the pure-Go ECMAScript interpreter
[`docs/specs/gateway.md`](../specs/gateway.md) *Dependencies* names for evaluating the console's port-control model.
No runtime module is added and no vendored browser file changes.

**Spec:** [`docs/specs/gateway.md`](../specs/gateway.md), `Accepted`, is the design of record;
[`docs/specs/ui.md`](../specs/ui.md), `Accepted`, defines the `/v1/limits` shape and the console's port control.
Both are cited by heading name below, never by number;
an unqualified heading is the gateway spec's.
The gateway spec's *Amendments* block lists this change's edits as already made,
and names the documents updated when the implementation lands;
the last task is where that second list is honored.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- **The permission invariant text needs no edit.**
  `AGENTS.md`, `README.md`, [`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md),
  and the spec's *Permission Boundary* already carry the wording that names `discovery.pprof.allowedSelections`.
  Confirm they still match before the last commit; do not reword them.
- **No RBAC change.**
  No Kubernetes verb, resource, or API group moves.
  `TestClusterRoleTuples` in `deploy/deploy_test.go`
  and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go` stay green and untouched by every task.
- **No new runtime module.**
  `github.com/dop251/goja` is imported only from `_test.go` files in `internal/ui`;
  the binary's module set does not change.
  No import check is added to `scripts/check-repo.py`:
  the greps there guard packages that can widen the gateway's runtime capability,
  and an interpreter used by a test cannot.
- **Only `internal/k8s` imports `k8s.io/client-go`**, unchanged;
  `mise run check` enforces it.
- **The request algorithm does not move.**
  The parameter step validates the selection's grammar and then checks it against the list, before discovery;
  a realm denial still precedes both,
  and a refused value reaches no `Discovery` call (*Request algorithm*, *Port resolution*).
- **Non-disclosure holds.**
  A `400 port_not_allowed` body names only the value the client sent;
  no response, header, or audit line carries the number a `portName` resolved to (*Non-disclosure*, *Errors*).
- **The audit record and the metric label sets do not change.**
  `port` keeps the meaning *Logging* gives it — the selection as sent, empty when absent or malformed,
  the client's value beside `port_not_allowed` — and the port selection becomes no metric label (*Metrics*).
- **This is a breaking change.**
  A configuration that sets either removed key, or either removed environment variable,
  fails validation with a message naming `allowedSelections` and `PROFGATE_PPROF_ALLOWED_SELECTIONS` (*Configuration*).
  It ships in the next minor release; the `CHANGELOG.md` entry says so.
- **Structured error details are out of scope.**
  *Errors* gives `port_not_allowed` a `details` item whose `code` is `not_admitted`,
  but `details` belongs to the machine-contract work [`docs/plans/roadmap.md`](roadmap.md) orders after this one
  and no `details` field exists in `internal/httpapi/errors.go` today.
  This plan changes no error envelope.
- No jargon: code comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a review round, or a task name.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on every Markdown file a task writes or edits.
- Commit headers are Conventional Commits under 50 characters, with no trailer of any kind.

---

## File Structure

```text
internal/config/config.go                       # Selection, SelectionKind, AllowedSelections, ParseSelection, AllowsPort, AllowsPortName
internal/config/config_test.go                  # the load, validation, and environment tables; TestPprofAllows rewritten
internal/config/testdata/selections-*.yaml      # the new fixtures; the allowed-ports-*.yaml files are deleted
internal/httpapi/listing.go                     # pprofView carries allowedSelections
internal/httpapi/listing_test.go                # the /v1/limits shape
internal/httpapi/server.go                      # allowPort, unchanged in shape
internal/httpapi/server_test.go                 # the refusal table under the new model
internal/ui/static/portmodel.js                 # the port control's two pure functions
internal/ui/static/app.js                       # renders from portmodel.js instead of the two lists
internal/ui/portmodel_test.go                   # the goja table over both functions
internal/ui/scan_test.go, ui_test.go, vendor_test.go   # portmodel.js joins the scanned tree
go.mod, go.sum                                  # github.com/dop251/goja, test-only
deploy/base/configmap.yaml                      # allowedSelections: []
deploy/chart/profgate/values.yaml               # allowedSelections: []
deploy/chart/profgate/README.md                 # the shipped value and its row
deploy/deploy_test.go, deploy/chart_test.go     # the shipped-empty assertions
test/e2e/harness_test.go                        # the default gateway's two entries
test/e2e/overlays/ports-gateway/configmap.yaml  # allowedSelections: []
test/e2e/scenarios_test.go                      # the two port-selection scenarios
test/e2e/scenarios_auth_test.go                 # the /v1/limits assertion
docs/api.md, configuration.md, deployment.md, console.md
.agents/rules/500-validation-and-workflow.md
CHANGELOG.md
docs/plans/roadmap.md
docs/plans/port-selection.md
```

---

## One list replaces two allowlists

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Create: `internal/config/testdata/selections-*.yaml`
- Delete: `internal/config/testdata/allowed-ports.yaml`, `allowed-ports-dup.yaml`,
  `allowed-ports-dup-name.yaml`, `allowed-ports-name.yaml`, `allowed-ports-range.yaml`,
  `allowed-ports-unknown.yaml`
- Modify: `internal/httpapi/listing.go`, `internal/httpapi/listing_test.go`, `internal/httpapi/server_test.go`
- Modify: `internal/ui/static/app.js`
- Modify: `deploy/base/configmap.yaml`, `deploy/chart/profgate/values.yaml`,
  `deploy/chart/profgate/README.md`, `deploy/deploy_test.go`, `deploy/chart_test.go`
- Modify: `test/e2e/overlays/ports-gateway/configmap.yaml`, `test/e2e/scenarios_auth_test.go`

**Why this commit is large.**
`AllowedPorts` and `AllowedPortNames` are read by non-test code (`internal/httpapi/listing.go`).
`deploy/deploy_test.go` and `deploy/chart_test.go` load the shipped and the rendered ConfigMap through `config.Load`,
so validation refusing the old keys turns those tests red until the manifests carry the new key.
Every task ends with a green `mise run lint && mise run test && mise run check`,
so the field rename and every place that names it land together.
Two of those places fail silently rather than loudly and must not be forgotten:
`test/e2e/overlays/ports-gateway/configmap.yaml`, whose gateway would simply refuse to start,
and `internal/ui/static/app.js`, which no test executes today
and would quietly render an empty menu.
The richer behavior — the refusal table, the `/v1/limits` shape, the console model, the cluster proofs —
lands in the tasks after this one, on a tree that already compiles.

**Produces:**

```go
package config

// SelectionKind is the request parameter one allowedSelections entry bounds.
type SelectionKind string

const (
    SelectionPort     SelectionKind = "port"
    SelectionPortName SelectionKind = "portName"
)

// AnySelection is the wildcard an entry carries in place of a concrete value.
const AnySelection = "*"

// Selection is one entry of discovery.pprof.allowedSelections:
// the parameter it bounds and the value it admits, which is a decimal port
// number, a container-port name, or AnySelection.
type Selection struct {
    Kind  SelectionKind
    Value string
}

// UnmarshalYAML reads one entry: a mapping carrying exactly one of port and
// portName, whose scalar is a number, a name, or "*".
func (s *Selection) UnmarshalYAML(node *yaml.Node) error

// MarshalJSON writes the one-key object /v1/limits reports:
// {"port":6061}, {"port":"*"}, {"portName":"pprof-alt"}, {"portName":"*"}.
func (s Selection) MarshalJSON() ([]byte, error)

// String is the environment token form: "port:6061", "portName:*".
func (s Selection) String() string

// ParseSelection reads one environment token.
func ParseSelection(token string) (Selection, error)

// PprofConfig loses AllowedPorts and AllowedPortNames and gains:
    AllowedSelections []Selection `yaml:"allowedSelections"`

// AllowsPort reports whether a request may name port n: the configured
// default always, an entry holding that number, or the port wildcard.
func (p PprofConfig) AllowsPort(n int32) bool

// AllowsPortName reports whether a request may name that container port:
// the configured default always, an entry holding that name, or the name
// wildcard.
func (p PprofConfig) AllowsPortName(name string) bool
```

`Selection` carries no `env` tag,
because fuda applies an `env` tag whenever the variable is *set*, empty value included,
and its string-to-slice conversion parses the value as one CSV record,
which cannot express the empty list *Configuration* requires from an empty variable.
`Load` therefore reads `PROFGATE_PPROF_ALLOWED_SELECTIONS` itself.
Record that reason in a comment beside the field so nobody restores the tag.

`Load` runs these steps in this order:

1. Refuse `PROFGATE_PPROF_ALLOWED_PORTS` and `PROFGATE_PPROF_ALLOWED_PORT_NAMES` by name with `os.LookupEnv`.
   No decoder can reach them: a variable no field claims is invisible to fuda (*Configuration*).
2. Refuse a file that still sets `discovery.pprof.allowedPorts` or `discovery.pprof.allowedPortNames`,
   before the strict unknown-key pass,
   which would otherwise report only that the key is unknown and leave the operator to guess the replacement.
   Decode the bytes into a small probe holding only those two keys as `*yaml.Node`, without `KnownFields`,
   so only that nesting level is examined.
3. The existing strict `KnownFields(true)` decode.
4. The existing fuda load.
5. Apply `PROFGATE_PPROF_ALLOWED_SELECTIONS` when it is set:
   the empty string replaces the file's list with an empty one;
   any other value is split on commas into tokens read by `ParseSelection`, in order,
   and an empty token is an error rather than a dropped entry.
6. `normalize`, unchanged.
7. `validate`, which gains the list rules.

Validation splits three ways, and each half has one home:
the entry's shape and its scalar grammar in `UnmarshalYAML`;
the token's shape in `ParseSelection`;
and the list rules — no duplicate entry, no wildcard beside a concrete entry of its own kind — in `validate`,
because those must judge the list the environment may have replaced.

A custom `UnmarshalYAML` bypasses `KnownFields(true)` inside the entry,
so the unmarshaler itself refuses an entry carrying both keys, neither, or a third key.

- [ ] **Write the configuration tests**

`config_test.go` gains three tables and rewrites `TestPprofAllows`,
whose present rows assert the fail-open reading that this change inverts.
The rows restate *Port resolution* and *Configuration* and the `internal/config` rows of *Layers*.

Admission, one row per case, over `AllowsPort` and `AllowsPortName`:

| Configured default | `allowedSelections` | Request names | Admitted |
|---|---|---|---|
| `port: 6060` | empty | `port=6060` | yes: the configured default is always permitted |
| `port: 6060` | empty | `port=6061` | no |
| `port: 6060` | empty | `portName=pprof` | no: an empty list refuses the other kind outright |
| `portName: pprof` | empty | `portName=pprof` | yes |
| `portName: pprof` | empty | `port=6060` | no: a named default is no numeric default |
| `port: 6060` | `{port: 6061}` | `port=6061` | yes |
| `port: 6060` | `{port: 6061}` | `port=6062` | no |
| `port: 6060` | `{port: 6061}` | `portName=pprof-alt` | no |
| `port: 6060` | `{port: 6060}` | `port=6060` | yes: listing the default changes nothing |
| `port: 6060` | `{port: "*"}` | `port=65535` | yes |
| `port: 6060` | `{port: "*"}` | `portName=pprof-alt` | no: each wildcard covers its own kind |
| `port: 6060` | `{portName: "*"}` | `portName=anything` | yes |
| `port: 6060` | `{portName: "*"}` | `port=6061` | no |
| `portName: pprof` | `{port: 6061}` | `portName=pprof` | yes |
| `portName: pprof` | `{port: 6061}` | `port=6061` | yes |
| `portName: pprof` | `{port: 6061}` | `port=6060` | no |
| `portName: pprof` | `{port: 6061}` | `portName=metrics` | no |
| `port: 6060` | `{port: "*"}` and `{portName: "*"}` | any number, any name | yes to both |

Loading and validation, one fixture per row under `testdata/selections-*.yaml`:

| Fixture | Expect |
|---|---|
| no `allowedSelections` key | loads; the list is empty |
| `- port: 6061` | loads; one numeric entry |
| `- port: "*"` | loads; the port wildcard |
| `- portName: pprof-alt` | loads; one named entry |
| `- portName: "*"` | loads; the name wildcard |
| `- {port: 6061, portName: pprof}` | error: an entry names exactly one of `port` and `portName` |
| `- {}` | the same error |
| `- {port: 6061, extra: x}` | the same error: the entry refuses a third key itself |
| `- port: 0` and `- port: 65536` | error naming the 1–65535 range |
| `- port: abc` | error naming the range |
| `- portName: Pprof`, `-x`, sixteen characters, all digits | error naming the container-port name rule |
| `- port: 6061` twice | error naming the duplicate entry |
| `- portName: pprof-alt` twice | error naming the duplicate entry |
| `- port: "*"` beside `- port: 6061` | error: a wildcard beside a concrete entry of its own kind |
| `- portName: "*"` beside `- portName: pprof-alt` | error |
| `- port: "*"` beside `- portName: pprof-alt` | loads: the kinds do not interact |
| a file setting `allowedPorts` | error naming `allowedSelections` and `PROFGATE_PPROF_ALLOWED_SELECTIONS` |
| a file setting `allowedPortNames` | the same message |

The environment, over the good fixture unless a row says otherwise:

| Variable | File list | Expect |
|---|---|---|
| absent | `- port: "*"` | the file's list stands, wildcard included |
| empty string | `- port: "*"` | the list becomes empty; only the configured default is accepted |
| `port:6061` | any | one numeric entry |
| `port:6061,portName:pprof-alt` | any | both entries, in that order |
| `port:*` | any | the port wildcard |
| `portName:*` | any | the name wildcard |
| `,port:6061` | any | error: an empty token |
| `port:6061,` | any | error: an empty token |
| `port:6061,,portName:x` | any | error: an empty token |
| `6061` | any | error naming the token grammar |
| `ports:6061` | any | error naming the token grammar |
| `port:70000` | any | error naming the 1–65535 range |
| `port:6061,port:6061` | any | error naming the duplicate entry |
| `port:*,port:6061` | any | error: a wildcard beside a concrete entry of its own kind |
| `PROFGATE_PPROF_ALLOWED_PORTS` set | any | error naming `allowedSelections` and `PROFGATE_PPROF_ALLOWED_SELECTIONS` |
| `PROFGATE_PPROF_ALLOWED_PORT_NAMES` set | any | the same message |

The four refusal messages are asserted on their text,
so an operator carrying an older deployment forward reads what to write (*Configuration*).

- [ ] **Run the tests and watch them fail**

- [ ] **Implement the configuration**

Add the type, the field, the four methods, the `Load` steps, and the `validate` rules as written above.
Delete `AllowedPorts`, `AllowedPortNames`, and the `firstDuplicate` calls they fed
if nothing else uses that helper.
Error text follows the existing style: the key path, then the rule.

- [ ] **Carry the rename through the tree**

| File | Change |
|---|---|
| `internal/httpapi/listing.go` | `pprofView` carries `AllowedSelections []config.Selection` under the JSON name `allowedSelections`, built so an empty list encodes `[]` and never `null` |
| `internal/httpapi/listing_test.go`, `server_test.go` | every `config.PprofConfig` literal builds `AllowedSelections` instead of the two lists; behavior rows stay as they are and grow in the next tasks |
| `internal/ui/static/app.js` | the port control reads `pprof.allowedSelections`, offering one option per entry and a free field only for a wildcard; the model moves to its own module in a later task |
| `deploy/base/configmap.yaml`, `deploy/chart/profgate/values.yaml` | `allowedSelections: []`, with the comment saying an empty list accepts only the configured default |
| `deploy/chart/profgate/README.md` | the example block and the `config` row name `allowedSelections` and say the empty list is default-deny |
| `deploy/deploy_test.go`, `deploy/chart_test.go` | the shipped-empty assertions read `allowedSelections: []` and `AllowedSelections` |
| `test/e2e/overlays/ports-gateway/configmap.yaml` | `allowedSelections: []`, which is what its scenario proves; the realm's `namespaces: ["placeholder"]` line stays exactly as written, because `deployScopedGateway` patches that literal and fails the scenario when it finds nothing to replace |
| `test/e2e/scenarios_auth_test.go` | the `/v1/limits` struct reads `allowedSelections`; the assertion is adjusted again when the default gateway's list grows |

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/... ./deploy/
mise exec -- go vet -tags e2e ./test/e2e/...
mise run lint && mise run test && mise run check
semlf check deploy/chart/profgate/README.md
git add internal/ deploy/ test/e2e/
git commit -m "feat(config)!: one allowedSelections list"
```

---

## What the gateway refuses

**Files:**
- Modify: `internal/httpapi/server_test.go`
- Modify: `internal/httpapi/server.go` (only if the table finds a gap)

`allowPort` in `internal/httpapi/server.go` already asks the configuration snapshot the two questions
and answers `400 port_not_allowed` with the value as sent,
so this task is where the request-level outcomes of the new model are proven.
Change code only where a row fails.

- [ ] **Write the refusal table**

Rows restate *Port resolution*, *Request algorithm*, *Errors*, *Logging*, and the `internal/httpapi` rows of *Layers*,
each run against the targets endpoint and the profile endpoint.

| Case | Configuration | Request | Expect |
|---|---|---|---|
| empty list, the default by number | `port: 6060`, empty | `port=6060` | `200`; the fake `Discovery` sees the numeric selection |
| empty list, a number beyond it | `port: 6060`, empty | `port=6061` | `400 port_not_allowed`; the fake records no call |
| empty list, the other kind | `port: 6060`, empty | `portName=pprof` | `400 port_not_allowed`; no call |
| empty list, the default by name | `portName: pprof`, empty | `portName=pprof` | `200` |
| empty list, a number under a named default | `portName: pprof`, empty | `port=6060` | `400 port_not_allowed`; no call |
| a listed entry | `port: 6060`, `{port: 6061}` | `port=6061` | `200` |
| an unlisted value of the listed kind | `port: 6060`, `{port: 6061}` | `port=6062` | `400 port_not_allowed`; no call |
| an unlisted kind | `port: 6060`, `{port: 6061}` | `portName=pprof-alt` | `400 port_not_allowed`; no call |
| the default is listed too | `port: 6060`, `{port: 6060}` | `port=6060` | `200` |
| the port wildcard | `port: 6060`, `{port: "*"}` | `port=65535` | `200` |
| the port wildcard, other kind | `port: 6060`, `{port: "*"}` | `portName=pprof-alt` | `400 port_not_allowed`; no call |
| the name wildcard | `port: 6060`, `{portName: "*"}` | `portName=whatever` | `200` |
| the name wildcard, other kind | `port: 6060`, `{portName: "*"}` | `port=6061` | `400 port_not_allowed`; no call |
| a named default beside numeric entries | `portName: pprof`, `{port: 6061}` | `portName=pprof`, `port=6061` admitted; `portName=metrics`, `port=6060` refused | as listed; each refusal reaches no call |
| a numeric default beside named entries | `port: 6060`, `{portName: pprof-alt}` | `port=6060`, `portName=pprof-alt` admitted; `port=6061`, `portName=other` refused | as listed |
| realm before the list | any list refusing the value | a namespace the realm denies, with that value | `403 realm_denied`; no call, and no mention of the port |
| grammar before the list | any | `port=0`, `port=65536`, `port=abc`, an empty `port`, a repeated `port` | `400 invalid_parameter`, not `port_not_allowed` |
| both parameters | any | `port=6061&portName=pprof-alt` | `400 invalid_parameter` |
| a snapshot swap mid-request | narrowed between realm and discovery | a value the older list admitted | one snapshot decides; the request does not straddle two |

The audit line, from *Logging*:

| Request | `port` field | `code` |
|---|---|---|
| no selection | empty | as the request otherwise ends |
| `port=6061`, admitted | `6061` | `ok` |
| `portName=pprof-alt`, admitted | `pprof-alt`, never the number it resolved to | `ok` |
| `port=6061`, refused | `6061` | `port_not_allowed` |
| `portName=pprof-alt`, refused | `pprof-alt` | `port_not_allowed` |
| repeated, malformed, or both parameters | empty | `invalid_parameter` |

Non-disclosure, from *Non-disclosure*:
no response body, header, or audit line in any row above carries the number a `portName` selection resolved to,
and a `400 port_not_allowed` body names the client's value and nothing else.

Eligibility under a name, from *Eligibility* and *Layers*:
a `portName` one fake Pod declares and another does not lists only the first on the targets endpoint,
and one no Pod declares lists none and answers `503 no_targets` on the profile endpoint.
These rows exist today; keep them and put them under a list that admits the name.

- [ ] **Run the tests and watch the new rows fail**

- [ ] **Close any gap the table finds**

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "test(httpapi): prove default-deny ports"
```

---

## What `/v1/limits` reports

**Files:**
- Modify: `internal/httpapi/listing_test.go`
- Modify: `internal/httpapi/listing.go` (only if a row fails)

The shape landed with the rename so the tree compiles; this task proves it against the encoded body,
per [`docs/specs/ui.md`](../specs/ui.md) *Limits* and the listing rows of *Layers*.

- [ ] **Write the shape table**

| Configuration | Body |
|---|---|
| `port: 6060`, empty list | `"default":{"port":6060}` and `"allowedSelections":[]` |
| `portName: pprof`, empty list | `"default":{"portName":"pprof"}` and `"allowedSelections":[]` |
| `{port: 6061}`, `{portName: pprof-alt}` | `[{"port":6061},{"portName":"pprof-alt"}]` — one key per object, a number for a port |
| `{portName: pprof-alt}`, `{port: 6061}` | the same two objects in the configured order: the response never sorts |
| `{port: "*"}` | `[{"port":"*"}]` — the wildcard is a string |
| `{portName: "*"}` | `[{"portName":"*"}]` |
| an entry equal to the default | reported anyway: the response is the configuration, not the menu the page builds |
| any of the above | the body never contains `null`, and no listing response carries a Pod IP or a port a Pod declares |

The route keeps everything else *Limits* gives it — `cpuSeconds`, `traceSeconds`, `profiles`, `pgo.enabled` —
and every authenticated caller reads it, which *Non-disclosure* already accounts for.

- [ ] **Run the tests and watch them fail if the encoder disagrees**

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "test(httpapi): limits reports the selections"
```

---

## The console's port control

**Files:**
- Create: `internal/ui/static/portmodel.js`, `internal/ui/portmodel_test.go`
- Modify: `internal/ui/static/app.js`, `internal/ui/scan_test.go`, `internal/ui/ui_test.go`,
  `internal/ui/vendor_test.go`
- Modify: `go.mod`, `go.sum`

[`docs/specs/ui.md`](../specs/ui.md) *Controls* puts the port control's rules in two pure functions in `portmodel.js`,
importing nothing,
and *Unit* runs them in a pure-Go ECMAScript interpreter — the one part of the page a test executes.

**Produces:**

```javascript
// deriveControl turns the pprof block of /v1/limits into the control:
// the menu's options and whether each free-form field exists.
function deriveControl(pprof)

// selectionParams turns the control's state into the query it sends:
// {}, {port}, or {portName}, never both.
function selectionParams(state)

export { deriveControl, selectionParams };
```

The module imports nothing, declares plain functions,
and ends in that single `export` statement and no other export,
which is what makes cutting the last statement before evaluation safe —
the interpreter has no module loader and cannot parse `export` ([`docs/specs/ui.md`](../specs/ui.md) *Unit*).

- [ ] **Add the interpreter**

```bash
mise exec -- go get github.com/dop251/goja
```

It is imported only from `internal/ui/portmodel_test.go`.
`go.mod` records it as a test-only dependency of this module;
the `go` directive stays `1.26.0`, which `mise run check` verifies.

- [ ] **Write the model tests**

`portmodel_test.go` reads the source from the embedded tree, cuts the trailing `export` statement,
evaluates the rest in the interpreter, and drives both functions from a table.
A separate case asserts the shape the cut depends on:
no `import` and no dynamic `import(` anywhere, and exactly one `export`, the last statement.

Deriving the control, run against a numeric default and against a named one:

| `allowedSelections` | Menu | Numeric field | Name field |
|---|---|---|---|
| empty | `default` alone | no | no |
| `{port: 6061}` | `default`, `6061` | no | no |
| `{portName: pprof-alt}` | `default`, `pprof-alt` | no | no |
| `{port: 6061}`, `{portName: pprof-alt}` | `default` then both, in the configured order | no | no |
| `{port: "*"}` | `default` alone: a wildcard is never an option | yes | no |
| `{portName: "*"}` | `default` alone | no | yes |
| both wildcards | `default` alone | yes | yes |
| an entry equal to the default | that entry is left out of the menu, and the input list is unchanged | as above | as above |
| `{portName: "6060"}` beside a `{port: 6060}` default | the entry is offered: kinds do not compare | no | no |

Serializing the choice:

| State | Sends |
|---|---|
| `default` | neither parameter |
| a `{port: N}` option | `port=N` |
| a `{portName: name}` option | `portName=name` |
| the numeric field holding `7000` | `port=7000` |
| the name field holding `pprof-alt` | `portName=pprof-alt` |
| the name field holding `123` | `portName=123`, never `port=123`: the control decides the parameter, not the value |
| a non-empty field beside a chosen menu entry | the field wins |
| one field set after the other | the other is cleared, so the two parameters are never sent together |

- [ ] **Run the tests and watch them fail**

- [ ] **Write the module and move the page onto it**

`app.js` imports `./portmodel.js` and renders from `deriveControl`,
and builds its request parameters with `selectionParams`.
The page keeps its state fields and its handlers; only the rules move.
`urls.js` stays the only module that spells a `/v1` path.

- [ ] **Let the existing scans see the new file**

`consoleSources()` in `scan_test.go` gains `portmodel.js`, so the injection and path-literal scans cover it;
`ui_test.go` gains its content type and includes it in the tree-hash cases;
the import scan in `vendor_test.go` holds it to the stricter rule *Unit* states —
no `import` and no `import(` at all, where `app.js` may hold relative ones.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/ go.mod go.sum
git commit -m "feat(ui): derive the port control from limits"
```

---

## What the cluster proves

**Files:**
- Modify: `test/e2e/harness_test.go`, `test/e2e/scenarios_test.go`, `test/e2e/scenarios_auth_test.go`

*Harness* gives the default gateway an `allowedSelections` holding `{port: 6061}` and `{portName: pprof-alt}`,
so it accepts the test application's second port and its name,
and leaves the `ports-gateway` overlay's list empty,
because configuration is loaded once per process and one gateway cannot show both outcomes.
The test application already serves `net/http/pprof` on `:6061` under the container port `pprof-alt`
and already reports per-listener counts from `GET /hits`, so no application change is needed.

The default gateway's configuration is composed in Go by `gatewayConfig` in `test/e2e/harness_test.go`
and applied as a ConfigMap patch over the `default` overlay,
so the two entries are written there rather than in a kustomize file.

- [ ] **Give the default gateway its list**

`gatewayConfig` writes, under `discovery.pprof`, the two entries beside `port: 6060`.
Every gateway that configuration builds gains them,
which includes the two authentication lanes.
The `/v1/limits` assertion in `scenarios_auth_test.go` changes with them:
`allowedSelections` holds exactly those two one-key objects, in the configured order,
and `default` stays `{"port":6060}`.

- [ ] **Rewrite the two scenarios**

| Scenario | Gateway | Proves |
|---|---|---|
| the second port is reachable | the default gateway, whose list holds both entries | `?port=6061` fetches a profile the application's `/hits` attributes to `:6061`; `?portName=pprof-alt` does the same; no `X-Pprof-Target-*` header carries the number; `?portName=pprof-alt` on the targets endpoint lists the application's Pods |
| an empty list refuses both | the `ports-gateway` overlay, whose list is empty | `?port=6061` and `?portName=pprof-alt` are each `400 port_not_allowed` naming the value sent; the application's `:6061` count does not move; `?port=6060`, the configured default, still fetches |

Both scenarios keep their `needsPodReach` capability and their separate registrations,
so a degraded lane skips them by name.
Their comments say what the gateway's list holds;
the present wording — that the default gateway's allowlists are empty —
described the model where an empty list admitted everything and is now false.

- [ ] **Run the suite on the current lane**

```bash
PROFGATE_E2E_LANE=current mise run test:e2e
```

- [ ] **Validate and commit**

```bash
mise exec -- go vet -tags e2e ./test/e2e/...
mise run lint && mise run test && mise run check
git add test/e2e/
git commit -m "test(e2e): default-deny port selection"
```

---

## Documentation, the changelog, and the roadmap

**Files:**
- Modify: `docs/api.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/console.md`,
  `deploy/chart/profgate/README.md`, `.agents/rules/500-validation-and-workflow.md`,
  `CHANGELOG.md`, `docs/plans/roadmap.md`, `docs/plans/port-selection.md`

The gateway spec's *Amendments* block names the first five files as updated when the implementation lands.
Two more are added here because this change makes their current text false,
which is the same reason the table's own rows exist:
`docs/console.md` describes the page's controls, and its port control is now a menu or a free field;
`.agents/rules/500-validation-and-workflow.md` says the page's own JavaScript runs in no test,
which stops being true when `portmodel.js` is evaluated by a Go test.

- [ ] **Update the guides**

| File | Change |
|---|---|
| `docs/api.md` | the `/v1/limits` body carries `allowedSelections` as one-key objects, `[]` when empty; the `port` and `portName` rows name `discovery.pprof.allowedSelections`; the paragraph on what the endpoint discloses names the one list; the port observations under what choosing ports reveals name it too |
| `docs/configuration.md` | the `discovery` table row for `allowedSelections` with its environment variable and its constraints; the prose replacing the two independent fail-open lists with one default-deny list, the two wildcards, and the empty list admitting only the configured default; the three environment cases and the empty-token error; the cross-key validation bullets; the example block; a migration table converting `allowedPorts: []` to `- port: "*"`, `allowedPortNames: []` to `- portName: "*"`, and each concrete old entry to its one-key entry |
| `docs/deployment.md` | the minimum-useful-configuration paragraph: the pprof port is bounded by `discovery.pprof.allowedSelections`, and the shipped manifests leave it empty, so a client may name only the configured default until the operator lists more |
| `docs/console.md` | the port control is a menu of the configured default and every listed selection, with a free-form field only where the matching wildcard is configured |
| `deploy/chart/profgate/README.md` | confirm the value row and the example landed with the manifests |
| `.agents/rules/500-validation-and-workflow.md` | the console's port-control model runs in a Go test; the rest of the page still runs in none, so a change to `internal/ui/static/` outside `portmodel.js` still needs a check in a browser |
| `CHANGELOG.md` | under `## [Unreleased]`, a `### Changed` entry marked as a breaking change: `discovery.pprof.allowedPorts` and `allowedPortNames` are removed together with `PROFGATE_PPROF_ALLOWED_PORTS` and `PROFGATE_PPROF_ALLOWED_PORT_NAMES`, replaced by `discovery.pprof.allowedSelections`; an empty list now admits only the configured default where an empty allowlist used to admit anything; the migration is the two wildcards; `/v1/limits` reports `allowedSelections` in place of the two arrays. Leave the released `0.4.0` section as it is: it describes what that version shipped |

- [ ] **Confirm the invariant wording still matches**

`AGENTS.md`, `README.md`, `.agents/rules/800-security-invariant.md`,
and the spec's *Permission Boundary* already name `discovery.pprof.allowedSelections`;
no edit belongs in this commit.

- [ ] **Finish the plan in the same commit**

Tick the remaining checkbox of the client-selected-port item in [`docs/plans/roadmap.md`](roadmap.md);
set line 3 of this file to `**Status:** Done`;
insert `**Outcome:**` as line 4, naming the commit or tag that shipped the change.
[`.agents/rules/900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
binds that flip to the change that lands the last task,
and the next commit that touches this file deletes it and rewrites every link that cited it.

- [ ] **Validate and commit**

```bash
semlf check docs/api.md docs/configuration.md docs/deployment.md docs/console.md \
  deploy/chart/profgate/README.md .agents/rules/500-validation-and-workflow.md \
  CHANGELOG.md docs/plans/roadmap.md docs/plans/port-selection.md
mise run lint && mise run test && mise run check
git add docs/ deploy/chart/profgate/README.md .agents/rules/ CHANGELOG.md
git commit -m "docs: default-deny client-selected ports"
```

---

## Risks and What This Plan Does Not Cover

- **fuda's treatment of a custom YAML unmarshaler.**
  fuda decodes the file with `gopkg.in/yaml.v3` and knows the `yaml.Unmarshaler` interface,
  so `Selection.UnmarshalYAML` should run under both the strict probe and the fuda load.
  Prove it with the first fixture row before writing the rest;
  if fuda does not honor it, decode the list in the strict pass and copy it onto the loaded configuration,
  and say so in a comment.
- **The interpreter's own dependencies.**
  `github.com/dop251/goja` pulls transitive modules
  that must resolve under `GOTOOLCHAIN=local` with the toolchain [`mise.toml`](../../mise.toml) pins.
  A module requiring a newer Go is a loud failure at `go get`, not a silent download; treat it as a blocker.
- **An operator upgrading in place.**
  A deployment whose configuration still sets a removed key does not start.
  That is the point — the alternative is a key ignored into a default-deny gateway — but it is a restart-time failure,
  so the changelog entry and `docs/configuration.md` carry the conversion table.
- **No test proves the browser renders the control.**
  The two pure functions are evaluated; the rendering around them is not.
  That gap is what the console work in [`docs/plans/roadmap.md`](roadmap.md) closes.
- **Structured error `details`.**
  *Errors* gives `port_not_allowed` a `details` item; no `details` field exists yet,
  and building one is the machine-contract work ordered after this change.

---

## Self-Review

- Spec coverage:
  the entry shape, the empty list, the two wildcards, the always-permitted default, and `400 port_not_allowed`
  (*Port resolution*, in the first two tasks);
  the parameter step before discovery and the realm step before it (*Request algorithm*, second task);
  the parameter rows of both endpoints (*List targets*, *Fetch a profile*, second task);
  the error code and what its body may name (*Errors*, *Non-disclosure*, second task);
  the audit field (*Logging*, second task) and the absence of a label (*Metrics*, no change);
  the global reach of the list (*Limits are not authorization*, stated as a constraint);
  the `/v1/limits` shape ([`docs/specs/ui.md`](../specs/ui.md) *Limits*, third task);
  the port control ([`docs/specs/ui.md`](../specs/ui.md) *Controls*, *Unit*, fourth task);
  the key, its environment form, the removed keys and variables, and the migration table
  (*Configuration*, first and last tasks);
  shipped manifests (*Build and Deployment*, first task);
  the harness and both cluster proofs (*Harness*, *What end-to-end proves*, fifth task);
  the unit rows of *Layers* split across the tasks that own their packages;
  the documents *Amendments* says are updated with the implementation (last task).
- Names defined once and used by those names afterwards:
  `config.Selection`, `config.SelectionKind` with `SelectionPort` and `SelectionPortName`,
  `config.AnySelection`, `config.ParseSelection`, `PprofConfig.AllowedSelections`,
  and the two predicates `AllowsPort` and `AllowsPortName`, which keep their names and change their answers;
  in the browser, `deriveControl` and `selectionParams`.
- Current-source facts this plan rests on:
  `PortSelection`, `Targets`, and per-Pod port resolution already carry a client's choice,
  so the Kubernetes seam is untouched;
  `allowPort` already asks the configuration snapshot and builds the error with the value as sent;
  `parsePortParams` already holds the grammar of both parameters and already removes them from the query;
  the audit `port` field and its `invalid_parameter` and `port_not_allowed` cases already exist;
  `pprofView` in `internal/httpapi/listing.go` is non-test code reading both removed fields;
  `deploy/deploy_test.go` and `deploy/chart_test.go` load the shipped and rendered ConfigMap through `config.Load`;
  `test/e2e/overlays/ports-gateway/configmap.yaml` today lists a narrow allowlist rather than an empty one;
  the default gateway's configuration is composed by `gatewayConfig` and applied by `configPatch`;
  the two authentication lanes build their gateways from that same function;
  the test application already listens on `:6061` as `pprof-alt` and counts hits per listen address;
  `internal/ui/static/` holds `app.js` and `urls.js` and no model module,
  and the scans in `scan_test.go`, `ui_test.go`, and `vendor_test.go` enumerate the files they cover;
  `go.mod` does not yet require `github.com/dop251/goja`;
  fuda applies an `env` tag on presence, empty value included,
  and converts a string into a slice as one CSV record, which cannot yield an empty list;
  no `details` field exists in `internal/httpapi/errors.go`.
- Decided here because the spec leaves it to the implementer:
  `Selection` is a two-field record — the kind and the value, with `"*"` as the value of a wildcard —
  rather than four fields that could contradict each other;
  the environment variable is read by `Load` rather than by an `env` tag, for the fuda reason above;
  the removed file keys are detected by a probe decode before the strict pass,
  and the removed variables by name before the file is read;
  the list rules run after the environment override, on the list that will be used;
  the two browser functions are named `deriveControl` and `selectionParams`.
- Left to the implementer by design: helper names inside test files,
  the exact fixture file names under `internal/config/testdata/`,
  the wording of the console's labels,
  and whether the removed-key probe is one struct or two.
