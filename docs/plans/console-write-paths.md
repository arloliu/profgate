# The Console Writes, Is Driven, and Serves Stable Paths

**Status:** In Progress

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Every task that lands Go code is test-first,
> and each of those begins by adding the declarations its tests name — types, method stubs, test helpers —
> so its first run fails on assertions rather than on compilation.
> Checkboxes (`- [ ]`) track progress.
> The accepted specs outrank this plan:
> where the two differ the spec wins and the plan is the bug.

**Goal:** close the three things the console still owes an operator.
It can start and cancel a Collection instead of sending them to `curl` with a request body.
Its JavaScript is executed by something before a person opens it,
which today is nothing at all.
And an asset both builds of a rollout carry stops answering `404` during one,
which under the content-hashed tree it did not.
That is the whole of what stable paths buy, and *Layout and embedding* is exact about the rest:
a release that adds a file or drops one still has a path the other build answers `404` for,
the release that moves off hashed prefixes is one where neither build serves what the other's shell names,
and a load can combine a shell from one build with a module from another,
which runs unless the two changed incompatibly.
A reload after the rollout converges recovers every one of those.

**Architecture:** the change is concentrated in `internal/ui`.
Its handler stops hashing the tree and starts holding one entry per file —
bytes, `Content-Type`, length, and an entity tag — served at the path the file has under `static/`.
Its static tree gains `collectionmodel.js`, a fourth pure module beside `urls.js`, `portmodel.js`, and `targetmodel.js`,
and `app.js` gains the two controls that module decides for.
`test/e2e` gains two scenarios that drive a headless Chromium,
and the `Harness` gains the browser it discovers once.
`internal/httpapi` changes in one way only:
one comment in `internal/httpapi/routes.go` describes a hashed tree that no longer exists.
`internal/k8s`, `internal/config`, `internal/auth`, `internal/pgo`, `internal/natskv`, and `deploy/` gain nothing.

**Tech Stack:** everything already pinned in [`mise.toml`](../../mise.toml),
plus `github.com/chromedp/chromedp` and the DevTools Protocol and WebSocket modules it brings,
which only `test/e2e` imports and only behind the suite's build tag.
No module reaches `cmd/profgate`, so the binary a release ships links none of them.
chromedp needs a Chromium executable, which is not a Go module:
the workflow installs a pinned one and the runner refuses a version outside the range it accepts.

**Spec:** [`docs/specs/ui.md`](../specs/ui.md), `Accepted`, is the design of record for all three:
*Starting and cancelling a Collection* holds the two controls, the idempotency key, and both answer tables;
*Layout and embedding* and *Headers* hold the stable paths, the entity tag, and the cache rules;
*Unit* and *End to end* hold every test named here.
[`docs/specs/pgo.md`](../specs/pgo.md), `Accepted`, holds the create, the replay, the cancel, and the media-type rule,
all of which the gateway already implements;
this plan adds no route and no gateway behavior.
Sections are cited by heading name, never by number;
an unqualified heading is the console spec's.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

## Global Constraints

- **The permission invariant does not move.**
  No task edits `internal/k8s`, so the seven read tuples stay seven,
  and no task adds a NATS key prefix.
  The console reaches the two write routes as any other client does,
  under the realm flags the gateway already checks.
  `TestClusterRoleTuples` in `deploy/deploy_test.go` and
  `TestChartClusterRoleMatchesBase` in `deploy/chart_test.go` stay green and untouched.
- **The gateway gains no route, no status, and no header.**
  `POST /v1/namespaces/{ns}/services/{svc}/collections` and `POST /v1/collections/{id}/cancel` are in `internal/httpapi/routes.go` today,
  the `Idempotency-Key` header and the JSON media-type step are implemented,
  and every status either answer table names is one the gateway already writes.
  A task that finds itself editing a handler in `internal/httpapi` has misread the spec.
- **No Go module reaches the gateway binary.**
  chromedp is added under `require` and imported only from files carrying the suite's build tag.
  A test that asserts the binary's module set, if one exists, stays green.
- **The page renders every value it did not write as text.**
  The source scan of *Unit* is extended to `collectionmodel.js` rather than relaxed for it,
  and `app.js` keeps holding no string literal beginning with `/v1`, `/ui`, or `/auth`.
- **`window.confirm`, `window.alert`, and `window.prompt` stay forbidden.**
  Both controls confirm in place, and the scan refuses those three names.
- No jargon: code comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a task name, or a review round.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose and Go doc comments use semantic line breaks;
  run `semlf --base HEAD` before each commit and fix what it reports.
- Commit headers are Conventional Commits under 50 characters, with no trailer of any kind.

---

## What This Plan Leaves Alone

| Not in this plan | Why |
|---|---|
| the policy editor | *Non-goals*: `PUT` and `DELETE` on the policy route carry a precondition, and designing that flow is the work |
| any sampling field on the start request | choosing rounds and durations is editing policy, and the page sends one empty object |
| a second browser | *Non-goals*: one headless Chromium, no Firefox, no Safari, no browser matrix |
| a browser in `mise run test` | *What is not proven*: no unit test starts one |
| a rolling-update test | *Layout and embedding* puts two builds in one cluster out of scope |
| the **Copy URL** button's clipboard path | *What is not proven*: the suite grants no clipboard permission |
| CORS of any kind | *Non-goals*: the page and the API are one origin |

---

## Four Things This Plan Decides

### The handler holds a table of every file, and routing is what hides one

*Layout and embedding* says the constructor "walks the embedded tree once and builds one table:
for each regular file its bytes, its `Content-Type`, its length, and its **entity tag**."
Every regular file means `index.html` too, and *Unit* asks for each file's tag,
so the table holds it and the shell is answered from its entry.
What refuses `/ui/index.html` is the route, which names that file and answers `404 route_unknown`,
and not a gap in the table:
a table missing an entry would make the `404` an accident of construction rather than a decision,
and would leave one file of the tree with no tag for the test that walks them all.
The table is a `map[string]asset` keyed by the file's path under `static/`,
built in `newFromFS` and never written afterwards,
so `ServeHTTP` reads it without a lock and computes no hash per request.
The shell is served with `Cache-Control: no-store` and no `ETag` (*Headers*),
so holding its tag costs one field and answers nothing differently.

### The entity tag is the whole digest, quoted

