# The Install Path Holds on a Fresh Cluster

**Status:** Done
**Outcome:** commits `d1b0cf2` through `80e37f8` on `docs/plan-install-path` carry the repairs; the commit that carries this line links the guides.

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.

**Goal:** make the first fifteen minutes of `v0.5.0` survive contact with a cluster nobody prepared.
`kubectl apply -k deploy/base` applies on a cluster that has never heard of Profgate.
The chart refuses at render time the three configurations the roadmap names —
a `memoryLimitWithoutPGO` the chart cannot read as bytes,
`basic` mode with no user set, and `oidc` mode with no issuer —
rather than handing back a Deployment that crash-loops or a container sized in bytes.
Configurations that will also fail at startup for reasons the chart cannot see stay out of scope.
The install notes describe the release that was actually rendered.
The kustomize base says which surface it is and names the image it runs.
The guides link the changelog that carries `0.5.0`'s five breaking changes,
and restate the port-selection one where an operator upgrades,
and the quickstart shows how to find the namespace and Service its own examples need.

**Architecture:** no Go code changes, no package moves, no new route, and no new chart value.
`deploy/base/` gains one Namespace object;
`deploy/chart/profgate/templates/_helpers.tpl` gains one validation helper and one call to an existing one;
`NOTES.txt`, four Markdown guides, and the two deployment test files carry the rest.
The Kubernetes interface, the ClusterRole, and every runtime code path are untouched.

