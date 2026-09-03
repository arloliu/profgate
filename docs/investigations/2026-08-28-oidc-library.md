# OIDC transport from a library

Date: 2026-08-28

**Finding: replacing the hand-written OIDC transport with a library is not justified.
`docs/specs/auth.md` stands unchanged.**

Library examined: `github.com/coreos/go-oidc/v3` **v3.20.0**,
read from the module cache at
`$(go env GOMODCACHE)/github.com/coreos/go-oidc/v3@v3.20.0/oidc/`
(`doc.go` 65, `jose.go` 40, `jwks.go` 316, `logout.go` 188, `oidc.go` 687, `verify.go` 340 — 1,636 non-test lines).
Also examined: `github.com/go-jose/go-jose/v4/jwt` at the version already in `go.mod` (v4.1.4),
and `github.com/zitadel/oidc/v3` v3.49.3 at the dependency-graph level only.

Repository code under evaluation:
`internal/auth/discovery.go` (94), `jwks.go` (290), `issuer.go` (222), `verify.go` (321) — 927 lines,
plus `oidc.go` (170) which wires them; 1,097 lines together.

---

## 1. The spec's rejection still holds at v3.20.0

`docs/specs/auth.md` rejects `github.com/coreos/go-oidc/v3` in four clauses under *Dependencies*.
All four are true of v3.20.0, verified against source rather than recalled:

| Spec clause | Source | Verdict |
|---|---|---|
| "refreshes on every signature failure with no cooldown" | `oidc/jwks.go:176-187` — a cache miss falls straight through to `keysFromRemote` | **True, with a refinement below** |
| "no periodic refresh" | no `time.Ticker`, `time.NewTimer`, or `NewTicker` anywhere in the package | True |
| "tries every held key for a token without `kid`" | `oidc/jwks.go:165` and `:182` — `if keyID == "" \|\| key.KeyID == keyID` | True |
| "applies a fixed skew to `nbf` and none to `exp`" | `oidc/verify.go:269` (`t.Expiry.Before(nowTime)`), `:278` (`leeway := 5 * time.Minute`, not configurable) | True |
| "reads issuer responses without a size limit" | `oidc/jwks.go:301`, `oidc/oidc.go` `NewProvider`, `oidc/verify.go:163` — three unbounded `io.ReadAll` | True |

Two sharpenings the spec's own wording undersells:

- **"No cooldown" is imprecise in the library's favour and in its disfavour.**
  `keysFromRemote` (`oidc/jwks.go:199-237`) *does* coalesce **concurrent** fetches through its `inflight` singleflight.
  What is absent is any cooldown across **sequential** requests:
  an attacker sending bogus-`kid` tokens one at a time drives one issuer fetch per token, indefinitely.
  That is the threat the repository's `refreshOnDemand` cooldown (`internal/auth/jwks.go:161-174`) exists to bound,
  and it is the sharper claim.
- **`iat` is not checked at all**, not merely unskewed.
  `oidc/verify.go:228` copies `iat` into `IDToken.IssuedAt` and never compares it.
  The repository requires `iat` present and `iat > now+skew` to be `ReasonExpired` (`internal/auth/verify.go:202-205`).

One gap the spec does not mention, which matters for adoption cost:
**`end_session_endpoint` is not in go-oidc's discovery struct.**
`providerJSON` (`oidc/oidc.go`) parses `issuer`, `authorization_endpoint`, `token_endpoint`,
`device_authorization_endpoint`, `jwks_uri`, `userinfo_endpoint`, and `id_token_signing_alg_values_supported`.
`end_session_endpoint` appears only inside a doc comment (`oidc/oidc.go:126`).
The browser flow's logout needs it, so a hand-written discovery fetch survives adoption regardless —
which means `issuerClient.getJSON` is not deleted either.

---

## 2. Per-feature table

