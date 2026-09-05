# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Six more alerts ship with the chart.**
  A failing certificate re-read, a certificate within a week of expiry, an authenticator that cannot decide,
  a saturated authentication gate,
  a replica whose profile requests all fail at the dial or the deadline,
  and, only when `pgo.enabled`, a NATS connection that is down.
  The last is the one alert that fires during a NATS outage at startup,
  where `profgate_pgo_synced` does not exist yet and `ProfgatePGONotSynced` is silent.
  Each of the new rules names the replica it fired on rather than the fleet.
  A deployment that set `prometheusRule.rules` keeps its own set and sees none of them.
- **Every release tag gets a GitHub Release.**
  The release workflow creates one after the image and chart are pushed,
  with that version's changelog section, the image tag and digest, and the chart version as its notes.
  `v0.4.0` and `v0.5.0` have releases written the same way by hand;
  the tags before them already had one.
- **The guides link the changelog, and the README names the client.**
  `README.md`, the deployment guide, and the chart README each link this file where a reader stands when it matters,
  and the deployment guide's upgrade section restates `0.5.0`'s port-selection change there.
  The README also names the `profgate` binary as a client, links its guide,
  and its quickstart lists the namespaces and the Services a caller's realm admits before it asks for a profile.
- **Every command line answers `-h` and `--help`.**
  The bare binary, each verb, each subverb, and each group that takes one print their grammar and their flags,
  and so do the operator command lines `serve`, `config validate`, `auth hash`, and `version`.
  Each page goes to stdout and exits 0, having sent no request and read no stdin:
  `profgate auth hash --help` prints its grammar where it read a password before.
  The page is the deepest verb and subverb the line names, wherever the flag sits,
  each subverb prints only what its own command line takes,
  an operator page prints no global flag because that half accepts none,
  and a name the binary does not have prints the bare binary's help.
  `profgate whoami --help` printed `flag: help requested` on stderr and exited 2.
- **`collection get` prints what a record's end depends on.**
  `resolvedVersion`, `finishedAt`, `expiresAt`, and `artifactBytes` were in the gateway's document
  and dropped by the table that read it,
  so the table said neither which binary was profiled nor how long `download` will still work.
  Each row is printed only by a record that carries it,
  and the artifact's object name is not printed, because it is a store key and nothing a caller can act on.
- **`targets --explain` names how many Pods the selector matched.**
  A Service whose selector matched no Pod
  and one whose selector matched Pods that were all excluded both printed two empty headers,
  with nothing to tell them apart;
  a `selectorMatched` row now prints between the target list and the `REASON  COUNT` table,
  the count the gateway already sent and no renderer read.
  It prints whether or not it is zero, and only beside `--explain`; without the flag nothing changes.
- **The deployment guide names every value the `code` label takes.**
  The forty envelope codes by the tables that hold them, the eight audit-only outcomes in full,
  and the `upstream_<status>` family;
  and it says that `profgate_confirm_total` and `profgate_profiles_in_flight` count the interactive path alone.
- **The guides have troubleshooting sections.**
  For each failure the gateway measures, the deployment guide names the symptom, the signal, and the step,
  including a bucket deleted or recreated under a running process, a restore from backup, and NATS maintenance;
  the authentication guide does the same for each authentication failure reason.
  Both guides now say that a NATS disconnect of any length aborts the Collections a replica owns
  and spends an attempt on each.
- **The deployment guide has a page of example queries.**
  One per question an operator asks of the metrics — request rate, error share, latency, refusals,
  certificate lifetime, authentication failures, readiness across replicas, and collection outcomes —
  so a dashboard starts from a query that already names the right series and labels.
  `promtool` parses every one of them in the test suite, and the guide and the checked-in set have to agree.

### Changed

- **BREAKING: the round display counts from one.**
  `progress.round` is the zero-based index of the running round,
  and every surface that showed it put it on the line beside the round count,
  so a completed three-round Collection read `round 2 of 3` and a starting one read `round 0 of 3`.
  `collection get` and the progress lines `collect --wait` writes now read `round 3 of 3` and `round 1 of 3`,
  and the console's Collection detail counts the same way, so the two surfaces say one thing about one record.
  That detail line now reads `-` until a round has been claimed,
  where it read `round 0 of 0, samples ok 0, failed 0`, the shape the command line already leaves out.
  The document is unchanged: under `--output json` the record still carries the index the gateway stored.
- **BREAKING: under `--output json` a gateway refusal writes its envelope to stdout.**
  Stdout was empty on every failure, so a `jq` pipeline that read a `200` had nothing to read on a `400`;
  the gateway's own bytes are now copied there, byte for byte and not rebuilt,
  while the one line stays on stderr and the exit code is unchanged.
  Only a refusal has an envelope to copy:
  a transport failure, a response the client could not read as the envelope, and a usage error still leave stdout empty.
  A script that treated any stdout byte as success now sees a refusal's document there.
- **BREAKING: `pgo policy get` and `pgo policy delete` refuse the flags only `set` reads.**
  All three subverbs shared one flag set, so `--file`, `--enabled`, `--every`, `--jitter`,
  and the field flags of `collect` parsed on the two that never read them and were dropped without a word;
  each subverb now registers what its own command line takes,
  and one of those flags beside `get` or `delete` is a usage error naming the flag.
- **BREAKING: `collect --wait` sends its receipt to stderr.**
  Table mode printed `id` and `state` to stdout before polling,
  so a `--wait` run left two documents on stdout once the final record arrived;
  `--output json` printed no receipt at all, so a `jq` caller had no identifier until the record did.
  Both modes now print `id: <id>` and `state: <state>` to stderr before the first poll,
  and stdout carries the final record alone, table or JSON.
  `collect` without `--wait` is unchanged: there the receipt is the document the command produced.
