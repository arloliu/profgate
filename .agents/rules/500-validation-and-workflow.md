# 500 - Validation and Workflow

Apply before every commit and before opening a PR.

## Once per clone

```bash
mise run hooks
```

This points `core.hooksPath` at `.githooks/`.
The `pre-commit` hook runs `semlf --base HEAD`,
which reports only the semantic-linefeed findings the lines you changed own,
and refuses the commit on a fused or column-wrapped line;
an old finding elsewhere in a file you touched does not block you.
The `commit-msg` hook holds the message to [600](600-git-conventions.md).
Both need `semlf` on `PATH` and fail plainly when it is missing.
A pull request runs the same check against its base branch in CI.

A refused message stops the commit after `git commit` has already run,
and nothing in the output says the work did not land.
A commit is finished when `git log --oneline -1` shows it and `git status --short` is clean;
read both before moving on.
The fix for a finding on the body is to break the line at a clause boundary,
and to pass a long message with `-F` rather than a stack of `-m` flags.

## Before every commit

Run the validation block and fix what it reports:

```bash
mise run lint && mise run test && mise run check
```

Prose gets the same treatment before the hook sees it:
run `semlf check <file>` on every Markdown file and every Go file with doc comments you wrote or edited,
and fix the findings your own diff owns.
`semlf check` reads the whole file and reports on prose you did not write;
rewrapping an untouched line makes a diff that has to be reviewed for nothing.
The hook's `semlf --base HEAD` is the scope that decides.
`semlf` analyzes Markdown and Go doc comments and never YAML,
so a chart template draws no finding from it.
`mise run prose` runs the check over everything changed since `main`.

## When a failure is not about your change

`helm template --show-only <template>` renders the whole chart before it selects one file.
A failure in any other template fails the render,
and the error names the template that failed rather than the one asked for.
When a single-template render fails, check that the values given render every template.

Editor and language-server diagnostics can report a build that no longer matches the tree,
most often right after a rename or a signature change.
The validation block above decides;
a diagnostic it does not reproduce is noise.

## A skip is not a pass

The chart checks in `deploy/chart_test.go` shell out to `helm` and skip when they cannot find it.
The versions pinned in `mise.toml` are not on a bare `PATH`,
so `go test ./deploy/` from a plain shell skips every one of them and still prints `ok`.
Run the suite through `mise run test` or `mise exec --` so those checks execute at all.

A pin that does not resolve produces the same silence.
Confirm a version installs before pinning it:

```bash
mise x <tool>@<version> -- <tool> --version
```

## Before a PR

Eight packages need the end-to-end suite on the `current` lane before a PR opens:
`internal/k8s`, `internal/proxy`, `internal/pgo`, `internal/natskv`, `internal/auth`, `internal/ui`,
`internal/client`, and `deploy/`.

```bash
mise run test:e2e
```

The harness pulls the NATS, Dex, and Keycloak images into the cluster before any scenario runs,
and exits when a pull fails.
A failure at that point is the registry rather than the change under test.
`PROFGATE_E2E_REGISTRY` substitutes the kind node image only and is no way around it.

Lease reclaim, the publication protocol, and the NATS preflight meet a real cluster only in the PGO scenarios.
Their unit tests run against an embedded server and never see a Pod restart.
The browser login round trip, the users file and cookie key read from a mounted Secret,
and a signing-key rotation at the issuer meet a real issuer only in the authentication scenarios;
their unit tests drive fakes.
The console meets a real gateway and issuer only in the authentication and console scenarios:
its shell, its assets at their stable paths, their headers, and the login return.
Its three model modules — `portmodel.js`, `targetmodel.js`, and `collectionmodel.js` —
run under the goja interpreter in Go tests,
and `app.js` itself runs in the two browser scenarios, `console-oidc` and `console-basic`,
which drive a headless Chromium and skip by name on a machine that has none,
so a change to `internal/ui/static/` needs the suite on a machine with a browser installed.
The command-line client's device grant, refresh, and port-forward transport meet a real issuer only there too,
which run the client as a separate process against Dex and Keycloak;
its unit tests drive `httptest` servers.

Report what ran and what was skipped in the PR description ([600](600-git-conventions.md)).