| Feature the spec requires | go-oidc v3.20.0 |
|---|---|
| Discovery fetch, issuer equality byte for byte | **Provides**, `NewProvider` + `IssuerMismatchError` |
| Issuer must be `https` | **Lacks** — no scheme check on the configured issuer |
| Discovered endpoints validated: absolute `https`, host present, no userinfo, no fragment | **Lacks** — endpoints are stored as received |
| `end_session_endpoint` recovered from discovery | **Lacks** — not a field of `providerJSON` |
| Exactly one JSON value in the response, bytes after it refused | **Lacks** — plain `json.Unmarshal` on the whole body |
| Response body bounded (1 MiB) | **Lacks** — unbounded `io.ReadAll` in three places |
| JWKS cache, timer refresh every `jwksRefresh` | **Lacks** — no timer exists in the package |
| JWKS on-demand refresh with a `jwksRefreshMin` cooldown shared across requests | **Differently** — refetches on every miss; concurrent misses coalesce, sequential ones do not |
| `jwksMaxStale` fail-closed (`503 auth_unavailable` past the bound) | **Lacks** — a set fetched once is trusted forever |
| A fetch with no usable key is a *failed* fetch that replaces nothing | **Lacks** — an empty `keys` array is stored as the cache |
| Duplicate `kid` rejects the whole set | **Lacks** — both keys are kept and both are tried |
| Minimum RSA modulus 2048 bits | **Lacks** |
| `use` must be `sig` or absent | **Lacks** |
| `alg` ↔ curve binding (`ES256`/P-256, `ES384`/P-384, `ES512`/P-521; `RS*`/`PS*` need RSA) | **Lacks** — only a key-level `alg`-is-supported filter (`oidc/jwks.go:244-286`) |
| A token without `kid` verifies only when exactly one compatible key is held | **Contradicts** — tries every held key (`oidc/jwks.go:165`) |
| `EdDSA` excluded from the allowed algorithms | **Differently** — `allAlgs` includes `EdDSA` (`oidc/jose.go:31`); excludable via `Config.SupportedSigningAlgs` |
| Exactly one `alg` member in the protected header, string-typed | **Lacks** — go-jose's parser keeps the last of two; the repo re-walks the header (`internal/auth/verify.go:126-165`) |
| Redirect limit of 3 on GET, none on the token POST | **Neither** — a property of the injected `http.Client`; kept by the caller either way |
| TLS pinning of the issuer client (`caFile`, explicit proxy, per-step timeouts) | **Neither** — injectable through `oidc.ClientContext`; the transport code is the caller's either way |
| Configurable clock skew on `exp`, `iat`, `nbf` | **Lacks** — `exp` unskewed, `nbf` fixed 5 min, `iat` unchecked |
| `exp` and `iat` required present | **Lacks** — both are `omitempty` pointers; absent means "skip" |
| Numeric dates rejected outside `int64` | **Lacks** — see section 4 |
| Audience membership | **Provides** (`oidc/verify.go:251-259`) |
| `azp` required when an ID token carries several audiences | **Lacks** — the code comments say so explicitly at `oidc/verify.go:250` |
| RFC 9068 `typ: at+jwt` when `tokenType: access` | **Lacks** |
| Configurable username and groups claim names, with shape and length bounds | **Lacks** — fixed `IDToken` fields; custom claims via `token.Claims(&v)` only |
| Identity claims bounded to 256 bytes, non-empty, no NUL | **Lacks** |
| `sub` required | **Lacks** as a check |
| Per-refresh metrics (`JWKSRefresh`, `JWKSKeys`, `JWKSFetched`) | **Lacks** |

Three rows read "Neither" on purpose:
TLS pinning and the redirect limits are properties of the `http.Client` the caller hands to `oidc.ClientContext`,
so `newIssuerTransport` and both `http.Client` values in `internal/auth/issuer.go` survive adoption untouched.
They are not an argument either way.

---

## 3. Line accounting

Function by function, if go-oidc replaced everything it plausibly could.

**Deleted: nothing.**

The only line go-oidc could plausibly take over is `discover`'s fetch and its issuer-equality check
(`discovery.go:34-41`), through `NewProvider`.
It cannot: `end_session_endpoint` is not a field of `providerJSON`,
so the browser flow's logout endpoint has to be read from a hand-written fetch of the same document,
and fetching that document twice to let a library re-check an equality the repository already checks is not a saving.
Every other candidate is ruled out by a row of section 2.

**Kept — all 927 lines, unchanged**

| Code | Lines | Why |
|---|---|---|
| all of `discovery.go` — `discoveryDocument`, `discover`, `validateEndpoint` | 94 | the document is still fetched by hand for `end_session_endpoint`, and the `https`/host/userinfo/fragment rules have no counterpart |
| `jwks.go` — `allowedAlgs` and helpers, `keyFetcher`, `issuerState`, `keySet`, `jwksCache` | 200 | the six cache behaviours in section 2 that `RemoteKeySet` lacks: timer, cooldown, `maxStale`, empty-set-is-a-failure, atomic swap, metrics |
| `jwks.go` — `usableKeys`, `unusable`, `compatible`, `curveFor` | 90 | the key hardening every proposal to adopt a library agrees stays |
| all of `issuer.go` | 222 | transport and CA pinning, redirect policy, body bound, `decodeOne`, `postForm` — none provided, and the two clients are what a library would be handed |
| all of `verify.go` | 321 | go-oidc covers `iss` equality and `aud` membership (≈8 of these lines) and would have to be configured with `SkipExpiryCheck` to avoid its own unskewed `exp` and fixed `nbf` leeway |
| **Total kept** | **927** | |

**Newly written glue**

