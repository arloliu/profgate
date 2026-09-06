# One Console Panel to a Row, and the Identity a Disclosure

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> On this machine `mise run lint` runs a golangci-lint 2.1.6 that shadows the pinned 2.12.2,
> so every validation block below runs the linter as `mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`
> and never as `mise run lint`.

**Goal:** give the console's page to the controls on it.
`.panels` is a grid of `repeat(auto-fit, minmax(20rem, 1fr))` columns with every panel aligned to its top
(`internal/ui/static/app.css:9-14`),
so at a wide window the identity, Service, and Profile panels stand side by side.
The Profile panel draws its profile, port, Pod, and version controls one to a line,
with the URL field and the download actions under them (`internal/ui/static/app.js:1322-1373`, `:1378-1389`),
while the two beside it hold seven facts and two selects (`:1165-1201`, `:1204-1231`);
below those two the page is a column of nothing as wide as they are and as tall as the difference.
The identity is an `<article>` (`:1170-1200`) rendered into that grid (`:1141-1142`),
seven facts and a sign-out link, none of them read on the way to a profile.
The `realm_denied` hint names "the identity panel" (`:62`).
After this plan the page is an identity disclosure and then panels, one panel to a row,
each panel laying its controls across its row with every control keeping its own label;
the identity is a `<details>` closed on load whose summary names the principal and the realm,
holding the same seven facts and the same sign-out link one click away;
a `403` whose envelope names `realm_denied`, on a listing, on a profile download, or on the current start attempt,
opens it, each path first discarding an answer it has moved past,
for which the Service listing gains the generation counter the other two listings already carry,
so the hint that names the identity stays true, and the hint drops the word panel;
and the console guide describes the page as it is.
No endpoint, configuration key, chart value, Kubernetes call, or NATS permission is added,
and every request the page sends after this plan is one it sent before.

**Architecture:** `internal/ui/static/app.js` renders the identity as a `<details class="identity">` above `.panels`,
moves `panelError("whoami")` out of it to sit between the disclosure and the panels,
gives the Service and Profile panels class names,
wraps each panel's field controls in a `<div class="fields">`,
and the Profile panel's explanatory lines in a `<div class="notes">`,
stamps the Service listing with a generation of its own,
holds a `ref` to the disclosure and sets its `open` once per qualifying answer,
and drops the word panel from the `realm_denied` hint;
`internal/ui/static/app.css` gains the disclosure's rules, the one-panel-to-a-row grid,
the flex field row, the port control's own row of fields, and the two-pairs-per-row identity list,
and loses the auto-fit column grid;
`internal/ui/scan_test.go` holds `app.js` to opening the disclosure from exactly two places
and to opening it from neither of the two the outcome refetches run through;
`test/e2e/browser_test.go` gains a helper that answers a paused request with a response the step wrote,
beside the one that holds a request unanswered,
and routes paused requests through an ordered list of handlers so the two compose;
`test/e2e/scenarios_console_test.go` reads the identity at the disclosure and its summary,
checks the escaped query values in the Service panel,
scopes the Collection detail's selectors to the Collections panel,
and drives the six cases the disclosure's rule has to tell apart;
`docs/specs/ui.md` records that coverage, gains the scan's case,
and clears the one edit it says this revision owes;
`docs/console.md` describes the page as it is.
No route, model module, or vendored file moves.

**Spec:** the design is accepted text in [`ui.md`](../specs/ui.md):
*Controls* **The arrangement** for one panel to a row and how each panel lays its controls across it,
the extra field the port control's menu can open included (`docs/specs/ui.md:752-773`),
**The identity** for the disclosure, its summary, its seven facts,
and where a failed refetch's recovery renders (`:775-788`);
*Starting and cancelling a Collection* for the start's `403 realm_denied` opening it (`:953`)
and the cancel's `404 collection_not_found` not opening it (`:965`);
*Signing in and out* for **Sign out** and the `basic` note at the disclosure's foot (`:1062`, `:1077`);
*Errors* for the hint that drops the word panel (`:1095`)
and **When the identity disclosure opens** for the rule, the predicate each path applies,
the generation the Service listing gains, the status the opening reads beside the code,
`open` as the element's own state, and the six cases (`:1114-1160`);
*Layout and embedding* for the stylesheet's description (`:1190`);
*What is not proven* for the arrangement, the identity refetch, and the disclosure's state (`:1759-1762`);
*End to end* for the identity read at the disclosure and its summary (`:1838`, `:1894`, `:1916`)
and **Where the scenarios read the identity** for the four reads that move (`:1920-1934`);
*Required by this revision and not yet made* for the one edit owed (`:2094-2110`);
and the amendment row that lists them (`:2148`).
The roadmap item is *Lay the console out one panel to a row, and fold the identity into a disclosure*
(`docs/plans/roadmap.md:355-392`).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the page, not as the defects that revealed them.

- **The page gives each panel a row and lays that panel's controls across it.**
  `.panels` is `display: grid` over `repeat(auto-fit, minmax(20rem, 1fr))` with `align-items: start`
  (`internal/ui/static/app.css:9-14`),
  and Pico gives every block control a line and a bottom margin of its own,
  so a panel wide enough for four controls draws them in a column of four.
- **The identity is one click away and discloses nothing new.**
  `renderIdentity` holds seven term and value pairs — principal, realm, namespaces, Services, profiles,
  the three `pgo` flags, and the authentication mode (`internal/ui/static/app.js:1172-1192`) —
  and a footer holding **Sign out** or the note `basic` shows in its place (`:1194-1199`).
  Each value the summary names is one of the seven.
- **A failed identity refetch's recovery renders outside the collapsible body.**
  `panelError("whoami")` sits inside the identity article today (`:1193`).
  It returns a `SignInRequired` or an `ErrorBox`, each of which renders its own `div.error`
  (`:1152-1163`, `:280-291`, `:297-304`),
  and `app.css:70-81` styles `.error` without reference to `.panels`,
  so the node stands wherever the template puts it.
- **The disclosure opens where the answer is classified, and never in the function that runs an outcome's refetches.**
  `refetch(what)` (`:697-711`) is called by `sendStart` for a start's `403 realm_denied` (`:797-798`,
  `internal/ui/static/collectionmodel.js:211-212`)
  and by `sendCancel` for a cancel's `404 collection_not_found` (`:849-850`,
  `internal/ui/static/collectionmodel.js:252-256`),
  and the design opens the disclosure for the first and not for the second
  (`docs/specs/ui.md:953`, `:965`, `:1142-1144`).
  An opening hooked in `refetch` would open on a cancel that names no realm that refused,
  and no test and no browser check reaches that path.
- **Each path discards an answer it has moved past before that answer is recorded.**
  `request` returns at `:485-487` when its `stale` predicate holds, before it reaches `settle` at `:492`,
  so a discarded listing answer records nothing and can open nothing.
  Two of the three listings a realm can refuse pass such a predicate:
  `loadTargets` stamps `targetsSeq` at `:586` and passes it at `:596`,
  `loadCollections` stamps `collectionsSeq` at `:634` and passes its own at `:641`,
  and the two counters are declared apart at `:324-328`.
  The third, the Service listing, passes none.
  `loadServices` calls `request` with no `stale` argument (`:565`)
  and compares the namespace it asked for at `:566`, which is after `request` has already called `settle`,
  so a denial for a namespace the page has left is recorded as if it were current,
  and an opening hung on that arm would open the disclosure for it
  (`docs/specs/ui.md:1124-1130`).
  It gains a counter of its own rather than an earlier comparison:
  a namespace chosen, left, and chosen again leaves `this.state.ns` where a namespace never left leaves it,
  so no comparison of the namespace can tell those two apart.
  A download's answer is always current, because `downloadAllowed` (`:1010-1012`) reads `downloading`,
  `onDownload` sets it at `:1025` before the fetch and the control is disabled from it at `:1383`,
  so a second download is never in flight.
  A start's answer is the current attempt's exactly when `startEvent` reports the step moved (`:794`).
- **The identity's own refusal asks for no second fetch, and opens nothing.**
  `settle` refetches on `realm_denied` only when `key !== "whoami"` (`:525-527`),
  which is what stops a refused refetch from starting another.
  The opening belongs inside that same condition and not above it:
  the design names three paths that open the disclosure and the identity refetch is none of them
  (`docs/specs/ui.md:1115-1116`, `:1145`),
  and what a refused refetch shows renders outside the collapsible body instead (`:1146-1147`).
- **The opening reads the status as well as the code.**
  The design's predicate is a `403` carrying an envelope whose `code` is `realm_denied`,
  for a request that is not the identity refetch (`docs/specs/ui.md:1134-1135`).
  `settle`'s arm reads the envelope and the key and not the status (`:525`),
  so an answer of some other status carrying that code reaches the same arm;
  `sendStart` has both to hand, because `answerOf` (`:160-171`) carries the status and the code together.
- **Every hint is true.**
  `hints.realm_denied` reads "your realm does not admit this; the identity panel shows what it does" (`:62`),
  and the design's table now reads "the identity shows what it does" (`docs/specs/ui.md:1095`).
- **The browser scenarios read the identity where the page renders it.**
  `console-oidc` reads its text out of `.panels` at `test/e2e/scenarios_console_test.go:173-178`
  and `console-basic` at `:416-421`;
  `assertRenderedAsText` (`:800-821`) reads `.panels` markup at `:808`,
  and takes its payloads as one variadic list:
  `:193` passes the hostile query and the hostile principal together,
  and `:377` passes the principal alone, because the working load carries no hostile selection.
  A read of the whole disclosure passes whether or not its summary names the principal and the realm,
  because both stay in the body a closed `<details>` still carries,
  so the summary is read at `details.identity > summary` and not at the disclosure
  (`docs/specs/ui.md:1838`).
  The Collection detail is selected as a bare `details` or `details summary` at `:231`, `:232`, and `:977`,
  and the identity becomes the document's first `<details>`.

