# Profgate Authentication

**Status:** Accepted

This document designs the two authentication modes the gateway design
([`gateway.md`](gateway.md)) reserved for later: `basic` and `oidc`.
Everything there — permission boundary, discovery seam, HTTP API, realms,
configuration, testing — is assumed and not restated;
this document adds only the step that turns a request into a principal
and the step that turns a principal into a realm.
Gateway sections are cited by heading name.

---

## 1. Overview

The gateway attributes every request to a principal,
maps the principal to exactly one realm,
and lets the realm decide what the request may do
(gateway *Principals and realms*).
The accepted design defines one mode, `disabled`,
in which the principal is always `anonymous` and the realm is `auth.anonymousRealm`.

This document adds two modes that resolve the principal from a credential instead:

- **`basic`** — HTTP Basic authentication against a static user list.
  For a single team, a lab cluster, or any place where a shared secret in a Secret is enough.
- **`oidc`** — verification of a JWT issued by an OpenID Connect provider.
  A command-line client presents the token as a bearer credential;
  a browser, when the optional browser flow is configured, obtains one through the provider's login page
  and afterwards presents an encrypted session cookie that the gateway minted from it.

Both modes end at the same point:
a principal name and a realm name.
Nothing after realm resolution changes.

### 1.1 Core decisions

1. **The command line is the first client; the browser is the second.**
   The gateway's documented clients are `curl` and `go tool pprof`.
   `go tool pprof <url>` fetches with `http.Client.Get` and offers no way to add a header,
   so a Basic credential in the URL's userinfo reaches the gateway and a bearer token does not.
   That asymmetry is documented, not worked around.
   The browser flow exists because a pprof endpoint has always been something a browser can download from,
   and it changes nothing about how a bearer request is handled.
2. **The gateway verifies tokens; it does not mint identity.**
   Under `oidc`, the only credential the gateway trusts is a JWT the issuer signed.
   The browser flow obtains exactly such a JWT through the authorization-code flow,
   verifies it with the same rules as a bearer token,
   and then carries the *result* — a principal and a realm — in a cookie the gateway encrypted.
   No refresh token is ever held by the gateway; token renewal belongs to the client or the issuer.
3. **No identity-provider-specific code.**
   Every issuer is reached through OpenID Connect discovery,
   every claim name the gateway reads is configuration,
   and the differences between Keycloak, Dex, Okta, Entra ID, and Google live in `docs/`, not in Go.
4. **A credential that fails to resolve to a realm is denied.**
   There is no fall-through to `auth.anonymousRealm` in `basic` or `oidc`.
   Wide-open stays explicit (gateway *Wide-open is explicit*):
   an installation that wants every authenticated user in one realm writes that realm as the default.
5. **One principal, one realm.**
   A principal that matches several mapping entries takes the first;
   realms are not merged, so a principal's rights are always one realm's rights.
6. **Credentials travel only in the `Authorization` header or the session cookie.**
   A token in the query string would land in access logs, Ingress logs, and shell history;
   the gateway rejects `access_token` as a query parameter before it looks for any credential.
7. **Stateless.**
   The issuer's signing keys are the only authentication state an `oidc` gateway acquires at runtime,
   and every replica fetches them independently;
   users, mappings, secrets, and cookie keys are snapshots of configuration files.
   The browser session lives in the cookie, sealed with a key that is configuration, not state,
   so any replica can open a session any other replica minted.
   Nothing about authentication touches NATS or Kubernetes.
8. **Fail closed.**
   Keys older than a bound, an issuer that cannot be reached when it must be, a random source that fails:
   each answers `503` rather than admitting anyone on stale trust.

### 1.2 Non-goals

- Client-certificate authentication.
  It belongs to `auth.mode` like these two do, and is a later revision.
- Token acquisition for the command line.
  Under Keycloak's defaults an ID token lives five minutes,
  so a `curl` user re-authenticates often;
  the fix is a `profgate` command-line client that performs a device-code flow,
  keeps the refresh token on the user's own machine, and wraps `go tool pprof`.
  That is a separate design with its own document; the gateway needs nothing from it.
- Introspection of opaque access tokens (RFC 7662).
  It puts the issuer on every request's path;
  installations whose access tokens are opaque send the ID token instead.
- Session revocation before expiry.
  A stateless session cannot be revoked; a short `sessionTTL` is the compensation, and the document says so.
- Per-request authorization beyond realms: scopes, roles inside a realm, or claims read by handlers.
  Claims are consumed once, to pick a realm, and then discarded.
- Hot-reloading trust: the mode, the issuer, the audience, the CA, the client ID, the cookie key path.
  Users and mappings are policy and follow the gateway's hot classification;
  trust is restart-only, like the listen address.
- Browsers without Fetch Metadata (`Sec-Fetch-*` headers).
  The browser flow's cross-site protection reads them and admits nothing without them;
  every browser released since 2020 sends them.
- A web UI.
  The browser flow authenticates a browser; what the browser then sees is the API.
  A UI, and the listing endpoints it would need, is a later document.

---

## 2. Request algorithm

The gateway *Request algorithm* has an **Authentication** step between readiness and realm evaluation,
and [`pgo.md`](pgo.md) *HTTP API* inserts `501 pgo_disabled` and `503 pgo_unavailable` in the same gap.
The composed order for every route under `/v1` is:

1. Route → `404 route_unknown`, `404 profile_unknown`.
2. Method → `405 method_not_allowed`.
3. Readiness → `503 not_ready`.
4. PGO routes only: `501 pgo_disabled`, `503 pgo_unavailable`.
5. **Credential placement.**
   A query parameter named `access_token` → `400 invalid_parameter`, in every mode, before any credential is read.
6. **Authentication.**
   Resolve the principal and realm per `auth.mode` (this document)
   → `401 unauthenticated`, `429 too_many_auth`, `503 auth_unavailable`, or, for a browser navigation, `302`.
7. Realm → `403 realm_denied`.
8. Parameters and everything after, unchanged.

Step 5 sits before authentication so that a token in the URL is refused even when a valid one is also in the header;
the point is to make the URL form never work, not to make it redundant.

The three `/auth/` routes (section 6) are not under `/v1` and do not run this algorithm;
section 6.5 defines theirs.

Per mode, step 6 is:

| Mode | Step outcome |
|---|---|
| `disabled` | principal `anonymous`, realm `auth.anonymousRealm` (unchanged) |
| `basic` | parse `Authorization: Basic`, verify against the user set (section 3), principal is the user name, realm is the user's `realm` |
| `oidc` | `Authorization: Bearer` present → verify the JWT (section 4.3) and map it (section 5); otherwise, browser flow configured and a session cookie present → open it (section 6.6); otherwise no credential |

Under `oidc`, a request that carries both a bearer token and a session cookie is judged on the bearer token alone.

### 2.1 Failure responses

A request that fails step 6 answers **`401 unauthenticated`** with a `WWW-Authenticate` header,
which names the scheme the mode accepts:
`Basic realm="profgate"` or `Bearer realm="profgate"`.
The status is `401`, not `403`,
so a client can tell "present a credential" from "your credential does not reach this";
`403 realm_denied` keeps its meaning and follows only a successful authentication.

`401` is returned for all of:
no credential,
a credential of the wrong scheme,
a malformed credential,
a wrong password,
an invalid or expired token or session,
a session presented from another site,
and a valid credential that maps to no realm (section 5).
The response body is the standard error envelope with code `unauthenticated`
and one fixed message, `authentication required`,
identical across every reason;
the status, headers, code, and message of a `401` carry no information about which check failed.
The audit log records the reason (section 7).

