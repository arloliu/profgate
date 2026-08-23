# Gateway Implementation Plan

**Status:** Approved

> **For agentic workers:** implement this plan one task at a time, in order; each task is written test-first and ends with its own validation block and commit.
> Run a task inline or hand it to a subagent, whichever fits its size.
> Checkboxes (`- [ ]`) track progress.

**Goal:** Build the pprof gateway defined in [`docs/specs/gateway.md`](../specs/gateway.md):
cluster-wide informer discovery, static realms, a confirmed proxy to Pod pprof ports, an ops listener,
and the unit and end-to-end layers that prove it.

**Architecture:** One binary, two listeners.
`internal/k8s` is the only non-test importer of client-go and exposes `Discovery`;
`internal/httpapi` runs the request algorithm against it and hands a confirmed `Target` to `internal/proxy`;
`internal/config` loads and normalizes configuration once;
`internal/metrics` records through a `Recorder`.
`deploy/` is a kustomize base;
`test/e2e/` is a plain `go test` harness over a kind lane matrix.

**Tech Stack:** Go 1.26 (module directive `go 1.26.0`), `k8s.io/{client-go,api,apimachinery} v0.36.4`,
`github.com/arloliu/fuda v1.6.0`, `github.com/go-playground/validator/v10 v10.30.1` (fuda's own validator, for YAML error paths),
`github.com/prometheus/client_golang v1.24.1`, `gopkg.in/yaml.v3 v3.0.1`,
`sigs.k8s.io/yaml v1.6.0` (tests), `github.com/google/pprof` (tests), kind 0.32.0 and 0.22.0, ko 0.19.1, kubectl 1.36.4.

**Spec:** [`docs/specs/gateway.md`](../specs/gateway.md).
Every behavior table below restates the spec for the task at hand;
where they differ the spec wins, and the plan is the bug.
Spec sections are cited by heading name.
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- `go.mod` declares `go 1.26.0`; `mise run check` enforces it.
- `go.mod` never contains `github.com/nats-io/nats.go`.
- `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` share one minor version.
- Only `internal/k8s` imports `k8s.io/client-go` outside `_test.go` files and the `test/` tree.
- The shipped ClusterRole is exactly seven tuples:
  `services` and `endpointslices` with `list`,`watch`;
  `pods` with `get`,`list`,`watch`.
- No response the gateway generates contains a Pod IP or port.
- Global state: only the `atomic.Pointer[config.Config]` and the linker-set `version` string;
  everything else is constructed and injected.
  Unexported arrays of constants behind an accessor function (profile names, scenario metadata) are immutable and allowed.
  Test-only exception: the e2e package's `harness` variable, filled by `TestMain`.
- Commit messages follow [`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md):
  Conventional Commits, header under 50 characters, no attribution trailers, no sequencing labels.
- Module files: the first task pins every dependency with `go get`.
  A task that imports a module for the first time repeats that module's exact `go get` line before its first compile
  (idempotent), then runs `go mod tidy` and stages `go.mod` and `go.sum` with its package.
  Tasks that import nothing new do not run tidy.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on what you wrote.

---

## File Structure

```text
go.mod, go.sum
.golangci.yml
.ko.yaml
mise.toml                          # tools + tasks: build, test, lint, test:e2e, check
scripts/check-repo.py              # + client-go importers, k8s minor alignment, nats absence
cmd/profgate/main.go               # subcommand dispatch
cmd/profgate/serve.go              # wiring and lifecycle
cmd/profgate/testdata/{good,bad}.yaml
internal/config/config.go          # Config struct, Load, unknown-key walk, normalize, validate
internal/config/config_test.go, testdata/*.yaml
internal/k8s/discovery.go          # Target, Discovery, sentinel errors
internal/k8s/client.go             # rest config, QPS, namespace file, Preflight
internal/k8s/cluster.go            # Cluster: informers, Run, HasSynced
internal/k8s/eligibility.go        # Targets
internal/k8s/confirm.go            # Confirm
internal/k8s/export_test.go        # test seams: startFixture, waitCache
internal/k8s/*_test.go
internal/metrics/recorder.go       # Recorder, Noop
internal/metrics/prometheus.go
internal/proxy/proxy.go            # Proxy: one client, header allowlist, outcome classification
internal/proxy/proxy_test.go
internal/httpapi/server.go         # New, router, request algorithm
internal/httpapi/realm.go
internal/httpapi/errors.go
internal/httpapi/targets.go
internal/httpapi/profile.go
internal/httpapi/audit.go
internal/httpapi/*_test.go
internal/ops/ops.go                # /healthz /readyz /metrics
deploy/base/*.yaml, kustomization.yaml
deploy/deploy_test.go
test/e2e/versions.yaml
test/e2e/lanes.go, lanes_test.go   # no build tag: parse + validate lanes and scenario registry
test/e2e/registry.go               # scenario registry with capability flags
test/e2e/harness_test.go           # TestMain and helpers (e2e tag)
test/e2e/scenarios_test.go         # (e2e tag)
test/e2e/testapp/main.go
test/e2e/overlays/{default,reduced-no-watch,reduced-no-get,api-outage}/
.github/workflows/{check.yml,e2e.yml}
.agents/rules/{200-coding-standards,300-testing,500-validation-and-workflow}.md
```

---

## Module, toolchain, and repository checks

**Files:**
- Create: `go.mod`, `go.sum`, `.golangci.yml`, `.ko.yaml`
- Modify: `mise.toml`, `scripts/check-repo.py`, `AGENTS.md`, `.agents/rules/AGENTS.md`, `.agents/rules/100-project-map.md`
- Create: `.agents/rules/200-coding-standards.md`, `.agents/rules/300-testing.md`, `.agents/rules/500-validation-and-workflow.md`

**Produces:** `mise run build|test|lint|test:e2e|check`; a `go.mod` with every direct dependency pinned.

- [ ] **Initialize the module and pin the directive**

```bash
mise exec -- go mod init github.com/arloliu/profgate
sed -i 's/^go .*/go 1.26.0/' go.mod
```

- [ ] **Add the dependencies at exact versions**

```bash
mise exec -- go get \
  k8s.io/client-go@v0.36.4 k8s.io/api@v0.36.4 k8s.io/apimachinery@v0.36.4 \
  github.com/arloliu/fuda@v1.6.0 github.com/go-playground/validator/v10@v10.30.1 \
  github.com/prometheus/client_golang@v1.24.1 \
  gopkg.in/yaml.v3@v3.0.1 \
  sigs.k8s.io/yaml@v1.6.0 \
  github.com/google/pprof@v0.0.0-20260802141513-ef3492d7dac3
test "$(grep -cE '^\s+k8s.io/(client-go|api|apimachinery) v0\.36\.4' go.mod)" -eq 3
test "$(grep -c nats go.mod)" -eq 0
test "$(sed -n 's/^go //p' go.mod)" = "1.26.0"
```

Do not run `go mod tidy` in this task: nothing imports these modules yet and tidy would drop them.
Each later task that first imports a module repeats that module's `go get` line, then runs `go mod tidy` and stages `go.mod` and `go.sum`;
tidy keeps what is imported, so the task that first uses a module is the one that re-adds its pin.

- [ ] **Replace the `[tools]` table in `mise.toml` and add tasks**

Edit the existing `[tools]` table in place (do not append a second one):

```toml
[tools]
go = "1.26.7"
golangci-lint = "2.12.2"
kind = ["0.32.0", "0.22.0"]
ko = "0.19.1"
kubectl = "1.36.4"
```

Append after the existing `[tasks.check]`:

```toml
[tasks.build]
run = "go build ./..."

[tasks.test]
run = "go test -race ./..."

[tasks.lint]
run = "golangci-lint run ./..."

[tasks."test:e2e"]
description = "End-to-end suite on the lane named by PROFGATE_E2E_LANE (default current)"
run = "go test -tags e2e -count=1 -timeout 40m ./test/e2e/..."
```

Run `mise install` and `mise x kind@0.22.0 -- kind version`; expected output starts with `kind v0.22.0`.

- [ ] **Write `.golangci.yml`** (golangci-lint v2 schema)

```yaml
version: "2"
linters:
  default: standard
  enable: [bodyclose, errorlint, exhaustive, gosec, misspell, noctx, revive]
  settings:
    govet:
      enable: [stdversion]
formatters:
  enable: [gofmt, goimports]
```

- [ ] **Write `.ko.yaml`**

```yaml
defaultBaseImage: cgr.dev/chainguard/static:latest
builds:
  - id: profgate
    main: ./cmd/profgate
    ldflags:
      - -s -w -X main.version={{.Env.VERSION}}
  - id: testapp
    main: ./test/e2e/testapp
```

`VERSION` is exported by CI from the git tag and by the harness as `e2e`.

- [ ] **Extend `scripts/check-repo.py`**

Add three functions and register them where the existing checks are collected:

```python
def check_clientgo_importers(root):
    bad = []
    for path in root.rglob("*.go"):
        rel = path.relative_to(root).as_posix()
        if rel.endswith("_test.go") or rel.startswith("test/") or rel.startswith("internal/k8s/"):
            continue
        if '"k8s.io/client-go' in path.read_text():
            bad.append(f"{rel}: imports k8s.io/client-go outside internal/k8s")
    return bad

def check_k8s_minor_alignment(root):
    gomod = (root / "go.mod").read_text() if (root / "go.mod").exists() else ""
    minors = {}
    for mod in ("k8s.io/client-go", "k8s.io/api", "k8s.io/apimachinery"):
        m = re.search(rf"^\s*{re.escape(mod)} v0\.(\d+)\.", gomod, re.M)
        if m:
            minors[mod] = m.group(1)
    if len(set(minors.values())) > 1:
        return [f"go.mod: Kubernetes modules on different minors: {minors}"]
    return []

def check_no_nats(root):
    gomod = (root / "go.mod").read_text() if (root / "go.mod").exists() else ""
    if "github.com/nats-io/nats.go" in gomod:
        return ["go.mod: github.com/nats-io/nats.go is not allowed in the gateway"]
    return []
```

Run `mise run check`; expected: `repository invariants hold`.

- [ ] **Write the three rule files and retire the "no code" text**

Load the `writing-for-agents` skill first.
`200-coding-standards.md`: gofmt and goimports; `log/slog` only; wrap errors with `%w` and match with `errors.Is`;
no `init()`; `context.Context` is the first parameter;
the only package-level mutable state is the config pointer, plus the linker-set `version` string in `cmd/profgate`.
`300-testing.md`: the two layers from the spec's *Testing* section; `-race` always;
table tests with named subtests; `httptest` for HTTP; `client-go/kubernetes/fake` for Kubernetes;
a fresh fixture per subtest.
`500-validation-and-workflow.md`: the validation block from *Global Constraints* before every commit;
`semlf check` on prose; `mise run test:e2e` on `current` before any PR touching `internal/k8s`, `internal/proxy`, or `deploy/`.
Add the three files to the trigger index in `.agents/rules/AGENTS.md`.
Delete the "This Repository Has No Code Yet" section from `AGENTS.md` and the "no Go code yet" sentence from `100-project-map.md`.

- [ ] **Validate and commit**

```bash
mise run build && mise run lint && mise run test && mise run check
semlf check .agents/rules/200-coding-standards.md .agents/rules/300-testing.md .agents/rules/500-validation-and-workflow.md
git add go.mod go.sum .golangci.yml .ko.yaml mise.toml scripts/check-repo.py AGENTS.md .agents/rules/
git commit -m "build: add Go module, toolchain, and checks"
```

---

## Configuration

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`,
  `internal/config/testdata/{good,unknown-top,unknown-nested,unknown-realm,neither-port,name-only,both-ports,bad-name,bad-port,no-realms,bad-anon,bad-entry,bad-profile,same-listen}.yaml`

**Produces:**

```go
package config

type Config struct {
    Server    ServerConfig     `yaml:"server"`
    Discovery DiscoveryConfig  `yaml:"discovery"`
    Limits    LimitsConfig     `yaml:"limits"`
    Auth      AuthConfig       `yaml:"auth"`
    Realms    map[string]Realm `yaml:"realms" validate:"required,min=1,dive"`
}
type ServerConfig struct {
    Listen    string `yaml:"listen"    env:"LISTEN"     default:":8080" validate:"required,hostname_port"`
    OpsListen string `yaml:"opsListen" env:"OPS_LISTEN" default:":9090" validate:"required,hostname_port"`
}
type DiscoveryConfig struct {
    VersionLabel string      `yaml:"versionLabel" env:"VERSION_LABEL" default:"app.kubernetes.io/version" validate:"required"`
    Pprof        PprofConfig `yaml:"pprof"`
}
type PprofConfig struct {
    Port     int32  `yaml:"port"     env:"PPROF_PORT"      validate:"min=0,max=65535"` // 0 = unset; normalized
    PortName string `yaml:"portName" env:"PPROF_PORT_NAME"`
}
type LimitsConfig struct {
    CPUSeconds            int `yaml:"cpuSeconds"            env:"LIMIT_CPU_SECONDS"             default:"60" validate:"min=1,max=86400"`
    TraceSeconds          int `yaml:"traceSeconds"          env:"LIMIT_TRACE_SECONDS"           default:"60" validate:"min=1,max=86400"`
    MaxConcurrentProfiles int `yaml:"maxConcurrentProfiles" env:"LIMIT_MAX_CONCURRENT_PROFILES" default:"16" validate:"min=1,max=1024"`
}
type AuthConfig struct {
    Mode           string `yaml:"mode"           env:"AUTH_MODE"       default:"disabled" validate:"oneof=disabled"`
    AnonymousRealm string `yaml:"anonymousRealm" env:"ANONYMOUS_REALM" validate:"required"`
}
type Realm struct {
    Namespaces []string `yaml:"namespaces" validate:"required,min=1"`
    Services   []string `yaml:"services"   validate:"required,min=1"`
    Profiles   []string `yaml:"profiles"   validate:"required,min=1"`
}

// IsProfile reports whether name is one of the eight profile names; httpapi uses it.
// The names are an unexported array, not mutable package state.
func IsProfile(name string) bool
// Profiles returns a copy of the eight names in the spec's order.
func Profiles() []string

// Load reads path, rejects unknown keys at any nesting level (the error names the field and the
// struct type, as yaml.v3 reports them), applies defaults and PROFGATE_-prefixed environment
// overrides through fuda, normalizes, and validates.
func Load(path string) (*Config, error)

// RequiredGracePeriod is max(CPUSeconds, TraceSeconds) + 60 seconds.
func (c *Config) RequiredGracePeriod() time.Duration
```

fuda tag names used: `default`, `env` (with `WithEnvPrefix("PROFGATE_")` so `env:"LISTEN"` reads `PROFGATE_LISTEN`), `validate`.
These are the names in `fuda/docs/tag-spec.md`.

- [ ] **Write the failing tests**

One subtest per row; each loads its own testdata file (fresh `t.Setenv` where env is involved)
and asserts the exact field value or that `err.Error()` contains the quoted text.

| Subtest | Fixture or env | Expect |
|---|---|---|
| good | `good.yaml` | no error; every field equals the spec's default or the file value |
| unknown top-level | `unknown-top.yaml` (`extra: 1`) | error contains `field extra not found in type config.Config` |
| unknown nested | `unknown-nested.yaml` (`limits: {foo: 1}`) | error contains `field foo not found in type config.LimitsConfig` |
| unknown in realm | `unknown-realm.yaml` (`realms: {developer: {profilse: ["*"]}}`) | error contains `field profilse not found in type config.Realm` |
| limit zero | env `PROFGATE_LIMIT_CPU_SECONDS=0` | error contains `limits.cpuSeconds` |
| limit too large | env `PROFGATE_LIMIT_TRACE_SECONDS=86401` | error contains `limits.traceSeconds` |
| concurrency out of range | env `PROFGATE_LIMIT_MAX_CONCURRENT_PROFILES=1025` | error contains `limits.maxConcurrentProfiles` |
| neither port | `neither-port.yaml` | `Port == 6060`, `PortName == ""` |
| name only | `name-only.yaml` (`portName: pprof`) | `Port == 0`, `PortName == "pprof"` |
| both | `both-ports.yaml` | error contains `exactly one of discovery.pprof.port and discovery.pprof.portName` |
| bad port name | `bad-name.yaml` (`portName: "-bad"`) | error contains `discovery.pprof.portName` |
| port out of range | `bad-port.yaml` (`port: 70000`) | error contains `discovery.pprof.port` |
| bad version label | env `PROFGATE_VERSION_LABEL=bad label` | error contains `discovery.versionLabel` |
| bad listen | env `PROFGATE_LISTEN=nope` | error contains `server.listen` |
| same listen | `same-listen.yaml` | error contains `server.opsListen must differ` |
| no realms | `no-realms.yaml` | error contains `realms` |
| anonymous realm unknown | `bad-anon.yaml` | error contains `auth.anonymousRealm "nope" is not a realm` |
| realm entry invalid | `bad-entry.yaml` (`namespaces: ["Bad_Name"]`) | error contains `realms.developer.namespaces` |
| profile unknown | `bad-profile.yaml` | error contains `realms.developer.profiles` |
| env overrides | every `PROFGATE_*` variable from the spec's configuration table set | each lands on its field |
| grace period | cpu 60, trace 90 | `RequiredGracePeriod() == 150*time.Second` |

- [ ] **Add the modules this package imports, then run the tests and watch them fail to compile**

```bash
mise exec -- go get github.com/arloliu/fuda@v1.6.0 github.com/go-playground/validator/v10@v10.30.1 gopkg.in/yaml.v3@v3.0.1 k8s.io/apimachinery@v0.36.4
mise exec -- go test ./internal/config/
```

- [ ] **Implement `Load`**

1. `b, err := os.ReadFile(path)`.
2. Unknown keys: `dec := yaml.NewDecoder(bytes.NewReader(b)); dec.KnownFields(true); var probe Config; err := dec.Decode(&probe)`;
   `KnownFields` applies at every nesting level, including inside each `Realm` value of the `Realms` map,
   and yaml.v3 reports `field <name> not found in type <pkg.Type>`;
   return that error wrapped as `fmt.Errorf("config: %w", err)` and discard `probe`.
3. Build a validator whose error paths use YAML names:
   `v := validator.New(); v.RegisterTagNameFunc(func(f reflect.StructField) string { return strings.Split(f.Tag.Get("yaml"), ",")[0] })`;
   then `loader, err := fuda.New().FromBytes(b).WithEnvPrefix("PROFGATE_").WithValidator(v).Build()`; `loader.Load(&cfg)`;
   fuda runs `default`, `env`, and `validate` tags;
   on a `validator.ValidationErrors` (reach it with `errors.As` through fuda's wrapper) build the message from each
   `FieldError.Namespace()` with the leading struct name stripped and lowercased first segment, for example `limits.cpuSeconds`,
   and wrap as `fmt.Errorf("config: %s: %w", path, err)`.
   The `validator` import is `github.com/go-playground/validator/v10`, which fuda already requires; add it to the dependency table.
4. `normalize(&cfg)`: if `cfg.Discovery.Pprof.Port == 0 && cfg.Discovery.Pprof.PortName == ""` then `Port = 6060`.
5. `validate(&cfg)` hand checks, each returning an error that names the key:
   exactly one of `Port != 0` / `PortName != ""`;
   `PortName == "" || len(validation.IsValidPortName(PortName)) == 0` (`k8s.io/apimachinery/pkg/util/validation`);
   `len(validation.IsQualifiedName(VersionLabel)) == 0`;
   `Listen != OpsListen`;
   `AnonymousRealm` is a key of `Realms`;
   every realm list entry is `"*"` or passes `validation.IsDNS1123Label` (namespaces, services)
   or satisfies `IsProfile` (profiles).

- [ ] **Run the tests until they pass, then validate and commit**

```bash
mise exec -- go test -race ./internal/config/ && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add internal/config/ go.mod go.sum
git commit -m "feat(config): load and validate configuration"
```

---

## CLI entry points

**Files:**
- Create: `cmd/profgate/main.go`, `cmd/profgate/main_test.go`, `cmd/profgate/testdata/good.yaml`, `cmd/profgate/testdata/bad.yaml`

**Produces:** `func run(args []string, stdout, stderr io.Writer) int`;
subcommands `version`, `config validate --config <path>`, `serve --config <path>`;
`var version = "dev"` (ko's ldflags set it) is the one package-level variable besides the config pointer;
it is written only by the linker and is named as that exception in `200-coding-standards.md`.
`serve` is wired in *Serve lifecycle and ops listener*; until then it loads the config and returns 0.

- [ ] **Write the fixtures**

`testdata/good.yaml` is the spec's example configuration verbatim.
`testdata/bad.yaml` is the same with `realms: {}`.

- [ ] **Write the failing tests**

| Subtest | Args | Expect |
|---|---|---|
| version | `version` | exit 0, stdout `profgate dev\n` |
| validate good | `config validate --config testdata/good.yaml` | exit 0, stdout contains `required terminationGracePeriodSeconds: 120` |
| validate bad | `config validate --config testdata/bad.yaml` | exit 2, stderr contains `realms` |
| unknown | `bogus` | exit 2, stderr contains `usage` |
| no subcommand | (none) | exit 2 |

- [ ] **Implement** with one `flag.NewFlagSet` per subcommand and `main()` as `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./cmd/profgate/
mise run lint && mise run test && mise run check
git add cmd/profgate/
git commit -m "feat(cli): add version and config validate"
```

---

## Discovery types, client, and preflight

**Files:**
- Create: `internal/k8s/discovery.go`, `internal/k8s/client.go`, `internal/k8s/client_test.go`

**Produces:**

```go
package k8s

type Target struct {
    Namespace, Service, Pod, Node, PodIP string
    Port    int32
    Version string
    UID     string
}

var (
    ErrServiceNotFound      = errors.New("service not found")
    ErrServiceSelectorless  = errors.New("service has no selector")
    ErrTargetChanged        = errors.New("target changed")
    ErrDiscoveryUnavailable = errors.New("discovery unavailable")
)

type Discovery interface {
    Targets(ctx context.Context, namespace, service string) ([]Target, error)
    HasSynced() bool
    Confirm(ctx context.Context, t Target) error
}

type Options struct {
    VersionLabel         string
    Port                 int32
    PortName             string
    NamespaceFile        string        // default "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
    PreflightCallTimeout time.Duration // default 10s when zero
    Logger               *slog.Logger
}

// NewClientset uses in-cluster config, or KUBECONFIG when set, with QPS 20 and Burst 50.
func NewClientset() (kubernetes.Interface, error)

// OwnNamespace reads Options.NamespaceFile and returns its trimmed content.
func OwnNamespace(opts Options) (string, error)

type ErrForbidden struct{ Resource, Verb string }
func (e ErrForbidden) Error() string { return fmt.Sprintf("forbidden: %s %s", e.Verb, e.Resource) }

// Preflight performs the seven calls from the spec's "Startup preflight" section.
// A 403 on any of them returns ErrForbidden; any other error is returned wrapped.
func Preflight(ctx context.Context, cs kubernetes.Interface, opts Options, ownNamespace string) error
```

`Options.PreflightCallTimeout` defaults to 10s when zero; tests pass a short value.

- [ ] **Write the failing tests**

Every subtest builds its own `fake.NewClientset()` (the client-go 0.36 constructor) and reactors.
Ordinary verbs use `PrependReactor(verb, resource, fn)`; watches use `PrependWatchReactor(resource, fn)` —
the two chains are separate.

| Subtest | Reactor | Expect |
|---|---|---|
| all allowed | none (fake returns empty lists, accepted watch, 404 get) | `nil` |
| watch pods forbidden | `PrependWatchReactor("pods", …)` returning `apierrors.NewForbidden(schema.GroupResource{Resource:"pods"}, "", nil)` | `ErrForbidden{"pods","watch"}` |
| get pods forbidden | `PrependReactor("get","pods",…)` forbidden | `ErrForbidden{"pods","get"}` |
| list endpointslices forbidden | `PrependReactor("list","endpointslices",…)` forbidden | `ErrForbidden{"endpointslices","list"}` |
| get not found | `PrependReactor("get","pods",…)` returning `NewNotFound` | `nil` |
| generic list error | `PrependReactor("list","services",…)` returning `errors.New("boom")` | error wraps `boom`; `!errors.As(err, &ErrForbidden{})` |
| blocking call | a list reactor that blocks until the test's own timer; `Options{PreflightCallTimeout: 50*time.Millisecond}` | returns an error wrapping `context.DeadlineExceeded` within 1s |
| exact tuples | allow all; after `Preflight`, inspect `cs.Actions()` | the multiset of `(verb, resource)` is exactly `{list,watch}×{services,pods,endpointslices} ∪ {(get,pods)}`; the `get` names `ownNamespace/profgate-preflight` |

`OwnNamespace`: a temp file containing `"payment\n"` → `"payment"`; a missing file → error.

- [ ] **Add the Kubernetes modules, then run the tests and watch them fail to compile**

```bash
mise exec -- go get k8s.io/client-go@v0.36.4 k8s.io/api@v0.36.4 k8s.io/apimachinery@v0.36.4
mise exec -- go test ./internal/k8s/
```

- [ ] **Implement**

`Preflight`, for each of `Services("")`, `Pods("")`, `EndpointSlices("")` (the `discovery/v1` client):
`List(callCtx, metav1.ListOptions{Limit: 1})`;
then `one := int64(1); w, err := …Watch(callCtx, metav1.ListOptions{TimeoutSeconds: &one})`; `w.Stop()`.
Then `Pods(ownNamespace).Get(callCtx, "profgate-preflight", metav1.GetOptions{})` ignoring `apierrors.IsNotFound`.
`callCtx` is `context.WithTimeout(ctx, opts.PreflightCallTimeout)` per call.
`apierrors.IsForbidden(err)` → `ErrForbidden{resource, verb}`; otherwise `fmt.Errorf("preflight %s %s: %w", verb, resource, err)`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/k8s/ && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add internal/k8s/ go.mod go.sum
git commit -m "feat(k8s): add discovery types and preflight"
```

---

## Informers and target resolution

**Files:**
- Create: `internal/k8s/cluster.go`, `internal/k8s/eligibility.go`, `internal/k8s/export_test.go`, `internal/k8s/eligibility_test.go`

**Produces:**

```go
// Cluster implements Discovery over cluster-wide shared informers.
type Cluster struct{ /* unexported */ }

func New(cs kubernetes.Interface, opts Options) *Cluster
// Run starts the informers and blocks until ctx is done.
func (c *Cluster) Run(ctx context.Context)
func (c *Cluster) HasSynced() bool
func (c *Cluster) Targets(ctx context.Context, namespace, service string) ([]Target, error)
```

`export_test.go` (package `k8s`, `_test.go` so it ships with tests only):

```go
// startFixture creates a fake clientset pre-loaded with objs, constructs a Cluster, runs it,
// and waits for HasSynced. cancel stops informer delivery; the clientset stays usable for live calls.
func startFixture(t *testing.T, opts Options, objs ...runtime.Object) (cs *fake.Clientset, c *Cluster, cancel context.CancelFunc)
// waitCache polls until pred is true or 5s pass, then fails the test.
func waitCache(t *testing.T, pred func() bool)
```

- [ ] **Write the baseline fixture objects** in `eligibility_test.go`

Service `payment/payment-api` with `Spec.Selector{"app":"payment"}`;
Pod `payment-api-1` (UID `u1`, labels `app=payment`, `app.kubernetes.io/version=1.2.3`, `Spec.NodeName=worker-1`,
`Status.Phase=Running`, `Status.Conditions[{Type:Ready,Status:True}]`, `Status.PodIPs=[{IP:"10.0.0.5"}]`,
one container with `Ports=[{Name:"pprof",ContainerPort:6060,Protocol:TCP}]`);
EndpointSlice `payment-api-abc` (`AddressType: IPv4`, label `kubernetes.io/service-name=payment-api`,
one endpoint `{Addresses:["10.0.0.5"], Conditions:{Ready: &ready} with `ready := true`, TargetRef:{Kind:"Pod", Namespace:"payment", Name:"payment-api-1", UID:"u1"}}`).
Default `Options{VersionLabel:"app.kubernetes.io/version", PortName:"pprof"}`.

- [ ] **Write the failing tests**

Each subtest calls `startFixture` with the baseline objects **after applying its mutation to the object literals**
(no mutation of a running fixture), so the cache is deterministic once `HasSynced` is true.
Baseline expects exactly
`[]Target{{Namespace:"payment", Service:"payment-api", Pod:"payment-api-1", Node:"worker-1", PodIP:"10.0.0.5", Port:6060, Version:"1.2.3", UID:"u1"}}`.
Rows expect zero targets unless stated.

| Subtest | Mutation | Expect |
|---|---|---|
| service missing | omit the Service | `ErrServiceNotFound` |
| selectorless | `Spec.Selector = nil` | `ErrServiceSelectorless` |
| selector mismatch | Pod label `app=other` | none |
| stale uid | `TargetRef.UID = "u0"` | none |
| wrong namespace in targetRef | `TargetRef.Namespace = "other"` | none |
| targetRef not Pod | `TargetRef.Kind = "Node"` | none |
| ready false | `Conditions.Ready` points at `false` | none |
| ready nil | `Conditions.Ready = nil` | one target |
| pod pending | `Phase = Pending` | none |
| pod not ready | Ready condition `False` | none |
| terminating | `DeletionTimestamp` set | none |
| address not in podIPs | endpoint address `10.0.0.9` | none |
| named port missing | Pod has no port named `pprof` | none |
| named port UDP | port `pprof` protocol `UDP` | none |
| named port protocol unset | port `pprof` with `Protocol == ""` | one target, `Port == 6060` |
| numeric port mode | `Options{Port:7070}`, Pod ports empty | one target, `Port == 7070` |
| version absent | remove the version label | one target, `Version == ""` |
| two slices same pod | add a second slice with the same endpoint | one target |
| valid plus invalid duplicate | second slice with address `10.0.0.9` | one target at `10.0.0.5` |
| two valid conflicting | `PodIPs=[10.0.0.5, 10.0.0.6]`, slices list each | none; a log record containing `conflict` (capture with a `slog.Handler` test sink) |
| dual stack | add an `IPv6` slice with address `fd00::5` and `PodIPs` including it | one target, `PodIP == "10.0.0.5"` |
| ipv6 only | only an `IPv6` slice, `PodIPs=[fd00::5]` | one target, `PodIP == "fd00::5"` |
| not synced | construct with `New` but do not `Run` | `HasSynced() == false` |
| cache follows deletes | baseline; then `cs.CoreV1().Pods("payment").Delete(...)`; `waitCache` until `Targets` is empty | passes within 5s |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

`cluster.go`: `informers.NewSharedInformerFactory(cs, 10*time.Minute)`;
listers for `Core().V1().Services()`, `Core().V1().Pods()`, `Discovery().V1().EndpointSlices()`;
`Run` calls `factory.Start(ctx.Done())`, `factory.WaitForCacheSync(ctx.Done())`, sets a synced flag, then `<-ctx.Done()`.
`eligibility.go`: the spec's *Eligibility* rules in order:
Service lookup and selector check;
`slices, _ := sliceLister.EndpointSlices(ns).List(labels.SelectorFromSet(labels.Set{"kubernetes.io/service-name": service}))`;
choose the address family (`IPv4` slices if any exist, else `IPv6`);
for each endpoint validate, against the cached Pod (`podLister.Pods(ns).Get(targetRef.Name)`),
the targetRef kind and namespace, the UID match, selector match, endpoint readiness, Pod phase and readiness, absence of a deletion timestamp,
`labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels))`,
readiness, phase, deletion timestamp, `addresses[0] ∈ pod.Status.PodIPs`,
port resolution (named mode: a container port with that name and `Protocol == "" || Protocol == corev1.ProtocolTCP`);
collect valid entries keyed by UID; a UID with two different addresses is dropped and logged with `"conflict"`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/k8s/
mise run lint && mise run test && mise run check
git add internal/k8s/
git commit -m "feat(k8s): resolve Service backends from caches"
```

---

## Confirmation before connecting

**Files:**
- Create: `internal/k8s/confirm.go`, `internal/k8s/confirm_test.go`

**Produces:** `func (c *Cluster) Confirm(ctx context.Context, t Target) error` per the spec's *Confirmation before connecting*,
and the runtime that later tasks construct the package through without naming a client-go type:

```go
// Runtime owns the clientset, preflight, and the Cluster. NewRuntime builds the in-cluster client;
// NewRuntimeWithClientset is exported for tests in other packages, which may name kubernetes.Interface
// only from _test.go files.
type Runtime interface {
    OwnNamespace() (string, error)
    Preflight(ctx context.Context) error   // Preflight with the runtime's own namespace and options
    Cluster() *Cluster                     // constructed, not yet running
}
func NewRuntime(opts Options) (Runtime, error)
func NewRuntimeWithClientset(cs kubernetes.Interface, opts Options) Runtime
```

A `runtime_test.go` row: `NewRuntimeWithClientset(fakeCS, opts).Cluster()` is a `*Cluster` whose `HasSynced()` is false,
and `Preflight` on it returns the same `ErrForbidden` the direct call returns under a forbidden watch reactor.

- [ ] **Write the failing tests**

Each subtest: `cs, c, cancel := startFixture(...)` with the baseline, take `t0 := Targets(...)[0]`,
then `cancel()` to stop informer delivery, then mutate **the fake API** through `cs` (the cache no longer follows),
then call `c.Confirm(ctx, t0)`.

| Subtest | Live mutation | Expect |
|---|---|---|
| unchanged | none | `nil` |
| deleted | `Pods.Delete` | `ErrTargetChanged` |
| recreated same name and IP | delete, then create with UID `u2` | `ErrTargetChanged` |
| terminating | set `DeletionTimestamp` via `Update` | `ErrTargetChanged` |
| not ready | Ready `False` via `UpdateStatus` | `ErrTargetChanged` |
| not running | `Phase=Succeeded` | `ErrTargetChanged` |
| ip moved | `PodIPs=[10.0.0.7]` | `ErrTargetChanged` |
| api error | `PrependReactor("get","pods",…)` returning `errors.New("boom")` | `errors.Is(err, ErrDiscoveryUnavailable)` |
| api timeout | blocking reactor, `ctx` with 50ms deadline | `ErrDiscoveryUnavailable` |
| one call only | recording | exactly one action, `get pods payment/payment-api-1` |
| cache unused | delete the Pod live without `cancel()` and call `Confirm` before the cache observes it | `ErrTargetChanged` (the live read, not the cache, decides) |
| uid from target | after recreation, let the cache observe the new Pod (no `cancel()`), then `Confirm(t0)` | `ErrTargetChanged` (captured `t0.UID` is `u1`) |
| tuples across the lifecycle | `startFixture`, one `Targets`, one `Confirm`; inspect `cs.Actions()` | every `(verb, resource)` is within the seven RBAC tuples and `Confirm` added exactly one `get pods` |
| relist continues under load | 64 goroutines loop `Confirm` for 2s through a reactor that sleeps 20ms; meanwhile `cs.Tracker().Add` a new eligible Pod and slice | the cache lists the new Pod within 5s while confirmations continue; the in-flight cap is asserted in `httpapi`, not here |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

`callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)`;
`pod, err := cs.CoreV1().Pods(t.Namespace).Get(callCtx, t.Pod, metav1.GetOptions{})`;
`IsNotFound` → `ErrTargetChanged`; other error → `fmt.Errorf("%w: %v", ErrDiscoveryUnavailable, err)`;
then `string(pod.UID) != t.UID`, `pod.DeletionTimestamp != nil`, `pod.Status.Phase != Running`,
Ready condition not `True`, or `t.PodIP ∉ pod.Status.PodIPs` → `ErrTargetChanged`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/k8s/
mise run lint && mise run test && mise run check
git add internal/k8s/
git commit -m "feat(k8s): confirm Pod before dialing"
```

---

## Metrics recorder

**Files:**
- Create: `internal/metrics/recorder.go`, `internal/metrics/prometheus.go`, `internal/metrics/prometheus_test.go`

**Produces:**

```go
package metrics

type Endpoint string
const (
    EndpointTargets Endpoint = "targets"
    EndpointProfile Endpoint = "profile"
)

type Recorder interface {
    // Request records one completed /v1 request. endpoint and profile come from the resolved route
    // when there is one (method failures included): targets → ("targets","none"), a known profile
    // route → ("profile", name). Requests that fail before a route resolves, or name an unknown
    // profile, record ("profile","none").
    Request(endpoint Endpoint, profile, code string, d time.Duration)
    Confirm(result string)                                           // "ok" | "changed" | "unavailable"
    ProfilesInFlight(delta int)
    DiscoverySynced(synced bool)
}

type Noop struct{}            // implements Recorder with empty methods
func NewPrometheus(reg prometheus.Registerer) *Prometheus
```

Metric names and labels exactly as the spec's *Metrics* table:
`profgate_requests_total{endpoint,profile,code}`, `profgate_request_duration_seconds{profile}`,
`profgate_confirm_total{result}`, `profgate_profiles_in_flight`, `profgate_discovery_synced`.

- [ ] **Add the module**: `mise exec -- go get github.com/prometheus/client_golang@v1.24.1`.
- [ ] **Write the failing tests**: register on `prometheus.NewPedanticRegistry()`, call each method once,
  and compare with `testutil.GatherAndCompare` against the expected exposition text for each metric.
- [ ] **Implement**, then validate and commit:

```bash
mise exec -- go test -race ./internal/metrics/ && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add internal/metrics/ go.mod go.sum
git commit -m "feat(metrics): add Recorder and Prometheus"
```

---

## Proxy

**Files:**
- Create: `internal/proxy/proxy.go`, `internal/proxy/proxy_test.go`

**Produces:**

```go
package proxy

type Request struct {
    Target        k8s.Target
    Path          string            // upstream path from the spec's profile table
    Seconds       int               // effective duration; 0 for profiles without one
    TargetHeaders map[string]string // X-Pprof-Target-*; written only on forwarded responses
}

type Outcome struct {
    Code      string // "ok", "upstream_<status>", "upstream_unreachable", "upstream_timeout",
                     // "upstream_redirect", "upstream_stream_failed", "client_gone"
    Status    int    // status written, or the status the caller should write when !Committed
    Committed bool   // response headers were sent to the client
}

type Options struct {
    // HeaderDeadline maps the effective duration to the header deadline; nil means the spec's
    // rule: seconds+10s, or 30s when seconds is 0. Tests inject short values.
    HeaderDeadline func(seconds int) time.Duration
}
type Proxy struct{ client *http.Client; headerDeadline func(int) time.Duration }

// New builds the one shared immutable transport: Proxy nil, DisableCompression true,
// 5s dial timeout, and a client whose CheckRedirect returns http.ErrUseLastResponse.
func New(opts Options) *Proxy

// Do issues the upstream request under ctx (the caller's overall budget) plus its own header
// deadline, streams forwarded responses to w, and never writes a JSON body itself:
// when Outcome.Committed is false the caller writes the error envelope for Outcome.Code.
func (p *Proxy) Do(ctx context.Context, w http.ResponseWriter, req Request) Outcome
```

- [ ] **Write the failing tests**

Each subtest starts its own `httptest.Server` (or a listener it closes for "refused") and calls `New(Options{}).Do` with
`Target{PodIP: host, Port: port}` parsed from the server URL, and a `ctx` with a stated deadline.
A trap server is an `httptest.Server` whose handler increments a counter; every row asserts the trap count.
Rows that shorten the header deadline construct `New(Options{HeaderDeadline: …})`; others use `New(Options{})`.

| Subtest | Upstream behavior | ctx deadline | Expect |
|---|---|---|---|
| ok | 200, body `abc`, `Content-Type: application/octet-stream` | 2s | 200, body `abc`, `Code=="ok"`, `Committed` |
| target headers on 2xx | 200 | 2s | `X-Pprof-Target-Pod` etc. present |
| gzip preserved | 200, `Content-Encoding: gzip`, gzip bytes | 2s | identical bytes, header present |
| disposition preserved | `Content-Disposition: attachment; filename="profile"` | 2s | header present |
| headers dropped | `Set-Cookie`, `Server`, `Connection: keep-alive`, `X-Pprof-Target-Pod: forged`, `Cache-Control: public` | 2s | none present; `X-Pprof-Target-Pod` equals the request's value |
| 404 passthrough | 404, body `nope` | 2s | 404, body `nope`, `upstream_404`, target headers present |
| 429 passthrough | 429 | 2s | `upstream_429` |
| 500 passthrough | 500 | 2s | `upstream_500` |
| absolute redirect | 302 `Location: http://<trap>/x` | 2s | `upstream_redirect`, `!Committed`, trap 0 |
| relative redirect | 302 `Location: /x` | 2s | `upstream_redirect`, trap 0 |
| env proxy ignored | `t.Setenv("HTTP_PROXY", trapURL)` | 2s | upstream reached, trap 0 |
| refused | closed port | 2s | `upstream_unreachable`, `!Committed` |
| reset before headers | handler hijacks and closes | 2s | `upstream_unreachable` |
| eof before headers | handler hijacks, writes `HTTP/1.1 200 OK\r\n` then closes | 2s | `upstream_unreachable` |
| header deadline | accepts, sleeps forever; `HeaderDeadline` returns 200ms | 60s | `upstream_timeout` within 1s |
| overall deadline | same upstream, default header deadline | 200ms | `upstream_timeout` |
| body outlives header deadline | headers at once, body streamed over 600ms; `HeaderDeadline` returns 200ms | 5s | `ok`, full body |
| headers at the deadline | `HeaderDeadline` returns 50ms; upstream writes headers after exactly 50ms then a 300ms body; 200 runs | every run is either `upstream_timeout` with `!Committed` or `ok` with the full body; never a truncated `ok` |
| stream failed | writes headers + half body, hijacks and closes | 2s | `Committed`, `upstream_stream_failed` |
| client gone | cancel ctx mid-body | — | `client_gone`; upstream handler's `r.Context().Done()` fires |
| independent header deadlines | two concurrent requests through one `Proxy` whose `HeaderDeadline` returns 100ms for `Seconds==1` and 2s for `Seconds==2`, against a 500ms-delay upstream | 5s | first `upstream_timeout`, second `ok` |
| body closed | every forwarded, redirected, and failed-stream row, using a transport wrapper that counts `Body.Close` | exactly one close per response |
| no leak | every non-2xx and error row | — | no header value or written byte contains the upstream host or port |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

URL: `"http://" + net.JoinHostPort(t.PodIP, strconv.Itoa(int(t.Port))) + req.Path` plus `?seconds=N` when `Seconds > 0`.
`p.headerDeadline(seconds)` = `seconds+10` seconds, or 30s when `seconds == 0`, unless injected;
the request context must not carry the header deadline, because Go applies a request's context to body reads as well:
`reqCtx, cancelReq := context.WithCancelCause(ctx)`;
run `p.client.Do(httpReq.WithContext(reqCtx))` in a goroutine that sends `(resp, err)` on a buffered channel `done`;
then `select { case r := <-done: …; case <-time.After(p.headerDeadline(req.Seconds)): cancelReq(errHeaderDeadline); r := <-done; … }`.
The cancellation happens only on the timeout branch of the `select`, so headers that arrive first can never be followed by a late cancel,
and the body is then read under `reqCtx`, which expires only with the overall budget `ctx`.
Classification of `err` before headers:
`errors.Is(context.Cause(reqCtx), errHeaderDeadline)` or `errors.Is(ctx.Err(), context.DeadlineExceeded)` → `upstream_timeout`;
`errors.Is(ctx.Err(), context.Canceled)` → `client_gone`; anything else → `upstream_unreachable`.
`resp.StatusCode` in 300–399 → drain and close the body, `upstream_redirect`.
Otherwise copy allowlisted headers (`Content-Type`, `Content-Length`, `Content-Encoding`, `Content-Disposition`, `X-Content-Type-Options`),
set `TargetHeaders`, `WriteHeader(resp.StatusCode)`, then `io.Copy(w, resp.Body)` under `reqCtx`
(wrap the body reader to observe `ctx.Done()`);
a copy error → `upstream_stream_failed` unless `errors.Is(ctx.Err(), context.Canceled)` → `client_gone`.
`Code` for 2xx is `ok`; for 4xx/5xx `fmt.Sprintf("upstream_%d", status)`.
`defer resp.Body.Close()` immediately after a non-nil response on every path, including redirect and copy failure.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/proxy/
mise run lint && mise run test && mise run check
git add internal/proxy/
git commit -m "feat(proxy): forward pprof via a pinned client"
```

---

## HTTP API

**Files:**
- Create: `internal/httpapi/server.go`, `realm.go`, `errors.go`, `targets.go`, `profile.go`, `audit.go`,
  `server_test.go`, `realm_test.go`, `errors_test.go`, `profile_test.go`, `targets_test.go`

**Produces:**

```go
package httpapi

// Upstream is what the profile handler needs from internal/proxy.
type Upstream interface {
    Do(ctx context.Context, w http.ResponseWriter, req proxy.Request) proxy.Outcome
}

type Deps struct {
    Discovery k8s.Discovery
    Upstream  Upstream
    Config    *atomic.Pointer[config.Config]
    Recorder  metrics.Recorder
    Logger    *slog.Logger
    Choose    func(n int) int // nil means math/rand/v2 IntN
}

func New(d Deps) http.Handler
```

- [ ] **Write the errors and realm tests**

`errors_test.go`: `writeError(w, 404, "service_not_found", "service x not found in namespace y")` yields
`Content-Type: application/json`, `Cache-Control: no-store`, no `X-Pprof-Target-*`,
body exactly `{"error":"service x not found in namespace y","code":"service_not_found"}` plus newline.
`realm_test.go`: a table over `(realm lists, namespace, service, profile, endpoint)` → allowed or denied,
covering `*`, exact match, mismatch on each of the three lists, and that `profiles` is ignored for the targets endpoint.

- [ ] **Write the handler tests**

A `fakeDiscovery` implementing `k8s.Discovery` with fields `targets []k8s.Target`, `err error`, `synced bool`,
`confirmErr error`, an optional `onTargets func()` hook, and call counters;
a `fakeUpstream` recording calls and returning a configured `Outcome`, or blocking on a channel for the admission row;
and a trap `httptest.Server` whose address is used as the target `PodIP`/`Port` for the no-dial rows.
Every request goes through `httptest.NewRecorder`.
Rows assert status, `code`, headers, `Targets` call count, `Upstream.Do` call count,
and that no response byte contains `10.0.0.5` or `6060`.

| Subtest | Request | Expect |
|---|---|---|
| route unknown | `GET /v1/bogus` | 404 `route_unknown` |
| trailing slash | `GET …/targets/` | 404 `route_unknown` |
| bad namespace segment | `GET /v1/namespaces/Bad_/services/x/targets` | 404 `route_unknown` |
| bad route beats method | `POST /v1/namespaces/Bad_/services/x/targets`, `HEAD /v1/bogus` | 404 `route_unknown` |
| method | `POST …/targets`, `HEAD …/profiles/heap` | 405 `method_not_allowed`, `Allow: GET` |
| not synced | `synced=false` | 503 `not_ready`, `Targets` 0 calls |
| realm denied, service exists | realm denies ns | 403 `realm_denied`, `Targets` 0 calls |
| realm denied, service missing | same realm, `err=ErrServiceNotFound` | 403, identical body to the row above |
| realm denies profile | profiles `[heap]`, request `cpu` | 403 |
| targets query | `…/targets?x=1` | 400 `invalid_parameter` |
| targets ok | two targets unsorted | 200, sorted by `pod`, keys `pod`,`node`,`version` only, `version` present when empty |
| targets empty | none | 200 `{"namespace":…,"service":…,"targets":[]}` |
| service not found | | 404 `service_not_found` |
| selectorless | | 422 `service_selectorless` |
| profile unknown | `…/profiles/bogus` | 404 `profile_unknown` |
| seconds on heap | `heap?seconds=1` | 400 `invalid_parameter` |
| seconds grammar | `cpu?seconds=abc`, `=0`, `=-1`, `=1&seconds=2`, `=`, `=99999999999` | 400 `invalid_parameter` each |
| seconds over limit | `cpu?seconds=61`, limit 60 | 400 `seconds_exceeds_limit` |
| default over limit | `cpu`, limit 10 | 400 `seconds_exceeds_limit` |
| effective sent | `cpu`, limit 60 | `Upstream.Do` request has `Seconds == 30`; `trace` → 1 |
| unknown param | `heap?foo=1` | 400 `invalid_parameter` |
| pod grammar | `?pod=`, `?pod=a&pod=b`, `?pod=Bad_` | 400 `invalid_parameter` |
| pod subdomain ok | `?pod=a.b-c` matching a target | 200 |
| version grammar | `?version=`, `?version=a&version=b` | 400 `invalid_parameter` |
| strategy grammar | `?strategy=roundrobin`, `?strategy=` | 400 `invalid_parameter` |
| strategy default | no `strategy`, three targets, `Deps.Choose` is a double recording its `n` and returning 1 | `Choose(3)` called once; target index 1 reaches `Do` |
| strategy explicit | `?strategy=random` | same double called; behaves as the row above |
| version filter | `?version=1.0` | only matching target reaches `Do` |
| pod wrong version | `?pod=a&version=2.0` with a at 1.0 | 404 `pod_not_found` |
| pod not target | `?pod=zzz` | 404 `pod_not_found` |
| no targets | none | 503 `no_targets` |
| admission | `MaxConcurrentProfiles=1`, first request blocked in `fakeUpstream` | second → 429 `too_many_profiles`; after release, `ProfilesInFlight` net 0 |
| confirm changed | `confirmErr=ErrTargetChanged` with the trap as target | 503 `target_changed`, `Do` 0 calls, trap 0, `Recorder.Confirm("changed")` |
| confirm unavailable | `confirmErr=ErrDiscoveryUnavailable` | 503 `discovery_unavailable`, `Do` 0 calls, trap 0, `Recorder.Confirm("unavailable")` |
| confirm ok | | `Do` 1 call, `Recorder.Confirm("ok")` |
| admission bounds API reads and relist continues | `Discovery` is a real `k8s.Cluster` from `k8s.NewRuntimeWithClientset(fakeCS, opts).Cluster()` run over the baseline objects; a `get pods` reactor sleeps 20ms and tracks peak in-flight; `MaxConcurrentProfiles=4`; 64 concurrent `profiles/heap` requests against a `fakeUpstream`; meanwhile `fakeCS.Tracker().Add` a second eligible Pod and slice | peak in-flight `get` ≤ 4; some requests return 429; the new Pod appears in `targets` within 5s |
| no dial on confirm failure (real proxy) | `Upstream: proxy.New(proxy.Options{})`, target = trap address, `confirmErr=ErrTargetChanged` | 503 `target_changed`; trap 0; with the confirm step removed in a mutation (`d.Discovery = noConfirm{…}` wrapper that returns nil) the trap count becomes 1 and the test fails |
| residual window (real proxy) | `Upstream: proxy.New(proxy.Options{})`, `confirmErr=nil`, target = trap | trap 1: documents that nothing after `Confirm` re-checks the address |
| budget before confirm | `fakeDiscovery.Confirm` records `ctx.Deadline()` | deadline within 1s of now + effective + 30s |
| committed stream failure aborts the connection | `Upstream` is `proxy.New(proxy.Options{})` and the target is an `httptest.Server` that writes headers, half a body, then hijacks and closes; the handler runs inside a real `httptest.Server` | the client's `io.ReadAll` returns `io.ErrUnexpectedEOF`, not a clean EOF; the audit record carries `upstream_stream_failed` |
| target headers on forwarded errors | `fakeUpstream` writes `TargetHeaders` and 500 | headers present; `code` `upstream_500` in audit and recorder |
| gateway errors carry no target headers | every error row | absent |
| audit line | success and each error row | one JSON record with `principal,namespace,service,pod,profile,seconds,status,code,duration_ms`; never the IP |
| metrics | each row | `Recorder.Request(endpoint, profile, code, _)` called once; targets rows `("targets","none")`; `POST …/targets` → `("targets","none")`; `HEAD …/profiles/heap` → `("profile","heap")`; `route_unknown` and `profile_unknown` rows → `("profile","none")` |
| snapshot | `onTargets` swaps `Config` to a realm that denies the namespace | the request completes under the original config |
| no-store | every response | `Cache-Control: no-store` |
| json content type | every gateway body | `application/json` |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

One `http.HandlerFunc` registered at `/` does the whole algorithm, so route validation always precedes the method check
(Go's `ServeMux` method patterns would invert that order and also treat `HEAD` as `GET`).
Route parsing: `routeRE = regexp.MustCompile(`+"`"+`^/v1/namespaces/([^/]+)/services/([^/]+)/(targets|profiles/([^/]+))$`+"`"+`)`;
no match, or a namespace or service segment failing `validation.IsDNS1123Label`, → `404 route_unknown`;
then `r.Method != http.MethodGet` → `405 method_not_allowed` with `Allow: GET`;
then readiness, authentication, realm, parameters, discovery, filter and select, admit, confirm, proxy,
exactly the order of the spec's *Request algorithm*.
`cfg := d.Config.Load()` once at the top.
Selection: `Deps.Choose func(n int) int` (nil → `rand.IntN`) picks the index among remaining targets.
Admission: `slots := make(chan struct{}, cfg.Limits.MaxConcurrentProfiles)` created in `New` from the startup config
(a restart field); `select { case slots <- struct{}{}: default: return 429 }`; release in `defer`.
Budget: `ctx, cancel := context.WithTimeout(r.Context(), time.Duration(effective+30)*time.Second)` (30s when no duration)
right after admission; pass `ctx` to `Confirm` and `Upstream.Do`.
Target headers are passed to the proxy as `TargetHeaders`; gateway errors never set them.
Audit: one `slog.Info("request", …)` in a `defer` with the final `code`.
After `Upstream.Do` returns `Outcome{Committed: true, Code: "upstream_stream_failed"}`,
the handler records audit and metrics (the deferred calls run first) and then `panic(http.ErrAbortHandler)`,
which makes `net/http` drop the connection without a stack trace instead of finishing the chunked body cleanly;
the client observes a transport-level truncation, as the spec's *Failure Scenarios* requires.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "feat(httpapi): serve targets and profiles"
```

---

## Serve lifecycle and ops listener

**Files:**
- Create: `internal/ops/ops.go`, `internal/ops/ops_test.go`, `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`

**Produces:**

```go
package ops
// New returns a handler serving /healthz (always 200), /readyz (200 when ready() is true),
// and /metrics from reg.
func New(ready func() bool, reg *prometheus.Registry) http.Handler
```

`serve.go`: `func serve(ctx context.Context, cfgPath string, deps serveDeps, stdout, stderr io.Writer) int` where

```go
type serveDeps struct {
    namespaceFile string                             // production: the projected token directory's namespace file
    runtime  func(k8s.Options) (k8s.Runtime, error) // production: k8s.NewRuntime
    upstream httpapi.Upstream                        // production: proxy.New(proxy.Options{})
    registry *prometheus.Registry                    // production: prometheus.NewRegistry()
    recorder metrics.Recorder                        // production: metrics.NewPrometheus(registry)
    stop     <-chan struct{}                         // production: signal.NotifyContext(...).Done()
}
```

Tests fill `runtime` with a closure returning `k8s.NewRuntimeWithClientset(fakeCS, opts)` and `upstream` with a blocking fake.

- [ ] **Write the ops tests**: `/healthz` 200; `/readyz` 503 then 200 after the `ready` func flips;
  `/metrics` contains `profgate_discovery_synced`; no request carries auth.

- [ ] **Write the serve tests** (each with a fake clientset and a temp namespace file):

| Subtest | Setup | Expect |
|---|---|---|
| warning on disabled auth | good config | a stdout JSON line with `"level":"WARN"` and `authentication disabled` |
| logger is JSON on stdout | any | every stdout line parses as JSON |
| preflight forbidden | watch reactor forbidden on pods | exit 1; a log record with `"resource":"pods"` and `"verb":"watch"` |
| preflight transient then ok | list reactor fails twice then succeeds | continues; two records `preflight attempt` with `error`; `/readyz` becomes 200 |
| responses during preflight | list reactor blocks until released | while blocked: `/healthz` 200, `/readyz` 503, `GET /v1/namespaces/a/services/b/targets` → 503 `not_ready`; after release `/readyz` 200 |
| preflight forbidden closes listeners | watch forbidden | after exit both ports refuse connections |
| synced gauge | after ready | `DiscoverySynced(true)` called once (recording `Recorder`) |
| drain | send on `stop` while a `fakeUpstream` request blocks | `/readyz` 503 immediately; the listener refuses new connections; exit 0 after the request is released; with limits 1/1 and the request never released, exit within 31s |

- [ ] **Implement**

Order from the spec's *Startup and shutdown*:

1. `cfg, err := config.Load(cfgPath)`; `logger := slog.New(slog.NewJSONHandler(stdout, nil))`;
   `logger.Warn("authentication disabled; access is controlled only by network boundary and static realm policy")`;
   `var cfgPtr atomic.Pointer[config.Config]; cfgPtr.Store(cfg)`;
   `opts := k8s.Options{VersionLabel: cfg.Discovery.VersionLabel, Port: cfg.Discovery.Pprof.Port, PortName: cfg.Discovery.Pprof.PortName, NamespaceFile: deps.namespaceFile, Logger: logger}`;
   `rt, err := deps.runtime(opts)`; `cluster := rt.Cluster()` (not running).
2. `api := httpapi.New(httpapi.Deps{Discovery: cluster, Upstream: deps.upstream, Config: &cfgPtr, Recorder: deps.recorder, Logger: logger})`
   (`Choose` nil → random) and `opsHandler := ops.New(ready, deps.registry)`;
   production passes `registry := prometheus.NewRegistry()` and `recorder := metrics.NewPrometheus(registry)` so both see one registry;
   `ready` returns `!draining.Load() && cluster.HasSynced()`.
3. `net.Listen` on both addresses (port conflicts fail here), then `go apiServer.Serve(l1)` and `go opsServer.Serve(l2)`;
   from this point `/healthz` is 200, `/readyz` 503, and `/v1` answers `503 not_ready` because `HasSynced` is false.
4. Start the preflight goroutine: it loops `rt.Preflight(runCtx)` with backoff 1s doubling to 30s,
   logs each failure as `preflight attempt` with the error, and sends exactly one value on `preflightCh chan error`:
   `nil` on success, the `ErrForbidden` on a 403, or `runCtx.Err()` if cancelled.
5. The event loop, entered immediately after step 4, is the only place that reacts to events:

```go
for {
    select {
    case err := <-preflightCh:
        var fb k8s.ErrForbidden
        if errors.As(err, &fb) { log; shutdown(); return 1 }
        if err != nil { shutdown(); return 1 }          // runCtx cancelled during preflight
        go cluster.Run(runCtx); go waitSynced()   // waitSynced sends on syncedCh when HasSynced
    case <-syncedCh:
        recorder.DiscoverySynced(true)
    case err := <-errCh:                                   // a Serve returned
        if errors.Is(err, http.ErrServerClosed) { continue }
        log; shutdown(); return 1
    case <-deps.stop:
        shutdown(); return 0
    }
}
```

   `shutdown()`: `draining.Store(true)`; `cancelRun()` (stops informers and any preflight attempt);
   `apiServer.Shutdown(drainCtx)` with `drainCtx` bounded by `max(cpu,trace)+30s`,
   and `apiServer.Close()` if that returns `context.DeadlineExceeded`;
   `opsServer.Shutdown` with a 5s context.
   `draining` is an `atomic.Bool` read by `ready`; `HasSynced` is synchronized by the informer library;
   the lifecycle has no other state variable — readiness is `!draining && HasSynced()`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ops/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/ops/ cmd/profgate/
git commit -m "feat(cli): run the gateway end to end"
```

---

## Deployment manifests and manifest tests

**Files:**
- Create: `deploy/base/serviceaccount.yaml`, `clusterrole.yaml`, `clusterrolebinding.yaml`, `configmap.yaml`,
  `deployment.yaml`, `service.yaml`, `networkpolicy-gateway.yaml`, `networkpolicy-app-example.yaml`, `kustomization.yaml`;
  `deploy/deploy_test.go`

- [ ] **Write the failing tests** (`sigs.k8s.io/yaml` into typed objects; `kustomization.yaml` lists every file):

| Test | Assertion |
|---|---|
| ClusterRole tuples | the set of `(apiGroup,resource,verb)` equals exactly the seven; every rule has only `apiGroups`,`resources`,`verbs`; no `*`, no `resourceNames`, no `nonResourceURLs` |
| ClusterRoleBinding | binds the ClusterRole to ServiceAccount `profgate` in namespace `profgate` |
| Deployment | `replicas: 2`; securityContext exactly the spec's *Container* block; volumes: only the ConfigMap, mounted read-only at `/etc/profgate`; args contain `--config` and `/etc/profgate/config.yaml`; readiness probe `/readyz` on port `ops` (9090); `terminationGracePeriodSeconds: 120`; container ports named `api` 8080 and `ops` 9090 |
| Service | `ClusterIP`; exactly one port, 8080 named `api` |
| Gateway NetworkPolicy | `policyTypes: [Ingress]`; rule 1 port 8080 from `namespaceSelector` `kubernetes.io/metadata.name: ingress-nginx`; rule 2 port 9090 from `namespaceSelector` `kubernetes.io/metadata.name: monitoring`; nothing else |
| App example NetworkPolicy | ingress on `6060/TCP` from `namespaceSelector` `kubernetes.io/metadata.name: profgate` and `podSelector` `app.kubernetes.io/name: profgate`; nothing else |
| ConfigMap | `config.Load` on the embedded `config.yaml` succeeds and the anonymous realm lists `"*"` for all three lists |

- [ ] **Write the manifests** to pass; the base image is `ghcr.io/arloliu/profgate:latest` (overridden by overlays and production kustomizations).

- [ ] **Validate and commit**

```bash
mise exec -- go get sigs.k8s.io/yaml@v1.6.0
mise exec -- go test -race ./deploy/ && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add deploy/ go.mod go.sum
git commit -m "feat(deploy): add kustomize base with pinned RBAC"
```

---

## End-to-end lanes, registry, harness, and test application

**Files:**
- Create: `test/e2e/versions.yaml`, `test/e2e/lanes.go`, `test/e2e/lanes_test.go`, `test/e2e/registry.go`,
  `test/e2e/harness_test.go`, `test/e2e/testapp/main.go`, `test/e2e/testapp/deployment.yaml`,
  `test/e2e/overlays/default/kustomization.yaml`, `test/e2e/overlays/reduced-no-watch/*`, `test/e2e/overlays/reduced-no-get/*`,
  `test/e2e/overlays/api-outage/networkpolicy.yaml`

**Produces** (no build tag, so unit tests cover them):

```go
package e2e

type Lane struct {
    Name          string `yaml:"name"`
    Frozen        bool   `yaml:"frozen"`
    Degraded      bool   `yaml:"degraded"`
    NetworkPolicy bool   `yaml:"networkPolicy"`
    Kind          string `yaml:"kind"`
    Image         string `yaml:"image"`
}
func LoadLanes(path string) ([]Lane, error)   // validates per the spec's "Cluster matrix"
// LaneNames returns the names in file order; unfrozenOnly keeps lanes with Frozen == false.
func LaneNames(lanes []Lane, unfrozenOnly bool) []string

// Scenario is metadata only, so it compiles without the e2e tag; runners live in tagged files.
type Scenario struct {
    Name               string
    NeedsPodReach      bool
    NeedsNetworkPolicy bool
}
// Scenarios returns a copy of the complete, ordered scenario metadata (an unexported array).
func Scenarios() []Scenario
var scenarios = [...]Scenario{
    {Name: "dedupe and wrong-address slice"},
    {Name: "ineligible pods", NeedsPodReach: true},
    {Name: "convergence on delete"},
    {Name: "convergence on ready", NeedsPodReach: true},
    {Name: "profiles parse", NeedsPodReach: true},
    {Name: "errors"},
    {Name: "version filter"},
    {Name: "rbac"},
    {Name: "replicas agree", NeedsPodReach: true},
    {Name: "api outage", NeedsNetworkPolicy: true},
}
func (s Scenario) Skips(l Lane) (bool, string) // the reason names the scenario and the missing capability
```

`harness_test.go` (`//go:build e2e`):

```go
type Harness struct {
    Lane     Lane
    Cluster  string               // kind cluster name
    Client   kubernetes.Interface // tester kubeconfig
    Gateways [2]*http.Client      // through standing port-forwards opened in TestMain
    scenario *Scenario            // set by TestScenarios before Run
}

// runners returns the implementation for every scenario name. It is a function, not package
// state. In this task every entry is the placeholder below; the scenarios task replaces each value
// with a named function from scenarios_test.go.
func runners() map[string]func(t *testing.T, h *Harness) {
    notWritten := func(t *testing.T, _ *Harness) { t.Skip("runner not written") }
    return map[string]func(t *testing.T, h *Harness){
        "dedupe and wrong-address slice": notWritten,
        "ineligible pods":                notWritten,
        "convergence on delete":          notWritten,
        "convergence on ready":           notWritten,
        "profiles parse":                 notWritten,
        "errors":                         notWritten,
        "version filter":                 notWritten,
        "rbac":                           notWritten,
        "replicas agree":                 notWritten,
        "api outage":                     notWritten,
    }
}
func (h *Harness) Namespace(t *testing.T) string                      // creates a namespace from t.Name(), deletes it on cleanup
func (h *Harness) Apply(t *testing.T, ns, overlay string)
func (h *Harness) ForwardTestApp(t *testing.T, ns, pod string) string // fails the test unless h.scenario.NeedsPodReach
func (h *Harness) WaitPodReady(t *testing.T, ns, name string)
func (h *Harness) WaitPodGone(t *testing.T, ns, name string)
func (h *Harness) WatchPods(t *testing.T, ns string) <-chan watch.Event

// harness is the one test-only package variable: TestMain fills it, TestScenarios reads it.
// It is the named exception to the global-state rule for the e2e package.
var harness *Harness
```

- [ ] **Write `versions.yaml`** exactly as the spec's *Cluster matrix*.

- [ ] **Write `lanes_test.go`** (no build tag): `LoadLanes` on the real file succeeds; `LaneNames` rows for both flag values;
  synthetic inputs fail for a registry-prefixed image, a non-64-hex digest, an unknown kind version,
  `degraded` on a non-frozen lane, and no lane with `networkPolicy: true`;
  `Scenarios()` contains a scenario named `convergence on ready` with `NeedsPodReach`;
  every scenario name is unique;
  for `Lane{Degraded: true}` the skipped names are exactly `ineligible pods`, `convergence on ready`, `profiles parse`, `replicas agree`;
  for `Lane{NetworkPolicy: false}` exactly `api outage`.

- [ ] **Write the test application**

`main.go`: `net/http/pprof` on `:6060`; `/healthz` returns 200 or 503 from an `atomic.Bool` (initially true);
`POST /healthz/fail` sets false, `POST /healthz/pass` sets true;
a counter of `/debug/pprof/*` requests exposed at `GET /hits` as `{"pprof":N}`
so the API-outage scenario can prove no profile request arrived.
`deployment.yaml`: label `app.kubernetes.io/version=1.0.0`, readiness probe `/healthz` with `periodSeconds: 1`, `failureThreshold: 1`,
`terminationGracePeriodSeconds: 60`, `preStop` `sleep 30`, container port `pprof` 6060/TCP, a Service `testapp` selecting it.

- [ ] **Write the overlays**

`default`: `resources: [../../../../deploy/base]`, `images: [{name: ghcr.io/arloliu/profgate, newName: ko.local/profgate, newTag: e2e}]`,
and a patch setting both namespace selectors in the gateway NetworkPolicy to the harness namespace.
`reduced-no-watch` and `reduced-no-get`: each defines its own ServiceAccount, ClusterRole (missing the named tuple),
ClusterRoleBinding, and Deployment (replicas 1, same image), all named `profgate-no-watch` or `profgate-no-get` respectively,
so both can exist at once.
`api-outage/networkpolicy.yaml`: `policyTypes: [Egress]` selecting the gateway Pods;
egress allowed to the test-app namespace on 6060/TCP and to `kube-system` on 53 UDP and TCP;
nothing else, so the `kubernetes` Service and the control-plane node are unreachable while the test app stays reachable.

- [ ] **Write `TestMain`**

```go
func TestMain(m *testing.M) {
    lane := laneFromEnv()                       // PROFGATE_E2E_LANE, default "current"
    cluster := "profgate-" + lane.Name
    keep := os.Getenv("PROFGATE_E2E_KEEP") != ""
    if !(keep && clusterExists(lane, cluster)) {
        run("mise", "x", "kind@"+lane.Kind, "--", "kind", "create", "cluster", "--name", cluster,
            "--image", registry()+"/"+lane.Image)
    }
    os.Setenv("KO_DOCKER_REPO", "ko.local")
    os.Setenv("VERSION", "e2e")
    run("ko", "build", "--local", "--bare", "--tags", "e2e", "./cmd/profgate")      // -> ko.local/profgate:e2e
    run("ko", "build", "--local", "--bare", "--tags", "e2e", "./test/e2e/testapp")   // -> ko.local/testapp:e2e
    run("mise", "x", "kind@"+lane.Kind, "--", "kind", "load", "docker-image", "--name", cluster,
        "ko.local/profgate:e2e", "ko.local/testapp:e2e")
    // apply overlays/default into namespace "profgate", wait for 2 ready gateway Pods,
    // open one port-forward per gateway Pod into harness.Gateways
    code := m.Run()
    if !keep {
        run("mise", "x", "kind@"+lane.Kind, "--", "kind", "delete", "cluster", "--name", cluster)
    }
    os.Exit(code)
}
```

`--bare` makes ko name the image `$KO_DOCKER_REPO/<last path element>` with no hash suffix, so both references are known in advance.
`registry()` returns `PROFGATE_E2E_REGISTRY` or `docker.io`.
Port-forwards use `k8s.io/client-go/tools/portforward` with the tester's kubeconfig.

- [ ] **Write `TestScenarios`** in `harness_test.go` (it never calls `t.Parallel`, because it sets the current scenario):

```go
func TestScenarios(t *testing.T) {
    rs := runners()
    for _, s := range Scenarios() {
        s := s
        t.Run(s.Name, func(t *testing.T) {
            run, ok := rs[s.Name]
            if !ok {
                t.Fatalf("scenario %q has no runner", s.Name)
            }
            if skip, why := s.Skips(harness.Lane); skip {
                t.Log(why)
                t.Skip(why)
            }
            harness.scenario = &s
            run(t, harness)
        })
    }
}
```

- [ ] **Run the harness with placeholder runners**

```bash
PROFGATE_E2E_LANE=current PROFGATE_E2E_KEEP=1 mise run test:e2e
```

Expected: cluster created, both images loaded, two gateway Pods ready, `TestScenarios` reports every subtest as skipped with `runner not written`.

- [ ] **Validate and commit**

```bash
mise exec -- go vet -tags e2e ./test/e2e/... && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add test/e2e/ go.mod go.sum
git commit -m "test(e2e): add lane matrix, harness, and test app"
```

---

## End-to-end scenarios

**Files:**
- Create: `test/e2e/scenarios_test.go` (`//go:build e2e`; one named function per scenario)
- Modify: `test/e2e/harness_test.go` (`runners()` maps each name to its function instead of `notWritten`; `TestScenarios` is unchanged)

Each entry below is a function `scenario<Name>(t *testing.T, h *Harness)` in `scenarios_test.go`,
wired into `runners()` in place of `notWritten`;
the capability flags already live in `Scenarios()`.
Every scenario creates its namespace with `h.Namespace(t)` and applies the test app unless stated.

- [ ] **`dedupe and wrong-address slice`**: create a manual EndpointSlice (`endpointslice.kubernetes.io/managed-by: profgate-e2e`,
  label `kubernetes.io/service-name: testapp`) duplicating the controller's endpoint,
  and a second one with address `10.255.255.255` and the real `targetRef`;
  `targets` lists each Pod exactly once; `profiles/heap` succeeds on both gateways.
- [ ] **`ineligible pods`** (`NeedsPodReach`): `ForwardTestApp` + `POST /healthz/fail` → the Pod leaves `targets` within 10s;
  delete a Pod → it is absent from `targets` while `Terminating`;
  a Service with `publishNotReadyAddresses: true` over a failing Pod → empty `targets`.
- [ ] **`convergence on delete`**: expected set = current minus one Pod; `WatchPods` until the `DELETED` event;
  poll both gateways every 500ms until both equal the expected set three times in a row, all within 10s.
- [ ] **`convergence on ready`** (`NeedsPodReach`): `fail` a Pod, wait for `targets` to drop it, compute expected = current plus that Pod,
  `pass`, watch until the Pod's `Ready` condition is `True`, then the same poll rule.
- [ ] **`profiles parse`** (`NeedsPodReach`): for each of the eight profiles fetch through gateway 0;
  `profile.Parse` succeeds for all but `trace`; the `trace` body starts with `go 1.`; `cpu?seconds=2` takes between 2s and 5s;
  every response carries the three `X-Pprof-Target-*` headers and `Cache-Control: no-store`.
- [ ] **`errors`**: the shared gateways run the wildcard realm and cannot be reconfigured live,
  so this scenario deploys its own gateway: apply the `default` overlay into the scenario namespace
  with a ConfigMap whose realm lists `namespaces: ["<scenario ns>"]`, wait for its Pod, port-forward to it,
  and run every assertion below against that gateway.
  With namespace `other` created and holding a `testapp` Service:
  `other/testapp` (exists) and `other/missing` → both 403 with identical bodies;
  `<ns>/missing` → 404 `service_not_found`; a selectorless Service with a manual Pod-backed slice → 422;
  `?pod=<pod of another Service>` → 404 `pod_not_found`.
- [ ] **`version filter`**: two test-app Deployments with versions `1.0.0` and `2.0.0` and one without the label, one Service over all three;
  100 requests with `?version=2.0.0` all report `X-Pprof-Target-Version: 2.0.0`; `?version=3.0.0` → 503 `no_targets`.
- [ ] **`rbac`**: the main gateway Pods are ready; apply `reduced-no-watch` and `reduced-no-get` together;
  the `profgate-no-watch` Pod and the `profgate-no-get` Pod each reach `CrashLoopBackOff`,
  and their logs contain `"resource":"pods"` with `"verb":"watch"` and `"verb":"get"` respectively;
  on lane `1.24`, no Secret of type `kubernetes.io/service-account-token` references the gateway ServiceAccount.
- [ ] **`replicas agree`** (`NeedsPodReach`): sorted `targets` from both gateways are equal; `?pod=<x>` to each returns identical `X-Pprof-Target-*`.
- [ ] **`api outage`** (`NeedsNetworkPolicy`): read `/hits` from the test app; apply `overlays/api-outage`;
  on both gateways `/readyz` (through a port-forward to 9090) is 200, `targets` returns the cached list,
  `profiles/heap` → 503 `discovery_unavailable`; `/hits` unchanged; delete the policy; `profiles/heap` → 200 within 30s.

- [ ] **Run all lanes locally once**

```bash
PROFGATE_E2E_LANE=current mise run test:e2e
PROFGATE_E2E_LANE=1.23 mise run test:e2e
PROFGATE_E2E_LANE=1.24 mise run test:e2e
```

Expected: every scenario passes on `current`;
on `1.23` and `1.24` the `api outage` scenario is skipped with its logged reason and everything else passes.

- [ ] **Validate and commit**

```bash
mise exec -- go get github.com/google/pprof@v0.0.0-20260802141513-ef3492d7dac3
mise exec -- go vet -tags e2e ./test/e2e/... && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add test/e2e/ go.mod go.sum
git commit -m "test(e2e): prove discovery, proxying, and RBAC"
```

---

## Continuous integration

**Files:**
- Create: `.github/workflows/check.yml`, `.github/workflows/e2e.yml`

- [ ] **`check.yml`**: `on: push`; `jdx/mise-action@v2`; `mise run check && mise run lint && mise run test`.
- [ ] **Lane printer**: `test/e2e/cmd/lanes/main.go` (untagged) calls `LoadLanes` and `LaneNames` from the `e2e` package
  and prints the JSON list, taking `-unfrozen` as its only flag.
  `LaneNames` and its tests already exist from the harness task.
- [ ] **`e2e.yml`**:

```yaml
name: e2e
on:
  pull_request:
  push:
    branches: [main]
jobs:
  lanes:
    runs-on: ubuntu-latest
    outputs:
      lanes: ${{ steps.list.outputs.lanes }}
    steps:
      - uses: actions/checkout@v4
      - uses: jdx/mise-action@v2
      - id: list
        run: |
          FLAG=""
          if [ "${{ github.event_name }}" = "pull_request" ]; then FLAG="-unfrozen"; fi
          echo "lanes=$(mise exec -- go run ./test/e2e/cmd/lanes $FLAG)" >> "$GITHUB_OUTPUT"
  e2e:
    needs: lanes
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        lane: ${{ fromJSON(needs.lanes.outputs.lanes) }}
    env:
      PROFGATE_E2E_REGISTRY: ${{ secrets.E2E_REGISTRY }}
      PROFGATE_E2E_LANE: ${{ matrix.lane }}
    steps:
      - uses: actions/checkout@v4
      - uses: jdx/mise-action@v2
      - uses: docker/login-action@v3
        with:
          registry: ${{ secrets.E2E_REGISTRY }}
          username: ${{ secrets.E2E_REGISTRY_USER }}
          password: ${{ secrets.E2E_REGISTRY_TOKEN }}
      - run: mise run test:e2e
```
- [ ] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add .github/ test/e2e/
git commit -m "ci: run checks on push and lanes on main"
```

---

## Finish the plan

- [ ] Confirm the `main` run passed every lane.
- [ ] In the same change: set line 3 of this file to `**Status:** Done` and add line 4 `**Outcome:** <tag or commit that shipped the gateway>`.
- [ ] `mise run lint && mise run test && mise run check`;
  `git add docs/plans/gateway.md`; `git commit -m "docs: mark the gateway plan done"`.

---

## Self-Review

- Spec coverage: permission boundary and preflight (*Module…*, *Discovery types…*, *Deployment manifests…*, the `rbac` scenario);
  compatibility (pinned modules, lanes); resolution and confirmation (*Informers…*, *Confirmation…*);
  HTTP API and errors (*HTTP API*); proxy (*Proxy*); realms and non-disclosure (*HTTP API* rows); operations (*Serve lifecycle…*);
  metrics (*Metrics recorder*); configuration (*Configuration*); build and deployment (*Module…*, *Deployment manifests…*, *Continuous integration*);
  every end-to-end scenario (*End-to-end scenarios*); failure scenarios (serve tests, `api outage`).
- Types: `k8s.Target`, `k8s.Discovery`, `k8s.Cluster`, `proxy.Request`, `proxy.Outcome`, `proxy.Proxy`,
  `httpapi.Upstream`, `httpapi.Deps`, `metrics.Recorder`, `config.Config`, `e2e.Lane`, `e2e.Scenario`, `e2e.Harness`
  are each defined once and consumed by those names afterwards.
- Left to the implementer by design: helper names inside test files, exact `revive` rules, the goimports local-prefix setting.
- Decided during implementation, recorded here so nobody mistakes them for omissions:
  the exact fixture for the spec's rate-limited relist proof
  (a shared `flowcontrol.RateLimiter` between informer and confirmation calls, and a forced watch close that triggers a relist)
  belongs to the *HTTP API* admission test and is designed when that test is written;
  the `errors` scenario's gateway is an `errors-gateway` overlay with a namespace transform,
  resources renamed `profgate-errors` (ServiceAccount, ClusterRoleBinding, one-replica Deployment),
  and its own ConfigMap, written when that scenario is implemented.
