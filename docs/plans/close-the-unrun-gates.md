# Every Gate the Repository Names Runs

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> On this machine `mise run lint` runs a golangci-lint 2.1.6 that shadows the pinned 2.12.2,
> so every validation block below runs the linter as `mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`
> and never as `mise run lint`.
> In a `git worktree` the image build `ko` runs for the end-to-end suite fails on the version-control stamp;
> `GOFLAGS=-buildvcs=false` in front of `mise run test:e2e` is what an earlier run used to get past it.

**Goal:** make every gate this repository says it runs run, and pin the claims nothing checks.
`.github/workflows/check.yml` runs `mise run check`, the linter, and the unit tests on `push` alone (`.github/workflows/check.yml:3-7`),
so a pull request from a fork, which fires `pull_request` alone,
gets the `current` end-to-end lane and the prose check and none of the three.
`PUT` and `DELETE` on the policy route read `If-Match` (`internal/httpapi/pgo_policy.go:112`, `:203`),
the OpenAPI document names the header in two descriptions and declares it nowhere (`internal/httpapi/openapi.json:2289`, `:2329`),
and `TestOpenAPIDocumentParameters` walks `query` and `path` and never `header` (`internal/httpapi/openapi_test.go:563-582`).
`.agents/rules/900-design-and-review-loops.md:32-33` says no CI invokes `mise run check`,
and `check.yml` has since 2026-08-23.
Fifteen route counts stated in prose across three guides and the project map are pinned to nothing,
and three of them are wrong.
`docs/api.md:84`, `docs/api.md:844`, and `.agents/rules/100-project-map.md:139` each say one `/v1` route needs no credential,
where `internal/httpapi/server.go:504-512` answers two, `kindAuth` and `kindOpenAPI`.
`TestRoundsDecodeHeapDelta` skips under `-race` (`internal/pgo/rounds_test.go:849-851`)
and every test command passes `-race` (`mise.toml:34`, `check.yml:12`, `.github/workflows/release.yml:21`),
so the decoder's memory guard has never run, although the roadmap ticks that bullet.
`mise.toml:41` runs the end-to-end suite without `-race` while rule 300 says the flag is always on.
`test/e2e/harness_test.go` is 1702 lines,
and the decision record that governs it says its size trigger fired and the split it calls for is not done.
After this plan the unit gates run on every pull request and every push to `main`,
the document declares the conditional and idempotency headers its operations read
and the check holds every operation to a reviewed set,
rule 900 names what runs the check,
`mise run check` refuses a route count the prose gets wrong,
the heap-delta guard runs under `-race` with every other test,
the end-to-end suite runs under `-race`,
and the harness is five files along the subjects its record names.
No route, configuration key, chart value, Kubernetes call, or NATS permission is added.

**Architecture:** `.github/workflows/check.yml` takes the event shape `e2e.yml` already has;
`docs/specs/gateway.md` gains the amendment block its revision on this branch owes;
`internal/httpapi/openapi.json` gains an `IfMatch` parameter component and the two policy writes reference it;
`internal/httpapi/openapi_test.go` gains `headerParameters` beside `queryParameters`,
`TestOpenAPIDocumentParameters` walks `header` against it,
and `TestOpenAPIDocumentHeaders` keeps the response header alone;
`.agents/rules/900-design-and-review-loops.md` says what runs the check;
`scripts/check-repo.py` gains `check_route_counts`,
which reads `routeTable` and the dispatch and holds sixteen count occurrences in five files to them,
and `docs/api.md` and `.agents/rules/100-project-map.md` correct the three of those
that say one `/v1` route needs no credential;
`internal/pgo` loses its `raceEnabled` skip and the two build-tagged files that carry the constant,
and `docs/specs/pgo.md` stops saying the guard skips;
`mise.toml` passes `-race` to the end-to-end suite;
`test/e2e/harness_test.go` gives its NATS, configuration, port-forward, and Pod helpers to four files beside it,
and `docs/decisions/e2e-without-framework.md` carries the measurements the split makes false.
Nothing under `internal/` changes behavior; `internal/pgo` loses a skip and nothing else.

**Spec:** the pull-request gates are accepted text in [`gateway.md`](../specs/gateway.md) *Continuous integration* (`docs/specs/gateway.md:2226-2238`),
revised on this branch to name both events on one job with no event gate (`:2235-2238`).
The document's duty to carry "parameters and their grammars" is *The OpenAPI document* (`:1228-1229`),
and its check is the five comparisons of that section (`:1295-1306`).
The heap-delta guard is *Unit* of [`pgo.md`](../specs/pgo.md) (`docs/specs/pgo.md:3496-3497`), which this plan amends.
The other bullets have no spec;
they are the roadmap's own, under *Close the gates that do not run* (`docs/plans/roadmap.md:318-338`).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the repository, not as the defects that revealed them.

- **Every gate the specification names runs on every pull request.**
  `check.yml` fires on `push` and `pull_request` (`.github/workflows/check.yml:2-4`) and gates `check` on `push` (`:7`),
  so the contribution that fires `pull_request` alone, a fork's, runs lint, unit tests, and `mise run check` nowhere;
  `e2e.yml` names both events and gates neither job (`.github/workflows/e2e.yml:2-12`, `:21`, `:33-35`).
- **The document declares the conditional and idempotency headers its operations read, and no others.**
  `parseIfMatch` reads `If-Match` on the policy `PUT` and `DELETE` (`internal/httpapi/pgo_policy.go:112`, `:203`),
  `idempotencyKey` reads `Idempotency-Key` on the create (`internal/httpapi/pgo_collections.go:93`, called at `:463`),
  `ServeHTTP` in `internal/ui` reads `If-None-Match` for a console asset (`internal/ui/ui.go:125`),
  and `RequestID` reads `X-Request-Id` before any route resolves (`internal/httpapi/requestid.go:28`, `internal/httpapi/server.go:403-406`);
  the document declares `Idempotency-Key` and `If-None-Match` and nothing else in a header (`internal/httpapi/openapi.json:137`, `:149`),
  and `TestOpenAPIDocumentParameters` reads `query` and `path` (`internal/httpapi/openapi_test.go:567`, `:578`).
  The credential headers are outside the invariant: OpenAPI describes them as a security scheme, not as a parameter.
  `internal/auth` reads `Authorization` (`internal/auth/basic.go:130`, `internal/auth/oidc.go:146`)
  and the `Sec-Fetch-*` triple (`internal/auth/browser.go:154`, `:191`) at the realm step.
- **A rule states the current fact.**
  Rule 900 says "No CI invokes it yet" (`.agents/rules/900-design-and-review-loops.md:32`);
  `check.yml` has run `mise run check` since `9632a40`,
  and the paragraph has stood unedited since `0a1687a` (`git blame -L 29,35`), which landed the same day.
- **A number the prose states about the code is a number a check reads from the code.**
  `routeTable` is a literal of twenty-one templates, each naming its kind (`internal/httpapi/routes.go:46-74`);
  `docs/api.md:67`, `:81`, `:84`, `:121`, `:501`, `:506`, `:844`, `:896`, `:1002`,
  `docs/configuration.md:334`, `docs/deployment.md:648`,
  and `.agents/rules/100-project-map.md:130`, `:139` state counts over it, and nothing reads the table for them.
  Three of those counts are already wrong:
  `docs/api.md:84`, `:844`, and `.agents/rules/100-project-map.md:139` say one `/v1` route needs no credential
  where `internal/httpapi/server.go:504-512` answers `kindAuth` and `kindOpenAPI` without one.
- **Every test runs under `-race`.**
  Rule 300 says the flag is always on (`.agents/rules/300-testing.md:16`);
  `TestRoundsDecodeHeapDelta` skips when `raceEnabled` (`internal/pgo/rounds_test.go:849-851`),
  a constant two build-tagged files set (`internal/pgo/race_off_test.go:6`, `internal/pgo/race_on_test.go:8`),
  and `test:e2e` passes no `-race` (`mise.toml:41`) where `test` does (`:34`).
- **A decision record's measurements say what is true of the file they measure.**
  `docs/decisions/e2e-without-framework.md:28` names one harness file,
  and `:36-40` and `:58-70` say it is 1672 lines with 59 functions and that the split it calls for is not done;
  the file is 1702 lines with 60 (`wc -l`, `grep -c '^func '`), and unsplit.

---

## Decisions

Eighteen choices settle how the bullets are carried, and the facts of the code that shape each one.

**Eight tasks: one per bullet of the item, one for the end-to-end flag, and the closing task.**
The roadmap's item has six bullets; five are open and the first is ticked,
by the commit that closed the previous plan (`8d44a2c`), with no change to the code it names:
the guard still skips and every command still passes `-race`.
A tick records that a decision is settled or the work is done (`docs/plans/roadmap.md:11-15`); this one records neither,
so the plan carries a task for it rather than a `Shipped:` line over a bullet that is not built.
The end-to-end flag has no bullet and is the same defect:
a rule that says `-race` is always on over a command that never passes it.
The rule 900 task follows the workflow task because its sentence describes the world that task creates.
The harness split is last, in its own commit, because it is the largest diff and depends on no other task.
It is required all the same:
it is the only task that closes its roadmap bullet, and the closing task ticks that bullet.

**`check.yml` takes `e2e.yml`'s shape: `pull_request` plus `push` to `main`, and no event gate on the unit job.**
`e2e.yml` names `pull_request` and `push` to `main` (`.github/workflows/e2e.yml:2-12`)
and gates neither job (`:21`, `:33-35`);
`check.yml` names `push` on every branch and `pull_request` (`.github/workflows/check.yml:2-4`),
gates `check` on `push` (`:7`), and `prose` on `pull_request` (`:14`).
The `check` job loses its gate and the workflow's `push` narrows to `branches: [main]`;
the `prose` job keeps its gate because `semlf --base origin/${{ github.base_ref }}` (`:21`) needs a base branch,
which a push does not carry.
`release.yml` has a `gates` job of its own (`.github/workflows/release.yml:16-21`) and reads nothing from `check.yml`,
so a tag is unaffected.
The spec already says so (`docs/specs/gateway.md:2230`, `:2235-2238`);
that revision landed on this branch (`8bee224`) without the block every revision of the file records under *Amendments* (`:2623-2632`),
and task 1 adds it, because a spec edit rides with the change that makes it true.

