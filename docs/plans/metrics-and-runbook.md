# The Alerts Fire in the Outages They Name

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.

**Goal:** make every gauge an operator alerts on mean what a threshold reads it as,
give every failure the gateway already measures a rule that fires in it,
and give the person holding the pager a page that names the symptom, the series, and the step.
`profgate_tls_certificate_expiry_seconds` reads `NaN` on an install with no certificate,
so an expiry rule is inert there instead of firing on a certificate that expired at the epoch.
`profgate_nats_connected` reads `NaN` where no connection is ever made and `0` from the moment one is configured,
so a rule over it is inert on an install that runs no collection
and fires throughout a NATS outage at startup.
`ProfgateNotReady` says a replica has not completed its initial discovery sync,
which is what its gauge measures, and the gauge's own `HELP` text says the same.
Every rule ships with a test that evaluates it, so a rule that cannot fire in the outage it names fails the suite.
Six failure modes that had a metric and no rule get one,
including the NATS outage in which `profgate_pgo_synced` is absent rather than `0`.
The `code` label's value set is written down, and so is the fact that two metrics watch the interactive path alone.
Three log lines that named no Collection and no request gain the identifier that joins them to the rest of the record.
An operator building a dashboard starts from queries that already work.

**Architecture:** two gauges gain a seed at construction in `internal/metrics/prometheus.go`,
one of the two also gains an explicit `0` in `cmd/profgate/serve.go`,
on the path configured to reach NATS and before its first connection attempt,
and a third gauge gains the description it always should have had;
`deploy/chart/profgate/templates/podmonitor.yaml` grows one target relabeling,
so the `endpoint` label the gateway sets reaches a query under the name it exports;
`deploy/chart/profgate/templates/prometheusrule.yaml` grows six rules and rewrites one annotation pair;
three `slog` calls gain an attribute each, and one of them gains a parameter to carry it;
`deploy/` gains a `promtool` fixture that evaluates every shipped rule and parses every documented query,
and `mise.toml` pins the tool that runs it;
and four documents grow the sections an operator reads when something is wrong.
No package, route, chart value, configuration key, or Kubernetes permission moves,
and `go.mod` gains nothing.
No new metric is created: every rule below reads a series the binary exports today,
which `deploy/chart_test.go` checks by reading `internal/metrics/prometheus.go` for each metric name.

**Spec:** the three behavior changes here are already accepted text.
[`gateway.md`](../specs/gateway.md) *Metrics* states that
`profgate_tls_certificate_expiry_seconds` is registered on every install and reads `NaN` until a certificate is loaded
(`docs/specs/gateway.md:1570-1582`),
that `profgate_nats_connected` reads `NaN` on a process that makes no connection,
`0` from the moment one is configured and before the first attempt, and `1` while it is up (`:1555-1566`),
and that `profgate_discovery_synced` is the initial informer sync alone,
never returns to `0`, and is not a readiness gauge (`:1530-1543`).
Its amendment block names the four files this plan changes to carry those meanings
(`docs/specs/gateway.md:2826-2829`).
Everything else here adds no behavior the specs describe:
the six rules read series [`gateway.md`](../specs/gateway.md) *Metrics* and [`pgo.md`](../specs/pgo.md) *Metrics* already define,
the runbook restates behavior [`gateway.md`](../specs/gateway.md) *Failure Scenarios*,
[`pgo.md`](../specs/pgo.md) *Health*, and [`pgo.md`](../specs/pgo.md) *The owner loop* already state,
and the log attributes change no record's meaning.
This work is ordered by [`roadmap.md`](roadmap.md),
under *Make the gauges and the alerts true, and write the runbook*.
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the system, not as the defects that revealed them.

- **A gauge that has nothing to report reports nothing a threshold can cross.**
  A series whose writer never runs must not read as a legitimate value.
  `profgate_oidc_jwks_age_seconds` already holds this with `NaN`
  (`internal/metrics/prometheus.go:139-149`);
  neither `profgate_tls_certificate_expiry_seconds` nor `profgate_nats_connected` does.
- **An alert fires in the outage it names and is silent otherwise.**
  A rule whose expression cannot become true during the failure its text describes is worse than no rule:
  it reads as coverage.
  `ProfgatePGONotSynced` is that rule during a NATS outage at startup,
  because the gauge it reads is registered inside `startPGO` (`cmd/profgate/serve.go:668`),
  which that outage never reaches.
- **An alert's text says what its expression measures.**
  A rule may be narrower than the condition an operator cares about;
  what it may not do is claim the wider one.
- **Every `code` label value is documented, or the label is not exposed.**
  A closed label set an operator cannot enumerate is not usable for a query,
  and a query written against a guessed value silently matches nothing.
- **A log line names the work it belongs to.**
  A record that names no request, no Collection, and no Service says that something failed and nothing about what.
- **A metric's scope is stated where the metric is documented.**
  Two of them count the interactive path alone,
  and a reader who takes them for totals under-counts every Collection the collection loops ran.

---

## Decisions

Three choices here settle something the roadmap left open, and the first departs from what its bullet proposed.

**`profgate_nats_connected` reports three cases, and the chart's `pgoEnabled` guard stays.**
The gauge is constructed at `internal/metrics/prometheus.go:95-98` and registered at `:154`, on every install,
while its only writer is `NATSConnected` (`:236-242`),
reached from the callback wired at `cmd/profgate/serve.go:590`, inside the NATS options built only on the PGO path.
`internal/natskv/preflight.go:52-53` calls that callback only after the initial connection succeeds,
which `internal/natskv/natskv.go:128-133` states as the contract,
so the callback never runs during a NATS outage at startup.
One constructed `0` therefore carries two opposite consequences:
an install with `pgo.enabled: false` exports `profgate_nats_connected 0` forever,
which no threshold can be written over,
and a startup outage on a `pgo.enabled` install reads `0` throughout,
which is exactly what a rule needs.
The gauge reports three cases instead, so both hold on their own.

`NaN`, seeded at construction the way the expiry gauge is,
is what a process that makes no NATS connection reports,
so an install with collection off crosses no threshold.
`0`, written once on the path that *is* configured for NATS —
in `natsPreflight` before its retry loop and therefore before the first dial —
is what keeps `ProfgateNATSDisconnected` firing during a NATS outage at startup.
That is the one outage `ProfgatePGONotSynced` cannot see,
because `profgate_pgo_synced` is not registered until `startPGO` runs (`cmd/profgate/serve.go:668`),
which such an outage never reaches.
`1` while the connection is up and `0` again while it is down is the existing behavior, unchanged.
[`gateway.md`](../specs/gateway.md) *Metrics* carries all three cases (`docs/specs/gateway.md:1555-1566`),
and its amendment block names the packages that hold them (`:2826-2829`).

The `0` write goes in `cmd/profgate` and not inside `natskv.Preflight`.
`internal/natskv` reports connection events and holds no opinion about a connection not yet attempted;
the gateway is where the decision to run NATS at all is made,
and `natsPreflight` is the one function reached only under that decision.
The roadmap bullet proposes the rule "without a code change" and its `Spec:` line declares no revision for it;
both are superseded here, and the closing task updates that line.

The chart's `pgoEnabled` guard on `ProfgateNATSDisconnected` stays.
With the seed the rule would be safe unguarded, because `NaN == 0` is false,
but `TestChartPrometheusRule` compares rendered alert names with `slices.Equal`,
so the rendered order is part of the assertion,
and moving the rule out of the guarded block churns both `want` lists for nothing.
`deploy/chart/profgate/templates/prometheusrule.yaml:54` renders `ProfgatePGONotSynced` only when `pgo.enabled`,
the new rule joins that block,
and the chart renders the gauge's rule exactly where the binary is configured to write the gauge.

[`pgo.md`](../specs/pgo.md) *Metrics* is not wrong about this gauge:
it writes "exists only when `pgo.enabled`" for `profgate_pgo_synced` (`docs/specs/pgo.md:2937`)
and for `profgate_pgo_collector_available` (`:2925-2926`), and never for `profgate_nats_connected`.
The first claim is true of the code — `PGOSyncedFrom` registers the gauge from inside `startPGO`.
The second describes a gauge no Go file names,
which that spec now says in the sentences that follow it rather than leaving a reader to find out.

**The rule's wording narrows; the gauge does not follow `ready()`.**
The roadmap offers both.
The spec has already chosen: `profgate_discovery_synced` "is not a readiness gauge"
and "reports the informer gate and none of the other three" (`docs/specs/gateway.md:1531-1544`),
with `/readyz` named as the complete signal and the Deployment's readiness probe.
Making the gauge follow `ready()` would contradict accepted text
and would also make it fall back to `0` during a drain,
which the same paragraph rules out.
So `ProfgateNotReady`'s two annotations are rewritten and `cmd/profgate/serve.go:475` is untouched.
The three gates the rule stops claiming do not go uncovered:
the NATS preflight gate gains `ProfgateNATSDisconnected` below,
the issuer gate is described by `ProfgateOIDCKeysStale` and the JWKS gauges,
and the drain has no metric by design (`docs/specs/gateway.md:1546`).

**The upstream rule pairs failures with the absence of successes, on one replica.**
The failure the roadmap names is *every* upstream refusing —
a NetworkPolicy that closed the pprof port, or a `discovery.pprof.port` that no workload listens on —
and not the one crashed Pod a bare failure count would fire on.
A ratio over `profgate_requests_total{endpoint="profile"}` cannot say that,
because that denominator counts requests which never reached an upstream:
`labels()` returns `("profile","none")` for every request that resolved no route
(`internal/httpapi/server.go:256-264`),
so a readiness, authentication, realm, parameter, discovery, or admission refusal lands in it.
A fleet-wide `sum` cannot say it either: one healthy replica hides a broken one in both terms.
So the expression is `A unless B` over per-target series —
the dial and deadline failures a replica recorded,
minus the replicas that also served a profile request successfully in the same window.
`unless` drops every left-hand series that has an identically-labelled right-hand one,
so a replica that served one profile fetch raises nothing
and a replica that served none while failing raises the alert by itself.
Both sides aggregate with `sum without (endpoint, profile, code)`,
which removes the three labels the gateway sets and keeps the ones the scrape adds,
so the alert names the replica.
Both sides are empty on a gateway nobody asks for profiles, so the rule is silent there rather than firing.

**Every shipped rule is evaluated by a pinned `promtool`, and every documented query is parsed by one.**
`TestChartPrometheusRule` compares alert names, checks the four metadata fields,
and looks for strings inside an expression;
it parses no PromQL, so it cannot fail when an expression stops matching the outage its text names —
which is the defect this plan exists to repair,
and which [`000-agent-contract.md`](../../.agents/rules/000-agent-contract.md) *Test Intent* rules out.
Three ways to close that were weighed.
A test-only dependency on `github.com/prometheus/prometheus` brings hundreds of packages with it,
into a tree whose import boundaries `scripts/check-repo.py` polices,
and it buys one test file.
Parsing without evaluating needs that same parser, so it costs the same and proves much less:
a rule can parse perfectly and still be unable to fire.
`promtool test rules` is the facility Prometheus ships for exactly this,
`mise registry` resolves `promtool` to `aqua:prometheus/prometheus`, which installs at `3.7.3`,
and `deploy/chart_test.go` already runs a mise-pinned binary and skips when it is absent
(`helmBin`, `deploy/chart_test.go:34-40`).
So `promtool` joins `[tools]` in `mise.toml` beside `helm`;
`.github/workflows/check.yml` installs it through `jdx/mise-action` with no edit to the workflow;
and a bare `go test` on a machine without it skips, as the Helm tests already do.
What that buys and what it leaves to review is written out in the alert task.

---

## Global Constraints

- **No new metric, configuration key, route, chart value, or Kubernetes permission.**
  Every rule reads a series `internal/metrics/prometheus.go` constructs today.
  `internal/k8s` is not touched and the ClusterRole does not move.
- **Every rule clears five gates in `deploy/`.**
  Four are `TestChartPrometheusRule`'s (`deploy/chart_test.go:1045-1163`):
  it appears in the ordered `want` list for the branch that renders it;
  every `profgate_*` name in its expression appears as a quoted string in `internal/metrics/prometheus.go`;
  it carries a `for`, a `severity` label, a `summary`, and a `description`;
  and every `code` label value in its expression is a constant in `internal/httpapi/codes.go`.
  The fifth is new:
  the rendered rule file is fed to `promtool test rules` against a checked-in series fixture,
  and the rule fires exactly on the cases the fixture declares.
  The fourth and fifth are new; the first three exist.
  A rule the chart cannot render, or the tests cannot check, is not shippable.
- **A new tool is pinned, and no new Go module.**
  `mise.toml` gains `promtool = "3.7.3"` in `[tools]`.
  `go.mod` gains nothing: the evaluation runs in a subprocess, as the Helm renders already do.
- **The shipped alert order is append-only.**
  `TestChartPrometheusRule` compares the rendered alert names with `slices.Equal`,
  so the order in the template is part of the assertion.
  New always-rendered rules go after `ProfgateOIDCKeysStale`,
  and the one guarded rule goes before `ProfgatePGONotSynced` inside the existing `pgoEnabled` block,
  which leaves every shipped name where it is today.
- **Every task that changes what a machine reads shows a red test before the fix.**
  Seven of the nine do: the three that change Go behavior, the one that changes the chart's scrape,
  the one that changes the chart's rules, the one that changes the discovery gauge's `HELP` string,
  and the one that adds the query fixture each name the test and the exact command that shows it red.
  The two that only add prose to a guide — the `code` label reference and the troubleshooting sections —
  add no behavior and have no red state to show, which the *Risks* section records as the gap it is.