---

## Decisions

Twelve choices settle how the design's text is carried, and the facts of the code that shape each check.

**Four tasks: the arrangement, the opening rule, the guide, and the close.**
The arrangement is markup and stylesheet, the four browser reads that markup moves,
and the two of the disclosure's six cases the markup alone decides;
the opening rule is behavior over that markup, so it follows it rather than riding with it,
and it carries the other four cases.
The browser reads ride with the task whose change makes them red,
because a suite left failing between two commits is a suite nobody can bisect through.
The guide is last before the close, after every behavior it describes exists,
and the closing task flips `Status:` and writes the roadmap's line.
No task is split further:
the markup and the stylesheet are one change with one check and no test between them,
and the hint's wording is one line of the task whose behavior makes it true.

**The prototype's markup and stylesheet are taken as they are,
and every new rule is held to what it may reach.**
A working page laid out against the design is worth more than a second guess at the same rules,
and the arrangement is the one thing on this page no test asserts,
so a rewrite of it would be a rewrite nothing catches.
Three panels gain class names, `identity` becoming a `<details>` rendered above `.panels` rather than inside it;
each panel's field controls are wrapped in a `<div class="fields">`;
the Profile panel's explanatory lines are wrapped in a `<div class="notes">`;
`panelError("whoami")` moves out of the identity and renders between the disclosure and `.panels`;
and `app.css` gains the disclosure's rules, the one-panel-to-a-row grid, the flex field row,
and the two-pairs-per-row identity list, and loses the auto-fit column grid.

**A rule that names `.panels` and no class reaches the Collection detail too.**
That detail renders a `<details open>` and a `<dl>` of its own inside the Collections panel
(`internal/ui/static/app.js:1560-1562`), so each new rule is written against it:
the two-pairs-per-row list is `details.identity dl`,
which leaves the Collection detail the single column of pairs `.panels dl` gives it
(`internal/ui/static/app.css:42-47`);
`.panels dt, details.identity dt` leaves its terms the weight they have (`:49-51`);
and `.panels dd` keeps `overflow-wrap: anywhere` (`:53-56`) while `details.identity dd` takes its own declaration,
because the Collection detail's list holds four 30-character timestamps in that grid's `1fr` track (`app.js:1570-1575`),
and `break-word` and `anywhere` do not give that track the same minimum width.
The Collection detail sits inside no `.fields` and no `.notes` and holds no `<label>`,
so the field row's rules, the notes' rules, and `.panels label` reach it nowhere.
`.panels article` and `.panels article header` reach the Collections panel itself, which is what they are for.
A `.panels article footer` rule would reach nothing at all once the identity moves out of `.panels`,
its `<footer>` being the only one the page renders (`app.js:1194`),
so the disclosure's foot is styled as `details.identity > footer` and no panel footer rule is added.

**The port control's own fields are laid across the row with the controls beside them.**
`renderPortControl` is a `<fieldset>` holding a legend, the port `<select>`,
and a **Port number** or **Port name** label when `allowedSelections` names a wildcard
(`internal/ui/static/app.js:1233-1278`),
and the design puts that extra field on the row rather than under it (`docs/specs/ui.md:757-758`).
`.fields` lays out its immediate children and the fieldset is one of them,
so the fieldset lays its own contents across in turn and takes a share of the row that grows with what it holds.
The rule is stated in the task, and the browser check looks at a number wildcard, at a name wildcard,
and at a configured seconds limit, which are the three ways that row gains a control.

**`open` is set on the element and never bound in the template.**
*Errors* settles this (`docs/specs/ui.md:1149-1152`):
a native close changes the element's `open` without the template knowing,
and a template that re-asserts the value it asserted last render need not reopen what a person closed,
so a bound `open` is not a reliable way to open the disclosure at all.
The page imports `createRef` from `./vendor/preact/preact.module.js`,
which the vendored build exports (`internal/ui/static/vendor/preact/preact.module.js`, `M as createRef`),
holds one ref beside the request counters (`internal/ui/static/app.js:324-328`),
passes it to the `<details class="identity">`,
and `openIdentity()` sets `open` on `ref.current` when there is one.
It tolerates an absent node rather than throwing:
`render` answers `booting`, `navigating`, `signInRequired`, and `error` before it renders the identity (`:1124-1142`),
so the guard is one line, and a page that threw on a null ref would take the console down with it.

**The opening is called from `settle` and from `sendStart`, and from nowhere else.**
`settle` (`:503-529`) is the one place a listing's and a download's answer is classified:
`request` calls it at `:492` after dropping every answer its `stale` predicate rejects at `:485-487`,
and `onDownload` calls it at `:1031` for an answer that is current by the control's own guard.
Its `realm_denied` arm at `:525-527` already refetches `/v1/whoami`, and the opening joins it there,
before the refetch rather than after it,
because the design says the opening does not wait (`docs/specs/ui.md:1137-1138`).
The opening's own condition is narrower than the arm's:
the arm reads the envelope's code and the key (`:525`) and the opening reads the status too,
so an answer that is not a `403` refetches the identity, as it does today, and opens nothing.
`sendStart` calls it after `step.moved` (`:794`) and before the refetch loop at `:797-799`,
on that same predicate, which it reads from the `answer` it already built at `:785`.
`refetch` is left alone, for the reason the invariant above states.
Three listings reach `settle` and only two of them discard an answer the page has moved past,
so the Service listing gains its counter in this same task:
without it, an opening added to `settle` opens the disclosure for a namespace the caller has left.

**A source scan holds the call sites to two, and it proves no behavior.**
The scan is:
`app.js` holds exactly two occurrences of `this.openIdentity()`,
and the body of `refetch` holds none.
What it catches is a third call site, which is a path the design did not name,
and the one wrong site a reasonable implementer would pick,
which is `refetch`, the function that serves both the start's denial and the cancel's `404`.
What it does not catch is everything else.
It passes whether or not `openIdentity` does anything,
whether or not its ref is ever attached,
whether the call in `sendStart` sits before or after the return that drops an abandoned attempt,
and whether `settle` calls it for one arm or for every error.
A count of call sites is not a behavior, and this plan claims none for it:
the behavior is the browser's, below.
The scan is kept for the one thing it does hold, and its case in *Unit* says exactly that much.
The body is cut the way the hints scan cuts its object:
`hintsObjectRe` (`internal/ui/scan_test.go:247`) is one lazy match anchored on the declaration
and on a closing brace at the declaration's indentation,
and `hintKeys` (`:259-276`) refuses a body it cannot read as that shape rather than passing on a match of nothing,
which is what `TestScanHintKeysRefuseWhatTheyCannotRead` (`:280-311`) holds it to.
The new scan refuses the same way:
a scan that matched nothing would pass a page that never opened the disclosure at all.

**The six cases are the browser's, split between the two tasks by what makes each red.**
*Errors* lists six cases a test has to tell apart (`docs/specs/ui.md:1154-1160`),
and every one of them is a statement about `details.identity.open` in a rendered page,
which the end-to-end suite is the only thing that executes (`:1705`).
Two of them need the markup alone and ride with it:
the disclosure closed on load, and a person's opening surviving a render.
Four need an answer the page classifies, and ride with the opening rule:
a qualifying denial opening it;
a second identical denial opening it again;
a person's closing surviving both a late refetch answer and an unrelated render;
and a discarded stale answer, or a cancel's `404 collection_not_found`, leaving it as it stood.
The suite produces those answers itself rather than narrowing a realm mid-scenario.
The Fetch domain is already enabled for a step at `test/e2e/browser_test.go:371-404`,
which pauses a request the step names and hands back a release for it,
and `observe` decides what to do with each paused request at `:348-359`;
a second helper beside `holdRequest` answers a paused request with a response the step wrote,
so a listing, a download, and a start each reach their `403 realm_denied` under the test's own control.
The session carries one matcher and one channel today (`:218-219`),
so the two helpers compose through an ordered list of handlers that replaces them,
which the task states in full.
*What is not proven* records the disclosure's state as unasserted today (`docs/specs/ui.md:1760-1762`);
each task rewrites the part of that sentence its own cases make false,
and the arrangement stays on it, because no test asserts a layout.

**The four browser reads move to where the page renders each value,
and the summary is asserted at the summary.**
*End to end* **Where the scenarios read the identity** settles all four (`docs/specs/ui.md:1920-1934`).
The two text reads (`test/e2e/scenarios_console_test.go:173`, `:416`) become reads of `details.identity`,
asserting the same three values;
neither has to open the disclosure, because a closed `<details>` carries its body in `textContent`,
which is what `textOf` reads (`:1112-1120`).
That is also why the disclosure alone is not enough:
a read of the whole element passes with a summary that names neither the principal nor the realm,
both of them being in the body either way,
and *End to end* promises that the summary names them (`docs/specs/ui.md:1838`).
So the principal and the realm are read at `details.identity > summary`,
and the authentication mode, which the summary does not carry, at `details.identity`.
`assertRenderedAsText` (`:800-821`) splits its one markup read at `:808` into two:
the escaped principal at `details.identity`, and the escaped namespace and Service values at `.selection`,
which is where the page renders them and where they stay.
It is two reads and not one wider one:
a read of `document.body` would pass and stop proving which container each value reached.
The `img` count at `:802-806` and the two sentinels at `:814-820` are document-wide already and do not move.
The Collection detail's three selectors (`:231`, `:232`, `:977`) are scoped to `.collections`,
because the identity becomes the document's first `<details>`
and a bare selector would read it rather than the Collection it names.
The unlisted-selection read at `:189` stays in `.panels`, and so do the four `.panels` reads
that only build a failure message (`:283`, `:304`, `:366`, `:891`).