Sixty-four lowercase hex digits inside double quotes, as `sha256sum` prints them.
Not truncated, because a tag a developer can reproduce on the file in this repository turns a mismatch into a fact.
Not weak: no response of this handler sets `W/`,
and `If-None-Match` comparison trims a leading `W/` from what a client sent
so a proxy that weakened the tag still revalidates.

### The idempotency key belongs to the attempt, not to the press

*Starting and cancelling a Collection* generates it when the control arms,
sends it again after any outcome the page could not classify,
and drops it as soon as a response says what happened.
`collectionmodel.js` decides all three, so each is a row of a table test rather than a branch in `app.js`.
The three outcomes that keep it are named in the spec and nowhere else:
a rejected `fetch`, a `503 pgo_unavailable`, and any other `5xx` besides `503 collector_unavailable`.

### The browser is a property of the machine, not of the lane

*End to end* says every field of the lane matrix describes a cluster.
Discovery therefore runs once into the `Harness`,
reads `PROFGATE_E2E_BROWSER` and then the names Chromium and Chrome ship under,
prints the path and the version it found,
and skips both scenarios by name with one shared reason when it finds none.
A version outside the range the suite pins fails the scenario rather than skipping it,
because a browser too old for the page's `crypto`, `URL`, or module behavior is a red test and not a mystery.

---

## File Structure

```text
internal/ui/ui.go                          the asset table, the entity tag, the conditional answer; treeHash and renderShell go
internal/ui/ui_test.go                     rewritten against stable paths
internal/ui/scan_test.go                   the source scan gains collectionmodel.js
internal/ui/collectionmodel_test.go        new: the Collection-control model under the interpreter
internal/ui/static/index.html              names /ui/app.css and /ui/app.js itself; no placeholder
internal/ui/static/collectionmodel.js      new: the Collection controls' pure functions
internal/ui/static/urls.js                 gains the cancel path builder
internal/ui/static/app.js                  the two controls, wired to the model
internal/ui/static/app.css                 a few rules for the armed control
internal/httpapi/routes.go                 one comment: the remainder is not a hashed tree any more
internal/httpapi/requestid_test.go         one asset path and the cache policy beside it
internal/httpapi/console_test.go           three asset paths and the cache policy they assert
internal/httpapi/openapi.json              the console operations: an ETag on an asset, no 304 on the shell
internal/httpapi/openapi_test.go           a declared 304 belongs to a response that carries an entity tag
cmd/profgate/serve_test.go                 the shell names an unhashed script
internal/ui/vendor_test.go                 the third import-free model
test/e2e/registry.go                       two entries: console-oidc and console-basic
test/e2e/harness_test.go                   browser discovery, held on the Harness
test/e2e/browser_test.go                   new: the chromedp helpers both scenarios use
test/e2e/scenarios_console_test.go         new: the two runners
test/e2e/scenarios_auth_test.go            the two wire proofs read stable paths and a conditional answer
.github/workflows/e2e.yml                  a pinned Chromium, asserted before the suite runs
docs/console.md                            the write controls, and what a rolling update now does
docs/specs/gateway.md                      Dependencies, Layers, What end-to-end proves, Failure Scenarios
docs/specs/auth.md                         Testing: the two lanes gain the browser scenarios
docs/specs/pgo.md                          Create a Collection, Errors: what Retry-After a 429 carries
.agents/rules/500-validation-and-workflow.md   the console's assets, and the tests that run its page
docs/api.md                                nothing: it already documents both write requests
CHANGELOG.md                               one Unreleased entry per landed change
```

---

## The assets serve at stable paths

**Files:**
- Modify: `internal/ui/ui.go`, `internal/ui/ui_test.go`, `internal/ui/static/index.html`,
  `internal/httpapi/routes.go`, `internal/httpapi/requestid_test.go`, `internal/httpapi/console_test.go`,
  `cmd/profgate/serve_test.go`

*Layout and embedding* and *Headers* are the whole of this task.
Nothing about the page's behavior changes;
what changes is the URL every asset has and the header that decides when a browser refetches it.

**The shell.**
`index.html` names `/ui/app.css` in its `<link href>` and `/ui/app.js` in its `<script src>`,
written into the file rather than substituted at startup.
`renderShell`, the two placeholder constants, and the `bytes` import go with it.
A mistyped path in that file is now caught by the test that fetches both, or by nothing.

**The table.**
`newFromFS` walks the tree once and builds `map[string]asset`,
where `asset` holds the bytes, the `Content-Type` `contentType` already computes, the length, and the tag.
`index.html` is in it like every other regular file, and the shell is answered from its entry;
what refuses `/ui/index.html` is the route, which names that file
(*The handler holds a table of every file, and routing is what hides one*).
`treeHash`, `Hash`, `assetPrefix`, and `cacheImmutable` go;
`encoding/binary`, `sort`, and `crypto/sha256`'s framing loop go with them,
and `crypto/sha256` stays for the per-file digest.

**The answer.**
`/` is unchanged: `302` to `/ui/` with `Cache-Control: no-store`.
`/ui/` is the shell: `no-store`, `text/html; charset=utf-8`, no `ETag`.
Every other path under `/ui/` is looked up in the table by its remainder:
a hit answers `200` with the bytes, the file's `Content-Type`, its `Content-Length`, its `ETag`,
and `Cache-Control: no-cache`;
a miss is `404 route_unknown`.
The lookup refuses, before it reads the table, an empty rest, a leading slash, a backslash,
a path `fs.ValidPath` rejects, and `index.html`.
A directory name and a traversal are refused by the same rules,
since neither is a key of a table built from regular files —
but the explicit refusals stay, because the segment that used to reject some of them by itself is gone.

**The conditional answer.**
`If-None-Match` is parsed as a comma-separated list.
`*`, or any member equal to the file's tag after trimming a leading `W/` and surrounding spaces,
answers `304` with the `ETag` and the `Cache-Control`, no body, and no `Content-Length`.
Anything else answers the file.
`HEAD` answers the same headers as `GET` with no body, the `304` included.
A method that is neither is `405` with `Allow: GET, HEAD`, as today.

- [ ] **Write the tests**

