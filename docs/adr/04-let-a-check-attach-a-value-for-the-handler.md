# 04. Let a check attach a value for the handler

## Status

Accepted.

## Context

The workspace access check decides, for a caller, a slug and a need, what
the caller is on the workspace, and reads the workspace's record to do so.
The route's handler needs that answer: the caller's level and the record as
read during the decision, so that it neither reads the record again nor
decides access a second time, perhaps differently.

The plan had the check store its decision on the request's context. A
check of the pipeline is a `server.Check`, `func(*http.Request) error`: it
returns only an error, and the pipeline hands the same request to the next
check and to the handler. A check can make a context of its own with
`r.WithContext`, but that request goes nowhere, so nothing it adds reaches
the handler. Changing `server.Check` to return a request would change
every check the pipeline already holds, the authenticator's neighbours
included, for the needs of one.

## Decision

The router's handler of a REST route gives each request an empty holder for
values, in its context, before the pipeline runs. Two functions of
`internal/server`, in `internal/server/route.go`, use it:

- `server.Attach(r, key, value)` leaves `value` under `key` on the request.
  A check calls it to hand on what it found. Outside a request to a REST
  route, where there is no holder, it does nothing.
- `server.Attached(r, key)` returns the value attached under `key` and
  `true`, or `nil` and `false` when nothing was attached, also outside a
  request to a REST route.

Every later check and the handler of the same request see the values,
because the contexts the pipeline derives later (the caller's, the body's)
keep the holder. Each request has a holder of its own, so concurrent
requests never see each other's values. Keys follow the rule of context
keys: a value of an unexported type of the package that attaches, so that
no other package can read or overwrite it by accident.

The workspace access check attaches its granting decision under an
unexported key of `internal/access`, and `access.DecisionOf(r)` reads it
for the handler.

## Consequences

- `server.Check` keeps its signature, and no check installed before this
  one changes.
- A package that wants to hand a check's finding to its handlers attaches
  it and exports a reader, as `access.DecisionOf` does, rather than asking
  for a new position in the pipeline.
- A value is there only once the check that attaches it has run: a check
  earlier in the pipeline, or the handler of a route the check passes
  without deciding, reads nothing.