- **A usage error prints the grammar of the command line it was given.**
  A `context` subverb printed its cause with no grammar line below it,
  which every other verb's usage error already prints, and now prints its own:
  `profgate context list foo` names `usage: profgate context list`.
  A line that matched no subverb prints the group's line instead,
  which names the subverbs and not their positionals,
  so `profgate context` reads `usage: profgate context list|show|use|delete`
  and `profgate pgo` reads `usage: profgate pgo policy`,
  each sending the reader to that subverb's own page for what it takes.
  The operator command lines print those same two lines in place of `flag`'s own usage block,
  so `profgate serve --bogus` names the flag and then `usage: profgate serve --config <path>`.
  The two operator groups print their own line as well:
  `profgate config` reads `usage: profgate config validate` and `profgate auth` reads `usage: profgate auth hash`,
  where each printed the bare binary's usage line, which names every command the binary has.
- **BREAKING: the container memory limit follows `pgo.enabled`.**
  `profgate config validate` printed a merge budget for a gateway that never merges,
  three times what the chart renders for the same file;
  with collection off it now prints the gateway's own footprint alone,
  and prints `pgo collection: disabled` in place of the `pgo working set bytes` line,
  so a reader of the second line sees the state rather than a budget.
  The kustomize base drops from `1536Mi` to `512Mi` with it,
  so uncommenting its PGO block now means raising the limit too.
- **BREAKING: `discovery.pprof.port: 0` is refused.**
  It was read as "unset" and normalized to `6060`, or ignored beside a `portName`;
  a configuration that writes it, in the file or as `PROFGATE_PPROF_PORT`, now fails at startup,
  and omitting the key is how the default is taken.
- **BREAKING: an `auth.oidc.browser` or `auth.oidc.cli` variable with no block to land in is refused.**
  The eight `PROFGATE_AUTH_OIDC_*` variables of the two blocks were applied to a nil pointer and dropped without a word;
  a deployment that exports one without opening the block in the file now fails at startup,
  naming the variable and the block, whatever `auth.mode` it runs.
  An empty mapping, `browser: {}` or `cli: {}`, is enough to open the block.
- **BREAKING: the chart validates `memoryLimitWithoutPGO` on both branches.**
  With `pgo.enabled` false the chart printed the value unchecked, so `512` rendered a limit of 512 bytes.
  Both branches now hold it to the one grammar, a whole number of `Mi` or `Gi`,
  so a values file carrying `512`, `1500M`, or `0.5Gi` fails rendering where it rendered before.
  An accepted value renders exactly what it rendered.
- **BREAKING: `profile` and `download` refuse `-o json` and `-o yaml`.**
  `-o` names the file the bytes are written to,
  so a caller reaching for kubectl's output flag collected a profile,
  wrote it to a pprof file called `json`, and was told that file had been written.
  Both verbs now refuse those two values before a request is sent, naming `--output` as the format flag.
  Nothing else is refused: `-o ./json` still writes a file called `json`.
- **BREAKING: `logout` writes its two lines to stderr.**
  Success printed nothing at all, so a logout that deleted a credential looked the same as one that did nothing,
  and the notice that nothing was cached went to stdout, where a composed English sentence does not belong.
  Both lines are now on stderr: `logged out of <context or gateway>` where an entry was deleted,
  and `nothing is cached for <context or gateway>` where none was there.
  A script reading `logout`'s stdout for that notice now reads nothing.
- **BREAKING: `collect --file` replaces `collect --body`.**
  `collect` and `pgo policy set` named the same concept two ways: one JSON file sent as the whole request,
  in place of the field flags.
  `collect` now takes `--file <path>`, the name `pgo policy set` already used;
  `--body` is gone, and no alias is registered for it.
  A script that still sends `--body` now exits 2, `flag provided but not defined: -body`.
- **`ProfgateNotReady` and `profgate_discovery_synced` say what the gauge measures.**
  The alert claimed that a replica was not serving,
  and the gauge's own `HELP` text called it a current cache state,
  where it reports the completion of the initial informer sync alone and never returns to `0`.
  The alert now names that gate, names the three readiness gates it does not report,
  and says that the gauge is also `0` while an issuer or a Kubernetes preflight keeps the informers from starting.
  The expression, the window, and the severity are unchanged.
- **BREAKING: the chart's `PodMonitor` drops the `endpoint` target label.**
  prometheus-operator writes one from the port name, the gateway sets its own `endpoint` on
  `profgate_requests_total`,
  and with `honorLabels` unset the scrape renamed the gateway's value to `exported_endpoint`,
  so no query or rule scoped `endpoint="profile"` matched anything.
  The target label is now dropped before the scrape and the gateway's value arrives as `endpoint`.
  On an install already running `podMonitor.enabled: true`,
  a dashboard or rule reading `exported_endpoint` stops matching and reads `endpoint` instead,
  and `up{endpoint="ops"}` and the other per-scrape series lose a label whose value was always `ops`.
  The kustomize install renders no `PodMonitor` and is unchanged.

### Fixed

- **A client that stops reading holds nothing past the request budget.**
  The budget bounded confirmation, the dial, the header wait, and the upstream read,
  and not the write to the client:
  a client that read the headers and then stopped held the handler, its admission slot, and the Pod connection for as long as it kept the socket open,
  and sixteen such clients answered every later profile request `429 too_many_profiles`.
  The write to the client now fails at the budget's end,
  the request is audited `upstream_stream_failed` as any expiry after the response is committed is,
  and the slot and the Pod connection are released then.
- **The upstream transport's idle pool is bounded.**
  An idle connection to a Pod was kept until the Pod closed it or TCP keepalive noticed the Pod was gone,
  so the pool grew with every Pod ever profiled.
  The transport now keeps at most 100 idle connections and closes one idle for 90 seconds,
  the standard transport's values; two per Pod, as before.