**The design's citations into `app.js` are refreshed by each task that moves them.**
*Errors* cites twelve positions in `app.js` across six of its own lines
(`docs/specs/ui.md:1123`, `:1125`, `:1126`, `:1132`, `:1133`, `:1136`).
The arrangement task's edits to `app.js` begin in `render`, below every one of those positions but one,
so it moves the **Download** control at `:1383` and leaves the rest where they are.
The opening task moves the rest:
it adds an import name, a constructor line, a method, and a call inside two functions,
each of which shifts every position below it, and it rewrites `loadServices` itself.
Each task refreshes the lines its own tree moved, and the second reads the finished tree.
No amendment row is added for either refresh:
the amendment table records the edits that changed what the document claims,
and a citation refresh changes no claim.

**The guide is one edit, and it clears the section that says so.**
`docs/console.md:32` says the page has four parts and names Identity first;
`:35-37` describes it as one of them;
`:76`, `:83`, and `:91` are where the word panel is written, one per authentication mode.
`docs/specs/ui.md:2094-2110` records the guide as the one edit this revision owes,
in prose and not in a table: `:2096-2100` names it,
and `:2101-2110` lists the documents this one is no longer ahead of.
The task that writes the guide replaces the first five of those lines with "Nothing."
and adds the console guide's account of the page to the list that follows.

**The plan closes naming the pull request.**
The merge rebases this branch onto `main` and rewrites every hash on it,
so `Outcome:` and the roadmap's `Shipped:` line (`docs/plans/roadmap.md:389`) both name the pull request,
which is the same number before and after;
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) admits a pull request there for that reason.
The file is deleted by the commit after the merge.

---

## Global Constraints

- **No new endpoint, configuration key, chart value, Kubernetes call, or NATS permission.**
  Every request the page sends after this plan is one it sent before.
- **No new module under `internal/ui/static/`.**
  *Layout and embedding* enumerates the five console modules (`docs/specs/ui.md:1182-1201`),
  and a sixth would be an edit to an accepted package layout and to the tree `internal/ui/ui_test.go` asserts,
  for a predicate that belongs to neither the port control, the targets list, nor the Collection controls.
  The MANIFEST is not part of that count: it covers the vendored tree alone
  (`internal/ui/vendor_test.go:12-14`).
- **Every task that changes behavior shows a red check before its change,
  and says of every check it adds whether that check could have failed.**
  Task 1 writes the identity reads and its two disclosure cases first
  and runs the two console scenarios red against the unchanged page.
  Task 2 writes its scan and its four remaining cases first;
  the scan, the two denial cases, and the delayed Service-listing case run red,
  while the cancel case and the closing-survives-a-late-answer case pass before the change,
  because nothing opens the disclosure at all yet, and exist to stay green across it.
  Tasks 3 and 4 change no behavior and each says what verifies it.
- **A source scan where the page must call something, the browser where the DOM decides,
  and the hand check of rule 500 where no test reaches.**
  `app.js` runs in `console-oidc` and `console-basic` and nowhere else (`docs/specs/ui.md:1705-1706`);
  the scans hold it to what it imports and calls (`internal/ui/scan_test.go:96-108`, `:139-212`).
  A behavior of `app.js` no scan reaches is either a step of a console scenario
  or a line under *What is not proven*, named per task.
- **`-race` and `-count=1` on every red run.**
- **Several cases of one shape are named table cases** with `t.Run(tc.name, ...)`
  ([`300-testing.md`](../../.agents/rules/300-testing.md)).
- **No jargon:** comments, commit messages, and documentation state the current fact,
  never this plan's ordering or a task name.
- Markdown prose uses semantic line breaks, and so do the comments in `app.js`:
  `semlf check internal/ui/static/app.js` reports 23 `wrap` findings on lines this plan does not touch,
  and the `pre-commit` hook's `semlf --base HEAD` reports the findings the changed lines own, whatever the file type.
  `semlf check internal/ui/static/app.css` reports nothing, so the stylesheet's comments draw no finding.
  Run `semlf check` on every Markdown file, every Go file with doc comments, and `app.js` when a task edits it
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
internal/ui/static/app.js                    # the identity as a details above the panels; the field and note wrappers;
                                             # the panel class names; the Service listing's generation; the ref and
                                             # openIdentity; the hint drops the word panel
internal/ui/static/app.css                   # one panel to a row; the disclosure's rules; the flex field row; the
                                             # port control's own fields across it; two pairs to a row in the
                                             # identity list
internal/ui/scan_test.go                     # two calls of openIdentity, and none in refetch
test/e2e/browser_test.go                     # an ordered list of paused-request handlers; answer a paused request
                                             # with a response the step wrote
test/e2e/scenarios_console_test.go           # the identity read at the disclosure and its summary; the escaped query
                                             # values in the Service panel; the Collection detail scoped to the
                                             # Collections panel; the six cases of the disclosure's rule
docs/specs/ui.md                             # the scan's Unit case; the browser coverage in End to end and in What is
                                             # not proven; the refreshed Errors citations; the owed edit cleared