**A same-repo pull request runs the unit gates once, and the fork case is reasoned rather than observed.**
A push to a branch other than `main` matches no `push` filter and starts nothing;
each push to the pull request's branch fires `pull_request` once;
the merge is one push to `main` and fires `push` once.
That `push` run is a distinct run on a distinct ref, not a first look at the content:
a `pull_request` checkout tests the merge of the branch into its base,
so the merged tree is what the pull request runs already saw.
A pull request from a fork fires `pull_request` in this repository and nothing else, and the ungated job answers it.
The evidence is the workflow run itself rather than a check name:
a job GitHub skips still reports success in the checks list,
so `check` appearing there proves nothing about whether the three commands ran.
What is kept is the run URL, the `pull_request` event, the commit it tested,
and the log of the step that ran `mise run check && mise run lint && mise run test` (`.github/workflows/check.yml:12`),
together with the absence of a second run from a branch `push`
and, after the merge, the separate `push` run on `main`.
No fork of this repository can be raised from this checkout.
Event eligibility and an observed fork run are two things:
the first follows from `check.yml` naming `pull_request` with one ungated job definition and is settled here;
the second waits for a first fork contribution, and the plan says so where it is claimed.

**`If-Match` is one component, referenced by the two writes, with the grammar `parseIfMatch` accepts.**
`ifMatchRE` is `^"[0-9]+"$` (`internal/httpapi/pgo_policy.go:25`), the schema's pattern.
The pattern is not the whole grammar:
`parseIfMatch` also parses the digits as a `uint64`
and refuses what overflows (`internal/httpapi/pgo_policy.go:302-305`),
so a value the pattern admits and 64 bits cannot hold is `400 invalid_parameter`,
which is what every other malformed form gets (`internal/httpapi/pgo_policy.go:312-315`).
The description carries that range and the schema carries the pattern,
because JSON Schema has no integer bound to put on a string.
`required` is `false`:
the first `PUT` must omit it,
and a `PUT` that carries it where no override exists is `412` (`:139-142`);
over an existing override both writes refuse its absence with `428` (`:160-163`, `:221-222`)
and a stale revision with `412` (`:165`, `:226`);
the description says all four.
The `ETag` component already names the round trip (`internal/httpapi/openapi.json:17`).
`compareEncoding` re-encodes the file with keys in name order and two-space indent (`internal/httpapi/openapi_test.go:276-294`),
which puts `IfMatch` between `IdempotencyKey` and `IfNoneMatch`
and leaves each operation's array in the order it is written.

**The header map is exact and lives in `TestOpenAPIDocumentParameters`; `TestOpenAPIDocumentHeaders` keeps the response header.**
`queryParameters` holds every operation to exactly the query it reads, naming none for the rest (`:55-73`);
`headerParameters` has the same shape and the same walk,
so the two locations a client sends are read by one test under one rule.
What the walk compares is two maintained descriptions, the map and the document, and never a handler:
it holds the document's declared header parameters to a reviewed expected set,
so a change to what a handler reads has to update the set and the document together,
and neither one alone can drift from the other.
A handler that starts reading a header while both stay as they are passes, and no test in this repository catches that.
`TestOpenAPIDocumentHeaders` holds two things today:
`X-Request-Id` on every response, and `Idempotency-Key` on the create and nowhere else (`:609-638`).
The second is the map's rule for one header; keeping both would be the same assertion twice, once loosely.
The tests do not merge:
one reads what a client sends, per operation and per location;
the other reads what every answer carries, which is not a parameter,
and a merged test would carry a response-side walk under a name about parameters.
The key's rule moves into the map, the response walk stays,
and the test's comment says it reads the one header every answer carries.

**The map's scope is the headers the document describes as parameters, and the credential headers are outside it.**
A request header reaches an operation through one of three OpenAPI shapes:
a parameter, a media type, or a security scheme.
`headerParameters` is about the first alone.
`Authorization` is the third: `internal/auth` reads it on every `/v1` route under `basic` and under `oidc`
(`internal/auth/basic.go:130`, `internal/auth/oidc.go:146`),
and the browser path reads `Sec-Fetch-Mode`, `Sec-Fetch-Dest`, and `Sec-Fetch-Site`
(`internal/auth/browser.go:154`, `:191`) to tell a navigation from a cross-site form post.
None of the four is a parameter of any operation, and none belongs in the map.
The document declares no security scheme either —
`grep -n '"Authorization"\|securitySchemes\|"security"' internal/httpapi/openapi.json` finds nothing —
which is a real gap in what a generated client can send, and a document change with client-visible consequences.
This plan does not make it:
no bullet asks for it, it is not the defect the `If-Match` bullet names,
and adding a scheme reaches every operation of the document at once.
*Risks and What This Plan Does Not Cover* names it as owed.
`Content-Type` is the second shape and is already held by `TestOpenAPIDocumentWriteRoutesRequireJSON`.

**`X-Request-Id` is not a parameter, and the map says so by holding every operation to none.**
`ServeHTTP` reads it before any routing decision (`internal/httpapi/server.go:403-406`),
and the ops mux reads it through `WithRequestID` (`internal/httpapi/requestid.go:39-45`);
a value the gateway will not take is replaced, never refused, because the identifier decides nothing (`:26`).
A parameter of an operation is an input its answer depends on;
this header is a property of both listeners, like `Cache-Control: no-store`, and no handler reads it.
The document describes it once, on every response,
and the `RequestId` component's description names the client's own value (`internal/httpapi/openapi.json:52-53`);
the guide says a request may choose its own (`docs/api.md:163`).
Declaring it as a parameter would be one reference on every operation of the document for a header that changes no answer,
and the map holding every operation to none is what turns that choice into a tested fact rather than an omission.

**`If-None-Match` is declared where it is read; the map holds the set and `compareConditional` holds the relationship.**
`internal/ui` reads it for an asset (`internal/ui/ui.go:125`) and for nothing else:
the root redirects and the shell is written without a tag (`:111-117`).
The document declares it on `GET` and `HEAD` of `/ui/{file}` (`internal/httpapi/openapi.json:1995`, `:2018`)
and on no `/v1` operation, which is right.
`compareConditional` already ties the header to a `304` and an `ETag` (`internal/httpapi/openapi_test.go:771-812`),
and two drift cases prove it (`:429-441`);
that is a relationship over three facts, and the map is a set per operation,
so the two do not overlap and neither is dropped.

**Rule 900 names the workflow, and the two mechanisms it said were pending.**
The stale sentence is `.agents/rules/900-design-and-review-loops.md:32-33`; the bullet names it.
The next sentence, `:34-35`, says the two mechanisms of rule 800 "join the same task once Go code exists";
the import grep is `check_clientgo_importers` in the same script (`scripts/check-repo.py:121-129`),
the golden ClusterRole test is `TestClusterRoleTuples` (`deploy/deploy_test.go:76`),
and the script's own docstring says where the second one lives (`scripts/check-repo.py:24-25`).
It is the same staleness one sentence on, in the same paragraph,
and a reader who found the first sentence corrected and the next still pending would read the mechanisms as not yet built,
so task 3 rewrites both and nothing else in the file.

**The route-count rule holds fourteen patterns over sixteen occurrences, each count read from the code.**
`routeTable` is a literal, one entry per line carrying its template and its kind (`internal/httpapi/routes.go:47-73`),
which a regular expression reads without a Go toolchain, as the rest of the script works.
Seven counts follow from those two fields:
the templates under `/v1/` (fifteen), those of them naming `{service}` or `{id}` (nine) and the rest (six),
those naming `/pgo` or `/collections` (seven), the templates under `/auth/` (three),
the console's, named by template (`/`, `/ui/`, `/ui/{file}`: three),
and the `/v1` templates whose kind answers before the credential step (two),
the kinds themselves read from the span of `internal/httpapi/server.go` between the readiness check and the PGO steps
(`internal/httpapi/server.go:498`, `:504-512`, `:522`).
The console group is classified by its own templates rather than by what is left over,
so a template that fits no group is reported as unclassified instead of silently counted as the console's.
Fourteen patterns state those counts over sixteen occurrences, because two patterns match twice:
`docs/api.md:67` (twice), `:81`, `:84`, `:121`, `:501`, `:506`, `:844`, `:896`, `:1002` (twice),
`docs/configuration.md:334`, `docs/deployment.md:648`,
`.agents/rules/100-project-map.md:130`, `:139`,
and the comment over the span itself, `internal/httpapi/server.go:504`,
so the code's own statement of the count moves with the guides'.

**The three sentences that miscount the credential-free routes are corrected in the same task that pins them.**
`docs/api.md:84` and `:844` call `GET /v1/auth` "the one `/v1` route that requires no credential",
and `.agents/rules/100-project-map.md:139` calls it "the only `/v1` route with no authentication step".
`internal/httpapi/server.go:504-512` answers two before the credential step, `kindAuth` and `kindOpenAPI`,
under a comment that says so;
`docs/api.md:90` says of `/v1/openapi.json` "It is served from the binary and takes no credential",
six lines under the sentence that says it is the only one.
So the guide contradicts itself and the project map repeats the error.
Task 4 corrects all three to the two the code answers and pins the corrected count,
which is why the count is derived from the kind field and not from the path alone:
a route needing no credential is a property of `kind`, and the path does not carry it.
The kinds are read from the dispatch rather than kept beside the pattern table:
`open_kinds` takes the span of the request algorithm between the readiness check and the PGO steps
and collects every `rt.kind ==` in it,
so a route that joins them moves the count and turns all four places red at once.
A span that does not resolve, or a kind that answers there and names no route in the table,
is an error naming the file rather than a count that quietly stays two.
The pattern table is then the one list the check keeps rather than reads.

**Each pinned occurrence is identified on its own, and its cardinality is asserted.**
The rule holds each pattern by its wording with the number word as the one free part.
Every match is checked, not the first one, so a reworded occurrence beside a matching one cannot ride along;
and each row names how many occurrences it expects,
so a sentence that vanishes or a copy that appears is a failure naming the row.
The patterns match across a line break — every run of spaces becomes `\s+` and the file is read whole —
because this repository writes prose in semantic line breaks
and a sentence legitimately wraps at a clause boundary (`.agents/rules/500-validation-and-workflow.md`).
The `/v1`-route pattern carries the opening parenthesis that follows it at `docs/api.md:121` and `:1002`,
so it cannot also match the credential sentences.
A count the check has no number word for is reported as that, with the count and the name,
rather than raising out of the script.
The pattern table and the two credential kinds are the only lists in the check not read from the code,
and they are the price of a failure that names a sentence and never a parse.
The guides carry no `Status:` and age by being kept true against the code (`docs/README.md:13-14`),
which is what a check over them means.
The specs are left out because their counts name groups the specs define — the four listing routes, the two `latest` routes —
and their amendment tables quote earlier wording verbatim, which a scan would flag as drift;
`CHANGELOG.md` is history and is never rewritten;
decision records and investigations are not kept true against the code the way a guide is (`docs/README.md:24-25`).
"Four routes let a script" (`docs/api.md:280`) and "the four routes it calls" (`docs/console.md:8`) are left unpinned:
which routes are the console's group is a fact of `ui.md` and of the page, not of the template grammar,
and a check could hold it only as a list of its own.
So are the counts in `docs/api.md:506-507` other than the denominator:
"Two of the seven take query parameters" and "the other five" split the PGO group by whether an operation reads a query,
which is a fact of `queryParameters` and not of the route table;
the seven is pinned and the two and the five are not.

