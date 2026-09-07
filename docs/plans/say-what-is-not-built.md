# The Spec Says What Is Not Built

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.
> On this machine `mise run lint` runs a golangci-lint 2.1.6 that shadows the pinned 2.12.2,
> so every validation block below runs the linter as `mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`
> and never as `mise run lint`.
> `git log --oneline -1` after every commit:
> the `semlf` `commit-msg` hook exits `0` and lands nothing when it refuses a message.

**Goal:** make a reader of the accepted PGO design able to tell what this build carries from what it does not,
without knowing that a decision record exists,
and take out of the code the one artifact of the deferred design that shipped anyway.

`docs/specs/pgo.md` *Collector availability* defines a `collector.<instance>` heartbeat and a freshness rule,
the `profgate_pgo_collector_available` gauge, an alert over it,
and a `503 collector_unavailable` refusal on `POST /collections`.
None of it is built.
The collection loops run in every `profgate serve` replica with `pgo.enabled`,
and the deferral is recorded only in
[`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md),
which a reader of the spec has no reason to open.
`internal/httpapi/codes.go:90-92` carries `CodeCollectorUnavailable` with a comment saying no route answers it,
`internal/httpapi/openapi.json:1130` enumerates the value,
`internal/ui/static/app.js:71` writes a hint for it,
and `internal/ui/static/collectionmodel.js:217` branches on it,
so the console holds a rule for an answer it can never receive,
and every client reading the document is told to expect a code nothing sends.

`docs/pgo.md` contradicts itself about the same routes:
`:145-147` says the Service listing pages, with `nextCursor` to walk it,
and `:261-264` says it "holds at most the newest 100 records and offers no pagination".
The route pages.

After this plan the spec says once, under *Overview*, that the separation is not built,
and says it again wherever the deferral changes what a reader would conclude about this build.
`503 collector_unavailable` is named below the error table as a code this build does not register,
and the console's start table and hints table carry no row for it.
The registry entry, the OpenAPI enum value, the console hint, and the console branch are gone with their tests.
The guide's registered-code count is right and its listing paragraphs agree with each other,
and `CHANGELOG.md` carries the enum removal as the breaking change it is.
No route, configuration key, chart value, Kubernetes call, or NATS permission changes.

**Architecture:** `docs/specs/pgo.md` gains a *What this build does not carry* subsection under *Overview*,
which scopes the deferral for the whole document,
and then a local marker wherever the deferral changes what a reader would conclude about this build:
the core decision that names two kinds of process, the three loops under *Architecture*,
the collector memory limit under *Container*, the watch table and the two heartbeat tables under *NATS Access*,
the head of *Collector availability*, the `collector.*` sweep row,
the collector check in *Create a Collection* and again where the replay lookup names it,
the collector Deployment under *Deployment*, the heartbeat delete under *Shutdown*,
the collector cases under *Unit*, the harness and the three collector scenarios under *End to end*,
the three unbuilt entries under *Package Layout*,
and the rows of *Failure Scenarios* that turn on an absent collector.
It also gains an *Errors* table without the code and a paragraph below it saying the code is not registered,
and an *Amendments* block.
`docs/specs/ui.md` loses the code from *Starting and cancelling a Collection*, *Errors*, and *End to end*,
which makes "any other `5xx`" the whole rule for a key that survives, and gains an *Amendments* row.
`internal/httpapi` loses the constant, the registry entry, and the enum value,
and `codes_test.go`'s `specCodes` follows the table it transcribes.
`internal/ui/static/collectionmodel.js` loses the `503` branch and the comment sentence naming it,
and `internal/ui/static/app.js` loses the hint.
`internal/ui/collectionmodel_test.go` loses one table case and substitutes a classifying answer in another test,
and `internal/ui/scan_test.go`'s `hintCodes` and its comment follow the hints table.
`docs/deployment.md` corrects the count and drops the sentence about the unanswered code.
`docs/pgo.md` corrects the second listing paragraph.
Nothing under `internal/` changes an answer a client can observe, because no route answered the code being removed.

**Spec:** the collector design runs through [`pgo.md`](../specs/pgo.md) —
*Overview*, *NATS Access*, *Collector availability*, *Create a Collection*, *Deployment*,
*Errors*, *Unit*, *End to end*, and *Failure Scenarios* —
revised by tasks 1 and 2 of this plan.
The console's rules are *Starting and cancelling a Collection*, *Errors*,
and *End to end* of [`ui.md`](../specs/ui.md);
task 3 revises them.
The guide corrections have no spec;
they are the roadmap's own, under *Say in the spec what is not built* (`docs/plans/roadmap.md:346-359`).
Rules in force:
[`.agents/rules/`](../../.agents/rules/).

---

## Invariants

Each task below exists to hold one of these.
They are stated as properties of the repository, not as the defects that revealed them.

- **A reader of an accepted spec can tell what the build carries from what it does not, from the spec alone.**
  *Metrics* already does this for the collector gauge (`docs/specs/pgo.md` *Metrics*):
  "No process exports it in this build: it arrives with the collector Deployment,
  and the sentences above describe it from that point on."
  *Collector availability* and the collector check in *Create a Collection* carry no such sentence,
  and the decision that defers them lives outside the spec (`docs/decisions/collection-stays-in-the-gateway.md`).

- **A registered envelope code is one a route can answer.**
  `internal/httpapi/codes.go:90-92` registers `collector_unavailable`,
  and says in its own comment that no route answers it.
  `EnvelopeCodes()` is what `internal/httpapi/openapi_test.go:243` compares the document's `Error.code` enum against,
  so the unanswerable code is published to every client (`internal/httpapi/openapi.json:1130`).

- **The console holds a rule for each answer it can receive,
  and for no other.**
  `internal/ui/static/collectionmodel.js:217` branches on `503 collector_unavailable` to drop the idempotency key,
  and `internal/ui/static/app.js:71` writes its hint;
  neither can run while no route answers the code.

- **A hand-transcribed list matches the table it transcribes.**
  `internal/httpapi/codes_test.go:14-16` says the registry is compared against the two error tables written out,
  and `specCodes()` (`:40`) transcribes the `503` row of `docs/specs/pgo.md` *Errors*.
  `internal/ui/scan_test.go:216-243` holds the hints vocabulary of `docs/specs/ui.md` *Errors*,
  and states its size in a comment ("the eighteen codes its sixteen rows name").

- **A number the prose states about the code is a number that is right.**
  `docs/deployment.md:489` says forty values are registered envelope codes;
  `envelopeCodes` holds forty (`internal/httpapi/codes.go`), and thirty-nine once the code goes.

- **A guide does not contradict itself.**
  `docs/pgo.md:145-147` says the Service listing shows 100 records to a page,
  with `nextCursor` to page through the rest;
  `:261-264` says the same listing "offers no pagination".
  `internal/httpapi` pages it.

---

## Decisions

**A spec edit rides with the change that makes it true, not with the other spec edits.**
The seam between the commits is what is true when each lands, not spec against code.
Saying at the head of *Collector availability* that nothing in it is built is true today, so it lands alone.
Saying `503 collector_unavailable` is not a registered code is false until the registry loses it,
so that edit rides with `internal/httpapi`.
Saying the console has no rule for it is false until `collectionmodel.js` loses the branch,
so `docs/specs/ui.md` rides with `internal/ui`.
A spec that is false on the commit that lands it is what rule 000 calls a bug in the document
(`.agents/rules/000-agent-contract.md`).

`pgo.md`'s *Amendments* convention already draws this line:
its first table is "the edits made in the same change as this section",
and its second is what is "updated when the implementation lands".
So the whole amendment block lands in task 1,
with the edits that are true then in the first table and everything tasks 2 and 3 carry in the second.

**`pgo.md` keeps every sentence of the collector design;
`ui.md` loses its rows.**
The two documents are in different positions.
`pgo.md` describes a design that is accepted and unbuilt, so the *Metrics* treatment applies:
keep the text, say at its head that none of it is built.
`ui.md` describes what the console does, and after task 3 the console does nothing for this code —
so a row saying the key is dropped on `503 collector_unavailable` would be a spec that disagrees with the code,
which rule 000 calls a bug in the document.
Its rows go, and the clauses that carved the code out of "any other `5xx`" collapse.

**The decision record is left as it stands.**
`docs/decisions/collection-stays-in-the-gateway.md:104` records, as a consequence,
that `collector_unavailable` stays a registered code no route answers.
Decision records are immutable once accepted and superseded rather than edited (`docs/README.md`),
and that consequence was true when the record was written.
The roadmap is the later approved decision, and the spec revision is where the new state is recorded.
Nothing in the record's *Context* or *Revisit* is affected: the separation is still deferred,
and rebuilding it still costs what the record says it costs.

**The `503` row of the *Errors* table loses the code,
and a paragraph below the table carries it.**
`internal/httpapi/codes_test.go`'s `specCodes()` transcribes that table by hand,
so a code left in the table and removed from the registry would be a disagreement no check can catch.
That is why the table edit and the registry edit are one commit.
Below the table the code is prose, which nothing transcribes,
and the paragraph that already explained why it is a code of its own keeps doing so,
for the design that returns with the collector Deployment.

**One test substitutes a classifying answer rather than losing a case.**
`TestCollectionModelKeySurvivesLostAnswers` reads `Keep`, `Phase`, `Key`,
and `Token` and never `Refetch`, so the substitute's refetch list does not enter the assertion.
`internal/ui/collectionmodel_test.go:826-841` drives the whole sequence "a series of unclassified `5xx` keeps one key,
then a classified answer drops it and the next arm gets a new key",
and it uses `503 collector_unavailable` as the classifying answer.
The sequence is the assertion, not the code:
`403 realm_denied` classifies, drops the key, and refetches `whoami`,
and `429 collection_in_progress` classifies and refetches the list.
`501 pgo_disabled` is the substitute, because it classifies, drops the key, and is reached by the same `POST` —
and because the test asserts on `Keep` alone.
The separate table case at `:386-388` has no such role and is removed.

**`docs/api.md` needs no edit.**
`docs/specs/pgo.md` records an amendment
that would have added `503 collector_unavailable` to `docs/api.md` on `POST /collections`.
That edit never landed:
`grep -n collector_unavailable docs/api.md` finds nothing.
The guide is already right and is left alone.

**`docs/pgo.md`'s first listing paragraph is right and the second is wrong.**
`internal/httpapi` answers the Service listing with `nextCursor`,
and `profgate collections` walks every page (`docs/cli.md` *Collections*).
So `:261-264` is corrected to say the listing pages,
and that a record stays readable at `GET /v1/collections/{id}` regardless,
which is the point that paragraph is making.

---

## Tasks

### 1. Say in the spec that the collector design is not built

- [ ] `docs/specs/pgo.md` *Overview* gains a *What this build does not carry* subsection:
      one process runs the scheduler, the worker, and the sweeper;
      no collector Deployment is built and `profgate collector` is not a subcommand;
      no process writes or reads a `collector.<instance>` heartbeat;
      no process exports `profgate_pgo_collector_available` and the chart renders no alert over it.
      It names [`collection-stays-in-the-gateway.md`](../decisions/collection-stays-in-the-gateway.md)
      as where the reasoning and the conditions that revisit it live.
      It says everything else the document specifies is built,
      names what a reader discounts until the separation ships,
      and says a section repeats this in its own words
      where the deferral changes what a reader would otherwise conclude about this build.
      It does not say `collector_unavailable` is unregistered: that is false until task 2.
- [ ] A local marker lands where the deferral changes what a reader would otherwise conclude about this build —
      a chart that computes a figure, a test that runs, a harness that deploys, a sweep that deletes —
      and nowhere else.
      The *Overview* subsection is the authority;
      a marker is a courtesy to a reader who arrives at a section directly.
      The sites: the core decision naming two kinds of process; the three loops under *Architecture*;
      the collector memory limit under *Container*;
      the watch table under *NATS Access*, followed by the four watches this build's one role opens;
      the liveness row of the atomicity table and the heartbeat access table; the head of *Collector availability*;
      the `collector.*` row of the *Sweeper* table;
      the collector check in *Create a Collection*,
      and again where the replay lookup lists it among the steps a replay skips;
      the collector Deployment under *Deployment*; the draining collector's heartbeat delete under *Shutdown*;
      the `collect` cases, the collector manifest assertions, the heartbeat cases,
      and the collector-cache create cases under *Unit*;
      the harness's collector Deployment and the three collector scenarios under *End to end*,
      whose lane sentence stops claiming they run;
      the three unbuilt entries under *Package Layout*;
      and the rows of *Failure Scenarios* that turn on an absent collector, its heartbeat,
      or a `pgo.enabled` manifest with no collector Deployment.
- [ ] No marker is added to the design reasoning that merely names the collector —
      *Permission Boundary*, *Slots*, the replica-count argument under *Architecture*.
      Those describe the design, not a claim about what runs, and the *Overview* subsection covers them.
      Annotating every mention is what rule 900 calls patching.
- [ ] The *Failure Scenarios* note says which rows stay reachable:
      a rollout under a running Collection turns on the lease, the claim, and the reclaim,
      and all three run in the gateway.
      It states no count of rows; a count is a number nothing checks.
- [ ] `docs/specs/pgo.md` gains one *Amendments* block at the end of the document.
      Its first table — the edits made in this change — holds the markers above.
      Its second table — updated with the implementation —
      holds what tasks 2 and 3 carry:
      the *Errors* edits, the `docs/specs/ui.md` edits, `internal/httpapi`, `internal/ui`, `docs/deployment.md`,
      and `CHANGELOG.md`.
- [ ] New text cites a section by its name, never by its number:
      `AGENTS.md` forbids citing a document's internal section numbers.
      The numbered references already in the document are left alone.
- [ ] No historical *Amendments* row is rewritten: those record earlier designs,
      and rewriting one records process rather than fact.

**Validation:** `mise run check`;
`mise run prose`.
The task edits Markdown alone, so no linter or test reads anything it changed.
Every sentence this task lands is true of the tree it lands in:
no heartbeat, no gauge, no alert, no second Deployment, and a registry that still holds the code,
which this task never denies.

**Commit:** `docs(specs): say the collector design is unbuilt`

### 2. Take the unanswerable code out of the contract

The spec edits here are false until the registry loses the code, so they land in this commit and not task 1.

- [ ] `internal/httpapi/codes.go` loses `CodeCollectorUnavailable` and its comment,
      and `envelopeCodes` loses the entry, leaving thirty-nine.
- [ ] `internal/httpapi/codes_test.go`'s `specCodes()` loses `"collector_unavailable"`.
- [ ] `internal/httpapi/openapi.json` loses `"collector_unavailable"` from the `Error.code` enum.
- [ ] `docs/specs/pgo.md` *Errors*:
      the `503` row of the status table is `pgo_unavailable` alone,
      and the `Retry-After` sentence names the three codes that remain.
      The paragraph below the table says `503 collector_unavailable` is not in the table
      and is not registered in this build,
      arrives with the collector Deployment, and joins the `503` row then —
      followed by the existing reasoning for why it is a code of its own, restated as the design that returns with it.
- [ ] `docs/specs/pgo.md` *Overview* and *Collector availability* add the clause task 1 held back:
      `collector_unavailable` is not a registered envelope code.
- [ ] `docs/deployment.md` says thirty-nine values are registered envelope codes,
      and drops the sentence saying `collector_unavailable` is one of them
      and never appears on a series.

**Validation:** `mise run check`;
`mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`;
`mise run test`;
`mise run prose`.
`TestEnvelopeCodesMatchTheErrorTables` and `compareCodes` in `openapi_test.go` are what prove this task:
the first holds the registry to `specCodes()`, a hand-written list this task edits to match the table it transcribes,
so the list and the table are checked by review and the list and the registry by the test;
the second holds the document's enum to the registry, so the two removals cannot land apart.

**Commit:** `refactor(httpapi): drop the code no route answers`

### 3. Take the console's rule for it out too

The `docs/specs/ui.md` edits are false until the console loses its branch, so they land here.

- [ ] `internal/ui/static/collectionmodel.js` loses the `status === 503 && code === "collector_unavailable"` branch,
      and the comment above `startOutcome` says the three outcomes that keep the key
      are a rejected fetch, a `503 pgo_unavailable`, and any other `5xx`.
- [ ] `internal/ui/static/app.js` loses the `collector_unavailable` hint.
- [ ] `internal/ui/collectionmodel_test.go` loses the `"503 collector_unavailable drops the key"` table case,
      and the classified answer in the key-lifetime test becomes `501 pgo_disabled`,
      with its message and its assertion text following.
- [ ] `internal/ui/scan_test.go`'s `hintCodes` loses the code, leaving nineteen entries,
      and its comment reads "the seventeen codes its fifteen rows name, plus `too_many_auth` and `auth_unavailable`".
      The assertion and its message near the end of the same test count the vocabulary too;
      both move from twenty to nineteen.
      Recount against `docs/specs/ui.md` *Errors* as this task leaves it rather than trusting these figures:
      before the removal the table has eighteen data rows naming twenty codes, two rows naming two codes each,
      and the comment's count excludes the two authentication rows.
- [ ] `docs/specs/ui.md` loses `503 collector_unavailable` from three places:
      *Starting and cancelling a Collection*, in the retained-state sentence and the start-outcome table;
      the *Errors* hints table; and the *End to end* key rules.
      "Any other `5xx`" is the whole rule in each place afterwards,
      and the document gains one *Amendments* row for the four edits.

**Validation:** `mise run check`;
`mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`;
`mise run test`;
`mise run prose`; and `mise run test:e2e` on a machine with a browser installed.
`.agents/rules/500-validation-and-workflow.md` requires the suite for a change under `internal/ui/static/`,
because `app.js` runs for real only in `console-oidc` and `console-basic`,
which drive a headless Chromium and skip by name where none is installed.
`PROFGATE_E2E_KEEP=1` keeps the kind cluster for the next run.
`TestScanHintsNameEveryCode` is what proves the hint and its vocabulary moved together.

**Commit:** `refactor(ui): drop a rule for an unanswered code`

### 4. Make the guide's listing paragraphs agree

- [ ] `docs/pgo.md:261-264` says the listing holds the newest records a page at a time,
      rather than "at most the newest 100 records and offers no pagination",
      keeping the paragraph's point that a record stays readable at `GET /v1/collections/{id}`
      after it leaves the page.

**Validation:** `mise run check`;
`mise run prose`.
The task edits one guide paragraph, so no linter or test reads anything it changed.

**Commit:** `docs(pgo): say the listing pages`

### 5. Record the removal and close the item

- [ ] `CHANGELOG.md` gains a `### Removed` section under `[Unreleased]`,
      between `### Changed` and `### Fixed` as Keep a Changelog orders them.
      It says `collector_unavailable` leaves the `Error.code` enum of `/v1/openapi.json`,
      that no route in this build ever answered it,
      and that a client generated from the document will no longer accept it —
      the breaking change to the enum this release note exists to carry.
- [ ] `docs/plans/roadmap.md` ticks both bullets of *Say in the spec what is not built*,
      and its `Shipped:` line names the pull request.
- [ ] This plan's `Status:` becomes `Done`,
      and line 4 becomes an `Outcome:` line naming the pull request, in this same commit.

**Validation:** `mise run check`;
`mise run prose`;
`mise exec golangci-lint@2.12.2 -- golangci-lint run ./...`;
`mise run test`.

**Commit:** `docs: record the enum removal and close the item`

### 6. Delete this plan

- [ ] Delete `docs/plans/say-what-is-not-built.md`.
      Nothing links to it but itself, so the deletion rewrites no link —
      confirm that with `grep -rn say-what-is-not-built .` before deleting, and rewrite whatever it finds.
- [ ] This is the commit after the one that set `Done`, never the same commit:
      a commit's tree either holds the finished document under the checks or does not
      (`.agents/rules/900-design-and-review-loops.md`).

**Validation:** `mise run check`, whose link check is what proves nothing still cites the file.

**Commit:** `docs: retire the unbuilt-collector plan`

---

## What This Plan Does Not Do

- It does not build the collector Deployment, the heartbeat, the gauge, or the alert.
  The decision to defer them stands, with the record that argues it and the conditions that revisit it.
- It does not edit `docs/decisions/collection-stays-in-the-gateway.md`.
- It does not touch `TestRoundsDecodeHeapDelta`, `PGODecodeFactor`, or `PGOMemoryBytes`,
  which are the roadmap's next item.
- It changes no answer an e2e scenario reads.
  Task 3 still runs the suite, because it edits `internal/ui/static/` and the rule requires it there;
  what the run proves is that the console still behaves as the two browser scenarios assert.
