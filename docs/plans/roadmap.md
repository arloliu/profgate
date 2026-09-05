# Roadmap

**Status:** Approved

> **How to read this document:** it orders the work that follows `v0.5.0`,
> so that each item is taken up in turn and nothing is started out of order.
> It is not an implementation plan:
> an item that changes behavior first revises the spec it names,
> and only then gets a plan of its own under `docs/plans/`.
> Items that change no behavior — a docs repair, a chart template, an alert rule — need no spec and are executed directly.
> A ticked bullet records that the design decision it carries is settled in the spec that item names,
> whatever the bullet's wording:
> a bullet phrased as behavior is ticked once the revision that settles that behavior is in the spec.
> A bullet the item's named spec does not cover is ticked when the work itself is done,
> because there is no revision a tick could record for it instead.
> Whether a settled decision has also shipped is the item's own `Shipped:` line,
> which names the version that carries it, the pull request that landed it, or says it is not built yet.
> A ticked item that has not shipped is an ordinary state rather than a contradiction:
> the design is settled and the code is still to write.
> That line is where the answer lives because a plan under `docs/plans/` is deleted once its work lands,
> so a finished item leaves nothing there to read.

**Goal:** add no feature.
Make what `v0.5.0` ships installable without a stumble,
discoverable from a terminal,
honest in what it reports when it fails,
and stable under the clients and stores it actually meets.

**Sources:** the review that produced this ordering is
[`2026-09-03-usability-and-stability.md`](../investigations/2026-09-03-usability-and-stability.md);
every claim below is restated against the committed file it cites.
That review found the mechanisms sound — CAS and lease discipline, the store-generation barrier,
admission release, every reload path's last-good, metric cardinality, the OpenAPI drift tests —
and the defects in what an operator sees first.

## Ordering Principle

Earlier items are cheaper, are met sooner by a first-time operator, or unblock later items.
Within the list: the install path, then the surfaces a person touches every day,
then what the program says about itself when it fails,
then the defects a reproduction already demonstrates,
then the gates and specs that keep all of it true.
An item may be reordered only by editing this list, never by starting later work first.

## Items

### 1. Make the install path hold on a fresh cluster

- [x] `kubectl apply -k deploy/base` fails on a cluster without a `profgate` namespace:
  no Namespace object exists in `deploy/base/`, and every namespaced resource names one (`docs/deployment.md:44`).
  Add the Namespace to the base, or tell the reader to create it first.
- [x] The chart's `memoryLimitWithoutPGO` guard runs only on the PGO-on branch
  (`deploy/chart/profgate/templates/_helpers.tpl:252-272`),
  and the PGO-off branch takes the value as written (`:302-308`);
  `--set memoryLimitWithoutPGO=512` on the default branch renders `memory: 512` bytes and the Pod is OOM-killed.
  Guard both branches and test the default one (`deploy/chart_test.go:613-616` covers only PGO on).
- [x] `auth.mode=basic` with no users and `auth.mode=oidc` with no issuer render a Deployment that crash-loops;
  `ingress` without hosts and `pgo` without `nats.url` are refused at render time.
  Refuse the two auth cases the same way.
- [x] `NOTES.txt` says nothing about the backend-protocol annotation
  when `ingress.enabled` and `tls.enabled` are both set,
  though `values.yaml:69-74` and `docs/deployment.md:118-124` both warn that the Ingress fails without it;
  in `basic` mode it promises a list of users and prints realms (`NOTES.txt:17-19,28,47-49`).
- [x] The upgrade section of `docs/deployment.md` names no breaking change and nothing in `README.md`,
  `docs/deployment.md`, or the chart README links `CHANGELOG.md`,
  so `0.5.0`'s five breaking changes reach an operator only by accident.
  The port one is the trap: `discovery.pprof.allowedPorts` and `allowedPortNames` are removed,
  so a `0.4.x` configuration that still sets either does not start,
  and an empty `allowedSelections` admits only the configured default.
- [x] `README.md` does not link `docs/cli.md` or say the binary is also a client,
  and its quickstart gives no way to discover a namespace or a Service name.
