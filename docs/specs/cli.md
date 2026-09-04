# Profgate Command Line

**Status:** Accepted

This document designs a first-party command-line client for the gateway
([`gateway.md`](gateway.md)):
client verbs in the same `profgate` binary that obtain a token, list what a realm admits,
pull a profile, and drive PGO collection ([`pgo.md`](pgo.md)).
Everything in the gateway design — permission boundary, request algorithm, realms, non-disclosure,
error envelope, configuration — is assumed and not restated,
and so are the token rules of [`auth.md`](auth.md), which defers command-line token acquisition to this document.
Sections of this document and of the other specs are cited by heading name.

---

## 1. Overview

Under `oidc` the gateway accepts one credential from a script:
a bearer token in the `Authorization` header, and obtaining one is left to whoever calls it.
`curl` carries a token it is given but cannot obtain one,
`go tool pprof` cannot set a header at all ([`auth.md`](auth.md) *Core decisions*),
and under Keycloak's defaults an ID token lives five minutes,
so a person working from a terminal re-authenticates roughly once per profile.

The client closes that gap and the two next to it:
a caller who does not know what their realm admits,
and a caller who cannot read the API's shape from `curl` output.
It obtains a token through the device authorization grant (RFC 8628),
keeps it in a per-context cache bound to the gateway it was obtained for,
refreshes it while the issuer allows,
attaches it to every request over TLS, or over loopback plaintext for a port-forward,
and prints the gateway's answers as tables or as the gateway's own JSON.

### 1.1 Core decisions

1. **One binary, client verbs at the top level.**
   `profgate profile`, not `profgate client profile`.
   The operator verbs — `serve`, `config validate`, `auth hash`, `version` —
   share a dispatcher, a `flag` idiom, and a version string with the client verbs,
   and a person who has the gateway image already has the client (*Command grammar*).
2. **The client is a client of the API, not a second API.**
   Each read verb maps to exactly one route and prints that route's response body,
   verbatim under `--output json`.
   It invents no field, merges no two responses, and pre-validates nothing the gateway validates:
   `--seconds` above `limits.cpuSeconds` is sent and answered `400 seconds_exceeds_limit`,
   a port outside `allowedSelections` is sent and answered `400 port_not_allowed`.
   A client that judged those locally would drift from the gateway that actually decides.
3. **The token is obtained by the client and verified by the gateway.**
   The gateway's trust model does not change: it verifies a JWT the issuer signed,
   exactly as it does for a token from any other source ([`auth.md`](auth.md) *Token verification*).
   The client holds a refresh token, which the gateway deliberately never holds.
4. **The client learns the issuer from the gateway.**
   A new unauthenticated route, `GET /v1/auth`, reports the mode and,
   where an operator configured a command-line client, the issuer, the client identifier,
   the token type, the scopes, and whether the issuer takes PKCE on its device endpoint
   (*Gateway discovery*).
   Without it every user would copy those values out of the gateway's configuration by hand,
   and a mismatch would show as a `401` whose body says nothing.
5. **A credential is bound to one gateway and travels only over TLS.**
   Every cache entry records the gateway origin it was obtained for and is used for that origin alone
   (*The token cache*);
   a credential is attached only to an `https://` URL, or to a loopback `http://` URL,
   which is the port-forward case (*Transport*).
6. **Credentials never appear on a command line or in a URL.**
   There is no `--token <value>` and no `--password <value>`:
   an argument lands in shell history, in `ps`, and in CI logs.
   A token arrives from `--token-file`, `--token-stdin`, or `PROFGATE_TOKEN`;
   a password from a prompt without echo or `PROFGATE_PASSWORD`.
7. **The cache is a file, not a daemon.**
   No agent, no keyring integration, no background refresh:
   one JSON file per context, `0600` inside a `0700` directory,
   replaced atomically under a lock (*The token cache*).
8. **Rendering stays with `go tool pprof`.**
   `--open` writes the profile and runs `go tool pprof -http=:0` on it; the client draws nothing.
9. **Fail loud, and print the gateway's words.**
   An error prints the envelope's `code` and `error` unchanged,
   and under `--output json` the envelope's own bytes go to stdout beside that line;
   a response that is not envelope-shaped prints its status and nothing the response carried.
   Exit codes separate "the gateway refused" from "you typed it wrong" from "log in"
   (*Output and exit codes*).

### 1.2 Non-goals

- Rendering profiles: flamegraphs, call graphs, diffs, top tables.
  `go tool pprof` renders them and is already the tool every user of this project has.
- Packaging as a `kubectl` plugin — a second binary name for the same code,
  which is a distribution decision and stays out until a release process would publish it.
- Shell completion beyond what `flag` gives, which is nothing:
  hand-written scripts for four shells would be a second description of the grammar that drifts.
- Port-forwarding.
  `kubectl port-forward` already does it, and doing it here would need Kubernetes credentials
  and a `pods/portforward` capability this project does not have and will not acquire.
- Discovering gateways.
  The server URL is configuration; there is no cluster scan and no well-known name.
- Any change to how the gateway authenticates or authorizes a request.
- Writing profiles anywhere but a file the user named or a temporary file it deletes:
  no profile store, no history, no cache of profile bytes.
- Interactive selection menus.
  `namespaces`, `services`, and `targets` print lists; the shell composes them.

---

## 2. Command grammar

```text
profgate [global flags] <verb> [<subverb>] [<positional>...] [flags]
```

Standard-library `flag` with the hand-written dispatch the binary uses today (gateway *CLI*).
`flag` stops parsing at the first argument that is not a flag,
which is what makes the two flag positions work without a parser of our own:
the global set stops at the verb, and the verb's set starts after the verb's positionals.
The global flags are the ones *Resolution* lists —
they describe which gateway is being talked to, not what is being asked of it —
and each client verb registers them again alongside its own,
so `--server` is accepted in either position and the later occurrence wins.
An operator command line accepts none of them (*Help*).
Every other flag belongs to one verb.
The rest of the grammar is fixed rather than discovered:
**each verb declares how many positionals it takes,
the dispatcher removes exactly that many from the front of its arguments,
and everything after them goes to `FlagSet.Parse`.**
A verb's own flags therefore follow its positionals.
`profgate profile payments/checkout cpu --seconds 30` parses;
`profgate profile --seconds 30 payments/checkout cpu` is a usage error that prints the verb's grammar.
A verb given too few positionals, or a positional that does not match its grammar, is the same usage error.

A Service is addressed as `<namespace>/<service>`, in the order the route spells it.
With `namespace` set in the current context, a bare `<service>` means that namespace;
without it, a bare `<service>` is a usage error naming the flag and the context key.

### 2.1 The verbs

| Command | Route | Positionals |
|---|---|---|
| `profgate login` | issuer, then `GET /v1/whoami` | — |
| `profgate logout` | issuer revocation, when published | — |
| `profgate whoami` | `GET /v1/whoami` | — |
| `profgate limits` | `GET /v1/limits` | — |
| `profgate namespaces` | `GET /v1/namespaces` | — |
| `profgate services <namespace>` | `GET /v1/namespaces/{ns}/services` | 1 |
| `profgate targets <ns>/<svc>` | `GET .../targets` | 1 |
| `profgate profile <ns>/<svc> <profile>` | `GET .../profiles/{profile}` | 2 |
| `profgate collect <ns>/<svc>` | `POST .../collections` | 1 |
| `profgate collections <ns>/<svc>` | `GET .../collections` | 1 |
| `profgate collection get <id>` | `GET /v1/collections/{id}` | 1 |
| `profgate collection cancel <id>` | `POST /v1/collections/{id}/cancel` | 1 |
| `profgate download <id>` | `GET /v1/collections/{id}/profile` | 1 |
| `profgate pgo policy get <ns>/<svc>` | `GET .../pgo` | 1 |
| `profgate pgo policy set <ns>/<svc>` | `GET .../pgo` then `PUT .../pgo` | 1 |
| `profgate pgo policy delete <ns>/<svc>` | `GET .../pgo` then `DELETE .../pgo` | 1 |
| `profgate context list\|show\|use\|delete` | — | 0 or 1 |

`collections` lists and `collection` acts on one record,
which is a plural that differs from its singular by one letter and is worth stating rather than hoping is obvious:
the plural takes a Service, the singular takes an identifier, and each rejects the other's argument by grammar.
An unknown verb, an unknown subverb, and an unknown flag each print the usage line and exit 2.
`-h` and `--help` are not among them: where they are a help argument they print help on stdout and exit 0 (*Help*).

### 2.2 Reserved names

The binary's verbs are one namespace with two halves:
operator verbs configure and run a gateway, client verbs talk to one.
**A name belongs to one half permanently.**
A new verb joins the table above in the change that adds it,
and a name that table already holds is never reused for the other half.
The operator half is `serve`, `collector`, `version`, `config`, and `auth`;
the collector process is `collector`, a noun, so the client verb `collect` stands.
That rule is why a user's own login is `login` rather than `auth login`,
and why the client's file is reached through `context` rather than `config set-context`:
`auth hash` mints a bcrypt hash for `auth.basic.users` and `config validate` reads a gateway configuration file,
and reusing either name would damage the meaning it already carries.

### 2.3 Help

`-h` and `--help` print help on stdout and exit 0.
The bare binary prints the usage line and the global flags.
A leaf command line prints its grammar line and its flags.
A group — a name that takes a subverb — prints its grammar line and the subverbs it takes.

One shape covers every command line the binary has:
the bare binary,
the twenty client leaves of *The verbs* — its rows, with the `context` row's four subverbs counted on their own —
the four client groups `collection`, `pgo`, `pgo policy`, and `context`,
and the operator command lines of *Reserved names*.
A client command line prints the global flags of *Resolution* beside its own,
because a client verb accepts those in either position.
An operator command line prints only the flags it accepts and no global flag, because it accepts none:
the dispatcher hands an operator name to its own function before the global flag set exists,
and an operator name behind a global flag is a usage error naming that rule.

| Operator command line | Help prints |
|---|---|
| `profgate serve` | the grammar line and `--config <path>` |
| `profgate version` | the grammar line; the command takes no flag |
| `profgate config` | the grammar line and its one subverb, `validate` |
| `profgate config validate` | the grammar line and `--config <path>` |
| `profgate auth` | the grammar line and its one subverb, `hash` |
| `profgate auth hash` | the grammar line; the command takes no flag |
| `profgate collector` | no grammar line: the bare binary's help beside a help argument, and exit 2 without one |

`collector` names no implementation yet (*Reserved names*), so it has no grammar line to print;
the bare binary's help is the answer any name the binary does not run gets.
The operator command lines are in this rule because they are in this binary:
a user meets one command line, not two halves,
and a `--help` that answers `profgate whoami` and not `profgate serve` reads as a broken binary.
`profgate auth hash --help` prints help and reads no password,
because help is answered before the verb runs and the prompt is inside the verb.

