# The Console Is Safe to Click and Honest About What It Shows

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> On this machine `mise run lint` runs a golangci-lint 2.1.6 that shadows the pinned 2.12.2,
> so every validation block below runs the linter as `mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`
> and never as `mise run lint`.

**Goal:** make every control on the console do what its label says, and nothing a reflex can do instead.
A mouse double-click on **Start collection** arms the control with its first click and confirms it with its second,
so one gesture creates a Collection, and **Cancel** has the same shape.
**Download** is a navigation, so the `503 no_targets` a Service with no eligible Pod earns shows the page nothing,
and neither does any other error the profile endpoint answers.
Nothing on the page refreshes:
a Collection started from it never leaves `pending` without a reload,
and the list shows one page of at most 100 records with `nextCursor` dropped on the floor.
The Collections table sits in one grid column beside empty page, so its right-hand columns are off-screen at 1600 px;
**Keep** and **Confirm start** render in the same primary blue because the vendored Pico build has no `.secondary` rule;
`loginURL` sends a return path of any length where the design bounds it at 1024 bytes;
the hints table lacks four codes the design lists, words two by a rule the gateway no longer applies,
and promises an identity panel a listing `403` never refreshes;
the page has no heading and no pointer to the CLI verb that does the same job;
and `docs/console.md` states four things the page does not do and two it cannot promise.
After this plan a double-click creates nothing,
a download that cannot happen is disabled with its reason beside it and one that fails says why,
each list has a **Refresh** control and the Collections table says when older records exist and which verb lists them,
`profgate collections` lists them all,
and the console guide describes the page as it is.
No endpoint, configuration key, Kubernetes call, or NATS permission is added.

**Architecture:** `internal/ui/static/collectionmodel.js` gains the confirmation window the two write controls hold a second press to,
and the line under the Collections table that names `profgate collections`;
`internal/ui/static/targetmodel.js` gains the line beside a disabled **Download**;
`internal/ui/static/app.js` wires all three,
turns **Download** into a `fetch` followed by a save through an object URL,
gives the targets list and the Collections table one **Refresh** control each over two independent request generations,
refetches `/v1/whoami` when a listing answers `403 realm_denied`,
gains four hints, corrects two, and gains the pointer to the CLI verb,
and marks the Collections panel for the stylesheet;
`internal/ui/static/app.css` spans that panel across the grid and gives `.secondary` a rule from Pico's own variables;
`internal/ui/static/urls.js` measures the return path against the 1024-byte bound;
`internal/ui/static/index.html` gains the heading;
`cmd/profgate` walks every page of the listing under `collections` and `internal/client` reads `nextCursor`;
`test/e2e` proves each repaired control in the browser, in the task that repairs it;
`docs/console.md` describes the page as it is.
No route, chart value, configuration key, or permission moves.

**Spec:** the four settled behaviors are accepted text in [`ui.md`](../specs/ui.md):
*Core decisions* for the refresh adding no endpoint (`docs/specs/ui.md:56`);
*Non-goals* for no polling (`:95-105`);
*Collections* for the first page and the token never sent back (`:401-405`);
*Targets, with reasons* for the one shape that disables **Download** (`:436-439`);
*Flow* for the download as a fetch and a save, its filename, its disabled state, and its errors (`:488-489`, `:516-557`);
*Controls* for the duration input disabling **Download** (`:588`),
the four functions of `targetmodel.js` (`:658-669`),
the disabled download and its line (`:671-686`),
the paging line (`:705-717`),
and **Refresh** (`:719-746`);
*Starting and cancelling a Collection* for the two presses and what a refresh leaves alone (`:781-788`);
*Rendering response values* for the one URL not built in `urls.js` (`:972-974`);
*Errors* for the hints table and the download read by its rule (`:1043-1077`);
*Response headers and CSP* for the anchor and the absent `blob:` source (`:1236-1242`);
*Failure scenarios* for the five rows (`:1340-1344`);
*Unit* for the fourth `targetmodel.js` case and the line under the table (`:1473`, `:1497-1502`, `:1539-1543`);
*What is not proven* for the download and the refresh wiring (`:1658-1660`);
*End to end* for the scenario steps (`:1735-1761`);
*Required by this revision and not yet made* for the one edit owed (`:1965-1981`);
and the amendment block that lists them (`:2013`).
The verb's walk is accepted text in [`cli.md`](../specs/cli.md):
*The verbs* (`docs/specs/cli.md:145`), *Collections* (`:953-966`), *Output and exit codes* (`:1006-1007`),
*Failure scenarios* (`:1157`), *Testing* (`:1336-1345`), and its two amendment rows (`:1611-1612`).
The six repairs no spec settles are the roadmap's own bullets,
under *Make the console safe to click and honest about what it shows* (`docs/plans/roadmap.md:280-317`).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the page, not as the defects that revealed them.

- **A write control sends a request on the second deliberate press and never on one gesture.**
  `onStart` arms on the first press and submits on any later one (`internal/ui/static/app.js:623-634`),
  `onCancel` does the same with a row identifier (`:672-682`),
  and the armed branch renders **Confirm start** where **Start collection** stood (`:1204-1213`),
  so the second click of a double-click lands on the confirm control a few tens of milliseconds after the first armed it.
- **A download that cannot succeed is disabled with its reason beside it, and one that fails says why.**
  The control is an `<a href download>` rendered whenever a URL can be built (`:1146`),
  built by `currentProfileURL` from the selection and the duration alone (`:902-913`),
  so a targets response listing no Pod leaves the link live and the `503 no_targets` it earns is a navigation the browser shows nothing for.
- **Each list can be fetched again from the page, and a refresh of one list never discards the other's answer.**
  `loadTargets` stamps `++this.seq` (`:474`) and `loadCollections` reads the same counter (`:511`),
  so a targets fetch started after a Collections fetch drops the Collections answer at `:513-515`,
  and no control repeats either fetch (`:1071-1157`, `:1218-1281`);
  a control that repeats one must know which answer is the latest, because a stale answer must neither be applied nor end the wait.
- **A page that is one page of many says so, and names a verb that lists the rest.**
  `loadCollections` keeps `body.collections` and nothing else (`:521`),
  and `collectionsVerb` sends one `GET` with no cursor (`cmd/profgate/collect.go:270-299`).
- **What the page shows is laid out to be seen.**
  `.panels` is an auto-fit grid of 20 rem columns (`internal/ui/static/app.css:9-14`)
  and the Collections article is one cell of it (`internal/ui/static/app.js:931-938`, `:1221`).
- **A secondary control looks secondary.**
  `class="secondary"` is set on both **Keep** buttons (`:1207`, `:1296`)
  and the vendored `pico.classless.min.css` holds no `.secondary` selector,
  while it does define `--pico-secondary-background`, `--pico-secondary-border`, and `--pico-secondary-inverse`.
- **A return path the browser flow would refuse is never sent.**
  `loginURL` builds the return path from `ns` and `svc` whatever their length (`internal/ui/static/urls.js:103-106`).
- **Every hint is true, and every code a user can act on has one.**
  `hints` (`internal/ui/static/app.js:56-74`) holds sixteen keys against the eighteen codes of the design's table (`docs/specs/ui.md:1046-1063`):
  `version_conflict` and `version_missing` are absent,
  and so are `too_many_auth` and `auth_unavailable`, which the gateway answers on every `/v1` route (`internal/httpapi/codes.go:65`, `:87`)
  and the design lists as answers of every listing (`docs/specs/ui.md:200`, `:1281-1282`) without a row in the table;
  its `no_targets` and `port_not_allowed` texts (`:60-61`) describe a rule older than the table's;
  and its `realm_denied` hint names the identity panel while `request` records a `403` and refetches nothing (`:386-421`).
- **The guide describes the page.**
  `docs/console.md:90-91` calls **Download** an ordinary link;
  `:103-105` says **Copy URL** is absent on an HTTP page while the recipe at `:24-28` is `localhost`, a secure context;
  `:122-123` says **Keep** puts the control back as it was;
  `:139-142` describes the move off hashed asset paths without naming the releases;
  `:63` and `:82` promise a return to the same selection, which the return-path bound makes untrue for a long one;
  and `:150` promises the bytes `curl` would receive, which a `Content-Encoding` an intermediary adds makes untrue.

---

## Decisions

Sixteen choices settle how the spec's text is carried, and the facts of the code that shape each test.

**Eleven tasks, one behavior each, and each task proves its own.**
The roadmap's ten bullets carry more than ten behaviors:
the refresh bullet is two — the control and the paging line — with different model modules.
The download bullet is one task although it has two seams,
because the disabled control is a `<button>` and the anchor it replaces downloads today:
a task that replaced the element and wired its handler later would leave a live control that does nothing.
The two stylesheet repairs share one file, one check, and no test, so they are one task.
`profgate collections` walks every page before the console's line names it as the verb that lists them all,
so the page never names a promise the binary does not yet keep.
Every browser step lives in the task whose change it proves,
because a step written after every repair proves nothing about the defect it was written for,
and tasks 2, 3, and 7 each run the scenario red there.
Two steps are not run red in the task that writes them:
task 1's double-click, whose red is its model test and whose step is shown load-bearing in *Validation*,
and task 8's tightened `no_targets` wording, which the task's own hint change satisfies
and which runs with the suite in *Validation* rather than earning a second run of its own.
The suite as a whole runs once more in *Validation*.
The guide's repair is last, after every behavior it describes, and the closing task follows it.

**The second press is refused inside a window measured from the arm, in the model.**
The roadmap offers two mechanisms (`docs/plans/roadmap.md:284-286`).
Placing the confirm control away from where the first press landed depends on widths:
**Keep** is narrower than **Start collection**,
so a second click at the right-hand end of where **Start collection** stood lands on **Confirm start** whichever order the two are drawn in.
A window does not depend on layout.
`collectionmodel.js` gains `confirmDelayMs`, 500 milliseconds, the interval every platform's double-click fits inside,
and `confirmAccepted(armedAt, now)`, true when `now - armedAt` is at least that.
`startNext` carries `armedAt` in the state:
the `arm` event gains `now`, and an `armed` control answers a `submit` whose `now` is inside the window with the unchanged state,
which `startEvent` reports as not moved and `onStart` sends nothing for (`internal/ui/static/app.js:630-633`).
A `retained` control answers `submit` as it does today:
its arm is minutes old, and the press that follows a lost answer is the retry the key exists for.
`onCancel` keeps `cancelArmedAt` beside `cancelArmed`
and returns before `clearTimeout(this.cancelTimer)` when `confirmAccepted` refuses the press,
so the ten-second disarm keeps running and the row stays armed.
A person who presses twice faster than half a second sees **Confirm start** still standing and presses it again.
The function lives in `collectionmodel.js` because it is a decision about two presses,
the goja test can drive it with any two clock values,
and the scan already holds `app.js` to calling every function it imports from that module (`internal/ui/scan_test.go:177-202`).
Both `collectionModelFunctions` (`internal/ui/collectionmodel_test.go:17-28`) and the scan's list gain the name,
and the module's export statement grows by one, which `TestCollectionModelShape` asserts verbatim (`:69-72`).

**The browser proves the double-click with two clicks fifty milliseconds apart, dispatched from one evaluation.**
`chromedp.DoubleClick` sends one mouse press with `clickCount` 2
(`$(go env GOMODCACHE)/github.com/chromedp/chromedp@v0.16.0/query.go:1066-1074`, `input.go:147-151`),
which the page receives as one `click` with `detail` 2 and one `dblclick` it does not listen for;
that is not what a physical double-click delivers, which is two `click` events inside the platform interval.
Two `chromedp.Click` actions in one `Run` are two real presses,
but each waits for its node to be visible through a poll, and nothing bounds the gap between them.
The step therefore runs one evaluation in the page:
click the button labelled **Start collection**, wait fifty milliseconds on a timer,
click the button labelled **Confirm start** the first press rendered, and return the elapsed milliseconds;
the test fails when the elapsed time is 400 or more, so the assertion is inside the window by construction.
It then asserts **Confirm start** and **Keep** are standing and no `POST` has been sent,
presses **Confirm start** once more after waiting past the window,
and `assertStartRequest` (`test/e2e/scenarios_console_test.go:494-507`) asserts exactly one `POST` as it does today.
The existing arm-then-confirm pairs (`:208-209`, `:221-222`) gain a wait past the window between the two presses,
because two `Run` calls can land inside half a second on a fast machine and the confirm would then be refused.
`HTMLElement.click()` dispatches a `click` the page's listeners receive,
and Preact has rendered the armed branch by the time the timer fires, so the second click finds **Confirm start**.

