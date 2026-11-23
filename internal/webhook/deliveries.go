package webhook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The refusals of a redelivery.
const (
	errorTypeDeliveryNotFound = "delivery_not_found"
	errorTypeDeliveryPending  = "delivery_pending"
	errorTypeEventExpired     = "event_expired"
	// msgDeliveryNotFound is built from a constant alone, so that a delivery
	// of another subscription is answered with the same bytes as an id that
	// names none.
	msgDeliveryNotFound = "delivery not found"
)

// deliveryStatuses are the statuses of a delivery, which the list filters
// by.
var deliveryStatuses = []string{deliveryPending, deliverySucceeded, deliveryFailed}

// record is a stored delivery as the routes answer it: nullable columns are
// nil while they have no value.
type record struct {
	ID            string
	Seq           int64
	Type          string
	Status        string
	Attempts      int
	ResponseCode  *int64
	Error         *string
	DurationMS    *int64
	CreatedAt     time.Time
	LastAttemptAt *time.Time
	NextAttemptAt *time.Time
}

// recordJSON is the delivery record: its eleven members, each always
// present, the nullable ones null while they have no value. It holds neither
// the body sent nor the response's.
type recordJSON struct {
	ID            string        `json:"id"`
	Seq           int64         `json:"seq"`
	Type          string        `json:"type"`
	Status        string        `json:"status"`
	Attempts      int           `json:"attempts"`
	ResponseCode  *int64        `json:"response_code"`
	Error         *string       `json:"error"`
	DurationMS    *int64        `json:"duration_ms"`
	CreatedAt     timefmt.Time  `json:"created_at"`
	LastAttemptAt *timefmt.Time `json:"last_attempt_at"`
	NextAttemptAt *timefmt.Time `json:"next_attempt_at"`
}

// recordOf returns the delivery record of d.
func recordOf(d record) recordJSON {
	return recordJSON{
		ID: d.ID, Seq: d.Seq, Type: d.Type, Status: d.Status, Attempts: d.Attempts,
		ResponseCode: d.ResponseCode, Error: d.Error, DurationMS: d.DurationMS,
		CreatedAt: timefmt.Time(d.CreatedAt), LastAttemptAt: timeOrNil(d.LastAttemptAt),
		NextAttemptAt: timeOrNil(d.NextAttemptAt),
	}
}

// deliveries answers GET /workspaces/:slug/webhooks/:id/deliveries: the
// subscription's delivery records, pings included, in (created_at, id)
// descending order, a page at a time, only those in the status the query
// names when it names one; a status that is none of pending, succeeded and
// failed is 400 invalid_request.
func (c *Component) deliveries(w http.ResponseWriter, r *http.Request, q request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	status := query.Get("status")
	if query.Has("status") && !slices.Contains(deliveryStatuses, status) {
		return server.InvalidRequest("status must be one of " + strings.Join(deliveryStatuses, ", "))
	}
	var after listPosition
	more, err := page.After(&after)
	if err != nil {
		return err
	}
	var from *listPosition
	if more {
		from = &after
	}
	recs, err := listDeliveries(r.Context(), c.deps.Store.Main(), q.subscription.ID, status, from, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]recordJSON, len(recs))
	for i, d := range recs {
		items[i] = recordOf(d)
	}
	return server.WritePage(w, page, items, func(o recordJSON) any {
		return listPosition{CreatedAt: time.Time(o.CreatedAt).UnixNano(), ID: o.ID}
	})
}

