package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/google/uuid"
)

// EventPing is the type of the test delivery a ping sends. It is never
// written to the event log.
const EventPing = "parley.webhook.ping"

// actionPing is the action of a ping's body.
const actionPing = "ping"

// ping answers POST /workspaces/:slug/webhooks/:id/ping in any subscription
// state and workspace status: one signed test delivery, sent at once outside
// the order of the subscription's deliveries and never retried, whose body
// is an event record of type parley.webhook.ping with seq 0 that the log
// does not hold. It records the delivery with its one attempt and answers 200
// with the record whether the attempt succeeded or failed; the subscription
// does not change, since a test says nothing about the events it is due.
// A signing secret no key opens is 503 secrets_unavailable, and nothing is
// sent or recorded.
func (c *Component) ping(w http.ResponseWriter, r *http.Request, q request) error {
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return server.Internal("the webhook ping found no caller")
	}
	sub := *q.subscription
	plain, err := c.open(sub)
	if err != nil {
		return err
	}
	at := c.deps.Clock.Now().UTC().Truncate(time.Second)
	actorType, actorID := actorOf(caller)
	body, err := event.Record{
		ID: uuid.NewString(), Type: EventPing, Time: at, Workspace: &sub.Workspace,
		ActorType: actorType, ActorID: actorID, ResourceType: resourceType, ResourceID: sub.ID,
		Action: actionPing, Metadata: json.RawMessage("{}"),
	}.MarshalJSON()
	if err != nil {
		return err
	}
	d := delivery{ID: uuid.NewString(), Type: EventPing}
	out := c.sender.send(r.Context(), attempt{
		url: sub.URL, body: body, eventType: EventPing, delivery: d.ID, secret: plain,
	})
	// The request's context may have ended with the attempt; the record is
	// still the outcome the receiver saw.
	ctx := context.WithoutCancel(r.Context())
	if err := insertPing(ctx, c.deps.Store.Main(), sub.ID, d, at, out); err != nil {
		return err
	}
	c.reportOutcome(out)
	rec, _, err := deliveryOf(ctx, c.deps.Store.Main(), sub.ID, d.ID)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, recordOf(rec))
	return nil
}

// open opens the signing secret of sub with the operator's keys as they are
// now and hands it to the hub's scrubber, which a restart left without it. A secret no key opens, because there is none or the one that sealed
// it is gone, is 503 secrets_unavailable: nothing leaves the hub unsigned.
func (c *Component) open(sub Subscription) (string, error) {
	plain, err := secret.OpenValue(c.deps.Keys(), binding(sub.ID), sub.Sealed)
	if errors.Is(err, secret.ErrUnavailable) || errors.Is(err, secret.ErrUnreadable) {
		return "", secret.UnavailableError
	}
	if err == nil {
		c.learn(plain)
	}
	return plain, err
}

// reportOutcome counts an attempt that is never retried, a ping's or a
// redelivery's: ok when it succeeded, failed when not.
func (c *Component) reportOutcome(out outcome) {
	if out.success {
		c.report(resultOK)
	} else {
		c.report(resultFailed)
	}
}
