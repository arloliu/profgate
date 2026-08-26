# Authentication Implementation Plan

**Status:** Approved

> **For agentic workers:** implement this plan one task at a time, in order;
> each task is written test-first and ends with its own validation block and commit.
> Run a task inline or hand it to a subagent, whichever fits its size.
> Checkboxes (`- [ ]`) track progress.

**Goal:** Build the two authentication modes defined in [`docs/specs/auth.md`](../specs/auth.md):
`basic` against a bcrypt user list,
`oidc` against a JWT the issuer signed,
the optional browser flow that turns an authorization-code login into an encrypted session cookie,
the audit reasons and metrics that make failures attributable,
the deployment pieces that mount the secrets,
and the unit and end-to-end layers that prove it.

**Architecture:** One new package.
`internal/auth` is the only non-test importer of `go-jose` and `x/crypto`
and exposes `Authenticator`, `Principal`, and `Failure`;
`internal/config` gains the `basic` and `oidc` blocks and their validation;
`internal/httpapi` calls the authenticator where `principalRealm` is called today
and registers the three `/auth/` routes from a handler `internal/auth` provides;
`cmd/profgate` constructs the authenticator for the configured mode,
adds the `[discovering]` startup state, and gains `auth hash`;
`internal/metrics` gains the authentication rows;
`deploy/` mounts one Secret at `/etc/profgate/auth/`;
`test/e2e/` runs Dex in kind.

**Tech Stack:** everything already pinned, plus
`github.com/go-jose/go-jose/v4 v4.1.4` (JWS parsing, signature verification, JWK set parsing; only in `internal/auth`),
`golang.org/x/crypto v0.55.0` (bcrypt; already indirect, becomes direct; only in `internal/auth`),
and `golang.org/x/term v0.45.0` (already indirect, becomes direct; `cmd/profgate` reads a password without echo).
Dex `v2.45.1` runs in kind for the end-to-end scenarios.

**Spec:** [`docs/specs/auth.md`](../specs/auth.md), layered on [`docs/specs/gateway.md`](../specs/gateway.md).
Every behavior table below restates the spec for the task at hand;
where they differ the spec wins, and the plan is the bug.
Spec sections are cited by heading name; unqualified sections are the authentication spec's.
The unit-test cases in the spec's *Testing* section are normative:
each task below names its slice of them, and the task is done only when every bullet for its package passes.
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- Everything in the gateway and PGO plans' constraints still holds.
- Only `internal/auth` imports `github.com/go-jose/go-jose/v4` and `golang.org/x/crypto` outside `_test.go` files and the `test/` tree;
  `mise run check` already enforces it (`check_auth_importers` in `scripts/check-repo.py`).
- Only `cmd/profgate` imports `golang.org/x/term`, for `auth hash`;
  the *Configuration* task adds `check_term_importers` to `scripts/check-repo.py` so `mise run check` enforces it.
- Only `internal/k8s` imports `k8s.io/client-go`, unchanged;
  no new RBAC tuple, no new `internal/k8s` method, no change to the shipped ClusterRole.
  Authentication touches neither Kubernetes nor NATS (*Core decisions*, "Stateless").
- Every response the gateway generates for an authentication failure carries the fixed message `authentication required`
  and never the credential, the token, the subject, or which check failed (*Failure responses*).
- Credentials travel only in the `Authorization` header or the session cookie;
  `access_token` in the query string is `400 invalid_parameter` before any credential is read, in every mode.
- No fall-through: under `basic` and `oidc`, a request that resolves to no realm is `401`, never `auth.anonymousRealm`.
- Trust is restart-only: the mode, the issuer, the audience, the CA, the client, the key paths.
  No configuration reloader exists (gateway *Non-goals*);
  the spec's `hot` rows name what a future reloader may swap because every request reads one snapshot.
  The only runtime re-reads are the users file and the cookie key file,
  each polled every 30 seconds by the certificate's mechanism.
- Fail closed: stale keys, a failed random source, an unreachable token endpoint each answer `503 auth_unavailable`.
- Global state stays as the gateway plan lists it; every poller, cache, and client is constructed and injected.
- Goroutines that run timers (`Run(ctx)`) are never started under unit test;
  tests drive `Refresh(ctx)` and `Poll()` directly (*Testing*).
- Module files: a task that imports a module for the first time repeats that module's exact `go get` line
  (idempotent), then runs `go mod tidy` and stages `go.mod` and `go.sum` with its package.
  `go build` is broken on the authoring machine because the vfox Go SDK lacks `src/`;
  the versions above were read from the module proxy, and the first task that runs `go get` confirms them.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on what you wrote.

---

## File Structure

```text
go.mod, go.sum                              # + go-jose/v4, x/crypto (direct), x/term (direct)
scripts/check-repo.py                       # + check_term_importers
internal/config/config.go                   # AuthConfig gains Basic and OIDC pointer blocks; validation; realm names
internal/config/users.go                    # BasicUser, ParseUsers, LoadUsersFile, ValidateBasicUsers, bcrypt hash grammar
internal/config/config_test.go, users_test.go, testdata/auth-*.yaml
internal/metrics/recorder.go                # + authentication methods on Recorder and Noop
internal/metrics/prometheus.go              # + the seven authentication metrics
internal/auth/auth.go                       # Authenticator, Principal, Failure, reasons, Challenge, Disabled
internal/auth/poll.go                       # filePoller: Poll, Run, fileReader seam, 30s interval
internal/auth/basic.go                      # Basic: header parsing, gate, dummy hash, passwordComparer
internal/auth/users.go                      # users-file poller and merged user set
internal/auth/hash.go                       # HashPassword for `profgate auth hash`
internal/auth/issuer.go                     # issuerClient: TLS, proxy, timeouts, redirects, body limit
internal/auth/discovery.go                  # discoveryDocument, discover
internal/auth/jwks.go                       # keyFetcher, keySet, jwksCache: Refresh, on-demand cooldown, staleness
internal/auth/verify.go                     # verifier: the ordered checks, claims
internal/auth/mapping.go                    # mapRealm
internal/auth/oidc.go                       # OIDC: NewOIDC, Discover, Run, Authenticate, Routes
internal/auth/cookie.go                     # sealer, cookie key file, session and transaction encodings
internal/auth/wire.go                       # random wire values, PKCE challenge, canonicalReturn
internal/auth/browser.go                    # browser: login, callback, logout; RouteOutcome; AuthRoutes
internal/auth/*_test.go
internal/httpapi/server.go                  # Deps gains Auth, AuthRoutes, Ready; steps 5 and 6; /auth/ dispatch
internal/httpapi/auth.go                    # failure mapping, WWW-Authenticate, redirect, /auth/ handling
internal/httpapi/realm.go                   # principalRealm and anonymousPrincipal go away
internal/httpapi/audit.go                   # + auth_reason and route fields
internal/httpapi/auth_test.go, server_test.go
cmd/profgate/main.go                        # + auth hash
cmd/profgate/auth.go                        # runAuth
cmd/profgate/serve.go                       # authenticator construction, [discovering], readiness, pollers
cmd/profgate/*_test.go
deploy/base/deployment.yaml                 # + optional profgate-auth Secret volume at /etc/profgate/auth/
deploy/base/networkpolicy-gateway.yaml      # + commented egress rule to the issuer
deploy/secret-auth-example.yaml             # commented example Secret, outside the base
deploy/chart/profgate/values.yaml           # + auth block, the auth Secret mount, a commented egress example
deploy/chart/profgate/templates/*.yaml      # + rendered auth keys, volume, mount, env guards
deploy/chart/profgate/README.md
deploy/deploy_test.go, chart_test.go        # + Secret example, volume, chart rows
test/e2e/dex.yaml                           # Dex Deployment + Service for kind
test/e2e/overlays/oidc-gateway/, basic-gateway/
test/e2e/harness_test.go                    # + loadImage, deployDex, applyAuthSecret, config options
test/e2e/registry.go                        # + auth-oidc-browser, auth-basic
test/e2e/scenarios_auth_test.go             # (e2e tag)
docs/keycloak-realm.json                    # the Keycloak realm export the manual verification imports
docs/api.md, configuration.md, deployment.md, authentication.md, README.md
CHANGELOG.md
```

---

## Configuration

**Files:**
- Modify: `internal/config/config.go`, `config_test.go`, `scripts/check-repo.py`
- Create: `internal/config/users.go`, `users_test.go`, `testdata/auth-*.yaml`

**Produces:**

```go
package config

// AuthConfig selects the authentication mode and the block that mode reads.
// Basic and OIDC are pointers so an absent block and a present one with defaults are distinguishable.
type AuthConfig struct {
    Mode           string       `yaml:"mode"           env:"AUTH_MODE"       default:"disabled" validate:"oneof=disabled basic oidc"`
    AnonymousRealm string       `yaml:"anonymousRealm" env:"ANONYMOUS_REALM"`
    Basic          *BasicConfig `yaml:"basic"`
    OIDC           *OIDCConfig  `yaml:"oidc"`
}

type BasicUser struct {
    Name         string `yaml:"name"`
    PasswordHash string `yaml:"passwordHash"`
    Realm        string `yaml:"realm"`
}

type BasicConfig struct {
    Users          []BasicUser `yaml:"users"`
    UsersFile      string      `yaml:"usersFile"      env:"AUTH_BASIC_USERS_FILE"`
    AllowPlaintext bool        `yaml:"allowPlaintext" env:"AUTH_BASIC_ALLOW_PLAINTEXT" default:"false"`
    MaxConcurrent  int         `yaml:"maxConcurrent"  env:"AUTH_BASIC_MAX_CONCURRENT"  default:"16" validate:"min=1,max=1024"`
}

type OIDCConfig struct {
    Issuer           string        `yaml:"issuer"           env:"AUTH_OIDC_ISSUER"`
    Audience         string        `yaml:"audience"         env:"AUTH_OIDC_AUDIENCE"`
    TokenType        string        `yaml:"tokenType"        env:"AUTH_OIDC_TOKEN_TYPE"        default:"id" validate:"oneof=id access"`
    UsernameClaim    string        `yaml:"usernameClaim"    env:"AUTH_OIDC_USERNAME_CLAIM"    default:"sub"    validate:"min=1,max=64"`
    GroupsClaim      string        `yaml:"groupsClaim"      env:"AUTH_OIDC_GROUPS_CLAIM"      default:"groups" validate:"min=1,max=64"`
    CAFile           string        `yaml:"caFile"           env:"AUTH_OIDC_CA_FILE"`
    HTTPProxy        string        `yaml:"httpProxy"        env:"AUTH_OIDC_HTTP_PROXY"`
    DiscoveryTimeout time.Duration `yaml:"discoveryTimeout" env:"AUTH_OIDC_DISCOVERY_TIMEOUT" default:"30s" validate:"min=1s,max=10m"`
    ClockSkew        time.Duration `yaml:"clockSkew"        env:"AUTH_OIDC_CLOCK_SKEW"        default:"30s" validate:"min=0,max=5m"`
    JWKSRefresh      time.Duration `yaml:"jwksRefresh"      env:"AUTH_OIDC_JWKS_REFRESH"      default:"1h"  validate:"min=1m,max=24h"`
    JWKSRefreshMin   time.Duration `yaml:"jwksRefreshMin"   env:"AUTH_OIDC_JWKS_REFRESH_MIN"  default:"1m"  validate:"min=1s,max=1h"`
    JWKSMaxStale     time.Duration `yaml:"jwksMaxStale"     env:"AUTH_OIDC_JWKS_MAX_STALE"    default:"24h" validate:"max=168h"`
    Mapping          OIDCMapping   `yaml:"mapping"`
    Browser          *OIDCBrowser  `yaml:"browser"`
}

type OIDCMappingEntry struct {
    Name  string `yaml:"name"`
    Realm string `yaml:"realm"`
}

type OIDCMapping struct {
    Users        []OIDCMappingEntry `yaml:"users"`
    Groups       []OIDCMappingEntry `yaml:"groups"`
    DefaultRealm string             `yaml:"defaultRealm" env:"AUTH_OIDC_DEFAULT_REALM"`
}

type OIDCBrowser struct {
    ClientID         string        `yaml:"clientID"         env:"AUTH_OIDC_CLIENT_ID"`
    ClientSecretFile string        `yaml:"clientSecretFile" env:"AUTH_OIDC_CLIENT_SECRET_FILE"`
    RedirectURL      string        `yaml:"redirectURL"      env:"AUTH_OIDC_REDIRECT_URL"`
    Scopes           []string      `yaml:"scopes"`
    CookieKeyFile    string        `yaml:"cookieKeyFile"    env:"AUTH_OIDC_COOKIE_KEY_FILE"`
    SessionTTL       time.Duration `yaml:"sessionTTL"       env:"AUTH_OIDC_SESSION_TTL"     default:"8h" validate:"min=5m,max=24h"`
    TransactionTTL   time.Duration `yaml:"transactionTTL"   env:"AUTH_OIDC_TRANSACTION_TTL" default:"5m" validate:"min=1m,max=15m"`
}

// ParseUsers decodes YAML holding a single `users` list, rejecting unknown keys;
// the users-file poller in internal/auth calls it on the bytes it read.
func ParseUsers(b []byte) ([]BasicUser, error)

// LoadUsersFile reads path and hands the bytes to ParseUsers.
func LoadUsersFile(path string) ([]BasicUser, error)

// ValidateBasicUsers checks inline and file users together and returns the one bcrypt cost they share.
func ValidateBasicUsers(inline, file []BasicUser, realms map[string]Realm) (cost int, err error)
```

`internal/config` cannot import `x/crypto`, so the bcrypt check is the hash grammar:
`^\$2[aby]\$(\d\d)\$[./A-Za-z0-9]{53}$` with the two digits read as the cost.
That is exactly the string `bcrypt.Cost` accepts, and it is what turns a plaintext password into a validation error.
The default scopes `openid, profile, email` are applied in `normalize`
because a `default` tag cannot express a list.
Realm names (the keys of `realms`) become DNS-1123 labels here:
`validate` checks only realm contents today,
and the spec's *Wire values and bounds* seals the realm name into the session cookie,
so the 63-byte label bound is what keeps the cookie under its size cap.
`scripts/check-repo.py` gains `check_term_importers`, the shape of `check_auth_importers`:
`"golang.org/x/term` may appear only under `cmd/profgate/`.

- [ ] **Write the configuration tests**

`config_test.go` gains a table over `testdata/auth-*.yaml` fixtures, one per row,
and `users_test.go` covers `LoadUsersFile` and `ValidateBasicUsers` directly.
Each rejected row asserts the error names the key the spec's table names.
The tables restate *`basic` mode*, *Users*, *Transport*, *Discovery*, *Principal to realm*, *Configuration* (browser), and *Configuration*.

