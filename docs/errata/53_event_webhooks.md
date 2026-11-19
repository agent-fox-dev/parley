# Errata: 53_event_webhooks

Where the implementation of package 53 (`.specs/53_event_webhooks`) differs
from what the package says, or decides what it leaves open.

## The hub part exists from the first task, and keeps the keys

The package composes the webhook component in its tenth task. The binding
guard, the scope check and the access rule the routes rely on are installed
by the hub, so `internal/hub/webhook.go` (`composeWebhooks`, `Webhooks`,
`AdjustWebhooks`) was created with the first route and grown with the
tasks; the route tests run through the hub's router with `hubtest`.

The component takes `Keys func() secret.Keys` and `Available func() bool`
in its `Deps` rather than the secret component: `SealValue` and `OpenValue`
need the operator's keys, which `*secret.Component` does not export
([ADR 06](../adr/06-load-the-secret-keys-before-the-stores-open.md)). So
`composeSecrets` keeps the keys its configuration check loads on the
composition (`secretKeys`), and the webhook part hands them on.

## The show-once note is the CLI's shared warning

The package has `parley webhook create` and `rotate-secret` write a note on
stderr that the secret is shown only once, and its check looks for the
words "only once". Every command that prints a secret the hub shows once
writes the same line
([ADR 02](../adr/02-declare-every-cli-command-through-one-helper.md)),
`warning: this value is shown once and will not be shown again`, and these
two do too. It is written only when the answer carries `secret`: an
`Idempotency-Key` replay of a create answers without it.

## A failed ping or redelivery is a sixth CLI error kind

The package fixes exit status `1` and `error_type` `delivery_failed` with
`code` `0` for a failed `ping` or `redeliver`. None of the CLI's five error
kinds has that type, so `internal/cli/errors.go` adds `delivery_failed`,
mapped to `1`; ADR 02 lists it.

## PruneDeliveries returns an int64

The package's design decision 14 has `PruneDeliveries(ctx, before)` return
the number deleted as an `int`. `audit.DeliveryPruner`, which the code of
package 51 declares, returns `(int64, error)`, and the method has that
signature. It is a method of `*webhook.Component`, which holds the delivery
store, so the hub registers the component.

## One attempt is bounded as a whole by [webhooks].timeout

The package measures `[webhooks].timeout` from the connect to the response
status. The sender bounds the whole attempt with it: resolving the host,
the dial and the TLS handshake included, so that no attempt outlasts the
timeout. `duration_ms` is measured in wall time.

## A recorded delivery moves the position to just before its event

The package moves the position past an event when its delivery succeeds or
is given up. Recording the `pending` delivery moves it to just before the
event, past the events in between that the subscription does not receive,
so that a restart does not read them again; the position reaches the event
only on success or give-up, as the package says.

## A vanished pending event is counted once

When the event of a `pending` delivery has left the event log, the delivery
becomes `failed` and the position moves past the event or to just before
the log's oldest event, whichever is later, with one `failed` counted, not
one for the delivery and one more for the expired position.

## Ending a delivery without an attempt reports no metric

A switch-off (`active: false`) and the 72-hour disabling end the `pending`
delivery as `failed` without an attempt, so neither adds to
`parley_webhook_deliveries_total`; only an attempt, a give-up, a ping, a
redelivery and an event skipped because it left the log are counted. An
attempt in flight when its delivery is ended this way still counts once:
`ok` when it succeeded, `failed` when not, since no retry follows.

## A delivery ended without an attempt is pruned by its creation

Design decision 14 prunes the ended records whose last attempt is older
than 7 days. A record that ended without an attempt has none, so it is
pruned 7 days after its creation.

## IPv6 forms that carry an IPv4 address are judged by it

Design decision 10 judges an IPv4-mapped IPv6 address by its IPv4 address.
The other IPv6 forms that carry an IPv4 address are judged by it too:
IPv4-compatible (`::a.b.c.d`, other than `::` and `::1`), the NAT64
well-known prefix `64:ff9b::/96` and 6to4 `2002::/16` (the IPv4 address in
bits 16 to 47), so that no spelling of an address of the hub's networks
escapes the ranges. No IPv4 range is added.

## The OnCommit callback is registered by the first loop

The package registers the delivery loops' wake-up with the event component
when the hub is composed. Any `OnCommit` callback makes the event component
read the store every 100 ms
([ADR 05](../adr/05-deliver-events-by-reading-the-log-by-position.md)), so
the component registers its callback when its first loop starts, and a hub
without subscriptions stays idle. Each loop's first read covers the commits
made before.

## A null member of a create body is a bad value

The package does not say how a JSON `null` is read. A `null` `url`,
`types`, `include_forks` or `description` is refused `400`
`invalid_request` naming the field, as a value of the wrong type, not taken
as absent.

## Where the checks of a few criteria live

- The route checks run through a whole hub started by `hubtest`, not a bare
  store with the schema steps applied, and "u_1" is the generated owner's
  id. The sender, the destination rule and the configuration are tested
  directly in the package's internal tests; a check that drives a route
  from one of them (the undecryptable secret's ping and redelivery, the
  destination's create and `PATCH`) lives in the external test file of the
  same area, because an internal test cannot import `hubtest`.
- The stub event log and stub sender of a few checks are the real event
  component, expired with `Expire` on the controlled clock, and recording
  `httptest` receivers on loopback.
- The metrics and the converter are checked through the hub: the counts
  from `GET /metrics`, the queue depth through its registered source, and
  the converter run by the hub's `secret_reencryption` pass under a real key
  rotation, its counts read from the `secret re-encryption` line.
- The binding guard writes its own `parley.token.bound_refused` event when
  it refuses a bound token, so the refusal check allows those events and
  compares the subscriptions without the columns the loop moves.