- [x] `deploy/base/deployment.yaml` reserves 1536Mi with PGO off where the chart reserves 512Mi,
  its comment describes a PGO block the ConfigMap comments out, and it pins `:latest`.
  Say which surface does what, and pin the tag.

Spec: none.
Shipped: pull request #19.
Why first: every item here is met in the first fifteen minutes, and each is a docs or template change.

### 2. Make `config validate` tell the truth

- [x] `GatewayMemoryBytes` (`internal/config/config.go:541-556`) never reads `pgo.enabled`,
  so `config validate` prints a PGO-sized container limit for a gateway with PGO off,
  three times what the chart renders; `deploy/chart_test.go:604` already subtracts the working set to work around it.
  The code is the bug: with PGO off the limit is `memoryLimitWithoutPGO` alone, as `docs/deployment.md:390` says.
- [x] The "complete configuration" example in `docs/configuration.md:670-673` raises three `pgo.limits` values above the defaults
  and the sentence at `:706` says every value is the shipped default.
- [x] YAML decode errors name a Go type instead of a key path and omit the file name every other error carries
  (`field logLevl not found in type config.ServerConfig`), against the promise at `docs/configuration.md:14-17`.
- [x] `discovery.pprof.port: 0` is accepted as "unset" (`internal/config/config.go:87,722`)
  where the doc says `1` to `65535`.
- [x] Eight documented `PROFGATE_AUTH_OIDC_{BROWSER,CLI}_*` overrides are dropped without a word
  when the file lacks the block;
  `realms.<name>.profiles` refuses an entry without listing the eight accepted names.
  State the rule for pointer blocks in `docs/configuration.md:22-31`, and list the names.
- [x] Four shipped defaults sit on their own ceiling, so narrowing one ceiling fails startup until a second key moves;
  `pgo.limits.maxRetention` documents a `720h` maximum that the shipped `jobRetention` caps at `167h`.
  Document the pairs.