**Download is a fetch in `app.js`, and `urls.js` gains nothing.**
*Flow* (`docs/specs/ui.md:516-557`) fixes the shape:
`fetch`, a `Blob`, `URL.createObjectURL`, an `<a download>` clicked and its URL revoked, the `Content-Disposition` filename.
`fetchJSON` (`internal/ui/static/app.js:89-138`) gains a third parameter, `asBlob`:
under it a `200` is read with `res.blob()` into `out.blob` and `out.body` stays `null`,
and any other status is read exactly as today, so the envelope rule is one code path.
The test is `res.status === 200` and not `res.ok`,
because the page's success boundary is `200` (`:395`), *Flow* saves a `200` and nothing else (`docs/specs/ui.md:521`),
and a `204` read as a `Blob` would save an empty file.
A rejected `fetch` and a `blob()` that throws both reach the existing `catch` (`:133-137`) and read "request failed",
which is the one failure *Flow* asks for (`:553-555`).
The tail of `request` — the `401` rule and the error record (`:399-420`) — moves into `settle(key, res, retry)`,
which `request` calls and the new `onDownload` calls too,
so a `401` on a download navigates once under `oidc` and asks to sign in under `basic` exactly as every listing does.
`settle` returns the error it recorded, or `null` when the `401` rule took the answer,
so `onDownload` refetches the Service list on a `service_not_found` from the value returned,
the refetch the two loaders make through `afterServiceError` (`:721-726`, called at `:489` and `:518`),
whose hint would otherwise promise a refresh the download never made;
it reads the returned value and not `this.state.errors`, because a `setState` is applied later than the line after it.
The save is an anchor the page creates with `document.createElement("a")`,
given the object URL as `href` and the filename as `download`, clicked, and removed,
and the URL is revoked when the click has returned;
the anchor is never in the template, so the template's **Download** becomes a `<button type="button">`,
which is what a control that can be disabled has to be.
The filename is the `filename` of `Content-Disposition`, quoted or bare, and `profile` when the header carries none,
read by a small helper beside `isEnvelope`.
`urls.js` gains nothing:
*Rendering response values* excepts the object URL from that module by name (`:972-974`),
and a filename is not a URL.
The page reads `status`, `Content-Type`, and `Content-Disposition` and nothing else (`:556`),
so `X-Pprof-Target-*` stays unread.
The state gains `downloading`, true from the press until the save has begun or the fetch has settled,
and the control is disabled and reads "Downloading" while it is set.
No unit test runs `app.js`, so the fetch, the blob, the anchor, and the filename are proven by the browser scenario
and stated under *What is not proven* as the spec already does (`:1658-1659`).

**The line beside a disabled Download is the fourth function of `targetmodel.js`.**
*Controls* names it (`:671-686`) and *Unit* lists its four cases (`:1497-1502`).
`downloadNote(summary)` takes what `targetSummary` returns and yields a string:
empty for a summary with targets,
each row's count and wording joined with `; ` in the rows' order for the `reasons` kind,
the selector sentence for `noSelector`, and "no target listed" for `plain`.
The control is disabled when `targetSummary` is non-null and `summary.empty` is non-null,
enabled while `targetSummary` is `null`, which is both before any answer and after a failed one (`:682-686`).
`targetModelFunctions` (`internal/ui/targetmodel_test.go:16`), the scan's list (`internal/ui/scan_test.go:157`),
and the module's export statement each gain the name.

**Two request generations and two in-flight flags, each flag owned by the generation that set it.**
Today one `seq` stamps both fetches (`internal/ui/static/app.js:254-256`, `:474`, `:511`):
`loadTargets` raises it, `loadCollections` reads it,
so a targets refetch started while a Collections fetch is in flight drops the Collections answer,
which is the coupling *Controls* forbids (`:731-732`).
`seq` splits into `targetsSeq` and `collectionsSeq`;
`onNamespace` and `onService` raise both (`:735`, `:764`), and each loader raises and reads only its own.
The state gains `targetsLoading` and `collectionsLoading`,
set when a loader starts and cleared only by the answer of the generation that is still the latest;
an answer a newer request has superseded touches nothing, not even the flag,
because clearing it would enable **Refresh** while the newer request is still in flight.
A selection change resets both flags with the lists it empties.
`request` gains an optional `stale` predicate, and a stale answer is dropped before it records an error,
schedules a `not_ready` retry, or navigates on a `401`,
because the error recorded under a key is the latest request's and a stale one arriving late would overwrite it.
A `not_ready` retry is the loader itself, so it raises the generation when it fires;
a **Refresh** pressed during the two-second wait and the retry that follows are two generations for one selection,
and the later one wins by the same rule every stale answer already follows, with no second mechanism to decide between them.
Each **Refresh** button is `disabled` while its flag is set.
`onRefreshTargets` calls `loadTargets` and nothing else;
`onRefreshCollections` calls `loadCollections` and nothing else;
neither touches `startTimer`, `cancelTimer`, the `start` state, or `cancelArmed`,
which is how a refresh disturbs no write control (`:735-739`).
After a targets answer the page keeps `pod` and `version` when the new summary still lists them and returns each to `""` otherwise,
two lines in `loadTargets` after `targetSummary` is applied;
`refetchTargets` (`:805-809`), the port change, keeps clearing both before it fetches, as *Controls* says a port change does.
The wiring runs in no unit test and is stated under *What is not proven* (`:1660`);
the browser scenario proves the one `GET` a refresh sends and the armed row it leaves standing.

**The paging line is a function of `collectionmodel.js`, and the page keeps the token only to know it exists.**
`olderCollectionsNote(body, ns, svc)` yields the line when `body.nextCursor` is a non-empty string and `""` otherwise,
never placing the token in it (`:1539-1543`).
The text is fixed:
"Older Collections exist beyond this page; `profgate collections <ns>/<svc>` lists them all",
with the two values interpolated as text.
`loadCollections` stores `collectionsNote` beside `collections` and clears it in the failure update that empties `collections`,
so the rows and the line under them are one result and a failed refresh never leaves the line over an empty table;
`renderCollections` renders it under the table when non-empty.
The module spells no path and sends nothing, so the note is a string and the page never reads the token again.

**The Collections panel spans the grid; `.secondary` is one rule in `app.css` from Pico's variables.**
`grid-column: 1 / -1` on `.panels article.collections` puts the table on a row of its own below the three panels,
so nine columns get the page's width rather than one cell's;
`renderCollections` gives its article `class="collections"`, one attribute.
`.secondary` is absent from the classless build (`grep -c '\.secondary' internal/ui/static/vendor/pico/pico.classless.min.css` is `0`)
and present in Pico's full build, which is a different file with a different hash, size, and manifest line,
and *Vendoring rule* accepts a vendored file only as the upstream's published build byte for byte (`docs/specs/ui.md:1193-1211`).
Swapping builds to gain one rule is the larger change;
`button.secondary` in `app.css` takes its three colors from variables the classless build defines in both themes:
`--pico-secondary-background`, `--pico-secondary-border`, and `--pico-secondary-inverse`;
it stands beside `button.armed` (`internal/ui/static/app.css:78-82`).
No test measures layout or color; the browser check of rule 500 is the check, and its steps are named in the task.

**`loginURL` measures the return path it builds, and the browser proves it with a second session.**
*Flow* (`:506-508`) measures the encoded result against the 1024-byte bound and sends `/ui/?returned=1` alone when a selection would cross it.
`URLSearchParams` percent-encodes every byte outside the unreserved set,
so `page.pathname + page.search` is ASCII and its `length` is its byte count.
`loginURL` builds the login with `return` set to that string when its length is at most 1024
and to the path and search of `build("/ui/", [], { returned: "1" })` otherwise, one comparison and one branch.
`urls.js` cannot run under goja:
it exports each function in place rather than through one trailing statement, which `cutExport` requires (`internal/ui/portmodel_test.go:30-42`),
and it uses `URL` and `URLSearchParams`, which the interpreter lacks.
The proof is a browser step in `console-oidc`:
a second `newSession`, with no cookie, navigates to `/ui/` with an `ns` of 1100 characters,
the page answers its `401` by navigating to `/auth/login`,
and the test reads the `return` parameter of that request from the network events, as `assertNavigatedToLogin` does (`test/e2e/scenarios_console_test.go:452-475`),
and asserts it is exactly `/ui/?returned=1`.
The login is not completed; the request the page sent is the whole proof.

**Four hints are added, two are reworded, and a listing `403` refetches `/v1/whoami`.**
The spec's hints table (`docs/specs/ui.md:1046-1063`) has sixteen rows and eighteen codes;
`hints` (`internal/ui/static/app.js:56-74`) has sixteen keys and lacks `version_conflict` and `version_missing`, the table's last row.
Both tables lack `too_many_auth` and `auth_unavailable`,
although *Request algorithm* names both as answers of every listing route (`:200`) and *Errors* lists them (`:1281-1282`).
The task adds the two version codes to `hints` with the table's wording,
adds the two authentication rows to the spec's table and to `hints`, in the same commit, with an amendment row:
`too_many_auth`, "the gateway is checking too many passwords at once; retry in a moment",
and `auth_unavailable`, "the gateway cannot decide who you are right now; retry";
and replaces the `no_targets` and `port_not_allowed` texts (`:60-61`) with the table's,
because the code's describe a rule — a Ready Pod declaring the port, an allowlist — that the gateway's eligibility and `allowedSelections` no longer state,
and the `no_targets` text is the hint the browser's scaled-down step reads.
The `realm_denied` hint promises a panel the page never refreshes on a listing `403`.
The code repair is preferred to a reworded promise:
`settle` calls `reloadWhoami` when the recorded error's code is `realm_denied` and the key is not `whoami`,
the refetch a start's `403` already causes through `startOutcome` (`internal/ui/static/collectionmodel.js:205-207`).
The spec's row reads "the whoami panel" and the panel's header reads "Identity" (`internal/ui/static/app.js:962`);
the row is aligned to the page's word in the same edit.
A source scan compares the key set of the `hints` object against the twenty codes exactly:
a code with no key and a key with no code are each a failure,
so a hint the page gains without the list turns the scan red as much as a listed code the page lacks.
The list is held in the test, not read from the spec's Markdown,
the shape `TestTargetModelNamesEveryReason` gives the reasons (`internal/ui/targetmodel_test.go:76-86`)
and the three import lists give the scans:
every scan in `internal/ui` names what it holds the page to, a failure then names a code and never a parse,
and the list and the table agree by review, as the reasons vocabulary does.
`deploy_test.go` reads a spec table for the NATS permissions (`deploy/deploy_test.go:668-700`),
a set the permission invariant makes worth a parser; a hint's wording is not.
The refetch itself runs in no unit test and is stated under *What is not proven*.

**The heading is in the shell and the pointer is in the Profile panel.**
Preact's `render` diffs against the container's existing children and removes what it did not render,
so a heading inside `<main id="app">` would not survive the first render;
`index.html` gains `<header><h1>Profgate</h1></header>` before `<main>`,
which Pico's classless build styles as a container, and `TestShellInlineForms` (`internal/ui/ui_test.go:611-615`) gains `<h1>` in the strings it wants.
The pointer is one line under the copy note (`internal/ui/static/app.js:1150`):
"From a terminal: `profgate profile <ns>/<svc> <profile>`", with the selection filled in as text
and shown only while a URL is built, because a verb without its arguments points nowhere.
The scan asserts `app.js` holds the literal `profgate profile`, the way `TestScanNoBlockingDialogs` asserts an absence (`internal/ui/scan_test.go:94-98`).

**`collections` walks in its own `run`, and `env.read` keeps its one `GET`.**
`env.read` is "exactly one GET" by its comment and its shape (`cmd/profgate/read.go:15-50`),
and every other read verb relies on that;
`collectionsVerb` therefore builds the client with `env.gateway` as `read` does (`:28`),
and loops: `gw.JSON(ctx, req)` with `req.Query` carrying `cursor` from the previous page's `NextCursor`,
until a page carries none.
Every page's body is kept in a slice as it arrives and nothing is written until the walk completes,
so a page that fails mid-walk reaches `fail(env, s.Output, err)` (`cmd/profgate/exit.go:53-60`) with stdout still empty:
under `table` the envelope goes to stderr and no row is printed;
under `json` `fail` writes the failing page's envelope bytes, `APIError.Body`, and nothing else,
which is the copied-not-rebuilt envelope *Collections* asks for (`docs/specs/cli.md:964-967`).
The exit code is `exitCode`'s (`cmd/profgate/exit.go:23-34`): `3` for a `401`, `1` for every other refusal,
which is what *Collections* (`docs/specs/cli.md:961-963`), the *Failure scenarios* row (`:1157`), and the *Testing* bullet (`:1341-1343`) say:
a page that fails mid-walk exits by the rule of *Output and exit codes* (`:1054`),
because a login is still the thing that might fix a `401` wherever in the walk it arrives.
The test has a `401` second-page case beside the `503` one, so the two exit codes are both asserted.
On success the table holds every page's rows in arrival order,
and `json` writes each page's body in order, unchanged.
`client.CollectionsResponse` (`internal/client/wire.go:86-89`) gains `NextCursor string` under `json:"nextCursor"`,
which the gateway omits when the listing has ended (`internal/httpapi/pgo_collections.go:389-396`).
The stale comment at `cmd/profgate/collect.go:270-271` says the listing accepts no cursor and goes.
The tests follow the shape of `retryTransport` (`cmd/profgate/collect_test.go:195-213`):
a transport that answers by the `cursor` query value, recording every request.

