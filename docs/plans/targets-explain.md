# Target Exclusion Diagnostics

**Status:** Done
**Outcome:** `9e40fcd`..`5ed3dcf` and this commit.

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Every task that lands Go code is test-first,
> and each of those begins by adding the declarations its tests name — types, method stubs, generalized test helpers —
> so its first run fails on assertions rather than on compilation.
> The one task whose subject a test cannot execute says so in its own words.
> Checkboxes (`- [ ]`) track progress.
> The accepted spec outranks this plan:
> where the two differ the spec wins and the plan is the bug.

**Goal:** make an empty targets listing say why.
`GET /v1/namespaces/{ns}/services/{svc}/targets?explain=true` keeps the listing it already returns
and adds `selectorMatched` — the Pods the Service's selector matches — and `excluded`,
one entry per exclusion reason with a non-zero count, in a closed vocabulary the gateway writes
([`docs/specs/gateway.md`](../specs/gateway.md) *List targets*).
The console turns that into its empty state,
and `profgate targets --explain` prints the same rows beside the list.
The endpoint also gains the `version` and `pod` filters it does not accept today.

**Architecture:** one new method on the Kubernetes seam,
and three accepted query parameters on the targets endpoint —
the diagnostic `explain` and the two filters `version` and `pod`, none of which it takes today.
`internal/k8s` gains `Explain`.
It reports the targets of a Service beside the reasons its other selected Pods were dropped,
from one pass over one captured Pod list
([`docs/specs/gateway.md`](../specs/gateway.md) *The seam*).
Eight of the ten reasons are decided there, from the informer caches;
the two a request produces by filtering, `version_mismatch` and `pod_name_mismatch`,
are decided in `internal/httpapi`, which applies `version` and then `pod` to what `Explain` returned.
`internal/ui/static` gains `targetmodel.js`, three pure functions the Go tests execute:
the query the page sends, the rule that repeats a refused fetch without `explain`,
and the summary that becomes the Pod menu, the version menu, and the empty state's rows
([`docs/specs/ui.md`](../specs/ui.md) *Targets, with reasons*, *Controls*).
`internal/client` and `cmd/profgate` gain the two response fields and the `--explain` flag.
`internal/pgo` is untouched but for its test fake, which implements the seam's interface.
Untouched entirely:
`internal/auth`, `internal/natskv`, `internal/proxy`, `internal/admit`, and `deploy/`.

**Tech Stack:** everything already pinned in [`mise.toml`](../../mise.toml).
**No Go module is added.**
`internal/ui/portmodel_test.go` already evaluates `internal/ui/static/portmodel.js` under `github.com/dop251/goja`,
and evaluates the new module the same way.

**Spec:** [`docs/specs/gateway.md`](../specs/gateway.md), `Accepted`, is the design of record:
the seam, the reason vocabulary, the parameter grammar, the response, the audit field, and the tests.
[`docs/specs/ui.md`](../specs/ui.md), `Accepted`, holds the console's fetch, its one retry,
its wording table, and `targetmodel.js`.
Two of the three carry text this plan repairs before it implements anything;
[`docs/specs/cli.md`](../specs/cli.md), `Accepted`, carries a third gap.
*The accepted specs are repaired* is the task that closes all three, and it runs first.
Sections are cited by heading name, never by number;
an unqualified heading is the gateway spec's.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- **The permission invariant does not move.**
  `Explain` reads the Pod, Service, and EndpointSlice informer caches and issues no request (*The seam*),
  so the seven read tuples stay seven and no ClusterRole verb, resource, or API group changes.
  Two tests stay green and untouched by every task:
  `TestClusterRoleTuples` in `deploy/deploy_test.go`,
  and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go`.
  The invariant wording in `AGENTS.md`, `README.md`,
  [`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md),
  and the gateway spec's *Permission Boundary* needs no edit.
