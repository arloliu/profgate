# A Failed Watch Open Cleans Up After Itself

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> The first task is test-first, and it begins by giving the test fixture the control its case needs,
> so its first run fails on assertions rather than on compilation.
> Checkboxes (`- [ ]`) track progress.
> The accepted specs outrank this plan:
> where the two differ the spec wins and the plan is the bug.

**Goal:** stop `Caches.Run` walking away from watches it opened.
When one `Watch` fails, the consumers already started keep running under the caller's context,
and the reopen adds another consumer for each of those prefixes every time it is tried.
After this change a failed open cancels what it started and waits for it,
so a process that reopens its caches has one consumer per prefix rather than a growing number.

**Architecture:** the change is confined to one function.
`internal/pgo/caches.go` opens every watch under a context derived from the caller's,
cancels that context on the failed-open path,
and waits for the consumers it started before it returns the error.
No other file in `internal/pgo` changes,
and `cmd/profgate/serve.go` is not edited:
`runCaches` (`:729-742`) already answers a returning `Run` by reopening.

**Tech Stack:** everything already pinned in [`mise.toml`](../../mise.toml).
**No Go module is added.**

**Spec:** [`docs/specs/pgo.md`](../specs/pgo.md), `Accepted`, is unchanged by this plan.
Nothing here alters what a watch delivers, when a barrier lifts, or what a generation means.
This work is ordered by [`docs/plans/roadmap.md`](roadmap.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

## Global Constraints

- **The permission invariant does not move.**
  No task edits `internal/k8s`, `deploy/`, or the NATS account fragment, and no key or prefix is added.
- **The NATS seam gains no method and is not edited.**
  `internal/natskv` is untouched.
- **No behavior visible to a client changes.**
  No route, error code, metric, or configuration key is added, removed, or renamed.
- **No cache is cleared, and no barrier is shut, by this plan.**
  That is a deliberate limit and the next section gives the reason.
- No jargon: code comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a review round, or a task name.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

- Markdown prose uses semantic line breaks; run `semlf check <file>` on every Markdown file a task writes or edits.
- Commit headers are Conventional Commits under 50 characters, with no trailer of any kind.

---

## What This Plan Decides

### The leak is real, and it is the only thing in `Caches.Run` that is

`Caches.Run` (`internal/pgo/caches.go:282-315`) opens four watches in a loop.
A `Watch` that fails returns the error from inside that loop (`:303-305`),
reaching neither the `wg.Wait()` at `:312` nor any cancellation,
so every consumer already started keeps running under the caller's context.
`runCaches` (`cmd/profgate/serve.go:729-742`) answers a failure by calling `Run` again,
which opens a second watch for each of those prefixes;
every later attempt adds another consumer feeding the same cache.

### A watch that is cut does not reach `Caches.Run` at all

This is why the plan stops at the leak.

The channel `Caches.consume` reads belongs to `runWatch`, not to a NATS watcher.
When the underlying watcher closes, `consumeWatcher` reports it as a reconnect rather than as completion
(`internal/natskv/client.go:636-638`),
and `runWatch` stops that watcher and opens another **without returning**
(`internal/natskv/client.go:596-611`),
so its `defer close(ch)` at `:585` does not run.
The consumer never finishes, `Run` never returns, and there is nothing in `Caches.Run` to repair:
a cut watch is invisible at this boundary by design.

### Clearing a cache under a live generation would break a reader that already passed the barrier

The second reason to stop here.

`Session` (`internal/pgo/runtime.go:112-137`) checks `Caches.Synced(gen)` once
and hands back a session bound to `View(gen)`.
A route reads the cache later without rechecking:
`serveCollectionList` calls `sess.Collections(...)` at `internal/httpapi/pgo_collections.go:413`
and answers from whatever it finds.
So clearing the maps while `gen` stays current turns an admitted request into a false empty page,
and a create into one that layers defaults over a stored override it can no longer see.
The seam's existing answer to a gap is to move the generation, which invalidates the view a session holds;
a cache-only reset has no way to reach a reader already inside the barrier.
Nothing in this plan clears a cache.

### The failed-open path is the only one that needs the cancellation

`Run` returns in exactly three ways:
`View` fails before any watch, a `Watch` fails, or the caller's context ends and every consumer finishes.
The third is shutdown.
The first two are reached only before this `Caches` has had all four kinds synced,
because a `Run` that reaches its wait returns only when the caller's context ends,
and the slot watch is opened last, so an earlier failed open leaves its cache without a marker.
No barrier is standing when either error path is taken,
so the repair needs no reset to be safe: only the cancel and the wait.

**That is a property of how `Caches.Run` is called, not of the function alone.**
`Run` does not clear its sync flags when a healthy call ends (`internal/pgo/caches.go:479-490`),
and `Client.Synced` reports true vacuously once no watch is registered
(`internal/natskv/client.go:251-263`),
so one caller could still produce the synced-but-unfed cache this section says cannot arise:
one that cancelled a healthy `Run`, kept the same `Caches`, and then retried into a failed first open.
No caller does that: production builds one `Caches` per process and cancels only to shut down
(`cmd/profgate/serve.go:681-683`, `:724-742`),
and both fixtures call `Run` once and cancel during cleanup
(`internal/pgo/fixtures_test.go:659-674`, `internal/httpapi/fixtures_test.go:1584-1598`).
A `Caches` is not reusable after a healthy `Run` returns,
and the repair carries a comment on `Run` saying so,
because the next caller added is what would break the proof rather than the code.

---

## An attempt owns the watches it opened

**Files:**
- Modify: `internal/pgo/caches.go`, `internal/pgo/fixtures_test.go`
- Add: `internal/pgo/caches_test.go`

**The repair.**
`Run` opens every watch under a context derived from the caller's.
On a failed open it cancels that context, waits for the consumers it started, and returns the error.
On the success path nothing changes: the wait ends when the caller's context does.

| Path out of `Run` | Today | After |
|---|---|---|
| `View` fails before any watch | returns the error | unchanged |
| `Watch` fails at position `n` | returns, leaving `n-1` consumers running under the caller's context | cancels the attempt, waits for those `n-1`, returns the same error |
| the caller's context ends | returns nil once all four consumers finish | unchanged |

Cancelling the attempt closes the sibling channels:
`consumeWatcher` returns on `ctx.Done()` while waiting for an update and while waiting to forward one
(`internal/natskv/client.go:629-651`)
and `runWatch` then closes its channel through the `defer` at `:585`.

- [ ] **Give the fixture a `Watch` it can fail**

`hookKV` (`internal/pgo/fixtures_test.go:1502-1560`) wraps `Get`, `Create`, `Update`, `Delete`, and `Keys`,
and `hookClient.View` (`:1346-1374`) is what puts it in front of both buckets.
It gains `Watch`, able to fail for a chosen prefix.
Every channel it hands back must honour the context it was given —
closing when that context is done, as `kvView.Watch` does —
or the cancel-then-wait this task adds would hang against a fake no real implementation matches.
`internal/pgo/fixtures_test.go:659-665` calls `caches.Run` with the raw client,
so the new test wires the hook itself rather than relying on the existing replica fixture.

- [ ] **Write the tests**

`internal/pgo/caches_test.go`, which does not exist today:

| Case | Expect |
|---|---|
| `Watch` fails at the third of four sources | `Run` returns that error, naming the prefix |
| the same | the contexts the first two watches were opened under are done before `Run` returns |
| the same | both consumers have returned before `Run` returns |
| the same, then the failure lifted and `Run` called again | four watches are active, not six |
| `View` fails before any watch | `Run` returns that error and opens nothing |
| the caller's context ends during a healthy run | `Run` returns nil and every consumer has returned |
| a healthy run | the barrier lifts once all four replay markers are applied, as it does today |

The first three fail against the current code because the two consumers are still live when `Run` returns.
The fourth is what makes the leak visible as a count rather than as a description.

- [ ] **Run the tests and watch them fail**

- [ ] **Land the repair**

- [ ] **Say what changed**

`CHANGELOG.md`, under `## [Unreleased]` and `### Fixed`:
a gateway with `pgo.enabled` no longer accumulates a duplicate cache consumer each time a watch fails to open.

- [ ] **Validate and commit**

```bash
semlf check CHANGELOG.md
mise exec -- go test -race -count=1 ./internal/pgo/
mise run lint && mise run test && mise run check
git add internal/pgo/ CHANGELOG.md
git commit -m "fix(pgo): clean up after a failed watch open"
```

---

## The roadmap and this plan's own status

**Files:**
- Modify: `docs/plans/roadmap.md`, this plan

- [ ] **Tick the bullet this plan covers**

The item's first bullet is ticked and its `Shipped:` line names the change that carried it.
The second bullet stays open: it is a different defect in a different package,
and this plan does not touch it.

- [ ] **Flip this plan**

Line 3 becomes `**Status:** Done` and line 4 an `Outcome:` naming the same change.
The plan is deleted in the next commit that touches it, per
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md);
one commit cannot do both.