- **A request body that arrives slowly is refused after ten seconds.**
  A PGO route read its JSON body with no bound once the headers were in,
  so a body sent one byte at a time held a handler goroutine for as long as the client chose,
  and a route that takes no body waited the same way for the one byte that would prove one was sent.
  Both reads now have the ten seconds the headers have,
  and a body that has not arrived by then is answered `400 invalid_parameter` with a `body_malformed` detail that names the bound.
  A request refused before its body was read waited the same way:
  `net/http` discards the unread body before it sends the refusal, and that wait had no bound either.
  The deadline is now set when a request with a body enters the handler,
  so a refused request whose body never arrives no longer holds the connection.
  The bound is a constant, as the header bound is; there is no key for it.
  A client fetching several short profiles from one Pod still reuses its connection.
- **An idle connection is closed after two minutes, and `net/http`'s own lines are JSON on stdout.**
  A keep-alive connection that sent nothing after its last request was held until the process exited;
  both listeners now close it after 120 seconds.
  A TLS handshake failure and a recovered handler panic were printed to stderr as text, outside `server.logLevel`;
  they are now `ERROR` records on stdout like everything else the gateway says.
- **Kubernetes client failures are JSON on stdout.**
  client-go's informers log through klog, whose default sink is stderr text outside `server.logLevel`,
  so a watch that kept failing after the first sync was invisible in the gateway's own log.
  klog now writes through the gateway's logger at the level client-go emits:
  a list that fails is an `ERROR` record, a watch that ends with an error and is retried is `INFO`,
  and client-go's verbose lines, a watch that closes cleanly among them, appear under `server.logLevel: debug`.
- **Three log lines name the work they belong to.**
  A failed collection sample names its Collection,
  so it is attributable to one of the several a replica may be running,
  and the authenticator error and the unreadable idempotency receipt carry `requestId`,
  with the receipt warning naming the Service too,
  so each joins the audit record of the same request.
- **`login --context <name>` names the command that selects the context.**
  `--context` says which context one command speaks through and selects nothing,
  so a first login wrote the context and left every later command resolving the old one, without a word.
  A login that recorded a context which is not the selected one now prints on stderr
  `context <name> is not the selected context; select it with profgate context use <name>`.
  Nothing is selected automatically: selecting would change what every later command does without being asked.
- **`context delete` says what it deleted and what the deletion took with it.**
  A successful delete printed nothing, so removing the selected context left no context selected and said so nowhere.
  It now prints `deleted context <name>` on stderr,
  and `no context is selected; select one with profgate context use` below it when that name was the selected one.
- **The plaintext warning follows the credential it describes.**
  Against a loopback `http://` gateway the warning was printed before the credential was resolved,
  so a command with an expired cached token and no refresh token announced a credential it never sent.
  The warning now prints after the credential is attached.
  A non-loopback plaintext URL is still refused before either happens.
- **A misspelled key in the contexts file is named without a Go type.**
  `field srever not found in type client.File` told the reader about this program's source rather than their file;
  the refusal now reads `<path>: line 4: srever is not a contexts-file key`.
  An entry the rewrite does not recognize keeps the library's wording, after the file name.
- **The client guide describes the rendering it has.**
  It said a single record prints as `key: value` lines,
  where the renderer writes the key and the value as two columns, tab-separated in a pipe and padded on a terminal,
  which is a listing without its header line.
- **`-n` and `-o` name the flag this binary spells in full.**
  Either of kubectl's two short flags, on a command line that defines neither,
  failed with `flag provided but not defined` and nothing else;
  a line naming `--namespace`, or `--output`, now stands between that cause and the usage line.
- **A response the client cannot read as the gateway's envelope says so.**
  HTML from an Ingress, an empty body, truncated JSON,
  and a `2xx` from a JSON route whose body is not one JSON document each printed `HTTP 502 Bad Gateway` alone,
  a status with no account of why it was a failure;
  each now prints `HTTP 502 Bad Gateway: body is not a profgate JSON document`,
  still with no media type, no length, and no byte of the body.
  A `401` in that state still exits 3 and every other status exits 1.
- **An error body with no `error` key is no longer read as the envelope.**
  A JSON document carrying a `code` and nothing else printed `code: ` with an empty message;
  the client now requires `error` to be present and a string,
  so such a body is one of the responses above and prints the fixed line.
- **A response body that fills the client's bound names the bound.**
  A non-`2xx` whose body exceeded the 1 MiB the client reads was reported as a bare status,
  losing the one fact that explained it;
  both read paths now print `body exceeds the 1048576-byte bound this client reads` beside the status.
- **The chart refuses an authentication mode it can see will not start.**
  `auth.mode: basic` with neither `auth.basic.users` nor `auth.basic.usersFile`,
  and `auth.mode: oidc` with no `auth.oidc.issuer`,
  each rendered a Deployment whose Pod exited at startup;
  both now fail while the chart renders.
  The raw `config:` block is read first for `auth.mode`, `auth.basic.users`, and `auth.oidc.issuer`,
  so a value supplied only there is the one judged and the one named,
  and any `PROFGATE_AUTH_`-prefixed entry in `extraEnv` switches both checks off,
  because the environment can carry a value the chart cannot see.
- **`kubectl apply -k deploy/base` works on a cluster that has never heard of Profgate.**
  The base creates the `profgate` namespace its own resources name,
  so `kubectl delete -k deploy/base` now removes that namespace and everything in it,
  and a repository whose namespaces are managed elsewhere drops the entry from `resources`.
  The base also pins the released image tag where it pinned `latest`.
- **The install notes describe the release that was rendered.**
  Basic mode's notes name where users come from instead of promising a list and printing the realms,
  and an Ingress in front of a TLS-enabled API port is warned about the backend-protocol setting it needs.
