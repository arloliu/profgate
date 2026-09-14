# One Catalog Behind Both Menus, and a Search Across Them

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> Task 1 writes this design into the accepted specs, and no later task may start before it lands:
> [`000-agent-contract.md`](../../.agents/rules/000-agent-contract.md#document-authority)
> makes an accepted spec the design of record,
> and the code tasks below implement from that text and not from this one.

**Goal:** let a person find a Service on the console without knowing its namespace first,
and let them narrow a menu of hundreds without scrolling it.

Two things are hard on a large cluster today.
A person who knows a Service name but not its namespace has no way to look for it:
the page fetches the namespace list at load and the Service list only after a namespace is chosen
(`internal/ui/static/app.js:466-467`, `:915-947`),
so finding `checkout` across three hundred namespaces means choosing each namespace in turn.
And each menu is a bare `<select>` (`:1259`, `:1266`),
whose only narrowing is the browser's own typeahead —
a behavior this plan does not characterize, because no source in this repository defines it.

The catalog that answers the first is already built and discarded.
`serveListing` calls `Catalog(ctx, "")`, which lists every Service in the cache
(`internal/k8s/catalog.go:28-45`), applies the realm filter,
and keeps only the distinct namespaces (`internal/httpapi/listing.go:102-117`).

After this plan `GET /v1/catalog` answers that filtered catalog,
and the console holds it as **the one thing it knows about namespaces and Services**:
the namespace menu, the Service menu, and a new search field are all derived from it.
The page stops fetching `/v1/namespaces` and `/v1/namespaces/{ns}/services`,
which stay as routes for every other client.

**Why one source and not two.**
An earlier draft kept both listings and let the catalog answer feed the search alone.
That made two sources for one set of facts, and every question after it was a refereeing question:
which answer wins when an older namespace answer lands after a newer catalog,
what happens when the catalog knows a Service the Service list does not,
who owns the targets fetch when both can start it.
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
names a second coordination mechanism for one piece of state as the signal to stop patching.
Deriving both menus from the catalog removes the referee rather than specifying it:
there is no seeding, no answer ordering, and no shared ownership,
because there is only ever one answer.

**Architecture:** `internal/httpapi/routes.go` declares `/v1/catalog`;
`internal/httpapi/server.go` gains the route kind and counts it in every switch that enumerates kinds;
`internal/metrics/recorder.go` gains the `catalog` endpoint value;
`internal/httpapi/listing.go` answers the route from the cache read and the realm filter it already runs;
`internal/httpapi/openapi.json` and `docs/api.md` gain the path in that same commit,
because `TestOpenAPIDocumentRoutes` compares the document against the declarations
(`internal/httpapi/openapi_test.go:196-232`, `:373-377`)
and `scripts/check-repo.py` compares the guide's spelled-out route counts against the route table
(`scripts/check-repo.py:289-307`);
a sixth console module `internal/ui/static/catalogmodel.js` holds four pure functions;
`internal/ui/static/urls.js` gains `catalogURL` and loses `namespacesURL` and `servicesURL`;
`internal/ui/static/app.js` loses `loadNamespaces`, `loadServices`, and the Service listing's generation,
and gains the catalog fetch, two derivations, one writer of the selection, and the search;
`internal/ui/static/app.css` lays out the new controls;
`internal/ui/scan_test.go` and `internal/ui/vendor_test.go` count the sixth module;
`test/e2e/scenarios_console_test.go` follows the page;
`docs/specs/ui.md` and `docs/specs/gateway.md` carry the design,
and the repository's inventories of listing routes and model modules follow it.

No Kubernetes call, RBAC verb, configuration key, chart value, or NATS permission is added.
The command-line client is not in this plan.

**Spec:** this plan's Task 1 writes the design into [`ui.md`](../specs/ui.md),
which holds the listing routes, their shared request algorithm, their response shapes,
the console's controls, and the console's module layout,
and into [`gateway.md`](../specs/gateway.md),
which holds the route inventory, the request algorithm, the logging, and the metrics.
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.

- **The catalog route adds no Kubernetes access.**
  `Catalog(ctx, "")` is what `/v1/namespaces` calls today (`internal/httpapi/listing.go:102`),
  and `Catalog` reads the Service lister and issues no request (`internal/k8s/catalog.go:22-45`).
  The `Discovery` seam gains no method, the ClusterRole gains no rule,
  and the golden checks of [`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md)
  stay green with no edit to the golden file
  (`deploy/deploy_test.go:76-117`, `deploy/chart_test.go:348-356`).

- **The catalog is realm-filtered, never realm-refused.**
  `ui.md` *Request algorithm for the listing endpoints* step 6 refuses only the Service list,
  because that route names a namespace in its path;
  the namespace list is filtered instead (`docs/specs/ui.md:202-206`).
  The catalog names no namespace in its path and is filtered the same way:
  a realm that admits nothing gets `200` and an empty list, never `403 realm_denied`.

- **Over one cache read and one realm snapshot, the catalog and the two lists agree.**
  The names the catalog carries under a namespace are what the Service list derives for that namespace,
  and the catalog's distinct namespaces are what the namespace list derives.
  All three run `Catalog` and `filterCatalog` over the lister
  (`internal/httpapi/listing.go:102-117`, `:182-190`), so the property holds by construction.
  It is stated as equivalent filtering over the same data and not as equality between two responses:
  each request loads the configuration and calls the lister on its own
  (`internal/httpapi/server.go:400-401`), so two responses taken while the cluster moves may differ.
  A test states it over one immutable fake,
  because a later change that filtered one side differently would break it silently.
  The predicate is idempotent, so what the test catches is omitted or divergent filtering,
  not filtering applied twice.

- **The catalog takes no query parameter.**
  `serveListing` refuses any raw query before it reads anything (`internal/httpapi/listing.go:87-91`),
  and `ui.md` step 7 says none of the listing routes takes one.
  This route joins them: no `q=`, no `limit=`, no `cursor=`.
  Matching is the page's, over the answer it holds.

- **The page holds one source for namespaces and Services.**
  `state.catalog` is stored; the namespace menu's options, the Service menu's options,
  and the search's results are derived from it at render.
  Nothing else writes either list, so no two answers can disagree about what exists,
  and no rule is needed to say which of them wins.

- **The load sends the same three requests it sends today.**
  `/v1/whoami` at boot, then `/v1/limits` and the catalog (`internal/ui/static/app.js:437-467`).
  One of the three now carries more; none is added.

- **One method writes the selection after the constructor.**
  The constructor restores `ns` and `svc` from the query (`internal/ui/static/app.js:314`, `:353-361`).
  After that, `selectPair(ns, svc)` is the only writer of either,
  and `onNamespace`, `onService`, and a search result all go through it,
  so every path settles in the same state.
  Because the menus are derived, `selectPair` starts no listing fetch at all:
  it starts targets and Collections, and on a selection change nothing else does.
  A catalog answer starts them too, for a selection that has not had them yet,
  which is the first activation of a selection rather than a change of one.

- **A filter never removes the value its menu is showing, and never adds one the listing lacks.**
  `filterOptions` keeps an entry when it matches the query **or** equals the value passed as `keep`,
  and it only ever keeps entries the list already held.
  A bookmarked selection outside the catalog stays outside it,
  and the template goes on passing `""` to that menu with the line that says it is not listed,
  which is the accepted contract (`docs/specs/ui.md:689-693`, `internal/ui/static/app.js:1250-1275`).

- **The claim on a catalog request is an instance field, not component state.**
  `setState` merges into pending state and queues a render;
  `this.state` still reads the old value until it is applied,
  which is why the page's generations, `collectionsFor`, and the timer handles are all instance fields
  and not state (`internal/ui/static/app.js:335-352`).
  `this.catalogPending` is claimed and released synchronously and is what `loadCatalog` tests;
  `state.catalogLoading` exists only so the Refresh control can render disabled.
  A guard that read `state.catalogLoading` would let two presses in one tick both see `false`
  and both send a request.

- **One catalog request at a time, and one scheduled attempt behind it.**
  `loadCatalog` returns at once while `this.catalogPending` is true,
  so a Refresh or a Retry pressed beside a pending request starts nothing.
  It raises a `catalogSeq` of its own,
  and the callback it hands `request` as the retry runs only while that generation is still current.
  `settle` schedules that callback on `not_ready` and records it as the error's Retry control
  (`internal/ui/static/app.js:494-499`, `:527-533`, `:286`),
  so one wrapper covers the timer and the button,
  and a manual attempt that supersedes a scheduled one makes the timer a no-op, not a second request.
  Every settled outcome releases the claim, `not_ready` included:
  holding it across the wait would hang the page,
  because the scheduled call would meet the claim it is waiting on and return.

- **An invalidation that arrives during a request is not lost, and is consumed once.**
  A `404 service_not_found` while a request is pending sets `this.catalogAgain`,
  and the settling request reads it, clears it, and starts exactly one more —
  synchronously, after releasing the claim, so the follow-up's own claim succeeds.
  Without it the answer already in flight, computed before the Service went, would stand as the catalog.

- **A catalog answer activates each downstream fetch on its own condition.**
  `loadTargets` records the selection it fetched for in a `targetsFor`,
  as `maybeLoadCollections` already records `collectionsFor`
  (`internal/ui/static/app.js:335-340`, `:638-643`),
  and a catalog answer tests each separately rather than requiring that neither has run.
  A single combined condition would strand the other:
  a port change before the catalog answers runs `loadTargets` with no membership check of its own
  (`:1006-1010`, and the port menu has no disabled predicate at `:1286-1290`),
  and the Collections fetch that limits could not start would then never start either.
  A Refresh repeats neither fetch for a selection that has had them, because both conditions are met,
  while a Refresh that recovers a failed load starts the first of each, because neither has run,
  and a recovery whose catalog still lists the Service cannot restart the targets fetch that asked for it.

---

## Decisions

**Six tasks: the specs, the route, the filters, the one source, the search, and the close.**
The specs come first because the code tasks implement from them.
The route and both documents that describe it are one task,
because a commit that adds the route without the OpenAPI path,
or without the guide's route counts, leaves a required check red,
which is a commit nobody can bisect through.
The filters come next.
They narrow whatever list a menu is given,
so they are correct before and after the source changes underneath them,
and they introduce the module the later tasks fill.
The one-source change is its own task because it preserves every selection behavior
while changing where the menus come from, how fresh they are, and how a missing Service is recovered.
The existing browser scenarios prove the first of those and cannot prove the others,
which is why it carries cases of its own.
The search rides last, on a page that already holds the catalog.
Each task also repairs the inventories its own change falsifies.

**`/v1/catalog`, not `/v1/services`.**
`{"services": [...]}` already names an array of strings on the namespace-scoped listing
(`internal/httpapi/listing.go:23-25`).
A second route answering `{"services": [...]}` at an array of objects is the shape a client misparses,
and this design names the thing a catalog everywhere else —
the seam's `Catalog`, the handler's `filterCatalog`, the metrics value.

**A flat array of namespace-and-name pairs, not a grouping.**

```json
{"catalog": [{"namespace": "orders", "name": "checkout"}, {"namespace": "payments", "name": "ledger"}]}
```

It is one row a pair, which is what the search matches and what a result renders,
so the handler has no grouping step and the page has no ungrouping one.
`k8s.ServiceRef` carries no JSON tags (`internal/k8s/catalog.go:13-16`)
and `writeJSON` uses the ordinary encoder, so the response is a view type with lowercase tags,
as the other listing bodies are.

The cost of flatness is bytes on the wire.
These are estimates from the encoding, not measurements.
At fifteen ASCII characters in each name, one compact pair is 56 bytes and 57 with its separator,
so five thousand pairs are about **280 KiB**;
grouped, a Service name alone costs about 18 bytes,
so the same five thousand names are about 88 KiB before namespace keys.
Nothing in `internal/httpapi` sets `Content-Encoding`, so neither is compressed.
The response is proportional to the number of Services the realm admits and the handler imposes no ceiling
(`internal/httpapi/listing.go:182-190`);
naming namespaces in a realm bounds which namespaces are admitted, not how many Services they hold.
Compression is a gateway-wide decision about every response, not this route's.

**The catalog is fetched at load, because both menus are derived from it.**
An earlier draft fetched it lazily to keep a search off the load path.
That is not available to a design whose namespace menu comes from it:
a menu the page cannot draw until someone types is worse than a larger first response.
The load still sends three requests, one of them larger,
and the page drops the per-namespace Service fetch it used to send on every namespace change.

**Freshness is one Refresh, and the Service panel carries it.**
Today the namespace list is never refetched after load
and the Service list is refetched whenever a namespace is chosen
(`internal/ui/static/app.js:562-573`, `:939-946`).
After this plan one **Refresh** on the Service panel refetches the catalog and redraws both menus,
which is more freshness than the namespace menu has now and less than the Service menu has now.
The `404 service_not_found` recovery that refetches the Service list today refetches the catalog instead
(`internal/ui/static/app.js:901-907`).
Automatic polling stays a non-goal, as it is for the targets and Collections tables.

**A rendered cap on the results, and no truncation on the wire.**
The page holds the whole catalog and renders at most fifty result rows,
saying how many matched when there are more.
This is a rendering decision with the truth in hand, which a person narrows by typing more.
A `truncated` flag in the response was rejected:
a caller that must render "there is more, and I will not say what" is worse served than one that holds the answer.

**Four pure functions in one module.**
`catalogmodel.js` holds `namespacesOf(catalog)`, `servicesOf(catalog, ns)`,
`filterOptions(list, query, keep)`, and `searchCatalog(catalog, query, limit)`.
The two derivations are what make the menus derived rather than stored,
and the two matching functions share one rule:
trim the query, lowercase both sides, test for a substring,
with `searchCatalog` matching `namespace + "/" + name` as one string so `payments/check` narrows by both.
Two matching rules would mean a person who learned the search field guesses wrong at the filter field.
The module is introduced in the filter task holding one function and filled by the two after it.

**Both the search and the filters exist, because they answer different questions.**
The search finds a Service across namespaces.
The filter narrows a menu already chosen: a shared namespace holds hundreds of Services,
and the Service menu earns its filter even when the namespace is known.

**The filter fields are always rendered, with no threshold.**
A control that appears at some number of options and not below it is more design surface, not less:
*Controls* is a contract, so a threshold would be a number in that contract with a rule on each side of it.
An empty filter changes nothing, and a three-namespace cluster pays one empty input.

**A sixth module under `internal/ui/static/`.**
`consoleSources()` is the fixed application-module inventory,
and `TestScanCoversEveryJSFile` compares it against the tree (`internal/ui/scan_test.go:15-17`, `:395-424`),
so that list and the import-free list (`internal/ui/vendor_test.go:306`) are the two code-side edits;
the asset and entity-tag tests walk the tree and need none
(`internal/ui/ui_test.go:137-160`, `:361-388`).

**The two listing routes stay, and the page stops calling them.**
`/v1/namespaces` and `/v1/namespaces/{ns}/services` are the command-line client's
(`cmd/profgate/read.go:127-163`) and every other client's.
Only the console's use of them ends, which is why `urls.js` loses two builders that nothing else imports.

**The command-line client is not in this plan.**
`profgate services` keeps its required namespace positional.
A cross-namespace form of that verb is a `cli.md` revision of its own.

---

## Global Constraints

- **No new Kubernetes call, RBAC verb, configuration key, chart value, or NATS permission.**
- **No change to `internal/k8s`.**
  `Catalog` already answers this, for every namespace, with the argument it already takes.
- **Every control the page gains is a native `<select>`, `<input>`, `<button>`, or `<a>`.**
  *Non-goals* declares accessibility work beyond what Pico and plain HTML give,
  on the ground that every control is one of those four (`docs/specs/ui.md:124-125`).
  A search field is an `<input>` and a result row is a `<button>`;
  a listbox with roles and keyboard management is what that bullet rules out.
- **`app.js` spells no `/v1` path.**
  *Rendering response values* forbids a string literal beginning with a gateway prefix,
  and `TestScanPathsLiveInURLs` enforces it; the catalog's URL is built in `urls.js`.
- **Every task that changes behavior writes its test first and shows it red before the change,
  and says of every test it adds whether that test could have failed.**
  Task 4 keeps every selection behavior and changes menu freshness and recovery;
  its statement is which existing scenarios prove what it kept,
  and which of its new cases could have failed before it.
- **No e2e assertion counts the page's total requests.**
  The recorder holds the navigation and every asset too (`test/e2e/browser_test.go:323-330`).
  Requests are counted from a boundary taken with `s.requestCount()`, per route.
- **Several cases of one shape are named table cases** with `t.Run(tc.name, ...)`
  ([`300-testing.md`](../../.agents/rules/300-testing.md)).
- **No jargon:** comments, commit messages, and documentation state the current fact,
  never this plan's ordering or a task's name
  ([`AGENTS.md`](../../AGENTS.md#no-jargon-anywhere)).
- Markdown prose and Go doc comments use semantic line breaks, and so do the comments in `app.js`;
  run `semlf check` on every file a task edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters,
  with a body that says what changed and why, one sentence per line under 120 characters,
  and no trailer of any kind ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
  A commit is finished when `git log --oneline -1` shows it and `git status --short` is clean,
  because the hook can refuse a message after `git commit` has already run.
- **Every task ends with the same block before its commit**, the two that change no code included,
  because the linter runs before every commit:

```bash
mise run lint && mise run test && mise run check && mise run prose
```

---

## File Structure

```text
docs/specs/ui.md                      # the fifth listing route, its shape, and its ordering; the page's one
                                      # source; the search, the two filter fields, the Refresh, and the single
                                      # writer of the selection; the catalog's error surface; the sixth module;
                                      # every "four listing routes" and "three model modules" inventory in the
                                      # document; the gateway.md edits it requires; the amendment row
docs/specs/gateway.md                 # the fifth listing route in HTTP API, Request algorithm, Listing
                                      # endpoints, Errors, Logging, Metrics, Layers, the request-identifier and
                                      # OpenAPI inventories, the disabled-console row, and the console testing
                                      # description
internal/httpapi/routes.go            # the /v1/catalog declaration
internal/httpapi/server.go            # kindCatalog, and every switch that enumerates kinds
internal/httpapi/listing.go           # the catalog body type and the branch that answers it
internal/httpapi/pgo.go               # the Collection-route exclusion branch lists the new kind
internal/httpapi/openapi.json         # the path, its response, its errors, and its request-id header
internal/metrics/recorder.go          # EndpointCatalog, and Recorder.Request's listing-endpoint comment
docs/api.md                           # the route, and the four spelled-out route counts check-repo compares
AGENTS.md                             # five listing endpoints
.agents/rules/100-project-map.md      # five listing routes
docs/deployment.md                    # the endpoint label gains catalog, and the counts beside it
internal/ui/static/catalogmodel.js    # namespacesOf, servicesOf, filterOptions, searchCatalog
internal/ui/static/urls.js            # catalogURL; namespacesURL and servicesURL go
internal/ui/static/app.js             # the catalog fetch and its Refresh; both menus derived; selectPair; the
                                      # search field and its results; the two filter fields; loadNamespaces,
                                      # loadServices, and servicesSeq go
internal/ui/static/app.css            # the search panel and the filter rows
internal/ui/scan_test.go              # catalogmodel.js in consoleSources; the import and export scans
internal/ui/vendor_test.go            # catalogmodel.js imports nothing
internal/ui/catalogmodel_test.go      # the four functions, evaluated
.agents/rules/500-validation-and-workflow.md  # four model modules
test/e2e/scenarios_console_test.go    # the page follows one catalog; the search; the filters
docs/console.md                       # the guide describes the search, the filters, and the Refresh
CHANGELOG.md                          # one entry per behavior
docs/plans/cross-namespace-service-catalog.md  # this file
```

---

## 1. The design, in the two specs

Nothing runs in this task; it writes the text the rest of the plan implements from.
Every count below is a live inventory to correct, never an entry in an amendment table,
which records what a past revision did and stays as written.

**`docs/specs/ui.md`**

- [ ] *Overview* and *Core decisions*: the endpoint count and the filtered-catalog sentence (`:8`, `:47-56`).
- [ ] *Routes*: the table (`:146-147`) gains a `/v1/catalog` row — `GET`, authenticated, `filter`, always offered;
      and the section's own counts outside the table follow it (`:136`, `:153-156`).
- [ ] *Request algorithm for the listing endpoints*: five routes, here and wherever the section counts them
      (`:186-215`).
      Step 1 says the catalog's path captures nothing.
      Step 6 says the catalog is filtered rather than refused, for the reason the namespace list is (`:202-206`).
      Step 8 says it calls `Catalog` with the empty namespace, applies the realm filter,
      and answers `200` with the pairs in namespace-then-name order;
      an error from `Catalog` is `503 discovery_unavailable`, as it is for the other two.
- [ ] *Response shapes*: the preamble's count and its ordering wording (`:278-287`),
      and a subsection for the catalog carrying the flat pair array, its namespace-then-name order,
      and the sentence that over one cache read and one realm snapshot its names under a namespace
      are what the Service list derives and its distinct namespaces are what the namespace list derives.
- [ ] *The realm filter*: the catalog is a third place the filter runs, over `filterCatalog` alone.
- [ ] *Flow*: the diagram (`:474-478`) is rewritten —
      the load is `/v1/whoami`, `/v1/limits`, and `/v1/catalog`;
      both menus and the search come from the catalog;
      the page sends no namespace or Service listing.
- [ ] *Controls*: **The page's one source** — `state.catalog` is stored, both menus are derived,
      and the two listing routes the page no longer calls remain for other clients.
- [ ] *Controls*: the table gains the search field, the two filter fields, and the catalog's Refresh —
      what each offers, its default, that none of the four is sent anywhere, and what a change does.
      Beneath it, the rules of `catalogmodel.js`:
      `namespacesOf` and `servicesOf` deriving the menus in the catalog's order,
      `filterOptions` keeping an entry that matches **or** equals `keep` and never adding one the list lacks,
      `searchCatalog` matching `namespace/name` as one string,
      and the fifty-row rendered cap with its count line.
- [ ] *Controls*: **The selection** — `selectPair(ns, svc)` as the only writer after the constructor,
      with the transition table Task 4 implements, case by case,
      naming every field cleared, every error key cleared with `clearError`, every generation raised,
      and the two fetches it starts.
- [ ] *Controls*: the existing Refresh contract (`:719-739`) gains the catalog's Refresh.
      It refetches the catalog and redraws both menus from the answer.
      It repeats no downstream fetch the selection has already had,
      and it does start a first targets or Collections fetch the selection is still owed —
      which is how a load whose catalog failed recovers.
- [ ] *Errors* (`:1084-1090`, `:1095`) and *Failure scenarios* (`:1372-1375`, `:1423-1440`, `:1432`):
      one `catalog` error key in place of `namespaces` and `services`;
      where its error, sign-in, and retry controls render;
      what a failed load and a failed Refresh leave on screen;
      the retry lifecycle, its generation, and the coalesced invalidation;
      and the three `service_not_found` recoveries, the profile download included.
- [ ] *Errors* **When the identity disclosure opens** (`:1118-1128`):
      the Service listing's generation is gone with the listing,
      and what each remaining path discards before it records an answer is restated over the
      generations that remain.
- [ ] *Layout and embedding*: the tree gains `catalogmodel.js`, and `app.js`'s import list gains it
      and loses nothing else (`:1176-1201`).
- [ ] *Unit* (`:1513-1520`, `:1652-1675`), *What is not proven* (`:1691`, `:1716`, `:1785-1786`),
      *Dependencies* (`:1973`), and *Package layout* (`:2010-2014`):
      the fourth model module, the fifth listing route in the HTTP route, cache-error, disclosure,
      and endpoint inventories, and the metrics-value count.
- [ ] *Audit and metrics*: the `endpoint` label gains `catalog`;
      the audit record carries `namespace` as the empty string,
      which is the shape the writer always emits and the shared test requires
      (`internal/httpapi/audit.go:57-66`, `internal/httpapi/listing_test.go:738-748`).
- [ ] *End to end*: the browser cases Tasks 3 to 5 add, and the case that goes —
      a Service listing answered for a namespace the page had left has no listing to answer.
- [ ] *Changes to the accepted designs* and *Required by this revision and not yet made* (`:2123-2127`):
      the rows naming `gateway.md` gain the fifth route and the `catalog` label,
      and the pending table stays empty because Task 1 lands both specs together.
- [ ] *Amendments*: one row naming the sections this revision edits.

**`docs/specs/gateway.md`**

- [ ] *HTTP API* (`:732-734`): five listing routes, `/v1/catalog` named among them.
- [ ] *Request algorithm* (`:795-801`): the listing tail counts five,
      and the realm check refuses the Service list alone while the namespace list and the catalog are filtered.
- [ ] *Listing endpoints*: five response shapes, pointing at [`ui.md`](../specs/ui.md) *Response shapes*.
- [ ] *Errors*: `503 discovery_unavailable` covers the catalog's cache read.
- [ ] *Logging* (`:1508`): five listing routes, `namespace` set on the Service list alone.
- [ ] *Metrics* (`:1575`): the `endpoint` vocabulary gains `catalog`.
- [ ] The request-identifier and OpenAPI route inventories (`:1196`, `:1263`),
      and the disabled-console row (`:2610`): five listing routes.
- [ ] *Layers* and the console testing description (`:1992-1995`): the fourth model module.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
git log --oneline -1 && git status --short
```

- [ ] Commit: `docs(specs): put one catalog behind both menus`

---

## 2. The route

Write the tests first and run them red.

- [ ] `internal/metrics/recorder.go`: `EndpointCatalog Endpoint = "catalog"` with the doc comment its siblings carry,
      and `Recorder.Request`'s listing-endpoint list gains it (`:59-64`).
- [ ] `internal/httpapi/server.go`: `kindCatalog` in the `routeKind` block;
      `isListing` returns true for it;
      `isAuthRoute`, `isPGO`, `isPGOWrite`, and `isCollectionScoped` list it among the kinds they answer false for;
      `labels()` returns `EndpointCatalog`.
- [ ] `internal/httpapi/auth.go`: `authRouteName` lists it among the kinds that name no auth route.
- [ ] `internal/httpapi/realm.go`: `pgoAllows` lists it among the kinds with no PGO flag.
- [ ] `internal/httpapi/pgo.go:150-161`: the Collection-route switch lists it among the kinds it refuses.
- [ ] `internal/httpapi/routes.go`: `{"/v1/catalog", kindCatalog, []string{http.MethodGet}}`,
      beside the two listing rows it belongs with (`:62-63`).
- [ ] `internal/httpapi/listing.go`: `catalogBody` holding `Catalog []serviceRefView`,
      each `{namespace, name}` with lowercase JSON tags, the slice initialized so an empty answer is `[]`;
      the `kindNamespaces, kindServices` case gains `kindCatalog` and its branch,
      which appends the filtered refs in order and sorts nothing,
      because `Catalog` sorts by namespace then name before it returns (`internal/k8s/catalog.go:43-45`).
- [ ] `internal/httpapi/openapi.json`: the `/v1/catalog` path, its `200` shape, its error codes,
      and the `X-Request-Id` header every response documents.
      The document's own checks cover canonical encoding, resolvable references, and parameters
      (`internal/httpapi/openapi_test.go:296-310`, `:359-370`, `:585-610`, `:637-651`).
- [ ] `docs/api.md`, **in this commit and not the last one**:
      the route, its response, and its errors;
      and the four spelled-out counts `scripts/check-repo.py` compares against the route table —
      fifteen `/v1` routes becomes sixteen (`:67`, `:121`, `:1002`)
      and six unnamed routes becomes seven (`:81`),
      with the listing sentence beside them counting five (`:82`).
      `scripts/check-repo.py:289-307` is the list of sentences it checks;
      `mise run check` fails without these edits.
- [ ] `AGENTS.md:25` and `.agents/rules/100-project-map.md:121-128`: five listing routes.
- [ ] `docs/deployment.md:502-503`: the `endpoint` label gains `catalog`,
      and both the listing-route count and the total endpoint-value count beside it.

**Tests.**
These three would leave the commit red if the kind were added without them, so they are part of it:

- [ ] `internal/httpapi/routes_test.go:66-100`: `endpointOf` gains the catalog case.
      Without it the expectation falls back to `EndpointProfile`
      and `TestRouteTableAcceptedMethod` compares that against the handler (`:226-240`).
- [ ] `internal/httpapi/listing_test.go:677-708`: `TestRouteKinds` gains its row, whose order matches the enum.
- [ ] `internal/httpapi/listing_test.go:26-35`: `listingPaths()` gains the catalog,
      which carries the new route into the shared method, readiness, authentication, redirect,
      and audit cases (`:148-249`, `:726-751`);
      the endpoint map in `TestListingAuditAndMetrics` follows it (`:726-732`),
      and the port-disclosure condition, which names only the namespace and Service paths today,
      gains the catalog (`:597-598`),
      and the audit assertion expects `namespace` as the empty string, as the namespace listing does
      (`:738-748`).

The rest, in `internal/httpapi/listing_test.go`:

- [ ] The catalog answers every admitted pair, in namespace-then-name order, over the harness's `realmLists`
      (`:60`), with duplicate Service names across namespaces and all four realm-filter combinations.
- [ ] A realm that admits no namespace gets `200` and an empty array, never `403`.
      This is the invariant that separates this route from the Service list; it could fail, and must.
- [ ] The catalog is asked for with an empty namespace argument, once, which the fake records
      (`internal/httpapi/fixtures_test.go:169-183`).
- [ ] Over one immutable fake, the catalog's names under each namespace are what the Service list answers,
      and its distinct namespaces are what the namespace list answers.
- [ ] The response keys are lowercase and an empty catalog is `[]`, not `null`.
- [ ] Any query parameter is `400 invalid_parameter`, `access_token` included,
      and the refusal happens before any cache read.
- [ ] `POST` is `405` with `Allow: GET`.
- [ ] A `Catalog` error is `503 discovery_unavailable`.
- [ ] The response carries no address, `podIP`, or port —
      the disclosure assertion the other listing responses run (`:581-605`).
- [ ] The metrics label of a catalog request is `catalog` with profile `none`.
- [ ] The golden ClusterRole and chart rule tests run unchanged and green, with no edit to the golden file.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
git log --oneline -1 && git status --short
```

- [ ] Commit: `feat(httpapi): answer the whole Service catalog`

---

## 3. The two filter fields

No request changes in this task.
It narrows two menus, whichever list is behind them, which is why it is correct before Task 4 and after it.

- [ ] `internal/ui/static/catalogmodel.js`: `filterOptions(list, query, keep)`.
      It trims and lowercases the query and keeps an entry when the entry's lowercase value holds it
      as a substring **or** the entry equals `keep`.
      It never adds an entry the list lacks, preserves the input order, does not mutate the input,
      and returns a new array holding every entry when the query is empty.
      The module imports nothing and ends with one export statement as its last statement,
      the shape `cutExport` requires (`internal/ui/portmodel_test.go:28-44`).
- [ ] `internal/ui/scan_test.go`: `catalogmodel.js` in `consoleSources()` (`:15-17`);
      an import scan holding `app.js` to importing and calling `filterOptions`,
      as the three model modules have;
      and an export scan asserting the module's export statement names exactly what `app.js` imports —
      the interpreter cuts that statement and reads globals
      (`internal/ui/portmodel_test.go:30-55`, `:130-145`),
      so evaluating the model proves nothing about what the browser can import.
      Both scans name `filterOptions` alone here and grow in Tasks 4 and 5.
- [ ] `internal/ui/vendor_test.go:306`: `catalogmodel.js` among the modules that import nothing.
- [ ] `internal/ui/static/app.js`: state gains `nsFilter` and `svcFilter`;
      `renderSelection` (`:1249-1278`) draws a labelled `<input type="search">` beside each menu,
      as a sibling `<label>` in a shared wrapper and never nested inside the menu's own `<label>`,
      so a `<label>` that holds a `<select>` still holds exactly one control
      and `chooseOption` still finds it (`test/e2e/scenarios_console_test.go:1354-1357`).
      Each menu's options come from `filterOptions` with that menu's current value as `keep`,
      and a line beside each says how many of how many are shown while the query is not empty.
      Choosing a namespace clears `svcFilter`.
- [ ] `internal/ui/static/app.css`: the filter field and its menu inside one field cell.
- [ ] `.agents/rules/500-validation-and-workflow.md:93-95`: four model modules.

**Tests**

- [ ] `internal/ui/catalogmodel_test.go`, table cases evaluated in the interpreter `portmodel_test.go` uses:
      an empty query returns every entry; a query matches case-insensitively and mid-string;
      a query is trimmed; `keep` survives a query that excludes it;
      `keep` is not duplicated when it also matches;
      a `keep` the list lacks is **not** added; an empty list stays empty;
      the order is the input's; the input array is not mutated.
- [ ] `test/e2e/scenarios_console_test.go`: typing in the namespace filter narrows its menu;
      the chosen namespace stays selectable while the query excludes it;
      a bookmarked namespace outside the listing is still absent from the options
      and still draws its not-listed line;
      both menus filtered at once; the Service filter clears when the namespace changes;
      the page's query string is unchanged by any of it;
      and `select.value` is read from the DOM after each render rather than inferred.
- [ ] The existing console scenarios run before the change and after it,
      which is what proves `chooseOption` survived the added labels.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
mise run test:e2e
git log --oneline -1 && git status --short
```

- [ ] Commit: `feat(ui): narrow the two Service menus`

---

## 4. One catalog behind both menus

Every selection behavior survives this task, and three things change:
the menus come from one answer, a namespace change no longer refetches anything,
and one **Refresh** refetches both menus.
The existing console and authentication scenarios staying green is what proves the first;
the freshness and recovery changes need the cases below, because no existing scenario reaches them.

### What goes

- [ ] `internal/ui/static/app.js`: `loadNamespaces`, `loadServices`, the `servicesSeq` field and every use of it,
      and the `stale` predicate the Service listing passed (`:562-595`, `:335-340`).
      There is no second listing whose late answer can arrive for a namespace the page has left,
      because there is no second listing.
- [ ] `internal/ui/static/app.js`: the `namespaces` and `services` state fields.
      Both become derivations of `state.catalog` at render.
- [ ] `internal/ui/static/urls.js`: `namespacesURL` and `servicesURL`, which nothing else imports,
      and their imports in `app.js:9-10`.
- [ ] The error keys `namespaces` and `services`, and the two `panelError` calls that render them
      (`:1273`, `:1275`), replaced by one `catalog` key and one call.

### What arrives

- [ ] `internal/ui/static/urls.js`: `catalogURL()`, built with `build("/v1", ["catalog"])`.
- [ ] `internal/ui/static/catalogmodel.js`: `namespacesOf(catalog)`, the distinct namespaces in the
      catalog's order, which is already namespace-then-name, so it compacts and does not sort;
      and `servicesOf(catalog, ns)`, the names under `ns` in the catalog's order.
- [ ] `internal/ui/static/app.js`: state gains `catalog`, `catalogLoaded`, and `catalogLoading`,
      the last of which only the Refresh control renders from;
      the instance gains `catalogPending`, `catalogSeq`, `catalogAgain`, and `targetsFor`,
      beside the counters and the `collectionsFor` it already holds (`:335-352`).
      `loadAfterBoot` calls `loadLimits()` and `loadCatalog()` (`:465-468`).
- [ ] `internal/ui/static/app.js`: `loadCatalog` is the whole lifecycle, and it is this shape:

```text
loadCatalog():
  if (this.catalogPending) return                 // the claim is synchronous, so two presses
  this.catalogPending = true                      // in one tick cannot both pass it
  const seq = ++this.catalogSeq
  setState({catalogLoading: true})
  const retry = () => { if (seq === this.catalogSeq) this.loadCatalog() }
  const body = await request("catalog", catalogURL(), retry)
  this.catalogPending = false                     // released before anything else runs
  const again = this.catalogAgain
  this.catalogAgain = false
  if (body) setState({catalog: body.catalog, catalogLoaded: true, catalogLoading: again}, activate)
  else      setState({catalogLoading: again})     // the catalog it had stays, the error beside it
  if (again) this.loadCatalog()
```

      `retry` is what `settle` schedules on `not_ready` and what it records as the error's Retry
      control (`:494-499`, `:527-533`, `:286`), so the timer and the button are one wrapper,
      and a later attempt makes an earlier timer inert by raising the generation.
      Every settled outcome releases the claim, `not_ready` included.
- [ ] `internal/ui/static/app.js`: `activate` is the callback above, and it tests each fetch separately:

```text
activate():
  if (!this.selectionListed()) return
  if (this.targetsFor !== this.selection) this.loadTargets()
  this.maybeLoadCollections()                     // owns its own collectionsFor check
```

      `loadTargets` records `targetsFor` as `maybeLoadCollections` records `collectionsFor` (`:638-643`).
      One condition over both would strand the other:
      a port change before the catalog answers reaches `loadTargets` with no membership check of its own
      (`:1006-1010`), and the Collections fetch that limits could not start would then never start.
      This is the bookmark-restore path `loadNamespaces` used to run through `loadServices`
      (`:568-572`, `:588-594`).
      `maybeLoadCollections` waits on limits and the catalog rather than limits and the Service list,
      and is still started by whichever answers last (`:550-558`, `:638-643`).
- [ ] `internal/ui/static/app.js`: a **Refresh** on the Service panel calls `loadCatalog`,
      disabled while `state.catalogLoading` is true, as the targets and Collections controls are
      (`:1412-1420`, `:1519-1525`).
- [ ] `internal/ui/static/app.js`: `selectionListed()` is derived from the catalog rather than from the
      two deleted arrays (`:1141-1143`).
      It reads the whole catalog and never a filtered menu,
      because it gates the Collections view, the profile URL, and the start control
      (`:638-643`, `:1153-1156`, `:1489-1492`),
      none of which may turn on what a person typed into a filter field.
- [ ] `internal/ui/static/app.js`: the two `service_not_found` recoveries refetch the catalog.
      `afterServiceError` covers targets and Collections (`:901-907`),
      and `onDownload` covers the profile download (`:1073-1075`),
      whose `setState({ downloading: false })` keeps running after the recovery starts (`:1078`).
      Each sets `catalogAgain` instead when a catalog request is already in flight.
      These three are the only refetches that happen without a press.
- [ ] `internal/ui/static/app.js:63`: the `service_not_found` hint says the page refreshes the Service
      list, which it no longer has; it names the catalog instead.
      The `no_targets` hint beside it (`:64-65`) names the targets list's own Refresh and stands.
- [ ] `internal/ui/scan_test.go`: the import and export scans gain `namespacesOf` and `servicesOf`.

### The selection transition

`selectPair(ns, svc)` is added, and `onNamespace`, `onService`, and a search result all call it.
Because both menus are derived, it starts no listing fetch,
and it is the only thing that starts targets and Collections on a selection change.

**It has one case, not three.**
Identical arguments produce identical behavior, whichever caller passed them:
there is no namespace branch, because with the Service list derived there is nothing to clear or fetch
when only the namespace moves.

```text
selectPair(ns, svc):
  targetsSeq++; collectionsSeq++; selection++
  writeQuery(ns, svc)
  setState({ns, svc, and the cleared fields below}, then:
    clearError("targets"); clearError("collections")
    clearWriteControls()
    if (svc) { loadTargets(); maybeLoadCollections() })
```

Cleared with the selection: `targets`, `targetSummary`, `targetsLoading`,
`collections`, `collectionsNote`, `collectionsLoading`, `collection`, `pod`, `version` —
the fields both handlers clear today (`internal/ui/static/app.js:920-943`, `:953-973`).

The three callers:

| Caller | Call | Its own extra work |
|---|---|---|
| `onNamespace` | `selectPair(ns, "")` — the menu always clears the Service | clears `svcFilter` |
| `onService` | `selectPair(this.state.ns, svc)` | none |
| a search result | `selectPair(ns, name)` | clears `svcFilter` |

**Filter clearing belongs to the callers, not to `selectPair`.**
It is the one thing that differs between a Service-menu pick and a result click naming the same pair.
A function whose behavior depended on which of them called it would be a function whose arguments lie.

Four rules the sketch does not carry on its face:

- **Clearing either menu is the same call.**
  Clearing the namespace is `selectPair("", "")` from `onNamespace`;
  clearing the Service is `selectPair(ns, "")` from `onService`.
  The `if (svc)` condition is what makes both start nothing.
- **The same pair chosen again runs the same transition.**
  `selectPair` tests no equality, so a result clicked twice refetches its targets,
  which is what its Refresh does.
  This is not hypothetical in tests.
  `chooseOption` dispatches `change` even when it assigns a value the menu already holds
  (`test/e2e/scenarios_console_test.go:1354-1363`),
  so a repeated selection is reachable there whatever a browser does on its own.
- **`clearError(key)` is called, not an assignment to `errors`.**
  It clears the `signIn` entry beside the error (`internal/ui/static/app.js:543-547`),
  and leaving a stale sign-in control on screen is what an assignment would do.
- **`clearWriteControls()` is called, not imitated.**
  It transitions the start attempt, disarms a cancel, cancels the cancel retry timer,
  and clears the write errors (`internal/ui/static/app.js:690-702`).

`writeQuery(ns, svc)` runs before the state is applied, as it does in both handlers today
(`:923`, `:956`),
and the error clearing, the write-control cleanup, and the two fetches run in the `setState` callback,
after the new selection is applied, as they do today (`:930-946`, `:963-977`).

**Tests**

- [ ] `internal/ui/catalogmodel_test.go`: `namespacesOf` over an empty catalog, one namespace,
      several namespaces each with several Services, and a catalog whose order it must preserve;
      `servicesOf` for a namespace with none, one, and several, and for a namespace the catalog lacks;
      neither mutates its input.
- [ ] `test/e2e/scenarios_console_test.go`: the load sends `/v1/whoami`, `/v1/limits`, and `/v1/catalog`,
      and sends neither listing route, asserted per route with `s.sentTo` and never as a total.
- [ ] `test/e2e/scenarios_console_test.go`, the freshness change, which no existing scenario reaches:
      a Service created after the catalog answered is absent from the menu,
      changing namespaces sends no request at all,
      and **Refresh** makes it selectable.
- [ ] `test/e2e/scenarios_console_test.go`: a bookmarked selection is drawn once the catalog answers,
      and on a load where nothing else is touched the catalog's answer is what starts its targets fetch.
      This could fail: it is the path `loadNamespaces` used to own.
      The assertion is scoped to an untouched load, because a port change reaches `loadTargets`
      on its own (`internal/ui/static/app.js:1006-1010`) and this plan does not gate that.
- [ ] `test/e2e/scenarios_console_test.go`: **Refresh** on the Service panel sends exactly one catalog request
      from a boundary, is disabled while that request is held, and redraws both menus from the answer.
- [ ] `test/e2e/scenarios_console_test.go`, the retry lifecycle, counted per route from a boundary:
      a catalog answered `503 not_ready` clears the loading state and schedules one attempt;
      that attempt sends exactly one request;
      a Refresh pressed during the wait sends one request and makes the scheduled attempt a no-op,
      so no late request follows it;
      a Refresh or Retry pressed while a request is held sends nothing;
      and a repeated `not_ready` schedules one attempt each time, never two.
      Every one of these could fail, and the first would fail against a guard held across the wait.
- [ ] `test/e2e/scenarios_console_test.go`, the synchronous claim:
      two presses dispatched in one evaluated expression send one request, not two.
      `s.eval` runs a single expression that can carry both actions
      (`test/e2e/browser_test.go:567-573`), so this needs no export from the page.
      It could fail, and would against a guard that read component state.
- [ ] `test/e2e/scenarios_console_test.go`: several `404 service_not_found` answers during one held
      catalog request start exactly one further request when it settles;
      a recovery whose catalog still lists the Service starts no second targets fetch,
      which is what stops the two from recovering at each other;
      and when that follow-up succeeds after a `not_ready`, the earlier timer sends nothing.
- [ ] `test/e2e/scenarios_console_test.go`, a load whose catalog failed and recovers through Refresh:
      the answer starts the first targets and the first Collections fetch, one each,
      and a further Refresh starts neither again.
- [ ] `test/e2e/scenarios_console_test.go`, the two activation conditions, counted separately:
      with the catalog held, let limits answer, change the port so targets are fetched,
      then release the catalog — the Collections fetch still starts.
      This is the case a single combined condition fails.
- [ ] `test/e2e/scenarios_console_test.go`: a catalog answered `503` on a Refresh keeps the earlier menus
      with the error beside them.
      A `401` showing the sign-in control is asserted under `basic`, or under `oidc` in the returned state,
      because an ordinary eligible `oidc` `401` navigates to the login instead
      (`internal/ui/static/app.js:432-433`, `:512-524`).
- [ ] `test/e2e/scenarios_console_test.go`: a profile download answered `404 service_not_found`
      refetches the catalog and still clears `downloading` (`internal/ui/static/app.js:1073-1078`).
- [ ] `test/e2e/scenarios_console_test.go`: with both filter fields carrying a query that matches
      neither selected value, both menus still show the pair — `filterOptions` keeps the current value —
      and the profile URL is still built and the Collections view still offered.
      The browser cannot tell this page from one whose membership read the retained menus,
      because a menu that retains its value answers the same;
      that `selectionListed()` reads the whole catalog is held by reading the source,
      which is what the scan tests are for.
- [ ] `test/e2e/scenarios_console_test.go`: the existing case that drives a Service listing answered for a
      namespace the page had left (`:453-467`) goes with the listing it drove.
      **The identity-disclosure cases stay as they are**: they are driven by a targets denial, which is a
      real answer of a real route (`:360-411`).
      Re-pointing them at the catalog would contradict this design's own invariant, since the catalog is
      filtered and never realm-refused, and the realm check refuses `kindServices` alone
      (`internal/httpapi/realm.go:18-20`).
      What the removed case also proved — that an answer the page has moved past records no error,
      opens no disclosure, and asks for no identity — is replaced by delayed **targets** and
      **Collections** answers across a selection change and a return to the original pair,
      whose generations this transition still raises.
- [ ] Two limits of the browser harness belong in these instructions.
      `holdRequest` holds a request before it reaches the gateway (`test/e2e/browser_test.go:466-473`),
      so no case may assume an already-computed successful answer can be replayed;
      and `answerRequest` writes an error envelope (`:511-529`),
      so a case needing an old success needs an injection extension named as part of its own task.
- [ ] The two console scenarios and both authentication scenarios pass unchanged otherwise.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
mise run test:e2e
git log --oneline -1 && git status --short
```

An end-to-end failure is classified by the step that failed:
`TestMain` resolves the lane, creates the cluster, builds the images, connects, provisions,
and deploys the gateway before any scenario runs (`test/e2e/harness_test.go:171-270`).

- [ ] Commit: `refactor(ui): fill both menus from one catalog`

---

## 5. The search across namespaces

- [ ] `internal/ui/static/catalogmodel.js`: `searchCatalog(catalog, query, limit)`,
      matching `namespace + "/" + name` under the rule `filterOptions` uses,
      returning the first `limit` matches in the catalog's order and the total number matched.
      An empty query matches nothing, so results appear only once a person has typed.
- [ ] `internal/ui/static/app.js`: state gains `search`;
      the results render as one `<button>` a match, labelled `namespace/name`, each calling `selectPair`
      with both values;
      a line says how many matched when more matched than were drawn,
      and says the catalog is still loading while it is.
- [ ] `internal/ui/scan_test.go`: the import and export scans gain `searchCatalog`.
- [ ] `internal/ui/static/app.css`: the search field and the result rows.

**Tests**

- [ ] `internal/ui/catalogmodel_test.go`: `searchCatalog` table cases —
      an empty query matches nothing; a query matches the name alone; a query matches the namespace alone;
      a query holding the separator matches both parts;
      the cap bounds the matches while the total counts every match;
      the order is the catalog's; the input is not mutated.
- [ ] `test/e2e/scenarios_console_test.go`: a search for the test app's name draws a result naming its namespace;
      clicking it selects both menus, enables the Service menu, builds the profile URL,
      and fetches targets exactly once from a boundary.
- [ ] `test/e2e/scenarios_console_test.go`: the result click and the two-menu sequence settle in the same state —
      the query string, both menus, the Pod and version menus, the Collections state, the write controls,
      and the errors, which are the fields the transition touches.
- [ ] `test/e2e/scenarios_console_test.go`: a search that matches nothing draws no row and says so;
      a search run before the catalog answers says it is loading and draws its rows when the answer lands.
- [ ] `test/e2e/scenarios_console_test.go`: a Service deleted between the catalog answer and the click —
      the targets fetch answers `404 service_not_found` (`internal/httpapi/server.go:656-663`),
      `loadTargets` clears the summary and `afterServiceError` refetches the catalog
      (`internal/ui/static/app.js:620-623`, `:901-907`),
      and the redrawn menus no longer offer it.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
mise run test:e2e
git log --oneline -1 && git status --short
```

- [ ] Commit: `feat(ui): search Services across namespaces`

---

## 6. The guide, and the close

- [ ] `docs/console.md`: the search field, what it matches, the result rows,
      the Refresh that refetches the catalog, and the two filter fields.
- [ ] `CHANGELOG.md`: one entry for the route and one for the console.
- [ ] `Status:` becomes `Done` and line 4 gains `**Outcome:**` naming the pull request that carried the tasks.
- [ ] The next change that touches this file deletes it, per
      [`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md).

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
git log --oneline -1 && git status --short
```

- [ ] Commit: `docs: describe the catalog search`

---

## Risks and What This Plan Does Not Cover

- **The load carries the whole admitted catalog.**
  About 280 KiB at five thousand Services by the estimate above, uncompressed, once per load.
  The handler imposes no ceiling; compression is a separate decision about every response.
  This is the price of one source, and it is the trade this design makes on purpose.
- **The Service menu is less fresh than it is today, and the namespace menu is fresher.**
  Today a namespace change refetches that namespace's Services and nothing refetches namespaces
  (`internal/ui/static/app.js:562-573`, `:939-946`).
  After this plan neither refetches on its own, and one Refresh refetches both.
  A Service created after the load appears when someone presses it.
- **`/v1/catalog` walks the whole cache, as `/v1/namespaces` already does.**
  This plan adds a second route with that cost and memoizes neither.
- **Two routes lose their only browser caller.**
  `/v1/namespaces` and `/v1/namespaces/{ns}/services` keep every other client
  and keep their tests; only the console stops calling them.
- **The command-line client cannot reach the new route.**
  `profgate services` still requires its namespace.
- **No paging, no server-side search.**
  `ui.md` step 7 keeps every listing route free of parameters,
  and *The realm filter* states that realm matching has no prefix, glob, or pattern form;
  a search grammar answering next to it would invite exactly that inference.
- **The arrangement of the new controls is not proven.**
  *What is not proven* already records that panel placement and control wrapping are unproven;
  the search panel and the two filter fields join that list.
