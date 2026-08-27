# 200 - Coding Standards

Apply when writing or editing Go code.
The linter configuration in [`.golangci.yml`](../../.golangci.yml) is the mechanical half of this file;
`mise run lint` runs it.

## Formatting

`gofmt` and `goimports` output is the only accepted formatting.
The formatters run under `mise run lint`, so a file that fails them fails the build.

## Logging

Log through `log/slog` only.
Pass a `*slog.Logger` into the constructor that needs it;
nothing calls `fmt.Print*` or `log.Print*` for operational output.

## Errors

Wrap with `fmt.Errorf("...: %w", err)` so the cause survives,
and match with `errors.Is` or `errors.As` rather than string comparison.
Sentinel errors live in the package that produces them and are exported when a caller must branch on them.

## Construction and state

- No `init()` functions.
  Everything is constructed explicitly and injected where it is used.
- `context.Context` is the first parameter of any function that does I/O or can block.
- The only package-level mutable state is the `atomic.Pointer[config.Config]` that holds the loaded configuration,
  plus the linker-set `version` string in `cmd/profgate`.
  Unexported arrays of constants behind an accessor function are immutable and allowed.
  So is an unexported compiled regular expression (`regexp.MustCompile`),
  the shape `internal/httpapi/pgo_policy.go` already holds.
  The test-only exceptions are the e2e package's `harness` variable, filled by `TestMain`,
  and a `sync.Once` memo of a fixture that is slow to generate, such as a key pair or a bcrypt hash.

## The Kubernetes seam

`internal/k8s` is the only non-test package outside `test/` that imports `k8s.io/client-go`;
`mise run check` enforces it ([800](800-security-invariant.md)).