- **A realm that names a profile wrong is told the names it may choose from.**
  `realms.<name>.profiles: invalid entry "heaap"` was the whole message;
  it now ends with the eight profile names in order and the `"*"` wildcard,
  the one realm list whose accepted set is closed.
  `namespaces` and `services` hold DNS-1123 labels and keep their message.
- **A decode error names the file, the line, and the key path.**
  A key the schema does not define was reported as `field opsListn not found in type config.ServerConfig`,
  a Go type the operator has never seen, with no file named;
  it now reads `config: /etc/profgate/config.yaml: line 3: unknown key server.opsListn`,
  a value of the wrong type carries its key the same way,
  and a line that names no single key keeps the file and the line.
- **Four refusals name where the next request goes.**
  `port_not_allowed`, `no_targets`, `collection_in_progress`, and `pgo_disabled` said only what was refused,
  leaving a caller to already know the gateway's shape to find the fix.
  Each message keeps its status, its code, and its details, and now ends with a clause naming the next step:
  `GET /v1/limits`, the targets route with `explain=true`, the collections route that lists the live one,
  and the `pgo.enabled` configuration key, in that order.
- **`profgate_tls_certificate_expiry_seconds` reads `NaN` before a certificate is loaded.**
  It read `0` on every install without `server.tls`, a certificate that expired at the epoch,
  which no expiry threshold could be written over.
  An operator's own rule that compared the gauge to zero no longer matches on such an install;
  `profgate_tls_certificate_expiry_seconds - time() < 604800` is the form that now works everywhere.
- **`profgate_nats_connected` reads `NaN` where no NATS connection is ever made.**
  It read `0` on every install with `pgo.enabled: false` — a transport that was never configured, reported as down —
  which no rule could be written over.
  It now reads `0` under `pgo.enabled`, from the start of the NATS preflight and before the first attempt,
  rather than only once a connection has been made and lost.
  An operator's own `profgate_nats_connected == 0` rule no longer matches on an install that runs no collection,
  and it now matches from the start of a NATS outage at startup instead of staying silent through it.

## [0.5.0] - 2026-09-03

Adds a first-party command line, target exclusion diagnostics, and an HTTP contract automation can build on,
alongside Collection controls in the console, lower PGO defaults,
and a store-generation barrier that keeps the watched PGO caches honest, with five breaking changes.

### Added

- **The `profgate` binary is also a client.**
  `login`, `logout`, `whoami`, `limits`, `namespaces`, `services`, `targets`, `profile`,
  `collect`, `collections`, `collection get|cancel`, `download`, `pgo policy get|set|delete`,
  and `context list|show|use|delete` talk to a gateway from a terminal, and `docs/cli.md` is the guide.
  Under `oidc`, `login` obtains a token by the device-code grant and caches it under `$XDG_STATE_HOME/profgate/tokens/`;
  under `basic` it verifies a user name and a password it never stores.
  `profile --open` hands the fetched profile to `go tool pprof -http`.
- **A device login a client can discover.**
  `GET /v1/auth`, the one `/v1` route with no authentication step, reports `auth.mode` and,
  where the new optional `auth.oidc.cli` block (`clientID`, `scopes`, `pkce`) is configured,
  the issuer, client identifier, token type, scopes, and whether the device endpoint accepts PKCE.
  The chart renders that block by default through `auth.oidc.cli.enabled: true`.
- **Every response of both listeners carries `X-Request-Id`** — the caller's own when the request sends one,
  generated otherwise — and every audit record names the same value under `requestId`.
- **Every refusal whose code has a vocabulary carries a `details` array of `{field, code, message}` items.**
  `invalid_parameter` has twelve values, `limit_exceeded` five, and `port_not_allowed` its one,
  and every entry of `GET /pgo`'s `violations` carries a `code` from the `limit_exceeded` vocabulary.
- **`POST .../collections` takes an `Idempotency-Key`.**
  A repeat under one key answers with the Collection the first request created rather than starting a second,
  and a repeat asking for something else answers `409 idempotency_mismatch`.
- **`GET /v1/collections/{id}` takes `wait=`, a duration from `1s` to `60s`,**
  and holds the request open until the Collection's state moves, the deadline passes,
  the replica drains, or the client leaves.
- **The collections listing filters and pages, and `latest` answers without an identifier.**
  `GET .../collections` takes `state`, `origin`, `since`, `limit`, and `cursor`, and its body carries `nextCursor`;
  `GET .../collections/latest` and `.../latest/profile` answer for the newest Collection still holding a profile.
- **`GET /v1/openapi.json` serves a hand-maintained OpenAPI 3.1 document.**
  It describes every route the listener serves, whatever the configuration enables.
- **`explain=true` on the targets endpoint, and the `version` and `pod` filters it now accepts.**
  `GET .../targets?explain=true` keeps the plain listing and adds `selectorMatched` and `excluded`,
  one entry per exclusion reason with a non-zero count, so an empty answer says why it is empty.
  The console shows those reasons where a Service has no target, and `profgate targets --explain` prints them too.
- **The console starts and cancels a Collection.**
  A **Start collection** control, and a **Cancel** on every `pending` or `running` row,
  appear when `pgo.enabled` is true and the caller's realm carries `pgo.collect`.
  Each takes two presses in place, and the page still edits no Service's PGO policy.
- **Optional chart templates for the pieces every install needed to write by hand.**
  `ingress.enabled` renders an Ingress routing `/`, `/ui/`, `/auth/`, and `/v1/` to the API port;
  `podMonitor.enabled` renders a PodMonitor for the ops port, which the Service deliberately omits;
  `prometheusRule.enabled` renders alerts for stale JWKS keys, discovery not synced, and admission saturation.
  All three are off by default.
- **A default CPU request.**
  The container ships `resources.requests.cpu: 100m`, so a namespace whose quota counts CPU requests admits it.
