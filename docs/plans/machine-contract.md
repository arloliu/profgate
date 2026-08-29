# A Machine Contract

**Status:** In Progress

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Every task that lands Go code is test-first,
> and each of those begins by adding the declarations its tests name — types, method stubs, generalized test helpers —
> so its first run fails on assertions rather than on compilation.
> Checkboxes (`- [ ]`) track progress.
> The accepted specs outrank this plan:
> where the two differ the spec wins and the plan is the bug.

**Goal:** make the gateway answerable by a program rather than by a person reading text.
Every response names its request (`X-Request-Id`),
every refusal names the inputs the caller has to change (`details`),
a create can be retried without creating twice (`Idempotency-Key`),
a client can wait for a Collection to move instead of polling (`?wait=`),
a build can fetch the newest usable profile in one request (`.../collections/latest`),
a listing can be filtered and paged (`state`, `since`, `origin`, `cursor`),
an artifact outlives the interval that produced it (`artifact.retention >= schedule.every`),
and one document describes all of it (`GET /v1/openapi.json`), checked against the router that serves it.

**Architecture:** the change is concentrated in `internal/httpapi`,
which gains a request identifier, a `details` vocabulary on the error envelope,
one route table that every API-listener route is dispatched from,
a static registry of every envelope code,
the JSON media-type step, and five new pieces of PGO behavior.
`internal/pgo` gains what those need from the store:
`snapshotHash` on the record, the `idem.<hash>` receipt its publisher writes and its sweeper removes,
a per-record pulse the caches fan out from the `job.*` watch they already consume,
a connection-generation broadcast the runtime owns and a session captures, and `code` on `Violation`.
`internal/config` moves one default and adds one cross-field rule.
`internal/ops` echoes the identifier on its three paths.
`internal/metrics` gains one endpoint value.
`internal/client` and `cmd/profgate` gain the create retry the command line has not had.
`internal/ui` changes in one way only: its two envelope codes become registry constants.
`internal/k8s`, `internal/proxy`, `internal/auth`, `internal/natskv`, and `internal/admit` gain nothing;
`deploy/` is untouched.

**Tech Stack:** everything already pinned in [`mise.toml`](../../mise.toml).
**No Go module is added.**
The OpenAPI document is a hand-written JSON file embedded with `go:embed`,
parsed in tests with `encoding/json` alone; no OpenAPI library is introduced.

**Spec:** [`docs/specs/gateway.md`](../specs/gateway.md), `Accepted`, is the design of record for these:
the identifier, the `details` array, the route table, the error-code registry, the document and its check,
the audit record, the metrics label sets, and the drain's effect on a parked request.
[`docs/specs/pgo.md`](../specs/pgo.md), `Accepted`, holds the PGO half:
the receipt, the replay, the wait, the two `latest` routes, the listing filters and the cursor,
the machine form of `limit_exceeded`, and the effective-policy retention rule.
[`docs/specs/auth.md`](../specs/auth.md) already carries the JSON media-type step in its composed order,
and [`docs/specs/cli.md`](../specs/cli.md) defines the create retry this plan lands.
Four rules those documents state are ambiguous, self-contradictory, or narrower than the failure they cover;
*The accepted specs and the roadmap are repaired* closes all four, and it runs first.
Sections are cited by heading name, never by number;
an unqualified heading is the gateway spec's.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/), especially
[`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md).

## Global Constraints

- **The permission invariant does not move.**
  No task adds a Kubernetes API group, resource, or verb:
  `internal/k8s` is not edited at all, so the seven read tuples stay seven.
  Two tests stay green and untouched:
  `TestClusterRoleTuples` in `deploy/deploy_test.go`,
  and `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go`.
  The one new NATS key prefix, `idem.`, lives in `PROFGATE_JOBS`,
  which the shipped account fragment already grants as `$KV.PROFGATE_JOBS.>`
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Paths that touch each key*),
  so the fragment and the test that pins it do not change either.
  The invariant wording in `AGENTS.md`, `README.md`,
  [`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md),
  and the gateway spec's *Permission Boundary* needs no edit.
- **The NATS seam gains no method.**
  `natskv.Client` keeps `Connected`, `Generation`, `Synced`, and `View`;
  `natskv.KV` keeps its five ([`docs/specs/pgo.md`](../specs/pgo.md) *The seam*).
  A waiting request opens no watch and creates no consumer:
  its wake-up is a fan-out over entries the `job.*` watch already delivers,
  and the connection-generation broadcast is fed by the `OnConnectionChange` hook `natskv.Options` already carries
  (`internal/natskv/client.go:124-132`, where `bumpGeneration` runs before the hook,
  so a broadcast receiver always sees the moved generation).
- **Two packages embed files and no third.**
  `internal/ui` embeds the console tree and `internal/httpapi` embeds the one OpenAPI document
  (*Package Layout*).
  No task adds `go:embed` anywhere else.
- **No new metrics label, and no label built from client text.**
  `internal/metrics` gains one endpoint *value*, `openapi`; the label sets stay closed.
  The request identifier, `wait`, and the listing filters are audit fields or nothing at all
  (*Metrics*, [`docs/specs/pgo.md`](../specs/pgo.md) *Metrics*).
- **Nothing a caller can read gains a Pod IP or a resolved port.**
  That holds for the identifier it sent back, every `details` item, every replay body,
  every record a `latest` route answers with, and every audit line
  (*Non-disclosure*, [`docs/specs/pgo.md`](../specs/pgo.md) *Non-disclosure*).
- **A `details` item is written only where a vocabulary exists.**
  `details` is omitted entirely — never `null`, never `[]` — for a code no vocabulary covers (*Errors*).
- No jargon: code comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a review round, or a task name.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on every Markdown file a task writes or edits.
- Commit headers are Conventional Commits under 50 characters, with no trailer of any kind.

---

## What This Plan Leaves Alone

Running PGO collection in its own process is a separate change and none of it is started here.
Out of scope, named so no task drifts into it:

| Not in this plan | Why |
|---|---|
| the collector Deployment and the `profgate collector` subcommand | the loops keep running in the gateway process, as they do today |
| `pgo.preset` and the collapse of the twelve `pgo.limits` ceilings | every ceiling keeps the default it has in `internal/config` today |
| the `collector.<instance>` heartbeat, its watch, its gauge, and `503 collector_unavailable` as an answer | no route answers it in this build |
| the chart's memory and grace-period arithmetic, and its renamed values | `deploy/` is untouched by every task |
| removing `internal/admit.Acquire` | the admission gate is not edited |

**Two things inside that change are in this plan**, and they are here because
[`docs/plans/roadmap.md`](roadmap.md) puts them here and because *The latest completed Collection* depends on them:
`pgo.defaults.artifact.retention` moving from `2h` to `24h`,
and the effective-policy rule `artifact.retention >= schedule.every`,
validated in each of the five places *Ceilings* names.
Both sit textually inside the collector amendment block of
[`docs/specs/pgo.md`](../specs/pgo.md) *Amendments*, which is the overlap a reader should know about;
neither needs a preset, a collector, or a chart change to hold.

---

## Twelve Things This Plan Decides

### The request identifier lives in `internal/httpapi`, and `internal/ops` imports it

The header is set on every response on both listeners (*Request identifier*),
and the ops listener is a different package with a different handler.
`internal/httpapi` exports the middleware and `internal/ops.New` wraps its mux with it.
The import runs one way — `internal/httpapi` imports nothing of `internal/ops` —
which is the shape [`docs/specs/ui.md`](../specs/ui.md) *Package layout* already established:
`internal/ui` imports `httpapi.ErrorEnvelope`.
The alternative is a package of its own,
which the accepted *Package Layout* does not name and which would hold thirty lines two callers use.

### The route table declares the console subtree, and `internal/ui` still writes its own `405`

*The OpenAPI document* says every API-listener route consumes one declaration with no dispatch beside it,
and that the `Allow` header of a `405` is the methods the matched declaration lists.
[`docs/specs/ui.md`](../specs/ui.md) *Package layout* says `internal/ui` owns the method check,
the `405` with `Allow: GET, HEAD`, and every security header on it.
Both hold at once when the declaration is the *source* of a route's methods rather than the writer of every answer:
the router matches `/ui/`, `/ui/{file}`, and `/` against declarations and hands the request to `Console`,
which answers the method check as it does today with the same `Allow` value.
A test drives one refused method per declaration and compares the `Allow` header with the declared methods,
so the two cannot drift.
Making the router answer the console's `405` instead would drop the security headers that answer carries
and leave `internal/ui`'s own branch unreachable.

### The registry covers the console's two codes, and holds one no route answers

*The OpenAPI document* says the registry holds "every code the gateway can write into an envelope",
and that "every constructor of a gateway error ... takes its code from the registry".
`internal/ui` writes two of them — `route_unknown` and `method_not_allowed` —
as literals through `httpapi.ErrorEnvelope` (`internal/ui/ui.go:90`, `:106`, `:112`),
so a registry that excluded them would be a registry of most of the codes.
The constants for those two are exported and `internal/ui` uses them,
which is the one edit this plan makes to that package;
the import already runs that way for `ErrorEnvelope`.
The membership test drives the console's `404` and its `405` and reads the code out of each body.

*Layers* also pins the registry to "exactly the codes of *Errors* and of [`pgo.md`](../specs/pgo.md) *Errors*",
and that table lists `503 collector_unavailable`.
The registry therefore declares it and the document enumerates it, so the check compares equal sets.
The spec already states what this costs: the check does not catch a code no route can answer with,
"which is a document that over-promises rather than one that lies".
Its constant carries a comment saying no route answers it in this build.
Audit-only codes — `cas_contended`, `artifact_stream_failed`, `client_gone`, `upstream_stream_failed` —
are not in the registry and not in the document:
they are never written into an envelope.
Upstream statuses passed through as `upstream_<status>` are out for the same reason.

### A `wait` compares against the record the route already read

*Get a Collection* says the handler registers first and reads second,
and that a wait ends when "an authoritative read after a pulse shows a `state` other than the one the first read returned".
It does not say which read is the baseline, and the two readings differ:
with the baseline taken from the read that *follows* registration,
a transition landing between registration and that read leaves baseline and answer equal,
the buffered pulse re-reads to the same state, and the request parks to its deadline —
which is exactly the case [`docs/specs/pgo.md`](../specs/pgo.md) *Unit* requires to answer at once.
A Collection-scoped route reads its record before the realm step already
(`internal/httpapi/server.go:464`, `internal/httpapi/pgo.go:87`),
so the baseline is free: it is that read.
The order is therefore read (for the realm and the baseline), register, read again, compare.
**A Draft plan does not resolve an ambiguity inside an Accepted spec quietly.**
*The accepted specs and the roadmap are repaired* writes the two reads into the spec,
and it runs before the task that implements from them.

### The generation broadcast is pushed, never polled

[`docs/specs/pgo.md`](../specs/pgo.md) *Unit* requires the mid-wait refusal to be driven by the broadcast,
never by a timer,
and fails a handler that "reads `Generation()` once and never learns of the move".
A ticker over `Generation()` is therefore not an implementation of it.
`natskv.Options.OnConnectionChange` already fires in the disconnected callback,
immediately after `bumpGeneration` has moved the generation (`internal/natskv/client.go:124-132`),
and `cmd/profgate` already wires it to the connection gauge at `cmd/profgate/serve.go:606`.
It gains a second consumer: the broadcast a parked handler selects on.
The seam gains nothing, which is what *The seam* requires.

**The broadcast belongs to `pgo.Runtime`, not to `Caches`, for two reasons.**
The first is construction order.
`natskv.Options` is built inside `natsPreflight` at `cmd/profgate/serve.go:597-610`,
and `pgo.NewCaches` runs at `:650`, after preflight has returned —
so a closure over the caches cannot exist when the option is built.
`pgoRuntime` is constructed at `:196`, before the preflight goroutine at `:344` starts,
which is exactly the late-bound holder this needs.
`natsPreflight` therefore takes the runtime beside `deps` and composes one `OnConnectionChange`
that reports to the gauge and moves the broadcast,
so the single field keeps its one existing consumer and gains the second.

The second reason is that the channel a parked handler waits on has to be the one its own reads belong to.
A broadcast read as "the channel that is current now" loses a wake-up:
a generation that moves after the handler's authoritative read
and before it asks for the channel hands back the *replacement*,
and the move that already happened is signalled on a channel nobody holds —
so the handler parks to its deadline over an outage it should have answered `503 pgo_unavailable` for.
`Session()` therefore captures the channel of its own generation in the same step
that takes the generation and the view (`internal/pgo/runtime.go:81-101`),
and `Session.GenerationMoved()` returns that captured channel rather than reading a field again.
A generation that moved before `Session()` ran leaves `View(gen)` failing, which is the existing answer;
one that moves after it closes the channel the session holds, whenever the handler gets round to selecting on it.

### `snapshotHash` is a hash of the policy struct, not of the request bytes

*Create a Collection* says a replay compares "the canonical effective-policy snapshot",
and that "bytes, whitespace, and field order in the request therefore decide nothing,
and identical JSON can still mismatch".
The hash is SHA-256 over `json.Marshal` of the `pgo.Policy` value the request would publish,
whose field order is the struct's and whose durations render through the existing `Duration` marshaller.
It is computed by one function in `internal/pgo` that both the publisher and the handler call,
so the value stored on the record and the value a replay compares can never be produced two ways.

### The `Idempotency-Key` grammar is checked before the body is decoded

*Create a Collection* places the receipt lookup after the body decode and before every refusal that writes nothing,
but does not say where the header's own grammar is checked.
It is checked at the parameter step, with the other header refusals,
so a key the gateway cannot read is refused whatever the body carries.
That is the reading the spec's own sentence supports —
"a key the gateway cannot read is refused rather than replaced ... this header decides whether a Collection is created" —
and it keeps one rule for a request carrying two faults.

### `violations` keeps its dotted field and gains `code`; only the `details` item uses a pointer

*Ceilings* asks for one vocabulary in two renderings:
`GET /pgo` publishes `violations` with `field`, `ceiling`, `detail`, and now `code`,
while the `details` array of a `400 limit_exceeded` writes the same field as a JSON pointer (`/schedule/every`).
`pgo.Violation` therefore keeps `Field` dotted and gains `Code`,
and the conversion to a pointer happens in the one `internal/httpapi` function that builds the items.
Renaming the stored field would change a body every current client of `GET /pgo` already reads.

### The identifier is generated for every request, including the ones that write no record

*Request identifier* sets the header on `/ui/`, `/`, and the three ops paths, which write no audit record.
The middleware therefore mints or accepts the value and sets the header before any routing decision,
for every request on either listener,
and the audit record reads it from the request rather than minting one of its own.
One request has exactly one identifier and one record at most.

### The selection that finds the latest artifact hands back the reader it confirmed

*The latest completed Collection* has both routes "confirm the object the record names is in the store"
and requires that neither "answer `410 artifact_gone` while an intact artifact exists".
A selection that confirms an object and then hands its record to a download helper that opens the object again
(`internal/httpapi/pgo_collections.go:245`) cannot keep that promise:
the sweeper can delete between the two, and the second open answers `410`
while the completed Collection behind it still has its bytes.

