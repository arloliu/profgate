# The chart's install notes are asserted as prose, and stay that way

**Decision:** `deploy/chart_test.go` keeps its assertions over the rendered `NOTES.txt`,
and keeps the probe-template machinery that renders it.
Deleting them in favour of the end-to-end suite was considered and rejected.

## Context

`helm template` never emits `NOTES.txt`, and parses every other template as a YAML manifest.
Reaching the rendered prose therefore costs a helper that copies the chart to a temporary directory,
wraps the notes in a block scalar under a template of its own, and renders that one file
(`renderNotes` and `renderNotesForAppVersion`, about sixty lines).
That cost belongs to the file being unreachable any other way,
not to the assertions written over it:
any way of testing those notes pays it.

The proposal was to delete both, on the grounds that two real defects in the notes had shipped
and neither was caught here.
Both are real:
the curl examples once used a path matching no route, fixed in `v0.3.0`;
and the TLS example lacked the certificate's own name and `--resolve`,
found while writing an end-to-end test for certificate rotation, not by these assertions.

What the assertions actually hold is narrower than "the notes mention these words",
and wider than the two escapes suggest.
The notes are a page of conditionals over four authentication modes, TLS, and PGO,
and the assertions hold each mode to the instructions it should print
and to the ones it must not:
`disabled` may not print `auth hash` or a login path,
`basic` may not print that authentication is disabled,
and `oidc` without a browser block may not print a login path.
The negative half is the load-bearing one.
An operator told to run a command their configuration does not admit is a defect no render error and no lint reports.

## Consequences

- **What is proven:** the notes name the values the operator supplied,
  and each configuration prints the instructions belonging to it and no others.
- **What is not proven:** that any instruction the notes print actually works.
  Both defects that escaped were of that kind — a path, and a missing flag —
  and neither is reachable by asserting over rendered text.
- **The nearest unwritten check** would hold every `/v1` path in the notes to a path `internal/httpapi/openapi.json` declares.
  That is the shape `TestChartPrometheusRule` already uses against the exported series
  and the error codes, and it would have caught the first of the two.
  It is not written, because it adds a check where this work was removing them;
  it wants a roadmap item of its own,
  together with the question of whether the end-to-end suite should run the notes' commands rather than read them.
- No revisit trigger is set.
  [`e2e-without-framework.md`](e2e-without-framework.md) set one on harness size,
  and it passed unnoticed until someone went looking.
  A condition nobody watches is not a mechanism;
  naming what is unproven is the more honest record.