One exception applies under `oidc` with the browser flow configured:
a request that carries no credential and is a browser navigation (section 6.2)
is answered with `302` to `/auth/login` instead of `401`.
A request that carries a credential and fails is `401` regardless of what sent it,
except that an expired or unopenable session cookie is first cleared and then treated as no credential,
so a returning browser is sent back to login rather than shown an error (section 6.6).

**`429 too_many_auth`** is returned by `basic` mode when the per-replica comparison gate is full (section 3.1).
It carries `Retry-After: 1`.

**`503 auth_unavailable`** is returned when the gateway cannot decide:
the signing keys are older than `jwksMaxStale` (section 4.4),
the random source fails while minting a transaction or session (section 6.4),
the issuer's token endpoint cannot be reached or answers with a server error during the browser flow (section 6.5),
or the authenticator returns an error that is not one of the classified failures (a programming error, logged at error).
It carries `Retry-After: 5` and no `WWW-Authenticate`.

`405`, `404 route_unknown`, `503 not_ready`, and the PGO availability codes precede authentication,
so an unauthenticated probe learns whether a route exists and whether the gateway is ready, and nothing else.
The ops listener is unchanged: no authentication, no realm.

### 2.2 Configuration snapshot

Step 6 evaluates against the configuration snapshot the request loaded at entry,
the same pointer the realm step reads.
Users, mappings, and realms therefore always come from one snapshot;
a hot reload that replaces the pointer takes effect on the next request and never mid-request.

---

## 3. `basic` mode

```yaml
auth:
  mode: basic
  basic:
    users:
      - name: alice
        passwordHash: "$2a$12$..."   # bcrypt; every hash shares one cost, 10–14
        realm: developer
    usersFile: /etc/profgate/auth/users.yaml   # optional; same shape, merged
    allowPlaintext: false
    maxConcurrent: 16
```

### 3.1 Credential

`Authorization: Basic base64(name ":" password)` per RFC 7617.
The header value is limited to 1 KiB before decoding; a longer one is `malformed`.
The user name is looked up exactly; there is no case folding.
A password longer than 72 bytes is rejected as `malformed` before comparison,
because bcrypt reads no further and a silent truncation would make two passwords equal.

The password is checked with `bcrypt.CompareHashAndPassword`.
Exactly one bcrypt comparison runs per well-formed Basic attempt that acquires a gate slot, whether the user exists or not,
and every comparison runs at the one cost the user set shares (section 3.2):
an unknown name is compared against a *dummy hash* the gateway computed at load time at that cost.
The unknown-user path and the wrong-password path therefore do the same work.

A bcrypt comparison is deliberately slow, which makes unauthenticated requests a lever on CPU.
Comparisons pass through a per-replica gate of `maxConcurrent` slots, acquired without waiting;
a request that finds none free is `429 too_many_auth` without any comparison.
The gate is separate from `limits.maxConcurrentProfiles`, which admits work that has already authenticated.

### 3.2 Users

| Field | Validation |
|---|---|
| `name` | required; 1–256 bytes; no `:` (RFC 7617 excludes it from the user-id) |
| `passwordHash` | required; a bcrypt hash (`$2a$`, `$2b$`, or `$2y$` prefix) with cost 10–14 |
| `realm` | required; names an entry in `realms` |

Every hash in the user set — inline and file together — must carry the same cost;
a set with two costs is a validation error naming both.
One cost is what makes the dummy comparison indistinguishable from a real one;
the range bounds the per-request CPU price an operator can set.
Only hashes are accepted.
A plaintext password in configuration is a validation error, not a warning:
the check is that the value parses as a bcrypt hash within the cost range.
`profgate auth hash` reads a password from the terminal and prints its hash at cost 12,
so an operator never has to find `htpasswd`.

`usersFile` points at a YAML file with a single `users` list of the same shape,
so the hashes can live in a Secret volume while the rest of the configuration stays in a ConfigMap.
Names must be unique across the inline list and the file together;
a duplicate is a validation error.
At least one user must exist in `basic` mode.

The file follows the certificate's rule (gateway *Configuration*):
its path is fixed for the life of the process,
a goroutine re-reads it every 30 seconds, hashes the bytes,
and parses and swaps the merged user set only when the hash differs;
the merged set is the inline users plus the file users, plus the recomputed dummy hash.
A read or parse that fails — including a cost that differs from the inline users' — leaves the previous set in place,
logs at warn, and counts a `failed` reload.
Polling rather than watching, for the reason the certificate is polled: a Secret volume update replaces the inode.

### 3.3 Transport

Basic authentication sends the password on every request.
`mode: basic` with `server.tls` unset is a validation error
unless `auth.basic.allowPlaintext` is `true`,
in which case the gateway starts and logs, at warning level:

```text
basic authentication over plaintext HTTP; passwords cross the network in the clear
```

The escape hatch exists for a lab behind a TLS-terminating Ingress;
the default refuses.

### 3.4 Clients

```bash
curl -u alice -sf -o cpu.pprof \
  "https://profgate.example/v1/namespaces/payments/services/checkout/profiles/cpu?seconds=30"
go tool pprof "https://alice:PASSWORD@profgate.example/v1/namespaces/payments/services/checkout/profiles/cpu"
```

The second form works because Go's HTTP client turns URL userinfo into a Basic header;
it also puts the password in shell history and process listings, which the documentation says.
A browser that receives `401` with `WWW-Authenticate: Basic` shows its own login dialog and retries,
so `basic` mode needs nothing else to serve a browser.

---

## 4. `oidc` mode

```yaml
auth:
  mode: oidc
  oidc:
    issuer: https://keycloak.example/realms/engineering
    audience: profgate
    tokenType: id                  # id | access
    usernameClaim: preferred_username
    groupsClaim: groups
    caFile: /etc/profgate/auth/issuer-ca.crt   # optional
    mapping:
      users:
        - name: ci-bot
          realm: automation
      groups:
        - name: platform-admins
          realm: admin
        - name: payments-dev
          realm: payments
      defaultRealm: ""
    browser:                       # optional; section 6
      clientID: profgate           # must equal audience
      redirectURL: https://profgate.example/auth/callback
      cookieKeyFile: /etc/profgate/auth/cookie.key
      sessionTTL: 8h
```

### 4.1 Issuer client

Every request the gateway makes to the issuer — discovery, JWKS, and the browser flow's token exchange —
goes through one dedicated `http.Client`:

- `https://` only.
  Discovery and JWKS requests follow at most 3 redirects, each to an `https://` URL;
  the token-endpoint request follows none,
  because a `307` or `308` would replay the client secret to whatever host the redirect names.
- Connect timeout 5s, TLS handshake timeout 5s, response-header timeout 5s, overall deadline 10s per request.
- Response bodies are read through a reader limited to 1 MiB + 1 byte;
  a body that fills it, or one with any bytes after the JSON value, fails the request.
- TLS verification uses the system pool, plus the certificates in `caFile` when set;
  `caFile` must parse to at least one certificate or validation fails.
- No proxy from the environment unless `auth.oidc.httpProxy` names one;
  a corporate issuer is usually inside the cluster's reach.

### 4.2 Discovery

