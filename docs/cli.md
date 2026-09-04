# Command-Line Client

The `profgate` binary that runs the gateway is also its client.
Its client verbs log in to a gateway, list what your realm admits, pull a profile,
and start, watch, and download PGO Collections,
each verb calling one route of the API that [api.md](api.md) documents.
This guide is for a person at a terminal:
the first login, contexts, the verbs with one example each, where a credential comes from,
the token cache, the exit codes, and the shape a script uses.
The full design lives in [specs/cli.md](specs/cli.md);
the issuers the login has been run against are in [authentication.md](authentication.md).

The client holds no kubeconfig and reaches no Kubernetes API:
it talks to the gateway and, for a login, to the identity provider, and to nothing else.

## Reaching the gateway

The client needs the gateway's base URL and nothing else to start.
From a workstation, port-forward the gateway's Service and point the client at localhost,
in a second terminal while the forward runs:

```sh
kubectl -n profgate port-forward svc/profgate 8080:8080
profgate --server http://localhost:8080 whoami
```

A gateway that serves HTTPS (`server.tls`) holds a certificate issued for a DNS name, not for `localhost`.
`--server-name` is the name the certificate is verified against,
and `--ca-file` adds the authority that issued it to the system trust pool;
the URL stays the forwarded address:

```sh
kubectl -n profgate port-forward svc/profgate 8443:8443
profgate --server https://localhost:8443 --server-name profgate.example --ca-file ca.crt whoami
```

This is the client's form of the `curl --resolve --cacert` recipe in [api.md](api.md#when-the-listener-serves-https).
There is no flag that skips certificate verification,
and TLS 1.2 is the minimum the client accepts.
A self-signed gateway is reached with `--ca-file` naming its own certificate.

A credential travels only to an `https://` URL,
or over `http://` to a host spelled `127.0.0.1`, `::1`, or `localhost`, which is the port-forward above.
Every request that sends a credential over loopback plaintext prints one warning line on stderr.
Any other `http://` gateway is refused before the request is built, with exit 2,
unless the command has no credential to send, which is every command against a gateway in `disabled` mode.
`profgate login` is refused outright over such a gateway, credential or not,
because the issuer it would learn from `GET /v1/auth` could be anyone's.

## The first login

`profgate login` reads `GET /v1/auth` to learn how the gateway authenticates,
then runs the login that mode needs.
Naming a context on the first login is what creates it:

```sh
profgate login --context prod --server https://profgate.example
profgate context use prod
```

The first command writes the context `prod` into the contexts file with the server,
and records the authentication settings the gateway reported;
the second makes it the current context, so later commands need no `--context` and no `--server`.
`login --server <url>` with no context logs in as well, but writes no contexts file:
the token is cached under an entry named after the gateway's origin, and each later command must name `--server` again.

What the login does depends on the gateway's `auth.mode`:

- **`oidc`.**
  The client fetches the issuer's discovery document,
  asks the issuer's device endpoint for a code, and prints two lines on stderr:

  ```text
  Enter the code XRWP-KLMT
  at https://keycloak.example/realms/engineering/device
  ```

  and a third line, `or open <url>`, when the issuer supplies a link that carries the code.
  No browser is opened:
  open the address yourself, on any machine, and approve the code.
  The client polls the issuer until the code is approved,
  the code expires, or `--login-timeout` (10 minutes by default, 1m to 30m) passes.
  It then caches the token, calls `GET /v1/whoami` with it, and prints on stdout:

  ```text
  principal: alice
  realm: developer
  ```

  A gateway that serves no `GET /v1/auth`, or reports `oidc` without the device-login settings,
  is logged in to with `--issuer`, `--client-id`, `--token-type id|access`, `--scope` (repeatable),
  and `--pkce` or `--no-pkce`, or with the same values written into the context's `auth` block by hand;
  each flag overrides what the gateway reported.
  `--issuer-ca-file` adds an authority for the issuer's certificate, separately from `--ca-file`,
  because the gateway and the issuer are two hosts with two certificates.
- **`basic`.**
  The client takes the user name from `-u <name>` or `PROFGATE_USER`,
  the password from `PROFGATE_PASSWORD` or a prompt without echo,
  verifies the pair against `GET /v1/whoami`, prints the principal and realm, and stores nothing:
  its last line on stderr says that the next command reads the two variables, or `-u` with a prompt.
  A password is never cached, because it has no expiry.
- **`disabled`.**
  The login prints that the gateway authenticates nobody and exits 0;
  every command then sends no credential.

