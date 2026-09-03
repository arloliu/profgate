# `config validate` Tells the Truth

**Status:** Done
**Outcome:** commits `d274f99` through `c2a773e` on `docs/plan-config-validate` carry the repairs; the commit that carries this line corrects the example.

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.

**Goal:** make the one command an operator runs before a rollout agree with the gateway it describes,
and make every refusal on the way name the file, the key, and what the key accepts.
`profgate config validate` prints the container limit the configuration in front of it needs,
which for a gateway with collection off is the gateway's own footprint and not a merge budget it will never spend,
and it says which of the two it printed.
A key the schema does not define is refused by its key path in the file it was written in, never by a Go type name.
`discovery.pprof.port: 0` is refused rather than read as "unset".
Each of the eight `PROFGATE_AUTH_OIDC_*` variables of `auth.oidc.browser` and `auth.oidc.cli` is refused whenever its block is absent,
in every `auth.mode`, and never dropped.
A realm that names a profile wrong is told the eight names it may choose from.
The reference documents the four places a shipped value already sits on its own ceiling,
and its complete example is the set of defaults it claims to be.

**Architecture:** one function grows a branch, `internal/config.Config.GatewayMemoryBytes`;
`Load` grows three refusals of the shape it already carries for the removed pprof keys;
the decoder's error is rewritten by a new unexported helper in the same package;
and one realm message gains the list it was missing.
No package, route, chart value, or Kubernetes permission moves.
The Helm chart's templates are untouched:
`profgate.resources` in `deploy/chart/profgate/templates/_helpers.tpl:295-309` already branches on `pgo.enabled`,
so this work brings the binary to the chart rather than the other way round.
The kustomize base's memory figure moves, its Deployment and ConfigMap comments move with it,
and the end-to-end harness gains the patch that raises the figure back for the one gateway it runs with collection on.