- **The seam grows by exactly one method.**
  `Discovery` in `internal/k8s/discovery.go:42-54` has four methods today;
  it ends this plan with five.
  That interface is the capability set a reviewer reads
  ([`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) *What Each One Actually Catches*),
  so no task adds a second method, a `GetPod`, or a lister accessor.
  `check_clientgo_importers` in `scripts/check-repo.py:121` keeps passing unchanged:
  the namespace-wide Pod read lives inside `internal/k8s`,
  because reading a lister means importing client-go.
- **Two test fakes implement `Discovery`, and the method lands in both at once.**
  Two files each define a `fakeDiscovery` with all four methods —
  `internal/httpapi/fixtures_test.go:92-195` and `internal/pgo/fixtures_test.go:1582-1635` —
  the second assigned to a `k8s.Discovery` field at `internal/pgo/fixtures_test.go:1736`.
  A fifth method added to the interface without both leaves the tree uncompilable,
  so the task that adds it edits both files.
- **`Explain` returns its list and its counts from one pass, and `Targets` pays nothing for them.**
  A caller that read the list and the counts separately could read a cache that moved between them,
  and a Pod would be counted twice or not at all (*The seam*).
  What the two methods share is the evaluation of the eligibility rules, not the reading of the caches.
  The namespace-wide Pod read is `Explain`'s alone:
  *The seam* calls it "one namespace-wide read of the Pod cache that `Targets` does not pay"
  (`docs/specs/gateway.md:397`),
  and `Targets` in `internal/k8s/eligibility.go:21` keeps reaching Pods one endpoint at a time,
  through the `c.pods.Pods(ns).Get` at `eligibility.go:111`.
  Its observable answer does not change either,
  which the existing `eligibility_test.go` cases prove by staying green.
- **No caller-visible answer names an excluded Pod.**
  `excluded` carries a reason from the gateway's own vocabulary and a number, and nothing else
  (*List targets*, *Non-disclosure*).
  No task adds a Pod name, an address, a node, a version, or a message to an exclusion entry,
  and no task adds one to a response header or an audit record either.
  The gateway's operator log is a different surface:
  `internal/k8s/eligibility.go:68` already names a Pod there when its endpoints disagree on its address,
  and *The conflict rule needs the spec repaired, and it moves one log line* says what happens to that line.
  The realm step precedes discovery, `explain` included (`internal/httpapi/server.go:470-475`),
  so a caller the realm denies learns none of it.
- **`explain` adds no metrics label.**
  It would double every `targets` series to record a parameter the audit line already carries (*Metrics*).
  `labels()` in `internal/httpapi/server.go:283-320` is untouched by every task.
- **Every deadline stays where it is.**
  Nothing in this plan sleeps, polls, or takes a timeout;
  `Explain` reads caches and cannot block, the way `Targets` and `Catalog` already cannot
  (`internal/k8s/eligibility.go:18-20`, `internal/k8s/catalog.go:22`).
- No jargon: code comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a review round, or a task name.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on every Markdown file a task writes or edits.
- Commit headers are Conventional Commits under 50 characters, with no trailer of any kind.

---

## Three Things This Plan Decides

### The command-line spec does not define `--explain`

[`docs/plans/roadmap.md`](roadmap.md) asks that the command line print the reasons.
The gateway spec's amendment table says
`docs/specs/cli.md` *`targets`* gains "`--explain` sends `explain=true` and prints the `excluded` rows beside the list",
adding that the document "is `Draft` and its text is not edited by this change".
Both halves of that row are now stale:
`docs/specs/cli.md:3` reads `**Status:** Accepted`,
and `git log` shows it was accepted in `184d8ed` — after `f1f4964`, the change that wrote the row —
with no `explain` anywhere in it.
`grep -n explain docs/specs/cli.md` prints nothing.

An accepted spec is the implementation source
([`000-agent-contract.md`](../../.agents/rules/000-agent-contract.md) *Document Authority*),
so the flag is written into the spec before it is written in Go.
It adds no design: *The accepted specs are repaired* writes the sentence the gateway spec already dictates,
and repairs the stale row in the same commit.

### The conflict rule needs the spec repaired, and it moves one log line

*Eligibility* gives two readings of the `endpoint_address_conflict` population.
`docs/specs/gateway.md:525` defines a trusted endpoint as **eligible** when rules 5, 7, **and 8** hold,
and `:529-531` describes the conflict among a Pod's eligible trusted endpoints.
`docs/specs/gateway.md:544` says the opposite in the same section:
two trusted endpoints "that each satisfy rules 5 and 7 name it with different addresses it holds,
**whether or not a pprof port resolves**".

The table row is the reading this plan takes, for three reasons.
The clause is explicit and would mean nothing under the other reading.
The row sits above `port_name_not_declared` in the attribution order,
which only matters for a Pod that is both conflicted and portless — the case the two readings disagree about.
And the aggregation rule at `docs/specs/gateway.md:497-499` validates each entry against rules 2–7 before deduplicating,
which is rule 8 left out of the conflict step by the section's own procedure.

**A Draft plan cannot resolve a contradiction inside an Accepted spec by choosing one side quietly.**
*The accepted specs are repaired* amends `docs/specs/gateway.md` *Eligibility* so the two agree,
and it runs before the code task that relies on the reading.

**One operator log line moves.**
`internal/k8s/eligibility.go:53-68` detects a conflict only among endpoints that already passed `eligible`,
which includes the port rule at `:136-139`.
Evaluating the conflict without rule 8 therefore makes a portless conflicted Pod log the warning at `:68`,
which names a Pod, where it logs nothing today.
That is the internal operator log, not a caller-visible surface:
no response, header, or audit record gains a Pod name from it.
The warning follows the attribution rather than the old population,
because a line that fires for some conflicts and not others is a rule nobody will remember;
the task that lands the reading asserts the line fires once for a portless conflict
and that nothing a caller can read mentions the Pod.

`Targets` answers identically under both readings,
because a Pod with no resolving port is not a target and a conflicted Pod is not a target.

### The console's retry needs the envelope code, which `request` discards

`request` in `internal/ui/static/app.js:276` resolves to the body on `200` and `null` otherwise,
recording `{error, retry}` under its key,
so `loadTargets` at `app.js:353-366` never sees the status or the envelope `code`.
Those two are what the retry rule of [`docs/specs/ui.md`](../specs/ui.md) *Targets, with reasons* keys on.

`request` gains one optional parameter, `retryOnce`.
When the first attempt is not a `200`, `request` calls `retryOnce(status, code)`;
when it returns a URL, `request` issues exactly one more `fetchJSON` and treats that result as the answer —
its `200`, its `401` handling, its `not_ready` retry, and its error recording all as they are today.
`loadTargets` is the only caller that passes it.

That shape keeps the mode handling, the sign-in path, and the error recording in one place,
keeps the rule pure and testable in `targetmodel.js`,
and gives the spec's clauses for free:
the hook is consulted once, so a second failure is not retried,
and a `400 invalid_parameter` a current gateway earned on the port selection is retried and fails the same way.

---

## File Structure

```text
docs/specs/gateway.md                # Eligibility's conflict sentence; two amendment rows
docs/specs/cli.md                    # targets --explain
internal/k8s/discovery.go            # Exclusion, Explanation, the vocabulary accessor, Explain on Discovery
internal/k8s/eligibility.go          # the shared resolve pass, per-Pod attribution, Explain
internal/k8s/eligibility_test.go     # one case per cache-derived reason, order independence, the sum
internal/pgo/fixtures_test.go        # its fakeDiscovery gains Explain
internal/httpapi/profile.go          # parseTargetsParams reads the whole grammar in name order
internal/httpapi/targets.go          # the shared view step, the plain body, the explain body
internal/httpapi/server.go           # the parameter dispatch, serveTargets, the shared error mapping
internal/httpapi/audit.go            # explain on the interactive record
internal/httpapi/fixtures_test.go    # its fakeDiscovery gains Explain and records its calls
internal/httpapi/targets_test.go     # the existing view cases, plus the grammar and the bodies
internal/ui/static/targetmodel.js    # the query, the retry rule, the target summary
internal/ui/static/urls.js           # targetsURL takes the whole query
internal/ui/static/app.js            # the targets fetch, the one retry, the empty state
internal/ui/targetmodel_test.go      # the three functions under the interpreter
internal/ui/portmodel_test.go        # the loader and cutExport take the module name
internal/ui/scan_test.go             # consoleSources gains the module; the ten reason names
internal/ui/vendor_test.go           # the import-free model list gains the module
internal/ui/ui_test.go               # the served-asset table and the tree-hash mutation table
internal/client/wire.go              # TargetsResponse gains selectorMatched and excluded
cmd/profgate/read.go                 # targets --explain and the reason rows
cmd/profgate/read_test.go
test/e2e/scenarios_test.go           # the three scenarios that gain an explain assertion
docs/api.md, docs/cli.md, docs/console.md
CHANGELOG.md
docs/plans/targets-explain.md
```

---

## The accepted specs are repaired

**Files:**
- Modify: `docs/specs/gateway.md`, `docs/specs/cli.md`

Three gaps in accepted text stand between this plan and its first line of Go.
None is a design change: each writes down what the accepted design already decided elsewhere.
This task is first because two of the three are the source the next tasks implement from.

- [x] **Amend the conflict sentence**

`docs/specs/gateway.md:525` and `:529-531` describe the conflict among **eligible** trusted endpoints,
where eligible includes the port rule;
`:544` says it holds whether or not a port resolves.
Rewrite the first so it agrees with the table:
a Pod is a target when it has at least one eligible trusted endpoint,
and the conflict is decided over its trusted endpoints satisfying rules 5 and 7,
so a Pod whose slices disagree about its address is attributed to `endpoint_address_conflict`
whether or not a pprof port resolves for it.
Say in the same place that this changes no target list —
a portless Pod and a conflicted Pod are both excluded either way —
and that it decides which reason a Pod satisfying both carries.

Leave `:497-499`, `:544`, and the attribution order as they are; they already say this.

- [x] **Write `--explain` into the command-line spec**

*Reading* gives `targets` its `--port` and `--port-name` flags at `docs/specs/cli.md:696-697`.
Add `--explain`: it sends `explain=true` and prints the `excluded` rows beside the list,
the example at `:677-680` gains those rows,
and `--output json` copies the body through as it does for every other reading verb.
The flag is the whole addition; the verb's request count and its positional grammar do not change.

- [x] **Repair the two amendment rows**

| Row | Change |
|---|---|
| the row naming `docs/specs/cli.md` *`targets`* under "Reads this endpoint and is revised on its own" | moves into the table of edits made now, because the revision is in this commit; the clause calling that document a draft goes with it |
| the row naming `docs/specs/gateway.md` *Eligibility* | gains the conflict sentence's repair, so the amendment block records what this commit did to the section it names |

- [x] **Validate and commit**

Both files keep their `Accepted` status and gain no `Outcome:` line;
`check_status` in `scripts/check-repo.py:77` verifies line 3 of each.

```bash
semlf check docs/specs/gateway.md docs/specs/cli.md
mise run lint && mise run test && mise run check
git add docs/specs/
git commit -m "docs(spec): settle the exclusion contract"
```

---

## `Explain` on the Kubernetes seam

**Files:**
- Modify: `internal/k8s/discovery.go`, `internal/k8s/eligibility.go`, `internal/k8s/eligibility_test.go`,
  `internal/k8s/export_test.go`,
  `internal/httpapi/fixtures_test.go`, `internal/pgo/fixtures_test.go`

*The seam* adds one method, two types, and one namespace-wide read of the Pod cache.
*Eligibility* defines the counted population, the trusted endpoint, the closed vocabulary, and the attribution rule.

**Produces:**

```go
// Exclusion counts the Pods one reason kept out of a Service's target list.
type Exclusion struct {
    Reason string // one of ExclusionReasons
    Count  int
}

// Explanation is what Explain reports about one Service.
type Explanation struct {
    Targets         []Target    // what Targets returns for the same arguments
    SelectorMatched int         // Pods in the namespace whose labels match spec.selector
    Excluded        []Exclusion // the reasons with a non-zero count, in vocabulary order
}

// ExclusionReasons returns the exclusion vocabulary in the order a report
// carries it. The first eight are decided here, from the caches; the last two,
// version_mismatch and pod_name_mismatch, are decided by the request's filters
// and are never produced by Explain.
func ExclusionReasons() []string

// Explain returns the targets of a Service beside the reasons its other selected Pods were dropped,
// from one captured list of the namespace's selected Pods and the EndpointSlice pass Targets makes.
// It counts Pods and names none.
func (c *Cluster) Explain(ctx context.Context, namespace, service string, port PortSelection) (Explanation, error)
```

`ExclusionReasons` is an unexported array of constants behind an accessor returning a copy,
the shape [`200-coding-standards.md`](../../.agents/rules/200-coding-standards.md) *Construction and state* allows.
`internal/httpapi` reads it to order the two counts it adds,
so the vocabulary and its order live in one place.

**One shared evaluation, two ways of reaching Pods.**
`Targets` at `internal/k8s/eligibility.go:21` and `Explain` share the endpoint pass and nothing else.

```go
// podFor resolves an endpoint's targetRef.name to the cached Pod behind it.
// Targets passes the lister's per-name read; Explain passes a lookup into the
// one selector-matched list it captured, so it reads the Pod cache once.
type podFor func(name string) (*corev1.Pod, bool)

// resolve walks the Service's slices of the read address family and evaluates
// the eligibility rules once: the targets it yields, and, per Pod a trusted
// endpoint named, what its endpoints said about it.
func (c *Cluster) resolve(svc *corev1.Service, selector labels.Selector,
    epSlices []*discoveryv1.EndpointSlice, sel PortSelection, lookup podFor) (targets []Target, seen map[string]endpointFacts)
```

`Targets` reads the Service and its slices as it does today (`eligibility.go:22-39`),
calls `resolve` with `c.pods.Pods(ns).Get` wrapped as the lookup —
one cache read per endpoint, exactly what `eligible` performs at `eligibility.go:111` now —
and returns the targets, discarding the facts.
**It performs no namespace-wide Pod read**, because *The seam* says the endpoint that asked for counts pays for them.

`Explain` reads the Service and its slices the same way,
then lists the namespace's Pods under `spec.selector` **once** and indexes them by name,
and calls `resolve` with a lookup into that captured map rather than the lister,
so the population it counts and the Pods it resolves endpoints against are the same snapshot
(`docs/specs/gateway.md:402-404`), and it reads the Pod cache once rather than once per endpoint.
It then attributes: `SelectorMatched` is the length of that list,
every Pod in it that `resolve` did not yield as a target takes the first reason in the vocabulary that holds for it,
from the Pod's own object or from the facts its trusted endpoints produced.

The two lookups agree because `resolve` applies rule 4 either way,
as `eligible` does at `eligibility.go:119`:
a Pod absent from the captured map is a Pod the selector does not match,
which is the same Pod the lister path drops on that rule.
Over one cache instant the two therefore yield the same targets, which one test asserts directly.

Sharing the evaluation is what keeps the sum invariant true.
Every target `resolve` yields passed rule 4 and is therefore in the population `Explain` counted,
and every other Pod of that population carries exactly one reason,
so the target count plus the counts is the list's length.
`Explain` deriving eligibility on its own would drift from `Targets` silently,
and no test would catch a drift the two computed the same way.

`eligible` at `eligibility.go:106` is per-endpoint and returns a `bool`,
which cannot express `endpoint_missing`:
that reason belongs to a Pod no trusted endpoint names, and the endpoint loop never visits such a Pod.
Attribution is therefore per Pod, over that Pod's trusted endpoints as a group (*Eligibility*),
with the selected-Pod list as the population — a population only `Explain` has.
A Pod the selector does not match is counted nowhere, whatever a slice says about it.

**The conflict and its warning.**
The conflict is decided over trusted endpoints satisfying rules 5 and 7, without the port rule,
the reading the previous task wrote into *Eligibility*.
The warning at `eligibility.go:68` follows the attribution:
a Pod attributed `endpoint_address_conflict` logs it once, portless or not.
It is the operator log and names the namespace, Service, and Pod, as it does today;
nothing a caller reads gains a name from it.

- [x] **Add the compile seams**

Declare `Exclusion`, `Explanation`, and `ExclusionReasons` in `internal/k8s/discovery.go`,
add `Explain` to the `Discovery` interface there with the doc comment *The seam* gives,
and add a `Cluster.Explain` that returns the zero `Explanation` and a nil error.
Add the counting `PodLister` and its swap helper to `internal/k8s/export_test.go`,
so the cache-read cases compile before they assert anything.
Add the same method, with the same zero answer, to both fakes:
`internal/httpapi/fixtures_test.go:92-195` and `internal/pgo/fixtures_test.go:1582-1635`,
the second of which is assigned to a `k8s.Discovery` field at `internal/pgo/fixtures_test.go:1736`.

`mise exec -- go build ./... && mise exec -- go vet ./...` passes at the end of this step.
Every assertion the next step writes then fails on content rather than on a missing declaration,
which is what [`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
*Test plans compile against current source* asks for.

- [x] **Write the seam tests**

`eligibility_test.go` restates *Eligibility* and the target-exclusion bullet of *Layers*,
one subtest per row against a fresh fixture from `startFixture` in `internal/k8s/export_test.go:24`:

| Fixture | Expect |
|---|---|
| a Pod with `deletionTimestamp` set, also unready | `pod_terminating`, count 1, and no other reason |
| a Pod whose `status.phase` is not `Running` | `pod_not_running` |
| a Pod whose `Ready` condition is not `True`, whose endpoint also carries `ready: false` | `pod_not_ready`, the first reason it satisfies |
| a selected Pod no slice entry names | `endpoint_missing` |
| the same, for a `nil` `targetRef` | `endpoint_missing` |
| the same, for a `targetRef` of another kind | `endpoint_missing` |
| the same, for a `targetRef` naming another namespace | `endpoint_missing` |
| the same, for a `targetRef.name` no Pod in the cache carries | `endpoint_missing` |
| the same, for a `targetRef.uid` a recreated Pod no longer holds | `endpoint_missing` |
| a trusted endpoint carrying `conditions.ready: false` | `endpoint_not_ready` |
| a trusted endpoint with no address | `endpoint_address_mismatch` |
| a trusted endpoint whose first address is not in `status.podIPs` | `endpoint_address_mismatch` |
| one Pod holding two addresses of the read family, named by two rule-5-and-7 entries | `endpoint_address_conflict` |
| the same Pod declaring no container port of the effective name | `endpoint_address_conflict`, never `port_name_not_declared` |
| a Pod with no container port of the configured default name | `port_name_not_declared` |
| a Pod declaring that name over UDP | `port_name_not_declared` |
| a Pod with no container port of the request's `portName` | `port_name_not_declared` |
| a Service whose selector matches no Pod | `SelectorMatched` is `0` and `Excluded` is empty |
| a Pod the selector does not match, present in a slice | counted under no reason, and absent from `SelectorMatched` |
| a Service the cache lacks | `ErrServiceNotFound`, as for `Targets` |
| a Service with no selector | `ErrServiceSelectorless`, as for `Targets` |
| a Service whose Pods are all targets | `Excluded` is empty and `SelectorMatched` equals the target count |

Order and agreement, over a multi-Pod fixture holding several reasons at once:

| Case | Expect |
|---|---|
| reasons inserted in an order the vocabulary does not use | `Excluded` comes back in the vocabulary's order |
| the EndpointSlice list reversed | every count and every attribution unchanged |
| the endpoints within a slice reversed | the same |
| the Pod list reversed | the same |
| two `Explain` calls over identical cached objects | identical `Excluded` slices |
| every fixture above | `len(Targets)` plus the sum of the counts equals `SelectorMatched` |
| every existing eligibility rejection fixture | the attribution its rejection earns, and the sum invariant |
| `Explain` and `Targets` on the same fixture and port selection | equal target sets, though one reached its Pods through the lister and the other through its captured list |

The last row of the first table and the last three of the second are what make the refactor safe:
a rule tested for exclusion is tested for its explanation in the same place.

**Which method reads what**, over a counting Pod lister.
`export_test.go` gains a `corelisters.PodLister` wrapping the informer's,
which counts namespace-wide `List` calls and per-name `Get` calls,
and a helper that swaps it onto a started `Cluster`'s `pods` field (`internal/k8s/cluster.go:28`).
Without it the cost split *The seam* states is a code shape nobody can fail:

| Call, over a Service with three trusted endpoints | Expect |
|---|---|
| `Targets` | zero namespace-wide `List` calls, and one `Get` per endpoint |
| `Explain` | exactly one namespace-wide `List` call, and zero `Get` calls |
| `Explain` over a namespace holding Pods of other Services | the `List` is filtered by the Service's selector, so those Pods are absent from `SelectorMatched` |

The warning and the seam's own contract, over a logger the fixture captures:

| Case | Expect |
|---|---|
| a conflicted Pod whose port resolves | one warning line naming the namespace, Service, and Pod, as today |
| a conflicted Pod declaring no port of the effective name | the same one line, which is the line this reading moves |
| a Pod excluded for any other reason | no warning line |
| a Pod lister whose namespace-wide read fails | `Explain` returns an error, never an empty count |
| a Pod lister whose namespace-wide read fails | `Targets` answers as it always did, because it never makes that read |
| every case above | the recording transport sees no request while any of it runs |

- [x] **Run the tests and watch them fail**

- [x] **Implement the endpoint pass and `Explain`**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/k8s/ ./internal/pgo/ ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/k8s/ internal/pgo/ internal/httpapi/
git commit -m "feat(k8s): explain why a Pod is not a target"
```

---

## The targets endpoint takes `version`, `pod`, and `explain`

**Files:**
- Modify: `internal/httpapi/profile.go`, `internal/httpapi/targets.go`, `internal/httpapi/server.go`,
  `internal/httpapi/audit.go`, `internal/httpapi/fixtures_test.go`, `internal/httpapi/targets_test.go`

The endpoint accepts the port selection and nothing else today:
`parseTargetsParams` at `internal/httpapi/profile.go:108-118` refuses every other name,
and `serveTargets` at `internal/httpapi/server.go:577-587` hands what `Targets` returned straight to `writeTargets`.
This task adds the three parameters of *List targets* and the filter step of *Request algorithm*.

**Produces:**

```go
// targetsParams is a validated targets query: the port selection, the two
// filters, and whether the caller asked for the exclusion counts.
type targetsParams struct {
    port    portParams
    version string
    pod     string
    explain bool
}

// exclusionView is one entry of the excluded array: a reason from the
// gateway's own vocabulary and a count, and nothing about any Pod.
type exclusionView struct {
    Reason string `json:"reason"`
    Count  int    `json:"count"`
}

// explainBody is the targets response with the counts: the same targets the
// plain body carries, plus the two fields explain=true adds.
type explainBody struct {
    Namespace       string          `json:"namespace"`
    Service         string          `json:"service"`
    Targets         []targetView    `json:"targets"`
    SelectorMatched int             `json:"selectorMatched"`
    Excluded        []exclusionView `json:"excluded"`
}

// targetViews converts targets to their views and sorts them by Pod name.
// Both bodies build their targets through it, so the two can never disagree
// on the view or its order.
func targetViews(targets []k8s.Target) []targetView

// discoveryError maps a Discovery error to its response: the two sentinels and
// the 503 everything else earns. Targets and Explain share it.
func discoveryError(rt route, err error) *requestError
```

`writeTargets` at `internal/httpapi/targets.go:26-38` converts, sorts, and writes in one function today.
It keeps its name and its signature and calls `targetViews`;
a second writer beside it encodes `explainBody`.
`TestWriteTargets` at `internal/httpapi/targets_test.go:11` covers the sort, the address exclusion,
the empty-array encoding, and the caller's slice being left alone;
those cases stay and are what proves the split changed nothing.

**One ordered pass over the whole grammar.**
*List targets* requires that parameters be validated in name order,
so a query with several faults reports the same one every time (`docs/specs/gateway.md:838-843`).
`parseTargetsParams` today calls `parsePortParams` first (`internal/httpapi/profile.go:109`),
which returns a port fault immediately (`profile.go:68-101`),
so `?explain=yes&port=bad` would report `port` where the spec asks for `explain`.

`parseTargetsParams` therefore stops calling `parsePortParams`
and walks `slices.Sorted(maps.Keys(values))` itself,
the shape `parseProfileParams` already uses at `profile.go:145-175`, over the five names it accepts:

| Name | Rule |
|---|---|
| `explain` | once with a value; `true` or `false` and nothing else |
| `pod` | once with a value; a DNS-1123 subdomain |
| `port` | once with a value; a decimal integer 1–65535 |
| `portName` | once with a value; a container-port name |
| `version` | once with a value; no further grammar |
| anything else | `400 invalid_parameter` naming it |

`port` and `portName` together is `400 invalid_parameter`,
checked **after** the loop, because it is a fault of the pair rather than of one name.
Every fault is `400 invalid_parameter`;
the first one in name order is the one returned.

`parsePortParams` stays exactly as it is for the profile endpoint, and `parseTargetsParams` no longer mutates `values`.

**The audit port is extracted separately.**
`server.go:505-508` records the selection as sent even when another parameter fails.
`parseTargetsParams` returns its `targetsParams` beside any error,
with `port` filled from the query's `port` and `portName` whenever that selection alone is well-formed,
whatever else in the query failed,
and empty when the selection itself is malformed, repeated, or doubled —
the rule `server.go:505-507` already states in its comment.

**The dispatch at the parameter step.**
`server.go:496-513` declares one `profileParams`,
fills its `port` field from `parseTargetsParams` on the targets branch,
and reads `params.port` at `:507-508` for both branches.
The targets branch stops borrowing `profileParams`:
it keeps a `targetsParams` of its own,
and both branches still set `q.port` and `q.audit.port` before the error check at `:509`.

**The handler takes the parameters.**
`serveTargets` at `server.go:526-530` and `:577-587` takes one more argument, the `targetsParams`,
rather than reading them off `request`:
`request` carries what the audit line and the metrics labels need (`server.go:268-278`),
and these three are neither.
`serveTargets` then:

1. calls `Explain` when `params.explain` is set and `Targets` otherwise (*Request algorithm*),
   mapping either error through `discoveryError`;
2. applies `version`, then `pod`, to the targets it got;
3. writes the plain body,
   or the explain body carrying the seam's counts plus one entry per target the filters dropped.

`discover` at `server.go:549-574` holds that mapping inline today;
the mapping moves into `discoveryError` and `discover` calls it,
so the profile path and both targets paths answer a missing Service, a selectorless one,
and an unreadable cache the same way.
`selectTarget` at `internal/httpapi/profile.go:248` is **not** reused:
it mints `404 pod_not_found` and `503 no_targets`, and this endpoint reports where that one selects.
A `pod` no eligible target carries is `200` with an empty array (*List targets*).

The two filter-derived counts go in the order `k8s.ExclusionReasons()` gives,
merged with the seam's slice rather than appended to it.
The filters narrow `targets` whether or not `explain` was sent;
without `explain` they add no field, because there is no field to add.

**The audit field.**
`auditRecord` in `internal/httpapi/audit.go:14-30` gains one field,
written into the interactive attribute list at `audit.go:50-59` only when it is set,
the way `auth_reason` is added at `audit.go:66-68`.
`pod` keeps the one meaning it has, the upstream Pod a profile request selected,
so a targets request writes it empty however its query filtered,
and `version` becomes a field of no record (*Logging*).
`labels()` at `server.go:283-320` is untouched: `explain` is not a metrics label (*Metrics*).

- [x] **Add the compile seams**

`fakeDiscovery` at `internal/httpapi/fixtures_test.go:92-195` gains a settable `k8s.Explanation`,
an error, a record of the port selection of every `Explain` call beside the one `Targets` keeps at line 110,
and a count of `Explain` calls, so a test can assert the method was not reached.
Declare `targetsParams`, `exclusionView`, `explainBody`, `targetViews`, and `discoveryError`,
and give `serveTargets` its new argument, before the assertions are written.

- [x] **Write the endpoint tests**

`targets_test.go` keeps `TestWriteTargets` and restates *List targets*
and the HTTP half of the target-exclusion bullet of *Layers*.

The grammar, in name order:

| Query | Answer |
|---|---|
| `explain=true` | `200` with `selectorMatched` and `excluded` |
| `explain=false` | `200` with today's body, byte for byte |
| no `explain` | the same |
| `explain=1`, `explain=TRUE`, `explain=yes` | `400 invalid_parameter`, one row each |
| `explain` repeated, and `explain=` | `400 invalid_parameter`, one row each |
| `pod=` repeated, `version=` repeated, `port=` repeated, `portName=` repeated | `400 invalid_parameter`, one row each |
| `pod=`, `version=`, `port=`, `portName=` each empty | `400 invalid_parameter`, one row each |
| `pod=` outside the DNS-1123 subdomain grammar | `400 invalid_parameter` |
| `port=0`, `port=65536`, `port=+1`, `port=6060 ` | `400 invalid_parameter`, one row each |
| `explain=yes&port=bad` | the `explain` fault, because `explain` sorts first |
| `pod=BAD&version=` | the `pod` fault |
| `port=bad&version=` | the `port` fault |
| `portName=BAD&version=` | the `portName` fault |
| `unknown=1&version=` | the `unknown` fault, unknown names taking their place in the same order |
| every row above, run twice with the query terms in the other order | the same code and message both times |
| `port=6061&portName=pprof` | `400 invalid_parameter` for the pair, checked after the name loop |
| `port=6061&portName=pprof&explain=yes` | the `explain` fault, which sorts before either |
| `version=1.42.3&pod=checkout-7c8f8c9b9-xabcd&explain=true&port=6061` | accepted, every value non-empty and inside its grammar |
| a value `allowedSelections` does not admit | `400 port_not_allowed` before discovery, the fake recording no call |

The audit port, over the rows that fail:

| Query | The line's `port` |
|---|---|
| `explain=yes&port=6061` | `6061`, because the selection itself is well-formed |
| `explain=yes&portName=pprof` | `pprof` |
| `explain=yes&port=bad` | empty |
| `explain=yes&port=6061&portName=pprof` | empty |
| a query with no port parameter | empty |

The bodies:

| Case | Body |
|---|---|
| `explain=true` over an explanation with counts | `targets` sorted by Pod name, `selectorMatched`, and `excluded` in vocabulary order |
| every Pod a target | `excluded` encodes as `[]` and never `null` |
| a selector matching no Pod | `selectorMatched` is `0` and `excluded` is `[]` |
| any explain body | no `serviceFound` field and no `cacheSynced` field |
| `version=` beside `explain=true` | `targets` narrowed, `version_mismatch` added in vocabulary order, the seam's counts unchanged |
| `pod=` beside `explain=true` | the same, under `pod_name_mismatch` |
| both filters beside `explain=true` | both rows added, each once, in vocabulary order |
| every combination above | `selectorMatched` equals `len(targets)` plus the sum of the counts |
| `pod=` naming no eligible target | `200` with an empty array, never `404 pod_not_found` |
| `version=` matching no target, with no `pod=` | `200` with an empty array, never `503 no_targets` |
| `version=` or `pod=` without `explain` | today's body, narrowed, with no added field |
| the plain body and the explain body over the same targets | the same `targets` array, which is what the shared view step buys |

Non-disclosure, asserted by scanning rather than by reading named fields.
The fixture's `Explanation` carries sentinels a leak would have to print:
targets whose `PodIP` and `Port` are set,
and one target the `pod=` filter drops whose name appears nowhere else:

| Scan | Expect |
|---|---|
| the raw response bytes | no sentinel Pod name, no `PodIP`, and no resolved port number |
| every response header name and value | the same |
| the captured audit output for the request | the same |
| the same three, for `explain=true`, `explain=false`, and no `explain` | the same, one row each |

A scan over bytes is what a typed assertion cannot do:
a field added to `targetView` or `exclusionView` later fails it, where reading known fields would not.

The step order and the record:

| Case | Answer |
|---|---|
| a realm that denies the namespace, with `explain=true` | `403 realm_denied`, the same body whether or not the Service exists, and the fake records that neither `Explain` nor `Targets` was called |
| readiness false, with `explain=true` | `503 not_ready`; no explain body is written |
| a fake whose `Explain` returns an unclassified error | `503 discovery_unavailable` |
| a fake whose `Explain` returns `ErrServiceNotFound` | `404 service_not_found` |
| a fake whose `Explain` returns `ErrServiceSelectorless` | `422 service_selectorless` |
| the same three errors from `Targets` on a request without `explain` | the same three answers, which is what the shared mapping buys |
| `explain=true` accepted | the audit line carries `explain` with the value `true` |
| `explain=false`, and no `explain` | the audit line carries no `explain` |
| `explain=notabool` | `400 invalid_parameter` and no `explain` on the line |
| a targets request carrying `pod=` | the audit line's `pod` is empty |
| any targets request | the audit line carries no `version` field |
| any targets request | one metrics row on the `targets` endpoint, with no label the parameter added |

- [x] **Run the tests and watch them fail**

- [x] **Implement the parser, the handler, and the bodies**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "feat(httpapi): serve targets?explain=true"
```

---

## The API guide

**Files:**
- Modify: `docs/api.md`

Two passages become false with the endpoint's new grammar, and both move with it.

- [x] **Update the request algorithm**

`docs/api.md:96-98` says a targets request runs steps 1 through 9 and answers from what discovery found,
never reaching single-target selection.
That is now half true: the request reaches step 10 to **filter**, and stops before anything selects one target.
Rewrite it in those terms:
a targets request runs steps 1 through 10, applying `version` and then `pod` at step 10 and choosing no target,
and never reaches admission, confirmation, or the proxy.
Step 10 at `docs/api.md:129-132` today reads "filters are applied and one target is chosen"
with `404 pod_not_found` and `503 no_targets` beside it;
say there that those two codes belong to the profile endpoint,
and that on the targets endpoint the same filters narrow a list and an empty result is `200` with an empty array.
The accepted algorithm this tracks is *Request algorithm*, whose filter step names both endpoints.

- [x] **Update the targets section**

`docs/api.md:146-147` says the endpoint takes the port selection "and no other" parameter.
*Listing targets* gains the parameter table — `port`, `portName`, `version`, `pod`, and `explain` —
an `explain=true` example body beside the plain one,
`selectorMatched` and `excluded` with what each means,
and the reason table with one line of prose per reason.
It states that `excluded` is `[]` and never `null`,
that `selectorMatched` equals the number of targets plus the sum of the counts,
that `version_mismatch` and `pod_name_mismatch` appear only for a request that sent the matching filter,
that parameters are validated in name order,
and that a `pod` no target carries is `200` with an empty array rather than `404 pod_not_found`.

The error table's `no_targets` row at `docs/api.md:838` is the profile endpoint's and stays as it is.

- [x] **Validate and commit**

```bash
semlf check docs/api.md
mise run lint && mise run test && mise run check
git add docs/api.md
git commit -m "docs(api): the targets exclusion counts"
```

---

## `targetmodel.js` and its tests

**Files:**
- Create: `internal/ui/static/targetmodel.js`, `internal/ui/targetmodel_test.go`
- Modify: `internal/ui/portmodel_test.go`, `internal/ui/scan_test.go`, `internal/ui/vendor_test.go`,
  `internal/ui/ui_test.go`

The module lands before the page that imports it,
so the interpreter test and the scans are what prove it before any DOM does.
[`docs/specs/ui.md`](../specs/ui.md) *Controls* and *Unit* define all three functions and every case.

**Produces**, in `internal/ui/static/targetmodel.js`, importing nothing
and ending in one `export { ... }` statement:

```js
// targetsQuery(port, withExplain) -> the query a targets fetch sends:
//   the port selection as portmodel.js produced it, plus explain: "true"
//   when withExplain is true and no explain key at all when it is false.
//   Never version, never pod. The retry is the same call with false, which is
//   how it drops only explain and preserves the port selection by construction.
// retryWithoutExplain(status, code, sentExplain) -> true when a fetch that
//   carried explain=true was refused 400 with the envelope code
//   invalid_parameter; false otherwise.
// targetSummary(body) -> { pods, versions, empty }:
//   pods is each targets[].pod in the order the response listed them,
//   versions the distinct non-empty targets[].version values in that order,
//   and empty is null when targets is non-empty, otherwise one of
//   { kind: "noSelector" } for a selectorMatched of 0,
//   { kind: "plain" } for a body with no excluded field or an empty one,
//   or { kind: "reasons", rows: [{ reason, count, text }] } in the order
//   the gateway sent, an unrecognized reason carrying its own name as text.
```

`targetsQuery` taking the flag rather than the caller deleting a key is what makes the retry provable:
the retry URL differs from the first by the `explain` key and by nothing else,
and a test drives both calls rather than trusting a deletion written at the call site.

The wording table of [`docs/specs/ui.md`](../specs/ui.md) *Controls* is a plain object in this module,
its ten keys the gateway's vocabulary and its values that section's sentences, copied exactly.
The wording is plural whatever the count is, so the module carries no grammar rule.

- [x] **Add the compile seams**

`cutExport` at `internal/ui/portmodel_test.go:29`, and `loadPortModel` at `:46`,
name `portmodel.js` through the `portModelName` constant at `:14`.
Give both the module name and the function names as parameters,
move them beside the other shared helpers,
and leave `loadPortModel` as a one-line call so the port-control cases are untouched.
`callModel` at `portmodel_test.go:71` already takes the runtime and needs no change.
Create `targetmodel.js` with the three functions returning empty values,
so the loader finds them and every assertion fails on content.

- [x] **Write the model tests**

`targetmodel_test.go` drives all three functions in one interpreter, table-driven, restating *Unit*.

The query, over each state the port control can be in:

| State | Query |
|---|---|
| `default`, with `withExplain` true | `explain=true` alone |
| a numeric selection | `port=` beside `explain=true` |
| a named selection | `portName=` beside `explain=true` |
| every state above with `withExplain` false | the same port parameter and no `explain` key |
| every state above | neither `version=` nor `pod=` is ever sent |

The retry rule:

| Case | Retried |
|---|---|
| `400` with `invalid_parameter`, on a fetch that carried `explain=true` | yes |
| the same, on a fetch that carried no `explain` | no |
| `400` with another code | no |
| `403`, `404`, `503` | no |
| a second failure, whatever its code | no, because the rule is consulted once per fetch |

The summary, over a response with targets:

| Case | Expect |
|---|---|
| several targets | the Pod menu holds each `targets[].pod` in the response's order |
| targets sharing versions, one with an empty version | the version menu holds the distinct non-empty values in that order |
| a non-empty `excluded` beside non-empty `targets` | `empty` is null |

The summary, over a response with no targets:

| Case | Expect |
|---|---|
| each `excluded` entry | one row of its count beside the wording of *Controls* |
| the ten reasons in the gateway's vocabulary order | rows in that order |
| the same ten shuffled | rows in the order they arrived, never re-sorted |
| a `reason` the table does not hold | a row carrying that name as its own text, dropped by nothing |
| `selectorMatched` of `0` | `noSelector` and no rows, whatever `excluded` holds |
| a body with no `excluded` field | `plain` and no rows |
| an `excluded` of `[]` | the same |
| a `count` of `1` and a `count` of `9` | the same plural wording |
| any call | the argument handed in is unchanged, as `callModel` reports |

The scans:

| Check | Change |
|---|---|
| `consoleSources()` at `scan_test.go:14` | gains `targetmodel.js`, so the HTML-interface scan covers it |
| `TestVendorImportFreeModels` at `vendor_test.go:305-306` | its list gains `targetmodel.js`: no import at all, static or dynamic |
| the shape assertion | `targetmodel.js` declares plain functions and ends in one `export` statement, as `portmodel.js` does |
| a new test of its own | `targetmodel.js` contains each of the ten reason names of the gateway's vocabulary as a literal, the ten written out in the Go test rather than derived from the page |
| the served-asset table in `TestAssets` at `ui_test.go:212-224` | gains a `targetmodel.js` row with the JavaScript content type, beside `portmodel.js` at `:220` |
| the tree-hash mutation table in `TestHashMoves` at `ui_test.go:73-113` | gains a case appending a byte to `targetmodel.js`, beside the `portmodel.js` case at `:91` |

A reason added to the gateway without console wording turns the suite red,
which is what writing the ten names out in Go buys.
The test that holds `app.js` to the module lands with the page, in the next task.

- [x] **Run the tests and watch them fail**

- [x] **Write the module**

`collectionmodel.js` is not created, and `consoleSources()` does not name it:
it belongs to a later item of [`docs/plans/roadmap.md`](roadmap.md).
[`docs/specs/ui.md`](../specs/ui.md) *Layout and embedding* draws a static tree holding it,
and that tree is the later item's end state rather than this one's.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/
git commit -m "feat(ui): the target summary model"
```

---

## The console asks for the reasons

**Files:**
- Modify: `internal/ui/static/urls.js`, `internal/ui/static/app.js`,
  `internal/ui/scan_test.go`

**`targetsURL` takes the whole query.**
`targetsURL(ns, svc, port)` lives at `internal/ui/static/urls.js:53-56`,
and turns its third argument into a query with `portParams` at `:32-43`.
Its third argument becomes the query object `targetsQuery` built, passed to `build` as it stands:

```js
// targetsURL lists the Pods of a Service; query is what targetmodel.js built.
export function targetsURL(ns, svc, query) {
  return build("/v1", ["namespaces", ns, "services", svc, "targets"], query);
}
```

A fourth argument was the alternative and is rejected:
the page would then hold the port selection in two places,
and `build` at `urls.js:14-28` already drops an empty value, so one object says everything.
`portParams` at `urls.js:30-43` has no other caller —
`profileURL` at `:86-95` spells `port` and `portName` itself — so it is deleted with this change.

**The page's state.**
`state.targets` stays the array it is:
it is initialized at `internal/ui/static/app.js:186`,
reset at `:430`, `:447`, and `:483`,
and read with `.length` at `:807`.
A second field, `state.targetSummary`, holds what `targetSummary` returned, or `null`.
Every place that sets `targets: []` also sets `targetSummary: null`,
which is the initial state and the three resets above, and the `!body` branch at `:361`.
`loadTargets` sets both on success.

```js
loadTargets = async () => {
  const { ns, svc } = this.state;
  const seq = ++this.seq;
  const port = this.portChoice();
  const retryOnce = (status, code) =>
    retryWithoutExplain(status, code, true) ? targetsURL(ns, svc, targetsQuery(port, false)) : null;
  const body = await this.request("targets", targetsURL(ns, svc, targetsQuery(port, true)), this.loadTargets, retryOnce);
  if (seq !== this.seq) {
    return;
  }
  if (!body) {
    this.setState({ targets: [], targetSummary: null });
    this.afterServiceError("targets");
    return;
  }
  this.setState({ targets: asList(body.targets), targetSummary: targetSummary(body) });
};
```

The retry URL drops `explain` and preserves the port selection because `targetsQuery` builds both,
which is the rule [`docs/specs/ui.md`](../specs/ui.md) *Targets, with reasons* states
and the model test drives directly.

**The empty state replaces the controls.**
`app.js:792-809` renders the Pod `<select>`, the version `<select>`, and then a message below them.
[`docs/specs/ui.md`](../specs/ui.md) *Controls* says the page "puts the reasons the response counted in their place",
so when `summary` is non-null and `summary.empty` is non-null
**the two `<select>` controls are not rendered at all** and the empty state stands where they were.
When `summary.empty` is null the two controls render,
the Pod menu from `summary.pods` and the version menu from `summary.versions`,
which is what stops the page deriving them itself at `:753` and `:796`.
Before the first fetch answers, `summary` is null: both controls render empty and disabled, as today.

The three empty states, in the place the controls occupied:

| `summary.empty.kind` | Rendered |
|---|---|
| `reasons` | one row per entry, its count beside its text, in the order the gateway sent |
| `noSelector` | the sentence saying the Service's selector matches no Pod |
| `plain` | today's "no target listed yet" |

Every value reaches the template as `${value}` in child position, as text, like every other response value
([`docs/specs/ui.md`](../specs/ui.md) *Rendering response values*),
which the HTML-interface scan over `app.js` in `scan_test.go` already enforces.
`app.js` sends no `version=` and no `pod=` on a targets fetch:
those two controls are filled from this response, and sending them back would narrow the choices it offers.

- [x] **Write what a Go test can hold**

**This task is not behaviorally test-first, and says so rather than implying otherwise.**
No test executes `app.js`
([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)),
so the fetch, the retry, the state transitions, and the rendering are proven by a person in a browser.
The browser-driven test layer that would prove them is a later item of [`docs/plans/roadmap.md`](roadmap.md).
What a Go test holds here is the source:

- a test beside `TestScanPageUsesPortModel` at `scan_test.go:107-122`, in its shape:
  `app.js` imports `targetsQuery`, `retryWithoutExplain`, and `targetSummary` from `./targetmodel.js`
  and calls each at least once;
- `urls.js` still passes the source scan at `scan_test.go:82-90`,
  which it must after `portParams` is deleted and `targetsURL` changes.

What no test proves, listed so it is checked by hand rather than assumed:
that exactly one retry is issued,
that the retry keeps the port selection,
that `targets` and `targetSummary` are reset together everywhere,
and that the two `<select>` controls disappear when the empty state appears.

- [x] **Run the tests and watch them fail**

- [x] **Wire the page**

- [x] **Check it in a browser**

Against a running gateway, with a Service whose Pods are all ineligible,
a Service whose selector matches nothing, and a Service with targets:

- the counted rows appear in the gateway's order with their wording;
- the selector sentence appears for `selectorMatched` of `0`;
- **the Pod and version controls are absent whenever an empty state is shown**, not merely empty;
- both controls return, populated, on switching to a Service with targets;
- the network panel shows `explain=true` on every targets fetch, and no `version=` or `pod=`;
- against a gateway that refuses `explain` — one built without this change —
  the panel shows exactly two targets requests, the second without `explain` and with the port selection intact,
  and the page renders the plain listing with no error;
- switching namespace and Service clears the rows rather than carrying them to the next selection.

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/
git commit -m "feat(ui): show why a Service has no target"
```

---

## `profgate targets --explain`

**Files:**
- Modify: `internal/client/wire.go`, `cmd/profgate/read.go`, `cmd/profgate/read_test.go`

**Produces:**

```go
// TargetsResponse is GET .../targets; the last two fields are present only
// for a request that sent explain=true.
type TargetsResponse struct {
    Targets         []Target    `json:"targets"`
    SelectorMatched int         `json:"selectorMatched"`
    Excluded        []Exclusion `json:"excluded"`
}

// Exclusion is one reason the gateway counted, and how many Pods it kept out.
type Exclusion struct {
    Reason string `json:"reason"`
    Count  int    `json:"count"`
}
```

`targetsVerb` at `cmd/profgate/read.go:178-219` gains `--explain`,
which sets `explain=true` on the query it builds at `read.go:196-203`.
The renderer at `read.go:205-215` prints today's `POD NODE VERSION` table,
then, when the flag was given, a blank line and a second table of `REASON COUNT` rows,
in the order the gateway sent them, through the same `writeTable`,
so a pipe into `cut` behaves and a terminal reads
([`docs/specs/cli.md`](../specs/cli.md) *Reading*).
An empty `excluded` prints its header and nothing else, like every other empty list.
`--output json` copies the body byte for byte and is unaffected, as it is for every reading verb.

The client decodes the reason as text and interprets none of it:
a reason a newer gateway added is printed as it arrived, never dropped.

- [x] **Write the verb tests**

`read_test.go` restates the `targets` rows of [`docs/specs/cli.md`](../specs/cli.md) *Reading*,
against an `httptest` gateway as the file's other verb cases already are:

| Case | Expect |
|---|---|
| no `--explain` | no `explain` parameter is sent, and the output is today's one table |
| `--explain` | `explain=true` is sent once |
| `--explain` with counted reasons | the second table's rows in the order the body listed them |
| `--explain` with `excluded` absent, and with `[]` | the second table's header and no rows |
| `--explain` with a reason outside the vocabulary | printed as it arrived |
| `--explain --output json` | the body byte for byte, and no table |
| `--explain --port 6061` | both parameters sent |
| `--explain` beside `--port` and `--port-name` together | the usage error at `read.go:189-191`, before any request |
| `--explain` against a gateway answering `400 invalid_parameter` | the envelope's message and exit 1, with no retry |

The last row is the deliberate difference from the console:
the client is a person's tool at a terminal, its `--explain` is an explicit request,
and silently answering a different question than the one asked is worse than a message.

- [x] **Run the tests and watch them fail**

- [x] **Implement the flag and the rows**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/client/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/client/ cmd/profgate/
git commit -m "feat(cli): targets --explain"
```

---

## The end-to-end scenarios

**Files:**
- Modify: `test/e2e/scenarios_test.go`

*What end-to-end proves* names three existing scenarios that gain an assertion.
No scenario is added and `test/e2e/registry.go` is untouched:
each of the three already provisions the cluster state its new assertion needs.

`targetsResponse` at `test/e2e/scenarios_test.go:78-82` gains `selectorMatched` and `excluded`,
and a helper beside `targetNames` at `scenarios_test.go:176` reads the endpoint with `explain=true`.

**The helper reads the raw bytes before it decodes them.**
`json.Unmarshal` into a typed struct ignores a field the struct does not name,
so decoding alone cannot notice a Pod name or an address that appeared in an exclusion entry.
The helper therefore takes the names and addresses the caller expects to be absent,
fails when any of them occurs in the response bytes,
and only then decodes and returns the counts.

- [x] **Extend the three scenarios**

| Scenario | Assertion |
|---|---|
| `scenarioIneligiblePods` at `scenarios_test.go:577` | after the readiness flip, `?explain=true` counts the NotReady Pod under `pod_not_ready`, the first reason it satisfies, and the raw body holds neither that Pod's name nor its IP |
| `scenarioErrors` at `scenarios_test.go:719` | the `?pod=` naming a Pod of another Service, sent to the targets endpoint, is `200` with an empty array, and with `?explain=true` beside it the Service's own eligible Pods are counted under `pod_name_mismatch` and named nowhere in the body |
| `scenarioVersionFilter` at `scenarios_test.go:835` | `?version=2.0.0` on the targets endpoint lists only the Pods carrying it, and with `?explain=true` the rest are counted under `version_mismatch` and named nowhere in the body |

**Both replicas are polled until each reports the `pod_not_ready` count.**
Until the Pod update reaches a replica that has already seen the endpoint update,
that replica reports `endpoint_not_ready`, which is the correct answer to the cache it holds (*What end-to-end proves*).
A single read of one replica would be flaky for a reason that looks like a bug in attribution.
`poll` with `eligibilityDeadline` at `scenarios_test.go:51` is the shape the file already uses for this.

`scenarioErrors` runs against the scoped gateway `deployScopedGateway` builds at `scenarios_test.go:722`,
which is the gateway that owns the realm those assertions rely on;
read that line rather than assuming which client each assertion takes.

- [x] **Run the suite on the current lane**

```bash
mise run test:e2e
```

- [x] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add test/e2e/
git commit -m "test(e2e): assert the exclusion counts"
```

---

## The guides, the changelog, and the plan's own status

**Files:**
- Modify: `docs/cli.md`, `docs/console.md`, `CHANGELOG.md`, `docs/plans/targets-explain.md`

- [x] **Update the guides**

| File | Change |
|---|---|
| `docs/cli.md` | the `targets` line at `docs/cli.md:206` and the paragraph at `:219` gain `--explain`, with an example showing both tables; the automation example at `:445` stays as it is, because `--output json` is unchanged |
| `docs/console.md` | the **Profile** bullet of *What it shows* at `docs/console.md:42-43`, where the Pod and version controls are described: when a Service has no target those two controls are replaced by the counted reasons in the gateway's order with their wording, a Service whose selector matches no Pod reads as its own sentence, and a fetch a mid-rollout replica refuses is retried once without the diagnostic |
| `CHANGELOG.md` | under `## [Unreleased]`, an `### Added` entry beside the ones already there |

The changelog entry names what an operator and a user each get:
`explain=true` on the targets endpoint, with `selectorMatched`, `excluded`, and the ten-reason vocabulary;
the `version` and `pod` filters the endpoint now accepts,
with the note that a `pod` no target carries is `200` with an empty array;
the audit field `explain`;
the console's empty state;
and `profgate targets --explain`.
It says plainly that no Kubernetes permission changed and no Go module was added.
Leave the released sections as they are: they describe what those versions shipped.

- [x] **Confirm the invariant wording**

Read `AGENTS.md`, `README.md`, and
[`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) beside each other:
this change adds no Kubernetes verb, resource, or API group and touches no NATS store,
so none of the three changes.
Run the two greps
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) *Two Mechanisms* holds,
and confirm `internal/k8s` is still the only non-test importer of client-go.
Confirm [`.agents/rules/100-project-map.md`](../../.agents/rules/100-project-map.md)
still describes the seam correctly with five methods on it, and revise the sentence if it counts them.

- [x] **Finish the plan in the same commit**

Set line 3 of this file to `**Status:** Done`;
insert `**Outcome:**` as line 4, naming the commits or the tag that shipped the change.
[`.agents/rules/900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
binds that flip to the change that lands the plan's remaining work,
and the next commit that touches this file deletes it and rewrites every link that cited it.

**No bullet of [`docs/plans/roadmap.md`](roadmap.md) is ticked by this plan.**
All three of the item's bullets are already ticked at `docs/plans/roadmap.md:102-107`,
and the document's own note at `:11-12` says a tick records that a revision is in the spec,
with the implementation and the code following it.
The third bullet is the one exception the reader should know about:
it was ticked for a command-line behavior no accepted spec described until this plan's first task,
and that task is what makes the tick true.

- [x] **Validate and commit**

```bash
semlf check docs/cli.md docs/console.md CHANGELOG.md docs/plans/targets-explain.md
mise run lint && mise run test && mise run check
git add docs/ CHANGELOG.md
git commit -m "docs: target exclusion diagnostics"
```

---

## Risks and What This Plan Does Not Cover

- **The Pod-cache read is new cost on a request that asks for it, and on no other.**
  `Explain` pays one namespace-wide read of the Pod cache that `Targets` does not,
  bounded by the number of Pods in the namespace (*The seam*).
  The two methods diverge at exactly that read:
  they share the evaluation of the eligibility rules and each reaches Pods its own way,
  `Targets` one endpoint at a time and `Explain` once for the namespace.
  A request without `explain` therefore pays nothing,
  and the console sends `explain=true` on every targets fetch,
  so a console open on a large namespace pays it on every port change.
  No cache, memo, or index is added:
  the read is a lister call over a synced informer, and measuring it is cheaper than guessing at it.
  A gateway where it turns out to matter is a revision of *The seam*, not a patch here.
- **The seam grows and stays greppable, but the grep does not read the new method's body.**
  `check_clientgo_importers` catches a package outside `internal/k8s` reaching for client-go,
  and the golden ClusterRole catches a manifest granting access nobody needs;
  neither catches a mutating call added inside the seam
  ([`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) *What Each One Actually Catches*).
  `Explain` is a lister read and nothing else, and the review of its diff is what holds that.
- **The conflict reading widens one operator log line.**
  Take a Pod whose slices disagree about its address and which declares no port of the effective name:
  it now logs the warning at `internal/k8s/eligibility.go:68`, naming it, where it logs nothing today.
  That is the operator's log rather than a caller's answer,
  and the alternative — a warning that fires for some conflicts and not others — is a rule nobody remembers.
  For every other conflict that log already names a Pod,
  and an operator reading it sees nothing of a new kind.
- **Two replicas may attribute the same Pod differently while a cache update is in flight.**
  A replica that has seen the endpoint update but not the Pod update reports `endpoint_not_ready`
  where the other reports `pod_not_ready`, and both are correct answers to the caches they hold
  (*Eligibility*, *What end-to-end proves*).
  A caller polling through a rollout sees the reason move.
  The end-to-end scenario polls both replicas until each converges, which is why it is written that way;
  nothing makes the two agree mid-update, and nothing should.
- **No test runs `app.js`, and the console task says so rather than implying a proof it has not got.**
  The three model functions run under the interpreter, the source scans hold the page's imports and its shape,
  and the wiring between them — one retry and no more, the retry keeping the port selection,
  the two state fields reset together, and the controls disappearing with the empty state —
  is proven by a person in a browser and by nothing else.
  Each of those four is named in that task's list of what no test proves,
  and each is a line of its browser checklist.
  The browser-driven test layer is a later item of [`docs/plans/roadmap.md`](roadmap.md).
- **The console's one silent retry costs a request against a current gateway.**
  A `400 invalid_parameter` earned on the port selection is retried without `explain` and fails the same way
  ([`docs/specs/ui.md`](../specs/ui.md) *Targets, with reasons*).
  That is the price of a rule with no second clause, and it is paid only on a request already failing.
- **`selectorMatched` discloses the size of a workload whose Pods never become eligible.**
  A crash-looping Service reports a count where the plain listing reports nothing (*Non-disclosure*).
  It names no Pod, no address, and no node, and the realm already admits profiling that Service;
  a realm that should not disclose a Service's size is a realm that should not admit the Service.
- **Out of scope, and named so no task drifts into it:**

  | Not in this plan | Where it belongs |
  |---|---|
  | `X-Request-Id`, accepted or generated, echoed and audited | a later item of [`docs/plans/roadmap.md`](roadmap.md); `auditRecord` gains no `requestId` field here, though *Logging* lists one |
  | a `details` array on any code but `port_not_allowed` | the same item |
  | `Idempotency-Key` on `POST .../collections` | the same item |
  | `GET /v1/collections/{id}?wait=` and `.../collections/latest` | the same item |
  | the Collection listing's `state`, `since`, `origin` filters and its cursor | the same item |
  | the OpenAPI document and the route check that reads it | the same item |
  | `collectionmodel.js` and the console's Collection start and cancel | a later item; the file is not created and `consoleSources()` does not name it |
  | the browser-driven test layer that executes `app.js` | the same item |
  | stable asset paths in place of the content-hashed tree | the same item |

---

## Self-Review

- Spec coverage:
  the conflict contradiction and the stale command-line row, closed before anything is implemented from them
  (*Eligibility*, *Amendments*, [`docs/specs/cli.md`](../specs/cli.md) *Reading*,
  in *The accepted specs are repaired*);
  the method, its two types, the one captured Pod list, the absent RBAC change, and the failed read answered `503`
  (*The seam*, in *`Explain` on the Kubernetes seam*);
  the trusted endpoint, the counted population, the closed vocabulary and its order,
  and the per-Pod rule that attributes each counted Pod to one reason
  (*Eligibility*, in the same task);
  the parameter step taking `version`, `pod`, and `explain` in name order,
  the discovery step calling `Explain`,
  and the filter step answering an empty array for a `pod` no target carries
  (*Request algorithm*, *List targets*,
  in *The targets endpoint takes `version`, `pod`, and `explain`*);
  the response, `selectorMatched`, `excluded`, the sum invariant,
  and where the two filter-derived reasons are counted
  (*List targets*, in the same task);
  the audit field added only for an accepted `explain=true`, and the fields the filters overload
  (*Logging*, in the same task);
  the absent metrics label (*Metrics*, in *Global Constraints* and the same task);
  the unit rows split by where each reason is decided, and the non-disclosure proof over body, headers, and audit
  (*Layers*, in the first two code tasks);
  the three end-to-end assertions and the Pod named nowhere outside `targets`
  (*What end-to-end proves*, in *The end-to-end scenarios*);
  the console's fetch, its one retry, and its counted empty state replacing the two controls
  ([`docs/specs/ui.md`](../specs/ui.md) *Targets, with reasons*, *Controls*,
  in *`targetmodel.js` and its tests* and *The console asks for the reasons*);
  the three pure functions, the shape assertion, the interpreter, and the pinned reason names
  ([`docs/specs/ui.md`](../specs/ui.md) *Unit*, *Layout and embedding*, in the same two tasks);
  the two documents *Amendments* updates with the implementation
  (`docs/api.md` and `docs/console.md`, in *The API guide* and the last task);
  the command line's flag ([`docs/specs/cli.md`](../specs/cli.md) *Reading*,
  in *`profgate targets --explain`*).
- Each task's stated tests are green before its commit against the tree that task leaves,
  and each code task adds the declarations its tests name before the assertions that use them,
  so a first run fails on content rather than on compilation
  ([`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
  *Test plans compile against current source*).
  The spec repairs land first because two of the three are what the next tasks implement from;
  the seam lands before the HTTP layer that calls it,
  and carries both `Discovery` fakes with it so the tree compiles at that commit;
  the HTTP layer lands before the API guide, whose current sentences the endpoint makes false;
  the model lands before the page that imports it;
  the end-to-end scenarios land last of the code tasks, because each asserts against the finished endpoint.
- Names defined once and used by those names afterwards:
  `Exclusion`, `Explanation`, `ExclusionReasons`, and `Explain` in `internal/k8s`;
  `targetsParams`, `exclusionView`, `explainBody`, `targetViews`, and `discoveryError` in `internal/httpapi`;
  `targetsQuery`, `retryWithoutExplain`, and `targetSummary` in `internal/ui/static/targetmodel.js`;
  `retryOnce`, the one new parameter of `request`,
  and `state.targetSummary`, the one new field, in `internal/ui/static/app.js`;
  `TargetsResponse.SelectorMatched`, `TargetsResponse.Excluded`, and `Exclusion` in `internal/client`.
- Current-source facts this plan rests on:
  `internal/k8s/discovery.go:42-54` declares `Discovery` with four methods;
  `internal/k8s/eligibility.go:21` reads the Service, its slices, and one Pod per endpoint,
  and never lists the namespace's Pods;
  `internal/k8s/eligibility.go:106` is per-endpoint and returns a `bool`,
  with the port rule applied inside it at `:136-139`;
  `internal/k8s/eligibility.go:53-68` treats two entries as a conflict only among endpoints that passed that rule,
  and logs a Pod-naming warning at `:68`;
  `internal/k8s/cluster.go:28` holds the Pod lister the namespace-wide read uses,
  and `internal/k8s/catalog.go:29-31` is the shape of a lister read that issues no request;
  `internal/httpapi/fixtures_test.go:92-195` and `internal/pgo/fixtures_test.go:1582-1635` each implement `Discovery`,
  the second assigned to a `k8s.Discovery` field at `internal/pgo/fixtures_test.go:1736`;
  `internal/httpapi/profile.go:108-118` calls `parsePortParams` first,
  which returns a port fault immediately at `:68-101`, ahead of any other name;
  `internal/httpapi/profile.go:145-175` is the sorted-name loop the targets parser copies;
  `internal/httpapi/profile.go:248` mints `404 pod_not_found` and `503 no_targets`, which this endpoint must not;
  `internal/httpapi/server.go:496-513` fills one `profileParams` for both branches
  and records the selection as sent at `:505-508`;
  `internal/httpapi/server.go:526-530` dispatches to `serveTargets(w, r, q)`,
  and `:549-574` holds the sentinel mapping inline inside `discover`, which always calls `Targets`;
  `internal/httpapi/server.go:268-278` is what `request` carries, and it is the audit and metrics data;
  `internal/httpapi/server.go:470-475` denies the realm before discovery runs;
  `internal/httpapi/server.go:283-320` holds `labels()`, which gains nothing;
  `internal/httpapi/audit.go:14-30` has no `requestId` field and adds `auth_reason` conditionally at `:66-68`;
  `internal/httpapi/targets.go:26-38` converts, sorts, and writes in one function;
  `internal/httpapi/targets_test.go:11` already exists and covers the sort, the address exclusion,
  the empty-array encoding, and the caller's slice being left alone;
  `internal/ui/static/urls.js:53-56` builds the targets URL from a port choice,
  `:30-43` is `portParams` with `targetsURL` as its only caller,
  `:86-95` is `profileURL`, which spells `port` and `portName` itself,
  and `:14-28` drops an empty query value;
  `internal/ui/static/app.js:276` resolves to the body or `null` and records the envelope under a key;
  `internal/ui/static/app.js:186` initializes `targets` as an array,
  `:361`, `:430`, `:447`, and `:483` reset it,
  and `:753`, `:796`, and `:807` read it;
  `internal/ui/static/app.js:792-809` renders both controls unconditionally and a message below them;
  `internal/ui/portmodel_test.go:14` names the module in a constant the two helpers read;
  `internal/ui/scan_test.go:14` lists `app.js`, `urls.js`, and `portmodel.js` and no fourth module,
  and `:107-121` holds the page to `portmodel.js`;
  `internal/ui/vendor_test.go:305-306` holds the import-free list, `portmodel.js` alone;
  `internal/ui/ui_test.go:212-224` is the served-asset table, `portmodel.js` at `:220`,
  and `:73-113` is the tree-hash mutation table, its `portmodel.js` case at `:91`;
  `internal/ui/static/collectionmodel.js` does not exist;
  `cmd/profgate/read.go:178-219` builds the `targets` query and renders three columns;
  `internal/client/wire.go:64-67` decodes `targets` and no other field;
  `docs/specs/cli.md:3` reads `**Status:** Accepted` and the document holds no `explain`;
  `docs/specs/gateway.md:525` and `:529-531` define the conflict over eligible endpoints
  where `:544` defines it over rules 5 and 7, and `:838-843` requires validation in name order;
  `docs/api.md:96-98` says a targets request never reaches selection,
  `:129-132` describes step 10 as filtering and choosing,
  and `:146-147` says the endpoint takes no parameter but the port selection;
  `docs/console.md:42-43` is where the Pod and version controls are described;
  `docs/plans/roadmap.md:102-107` has all three of the item's bullets already ticked,
  and `:11-12` says what a tick records;
  `test/e2e/registry.go:20-44` lists twenty-five scenarios, among them `ineligible pods`, `errors`, and `version filter`;
  `test/e2e/scenarios_test.go:78-82` decodes the targets body,
  and `:176` and `:217` are the helpers that read and poll it.
- Decided here because the spec leaves it to the implementer:
  `Targets` and `Explain` share the endpoint pass rather than each deriving eligibility,
  because a second derivation drifts silently,
  and the pass takes the Pod lookup as a parameter so only `Explain` reads the namespace;
  a counting lister in `export_test.go` is what makes that split a fact a test can fail;
  the vocabulary is an unexported array behind an accessor in `internal/k8s`,
  so the two reasons `internal/httpapi` adds are ordered against one list;
  the conflict warning follows the attribution rather than the old population;
  `parseTargetsParams` walks every accepted name in one sorted pass and stops calling `parsePortParams`,
  and the port-and-portName pair is checked after that loop because it is a fault of neither name alone;
  the audit port is extracted from the raw values independently of the fault that was returned;
  `serveTargets` takes the parsed parameters as an argument rather than reading them off `request`,
  which carries what the audit line and the metrics labels need and nothing else;
  the sentinel mapping moves into `discoveryError` so all three discovery calls answer alike,
  and `targetViews` is shared so the two bodies cannot disagree on the target array;
  `targetsURL` takes the whole query object rather than gaining a fourth argument,
  and `portParams` is deleted with it;
  `targetsQuery` takes a flag rather than the page deleting a key,
  so the retry provably differs by `explain` alone;
  `state.targets` stays an array and `state.targetSummary` is a second field,
  so no existing reset or reader changes meaning;
  the non-disclosure proofs scan bytes rather than read named fields,
  because a field added later is exactly what they exist to catch;
  the command line prints the reasons as a second table rather than as extra columns,
  because a count is not a property of a Pod row;
  the command line performs no retry without `explain`, unlike the console,
  because `--explain` is an explicit request from a person at a terminal.
- Left to the implementer by design: helper and test-fixture names,
  the exact wording of error messages beyond the values each must name,
  the column widths of the two tables,
  the internal split of the endpoint pass across functions in `internal/k8s`,
  and the markup the console's rows are rendered with, so long as every value reaches the DOM as text.