**The heap-delta guard is a retained-heap regression guard, and task 5 measures whether it holds under `-race`.**
The skip says the detector's allocator accounting makes the delta meaningless (`internal/pgo/rounds_test.go:846-850`).
What the assertion actually measures is what the decoded profile keeps live across a collection,
not the peak memory decoding reaches:
`runtime.MemStats.HeapAlloc` is the heap in allocated objects at the moment it is read,
and a `runtime.GC()` sits between the parse and the second read (`internal/pgo/rounds_test.go:867-868`).
Calling it a bound on decoding memory would be a claim the method does not support;
calling it a regression guard on retained heap is what it is, and that is the wording the comment and the spec take.
An earlier measurement disabled the skip through a `go test -overlay` and logged the delta.
Over `cpu-heap.pprof`, 516,906 bytes plain, against the bound of 4,135,248
(`config.PGODecodeFactor` is `8`, `internal/config/config.go:529`),
it put three runs under `-race` at 3,932,736, 3,932,752, and 3,942,928 bytes
and three without at 3,927,256, 3,932,720, and 3,937,336 —
about five percent of margin either way, and a difference between the builds under a third of one percent.
That measurement is what prompted this task and is not its evidence:
it did not keep the decompressed input alive to the final read,
so a collection of `plain` could have been subtracted from what looks like the decoder's footprint,
and 192,320 bytes of margin is less than the input allocation whose lifetime was uncontrolled.
Task 5 controls both lifetimes and re-measures, and its own numbers are what decide the skip.
The direction the earlier numbers point is that the detector does not move the delta enough to matter,
which is why the plan carries removing the skip rather than adding a second `go test` command:
that alternative would write an exception into rule 300 for a claim no measurement supports.
If the corrected measurement says otherwise, task 5 stops and says so rather than shipping the flag.

**`-race` on the end-to-end suite is finished when a full run passes under it, and a race it reports is fixed there.**
Rule 300 says `-race` is always on (`.agents/rules/300-testing.md:16`);
`mise.toml:41` passes no flag where `:34` does.
The flag instruments the test binary — the harness, the scenarios, the browser session's recorder —
and not the images `ko` builds (`test/e2e/harness_test.go:366-382`).
A full run on the `current` lane takes about 700 seconds today, the figure the last full local run gave;
the detector will grow that, and the task measures by how much.
A reported race is a defect in the harness or the gateway, which is what the rule says,
so it is reproduced and fixed inside this task and the suite is rerun under the flag.
The flag is never left off as a finished outcome, and rule 300 is never reworded to fit the tooling.
If a fix turns out to need a design change larger than this plan, the task stops and says so:
the plan does not reach `Done` with the flag off,
and whether to carve that work out is decided then rather than pre-approved here.
The flag and the harness split touch the same declarations,
so a race in code task 7 moves is fixed before the move commit,
and the move is then validated by the run in *Validation*.

**The harness gives four new files to the subjects its record names and to the Pod helpers beside them.**
*Revisit* names three subjects (`docs/decisions/e2e-without-framework.md:66-69`):
NATS, about 425 lines; gateway configuration rendering and Secret application; port forwarding.
Read by declaration (`grep -n '^func \|^type ' test/e2e/harness_test.go`), those are about 430, 305, and 170 lines.
The namespace and Pod helpers every scenario calls are about 200 more and no subject of the record's,
and a fourth file takes them,
because leaving them would keep `harness_test.go` near 800 lines, which is the size the trigger fired on.
What stays is the lifecycle the record gives `TestMain` (`docs/decisions/e2e-without-framework.md:30`)
and the shell it runs on: about 600 lines.
The record names the files, and the sizes are measured after the move, not estimated in it.

**The record keeps its decision and loses its stale measurements.**
`docs/README.md:24` says a decision record is immutable once accepted and superseded rather than edited,
and it describes no exception; this plan does not invent one.
What the repository does is narrower than editing and is already established:
`6dff39f` corrected one measurement in *Consequences* — "roughly a few hundred lines" became the file's name —
added a pointer under *Context* to the re-examination, and appended a *Revisit* section,
leaving the *Decision* line exactly as accepted.
Task 7 does the same thing for the same reason.
The split makes four measurements false:
*Consequences* names one harness file (`:28`),
and *Revisit* says 1672 lines, 59 functions, and 5223 scenario lines (`:37-40`, `:58`, `:63`)
where the tree gives 1702, 60, and 6447.
It also makes the closing sentences of *Revisit* false, which say the split is not done (`:66-70`).
Those are the only sentences the task touches; *Decision*, *Context*, and every line of reasoning stay.
The alternative, a superseding record, would restate a decision nothing has changed in order to correct a line count,
and would leave the accepted record telling a reader that `test/e2e/harness_test.go` is the harness after it stopped being.
`docs/README.md:24` is owed a revision describing what the repository actually does with a record's measurements;
that is a change to a documentation-lifecycle rule, it is not this plan's bullet, and *Risks* names it as owed.

**One changelog entry, for the document.**
The document is what a client parses (`docs/specs/gateway.md:1232-1234`),
and a parameter it gains is a change a client sees: `### Fixed`.
A workflow, a rule, a repository check, a test's skip, a test flag, and a file split change nothing a user of the gateway or the client meets,
and `CHANGELOG.md` carries none of them.
Correcting the three route-count sentences carries none either:
the routes have answered without a credential since they were written,
so what changes is a guide that described them wrongly and not the gateway.

**The plan closes naming the pull request.**
The merge rebases the branch and rewrites every hash,
so `Outcome:` and the roadmap's `Shipped:` line (`docs/plans/roadmap.md:337`) both name the pull request,
and the file is deleted by the commit after the merge, as the last plan was.

---

## Global Constraints

- **No new route, configuration key, chart value, Kubernetes call, or NATS permission.**
  Every request the gateway answers after this plan it answered before; the document describes a header it already read.
- **Every task that changes what a check or a test asserts shows it red first, or says why it cannot and what verifies it instead.**
  Task 2 runs the header walk red against the shipped document;
  task 4 shows the rule red on each occurrence it pins and red for a change to `routeTable`;
  task 5 shows the skip before the change and the pass after it;
  tasks 1, 3, and 7 have no red — a workflow, a paragraph, a move — and each says what verifies it;
  task 6's evidence is a full run under the flag; task 8 changes no behavior.
- **`-race` and `-count=1` on every red run.**
- **No jargon:** comments, commit messages, and documentation state the current fact, never this plan's ordering or a task name.
- Markdown prose uses semantic line breaks;
  run `semlf check` on every Markdown file and every Go file with doc comments a task writes or edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters — the hook refuses 50 or more —
  with a body that says what changed and why, one sentence per line under 120 characters, and no trailer of any kind
  ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
  A commit is finished when `git log --oneline -1` shows it and `git status --short` is clean,
  because the hook can refuse a message after `git commit` has already run;
  every validation block below ends with both.
- Every task ends with the same validation block before its commit:

```bash
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
```

---

## File Structure

```text
.github/workflows/check.yml                  # pull_request and push to main; the check job loses its event gate
docs/specs/gateway.md                        # the amendment block for the Continuous integration revision
internal/httpapi/openapi.json                # the IfMatch parameter component; the two policy writes reference it
internal/httpapi/openapi_test.go             # headerParameters; the header walk in TestOpenAPIDocumentParameters; the response header alone in TestOpenAPIDocumentHeaders
.agents/rules/900-design-and-review-loops.md # what runs mise run check, and where rule 800's mechanisms run
scripts/check-repo.py                        # check_route_counts over routeTable, the dispatch, and fourteen patterns
docs/api.md                                  # the two sentences that miscount the /v1 routes needing no credential
.agents/rules/100-project-map.md             # the third of those sentences
internal/pgo/rounds_test.go                  # the guard runs under -race, and both lifetimes are held to the last read
internal/pgo/race_off_test.go                # deleted
internal/pgo/race_on_test.go                 # deleted
docs/specs/pgo.md                            # the guard no longer skips; an amendment block
mise.toml                                    # -race on test:e2e
test/e2e/harness_test.go                     # the lifecycle TestMain owns and the shell it runs on
test/e2e/harness_nats_test.go                # NATS identity, users, server, stores
test/e2e/harness_config_test.go              # overlays, patches, gateway configuration, Secrets
test/e2e/harness_forward_test.go             # the standing gateway forwards, the test-app forward, and their refresh
test/e2e/harness_pods_test.go                # namespaces, Pod waits, the crash, poll
docs/decisions/e2e-without-framework.md      # the measurements the split makes false, in Consequences and Revisit
CHANGELOG.md                                 # one entry, for the document
docs/plans/roadmap.md                        # the item's checkboxes and Shipped line, in the closing task
docs/plans/close-the-unrun-gates.md          # this file
```

---

## 1. The unit gates run on every pull request and every push to `main`

Closes the roadmap bullet beginning *`.github/workflows/check.yml:1-13` runs `check`, lint, and unit tests on `push` only* (`docs/plans/roadmap.md:322-324`).

**Files:**
- Modify: `.github/workflows/check.yml`, `docs/specs/gateway.md`

**The decision, and why.**
*Decisions* settles the shape, the once-per-pull-request argument, and what is observed.

`.github/workflows/check.yml:2-8` becomes:

```yaml
on:
  pull_request:
  push:
    branches: [main]
jobs:
  check:
    runs-on: ubuntu-latest
```

The `check` job's steps (`:9-12`) are unchanged.
The `prose` job (`:13-21`) is unchanged and keeps `if: github.event_name == 'pull_request'`,
because `semlf --base origin/${{ github.base_ref }}` (`:21`) diffs against the base branch,
and `github.base_ref` is empty on a `push`.
`release.yml` runs its own `gates` job (`.github/workflows/release.yml:16-21`) and reads nothing from this file.

`docs/specs/gateway.md` *Amendments* gains a block at its end,
the shape every block before it has (`docs/specs/gateway.md:2623-2632`):
"Running the unit gates on every pull request amends the following text.",
an *Amended now* table with one row —
`docs/specs/gateway.md` | *Continuous integration* | the unit gates run on every pull request and every push to `main`, on one job with no event gate; why a same-repo pull request runs it once —
and an *Updated with the implementation* table with one row —
`.github/workflows/check.yml` | `pull_request` and `push` to `main` as the events; the `check` job loses its event gate; the `prose` job keeps its own, because it diffs against `github.base_ref`, which a push does not carry.
The revision itself is already in the tree (`8bee224`, `:2230`, `:2235-2238`);
the block is what that commit did not add.

- [x] **Name the check**

