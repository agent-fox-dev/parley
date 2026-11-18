# Errata: 43_forge_webhook_ingress

Where the implementation of package 43 (`.specs/43_forge_webhook_ingress`)
differs from what the package says, or settles what it leaves open.

## The deliveries remover has the remover's signature

The package describes the remover of a workspace's deliveries as "a function
of the transaction and one slug that returns the ids of the tokens it
revoked". The workspace service's removers are
`workspace.Remover func(ctx context.Context, tx *sql.Tx, slug string) error`;
token revocation is the separate `TokenRevoker`
([ADR 07](../adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)).
`forge.RemoveDeliveries` (`internal/forge/delivery.go`) has that signature,
revokes nothing, and is registered with
`c.workspaces.Teardown().RegisterRemover` in `composeForge`.

## The ingress is a surface outside the REST API

The package registers `POST /hooks/forge/:slug` "outside the /api/v1 mount
as a public route". Every REST route lies under `/api/v1` and passes the
rate limiter, so the ingress is mounted on the server's mux with
`c.Handle(forge.IngressPattern, comp.Ingress())`
(`IngressPattern = "POST /hooks/forge/{slug}"`, `internal/forge/ingress.go`),
which no REST check reaches: the handler does its own body limit and writes
its errors with the error envelope helper. Another method on the path is
answered by the server's own `405` or `404`.

## The forge component gains three dependencies

The webhook routes change a secret and record its event in one store
transaction, which the component's `EmitNow` emitter of the forge writes
cannot do. `forge.Deps` gains `Store` (the transactional store),
`SecretWriter` (the secret component's `Put`, `Delete` and `Available`) and
`Log` (the event component's `Emit` inside a transaction), set by the start
hook `forge` in `internal/hub/forge.go`.

## The show-once note depends on the answer

`parley forge webhook set` writes its note only when the hub's answer holds
`secret`. A command's `ShowsOnce` writes the warning after every answer, so
the command has its own run step, which writes the same line
(`this value is shown once and will not be shown again`) only then.

## A test of the hub registers its failing task under another name

`TestRun_LogsAFailingMaintenanceTaskAtEveryPassAndKeepsServing`
(`internal/hub/hub_test.go`) registered its own failing task under the name
`forge_delivery_pruning`, which the hub now registers itself; a name
registered twice stops the start. The test's task is named `failing_task`;
the test does not depend on the task's place in the order.

## Where the checks of a few criteria live

- The check of an id holding the byte `0x7F` calls the ingress handler
  directly with a recorded request: Go's HTTP client refuses to send a
  control byte in a header value, and the server refuses one with a plain
  `400` before any handler runs. Every other row of the ingress checks posts
  over the hub's listener.
- The check of a pruning pass whose store fails makes the delete fail with a
  SQLite `BEFORE DELETE` trigger that raises an error, dropped afterwards,
  rather than with a closed database handle, so that the hub's real
  maintenance loop runs the task and its error is read from the loop's log
  line.
- The check that no relayed event holds the webhook secret asserts it on the
  stored events of the recording and ingress tests: the reduction is never
  given the secret, so no draft can hold it.