`issuer` is the OpenID Connect issuer URL and must be `https://`;
a plaintext issuer is a validation error with no override.
At startup the gateway fetches `<issuer>/.well-known/openid-configuration`
and requires the document's `issuer` to equal the configured value byte for byte.
It records `jwks_uri`, and, when the browser flow is configured, `authorization_endpoint`, `token_endpoint`,
and `end_session_endpoint` (the last optional).
Each recorded endpoint must be an absolute `https://` URL with no userinfo and no fragment;
one that is not fails discovery, because the gateway will later send a browser to the first
and a client secret to the second.

It then performs the initial JWKS fetch (section 4.4) and requires at least one usable key.
Both fetches retry with backoff for up to `auth.oidc.discoveryTimeout` (default 30s);
if either has not succeeded by then the process exits.
A gateway that cannot reach its issuer cannot authenticate anyone,
and exiting is better than serving `503` to every request while looking healthy.

In the gateway's startup sequence (gateway *Startup and shutdown*), issuer discovery is a state between
`[listening]` and `[preflight]`: both listeners are open, `/healthz` is `200`, `/readyz` is `503`,
and the Kubernetes preflight does not begin until discovery and the initial JWKS fetch have succeeded.
`/readyz` therefore stays `503` until both have succeeded,
so a rolling update does not shift traffic onto a replica that cannot verify.

### 4.3 Token verification

A token is a compact JWS whose payload is a JWT.
`tokenType` selects which profile the token must meet;
one gateway accepts one kind.

Checks common to both kinds, in order:

1. The compact serialization parses; the header has exactly one `alg`; the token is at most 16 KiB.
2. `alg` is in the allowed set: `RS256`, `RS384`, `RS512`, `ES256`, `ES384`, `ES512`, `PS256`, `PS384`, `PS512`.
   `none` and every HMAC algorithm are rejected before any key lookup.
   Then, and only then, the held key set must be younger than `jwksMaxStale` (section 4.4);
   a stale set first triggers one refresh subject to section 4.4's cooldown,
   and if the set is still stale afterwards the answer is `503 auth_unavailable` with audit reason `keys_stale`.
   A token that fails checks 1 or 2 is `401` whether or not the keys are stale.
3. Key selection.
   With a `kid` header: the held key with that `kid`, which must be compatible with `alg` (section 4.4);
   none held → one refresh subject to section 4.4's cooldown, then one more lookup.
   Without a `kid` header: the single held key compatible with `alg`, if exactly one exists; otherwise rejected.
   The verifier never tries every key.
4. The signature verifies against the selected key.
5. `iss` equals the configured issuer.
6. `sub` is present and a non-empty string of at most 256 bytes with no NUL byte.
7. `iat` is present, a JSON number, and at most `clockSkew` in the future.
8. `exp` is present, a JSON number, and in the future; `nbf`, when present, is a JSON number and in the past;
   each with `clockSkew` (default 30s) of tolerance.
   A temporal claim that is missing when required, or not a number, fails its check with reason `expired`,
   because the token cannot be shown to be in its validity window.
9. The username claim is present and a non-empty string of at most 256 bytes with no NUL byte.
   It defaults to `sub`; when it is something else, `sub` is still required by check 6.

Under `tokenType: id` (OpenID Connect Core 1.0, ID Token Validation):

10. `aud` contains `audience`.
11. If `aud` has more than one value, `azp` is present and equals `audience`.

Under `tokenType: access` (RFC 9068):

10. The JOSE header `typ` is `at+jwt` (case-insensitive, per RFC 9068).
11. `aud` contains `audience`.

The two profiles exist because an ID token and an access token are issued to different parties with different audiences,
and a gateway that accepted either could be fed the one the operator did not intend.
An access token that lacks `typ: at+jwt` is rejected, not downgraded to an ID token.

### 4.4 Signing keys

The gateway holds the issuer's JWKS in memory and serves every verification from that copy.
A key is *usable* when its `kty` is `RSA` or `EC`, its `use` is `sig` or absent, its public key parses,
an RSA modulus is at least 2048 bits,
and, when the key carries `alg`, that `alg` is in the allowed set;
others are dropped at load with a warning naming their `kid`.
A set in which two usable keys share a `kid` is rejected as a whole, because a `kid` that names two keys names none.

Compatibility between a token's `alg` and a key is:
`RS*` and `PS*` need an RSA key;
`ES256` needs an EC key on P-256, `ES384` on P-384, `ES512` on P-521;
a key that carries `alg` must carry the token's.

The copy is replaced:

- on a timer, every `jwksRefresh` (default 1h);
- on demand, when a token names a `kid` the copy does not hold,
  at most once per `jwksRefreshMin` (default 1m) across all requests;
  a token arriving inside the cooldown after a refresh that did not produce its `kid` is rejected without another fetch.

A fetch *succeeds* only when it yields at least one usable key and no duplicate `kid`;
an HTTP `200` whose keys are all dropped is a failed fetch and replaces nothing.
A successful fetch replaces the whole set atomically behind a pointer;
verifications already in flight finish against the set they loaded.
Overlap during an issuer key rotation needs nothing special:
the issuer publishes both keys, a fetch brings both, and tokens signed with either verify.
A refresh that fails leaves the previous set in place, logs at warn, and counts a `failed` refresh.

The previous set is trusted only for `jwksMaxStale` (default 24h) after the last successful fetch.
Past that bound, every token that passes section 4.3's checks 1 and 2 is answered `503 auth_unavailable` (audit reason `keys_stale`) before any key is selected,
until a fetch succeeds,
because a key the issuer may have withdrawn a day ago is not a key to keep accepting.
Each such token first attempts one refresh, subject to the cooldown,
so a recovered issuer is noticed within `jwksRefreshMin` rather than at the next timer tick.

Every replica fetches its own copy.
There is no shared cache and no coordination;
the issuer sees one discovery and one JWKS fetch per replica per interval.

---

## 5. Principal to realm

`oidc` mode resolves the realm in this order and stops at the first match:

1. `mapping.users`: the username claim equals `name`.
2. `mapping.groups`: the groups claim contains `name`; entries are tried in the order written.
3. `mapping.defaultRealm`, when set.

No match is `401 unauthenticated` with audit reason `no_realm`.
`defaultRealm: ""`, or the key absent, means step 3 never matches.

The groups claim, when present, must be a JSON array of strings or a single string;
a string is treated as a one-element array.
An absent claim is an empty array,
so an installation that maps only users, or only `defaultRealm`, needs none.
A claim that is present with any other shape is `401` with reason `claim`;
malformed identity data must not fall through to the default realm.
Group names are compared exactly.
Keycloak emits group paths (`/engineering/payments`) unless its mapper is told otherwise;
the issuer notes say so.

| Field | Validation |
|---|---|
| `mapping.users[].name` | required; 1–256 bytes; unique within the list |
| `mapping.users[].realm` | required; names an entry in `realms` |
| `mapping.groups[].name` | required; 1–256 bytes; unique within the list |
| `mapping.groups[].realm` | required; names an entry in `realms` |
| `mapping.defaultRealm` | optional; when non-empty, names an entry in `realms` |

At least one of `mapping.users`, `mapping.groups`, or `mapping.defaultRealm` must be set,
otherwise `oidc` mode can admit nobody and the configuration is rejected.

`basic` mode has no mapping step: each user names its realm.

The principal name is the username claim's value.
Under `basic` it is the user name.
It is what the audit log and the PGO records' `CreatedBy` and `UpdatedBy` ([`pgo.md`](pgo.md)) carry.

---

## 6. Browser flow

The browser flow is the optional `auth.oidc.browser` block.
Without it, `oidc` mode is bearer-only and every route answers a browser with `401`,
which is a correct and complete configuration for an installation whose clients are all command lines.