| Subtest | Configuration | Expect |
|---|---|---|
| disabled unchanged | `mode: disabled`, `anonymousRealm: developer` | loads; `Basic == nil`, `OIDC == nil` |
| realm name label | a realm keyed `""`; a 64-character name; `Developer`; `dev_team` | error names `realms.<name>` and DNS-1123 each; a 63-character lowercase name loads |
| absent block stays nil | `mode: disabled` with no `basic` or `oidc` key, `PROFGATE_AUTH_BASIC_MAX_CONCURRENT=8` in the environment | `Basic == nil`: an environment default cannot make an absent block look configured |
| anonymousRealm required in disabled | `mode: disabled`, no `anonymousRealm` | error names `auth.anonymousRealm` |
| anonymousRealm forbidden in basic | `mode: basic` with `anonymousRealm` | error names `auth.anonymousRealm` |
| anonymousRealm forbidden in oidc | `mode: oidc` with `anonymousRealm` | error names `auth.anonymousRealm` |
| basic block under oidc | `mode: oidc` with a `basic` block | error names `auth.basic` |
| oidc block under basic | `mode: basic` with an `oidc` block | error names `auth.oidc` |
| basic block under disabled | `mode: disabled` with a `basic` block | error names `auth.basic` |
| basic needs a block | `mode: basic`, no `basic` key | error names `auth.basic` |
| basic ok | one inline user at cost 12, `server.tls` set | loads; `Basic.MaxConcurrent == 16` |
| user name rules | empty name; 257 bytes; `a:b` | error names `auth.basic.users[0].name` each |
| hash grammar | `passwordHash: hunter2`; a `$2a$` hash with 52 trailing chars; `$1$` prefix | error names `auth.basic.users[0].passwordHash` each |
| cost range | cost 09; cost 15 | error names the user and the cost each |
| mixed costs | inline users at 10 and 12 | error names both users and both costs |
| mixed costs across file | inline at 12, file at 10 | same shape, naming the file user |
| user realm | `realm: nobody` | error names `auth.basic.users[0].realm` |
| duplicate names | `alice` inline and in the file | error names `alice` and `auth.basic.usersFile` |
| no users | `basic: {maxConcurrent: 4}` | error: at least one user |
| users file unreadable | `usersFile: /nonexistent` | error names `auth.basic.usersFile` |
| users file unknown key | file with `users[0].password` | error names the key |
| parse users | `ParseUsers` on the bytes of the two rows above | the same results as through `LoadUsersFile` |
| plaintext refused | `mode: basic`, no `server.tls` | error names `auth.basic.allowPlaintext` and `server.tls` |
| plaintext allowed | same with `allowPlaintext: true` | loads |
| maxConcurrent range | `0`; `1025` | error each |
| oidc ok | issuer `https://issuer.example`, audience `profgate`, `mapping.defaultRealm: developer` | loads; defaults `tokenType id`, `usernameClaim sub`, `groupsClaim groups`, `discoveryTimeout 30s`, `clockSkew 30s`, `jwksRefresh 1h`, `jwksRefreshMin 1m`, `jwksMaxStale 24h`, `Browser == nil` |
| issuer required | no `issuer` | error names `auth.oidc.issuer` |
| issuer plaintext | `http://issuer.example` | error names `auth.oidc.issuer`; `allowPlaintext` does not apply |
| issuer shape | `https://user@issuer.example`; `https://issuer.example/#x` | error each |
| audience | absent; 257 bytes | error names `auth.oidc.audience` each |
| claim lengths | `usernameClaim` 65 bytes; `groupsClaim` empty | error each |
| caFile | nonexistent; a PEM with no certificate | error names `auth.oidc.caFile` each |
| httpProxy | `ftp://x`; `http://proxy:3128` | error; loads |
| duration ranges | `discoveryTimeout 0s`; `clockSkew 6m`; `jwksRefresh 30s`; `jwksRefreshMin 2h`; `jwksMaxStale 8d` | error each |
| maxStale below refresh | `jwksRefresh 2h`, `jwksMaxStale 1h` | error names both keys |
| mapping empty | no `users`, no `groups`, no `defaultRealm` | error: `oidc` mode can admit nobody |
| mapping names | `users[0].name` empty; 257 bytes; duplicate in `groups` | error each |
| mapping realms | `users[0].realm: nobody`; `defaultRealm: nobody` | error each |
| defaultRealm empty is unset | `defaultRealm: ""` with one group | loads |
| browser ok | `clientID: profgate`, `redirectURL: https://profgate.example/auth/callback`, `cookieKeyFile` readable, `server.tls` set | loads; `Scopes == [openid profile email]`, `SessionTTL 8h`, `TransactionTTL 5m` |
| clientID equals audience | `clientID: other` | error names both keys |
| clientSecretFile | nonexistent; empty file; 1025 bytes | error each; 1024 bytes loads |
| redirectURL | `http://`; with userinfo; with `?x=1`; with `#f`; path `/callback` | error each |
| scopes | without `openid`; duplicate `profile`; a 65-byte scope; a scope with a space | error each |
| cookieKeyFile | absent; unreadable | error each |
| ttl ranges | `sessionTTL 4m`; `sessionTTL 25h`; `transactionTTL 30s`; `transactionTTL 16m` | error each |
| browser needs tls | browser block, no `server.tls` | error names `server.tls`; `allowPlaintext` does not apply |
| browser needs id tokens | browser block with `tokenType: access` | error names `auth.oidc.tokenType` |
| unknown keys | `auth.basic.user`; `auth.oidc.browser.clientId` | error names the key (yaml `KnownFields`) |
| env overrides | every `PROFGATE_AUTH_*` name in the spec's *Configuration* table set on a matching block | the field carries the value |

- [ ] **Run the tests and watch them fail**

- [ ] **Implement**

The realm loop in `validate` first checks `isDNSLabel(name)` on each key
and fails with `realms.<name>: not a DNS-1123 label`.
`validate` gains `validateAuth(cfg)`, run after the realm loop so realm names exist:
the mode selects which block must be present and which must be nil;
`anonymousRealm` is required in `disabled` and forbidden otherwise.
`validateBasic` opens `usersFile` when set, loads it through `LoadUsersFile`,
and runs `ValidateBasicUsers(inline, file, cfg.Realms)`;
the plaintext rule reads `cfg.Server.TLS.Enabled()`.
`validateOIDC` parses `issuer` with `url.Parse` and requires scheme `https`, a host, no user, no fragment;
opens `caFile` and requires `x509.CertPool.AppendCertsFromPEM` to return true;
parses `httpProxy` and requires one of the three schemes;
checks `jwksMaxStale >= jwksRefresh`;
validates the three mapping lists;
and, when `Browser` is set, the browser rows including `tokenType == "id"` and `cfg.Server.TLS.Enabled()`.
File paths are opened and closed the way `validateTLS` does it, so a typo names the key at startup.
`ParseUsers` decodes with `yaml.v3` and `KnownFields(true)` into `struct{ Users []BasicUser }`;
`LoadUsersFile` is `os.ReadFile` then `ParseUsers`.
Error text follows the existing style: the key path, the offending value, the rule.
`check_term_importers` in `scripts/check-repo.py` walks every `.go` file outside `cmd/profgate/`
and reports any that contains `"golang.org/x/term`; `main()` appends its result like the other importer checks.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/config/
mise run lint && mise run test && mise run check
git add internal/config/ scripts/check-repo.py
git commit -m "feat(config): accept basic and oidc auth settings"
```

---

## Metrics recorder

**Files:**
- Modify: `internal/metrics/recorder.go`, `prometheus.go`, `prometheus_test.go`
- Modify: every fake `Recorder` in the repository
  (`grep -rl 'TLSCertificateExpiry(' --include='*_test.go' .` lists them:
  `internal/httpapi/fixtures_test.go`, `internal/pgo/fixtures_test.go`, `internal/tlscert/loader_test.go`, `cmd/profgate/serve_test.go`)

**Produces:**

```go
package metrics

// EndpointAuth is the route family of the three /auth/ routes.
const EndpointAuth Endpoint = "auth"

// CookieKey is one loaded cookie key as the info gauge reports it.
type CookieKey struct {
    Fingerprint string // first 8 hex digits of SHA-256(key)
    Role        string // "current" or "previous"
}

// Recorder gains:
    // AuthFailure records one authentication failure answered 401, 429, or 503; redirects are not failures.
    AuthFailure(mode, reason string)
    // AuthSessionIssued records one browser session minted.
    AuthSessionIssued()
    // JWKSRefresh records one key fetch: "ok" or "failed".
    JWKSRefresh(result string)
    // JWKSKeys reports how many usable keys are held.
    JWKSKeys(n int)
    // JWKSFetched reports when the last successful fetch happened; the age gauge is derived from it.
    JWKSFetched(at time.Time)
    // AuthFileReload records one poll of a file: file is "users" or "cookie_key", result "ok" or "failed".
    AuthFileReload(file, result string)
    // CookieKeys replaces the set of loaded cookie keys the info gauge reports.
    CookieKeys(keys []CookieKey)
```

- [ ] **Write the metrics tests**

`prometheus_test.go` rows, each scraping the registry and asserting the series (*Audit and metrics*):

| Subtest | Call | Expect |
|---|---|---|
| failures | `AuthFailure("basic","bad_credential")` twice | `profgate_auth_failures_total{mode="basic",reason="bad_credential"} 2` |
| sessions | `AuthSessionIssued()` | `profgate_auth_sessions_issued_total 1` |
| jwks refresh | `JWKSRefresh("ok")`, `JWKSRefresh("failed")` | `profgate_oidc_jwks_refresh_total{result="ok"} 1`, `{result="failed"} 1` |
| jwks keys | `JWKSKeys(3)` | `profgate_oidc_jwks_keys 3` |
| jwks age | `JWKSFetched(now - 90s)` | `profgate_oidc_jwks_age_seconds` within 1s of 90 at scrape time; before any call it reads `NaN` |
| file reloads | `AuthFileReload("users","failed")`, `AuthFileReload("cookie_key","ok")` | two series with those labels |
| cookie keys | `CookieKeys([{aaaa,current},{bbbb,previous}])`, then `CookieKeys([{bbbb,current}])` | first scrape has two `profgate_auth_cookie_key_info` series at 1; second has exactly one, `{fingerprint="bbbb",role="current"}` |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

`profgate_oidc_jwks_age_seconds` is a `GaugeFunc` over an atomic Unix timestamp set by `JWKSFetched`,
so the scraped value is the age at scrape time rather than at the last fetch.
Before the first successful fetch the function returns `math.NaN()`:
a `GaugeFunc` cannot be absent, the process start is not a fetch,
and `NaN` never satisfies a threshold alert by accident the way `0` or a growing age would.
`AuthFailure` takes `mode` and `reason` as free strings and pre-registers no label value;
`internal/metrics` imports nothing from `internal/auth`,
which imports `internal/metrics` through `BasicOptions` and `OIDCOptions`,
so the reverse import would be a cycle and the reason table cannot live here.
The label set is still bounded because only `internal/auth` produces reason values:
its `Reasons()` test pins every `Failure.Reason` the package emits,
and the *HTTP API integration* task's closed-set row checks that every `auth_reason` `httpapi` writes is one of `auth.Reasons()`.
`CookieKeys` calls `Reset()` on the `GaugeVec` and sets each key to 1, so a removed key's series disappears.
`Noop` and every fake gain the seven methods; a fake that embeds `metrics.Noop` needs nothing.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/metrics/ ./internal/httpapi/ ./internal/pgo/ ./internal/tlscert/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/metrics/ internal/httpapi/fixtures_test.go internal/pgo/fixtures_test.go internal/tlscert/loader_test.go cmd/profgate/serve_test.go
git commit -m "feat(metrics): record authentication outcomes"
```

---

## Auth package core and `basic` mode

**Files:**
- Create: `internal/auth/auth.go`, `poll.go`, `basic.go`, `users.go`, `hash.go`,
  `auth_test.go`, `poll_test.go`, `basic_test.go`, `users_test.go`, `hash_test.go`
- Modify: `go.mod`, `go.sum`

**Produces:**

```go
package auth

// Authenticator resolves a request to a principal and the name of its realm,
// judged against the configuration snapshot the request loaded.
// A failure carries a Reason for the audit log and never the credential.
type Authenticator interface {
    Authenticate(ctx context.Context, r *http.Request, cfg *config.Config) (Principal, error)
}

type Principal struct {
    Name  string // audit log and PGO CreatedBy/UpdatedBy
    Realm string // key into cfg.Realms
}

// Failure is the error Authenticate returns when the request is not admitted.
type Failure struct {
    Status   int    // 401, 429, or 503
    Reason   string // one value from the audit reason table
    Redirect string // non-empty when a navigation should be sent to login instead
    // ClearSession asks the caller to delete the session cookie before answering;
    // set when the cookie was unopenable or expired.
    ClearSession bool
}

func (f *Failure) Error() string

// The audit reasons, one constant per row of the spec's table.
const (
    ReasonMissing = "missing"; ReasonScheme = "scheme"; ReasonMalformed = "malformed"; ReasonBadCredential = "bad_credential"
    ReasonThrottled = "throttled"; ReasonSignature = "signature"; ReasonAlg = "alg"; ReasonIssuer = "issuer"
    ReasonAudience = "audience"; ReasonTokenType = "token_type"; ReasonExpired = "expired"; ReasonClaim = "claim"
    ReasonNonce = "nonce"; ReasonNoRealm = "no_realm"; ReasonSession = "session"; ReasonState = "state"
    ReasonIssuerDenied = "issuer_denied"; ReasonExchangeDenied = "exchange_denied"; ReasonExchange = "exchange"
    ReasonCSRF = "csrf"; ReasonKeysStale = "keys_stale"; ReasonEntropy = "entropy"
    ReasonInternal = "internal" // an Authenticate error that is not a *Failure; httpapi assigns it
)

// Reasons returns every reason in table order, for the uniformity and closed-set tests;
// the metrics recorder takes the strings without knowing the set.
func Reasons() []string

// Challenge is the WWW-Authenticate value for mode: Basic realm="profgate", Bearer realm="profgate", or "" for disabled.
func Challenge(mode string) string

// Disabled is the mode the gateway ships with: principal anonymous, realm cfg.Auth.AnonymousRealm.
type Disabled struct{}

// HashPassword returns the bcrypt hash of password at cost 12, for `profgate auth hash`.
func HashPassword(password []byte) (string, error)

// BasicOptions is what Basic needs from the outside.
type BasicOptions struct {
    Logger   *slog.Logger
    Recorder metrics.Recorder
}

// NewBasic builds the authenticator from the startup snapshot:
// the gate, the dummy hash at the shared cost, and the users-file poller when usersFile is set.
func NewBasic(cfg *config.Config, opts BasicOptions) (*Basic, error)

// Run polls the users file every 30 seconds until ctx ends; it returns at once when no file is configured.
func (b *Basic) Run(ctx context.Context)
func (b *Basic) Authenticate(ctx context.Context, r *http.Request, cfg *config.Config) (Principal, error)
```