- **`profgate_pgo_synced`, and the chart's `ProfgatePGONotSynced` alert.**
  The gauge is `1` only when the watched PGO caches have replayed and applied under the current store generation.
  The alert fires when it has read `0` for ten minutes, and is off by default like the other three.
- **The end-to-end suite drives the console in a headless Chromium.**
  Two scenarios execute the page's own JavaScript, which nothing else does, and a machine with no Chromium skips them.

### Changed

- **BREAKING: client-selected ports are default-deny.**
  `discovery.pprof.allowedPorts` and `allowedPortNames` are removed, with their `PROFGATE_*` variables,
  and replaced by the one list `discovery.pprof.allowedSelections`
  (`PROFGATE_PPROF_ALLOWED_SELECTIONS`, comma-separated `port:N`, `portName:name`, `port:*`, `portName:*`).
  `{port: "*"}` admits any port number and `{portName: "*"}` admits any port name, each on its own.
  An empty list now admits only the configured default, where an empty allowlist used to admit anything,
  and a configuration that still sets a removed name fails validation with a message naming the replacement.
  `/v1/limits` reports `pprof.allowedSelections` in place of the two arrays, and each old list converts on its own:

  | Old value | New entry |
  |---|---|
  | `allowedPorts: []` | `- port: "*"` |
  | `allowedPortNames: []` | `- portName: "*"` |
  | `allowedPorts: [6061, 6062]` | `- port: 6061` and `- port: 6062` |
  | `allowedPortNames: [pprof-alt]` | `- portName: pprof-alt` |

- **BREAKING: `pgo.defaults.target.versionPolicy` is removed.**
  It was the only key under `pgo.defaults.target`, so delete the whole block; nothing replaces it.
  A configuration file that still sets it fails validation, a request body carrying the field is refused,
  and `effective.target.versionPolicy` is gone from `GET /pgo` and from `profgate pgo policy`.
  Removing it also moves the policy hash an `Idempotency-Key` is bound to,
  so retry with a fresh key: one minted before the upgrade answers `409 idempotency_mismatch`
  until its receipt expires, which takes `pgo.jobRetention`, a week by default.
- **BREAKING: an artifact is kept for at least the interval that produces it.**
  `pgo.defaults.artifact.retention` moves from `2h` to `24h`,
  and every effective policy must now hold `artifact.retention` at least `schedule.every`.
  A policy that breaks the rule is refused with `400 limit_exceeded` and a `retention_under_interval` detail,
  a stored override that breaks it makes the Service ineligible for scheduling, and a file pinning it no longer starts.
  Raise the retention, or lower the interval, until one covers the other.
- **BREAKING: the container memory limit falls from 4 GiB to `1536Mi` and now counts the gateway's own footprint.**
  `pgo.limits.maxSampleBytes` (`33554432` to `16777216`), `maxMergedBytes` (`67108864` to `33554432`),
  and `maxActiveCollections` (`2` to `1`) take the working set from 4 GiB to 1 GiB; `maxParallel` keeps its `4`.
  An operator who set any of the three keys keeps their own value, and the chart sizes the container from it;
  the chart's `memoryLimitWithoutPGO` is now read as a byte count and must be a whole number of `Mi` or `Gi`.
  A hand-written Deployment carrying `4Gi` still runs, and the kustomize base moves to `1536Mi`.
  `profgate config validate` prints `pgo working set bytes` and `container memory bytes` in place of `pgo memory bytes`.
- **BREAKING: the two Collection writes require `Content-Type: application/json`.**
  `POST .../collections` and `POST /v1/collections/{id}/cancel` refuse anything else with `400 invalid_parameter`,
  so a client that omitted the header declares it.
- **Console assets are served at stable paths.**
  No asset URL carries a content hash, and each asset carries an `ETag` and `Cache-Control: no-cache`,
  so a browser revalidates on every load and is answered `304` while its copy is current.
  This removes the rolling-update failure the hashed tree produced, at the cost of one rollout —
  the one that carries this release — where a console load can fail until the replicas converge.
- **Upgrading invalidates every browser session.**
  Session and transaction cookies now carry a JSON object where they carried length-prefixed fields,
  so a browser holding a session is signed out once and logs in again,
  and a login in flight across the upgrade returns `401` with reason `state` and starts over.
- **A rollout interrupts a running Collection instead of waiting for it.**
  A terminating replica stops renewing the lease on every Collection it owns,
  and returns once each owner has committed or reached the cutoff of the lease it last renewed,
  which is `pgo.leaseTTL` minus five seconds of clock skew.
  An owner still merging at that cutoff commits nothing,
  and the replica that reclaims the record retries it from round zero under `pgo.maxAttempts`.
  `profgate config validate` no longer prints a second grace period for PGO.

### Removed

- **`slot_timeout` no longer appears in a Collection manifest.**
  Sampling takes no slot in the admission gate interactive requests pass through,
  so a sample never waits for one and never fails for want of one.
- **The rule measuring the PGO fan-out against `limits.maxConcurrentProfiles` is gone.**
  A configuration whose `pgo.limits.maxParallel × pgo.limits.maxActiveCollections` reaches that ceiling now loads,
  and every configuration that loaded before still loads.

### Fixed

- **A gateway with `pgo.enabled` no longer gains a duplicate cache consumer when a watch fails to open.**
  A failed open now ends the watches the attempt had already opened,
  so a process that reopens its caches runs on one consumer per prefix.
- **A watch cut under a live connection no longer leaves the watched PGO caches stale.**
  A re-opened watcher used to replay under the generation it already held,
  so nothing rebuilt the cache and a key changed during the gap could stay missing indefinitely.
  Such a cut now moves the store generation the way a disconnect already does, and every watched cache rebuilds.
  A session's cache reads carry the generation they were admitted under,
  so the collections listing, a Collection create, and the two `latest` routes now answer `503 pgo_unavailable`
  where they used to answer over caches that had gone stale.


