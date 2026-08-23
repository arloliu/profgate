# 800 - Security Invariant

Apply to any change that touches Kubernetes access, NATS access, RBAC
manifests, authentication, authorization, or the container security context.

## The Boundary

This file holds the authoritative wording of the boundary; the copy in the
root [`AGENTS.md`](../../AGENTS.md) tracks it.

> Profgate requires no Kubernetes write permissions.
> It observes Services, Pods, and EndpointSlices in authorized namespaces,
> connects to explicitly permitted application pprof ports, and manipulates
> only its dedicated `PROFGATE_*` NATS stores.

A compromised gateway reaches exactly this far, and no further.
The boundary is the reason Profgate is deployable in a production cluster at
all; a version of it that needs broader access has lost its argument for
existing.

### Kubernetes

| API group | Resource | Verbs |
|---|---|---|
| core (`""`) | `services` | `list`, `watch` |
| core (`""`) | `pods` | `get`, `list`, `watch` |
| `discovery.k8s.io` | `endpointslices` | `list`, `watch` |

Three resources, seven read tuples.
`list` and `watch` feed the informer caches every selection reads from;
the single `get`, on Pods, reads one named Pod in two places:
the startup preflight, and the confirmation read issued right before a proxy connection.
That read verifies the selected Pod's identity and address against the API server;
Service membership is still decided from the cache and is as old as the cache,
which the gateway spec states as its contract rather than hiding.
A startup preflight exercises exactly these seven tuples and exits on a `403`,
so an under-privileged ClusterRole is a crash rather than a silent retry;
`authorization.k8s.io` review resources are not used for this.
Profiling reaches applications over ordinary HTTP to their pprof ports, so
`pods/exec`, `pods/log`, and `pods/portforward` stay out, along with
`secrets`, `configmaps`, `nodes`, every `apps/*` workload resource, and every
mutating verb.

The target model stops at the Pod: `Service -> EndpointSlice -> Pod`.
Which controller owns the Pod does not affect profiling, which is what keeps
`apps/*` access unnecessary.
`spec.nodeName` on the Pod already records which node hosted a sample, which
is what keeps `nodes` access unnecessary.

### NATS

`PROFGATE_CONFIG`, `PROFGATE_JOBS`, and optionally `PROFGATE_ARTIFACTS`.
Stream, bucket, and account administration belongs to whoever provisions the
cluster; Profgate uses stores that already exist.

### Container

Non-root, no Linux capabilities, no privilege escalation, read-only root
filesystem.
The gateway has no writable volume at all;
if the PGO draft is accepted, its ephemeral profile bytes are confined to an
`emptyDir` and nothing else becomes writable.
Host namespaces, host paths, and `SYS_PTRACE` stay out — Profgate talks HTTP
to applications rather than attaching to their processes.

## Two Mechanisms

Prose does not hold a boundary. These do.

### One Importer of client-go

The package named in [100](100-project-map.md) is the only non-test importer of `k8s.io/client-go`.
That makes the invariant greppable:

```bash
grep -rl 'k8s.io/client-go' --include='*.go' --exclude='*_test.go' . \
  | grep -v '^./internal/k8s/' | grep -v '^./test/'
```

Empty output is the passing state.
The end-to-end harness under `test/` drives the cluster with the tester's
kubeconfig, not the gateway's ServiceAccount; its client-go use is test
tooling, which is why the grep excludes that tree.
Wire this as a test or CI step once Go code exists.

### Golden ClusterRole

The shipped ClusterRole manifest is pinned by a golden test.
Any change to `apiGroups`, `resources`, or `verbs` turns the test red, which
puts the widening in the diff where a reviewer argues about it.

### What Each One Actually Catches

The grep catches a package outside the seam reaching for client-go directly.
The golden test catches a manifest granting access nobody needs.

Neither catches the case in between: `internal/k8s` legitimately imports
client-go, so a mutating call added *inside* the seam leaves both green and
fails in production instead of CI.

What closes that gap is structural rather than automated, and it is the reason
the seam exists at all — **the methods `internal/k8s` exposes are the set of
things Profgate can do to Kubernetes** ([100](100-project-map.md)).
Reviewing that interface is reviewing the capability set, which works only
while the interface stays small enough to read in one sitting.
Keeping it that small is the actual defense; the two automated checks guard
its edges.

## Widening the Boundary

Broader access is a design decision, argued in
[`docs/specs/gateway.md`](../../docs/specs/gateway.md) before
it is written in code.
A proposal to widen carries:

1. The capability the feature needs, and why the existing three read-only
   resources cannot supply it.
2. What a compromised gateway gains — stated as the attacker's new capability,
   not as the feature's benefit.
3. The narrower alternative that was considered and why it failed.

Convenience is not an argument.
Reaching for a new Kubernetes verb usually means the feature is asking
Profgate to become an operator, and the answer is to reshape the feature.