- [ ] **Validate and commit**

```bash
semlf check docs/plans/
mise run check
git add docs/plans/
git commit -m "docs: record the watch cleanup repair"
```

---

## Risks and What This Plan Does Not Cover

**The leak this fixes is the smaller of the two defects in that area.**
A watcher that reopens under an unchanged generation replays into a cache nothing clears,
and a key deleted during the gap survives.
That one is real and it lives in `internal/natskv`.
It needs an invalidation that reaches sessions already inside the barrier, not the caches alone.
[`docs/plans/roadmap.md`](roadmap.md) carries it as the open bullet beside the one this plan ticks.
Nothing here makes it better or worse.

**A reopen still costs whatever `runCaches` waits.**
The doubling backoff in `runCaches` (`cmd/profgate/serve.go:724-742`),
to the cap its constants set at `:36-38`, is unchanged.
What changes is that the process reopens from one consumer per prefix rather than from several.

**Not covered:** the same-generation replay above,
anything about which process runs the collection loops,
and anything in `internal/natskv`.

---

## Self-Review

- Every claim about the tree was run, not remembered:
  `internal/pgo/caches.go:282-315` is `Run`,
  its `Watch` error at `:303-305` returns without reaching the `wg.Wait()` at `:312`,
  and `:306-310` starts one consumer per source;
  `:318-325` is `consume`;
  `internal/natskv/client.go:585` is `runWatch`'s `defer close(ch)`,
  `:596-611` is the loop that reopens an underlying watcher without returning,
  and `:636-638` is where a closed watcher is reported as a reconnect rather than as completion;
  `:629-651` is where a consumer stops on `ctx.Done()`, on the receive side and on the send side alike;
  `:309-326` is `watchState`, which sets a marker and never clears one;
  `internal/pgo/runtime.go:112-137` is `Session`, which checks the barrier once;
  `internal/httpapi/pgo_collections.go:413` reads the cache from a session without rechecking it;
  `cmd/profgate/serve.go:724-742` is `runCaches`, whose doubling wait uses the constants at `:36-38`;
  `internal/pgo/fixtures_test.go:1502-1560` is `hookKV`, which wraps five methods and not `Watch`,
  `:1346-1374` is the `View` that installs it, and `:659-665` calls `caches.Run` with the raw client;
  `internal/pgo/caches_test.go` does not exist.
- Decided here because the evidence pointed away from the larger change:
  no cache is cleared and no barrier is shut,
  because a reader admitted by `Session` reads the cache later without rechecking it;
  the cancellation is on the failed-open path alone,
  because a cut watch never closes the channel `Caches.consume` reads;
  the fake's channels must honour their context, because otherwise the test models no real implementation.
- Left to the implementer by design: the hook's shape and its method names,
  and how the test observes that a consumer has returned,
  so long as "four and not six" is asserted rather than assumed.