**Spec:** none.
No accepted spec governs this item, and it changes no behavior a spec defines.
The two auth refusals are render-time restatements of rules
`internal/config` already enforces at startup.
The memory refusal is not one of those:
`memoryLimitWithoutPGO` is a chart value (`deploy/chart/profgate/values.yaml:141-146`),
the binary carries its own fixed base figure instead
(`PGOGatewayBaseMemory`, `internal/config/config.go:528-533`),
and the whole-number-of-`Mi`-or-`Gi` grammar is the chart's own contract,
already documented for `0.5.0` (`CHANGELOG.md:106-111`)
and already enforced on the PGO-on branch.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md),
under *Make the install path hold on a fresh cluster*,
and the evidence behind each task is in
[`docs/investigations/2026-09-03-usability-and-stability.md`](../investigations/2026-09-03-usability-and-stability.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

## Global Constraints

- **No feature.**
  No task adds a chart value, an HTTP route, or a configuration key.
  One Kubernetes resource kind the base does not carry today is added, and only one:
  the Namespace, which the roadmap's first bullet expressly offers as the remedy.
  The base already names it in five namespaced resources and in the ClusterRoleBinding's subject.
  Every new refusal names a value that exists today.
- **No RBAC change.**
  No Kubernetes verb, resource, or API group moves.
  `TestClusterRoleTuples` in `deploy/deploy_test.go`
  and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go` stay green and untouched.
- **The kustomize base's memory figure does not move, and this plan says why.**
  `TestDeploymentMemoryLimit` (`deploy/deploy_test.go:780-807`)
  asserts the base's `1536Mi` equals `cfg.GatewayMemoryBytes()` computed over the base's own ConfigMap,
  and `GatewayMemoryBytes` (`internal/config/config.go:547-552`) never reads `PGO.Enabled`,
  so it returns the PGO-sized figure for a configuration with collection off.
  The number is held in place by a defect the roadmap's *Make `config validate` tell the truth* item owns.
  This plan changes the comment above the figure and nothing else about it;
  the plan for that later item owns the number, the function, and this test together.
  That is the separation [`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
  asks for before deferring a coupled issue:
  the two changes share one assertion, and the install-path work does not touch it.
- **A render-time refusal reads the configuration the ConfigMap will carry, not the structured value alone.**
  `profgate.config` merges the raw `config:` block over the structured keys with `mergeOverwrite`,
  so a value supplied only through that block is what the gateway reads.
  Every guard added here follows the presence rule `profgate.natsURL` (`_helpers.tpl:644-666`) already uses:
  a key present in the raw block decides the message and is the value judged,
  and only in its absence is the structured key read.
  A `PROFGATE_`-prefixed entry in `extraEnv` is a third source that reaches the gateway without reaching the ConfigMap;
  *The chart refuses an auth mode it can see will not start* says what a guard does when it meets one.
- **A configuration that stays admitted renders exactly what it renders today, and the admitted set narrows.**
  The memory guard validates a value it still prints verbatim,
  so `512Mi` and `1Gi` render as they do now
  and `TestChartMemoryLimitWithoutPGO` keeps asserting `512Mi` as a quantity, unchanged.
  What changes is what the chart accepts.
  On the PGO-off branch `memoryLimitWithoutPGO` now has to be a whole number of `Mi` or `Gi` —
  the grammar `CHANGELOG.md:106-111` already documents for the value
  and the PGO-on branch already enforces — so `512` renders `memory: 512` today and is refused after this work,
  and so are `1500M` and `0.5Gi`, which Kubernetes itself accepts as quantities.
  The auth guards likewise refuse configurations that render today
  and produce a Pod that exits at startup.
  Widening the chart's grammar to every quantity Kubernetes accepts is separate work this plan does not do.
- No jargon: comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a task name, or a review round.
- Markdown prose uses semantic line breaks; run `semlf check <file>` on every Markdown file a task writes or edits.
- Commit headers are Conventional Commits under 50 characters, with no trailer of any kind.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

---

## File Structure

```text
deploy/base/namespace.yaml                        # the Namespace the base's five namespaced resources name
deploy/base/kustomization.yaml                    # namespace.yaml joins resources, first
deploy/base/deployment.yaml                       # the memory comment states what this base does; the image tag is pinned
deploy/chart/profgate/templates/_helpers.tpl      # profgate.validateAuthMode; the PGO-off branch validates its base term
deploy/chart/profgate/templates/deployment.yaml   # calls profgate.validateAuthMode beside profgate.validateAuthSecret
deploy/chart/profgate/templates/NOTES.txt         # basic mode's wording; the backend-protocol warning
deploy/chart/profgate/README.md                   # the two new refusals, the Mi/Gi rule on both branches, the changelog link
deploy/deploy_test.go                             # the Namespace, the image tag
deploy/chart_test.go                              # the memory guard, the auth refusals and the cases they must not refuse, the notes
README.md                                         # the client, the changelog, and a quickstart that finds its own arguments
docs/deployment.md                                # the Namespace, the base-versus-chart memory sentence, the upgrade note
docs/plans/roadmap.md                             # the item's checkboxes and its Shipped line
docs/plans/install-path.md                        # this file
```

---

## The base creates the namespace it names

Closes the roadmap bullet beginning
*`kubectl apply -k deploy/base` fails on a cluster without a `profgate` namespace*.

**Files:**
- Create: `deploy/base/namespace.yaml`
- Modify: `deploy/base/kustomization.yaml`, `deploy/deploy_test.go`, `docs/deployment.md`

**The decision, and why.**
The base gets a Namespace object rather than a documented `kubectl create namespace` step.
Five of its resources hard-code `metadata.namespace: profgate`
(`configmap.yaml:5`, `deployment.yaml:5`, `networkpolicy-gateway.yaml:5`, `service.yaml:5`,
`serviceaccount.yaml:5`),
a sixth file names the same namespace in the ClusterRoleBinding's subject (`clusterrolebinding.yaml:12`),
and `docs/deployment.md:44` presents `kubectl apply -k deploy/base` as the whole command.
A base that names a namespace it does not create is not a base anyone can apply;
a prose step in front of it leaves the guide's own command failing for every reader who copies it.
The Helm path already creates the namespace, with `--create-namespace` in the quickstart,
so this is the base catching up to the chart rather than a new idea.

Two consequences belong in the guide rather than in a comment nobody reads:
`kubectl delete -k deploy/base` now deletes the namespace and everything else in it,
and a repository that manages namespaces elsewhere drops the entry from `resources`.

`namespace.yaml` carries `apiVersion: v1`, `kind: Namespace`, and `metadata.name: profgate`, and no labels.
The directory is not uniformly unlabelled:
`deployment.yaml:6-7` carries `app.kubernetes.io/name: profgate` on the Deployment itself,
alongside the pod-template copy its selector matches.
Nothing selects a Namespace, though, so a label here would exist only for symmetry, and none is added.
`kustomization.yaml` lists it first, before `serviceaccount.yaml`, for a reader's benefit and not for the tool's:
kustomize emits a Namespace ahead of every namespaced kind whatever position the list gives it,
and `TestKustomizationListsEveryFile` sorts both sides before comparing, so position is invisible to the test too.

**What this does to the end-to-end suite.**
`test/e2e/overlays/default/kustomization.yaml` builds on `deploy/base`,
so the default gateway's manifests gain the Namespace as well.
No file under `test/e2e/` needs editing for it:
the harness wraps every overlay in a kustomization carrying `namespace: <ns>`,
and kustomize's namespace transformer renames a Namespace object to that value,
so the overlay emits the same `profgate` namespace `createNamespace` in `test/e2e/harness_test.go` has already made,
and applying it again is idempotent.
The suite is what proves that, which is one more reason the run below is not optional.

- [ ] **Write the test**

`TestKustomizationListsEveryFile` (`deploy/deploy_test.go:533`) already fails the moment the file exists
and `kustomization.yaml` does not list it, so it needs no edit and proves the second half on its own.
Add `TestNamespace` beside it:
it decodes `namespace.yaml`, asserts the name is `profgate`,
and asserts that every namespaced object in the directory names that same namespace,
reading each file's `metadata.namespace` rather than a literal,
so a rename that touches only one file fails here.

- [ ] **Add the Namespace**

Create the file, list it first in `kustomization.yaml`.

- [ ] **Say what applying and deleting the base now do**

In `docs/deployment.md`, under *With kustomize*:
the base creates the `profgate` namespace, so the command applies to a cluster that does not have one;
`kubectl delete -k deploy/base` therefore removes the namespace and everything in it;
a repository whose namespaces are managed elsewhere removes `namespace.yaml` from `resources`.

- [ ] **Validate and commit**

```bash
semlf check docs/deployment.md
mise run lint && mise run test && mise run check
git add deploy/base docs/deployment.md deploy/deploy_test.go
git commit -m "fix(deploy): the base creates its namespace"
```

---

## The disabled-PGO branch validates the value it renders

Closes the roadmap bullet beginning
*The chart's `memoryLimitWithoutPGO` guard runs only on the PGO-on branch*.

**Files:**
- Modify: `deploy/chart/profgate/templates/_helpers.tpl`, `deploy/chart_test.go`

**The decision, and why.**
`profgate.gatewayBaseMemoryBytes` (`_helpers.tpl:256-271`) already refuses anything but a whole number of `Mi` or `Gi`,
and already requires the value to be set at all.
The PGO-on branch reaches it through `profgate.gatewayMemoryBytes`;
the PGO-off branch at `_helpers.tpl:302-308` prints `.Values.memoryLimitWithoutPGO` without ever calling it,
so `--set memoryLimitWithoutPGO=512` renders `memory: 512`, which Kubernetes reads as 512 bytes.

The off branch calls the existing helper for its validation and discards the result,
then renders the value verbatim as it does today:

```gotemplate
{{- else -}}
{{- $_ := include "profgate.gatewayBaseMemoryBytes" . -}}
limits:
  memory: {{ .Values.memoryLimitWithoutPGO }}
{{- end -}}
```

Rendering the helper's byte count instead was considered and rejected:
it would change every PGO-off install's manifest from `512Mi` to `536870912` for no gain,
and the difference between the two branches' printed forms is a separate finding the roadmap does not order here.
Validating through the same helper both branches use is also what keeps one rule in one place:
a future change to the accepted suffixes cannot leave one branch behind.
The helper opens with `required`, so an empty `memoryLimitWithoutPGO` now fails on the off branch too,
where today it renders a container with no memory limit at all.

- [ ] **Write the tests**

| Test | What it asserts |
|---|---|
| `TestChartMemoryLimitRejectsAnUnreadableBase`, split into subtests `pgo on` and `pgo off` | the existing `--set memoryLimitWithoutPGO=512MB` case keeps its `pgoValues(t)` values as `pgo on`; `pgo off` runs `renderFailure` with `--set memoryLimitWithoutPGO=512` and no PGO values, and asserts the message names `memoryLimitWithoutPGO 512 must be a whole number of Mi or Gi` |
| `TestChartMemoryLimitWithoutPGO`, gaining subtest `an override renders as the quantity it was given` | `--set memoryLimitWithoutPGO=1Gi` renders `resources.limits.memory` as `1Gi`, proving the guard validates without changing the rendered form; the existing body becomes the `shipped default` subtest and keeps its `512Mi` assertion |

The `pgo off` case is the rendered-output test the guard needs:
it fails today, because the render succeeds and produces a Deployment,
and the assertion is on `helm`'s refusal rather than on a manifest.

- [ ] **Guard the branch**

- [ ] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add deploy/chart/profgate/templates/_helpers.tpl deploy/chart_test.go
git commit -m "fix(chart): validate the base memory term always"
```

---

## The chart refuses an auth mode it can see will not start

Closes the roadmap bullet beginning
*`auth.mode=basic` with no users and `auth.mode=oidc` with no issuer render a Deployment that crash-loops*.

**Files:**
- Modify: `deploy/chart/profgate/templates/_helpers.tpl`,
  `deploy/chart/profgate/templates/deployment.yaml`, `deploy/chart_test.go`

**The decision, and why.**
The chart already refuses two configurations of exactly this class,
and the two use different mechanisms for different reasons:
`ingress.yaml:3` calls `fail` directly, because the check belongs to the one template that renders that resource,
and `profgate.natsURL` uses `required` inside a helper, because the value it guards is also the value it returns.
The auth guards guard values the config helper renders but does not return,
so they take a third shape already present in the chart:
a validation-only helper called for its side effect from `deployment.yaml`,
which is what `profgate.validateAuthSecret` (`_helpers.tpl:580-597`) is
and how `deployment.yaml:2` calls it.
The new helper is `profgate.validateAuthMode`, called on the line beside it.
Extending `validateAuthSecret` instead was rejected:
its subject is whether a Secret mount is required, and its name would stop describing it.

**What the guard reads, and what it does when it cannot read.**
The guard refuses only a configuration the chart can see will not start.
It reads two sources, in the order the presence rule in the constraints gives:
the raw `config:` block first, because `mergeOverwrite` copies it over the structured key,
and the structured `auth` values only where the raw block is silent.

`extraEnv` is a third source, and the chart cannot see through it.
The binary applies `PROFGATE_`-prefixed overrides on top of the merged file
(`deploy/chart/profgate/README.md:188-219`),
and both values this guard judges have one:
`PROFGATE_AUTH_MODE` (`internal/config/config.go:243`)
and `PROFGATE_AUTH_OIDC_ISSUER` (`internal/config/config.go:262`).
So the rule is: **if any entry of `extraEnv` carries a name beginning `PROFGATE_AUTH_`,
the guard runs no check and refuses nothing.**
A literal value and a `valueFrom` are treated alike.
The chart could read a literal, but a Secret reference it cannot read is exactly the case
where refusing would reject a configuration that starts,
and one rule covering both is shorter than one that reads a literal and guesses at the rest.
`PROFGATE_AUTH_MODE` falls under the same rule for a sharper reason:
it decides which requirement applies at all,
so treating the structured mode as authoritative would disagree with the binary's own precedence.
Banning these overrides to simplify the helper is rejected:
the chart promises `extraEnv` for every key outside the sizing and mount families,
and the guard's job is to catch the visible failure, not to narrow the escape hatch.

Four names under that prefix never reach this helper.
`profgate.validateNoDerivedOverrides` (`_helpers.tpl:387`) already refuses
`PROFGATE_AUTH_BASIC_USERS_FILE`, `PROFGATE_AUTH_OIDC_CA_FILE`,
`PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE`, and `PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE` (`:552-555`),
because each names a file the Secret mount would have to carry,
and `deployment.yaml:1` calls it before `deployment.yaml:2`.
That refusal and its message are unchanged; the skip rule above governs only what gets past it.

The two checks, over the sources above:

- **`auth.mode: basic`** fails when neither `auth.basic.users` nor `auth.basic.usersFile` is non-empty.
  `values.yaml:306,312` ship both empty, so the shipped values plus `--set auth.mode=basic` is the failing case.
  Startup validation refuses the same configuration
  (`internal/config/config.go:930-935`, through `ValidateBasicUsers`),
  so the render is refusing what the Pod would refuse a moment later.
  The message names both keys and says one of them has to carry a user.
- **`auth.mode: oidc`** fails when `auth.oidc.issuer` is empty
  (`values.yaml:319`; `internal/config/config.go:954`).
  The message names the key.

A key present in the raw `config:` block is read from there and judged there,
including when the raw value is empty, because the merge copies it over the structured key either way;
the message then names `config.auth.basic.users`, `config.auth.basic.usersFile`, or `config.auth.oidc.issuer`,
so an operator is sent to the value that actually reaches the gateway.

- [ ] **Write the tests**

| Test | What it asserts |
|---|---|
| `TestChartAuth`, gaining subtest `basic with no users is refused` | `renderFailure(t, "--set", "auth.mode=basic", "--set", "auth.basic.allowPlaintext=true")` and the message names `auth.basic.users` and `auth.basic.usersFile` |
| `TestChartAuth`, gaining subtest `oidc with no issuer is refused` | `renderFailure(t, "--set", "auth.mode=oidc")` and the message names `auth.oidc.issuer` |
| `TestChartAuth`, subtest `basic`, unchanged | it already renders `deployment.yaml` after loading the configuration (`deploy/chart_test.go:2564-2607`), so structured `basic` reaches the guard as it stands |
| `TestChartAuth`, subtest `oidc` / `without caKey renders no caFile`, extended | it calls `loadRenderedConfig` alone today (`deploy/chart_test.go:2609-2624`), which reaches only the ConfigMap; add a `render[appsv1.Deployment]` of `deployment.yaml` over `authOIDCValues(t)` so structured `oidc` reaches the guard too |
| `TestChartConfigIsMergedAndParses`, gaining subtest `basic users arrive through the raw config block` | `auth.mode=basic` with `auth.basic.allowPlaintext=true` and the user set given only as `--set-json config.auth.basic.users=[...]`: a `render[appsv1.Deployment]` of `deployment.yaml` over those values succeeds, and `loadRenderedConfig` accepts the result |
| `TestChartConfigIsMergedAndParses`, gaining subtest `the issuer arrives through the raw config block` | `auth.mode=oidc` with `auth.oidc.audience=profgate` and `auth.oidc.mapping.defaultRealm=developer`, and the issuer given only as `--set config.auth.oidc.issuer=https://issuer.example`: the same Deployment render succeeds and `loadRenderedConfig` accepts the result |
| `TestChartAuth`, gaining subtest `a literal issuer in extraEnv is not refused` | `auth.mode=oidc` with the audience and mapping set and no issuer in either source, plus `extraEnv` carrying `PROFGATE_AUTH_OIDC_ISSUER` with a literal `value`: the same `render[appsv1.Deployment]` of `deployment.yaml` succeeds |
| `TestChartAuth`, gaining subtest `a secret-backed issuer in extraEnv is not refused` | the same values with that entry carrying a `valueFrom.secretKeyRef` in place of `value`: the Deployment renders |
| `TestChartAuth`, gaining subtest `an auth mode in extraEnv is not refused` | `auth.mode=oidc` with no issuer anywhere, plus `extraEnv` carrying `PROFGATE_AUTH_MODE`: the Deployment renders, where the same values without that entry are the refusal case above |

Every success case renders the Deployment, and that is the point of the shape.
`render[appsv1.Deployment]` over `deployment.yaml` (`deploy/chart_test.go:63`) is the helper that does it;
`loadRenderedConfig` (`:247`) goes through `renderConfigFile` (`:261`), which renders `configmap.yaml` and nothing else,
so a case that only calls it passes whatever the new helper does.
`renderFailure` (`:1235`) renders the whole chart, so the two refusal cases are already right.

The two raw-block cases separate the guard from a naive one:
a guard reading `.Values.auth.basic.users` alone passes the two refusal tests
and breaks a values file that supplies its users through the raw block,
which `TestChartConfigIsMergedAndParses` (`deploy/chart_test.go:1589`) is the established home for.
The three `extraEnv` cases sit in `TestChartAuth` instead,
because what they hold is the guard's behavior and not the merge's.

- [ ] **Add the helper and its call**

Three places in `deploy/chart_test.go` set an auth mode today,
and none of them goes red:
`authBasicValues` (`:160`) supplies both an inline user and a users file,
`authOIDCValues` (`:175`) supplies an issuer,
and the `secret required` subtest (`:2671`) sets `auth.mode=basic` with `auth.basic.usersFile`,
which satisfies the new guard and still reaches the refusal it is written for,
as long as `profgate.validateAuthMode` is called beside `profgate.validateAuthSecret` rather than replacing it.
Confirm that with `grep -n "auth.mode=" deploy/chart_test.go` before writing the helper,
because a subtest added since would be a red package the next task inherits.

- [ ] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add deploy/chart/profgate/templates deploy/chart_test.go
git commit -m "fix(chart): refuse an auth mode that cannot start"
```

---

## The install notes describe the release that was rendered

Closes the roadmap bullet beginning
*`NOTES.txt` says nothing about the backend-protocol annotation*.

**Files:**
- Modify: `deploy/chart/profgate/templates/NOTES.txt`, `deploy/chart_test.go`

This task runs after the auth guards, not before:
once the chart refuses `basic` mode with no user set,
the notes can no longer be reached in that state,
so the remaining defect in the basic-mode text is wording rather than a missing warning.

**Two edits.**

*Basic mode's promise.*
`NOTES.txt:17-19` says every request needs a user from the list below,
`:28` says this release grants, and `:47-49` prints the realms.
The list below is the realms, not the users.
Rewrite the basic-mode text to say the users come from `auth.basic.users`
and, when set, the mounted file the paragraph at `:20-27` already describes,
and to introduce the list that follows as the realms this release grants,
which is what `disabled` mode's `:15` and `oidc` mode's `:44` both already do.

*The Ingress that fails every request.*
`values.yaml:69-74` and `docs/deployment.md:118-124` both warn
that an Ingress in front of a TLS-enabled API port needs a backend-protocol annotation,
and that without it the controller speaks HTTP to an HTTPS listener and every request fails.
The rendered notes say nothing.
Add a sentence inside the existing `ingress.enabled` block (`:76-82`),
rendered only when `profgate.tlsEnabled` is also true,
naming `nginx.ingress.kubernetes.io/backend-protocol: HTTPS` as ingress-nginx's form of the setting
and saying other controllers have their own.
The condition matters: a plain-HTTP install must not be told to set an annotation it does not need.

- [ ] **Write the tests**

The notes are reached through `renderNotes` (`deploy/chart_test.go:2258`),
the helper that [`docs/decisions/chart-notes-assertions.md`](../decisions/chart-notes-assertions.md)
records as kept precisely so assertions like these can exist.

| Test | What it asserts |
|---|---|
| `TestChartAuthNotes`, subtest `basic`, extended | the notes name `auth.basic.users`, and do not claim the list that follows is a list of users |
| `TestChartIngressNotes`, new, subtest `tls on` | with `ingress.enabled`, one host, and `tls.enabled`, the notes contain `backend-protocol` |
| `TestChartIngressNotes`, new, subtest `tls off` | with `ingress.enabled` and one host but no `tls.enabled`, the notes do not contain `backend-protocol` |
| `TestChartIngressNotes`, new, subtest `no ingress` | with `tls.enabled` but `ingress.enabled` left off, the notes do not contain `backend-protocol` |

The third subtest is what holds the sentence inside the `ingress.enabled` block:
a warning moved out of that block, keyed on TLS alone, still passes the first two.

The negative subtests are the load-bearing ones,
for the reason the decision record gives:
an instruction printed to an operator whose configuration does not admit it is a defect nothing else reports.

- [ ] **Edit the notes**

- [ ] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add deploy/chart/profgate/templates/NOTES.txt deploy/chart_test.go
git commit -m "fix(chart): notes match the rendered release"
```

---

## The base says what it is and names the image it runs

Closes the roadmap bullet beginning
*`deploy/base/deployment.yaml` reserves 1536Mi with PGO off where the chart reserves 512Mi*.

**Files:**
- Modify: `deploy/base/deployment.yaml`, `deploy/deploy_test.go`,
  `docs/deployment.md`, `deploy/chart/profgate/README.md`

**The memory decision, and why.**
The comment moves to the truth; the figure stays at `1536Mi`.
The base ships PGO off — its ConfigMap comments the whole block out —
and reserves the limit the commented-out block would need,
so an operator who uncomments it does not also have to find and patch a second file.
That is a defensible thing for a base to do, and the comment at `deployment.yaml:53-56` does not say it:
it describes the PGO working set in the present tense, as though the ConfigMap sized one.
Rewrite it to state the arrangement, and to name the other surface:
the chart derives its limit from the branch it renders, so a chart install with PGO off gets `512Mi` instead.
The ConfigMap's own comment already says the 1536Mi is what the shipped ceilings size, and stays as it is.

Moving the figure to `512Mi` was rejected for the reason the constraints give:
`TestDeploymentMemoryLimit` ties it to `cfg.GatewayMemoryBytes()`,
which ignores `pgo.enabled`, and repairing that function is the next roadmap item's work.
Changing the number here would either break that test or pull that item's code change into this one.

**The tag decision, and why.**
`deploy/base/deployment.yaml:31` pins `ghcr.io/arloliu/profgate:latest`,
which Kubernetes reads as `imagePullPolicy: Always`,
so every Pod restart can land on a different build than its neighbours.
Pin a released tag.
Nothing in the tree rewrites this line at release time:
`.github/workflows/release.yml` stamps the tag into the image build and into the chart's `appVersion`,
`Chart.yaml` carries `appVersion: "latest"` for a checkout,
and there is no release checklist that would name this file.
So a concrete pin goes stale by construction, and the comment beside it says so and tells the reader to set their own.
The risk section records the maintenance cost.

The test asserts the property rather than the string:
a pin that has to be edited in two places every release is a pin that will be edited in one.

- [ ] **Change the test first**

`TestDeployment` (`deploy/deploy_test.go:138`) asserts the image equals `ghcr.io/arloliu/profgate:latest` at `:151`.
Replace that assertion with two:
the image's repository is `ghcr.io/arloliu/profgate`,
and its tag is present and is not `latest`.
The test then survives every version bump and still fails the day someone puts `latest` back.

- [ ] **Pin the tag and rewrite the comment**

Pin `v0.5.0`, the current release.
The comment beside it: a release does not rewrite this line, so set the version this repository runs.

- [ ] **Say which surface does what**

| File | Change |
|---|---|
| `docs/deployment.md`, under *Resources* | the kustomize base reserves the PGO-enabled limit while shipping collection off, so uncommenting the ConfigMap's PGO block needs no second edit; the chart derives its limit from the branch it renders, which is `memoryLimitWithoutPGO` alone with collection off |
| `docs/deployment.md`, under *With kustomize* | the base pins a released image tag, which a checkout does not update; set the version this repository runs, or an image digest |
| `deploy/chart/profgate/README.md`, the paragraph on the base at its head | one clause naming the memory difference, so the sentence that the two are not copies of each other says one way they differ |

- [ ] **Validate and commit**

```bash
semlf check docs/deployment.md deploy/chart/profgate/README.md
mise run lint && mise run test && mise run check
git add deploy/base deploy/deploy_test.go docs/deployment.md deploy/chart/profgate/README.md
git commit -m "fix(deploy): pin the base image, state its sizing"
```

---

## The guides link the changelog and the client

Closes the roadmap bullet beginning
*The upgrade section of `docs/deployment.md` names no breaking change*,
and the one beginning
*`README.md` does not link `docs/cli.md` or say the binary is also a client*.

**Files:**
- Modify: `README.md`, `docs/deployment.md`, `deploy/chart/profgate/README.md`,
  `docs/plans/roadmap.md`, `docs/plans/install-path.md`

**The decision, and why.**
`CHANGELOG.md` is linked from all three of the documents the roadmap names,
each at the place a reader is already standing when the link becomes useful,
rather than from one place with the other two pointing at it.
`docs/deployment.md`'s upgrade section is the one that also states a change itself:
a link alone asks an operator mid-upgrade to go read a file and work out which entry applies to them.
`0.5.0` carries five breaking changes (`CHANGELOG.md:18-22`),
and the upgrade section states one of them in full — the port-selection change — and links the rest.
That is the one an operator meets without doing anything,
and it is the one the roadmap bullet names.

What `CHANGELOG.md:75-91` and [`docs/specs/gateway.md`](../specs/gateway.md) *Port resolution* record about it:
`discovery.pprof.allowedPorts` and `allowedPortNames` are removed, with their `PROFGATE_*` variables,
and replaced by the single list `discovery.pprof.allowedSelections`.
A configuration that still sets either old name fails validation and the gateway does not start,
with a message naming the replacement.
An empty `allowedSelections` — the shipped default — admits only the configured `port` or `portName`,
where an empty old allowlist admitted anything.
An operator who relied on that open behavior restores it with `- port: "*"`, `- portName: "*"`, or both,
one wildcard per old list.

- [ ] **Edit the guides**

| File | Change |
|---|---|
| `README.md`, *Quickstart*, before the first `curl` | two commands that find the arguments the examples need: `GET /v1/namespaces` lists the namespaces the caller's realm admits, and `GET /v1/namespaces/<ns>/services` the Services in one — both routes exist today (`internal/httpapi/routes.go:62`, `internal/httpapi/openapi.json`) |
| `README.md`, *Quickstart*, after the `go tool pprof` example | the `profgate` binary is also a client, in the wording `docs/authentication.md:108` already uses, linking `docs/cli.md` |
| `README.md`, *Documentation* | a `docs/cli.md` row naming the client's login, verbs, and exit codes, and a `CHANGELOG.md` row saying what changed in each release and which changes are breaking |
| `docs/deployment.md`, *Upgrades and versioning* | a paragraph saying `0.5.0` carries five breaking changes and stating the port-selection one: `allowedPorts` and `allowedPortNames` are removed and a configuration keeping either does not start, an empty `allowedSelections` admits only the configured default, and `- port: "*"` and `- portName: "*"` restore the old open behavior; a link to `../CHANGELOG.md` carries the other four |
| `deploy/chart/profgate/README.md`, the release paragraph at its head | the changelog names what each chart version changed, linking `../../../CHANGELOG.md` |
| `deploy/chart/profgate/README.md`, *Authentication* | `basic` mode with neither `auth.basic.users` nor `auth.basic.usersFile` set, and `oidc` mode with no `auth.oidc.issuer`, both fail at render time, beside the sentence at `:405` that already records the Secret refusal |
| `deploy/chart/profgate/README.md`, *Values* | the `memoryLimitWithoutPGO` row says a whole number of `Mi` or `Gi` and that anything else fails rendering; the `auth.basic.users` and `auth.oidc.issuer` rows name the refusal |

- [ ] **Finish the plan in the same commit**

This change lands the last task, so it is the one that closes the plan.
In it: tick all seven bullets of the install-path item in [`docs/plans/roadmap.md`](roadmap.md),
set line 3 of this file to `**Status:** Done`,
and insert `**Outcome:**` as line 4.

`Outcome:` names a commit or a tag, and nothing else —
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) admits no third form,
and `check_status` in [`scripts/check-repo.py`](../../scripts/check-repo.py) reads that line.
The commit it names is the one the work lands on `main` as:
the merge commit, or the head the branch fast-forwarded to.
A release tag that carries the work is the other admissible value.
A pull request is not, so do not write one there.
The roadmap's `Shipped:` line is updated in the same change and must be true at that moment;
nothing checks its form, so a pull request reference is admissible there where it is not on line 4.