- **One change here is breaking, and `CHANGELOG.md` marks it.**
  `v0.5.0` is a tagged, published release, so every entry describes a change to a shipped installation.
  None of them narrows an admitted configuration, removes a series, or changes a status code,
  and the entries for the expiry gauge and the connection gauge each say what an operator's own rule sees.
  The scrape task is the exception:
  on an install running `podMonitor.enabled: true` it moves the values of `profgate_requests_total`'s
  `endpoint` label out of `exported_endpoint` and back under `endpoint`,
  and takes `endpoint="ops"` off `up` and the other per-scrape series.
  A dashboard already written against either name stops matching,
  so that entry carries the `BREAKING:` prefix the changelog uses for exactly this.
- **Every alert's text names the condition and the next step, in the shape the shipped rules use.**
  A `summary` is one sentence naming what is wrong;
  a `description` says what the expression measured, over what window, and what an operator does about it.
- No jargon: comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a task name, or a review round.
- Markdown prose uses semantic line breaks;
  run `semlf check` on every Markdown file and every Go file with doc comments a task writes or edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters — the hook refuses 50 or more —
  with a body that says what changed and why, and no trailer of any kind
  ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

---

## File Structure

```text
internal/metrics/prometheus.go                        # the expiry and connection gauges seeded NaN at construction; two HELP strings
internal/metrics/prometheus_test.go                   # the initial exposition of both TLS series and of the connection gauge; two pinned HELP strings
internal/metrics/recorder.go                          # what DiscoverySynced's doc comment says the gauge means
cmd/profgate/serve.go                                 # the connection gauge written 0 before the preflight's first dial
cmd/profgate/serve_test.go                            # the call sequence the two preflight tests expect
internal/pgo/rounds.go                                # the sample record names its Collection
internal/pgo/rounds_test.go                           # the attribute is asserted
internal/httpapi/auth.go                              # the authenticator error names the request
internal/httpapi/pgo_collections.go                   # the receipt warning names the request and the Service
internal/httpapi/auth_test.go                         # the authenticator error names its request
internal/httpapi/pgo_collections_test.go              # the receipt warning names its request and Service
deploy/chart/profgate/templates/podmonitor.yaml       # the target relabeling that leaves the gateway's endpoint label alone
deploy/chart/profgate/templates/prometheusrule.yaml   # six rules; ProfgateNotReady's two annotations
deploy/chart/profgate/values.yaml                     # the comment listing the shipped set; what the scrape drops
deploy/chart/profgate/README.md                       # the alert table, the readiness row's wording, the scrape paragraph
deploy/chart_test.go                                  # the relabeling assertion, the two want lists, the code-label subtest, and the two promtool runs
deploy/testdata/alerts_test.yaml                      # the promtool series fixture and its alert expectations
deploy/testdata/example-queries.yaml                  # the documented queries as a rule file promtool parses
mise.toml                                             # promtool pinned beside helm
docs/deployment.md                                    # the two metric rows, the code label, the scope, troubleshooting, queries
docs/authentication.md                                # the authentication troubleshooting section
docs/pgo.md                                           # what a NATS disconnect costs a running Collection
CHANGELOG.md                                          # the Added, Changed, and Fixed entries
docs/plans/roadmap.md                                 # the item's checkboxes and its Shipped line
docs/plans/metrics-and-runbook.md                     # this file
```

---

## The expiry gauge reports nothing until a certificate is loaded

Closes the roadmap bullet beginning
*`profgate_tls_certificate_expiry_seconds` registers unconditionally*.

**Files:**
- Modify: `internal/metrics/prometheus.go`, `internal/metrics/prometheus_test.go`,
  `docs/deployment.md`, `CHANGELOG.md`

**The decision, and why.**
The gauge is constructed at `internal/metrics/prometheus.go:103-106`
and registered with everything else at `:154`, on every install.
Its only writer is `Loader.apply` (`internal/tlscert/loader.go:146-151`),
and the only `tlscert.New` in the tree is at `cmd/profgate/serve.go:245`,
inside `if cfg.Server.TLS.Enabled()` (`:244`).
So an install with no `server.tls` exports `profgate_tls_certificate_expiry_seconds 0` —
a certificate that expired at the start of 1970.
`docs/deployment.md:224-225` promises that this gauge is how a stalled rotation becomes visible.
No threshold rule can keep that promise while every install without TLS sits below every threshold.

The seed goes at construction and nowhere else:

```go
tlsCertificateTTL: prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "profgate_tls_certificate_expiry_seconds",
	Help: "When the certificate the API listener serves stops being valid, in seconds since the epoch, or NaN before one is loaded.",
}),
```

followed, before the `reg.MustRegister` call at `:151`, by

```go
p.tlsCertificateTTL.Set(math.NaN())
```

`math` is already imported by this file, for `jwksAge`.
The mechanism is deliberately a `Set` on an ordinary gauge and not the `GaugeFunc` shape `jwksAge` uses:
this gauge has a real writer that publishes a timestamp,
so it needs a starting value rather than a function to compute one on the scrape goroutine.
The type is unchanged and `TLSCertificateExpiry` (`internal/metrics/prometheus.go:250-252`) is unchanged.

**What a rule then reads.**
[`gateway.md`](../specs/gateway.md) *Metrics* states the consequence and gives the expression to copy
(`docs/specs/gateway.md:1572-1576`):
the text exposition carries `profgate_tls_certificate_expiry_seconds NaN`,
so the series is present and `absent()` does not match it;
arithmetic on it stays `NaN` and the ordered comparisons filter it out;
and `profgate_tls_certificate_expiry_seconds - time() < 604800` matches nothing until a certificate is served.
That expression is the one the next task ships.

- [x] **Write the test**

Add `TestPrometheus_TLSBeforeAnyCertificate` to `internal/metrics/prometheus_test.go`,
beside `TestPrometheus_TLS` (`:267-292`).
`TestPrometheus_TLS` writes to both series before it asserts, so it cannot pin the initial state;
this is a separate test on a fresh `prometheus.NewPedanticRegistry()` with nothing recorded.

| What it asserts, and how it fails today |
|---|
| `testutil.ToFloat64(rec.tlsCertificateTTL)` is `NaN`, checked with `math.IsNaN`, as `TestPrometheus_JWKSAge` (`:370-377`) checks its own gauge; today it is `0` |
| `testutil.GatherAndCompare(reg, ...)` against the exposition `profgate_tls_certificate_expiry_seconds NaN` under its `# HELP` and `# TYPE` lines; today the exposition carries `0` |
| `testutil.GatherAndCount(reg, "profgate_tls_reloads_total")` is `0`, the call shape `TestPrometheus_PGOSyncedFrom` uses at `:109`; green today, and the case that holds the counter absent from an install without TLS, which the same spec paragraph states |

`TestPrometheus_TLS` needs no edit:
it records `applied`, `failed`, and an expiry before comparing,
so the seed is overwritten before either assertion runs.

The red state, before the seed lands:

```bash
go test ./internal/metrics/ -run 'TestPrometheus_TLSBeforeAnyCertificate'
```

The first two assertions fail on `0`; the counter assertion passes.

- [x] **Seed the gauge and say so where it is documented**

`docs/deployment.md:463`, the gauge's row in the metrics table, gains the seed:
`NaN` until a certificate is loaded, so an install without `server.tls` crosses no threshold.
The paragraph at `:224-225` gains the clause that makes the promise checkable:
the expiry threshold is inert until a certificate is served,
and the reload counter has no series at all on an install without TLS.

`CHANGELOG.md`, `### Fixed`:
**`profgate_tls_certificate_expiry_seconds` reads `NaN` before a certificate is loaded.**
It read `0` on every install without `server.tls`, a certificate that expired at the epoch,
which no expiry threshold could be written over.
An operator's own rule that compared the gauge to zero no longer matches on such an install;
`profgate_tls_certificate_expiry_seconds - time() < 604800` is the form that now works everywhere.

- [x] **Validate and commit**

```bash
semlf check internal/metrics/prometheus.go internal/metrics/prometheus_test.go \
  docs/deployment.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/metrics/prometheus.go internal/metrics/prometheus_test.go \
  docs/deployment.md CHANGELOG.md
git commit -m "fix(metrics): seed the expiry gauge NaN" -m "<body: the gauge is registered on every install and written only under server.tls, so an install without TLS read an expiry at the epoch; what NaN does to a threshold>"
```

---

## The connection gauge reports nothing where no connection is made

Carries the roadmap bullet beginning
*With `pgo.enabled` and NATS unreachable, `profgate_pgo_synced` is absent rather than `0`*,
which the alert task closes:
the rule that bullet asks for fires in the outage it names only because of the write this task adds.

**Files:**
- Modify: `internal/metrics/prometheus.go`, `internal/metrics/prometheus_test.go`,
  `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`,
  `docs/deployment.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* above states it: the gauge reports `NaN`, `0`, and `1`,
where today one constructed `0` has to serve two purposes that pull apart.
The gauge is constructed at `internal/metrics/prometheus.go:95-98` and registered at `:154`, on every install;
its only writer is `NATSConnected` (`:236-242`),
reached from `cmd/profgate/serve.go:590` through `natskv.Options.OnConnectionChange`;
and `internal/natskv/preflight.go:52-53` invokes that callback only after the initial connection succeeds
(`internal/natskv/natskv.go:128-133` is where that contract is written).
So an install without `pgo.enabled` reports a transport it never configured as down,
and a startup outage reports nothing the callback wrote.

The seed goes at construction, beside the expiry gauge's:

```go
natsConnected: prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "profgate_nats_connected",
	Help: "Whether the NATS connection is currently up: 1 if up, 0 if down, or NaN on a process that makes none.",
}),
```

followed, before the `reg.MustRegister` call at `:151`, by

```go
p.natsConnected.Set(math.NaN())
```

`math` is already imported by this file, for `jwksAge`.
`NATSConnected` is unchanged: it sets `1` or `0` and overwrites the seed on its first call.

The write goes in `natsPreflight` (`cmd/profgate/serve.go:579-616`),
between the options literal and `backoff := preflightBackoffFirst` (`:597`),
so it runs once and before the first dial:

```go
// The gauge reads NaN on a process that makes no connection.
// This process makes one, so the transport is down until the callback below reports otherwise,
// and a rule over the gauge fires through an outage that never reaches the callback at all.
deps.recorder.NATSConnected(false)
```

It goes here and not inside `natskv.Preflight` for the reason *Decisions* gives:
`internal/natskv` reports connection events and has no opinion about a connection not yet attempted.
The comment above `OnConnectionChange` (`:587-589`) says today that without the report `Preflight` makes
"the gauge would read zero until the first reconnect";
that describes the constructed `0` and is rewritten to describe the written one.

**What a rule then reads.**
[`gateway.md`](../specs/gateway.md) *Metrics* states all three cases (`docs/specs/gateway.md:1555-1566`).
`NaN` crosses no comparison, so `profgate_nats_connected == 0` is inert on an install that runs no collection.
`0` from before the first connection attempt is what makes that same expression fire through a NATS outage at startup.
That is the one outage `profgate_pgo_synced` cannot report at all,
because it is not registered until `startPGO` runs (`cmd/profgate/serve.go:668`).
`1` while the connection is up, and `0` again when it drops, is what the gauge already did.
That expression is the one the alert task ships as `ProfgateNATSDisconnected`.

- [x] **Write the test**

Three existing tests move and one is new, in two packages.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestServePGO/the connected gauge reports the initial connection` (`cmd/profgate/serve_test.go:1742-1751`) | the call sequence becomes `[false, true]`: the write before the retry loop, then the callback on the connection. The assertion reads `len(got) != 1 \|\| !got[0]` today, which is `[true]`, and it fails on the new first call |
| `TestNATSCallbacksSplitTheirDuties` (`cmd/profgate/serve_test.go:1566-1621`) | it drives the real `natsPreflight` with a stub that returns a client without ever invoking `OnConnectionChange`, so `connectedCalls()` is empty when preflight returns today and `[false]` afterwards. Both of its counts move: the check after `opts.OnConnectionChange(false)` (`:1604-1606`) expects `[false, false]`, and the count after `OnGenerationMove` (`:1618-1621`) moves with it. What the test pins — that a generation move is not a connection report — does not change, and neither does its prose |
| `TestPrometheus_NATSConnected` (`internal/metrics/prometheus_test.go:248-262`) | its exposition pins the gauge's `HELP` string (`:255`), which the `NaN` case rewrites. It records `NATSConnected(true)` before comparing, so the seed itself never reaches its assertion |
| `TestPrometheus_NATSBeforeAnyConnection`, new, beside it | on a fresh `prometheus.NewPedanticRegistry()` with nothing recorded: `testutil.ToFloat64(rec.natsConnected)` is `NaN` under `math.IsNaN`, as `TestPrometheus_JWKSAge` checks its own gauge, and `testutil.GatherAndCompare` matches the exposition `profgate_nats_connected NaN` under its `# HELP` and `# TYPE` lines. Today both read `0` |

The red state, before the seed and the write land:

```bash
go test ./internal/metrics/ -run 'TestPrometheus_NATS'
go test ./cmd/profgate/ -run 'TestServePGO|TestNATSCallbacksSplitTheirDuties'
```