## [0.4.0] - 2026-08-27

Adds authentication, the embedded operator console, and client-selected pprof ports.

### Added

- **A client can select the pprof port for one request.**
  A `port` (a decimal integer) or `portName` (a container-port name) query parameter,
  on the targets and profile endpoints,
  replaces the configured default for that request, never both together.
  Two independent operator allowlists,
  `discovery.pprof.allowedPorts` and `allowedPortNames`, bound what a client may name;
  an empty list permits any value of its parameter, and the configured default always passes.
  A value a non-empty allowlist excludes is refused with `400 port_not_allowed` before discovery runs.
  The audit log gains a `port` field recording the selection as sent —
  a number or a name, empty when absent, and for a name never the number it resolved to.
  The end-to-end test application now serves pprof on a second listener
  so the two allowlists have a real alternate port and port name to exercise.

- **Two authentication modes.**
  `auth.mode: basic` checks HTTP Basic credentials against a static, bcrypt-hashed user list;
  `auth.mode: oidc` verifies a bearer JWT against an OpenID Connect issuer
  and maps it to a realm by username, by group, or by a default.
  A credential that fails to resolve to a realm is denied outright —
  neither mode falls through to `auth.anonymousRealm` —
  and every authentication failure answers `401 unauthenticated` with a `WWW-Authenticate` header naming the scheme,
  `429 too_many_auth` when Basic's per-replica bcrypt gate is full,
  or `503 auth_unavailable` when the gateway cannot decide
  (stale signing keys, an unreachable issuer, a failed random read).
  `internal/auth` is the only non-test importer of `github.com/go-jose/go-jose/v4` and `golang.org/x/crypto`.

- **An optional browser login for `oidc` mode.**
  `auth.oidc.browser` turns a browser navigation with no credential into a redirect to the issuer's login page,
  instead of `401`, using the authorization-code flow with PKCE,
  and mints an encrypted, stateless session cookie from the ID token it receives.
  The three `/auth/login`, `/auth/callback`, and `/auth/logout` routes exist only when it is configured.
  A session cannot be revoked before it expires; `sessionTTL` (8 hours by default) bounds that exposure.
  The cookie key rotates across replicas in a staged five-step procedure that never drops a session,
  and `profgate_auth_cookie_key_info` reports which fingerprints every replica has loaded.

- **`profgate auth hash`.**
  Reads a password from the terminal without echo and prints its bcrypt hash at cost 12,
  so an operator never has to find `htpasswd`.

- **Authentication metrics, audit fields, and the authentication Secret.**
  The ops listener gains `profgate_auth_failures_total`, `profgate_auth_sessions_issued_total`,
  `profgate_oidc_jwks_refresh_total`, `profgate_oidc_jwks_keys`, `profgate_oidc_jwks_age_seconds`,
  `profgate_auth_file_reload_total`, and `profgate_auth_cookie_key_info`.
  The audit log gains `auth_reason` on a failure, and the `/auth/` routes write their own record.
  The Helm chart mounts a Secret at `auth.secret.mountPath` for the users file, the issuer CA certificate,
  a browser client secret, and the cookie key, none of which belong in the rendered ConfigMap;
  [`deploy/secret-auth-example.yaml`](deploy/secret-auth-example.yaml) is a commented manifest for it.

- **An embedded operator console, off by default.**
  `ui.enabled` serves a static page at `/ui/`:
  pick a namespace, then a Service, then a profile,
  download it or copy the URL `go tool pprof` fetches it from,
  see who you are and what your realm admits,
  and, when PGO collection is enabled, browse a Service's Collections and download a finished artifact.
  The page renders no profile and stores nothing; every fact it shows came from a `/v1` response a realm bounded.
  Four listing routes back it and are useful from a script too, realm-filtered and read-only:
  `GET /v1/namespaces`, `GET /v1/namespaces/{ns}/services`, `GET /v1/whoami`, and `GET /v1/limits`.
  `Catalog` on the discovery seam reads the Service cache for the two lists and issues no request,
  so the console adds no Kubernetes capability.
  `profgate_requests_total`'s `endpoint` label gains `namespaces`, `services`, `whoami`, `limits`, and `ui`.
  The page vendors Preact, htm, and Pico CSS under `internal/ui/static/vendor/`,
  compiled into the binary with `go:embed`; it loads nothing from a CDN.
  The Helm chart gains a `ui.enabled` value, off by default.

### Changed

- **The targets endpoint now accepts `port` and `portName`.**
  It previously took no query parameters; any other query parameter still answers
  `400 invalid_parameter`.
- **The permission invariant's wording.**
  It now says the gateway connects to application ports the operator permits,
  and that an empty allowlist accepts any port or port name a client names,
  rather than naming one fixed port as the only one reachable.
- **The request algorithm gained two steps.**
  Credential placement rejects an `access_token` query parameter before any credential is read,
  and authentication resolves the principal and its realm; every later step renumbers.
  `401 unauthenticated` now precedes `403 realm_denied`,
  so a client can tell "present a credential" from "your credential does not reach this."
- **`/` now redirects to `/ui/` when the console is enabled.**
  Logout's fallback redirect lands there too, so signing out returns to a signed-out console.

## [0.3.0] - 2026-08-26

Documents the project for public use, hardens the chart against values it cannot honor,
and corrects guidance the code contradicted.

### Added

- **A documentation set for public use.**
  A root README positions the gateway and walks a Helm quickstart to a first profile;
  four guides cover the API, the configuration file, deployment, and PGO collection end to end;
  this changelog backfills the released versions;
  and an Apache-2.0 license states the terms the published image and chart carry.

