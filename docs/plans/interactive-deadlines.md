# A Slow Client Holds Nothing Past the Budget

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> On this machine `mise run lint` runs a golangci-lint 2.1.6 that shadows the pinned 2.12.2,
> so every validation block below runs the linter as `mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`
> and never as `mise run lint`.

**Goal:** make every wait on the interactive path end when the gateway says it ends, not when the client chooses.
A client that stops reading a profile holds the handler, its admission slot, and the Pod connection today for as long as it keeps the socket open,
and sixteen such clients — the default `limits.maxConcurrentProfiles` — answer every later profile request `429`.
A PGO request whose JSON body arrives one byte at a time holds a handler goroutine with no bound at all.
A keep-alive connection that sends nothing is held until the process exits,
the upstream transport's idle pool grows with every Pod ever profiled,
`net/http`'s own lines go to stderr as text, and so do client-go's reflector failures,
outside `server.logLevel` and the JSON contract.
Two outcomes are misattributed:
a client gone during the confirmation read counts as an unavailable API server,
and a connection the drain bound closes is audited as a client that left.
Every fatal startup path sleeps `server.drainDelay` though `/readyz` never answered 200.
After this plan each of those has a bound or a true name,
no configuration key is added, and no request that behaves well sees a difference.

**Architecture:** the upstream transport in `internal/proxy/proxy.go` bounds its idle pool in size and age,
and `Proxy.Do` arms a write deadline on the client connection at the budget's end the moment it commits a response;
`internal/httpapi` bounds the body reads and probe reads of the PGO routes with a ten-second read deadline,
gains one exported constant both listeners read for `ReadHeaderTimeout`,
owns the drain's cancellation cause and reads it once, at the layer every route shares,
and replaces the never-written `cancelled` among its audit-only outcomes with `drain_expired`;
`cmd/profgate/serve.go` sets `IdleTimeout` and `ErrorLog` on both servers,
hands the API server a drain context every request context derives from,
cancels it with that cause before it closes connections,
waits for the informers to stop before it returns,
and spends `server.drainDelay` only after `/readyz` has answered 200;
`internal/k8s` installs the process's `slog` logger as klog's sink once, when the runtime is built and before any client or informer exists,
and returns a caller's cancellation from `Confirm` as a cancellation rather than `ErrDiscoveryUnavailable`;
`internal/metrics` documents `client_gone` as a result of `profgate_confirm_total`.
`go.mod` gains no module: `k8s.io/klog/v2` moves from indirect to direct.
No route, chart value, configuration key, metric, or Kubernetes permission moves.

**Spec:** every behavior here is accepted text in [`gateway.md`](../specs/gateway.md):
*Network* for the listener timeouts, the body and probe read deadline, and `ErrorLog` at `error` (`docs/specs/gateway.md:239-255`);
*Confirmation before connecting* for a client gone during the read and its `result="client_gone"` count (`:696-700`);
*Proxy behavior* for the bounded pool, the write deadline at commit, and the `drain_expired` row
(`:997-1003`, `:1015-1026`, `:1051-1054`);
*Logging* for klog at the level client-go emits, in the three cases it names (`:1533-1540`);
*Metrics* for the fourth `result` value (`:1572`);
*Startup and shutdown* for the drain context, its cause, and the exits that skip `server.drainDelay` (`:1708-1719`, `:1758-1760`);
*Failure Scenarios* for the drain bound ending with a request in flight (`:2574`);
and the amendment block that lists them (`:2902-2929`).
[`pgo.md`](../specs/pgo.md) carries the `drain_expired` row of the download's outcome table (`docs/specs/pgo.md:2474`)
and lists it among the audit-only codes (`:2603`),
and `docs/deployment.md:488-490` names the eight audit-only values, `drain_expired` among them and `cancelled` no longer.
This work is ordered by [`roadmap.md`](roadmap.md),
under *Bound what a slow client can hold on the interactive path*.
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the system, not as the defects that revealed them.

- **Every wait on the interactive path has a bound the gateway set.**
  The overall budget already bounds confirmation, dial, header wait, and body streaming
  (`internal/httpapi/server.go:713-715`);
  a write that blocks on a client that stopped reading is the one wait inside that budget the budget cannot end,
  because `io.Copy` at `internal/proxy/proxy.go:173` blocks in `w.Write`, which reads no context.
- **A request body is read under a deadline, or not at all.**
  `decodeBody` reads at most 64 KiB (`internal/httpapi/pgo.go:207`) and the probe read reads one byte (`:246`);
  neither has a bound once the headers are in, because `ReadHeaderTimeout` ends at the headers.
- **A connection that sends nothing is closed, not held, and a pool is bounded in size and age.**
  With `IdleTimeout` unset, `conn.serve` parks in `Peek(4)` with no read deadline between two requests on one connection;
  with `MaxIdleConns` and `IdleConnTimeout` unset, the upstream transport keeps every idle connection until its peer closes it.
- **Everything the process says goes to stdout as JSON under `server.logLevel`.**
  Neither server sets `ErrorLog` (`cmd/profgate/serve.go:234-235`),
  and klog's default sink is stderr text.
- **An audit code names what happened, not which timer noticed.**
  A client that left is `client_gone`; the drain cutting a request is `drain_expired`;
  an API server that could not answer is `discovery_unavailable`.
  `profgate_confirm_total` counts every attempt under the result that ended it.
- **The documented `code` set is the set the binary writes.**
  A value documented and never written misleads every query written against it.
- **The endpoint-removal window is spent only when something routes here.**
  `server.drainDelay` exists so requests already routed to this replica are served rather than reset
  (`docs/specs/gateway.md:1734-1737`);
  a replica `/readyz` never admitted was never routed to.

---

## Decisions

Nine choices settle how the spec's text is carried, and the Go and client-go facts that shape each test.

**The write deadline is armed when the response is committed, and the handler never clears it.**
That is the spec's text (`docs/specs/gateway.md:1015-1026`), and the reason its tests look as they do is in Go 1.26.7.
`conn.serve` runs `c.rwc.SetWriteDeadline(time.Time{})` right after `w.finishRequest()` for every request,
whether or not `WriteTimeout` is set
(`$(go env GOROOT)/src/net/http/server.go`, the statement following `w.finishRequest()` in `conn.serve`, line 2075 in 1.26.7);
the only deadline `readRequest` arms is under `WriteTimeout` (`:987-989`), which the gateway leaves zero.
So the flush after the handler runs under the deadline the handler set,
and the next request on the connection starts clean,
and a test that sends a second request on the same connection after a normal response is green today and after this plan;
this plan writes no such test.
Nothing is written to the client before the response is committed,
and the error envelopes the handler writes after a budget expiry —
the `504 upstream_timeout` at `internal/httpapi/server.go:755` and the `503 discovery_unavailable` at `:730-734` —
run with no deadline armed and fit the socket's send buffer, so a client that has stopped reading cannot hold them.
The bound is the same as the spec's: the budget's end, read from `ctx.Deadline()`,
and the arming point is where a write can first block.

**The read deadline is cleared after a full read and kept after a failed one, and a deadline's error is a fixed message.**
That is the spec's text (`docs/specs/gateway.md:243-251`).
After the handler returns, `finishRequest` closes the request body,
and a body not read to EOF is read up to 256 KiB looking for one
(`$(go env GOROOT)/src/net/http/transfer.go:984-999`, `maxPostHandlerReadBytes` at `server.go:1102`, `doEarlyClose` set at `:1038`);
against the same stalled client that read blocks with no deadline unless the handler's is still armed,
and with it armed in the past the read fails at once and the connection is closed rather than reused.
`conn.serve` overwrites the read deadline before it waits for the next request on the connection
(`$(go env GOROOT)/src/net/http/server.go:2091-2107` in 1.26.7),
so the clear after a full read costs nothing and hides nothing.
The error a deadline produces is a `*net.OpError` whose text names both socket addresses;
`decodeBody` today folds any read error into the envelope message (`internal/httpapi/pgo.go:214`),
so a deadline is mapped with `errors.Is(err, os.ErrDeadlineExceeded)` to a fixed message that names the bound and nothing else.
The probe read of `rejectBody` (`:244-253`) takes the same deadline and, on a failed read, the same answer;
today it drops the error and treats a failed read as no body (`:246-252`).

**One exported constant, read by both listeners and the body reads, with test seams on the handler's state.**
The spec fixes the body deadline as "the same constant as the listener's `ReadHeaderTimeout`" (`docs/specs/gateway.md:245`).
`readHeaderTimeout` lives in `cmd/profgate/serve.go:43-44`, which `internal/httpapi` cannot import.
`internal/httpapi` exports `RequestReadTimeout`, ten seconds,
documented as how long a client gets to deliver its request headers and, on a body-reading route, its body;
both servers read it for `ReadHeaderTimeout` and the constant in `cmd/profgate` is deleted, so the number exists once.
A test shortens it through a field on the handler's state that `export_test.go` sets,
the seam `setBeforeAllowlist` already is (`internal/httpapi/export_test.go:5-9`);
the same seam carries `budgetGrace` (`internal/httpapi/server.go:29-31`),
without which no real-socket test of the write deadline finishes under thirty seconds.
`IdleTimeout` gets the shape `tlsRefresh` and `authPoll` already have on `serveDeps` (`cmd/profgate/serve.go:92-93`),
and `IdleConnTimeout` gets the shape `HeaderDeadline` has on `proxy.Options` (`internal/proxy/proxy.go:70-75`):
a field production leaves zero and a test shortens.

**klog is installed once, when the runtime is built, and klog itself is the memo of what is installed.**
klog's setter mutates unsynchronised package state (`k8s.io/klog/v2@v2.140.0/contextual.go:72-80`)
and its contract is that no goroutine logs while it runs (`:57-58`);
a reflector logs from its own goroutine, so the install has to precede every informer and never repeat while one runs.
Production builds one runtime per process, at `cmd/profgate/serve.go:151`,
before `NewClientset` and before `cluster.Run` starts at `:472`;
that is where the install goes, in `NewRuntime` before the client and in `NewRuntimeWithClientset` before the cluster,
through one helper, and nowhere in `Cluster.New`, which every test that builds a cluster calls.
The helper does not write when the handler asked for is the one already installed:
`klog.Background()` returns the installed logger (`contextual.go:177`),
`logr.ToSlogHandler` returns the `slog.Handler` behind a logger `FromSlogHandler` built (`github.com/go-logr/logr@v1.4.3/slogr.go:59`),
and equal handlers mean nothing to change.
So the memo is the state klog already holds, and this repository adds none:
[`200-coding-standards.md`](../../.agents/rules/200-coding-standards.md) admits no package-level `sync.Once` outside tests,
and the one state it does admit is held the same way, as a local of `serve` that is passed down (`cmd/profgate/serve.go:106`).
What closes the remaining window is `serve` itself:
it starts `cluster.Run` at `:472` and returns without waiting for it,
so a process that builds a second runtime while the first cluster is still inside `factory.Shutdown` would race;
`serve` therefore waits for `cluster.Run` to return after `cancelInformers()` at `:427`,
and a returned `serve` means every goroutine it started that logs through klog has stopped.
The logging test builds each subtest's cluster through `NewRuntimeWithClientset`, which is what installs the sink,
and stops that subtest's informer before the next runtime is built.