With it, the gateway becomes an OpenID Connect *relying party* for browsers,
using the authorization-code flow with PKCE (RFC 7636),
and mints a session cookie from the ID token it receives.
The issuer's own session then supplies the single-sign-on experience:
when the gateway's cookie expires,
the redirect to the issuer completes without a password prompt for as long as the issuer's session lives.

### 6.1 Configuration

| Key | Default | Validation |
|---|---|---|
| `clientID` | — | required; equals `auth.oidc.audience` |
| `clientSecretFile` | — | optional; a readable file; its trimmed contents are the secret, 1–1024 bytes |
| `redirectURL` | — | required; `https://` URL with no userinfo, query, or fragment, whose path is `/auth/callback` |
| `scopes` | `["openid", "profile", "email"]` | must contain `openid`; each 1–64 bytes of RFC 6749 scope characters; unique |
| `cookieKeyFile` | — | required; section 6.3 |
| `sessionTTL` | `8h` | 5m–24h |
| `transactionTTL` | `5m` | 1m–15m |

`clientID` must equal `audience` because the ID token the flow receives is issued to the client,
carries the client ID in `aud`, and is verified by section 4.3 against `audience`;
two different values would validate and never log anyone in.

The browser block requires `server.tls`:
the cookies below carry `Secure` and a `__Host-` prefix, which a plaintext listener cannot set.
A TLS-terminating Ingress in front of a plaintext gateway is the case `allowPlaintext` covers for Basic;
it is not covered here, because the gateway would then be unable to tell a secure origin from an insecure one.

The client is a *public* client with PKCE unless `clientSecretFile` is set;
Keycloak, Dex, and Okta all issue tokens to a public client whose redirect URI is registered.
A secret, when set, is sent with the code exchange as `client_secret_post`.

`tokenType` must be `id` when the browser block is configured:
the browser flow verifies the ID token from the code exchange, and there is no reason to hand a browser an access token.

### 6.2 What is redirected

A request under `oidc` that carries no `Authorization` header and no session cookie is a *navigation*
when it has `Sec-Fetch-Mode: navigate` and `Sec-Fetch-Dest: document`.
A navigation to a route under `/v1` is answered with `302` to `/auth/login?return=<path>`,
where `<path>` is the request's path and query;
any other request without a credential is `401`.
There is no fallback on `Accept`:
a client without Fetch Metadata could not use the session it would be sent to obtain (section 6.6).

The distinction is what keeps `curl` from being sent to a login page:
a script gets `401` and a `WWW-Authenticate` header, a browser gets a login.
`fetch()` calls from a page — a future UI's JSON requests — have `Sec-Fetch-Mode: cors` or `same-origin`,
are not navigations, and get `401`;
the page decides whether to navigate to `/auth/login`.

### 6.3 Cookie key

`cookieKeyFile` holds one or two 32-byte keys, base64-encoded, one per line.
The first line is the *current* key and seals every new cookie;
every line opens.
The file is re-read every 30 seconds by the same mechanism as the certificate and the users file;
a read that fails, or a file with zero or more than two keys,
leaves the previous keys in place and counts a `failed` reload.
If no keys were ever loaded — the file was unreadable at startup — startup fails.

Rotation across replicas is staged, because replicas re-read the file at different moments
and a cookie sealed with a key another replica has not loaded yet is a lost session:

1. Write `old` then `new`: every replica learns to open `new` while still sealing with `old`.
2. Wait until every replica reports both fingerprints (below).
3. Write `new` then `old`: replicas begin sealing with `new`; `old` still opens.
4. Wait one `sessionTTL` after every replica reports `new` as current.
5. Write `new` alone.

The ops listener reports `profgate_auth_cookie_key_info{fingerprint,role}` with value 1 per loaded key,
where `fingerprint` is the first 8 hex digits of `SHA-256(key)` and `role` is `current` or `previous`;
that is how an operator confirms propagation without reading key material.

Cookies are sealed with AES-256-GCM under the current key,
with a 12-byte nonce from `crypto/rand` per cookie
and the cookie's name as associated data,
so a transaction cookie cannot be replayed as a session cookie.
The cookie value is `base64url(nonce || ciphertext)`.
Every replica shares the file through the same Secret volume, which is what makes the session stateless.

### 6.4 Wire values and bounds

`state`, `nonce`, and the PKCE `code_verifier` are each 32 bytes from `crypto/rand`,
encoded as unpadded base64url into 43-character strings;
the verifier therefore satisfies RFC 7636's 43–128 character rule,
and `code_challenge` is `base64url(SHA-256(ASCII(code_verifier)))` with `code_challenge_method=S256`.
A read from `crypto/rand` that fails answers `503 auth_unavailable` with reason `entropy`.

The *return path* is the one piece of client-supplied data a cookie carries.
It is accepted only when, after percent-decoding,
`url.Parse` yields a value with an empty scheme, host, user, and opaque part,
a path that begins with `/` and not `//`,
no `\` and no byte below `0x20` anywhere,
and a total length of at most 1024 bytes;
it is then re-serialized as path plus query — the fragment is dropped — and that string is what is sealed.
Anything else becomes `/`.
The rule is stated in terms of the parsed form because browsers turn `/\evil.example` into `//evil.example`,
and a prefix check on the raw string would let it through.

Both cookies omit `Domain`, so they bind to the exact host.
Sealed plaintexts are length-prefixed: each field is a two-byte big-endian length followed by its bytes,
except `exp`, which is eight bytes of big-endian Unix seconds,
and the opener rejects a plaintext with bytes left over after the last field.
The session fields are the principal (at most 256 bytes; section 4.3 already bounds the username claim and forbids NUL),
the realm (a DNS-1123 label), and `exp`;
the transaction fields are the three wire values, the return path, and `exp`.
Both encode to well under 2 KiB, and the gateway refuses to set a cookie whose final value exceeds 4000 bytes,
which cannot happen within these bounds and is checked so that a future field cannot silently cross the browser limit.

### 6.5 The `/auth/` routes

The three routes exist only when the browser block is configured and are `404 route_unknown` otherwise.

*Deleting* a cookie, wherever this section says so,
means a `Set-Cookie` for the same name with an empty value and `Max-Age=0`,
carrying exactly the attributes it was set with: `Secure; HttpOnly; SameSite=Lax; Path=/` and no `Domain`.
A browser matches a deletion to a cookie by name, path, and host,
and a `__Host-` cookie refuses any other shape.
They are not under `/v1`, carry `Cache-Control: no-store`, accept `GET` only (`405` with `Allow: GET` otherwise),
answer `503 not_ready` until the gateway is ready — the callback needs discovery to have succeeded —
and have no credential placement, authentication, or realm step:
they are how a credential is obtained, and the session they mint is judged by section 2 on the next request.
Each writes one audit line (section 7).

**`GET /auth/login?return=<path>`** starts a login.
The gateway validates the return path (section 6.4), generates the three wire values,
seals `{state, nonce, code_verifier, return, exp: now + transactionTTL}` into `__Host-profgate_txn`
(`Secure; HttpOnly; SameSite=Lax; Path=/; Max-Age=<transactionTTL seconds>`),
and answers `302` to the issuer's `authorization_endpoint` with
`response_type=code`, `client_id`, `redirect_uri`, `scope`, `state`, `nonce`,
`code_challenge`, and `code_challenge_method=S256`.

**`GET /auth/callback?code=...&state=...`** completes it:

1. Open `__Host-profgate_txn`; absent, unopenable, or expired → `401` with reason `state`, and the cookie is deleted.
2. `state` equals the sealed one, byte for byte → otherwise `401` with reason `state`.
3. An `error` parameter from the issuer → `401` with reason `issuer_denied`; the `error` value is logged, never echoed.
4. `POST token_endpoint` through the issuer client (section 4.1)
   with `grant_type=authorization_code`, `code`, `redirect_uri`, `client_id`, `code_verifier`,
   and `client_secret` when configured.
   A transport failure or a `5xx` → `503 auth_unavailable` with reason `exchange`;
   a `4xx` → `401` with reason `exchange_denied`,
   because the issuer refused the code, which a replayed or expired code produces;
   a `200` without an `id_token` string → `401` with reason `exchange_denied`.
5. Verify `id_token` per section 4.3 with `tokenType: id`, plus: the `nonce` claim equals the sealed one.
   Failure → `401` with the section 4.3 reason, or `nonce`.
6. Map to a realm per section 5. No match → `401` with reason `no_realm`.
7. Seal `{principal, realm, exp: now + sessionTTL}` into `__Host-profgate_session`
   (`Secure; HttpOnly; SameSite=Lax; Path=/; Max-Age=<sessionTTL seconds>`),
   delete `__Host-profgate_txn`, and `302` to the sealed return path.

A `401` from the callback is the standard envelope; a browser shows it as a page.
It is not redirected back to login, because a loop between two failing endpoints is worse than an error page.

**`GET /auth/logout`** deletes the session cookie and answers `302` to the issuer's `end_session_endpoint`
with `post_logout_redirect_uri=<scheme://host of redirectURL>/` and `client_id`, when discovery published one,
and `302` to `/` otherwise.
Logout is a `GET` because a bookmark or a link must be able to trigger it;
a cross-site request that logs a user out is a nuisance, not a breach.

### 6.6 The session

The session lives for `sessionTTL` from the moment it is minted.
The gateway does not consult the issuer again while it lives:
an issuer-side logout, a disabled account, or a changed group reaches the gateway at the next login and not before,
so `sessionTTL` is exactly the exposure an operator accepts.
The realm is sealed at login for the same reason.
A realm's *contents* are read from configuration on every request, so a realm edit applies at once;
a principal's *mapping* applies at its next login.

A request that carries `__Host-profgate_session` and no `Authorization` header:

1. Open the cookie.
   Unopenable or expired → delete it, record reason `session`,
   then treat the request as carrying no credential: a navigation is `302` to login, anything else is `401`.
2. `Sec-Fetch-Site` must be `same-origin` or `none`, for every method including `GET`;
   `same-site`, `cross-site`, any other value, or the header absent → `401` with reason `csrf`.
   The cookie is sent automatically by the browser,
   `SameSite=Lax` still attaches it to a cross-site top-level `GET`,
   and a profile `GET` spends the victim's CPU-profiling authority even when the attacker cannot read the response.
   `none` admits a typed URL or a bookmark; a link from another origin arrives as `cross-site`.
3. The principal and realm are the sealed values; nothing else is looked up.

The `302` that a navigation receives, from step 1 here or from section 6.2,
is recorded in the audit log with status `302`, code `auth_redirect`, and the reason (`missing` or `session`),
and is counted under that code in the request metrics;
it is not a `401` and is not counted as an authentication failure.

### 6.7 Limits of the design

- A session cannot be revoked before its `exp`; disabling the user at the issuer stops the *next* login only.
  `sessionTTL` is the bound on that exposure.
- Two gateways behind one hostname must share `cookieKeyFile`; the Helm chart mounts one Secret for all replicas.
- The gateway trusts `Sec-Fetch-*` headers as the browser sends them.
  A non-browser client can forge them, and gains only what it could already have with a bearer token: nothing.
- The token endpoint is the one issuer request on a user's path;
  an issuer outage stops logins and leaves existing sessions and bearer tokens untouched.

---

## 7. Audit and metrics

The audit record (gateway *Logging*) already carries `principal`.
A request that fails authentication is recorded with principal `-`,
the status and code it answered with,
and a new field `auth_reason` with one of:

| `auth_reason` | Meaning |
|---|---|
| `missing` | no credential |
| `scheme` | wrong `Authorization` scheme for the mode |
| `malformed` | the credential did not parse, or exceeded a size limit |
| `bad_credential` | unknown user or wrong password |
| `throttled` | the bcrypt comparison gate was full |
| `signature` | no key verified the token, or no key could be selected |
| `alg` | disallowed algorithm |
| `issuer` | `iss` mismatch |
| `audience` | `aud` or `azp` check failed |
| `token_type` | `typ` is not `at+jwt` under `tokenType: access` |
| `expired` | `iat`, `nbf`, or `exp` outside the validity window, missing when required, or not a number |
| `claim` | `sub` or the username claim missing, empty, or too long; the groups claim present with a bad shape |
| `nonce` | browser flow: ID token `nonce` mismatch |
| `no_realm` | authenticated but mapped to no realm |
| `session` | session cookie unopenable or expired |
| `state` | browser flow: transaction cookie missing, expired, or `state` mismatch |
| `issuer_denied` | browser flow: the issuer returned `error` |
| `exchange_denied` | browser flow: the token endpoint refused the code or returned no ID token |
| `exchange` | browser flow: the token endpoint unreachable or answered `5xx` |
| `csrf` | session-authenticated request whose `Sec-Fetch-Site` is not `same-origin` or `none` |
| `keys_stale` | signing keys older than `jwksMaxStale` |
| `entropy` | `crypto/rand` failed |
| `internal` | the authenticator returned an error that is none of the above; a programming error, logged at error |

Status by reason: `throttled` is `429`; `exchange`, `keys_stale`, `entropy`, and `internal` are `503`;
`missing` and `session` are `302` for a navigation under the browser flow and `401` otherwise;
every other reason is `401`.
`auth_reason` is absent on a successful request.
A failure records the reason and never the credential, the token, or the subject when it differs from the principal.
The `/auth/` routes write an audit line with route `auth_login`, `auth_callback`, or `auth_logout`,
principal `-` for login and logout and the resolved principal for a successful callback,
and no namespace or Service.

The ops listener gains:

- `profgate_auth_failures_total{mode,reason}` — one series per reason above, counted for `401`, `429`, and `503`;
  redirects are not failures.
- `profgate_auth_sessions_issued_total` — browser sessions minted.
- `profgate_oidc_jwks_refresh_total{result}` — `ok` or `failed`.
- `profgate_oidc_jwks_keys` — the number of usable keys currently held.
- `profgate_oidc_jwks_age_seconds` — seconds since the last successful fetch; the alertable form of `keys_stale`.
- `profgate_auth_file_reload_total{file,result}` — `file` is `users` or `cookie_key`; `result` is `ok` or `failed`.
- `profgate_auth_cookie_key_info{fingerprint,role}` — section 6.3.

`reason`, `mode`, `file`, `result`, and `role` are closed sets;
`fingerprint` has at most two values at a time.
Principal names never become labels.

---

## 8. Issuer notes

These are the differences between issuers that decision 3 keeps out of the code;
they live in `docs/` and are cited here so the design is checkable against real issuers.

- **Keycloak.** Access tokens carry `aud` only when a client scope adds an audience mapper,
  and carry `typ: Bearer` rather than `at+jwt` unless the client is configured for RFC 9068;
  ID tokens carry the client ID as `aud` by default, so `tokenType: id` is the natural setting.
  Groups appear only when a "Group Membership" mapper is added to the client scope,
  and appear as full paths (`/engineering/payments`) unless "Full group path" is off.
  `preferred_username` is present in both token kinds.
  ID and access tokens live 5 minutes by default and the SSO session 30 minutes idle, 10 hours maximum;
  a `curl` user should raise the client's token lifespan or use the command-line client.