**The browser observes what a press sent and what its answer did, and each step lives with its repair.**
`awaitRow` refetches by choosing the Service again (`test/e2e/scenarios_console_test.go:610-634`, `:664-682`),
which *End to end* replaces with a press of **Refresh** on the Collections table (`docs/specs/ui.md:1744-1745`);
`refetchCollections` becomes that press.
A request is recorded when the browser is about to send it (`test/e2e/browser_test.go:283`),
and `awaitRequest` (`test/e2e/scenarios_console_test.go:600-608`) answers whether a method and URL were ever sent,
which a second `GET` of a list the page fetched at load cannot be told apart by;
`session` gains `requestCount()` and `awaitRequestSince(since, match)`,
so a step records the count before a press, waits for the request the press sent by a predicate over the requests after that count,
and then waits for the answer to be applied — the **Refresh** it pressed enabled again — before it reads what the answer left standing,
because a request sent says nothing about a response applied.
The download is proven a fetch by what the browser records, not by the file alone:
`sentRequest` gains the request's `resourceType`, `Fetch` for the new control where a navigation is `Document`,
and `observe` records `EventDownloadWillBegin` — its `GUID`, `URL`, and `SuggestedFilename`
(`$(go env GOMODCACHE)/github.com/chromedp/cdproto@v0.0.0-20260714215040-dc233986426f/browser/events.go:12-17`) —
beside the completion it already reads from `EventDownloadProgress` (`test/e2e/browser_test.go:306-312`),
so the step asserts a `Fetch` to the profile route followed by a download whose URL starts with `blob:`.
The suggested name is asserted as the browser's suggestion and the file on disk is read by whatever name it has,
because the protocol says the two may differ;
the bytes are asserted gzip-framed as today, which is all a first two bytes can say.
Whether Chromium fires the two download events for a save from an object URL is not verified by reading:
`Browser.setDownloadBehavior` with `eventsEnabled` is enabled for the session (`:256-257`)
and the download manager it reports through is the one an `<a download>` on a `blob:` URL goes through,
so the plan expects both events; the first run of the suite is where that is settled, and *Risks* names the fallback and what it does not prove.
The spec says the saved name is `profile` (`docs/specs/ui.md:1737`);
Go's pprof handler writes `profile` for the CPU profile and the profile's own name for every other
(`$(go env GOROOT)/src/net/http/pprof/pprof.go:157`, `:271`),
and the scenario downloads `heap` (`test/e2e/scenarios_console_test.go:188`),
so the step asserts `heap` and the spec line is corrected with an amendment row.
The second Service, `nobody`, whose selector names a label no Pod carries,
is created through `h.Client.CoreV1().Services(ns).Create` right after the test app is deployed,
the way `applyConfigMap` creates its object (`test/e2e/harness_test.go:1569-1579`),
because `h.kubectl` takes arguments and no stdin (`:1103-1105`, `:1109-1121`) and a file is a second fixture for one selector;
it is created before the working load, because `loadServices` runs on the namespace list arriving, on a namespace change,
and on a `service_not_found` (`internal/ui/static/app.js:442-468`, `:721-726`, `:733-760`),
so a Service created after the load would never appear in the list the page keeps.
The scale to zero waits first for the page's Pod menu to list a Pod,
because `currentProfileURL` (`:902-913`) needs no targets answer and the URL field cannot say the targets fetch has landed;
then it uses the merge patch `deployTestAppScaled` uses (`test/e2e/scenarios_pgo_test.go:356-363`)
and awaits an empty targets answer through the scenario's own authenticated client against the gateway it deployed,
because `waitTargets` polls `h.Gateways` (`test/e2e/scenarios_test.go:324-349`, `:331`), which are not that gateway;
the page's list is left as it is.
The deleted Pods stay `Terminating` for the preStop sleep (`test/e2e/testapp/deployment.yaml:38-41`),
so the answer excludes them as `pod_terminating` first and then, once they are gone, reports `selectorMatched` of `0`,
and the assertion on the line beside the disabled control accepts either wording rather than one.

**Spec edits ride with the code they describe.**
Four tasks add a case to *Unit* or a step to *End to end* of `ui.md`, one adds two rows to *Errors*,
one corrects the download's name, and the guide's task clears the *Required by this revision and not yet made* table;
each carries one amendment row, and the tree after every task has a spec that matches its tests,
the shape `docs(spec): match the unit cases to the tests` gave the last plan.

**Changelog entries name the behavior, none marked breaking.**
The two **Refresh** controls, the paging line, the walk of `collections`, and the heading go under `### Added`;
**Download** as a fetch goes under `### Changed`, because a saved file's name is unchanged,
its bytes are unchanged for a response with no `Content-Encoding`, which is every response the pprof handler writes,
and decoded for one an intermediary encoded — a qualification the entry and the guide both carry, as *Flow* does (`docs/specs/ui.md:526-530`) —
while a 401 mid-download now signs in rather than showing a browser's own page;
the double-click, the disabled control and its error, the layout, the color, the return-path bound, and the hints go under `### Fixed`.

**The plan closes naming the pull request.**
The merge rebases the branch and rewrites every hash,
so `Outcome:` and the roadmap's `Shipped:` line (`docs/plans/roadmap.md:315`) both name the pull request,
and the file is deleted by the commit after the merge, as the last plan was.

---

## Global Constraints

- **No new endpoint, configuration key, chart value, Kubernetes call, or NATS permission.**
  Every request the page sends after this plan is one it sent before; the CLI sends the same `GET` with a query it already accepts.
- **Every task that changes behavior shows a red test before its change,
  or says why it cannot and what verifies it instead.**
  Tasks 1, 2, 4, 5, 8, and 9 each name a unit test, the exact command, and the wrong implementation it exists to catch;
  tasks 2, 3, and 7 also write their browser steps and run the console scenario once, red, against the code before the change,
  with `go test -tags e2e -race -count=1 -timeout 40m -run 'TestScenarios/console-oidc$' ./test/e2e/` on a machine with a Chromium and the kind lane,
  which is expensive and is why it runs once per task and not in the validation block;
  task 1 writes its browser step beside a unit test that is already red and shows the step load-bearing in *Validation*;
  tasks 6 and 10 have no red test — a stylesheet rule and prose — and each says what verifies it instead;
  task 11 changes no behavior.
  The whole suite runs once more in *Validation* with every task in place.
- **A goja test where the decision is pure, a source scan where the page must call it, the browser where the DOM decides.**
  The three model modules run under the interpreter (`internal/ui/portmodel_test.go:44-60`);
  `app.js` runs in the two browser scenarios and nowhere else (`docs/specs/ui.md:1606-1608`);
  the scans hold `app.js` to importing and calling each model function (`internal/ui/scan_test.go:126-202`).
  A behavior of `app.js` no scan reaches is either a step of `console-oidc` or a line under *What is not proven*, named per task.
- **Several cases of one shape are named table cases** with `t.Run(tc.name, ...)`
  ([`300-testing.md`](../../.agents/rules/300-testing.md)).
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
internal/ui/static/collectionmodel.js        # confirmAccepted and armedAt in startNext; olderCollectionsNote
internal/ui/static/targetmodel.js            # downloadNote, the fourth function
internal/ui/static/app.js                    # the window on both controls; Download as a fetch and a save; the disabled control and its line; two Refresh controls over two generations; the paging line; realm_denied refetches whoami; four hints and two reworded; the CLI pointer; class="collections"
internal/ui/static/app.css                   # the Collections panel spans the grid; button.secondary
internal/ui/static/urls.js                   # loginURL measures the return path
internal/ui/static/index.html                # the heading
internal/ui/collectionmodel_test.go          # the window cases; the line under the table
internal/ui/targetmodel_test.go              # the line beside a disabled Download
internal/ui/scan_test.go                     # the import lists; the hint keys against the table's codes; the CLI pointer literal
internal/ui/ui_test.go                       # the shell holds an h1
cmd/profgate/collect.go                      # collections walks every page
cmd/profgate/collect_test.go                 # the six paging cases
internal/client/wire.go                      # CollectionsResponse.NextCursor
test/e2e/browser_test.go                     # the request's resource type; the download-begin events; requestCount and awaitRequestSince
test/e2e/scenarios_console_test.go           # the double-click, the fetched download and its name, Refresh, the second Service, the scale to zero, the long selection
docs/specs/ui.md                             # Unit and End to end cases; two hint rows; the download's name; the owed edit cleared; amendment rows
docs/console.md                              # the guide describes the page
docs/plans/roadmap.md                        # the item's Shipped line, in the closing task
CHANGELOG.md                                 # one entry per behavior
docs/plans/console-safety.md                 # this file
```

---

## 1. A second press inside half a second of the arm sends nothing

Closes the roadmap bullet beginning *A mouse double-click on **Start collection*** (`docs/plans/roadmap.md:282-286`).

**Files:**
- Modify: `internal/ui/static/collectionmodel.js`, `internal/ui/static/app.js`,
  `internal/ui/collectionmodel_test.go`, `internal/ui/scan_test.go`,
  `test/e2e/scenarios_console_test.go`, `docs/specs/ui.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the window, its size, where it lives, and how the browser dispatches two clicks.

`collectionmodel.js` gains, beside `initializingRetryMs` (`internal/ui/static/collectionmodel.js:23-24`):

```js
// confirmDelayMs is how long after arming a control refuses the press that would send its request.
// A double-click delivers its second click inside this interval on every platform,
// so the gesture that arms a control cannot also confirm it.
const confirmDelayMs = 500;

// confirmAccepted reports whether a press at now, on a control armed at armedAt, sends the request.
function confirmAccepted(armedAt, now) {
  return count(now) - count(armedAt) >= confirmDelayMs;
}
```

`nextState` (`:259-268`) gains `armedAt`, kept for the attempt phases and `0` otherwise;
`startNext` (`:289-360`) reads `e.now` on `arm` into `armedAt`,
and in the `armed` phase answers `submit` with `inflight` only when `confirmAccepted(s.armedAt, e.now)`, and with `unchanged` otherwise;
the `retained` phase's `submit` is unchanged.
The doc comment of `startNext` (`:275-288`) names the new field and the window.
The export statement gains `confirmAccepted`.

`app.js`: `onStart` (`internal/ui/static/app.js:623-634`) passes `now: Date.now()` on the `arm` event and on the `submit` event;
`onCancel` (`:672-682`) sets `this.cancelArmedAt = Date.now()` when it arms
and, when the pressed row is the armed one, returns before `clearTimeout(this.cancelTimer)` unless `confirmAccepted(this.cancelArmedAt, Date.now())`;
the import from `./collectionmodel.js` (`:26-36`) gains the name.
The comments at `:620-622` and `:670-671` say the second press counts only past the window.

- [x] **Write the model test, and run the window case first**

`internal/ui/collectionmodel_test.go`, in two steps, because `loadModel` (`internal/ui/portmodel_test.go:47-59`)
fails a test at load when a listed function is absent,
so a list extended before the behavior case runs would hide the `inflight` the case exists to show.

First step, touching no list:
`startState` gains `ArmedAt int` under `json:"armedAt"`;
`armedState` sets `ArmedAt: 1000`;
the `arm` event of `TestCollectionModelStartNext` carries `now: 700` and `armedFresh` expects `ArmedAt: 700`;
the `submit` event carries `now: 1600`, and a new event `submit inside the window` carries `now: 1200`;
the named table gains `armed/submit inside the window` expecting the armed state unchanged and no message,
and `retained/submit inside the window` expecting `inflight`, because a retained control has no window.
`TestCollectionModelKeySurvivesLostAnswers` passes `now` values past the window on its submits.

```bash
go test -race -count=1 ./internal/ui/ -run 'TestCollectionModelStartNext'
```

`armed/submit inside the window` reports `inflight` where the armed state was expected,
because today every `submit` on an armed control moves;
the `armedFresh` case reports an `armedAt` the state does not carry.
That is the defect: a second press that sends whenever it arrives.

Second step:
`collectionModelFunctions` gains `"confirmAccepted"` after `"startNext"`;
a new `TestCollectionModelConfirmAccepted` is a table:
`(1000, 1000)` false, `(1000, 1499)` false, `(1000, 1500)` true, `(1000, 9000)` true,
`(nil, 200)` false and `(nil, 500)` true because an absent arm time reads as zero.
`internal/ui/scan_test.go`: the list in `TestScanPageUsesCollectionModel` (`:183-193`) gains `"confirmAccepted"`,
and the comment at `:171-176` counts ten functions and names `retryAfterSeconds` as the one not called.

```bash
go test -race -count=1 ./internal/ui/ -run 'TestCollectionModelShape|TestCollectionModelConfirmAccepted|TestScanPageUsesCollectionModel'
```

The shape test reports an export statement missing `confirmAccepted`;
the new table fails at load, because the function does not exist;
the scan reports an import that does not name it.

- [x] **Write the browser step**

`test/e2e/scenarios_console_test.go`, in `scenarioConsoleOIDC`:
`confirmWindow` is a constant of 600 milliseconds with a comment naming the page's half second;
`pressTwiceInsideTheWindow(t, s)` runs one evaluation in the page
that clicks the button labelled **Start collection**, awaits a 50 millisecond timer,
clicks the button labelled **Confirm start** the first press rendered, and returns the elapsed milliseconds;
the step fails when the elapsed time is 400 or more, so the two clicks are inside the window by construction.
It then asserts **Confirm start** and **Keep** are rendered and `len(s.sentTo(http.MethodPost, route)) == 0`,
waits `confirmWindow`, and presses **Confirm start** once.
The pair at `:208-209` becomes this step, `assertStartRequest` (`:216`) then asserts exactly one `POST` as today,
and the cancel pair at `:221-222` gains a `chromedp.Sleep(confirmWindow)` between its two presses,
because two `Run` calls can land inside half a second on a fast machine and the confirm would then be refused.

The step is written here and runs with the suite in *Validation*,
where one run with `confirmDelayMs` at `0` shows it load-bearing:
against a page with no window the second click confirms, **Confirm start** is gone, and the step fails on its absence.
The model test above is this task's red;
the suite is not run against the unchanged page for a defect the model test already shows.

- [x] **Refuse the press and say so**

`docs/specs/ui.md` *Unit*, in the armed-state paragraph of the Collection-control bullet (`:1523-1532`):
"a `submit` inside half a second of the arm leaves an armed control armed and says nothing,
one at or past it moves to `inflight`,
and a retained control has no window";
*Starting and cancelling a Collection* (`:781-784`) gains one sentence after "only the second press sends a request":
"a second press inside half a second of the first is refused, so a double-click arms and sends nothing";
and the start bullet of *End to end* (`:1740-1746`) gains
"two clicks fifty milliseconds apart on **Start collection** leave **Confirm start** standing and send no `POST`".
One amendment row names the three sections.

`CHANGELOG.md`, `### Fixed`:
**A double-click on Start collection or Cancel no longer creates or cancels a Collection.**
The first click armed the control and the second, a few tens of milliseconds later, confirmed it,
so one gesture sent the request the two presses exist to guard.
A press inside half a second of the arm is now ignored and the control stays armed;
a person who presses twice deliberately sees **Confirm start** standing and presses it again.