**klog records arrive at the level client-go emits, and the test asserts the cases the spec names.**
`klog.SetSlogLogger` wraps the handler in `logr.FromSlogHandler` with contextual logging on
(`k8s.io/klog/v2@v2.140.0/contextual_slog.go:29-31`).
klog's `output` sends severity `Error` and `Fatal` to `logger.Error` and everything else to `logger.Info` at verbosity zero
(`klog.go`, the `logger != nil` branch of `loggingT.output`),
and logr's slog sink maps `Info` at verbosity `n` to `slog.Level(-n)` and `Error` to `slog.LevelError`
(`github.com/go-logr/logr`, `slogsink.go:69-79`).
So klog `Info` and `Warning` land at `INFO`;
`V(1)` through `V(4)` land at `-1` through `-4`, visible only under `server.logLevel: debug`;
`Error`, `Fatal`, and `utilruntime.HandleErrorWithContext` land at `ERROR`.
A list that fails reaches `DefaultWatchErrorHandler`'s default branch,
which calls `HandleErrorWithContext(ctx, err, "Failed to watch", ...)`
(`k8s.io/client-go@v0.36.4/tools/cache/reflector.go:215-229`, reached from `RunWithContext` at `:429-430`),
and that handler calls `logger.Error` (`k8s.io/apimachinery@v0.36.4/pkg/util/runtime/runtime.go:252`): `ERROR`.
A watch that ends with an error the reflector does not classify —
a `VeryShortWatchError`, a watch closed under a second with no event (`:1088-1090`) —
is logged `Warning: watch ended with error` at verbosity zero (`:664`) and retried: `INFO`.
A watch that closes cleanly logs `Watch close` at `V(4)` (`:1093`), which only `server.logLevel: debug` shows;
the spec names all three (`docs/specs/gateway.md:1536-1539`), and the test produces the first two.

**The upstream pool is bounded with the standard transport's values, and its age is a test seam.**
The spec keeps keep-alives and sets `MaxIdleConns: 100` and `IdleConnTimeout: 90s`,
leaving the per-host cap at Go's default of two (`docs/specs/gateway.md:997-1003`).
`proxy.Options` gains `IdleConnTimeout time.Duration`, zero meaning the spec's 90 seconds, which tests shorten,
because a test that reads the transport's fields proves the literal and not the behavior,
and a test that waits 90 seconds proves nothing in the time a unit test has.
The red run adds the field and the test before the transport reads either value,
so the idle connection the test watches is never closed.

**`Recorder.Confirm` takes `client_gone`, and no interface changes.**
The spec counts a client gone during the read under `result="client_gone"` (`docs/specs/gateway.md:698`, `:1572`).
`profgate_confirm_total` is a `CounterVec` on `result` with no fixed value set (`internal/metrics/prometheus.go:58-60`, `:177-182`),
so the fourth value is a call the handler makes and a sentence in the `Recorder` doc comment (`internal/metrics/recorder.go:67`).
The counter never takes a fifth: a drain cut during the read is counted `client_gone` and audited `drain_expired`,
because the counter says what ended the read and the audit says what ended the request.

**The drain's cause is read once, at the layer every route shares, and a cut request writes nothing.**
The spec names the mechanism: one drain context the process owns, every API request context derived from it,
cancelled with the `drain_expired` cause before the connections are closed,
and a classification on every route that reads the cause (`docs/specs/gateway.md:1711-1719`).
The sentinel lives in `internal/httpapi`, which owns the classification;
[`200-coding-standards.md`](../../.agents/rules/200-coding-standards.md) puts a sentinel in the package that produces it,
and the producer is `cmd/profgate`, which nothing can import
and which already imports `internal/httpapi` (`cmd/profgate/serve.go:23`).
`internal/proxy` is not touched by the cut:
it keeps reporting every cancellation as `client_gone`, which is true of what it saw,
and the amendment's package table places the cause and its reading in `internal/httpapi` (`docs/specs/gateway.md:2926`).
A request context derived from the cancelled one reports the cause through `context.Cause`,
because the first cancellation of a context or of any of its ancestors sets the cause.
Every `/v1` request passes through two places:
`ServeHTTP`'s deferred function, which emits the metrics row and the audit record (`internal/httpapi/server.go:354-361`),
and `request.fail`, which writes every gateway envelope (`:324-331`),
the listing routes included (`internal/httpapi/listing.go:86-137`).
The three `/auth/` routes pass through neither:
they are dispatched before the `/v1` algorithm (`:370-382`),
`serveAuthRoute` hands `AuthRoutes.ServeAuth` the unwrapped writer and takes the status and code it returns
(`internal/httpapi/auth.go:167-174`),
and the browser's token exchange runs on `r.Context()` (`internal/auth/browser.go:364-368`),
so a drain that cancels an exchange in flight has the route write `503 auth_unavailable` on its own.
That dispatch gets a third place: a writer the `/auth/` boundary installs,
which refuses every write once the request was cut, and an abort after `ServeAuth` returns.
A store call that returns the cancelled context's error is mapped to `pgo_unavailable` today
(`internal/httpapi/pgo.go:168-187`, `internal/httpapi/pgo_policy.go:86-90`, the dispatch at `internal/httpapi/pgo.go:131-156`)
and reaches `fail`,
so a handler inside a store call when the drain cancels would write an envelope in the interval before `Close`.
So `fail` refuses to write when the request was cut, and the deferred function relabels a cancellation the cut caused;
no route needs to know.
After the response is committed the spec says "connection closed; nothing more is written" (`docs/specs/gateway.md:1051`),
and the download's row says "connection closed; body truncated" (`docs/specs/pgo.md:2474`);
a handler that returns normally lets `net/http` write a chunked terminator, or an empty `200` when nothing was written,
so a cut request ends in `http.ErrAbortHandler`,
the abort `upstream_stream_failed` already takes (`internal/httpapi/server.go:760-764`),
and the client sees a truncated body or nothing rather than a well-formed end.
`cancelled` leaves the audit-only set in the same task:
`codeCollectionCancelled` (`internal/httpapi/pgo.go:35`) is written by no non-test code,
`docs/deployment.md:490` no longer lists it,
and the comment at `internal/httpapi/codes.go:101-104` and the slice at `codes_test.go:71-75` still do.

**Whether the window is spent follows readiness, not the call site.**
The five fatal startup exits at `cmd/profgate/serve.go:449`, `:461`, `:467`, `:484`, and `:493` all pass `drainEndpoints`,
and the spec's rule is "any exit before `/readyz` has first answered 200 skips `server.drainDelay`" (`docs/specs/gateway.md:1759`).
Four of the five cannot run after readiness:
issuer discovery holds `issuerReady` false (`:449`),
the two preflight exits run before the informers start and so before `HasSynced` (`:461`, `:467`),
and the NATS preflight holds `natsReady` false under `pgo.enabled` (`:484`);
`ready()` at `:172-174` is false throughout each.
The fifth can:
`natsReady.Store(true)` at `:488` runs before `startPGO` at `:490`,
so a probe in that window answers 200 and may put the replica behind the Service before `:493` exits.
A mode per call site would get that one wrong in the rare case,
so the decision is a flag `ready()` sets the first time it answers true,
read once in `shutdown` beside `mode == drainEndpoints` (`:380`);
the five callers do not change, `listenerFailed` keeps its meaning for a listener that fails after readiness,
and a stop request that arrives before readiness skips the window too, which the spec's "any exit" covers.

**Two changelog entries are marked breaking; six are not.**
`v0.5.0` is a published release, so every entry describes a change a running install sees,
and this changelog marks `BREAKING:` any change that alters what a shipped query, label, or code matches,
without weighing the break (`CHANGELOG.md:72-99` is the shape).
Four changes here do that, carried by two entries:
`client_gone` and `discovery_unavailable` stop matching what they matched,
and `profgate_confirm_total` gains a `result` value while its `unavailable` series counts fewer (task 6);
`drain_expired` is a `code` value that did not exist, and `cancelled` leaves the documented set (task 7).
Both go under `### Changed` with the prefix.
The timeouts, the pool bound, the klog and `ErrorLog` routing, and the skipped drain delay change what the gateway does
and not what any query matches; those six entries go under `### Fixed` without it.

---

## Global Constraints

- **No new configuration key, route, metric, chart value, or Kubernetes permission.**
  Every bound here is a constant the spec fixes, and `internal/k8s` widens no interface.
- **Every implementation task shows a red test before its change, and says what the red run observes.**
  Tasks 1 through 8 each name the test, the exact command, and the losing state the test forces:
  a client that stops reading, a socket that delivers one byte, a context cancelled with a cause,
  a keep-alive connection that goes quiet, an idle upstream connection, a store call held across the cut, a preflight that is refused.
  The rows whose assertion is an inventory rather than a behavior — the audit-only slice and the pinned-transport subtest —
  say so where they are added.
  Task 9 changes no behavior; the document-lifecycle checks of `mise run check` govern it.
- **Real sockets where the behavior is the socket.**
  [`300-testing.md`](../../.agents/rules/300-testing.md) tests HTTP behavior against `httptest` stand-ins;
  a write that blocks, a read that stalls, a connection that idles, and a cut that must leave nothing written are socket behavior,
  so those tests run an `httptest.Server` over the handler under test
  and drive it with a raw `net.Conn`, the shape `internal/httpapi/server_test.go:714-772` already uses.
  A `ResponseRecorder` carries no connection, so `http.ResponseController` answers it `ErrNotSupported`;
  every deadline call ignores that error, as the `Flush` at `internal/httpapi/pgo_collections.go:1035-1037` does,
  which is what keeps every recorder-based test in the tree green.
- **`-race` and `-count=1` on every red run.**
  The commands below carry both.
- **No jargon:** comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a task name, or a review round.
- Markdown prose uses semantic line breaks;
  run `semlf check` on every Markdown file and every Go file with doc comments a task writes or edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters — the hook refuses 50 or more —
  with a body that says what changed and why, one sentence per line, and no trailer of any kind
  ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
  A commit is finished when `git log --oneline -1` shows it and `git status --short` is clean,
  because the hook can refuse a message after `git commit` has already run ([500](../../.agents/rules/500-validation-and-workflow.md));
  every validation block below ends with both.
- Every task ends with the same validation block before its commit:

```bash
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
```

---

## File Structure