// redeliver answers POST
// /workspaces/:slug/webhooks/:id/deliveries/:delivery_id/redeliver in any
// subscription state: it sends the event of an ended delivery once more, at
// once, outside the order and without a retry, under the same delivery id,
// to the subscription's current url and signed with its current secret, and
// answers 200 with the record updated with this attempt. The subscription
// does not change: its position, state and last outcome follow the loop
// alone.
//
// It refuses, sending nothing and changing no record, in this order: 404
// delivery_not_found for an id that is not one of the subscription's
// deliveries, 400 invalid_request for a ping, which has no event to send,
// 503 secrets_unavailable when no key opens the signing secret, 409
// delivery_pending for a delivery the loop still has, and 410 event_expired
// when the event has left the log. A record that goes while the attempt is
// in flight is 404 after it: webhook_not_found when its subscription was
// deleted, delivery_not_found when the record was pruned.
func (c *Component) redeliver(w http.ResponseWriter, r *http.Request, q request) error {
	ctx := r.Context()
	db := c.deps.Store.Main()
	sub := *q.subscription
	d, found, err := deliveryOf(ctx, db, sub.ID, server.Param(r, "delivery_id"))
	if err != nil {
		return err
	}
	if !found {
		return server.NewError(errorTypeDeliveryNotFound, http.StatusNotFound, msgDeliveryNotFound)
	}
	if d.Type == EventPing {
		return server.InvalidRequest("a ping cannot be redelivered; send a new ping")
	}
	plain, err := c.open(sub)
	if err != nil {
		return err
	}
	if d.Status == deliveryPending {
		return server.NewError(errorTypeDeliveryPending, http.StatusConflict,
			"the delivery is still pending; the hub keeps trying it")
	}
	rec, found, err := c.eventAt(ctx, d.Seq)
	if err != nil {
		return err
	}
	if !found {
		return server.NewError(errorTypeEventExpired, http.StatusGone,
			"the event of the delivery has left the event log")
	}
	body, err := rec.MarshalJSON()
	if err != nil {
		return err
	}
	at := c.deps.Clock.Now().UTC().Truncate(time.Second)
	// A caller that leaves does not end the attempt, bounded by
	// [webhooks].timeout on its own, so that the record holds what the
	// receiver answered rather than a failure the receiver never caused.
	ctx = context.WithoutCancel(ctx)
	out := c.sender.send(ctx, attempt{
		url: sub.URL, body: body, eventType: rec.Type, seq: rec.Seq, delivery: d.ID, secret: plain,
	})
	c.reportOutcome(out)
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		recorded, err := recordRedelivery(ctx, tx, d.ID, at, out)
		if err != nil {
			return err
		}
		if recorded {
			d, recorded, err = deliveryOf(ctx, tx, sub.ID, d.ID)
			if err != nil || recorded {
				return err
			}
		}
		// The record went while the attempt was in flight: with its
		// subscription, deleted, or pruned on its own.
		_, found, err := subscriptionByID(ctx, tx, sub.ID)
		switch {
		case err != nil:
			return err
		case !found:
			return webhookNotFound()
		}
		return server.NewError(errorTypeDeliveryNotFound, http.StatusNotFound, msgDeliveryNotFound)
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, recordOf(d))
	return nil
}

// eventAt reads the event seq from the log; found is false when it has left
// it: the log reports the position before it expired, or holds another
// event first.
func (c *Component) eventAt(ctx context.Context, seq int64) (event.Record, bool, error) {
	recs, _, err := c.deps.Events.ReadAfter(ctx, seq-1, 1)
	if errors.Is(err, event.ErrPositionExpired) {
		return event.Record{}, false, nil
	}
	if err != nil {
		return event.Record{}, false, err
	}
	if len(recs) == 0 || recs[0].Seq != seq {
		return event.Record{}, false, nil
	}
	return recs[0], true, nil
}

// PruneDeliveries deletes the delivery records that have ended, succeeded
// or failed, and whose last attempt was before before, and returns how many
// it deleted. A record that ended without an attempt (switched off or
// disabled while its first was in flight or not yet possible, or its event
// gone from the log first) is judged by its creation, so that it does not
// stay for ever. A pending record stays whatever its age: the loop still
// has it. It has the shape of audit.DeliveryPruner, which the hub's
// retention step webhook_deliveries calls with a time seven days back.
func (c *Component) PruneDeliveries(ctx context.Context, before time.Time) (int64, error) {
	res, err := c.deps.Store.Main().ExecContext(ctx, `DELETE FROM webhook_deliveries
		WHERE status IN ('succeeded', 'failed') AND coalesce(last_attempt_at, created_at) < ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("prune the event webhook deliveries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune the event webhook deliveries: %w", err)
	}
	return n, nil
}

// recordColumns are the columns scanRecord reads, in its order.
const recordColumns = `id, seq, type, status, attempts, response_code, error, duration_ms,
	created_at, last_attempt_at, next_attempt_at`

// deliveryOf reads the delivery id of the subscription sub; found is false
// when the subscription has none of that id, also when another has.
func deliveryOf(ctx context.Context, q querier, sub, id string) (record, bool, error) {
	d, err := scanRecord(q.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM webhook_deliveries WHERE subscription_id = ? AND id = ?`, sub, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, fmt.Errorf("read delivery %s: %w", id, err)
	}
	return d, true, nil
}

