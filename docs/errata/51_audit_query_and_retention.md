# Errata: 51_audit_query_and_retention

Where the implementation of package 51 (`.specs/51_audit_query_and_retention`)
differs from what the package says, or decides what it leaves open.

## The delivery pruner is registered on the hub's composition

The package has the webhook feature register its delivery store as the
`DeliveryPruner` of the `webhook_deliveries` step through a package-level
`audit.RegisterDeliveryPruner`. A package-level registry would be shared by
every hub of a test binary, which runs many hubs at once, so the pruner is
kept on its instance, as every collaborator is
([ADR 03](../adr/03-register-removers-of-owned-data-on-the-identity-service.md),
[ADR 07](../adr/07-register-the-teardown-collaborators-on-the-workspace-service.md),
[ADR 08](../adr/08-register-the-push-collaborators-on-the-ref-writer.md)).
The webhook part registers it with
`(*hub.Composition).RegisterDeliveryPruner(audit.DeliveryPruner)` while the
hub is composed; a second registration is refused and stops the start. The
audit part's start hook builds the step with
`audit.DeliverySteps(pruner, clock)`, which calls
`PruneDeliveries(ctx, now − 7d)` and, with no pruner registered, does
nothing and succeeds. `audit.DeliveryPruner` is declared in
`internal/audit` as the package says, and `internal/audit` imports nothing
of the webhook feature.

## A workspace-bound token sees no run event of a deleted fork

The plan has the audit query add, for every caller but the admin token,
the run events of deleted task forks whose parent the caller can read. For
a workspace-bound PAT the query leaves them out (`runScope` in
`internal/audit/sources.go`): the criterion says a bound token sees only
the records of the workspaces its binding covers, the covered set holds no
deleted fork, and the event visibility rule hides the same fork's hub
events from such a token. Every other caller gets them as the plan says.

## The method label is bounded

The package labels the HTTP request metrics with the request's method in
upper case. `net/http` accepts any method token and `/metrics` needs no
credential, so a client could create a series per token it sends. Every
method other than `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`,
`OPTIONS`, `CONNECT` and `TRACE` is counted under `OTHER`
(`internal/metrics/http.go`), as the route template, not the request's
path, bounds `path`. A request whose connection the hub takes over (a
sandbox's attach) sends its status on the connection, not through the
response writer; it is counted with `101`.

## Hub events leave the event log before they are deleted

The `hub_events` and `orphans` steps delete hub events by age. Deleting an
event still in the event log would leave a hole in it, which a reader of
`GET /events/log` skips without a `410`, against
[ADR 05](../adr/05-deliver-events-by-reading-the-log-by-position.md). That
happens when the `event_log` step failed in the same run, or when
`[audit].orphan_retention` is shorter than `[events].retention`. Both steps
therefore delete only events whose `seq` is below the log's `oldest_seq`
(`internal/event/retention.go`); an orphan's event still in the log is
deleted at the first run after it has left it, so with an
`orphan_retention` shorter than `[events].retention` it is kept for
`[events].retention`.