```text
internal/proxy/proxy.go                      # MaxIdleConns, IdleConnTimeout and its option; the write deadline at commit
internal/proxy/proxy_test.go                 # the idle upstream connection closed; the stalled client
internal/httpapi/server.go                   # RequestReadTimeout; budgetGrace and bodyReadTimeout; ErrDrainExpired; codeDrainExpired; the cut read in fail and the deferred record; the confirm cancel branch
internal/httpapi/pgo.go                      # decodeBody and rejectBody read under the deadline; codeCollectionCancelled deleted
internal/httpapi/pgo_collections.go          # rejectBody's receiver
internal/httpapi/auth.go                     # the cut-aware writer around ServeAuth and the abort after it
internal/httpapi/auth_test.go                # an exchange held across the cut
internal/httpapi/pgo_policy.go               # decodeBody's and rejectBody's receiver
internal/httpapi/codes.go                    # drain_expired in place of cancelled in the audit-only comment
internal/httpapi/codes_test.go               # drain_expired in place of cancelled in the auditOnly slice
internal/httpapi/export_test.go              # setBudgetGrace, setBodyReadTimeout
internal/httpapi/fixtures_test.go            # a Confirm that blocks until cancelled; a store Get that signals entry and blocks until cancelled
internal/httpapi/server_test.go              # the stalled client releases its slot; the confirm cancel; the drain cut at confirm, mid-stream, and its mirror
internal/httpapi/pgo_test.go                 # the dripping body; the empty probe
internal/httpapi/pgo_collections_test.go     # the download cut truncates; a store call held across the cut; the cancel rows without the deleted constant
internal/k8s/confirm.go                      # a cancellation returned as one
internal/k8s/confirm_test.go                 # the caller-cancelled row
internal/k8s/runtime.go                      # klog installed once, before the client and before the cluster
internal/k8s/cluster_test.go                 # new: a refused list at ERROR, a watch that ends at INFO
internal/metrics/recorder.go                 # client_gone among the results Confirm takes
cmd/profgate/serve.go                        # IdleTimeout, ErrorLog at error, RequestReadTimeout; the drain context and its cause; the wait for the informers; the readiness flag
cmd/profgate/serve_test.go                   # idleTimeout on gatewayOpts; the idle close; the handshake record; the cut cause; the held token exchange; the two skipped windows
go.mod                                       # k8s.io/klog/v2 direct
docs/deployment.md                           # the drain cut in the shutdown steps; the fourth confirm result
docs/plans/roadmap.md                        # the item's Shipped line, in the closing task
CHANGELOG.md                                 # one entry per behavior
docs/plans/interactive-deadlines.md          # this file
```

---

## 1. The upstream pool is bounded in size and age

Carries the roadmap bullet beginning *Neither server sets `ErrorLog`*, its last clause:
the transport sets no `IdleConnTimeout` and no global `MaxIdleConns`.

**Files:**
- Modify: `internal/proxy/proxy.go`, `internal/proxy/proxy_test.go`, `CHANGELOG.md`

**The decision, and why.**
The transport is built once at `internal/proxy/proxy.go:86-90` with `Proxy: nil`, `DisableCompression: true`, and a 5-second dialer,
and nothing else.
Its idle pool is therefore unbounded in count and in age:
one entry per Pod ever profiled, up to Go's per-host default of two,
kept until the peer closes or TCP keepalive notices the peer is gone.
The spec keeps keep-alives, for a client fetching several short profiles from one Pod,
and bounds the pool with the standard transport's `MaxIdleConns: 100` and `IdleConnTimeout: 90s` (`docs/specs/gateway.md:997-1003`).

`Options` (`:70-75`) gains, beside `HeaderDeadline`:

```go
	// IdleConnTimeout is how long an idle upstream connection is kept; zero means the spec's 90 seconds.
	// Tests inject short values.
	IdleConnTimeout time.Duration
```

The constants at `:19-29` gain `maxIdleConns = 100` and `defaultIdleConnTimeout = 90 * time.Second`;
the transport literal gains `MaxIdleConns: maxIdleConns` and `IdleConnTimeout: idle`,
where `idle` is the option or the default;
`MaxIdleConnsPerHost` is not set, so Go's `DefaultMaxIdleConnsPerHost` of two applies.
The doc comment of `New` (`:83-84`) names both.

- [x] **Write the test**

Two assertions, in `internal/proxy/proxy_test.go`.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestNew/transport is pinned` (`:649-660`) | gains `tr.MaxIdleConns == 100`, `tr.IdleConnTimeout == 90*time.Second`, `tr.MaxIdleConnsPerHost == 0`, and `!tr.DisableKeepAlives`; the first two are zero today. This is the inventory of the transport's settings, not a behavior |
| `TestDo/an idle upstream connection is closed after its timeout`, new | `newFixture(t, Options{IdleConnTimeout: 100 * time.Millisecond})`; the upstream is an `httptest.NewUnstartedServer` whose `Config.ConnState` records every state under a mutex, started and turned into a Target with `targetOf` (`:108-124`); one `f.do` with a body the upstream ends, so the connection returns to the pool idle (`resp.Body.Close()` at `internal/proxy/proxy.go:150`); within one second the upstream has seen `http.StateClosed` for it. Run with the option added and the transport not yet reading it: the transport's `IdleConnTimeout` is zero, the idle connection is never closed, and the wait times out |

The red state:

```bash
go test -race -count=1 ./internal/proxy/ -run 'TestNew/transport|TestDo/an_idle_upstream'
```

- [x] **Bound the pool**

`CHANGELOG.md`, `### Fixed`:
**The upstream transport's idle pool is bounded.**
An idle connection to a Pod was kept until the Pod closed it or TCP keepalive noticed the Pod was gone,
so the pool grew with every Pod ever profiled.
The transport now keeps at most 100 idle connections and closes one idle for 90 seconds,
the standard transport's values; two per Pod, as before.
A client fetching several short profiles from one Pod still reuses its connection.

- [x] **Validate and commit**

```bash
semlf check internal/proxy/proxy.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/proxy/proxy.go internal/proxy/proxy_test.go CHANGELOG.md
git commit -m "fix(proxy): bound the idle upstream pool" -m "<body: the pool had no size and no age, so it grew with every Pod ever profiled; the standard transport's two bounds, and why keep-alives stay>"
git log --oneline -1 && git status --short
```

---

## 2. A client that stops reading holds nothing past the budget

Closes the roadmap bullet beginning *A client that stops reading holds the handler, its admission slot, and the Pod connection past the request budget*.

**Files:**
- Modify: `internal/proxy/proxy.go`, `internal/proxy/proxy_test.go`,
  `internal/httpapi/server.go`, `internal/httpapi/export_test.go`, `internal/httpapi/server_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles where and when: at commit, in `Proxy.Do`, never cleared by the handler.
Immediately before `w.WriteHeader(resp.StatusCode)` at `internal/proxy/proxy.go:167`:

```go
	// From the first byte a client that stops reading can stall the copy below;
	// the budget's end is the bound, and net/http clears the deadline before the connection serves another request.
	// A writer with no connection behind it answers ErrNotSupported and is left unbounded.
	if end, ok := ctx.Deadline(); ok {
		_ = http.NewResponseController(w).SetWriteDeadline(end)
	}
```

`ctx` is the caller's overall budget (`:116-119`), so `end` is the budget's end;
a context without a deadline — the proxy tests' plain contexts — arms nothing.
The Collection sampler passes a `sampleSink` (`internal/pgo/rounds.go:462-467`), which has no connection,
so the call answers `ErrNotSupported` there and the sampler is unchanged.
After the deadline fires, `w.Write` returns a timeout, `io.Copy` at `:173` returns it,
and the classification at `:174-178` reads `ctx.Err()`:
nil or `DeadlineExceeded`, since the deadline is the budget's end, so the outcome is `upstream_stream_failed`,
the row the spec's table carries for "failure or budget expiry after the client response was committed" (`docs/specs/gateway.md:1049`).
The handler then panics `http.ErrAbortHandler` (`internal/httpapi/server.go:760-764`),
and `resp.Body.Close()` releases the upstream connection.
`ServeTLS` enables HTTP/2, whose response writer implements `SetWriteDeadline` as well
(`$(go env GOROOT)/src/net/http/h2_bundle.go`, `http2responseWriter.SetWriteDeadline`);
the unit tests below run HTTP/1.1, which is what `httptest.NewServer` serves.

The handler's budget grace becomes a field so a test can shorten it.
`server` (`internal/httpapi/server.go:88-93`) gains `budgetGrace time.Duration`,
`New` (`:97-115`) sets it from the constant at `:31`,
`serveProfile` reads `s.budgetGrace` at `:714`,
and `export_test.go` gains `setBudgetGrace(h http.Handler, d time.Duration)` beside `setBeforeAllowlist`.

- [x] **Write the test**

Two tests, one per package, both on real sockets.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestDo/a client that stops reading is cut at the budget`, `internal/proxy/proxy_test.go` | an `httptest.Server` whose handler calls `f.p.Do` under `context.WithTimeout(r.Context(), 500*time.Millisecond)` and sends the outcome and the elapsed time on a channel; the upstream (`f.upstream`, `:98-105`) writes 64 KiB chunks with a `Flush` after each until its request context ends; the client is a raw `net.Dial` that writes a `GET`, reads until the blank line that ends the headers, and then holds the socket open without reading. `Do` returns within 1.5 seconds with `upstream_stream_failed`, committed. Today `Do` does not return while the socket is held: the loopback buffers fill within milliseconds and `w.Write` blocks with no deadline, so the test's wait of 3 seconds expires and the blocked write ends only at cleanup when the client's socket closes |
| `TestProfileProxy/a client that stops reading releases its slot`, `internal/httpapi/server_test.go`, beside the committed-stream-failure test at `:714-772` | `h.gate = admit.New(1)` (`internal/httpapi/fixtures_test.go:456`), `h.upstream = proxy.New(proxy.Options{})`, a `newTrap` upstream (`:407-435`) streaming as above, `setBudgetGrace(handler, 500*time.Millisecond)`, `httptest.NewServer(handler)`; the same raw client. Within 2 seconds: `h.gate.TryAcquire()` succeeds (`internal/admit/gate.go:22-29`), the audit record is `upstream_stream_failed` (the `h.audits` poll at `:753-767`), and `h.rec.snapshot()`'s in-flight count is 0 (`:769-771`). Today the slot never comes back while the socket is held, because `release()` is deferred at `internal/httpapi/server.go:709` and the handler is blocked in the write |

The red state:

```bash
go test -race -count=1 ./internal/proxy/ -run 'TestDo/a_client_that_stops_reading'
go test -race -count=1 ./internal/httpapi/ -run 'TestProfileProxy/a_client_that_stops_reading'
```

Both time out on their own waits and report the handler still blocked.

- [x] **Arm the deadline at commit and say so**

