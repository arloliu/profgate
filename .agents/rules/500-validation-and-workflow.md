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

A PR that touches `internal/k8s`, `internal/proxy`, or `deploy/` runs the end-to-end suite on the `current` lane first:

```bash
mise run test:e2e
```

Report what ran and what was skipped in the PR description ([600](600-git-conventions.md)).