The selection therefore **is** the open.
`LatestCompleted` returns the record together with the open `io.ReadCloser` its own confirmation produced,
and the caller closes it;
`latest` closes it at once, which costs the same one `Get` a probe would have cost,
and `latest/profile` streams the reader the walk confirmed.
An object that disappears mid-stream is then the ordinary truncation `internal/httpapi` already classifies
as `artifact_stream_failed`, which is what happens to a download that started before an expiry today.
The alternative — a second confirmation before the second open — narrows the window without closing it,
and a window that is merely narrow is one nobody will test.

### The cursor is self-contained and versioned, and claims no authenticity

*List Collections* asks that a cursor "encode the `createdAt` and the `id` of the last entry the response carried,
and the `state`, `since`, and `origin` filters the request that produced it carried",
and that it be opaque.
It asks for nothing about who minted it, and a gateway could not promise that if it wanted to:
replicas share no signing material and no cursor state,
so a token minted by one replica must be readable by every other.
The plan therefore claims only what the design supports:
a decoding that is strict and total.

The token is a version byte and a strict encoding of the four values,
refused with `400 invalid_parameter` when it does not decode,
carries a version this build does not know,
or decodes to values outside their own grammars — a `state` outside the closed set, a `since` that is not a timestamp.
An attacker who forges one names a position and a filter set,
which is what any client can already ask for by sending the filters and paging to that point.
The `state` filter is repeatable, so both the request's and the token's are compared **as sets**:
`state=running&state=pending` matches a token minted under `state=pending&state=running`,
and a repeated identical value does not make two filter sets differ.
A test mints a token in one runtime and consumes it in another, which is the cross-replica case stated as a test.

### An unknown body field is located before the body is decoded

*Errors* gives `unknown_field` "a pointer into the body", and a create body nests two levels
(`sampling.rounds`, `artifact.retention`).
`encoding/json` with `DisallowUnknownFields` (`internal/httpapi/pgo.go:162-163`) reports a field *name* and no path,
so `{"sampling": {"bogus": 1}}` and `{"bogus": 1}` produce the same message
and a pointer built from it would be wrong for the nested case.

`decodeBody` therefore locates the fault before it decodes:
it unmarshals the bounded bytes into `map[string]any` once,
walks that against the target type's declared JSON names, reflected once per type,
and reports as a pointer the first unknown name it reaches in document order,
then decodes strictly into the value as it does today.
The cost is one extra unmarshal of at most 64 KiB on a body that is about to be rejected,
and on an accepted body the walk finds nothing and the strict decode is unchanged.
A body that is not JSON at all fails the first step and is `body_malformed`, which is the same answer as today.

---

## File Structure

```text
docs/specs/pgo.md                      # the wait's two reads, its deadline, and a receipt with no Collection
docs/specs/cli.md                      # which create failures the command line retries
docs/plans/roadmap.md                  # what a ticked bullet records
internal/httpapi/requestid.go          # the grammar, the generator, the middleware
internal/httpapi/errors.go             # the details vocabulary and its constructors
internal/httpapi/codes.go              # the static registry of envelope codes
internal/httpapi/routes.go             # one declaration per API-listener route
internal/httpapi/openapi.json          # the hand-maintained document
internal/httpapi/openapi.go            # the embed and the route that serves it
internal/httpapi/openapi_test.go       # the four comparisons against the code
internal/httpapi/server.go             # the table lookup, the media-type step, the new dispatches
internal/httpapi/audit.go              # requestId first, wait beside explain
internal/httpapi/profile.go            # parameter faults carry their items
internal/httpapi/pgo.go                # body and header faults carry theirs
internal/httpapi/pgo_policy.go         # limit_exceeded in machine form
internal/httpapi/pgo_collections.go    # the receipt lookup, the wait, latest, the filters and the cursor
internal/ui/ui.go                      # its two envelope codes come from the registry
internal/ops/ops.go                    # the identifier on the three ops paths
internal/metrics/recorder.go           # EndpointOpenAPI
internal/pgo/policy.go                 # Violation.Code and the retention rule
internal/pgo/record.go                 # snapshotHash and the canonical hash
internal/pgo/publisher.go              # the receipt write and the withdraw
internal/pgo/caches.go                 # per-record pulses and the generation broadcast
internal/pgo/runtime.go                # the session's receipt reads, subscriptions, and latest walk
internal/pgo/sweeper.go                # the receipt rules
internal/config/config.go              # the retention default and the default-validation rule
internal/client/collect.go             # the create retry
cmd/profgate/serve.go                  # the drain signal and the broadcast wiring
cmd/profgate/collect.go                # the retried create
test/e2e/scenarios_pgo_test.go         # the key, the wait, and the latest assertions
docs/api.md, docs/pgo.md, docs/cli.md, docs/configuration.md
CHANGELOG.md
docs/plans/machine-contract.md
```

---

## The accepted specs and the roadmap are repaired

**Files:**
- Modify: `docs/specs/pgo.md`, `docs/specs/cli.md`, `docs/plans/roadmap.md`

Three documents say less than the tasks below read out of them.
[`docs/specs/pgo.md`](../specs/pgo.md) contradicts itself in two places this plan implements from,
and [`docs/specs/cli.md`](../specs/cli.md) describes a narrower retry than the failure it exists to survive.
No repair here is a design change: each writes down a rule the accepted design already depends on,
or the meaning a glyph already carries.
This task is first because four of the five repairs are the source the later tasks implement from,
and a plan may not implement around a contradiction inside an accepted spec.

- [x] **Name the two reads a `wait` makes**

*Get a Collection* says the handler "registers first and reads second"
and that a wait ends when an authoritative read "shows a `state` other than the one the first read returned".
Two implementers read "the first read" as two different reads,
and only one of the two passes the case *Unit* pins:
a record that moves between the handler's registration and its first read answers at once.
Rewrite the paragraph so it names both reads:
a Collection-scoped route has already read the record to evaluate the realm,
that read is the state a wait compares against,
the handler then registers its channel and issues an authoritative `Get`,
and it answers at once when that read is terminal or shows a different `state`.
Say in the same place why the order is that way:
registering before the read is what stops a transition landing between them from pulsing a channel that does not exist,
and comparing against the earlier read is what keeps a transition inside that window visible:
without it, that transition reads as the state the client already had.
Leave the pulse rules, the clamp refusal, `X-Wait-Elapsed`, and the four other ways a wait ends as they are.

- [x] **Say what the deadline reads**

*Get a Collection* answers the same question twice and differently.
The list of endings says "`wait` elapses, and the record as last read is the answer",
while the pulse rules two paragraphs below say
"the next pulse — **or the wait's own deadline** — brings the handler back to a read
that sees whatever the bucket holds by then",
and that "the one thing a dropped pulse can cost is latency inside the wait, never a wrong answer".
Both cannot hold.
A terminal transition whose pulse was dropped, or one that becomes ready in the same instant as the timer,
is answered stale under the first and correctly under the second.

The pulse rules are the reading to keep, for two reasons.
They are the section's stated invariant — a dropped pulse costs latency and never an answer —
and the other reading turns a coalesced pulse, which the design deliberately allows, into a wrong answer.
Rewrite the ending so it agrees:
the deadline, like a pulse, brings the handler back to one authoritative `Get`,
and that read is the answer.
Say in the same place that this is what keeps the two rules one rule:
every answer a wait gives comes from a read taken after the event that ended the wait,
never from a read taken before it.
The drain ending stays as it is — it answers with the record it last read,
because a draining replica is being taken out of service and its store call may not return.

**The normative test row moves with the prose, in the same edit.**
*Unit* requires that a wait which expires
"answers the record as last read with an elapsed value at least the duration asked for",
which is the reading this repair drops;
left as it is, it would fail the handler the repaired prose asks for.
Rewrite it so the wait that expires answers the record its final read returned,
keeping the elapsed clause it already carries,
and add beside it the case the repair exists for:
a terminal transition whose pulse was dropped is answered at the deadline, not reported as the state before it.
A repair that changed the prose and left the row would leave the document arguing with itself in a new place.

- [x] **Say what a receipt whose Collection never ran answers**

*Create a Collection* gives two answers for a keyed record the scan failed `not_published`.
Its lookup rule says "a receipt whose record exists is a replay when its `snapshotHash` equals this request's",
and its guarantee runs "for as long as the record exists";
*Unit* requires that "a keyed record the scan failed `not_published` answers no replay,
and a retry with its key creates anew".
The section's own argument for the second reading is already written —
such a record "never ran: no worker claimed it and no sample was taken" —
but the sentence that carries it,
"the only records a reader can find without a receipt are therefore an `initializing` one
and a `failed` one whose reason is `not_published`",
assumes those records have no receipt.
They can have one: the receipt is written before the `pending` update,
so a creator that dies in that window leaves a receipt naming an `initializing` record,
which the scan then fails `not_published`.

Rewrite the lookup rule to name that state:
a receipt whose record exists is a replay **unless** that record is `failed` with reason `not_published`,
which never became claimable and never ran;
such a receipt is stale in the same sense as one whose record is gone,
and the handler deletes it at the revision its own `Get` returned and creates as a request with no history does.
Correct the "without a receipt" sentence in the same place,
so it says what is true of those two records — neither ever ran — rather than what is true of their receipts.
Leave the guarantee as it is: it is about a Collection that was published, and this one was not.

- [x] **Widen the retry the command line promises**

*Collections* says `collect` "retries the create with that key on a transport failure or a `5xx`".
Those two do not cover the failure the header exists for.
`Create` obtains the response and reads its body afterwards (`internal/client/collect.go:55`, `:60`),
so an answer whose `202` headers arrived and whose body was cut off is neither a transport failure nor a `5xx`;
under the sentence as written it is reported,
and the caller is left holding no identifier for a Collection that is running —
the exact outcome *Collections* opens by describing.

Widen the classification to what the section already argues for:
a create is retried under its key whenever its result is unknown,
which is no answer at all, an answer the gateway could not complete (`5xx`),
and an answer that did not arrive whole.
An answer that arrived whole and says something is not retried,
which keeps `429 collection_in_progress` and every other `4xx` exactly where they are.
Add the third case to the *Testing* list beside the `5xx` row it already carries,
so the spec asks for the test the command-line task writes.

- [x] **Say what a ticked bullet records**

[`docs/plans/roadmap.md`](roadmap.md) has one checkbox with two meanings and no statement of which applies where.
`git show bfdad0f` ticked every bullet of five unimplemented items in one commit
and added the line "A ticked spec bullet means the revision is in the spec;
the implementation plan and the code follow it".
Two defects follow.
The glyph is overloaded: on an item with no spec — the release, the chart templates, the document deletions —
a tick means the work shipped, and on an item with a spec it means only that the spec revision landed.
And the legend describes "a ticked spec bullet",
while several items word their bullets as behavior rather than as revisions —
"`X-Request-Id`: accepted from the client or generated, echoed on every response" reads as shipped behavior when ticked,
and none of it is served today.

Rewrite the two legend lines in the opening block quote so that both meanings are stated
and a reader can tell which one applies to the bullet in front of them:
a tick records that the item's design decision is settled in the spec the item names,
whatever the bullet's wording;
for an item that names no spec, a tick records that the work itself is done,
because there is no revision for it to mean instead.
Say in the same place where shipping is read instead —
the item's plan under `docs/plans/`, and `CHANGELOG.md` once it ships —
so the checkbox is not asked to carry a state it cannot hold.
The wording must fit an item whose bullets are split between the two kinds
(the small removals name a spec for one bullet and none for the other five)
and an item with no checkboxes at all (the withdrawn library comparison).

No bullet is ticked or unticked by this task.
Item 6's bullets stay as they are: their spec revisions are in the specs, which is what the tick now says.

- [x] **Validate and commit**

All three files keep the `Status:` line they have;
`check_status` in `scripts/check-repo.py` verifies line 3 of each.

```bash
semlf check docs/specs/pgo.md docs/specs/cli.md docs/plans/roadmap.md
mise run lint && mise run test && mise run check
git add docs/specs/ docs/plans/roadmap.md
git commit -m "docs: settle the rules this plan reads"
```

---

## Every response carries a request identifier

**Files:**
- Create: `internal/httpapi/requestid.go`, `internal/httpapi/requestid_test.go`
- Modify: `internal/httpapi/server.go`, `internal/httpapi/audit.go`,
  `internal/ops/ops.go`, `internal/ops/ops_test.go`

*Request identifier* defines the grammar, the generated value, where the header is set, and what reads it.
*Logging* puts `requestId` first in every record shape.
*Health* puts it on all three ops paths.

**Produces:**

```go
// RequestID returns the identifier for one request: the client's when the
// request carries exactly one X-Request-Id of 1 to 128 bytes drawn from
// [A-Za-z0-9._-], and 16 bytes from crypto/rand as 32 lowercase hexadecimal
// characters otherwise. A value the gateway will not take is replaced, never
// refused: the identifier decides nothing.
func RequestID(r *http.Request) string

// WithRequestID sets X-Request-Id on every response next writes, from the
// client's value or a generated one, and puts the value on the request context
// so a handler names the same one in its audit record.
// internal/ops wraps its mux with it; the API listener sets it in ServeHTTP.
func WithRequestID(next http.Handler) http.Handler
```

**Where it is set.**
`ServeHTTP` at `internal/httpapi/server.go:356` already sets `Cache-Control` before it routes;
the identifier is set beside it, before the console dispatch at `:371` and the `/auth/` dispatch at `:377`,
so the console's `404`, its files, an `/auth/` `302`, and every error envelope carry it without another writer.
A forwarded upstream response carries it because it is already in the header map when the proxy writes:
`allowedHeaders()` in `internal/proxy/proxy.go:45-47` copies five upstream headers and `X-Request-Id` is not one,
so an upstream's own value never reaches the client and the gateway's is not overwritten.
`internal/proxy` is not edited.

**The audit record.**
`auditRecord` in `internal/httpapi/audit.go:14-31` gains `requestID`,
filled from the request at the top of `ServeHTTP`,
and `writeAudit` prepends `"requestId", rec.requestID` to each of its three attribute lists,
so the field is first on the interactive record, the PGO one, and the `/auth/` one.
The console arm writes no record and is unaffected.
`labels()` at `server.go:283` is untouched: the identifier is not a metrics label (*Metrics*).

- [x] **Add the compile seams**

Declare `RequestID` and `WithRequestID` returning a fixed value and the handler unchanged,
add the `requestID` field to `auditRecord`,
and let `internal/ops.New` wrap its mux, so every assertion below fails on content.

- [x] **Write the identifier tests**

`requestid_test.go` restates *Request identifier* and the identifier row of *Layers*,
against the assembled handler and against `ops.New`:

| Request | Response header |
|---|---|
| one `X-Request-Id` of one byte | that value, unchanged |
| one of 128 bytes | the same |
| one holding every character of `[A-Za-z0-9._-]` | the same |
| no header | 32 lowercase hexadecimal characters |
| an empty value | a generated one |
| 129 bytes | a generated one |
| a space, a colon, a `CR`, an `LF`, or a non-ASCII byte | a generated one, one row each |
| two `X-Request-Id` headers | a generated one |
| two requests with no header | two different generated values |

| Surface | Expect |
|---|---|
| a `200` from the targets endpoint | the header, and `Cache-Control: no-store` |
| every gateway error envelope | the header |
| a `405` | the header beside `Allow` |
| a console file response and its `404` | the header, with the console's own cache policy intact |
| an `/auth/` `302` | the header |
| a forwarded upstream response whose upstream sent its own `X-Request-Id` | the gateway's value, and the upstream's nowhere in the response |
| `/healthz`, `/readyz` passing and failing, and `/metrics` | the header, and a `text/plain` body in both outcomes of each |
| any response | exactly one `X-Request-Id` header value |

