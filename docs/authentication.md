# Authentication

Profgate attributes every request to a principal and maps it to exactly one realm
([api.md](api.md#authentication) covers the request-level view,
[configuration.md](configuration.md#auth) the field reference,
and [specs/auth.md](specs/auth.md) the full design).
Every identity provider is reached through OpenID Connect discovery,
and the differences between them — claim names, group shapes, token lifetimes — live here rather than in code.
This guide is the working configuration for each issuer the suite runs against or has been checked by hand,
plus two limits worth stating plainly before choosing `oidc` mode.

## Dex

The end-to-end suite runs `oidc` mode with the browser flow against Dex,
because it is a single binary with a static configuration and no database.
Dex's own configuration, in memory storage, one public client, and one static password:

```yaml
issuer: https://dex.example.com:5556
storage:
  type: memory
web:
  https: 0.0.0.0:5556
  tlsCert: /etc/dex/tls/tls.crt
  tlsKey: /etc/dex/tls/tls.key
oauth2:
  skipApprovalScreen: true
  passwordConnector: local
staticClients:
  - id: profgate
    name: profgate
    public: true
    redirectURIs:
      - https://profgate.example.com/auth/callback
enablePasswordDB: true
staticPasswords:
  - email: alice@example.com
    hash: "$2a$10$..."   # bcrypt; profgate auth hash also works, Dex reads any bcrypt hash
    username: alice
    userID: "08a8684b-db88-4b73-90a9-3cd1661f5466"
```

The matching gateway configuration:

```yaml
auth:
  mode: oidc
  oidc:
    issuer: https://dex.example.com:5556
    audience: profgate
    usernameClaim: email
    caFile: /etc/profgate/auth/issuer-ca.crt
    mapping:
      users:
        - name: alice@example.com
          realm: developer
    browser:
      clientID: profgate
      redirectURL: https://profgate.example.com/auth/callback
      scopes: [openid, email]
      cookieKeyFile: /etc/profgate/auth/cookie.key
```

Dex's ID tokens carry the client ID as `aud`, `groups` as a flat array, and `email` and `name`;
there is no `preferred_username`, which is why `usernameClaim: email` is the usual choice.
Dex has no `end_session_endpoint`: `/auth/logout` clears the session cookie and redirects to `/`.

## Keycloak

Keycloak was verified against Keycloak `26.7.2` with the realm export in
[`keycloak-realm.json`](keycloak-realm.json):
a realm `profgate`, a public client `profgate` with PKCE `S256` and direct access grants,
an audience mapper adding `profgate` to the access token,
a "Group Membership" mapper on claim `groups` with "Full group path" off,
groups `engineering` and `payments`, and a user `alice` in `engineering`.
Import it into a Keycloak instance to reproduce the run.

Access tokens carry `aud` only when a client scope adds an audience mapper,
and carry `typ: JWT` rather than `at+jwt` unless the client is configured for RFC 9068;
ID tokens carry the client ID as `aud` by default, so `tokenType: id` is the natural setting.
Groups appear only when a "Group Membership" mapper is added to the client scope,
and appear as full paths (`/engineering/payments`) unless "Full group path" is off, as in the export above.
`preferred_username` is present in both token kinds.
ID and access tokens live 5 minutes by default and the SSO session 30 minutes idle, 10 hours maximum;
a `curl` user should raise the client's token lifespan, or use a client that refreshes on its own
(see [The command-line client](#the-command-line-client) below).
The logout redirect reaches a confirmation page, because the gateway sends no `id_token_hint`;
Keycloak returns to `/` once the user confirms.

## Okta, Entra ID, and Google — unverified

These notes are derived from each provider's published documentation and marked unverified
until someone runs `oidc` mode against a real instance and records what changed.

- **Okta.** ID tokens carry the client ID as `aud`; `groups` requires a claim added to the authorization server.
- **Entra ID.** `aud` on an ID token is the application (client) ID;
  `groups` is present when configured and holds group object IDs, not names, unless the tenant opts into names;
  a user with more than 200 groups gets an overage claim instead, which the gateway does not follow.
- **Google.** ID tokens carry `email` and no groups claim; map users or `auth.oidc.mapping.defaultRealm`.

## The command-line client

Profgate ships no client that acquires a token for `curl` or `go tool pprof`.
Under Keycloak's defaults an ID token lives five minutes, so a `curl` user re-authenticates often;
the fix is a `profgate` command-line client that performs a device-code flow,
keeps the refresh token on the user's own machine, and wraps `go tool pprof`.
That client does not exist yet — it is a separate design with its own document —
and the gateway needs nothing from it to verify tokens another client obtains.
`go tool pprof <url>` in particular fetches with `http.Client.Get` and offers no way to add a header,
so it cannot present a bearer token at all; `basic` mode's userinfo-URL form is what it can use directly
(see [api.md](api.md#basic-mode)).

## Session revocation

A browser session is stateless: once minted, the gateway does not consult the issuer again while it lives.
Disabling a user, an issuer-side logout, or a changed group reaches the gateway at the user's *next* login,
never before — there is no way to revoke a session before its `exp`.
`auth.oidc.browser.sessionTTL` (8 hours by default, 24 at most) is exactly the exposure an operator accepts;
choosing a shorter one is the only lever for narrowing it.
