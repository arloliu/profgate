# Kubernetes 1.23 and 1.24 end-to-end lanes run on frozen kind versions

**Decision:** the end-to-end matrix tests Kubernetes 1.23 and 1.24 with
`kindest/node` images pinned by digest and a kind binary pinned per lane,
mirrored to a registry the project controls.
These lanes are frozen:
never upgraded, only removed when the production clusters leave those versions.

## Context

Production clusters run vanilla kubeadm at 1.23 and 1.24, both past upstream end of life.
kind stopped publishing 1.23 and 1.24 node images with its v0.22.0 release (2024-02-14),
and its release notes disclaim node-image compatibility across kind releases.
The last published images are
`kindest/node:v1.23.17@sha256:14d0a9a892b943866d7e6be119a06871291c517d279aedb816a4b4bc0ec0a5b3`
and `kindest/node:v1.24.17@sha256:bad10f9b98d54586cba05a7eaa1b61c6b90bfc4ee174fdc43a7b75ca75c95e51`.

## Alternatives considered

- **testcontainers-go k3s module** with `rancher/k3s:v1.23.17-k3s1` / `v1.24.17-k3s1`.
  Still pullable, no kind binary to version, one container per cluster.
  Rejected because k3s swaps the CNI and controller set of a kubeadm cluster
  and needs a privileged container;
  the production clusters are kubeadm, and the behavior under test
  (EndpointSlice controller, readiness propagation) should come from the same components.
- **Drop the old lanes and rely on "1.23-stable fields only" code review.**
  Rejected while real 1.23/1.24 clusters exist: field availability is reviewable,
  behavior is not.
- **kwok or envtest for the old API-server versions.**
  Rejected: neither runs a kubelet, so no Pod is reachable and the proxy path is untested.

## Consequences

- `test/e2e/versions.yaml` is the only place lane definitions live;
  the harness and CI matrix both read it.
- The kind binary is installed per lane through mise (`mise x kind@<version>`).
- A frozen lane may break on a future Docker or containerd change that nobody upstream will fix.
  The accepted response is to mark the lane degraded and skip proxy-level tests on it,
  not to block the pipeline; the harness reserves a field for this and does not yet implement it.
- Frozen lanes run only on pushes to `main`; pull requests run the `current` lane.
