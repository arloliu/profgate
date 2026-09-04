# Every Command Line Answers for Itself

**Status:** Approved

> **For the implementer:** implement this plan one task at a time, in order;
> each task ends with its own validation block and one commit.
> Checkboxes (`- [ ]`) track progress.
> Where this plan and the code disagree, the code is the fact and this plan is the bug.

**Goal:** make the `profgate` binary answerable from a terminal.
Every command line it has — the bare binary, each client verb, each group that takes a subverb,
and each operator command line — prints its own grammar and its own flags for `-h` or `--help`,
on stdout, with exit 0, having sent no request and read no stdin.
What the binary says when it fails becomes one shape:
a gateway refusal prints the gateway's `code` and `error` on stderr and, under `--output json`,
copies the envelope's own bytes to stdout;
a response that is not envelope-shaped prints its status and nothing the response carried.
The tables stop lying:
`collection get` prints the round a person would count,
and the four fields that say how long `download` will still work;
`targets --explain` prints how many Pods the selector matched,
and `collect --wait` keeps stdout for the record alone.
Two flags that name one concept become one name, and the gateway's own refusals name the next step to take.

**Architecture:** one new file, `cmd/profgate/help.go`, holds a node per command line
and the pre-scan that finds a help argument before any flag set parses.
`internal/client` grows the raw envelope bytes on `APIError` and one detail string on `StatusError`;
`fail` in `cmd/profgate/exit.go` takes the resolved output mode so a refusal can copy those bytes.
`internal/client/wire.go` grows four fields on `CollectionRecord`.
Four message strings in `internal/httpapi` gain a clause naming the endpoint or the configuration key.
No package moves, no route is added, no Kubernetes or NATS permission changes,
and `internal/k8s` is not touched.

**Spec:** [`cli.md`](../specs/cli.md) is `Accepted`, and its
*Help*, *Command grammar*, *Reading*, *Collections*, *`pgo policy`*, *Output and exit codes*,
*Failure scenarios*, and *Testing* sections are what the tasks below implement.
Its *Changes to the accepted designs* records one edit still owed to
[`gateway.md`](../specs/gateway.md) *CLI*,
which *Every operator command line prints its own help* makes and clears.
The `selectorMatched` row, the round display, the receipt's stream, the `-n` and `-o` hints,
the wording repairs, and the gateway's next-step clauses are defined by no spec:
[`roadmap.md`](roadmap.md) says so on the item's `Spec:` line, and each is decided in *Decisions* below.
This work is ordered by [`roadmap.md`](roadmap.md),
under *Give every CLI verb a `--help` and an honest table*,
and the evidence behind each bullet is in
[`2026-09-03-usability-and-stability.md`](../investigations/2026-09-03-usability-and-stability.md).
Rules in force: [`.agents/rules/`](../../.agents/rules/).

---

## Invariants

These hold at the end of every task, not only at the end of the plan.

1. **Help sends nothing and reads nothing.**
   A command line carrying a help argument opens no connection, resolves no context file,
   reads no token cache, and reads no byte of stdin.
   `profgate auth hash --help` prints help against a stdin that fails the test when it is read.
2. **Stdout carries only what the gateway sent or the command produced.**
   No line this client composed about a failure reaches stdout.
   Under `--output json` a refusal's stdout is the gateway's envelope bytes, copied and not rebuilt;
   every other failure leaves stdout empty.
3. **A failure this client describes discloses nothing the response carried.**
   A response that is not envelope-shaped prints its status, its standard reason, and a fixed clause,
   and no media type, no length, no prefix, no body byte, and no credential.
   The envelope copy of invariant 2 is the one place a response's bytes reach a stream,
   and there they are the gateway's own document rather than a line this client composed about a failure.
4. **The client relays the gateway's words.**
   Where a refusal should name a next step, the gateway's message gains the clause;
   the client never maps a code to a verb, because that is a judgement the gateway did not make.
5. **No Kubernetes permission moves.**
   `internal/k8s` is untouched, the ClusterRole is untouched, and no route is added or removed.

---

## Decisions

Nine choices this plan makes that the spec or the roadmap left open, or that overturn something deliberate.
All are settled.

**`fail` takes the resolved output mode as a parameter.**
`fail(env, err)` (`cmd/profgate/exit.go:47-50`) sees only the error,
and the envelope copy of *Output and exit codes* has to happen there:
it is the one site every refusal passes through.
The mode has one resolution and two readers.
`Settings.Output` (`internal/client/contexts.go:409-412`) is `table`, then `PROFGATE_OUTPUT`, then `--output`;
the contexts file cannot set it, because `Context` (`internal/client/contexts.go:35-41`) has no output key.
`env.outputFormat` (`cmd/profgate/contextcmd.go:143-155`) reads the same three in the same order,
for the context verb, which resolves no gateway and so holds no `Settings`.
`Settings.Output` is what `fail` is handed, because only a gateway refusal has bytes to copy
and every verb that can hold one has resolved its settings before the refusal exists.
The signature becomes `fail(env *cmdEnv, output string, err error) int`,
and a caller with no resolved settings passes `""`.
Storing the mode on `cmdEnv` instead was rejected:
a forgotten assignment fails silently, while a forgotten argument fails to compile.

**The gateway's messages gain the next step; the client stays verbatim.**
The roadmap bullet ends "The gateway's text names the endpoint or the configuration key; the client stays verbatim,"
and that is the reading this plan takes.
A client-side hint would need a code-to-verb map, which is exactly the judgement
*Core decisions* forbids the client from making,
and it would put on stdout or stderr a sentence no gateway ever sent.
Four `internal/httpapi` messages change instead, which makes this a change to gateway-visible text
and earns its own `CHANGELOG.md` line.
The cost is that an old gateway behind a new client says nothing extra, which is correct:
the gateway is the thing that knows whether the endpoint exists.

**`progress.round` prints one-based.**
`internal/pgo/rounds.go:142-145` sets `Rounds` from the policy when the run state is built
and then assigns `Round = round` from a zero-based loop,
so a checkpoint with `Rounds > 0` and `Round == 0` means the first round is running,
and a completed record carries `Round == Rounds-1`.
Both printers add one — `cmd/profgate/collect.go:366` and `internal/client/collect.go:228` —
so a completed three-round Collection reads `round 3 of 3` and a starting one reads `round 1 of 3`.
The wire field is untouched: `--output json` still carries the index the gateway stored,
because the rendering is where a person counts and the document is where a machine does.
Renumbering the field in `internal/pgo` was rejected: it is a stored record's field,
and changing it would rewrite the meaning of every record already in the store.

**`selectorMatched` is a row inside the `--explain` block, not a line above the list.**
Nothing may displace `POD` as the first line of stdout, with `--explain` or without it,
because the header is what tells a script the request succeeded (*Reading*)
and a pipe into `cut` reads the first line.
So the number is rendered as a key-and-value row — `writeTable` with a nil header,
the shape the repository already uses for a single record —
after the blank line that ends the target list and before the `REASON  COUNT` header,
with a blank line between the two.
Putting it in the `REASON  COUNT` table was rejected:
a count of matched Pods sitting under a `COUNT` header of excluded ones re-creates the confusion this fixes.
The example in [`cli.md`](../specs/cli.md) *Reading* illustrates the old rendering,
so that example moves with the code and the change is recorded in that document's *Amendments*.
An illustration that no longer matches the code is a document bug
([`000-agent-contract.md`](../../.agents/rules/000-agent-contract.md)), not licence to leave it.

**`logout` writes to stderr, on both outcomes.**
`internal/client/login.go:427` prints `nothing is cached for <...>` on stdout and prints nothing on success.
Both lines move to stderr and success gains one:
`logout` fetches no document, and the local action it reports is the same kind of thing
`pgo policy delete` already reports on stderr (`cmd/profgate/policy.go:225`).
Leaving the "nothing is cached" line on stdout and adding a success line beside it was rejected:
it would put a composed English sentence on stdout under `--output json`, which invariant 2 refuses.
This changes what a script redirecting `logout`'s stdout sees, so it is marked breaking.

**`login --context <name>` says what to run next and selects nothing.**
`File.RecordLogin` (`internal/client/contexts.go:159-169`) creates the entry and never touches `CurrentContext`,
which is correct: `--context` names which context this one command speaks through,
and selecting it would change what every later command does without being asked.
The repair is one stderr line naming `profgate context use <name>`,
printed only when the login recorded a context that is not the selected one.

**The operator flag sets stop printing through `flag`.**
`runVersion`, `runConfig`, and `runServe` (`cmd/profgate/main.go:49,65,94`) call `fs.SetOutput(stderr)`,
so `flag` writes its own usage block to stderr before returning an error.
The help backstop of *Help* — `flag.ErrHelp` from any flag set prints help rather than `flag: help requested` —
cannot be honest while that happens, because the block would already be on stderr.
The three sets move to `io.Discard` and the functions print `profgate: <err>` and their usage line themselves,
which is what the client half already does (`cmd/profgate/client.go:214`).
The consequence is that `profgate serve --bogus` prints two lines in the client half's shape,
instead of `flag`'s own three,
and that shape is now the binary's only one.

**Each leaf carries its own grammar, positionals, and flags.**
A verb declares one `grammar`, one `positionals`, and one `flags` (`cmd/profgate/client.go:27-35`),
so every subverb of a verb shares all three.
That is invisible while nothing prints them and dishonest the moment something does:
`pgo policy get` would advertise `--file`, `--enabled`, and `--every`, which only `set` reads
(`cmd/profgate/policy.go:21-40`),
and each `context` leaf would print the combined line
`context list|show [<name>]|use <name>|delete <name>` (`cmd/profgate/contextcmd.go:23`).
So the three fields move to a `leaf` the verb declares one of per subverb,
and `verb.parse` reads the matched leaf where it reads the verb today.
One table then answers both the parse and the page,
which is what makes a page a fact about the parse rather than a second description of it.
No `grammar` stays behind on the verb, because one stored string cannot describe two groups:
`pgo` takes only `policy`, and only `pgo policy` takes `get`, `set`, and `delete`.
A group's line is built from its own path and its immediate children instead —
`pgo policy` for `pgo`, and `pgo policy get|set|delete` for `pgo policy` —
so it names the one word the reader has to add next and names no flag,
because a flag belongs to the leaf that registers it
and a group page that listed one would be this same untruth one level up.
What that word then takes is on the child's own page,
which is what the subverbs printed below the line send the reader to.
The consequence is that `pgo policy get --file x` and `pgo policy delete --enabled` stop parsing:
a flag only `set` registers is undefined on its two siblings, exit 2 naming the flag.
Registering them and ignoring them was rejected —
a page that lists a flag the command ignores is the same untruth one page down —
and because this refuses input the binary accepts today it is marked breaking.