docs/console.md                              # the guide describes the page
docs/plans/roadmap.md                        # the item's guide bullet and its Shipped line, in the closing task
CHANGELOG.md                                 # one entry per behavior
docs/plans/console-layout.md                 # this file
```

---

## 1. One panel to a row, and the identity a disclosure

Closes the roadmap bullets beginning *`.panels` is a grid*, *The identity is an `<article>`*,
and *`test/e2e/scenarios_console_test.go:173,416` read the identity through `.panels`*
(`docs/plans/roadmap.md:357-368`, `:374-381`).

**Files:**
- Modify: `internal/ui/static/app.js`, `internal/ui/static/app.css`,
  `test/e2e/scenarios_console_test.go`, `docs/specs/ui.md`, `CHANGELOG.md`
- `scenarios_console_test.go` is edited in both console scenarios and in one shared helper,
  because `console-oidc` reads the identity at `:173`, `console-basic` at `:416`,
  and `assertRenderedAsText` at `:800-821` serves the first from two calls, `:193` and `:377`.

**The change, and why.**
*Decisions* settles that the prototype's markup and stylesheet are taken as they are,
and what each of the two files gains.

`internal/ui/static/app.js`:

- `render` (`:1140-1147`) takes the identity out of `<div class="panels">`.
  It renders `${this.renderIdentity()}` and then `${this.panelError("whoami")}` above that container,
  so the error a refetch of `/v1/whoami` produces, and the **Retry** or **Sign in again** control it carries,
  render between the disclosure and the panels (`docs/specs/ui.md:785-787`).
- `renderIdentity` (`:1165-1201`) returns a `<details class="identity">` in place of the `<article>`,
  its `<summary>` holding `<strong>Identity</strong>` and a `<span class="who">` naming the principal and the realm,
  the same seven pairs, and the same footer;
  `${this.panelError("whoami")}` (`:1193`) is removed from it, having moved to `render`.
  Nothing new is disclosed: each value in the summary is one of the seven (`docs/specs/ui.md:783`).
  `open` is not bound on `details.identity`, here or anywhere,
  because the opening rule rests on the template never asserting it.
  The Collection detail keeps the `<details open>` it renders today (`:1560`):
  that disclosure is meant to be open when it appears, and nothing here changes it.
- `renderSelection` (`:1204`) gives its article `class="selection"`
  and wraps the namespace and Service labels in one `<div class="fields">`,
  with the two "is not listed" lines and the two panel errors under it rather than between the selects.
- `renderRequest` (`:1280`) gives its article `class="request"`,
  wraps the profile, seconds, port, Pod, version, and **Refresh** controls in one `<div class="fields">`,
  wraps the empty state in a `<div class="empty">` inside it,
  and wraps the four explanatory lines under the download actions in a `<div class="notes">`.

`internal/ui/static/app.css`:

- `.panels` becomes `grid-template-columns: minmax(0, 1fr)` with a `0.75rem` gap,
  losing the auto-fit columns and the `align-items: start` that only mattered beside them (`:9-14`).
- `details.identity` and its summary, list, and footer gain rules,
  including `margin-inline-start: auto` on the summary's chevron
  so a flex summary reads with the caption at one end and the chevron at the other.
- `.fields` is a wrapping flex row whose children take an equal share and stop growing at `15rem`,
  `.panels .fields > .actions` takes the width of its own content so **Refresh** takes the width of its label,
  and `.fields .empty` takes the whole row because it is a sentence and a table rather than a control
  (`docs/specs/ui.md:761-767`).
- `.fields fieldset` is a wrapping flex row of its own, so the port control's `<select>`
  and the **Port number** or **Port name** field a wildcard opens (`internal/ui/static/app.js:1245-1275`)
  sit on the row with the controls beside them rather than stacked in one share of it,
  and the fieldset's share grows with the fields it holds instead of stopping at the cap a single control takes
  (`docs/specs/ui.md:757-758`).
  Its legend keeps the label text's own size and weight.
  Where a `<legend>` sits inside a flex `<fieldset>` is not the same in every engine,
  so the visual check reads the caption's position beside the labels around it
  and not only whether the extra field reached the row.
- The identity's term and value list becomes two pairs to a row above `48rem` and one below it,
  written as `details.identity dl`,
  so the Collection detail keeps the single-column `.panels dl` it has today (`internal/ui/static/app.css:42-47`).
- `.panels dd` keeps `overflow-wrap: anywhere` (`internal/ui/static/app.css:53-56`)
  and `details.identity dd` takes its own declaration,
  because the Collection detail's list holds four 30-character timestamps in that grid's `1fr` track
  (`internal/ui/static/app.js:1570-1575`),
  and `break-word` and `anywhere` do not give that track the same minimum width.
  No panel footer rule is added: the identity's `<footer>` is the only one the page renders (`:1194`),
  and it moves into the disclosure, so `details.identity > footer` is where it is styled.
- The comment on `.panels article.collections` (`internal/ui/static/app.css:36-37`) says the Collections panel
  "takes a row of its own below the three panels rather than one cell beside them";
  under a single-column grid every panel takes a row and `grid-column: 1 / -1` is a no-op,
  so the comment is rewritten to say the rule keeps the panel spanning whatever columns the grid has.
  The rule itself stays: it is the prototype's, and removing it is a change this plan did not need to make.

`docs/specs/ui.md`:

- *Errors* cites twelve positions in `app.js` across six lines
  (`:1123`, `:1125`, `:1126`, `:1132`, `:1133`, `:1136`).
  This task's edits to `app.js` begin in `render`, so the one position it moves is the **Download** control at
  `:1383`, cited on `:1132`; that line is rewritten to the position this task's tree holds and the other five stand.
  No amendment row: the claim is unchanged.
- *What is not proven* says no browser scenario asserts panel placement, control wrapping,
  the identity disclosure's initial state, or its opening and closing (`:1760-1762`).
  This task proves the initial state and a person's opening surviving a render,
  which is both of those last two clauses except for the opening a denial causes,
  so the sentence becomes: none of them asserts panel placement, control wrapping,
  or the disclosure's opening on a denial.
  The next task takes off what it leaves.
- *End to end* `console-oidc` gains the two cases this task adds, beside the summary read it already promises
  (`:1838`).
  One amendment row names *What is not proven* and *End to end*.

- [ ] **Move the browser reads, add the two cases the markup decides, and run the console scenarios red**

`test/e2e/scenarios_console_test.go`:

- `:173-178`, in `scenarioConsoleOIDC`, reads `details.identity` instead of `.panels`
  and asserts the authentication mode, `oidc`, there,
  with a failure message naming the identity disclosure;
  the issuer's user and `developer` are asserted at `details.identity > summary`,
  because a read of the whole disclosure passes whether or not the summary names them
  (`docs/specs/ui.md:1838`).
- `:416-421`, in `scenarioConsoleBasic`, does the same for the Basic user, `developer`, and `basic`.
- `assertRenderedAsText` (`:800-821`) takes the principal and the query payloads apart:
  the escaped principal is read out of `details.identity`'s `innerHTML`
  and the escaped namespace and Service values out of `.selection`'s.
  Its variadic payload list becomes two named parameters, the principal and the selection,
  because each is now read from a container of its own and a list cannot say which is which.
  It has two callers, both in `scenarioConsoleOIDC`:
  `:193` passes the query payload as the selection and the principal payload as the principal,
  and `:377`, the working load, passes the principal payload and an empty selection,
  which the helper reads as no Service-panel assertion to make.
  The `img` count (`:802-806`) and the two sentinel reads (`:814-820`) are unchanged.
- `:231`, `:232`, and `:977` select `.collections details summary`, `.collections details`,
  and `.collections details summary` in place of the bare selectors,
  because the identity becomes the document's first `<details>`.
- `scenarioConsoleOIDC` gains two reads of the disclosure's own state, on the working load:
  `document.querySelector("details.identity").open` is `false` before anything is clicked,
  and, after a click on `details.identity > summary` and a press of **Refresh** on the targets list
  that lands its answer, it is `true`.
  Both read the property and not the attribute, because `open` is the element's state
  and the template never writes the attribute (`docs/specs/ui.md:1149-1152`).

No test in `internal/ui` goes red for this task.
`internal/ui/scan_test.go` holds `app.js` to what it imports and calls and to the interfaces it must not contain,
`internal/ui/ui_test.go` holds the served tree, its headers, and the shell's two asset references,
and `internal/ui/vendor_test.go` holds the vendored files and the MANIFEST;
none of the three names the identity, `.panels`, or a class this task adds,
and the entity tags they assert are computed from the bytes at startup rather than written down.
The two browser scenarios are the whole red of this task.
`internal/ui/scan_test.go` goes red in the next task, by the scan it adds.

```bash
go vet -tags e2e ./test/e2e/
go test -tags e2e -race -count=1 -timeout 40m -run 'TestScenarios/(console-oidc|console-basic)$' ./test/e2e/
```

What that run proves, stated exactly:
`details.identity` matches nothing on the unchanged page, where the identity is an article inside `.panels`,
so each scenario fails at its first identity read and stops there.
That is one selector failing twice, and it is the whole of this task's red.
The assertions past it — the escaped values in their two containers,
the Collection selectors, and the two disclosure-state reads — are not reached and are not separately red;
`.collections details` in particular already matches the current page,
because the Collections article carries its class today (`internal/ui/static/app.js:1465`),
so scoping those three selectors could not have failed on its own.
One run covers both scenarios.
It needs a machine with a Chromium and the kind lane, so it runs once here and not in the validation block.

- [ ] **Lay the page out**

Apply the two file changes above, then run the same command again;
both scenarios pass, the Collection detail assertions read the Collection's disclosure rather than the identity's,
and the disclosure reads closed on load and open after a person's click and a render.

`CHANGELOG.md`, `### Changed`:
**The console lays one panel to a row and folds the identity into a disclosure.**
The three panels stood in a grid of auto-fitting columns,
so a wide window put the tall Profile panel beside two short ones and left a column of empty page below them,
while each panel drew its controls one to a line down a column wide enough for four.
Each panel now takes a row of the page and lays its controls across it, every control keeping its own label,
wrapping onto the next line in the same order below the width a line needs.
The identity is a disclosure above the panels, closed on load, its summary naming the principal and the realm;
opening it shows the same seven facts and the same sign-out link the panel held.
An error from a refetch of the identity, and the control it carries, render outside the disclosure,
so a closed one hides no way out of a failed identity fetch.

- [ ] **Validate and commit**

```bash
semlf check internal/ui/static/app.js test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/app.js internal/ui/static/app.css test/e2e/scenarios_console_test.go \
  docs/specs/ui.md CHANGELOG.md
git commit -F <file holding: "feat(ui): give each console panel a row" and a body of one sentence per line under 120 characters saying that columns of panels left an empty column beside the tall one, that each panel now lays its controls across its row, and that the identity is a disclosure above the panels>
git log --oneline -1 && git status --short
```

---

## 2. A denial that names a realm opens the identity

Closes the roadmap bullet beginning *The `realm_denied` hint names "the identity panel"*
(`docs/plans/roadmap.md:369-373`).

**Files:**
- Modify: `internal/ui/static/app.js`, `internal/ui/scan_test.go`, `test/e2e/browser_test.go`,
  `test/e2e/scenarios_console_test.go`, `docs/specs/ui.md`, `CHANGELOG.md`
- Every `app.js` position cited below is a position in the tree this plan starts from.
  The previous task moved those from `render` downwards, so the implementer reads the current file
  and refreshes the design's citations at the end of this task from the finished tree.

**The change, and why.**
*Decisions* settles that `open` is set on the element rather than bound in the template,
that the call sites are `settle` and `sendStart` and never `refetch`,
that the Service listing gains the generation the other two listings already carry,
and that the scan holds the count to two while the browser holds the behavior.

`internal/ui/static/app.js`:

- The import at `:6` gains `createRef`, which the vendored Preact build exports.
  It is the fourth name the page takes from Preact; `:6` imports three today.
- The constructor gains `this.servicesSeq = 0` beside `targetsSeq` and `collectionsSeq` (`:324-328`),
  under the comment those two already carry, extended to say that the Service list counts apart as well.
- `loadServices` (`:563-576`) stamps `const seq = ++this.servicesSeq` before the request,
  passes `() => seq !== this.servicesSeq` as `request`'s `stale` argument (`:565`),
  and keeps its own return at `:566` as `seq !== this.servicesSeq`.
- `onNamespace` raises `this.servicesSeq` beside the two counters it raises already (`:876-880`).
  It calls `loadServices` only when the chosen namespace is not empty (`:902-904`),
  so choosing the placeholder would otherwise leave a request in flight that still looked current,
  which is the same reason that function bumps `targetsSeq` and `collectionsSeq` itself.
  With both in place, an answer for a namespace the page has left is dropped inside `request` (`:485-487`)
  and never reaches `settle`.
  A counter rather than the namespace comparison:
  choosing A, leaving for B, and returning to A leaves `this.state.ns` equal to the namespace A's answer named,
  so that comparison passes an answer the page has moved past (`docs/specs/ui.md:1124-1130`).
- The constructor also gains `this.identity = createRef()` beside those counters,
  with a comment saying the disclosure's `open` is its own state
  and that the page sets it once per qualifying answer rather than binding it in the template,
  because a native close changes `open` without the template knowing.
- `renderIdentity`'s `<details class="identity">` gains `ref=${this.identity}`, and still no `open`.
- A new `openIdentity()` sets `open` to `true` on `this.identity.current` when there is one,
  and returns otherwise:
  `render` answers `booting`, `navigating`, `signInRequired`, and `error` before it renders the identity
  (`:1124-1142`), so an answer classified before the identity is on the page has no element to open.