**Help wins over every other reading of the command line.**
The dispatcher looks for a help argument before it strips positionals and before any flag set parses.
An argument is that one when its name — what follows one or two leading dashes, up to an `=` — is `h` or `help`,
which is the rule `flag` itself applies:
`-h`, `--h`, `-help`, `--help`, and `--help=<anything>` are one flag to it and have to be one here.
The page printed is the deepest verb and subverb the line names, wherever the help argument sits,
so `profgate --help profile` and `profgate profile --help` are the same request.
`profgate profile --help` therefore prints the `profile` grammar,
not the too-few-positionals error of *Command grammar*,
and `profgate collection --help` prints the `collection` group, not the missing-subverb error.
A line whose verb, or whose subverb, is not one the binary has prints the bare binary's help and exits 0:
help answers the whole line, and a name nobody recognizes is what a person asks for help about.

**The search stops at a bare `--`**, because `flag` stops there,
and a line the search and the flag set read differently is a line nobody can predict.
The cost of the search is that a help spelling cannot be handed to a flag as a separated value:
`--file --help` is help, because the search runs before any flag set sees the line.
The attached form reaches it, because the name before the `=` is `file` and not `help`,
so `--file=--help` sends a file named `--help` and `-o=--help` writes to one.
`flag.ErrHelp` from any flag set is the same answer,
so a line the search does not reach still prints help rather than `flag: help requested` with exit 2.

| Command line | Answer |
|---|---|
| `profgate` | the usage line on stderr, exit 2: a bare line asks for nothing, and help is what a help argument asks for |
| `profgate --help` | the bare binary's help on stdout, exit 0 |
| `profgate --help profile`, `profgate profile --help` | the `profile` help on stdout, exit 0 |
| `profgate profile payments/checkout cpu --help` | the `profile` help on stdout, exit 0, and no request |
| `profgate collect payments/checkout --duration 30s --help` | the `collect` help on stdout, exit 0, and no request |
| `profgate --help=0` | the bare binary's help on stdout, exit 0; the value is not read |
| `profgate collect payments/checkout -- --help` | not help: one argument too many, a usage error on stderr, exit 2 |
| `profgate collect payments/checkout --file=--help` | not help: `--file` receives the path `--help` |
| `profgate frobnicate --help` | the bare binary's help on stdout, exit 0 |

Help goes to stdout because it is what the command asked for,
and a usage error goes to stderr with exit 2 because something else was asked for and could not be given.
That split is what lets `profgate collect --help | grep retention` work
while a mistyped flag still leaves stdout empty for the shell that was going to consume it.

---

## 3. Contexts

### 3.1 The file

```yaml
# $XDG_CONFIG_HOME/profgate/config.yaml
currentContext: prod
contexts:
  prod:
    server: https://profgate.example
    caFile: /home/alice/corp-ca.crt
    serverName: profgate.example
    namespace: payments
    auth:
      mode: oidc
      issuer: https://keycloak.example/realms/engineering
      clientID: profgate
      tokenType: id
      scopes: [openid, offline_access]
      pkce: true
```

`$XDG_CONFIG_HOME` defaults to `~/.config` when unset or not absolute, per the XDG Base Directory Specification.
The file is created by `profgate login` and by `profgate context use` and is otherwise the user's to edit;
an unknown key at any level is an error naming the key, the same strictness the gateway applies to its own file
(gateway *Configuration*).
It goes through the same atomic-write helper as the token cache,
with mode `0600` in a directory created `0700`,
because it names internal hostnames and an issuer, and because the tokens beside it demand that anyway.

The `auth` block is the snapshot `GET /v1/auth` returned at the last login (*Gateway discovery*).
**Normal commands act on that snapshot and never read `/v1/auth`; only `login` refreshes it.**
A gateway whose operator changes mode — `oidc` to `basic`, or one issuer to another —
therefore answers `401` until the user logs in again, which is the round trip that rewrites the block.
That is the cost of one route per command,
paid once per reconfiguration rather than on every operation a per-command `/v1/auth` fetch would tax.
The block is also what lets a gateway that does not serve `/v1/auth` be used at all, written by hand.

| Key | Meaning | Validation |
|---|---|---|
| `currentContext` | the context used when `--context` is absent | names an entry in `contexts` |
| `contexts.<name>` | one gateway | `<name>` is a DNS-1123 label (*The token cache*) |
| `server` | base URL | absolute `https://`, or `http://` under *Transport*; no userinfo, query, or fragment |
| `caFile` | extra certificates to trust | a readable PEM file with at least one certificate |
| `serverName` | TLS server name | a DNS name; `tls.Config.ServerName` only |
| `namespace` | default namespace for a bare `<service>` | DNS-1123 label |
| `auth.mode` | `disabled`, `basic`, or `oidc` | as `/v1/auth` reports |
| `auth.issuer` | issuer URL | `https://`, required when `mode` is `oidc` |
| `auth.clientID` | the client the device grant names | 1–256 bytes |
| `auth.tokenType` | `id` or `access` | which token of the response is presented |
| `auth.scopes` | scopes requested | each 1–64 bytes; must contain `openid` |
| `auth.pkce` | send PKCE on the device endpoint | boolean; default `false` |
| `auth.issuerCAFile` | extra certificates to trust when reaching the issuer | a readable PEM file with at least one certificate |

### 3.2 Resolution

`--context`, or `PROFGATE_CONTEXT` when the flag is absent, selects which context applies;
with neither, `currentContext` does.
Selection happens first,
and every other value is then resolved against the selected context in this order,
each step overriding the one before:

```text
built-in default  <  context file  <  environment  <  command-line flag
```

A context file is a durable statement about one named gateway;
an environment variable is a process-local override of it — a CI job, a container,
one shell that exports `PROFGATE_SERVER` for one task — which is why those variables exist at all.
An order that let the file win would make `PROFGATE_SERVER`, `PROFGATE_CA_FILE`, `PROFGATE_SERVER_NAME`,
and `PROFGATE_NAMESPACE` silently do nothing whenever the selected context set the same key.
A flag beats both because it is the most local statement there is.
Overriding is field by field:
`PROFGATE_NAMESPACE` replaces the selected context's `namespace` and leaves its `server` alone.

| Flag | Environment | Context key |
|---|---|---|
| `--server` | `PROFGATE_SERVER` | `server` |
| `--context` | `PROFGATE_CONTEXT` | — (selects the context) |
| `--ca-file` | `PROFGATE_CA_FILE` | `caFile` |
| `--issuer-ca-file` | `PROFGATE_ISSUER_CA_FILE` | `auth.issuerCAFile` |
| `--server-name` | `PROFGATE_SERVER_NAME` | `serverName` |
| `--namespace` | `PROFGATE_NAMESPACE` | `namespace` |
| `--output` | `PROFGATE_OUTPUT` | — |

Because a flag or a variable can move a named context to another gateway,
the token cache is bound to the resolved gateway and not to the context name (*The token cache*).

`--server` without a context is a complete configuration:
a CI job passes `--server` and `PROFGATE_TOKEN` and never writes a file.
A gateway no context describes gets its own cache entry named `adhoc-<hex>.json`,
where `<hex>` is the first 32 hex characters of the SHA-256 of its canonical origin.
A digest rather than a sanitized authority is what keeps `https://a.example:8443` and
`https://a-example:8443` in separate entries, and what makes an IPv6 authority nameable at all.

`profgate context list` prints the contexts and marks the current one;
`context show [<name>]` prints one, with no token material in it;
`context use <name>` sets `currentContext`;
`context delete <name>` removes the entry and its token cache file.

### 3.3 Transport

The client trusts the system pool, plus the certificates in `--ca-file` when given.
`--server-name` sets `tls.Config.ServerName` and nothing else:
the `Host` header stays the URL's authority, and the URL stays what the user typed.
That combination is what makes a port-forward work against a certificate issued for a DNS name —
the case [`api.md`](../api.md) covers today with `curl --resolve`:

```sh
kubectl -n profgate port-forward svc/profgate 8443:8443
profgate --server https://localhost:8443 --server-name profgate.example --ca-file ca.crt whoami
```

**There is no flag that skips verification.**
The gateway is a credential-bearing endpoint,
and a client that can be told to trust anything is a client whose token can be collected by anything on the path.
A self-signed gateway is reached with `--ca-file` naming its own certificate,
which is one extra file and no loss of verification.
Minimum TLS version is 1.2, matching the gateway's own default.

**Plaintext carries no credential.**
That refusal is worth nothing over `http://`, where there is no verification to skip,
so the rule is stated on the credential rather than on the certificate:
**a bearer token, a `PROFGATE_TOKEN`, and a `basic` password are attached to an `https://` URL only.**
One exception exists, and it is the case this design already documents:
a `server` whose host is literally `127.0.0.1`, `::1`, or `localhost` may be `http://`,
which is `kubectl port-forward` in front of a gateway serving plain HTTP,
where the bytes cross a loopback interface and a local tunnel and no network.
The client prints one warning line on stderr naming the URL each time it sends a credential that way.
The check reads the host exactly as the URL spells it and resolves nothing:
a name that happens to resolve to `127.0.0.1` today is not loopback for this rule,
because what the rule needs to know is where the bytes go, and resolution can change under it.
Attaching a credential to any other `http://` URL exits 2 before the request is built,
with a message naming the URL and the `https://` requirement.
The refusal is on the credential, so a command with none to attach —
every command against a gateway in `disabled` mode — reaches a plain-HTTP server anywhere.

---

## 4. Authentication

### 4.1 What the client must obtain

Under `oidc` the gateway admits exactly the tokens *Token verification* in [`auth.md`](auth.md) admits,
and its `tokenType` decides which identity-provider registration a command-line client can use.

**Under `tokenType: id`, one public registration, shared with the gateway's audience.**
OpenID Connect requires an ID token's `aud` to contain the client it was requested for;
the gateway requires `aud` to contain `auth.oidc.audience`,
and requires `azp` to equal it whenever `aud` carries more than one value.
A separate command-line client — `profgate-cli` with a mapper adding `profgate` to `aud` —
produces exactly that multi-valued `aud`, with `azp: profgate-cli`, and is refused.
So the client's `client_id` must equal `auth.oidc.audience`: one registration, and a public one,
because the device grant sends no client secret,
and no single registration is confidential for a browser and public for a device flow at the same time.
An ID-token deployment that also runs the browser flow therefore shares one public client with it.
A separate command-line client here would need an accepted change to [`auth.md`](auth.md)
that configures and verifies the authorized party; this document proposes none.

**Under `tokenType: access`, a separate public registration works.**
RFC 9068 applies and there is no `azp` check,
so a public `profgate-cli` whose audience mapper adds `auth.oidc.audience` is admitted,
which is what the mapper in [`keycloak-realm.json`](../keycloak-realm.json) already does.

The gateway enforces the first case at startup rather than at a first login:
`auth.oidc.cli.clientID` must equal `auth.oidc.audience` while `tokenType` is `id` (*Configuration*).