`CHANGELOG.md`, `### Fixed`:
**A client that stops reading holds nothing past the request budget.**
The budget bounded confirmation, the dial, the header wait, and the upstream read,
and not the write to the client:
a client that read the headers and then stopped held the handler, its admission slot, and the Pod connection for as long as it kept the socket open,
and sixteen such clients answered every later profile request `429 too_many_profiles`.
The write to the client now fails at the budget's end,
the request is audited `upstream_stream_failed` as any expiry after the response is committed is,
and the slot and the Pod connection are released then.

- [x] **Validate and commit**

```bash
semlf check internal/proxy/proxy.go internal/httpapi/server.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/proxy/proxy.go internal/proxy/proxy_test.go \
  internal/httpapi/server.go internal/httpapi/export_test.go internal/httpapi/server_test.go CHANGELOG.md
git commit -m "fix(proxy): bound writes by the request budget" -m "<body: the write to the client was the one wait inside the budget the budget could not end; the deadline is armed at commit, where a write can first block, and net/http clears it after the handler>"
git log --oneline -1 && git status --short
```

---

## 3. A body that drips holds a handler ten seconds

Closes the roadmap bullet beginning *A PGO route reads its JSON body with no deadline once the headers are in*.

**Files:**
- Modify: `internal/httpapi/server.go`, `internal/httpapi/pgo.go`, `internal/httpapi/pgo_collections.go`,
  `internal/httpapi/pgo_policy.go`, `internal/httpapi/export_test.go`, `internal/httpapi/pgo_test.go`,
  `cmd/profgate/serve.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the constant, the seam, the clear on success only, and the message.

`internal/httpapi/server.go` exports, beside `budgetGrace`:

```go
	// RequestReadTimeout is how long a client gets to deliver its request headers,
	// and, on a route that reads or probes a body, how long it gets to deliver the body.
	// Both listeners read it for ReadHeaderTimeout; a body is at most 64 KiB, so one bound serves both.
	RequestReadTimeout = 10 * time.Second
```

`server` gains `bodyReadTimeout time.Duration`, set from it in `New`,
and `export_test.go` gains `setBodyReadTimeout`.
`decodeBody` (`internal/httpapi/pgo.go:206-234`) and `rejectBody` (`:244-253`) become methods on `*server`
so they read the field;
their four callers gain the receiver:
`internal/httpapi/pgo_collections.go:471` and `:1099`, `internal/httpapi/pgo_policy.go:120` and `:197`.
Each arms the deadline once, before its read, and clears it only after a read that reached the body's end:

```go
	// A body that arrives one byte at a time held this read for as long as the client chose.
	// A read that fails leaves the deadline armed:
	// net/http reads the rest of an unread body after the handler returns, up to 256 KiB,
	// and the same deadline ends that read at once instead of letting the same client stall it.
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(s.bodyReadTimeout))
```

with `_ = control.SetReadDeadline(time.Time{})` on the success path of each, after the read returned without error.
In `decodeBody`'s error branch (`:208-215`), before the `MaxBytesError` check:

```go
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return bodyMalformed(fmt.Sprintf("the request body did not arrive within %s", s.bodyReadTimeout))
		}