- **Dex.** ID tokens carry the client ID as `aud`, `groups` as a flat array, and `email` / `name`;
  there is no `preferred_username`, so `usernameClaim: email` is the usual choice.
  Dex has no `end_session_endpoint`; logout clears the cookie and returns to `/`.
- **Okta.** ID tokens carry the client ID as `aud`;
  `groups` requires a claim added to the authorization server.
- **Entra ID.** `aud` on an ID token is the application (client) ID;
  `groups` is present when configured and holds group object IDs, not names, unless the tenant opts into names;
  a user with more than 200 groups gets an overage claim instead, which this gateway does not follow.
- **Google.** ID tokens carry `email` and no groups claim; map users or `defaultRealm`.

The end-to-end suite (section 10) runs against Dex,
because it is a single binary with a static configuration and no database.
Keycloak is verified against a real instance during implementation, and the verification is recorded in the issuer notes;
the Okta, Entra ID, and Google notes are derived from their published documentation and marked unverified until someone runs them.

---

## 9. Configuration

Added and changed rows in the gateway *Configuration* table:

| Key | Env | Default | Reload | Validation |
|---|---|---|---|---|
| `auth.mode` | `PROFGATE_AUTH_MODE` | `disabled` | restart | `disabled`, `basic`, `oidc` |
| `auth.anonymousRealm` | `PROFGATE_ANONYMOUS_REALM` | — | hot | required in `disabled`, forbidden otherwise; names an entry in `realms` |
| `auth.basic.users` | — | — | hot | section 3.2 |
| `auth.basic.usersFile` | `PROFGATE_AUTH_BASIC_USERS_FILE` | — | restart (path) | a readable file of the section 3.2 shape |
| `auth.basic.allowPlaintext` | `PROFGATE_AUTH_BASIC_ALLOW_PLAINTEXT` | `false` | restart | section 3.3 |
| `auth.basic.maxConcurrent` | `PROFGATE_AUTH_BASIC_MAX_CONCURRENT` | `16` | restart | 1–1024 |
| `auth.oidc.issuer` | `PROFGATE_AUTH_OIDC_ISSUER` | — | restart | required in `oidc`; `https://` URL |
| `auth.oidc.audience` | `PROFGATE_AUTH_OIDC_AUDIENCE` | — | restart | required in `oidc`; 1–256 bytes |
| `auth.oidc.tokenType` | `PROFGATE_AUTH_OIDC_TOKEN_TYPE` | `id` | restart | `id`, `access`; `id` when `browser` is set |
| `auth.oidc.usernameClaim` | `PROFGATE_AUTH_OIDC_USERNAME_CLAIM` | `sub` | restart | 1–64 bytes |
| `auth.oidc.groupsClaim` | `PROFGATE_AUTH_OIDC_GROUPS_CLAIM` | `groups` | restart | 1–64 bytes |
| `auth.oidc.caFile` | `PROFGATE_AUTH_OIDC_CA_FILE` | — | restart | a readable PEM file with at least one certificate |
| `auth.oidc.httpProxy` | `PROFGATE_AUTH_OIDC_HTTP_PROXY` | — | restart | `http://`, `https://`, or `socks5://` URL |
| `auth.oidc.discoveryTimeout` | `PROFGATE_AUTH_OIDC_DISCOVERY_TIMEOUT` | `30s` | restart | 1s–10m |
| `auth.oidc.clockSkew` | `PROFGATE_AUTH_OIDC_CLOCK_SKEW` | `30s` | restart | 0s–5m |
| `auth.oidc.jwksRefresh` | `PROFGATE_AUTH_OIDC_JWKS_REFRESH` | `1h` | restart | 1m–24h |
| `auth.oidc.jwksRefreshMin` | `PROFGATE_AUTH_OIDC_JWKS_REFRESH_MIN` | `1m` | restart | 1s–1h |
| `auth.oidc.jwksMaxStale` | `PROFGATE_AUTH_OIDC_JWKS_MAX_STALE` | `24h` | restart | ≥ `jwksRefresh`, ≤ 7d |
| `auth.oidc.mapping.users` | — | — | hot | section 5 |
| `auth.oidc.mapping.groups` | — | — | hot | section 5 |
| `auth.oidc.mapping.defaultRealm` | `PROFGATE_AUTH_OIDC_DEFAULT_REALM` | — | hot | section 5 |
| `auth.oidc.browser.clientID` | `PROFGATE_AUTH_OIDC_CLIENT_ID` | — | restart | section 6.1 |
| `auth.oidc.browser.clientSecretFile` | `PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE` | — | restart | section 6.1 |
| `auth.oidc.browser.redirectURL` | `PROFGATE_AUTH_OIDC_REDIRECT_URL` | — | restart | section 6.1 |
| `auth.oidc.browser.scopes` | — | `openid, profile, email` | restart | section 6.1 |
| `auth.oidc.browser.cookieKeyFile` | `PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE` | — | restart (path) | section 6.3 |
| `auth.oidc.browser.sessionTTL` | `PROFGATE_AUTH_OIDC_SESSION_TTL` | `8h` | restart | 5m–24h |
| `auth.oidc.browser.transactionTTL` | `PROFGATE_AUTH_OIDC_TRANSACTION_TTL` | `5m` | restart | 1m–15m |

`anonymousRealm` is required only in `disabled` mode;
in the other two it is a validation error if set,
so a configuration cannot carry a dormant wide-open realm that a later mode change silently activates.
The `basic` block is a validation error unless `mode` is `basic`, and likewise for `oidc`,
so a block that does not apply cannot be mistaken for one that does.
In Go the inactive blocks are pointers, so "absent" and "present with defaults" are distinguishable
and a default inside an absent block cannot make the block look configured.

Only policy is hot: users, mappings, and `anonymousRealm`.
No configuration reloader exists yet (gateway *Non-goals*);
`hot` marks the fields such a reloader may replace without a restart,
because every request reads one snapshot (section 2.2);
until one ships, the users file and the cookie key file are the only values re-read while running.
Everything that establishes trust is restart-only —
the mode, the issuer, the audience, the CA, the client, the key paths —
so a reload can change who is admitted but never whom the gateway believes.
The two `restart (path)` files follow the certificate's re-read rule.

---

## 10. Testing

The seams the tests need, all in `internal/auth`:

- a clock (`func() time.Time`) on the verifier, the JWKS cache, the session sealer, and the file pollers;
- a `Refresh(ctx)` method on the JWKS cache and a `Poll()` method on each file poller,
  so a test drives what the timer and the 30-second goroutine drive in production,
  and the goroutines are not started under test;
- a `keyFetcher` interface the JWKS cache calls, so a test counts fetches and fails them on demand;
- a `fileReader` (`func(path string) ([]byte, error)`) on each poller, so a test replaces a file without touching disk;
- a `passwordComparer` interface with the bcrypt implementation, so a test counts comparisons;
- an `io.Reader` random source on the browser flow, so a test fixes wire values and makes the source fail;
- the issuer client's `http.RoundTripper`, so a test serves discovery, JWKS, and the token endpoint from `httptest`.

Unit, in `internal/auth`:

- Basic: correct password admits;
  wrong password, unknown user, malformed header, oversize header, oversize password, and wrong scheme each answer `401`,
  with the matching `auth_reason`;
  unknown user and wrong password each perform exactly one comparison;
  a set mixing costs 10 and 12, a cost of 9, and a cost of 15 are rejected at validation by name;
  with `maxConcurrent: 1` and a comparer that blocks, a second request is `429` without a comparison.
- Response uniformity: every `401` reason produces the identical status, `WWW-Authenticate`, `code`, and message;
  a test iterates the reason table and diffs the responses.
- OIDC verification, against an `httptest` JWKS and tokens minted with test keys:
  each check in section 4.3 fails alone and answers `401` with its reason;
  `alg: none` and `alg: HS256` are rejected before the key lookup, proven by a fetcher that fails the test if called;
  a token without `kid` verifies when one compatible key is held and is rejected when two are;
  an `ES256` token against a P-384 key, and an `RS256` token against an EC key, are `signature`;
  `tokenType: access` rejects `typ: JWT` and accepts `typ: at+jwt`;
  `tokenType: id` with two audiences requires `azp`.
- Key sets: an empty set, a set of only 1024-bit RSA keys, and a set with a duplicate `kid` are failed fetches
  that leave the previous set in place; a set with one unusable key among usable ones keeps the usable ones.
- Key refresh: an unknown `kid` triggers one fetch;
  a second unknown `kid` within `jwksRefreshMin` does not, and 100 concurrent ones trigger at most one;
  a failed fetch leaves the old set verifying;
  a rotated key verifies after `Refresh` is driven;
  a set older than `jwksMaxStale` answers `503 auth_unavailable` for a well-formed token,
  still answers `401 alg` for `alg: none` and `401 malformed` for a 17 KiB token,
  and recovers on the next successful fetch;
  a verification in flight during a swap completes against the set it loaded.
- Issuer client: each of the following fails the fetch by name:
  an `http://` issuer, an `http://` `jwks_uri`, a `token_endpoint` with userinfo, a redirect to `http://`,
  a fourth redirect, any redirect from the token endpoint, a body of 1 MiB + 1, and a valid object followed by a second value.
- Mapping: users before groups before default; first group in configuration order wins;
  a string groups claim, an absent claim, and a non-array claim (`401 claim`); no match is `401 no_realm`.
- Sealing: a cookie sealed with the current key opens;
  one sealed with the previous key opens; one sealed with a removed key does not;
  a transaction cookie presented as a session cookie does not open;
  a tampered byte anywhere does not open;
  a session expires at `now + sessionTTL` with the clock faked;
  every cookie attribute in sections 6.5 and 6.6 is asserted on the `Set-Cookie` header;
  a `net/http/cookiejar` holds no transaction cookie after a successful callback or an invalid transaction,
  holds exactly the session cookie after a successful callback,
  and holds no session cookie after an expired session or a logout;
  a principal containing a NUL byte is rejected at verification, and a plaintext with trailing bytes does not open.
- Wire values: with a fixed random source, `state`, `nonce`, `code_verifier`, and `code_challenge` equal known vectors;
  the verifier is 43 characters of the base64url alphabet;
  a random source that fails answers `503` with reason `entropy`.
- Return path: `/\evil.example`, `//evil.example`, `https://evil.example/`, `%2F%2Fevil`, `/a%5Cb`,
  a path with a `0x0a`, a 2 KiB path, and `javascript:` each become `/`;
  `/v1/x?seconds=5#frag` seals as `/v1/x?seconds=5`.
- Key rotation: with two pollers over two fake files,
  the staged procedure of section 6.3 leaves every cookie openable at every step;
  a one-step swap is shown to lose a session, and that test documents why the procedure is staged.
- Browser flow, against an `httptest` issuer:
  the callback rejects a wrong `state`, an expired transaction, a `nonce` mismatch, an `error` parameter,
  a token endpoint that answers `400` (`401 exchange_denied`) and one that answers `500` (`503 exchange`);
  a session-authenticated `GET` with `Sec-Fetch-Site: cross-site`, `same-site`, or absent is `401 csrf`,
  and with `none` or `same-origin` is admitted;
  a navigation without a credential is `302` to `/auth/login` with the path and query in `return`;
  a `fetch`-shaped request is `401`; the `/auth/` routes are `404` without the browser block.
- Configuration: each validation rule in sections 3.2, 3.3, 4.2, 5, 6.1, and 9 rejected by name,
  including `clientID` differing from `audience`, a `redirectURL` with a query, a duplicate scope,
  `anonymousRealm` set under `basic`, a `basic` block under `oidc`, and `browser` with `tokenType: access`.
- File reload: a replaced users file is in effect after one `Poll`;
  an unparseable replacement, and one whose cost differs, leave the previous set and count `failed`.
- Snapshot: a request that started under one user set completes under it after a reload swaps the pointer.

Integration, in `internal/httpapi`:

- The composed order of section 2: an unauthenticated request to an unknown route gets `404`;
  to a not-ready gateway `503 not_ready`; to a PGO route with PGO disabled `501`;
  with `?access_token=` `400` even when a valid bearer is also present;
  to a denied namespace `401`, not `403`.
- The audit line for a `401` carries `auth_reason` and principal `-`;
  for a `302` carries code `auth_redirect`;
  for a successful callback carries the resolved principal.

End to end, under `//go:build e2e`:

- One lane runs `oidc` mode with the browser block against Dex with a static client and static password.
  A headless-browser-free client walks the flow by hand:
  request a profile with navigation headers, follow the `302` to Dex, submit Dex's password form,
  follow the callback, and pull a profile with the resulting cookie and `Sec-Fetch-Site: none`;
  then pull the same profile with an ID token obtained from Dex's password connector as a bearer.
  The lane then rotates Dex's signing key by restarting Dex with a new key,
  and a token signed with the new key verifies within `jwksRefreshMin` without restarting the gateway.
- One lane runs `basic` mode over TLS and pulls a profile with `go tool pprof` and a userinfo URL.

---

## 11. Dependencies

Added to the gateway *Dependencies* table:

| Module | Purpose |
|---|---|
| `github.com/go-jose/go-jose/v4` | JWS parsing and signature verification, JWK set parsing (only in `internal/auth`) |
| `golang.org/x/crypto` | `bcrypt` (only in `internal/auth`; today an indirect dependency, becomes direct) |
| `golang.org/x/term` | reading a password without echo for `profgate auth hash` (only in `cmd/profgate`; today an indirect dependency, becomes direct) |

Discovery, the JWKS cache, the claim checks, the issuer client, the browser flow, and the cookie sealer are written on the standard library:
each is a few dozen lines against a documented wire format,
and each has a rule in this document that no library implements as stated.
`github.com/coreos/go-oidc/v3` was considered and rejected:
its key set refreshes on every signature failure with no cooldown and no periodic refresh,
tries every held key for a token without `kid`,
applies a fixed skew to `nbf` and none to `exp`,
and reads issuer responses without a size limit;
using it would have meant replacing everything but the JOSE layer, which is `go-jose` itself.
Signature verification is the one piece not written here,
because a mistake in it is a full bypass and `go-jose` is the implementation the ecosystem has reviewed.

`internal/auth` will be the only non-test importer of `go-jose` and `x/crypto`,
and `cmd/profgate` the only importer of `x/term`,
each checked by `mise run check` with the same grep shape as client-go and nats.go.
Neither module is in `go.mod` today;
the implementation plan pins the `go-jose` version it reviews, and the dependency table names it then.

---

## 12. Package layout