No test reads a workflow file.
What verifies this task is the workflow run on the pull request this plan opens, not the name in its checks list:
GitHub reports a job its `if:` skipped as a success, so `check` appearing there is no evidence the commands ran.
What is recorded, once the pull request exists:

- the run's URL, its event (`pull_request`), and the commit it tested;
- the log of the `check` job's one step,
  showing `mise run check && mise run lint && mise run test` (`.github/workflows/check.yml:12`) executed and passing;
- that no second run started from a `push` to the branch, which the `branches: [main]` filter is there to prevent;
- after the merge, the separate `push` run on `main`, which is a second run of the same job on a different ref.

The fork case is reasoned in *Decisions* and not run:
its event eligibility follows from the workflow file, and an observed fork run waits for a first fork contribution.

- [x] **Validate and commit**

```bash
semlf check docs/specs/gateway.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add .github/workflows/check.yml docs/specs/gateway.md
git commit -F <file holding: "ci: run the unit gates on pull requests" and a body saying a fork's pull request fired pull_request alone and ran none of the three gates, that the job now names both events and gates on neither, and why a same-repo pull request runs it once>
git log --oneline -1 && git status --short
```

---

## 2. The document declares `If-Match`, and the check walks header parameters

Closes the roadmap bullet beginning *`If-Match` is required by `PUT` and `DELETE` on the policy route* (`docs/plans/roadmap.md:325-328`).

**Files:**
- Modify: `internal/httpapi/openapi.json`, `internal/httpapi/openapi_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the component, the exact map, where `X-Request-Id` and `If-None-Match` stand, and what each test keeps.

`internal/httpapi/openapi_test.go` gains, after `queryParameters` (`internal/httpapi/openapi_test.go:55-73`):

```go
// headerParameters is the reviewed set of header parameters each operation
// declares: the conditional and idempotency headers its handler reads.
// An operation the map does not name declares none, which the check holds it to,
// so the map and the document have to change together and neither drifts alone.
// Credentials are outside it: internal/auth reads Authorization and the
// Sec-Fetch-* triple, which OpenAPI describes as a security scheme, not a
// parameter, and this document declares no scheme yet.
// X-Request-Id is absent on purpose: ServeHTTP reads it before any route resolves,
// its value decides nothing, and the document describes it once, on every response.
var headerParameters = map[string][]string{
	"PUT /v1/namespaces/{namespace}/services/{service}/pgo":          {"If-Match"},
	"DELETE /v1/namespaces/{namespace}/services/{service}/pgo":       {"If-Match"},
	"POST /v1/namespaces/{namespace}/services/{service}/collections": {idempotencyKeyHeader},
	// The console's asset route answers 304 to a tag it served.
	// compareConditional holds that relationship; this map holds the set.
	"GET /ui/{file}":  {"If-None-Match"},
	"HEAD /ui/{file}": {"If-None-Match"},
}
```

`TestOpenAPIDocumentParameters` (`:559-582`):
its comment gains "every header parameter an operation declares is in the reviewed set for that operation,
and an operation the set does not name declares none";
its body gains, after the query comparison (`:566-569`):

```go
		want = slices.Sorted(slices.Values(headerParameters[pair]))
		if got := parametersOf(t, doc, op, "header"); !slices.Equal(got, want) {
			t.Errorf("%s describes header parameters %v, want %v", pair, got, want)
		}
```

`TestOpenAPIDocumentHeaders` (`:609-638`):
the comment reads "reads the one header every answer carries: X-Request-Id names the request on every response of every operation",
the `create` constant (`:613`) and the block from `headers := parametersOf(t, doc, op, "header")` to its closing brace (`:629-636`) go,
and the response walk (`:617-628`) stays as it is.
The key's rule now lives in the map, which holds it more tightly:
the create describes exactly that header, and no other operation describes it.

`internal/httpapi/openapi.json`:
`components.parameters` gains, between `IdempotencyKey` (`:135-146`) and `IfNoneMatch` (`:147-155`), where name order puts it:

```json
      "IfMatch": {
        "description": "the revision the write or the delete is made at, as the ETag of the last read carries it: a quoted decimal whose digits fit an unsigned 64-bit integer. Any other form, * included, and any value past that range, is 400 invalid_parameter. Over an existing override it is required, and a write without it is 428 precondition_required; one naming a revision the policy has moved past is 412 precondition_failed, and so is a PUT that carries it where no override exists.",
        "in": "header",
        "name": "If-Match",
        "required": false,
        "schema": {
          "pattern": "^\"[0-9]+\"$",
          "type": "string"
        }
      },
```

The `pattern` is `ifMatchRE` (`internal/httpapi/pgo_policy.go:25`),
and the description carries the rest of what `parseIfMatch` accepts,
because the pattern admits digit strings no `uint64` holds and `parseIfMatch` refuses those (`:302-305`);
JSON Schema has no integer bound to put on a string, so the range is described rather than declared.
`required` is false because the first `PUT` must omit it (`:139-142`);
the four outcomes are the handlers' (`:139-142`, `:160-165`, `:221-226`).
The `put` and `delete` parameter arrays of the policy route each gain `{"$ref": "#/components/parameters/IfMatch"}` after `Service`,
at `:2330-2337` and `:2290-2297`.
`compareEncoding` (`internal/httpapi/openapi_test.go:276-294`) re-encodes objects with keys in name order, two-space indent, and a trailing newline,
and leaves arrays in the order they are written, so the file is written that way and the array's order is free.
The two descriptions that name the header (`:2289`, `:2329`) stay.

- [ ] **Write the map, and run it red**

The map and the walk first, the document untouched:

```bash
go test -race -count=1 ./internal/httpapi/ -run 'TestOpenAPIDocumentParameters|TestOpenAPIDocumentHeaders'
```

`TestOpenAPIDocumentParameters` reports `PUT /v1/namespaces/{namespace}/services/{service}/pgo describes header parameters [] want [If-Match]`
and the same for `DELETE`;
the create and the two asset operations pass, because the document already declares theirs;
`TestOpenAPIDocumentHeaders`, shrunk, passes.
That is the defect: a client that reads the document has no parameter to send where the handler requires one.
The exact set is shown load-bearing in the same run by what passes:
every other operation describes no header parameter, which the walk asserts for the first time.
What this run does not and cannot show is a handler reading an undescribed header:
the walk compares the map against the document, and both are maintained by hand.

- [ ] **Declare the header and say so**

The component and the two references, then the same command, green,
and `TestOpenAPIDocumentEncoding`, `TestOpenAPIDocumentReferences`, and `TestOpenAPIDocumentConditional` green with it:
the file re-encodes to itself, the two new references resolve, and no operation gained `If-None-Match`.

`CHANGELOG.md`, `### Fixed`:
**The OpenAPI document declares `If-Match` on the policy `PUT` and `DELETE`.**
The two writes read the header and the document named it in prose alone,
so a client built from the document had no parameter to send where the gateway answers `428` without one.
The document now carries the header with its grammar, a quoted decimal revision within an unsigned 64-bit range,
and the check holds every operation to exactly the header parameters it should declare.

- [ ] **Validate and commit**

```bash
semlf check internal/httpapi/openapi_test.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/httpapi/openapi.json internal/httpapi/openapi_test.go CHANGELOG.md
git commit -F <file holding: "fix(httpapi): declare If-Match in the document" and a body saying the two writes read a header the document only described, that the check now walks header parameters against a reviewed set per operation, and why X-Request-Id and the credential headers are in no operation>
git log --oneline -1 && git status --short
```

---

## 3. Rule 900 names what runs `mise run check`

Closes the roadmap bullet beginning *`.agents/rules/900-design-and-review-loops.md:32` says no CI invokes `mise run check`* (`docs/plans/roadmap.md:329-330`).
It follows task 1 because its sentence describes the workflow task 1 leaves.

**Files:**
- Modify: `.agents/rules/900-design-and-review-loops.md`

**The change.**
`.agents/rules/900-design-and-review-loops.md:32-35`, four lines, become:

```
`.github/workflows/check.yml` runs it on every pull request and every push to `main`,
ahead of the linter and the unit tests,
so a `Status:` or a link the check refuses never reaches `main` unnoticed;
run it locally all the same, because the workflow reports only once the commit exists.
The import greps of 800 run in the same script,
and its golden ClusterRole test runs with `mise run test`.
```

In the file, `800` keeps the link `:34` carries today.
Lines `:29-31` stay.
"Every push" would have been true before task 1 and false after it, which is why this task follows it.
The last two lines replace "The two mechanisms in 800 join the same task once Go code exists" (`:34-35`)
for the reason *Decisions* gives:
the grep is `check_clientgo_importers` (`scripts/check-repo.py:121-129`),
the golden test is `TestClusterRoleTuples` (`deploy/deploy_test.go:76`),
and the script's docstring says where the second one lives (`scripts/check-repo.py:24-25`).

- [ ] **Name the check**

Prose has no red test.
`mise run check` holds the file's links; `semlf check` holds its lines;
a reviewer reads the paragraph beside `check.yml` as task 1 left it.

- [ ] **Validate and commit**

```bash
semlf check .agents/rules/900-design-and-review-loops.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add .agents/rules/900-design-and-review-loops.md
git commit -F <file holding: "docs(rules): say what runs mise run check" and a body saying the rule claimed no CI invoked the check while check.yml had since 2026-08-23, and that the two mechanisms of rule 800 are named where they run>
git log --oneline -1 && git status --short
```

---

## 4. `mise run check` pins the route counts to the route table

Closes the roadmap bullet beginning *`docs/api.md:121,1002` states route counts in prose that nothing pins* (`docs/plans/roadmap.md:331-332`).

**Files:**
- Modify: `scripts/check-repo.py`, `docs/api.md`, `.agents/rules/100-project-map.md`

**The decision, and why.**
*Decisions* settles the scope — fourteen patterns, sixteen occurrences, five files, seven counts,
all read from `routeTable` — the three sentences that are wrong today, and what is left unpinned.

`scripts/check-repo.py` gains, before `check_hooks` (`scripts/check-repo.py:270`):

