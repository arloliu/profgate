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

## Before every commit

Run the validation block and fix what it reports:

```bash
mise run lint && mise run test && mise run check
```

Prose gets the same treatment before the hook sees it:
run `semlf check <file>` on every Markdown file and every Go file with doc comments you wrote or edited,
and fix the findings.
`mise run prose` runs the check over everything changed since `main`.

## Before a PR

Eight packages need the end-to-end suite on the `current` lane before a PR opens:
`internal/k8s`, `internal/proxy`, `internal/pgo`, `internal/natskv`, `internal/auth`, `internal/ui`,
`internal/client`, and `deploy/`.

```bash
mise run test:e2e
```

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