The deletion is a separate commit, and has to be:
the tree a commit writes either holds this finished plan or does not,
which is the two-commit protocol
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records.
So the next commit that touches this file deletes it
and rewrites every link that cited it, which `check_links` enforces.
Run `grep -rn install-path --include='*.md' .` before that deletion to find them:
today no file outside this one names the path, so the deletion is the whole of that commit,
but a link added while the work runs is one `mise run check` would catch afterwards rather than before.

- [ ] **Validate and commit**

```bash
semlf check README.md docs/deployment.md deploy/chart/profgate/README.md \
  docs/plans/roadmap.md docs/plans/install-path.md
mise run lint && mise run test && mise run check
git add README.md docs/ deploy/chart/profgate/README.md
git commit -m "docs: link the changelog and the client"
```

---

## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite:

```bash
mise run test:e2e
```

[`.agents/rules/500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
names `deploy/` among the eight packages that need the suite on the `current` lane before a pull request,
and every task in this plan changes a file under `deploy/`.
The chart template changes are the reason the trigger is not merely formal:
`helm template` proves what renders, and only a cluster proves that what rendered applies.
The kustomize base is itself an input to the suite —
`test/e2e/overlays/default/kustomization.yaml` lists it as its one resource —
so the Namespace and the pinned tag reach a real cluster in that run and nowhere else.
The overlay's image transformer matches `ghcr.io/arloliu/profgate` by name and replaces both name and tag,
so pinning a tag in the base does not change what the suite runs.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it, on every Markdown file a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **A pinned tag in the base goes stale.**
  Nothing rewrites `deploy/base/deployment.yaml` at release,
  so the pin names the release current when this work lands and does not follow later ones.
  That is the accepted cost of not shipping `:latest`;
  the comment beside the line tells the reader to set their own version,
  and the test asserts only that the tag is not `latest`, so a stale pin is not a failing build.
  Making the release workflow rewrite the line is a change to the release path,
  which this item does not order.
- **`auth.mode: oidc` can still render a Deployment that exits at startup.**
  `auth.oidc.audience` (`internal/config/config.go:960`)
  and `auth.oidc.mapping` (`:1033`) are required in `oidc` mode as well,
  and the chart refuses neither.
  The roadmap item names the issuer, and a ticked bullet has to describe the change that landed,
  so widening the guard is a roadmap edit rather than a decision this plan may make.
  The two are recorded here so the next reader finds them.
- **An install carrying a `PROFGATE_AUTH_*` entry in `extraEnv` gets neither auth refusal.**
  That is the skip rule *The chart refuses an auth mode it can see will not start* states, and it is deliberate:
  the chart cannot read a `valueFrom`, and `PROFGATE_AUTH_MODE` moves which requirement applies at all.
  It is also the largest remaining way `basic` or `oidc` renders a Pod that exits at startup,
  larger than the audience and the mapping,
  because one unrelated `PROFGATE_AUTH_` name switches both checks off rather than leaving one key unguarded.
  Reading the literal values and skipping only the unreadable ones would narrow it,
  and is more helper than the roadmap bullet orders.
- **The two branches still print the memory limit in different forms.**
  With collection on the chart renders a byte count, with it off a quantity.
  This plan validates both and changes neither, because the roadmap does not order that difference here.
- **The base's `1536Mi` is not the figure a PGO-off gateway needs.**
  It is the figure the ConfigMap's commented-out PGO block would need,
  and it is pinned in place by `GatewayMemoryBytes` ignoring `pgo.enabled`.
  The roadmap's *Make `config validate` tell the truth* item owns that function,
  the test that asserts over it, and the question of what the base should reserve.
- **The notes are asserted as prose, and prose assertions do not run commands.**
  [`docs/decisions/chart-notes-assertions.md`](../decisions/chart-notes-assertions.md) records that limit:
  the new backend-protocol sentence is proven to appear under TLS and to stay absent without it,
  and not proven to be the right annotation for the controller an operator runs.

---

## Self-Review

- Bullet coverage, one line each:
  the Namespace (*The base creates the namespace it names*);
  the PGO-off memory guard and its default-branch test (*The disabled-PGO branch validates the value it renders*);
  the two auth refusals (*The chart refuses an auth mode it can see will not start*);
  the backend-protocol warning and basic mode's wording (*The install notes describe the release that was rendered*);
  the base's memory comment and its image tag (*The base says what it is and names the image it runs*);
  the upgrade note with the changelog links, and the README's client link and quickstart
  (*The guides link the changelog and the client*).
- Current-source facts this plan rests on, each confirmed against the tree:
  `deploy/base/` holds no `kind: Namespace`,
  five of its resources set `metadata.namespace: profgate` and the ClusterRoleBinding's subject names it too,
  and `deployment.yaml` carries `app.kubernetes.io/name` where the other files carry no labels;
  `kustomization.yaml` lists seven files and `TestKustomizationListsEveryFile` holds it to the directory,
  sorting both sides, so the order of the list is not something it can see;
  kustomize emits a Namespace ahead of every namespaced kind whatever position the list gives it,
  and its `namespace:` transformer renames a Namespace object rather than skipping it,
  both confirmed by building the base with the file added;
  `test/e2e/overlays/default/kustomization.yaml` is the one place outside `deploy/` that consumes the base,
  it is wrapped by the harness with `namespace: profgate`,
  and its image transformer matches by name and replaces the tag;
  `profgate.gatewayBaseMemoryBytes` validates `Mi` and `Gi` and is reached only through `profgate.gatewayMemoryBytes`,
  so `profgate.resources`'s else branch never calls it;
  `ingress.yaml:3` refuses with `fail` and `profgate.natsURL` with `required`,
  and `profgate.validateAuthSecret` is a validation-only helper called from `deployment.yaml:2`;
  `profgate.config` merges the raw `config:` block over the structured keys with `mergeOverwrite`,
  which is why `profgate.natsURL` decides by presence and why the auth guards do too;
  `values.yaml` ships `auth.basic.users: []`, `auth.basic.usersFile: ""`, and `auth.oidc.issuer: ""`;
  startup validation requires a user set in `basic` mode and an issuer in `oidc` mode,
  and also an audience and a mapping, which this plan does not guard;
  `NOTES.txt` promises a user list and then prints realms, and its `ingress.enabled` block prints paths only;
  the binary reads `PROFGATE_AUTH_MODE` and `PROFGATE_AUTH_OIDC_ISSUER` on top of the file,
  while `profgate.validateNoDerivedOverrides` already refuses the four file-path `PROFGATE_AUTH_*` names;
  `renderNotes`, `renderFailure`, and `render[T]` already exist in `deploy/chart_test.go`,
  and `loadRenderedConfig` renders `configmap.yaml` alone;
  `TestDeployment` asserts the image string exactly,
  and `TestDeploymentMemoryLimit` asserts the base's limit against `GatewayMemoryBytes` over the base's own ConfigMap;
  `GatewayMemoryBytes` never reads `PGO.Enabled`;
  `/v1/namespaces` and `/v1/namespaces/{namespace}/services` are in the route table and the OpenAPI document;
  no file under `docs/`, `README.md`, or the chart README links `CHANGELOG.md`,
  and `README.md` links no `docs/cli.md`;
  `docs/authentication.md` already carries the sentence that the binary is also a client;
  `.github/workflows/release.yml` rewrites no file under `deploy/base/`.
- Decided here, with the reason stated at the task that carries it:
  a Namespace object rather than a prose step;
  a validation-only helper rather than an extension of `profgate.validateAuthSecret`;
  the auth guard standing down for any `PROFGATE_AUTH_*` entry in `extraEnv` rather than banning the override;
  validation through the shared helper on the PGO-off branch with the rendered form unchanged
  and the accepted input set narrowed to the grammar the release already documents;
  the base's memory comment moving rather than its figure;
  a pinned tag with a property assertion rather than a string one;
  the changelog linked from all three documents
  and the port-selection breaking change restated where an operator upgrades.
- Left to the implementer: the exact wording of every comment and message,
  the subtest helper names, and the released tag the base pins,
  which is whatever the current release is when the work lands.