- `settle`'s `realm_denied` arm (`:525-527`) calls `this.openIdentity()` before `this.reloadWhoami()`,
  because the opening does not wait for the refetch (`docs/specs/ui.md:1137-1138`).
  The arm keeps the condition it has, so the refetch behaves as it does today;
  the opening is guarded on `res.status === 403` as well,
  because the design's predicate is a `403` carrying that envelope and not the envelope alone
  (`docs/specs/ui.md:1134-1136`).
  This is the listing path and the download path both:
  `request` reaches `settle` only for an answer its `stale` predicate did not discard (`:485-487`, `:492`),
  which is now true of the Service listing as well,
  and `onDownload` reaches it (`:1031`) only for an answer that is current by the control's own guard
  (`:1010-1012`, `:1021`, `:1025`, `:1383`).
- `sendStart` calls `this.openIdentity()` after `step.moved` (`:794`) and before the refetch loop (`:797-799`),
  when the answer it built at `:785` is a `403` whose code is `realm_denied`;
  `answerOf` carries both, the status at `:163` and the code at `:164` (`:160-171`).
  Before `step.moved` it would open for an attempt the operator has already left,
  and after the refetch loop it would wait on a fetch the design says it does not wait on.
  It is not hooked in `refetch`: that function also runs the whoami refetch a cancel's
  `404 collection_not_found` asks for (`:849-850`, `internal/ui/static/collectionmodel.js:252-256`),
  which names no realm that refused and opens nothing (`docs/specs/ui.md:965`, `:1142-1144`).
- `hints.realm_denied` (`:62`) becomes "your realm does not admit this; the identity shows what it does",
  the design's own row (`docs/specs/ui.md:1095`).
  The hint scan reads keys and not wording (`internal/ui/scan_test.go:247`, `:259-276`),
  so the key set is unchanged.

- [ ] **Write the scan and the four browser cases, and run what can fail red**

`internal/ui/scan_test.go`, beside the hints scan:

- `openIdentityCallRe` matches `this.openIdentity()`.
- `refetchBodyRe` cuts the body of `refetch`, anchored on its own opening line
  and on a closing brace at the method's indentation, the shape `hintsObjectRe` (`:247`) uses.
- `TestScanIdentityOpensWhereTheAnswerIsClassified` asserts two things:
  `app.js` holds exactly two calls of `this.openIdentity()`,
  and the body `refetchBodyRe` cuts holds none.
  It fails rather than passes when the body cannot be cut,
  because a scan that matched nothing would pass a page that never opened the disclosure at all,
  which is what `TestScanHintKeysRefuseWhatTheyCannotRead` (`:280-311`) holds the other scan to.
  Its doc comment says what the count is and is not:
  it catches a third call site, which is a path the design did not name,
  and a call inside `refetch`, which is the site that would open on a cancel,
  and it proves nothing about what `openIdentity` does, whether its ref is attached,
  or where inside a function each call sits — the browser cases below are what prove that.

`test/e2e/browser_test.go` gains a second interception helper,
and the two compose through one registry rather than through the single matcher the file carries today.
`session` holds one `hold func(url string) bool` and one `held chan fetch.RequestID` (`:218-219`),
`EventRequestPaused` consults that single `hold` (`:348-359`),
and `holdRequest` sets it, waits, and clears it (`:371-404`),
so two helpers that each assigned `s.hold` would clobber each other.

- `session` holds an ordered list of paused-request handlers in place of that matcher and that channel.
  Each entry is a URL match and what to do with the identifier the match accepts.
  `EventRequestPaused` walks the list in order,
  gives the request to the first entry whose match accepts it,
  and continues any request no entry accepts, which is what every request gets today.
  First match wins, so two helpers on disjoint patterns never contend,
  and two on overlapping patterns resolve by registration order rather than by luck.
- An entry takes exactly one request.
  Once it has parked one the walk continues past it,
  so a second request on the same route reaches the gateway.
  That changes nothing for the two steps that hold a request today
  (`test/e2e/scenarios_console_test.go:261`, `:346`):
  each holds a **Refresh** that is disabled while its request stands sent,
  so no second request on that route is sent,
  and the request count beside each release would report a second one if it were (`:272`, `:355`).
  It is what the stale Service-listing case below needs,
  whose second request on the Service route has to reach the gateway and answer normally.
- `session` also holds the set of Fetch URL patterns its live handlers need.
  Registering a handler adds its pattern and re-runs `fetch.Enable()` with the whole set,
  because the Fetch domain replaces its patterns on each enable rather than adding to them.
  Releasing a handler removes its pattern and re-runs `fetch.Enable()` with what is left,
  and disables the domain only when nothing is left and `intercepting` is false (`:215`),
  so a session answering an authentication challenge keeps its interception whatever a step does.
  Every enable such a session runs carries `WithHandleAuthRequests(true)` and no pattern at all,
  which is what `newSession` runs at `:282` and what pauses every request the page sends (`:234-235`):
  narrowing an intercepting session to one step's pattern, or dropping that flag,
  would break the challenge in the middle of the scenario that answers one.
- `holdRequest` keeps its signature and its `t.Cleanup` (`:401`).
  What changes is that it registers a handler rather than assigning the one matcher,
  and its `release` deregisters that handler rather than clearing it.
- `heldCount` reads `len(s.held)` (`:406-409`), and `newSession` makes that channel at `:258`.
  Both move into the entry.
  `heldCount` becomes the number of live handlers holding a request nothing has released.
  Both its callers read it after their own release (`test/e2e/scenarios_console_test.go:272`, `:355`),
  where that number is zero as `len(s.held)` is today,
  because `holdRequest` takes the identifier out of the channel before either of them runs.
- `answerRequest` is `holdRequest` with `fetch.FulfillRequest` where that one continues.
  It takes the same name, pattern, match, and press, and the response to write — a status, a `code`, and an `error` —
  registers a handler, runs press, returns once the request the match accepts is paused,
  and hands back a release that writes the response and deregisters the handler.
  The release answers rather than the handler answering the moment the request pauses,
  because the stale Service-listing case below needs its answer to land after the page has moved on,
  and a handler that answered on pause could not produce that order;
  a case that wants the answer at once calls the release at once.
  Like `holdRequest`'s, the release runs its body at most once and is registered as a `t.Cleanup` of its own.
- The response carries `Content-Type: application/json`,
  and a body that decodes to an object with string `error` and `code` fields,
  because the page reads a body as an envelope only then
  and otherwise shows `HTTP <status> <statusText>` with nothing from the body (`docs/specs/ui.md:1165-1167`);
  an answer without that header would fail the case for the wrong reason.
- Where a case registers two handlers, each helper owns its own cleanup and neither owns the other's.
  `t.Cleanup` runs in reverse registration order, so the handler registered second releases first.
  The order does not matter: each release runs at most once, each removes only its own pattern,
  and the domain is disabled only when the set empties.
- Each pattern names the route its case answers and no more,
  because a wider one would pause the whole page;
  every request no handler accepts still reaches the gateway.

`test/e2e/scenarios_console_test.go`, in `scenarioConsoleOIDC`, gains the four cases
*Errors* leaves to an answer the page classifies (`docs/specs/ui.md:1154-1160`),
each reading `document.querySelector("details.identity").open`.

They go in immediately after `s.waitFor(t, "the Pod control lists a Pod", podListed)` (`:310`)
and before the comment that opens the scale-down (`:311`), while the app still has a Ready Pod.
The scenario scales `testapp` to zero at `:311-318` as its last step against the app,
and every step below that is written against a Service with no eligible Pod,
where **Download** is a disabled control (`:330-332`, `:358-367`).
These cases press **Refresh**, **Download**, and **Start collection**,
each of which needs a Service the page can build a live request for,
so above the scale-down is the only place in the scenario they work.

Each case releases every handler it registers before it ends:
a parked request left behind would fail the count beside the targets release at `:355`,
which is an assertion about that **Refresh** and not about anything these cases do.
Each press waits for its own control to be idle first, the way `:343` does.

- **A qualifying denial opens it.**
  With the disclosure closed, **Refresh** on the targets list is pressed
  and the targets request is answered `403 realm_denied`;
  the disclosure is open, the Profile panel shows the code, the message, and the hint,
  and the page sent the `/v1/whoami` refetch.
  The same is asserted for the download path,
  by answering the profile request `403 realm_denied` on a press of **Download**,
  and for the start path, by answering the `POST` of a confirmed **Start collection** the same way.
- **A second identical denial opens it again.**
  The disclosure is closed by a click on its summary,
  the same press is repeated against the same answer, and it is open again.
- **A person's closing survives a late refetch answer and an unrelated render.**
  It registers two answering handlers, in one order and not the other.
  The targets handler goes first, its press pressing **Refresh** on the targets list,
  which leaves the targets request parked.
  The `/v1/whoami` handler goes second, taking the targets release as its own press,
  so the `403 realm_denied` is written only once the handler that catches the refetch is live;
  registering that handler after the release would let the refetch reach the gateway and answer,
  leaving the handler waiting out its deadline for a request that is already past it.
  The denial opens the disclosure and the refetch stands parked;
  the disclosure is closed while that request stands sent,
  and the parked request is then answered with an error carrying its own envelope.
  Both are answered and neither is merely held,
  because a held request the release continues reaches the gateway and answers `200`,
  which leaves no error and no **Retry** control to read.
  The disclosure is still closed once that answer has landed,
  and still closed after a press of **Refresh** on the Collections table that renders the page again.
  The same step reads the recovery the failed refetch offers:
  its error and its **Retry** control are in the document and outside `details.identity`,
  because a closed disclosure must hide no way out of a failed identity fetch
  (`docs/specs/ui.md:785-787`, `:1146-1147`).