**Expiry.**
The client presents the `id_token` under `tokenType: id` and the `access_token` under `tokenType: access`,
and learns each one's lifetime differently.
`expires_in` is the access token's lifetime and nothing else (RFC 6749),
so it sets `expiresAt` under `tokenType: access` and is not used under `tokenType: id`,
where applying it would refresh too early or send an expired token,
depending on which lifetime the issuer made longer.
Under `tokenType: id` the client reads the token's own `exp`:
base64url-decode the second dot-separated segment of a token of at most 16 KiB,
parse it as JSON, take `exp` as a number of seconds.
**That decode is not a verification and grants nothing.**
The client reads one number to decide when to refresh;
the gateway verifies the signature, the issuer, the audience, and every temporal claim.
A payload that does not decode, does not parse, or carries no numeric `exp` is unusable:
the token is not cached and not sent, and the command exits 3 saying so.
Reading a claim this way needs no JOSE library and no key (*Dependencies*).

### 4.2 Gateway discovery

```http
GET /v1/auth
```

```json
{
  "mode": "oidc",
  "oidc": {
    "issuer": "https://keycloak.example/realms/engineering",
    "clientID": "profgate",
    "tokenType": "id",
    "scopes": ["openid", "offline_access"],
    "pkce": true
  }
}
```

The body has four shapes.
Under `basic` it is `{"mode": "basic"}` and under `disabled` `{"mode": "disabled"}`.
Under `oidc` it is `{"mode": "oidc"}`, carrying the `oidc` object
**only when `auth.oidc.cli` is configured** —
the operator's statement that this gateway's issuer admits a device login (*Configuration*).
`clientID` is `auth.oidc.cli.clientID`, defaulting to `auth.oidc.audience`;
`scopes` is `auth.oidc.cli.scopes`, defaulting to `["openid", "offline_access"]`;
`pkce` is `auth.oidc.cli.pkce`.
None is derived from `auth.oidc.browser`:
a browser registration and a device-flow registration are the same one under `tokenType: id`
and may differ under `tokenType: access`, and neither fact follows from the browser block's presence.

**This is the one `/v1` route with no authentication step.**
It is the route a client reads *before* it has a credential;
requiring one would make it answer only callers who no longer need it.
Everything else in the gateway *Request algorithm* applies unchanged:
`GET` only (`405 method_not_allowed` with `Allow: GET`),
`503 not_ready` while the gateway is not ready, like every other `/v1` route,
`400 invalid_parameter` for any query parameter, and `Cache-Control: no-store`.
It has no realm step, because it names nothing a realm bounds.
It writes no audit record, for the same reason `/ui/` writes none ([`ui.md`](ui.md) *Logging* row),
and is counted in `profgate_requests_total` with `endpoint` value `auth`.
It is a `/v1` route and not a member of the `/auth/` family of [`auth.md`](auth.md) *The `/auth/` routes*,
which exists only with the browser block and performs a login; this one only describes.

**What it discloses.**
Under `basic` and `disabled` it publishes the mode,
which the `WWW-Authenticate` header on every `401` already names.
Under `oidc` with a command-line client configured it publishes four more values:
an issuer URL, a public client identifier, a token type, and scope names.
A deployment running the browser flow already hands all four to an unauthenticated caller,
because `/auth/login` answers a `302` carrying them.
**A bearer-only deployment does not, and for it this route is a new unauthenticated disclosure.**
It is accepted on its own terms rather than on that precedent:
an OpenID Connect issuer publishes its own discovery document to the world by design,
a public client identifier cannot by definition be kept secret (RFC 6749),
and no namespace, Service, Pod, realm, principal, or credential appears in the body.
An operator who declines it configures no `auth.oidc.cli` block, and the route reports the mode alone.
That a gateway exists at this address at all is a network property,
already true of `/healthz`, `/ui/`, and every `401`.

### 4.3 `profgate login` under `oidc`

`login` takes `--issuer`, `--client-id`, `--token-type`, `--scope` (repeatable),
`--pkce`, `--no-pkce`, `--issuer-ca-file`, and `--login-timeout`;
each overrides what `/v1/auth` reported, and together they are enough to log in to a gateway
that does not serve it.

1. Read `GET /v1/auth`.
   On `404 route_unknown`, or on a body reporting `oidc` with no `oidc` object,
   fall back to the context's `auth` block, and without one to the flags above;
   with neither, exit 2 naming what to write.
2. Fetch `<issuer>/.well-known/openid-configuration` through the issuer client (*The issuer client*)
   and require the document's `issuer` to equal the configured value byte for byte.
   Record `device_authorization_endpoint`, `token_endpoint`, and `revocation_endpoint` when published.
   An issuer publishing no `device_authorization_endpoint` exits 2 with a message naming the grant
   and pointing at `--token-file`.
3. `POST device_authorization_endpoint` with `client_id` and `scope`.
4. Print, on stderr, the `user_code` and the `verification_uri`,
   and `verification_uri_complete` on its own line when the response carries one.
   **No browser is opened.**
   Printing is what makes the flow work where it is most needed —
   a terminal over SSH, a container, a machine with no graphical session —
   and a user with a browser pastes one line.
5. Poll `token_endpoint` with `grant_type=urn:ietf:params:oauth:grant-type:device_code`,
   `device_code`, and `client_id`, no sooner than every `interval` seconds
   (5 when the response omits `interval`, per RFC 8628).
   Per RFC 8628 the polling errors are handled as:

   | Error | Behavior |
   |---|---|
   | `authorization_pending` | wait one interval and poll again |
   | `slow_down` | add 5 seconds to the interval, then wait and poll again |
   | `access_denied` | stop; exit 1 with "the request was denied at the issuer" |
   | `expired_token` | stop; exit 1 with "the code expired; run profgate login again" |
   | any other `4xx` | stop; exit 1 printing the issuer's `error` value and nothing else from the body |
   | a transport failure or `5xx` | wait one interval and poll again, until the deadline |

   Polling stops at `expires_in` seconds after the device authorization response,
   or at `--login-timeout` (default 10m, 1m–30m), whichever is sooner.
6. Take the token named by `tokenType` from the `200` response.
   A response that lacks it exits 1 naming which token was expected,
   because that is what a client registered without the needed scope produces
   and the message must not be "login failed".
7. Write the cache (*The token cache*), then call `GET /v1/whoami` with the token
   and print the principal and realm.
   A `401` here exits 3 with the two values the token and the gateway disagreed on —
   the issuer and the audience the client asked for — since the gateway's own `401` says nothing.

**PKCE.**
RFC 8628 defines no `code_challenge` for the device grant,
and issuer metadata does not answer whether one is accepted there:
`code_challenge_methods_supported` (RFC 8414) describes the authorization endpoint.
So this is an operator assertion rather than an inference.
`auth.oidc.cli.pkce` defaults to `false`,
because an issuer that rejects parameters it does not recognize would otherwise refuse every login;
an operator whose issuer implements the extension — Keycloak and the pinned Dex both do — sets it `true`,
and `/v1/auth` reports it.
When true, the device authorization request carries `code_challenge` and `code_challenge_method=S256`
and every poll carries the matching `code_verifier`.
The method is always sent explicitly, because an issuer that defaults it defaults to `plain`.
It is all or nothing — sent on both requests or on neither, never decided per request.
The client is a public client either way: the device grant needs no client secret (*Configuration*).

### 4.4 Tokens the client did not obtain

`login` is not the only way to hold a credential, and the alternatives exist for CI and for `basic`.
Each is a credential, so each reaches the gateway only under the rule of *Transport*.

- `--token-file <path>` reads a token from a file, `--token-stdin` from standard input,
  and `PROFGATE_TOKEN` from the environment.
  A token from any of the three is used for that one command and **not** written to the cache:
  the client did not obtain it, does not know when it expires, and cannot refresh it.
  Whitespace at both ends is trimmed; an empty value is a usage error.
- Under `basic`, credentials come from `-u <name>` with a prompt without echo,
  or from `PROFGATE_USER` and `PROFGATE_PASSWORD`.
  `login` under `basic` **stores nothing**:
  it verifies the credential against `GET /v1/whoami`, prints the principal and realm,
  and says which two variables the next command will read.
  Storing a password would be storing a credential with no expiry, which the cache's threat model rejects.
  Two local refusals happen before any request, because the gateway's `401` is deliberately uninformative:
  a user name containing `:` and a password longer than 72 bytes each exit 2 naming the rule
  ([`auth.md`](auth.md) *Credential*).
- Under `disabled`, `login` prints that the gateway authenticates nobody and exits 0,
  and every command sends no credential.

The order for one request is: `--token-file` or `--token-stdin`, then `PROFGATE_TOKEN`,
then the cached token for the context, then, under `basic`, the user name and password, then nothing.

### 4.5 The token cache

```text
$XDG_STATE_HOME/profgate/tokens/<name>.json
```

`$XDG_STATE_HOME` defaults to `~/.local/state`.
The directory is created `0700` and each file written `0600`.
Writing is create-a-temporary-file-in-the-same-directory then `os.Rename`,
so a crash mid-write leaves the previous token rather than a truncated one,
and the temporary file is created with the final mode rather than chmod-ed afterwards.

`<name>` is the context name for a named context,
and `adhoc-<hex>` for a gateway named by `--server` alone (*Resolution*).
A context name must be a DNS-1123 label — lowercase alphanumerics and `-`,
starting and ending alphanumeric — and a digest name is a fixed 32 hex characters,
so neither form can name a file outside the directory.
The grammar is checked when the context is resolved, before any path is built;
this is the shape of the return-path rule in [`auth.md`](auth.md) *Wire values and bounds*,
where the value is validated in its parsed form rather than pattern-matched as a string.
Lock files carry a `.lock` suffix, which neither naming scheme can produce.

```json
{
  "origin": "https://profgate.example:443",
  "issuer": "https://keycloak.example/realms/engineering",
  "clientID": "profgate",
  "tokenType": "id",
  "token": "<jwt>",
  "expiresAt": "2026-08-28T09:35:12Z",
  "refreshToken": "<opaque>",
  "refreshExpiresAt": "2026-08-28T19:05:12Z",
  "obtainedAt": "2026-08-28T09:30:12Z"
}
```

**`origin` binds the entry to one gateway.**
It is the canonical origin of the `server` the entry was obtained for:
scheme and host lowercased, an IPv6 host in brackets, and the port always explicit —
`443` for `https://`, `80` for `http://` — with no path, query, fragment, or userinfo.
A cached token is used, and refreshed,
only when the origin the command resolves to equals the entry's `origin` byte for byte;
a different host, a different port, or `http://` against a cached `https://` is a different gateway.

This is the binding a cache keyed by issuer, client, and token type does not provide.
`profgate --context prod --server https://someone-elses.example whoami`
matches all three of those,
and would otherwise hand `prod`'s bearer token to a server whose only job is to replay it against the real gateway.
With `origin` the token is not sent and the command exits 3, naming the context and both origins.
The same comparison keeps `login` from writing a token obtained for one gateway into an entry another reads.
An entry whose `issuer`, `clientID`, or `tokenType` no longer matches the context is unusable for a related reason —
it belongs to a gateway that has been reconfigured —
and both kinds of mismatch are overwritten at the next `login` and never before it.