The metrics tests fail on the `HELP` string and on `0` where `NaN` is expected;
the two in `cmd/profgate` fail on the call sequence, each naming the calls it got.

- [x] **Seed the gauge, write the zero, and say so where it is documented**

`docs/deployment.md:460`, the gauge's row in the metrics table, reads "1 while the NATS connection is up"
and gains the two cases it does not name:
`NaN` where the process makes no connection, and `0` from the moment one is configured.

`CHANGELOG.md`, `### Fixed`:
**`profgate_nats_connected` reads `NaN` where no NATS connection is ever made.**
It read `0` on every install with `pgo.enabled: false` — a transport that was never configured, reported as down —
which no rule could be written over.
It now reads `0` from the moment a process is configured to reach NATS, before its first attempt,
rather than only once a connection has been made and lost.
An operator's own `profgate_nats_connected == 0` rule no longer matches on an install that runs no collection,
and it now matches from the start of a NATS outage at startup instead of staying silent through it.

- [x] **Validate and commit**

```bash
semlf check internal/metrics/prometheus.go internal/metrics/prometheus_test.go \
  cmd/profgate/serve.go cmd/profgate/serve_test.go docs/deployment.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/metrics/prometheus.go internal/metrics/prometheus_test.go \
  cmd/profgate/serve.go cmd/profgate/serve_test.go docs/deployment.md CHANGELOG.md
git commit -m "fix(metrics): seed the NATS gauge NaN" -m "<body: the gauge is registered on every install and written only by the connection callback, so an install with collection off reported a transport it never configured as down; the explicit zero before the first dial is what keeps a rule live through an outage at startup>"
```

---

## The readiness alert and the gauge's own text say what the gauge measures

Closes the roadmap bullet beginning
*`profgate_discovery_synced` moves once, `0` to `1`*.

**Files:**
- Modify: `internal/metrics/prometheus.go`, `internal/metrics/prometheus_test.go`,
  `internal/metrics/recorder.go`,
  `deploy/chart/profgate/templates/prometheusrule.yaml`,
  `deploy/chart/profgate/README.md`, `docs/deployment.md`, `CHANGELOG.md`

**The decision, and why.**
The *Decisions* section above settles it: the wording narrows and the gauge stays as it is.
`deps.recorder.DiscoverySynced(true)` runs once, at `cmd/profgate/serve.go:475`,
on the branch that reports the initial informer sync;
`ready()` (`:172-174`) reads four things —
not draining, the issuer discovered under `oidc`, `cluster.HasSynced()`, and the NATS preflight under `pgo.enabled` —
and three of them can be false with the gauge at `1`.
The rule's summary says "A profgate replica is not serving."
(`deploy/chart/profgate/templates/prometheusrule.yaml:28`)
and its description says "/readyz answers 503 and no traffic reaches this replica" (`:29-31`),
both of which claim the wider condition.

**The rule is not the only text that has to move.**
[`gateway.md`](../specs/gateway.md) *Metrics* states the gauge is the initial sync alone and never returns to `0`
(`docs/specs/gateway.md:1530-1546`),
and three places in the tree still describe a live cache state:
the gauge's `HELP` string, "Whether the discovery cache is currently synced: 1 if synced, 0 otherwise."
(`internal/metrics/prometheus.go:66-69`);
the `DiscoverySynced` doc comment on the `Recorder` interface, "reports whether the discovery cache is currently synced"
(`internal/metrics/recorder.go:71`);
and the metric table's row, "1 when the discovery cache is synced" (`docs/deployment.md:453`).
An operator reads the `HELP` string in every scrape and the table row before writing a rule,
so leaving either saying "currently" leaves the alert's repair half-done.
All four move together, which is what makes the amendment [`gateway.md`](../specs/gateway.md) already carries true
(`docs/specs/gateway.md:2816-2820`).

**The exact text.**
`prometheusrule.yaml:22-31` becomes:

```yaml
        - alert: ProfgateNotReady
          expr: profgate_discovery_synced == 0
          for: 10m
          labels:
            severity: critical
          annotations:
            summary: A profgate replica has not completed its initial discovery sync.
            description: >-
              This replica has reported no initial informer sync for ten minutes, so
              /readyz answers 503 and no traffic reaches it. The gauge says that and
              nothing more: it also reads 0 while an unreachable issuer or a Kubernetes
              preflight that has not passed keeps the informers from starting, and it
              never returns to 0, so a replica that goes unready later -- draining, an
              issuer it cannot reach, or a NATS preflight still retrying -- is not what
              fired this.
```

The `HELP` string at `internal/metrics/prometheus.go:66-69` becomes
"Whether the initial discovery sync has completed: 1 once every informer has finished its first list, 0 before that.",
and `DiscoverySynced`'s doc comment at `internal/metrics/recorder.go:71` says the same in a sentence:
it reports whether the initial informer sync has completed, is called once, and never reports `false` afterwards.

`deploy/chart/profgate/README.md:488`, the readiness row of the alert table, becomes:

```text
| `ProfgateNotReady` | `profgate_discovery_synced == 0` | A replica has not completed its initial informer sync for ten minutes, so `/readyz` answers 503. The gauge is also `0` while an unreachable issuer or a failing Kubernetes preflight keeps the informers from starting, and it never returns to `0`, so it does not report a replica that goes unready later |
```

`docs/deployment.md:453`, the gauge's row in the metric table, becomes
`1 once the initial discovery sync has completed; never back to 0`.

`deploy/chart/profgate/values.yaml:231` names the alert and its metric and claims nothing about either,
so it needs no edit;
`docs/deployment.md:488` names the expression and claims nothing either.
Confirm before the commit with

```bash
grep -rn "not serving" deploy/chart/profgate/ docs/deployment.md docs/authentication.md
```

which is one hit today, the template's summary, and none after.
The phrase also appears in the roadmap, the accepted spec's amendment table, and an investigation,
which are records of the repair rather than operator-facing text,
so a `grep` across all of `deploy/` and `docs/` finds them and proves nothing.

- [x] **Write the test**

No new test, and a red state that already exists.
`TestPrometheus_DiscoverySynced` pins the `HELP` string in its expected exposition
(`internal/metrics/prometheus_test.go:89-101`),
so changing the string makes that test fail before the expected text is updated with it:

```bash
go test ./internal/metrics/ -run 'TestPrometheus_DiscoverySynced'
```

`TestChartPrometheusRule` asserts that every rule carries a non-empty `summary` and `description`
(`deploy/chart_test.go:1096-1098`) and nothing about their text,
which is the right boundary:
an annotation string is prose,
and a test that pinned it would fail on every wording repair without catching a wrong one.
The expression is unchanged here, so the `promtool` fixture the next task adds covers it there.
The rendered output is confirmed once with:

```bash
helm template deploy/chart/profgate --set prometheusRule.enabled=true --show-only templates/prometheusrule.yaml
```

- [x] **Rewrite the annotations, the two strings, and the two table rows**

`CHANGELOG.md`, `### Changed`:
**`ProfgateNotReady` and `profgate_discovery_synced` say what the gauge measures.**
The alert claimed that a replica was not serving, and the gauge's own `HELP` text called it a current cache state,
where it reports the completion of the initial informer sync alone and never returns to `0`.
The alert now names that gate, names the three readiness gates it does not report,
and says that the gauge is also `0` while an issuer or a Kubernetes preflight keeps the informers from starting.
The expression, the window, and the severity are unchanged.

- [x] **Validate and commit**

```bash
semlf check internal/metrics/prometheus.go internal/metrics/recorder.go \
  internal/metrics/prometheus_test.go deploy/chart/profgate/README.md \
  docs/deployment.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/metrics/prometheus.go internal/metrics/recorder.go \
  internal/metrics/prometheus_test.go \
  deploy/chart/profgate/templates/prometheusrule.yaml \
  deploy/chart/profgate/README.md docs/deployment.md CHANGELOG.md
git commit -m "docs(metrics): narrow the readiness gauge" -m "<body: the alert and the gauge's own HELP claimed a live cache state where the gauge reports the initial informer sync alone; which gates it does not report>"
```

---

## The gateway's `endpoint` label reaches a query under the name it exports

Closes no roadmap bullet of its own.
It is what makes the label the two tasks below select on and group by mean anything on an install the chart builds,
which is why it runs before either of them.

**Files:**
- Modify: `deploy/chart/profgate/templates/podmonitor.yaml`, `deploy/chart_test.go`,
  `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md`,
  `docs/deployment.md`, `CHANGELOG.md`

**Why the label does not arrive.**
`profgate_requests_total` carries `endpoint`, `profile`, and `code`
(`internal/metrics/prometheus.go:50-52`).
prometheus-operator's `PodMonitor` generator writes a target label of the same name,
holding the endpoint's port name as its value:
`generatePodMonitorConfig` appends `target_label: endpoint` with `replacement: <port>` to the job's `relabel_configs`,
and appends the endpoint's own `relabelings` after it.
`deploy/chart/profgate/templates/podmonitor.yaml:23-26` names `port: ops`,
so every target of the chart's own `PodMonitor` carries `endpoint="ops"`.
Prometheus resolves that collision before anything else can:
`mutateSampleLabels` sets each target label over the scraped sample,
renames every exposed label it displaced to `exported_<name>`,
and only then runs `metric_relabel_configs`.
So a chart install with `podMonitor.enabled: true` exports `profgate_requests_total` with `endpoint="ops"`,
the gateway's own value under `exported_endpoint`,
and a query or a rule scoped `endpoint="profile"` matches nothing at all.

**The decision, and why.**
The chart's `PodMonitor` drops the target label, in `relabelings`, and nothing else changes.

```yaml
  podMetricsEndpoints:
    - port: ops
      path: /metrics
      interval: {{ .Values.podMonitor.interval | quote }}
      relabelings:
        - action: labeldrop
          regex: endpoint
```

`relabelings` and not `metricRelabelings`, because the two run on opposite sides of the rename.
`relabelings` becomes the job's `relabel_configs`,
which `PopulateLabels` runs over the discovered label set to produce the target's labels, before any scrape.
Drop `endpoint` there and the target never carries one,
no collision arises, and the gateway's `endpoint="profile"` arrives under its own name.
`metricRelabelings` becomes `metric_relabel_configs`, which runs last, after the rename:
the same `labeldrop` there would delete the target's `endpoint`
and leave the gateway's value sitting in `exported_endpoint`,
so `endpoint="profile"` would still match nothing and the name would now be gone from both.
That shape is not a smaller version of this fix; it is a different outcome.

Two other shapes were weighed and rejected.
`honorLabels: true` makes *every* exposed label win over its target label rather than this one,
so a label the gateway adds later named `pod`, `namespace`, or `job` would silently replace the scrape's own answer,
and the `PodMonitor` would stop being able to say which Pod a series came from.
Renaming the gateway's label from `endpoint` to `route` changes the exported label set of a published release,
breaks every query an operator has already written against it,
and reaches past this item into `internal/metrics` and every document that names the label.

**What this costs, stated plainly.**
The target label is dropped for every series of that target, `up` and `scrape_duration_seconds` included,
so `up{endpoint="ops"}` stops matching.
It distinguished nothing: this `PodMonitor` declares one endpoint and its value is always `ops`.
On an install already running `podMonitor.enabled: true`,
the values that were reaching a dashboard as `exported_endpoint` arrive as `endpoint` instead,
which is why the changelog entry below is marked breaking.

**What it does not touch.**
`labeldrop` deletes every label whose *name* the regex matches,
and a relabeling regex is anchored, so `regex: endpoint` matches that name exactly
and never `exported_endpoint`.
It iterates the labels that are present, so where a target sets no `endpoint` it removes nothing
and fails nothing.
That matters for the kustomize install, which renders no `PodMonitor` at all —
`deploy/base/` has none, and this relabeling is not part of it.
Where a Prometheus scrapes those Pods from a configuration of its own,
it attaches whatever target labels that configuration writes,
and the gateway's `endpoint` collides there only if that configuration sets one.

- [x] **Write the test**

`TestChartPodMonitor` (`deploy/chart_test.go:963`) gains a third subtest beside `off by default` and `on`,
because the file keeps one test per template.
The `podMonitor` type declares only what these tests read (`:933-955`),
so its `PodMetricsEndpoints` element gains
`Relabelings []struct{ Action string; Regex string }` under the `relabelings` JSON name.

The subtest renders with `podMonitor.enabled=true`,
and asserts that the one endpoint carries exactly one relabeling,
whose `action` is `labeldrop` and whose `regex` is `endpoint`.
The regex is pinned as a literal rather than read from `internal/metrics/prometheus.go`:
it is the label name the gateway exports, and a rename of that name is a breaking change
that `internal/metrics` tests catch on their own.

The red state:

```bash
go test ./deploy/ -run 'TestChartPodMonitor'
```

The rendered endpoint carries no relabelings, so the new subtest fails on an empty list.
The `off by default` and `on` subtests pass before and after.

- [x] **Drop the target label, and say so where the scrape is documented**

The template gains the `relabelings` block above,
with a comment saying what the operator writes, what the gateway sets,
and that the dropped value was the constant `ops`.

| File | Change |
|---|---|
| `deploy/chart/profgate/README.md`, the `PodMonitor` paragraph (`:469-478`) | the rendered `PodMonitor` drops the `endpoint` target label prometheus-operator writes from the port name, so `profgate_requests_total`'s own `endpoint` reaches a query under that name; `up` and the other per-scrape series carry no `endpoint` as a result |
| `docs/deployment.md`, the `PodMonitor` paragraph (`:477-485`) | the same sentence, next to the metric table that lists the label |
| `deploy/chart/profgate/values.yaml`, the comment above `podMonitor` (`:204-212`) | one line saying the rendered scrape drops that target label and why |