Seams, all unexported fields set by in-package tests (*Testing*):
`passwordComparer` (`compare(hash, password []byte) error`; production is `bcrypt.CompareHashAndPassword`),
`fileReader` (`func(path string) ([]byte, error)`; production is `os.ReadFile`),
and `now func() time.Time` on the poller.
`filePoller` in `poll.go` is shared with the cookie key file:
`newFilePoller(path string, apply func([]byte) error, file string, rec, log)`,
`Poll()` reads, hashes with SHA-256, calls `apply` only when the hash differs,
and records `AuthFileReload(file, result)`;
`Run(ctx)` ticks every 30 seconds and calls `Poll()`.
A read or `apply` that fails leaves the previous state, logs at warn, and counts `failed`.

- [ ] **Write the core and basic tests**

`auth_test.go`: `Disabled` returns `anonymous` and `cfg.Auth.AnonymousRealm`;
`Challenge` for the three modes;
`Reasons()` has 23 entries with no duplicate and equals the list of `Reason*` constants,
and every `Failure` literal in the package names a `Reason*` constant rather than a string
(the test greps the package's non-test sources for `Reason: "` and expects no match),
so the reasons the package can emit are exactly `Reasons()`.
`hash_test.go`: `HashPassword` output parses with cost 12 and verifies with `bcrypt.CompareHashAndPassword`.
`poll_test.go`: with a `fileReader` returning a sequence of byte slices,
`Poll()` calls `apply` on the first read, not on an identical second read, and again on a changed third;
a reader error and an `apply` error each keep the previous state and record `failed`;
an unchanged read records `ok` without calling `apply`.
`basic_test.go` builds `Basic` from a `config.Config` with inline users at cost 10.
The test hashes are precomputed constants in `basic_test.go`:
cost-10 bcrypt hashes of `secret` and of one other password, each with a comment naming its password,
so nothing is minted per run, every fixture passes `ValidateBasicUsers`'s 10–14 cost range,
and the dummy hash follows the configured cost (rows that need cost 12 carry a cost-12 constant).
Every row asserts the `Failure` status and reason, and the comparer's call count.

| Subtest | Request | Expect |
|---|---|---|
| correct password | `Basic alice:secret` | `Principal{alice, developer}`; 1 comparison |
| wrong password | `Basic alice:nope` | 401 `bad_credential`; 1 comparison |
| unknown user | `Basic mallory:x` | 401 `bad_credential`; 1 comparison, against a hash that is not any user's |
| no header | none | 401 `missing`; 0 comparisons |
| wrong scheme | `Bearer abc` | 401 `scheme`; 0 comparisons |
| scheme case | `basic <b64>` | admitted: the scheme token is case-insensitive per RFC 7617 |
| malformed base64 | `Basic !!!` | 401 `malformed` |
| no colon | `Basic base64("alice")` | 401 `malformed` |
| oversize header | a 1025-byte value | 401 `malformed`; 0 comparisons |
| oversize password | a 73-byte password | 401 `malformed`; 0 comparisons |
| 72-byte password | exactly 72 bytes | compared |
| exact name | `Basic Alice:secret` | 401 `bad_credential`: no case folding |
| gate full | `maxConcurrent: 1`, a comparer that blocks on a channel, second request concurrently | second is 429 `throttled` with 0 comparisons of its own; after release a third request is admitted |
| gate released on failure | wrong password, then correct | both complete: the slot is released on every path |
| file user | `usersFile` set, `fileReader` serving a file with `bob` | `bob` admitted after `Poll()`; before it the file set is what `NewBasic` loaded |
| file replaced | `Poll()` after the reader serves a file with `carol` instead of `bob` | `carol` admitted, `bob` is `bad_credential` with 1 comparison |
| file unparseable | reader serves `users: [` | previous set stays; `AuthFileReload("users","failed")` |
| file cost differs | reader serves a user at cost 12 while inline is 10 | previous set stays; `failed` |
| file realm unknown | reader serves `realm: nobody` | previous set stays; `failed` |
| file duplicates inline | reader serves `alice` | previous set stays; `failed` |
| snapshot | a comparer that blocks until the test swaps the file set with `Poll()` | the blocked request completes against the set it loaded |
| dummy cost | inline users at cost 10 | the dummy hash parses with cost 10 |
| plaintext hash rejected upstream | — | covered by the *Configuration* task; `NewBasic` trusts a validated config |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

```bash
mise exec -- go get golang.org/x/crypto@v0.55.0
```

`Authenticate` order: header absent → `missing`;
scheme (the token before the first space, compared case-insensitively) not `Basic` → `scheme`;
value over 1024 bytes → `malformed`;
base64 (standard, padded) fails or no `:` → `malformed`;
password over 72 bytes → `malformed`;
gate `select` with `default` → `throttled`;
lookup by exact name in `cfg.Auth.Basic.Users` (a linear scan of the snapshot, so inline users honor *Configuration snapshot*)
then in the poller's file set (an `atomic.Pointer` to a map);
compare against the user's hash or the dummy hash;
any mismatch → `bad_credential`.
The gate is a `chan struct{}` of `maxConcurrent`, released in `defer`.
The dummy hash is `bcrypt.GenerateFromPassword` of 32 random bytes at the cost `config.ValidateBasicUsers` returned,
computed in `NewBasic` and again by the poller's `apply` when the file changes.
The poller's `apply` parses the bytes it was handed with `config.ParseUsers`
and re-runs `config.ValidateBasicUsers(inline, file, realms)` against the snapshot the poller holds:
the inline users and realm names it captured from the startup configuration.
No reloader replaces that snapshot (*Configuration*), so a file user is judged against the same policy on every poll.

- [ ] **Validate and commit**

```bash
mise exec -- go mod tidy && mise exec -- go test -race ./internal/auth/
mise run lint && mise run test && mise run check
git add internal/auth/ go.mod go.sum
git commit -m "feat(auth): verify Basic credentials with bcrypt"
```

---

## Issuer client, discovery, and signing keys

**Files:**
- Create: `internal/auth/issuer.go`, `discovery.go`, `jwks.go`,
  `issuer_test.go`, `discovery_test.go`, `jwks_test.go`
- Modify: `go.mod`, `go.sum`

**Produces:**

```go
package auth

// issuerClient is the one dedicated http.Client every request to the issuer goes through.
type issuerClient struct {
    get  *http.Client // discovery and JWKS: at most 3 redirects, each https
    post *http.Client // token endpoint: no redirects
}

// issuerOptions: CAFile, HTTPProxy, and the RoundTripper seam (nil means the transport built here).
func newIssuerClient(o issuerOptions) (*issuerClient, error)

// getJSON fetches url, enforces the body limit, decodes one JSON value, and rejects bytes after it.
func (c *issuerClient) getJSON(ctx context.Context, url string, into any) error
// postForm posts form to url without following redirects and returns the status and the limited body.
func (c *issuerClient) postForm(ctx context.Context, url string, form url.Values) (int, []byte, error)

type discoveryDocument struct {
    Issuer, JWKSURI, AuthorizationEndpoint, TokenEndpoint, EndSessionEndpoint string
}

// discover fetches <issuer>/.well-known/openid-configuration and validates it;
// browser says whether the two browser endpoints are required.
func discover(ctx context.Context, c *issuerClient, issuer string, browser bool) (discoveryDocument, error)

// keyFetcher is what the cache calls; the tests program it, production is httpKeyFetcher.
type keyFetcher interface {
    fetch(ctx context.Context) (jose.JSONWebKeySet, error)
}

// httpKeyFetcher fetches one jwks_uri through the issuer client with getJSON.
// Discover builds one per attempt, bound to the jwks_uri the document it just validated names.
type httpKeyFetcher struct {
    client *issuerClient
    url    string
}

func (f *httpKeyFetcher) fetch(ctx context.Context) (jose.JSONWebKeySet, error)

// issuerState is everything discovery produced, published as one value:
// the validated document and a key cache that has fetched at least one usable key.
// OIDC holds it in an atomic.Pointer[issuerState] that is nil until the first successful Discover;
// the verifier and the browser routes load that pointer per request, so no endpoint is usable
// before the keys behind it are, and a retry replaces the whole value.
type issuerState struct {
    doc  discoveryDocument
    keys *jwksCache
}

// keySet is one immutable, validated snapshot of the issuer's usable keys.
// A verification loads one pointer and uses it for staleness, selection, and the signature check.
type keySet struct {
    byKID   map[string]jose.JSONWebKey
    all     []jose.JSONWebKey
    fetched time.Time
}

// stale reports whether this set is older than maxStale at now; a nil set (never fetched) is stale.
func (k *keySet) stale(now time.Time, maxStale time.Duration) bool

type jwksCache struct {
    fetcher    keyFetcher
    now        func() time.Time
    refresh    time.Duration // timer interval
    refreshMin time.Duration // on-demand cooldown
    maxStale   time.Duration
    cur        atomic.Pointer[keySet]
    mu         sync.Mutex // guards lastAttempt
    lastAttempt time.Time
    log *slog.Logger
    rec metrics.Recorder
}

// newJWKSCache builds a cache over fetcher with an empty set; nothing is fetched until Refresh.
func newJWKSCache(fetcher keyFetcher, cfg *config.OIDCConfig, now func() time.Time, log *slog.Logger, rec metrics.Recorder) *jwksCache
// Refresh fetches once and swaps the set on success; the timer and the tests call it.
func (c *jwksCache) Refresh(ctx context.Context) error
// refreshOnDemand runs Refresh at most once per refreshMin across all callers and reports whether it ran.
func (c *jwksCache) refreshOnDemand(ctx context.Context) bool
// Run drives Refresh every refresh until ctx ends.
func (c *jwksCache) Run(ctx context.Context)
func (c *jwksCache) current() *keySet
// stale is current().stale(now(), maxStale), for the tests; the verifier asks the set it loaded instead.
func (c *jwksCache) stale() bool
```

- [ ] **Write the issuer client and discovery tests**

`issuer_test.go` serves everything from `httptest.NewTLSServer` and hands the client the server's transport as the `RoundTripper` seam
(or writes the server certificate to a temporary `CAFile`; the CA row does the latter).
Each row asserts the fetch fails with an error naming the rule (*Issuer client*, *Discovery*, *Testing*).

| Subtest | Server | Expect |
|---|---|---|
| plaintext issuer | `discover` with `http://` | error: issuer must be https |
| issuer mismatch | document `issuer` differs by a trailing slash | error naming both values |
| jwks_uri plaintext | `jwks_uri: http://…` | error naming `jwks_uri` |
| token_endpoint userinfo | `https://u@host/token` with `browser=true` | error naming `token_endpoint` |
| endpoint fragment | `authorization_endpoint` with `#x` | error |
| endpoint relative | `jwks_uri: /keys` | error |
| browser endpoints optional | `browser=false`, document without `authorization_endpoint` | ok |
| browser endpoints required | `browser=true`, document without `token_endpoint` | error |
| end_session optional | `browser=true`, no `end_session_endpoint` | ok, field empty |
| redirect to plaintext | discovery answers `302` to `http://` | error |
| three redirects | three `302`s to https then `200` | ok |
| fourth redirect | four `302`s | error |
| token endpoint redirect | `postForm` answered `307` | error: no redirect followed, the secret was not replayed (the second handler counts zero hits) |
| body limit | a body of 1 MiB + 1 | error |
| body at limit | exactly 1 MiB of valid JSON | ok |
| trailing value | `{} {}` | error |
| trailing whitespace | `{}\n` | ok |
| CA file | server certificate written to `CAFile`, no transport seam | ok; with an empty pool the fetch fails |
| timeouts | a handler that sleeps 6s before headers | error within 5.5s |
| http key fetcher | `httpKeyFetcher{client, url}` against a JWKS handler | `fetch` returns the parsed set; a `404` and a body of `{}` followed by `{}` each fail |
| no environment proxy | `HTTP_PROXY=http://127.0.0.1:1` in the test's environment, no `HTTPProxy` | the fetch succeeds |
| configured proxy | `HTTPProxy` naming an `httptest` proxy that records `CONNECT` | the proxy sees one request |

- [ ] **Write the key set tests**

`jwks_test.go` uses a `fakeFetcher` returning a programmable `jose.JSONWebKeySet` or error and counting calls,
keys generated once per package with `rsa.GenerateKey` (2048 and 1024) and `ecdsa.GenerateKey` (P-256, P-384).
The clock is a variable the test moves.

| Subtest | Steps | Expect |
|---|---|---|
| usable filter | RSA 2048 `sig`, EC P-256 no `use`, an `enc` key, an `oct` key, a 1024-bit RSA, a key with `alg: HS256` | the first two held; the rest dropped with a warn line naming each `kid`; `JWKSKeys(2)` |
| empty set | zero keys | fetch failed; previous set stays; `JWKSRefresh("failed")` |
| all weak | only 1024-bit RSA keys | failed; previous stays |
| duplicate kid | two usable keys sharing `kid` | failed as a whole; previous stays |
| one bad among good | one unusable key among usable ones | ok; usable ones held |
| swap is atomic | a `keySet` pointer loaded before `Refresh`, compared after | the loaded pointer still holds the old keys |
| timer-free | `Run` never called | no goroutine; `Refresh` alone moves `fetched` |
| on-demand once | `refreshOnDemand` twice within `refreshMin` | 1 fetch |
| on-demand after cooldown | clock moved past `refreshMin`, again | 2 fetches |
| concurrent on-demand | 100 goroutines calling `refreshOnDemand` at once | at most 1 fetch |
| failed refresh keeps keys | fetcher errors | old set still current; `failed` counted |
| stale | clock moved `maxStale + 1s` past `fetched` | `stale()` true; after a successful `Refresh` false |
| fetched recorded | successful `Refresh` | `JWKSFetched(now)` called with the fake clock's value |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

```bash
mise exec -- go get github.com/go-jose/go-jose/v4@v4.1.4
```

The transport: `net.Dialer{Timeout: 5s}`, `TLSHandshakeTimeout: 5s`, `ResponseHeaderTimeout: 5s`,
`Proxy: nil` or `http.ProxyURL(parsed)`, `TLSClientConfig.RootCAs` = system pool plus `CAFile`.
Every request runs under `context.WithTimeout(ctx, 10s)`.
`get.CheckRedirect` refuses when `len(via) > 3` or the next URL's scheme is not `https`
(`via` holds the requests already sent, the original first,
so the fourth redirect is the first to see four);
`post.CheckRedirect` always returns an error.
Bodies are read through `io.LimitReader(body, 1<<20+1)`;
a read that fills the limit fails; `json.NewDecoder(...).Decode` then `dec.Token()` must return `io.EOF`
(after `dec.More()` is false), so trailing whitespace passes and a second value fails.
`discover` requires the document's `issuer` to equal the configured one byte for byte,
then validates each recorded endpoint with `url.Parse`: scheme `https`, host present, `User == nil`, `Fragment == ""`.
Key usability follows *Signing keys*;
compatibility between `alg` and a key is a function `compatible(alg string, k jose.JSONWebKey) bool`
that the next task's verifier reuses.

- [ ] **Validate and commit**

```bash
mise exec -- go mod tidy && mise exec -- go test -race ./internal/auth/
mise run lint && mise run test && mise run check
git add internal/auth/ go.mod go.sum
git commit -m "feat(auth): fetch issuer discovery and keys"
```

---

## Token verification, realm mapping, and the `oidc` authenticator

**Files:**
- Create: `internal/auth/verify.go`, `mapping.go`, `oidc.go`,
  `verify_test.go`, `mapping_test.go`, `oidc_test.go`

**Produces:**

```go
package auth

// claims is what verification keeps: everything else in the token is discarded.
type claims struct {
    Subject  string
    Username string
    Groups   []string
    Nonce    string // browser flow only
}

type verifier struct {
    issuer, audience, tokenType, usernameClaim, groupsClaim string
    skew   time.Duration
    state  *atomic.Pointer[issuerState] // shared with OIDC; nil value until Discover succeeds
    now    func() time.Time
    // onKeysLoaded, when set, runs right after the single key set load and before key selection.
    // Production leaves it nil; the in-flight swap test blocks in it.
    onKeysLoaded func()
}

// verify runs the ordered checks of *Token verification* and returns the claims or the Failure.
func (v *verifier) verify(ctx context.Context, token string) (claims, *Failure)

// mapRealm resolves a realm per *Principal to realm*: users, then groups in order, then defaultRealm.
func mapRealm(m config.OIDCMapping, c claims) (realm string, ok bool)

// OIDCOptions is what OIDC needs from the outside.
type OIDCOptions struct {
    Logger   *slog.Logger
    Recorder metrics.Recorder
}

// OIDC is the oidc-mode authenticator: bearer tokens always, the browser flow when configured.
type OIDC struct {
    client   *issuerClient
    state    atomic.Pointer[issuerState] // nil until Discover succeeds; replaced whole on every success
    verifier *verifier
    browser  *browser // nil unless configured
    /* cfg fields, logger, recorder */
}

// NewOIDC builds the client, the verifier, and the browser from the startup snapshot; it performs no network I/O.
// The key cache is not built here: its fetcher is bound to the jwks_uri that discovery returns.
func NewOIDC(cfg *config.Config, opts OIDCOptions) (*OIDC, error)
// Discover fetches and validates the discovery document, then the key set behind its jwks_uri,
// and publishes both as one issuerState only when both succeeded; the caller retries with backoff.
func (o *OIDC) Discover(ctx context.Context) error
// Run drives the refresh timer of the published cache and, with the browser flow, the cookie key poller, until ctx ends.
func (o *OIDC) Run(ctx context.Context)
func (o *OIDC) Authenticate(ctx context.Context, r *http.Request, cfg *config.Config) (Principal, error)
```

The allowed algorithms are a package constant slice
`[]jose.SignatureAlgorithm{RS256, RS384, RS512, ES256, ES384, ES512, PS256, PS384, PS512}`.

- [ ] **Write the verification tests**

`verify_test.go` mints tokens with the package's test keys through `go-jose`'s signer,
serves them against a `fakeFetcher` holding the matching public keys
(the test builds `newJWKSCache(fake, …)`, calls `Refresh` once, and stores an `issuerState` holding it in the verifier's pointer),
and calls `verify` with a fake clock at the token's `iat`.
`mint(t, opts)` takes the key, `kid`, `alg`, `typ`, and a claims map, so each row changes one thing.

| Subtest | Token | Expect |
|---|---|---|
| valid id token | RS256, `kid` k1, `iss`, `sub`, `aud: profgate`, `iat`, `exp: +5m` | claims with `Username == sub` |
| oversize | 17 KiB | 401 `malformed`; fetcher not called |
| not compact | `a.b` | 401 `malformed` |
| flattened JSON JWS | the valid token re-serialized as `{"payload":…,"protected":…,"signature":…}` | 401 `malformed`; fetcher not called |
| general JSON JWS | the same with a `signatures` array | 401 `malformed`; fetcher not called |
| alg not a string | protected header `{"alg": 1, …}` | 401 `malformed` |
| alg twice | protected header carrying `alg` twice | 401 `malformed` |
| alg none | `alg: none` | 401 `alg`; the fetcher fails the test if called |
| alg HS256 | HMAC-signed | 401 `alg`; fetcher not called |
| stale after alg | set older than `maxStale`, valid RS256 token | 503 `keys_stale`; one on-demand fetch attempted |
| stale still 401 for alg none | stale set, `alg: none` | 401 `alg` |
| stale still 401 for oversize | stale set, 17 KiB | 401 `malformed` |
| stale recovers | stale set, fetcher now succeeds | the same token verifies |
| unknown kid refreshes once | `kid` k2 not held, fetcher returns k2 on the second call | verifies; 1 extra fetch |
| unknown kid in cooldown | `kid` k3 right after the row above | 401 `signature`; 0 extra fetches |
| kid never appears | `kid` k9, fetcher unchanged | 401 `signature` |
| no kid, one compatible | token without `kid`, one RSA key held | verifies |
| no kid, two compatible | two RSA keys held | 401 `signature`: the verifier never tries every key |
| no kid, incompatible only | ES256 token, only RSA keys | 401 `signature` |
| ES256 against P-384 | | 401 `signature` |
| RS256 against EC | `kid` names an EC key | 401 `signature` |
| key alg pinned | key carries `alg: RS256`, token `alg: RS384` | 401 `signature` |
| bad signature | payload altered after signing | 401 `signature` |
| issuer | `iss: https://other` | 401 `issuer` |
| sub missing | | 401 `claim` |
| sub empty, too long, NUL | `""`; 257 bytes; `a\x00b` | 401 `claim` each |
| sub not a string | `sub: 42` | 401 `claim` |
| iat missing | | 401 `expired` |
| iat string | `"iat": "123"` | 401 `expired` |
| iat future | `iat: now + 31s` with skew 30s | 401 `expired`; `now + 29s` verifies |
| exp missing | | 401 `expired` |
| exp past | `exp: now - 31s` | 401 `expired`; `now - 29s` verifies |
| exp string | `"exp": "123"` | 401 `expired` |
| nbf future | `nbf: now + 31s` | 401 `expired`; absent `nbf` verifies |
| nbf string | `"nbf": "123"` | 401 `expired` |
| username claim | `usernameClaim: preferred_username` absent | 401 `claim`; present → `Username` is its value and `sub` still required |
| username claim shape | `preferred_username` as `""`; 257 bytes; `a\x00b`; `42` | 401 `claim` each |
| id: aud missing | | 401 `audience` |
| id: aud array contains | `aud: [x, profgate]`, `azp: profgate` | verifies |
| id: multiple aud without azp | `aud: [x, profgate]` | 401 `audience` |
| id: azp wrong | `azp: x` | 401 `audience` |
| access: typ | `tokenType: access`, `typ: JWT` | 401 `token_type`; `typ: at+jwt` and `typ: AT+JWT` verify |
| access: aud | `typ: at+jwt`, `aud: other` | 401 `audience` |
| groups string | `groups: "admins"` | `Groups == [admins]` |
| groups absent | | `Groups` empty |
| groups object | `groups: {}` | 401 `claim` |
| groups mixed | `groups: ["a", 1]` | 401 `claim` |
| in-flight swap | `onKeysLoaded` blocks on a channel; `verify` started in a goroutine with a token under `kid` k1; once the hook is entered the test points the fetcher at a set holding only k2, calls `Refresh`, and waits for it to return; then the hook is released | `verify` succeeds: it selects k1 from the set it loaded before the swap; `state.keys.current()` afterwards lacks k1 and the fetcher was called exactly once by the test's `Refresh` |
| never discovered | the verifier's pointer holds nil | 503 `keys_stale`; no fetch |
| nonce kept | `nonce: abc` | `claims.Nonce == "abc"` |

`mapping_test.go` (*Principal to realm*):

| Subtest | Mapping | Claims | Expect |
|---|---|---|---|
| user first | users `[alice→a]`, groups `[g→b]`, default `c` | alice in g | `a` |
| group order | groups `[g2→b, g1→a]` | groups `[g1, g2]` | `b`: configuration order wins |
| default | groups `[g→b]`, default `c` | no groups | `c` |
| no default | groups `[g→b]` | no groups | not ok |
| exact group | groups `[/eng/pay→a]` | `pay` | not ok |

`oidc_test.go` builds `OIDC` from a config against an `httptest` issuer (discovery + JWKS handlers)
and calls `Authenticate` with real `http.Request`s:

| Subtest | Request | Expect |
|---|---|---|
| bearer ok | `Authorization: Bearer <valid>` with `mapping.users` naming the sub | `Principal{sub, realm}` |
| no credential | none | 401 `missing`; `Redirect == ""` without the browser block |
| wrong scheme | `Basic …` | 401 `scheme` |
| bearer wins | valid bearer and a garbage session cookie | admitted on the bearer |
| no realm | valid token, mapping matches nothing | 401 `no_realm` |
| navigation without browser block | `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document`, no credential | 401 `missing`, no redirect |
| discover fails | discovery handler answers 500 | `Discover` returns an error; the state pointer stays nil; `Authenticate` on a never-discovered `OIDC` is 503 `keys_stale` |
| discover requires a key | JWKS empty on the initial fetch | `Discover` returns an error; the state pointer stays nil |
| discovery ok, keys fail, retry | discovery answers a valid document, the JWKS handler answers 500; `Discover`; then the JWKS handler answers a usable set; `Discover` again | the first `Discover` errors and publishes nothing: the pointer is nil, a valid bearer is 503 `keys_stale`, and no request reached the token endpoint; the second `Discover` publishes document and keys together, and the same bearer verifies |
| retry replaces the state | after a successful `Discover`, discovery answers a document with a different `jwks_uri` and keys served only there; `Discover` again | the pointer holds a new `issuerState`; a token under a `kid` only the new URI serves verifies, and the old `jwksCache` is no longer the one `Run` would drive |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

`verify`: length check;
`jose.ParseSignedCompact(token, allowedAlgs)` — the compact parser only,
because `jose.ParseSigned` also accepts the flattened and general JSON serializations the spec rejects —
which refuses `none` and HMAC before any key work;
its error maps to `alg` when the header `alg` is a string outside the set and `malformed` otherwise
(a non-string or repeated `alg` is caught by decoding the protected header a second time with `json.Decoder`
and counting its `alg` members);
exactly one signature.
Then `st := v.state.Load()`; nil (never discovered) → `Failure{503, keys_stale}`;
`ks := st.keys.current()` is loaded once, and `v.onKeysLoaded()` runs here when set;
`ks.stale(now(), maxStale)` → `st.keys.refreshOnDemand` → a second, explicit load `ks = st.keys.current()` → still stale → `Failure{503, keys_stale}`;
key selection per *Token verification* from `ks.byKID` and `ks.all` with `compatible`,
where an unknown `kid` likewise runs `refreshOnDemand` and, when it reports that a fetch ran, performs the same second load;
`jws.Verify(key.Key)` with the key taken from the `ks` in hand.
A verification loads the key set pointer once, or exactly twice when a refresh it asked for ran, and never in a loop:
a request that already refreshed for staleness finds the cooldown active at the unknown-`kid` step, so no third load exists.
A swap by the timer or by another request between those points is invisible to the verification in flight (*Signing keys*).
Decode the payload into `map[string]json.RawMessage`;
the claim checks in order with the reasons of *Audit and metrics*.
`sub` and the username claim must decode as JSON strings; any other type is `claim`.
`iat`, `exp`, `nbf` are JSON numbers read as `float64` seconds; a string or missing value is `expired`.
`mapRealm` is three loops.
`Authenticate` under `oidc`: `Authorization` present → scheme must be `Bearer` → `verify` → `mapRealm(cfg.Auth.OIDC.Mapping, c)`;
no header and `browser != nil` and a session cookie → the next tasks' session path;
otherwise `missing`, with `Redirect` set only by the browser task.
`Discover`, in order: `doc, err := discover(ctx, o.client, issuer, o.browser != nil)` into a local variable;
`cache := newJWKSCache(&httpKeyFetcher{client: o.client, url: doc.JWKSURI}, …)`;
`cache.Refresh(ctx)`, which fails when the set holds no usable key;
only then `o.state.Store(&issuerState{doc: doc, keys: cache})`.
A failure at any step returns the error and stores nothing,
so the previous pointer (nil before the first success) stays in force,
no endpoint becomes usable between a good document and a bad key set,
and every success replaces the whole value.
`Run` loads the pointer once (the caller starts it only after `Discover` succeeded) and starts that cache's `Run`,
returning when ctx ends.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/auth/
mise run lint && mise run test && mise run check
git add internal/auth/
git commit -m "feat(auth): verify bearer tokens and map realms"
```

---

## Cookie sealing, key file, and wire values

**Files:**
- Create: `internal/auth/cookie.go`, `wire.go`, `cookie_test.go`, `wire_test.go`

**Produces:**

```go
package auth

const (
    cookieSession = "__Host-profgate_session"
    cookieTxn     = "__Host-profgate_txn"
    cookieMaxLen  = 4000
)

// cookieKeys is one snapshot of the key file: current seals, both open.
type cookieKeys struct {
    current  [32]byte
    previous *[32]byte
}

// parseCookieKeys reads one or two base64 lines of 32 bytes each.
func parseCookieKeys(b []byte) (cookieKeys, error)
// fingerprint is the first 8 hex digits of SHA-256(key).
func fingerprint(key [32]byte) string

// sealer seals and opens cookie values under the current key file snapshot.
type sealer struct {
    keys atomic.Pointer[cookieKeys]
    rand io.Reader
    now  func() time.Time
}

// seal returns base64url(nonce || AES-256-GCM(plaintext, aad=name)) or a Failure{503, entropy}.
func (s *sealer) seal(name string, plaintext []byte) (string, *Failure)
// open tries the current key then the previous one; false when neither opens.
func (s *sealer) open(name, value string) ([]byte, bool)

type session struct { Principal, Realm string; Exp time.Time }
type transaction struct { State, Nonce, Verifier, Return string; Exp time.Time }
// encode/decode: two-byte big-endian length per string field, eight-byte big-endian Unix seconds for Exp,
// no bytes left over.
func (s session) encode() []byte;  func decodeSession(b []byte) (session, bool)
func (t transaction) encode() []byte;  func decodeTransaction(b []byte) (transaction, bool)

// setCookie writes name=value; Secure; HttpOnly; SameSite=Lax; Path=/; Max-Age=<seconds>, refusing a value over cookieMaxLen.
func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) error
// deleteCookie writes the same attributes with an empty value and Max-Age=0.
func deleteCookie(w http.ResponseWriter, name string)
// DeleteSessionCookie is deleteCookie for the session cookie, exported for httpapi's ClearSession handling.
func DeleteSessionCookie(w http.ResponseWriter)

// randomValue reads 32 bytes from r and returns them as 43 characters of unpadded base64url.
func randomValue(r io.Reader) (string, *Failure)
// challenge is base64url(SHA-256(ASCII(verifier))).
func challenge(verifier string) string
// canonicalReturn applies the return-path rule of *Wire values and bounds*; anything else is "/".
func canonicalReturn(raw string) string
```

- [ ] **Write the sealing and wire tests**

`cookie_test.go` (*Cookie key*, *Wire values and bounds*, *Testing* "Sealing", "Key rotation"):

| Subtest | Steps | Expect |
|---|---|---|
| parse one key | one base64 line | `previous == nil` |
| parse two keys | two lines | both set, in order; trailing newline tolerated |
| parse errors | zero lines; three lines; a 31-byte key; not base64 | error each |
| fingerprint | a known key | matches `sha256sum` of the raw bytes, first 8 hex |
| seal opens | `seal(cookieSession, p)` then `open` | `p` |
| previous opens | seal under key A, keys become `[B, A]` | opens |
| removed key | seal under A, keys become `[B]` | does not open |
| name is AAD | seal as `cookieTxn`, open as `cookieSession` | does not open |
| tamper | flip each byte of the value in turn (nonce and ciphertext) | none opens |
| bad base64 | `open` of `!!!` | false |
| short value | fewer than 12 + 16 bytes | false |
| entropy | `rand` returning an error | `Failure{503, entropy}` |
| fixed nonce | `rand` returning 12 known bytes | the value's first 16 base64url characters are those bytes |
| session round trip | `{alice, developer, exp}` | decodes equal; `Exp` at second precision |
| trailing bytes | encoded session plus one byte | `decodeSession` false |
| truncated | encoded session minus one byte | false |
| field bound | a principal of 65536 bytes | `encode` panics or errors by design; the verifier already bounds it at 256, and the test documents the two-byte prefix |
| transaction round trip | the four strings and `exp` | decodes equal |
| set-cookie attributes | `setCookie(w, cookieSession, v, 8h)` | header is exactly `__Host-profgate_session=<v>; Path=/; Max-Age=28800; HttpOnly; Secure; SameSite=Lax` (attribute order as `net/http` writes it, asserted through `http.ParseSetCookie`) and no `Domain` |
| delete-cookie attributes | `deleteCookie(w, cookieTxn)` | empty value, `Max-Age=0`, the same four attributes, no `Domain` |
| delete session cookie | `DeleteSessionCookie(w)` | the same shape for `__Host-profgate_session` |
| value cap | a 4001-byte value | `setCookie` returns an error and writes nothing |
| staged rotation | two `sealer`s with two `fileReader`s (the pollers of the next task's key file), the five steps of *Cookie key*, a cookie sealed at every step by each replica | every cookie opens on both replicas at every step |
| one-step swap loses a session | replica A on `[new]`, replica B still on `[old]` | a cookie A sealed does not open on B; the test comment says this is why the procedure is staged |

`wire_test.go`:

| Subtest | Input | Expect |
|---|---|---|
| fixed vectors | `rand` returning bytes `0x00..0x1f` | `state` is the base64url of those bytes (43 chars); `challenge(verifier)` equals the SHA-256 vector computed in the test with `crypto/sha256` |
| alphabet | 100 random values | each is 43 characters of `[A-Za-z0-9_-]` |
| entropy | failing reader | `Failure{503, entropy}` |
| return paths | `/\evil.example`; `//evil.example`; `https://evil.example/`; `%2F%2Fevil`; `/a%5Cb`; `/a\nb`; a 2 KiB path; `javascript:alert(1)`; `` (empty) | each becomes `/` |
| query judged decoded | `/v1/x?q=%0A`; `/v1/x?q=%5C`; `/v1/x?q=a\b` (a literal backslash); `/v1/x?q=a` followed by a literal `0x01` byte; `/v1/x?q=%ZZ` | each becomes `/`: the query is percent-decoded for the check, and a query that does not decode is refused |
| return kept | `/v1/x?seconds=5#frag` | `/v1/x?seconds=5` |
| query untouched | `/v1/x?name=a%2Fb` | `/v1/x?name=a%2Fb`: the query is re-emitted byte for byte |
| path re-escaped | `/v1/a%20b` | `/v1/a%20b` from `EscapedPath` |
| dot segments | `/v1/../ops`; `/v1/./x` | `/` each |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

`seal`: `aes.NewCipher(current)`, `cipher.NewGCM`, `io.ReadFull(rand, nonce[:12])` (an error is `entropy`),
`gcm.Seal(nil, nonce, plaintext, []byte(name))`, `base64.RawURLEncoding`.
`open` decodes, splits the nonce, and tries `current` then `previous` with the same AAD.
`parseCookieKeys` splits on `\n`, drops a trailing empty line, decodes each with `base64.StdEncoding`
(a line that also decodes as URL-safe base64 is accepted through `StdEncoding` only when it is valid there;
the documentation shows `openssl rand -base64 32`, which is standard base64).
`canonicalReturn`: refuse when `raw` is empty or over 1024 bytes;
`u, err := url.Parse(raw)`, refusing an error;
require `u.Scheme == ""`, `u.Host == ""`, `u.User == nil`, `u.Opaque == ""`;
validate the decoded `u.Path` (never the raw string, so `%2F%2F`, `%5C`, and `%0A` are judged after one decoding):
it begins with `/` and not `//`, contains no `\` and no byte below `0x20`,
and `path.Clean(u.Path)` equals it apart from a trailing `/`, which rules out `.` and `..` segments;
validate the query the same way: `q, err := url.QueryUnescape(u.RawQuery)`, refusing an error,
then refusing a `\` or a byte below `0x20` anywhere in `q`;
return `u.EscapedPath()` plus `?` plus `u.RawQuery` unchanged when the query is non-empty,
so both parts are judged decoded and both travel escaped, and a `%2F` inside a value stays escaped.
Anything refused is `/`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/auth/
mise run lint && mise run test && mise run check
git add internal/auth/
git commit -m "feat(auth): seal browser cookies and rotate keys"
```

---

## Browser flow

**Files:**
- Create: `internal/auth/browser.go`, `browser_test.go`
- Modify: `internal/auth/oidc.go`, `oidc_test.go`

**Produces:**

```go
package auth

// RouteOutcome is what one /auth/ route reports for the audit line and the metrics row.
type RouteOutcome struct {
    Status    int
    Code      string // ok, auth_redirect, unauthenticated, auth_unavailable
    Reason    string // an audit reason, or "" on success
    Principal string // the resolved principal on a successful callback, "-" otherwise
}

// AuthRoutes serves /auth/login, /auth/callback, and /auth/logout.
// The caller owns the route match, the method check, readiness, Cache-Control, the audit line, and the metrics row.
type AuthRoutes interface {
    ServeAuth(w http.ResponseWriter, r *http.Request, cfg *config.Config) RouteOutcome
}

// Routes returns the /auth/ handler, or nil when the browser block is not configured.
func (o *OIDC) Routes() AuthRoutes

// browser is the relying party: it holds the sealer, the key file poller, the issuer state pointer, and the client.
type browser struct {
    clientID, redirectURL, secret string
    scopes                        []string
    sessionTTL, transactionTTL    time.Duration
    state                         *atomic.Pointer[issuerState] // OIDC's pointer; endpoints are read from it per request
    client                        *issuerClient
    verifier                      *verifier
    sealer                        *sealer
    keyFile                       *filePoller
    rand                          io.Reader
    now                           func() time.Time
    mapping                       func(cfg *config.Config) config.OIDCMapping
    log                           *slog.Logger
    rec                           metrics.Recorder
}

// isNavigation reports Sec-Fetch-Mode: navigate with Sec-Fetch-Dest: document.
func isNavigation(r *http.Request) bool
// loginRedirect is /auth/login?return=<canonical path and query of r>.
func loginRedirect(r *http.Request) string
```

`NewOIDC` builds `browser` when `cfg.Auth.OIDC.Browser` is set:
it reads `clientSecretFile` (trimmed) and the cookie key file once
(a file that cannot be read or parsed fails construction, per *Cookie key*),
and `Run` starts the key file poller beside the key refresh timer.
The poller's `apply` parses the keys, swaps the sealer's pointer,
and calls `Recorder.CookieKeys` with the fingerprints and roles.

- [ ] **Write the browser flow tests**

`browser_test.go` runs the whole `OIDC` against an `httptest` issuer that serves discovery, JWKS,
an authorization endpoint that records its query and redirects to the callback with a code,
and a token endpoint whose behavior each row programs.
Requests go through a real `httptest.Server` wrapping a minimal dispatcher
(`/auth/*` → `ServeAuth`, `/v1/*` → `Authenticate` and a `200` or the failure's status),
and a client with `net/http/cookiejar` and `CheckRedirect` returning `http.ErrUseLastResponse`
so each hop is asserted.
`rand` is fixed for the vector rows and real elsewhere.

| Subtest | Steps | Expect |
|---|---|---|
| navigation redirects | `GET /v1/…/targets?x=1` with navigation headers, no credential | `Failure{401, missing, Redirect: "/auth/login?return=%2Fv1%2F…%2Ftargets%3Fx%3D1"}` |
| fetch gets 401 | `Sec-Fetch-Mode: cors` | `Failure{401, missing}` with no redirect |
| no fetch metadata | no `Sec-Fetch-*` headers | 401 `missing`, no redirect |
| login sets txn | `GET /auth/login?return=/v1/x` | `302` to `authorization_endpoint` with `response_type=code`, `client_id`, `redirect_uri`, `scope=openid profile email`, `state`, `nonce`, `code_challenge`, `code_challenge_method=S256`; jar holds `__Host-profgate_txn` with `Max-Age=300`; outcome `{302, auth_redirect, "", "-"}` |
| login vectors | fixed `rand` | `state`, `nonce`, and `code_challenge` equal the known vectors |
| login bad return | `return=//evil.example` | the sealed return is `/` (asserted after the callback lands on `/`) |
| login entropy | failing `rand` | `503 auth_unavailable` with `Retry-After: 5`, outcome reason `entropy`; no cookie set |
| callback ok | login, then `GET /auth/callback?code=c&state=<state>` with the token endpoint answering an ID token with the sealed `nonce` | `302` to `/v1/x`; jar holds exactly `__Host-profgate_session` (`Max-Age=28800`) and no txn cookie; `AuthSessionIssued()`; outcome `{302, ok, "", <principal>}` |
| callback then request | the session cookie with `Sec-Fetch-Site: none` on `/v1/…` | `Principal{<username>, <realm>}` |
| token endpoint request | the row above | the endpoint saw `grant_type=authorization_code`, `code`, `redirect_uri`, `client_id`, `code_verifier` matching `code_challenge`, no `client_secret` |
| client secret | `clientSecretFile` set | `client_secret` sent as a form field |
| no txn cookie | callback without a jar | `401`, reason `state`; a deletion of `__Host-profgate_txn` is in the response |
| expired txn | clock moved 6m after login | `401 state`; txn deleted |
| wrong state | `state=other` | `401 state` |
| issuer error | `?error=access_denied&state=<state>` | `401 issuer_denied`; the log line carries `access_denied`; the body does not |
| exchange 400 | token endpoint answers `400` | `401 exchange_denied` |
| exchange 500 | answers `500` | `503 exchange`; outcome code `auth_unavailable`; `Retry-After: 5` |
| exchange unreachable | endpoint closed | `503 exchange`; `Retry-After: 5` |
| exchange redirect | endpoint answers `307` | `503 exchange`; `Retry-After: 5`; the redirect target saw nothing |
| no id_token | `200 {"access_token":"x"}` | `401 exchange_denied` |
| nonce mismatch | ID token with another `nonce` | `401 nonce` |
| bad id token | ID token with `aud: other` | `401 audience` |
| stale keys at callback | the key set older than `maxStale`, the fetcher still failing | `503 keys_stale`; `Retry-After: 5`; txn deleted; no session cookie |
| session entropy | `rand` fails after the exchange, while sealing the session | `503 entropy`; `Retry-After: 5`; txn deleted; no session cookie; `AuthSessionIssued` not called |
| no realm | ID token mapping to nothing | `401 no_realm` |
| callback error body | any `401` above | the standard envelope with code `unauthenticated` and `authentication required`; no redirect |
| session csrf | session cookie with `Sec-Fetch-Site: cross-site`; `same-site`; absent | `401 csrf` each |
| session admitted | `same-origin`; `none` | admitted each |
| session expired | clock moved `sessionTTL + 1s` | `Failure{401, session, ClearSession: true}`; with navigation headers `Redirect` is set |
| expired session leaves the jar | the row above through the dispatcher and the jar client | the response carries the deletion and `jar.Cookies(gatewayURL)` no longer holds `__Host-profgate_session` |
| session tampered | a byte flipped | same as expired |
| session from previous key | keys rotated to `[new, old]` after login | admitted |
| bearer beats session | valid bearer and a valid session with `Sec-Fetch-Site: cross-site` | admitted on the bearer: the cookie is not judged |
| logout with end_session | discovery published `end_session_endpoint` | `302` to it with `post_logout_redirect_uri=https://<redirectURL host>/` and `client_id`; session deleted; outcome `{302, auth_redirect, "", "-"}` |
| logout without end_session | Dex-shaped discovery | `302` to `/`; session deleted |
| logout without session | no cookie | still `302`, still a deletion header |
| logout leaves the jar | the logout rows through the jar client | `jar.Cookies(gatewayURL)` holds no session cookie afterwards |
| key rotation while running | the key file poller's `fileReader` serves `[new, old]`, then `Poll()` | `CookieKeys` called with two fingerprints, roles `current` and `previous`; a session sealed before opens after |
| key file bad | reader serves three lines | previous keys stay; `AuthFileReload("cookie_key","failed")` |
| routes without browser | `Routes()` on an `OIDC` without the block | nil |
| routes before discovery | `GET /auth/login` and `/auth/logout` on an `OIDC` whose `Discover` has not succeeded | `503 not_ready` with the standard envelope; outcome `{503, not_ready, "", "-"}`; no cookie set and no redirect |
| routes after a failed key fetch | discovery valid, JWKS answering 500, `Discover` failed | the same `503 not_ready`: a validated document alone publishes no endpoint |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

`ServeAuth` loads `st := b.state.Load()` once per request and switches on `r.URL.Path`;
a nil `st` answers `503 not_ready` (outcome code `not_ready`, no reason) before anything else,
which `httpapi`'s readiness step already prevents in production and the routes still refuse on their own.
Every endpoint below (`authorization_endpoint`, `token_endpoint`, `end_session_endpoint`) is read from `st.doc`.
`login`: `canonicalReturn(r.URL.Query().Get("return"))`, three `randomValue`s, seal the transaction,
`setCookie(cookieTxn, …, transactionTTL)`, `302` to the authorization URL built with `url.Values`.
`callback`: the seven steps of *The `/auth/` routes* in order,
each failure writing the `401` or `503` envelope itself (every `503` with `Retry-After: 5`)
(the same bytes `httpapi.writeError` produces; `internal/auth` cannot import `httpapi`,
so the envelope writer moves to a tiny shared helper or is duplicated with a test that diffs the two)
and returning the outcome;
a `401` or `503` from the callback also deletes the transaction cookie.
The token response is decoded as `map[string]json.RawMessage` and `id_token` must be a JSON string.
The ID token is verified by the same `verifier` with `tokenType: id` and then `claims.Nonce` is compared byte for byte.
`Authenticate`'s session path: cookie absent → fall through to `missing`
(with `Redirect: loginRedirect(r)` when `isNavigation`);
`open` fails or `exp <= now` → `Failure{401, session, ClearSession: true, Redirect: <same rule>}`;
`Sec-Fetch-Site` not `same-origin` or `none` → `csrf`;
else `Principal{s.Principal, s.Realm}`.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/auth/
mise run lint && mise run test && mise run check
git add internal/auth/
git commit -m "feat(auth): log a browser in through the issuer"
```

---

## HTTP API integration

**Files:**
- Modify: `internal/httpapi/server.go`, `realm.go`, `errors.go`, `audit.go`, `fixtures_test.go`, `server_test.go`, `realm_test.go`
- Create: `internal/httpapi/auth.go`, `auth_test.go`

**Produces:**

```go
package httpapi

// Deps gains:
    // Auth resolves the principal; nil means auth.Disabled{}.
    Auth auth.Authenticator
    // AuthRoutes serves /auth/*; nil means the three routes are 404 route_unknown.
    AuthRoutes auth.AuthRoutes
    // Ready is the same closure /readyz answers from: not draining, discovery synced,
    // the NATS preflight passed when PGO is enabled, and, under oidc, the issuer discovered.
    // The /v1 and /auth/ readiness steps call it; nil means Discovery.HasSynced alone.
    Ready func() bool
```

`auditRecord` gains `reason string` (emitted as `auth_reason` when non-empty) and `route string`
(emitted for the `/auth/` routes as `auth_login`, `auth_callback`, `auth_logout`).
`principalRealm` and `anonymousPrincipal` leave `realm.go`; `auth.Disabled` is where they went.

- [ ] **Write the integration tests**

`auth_test.go` uses the existing `harness` with a `fakeAuth`
whose `Authenticate` returns a programmed `Principal` or `*auth.Failure` and records the `cfg` pointer it was handed,
a `fakeRoutes` returning a programmed `RouteOutcome`,
and `Ready` set to a closure over a test variable (`ready=false` below) beside the existing `synced` switch.
Rows restate *Request algorithm*, *Failure responses*, *What is redirected*, *The `/auth/` routes*, and *Audit and metrics*.

| Subtest | Request | Expect |
|---|---|---|
| composed order: route | unauthenticated `GET /v1/bogus` | 404 `route_unknown`; `Authenticate` not called |
| composed order: method | `POST …/targets` | 405; not called |
| composed order: not ready | `ready=false` | 503 `not_ready`; not called |
| readiness is the closure | `synced=true`, `ready=false` | 503 `not_ready`: `/v1` no longer asks `HasSynced` itself |
| readiness default | `Deps.Ready == nil`, `synced=false` | 503 `not_ready`; `synced=true` serves |
| composed order: pgo disabled | `GET …/pgo` with `PGO.Enabled=false` | 501 `pgo_disabled`; not called |
| access_token first | `…/targets?access_token=x` with `fakeAuth` set to admit | 400 `invalid_parameter`; not called |
| access_token on every route | `…/profiles/heap?access_token=`, `/v1/collections/<id>?access_token=x` | 400 each |
| access_token empty value | `?access_token` | 400 |
| access_token malformed value | `?access_token=%ZZ` with a valid `Authorization: Bearer` header and `fakeAuth` set to admit | 400 `invalid_parameter`; `Authenticate` not called: `url.ParseQuery` would drop the pair, the raw scan does not |
| access_token after semicolon | `?a=1;access_token=x`, the same header | 400; not called: `;` is a separator to the scan and an error to `url.ParseQuery` |
| access_token encoded key | `?%61ccess_token=x`, the same header | 400; not called: the key is decoded before the comparison |
| access_token key case | `?Access_Token=x` on the targets route, the same header | `Authenticate` called: the comparison is case-sensitive; the parameter step then answers 400 as for any targets query |
| 401 shape | `Failure{401, bad_credential}` under `auth.mode: basic` | 401, `WWW-Authenticate: Basic realm="profgate"`, body `{"error":"authentication required","code":"unauthenticated"}`, `Cache-Control: no-store` |
| 401 bearer challenge | mode `oidc` | `WWW-Authenticate: Bearer realm="profgate"` |
| uniformity | every reason from `auth.Reasons()` whose status is 401 | identical status, headers, and body across the table |
| reasons closed set | every row above and below that writes an audit record with `auth_reason` | the value written is one of `auth.Reasons()`; `httpapi` names reasons only through `auth.Reason*` constants |
| denied namespace is 401 | `Failure{401, missing}` on a namespace the realm would deny | 401, not 403: authentication precedes the realm |
| 429 | `Failure{429, throttled}` | 429 `too_many_auth`, `Retry-After: 1`, no `WWW-Authenticate` |
| 503 | `Failure{503, keys_stale}` | 503 `auth_unavailable`, `Retry-After: 5`, no `WWW-Authenticate` |
| redirect | `Failure{401, missing, Redirect: "/auth/login?return=%2Fv1%2Fx"}` | 302, `Location` as given, empty body, `Cache-Control: no-store` |
| clear session | `Failure{401, session, ClearSession: true}` | a `Set-Cookie` deleting `__Host-profgate_session` with `Max-Age=0`; then 401 |
| clear session on redirect | same with `Redirect` | the deletion and the 302 |
| no realm | `Principal{alice, nobody}` | 401 `unauthenticated`, audit reason `no_realm` |
| admitted | `Principal{alice, developer}` | 200; audit `principal` alice |
| realm still applies | `Principal{alice, narrow}` on a denied namespace | 403 `realm_denied` |
| collection routes | `/v1/collections/<id>` with a failure | 401 before the record is read |
| snapshot | `fakeAuth` records `cfg`; the harness swaps `Config` inside `Authenticate` | the realm step used the pointer `Authenticate` received |
| non-Failure error | `Authenticate` returns `errors.New("boom")` | 503 `auth_unavailable`, `Retry-After: 5`, audit `auth_reason internal`, an error log line carrying the error; `AuthFailure(mode, "internal")` once |
| audit 401 | any 401 row | one record with `principal "-"`, `status 401`, `code unauthenticated`, `auth_reason <reason>` |
| audit 302 | the redirect row | `status 302`, `code auth_redirect`, `auth_reason missing`; no `auth_reason` key on a success record |
| metrics 401 | the 401 rows | `Recorder.AuthFailure(mode, reason)` once; `Request(endpoint, profile, "unauthenticated", _)` |
| metrics 302 | the redirect row | `AuthFailure` not called; `Request(…, "auth_redirect", _)` |
| auth routes absent | `GET /auth/login` with `AuthRoutes == nil` | 404 `route_unknown` |
| auth routes method | `POST /auth/login` | 405, `Allow: GET` |
| auth routes readiness | `ready=false` with `synced=true` | 503 `not_ready`; `ServeAuth` not called |
| auth routes dispatch | `GET /auth/callback?code=x` with `fakeRoutes` returning `{302, ok, "", "alice"}` | `ServeAuth` called once with the snapshot; audit `route auth_callback`, `principal alice`, `status 302`, `code ok`, no namespace or service keys; `Request(EndpointAuth, "none", "ok", _)` |
| auth routes login | `GET /auth/login` with `{302, auth_redirect, "", "-"}` | audit `route auth_login`, `principal "-"`, `status 302`, `code auth_redirect`, no `auth_reason` key; `AuthFailure` not called |
| auth routes logout | `GET /auth/logout` with `{302, auth_redirect, "", "-"}` | audit `route auth_logout`, `principal "-"`, `status 302`, `code auth_redirect`, no `auth_reason` key |
| auth routes failure | `{401, unauthenticated, state, "-"}` | audit `auth_reason state`, `principal "-"`; `AuthFailure(mode, state)` |
| auth routes 503 | `{503, auth_unavailable, entropy, "-"}` | audit `auth_reason entropy`; `AuthFailure(mode, entropy)`; `Request(EndpointAuth, "none", "auth_unavailable", _)` |
| auth routes no-store | every `/auth/` response | `Cache-Control: no-store` |
| auth routes not under /v1 | `GET /v1/auth/login` | 404 `route_unknown` |
| disabled default | `Deps.Auth == nil`, `auth.mode: disabled` | anonymous principal, `anonymousRealm`, as today |

`server_test.go` and `realm_test.go`: replace every use of `principalRealm` with the `Deps.Auth` default;
the existing rows keep passing, their `synced=false` rows now through the nil-`Ready` default.

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

`ServeHTTP`: before `parseRoute`, `strings.HasPrefix(r.URL.Path, "/auth/")` dispatches to `serveAuthRoute`,
which matches the three exact paths (anything else is `404 route_unknown`),
checks the method, checks `s.ready()`, then calls `AuthRoutes.ServeAuth(w, r, cfg)`
and fills the audit record and metrics from the outcome.
`s.ready()` is `deps.Ready` when set and `deps.Discovery.HasSynced` otherwise;
the `/v1` readiness step calls the same method in place of `HasSynced`,
so both paths answer `503 not_ready` under exactly the conditions `/readyz` does.
On the `/v1` path, after the PGO step:
`hasAccessToken(r.URL.RawQuery)` → `invalidParameter("access_token is not accepted as a query parameter")`,
where `hasAccessToken` never calls `url.ParseQuery`:
it splits the raw query on `&` and `;`, takes each piece up to the first `=`,
percent-decodes that key with `url.QueryUnescape` (a decode error keeps the raw key),
and compares it to `access_token` case-sensitively,
so a value the parser would drop (`%ZZ`) or a separator it would reject (`;`) still names the parameter,
and the rejection happens whatever the value holds and before any authenticator call.
The parameter step is unchanged and stays the place a malformed query is judged:
the targets and PGO routes refuse any query, and `parseProfileParams` answers `400 invalid_parameter` when `url.ParseQuery` fails;
then `p, err := s.deps.Auth.Authenticate(r.Context(), r, cfg)`;
`errors.As(err, &f)` → `q.failAuth(w, f, cfg.Auth.Mode)` in `auth.go`:
`ClearSession` → `auth.DeleteSessionCookie(w)` (defined in the *Cookie sealing* task);
`Redirect != ""` → `q.audit.reason = f.Reason`, status 302, code `auth_redirect`, `Location`, `w.WriteHeader(302)`;
else `q.audit.reason = f.Reason`, `Recorder.AuthFailure(mode, reason)`, and the envelope by status
(`401 unauthenticated` with `WWW-Authenticate: auth.Challenge(mode)`;
`429 too_many_auth` with `Retry-After: 1`;
`503 auth_unavailable` with `Retry-After: 5`).
A non-`Failure` error is a programming error:
it logs at error with the error text and is answered through the same path as `Failure{503, auth.ReasonInternal}`,
so the audit line carries `auth_reason internal`, the response `Retry-After: 5`, and the failure metric counts it.
Then `realm, ok := cfg.Realms[p.Realm]`; `!ok` → `Failure{401, no_realm}` through the same path;
`q.audit.principal = p.Name`; the realm step and everything after are unchanged.
`writeAudit` adds `auth_reason` when set and, for `/auth/` records, `route` in place of the namespace and Service keys.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "feat(httpapi): authenticate before the realm step"
```

---

## Serve lifecycle and `auth hash`

**Files:**
- Modify: `cmd/profgate/main.go`, `main_test.go`, `serve.go`, `serve_test.go`
- Create: `cmd/profgate/auth.go`, `auth_test.go`
- Modify: `internal/auth/basic.go`, `oidc.go` (the `PollInterval` option)
- Modify: `go.mod`, `go.sum`

**Produces:** `profgate auth hash`;
`serve` constructs the authenticator for `cfg.Auth.Mode`, runs `[discovering]` under `oidc`,
gates `/readyz` on it, and starts the pollers.

```go
// runAuth dispatches the "auth" subcommands; "hash" reads a password and prints its bcrypt hash at cost 12.
func runAuth(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// serveDeps gains:
    // authPoll is the users-file and cookie-key-file poll interval; zero means 30 seconds.
    authPoll time.Duration

// auth.BasicOptions and auth.OIDCOptions gain:
    // PollInterval replaces the 30-second file poll; zero keeps it. Mirrors tlscert.Options.Interval.
    PollInterval time.Duration
```

`serveDeps` gains only `authPoll`, which `serve` passes as `PollInterval`;
the serve tests reach an `httptest` issuer through `auth.oidc.caFile` holding the test server's certificate,
which is the production path.

- [ ] **Write the CLI tests**

`auth_test.go`:

| Subtest | Invocation | Expect |
|---|---|---|
| hash from pipe | `auth hash` with stdin `secret\n` | stdout is one bcrypt line at cost 12 that verifies `secret`; exit 0 |
| hash strips newline | stdin `secret\r\n` | verifies `secret` |
| empty password | stdin empty | exit 2, stderr names the problem |
| over 72 bytes | 73 bytes | exit 2: bcrypt would truncate |
| usage | `auth`; `auth other` | exit 2 with the usage line, which now lists `auth hash` |

- [ ] **Write the serve tests**

Rows extend `TestServe` with `gatewayOpts` gaining `authBlock string` (a raw YAML block replacing the disabled one)
and `writeConfig` writing it.
The `oidc` rows start an `httptest.NewTLSServer` issuer whose certificate is written to a `caFile`,
with `jwksRefreshMin: 1s` and `discoveryTimeout: 3s`.

| Subtest | Configuration | Expect |
|---|---|---|
| disabled warns | `mode: disabled` | the existing "authentication disabled" record; absent under `basic` and `oidc` |
| basic plaintext warns | `mode: basic`, `allowPlaintext: true`, no TLS | the record `basic authentication over plaintext HTTP; passwords cross the network in the clear` at warn |
| basic serves | `mode: basic` over TLS, one user | `GET …/targets` without a credential is 401 with `WWW-Authenticate: Basic realm="profgate"`; with the credential 200 |
| oidc discovering | issuer handler blocks discovery until released | `/healthz` 200 and `/readyz` 503 while blocked; the Kubernetes preflight has not run (the fake clientset's action list is empty); after release the log shows `issuer discovered` then `preflight passed`; `/readyz` 200 |
| oidc discovery timeout | issuer answers 500 forever | exit 1 within `discoveryTimeout + 2s`; log `issuer discovery failed`; the preflight never ran |
| oidc bearer | after ready, a token minted with the issuer's test key | 200; a token with an unknown `kid` after the issuer rotates is 200 within 3s |
| oidc readiness order | `pgo.enabled` with the NATS preflight seam and `oidc` | `/readyz` needs discovery, preflight, sync, and NATS all four |
| /auth/ waits for the preflight | `pgo.enabled`, the browser block, the NATS preflight seam held pending, discovery done | `GET /auth/login` and `GET /v1/…/targets` are `503 not_ready` while `/readyz` is 503; both serve once the preflight passes |
| /auth/ while draining | the browser block; `stop` closed with a request in flight | `GET /auth/login` is `503 not_ready` after the drain starts, as `/v1` is |
| users file polled | `basic` with `usersFile` in a temp directory, `authPoll: 100ms`; the file rewritten after start | the new user is admitted within 5s; the Pod-equivalent (the process) did not restart |
| cookie key fails startup | `oidc` with the browser block and an unreadable `cookieKeyFile` | `config.Load` already refuses it; the row asserts exit 2 and the key name |

- [ ] **Run the tests and watch them fail to compile**

- [ ] **Implement**

```bash
mise exec -- go get golang.org/x/term@v0.45.0
```

`runAuth`: `hash` reads the password without echo through `term.ReadPassword` when `stdin` is a terminal
(`term.IsTerminal(int(os.Stdin.Fd()))`, only when `stdin` is `os.Stdin`), else one line from `stdin`;
trims one trailing `\r\n` or `\n`; rejects empty and over-72-byte input; prints `auth.HashPassword`.
`usage` becomes `usage: profgate <version|config validate|auth hash|serve> [flags]`.

`serve`: after the logger, `switch cfg.Auth.Mode`:
`disabled` keeps the warning;
`basic` builds `auth.NewBasic` and logs the plaintext warning when `allowPlaintext` and no TLS;
`oidc` builds `auth.NewOIDC`.
Both constructors receive `PollInterval: deps.authPoll`.
A construction error ends startup with exit 1 before anything binds.
`httpapi.Deps` receives `Auth`, `Ready: ready` (the closure `/readyz` already uses),
and, for `oidc`, `AuthRoutes: o.Routes()`.
`ready()` gains `&& issuerReady.Load()` where `issuerReady` starts true unless the mode is `oidc`.
Under `oidc`, the preflight goroutine is not started at listen time;
instead `go func() { discoverCh <- discoverIssuer(runCtx, o, cfg.Auth.OIDC.DiscoveryTimeout, logger) }()` runs,
where `discoverIssuer` retries `o.Discover` with the preflight backoff until success or the timeout's context ends;
the select loop gains `case err := <-discoverCh`:
an error logs `issuer discovery failed`, runs `shutdown(drainAll)`, and returns 1;
success sets `issuerReady`, logs `issuer discovered`, starts `go o.Run(runCtx)` and the preflight goroutine.
Under `basic`, `go b.Run(runCtx)` starts at listen time.

- [ ] **Validate and commit**

```bash
mise exec -- go mod tidy && mise exec -- go test -race ./cmd/profgate/
mise run lint && mise run test && mise run check
git add cmd/profgate/ internal/auth/ go.mod go.sum
git commit -m "feat(serve): wire auth, discovery, and auth hash"
```

---

## Deployment manifests and chart

**Files:**
- Modify: `deploy/base/deployment.yaml`, `deploy/base/networkpolicy-gateway.yaml`, `deploy/deploy_test.go`
- Create: `deploy/secret-auth-example.yaml`
- Modify: `deploy/chart/profgate/values.yaml`, `templates/deployment.yaml`, `templates/configmap.yaml`,
  `templates/_helpers.tpl`, `templates/NOTES.txt`, `README.md`, `deploy/chart_test.go`

The ClusterRole, the ClusterRoleBinding, and the ServiceAccount do not change;
`TestClusterRoleTuples` and `TestChartClusterRoleMatchesBase` stay green untouched (*Permission boundary*).

- [ ] **Write the manifest tests**

| Subtest | Assertion |
|---|---|
| base volume | `TestDeployment` gains: a volume `profgate-auth` from Secret `profgate-auth`, `defaultMode: 0440`, `optional: true`, mounted read-only at `/etc/profgate/auth/`; `fsGroup` unchanged |
| secret example | `TestSecretExamplesAreCommented` gains `{secret-auth-example.yaml, profgate-auth}`; the file also names `users.yaml`, `cookie.key`, `issuer-ca.crt`, and `client-secret` |
| egress comment | `TestGatewayNetworkPolicy` gains: the live policy still has `policyTypes: [Ingress]` only; the file contains a commented `Egress` block naming the issuer, asserted as text |
| egress comment is complete | the commented block, in the base file and in `values.yaml`, names DNS, the Kubernetes API, the Pod pprof ports, NATS, and the issuer, asserted as text |
| chart off by default | `TestChartAuth`: no `auth` volume; rendered `auth.mode` is `disabled` with `anonymousRealm` |
| chart basic | values `auth.mode: basic`, `auth.basic.users`, `auth.secret.enabled: true` → the rendered config has the `basic` block with `usersFile: /etc/profgate/auth/users.yaml`, no `anonymousRealm`; the `auth` volume is a Secret `profgate-auth`, `0440`, read-only, not optional |
| chart oidc | `auth.mode: oidc` with issuer, audience, mapping, and `auth.secret.enabled: true` → the `oidc` block renders; `caFile` rendered only when `auth.oidc.caKey` is set |
| chart browser | `auth.oidc.browser` values → the browser block with `cookieKeyFile: <mountPath>/cookie.key`, and rendering fails when `tls.enabled` is false |
| chart secret required | `auth.mode: basic` with `auth.basic.usersFile` set and `auth.secret.enabled: false` → rendering fails |
| chart env guards | `TestChartGuardedEnvNamesMatchTheBinary` gains `PROFGATE_AUTH_BASIC_USERS_FILE`, `PROFGATE_AUTH_OIDC_CA_FILE`, `PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE`, `PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE` |
| chart config guards | `TestChartRejectsDerivedKeyOverrides` gains the four path keys under `config.auth` |
| chart rendered config loads | `TestChartConfigIsMergedAndParses` covers the basic and oidc value sets through `config.Load` with the mounted files stubbed |
| chart egress | `networkPolicy.enabled: true` with the default values renders `policyTypes: [Ingress]` and no `egress` key; no values key turns egress on |
| chart secret not hashed | like TLS: no `checksum/auth` annotation |
| notes disabled | `renderNotes(t)` with the defaults | contains "Authentication is disabled" and the `anonymousRealm` sentence, as today; contains neither `auth hash` nor `/auth/login` |
| notes basic | `renderNotes(t, "--set", "auth.mode=basic", "--set", "auth.secret.enabled=true", …)` | contains the `kubectl create secret generic profgate-auth --from-file=users.yaml` line, the mount path, and `profgate auth hash`; does not contain "Authentication is disabled" or `anonymousRealm` |
| notes oidc | `renderNotes(t, "--set", "auth.mode=oidc", "--set", "auth.oidc.issuer=https://issuer.example", …)` with and without the browser block | contains the issuer URL; with the browser block also `<redirectURL host>/auth/login`; without it, no `/auth/login`; never "Authentication is disabled" |

- [ ] **Run the tests and watch them fail**

- [ ] **Implement**

`secret-auth-example.yaml` is fully commented, like the other two,
and shows `kubectl create secret generic profgate-auth --from-file=users.yaml --from-file=cookie.key --from-file=issuer-ca.crt --from-file=client-secret`,
the `openssl rand -base64 32` line for a key, the two-key rotation shape, and the mount paths each configuration key points at.
The base Deployment mounts the Secret optionally, as the NATS credentials are mounted;
the comment says the gateway refuses to start when a configured file is missing,
so the volume being optional only lets the base apply first.
The base NetworkPolicy file and the chart's `values.yaml` each gain a commented example only:
an `Egress` policy type with rules for the issuer,
and one sentence saying that a policy selecting the gateway Pods for egress must also allow DNS,
the Kubernetes API, the Pod pprof ports, and NATS when PGO is enabled, or the gateway stops working.
No rendered egress rule exists (the chart's NetworkPolicy template is unchanged):
once a policy selects the Pods for egress, an issuer-only allowance would isolate every other destination.
The chart: `auth.mode`, `auth.anonymousRealm`, `auth.basic.{users,usersFile,allowPlaintext,maxConcurrent}`,
`auth.oidc.{issuer,audience,tokenType,usernameClaim,groupsClaim,caKey,httpProxy,mapping,browser}`,
`auth.secret.{enabled,existingSecret,mountPath}` with `existingSecret: profgate-auth` and `mountPath: /etc/profgate/auth`;
`profgate.authSecretEnabled` in `_helpers.tpl`; the config template renders only the block the mode selects
and derives every file path from the mount path, refusing a values file that names a file with the mount off.
`README.md` gains an *Authentication* section beside *HTTPS on the API port*.
`templates/NOTES.txt`'s second item branches on `.Values.auth.mode`:
`disabled` keeps the current sentence (authentication is disabled, every request is granted `auth.anonymousRealm`);
`basic` says every request needs a user from the list,
shows `kubectl -n <ns> create secret generic <existingSecret> --from-file=users.yaml` when `usersFile` is set,
names the mount path, and shows `profgate auth hash` as the way to produce a `passwordHash`;
`oidc` names the issuer, says a request needs a bearer token it issued for the audience,
and, when the browser block is set, prints the login URL as `https://<redirectURL host>/auth/login`.
The realm listing that follows stays in every mode.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./deploy/
mise run lint && mise run test && mise run check
git add deploy/
git commit -m "feat(deploy): mount the authentication Secret"
```

---

## End-to-end scenarios

**Files:**
- Create: `test/e2e/dex.yaml`, `test/e2e/scenarios_auth_test.go`,
  `test/e2e/overlays/oidc-gateway/{kustomization,serviceaccount,clusterrolebinding,configmap,deployment}.yaml`,
  `test/e2e/overlays/basic-gateway/…` (the same five files)
- Modify: `test/e2e/registry.go`, `lanes_test.go`, `harness_test.go`, `scenarios_tls_test.go`

The spec's *Testing* calls these two "lanes"; in this repository a lane is a Kubernetes version
and the unit of a proof is a scenario, so they are two scenarios that run on every lane
(*Cluster matrix* in the gateway spec).
Both need a reachable Pod, so a degraded lane skips them.

- [ ] **Register the scenarios**

`registry.go` gains `{Name: "auth-oidc-browser", NeedsPodReach: true}` and `{Name: "auth-basic", NeedsPodReach: true}`;
`lanes_test.go`'s registry rows follow; `runners()` maps them to `scenarioAuthOIDCBrowser` and `scenarioAuthBasic`.

- [ ] **Write the harness pieces**

- `loadImage(ctx, image)` generalizes `loadNATSImage` (which becomes a call to it)
  and loads `ghcr.io/dexidp/dex:v2.45.1` in `TestMain`;
  a `dexImage` constant beside `natsImage`.
- `newAuthority(t, hosts ...string)` takes the names the leaf certifies:
  a host that `net.ParseIP` accepts goes into `IPAddresses`, any other into `DNSNames`;
  `scenarios_tls_test.go` passes `tlsHost`, the basic scenario passes `tlsHost` and `"127.0.0.1"`,
  and Dex gets `dexHost = "dex"`.
- `deployDex(ctx, ns, ca authority, clientRedirect string) (*dexServer, error)`:
  a ConfigMap with Dex's static configuration
  (`issuer: https://dex:5556`, `storage: {type: memory}`, `web: {https: 0.0.0.0:5556, tlsCert, tlsKey}`,
  `oauth2: {skipApprovalScreen: true, passwordConnector: local}`,
  `staticClients: [{id: profgate, public: true, redirectURIs: [<clientRedirect>]}]`,
  `enablePasswordDB: true`, `staticPasswords: [{email: alice@example.com, hash: <bcrypt>, username: alice}]`),
  the TLS Secret from `ca`, `test/e2e/dex.yaml` (Deployment + Service `dex` on 5556), a rollout wait,
  and a port-forward; `restart(ctx)` runs `kubectl rollout restart` and re-forwards, which is how the signing key rotates
  (memory storage mints a new key on every start).
- `applyAuthSecret(ctx, ns, files map[string][]byte)` creates or replaces the `profgate-auth` Secret.
- `gatewayConfigOptions` gains `AuthBlock string`, written in place of the `disabled` block when set,
  and `IssuerCAMount string` used by the oidc block.
- `authClient(gwLocal, dexLocal string, pool *x509.CertPool) *http.Client`:
  a cookie jar, `CheckRedirect` returning `http.ErrUseLastResponse`, keep-alives off,
  and a `DialContext` that maps `gateway:443` to the gateway forward and `dex:5556` to the Dex forward,
  so the browser walk follows real `Location` headers with real hostnames.

- [ ] **Write the scenarios**

`scenarioAuthOIDCBrowser` (*Testing*, end to end):

| Step | Action | Assert |
|---|---|---|
| deploy | test app; Dex behind its own authority; the `oidc-gateway` overlay over TLS with `issuer: https://dex:5556`, `caFile: /etc/profgate/auth/issuer-ca.crt`, `usernameClaim: email`, `mapping.users: [{alice@example.com → developer}]`, the browser block with `redirectURL: https://gateway/auth/callback`, `cookieKeyFile`, `jwksRefreshMin: 1s` | the gateway's `/readyz` turns 200 only after its log shows `issuer discovered` |
| curl gets 401 | `GET …/profiles/heap` without headers | 401, `WWW-Authenticate: Bearer realm="profgate"` |
| navigation | the same with `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document` | 302 to `/auth/login?return=…` |
| login | follow it | 302 to `https://dex:5556/auth?…`; the jar holds `__Host-profgate_txn` |
| dex | follow to Dex; Dex answers its password form (a 200 with a form, or a 302 to `/auth/local?…` first); `POST` the form with `login=alice@example.com`, `password` | 303 to `/approval?…` or straight to `https://gateway/auth/callback?code=…&state=…` (follow `/approval` when Dex returns it) |
| callback | follow to the gateway | 302 to the original path and query; the jar holds exactly `__Host-profgate_session` and no txn cookie |
| profile with session | `GET …/profiles/heap` with the jar and `Sec-Fetch-Site: none` | 200, the body parses as a profile (`profile.Parse`) |
| csrf | the same with `Sec-Fetch-Site: cross-site` | 401 |
| bearer | `POST https://dex:5556/token` with `grant_type=password`, `client_id=profgate`, `username`, `password`, `scope=openid email` through the Dex forward; take `id_token` | `GET …/profiles/heap` with `Authorization: Bearer` is 200 |
| query token refused | `…/profiles/heap?access_token=<id_token>` with the bearer header too | 400 `invalid_parameter` |
| rotation | `dex.restart`; obtain a new `id_token` (signed with the new key) | the first bearer request with it may be 401 while the fetch is in cooldown; polled for up to 15s it becomes 200; the gateway Pod's UID and restart count are unchanged (`podState`) |
| logout | `GET /auth/logout` with the jar | 302 to `/` (Dex publishes no `end_session_endpoint`); the jar holds no session cookie |
| audit | the gateway log | a record with `code auth_redirect`, one with `auth_reason csrf`, and the successful callback with `principal alice@example.com` and `route auth_callback` |

`scenarioAuthBasic`:

| Step | Action | Assert |
|---|---|---|
| deploy | the `basic-gateway` overlay over TLS with one inline user (`alice`, a cost-10 hash the test mints) and a `usersFile` naming `bob` through the Secret | `/readyz` 200 |
| no credential | `GET …/targets` | 401, `WWW-Authenticate: Basic realm="profgate"` |
| wrong password | `curl -u alice:wrong` shape through the client | 401 |
| inline user | `alice` with the right password | 200 |
| file user | `bob` | 200 |
| go tool pprof | `exec.Command("go", "tool", "pprof", "-proto", "-output", file, "https://alice:PASSWORD@127.0.0.1:<port>/v1/…/profiles/heap")` against the gateway forward, with `SSL_CERT_FILE` pointing at the authority's PEM and `HTTPS_PROXY` unset; the leaf carries `127.0.0.1` in `IPAddresses`, so no hostname mapping is needed | exit 0; `file` parses as a profile |
| users file rotation | replace the Secret with a file naming `carol` | `carol` is 200 within `rotationDeadline`; the Pod is not replaced (`podState`) |

Keycloak is not run here; the next task verifies it by hand.

- [ ] **Run the suite on the current lane**

```bash
PROFGATE_E2E_LANE=current mise run test:e2e
```

- [ ] **Validate and commit**

```bash
mise exec -- go vet -tags e2e ./test/e2e/... && mise exec -- go mod tidy
mise run lint && mise run test && mise run check
git add test/e2e/ go.mod go.sum
git commit -m "test(e2e): prove basic and oidc against Dex"
```

---

## Verify against Keycloak

**Files:**
- Create: `docs/keycloak-realm.json`
- Modify: `docs/specs/auth.md` (the *Issuer notes* sentence that says Keycloak is verified during implementation)

The spec's *Issuer notes* describe Keycloak from its documentation and require a run against a real instance;
this task is that run, done by hand, and it leaves two things behind:
the realm export the run imported, and the spec sentence turned into a fact.
The export lives in `docs/` because that is where the spec says the issuer notes live,
and `docs/authentication.md` (the *Documentation* task) links to it.

- [ ] **Build the realm export**

`docs/keycloak-realm.json` is a Keycloak realm export (`kc.sh export --realm profgate`) holding:
realm `profgate`;
a public client `profgate` with PKCE `S256`, `redirectUris: [https://localhost:9443/auth/callback]`,
and direct access grants on (so `grant_type=password` yields tokens for the bearer checks);
a client scope with an audience mapper adding `profgate`,
and a "Group Membership" mapper on claim `groups` with "Full group path" off;
groups `engineering` and `payments`; user `alice` (password `secret`) in `engineering`.

- [ ] **Run the checklist**

Each line is done when the observed response matches; record the Keycloak version from its admin console.

| Step | Command or action | Expect |
|---|---|---|
| start | `docker run --rm -p 8443:8443 -v $PWD/docs/keycloak-realm.json:/opt/keycloak/data/import/realm.json -v <cert dir>:/certs quay.io/keycloak/keycloak:<version> start-dev --import-realm --https-certificate-file=/certs/tls.crt --https-certificate-key-file=/certs/tls.key` with a self-signed certificate for `localhost` | `https://localhost:8443/realms/profgate/.well-known/openid-configuration` answers, `issuer` is `https://localhost:8443/realms/profgate` |
| gateway | `profgate serve` on the workstation against any cluster the kubeconfig names (the e2e kind cluster works), listening with TLS on `:9443`, `mode: oidc`, `issuer` as above, `audience: profgate`, `caFile` naming the self-signed certificate, `usernameClaim: preferred_username`, `mapping.groups: [{engineering → developer}]`, and the browser block with `redirectURL: https://localhost:9443/auth/callback` | `/readyz` 200 after `issuer discovered` |
| id token as bearer | `curl -d grant_type=password -d client_id=profgate -d username=alice -d password=secret -d scope=openid` to the token endpoint; take `id_token` | `GET /v1/…/targets` with it is 200; the audit line has `principal alice` |
| access token as bearer under id | the `access_token` from the same response under `tokenType: id` | 401 (its `aud` is `account`, not `profgate`, unless the audience mapper applies to it) — record which |
| access token under access | restart with `tokenType: access` (no browser block) | `typ` is `JWT` by default → 401 `token_type`; after enabling the client's RFC 9068 token type it is `at+jwt` → 200 — record both |
| groups shape | decode the ID token | `groups` is `["engineering"]` with the full-path option off and `["/engineering"]` with it on; the spec's note holds |
| browser login | walk `/auth/login` in a browser | Keycloak's form, then the callback sets the session cookie and `/v1/…/targets` renders |
| logout | `/auth/logout` | 302 to Keycloak's `end_session_endpoint` with `post_logout_redirect_uri` and `client_id`; Keycloak returns to `/` |
| lifetimes | inspect the realm's token settings | ID and access tokens 5 minutes, SSO session 30 minutes idle and 10 hours maximum, as the spec's note says |

- [ ] **Record the result**

Edit the spec's *Issuer notes*:
the sentence saying Keycloak is verified during implementation becomes
"Keycloak was verified against Keycloak `<version>` with the realm export in `docs/keycloak-realm.json`",
and any Keycloak bullet the run proved wrong is corrected in the same edit.
Nothing else in the spec moves.

- [ ] **Validate and commit**

```bash
semlf check docs/specs/auth.md
mise run lint && mise run test && mise run check
git add docs/keycloak-realm.json docs/specs/auth.md
git commit -m "docs(spec): record the Keycloak verification"
```

---

## Documentation

**Files:**
- Modify: `docs/api.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/README.md`, `CHANGELOG.md`,
  `deploy/chart/profgate/README.md` (if the deployment task left anything), `.agents/rules/100-project-map.md`
- Create: `docs/authentication.md`

- [ ] **Update the guides**

| File | Change |
|---|---|
| `docs/api.md` | *How a request is processed* gains steps 5 (credential placement) and 6 (authentication) with the renumbered tail; a new *Authentication* section: the `basic` `curl -u` and userinfo forms, the `Bearer` form, `WWW-Authenticate`, the browser flow from a user's view, the three `/auth/` routes; the *Errors* table gains `400 invalid_parameter` for `access_token`, `401 unauthenticated`, `429 too_many_auth`, `503 auth_unavailable` |
| `docs/configuration.md` | the *`auth`* section becomes the full table of the spec's *Configuration*, with subsections for `auth.basic`, `auth.oidc`, `auth.oidc.mapping`, `auth.oidc.browser`, the restart-versus-hot column, the users file shape, the cookie key file shape, and `profgate auth hash`; *Cross-Key Validation* gains the auth rules; *Examples* gain a `basic` and an `oidc` file |
| `docs/deployment.md` | a section *Authentication secrets* beside *TLS on the API listener*: the `profgate-auth` Secret, the chart values, the commented egress example and the sentence about what else such a policy must allow, the staged cookie key rotation with the `profgate_auth_cookie_key_info` check; *Probes and readiness* notes the `[discovering]` state; *Metrics* lists the seven series; *Audit log* shows `auth_reason` and an `/auth/` record |
| `docs/authentication.md` | the issuer notes of the spec's *Issuer notes*, one subsection per issuer, with a working Dex configuration (the one the suite uses), the Keycloak client settings as the *Verify against Keycloak* task recorded them, with the version and a link to `docs/keycloak-realm.json`, and Okta, Entra ID, and Google marked unverified; the command-line client non-goal and the session revocation limit stated plainly |
| `docs/README.md` | the guide list gains *Authenticate users*: `authentication.md`; the `plans/` sentence names three plans |
| `CHANGELOG.md` | an `## [Unreleased]` section above `0.3.0` with *Added* entries for the two modes, the browser flow, `auth hash`, the metrics, the Secret mount, and the chart values, and a *Changed* entry for the request algorithm's new steps and the `401` that now precedes `403` |
| `.agents/rules/100-project-map.md` | confirm `internal/auth/` and the three `/auth/` routes are already listed (they are); add `docs/authentication.md` nowhere — the map does not list guides |
| `deploy/chart/profgate/README.md` | the *Values* table rows for the `auth` block |

- [ ] **Validate and commit**

```bash
semlf check docs/api.md docs/configuration.md docs/deployment.md docs/authentication.md docs/README.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add docs/ CHANGELOG.md deploy/chart/profgate/README.md .agents/rules/
git commit -m "docs: describe the authentication modes"
```

---

## Finish the plan

- [ ] Confirm the `main` run passed every lane (the existing workflows need no change:
  `check.yml` covers the new unit tests and `e2e.yml` the lanes).
- [ ] Decide whether `internal/auth` changes should trigger the end-to-end suite before a PR
  and record the decision in `.agents/rules/500-validation-and-workflow.md` either way.
- [ ] In the same change: set line 3 of this file to `**Status:** Done` and add line 4
  `**Outcome:** <tag or commit that shipped authentication>`.
- [ ] `mise run lint && mise run test && mise run check`;
  `git add docs/plans/auth.md .agents/rules/`; `git commit -m "docs: mark the authentication plan done"`.

---

## Self-Review

- Spec coverage: request algorithm and failure responses (*HTTP API integration*);
  `basic` credential, users, transport, clients (*Configuration*, *Auth package core and `basic` mode*, *Serve lifecycle*, `auth-basic`);
  issuer client, discovery, token verification, signing keys (*Issuer client…*, *Token verification…*);
  principal to realm (*Token verification…*);
  browser flow: configuration, redirection, cookie key, wire values, routes, session (*Configuration*, *Cookie sealing…*, *Browser flow*);
  audit and metrics (*Metrics recorder*, *HTTP API integration*);
  issuer notes (*Documentation*); configuration table (*Configuration*);
  testing (every task names its slice; the two end-to-end proofs are `auth-oidc-browser` and `auth-basic`;
  the Keycloak run is *Verify against Keycloak*);
  dependencies (the three `go get` lines, `check_auth_importers`, and `check_term_importers`);
  package layout (`internal/auth` as listed);
  the amendments the spec's *Changes to the accepted gateway design* lists are already in the gateway spec and the rules,
  and the deployment ones land in *Deployment manifests and chart*.
- Types: `auth.Authenticator`, `auth.Principal`, `auth.Failure`, `auth.Disabled`, `auth.Basic`, `auth.OIDC`,
  `auth.AuthRoutes`, `auth.RouteOutcome`, `config.AuthConfig`, `config.BasicConfig`, `config.BasicUser`,
  `config.OIDCConfig`, `config.OIDCMapping`, `config.OIDCBrowser`, `metrics.CookieKey`,
  and the unexported `filePoller`, `issuerClient`, `discoveryDocument`, `keySet`, `jwksCache`, `verifier`, `claims`,
  `sealer`, `session`, `transaction`, `browser` are each defined once, in the task that first needs them,
  and consumed by those names afterwards.
- Task order compiles at every step:
  config → metrics → auth core and basic → issuer and keys → verification and oidc → cookies → browser → httpapi → cmd → deploy → e2e → Keycloak → docs;
  no task imports a package a later task creates, and no package imports one that imports it back:
  `internal/config` and `internal/metrics` import neither each other nor `internal/auth`
  (the metrics task pre-registers no reason label, which is what keeps `metrics → auth → metrics` out),
  `internal/auth` imports `config` and `metrics`,
  `internal/httpapi` imports `auth`,
  `cmd/profgate` imports all four,
  `deploy/` tests import `config` alone,
  and the end-to-end tree imports nothing under `internal/`;
  every fake `Recorder` is extended in the metrics task before any producer calls the new methods.
- Current source and the spec's `Reload` column:
  the spec marks users, mappings, and `anonymousRealm` `hot`,
  and no configuration reloader exists
  (`cmd/profgate/serve.go:111-112` stores the pointer once and nothing signals a reload;
  the chart rolls the Pods on a configuration change under `configChecksumAnnotation`).
  That is not a gap: the gateway spec's *Non-goals* defer hot reload to a later revision,
  and the auth spec's *Configuration* says `hot` names what such a reloader may swap,
  because every request reads one snapshot.
  The plan honors *Configuration snapshot* through the pointer every request loads,
  makes the users file and the cookie key file the only runtime re-reads,
  and adds no reload task.
  `cmd/profgate/serve.go:110` logs "authentication disabled" unconditionally today;
  the *Serve lifecycle* task makes it mode-dependent.
  `internal/config/config.go:94-95` pins `mode` to `disabled` and marks `anonymousRealm` `required`;
  the *Configuration* task replaces both.
- Decided here because the spec leaves them to the implementer, recorded so nobody mistakes them for omissions:
  `Failure` carries a fourth field, `ClearSession`, because `Authenticate` has no `ResponseWriter`
  and the spec's session rule deletes the cookie before treating the request as credential-less;
  the `/auth/` handler contract is `AuthRoutes.ServeAuth` returning a `RouteOutcome`,
  so routing, the method check, readiness, `Cache-Control`, the audit line, and the metrics row stay in `httpapi`;
  the `/auth/` routes record under a new `metrics.EndpointAuth` route family with profile `none`;
  a non-`Failure` error from `Authenticate` is a programming error:
  logged at error and answered as `503 auth_unavailable` with audit reason `internal`,
  which the spec's reason table carries;
  a principal whose realm the snapshot lacks is `401 no_realm`
  (fail closed, as the current `principalRealm` comment says);
  the users file is validated at poll time against the snapshot the poller holds,
  the inline users and realm names captured from the startup configuration;
  `httpapi.Deps.Ready` is the closure `/readyz` uses, so `/v1` and `/auth/` readiness cannot drift from it;
  realm names are DNS-1123 labels, which is what bounds the session cookie;
  the cookie key file is standard base64, one key per line;
  `profgate_oidc_jwks_age_seconds` is a `GaugeFunc` over the last fetch timestamp and `NaN` before the first fetch;
  the chart renders no egress rule and ships only a commented example,
  because a selecting egress policy must allow far more than the issuer;
  `auth hash` reads without echo only when stdin is a terminal, so pipes and tests work;
  the bcrypt hash check in `internal/config` is the hash grammar,
  because `x/crypto` may only be imported by `internal/auth`;
  `x/term` is imported only by `cmd/profgate`, checked the same way;
  the base Deployment mounts the auth Secret optionally, as it mounts the NATS credentials;
  Dex in the suite uses memory storage so a restart is a key rotation;
  the PKCE verifier, `state`, and `nonce` share one 32-byte generator.
- Left to the implementer by design: helper names inside test files, the test key fixtures' exact construction,
  the shape of the Dex password form walk
  (Dex's HTML changes between releases; the scenario follows `Location` headers and posts the form it is given),
  and whether `writeError`'s envelope helper moves to a shared package or is duplicated with a diff test
  (the *Browser flow* task allows either).