| Record | Expect |
|---|---|
| a targets request | one audit record whose first attribute is `requestId`, equal to the response header |
| a PGO request | the same |
| an `/auth/` request | the same |
| a request under `/ui/` | no audit record, and the response still carries the header |
| `/v1/auth` | no audit record, and the header present |
| any request | the recorder sees no label built from the identifier |

- [x] **Run the tests and watch them fail**

- [x] **Implement the grammar, the middleware, and the record**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/ops/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/ops/
git commit -m "feat(httpapi): give every response a request id"
```

---

## An error names the inputs it refuses

**Files:**
- Modify: `internal/httpapi/errors.go`, `internal/httpapi/profile.go`, `internal/httpapi/pgo.go`,
  `internal/httpapi/pgo_policy.go`, `internal/httpapi/listing.go`, `internal/httpapi/server.go`,
  `internal/pgo/policy.go`, `internal/pgo/policy_test.go`,
  and the `internal/httpapi` tests of each refusal

*Errors* defines the item shape, the rule that an error with no vocabulary carries no key at all,
and the `invalid_parameter` vocabulary in full.
[`docs/specs/pgo.md`](../specs/pgo.md) *Ceilings* defines the machine form of `limit_exceeded`.
`details` exists today for one code:
`portNotAllowed` at `internal/httpapi/profile.go:307-315` fills it with `not_admitted`
and the comment at `errors.go:19` says so.
This task makes every refusal that has a vocabulary carry one.

**Produces:**

```go
// The details codes of invalid_parameter, one per refusal Errors names.
const (
    detailUnknownParameter        = "unknown_parameter"
    detailRepeatedParameter       = "repeated_parameter"
    detailEmptyParameter          = "empty_parameter"
    detailMalformedParameter      = "malformed_parameter"
    detailParameterNotApplicable  = "parameter_not_applicable"
    detailMutuallyExclusive       = "mutually_exclusive"
    detailHeaderRequired          = "header_required"
    detailHeaderMalformed         = "header_malformed"
    detailUnknownField            = "unknown_field"
    detailFieldNotApplicable      = "field_not_applicable"
    detailBodyNotAllowed          = "body_not_allowed"
    detailBodyMalformed           = "body_malformed"
)

// invalidParameter is 400 invalid_parameter with the items the refusal earns,
// in the order the parameters were validated, which is name order.
// A caller that has no item to give passes none, and the body then carries no
// details key at all.
func invalidParameter(message string, items ...errorDetail) *requestError

// paramFault, headerFault, and bodyFault build one item apiece, so a field name
// and its code are chosen in one place per kind rather than at each call site.
func paramFault(code, name, message string) errorDetail
func headerFault(code, name, message string) errorDetail
func bodyFault(code, pointer, message string) errorDetail
```

`invalidParameter` at `profile.go:317` takes a message today and every caller passes one;
it gains a variadic item list, so a call that has nothing to name compiles unchanged
and the ones that do name it at the fault.

**Where each item is produced.**

| Refusal | Item |
|---|---|
| `parseTargetsParams` and `parseProfileParams` (`internal/httpapi/profile.go`) | one per rule of the table below, naming the parameter |
| the raw query string failing `url.ParseQuery` (`server.go:490`) | `malformed_parameter` with an empty `field` |
| `access_token` as a query parameter (`server.go:449`) | `unknown_parameter` naming `access_token` |
| the listing routes refusing every parameter (`internal/httpapi/listing.go`) | `unknown_parameter` naming it |
| `seconds` on a profile that does not take it | `parameter_not_applicable` |
| `port` with `portName` | two items, one per name, in name order |
| `If-Match` outside its grammar (`internal/httpapi/pgo_policy.go`) | `header_malformed` naming the header |
| `decodeBody` on an unknown field (`internal/httpapi/pgo.go:166`) | `unknown_field` with a pointer to it, located by the walk *An unknown body field is located before the body is decoded* describes |
| `decodeBody` on a body that is not JSON or over `maxBodyBytes` | `body_malformed` with an empty `field` |
| `rejectBody` on a route that accepts none (`pgo.go:191`) | `body_not_allowed` with an empty `field` |
| `enabled` or `schedule` in a create body (`pgo_collections.go:129`) | `field_not_applicable` with a pointer to each, in the order the table above lists them |

**`limit_exceeded` in machine form.**
`pgo.Violation` at `internal/pgo/policy.go:317-321` gains `Code string \`json:"code"\``,
and every `add` call in `Validate` names one of `above_maximum`, `below_minimum`, `out_of_range`, or `not_permitted`
([`docs/specs/pgo.md`](../specs/pgo.md) *Ceilings*).
The fifth value, `retention_under_interval`, arrives with the rule that produces it, in a later task.
`Field` stays dotted, because `GET /pgo` publishes it and clients read it;
`limitExceeded` at `internal/httpapi/pgo_policy.go:312` builds one item per violation,
converting `schedule.every` to `/schedule/every` there and nowhere else,
keeping the violation's `Detail` as the item's `message` and its `Code` as the item's `code`.
The envelope's own message keeps the text it writes today.

- [x] **Add the compile seams**

Add the constants, the three builders, `Violation.Code`, and the variadic parameter,
then run `mise exec -- go build ./... && mise exec -- go vet ./...`.

- [x] **Write the refusal tests**

`internal/httpapi` asserts against the **encoded body**, not the struct, restating the `details` row of *Layers*:

| Case | Body |
|---|---|
| every row of the targets and profile parameter tables | one item, whose `code` is the value *Errors* names and whose `field` is the parameter |
| `port` with `portName` | two items in name order, each `mutually_exclusive` |
| a value `allowedSelections` refuses | one `not_admitted` item naming the parameter the client sent |
| a raw query string that does not parse | `malformed_parameter` with `"field": ""` |
| `seconds` on `heap` | `parameter_not_applicable` |
| an unknown top-level field in a `PUT /pgo` body | `unknown_field` with the pointer `/bogus` |
| an unknown field inside `schedule`, inside `sampling`, inside `target`, and inside `artifact` | the nested pointer, one row each: `/sampling/bogus` and never `/bogus` |
| a body with two unknown fields | one item, naming the first in document order |
| a known field carrying a value of the wrong type | today's answer, `body_malformed`, because the walk finds no unknown name |
| a body over 64 KiB, and one that is not JSON | `body_malformed`, `"field": ""` |
| a body sent to `DELETE /pgo` and to the cancel route | `body_not_allowed`, one row each |
| `enabled` and `schedule` in a create body | `field_not_applicable`, one item each, in name order |
| `If-Match: *` and `If-Match: 42` | `header_malformed` naming `If-Match` |
| `403 realm_denied`, `404 service_not_found`, `503 not_ready`, `429 too_many_profiles` | no `details` key at all |
| every error body in the suite | never `"details": []` and never `"details": null` |
| every item in every row above | no Pod name, address, or resolved port |

`internal/pgo/policy_test.go` gains a row per ceiling asserting the `code` each violation carries,
and one asserting that every violation `Validate` can produce carries a non-empty `code`,
so a ceiling added later cannot ship without one.
`internal/httpapi/pgo_policy_test.go` asserts the pointer conversion,
that `GET /pgo`'s `violations` keep the dotted field and gain the same `code`,
and that a refusal with several violations carries one item apiece in the order `Validate` produced them.

- [x] **Run the tests and watch them fail**

- [x] **Implement the items**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/pgo/
git commit -m "feat(httpapi): name the inputs an error refuses"
```

---

## One table declares every route, one registry declares every code

**Files:**
- Create: `internal/httpapi/routes.go`, `internal/httpapi/codes.go`, `internal/httpapi/routes_test.go`
- Modify: `internal/ui/ui.go`, `internal/ui/ui_test.go`,
  `internal/httpapi/server.go`, `internal/httpapi/console.go`, `internal/httpapi/auth.go`,
  `internal/httpapi/errors.go`, `internal/httpapi/pgo.go`, `internal/httpapi/pgo_collections.go`,
  `internal/httpapi/pgo_policy.go`, `internal/httpapi/profile.go`, `internal/httpapi/listing.go`,
  `internal/httpapi/server_test.go`

*The OpenAPI document* requires one declaration per API-listener route
and one static registry of every envelope code, before it requires a document.
This task lands both without adding a route or a code, so no client-visible answer changes,
and the tasks after it add declarations and constants rather than dispatch branches and string literals.

The package has no table today.
`parseRoute` at `internal/httpapi/server.go:213-266` matches four exact paths and three regular expressions;
`isConsolePath` at `console.go:12` and `authRoute` at `auth.go:29` are two more dispatches beside it;
`routeKind.methods()` at `server.go:186-200` is the method list, already one place.

**Produces:**

```go
// declaration is one route the API listener serves: the path template the
// document publishes, the kind that names its handler, and the methods it
// accepts, in the order the Allow header carries them.
type declaration struct {
    Template string
    Kind     routeKind
    Methods  []string
}

// declarations is every route the API listener serves, in document order.
// The router matches a request against it, the Allow header of a 405 is the
// matched declaration's methods, and the check of the OpenAPI document reads
// it. A route absent from it cannot be reached.
func declarations() []declaration

// match resolves a path to its declaration and the segments it captured.
// It is the only path dispatch in the package.
func match(path string) (route, declaration, bool)
```

**The table's contents**, one row per route the API listener serves,
which is the inventory *The OpenAPI document* enumerates:

| Template | Methods |
|---|---|
| `/v1/namespaces/{namespace}/services/{service}/targets` | `GET` |
| `/v1/namespaces/{namespace}/services/{service}/profiles/{profile}` | `GET` |
| `/v1/namespaces/{namespace}/services/{service}/pgo` | `GET`, `PUT`, `DELETE` |
| `/v1/namespaces/{namespace}/services/{service}/collections` | `GET`, `POST` |
| `/v1/collections/{id}` | `GET` |
| `/v1/collections/{id}/profile` | `GET` |
| `/v1/collections/{id}/cancel` | `POST` |
| `/v1/namespaces` | `GET` |
| `/v1/namespaces/{namespace}/services` | `GET` |
| `/v1/whoami` | `GET` |
| `/v1/limits` | `GET` |
| `/v1/auth` | `GET` |
| `/auth/login`, `/auth/callback`, `/auth/logout` | `GET`, one row each |
| `/ui/` | `GET`, `HEAD` |
| `/ui/{file}` | `GET`, `HEAD` |
| `/` | `GET`, `HEAD` |

`/v1/openapi.json` and the two `latest` routes are **not** added here;
each arrives with the task that serves it.

**What changes in `ServeHTTP`.**
The three dispatches become one `match` call.
`isConsolePath` and `authRoute` are deleted, and the kinds they decided become kinds in the table:
a console kind whose handler is `serveConsole`, and one auth-route kind per path whose handler is `serveAuthRoute`.
`q.console`, `q.authRoute`, and the metrics and audit shapes they select stay exactly as they are;
they are set from the matched kind rather than from a prefix test.
`{file}` matches any non-empty remainder under `/ui/`, including the hashed asset tree `internal/ui` serves today,
so the console keeps deciding shell from asset from `404 route_unknown` inside its own handler.
The path grammars `parseRoute` enforces — DNS-1123 labels for `{namespace}` and `{service}`,
`pgo.ValidID` for `{id}` — move into `match` unchanged, and a segment that fails one is still `404 route_unknown`.

**The table changes path dispatch and nothing else: which steps a kind runs is untouched.**
That is not automatic, and it is where this refactor could ship a regression.
`isConsolePath` is tested first today, at `internal/httpapi/server.go:371`,
**before** the readiness step at `:414`,
so a console request serves its files while the caches are still syncing.
`/auth/` dispatches second, at `:377`, and runs a readiness step of its own inside `serveAuthRoute`
(`internal/httpapi/auth.go:155`), after its own method check.
A `match` at the top of `ServeHTTP` that then ran the `/v1` algorithm for every kind would answer
`503 not_ready` for files that are in the binary,
which no spec asks for.
The console kinds therefore dispatch to `serveConsole` immediately after the match,
and the `/auth/` kinds to `serveAuthRoute`, exactly where they go today.
The method step keeps answering `405` with `Allow` from the matched declaration for every `/v1` and `/auth/` route;
for the three console declarations the router hands the request to `Console`,
which writes its own `405` with `Allow: GET, HEAD` and its security headers,
as [`docs/specs/ui.md`](../specs/ui.md) *Package layout* assigns it.
`internal/ui` keeps that answer and changes in one way:
the two codes it writes as literals become the registry's exported constants.

**The registry.**

```go
// The complete set of codes internal/httpapi writes into an error envelope:
// the gateway's own and the PGO ones. No handler here writes a code that is not
// in it, and the OpenAPI document enumerates exactly this set.
// Audit-only outcomes are not codes of this kind: they never reach an envelope.
const (
    CodeRouteUnknown = "route_unknown"
    // ... one per row of Errors and of pgo.md Errors
)

// EnvelopeCodes returns the registry in a stable order.
func EnvelopeCodes() []string
```

Every constructor of a gateway error and every transport mapping takes its code from a constant.
No code literal is left in `server.go`, `profile.go`, `pgo.go`, `pgo_policy.go`, `pgo_collections.go`, or `listing.go`,
and the two mappings — `storeError` at `pgo.go:154` and `discoveryError` at `server.go:564` —
become exhaustive switches over their own closed inputs.
`consoleCode` at `console.go:42` and the proxy outcome mapping keep their audit-only values,
which are not registry constants and are documented as such beside the registry.
`internal/ui` writes two envelope codes of its own through `httpapi.ErrorEnvelope`
(`internal/ui/ui.go:90`, `:106`, `:112`);
the constants for those two are exported and it uses them,
so the registry holds every code the gateway writes rather than every code this package writes.

- [x] **Add the compile seams**

Declare `declaration`, `declarations`, `match`, the constants, and `EnvelopeCodes`,
with `match` delegating to `parseRoute` and the constants unused, so the tree builds before anything moves.

- [x] **Write the table and registry tests**

`routes_test.go` restates the route-table row of *Layers*:

| Case | Expect |
|---|---|
| one request per declaration, with a method it accepts | the handler that route had before this task answers |
| one request per declaration, with a method it does not | `405`, and `Allow` equal to that declaration's methods, joined as the router joins them |
| the same, for the three console declarations | the console's own `405`, whose `Allow` equals the declaration's methods |
| a path matched by no declaration | `404 route_unknown` |
| `/v1/namespaces/BAD/services/x/targets`, and an `{id}` outside the identifier grammar | `404 route_unknown`, not `400` |
| every console declaration with readiness false | the file, the shell, or the `302` — never `503 not_ready` |
| an `/auth/` route with readiness false | `503 not_ready` from `serveAuthRoute`, after its own method check |
| every `/v1` declaration with readiness false | `503 not_ready`, as today |
| a scan of the package for a second path dispatch | `match` is the only one: no `strings.HasPrefix` on a request path outside it |
| every declaration | its template's parameter names are the ones the handler reads |
| the table | no two declarations share a template, and every kind the switch statements name has one |

| Case | Expect |
|---|---|
| `EnvelopeCodes()` | equals the codes of *Errors* and of [`pgo.md`](../specs/pgo.md) *Errors*, written out in the test |
| every code a handler writes in the whole `internal/httpapi` suite | is in the registry |
| the console's `404` for an unknown asset, and its `405` | the code in each body is a registry constant, read out of the encoded envelope |
| a scan of `internal/ui/ui.go` | neither code appears as a string literal |
| `storeError` over each of its inputs, and `discoveryError` over each of its sentinels | a registry constant, one row per input |
| the audit-only outcomes | absent from the registry |

The last registry row is what makes the document's check meaningful later:
a code the gateway can write and the registry does not hold would pass a document comparison and still ship.

- [x] **Run the tests and watch them fail**

- [x] **Move the dispatch and the codes**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/ui/
git commit -m "refactor(httpapi): one table for every route"
```