- [x] **Validate and commit**

```bash
go vet -tags e2e ./test/e2e/
semlf check internal/ui/static/collectionmodel.js internal/ui/static/app.js test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/collectionmodel.js internal/ui/static/app.js internal/ui/collectionmodel_test.go \
  internal/ui/scan_test.go test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
git commit -F <file holding: "fix(ui): refuse a confirm inside the arm window" and a body of one sentence per line under 120 characters saying that a double-click confirmed what its first click armed and that the model now measures a second press against the arm>
git log --oneline -1 && git status --short
```

---

## 2. Download is disabled while no Pod is eligible, fetches otherwise, and shows its error

Closes the roadmap bullet beginning ***Download** with no target does nothing visible* (`docs/plans/roadmap.md:287-289`):
the disabled control and the envelope shown when a download fails.
The two halves are one task because a control that can be disabled is a `<button>`,
and a `<button>` that downloads needs the fetch;
a task that replaced the anchor and wired nothing would leave a live control that does nothing.

**Files:**
- Modify: `internal/ui/static/targetmodel.js`, `internal/ui/static/app.js`,
  `internal/ui/targetmodel_test.go`, `internal/ui/scan_test.go`,
  `test/e2e/browser_test.go`, `test/e2e/scenarios_console_test.go`, `docs/specs/ui.md`, `CHANGELOG.md`
- `scenarios_console_test.go` is edited in both console scenarios,
  because `awaitDownload` returns two values here and each of its two callers unpacks them.

**The decision, and why.**
*Decisions* settles the function and the enabled states, `asBlob`, `settle`, the anchor, the filename,
what `urls.js` gains, which is nothing, and what the browser observes.

`targetmodel.js` gains, after `targetSummary`:

```js
// downloadNote is the line beside a disabled Download, over the summary targetSummary built:
// empty for a summary with targets, which is the enabled control;
// each excluded row's count and wording in the rows' order for the reasons kind;
// the selector sentence for noSelector; and the plain wording otherwise.
function downloadNote(summary) {
  const s = summary || {};
  const empty = s.empty;
  if (!empty) {
    return "";
  }
  if (empty.kind === "reasons") {
    return (empty.rows || []).map((r) => `${r.count} ${r.text}`).join("; ");
  }
  if (empty.kind === "noSelector") {
    return "the Service's selector matches no Pod";
  }
  return "no target listed";
}
```

and its export statement becomes `export { targetsQuery, retryWithoutExplain, targetSummary, downloadNote };`.

`app.js`, the control:
`renderRequest` (`internal/ui/static/app.js:1071-1157`) computes `const note = downloadNote(summary)`
and renders, in place of the anchor at `:1146`,
`<button type="button" disabled=${!url || note !== "" || downloading} onClick=${this.onDownload}>${downloading ? "Downloading" : "Download"}</button>`,
with `${note ? html\`<small>${note}</small>\` : null}` beside it inside `.actions`
and `${this.panelError("download")}` under `.actions`.
The import at `:25` gains `downloadNote`.
The button exists whenever `limits` has arrived, disabled while no URL is built, so it is never hidden (`docs/specs/ui.md:672-673`).
The state gains `downloading: false`.

`app.js`, the fetch:
`fetchJSON(url, req, asBlob)` (`:89-138`):
`out` gains `blob: null`;
when `asBlob` and `res.status === 200`, `out.blob = await res.blob()` and the function returns `out` before the text is read;
every other status is read exactly as today, so the envelope rule is one code path.
The doc comment (`:76-88`) says a `200` under `asBlob` is a `Blob` and every other answer is read as it is today.
`200` and not `res.ok`: the page's success boundary is `200` (`:395`) and *Flow*'s is `200` (`docs/specs/ui.md:521`),
and a `204` read as a `Blob` would save an empty file.
`request` (`:386-421`) keeps its two fetches and its `200` branch, and its tail from `:399` becomes:

```js
    return this.settle(key, res, retry);
  }

  // settle records what a request that did not answer 200 leaves behind:
  // the 401 rule of Signing in and out, the not_ready retry, and the error under key.
  // It returns the error it recorded, or null when the 401 rule took the answer,
  // so a caller can act on the code without reading state a setState has not applied yet.
  settle(key, res, retry) {
    ... the body of :399-420, each `return null` before the record unchanged ...
    return res.error;
  }
```

`request` returns `null` where it did before: `return this.settle(...)` becomes `this.settle(...); return null;`.
The `realm_denied` refetch lands in task 8; here `settle` is the move and the return value.

`onDownload`:

```js
  // onDownload fetches the profile and saves a 200 through an object URL;
  // any other answer is shown in the Profile panel under the rule every listing follows,
  // and a service_not_found refetches the Service list as the two loaders do.
  // The control is disabled from the press until the body has been read whole and the save has begun.
  onDownload = async () => {
    const url = this.currentProfileURL();
    if (!url || this.state.downloading) {
      return;
    }
    this.setState({ downloading: true });
    const res = await fetchJSON(url.href, undefined, true);
    if (res.blob !== null) {
      this.clearError("download");
      saveBlob(res.blob, filenameOf(res.headers.get("content-disposition")));
    } else {
      const err = this.settle("download", res, this.onDownload);
      if (isEnvelope(err) && err.code === "service_not_found") {
        this.loadServices();
      }
    }
    this.setState({ downloading: false });
  };
```

The refetch reads the value `settle` returned and not `this.state.errors`,
because `setState` is applied later than the line after it,
and `afterServiceError` (`:721-726`) reads state the loaders' callers have already let settle.
`saveBlob(blob, name)` mints the URL with `URL.createObjectURL`, creates an anchor with `document.createElement("a")`,
sets `href` and `download`, appends it to `document.body`, clicks it, removes it, and revokes the URL;
`filenameOf(header)` returns the `filename` parameter of a `Content-Disposition`, quoted or bare, and `"profile"` when there is none.
Both are module-level functions beside `isEnvelope`, and neither spells a path.
`clearWriteControls` is untouched: a download is a read.

- [x] **Write the model test**

`internal/ui/targetmodel_test.go`:
`targetModelFunctions` gains `"downloadNote"` and the comment at `:12` counts four;
a new `TestTargetModelDownloadNote` drives `targetSummary` and then `downloadNote` on its result in one interpreter, a table of:
`a summary with targets yields no line` over the body of `TestTargetModelSummaryWithTargets`, expecting `""`;
`the ten reasons in order` over the vocabulary body of `TestTargetModelSummaryEmpty`,
expecting the ten `<count> <wording>` pairs joined with `; ` in vocabulary order;
`the ten reasons shuffled keep their order` over the shuffled body, expecting the reversed joining;
`an unrecognized reason is its own words` over the `pod_on_fire` body,
expecting `1 Pods whose Ready condition is not True; 3 pod_on_fire`;
`selectorMatched of 0` expecting the selector sentence;
`no excluded field` and `excluded of []` each expecting `no target listed`.
Each case also asserts `Unchanged`.

`internal/ui/scan_test.go`: the list at `:157` gains `"downloadNote"`, and the comment at `:148-150` counts four.

```bash
go test -race -count=1 ./internal/ui/ -run 'TestTargetModelShape|TestTargetModelDownloadNote|TestScanPageUsesTargetModel'
```

The shape test reports an export of three names;
the new test fails at load, because there is no fourth function;
the scan reports an import that does not name it.
The wrong implementation is a page that keeps the control live over an empty listing and lets the gateway answer `503 no_targets` to nobody.

- [x] **Write the browser steps, and run them red**

`test/e2e/browser_test.go`:
`sentRequest` (`:151-158`) gains `resourceType network.ResourceType`, set by `observe` from `e.Type` of `EventRequestWillBeSent`
(`$(go env GOMODCACHE)/github.com/chromedp/cdproto@v0.0.0-20260714215040-dc233986426f/network/events.go:73`);
`session` gains `began []downloadBegan`, a record of `guid`, `url`, and `suggested`,
appended under `mu` by `observe` on `*cdpbrowser.EventDownloadWillBegin` from its `GUID`, `URL`, and `SuggestedFilename`
(`browser/events.go:12-17` of the same module);
`downloadsBegun() int` returns `len(s.began)` under `mu`.
`awaitDownload` (`:436-466`) returns `(downloadBegan, []byte)`:
the completion's `GUID`, which `finished` already carries, selects the `began` record,
and the test fails when no record carries it,
because a completion for a download the browser never announced is a harness fault.
The bytes are read from the directory as today, under whatever name the browser wrote;
the name on disk is not asserted anywhere,
because `SuggestedFilename` is documented as the name the disk may differ from.

`test/e2e/scenarios_console_test.go`:

`nobodyService(t, h, ns)` runs right after `deployTestApp` (`:64`).
It creates a `Service` named `nobody` in `ns` through `h.Client.CoreV1().Services(ns).Create`,
the way `applyConfigMap` creates its object (`test/e2e/harness_test.go:1569-1579`):
selector `app.kubernetes.io/name: nobody`, one `ClusterIP` port, no other field.
The catalog lists a Service with a selector whatever it matches (`internal/k8s/catalog.go:38-42`),
and the eligibility read answers `selectorMatched` of `0` for one that matches nothing (`internal/k8s/eligibility.go:92-96`).
It is created before the working load (`:180-183`), which is where the page fetches the Service list it keeps,
and every fixture between the two — the issuer, the gateway rollout, the seeded Collection —
is time for the informer to deliver it;
`chooseOption` names the options offered when a list lacks a value, which is what a failure here would read.
A Service created after that load would never appear:
`loadServices` runs on the namespace list arriving, on a namespace change, and on a `service_not_found` (`internal/ui/static/app.js:442-468`, `:721-726`, `:733-760`),
and on nothing else.

`awaitTargetsEmpty(t, c, header, rawURL)` polls `try(ctx, c, http.MethodGet, rawURL, header, nil)` for `settleDeadline`
until the answer is `200` and its body decodes into `targetsResponse` with no `targets`,
the shape `awaitPGORoutes` gives its poll (`:368-384`);
`waitTargets` (`test/e2e/scenarios_test.go:324-349`) is not used,
because it polls `h.Gateways`, not the gateway this scenario deployed.

In `scenarioConsoleOIDC`:

1. The download (`:194-195`) becomes:
   press **Download**; `began, body := s.awaitDownload(...)`;
   `assertGzipFramed(body)`;
   assert `began.suggested == "heap"`, the profile the scenario chose, which the pprof handler names in `Content-Disposition`;
   assert `strings.HasPrefix(began.url, "blob:")`;
   assert the last request in `s.sentTo(http.MethodGet, wantURL)` has `resourceType == network.ResourceTypeFetch`.
   Today the request is a `Document` and the download's URL is `wantURL` itself,
   so the last two assertions are what a navigation fails.
2. After the cancel moves the row (`:223-226`):
   `chooseOption(t, "Service", "nobody")`;
   `waitFor` the Profile panel to hold "the Service's selector matches no Pod";
   assert the **Download** button is `disabled` and that a `<small>` inside `.actions` holds the same sentence.
3. `chooseOption(t, "Service", testAppName)`;
   `waitFor` the Pod control to list a Pod:
   the `select` under the label starting with `Pod` has `options.length > 1`, the placeholder `any` being the one option an empty menu has;
   the URL field is not the signal,
   because `currentProfileURL` (`:902-913`) builds from the selection and the duration and needs no targets answer,
   and a scale-down racing the page's own targets fetch would disable the control before the press this step is for.
   Then patch `testapp` to zero replicas with the merge patch of `deployTestAppScaled` (`test/e2e/scenarios_pgo_test.go:359-363`);
   `awaitTargetsEmpty(t, client, bearer, gatewayOrigin+"/v1/namespaces/"+ns+"/services/"+testAppName+"/targets")`;
   assert the page's Pod control still lists a Pod;
   `before := s.downloadsBegun()`;
   press **Download**;
   `waitFor` the Profile panel to hold `no_targets`;
   assert `s.downloadsBegun() == before`.

In `scenarioConsoleBasic`, the download (`:285-286`) becomes `began, body := s.awaitDownload(...)`,
`assertGzipFramed(t, "the file the browser saved", body)`,
and an assertion that `began.suggested == "heap"`, the profile that scenario chose as well (`:281`).
That call is edited whether or not a run selects the scenario,
because a filtered scenario is still compiled and a two-result call cannot stand where one argument is expected.

`docs/specs/ui.md` *End to end* (`:1737`): the download's name reads `heap`,
"the profile the scenario chose, which Go's pprof handler names in `Content-Disposition`",
and the bullet says the download is observed as a `Fetch` request followed by a download from a `blob:` URL.
One amendment row names *End to end*.

The red run, against the unchanged page, on a machine with a Chromium and the kind lane:

```bash
go test -tags e2e -race -count=1 -timeout 40m -run 'TestScenarios/console-oidc$' ./test/e2e/
```

Step 1 fails first: the download's URL is the profile URL and not a `blob:` one, and the request is a `Document`.
That failure is the red evidence, and it also ends the run:
`waitFor` and the assertions around it fail the scenario (`test/e2e/browser_test.go:373`),
so no step after the first failing one is reached.
What steps 2 and 3 would fail on is read from the code and not observed —
step 2 a **Download** that is an enabled anchor,
step 3 a Profile panel that never holds `no_targets`, because a navigation shows the page nothing —
and the run after the change is what proves both green.

- [x] **Name what the scans hold**