Under every mode the login records the mode it found in the context's `auth` block.
Normal commands act on that snapshot and never read `GET /v1/auth` again;
an operator who changes the gateway's mode or issuer shows up as a `401` until you log in again.

### Under Keycloak

The default scope list, at the gateway and in the client, is `openid` and `offline_access`,
because Dex issues a refresh token only when `offline_access` is asked for.
Keycloak reads that scope differently:
it issues an offline token instead of a session-bound refresh token,
and refuses the request unless the user holds the `offline_access` realm role.
The realm export in [`keycloak-realm.json`](keycloak-realm.json) gives `alice` no such role,
so a login against it with the default scopes fails with `the issuer answered 400: not_allowed` and exit 1.
Either grant the user the `offline_access` role,
or set `auth.oidc.cli.scopes: [openid]` on the gateway,
under which Keycloak still returns a refresh token, bound to the SSO session;
the end-to-end suite runs Keycloak with the second choice.

## Contexts

The contexts file is `$XDG_CONFIG_HOME/profgate/config.yaml`,
`~/.config/profgate/config.yaml` when the variable is unset or not absolute.
`login` and `context use` create it; it is otherwise yours to edit,
and an unknown key at any level is refused by name.

```yaml
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

A context name is a DNS-1123 label.
`namespace` is what a bare `<service>` means, so `profgate targets checkout` reads `payments/checkout` above;
without it, a bare Service name is a usage error.

`--context`, or `PROFGATE_CONTEXT` when the flag is absent, selects the context;
with neither, `currentContext` does.
Every other value is then resolved field by field, each source overriding the one before it:

```text
built-in default  <  context file  <  environment  <  command-line flag
```

| Flag | Environment | Context key |
|---|---|---|
| `--server` | `PROFGATE_SERVER` | `server` |
| `--context` | `PROFGATE_CONTEXT` | selects the context |
| `--ca-file` | `PROFGATE_CA_FILE` | `caFile` |
| `--issuer-ca-file` | `PROFGATE_ISSUER_CA_FILE` | `auth.issuerCAFile` |
| `--server-name` | `PROFGATE_SERVER_NAME` | `serverName` |
| `--namespace` | `PROFGATE_NAMESPACE` | `namespace` |
| `--output` | `PROFGATE_OUTPUT` | — |

These global flags may go before the verb or after its positionals, and the later occurrence wins.
Every other flag belongs to one verb and follows that verb's positionals:
`profgate profile payments/checkout cpu --seconds 30` parses,
and `profgate profile --seconds 30 payments/checkout cpu` is a usage error that prints the verb's grammar.

The four `context` subverbs act on the file and send nothing:

```sh
profgate context list            # CURRENT  NAME  SERVER  NAMESPACE; the current one marked *
profgate context show [<name>]   # one context as YAML, the current one when no name is given
profgate context use <name>      # sets currentContext
profgate context delete <name>   # removes the entry and its token cache file
```

`context show` prints the file's entry and never opens the token cache,
so no token can reach the output.

## The verbs

Every verb prints the gateway's answer.
`--output table` (the default) renders it:
a listing as columns under a header line,
and a single record as `key: value` lines.
Columns are space-padded when stdout is a terminal and tab-separated when it is not,
so a pipe into `cut` behaves.
An empty list prints its header and nothing else,
because the header is what tells a script the request succeeded.
`--output json` copies the response body to stdout byte for byte, so `jq` sees the API's own contract.

### Asking a command what it takes

`-h` and `--help` print the command's grammar and its flags on stdout and exit 0,
having sent no request and read no stdin,
so `profgate collect --help | grep retention` works
while a mistyped flag still leaves stdout empty for the shell that was going to consume it.
The page is the deepest verb and subverb the line names, wherever the flag sits:
`profgate --help profile` and `profgate profile --help` are the same request,
and `profgate profile payments/checkout cpu --help` is that request too.
Each subverb answers for itself and prints no sibling's flag,
so `profgate pgo policy set --help` lists the eleven flags that command line takes
and `profgate pgo policy get --help` lists none.
A group prints the one word to add next and the subverbs it takes:
`profgate pgo --help` prints `policy`, and `profgate pgo policy --help` prints `get`, `set`, and `delete`.
A verb or subverb the binary does not have prints the bare binary's help,
because a name nobody recognizes is what a person asks for help about.
The operator command lines answer the same way — `profgate serve`, `config validate`, `auth hash`, and `version` —
and print no global flag, because they accept none:
`profgate auth hash --help` prints its grammar and reads no password.

### Reading

```sh
profgate whoami                        # the principal, the realm, and what it admits
profgate limits                        # the duration limits, the profiles, the port selections, pgo on or off
profgate namespaces                    # NAMESPACE
profgate services payments             # SERVICE
profgate targets payments/checkout     # POD  NODE  VERSION
profgate targets payments/checkout --explain  # the same, plus REASON  COUNT
```

```text
$ profgate whoami
principal   alice
realm       developer
namespaces  payments
services    *
profiles    cpu, heap, goroutine
pgo         read
```

`targets` takes `--port <n>` or `--port-name <name>`,
which the gateway needs in order to decide which Pods are eligible under that port;
both together is a usage error before any request.
`--explain` sends `explain=true` and prints a second table below the target list,
`REASON  COUNT`, one row per reason the gateway counted, in the order it sent them:

```text
$ profgate targets payments/checkout --explain
POD                       NODE      VERSION
checkout-5f7c9d8b6-abcde  worker-1  1.42.0