---

## The two write routes require a JSON media type

**Files:**
- Modify: `internal/httpapi/server.go`, `internal/httpapi/pgo_collections.go`,
  `internal/httpapi/server_test.go`, `internal/httpapi/pgo_collections_test.go`

[`docs/specs/pgo.md`](../specs/pgo.md) *Request media type* defines the rule,
and *HTTP API* places it: immediately after the method step and before readiness,
which is the order all four accepted designs now state identically
(*Request algorithm*, [`docs/specs/auth.md`](../specs/auth.md) *Request algorithm*).

**The step.**
After the method check in `ServeHTTP` and before `s.ready()` at `internal/httpapi/server.go:414`:
when the matched declaration is `POST .../collections` or `POST /v1/collections/{id}/cancel`
and the method is `POST`, the request must declare a JSON media type.

```go
// mediaTypeFault reports why a write route refuses this request's Content-Type,
// or nil when the header is one it accepts.
// The essence must be application/json; every parameter mime.ParseMediaType
// returns is accepted and ignored, charset among them.
func mediaTypeFault(h http.Header) *requestError
```

An absent header is `400 invalid_parameter` with a `header_required` item naming `Content-Type`;
the same status with `header_malformed` covers a repeated header,
one that does not parse,
and one whose essence is anything else.
`PUT /pgo` is not covered: the rule names the two `POST` routes, and no form can issue a `PUT`.

**Why the position matters, stated as a test rather than as a comment.**
The refusal is the answer with `pgo.enabled` false, behind the replay barrier,
with the caches unsynced, with no credential under `basic`, and for a realm-denied Service,
and the fake store records no call in any of them.

- [x] **Write the media-type tests**

| `Content-Type` on a `POST` to either write route | Answer |
|---|---|
| absent | `400 invalid_parameter`, one `header_required` item naming `Content-Type` |
| `text/plain`, `application/x-www-form-urlencoded`, `multipart/form-data` | `400 invalid_parameter`, `header_malformed`, one row each |
| repeated, and one that does not parse | the same, one row each |
| `application/json` | accepted |
| `application/json; charset=utf-8`, and `application/json; profile=x` | accepted |

| Ordering case | Expect |
|---|---|
| `pgo.enabled` false | the media-type refusal, not `501 pgo_disabled` |
| the replay barrier not cleared | the refusal, not `503 pgo_unavailable` |
| readiness false | the refusal, not `503 not_ready` |
| no credential under `basic` | the refusal, not `401 unauthenticated` |
| a realm that denies the Service | the refusal, not `403 realm_denied` |
| every row above | the fake store records no call |
| a method the route does not accept, with no `Content-Type` | `405`, because the method step is still first |
| `PUT /pgo` with no `Content-Type` | unchanged from today |

- [x] **Run the tests and watch them fail**

- [x] **Implement the step**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "feat(httpapi): require json on the write routes"
```

---

## An artifact outlives the interval that produced it

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`,
  `internal/config/testdata/pgo-full.yaml`,
  `internal/pgo/policy.go`, `internal/pgo/policy_test.go`,
  `docs/configuration.md`, `CHANGELOG.md`

[`docs/specs/pgo.md`](../specs/pgo.md) *Ceilings* states one rule that judges an effective policy against itself
and names the five places it is validated;
*Configuration* gives `pgo.defaults.artifact.retention` the default `24h`
and the validation `>= pgo.defaults.schedule.every`.
*The latest completed Collection* is the endpoint that depends on it:
without the rule a Service can collect hourly, retain for a minute, and answer `404` for most of every hour.

**The default moves.**
`internal/config/config.go:402` reads `default:"2h"` and becomes `24h`.
It stays inside every neighbouring bound at the shipped values:
`pgo.limits.maxRetention` defaults to `24h` (`config.go:359`),
`pgo.jobRetention` defaults to `168h` and must be at least `maxRetention + 1h` (`config.go:818`),
and `pgo.defaults.schedule.every` defaults to `6h` (`config.go:381`).

**The rule lands once and covers four places.**
`pgo.Validate` at `internal/pgo/policy.go:328` is what `PUT /pgo`, `POST /collections`,
the scheduler's evaluation, and the worker's claim and reclaim all call,
so adding the rule there gives four of the five places named in *Ceilings* in one change.
It reports on `/artifact/retention` — the field the writer is asked to raise —
with `schedule.every` named in the message beside it,
`Ceiling` naming `schedule.every`, and `Code` the fifth value, `retention_under_interval`.
The fifth place, configuration-default validation, is `internal/config`:
`pgo.defaults.artifact.retention >= pgo.defaults.schedule.every`,
checked beside the `maxRetention` rule at `config.go:1232`,
**whether or not `pgo.enabled` is true**,
the way every other cross-field rule inside the `pgo` block already runs.
`pgo.limits.maxRetention >= pgo.limits.maxEvery` stays exactly as it is:
it guarantees a long enough retention is available, which is a different claim.

**What in the tree the rule would break, checked rather than assumed.**
`grep -rn retention internal/config/testdata deploy/ test/e2e/` finds three places and no chart value:

| Place | State | Action |
|---|---|---|
| `internal/config/testdata/pgo-full.yaml:39` | `retention: 2h` beside `every: 6h`, which the new rule refuses | set it to `24h`, the value the spec's own complete example carries |
| `internal/config/testdata/pgo-zero-retention.yaml:9` | `retention: 0s`, expected to fail on `must be at least 1m` | unchanged: the `min=1m` field rule runs before any cross-field rule, so the message it asserts stays |
| `test/e2e/harness_test.go:872` | `every: 1m` with no retention override | unchanged: the new default of `24h` satisfies the rule with room |
| `deploy/` | names `retention` nowhere; the chart does not render `pgo.defaults` | nothing to change, which is why no chart file appears in this task |

`internal/config/config_test.go:666` asserts `2h` and moves with the default.

- [x] **Write the rule tests**

`internal/pgo/policy_test.go`, restating the retention rows of [`docs/specs/pgo.md`](../specs/pgo.md) *Unit*:

| Effective policy | Expect |
|---|---|
| `retention` below `every` | one violation on `artifact.retention`, code `retention_under_interval`, message naming both values |
| `retention` equal to `every` | no violation |
| `retention` above `every` | no violation |
| an override setting only `schedule.every` above the default retention | the violation, because the rule reads the effective policy and not the override |
| an override setting only `artifact.retention` below the default `every` | the violation |
| a policy violating the rule and a ceiling | both violations, the ceiling's in its existing order and this one on its own field |

`internal/config/config_test.go`:

| Configuration | Expect |
|---|---|
| the shipped defaults | `artifact.retention` is `24h` and loading succeeds |
| `defaults.artifact.retention: 1h` with `defaults.schedule.every: 6h`, `pgo.enabled: true` | rejected, naming both keys |
| the same with `pgo.enabled: false` | rejected identically |
| `retention: 0s` | the existing `must be at least 1m` message, unchanged |
| `retention` above `maxRetention` | the existing message, unchanged |
| `testdata/pgo-full.yaml` | loads and validates as written |

`internal/httpapi` and `internal/pgo` gain one case each for the two request paths and the claim path:
`PUT /pgo` and `POST /collections` whose effective policy breaks the rule answer `400 limit_exceeded`,
carrying the `retention_under_interval` item on `/artifact/retention`;
a stored override that breaks it makes the Service ineligible for scheduling
and appears in `GET /pgo`'s `violations` with that `code`;
a worker claiming a snapshot that breaks it fails the Collection `limit_exceeded`
**before any local slot is reserved**, the way a ceiling violation already does,
and the test fails when the rule is checked only at write time.

- [x] **Run the tests and watch them fail**

- [x] **Move the default and add the rule**

- [x] **Update the two documents a client reads**

| File | Change |
|---|---|
| `docs/configuration.md:438` | the `artifact.retention` row: default `24h`, and the bound gains "at least the effective `schedule.every`" |
| `docs/configuration.md:658` | the example's `retention: 2h` becomes `24h`, so the example still validates |
| `CHANGELOG.md` | under `## [Unreleased]`, a `### Changed` entry: the default moves from `2h` to `24h`, and an effective policy must retain its artifact for at least one interval, refused at `PUT /pgo` and `POST /collections` and reported in `violations` otherwise |

The changelog entry says plainly what an operator must do:
a file that pins a retention under its interval no longer starts, and the message names both keys.

- [x] **Validate and commit**

```bash
semlf check docs/configuration.md CHANGELOG.md
mise exec -- go test -race ./internal/config/ ./internal/pgo/ ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/config/ internal/pgo/ internal/httpapi/ docs/configuration.md CHANGELOG.md
git commit -m "feat(pgo): keep an artifact for a whole interval"
```

---

## One idempotency key binds one Collection

**Files:**
- Modify: `internal/pgo/record.go`, `internal/pgo/caches.go`, `internal/pgo/publisher.go`,
  `internal/pgo/runtime.go`, `internal/pgo/sweeper.go`,
  `internal/pgo/record_test.go`, `internal/pgo/publisher_test.go`, `internal/pgo/sweeper_test.go`,
  `internal/pgo/fixtures_test.go`

[`docs/specs/pgo.md`](../specs/pgo.md) *Create a Collection* defines the receipt, its scope, and its lifetime;
*Atomicity primitives* names its one primitive and its three writers;
*Paths that touch each key* names its four paths and states that no watch is opened on the prefix;
*Algorithm* gives the publication order; *Record* adds two fields; *Sweeper* gives the two removal rules.
This task lands the store half; the handler that reads it is the next task.

**Produces:**

```go
// ReceiptKey is idem. followed by the first 32 hexadecimal characters of the
// SHA-256 of the scope, which is each of the principal, the namespace, the
// service, and the client's key written as its byte length, a colon, and its
// bytes, so no value can be read as part of the one beside it.
func ReceiptKey(principal, namespace, service, key string) string

// Receipt is what one idempotency key created.
type Receipt struct {
    ID           string    `json:"id"`
    SnapshotHash string    `json:"snapshotHash"`
    CreatedAt    time.Time `json:"createdAt"`
}

// SnapshotHash is SHA-256, as 64 lowercase hexadecimal characters, of the
// canonical encoding of a policy snapshot: json.Marshal of the Policy value,
// whose field order is the struct's. It is never computed from request bytes,
// so whitespace and field order in a body decide nothing and a snapshot that
// moved under a stored override or the operator defaults produces a different
// hash from identical JSON.
func SnapshotHash(p Policy) string
```

`Record` at `internal/pgo/record.go:72` gains `IdempotencyKey` and `SnapshotHash`,
both inside the size arithmetic *Record* already states
(the 128-byte key and the 64-character hash are counted there).
**Only `IdempotencyKey` is empty for a scheduled Collection.**
*Record* defines `snapshotHash` as the hash of the policy the Collection runs with, whatever created it,
and a scheduled record carries it like every other:
the field is what a reader compares, and a Collection with no hash could not be compared with anything.
`PublishInput` at `publisher.go:35-50` gains **one** field, `IdempotencyKey`;
the principal the receipt is scoped to is the existing `CreatedBy` at `:49`,
which already holds the requesting principal for an api Collection and `createdBySchedule` for a scheduled one.
The scheduler passes the new field empty,
which is what makes "the scheduler writes no receipt" a property of the call rather than a branch to remember.

**The publication order, which is where the guarantee comes from.**
`Publisher.Publish` at `publisher.go:237` performs its three writes today.
The receipt becomes a fourth, written **after** the active key is won and **before** the
`initializing -> pending` update that makes the record claimable
([`docs/specs/pgo.md`](../specs/pgo.md) *Algorithm*):

1. `Create` `job.<id>` as `initializing`, carrying `snapshotHash`, the key, and `createdBy`;
2. `Create` `active.<ns>.<svc>`; a loss deletes the creator's own record and reports busy, as today, and writes no receipt;
3. when a key was given, `Create` `idem.<hash>`;
   `ErrUnavailable` is retried once behind the same generation;
   a `Create` that finds the key already holding this identifier counts as success;
   a `Create` that meets a receipt whose record a fresh `Get` shows absent deletes it at the revision that `Get` returned,
   then creates again;
   any other failure **withdraws** — delete the `initializing` record at the revision held,
   delete the active key at the revision its `Create` returned, and report unavailable;
4. `Update` the record to `pending`.

A keyed record therefore never becomes claimable without its receipt,
and the only records a reader can find without one are an `initializing` record
and a `failed` one whose reason is `not_published` — neither of which ever ran.

**The sweeper.**
`internal/pgo/sweeper.go` gains two rules ([`docs/specs/pgo.md`](../specs/pgo.md) *Sweeper*):
the record-retention rule deletes `idem.<hash>` immediately after the record it names,
computing the key from the record's own `createdBy`, namespace, service, and `idempotencyKey`;
and a reconciliation that runs on **one sweep in sixty** issues `Keys("idem.")`,
reads each name, and deletes a receipt whose `createdAt + jobRetention + skewMargin` has passed
and whose record a fresh `Get` shows absent, at the revision that read returned.
Nothing else touches the prefix, no watch is opened on it, and no cache indexes it.

**Both rules delete a receipt only after reading it and finding it names the Collection they are deleting for.**
A revision guard alone is not enough on the record-retention path, and the race is not exotic:
the sweeper deletes record A;
a request carrying A's key finds the receipt stale, deletes it, and publishes Collection B under the same key;
the sweeper then reads `idem.<hash>`, finds B's receipt, and deletes it at the revision it just read.
B is live with no receipt, and the guarantee that binds a key for a record's whole life is gone —
a retry would create a second Collection for a key that already has one.
The rule is therefore: read the receipt, unmarshal it,
and delete at that revision **only when its `id` equals the id of the record being deleted**.
The reconciliation path needs no separate rule for this:
it deletes only a receipt whose named record a fresh `Get` shows absent,
and a successor's receipt names a record that exists.
[`docs/specs/pgo.md`](../specs/pgo.md) *Atomicity primitives* already states the shape —
each of the three writers "act only on a receipt whose record a fresh `Get` shows absent" —
and this is that rule applied to the writer that starts from the record rather than from the key.

- [x] **Add the compile seams**

Declare `ReceiptKey`, `Receipt`, `SnapshotHash`, the two record fields,
and the **one** new `PublishInput` field, `IdempotencyKey` —
the principal is `CreatedBy` at `internal/pgo/publisher.go:49` and a second field for it would not compile.
Give `Publisher` a no-op receipt step, and leave the existing constructions of `PublishInput`
(`internal/pgo/scheduler.go` and `internal/httpapi/pgo_collections.go`) to default the new field to empty,
so the tree builds before any assertion is written.

- [x] **Write the receipt tests**

`internal/pgo`, over the in-process server, restating the receipt rows of *Unit*:

| Case | Expect |
|---|---|
| `ReceiptKey` output | `idem.` and exactly 32 hexadecimal characters |
| two scopes differing only in where a `\|` falls inside a principal | different keys |
| the same four fields | the same key on every call, in this process and after a restart of the fixture |
| `SnapshotHash` over one policy encoded two ways | one value; the hash reads the struct and never bytes |
| `SnapshotHash` over two policies differing in one field | different values |

| Publication | Expect |
|---|---|
| a keyed publication, with a barrier between the active create and the receipt create | the receipt is in the bucket before the `pending` update, proven from the barrier |
| a keyed publication killed after its record and before its receipt | a record no key resolves, and the scan later fails it `not_published` |
| a receipt `Create` returning `ErrUnavailable` once | retried once, and the publication completes |
| a receipt `Create` failing twice | the record and the active key are gone and the outcome is unavailable |
| a receipt `Create` meeting a receipt naming this same identifier | success, no second write |
| a receipt `Create` meeting a receipt whose record is absent | that receipt is deleted at the revision read, and a new one is created |
| a keyed publication whose active create loses | no receipt is written |
| a scheduled publication | no receipt, an empty `idempotencyKey`, and a `snapshotHash` over its own policy |
| every publication above | the record carries `snapshotHash`; a keyed one also carries the key, and `createdBy` is the principal the receipt was scoped to |
| a publication that died between its receipt and its `pending` update | the receipt is in the bucket naming an `initializing` record, which the scan then fails `not_published` |

| Sweep | Expect |
|---|---|
| a record past `jobRetention` with a key | the record is deleted first, then its receipt |
| the same, whose receipt delete returns `ErrUnavailable` | the record is gone and the receipt survives to the reconciliation |
| a barrier that replaces the receipt between the record's deletion and the sweeper's receipt read, so it names a live successor | the successor's receipt survives, and the test fails when the rule deletes at the revision it read without comparing the id |
| the same barrier, with the replacement naming the same id | the receipt is deleted, so the id check refuses only a successor and not a rewrite |
| a receipt whose record is still present, on the reconciliation pass | left alone |
| a receipt younger than `jobRetention + skewMargin` | left alone, with no record lookup |
| a receipt past it whose record a fresh `Get` shows absent | deleted at the revision read |
| sixty sweeps | exactly one issues `Keys("idem.")`; the other fifty-nine read no `idem.` key |
| a receipt key | matched by no `job.*`, `active.*`, or `schedule.*` rule, and counted by no `cachedLive` figure |
| a scan of the package's cache types | no cache indexes an idempotency key |

- [x] **Run the tests and watch them fail**

- [x] **Implement the receipt, the record fields, and the sweep rules**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/pgo/ ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/pgo/ internal/httpapi/
git commit -m "feat(pgo): bind an idempotency key to a record"
```

---

## A create can be retried

**Files:**
- Modify: `internal/httpapi/pgo_collections.go`, `internal/httpapi/server.go`,
  `internal/httpapi/codes.go`, `internal/pgo/runtime.go`,
  `internal/httpapi/pgo_collections_test.go`, `internal/pgo/fixtures_test.go`

[`docs/specs/pgo.md`](../specs/pgo.md) *Create a Collection* defines the header's grammar,
where the lookup sits in the handler, what a replay answers, and what is not a replay.
*Errors* adds `409 idempotency_mismatch`, answered by this route alone and carrying no `details`.

**Produces:**

```go
// ReadReceipt reads idem.<hash> authoritatively; no cache stands in front of
// it, because a receipt decides whether a Collection is created.
// It is natskv.ErrKeyNotFound for a key with no history.
func (s *Session) ReadReceipt(ctx context.Context, key string) (Receipt, uint64, error)

// DeleteReceipt removes a stale receipt at the revision its read returned.
func (s *Session) DeleteReceipt(ctx context.Context, key string, revision uint64) error
```

**Where the lookup sits.**
`serveCollectionCreate` at `internal/httpapi/pgo_collections.go:120` decodes the body
and builds the snapshot at `:139`.
The receipt lookup goes **immediately after that** and before the ceiling refusal at `:141`,
so it precedes the ceiling refusal, the token bucket, the advisory target resolution, and the reservation.
A replay creates nothing, so none of the bounds those steps hold apply to it,
and a ceiling that moved between two requests cannot refuse a Collection that is already running.
The header's own grammar is checked earlier, at the parameter step,
so a key the gateway cannot read is refused whatever the body carries.

**What the lookup answers.**

| Read | Answer |
|---|---|
| no receipt | create, as a request with no history does |
| a receipt whose record exists and whose `snapshotHash` equals this request's | `200`, `{id, state}` with the state read from the record, and the same `Location` |
| a receipt whose record exists and whose hash differs | `409 idempotency_mismatch`, nothing written |
| a receipt whose record is `failed` with reason `not_published` | stale: delete the receipt at the revision read, then create, because that Collection never became claimable and never ran |
| a receipt whose record a fresh `Get` shows absent | delete the receipt at the revision read, then create; a lost delete re-reads once |
| `ErrUnavailable` on either read | `503 pgo_unavailable` |

The `not_published` row is the crash window the publication order opens:
the receipt lands, the creator dies before the `pending` update, and the scan fails the record it left.
The first task writes that row into [`docs/specs/pgo.md`](../specs/pgo.md) *Create a Collection*,
which today answers it one way in its lookup rule and the other way in *Unit*.

**The thin replay is the whole answer.**
`{id, state}` and nothing else, encoded through the same `acceptedBody` the `202` uses at `:204`,
with `Location` set the same way and the status `200` rather than `202`.
`POST .../collections` is a `pgo.collect` route and `GET /v1/collections/{id}` is a `pgo.read` one,
and the two flags are independent,
so a full record here would hand a collect-only principal the manifest and the placement its realm denies.

**The two concurrency cases are two barriers, not one race run twice.**
[`docs/specs/pgo.md`](../specs/pgo.md) *Unit* pins both outcomes,
and which one a given run produces depends on where the loser is released:
before the winner's receipt exists it can only be `429 collection_in_progress`,
and after the winner's `pending` update it can only be `200`.
Each test therefore names its release point rather than starting two requests and asserting a pair;
a test that released the loser at an unconstrained moment would be green for either answer and prove neither,
so the plan states that third row as the disjunction it is, with the retry as the assertion that carries weight.

**How a loser resolves.**
The publication's active create can still lose.
The loser deletes its own `initializing` record as it does today,
re-reads `idem.<hash>` once, and answers from it under the rules above.
With no receipt it reads the active key it lost to and the record that key names, once each,
and answers `429 collection_in_progress` with `Retry-After: 1` whatever that record carries:
an identifier handed out from an `initializing` record could name a Collection the winner deletes a moment later.
Those two reads cover the publication window and never answer `200`.

- [x] **Add the compile seams**

Declare the two session methods, the `idempotency_mismatch` registry constant,
and the header parse, then build and vet.

- [x] **Write the create tests**

`internal/httpapi`, restating the `Idempotency-Key` rows of *Unit*:

| Header | Answer |
|---|---|
| one byte, and 128 bytes | accepted |
| empty, 129 bytes, a byte outside `[A-Za-z0-9._-]`, and the header sent twice | `400 invalid_parameter` with a `header_malformed` item naming it, nothing written |
| absent | today's behavior, and no receipt in the bucket |

| Case | Answer |
|---|---|
| a second `POST` carrying the first's key | `200`, `{id, state}` and the same `Location`, the state read from the record |
| the same, for an `initializing`, a `running`, and a terminal record whose active key is gone | `200` each time |
| the replay body, asserted against the encoded bytes | exactly two fields, and no record field |
| a principal holding `pgo.collect` and not `pgo.read` | that same `200`, while `GET /v1/collections/{id}` for the identifier is `404 collection_not_found` |
| a replay whose realm no longer admits the Service | `403 realm_denied`, before the receipt is read |
| the same key from another principal, and the same key on another Service | a new Collection each, and two distinct receipts in the bucket |
| the same key with a body whose snapshot hash differs | `409 idempotency_mismatch`, nothing written, no `details` key |
| the same key with identical JSON after the stored override changed | `409 idempotency_mismatch` |
| the same key with identical JSON after the operator defaults moved | `409 idempotency_mismatch` |
| a different key while a Collection is live | `429 collection_in_progress` |
| two concurrent keyed requests, the loser released only after the winner's `pending` update returns | one `202` and one `200` naming one identifier; one `job.*`, one `active.*`, one `idem.*`, and no `initializing` record left |
| the same pair with the winner held between its active create and its receipt create | one `202` and one `429 collection_in_progress` with `Retry-After: 1`; the loser's retry after the barrier is `200` |
| the same pair with the loser released at an unconstrained moment | either `200` or `429 collection_in_progress` with `Retry-After: 1`, and a retry after the winner finishes is `200` in both branches |
| a keyed record the scan failed `not_published`, retried with its key | no replay: the receipt is replaced and a new Collection is created |
| the same, with the original receipt still naming the failed record | exactly one `idem.*` key afterwards, naming the new Collection |
| a replay with every `job.*` cache in the replica frozen since the first create | `200` with the original identifier |
| a replay whose receipt names a record the sweeper deleted | the receipt is replaced and a new Collection is created |
| a record written without a receipt | never replayed |
| a scheduled Collection | no key, no receipt, no replay |
| a keyed request refused `429 rate_limited` or `429 capacity_exhausted` on its first attempt | no receipt written, so a retry creates |

- [x] **Run the tests and watch them fail**

- [x] **Implement the lookup, the replay, and the mismatch**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/pgo/
git commit -m "feat(httpapi): replay an idempotent create"
```

---

## A client can wait for a Collection to move

**Files:**
- Modify: `internal/pgo/caches.go`, `internal/pgo/runtime.go`, `internal/pgo/caches_test.go`,
  `internal/httpapi/pgo_collections.go`, `internal/httpapi/server.go`, `internal/httpapi/audit.go`,
  `internal/httpapi/pgo_collections_test.go`, `internal/httpapi/fixtures_test.go`,
  `cmd/profgate/serve.go`, `cmd/profgate/serve_test.go`

[`docs/specs/pgo.md`](../specs/pgo.md) *Get a Collection* defines the parameter, the six ways a wait ends,
the pulse rules, the generation broadcast, `X-Wait-Elapsed`, and what a wait holds.
*Startup and shutdown* states the drain's side of it.
*Logging* adds the `wait` audit field.

**Produces:**

```go
// Subscribe registers a channel pulsed for every job.<id> entry applied for id.
// The pulse is a hint and never an answer: the handler re-reads the record and
// decides from that read alone, so a pulse carries nothing and a full buffer
// drops one rather than blocking apply.
// The returned function removes the registration and is called when the request ends.
func (s *Session) Subscribe(id string) (<-chan struct{}, func())

// GenerationMoved returns the channel this session captured when it was taken:
// the one closed when the generation its view is bound to is left behind.
// It is a field of the session and not a lookup, so a generation that moves
// between a handler's read and its select still closes the channel that handler
// holds; a lookup would hand back the replacement and lose the signal.
func (s *Session) GenerationMoved() <-chan struct{}
```

`Caches` at `internal/pgo/caches.go:142` gains a map of per-record subscribers beside `jobPulse`,
fanned out inside `applyJob`'s existing call in `apply` at `:272`:
each send is non-blocking, so a slow subscriber never holds `c.mu` and never blocks another.

**The generation broadcast belongs to `Runtime`, and a session captures it rather than looking it up.**
`Runtime` holds one channel per generation, closed and replaced when the connection drops.
`Session()` at `internal/pgo/runtime.go:81-101` takes the generation, checks the barrier, and takes the view;
it captures that generation's channel in the same step and stores it on the `Session`.
`GenerationMoved()` returns the captured field.
Reading the current channel at select time instead would lose a wake-up:
the channel is the replacement when the generation moved between the handler's read and its request for it,
and the close that already happened is signalled on a channel nobody holds,
so the request parks to its deadline over an outage it should have refused.
The channel is fed by `natskv.Options.OnConnectionChange`,
which `cmd/profgate` already wires to the connection gauge
and which fires immediately after `bumpGeneration` has moved the generation
(`internal/natskv/client.go:124-132`), so the seam gains no method.

**The two routes that take parameters need a parameter step, because no PGO route has one today.**
Every request carrying any query string at all is refused today, with one message and no item,
by `servePGOService` at `internal/httpapi/pgo.go:64` and `servePGOCollection` at `:91`.
`GET /v1/collections/{id}` and `GET .../collections` each gain a parameter step of their own instead:
the names their table admits, validated in name order.
Each of these is `400 invalid_parameter` carrying the item *Errors* names:
a name the table does not hold, a repeated one, and an empty value.
The other five PGO routes keep the blanket refusal exactly as it is,
which is what [`docs/specs/pgo.md`](../specs/pgo.md) *HTTP API* and *Policy* still describe;
their refusal gains the `unknown_parameter` item the earlier task gives every other route.

**The handler.**
`GET /v1/collections/{id}` takes `wait`, a duration from `1s` to `60s`, and no other parameter.
Each of these is `400 invalid_parameter` with the item it earns:
a value that is not a duration, one at or below zero, one above `60s`, an empty one, and a repeated one;
a value above the grammar is **refused, never clamped**.
The route reads its record before the realm step already
(`internal/httpapi/server.go:464`, `internal/httpapi/pgo.go:87`), and that read is the wait's baseline.
The handler then registers, issues one authoritative `Get`, and answers when that read is terminal
or shows a `state` other than the baseline; otherwise it parks and selects on:

| Event | Answer |
|---|---|
| a pulse | one authoritative `Get`; answer when the `state` differs from the baseline, `404 collection_not_found` when the record is gone |
| the wait elapsing | one final authoritative `Get`, and **that** read is the answer |
| the client disconnecting | nothing; the audit code is `client_gone` |
| the drain signal | the record as last read, at once |
| the generation broadcast | `503 pgo_unavailable` |

**The deadline reads, for the same reason a pulse does.**
Answering the deadline from the record last read makes a dropped pulse a wrong answer rather than a delay:
a terminal transition whose pulse the full buffer dropped would be reported as the state before it,
and so would one that becomes ready in the same instant as the timer.
The first task repairs the sentence in *Get a Collection* that says otherwise,
so the ending list and the pulse rules two paragraphs below it agree.
A final `Get` that returns `ErrUnavailable` is `503 pgo_unavailable`, as any other store failure inside the wait is,
and one that finds the record gone is `404 collection_not_found`.
The drain ending keeps answering from the last read:
a replica whose listener is closing is being taken out of service,
and a store call there is exactly the wait the drain bound exists to end.

Only a change of `state` ends a wait: an owner writes `progress` with every renewal,
so a wait that answered on any write would answer every twenty seconds with a record that had not moved.
Every answer to an accepted `wait` carries `X-Wait-Elapsed` in decimal seconds with millisecond precision,
`0.000` included; an answer to a request that carried none, or whose `wait` was refused, carries none.
`auditRecord` gains `wait`, written only for a request whose `wait` the gateway accepted,
carrying the duration asked for, the way `explain` is written.
No metrics label is added: a long poll's duration lands in `profgate_request_duration_seconds` and nowhere else.