No unit test runs `app.js`.
`TestScanNoHTMLInterfaces`, `TestScanPathsLiveInURLs`, `TestScanNoInlineForms`, and `TestScanNoMutableTopLevelState`
(`internal/ui/scan_test.go:75-89`, `:204-212`, `:246-254`) run over the changed file and stay green,
and a `saveBlob` that reached for `innerHTML` or a literal `/v1` path would turn them red.
*What is not proven* already states the rest (`docs/specs/ui.md:1658-1659`).

- [x] **Disable, fetch, save, and say so**

`CHANGELOG.md`, `### Fixed`:
**Download is disabled, with the reason beside it, while a Service has no eligible Pod.**
The link stayed live over an empty target list, and the `503 no_targets` it earned was a navigation the browser showed nothing for.
The control is now disabled while the targets response lists no Pod,
and the counted reasons, the selector sentence, or "no target listed" stand beside it;
a targets response that lists a Pod enables it again.

`CHANGELOG.md`, `### Changed`:
**Download fetches the profile and saves it, rather than navigating to it.**
A navigation showed nothing when the profile endpoint answered an error,
so `no_targets`, `service_not_found`, and `realm_denied` were invisible from the console.
The page now fetches the profile, saves a `200` under the name its `Content-Disposition` carries,
and shows any other answer with its hint where every other error is shown;
the control is disabled while the download is in flight, so a second press sends nothing.
A response that declares no `Content-Encoding`, which is every response the pprof handler writes,
is saved as the gzip-framed body `curl` receives;
a response an intermediary encoded is saved decoded, so its bytes are not the wire bytes.

- [x] **Validate and commit**

```bash
go vet -tags e2e ./test/e2e/
semlf check internal/ui/static/targetmodel.js internal/ui/static/app.js test/e2e/browser_test.go test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/targetmodel.js internal/ui/static/app.js internal/ui/targetmodel_test.go internal/ui/scan_test.go \
  test/e2e/browser_test.go test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
git commit -F <file holding: "feat(ui): fetch a profile, or say why it cannot" and a body saying the link stayed live over no target and a navigation showed no error, what the fourth model function yields, how a 200 is saved and named, and that the control is disabled while in flight>
git log --oneline -1 && git status --short
```

---
## 3. Each list has a Refresh control, and the two are independent

Closes the first half of the roadmap bullet beginning *Nothing refreshes* (`docs/plans/roadmap.md:292-297`):
the control.

**Files:**
- Modify: `internal/ui/static/app.js`, `test/e2e/browser_test.go`, `test/e2e/scenarios_console_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the two counters, the two flags, what a refresh leaves alone, and how the browser observes a press.

`app.js`:
`this.seq` (`internal/ui/static/app.js:254-256`) becomes `this.targetsSeq` and `this.collectionsSeq`, each with its own comment;
`onNamespace` (`:735`) and `onService` (`:764`) raise both and set `targetsLoading: false` and `collectionsLoading: false` in the state they reset,
because a selection change leaves no request whose answer will be applied.
`loadTargets` (`:472-493`) raises `targetsSeq` into `seq` and sets `targetsLoading: true` before its request;
it passes `request` a fifth argument, `() => seq !== this.targetsSeq`;
when the answer arrives and `seq !== this.targetsSeq` it returns, touching nothing;
otherwise it sets `targetsLoading: false` in the same update that applies or drops the answer,
and after `targetSummary` is applied keeps `pod` when `summary.pods` includes it and `version` when `summary.versions` includes it,
returning each to `""` otherwise.
`loadCollections` (`:506-522`) does the same over `collectionsSeq` and `collectionsLoading`.
Each flag therefore belongs to the request generation that set it:
a request that finishes after a newer one started is stale, and a stale completion clears nothing,
so **Refresh** stays disabled until the newest request has settled.
`maybeLoadCollections` is unchanged.

`request(key, url, retry, retryOnce, stale)` (`:386-421`) gains the fifth parameter, optional:
when `stale` is given and returns `true` once the answer has arrived, `request` returns `null` before the `200` branch and before `settle`,
so a stale answer records no error, schedules no `not_ready` retry, and navigates nowhere;
the doc comment (`:378-385`) says so.
Every other caller passes nothing and is unchanged.
The `not_ready` retry a live answer schedules is the loader itself (`:414`, `retry` is `this.loadTargets`),
which raises the generation when it fires;
a **Refresh** pressed during the two-second wait starts a generation of its own,
the retry then starts a newer one, and the earlier answer is dropped whole by its `stale` check.
The two requests are for one selection, and the later one wins, which is the rule the generation already states;
no second mechanism decides between a retry and a press.
`startTimer`, `cancelTimer`, and `cancelRetryTimer` are neither read nor cleared by any of this.

`onRefreshTargets = () => { if (this.state.svc) { this.loadTargets(); } }` and `onRefreshCollections = () => this.loadCollections()`,
each with a comment saying what it does not touch:
the start state, the armed cancel, the three write-control timers, and the other list.
`renderRequest` draws the control after the Pod and version controls:
`<button type="button" class="secondary" disabled=${!svcListed || targetsLoading} onClick=${this.onRefreshTargets}>Refresh</button>`,
and `renderCollections` draws the same shape over `collectionsLoading` in its header beside the panel's title.
The state gains `targetsLoading: false` and `collectionsLoading: false`.

- [x] **Write the browser steps, and run them red**

`test/e2e/browser_test.go`:
`session` gains `requestCount() int`, `len(s.requests)` under `mu`,
and `awaitRequestSince`, whose signature is
`func (s *session) awaitRequestSince(t *testing.T, since int, what string, match func(sentRequest) bool) sentRequest`:
it polls for `settleDeadline`
until a request at an index of `since` or beyond satisfies `match`, and returns the first that does,
reading the request slice under `s.mu` and releasing the lock before it fails;
the failure names `what` and prints `s.report()`.
`awaitRequest` (`test/e2e/scenarios_console_test.go:600-608`) is left as it is:
it answers "was this ever sent", and the two presses below ask "what did this press send",
which an exact match over the whole list cannot answer once the same `GET` has been sent before.
A request is recorded when the browser is about to send it (`test/e2e/browser_test.go:283`),
so a step that reads what the answer did waits for the answer to be applied first:
the **Refresh** control it pressed is disabled while the fetch is in flight and enabled again once the answer is applied or dropped,
and that transition is the wait.

`test/e2e/scenarios_console_test.go`:
`refreshButton(panel)` is the XPath for the **Refresh** button inside the `article` whose header reads `panel`,
`Profile` for the targets list and `Collections` for the table, in the shape `control` gives a label (`:560-562`);
`refetchCollections` (`:670-682`) presses **Refresh** on the Collections table instead of choosing the Service again,
and `awaitRow`'s comment (`:610-613`) says so;
`awaitRow` keeps its poll and its second failure wording,
and its first becomes "the Collections Refresh control never became available, so the list was never asked for again",
because that control is what the helper presses from here on.

In `scenarioConsoleOIDC`:

1. After `awaitRow(t, started, ...)` (`:218`) and the cancel's arm (`:221`):
   `waitFor` the Collections **Refresh** to be enabled, which is the page idle on that list;
   `n := s.requestCount()`;
   press **Refresh** on the Collections table;
   `awaitRequestSince(t, n, "the Refresh of the Collections table", r.method == GET && r.url == route)`;
   `waitFor` the Collections **Refresh** to be enabled again;
   assert `s.requestCount() == n+1`, so the press sent that `GET` and nothing else;
   assert **Confirm cancel** and **Keep** are rendered on the row;
   then `chromedp.Sleep(confirmWindow)` and confirm the cancel as task 1 left it.
   The armed cancel disarms itself after ten seconds (`internal/ui/static/app.js:676`),
   and one `GET` of the list settles well inside that.
2. After the `no_targets` step task 2 wrote, as the scenario's last step against the app:
   `waitFor` the targets **Refresh** to be enabled;
   `n := s.requestCount()`;
   press **Refresh** on the targets list;
   `awaitRequestSince(t, n, ..., r.method == GET && strings.HasPrefix(r.url, targetsRoute) && strings.Contains(r.url, "explain=true"))`;
   `waitFor` the targets **Refresh** to be enabled again;
   assert `s.requestCount() == n+1`;
   assert **Download** is `disabled` and the `<small>` beside it is non-empty,
   holding either the `pod_terminating` wording or the selector sentence:
   the deleted Pods stay `Terminating` for the preStop sleep (`test/e2e/testapp/deployment.yaml:38-41`),
   so the answer excludes them as `pod_terminating` first and reports `selectorMatched` of `0` once they are gone.

The red run, against the page as task 2 left it:

```bash
go test -tags e2e -race -count=1 -timeout 40m -run 'TestScenarios/console-oidc$' ./test/e2e/
```

`awaitRow` fails first, on no **Refresh** button to press in the Collections panel;
step 2 would fail the same way on the targets list.
The independence of the two counters and the Pod and version reconciliation run in no test
and are stated under *What is not proven* (`docs/specs/ui.md:1660`);
the browser check of *Validation* exercises both by hand.

- [x] **Name what the scans hold**

`TestScanNoInlineForms` and `TestScanNoMutableTopLevelState` stay green over the changed file.

- [x] **Add the two controls and say so**

`CHANGELOG.md`, `### Added`:
**The targets list and the Collections table each have a Refresh control.**
A Collection started from the page never left `pending` without a reload,
because nothing on the page fetched a list again.
Each control repeats the one fetch its list came from and nothing else,
is disabled while that fetch is in flight, leaves an armed **Start collection** or **Cancel** as it is,
and a refresh of one list never discards the other's answer.
Watching a Collection leave `pending` is a press, never a timer.

- [x] **Validate and commit**

```bash
go vet -tags e2e ./test/e2e/
semlf check internal/ui/static/app.js test/e2e/browser_test.go test/e2e/scenarios_console_test.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/app.js test/e2e/browser_test.go test/e2e/scenarios_console_test.go CHANGELOG.md
git commit -F <file holding: "feat(ui): add a Refresh control to each list" and a body saying nothing refetched a list, that each control repeats one fetch, and that the two lists no longer share a request counter>
git log --oneline -1 && git status --short
```

---

## 4. `profgate collections` walks every page

Closes the edit the spec revision made in [`cli.md`](../specs/cli.md) *Collections* (`docs/specs/cli.md:953-966`),
before the console names the verb as the one that lists every Collection (task 5),
so the page never names a verb that does not yet do what the page says.

**Files:**
- Modify: `cmd/profgate/collect.go`, `cmd/profgate/collect_test.go`, `internal/client/wire.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the loop in `run`, the buffered pages, the failure path, and the exit code of a page that fails.

`internal/client/wire.go:86-89`: `CollectionsResponse` gains `NextCursor string `json:"nextCursor"``,
with a comment saying it is the token that continues the listing and empty on the last page.

`cmd/profgate/collect.go:270-299` becomes:

```go
// collectionsVerb is GET .../collections, walked through every page:
// one GET per page, each after the first carrying the previous answer's nextCursor as cursor,
// until an answer carries none.
// The table is printed once the walk completes, so a page that fails leaves no partial table that reads as complete;
// under --output json each page's body is written after the walk, in order, unchanged.
// The plural takes a Service; an identifier in its place fails the address grammar before any request.
func collectionsVerb() verb {
	return verb{
		name: "collections", leaves: []leaf{{grammar: "collections <ns>/<svc>", positionals: 1}},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			gw, s, err := env.gateway(ctx, in.globals)
			if err != nil {
				return fail(env, "", err)
			}
			ns, svc, err := address(in.positionals[0], s.Namespace)
			if err != nil {
				return fail(env, s.Output, err)
			}
			pages, rows, err := walkCollections(ctx, gw, servicePath(ns, svc)+"/collections")
			if err != nil {
				return fail(env, s.Output, err)
			}
			if s.Output == "json" {
				for _, body := range pages {
					if _, err := env.stdout.Write(body); err != nil {
						return fail(env, s.Output, err)
					}
				}
				return exitOK
			}
			if err := writeTable(env.stdout, env.terminal, []string{"ID", "STATE", "ORIGIN", "CREATED"}, rows); err != nil {
				return fail(env, s.Output, err)
			}
			return exitOK
		},
	}
}
```

`walkCollections(ctx, gw, path)` loops `gw.JSON(ctx, client.Request{Method: http.MethodGet, Path: path, Query: q})`,
with `q` nil on the first page and `url.Values{"cursor": {next}}` afterwards,
decodes each body with `client.Decode[client.CollectionsResponse]`,
appends the body and the rows, and stops when `NextCursor` is empty;
an error from any page returns at once with the pages so far discarded.
The `Query` field of `client.Request` (`internal/client/client.go:55-61`) is what `build` encodes (`:188`).
The error reaches `fail` (`cmd/profgate/exit.go:53-60`) and `exitCode` (`:23-34`),
which is the general rule of every verb:
`3` for a `401`, envelope or not, and `1` for every other refusal,
and is what *Collections* (`docs/specs/cli.md:961-963`) and *Testing* (`:1341-1343`) say a page that fails mid-walk exits with.

- [x] **Write the test**

`cmd/profgate/collect_test.go` gains `pagedTransport`, beside `retryTransport` (`:195-213`):
a map from the `cursor` query value to a `func() (*http.Response, error)`, recording every request.
Two more bodies:
`collectionsPageOne` is `collectionsBody` with `"nextCursor":"c1"` added,
and `collectionsPageTwo` holds one record, `9k3m5p7r2t4v6w8x0y1z`, `state` `expired`, and no `nextCursor`.
A table test `TestCollectionsWalksEveryPage`:

| Case | Transport | Asserts |
|---|---|---|
| `one page sends one request` | `""` answers `collectionsBody` | one request with an empty `RawQuery`; the table of `TestCollectionsTable`'s first case; exit 0 |
| `two pages send two requests and one table` | `""` answers page one, `c1` answers page two | two requests, the second with `RawQuery` `cursor=c1`; stdout is the header and the three rows in arrival order; exit 0 |
| `a second page that fails prints the envelope and no row` | `""` answers page one, `c1` answers `503 {"error":"the store is unavailable","code":"pgo_unavailable"}` | exit 1; stderr holds `pgo_unavailable: the store is unavailable`; stdout is empty, asserted against the two rows page one carried |
| `a second page answered 401 exits 3` | `""` answers page one, `c1` answers `401 {"error":"the session has expired","code":"unauthenticated"}` | exit 3; stderr holds `unauthenticated: the session has expired`; stdout is empty |
| `two pages under json write two documents` | as the second case, with `--output json` | stdout is page one's body followed by page two's, byte for byte |
| `a failing page under json writes its envelope alone` | as the third case, with `--output json` | stdout is the envelope's bytes exactly, and holds neither identifier of page one |

`TestCollectionsOneGET`, `TestCollectionsTable`, and `TestCollectionsJSON` (`:497-563`) stay as they are and green:
`collectionsBody` carries no `nextCursor`, so the walk is one request.

The red state:

```bash
go test -race -count=1 ./cmd/profgate/ -run 'TestCollectionsWalksEveryPage'
```

The two-page case reports one request where two were expected and a table missing the third row;
the two failing-page cases report exit 0 and a two-row table, because today the first page is the whole answer;
the json cases report one document.
The wrong implementation is the one-`GET` verb that ships today.

- [x] **Walk the pages and say so**

`CHANGELOG.md`, `### Added`:
**`profgate collections` lists every Collection a Service retains, not the first hundred.**
The verb sent one request and printed the first page, dropping the token that continued the listing.
It now follows `nextCursor` through every page and prints one table once the walk completes;
a page that fails mid-walk prints its envelope and no row, and exits as any refusal does,
and under `--output json` each page's body is written in order, or the failing page's envelope alone.