`expiresAt` is `obtainedAt + expires_in` under `tokenType: access`,
and the token's own `exp` under `tokenType: id` (*What the client must obtain*).
`refreshExpiresAt` is `obtainedAt + refresh_expires_in` when the issuer sends that field, and absent otherwise;
an absent value means the client tries the refresh and believes the issuer's answer.

**Modes are checked, never repaired.**
Before reading or writing, the client requires the tokens directory, and each cache and lock file in it,
to grant nothing to group or other.
A path that grants more exits 2, naming the path and the mode expected.
The client does not `chmod` a file it did not create:
a permission something else widened is a fact it cannot tell from an attack,
and narrowing it silently would hide both.

**One writer at a time.**
Refresh, `login`'s write, and `logout` hold an exclusive lock on `<name>.lock`,
a file created `0600` beside the cache entry and held for the duration of the operation.
The lock is taken before the cache is read, and **the cache is read again after it is acquired**,
so a process that waited acts on what the winner wrote rather than on what it saw before waiting.
A lock not acquired within 30 seconds exits 1 naming the file;
breaking a lock it could not acquire would defeat the serialization the lock exists for.
Without this, two processes read the same rotating refresh token, race at the token endpoint,
and the loser — now holding a token the issuer invalidated when it rotated —
deletes the valid cache the winner just wrote.
An atomic rename prevents a torn file and does nothing about that.

**Refresh.** Before each command the client compares `expiresAt` to now plus 30 seconds.
If the token is still good it is used, and no lock is taken.
Otherwise, with a refresh token, the client takes the lock, re-reads,
and — if the re-read entry is still inside that window —
posts `grant_type=refresh_token` with `client_id` to the token endpoint.
Without a refresh token the command exits 3 with "no valid token for context <name>; run profgate login".
The response decides what the cache becomes:

| Response | Outcome |
|---|---|
| carries the token `tokenType` names | it and its expiry replace the cached token; the command proceeds |
| carries a rotated `refresh_token` | it replaces the stored one, whether or not the selected token was present |
| carries no rotated `refresh_token` | the stored refresh token is kept; an issuer that does not rotate has not revoked |
| carries no token of the selected type | the cached token is kept until its own expiry; past that, exit 3 naming the missing token |
| transport failure, or issuer `5xx` | the cache is left exactly as it was; exit 1 with the transport or status line |
| `invalid_grant`, or another permanent `4xx` | the cache file is deleted; exit 3, "run profgate login" |

A successful refresh returning no ID token is what an issuer configured not to re-issue one produces,
and OpenID Connect permits it.
The token already held stays valid until its own `exp`,
so the command that triggered the refresh still runs, and the next `login` is what recovers.
`tokenType: access` is the setting to recommend against such an issuer,
because an access token is what a refresh grant is defined to return.

Deletion happens under the lock,
and only after the re-read shows the same `obtainedAt` the refused refresh token came from.
Holding the lock should make that comparison always true;
it is made anyway,
because a filesystem that does not honour the lock — a network home directory is the usual one —
degrades to no serialization at all,
and losing a valid cache is the worse of the two failures.

Refresh is what makes Keycloak usable from a terminal.
Its ID and access tokens live five minutes by default ([`auth.md`](auth.md) *Issuer notes*),
so nearly every command after the first would otherwise fail;
with the refresh token the client renews silently until the SSO session's own bound —
30 minutes idle, 10 hours maximum by default — and only then asks for a new login.
Dex issues a refresh token only when `offline_access` is among the scopes,
which is why that scope is in the default `scopes` list.

The token is checked once, before the request is sent.
A token that expires *during* a long `seconds=300` profile is not a problem:
the gateway authenticates at request entry and never re-checks
([`auth.md`](auth.md) *Request algorithm*).

**`profgate logout`** takes the lock, revokes, and deletes the cache file.
When issuer discovery published a `revocation_endpoint` (RFC 7009),
it first posts the refresh token there with `token_type_hint=refresh_token` and `client_id`,
which is how a public client identifies itself on that endpoint.
A failure there is a warning on stderr and does not stop the deletion,
because a local credential outliving a failed revocation is the worse of the two outcomes.
If the deletion itself fails, the command exits 1, says plainly that the credential is still on disk,
and names the file to remove:
the client cannot promise a filesystem operation will succeed, and must not report one that did not.
Logout does not touch the gateway: a bearer token carries no server-side session to end.

### 4.6 The issuer client

Every request the client makes to the issuer — discovery, the device endpoint, the token endpoint, revocation —
goes through one `http.Client` with the rules [`auth.md`](auth.md) *Issuer client* sets for the gateway's:

- `https://` only; discovery follows at most 3 redirects, each to an `https://` URL,
  and the token, device, and revocation endpoints follow none.
- Connect, handshake, and response-header timeouts of 5 seconds; a 10-second deadline per request.
- Response bodies read through a reader limited to 1 MiB + 1 byte, rejecting a body that fills it
  or that carries bytes after the JSON value.
- The system trust pool plus `--issuer-ca-file` when given,
  which is a separate flag from `--ca-file` because the gateway and the issuer are two hosts with two certificates.
- Every recorded endpoint must be an absolute `https://` URL with no userinfo and no fragment.