REASON            COUNT
pod_not_ready     1
endpoint_missing  1
```

An empty `excluded` prints the `REASON  COUNT` header and no rows, like every other empty list;
`--output json` copies the response body unchanged, so the two extra fields ride along in it as sent.

### `profile`

```sh
profgate profile payments/checkout cpu --seconds 30 -o cpu.pprof
go tool pprof cpu.pprof
```

| Flag | Meaning |
|---|---|
| `--seconds <n>` | the duration, for `cpu` and `trace` |
| `--pod <name>` | pin the exact Pod |
| `--version <v>` | keep only Pods whose version label equals this value |
| `--port <n>`, `--port-name <name>` | the pprof port for this request; both together is a usage error |
| `-o <path>` | write the profile here; `-o -` writes it to stdout |
| `--open` | run `go tool pprof -http=:0` on the profile |

Every parameter is sent as given and judged by the gateway:
`--seconds` above `limits.cpuSeconds` is answered `400 seconds_exceeds_limit`,
and a port outside `discovery.pprof.allowedSelections` is answered `400 port_not_allowed`.
The client refuses locally only a profile name outside the eight the gateway knows,
a `--seconds` that is not a positive integer, and both port flags together.

Without `-o`, the profile is written to `<namespace>-<service>-<profile>-<timestamp>.pprof` in the working directory,
`payments-checkout-cpu-20260828T093012Z.pprof` for example.
A binary body never reaches a terminal unless `-o -` asks for it.
The file is opened before the request is sent, so an unwritable path is refused before a profile is collected,
and a request that fails leaves no partial file behind.
Which replica answered is printed on stderr from the response's target headers,
followed by the path written:

```text
pod: checkout-7c8f8c9b9-xabcd
node: worker-07
version: 1.42.3
wrote cpu.pprof
```

`--open` looks `go` up on `PATH` before anything is fetched, and exits 2 when it is absent.
Without `-o` it writes the profile into a temporary directory,
runs `go tool pprof -http=:0` on it, and removes the directory when the viewer exits;
with `-o` the named file stays.
`-http=:0` lets pprof pick a free port and print it.

### Collections

```sh
profgate collect payments/checkout --duration 30s --rounds 3 --wait
profgate collections payments/checkout
profgate collection get 7h2k9m4p6r8t0v1w3x5y
profgate collection cancel 7h2k9m4p6r8t0v1w3x5y
profgate download 7h2k9m4p6r8t0v1w3x5y -o merged.pprof
go build -pgo=merged.pprof ./cmd/yourapp
```

`collect` starts an on-demand Collection and prints its `id` and `state`.
The request body comes from `--duration`, `--rounds`, `--round-interval`, `--replicas all|<n>`,
`--max-parallel`, `--target-version`, and `--retention`,
each absent from the body when its flag is not given, so the Service's effective policy decides it;
`--body <path>` sends a JSON file as the whole body instead and excludes the field flags.
The PGO routes need `pgo.enabled` on the gateway and the realm's `pgo.collect` flag.

Every `collect` mints one `Idempotency-Key` and sends it on every attempt of that invocation.
The gateway binds the key to the Collection it creates,
which is what lets a create whose result is unknown simply be sent again:
no answer arrived, the gateway answered `5xx`, or an answer did not arrive whole.
The retry waits a second, doubling to eight, for at most 30 seconds,
and the answer it gets names the Collection the first attempt created rather than a second one.
An answer that arrived whole is reported as it came, every `4xx` included.
A create still unresolved when the 30 seconds run out is reported once and exits 1,
saying that a Collection may already have been created
and naming `profgate collections <ns>/<svc>` as the way to find out.
`429 collection_in_progress` means another Collection holds the Service and exits 1 without waiting.

`--wait` polls the record every `--poll-interval` (2s by default, 1s to 1m)
until it reaches `completed`, `failed`, `cancelled`, or `expired`,
or `--wait-timeout` (30m by default, 1m to 24h) passes.
Each change in progress prints `round <n> of <m>, <ok> ok, <failed> failed` on stderr,
and the final record goes to stdout.
`completed` exits 0;
`failed` and `cancelled` exit 1 with the record's `reason`;
`expired` exits 1 saying the artifact's retention elapsed before it was downloaded;
the timeout exits 1 naming the identifier.
Reading the record needs the realm's `pgo.read` flag:
under a realm that holds `collect` alone the identifier is printed and the denied read is reported,
and the Collection runs on.
Interrupting a wait stops the watching and not the collecting:
the message names the identifier and `profgate collection get <id>` to watch it again,
and `collection cancel` is what stops the work.

`collections` takes a Service and lists its records newest first, `ID  STATE  ORIGIN  CREATED`.
`collection get` and `collection cancel` take an identifier,
20 lowercase Crockford base32 characters, and refuse anything else before a request is sent.
`cancel` retries `409 collection_initializing` once a second for up to ten seconds,
because it means the record is not claimable yet;
`409 collection_terminal` is reported as it came.

`download` streams a completed Collection's merged profile to `-o <path>`,
to `<id>.pprof` in the working directory without `-o`, or to stdout under `-o -`,
and prints the Collection and the version it profiled on stderr.
`410 artifact_gone` and `409 collection_not_completed` print their envelope and exit 1, with no file left behind.

### `pgo policy`

```sh
profgate pgo policy get payments/checkout
profgate pgo policy set payments/checkout --enabled --every 6h --rounds 3
profgate pgo policy set payments/checkout --file override.json
profgate pgo policy delete payments/checkout
```

`get` prints the effective policy one field per line, with its `source`,
the update fields when an override is stored, and one `violation` line per field a ceiling would refuse.
`set` takes `--file <path>` holding the whole override document,
or `--enabled[=false]`, `--every`, `--jitter`, and the field flags of `collect`;
`get` and `delete` take none of them, and refuse one with the flag named;
it reads the policy first for its `ETag`, then sends one `PUT` conditioned on it,
with no `If-Match` at all when no override was stored, which is what creates one.
A `412` or `428` means the policy changed between the read and the write:
the command reports it, names `profgate pgo policy get` to look at the current value, and never retries,
because a retry would overwrite what the other writer decided.
`delete` follows the same read-then-write shape and reports `404 pgo_override_not_found` as it came.
`set` and `delete` need the realm's `pgo.configure` flag.

## Where a credential comes from

For one command, the client picks the first of these that applies:

1. `--token-file <path>`, or `--token-stdin`;
2. `PROFGATE_TOKEN`;
3. the cached token for the resolved gateway, refreshed when it is about to expire;
4. under `basic`, the user name from `-u <name>` or `PROFGATE_USER`
   and the password from `PROFGATE_PASSWORD` or a prompt;
5. nothing.

A token from the first two sources is used for that one command and never written to the cache:
the client did not obtain it, does not know when it expires, and cannot refresh it.
Whitespace around it is trimmed, and an empty value is a usage error.
No flag takes a token or a password as its value,
because an argument lands in shell history, in `ps`, and in CI logs.

`-u` prompts for the password without echo when stdin is a terminal,
and reads one line from stdin as the password when it is a pipe.
`--token-stdin` and `-u` cannot both read stdin, so together they are a usage error unless `PROFGATE_PASSWORD` is set.
A user name containing `:` and a password longer than 72 bytes are refused before any request,
because the gateway's `401` deliberately says nothing about why.

## The token cache

```text
$XDG_STATE_HOME/profgate/tokens/<name>.json
```

`$XDG_STATE_HOME` defaults to `~/.local/state`.
`<name>` is the context name, or `adhoc-<hex>` for a gateway named by `--server` alone,
where `<hex>` is derived from the gateway's origin.
Each entry records the gateway origin it was obtained for, the issuer, the client identifier, the token type,
the token and its expiry, and the refresh token and its expiry when the issuer granted one.

**The cache file is a bearer credential, and file permissions are all that protect it.**
The directory is created `0700` and each file written `0600`, atomically,
so a crash mid-write leaves the previous token.
That keeps the file from other users on a shared machine;
it does not keep it from `root`, from a backup of `~/.local/state`, or from any process running as you.
The client checks those modes before every read and write and never repairs them:
a directory or file that grants anything to group or other exits 2 naming the path and the expected mode,
because a permission something else widened cannot be told from an attack.

Two rules bound what the cache can be used for:

- **The entry is bound to one gateway.**
  A cached token is sent only when the command resolves to the origin it was obtained for, byte for byte.
  `profgate --context prod --server https://other.example whoami` sends no token and exits 3,
  naming the context and both origins.