// listDeliveries reads up to limit deliveries of the subscription sub in
// (created_at, id) descending order, after the position from when it is not
// nil, in the status status when it is not "".
func listDeliveries(ctx context.Context, db *sql.DB, sub, status string, from *listPosition, limit int) ([]record, error) {
	query := `SELECT ` + recordColumns + ` FROM webhook_deliveries WHERE subscription_id = ?`
	args := []any{sub}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if from != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, from.CreatedAt, from.CreatedAt, from.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the deliveries of event webhook %s: %w", sub, err)
	}
	defer rows.Close()
	var out []record
	for rows.Next() {
		d, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("list the deliveries of event webhook %s: %w", sub, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the deliveries of event webhook %s: %w", sub, err)
	}
	return out, nil
}

// insertPing records the ping d of the subscription sub, made at at with
// outcome out: its one attempt, ended, with no next one.
func insertPing(ctx context.Context, q querier, sub string, d delivery, at time.Time, out outcome) error {
	_, err := q.ExecContext(ctx, `INSERT INTO webhook_deliveries (id, subscription_id, seq, type, status, attempts,
		response_code, error, duration_ms, created_at, first_attempt_at, last_attempt_at)
		VALUES (?, ?, 0, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
		d.ID, sub, d.Type, statusOf(out), nullIfZero(int64(out.responseCode)), nullIfEmpty(out.failure),
		out.durationMS, at.UnixNano(), at.UnixNano(), at.UnixNano())
	if err != nil {
		return fmt.Errorf("record ping %s of event webhook %s: %w", d.ID, sub, err)
	}
	return nil
}

// recordRedelivery records the redelivery of the ended delivery id made at
// at with outcome out: one more attempt, its status, code, error and
// duration, at as the last attempt, and no next one. A delivery that is
// pending again is the loop's, which a redelivery never touches. recorded is
// false when no ended delivery id is left to record it on.
func recordRedelivery(ctx context.Context, q querier, id string, at time.Time, out outcome) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE webhook_deliveries
		SET status = ?, attempts = attempts + 1, response_code = ?, error = ?, duration_ms = ?,
			last_attempt_at = ?, next_attempt_at = NULL
		WHERE id = ? AND status != 'pending'`,
		statusOf(out), nullIfZero(int64(out.responseCode)), nullIfEmpty(out.failure), out.durationMS,
		at.UnixNano(), id)
	if err != nil {
		return false, fmt.Errorf("record a redelivery of delivery %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record a redelivery of delivery %s: %w", id, err)
	}
	return n == 1, nil
}

// statusOf is the status of a delivery whose last attempt, never retried,
// ended with out.
func statusOf(out outcome) string {
	if out.success {
		return deliverySucceeded
	}
	return deliveryFailed
}

// scanRecord scans a row of recordColumns.
func scanRecord(row interface{ Scan(dest ...any) error }) (record, error) {
	var d record
	var code, duration sql.NullInt64
	var failure sql.NullString
	var createdAt int64
	var last, next sql.NullInt64
	if err := row.Scan(&d.ID, &d.Seq, &d.Type, &d.Status, &d.Attempts, &code, &failure, &duration,
		&createdAt, &last, &next); err != nil {
		return record{}, err
	}
	if code.Valid {
		d.ResponseCode = &code.Int64
	}
	if failure.Valid {
		d.Error = &failure.String
	}
	if duration.Valid {
		d.DurationMS = &duration.Int64
	}
	d.CreatedAt = time.Unix(0, createdAt).UTC()
	d.LastAttemptAt, d.NextAttemptAt = store.TimeOf(last), store.TimeOf(next)
	return d, nil
}
