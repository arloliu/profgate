# Client-Selected Port Becomes Default-Deny

**Status:** Approved

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
`400 port_not_allowed` names the parameter it refused in a `details` item,
`/v1/limits` reports the list so the console can offer a menu or a free field,
and the end-to-end suite proves both outcomes against a real cluster.

**Architecture:** No new package, no new seam, and no change to the Kubernetes interface.
`internal/config` gains the `Selection` value type, the `allowedSelections` field,
and the two predicates the HTTP layer already calls;
the admission point in `internal/httpapi` keeps its shape and changes only what the predicates answer;
the error envelope in `internal/httpapi/errors.go` gains an optional `details` array,
filled by `port_not_allowed` alone;
`internal/httpapi/listing.go` renders the list into `/v1/limits`;
`internal/ui` gains `static/portmodel.js`, two pure functions that turn `/v1/limits` into the port control
and an edit of the control into its next state and the query parameter it sends;
`deploy/` and `test/e2e/` ship the new key;
`scripts/check-repo.py` proves the removed names are gone from code and manifests;
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
the documentation change is where that second list is honored.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- **The permission invariant text needs no edit.**
  `AGENTS.md`, `README.md`, [`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md),
  and the spec's *Permission Boundary* already carry the wording that names `discovery.pprof.allowedSelections`.
  Confirm they still match before the last commit; do not reword them.
  `docs/deployment.md` is the one document whose invariant paragraph still describes the old lists,
  and the documentation change replaces it with that same wording.
- **No RBAC change.**
  No Kubernetes verb, resource, or API group moves.
  `TestClusterRoleTuples` in `deploy/deploy_test.go`
  and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go` stay green and untouched by every task.
- **No new runtime module.**
  `github.com/dop251/goja` is imported only from `_test.go` files in `internal/ui`;
  the binary's module set does not change.
  No import check for it is added to `scripts/check-repo.py`:
  the import greps there guard packages that can widen the gateway's runtime capability,
  and an interpreter used by a test cannot.
  The one check this plan adds there scans for the removed names, which is a different kind of guard.
- **Only `internal/k8s` imports `k8s.io/client-go`**, unchanged;
  `mise run check` enforces it.
- **The request algorithm does not move.**
  The parameter step validates the selection's grammar and then checks it against the list, before discovery;
  a realm denial still precedes both,
  and a refused value reaches no `Discovery` call (*Request algorithm*, *Port resolution*).
- **Non-disclosure holds.**
  A `400 port_not_allowed` body names only the value the client sent, in `error` and in its one `details` item;
  no response, header, or audit line carries the number a `portName` resolved to (*Non-disclosure*, *Errors*).
- **The audit record and the metric label sets do not change.**
  `port` keeps the meaning *Logging* gives it — the selection as sent, empty when absent or malformed,
  the client's value beside `port_not_allowed` — and the port selection becomes no metric label (*Metrics*).
- **This is a breaking change.**
  A configuration that sets either removed key, or either removed environment variable,
  fails validation with a message naming `allowedSelections` and `PROFGATE_PPROF_ALLOWED_SELECTIONS` (*Configuration*).
  It ships in the next minor release; the `CHANGELOG.md` entry says so.
- **`details` is laid for one code and extended later.**
  *Errors* gives `port_not_allowed` exactly one `details` item, `code` `not_admitted`, `field` `port` or `portName`.
  This plan adds the field to the envelope and fills it for that code alone;
  every other error omits the key.
  The vocabularies of `invalid_parameter` and `limit_exceeded` belong to the machine-contract work
  that [`docs/plans/roadmap.md`](roadmap.md) orders after this one,
  and that work extends the field this plan lays rather than adding a second one.
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
internal/config/testdata/removed-*.yaml         # files still setting a removed key, one per row
internal/httpapi/errors.go                      # errorDetail; details on the envelope, omitted when empty
internal/httpapi/errors_test.go                 # the envelope with and without details
internal/httpapi/profile.go                     # portNotAllowed carries the one details item
internal/httpapi/fixtures_test.go               # testConfig states its pprof block; expectError refuses stray details
internal/httpapi/listing.go                     # pprofView carries allowedSelections
internal/httpapi/listing_test.go                # the /v1/limits shape
internal/httpapi/server.go                      # allowPort, unchanged in shape
internal/httpapi/server_test.go                 # every port case under a stated list
internal/ui/static/portmodel.js                 # the port control's two pure functions
internal/ui/static/app.js                       # renders and edits through portmodel.js
internal/ui/portmodel_test.go                   # the goja table over both functions
internal/ui/scan_test.go, ui_test.go, vendor_test.go   # portmodel.js joins the scanned tree; app.js is held to it
go.mod, go.sum                                  # github.com/dop251/goja, test-only
scripts/check-repo.py                           # the removed-name scan
deploy/base/configmap.yaml                      # allowedSelections: []
deploy/chart/profgate/values.yaml               # allowedSelections: []
deploy/chart/profgate/README.md                 # the shipped value and its row
deploy/deploy_test.go, deploy/chart_test.go     # the shipped-empty and the mixed ordered list
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
- Create: `internal/config/testdata/selections-*.yaml`, `internal/config/testdata/removed-*.yaml`
- Delete: `internal/config/testdata/allowed-ports.yaml`, `allowed-ports-dup.yaml`,
  `allowed-ports-dup-name.yaml`, `allowed-ports-name.yaml`, `allowed-ports-range.yaml`,
  `allowed-ports-unknown.yaml`
- Modify: `internal/httpapi/fixtures_test.go`, `internal/httpapi/server_test.go`,
  `internal/httpapi/listing.go`, `internal/httpapi/listing_test.go`
- Modify: `internal/httpapi/server.go` (only if the refusal table finds a gap)
- Modify: `internal/ui/static/app.js`
- Modify: `scripts/check-repo.py`
- Modify: `deploy/base/configmap.yaml`, `deploy/chart/profgate/values.yaml`,
  `deploy/chart/profgate/README.md`, `deploy/deploy_test.go`, `deploy/chart_test.go`
- Modify: `test/e2e/overlays/ports-gateway/configmap.yaml`, `test/e2e/scenarios_auth_test.go`

**Why this commit is one commit.**
An empty list stops meaning *anything* and starts meaning *the default alone*,
and the shared HTTP test configuration, `testConfig` in `internal/httpapi/fixtures_test.go`,
sets no `Discovery.Pprof` at all.
Every port case in `internal/httpapi/server_test.go` that names a value beyond the default passes today,
because the old predicates accept everything under an empty list,
so the rename and the new meaning cannot be separated into a commit that compiles and a commit that passes:
the moment the predicates change, those cases fail unless each states the entry that admits its value.
`AllowedPorts` and `AllowedPortNames` are also read by non-test code (`internal/httpapi/listing.go`),
and `deploy/deploy_test.go` and `deploy/chart_test.go` load the shipped and the rendered ConfigMap through `config.Load`,
so validation refusing the old keys turns those tests red until the manifests carry the new key.
Every task ends with a green `mise run lint && mise run test && mise run check`,
so the field, its meaning, and every place that names it land together.
Two of those places fail silently rather than loudly and must not be forgotten:
`test/e2e/overlays/ports-gateway/configmap.yaml`, whose gateway would simply refuse to start,
and `internal/ui/static/app.js`, which no test executes today
and would quietly render an empty menu.
The `/v1/limits` shape, the `details` item, the console model, and the cluster proofs land in the tasks after this one,
on a tree that is already default-deny.

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

1. Refuse `PROFGATE_PPROF_ALLOWED_PORTS` and `PROFGATE_PPROF_ALLOWED_PORT_NAMES` by name with `os.LookupEnv`,
   so a variable set to the empty string is refused like any other.
   No decoder can reach them: a variable no field claims is invisible to fuda (*Configuration*).
2. Refuse a file that still sets `discovery.pprof.allowedPorts` or `discovery.pprof.allowedPortNames`,
   before the strict unknown-key pass,
   which would otherwise report only that the key is unknown and leave the operator to guess the replacement.
   Decode the bytes into a `yaml.Node`, walk to the `discovery` mapping and then to its `pprof` mapping,
   decode that one mapping into a `map[string]any`,
   and look the two names up in the map.
   Presence is the key, never its value:
   `allowedPorts: null` and `allowedPorts: []` are both a set key, the first with a `nil` value,
   where a decode into a pointer field would read `null` as absent.
   Decoding into a map rather than reading key nodes is what makes a merge key count:
   `gopkg.in/yaml.v3` resolves `<<` while decoding a mapping into a map as it does into a struct,
   so a removed key carried in by an anchored mapping, or by a sequence of them,
   is present in the map and gets the replacement message,
   where a walk over the mapping's own key nodes would see only `<<`.
   A missing `discovery` or `pprof` mapping means neither key is set,
   and a `pprof` value that is not a mapping is left to the strict decode to refuse.
3. The existing strict `KnownFields(true)` decode.
4. The existing fuda load.
5. Apply `PROFGATE_PPROF_ALLOWED_SELECTIONS` when it is set:
   the empty string replaces the file's list with an empty one;
   any other value is split on commas into tokens read by `ParseSelection`, in order,
   and an empty token is an error rather than a dropped entry.
6. `normalize`, unchanged.
7. `validate`, which gains the list rules.

Validation splits three ways, and each part has one home:
the entry's shape and its scalar grammar in `UnmarshalYAML`;
the token's shape in `ParseSelection`;
and the list rules — no duplicate entry, no wildcard beside a concrete entry of its own kind — in `validate`,
because those must judge the list the environment may have replaced.

A custom `UnmarshalYAML` bypasses `KnownFields(true)` inside the entry,
so the unmarshaler itself refuses an entry carrying both keys, neither, or a third key.
It counts the keys it is handed rather than assigning them,
so a mapping that repeats one key — `{port: 6061, port: 6062}` — is refused as two keys,
not collapsed into whichever value came last.

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

Loading and validation, one fixture per row under `testdata/selections-*.yaml`
and, for the removed keys, `testdata/removed-*.yaml`:

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
| `- {port: 6061, port: 6062}` | error: a repeated key is two keys, not the last one |
| `- port: 0` and `- port: 65536` | error naming the 1–65535 range |
| `- port: abc` | error naming the range |
| `- portName: Pprof`, `-x`, sixteen characters, all digits | error naming the container-port name rule |
| `- port: 6061` twice | error naming the duplicate entry |
| `- portName: pprof-alt` twice | error naming the duplicate entry |
| `- port: "*"` beside `- port: 6061` | error: a wildcard beside a concrete entry of its own kind |
| `- portName: "*"` beside `- portName: pprof-alt` | error |
| `- port: "*"` beside `- portName: pprof-alt` | loads: the kinds do not interact |
| `allowedPorts: [6061]` | error naming `allowedSelections` and `PROFGATE_PPROF_ALLOWED_SELECTIONS` |
| `allowedPortNames: [pprof-alt]` | the same message |
| `allowedPorts: null` | the same message: the key is set, whatever its value |
| `allowedPortNames: null` | the same message |
| `allowedPorts: [6061]` behind `<<: *old`, an anchored mapping defined above `discovery` | the same message: a merged key is a set key |
| `allowedPortNames: [pprof-alt]` behind `<<: [*a, *b]`, a sequence of anchored mappings | the same message |
| `allowedSelections: [{port: 6061}]` beside `<<: *old` carrying `allowedPorts` | the same message: a valid key beside a merged removed one does not hide it |
| `allowedPorts: []` | the same message |
| `allowedPortNames: []` | the same message |
| `allowedPorts: []` and `allowedPortNames: []` together | the same message, and it is the removed-key message rather than the unknown-key one |

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
| `PROFGATE_PPROF_ALLOWED_PORTS=6061` | any | error naming `allowedSelections` and `PROFGATE_PPROF_ALLOWED_SELECTIONS` |
| `PROFGATE_PPROF_ALLOWED_PORT_NAMES=pprof-alt` | any | the same message |
| `PROFGATE_PPROF_ALLOWED_PORTS=` (set, empty) | any | the same message: the variable is refused on presence, as `os.LookupEnv` reports it |
| `PROFGATE_PPROF_ALLOWED_PORT_NAMES=` (set, empty) | any | the same message |

The refusal messages are asserted on their text,
so an operator carrying an older deployment forward reads what to write (*Configuration*).
`t.Setenv` with an empty value is what sets a variable to the empty string in a Go test.

- [ ] **Write the refusal table**

`allowPort` in `internal/httpapi/server.go` already asks the configuration snapshot the two questions
and answers `400 port_not_allowed` with the value as sent,
so the request-level outcomes of the new model are proven here and the code changes only where a row fails.
Rows restate *Port resolution*, *Request algorithm*, *Errors*, *Logging*, and the `internal/httpapi` rows of *Layers*,
each run against the targets endpoint and the profile endpoint.

First the shared configuration.
`testConfig` in `fixtures_test.go` gains `Discovery.Pprof = config.PprofConfig{Port: 6060}` and no entry,
so every harness starts default-deny and states the list it needs;
the `pprof` helper in `TestPortAllowlist` keeps replacing the whole block.

Then the cases that exist today, each of which named a value the old empty list let through.
Every one keeps its purpose and gains the entry that admits its value,
or turns into the refusal it now proves:

| Test | Case today | Under a stated list |
|---|---|---|
| `TestPortAllowlist` | `portName abc is valid` — an arbitrary name is accepted under empty lists | under `{portName: "*"}`: `200`, and the fake sees `PortName: "abc"`; under the empty list the same request is `400 port_not_allowed` with no call |
| `TestPortAllowlist` | `empty lists accept any port` — `port=9999` under empty lists | becomes `an empty list refuses a number beyond the default`: `400 port_not_allowed`, counts `0, 0`, no call; a sibling under `{port: "*"}` sends `port=9999` and gets `200` with the numeric selection seen |
| `TestPortAllowlist` | `disallowed port` — `AllowedPorts: [6060]`, `port=6061` | the empty list; the body assertions stay: the message names `6061` and no other number |
| `TestPortAllowlist` | `disallowed name` — `AllowedPortNames: [pprof]`, `portName=pprof-alt` | `{portName: pprof}`; the message names `pprof-alt` |
| `TestPortAllowlist` | `lists are independent` — ports bound leaves any name, names bound leaves any port | becomes `each kind is bounded on its own`: `{port: 6061}` with `portName=anything` is `400 port_not_allowed`, `{portName: pprof-alt}` with `port=9999` is `400 port_not_allowed`, `{port: "*"}` with `portName=anything` is `400 port_not_allowed`, and `{portName: "*"}` with `port=9999` is `400 port_not_allowed` |
| `TestPortAllowlist` | `default number always passes` — `AllowedPorts: [7070]`, `port=6060` | `{port: 7070}`; unchanged otherwise |
| `TestPortAllowlist` | `default name always passes` — `PortName: pprof`, `AllowedPortNames: [metrics]` | `{portName: metrics}`; unchanged otherwise |
| `TestPortAllowlist` | `narrowed` — both old lists set; no selection on either endpoint | `{port: 7070}` and `{portName: metrics}`; both rows still `200` with the zero selection |
| `TestPortAllowlist` | `allowlist reads the request's snapshot` — `[6060, 7070]` narrowed to `[6060]` mid-request | `{port: 7070}` narrowed to the empty list; `port=7070` still `200` |
| `TestPortAllowlist` | `realm before allowlist`, `grammar before allowlist`, `allowlist before discovery error`, `parameters before allowlist` | each configures the empty list, which refuses the value the row sends; outcomes unchanged |
| `TestPortResolution` | `altHarness` — `pprof-alt` on one Pod of two, `nowhere` on none | the harness configures `{portName: pprof-alt}` and `{portName: nowhere}`, so `name on one Pod of two`, both `name on no Pod` rows, and `audit name` run under an admitted name and keep their outcomes: one Pod listed, an empty list, `503 no_targets`, and no resolved number in the audit line |
| `TestPortResolution` | `audit numeric` — `port=6061` under the default harness | configures `{port: 6061}`; the audit field is still `6061` |
| `TestPortResolution` | `audit disallowed` — `AllowedPorts: [6060]`, `port=6061` | the empty list; unchanged otherwise |
| `TestPortResolution` | `non-pprof listener passes through` — `port=8080` under the default harness | configures `{port: 8080}`; the upstream `404` and the audit field are unchanged |

Then the rows of the new model:

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

- [ ] **Write the shipped-manifest tests**

`TestConfigMap` in `deploy/deploy_test.go` asserts the base ConfigMap renders `allowedSelections: []`
and loads with an empty `AllowedSelections`.

`deploy/chart_test.go` has four subtests that name the old fields, and each has a replacement:

| Test | Subtest today | Replacement |
|---|---|---|
| `TestChartConfigIsMergedAndParses` | `raw config block` — asserts both old lists empty beside the raw block's values | asserts `AllowedSelections` is empty |
| `TestChartPortAllowlists` | `defaults render both lists empty` | `defaults render the list empty`: the rendered `config.yaml` contains `allowedSelections: []`, and the loaded list is empty; the test's comment says the empty list admits only the configured default |
| `TestChartPortAllowlists` | `portName only` — a named default leaves both lists empty | a named default leaves `AllowedSelections` empty |
| `TestChartPortAllowlists` | `narrows the lists` — `allowedPorts=[6060]` and `allowedPortNames=["pprof"]` set independently | `a mixed list keeps its order`: `--set-json config.discovery.pprof.allowedSelections=[{"portName":"pprof-alt"},{"port":6061},{"port":"*"}]`, loaded through `config.Load`, is exactly those three entries in that order with their kinds intact — a named entry, a numeric one, and the port wildcard, proving the chart's merge neither sorts nor re-types them |
| `TestChartPortAllowlists` | none | `the chart cannot bypass validation`: `--set-json config.discovery.pprof.allowedSelections=[{"port":"*"},{"port":6061}]` renders, and `config.Load` refuses the rendered file with the wildcard-beside-concrete message; the same for `[{"portName":"*"},{"portName":"pprof-alt"}]` |

`TestChartPortAllowlists` is renamed to say what it now covers, `TestChartAllowedSelections`.

- [ ] **Write the removed-name scan**

`scripts/check-repo.py` gains `check_removed_port_keys`,
beside the import checks it already runs over `*.go` files and in the same style.
It reads every `*.go`, `*.yaml`, `*.yml`, and `*.tpl` file under the tree,
plus `deploy/chart/profgate/README.md`,
and fails on any line containing `AllowedPorts`, `AllowedPortNames`, `allowedPorts`, `allowedPortNames`,
`PROFGATE_PPROF_ALLOWED_PORTS`, or `PROFGATE_PPROF_ALLOWED_PORT_NAMES`.
It skips `internal/config/`, which holds the refusal and the fixtures that prove it,
and it never reads `CHANGELOG.md` or `docs/configuration.md`,
where the released entry and the migration table name the old keys on purpose.
`mise run check` runs it in every validation block from here on,
and the check's docstring gains one line saying what it guards.

- [ ] **Run the tests and watch them fail**

- [ ] **Implement the configuration**

Add the type, the field, the four methods, the `Load` steps, and the `validate` rules as written above.
Delete `AllowedPorts`, `AllowedPortNames`, and the `firstDuplicate` calls they fed
if nothing else uses that helper.
Error text follows the existing style: the key path, then the rule.

- [ ] **Carry the rename through the tree**

| File | Change |
|---|---|
| `internal/httpapi/fixtures_test.go` | `testConfig` states its pprof block as written above |
| `internal/httpapi/server_test.go` | the two tables above |
| `internal/httpapi/listing.go` | `pprofView` carries `AllowedSelections []config.Selection` under the JSON name `allowedSelections`, built so an empty list encodes `[]` and never `null` |
| `internal/httpapi/listing_test.go` | every `config.PprofConfig` literal builds `AllowedSelections` instead of the two lists; the body assertions read `allowedSelections` and grow in the task after this one |
| `internal/httpapi/server.go` | only where a row of the refusal table fails |
| `internal/ui/static/app.js` | the port control reads `pprof.allowedSelections`, offering one option per entry and a free field only for a wildcard; the model moves to its own module with the port-control change |
| `deploy/base/configmap.yaml`, `deploy/chart/profgate/values.yaml` | `allowedSelections: []`, with the comment saying an empty list accepts only the configured default |
| `deploy/chart/profgate/README.md` | the example block and the `config` row name `allowedSelections` and say the empty list is default-deny |
| `deploy/deploy_test.go`, `deploy/chart_test.go` | the tables above |
| `test/e2e/overlays/ports-gateway/configmap.yaml` | `allowedSelections: []`, which is what its scenario proves; the realm's `namespaces: ["placeholder"]` line stays exactly as written, because `deployScopedGateway` patches that literal and fails the scenario when it finds nothing to replace |
| `test/e2e/scenarios_auth_test.go` | the `/v1/limits` struct reads `allowedSelections`; the assertion is adjusted again when the default gateway's list grows |

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/... ./deploy/
mise exec -- go vet -tags e2e ./test/e2e/...
mise run lint && mise run test && mise run check
semlf check deploy/chart/profgate/README.md
git add internal/ scripts/ deploy/ test/e2e/
git commit -m "feat(config)!: one allowedSelections list"
```

---

## The refusal names its parameter

**Files:**
- Modify: `internal/httpapi/errors.go`, `internal/httpapi/errors_test.go`,
  `internal/httpapi/profile.go`, `internal/httpapi/fixtures_test.go`, `internal/httpapi/server_test.go`

*Errors* gives `port_not_allowed` one `details` item, and no other error this plan touches carries one;
and the `internal/httpapi` row of *Layers* asks for it against the encoded body.
The envelope today is two strings, `error` and `code`, in `errorBody`,
written by `WriteError` from a `requestError` that `fail` hands it.
The smallest change that carries the item:

```go
// errorDetail is one input the caller has to change (*Errors*).
type errorDetail struct {
    Field   string `json:"field"`
    Code    string `json:"code"`
    Message string `json:"message"`
}

// requestError gains:
    details []errorDetail

// errorBody gains:
    Details []errorDetail `json:"details,omitempty"`
```

`fail` writes the body with the details its `requestError` carries;
`ErrorEnvelope` and `WriteError` keep their signatures and write no details,
because their callers — the console's own `404` and `405` in `internal/ui/ui.go`,
and the upstream-outcome path in `server.go` — have none.
`omitempty` on a nil slice is what omits the key;
nothing may build an empty non-nil slice, and the test below holds that.
`portNotAllowed` in `profile.go` takes the parameter beside the value,
and `allowPort` passes `port` or `portName` for whichever case fired:

```go
details: []errorDetail{{Field: "port", Code: "not_admitted", Message: `6061 is not an admitted selection`}}
```

The message names the value the client sent and nothing else,
which is what *Non-disclosure* already allows.

- [ ] **Write the envelope tests**

`errors_test.go` keeps `TestWriteError` byte for byte:
its expected body has no `details` key, which is the proof that an error without details omits it.
`server_test.go` adds, against the encoded body rather than the struct:

| Request | Body |
|---|---|
| `port=6061` refused | `"details":[{"field":"port","code":"not_admitted","message":"6061 is not an admitted selection"}]`, exactly one item |
| `portName=pprof-alt` refused | `"details":[{"field":"portName","code":"not_admitted","message":"pprof-alt is not an admitted selection"}]` |
| `portName=pprof-alt` refused under the `altHarness` Pods | no item, and no byte of the body, carries `6061`, the number the name resolves to on one Pod |
| `403 realm_denied`, `400 invalid_parameter`, `404 service_not_found`, `503 no_targets` | the body has no `details` key at all |

`expectError` in `fixtures_test.go` gains one assertion that runs on every error it checks:
the body contains no `"details":null` and no `"details":[]`,
and contains `"details"` only when the code is `port_not_allowed`.
Every existing error case in the package then proves the omission for its own code.

- [ ] **Run the tests and watch them fail**

- [ ] **Carry the item**

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "feat(httpapi): port_not_allowed names its field"
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
*Unit* also asks the table to prove that setting one field clears the other,
and that transition today lives in the page's two input handlers, `onPortNumber` and `onPortName` in `app.js`,
which a test cannot reach.
So the second function is the transition itself, not a serializer of an already-formed state:
it takes the control's state and one edit, and returns the next state and what that state sends.
Both handlers call it, and so does the request builder, with no edit,
so clearing passes through the function the table drives.
The spec names no function, so no spec edit is needed.

**Produces:**

```javascript
// deriveControl turns the pprof block of /v1/limits into the control:
// the menu's options and whether each free-form field exists.
function deriveControl(pprof)

// applyInput applies one edit — source is "menu", "number", or "name",
// or undefined for no edit — to the control's state and returns
// { state, params }: the next state, and the query it sends, which is
// {}, {port}, or {portName}, never both. An edit to one free-form field
// clears the other.
function applyInput(state, source, value)

export { deriveControl, applyInput };
```

The state is the three fields the page already keeps — `portChoice`, `portNumber`, `portName` —
and `applyInput` returns a new object rather than mutating the one it was handed.

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

Applying an edit, starting from the empty state unless a row says otherwise,
asserting both the `state` and the `params` returned:

| Edit | State after | Sends |
|---|---|---|
| none, on the empty state | unchanged | neither parameter |
| menu `default` | `portChoice` `default` | neither parameter |
| menu, a `{port: N}` option | `portChoice` names it | `port=N` |
| menu, a `{portName: name}` option | `portChoice` names it | `portName=name` |
| number `7000` | `portNumber` `7000`, `portName` empty | `port=7000` |
| name `pprof-alt` | `portName` `pprof-alt`, `portNumber` empty | `portName=pprof-alt` |
| name `123` | `portName` `123` | `portName=123`, never `port=123`: the control decides the parameter, not the value |
| number `7000` after a menu option was chosen | the menu choice stays in the state | `port=7000`: the field wins |
| name `pprof-alt`, then number `7000` | `portName` is empty and `portNumber` is `7000` | only `port=7000` |
| number `7000`, then name `pprof-alt` | `portNumber` is empty and `portName` is `pprof-alt` | only `portName=pprof-alt` |
| number `7000`, then number cleared to the empty string | both fields empty | what the menu choice sends |
| any sequence of edits | the returned `params` never holds both keys, and the state handed in is not mutated | — |

- [ ] **Run the tests and watch them fail**

- [ ] **Write the module and move the page onto it**

`app.js` imports `./portmodel.js` and renders from `deriveControl`.
`onPortChoice`, `onPortNumber`, and `onPortName` each call `applyInput` with their source and value
and store the returned `state`;
the `portChoice()` method, which today serializes the three fields by hand,
becomes `applyInput(this.state).params`, so the rules exist in one place.
`urls.js` stays the only module that spells a `/v1` path.

- [ ] **Let the existing scans see the new file, and hold the page to it**

`consoleSources()` in `scan_test.go` gains `portmodel.js`, so the injection and path-literal scans cover it;
`ui_test.go` gains its content type and includes it in the tree-hash cases;
the import scan in `vendor_test.go` holds it to the stricter rule *Unit* states —
no `import` and no `import(` at all, where `app.js` may hold relative ones.
`scan_test.go` also gains `TestScanPageUsesPortModel`:
`app.js` contains an `import` whose specifier is `./portmodel.js` naming both functions,
and calls `deriveControl(` and `applyInput(` at least once each,
so a page that stops going through the model turns the suite red, even though no test renders it.

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
| an empty list refuses both | the `ports-gateway` overlay, whose list is empty | `?port=6061` and `?portName=pprof-alt` are each `400 port_not_allowed` naming the value sent, with one `details` item whose `field` is the parameter sent; the application's `:6061` count does not move; `?port=6060`, the configured default, still fetches |

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

The gateway spec's *Amendments* block names the first five files as updated when the implementation lands,
and for `docs/deployment.md` names three edits:
the invariant sentence, the pprof-port prose, and the NetworkPolicy sentence.
Two more files are added here because this change makes their current text false,
which is the same reason the table's own rows exist:
`docs/console.md` describes the page's controls, and its port control is now a menu or a free field;
`.agents/rules/500-validation-and-workflow.md` says the page's own JavaScript runs in no test,
which stops being true when `portmodel.js` is evaluated by a Go test.

- [ ] **Update the guides**

| File | Change |
|---|---|
| `docs/api.md` | the `/v1/limits` body carries `allowedSelections` as one-key objects, `[]` when empty; the `port` and `portName` rows name `discovery.pprof.allowedSelections`; `400 port_not_allowed` shows its one `details` item and says every other error omits the key; the paragraph on what the endpoint discloses names the one list; the port observations under what choosing ports reveals name it too |
| `docs/configuration.md` | the `discovery` table row for `allowedSelections` with its environment variable and its constraints; the prose replacing the two independent fail-open lists with one default-deny list, the two wildcards, and the empty list admitting only the configured default; the three environment cases and the empty-token error; the cross-key validation bullets; the example block; a migration table converting `allowedPorts: []` to `- port: "*"`, `allowedPortNames: []` to `- portName: "*"`, and each concrete old entry to its one-key entry |
| `docs/deployment.md`, the pprof-port bullet under the minimum useful configuration | the pprof port is bounded by `discovery.pprof.allowedSelections`, and the shipped manifests leave it empty, so a client may name only the configured default until the operator lists more |
| `docs/deployment.md`, the permission-invariant paragraph under *RBAC* | today it says that when an allowlist is empty the gateway connects to any port or port name a client names; replace it with the invariant wording `AGENTS.md`, `README.md`, and `.agents/rules/800-security-invariant.md` carry — the configured pprof port, and any port or port name `allowedSelections` admits by an exact entry or by a wildcard, wherever NetworkPolicy permits |
| `docs/deployment.md`, the closing sentence of *NetworkPolicy, disruption budget, and scheduling* | today it says each empty allowlist accepts every value and two empty lists leave NetworkPolicy as the only bound; replace it with: a client may name the configured default and any selection `allowedSelections` admits, exactly or by wildcard, and only under a wildcard is NetworkPolicy the bound on which Pod ports the gateway reaches |
| `docs/console.md` | the port control is a menu of the configured default and every listed selection, with a free-form field only where the matching wildcard is configured |
| `deploy/chart/profgate/README.md` | confirm the value row and the example landed with the manifests |
| `.agents/rules/500-validation-and-workflow.md` | the console's port-control model runs in a Go test; the rest of the page still runs in none, so a change to `internal/ui/static/` outside `portmodel.js` still needs a check in a browser |
| `CHANGELOG.md` | under `## [Unreleased]`, a `### Changed` entry marked as a breaking change: `discovery.pprof.allowedPorts` and `allowedPortNames` are removed together with `PROFGATE_PPROF_ALLOWED_PORTS` and `PROFGATE_PPROF_ALLOWED_PORT_NAMES`, replaced by `discovery.pprof.allowedSelections`; an empty list now admits only the configured default where an empty allowlist used to admit anything; the migration is the two wildcards; `/v1/limits` reports `allowedSelections` in place of the two arrays; `400 port_not_allowed` carries a `details` item. Leave the released `0.4.0` section as it is: it describes what that version shipped |

- [ ] **Confirm the invariant wording matches everywhere**

`AGENTS.md`, `README.md`, `.agents/rules/800-security-invariant.md`,
and the spec's *Permission Boundary* already name `discovery.pprof.allowedSelections`;
no edit belongs in this commit for those four.
Read the rewritten `docs/deployment.md` paragraph beside them and hold it to the same wording.
Then search the guides and the manifests for the two removed key names:
`mise run check` already refuses them in Go, YAML, and the chart,
and a manual search over `docs/` and `README.md` should find them only in `docs/configuration.md`'s migration table,
in `CHANGELOG.md`, and in the specs and plans that record the change.

- [ ] **Finish the plan in the same commit**

Tick the remaining checkbox of the client-selected-port item in [`docs/plans/roadmap.md`](roadmap.md);
set line 3 of this file to `**Status:** Done`;
insert `**Outcome:**` as line 4, naming the commit or tag that shipped the change.
[`.agents/rules/900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
binds that flip to the change that lands the plan's remaining work,
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
- **No test renders the browser control.**
  The two pure functions are evaluated, and a scan holds `app.js` to importing and calling them;
  the rendering around them is not executed.
  That gap is what the console work in [`docs/plans/roadmap.md`](roadmap.md) closes.
- **`details` for the other codes.**
  The field exists after this plan and one code fills it.
  The `invalid_parameter` and `limit_exceeded` vocabularies *Errors* defines are the machine-contract work
  that [`docs/plans/roadmap.md`](roadmap.md) orders after this one,
  and until it lands those errors omit the key, which *Errors* permits for a code without a vocabulary in place.

---

## Self-Review

- Spec coverage:
  the entry shape, the empty list, the two wildcards, the always-permitted default, and `400 port_not_allowed`
  (*Port resolution*, in *One list replaces two allowlists*);
  the parameter step before discovery and the realm step before it (*Request algorithm*, the same task);
  the parameter rows of both endpoints (*List targets*, *Fetch a profile*, the same task);
  the error code, what its body may name, and its one `details` item
  (*Errors*, *Non-disclosure*, *The refusal names its parameter*);
  the audit field (*Logging*, the configuration change) and the absence of a label (*Metrics*, no change);
  the global reach of the list (*Limits are not authorization*, stated as a constraint);
  the `/v1/limits` shape ([`docs/specs/ui.md`](../specs/ui.md) *Limits*, *What `/v1/limits` reports*);
  the port control, including the clearing transition
  ([`docs/specs/ui.md`](../specs/ui.md) *Controls*, *Unit*, *The console's port control*);
  the key, its environment form, the removed keys and variables including the null and empty forms,
  and the migration table
  (*Configuration*, the configuration change and the documentation change);
  shipped manifests (*Build and Deployment*, the configuration change);
  the harness and both cluster proofs (*Harness*, *What end-to-end proves*, *What the cluster proves*);
  the unit rows of *Layers* split across the tasks that own their packages;
  the three `docs/deployment.md` edits and the other documents *Amendments* says are updated with the implementation
  (the documentation change).
- Each task's stated tests are green before its commit against the tree that task leaves:
  the configuration change carries the shared fixture and every port case the new meaning touches,
  so no change after it inherits a red package.
- Names defined once and used by those names afterwards:
  `config.Selection`, `config.SelectionKind` with `SelectionPort` and `SelectionPortName`,
  `config.AnySelection`, `config.ParseSelection`, `PprofConfig.AllowedSelections`,
  and the two predicates `AllowsPort` and `AllowsPortName`, which keep their names and change their answers;
  `errorDetail` and the `details` field in `internal/httpapi`;
  `check_removed_port_keys` in `scripts/check-repo.py`;
  in the browser, `deriveControl` and `applyInput`.
- Current-source facts this plan rests on:
  `PortSelection`, `Targets`, and per-Pod port resolution already carry a client's choice,
  so the Kubernetes seam is untouched;
  `allowPort` already asks the configuration snapshot and `portNotAllowed` builds the error with the value as sent;
  `parsePortParams` already holds the grammar of both parameters and already removes them from the query;
  the audit `port` field and its `invalid_parameter` and `port_not_allowed` cases already exist;
  `testConfig` in `internal/httpapi/fixtures_test.go` sets no `Discovery.Pprof`,
  and the port cases in `server_test.go` that name a value beyond the default rely on the old empty-list reading;
  `errorBody` in `internal/httpapi/errors.go` is two strings, `fail` writes it through `WriteError`,
  and `internal/ui/ui.go` calls `ErrorEnvelope` for the console's own errors;
  `pprofView` in `internal/httpapi/listing.go` is non-test code reading both removed fields;
  `deploy/deploy_test.go` and `deploy/chart_test.go` load the shipped and rendered ConfigMap through `config.Load`,
  and four chart subtests name the old fields;
  `scripts/check-repo.py` already walks `*.go` files with path exemptions, and `mise run check` runs it;
  `test/e2e/overlays/ports-gateway/configmap.yaml` today lists a narrow allowlist rather than an empty one;
  the default gateway's configuration is composed by `gatewayConfig` and applied by `configPatch`;
  the two authentication lanes build their gateways from that same function;
  the test application already listens on `:6061` as `pprof-alt` and counts hits per listen address;
  `internal/ui/static/` holds `app.js` and `urls.js` and no model module,
  `app.js` clears one port field in the handler of the other and serializes the three fields in `portChoice()`,
  and the scans in `scan_test.go`, `ui_test.go`, and `vendor_test.go` enumerate the files they cover;
  `go.mod` does not yet require `github.com/dop251/goja`;
  fuda applies an `env` tag on presence, empty value included,
  and converts a string into a slice as one CSV record, which cannot yield an empty list;
  `gopkg.in/yaml.v3` decodes a `null` value into a pointer field as the nil pointer,
  which is why the removed-key probe decodes the `discovery.pprof` mapping into a map and looks the names up as keys,
  where a `null` value is still a present key;
  `docs/deployment.md` names the old lists in three places:
  the pprof-port bullet, the invariant paragraph, and the NetworkPolicy sentence.
- Decided here because the spec leaves it to the implementer:
  `Selection` is a two-field record — the kind and the value, with `"*"` as the value of a wildcard —
  rather than four fields that could contradict each other;
  the environment variable is read by `Load` rather than by an `env` tag, for the fuda reason above;
  the removed file keys are detected by decoding the `discovery.pprof` mapping into a map before the strict pass,
  so a key a YAML merge carries in is seen,
  and the removed variables by name before the file is read;
  the list rules run after the environment override, on the list that will be used;
  `details` is a nil slice under `omitempty` rather than a pointer or a second envelope type;
  the removed-name scan lives in `scripts/check-repo.py` because that is where the tree-wide greps already are;
  the two browser functions are named `deriveControl` and `applyInput`,
  and the second carries the edit so that clearing is a tested transition.
- Left to the implementer by design: helper names inside test files,
  the exact fixture file names under `internal/config/testdata/`,
  the wording of the console's labels,
  and the exact `applyInput` source names beyond the three listed.