- **A discarded answer and a cancel's `404` leave it as it stood.**
  The Service listing's stale answer is the case blocker of the two.
  The scenario runs in one namespace (`:74`), so it is written as the namespace, the placeholder, the namespace again,
  which is the sharper sequence anyway:
  the namespace is chosen and its Service-list request is parked,
  the empty option is chosen, and the namespace is chosen once more.
  `onNamespace` raises the two counters it has on every change (`:876-880`)
  and calls `loadServices` only when the chosen namespace is not empty (`:902-904`),
  so the empty option leaves the parked request in flight with nothing to invalidate it,
  and choosing the namespace again sends a second Service-list request,
  which reaches the gateway and answers normally.
  The case waits for the Service select to offer the Service again, which is that answer landing,
  and only then answers the parked request `403 realm_denied`.
  That order is part of the case and not a convenience:
  `request` clears the error under the key on a `200` (`:488-491`),
  so a success landing after the denial would wipe the panel error
  and leave the case green against the unchanged page.
  On the denial the disclosure is still closed,
  and the Service panel, which is where a Service listing's error renders
  (`internal/ui/static/app.js:1228`), shows no denial.
  This is the sequence a namespace comparison cannot pass:
  the page is back on the namespace the parked request asked for,
  so `this.state.ns` at `:566` says that answer is current and a counter says it is not.
  Choosing the namespace cleared the Service selection,
  so the sequence ends by choosing the Service again and waiting for the Pod control to list a Pod,
  which leaves the page as `:310` left it and is what the cancel half below needs.
  For the cancel, the same case starts a Collection of its own with nothing intercepted,
  through **Start collection** and its confirmation, so the gateway creates one and its row offers **Cancel**:
  `renderCancel` draws that control only on the states `cancelOffered` admits
  (`internal/ui/static/app.js:1538-1547`),
  and the Collection the scenario started earlier is terminal by `:286` and offers none.
  **Cancel** is then pressed through its confirmation with only the cancel route intercepted,
  answered `404 collection_not_found`; the disclosure is still closed.
  The interception is scoped to the cancel route on purpose:
  the Collections refetch that outcome causes goes to the gateway and answers normally,
  and a `403` on that refetch would open the disclosure correctly and make the case say the opposite of what it means.
  The case then releases the handler and presses **Cancel** through its confirmation once more,
  so the gateway ends the Collection this case started and the scenario leaves none running.

```bash
go vet -tags e2e ./test/e2e/
go test -race -count=1 ./internal/ui/ -run 'TestScanIdentityOpensWhereTheAnswerIsClassified'
go test -tags e2e -race -count=1 -timeout 40m -run 'TestScenarios/console-oidc$' ./test/e2e/
```

What is red, and what is not:

- The scan reports zero calls where two were wanted, because the method does not exist.
- The three denial cases fail: the disclosure stays closed on every one of them.
  That is the defect — a `403 realm_denied` refetches the identity into a disclosure that stays shut,
  and the hint that names the identity points at nothing on screen.
- The stale Service-listing case fails too, and it fails for the second defect,
  on the assertion that the Service panel shows no denial and not on the one about the disclosure.
  The disclosure reads closed before the change whatever happens, because nothing opens it yet.
  The panel does show the denial: `loadServices` passes no `stale` predicate,
  so the parked answer reaches `settle` at `:492` and is recorded,
  and only then does `loadServices` compare the namespace at `:566` and find it unchanged.
  It is the last answer to land, because the case writes it after the second one has,
  so nothing clears it under the key afterwards (`:488-491`).
  Once `settle` opens the disclosure,
  that same recorded answer opens it for a namespace the page has left and come back to.
  With the counter, `request` drops the answer at `:485-487` before `settle`, and both assertions hold.
  Written before the counter, this case is red; written after it, it would pass for free.
- The cancel case and the closing-survives-a-late-answer case **pass** before the change,
  because nothing opens the disclosure at all yet.
  They are written now so the change is made with them watching, and they must stay green across it.

- [ ] **Open it, and say so**

Apply the `app.js` changes above, and run:

```bash
go test -race -count=1 ./internal/ui/
go test -tags e2e -race -count=1 -timeout 40m -run 'TestScenarios/(console-oidc|console-basic)$' ./test/e2e/
```

`docs/specs/ui.md`:

- *Unit*, after the `targetmodel.js` reason bullet (`:1529-1532`), gains one bullet:
  `app.js` holds exactly two calls of the disclosure's opening,
  and the body of the function that runs an outcome's refetches holds none,
  because that function serves a start's `403 realm_denied`, which opens the disclosure,
  and a cancel's `404 collection_not_found`, which does not (*Errors*);
  the bullet says the scan counts call sites and proves no behavior,
  the six cases being the browser scenario's (*End to end*).
- *End to end* `console-oidc` gains the four cases this task adds,
  and the reading that a failed identity refetch's recovery is outside the closed disclosure.
  The test writes the `403`, the `404`, and the refetch's own error into a paused request,
  rather than producing them by narrowing a realm.
- *What is not proven* (`:1760-1762`) loses its last clause, the disclosure's opening on a denial,
  which is what the previous task left on it after taking off the initial state and a person's own toggle;
  panel placement and control wrapping stay, and are the whole of what the sentence then says.
- *What is not proven* (`:1759`) keeps the two **Refresh** controls' wiring
  and loses the refetch of `/v1/whoami` a listing's `403` causes,
  which the closing-survives-a-late-answer case observes as a request it holds.
- *Errors* cites twelve positions in `app.js` across six lines
  (`:1123`, `:1125`, `:1126`, `:1132`, `:1133`, `:1136`);
  this task moves the ones below each line it adds and rewrites `loadServices` itself,
  so all six are rewritten to the finished tree's positions.
  No amendment row for that: the claim is unchanged.
- One amendment row names *Unit*, *End to end*, and *What is not proven*.

`CHANGELOG.md`, `### Fixed`:
**A `403 realm_denied` opens the identity disclosure its hint names.**
The hint told a caller the identity showed what their realm admits
while the disclosure that holds it stayed closed.
A denial of a listing, of a profile download, or of the current start attempt now opens it,
at the same moment the page refetches `/v1/whoami` rather than after the answer arrives.
A person's own opening and closing stand until the next such denial;
a cancel's `404 collection_not_found` leaves the disclosure as it stood.
The Service list now counts its requests the way the targets and Collections lists do,
so an answer for a namespace the page has left is discarded instead of shown,
which is what keeps a stale denial from opening the disclosure for a namespace nobody is looking at.
The hint drops the word panel.

- [ ] **Validate and commit**

```bash
semlf check internal/ui/static/app.js docs/specs/ui.md CHANGELOG.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add internal/ui/static/app.js internal/ui/scan_test.go test/e2e/browser_test.go \
  test/e2e/scenarios_console_test.go docs/specs/ui.md CHANGELOG.md
git commit -F <file holding: "fix(ui): open the identity on a realm denial" and a body saying the hint named an identity a closed disclosure hid, which of the three answers open it, that the opening is set on the element rather than bound in the template, and that the Service list now counts its requests so a stale denial is discarded>
git log --oneline -1 && git status --short
```

---

## 3. The guide describes the page

Closes the roadmap bullet beginning *`docs/console.md:32` lists the page's parts*
(`docs/plans/roadmap.md:382-383`)
and clears *Required by this revision and not yet made* (`docs/specs/ui.md:2094-2110`).

**Files:**
- Modify: `docs/console.md`, `docs/specs/ui.md`

**The change.**
`docs/console.md`:

- *What it shows* (`:32-33`) lists the page's four parts and names Identity first;
  it becomes: the page is the identity disclosure and then panels, one panel to a row,
  in the order they load — Service, Profile, and Collections when offered —
  each panel laying its controls across its row and wrapping them onto the next line at a narrow window.
- The Identity bullet (`:35-37`) describes the identity as one of those parts;
  it becomes: the identity is a disclosure above the panels, closed on load,
  whose summary names who you are and your realm;
  opening it shows your realm's namespaces, Services, and profiles exactly as configured
  (the wildcard `*` shown as written), the three PGO flags, and the authentication mode,
  with a sign-out link at its foot when one would do something.
  It adds that a `403 realm_denied` opens it, so the realm that refused is on screen with the message,
  and that an error from that refetch shows above the panels rather than inside the disclosure.
- `:76`, `:83`, and `:91` are the three places the guide writes the word panel, one per authentication mode.
  `:76`: "The Identity panel shows `anonymous`" becomes "The identity shows `anonymous`".
- `:83`: "the Identity panel says so in place of a sign-out link"
  becomes "the identity says so in place of a sign-out link, at the foot of the disclosure".
- `:91`: "A **Sign out** link appears in the Identity panel"
  becomes "A **Sign out** link appears at the foot of the identity disclosure".

`docs/specs/ui.md` *Required by this revision and not yet made* (`:2094-2110`) is prose, not a table.
Its first five lines say one edit is owed and name the guide (`:2096-2100`), and they become "Nothing."
The console guide's account of the page joins the list of documents this one is no longer ahead of (`:2101-2110`).
One amendment row names the section.

- [ ] **Name the check**