- [x] **Validate and commit**

```bash
semlf check cmd/profgate/collect.go internal/client/wire.go CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add cmd/profgate/collect.go cmd/profgate/collect_test.go internal/client/wire.go CHANGELOG.md
git commit -F <file holding: "feat(cli): walk every page of collections" and a body saying the verb printed one page, how the cursor is carried, what a failing page prints under each output, and that a 401 mid-walk exits 3 as every 401 does>
git log --oneline -1 && git status --short
```

---

## 5. The table says when older Collections exist, and names the verb

Closes the second half of the roadmap bullet beginning *Nothing refreshes*:
the paging line, which names a verb task 4 made true.

**Files:**
- Modify: `internal/ui/static/collectionmodel.js`, `internal/ui/static/app.js`,
  `internal/ui/collectionmodel_test.go`, `internal/ui/scan_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the function, the text, and that the line and the rows are one result.

`collectionmodel.js` gains, after `progressText`:

```js
// olderCollectionsNote is the line under the Collections table when the listing carries a nextCursor:
// older Collections exist beyond the page, and the command line lists them all.
// The token is never shown, because the page offers no paging control and sends it nowhere.
function olderCollectionsNote(body, namespace, service) {
  const b = body || {};
  if (text(b.nextCursor) === "") {
    return "";
  }
  return `Older Collections exist beyond this page; profgate collections ${text(namespace)}/${text(service)} lists them all`;
}
```

and its export statement gains the name.
`app.js`: the state gains `collectionsNote: ""`;
`loadCollections` stores `collectionsNote: olderCollectionsNote(body, ns, svc)` in the update that stores `collections`,
and `collectionsNote: ""` in the failure update that stores `collections: []` (`internal/ui/static/app.js:516`),
so a Refresh that fails after a page carried a token leaves an empty table with no line under it saying older records exist;
`onNamespace` and `onService` clear it with `collections`;
`renderCollections` renders `<p><small>${collectionsNote}</small></p>` under `.table` when it is non-empty;
the import gains the name.

- [x] **Write the test**

`internal/ui/collectionmodel_test.go`:
`collectionModelFunctions` gains `"olderCollectionsNote"` last;
a new `TestCollectionModelOlderCollectionsNote` is a table:
`a non-empty nextCursor yields the line` over `{"collections": [], "nextCursor": "MWFiYw"}` with `payment`, `payment-api`,
expecting the fixed text with `payment/payment-api`;
`another token yields the same text` over `nextCursor` `"Mnh5eg"`, expecting the same string;
`no nextCursor yields none` and `an empty nextCursor yields none`, each expecting `""`;
`the token is never in the line` asserts the yielded string does not contain the token.
Each case asserts `Unchanged`.

`internal/ui/scan_test.go`: the list in `TestScanPageUsesCollectionModel` gains `"olderCollectionsNote"`, and the comment counts eleven.

The red state:

```bash
go test -race -count=1 ./internal/ui/ -run 'TestCollectionModelShape|TestCollectionModelOlderCollectionsNote|TestScanPageUsesCollectionModel'
```

The shape test reports the export statement;
the new test fails at load;
the scan reports the import.
The wrong implementation is today's `loadCollections`, which keeps `body.collections` and drops the token at `:521`,
so a Service with a hundred and one Collections shows a hundred and says nothing.
The clearing on failure runs in no test and is stated under *What is not proven* with the rest of the wiring (`docs/specs/ui.md:1660`).

- [x] **Render the line and say so**

`CHANGELOG.md`, `### Added`:
**The Collections table says when older Collections exist, and names the verb that lists them.**
The table showed the newest hundred and dropped the token that said more existed.
When the listing carries `nextCursor`, one line under the table now says older Collections exist beyond the page
and names `profgate collections <ns>/<svc>`;
the console adds no paging control.

- [x] **Validate and commit**

```bash
semlf check internal/ui/static/collectionmodel.js internal/ui/static/app.js CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/collectionmodel.js internal/ui/static/app.js internal/ui/collectionmodel_test.go internal/ui/scan_test.go CHANGELOG.md
git commit -F <file holding: "feat(ui): say when older Collections exist" and a body saying the token was dropped, what the line names, and that no paging control is added>
git log --oneline -1 && git status --short
```

---
## 6. The Collections table spans the page, and Keep is not blue

Closes the roadmap bullets beginning *The Collections table sits in one grid column* (`docs/plans/roadmap.md:290-291`)
and ***Keep** and **Confirm start** render in the same primary blue* (`:298-299`).

**Files:**
- Modify: `internal/ui/static/app.css`, `internal/ui/static/app.js`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the span and the rule from Pico's variables.

`app.css` gains, after `.panels article` (`internal/ui/static/app.css:16-18`):

```css
/* The Collections table has nine columns, so its panel takes a row of its own
   below the three panels rather than one cell beside them. */
.panels article.collections {
  grid-column: 1 / -1;
}
```

and, after `button.armed` (`:78-82`):

```css
/* Pico's class-less build defines the secondary colors and no rule that applies them,
   so Keep would otherwise wear the primary blue of the control it stands beside. */
button.secondary {
  background: var(--pico-secondary-background);
  border-color: var(--pico-secondary-border);
  color: var(--pico-secondary-inverse);
}
```

`renderCollections` (`internal/ui/static/app.js:1221`) opens `<article class="collections">`.

- [x] **Name the check**

This task has no red test: no test measures layout or color, and none is added,
because a test of a stylesheet rule would assert the rule's text and not what the page looks like.
`TestVendorManifestHashes` (`internal/ui/vendor_test.go:189-224`) stays green because no vendored byte moves.
What verifies the task is the browser check of *Validation*, at a 1600 px window:
the Collections table shows `id` through `cancel` without a horizontal scroll,
and an armed row shows **Keep** in grey beside a red **Confirm cancel**;
the pull request description reports both as seen.

- [x] **Style and say so**

`CHANGELOG.md`, `### Fixed`:
**The Collections table takes the page's width, and Keep looks secondary.**
The table sat in one grid column beside empty page, so `state`, the timestamps, and **Cancel** were off-screen at 1600 px,
and **Keep** wore the same primary blue as **Confirm start**, because the class-less Pico build has no rule for `.secondary`.
The panel now spans the grid, and **Keep** takes Pico's secondary colors.

- [x] **Validate and commit**

```bash
semlf check internal/ui/static/app.js CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/app.css internal/ui/static/app.js CHANGELOG.md
git commit -F <file holding: "fix(ui): span the table and color Keep" and a body saying where the table sat, why Keep was blue, and that the vendored file is untouched>
git log --oneline -1 && git status --short
```

---

## 7. `loginURL` keeps the return path under 1024 bytes

Closes the roadmap bullet beginning *`loginURL` omits the 1024-byte return-path bound* (`docs/plans/roadmap.md:300-301`);
the bound is *Flow*'s (`docs/specs/ui.md:506-508`).

**Files:**
- Modify: `internal/ui/static/urls.js`, `test/e2e/scenarios_console_test.go`, `docs/specs/ui.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the measurement and why the proof is a browser step.

`loginURL` (`internal/ui/static/urls.js:100-106`) becomes:

```js
// returnPathBound is the longest return path the browser flow accepts, in bytes of the encoded value.
const returnPathBound = 1024;

// loginURL starts the browser flow and asks it to return to the page's own path and query,
// with the returned marker added so the page can tell a fresh return from a plain load.
// The encoded path and query are ASCII, so their length is their byte count;
// a selection that would cross the bound is left out, and the return carries the marker alone.
export function loginURL(ns, svc) {
  let page = build("/ui/", [], { ns: ns, svc: svc, returned: "1" });
  if (page.pathname.length + page.search.length > returnPathBound) {
    page = build("/ui/", [], { returned: "1" });
  }
  return build("/auth/login", [], { return: `${page.pathname}${page.search}` });
}
```

`TestScanURLsBuilds` (`internal/ui/scan_test.go:100-110`) stays green: the module still concatenates no literal.

- [x] **Write the browser step, and run it red**

No unit test can load `urls.js` (*Decisions*).
`test/e2e/scenarios_console_test.go`, in `scenarioConsoleOIDC`, last before the final `assertClean`:
a second session, `newSession(t, b, sessionOptions{MapTo: local})`, with no cookie,
navigates to `/ui/` with `ns` set to 1100 `a`s and no `svc`;
`awaitRequestSince(t, 0, "the second session's login", r.method == GET && strings.HasPrefix(r.url, gatewayOrigin+"/auth/login?"))`
returns the request, the way `assertNavigatedToLogin` reads the first session's (`:452-475`);
the step parses its `return` query value and asserts it is exactly `/ui/?returned=1`.
The login is not completed; the request the page sent is the whole proof.

`docs/specs/ui.md` *End to end* (`:1735-1761`) gains a bullet:
"a second session with no cookie, opened with an `ns` of 1100 characters, navigates to `/auth/login` with `return=/ui/?returned=1` alone".
One amendment row names *End to end*.

The red run, against the page as task 6 left it:

```bash
go test -tags e2e -race -count=1 -timeout 40m -run 'TestScenarios/console-oidc$' ./test/e2e/
```

The step fails on a `return` carrying the 1100 characters,
because today `loginURL` builds the path from the selection whatever its length.

- [x] **Bound the path and say so**

`CHANGELOG.md`, `### Fixed`:
**The console never sends a login return path longer than the browser flow accepts.**
The page built its return path from the selection whatever its length,
and the browser flow bounds the value at 1024 bytes.
A selection that would cross the bound is now left out and the return carries the marker alone,
so the login completes and lands on a console with no selection rather than on a refusal.

- [x] **Validate and commit**

```bash
go vet -tags e2e ./test/e2e/
semlf check internal/ui/static/urls.js test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/urls.js test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
git commit -F <file holding: "fix(ui): bound the login return path" and a body saying the path was unbounded, what the bound is, and what is sent when a selection crosses it>
git log --oneline -1 && git status --short
```

---

## 8. Every hint is true, and every code of the table has one

Closes the roadmap bullet beginning *The `hints` table lacks* (`docs/plans/roadmap.md:302-303`).

**Files:**
- Modify: `internal/ui/static/app.js`, `internal/ui/scan_test.go`, `test/e2e/scenarios_console_test.go`, `docs/specs/ui.md`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles the four rows, the two replaced texts, the exact-set scan, and the refetch.

The spec's table (`docs/specs/ui.md:1046-1063`) has sixteen rows and eighteen codes,
because `rate_limited, capacity_exhausted` and `version_conflict, version_missing` share a row each;
`hints` (`internal/ui/static/app.js:56-74`) has sixteen keys.
`hints` gains four:
`version_conflict` and `version_missing`, each "the Service's Pods carry more than one version, or none", the table's wording;
`too_many_auth`, "the gateway is checking too many passwords at once; retry in a moment";
and `auth_unavailable`, "the gateway cannot decide who you are right now; retry".
Two keys change their text to the table's:
`no_targets` (`:60`) becomes "no Pod is eligible for the selection; Refresh on the targets list updates the available Pods and the empty state",
the table's row without its bold marks, because a hint is text;
`port_not_allowed` (`:61`) becomes "`allowedSelections` does not admit the value; the port control shows what it does admit".
`settle` (task 2) gains, after it records the error:
when the recorded error is an envelope whose code is `realm_denied` and `key !== "whoami"`, `this.reloadWhoami()`,
so the identity panel the hint names is refreshed by the answer that says the realm moved,
the shape `afterServiceError` gives `service_not_found` (`:721-726`).
`reloadWhoami` (`:554-560`) is already the refetch that replaces the realm without running `boot`.

- [x] **Write the test**