The client verifies no signature and holds no key set.
The one thing it reads out of a token is the ID token's `exp`, with `encoding/base64` and `encoding/json`,
and it treats nothing in that payload as authorization (*What the client must obtain*).
That is why nothing in this document imports the JOSE library that `internal/auth` owns,
and why the one-importer rule of [`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md)
stays true without an exception.

---

## 5. The verbs

### 5.1 Reading

`whoami`, `limits`, `namespaces`, `services`, and `targets` each issue one `GET` and print the result.
Under `--output json` the response body is copied to stdout byte for byte.
Under the default `--output table`:

```text
$ profgate namespaces
NAMESPACE
orders
payments

$ profgate targets payments/checkout --explain
POD                              NODE       VERSION
checkout-7c8f8c9b9-xabcd         worker-07  1.42.3
checkout-7c8f8c9b9-ylmno         worker-03  1.42.3

selectorMatched  5

REASON                  COUNT
pod_not_ready           2
port_name_not_declared  1

$ profgate whoami
principal  alice
realm      payments-dev
namespaces payments
services   *
profiles   cpu, heap, goroutine
pgo        read
```

Columns are tab-separated when stdout is not a terminal and space-padded when it is,
so a pipe into `cut` behaves and a terminal reads.
An empty list prints its header and nothing else, never "no results",
because the header is what tells a script the request succeeded.

**A namespace the gateway does not know is that same empty list.**
The gateway observes no Namespace objects,
so "the namespace exists and holds no Service" and "the namespace does not exist" are one fact to it,
and the Service list answers `200` with an empty `services` for both ([`ui.md`](ui.md) *The realm filter*).
There is no `namespace_not_found` to surface, and the client invents none:
a not-found decided here would be a judgement the gateway did not make,
which is what *Core decisions* refuses.
`profgate services <typo>` therefore prints its header, prints no row, writes nothing to stderr, and exits 0.
A namespace the caller's realm does not admit is the other answer, `403 realm_denied`,
identical for a namespace that exists and one that does not ([`ui.md`](ui.md) *Errors*).
The client cannot separate a syntactically valid typo from an empty namespace.
`profgate whoami` prints the realm's configured lists as they are written,
which exposes a misspelled explicit entry;
a realm whose list is `*` carries no inventory to check a name against.

`targets` takes `--port` or `--port-name`,
which the gateway needs in order to decide eligibility (gateway *List targets*).
It also takes `--explain`, which sends `explain=true` and prints two things beside the list, each after a blank line:
`selectorMatched`, a key-and-value row naming how many Pods the selector matched, whether or not it is zero,
and then the `excluded` rows under their own header,
one row per reason with its count, in the order the response holds them.
The list keeps its shape for a script, and the count and the reasons read below it.
Under `--output json` the body is copied through as it is for every other reading verb,
`selectorMatched` and `excluded` included.
The flag is the whole addition: the verb still issues one `GET`,
and its positional argument does not change.

### 5.2 `profile`

```sh
profgate profile payments/checkout cpu --seconds 30 -o cpu.pprof
profgate profile payments/checkout heap --open
```

| Flag | Meaning |
|---|---|
| `--seconds` | `seconds` for `cpu` and `trace` |
| `--pod` | `pod` |
| `--version` | `version` |
| `--port`, `--port-name` | the port selection; both given is a usage error before any request |
| `-o <path>` | write here; `-o -` writes to stdout |
| `--open` | run `go tool pprof -http=:0` on the profile |

Every parameter is passed through and judged by the gateway (*Core decisions*).
The client refuses only what it can refuse without guessing the gateway's configuration:
both port flags together, a `--seconds` that is not a positive integer, an unknown profile name
(the eight of gateway *Fetch a profile*, so a typo does not become a `404 profile_unknown` round trip).

Without `-o` and without `--open`, the profile is written to
`<namespace>-<service>-<profile>-<YYYYMMDDTHHMMSSZ>.pprof` in the working directory,
and the path is printed on stderr.
A binary body is never written to a terminal by default; `-o -` is how a user asks for that.

The response's `X-Pprof-Target-Pod`, `-Node`, and `-Version` headers are printed on stderr,
so the user knows which replica answered without a second `targets` call.
Under `--output json` the client prints those three values as a JSON object on stderr
and the profile still goes to the file:
the profile is bytes, not a document, and `--output` describes documents.

**`--open`** resolves `go` with `exec.LookPath` **before the profile is fetched**,
and exits 2 naming `go` and `PATH` when it is absent.
The order matters:
without `-o` the profile lands in a temporary directory this command removes on the way out,
so a message naming a file the cleanup has already deleted would be worse than no file at all,
and refusing before the request means no profile is collected and thrown away.
With `go` present, the client writes the profile into a directory made by `os.MkdirTemp`
(mode `0700`, file `0600`) when `-o` is absent,
runs `go tool pprof -http=:0 <file>` through `exec.CommandContext` with the user's stdio,
waits for it, and removes the directory afterwards.
With `-o` it opens that file and removes nothing.
The viewer is a child process and not a `syscall.Exec` replacement,
precisely so the temporary file can be removed when it exits;
`CommandContext` is what makes a cancelled command stop the viewer before that cleanup runs.
`-http=:0` lets `pprof` choose a free port and print it, which is what makes two viewers coexist.

### 5.3 Collections

```sh
profgate collect payments/checkout --duration 30s --rounds 3 --wait
profgate collections payments/checkout
profgate collection get 7h2k9m4p6r8t0v1w3x5y
profgate collection cancel 7h2k9m4p6r8t0v1w3x5y
profgate download 7h2k9m4p6r8t0v1w3x5y -o merged.pprof
```

`collect` and `collection cancel` send `Content-Type: application/json`,
the cancel with no body, because [`pgo.md`](pgo.md) *Request media type* refuses a `POST` without it.
`collect` builds the request body of [`pgo.md`](pgo.md) *Create a Collection* from
`--duration`, `--rounds`, `--round-interval`, `--replicas`, `--max-parallel`, `--target-version`, and `--retention`;
a flag left unset is absent from the body, so the Service's effective policy decides it.
`--file <path>` sends a JSON file instead and is mutually exclusive with the field flags,
which is how a field this client does not name yet is still sendable.
`pgo policy set` takes the same flag under the same name (*`pgo policy`*), because it is the same concept:
the value is a path, and the field flags build a request body too,
so what distinguishes this way of asking is the file and not the body.
This renames a shipped flag:
`collect --body` is removed and replaced by `collect --file`, no alias is accepted,
and `pgo policy set --file` is unchanged.
The `202` prints the identifier and the state.

**The identifier survives a lost response.**
The create commits before it answers,
so a connection dropped after the write leaves a Collection running and the caller holding no identifier.
A blind retry then answers `429 collection_in_progress`,
which does not say whether the live Collection is the one this command created or one a schedule started,
and carries no identifier to wait on.
Every `collect` therefore generates one UUIDv4 and sends it in an `Idempotency-Key` header,
under the Idempotency-Key contract in [`pgo.md`](pgo.md) *Create a Collection*:
a repeat of the same key answers `200` with the identifier and state of the Collection that key created,
and the `Location` header the first answer carried, from a durable read that no cache stands in front of;
a different key while a Collection is live is `429 collection_in_progress`;
and the same key whose effective policy snapshot has changed is `409 idempotency_mismatch`,
which the same flags can produce after the Service's stored override or the operator defaults moved,
reported with its envelope and exit 1.
The key is generated once per invocation from `crypto/rand` with RFC 9562's version and variant bits set,
which the standard library supplies (*Dependencies*),
and the same key is reused for every retry of that invocation and never afterwards.
`collect` retries the create under that key whenever the result of the create is unknown,
at one second doubling to eight, for at most 30 seconds.
A result is unknown in three ways:
no answer arrived at all, the gateway could not complete one (`5xx`),
or an answer did not arrive whole —
a `202` whose headers arrived and whose body was cut off before the identifier could be read out of it.
The third is the one the header exists for and the one neither of the others covers:
the response is obtained and its body read afterwards,
so a truncated body is neither a transport failure nor a status the gateway chose.
An answer that arrived whole and says something is not retried.
`429 collection_in_progress` is reported and exits 1 without waiting,
because it means a Collection this command did not create holds the Service.
Any other `4xx` prints its envelope and stops.
**`--wait` begins only once a response has carried a concrete identifier**, first answer or replay.
A replay carries the identifier and the state and no more,
because `POST .../collections` is a `pgo.collect` route and the record route is a `pgo.read` one;
`--wait` then polls that record, which a principal holding `collect` alone cannot read.
`collect --wait` under such a realm prints the identifier, reports that the record route is denied, and exits 1;
the Collection it started keeps running.
`SIGINT` before an identifier exists prints that a Collection may already have been created
and names `profgate collections <ns>/<svc>` as the way to find out.

**`--wait`** polls `GET /v1/collections/{id}` every `--poll-interval` (default 2s, 1s–1m)
until the record reaches a terminal state or `--wait-timeout` (default 30m, 1m–24h) elapses.
The terminal states are `completed`, `failed`, `cancelled`, and `expired`;
`initializing`, `pending`, and `running` continue the wait.
`state` is a closed set ([`pgo.md`](pgo.md) *Record*),
so a value outside those seven stops the wait and exits 1 naming the value —
a client that treated an unknown state as non-terminal would poll until its deadline for no reason.
While waiting, a `503 pgo_unavailable` is retried (the record outlives a NATS outage)
and a `404 collection_not_found` ends the wait with exit 1.
Progress lines go to stderr as `round <n> of <n>, <ok> ok, <failed> failed` from the record's `progress`;
the final record goes to stdout.

A wait that ends `completed` exits 0.
One that ends `failed` or `cancelled` exits 1 and prints the record's `reason`,
which [`pgo.md`](pgo.md) *Record* defines for exactly those two states.
One that ends `expired` exits 1 with a fixed message —
the artifact's retention elapsed before it was downloaded —
because an expired record carries no `reason` to print.
A wait that reaches `--wait-timeout` exits 1 naming the identifier.

`SIGINT` during a wait stops the *watching*, not the *collecting*.
The client prints the identifier and the command that resumes watching, and exits 1.
Stopping the work is `collection cancel`.

`collection cancel` retries `409 collection_initializing` once per second for up to 10 seconds,
because that status means "not yet claimable, retry" rather than "no" ([`pgo.md`](pgo.md) *Cancel*);
`409 collection_terminal` is reported as-is and never retried.

`download` streams the artifact to `-o <path>`, or to
`<id>.pprof` in the working directory when `-o` is absent, and honours `-o -`.
`410 artifact_gone` and `409 collection_not_completed` print their envelope and exit 1.

### 5.4 `pgo policy`

`get` prints the body of [`pgo.md`](pgo.md) *Policy* and, under `--output table`,
the effective policy with the source and any violations.

`set` takes `--file <path>` holding the override document — the same flag `collect` takes (*Collections*) —
or the field flags of `collect` plus `--enabled` and `--every`/`--jitter`.
It then:

1. issues `GET .../pgo` and reads its `ETag`;
2. sends `PUT` with `If-Match: <etag>` when the `GET` carried one, and with no `If-Match` at all when it did not,
   which is exactly the create-versus-update distinction of [`pgo.md`](pgo.md) *Policy*;
3. on `412 precondition_failed` or `428 precondition_required`, **reports and stops**.

`412` means the policy changed between the read and the write.
`428` means it was *created* between them: the `GET` found no override, so the `PUT` carried no `If-Match`,
and by the time it arrived another writer had made one.
Both are the same event — the policy is no longer what this command read —
and neither is retried automatically,
because re-reading and retrying would silently overwrite whatever the other writer decided,
which is the exact outcome the precondition exists to prevent.
The message names the command to run again after looking at the current value.
The client never sends `If-Match: *`, which that route answers `400 invalid_parameter`:
the absent-override case sends no header at all.
`delete` follows the same read-then-write shape, treats `412` and `428` the same way,
and reports `404 pgo_override_not_found` as-is.
Every invocation of `set` and `delete` sends at most one modifying request.

---

## 6. Output and exit codes

`--output` takes `table` (default) or `json`, on every verb.
Under `json`, a verb that maps to one route copies that route's body to stdout unchanged;
`collect --wait` prints the final record; `profile` and `download` write bytes to their file
and print their metadata as JSON on stderr.
Nothing is re-encoded, so `jq` sees the API's contract and not this client's idea of it.

Errors go to stderr in one line, under either `--output`:

```text
profgate: seconds_exceeds_limit: effective duration 120s exceeds the limit of 60s
```

The `code` and the `error` string come from the gateway's envelope verbatim (gateway *Errors*).
**Under `--output json` the envelope's own bytes also go to stdout**, copied and not rebuilt,
so `jq .code` reads a refusal exactly as it reads every other answer
and the promise that stdout carries the API's bytes holds for a `400` as much as for a `200`.
The line stays on stderr for the person reading the terminal:
the two streams have two audiences, and the exit code says which happened either way.
Only a gateway refusal has bytes to copy, so stdout stays empty for every other failure —
a transport error and a response that is not envelope-shaped carry no envelope,
a usage error is decided before any request is sent,
and composing one would put a `code` on stdout that no gateway ever sent.

**Envelope-shaped** is the whole test the client applies to an error body:
the response's `Content-Type` is `application/json`,
and the body is one JSON object with a non-empty string `code` and a string `error`,
`details` optional and every other field ignored (gateway *Errors*).
The word is "envelope-shaped" and not "the envelope"
because the client cannot prove that an Ingress did not imitate the gateway's shape;
what it can decide is whether the bytes read like the contract, and it decides only that.
The console applies the same test for the same reason ([`ui.md`](ui.md) *Errors*).

Two answers are the same failure.
One is a non-`2xx` whose body is not envelope-shaped: HTML from an Ingress, an empty body, truncated JSON.
The other is a `2xx` from a route that answers JSON whose body is not exactly one valid JSON document.
Both print one fixed line:

```text
profgate: HTTP 502 Bad Gateway: body is not a profgate JSON document
```

The status and its reason are the whole message;
the reason is the standard text for that status, and is left out for a status that has none.
**No response header, no body byte, and nothing derived from either is printed** —
not the media type, not a length, not a prefix.
A response an Ingress produced was bounded by no realm,
so anything read out of it is a fact the caller's realm never admitted, which is what *Security* refuses,
and it is the rule the console follows for the same reason ([`ui.md`](ui.md) *Errors*).
A `401` in this state exits 3, because a login is still the thing that might fix it;
every other status exits 1.
A `2xx` carrying one JSON document is the answer the verb prints, and not this failure.
A body that fills the client's response bound is a different failure and says so, naming the bound:
those bytes were never read, so there is nothing to classify.
A rejected request — DNS failure, refused connection, TLS failure —
prints the transport error with the URL's scheme, host, and port, and no path.

| Code | Meaning |
|---|---|
| 0 | the command did what it said |
| 1 | the gateway refused, the transport failed, or a waited-for Collection did not complete |
| 2 | usage: an unknown verb or flag, a bad positional, a local validation failure, a missing configuration |
| 3 | authentication is needed: no usable token, a refresh the issuer refused, or `401 unauthenticated` |

A help argument exits 0 on every command line the binary has, because printing help is what that command asked for;
*Help* says what counts as one, and where the search for it stops.
Exit 3 is separate from exit 1 so that a script can re-run `profgate login` on it without parsing a message.
`403 realm_denied` is exit 1, not 3:
a new login changes nothing when the realm is the thing refusing.

`--verbose` adds one stderr line per HTTP request:
method, status, and duration, with the full URL for a gateway request
and the method, host, and endpoint name for an issuer request,
which keeps the issuer lines to the shape the transport-error rule above already uses.
It never prints a header, so no `Authorization` value can reach a terminal or a CI log through it.
No log level prints a token, a password, a refresh token, or a device code.

---

## 7. Security

The client widens nothing in the permission invariant of
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md):
it holds no kubeconfig, reaches no Kubernetes API, and touches no NATS store.
The gateway-side changes it proposes are one unauthenticated read-only route
and three optional configuration keys (*Changes to the accepted designs*);
no Kubernetes verb, no NATS subject, and no realm behavior changes.

**The token cache is the asset.**
The file is a bearer credential for as long as the token lives, and, when the issuer grants one,
a refresh token that is the more valuable of the two because it mints more.
It is protected by file permissions and nothing else:
`0600` inside a `0700` directory keeps it from other users on a shared machine,
and does not keep it from `root`, from a backup that copies `~/.local/state`,
or from any process running as the user — including anything the user installs.
That is the same protection `kubectl` gives a kubeconfig with an embedded token,
and it is stated here rather than implied so nobody assumes more.
What the design does about it: the token's lifetime is the issuer's, not the file's;
the entry names the one gateway origin it may be sent to;
a credential never crosses a plaintext network (*Transport*);
`logout` deletes the file and revokes the refresh token where the issuer offers an endpoint;
and a refresh token is stored only when the issuer chose to grant one,
which an operator declines by leaving `offline_access` out of `auth.oidc.cli.scopes`.

**Non-disclosure holds.**
Every fact the client prints came from a response the caller's realm bounded (gateway *Non-disclosure*).
The client neither infers nor aggregates: it does not scan namespaces to find one that answers,
and it does not turn a `403` into a probe.

---

## 8. Failure scenarios

| Event | Behavior |
|---|---|
| `-h` or `--help` anywhere before a bare `--` | the command's help on stdout, or the bare binary's when the verb is not one; exit 0; no request is sent |
| No context, no `--server` | exit 2 naming `--server` and `profgate context use` |
| Context names a server that does not resolve | exit 1 with the transport error, scheme, host, and port |
| A non-`2xx` whose body is not envelope-shaped, or a JSON route's `2xx` that is not one JSON document | the status and its reason, nothing the response carried, and exit 1 |
| A `401` whose body is not envelope-shaped | the same line, and exit 3: a login is still the thing that might fix it |
| A `2xx` carrying one JSON document | the success path: the verb prints it, and no diagnostic is written |
| A gateway refusal under `--output json` | the envelope's bytes on stdout, the one-line form on stderr, and the exit code the refusal already had |
| `services` for a namespace the gateway does not know | the header, no rows, and exit 0: the gateway answers `200` with an empty list |
| A credential would go to a non-loopback `http://` server | exit 2 before the request, naming the URL and `https://` |
| A credential goes to a loopback `http://` server | sent, with one warning line on stderr |
| Cached entry's `origin` differs from the resolved gateway | the token is not sent; exit 3 naming the context and both origins |
| Gateway now serves `basic`, the context snapshot still says `oidc` | the cached token is sent and answered `401`; exit 3; `login` rewrites the snapshot |
| Gateway serves `basic` and the snapshot agrees | the user name and password path applies; a missing password is exit 2 |
| Gateway serves `oidc`, no cached token | exit 3 before any request is sent |
| Cached token expired, refresh token present | refreshed under the lock; the command proceeds |
| Cached token expired, no refresh token | exit 3, "run profgate login" |
| Refresh fails on transport or issuer `5xx` | the cache is preserved; exit 1 |
| Refresh refused with `invalid_grant` | the cache file is deleted under the lock; exit 3 |
| Refresh returns no token of the selected type | the held token is kept until its own expiry, then exit 3 |
| The cache lock cannot be acquired within 30 seconds | exit 1 naming the lock file |
| The tokens directory or a file in it grants group or other access | exit 2 naming the path and the expected mode |
| `logout` cannot delete the cache file | exit 1 stating that the credential remains, and naming the file |
| `401` from the gateway with a token the client believes is valid | exit 3, printing the issuer and audience the token was obtained for |
| Gateway not ready | `503 not_ready` printed; exit 1; `/v1/auth` answers the same way |
| `/v1/auth` is `404 route_unknown`, or reports `oidc` with no `oidc` object | fall back to the context's `auth` block or the login flags; exit 2 with the keys to write when there is neither |
| Issuer publishes no `device_authorization_endpoint` | exit 2 naming the grant and `--token-file` |
| Issuer unreachable during login | polled until `--login-timeout`, then exit 1 |
| User never approves the device code | `expired_token` at `expires_in`; exit 1 |
| ID token payload does not decode, or has no numeric `exp` | not cached, not sent; exit 3 |
| `go` missing for `--open` | exit 2 naming `go` and `PATH`, before the profile is fetched |
| `-o` path not writable | exit 2 before the request is sent, so no profile is collected and discarded |
| Client disconnects mid-profile (`SIGINT`) | the partial file is removed; the gateway records `client_gone` (gateway *Proxy behavior*) |
| `collect` loses the create response | retried with the same `Idempotency-Key`; the replay carries the identifier and the state for as long as the record exists |
| `collect` retried after the Service's policy moved | `409 idempotency_mismatch`; the envelope is printed and exit 1, because the key would otherwise stand for two different Collections |
| `collect --wait` under a realm with `pgo.collect` and not `pgo.read` | the identifier is printed, the denied record route is reported, and exit 1; the Collection runs on |
| `collect` answered `429 collection_in_progress` | another Collection holds the Service; reported; exit 1 without waiting |
| `SIGINT` during `collect` before an identifier exists | a Collection may exist; `collections <ns>/<svc>` is named; exit 1 |
| `SIGINT` during `--wait` | watching stops, collecting does not; the identifier is printed; exit 1 |
| Collection ends `failed` or `cancelled` under `--wait` | the record's `reason` is printed; exit 1 |
| Collection ends `expired` under `--wait` | the fixed retention message is printed; exit 1 |
| `412` or `428` from `pgo policy set` or `delete` | reported; never retried automatically |
| Two clients share one context file | last write wins on the context file; the token cache is serialized by its lock |

---

## 9. Configuration

The client's own file is *Contexts*.
Three keys are added to the gateway's *Configuration* table, all optional and all about `/v1/auth` only:

| Key | Env | Default | Reload | Validation |
|---|---|---|---|---|
| `auth.oidc.cli.clientID` | `PROFGATE_AUTH_OIDC_CLI_CLIENT_ID` | `auth.oidc.audience` | restart | 1–256 bytes; must equal `auth.oidc.audience` when `tokenType` is `id` |
| `auth.oidc.cli.scopes` | — | `openid, offline_access` | restart | must contain `openid`; each 1–64 bytes of RFC 6749 scope characters; unique |
| `auth.oidc.cli.pkce` | `PROFGATE_AUTH_OIDC_CLI_PKCE` | `false` | restart | boolean |

When `auth.oidc.cli` is configured and `tokenType` is `id`,
`auth.oidc.browser.clientSecretFile` must be unset:
the registration is shared with the browser flow,
and a registration that holds a secret is confidential,
which the device grant sent without a secret cannot use.
Setting both is a validation error naming the two keys.

**The presence of the `auth.oidc.cli` block is what enables device-login discovery.**
An empty `auth.oidc.cli: {}` is valid and enables it with every default above;
without the block, `/v1/auth` reports the mode and no `oidc` object,
and no default is inferred from `auth.oidc.browser` (*Gateway discovery*).
All three keys are a validation error unless `auth.mode` is `oidc`, like every other key in that block.
They change no behavior of the gateway beyond what `/v1/auth` reports:
the gateway does not talk to the issuer's device endpoint and holds no client secret for the command line.
The equality rule on `clientID` is where *What the client must obtain* is enforced,
so an operator who registers a separate command-line client under `tokenType: id` learns it at startup,
and not from a `401` whose body says nothing.

The client is a public client: it sends no client secret, and there is nowhere to configure one.
A device grant needs none — RFC 8628 is defined for public clients —
and a secret shipped in a binary that runs on user machines is not a secret.

**The chart renders the block by default.**
`values.yaml` carries `auth.oidc.cli.enabled`, default `true`,
beside `clientID`, `scopes`, and `pkce` values that render only when set,
so a chart install under `oidc` serves a device login with every default above
and the operator who declines it sets `enabled: false`.
The chart omits the block when `auth.oidc.browser.clientSecretFile` is set and `tokenType` is `id`,
because the binary refuses that pair,
and says so in `NOTES.txt` so the omission is not silent.
The binary's own default is unchanged: without the block in its file, `/v1/auth` reports the mode alone.
Opt-in stays the rule at the binary and becomes the default at the chart,
because the chart is where a fresh install is shaped
and a value the operator can read in `values.yaml` costs less than a block the operator must know to add
(gateway *Build and Deployment*).

---

## 10. Testing

The seams the tests need, all in `internal/client`:

- a clock (`func() time.Time`) and a sleeper (`func(context.Context, time.Duration) error`)
  on the device-grant poller, the refresh window, the create retry, and the `--wait` loop.
  **Every deadline and interval is computed from the injected clock**,
  never from `context.WithTimeout`, `time.After`, or a `time.Ticker`,
  so no test sleeps and none is timing-dependent;
- the `http.RoundTripper` of both the gateway client and the issuer client,
  so a test serves `/v1/auth`, discovery, the device endpoint, and the token endpoint from `httptest`.
  A *refusing* round tripper — one that fails the test if it is called — is what proves each local refusal;
- a file-system root and a write seam
  (`func(dir, name string, data []byte, mode os.FileMode) error`) under the atomic-write helper,
  so a test observes the temporary-file-then-rename sequence and asserts modes in `t.TempDir()`;
- an environment lookup (`func(string) (string, bool)`), so precedence is tested without mutating the process;
- a command runner (`func(ctx, name string, args ...string) error`) and a path lookup
  (`func(string) (string, error)`) for `--open`;
- a random source for the `Idempotency-Key`.

Unit, in `internal/client` and `cmd/profgate`.
The security and recovery cases come first: their absence is a defect rather than a rough edge.

- **Cache binding.**
  An entry is used only when the resolved gateway's canonical origin matches it byte for byte;
  `--context prod --server https://other.example` sends no token and exits 3, against a refusing round tripper;
  `http://host` and `https://host` do not share an entry, and `https://host` and `https://host:443` do;
  an IPv6 authority round-trips through the canonical form and the ad-hoc file name;
  `a.example:8443` and `a-example:8443`, which a sanitizing scheme would collide, get different files;
  a refresh is attempted only for a matching origin.
- **Plaintext refusal.**
  A cached token, a `PROFGATE_TOKEN`, and a `basic` password each exit 2 against `http://gateway.example`
  before the request is built, against a refusing round tripper;
  each is sent against `http://127.0.0.1:8443`, `http://[::1]:8443`, and `http://localhost:8443`
  with a warning on stderr;
  a host name resolving to `127.0.0.1` is refused like any other name;
  a `disabled`-mode command reaches `http://gateway.example` and sends nothing.
- **Token cache.**
  A written cache is `0600` and its directory `0700`;
  the write seam observes a temporary file in the same directory followed by a rename,
  and a concurrent reader sees the old contents or the new, never a partial file;
  a directory or file granting any group or other bit exits 2 naming the path;
  an entry whose `issuer`, `clientID`, or `tokenType` differs from the context is ignored;
  a token more than 30 seconds from expiry is used against a refusing round tripper;
  a token inside that window triggers exactly one refresh.
- **Expiry by token type.**
  Under `access`, `expiresAt` is `obtainedAt + expires_in`;
  under `id`, it is the payload's `exp`, and a disagreeing `expires_in` is ignored;
  a payload that is not base64url, is not JSON, exceeds 16 KiB, or has a non-numeric `exp` exits 3 and caches nothing.
- **Refresh outcomes.**
  A rotated refresh token replaces the stored one, and a response without one keeps it;
  a response carrying a rotation but no token of the selected type stores the rotation,
  keeps the old token until its own expiry, then exits 3;
  a transport failure and a `500` each leave the file byte-identical and exit 1;
  an `invalid_grant` deletes the file and exits 3;
  a file whose `obtainedAt` moved forward is not deleted;
  the request carries `client_id`.
- **Refresh serialization.**
  Two clients over one cache produce exactly one token-endpoint request:
  the second acquires the lock after the first, re-reads, finds a fresh token, and issues none;
  a lock held past the bound exits 1 naming the file;
  `logout` and `login` take the same lock.
- **Logout.**
  Revocation carries `token_type_hint=refresh_token` and `client_id`;
  a revocation failure warns and still deletes;
  a deletion failure exits nonzero with the file named in the message.
- **Idempotent create.**
  A lost response followed by a retry sends the same `Idempotency-Key`,
  and the replay's identifier is what `--wait` polls;
  the replay body carries `id` and `state` and no record fields, asserted against the decoded answer;
  a replay whose snapshot has moved is `409 idempotency_mismatch`, printed and exit 1, with no poll;
  `--wait` against a realm that denies the record route reports the denial and exits 1 after printing the identifier;
  `collect` and `collection cancel` each send `Content-Type: application/json`, the cancel with no body;
  two invocations generate different keys;
  a `5xx` is retried within the window and a `429 collection_in_progress` is not, exiting 1 with no poll;
  an answer whose body is cut off before it decodes is retried under the same key,
  and the retry's replay is what `--wait` polls;
  a `400` stops immediately;
  `--wait` issues no `GET` until an identifier exists;
  cancellation before an identifier names the `collections` command.
- **Precedence.** For each of `server`, `caFile`, `serverName`, `namespace`, and `output`:
  a context value beats the default, an environment value beats the context value, and a flag beats both;
  `PROFGATE_CONTEXT` selects the context, `--context` overrides that selection,
  and a field-level variable then overrides a key inside the selected context.
- **Context file.**
  An unknown key at any level is rejected by name; `currentContext` naming no entry is rejected;
  a name that is not a DNS-1123 label is rejected before any path is built,
  with `../../evil` and an absolute path among the cases;
  the file goes through the atomic-write helper, `0600` in a directory created `0700`.
- **Device grant, on the injected clock.**
  `authorization_pending` then `200` completes after exactly one `interval`;
  `slow_down` raises the interval by 5 seconds and the next poll happens at the raised interval,
  asserted on the clock's advance;
  an absent `interval` polls every 5 seconds;
  `access_denied` and `expired_token` each stop with their own message;
  a `500` is retried and a `400` with an unrecognized `error` stops;
  polling stops at `expires_in` and at `--login-timeout`, whichever is sooner;
  a `200` with no `id_token` under `tokenType: id` exits 1 naming the missing token;
  with `pkce` true the device request carries `code_challenge` and `code_challenge_method=S256`
  and every poll the matching verifier, and with `pkce` false neither parameter is sent;
  the device code, the token, and the refresh token appear in no writer the test supplies.
- **`/v1/auth` fallback.**
  A `404 route_unknown`, and an `oidc` body with no `oidc` object,
  each fall back to the context's `auth` block;
  with no block and no flags the exit is 2 naming the keys.
- **Issuer client.**
  Each rule of *The issuer client* fails a fetch by name:
  an `http://` issuer, an `http://` endpoint in discovery,
  a discovery `issuer` differing from the configured one,
  a redirect from the token endpoint, a fourth redirect, a body of 1 MiB + 1,
  and a valid JSON value followed by a second one.
- **`--wait`, on the injected clock.**
  `pending` → `running` → `completed` exits 0 and polls at the configured interval;
  `failed` and `cancelled` each exit 1 with the record's `reason`;
  `expired` exits 1 with the fixed retention message and reads no `reason`;
  a state outside the seven exits 1 naming the value;
  a `503 pgo_unavailable` mid-poll is retried and a `404` ends the wait;
  the timeout exits 1 naming the identifier;
  a cancelled context prints the identifier and sends no cancel request.
- **`collection cancel`.**
  `409 collection_initializing` is retried at one second up to ten;
  `409 collection_terminal` is not retried.
- **`pgo policy set` and `delete`.**
  A `GET` with an `ETag` sends `If-Match` with that value;
  a `GET` without one sends no `If-Match` header and never `If-Match: *`;
  a concurrent create answering `428`, a concurrent update answering `412`,
  and a concurrent delete answering `412` are each reported and not retried;
  `404 pgo_override_not_found` on an absent delete is reported as-is;
  each invocation issues exactly one modifying request, counted by the round tripper.
- **`--open`.**
  The path lookup runs before any gateway request — a missing `go` produces none,
  against a refusing round tripper — and exits 2 naming `go` and `PATH`;
  the runner receives `go` with `["tool", "pprof", "-http=:0", <path>]` and a context;
  the temporary directory is `0700`, the file `0600`, and both are gone after the runner returns;
  with `-o` the named file survives;
  a cancelled context reaches the runner before the cleanup runs.
- **Grammar.** A table over every verb: the right positional count parses;
  a global flag before the verb, after the positionals, and in both places (the later wins) each resolve;
  one too few and one too many are usage errors;
  a flag placed before a positional is a usage error naming the rule;
  `payments/checkout` resolves, a bare `checkout` resolves only with a context namespace,
  and `a/b/c` is a usage error, as *Command grammar* says;
  an unknown verb, an unknown subverb, and an unknown flag each exit 2.
- **Help.**
  A table over every command line the binary has —
  the bare binary,
  the client leaves of *The verbs* with the `context` subverbs counted on their own,
  the client groups `collection`, `pgo`, `pgo policy`, and `context`,
  and every operator command line: `serve`, `collector`, `version`, `config`, `config validate`, `auth`, `auth hash` —
  asserts that `-h` and `--help` each write help to stdout and exit 0 against a refusing round tripper,
  a leaf printing its grammar line and its flags and a group printing its subverbs,
  with the global flags on every client line and on no operator line;
  `-h`, `--h`, `-help`, `--help`, and `--help=0` are the same request on each of them;
  `profgate --help profile` and `profgate profile --help` print the same page;
  `profgate profile --help` prints help and not the too-few-positionals error,
  and `profgate collection --help` prints help and not the missing-subverb error;
  `--help` after `--` is one argument too many and exits 2, while `--file=--help` reaches the flag as a path;
  an unrecognized verb and an unrecognized subverb print the bare binary's help and exit 0 beside a help argument,
  and exit 2 without one;
  the binary with no argument at all writes the usage line to stderr and exits 2;
  `profgate auth hash --help` prints help against a stdin that fails the test when it is read;
  no output carries `flag: help requested`, and no help path writes to stderr.
- **Reserved names.**
  The dispatcher's client verb set and its operator verb set are disjoint,
  asserted over the two lists rather than by reading the switch.
- **Errors and exit codes.**
  Each envelope prints `code: error`;
  a JSON object without a `code`, and a well-formed envelope under `text/plain`,
  are each not envelope-shaped;
  an HTML body, an empty body, truncated JSON, and a `2xx` that is not JSON each print the same fixed line,
  `HTTP <status> <reason>: body is not a profgate JSON document`, and nothing else,
  asserted against a response whose headers and body a metadata-printing client would have quoted;
  a `401` in that state exits 3 and every other status exits 1,
  while a `200` carrying one JSON document is the success path and prints the body;
  a body that fills the response bound names the bound instead;
  under `--output json` a refusal writes the envelope's bytes to stdout byte for byte and the one line to stderr,
  while a transport failure, a response that is not envelope-shaped, and a usage error each leave stdout empty;
  `401` exits 3, `403 realm_denied` exits 1, a transport failure exits 1, a bad flag exits 2;
  the transport error line carries no path.
- **Basic refusals.**
  A user name with `:` and a password of 73 bytes each exit 2 before any request,
  against a refusing round tripper.
- **TLS options.**
  `--server-name` sets `tls.Config.ServerName` and leaves the `Host` header at the URL's authority;
  `--ca-file` adds to the system pool; a `--ca-file` with no certificate exits 2;
  no verb's flag set contains a flag that disables verification.
- **Output.** For each read verb, `--output json` writes the response body byte for byte;
  the table form of an empty list prints its header only;
  a Service list of `[]` for a namespace prints that header, writes nothing to stderr, and exits 0,
  and a `403 realm_denied` for a namespace the realm denies exits 1;
  a terminal and a pipe produce padded and tab-separated columns respectively.

Unit, in `internal/httpapi`, over `/v1/auth`:
the four body shapes — `basic`, `disabled`, `oidc` without an `auth.oidc.cli` block, and `oidc` with one;
an empty `cli` block yields `clientID` from `audience`, the default scopes, and `pkce` false;
`clientID`, `scopes`, and `pkce` come from the `cli` block when set,
and a configured `auth.oidc.browser` block changes none of the three;
no credential is required and none is read;
`POST` is `405` with `Allow: GET`, a query parameter is `400 invalid_parameter`,
an unready gateway is `503 not_ready`, and the response carries `Cache-Control: no-store`;
the route writes no audit record and records `endpoint` `auth` in the metrics recorder;
the body carries no namespace, Service, Pod, realm, or principal,
asserted against a configuration that has all five.

Unit, in `internal/config`:
`auth.oidc.cli.clientID` differing from `audience` under `tokenType: id` is rejected by name
and accepted under `tokenType: access`;
an empty `cli` block is accepted and a `cli` block under `basic` or `disabled` is rejected;
`scopes` without `openid`, with a duplicate, and with an out-of-range entry are each rejected.

End to end, under `//go:build e2e`, inside the two authentication lanes that already exist
([`auth.md`](auth.md) *Testing*):

- **`basic` over TLS.**
  The lane builds the binary it already builds and runs the client against the port-forwarded gateway with
  `--server https://<name>:<port> --server-name <name> --ca-file <the lane's CA>`,
  which is this design's answer to the `--resolve` shape [`api.md`](../api.md) documents.
  With `PROFGATE_USER` and `PROFGATE_PASSWORD`: `whoami` names the lane's user and realm,
  `namespaces` and `services` list the test namespace and the test app's Service,
  `targets` lists the app's Pods,
  and `profile <ns>/<svc> heap -o <file>` writes a file that `github.com/google/pprof` parses.
  Without the variables, the same command exits 2.
  With a `--ca-file` that does not name the lane's CA, it exits 1 on a TLS failure,
  which is what proves no verification is skipped.
  Against the gateway's `http://` address it exits 2 without sending the password.
- **`oidc` under Dex.**
  `/v1/auth` reports Dex's issuer, the lane's client identifier, `tokenType: id`, and `pkce: true`.
  `profgate login` runs the device grant against Dex and, once the lane approves the user code,
  `whoami` names the Dex user and its mapped realm,
  the cache file is `0600` and carries the gateway's origin,
  a refresh token was cached, and `logout` removes the file.
  The lane approves the code the way its browser walk already submits Dex's password form:
  by driving `verification_uri_complete` with the same HTTP client and the `Sec-Fetch` headers a browser sends.
  **PKCE enforcement is proven negatively**:
  a second run polling with a verifier that does not match the challenge it sent is refused by Dex.
  Without that case, an issuer silently ignoring both parameters would pass a login-succeeds test.
  Dex publishes `device_authorization_endpoint` and admits the grant by default,
  so its lane configuration needs one change:
  Dex validates a client that lists any redirect URI against that list alone,
  including the internal `/device/callback` the device flow redirects to,
  so `/device/callback` joins the list.
  The client stays public and secretless, which is the other half of what the grant needs there.
  The scope list gains `offline_access`, without which Dex issues no refresh token.
  Keycloak's fixture needs the device grant enabled on its client for the same lane shape to run there,
  and the Keycloak lane runs the same mismatched-verifier case,
  so enforcement is proven against both issuers.

The client's scenarios reach the application Pods through the gateway,
so the lanes that run them declare `needsPodReach` as the profile scenarios already do.

---

## 11. Dependencies

**No Go module is added.**
Everything the client needs is in the standard library:
`net/http` and `crypto/tls` for both clients, `encoding/json` and `net/url` for their wire formats,
`encoding/base64` for the one claim the client reads, `crypto/sha256` for the ad-hoc cache name,
`crypto/rand` for the `Idempotency-Key`, `os/exec` for the viewer, and `flag` for the dispatcher;
`gopkg.in/yaml.v3` already ships and reads the context file with `KnownFields(true)`,
which is what gives the unknown-key rule of *The file*;
`golang.org/x/term`, already direct for `profgate auth hash`, reads the `basic` password without echo.

What is hand-written here is not two form posts and two decodes.
It is the device grant's polling rules, refresh with rotation, revocation, PKCE, error classification,
response bounds, and the cache's locking and expiry —
security-sensitive protocol code, held by the *Testing* cases above rather than by its size.
`golang.org/x/oauth2` was still considered and rejected, on the same grounds
[`auth.md`](auth.md) *Dependencies* rejected a library for the gateway's own OpenID Connect code:
what is left after this document's rules are applied is not the library's job.
`Config.DeviceAuth` and `Config.DeviceAccessToken` exist in the version already in the module graph,
and `DeviceAccessToken` implements RFC 8628's `slow_down` and default-interval rules correctly.
It also owns the loop, and the loop is where this document's rules live:

- it drives the interval from its own `time.Ticker`,
  so a test of the interval, of `slow_down`, or of the deadline has to sleep in real time,
  where *Testing* requires an injected clock as every other timing rule in this project does;
- it returns on any error that is not an OAuth error response,
  so one refused connection ends a login the user is standing in front of,
  where *`profgate login` under `oidc`* retries until the deadline;
- it turns `expires_in` into a context deadline,
  so against an issuer whose device code lives five minutes a caller sees `context.DeadlineExceeded`
  and not the `expired_token` this document answers by name;
- it owns neither refresh nor revocation nor the cache, which is where the rest of the rules are.

Signature verification, the one piece [`auth.md`](auth.md) refuses to hand-write, does not arise here:
the client verifies nothing (*The issuer client*).
The one-importer rules of
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) are unchanged and gain nothing.
`golang.org/x/term` stays importable only from `cmd/profgate`,
which is why the password prompt lives there,
and the credential is passed into `internal/client` rather than read inside it.