Spec: the configuration table in [`gateway.md`](../specs/gateway.md), which gives `discovery.pprof.port` as `1–65535`;
the paragraph in [`pgo.md`](../specs/pgo.md) that describes what `config validate` prints
and gives a gateway replica a static limit no `pgo.limits` key enters;
and [`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md),
which sizes the in-process collector as the gateway's footprint plus the working set.
Shipped: pull request #20.
Why here: `config validate` is the one tool an operator runs before every rollout,
and it disagreed with the chart.

### 3. Give every CLI verb a `--help` and an honest table

- [x] No client verb answers `--help`: the flag set writes to `io.Discard` (`cmd/profgate/client.go:300`)
  and nothing handles `flag.ErrHelp`, so `--help` prints `flag: help requested` and exits 2.
  The eleven global flags appear in no output the binary can produce.
  [`cli.md`](../specs/cli.md) *Help* gives the shape:
  `-h` and `--help` print the grammar line and the command's own flags on stdout and exit 0,
  the global flags beside a client verb's own and on no operator command line,
  one shape for every command line the binary has.
- [x] `profgate auth hash --help` prints `Password:` and waits on stdin forever (`cmd/profgate/auth.go:28-40`).
- [x] `-n` and `-o` are the kubectl reflexes; `-n` failed without naming `--namespace`,
  and `-o json` on `profile` wrote a pprof file called `json`.
  The error names the long flag,
  and `-o json` or `-o yaml` on `profile` and `download` is refused before a request is sent.
- [x] `collection get` prints `round 0 of 1` for a completed Collection:
  `progress.round` is a zero-based index (`internal/pgo/rounds.go:144-145`)
  and `cmd/profgate/collect.go:366` prints it raw.
  Its table drops `expiresAt`, `finishedAt`, `resolvedVersion`, and `artifact.bytes`,
  so nothing says how long `download` will still work.
- [x] `collect --wait` prints the receipt and the final record on stdout in table mode
  and never prints the receipt under `--output json` (`cmd/profgate/collect.go:225-229`);
  `docs/cli.md:316` and [`cli.md`](../specs/cli.md) *Collections* both say only the final record goes to stdout.
  The receipt goes to stderr in both modes.
- [x] `targets --explain` on a Service whose selector matches no Pod prints two empty headers;
  the table never shows `selectorMatched`, the number that separates "selects nothing" from "selected and all excluded".
- [x] Under `--output json` an error is still one text line on stderr and nothing on stdout (`cmd/profgate/exit.go:48`);
  a `2xx` with a body that is not an envelope prints `profgate: HTTP 200 OK` (`internal/client/client.go:168`);
  `services <unknown namespace>` prints a header and exits 0;
  `collect` and `pgo policy set` name one concept twice.
  [`cli.md`](../specs/cli.md) defines each: the envelope's bytes on stdout under `--output json`,
  a fixed line naming the status and nothing the response carried,
  the header and exit 0 kept as the honest rendering of a `200` with an empty list,
  which is what the gateway answers for a namespace it does not know — there is no `namespace_not_found` —
  and `--file` as the one flag name on both.
- [x] Small wording: `logout` is silent on success and loud when nothing is cached;
  `login --context` creates a context and does not select it, and says nothing about `context use`;
  `context delete` of the current context is silent;
  an expired cached token still triggers the plaintext warning before "no valid token";
  the contexts-file refusal leaks `client.File`;
  `docs/cli.md:192` says `key: value` where the rendering is a tab.
- [x] Gateway messages the client relays verbatim name no next step where the client has a verb for it:
  `port_not_allowed`, `collection_in_progress`, `no_targets`, `pgo_disabled` (`internal/httpapi/server.go:448`).
  The gateway's text names the endpoint or the configuration key; the client stays verbatim.

Spec: [`cli.md`](../specs/cli.md) for `--help`, the JSON error shape, the malformed-body message,
the unknown-namespace answer, and the flag name; none for the rest.
Shipped: pull request #21.
Why here: the CLI is `v0.5.0`'s headline, and its first contact — `--help` — fails on every client verb.

### 4. Make the gauges and the alerts true, and write the runbook

- [x] With `pgo.enabled` and NATS unreachable, `profgate_pgo_synced` is absent rather than `0`:
  the gauge registers inside `startPGO` (`cmd/profgate/serve.go:668`), reached only after a preflight
  that retries `ErrUnavailable` forever (`:597-614`).
  `ProfgatePGONotSynced` is silent through the outage it names while `/readyz` stays 503.
  A `profgate_nats_connected == 0` rule inside the existing `pgoEnabled` guard covers it without a code change.
- [x] `profgate_tls_certificate_expiry_seconds` registers unconditionally (`internal/metrics/prometheus.go:150-153`)
  and is written only under `server.tls` (`internal/tlscert/loader.go:149`),
  so a default install reads `0` — a certificate that expired at the epoch —
  and no threshold rule can keep the promise at `docs/deployment.md:218-219`.
  Seed it `NaN`, as `jwksAge` already is.
- [x] `profgate_discovery_synced` moves once, `0` to `1`, at `cmd/profgate/serve.go:475`;
  `ProfgateNotReady` (`deploy/chart/profgate/templates/prometheusrule.yaml:22-31`) says "not serving"
  where three of the four readiness gates (`serve.go:172-173`) can be red with the gauge at `1`.
  Either the rule's wording narrows to what the gauge means, or the gauge follows `ready()`.
- [x] Six failure modes have a metric and no alert: TLS reload failing, certificate expiry,
  NATS disconnected while running, authenticator unavailable, every upstream refusing, the auth limiter saturated.
  Ship the rules and the table row for each.
- [x] `docs/deployment.md` and `docs/authentication.md` have no troubleshooting section;
  the only failure table is `docs/specs/gateway.md:2439`, which names behavior and almost never a signal.
  Write the section: symptom, the metric or log line, the recovery step —
  including a bucket deleted or recreated under a running process (`docs/specs/pgo.md:2883-2888`),
  a restore from backup, NATS maintenance,
  and the fact that any NATS disconnect aborts the Collections a replica owns and costs each an attempt (`docs/specs/pgo.md:1450`),
  which `docs/pgo.md:310-316` and `docs/deployment.md:356` describe only as an outage.
- [x] The `code` label's value set is undocumented:
  forty envelope codes, seven audit-only outcomes, and the `upstream_<status>` family
  (`internal/httpapi/codes.go:11-105`, `internal/proxy/proxy.go:171`); `docs/deployment.md:439,464` names one.
  `profgate_confirm_total` and `profgate_profiles_in_flight` observe the interactive path only (`internal/pgo/rounds.go:448`);
  say so at `docs/deployment.md:441-442`.
- [x] A failed PGO sample logs at debug with no Collection identifier (`internal/pgo/rounds.go:411-412`);
  `authenticator failed` (`internal/httpapi/auth.go:86`) carries no `requestId`,
  and `idempotency receipt is not readable` (`internal/httpapi/pgo_collections.go:633`) carries neither that nor the Service.
- [x] One page of example queries under `docs/deployment.md`, so an operator building a dashboard knows which series to plot.
  A dashboard file is a feature and stays off this list.

Spec: [`gateway.md`](../specs/gateway.md) *Metrics* for the `NaN` seed of the expiry gauge,
the meaning of `profgate_discovery_synced`, and the three cases `profgate_nats_connected` reports;
none for the rest.
Shipped: pull request #24.
Why here: three gauges read wrong or read nothing in exactly the outage they exist for,
and an operator on call has no page that says what to look at.

### 5. Bound what a slow client can hold on the interactive path

- [ ] A client that stops reading holds the handler, its admission slot, and the Pod connection past the request budget:
  `internal/proxy/proxy.go:173` blocks in `w.Write` and `cmd/profgate/serve.go:234` sets only `ReadHeaderTimeout`.
  Sixteen such clients — the default `limits.maxConcurrentProfiles` — answer every later profile request `429`.
  Reproduced: a two-second budget, a handler still blocked at eight seconds;
  with a write deadline at the budget it returned at two.
  `docs/specs/gateway.md:981-983` already promises the budget bounds body streaming; the code catches up.
- [ ] A PGO route reads its JSON body with no deadline once the headers are in
  (`internal/httpapi/pgo.go:206`); a body dripped one byte at a time holds a handler goroutine indefinitely.
  Reproduced: a handler blocked in `decodeBody` until the client hung up, and back in one second with a read deadline.
  A read deadline on the small-body routes through `http.ResponseController`, set before the read and cleared after;
  the write deadline above is the same idiom on the streaming side.
  A server-wide `ReadTimeout` or `WriteTimeout` is not the fix: it cancels a long profile handler.
- [ ] client-go reflector failures go to stderr as text, outside `server.logLevel` and the JSON contract at `docs/specs/gateway.md:1443`;
  a watch that keeps failing after the first sync is invisible everywhere else.
  Reproduced: every Pod list failing, `HasSynced` false, the gateway log empty.
  Route klog through `slog` in `internal/k8s`.
- [ ] Neither server sets `ErrorLog` (`cmd/profgate/serve.go:234-235`),
  so TLS handshake failures and recovered panics print through `log.Printf`;
  neither sets `IdleTimeout`;
  the upstream transport (`internal/proxy/proxy.go:86-90`) sets no `IdleConnTimeout` and no global `MaxIdleConns`.
- [ ] Two outcomes are misattributed: a client gone during the confirmation read counts as
  `profgate_confirm_total{result="unavailable"}` and audits `503 discovery_unavailable` (`internal/k8s/confirm.go:44`);
  a connection the drain deadline closes audits `client_gone` (`internal/proxy/proxy.go:174`).
- [ ] Every fatal startup path sleeps `server.drainDelay` before exiting (`cmd/profgate/serve.go:380-383`)
  though `/readyz` never turned green; `docs/specs/gateway.md:1635` gives the reason to skip the window,
  and it holds here.

Spec: [`gateway.md`](../specs/gateway.md) *Network* for the write deadline, the body read deadline, and the idle timeouts;
*Logging* already covers the reflector records.
Shipped: not built yet.
Why here: the first bullet is reproduced and reaches the default limit with sixteen idle sockets.

### 6. Bound what NATS can hold on the PGO path

- [ ] An artifact transfer inherits the five-second call deadline over the whole stream
  (`internal/natskv/client.go:24,815,831-854`): a download the client cannot drain in five seconds is cut mid-body,
  and nothing is tunable.
  Reproduced: 64 KiB of a 2 MiB object, then `nats: timeout`.
  The short deadline bounds establishment;
  the bytes follow the request for a download and the committed lease for an upload.
- [ ] Watch re-open retries every 50 ms with no backoff, no jitter, and no bound (`internal/natskv/client.go:27-28,694-714`);
  reproduced at 58.7 failed opens per second for the three `PROFGATE_JOBS` watches while the bucket is absent.
  Process-level retries (`cmd/profgate/serve.go:515`) back off without jitter, so replicas retry in step.
  `docs/specs/pgo.md:2863` says "a fixed interval";
  the revision says capped exponential backoff with jitter, reset after a completed replay.
- [ ] `Drain` reads each owner's cutoff once (`internal/pgo/worker.go:232-246`);
  a renewal already past `stopping()` that then succeeds extends the durable lease and the drain timer does not follow.
  Reproduced: `Drain` returned with 24 seconds of lease left and the work uncancelled.
  The timer re-reads the cutoff when it fires.
- [ ] A replica that reclaims its own aborted Collection has the second owner's `inFlight` entry deleted by the first owner's exit
  (`internal/pgo/worker.go:469-479`), and `Drain` returns without waiting; reproduced with `maxActiveCollections` at two.
- [ ] Publication runs under the request context (`internal/httpapi/pgo_collections.go:572`);
  a client gone between the first and last write leaves an `initializing` record
  that blocks the Service for about 65 seconds.
  Publication finishes under its own bounded context once the first write has landed.
- [ ] A `completed` to `expired` transition that fails to persist for a reason other than a lost race is dropped without a record
  (`internal/pgo/runtime.go:453`, `internal/httpapi/pgo_collections.go:1079`, `internal/pgo/sweeper.go:255`);
  the probe sweep skips a cut listing without a record (`internal/pgo/sweeper.go:420-424`).
  Log and count the unexpected case; leave the lost race silent.
- [ ] The worker scan re-reads every nonterminal record on every `job.*` delivery (`internal/pgo/worker.go:167-181,285-299`),
  quadratic in live Collections.
  Carry `LeaseUntil`, `ClaimBy`, and `Deadline` in the cache entry so a pulse can skip what it cannot claim,
  and index jobs per Service so a one-Service listing stops allocating and sorting against every retained record under the cache lock
  (`internal/pgo/caches.go:743,816`; 31 ms and 10 MiB per listing at the default on-demand ceiling held for a week).

Spec: [`pgo.md`](../specs/pgo.md) for the transfer deadline, the re-open backoff, and the publication context.
Shipped: not built yet.
Why here: the first two bullets are reproduced; the rest are the same package and the same plan.

### 7. Make the console safe to click and honest about what it shows

- [ ] A mouse double-click on **Start collection** arms and confirms in one gesture and creates a Collection
  (`internal/ui/static/app.js:622-633,1203-1212`); **Cancel** has the same shape.
  `docs/specs/ui.md:661-663` requires two presses; the code catches up — the confirm control is not placed
  where the first press landed,
  or ignores a press within a short window of arming.
- [ ] **Download** with no target does nothing visible; the gateway answers `503 no_targets` and the page shows nothing
  (`app.js:901-912,1145`).
  Disable the control when the target summary is empty, and show the envelope when a download fails.
- [ ] The Collections table sits in one grid column beside 900 px of empty page (`app.css:9-14`, `app.js:930-937`);
  `state`, the timestamps, and **Cancel** are off-screen at 1600 px.
- [ ] Nothing refreshes: a Collection started from the page never leaves `pending` without a reload (`app.js:471-503`),
  and the list shows one page of at most 100 with `nextCursor` dropped (`app.js:505-521`).
  `docs/specs/ui.md` says nothing about either.
  The revision adds a **Refresh** control on the two lists —
  the one control this roadmap adds, because a Collection started from the page cannot otherwise be watched from it —
  and has the list say that older Collections exist and which CLI verb lists them, rather than adding a paging control.
- [ ] **Keep** and **Confirm start** render in the same primary blue:
  the vendored classless Pico has no `.secondary` rule (`app.css:78-82`).
- [ ] `loginURL` omits the 1024-byte return-path bound
  that `docs/specs/ui.md:482` requires (`internal/ui/static/urls.js:103-106`).
- [ ] The `hints` table lacks `too_many_auth` and `auth_unavailable` (`app.js:55-73`),
  and the `realm_denied` hint promises an identity panel that a listing `403` never refreshes (`app.js:385-420`).
- [ ] The page has no heading and no pointer to the CLI verb that does the same job.
- [ ] `docs/console.md:104` says **Copy URL** is absent on an HTTP page,
  and the port-forward recipe at `:23-30` is a secure context where it appears;
  `:123` says **Keep** restores the control, which holds only before the first request;
  `:141` describes the `v0.4.0` to `v0.5.0` asset move without naming it.

Spec: [`ui.md`](../specs/ui.md) for the refresh control, the disabled download, the download error, and the paging notice;
none for the rest.
Shipped: not built yet.
Why here: the console is the surface a person clicks by reflex, and today a reflex creates a Collection.

### 8. Close the gates that do not run

- [ ] `TestRoundsDecodeHeapDelta` skips under `-race` (`internal/pgo/rounds_test.go:818-821`),
  and every test command in `mise.toml:33` and every workflow passes `-race`; the decoder memory guard has never run.
- [ ] `.github/workflows/check.yml:1-13` runs `check`, lint, and unit tests on `push` only;
  a pull request from a fork fires `pull_request` alone and gets the `current` e2e lane and prose.
  `docs/specs/gateway.md:2098-2103` describes the split; the revision runs the unit gates on `pull_request` too.
- [ ] `If-Match` is required by `PUT` and `DELETE` on the policy route (`internal/httpapi/pgo_policy.go:112,203`)
  and declared nowhere in `internal/httpapi/openapi.json` but its prose;
  `TestOpenAPIDocumentParameters` walks `query` and `path` only.
  Declare the header and extend the test to `header`.
- [ ] `.agents/rules/900-design-and-review-loops.md:32` says no CI invokes `mise run check`;
  `check.yml` has since `2026-08-23`.
- [ ] `docs/api.md:121,1002` states route counts in prose that nothing pins;
  a `check-repo.py` rule pins them to `routes.go`.
- [ ] `docs/decisions/e2e-without-framework.md` records that its size trigger fired;
  `test/e2e/harness_test.go` is 1,676 lines and unsplit.

Spec: [`gateway.md`](../specs/gateway.md) *Continuous integration* for the pull-request gates; none for the rest.
Shipped: not built yet.
Why here: a gate that does not run is a claim the repository makes and does not keep.

### 9. Say in the spec what is not built

- [ ] `docs/specs/pgo.md:1190-1242,3749-3800` describe a collector Deployment, a heartbeat, a gauge, and `503 collector_unavailable`
  that no code implements; `internal/httpapi/codes.go:92` is dead,
  and the OpenAPI enum and the console's `hints` carry the code.
  The deferral lives only in `docs/decisions/collection-stays-in-the-gateway.md`.
  The spec marks the amendment deferred where it stands, so a reader need not know to discount it;
  the dead code and its enum entry are removed with a changelog line.
- [ ] `docs/pgo.md:147` says the listing pages and `:263` says it offers no pagination; it pages.

Spec: [`pgo.md`](../specs/pgo.md), and [`ui.md`](../specs/ui.md) where it answers `collector_unavailable` (`docs/specs/ui.md:783-794`).
Shipped: not built yet.
Why last: nothing here changes behavior,
and the removal is a breaking change to the enum that a release note must carry.

## Not on This List

- A build version on any `/v1` response, a Grafana dashboard file, an HPA, any new route or chart resource.
- Turning `pgo.configAPI` into a boolean or `limits.cpuSeconds` into a duration:
  both are breaking changes whose only return is consistency.
- Replacing `deadlineGuard` in `internal/k8s`: its production trigger is a transport that ignores its context,
  which client-go does not do.
- Rendering profiles, non-Go producers, multi-cluster, continuous profiling, long-term profile storage.

## Validation

Every item that lands code ends with:

```bash
mise run lint && mise run test && mise run check
```

Prose uses semantic line breaks; run `semlf check` on what you wrote.