- **Refresh runs before a command, under a lock.**
  A token within 30 seconds of expiry is refreshed at the issuer before the request goes out,
  when a refresh token was cached and has not itself expired;
  otherwise the command exits 3 asking for `profgate login`.
  The lock, `<name>.lock` beside the entry, serializes two commands sharing one cache
  so only one of them presents a rotating refresh token;
  a lock not acquired within 30 seconds exits 1 naming the file.
  An issuer that refuses the refresh (`invalid_grant`) deletes the entry, exit 3;
  a transport failure or an issuer `5xx` leaves it as it was, exit 1.

Refresh is what makes Keycloak usable from a terminal:
its ID token lives five minutes by default, and the client renews it silently for as long as the session lasts.

`profgate logout` revokes the refresh token at the issuer when discovery publishes a revocation endpoint,
then deletes the entry.
A failed revocation is a warning and the deletion still happens;
a failed deletion exits 1 and names the file that still holds the credential.
Logout touches nothing on the gateway: a bearer token carries no server-side session to end.

## Errors and exit codes

An error is one line on stderr, `profgate:` followed by the gateway's envelope verbatim:

```text
profgate: seconds_exceeds_limit: effective duration 120s exceeds the limit of 60s
```

Under `--output json` the envelope's own bytes also go to stdout, copied and not rebuilt,
so `jq .code` reads a refusal exactly as it reads every other answer.
The line stays on stderr either way, and only a gateway refusal has bytes to copy:
a transport failure, a response this client could not read as the envelope, and a usage error each leave stdout empty.