---

## 12. Package layout

```text
cmd/profgate/            gains the client verbs: dispatch, flag sets, table rendering,
                         the password prompt (the only x/term importer), exit codes
internal/client/         the API client: context resolution, request building, envelope decoding,
                         transport rules, the token cache and its lock, the issuer client,
                         the device grant, the wait loop
internal/httpapi/        gains the /v1/auth route
internal/config/         gains auth.oidc.cli
```

```go
// Client talks to one gateway as one principal.
// Every method issues exactly one request, except where this document says otherwise.
type Client struct { /* ... */ }

// Credential supplies the Authorization header for one request.
// It is resolved once per command and may refresh a cached token before returning.
type Credential interface {
    Apply(ctx context.Context, r *http.Request) error
}
```

`internal/client` imports no Kubernetes package, no NATS package, and nothing from `internal/k8s`,
`internal/natskv`, or `internal/auth`.
It is reachable only from `cmd/profgate`, which is what keeps the gateway's binary size
and its dependency set an argument about one package rather than about the whole tree.

---

## 13. Changes to the accepted designs

The following text is amended to match this document.
Each row names the heading it edits.

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *HTTP API* | the route list gains `/v1/auth`, a `/v1` route with no authentication step, defined in [`cli.md`](cli.md) |
| `docs/specs/gateway.md` | *Request algorithm* | `/v1/auth` runs route, method, readiness, and parameter checks and stops: it has no credential-placement, authentication, or realm step, because it is read before a credential exists |
| `docs/specs/gateway.md` | *Non-disclosure* | a fifth listed observation: `/v1/auth` returns the mode, and where a command-line client is configured the issuer, client identifier, token type, scopes, and PKCE assertion, to an unauthenticated caller, with the argument of [`cli.md`](cli.md) *Gateway discovery* |
| `docs/specs/gateway.md` | *Logging* | `/v1/auth` writes no audit record, as `/ui/` writes none |
| `docs/specs/gateway.md` | *Metrics* | `endpoint` gains `auth` |
| `docs/specs/gateway.md` | *CLI* | the client verbs of [`cli.md`](cli.md) *The verbs* |
| `docs/specs/gateway.md` | *Configuration* | none: the `auth.mode` row already sends the mode-specific keys to [`auth.md`](auth.md), where the three `auth.oidc.cli` rows are |
| `docs/specs/gateway.md` | *Build and Deployment* | the chart's `auth.oidc.cli.enabled` value, default `true`, rendering an `auth.oidc.cli` block under `oidc` unless a browser client secret under `tokenType: id` forbids it |
| `docs/specs/gateway.md` | *Dependencies* | a closing sentence: the command line adds no Go module, for the reason in [`cli.md`](cli.md) *Dependencies* |
| `docs/specs/gateway.md` | *Package Layout* | `internal/client/` |
| `docs/specs/gateway.md` | *Layers* | the unit rows of [`cli.md`](cli.md) *Testing* and the client steps in the two authentication lanes |
| `docs/specs/gateway.md` | *Failure Scenarios* | a row for `/v1/auth` before readiness, answering `503 not_ready` like every other `/v1` route |
| `docs/specs/auth.md` | *Request algorithm* | the composed order applies to every `/v1` route **except `/v1/auth`**, which has no credential-placement, authentication, or realm step; its failure and disclosure wording gains the same exemption, so "every `/v1` route authenticates" stops contradicting the route this document adds |
| `docs/specs/auth.md` | *Non-goals* | "Token acquisition for the command line … That is a separate design with its own document" becomes a pointer to [`cli.md`](cli.md) |
| `docs/specs/auth.md` | *Configuration* | the three `auth.oidc.cli` rows and the equality rule that binds `cli.clientID` to `audience` under `tokenType: id` |
| `docs/specs/auth.md` | *Issuer notes* | the Keycloak note's "a `curl` user should raise the client's token lifespan or use the command-line client" names [`cli.md`](cli.md); the Dex note records that a refresh token needs `offline_access` |
| `docs/specs/pgo.md` | *Create a Collection* | none: the Idempotency-Key contract this document reads now exists there, with a receipt that binds one key to one Collection for the record's whole life, a `{id, state}` replay, and `409 idempotency_mismatch` on a changed effective policy snapshot |
| `docs/specs/pgo.md` | *HTTP API* | a sentence naming [`cli.md`](cli.md) as the client that drives these routes, changing no behavior |
| `.agents/rules/100-project-map.md` | *Planned Structure* | `internal/client/` |
| `.agents/rules/100-project-map.md` | *External HTTP API* | `/v1/auth` |
| `AGENTS.md` | *Four Specs, All Accepted* | five, adding this document |
| `docs/README.md` | *Where Contributors Start* | [`specs/cli.md`](cli.md) beside the other specs |
| `docs/README.md` | the opening guide list | the client guide [`docs/cli.md`](../cli.md) beside the other user guides |

