# Reaching a workload with no Service

Date: 2026-08-29
Scope: `docs/specs/gateway.md` and `docs/specs/pgo.md` (both `Accepted`),
`.agents/rules/800-security-invariant.md`, `internal/k8s`, `internal/pgo`, `deploy/`.
Method: two independent investigations —
one here, one written by an external model with no knowledge of this document's conclusions —
each derived independently from source and spec,
then compared point by point and merged.
Disagreements, and each side's arguments, are kept in the text.

## Verify this first, or everything below is void

The phrase "pull-based workers do not accept inbound traffic" hides a premise.
profgate samples by dialing out to the pprof listener at `PodIP:port` —
the Service is indeed unnecessary for that connection,
but the listener, and a NetworkPolicy that admits it, are necessary
([`docs/specs/gateway.md`](../specs/gateway.md) *Network*, `internal/proxy/proxy.go`).

If these workers' "no inbound" also covers this connection from profgate —
no pprof listener open, or a NetworkPolicy that does not admit the gateway —
then no discovery mechanism or RBAC change helps.

This is the first thing to measure before any work starts,
and it costs one `curl` against one worker Pod.

## What a Service does today

*Eligibility* decides whether a Pod is a target:

1. the Service exists in the cache and its `spec.selector` is non-empty;
2. an EndpointSlice labeled `kubernetes.io/service-name=<svc>` has a `targetRef`
   that points to that Pod by name and UID;
3. the Pod's labels match the selector, and it is `Running`, `Ready`, and not terminating;
4. a pprof port resolves for it.

The address comes from the endpoint's first address,
cross-checked against the Pod's `status.podIPs` by rule 7.

**PGO takes the same path — this is verified, not assumed:**
*Rounds* calls `discovery.Targets(ns, svc, PortSelection{})` on every round;
scheduling only walks `(ns, svc)` pairs that have an override in `PROFGATE_CONFIG`;
the KV keys are `schedule.<ns>.<svc>.<slot>` and `active.<ns>.<svc>`;
policy is written at `PUT /v1/namespaces/{ns}/services/{svc}/pgo`.

A realm's three lists (`namespaces`, `services`, `profiles`)
match only by exact DNS-1123 label or `"*"`; there is no glob.
**Every authorization decision keys on the Service's name.**

In sum, a Service is (a) a container for a selector,
(b) the trigger for an EndpointSlice,
and (c) the naming key realm and PGO share.
**It is not a network destination** —
the spec says plainly that the application Service does not need to expose the pprof port.

## Adding `apps/deployments` read permission alone changes nothing

Both investigations reached this same conclusion independently,
and it is the most important point in this document.

No Service means no EndpointSlice,
and eligibility rules 2, 3, 5, and 7 all take an EndpointSlice as input.
Production code builds only three informers — Service, Pod, EndpointSlice;
every route is named by a Service;
`Targets` rejects a nonexistent Service before it checks any Pod.
Changing RBAC does not produce an informer, a lister, a route, or an authorization field,
and it does not change eligibility.

So letting profgate read Deployments is **necessary but not sufficient**.
The real work is a separate discovery path that reads only Pod objects,
and reading the Deployment is just the step in it that gets the group's name and selector.

That Pod-only path is itself **simpler** than the current Service path:
the address comes directly from `pod.status.podIPs`;
readiness comes from the Pod's own `Ready` condition (rule 6 already reads it);
the pre-connection `Confirm` (re-reading the Pod once by UID) stays exactly as it is —
the actual gatekeeper is still there.
What is lost is rules 2, 3, 5, and 7 —
a second endorsement of the address,
and the address's upstream source was always the Pod object anyway.
Two new design decisions follow: which IP family to pick
when there is no slice `addressType` to follow,
and `explain` needs new exclusion reasons because every `endpoint_*` row stops applying.

## The premise that pprof is not sensitive

It conflicts with the project's own threat model:
spec §3.5 says profiling output is sensitive production data
(stack traces, package names, request strings),
and the gateway deliberately hides Pod IPs and the names a realm rejects.

But this conflict **changes the argument rather than ending it**.
It closes off the path of "profiles are not sensitive,
so realm and disclosure controls can be bypassed,"
but it does not prove Deployment metadata is unreadable,
and the verb being requested is indeed read-only.

