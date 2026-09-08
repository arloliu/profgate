# An envelope code is registered when a route can answer it

**Decision:** `internal/httpapi/codes.go` holds exactly the codes some route can write,
and a code belonging to a design that is accepted but not built is not registered ahead of it.

This supersedes one consequence of
[`collection-stays-in-the-gateway.md`](collection-stays-in-the-gateway.md):
the one reading that `collector_unavailable` "stays a registered code no route answers",
cited there to `internal/httpapi/codes.go:90-92`.
That record's decision — collection runs in the gateway process until measurement says otherwise —
stands, as does every other consequence it draws and every trigger under its *Revisit*.

## Context

The registry is not a private list.
Its own doc comment states what it is for:
no handler writes a code that is not there,
and `internal/httpapi/openapi.json` enumerates exactly that set.
A registered code therefore reaches a client as a published enum value,
and the console carries a hint keyed by code for the ones a person can meet.

So a code registered ahead of the design that answers it is not inert.
It tells a generated client to branch on an answer the gateway cannot produce,
and it gives the console a hint no response reaches.
`collector_unavailable` was both at once while the collector separation stayed unbuilt.

Registering the code late costs three edits — the constant, the enum entry, and the hint —
in the change that builds the collector and makes them true.
That is where a claim about behavior belongs anyway:
the commit seam is what is true now against what is true after,
so the edits ride with the code rather than waiting for it.

The other direction was what the superseded consequence chose,
on the reasoning that the separation is accepted design and the code would be wanted again.
It would be, and it is cheap to add back.
What it was not is free to leave in place:
the enum is part of the HTTP contract,
and removing a published value is a breaking change to it,
which is the bill a reader pays for the code having been registered early.

## Consequences

- `collector_unavailable` is not a registered envelope code.
  No constant declares it, the OpenAPI enum does not list it,
  and the console has no hint for it.
- The superseded consequence's citation reads as a defect rather than a fact:
  `internal/httpapi/codes.go:90-92` names three lines that now carry a different constant
  and the end of the block.
- [`pgo.md`](../specs/pgo.md) *What this build does not carry* is where the current state is read.
  It says the code is not registered, and says that sentence overtakes the earlier record.
- A reader who opens the superseded record directly gets no signal that it has been superseded.
  Decision records carry no status field, `docs/decisions/` has no index,
  and `scripts/check-repo.py` inspects neither,
  so the pointer runs one way only — from here to there, and from the spec to both.
  Closing that would mean giving decision records a status field or an index,
  which is a change to how the whole directory works and is not made here.
- When the collector separation is built, the code is registered again in that change,
  with its enum entry and its hint.
  The conditions that would start that work are the superseded record's, unchanged.