`internal/ui/scan_test.go` gains `TestScanHintsNameEveryCode`:
`hintCodes` is the list of the twenty codes, written out, the sixteen rows' eighteen plus `too_many_auth` and `auth_unavailable`;
the test cuts the `hints` object out of `app.js`, from `const hints = {` to the `};` that closes it,
reads every key as the identifier before the `:` at the start of a line,
and compares the two sets exactly:
a code with no key and a key with no code are each a failure naming the name.
The list is held in the test and not read from the spec's Markdown,
the way `exclusionReasons` is held for `TestTargetModelNamesEveryReason` (`internal/ui/targetmodel_test.go:76-86`)
and the three import lists are held for the scans (`internal/ui/scan_test.go:135`, `:157`, `:183-193`):
every scan in this package names what it holds the page to, so a failure names a code and never a parse of a table,
and the spec and the list agree by review, as the reasons vocabulary does.
Because the comparison is exact, a hint the page gains without the list, or the list gains without the page, is red either way.

The red state:

```bash
go test -race -count=1 ./internal/ui/ -run 'TestScanHintsNameEveryCode'
```

It reports the four codes with no key: `version_conflict`, `version_missing`, `too_many_auth`, `auth_unavailable`.
The wrong implementation is a page that shows `too_many_auth` as a bare code to a person who can act on it by waiting.

- [x] **Tighten the browser's `no_targets` assertion**

`test/e2e/scenarios_console_test.go`: the `waitFor` of task 2's third step,
which waits for the Profile panel to hold `no_targets`,
now waits for it to hold "Refresh on the targets list updates the available Pods and the empty state",
the hint's guidance and not the code alone;
against today's hint the panel holds "no Ready Pod declares the selected port",
so the tightened wait is red until this task lands,
and it runs with the suite in *Validation*.

- [x] **Add the rows and say so**

`docs/specs/ui.md` *Errors*, in the table (`:1046-1063`):
a `too_many_auth` row and an `auth_unavailable` row with the two hints above,
and the `realm_denied` row reads "the identity panel shows what it does", the word the page's header uses (`internal/ui/static/app.js:962`);
one sentence after the table: "A `403 realm_denied` on a listing refetches `/v1/whoami`, so the panel that hint names shows the realm that refused."
*What is not proven* (`:1660`) gains "and the refetch of `/v1/whoami` a listing's `403` causes".
One amendment row names *Errors* and *What is not proven*.

`CHANGELOG.md`, `### Fixed`:
**The console explains every code its design lists, and its `realm_denied` hint is true.**
`too_many_auth` and `auth_unavailable`, which every route can answer, and `version_conflict` and `version_missing` were shown as bare codes,
the `no_targets` and `port_not_allowed` hints described a rule older than the one the gateway applies,
and the `realm_denied` hint named an identity panel that a listing's `403` never refreshed.
All four codes now carry a one-line hint, the two texts match the gateway's rule, and a `403 realm_denied` on any listing refetches `/v1/whoami`.

- [x] **Validate and commit**

```bash
go vet -tags e2e ./test/e2e/
semlf check internal/ui/static/app.js test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/app.js internal/ui/scan_test.go test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
git commit -F <file holding: "fix(ui): hint every listed code, refetch on 403" and a body saying which codes had no hint, which two texts were stale, what the realm_denied hint promised, and what a listing's 403 now does>
git log --oneline -1 && git status --short
```

---

## 9. The page has a heading and points at the verb

Closes the roadmap bullet *The page has no heading and no pointer to the CLI verb that does the same job* (`docs/plans/roadmap.md:304`).

**Files:**
- Modify: `internal/ui/static/index.html`, `internal/ui/static/app.js`, `internal/ui/ui_test.go`, `internal/ui/scan_test.go`, `CHANGELOG.md`

**The decision, and why.**
*Decisions* settles where each lands.

`index.html` gains `<header><h1>Profgate</h1></header>` before `<main id="app"></main>` (`internal/ui/static/index.html:10-12`).
`renderRequest` gains, after the copy note (`internal/ui/static/app.js:1150`) and only while `url` is built:
`<p><small>From a terminal: <code>profgate profile ${ns}/${svc} ${profile}</code></small></p>`;
the three values are text children.

- [x] **Write the test**

`internal/ui/ui_test.go`: the wanted strings of `TestShellInlineForms` (`:611`) gain `<h1>`.
`internal/ui/scan_test.go` gains `TestScanPageNamesTheVerb`, asserting `app.js` contains the literal `profgate profile`.

The red state:

```bash
go test -race -count=1 ./internal/ui/ -run 'TestShellInlineForms|TestScanPageNamesTheVerb'
```

The shell test reports the missing `<h1>`; the scan reports the missing literal.

- [x] **Add both and say so**

`TestShellNamesItsAssets` (`internal/ui/ui_test.go:341-359`) stays green: the heading names no asset.
`TestAssetTags` and `TestConditional` keep passing over the changed bytes, because they read whatever the tree holds.

`CHANGELOG.md`, `### Added`:
**The console has a heading and names the command that does the same job.**
The page opened on its first panel with nothing naming what it was,
and nothing pointed a reader at `profgate profile`, which fetches the same profile from a terminal.
The shell carries a heading, and the Profile panel names the verb with the selection filled in once a URL is built.

- [x] **Validate and commit**

```bash
semlf check internal/ui/static/app.js CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/index.html internal/ui/static/app.js internal/ui/ui_test.go internal/ui/scan_test.go CHANGELOG.md
git commit -F <file holding: "feat(ui): add a heading and name the verb" and a body saying the page had neither, where each lands, and why the heading is in the shell>
git log --oneline -1 && git status --short
```

---

## 10. The guide describes the page

Closes the roadmap bullet beginning *`docs/console.md:104` says* (`docs/plans/roadmap.md:305-308`)
and clears *Required by this revision and not yet made* (`docs/specs/ui.md:1965-1981`).

**Files:**
- Modify: `docs/console.md`, `docs/specs/ui.md`

**The change.**
`docs/console.md`:

- *What it shows*, the Profile bullet (`:43-57`): "a **Download** link" becomes "a **Download** control";
  after the empty-state sentences: "**Download** is disabled while the Service has no eligible Pod, with those reasons beside it,
  and a **Refresh** control fetches the targets again."
- The selection sentence (`:62-63`), "lands back on the same selection" gains:
  "unless including the selection would make the encoded return path exceed the 1024 bytes the login accepts,
  in which case signing in lands on the console with no selection".
- The `oidc` bullet (`:82`), "signing in returns you to the same selection" gains the same exception in a clause:
  "or to the console with no selection when including it would push the encoded return path past that bound".
- *Downloading a profile and copying its URL* (`:88-91`):
  "**Download** fetches the profile through the same request the gateway runs for `curl` and saves the bytes under the name the response carries;
  a response that declares no `Content-Encoding`, which is every response the pprof handler writes, is saved as the gzip-framed body `curl` receives,
  and a response an intermediary encoded is saved decoded;
  an answer that is not a profile — `no_targets`, `service_not_found`, `realm_denied` — is shown in the panel with its hint,
  and the control is disabled while a download is in flight."
- `:103-105`: "**Copy URL** appears only when the browser exposes its clipboard to the page,
  which it does in a secure context: an `https://` page, or `http://localhost` such as the port-forward above.
  A plain `http://` host under `disabled`, or under `basic` with plaintext permitted, shows the URL for copying by hand."
- *Collections* (`:109-131`):
  after the first sentence: "The table is the newest hundred;
  when older Collections exist a line under it says so and names `profgate collections <ns>/<svc>`, which lists them all.
  A **Refresh** control fetches the list again, and it is how a Collection started here is watched: the page polls nothing."
  `:120-123` become: "Each control takes two presses: the first arms it and offers **Keep**,
  the second — **Confirm start** or **Confirm cancel** — sends the request;
  a second press inside half a second of the first is ignored, so a double-click sends nothing.
  An armed control that is neither confirmed nor kept disarms itself after ten seconds.
  **Keep** disarms a control that has sent nothing;
  after a start whose answer never arrived, **Keep** abandons the attempt and says a Collection may already exist."
- *During a rolling update* (`:139-142`): "the release that moves the console off its old content-hashed asset URLs"
  becomes "the `v0.4.0` to `v0.5.0` upgrade, which moved the console off its content-hashed asset URLs".
- *What the console never does* (`:149-154`):
  "It downloads the same bytes `curl` would" becomes
  "It downloads the bytes `curl` would, decoded where an intermediary added a `Content-Encoding`",
  and the storage bullet gains "a download holds the profile in memory until the save has begun, and nothing after".

`docs/specs/ui.md` *Required by this revision and not yet made* (`:1965-1981`) returns to "Nothing." with the sentence that followed it before this revision:
every edit this document requires elsewhere has been made, the console guide's account of the four controls included;
the table is removed.
One amendment row names the section.

- [ ] **Name the check**

This task has no red test: prose has none, and a check that the old sentences were false is a reading of the page against them,
which the tasks above have made.
What verifies it is `mise run check` for the spec's `Status:` and links, `mise run prose` for the wording,
and a reviewer reading each changed sentence beside the control it describes.

- [ ] **Validate and commit**

```bash
semlf check docs/console.md docs/specs/ui.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add docs/console.md docs/specs/ui.md
git commit -F <file holding: "docs(console): describe the repaired controls" and a body naming the statements that were false and the controls the guide now describes>
git log --oneline -1 && git status --short
```

---

## 11. Close the plan

**Files:**
- Modify: `docs/plans/console-safety.md`, `docs/plans/roadmap.md`