```text
internal/auth/       Authenticator interface; basic, oidc, and disabled implementations;
                     issuer client, discovery, JWKS cache, claim checks, browser flow, cookie sealer
```

```go
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
}
```

`internal/httpapi` calls the interface where `principalRealm` is called today,
writes `Failure.Reason` to the audit record, and maps `Failure.Status` or `Failure.Redirect` to the response;
the `disabled` mode is the trivial implementation.
The `/auth/` routes are registered by `internal/httpapi` from a handler `internal/auth` provides,
so routing stays in one package.
`cmd/profgate` gains `auth hash`.

---

## 13. Changes to the accepted gateway design

Accepting this document amends the following text in the same change.
Nothing else in the gateway design moves.

| File | Current text | New text |
|---|---|---|
| `docs/specs/gateway.md`, *Core decisions* 7 | "**Authentication is optional and static.** This design defines the `disabled` mode and the authorization structure every mode shares." | "**Authentication is optional and static.** This design defines the `disabled` mode and the authorization structure every mode shares; [`auth.md`](auth.md) defines `basic` and `oidc` on that structure." |
| `docs/specs/gateway.md`, *Non-goals* | "Hot-reloading configuration, Basic Auth, OIDC. Each is designed for in a later revision of this document; PGO collection is designed in [`pgo.md`](pgo.md)." | "Hot-reloading configuration is designed for in a later revision of this document; PGO collection is designed in [`pgo.md`](pgo.md) and authentication modes in [`auth.md`](auth.md)." |
| `docs/specs/gateway.md`, *Network* | (required flows) | add "gateway → OpenID Connect issuer, HTTPS, when `auth.mode` is `oidc` ([`auth.md`](auth.md)); `deploy/` ships a commented egress rule for it" |
| `docs/specs/gateway.md`, *Container* | (read-only mounts) | add: when `auth.basic.usersFile`, `auth.oidc.caFile`, `auth.oidc.browser.clientSecretFile`, or `auth.oidc.browser.cookieKeyFile` is configured, a Secret volume at `/etc/profgate/auth/`, mounted `readOnly: true` with `defaultMode: 0440`, under the same `fsGroup`; the kubelet mounts it and the RBAC table is unchanged |
| `docs/specs/gateway.md`, *What a compromised gateway can do* | (paragraph) | add: "Under `basic` it holds bcrypt hashes, not passwords. Under `oidc` it holds the issuer's public keys, the cookie key, and, if configured, a client secret; with those it can mint a session cookie for any principal and realm it already serves, which is no more than it can already do by ignoring authentication. It holds no refresh token and cannot obtain a token from the issuer on its own." |
| `docs/specs/gateway.md`, *HTTP API* | "All paths are under `/v1` on the API listener." | "All paths are under `/v1` on the API listener, except the three `/auth/` routes that [`auth.md`](auth.md) adds when its browser flow is configured." |
| `docs/specs/gateway.md`, *Request algorithm* step 4 | "**Authentication.** Resolve the principal (section 7.1)." | two steps: "**Credential placement.** `access_token` as a query parameter → `400 invalid_parameter`." then "**Authentication.** Resolve the principal and its realm per `auth.mode` → `401 unauthenticated`, `429 too_many_auth`, `503 auth_unavailable`, or a `302` to login ([`auth.md`](auth.md))."; later steps renumber |
| `docs/specs/gateway.md`, *Errors* table | (401 absent) | add `401 \| unauthenticated`, `429 \| too_many_auth`, and `503 \| auth_unavailable` |
| `docs/specs/gateway.md`, *Authentication and Authorization* | "Both are static process configuration (section 10), never runtime state." | "Both are static process configuration (section 10), never runtime state; [`auth.md`](auth.md) keeps that true for its two modes, whose only runtime-acquired trust state is the issuer's public keys; users, mappings, secrets, and cookie keys are configuration-derived snapshots." |
| `docs/specs/gateway.md`, *Principals and realms* | "Later modes (Basic, OIDC) add a principal → realm mapping step and change nothing below it." | "[`auth.md`](auth.md) defines the `basic` and `oidc` modes; each resolves a principal and a realm and changes nothing below it." |
| `docs/specs/gateway.md`, *CLI* | (three commands) | add `profgate auth hash` |
| `docs/specs/gateway.md`, *Logging* | (audit fields) | add `auth_reason`, present on authentication failures and redirects; the `/auth/` routes' audit shape |
| `docs/specs/gateway.md`, *Health* | "`/readyz` \| preflight has passed and `HasSynced()` is true" | "`/readyz` \| issuer discovery and the initial key fetch have succeeded when `auth.mode` is `oidc`, preflight has passed, and `HasSynced()` is true" |
| `docs/specs/gateway.md`, *Metrics* | (list) | add the section 7 metrics |
| `docs/specs/gateway.md`, *Startup and shutdown* | (diagram) | add a `[discovering]` state between `[listening]` and `[preflight]`, entered only under `oidc`: fetch discovery and the JWKS, retry with backoff for `auth.oidc.discoveryTimeout`, then exit 1 |
| `docs/specs/gateway.md`, *Layers* | (unit and integration bullets) | add `internal/auth` per section 10; the two end-to-end lanes |
| `docs/specs/gateway.md`, *Configuration* table | `auth.mode` validation `disabled` | the rows in section 9 |
| `docs/specs/gateway.md`, *Build and Deployment* | (`deploy/` bullet) | add: a commented example Secret for `/etc/profgate/auth/`, the Deployment's volume and mount, an egress NetworkPolicy rule to the issuer, and Helm values for each; pinned by the manifest test |
| `docs/specs/gateway.md`, *Dependencies* | (table) | the rows in section 11 |
| `docs/specs/gateway.md`, *Package Layout* | (tree) | add `internal/auth/` |
| `docs/specs/gateway.md`, *Failure Scenarios* | (table) | add: issuer unreachable at startup → retry for `discoveryTimeout`, then exit; JWKS refresh fails while running → previous keys stay in use, warning logged, `failed` counted, `503 auth_unavailable` after `jwksMaxStale`; issuer rotates keys → verified within `jwksRefreshMin` of the first token naming the new `kid`; token endpoint down → browser logins answer `503 auth_unavailable`, existing sessions and bearer tokens unaffected; users file or cookie key file unreadable while running → previous contents stay in use |
| `.agents/rules/800-security-invariant.md`, *Two Mechanisms* | (two importer greps) | add `internal/auth` as the only importer of `go-jose` and `x/crypto` |
| `.agents/rules/100-project-map.md`, *Planned Structure* | (tree) | add `internal/auth/` |
| `.agents/rules/100-project-map.md`, *External HTTP API* | (route list) | add the three `/auth/` routes, present only with the browser block |
| `AGENTS.md`, *Two Specs, One Accepted* | (two specs) | rename to *Three Specs* and add [`docs/specs/auth.md`](auth.md): authentication modes layered on the gateway's realm step |
| `docs/specs/gateway.md`, *Configuration* table | (`Reload` column) | `hot` marks the fields a future reloader may replace without a restart; no reloader ships, and the users file and the cookie key file are the only values re-read while running |
| `docs/specs/gateway.md`, *Logging* | (`auth_reason` values) | add `internal`: the authenticator returned an unclassified error, answered `503 auth_unavailable` |
| `docs/specs/gateway.md`, *Dependencies* | (table) | add `golang.org/x/term`, imported only by `cmd/profgate` for `auth hash`; `.agents/rules/800-security-invariant.md` gains that importer grep |