This task has no red test: prose has none,
and a check that the old sentences were false is a reading of the page against them,
which task 1 and task 2 have made.
What verifies it is `mise run check` for the spec's `Status:` and its links,
`mise run prose` for the wording,
and a reviewer reading each changed sentence beside the part of the page it describes.

- [ ] **Validate and commit**

```bash
semlf check docs/console.md docs/specs/ui.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add docs/console.md docs/specs/ui.md
git commit -F <file holding: "docs(console): describe the page's new shape" and a body naming the four places the guide called the identity a panel and saying the page is now one panel to a row>
git log --oneline -1 && git status --short
```

---

## 4. Close the plan

**Files:**
- Modify: `docs/plans/console-layout.md`, `docs/plans/roadmap.md`

This task runs in one order and not another.
The whole change is validated first — the suite, the browser check, and every block above —
then the pull request is opened and its number read from it,
and only then is this file's line 3 changed to `**Status:** Done`
and line 4 to `**Outcome:** pull request #<n> ...`, naming that number and what shipped.
A placeholder on line 4 satisfies the text check and closes nothing,
so the number is the real one or the task is not done.
In the same commit the roadmap item's guide bullet (`docs/plans/roadmap.md:382-383`) is ticked
and its `Shipped:` line (`:389`) names that pull request.
The pull request is named rather than a commit because the merge rebases this branch onto `main`
and rewrites every hash on it, while the number is the same before and after;
[`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md) admits a pull request there for that reason,
and `check_status` in [`check-repo.py`](../../scripts/check-repo.py) requires `**Outcome:** ` followed by text on line 4.
This commit does not delete the plan.
The deletion is the next commit that touches this file, and it is taken after the merge,
which is a choice of when rather than something
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) requires;
what the protocol requires is that the next commit touching a finished plan is the one that removes it.
That commit deletes this file and rewrites every link that cited it,
which `check_links` enforces, and changes nothing else.
`grep -rn console-layout --include='*.md' .` finds the links.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/console-layout.md docs/plans/roadmap.md
mise exec golangci-lint@2.12.2 -- golangci-lint run ./... && mise run test && mise run check && mise run prose
git add docs/plans/console-layout.md docs/plans/roadmap.md
git commit -F <file holding: "docs: close the console layout plan" and a body saying the item's bullets are done and its Shipped line names the pull request>
git log --oneline -1 && git status --short
```

---

## Validation

Every task ends with the block above.
Before the pull request opens, the whole change also runs the end-to-end suite,
on a machine with a Chromium installed:

```bash
mise run test:e2e
```

It is required, and it is what
[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md) asks for:
it lists `internal/ui` among the eight packages that need the suite on the `current` lane before a pull request,
and says a change to `internal/ui/static/` needs the browser scenarios,
which skip by name on a machine with no browser (`.agents/rules/500-validation-and-workflow.md:91-97`).
What the suite proves here:
the identity's principal and realm read at the disclosure's summary and its authentication mode at the disclosure,
under both authentication modes;
the hostile principal escaped in the disclosure's markup and the hostile query escaped in the Service panel's,
with no element either string names anywhere in the document and no script run from either;
every Collection assertion reading the Collection's own disclosure rather than the identity's;
and the six cases of the disclosure's rule, over `details.identity.open` in the running page.
Those reads and cases ran red once, in the task that made each of them fail,
except the two that could not run red and say so where they are written.
The suite proves nothing about the arrangement:
no scenario asserts panel placement or control wrapping, which `ui.md` states outright
(`docs/specs/ui.md:1760-1761`), and the visual check below is the whole of that proof.

The harness pulls the NATS, Dex, and Keycloak images into the cluster before any scenario runs
and exits when a pull fails, so a failure at that point is the registry rather than this change.

**The visual check.**
It runs once after the last change to `internal/ui/static/`, and it is required rather than optional,
because the arrangement is what this plan changes and no test asserts it.
It is this plan's own check and not a substitute for the suite, which the rule above requires separately.

Set up the cluster and the page:

- Run the suite with `PROFGATE_E2E_KEEP=1`, which is what keeps the cluster;
  a cluster named `profgate-current` whose node image matches the lane's is reused on the next run,
  and one that does not match is deleted and recreated (`test/e2e/harness_test.go:205-223`).
- Turn the console on in the in-cluster ConfigMap and restart the Deployment,
  apply the test app into a fresh namespace, and port-forward the gateway.
- An implementer working in the main checkout needs nothing more;
  one working in a `git worktree` sets `GOFLAGS=-buildvcs=false`, without which `ko`'s build fails.

Then, in Chrome, at a 1600 px window and again at 700 px,
which is below the `48rem` the identity list flips at:

- a Service with targets: each of the three panels takes a row of the page,
  the Service panel's two selects sit on one line,
  the Profile panel's profile, port, Pod, and version controls sit on one line,
  with **Refresh** at the width of its own label,
  the URL field and the download actions sit on the lines under them,
  and at 700 px the controls wrap onto further lines as they need to, in the same order,
  with nothing hidden and nothing reordered;
- the port control's own fields on the row: with `discovery.pprof.allowedSelections` carrying a number wildcard,
  the **Port number** field sits on the line beside the port menu rather than under it,
  and the fieldset's **Port** caption sits where the labels beside it do;
  again with a name wildcard and its **Port name** field;
  and again with `pprof.maxSeconds` configured, whose seconds control is a fourth field on the same line;
- a long value on the row:
  a Pod name near the 63-character bound and a port name at its own bound leave every control at its own share,
  and push nothing off the row;
- a Service with no eligible Pod: the empty state stands where the Pod and version controls were
  and takes the whole row, with **Download** disabled and its reasons beside it;
- the disclosure closed on load, its summary naming the principal and the realm,
  and its seven term and value pairs reading as two pairs to a row at 1600 px and one at 700 px,
  with no stray gap under a value;
- the Collection detail below it unchanged: its own disclosure open when a row is opened,
  its list one column of pairs, and its timestamps wrapping inside their values as they do today;
- a person's toggle surviving a render: open the disclosure, press **Refresh** on the targets list,
  and it is still open when the answer lands; close it, press **Refresh** again, and it is still closed;
- a real `403 realm_denied` opening it,
  which the suite produces from a paused request and this check produces from the gateway.
  With the console open on a listed Service,
  patch the in-cluster ConfigMap so the realm no longer admits that Service —
  removing the exact entry and every wildcard that would still admit it,
  because `realmAllows` matches by the wildcard or the exact string
  (`internal/httpapi/realm.go:15`, `:18-27`) —
  restart the Deployment, wait for the rollout and for the gateway to answer as ready,
  and re-establish the port-forward on the same local port, so the page's origin does not change.
  The browser page stays open across all of that and is never reloaded:
  **Refresh** on the targets list is disabled while the chosen Service is absent from the page's Service list,
  and a reload after the restart would fetch a list the narrowed realm no longer puts it in.
  The page's Service list still holds the selection from before the restart, so the control is enabled
  and the request goes.
  Read the answer in the browser's network panel and confirm it is a `403` whose body names `realm_denied`
  (`internal/httpapi/server.go:561-563`), not some other refusal;
  the Profile panel shows the code, the message, and the hint,
  and the disclosure opens with the realm that refused inside it.
  Close it and press **Refresh** once more: the second denial opens it again.

`kind delete cluster --name profgate-current` afterwards.
Report what ran and what was skipped in the pull request description.

Prose gets `semlf check` before the hook sees it,
on every Markdown file, every Go file with doc comments, and `app.js` when a task edits it;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **The arrangement is proven by a person looking at it.**
  No test asserts panel placement or control wrapping, and `ui.md` says so (`docs/specs/ui.md:1760-1761`).
  The visual check above is the whole proof, which is why it is required,
  why it names two widths, and why it names the three port configurations that change the row.
- **Nothing holds the template to never writing `open` on the disclosure.**
  The scan holds the call sites and reads no attribute.
  An implementer who bound `open` in the template would pass the scan
  and fail the browser case that closes the disclosure and renders the page again,
  which is where that mistake shows.
- **The scan counts call sites and proves no behavior.**
  Two calls in the wrong two functions pass it, an `openIdentity` with an empty body passes it,
  and a ref that is never attached passes it.
  What it catches is a third site and a site inside `refetch`,
  and its bullet in *Unit* says that much and no more.
  The behavior is the browser scenario's.
- **The browser cases write the denial rather than earning it.**
  A `403 realm_denied` from a paused request is the same envelope the gateway sends
  and reaches the page through the same `fetch`,
  but it does not prove the gateway sends it for a narrowed realm;
  the wire proof of that is `internal/httpapi`'s, and the visual check above produces one from a real gateway.
  What the cases prove is what the page does with such an answer, which is what this plan changes.
- **`createRef` is the fourth name the page imports from Preact.**
  `internal/ui/static/app.js:6` imports three today.
  It is exported by the vendored build and adds no file and no dependency;
  a Preact bump that dropped it would fail the page at load,
  which no unit test would see and the console scenarios would.
- **Each task's red run costs a lane run.**
  Both console scenarios fail on the same missing selector in the first task, so one run covers both;
  the second task's cases run in `console-oidc` alone.
  Each needs a machine with a Chromium and the kind lane,
  and a machine without one cannot show either task red and must say so in the pull request description.
- **The plan's deletion is not one of its tasks.**
  The closing task leaves the finished document in the tree under the lifecycle checks;
  the commit that deletes it and rewrites its links follows the merge.

---

## Self-Review

- Bullet coverage, one line each:
  the grid and the Profile panel's column (task 1);
  the identity as a `<details>` (task 1);
  the browser reads that move (task 1, in the task whose change makes them red);
  the hint and the denial that opens the disclosure (task 2);
  the guide (task 3).
- Where the design's text and the code disagreed, and what this plan does about it:
  *Errors* now says the Service listing carries no generation and gains one (`docs/specs/ui.md:1124-1130`),
  which is what `loadServices` shows (`internal/ui/static/app.js:565`, `:566`),
  so the opening task adds the counter and the browser case that a namespace comparison cannot pass;
  *Errors* says the opening reads the status as well as the code (`:1134-1136`)
  where `settle`'s arm reads the code and the key (`app.js:525`),
  so the opening carries a status test of its own.
  *Errors* lists six cases a test has to tell apart (`docs/specs/ui.md:1154-1160`)
  while *What is not proven* says no browser scenario asserts the disclosure's initial state
  or its opening and closing (`:1760-1762`);
  this plan puts all six in the browser scenario and rewrites that sentence in the two tasks that make it false,
  leaving panel placement and control wrapping on it.
  The roadmap's own citations are correct as they stand:
  `app.js:1141-1142,1170-1200` is the identity's place in the grid and the article itself,
  and `app.js:1172-1192`, `:1211-1226` are the seven pairs and the two selects.
- Current-source facts this plan rests on, each confirmed by reading the file:
  `hints` is `internal/ui/static/app.js:56-81` with its `realm_denied` row at `:62`;
  the Preact import at `:6` names three exports;
  `ErrorBox` is `:280-291` and `SignInRequired` is `:297-304`, each rendering its own `div.error`;
  `targetsSeq` and `collectionsSeq` are declared at `:324-328`;
  `boot` is `:431-457`, setting `phase: "ready"` before it calls `loadAfterBoot` at `:435`,
  and `realmAllows` admits every listing but the Service list unconditionally
  (`internal/httpapi/realm.go:19-20`), so no `/v1/limits` answer is a `403 realm_denied`;
  `request`'s stale return is `:485-487`, its `200` arm clears the error under the key at `:488-491`,
  and its call of `settle` is `:492`;
  `settle` is `:503-529` with its `realm_denied` arm at `:525-527`, testing the code and the key and not the status;
  `loadServices` is `:563-576`, passing no `stale` argument at `:565` and comparing the namespace at `:566`;
  `loadTargets` stamps at `:586` and passes its predicate at `:596` and compares at `:598`;
  `loadCollections` stamps at `:634`, passes at `:641`, and compares at `:643`;
  `reloadWhoami` is `:688-694`; `refetch` is `:697-711`;
  `sendStart` builds the answer at `:785`, reads `step.moved` at `:794`, and refetches at `:797-799`;
  `sendCancel` refetches at `:849-850`;
  `downloadAllowed` is `:1010-1012`, `onDownload` guards at `:1021` and sets `downloading` at `:1025`,
  and the control is disabled from it at `:1383`;
  `render` is `:1124-1148` with the panels at `:1140-1147`;
  `panelError` is `:1150-1163`;
  `renderIdentity` is `:1165-1201`, its `<article>` `:1170-1200`, its seven pairs `:1172-1192`,
  its whoami error `:1193`, and its footer `:1194-1199`, the only `<footer>` the page renders;
  `renderSelection` is `:1204-1231`;
  `renderPortControl` is `:1233-1278`, its fieldset `:1240-1276` and its two optional fields `:1245-1275`;
  `renderRequest` is `:1280-1401`, its `<article>` at `:1317`;
  `renderCollections` is `:1462` and its article already carries `class="collections"` at `:1465`;
  the Collection detail is `<details open>` at `:1560`, its `<dl>` at `:1562`,
  and its four timestamp values at `:1570-1575`;
  `startOutcome`'s `realm_denied` arm is `internal/ui/static/collectionmodel.js:211-213`
  and `cancelOutcome`'s `collection_not_found` arm is `:252-256`;
  `.panels` is `internal/ui/static/app.css:9-14`, `.panels article header` is `:22-26`,
  `.panels article.collections` is `:38-40`, `.panels dl` is `:42-47`, `.panels dt` is `:49-51`,
  `.panels dd` is `:53-56` with `overflow-wrap: anywhere`, and `.error` is `:70-81`;
  the vendored Preact build exports `createRef`;
  the MANIFEST covers the vendored tree alone (`internal/ui/vendor_test.go:12-14`);
  `TestScanNoBlockingDialogs` is `internal/ui/scan_test.go:96-100`,
  `TestScanPageNamesTheVerb` is `:104-108`,
  `hintCodes` is `:223-244`, `hintsObjectRe` is `:247`, `hintKeys` is `:259-276`,
  and `TestScanHintKeysRefuseWhatTheyCannotRead` is `:280-311`;
  no test in `internal/ui` names the identity or `.panels`, so only the browser scenarios move;
  `test/e2e/scenarios_console_test.go` reads the identity at `:173-178` and `:416-421`,
  `assertRenderedAsText` is `:800-821`, reading the markup at `:808` and called twice, at `:193` and at `:377`,
  `textOf` is `:1112-1120`,
  the Collection detail selectors are `:231`, `:232`, and `:977`,
  the unlisted-selection read is `:189`,
  and the four `.panels` reads that only build a failure message are `:283`, `:304`, `:366`, and `:891`;
  `holdRequest` is `test/e2e/browser_test.go:371-404`, registering its cleanup at `:401`,
  and `observe` decides each paused request at `:348-359`;
  the session's one matcher and one channel are `:218-219`, `intercepting` is `:215`,
  `newSession` makes that channel at `:258` and enables Fetch with the authentication flag and no pattern at `:282`,
  which is what pauses every request (`:234-235`), and `heldCount` reads the channel's length at `:406-409`;
  the two steps that hold a request are `test/e2e/scenarios_console_test.go:261` and `:346`,
  each reading the count beside its release at `:272` and `:355`,
  and `:343` waits for the control to be idle before the press;
  the Pod control's wait is `:310` and the scale-down is `:311-318`,
  after which **Download** is a disabled control (`:330-332`, `:358-367`);
  `chooseOption` sets the select's value and dispatches a change, and offers the placeholder like any option
  (`:1021-1049`);
  the Service listing's error renders in the Service panel (`internal/ui/static/app.js:1228`),
  and `renderCancel` offers its control only on the states `cancelOffered` admits (`:1538-1547`),
  so the Collection the scenario cancels at `test/e2e/scenarios_console_test.go:286` offers none afterwards;
  the cluster is reused only when it was kept and its image matches (`test/e2e/harness_test.go:205-223`);
  the scenarios are registered as `console-oidc` and `console-basic` at `test/e2e/registry.go:45-46`
  and dispatched at `test/e2e/harness_test.go:180-181`;
  the gateway answers `403 realm_denied` at `internal/httpapi/server.go:561-563`;
  `docs/console.md` lists the page's parts at `:32-33`, describes the identity at `:35-37`,
  and writes the word panel at `:76`, `:83`, and `:91`, quoting no hint text;
  `CHANGELOG.md` carries `### Added`, `### Changed`, and `### Fixed` under `## [Unreleased]` at `:10`, `:98`, `:234`;
  the roadmap's guide bullet is `docs/plans/roadmap.md:382-383` and its `Shipped:` line is `:389`;
  `check_status` requires `**Outcome:** ` on line 4 of a `Done` plan and nothing on line 4 of a `Draft` one
  (`scripts/check-repo.py:83-98`);
  `semlf check` reports 23 `wrap` findings in `app.js` and none in `app.css`;
  a kind cluster named `profgate-current` is running on this machine;
  every commit header above is under 50 characters.
- Decided here, with the reason stated where it is carried:
  four tasks, the browser work riding with the task whose change makes it red;
  the prototype taken as it is, with every new rule read against the Collection detail it could reach
  and the port control's own fields laid across the row the design puts them on;
  `open` set on the element through a `createRef`, never bound in the template;
  the opening called from `settle` and from `sendStart`, never from `refetch`,
  and guarded on the status as well as the code;
  the Service listing given a generation counter, because it is the one listing without one;
  a source scan counting two call sites and refusing one inside `refetch`,
  kept for that and claiming no behavior, with its own case in *Unit* saying so;
  the six cases proven in the browser, two with the markup and four with the opening rule,
  from answers the test writes into a paused request;
  the session's single matcher replaced by an ordered list of handlers, each taking one request,
  so the two interception helpers compose and a second request on an answered route still reaches the gateway;
  the answering helper's release writing the response rather than its handler answering on pause,
  because the stale Service-listing case needs its answer to land after the page has moved on;
  the four cases placed above the scale-down, where the app still has a Ready Pod;
  the stale sequence written as the namespace, the placeholder, and the namespace again,
  because the scenario has one namespace and that sequence is the one no namespace comparison can pass;
  the identity's summary asserted at the summary, because the disclosure's text holds those values either way;
  the markup read split in two so each value is proven in the container that renders it;
  the design's `app.js` citations refreshed by each task that moves them, with no amendment row;
  the guide last before the close, clearing the prose that names it;
  the plan closed after the validation,
  naming the real pull request number in both `Outcome:` and the roadmap's `Shipped:` line,
  and deleted by the commit after the merge.
- Left to the implementer:
  the exact regular expressions `openIdentityCallRe` and `refetchBodyRe` cut with;
  the Go type of a paused-request handler and of the session's pattern set,
  and how a case names the route it answers;
  the new signature of `assertRenderedAsText` and the wording of its two failure messages;
  the exact wording of the three amendment rows;
  and the wording of every commit body.