| Case | Expect |
|---|---|
| each file's `ETag` | quoted 64-digit lowercase hex of its SHA-256, identical across two constructions |
| one byte of one file changed | that file's tag changes and every other file's is unchanged |
| `GET /ui/` | the shell, every header of *Response headers and CSP*, `no-store`, no `ETag` |
| the shell's `<link href>` and `<script src>` | `/ui/app.css` and `/ui/app.js`, and each answers `200` |
| a `GET` for **every regular file of the tree** but `index.html`, walked rather than listed | `200` with the file's `Content-Type`, `Content-Length`, `ETag`, and `no-cache` |
| `Last-Modified` on the shell, on an asset `200`, on a `304`, and on a `HEAD` | absent, as it is today: an embedded file has no meaningful modification time |
| the same request with `If-None-Match` repeating the tag | `304`, the `ETag`, the `Cache-Control`, no body, no `Content-Length` |
| `If-None-Match: *` | `304` |
| a list holding the tag beside two others | `304` |
| the tag with a `W/` prefix | `304` |
| another tag | `200` with the bytes |
| `HEAD` on each of the two above | the same `304`, and the same `200` headers with no body |
| `POST /ui/app.js` | `405` with `Allow: GET, HEAD` |
| `/ui/nothing.js`, `/ui/vendor`, `/ui/vendor/`, `/ui/index.html` | `404 route_unknown` |
| `/ui/../app.js`, `/ui//app.js`, `/ui/vendor/..%2Fapp.js`, a backslash between segments, `/ui/` with an empty rest handled as the shell | `404 route_unknown` except the last, which is the shell |

The asset table is walked, not enumerated:
the test reads the embedded tree and asserts the row above for each file it finds,
so a vendored file added later is covered without an edit,
and the four content types the old table spelled out stay as their own rows.

- [ ] **Run the tests and watch them fail**
- [ ] **Implement the table and the conditional answer**
**One assertion inverts rather than moves.**
`checkCommonHeaders` in `internal/ui/ui_test.go:161-165` fails any response that carries an `ETag`,
and nearly every test in the file calls it.
It splits by response class:
the shell and the `404` and `405` envelopes still carry no validator,
and every asset `200` and every `304` must carry one.
`Last-Modified` stays absent everywhere.

**The shell's two paths are asserted or they are asserted nowhere.**
`TestShellInlineForms` at `internal/ui/ui_test.go:463-484` reads `index.html`
and does not check the `<link href>` or the `<script src>` value.
Once nothing rewrites the shell, a mistyped path in that file is caught only by the test this task adds.

- [ ] **Repair what cited a hashed path**

Four files outside `internal/ui` spell the hashed shape or the cache policy that went with it:

| Where | What it says today |
|---|---|
| `internal/httpapi/requestid_test.go:200-206` | `/ui/static/abc/app.js`, and `public, max-age=31536000, immutable` as the console's own policy |
| `internal/httpapi/console_test.go:80`, `:139`, `:204-210` | the same two, and a deeper path that proves `paramFile` spans separators |
| `cmd/profgate/serve_test.go:1066`, `:1130-1138` | `shellScript` matches `/ui/static/[0-9a-f]+/app\.js` out of the shell and fails with "the shell names no hashed app.js path" |
| `internal/httpapi/routes.go:22-25` | the `paramFile` comment: the remainder spans separators "because the hashed asset tree is nested" |

The first three become the stable path and `no-cache`;
the deeper path stays, because the *vendored* tree is nested, which is what the comment should have said.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/ ./internal/httpapi/ ./cmd/profgate/
mise exec -- go vet -tags e2e ./test/e2e/
mise run lint && mise run test && mise run check
git add internal/ui/ internal/httpapi/ cmd/profgate/ test/e2e/
git commit -m "feat(ui): serve every asset at a stable path"
```

The wire-proof repair below is part of **this** commit, not the next one.
A tree whose handler serves stable paths and whose end-to-end suite still matches a hashed one is red,
and a commit is not a place to leave the suite red on purpose.

---

## The document declares what the handler answers

**Files:**
- Modify: `internal/httpapi/openapi.json`, `internal/httpapi/openapi_test.go`

`internal/httpapi/openapi.json:1938-2010` already declares a `304` on all four console operations,
which the handler could not write before this plan and which the shell cannot write after it.
The document was written against the design and the code was not,
and no check reads that part of it, so the drift is silent rather than red.

Two repairs and one check:

- `/ui/{file}` keeps its `304` and its `ConsoleAsset` response gains an `ETag` header.
  The existing `components/headers/ETag` describes the stored policy revision an `If-Match` sends back;
  it is a different header with a different meaning, so a second component is declared rather than reused.
- `/ui/` loses its `304` on `get` and on `head`.
  The shell is `no-store` with no entity tag (*Headers*) and can never answer one.
- `openapi_test.go` gains the check that would have caught both:
  an operation that declares a `304` names a response whose `200` carries an entity-tag header,
  and one that declares no entity tag declares no `304`.

- [ ] **Write the check and watch it fail against the document as it stands**
- [ ] **Repair the document**
- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/httpapi/
mise run lint && mise run test && mise run check
git add internal/httpapi/
git commit -m "fix(httpapi): declare the console's entity tag"
```

---

## The wire proofs read a stable path

**Files:**
- Modify: `test/e2e/scenarios_auth_test.go`

This is not a commit of its own: it lands inside the task above, which is why it carries no validation block.
It is written separately because it is the one part of that task the unit suite cannot catch.

The two wire proofs of *End to end* already run inside the authentication scenarios.
`assetRe` at `test/e2e/scenarios_auth_test.go:2008` matches `/ui/static/<16 hex>/…`,
`assertAsset` at `:2057-2076` asserts `public, max-age=31536000, immutable`,
and `assetPaths` at `:2082-2093` derives the vendored path by slicing the hash directory out of the first match.
All three read a shape the task above deleted.

- [ ] **Point the proofs at the stable paths**

The regular expression matches `/ui/` followed by a path with no hash segment.
Each asset the shell names is fetched and asserted `200` with `Cache-Control: no-cache` and an `ETag`;
the same request repeating that tag in `If-None-Match` is `304` with no body.
`GET /ui/` keeps its `no-store` and gains an assertion that it carries no `ETag`.
The audit assertion is unchanged: the shell and the asset requests still write no record.

