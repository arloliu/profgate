# 300 - Testing

Apply when writing or running tests.
The layers and the cases each one must cover are defined in the *Testing* section of
[`docs/specs/gateway.md`](../../docs/specs/gateway.md); this file holds the mechanics.

## Two layers

- **Unit**, `mise run test`: `go test -race ./...`, runs in seconds and needs no cluster.
- **End-to-end**, `mise run test:e2e`: plain `go test` under the `e2e` build tag in `test/e2e/`,
  against a kind cluster from the lane named by `PROFGATE_E2E_LANE` (default `current`).
  No end-to-end framework.

## Mechanics

- `-race` is always on; a test that only passes without it is a bug.
- Table tests with named subtests (`t.Run(tc.name, ...)`) so a failure names its case.
- HTTP behavior is tested against `httptest.Server` stand-ins, never a live port.
- Kubernetes behavior is tested against `k8s.io/client-go/kubernetes/fake` with real informers.
- Every subtest builds a fresh fixture;
  subtests never share a fake clientset, informer, or server.
- A test encodes why the behavior matters ([000](000-agent-contract.md#test-intent)):
  a test that cannot fail when the logic changes is wrong.