Updated with the implementation:
[`docs/api.md`](../api.md) (the `/v1/auth` route and the client's examples beside the `curl` ones),
[`docs/authentication.md`](../authentication.md) (*The command-line client*,
the device-login walk-through),
[`docs/configuration.md`](../configuration.md) (`auth.oidc.cli`),
and the client guide [`docs/cli.md`](../cli.md).
[`docs/keycloak-realm.json`](../keycloak-realm.json) gains
`oauth2.device.authorization.grant.enabled` on its client,
which Keycloak reads as `false` when it is absent,
so the exported realm reproduces a device login rather than only a browser one.

### 13.1 Required by this revision and not yet made

None.
The table above records edits that have been made,
and [`gateway.md`](gateway.md) *CLI* now says that every command line it lists answers `-h` and `--help`.

---

## 14. Amendments

Edits made to this document after it was accepted, each in the change that made it.

| Section | Change |
|---|---|
| *Collections*, *Failure table*, *Testing*, *Changes to the accepted designs* | the Idempotency-Key contract this document reads exists in [`pgo.md`](pgo.md) *Create a Collection*: a replay answers `{id, state}` with a `Location` rather than the record, `--wait` needs `pgo.read` to poll what it names, a mismatch is decided on the effective policy snapshot, and `collect` and `collection cancel` send `Content-Type: application/json` |
| *Configuration*, *Changes to the accepted designs* | the chart renders `auth.oidc.cli` by default through `auth.oidc.cli.enabled`, and omits it beside a browser client secret under `tokenType: id`; the binary's opt-in is unchanged |
| *Core decisions*, *Command grammar*, *Help*, *Reading*, *Collections*, *`pgo policy`*, *Output and exit codes*, *Failure scenarios*, *Testing* | a help argument prints help on stdout and exits 0 on every command line the binary has, and wins over positional and flag parsing; a refusal under `--output json` copies the envelope's bytes to stdout beside the one line on stderr; a response that is not envelope-shaped prints its status and nothing the response carried, and a `401` in that state still exits 3; a namespace the gateway does not know is the empty list it answers rather than a not-found the client invents, which no verb can tell from a typo; and the file flag of `collect` and `pgo policy set` is `--file` on both |
| *Reading* | `targets --explain` prints `selectorMatched` as a row of its own, between the target list and the `REASON  COUNT` table, so a selector that matched no Pod no longer prints two empty headers indistinguishable from a selector whose Pods were all excluded |