**The wait's deadline runs on the PGO clock, and the fake has to grow a timer.**
The handler takes its deadline from `Clock.NewTimer` (`internal/pgo/clock.go:6-11`),
so a test drives it without sleeping.
`fakePGOClock` in `internal/httpapi/fixtures_test.go:1154-1179` cannot do that today:
its `NewTimer` and `NewTicker` panic with "no PGO route uses a timer", which is about to stop being true.
Giving it a working timer is part of this task and not a discovery during it:
`internal/pgo/fixtures_test.go:916-1030` already holds a `fakeClock` and a `fakeTimer` that fire on `advance`,
and the `internal/httpapi` fake gains the same shape.
Without it every wait case would either sleep or be untestable, and the deadline rows below could not be written.

**The drain signal.**
`Deps` gains `Drain <-chan struct{}`; nil means a channel that never closes, which is what a test wants by default.
`cmd/profgate/serve.go` closes it where `draining.Store(true)` runs at `serve.go:352` —
the moment `/readyz` turns 503 and before `server.drainDelay` at `:368` —
so a parked request cannot outlast the drain window the deployment sized.
The drain bound itself does not move.

- [x] **Add the compile seams**

Declare `Subscribe`, `GenerationMoved`, `Deps.Drain`, the `wait` parameter parse, and the audit field.

- [x] **Write the wait tests**

`internal/pgo/caches_test.go`:

| Case | Expect |
|---|---|
| a subscriber for one record | a pulse for every entry applied for that record, and none for another record's |
| a subscriber that never reads | blocks no `apply`, proven by a second subscriber that still receives |
| a subscriber whose buffer is full | the pulse is dropped, and the next entry pulses it again |
| a registration removed | no further pulse, and no goroutine or map entry left |
| a connection-generation change | the broadcast reaches every session that captured that generation's channel, once |
| a session taken after the change | it captures the replacement channel, which the earlier change does not close |

`internal/httpapi`, restating the `wait` rows of *Unit*:

| Query | Answer |
|---|---|
| no `wait` | today's answer, no `X-Wait-Elapsed`, and no `wait` audit field |
| `?bogus=1` | `400 invalid_parameter` with an `unknown_parameter` item naming it, and no subscription |
| any query parameter on the five PGO routes that take none | `400 invalid_parameter`, refused as today, now with its item |
| `wait=0`, `wait=-1s`, `wait=abc`, `wait=61s`, `wait=120s`, empty, repeated | `400 invalid_parameter` with the item each earns, no subscription registered, no `X-Wait-Elapsed` |
| `wait=5s` on a terminal record | at once, with `X-Wait-Elapsed: 0.000` |
| a record moving `pending` to `running` between the registration and the first read | answers at once, which fails when the handler reads before it registers |
| a `pending` record that becomes `running` | the record read after the pulse, never the cached entry |
| a subscriber whose buffer was filled before the terminal write | still answers terminal, because the read decides |
| a renewal writing only `progress` | does not answer, proven by a wait outliving two renewals |
| a record deleted mid-wait | `404 collection_not_found` |
| a wait that expires with no transition | the record as the final read returns it, with an elapsed value at least the duration asked for |
| a terminal transition whose pulse is dropped, the buffer filled first, then the deadline | the terminal record, because the deadline reads; the test fails when the deadline answers from the last read |
| a terminal transition committed and the timer fired before the handler selects | the terminal record, whichever arm the select takes |
| a final read that returns `ErrUnavailable` | `503 pgo_unavailable` |
| a record deleted just before the deadline | `404 collection_not_found` |
| a client that disconnects mid-wait | audited `client_gone`, and no subscription left after the handler returns |
| the generation moving mid-wait | `503 pgo_unavailable`, driven by the broadcast, failing when the handler reads `Generation()` once |
| the generation moving between the handler's authoritative read and its first select | `503 pgo_unavailable`, because the session captured its own generation's channel; the test fails when the handler looks the channel up at select time |
| the drain signal | every waiting request answers at once with the record it last read |
| fifty concurrent waits on one record | one applied entry wakes all fifty, one `Get` per woken request, and no watch beyond the caches' own |
| an accepted `wait` | one audit record carrying `wait` with the duration asked for, and the recorder sees no label from it |

`cmd/profgate/serve_test.go` asserts the wiring:
`serve` closes the drain signal when `/readyz` turns 503 and before `server.drainDelay`,
and a request parked in `wait=` answers at that moment rather than at its own deadline.

- [x] **Run the tests and watch them fail**

- [x] **Implement the subscriptions, the broadcast, the handler, and the signal**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/pgo/ ./internal/httpapi/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/pgo/ internal/httpapi/ cmd/profgate/
git commit -m "feat(httpapi): wait for a collection to move"
```

---

## A build asks for the latest profile

**Files:**
- Modify: `internal/httpapi/routes.go`, `internal/httpapi/server.go`, `internal/httpapi/pgo_collections.go`,
  `internal/pgo/runtime.go`, `internal/pgo/caches.go`,
  `internal/httpapi/pgo_collections_test.go`, `internal/httpapi/routes_test.go`

[`docs/specs/pgo.md`](../specs/pgo.md) *The latest completed Collection* defines the two routes,
the four-step selection both run, and why only a `completed` record with its bytes is the latest one.
*HTTP API* gives them `GET` and `pgo.read`, and says `latest` is a path segment and never an identifier.

**Two declarations join the table:**

| Template | Methods | Realm flag |
|---|---|---|
| `/v1/namespaces/{namespace}/services/{service}/collections/latest` | `GET` | `pgo.read` |
| `/v1/namespaces/{namespace}/services/{service}/collections/latest/profile` | `GET` | `pgo.read` |

They are Service-scoped, so the realm is evaluated on namespace, Service, and `pgo.read` before anything is read,
and the identifier grammar of the Collection routes is untouched:
`/v1/collections/latest` stays `404 route_unknown`.
Two metrics values are **not** added: the two routes are counted under `collection` and `collection_profile`
([`docs/specs/pgo.md`](../specs/pgo.md) *Metrics*).

**Produces:**

```go
// LatestCompleted returns the newest Collection of a Service that is completed
// and whose artifact is still in the store, together with that artifact open
// for reading; the caller closes it.
// The watched cache is a candidate filter and never the authority: each
// candidate costs one authoritative Get, a candidate whose fresh read is not
// completed is dropped, and one whose object cannot be opened is flipped to
// expired at the revision that read returned before the walk continues.
// The open is the confirmation, so the reader a caller streams is the one the
// walk confirmed and no second open can find the object gone.
// It is natskv.ErrKeyNotFound when no candidate survives, and ErrUnavailable is
// returned rather than an older Collection.
func (s *Session) LatestCompleted(ctx context.Context, ns, svc string) (StoredRecord, io.ReadCloser, error)
```

Both routes call it, so the record `latest` answers with is the record whose bytes `latest/profile` streams,
and neither can answer `410 artifact_gone` while an intact artifact exists.
`latest` writes the record exactly as `GET /v1/collections/{id}` writes it, through `serveCollectionRead`,
and closes the reader at once — the open cost it the one `Get` a probe would have cost anyway.
`latest/profile` streams **the reader the walk returned**.

**The download helper splits, because confirming and opening cannot be two operations.**
`serveCollectionDownload` at `internal/httpapi/pgo_collections.go:245` resolves the record's object
and opens it at `:276` before streaming.
A `latest/profile` that confirmed an object in the walk and then handed the record to that helper opens it twice.
A sweep between the two opens then answers `410 artifact_gone`,
while the completed Collection behind it still has its bytes —
the answer [`docs/specs/pgo.md`](../specs/pgo.md) *The latest completed Collection* forbids.
The helper therefore splits in two: the resolve-and-open half stays with `GET /v1/collections/{id}/profile`,
and the streaming half — the headers, the flush, the copy, and the three outcome codes —
takes an already-open reader and serves both routes.
An object deleted after the walk opened it is then the truncation the streaming half already classifies
as `artifact_stream_failed`, which is exactly what happens to a download that began before an expiry today.
The audit record names in `collection` the identifier that answered, so a reader sees which Collection a build took.
The candidate order is the listing's: `createdAt` descending, `id` descending on a tie
(`internal/pgo/caches.go:551-557`, which already sorts that way).

- [x] **Add the compile seams**

Add the two declarations and their kinds, `LatestCompleted` returning `ErrKeyNotFound`,
and the dispatch, so every assertion fails on content.

- [x] **Write the latest tests**

| Fixture | Answer |
|---|---|
| a newest `completed` record with a newer `failed`, a newer `running`, and a newer `expired` beside it | that record, byte for byte equal to `GET /v1/collections/{id}` for it |
| a Service with no `completed` record | `404 collection_not_found` |
| a Service with no records at all | the same |
| a Service whose only completed record expired | the same |
| a newest completed record whose object is gone | the completed one behind it, and the newest is `expired` in the bucket afterwards |
| two newest completed records that have both lost their objects | the walk passes both |
| a record the cache shows `completed` and a fresh read shows `expired` | skipped |
| the newest object deleted by a barrier after the walk confirmed it and before the response streams | the bytes the walk opened are streamed to the end, never `410 artifact_gone`, which fails when the profile route opens the object a second time |
| the same, with the object store dropping the open reader mid-stream | the connection is closed and the audit code is `artifact_stream_failed`, as for any other download |
| `latest` over any fixture | the reader the walk opened is closed before the handler returns, asserted by the fake store's open and close counts |
| `ErrUnavailable` during the walk | `503 pgo_unavailable`, never an older Collection |
| both routes over one fixture | the same identifier, compared through `X-Pprof-Collection` |
| `latest/profile` | the bytes and the headers the identifier route streams |
| a denied namespace, and a denied Service | `403 realm_denied` before the cache is read |
| `/v1/collections/latest` | `404 route_unknown` |
| `POST` to either route | `405` with `Allow: GET` from its declaration |
| any answer | the audit record names the identifier that answered, and the metrics row is `collection` or `collection_profile` |
| a walk that discards candidates | one authoritative `Get` per discarded candidate and no more |

- [x] **Run the tests and watch them fail**

- [x] **Implement the walk and the two routes**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/pgo/
git commit -m "feat(httpapi): serve the latest collection"
```

---

## The listing is filtered and paged

**Files:**
- Modify: `internal/httpapi/pgo_collections.go`, `internal/pgo/caches.go`, `internal/pgo/runtime.go`,
  `internal/httpapi/pgo_collections_test.go`, `internal/pgo/caches_test.go`

[`docs/specs/pgo.md`](../specs/pgo.md) *List Collections* defines the parameters, the total order,
the cursor and what it carries, `nextCursor`, and what the order promises.
The listing today reads `Collections` at `internal/pgo/caches.go:525`,
which truncates at 100 with nothing behind it (`maxListCollections` at `:519`).

**The parameters**, validated in name order, each fault `400 invalid_parameter` with the item it earns:

| Parameter | Grammar | Meaning |
|---|---|---|
| `cursor` | a token a previous response carried | continue after the entry it names |
| `limit` | decimal integer 1 to 100; absent means 100 | at most this many entries |
| `origin` | `schedule` or `api` | keep records of that origin |
| `since` | RFC 3339 | keep records whose `createdAt` is at or after it |
| `state` | one of the record states, repeatable | keep records in any of them |

Filters apply first, then the cursor, then `limit`.
`state` is the closed set of *Record*; every other value is refused.
`origin` takes the record's own two values, so a client reading the field and the filter reads one vocabulary.

**The cursor.**
It encodes the `createdAt` and the `id` of the last entry a response carried,
and the `state`, `since`, and `origin` the request that produced it carried.
A page is the entries sorting after that pair by value, never by position,
so a cursor naming a record the sweeper has deleted still works.

**What the order promises is what a live walk can keep.**
[`docs/specs/pgo.md`](../specs/pgo.md) *List Collections* promises
"stable order over the records the listing already held, and no guarantee about records inserted mid-walk".
It does not promise that a record deleted mid-walk is still returned,
and the walk reads a live cache rather than a snapshot, so it could not:
a record the sweeper removed between two pages is gone from the second one.
The tests below say that plainly rather than asking for membership the design does not provide —
no duplicates, head insertions do not disturb the continuation, deletions may take entries away,
and deleting the record the cursor names does not break the next page.
A cursor is `400 invalid_parameter`, with a `malformed_parameter` item naming `cursor`,
when it is presented beside a `state`, `since`, or `origin` set other than the one it was minted under:
a position in one filtered listing is not a position in another,
and reading it against a second listing would skip records silently where a refusal costs one request.
`limit` is not part of the token: it bounds a page and not the order.
**The token is self-contained, versioned, and strictly decoded, and it claims nothing about who minted it.**
Every replica must read a token every other replica wrote:
they share no signing material and no cursor state, and this plan introduces neither,
so "a token this gateway did not mint" is not a property any replica could decide.
What it can decide is that a token decodes:
the encoding carries a version, and a token is `400 invalid_parameter` when it does not decode,
carries a version this build does not know,
or decodes to a value outside its own grammar — a `state` outside the closed set, a `since` that is not a timestamp.
A forged token names a position and a filter set,
which is what any client can reach anyway by sending those filters and paging to that point,
so nothing behind it is disclosed that the listing does not already give.
The encoding lives in one function pair in `internal/httpapi` and nothing else reads it.

**`state` is compared as a set.**
It is the one repeatable filter,
so `state=running&state=pending` must match a token minted under `state=pending&state=running`,
and a repeated identical value must not make two filter sets differ.
The token stores the set in one canonical order and the comparison is set equality, not sequence equality.
A response with more entries carries `nextCursor`; one that reached the end omits the field entirely.

`Caches.Collections` gains the filters and the position and stops truncating at a constant of its own;
`maxListCollections` becomes the `limit` ceiling, applied after filtering and paging.

- [x] **Add the compile seams**

Declare the parameter struct, the cursor encode and decode, and the widened cache method.

- [x] **Write the listing tests**

| Query | Answer |
|---|---|
| `state=` once, and repeated | the records in those states, and nothing else |
| no parameter at all | today's listing, unchanged |
| `?bogus=1` | `400 invalid_parameter` with an `unknown_parameter` item naming it |
| `since=`, `origin=schedule`, `origin=api` | each filters as specified |
| several filters at once | their intersection |
| an unknown name, an empty value, `state=running,pending`, `state=nonsense`, `origin=on-demand`, `since=yesterday`, `limit=0`, `limit=101`, `limit=abc`, a repeated `limit`, a cursor that does not decode | `400 invalid_parameter` with the item each earns, one row apiece |
| a query with several faults | the first fault in name order, both orders of the query terms |
| a cursor minted under one `state`, `since`, or `origin`, presented beside another | `400 invalid_parameter` naming `cursor` |
| the same cursor beside the filters it was minted under | works |
| the same cursor beside a different `limit` | works |
| 250 records including duplicate `createdAt` values | three requests page through with none repeated and none skipped |
| records inserted at the head between two pages | the walk continues where the cursor left it, and no entry the client already saw comes back |
| records deleted from the tail between two pages | those entries may be absent, and every entry that is returned is returned once |
| the record at the cursor's own position deleted between two pages | the next page still returns the entries after it, because the pair is a position and nothing looks that record up |
| a cursor naming a record the sweeper has deleted | the entries after it |
| a token minted against one runtime and presented to another | accepted, which is the cross-replica case a shared-nothing gateway has to hold |
| a token that does not decode, one carrying an unknown version, and one decoding to a `state` outside the closed set | `400 invalid_parameter` naming `cursor`, one row each |
| a token minted under `state=pending&state=running`, presented with `state=running&state=pending` | accepted, because the filter is compared as a set |
| the same token presented with `state=running` alone | `400 invalid_parameter` naming `cursor` |
| the last page | no `nextCursor` key at all |
| every response | entries keep the shape they have today and gain no field |
| every response, header, and entry | no Pod IP and no port |