One more point needs to be clear: **"read-only" is not this project's complete boundary.**
The invariant enumerates resources and verbs one by one,
says a compromised gateway reaches exactly this far and no further,
and **excludes every `apps/*` workload resource even where the verb is read-only**.
So this has to go through §800's formal relaxation process;
it cannot be waved through with "it's read-only."

## Candidate mechanisms

| Mechanism | New K8s permission | Grows automatically | Needs workload-owner cooperation | Verdict |
|---|---|---|---|---|
| One headless Service per workload | None | No | Yes (a manifest addition) | Ruled out by the premise |
| An external controller generates the Service | None (on profgate's side) | Yes | No | Moves write permission elsewhere, see below |
| profgate creates the Service itself | **write** | Yes | No | Rejected in one line |
| One catch-all Service with a broad selector | None | Yes | No | Destroys the Service's role as an identity, see below |
| A named target group in profgate's config | None | No | No | Ruled out by scale and `restart` semantics |
| A Pod label convention (the Prometheus pattern) | None | Yes | Yes (a PodTemplate change) | Ruled out by the premise |
| A direct Pod route | None | N/A | No | A useful diagnostic escape hatch, not an answer |
| Grouping by the Pod's `ownerReferences` alone | None | Yes | No | Gives a ReplicaSet identity, see below |
| A native Deployment target | `apps/*` read-only | Yes | No | **Recommended** |

A few of these need expanding:

**profgate creates the Service itself** —
the invariant's first sentence is that profgate requires no Kubernetes write permissions.
Not up for discussion.

**An external controller generates a profiling-only Service** —
profgate's boundary does not move at all, the most compatible workaround.
The cost is that it still produces the Service object the premise rules out,
and it moves cluster write permission into another component.

**A catch-all Service with a broad selector** —
fewer objects, but a caller granted authorization on the Service gets every matching workload,
and exact Service authorization loses its meaning.
PGO is worse off: it re-resolves and merges within a Service's boundary,
separating different builds by their version label,
and a catch-all would merge unrelated binaries
that happen to share a version string into the same artifact.

**A named target group in profgate's config** —
zero new K8s permission (Pod `list`/`watch` is already cluster-wide),
it has a name so a realm has a key for it,
and the change is to profgate's own ConfigMap.
But every key under `discovery.*` is `restart`
(only `auth.anonymousRealm` and `realms` are `hot`),
so for a set that keeps growing this means restarting the gateway for every workload added.

**A direct Pod route** —
profgate already lists Pods cluster-wide, has their IPs, can resolve a port,
and can confirm before connecting,
so a route like `/pods/{pod}` **needs no new K8s permission**.
It cannot durably name a set of replicas,
and it cannot give PGO a persistent workload grouping, so it is not the answer;
but it is the lowest-cost diagnostic escape hatch available today.

**Grouping by `ownerReferences` alone** —
needs no new permission, but a Deployment's Pod is directly owned by its **ReplicaSet**,
so the identity this gives is a ReplicaSet,
and one Deployment splits into several across a rollout.
Stripping the hash suffix off a ReplicaSet's name to recover the Deployment's name is a guess,
and the project's rules forbid guessing when an API object can answer instead.
(The helper in `test/e2e/scenarios_test.go` that orphans a Deployment together with its ReplicaSet,
leaving only the Pod, is ready-made evidence that this ownership chain exists.)

## The real decision axis: two strengths of membership

A native Deployment target has two possible designs,
and **they pay a different invariant cost**.
This is where the two sources behind this document disagree;
both sides' arguments are listed.

**selector-only**: read the Deployment for its name and `spec.selector`,
and use that selector to pick Pods from the existing Pod cache.
- Permission: `list`/`watch` on `apps/deployments` — **two tuples, nine total**.
- Argument for it: this is exactly what a Service does today.
  A Service also only matches labels and never checks ownership;
  a Deployment's `spec.selector` carries no `pod-template-hash`
  (that belongs to the ReplicaSet's selector),
  so it hits Pods across every ReplicaSet,
  its behavior during a rollout is identical to a Service's,
  and *Rounds* already resolves a single version and filters on it.
- **Key advantage**: the invariant's sentence
  that which controller owns the Pod is irrelevant to profiling **survives**,
  because the Deployment is read for its selector and name, not for an ownership relationship.

**owner-verified**: additionally read ReplicaSet,
and require the chain Pod owner UID → ReplicaSet owner UID → Deployment UID to hold.
- Permission: add `list`/`watch` on `apps/replicasets` — **four tuples, eleven total**.
- Argument for it: this relationship **decides the caller's authorization**.
  The asymmetry is that a Service's selector is written by hand by an operator,
  opted into one at a time;
  once a Deployment route exists, **every** Deployment in the cluster is implicitly nameable.
  When labels collide, naming Deployment A returns B's Pods —
  the name lies, and the exposure is much larger.
- Cost: the invariant's sentence that which controller owns the Pod is irrelevant
  **has to be deleted**.

**This document's position**: this is a spec decision, not an RBAC edit,
and it should be argued out in the spec before any code is written.
If one has to be picked right now: **owner-verified**,
because that relationship controls authorization, and authorization should be conservative.
But selector-only gives up one fewer invariant claim,
and if the fleet's label convention really is unique and stable,
it is the smaller change.
**This choice hinges on something this document cannot answer from the repository:
whether labels in the field are unique and stable.**

## The relaxation request §800 requires

1. **What capability is needed, and why the existing three read-only resources cannot supply it.**
   A stable workload identity that does not depend on a Service,
   along with its membership selector and UID,
   has to come from the cluster's own objects —
   workload owners will not add a Service or a label to cooperate,
   and an operator-side config file cannot keep up with a set that keeps growing.
   A Pod object carries labels, an IP, readiness, and a **direct** owner reference,
   but not a Deployment's selector, and not the ownership edge from ReplicaSet to Deployment.
   When a workload deliberately has no Service, Service and EndpointSlice supply none of this.
   The existing Pod `get` remains sufficient for pre-connection identity and address confirmation,
   so no new `get`, subresource, or write verb is needed.

2. **What a compromised gateway gains.**
   It can read Deployment objects cluster-wide
   (and, under owner-verified, ReplicaSet objects too),
   including their names, labels, selectors, replica topology, Pod template,
   image references, and rollout configuration and status.
   It still cannot modify them, cannot create a Service, cannot exec,
   cannot read a Secret or a log, and cannot port-forward.
   **Its raw network reach does not change**:
   the current threat model already says a compromised gateway can read every Pod IP
   and connect to an admitted port wherever NetworkPolicy allows it.
   The new attacker capability is **information at the workload-controller layer**,
   not new network reach.

3. **Narrower alternatives considered, and why they fail.**
   Pod name alone — keeps current permissions,
   but a name is short-lived and cannot define the cross-replica, cross-round identity PGO needs.
   Caller-chosen Pod label — keeps current permissions,
   but letting the caller pick a selector amounts to granting namespace-scoped Pod authorization;
   turning it into an allowlist falls back to configuring one at a time.
   `ownerReferences` alone — the identity it exposes is the ReplicaSet's,
   one workload splits apart across a rollout,
   and stripping the name suffix is an unverifiable guess.
   An externally generated Service — keeps profgate's boundary intact,
   the most compatible workaround,
   but it produces the Service object the premise rules out,
   and moves cluster write permission into another controller.

**Four claims the boundary text has to give up**
(kept in sync across `.agents/rules/800-security-invariant.md`, `AGENTS.md`,
`README.md`, and `docs/specs/gateway.md`):

- "observes Services, Pods, and EndpointSlices" becomes an explicit list
  that also names Deployment (and ReplicaSet);
- "serves each caller only the namespaces, **Services**, and profiles"
  becomes a typed claim covering both Service and explicitly admitted Deployment;
- "Three resources, seven read tuples" becomes four or five resources and nine or eleven tuples;
- "no `apps/*` workload resource" is no longer true;
  "which controller owns the Pod is irrelevant" is also no longer true under owner-verified.

**What stays unchanged**: no write permission of any kind is needed,
the direct-HTTP model, the explicit boundary on ports,
touching only the three `PROFGATE_*` stores, container hardening,
and the mechanism that client-go has exactly one importer.

**Three checking mechanisms that must change in the same change**:
the golden ClusterRole test that pins the seven tuples,
the startup preflight that exercises only those seven and exits on `403`,
and the recording transport that asserts only those seven are touched.

## Recommended design boundary

Internally this converges on one typed target reference,
and a Service's behavior does not change at all:

```text
TargetRef { Kind: service|deployment, Namespace, Name }
```

- `service`: every current rule stays exactly as it is.
- `deployment`: resolves from the cached Deployment to Pods
  (via the cached ReplicaSet under owner-verified),
  requires the selector to match and the Pod to be `Running`/`Ready`/not terminating,
  follows the same global port policy, and takes the address from `status.podIPs`;
  every interactive or PGO connection is preceded by the existing Pod `get`
  and UID/address confirmation.

**Put the kind in the path; do not infer it from the name** —
for example `/v1/namespaces/{ns}/workloads/deployments/{name}/...`.
This shape does not need its route grammar changed again the next time another controller kind is added.

**The kind has to run through every piece of persistent state**:
the audit record, the metrics endpoint's closed value set, PGO's policy key,
the `active` key, the Collection record, the manifest, the idempotency scope,
and artifact lookup.
The current NATS keys scope only by namespace and name;
**omitting the kind lets a Service and a Deployment with the same name collide**.

**A realm gains a `deployments` list**,
with the same grammar as Services (an exact name or `"*"`).
**When the field is absent it must admit zero Deployments**,
so existing configuration does not quietly widen after an upgrade —
reusing `services:` would give every realm that wrote `["*"]` all of them the moment it upgrades.
An operator who wants a continuously growing set to be admitted automatically can write `deployments: ["*"]` within their admitted namespace.
The Service and Deployment lists should stay separate, or return typed entries;
they should not be folded into the same untyped `services` response.

**Both the interactive path and every PGO round use the same target provider**,
to keep PGO's existing property that it re-resolves every round as Pods leave and join,
and to avoid building an on-demand feature that scheduled collection cannot use.

## Proposed roadmap item text

Placed after item 7 and before item 8 (item 8 is a pure removal and can yield its place);
work starts only after item 6 lands:
the route table, error-code registry, and OpenAPI document it is changing are the same three places a new target kind would land in.

> ### A workload with no Service can be sampled too
>
> profgate can reach a workload only through a Service,
> and pull-based workers (Temporal, Conductor, queue consumers)
> open outbound connections to a broker and accept no inbound business traffic,
> so putting a Service in front of them serves no purpose.
> This is a whole class of workload, and the set keeps growing.
>
> - [ ] Revise `docs/specs/gateway.md`: a typed target reference,
>   with a `deployment` kind that resolves Pods without a Service or EndpointSlice,
>   taking the address from `status.podIPs` and readiness from the Pod itself,
>   with pre-connection confirmation unchanged;
>   `explain`'s exclusion reasons cover this path; and a rule for choosing an IP family.
> - [ ] Revise `docs/specs/gateway.md` *Permission Boundary* using the three-part format
>   §800 requires, and keep the boundary text in sync across
>   `AGENTS.md`, `README.md`, and `.agents/rules/800-security-invariant.md`;
>   move the golden ClusterRole test, the startup preflight,
>   and the recording transport in the same change.
> - [ ] Revise `docs/specs/gateway.md` *Realm structure*:
>   add a `deployments` list that admits zero when the field is absent.
> - [ ] Revise `docs/specs/pgo.md`: the kind enters the policy key, the `active` key,
>   the Collection record, and the manifest;
>   a Service and a Deployment with the same name must not collide.
> - [ ] Revise `docs/specs/ui.md` and `docs/specs/cli.md`: listings and routes carry the kind.
> - [ ] Write an implementation plan.
>
> Spec: `docs/specs/gateway.md`, `docs/specs/pgo.md`, `docs/specs/ui.md`,
> `docs/specs/cli.md` (all need revision).
> Prerequisite: confirm these workers actually serve a pprof listener on their Pod IP,
> and that NetworkPolicy admits the gateway.

## Unconfirmed items

None of the following can be answered from the repository,
and the first two would change the design:

- **Whether these workers really have a pprof listener open on their Pod IP,
  and whether NetworkPolicy admits the gateway.**
  If not, every mechanism in this document is void.
  Measure this before any work starts.
- **Whether Pod labels in the field are unique and stable.**
  This decides the choice between selector-only and owner-verified.
- **Whether this set of workloads is entirely Deployments.**
  If there are also StatefulSets, DaemonSets, Jobs, bare Pods, or custom controllers,
  `apps/deployments` is only a partial answer,
  and each kind needs its own membership rule and an explicit RBAC resource.
  Until that inventory is done,
  do not describe "supports Deployment" as "supports every workload with no Service."
- **Whether these workloads declare a named pprof containerPort.**
  A numeric default port needs no declaration; a named one does.
- Whether a headless Service's EndpointSlice satisfies eligibility rules 5 and 7 —
  this is unrelated to this document's recommendation,
  but if any of these workloads can in fact take a Service,
  that path is worth confirming with a real test first.