**The help pre-scan walks the node tree and steps over a flag's value.**
A scan that took the first argument without a leading dash would read
`profgate --server https://g.example --help profile` as naming the verb `https://g.example`.
Instead the scan walks the arguments left to right against the node tree.
It skips a word beginning with `-`, and where that word's name — the text after its dashes, cut at the first `=`,
which is the normalization the help spelling already uses — names a flag that takes a value,
it skips the word after it too.
`--flag=value` is one word and consumes nothing after it.
`profgate --namespace collect --help` therefore names no verb and prints the bare binary's help,
and so does `profgate --namespace=collect --help`, whose value is in the same word.
The flags the scan knows are the global ones at every node — nine of the eleven take a value —
and, once the walk stands on a verb or a group, that node's own beside them.
A group's set is the union of its leaves' value-taking flags,
because the leaf the line names is not known while the walk stands on the group.
`profgate pgo policy --file get --help` therefore prints the `pgo policy` page:
`get` there is the value of `--file`, so `pgo policy` is the deepest verb and subverb the line names.
That reading is sound rather than a guess,
because a subverb precedes its leaf's flags in *Command grammar* —
`verb.parse` matches the subverb words at the front of the arguments (`cmd/profgate/client.go:253-267`) —
so a bare word standing after a flag is never the subverb of a line that parses.
Nothing is pre-parsed for a verb the walk has not reached:
the scan classifies the flags of the node it already stands on and no others.
Each set is derived rather than written down:
a throwaway `flag.FlagSet` takes `globals.register`, or the leaf's own `flags`,
and every flag whose value is not a boolean is one,
so a flag added later is classified without a second edit.
While the path names a group, the next bare word must name one of that group's children,
and a word that names none prints the bare binary's help:
`profgate collection frobnicate --help` prints the bare page and not the `collection` group,
which is what *Help* says a subverb the binary does not have prints.
Once the path reaches a leaf the walk stops, because every word after a leaf is that leaf's own argument,
which is what makes `profgate profile payments/checkout cpu --help` the `profile` page.

---

## Global Constraints