The end-to-end suite runs in the browser task, which is where a console change first needs a cluster,
and again before the pull request opens;
*Before a PR* in [`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md) is what binds the second run.

---

## The Collection controls decide in a module of their own

**Files:**
- Add: `internal/ui/static/collectionmodel.js`, `internal/ui/collectionmodel_test.go`
- Modify: `internal/ui/scan_test.go`, `internal/ui/vendor_test.go`

*Starting and cancelling a Collection* says the mapping from a response to the page's next state is a pure function per control,
"so every arm is a test case rather than a branch a reader has to trust".
This task lands the module and its table test and wires nothing:
`app.js` is untouched until the next task, so a failure here is a failure of the decision table alone.

**The module's shape** is `portmodel.js`'s and `targetmodel.js`'s:
it imports nothing, declares plain functions, and ends in a single `export { … };` statement.
`loadModel` in `internal/ui/portmodel_test.go` cuts that statement off and reads the functions as globals,
and the shape assertion is what makes cutting it safe.
`urls.js` is not the template: it exports inline with `export function`,
which `cutExport` at `internal/ui/portmodel_test.go:29-41` refuses.

**The functions.**

```text
startOffered(limits, whoami, service)   the start control exists
cancelOffered(state, whoami)            a row's cancel control exists
uuidFromBytes(bytes)                    sixteen bytes as a UUIDv4 string
startRequest(route, key)                what the start POST carries
cancelRequest(route)                    what the cancel POST carries
startOutcome(answer)                    what an answer to a start does
cancelOutcome(answer, try)              what an answer to a cancel does
retryAfterSeconds(header)               the delay a 429 asks for
startNext(state, event)                 the armed, in-flight, retained, and cooling states
```

`startOffered` is true only when `limits.pgo.enabled`, `whoami.realm.pgo.read`, `whoami.realm.pgo.collect`,
and a non-empty Service all hold;
any one missing is false, and the caller draws nothing rather than a disabled control.
`cancelOffered` is true for `pending` and `running` under `whoami.realm.pgo.collect`,
and false for `initializing`, for the four terminal states, and for any value outside the set.

`uuidFromBytes` writes the version nibble `4` into the seventh byte and the variant bits `10` into the ninth,
whatever those bytes held there,
and formats lowercase hex in the 8-4-4-4-12 grouping.
`app.js` calls `crypto.randomUUID()` when the browser defines it
and `uuidFromBytes(crypto.getRandomValues(new Uint8Array(16)))` when it does not;
the module holds no random source, which is what makes it testable without one.

`startRequest(route, key)` returns
`{method: "POST", url: route, headers: {"Content-Type": "application/json", "Idempotency-Key": key}, body: "{}"}`.
`cancelRequest(route)` returns the same shape with no `Idempotency-Key` and no body:
`{method: "POST", url: route, headers: {"Content-Type": "application/json"}, body: null}`.
A cancel carries no body and still declares the media type,
because the gateway refuses a write route that does not (*Why another site cannot send these two requests*),
and the check reads the header and never the body.
Neither spells a path: `urls.js` is the only module that spells a `/v1` path,
and `app.js` hands the built route in.

`startOutcome(answer)` takes

```text
{rejected, status, code, message, id, body, retryAfter}
```

where `rejected` says the `fetch` never produced a response,
`code` and `message` are the envelope's when the body parsed as one,
`id` is the identifier a `2xx` carried,
`body` is the response text as it arrived — which the invalid-`2xx` row shows and no other row reads —
and `retryAfter` is the header verbatim or `null`.
It returns

```text
{keep, armed, select, refetch, error, disableSeconds}
```

where `keep` says the idempotency key is sent again,
`armed` says the control returns to its armed state,
`select` is a Collection identifier or `null`,
`refetch` is an array of `"collections"`, `"whoami"`, and `"limits"` — empty, never `null`, and never a bare string,
`error` is `null` or the text to show,
and `disableSeconds` is `0` unless a `429` asked for a delay.

| Answer | `keep` | `armed` | `select` | `refetch` | `error` | `disableSeconds` |
|---|---|---|---|---|---|---|
| `2xx` with an `id` matching the identifier grammar | no | no | the `id` | `collections` | none | 0 |
| `2xx` with no `id`, or one outside the grammar | no | no | none | none | the body as text | 0 |
| `429 collection_in_progress` | no | no | none | `collections` | none | 0 |
| `429 rate_limited`, `429 capacity_exhausted` | no | no | none | none | the wait, named | the header |
| `409 idempotency_mismatch` | no | no | none | `collections` | the envelope | 0 |
| `403 realm_denied` | no | no | none | `whoami` | the envelope | 0 |
| `501 pgo_disabled` | no | no | none | `limits` | the envelope | 0 |
| `503 pgo_unavailable` | **yes** | **yes** | none | none | the envelope | 0 |
| `503 collector_unavailable` | no | no | none | none | the envelope | 0 |
| a rejected `fetch`, or any other `5xx` | **yes** | **yes** | none | none | the envelope or the rejection | 0 |
| any other status | no | no | none | none | the envelope | 0 |

The first row reads the whole `2xx` range and not `202` and `200`,
so a later release that answers a replay with another success status still selects the record.

`cancelOutcome(answer, try)` returns `{replace, refetch, error, retryAfterMs}`.
`try` is which press of this cancel produced the answer, `1` or `2`,
and is named for the retry rather than `attempt`,
which is a field of the Collection record itself and would read as that one.

| Answer | Result |
|---|---|
| `200` | `replace` is the returned record; `refetch` is `collections` |
| `409 collection_terminal` | `refetch` is `collections`; no error |
| `409 collection_initializing`, `try` 1 | `retryAfterMs` is 1000; nothing else |
| `409 collection_initializing`, `try` 2 | the code is shown; the row is left as it is |
| `404 collection_not_found` | `refetch` is `collections` and `whoami` |
| a rejected `fetch`, or any other status | the error is shown; the row is left as it was |

`retryAfterSeconds` reads a count of seconds and nothing else:
`"7"` is 7, `"0"` is 0, `"900"` clamps to 300,
and absent, empty, negative, fractional, non-numeric, and an HTTP-date each read as 5.

`startNext(state, event)` is the armed state, and it is where the attempt's identity lives.

`state` is `{phase, key, route, token, until}`.
`phase` is one of `idle`, `armed`, `inflight`, `retained`, and `cooling`.
`token` is the attempt's own number, raised on every `arm`;
`app.js` sends it with the request and hands it back on the `outcome`,
so an answer to an attempt the page has left is discarded instead of selecting a record of a Service nobody is looking at.
`until` is when a `cooling` phase ends, on the same millisecond clock `timer` reports,
and is `0` in every other phase.

`event` is `{kind, …}` with `kind` one of
`arm`, `submit`, `outcome`, `timer`, `keep`, and `selection`.
`arm` carries `{key, route}`;
`outcome` carries `{token, keep, disableSeconds}` and `timer` carries `{now}`;
`keep` and `selection` carry nothing.
`startNext` returns `{state, message}`, where `message` is `null` or the text the page shows.

| Phase and event | Next |
|---|---|
| `idle` + `arm` | `armed`, holding the key and route the event carried, with a fresh token |
| `armed` + `timer` | `idle`, dropping both — the ten seconds elapsed and nothing was sent |
| `armed` + `submit` | `inflight`, keeping the token |
| `armed` + `keep` | `idle`, with no message |
| `inflight` + `timer` | `inflight`, unchanged — a timer never fires under a request |
| `inflight` + `outcome` whose `token` is not the state's | `inflight`, unchanged: the answer is to an attempt already abandoned |
| `inflight` + `outcome` whose `keep` is true | `retained`, holding the key and route |
| `inflight` + `outcome` whose `keep` is false and `disableSeconds` is `0` | `idle`, dropping both |
| `inflight` + `outcome` whose `keep` is false and `disableSeconds` is above `0` | `cooling` until `now + disableSeconds * 1000`, dropping the key |
| `retained` + `timer` | `retained`, unchanged — the retained state survives an arbitrary wait |
| `retained` + `submit` | `inflight`, sending the key it holds under the token it holds |
| `retained` + `keep` | `idle`, dropping both, with the message that a Collection may already exist and names the Collections table |
| `cooling` + `submit` | `cooling`, unchanged: a press before `until` sends nothing |
| `cooling` + `timer` at or past `until` | `idle` |
| `cooling` + `arm` before `until` | `cooling`, unchanged |
| `idle`, `armed`, or `cooling` + `selection` | `idle`, with no message |
| `inflight` or `retained` + `selection` | `idle`, dropping the key and route, with the message that a Collection may already exist |
| `idle` + `outcome` | `idle`, unchanged: the attempt it answers was abandoned by a `selection`, and the token counter is not rewound |
| every other pair | the state is returned unchanged and the message is `null` |

The last row is the function's shape, not a gap in the table:
`startNext` is total, and a pair with no rule of its own is a pair where nothing should happen.
A test drives every phase against every event so that "nothing should happen" is asserted rather than assumed.

**Why `inflight` warns and `armed` does not.**
*Two presses, never a dialog* says the armed state clears on any change of namespace or Service,
and *What the page holds after an outcome it could not classify* says changing them
"abandons the attempt the same way and says the same thing".
An `armed` control has sent nothing, so there is nothing to warn about.
An `inflight` one has sent a `POST` that commits before it answers,
so leaving the Service silently is exactly the loss the message exists to prevent —
and the token is what stops that request's late answer from acting on the Service the page moved to.

- [ ] **Write the shape assertion and the table test**

`internal/ui/collectionmodel_test.go` follows `internal/ui/targetmodel_test.go`:
the same `loadModel` helper, the same shape assertion, one table per function.
Every row of both answer tables, every row of the phase table,
each `retryAfterSeconds` case, and the key format —
sixteen fixed bytes formatted twice to the same string, two different inputs formatting differently,
and the version and variant bits forced whatever the input held there.

Four cases *Unit* names that a reader of the tables alone could miss:

- a `202` whose `id` is outside the identifier grammar selects nothing and builds no path;
- the key is unchanged across presses that follow a rejected `fetch`, a `503 pgo_unavailable`,
  or any other `5xx` besides `503 collector_unavailable`,
  and different after any response that classified the attempt, `503 collector_unavailable` included —
  which is a sequence of `startOutcome` and `startNext` calls, not one row of either;
- a press after a lost response sends the key the lost press sent;
- the cooling phase refuses a press before `until`,
  while `disableSeconds` of `0` produces no cooling at all and returns the control to `idle`,
  which is what separates the `"0"` row of `retryAfterSeconds` from the absent-header row's five seconds.

- [ ] **Run the tests and watch them fail**
- [ ] **Write the module**
- [ ] **Extend the source scan, and add the one it is missing**

`consoleSources()` at `internal/ui/scan_test.go:13-15` is a closed set,
and `TestScanCoversEveryJSFile` asserts it equals the `.js` files of the tree,
so the new module turns the suite red until it joins that list —
which is the mechanism working, not a failure.
It joins the markup-interface scan with it,
and `TestVendorImportFreeModels` at `internal/ui/vendor_test.go:300-311`,
which iterates two models today and must iterate three.

One scan *Unit* requires does not exist at all and is written here:
`app.js` contains none of `confirm(`, `alert(`, or `prompt(`.
The pattern carries the opening parenthesis on purpose —
`app.js:44` holds the word `confirm` inside an error hint, which is prose and not a call.
A fixture that must fail is written beside it, as every other scan has,
so a pattern that matches nothing is itself caught.

`TestScanPageUsesCollectionModel` joins `TestScanPageUsesPortModel` at `internal/ui/scan_test.go:110-124`
and `TestScanPageUsesTargetModel` at `:132-146`,
so a page that stops calling the model turns the suite red.
It is written in the task that wires the page, because until then `app.js` calls nothing.

- [ ] **Record the interpreter's third model**

`docs/specs/gateway.md` *Dependencies* has one row for the interpreter,
and it names the port-control model alone —
which has been short by one since `targetmodel.js` landed and is short by two once this module does.
The row names all three.
*Dependencies* says a module's row is written when the tests that need it land,
which is here and not in the documentation task.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/ docs/specs/gateway.md
git commit -m "feat(ui): decide the collection controls in one module"
```

---

## The page starts and cancels a Collection

**Files:**
- Modify: `internal/ui/static/app.js`, `internal/ui/static/urls.js`, `internal/ui/static/app.css`,
  `internal/ui/scan_test.go`

`urls.js` gains `collectionCancelURL(id)`, built the way `collectionURL` is,
and `app.js` imports it and the eight functions of `collectionmodel.js`.

**The page's transport reads and does not write.**
`fetchJSON(url)` at `internal/ui/static/app.js:55-81` calls `fetch(url, {credentials: "same-origin"})`,
with no method, no headers, and no body,
and `request(key, url, retry, retryOnce)` at `:281` wraps it;
all six call sites are `GET`s.
Sending the two `POST`s means giving both an optional request description rather than writing a second transport,
so one place still decides what a rejected `fetch` and a non-JSON body mean.

**The error hints are short of the vocabulary.**
The table at `internal/ui/static/app.js:36-46` covers nine of the sixteen codes *Errors* lists.
Seven of the missing ones are answers these two controls can receive —
`collector_unavailable`, `collection_in_progress`, `rate_limited`, `capacity_exhausted`,
`collection_terminal`, `collection_initializing`, and `limit_exceeded` —
and each gains a hint here.
`version_conflict` and `version_missing` belong to the policy route this page does not call,
and are left out.

**Start collection** sits above the Collections table when `startOffered` says so.
Pressed once it arms: the page generates the key, records the route, and shows **Confirm start** beside **Keep**.
Pressed again it submits `startRequest`, and the answer goes through `startOutcome`;
`refetch` drives the fetches the page already has,
`select` reuses `onSelectCollection`,
and `error` is shown through the page's existing error surface.
The ten-second timer is a `setTimeout` cleared on submit and never restarted from `inflight` or `retained`,
which is `startNext` executed rather than restated.

**Cancel** sits on a row whose `state` `cancelOffered` accepts, and arms the same way.
`409 collection_initializing` schedules the single retry `cancelOutcome` asks for.

`disableSeconds` is consumed here or nowhere:
a `429 rate_limited` or `429 capacity_exhausted` puts the control in `cooling`,
the button says how long it is waiting,
and a `setTimeout` sends the `timer` event that ends it.
A press during `cooling` sends nothing, which is `startNext` executed rather than a second guard in the page.

Both controls clear on a change of namespace or Service through the `selection` event,
which the page already tracks as `this.selection`.
Every start response carries the token the page sent with it,
so an answer that arrives after the operator has moved on is handed to `startNext` and discarded there.

- [ ] **Wire the two controls**
- [ ] **Keep the scan green, and add the model-usage scan**

`app.js` still contains no `confirm(`, `alert(`, or `prompt(`,
no string literal beginning with `/v1`, `/ui`, or `/auth`,
and none of the six markup interfaces.
This task must not need to relax one of those.
`TestScanPageUsesCollectionModel` is written here, now that the page calls the model.

- [ ] **Validate and commit**

```bash
mise exec -- go test -race ./internal/ui/
mise run lint && mise run test && mise run check
git add internal/ui/
git commit -m "feat(ui): start and cancel a collection"
```

---

## A browser executes the page

**Files:**
- Add: `test/e2e/browser_test.go`, `test/e2e/scenarios_console_test.go`
- Modify: `test/e2e/registry.go`, `test/e2e/lanes_test.go`, `test/e2e/harness_test.go`,
  `test/e2e/scenarios_auth_test.go`, `test/e2e/scenarios_tls_test.go`,
  `.github/workflows/e2e.yml`, `go.mod`, `go.sum`,
  `docs/specs/gateway.md`

*End to end* is the whole of this task.
`app.js` is executed by nothing today,
so this layer is the first thing that ever runs it.

**The dependency.**
`github.com/chromedp/chromedp` enters `go.mod`, imported only from files carrying the suite's build tag.
Its `gorilla/websocket` and `mailru/easyjson` are already indirect requirements of this module,
so the `go.sum` growth is the DevTools Protocol packages and little else.
`docs/specs/gateway.md` *Dependencies* gains its row **in this task**, not in the documentation task:
*Dependencies* says each module is documented when the tests that need it land.

### Registering the two scenarios is three edits, not one

`registry.go` holds metadata and `harness_test.go` holds runners, and both are checked against each other.

- `test/e2e/registry.go:19-45` gains `{Name: "console-oidc", NeedsPodReach: true}` and
  `{Name: "console-basic", NeedsPodReach: true}`.
- `runners()` at `test/e2e/harness_test.go:152-180` gains `scenarioConsoleOIDC` and `scenarioConsoleBasic`;
  `TestScenarios` fails with "scenario %q has no runner" otherwise, which is the parity check working.
- `test/e2e/lanes_test.go` asserts the exact set of scenarios a degraded lane skips.
  Two more `NeedsPodReach` entries change that set, so its expectations move with the registry.

### What discovery holds, and what it refuses

`Harness` gains one field of a type declared in `browser_test.go`:

```go
// browser is the Chromium the console scenarios drive, discovered once in TestMain.
// A machine with none leaves path empty and skip filled, and both scenarios skip with that reason.
type browser struct {
    path    string // the executable, from PROFGATE_E2E_BROWSER or from PATH
    version string // what --version printed, whole
    major   int    // the major version parsed out of it
    skip    string // why no executable was found, naming everything that was looked for
}
```

The search order is `PROFGATE_E2E_BROWSER` when it names an executable,
then, on `PATH`, `chromium`, `chromium-browser`, `google-chrome`, `google-chrome-stable`.
`skip` names all five so a developer reads what was looked for.
Discovery runs `--version`, logs the path and the whole string, and reads the major version
as the first run of digits in it — `Chromium 141.0.7390.54` and `Google Chrome 141.0.7390.54` both give `141`.
A string with no such run is a failure, not a skip:
an executable that answered `--version` with something unreadable is a broken pin, not an absent browser.

The accepted range is a pair of constants beside the type, a floor and a ceiling.
A major version outside **either** bound fails the scenario rather than skipping it,
because a browser too old for the page's `crypto`, `URL`, or module behavior is a red test and not a mystery,
and one far newer than anything this suite has run against is a claim nobody checked.
The floor is the oldest version the page's behavior is known to hold on;
the ceiling moves with the version the workflow pins, in the same commit that moves the pin.
A missing executable skips; an unusable or unpinned one does not.

### Reaching the gateway from a browser

`deployHTTPSGateway` returns `127.0.0.1:<port>` for a forward,
while the certificate certifies `gateway` and the OIDC callback is `https://gateway/auth/callback`.
`authClient` bridges that with a dialer; a browser has no dialer, so the launch bridges it with two flags:

- `--host-resolver-rules=MAP gateway:443 <the exact local host and port>` — the port matters,
  because `https://gateway` is port 443 and the forward is not;
- `--ignore-certificate-errors`, which *End to end* names as the one browser setting either scenario changes.

Dex is reached at its NodePort exactly as the wire proofs reach it, and needs no rule.

### Four protocol domains, all enabled before the first navigation

*End to end* states the ordering for two of them and the other two are needed for proofs it also states.
One helper enables all four on the context and starts collecting, and every scenario calls it first.

| Domain | What it is for |
|---|---|
| `Log` | a Content Security Policy violation entry fails the scenario |
| `Runtime` | an uncaught exception fails the scenario |
| `Network` | the start `POST`'s `Content-Type` and `Idempotency-Key`, read as the page sent them |
| `Fetch` | `console-basic` answers the `authRequired` challenge and counts how many arrive |

Header comparison is case-insensitive, and the assertion is that exactly one `Idempotency-Key` entry exists,
which is what "exactly one" in *End to end* means and what a repeated header would break.

**Downloads.** `Browser.setDownloadBehavior` points at a directory the test creates and cleans up,
the scenario waits for the download to complete rather than for the click to return,
and then reads the file and asserts it is the gzip-framed body the profile endpoint streams.

### The gateway `console-oidc` needs is not the one the auth scenarios deploy

The Collections proofs need PGO on, which needs a NATS credential the HTTPS overlays do not mount.
Every piece exists and none of them is wired together today:

- `gatewayConfig` already composes `NATSURL`, `RealmPGO`, `TLSMount`, `AuthBlock`, and `UIEnabled` independently
  (`test/e2e/harness_test.go:804-903`);
- `credsMountPatch` exists precisely to add the credentials mount
  "to a gateway Deployment an overlay wrote without one" (`test/e2e/harness_test.go:911`);
- `deployOwnGateway` shows the rest (`test/e2e/scenarios_pgo_test.go:1255`):
  `gatewayPermissions`, `h.NATS.ID.user`, `h.applyCredsSecret`,
  and the poll past the replay barrier, which a gateway with PGO on needs and a rollout does not wait for.

`deployHTTPSGateway` gains a variadic `...patch` parameter, which its two existing callers pass nothing to.
The console runner's setup is, in order:

```text
pub, sub := gatewayPermissions(h.root)         the account fragment the gateway is granted
user     := h.NATS.ID.user("profgate", …)      a NATS user for this scenario alone
h.applyCredsSecret(ctx, ns, user.Creds)        the credential the mount will carry
cfg      := gatewayConfig(gatewayConfigOptions{
                NATSURL: h.NATS.URL, RealmPGO: true,
                TLSMount: tlsMountPath, AuthBlock: oidcAuthBlock(dex.issuer), UIEnabled: true})
deployHTTPSGateway(t, h, ns, "oidc-gateway", …, gwCA, cfg, credsMountPatch(name))
```

The runner then polls the policy route past the replay barrier as `deployOwnGateway` does.
A `403` or a `503` from the first Collections listing is that barrier, not a bug, and the poll is what absorbs it.

**Two Collections, deliberately different.**
The list and detail proof needs a record that has finished:
the scenario creates one over the API with the configured two-second sampling and waits for `completed`.
The start and cancel proof needs one that has not:
before pressing **Start collection**,
the scenario raises `sampling.duration` in a Service policy override,
well past the time a browser needs to press **Cancel** twice,
through the policy route the realm's `configure` flag already admits.
Without that override the two-second default can finish first,
and a **Cancel** on a terminal Collection is `409 collection_terminal`,
which is a green-looking scenario proving the wrong thing.

### The hostile principal is a fixture change, not a string

Dex's user carries `alice@example.com` and `oidcAuthBlock` maps that fixed value
(`test/e2e/scenarios_auth_test.go:114`, `:475`).
A principal carrying `<img src=x onerror=…>` therefore needs the fixture parameterized:
`dexConfig` and `deployDex` take the claim value,
the login keeps a well-formed email so it can succeed,
and the claim the gateway reads as the principal — named by `usernameClaim` — carries the hostile string,
with the realm mapping naming that exact value.
Setup fails if the issuer cannot be configured with it,
rather than the scenario running and proving half of what it says it proves.

**"No script ran" is proven by a sentinel, not by an absence.**
The payload sets a global when it executes;
the scenario asserts the document holds no element the string names,
that the container's markup holds the escaped form,
and that the sentinel is undefined after the load.
"No `img` element" and "no error in the log" together do not prove that nothing ran.

### The proofs

- [ ] **Add the dependency, the discovery, and the registration**

`go get github.com/chromedp/chromedp`, the `browser` type and its search, the two registry entries,
the two runner entries, and the lane-test expectations that move with them.
A run on a machine with no browser skips both scenarios and the rest of the lane is unaffected;
a run on this one drives them.

- [ ] **Write `console-oidc`**

| Step | Proof |
|---|---|
| a load with no session | the browser navigates to `/auth/login` of its own accord |
| the issuer's form | completed with the scenario's user and password |
| the return | the landing page brings the browser back to `/ui/?ns=…&svc=…` |
| the identity panel | names the issuer's user and the realm it mapped to |
| namespace, Service, and profile chosen | the profile URL field holds the URL *Flow* describes |
| **Download** | the completed download is the gzip-framed body the profile endpoint streams |
| the Collections table | lists the seeded Collection, and its row's detail shows the record |
| **Start collection**, pressed twice | exactly one `POST` in the network events, carrying `Content-Type: application/json` and one `Idempotency-Key`; a row for the new Collection appears |
| **Cancel** on that row, pressed twice | the row moves to `cancelled` and the button goes with the state |
| `ns` and `svc` carrying `<img src=x onerror=…>` | both appear through the "is not listed" message as text: no element either names, the escaped form in the container's markup, and the sentinel undefined |
| the same hostile query through the login round trip | the return path carries it and the landing page brings it back, which is where a path built by joining strings would show |
| the identity panel's principal, from the issuer claim carrying the same string | the same three assertions |
| every load of the scenario | no Content Security Policy violation entry and no uncaught exception |

- [ ] **Write `console-basic`**

The first `fetch` is answered `401` with `WWW-Authenticate: Basic`,
the `Fetch` domain delivers the challenge,
the test answers it with the lane's user and password,
and the page continues to the identity panel and to a completed profile download.
The challenge count is asserted to be one, which is what "without a second prompt" means here.
Whether Chromium drew a native dialog is not observed and is not claimed.
It needs no NATS and no PGO: nothing in it reaches a Collection.

- [ ] **Pin a Chromium in the workflow**

`.github/workflows/e2e.yml` gains a step before `mise run test:e2e` that installs a pinned Chromium,
asserts the executable exists, and exports its path as `PROFGATE_E2E_BROWSER`,
so the suite drives the version the workflow chose rather than whatever the runner image happens to carry.

- [ ] **Record the dependency**

`docs/specs/gateway.md` *Dependencies*: the chromedp row, and the sentence below the table
that counts the console's test-only modules.

- [ ] **Validate and commit**

```bash
mise exec -- go vet -tags e2e ./test/e2e/
mise run lint && mise run test && mise run check
mise run test:e2e
git add test/e2e/ go.mod go.sum .github/ docs/
git commit -m "test(e2e): drive the console in a browser"
```

The suite runs **before** this commit, not after it.
It is the only thing that executes what this task wrote,
so a commit made without it is a commit of unrun code.

---

## The guides, the changelog, and this plan's status

**Files:**
- Modify: `docs/console.md`, `docs/specs/gateway.md`, `docs/specs/auth.md`, `docs/specs/pgo.md`,
  `docs/specs/ui.md`, `.agents/rules/500-validation-and-workflow.md`, `CHANGELOG.md`,
  `docs/plans/roadmap.md`, `docs/plans/console-write-paths.md`

*Required by this revision and not yet made* in [`docs/specs/ui.md`](../specs/ui.md) is the list this task closes.
Every row of it that no earlier task took is below;
a row that lands leaves that table, and the table is empty when this task ends.

The *Dependencies* rows are the exception and are already gone:
that section says a module is recorded when the tests that need it land,
so the interpreter's row moved with `collectionmodel.js` and chromedp's with the browser scenarios.
The rows here describe behavior rather than modules,
and they land together so that the spec's pending table empties in one place a reader can find.

- [ ] **`docs/console.md`**

*Collections, read-only* becomes the Collections section:
the table it already describes, plus **Start collection** and **Cancel**,
what each needs of `pgo.enabled` and the two realm flags, the two-press confirmation,
and what the page does when an answer never arrives.
*What the console never does* drops "Write PGO state" and keeps the other two;
the policy editor stays named as absent.
*During a rolling update* is rewritten: an asset URL no longer depends on which replica answers,
and what remains is a release that adds or drops a file, which a reload recovers from.

- [ ] **`docs/specs/gateway.md`, three sections**

*Dependencies* was written by the two tasks that landed the modules, and nothing is left for it here.
*Layers*: the `internal/ui` rows for entity tags and `304`,
the `internal/httpapi` rows for the media-type step and the idempotency key,
and the two browser scenarios in the end-to-end row.
*What end-to-end proves*: the two browser scenarios.
*Failure Scenarios*: the rolling-update row —
stable paths remove the `404` for an asset both builds carry,
a release that adds or drops one still fails a load until the rollout converges,
and a reload then recovers.

- [ ] **`docs/specs/auth.md` *Testing***

The two authentication lanes gain the browser scenarios beside the wire proofs they already carry.

- [ ] **`docs/specs/pgo.md` *Create a Collection* and *Errors***

Whether `429 rate_limited` and `429 capacity_exhausted` carry `Retry-After`.
They do not: the gateway writes the header on `429 collection_in_progress` alone
(`internal/httpapi/pgo_collections.go:703`), and this records that fact rather than changing it.
A client reads the header when it is there and assumes a delay of its own when it is not,
which is what the console already does.

- [ ] **`.agents/rules/500-validation-and-workflow.md` *Before a PR***

"hashed assets" becomes the stable paths,
and the sentence saying the page's own JavaScript runs in no test becomes the two browser scenarios that run it.
The same sentence names `portmodel.js` as the one model an interpreter drives,
which has been wrong since `targetmodel.js` landed; it names all three.

- [ ] **`CHANGELOG.md`**

Under `Unreleased`: the console starts and cancels a Collection;
console assets serve at stable paths with an entity tag and `Cache-Control: no-cache`,
replacing the content-hashed tree, which is a change to every asset URL and to nothing a caller sends;
the suite drives the page in a headless Chromium.
The asset entry says what an operator will see during the upgrade that carries it:
the release that moves off hashed prefixes is one where neither build serves what the other's shell names,
so a console load can fail until the rollout converges, and a reload afterwards succeeds.

- [ ] **`docs/plans/roadmap.md`**

Item 7's `Shipped:` line names the pull request that lands this.

- [ ] **Flip this plan**

`Status:` becomes `Done` and line 4 becomes an `Outcome:` naming the commits that shipped it.
The file is deleted by the next commit that touches it,
per [Deleting a Finished Document](../../.agents/rules/900-design-and-review-loops.md#deleting-a-finished-document).

- [ ] **Run the end-to-end suite**

```bash
mise run test:e2e
```

`internal/ui` and `test/e2e` are both changed, so *Before a PR* requires it on the `current` lane.
Report what ran and what was skipped in the pull request description.

- [ ] **Validate and commit**

```bash
mise run lint && mise run test && mise run check
git add docs/ CHANGELOG.md
git commit -m "docs: the console writes and serves stable paths"
```

---

## Risks and What This Plan Does Not Cover

- **Stable paths remove one failure of a rolling update and not the others.**
  What they remove is the `404` for an asset both builds carry.
  What stays, and what *Layout and embedding* accepts rather than solves:
  a release that adds a file or drops one has a path the other build answers `404` for —
  `collectionmodel.js` is such a file in this very release —
  the rollout that moves off hashed prefixes is one where neither build serves what the other's shell names,
  and a load can combine a shell from one build with a module from another,
  which runs unless those two changed incompatibly.
  A reload once the rollout converges recovers all three,
  which `no-cache` makes fetch the current bytes.
  No rollout affinity, shared asset store, or staged compatibility route is added.
- **A browser is a property of the machine.**
  A developer without one runs the rest of the lane and sees two named skips.
  What that leaves unproven is *What is not proven*'s list, unchanged by this plan.
- **`app.js` grows.**
  Two controls, an armed state, and a timer are new branches in a file that no unit test executes.
  The decision table is in `collectionmodel.js` precisely so that the part of it worth testing is tested;
  what stays in `app.js` is wiring, and review is what holds it.
- **The identifier grammar is checked twice.**
  `urls.js` already refuses an identifier outside it before building a path,
  and `startOutcome` refuses one before selecting a record.
  Both are deliberate: the model must decide without reaching for `urls.js`, which it does not import.