```

`rejectBody` reads `n, err := io.ReadFull(...)` (`:246`) and drops the error today;
it keeps the `n > 0` refusal, treats `io.EOF` as no body, and answers any other error the way `decodeBody` does,
the deadline with the fixed message above,
which is the spec's "answered the way an unreadable body is" (`docs/specs/gateway.md:251`).
The deadline's read error cancels the request context
(`$(go env GOROOT)/src/net/http/server.go`, `connReader.handleReadErrorLocked`, `:769-777` in 1.26.7);
both callers of `decodeBody` return on its error before any store call
(`internal/httpapi/pgo_collections.go:471-475`, `internal/httpapi/pgo_policy.go:120-124`),
and the `400` envelope is written to a connection that is still open.

`cmd/profgate/serve.go:234-235` reads `httpapi.RequestReadTimeout` for both `ReadHeaderTimeout` values,
and the constant at `:43-44` is deleted.

- [x] **Write the test**

Two subtests in `internal/httpapi/pgo_test.go`, on a real socket over the PGO harness
(`newPGOHarness`, `internal/httpapi/fixtures_test.go:1604`),
sending the headers `doPGO` sends (`:1814-1830`) by hand over a raw `net.Conn`.

| Test | What it asserts, and how it fails today |
|---|---|
| `a body that drips is refused at the deadline` | `setBodyReadTimeout(handler, 300*time.Millisecond)`; the client sends `POST` to the Service's collections path with `Content-Type: application/json`, `Content-Length: 2`, then the one byte `{` and nothing more; it reads the response under a 2-second read deadline of its own. Within 1 second: `400`, code `invalid_parameter`, a `body_malformed` detail whose message names the bound and no address. Today the handler is blocked in `io.ReadAll` at `internal/httpapi/pgo.go:207` and the client's read times out |
| `a claimed body that never arrives is refused at the deadline` | the harness seeds a running Collection first, `rec := h.seedRecord(t, h.newRecord(pgo.StateRunning))` (`internal/httpapi/fixtures_test.go:1699`, `:1725`), because the dispatch at `internal/httpapi/pgo.go:131-136` answers `404 collection_not_found` for an unknown identifier before `rejectBody` runs; the same client sends `POST` to that Collection's cancel path (`internal/httpapi/pgo_collections.go:1099`) with `Content-Length: 1` and no byte. Within 1 second: `400 invalid_parameter` with the `body_malformed` detail. Today the handler is blocked in `io.ReadFull` at `internal/httpapi/pgo.go:246` |

The red state:

```bash
go test -race -count=1 ./internal/httpapi/ -run 'TestDecodeBody/a_body_that_drips|TestDecodeBody/a_claimed_body'
```

Both report the client's read timing out with no response.
The existing `decodeBody` rows on a `ResponseRecorder` stay green:
the recorder answers `ErrNotSupported`, which is ignored.

- [x] **Bound the reads and say so**

`CHANGELOG.md`, `### Fixed`:
**A request body that arrives slowly is refused after ten seconds.**
A PGO route read its JSON body with no bound once the headers were in,
so a body sent one byte at a time held a handler goroutine for as long as the client chose,
and a route that takes no body waited the same way for the one byte that would prove one was sent.
Both reads now have the ten seconds the headers have,
and a body that has not arrived by then is answered `400 invalid_parameter` with a `body_malformed` detail that names the bound.
The bound is a constant, as the header bound is; there is no key for it.

- [x] **Validate and commit**

```bash
semlf check internal/httpapi/server.go internal/httpapi/pgo.go cmd/profgate/serve.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/httpapi/server.go internal/httpapi/pgo.go internal/httpapi/pgo_collections.go \
  internal/httpapi/pgo_policy.go internal/httpapi/export_test.go internal/httpapi/pgo_test.go \
  cmd/profgate/serve.go CHANGELOG.md
git commit -m "fix(httpapi): bound the body read to ten seconds" -m "<body: the body read and the probe read had no bound once the headers were in; the deadline is the listener's header bound, exported once, and is kept after a failed read so the post-handler discard cannot stall on the same client>"
git log --oneline -1 && git status --short
```

**A second commit: the unread body of a refused request.**
Implementing the two reads showed a third wait with the same shape.
A route that refuses a request before reading its body —
`mediaTypeFault` on a write route with no JSON media type (`internal/httpapi/pgo_collections.go:118`),
or any refusal `fail` writes before the read —
returns at once, and `net/http` then discards the unread body before it flushes the response
(`$(go env GOROOT)/src/net/http/server.go`, the `io.CopyN(io.Discard, w.reqBody, ...)` in `chunkWriter.writeHeader`, line 1408 in 1.26.7);
against a body that never arrives that discard blocks with no deadline, and the refusal never reaches the client.
The decision: `ServeHTTP` arms the same read deadline, `RequestReadTimeout` from the same field,
for every request whose `r.ContentLength != 0`, which is a declared body or a chunked one reported as `-1`,
before the `/auth/` and `/v1` dispatch and so before any route can refuse;
the body-reading routes keep arming and clearing as above, so a full read still clears it.
The ops listener is unchanged.
The spec's *Network* paragraph and the amendment's `internal/httpapi` row carry the entry-point deadline.

- [x] **Write the test**

A third subtest in `TestPGOBodiesAreBounded`, on the same raw client:
`POST` to the collections path with `Content-Type: text/plain`, `Content-Length: 5`, and no byte,
which `mediaTypeFault` refuses without a read.
With `setBodyReadTimeout(handler, 300*time.Millisecond)` the client must receive the `400 invalid_parameter` within 1 second.
Red: the client's read times out, because the refusal sits behind the drain.

```bash
go test -race -count=1 ./internal/httpapi/ -run 'TestPGOBodiesAreBounded/a_refused_request'
```

- [x] **Arm the deadline at entry and say so**

The `CHANGELOG.md` entry of this task gains a sentence:
a refused request whose body never arrives no longer holds the connection either.

- [x] **Validate and commit**

```bash
semlf check internal/httpapi/server.go docs/specs/gateway.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/httpapi/server.go internal/httpapi/pgo_test.go docs/specs/gateway.md CHANGELOG.md docs/plans/interactive-deadlines.md
git commit -m "fix(httpapi): bound the drain of an unread body" -m "<body: a refusal written before the body was read waited in net/http's discard of that body with no deadline; the handler now arms the read deadline at entry for every request with a body>"
git log --oneline -1 && git status --short
```

---

## 4. An idle connection closes, and net/http's own lines reach stdout

Closes the roadmap bullet beginning *Neither server sets `ErrorLog`*, its first two clauses.

**Files:**
- Modify: `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`, `CHANGELOG.md`

**The decision, and why.**
Both servers are built at `cmd/profgate/serve.go:234-235` with a handler and `ReadHeaderTimeout` and nothing else.
With `IdleTimeout` zero, `conn.serve` parks in `Peek(4)` with the read deadline cleared between two requests on one connection
(`$(go env GOROOT)/src/net/http/server.go:2091-2099` in 1.26.7),
so a keep-alive connection that sends nothing is held until the process exits;
a fresh connection that sends nothing is already bounded, because `readRequest` arms `ReadHeaderTimeout` for a first request.
With `ErrorLog` nil, `Server.logf` falls back to `log.Printf`,
which writes to the process's stderr as text, outside `server.logLevel`;
a TLS handshake failure and a recovered handler panic with its stack both go there.

The constants at `:34-52` gain `idleTimeout = 120 * time.Second`;
`serveDeps` (`:81-94`) gains `idleTimeout time.Duration` documented as `tlsRefresh` is:
production 0, so both listeners use the constant.
Both literals gain `IdleTimeout` and `ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)`,
so every line `net/http` writes on its own is an `ERROR` record on stdout under the process's handler,
the level the spec names (`docs/specs/gateway.md:253-255`).
`gatewayOpts` (`cmd/profgate/serve_test.go:445-463`) gains `idleTimeout`, copied into `deps` where `tlsRefresh` is (`:503`).

- [x] **Write the test**

Two subtests of `TestServe`.

| Test | What it asserts, and how it fails today |
|---|---|
| `an idle keep-alive connection is closed` | `startGatewayWith(..., gatewayOpts{idleTimeout: 200 * time.Millisecond})`; a raw `net.Dial` to `gw.opsAddr` writes `GET /healthz HTTP/1.1` with a `Host` header, reads the response with `http.ReadResponse` and drains its body, then sets a 2-second read deadline and reads again: the read returns `io.EOF`. The same over `gw.apiAddr` with `/v1/openapi.json`, whose `503 not_ready` before sync is a complete response. Today both reads return a timeout: the server parks in `Peek(4)` with no deadline |
| `a handshake failure is an error record on stdout` | `gatewayOpts{tlsDir: dir}` as the rotation subtest does (`:772-776`); a raw `net.Dial` to `gw.apiAddr` writes `GET / HTTP/1.1` in plaintext; `waitFor` a record in `gw.records(t)` whose `level` is `ERROR` and whose `msg` contains `TLS handshake error`. Today no such record ever appears: the line went to `os.Stderr` through `log.Printf`, which the harness cannot see, and the wait times out |

The ops server's `ErrorLog` is the same expression in the neighboring literal;
nothing a test can send makes the ops server write a line of its own, and review holds that one.

The red state:

```bash
go test -race -count=1 ./cmd/profgate/ -run 'TestServe/an_idle_keep-alive|TestServe/a_handshake_failure'
```

- [x] **Set the two fields and say so**

`CHANGELOG.md`, `### Fixed`:
**An idle connection is closed after two minutes, and `net/http`'s own lines are JSON on stdout.**
A keep-alive connection that sent nothing after its last request was held until the process exited;
both listeners now close it after 120 seconds.
A TLS handshake failure and a recovered handler panic were printed to stderr as text, outside `server.logLevel`;
they are now `ERROR` records on stdout like everything else the gateway says.

- [x] **Validate and commit**

```bash
semlf check cmd/profgate/serve.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add cmd/profgate/serve.go cmd/profgate/serve_test.go CHANGELOG.md
git commit -m "fix(serve): close idle connections, log via slog" -m "<body: an idle keep-alive connection was held until exit and net/http's own lines went to stderr as text; what IdleTimeout and ErrorLog now do on both listeners>"
git log --oneline -1 && git status --short
```

---

## 5. klog writes through slog

Closes the roadmap bullet beginning *client-go reflector failures go to stderr as text*.

**Files:**
- Modify: `internal/k8s/runtime.go`, `cmd/profgate/serve.go`, `go.mod`, `CHANGELOG.md`
- Add: `internal/k8s/cluster_test.go`

**The decision, and why.**
The mapping, the three levels the spec names, the single install, and the memo klog holds are settled in *Decisions*.
The install is one helper in `internal/k8s/runtime.go`, beside `NewRuntime`:

```go
// installKlog routes client-go's klog output through log,
// so reflector and watch failures reach stdout as JSON at the level client-go emits.
// klog's setter mutates unsynchronised package state and must run while no informer logs,
// so it runs where the runtime is built, before any client or informer exists,
// and writes nothing when log's handler is already the one klog holds.
func installKlog(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if logr.ToSlogHandler(klog.Background()) == log.Handler() {
		return
	}
	klog.SetSlogLogger(log)
}
```

`NewRuntime` (`internal/k8s/runtime.go:30-37`) calls it with `opts.Logger` before `NewClientset()`,
so the lines `rest.InClusterConfig` and `clientcmd` write while the client is built go the same way;
`NewRuntimeWithClientset` (`:41-43`) calls it before `New`,
so the runtime the `cmd/profgate` tests build routes klog the same way.
`Cluster.New` (`internal/k8s/cluster.go:39-55`) does not call it:
every test that builds a cluster calls `New`, some while another cluster's informers are still logging.
`github.com/go-logr/logr` is already in the module graph as a dependency of klog; the import makes it direct, as it does klog.
`k8s.io/klog/v2` is at `v2.140.0` (`go.mod:82`, indirect);
`go mod tidy` marks both direct, and `mise run check` holds the `go` directive where it is.
`internal/k8s` stays the sole importer of client-go;
klog and logr are not client-go, and the seam check in `scripts/check-repo.py` is unchanged.

`serve` waits for the informers.
`go cluster.Run(informerCtx)` at `cmd/profgate/serve.go:472` gets a channel closed when `Run` returns,
and `shutdown` waits on it right after `cancelInformers()` at `:427`, before the ops listener drains;
`Cluster.Run` returns once `factory.Shutdown` has joined every informer goroutine (`internal/k8s/cluster.go:72-73`),
so a returned `serve` means nothing it started still logs through klog,
and a test that starts another gateway afterwards installs a new sink with no goroutine reading the old one.

- [x] **Write the test**

`TestClusterLogsThroughSlog`, in a new `internal/k8s/cluster_test.go`, with two subtests.
`startFixture` (`internal/k8s/export_test.go:27-47`) builds the fake clientset itself and blocks in `waitCache` until `HasSynced`,
so neither subtest can use it: each builds `fake.NewClientset(baseline().objects()...)`,
installs its reactor, sets `Options.Logger` to a `slog.NewJSONHandler` over a mutex-guarded buffer,
takes `c := NewRuntimeWithClientset(cs, opts).Cluster()` (`internal/k8s/runtime.go:29-42`) and runs `go c.Run(ctx)`,
and cancels and waits for `Run` to return before it ends,
so the next subtest builds its runtime with no informer goroutine running.
The test names nothing this task adds, so it compiles on the tree as it is,
and the constructor is what installs the sink, so a green run proves the wiring and not only the helper.

| Test | What it asserts, and how it fails today |
|---|---|
| `a refused list is an error record` | a reactor refuses every Pod list, the shape `cmd/profgate/serve_test.go:882-888` uses; within 5 seconds the buffer holds a record with `level` `ERROR`, `msg` `Failed to watch`, and an `err` containing the reactor's text, and `HasSynced()` is still false, polled rather than waited for, because `waitCache` (`internal/k8s/export_test.go:50-60`) fails the test on a cache that never syncs. Today the buffer receives nothing — klog writes the line to stderr — and the wait times out |
| `a watch that ends is an info record` | a `PrependWatchReactor` for Pods returns a `watch.NewFake()` it has already stopped, so the reflector's watch closes at once with no event and returns a `VeryShortWatchError` (`reflector.go:1088-1090`); `waitCache(t, c.HasSynced)` passes, because the list succeeded; within 5 seconds the buffer holds a record with `level` `INFO` and `msg` `Warning: watch ended with error` (`:664`). Today the buffer receives nothing |

The red state:

```bash
go test -race -count=1 ./internal/k8s/ -run 'TestClusterLogsThroughSlog'
```

klog's sink is package state; the package's tests do not run in parallel, and this test says so in a comment.

- [x] **Install the sink and say so**

`CHANGELOG.md`, `### Fixed`:
**Kubernetes client failures are JSON on stdout.**
client-go's informers log through klog, whose default sink is stderr text outside `server.logLevel`,
so a watch that kept failing after the first sync was invisible in the gateway's own log.
klog now writes through the gateway's logger at the level client-go emits:
a list that fails is an `ERROR` record, a watch that ends with an error and is retried is `INFO`,
and client-go's verbose lines, a watch that closes cleanly among them, appear under `server.logLevel: debug`.

- [x] **Validate and commit**

```bash
go mod tidy && git diff --stat go.mod go.sum
semlf check internal/k8s/runtime.go cmd/profgate/serve.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/k8s/runtime.go internal/k8s/cluster_test.go cmd/profgate/serve.go go.mod go.sum CHANGELOG.md
git commit -m "fix(k8s): route klog through slog" -m "<body: reflector failures went to stderr as text outside server.logLevel; where the sink is installed, why once, and the level each klog severity lands at>"
git log --oneline -1 && git status --short
```

---

## 6. A client gone during confirmation is `client_gone`

Closes the first half of the roadmap bullet beginning *Two outcomes are misattributed*.

**Files:**
- Modify: `internal/k8s/confirm.go`, `internal/k8s/confirm_test.go`,
  `internal/httpapi/server.go`, `internal/httpapi/fixtures_test.go`, `internal/httpapi/server_test.go`,
  `internal/metrics/recorder.go`, `docs/deployment.md`, `CHANGELOG.md`

**The decision, and why.**
`Confirm` runs its `Get` under `deadlineGuard` (`internal/k8s/confirm.go:33-38`),
which returns `callCtx.Err()` when the context ends first (`internal/k8s/client.go:84-93`):
`context.Canceled` exactly when the caller's context was cancelled,
`context.DeadlineExceeded` when either deadline — the confirmation's own or the budget's — passed.
Every non-404 error is then wrapped as `ErrDiscoveryUnavailable` at `:44`,
with `%s` on the cause, so not even `errors.Is` can tell a cancellation apart afterwards.
The handler records `unavailable` and writes `503 discovery_unavailable` (`internal/httpapi/server.go:728-736`)
to a client that is not there.

The error branch at `:39-45` gains one case between the 404 and the wrap:

```go
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			// The caller left before the API server answered; nothing about the target is known, and nothing is claimed.
			return fmt.Errorf("confirm pod %s/%s: %w", t.Namespace, t.Pod, context.Canceled)
		}
```

`errors.Is(err, context.Canceled)` catches a real client-go call that returned its context's error;
`ctx.Err()` catches the guard's own answer.
A `DeadlineExceeded` from either deadline falls through to `ErrDiscoveryUnavailable`, as today.
The doc comment at `:19-22` names the third outcome.
The Collection sampler's caller (`internal/pgo/rounds.go:451-460`) is unchanged:
a cancellation lands in its default branch, `ReasonDiscoveryUnavailable`, exactly where the wrapped error landed before.

`serveProfile` gains a branch after the `ErrTargetChanged` one (`internal/httpapi/server.go:718-727`):

```go
		if errors.Is(err, context.Canceled) {
			// The client left during the read: nobody to answer, and an attempt the counter still sees.
			s.deps.Recorder.Confirm(codeClientGone)
			q.audit.code = codeClientGone

			return
		}
```

`codeClientGone` is the constant at `internal/httpapi/pgo.go:34`.
The status stays 0 and nothing is written, the shape the proxy's `client_gone` already takes (`:752-758`).
This branch is what the drain cut of the next task relabels, and only the audit code:
the counter's `client_gone` stays, so `profgate_confirm_total` keeps its four values.
`Recorder.Confirm`'s doc comment (`internal/metrics/recorder.go:67`) names the fourth result;
the counter is a `CounterVec` on `result` (`internal/metrics/prometheus.go:58-60`, `:177-182`) and needs no code change.
`docs/deployment.md:457`, the counter's row, names the four results.

- [x] **Write the test**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestConfirm/caller cancelled`, `internal/k8s/confirm_test.go`, beside `api timeout` (`:257-280`) | the same blocking `get` reactor; the context is `context.WithCancel`, cancelled from the test once the reactor has been entered; `Confirm` returns within a second with an error for which `errors.Is(err, context.Canceled)` holds and `errors.Is(err, ErrDiscoveryUnavailable)` does not. Today it returns `ErrDiscoveryUnavailable` with the cause flattened into text. `api timeout` stays green: `DeadlineExceeded` is still `unavailable` |
| `TestProfileProxy/a client gone during confirmation is client_gone`, `internal/httpapi/server_test.go` | `fakeDiscovery` (`internal/httpapi/fixtures_test.go:151-159`) gains a `confirmBlocks bool` under which `Confirm` waits on `ctx.Done()` and returns `ctx.Err()`; the request is built with `httptest.NewRequestWithContext` under a cancellable context, run on a goroutine, and cancelled once `confirmCalls` reads 1; then: the body is empty, the audit record has status 0 and code `client_gone` (`h.expectAudit`, `:711`), and the second value of `h.rec.snapshot()` (`:371`), the confirm results, is exactly `["client_gone"]`. Today the response is `503 discovery_unavailable` and the results are `["unavailable"]` |

The red state:

```bash
go test -race -count=1 ./internal/k8s/ -run 'TestConfirm/caller_cancelled'
go test -race -count=1 ./internal/httpapi/ -run 'TestProfileProxy/a_client_gone_during_confirmation'
```

- [x] **Return the cancellation and say so**

`CHANGELOG.md`, `### Changed`:
**BREAKING: a client that leaves during the confirmation read is `client_gone`, in the audit record and in `profgate_confirm_total`.**
It was answered `503 discovery_unavailable` — to nobody — and counted under `result="unavailable"`,
so a burst of impatient clients read as an API server that could not vouch for anything.
The request now ends with nothing written and `client_gone` in the audit record,
and the counter has a fourth result, `client_gone`, for exactly those attempts.
A query over `profgate_confirm_total{result="unavailable"}` counts fewer,
and a rule that summed the three documented results now misses a fourth unless it is added.

- [x] **Validate and commit**

```bash
semlf check internal/k8s/confirm.go internal/httpapi/server.go internal/metrics/recorder.go docs/deployment.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/k8s/confirm.go internal/k8s/confirm_test.go \
  internal/httpapi/server.go internal/httpapi/fixtures_test.go internal/httpapi/server_test.go \
  internal/metrics/recorder.go docs/deployment.md CHANGELOG.md
git commit -m "fix(httpapi): confirm cancel is client_gone" -m "<body: a cancelled confirmation read was wrapped as an unavailable API server, answered to a client that had left, and counted as unavailable; what Confirm returns now, and the fourth result the counter takes>"
git log --oneline -1 && git status --short
```

---

## 7. A request the drain cuts is `drain_expired`

Closes the second half of the roadmap bullet beginning *Two outcomes are misattributed*,
and takes `cancelled` out of the audit-only set, where nothing wrote it.

**Files:**
- Modify: `internal/httpapi/server.go`, `internal/httpapi/auth.go`, `internal/httpapi/pgo.go`, `internal/httpapi/codes.go`,
  `internal/httpapi/codes_test.go`, `internal/httpapi/fixtures_test.go`, `internal/httpapi/server_test.go`,
  `internal/httpapi/auth_test.go`, `internal/httpapi/pgo_collections_test.go`,
  `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`,
  `docs/deployment.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles where the sentinel lives, that the cause is read at one layer, what a cut request writes, and why `cancelled` goes.
`internal/proxy` is not in this task's file list: it keeps classifying every cancellation `client_gone`.

`internal/httpapi/server.go` gains, beside the constants at `:28-44`:

```go
	// codeDrainExpired is the outcome of a request the drain bound cut.
	codeDrainExpired = "drain_expired"
```

and, exported, the cause `cmd/profgate` sets:

```go
// ErrDrainExpired is the cancellation cause the gateway sets on every request still in flight
// when its drain bound ends, just before it closes their connections.
// Every request context derives from the cancelled one and reports it through context.Cause,
// which is how a cut is told from a client that left on its own.
var ErrDrainExpired = errors.New("the drain bound ended with the request in flight")
```

`request` (`:242-251`) gains `ctx context.Context`, set from `r.Context()` where the request is built (`:352`),
and one method:

```go
// cut reports whether the drain bound ended while this request was in flight.
func (q *request) cut() bool { return errors.Is(context.Cause(q.ctx), ErrDrainExpired) }
```

`fail` (`:324-331`) reads it first:
when the request was cut, the status is 0, the code is `codeDrainExpired`, no envelope is written,
and the handler ends in `panic(http.ErrAbortHandler)`,
so `net/http` writes neither the envelope nor the empty `200` a handler that returns silently gets.
The deferred function in `ServeHTTP` (`:354-361`) reads it once more, before the metrics row and the audit record:
when the request was cut and its code is `codeClientGone` —
the code every route already sets when its context is cancelled mid-work,
at `serveProfile`'s confirmation branch, at the proxy outcome `:750-751`, at the wait `internal/httpapi/pgo_collections.go:898-902`,
and at the download `:1040-1046` — the code becomes `codeDrainExpired`,
the row and the record are written, and the function panics `http.ErrAbortHandler` after them,
so a committed stream is truncated and an uncommitted one ends with nothing written.
`ErrAbortHandler` is what `conn.serve` recovers silently, from a deferred call as from the handler body.
A request whose code is anything else is not touched by the cut:
it produced its response, and the terminator `net/http` writes after it is the end of a complete body.
The envelope after `Do` at `:755` is guarded by `!q.cut()` as well, for the timeout that lands in the same instant as the cut.
The `/auth/` boundary is the third place, in `internal/httpapi/auth.go`:

```go
// errRequestCut is what a write after the drain cut returns:
// the route's own error handling sees a failed write, and the client sees nothing.
var errRequestCut = errors.New("the drain bound cut this request")

// cutWriter is the writer the /auth/ routes answer through.
// Once the drain bound has cut the request it refuses every write,
// so a route that answers after the cut writes nothing the client could mistake for an answer.
type cutWriter struct {
	http.ResponseWriter
	q         *request
	committed bool // a status was written before any cut
}

func (c *cutWriter) WriteHeader(status int) {
	if c.q.cut() {
		return
	}
	c.committed = true
	c.ResponseWriter.WriteHeader(status)
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if c.q.cut() {
		return 0, errRequestCut
	}
	c.committed = true

	return c.ResponseWriter.Write(p)
}

// Unwrap keeps http.ResponseController reaching the connection.
func (c *cutWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
```

`serveAuthRoute` (`:143-175`) passes `cw := &cutWriter{ResponseWriter: w, q: q}` to `ServeAuth` at `:167`,
and after it returns, before the outcome is copied at `:168-171`:
when the request was cut and `cw.committed` is false, the status is 0, the code is `codeDrainExpired`, the reason is empty,
no `AuthFailure` is recorded, and the handler panics `http.ErrAbortHandler`;
a route that had committed its answer before the cut keeps the outcome it returned, as a `/v1` request does.
No route reads the cause itself.
`serveProfile`'s abort at `:760-764` stays for `upstream_stream_failed`,
and `streamArtifact`'s at `:1047-1051` for `artifact_stream_failed`;
the drain's aborts are the three above: `fail`, the deferred record, and the `/auth/` boundary.
`Recorder.Confirm(codeClientGone)` on the confirmation branch is untouched by the relabel,
so the counter keeps the four results the spec lists.

`codeCollectionCancelled` (`internal/httpapi/pgo.go:35`) is deleted;
its two test uses at `internal/httpapi/pgo_collections_test.go:1731` and `:1734` compare a `string` Collection result (`internal/httpapi/fixtures_test.go:315-326`) and an `any` holding the transition's state,
both of which are the `pgo.State` `pgo.StateCancelled` (`internal/pgo/record.go:38`) spelled out,
and read `string(pgo.StateCancelled)` instead.
`internal/httpapi/codes.go:101-104` names `drain_expired` where it named `cancelled`,
and `codes_test.go:71-75` does the same in `auditOnly`;
that row is the inventory the registry is checked against, and it is green the moment the constant exists.

`cmd/profgate/serve.go`: before the two server literals at `:234`,

```go
	// Every API request context descends from this one.
	// The drain cancels it with httpapi.ErrDrainExpired just before it closes the connections its bound cut,
	// so a handler can tell that cut from a client that left on its own.
	drainCtx, cutInFlight := context.WithCancelCause(ctx)
	defer cutInFlight(nil)
```

`apiServer` gains `BaseContext: func(net.Listener) context.Context { return drainCtx }`,
and the drain goroutine calls `cutInFlight(httpapi.ErrDrainExpired)` on the line before `_ = apiServer.Close()` (`:404`);
the `drainCtx` name already used inside that goroutine (`:395`) is renamed to `shutdownCtx` to make room.
`Close` still cancels every connection's context through the background read error,
but by then the cause is set, and a second cancellation changes nothing.

`docs/deployment.md`, the shutdown steps (`:421-434`), step 3 gains one sentence:
a request still in flight when the API drain's bound ends is cut, nothing more is written to it,
and its audit record carries `drain_expired`, not `client_gone`.

- [ ] **Write the test**

Every row below that involves a socket builds `httptest.NewUnstartedServer(handler)`,
sets `srv.Config.BaseContext` to return a `context.WithCancelCause` context, and calls `srv.Start()`;
"the cut" is cancelling that context with `ErrDrainExpired` while the request is in flight, with the socket left open,
which is the interval before `Close` the mechanism exists for.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestProfileProxy/the drain cut mid-stream is drain_expired, a closed socket is client_gone`, `internal/httpapi/server_test.go` | `proxy.New` over a streaming `newTrap`; the raw client reads the headers and keeps draining the body on a goroutine, so nothing but the cut ends the stream. After the cut: the audit record, polled as at `:753-767`, is `drain_expired`, the metrics row is `drain_expired`, and the client's read ends in `io.ErrUnexpectedEOF`, the truncation the abort produces. The mirror closes the client's socket instead: `client_gone`. Today the first says `client_gone`, and the client's read ends cleanly |
| `TestProfileProxy/the drain cut during confirmation audits drain_expired and counts client_gone` | the blocking `fakeDiscovery.Confirm` of the previous task, the request over the socket; after the cut: the audit record is status 0 `drain_expired`, the confirm results in `h.rec.snapshot()` are exactly `["client_gone"]`, and the client reads zero bytes before `io.EOF`. Today the audit says `client_gone`, and the client reads an empty `200` |
| `TestAStoreCallHeldAcrossTheDrainCutWritesNothing`, `internal/httpapi/pgo_collections_test.go` | `fakeKV.Get` (`internal/httpapi/fixtures_test.go:932-953`) ignores its context today and signals nothing when it is entered; the fake gains a `blockUntilCancelled bool` under which `Get` closes a per-fixture `getStarted` channel and then waits on `ctx.Done()` and returns `ctx.Err()`; the request is `GET` of a seeded Collection over the socket, which reaches `readCollection` (`internal/httpapi/pgo.go:131-136`); the test receives from `getStarted` before it cancels the drain context, so the call is held across the cut and not merely preceded by it; after the cut: the client reads zero bytes before `io.EOF` and the audit record is `drain_expired`. Today the store's error is mapped to `503 pgo_unavailable` (`:177-178`) and the client reads that envelope |
| `TestADownloadCutByTheDrainIsTruncated` | a seeded completed Collection with an object the fake store serves in pieces, held mid-copy the same way, over the socket; the client reads the headers and drains; after the cut: `io.ErrUnexpectedEOF` on the client and `drain_expired` in the audit record, the row the download's outcome table carries (`docs/specs/pgo.md:2474`). Today `client_gone` and a body that ends cleanly |
| `TestAuthRoutes/an exchange held across the drain cut writes nothing`, `internal/httpapi/auth_test.go` | `fakeRoutes` (`:64-80`) gains a `blockUntilCancelled bool` under which `ServeAuth` closes an entered channel, waits on `r.Context().Done()`, and then writes `503 auth_unavailable` with `ReasonExchange`, the answer the browser gives a cancelled exchange (`internal/auth/browser.go:364-368`); the request is `GET /auth/callback` over the socket; the test receives from the entered channel, cuts, and asserts the client reads zero bytes before `io.EOF`, the audit record is `route=auth_callback`, status 0, `drain_expired`, and no `AuthFailure` was recorded. Today the client reads the `503` envelope and the audit says `auth_unavailable` |
| `TestServeAuth/a token exchange held across the drain bound is drain_expired`, `cmd/profgate/serve_test.go` | `testIssuer` (`:2022-2057`) gains a `/token` case that, under a `holdToken` flag, waits on its request context and answers nothing; the gateway runs the browser flow with `limits{cpu: 1, trace: 1}` as `drain bound` does; a raw TLS client fetches `/auth/login`, keeps the transaction cookie and the `state` of the `Location`, and sends `GET /auth/callback?code=x&state=<state>` with the cookie, holding the socket; `gw.stopOnce()`; after the bound the cut cancels the exchange, the issuer's held request ends with it, the client reads zero bytes before `io.EOF`, and the `request` record with `route` `auth_callback` carries `drain_expired`. Today the record carries `auth_unavailable`, because the cancelled exchange is answered `503` in the interval before `Close`. The subtest takes 31 seconds, as `drain bound` does |
| `TestAClientThatLeavesMidWaitIsAudited` (`:3067-3082`) and the existing download cancel | unchanged and green: a cancel with no cause is still `client_gone` |
| `TestServe/drain bound` (`cmd/profgate/serve_test.go:1021-1043`) | `fakeUpstream.Do` (`:113-120`) records `context.Cause(ctx)` when its context ends before `release` and keeps blocking on `release`, so every drain row keeps the timing it has; the subtest asserts the recorded cause is `httpapi.ErrDrainExpired` after exit. Today it is `context.Canceled`: `Close` cancels with no cause |

The red state:

```bash
go test -race -count=1 ./internal/httpapi/ -run 'TestProfileProxy/the_drain_cut|HeldAcrossTheDrainCut|CutByTheDrain|TestAuthRoutes/an_exchange'
go test -race -count=1 ./cmd/profgate/ -run 'TestServe/drain_bound|TestServeAuth/a_token_exchange'
```

The last two take 31 seconds each.

- [ ] **Classify the cut and say so**

`CHANGELOG.md`, `### Changed`:
**BREAKING: a request the drain bound cuts is audited `drain_expired`, and `cancelled` leaves the `code` set.**
A connection the drain closed after its bound was audited `client_gone`, the code for a client that left on its own,
so a rollout that cut long profiles read as impatient clients.
The cut now carries its own code, `drain_expired`, in the audit record and the `code` label, on every route;
it is audit-only, never an envelope, and a query over `client_gone` stops matching it.
`cancelled` was documented among the audit-only values and written by nothing;
it is no longer documented, and a query that named it matched nothing before and matches nothing now.

- [ ] **Validate and commit**

```bash
semlf check internal/httpapi/server.go internal/httpapi/auth.go internal/httpapi/pgo.go internal/httpapi/codes.go \
  cmd/profgate/serve.go docs/deployment.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/httpapi/server.go internal/httpapi/auth.go internal/httpapi/pgo.go internal/httpapi/codes.go \
  internal/httpapi/codes_test.go internal/httpapi/fixtures_test.go internal/httpapi/server_test.go \
  internal/httpapi/auth_test.go internal/httpapi/pgo_collections_test.go \
  cmd/profgate/serve.go cmd/profgate/serve_test.go docs/deployment.md CHANGELOG.md
git commit -m "feat: audit a drain cut as drain_expired" -m "<body: the drain closed connections through Close alone, which cancels with no cause, so a cut request read as a client that left and could still be answered an envelope; the drain context, the cause, the one layer that reads it, and the code nothing wrote>"
git log --oneline -1 && git status --short
```

---

## 8. An exit before readiness skips the drain delay

Closes the roadmap bullet beginning *Every fatal startup path sleeps `server.drainDelay` before exiting*.

**Files:**
- Modify: `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the mechanism: a flag set by readiness, read beside the mode.
Beside `draining` (`cmd/profgate/serve.go:159`):

```go
	// answeredReady is set the first time /readyz answers 200.
	// Until then nothing routes to this replica, so an exit spends no endpoint-removal window.
	var answeredReady atomic.Bool
```

`ready` (`:172-174`) stores true into it when it returns true;
the ops handler is its only caller (`:235`, `internal/ops/ops.go:25-31`).
The guard at `:380` becomes `delay > 0 && mode == drainEndpoints && answeredReady.Load()`,
and the comment above it (`:371-379`) says both conditions.
The five fatal callers and the stop request (`:506-510`) are unchanged;
`shutdownMode`'s comments (`:57-67`) say that `drainEndpoints` spends the window only once the replica has been ready.

- [ ] **Write the test**

Two subtests of `TestServe`, each with `gatewayOpts{drainDelay: 30 * time.Second}`, six times the wait they allow.

| Test | What it asserts, and how it fails today |
|---|---|
| `a refused preflight exits without the drain delay` | `denyWatch(cs, "pods")` (`cmd/profgate/serve_test.go:295-299`); `gw.exitCode(t, waitTimeout)` is 1, and no record carries `msg` `draining; waiting for endpoint removal`. Today `shutdown` sleeps the 30 seconds at `cmd/profgate/serve.go:382` and the 5-second wait fails first |
| `a stop before readiness exits without the drain delay` | the blocked-preflight reactor of `responses during preflight` (`:907-912`); `/healthz` answers, `/readyz` is 503, `gw.stopOnce()`; `gw.exitCode(t, waitTimeout)` is 0 with no endpoint-removal record. Today the same sleep |

`drain delay holds the api listener open` (`:983-1000`) stays green and is the other half of the rule:
that replica was ready, so the window is spent.

The red state:

```bash
go test -race -count=1 ./cmd/profgate/ -run 'TestServe/a_refused_preflight|TestServe/a_stop_before_readiness'
```

Each red run spends the 30 seconds before its cleanup sees the exit.

- [ ] **Follow readiness and say so**

`CHANGELOG.md`, `### Fixed`:
**An exit before `/readyz` has answered 200 does not wait `server.drainDelay`.**
A refused preflight, a failed issuer discovery, a failed NATS preflight, and a stop request during startup each slept the delay before exiting,
though nothing had ever been routed to the replica.
The window is spent only by a replica that has been ready;
a crash loop at startup is now as fast as the failure it reports.

- [ ] **Validate and commit**

```bash
semlf check cmd/profgate/serve.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add cmd/profgate/serve.go cmd/profgate/serve_test.go CHANGELOG.md
git commit -m "fix(serve): no drain delay before readiness" -m "<body: every fatal startup path slept the endpoint-removal window for a replica nothing had routed to; the window now follows whether /readyz ever answered 200 rather than which path is exiting>"
git log --oneline -1 && git status --short
```

---

## 9. Close the plan

**Files:**
- Modify: `docs/plans/interactive-deadlines.md`, `docs/plans/roadmap.md`

Line 3 becomes `**Status:** Done` and line 4 `**Outcome:** pull request #<n> ...`,
naming the pull request that carries the eight tasks above,
and in the same commit the roadmap item's `Shipped:` line names that pull request,
the shape commit `528a4a1` gave the previous plan's closing commit.
The pull request is named rather than a commit because the merge rebases this branch onto `main`
and rewrites every hash on it, while the number is the same before and after;
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) admits a pull request there for that reason,
and `check_status` in [`check-repo.py`](../../scripts/check-repo.py) requires `**Outcome:** ` followed by text on line 4.
This commit does not delete the plan.
The deletion is the next commit that touches the file, after the merge,
the protocol [`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records
and the retirement of the previous plan followed in pull request #25;
it deletes this file and rewrites every link that cited it, which `check_links` enforces, and changes nothing else.
`grep -rn interactive-deadlines --include='*.md' .` finds the links.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/interactive-deadlines.md docs/plans/roadmap.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add docs/plans/interactive-deadlines.md docs/plans/roadmap.md
git commit -m "docs: close the interactive deadlines plan" -m "<body: the item's six bullets are done and its Shipped line names the pull request; the plan is Done>"
git log --oneline -1 && git status --short
```

---

## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite:

```bash
mise run test:e2e
```

It is required.
[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
lists `internal/k8s` and `internal/proxy` among the eight packages that need the suite on the `current` lane before a pull request,
and this plan changes both.
What the suite proves here is narrow:
the klog sink meets a real API server, so the reflector's own lines are seen routed under a real list and watch,
and the bounded transport meets real Pods over a real network.
It proves nothing about a slow client:
no scenario drives a client that stops reading, drips a body, or idles a connection,
and none cuts a drain mid-stream;
the unit tests above are the evidence for each of those, on loopback sockets.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **The write deadline under HTTP/2 is not unit-tested.**
  `ServeTLS` negotiates HTTP/2, and `http2responseWriter.SetWriteDeadline` exists,
  but every socket test here runs HTTP/1.1, which is what `httptest.NewServer` speaks.
  Under HTTP/2 the deadline is per stream, and an expiry resets the stream rather than the connection.
  The bound is the same; the mechanism is Go's, and this plan does not test Go.
- **A read deadline that fires cancels the request context.**
  `connReader` cancels it on any read error.
  Both callers of `decodeBody` return before any store call, so nothing runs under the cancelled context;
  a future caller that reads a body and then calls the store would see a cancelled context after a slow body,
  which is the right answer for a client that stalled, and the comment on the deadline says so.
- **No test sends a second request on a connection after a deadline.**
  `conn.serve` clears the write deadline after every handler and rearms the read deadline before every request,
  so a second request works today and after this plan, and a test of it could not fail on any change here.
- **klog's sink is process-wide, and the memo of it is klog's.**
  This repository adds no state for it;
  a second install with a different handler while an informer logs would race,
  which `serve`'s wait for the informers and the logging test's own waits are what prevent.
  A cluster built through `Cluster.New` alone never installs a sink;
  a caller that wants one builds a runtime.
- **A request that completes in the instant of the cut keeps its own code.**
  The relabel applies to a code that already names a cancellation, so a `200` fully written before the cut stays `ok`
  and its terminator is written;
  a cut that lands between the handler's return and the flush leaves the cause set and the code unchanged.
- **Two constants are never observed at their production values.**
  The listener's 120 seconds and the transport's 90 seconds are each observed at a test-shortened value,
  and the constants are read by review and by the pinned-transport inventory.
- **`answeredReady` records a probe's answer, not a Service's membership.**
  A replica whose probe answered 200 once and was then removed from the Service still spends the window on exit.
  That is the spec's rule, and the cost is one delay on a rare path.
- **The per-host idle cap is Go's default.**
  Two idle connections per Pod is `DefaultMaxIdleConnsPerHost`, which the spec leaves as it is;
  a Go release that changed it would change the gateway's, and the inventory row would say so by failing.
- **The Collection sampler is unchanged by the confirmation repair.**
  `internal/pgo/rounds.go:451-460` folds every non-`ErrTargetChanged` error into `ReasonDiscoveryUnavailable`,
  a cancellation included, as it did when the cancellation arrived wrapped.
  Whether a drained sampler should record something else is that package's question and not this plan's.
- **The plan's deletion is not one of its tasks.**
  The closing task leaves the finished document in the tree under the lifecycle checks;
  the commit that deletes it and rewrites its links follows the merge, as the previous plan's did.

---

## Self-Review

- Bullet coverage, one line each:
  the stalled client and the write deadline (task 2);
  the dripping body and the read deadline (task 3);
  the reflector failures through slog (task 5);
  `ErrorLog`, `IdleTimeout`, and the transport's idle pool (tasks 4 and 1);
  the two misattributed outcomes (tasks 6 and 7);
  the fatal startup paths and the delay (task 8).
- Current-source facts this plan rests on, each confirmed by reading the file:
  `io.Copy` at `internal/proxy/proxy.go:173` writes through `w.Write` with no deadline and classifies at `:174-178`;
  the transport literal is `:86-90`, `Options` is `:70-75`, and `resp.Body.Close()` runs at `:150`;
  `classifyBeforeHeaders` is `:187-196`;
  the request is built at `internal/httpapi/server.go:352`, the deferred row and record are `:354-361`,
  `fail` is `:324-331`, the `/auth/` and console dispatch precedes the `/v1` algorithm at `:370-382`,
  `serveAuthRoute` is `internal/httpapi/auth.go:143-175` and hands the unwrapped writer to `ServeAuth` at `:167`,
  the token exchange runs on `r.Context()` at `internal/auth/browser.go:364-368`,
  the listing routes fail through `fail` at `internal/httpapi/listing.go:86-137`,
  `fakeRoutes` is `internal/httpapi/auth_test.go:64-80`, `testIssuer` is `cmd/profgate/serve_test.go:2022-2057`,
  the budget starts at `internal/httpapi/server.go:714` with `budgetGrace` at `:31`,
  the slot is released by the defer at `:709`, the confirmation branches are `:717-738`,
  the outcome is copied at `:750-751`, the envelope after `Do` is written at `:755`, and the abort is `:760-764`;
  `decodeBody` reads at `internal/httpapi/pgo.go:207` and `rejectBody` at `:246` and drops the error,
  called from `internal/httpapi/pgo_collections.go:471` and `:1099` and `internal/httpapi/pgo_policy.go:120` and `:197`;
  the Collection dispatch is `internal/httpapi/pgo.go:131-156`, `readCollection` maps store errors at `:168-187`,
  and `servePolicyRead` does at `internal/httpapi/pgo_policy.go:86-90`;
  the two cancellation sites in `pgo_collections.go` are `:898-902` and `:1040-1046`,
  `streamArtifact`'s abort is `:1047-1051`, and `http.NewResponseController` is already used at `:1035`;
  `codeClientGone` is `internal/httpapi/pgo.go:34` and `codeCollectionCancelled` is `:35`, written by no non-test code
  and read by two test lines that mean `string(pgo.StateCancelled)`;
  the audit-only comment is `internal/httpapi/codes.go:101-104`
  and the inventory is `internal/httpapi/codes_test.go:71-75`;
  `setBeforeAllowlist` is the one seam in `internal/httpapi/export_test.go:5-9`;
  `fakeDiscovery.Confirm` is `internal/httpapi/fixtures_test.go:151-159`, `harness.gate` is `:456`,
  `newTrap` is `:407-435`, the recorder's rows are `:281-290` and `:315-326`, `recorder.snapshot` is `:371`,
  `expectAudit` is `:711`, `fakeKV.Get` is `:932-953` and ignores its context and signals nothing when entered,
  `newRecord` is `:1699`, `seedRecord` is `:1725`, and `held` is `:1925-1950`;
  the committed-stream-failure test at `internal/httpapi/server_test.go:714-772` runs a real `httptest.Server` over the handler;
  `Confirm` guards its read at `internal/k8s/confirm.go:33-38` and wraps at `:44`,
  `deadlineGuard` returns `ctx.Err()` at `internal/k8s/client.go:84-93`,
  `NewClientset` builds at `:48`, `NewRuntime` is `internal/k8s/runtime.go:30-37`, `NewRuntimeWithClientset` is `:41-43`,
  `New` is `internal/k8s/cluster.go:39-55` and `Run` joins the informers at `:72-73`,
  `Options.Logger` is `internal/k8s/discovery.go:116`,
  `startFixture` is `internal/k8s/export_test.go:27-47` and `waitCache` is `:50-60`,
  and the `api timeout` row is `internal/k8s/confirm_test.go:257-280`;
  `profgate_confirm_total` is a `CounterVec` on `result` at `internal/metrics/prometheus.go:58-60`, written at `:177-182`,
  and its documented results are the `Recorder` comment at `internal/metrics/recorder.go:67`;
  the sampler's confirmation is `internal/pgo/rounds.go:451-460` and its sink is passed at `:463`;
  `readHeaderTimeout` is `cmd/profgate/serve.go:43-44`, `shutdownMode` is `:57-67`, `serveDeps` is `:81-94`,
  the one allowed state is a local of `serve` at `:106`, the runtime is built once at `:151`,
  `ready` is `:172-174` and is passed at `:235` alone, the servers are `:234-235`,
  the delay guard is `:380-383`, the drain goroutine's context is `:395` and it closes at `:404`,
  `cancelInformers()` is `:427`, `serving.Wait()` is `:435`,
  the fatal exits are `:449`, `:461`, `:467`, `:484`, `:493`, `cluster.Run` starts at `:472` with nothing waiting for it,
  the listener exit is `:503`, the stop is `:508`,
  and `natsReady.Store(true)` at `:488` precedes `startPGO` at `:490`;
  `cmd/profgate/serve_test.go` holds `waitTimeout` at `:76`, `fakeUpstream.Do` at `:113-120`, `denyWatch` at `:295-299`,
  `gatewayOpts` at `:445-463`, the `tlsRefresh` copy at `:503`, the rotation subtest at `:772-813`,
  the blocked preflight at `:907-912`, the delay subtest at `:983-1000`, and `drain bound` at `:1021-1043`;
  `internal/ops/ops.go:25-31` is the one reader of `ready`;
  `internal/admit/gate.go:22-29` is `TryAcquire`;
  `k8s.io/klog/v2` is `go.mod:82`, indirect, and client-go and apimachinery are `v0.36.4` (`:22-23`);
  `cmd/profgate` imports `internal/httpapi` at `cmd/profgate/serve.go:23` and `internal/proxy` at `:29`,
  `internal/httpapi` imports `internal/proxy` at `internal/httpapi/server.go:25`,
  and `internal/proxy` imports only `internal/k8s` among this module's packages;
  in Go 1.26.7, `conn.serve` clears the write deadline after every handler, parks with no read deadline when `IdleTimeout` is zero,
  and resets the read deadline before the next request,
  `readRequest` arms a write deadline only under `WriteTimeout`,
  `connReader` cancels the request context on a read error,
  and `body.Close` reads up to 256 KiB of an unread body after the handler returns;
  klog's `SetSlogLogger` is `contextual_slog.go:29-31`, its setter is `contextual.go:72-80` under the contract at `:57-58`,
  `Background` is `:177`, `logr.ToSlogHandler` is `slogr.go:59` in `logr@v1.4.3`,
  klog's `output` sends `Error` and `Fatal` to `logger.Error` and the rest to `logger.Info`,
  logr's slog sink maps verbosity `n` to `slog.Level(-n)` and `Error` to `LevelError`,
  the reflector's failed list reaches `HandleErrorWithContext`, which calls `logger.Error`,
  and its watch that ends with an unclassified error logs at verbosity zero (`reflector.go:664`)
  while a clean close logs at `V(4)` (`:1093`);
  every commit header above is under 50 characters.
- Decided here, with the reason stated where it is carried:
  the write deadline at commit and never cleared by the handler, with the Go facts that make that safe;
  the read deadline cleared only after a full read, because of the post-handler discard;
  one exported constant for both listeners and the body reads, with test seams on the handler's state;
  a deadline's error mapped to a fixed message rather than the socket addresses `net.OpError` carries;
  the probe read bounded and answered as an unreadable body;
  the idle age of the pool as an option a test shortens;
  klog installed once where the runtime is built, with klog's own sink as the memo and `serve` waiting for the informers;
  the klog level asserted as client-go emits it, in the two cases a fake can produce;
  `client_gone` as a fourth confirm result with no interface change, and no fifth for the cut;
  the sentinel in `internal/httpapi` because `main` can be imported by nothing and `internal/httpapi` classifies;
  the cause read at the layer every route shares — `fail`, the deferred record, and the `/auth/` boundary — and no route reading it;
  a cut request ending in `http.ErrAbortHandler` so nothing more is written, committed or not;
  `cancelled` deleted with `drain_expired` in its place;
  the drain delay following a readiness flag rather than a mode per call site;
  two changelog entries marked breaking because a shipped query or code changes what it matches, and six not;
  the plan closed naming the pull request in both the `Outcome:` and the roadmap's `Shipped:` line,
  and deleted by the commit after the merge.
- Left to the implementer: the exact request bytes the raw clients send, taken from `doPGO` and the route table;
  the chunk size and flush cadence of the streaming upstreams, sized to fill loopback buffers within the budget;
  how the fake store serves an object in pieces for the download cut;
  and the wording of every commit body.
