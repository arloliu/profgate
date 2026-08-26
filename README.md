# profgate

Profgate is a standalone, Kubernetes-aware pprof gateway for Go workloads:
one HTTP entry point that resolves a Kubernetes Service to its ready backend Pods,
picks one, and proxies a standard `/debug/pprof` profile straight from that Pod.
An optional PGO mode, off by default, collects representative CPU profiles on a schedule or on demand
and merges them into an artifact for `go build -pgo=`.

Profgate requires no Kubernetes write permissions.
It only lists and watches Services and EndpointSlices and gets, lists, and watches Pods in authorized namespaces,
connects only to the configured application pprof port,
and, in PGO mode, touches only its own `PROFGATE_*` NATS stores.

## Features

- **One entry point for cluster-wide profiling.**
  Ask for a profile by namespace and Service; the gateway finds a Pod and streams the bytes back.
- **All eight Go profile types**: `cpu`, `trace`, `heap`, `allocs`, `goroutine`, `mutex`, `block`, `threadcreate`.
- **Target discovery from informer caches** via the Service selector and EndpointSlices,
  with strict Pod eligibility checks — profiles come only from ready, unterminated Pods,
  and the chosen Pod is confirmed against the API server before the connection is made.
- **Pod selection you can steer**: random by default, or pinned with the `pod` and `version` query parameters.
- **Realm ACLs**: static allowlists of namespaces, services, and profiles per realm, exact string or `*`.
- **HTTPS on the API port** with the certificate re-read while the gateway runs,
  so a rotation — cert-manager or otherwise — needs no restart.
- **Prometheus metrics and JSON audit logs**: every `/v1` request emits one audit record and one labeled observation.
- **PGO CPU-profile collection**, opt-in:
  scheduled or on-demand Collections coordinated across replicas through NATS JetStream KV,
  merged in memory, stored in a NATS Object Store.

## Quickstart

Install the chart — two replicas, read-only RBAC, and a wide-open default realm:

```bash
helm install profgate oci://ghcr.io/arloliu/charts/profgate --version X.Y.Z \
  --namespace profgate --create-namespace
```

`X.Y.Z` is the chart version of the latest [release](https://github.com/arloliu/profgate/releases) —
the release tag without its leading `v`.

Reach the gateway with a port-forward;
it stays in the foreground, so leave it running in another terminal:

```bash
kubectl -n profgate port-forward svc/profgate 8080:8080
```

List the eligible Pods of a Service:

```bash
curl "http://localhost:8080/v1/namespaces/<ns>/services/<svc>/targets"
```

Fetch a profile and open it:

```bash
curl -o heap.pprof "http://localhost:8080/v1/namespaces/<ns>/services/<svc>/profiles/heap"
go tool pprof heap.pprof
```

`go tool pprof` also takes the gateway URL directly:

```bash
go tool pprof "http://localhost:8080/v1/namespaces/<ns>/services/<svc>/profiles/cpu?seconds=30"
```

The one requirement on the application:
its Pods must serve Go's `net/http/pprof` handlers on the port the gateway is configured to reach —
`discovery.pprof.port` (6060 by default) or `discovery.pprof.portName`.

## Documentation

- [`docs/api.md`](docs/api.md) — the HTTP API: routes, parameters, errors.
- [`docs/configuration.md`](docs/configuration.md) — the configuration file, environment overrides, realms.
- [`docs/deployment.md`](docs/deployment.md) — running the gateway in a cluster.
- [`docs/pgo.md`](docs/pgo.md) — collecting CPU profiles for Profile-Guided Optimization.
- [`deploy/chart/profgate/README.md`](deploy/chart/profgate/README.md) — the Helm chart and its values.
- [`docs/specs/gateway.md`](docs/specs/gateway.md) and [`docs/specs/pgo.md`](docs/specs/pgo.md) —
  the accepted designs the gateway and PGO collection are built from.

The guides track `main`;
when running a released chart, read them at its tag: `https://github.com/arloliu/profgate/tree/vX.Y.Z/docs`.

## Compatibility

- **Kubernetes 1.23 or newer**; only API fields present in the 1.23 `discovery.k8s.io/v1` schema are read.
- **No write RBAC**: the ClusterRole grants `list` and `watch` on Services and EndpointSlices
  and `get`, `list`, and `watch` on Pods — nothing else, and the chart offers no way to widen it.
- **Ops endpoints** on their own listener, `:9090` by default: `/healthz`, `/readyz`, and `/metrics`.
- **Image**: `ghcr.io/arloliu/profgate` — distroless, static, `linux/amd64` and `linux/arm64`.

Access control today is the realm ACL plus the network boundary;
`auth.mode: disabled` is the only authentication mode.

## License

[Apache-2.0](LICENSE).