| Glue | Lines |
|---|---|
| A `http.RoundTripper` wrapper restoring the 1 MiB body bound over go-oidc's unbounded `io.ReadAll` | ~25 |
| A `oidc.KeySet` implementation bridging the repository's `keySet` to go-oidc, or a second parse to recover `singleAlg` and `selectKey` — go-oidc parses internally and never surfaces which key verified | ~35 |
| `ClientContext` plumbing and `Provider` → `discoveryDocument` translation | ~20 |
| **Total new** | **~80** |

**Net: zero lines deleted, roughly +80 written, and one new module.**
The replacement does not shrink the package; it grows it.

The dependency delta is honestly small.
In a scratch module, `go get github.com/coreos/go-oidc/v3/oidc@v3.20.0` resolves to `go-jose/v4 v4.1.4` (already direct) and `golang.org/x/oauth2 v0.36.0` (already indirect),
so exactly **one** module joins the repository's 109-module graph.
The argument against adoption is behavioural, not supply-chain.
It does, however, cost the `800-security-invariant.md` sentence
"Everything else in authentication is standard library," which would have to be amended.

**Spec tests that a go-oidc `RemoteKeySet` fails outright**, from `docs/specs/auth.md` *Testing*:

- "a token without `kid` verifies when one compatible key is held and is rejected when two are" — go-oidc accepts it with two.
- "an empty set, a set of only 1024-bit RSA keys, and a set with a duplicate `kid` are failed fetches that leave the previous set in place" — go-oidc stores all three.
- "a second unknown `kid` within `jwksRefreshMin` does not [fetch], and 100 concurrent ones trigger at most one" — go-oidc passes the concurrent half and fails the sequential half.
- "a set older than `jwksMaxStale` answers `503 auth_unavailable`" — go-oidc has no staleness bound.

---

## 4. The other two candidates

**`github.com/go-jose/go-jose/v4/jwt` — rejected on evidence.**
It is the strongest-looking option (zero new modules; the module is already direct),
and it fails on the exact hardening rule `internal/auth/verify.go:287-304` was written for.
`NumericDate.UnmarshalJSON` (`jwt/claims.go:63-72`) is `strconv.ParseFloat` followed by `NumericDate(f)` with no range guard,
so `"exp": 1e100` becomes an implementation-defined `int64` —
precisely the case the repository's `numericDate` refuses.
`jwt.Claims` also makes `exp` and `iat` optional pointers, so an absent `exp` skips the check;
`ValidateWithLeeway` applies one leeway symmetrically and cannot express the repository's per-claim rules.
Adopting it would regress two documented behaviours to save about 40 lines.

`jwt.Audience` (`jwt/claims.go:87-111`) is the one exception: its unmarshalling matches
`stringOrArray` (`verify.go:306-321`) semantics exactly — a string becomes a one-element slice,
an array of non-strings is refused, anything else is refused.
Swapping it in saves ~13 lines and adds no module.
It is a real but very small win.

**`github.com/zitadel/oidc/v3` — rejected without a source read, on two grounds.**
First, the graph: v3.49.3 through `pkg/client/rp` adds **17 modules** the repository does not have,
including `go-chi/chi/v5`, `rs/cors`, `gorilla/securecookie`, `golang/mock`, `google/go-github/v31`,
and four `go.opentelemetry.io/otel` modules.
For a project that vendors its UI assets rather than reaching for a CDN
and holds `internal/auth` to three greppable importers,
that is a large surface for a package whose job is one HTTP GET and a claim check.
Second, and decisive: every gap in section 2 is a **policy** choice — cooldown, `maxStale`, single-key selection,
`azp`, `iat`, `at+jwt` `typ`, minimum RSA size — not missing plumbing.
A different library implements a different policy; it does not implement this one.
The zitadel row is explicitly unexamined at the source level, and that reason is why it did not need to be.

**`github.com/zitadel/oidc/v3` v3.49.3, measured at the build-graph level** (added after the paragraph above),
because [`docs/specs/cli.md`](../specs/cli.md) now needs a device-flow client and the library ships one.
The module count above is what `go.sum` sees;
what the binary links is smaller and depends on the import path,
read with `go list -deps` in a scratch module:

| Import path | Modules linked that the repository does not have | Note |
|---|---|---|
| `pkg/oidc` (types only) | `zitadel/oidc`, `zitadel/schema`, `muhlemmer/gu` | three, all small |
| `pkg/client` (the HTTP calls) | the three above plus `gorilla/securecookie`, `go-logr/logr`, `go-logr/stdr`, and four OpenTelemetry modules (`otel`, `otel/metric`, `otel/trace`, `auto/sdk`) | about ten |
| `pkg/client/rp` (where the device flow lives) | the same as `pkg/client` | `google/uuid` is already present |