- **Chart rendering fails on values it cannot honor.**
  The container memory limit is derived from the PGO ceilings,
  so overriding one of those keys through the raw config block or `extraEnv` now fails rendering,
  naming the supported values key,
  and an explicit `null` at `config.pgo` or `config.pgo.limits` — which would bypass that guard — fails too.
  The same guard covers the file-path keys the Deployment's Secret mounts are built from,
  because a raw or environment override there points the config at files nothing mounts;
  such an override fails rendering rather than shipping a crash-looping Pod,
  a `nats.credsFile` that does not name the file the Secret mount provides fails the same way,
  and a raw `config.nats.url` now satisfies the URL requirement the way the docs promise.
  A PodDisruptionBudget with both bounds set, or with neither, no longer ships a budget the operator did not ask for:
  rendering fails and names the fix,
  and `maxUnavailable: 0` counts as a set bound rather than reading as unset.

- **The informer sync wait logs its progress.**
  A Pod waiting for the Kubernetes informer caches warns every 15 seconds with the elapsed time,
  so each of the readiness waits now names itself in the logs.

### Changed

- **NATS credentials are optional in the chart.**
  The chart mounted the credentials Secret and rendered `credsFile` whenever PGO was enabled,
  though with an empty `credsFile` the binary skips the JWT credentials file.
  Both now render only when `nats.credsFile` is non-empty,
  so `nats.credsFile: ""` skips the Secret mount and authentication, if any, rides in the URL.

### Fixed

- **The application NetworkPolicy example left `deploy/base`.**
  `kubectl apply -k deploy/base` also applied the example into the target namespace,
  touching any workload matching its selector.
  It now lives at `deploy/` beside the Secret examples,
  as a template to customize per application namespace.

- **`config validate` states what a short PGO grace period costs.**
  The output said a shorter grace period loses no work;
  a drain waits through each Collection's deadline and abandons work still running there,
  a cut attempt's samples are dropped,
  and another replica retries only while the deadline and an attempt remain —
  otherwise the Collection fails as `deadline_exceeded` or `attempts_exhausted`.

- **The chart's install notes print API paths that exist.**
  The curl examples used `/v1/targets?namespace=<ns>&service=<svc>`, which matches no route;
  they now use `/v1/namespaces/<ns>/services/<svc>/targets`.

## [0.2.0] - 2026-08-25

Puts HTTPS on the API listener, with a certificate that rotates without a restart,
and makes a shutdown drain everything it promises to drain.

### Added

- **HTTPS on the API listener.**
  `server.tls` names a certificate and a key,
  and when both are set the API listener serves HTTPS on the same port under the same name.
  The two paths are set together or not at all,
  because half a pair would leave a listener that fails every handshake,
  and both files are opened at startup so a path typo names its key instead of surfacing per connection.
  `minVersion` accepts 1.2 or 1.3 and defaults to 1.2.
  Leaving the block unset keeps the listener plaintext for an Ingress or a mesh terminating TLS,
  which remains a supported topology,
  and the ops listener stays plaintext for the kubelet's probe and the metrics scraper.

- **Certificate rotation without a restart.**
  The pair is parsed once and handed to every handshake from an atomic pointer,
  and a goroutine re-reads both files every 30 seconds and swaps the pair only when the contents changed,
  so a cert-manager renewal is served without a rollout.
  A read or parse that fails leaves the pair already loaded in place,
  and re-reading rather than watching survives the kubelet's symlink-rename Secret updates.
  Two metrics make a rotation that quietly stopped working visible before the served certificate expires:
  `profgate_tls_reloads_total` and `profgate_tls_certificate_expiry_seconds`.
  An end-to-end scenario replaces the Secret with a certificate from a second authority
  and verifies the gateway serves it with no Pod restart.

- **TLS in the Helm chart.**
  `tls.enabled` mounts a `kubernetes.io/tls` Secret read-only
  and renders `server.tls` to point inside it.
  The port, its name, the Service, and the NetworkPolicy stay as they are;
  only the scheme changes.
  The chart deliberately adds no checksum annotation over the Secret,
  because a renewal must be re-read from disk, not roll the Deployment.
  The kustomize base keeps serving plain HTTP and gains a commented example Secret.

- **Drain visibility.**
  A finished drain now logs how long it took,
  whether the API listener closed on its own or on the deadline,
  how many requests a deadline close cut short,
  and whether the Collection drain finished or left Collections for another replica to reclaim,
  naming them.
  A drain still waiting on a Collection says so every 30 seconds,
  and a shutdown error that is not the deadline is logged rather than discarded.

### Fixed

- **Requests are no longer reset during a rollout.**
  Readiness turned 503 and the API listener closed in the same instant,
  which reset every request the endpoint controllers and the kube-proxies had not yet stopped routing here.
  The new `server.drainDelay` holds the listener open for that window:
  5 seconds by default, 60 at most, zero to turn it off.
  The gateway waits in process because the distroless image has no shell for a preStop hook,
  and the chart and the kustomize base raise `terminationGracePeriodSeconds` to 125 to match.

- **Discovery keeps moving through the drain.**
  The informers descended from the context the stop signal cancels,
  so discovery froze the moment the drain began,
  even though an in-flight Collection re-resolves its targets every round
  and a profile request confirms its Pod before it dials.
  The informers now run under a context of their own,
  cancelled only once the interactive and Collection waits have ended.

- **Claims that land during the drain are drained too.**
  The drain snapshotted the in-flight Collections once,
  so a claim past its capacity check but not yet committed owned nothing the snapshot could see,
  and the process could exit under a Collection still sampling and merging.
  The drain now refuses every later claim,
  waits for the claims already inside that window,
  and looks again after any wait before it returns.

- **A second stop signal cuts the drain short.**
  The second SIGTERM went into a buffer nobody read,
  which left SIGKILL as the only way to end a drain waiting on a merge
  that would outlast the operator's patience.
  The first signal still asks for the graceful drain;
  the second logs that the drain is being cut short and exits non-zero.