`CHANGELOG.md`, `### Changed`:
**BREAKING: the chart's `PodMonitor` drops the `endpoint` target label.**
prometheus-operator writes one from the port name, the gateway sets its own `endpoint` on
`profgate_requests_total`, and with `honorLabels` unset the scrape renamed the gateway's value to
`exported_endpoint`, so no query or rule scoped `endpoint="profile"` matched anything.
The target label is now dropped before the scrape and the gateway's value arrives as `endpoint`.
On an install already running `podMonitor.enabled: true`,
a dashboard or rule reading `exported_endpoint` stops matching and reads `endpoint` instead,
and `up{endpoint="ops"}` and the other per-scrape series lose a label whose value was always `ops`.
The kustomize install renders no `PodMonitor` and is unchanged.

- [x] **Validate and commit**

```bash
helm template deploy/chart/profgate --set podMonitor.enabled=true \
  --show-only templates/podmonitor.yaml
semlf check deploy/chart/profgate/README.md deploy/chart_test.go \
  docs/deployment.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add deploy/chart/profgate/templates/podmonitor.yaml deploy/chart_test.go \
  deploy/chart/profgate/values.yaml deploy/chart/profgate/README.md \
  docs/deployment.md CHANGELOG.md
git commit -m "fix(chart): keep the gateway's endpoint label" -m "<body: the operator's target label of the same name displaced the exposed one to exported_endpoint; the drop goes in relabelings, before the scrape, because metricRelabelings runs after the rename; what an existing dashboard sees>"
```

---

## Six failure modes each have a rule that fires in them

Closes the roadmap bullet beginning
*Six failure modes have a metric and no alert*,
and the bullet beginning
*With `pgo.enabled` and NATS unreachable, `profgate_pgo_synced` is absent rather than `0`*.

**Files:**
- Modify: `deploy/chart/profgate/templates/prometheusrule.yaml`,
  `deploy/chart/profgate/values.yaml`, `deploy/chart/profgate/README.md`,
  `deploy/chart_test.go`, `mise.toml`, `docs/deployment.md`, `CHANGELOG.md`
- Add: `deploy/testdata/alerts_test.yaml`

**Why the two bullets are one task.**
The NATS bullet asks for `profgate_nats_connected == 0` inside the existing `pgoEnabled` guard,
and "NATS disconnected while running" is one of the six failure modes the other bullet lists.
That is one rule, shipped once.
It covers both because the gauge task above writes `0` in `natsPreflight` before the first dial,
so the gauge reads `0` throughout an outage that never reaches `startPGO` (`cmd/profgate/serve.go:668`)
and therefore never registers `profgate_pgo_synced` at all.
`ProfgatePGONotSynced` is silent through exactly that outage, and this rule is not.
The same task seeds the gauge `NaN` where no connection is made,
which is why `profgate_nats_connected == 0` can stay inside the `pgoEnabled` guard
and still not fire on every install that runs no collection.

**What every expression here holds to.**
Each names only metrics `internal/metrics/prometheus.go` constructs
and only `code` values `internal/httpapi/codes.go` defines.
Each also keeps the identity of the replica it is about.
`profgate_requests_total` carries `endpoint`, `profile`, and `code`,
and everything else on the series is a label the scrape attached — `instance`, `pod`, `namespace`, `job`.
A bare `sum(...)` drops all of them, so one broken replica out of the chart's default two
(`deploy/chart/profgate/values.yaml:21-25`) is averaged into a fleet that looks fine.
`sum without (endpoint, profile, code)` removes the gateway's three labels and keeps the scrape's,
so the alert instance names the Pod an operator has to go and look at.
`ProfgateAdmissionSaturated` (`prometheusrule.yaml:32-41`) keeps its `sum(` because a shipped assertion pins it
(`deploy/chart_test.go:1140-1144`); every rule added here uses the aggregation that preserves the target.

**The six rules.**
The five always-rendered rules go after `ProfgateOIDCKeysStale` (`prometheusrule.yaml:42-53`);
`ProfgateNATSDisconnected` goes inside the `pgoEnabled` block (`:54`), before `ProfgatePGONotSynced`.

```yaml
        - alert: ProfgateAuthLimiterSaturated
          expr: sum without (endpoint, profile, code) (rate(profgate_requests_total{code="too_many_auth"}[5m])) > 0
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: profgate is refusing authentication attempts.
            description: >-
              This replica has answered at least one authentication attempt with 429
              too_many_auth in every five-minute window for the last ten minutes. It does
              not say the gate was full throughout: a steady trickle of refusals reads
              the same as a saturated one. Only the bcrypt gate at
              auth.basic.maxConcurrent answers this code, so raise that limit or add
              replicas -- the gate is per replica -- and a client retrying a wrong
              credential is the usual cause. The rule is inert outside auth.mode basic.
        - alert: ProfgateAuthUnavailable
          expr: sum without (endpoint, profile, code) (rate(profgate_requests_total{code="auth_unavailable"}[5m])) > 0
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: profgate is refusing requests it cannot authenticate.
            description: >-
              This replica has answered at least one request with 503 auth_unavailable in
              every five-minute window for the last ten minutes. Which callers are refused
              depends on why, and profgate_auth_failures_total by reason separates them:
              keys_stale refuses every token this replica verifies, entropy and exchange
              are the browser login alone and leave bearer tokens and live sessions
              working, and internal is a programming error that may be either. Read that
              series before treating this as a total outage.
        - alert: ProfgateUpstreamsUnreachable
          expr: >-
            sum without (endpoint, profile, code) (rate(profgate_requests_total{code=~"upstream_unreachable|upstream_timeout"}[5m])) > 0
            unless
            sum without (endpoint, profile, code) (rate(profgate_requests_total{code="ok",profile!="none",endpoint!~"collection.*"}[5m])) > 0
          for: 15m
          labels:
            severity: warning
          annotations:
            summary: profgate is failing every profile request it finishes.
            description: >-
              For fifteen minutes this replica has answered profile requests at the dial
              or the deadline and has served none successfully in the same five-minute
              window, which is a NetworkPolicy that closed the pprof port or a
              discovery.pprof.port no workload listens on, not one Pod that died. It says
              nothing about a replica serving some profiles and failing others, and it
              produces no sample while no profile request is served, so an outage that
              starts in a quiet period waits for traffic.
        - alert: ProfgateTLSReloadFailing
          expr: >-
            sum without (result) (rate(profgate_tls_reloads_total{result="failed"}[15m]))
            / sum without (result) (rate(profgate_tls_reloads_total[15m])) > 0.9
          for: 15m
          labels:
            severity: warning
          annotations:
            summary: profgate cannot re-read its API listener certificate.
            description: >-
              More than nine in ten of this replica's certificate re-reads over the last
              fifteen minutes failed, so it is still serving the pair it last applied and
              a renewal into the Secret is not reaching it. The loader re-reads every 30
              seconds, so that is at least 27 failures of 30 rather than one unlucky read
              during a Secret swap. profgate_tls_certificate_expiry_seconds still holds
              the served certificate's notAfter, which is how long that is safe for. The
              counter has no series at all without server.tls, so this rule is inert
              there.
        - alert: ProfgateTLSCertificateExpiring
          expr: profgate_tls_certificate_expiry_seconds - time() < 604800
          for: 1h
          labels:
            severity: warning
          annotations:
            summary: profgate's API listener certificate expires within a week, or has expired.
            description: >-
              The certificate this replica serves stops being valid in under seven days,
              and the rule keeps firing once it has expired, because the difference only
              falls further. The gauge is NaN until a certificate is loaded, and NaN
              crosses no comparison, so this rule is inert on an install without
              server.tls.
```

and, inside the `pgoEnabled` block:

```yaml
        - alert: ProfgateNATSDisconnected
          expr: profgate_nats_connected == 0
          for: 5m
          labels:
            severity: warning
          annotations:
            summary: profgate has no NATS connection.
            description: >-
              The transport has been down on this replica for five minutes. At startup
              that holds /readyz at 503 while the preflight retries a connection failure,
              and profgate_pgo_synced does not exist yet, so this is the only alert that
              fires then. On a running replica each Collection it owns aborts once the
              lease it last committed lapses, which is pgo.leaseTTL -- 60s by default and
              up to 10m -- so an outage shorter than that lease costs nothing, and every
              abort past it spends one of pgo.maxAttempts. PGO routes answer 503
              pgo_unavailable while it lasts.
```

**Why each window and severity.**
`ProfgateAuthLimiterSaturated` and `ProfgateAuthUnavailable` copy `ProfgateAdmissionSaturated`'s shape
(`prometheusrule.yaml:32-41`): a five-minute rate held for ten minutes,
so one burst decays before the rule fires and a sustained one does not.
Both are `warning`, and `ProfgateAuthUnavailable` is deliberately not the `critical`
that a total authentication outage would deserve.
The expression cannot tell the total case from the narrow one:
`auth_unavailable` is the code every `503` authentication failure answers with —
`internal/httpapi/auth.go:131-133` for the request path and `internal/auth/browser.go:416-425` for the login routes —
and [`auth.md`](../specs/auth.md) *Audit and metrics* maps four reasons to that status (`docs/specs/auth.md:763`).
Only one of the four refuses everything.
`keys_stale` is raised while verifying a token (`internal/auth/verify.go:85,95`), so it refuses every caller.
`entropy` is a random read that failed while sealing a login's cookies
(`internal/auth/cookie.go:124-138`, `internal/auth/wire.go:22-28`),
and its only callers are the browser routes (`internal/auth/browser.go:228,236,295`),
so it stops logins and touches no bearer token and no live session.
`exchange` is an issuer token endpoint that is unreachable,
which [`auth.md`](../specs/auth.md) says "stops logins and leaves existing sessions and bearer tokens untouched"
(`docs/specs/auth.md:725-726`).
`internal` is a programming error and reaches the code from both paths
(`internal/httpapi/auth.go:87`, `internal/auth/browser.go:432`).
A rule that pages at `critical` on a browser-login outage is a rule an operator learns to ignore,
so the severity is the one the weaker case justifies and the description says where to look to tell them apart.
`ProfgateUpstreamsUnreachable` takes five-minute rates rather than fifteen-minute ones
so that a steady outage is not hidden behind the decay of the last success:
the condition becomes true about five minutes after the last successful profile fetch, and `for: 15m` holds it,
which puts the page at roughly twenty minutes of continuous failure with traffic arriving throughout.
`ProfgateTLSReloadFailing` compares failures against every reload outcome rather than counting failures,
because `Loader.refresh` records `failed`, `unchanged`, or `applied` on every pass
(`internal/tlscert/loader.go:127-150`),
so a positive failure rate alone stays true for one bad read among twenty-nine good ones.
The ratio is `> 0.9` rather than `== 1` so that one lucky read inside a broken rotation does not silence it,
and the fifteen-minute window is roughly thirty re-reads at the loader's thirty-second period
(`docs/deployment.md:218-220`).
Its denominator is zero exactly when the numerator is, which yields `NaN` and no sample,
so the rule is silent on a replica doing no reloads and absent entirely without `server.tls`.
`ProfgateTLSCertificateExpiring` holds for an hour because the condition changes once a day at most;
the expression is the one [`gateway.md`](../specs/gateway.md) *Metrics* wrote to be copied (`docs/specs/gateway.md:1576`).
`ProfgateNATSDisconnected` uses five minutes rather than the ten `ProfgatePGONotSynced` uses,
because the transport repairs itself in seconds on a reconnect,
because five minutes is the window [`pgo.md`](../specs/pgo.md) *Metrics* already gives a collection outage,
and because at startup it is the only rule that can fire at all —
waiting ten minutes there would delay the one signal an operator has.
Five minutes is not a claim that owned Collections have aborted:
`pgo.leaseTTL` is configurable from `30s` to `10m` (`internal/config/config.go:345`),
an owner aborts once `committedLeaseUntil - skewMargin` passes without a successful renewal
(`docs/specs/pgo.md:1450-1456`),
and the description states the dependency rather than a fixed consequence.
The overlap with `ProfgatePGONotSynced` on a running replica is deliberate:
the transport rule fires at five minutes and the barrier rule at ten,
so an operator who sees only the first knows the outage is younger than ten minutes,
and one who sees both knows the barrier has not cleared either.

**What none of these rules promises.**
Every one is evaluated per scrape target, and a `for:` holds only while the same series keeps matching.
A scrape gap shorter than the instant selector's lookback or the rate range is bridged by the samples on either side;
a gap long enough to leave the expression without a sample resets the hold,
and the window starts again when scraping resumes.
A replacement Pod is a new target with a new `instance` and `pod`,
so its hold starts at zero even though the outage did not.
An alert that is firing therefore proves the condition held on one target for the window,
and never that the outage is younger or older than it looks across a restart or a scrape gap.
The runbook rows below are where an operator reads the state directly instead of inferring it from the hold.