- **No new route, no new configuration key, no new Kubernetes or NATS permission.**
  `internal/k8s` and `internal/natskv` are not touched;
  the route table, the OpenAPI document's route set, and the ClusterRole do not move.
  [`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md) is not engaged by any task here.
- **Seven changes to released behavior, each recorded under `Unreleased` in `CHANGELOG.md`.**
  `v0.5.0` is a tagged, published release (`CHANGELOG.md:8` opens the unreleased section above it),
  so each is a change to behavior an installation may already depend on.
  Marked breaking, under `### Changed`:
  the `--body` to `--file` rename;
  the `collect --wait` receipt moving from stdout to stderr;
  the envelope bytes appearing on stdout under `--output json`;
  `-o json` and `-o yaml` refused on `profile` and `download`;
  the `round N of M` display moving by one;
  `logout`'s two lines moving from stdout to stderr;
  and `pgo policy get` and `pgo policy delete` refusing the flags only `set` registers.
  One further `### Changed` entry is not breaking:
  a usage error under `context` prints the leaf's grammar line below its cause,
  which every other verb's usage error already prints, at the same exit code and with the same cause text.
  The same entry covers the other direction:
  a usage error that matched no subverb prints the group's line, which names the subverbs and not their positionals,
  so `profgate context` and `profgate pgo` each name what to add and send the reader to that subverb's page for the rest.
  Under `### Added`: `-h` and `--help` on every command line, and the four new table fields.
  Under `### Fixed`: the fixed line for a body that is not envelope-shaped,
  the wording repairs, and the gateway messages that now name a next step.
  None gets a compatibility shim or a deprecation window;
  this repository does not design around a breaking change.
- **Every code task writes its test first and shows it red before the fix.**
  Each task names the test, the file it goes in, and the exact command that shows it red,
  so an implementer who sees a green test knows the fixture is wrong rather than the code being right
  ([`000-agent-contract.md`](../../.agents/rules/000-agent-contract.md)).
  The harness already exists:
  `newTestEnv(t)` in `cmd/profgate/client_test.go:52` returns `te` with `te.env`, `te.stdout`, `te.stderr`,
  and `te.vars` over a temporary `HOME`;
  `refusingTransport(t)` (`:26`) fails the test on any request, which is what proves a local answer;
  `roundTripFunc` (`:22`) and `jsonResponse` (`:35`) are the response fakes;
  `dispatch(context.Background(), te.env, clientVerbs(), args)` drives one command line,
  and `run(args, stdout, stderr)` drives the process (`cmd/profgate/main_test.go:12`).
  No new harness is needed by any task below.
- **Every message this plan fixes has one exact text.**
  Each task gives the line it prints, so two implementers produce the same output
  and the tests can assert it whole.
- No jargon: comments, commit messages, and documentation state the current fact,
  never this plan's ordering, a heading from this file, or how the fact was arrived at.
  Cite a spec section by its name, never by its number.
- Markdown prose uses semantic line breaks;
  run `semlf check` on every Markdown file and every Go file with doc comments a task writes or edits
  ([`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)).
- Commit headers are Conventional Commits under 50 characters — the hook refuses 50 or more —
  with a body that says what changed and why, and no trailer of any kind
  ([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).
  Every `git add` names the files the task owns; nothing is staged by directory.
- Every task ends with the same validation block before its commit:

```bash
mise run lint && mise run test && mise run check
```

---

## File Structure

```text
cmd/profgate/help.go            # the node per command line, the pre-scan, and the printer
cmd/profgate/help_test.go       # the table over every command line the binary has
cmd/profgate/client.go          # the leaf, the pre-scan in dispatch, the ErrHelp backstop, the -n hint
cmd/profgate/main.go            # the operator flag sets, their help, and their own error lines
cmd/profgate/auth.go            # auth hash answers help before it reads a password
cmd/profgate/exit.go            # fail takes the output mode and copies the envelope's bytes
cmd/profgate/read.go            # the selectorMatched row
cmd/profgate/profile.go         # -o json and -o yaml refused
cmd/profgate/collect.go         # the two collection leaves, --file, the receipt on stderr, the round display, the four fields, -o
cmd/profgate/policy.go          # the three policy leaves and the fail call sites
cmd/profgate/contextcmd.go      # the four context leaves, the fail call site, and what context delete says
cmd/profgate/*_test.go          # the tests each task writes first
internal/client/client.go       # the envelope shape, the raw bytes, the bound, the warning's order
internal/client/errors.go       # APIError.Body, StatusError.Detail
internal/client/wire.go         # the four CollectionRecord fields
internal/client/collect.go      # the round display in the wait stream
internal/client/login.go        # logout's two lines, and what login says about context use
internal/client/contexts.go     # the contexts-file refusal without a Go type name
internal/client/*_test.go       # the tests each task writes first
internal/httpapi/profile.go     # port_not_allowed and no_targets name the endpoint
internal/httpapi/server.go      # pgo_disabled names the configuration key
internal/httpapi/pgo_collections.go  # collection_in_progress names the endpoint
internal/httpapi/*_test.go      # the message assertions
docs/cli.md                     # help, the error shape, the flag name, the streams, the tables
docs/api.md                     # the port_not_allowed example body
docs/specs/cli.md               # the Reading example, and Amendments
docs/specs/gateway.md           # the sentence CLI owes about --help
CHANGELOG.md                    # eight changed entries, seven of them breaking, two added, three fixed
docs/plans/roadmap.md           # the item's checkboxes and its Shipped line
docs/plans/cli-help.md          # this file
```

---

## Every client command line prints its own help

Closes the first roadmap bullet's client half.

**Files:**
- Create: `cmd/profgate/help.go`, `cmd/profgate/help_test.go`
- Modify: `cmd/profgate/client.go`, `cmd/profgate/policy.go`, `cmd/profgate/collect.go`,
  `cmd/profgate/contextcmd.go`, `cmd/profgate/client_test.go`, `cmd/profgate/policy_test.go`,
  `cmd/profgate/contextcmd_test.go`, `docs/cli.md`, `CHANGELOG.md`

**What exists today.**
`dispatch` (`cmd/profgate/client.go:189-220`) parses the global flags with the set's output at
`io.Discard` (`:195`) and each verb's set the same way (`:300`),
and nothing anywhere handles `flag.ErrHelp`.
So `profgate whoami --help` reaches `v.parse`, `flag` returns `ErrHelp`,
and `:214` prints `profgate: flag: help requested` with the usage line and exit 2.
The eleven global flags of `globals.register` (`:118-130`) appear in no output the binary can produce;
nine of them take a value and `--verbose` and `--token-stdin` do not.
`pgoVerb` (`cmd/profgate/policy.go:22`) declares its subverbs as the three strings
`"policy get"`, `"policy set"`, and `"policy delete"`,
and `verb.parse` (`client.go:253-267`) matches them word by word,
so there is no `pgo policy` node anywhere in the dispatcher for help to answer for.
A verb also declares one grammar, one positional count, and one flag set for all of its subverbs
(`client.go:27-35`), which is what *Decisions* moves to the leaf.

**The leaf.**
`verb` keeps the fields the dispatcher matches on and hands the rest to a leaf:

```go
// leaf is one command line that runs: the subverb words that name it under
// the verb, the grammar line printed after "usage: profgate ", how many
// positionals it takes, and the flags it registers.
// Every verb declares at least one: a verb with no subverbs declares the one
// whose words are empty, which is the verb's own command line.
type leaf struct {
	words       string
	grammar     string
	positionals int
	optional    bool
	flags       func(fs *flag.FlagSet)
}
```

`verb` keeps `name` and `run` and declares `leaves []leaf`, never empty.
The `grammar` field leaves `verb` altogether:
a leaf carries its own line, and a group's is built from its path and its children.
A verb with no subverbs declares the one leaf whose `words` are empty,
whose `grammar` is the line that verb prints and the line its usage error already prints:
the eleven verbs that take no subverb keep exactly one page each.
`verb.parse` matches the subverb words as it does today, then reads that leaf's
`positionals`, `optional`, and `flags` where it reads the verb's fields now.
`dispatch`'s usage line (`client.go:214`) prints the matched leaf's grammar,
and the derived group line where no subverb matched.
`profgate context` therefore prints `usage: profgate context list|show|use|delete [flags]`
where it prints those four subverbs with their positionals today,
and `profgate pgo` prints `usage: profgate pgo policy`.
The cause line above it still names the subverbs the verb takes, word for word as `verb.parse` writes it now,
and what each of them takes is on that subverb's own page.
The four verbs that declare subverbs get their leaves:

| Leaf | Grammar | Positionals | Flags |
|---|---|---|---|
| `collection get` | `collection get <id>` | 1 | none |
| `collection cancel` | `collection cancel <id>` | 1 | none |
| `pgo policy get` | `pgo policy get <ns>/<svc>` | 1 | none |
| `pgo policy set` | `pgo policy set <ns>/<svc> [--file <path> \| --enabled[=false] --every <d> --jitter <d> <field flags of collect>]` | 1 | the eleven `policy set` flags |
| `pgo policy delete` | `pgo policy delete <ns>/<svc>` | 1 | none |
| `context list` | `context list` | 0 | none |
| `context show` | `context show [<name>]` | 1, optional | none |
| `context use` | `context use <name>` | 1 | none |
| `context delete` | `context delete <name>` | 1 | none |

`context`'s three positional checks inside `runContext`
(`cmd/profgate/contextcmd.go:49-51`, `:56-58`, `:64-66`)
become unreachable and are deleted:
the parser produces the same sentences from the leaf, word for word,
once `pluralPositionals` (`client.go:309-314`) answers `no positional` for zero.
`context list foo` therefore keeps its cause line and its exit 2 and gains the grammar line below it,
which is what every other verb's usage error already prints.

**The node, and how the tree is built.**
`cmd/profgate/help.go` holds one type:

```go
// helpNode is one command line the binary answers a help argument for:
// the words that name it, the grammar line printed after "usage: profgate ",
// whether it prints the global flags, the subverbs it lists when it is a
// group, the flags it registers when it is a leaf, and the flags at this node
// that take a separated value, which is what the pre-scan steps over.
type helpNode struct {
	path       []string
	grammar    string
	globals    bool
	children   []string
	flags      func(fs *flag.FlagSet)
	valueFlags map[string]bool
}
```

`helpNodes(verbs []verb) []helpNode` derives the client half from the verb table alone,
so a verb added later gets its help without a second edit:

- the bare binary is `path` nil, no grammar, `globals` true, and `children` the verb names;
- each `leaf` of each verb becomes a help leaf:
  its path is the verb's name and the leaf's words, and it carries the leaf's own `grammar` and `flags`,
  with `globals` true and no children;
- every proper prefix of a leaf's path that is not itself a leaf becomes a group,
  whose children are the distinct next word of every leaf whose path extends it
  and whose grammar is the line `groupGrammar` builds from the two:
  the path, then those children joined by `|`.
  `pgo` therefore yields the group `pgo`, whose child is `policy` and whose line is `pgo policy`,
  and the group `pgo policy`, whose children are `get`, `set`, and `delete`
  and whose line is `pgo policy get|set|delete`, beside the three leaves of the table above;
  `collection` yields one group reading `collection get|cancel` and two leaves,
  and `context` one group reading `context list|show|use|delete` and four.

A group carries no `flags`, so no group page prints a `Flags:` block.
It carries `valueFlags` all the same — the union of its leaves' value-taking flags —
because the pre-scan has to step over a value while standing there.

That is twenty client leaves and four client groups,
which is what [`cli.md`](../specs/cli.md) *Help* counts.
A leaf prints only what its own command line takes, which is the point of the leaf:
`pgo policy get` prints `pgo policy get <ns>/<svc>` and no flag,
while its sibling `set` prints the eleven it registers.
A group prints the line that lists the subverbs plus the subverbs themselves,
which is what tells a reader which word to add.

**What a page looks like.**
Everything goes to stdout, in this order, with one blank line between blocks:

```text
usage: profgate profile <ns>/<svc> <profile> [--seconds <n>] [--pod <name>] ...

Flags:
  -seconds string
    	seconds, for cpu and trace
  ...

Global flags:
  -context string
    	the context to use; PROFGATE_CONTEXT when absent
  ...
```

A group prints its grammar line and then the words that extend it:

```text
usage: profgate pgo policy get|set|delete

Subcommands:
  get
  set
  delete
```

One level up, `pgo` prints `usage: profgate pgo policy` and the one subcommand `policy`.

The bare binary prints `usageLine(verbs)` (`client.go:179-185`) and the global-flags block.
The two flag blocks are two `flag.FlagSet` values built for printing alone —
one registered from the node's `flags`, one from `globals.register` — each with `SetOutput(stdout)`
and printed with `PrintDefaults`, so the flag descriptions stay in one place, the flag registrations.
A leaf with no flags of its own prints no `Flags:` block.

**The pre-scan.**
`findHelp(args []string) (rest []string, help bool)` walks `args` and stops at a bare `--`,
because `flag` stops there and a line the two read differently is a line nobody can predict.
An argument is a help argument when its name — the text after its one or two leading dashes,
cut at the first `=` — is `h` or `help`,
the same rule `flag` applies, so `-h`, `--h`, `-help`, `--help`, and `--help=0` are one flag to both.
`helpTarget(nodes []helpNode, args []string) helpNode` then finds the page,
walking left to right from the bare binary's node:

- a word beginning with `-` is skipped,
  and where its name is a value-taking flag of the node the walk stands on and the word carries no `=`,
  the word after it is skipped with it:
  the global set at every node, and the node's own `valueFlags` beside it once the walk leaves the root;
- a bare word while the walk stands on a group must name one of that group's children:
  it extends the path when it does, and the bare binary's node is the whole answer when it does not;
- the walk stops at the first leaf, and at a bare `--`,
  because every word after a leaf is that leaf's own positional or flag value.

`valueGlobals()` builds the global half of the set the first rule reads:
a `flag.FlagSet` takes `globals.register`,
and every flag whose `Value` does not report itself a boolean is one:
`VisitAll` asks each of them through `interface{ IsBoolFlag() bool }`,
which is the question `flag` itself asks when it decides whether to consume the next argument.
Nine of the eleven are in the set; `--verbose` and `--token-stdin` are not.
A node's own half is the same question asked of a set the leaf's `flags` registered,
answered once per leaf while `helpNodes` runs, and unioned into each group above it.
`findHelp` stays the separate walk it is and steps over no value,
because it asks only whether a help argument is on the line:
that is what makes `--file --help` help rather than a file named `--help` (*Help*),
and only `helpTarget`, which is choosing a page and not deciding whether to print one, consumes values.
`dispatch` calls both before anything else — before `isOperatorVerb`, before the global flag set,
before positionals are stripped — and on a help argument prints the page and returns `exitOK`.
`flag.ErrHelp` from either flag set (`client.go:197`, `:286`) prints the same page for the same node
and returns `exitOK`, which is the backstop that keeps `flag: help requested` out of every output.

- [ ] **Write the tests**

New file `cmd/profgate/help_test.go`.

| Test | What it asserts, and how it fails today |
|---|---|
| `TestHelpEveryClientCommandLine`, a table over the bare binary, the twenty leaves, and the four groups | for each, `-h` and `--help` each write the node's grammar line to stdout and exit 0 against `refusingTransport(t)` and a stdin that fails when read; a leaf's own flag names appear, a group's subverb words appear, and every one of the eleven global flag names appears. Today each returns 2 with `flag: help requested` on stderr |
| `TestHelpLeafPagesAreTheLeafs`, new | the two assertions that separate a leaf page from its verb's: the `pgo policy get` and `pgo policy delete` pages carry none of `-file`, `-enabled`, `-every`, and the `pgo policy set` page carries all three; the `context list` page's usage line is exactly `usage: profgate context list`, asserted whole because the group's `usage: profgate context list|show|use|delete` opens with the same characters and a substring check would pass on it, and `context show` carries `[<name>]` where `context use` and `context delete` carry `<name>`. Today one grammar and one flag set are declared per verb, so every one of these pages carries its siblings' |
| `TestHelpGroupPagesAreTheirChildren`, new | the assertion one grammar string cannot satisfy for two groups: the `pgo` page's usage line is exactly `usage: profgate pgo policy`, its `Subcommands:` block is the single word `policy`, and the page carries none of `get`, `set`, and `delete`, while the `pgo policy` page's line is exactly `usage: profgate pgo policy get|set|delete` and its block carries all three. `collection` reads `collection get|cancel` and `context` reads `context list|show|use|delete` beside them, and no group page carries a `Flags:` block of its own, so none of `-file`, `-enabled`, and `-every` appears on the `pgo` or `pgo policy` page. Today a verb declares one grammar for every group above its leaves, so `pgo` would advertise `get`, `set`, `delete`, and `set`'s flags |
| `TestPolicyGetRefusesSetFlags`, new in `cmd/profgate/policy_test.go` | `pgo policy get payments/checkout --file p.json` and `pgo policy delete payments/checkout --enabled` each exit 2 against `refusingTransport(t)` with `flag provided but not defined` naming the flag, while `pgo policy set payments/checkout --file p.json` still parses. Today all three parse and the first two ignore the flag |
| `TestContextLeafPositionals`, new in `cmd/profgate/contextcmd_test.go` | `context list foo` exits 2 with `context list takes no positional; "foo" is one too many` and the `context list` grammar line; `context use` and `context delete` with no name exit 2 with their own sentences. Today the first three sentences come from `runContext` with no grammar line below them |
| `TestValueTakingGlobalFlags`, new | `valueGlobals()` holds exactly `context`, `server`, `ca-file`, `issuer-ca-file`, `server-name`, `namespace`, `output`, `token-file`, `u`, and not `verbose` or `token-stdin`. A flag misclassified here silently changes which page prints, so the set is pinned rather than trusted |
| `TestHelpSpellings`, over `-h`, `--h`, `-help`, `--help`, `--help=0` on `profgate` and on `profgate profile` | the five produce byte-identical stdout for the same command line, and exit 0. Today all five exit 2 |
| `TestHelpWinsOverParsing` | `profgate profile --help` prints the `profile` page and not the too-few-positionals error; `profgate collection --help` prints the `collection` group and not the missing-subverb error; `profgate profile payments/checkout cpu --help` and `profgate collect payments/checkout --duration 30s --help` each print help, exit 0, and reach no transport. Today each is a usage error, exit 2 |
| `TestHelpPosition` | `profgate --help profile` and `profgate profile --help` print byte-identical stdout |
| `TestHelpUnknownName` | `profgate frobnicate --help` and `profgate collection frobnicate --help` each print the bare binary's help and exit 0, while `profgate frobnicate` alone still exits 2 with the usage line on stderr |
| `TestHelpStopsAtDoubleDash` | `profgate collect payments/checkout -- --help` exits 2 with `is one too many` on stderr and nothing on stdout; `profgate profile payments/checkout cpu -o=--help` reaches the flag as a path and writes a file named `--help`, because the name before the `=` is `o` and not `help`. `-o` is the attached-form case here because it exists on `profile` today; the `--file=--help` case of *Help* belongs to *One concept has one flag name*, which is where `--file` starts existing |
| `TestHelpFlagValueIsNotAVerb` | `profgate --namespace collect --help` and `profgate --namespace=collect --help` each print the bare binary's help, because `collect` there is a namespace and names no verb; `profgate --verbose collect --help` prints the `collect` page, because `--verbose` consumes no word after it; `profgate pgo policy --file get --help` prints the group page — the whole line `usage: profgate pgo policy get|set|delete` and its `Subcommands:` block — and not the `get` leaf's page, because `get` is the value of `--file`; `profgate collect payments/checkout --duration 30s --help` prints the `collect` page, whose line begins `usage: profgate collect <ns>/<svc>`, because the walk stopped at that leaf and read `30s` as nothing at all. The group case asserts the whole usage line: the `get` leaf's line opens with the same four words, so a substring check for `pgo policy` passes on the very page this is here to refuse. Today each exits 2 with `flag: help requested` |
| `TestHelpWritesNothingToStderr` | across every case above with exit 0, `te.stderr` is empty and no stdout carries `flag: help requested` |
| `TestDispatchGrammar` (`cmd/profgate/client_test.go:135`) | needs no edit and must stay green: the pre-scan runs before the grammar and must not change a line that carries no help argument |

The red state, before any production line moves:

```bash
go test ./cmd/profgate/ -run 'TestHelp|TestValueTakingGlobalFlags|TestPolicyGetRefusesSetFlags|TestContextLeafPositionals'
```

- [ ] **Move the grammar, the positionals, and the flags to the leaf**

- [ ] **Add the nodes, the pre-scan, and the printer**

- [ ] **Say so in the guide, and record the addition**

`docs/cli.md` gains a short section under *The verbs* saying that
`-h` and `--help` print the command's grammar and flags on stdout and exit 0,
that the page is the deepest verb and subverb the line names wherever the flag sits,
that each subverb answers for itself and prints no sibling's flag,
and that a verb or subverb the binary does not have prints the bare binary's help.
Where *`pgo policy`* documents `set`'s flags, say that `get` and `delete` take none.
`CHANGELOG.md` gains the `### Added` entry for help
and the breaking `### Changed` entry for the flags `pgo policy get` and `delete` no longer accept.

- [ ] **Validate and commit**

```bash
semlf check docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add cmd/profgate/help.go cmd/profgate/help_test.go cmd/profgate/client.go \
        cmd/profgate/policy.go cmd/profgate/collect.go cmd/profgate/contextcmd.go \
        cmd/profgate/client_test.go cmd/profgate/policy_test.go cmd/profgate/contextcmd_test.go \
        docs/cli.md CHANGELOG.md
git commit -m "feat(cli): answer --help on every client verb" -m "<body: what the flag set discarded, what each leaf prints, and that a subverb no longer advertises its siblings' flags>"
```

---

## Every operator command line prints its own help

Closes the first roadmap bullet's operator half and the `auth hash` bullet,
and makes the edit [`cli.md`](../specs/cli.md) *Changes to the accepted designs* records as owed.

**Files:**
- Modify: `cmd/profgate/help.go`, `cmd/profgate/help_test.go`, `cmd/profgate/main.go`,
  `cmd/profgate/auth.go`, `cmd/profgate/main_test.go`, `cmd/profgate/auth_test.go`,
  `docs/specs/gateway.md`, `docs/specs/cli.md`, `docs/cli.md`, `CHANGELOG.md`

**What exists today.**
`dispatch` hands an operator name to `runOperator` before the global flag set is built
(`cmd/profgate/client.go:190-192`), which is why an operator command line accepts no global flag
and why its help prints none.
`runAuth` (`cmd/profgate/auth.go:28-35`) matches `args[0] == "hash"`,
never looks at the argument after it,
and goes straight to `readPassword`,
so `profgate auth hash --help` reads stdin: it prompts `Password: ` and waits at a terminal,
and reads a line to EOF anywhere else, which under `go test` exits 2 with `password is empty`
(`cmd/profgate/auth.go:55-73`).
Either way the command reads a stream that invariant 1 says help must not touch.
`runVersion`, `runConfig`, and `runServe` each build a set with `SetOutput(stderr)`
(`cmd/profgate/main.go:48-50`, `:64-65`, `:93-94`).
`runOperator`'s default branch (`client.go:233-236`) is what `collector` reaches:
the usage line on stderr and exit 2.

**The nodes.**
`helpNodes` gains the operator half as a fixed table, because these command lines are not in `clientVerbs`.
Every one has `globals` false:

| Node | Grammar | Children or flags |
|---|---|---|
| `serve` | `serve --config <path>` | its `--config` flag |
| `version` | `version` | none |
| `config` | `config validate` | the child `validate` |
| `config validate` | `config validate --config <path>` | its `--config` flag |
| `auth` | `auth hash` | the child `hash` |
| `auth hash` | `auth hash` | none |

The two group rows take their line from the children beside them, as every client group does:
`config` reads `config validate` and `auth` reads `auth hash`,
so `--config` is named on `config validate`'s page and on no other.

`collector` gets no node: it names no implementation yet (*Reserved names*), so it has no grammar to print.
The pre-scan's rule already covers it — a bare word that names no child of the root is the bare binary's page —
so `profgate collector --help` prints the bare binary's help and exits 0,
and `profgate collector` without one still prints the usage line on stderr and exits 2.
That page is the bare binary's, so it carries the eleven global flags,
which is why `collector` is the one operator command line exempt from the assertion that a page carries none:
what it is asserted against instead is that its stdout is byte-identical to `profgate --help`'s.

**The three flag sets stop printing through `flag`.**
Each of `runVersion`, `runConfig`, and `runServe` sets `io.Discard` on its set
and, on a parse error, prints `profgate: <err>` and its own usage line to stderr before returning 2,
which is the shape `dispatch` already uses (`client.go:214`).
Each also returns `exitOK` after printing its page on `errors.Is(err, flag.ErrHelp)`,
which is the backstop; the pre-scan reaches these lines first, so the backstop is what makes
"no output carries `flag: help requested`" true rather than merely likely.
`runAuth` gets the same treatment at its front: it answers a help argument before `readPassword` is called.

**The sentence [`gateway.md`](../specs/gateway.md) is owed.**
Its *CLI* section lists the five operator command lines (`docs/specs/gateway.md:1417-1423`)
and says nothing about help.
One sentence is added below that block, in two lines:
that every one of these command lines answers `-h` and `--help` on stdout and exits 0,
under the one rule [`cli.md`](../specs/cli.md) *Help* states for the whole binary.
Write the citation as a relative link to `cli.md`, which is that document's sibling.

In the same change, [`cli.md`](../specs/cli.md) *Changes to the accepted designs* —
the subsection recording edits required and not yet made —
records no outstanding edit, because the one it names is now made.
While in `gateway.md` *CLI*, correct the adjacent sentence that reads
"`collect` runs the PGO collection loops" (`docs/specs/gateway.md:1429`):
the process is `collector`, which the paragraph below it names,
and the client verb `collect` is the one *Reserved names* keeps distinct from it.
Both edits are recorded as one row in that document's *Amendments*.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestHelpEveryOperatorCommandLine`, in `cmd/profgate/help_test.go` | over `serve`, `version`, `config`, `config validate`, `auth`, and `auth hash`: `-h` and `--help` each write help to stdout and exit 0, each page carries the grammar line of the table above, and **no page carries any of the eleven global flag names**. Today each exits 2 |
| the same test's `collector` case | `profgate collector --help` and `profgate collector -h` each exit 0 with stdout byte-identical to `profgate --help`'s. `collector` is exempt from the no-global-flag assertion because the page it prints is the bare binary's, which lists the global flags by definition |
| `TestHelpAnswersAuthHashBeforeThePassword`, new in `cmd/profgate/help_test.go` | `dispatch(ctx, te.env, clientVerbs(), []string{"auth", "hash", "--help"})` against a `te.env.stdin` whose `Read` calls `t.Fatal` exits 0, writes the grammar line to stdout, and writes nothing to stderr. It has to go through `dispatch` rather than `run`, because `run` builds its environment with `os.Stdin` (`cmd/profgate/main.go:42-44`) and a test cannot install a failing reader in it. Today `runAuth` (`cmd/profgate/auth.go:28-35`) never looks at `args[1]` and calls `readPassword`, which fails the test by reading |
| `TestRunAuthHash` (`cmd/profgate/auth_test.go:11`), new subtest `hash answers help` | `run([]string{"auth", "hash", "--help"}, ...)` exits 0 and writes the grammar line to stdout. Today `readPassword` reads the process's stdin, which under `go test` is not a terminal and reaches EOF, so the command exits 2 with `password is empty` — an unwanted read of a stream help must not touch, and on a terminal the same line waits at a `Password: ` prompt |
| `TestRun` (`cmd/profgate/main_test.go:12`), new subtests | `serve --bogus` and `config validate --bogus` each exit 2 with `profgate: flag provided but not defined` and the command's usage line on stderr, and with `flag`'s own usage block absent. Today `flag` writes that block to stderr |
| `TestOperatorNameBehindAGlobalFlag` | `profgate --server https://g.example version` still exits 2 naming the rule (`client.go:204-206`); the pre-scan must not turn an operator name behind a global flag into a success. With a help argument the same line is help: `profgate --server https://g.example version --help` prints the `version` page and exits 0, because *Help* answers the whole line before the rule about global flags applies |

The red state:

```bash
go test ./cmd/profgate/ -run 'TestHelpEveryOperatorCommandLine|TestHelpAnswersAuthHash|TestRunAuthHash|TestRun/serve'
```

- [ ] **Add the operator nodes and answer help before the password**

- [ ] **Write the sentence the gateway design owes, and clear what it recorded**

- [ ] **Validate and commit**

```bash
semlf check docs/specs/gateway.md docs/specs/cli.md docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add cmd/profgate/help.go cmd/profgate/help_test.go cmd/profgate/main.go cmd/profgate/auth.go \
        cmd/profgate/main_test.go cmd/profgate/auth_test.go \
        docs/specs/gateway.md docs/specs/cli.md docs/cli.md CHANGELOG.md
git commit -m "feat(cli): answer --help on operator commands" -m "<body: auth hash read a password before it read its arguments; the operator lines take no global flag and print none>"
```

---

## A refusal has one shape on both streams

Closes the roadmap bullet beginning
*Under `--output json` an error is still one text line on stderr*,
except its `--body` clause, which *One concept has one flag name* closes.

**Files:**
- Modify: `internal/client/client.go`, `internal/client/errors.go`, `internal/client/client_test.go`,
  `cmd/profgate/exit.go`, `cmd/profgate/read.go`, `cmd/profgate/profile.go`, `cmd/profgate/collect.go`,
  `cmd/profgate/policy.go`, `cmd/profgate/contextcmd.go`, `cmd/profgate/exit_test.go`,
  `cmd/profgate/client_test.go`, `cmd/profgate/read_test.go`, `docs/cli.md`, `CHANGELOG.md`

**What exists today.**
`decodeEnvelope` (`internal/client/client.go:244-254`) requires `application/json` and a non-empty `code`,
and accepts a body with no `error` field at all, because `envelope.Error` is a plain `string`
that unmarshals to `""` when the key is absent.
It keeps the code and the message and discards the bytes.
`StatusError.Error()` (`internal/client/errors.go:34-39`) returns `HTTP 200 OK` with no clause,
which is what a `2xx` carrying a non-JSON body prints today (`client.go:167-169`).
`Do` (`client.go:123-128`) writes `if body, err := readBounded(resp.Body); err == nil`,
so a body that fills the bound is discarded and the caller sees a bare `StatusError`;
`readBounded` (`:223-232`) already builds the message that names the bound, and `JSON` (`:163-166`) surfaces it.
`fail` (`cmd/profgate/exit.go:47-50`) prints one stderr line and writes nothing to stdout.

**What it becomes.**

`envelope` requires the `error` key to be present and a string:

```go
type envelope struct {
	Error *string `json:"error"`
	Code  string  `json:"code"`
}
```

`decodeEnvelope` returns nil unless the media type is `application/json`,
`json.Unmarshal` succeeds, `e.Code != ""`, and `e.Error != nil`.
A pointer is what makes an absent key and a `null` both fail,
and `json.Unmarshal` already refuses a non-string value for it,
which together is the whole of *Output and exit codes*'s envelope-shaped test.
The `APIError` it returns carries the bytes it decoded:

```go
type APIError struct {
	Status  int
	Code    string
	Message string
	Body    []byte // the envelope exactly as it arrived, for the --output json copy
}
```

`StatusError` gains one field and one default:

```go
// StatusError is a response this client could not read as the gateway's
// envelope. It carries the status and a clause of this client's own, and
// never a byte of the body.
type StatusError struct {
	Status int
	Detail string // empty means "body is not a profgate JSON document"
}
```

`Error()` prints `HTTP <status> <reason>: <detail>`,
leaving the reason out for a status `http.StatusText` has none for.
`Do` stops discarding the bound failure: it reads the body once,
and when `readBounded` fails it sets the failure to:

```go
&StatusError{
	Status: resp.StatusCode,
	Detail: fmt.Sprintf("body exceeds the %d-byte bound this client reads", maxResponseBytes),
}
```

Keeping it a `StatusError` is what keeps a `401` in that state on exit 3,
because `unauthorized` (`cmd/profgate/exit.go:37-42`) reads the status off either error type.
`JSON` uses the same construction for the same failure, so the two paths stop differing.

`fail` grows the mode and the copy:

```go
// fail prints the error as one stderr line and returns its exit code.
// Under --output json a gateway refusal's own envelope bytes go to stdout,
// copied and not rebuilt: only a refusal has bytes to copy, so every other
// failure leaves stdout empty.
// output is the resolved --output, or "" where no settings were resolved.
func fail(env *cmdEnv, output string, err error) int
```

It writes `ae.Body` to stdout, byte for byte with nothing appended,
when `output == "json"` and `errors.As(err, &ae)` finds a non-empty `Body`.
`errors.As` is what reaches the `APIError` the `401` diagnoser wraps (`client.go:129-131`).

Every call site takes the new argument, and the compiler names any that is missed.
There are twenty-three, which `grep -n "fail(" cmd/profgate/*.go` lists:

| File | Lines | What it passes |
|---|---|---|
| `read.go`, the read helper | `:30` | `""`: `env.gateway` is what failed, so there are no settings |
| `read.go`, the read helper | `:34`, `:38`, `:42`, `:47` | `s.Output`, from the settings `:28` resolved |
| `read.go`, `login` | `:276`, `:292` | `""`: the login flags are checked before anything is resolved, and `:292` is `env.settings` failing |
| `read.go`, `login` | `:296`, `:300`, `:304`, `:308`, `:324` | `s.Output` |
| `read.go`, `logout` | `:355` | `""`: `env.settings` is what failed |
| `read.go`, `logout` | `:359`, `:363`, `:366` | `s.Output` |
| `profile.go` | `:41` | `s.Output` |
| `collect.go` | `:50`, `:307`, `:386` | `s.Output`, which means `collect`, the wait, and `download` return their resolved settings from their helper or resolve them before the call |
| `policy.go` | `:42`, `:47` | `s.Output` |
| `contextcmd.go` | `:26` | `""`: the context verb sends no request and can hold no envelope |

A site that passes `""` is a site with no settings, and therefore a site with no envelope to copy;
where a helper resolved settings and then failed, the mode is the one that resolution produced.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestEnvelopeShape`, new in `internal/client/client_test.go` | a table: a full envelope is one; a JSON object with `code` and no `error` is not; `{"code":"x","error":null}` is not; `{"code":"x","error":5}` is not; a valid envelope under `text/plain` is not; a JSON array is not; `{"error":"x"}` with no code is not. Today the second case decodes as an envelope with an empty message |
| `TestStatusErrorLine`, new | `&StatusError{Status: 502}` prints `HTTP 502 Bad Gateway: body is not a profgate JSON document`, `{Status: 599}` prints `HTTP 599: body is not a profgate JSON document`, and a bound failure prints the byte figure. Today the first two print no clause |
| `TestDoSurfacesTheBound`, new | `Do` against a response whose body is `maxResponseBytes+1` bytes returns an error naming the bound; today it returns a bare `StatusError`. Assert `JSON` does the same, so the two paths agree |
| `TestNotEnvelopeShapedPrintsNothingCarried`, new in `cmd/profgate/exit_test.go`, a table over the four bodies *Testing* names | a `502` carrying HTML, a `502` carrying an empty body, a `502` carrying truncated JSON, and a `200` from a JSON route carrying a body that is not one JSON document. Each response carries a distinctive `Content-Type` and `Content-Length`, and each non-empty body carries a recognizable string. For each: stderr is exactly `profgate: HTTP <status> <reason>: body is not a profgate JSON document\n`, stdout is empty, the exit code is 1, and neither stream contains the media type, the length, or any substring of the body. This is the invariant-3 test |
| `TestNotEnvelopeShapedExitCodes`, new | a `401` in that state exits 3 and every other status exits 1; a `200` carrying one JSON document is the success path and prints the body |
| `TestRefusalUnderJSONCopiesTheEnvelope`, new | under `--output json`, a `400` envelope writes the response's bytes to stdout byte for byte, including its exact whitespace, and the one line to stderr, and the exit code is the one the refusal already had. Today stdout is empty |
| the same test, over one command line per family | a read verb, `profile`, `collect`, `pgo policy set`, and `download` each copy the envelope, so the mode reaches `fail` from every kind of caller and not only from the read helper. `context` is the family that cannot: it sends no request, and its subtest asserts stdout stays empty |
| `TestNoEnvelopeLeavesStdoutEmpty`, new | under `--output json`, a transport failure, a response that is not envelope-shaped, and a usage error each leave stdout empty. Green today, and it is what holds invariant 2 while the copy is added |
| `TestServicesUnknownNamespace`, new in `cmd/profgate/read_test.go` | against a transport answering `200` with `{"namespace":"nope","services":[]}`, `services nope` writes exactly `SERVICE\n` to stdout, writes nothing to stderr, and exits 0, which is the answer *Reading* gives for a namespace the gateway does not know. `TestReadVerbsTable` (`cmd/profgate/read_test.go:93`) covers an empty target list and an empty namespace list and asserts neither stderr nor an empty Service list, so this is its own test rather than one more row. Green today: it is the record that this answer is the design and not an accident, and the test that notices if a later change invents a not-found |

The `services` case belongs here because this is the work that closes the roadmap bullet naming it,
beside the envelope shape and the fixed line;
the behavior is one a `403` and a typo have to stay distinguishable from,
which is the same reading of the gateway's answers the rest of this work rests on.

The red state:

```bash
go test ./internal/client/ -run 'TestEnvelopeShape|TestStatusErrorLine|TestDoSurfacesTheBound'
go test ./cmd/profgate/ -run 'TestNotEnvelopeShaped|TestRefusalUnderJSON|TestServicesUnknownNamespace'
```

- [ ] **Change the two error types and the decoder**

- [ ] **Give `fail` the mode and the copy, and pass it at every call site**

- [ ] **Say what a failure looks like**

`docs/cli.md` *Errors and exit codes* (`:430-448`) gains the fixed line and the stdout copy,
gains the sentence that a namespace the gateway does not know is the empty list it answers
and not a not-found this client invents,
and *Automation* (`:459-475`) gains the sentence that a refusal's envelope reaches stdout under `--output json`,
so a `jq` pipeline reads a `400` the way it reads a `200`.
`CHANGELOG.md` gains the breaking `### Changed` entry for the copy and a `### Fixed` entry for the fixed line.

- [ ] **Validate and commit**

```bash
semlf check docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/client/client.go internal/client/errors.go internal/client/client_test.go \
        cmd/profgate/exit.go cmd/profgate/read.go cmd/profgate/profile.go cmd/profgate/collect.go \
        cmd/profgate/policy.go cmd/profgate/contextcmd.go cmd/profgate/exit_test.go \
        cmd/profgate/client_test.go cmd/profgate/read_test.go docs/cli.md CHANGELOG.md
git commit -m "fix(cli): one shape for every refusal" -m "<body: an absent error key read as an envelope, a 2xx non-document printed a bare status, and a refusal under json wrote nothing to stdout>"
```

---

## The kubectl reflexes are answered, not obeyed

Closes the roadmap bullet beginning *`-n` and `-o` are the kubectl reflexes*.

**Files:**
- Modify: `cmd/profgate/client.go`, `cmd/profgate/profile.go`, `cmd/profgate/collect.go`,
  `cmd/profgate/client_test.go`, `cmd/profgate/profile_test.go`, `cmd/profgate/collect_test.go`,
  `docs/cli.md`, `CHANGELOG.md`

**Two mechanisms, not one.**
`-n` is registered nowhere: `globals.register` (`cmd/profgate/client.go:118-130`) declares `namespace`,
so `-n payments` reaches `flag` as an undefined flag and `dispatch` prints
`profgate: flag provided but not defined: -n` with the verb's usage line.
That is a message repair.
`-o` **is** registered, on `profile` (`cmd/profgate/profile.go:36`) and on `download`
(`cmd/profgate/collect.go:382`), where it names the file the bytes are written to,
so `-o json` is a valid flag with a valid value and writes a pprof file named `json`.
That is a value refusal.

**What each becomes.**
`dispatch`'s usage-error path adds one line when the undefined flag's name is `n` or `o`,
read from the error text `flag` produces, or — preferably, and this is the shape to write —
from a check of the raw arguments before the parse, so the client does not read a library's wording.
The added line, on stderr, below the cause and above the usage line:

```text
profgate: -n is not a flag; the namespace flag is --namespace
```

and, for a verb that registers no `-o`:

```text
profgate: -o is not a flag; the output format flag is --output
```

`profile` and `download` refuse a `-o` value of exactly `json` or `yaml`, before any request:

```text
profgate: usage: -o names the file the profile is written to, not a format; --output json is the format flag, and -o ./json writes a file named json
```

The escape hatch is named in the message because the plan is refusing a legal filename:
a user who genuinely wants a file called `json` writes `-o ./json`.
`download`'s message says `artifact` where `profile`'s says `profile`.
The refusal covers exactly two values and nothing else,
so `-o=--help` still writes a file named `--help`
and the attached-form case the help work asserts stays green.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestDispatchGrammar` (`cmd/profgate/client_test.go:135`), new cases | `smoke x/y cpu -n payments` exits 2 and stderr names `--namespace`; `smoke x/y cpu -o json` on a verb with no `-o` exits 2 and stderr names `--output`. Today stderr carries only `flag provided but not defined` |
| `TestProfileRefusesAFormatAsAPath`, new in `cmd/profgate/profile_test.go` | `profile payments/checkout cpu -o json` and `-o yaml` each exit 2 against `refusingTransport(t)`, stderr names `--output` and `-o ./json`, and no file named `json` exists in the working directory afterwards. Today the command sends a request and writes the file |
| the same, for `download`, in `cmd/profgate/collect_test.go` | `download 7h2k9m4p6r8t0v1w3x5y -o json` exits 2 with the same shape |
| `TestProfileAcceptsAnEscapedPath`, new | `-o ./json` is accepted and writes there, which is what makes the refusal a refusal of a format and not of a name |

The red state:

```bash
go test ./cmd/profgate/ -run 'TestDispatchGrammar|TestProfileRefusesAFormatAsAPath|TestDownload'
```

- [ ] **Add the two hints and the value refusal**

- [ ] **Say so in the guide, and record the change**

`docs/cli.md` *`profile`* and *Collections* say that `-o` names a file and `--output` names the format,
and that `-o json` is refused for that reason.
`CHANGELOG.md` gains the breaking `### Changed` entry for the refusal and a `### Fixed` entry for the hints.

- [ ] **Validate and commit**

```bash
semlf check docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add cmd/profgate/client.go cmd/profgate/profile.go cmd/profgate/collect.go \
        cmd/profgate/client_test.go cmd/profgate/profile_test.go cmd/profgate/collect_test.go \
        docs/cli.md CHANGELOG.md
git commit -m "fix(cli): name the long flag behind -n and -o" -m "<body: -n failed without naming --namespace and -o json wrote a pprof file called json>"
```

---

## `collection get` prints the round a person counts, and what expires when

Closes the roadmap bullet beginning *`collection get` prints `round 0 of 1`*.

**Files:**
- Modify: `internal/client/wire.go`, `internal/client/collect.go`, `internal/client/collect_test.go`,
  `cmd/profgate/collect.go`, `cmd/profgate/collect_test.go`, `docs/cli.md`, `CHANGELOG.md`

**What exists today.**
`renderCollection` (`cmd/profgate/collect.go:359-372`) prints `id`, `state`, `origin`,
a `progress` row when `Progress.Rounds > 0`, and a `reason` row when there is one.
The progress row prints `p.Round` raw.
`Client.Wait` (`internal/client/collect.go:226-229`) prints the same index raw on the stream that `--wait` writes.
`CollectionRecord` (`internal/client/wire.go:100-106`) declares five fields,
so the four the gateway sends and a person needs — `resolvedVersion`, `finishedAt`, `expiresAt`,
and `artifact.bytes` — are decoded away.
`internal/pgo/record.go:91,94,106-107` are where the gateway writes them,
and `internal/pgo/record.go:153-156` is the artifact reference that carries `bytes`.

**What it becomes.**
Both printers add one to the index, for the reason *Decisions* gives.
`CollectionRecord` gains four fields,
each keeping the gateway's own text the way `CollectionSummary.CreatedAt` already does:

```go
ResolvedVersion string              `json:"resolvedVersion"`
FinishedAt      string              `json:"finishedAt"`
ExpiresAt       string              `json:"expiresAt"`
Artifact        *CollectionArtifact `json:"artifact"`
```

with

```go
// CollectionArtifact is the merged profile a completed record names.
type CollectionArtifact struct {
	Object string `json:"object"`
	Bytes  int64  `json:"bytes"`
}
```

The gateway sends `finishedAt` and `expiresAt` as a timestamp or `null`,
and `null` into a `string` leaves it empty rather than failing,
which is what lets one shape decode a `pending` record and a `completed` one.
Confirm that with the decoder before relying on it:
`client.Decode` is `internal/client/wire.go:155`, over `decodeOne`.

`renderCollection` appends a row for each field that carries something, in this order,
between `progress` and `reason`:

| Row | Printed when |
|---|---|
| `resolvedVersion` | it is not empty |
| `finishedAt` | it is not empty |
| `expiresAt` | it is not empty |
| `artifactBytes` | `Artifact` is not nil, printed as the number alone |

`reason` stays last, where it is today.
The object name is not printed: it is a store key, and nothing a caller can act on.
`--output json` is untouched — it copies the body, so it already carried all four.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestRenderCollectionRound`, new in `cmd/profgate/collect_test.go` | a body with `progress: {round: 0, rounds: 3}` renders `round 1 of 3`, and one with `{round: 2, rounds: 3}` renders `round 3 of 3`. Today they render `round 0 of 3` and `round 2 of 3` |
| `TestWaitStreamRound`, new in `internal/client/collect_test.go` | the stream `Wait` writes carries `round 1 of 3` for the first checkpoint. Today it carries `round 0 of 3`, which is the same defect on the other printer |
| `TestRenderCollectionCompleted`, new | a completed record with all four fields renders a `resolvedVersion`, a `finishedAt`, an `expiresAt`, and an `artifactBytes` row, each carrying the gateway's own value. Today none of the four appears |
| `TestRenderCollectionPending`, new | a pending record whose `finishedAt`, `expiresAt`, and `artifact` are `null` and whose `resolvedVersion` is empty renders none of the four rows and decodes without error |
| `TestCollectionJSONIsUntouched`, new | under `--output json` the record's bytes are copied byte for byte, so the wire `round` is still the index the gateway stored |

The red state:

```bash
go test ./cmd/profgate/ -run 'TestRenderCollection'
go test ./internal/client/ -run 'TestWaitStreamRound'
```

- [ ] **Add one to both printers and four fields to the record**

- [ ] **Say what the table shows**

`docs/cli.md` *Collections* (`:315`) already documents the progress line as `round <n> of <m>`;
add that `n` counts from one, and list the four rows `collection get` now prints.
`CHANGELOG.md` gains the breaking `### Changed` entry for the display and the `### Added` entry for the fields.

- [ ] **Validate and commit**

```bash
semlf check docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/client/wire.go internal/client/collect.go internal/client/collect_test.go \
        cmd/profgate/collect.go cmd/profgate/collect_test.go docs/cli.md CHANGELOG.md
git commit -m "fix(cli): count rounds from one and print expiry" -m "<body: a zero-based index reached two printers, and the table dropped the fields that say how long download works>"
```

---

## `collect --wait` keeps stdout for the record

Closes the roadmap bullet beginning *`collect --wait` prints the receipt and the final record on stdout*.

**Files:**
- Modify: `cmd/profgate/collect.go`, `cmd/profgate/collect_test.go`, `docs/cli.md`, `CHANGELOG.md`

**What exists today.**
`collect` under `--wait` (`cmd/profgate/collect.go:225-229`)
writes the identifier-and-state receipt to stdout in table mode,
and writes nothing at all under `--output json`.
The final record then goes to stdout too (`:249-255`),
so a table-mode caller gets two documents on one stream
and a `jq` caller gets no receipt anywhere, which is when the identifier matters most.
`docs/cli.md:316` and [`cli.md`](../specs/cli.md) *Collections* both say only the final record goes to stdout.

**What it becomes.**
The receipt goes to `env.stderr` in both modes, and the branch on `s.Output` disappears with it:
the receipt is the same two facts either way, so it prints the same way either way.
The final record keeps stdout, unchanged in both modes.
`collect` without `--wait` is untouched: there the receipt **is** the document the command produced,
so it stays on stdout and stays JSON under `--output json` (`:218-223`).

The rendering: `writeTable` takes `env.terminal`, which describes stdout (`cmd/profgate/client.go:77`)
and is now the wrong question for a stream going to stderr.
The receipt prints as two `fmt.Fprintf` lines rather than through `writeTable`:

```text
id: 7h2k9m4p6r8t0v1w3x5y
state: pending
```

That keeps it readable on a terminal and predictable in a log,
and it does not claim to be a table a script should parse — the record on stdout is that.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestCollectWaitReceiptStream`, new in `cmd/profgate/collect_test.go` | in table mode, stderr carries the two receipt lines and stdout carries the final record alone, with no `id` row before it. Today the receipt is the first thing on stdout |
| the same test's json subtest | under `--output json`, stderr carries the same two lines and stdout is exactly the final record's bytes. Today stderr carries no receipt at all |
| `TestCollectWithoutWaitIsUnchanged`, new | `collect` with no `--wait` still writes the receipt to stdout, as a table in table mode and as the response's bytes under `--output json`. Green today, and it is what stops the fix from spreading |

The red state:

```bash
go test ./cmd/profgate/ -run 'TestCollectWait'
```

- [ ] **Move the receipt**

- [ ] **Say which stream carries what**

`docs/cli.md` *Collections* says the receipt goes to stderr under `--wait` in both modes
and that stdout carries the final record alone.
`CHANGELOG.md` gains the breaking `### Changed` entry.

- [ ] **Validate and commit**

```bash
semlf check docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add cmd/profgate/collect.go cmd/profgate/collect_test.go docs/cli.md CHANGELOG.md
git commit -m "fix(cli): send the collect receipt to stderr" -m "<body: a table-mode wait put two documents on stdout and a json wait printed no identifier at all>"
```

---

## `targets --explain` says how many the selector matched

Closes the roadmap bullet beginning *`targets --explain` on a Service whose selector matches no Pod*.

**Files:**
- Modify: `cmd/profgate/read.go`, `cmd/profgate/read_test.go`,
  `docs/specs/cli.md`, `docs/cli.md`, `CHANGELOG.md`

**What exists today.**
`TargetsResponse` (`internal/client/wire.go:66-70`) already decodes `selectorMatched`,
and no renderer reads it.
The `--explain` renderer (`cmd/profgate/read.go:211-234`) prints the target table,
a blank line, and the `REASON  COUNT` table.
For a Service whose selector matches nothing, both tables are a header with no rows,
and nothing separates "the selector selected no Pod" from "it selected Pods and every one was excluded" —
which is the whole question `--explain` exists to answer.

**What it becomes.**
Between the blank line and the `REASON  COUNT` header, one key-and-value row and one more blank line,
rendered with `writeTable(env.stdout, env.terminal, nil, [][]string{{"selectorMatched", n}})`,
which is the shape the repository uses for a single record:

```text
POD                       NODE       VERSION
checkout-7c8f8c9b9-xabcd  worker-07  1.42.3

selectorMatched  3

REASON                  COUNT
pod_not_ready           2
```

and, for the empty selector:

```text
POD  NODE  VERSION

selectorMatched  0

REASON  COUNT
```

The row prints whenever `--explain` does, including when the count is zero:
zero is the answer the bullet asks for, so suppressing it would suppress the fix.
`POD` stays the first line of stdout under every combination, which is what *Reading* requires of a listing.
Without `--explain` nothing changes: no row, no blank line, no second table.

The *Reading* example in [`cli.md`](../specs/cli.md) illustrates the old rendering,
so it moves to the block above,
and one row is added to that document's *Amendments* naming *Reading* and what changed.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestTargetsExplainNamesTheSelector`, new in `cmd/profgate/read_test.go` | a response with `selectorMatched: 3`, two targets, and one exclusion renders, in order: the `POD` header, two rows, a blank line, `selectorMatched` with `3`, a blank line, the `REASON` header, one row. Today the `selectorMatched` block is absent |
| the same test's empty subtest | `selectorMatched: 0` with no targets and no exclusions renders `selectorMatched` with `0` between the two empty headers. Today the output is two headers and a blank line, which is the bullet's symptom |
| `TestTargetsWithoutExplain`, new or extended | without `--explain` the output is the target table alone: no blank line, no `selectorMatched` row, no second header. Green today, and it is what holds the change inside `--explain` |
| `TestTargetsJSONIsUntouched` | under `--output json` the body is copied byte for byte, `selectorMatched` included, exactly as *Reading* says |

The red state:

```bash
go test ./cmd/profgate/ -run 'TestTargets'
```

- [ ] **Add the row**

- [ ] **Move the example the spec draws, and record the amendment**

- [ ] **Validate and commit**

```bash
semlf check docs/specs/cli.md docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add cmd/profgate/read.go cmd/profgate/read_test.go docs/specs/cli.md docs/cli.md CHANGELOG.md
git commit -m "feat(cli): show what the selector matched" -m "<body: two empty headers could not separate selecting nothing from excluding everything>"
```

---

## One concept has one flag name

Closes the `--body` clause of the roadmap bullet beginning
*Under `--output json` an error is still one text line on stderr*.

**Files:**
- Modify: `cmd/profgate/collect.go`, `cmd/profgate/collect_test.go`,
  `docs/cli.md`, `CHANGELOG.md`

**What exists today.**
`collect` registers `--body` (`cmd/profgate/collect.go:43`) and `pgo policy set` registers `--file`
(`cmd/profgate/policy.go:26`) for the same thing: a path to a JSON document sent as the whole request.
[`cli.md`](../specs/cli.md) *Collections* now says `--file` on both,
that `collect --body` is removed, and that no alias is accepted.

**What it becomes.**
The flag, the struct field, the grammar line (`:34`), the three refusal messages (`:92`, `:96`, `:99`),
and the two doc comments that name it (`:27`, `:76`, `:87`) all say `file`.
`pgo policy set` is untouched — it already has the name.
No alias is registered: an alias would leave two names for one concept,
which is the thing being removed, and this repository does not design around a breaking change
because it has published no compatibility promise for the flag.

The flag has two spellings, and each needs its own scoped search,
because the registration and the struct field carry the bare word:

```bash
rg -n -- '--body' --glob '!docs/investigations/**' --glob '!docs/plans/cli-help.md'
rg -n 'f\.body|"body"' cmd/profgate/collect.go
```

The second search is the one that matters most:
`rg -- '--body'` finds neither `fs.StringVar(&f.body, "body", …)` (`cmd/profgate/collect.go:43`)
nor the field it writes (`:60`),
so a sweep of the first spelling alone renames the documentation and leaves the flag.
The word `body` on its own is the request body throughout that file
(`:103`, `:115`, `:195`, `:211`, `:242`, `:250`), and none of those move.

Must change:

| Hit | What it is |
|---|---|
| `cmd/profgate/collect.go:27`, `:76`, `:87` | the three comments naming the flag |
| `cmd/profgate/collect.go:34` | the grammar line |
| `cmd/profgate/collect.go:43` | the registration |
| `cmd/profgate/collect.go:60`, `:90`, `:94`, `:99` | the struct field and its three readers |
| `cmd/profgate/collect.go:92`, `:96`, `:99` | the three refusal messages |
| `cmd/profgate/collect_test.go:155`, `:163`, `:386-388` | the cases that drive it |
| `docs/cli.md:297` | the guide's sentence |

Must stay:

| Hit | Why |
|---|---|
| `docs/specs/cli.md:873` | the `Accepted` sentence that `collect --body` is removed and replaced by `collect --file`; it records the removal and is false without the old name |
| `docs/plans/roadmap.md:137` | the bullet quotes the defect, and *The plan is finished and the roadmap says where it went* restates it as what shipped |
| `docs/investigations/2026-09-03-usability-and-stability.md:116`, `:236` | frozen as of the day it ran, and never edited |
| `docs/plans/cli-help.md` | this file, which names the flag it removes |
| `TestCollectBodyFlagIsGone` | the negative test below, which proves the spelling is gone by spelling it |

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestCollectFileFlag`, the three renamed cases in `cmd/profgate/collect_test.go:155,386-388` | `--file <path>` sends the file, `--file` beside a field flag is refused naming `--file`, a non-JSON file is refused, and a missing file is refused. Today `--file` is not a flag of `collect`, so each exits 2 with `flag provided but not defined` |
| `TestCollectBodyFlagIsGone`, new | `collect payments/checkout --body /dev/null` exits 2 against `refusingTransport(t)`, which is what proves no alias survived |
| `TestCollectFileTakesAHelpSpelling`, new | `collect payments/checkout --file=--help` is not help: the name before the `=` is `file`, so the flag receives the path `--help` and the refusal names that file. This is the *Help* case *Every client command line prints its own help* leaves open, because `--file` does not exist on `collect` until here |

The red state:

```bash
go test ./cmd/profgate/ -run 'TestCollect'
```

- [ ] **Rename the flag**

- [ ] **Say so in the guide, and record the removal**

`docs/cli.md:297` names `--file`, and says in one clause that `pgo policy set` takes the same flag.
`CHANGELOG.md` gains the breaking `### Changed` entry, naming both the removal and the replacement.

- [ ] **Validate and commit**

```bash
semlf check docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add cmd/profgate/collect.go cmd/profgate/collect_test.go docs/cli.md CHANGELOG.md
git commit -m "feat(cli)!: rename collect --body to --file" -m "<body: one concept carried two names across two verbs; no alias is accepted>"
```

---

## The client says what it did and what to do next

Closes the roadmap bullet beginning *Small wording*.

**Files:**
- Modify: `internal/client/login.go`, `internal/client/contexts.go`, `internal/client/client.go`,
  `internal/client/login_test.go`, `internal/client/contexts_test.go`, `internal/client/client_test.go`,
  `cmd/profgate/contextcmd.go`, `cmd/profgate/contextcmd_test.go`, `docs/cli.md`, `CHANGELOG.md`

Six repairs, each small, each with its own test.
Confirm each test file's name with `ls internal/client` before writing;
the tests go beside the behavior they cover.

**`logout` speaks on both outcomes, on stderr.**
`Logout` (`internal/client/login.go:405-437`) prints `nothing is cached for <...>` on stdout at `:427`
and nothing at all when it deletes an entry.
Both move to `in.Stderr`, and success gains:

```text
logged out of context prod
```

built from `Settings.describe()` (`:454-459`), which names the context or the origin.
The reason both move is in *Decisions*.

**`login --context <name>` names the next command.**
`File.RecordLogin` (`internal/client/contexts.go:159-169`) creates the entry
and leaves `CurrentContext` where it was.
After the save at `internal/client/login.go:369-372`,
when `in.Settings.ContextName != ""` and it is not `f.CurrentContext`, print on stderr:

```text
context prod is not the selected context; select it with profgate context use prod
```

Nothing is selected automatically, for the reason *Decisions* gives.
A name with no entry is admitted only beside a server from the flag or the environment
(`Resolve`, `internal/client/contexts.go:368-379`),
which is the first-run shape this line follows.

**`context delete` of the selected context says the selection is gone.**
`DeleteContext` (`internal/client/contexts.go:190-210`) clears `CurrentContext` silently at `:206-208`.
`cmd/profgate/contextcmd.go:71` prints nothing on success.
Success prints on stderr:

```text
deleted context prod
```

and, when that name was the selected one, a second line:

```text
no context is selected; select one with profgate context use
```

`DeleteContext` returns whether it cleared the selection, so the message is composed where the streams are.

**The plaintext warning follows the credential, not precedes it.**
`build` (`internal/client/client.go:193-202`) prints
`profgate: warning: sending a credential over plaintext to <url>` before `c.credential.Apply`,
and `Apply` on an expired cached entry returns `no valid token; run profgate login`
(`internal/client/refresh.go:17`, reached from `:88-101`).
So a loopback `http://` gateway with an expired token warns about a credential it never sent.
`checkPlaintext` stays where it is — it must still refuse a non-loopback plaintext URL before `Apply` —
and only the warning line moves below the successful `Apply`.

**The contexts-file refusal names the file and the key, not a Go type.**
`LoadFile` (`internal/client/contexts.go:110-128`) decodes with `KnownFields(true)`,
so a misspelled key produces `field srever not found in type client.File`.
An unexported helper in `internal/client` rewrites that clause to name the key alone,
mirroring the one `internal/config` already carries;
read `nameDecodeErrorKeys` and `nameDecodeEntryKey` (`internal/config/config.go:802-819`, `:978-994`)
first and follow their shape rather than inventing a second one.
Two structures carry the rewrite, not one sentence:
the error `Decode` returns is a `*yaml.TypeError`, reached with `errors.As`, whose `Errors` are the entries,
and an unknown-key entry is `line <n>: field <name> not found in type <T>`.
The helper takes the line number, requires the `field ` prefix and the ` not found in type ` separator,
and returns an entry it does not recognize unchanged, so nothing is lost when the wording is not the one it knows.
`internal/config` already rests on both structures,
so a `yaml.v3` that changes either fails that package's tests in the same run,
rather than silently changing this message.
The contexts file needs no node walk of its own:
the fifteen keys of `File`, `Context`, and `AuthSnap` (`internal/client/contexts.go:28-29`, `:36-40`, `:46-52`)
are distinct from one another, so the name in the entry is the key to print.
The message becomes:

```text
/home/alice/.config/profgate/contexts.yaml: line 4: srever is not a contexts-file key
```

Leaving the type name in place and only prefixing the file path, which `:122` already does,
is not an option: the message the guide and the test both name is the exact contract,
and a refusal that says `client.File` tells a user about this program's source and not about their file.

**The guide describes the rendering it has.**
`docs/cli.md:192` says a single record renders as `key: value` lines.
`writeTable` with a nil header (`cmd/profgate/render.go:15-36`) writes the two cells as two columns:
tab-separated when stdout is not a terminal and space-padded when it is,
with no colon anywhere.
The sentence becomes: a single record renders as two columns, the key and the value,
padded on a terminal and tab-separated in a pipe, the same as a listing without its header line.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestLogoutSpeaks`, new | logging out of a cached context writes `logged out of context prod` to stderr and nothing to stdout, and logging out with nothing cached writes its line to stderr and nothing to stdout. Today the first writes nothing and the second writes to stdout |
| `TestLoginNamesContextUse`, new | a login under `--context prod --server <url>` with no `currentContext` writes the `context use prod` line to stderr; a login under the already-selected context writes no such line |
| `TestContextDeleteSpeaks`, new in `cmd/profgate/contextcmd_test.go` | deleting a context writes `deleted context prod` to stderr; deleting the selected one writes the second line too; deleting a name the file does not hold is unchanged |
| `TestPlaintextWarningFollowsTheCredential`, new | against a loopback `http://` gateway with an expired cached entry and no refresh token, stderr carries `no valid token` and **no** plaintext warning, and the exit code is 3; with a usable token the warning is present and the request is sent. Today the warning precedes the failure |
| `TestContextsFileKeyRefusal`, new | a contexts file carrying `srever:` is refused with a message naming the path, the line, and the key, and carrying neither `client.File` nor `not found in type` |
| `TestRenderSingleRecordIsTabbed` | `writeTable` with a nil header emits a tab and no colon — assert it if no test does today, because it is the fact the guide sentence is being corrected against |

The red state:

```bash
go test ./internal/client/ -run 'TestLogoutSpeaks|TestLoginNamesContextUse|TestPlaintextWarning|TestContextsFileKeyRefusal'
go test ./cmd/profgate/ -run 'TestContextDeleteSpeaks'
```

- [ ] **Make the six repairs**

- [ ] **Correct the guide, and record the changes**

`CHANGELOG.md` gains the breaking `### Changed` entry for `logout`'s streams
and `### Fixed` entries for the other five.

- [ ] **Validate and commit**

```bash
semlf check docs/cli.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/client/login.go internal/client/contexts.go internal/client/client.go \
        internal/client/login_test.go internal/client/contexts_test.go internal/client/client_test.go \
        cmd/profgate/contextcmd.go cmd/profgate/contextcmd_test.go docs/cli.md CHANGELOG.md
git commit -m "fix(cli): say what happened and what to run" -m "<body: logout was silent on success, a warning preceded the credential it described, and a refusal named a Go type>"
```

---

## The gateway's refusals name the next step

Closes the last roadmap bullet.

**Files:**
- Modify: `internal/httpapi/profile.go`, `internal/httpapi/server.go`,
  `internal/httpapi/pgo_collections.go`, the fixtures and tests that assert those messages,
  `docs/api.md`, `CHANGELOG.md`

**Why the gateway and not the client.**
*Decisions* gives the argument; the short form is that the client relays and does not compose,
and only the gateway knows which endpoint exists.

**The four messages.**
Each keeps its status, its code, and its details, and gains one clause after a semicolon.
Route templates are written in the form the route table already uses,
so no message interpolates a namespace or a Service it would have to be handed:

| Site | Today | Becomes |
|---|---|---|
| `internal/httpapi/profile.go:362` | `port %q is not allowed by this gateway` | `port %q is not allowed by this gateway; GET /v1/limits lists the admitted selections` |
| `internal/httpapi/profile.go:397` | `service has no eligible targets` | `service has no eligible targets; GET /v1/namespaces/{namespace}/services/{service}/targets?explain=true counts the reasons` |
| `internal/httpapi/pgo_collections.go:33` | `the service already has a live collection` | `the service already has a live collection; GET /v1/namespaces/{namespace}/services/{service}/collections lists it` |
| `internal/httpapi/server.go:448` | `pgo collection is not enabled` | `pgo collection is not enabled; the gateway's pgo.enabled is false` |

`portNotAllowed` (`:357-364`) keeps its one `details` item unchanged:
the clause is guidance and the detail is the fault, and they answer different questions.

Before writing, search for the four strings, with this file excluded because it quotes all four:

```bash
rg -n 'is not allowed by this gateway|has no eligible targets|already has a live collection|pgo collection is not enabled' \
   --glob '!docs/plans/cli-help.md'
```

Must change: the four sites in the table above;
`docs/api.md:966`, which shows the `port_not_allowed` body;
and two client-side fixtures, `internal/client/collect_test.go:328`
and `cmd/profgate/collect_test.go:342`, which quote the `collection_in_progress` message.
The fixtures are updated with the message so that what the tests relay is what the gateway sends;
that is also the check that the client still relays verbatim.

Must stay: this file, which quotes each message on both sides of the change.

- [ ] **Write the tests**

| Test | What it asserts, and how it fails today |
|---|---|
| `TestRefusalsNameTheNextStep`, new in `internal/httpapi`, one subtest per code | each of the four responses carries its code unchanged **and** an `error` string ending in the clause above. Today none of the four carries a clause. The existing `expectError` and `expectPGOError` helpers (`internal/httpapi/fixtures_test.go:2035`) compare the code alone, so this test reads the `error` string itself |
| `cmd/profgate/collect_test.go:342` | keeps asserting that the client prints `collection_in_progress: <the gateway's message>` with the message updated, which is the relay test: the client adds nothing and drops nothing |

The red state:

```bash
go test ./internal/httpapi/ -run 'TestRefusalsNameTheNextStep'
```

- [ ] **Add the four clauses**

- [ ] **Correct the guide's example, and record the change**

`docs/api.md:966` carries the `port_not_allowed` body verbatim and moves with the message.
`CHANGELOG.md` gains the `### Fixed` entry, naming the four codes.

- [ ] **Validate and commit**

```bash
semlf check docs/api.md CHANGELOG.md
mise run lint && mise run test && mise run check
git add internal/httpapi/profile.go internal/httpapi/server.go internal/httpapi/pgo_collections.go \
        internal/httpapi/server_test.go internal/httpapi/pgo_collections_test.go \
        internal/client/collect_test.go cmd/profgate/collect_test.go docs/api.md CHANGELOG.md
git commit -m "fix(api): name the next step in four refusals" -m "<body: four codes told a caller what was refused and not where to look; the client relays them unchanged>"
```

---

## The plan is finished and the roadmap says where it went

**Files:**
- Modify: `docs/plans/roadmap.md`, `docs/plans/cli-help.md`

*The gateway's refusals name the next step* carries the last commit that changes code,
so the end-to-end suite runs on it, before the pull request is updated;
the commit below adds no code for that suite to cover.

This change is the last one, so it is the one that closes the plan.
In it:

- tick all nine bullets of [`roadmap.md`](roadmap.md)'s item *Give every CLI verb a `--help` and an honest table*;
- set that item's `Shipped:` line to `Shipped: pull request #21`, which is this branch's pull request;
- set line 3 of this file to `**Status:** Done`;
- insert `**Outcome:**` as line 4, naming the range of work commits on this branch
  and what the flipping commit itself carries, in this shape:

```text
**Outcome:** commits `<first>` through `<last>` on `docs/spec-cli-help` carry the work; the commit that carries this line closes the plan.
```

`<first>` is the commit of *Every client command line prints its own help*
and `<last>` that of *The gateway's refusals name the next step*,
read from `git log --oneline main..` before the commit is written;
the flipping commit cannot name its own hash, so it names what it does instead.
`check_status` in [`check-repo.py`](../../scripts/check-repo.py)
requires `**Outcome:** ` followed by text on line 4 and nothing more,
and [`900-design-and-review-loops.md`](../../.agents/rules/900-design-and-review-loops.md)
asks that line to say where the work went.

Two roadmap bullets need their text corrected rather than only ticked,
because a ticked bullet is still read:
the `--body` bullet quotes a flag that no longer exists,
and the `-o json` bullet describes a file the client no longer writes.
Restate each as the behavior that shipped, in the item's own voice.

The deletion is the next commit, and has to be a separate one:
the tree a commit writes either holds this finished plan or does not,
which is the protocol
[`finished-documents-leave-the-tree.md`](../decisions/finished-documents-leave-the-tree.md) records.
That commit deletes this file and rewrites every link that cited it, which `check_links` enforces;
it changes nothing else.
Run `grep -rn cli-help --include='*.md' .` before the deletion to find the links.

- [ ] **Tick the bullets, write the Shipped line, and close the plan**

- [ ] **Validate and commit**

```bash
semlf check docs/plans/roadmap.md docs/plans/cli-help.md
mise run lint && mise run test && mise run check
git add docs/plans/roadmap.md docs/plans/cli-help.md
git commit -m "docs: close the CLI help plan" -m "<body: nine bullets shipped; the item names the pull request that carries them>"
```

---

## Validation

Every task ends with the block the constraints give:

```bash
mise run lint && mise run test && mise run check
```

Before the pull request is updated, the whole change also runs the end-to-end suite:

```bash
mise run test:e2e
```

[`500-validation-and-workflow.md`](../../.agents/rules/500-validation-and-workflow.md)
names `internal/client` among the eight packages that need the suite on the `current` lane before a pull request,
and this plan changes that package in five tasks.
There is a command-line lane to run it against:
the authentication scenarios drive the built binary as a separate process,
through `runClient` and `clientEnv` (`test/e2e/scenarios_auth_test.go:1324,1336`),
and assert rows out of `whoami` (`:1225-1227`, `:1430-1436`),
so a change to the client's streams, exit codes, or table shape meets a real gateway and a real issuer there.
*The gateway's refusals name the next step* changes `internal/httpapi`, which every scenario exercises.
Report what ran and what was skipped in the pull request description
([`600-git-conventions.md`](../../.agents/rules/600-git-conventions.md)).

Prose gets `semlf check` before the hook sees it,
on every Markdown file and every Go file with doc comments a task edits;
`mise run prose` covers everything changed since `main`.

---

## Risks and What This Plan Does Not Cover

- **A leaf's grammar line can still go stale.**
  The flags and the positionals a page prints are the ones the parser reads, so those cannot drift.
  The grammar line beside them is prose, and nothing compares it against either.
  The table over every command line catches a page that is missing, not a sentence inside one that is wrong.
- **The envelope-shaped test cannot prove the gateway sent the envelope.**
  An Ingress that imitates the shape is admitted,
  and the copy under `--output json` then puts that Ingress's bytes on stdout.
  [`cli.md`](../specs/cli.md) *Output and exit codes* names this and accepts it:
  what the client can decide is whether the bytes read like the contract.
- **The contexts-file rewrite reads a library's error text.**
  `yaml.v3` does not promise the wording of its unknown-field message,
  and the helper matches the `field … not found in type …` shape inside a `*yaml.TypeError`.
  This is the same exposure `internal/config` already carries on the same two structures,
  so a wording change fails that package's tests beside this one rather than passing unnoticed,
  and an entry the helper does not recognize is printed as the library wrote it.
- **The four gateway clauses are text, held by one test and by whoever reads them.**
  A route that moves leaves a message naming a route that no longer exists,
  and nothing compares the clause against the route table.
  Only the test that reads the `error` string will notice a message that stops carrying a clause at all.
- **The end-to-end harness pulls images unconditionally,**
  so a registry outage fails the suite before any scenario runs
  and the lane cannot then confirm the client changes.
  The unit tests are what stand behind every claim in this plan; the suite is the integration check on top.
- **Not covered: `auth.basic`'s and `auth.oidc`'s other refusals, the console, and the OpenAPI document.**
  No route, schema, or console file is touched.
  The four message strings change and their codes do not,
  so `/v1/openapi.json` and the console's error rendering see nothing new.
- **Not covered: `collector`.**
  It still names no implementation.
  It gains a help answer only as the bare binary's page, which is what *Help* asks for,
  and it gains no grammar line until the process exists.

---

## Self-Review

- Bullet coverage, one line each:
  `--help` on every command line and the eleven global flags
  (*Every client command line prints its own help*, *Every operator command line prints its own help*);
  `auth hash --help` reading stdin (*Every operator command line prints its own help*);
  `-n` and `-o` (*The kubectl reflexes are answered, not obeyed*);
  `round 0 of 1` and the four dropped fields
  (*`collection get` prints the round a person counts, and what expires when*);
  the `--wait` receipt (*`collect --wait` keeps stdout for the record*);
  `selectorMatched` (*`targets --explain` says how many the selector matched*);
  the json error shape, the non-envelope line, and the unknown namespace
  (*A refusal has one shape on both streams*);
  the flag name (*One concept has one flag name*);
  the six wording repairs (*The client says what it did and what to do next*);
  the four gateway messages (*The gateway's refusals name the next step*).
- Current-source facts this plan rests on, each confirmed by reading the file:
  both client flag sets write to `io.Discard` and nothing handles `flag.ErrHelp`,
  in the two client parses and the three operator ones;
  `pgoVerb` declares three multi-word subverbs and no `policy` node exists;
  `runAuth` reaches `readPassword` without looking at `args[1]`,
  and `run` builds its environment with `os.Stdin`, so only `dispatch` takes a stdin a test controls;
  the three operator sets write through `flag` to stderr;
  `dispatch` hands an operator name to `runOperator` before the global set is built,
  and an operator name behind a global flag is already a usage error;
  `globals.register` declares eleven flags, nine of which take a value, and `-n` is not one of them;
  a verb declares one grammar, one positional count, and one flag set for all of its subverbs,
  so `pgo policy get` and `delete` accept and ignore the flags only `set` reads;
  `runContext` carries the three positional checks the leaf's count would produce;
  `-o` is registered on `profile` and on `download` as a path;
  `envelope.Error` is a plain string, so an absent `error` decodes as an envelope;
  `decodeEnvelope` discards the bytes it read;
  `StatusError.Error` prints no clause;
  `Do` discards `readBounded`'s failure while `JSON` surfaces it;
  `unauthorized` reads the status off either error type, which is what keeps a non-envelope `401` on exit 3;
  `fail` takes no output mode and is called at twenty-three sites across six files;
  `Settings.Output` resolves `table`, then `PROFGATE_OUTPUT`, then the flag,
  and `Context` has no output key, so the contexts file cannot set the mode;
  `renderCollection` prints `p.Round` raw and `Client.Wait` prints the same index raw;
  `CollectionRecord` declares five fields where `internal/pgo`'s record carries
  `resolvedVersion`, `finishedAt`, `expiresAt`, and an artifact reference with `bytes`;
  `collect --wait` writes the receipt to stdout in table mode and writes none under json;
  `TargetsResponse` decodes `selectorMatched` and no renderer reads it;
  `collect` registers `--body` and `pgo policy set` registers `--file`;
  `Logout` writes its one line to stdout and writes nothing on success;
  `RecordLogin` never sets `CurrentContext`, and `Resolve` admits an unknown context name beside a server;
  `DeleteContext` clears the selection silently;
  `build` prints the plaintext warning before `Apply`, and `Apply` is what returns `no valid token`;
  `LoadFile` decodes with `KnownFields(true)` and wraps only the path,
  and `internal/config` already rewrites the same `*yaml.TypeError` entries;
  `TestReadVerbsTable` covers an empty target list and an empty namespace list and no empty Service list;
  `writeTable` with a nil header writes a tab and no colon;
  the four gateway messages carry no clause,
  and `docs/api.md` and two client fixtures quote two of them.
- Facts this plan states and does not verify:
  that `null` decoding into a `string` field leaves it empty,
  which is confirmed against `client.Decode` before it is relied on,
  in *`collection get` prints the round a person counts, and what expires when*;
  that the end-to-end command-line lane exercises the streams this work moves —
  the helpers exist and assert `whoami` rows, and no scenario asserts a receipt or an exit code today,
  so the suite is a check that nothing broke rather than a check that the fixes landed.
