# 100 - Project Map

Navigation aid, not ground truth.
This repository has no Go code yet — only a pinned toolchain.
Everything below is **planned** structure
drawn from [`docs/specs/profgate-design.md`](../../docs/specs/profgate-design.md),
which is itself a draft.
Confirm any path, package, or import claim with `ls` or `grep` before relying
on it ([000](000-agent-contract.md)).

## Identity

- **Project:** Profgate. **Module:** `github.com/arloliu/profgate`.
  **Binary:** `profgate`.
- **Toolchain:** pinned in [`mise.toml`](../../mise.toml) — read the versions
  there rather than from a rule file that would go stale.
  Run tools through `mise exec --` or an activated mise shell.
  `GOTOOLCHAIN=local` turns a `go.mod` that outruns the pinned toolchain into
  a loud failure instead of a silent download.
- **Module language version:** when `go.mod` lands it declares `go 1.26.0` —
  the oldest toolchain that can build Profgate, so consumers are not forced to
  upgrade in step with this repo.
  The pinned toolchain stays in the same minor series, which keeps the two
  effectively aligned: Go adds standard library API in minor releases, not
  patch releases, so a symbol that builds here builds for anyone on 1.26.
  Raising the pin to a newer minor reopens that gap, and `go vet`'s
  `stdversion` analyzer is what would close it — keep it enabled when
  `.golangci.yml` lands.
  `mise run check` holds the `go` directive itself at `1.26.0`, which
  `go mod init` and `go mod tidy` would otherwise rewrite to the running
  toolchain's version.
- **Language:** Go. **Deployment target:** a Linux container running as a
  normal Kubernetes Deployment, non-root, read-only root filesystem, no Linux
  capabilities.
- **Kubernetes baseline:** 1.23, using only stable API fields available at
  that release. 1.23 and 1.24 are first-class integration-test targets.
- **Runtime dependencies:** the Kubernetes API and NATS JetStream KV.
  No database, cache, message broker, PVC, or object storage.
  No Kubernetes CRDs and no operator.

## The Kubernetes Seam

**Exactly one package imports `k8s.io/client-go`.**
Everything else reaches Kubernetes through that package's interface.

The seam carries two invariants at once, which is why it is worth its cost:

- **Compatibility.** Client version differences and EndpointSlice field
  availability across 1.23 through current stay in one place instead of
  spreading through the codebase.
- **Permission boundary.** The set of Kubernetes calls Profgate can make is
  the set of methods this package exposes, so the boundary is readable in one
  file and checkable by `grep` rather than by reviewer memory
  ([800](800-security-invariant.md)).

Adding a Kubernetes capability means widening that interface, in that package,
where the cost is visible.

## Planned Structure

```
cmd/profgate/          // CLI entrypoint: serve, config validate, version
internal/k8s/          // the Kubernetes seam; sole importer of client-go
internal/proxy/        // pprof proxying to resolved Pod endpoints
internal/pgo/          // scheduling, collection, merge, manifest
internal/natskv/       // PROFGATE_CONFIG / PROFGATE_JOBS access
internal/httpapi/      // external HTTP API, auth, access realms
deploy/                // ClusterRole, Deployment, Service, NetworkPolicy
scripts/               // repository checks; check-repo.py exists today
docs/                  // see docs/README.md
```

Only `scripts/` and `docs/` exist.
Treat the list as the shape the design implies, and revisit it when the design
reaches `Status: Accepted`.

## External HTTP API

Resource-oriented and product-neutral.
The name `profgate` does not appear in versioned API paths:

```
/v1/namespaces/{namespace}/services/{service}/profiles/{profile}
/v1/namespaces/{namespace}/services/{service}/collections
```

## Documentation

[`docs/README.md`](../../docs/README.md) maps what each documentation
directory holds and how it ages.
Lifecycle rules for specs and plans:
[900](900-design-and-review-loops.md).