**Where the `endpoint` label reaches these rules, and where it may not.**
The task above drops the `endpoint` target label prometheus-operator writes,
so under the chart's own `PodMonitor` the gateway's value arrives under that name
and `endpoint!~"collection.*"` narrows the upstream rule's success side to what it says it does.
A Prometheus scraping these Pods from a configuration of its own can still attach a target label named `endpoint`,
and with `honorLabels` unset the gateway's value would land in `exported_endpoint` there.
That is why the condition each rule fires on selects on `code` and never on `endpoint`:
the one place `endpoint` appears above is that success-side exclusion,
which passes harmlessly where the label is missing, so the rule degrades rather than dies.
*Risks and What This Plan Does Not Cover* records what a hand-written scrape still costs.

- [x] **Write the tests**

Three edits in `deploy/chart_test.go`, one new subtest, one new fixture, and one line in `mise.toml`.

`mise.toml` gains `promtool = "3.7.3"` in `[tools]`, pinned to an exact version as every tool there is.
`deploy/testdata/alerts_test.yaml` is the `promtool test rules` unit-test file,
in a `testdata` directory `deploy/` does not have yet and three other packages already do
(`cmd/profgate/testdata`, `internal/pgo/testdata`, `internal/config/testdata`).
It is checked in, and the rules it runs against are rendered, so the two have to meet on disk:
the test renders the template with `pgo.enabled=true`,
writes a rule file named `rules.yaml` in one `t.TempDir()`
that serializes `groups:` with the rendered `spec.groups` beneath it,
copies `alerts_test.yaml` beside it,
and runs `promtool test rules alerts_test.yaml` with that directory as the working directory,
so the fixture's `rule_files: [rules.yaml]` resolves with no path built at runtime.
`promtoolBin` skips the way `helmBin` does (`deploy/chart_test.go:34-40`) when the binary is absent.
The fixture pins `evaluation_interval: 1m`,
gives every `input_series` the `instance` and `pod` labels a real scrape attaches so two replicas are distinguishable,
and sizes each case's `eval_time` past that case's own rule --
past the point its series first satisfies the expression, plus that rule's own `for:` --
rather than past one figure for the whole file.
The longest hold among every rule here is the one hour `ProfgateTLSCertificateExpiring` carries,
and holding every other case's series open that long would cost each of them the better part of an hour of samples for nothing,
against a ten- or fifteen-minute hold that needs none of it.
Only the certificate-expiring cases -- the gauge six days ahead, the gauge a day in the past, and the gauge at `NaN` --
need a series spanning past an hour of samples;
the `NaN` case would pass at any `eval_time`,
since the comparison it never crosses stays false no matter how long the hold runs,
but sizing it with the two that do fire keeps one fixture rather than a fourth global figure.
`promtool`'s series notation keeps that hour cheap:
a single `<value>x<count>` token drove a one-hour hold to fire in a direct check of this fixture,
not a written-out value for every one of the sixty-odd minutes.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestChartPrometheusRule/the_shipped_set`, case `pgo disabled` (`deploy/chart_test.go:1058-1062`) | `want` becomes the eight names in template order: `ProfgateNotReady`, `ProfgateAdmissionSaturated`, `ProfgateOIDCKeysStale`, `ProfgateAuthLimiterSaturated`, `ProfgateAuthUnavailable`, `ProfgateUpstreamsUnreachable`, `ProfgateTLSReloadFailing`, `ProfgateTLSCertificateExpiring`; today the render produces three and `slices.Equal` fails |
| `TestChartPrometheusRule/the_shipped_set`, case `pgo enabled` (`:1063-1069`) | `want` becomes those eight followed by `ProfgateNATSDisconnected` and `ProfgatePGONotSynced`; today the render produces four |
| `every code label in an expression is a code the gateway writes`, new, in `TestChartPrometheusRule` beside `the admission alert names a code the gateway writes` (`:1122-1145`) | renders with `pgo.enabled=true`, extracts every `code="..."` and every alternative inside `code=~"..."` from every rule's `expr` with a regular expression, and asserts each is a quoted string in `internal/httpapi/codes.go`, read the way the existing subtest reads it (`:1129-1133`). It also asserts the extracted set is non-empty, so a rewrite that stops matching cannot pass by finding nothing. Green today over the one shipped `code` value, and red the moment a new rule names a code that is not a constant |
| `the rules fire on the series they name`, new, `TestChartPrometheusRule` | renders, writes the rule file, and runs `promtool test rules` over `deploy/testdata/alerts_test.yaml`; today the fixture's expectations name six alerts the render does not carry, so `promtool` reports every one of them missing |

The existing `the admission alert names a code the gateway writes` subtest is untouched:
it also asserts the `sum(` prefix, which is specific to that rule.
The metric-name loop at `:1105-1116` needs no edit,
and it is what stops a rule over `profgate_pgo_collector_available`, a name no Go file holds.

**What the fixture evaluates.**
One case per property an annotation claims, so a wrong expression fails rather than a wrong name:

| Case | What it proves |
|---|---|
| two replicas, one answering `too_many_auth` and one not | `ProfgateAuthLimiterSaturated` fires with the failing `instance` in its labels, and only once |
| one refusal, then eleven quiet minutes | it does not fire: a burst that decays is not a saturated gate |
| `auth_unavailable` in every window for eleven minutes | `ProfgateAuthUnavailable` fires at `warning`, per replica |
| a replica failing every profile request while a second serves them | `ProfgateUpstreamsUnreachable` fires on the first alone |
| a replica failing some profile requests and serving others | it does not fire |
| no profile series at all | it does not fire, and produces no sample |
| a replica with one `unchanged` reload for every failed one | `ProfgateTLSReloadFailing` does not fire: the ratio is 0.5 |
| a replica whose every reload is `failed` | it fires |
| no `profgate_tls_reloads_total` series | neither reload case fires, which is an install without `server.tls` |
| `profgate_tls_certificate_expiry_seconds` at `NaN` | `ProfgateTLSCertificateExpiring` does not fire |
| the gauge six days ahead, and the gauge a day in the past | it fires in both, which is what "or has expired" claims |
| `profgate_nats_connected` `0` for six minutes, the gauge at `NaN`, and the series absent | `ProfgateNATSDisconnected` fires in the first and in neither of the others; `NaN` is what an install running no collection exports, and `== 0` does not match it |
| `profgate_discovery_synced` `0` for eleven minutes, then `1` | `ProfgateNotReady` fires and then stops |
| `too_many_profiles` in every window for eleven minutes | `ProfgateAdmissionSaturated` fires |
| one `too_many_profiles` refusal, then eleven quiet minutes | it does not fire: a burst that decays is not a saturated gate |
| `profgate_oidc_jwks_age_seconds` above `43200` for sixteen minutes | `ProfgateOIDCKeysStale` fires |
| `profgate_oidc_jwks_age_seconds` at `NaN` | it does not fire, which is an install outside `auth.mode: oidc` |
| a counter reset written into `input_series` | no rule fires on the reset itself: `rate` handles it, a difference would not |
| a scrape gap written as `_` for six samples | the `for:` hold restarts rather than carrying across the gap, which is what the paragraph above states |

**What still rests on review.**
The fixture proves that each expression fires on the series its text describes,
and stays silent on the series it does not.
It does not check the prose of a `summary` or a `description`,
the choice of `warning` against `critical`,
or whether ten minutes rather than five is the right patience for a human —
those are judgements, and pinning them to a string would fail on every wording repair.
It also runs against label sets the fixture writes,
so it cannot see what a scrape does to a label:
the render assertion on the `PodMonitor` is what holds the chart's own scrape,
and a scrape configured by hand is beyond either of them.

The red state:

```bash
go test ./deploy/ -run 'TestChartPrometheusRule'
```

Both `the shipped set` cases fail on the alert-name comparison, and the `promtool` subtest fails on six missing alerts.
The new code-label subtest passes before and after; it is the guard, not the driver.

- [x] **Add the six rules**

Before writing the `want` lists, read each of the six expressions the way the test does:
it extracts every `profgate_[a-z_]+` match from an expression
and requires that name to be a quoted string in `internal/metrics/prometheus.go`.
All six above clear it,
and a rule added later that does not fails at `mise run test` with a message naming the metric rather than the gate.

Confirm the render before running the suite:

```bash
helm template deploy/chart/profgate \
  --set prometheusRule.enabled=true --set pgo.enabled=true \
  --set nats.url=nats://nats.profgate.svc:4222 \
  --show-only templates/prometheusrule.yaml
```

Two expressions are folded with `>-` because they are longer than the file's other lines —
`ProfgateUpstreamsUnreachable` and `ProfgateTLSReloadFailing`, and no other rule here needs it;
YAML folding joins those lines with a single space, which PromQL accepts,
and `unless` and `/` are both operators that a space on either side leaves intact.
Confirm each rendered `expr` is one line before committing;
`promtool` reads the rendered file, so a fold that broke an expression fails the subtest rather than shipping.

- [x] **Say what each rule fires on, where the alerts are documented**

| File | Change |
|---|---|
| `deploy/chart/profgate/README.md`, the alert table (`:487-491`) and the sentence above it (`:483-484`) | one row per new rule, in template order, each naming the expression and what it fires on; the sentence becomes eight always and two more when `pgo.enabled`. A sentence after the table says the expiry and reload rules are inert without `server.tls`, the limiter rule is inert outside `auth.mode: basic`, and the upstream rule produces no sample while no profile request is served |
| `deploy/chart/profgate/values.yaml`, the comment at `:229-239` | lists the eight shipped names and the two rendered only when `pgo.enabled`, in place of the four it lists today |
| `docs/deployment.md`, the paragraph at `:487-497` | becomes a table with the same rows as the chart README's, since the prose list no longer fits ten rules; the sentences about the ops port, the stale-keys threshold, and `prometheusRule.rules` replacing the set outright stay as they are |
| `CHANGELOG.md`, `### Added` | **Six more alerts ship with the chart.** A failing certificate re-read, a certificate within a week of expiry, an authenticator that cannot decide, a saturated authentication gate, a replica whose profile requests all fail at the dial or the deadline, and, only when `pgo.enabled`, a NATS connection that is down. The last is the one alert that fires during a NATS outage at startup, where `profgate_pgo_synced` does not exist yet and `ProfgatePGONotSynced` is silent. Each of the new rules names the replica it fired on rather than the fleet. A deployment that set `prometheusRule.rules` keeps its own set and sees none of them |

- [x] **Validate and commit**

```bash
semlf check deploy/chart/profgate/README.md deploy/chart_test.go \
  docs/deployment.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add deploy/chart/profgate/templates/prometheusrule.yaml \
  deploy/chart/profgate/values.yaml deploy/chart/profgate/README.md \
  deploy/chart_test.go deploy/testdata/alerts_test.yaml mise.toml \
  docs/deployment.md CHANGELOG.md
git commit -m "feat(chart): alert on six measured failures" -m "<body: each of the six had a metric and no rule; every expression is evaluated by a pinned promtool; why the NATS rule is guarded, and why it is the only one that fires during a startup outage>"
```

---

## Three log lines name the work they belong to

Closes the roadmap bullet beginning
*A failed PGO sample logs at debug with no Collection identifier*.

**Files:**
- Modify: `internal/pgo/rounds.go`, `internal/pgo/rounds_test.go`,
  `internal/httpapi/auth.go`, `internal/httpapi/pgo_collections.go`,
  `internal/httpapi/auth_test.go`, `internal/httpapi/pgo_collections_test.go`, `CHANGELOG.md`

**What each line carries today.**
Each was read from the source, and each is the only record its failure writes:

| Line | Attributes today | What is missing |
|---|---|---|
| `collection sample` at debug (`internal/pgo/rounds.go:412`) | `pod`, `round`, `result`, `bytes` | the Collection: with several running, a failed sample cannot be attributed to one |
| `authenticator failed` at error (`internal/httpapi/auth.go:86`) | `error` | `requestId`, which is what joins a client's report to the gateway's record (`docs/deployment.md:504-506`) |
| `pgo: idempotency receipt is not readable` at warning (`internal/httpapi/pgo_collections.go:633`) | `error` | `requestId` and the Service whose Collection was being created |

**Where each identifier comes from.**
`recordSample` (`internal/pgo/rounds.go:406-414`) is called from `absorb` (`:389`),
which holds `state.man`, whose `Collection` field is the record's ID (`:131-132`).
So `recordSample` takes the identifier as a parameter rather than reading it from anywhere new:
`func (r *Rounds) recordSample(collection string, s Sample)`, called as `r.recordSample(state.man.Collection, s)`.
`absorb` is its only caller; confirm with `grep -rn "recordSample" internal/` before the change.

`authenticate` (`internal/httpapi/auth.go:77-79`) already takes `q *request`,
and `q.audit.requestID` is the value the audit record opens with (`internal/httpapi/audit.go:41`),
so the attribute is `"requestId", q.audit.requestID` with no signature change.

`lookupReceipt` (`internal/httpapi/pgo_collections.go:624-626`) takes no request.
Its two callers are at `:511` and `:714`, both of which hold the `*http.Request` and the parsed request.
Read both before choosing the shape:
the parameter list gains the request identifier and the Service, or it gains the `*request`,
whichever matches what those two call sites already have in hand.
Name the two attributes `requestId` and `service`,
which are the audit record's own names for them (`internal/httpapi/audit.go:41,50-53`),
so an operator greps one string across both records.
The namespace is not added: the audit record for the same request already carries it,
and `requestId` is what joins the two.

- [x] **Write the tests**

Each of the three is asserted against a logger the package's own fixtures already build.
`internal/pgo` has `logCapture` (`internal/pgo/fixtures_test.go:1278-1300`),
which flattens each record to its level, message, and attribute map,
and returns every record carrying a given message.
`internal/httpapi` has `harness.logger` (`internal/httpapi/fixtures_test.go:528`),
a JSON handler writing into `h.logs`, which the existing tests already read records out of.
Neither package needs a capture handler added.

| Test | What it asserts, and how it fails today |
|---|---|
| a new case in `internal/pgo/rounds_test.go` covering a round whose sample fails | the captured `collection sample` record carries a `collection` attribute equal to the record's ID; today the record has four attributes and none of them is it |
| a new case in `internal/httpapi/auth_test.go` driving an `Authenticate` that returns an error which is not an `*auth.Failure` | the captured `authenticator failed` record carries `requestId` equal to the `X-Request-Id` the request sent; today it carries `error` alone. `auth.Failure` is matched with `errors.As` (`internal/httpapi/auth.go:82`), so any other error type reaches this branch |
| a new case in `internal/httpapi/pgo_collections_test.go`, beside the other Collection-creation cases, driving a `ReadReceipt` that returns an error which is neither `nil` nor `natskv.ErrKeyNotFound` | the captured `pgo: idempotency receipt is not readable` record carries `requestId` and `service`; today it carries `error` alone |

The red state:

```bash
go test ./internal/pgo/ ./internal/httpapi/ -run 'Sample|Authenticator|Receipt'
```

Name the three cases so that pattern matches them.
All three fail on the missing attribute.

- [x] **Add the three attributes**

- [x] **Validate and commit**

`CHANGELOG.md`, `### Fixed`:
**Three log lines name the work they belong to.**
A failed collection sample now names its Collection,
and the authenticator error and the unreadable idempotency receipt now carry `requestId`,
with the receipt warning naming the Service too,
so each can be joined to the audit record of the same request.

```bash
semlf check internal/pgo/rounds.go internal/pgo/rounds_test.go \
  internal/httpapi/auth.go internal/httpapi/pgo_collections.go \
  internal/httpapi/auth_test.go internal/httpapi/pgo_collections_test.go CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/pgo/rounds.go internal/pgo/rounds_test.go \
  internal/httpapi/auth.go internal/httpapi/pgo_collections.go \
  internal/httpapi/auth_test.go internal/httpapi/pgo_collections_test.go CHANGELOG.md
git commit -m "fix(log): name the work three records failed" -m "<body: a failed sample named no Collection and two errors carried no requestId, so neither could be joined to the audit record of the same request>"
```

This is the last commit that carries code, so the end-to-end suite runs on it before the pull request opens.

---

## The metrics reference names every `code` value and the path two metrics watch

Closes the roadmap bullet beginning
*The `code` label's value set is undocumented*.

**Files:**
- Modify: `docs/deployment.md`, `CHANGELOG.md`

**What the label actually holds.**
Three families, each confirmed against the source:

- **Forty envelope codes**, the constants in `internal/httpapi/codes.go:12-95`,
  registered in `envelopeCodes` (`:105-147`).
  `TestEnvelopeCodesMatchTheErrorTables` (`internal/httpapi/codes_test.go:43-49`)
  compares the registry against the two error tables written out by hand,
  so the forty and the specs cannot drift apart without a red test.
- **Eight audit-only outcomes**, the inventory `TestAuditOnlyCodesAreNotRegistered` holds
  (`internal/httpapi/codes_test.go:71-73`):
  `ok`, `upstream_stream_failed`, `internal_error`, `auth_redirect`,
  `cas_contended`, `artifact_stream_failed`, `client_gone`, and `cancelled`.
  The comment at `internal/httpapi/codes.go:100-104` names seven of them and omits `auth_redirect`,
  so the test is the source here and the comment is the thing that has drifted.
  `auth_redirect` is written for a browser navigation sent to login
  (`internal/httpapi/auth.go:19,114-115`), counted in the request metrics,
  and already described at `docs/deployment.md:523-524`;
  it is not a failure and is not in `profgate_auth_failures_total`.
  None of the eight is ever written into an envelope, and none is in the registry,
  so a reader who works from `/v1/openapi.json` alone will never find them.
- **The `upstream_<status>` family**, minted at `internal/proxy/proxy.go:171` from any status of `400` or above.
  Its values are HTTP statuses, so the family is bounded but not enumerable in advance.

**The decision, and why.**
The eight and the family are written out; the forty are pointed at, not retyped.
The eight appear in no document an operator reads and are short enough to list.
The forty are already in [`gateway.md`](../specs/gateway.md) *Errors*
and [`pgo.md`](../specs/pgo.md) *Errors*, are served by `/v1/openapi.json`,
and are held to those tables by a test.
A fourth copy in the deployment guide would be the one nothing checks,
and would drift the first time a code is added.

**The other half of the bullet.**
`profgate_confirm_total` and `profgate_profiles_in_flight` are written only from
`internal/httpapi/server.go:710-711` and `:717-738`, the interactive profile handler.
`internal/pgo/rounds.go:448` calls `Discovery.Confirm`, the Kubernetes seam's own method,
and records nothing: `grep -rn "Recorder.Confirm\|ProfilesInFlight(" internal/ cmd/` finds no other writer.
So a Collection's samples confirm their targets and are counted by neither metric,
and a reader who takes either for a total under-counts every sample the collection loops ran.

- [x] **Write it where the label is documented**

`docs/deployment.md`, after the `endpoint` paragraph at `:472-476`:
the three families above, with the eight listed and the forty pointed at by document.
State that the union is closed apart from `upstream_<status>`,
so a query may safely match on an exact value.

The rows at `:451-452` gain the scope:
`profgate_confirm_total` and `profgate_profiles_in_flight` count the interactive profile path alone,
and a Collection's samples appear in `profgate_collection_samples_total` instead.

`CHANGELOG.md`, `### Added`:
**The deployment guide names every value the `code` label takes.**
The forty envelope codes by the tables that hold them, the eight audit-only outcomes in full,
and the `upstream_<status>` family;
and it says that `profgate_confirm_total` and `profgate_profiles_in_flight` count the interactive path alone.

- [x] **Validate and commit**

```bash
semlf check docs/deployment.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add docs/deployment.md CHANGELOG.md
git commit -m "docs: name every code label value" -m "<body: forty envelope codes, eight audit-only outcomes, and the upstream family; and the two metrics that watch the interactive path alone>"
```

---

## An operator on call has a page that says what to look at

Closes the roadmap bullet beginning
*`docs/deployment.md` and `docs/authentication.md` have no troubleshooting section*.

**Files:**
- Modify: `docs/deployment.md`, `docs/authentication.md`, `docs/pgo.md`, `CHANGELOG.md`

**The shape.**
A `### Troubleshooting` section under *Operations* in `docs/deployment.md`,
after *Audit log* and before *Smoke test*,
and a `## Troubleshooting` section at the end of `docs/authentication.md`.
Each opens with a table of symptom, the series or log line that shows it, and the step;
each closes with the cases that need more than a row.
Every row cites behavior already stated in an accepted spec —
[`gateway.md`](../specs/gateway.md) *Failure Scenarios* (`docs/specs/gateway.md:2494-2538`),
[`pgo.md`](../specs/pgo.md) *Health* (`docs/specs/pgo.md:2866-2887`) —
so this section restates, in the operator's order, what those tables state in the designer's.

**The rows `docs/deployment.md` must carry.**
At least these, each written from the file cited:

| Symptom | Where it shows |
|---|---|
| `/readyz` 503 from startup and never green | `profgate_discovery_synced` `0`; the log repeats `preflight attempt` with a resource and a verb (`cmd/profgate/serve.go:457-471`) |
| the process exits at startup naming a resource and a verb | the preflight received `403`; the ClusterRole lacks a tuple (`docs/specs/gateway.md:2499`) |
| `/readyz` 503 under `auth.mode: oidc`, no `issuer discovered` line | issuer discovery is retrying; the process exits after `auth.oidc.discoveryTimeout` (`docs/deployment.md:366-372`) |
| `/readyz` 503 under `pgo.enabled`, with `nats preflight attempt` repeating | the connection is unavailable and `profgate_nats_connected` reads `0`; only that failure retries, and it retries for as long as it lasts (`cmd/profgate/serve.go:597-614`). `ProfgateNATSDisconnected` is what fires |
| the process exits at startup after one `nats preflight failed` at error | a missing bucket, a bucket of the wrong kind, configuration outside the contract, or a probe the account is denied. None of those retries: the error names the bucket and the operation or the field (`cmd/profgate/serve.go:477-486,573-578`) |
| every profile request answers `502` or `504` | `profgate_requests_total` with `upstream_unreachable` or `upstream_timeout`; a NetworkPolicy or a wrong `discovery.pprof.port` |
| profile requests answer `429` | `profgate_requests_total{code="too_many_profiles"}`; `limits.maxConcurrentProfiles` is per replica |
| a renewed certificate is not being served | `profgate_tls_reloads_total{result="failed"}` climbing while `profgate_tls_certificate_expiry_seconds` does not move |
| the expiry gauge reads `NaN` | no certificate has been loaded: `server.tls` is not configured |
| PGO routes answer `503 pgo_unavailable` while `/readyz` is green | `profgate_pgo_synced` `0`; a store a watch cannot re-open (`docs/specs/pgo.md:2882-2887`) |

**One row the table does not carry.**
[`pgo.md`](../specs/pgo.md) *Failure modes* describes a collector Deployment going absent,
`profgate_pgo_collector_available` falling to `0`, and `POST /collections` answering `503 collector_unavailable`
(`docs/specs/pgo.md:3721`).
None of that is behavior this build has.
`internal/httpapi/codes.go:90-92` says of the constant, in the source, that no route answers it,
the gauge appears in no Go file,
and `cmd/profgate/serve.go:670-703` starts the worker, the scheduler, and the sweeper inside every gateway with
`pgo.enabled`, so nothing is ever absent to report.
An operator guide that documents it sends a reader looking for a series that does not exist.
The row arrives with the collector split, not before it,
and *Risks and What This Plan Does Not Cover* records the discrepancy against the accepted spec.

**The three cases that need prose.**
Each is a short subsection under the table, and each is why this bullet is not just a table:

- **A bucket deleted or recreated under a running process.**
  The process stays up, the seam retries the re-open until the bucket exists again,
  `/readyz` stays green, and every PGO route refuses for as long as that lasts
  (`docs/specs/pgo.md:2882-2887`).
  `profgate_pgo_synced` is where it shows and `ProfgatePGONotSynced` is what fires.
  Recreating the bucket with the same name is enough; no restart is needed.
  Say that the log writes one record when a re-open starts failing and one when every watch is open again,
  and nothing per retry (`docs/specs/pgo.md:2857-2864`),
  so a bucket absent for an hour leaves two records rather than hundreds.
- **A restore from backup.**
  A restore moves the store generation, which clears the replay barrier on every process,
  so every replica reads `profgate_pgo_synced` `0` until its watches have replayed under the new generation.
  Records restored from a backup carry leases and deadlines stamped before the restore;
  say that a Collection whose lease has lapsed is reclaimed by the ordinary scan and costs an attempt,
  and that one whose deadline has passed is expired rather than resumed.
- **NATS maintenance.**
  Plan it as an outage that costs attempts, not as a pause.
  A disconnect during maintenance aborts every Collection each replica owns, below.

**What a NATS disconnect costs, said where the guides describe it.**
[`pgo.md`](../specs/pgo.md) states it: the owner takes its store view once at claim time,
a generation move makes every later renewal `ErrUnavailable` on that view,
and the owner takes the abort path once `committedLeaseUntil - skewMargin` passes without a success
(`docs/specs/pgo.md:1450-1456`).
An aborted attempt is an attempt spent, against `pgo.maxAttempts`.
That key is what bounds the exhaustion, not the number three:
it is configurable from `1` to `10` and defaults to `3` (`internal/config/config.go:346`),
so the sentence is written as `pgo.maxAttempts` outages and the default is named as a default.
`docs/pgo.md:310-317` describes attempts being spent by a replica that dies and by a rollout, and not by this;
`docs/deployment.md:362-364` says only that a disconnect does not turn readiness off.
Both gain the sentence.
The troubleshooting table carries the operator-facing form:
a NATS outage that outlasts the lease an owner last committed ends the Collections in flight
and each one spends an attempt,
so `pgo.maxAttempts` such outages inside one Collection's deadline exhaust it — three at the default.
An outage shorter than that lease costs nothing,
and `pgo.leaseTTL` runs from `30s` to `10m` (`internal/config/config.go:345`),
so how long "shorter" is depends on the installation.

**The rows `docs/authentication.md` must carry.**
The table is keyed on `profgate_auth_failures_total`'s `reason` label and the audit record's `auth_reason`,
and it accounts for all twenty-three values [`auth.md`](../specs/auth.md) *Audit and metrics* defines
(`docs/specs/auth.md:737-761`).
A reason an operator acts on differently gets a row of its own;
reasons whose one step is the same share a row that names each of them,
so a reader who greps a value out of a log finds it here.
Every value below appears in exactly one row:

| Row | Reasons it names |
|---|---|
| no credential, or one the mode cannot read | `missing`, `scheme`, `malformed` |
| the users file does not hold that credential | `bad_credential` |
| the bcrypt gate is full | `throttled` |
| nothing verified the token's signature, or its algorithm or type is refused | `signature`, `alg`, `token_type` |
| the token was minted for another issuer or another audience | `issuer`, `audience` |
| the token or the session is outside its validity window | `expired`, `session` |
| the token carries no usable subject, username, or groups claim | `claim` |
| authenticated, and mapped to no realm | `no_realm` |
| a browser round trip whose integrity check failed | `nonce`, `state`, `csrf` |
| the issuer refused the login | `issuer_denied`, `exchange_denied` |
| the issuer's token endpoint is unreachable or answered `5xx` | `exchange` |
| the signing keys are older than `auth.oidc.jwksMaxStale` | `keys_stale` |
| the gateway itself failed | `entropy`, `internal` |

`entropy` sits in that last row rather than beside `keys_stale`
because it is raised while sealing a login's cookies and reaches nothing else
(`internal/auth/cookie.go:124-138`, `internal/auth/wire.go:22-28`,
called only from `internal/auth/browser.go:228,236,295`).

Each row names the status the caller sees, from `docs/specs/auth.md:763-765` —
`throttled` is `429`, the four `503` reasons are `exchange`, `keys_stale`, `entropy`, and `internal`,
`missing` and `session` are `302` for a navigation under the browser flow,
and every other reason is `401` —
and the step: fix the credential, fix the mapping, check the issuer, raise `auth.basic.maxConcurrent`.
The last row is the one that is not the operator's to fix: `entropy` and `internal` are the gateway failing,
and `internal` is logged at error with the authenticator's own record.
Close with the browser round trip: a navigation with no credential is a `302` with `auth_redirect`
and is not counted as a failure (`docs/deployment.md:523-524`),
so an empty `profgate_auth_failures_total` does not mean logins are working.
`ProfgateAuthUnavailable` fires on the code, not the reason,
so the table is also how an operator turns that alert into the four reasons it can mean.

- [x] **Write both sections and the two disconnect sentences**

- [x] **Validate and commit**

`CHANGELOG.md`, `### Added`:
**The guides have troubleshooting sections.**
The deployment guide names the symptom, the series or log line, and the step for each failure the gateway measures,
including a bucket deleted or recreated under a running process, a restore from backup, and NATS maintenance;
the authentication guide does the same for each authentication failure reason.
Both guides now say that a NATS disconnect ends the Collections a replica owns and spends an attempt on each.

```bash
semlf check docs/deployment.md docs/authentication.md docs/pgo.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add docs/deployment.md docs/authentication.md docs/pgo.md CHANGELOG.md
git commit -m "docs: add the troubleshooting sections" -m "<body: symptom, signal, and step for each measured failure; what a disconnect costs a running Collection, which neither guide said>"
```

---

## A dashboard starts from queries that already work

Closes the roadmap bullet beginning
*One page of example queries under `docs/deployment.md`*.

**Files:**
- Modify: `docs/deployment.md`, `deploy/chart_test.go`,
  `deploy/testdata/alerts_test.yaml`, `CHANGELOG.md`
- Add: `deploy/testdata/example-queries.yaml`

**The decision, and why.**
Queries in the guide, and no dashboard file.
The roadmap puts a dashboard file out of scope, and *Not on This List* names it again,
because a dashboard is an artifact with its own compatibility surface
and a query in a table is copied into whatever the operator already runs.
The page goes under `### Metrics`, after the alert table, as `### Example queries`,
so a reader meets the series, then the alerts over them, then the queries.

**The queries, written out.**
Each is decided here rather than left to the implementer,
because the grouping and the empty-denominator behavior are the part a reader gets wrong,
and a query left to the implementer is a query nothing reviewed.

*The interactive path.*

| Question | Query |
|---|---|
| Request rate by route family | `sum by (endpoint) (rate(profgate_requests_total[5m]))` |
| Share of requests that are not `ok` | `sum(rate(profgate_requests_total{code!="ok"}[5m])) / sum(rate(profgate_requests_total[5m]))` |
| 95th percentile profile fetch | `histogram_quantile(0.95, sum by (le, profile) (rate(profgate_request_duration_seconds_bucket[5m])))` |
| Profile fetches in flight, fleet-wide | `sum(profgate_profiles_in_flight)` |
| The ten refusals with the most volume | `topk(10, sum by (code) (rate(profgate_requests_total{code!="ok"}[1h])))` |

*The certificate.*

| Question | Query |
|---|---|
| Days left on each replica's served certificate | `(profgate_tls_certificate_expiry_seconds - time()) / 86400` |
| Re-read outcomes by result | `sum by (result) (rate(profgate_tls_reloads_total[15m]))` |

*Authentication.*

| Question | Query |
|---|---|
| Failures by reason | `sum by (reason) (rate(profgate_auth_failures_total[5m]))` |
| Signing key age per replica | `profgate_oidc_jwks_age_seconds` |
| Browser sessions minted | `sum(rate(profgate_auth_sessions_issued_total[1h]))` |

*Readiness.*

| Question | Query |
|---|---|
| Replicas that have completed their initial sync | `count(profgate_discovery_synced == 1) or vector(0)` |
| Replicas exporting the gauge at all, which is the denominator for the row above | `count(profgate_discovery_synced)` |
| Replicas with no NATS connection | `count(profgate_nats_connected == 0) or vector(0)` |
| Replicas whose PGO caches have not replayed | `count(profgate_pgo_synced == 0) or vector(0)` |

*Collection.*

| Question | Query |
|---|---|
| Collections by terminal result | `sum by (result) (rate(profgate_collections_total[1h]))` |
| Scheduling outcomes | `sum by (result) (rate(profgate_schedule_slots_total[1h]))` |
| Collections running now | `sum(profgate_collections_active)` |
| Sample outcomes | `sum by (result) (rate(profgate_collection_samples_total[15m]))` |
| 95th percentile Collection duration | `histogram_quantile(0.95, sum by (le) (rate(profgate_collection_duration_seconds_bucket[1h])))` |

**The grouping and denominator rules the page states once.**
These are the sentences that make the table above readable rather than copyable:

- Every series the gateway exports is per replica, because every one is scraped per Pod.
  A query with no aggregation answers per replica, which is what the certificate and key-age rows want;
  `sum(...)` or `sum by (<label>)` answers for the fleet, which is what every rate row wants.
  Drop the `sum` to see which replica a number came from.
- A ratio whose denominator is zero yields no sample rather than `0`.
  The error-share query is blank on a gateway nobody is calling, which is a gap on a graph and not a healthy zero;
  read it beside the request-rate query rather than alone.
- `histogram_quantile` needs the `le` label,
  so a histogram is always summed *by* `le` alongside whatever else the row groups by.
  Aggregating a `_bucket` series without `le` produces a number that means nothing.
- `count(<gauge> == 0)` produces no sample when nothing matches, which reads as a gap rather than as zero.
  `or vector(0)` is what turns "nothing is broken" into a zero a panel can draw,
  and it is why the readiness rows carry it.
- `code!="ok"` counts the `upstream_<status>` values,
  which are a target's own answers passed through and not the gateway refusing anything.
- `profgate_confirm_total` and `profgate_profiles_in_flight` count the interactive path alone,
  which the metric table now says too.
- The `endpoint` label is the one label a scrape can take away.
  The chart's `PodMonitor` drops the target label of that name so the gateway's value keeps it,
  and the first query is written for that install.
  A scrape configured by hand that attaches its own `endpoint` target label,
  with `honorLabels` unset, moves the gateway's value to `exported_endpoint` there,
  and the first query is the only one on the page that reads it.
  The page says this once, next to that query.

- [x] **Write the test**

`deploy/testdata/example-queries.yaml` is a Prometheus rule file holding one recording rule per query above.
`promtool check rules` parses every `expr` in it, so a typo, an unbalanced bracket,
or a function that does not exist fails the suite instead of reaching a reader who copies it.
A new subtest in `deploy/chart_test.go` runs that check and then asserts
that the set of expressions in the file and the set inside the query tables of `docs/deployment.md` are equal,
reading the document's fenced values out of the table cells.
That second half is what keeps the two together:
a query edited in the guide and not in the fixture fails, and so does the reverse.
`promtoolBin` skips when the binary is absent, the same way the alert fixture does.

The three queries whose behavior is load-bearing rather than syntactic —
the error share with a zero denominator,
the latency quantile's `le` grouping,
and `count(... == 0) or vector(0)` with nothing matching —
also get cases in `deploy/testdata/alerts_test.yaml` as `promql_expr_test` entries,
so the sentences above are evaluated rather than asserted.

The red state:

```bash
go test ./deploy/ -run 'TestChartExampleQueries'
```

The fixture does not exist, so the subtest fails on the missing file.

- [x] **Write the page**

- [x] **Validate and commit**

`CHANGELOG.md`, `### Added`:
**The deployment guide has a page of example queries.**
One per question an operator asks of the metrics — request rate, error share, latency, refusals,
certificate lifetime, authentication failures, readiness across replicas, and collection outcomes —
so a dashboard starts from a query that already names the right series and labels.
Every one of them is parsed by `promtool` in the test suite.

```bash
semlf check docs/deployment.md deploy/chart_test.go CHANGELOG.md
mise run lint && mise run test && mise run check
git add docs/deployment.md deploy/chart_test.go \
  deploy/testdata/example-queries.yaml deploy/testdata/alerts_test.yaml CHANGELOG.md
git commit -m "docs: add example metric queries" -m "<body: one query per question an operator asks, over the series the table above lists; promtool parses every one; no dashboard file, which the roadmap puts out of scope>"
```

---

## The roadmap item records where the work went

Closes the plan.

**Files:**
- Modify: `docs/plans/roadmap.md`, `docs/plans/metrics-and-runbook.md`

The log-line task's commit is the last one that carries code or its own tests,
so the end-to-end suite runs on it, before the pull request opens;
the three documentation commits after it and the two below add no code for that suite to cover.
The pull request opens once the queries commit is pushed,
and its number is what this commit writes to the roadmap.

In it:
tick all eight bullets of the *Make the gauges and the alerts true, and write the runbook* item in
[`roadmap.md`](roadmap.md);
set its `Shipped:` line, today `Shipped: not built yet.`, to `Shipped: pull request #<N>`,
the number of the pull request opened above;
extend its `Spec:` line,
which today names a revision for the expiry seed and the discovery gauge and none for the rest,
to name the third this work rests on:
the three cases `profgate_nats_connected` reports, in [`gateway.md`](../specs/gateway.md) *Metrics*;
set line 3 of this file to `**Status:** Done`;
and insert `**Outcome:**` as line 4, naming the pull request and not a commit:

```text
**Outcome:** pull request #<N> carries the two seeds, the six rules, the three log attributes, and the runbook; the commit that carries this line closes the plan.
```

The pull request is named rather than a commit because the merge rebases this branch onto `main`
and rewrites every hash on it.
The last merged pull request is the evidence: its own commits carry
`fe1292a` and `3b994a6`, and the same two commits sit on `main` as `6f8373b` and `958703b`,
message for message.
So a hash written into this file while the branch is being written names nothing after the merge,
while the pull request number is the same before and after.
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
admits a pull request on that line for exactly this reason,
and `check_status` in [`check-repo.py`](../../scripts/check-repo.py)
requires `**Outcome:** ` followed by text on line 4 and nothing more.

The merge keeps both of the commits below, which is why the two-commit protocol still works here.
The same rebase that rewrote those hashes preserved both commits as their own:
the one whose tree holds the finished plan under `check_status`,
and the one that deletes it.
The deletion is the next commit and has to be a separate one:
the tree a commit writes either holds this finished plan or does not,
which is the protocol
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records.
That commit deletes this file and rewrites every link that cited it, which `check_links` enforces;
it changes nothing else.
Run `grep -rn metrics-and-runbook --include='*.md' .` before the deletion to find the links.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/roadmap.md docs/plans/metrics-and-runbook.md
mise run lint && mise run test && mise run check
git add docs/plans/roadmap.md docs/plans/metrics-and-runbook.md
git commit -m "docs: close the metrics and runbook plan" -m "<body: the item's eight bullets are done and its Shipped line names the pull request; the plan is Done>"
```

---

## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite:

```bash
mise run test:e2e
```

It is required, and for two named reasons.
[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
lists `internal/pgo` and `deploy/` among the eight packages
that need the suite on the `current` lane before a pull request:
the log-line task changes `internal/pgo/rounds.go`,
and the scrape task and the alert task change the chart and `deploy/chart_test.go`.
`internal/httpapi`, `internal/metrics`, and `cmd/profgate` are not on that list,
so the two seeds, the write before the first dial,
and the two handler log attributes would not have required it on their own.
What the suite proves here is narrow and worth stating:
it deploys the chart and the kustomize overlays, so a template that renders invalid YAML fails it,
and it runs the PGO scenarios against a real NATS server, so the sample record's new attribute is written under load.
It does not evaluate any alert expression:
the `promtool` fixture in `deploy/` does that, on every `mise run test`, without a cluster.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **[`pgo.md`](../specs/pgo.md) still describes a gauge and an alert that no build carries.**
  `profgate_pgo_collector_available` is named by no Go file,
  and the chart renders one PGO alert rather than the two that spec's *Metrics* section designs.
  The design is right and unchanged; what changed is that the spec now says which of it this build carries
  (`docs/specs/pgo.md:2927-2928`, `:2960-2961`),
  so a reader is no longer left to discover it against the tree.
  Closing the gap is the deferred collector Deployment's work, not one of the six failure modes, and stays out of scope.
  The metric-name loop in `TestChartPrometheusRule` (`deploy/chart_test.go:1105-1116`)
  is what stops anyone shipping a rule over that gauge in the meantime.
- **`ProfgateUpstreamsUnreachable` is silent on a gateway nobody is asking for profiles.**
  Both sides of its expression are rates over profile requests,
  so an outage that starts during a quiet period is not alerted until traffic returns.
  That is the deliberate trade: the alternative fires on every idle gateway.
  Nothing here alerts on the absence of profile traffic, and nothing should:
  a gateway with no callers is the normal state.
- **A scrape configured by hand can still take the gateway's `endpoint` label away.**
  The chart's own `PodMonitor` no longer does: it drops the target label of that name before the scrape.
  A Prometheus that reaches these Pods from its own scrape configuration,
  or from a `PodMonitor` or `ServiceMonitor` written outside this chart,
  attaches whatever target labels that configuration writes,
  and with `honorLabels` unset an `endpoint` among them moves the gateway's value to `exported_endpoint`.
  Nothing in the chart can reach that configuration, and this plan does not try.
  What it costs is the upstream rule's success side,
  where `endpoint!~"collection.*"` stops excluding Collection reads,
  so a replica answering Collection routes while every profile fetch fails does not raise the alert there;
  no rule's firing condition depends on the label, which is why the rest are unaffected.
  The query page says which query this affects and what to write instead.
- **Five of the six new rules are inert on some installs, by construction.**
  The two TLS rules need `server.tls`, the limiter rule needs `auth.mode: basic`,
  the NATS rule needs `pgo.enabled`, and the upstream rule needs traffic.
  Each says so in its own description, which is where an operator looking for coverage will read it.
  Nothing renders a rule conditionally on `auth.mode` or `server.tls`,
  and nothing should: those two are inert because their series are absent or `NaN`,
  which is a stronger guarantee than a render-time branch.
- **The troubleshooting sections are documentation with no test behind them.**
  No test in this repository reads them —
  the only test that parses a Markdown document reads the NATS permission table of
  [`pgo.md`](../specs/pgo.md) (`deploy/deploy_test.go:668`).
  A code, a metric name, or a label that changes leaves them stale, and only a reader will notice.
  The mitigation is that every claim in them cites the accepted spec paragraph it restates,
  so a reader who doubts a row has one place to check it.
  The query page is the exception: `promtool` parses every expression on it
  and a subtest asserts the guide and the fixture hold the same set,
  so a query cannot silently stop being valid PromQL.
  What that does not check is whether a label a query groups by exists on the series it reads;
  that is still review against the metric table.
- **The collector row the accepted spec describes is not in the runbook.**
  [`pgo.md`](../specs/pgo.md) *Failure modes* documents `503 collector_unavailable`
  and `profgate_pgo_collector_available` (`docs/specs/pgo.md:3721`),
  and this build answers neither.
  An operator reading only the spec will look for both;
  the runbook does not repeat them, so the guide and the binary agree and the guide and the spec do not.
  That gap closes when the collector split lands, not here.
- **The `code` label documentation points at the specs rather than enumerating.**
  A reader who wants the forty has to open two documents or `/v1/openapi.json`.
  That is the trade for a list that cannot drift;
  `TestEnvelopeCodesMatchTheErrorTables` is what keeps the pointed-at tables true.

---

## Self-Review

- Bullet coverage, one line each:
  the absent `profgate_pgo_synced` during a NATS outage and the rule that covers it
  (*Six failure modes each have a rule that fires in them*, which also carries the six);
  the expiry gauge's `NaN` seed and its test (*The expiry gauge reports nothing until a certificate is loaded*);
  the `ProfgateNotReady` wording, the gauge's own `HELP` string, and its two documented rows
  (*The readiness alert and the gauge's own text say what the gauge measures*);
  the troubleshooting sections, the bucket, the restore, the maintenance, and the cost of a disconnect
  (*An operator on call has a page that says what to look at*);
  the `code` label's value set and the interactive-path scope of two metrics
  (*The metrics reference names every `code` value and the path two metrics watch*);
  the three log lines (*Three log lines name the work they belong to*);
  the example queries (*A dashboard starts from queries that already work*).
  Two tasks close no bullet of their own:
  *The gateway's `endpoint` label reaches a query under the name it exports*,
  which makes the label the rules and the queries use mean anything on an install the chart builds;
  and *The connection gauge reports nothing where no connection is made*,
  without which the rule the first bullet asks for is silent in the outage that bullet names.
- Current-source facts this plan rests on, each confirmed by reading the file:
  `profgate_tls_certificate_expiry_seconds` is constructed at `internal/metrics/prometheus.go:103`
  and registered at `:154` with everything else,
  and its only writer is `internal/tlscert/loader.go:150`, reached from `Loader.apply` alone,
  whose loader is constructed only at `cmd/profgate/serve.go:245` under `cfg.Server.TLS.Enabled()`;
  `profgate_oidc_jwks_age_seconds` is a `GaugeFunc` returning `math.NaN()` before the first fetch
  (`internal/metrics/prometheus.go:139-149`), a different mechanism from the seed this plan adds;
  `TestPrometheus_TLS` writes to both series before asserting and so cannot pin the initial state;
  `TestPrometheus_PGOSyncedFrom` uses `testutil.GatherAndCount` at `internal/metrics/prometheus_test.go:109`;
  `deps.recorder.DiscoverySynced(true)` runs once, at `cmd/profgate/serve.go:475`,
  and `ready()` at `:172-174` reads four gates;
  `profgate_nats_connected` is constructed at `internal/metrics/prometheus.go:95` and registered at `:154`,
  and `NATSConnected` (`:236-242`) is its only writer today,
  reached from the callback wired at `cmd/profgate/serve.go:590`, in the PGO options,
  which `internal/natskv/preflight.go:52-53` fires only after the initial connection succeeds —
  the contract `internal/natskv/natskv.go:128-133` states —
  so it runs before the bucket checks and not at all during an outage at startup;
  `TestPrometheus_NATSConnected` pins the gauge's `HELP` string in an exposition
  (`internal/metrics/prometheus_test.go:255`) after recording `NATSConnected(true)`,
  `TestServePGO/the connected gauge reports the initial connection` asserts the call sequence is `[true]`
  (`cmd/profgate/serve_test.go:1742-1751`),
  and `TestNATSCallbacksSplitTheirDuties` (`:1566-1621`) drives the real `natsPreflight` with a stub
  that invokes no connection callback, so it counts calls that a write before the loop moves;
  the NATS preflight retries `ErrUnavailable` forever in the loop at `cmd/profgate/serve.go:597-614`,
  and returns every other failure for `cmd/profgate/serve.go:477-486` to end startup with;
  `profgate_requests_total` carries `endpoint`, `profile`, and `code` and nothing else the gateway sets,
  and `labels()` (`internal/httpapi/server.go:256-264`) returns `("profile","none")` for a request that resolved no route,
  so `endpoint="profile"` is not a proxy attempt;
  `upstream_unreachable` and `upstream_timeout` are minted in `internal/proxy` (`internal/proxy/proxy.go:33-34`)
  and reach `profgate_requests_total` only through `internal/httpapi/server.go:750-751`;
  `Loader.refresh` records `failed`, `unchanged`, or `applied` on every pass (`internal/tlscert/loader.go:127-150`);
  `auth_unavailable` is the code of every `503` authentication failure,
  from `internal/httpapi/auth.go:131-133` and `internal/auth/browser.go:416-425`,
  and [`auth.md`](../specs/auth.md) maps `exchange`, `keys_stale`, `entropy`, and `internal` to that status
  (`docs/specs/auth.md:763`);
  `pgo.leaseTTL` admits `30s` to `10m` and `pgo.maxAttempts` admits `1` to `10`
  (`internal/config/config.go:345-346`);
  `mise registry` resolves `promtool` to `aqua:prometheus/prometheus`,
  and `deploy/chart_test.go:34-40` is the skip a missing pinned binary already takes;
  `deploy/chart/profgate/templates/podmonitor.yaml:23-26` sets `port: ops`, no `honorLabels`, and no relabeling,
  `deploy/base/` renders no `PodMonitor` at all,
  prometheus-operator's `generatePodMonitorConfig` writes `target_label: endpoint` from the port name
  and appends the endpoint's own `relabelings` after it,
  Prometheus renames a displaced exposed label to `exported_<name>` in `mutateSampleLabels`,
  which runs before `metric_relabel_configs`,
  `PopulateLabels` runs the job's `relabel_configs` to produce the target's labels,
  and a relabeling regex is anchored, so `labeldrop` with `regex: endpoint` matches that name alone
  and removes nothing where no such label is present;
  the only label of this gateway's whose name a `PodMonitor` target also carries is `endpoint`:
  the exported set is `endpoint`, `profile`, `code`, `result`, `kind`, `mode`, `reason`, `file`,
  `fingerprint`, and `role`, and the target set is `namespace`, `container`, `pod`, `endpoint`,
  `job`, and `instance`;
  `PGOSyncedFrom` registers `profgate_pgo_synced` from inside `startPGO` (`:668`),
  so [`pgo.md`](../specs/pgo.md)'s "exists only when `pgo.enabled`" is true of that gauge;
  `profgate_pgo_collector_available` appears in no Go file;
  `prometheusrule.yaml` ships four alerts, the fourth inside the `pgoEnabled` guard at `:54`;
  `TestChartPrometheusRule` compares alert names with `slices.Equal`,
  requires a `for`, a `severity`, a `summary`, and a `description` on each,
  and reads `internal/metrics/prometheus.go` for every metric name in every expression;
  the only other place the readiness alert's claim is repeated is `deploy/chart/profgate/README.md:488`,
  while `values.yaml:231` and `docs/deployment.md:488` name the alert without claiming anything about it;
  `internal/httpapi/codes.go` holds forty constants and a registry of forty,
  `TestEnvelopeCodesMatchTheErrorTables` compares that registry against the two error tables written out,
  the eight audit-only outcomes are the inventory of `TestAuditOnlyCodesAreNotRegistered`
  (`internal/httpapi/codes_test.go:71-73`), one more than the comment at `internal/httpapi/codes.go:100-104` names,
  and `upstream_<status>` is minted at `internal/proxy/proxy.go:171`;
  `Recorder.Confirm` and `ProfilesInFlight` are called only from `internal/httpapi/server.go:710-738`,
  while `internal/pgo/rounds.go:448` calls the Kubernetes seam's `Confirm` and records nothing;
  the three log lines are at `internal/pgo/rounds.go:412`, `internal/httpapi/auth.go:86`,
  and `internal/httpapi/pgo_collections.go:633`,
  `recordSample`'s only caller is `absorb` (`:389`) which holds `state.man.Collection`,
  `authenticate` already takes the `*request` whose `q.audit.requestID` is the audit key,
  and `lookupReceipt` is called from `internal/httpapi/pgo_collections.go:511` and `:714`;
  `auth.basic.maxConcurrent` defaults to `16` (`internal/config/config.go:257`)
  and `ReasonThrottled` is produced only by `internal/auth/basic.go:138`;
  every commit header above is under 50 characters.
- Roadmap claims the code contradicts, each carried by the task that owns it:
  the first bullet proposes the NATS rule "without a code change",
  and the item's `Spec:` line declares no revision for anything but the expiry seed and the discovery gauge,
  where the connection gauge needs both;
  *Decisions* settles that and the closing task writes it back;
  every line number in the item's bullets has drifted,
  most by a few lines and the failure-scenario reference by about fifty,
  because the accepted spec revision added lines above them —
  this plan cites what it read, not what the bullets say.
- Decided here, with the reason stated at the task or the decision that carries it:
  the NATS gauge reporting three cases — `NaN` where no connection is made, an explicit `0` from before the first dial,
  and the connection state after that — with the `0` written in `cmd/profgate` rather than in `internal/natskv`,
  and the chart's `pgoEnabled` guard on its rule kept so the rendered alert order does not churn;
  the readiness alert's wording narrowed rather than the gauge widened, which the spec had already chosen;
  the upstream rule written as failures without successes on one replica, rather than a fleet-wide ratio;
  the chart's `PodMonitor` dropping the operator's `endpoint` target label in `relabelings`,
  rather than setting `honorLabels` or renaming the gateway's own label,
  and not in `metricRelabelings`, which runs after the rename and would leave the value in `exported_endpoint`;
  no shipped rule's firing condition selecting on the `endpoint` label,
  since a scrape configured outside the chart can still rename it;
  the NATS rule at five minutes,
  with the cost to owned Collections stated as a function of `pgo.leaseTTL` rather than as a fixed consequence;
  the authentication-unavailable rule at `warning`,
  because its expression cannot tell a total outage from a token-endpoint one,
  and overlapping the stale-keys rule deliberately;
  the reload rule written as a share of all reload outcomes, per replica, so a broken replica is named;
  the alert expressions evaluated by a pinned `promtool` rather than by a new Go module,
  with what the fixture proves and what stays with review written out at the task;
  the example queries written into the plan and parsed by the same tool;
  the shipped alert order kept append-only so the existing assertion holds;
  the forty envelope codes pointed at rather than retyped, and the eight audit-only outcomes written out;
  the query page written as queries and no dashboard file;
  the plan closed in two commits, `Outcome:` naming the pull request rather than a commit,
  because the merge rebases this branch and rewrites every hash on it.
- Left to the implementer: the exact prose of both troubleshooting sections and the query page,
  the fixture and subtest names where a task does not fix them,
  the parameter shape `lookupReceipt` grows,
  and the wording of every commit body.
