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