**Spec:** no spec revision is needed; this work conforms to accepted text in three places.
The configuration table in [`gateway.md`](../specs/gateway.md) gives `discovery.pprof.port` as `1–65535`
(`docs/specs/gateway.md:2145`), so refusing `0` is the code catching up with the spec.
[`pgo.md`](../specs/pgo.md) describes what `config validate` prints once the collector Deployment exists
(`docs/specs/pgo.md:2798-2800`) and gives a gateway replica a static limit no `pgo.limits` key enters (`:413-419`);
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md) defers that Deployment (`:3-6`)
and sizes the in-process collector as the gateway's footprint plus the working set (`:91-92`, `:99-101`),
which is why the memory branch below describes the collector as it exists today:
a replica that collects is sized for collecting, and one that does not is sized as the footprint alone.
The memory rule it repairs is stated in `docs/deployment.md:396-398`
and in `deploy/chart/profgate/README.md:126-129`,
and the error-message promise it repairs is stated in `docs/configuration.md:14-17`.
This work is ordered by [`roadmap.md`](roadmap.md),
under *Make `config validate` tell the truth*,
and the evidence behind each task is in
[`2026-09-03-usability-and-stability.md`](../investigations/2026-09-03-usability-and-stability.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Decisions

Two choices in this plan overturn something deliberate or add output nothing asked for.
Both are settled.

**The kustomize base drops from `1536Mi` to `512Mi`.**
`deploy/base/deployment.yaml:55-60` carries a comment that justifies the larger figure:
the base ships collection off but reserves what the ConfigMap's commented-out PGO block would need,
so an operator who uncomments that block needs no second edit.
Once `GatewayMemoryBytes` reads `pgo.enabled`,
`TestDeploymentMemoryLimit` (`deploy/deploy_test.go:882-909`) computes `512Mi` from the base's own ConfigMap
and the base's `1536Mi` fails it.
The base moves to `512Mi`,
because the plan that landed the comment expressly left "the question of what the base should reserve" to this item,
and because a base that reserves three times what it uses teaches the wrong figure to anyone who copies it.
The cost is one extra edit on the PGO-enablement path,
which the ConfigMap's own comment names, with the figure.
The alternative — keep `1536Mi` and loosen `TestDeploymentMemoryLimit` so it no longer asserts equality —
is rejected: it trades a checked invariant for a comment.
The one place in this repository that walks the enablement path is the end-to-end harness:
`deployGateway` (`test/e2e/harness_test.go:492-503`) applies a configuration with `pgo.enabled: true` to the `default` overlay,
which inherits the base Deployment,
and no other overlay's Deployment carries a memory limit at all.
So the harness gains the patch that path needs, in the memory task below,
which keeps the Deployment the suite runs in agreement with what `config validate` prints for its ConfigMap.
The binary reads no cgroup limit, so nothing refuses to start under a smaller figure;
the patch is what makes the suite an instance of the documented step rather than an exception to it.

**`config validate` names the collection state in its output.**
With collection off the container figure changes meaning, and the printed lines do not say so.
The command prints `pgo collection: disabled` in place of the working-set line:

```console
$ profgate config validate --config /etc/profgate/config.yaml
required terminationGracePeriodSeconds: 125
pgo collection: disabled
container memory bytes: 536870912
```

The enabled output is unchanged.
The alternative was to print the two lines alone and let the reader infer the branch;
it would leave a figure that silently means two different things —
close to the defect this item exists to fix.

---

## Global Constraints

- **No new configuration key, route, chart value, or Kubernetes permission.**
  Every refusal added here names a key that exists today.
  `internal/k8s` is not touched and the ClusterRole does not move.
- **`PGOMemoryBytes` does not change.**
  It is the ceiling arithmetic, and `profgate.pgoMemoryBytes`
  (`deploy/chart/profgate/templates/_helpers.tpl:234-245`) is its mirror in the chart.
  Only `GatewayMemoryBytes`, which turns that arithmetic into a container figure, learns about `pgo.enabled`.
  `TestChartMemoryLimitIsDerived` and every other chart test that renders with PGO on keeps asserting exactly what it asserts today.
- **Four changes to released behavior, each recorded under `Unreleased` in `CHANGELOG.md`.**
  `v0.5.0` is a tagged, published release (`CHANGELOG.md:51`, and a GitHub Release carries the tag),
  so every change below is a change to behavior an installation may already depend on.
  Two narrow the admitted configuration set and are marked breaking under `### Changed`:
  `discovery.pprof.port: 0`, and the eight environment variables whose block is absent.
  One changes what `config validate` writes to stdout with collection off and goes under `### Changed`, marked breaking,
  because a script that reads the second line by position reads a different line.
  One changes a deployment default, the kustomize base's memory figure, and goes under `### Changed` as well.
  None gets a compatibility shim or a deprecation window; the repository does not design around a breaking change.
  The two message repairs go under `### Fixed`.
- **Every code task writes its test first and shows it red before the fix.**
  The five tasks that change Go code each name the test and the exact command that shows it red,
  so an implementer who sees a green test knows the fixture is wrong rather than the code being right
  ([`000-agent-contract.md`](../../.agents/rules/000-agent-contract.md)).
  The two documentation tasks add no behavior and have no red state to show.
- **Every refusal has one exact message.**
  Each task that adds or changes a message gives the template it prints,
  so two implementers produce the same output and the tests can assert it whole.
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
internal/config/config.go            # GatewayMemoryBytes branches; Load gains three refusals; the profiles message
internal/config/config_test.go       # the PGO-off figure, the port refusals, the dropped overrides, the decode messages, two ceiling cases
cmd/profgate/main.go                 # what config validate prints with collection off
cmd/profgate/main_test.go            # the exact stdout of both branches
cmd/profgate/testdata/good-pgo.yaml  # the PGO-enabled command fixture
cmd/profgate/testdata/nats.creds     # the credentials file that fixture names
deploy/base/deployment.yaml          # the memory figure the base ships, and the comment that explains it
deploy/base/configmap.yaml           # turning collection on names the second edit it needs
deploy/deploy_test.go                # TestDeploymentMemoryLimit's doc comment
deploy/chart_test.go                 # the base-term test stops subtracting the working set
test/e2e/harness_test.go             # the patch that sizes the PGO-enabled default gateway
docs/configuration.md                # the port range, the pointer-block rule, the error shape, the ceiling pairs, the example
docs/deployment.md                   # what the base reserves
docs/pgo.md                          # what config validate prints
deploy/chart/profgate/README.md      # the binary and the chart now agree on both branches
CHANGELOG.md                         # four changed lines and two fixes
docs/plans/roadmap.md                # the item's checkboxes and its Shipped line
docs/plans/config-validate.md        # this file
```

---

## The container is sized for the collection it does

Closes the roadmap bullet beginning
*`GatewayMemoryBytes` never reads `pgo.enabled`*.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `cmd/profgate/main.go`, `cmd/profgate/main_test.go`,
  `deploy/base/deployment.yaml`, `deploy/base/configmap.yaml`,
  `deploy/deploy_test.go`, `deploy/chart_test.go`, `test/e2e/harness_test.go`,
  `docs/configuration.md`, `docs/deployment.md`, `docs/pgo.md`,
  `deploy/chart/profgate/README.md`, `CHANGELOG.md`
- Create: `cmd/profgate/testdata/good-pgo.yaml`, `cmd/profgate/testdata/nats.creds`

**The decision, and why.**
`GatewayMemoryBytes` (`internal/config/config.go:548-553`) returns `PGOGatewayBaseMemory + c.PGOMemoryBytes()` unconditionally.
`PGOMemoryBytes` reads `pgo.limits`, which carry their defaults whether or not collection is on,
so a gateway with `pgo.enabled: false` is told it needs `1536Mi` where it needs `512Mi`.
`cmd/profgate/testdata/good.yaml` has no `pgo` block at all
and `config validate` prints `container memory bytes: 1610612736` for it today.
The documents already say the other thing:
`docs/configuration.md:437` — "With `pgo.enabled: false` the container limit is that term alone" —
and `docs/deployment.md:396-398`.
The chart already does the other thing:
`profgate.resources` renders `memoryLimitWithoutPGO` alone on the PGO-off branch.
The code is the bug.

The branch goes in `GatewayMemoryBytes` and nowhere else:

```go
func (c *Config) GatewayMemoryBytes() int64 {
	if !c.PGO.Enabled {
		return PGOGatewayBaseMemory
	}

	return PGOGatewayBaseMemory + c.PGOMemoryBytes()
}
```

`PGOMemoryBytes` stays as it is, for the reason the constraints give:
it is the ceiling arithmetic the chart mirrors, and it answers a question that does not depend on the flag.
Its doc comment already says so — "the PGO working set at the configured ceilings, and nothing else".
`GatewayMemoryBytes`'s comment has to change with the code:
it currently says "the container memory limit a collecting gateway needs", which is now half of what it answers.

**What the printed output becomes.**
`runConfig` (`cmd/profgate/main.go:81-83`) prints the working-set line only when `cfg.PGO.Enabled`,
and prints `pgo collection: disabled` in its place otherwise.
The enabled output keeps its three lines, byte for byte.

**What the harness needs.**
`gatewayConfig` (`test/e2e/harness_test.go:837`) writes `pgo.enabled: true` whenever it is given a NATS URL,
and sets `pgo.limits.minEvery`;
`PGOMemoryBytes` (`internal/config/config.go:541-546`) reads only `maxParallel`, `maxSampleBytes`, `maxMergedBytes`,
and `maxActiveCollections`,
none of which the harness sets,
so all four stay at their shipped defaults and the figure below holds.
`deployGateway` (`:492-503`) applies that configuration to the `default` overlay,
which is the base Deployment with the image swapped.
With the base at `512Mi` that gateway would run a configuration `config validate` sizes at `1536Mi`.
A new `memoryLimitPatch(deployment, limit string) patch`, beside `credsMountPatch` (`:915`) and `configPatch` (`:941`),
is applied in `deployGateway` with the ConfigMap patch,
raising the `profgate` container's `resources.limits.memory` to `1536Mi`;
its doc comment says the figure is what `profgate config validate` prints for the configuration the harness writes,
and that the base ships collection off at `512Mi`.
No other overlay inherits the base,
and no other overlay's Deployment sets a memory limit,
so the patch applies to one gateway.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestGatewayMemoryWithCollectionOff`, new, in `internal/config/config_test.go` beside `TestPGOSizing` (`:906`) | loads `pgo-full.yaml` under `t.Setenv("PROFGATE_PGO_ENABLED", "false")` and asserts `GatewayMemoryBytes()` is `512<<20` while `PGOMemoryBytes()` is still `1<<30`; today the first is `1536<<20`, so the flag alone is proven to move the container figure and to leave the ceiling arithmetic alone |
| `TestRun`, subtest `validate good` (`cmd/profgate/main_test.go:27-36`) | `wantStdoutExact` becomes the three lines the PGO-off branch prints, `required terminationGracePeriodSeconds: 125`, `pgo collection: disabled`, `container memory bytes: 536870912`; today the process prints the working-set line and `1610612736`, and the subtest fails on the exact comparison |
| `TestRun`, new subtest `validate good with collection on` | loads `testdata/good-pgo.yaml` — `good.yaml` plus a `nats` block naming `testdata/nats.creds` and `pgo: {enabled: true}` — and asserts the exact three lines `required terminationGracePeriodSeconds: 125`, `pgo working set bytes: 1073741824`, `container memory bytes: 1610612736`. It is green today and is the test that holds the enabled output still while the disabled one moves; `nats.url` is required with `pgo.enabled` (`internal/config/config.go:1183`) and `nats.credsFile` is opened at load (`:1191-1194`), so the fixture needs both and the creds file is a copy of `internal/config/testdata/nats.creds` |
| `TestDeploymentMemoryLimit` (`deploy/deploy_test.go:882-909`) | needs no edit: it computes the figure from the base's own ConfigMap, which ships collection off, so it turns red against the base's `1536Mi` the moment the branch lands. Its doc comment (`:878-881`) gains a clause saying the base ships collection off, so the figure it demands is the base term alone until the ConfigMap's PGO block is uncommented |
| `TestChartBaseTermIsTheSameFigureBothWays` (`deploy/chart_test.go:623-633`) | drops the `cfg.GatewayMemoryBytes() - cfg.PGOMemoryBytes()` subtraction: it loads the rendered configuration with no PGO values and compares the rendered `512Mi` against `cfg.GatewayMemoryBytes()` directly. Written that way it fails today, because a PGO-off configuration returns `1536Mi`, which is what makes it a test of the branch rather than of arithmetic |

The subtraction is the workaround the roadmap names,
and removing it is the point: with the branch in place the two sides are the same call on the same configuration,
so the test stops proving that two formulas agree and starts proving that the chart and the binary do.
Confirm `loadRenderedConfig(t)` with no values loads a PGO-off ConfigMap before rewriting the test;
`deploy/chart_test.go:257` is the helper.

The red state, before any production line moves:

```bash
go test ./internal/config/ -run 'TestGatewayMemoryWithCollectionOff'
go test ./cmd/profgate/ -run 'TestRun/validate_good'
go test ./deploy/ -run 'TestChartBaseTermIsTheSameFigureBothWays'
```

All three fail on the `1536Mi` figure.
`TestDeploymentMemoryLimit` goes red only after the branch lands, and green again when the base moves.

- [ ] **Branch the function and the printed output**

- [ ] **Move the base's figure, the comments that explain it, and the harness**

| File | Change |
|---|---|
| `deploy/base/deployment.yaml` | `memory: 512Mi`; the comment at `:55-59` is rewritten: collection is off and this is the gateway's own footprint, and turning collection on means raising this figure to what `profgate config validate` then prints, `1536Mi` at the shipped ceilings |
| `deploy/base/configmap.yaml` | the PGO paragraph's closing sentence (`:60-62`) names the second edit: uncommenting the block also means raising `deployment.yaml`'s memory limit to `1536Mi`, the `1Gi` working set the shipped ceilings size over the gateway's own `512Mi`, which is the figure `profgate config validate` prints for the uncommented file |
| `test/e2e/harness_test.go` | `memoryLimitPatch` as described above, applied in `deployGateway` |
| `docs/deployment.md`, *Resources* | the sentence at `:400-401` about the base reserving the PGO-enabled limit is replaced: the base ships collection off and reserves `512Mi`, and turning collection on means raising it to the figure `config validate` prints |
| `docs/configuration.md`, *`profgate config validate`* | the sample at `:579-583` and the sentence introducing it are re-sourced from a PGO-off file, so the sample shows the `pgo collection: disabled` line; the bullet describing the two figures says the working-set line appears with collection on and the `disabled` line otherwise; the sizing text at `:432` says which branch each figure belongs to |
| `docs/pgo.md:354` | "prints both figures" is qualified: the working set is printed when collection is on, and `pgo collection: disabled` otherwise |
| `deploy/chart/profgate/README.md`, the derived-memory paragraph (`:126-129`) | one clause: the binary's figure and the chart's now agree on both branches, so the chart no longer has a branch the binary lacks |
| `CHANGELOG.md`, `### Changed` | **BREAKING: the container memory limit follows `pgo.enabled`.** `config validate` printed a merge budget for a gateway that never merges, three times what the chart renders; it now prints the gateway's own footprint alone with collection off, and prints `pgo collection: disabled` in place of the `pgo working set bytes` line, so a reader of the second line sees the state rather than a budget. The kustomize base drops from `1536Mi` to `512Mi` with it, so uncommenting its PGO block now means raising the limit too |

- [ ] **Validate and commit**

```bash
semlf check internal/config/config.go internal/config/config_test.go \
  cmd/profgate/main.go cmd/profgate/main_test.go \
  deploy/deploy_test.go deploy/chart_test.go test/e2e/harness_test.go \
  docs/configuration.md docs/deployment.md docs/pgo.md \
  deploy/chart/profgate/README.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/config/config.go internal/config/config_test.go \
  cmd/profgate/main.go cmd/profgate/main_test.go \
  cmd/profgate/testdata/good-pgo.yaml cmd/profgate/testdata/nats.creds \
  deploy/base/deployment.yaml deploy/base/configmap.yaml \
  deploy/deploy_test.go deploy/chart_test.go test/e2e/harness_test.go \
  docs/configuration.md docs/deployment.md docs/pgo.md \
  deploy/chart/profgate/README.md CHANGELOG.md
git commit -m "fix(config): size collection-off memory" -m "<body: the figure followed pgo.limits and never pgo.enabled, so a gateway with collection off was told it needed a merge budget; what the command prints now, and why the base moved>"
```

---

## A pprof port of zero is refused

Closes the roadmap bullet beginning
*`discovery.pprof.port: 0` is accepted as "unset"*.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `docs/configuration.md`, `CHANGELOG.md`

**The decision, and why.**
The documented range wins: `0` is refused, and omitting the key is how the default is taken.
`docs/configuration.md:73` gives the constraint as `1` to `65535`,
and `allowedSelections` is already stricter than the key it bounds —
an entry of `- port: 0` fails with `1-65535` (`internal/config/config.go:178-181`,
covered by `internal/config/config_test.go:266`).
Documenting "0 means default" instead would leave one number meaning "unset" in the key
and "invalid" in the list that bounds it, in the same block.

Two shapes are admitted today and stop being admitted:

- `port: 0` with no `portName` loads and normalizes to `6060` (`internal/config/config.go:722-724`),
  so a typed zero silently becomes a port the operator did not write.
- `port: 0` beside a `portName` loads too,
  because the exactly-one rule at `internal/config/config.go:741` reads `pprof.Port != 0` as "port is unset",
  where the same file writing `port: 6060` beside a `portName` is refused.
  Writing `port: 0` is writing the key, so this configuration names both and is refused with the rest.

**Which source decides.**
An environment variable beats the file (`docs/configuration.md:26`), and the refusal follows that rule:

- A valid `PROFGATE_PPROF_PORT` over a file that writes `port: 0` loads with the variable's value.
  fuda applies the variable over the file, so the loaded `Port` is the variable's.
- `PROFGATE_PPROF_PORT=0` over a file that writes a valid port is refused, naming the variable.
  fuda sets the zero over the file's value:
  a file writing `port: 6060` beside a `portName` loads today under that variable with `Port` `0`,
  which is how the zero was proven to land before `normalize` refills it.
- `port: null` is absent.
  `yaml.v3` leaves an `int32` untouched on a null scalar, so the loaded `Port` is `0` with nothing written into it,
  and a file writing `port: null` and no `portName` loads as `6060` today;
  the refusal reads a null value node as the key not being written, so that stays true.
- A variable holding anything but a number never reaches the check:
  fuda refuses it as `field 'Port' (tag 'env'): invalid size format: <value>`,
  and refuses the empty value as `field 'Port' (tag 'env'): empty string`.

**Where the check goes.**
The loaded struct cannot tell an absent key from a written zero, so the check reads the sources.
It runs in `Load` after `loader.Load(&cfg)` (`internal/config/config.go:596`) and before `normalize` (`:607`),
and fires only when the loaded `Port` is `0`:

1. `os.LookupEnv("PROFGATE_PPROF_PORT")` is set —
   its value is necessarily `0`, since anything else either loaded or failed in fuda —
   so the variable is refused.
2. Otherwise the file's `discovery.pprof.port` value node, found through the `mappingKeys` walk
   `refuseRemovedPprofKeys` already uses (`:622-645`), is present and its tag is not `!!null`,
   so the key is refused.
3. Otherwise nothing was written, and `normalize` fills the default as it does today.

The struct comment at `internal/config/config.go:84`, "Port 0 means unset", is rewritten with the check:
`0` is the loaded value of an absent key, and a written zero is refused before normalization.
The `validate:"min=0"` tag at `:86` stays, because the absent key must still pass fuda's validator.

**The message.**
One template, with the source's name substituted:

```text
%s: 0 is not a port (1-65535); omit it for the default 6060, or set discovery.pprof.portName instead
```

`%s` is `discovery.pprof.port` for the file,
and the whole message is wrapped as `config: <path>: ...` the way every file error is;
`%s` is `PROFGATE_PPROF_PORT` for the variable,
and the message is `config: PROFGATE_PPROF_PORT: 0 is not a port ...` with no path,
the way the removed-variable messages at `:567-571` are written.

- [ ] **Write the tests**

Add `TestPprofPortZeroIsRefused` to `internal/config/config_test.go`,
beside the existing pprof port cases.
Three fixtures are new — `port-zero.yaml` (`port: 0`, no `portName`),
`port-zero-with-name.yaml` (`port: 0` beside `portName: pprof`), and `port-null.yaml` (`port: null`, no `portName`) —
named the way `bad-port.yaml`, `both-ports.yaml`, and `neither-port.yaml` already are.
The rest of the table reuses `good.yaml` (writes `port: 6060` alone), `neither-port.yaml`,
and the existing `selections-port-zero.yaml`:

| Subtest | Fixture | What it asserts, and how it fails today |
|---|---|---|
| `a zero port in the file` | `port-zero.yaml` | refused with the exact file message; today it loads and `cfg.Discovery.Pprof.Port` is `6060` |
| `a zero port beside a port name` | `port-zero-with-name.yaml` | refused with the same message; today it loads |
| `a zero port from the environment` | `neither-port.yaml`, under `PROFGATE_PPROF_PORT=0` | refused with the exact variable message; today it loads as `6060` |
| `a zero variable over a valid file port` | `good.yaml`, under `PROFGATE_PPROF_PORT=0` | refused with the variable message, so the variable is proven to beat the file in the refusing direction; today it loads as `6060` |
| `a valid variable over a zero file port` | `port-zero.yaml`, under `PROFGATE_PPROF_PORT=6061` | loads with `Port` `6061`, so the variable is proven to beat the file in the loading direction; green today, and the case that stops the check from reading the file alone |
| `a null port is absent` | `port-null.yaml` | loads with `Port` `6060`; green today, and the case that stops the check from reading key presence alone |
| `an absent port still defaults` | `neither-port.yaml` | loads with `Port` `6060`; green today |
| `a zero allowedSelections entry keeps its own message` | `selections-port-zero.yaml`, the existing `port zero` case (`internal/config/config_test.go:266`) | unchanged, so the list's refusal is not swallowed by the new one |

The red state:

```bash
go test ./internal/config/ -run 'TestPprofPortZeroIsRefused'
```

The four refusal subtests fail with a nil error; the three loading subtests pass.

- [ ] **Add the refusal**

Run `grep -rn "port: 0" internal/config/testdata/ deploy/ test/` first.
Today the only hit is `internal/config/testdata/selections-port-zero.yaml`,
which writes the zero under `allowedSelections` and not under `port`, so nothing else moves;
a fixture added since would be a red package the next task inherits.

- [ ] **Say so where the range is documented**

`docs/configuration.md`, the `discovery.pprof` table (`:73`) and the paragraph under it (`:77-80`):
`0` is not "unset" — the key is omitted to take the default, writing `0` is a startup error,
`null` counts as omitted, and `PROFGATE_PPROF_PORT` decides over the file in both directions.

`CHANGELOG.md`, `### Changed`:
**BREAKING: `discovery.pprof.port: 0` is refused.**
It was read as "unset" and normalized to `6060`, or ignored beside a `portName`;
a configuration that wrote it, in the file or as `PROFGATE_PPROF_PORT`, now fails at startup,
and omitting the key is how the default is taken.

- [ ] **Validate and commit**

```bash
semlf check internal/config/config.go internal/config/config_test.go \
  docs/configuration.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/config/config.go internal/config/config_test.go \
  internal/config/testdata/port-zero.yaml \
  internal/config/testdata/port-zero-with-name.yaml \
  internal/config/testdata/port-null.yaml \
  docs/configuration.md CHANGELOG.md
git commit -m "fix(config): refuse a pprof port of zero" -m "<body: a written zero normalized to 6060 or hid beside a portName; which source decides, and what null means>"
```

Each task below that adds or changes a fixture names it the same way: the exact path is staged,
never the `internal/config/testdata` directory, since the directory also holds fixtures a task did not touch.

---

## The realm refusal names the profiles it accepts

Closes the second half of the roadmap bullet beginning
*Eight documented `PROFGATE_AUTH_OIDC_*` overrides are dropped without a word*,
the half reading *`realms.<name>.profiles` refuses an entry without listing the eight accepted names*.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `CHANGELOG.md`

**The decision, and why.**
`realms.developer.profiles: invalid entry "heaap"` is the whole of today's message
(`internal/config/config.go:776-780`), verified by loading such a file.
`profiles` is the one of the three lists with a closed set:
`namespaces` and `services` are DNS-1123 labels, an open set nothing could enumerate,
while `profileNames` (`internal/config/config.go:476`) holds exactly eight —
`cpu`, `trace`, `heap`, `allocs`, `goroutine`, `mutex`, `block`, `threadcreate` —
and `Profiles()` (`:484-486`) already returns them in order.
So the hint belongs to that row alone,
which the shared loop expresses as a per-row hint string that is empty for the first two rows.
The accepted set the message prints is `Profiles()` joined, plus `"*"`,
built from the array rather than typed out, so a ninth profile name cannot leave the message behind.

**The message.**
The `namespaces` and `services` rows keep today's text.
The `profiles` row prints:

```text
realms.%s.profiles: invalid entry %q; accepted: cpu, trace, heap, allocs, goroutine, mutex, block, threadcreate, or "*"
```

- [ ] **Write the test**

Extend the realm-validation cases in `internal/config/config_test.go`.
`internal/config/testdata/bad-profile.yaml` writes `profiles: ["nope"]` today;
this task changes that entry to `heaap`, the closest typo of `heap`,
which the existing substring assertion at `:192` still passes since it checks only for `realms.developer.profiles`.
A subtest asserts that fixture is refused with exactly
`realms.developer.profiles: invalid entry "heaap"; accepted: cpu, trace, heap, allocs, goroutine, mutex, block, threadcreate, or "*"`
after the `config: <path>: ` prefix,
so the whole ordered list is held and a message built from a prefix of the array fails on `threadcreate`.
Today the message ends at the entry and the subtest fails on the comparison.
Add a companion subtest:
`internal/config/testdata/bad-entry.yaml`, which already exists, keeps its message unchanged,
so the hint is proven to be on the one row that has a closed set.

The red state:

```bash
go test ./internal/config/ -run 'TestRealm'
```

Name the subtests so the pattern matches them;
the profile subtest fails on the exact message and the namespaces subtest passes.

- [ ] **Add the hint**

- [ ] **Validate and commit**

`CHANGELOG.md`, `### Fixed`:
a realm that names a profile wrong is now told the eight names it may choose from.

```bash
semlf check internal/config/config.go internal/config/config_test.go CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/config/config.go internal/config/config_test.go \
  internal/config/testdata/bad-profile.yaml CHANGELOG.md
git commit -m "fix(config): name the profiles a realm may list" -m "<body: the message named the entry and nothing else; profiles is the one closed set, so it is the one row with a list>"
```

---

## An override with no block to configure is refused

Closes the first half of the roadmap bullet beginning
*Eight documented `PROFGATE_AUTH_OIDC_*` overrides are dropped without a word*.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `docs/configuration.md`, `CHANGELOG.md`

**The decision, and why.**
Refuse loudly.
`auth.oidc.browser` and `auth.oidc.cli` are pointers on purpose:
`internal/config/config.go:239-241` records that the loader never walks a nil pointer,
which is what stops an environment default from making an absent block look configured,
and `:295` records that the browser block's presence is what creates the three `/auth/` routes.
Making a variable create the block would hand `PROFGATE_AUTH_OIDC_SESSION_TTL` the power to open three routes,
which is exactly what that design refuses.
Documenting the drop is the other option, and it leaves the operator with a variable that does nothing
and a gateway that says nothing — the failure mode the roadmap bullet is about.
So the variable is refused, naming itself and the block it needs.

**The eight, and why only these eight.**
Verified by loading `internal/config/testdata/auth-oidc.yaml`, an `oidc`-mode file with no `browser` and no `cli` block,
under `PROFGATE_AUTH_OIDC_SESSION_TTL`:
both pointers stay nil and the variable is not mentioned.
Verified again by loading `good.yaml`, a `disabled`-mode file with no `auth.oidc` at all,
under `PROFGATE_AUTH_OIDC_CLI_PKCE`: the same silence.

- Under `auth.oidc.browser` (`internal/config/config.go:296-304`), six:
  `PROFGATE_AUTH_OIDC_CLIENT_ID`, `PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE`,
  `PROFGATE_AUTH_OIDC_REDIRECT_URL`, `PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE`,
  `PROFGATE_AUTH_OIDC_SESSION_TTL`, `PROFGATE_AUTH_OIDC_TRANSACTION_TTL`.
- Under `auth.oidc.cli` (`:309-313`), two:
  `PROFGATE_AUTH_OIDC_CLI_CLIENT_ID`, `PROFGATE_AUTH_OIDC_CLI_PKCE`.

`auth.basic` and `auth.oidc` are pointers too, and their own variables are left alone:
`auth.mode` already governs both blocks in both directions —
absent in the mode that needs it is a startup error (`internal/config/config.go:909-916`),
and present in a mode that forbids it is a startup error (`:900-905`).
The two sub-blocks have no such governor, which is the gap, and the check is scoped to their eight variables.

**The rule.**
Each of the eight is refused whenever its destination block is nil, in every `auth.mode`:
a browser variable when `auth.oidc` is nil or `auth.oidc.browser` is nil,
a CLI variable when `auth.oidc` is nil or `auth.oidc.cli` is nil.
No mode is exempt, so no override is ever dropped silently;
a `disabled`-mode gateway under `PROFGATE_AUTH_OIDC_CLI_PKCE` fails at startup rather than ignoring it.
The rule is safe to apply to the shipped manifests:
`deploy/base` sets no `PROFGATE_` variable,
and the chart's only path for one is `extraEnv`, which refuses two of the eight already —
`PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE` and `PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE`
(`deploy/chart/profgate/templates/_helpers.tpl:555-556`) — and sets none of the other six on its own.
The check lives in `Load`, after `loader.Load(&cfg)` and before `normalize`:
it needs `os.LookupEnv`, which `validate(cfg)` cannot reach, and it needs the loaded pointers.
It runs before `normalize` so a variable is not measured against a block `normalize` has filled in.
Presence is what is refused, the empty value included,
matching the removed-variable check `Load` already carries at `:567-571`.

**The message.**
One template, with the variable and its block substituted:

```text
%s is set but %s is absent; the variable overrides a key of that block and only the file opens it, so write the block (an empty mapping is enough) or unset the variable
```

The first `%s` is the variable, the second is `auth.oidc.browser` or `auth.oidc.cli`,
and the whole message is `config: PROFGATE_AUTH_OIDC_SESSION_TTL is set but auth.oidc.browser is absent; ...` with no path,
the way the removed-variable messages are written.
The block named is the destination block even when `auth.oidc` itself is what is missing,
because writing the destination block is what makes the variable land and it cannot be written without its parent.

- [ ] **Write the tests**

Add `TestOIDCOverridesNeedTheirBlock` to `internal/config/config_test.go`:

| Subtest | What it asserts, and how it fails today |
|---|---|
| one subtest per variable and mode, table-driven over the eight variables against `auth-oidc.yaml` (no sub-block) and `good.yaml` (`disabled`, no `auth.oidc`) | the file plus that one variable is refused with the exact message for the variable and its block; today every one of the sixteen loads clean |
| `an empty browser block accepts the override` | a new fixture, `auth-browser-empty.yaml` — a copy of the existing `auth-browser.yaml` (already loaded by `internal/config/config_test.go:1292` and its neighbors) with the `browser` mapping replaced by `browser: {}` and `server.tls` kept — under `PROFGATE_AUTH_OIDC_CLIENT_ID=profgate`, `PROFGATE_AUTH_OIDC_REDIRECT_URL=https://profgate.example/auth/callback`, `PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE=testdata/cookie.key`, and `PROFGATE_AUTH_OIDC_SESSION_TTL=1h`, loads with `SessionTTL` one hour; `{}` opens the pointer for the variables to land in and is not a valid browser block by itself, since `validateBrowser` (`internal/config/config.go:1072-1110`) requires the client ID, TLS, the redirect URL, and the cookie key, so the required fields arrive through their variables; green today, and the case that proves the refusal reads the block's presence and not the variable alone. `auth-browser.yaml` itself is untouched — it already writes a complete `browser` block for the tests that load it, and this task adds a fixture rather than replacing that block |
| `an empty cli block accepts the override` | `cli-empty.yaml`, which already writes `cli: {}`, under `PROFGATE_AUTH_OIDC_CLI_PKCE=true` loads with `PKCE` true; the CLI block's defaults are sufficient, so the empty mapping is valid on its own; green today |
| `a basic-mode variable is not refused` | `good.yaml` under `PROFGATE_AUTH_BASIC_MAX_CONCURRENT` loads, so the scope is proven to stay on the eight |

The empty-block subtests are the ones that stop a naive check:
a rule keyed on the variable alone passes the refusals and breaks a deployment
that opens the block in the file and configures it from the environment,
which is the arrangement the chart's `extraEnv` exists for.

The red state:

```bash
go test ./internal/config/ -run 'TestOIDCOverridesNeedTheirBlock'
```

The sixteen refusal subtests fail with a nil error; the three loading subtests pass.

- [ ] **Add the refusal**

- [ ] **State the rule where the overrides are documented**

`docs/configuration.md`, *Environment Overrides* (`:22-31`), gains the pointer-block rule:
`auth.basic`, `auth.oidc`, `auth.oidc.browser`, and `auth.oidc.cli` exist only when the file writes them,
and no environment variable creates one;
a variable of `auth.oidc.browser` or `auth.oidc.cli` set while that block is absent is a startup error naming both,
in every `auth.mode`,
and an empty mapping — `browser: {}` — is enough to open the block.
The `auth.oidc.browser` and `auth.oidc.cli` tables (`:300-310`, `:332-338`) each gain one sentence pointing at that rule.

`CHANGELOG.md`, `### Changed`:
**BREAKING: an `auth.oidc.browser` or `auth.oidc.cli` variable with no block to land in is refused.**
The eight were applied to a nil pointer and dropped without a word;
a deployment that exports one without opening the block in the file now fails at startup, whatever `auth.mode` it runs.

- [ ] **Validate and commit**

```bash
semlf check internal/config/config.go internal/config/config_test.go \
  docs/configuration.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/config/config.go internal/config/config_test.go \
  internal/config/testdata/auth-browser-empty.yaml \
  docs/configuration.md CHANGELOG.md
git commit -m "fix(config): refuse an override with no block" -m "<body: eight variables landed on a nil pointer and vanished; the block is opened in the file and only there, so the variable is refused>"
```

---

## A decode error names the file and the key

Closes the roadmap bullet beginning
*YAML decode errors name a Go type instead of a key path*.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `docs/configuration.md`, `CHANGELOG.md`

This task runs last among the code tasks.
It is the only one that adds a helper with logic of its own,
and the only one whose feasibility rests on a library's error text rather than on behavior already measured,
so a difficulty here does not hold up the four repairs before it.

**What the message is today.**
Verified by loading a file whose `server` block writes `opsListn` on its third line:

```text
config: yaml: unmarshal errors:
  line 3: field opsListn not found in type config.ServerConfig
```

It names a Go type the operator has never seen and omits the file,
which every other error in `Load` carries through `fmt.Errorf("config: %s: %w", path, err)`.
The strict decode at `internal/config/config.go:579-584` is the one error path that wraps without the path.
`docs/configuration.md:14-17` promises the other thing:
the process fails "naming the file", and every error names "the offending key".

**The design.**
`yaml.v3` returns a `*yaml.TypeError` whose `Errors` field is a slice of one string per problem,
each carrying the document line;
two typos in two blocks produce two entries under the one heading, verified by loading such a file.
A new unexported helper in `internal/config` rewrites those strings.
It takes the file bytes, the file path, and the decode error, and returns an error, and does three things:

1. Parses the document into a `yaml.Node` once,
   the way `refuseRemovedPprofKeys` already does at `internal/config/config.go:623-626`.
2. For each entry in `Errors`, reads the line number and the entry's shape:
   - An unknown-field entry, `line N: field <name> not found in type <type>`,
     is resolved by walking the node tree for the mapping key node on line `N` whose value is `<name>`.
     The path back to the root, joined with dots, is the key path — `server.opsListn`.
     Sequence elements are written with their index: `auth.oidc.mapping.users[0].nam`.
   - Any other entry, which for a type mismatch reads `line N: cannot unmarshal !!str `+"`abc`"+` into int`,
     carries no field name, so the walk looks for the mapping keys whose value node sits on line `N`.
     Exactly one such key means the path is unambiguous — `limits.cpuSeconds` — and the entry gains it;
     `yaml.v3` reports the value's line, which for a block mapping is the key's line too.
     Several keys on that line, which is what a flow mapping such as `limits: {cpuSeconds: abc, traceSeconds: 60}` produces,
     or none, leave the entry with the file and the line alone.
3. Rewrites the entry to name the file, the line, and what it found.

**The messages.**
Each entry becomes one line of one of three shapes,
and the entries are joined by newlines under a single `config: ` prefix:

```text
config: /etc/profgate/config.yaml: line 3: unknown key server.opsListn
config: /etc/profgate/config.yaml: line 5: limits.cpuSeconds: cannot unmarshal !!str `abc` into int
config: /etc/profgate/config.yaml: line 4: cannot unmarshal !!str `abc` into int
```

The first is the unknown-key shape;
the second is a type mismatch with an unambiguous key;
the third is the fallback, the library's own text after the file and the line.
Two typos produce two lines, the second without the `config: ` prefix:

```text
config: /etc/profgate/config.yaml: line 2: unknown key server.opsListn
/etc/profgate/config.yaml: line 7: unknown key limits.cpuSecs
```

**Why the fallback is loud.**
The helper reads `yaml.v3`'s error text, which is not an interface the library promises to keep.
An entry the helper cannot parse must still produce a message with the file in it —
the fallback shape above is that floor —
but the tests below assert the rewritten form for a known typo,
so a wording change in the library turns a test red rather than quietly restoring the old message.

**The assertions the helper breaks.**
Seven existing assertions hold the old wording and move to the new shape in the same change:
`internal/config/config_test.go:95`, `:98`, and `:101` (`extra`, `foo`, `profilse`),
`:1394` and `:1397` (`auth.basic.user`, `auth.oidc.browser.clientId`),
`:1517` (`auth.oidc.cli.clientId`),
and `:1594` (`ui.path`).
Each becomes an exact match on `line N: unknown key <path>` for its fixture.
Three stay as they are:
`:1111` asserts `field password not found` from the users-file loader, which this task does not touch;
`:320` and `:898` assert that `not found in type` is absent, which stays true;
and `:1593` asserts `cannot unmarshal !!str `+"`yes-please`"+` into bool`, which the rewritten message still contains.

- [ ] **Write the tests**

Add `TestDecodeErrorNamesTheKey` to `internal/config/config_test.go`.
Six fixtures are new, named the way `unknown-top.yaml`, `unknown-nested.yaml`, and `bad-port.yaml` already are:

| Subtest | Fixture | What it asserts, and how it fails today |
|---|---|---|
| `an unknown key in a nested block` | `unknown-server.yaml`, writing `opsListn` under `server` on its third line | refused with exactly `config: <path>: line 3: unknown key server.opsListn`, and the message does **not** contain `not found in type`; today the message contains the type name and no path |
| `an unknown key at the top level` | `unknown-realmz.yaml`, writing `realmz:` in place of `realms:` | refused with `line N: unknown key realmz` after the path |
| `an unknown key inside a sequence element` | `unknown-users-entry.yaml`, writing `nam` under the first `auth.oidc.mapping.users` entry | refused with `unknown key auth.oidc.mapping.users[0].nam` |
| `two typos are both named` | `unknown-two.yaml`, writing `opsListn` under `server` and `cpuSecs` under `limits` | refused with the exact two-line message above, so the multi-error join is covered |
| `a type mismatch names the key` | `bad-cpu-seconds.yaml`, writing `cpuSeconds: abc` in a block mapping | refused with `line N: limits.cpuSeconds: cannot unmarshal !!str `+"`abc`"+` into int` after the path |
| `a type mismatch in a flow mapping names the line` | `bad-cpu-seconds-flow.yaml`, writing `limits: {cpuSeconds: abc, traceSeconds: 60, maxConcurrentProfiles: 16}` | refused with `line N: cannot unmarshal !!str `+"`abc`"+` into int` after the path and no key, so the fallback is proven to stay silent where the line is ambiguous |
| `a valid file still loads` | the shipped `pgo-full.yaml` fixture | loads clean, so the rewrite is proven not to fire on a good file |

The `not found in type` assertion is the tripwire the whole task rests on:
it is the one that goes red if a library upgrade changes the text the helper reads.

The red state:

```bash
go test ./internal/config/ -run 'TestDecodeErrorNamesTheKey'
```

Six subtests fail on the message; the valid-file subtest passes.
The seven migrated assertions go red with them and green with the helper.

- [ ] **Add the helper and call it**

- [ ] **Say what an error looks like**

`docs/configuration.md`, the paragraph at `:14-17`, gains the shape of the message —
the file, the line, and the key path, with the two examples above —
so the promise and the output can be compared without running the binary.

`CHANGELOG.md`, `### Fixed`:
a decode error names the file, the line, and the key path in it, where it named a Go type and no file.

- [ ] **Validate and commit**

```bash
semlf check internal/config/config.go internal/config/config_test.go \
  docs/configuration.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/config/config.go internal/config/config_test.go \
  internal/config/testdata/unknown-server.yaml \
  internal/config/testdata/unknown-realmz.yaml \
  internal/config/testdata/unknown-users-entry.yaml \
  internal/config/testdata/unknown-two.yaml \
  internal/config/testdata/bad-cpu-seconds.yaml \
  internal/config/testdata/bad-cpu-seconds-flow.yaml \
  docs/configuration.md CHANGELOG.md
git commit -m "fix(config): name the key a decode error rejects" -m "<body: the strict decode named a Go type and no file; the key path is read from the document tree, and the fallback keeps the file>"
```

---

## The ceilings that move together are written down

Closes the roadmap bullet beginning
*Four shipped defaults sit on their own ceiling*.

**Files:**
- Modify: `docs/configuration.md`, `internal/config/config_test.go`

**The decision, and why.**
Every pair below is a rule the gateway already enforces correctly,
and the defect is that an operator meets them one failed startup at a time.
Each was measured by lowering the ceiling one notch over a file that writes no `pgo.limits`
and no `pgo.defaults` at all, so every figure below is a shipped default:

| What sits on what | Shipped | What a narrowing costs |
|---|---|---|
| `pgo.defaults.sampling.maxParallel` on `pgo.limits.maxParallel` | both `4` | `maxParallel: 3` fails until the default moves first |
| `pgo.defaults.artifact.retention` on `pgo.limits.maxRetention` | both `24h` | `maxRetention: 23h` fails until the default moves first |
| `pgo.limits.maxDuration` on `limits.cpuSeconds` | `60s` on `60` | `cpuSeconds: 59` fails while `pgo.enabled`, and not otherwise |
| `pgo.limits.maxEvery` on its own maximum | both `24h` | it cannot be raised at all |

And the pair the bullet's second sentence names:
`pgo.limits.maxRetention` documents a maximum of `720h`,
but `pgo.jobRetention` must exceed it by an hour (`internal/config/config.go:824-827`) and ships `168h`,
so the shipped configuration caps `maxRetention` at `167h`;
`168h` is already refused, and reaching `720h` needs `jobRetention` at `721h` or more,
which its own maximum of `2160h` admits.

Three of the five are the "narrowing fails until a second key moves" case the roadmap describes;
`maxEvery` is a value that cannot be raised, which is a different shape and is labelled as one.
`pgo.limits.maxRounds` looks like a fifth and is not:
`pgo.defaults.sampling.rounds` ships `2` against a ceiling of `5`, so it has headroom.

Where it goes: a short table under the `pgo.defaults` section of `docs/configuration.md`,
which is where the ceilings each default must obey are already listed,
with the `maxRetention` cap stated on the `pgo.limits` row that documents `720h`
so the two numbers are read together.

While the `pgo.defaults` table is open, one more line is wrong there:
`sampling.replicas` (`:458`) is documented as "`all`, or a count from `1` to `maxTargetsPerRound`",
and `all` skips the count check entirely (`internal/config/config.go:1224-1233`),
so the ceiling applies to a number and not to `all`.

**What holds the table.**
The cross-key cases at `internal/config/config_test.go:752-838` already cover the retention and duration rows —
`pgo-retention-above-max.yaml` and `max duration above cpu seconds` —
and nothing covers the other two.
Two cases join that table, green on arrival because the rules exist,
so the two figures the reference states are held by a test rather than by the reader:

| Case | What it asserts |
|---|---|
| `max parallel below the default` | `PROFGATE_PGO_LIMIT_MAX_PARALLEL=3` over `pgo-full.yaml` is refused with `pgo.defaults.sampling.maxParallel 4 must be at most pgo.limits.maxParallel 3` |
| `max every above its maximum` | `PROFGATE_PGO_LIMIT_MAX_EVERY=25h` over `pgo-full.yaml` is refused with a message containing `pgo.limits.maxEvery` |

Both messages were produced by running the loader; the second is the validator's `must be at most 24h`.

- [ ] **Write the table, correct the replicas row, and add the two cases**

The table's other three rows and the `jobRetention` cap rest on the existing cases and on the figures measured above;
a future change to a default or a ceiling that leaves the table stale is caught by whichever case it moves,
and by the reader for the rest.
A test that recomputes every pair from the struct tags was considered and left out:
it would encode the same arithmetic twice and pass whatever the tags say.

- [ ] **Validate and commit**

```bash
semlf check internal/config/config_test.go docs/configuration.md
mise run lint && mise run test && mise run check
git add docs/configuration.md internal/config/config_test.go
git commit -m "docs: state the ceilings that move together" -m "<body: four shipped values sit on their own ceiling and the reference did not say so; the two uncovered pairs gain a case>"
```

---

## The complete example is the defaults it claims

Closes the roadmap bullet beginning
*The "complete configuration" example raises three `pgo.limits` values*,
and closes the plan.

**Files:**
- Modify: `docs/configuration.md`, `docs/plans/roadmap.md`, `docs/plans/config-validate.md`

**The decision, and why.**
The example moves to the defaults; the sentence stays.
`docs/configuration.md:670-673` writes `maxSampleBytes: 33554432`, `maxMergedBytes: 67108864`,
and `maxActiveCollections: 2`,
where the shipped defaults are `16777216`, `33554432`, and `1`
(`internal/config/config.go:360-363`),
and `:706` says every value under `pgo.limits` and `pgo.defaults` above is the shipped default.

Correcting the sentence instead was rejected.
The example is introduced as "a complete configuration ... adapted from the shipped
[`configmap.yaml`](../../deploy/base/configmap.yaml)",
whose PGO comment says every ceiling then keeps the shipped default,
and the rest of the page is written against the defaults:
the sizing arithmetic at `:430-431` computes `1Gi` and `1536Mi` from them,
and the `config validate` sample prints the same figures.
An example carrying `maxActiveCollections: 2` would need `4Gi` and `4608Mi`,
so keeping it would leave three numbers on the page that no other number on the page agrees with.
Nothing machine-reads this file —
the only test that parses a document reads the NATS permission table of `docs/specs/pgo.md` (`deploy/deploy_test.go:668`) —
so the example is held by review,
which is one more reason for it to say the same thing twice rather than twice differently.

- [ ] **Correct the three values**

- [ ] **Finish the plan in the same commit**

The ceilings task's commit is the last one that carries code or its own tests,
so the end-to-end suite runs on it, before the pull request opens;
the two commits below add no code for that suite to cover.
The pull request opens once that commit is pushed,
and its number is what the finishing commit below writes to the roadmap.

This change lands the last task, so it is the one that closes the plan,
and it follows the shape the previous plan used.
In it:
tick all six bullets of the `config validate` item in [`roadmap.md`](roadmap.md);
rewrite that item's `Spec:` line, today:

```text
Spec: none; the memory rule is already stated in `docs/deployment.md` and the chart README.
```

to name the accepted text this work conforms to, without revising it:
the configuration table in [`gateway.md`](../specs/gateway.md), which gives `discovery.pprof.port` as `1–65535`;
the paragraph in [`pgo.md`](../specs/pgo.md) that describes what `config validate` prints
and gives a gateway replica a static limit no `pgo.limits` key enters;
and [`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md),
which sizes the in-process collector as the gateway's footprint plus the working set;
set its `Shipped:` line to `Shipped: pull request #<N>`, the number of the pull request opened above;
set line 3 of this file to `**Status:** Done`;
and insert `**Outcome:**` as line 4, naming the range of work commits on this branch
and what the flipping commit itself carries, in this shape:

```text
**Outcome:** commits `<first>` through `<last>` on `docs/plan-config-validate` carry the repairs; the commit that carries this line corrects the example.
```

`<first>` is the memory task's commit and `<last>` the ceilings task's,
read from `git log --oneline main..` before the commit is written;
the flipping commit cannot name its own hash, so it names what it does instead.
`check_status` in [`check-repo.py`](../../scripts/check-repo.py) requires `**Outcome:** ` followed by text on line 4 and nothing more,
and [`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) asks that line to say where the work went.

The deletion is the next commit, and has to be a separate one:
the tree a commit writes either holds this finished plan or does not,
which is the two-commit protocol
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records.
That commit deletes this file and rewrites every link that cited it, which `check_links` enforces;
it changes nothing else.
Run `grep -rn config-validate --include='*.md' .` before the deletion to find the links.

- [ ] **Validate and commit**

```bash
semlf check docs/configuration.md docs/plans/roadmap.md docs/plans/config-validate.md
mise run lint && mise run test && mise run check
git add docs/configuration.md docs/plans/roadmap.md docs/plans/config-validate.md
git commit -m "docs: make the complete example the defaults" -m "<body: three pgo.limits values sat above the defaults under a sentence saying every value is one; the plan is Done>"
```

---

## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite:

```bash
mise run test:e2e
```

[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
names `deploy/` among the eight packages that need the suite on the `current` lane before a pull request,
and the first task changes `deploy/base/deployment.yaml`, `deploy/base/configmap.yaml`, and the harness.
`test/e2e/overlays/default/kustomization.yaml` builds on that base,
and the harness runs that gateway with collection on under the `1536Mi` patch,
so the suite proves the patch and the PGO-enabled path and not the `512Mi` figure itself;
no gateway in the suite runs collection off on the base Deployment,
and `TestDeploymentMemoryLimit` is what holds the `512Mi` figure.
The three new refusals reach a real gateway in the same run:
every overlay's ConfigMap is loaded by the process under test, so a fixture that trips one of them fails the suite.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **The base's PGO-enablement path grows a step.**
  With the base at `512Mi`,
  uncommenting the ConfigMap's PGO block and applying it produces a container sized for the interactive path alone,
  and the Pod meets the shortfall as an out-of-memory kill during a merge rather than at startup.
  Nothing checks the two files against each other outside `TestDeploymentMemoryLimit`,
  which runs in this repository and not on an operator's cluster.
  The comments in both files, which name the figure, are the whole of the mitigation;
  the harness patch is the in-repository instance of the step and not a check on an operator's copy.
- **The decode rewrite reads a library's error text.**
  `yaml.v3` does not promise the wording of `TypeError.Errors`,
  and the helper matches on it to find the field name and the line.
  The `not found in type` assertion is what turns a library change into a red test;
  the retreat position, if the walk proves brittle in practice,
  is to wrap the decode error with the file path alone and leave the type name in the message,
  which keeps half the promise and none of the fragility.
- **A key path built from a line number is wrong for a key carried in by a merge key.**
  `yaml.v3` reports the line the offending key is written on,
  which for a key inherited through `<<` is the anchor's line rather than the using block's.
  The path then names where the key was written, not where it landed.
  That is still a location in the operator's file, and it is the location they have to edit.
- **The eight refused variables are refused, not honoured, in every mode.**
  A deployment that exports a common environment across gateways —
  some with a browser block, some without, some not running `oidc` at all — now fails on every gateway without the block.
  Opening an empty `browser: {}` in the file is the fix where the block is wanted,
  and unsetting the variable is the fix where it is not; the message says both,
  but the failure arrives at startup rather than at review.
- **`auth.basic`'s three variables and `auth.oidc`'s eleven are still dropped silently
  when their block is absent.**
  The scope argument in that task is why: `auth.mode` already refuses the state where the block is wanted and missing.
  A `disabled`-mode gateway under `PROFGATE_AUTH_OIDC_ISSUER` still says nothing,
  and it is recorded here so the next reader finds it.
- **The ceiling table is documentation with two cases behind it.**
  A future change to a default or a ceiling can leave the rows the cases do not hold stale,
  and only a reader will notice.

---

## Self-Review

- Bullet coverage, one line each:
  the memory branch, the base figure, and the chart-test workaround
  (*The container is sized for the collection it does*);
  the complete example (*The complete example is the defaults it claims*);
  the decode message (*A decode error names the file and the key*);
  the zero port (*A pprof port of zero is refused*);
  the eight dropped overrides (*An override with no block to configure is refused*)
  and the profiles list (*The realm refusal names the profiles it accepts*);
  the paired ceilings (*The ceilings that move together are written down*).
- Current-source facts this plan rests on, each confirmed by reading the file or by running the code:
  `GatewayMemoryBytes` adds `PGOGatewayBaseMemory` to `PGOMemoryBytes` with no reference to `PGO.Enabled`;
  `cmd/profgate/testdata/good.yaml` writes no `pgo` block and `config validate` prints `1610612736` for it;
  `docs/configuration.md` and `docs/deployment.md` both already state the PGO-off rule the code breaks;
  `profgate.resources` in the chart already branches on `pgo.enabled`;
  `TestDeploymentMemoryLimit` computes its expectation from the base's own ConfigMap;
  `TestChartBaseTermIsTheSameFigureBothWays` subtracts the working set to reach the base term;
  `deploy/base/deployment.yaml` carries `1536Mi` and a comment justifying it,
  and `deploy/base/configmap.yaml` carries the matching comment;
  the harness applies a PGO-enabled configuration to the `default` overlay, which inherits the base Deployment,
  and no other overlay's Deployment sets a memory limit;
  a file writing `discovery.pprof.port: 0` loads and normalizes to `6060`,
  one writing `port: 0` beside a `portName` loads as well,
  one writing `port: null` loads as `6060`,
  a valid `PROFGATE_PPROF_PORT` overrides a file zero,
  `PROFGATE_PPROF_PORT=0` lands on a file `6060` before `normalize` refills it,
  a non-numeric or empty variable fails inside fuda,
  and an `allowedSelections` entry of `- port: 0` is already refused with `1-65535`;
  a file writing `opsListn` under `server` fails with
  `field opsListn not found in type config.ServerConfig` and no file path,
  two typos in two blocks produce two entries under one heading,
  and a type mismatch reports the value's line;
  an `oidc`-mode file with no `browser` and no `cli` block leaves both pointers nil, silently,
  under `PROFGATE_AUTH_OIDC_SESSION_TTL`,
  and a `disabled`-mode file does the same under `PROFGATE_AUTH_OIDC_CLI_PKCE`;
  `deploy/base` sets no `PROFGATE_` variable and the chart's `extraEnv` refuses two of the eight;
  `auth.basic` and `auth.oidc` are each required in one mode and refused in the others;
  a realm naming `heaap` is refused with the entry alone and no list;
  `profileNames` holds eight names and `Profiles()` returns them;
  lowering `pgo.limits.maxParallel`, `pgo.limits.maxRetention`, or `limits.cpuSeconds` one notch fails startup at the shipped defaults,
  while lowering `maxEvery`, `minEvery`, `maxTargetsPerRound`, or `maxSampleBytes` does not,
  and `maxEvery` cannot be raised;
  `pgo.limits.maxRetention: 168h` is refused against the shipped `pgo.jobRetention: 168h`, and `167h` is accepted;
  the complete example raises three `pgo.limits` values above their defaults under a sentence saying every value is a default;
  no test reads `docs/configuration.md`;
  every commit header above is under 50 characters.
- Decided here, with the reason stated at the task that carries it:
  the branch in `GatewayMemoryBytes` alone and not in `PGOMemoryBytes`;
  the base at `512Mi` with the harness patched rather than the base held at `1536Mi`;
  `pgo collection: disabled` printed in place of the working-set line;
  `discovery.pprof.port: 0` refused rather than documented, which also refuses it beside a `portName`,
  with the variable deciding over the file and `null` read as absent;
  the pointer-block rule refusing every one of the eight whenever its block is nil, in every mode;
  the decode error rewritten from the document tree, with the key derived for an unambiguous type mismatch too,
  and a loud test on the library's wording;
  the profiles hint on the one list with a closed set, built from the array;
  the ceiling pairs documented rather than changed, with the two uncovered pairs gaining a case;
  the complete example moved to the defaults rather than the sentence moved to the example;
  the plan closed in two commits,
  `Outcome:` naming the branch's commit range and the roadmap's `Shipped:` line naming the pull request.
- Left to the implementer: the exact wording of every comment and commit body,
  the fixture and subtest names where a task does not fix them,
  and the shape of the node walk in the decode helper.