```python
ROUTE_TABLE_PATH = "internal/httpapi/routes.go"
ROUTE_DISPATCH_PATH = "internal/httpapi/server.go"
ROUTE_ENTRY_RE = re.compile(r'^\t\{"(/[^"]*)", (kind\w+)')
# The span of the request algorithm that answers before anything reads a
# credential: it opens where the readiness check stands and closes where the
# PGO steps begin. The kinds it names are read from there, never kept here.
ROUTE_OPEN_OPEN = "\tif !s.ready() {"
ROUTE_OPEN_CLOSE = "\tif rt.kind.isPGO() {"
ROUTE_OPEN_KIND_RE = re.compile(r"rt\.kind == (kind\w+)")
NUMBER_WORDS = dict(enumerate(
    "zero one two three four five six seven eight nine ten eleven twelve "
    "thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty".split()))

# One row per place the guides state a route count: the file, the sentence with
# {n} where the number word stands, the name of the count, and how many places
# in that file are expected to match. Every match is checked and the number of
# matches is asserted, so neither a reworded copy beside a matching one nor a
# sentence that disappears can leave a count unpinned.
ROUTE_COUNT_SENTENCES = (
    ("docs/api.md", "{n} routes live under `/v1`", "v1", 1),
    ("docs/api.md", "the {n} that name a Service or a Collection in the path", "named", 1),
    ("docs/api.md", "{n} more routes under `/v1` name neither", "unnamed", 1),
    # The opening parenthesis is part of both places this matches, and keeps the
    # pattern off the credential sentences two rows down.
    ("docs/api.md", "one of the {n} `/v1` routes (", "v1", 2),
    ("docs/api.md", "The {n} PGO routes", "pgo", 1),
    ("docs/api.md", "Two of the {n} take query parameters", "pgo", 1),
    ("docs/api.md", "{n} `/v1` routes that require no credential", "open", 2),
    ("docs/api.md", "The {n} routes exist only when the browser block is configured", "auth", 1),
    ("docs/api.md", "under `/auth/`, not one of the {n} routes", "auth", 1),
    ("docs/configuration.md", "the {n} `/auth/` routes", "auth", 1),
    ("docs/deployment.md", "The {n} `/auth/` routes", "auth", 1),
    (".agents/rules/100-project-map.md", "{n} `/v1` routes with no authentication step", "open", 1),
    # The comment over the span open_kinds reads, so the code's own count moves with it.
    ("internal/httpapi/server.go", "The {n} /v1 routes with no authentication step", "open", 1),
    (".agents/rules/100-project-map.md", "{n} routes present only when `ui.enabled`", "console", 1),
)


def open_kinds(root):
    """The kinds the request algorithm answers before the credential step.

    Read from internal/httpapi/server.go rather than kept here, so a route that
    joins them moves the count instead of slipping past it. A span that does not
    resolve, or resolves to nothing, is an error and never an empty set.
    """
    text = (root / ROUTE_DISPATCH_PATH).read_text()
    if text.count(ROUTE_OPEN_OPEN) != 1:
        return None
    start = text.index(ROUTE_OPEN_OPEN)
    end = text.find(ROUTE_OPEN_CLOSE, start)
    if end < 0:
        return None

    return set(ROUTE_OPEN_KIND_RE.findall(text[start:end])) or None


def route_counts(root, open_kinds):
    """Count routeTable's entries, and name the ones no group claims.

    Every group is a property of the entry: the template for the path groups,
    the kind for the routes that answer before the credential step.
    """
    entries = []
    inside = False
    for line in (root / ROUTE_TABLE_PATH).read_text().splitlines():
        if line.startswith("var routeTable = "):
            inside = True
        elif inside and line == "}":
            break
        elif inside:
            match = ROUTE_ENTRY_RE.match(line)
            if match:
                entries.append((match.group(1), match.group(2)))
    v1 = [t for t, _ in entries if t.startswith("/v1/")]
    named = [t for t in v1 if "{service}" in t or "{id}" in t]
    auth = [t for t, _ in entries if t.startswith("/auth/")]
    console = [t for t, _ in entries if t == "/" or t.startswith("/ui/")]
    counts = {
        "v1": len(v1),
        "named": len(named),
        "unnamed": len(v1) - len(named),
        "pgo": len([t for t in v1 if "/pgo" in t or "/collections" in t]),
        "auth": len(auth),
        "console": len(console),
        "open": len([t for t, kind in entries if t.startswith("/v1/") and kind in open_kinds]),
    }
    unclassified = [t for t, _ in entries if t not in v1 and t not in auth and t not in console]
    kinds = {kind for _, kind in entries}
    unrouted = sorted(k for k in open_kinds if k not in kinds)

    return counts, unclassified, unrouted


def sentence_pattern(sentence):
    """The sentence as a pattern whose one free part is its number word.

    Every run of spaces matches a line break, because the prose here uses
    semantic line breaks and a sentence wraps at a clause boundary.
    """
    parts = [re.sub(r"(?:\\?\s)+", r"\\s+", re.escape(part)) for part in sentence.split("{n}")]

    return re.compile(r"([A-Za-z]+)".join(parts))


def check_route_counts(root):
    """Hold every route count the guides state to the route table.

    Each count is read from internal/httpapi/routes.go, and the credential-free
    one from the dispatch in internal/httpapi/server.go, so a route added to
    either turns the prose red rather than the check stale. The sentence table is
    the one list this check keeps rather than reads, and it fails loudly instead
    of going quiet.
    """
    kinds = open_kinds(root)
    if kinds is None:
        return [f"{ROUTE_DISPATCH_PATH}: the span answering before the credential step did not resolve; the open route count is unchecked"]
    counts, unclassified, unrouted = route_counts(root, kinds)
    if not counts["v1"]:
        return [f"{ROUTE_TABLE_PATH}: no routeTable entry found; the route counts are unchecked"]
    bad = [f"{ROUTE_TABLE_PATH}: {t} is in no counted group; extend route_counts" for t in unclassified]
    bad += [f"{ROUTE_DISPATCH_PATH}: {k} answers before the credential step and names no route in {ROUTE_TABLE_PATH}" for k in unrouted]
    for name, value in sorted(counts.items()):
        if value not in NUMBER_WORDS:
            bad.append(f"{ROUTE_TABLE_PATH}: the {name} count is {value}, which this check has no word for; extend NUMBER_WORDS")
    if bad:
        return bad
    for path, sentence, name, occurrences in ROUTE_COUNT_SENTENCES:
        text = (root / path).read_text()
        hits = list(sentence_pattern(sentence).finditer(text))
        if len(hits) != occurrences:
            bad.append(f"{path}: {len(hits)} places match {sentence!r}, want {occurrences}; pin the reworded sentence in check_route_counts")
            continue
        for hit in hits:
            if hit.group(1).lower() != NUMBER_WORDS[counts[name]]:
                number = text.count("\n", 0, hit.start()) + 1
                bad.append(f"{path}:{number}: says {hit.group(1)!r} where {ROUTE_TABLE_PATH} declares {counts[name]} ({name})")

    return bad
```

`main()` (`:287-308`) gains `errors.extend(check_route_counts(root))` after `check_pkce_override_name`,
and the module docstring's list (`:4-21`) gains
"Every route count check_route_counts pins in the guides and the project map equals the count routes.go declares."
The docstring says "pins" and not "states", because the check covers the counts a route table's entry yields
and not every number the guides put beside the word "routes";
what it leaves out is listed under *Risks and What This Plan Does Not Cover*.
The counts today, read against the table's twenty-one entries (`internal/httpapi/routes.go:47-73`):
fifteen `/v1`, nine named, six unnamed, seven PGO, three `/auth/`, three console, two open.
"Fifteen" at `docs/api.md:67` and "Six" at `:81` open their sentences; the comparison lowercases the captured word.

The same task corrects the three places that state the open count wrongly, because the rule pins what they say:

- `docs/api.md:84` — "the one `/v1` route that requires no credential" of `GET /v1/auth`,
  six lines above `:90` saying `/v1/openapi.json` "is served from the binary and takes no credential";
- `docs/api.md:844` — "The one `/v1` route that requires no credential:", the same claim opening its section;
- `.agents/rules/100-project-map.md:139` — "the only `/v1` route with no authentication step".

Each becomes a statement of the two `internal/httpapi/server.go:504-512` answers, `kindAuth` and `kindOpenAPI`,
carrying the fragment the rule pins: "two `/v1` routes that require no credential" in the guide,
"two `/v1` routes with no authentication step" in the project map.
`docs/api.md:84` is an appositive to `GET /v1/auth` inside a three-clause list (`:82-85`),
so the correction is to the list and not to that line alone; one shape that works is
"…covers `GET /v1/auth`, and `GET /v1/openapi.json` serves the document described just below;
they are the two `/v1` routes that require no credential."
The other two keep their subject, which is one route each,
so both say "One of the two":
`docs/api.md:844` opens the section on `GET /v1/auth`,
and `.agents/rules/100-project-map.md:139` still says the command-line design adds one route.
The wording is the implementer's, provided the pinned fragment survives it
and no rewrite puts "one of the <word> `/v1` routes (" anywhere, which is a different row's pattern.
The check is what confirms all three: it is red on them before and green after, with nothing else changed.

- [ ] **Write the rule, and show it red**

```bash
mise run check
```

fails on the three sentences before they are corrected —
`docs/api.md: 0 places match ... want 2` and `.agents/rules/100-project-map.md: 0 places match ... want 1` —
and passes once they are, because every other count is right today.
The rule is then shown load-bearing occurrence by occurrence, each mutation ending with the file restored:

```bash
sed -i '506s/of the seven/of the eight/' docs/api.md && mise run check; git checkout docs/api.md
sed -i '121s/one of the fifteen `\/v1` routes (/one of sixteen routes under `\/v1` (/' docs/api.md && mise run check; git checkout docs/api.md
sed -i '/^Six more routes under `\/v1` name neither:$/d' docs/api.md && mise run check; git checkout docs/api.md
sed -i '0,/{"\/v1\/limits"/s//{"\/v1\/probe", kindLimits, []string{http.MethodGet}},\n\t{"\/v1\/limits"/' internal/httpapi/routes.go && mise run check; git checkout internal/httpapi/routes.go
sed -i 's/\tif rt.kind == kindOpenAPI {/\tif rt.kind == kindLimits {\n\t\ts.serveOpenAPI(w, r, q)\n\n\t\treturn\n\t}\n\tif rt.kind == kindOpenAPI {/' internal/httpapi/server.go && mise run check; git checkout internal/httpapi/server.go
sed -i 's/\tif !s.ready() {/\tif !s.isReady() {/' internal/httpapi/server.go && mise run check; git checkout internal/httpapi/server.go
```

The first reports `docs/api.md:506: says 'eight' where internal/httpapi/routes.go declares 7 (pgo)`,
which is the occurrence a rule matching only the `:501` denominator would have missed.
The second rewords one of the two places `one of the {n} \`/v1\` routes (` matches and leaves the other alone;
it reports `1 places match ... want 2`,
which is the cardinality assertion doing the work a "some line matched" rule would not.
The third reports the sentence missing.
The fourth changes `routeTable` rather than prose and turns four occurrences red at once —
`:67`, `:81`, `:121`, and `:1002` — which is the direction the rule exists for.
The fifth gives a routed kind an answer before the credential step,
and the open count moves to three: `docs/api.md:84`, `:844`, `.agents/rules/100-project-map.md:139`,
and the comment at `internal/httpapi/server.go:504` all report `says 'two' ... declares 3 (open)`,
which is the count the check reads from the dispatch rather than keeps.
The sixth renames the span's opening anchor and reports
`the span answering before the credential step did not resolve`,
so a restructured dispatch stops the check rather than letting it answer from a stale set.
Before the commit, every mutation is restored and `git status --short` shows only the intended change.
The wrong implementation is prose that keeps saying fifteen after a sixteenth `/v1` route lands,
which is what `docs/api.md:121` and `:1002` do today by luck.