A response this client could not read as the envelope prints one fixed line:

```text
profgate: HTTP 502 Bad Gateway: body is not a profgate JSON document
```

That is HTML from an Ingress, an empty body, truncated JSON,
and a `2xx` from a JSON route whose body is not exactly one JSON document.
The status and its standard reason are the whole message, the reason left out for a status that has none;
no media type, no length, and no byte of the body is printed,
because a response an Ingress produced was bounded by no realm.
A `401` in that state exits 3 and every other status exits 1.
A body that fills the client's response bound is a different failure and names the bound instead.
A request that got no answer prints the transport error with the gateway's scheme, host, and port, and no path.
A usage error prints its cause and the verb's grammar.

A namespace the gateway does not know is not an error at all.
The gateway observes no Namespace objects,
so `profgate services <typo>` prints its `SERVICE` header, prints no row, writes nothing to stderr, and exits 0,
the same answer an empty namespace gets.
A namespace the caller's realm does not admit is `403 realm_denied`, exit 1, which is the answer that differs.

| Code | Meaning |
|---|---|
| 0 | the command did what it said |
| 1 | the gateway refused, the transport failed, or a waited-for Collection did not complete |
| 2 | usage: an unknown verb or flag, a bad positional, a local refusal, a missing configuration |
| 3 | authentication is needed: no usable token, a refresh the issuer refused, or `401 unauthenticated` |

Exit 3 is separate so a script can run `profgate login` on it without parsing a message.
`403 realm_denied` is exit 1, not 3: a new login changes nothing when the realm is what refuses.
A `401` answered to a cached token the client believed valid names the issuer and client the token was obtained for,
which is what a reconfigured gateway looks like.

`--verbose` prints one stderr line per HTTP request, with the method, URL, status, and duration for the gateway
and the method, host, endpoint name, status, and duration for the issuer.
No line at any level prints a header, a token, a password, or a device code.

## Automation

A job needs no contexts file and no login.
It passes the gateway on the command line, the token through the environment, and asks for JSON:

```sh
export PROFGATE_SERVER=https://profgate.example
export PROFGATE_TOKEN="$(cat /run/secrets/profgate-token)"
profgate --output json targets payments/checkout | jq -r '.targets[].pod'
profgate profile payments/checkout cpu --seconds 30 -o cpu.pprof
```

The token is used as given and never cached;
when it expires the command exits 3, and obtaining a new one is the job's own step.
`PROFGATE_USER` and `PROFGATE_PASSWORD` are the same shape for a `basic` gateway.
Under `--output json`, `profile` and `download` still write bytes to their file
and print their metadata as one JSON object on stderr.
A refusal's envelope reaches stdout under `--output json` too,
so one `jq` pipeline reads a `400` the way it reads a `200` and the exit code says which arrived.