- **A fatal listener error restarts fast.**
  A listener failure ended the process through the same shutdown as SIGTERM,
  waiting without bound for in-flight Collections
  and spending the drain delay on an endpoint window that no longer received requests.
  A replica with no listener has nothing left to serve,
  so the fatal path now skips both waits,
  names the Collections it leaves running, and exits 1;
  they stop renewing their leases
  and another replica reclaims each one whose deadline has not passed and that has an attempt left,
  which is the documented recovery.

## [0.1.1] - 2026-08-25

Hardens the distribution: the image base, configuration loading,
the release gate, and installs on a private network.

### Added

- **Chart installation on a private network.**
  The chart README covered only a direct pull from GHCR,
  which a cluster without egress cannot do.
  It now covers three ways in:
  a proxy for the helm client,
  a one-time file transfer with `helm pull` and `skopeo copy --all`,
  and a standing mirror in an internal OCI registry that holds the chart next to the image.
  Each one names the version forms that differ,
  chart `X.Y.Z` against image `vX.Y.Z`,
  and the values an internal registry needs,
  `image.repository` and `imagePullSecrets`.

- **A guarded release task.**
  `mise run release -- vX.Y.Z` refuses a malformed version,
  a dirty tree, a `HEAD` that is not `origin/main`,
  a tag that already exists locally or on the remote,
  and a commit whose check and e2e runs on `main` did not both succeed,
  and only then creates and pushes the annotated tag that cuts a release.

### Changed

- **The image is based on distroless static.**
  `gcr.io/distroless/static-debian12:nonroot` replaces the Chainguard static image.
  Both bases carry the CA bundle profgate needs when `nats.url` uses `tls://`,
  but distroless offers a pinned tag that still receives security updates,
  where the Chainguard free tier tracks only `latest`.
  The runtime UID stays 65532, so no manifest changes.

- **Configuration loading adopts fuda v1.7.0.**
  The loader now tracks which keys the YAML document supplied,
  so a declared default no longer overwrites a value the operator wrote as the field's zero.
  Validation failures render as `<key>: <plain statement>`,
  for example `discovery.pprof.port: must be at most 65535`.

### Fixed

- **The PGO sampling defaults gained floors.**
  Four keys had a default but no lower bound:
  `pgo.defaults.sampling.duration`, `rounds`, `maxParallel`, and `pgo.defaults.artifact.retention`.
  A zero duration samples nothing,
  and zero rounds or zero `maxParallel` describes a Collection that does no work.
  Each field now refuses values below the floor an operator override was already held to:
  1s, 1, 1, and 1m.
  `pgo.defaults.sampling.roundInterval` keeps accepting zero,
  which is a setting an operator can mean.

## [0.1.0] - 2026-08-25

The initial release: a Kubernetes-aware pprof gateway for Go workloads,
and PGO CPU-profile collection layered on top of it.

### Added

- **The pprof gateway.**
  One HTTP entry point resolves a Kubernetes Service to its backend Pods,
  using EndpointSlice discovery with strict eligibility rules,
  and proxies eight profile types (`cpu`, `trace`, `heap`, `allocs`, `goroutine`, `mutex`, `block`, `threadcreate`)
  over a pinned HTTP client that confirms the Pod before it dials.
  The `/v1` API lists a Service's eligible targets and fetches a profile from a named or randomly selected Pod,
  with version restriction and a bounded duration for `cpu` and `trace`.

- **Access realms.**
  Authorization is static realms loaded from process configuration:
  which namespaces a caller may reach is decided per realm,
  and nothing the gateway emits reveals a Pod IP, a pprof port,
  or a name the caller's realm denies.

- **A read-only Kubernetes footprint.**
  Profgate requires no Kubernetes write permissions:
  it observes Services, Pods, and EndpointSlices in authorized namespaces
  and connects only to explicitly permitted application pprof ports.
  The deployment manifests pin the matching read-only RBAC,
  and a startup preflight confirms the granted permissions before serving.

- **PGO CPU-profile collection.**
  Scheduled and on-demand Collections gather representative CPU profiles for Profile-Guided Optimization:
  multi-round sampling across a Service's Pods,
  an in-memory merge, and retained artifacts.
  Replicas coordinate through dedicated NATS JetStream KV and Object stores,
  with leases, reclaim of Collections whose owner died,
  and a sweeper for expired state,
  while profile bytes stay ephemeral.
  Interactive profiling and PGO sampling share one admission gate,
  and the `/v1` API manages per-Service PGO policies and Collections end to end.

- **Deployment surfaces.**
  A Helm chart and a kustomize base with pinned RBAC,
  NATS account provisioning and credentials mounting for PGO,
  and a release workflow that publishes the image and the chart to GHCR on every tag.

- **Observability and operations.**
  Prometheus metrics for the gateway and the PGO loops,
  a JSON audit record for every `/v1` request,
  a configurable server log level,
  and a CLI with `version` and `config validate`.

- **End-to-end proof on real clusters.**
  kind-based e2e lanes exercise the gateway and PGO collection against real clusters,
  frozen Kubernetes 1.23 and 1.24 images and the current Kubernetes release,
  matching the 1.23 compatibility baseline.

[Unreleased]: https://github.com/arloliu/profgate/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/arloliu/profgate/releases/tag/v0.5.0
[0.4.0]: https://github.com/arloliu/profgate/releases/tag/v0.4.0
[0.3.0]: https://github.com/arloliu/profgate/releases/tag/v0.3.0
[0.2.0]: https://github.com/arloliu/profgate/releases/tag/v0.2.0
[0.1.1]: https://github.com/arloliu/profgate/releases/tag/v0.1.1
[0.1.0]: https://github.com/arloliu/profgate/releases/tag/v0.1.0