- [ ] **Validate and commit**

```bash
python3 -m py_compile scripts/check-repo.py
semlf check docs/api.md .agents/rules/100-project-map.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add scripts/check-repo.py docs/api.md .agents/rules/100-project-map.md
git commit -F <file holding: "build(check): pin the route counts to routes.go" and a body saying which places are held, that each count is read from routeTable and the open count from the kind field, that three sentences said one route needs no credential where the gateway answers two, and why the specs, the changelog, and the four listing routes are not held>
git log --oneline -1 && git status --short
```

---

## 5. The heap-delta guard runs under `-race`

Closes the roadmap bullet beginning *`TestRoundsDecodeHeapDelta` skips under `-race`* (`docs/plans/roadmap.md:320-321`),
which is ticked and not done.

**Files:**
- Modify: `internal/pgo/rounds_test.go`, `docs/specs/pgo.md`
- Delete: `internal/pgo/race_off_test.go`, `internal/pgo/race_on_test.go`

**The decision, and why.**
*Decisions* settles that the skip goes rather than a second run being added, what the assertion measures,
and why the earlier numbers are what prompted the task rather than its evidence.
The measurement below is this task's, and it is what decides the change.

`internal/pgo/rounds_test.go:843-851`:
the comment's last two lines, "It is skipped under -race, whose allocator accounting makes the delta meaningless" (`:846-847`), go,
and the comment says instead that the guard measures retained heap,
what the decoded profile keeps live across a collection, and not the peak the decoder reaches;
the three lines of `if raceEnabled { t.Skip(...) }` (`:849-851`) go with them.
`runtime.KeepAlive(plain)` joins `runtime.KeepAlive(parsed)` after the final `runtime.ReadMemStats(&after)` (`:868-869`),
so both the decompressed input and the decoded profile are live through the measurement:
today only `parsed` is held, and `len(plain)` in the failure message (`:874`) does not keep its backing array alive,
so a collection of the input can be subtracted from what reads as the decoder's footprint.
`race_off_test.go` and `race_on_test.go`, which exist to define `raceEnabled` (`internal/pgo/race_off_test.go:6`, `internal/pgo/race_on_test.go:8`), are deleted;
nothing else reads the constant (`grep -rn raceEnabled internal/` finds the three files and no other).

`docs/specs/pgo.md` *Unit* (`docs/specs/pgo.md:3496-3497`):
"a regression guard, skipped under `-race`, parses a fixture profile and asserts the heap delta
(`runtime.ReadMemStats` before and after) is under `decodeFactor × len(fixture)`"
becomes
"a regression guard parses a fixture profile and asserts the heap the parsed profile retains
(`runtime.ReadMemStats` before and after, with a collection between)
is under `decodeFactor × len(fixture)`, under `-race` like every other test".
*Amendments* (`:4026`) gains a block at its end, the shape of the ones before it:
"Running the decoder's heap-delta guard under the race detector amends the following text.",
one *Amended now* row —
`docs/specs/pgo.md` | *Unit* | the guard runs under `-race`, and what it bounds is retained heap rather than peak decoding memory —
and one *Updated with the implementation* row —
`internal/pgo` | the skip, the two build-tagged constants that carried it, and the input's lifetime through the measurement.

- [ ] **Record what the skip does today**

Before anything changes:

```bash
go test -race -count=1 -run '^TestRoundsDecodeHeapDelta$' -v ./internal/pgo/
```

prints `--- SKIP: TestRoundsDecodeHeapDelta` with the allocator sentence (`internal/pgo/rounds_test.go:850`).
`mise run test` passes no `-v` (`mise.toml:34`), so the skip is silent on every ordinary run
and nothing about it reaches a reader who is not looking for it.
That output goes into the commit body: it is the evidence the guard never ran.

- [ ] **Hold both lifetimes, drop the skip, and measure**

The skip is what stops the measurement: `go test -race` skips the test while it stands,
so it comes out before any number under the flag can be read, together with the two files that carry `raceEnabled`.
The `KeepAlive` goes in with it, so the measurement is taken on a body that holds what it claims to measure.
The delta is then read by adding a temporary `t.Logf` beside the assertion and running the test in a process of its own,
six times each way, with nothing else building:

```bash
go test -count=1 -run '^TestRoundsDecodeHeapDelta$' -v ./internal/pgo/
go test -race -count=1 -run '^TestRoundsDecodeHeapDelta$' -v ./internal/pgo/
```

Then the same delta under the package's own suite, where the guard actually runs,
because the heap it starts from is whatever the tests before it left:

```bash
go test -race -count=1 -v ./internal/pgo/
```

Both are run under the toolchain `mise.toml:2` pins, which `mise exec` selects,
and the Go version, `GOARCH`, and `GOMAXPROCS` of the run go into the commit body with the numbers.
What the numbers decide:
whether the delta under `-race` stays under `config.PGODecodeFactor × len(plain)` with margin the spread does not eat.
If it does, the change stands as made and the same command now prints `--- PASS`.
If it does not, the skip and its two files are restored and the task stops with the numbers,
rather than the factor being changed,
because `PGODecodeFactor` also sizes the gateway's own memory (`internal/config/config.go:552`)
and moving it is a change to that contract and not a test repair.
Whether a guard that cannot run under the detector is worth keeping at all is decided then, with the numbers in hand.

- [ ] **Prove the bound is load-bearing**

The bound is load-bearing as it was written:
`config.PGODecodeFactor` is `8` (`internal/config/config.go:529`), the fixture is 516,906 bytes plain,
so the bound is 4,135,248 against a measured delta the step above records;
a factor of `7` in its place would put the bound at 3,618,342 and fail the test,
which one temporary edit and the same command with `-count=1` confirm before the constant is restored.
The temporary `t.Logf` and the temporary factor are both gone before the commit,
and `git status --short` shows only `rounds_test.go`, the two deletions, and the spec.

- [ ] **Validate and commit**

```bash
semlf check internal/pgo/rounds_test.go docs/specs/pgo.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/pgo/rounds_test.go internal/pgo/race_off_test.go internal/pgo/race_on_test.go docs/specs/pgo.md
git commit -F <file holding: "test(pgo): run the heap-delta guard under -race" and a body saying the guard skipped under the flag every command passes and so never ran, what the delta measures under the detector, and that the spec no longer says it skips>
git log --oneline -1 && git status --short
```

---

## 6. The end-to-end suite runs under `-race`

No roadmap bullet; the same defect as task 5, named in *Decisions*.

**Files:**
- Modify: `mise.toml`

**The decision, and why.**
*Decisions* settles the measurement and that a reported race is fixed here rather than deferred.

`mise.toml:41` becomes `run = "go test -tags e2e -race -count=1 -timeout 40m ./test/e2e/..."`.
The flag instruments the test binary and nothing it builds:
`buildImages` runs `ko` for the gateway and the test application (`test/e2e/harness_test.go:366-382`),
which is unchanged,
so what the detector watches is the harness and the scenarios — the port-forwards, the watches, the browser session's recorder — and not the gateway.
Every consumer of `mise run test:e2e` takes the flag:
`e2e.yml` (`.github/workflows/e2e.yml:68`), `release.yml` (`.github/workflows/release.yml:40`), and rule 500's run before a pull request.
The requirement is rule 300's alone: `-race` is always on (`.agents/rules/300-testing.md:16`).
*Layers* of `gateway.md` (`docs/specs/gateway.md:2032-2033`) calls the suite plain `go test` under the `e2e` tag
and says nothing about the flag, so it is not what this task is answering and it does not change either.

- [ ] **Run the suite under the flag**

On this machine, with a Chromium installed and the `current` lane:

```bash
time mise run test:e2e
```

The last full run without the flag took about 700 seconds;
the detector slows the test binary, and most of the suite's time is spent waiting on the cluster,
so the growth is bounded by the harness's own work.
The wall time goes into the commit body and the pull request description.
If the run passes 30 minutes, `-timeout 40m` no longer holds a margin, and the same commit raises it and says so.
The task is finished when this run passes under the flag, and not before.

- [ ] **If the detector reports a race**

A reported race is a defect in the harness or the gateway, which is what rule 300 says,
so it is reproduced and fixed in this task and the suite is rerun under the flag.
The report — both goroutines' stacks, the scenario it ran under, and the lane — is kept whole
and goes into the pull request description beside the fix, because it is what makes the fix reviewable.
The commit lands only after a passing flagged run.
Restoring the uninstrumented command is not an outcome this task has:
the flag is never left off as finished work, and rule 300 is never reworded to match the tooling.
A race in a declaration task 7 moves is fixed before that move, so the move stays a move.
If a fix turns out to need a design change larger than this plan,
the task stops with the race reported and the plan does not reach `Done`;
whether that work is carved out is decided then, and this plan does not pre-approve it.

- [ ] **Validate and commit**

```bash
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add mise.toml
git commit -F <file holding: "test(e2e): run the suite under -race" and a body saying the command passed no -race where rule 300 says it is always on, what the flag instruments and what it does not, and the wall time of the run that proved it>
git log --oneline -1 && git status --short
```

---

## 7. The harness is five files along its subjects

Closes the roadmap bullet beginning *`docs/decisions/e2e-without-framework.md` records that its size trigger fired* (`docs/plans/roadmap.md:333-334`).
Last, in its own commit, because it is the largest diff and no other task depends on it.
It is required: it is the only task that closes its bullet, and task 8 ticks that bullet.

**Files:**
- Modify: `test/e2e/harness_test.go`, `docs/decisions/e2e-without-framework.md`
- Create: `test/e2e/harness_nats_test.go`, `test/e2e/harness_config_test.go`, `test/e2e/harness_forward_test.go`, `test/e2e/harness_pods_test.go`

**The decision, and why.**
*Decisions* settles the four subjects and what the record keeps.

Each new file opens with `//go:build e2e`, `package e2e`, and the imports its declarations need, as `browser_test.go` does;
`goimports`, enabled in `.golangci.yml` under `formatters`, reports what `harness_test.go` no longer uses.
Every declaration moves whole, with its doc comment and its lint directives, and no line inside one changes;
the package is one package, so no caller changes.
Import lists and the file preambles necessarily differ, and are the only lines the move writes:
`harness_forward_test.go` takes the `httpstream` import that `openForward` needs (`test/e2e/harness_test.go:653`)
and the `//nolint:staticcheck` annotation on it (`:38`) with it.
The line numbers below are today's, and the sizes are estimates from them; `wc -l` after the move is the fact.

