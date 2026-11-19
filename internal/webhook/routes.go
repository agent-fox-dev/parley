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

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The patterns of a workspace's webhook collection and of one subscription
// in it.
const (
	collection = "/workspaces/:slug/webhooks"
	item       = collection + "/:id"
)

// Routes are the webhook routes, for the server's router, each declared
// through route, so that every one refuses a workspace-bound token, needs
// level owner on its workspace and refuses a task fork. The create accepts
// an Idempotency-Key, and a replay answers without the signing secret.
func (c *Component) Routes() []server.Route {
	create := c.route(http.MethodPost, collection, scopeWrite, c.create)
	create.Create, create.ShowOnce = true, []string{"secret"}
	return []server.Route{
		create,
		c.route(http.MethodGet, collection, scopeRead, c.list),
		c.route(http.MethodGet, item, scopeRead, c.get),
		c.route(http.MethodPatch, item, scopeWrite, c.update),
		c.route(http.MethodDelete, item, scopeWrite, c.remove),
		c.route(http.MethodPost, item+"/rotate-secret", scopeWrite, c.rotate),
		c.route(http.MethodPost, item+"/ping", scopeWrite, c.ping),
		c.route(http.MethodGet, item+"/deliveries", scopeRead, c.deliveries),
		c.route(http.MethodPost, item+"/deliveries/:delivery_id/redeliver", scopeWrite, c.redeliver),
	}
}

// states are the states a subscription is in, which the list filters by.
var states = []string{StateActive, StateFailing, StateDisabled}

// listPosition is a subscription's place in the list's order: created_at,
// in nanoseconds as stored, then id.
type listPosition struct {
	CreatedAt int64  `json:"t"`
	ID        string `json:"id"`
}

// list answers GET /workspaces/:slug/webhooks: the workspace's
// subscriptions in (created_at, id) ascending order, a page at a time, only
// those in the state the query names when it names one; a state that is
// none of active, failing and disabled is 400 invalid_request.
func (c *Component) list(w http.ResponseWriter, r *http.Request, q request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	state := query.Get("state")
	if query.Has("state") && !slices.Contains(states, state) {
		return server.InvalidRequest("state must be one of " + strings.Join(states, ", "))
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
	subs, err := listIn(r.Context(), c.deps.Store.Main(), q.slug(), state, from, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]objectJSON, len(subs))
	for i, s := range subs {
		items[i] = objectOf(s)
	}
	return server.WritePage(w, page, items, func(o objectJSON) any {
		return listPosition{CreatedAt: time.Time(o.CreatedAt).UnixNano(), ID: o.ID}
	})
}

// withPending is the subscription object with pending, the answer of a
// read of one subscription.
type withPending struct {
	objectJSON
	Pending int `json:"pending"`
}

// get answers GET /workspaces/:slug/webhooks/:id: the subscription and the
// number of events pending for it.
func (c *Component) get(w http.ResponseWriter, r *http.Request, q request) error {
	n, err := c.pending(r.Context(), *q.subscription)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, withPending{objectOf(*q.subscription), n})
	return nil
}

// pendingBatch is how many events pending reads from the log at a time.
const pendingBatch = 500

// pending is the number of events after the position of s that s receives,
// the event of its pending delivery included, once, whether or not a changed
// filter still matches it; 0 when s is disabled, which receives nothing. A
// position whose events have left the log counts from the oldest event the
// log holds.
func (c *Component) pending(ctx context.Context, s Subscription) (int, error) {
	if s.State == StateDisabled {
		return 0, nil
	}
	n := 0
	seq, found, err := pendingDeliverySeq(ctx, c.deps.Store.Main(), s.ID)
	if err != nil {
		return 0, err
	}
	if found {
		n++
	}
	after := s.Position
	for {
		recs, examined, err := c.deps.Events.ReadAfter(ctx, after, pendingBatch)
		var expired *event.PositionExpiredError
		if errors.As(err, &expired) {
			after = expired.OldestSeq - 1
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, rec := range recs {
			if found && rec.Seq == seq {
				continue
			}
			if ok, err := c.receives(ctx, s, rec); err != nil {
				return 0, err
			} else if ok {
				n++
			}
		}
		if len(recs) < pendingBatch {
			return n, nil
		}
		after = examined
	}
}

// receives reports whether s receives rec: an event an owner of its
// workspace may see, of the workspace itself or, when s includes forks, of
// one of its task forks, whose type matches s's types.
func (c *Component) receives(ctx context.Context, s Subscription, rec event.Record) (bool, error) {
	visible, err := c.deps.Events.VisibleToOwnerOf(ctx, s.Workspace, rec)
	if err != nil || !visible || rec.Workspace == nil {
		return false, err
	}
	if !s.IncludeForks && *rec.Workspace != s.Workspace {
		return false, nil
	}
	return matchesTypes(s.Types, rec.Type), nil
}

// matchesTypes reports whether typ matches one of the entries of types: an
// exact type, or a prefix ending in * that matches every type beginning with
// the text before the *, the event stream's filter syntax.
func matchesTypes(types []string, typ string) bool {
	for _, entry := range types {
		if prefix, ok := strings.CutSuffix(entry, "*"); ok {
			if strings.HasPrefix(typ, prefix) {
				return true
			}
		} else if typ == entry {
			return true
		}
	}
	return false
}

// listIn reads up to limit subscriptions of the workspace slug in
// (created_at, id) order, after the position from when it is not nil, in
// the state state when it is not "".
func listIn(ctx context.Context, db *sql.DB, slug, state string, from *listPosition, limit int) ([]Subscription, error) {
	query := `SELECT ` + subscriptionColumns + ` FROM webhook_subscriptions WHERE workspace = ?`
	args := []any{slug}
	if state != "" {
		query += ` AND state = ?`
		args = append(args, state)
	}
	if from != nil {
		query += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, from.CreatedAt, from.CreatedAt, from.ID)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the event webhooks of %s: %w", slug, err)
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("list the event webhooks of %s: %w", slug, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the event webhooks of %s: %w", slug, err)
	}
	return out, nil
}

// pendingDeliverySeq is the seq of the event of the pending delivery of the
// subscription id; found is false when it has none.
func pendingDeliverySeq(ctx context.Context, q querier, id string) (int64, bool, error) {
	var seq int64
	err := q.QueryRowContext(ctx,
		`SELECT seq FROM webhook_deliveries WHERE subscription_id = ? AND status = 'pending'`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the pending delivery of event webhook %s: %w", id, err)
	}
	return seq, true, nil
}