Line 3 becomes `**Status:** Done` and line 4 `**Outcome:** pull request #<n> ...`,
naming the pull request that carries the ten tasks above,
and in the same commit the roadmap item's ten checkboxes are ticked
and its `Shipped:` line (`docs/plans/roadmap.md:315`) names that pull request,
the shape the previous plan's closing commit gave it.
The pull request is named rather than a commit because the merge rebases this branch onto `main`
and rewrites every hash on it, while the number is the same before and after;
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) admits a pull request there for that reason,
and `check_status` in [`check-repo.py`](../../scripts/check-repo.py) requires `**Outcome:** ` followed by text on line 4.
This commit does not delete the plan.
The deletion is the next commit that touches the file, after the merge,
the protocol [`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records;
it deletes this file and rewrites every link that cited it, which `check_links` enforces, and changes nothing else.
`grep -rn console-safety --include='*.md' .` finds the links.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/console-safety.md docs/plans/roadmap.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add docs/plans/console-safety.md docs/plans/roadmap.md
git commit -F <file holding: "docs: close the console safety plan" and a body saying the item's ten bullets are done and its Shipped line names the pull request>
git log --oneline -1 && git status --short
```

---
## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite, on a machine with a Chromium installed:

```bash
mise run test:e2e
```

It is required.
[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
lists `internal/ui` and `internal/client` among the eight packages that need the suite on the `current` lane before a pull request,
and says a change to `internal/ui/static/` needs the browser scenarios, which skip by name on a machine with no browser.
What the suite proves here:
a double-click sends nothing;
a download is a `Fetch` to the profile route followed by a save from a `blob:` URL, gzip-framed, suggested under the profile's name;
**Refresh** sends one request, and once its answer is applied an armed row is still standing;
a Service with no Pod disables the control with its sentence beside it;
a download after the Pods left shows `no_targets` with its **Refresh** guidance and begins no download;
and a long selection sends the bare return path.
Each of those steps ran red once in the task that wrote it, with three exceptions:
the double-click, whose red was the model test;
the disabled control and the `no_targets` panel,
which task 2's red run never reached, because the download's assertion fails the scenario first;
and that panel's **Refresh** guidance, which task 8 tightened and this run is the first to exercise.
So one assertion is shown load-bearing here before the run is trusted:
with `confirmDelayMs` set to `0` for one run, the double-click step of task 1 fails on **Confirm start** being absent,
which is the defect the plan exists to fix, observed once;
the constant is restored and the suite run again, `-count=1`.
The suite proves nothing about the two counters' independence or the Pod and version reconciliation;
those are the browser check's.

The browser check of rule 500, once after the last change to `internal/ui/static/`, follows the recipe that worked before:
keep the kind cluster with `PROFGATE_E2E_KEEP=1`,
turn the console on in the in-cluster ConfigMap and restart the Deployment,
apply the test app into a fresh namespace, and port-forward the gateway.
Then, in Chrome at a 1600 px window:
the heading reads Profgate;
the Collections table shows every column without a horizontal scroll;
an armed **Cancel** shows **Keep** in grey beside a red **Confirm cancel**;
a double-click on **Start collection** leaves **Confirm start** standing;
**Refresh** on the targets list while a Collection fetch is in flight leaves the Collections table's answer in place;
a chosen Pod stays chosen through a **Refresh** that still lists it;
and the Profile panel names `profgate profile <ns>/<svc> <profile>`.
`kind delete cluster --name profgate-current` afterwards.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **Chromium's download events for an object URL are expected and not verified by reading.**
  `awaitDownload` waits on `EventDownloadProgress` (`test/e2e/browser_test.go:436-444`),
  and the download's URL and suggested name are read from `EventDownloadWillBegin`;
  both are reported by the browser's download manager, which an `<a download>` on a `blob:` URL goes through under `Browser.setDownloadBehavior`.
  If the first run of the suite shows neither event for the save, the fallback is the download directory:
  poll it for a file that is not `.crdownload`, which `awaitDownload` already lists (`:445-462`), and read the bytes.
  That fallback proves the bytes and nothing about the suggested name or the download's URL,
  and the step then says so and drops those two assertions rather than reading the disk name as the suggestion;
  the `Fetch` resource type on the profile request stands either way, and is what proves the download was not a navigation.
- **A physical double-click is emulated, not performed.**
  The step dispatches two `click()` calls fifty milliseconds apart; a real double-click also fires `dblclick`, which the page ignores.
  `chromedp.DoubleClick` was rejected because it is one press with a click count of two, which yields one `click` event.
- **The window is half a second, on the page's clock.**
  A person who presses twice in under half a second on purpose has the second press ignored and presses again;
  a platform whose double-click interval is set above half a second can still confirm in one gesture, and the plan accepts that.
  The e2e's deliberate confirmations wait 600 milliseconds, a timing dependency bounded by a constant the page defines.
- **A download holds the body in memory.**
  *Flow* names the cost (`docs/specs/ui.md:538-541`); a `trace` at the configured limit is the download that pays most, and nothing here changes the limit.
- **Revoking the object URL right after the click is Chromium-proven only.**
  Firefox has needed the revocation deferred in some versions; no Firefox is driven (`:1651-1654`), and the plan does not add a timer.
- **The `realm_denied` refetch is not exercised by any test.**
  A realm change mid-scenario needs a gateway reconfiguration the suite does not perform;
  it is stated under *What is not proven*.
- **The two counters' independence is checked by hand.**
  A race between a targets refresh and a Collections answer needs two fetches in flight at once,
  which the browser check produces by pressing and no test times.
- **The empty targets wait depends on the informer excluding terminating Pods.**
  The preStop sleep keeps the Pods `Terminating` for thirty seconds, during which the listing excludes them as `pod_terminating`,
  so the empty answer arrives well inside `settleDeadline`;
  the wait is on the scenario's gateway's answer, under the scenario's credential, and not on the Pods' deletion.
- **The second Service is created through the clientset, not a manifest.**
  A file under `test/e2e/testapp/` would be a second fixture for one selector, and `h.kubectl` has no stdin for an inline one;
  `h.Client` creates the object the way it creates the ConfigMaps and Secrets the harness builds in Go.
  It is created before the page loads the Service list it keeps, minutes before, so the informer has delivered it by then;
  if a run shows it absent from the list, `chooseOption` names the options offered, and the repair is a wait on the scenario's gateway listing it.
- **Two lists of the same twenty codes agree by review.**
  The hint scan holds its code list in the test and the spec holds its table;
  a row added to one and not the other is caught by a reader, not by a test, as the reasons vocabulary already is.
- **`collections` under `--output json` writes several documents to stdout.**
  `jq` reads a stream of documents by default; `jq -s` collects them; the spec says one document per page (`docs/specs/cli.md:964-965`).
- **A `401` mid-walk exits 3, as every `401` does.**
  *Collections* said a failing page exits 1 unconditionally until this plan's revision qualified it to the general rule;
  the verb has never walked before this change, so no script has read the unqualified sentence against a walk.
- **The `heap` filename corrects the spec's `profile`.**
  Go's pprof handler writes the profile's own name for every profile except the CPU one (`pprof.go:157`, `:271`);
  the scenario chooses `heap`, and the assertion follows the code.
- **The plan's deletion is not one of its tasks.**
  The closing task leaves the finished document in the tree under the lifecycle checks;
  the commit that deletes it and rewrites its links follows the merge, as the previous plan's did.

---

## Self-Review

- Bullet coverage, one line each:
  the double-click (task 1, browser step in the same task);
  the disabled download and the shown error (task 2, browser steps in the same task);
  the refresh control (task 3, browser steps in the same task), the verb's walk (task 4), and the paging line (task 5);
  the Collections table's column and `.secondary` (task 6);
  the return-path bound (task 7, browser step in the same task);
  the hints and the `realm_denied` refetch (task 8, tightening task 2's `no_targets` step);
  the heading and the pointer (task 9);
  the statements in the guide (task 10).
- Where the spec's text did not match the code, and what this plan says instead:
  *End to end* names the saved file `profile` (`docs/specs/ui.md:1737`) while the pprof handler names a `heap` download `heap`
  (`$(go env GOROOT)/src/net/http/pprof/pprof.go:271`), so task 2 asserts `heap` and corrects the line;
  the roadmap says the code's hints table lacks two codes (`docs/plans/roadmap.md:302`);
  it lacks four against the spec's table, which has `version_conflict` and `version_missing` (`docs/specs/ui.md:1063`) and the code does not,
  and the spec's table lacks the two authentication codes as well, so task 8 adds four keys to the code and two rows to the spec;
  the code's `no_targets` and `port_not_allowed` hints (`internal/ui/static/app.js:60-61`) do not read as the table's rows (`docs/specs/ui.md:1051-1052`),
  so task 8 replaces both;
  the spec's `realm_denied` hint says "the whoami panel" and the page's panel is headed "Identity" (`internal/ui/static/app.js:962`),
  so task 8 aligns the row to the page;
  `cli.md` *Collections* said a page that fails mid-walk exits 1 while `exitCode` exits 3 on a `401` (`cmd/profgate/exit.go:29-30`)
  and *Output and exit codes* says so (`docs/specs/cli.md:1054`),
  so the revision that produced this plan qualified the sentence, the row, and the bullet (`:961-963`, `:1157`, `:1341-1343`)
  and task 4 asserts both codes;
  *End to end* has no double-click step and no long-selection step, so tasks 1 and 7 add them with an amendment row each;
  the roadmap cites `docs/specs/ui.md:661-663` and `:482` for the two presses and the bound, which are `:781-784` and `:506-508` today,
  and `app.js:1145` for the anchor, which is `:1146`.
- Current-source facts this plan rests on, each confirmed by reading the file:
  `hints` is `internal/ui/static/app.js:56-74`, `fetchJSON` is `:89-138` with its `catch` at `:133-137`,
  `request` is `:386-421` with its tail at `:399-420`, `clearError` is `:423-428`,
  `loadTargets` is `:472-493` stamping `seq` at `:474` and comparing at `:484`,
  `maybeLoadCollections` is `:498-504`, `loadCollections` is `:506-522` reading `seq` at `:511` and comparing at `:513`,
  `reloadWhoami` is `:554-560`, `startEvent` is `:600-618`, `onStart` is `:623-634`, `onCancel` is `:672-682`,
  `afterServiceError` is `:721-726`, `onNamespace` raises `seq` at `:735` and `onService` at `:764`,
  `refetchTargets` is `:805-809`, `currentProfileURL` is `:902-913`, the panels render at `:931-938`,
  the Identity header is `:962`, `renderRequest` is `:1071-1157` with the anchor at `:1146` and the copy note at `:1150`,
  `renderStart`'s armed branch is `:1204-1213`, `renderCollections` opens at `:1221`, `renderCancel`'s armed branch is `:1294-1297`;
  `collectionmodel.js`'s constants are `internal/ui/static/collectionmodel.js:13-41`, `nextState` is `:259-268`, `startNext` is `:289-360`,
  `startOutcome`'s `realm_denied` arm is `:205-207`, and the export statement is `:377`;
  `targetmodel.js`'s `targetSummary` is `internal/ui/static/targetmodel.js:58-93` and its export is `:95`;
  `loginURL` is `internal/ui/static/urls.js:100-106`;
  `.panels` is `internal/ui/static/app.css:9-14`, `.panels article` is `:16-18`, `button.armed` is `:78-82`;
  the shell's body is `internal/ui/static/index.html:10-12`;
  `pico.classless.min.css` holds no `.secondary` selector and defines the ten `--pico-secondary-*` variables;
  `cutExport` and `loadModel` are `internal/ui/portmodel_test.go:30-60`, `callModel` is `:78-104`;
  `collectionModelFunctions` is `internal/ui/collectionmodel_test.go:17-28`, `startState` is `:588-594`, `armedState` is `:632-634`,
  `TestCollectionModelStartNext` is `:640-707` with its events at `:641-657` and its named table at `:672-691`,
  `TestCollectionModelKeySurvivesLostAnswers` is `:713-765`;
  `targetModelFunctions` is `internal/ui/targetmodel_test.go:16`, `TestTargetModelNamesEveryReason` is `:76-86`,
  `TestTargetModelSummaryWithTargets` is `:231-253`, `TestTargetModelSummaryEmpty` is `:255-328`;
  the scans are `internal/ui/scan_test.go:75-110`, the three import lists are `:129-143`, `:151-165`, `:177-202`,
  `TestScanCoversEveryJSFile` is `:214-244`;
  `TestShellInlineForms` is `internal/ui/ui_test.go:595-616` with its wanted strings at `:611`;
  `specPermissions` reads a spec table at `deploy/deploy_test.go:668-700`;
  `env.read` is `cmd/profgate/read.go:27-50`, `collectionsVerb` is `cmd/profgate/collect.go:270-299`,
  `exitCode` is `cmd/profgate/exit.go:23-34` and `fail` is `:53-60`,
  `writeTable` is `cmd/profgate/render.go:15-36`;
  `recordingTransport` and `runRead` are `cmd/profgate/read_test.go:11-33`, `retryTransport` is `cmd/profgate/collect_test.go:195-213`,
  the three `collections` tests are `:497-563`;
  `Request` is `internal/client/client.go:55-61`, `build` encodes `Query` at `:188`, `JSON` is `:155-178`,
  `APIError` carries `Body` at `internal/client/errors.go:18-23`, `CollectionsResponse` is `internal/client/wire.go:86-89`;
  `collectionsBody.NextCursor` is `internal/httpapi/pgo_collections.go:395` and set at `:432-436`;
  `CodeNoTargets` is answered at `internal/httpapi/profile.go:393-399`; `Content-Disposition` passes through at `internal/proxy/proxy.go:52`;
  `sentRequest` is `test/e2e/browser_test.go:151-158`, `session` is `:187-199`, `newSession` enables downloads at `:256-257`,
  `observe` is `:269-325` and records a request at `:283`, `awaitDownload` is `:436-466`, `sentTo` is `:412-423`;
  `h.kubectl` is `test/e2e/harness_test.go:1103-1105` over `run` at `:1109-1121`, `applyConfigMap` is `:1569-1579`;
  a Service with no selector is skipped at `internal/k8s/catalog.go:38-42` and refused at `internal/k8s/eligibility.go:92-94`;
  the console scenario's download is `test/e2e/scenarios_console_test.go:194-195`, its start pair `:208-209`, its cancel pair `:221-222`,
  `assertNavigatedToLogin` is `:452-475`, `assertStartRequest` is `:494-507`, `control` is `:560-562`, `rowCell` is `:583-587`,
  `awaitRequest` is `:600-608`, `awaitRow` is `:614-634`, `refetchCollections` is `:670-682`, `chooseOption` is `:694-722`;
  `deployTestAppScaled`'s merge patch is `test/e2e/scenarios_pgo_test.go:356-363`,
  `waitTargets` is `test/e2e/scenarios_test.go:324-349` and polls `h.Gateways` at `:331`,
  `tryTargetNames` is `:205-232`, `settleDeadline` is `:54`, the preStop sleep is `test/e2e/testapp/deployment.yaml:38-41`;
  `try` is `test/e2e/scenarios_auth_test.go:336-357`, `awaitPGORoutes` is `test/e2e/scenarios_console_test.go:368-384`, `sent` is `:796-807`;
  `chromedp.Click` and `DoubleClick` are `query.go:1054-1074` and `ClickCount` is `input.go:147-151` of chromedp v0.16.0;
  `EventDownloadWillBegin` and `EventDownloadProgress` are `browser/events.go:12-29` of the pinned cdproto,
  `SuggestedFilename` is documented at `:16` as a name the disk may differ from,
  and `EventRequestWillBeSent.Type` is `network/events.go:73`;
  the pprof handler's `Content-Disposition` is `pprof.go:157`, `:183`, `:271`;
  the guide's lines are `docs/console.md:43-57`, `:88-91`, `:103-105`, `:109-131`, `:139-142`;
  the roadmap's `Shipped:` line is `docs/plans/roadmap.md:315`;
  every commit header above is under 50 characters.
- Decided here, with the reason stated where it is carried:
  eleven tasks, each carrying the browser step that proves it;
  the window in the model rather than a placement, half a second, both controls;
  two clicks from one evaluation rather than `chromedp.DoubleClick`;
  `asBlob` on `fetchJSON` for a `200` alone, `settle` split out of `request` and returning its record,
  the anchor created and clicked, the filename helper, the Service refetch on a download's `service_not_found`, `urls.js` unchanged;
  `downloadNote` as the fourth `targetmodel.js` function, in the task that wires the fetch;
  two generations and two flags each owned by its generation, a stale answer dropped before it records, reconciliation in `app.js`;
  `olderCollectionsNote` with a fixed text, cleared with the rows;
  a `button.secondary` rule from Pico's variables rather than another Pico build, and the panel spanning the grid;
  the bound measured in `loginURL` and proven by a second session;
  four hint keys, two rows in the spec's table, two texts replaced, an exact-set scan over a list held in the test, and the `403` refetch;
  the heading in the shell and the pointer in the panel;
  the walk in `collectionsVerb`'s own `run` with pages buffered until the walk completes, exiting by the general rule, before the console names it;
  `awaitRow` through **Refresh**, `requestCount` and `awaitRequestSince` over the recorded requests, the answer's application awaited through the control,
  the download proven by its resource type and its `blob:` URL with the suggested name kept apart from the disk name,
  the second Service through the clientset before the working load, the scale to zero after the Pod menu is populated and awaited through the scenario's own client;
  spec edits in the task that makes them true;
  the plan closed naming the pull request in both `Outcome:` and the roadmap's `Shipped:` line, and deleted by the commit after the merge.
- Left to the implementer:
  the exact shapes of `pagedTransport`, `pressTwiceInsideTheWindow`, `awaitRequestSince`, `awaitTargetsEmpty`, `refreshButton`, and `nobodyService`;
  the regular expression the hints scan cuts the object and its keys with;
  the exact wording of the amendment rows;
  and the wording of every commit body.