`harness_nats_test.go`, about 430 lines, is `natsIdentity` and its comment through `connections` (`test/e2e/harness_test.go:1139-1566`):
`natsIdentity`, `newNATSIdentity`, `serverConf`, `natsUser`, `user`, `gatewayPermissions`, `without`,
`natsServer`, `natsURL`, `deployNATS`, `close`, `provisionStores`, `purgeStores`, `keys`, `objects`, `recordsOf`, `connections`.
`loadNATSImage` and its comment (`:383-386`) stay: it is image loading, which `TestMain` owns (`:231`).

`harness_config_test.go`, about 305 lines, is `Apply` through `configPatch` (`:753-974`) —
`Apply`, `patch`, `apply`, `gatewayConfigOptions`, `gatewayConfig`, `credsMountPatch`, `pgoGatewayMemoryLimit`, `memoryLimitPatch`, `configPatch` —
and `applyConfigMap` through `applyAuthSecret` (`:1568-1650`).

`harness_forward_test.go`, about 170 lines, is `forwardGateways`, `forward`, and `openForward` (`:549-677`),
`ForwardTestApp` (`:976-990`), and `RefreshGateways` (`:1679-1702`).
`gatewayPods` and `dumpGateway` (`:522-547`) stay.
`gatewayPods` is read by three declarations here — `deployGateway` (`:517`), `forwardGateways` (`:553`),
and `RefreshGateways` (`:1691`), two of which this task moves out —
and by four scenario-side callers: `scenarioRBAC` (`test/e2e/scenarios_test.go:1047`),
`severAPIConnections` (`:1285`), `survivingGateway` (`test/e2e/scenarios_pgo_test.go:978`),
and `scenarioPGOClusterRole` (`:1083`).
`dumpGateway` has one caller, `TestMain` (`test/e2e/harness_test.go:274`).

`harness_pods_test.go`, about 200 lines, is `Namespace` through `waitNamespaceGone` (`:679-751`),
`WaitPodReady` through `poll` (`:992-1063`), `CrashGateway` (`:1065-1095`), and `waitOnePod` (`:1652-1677`).

`harness_test.go` keeps what `TestMain` owns and the shell it runs on, about 600 lines:
the imports and constants, `Harness`, `harness`, `runners`, `TestMain`, `TestScenarios`, `laneFromEnv`, `registry`, `clusterState`,
`buildImages`, `loadNATSImage`, `loadImage`, `ociIndexMembers`, `dropOCIIndex`, `connect`,
`deployGateway`, `dumpGateway`, `gatewayPods`, `kind`, `kubectl`, `run`, and `output`.

`docs/decisions/e2e-without-framework.md` keeps its *Decision*, its *Context*, and every line of reasoning.
What changes is the measurements the split makes false, which is what `6dff39f` did to this record before,
and *Decisions* argues why.
*Consequences* (`docs/decisions/e2e-without-framework.md:28`), "The harness is project code, `test/e2e/harness_test.go`",
becomes "The harness is project code, the `test/e2e/harness_*_test.go` files".
Under *Revisit*, "The size trigger has fired." (`:36`) stays;
`:37-40` say what the file was and what it is:
"`test/e2e/harness_test.go` was 1672 lines carrying 6 types and 59 top-level functions and methods when the trigger was recorded,
and is <n> lines after the split below, with <n> more across the four files beside it";
`:58` carries the measured lifecycle count in place of 330 and 1672;
`:63` carries the measured scenario count in place of 5223 —
6447 today over the five scenario files, `wc -l test/e2e/scenarios*_test.go`,
which is the glob that includes the 1431-line `scenarios_test.go`;
`scenarios_*_test.go` matches only the other four and totals 5016.
`:66-70` become
"What made it large is subject matter that is not cluster lifecycle, and it is split by subject:
NATS identity, users, server deployment, and store provisioning are `harness_nats_test.go`;
gateway configuration rendering and Secret application are `harness_config_test.go`;
port forwarding is `harness_forward_test.go`;
the namespace and Pod helpers every scenario calls are `harness_pods_test.go`;
and `harness_test.go` keeps the lifecycle `TestMain` owns."
Every number written is one the implementer measured with the command the record names.

- [ ] **Move, and name the check**

A move has no red test.
What verifies it is the declaration inventory before and after, not a diff summary:

```bash
diff <(git show HEAD:test/e2e/harness_test.go | grep '^func \|^type ' | sort) \
     <(cat test/e2e/harness*_test.go | grep '^func \|^type ' | sort)
```

It prints nothing: the five files after the move declare exactly what the one file declared before it,
and `git diff --stat` alone cannot say that, because it counts lines and not declarations.
Each moved body is compared to the one it replaced in the same review, declaration by declaration.
Beside it: the package compiles and vets under its tag,
the linter's `goimports` finds every import used,
and the suite runs once in *Validation* with the flag task 6 set.

```bash
go vet -tags e2e ./test/e2e/
```

- [ ] **Validate and commit**

```bash
go vet -tags e2e ./test/e2e/
semlf check test/e2e/harness_test.go test/e2e/harness_nats_test.go test/e2e/harness_config_test.go test/e2e/harness_forward_test.go test/e2e/harness_pods_test.go docs/decisions/e2e-without-framework.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add test/e2e/harness_test.go test/e2e/harness_nats_test.go test/e2e/harness_config_test.go test/e2e/harness_forward_test.go test/e2e/harness_pods_test.go docs/decisions/e2e-without-framework.md
git commit -F <file holding: "refactor(e2e): split the harness by subject" and a body naming the four files and what each holds, saying no declaration changed, and that the decision record's numbers are the measured ones>
git log --oneline -1 && git status --short
```

---

## 8. Close the plan

**Files:**
- Modify: `docs/plans/close-the-unrun-gates.md`, `docs/plans/roadmap.md`

