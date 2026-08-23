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

kuttl and chainsaw are YAML-assertion harnesses.
The scenarios here assert on HTTP response bodies
(fetched profiles must parse with `github.com/google/pprof/profile`)
and on response headers, which YAML assertions express poorly.

## Consequences

- The harness is project code, roughly a few hundred lines, and is maintained here.
- Scenarios are plain Go: any assertion the standard library can express is available.
- Cluster lifecycle, lane selection, and image loading are owned by `TestMain`.
- Revisit only if the harness grows past what a reader can hold in one sitting
  or a framework gains per-lane binary pinning.