- [x] **Run the tests and watch them fail**

- [x] **Implement the filters and the cursor**

- [x] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/pgo/
git commit -m "feat(httpapi): filter and page the collections"
```

---

## The command line retries a create whose answer was lost

**Files:**
- Modify: `internal/client/collect.go`, `internal/client/collect_test.go`,
  `cmd/profgate/collect.go`, `cmd/profgate/collect_test.go`

[`docs/specs/cli.md`](../specs/cli.md) *Collections* defines this behavior,
with the retry classification the first task widened,
and `Create` at `internal/client/collect.go:39` already sends the key.
What it does not do is retry:
its comment at `:34-38` says "the gateway does not yet record the key", which stops being true with this plan.

**What changes.**
`Create` retries with the same key on a transport failure, on a `5xx`, and on a response that arrived incompletely,
at one second doubling to eight, for at most 30 seconds.
It counts on the injected clock and sleeper the package already uses for the device grant and the wait loop.

**An answer that fails after its headers is the case the header exists for, and it is not a transport error.**
`Create` at `internal/client/collect.go:55` obtains the response and only then reads its body at `:60`,
so a create whose `202` headers arrived and whose body was cut off returns a body-read error, not a `Do` error,
and a predicate that retried transport failures alone would report it and leave the caller with no identifier.
The classification is therefore by where the failure landed rather than by which call returned it:
a create whose result is unknown is retried under the same key,
and three failures leave it unknown:
no answer at all, an answer the gateway could not complete (`5xx`), and an answer whose body did not arrive whole.
An answer that arrived whole and says something — every `4xx` — is not retried,
and neither is a `2xx` whose body decodes.
`429 collection_in_progress` is reported and exits 1 without waiting:
it means a Collection this command did not create holds the Service.
Any other `4xx` prints its envelope and stops, `409 idempotency_mismatch` included.
The key is generated once per invocation and reused for every retry of that invocation and never afterwards.
`--wait` begins only once a response has carried a concrete identifier, first answer or replay,
which is already how `cmd/profgate/collect.go` is shaped.
The stale comment goes with the change.

- [ ] **Write the retry tests**

`internal/client/collect_test.go`, against an `httptest` gateway, restating *Testing*:

| Case | Expect |
|---|---|
| a transport failure then a `202` | one identifier, two requests, the same `Idempotency-Key` on both |
| a `202` whose body fails midway, then a `200` replay | the replay's identifier, the same key on both requests, and no second Collection |
| a `202` whose body is complete but not JSON | reported, not retried, because the answer arrived whole |
| a `503` then a `200` replay | the replay's identifier, and that is what `--wait` polls |
| failures past the 30-second window on the injected clock | exit 1, and no test sleeps |
| the retry schedule | one second doubling to eight, asserted from the injected sleeper's arguments |
| `429 collection_in_progress` | exit 1 at once, no retry, no poll |
| `409 idempotency_mismatch` | the envelope printed, exit 1, no retry, no poll |
| a `400` | stops immediately |
| a `200` replay body | `id` and `state` and no record field, asserted on the decoded answer |
| two invocations | different keys |
| `--wait` under a realm denying the record route | the identifier printed, the denial reported, exit 1 |

- [ ] **Run the tests and watch them fail**

- [ ] **Implement the retry**

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/client/ ./cmd/profgate/
mise run lint && mise run test && mise run check
git add internal/client/ cmd/profgate/
git commit -m "feat(cli): retry a create whose answer was lost"
```

---

## The end-to-end scenario

**Files:**
- Modify: `test/e2e/scenarios_pgo_test.go`

[`docs/specs/pgo.md`](../specs/pgo.md) *End-to-end* says scenario 1 gains the key, the wait, and the latest assertions.
`pgo-on-demand` at `test/e2e/registry.go:32` is that scenario and already provisions everything the assertions need;
no scenario is added and `registry.go` is untouched.

- [ ] **Extend the on-demand scenario**

| Assertion | Against |
|---|---|
| a create carrying an `Idempotency-Key`, then a second `POST` with the same key and body | `200` with the first identifier and the same `Location`, and one Collection in the listing |
| the same key with a different body | `409 idempotency_mismatch` |
| a `POST` with no `Content-Type` | `400 invalid_parameter` naming the header |
| `GET /v1/collections/{id}?wait=30s` issued while the Collection runs | answers on a state change, before the wait elapses, carrying `X-Wait-Elapsed` |
| `GET .../collections/latest` once the Collection completes | that Collection's record |
| `GET .../collections/latest/profile` | bytes that parse with `github.com/google/pprof/profile` |
| `GET .../collections?state=completed` and `?limit=1` | the filter and the page, with `nextCursor` present only while entries remain |
| every response above | `X-Request-Id` present, and the raw bytes hold no Pod IP |

- [ ] **Run the suite on the current lane**

```bash
mise run test:e2e
```

- [ ] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add test/e2e/
git commit -m "test(e2e): assert the collection contract"
```

---

## The document, and the check that holds it to the router

**Files:**
- Create: `internal/httpapi/openapi.json`, `internal/httpapi/openapi.go`, `internal/httpapi/openapi_test.go`
- Modify: `internal/httpapi/routes.go`, `internal/httpapi/server.go`, `internal/httpapi/codes.go`,
  `internal/metrics/recorder.go`, `internal/metrics/prometheus_test.go`

**This task runs last of the code tasks**, because its check compares the document with the finished router
and the finished registry: every route, parameter, header, and envelope code the tasks above added exists by now.

*The OpenAPI document* defines the route, the file, the absent credential and realm step,
the fixed content across configuration, and the four comparisons the check makes.

**The route.**
One declaration joins the table: `/v1/openapi.json`, `GET`.
It runs the route, method, and readiness steps and the parameter step in the form that refuses every query parameter,
then answers `200` with the embedded bytes and `Content-Type: application/json`,
under the `Cache-Control: no-store` every `/v1` response carries.
It has no credential-placement, authentication, or realm step:
it publishes the route grammar,
which `404 route_unknown` and the `Allow` header of a `405` already publish one request at a time.
`internal/metrics` gains `EndpointOpenAPI`, whose codes are `ok`, `not_ready`, `route_unknown`,
`method_not_allowed`, and `invalid_parameter`, which is every answer the route has.

**The file.**
`internal/httpapi/openapi.json` is hand-written and embedded with `go:embed`;
the route writes those bytes and transforms nothing, so the file in a diff is the file a client parses.
It is an OpenAPI 3.1 document carrying every route the API listener serves with its methods,
path and query parameters with their grammars, request and response shapes,
the `X-Request-Id` response header, the error envelope,
and the `details` schema with every vocabulary of *Errors* and of [`pgo.md`](../specs/pgo.md) *Ceilings* as enumerations.
It does not vary with configuration:
the PGO routes are in it whether or not `pgo.enabled` is set — they answer `501 pgo_disabled`, which it says —
and the console and `/auth/` routes are in it whether or not those are configured.
The ops listener's three paths are not in it.

- [ ] **Add the compile seams: a placeholder document, then the check**

`go:embed openapi.json` with no such file is a compile error, not a failing assertion,
so the file exists before the directive does.
Write a minimal but valid OpenAPI 3.1 document first —
its `openapi` and `info` objects, an empty `paths`, and an empty `components.schemas` —
add the embed and the route against it, and only then write the check.
The check's first run then fails on content, every route and every code missing,
which is the failure the plan-wide rule asks for
and the one that tells an implementer what the document still owes.

`openapi_test.go` implements the four comparisons of *The OpenAPI document*:

1. it walks `declarations()`: every path-and-method pair appears in the document,
   and the document declares no pair the table does not hold;
2. it compares `EnvelopeCodes()` with the codes the document enumerates, and the two sets must be equal;
3. every `details` vocabulary appears as an enumeration in the document;
4. re-encoding the parsed document must equal the file byte for byte,
   so a hand edit cannot leave the file formatted one way and read another.

The check is itself exercised, over documents that differ from the code in exactly one way —
a missing route, an extra route, a missing method, a missing code, an extra code, a missing vocabulary value,
and a file reindented by hand — and each must fail while the shipped document passes.
Those seven fixtures are built by mutating the parsed shipped document in the test rather than by storing seven files.

**Four comparisons are what the accepted design asks for, and they are less than the document promises.**
They hold the document to the router's routes, methods, and codes, and to its own formatting;
they say nothing about whether a parameter, a header, a request shape, or a response shape is described correctly.
A document can therefore pass while describing `wait` as a boolean.
Rather than describe the check as more than it is, this task adds four assertions of its own,
one per thing a client would act on and get wrong:

| Assertion | Reads |
|---|---|
| every query parameter the route table's handlers accept is described on that operation | the parameter names this plan added: `wait`, `state`, `since`, `origin`, `limit`, `cursor` |
| both `POST` write routes require a JSON request body media type | the media-type step |
| `Idempotency-Key` is described on `POST .../collections`, and `X-Request-Id` as a response header on every operation | the two headers this plan added |
| the create operation documents `202` and `200` with the same two-field body, and `409` | the replay |

They are written as data — a table of operation, parameter, and header names in the test —
so a route added later fails them the way it fails the four comparisons.

| Route case | Expect |
|---|---|
| `GET /v1/openapi.json` | `200`, the embedded bytes byte for byte, `Content-Type: application/json`, `Cache-Control: no-store` |
| before the caches sync | `503 not_ready` |
| any query parameter, `access_token` included | `400 invalid_parameter` with an `unknown_parameter` item |
| `POST` | `405` with `Allow: GET` |
| no credential under `basic`, and under `oidc` | `200`, proving no authentication step runs |
| a `/ui/`, an `/auth/`, and a PGO route | present in the document whether or not their configuration enables them |
| `/healthz`, `/readyz`, `/metrics` | absent from the document |
| the metrics row | endpoint `openapi`, profile `none`, and one of its five codes |

- [ ] **Run the check and watch it fail**

- [ ] **Write the document until the check passes**

The check is what holds the file to the code; write the JSON against it,
and against *The OpenAPI document* for everything the check does not read.
Nothing generates it and no build step stands between the file and the response.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/ ./internal/metrics/
mise run lint && mise run test && mise run check
git add internal/httpapi/ internal/metrics/
git commit -m "feat(httpapi): serve the openapi document"
```

---

## The guides, the changelog, and the plan's own status

**Files:**
- Modify: `docs/api.md`, `docs/pgo.md`, `docs/cli.md`, `CHANGELOG.md`, `docs/plans/machine-contract.md`

- [ ] **Update the guides**

| File | Change |
|---|---|
| `docs/api.md:67-88` | the sentence "There is no index route and no OpenAPI document" goes; the count moves from twelve routes to fifteen, the path-parameter table gains the two `latest` rows, and the no-parameter list gains `/v1/openapi.json`; the step list gains the JSON media-type step in its stated place |
| `docs/api.md:481` | "None of the PGO routes take query parameters" is no longer true: two of them do, and the other five still refuse every one |
| `docs/api.md` | `X-Request-Id` on every response of both listeners, accepted or generated; the `details` array with its item shape and every vocabulary; `Idempotency-Key`, its grammar, the `200` replay carrying `{id, state}` and `Location`, and `409 idempotency_mismatch`; `wait=` with its grammar, its refusal above `60s`, and `X-Wait-Elapsed`; the two `latest` routes and the artifact they confirm; the listing's `state`, `since`, `origin`, `limit`, and `cursor`, the cursor's filter binding, and `nextCursor`; the retention default in the create example at `:506` |
| `docs/pgo.md:102` | the `artifact.retention` default; fetching the newest profile in one request; what an idempotent create buys a script that loses its answer |
| `docs/cli.md` | the `collect` paragraph that says a lost create is not retried: it is now, with the same key, whenever the result is unknown — no answer, a `5xx`, or an answer that did not arrive whole — at one second doubling to eight for at most 30 seconds, while every `4xx` that arrived whole is reported as it is |
| `CHANGELOG.md` | under `## [Unreleased]`, an `### Added` entry beside the retention entry the earlier task wrote |

The changelog entry names what a program gets:
`X-Request-Id` on every response and in every audit record;
`details` on every refusal that has a vocabulary, and a `code` on every `violations` entry of `GET /pgo`;
`Idempotency-Key` on the create, with the `200` replay and `409 idempotency_mismatch`;
`GET /v1/collections/{id}?wait=`;
the two `latest` routes;
the listing's filters and cursor;
the `Content-Type: application/json` the two PGO write routes now require,
which is a breaking change for a client that omitted it;
and `GET /v1/openapi.json`.
It says plainly that no Kubernetes permission changed, no NATS store was added, and no Go module was added.
Leave the released sections as they are.

- [ ] **Confirm the invariant wording**

Read `AGENTS.md`, `README.md`, and
[`.agents/rules/800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) beside each other:
this change adds no Kubernetes verb, resource, or API group, and writes only inside `PROFGATE_JOBS`,
so none of the three changes.
Run the three import greps *Two Mechanisms* holds and confirm each is still empty.
Confirm [`.agents/rules/100-project-map.md`](../../.agents/rules/100-project-map.md) is now true of the tree:
it already names `internal/httpapi` as the second `go:embed` user
and already lists `/v1/openapi.json` and the two `latest` routes, so it needs no edit — verify rather than assume.

- [ ] **Finish the plan in the same commit**

Set line 3 of this file to `**Status:** Done`;
insert `**Outcome:**` as line 4, naming the commits or the tag that shipped the change.
[`.agents/rules/900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
binds that flip to the change that lands the plan's remaining work,
and the next commit that touches this file deletes it and rewrites every link that cited it.

No bullet of [`docs/plans/roadmap.md`](roadmap.md) is ticked or unticked by this plan.
Item 6's bullets were already ticked when this plan was written,
and under the legend the first task repaired, a tick records that the design is settled in the specs it names —
which was true then and stays true now.

- [ ] **Validate and commit**

```bash
semlf check docs/api.md docs/pgo.md docs/cli.md CHANGELOG.md docs/plans/machine-contract.md
mise run lint && mise run test && mise run check
git add docs/ CHANGELOG.md
git commit -m "docs: the machine contract"
```

---

## Risks and What This Plan Does Not Cover

- **The route table is a refactor of the one thing every request passes through.**
  Three dispatches become one, and every route in the gateway is re-dispatched by it:
  the two interactive routes, the seven PGO ones, the four listing ones, `/v1/auth`,
  the three `/auth/` routes, and the console subtree.
  It lands as its own commit, adds no route and no code,
  and the test that drives one request per declaration is what proves the move changed no answer.
  The residue it cannot prove is a path nobody drove before and nobody drives after;
  the scan for a second path dispatch is what bounds that.
- **The sweeper now reads a receipt before deleting it, and one race is closed by that read alone.**
  A revision guard cannot tell a stale receipt from a successor's:
  the sweeper deletes a record, a request cleans that record's receipt and publishes a new Collection under the key,
  and a sweeper that deleted at the revision it read would take the live Collection's receipt with it.
  The id check closes it, and its cost is one `Get` per keyed record the retention rule deletes.
  What it does not close is a receipt whose delete fails outright: that one waits for the hourly reconciliation,
  which is what the reconciliation exists for.
- **The receipt adds a fourth write to a publication that had three.**
  A keyed create now costs one more `Create` before its record becomes claimable,
  and a receipt that will not land makes the winner withdraw and answer `503 pgo_unavailable`
  where it would previously have answered `202`.
  That is the promise being paid for: nothing keyed runs unbound.
  A create with no key is unchanged, three writes and no receipt read.
- **Every wait that expires now costs one more store read than a wait that is answered by a pulse.**
  The deadline reads rather than answering from the record it last held,
  because a dropped or coalesced pulse would otherwise turn into a stale answer
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Get a Collection*).
  A minute of waiting therefore ends in one `Get`, which is the same read a pulse would have caused.
  The drain ending is the exception and stays as it was: a replica being taken out of service does not read again.