`go-chi/chi/v5`, `rs/cors`, `golang/mock`, and `google/go-github/v31` stay in the module graph and out of the binary.
So the weight is two separate costs:
seventeen entries of `go.sum` for the vulnerability scanners to report on,
and, for any package that performs a request, OpenTelemetry and a cookie library linked into a binary that uses neither.
The command-line client runs on user machines, which makes the second cost the larger one there.

What the source read found, once it was done:

- `pkg/client/rp/jwks.go` has the JWKS policy of go-oidc:
  in-flight coalescing through its `inflight` type, no cooldown across sequential requests,
  no periodic refresh, no staleness bound, an empty set stored as the cache,
  and a `kid`-less token matched only under the `SkipRemoteCheck` option.
  `oidc.KeySet` is an interface, so the repository's `keySet` could stay,
  which leaves the library nothing to replace in `jwks.go`, `issuer.go`, or `discovery.go`.
- `pkg/oidc/verifier.go` is more configurable than go-oidc where the spec cares:
  one `offset` applied to `exp` and `iat`, `MaxAgeIAT`, an `azp` check, and a nonce check.
  Its discovery type carries `end_session_endpoint`.
  That is a few dozen lines of claim checks, and the repository's own bounds on identity claims stay beside them.
- `rp.DeviceAuthorization` and `rp.DeviceAccessToken` implement the device grant the command-line client needs.
  By hand that is about 150 lines,
  each step already fixed by [`docs/specs/cli.md`](../specs/cli.md) *`profgate login` under `oidc`*.
  How `DeviceAccessToken` treats `slow_down` was not read.
- An upgrade costs more than go-oidc's:
  three major versions since v1,
  and a `RelyingParty` value that carries the cookie handler and the tracer.

The conclusion is the same as for go-oidc, for the same reason and one more:
the gaps are policy, not plumbing, so the library replaces nothing on the gateway;
and on the command line the 150 lines it would save are not worth OpenTelemetry in a user's binary.
[`docs/specs/cli.md`](../specs/cli.md) *Dependencies* stands: the client adds no Go module.

---

## 5. What the roadmap does with this

The roadmap proposed replacing the transport, cache, and discovery with a maintained library.
That item was withdrawn on this evidence;
the roadmap that carried it shipped as `v0.5.0` and left the tree.
`docs/specs/auth.md` *Dependencies* says the opposite of the proposal and is `Accepted`;
`000-agent-contract.md` ranks an accepted spec above a plan,
and the roadmap's own preamble says an item that changes behavior first revises the spec it names.
The spec wins because its four stated reasons are all still true of the current release, not because of its rank.

If that paragraph in `docs/specs/auth.md` is ever touched, it should name the version it was verified against
and say "no cooldown across sequential requests":
the present wording is checkable but slightly loose, and `iat` is unchecked rather than unskewed.

## 6. Smaller simplifications worth doing instead

Two of the three obvious candidates do not survive their own evidence:

- **Collapsing the dual refresh paths: no.**
  The timer is load-bearing.
  `docs/specs/auth.md` pairs it with `jwksMaxStale`
  ("the previous set is trusted only for `jwksMaxStale` … after the last successful fetch"),
  and the Failure Scenarios amendment row promises `503 auth_unavailable` after that bound.
  On-demand refresh alone would make a low-traffic replica's staleness a function of its traffic:
  a replica that serves nothing for 24 hours would answer `503` to the next valid token it sees.
  The on-demand path is equally load-bearing in the other direction —
  "a recovered issuer is noticed within `jwksRefreshMin` rather than at the next timer tick."
  Each path covers the other's blind spot; removing either changes a promise.
- **go-jose's `jwt` for claims parsing: no.**
  Section 4.

What is left:

- **`jwt.Audience` in place of `stringOrArray`** — ~13 lines, no new module, semantics identical.
  Optional; it trades a self-contained helper for a cross-package coupling inside the one package
  `800-security-invariant.md` already allows to import go-jose.

That is the whole list.
No larger simplification in this area survives the spec's own rules,
and manufacturing one would cost more churn than it saves.

---

## Summary

- **The replacement is not justified.**
  `docs/specs/auth.md` stands unchanged.
- **Version examined: `github.com/coreos/go-oidc/v3` v3.20.0.**
- **Line counts: 0 deleted, 927 kept, ~80 new glue → net ≈ +80 lines and one new module.**
  The saving the proposal assumed — several hundred lines removed — is not approached; the sign is wrong.
- All four of the spec's rejection clauses are still true of the current release,
  with `iat` unchecked (not merely unskewed) and `end_session_endpoint` absent from discovery as two additions.
- `zitadel/oidc` adds 17 modules to `go.sum`, about ten to the binary including OpenTelemetry, and the same policy gaps;
  its device-flow client would save the command line about 150 lines at that price;
  `go-jose/v4/jwt` regresses the numeric-date range guard.
- The only surviving micro-simplification is `jwt.Audience` for `stringOrArray` (~13 lines), and it is optional.
