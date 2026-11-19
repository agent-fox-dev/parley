# Errata: 49_agent_sessions_and_usage

Where the implementation of package 49 (`.specs/49_agent_sessions_and_usage`)
differs from what the package says, or decides what it leaves open.

## The operations are methods of the session component

The package names the operations `session.Check`, `session.Record`,
`session.CloseActive`, `session.OnUsage` and `session.ActiveCounts`. They
need the store, the clock, the access rule and the event log of one hub,
and a test binary runs many hubs at once, so they are methods of
`*session.Component`, reached through `(*hub.Composition).Sessions()`, as
every collaborator is kept on its instance
([ADR 03](../adr/03-register-removers-of-owned-data-on-the-identity-service.md),
[ADR 07](../adr/07-register-the-teardown-collaborators-on-the-workspace-service.md),
[ADR 08](../adr/08-register-the-push-collaborators-on-the-ref-writer.md)).
`session.ErrUnknownSession`, `session.Link` and its constructors
(`ForkLink`, `SandboxLink`, `VersionLink`, `PushLink`, `CommitLink`,
`ClaimLink`, `LandLink`) are of the package. `Check` takes the caller as an
`identity.Principal`; the adapters of the consumers whose caller is of
another type (`change.Caller`, `awareness.Actor`, `gitserver.PushCaller`)
build one from it: the admin token, or a user id.

## The change linker is registered, not passed at construction

The package has the change feature take its session linker at
construction. The change component takes it with
`(*change.Component).RegisterSessionLinker`, as its other collaborators,
and `internal/hub/session.go` registers it there, after the change part has
created the component.

## A fork link names the parent and the fork

The package names the key of a fork link, the fork's slug, and not its
workspace. A fork link's workspace is the parent the fork was created from,
the workspace of the fork create that records it, and its key the fork's
slug; `forks` lists the keys.

## Sessions outlive their workspace, and no remover is registered

Issue #130 asked for a remover of session records beside the session
closer. A session, its usage records and its links stay when their
workspace is deleted, so that the creator, the admin token and the readers
of a fork's parent keep reading them; their removal is package 51's
retention. `internal/hub/session.go` registers the session closer alone.
`change.ChangesOfSession` is not used: a session's `changes` come from its
version links.

## A report without `until` counts the current second

The package defaults `until` to now, excluded, and normalises the bounds to
whole seconds. Usage records are stored with the whole second they were
reported in, so a window that ended at now truncated left out the usage
reported in the current second: an agent that reported usage and read the
cost at once did not see it. Without `until`, the window ends at now rounded
up to a whole second (`costPeriodOf` in `internal/session/cost.go`); a given
`until` is truncated as the package says.

## Where the package is silent

- A JSON `null` member of `POST /sessions` counts as absent. Labels are
  counted in characters (runes).
- `GET /sessions` refuses a parameter given twice, an empty `workspace`,
  and `include_forks=false` without `workspace`, each `400
  invalid_request`; a `since` or `until` with a fraction of a second is
  moved up to the next whole second, so that `since` stays inclusive and
  `until` exclusive against whole-second start times.
- The check of the session list says that dave, the owner of `other`, lists
  his own sessions; by the criterion's rule, the sessions of every
  workspace in the caller's readable set, he also lists the admin token's
  session on `other`, as `GET /sessions/:id` lets him read it.
- `CloseActive` refuses to run on a component bound without an event log,
  so that no session is closed without its event.

## A sandbox named in a fork create cannot name a session explicitly

The sandbox create asks the linking rule without a transaction
([erratum 45](45_sandbox_creation_and_credentials.md)). Inside a fork
create the fork is not committed yet, so the rule finds no record for it,
and a `sandbox.session_id` given explicitly is refused `400
unknown_session`. A sandbox that names no session takes the fork's, which
the fork create linked.