Line 3 becomes `**Status:** Done` and line 4 `**Outcome:** pull request #<n> ...`,
naming the pull request that carries the seven tasks above,
and in the same commit the roadmap item's five open checkboxes (`docs/plans/roadmap.md:322-334`) are ticked,
its first (`:320`) stays ticked and is now true,
and its `Shipped:` line (`:337`) names that pull request,
the shape the previous plan's closing commit gave it (`8d44a2c`).
Every one of the seven tasks is done before this one runs;
the plan does not reach `Done` with the end-to-end flag off or the harness unsplit,
because the flag's task is finished only by a passing flagged run
and the split is what its roadmap bullet's tick claims.
The pull request is named rather than a commit because the merge rebases this branch onto `main`
and rewrites every hash on it;
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) admits a pull request there for that reason,
and `check_status` in [`check-repo.py`](../../scripts/check-repo.py) requires `**Outcome:** ` followed by text on line 4 (`scripts/check-repo.py:95-98`).
This commit does not delete the plan.
The deletion is the next commit that touches the file, after the merge,
the protocol [`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records;
it deletes this file and rewrites every link that cited it, which `check_links` enforces, and changes nothing else.
`grep -rn close-the-unrun-gates --include='*.md' .` finds the links.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/close-the-unrun-gates.md docs/plans/roadmap.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add docs/plans/close-the-unrun-gates.md docs/plans/roadmap.md
git commit -F <file holding: "docs: close the unrun gates plan" and a body saying the item's six bullets are done and its Shipped line names the pull request>
git log --oneline -1 && git status --short
```

---

## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite, on a machine with a Chromium installed:

```bash
mise run test:e2e
```

It is required, and it runs under `-race`, which task 6 set.
Task 7 rewrites the suite's own files, and task 6's flag reaches CI through `mise.toml`;
the run after the last task is the evidence for both.
Task 6's own run is a second full run, earlier, and is what finishes that task;
the two together are about an hour on this machine, and neither is skipped for the other.
[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md) lists eight packages
that need the suite on the `current` lane before a pull request, and `internal/pgo` is among them (`:73-74`).
The change to `internal/pgo` independently requires the current-lane run;
the harness and flag changes also require it.

What the pull request itself proves:
the `check` job's run on the `pull_request` event, with the log of the step that ran the three commands —
not the name in the checks list, which a skipped job also earns.
The fork case is reasoned and not observed:
its event eligibility follows from the workflow file, and an observed fork run waits for a first fork contribution.

Report what ran, how long the flagged run took, and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **A fork's pull request is reasoned, not run.**
  The event model says `pull_request` fires in this repository for a fork's contribution and the ungated job answers it;
  the first fork contribution is where that is observed,
  and the same-repo pull request this plan opens is the evidence available now.
- **The end-to-end suite may report a race, and fixing it may be larger than this plan.**
  A race is reproduced and fixed inside task 6 and the suite rerun under the flag;
  the flag is not left off as a finished outcome.
  If the fix needs a design change this plan does not carry, task 6 stops with the race reported,
  the plan does not reach `Done`, and whether that work is carved out is decided then.
- **The flagged run's length is unknown until measured.**
  `-timeout 40m` is the bound today; task 6 raises it in the same commit if a run passes 30 minutes,
  and the CI lanes take the same flag through `mise.toml`.
- **The heap-delta guard is measured on one machine, and it bounds retained heap rather than peak decoding memory.**
  Task 5 measures on linux/amd64 under the pinned Go, in an isolated process and under the package's suite;
  the workflows run the same architecture and toolchain, and no other is covered.
  If a run elsewhere fails the bound under `-race`, the failure carries the delta and the bound,
  and what it leaves open is whether the decoder regressed or the machine differs, which is a person's call.
  Raising `PGODecodeFactor` is not a test repair:
  it also sizes the gateway's own memory (`internal/config/config.go:552`),
  so moving it changes that contract and belongs in its own change.
- **The route-count rule pins wording, and its pattern table can go stale.**
  A reworded occurrence turns the check red until its row is updated; that is the design, and the failure names the row.
  The pattern table is the one list the check keeps rather than reads,
  and it fails loudly: a row matching a different number of places than it declares is an error naming the row.
  Every count it compares against is read — six from `routeTable`'s templates,
  and the credential-free one from the span of `internal/httpapi/server.go` that answers before the credential step.
- **The four listing routes stay unpinned, and so do the PGO query splits.**
  "Four routes" at `docs/api.md:280` and `docs/console.md:8` name the console's group, which no template shape yields.
  "Two of the seven" and "the other five" at `docs/api.md:506-507` split the PGO group by query parameters,
  which is a fact of `queryParameters` and not of the route table; the seven is pinned and the two and the five are not.
- **`X-Request-Id` is not a parameter of any operation.**
  A client generated from the document sets it only through whatever its generator offers for arbitrary headers;
  the guide says a request may choose its own (`docs/api.md:163`),
  and the response header's description says the value comes back.
- **The document declares no security scheme, and this plan does not add one.**
  Authentication reads `Authorization` on every `/v1` route (`internal/auth/basic.go:130`, `internal/auth/oidc.go:146`)
  and the `Sec-Fetch-*` triple on the browser path (`internal/auth/browser.go:154`, `:191`),
  and `internal/httpapi/openapi.json` carries no `securitySchemes` and no `security` key
  (`grep -n '"Authorization"\|securitySchemes\|"security"'` finds nothing),
  so a client generated from the document has no credential to send.
  That is owed, and it is a document change with client-visible consequences that no bullet of this item asks for.
  `headerParameters` is scoped to parameters so that the gap stays visible instead of being papered over.
- **The header check compares two descriptions, not a handler.**
  A handler that starts reading a header while the map and the document both stay as they are passes;
  what the check forbids is the map and the document drifting apart.
  Nothing in this repository reads handler source for the headers it touches, and this plan adds nothing that does.
- **The harness split is a move, and a move can drop an import or a caller.**
  `go vet -tags e2e`, the linter's `goimports`, the declaration inventory task 7 compares,
  and the suite's run in *Validation* are what catch that; no declaration's body changes.
- **A decision record's measurements are corrected in place.**
  The record's *Decision*, *Context*, and reasoning stay as accepted;
  what changes is the line counts and the file name the split makes false,
  which is what `6dff39f` already did to this record.
  `docs/README.md:24` describes records as immutable and superseded rather than edited, and describes no such correction.
  A revision of that line, to say what the repository does with a record's measurements, is owed and is not made here.
- **`GOFLAGS=-buildvcs=false` in a worktree is an earlier run's recipe.**
  It is not exercised by this plan; an implementer in the main checkout never needs it.
- **The plan's deletion is not one of its tasks.**
  The closing task leaves the finished document in the tree under the lifecycle checks;
  the commit that deletes it and rewrites its links follows the merge, as the previous plan's did.

---

## Self-Review

- Bullet coverage, one line each:
  the unit gates on `pull_request` (task 1, with the spec's amendment block);
  `If-Match` declared and the header-parameter walk (task 2);
  rule 900's sentence (task 3, after task 1);
  the route counts pinned and the three miscounts corrected (task 4);
  the heap-delta guard under `-race` (task 5, the ticked bullet that is not built);
  `-race` on the end-to-end suite (task 6, no bullet);
  the harness split and the record's stale measurements (task 7, last and required).
- Where the roadmap's text did not match the code, and what this plan says instead:
  the first bullet is ticked (`docs/plans/roadmap.md:320`) while the guard still skips (`internal/pgo/rounds_test.go:849-851`)
  and every command still passes `-race`, so task 5 exists;
  the roadmap cites `rounds_test.go:818-821`, `mise.toml:33`, and `docs/specs/gateway.md:2098-2103`,
  which are `:849-851`, `:34`, and `:2226-2238` today;
  it says the harness is 1,676 lines and the decision record says 1672 with 59 functions and 5223 scenario lines,
  where `wc -l` and `grep -c '^func '` give 1702, 60, and 6447;
  the roadmap names two route-count lines and the guides, the project map, and the dispatch hold sixteen occurrences over seven counts,
  so task 4 pins all fifteen and corrects the three that are wrong;
  the skip's comment says the detector makes the delta meaningless,
  and the measurement that prompted this plan says otherwise, which task 5 re-derives with both lifetimes held;
  the spec revision on this branch (`8bee224`) added no amendment block, so task 1 adds it.
- Current-source facts this plan rests on, each confirmed by reading the file:
  `check.yml` is 21 lines with `on:` at `.github/workflows/check.yml:2-4`, the `check` gate at `:7`,
  the steps at `:10-12`, the `prose` gate at `:14`, and `semlf --base` at `:21`;
  `e2e.yml` names its events at `.github/workflows/e2e.yml:2-19` and runs `mise run test:e2e` at `:68`;
  `release.yml` runs the gates at `.github/workflows/release.yml:21` and the suite at `:40`;
  `mise.toml` pins Go at `:2`, runs `go test -race ./...` at `:34`, and the suite without the flag at `:41`;
  `9632a40` (2026-08-23) is the commit that first ran `mise run check` in `check.yml`,
  and `git blame -L 29,35 .agents/rules/900-design-and-review-loops.md` attributes the whole paragraph to `0a1687a`,
  which landed the same day;
  `parseIfMatch` is `internal/httpapi/pgo_policy.go:295-308` over `ifMatchRE` at `:25`, read at `:112` and `:203`,
  with its `uint64` parse at `:302-305`, `ifMatchMalformed` at `:312-315`,
  the `PUT` outcomes at `:139-142`, `:148`, `:160-165` and the `DELETE` outcomes at `:214`, `:221-226`;
  `idempotencyKey` is `internal/httpapi/pgo_collections.go:90-` over the constant at `:81`, called at `:463`;
  `RequestID` is `internal/httpapi/requestid.go:27-33` with its comment at `:23-26`, `WithRequestID` is `:39-45`,
  and `ServeHTTP` calls it at `internal/httpapi/server.go:405` under the comment at `:403-404`;
  the console reads `If-None-Match` at `internal/ui/ui.go:125`, after the root at `:111` and the shell at `:116-117`;
  the document's `ETag` component is `internal/httpapi/openapi.json:17`, `RequestId` is `:52-57`,
  `IdempotencyKey` is `:135-146`, `IfNoneMatch` is `:147-155`, the two `IfNoneMatch` references are `:1995` and `:2018`,
  the `IdempotencyKey` reference is `:2212`,
  the policy `delete` parameters are `:2290-2297` under the description at `:2289`,
  and the `put` parameters are `:2330-2337` under `:2329`;
  `queryParameters` is `internal/httpapi/openapi_test.go:55-73`, `compareEncoding` is `:276-294`,
  the two conditional drift cases are `:429-441`, `TestOpenAPIDocumentParameters` is `:559-582`,
  `TestOpenAPIDocumentHeaders` is `:609-638` with `create` at `:613` and the header block at `:629-636`,
  `compareConditional` is `:771-812`, and `TestOpenAPIDocumentConditional` is `:814`;
  `TestOpenAPIDocumentWriteRoutesRequireJSON` is `internal/httpapi/openapi_test.go:584-`;
  `internal/auth` reads `Authorization` at `internal/auth/basic.go:130` and `internal/auth/oidc.go:146`,
  and `Sec-Fetch-Mode`, `Sec-Fetch-Dest`, and `Sec-Fetch-Site` at `internal/auth/browser.go:154` and `:191`,
  and `openapi.json` has no `securitySchemes` and no `security` key;
  `routeTable` is `internal/httpapi/routes.go:46-74` with its twenty-one entries at `:47-73`,
  each carrying a template and a kind, and `internal/httpapi/server.go:504-512` answers
  `kindAuth` and `kindOpenAPI` before the credential step;
  `check-repo.py` is 312 lines with its docstring at `:2-31`, `check_status`'s `Outcome:` rule at `:95-98`,
  `check_clientgo_importers` at `:121-129`, `check_hooks` at `:270-284`, and `main` at `:287-308`;
  `TestClusterRoleTuples` is `deploy/deploy_test.go:76`;
  `TestRoundsDecodeHeapDelta` is `internal/pgo/rounds_test.go:848-` under its comment at `:843-847`,
  with the skip at `:849-851`, the collection and the second read at `:867-868`,
  `runtime.KeepAlive(parsed)` alone at `:869`, and `len(plain)` in the failure message at `:874`;
  `raceEnabled` is `internal/pgo/race_off_test.go:6` and `internal/pgo/race_on_test.go:8`;
  `PGODecodeFactor` is `internal/config/config.go:529` and is read by `PGOMemoryBytes` at `:552`;
  `pgo.md`'s guard bullet is `docs/specs/pgo.md:3496-3497` and its *Amendments* open at `:4026`;
  `gateway.md`'s *The OpenAPI document* is `docs/specs/gateway.md:1220-`, its check list `:1295-1306`, *Layers* `:2032-2033`,
  *Continuous integration* `:2226-2238`, and *Amendments* `:2623-2632`;
  the harness declarations are at the lines task 7 names,
  its `TestMain` is `test/e2e/harness_test.go:185-` and reads `loadNATSImage` at `:231`, `dumpGateway` at `:274`,
  and `forwardGateways` at `:277`, and `loadNATSImage` with its comment is `:383-386`;
  `wc -l test/e2e/scenarios*_test.go` is 6447 over five files and `scenarios_*_test.go` is 5016 over four;
  the decision record's lines are `docs/decisions/e2e-without-framework.md:28`, `:30-32`, `:36-40`, `:58`, `:63`, and `:66-70`,
  and `6dff39f` added *Revisit*, corrected the *Consequences* line, and left *Decision* alone;
  `docs/README.md:13-14` say the guides age by being kept true, `:24` calls decision records immutable,
  and `:25` calls investigations frozen;
  the roadmap's tick rule is `docs/plans/roadmap.md:11-15`, the item is `:318-338`, and its `Shipped:` line is `:337`;
  the route-count sentences are at the lines task 4 names;
  every commit header above is under 50 characters.
- Decided here, with the reason stated where it is carried:
  eight tasks, a task for the ticked bullet, the flag task after it, the split last and required;
  `e2e.yml`'s shape for `check.yml`, the `prose` gate kept, the amendment block added in task 1;
  the workflow run and its step log as task 1's evidence rather than the name in the checks list;
  one `IfMatch` component with `parseIfMatch`'s pattern, its `uint64` range in the description, `required` false,
  referenced by the two writes;
  a reviewed `headerParameters` map in `TestOpenAPIDocumentParameters`, scoped to parameters and not to credentials,
  the key's rule moved out of `TestOpenAPIDocumentHeaders`, the two tests not merged;
  `X-Request-Id` in no operation, `If-None-Match` on the two asset operations, `compareConditional` untouched;
  the absent security scheme named as owed rather than added;
  rule 900's two stale sentences rewritten together;
  fourteen patterns over sixteen occurrences in five files pinned by wording with their cardinality asserted,
  seven counts from the entry's template and kind, the three miscounts corrected in the same task,
  and the specs, the changelog, the four listing routes, and the PGO query split left alone;
  the skip removed on this task's own measurement, with both lifetimes held, rather than a second run added;
  the flag finished by a passing flagged run, a reported race fixed inside the task, and the rule untouched;
  four new harness files, the Pod helpers among them, the move verified by declaration inventory,
  and the record's stale measurements corrected in place as `6dff39f` did;
  one changelog entry;
  the plan closed naming the pull request in both `Outcome:` and the roadmap's `Shipped:` line,
  and deleted by the commit after the merge.
- Left to the implementer:
  the exact wording of the two amendment blocks, the rule 900 paragraph, the three corrected route-count sentences,
  and the decision record's *Consequences* and *Revisit* lines;
  the import lists of the four new harness files;
  the heap delta this plan does not carry a number for, and the wall time of the flagged run;
  and the wording of every commit body.