- **A wait holds a goroutine, a channel, and a connection for up to a minute.**
  Nothing bounds how many waits one client opens beyond the connection limits the server already has:
  `wait` takes no admission slot, by design, because it fetches nothing and reaches no Pod
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Get a Collection*).
  A deployment that finds this expensive is a revision of that section, not a patch here.
- **The retention default is a client-visible move and a startup-visible one.**
  A file that pins a retention under its interval stops loading, `pgo.enabled` false included,
  and an operator who wanted a short retention now has to lower `schedule.every` with it.
  The changelog entry names both keys, and the message the validator writes names them too.
  What this plan checked and states rather than assumes:
  no chart value and no `deploy/` file carries the key,
  one configuration fixture pairs `2h` with a 6h interval and moves with the change,
  and the end-to-end harness's one-minute interval is satisfied by the new default.
- **The hash a replay compares is over the effective policy, so identical JSON can mismatch.**
  A retry sent after the Service's stored override changed, or after the operator defaults moved,
  is `409 idempotency_mismatch` rather than a replay.
  That is the specified answer and the right one — the key would otherwise stand for two Collections that differ —
  but it will surprise a caller who reads the body and not the effective policy,
  which is why both the command line and the console print the envelope rather than retrying.
- **The `latest` walk opens an object per candidate, and `latest` closes it immediately.**
  Confirming and opening are one operation, because two of them leave a window a sweep can land in
  and answer `410 artifact_gone` while an intact artifact exists.
  The record route pays an open it does not use, which is the cost of a probe it would have paid anyway;
  the profile route streams the reader the walk confirmed.
  An object deleted after that open is a truncated stream rather than a `410`,
  which is what a download that began before an expiry already does.
- **A cursor can be forged, and the design does not pretend otherwise.**
  Replicas share no signing material and no cursor state, so no replica can decide it did not mint a token.
  A forged one names a position and a filter set, both of which a client can reach by sending the filters
  and paging to that point, so nothing is disclosed that the listing does not already give.
  What the encoding does promise is that it decodes strictly or is refused.
- **Locating an unknown body field costs a second unmarshal of a body that is about to be rejected.**
  `encoding/json` reports an unknown field's name and not its path,
  so a nested `unknown_field` pointer has to come from a walk of the decoded shape.
  The walk runs on at most 64 KiB, only on a body that fails, and finds nothing on a body that passes.
- **The document is hand-maintained, and the check is what makes that safe.**
  Nothing generates the JSON, so a route added without a document entry is a red test rather than a silent lie —
  as long as the check itself is exercised.
  The seven one-way-wrong fixtures are that exercise;
  a check that only ever ran against the shipped document would prove nothing.
  What the check still cannot catch is a code a constructor names and no route answers with,
  which the registry deliberately includes for `collector_unavailable`.
- **`X-Request-Id` reflects client text into a response header.**
  The byte set excludes `CR`, `LF`, and everything that could split or forge a header,
  and 128 bytes caps what one request reflects (*Request identifier*).
  The value is echoed into one header and one audit record and is read by nothing.
- **No browser test executes the console against any of this.**
  The console sends `Content-Type: application/json` and an `Idempotency-Key` on its own start button,
  which is a later item of [`docs/plans/roadmap.md`](roadmap.md);
  nothing in `internal/ui/static/` is edited here, and the media-type step is proven in Go alone.
  The one edit this plan makes to `internal/ui` is in `ui.go`:
  two code literals become registry constants, which its own tests already cover through the bodies it writes.
- **The end-to-end assertions run on one lane in one scenario.**
  The wait, the replay, and the latest walk meet a real NATS server only inside `pgo-on-demand`;
  their races meet only the in-process server the unit tests drive.

---

## Self-Review

- Spec coverage:
  the four gaps inside the accepted specs, closed before anything is implemented from them —
  which read a wait compares against, what its deadline reads in both the prose and the test row,
  what a receipt whose Collection never ran answers,
  and which create failures the command line retries
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Get a Collection*, *Create a Collection*, *Unit*,
  [`docs/specs/cli.md`](../specs/cli.md) *Collections*, *Testing*,
  in *The accepted specs and the roadmap are repaired*);
  the identifier's grammar, its generated form, where it is set, the audit field, and the absent label
  (*Request identifier*, *Logging*, *Health*, *Metrics*, in *Every response carries a request identifier*);
  the `details` item shape, the omission rule, and the `invalid_parameter` vocabulary in full
  (*Errors*, in *An error names the inputs it refuses*);
  the machine form of `limit_exceeded` and the `code` on every `violations` entry
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Ceilings*, in the same task and in the retention task);
  the route table every API-listener route is dispatched from, the `Allow` header, and the registry
  (*The OpenAPI document*, in *One table declares every route, one registry declares every code*);
  the JSON media-type step and its position ahead of readiness, authentication, the realm, and every store call
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Request media type*, *HTTP API*,
  [`docs/specs/auth.md`](../specs/auth.md) *Request algorithm*,
  in *The two write routes require a JSON media type*);
  the retention default and the effective-policy rule in each of the five places
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Ceilings*, *Configuration*,
  in *An artifact outlives the interval that produced it*);
  the receipt's key, value, writer, order, and sweep
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Create a Collection*, *Atomicity primitives*,
  *Paths that touch each key*, *Algorithm*, *Record*, *Sweeper*,
  in *One idempotency key binds one Collection*);
  the lookup's position, the thin replay, the mismatch, and how a loser resolves
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Create a Collection*, *Non-disclosure*,
  in *A create can be retried*);
  the wait's parameter, its six endings, the pulse rules, the broadcast, `X-Wait-Elapsed`, and the drain
  ([`docs/specs/pgo.md`](../specs/pgo.md) *Get a Collection*, *Startup and shutdown*,
  in *A client can wait for a Collection to move*);
  the two `latest` routes and the four-step selection they share
  ([`docs/specs/pgo.md`](../specs/pgo.md) *The latest completed Collection*,
  in *A build asks for the latest profile*);
  the filters, the total order, the cursor's filter binding, and `nextCursor`
  ([`docs/specs/pgo.md`](../specs/pgo.md) *List Collections*, in *The listing is filtered and paged*);
  the create retry ([`docs/specs/cli.md`](../specs/cli.md) *Collections*,
  in *The command line retries a create whose answer was lost*);
  the document, its four comparisons, and the route's shorter algorithm
  (*The OpenAPI document*, *Request algorithm*, *Metrics*,
  in *The document, and the check that holds it to the router*);
  the end-to-end assertions ([`docs/specs/pgo.md`](../specs/pgo.md) *End-to-end*, in *The end-to-end scenario*).
- Each task's stated tests are green before its commit against the tree that task leaves,
  and each code task adds the declarations its tests name before the assertions that use them
  ([`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
  *Test plans compile against current source*).
  The spec repair lands first because the wait task implements from it.
  The identifier and the `details` vocabulary land before the table,
  because both touch every error the table then re-dispatches.
  The table and the registry land before every task that adds a route or a code.
  Those then arrive as declarations and constants,
  rather than as regular-expression branches and string literals a later task would have to unwind.
  The receipt lands in `internal/pgo` before the handler that reads it.
  The document lands last of the code tasks, because its check reads the finished table and the finished registry.
- Names defined once and used by those names afterwards:
  `RequestID` and `WithRequestID` in `internal/httpapi`;
  `paramFault`, `headerFault`, `bodyFault`, and the twelve `detail*` constants;
  `declaration`, `declarations`, `match`, and `EnvelopeCodes`;
  `mediaTypeFault`;
  `ReceiptKey`, `Receipt`, `SnapshotHash`, `Session.ReadReceipt`, `Session.DeleteReceipt`,
  `Session.Subscribe`, `Session.GenerationMoved`, and `Session.LatestCompleted` in `internal/pgo`;
  `Violation.Code`, `Record.IdempotencyKey`, and `Record.SnapshotHash`;
  `Deps.Drain` in `internal/httpapi`;
  `EndpointOpenAPI` in `internal/metrics`.
- Current-source facts this plan rests on:
  `internal/httpapi/errors.go:19-20` says `details` is filled by `port_not_allowed` alone,
  and `internal/httpapi/profile.go:307-315` is the one constructor that fills it;
  `internal/httpapi/errors.go:35` already omits the key when the slice is nil;
  `internal/httpapi/audit.go:14-31` has no `requestId` field and builds three attribute lists at `:37-61`;
  `internal/httpapi/server.go:356-359` sets `Cache-Control` before it routes,
  `:371` and `:377` are the console and `/auth/` dispatches,
  `:213-266` is `parseRoute` over four exact paths and three regular expressions,
  `:186-200` is `routeKind.methods()`,
  `:407-412` writes the `405` and its `Allow`,
  `:414` is the readiness step, `:433-445` the PGO availability step, `:449` credential placement,
  `:464` the Collection-scoped read, `:470` the realm step,
  `:283-325` is `labels()`, and `:564` is `discoveryError`;
  `internal/httpapi/console.go:12` and `internal/httpapi/auth.go:29` are the two dispatches beside `parseRoute`;
  `internal/httpapi/pgo.go:154` is `storeError`, `:166` `decodeBody`, `:191` `rejectBody`;
  `internal/httpapi/pgo_collections.go:120-206` is the create handler,
  whose snapshot is built at `:139` and whose ceiling refusal is at `:141`,
  and `:204` writes `acceptedBody`;
  `internal/httpapi/pgo_policy.go:310-323` builds `limit_exceeded` from the violations' text;
  `internal/proxy/proxy.go:45-47` forwards five upstream headers and no others, `X-Request-Id` among the excluded;
  `internal/ops/ops.go:14-32` builds a mux of three paths and wraps nothing;
  `internal/ui/ui.go:85-93` owns the console's method check and writes its `405` with `Allow: GET, HEAD`;
  `internal/pgo/policy.go:317-321` is `Violation` with three fields and no `code`,
  and `:328-393` is `Validate`, which `PUT /pgo`, `POST /collections`, the scheduler, and the worker all call;
  `internal/pgo/record.go:72-101` is `Record`, with neither `idempotencyKey` nor `snapshotHash`;
  `internal/pgo/publisher.go:35-50` is `PublishInput`, whose `CreatedBy` at `:49` already holds the principal,
  and `:237` is `Publish`, which performs three writes;
  `internal/pgo/clock.go:6-11` is the `Clock` seam with `NewTimer`,
  and `internal/pgo/fixtures_test.go:916-1030` is a fake clock with a working timer;
  `internal/httpapi/fixtures_test.go:1154-1179` is the HTTP fake clock, whose `NewTimer` panics;
  `internal/httpapi/pgo_collections.go:245` resolves a download's object and `:276` opens it;
  `internal/httpapi/pgo.go:162-163` decodes with `DisallowUnknownFields`, which reports a name and no path;
  `internal/ui/ui.go:90`, `:106`, and `:112` write `method_not_allowed` and `route_unknown` as literals;
  `internal/client/collect.go:55` obtains the response and `:60` reads its body, so a cut-off body is not a `Do` error;
  `internal/pgo/caches.go:142-168` holds the four caches and the single `jobPulse`,
  `:252-282` is `apply`, and `:525-560` is `Collections`, truncating at the `maxListCollections` of `:519`;
  `internal/pgo/runtime.go:81-101` takes the generation, the barrier, and the view for one request,
  and `:131-142` is `ReadRecord`;
  `internal/natskv/natskv.go:107-119` is the `Client` interface with four methods,
  `:42-507` the `KV` interface with five,
  and `internal/natskv/client.go:124-132` runs `bumpGeneration` before `onConnectionChange(false)`;
  `internal/config/config.go:402` defaults `artifact.retention` to `2h`,
  `:359` defaults `maxRetention` to `24h`, `:381` defaults `schedule.every` to `6h`,
  `:818` holds `jobRetention >= maxRetention + 1h`, and `:1232` holds `retention <= maxRetention`;
  `internal/config/config_test.go:666` asserts `2h`;
  `internal/config/testdata/pgo-full.yaml:39` pairs `retention: 2h` with `every: 6h`;
  `internal/config/testdata/pgo-zero-retention.yaml:9` expects the `min=1m` message;
  `test/e2e/harness_test.go:872` configures `every: 1m` and no retention;
  `deploy/` names `retention` nowhere and the chart does not render `pgo.defaults`;
  `internal/client/collect.go:34-38` says the gateway does not record the key and that `Create` retries nothing;
  `internal/metrics/recorder.go:13-41` lists the endpoint values and has no `openapi`;
  `test/e2e/registry.go:32` is the `pgo-on-demand` scenario;
  `docs/api.md:67-88` states twelve routes and no OpenAPI document;
  `docs/configuration.md:438` and `:658`, `docs/pgo.md:102`, and `docs/api.md:506` carry the `2h` default;
  nothing under `internal/` or `cmd/` mentions `X-Request-Id` or `openapi.json` today.
- Decided here because the spec leaves it to the implementer:
  the identifier's middleware lives in `internal/httpapi` and `internal/ops` imports it;
  the route table declares the console subtree while `internal/ui` keeps writing its own `405`,
  with a test comparing the two;
  the registry holds `collector_unavailable`, which no route answers, because the check compares against both tables;
  a wait's baseline is the read the route already made for the realm;
  the generation broadcast is fed by `OnConnectionChange` rather than by a poll of `Generation()`;
  `snapshotHash` is over `json.Marshal` of the policy struct and never over request bytes;
  the `Idempotency-Key` grammar is checked at the parameter step, before the body is decoded;
  `Violation.Field` stays dotted and only the `details` item is a pointer;
  the cursor's encoding is opaque and left open, bounded only by round-tripping through one function pair
  and refusing a token it did not mint;
  the seven one-way-wrong document fixtures are built in the test, by mutating the parsed shipped document,
  rather than by storing seven files;
  `LatestCompleted` returns the artifact it opened, because confirming and opening cannot be two operations;
  the cursor is versioned and strictly decoded and claims no authenticity, with `state` compared as a set;
  an unknown body field is located by a walk before the strict decode, because `encoding/json` reports no path;
  the session captures its generation's broadcast channel rather than looking one up at select time;
  the wait's deadline ends in an authoritative read, so a dropped pulse costs latency and never an answer;
  the sweeper compares a receipt's id with the record it is deleting for before it deletes.
- Left to the implementer by design: helper and test-fixture names,
  the exact wording of error messages beyond the values each must name,
  the cursor's encoding, the layout of the OpenAPI file beyond what the check compares,
  and the internal split of the route table across functions,
  so long as `match` stays the only path dispatch in the package.
