# The Profile Panel's Offer and Its Request

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> The design is already accepted in [`ui.md`](../specs/ui.md) —
> *Controls*, *Starting and cancelling a Collection*, *Unit*, *What is not proven*,
> *Package layout*, and *Dependencies* —
> and every task implements from that text rather than from this one
> ([`000-agent-contract.md`](../../.agents/rules/000-agent-contract.md#document-authority)).

**Goal:** move the Profile panel's five decisions out of `app.js` into a fifth model module a test executes,
repair the one of them that accepts a duration the gateway refuses,
and give the Collections table's half of the `pgo` rule the same treatment.

The page decides five things about a profile request before it sends one:
which profiles the menu offers, what the duration's bound is, what value to put in the input,
whether a typed duration is within the bound, and what the request carries when all of that settles.
All five live in `app.js` today as component methods reading `this.state`
(`internal/ui/static/app.js:1213-1220`, `:1222-1235`, `:1237-1244`, `:1246-1254`, `:1278-1291`),
which is the one console file no unit test executes:
`app.js` imports Preact and htm, so the goja harness cannot evaluate it,
and what proves these rules today is a browser scenario or nothing.

Three of them are copies of gateway rules, and one of the three is wrong.
`listAllows` (`:254-257`) mirrors `internal/httpapi/realm.go:66-68`, the realm filter the gateway applies,
and the profile menu is that filter run a second time —
run by the page before it can ask, because the menu is drawn before anything is sent.
A copy that drifts offers a profile the gateway then answers `403 realm_denied` for
(`internal/httpapi/realm.go:25`).
`upstreamSeconds` (`:44`) is a copy of the gateway's own upstream defaults,
and `defaultSeconds` sends the lower of that and the configured bound so a request never rests on the upstream.

The third copy is `secondsValid`, and it admits values the gateway does not.
It tests `Number.isInteger(Number(seconds))` (`:1252-1253`),
so `1e1` reads as ten and passes, and `0x10` reads as sixteen and passes.
The input keeps the text that was typed rather than the number it read,
so `currentProfileURL` sends `seconds=1e1` (`:1288`),
and `parseSeconds` accepts an unsigned decimal integer and nothing else
(`internal/httpapi/profile.go:324-337`, and the grammar row at `docs/specs/gateway.md:962`).
The console therefore enables **Download** for a request it can already know will be refused.
The spec now requires digits alone and names it the second gateway rule the page copies;
this plan repairs it where it becomes provable, which is inside the module.
None of the three copies has a test that can fail today.

After this plan `internal/ui/static/profilemodel.js` holds the five as pure functions,
`app.js` is their caller, and a table test drives each of them in the interpreter.
`tableOffered(limits, whoami)` joins `startOffered` in `collectionmodel.js` in the same way:
that rule was written in the spec and held in the page,
which left the module explaining a rule it did not carry and no test executing it.
One scan the spec has always asked for and nobody wrote is written here too:
`selectionListed()` holds membership to the stored catalog, and `profileRequest` takes that answer as a parameter,
so the scan that holds the caller is the obligation the parameter creates.

**Architecture:** `internal/ui/static/profilemodel.js` is new and holds
`offeredProfiles`, `secondsLimit`, `defaultSeconds`, `secondsValid`, and `profileRequest`;
it imports nothing, spells no `/v1` path, builds no URL, and mutates no argument.
`internal/ui/static/app.js` loses `upstreamSeconds`, `listAllows`, four component methods,
and the parameter half of `currentProfileURL`, and gains one import.
`internal/ui/static/collectionmodel.js` gains `tableOffered`.
`internal/ui/profilemodel_test.go` is new.
`internal/ui/scan_test.go` and `internal/ui/vendor_test.go` count the fifth module,
`internal/ui/scan_test.go` gains the membership scan,
and `internal/ui/collectionmodel_test.go` counts the new function.
`.agents/rules/500-validation-and-workflow.md:93` counts five model modules.
`CHANGELOG.md` opens an `Unreleased` section for the duration repair.
`docs/console.md` says the duration field takes whole numbers.

No gateway code changes, no route changes, no Kubernetes call, no configuration key, and no chart value.
No response shape changes, and the only request that changes is one the gateway refuses today.

**Spec:** [`ui.md`](../specs/ui.md) carries this design as accepted text,
in the sections the note above names and in its *Amendments* row for them.
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.

- **The page offers a request only when the gateway can answer it.**
  This is the invariant the duration repair holds and the reason that repair is not a move.
  A console that enables **Download** for `seconds=1e1` has spent a round trip to learn what it already knew:
  `parseSeconds` reads decimal digits and nothing else (`internal/httpapi/profile.go:324-337`).
  After this plan the value the page refuses and the value the gateway refuses are the same set.

- **Everything else is a move and not a behavior change.**
  Every profile URL the page builds, every value the duration input starts at,
  every profile the menu offers, and every state in which **Download** is disabled is what it was before,
  for every input the gateway can produce and every duration both rules agree on.
  The deltas this plan does introduce are the duration rule above,
  and the malformed-answer readings of `offeredProfiles` and `tableOffered`,
  neither of which is reachable from a response `internal/httpapi` writes.

- **`urls.js` stays the only module that spells a `/v1` path.**
  *Rendering response values* forbids a path literal outside it,
  and `TestScanPathsLiveInURLs` holds `app.js` to it (`internal/ui/scan_test.go:87-91`).
  `profileRequest` answers parameters and `app.js` hands them to `profileURL`
  (`internal/ui/static/urls.js:79-88`), so the new module never learns what a URL looks like.
  A scan of its own states it.

- **Membership is read from the whole catalog and never from a filtered menu.**
  `selectionListed()` reads `servicesOf(catalog, ns)` over `state.catalog`
  (`internal/ui/static/app.js:1262-1271`), and it gates the Collections view, the profile URL,
  and the start control.
  `profileRequest` is handed the answer as `listed` rather than computing it,
  which is what lets the module import nothing.
  *Unit* has always specified a scan holding that method to the stored catalog, and no such scan exists:
  `selectionListed` appears in `app.js` and nowhere else in `internal/ui` or `scripts/`.
  The spec now asks the same scan to hold `profileRequest`'s `listed` argument to that method's result,
  because a table handing the parameter both values proves the function and never the caller.
  Task 2 writes it.

- **Each function takes an answer whole, and reaches into it itself.**
  The four that decide what the panel offers take the responses they read:
  `limits`, and for the one that filters by the realm, the whole `/v1/whoami` answer.
  `offeredProfiles(limits, whoami)` reaches the realm the way `pgoRealm` does
  (`internal/ui/static/collectionmodel.js:65-68`),
  because the menu is drawn before either answer has arrived
  and a caller reaching through an absent one throws before the function's own guard can run.
  The one that assembles a request takes the page's state,
  which is the split `deriveControl(pprof)` and `applyInput(state, ...)` already draw
  (`internal/ui/static/portmodel.js:23`, `:75`).

- **A model module imports nothing and mutates nothing.**
  `TestVendorImportFreeModels` holds the four existing modules to no import at all
  (`internal/ui/vendor_test.go:303-314`), where `app.js` and `urls.js` may hold relative ones,
  and every model's table test asserts `Unchanged` on each call
  (`internal/ui/portmodel_test.go:69-104`).
  The fifth module joins both.

- **`app.js` imports every function it uses from a model and calls each one.**
  Three scans hold the page to the three newest models
  (`internal/ui/scan_test.go:132-153`, `:155-175`, `:177-215`, `:217-237`),
  so a page that spells a moved rule again by hand turns the suite red.
  Each of the five needs a surviving call site in `app.js`; the table under Task 1 names them.

- **The export statement names exactly what `app.js` imports.**
  The interpreter cuts the trailing `export` and reads the functions as globals
  (`internal/ui/portmodel_test.go:27-42`, `:44-60`),
  so evaluating a model proves nothing about what a browser can import.
  `TestScanCatalogModelExportsWhatThePageImports` (`internal/ui/scan_test.go:256-289`) is what proves it,
  and `profilemodel.js` gets the same scan.

- **Every `.js` file outside `vendor/` is in the scan inventory.**
  `TestScanCoversEveryJSFile` walks the tree and compares it against `consoleSources()`
  (`internal/ui/scan_test.go:15-17`, `:469-499`).
  The file and its inventory entry are therefore one commit, never two.

---

## Decisions

**Three tasks, split by what each one can prove.**
The four functions that decide what the panel offers are one change:
they read `/v1/limits` and `/v1/whoami` and nothing else, and they move together.
`profileRequest` is a second,
because it rewrites `currentProfileURL` and touches the URL field, **Copy URL**, and **Download** through it.
`tableOffered` is a third, in another module, under another spec section.
Each is green on its own, and each is a diff a reviewer can hold in view.

**The duration repair lands inside the move rather than ahead of it.**
Making `secondsValid` refuse `1e1` is a behavior change and would read as a fix of its own,
landing before the extraction and changing the method in place.
It is folded into Task 1 instead, because `app.js` is executed by no Go test:
a repair landing there first would land with no test that could fail,
and the test proving it would arrive one commit later.
The move is what makes the rule provable, so the move and the repair are the same change,
and Task 1's commit is a `fix` naming the refusal rather than a `refactor` naming the move.

**The module, its inventory entries, its test, and the `app.js` rewiring are one commit.**
`TestScanCoversEveryJSFile` turns red the moment `profilemodel.js` exists outside `consoleSources()`,
and the export scan compares the module's export statement against `app.js`'s import list,
which does not exist until `app.js` is the caller.
There is no smaller green commit than "the module, wired, scanned, and tested".
Task 1 therefore carries seven files, the rule inventory and the CHANGELOG among them; Task 2 carries four.

**A choice the request does not carry is an absent key, which the spec now states.**
Today `currentProfileURL` builds `{pod: "", version: "", port: undefined, portName: undefined}`
and `build` drops every undefined, null, and empty value before it writes the query
(`internal/ui/static/urls.js:13-27`), so the URL is byte-identical under either shape.
The difference is visible only to the table test, because `callModel` round-trips the result through
`JSON.stringify` (`internal/ui/portmodel_test.go:88-93`):
an absent key and an `undefined` one collapse to the same document, an empty string does not.
An absent key makes the returned object *be* the query rather than a description of it,
so the test asserts `{}` and never `{"pod": ""}`.

**`any` is the empty state value and never the text.**
The Pod and version menus carry `<option value="">any</option>` (`internal/ui/static/app.js:1598`, `:1605`),
and `state.pod` and `state.version` start empty (`:447-448`),
so `profileRequest` tests those fields for emptiness and never for the string `any`.
A Pod actually named `any` is an ordinary choice and is sent.

**`defaultSeconds` keeps returning a string.**
It feeds the duration input's value (`internal/ui/static/app.js:625`, `:1101`),
and `secondsValid` reads that value back as text.
`""` for a profile with no bound is what leaves the input empty, and the move preserves both.
`secondsLimit` keeps returning a number, `0` for a profile with no bound,
which is how the page knows to draw no input at all.

**`secondsValid` tests the text, not the number it reads as.**
The repair is a character test before a range test:
every character is `0` to `9`, the value is not empty, and the number those digits spell is `1` to the bound.
That is `parseSeconds` (`internal/httpapi/profile.go:324-337`), transliterated.
Testing `Number.isInteger(Number(seconds))` is what admits `1e1`, `0x10`, ` 10 `, and `+10`,
each of which reads as a whole number and none of which is what the field sends,
because the field sends the text.
`defaultSeconds` returns digits for every bound it can reach,
so no value the page puts in the input is refused by the new rule.

**`asList` stays in `app.js`.**
It has six call sites outside this cluster (`:660`, `:729`, `:773`, `:1351`, `:1353`, `:1355`)
and one inside it (`:1219`), so moving it would drag six unrelated call sites through an import.
`profilemodel.js` carries an array guard of its own instead,
which is what every model module already does with the shapes it reads
(`internal/ui/static/catalogmodel.js:25`, `:41`, `:59`, `:97`).

**`listAllows` and `upstreamSeconds` move whole.**
Each has exactly one use: `listAllows` at `:1219` and `upstreamSeconds` at `:1243`,
both inside the cluster this plan moves.
Nothing in `app.js` reads either afterwards, so both declarations go with the functions that read them.

**`tableOffered` reads a malformed `/v1/whoami` as denial, where the page throws or believes it.**
The inline chain at `:1273-1276` reads `whoami.realm.pgo` without guarding `realm`,
so a body carrying no `realm` throws where `pgoRealm` returns `{}`
(`internal/ui/static/collectionmodel.js:65-68`);
and it reads `.read` for truth where `tableOffered` compares it against `true`,
as `startOffered` and `cancelOffered` already do (`:78`, `:84`),
so a flag arriving as `"false"` or `1` is believed today and denied after.
No valid answer changes:
`realmView.PGO` is a non-pointer struct with no `omitempty`, so `realm.pgo` is always written,
and `pgoFlags.Read` is a Go `bool`, so the field is always `true` or `false`
(`internal/httpapi/listing.go:46-53`, `:55-59`).
What changes is what the page does with a body some other server wrote, and it is a refusal either way.

**`tableOffered` gains a test where there was none, and inherits nothing.**
The rows reading "read alone" and "collect alone" call `startOffered` and assert only its answer
(`internal/ui/collectionmodel_test.go:112-135`);
neither says anything about the table.
The table's half of the rule has been stated in the spec, held in the page, and executed by nothing.

**`startOffered` is not rewritten to read `tableOffered`.**
The spec asks for two functions, not for one reading the other,
and `startOffered` has ten table cases green today (`internal/ui/collectionmodel_test.go:103-135`).
The duplicated `pgo.enabled` and `read` pair between them is what the two cases under Task 3 hold together.

**One CHANGELOG entry, under a section this work opens.**
The duration repair is a behavior an operator sees: a value the field accepted is now refused,
and **Download** stays disabled instead of sending a request the gateway answers `400 invalid_parameter`.
It is a `Fixed` entry.
The 0.6.0 cut replaced `## [Unreleased]` with the release heading rather than leaving the section open
when 0.6.0 was cut, so the first task opens a new one above `## [0.6.0]`.
Nothing else in this plan is an entry:
the move changes no behavior, and the malformed-answer readings are unreachable from this gateway.

**No e2e scenario changes.**
`test/e2e/scenarios_console_test.go` drives the rendered page —
`chooseOption(t, "Profile", "heap")` (`:333`), the **Download** button (`:341`), the URL field (`:467`) —
and names no module, function, or import.
Every behavior it asserts is preserved:
no scenario types a duration at all, so none reaches the value the repair refuses.
The suite is not run for this plan; `go vet -tags e2e ./test/e2e/...` is the compile check each task runs.

**`profileRequest` answers query parameters alone.**
The namespace, the Service, and the profile are the path,
and `app.js` hands those to `profileURL(ns, svc, profile, params)` itself (`:1290`).
`profileRequest` reads `state.profile` — an empty one is what yields `null` — and does not return it.
This was an open question when the plan was first written and is now spec text.

---

## Global Constraints

- **One behavior changes, and it is named.**
  The duration rule of Task 1 is the whole of it, and it carries the plan's only CHANGELOG entry.
  Every other change answers what it answers today.
- **Every task states which existing test proves the behavior it preserved,
  and, for each test it adds, whether that test could have failed before the change.**
  The duration case is shown green against the old rule before the new rule is written,
  because a digits test that never saw `1e1` pass proves nothing about what changed
  ([`300-testing.md`](../../.agents/rules/300-testing.md)).
- **`app.js` spells no `/v1` path**, and neither does `profilemodel.js`
  ([`ui.md`](../specs/ui.md) *Rendering response values*).
- **A model module imports nothing**, holds no top-level `let` or `var`
  (`internal/ui/scan_test.go:501-509`), and ends with one `export { ... };` as its last statement,
  which is the shape `cutExport` requires (`internal/ui/portmodel_test.go:27-42`).
- **Several cases of one shape are named table cases** with `t.Run(tc.name, ...)`
  ([`300-testing.md`](../../.agents/rules/300-testing.md)).
- **No jargon:** comments, commit messages, and documentation state the current fact,
  never this plan's ordering or a task's name
  ([`AGENTS.md`](../../AGENTS.md#no-jargon-anywhere)).
- Markdown prose, Go doc comments, and the comments in the `.js` files use semantic line breaks;
  run `semlf check` on every file a task edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters,
  with a body that says what changed and why, one sentence per line under 120 characters,
  and no trailer of any kind ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
  A commit is finished when `git log --oneline -1` shows it and `git status --short` is clean,
  because the hook can refuse a message after `git commit` has already run.
- **Every task ends with the same block before its commit:**

```bash
mise run lint && mise run test && mise run check && mise run prose
go vet -tags e2e ./test/e2e/...
```

`mise run check` validates the `Status:` line of this file and every relative link in the repository.
`go vet` is the e2e compile check:
`go test -tags e2e -run XXX` is not one,
because `TestMain` builds images and creates and deletes a kind cluster whatever `-run` names.

---

## File Structure

```text
internal/ui/static/profilemodel.js    # offeredProfiles, secondsLimit, defaultSeconds, secondsValid,
                                      # profileRequest; imports nothing, spells no path, builds no URL
internal/ui/static/app.js             # the five become imports; upstreamSeconds, listAllows, and four
                                      # component methods go; currentProfileURL keeps the profileURL call
internal/ui/static/collectionmodel.js # tableOffered beside startOffered
internal/ui/profilemodel_test.go      # the shape assertion and the table over all five functions
internal/ui/scan_test.go              # profilemodel.js in consoleSources; the import, export, and
                                      # path scans; the membership scan Unit specifies and nobody wrote;
                                      # the Collection import scan gains tableOffered
internal/ui/vendor_test.go            # profilemodel.js among the modules that import nothing
internal/ui/collectionmodel_test.go   # tableOffered in the export inventory, and its table cases
.agents/rules/500-validation-and-workflow.md  # five model modules
CHANGELOG.md                          # an Unreleased section, and the duration the console now refuses
docs/plans/console-profile-request-model.md   # this file
```

---

## 1. What the Profile panel offers, and the duration rule repaired

Four functions move out of the page, and one of them changes as it moves.
Three are a move: the same answers for every input the gateway can produce.
The fourth, `secondsValid`, comes to refuse a duration it accepts today —
a value the page offers **Download** for and the gateway then answers `400 invalid_parameter`.
The repair rides here because `app.js` is executed by no Go test,
so this is the first commit in which the rule can be shown red and then green.

Write `internal/ui/profilemodel_test.go` first and run it red — the module does not exist yet,
so `loadModel` fails on the read before it evaluates anything.

- [x] `internal/ui/static/profilemodel.js`, new, with the module comment `portmodel.js` and `catalogmodel.js`
      carry (`internal/ui/static/portmodel.js:1-3`, `internal/ui/static/catalogmodel.js:1-6`):
      it imports nothing and declares plain functions, and a Go test evaluates it with the trailing export cut off.
      - `offeredProfiles(limits, whoami)` keeps `limits.profiles` to what `realm.profiles` admits,
        by the wildcard or by the name, in the order `/v1/limits` gave them.
        It takes the whole `/v1/whoami` answer and reaches the realm itself,
        through a guard shaped like `pgoRealm` (`internal/ui/static/collectionmodel.js:65-68`),
        because the menu is drawn before either answer has arrived.
        A `limits` that is absent, a `whoami` that is absent, one carrying no `realm`,
        and a `profiles` of another shape each offer nothing rather than throwing.
        It carries `listAllows` from `app.js:254-257` unchanged, as a module-local function,
        and its comment keeps naming the gateway's filter it mirrors (`internal/httpapi/realm.go:66-68`).
      - `secondsLimit(limits, profile)` is `Number(limits.cpuSeconds)` for `cpu`,
        `Number(limits.traceSeconds)` for `trace`, and `0` for every other profile and for an absent `limits`.
        A value that reads as no number is `0`, which is the `|| 0` the method already applies (`:1226`, `:1229`).
      - `defaultSeconds(limits, profile)` is `String(Math.min(upstreamSeconds[profile], secondsLimit(...)))`,
        and `""` when there is no bound.
        `upstreamSeconds` comes with it from `app.js:44`,
        and the two comment lines above it (`:42-43`) come too, re-broken at the clause boundary:
        the page sends the lower of the two explicitly,
        so a request never rests on an upstream default a configured limit could sit below.
      - `secondsValid(limits, profile, seconds)` is `true` when there is no bound,
        and otherwise whether the value is decimal digits spelling a number from `1` to the bound.
        Digits first, range second:
        the value is read as text, every character is `0` to `9`, and the text is not empty,
        which is `parseSeconds` transliterated (`internal/httpapi/profile.go:324-337`).
        This is the one function that does not answer what it answers today;
        its comment says the page copies the gateway's grammar
        because the field sends the text that was typed and not the number it reads as.
- [x] `internal/ui/static/app.js`: import the four from `./profilemodel.js`,
      beside the four model imports already there (`:23-37`);
      delete `upstreamSeconds` and its comment (`:42-44`), `listAllows` and its comment (`:254-257`),
      and the four methods (`:1213-1220`, `:1222-1235`, `:1237-1244`, `:1246-1254`).
      Each call site passes what it reads, explicitly:

| Function | Call sites after this task | What replaces `this.state` |
|---|---|---|
| `offeredProfiles` | `:624` (twice, in the `loadLimits` callback) and `:1541` (render) | `offeredProfiles(limits, whoami)` |
| `defaultSeconds` | `:625` (the `loadLimits` callback) and `:1101` (`onProfile`) | `defaultSeconds(limits, profile)` |
| `secondsLimit` | `:1287` (`currentProfileURL`) and `:1543` (render) | `secondsLimit(limits, profile)` |
| `secondsValid` | `:1282` (`currentProfileURL`) and `:1544` (render) | `secondsValid(limits, profile, seconds)` |

- [x] The `loadLimits` callback (`:623-626`) reads `limits` and `whoami` off `this.state` inside the callback,
      where the new `limits` is already applied,
      and computes `offeredProfiles` once into a local rather than calling it twice.
- [x] The call site hands `whoami` whole and reaches into nothing.
      The guard `:1215-1217` runs today disappears into the module,
      which is why the signature takes the answer rather than the realm:
      a call site reaching through an absent `/v1/whoami` throws before the function's own guard can run,
      and that is the case *Unit* asks for when the menu is drawn before the answer has arrived.
- [x] `internal/ui/scan_test.go:15-17`: `profilemodel.js` joins `consoleSources()`,
      which carries it into the HTML-interface, relative-import, inline-form, top-level-state,
      and file-inventory scans in one edit (`:77-85`, `:122-130`, `:459-467`, `:469-499`, `:501-509`).
      Its position in that literal does not matter:
      `TestScanCoversEveryJSFile` sorts both sides before it compares them (`:493-497`).
- [x] `internal/ui/scan_test.go`: `profileModelImportRe` and `TestScanPageUsesProfileModel`,
      the shape `TestScanPageUsesCatalogModel` has (`:217-237`) —
      `app.js` imports every name in `profileModelFunctions` from `./profilemodel.js` and calls each at least once.
      It is written in this task and not the next,
      because the invariant that the page imports and calls what it uses is one this plan holds at every commit,
      and the scan counts no names: it iterates the list, so it is correct at four and again at five.
- [x] `internal/ui/scan_test.go`: `TestScanProfileModelExportsWhatThePageImports`,
      the shape `TestScanCatalogModelExportsWhatThePageImports` has (`:256-289`):
      one export statement, none remaining after `cutExport`,
      the statement naming exactly `profileModelFunctions`,
      and the same set as `app.js`'s import list, compared sorted.
- [x] `internal/ui/scan_test.go`: `TestScanProfileModelBuildsNoURL` —
      `profilemodel.js` holds no `/v1` literal, which is `pathLiteralFindings` (`:44-48`),
      and no occurrence of `profileURL`.
      This is what keeps the URL the caller's and this module's answer parameters,
      and it is written here because the module exists here.
      It could fail: a module that spelled the path itself would trip the first half,
      and one that returned a built URL would trip the second.
- [x] `internal/ui/vendor_test.go:306`: `profilemodel.js` among the modules that import nothing at all.
- [x] `.agents/rules/500-validation-and-workflow.md:93`: five model modules, `profilemodel.js` named.
- [x] `CHANGELOG.md`: a `## [Unreleased]` heading above `## [0.6.0]`, with a `### Fixed` entry
      for the duration the console now refuses.
      It names what an operator sees: a `seconds` of `1e1` disables **Download** and says the bound,
      where the page used to enable it and the gateway answered `400 invalid_parameter`.
      The 0.6.0 cut replaced the previous `Unreleased` heading rather than leaving it open,
      so the section is opened here.

**Tests**

- [x] `internal/ui/profilemodel_test.go`, `profileModelName` and `profileModelFunctions` beside a
      `loadProfileModel` helper, the shape `catalogmodel_test.go:10-27` uses.
      `profileModelFunctions` lists what the module exports, in the order of the export statement:
      four names here, and five once Task 2 adds `profileRequest` to the module and to this list.
- [x] `TestProfileModelShape`: no static import, no dynamic import, exactly one export statement,
      none remaining after `cutExport`, and the export statement naming exactly the module's functions —
      the assertion `TestPortModelShape` makes (`internal/ui/portmodel_test.go:130-147`).
- [x] `TestProfileModelOfferedProfiles`, table cases over one interpreter:
      a realm naming the wildcard offers everything `/v1/limits` offers, in the limits' order;
      a realm naming some of them offers those;
      a realm naming a profile the gateway does not offer adds nothing;
      a realm naming none offers nothing;
      an absent `/v1/limits`, an absent `/v1/whoami`, and a `/v1/whoami` carrying no `realm`
      each offer nothing, since the menu is drawn before both have answered;
      and a `profiles` that is a string, an object, or `null` offers nothing rather than throwing.
      One case uses a limits `profiles` that is not alphabetical, so the order assertion can fail.
- [x] `TestProfileModelSecondsLimit`: `cpu` at its configured bound and `trace` at its own,
      each its own case rather than one standing for the other, since the two carry different bounds;
      a profile with no bound, `heap` among them; an absent `/v1/limits`;
      a bound the body carries as the string `"45"`; and one it carries as `"soon"`, `null`, and `true`.
- [x] `TestProfileModelDefaultSeconds`: the upstream default below the bound (`cpu` at a bound of `60`),
      equal to it (`cpu` at `30`), and above it (`cpu` at `5`, which is the case the explicit parameter exists for);
      `trace` at each of the same three, its own cases because its default is `1` and not `30`;
      and a profile with no bound, which is `""`.
- [x] `TestProfileModelSecondsValid`: the bound itself, `1`, `0`, `-1`, `1.5`, the bound plus one,
      `""`, and `"abc"`, each over a profile that has a bound;
      and every one of those over a profile with no bound, where each is valid because none is sent.
- [x] `TestProfileModelSecondsValid` also drives the spellings that read as a whole number and are not digits,
      each refused over a profile that has a bound:
      `1e1`, `0x10`, `" 10 "`, `"+10"`, and `"10."`.
      `1e1` is the case the repair exists for:
      it passes `Number.isInteger(Number(seconds))` today, and the gateway answers `400 invalid_parameter` for it.
      Run this case against the old rule before writing the new one, and record that it passed;
      a digits test that never saw the old rule green proves nothing about what changed.
- [x] Each case asserts `Unchanged`, so none of the four mutates its input.
- [x] Each of these could have failed before this task, because none of the four rules had a test at all:
      `app.js` is not evaluated by any Go test,
      and no browser scenario types a duration, so none reaches a bound, a fraction, or `1e1`.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
go vet -tags e2e ./test/e2e/...
git log --oneline -1 && git status --short
```

- [x] Commit: `fix(ui): refuse a duration the gateway refuses`.
      The body names both halves: the four rules move into a module a test executes,
      and `secondsValid` comes to read decimal digits, so the page stops offering a download for `1e1`.
      It is a `fix` and not a `refactor` because the refusal is what an operator sees.

---

## 2. What the profile request carries

One function moves, and with it the rule behind the URL field, **Copy URL**, and **Download**.
Those three read one predicate today and must go on reading one.
The scan *Unit* has always specified and nobody wrote is written here too,
because membership reaching the module as a parameter is what leans on it.

- [x] `internal/ui/static/profilemodel.js`: `profileRequest(state, portParams, listed)`.
      It returns `null` when `listed` is false, when `state.profile` is empty,
      or when `secondsValid(state.limits, state.profile, state.seconds)` is false.
      Otherwise it returns the query: `pod` and `version` from `state` when each is non-empty,
      whichever of `port` and `portName` `portParams` carries,
      and `seconds` only when `secondsLimit(state.limits, state.profile)` is non-zero.
      A key it does not send is absent from the object, never present and empty,
      which is what makes the answer the query rather than a description of it.
      `any` is the empty value the menus' first option carries (`internal/ui/static/app.js:1598`, `:1605`),
      so the test is emptiness and never the string `any`,
      and a Pod actually named `any` is an ordinary choice that is sent.
      It returns no path segment: the namespace, the Service, and the profile are `app.js`'s to hand to `urls.js`.
      It reads `state.limits`, `state.profile`, `state.seconds`, `state.pod`, and `state.version`, and nothing else,
      and it copies out of `portParams` rather than returning it, so no caller's object is shared or changed.
      Its comment says why it builds no URL:
      `urls.js` is the only module that spells a `/v1` path, and `app.js` hands these parameters to it.
- [x] `internal/ui/static/app.js`: import `profileRequest` from `./profilemodel.js`.
      `currentProfileURL` (`:1278-1291`) becomes the call and the build:
      `profileRequest(this.state, this.portChoice(), this.selectionListed())`,
      `null` when that is `null`, and `profileURL(ns, svc, profile, params)` otherwise.
      `portChoice()` (`:1256-1260`), `selectionListed()` (`:1262-1271`), and `downloadAllowed()` (`:1171-1178`)
      stay as they are, and every caller of `currentProfileURL` (`:1161`, `:1177`, `:1190`, `:1545`) is untouched.
- [x] `secondsValid` keeps a call site in `app.js` after this task — `:1544`, the render —
      and `secondsLimit` keeps `:1543`, so the import scan stays green.
      Both lose their `currentProfileURL` use to `profileRequest`, which is the point of the move.
- [x] `internal/ui/profilemodel_test.go`: `profileModelFunctions` gains `profileRequest`,
      in the position the module's export statement puts it.
      That one edit carries the name into the shape assertion, the import scan, and the export scan,
      each of which iterates the list rather than counting it,
      so the three written in the first task need no change here.
      `TestScanProfileModelBuildsNoURL` gains nothing and is what holds this task's new function
      to answering parameters rather than a URL.
- [x] `internal/ui/scan_test.go`: `TestScanMembershipReadsTheWholeCatalog`, which *Unit* specifies
      and which does not exist — `selectionListed` appears in `app.js` and in no test or script.
      It cuts the method's body out of `app.js` the way `refetchBodyRe` cuts the refetch method's (`:428-431`),
      failing rather than passing when it cannot cut one,
      and asserts the body names `catalog` and `servicesOf` and none of
      `filterOptions`, `nsOptions`, `svcOptions`, `nsFilter`, or `svcFilter` —
      the filtered lists the menus are drawn from (`internal/ui/static/app.js:1389-1390`).
      It then holds the caller: `profileRequest(` appears exactly once in `app.js`,
      and that occurrence matches `profileRequest(this.state, this.portChoice(), this.selectionListed())`,
      so the membership the module is handed is the one the page computed and nothing else.
      A table handing `listed` both values proves the function, never the caller,
      which is the obligation taking membership as a parameter creates.

**Tests**

- [x] `internal/ui/profilemodel_test.go`, `TestProfileModelProfileRequest`, table cases over one interpreter,
      with a state fixture helper the way `limitsWith` and `whoamiWith` serve the Collection model
      (`internal/ui/collectionmodel_test.go:94-101`):
      - a complete listed selection with a bound profile yields the Pod, the version, the port selection
        handed in, and `seconds`;
      - the same with a profile that has no bound yields no `seconds`;
      - a numeric port, a named port, and the default each yield their own answer,
        the default carrying neither `port` nor `portName`;
      - a Pod chosen with the version left at `any`, and the reverse, each send one key and not the other;
      - `any` for both sends neither, and each key is absent rather than empty;
      - a Pod named `any` in the menu is sent, because the test is the empty value and never the text;
      - `listed` false yields `null`;
      - an empty `profile` yields `null`;
      - a duration of `0`, of the bound plus one, of a fraction, of `1e1`, and of a non-numeric string
        each yield `null`, since `profileRequest` reads `secondsValid`;
      - a duration outside the bound on a profile with no bound yields a request, because none is sent;
      - no answer carries a namespace, a Service, or a profile.
- [x] Each case asserts `Unchanged`, so neither `state` nor `portParams` is written to.
- [x] One case states the invariant this task exists for:
      the three controls read one predicate.
      Over a state whose duration is out of bounds, `profileRequest` is `null`,
      which is what empties the URL field, hides **Copy URL**, and disables **Download** —
      three renderings of one answer.
- [x] These could have failed: the browser scenario reads the URL field (`test/e2e/scenarios_console_test.go:467`)
      but drives no out-of-bound duration, no absent Pod with a present version, and no unbounded profile.
- [x] The membership scan could have failed and would have, on the day it was specified:
      show it red by pointing its caller assertion at a `profileRequest` call
      that passes `true` instead of `this.selectionListed()`, then restore the call.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
go vet -tags e2e ./test/e2e/...
git log --oneline -1 && git status --short
```

- [x] Commit: `refactor(ui): model the profile request's query`.
      The body says the three controls come to read one answer,
      and that the scan holding membership to the whole catalog is written for the first time.

---

## 3. The Collections table's half of the `pgo` rule

The spec states the rule and the module explains it; the page holds it and nothing executes it.
This task moves it, which also makes the two halves read the realm the same way.

- [x] `internal/ui/static/collectionmodel.js`: `tableOffered(limits, whoami)`,
      declared beside `startOffered` (`:70-79`) and above it,
      since the table is what the start control sits on.
      It is `pgo.enabled === true && pgoRealm(whoami).read === true`,
      reading `limits.pgo` and `pgoRealm` exactly as `startOffered` does (`:65-68`, `:76-77`).
      Its comment says an absent `/v1/whoami`, one carrying no realm,
      and one whose realm carries no `pgo` block each admit nothing,
      and that a flag arriving as anything but `true` is denial.
- [x] `internal/ui/static/collectionmodel.js:404`: the export statement gains `tableOffered`
      in the position it is declared in.
- [x] `internal/ui/static/app.js`: import `tableOffered`;
      `collectionsOffered()` (`:1273-1276`) becomes `return tableOffered(this.state.limits, this.state.whoami);`
      and keeps its three call sites (`:741`, `:751`, `:1315`), which is the Collections fetch and the table.
      Name the contract in the body of the commit message, not in a comment:
      no valid answer changes, because the gateway writes both flags as booleans
      (`internal/httpapi/listing.go:46-53`, `:55-59`);
      what changes is that a malformed answer offers nothing
      instead of throwing on an absent `realm` or believing a flag that is not `true`.
- [x] `internal/ui/collectionmodel_test.go:17-29`: `collectionModelFunctions` gains `tableOffered`
      in the same position as the export statement, which `TestCollectionModelShape` compares exactly (`:57-76`).
- [x] `internal/ui/scan_test.go:194-206`: the function list of `TestScanPageUsesCollectionModel` gains
      `tableOffered` and `shortTime`, and the doc comment above it (`:181-187`) counts thirteen
      imported functions and names `retryAfterSeconds` as the fourteenth export the page does not import.

**Tests**

- [x] `internal/ui/collectionmodel_test.go`, `TestCollectionModelTableOffered`, table cases beside
      `TestCollectionModelStartOffered` (`:103-135`) and reusing `limitsWith` and `whoamiWith` (`:94-101`):
      both hold; `pgo.enabled` false; `read` false; an absent `/v1/limits`; an absent `/v1/whoami`;
      a limits body with no `pgo` block; a whoami body with no `realm` block;
      and a realm with no `pgo` block.
      Each asserts `Unchanged`.
- [x] A `read` that is not a boolean — `"false"`, `1`, and `"true"` — offers nothing,
      which is the reading the page did not make.
- [x] Two cases state the pair the spec names:
      `read` alone yields the table and no start control,
      and `collect` alone yields neither — asserted by calling both functions over one whoami,
      which is what makes the pair a property of the module rather than of two separate tables.
      The existing rows of those names call `startOffered` and assert only its answer
      (`internal/ui/collectionmodel_test.go:112-135`),
      so these two cases are new work and inherit nothing.
- [x] Every case here could have failed, because nothing executed this rule before:
      `tableOffered` has no predecessor in the module,
      and the page's chain at `:1273-1276` is run by no Go test.
      The whoami-with-no-realm case is the sharpest of them: that chain throws on that body.

**Validation**

```bash
mise run lint && mise run test && mise run check && mise run prose
go vet -tags e2e ./test/e2e/...
git log --oneline -1 && git status --short
```

- [x] Commit: `refactor(ui): model the Collections table rule`

---

## 4. The guide, and the close

- [ ] `docs/console.md:73`: the duration field takes whole numbers,
      which is what the gateway accepts for `seconds`.
- [ ] `Status:` becomes `Done` and line 4 gains `**Outcome:**` naming the pull request that carried the tasks,
      in the same change that lands the last of them
      ([`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)).
      It names the pull request and never a commit, because the merge rebases the branch
      and rewrites every hash on it.
- [ ] The next change that touches this file deletes it, per the same rule.

**Validation**

```bash
mise run check && mise run prose
git log --oneline -1 && git status --short
```

- [ ] Commit: `docs: close the Profile panel's model module`

---

## What this plan does not cover

- **The browser scenarios are not re-run here.**
  `mise run test:e2e` needs a kind cluster and a headless Chromium,
  and no scenario changes, so the plan names `go vet -tags e2e ./test/e2e/...` as its compile check
  and leaves the suite to the pull request's own run.
- **`app.js` is still not executed by a unit test.**
  Five functions leave it; the wiring that calls them, the render that reads the answers,
  and the state transitions around them stay proven by review and by the two browser scenarios
  ([`ui.md`](../specs/ui.md) *What is not proven*).
  The membership scan Task 2 writes reads `app.js` as text and runs none of it,
  which is the most a scan can do and the reason *Unit* calls it a scan.
- **The gateway's own duration grammar is untouched.**
  `parseSeconds` refuses what it always refused (`internal/httpapi/profile.go:324-337`);
  this plan changes only which values the console offers to send it,
  so a client other than the console sees nothing.
- **The order of the profile menu is asserted against `/v1/limits`'s order, not against the gateway's.**
  `config.Profiles()` is what fills that field (`internal/httpapi/listing.go:195`);
  a change to its order would move the menu and no test here would notice.
- **The wiring of the duration input's `min` and `max` is not moved.**
  The bound itself is a table case after Task 1;
  what stays unproven by a unit test is that the render passes `secondsLimit`'s answer into those two attributes,
  which is proven by review and the browser scenarios, as the rest of the render is.
