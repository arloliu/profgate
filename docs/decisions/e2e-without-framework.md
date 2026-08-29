# End-to-end tests use plain `go test` and a thin harness, not a framework

**Decision:** `test/e2e/` is an ordinary Go test package behind `//go:build e2e`
that shells out to a pinned `kind` binary, loads images with `kind load`,
applies `deploy/` through kustomize overlays, and asserts with `client-go` and `net/http`.
No end-to-end framework is imported.

## Context

The candidates were `kubernetes-sigs/e2e-framework`, kuttl, and kyverno chainsaw.

`e2e-framework` v0.7.0 (2026-04-21) followed v0.6.0 by fifteen months,
has four approvers, and changed provider signatures in that release.
Its kind support shells out to whatever `kind` binary is on `PATH`,
so per-lane binary pinning (see [`kind-frozen-lanes.md`](kind-frozen-lanes.md))
would need a wrapper anyway.
Importing it raises the module's `client-go` to at least v0.35.3 through minimum version selection,
so the framework rather than the project would decide the client-go line.
Both of these claims are re-examined under *Revisit*.

kuttl and chainsaw are YAML-assertion harnesses.
The scenarios here assert on HTTP response bodies
(fetched profiles must parse with `github.com/google/pprof/profile`)
and on response headers, which YAML assertions express poorly.

## Consequences

- The harness is project code, `test/e2e/harness_test.go`, and is maintained here.
- Scenarios are plain Go: any assertion the standard library can express is available.
- Cluster lifecycle, lane selection, and image loading are owned by `TestMain`.
- Revisit only if the harness grows past what a reader can hold in one sitting
  or a framework gains per-lane binary pinning.

## Revisit

The size trigger has fired.
`test/e2e/harness_test.go` is 1672 lines carrying 6 types and 59 top-level functions and methods
(`wc -l`, `grep -c '^type '`, `grep -c '^func '`),
where this record's *Consequences* said "roughly a few hundred lines";
that line is corrected above.
Both reasons the *Context* gives against `sigs.k8s.io/e2e-framework` were re-examined against its v0.7.0,
which is still its newest release.

The `client-go` reason no longer bites.
v0.7.0 requires `k8s.io/client-go` v0.35.3 and this module is already at v0.36.4,
so importing it would raise nothing today;
the framework would decide the line only in a release whose floor rises above the project's.

The kind reason is wrong as written.
`support/kind.WithPath` names the binary a cluster runs,
and the framework uses that path when it exists rather than resolving `kind` on `PATH`,
so pinning a lane's binary needs no wrapper.
Its `WithVersion` writes a package-level variable rather than the cluster,
which one lane per test process does not expose.

The decision stands, on a reason neither of those was.
The framework supplies cluster lifecycle and a step vocabulary.
Lifecycle is roughly 330 of the harness's 1672 lines —
`TestMain`, lane selection, cluster creation, image building and loading, the connection, and the shell helpers —
and 40 of those are `dropOCIIndex`,
which rewrites an image archive for a registry that will not serve an OCI index,
a thing no framework offers.
The 5223 lines of scenarios assert HTTP bodies and headers, which the step vocabulary does not shorten.
Importing it would add a module and rewrite working lifecycle code without touching what made the harness large.

What made it large is subject matter that is not cluster lifecycle.
NATS identity, users, server deployment, and store provisioning are roughly 425 lines of the file;
gateway configuration rendering, Secret application, and port forwarding are most of the rest.
Splitting `harness_test.go` along those subjects is what the size trigger calls for,
and it is not done here.
