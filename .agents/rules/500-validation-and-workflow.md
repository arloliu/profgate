# 500 - Validation and Workflow

Apply before every commit and before opening a PR.

## Before every commit

Run the validation block and fix what it reports:

```bash
mise run lint && mise run test && mise run check
```

Prose gets the same treatment:
run `semlf check <file>` on every Markdown file you wrote or edited and fix the findings.

## Before a PR

Seven packages need the end-to-end suite on the `current` lane before a PR opens:
`internal/k8s`, `internal/proxy`, `internal/pgo`, `internal/natskv`, `internal/auth`, `internal/ui`, and `deploy/`.

```bash
mise run test:e2e
```

Lease reclaim, the publication protocol, and the NATS preflight meet a real cluster only in the PGO scenarios.
Their unit tests run against an embedded server and never see a Pod restart.
The browser login round trip, the users file and cookie key read from a mounted Secret,
and a signing-key rotation at the issuer meet a real issuer only in the authentication scenarios;
their unit tests drive fakes.
The console's shell, hashed assets, headers, and login return meet a real gateway and issuer only in those same scenarios;
the port-control model in `internal/ui/static/portmodel.js` runs under the goja interpreter in a Go test,
and the rest of the page's JavaScript runs in none,
so a change to `internal/ui/static/` outside `portmodel.js` also needs a check in a browser against a running gateway.

Report what ran and what was skipped in the PR description ([600](600-git-conventions.md)).
