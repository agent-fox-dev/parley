package forge

import (
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// DeliveryRetention is how long the hub keeps a webhook delivery: a
// maintenance pass deletes the deliveries received more than this before
// it. It is fixed; no configuration key changes it.
const DeliveryRetention = 7 * 24 * time.Hour

// deliveryItem is a delivery as the delivery log lists it.
type deliveryItem struct {
	DeliveryID string    `json:"delivery_id"`
	Forge      ForgeKind `json:"forge"`
	Event      string    `json:"event"`
	Action     string    `json:"action"`
	// Target is null when the payload named neither of the workspace's
	// repositories.
	Target     *Target      `json:"target"`
	Outcome    Outcome      `json:"outcome"`
	Relayed    []string     `json:"relayed"`
	ReceivedAt timefmt.Time `json:"received_at"`

	position DeliveryPosition
}

// deliveryRoutes are the routes of a workspace's delivery log. The log is
// read in every workspace state, so that a developer can see what an
// archived workspace received; like the webhook routes they refuse a task
// fork, which receives no deliveries.
func (c *Component) deliveryRoutes() []server.Route {
	return []server.Route{{
		Method: http.MethodGet, Pattern: "/workspaces/:slug/forge/deliveries", Scope: ScopeRead,
		Marks:   map[string]string{access.MarkNeed: string(access.NeedRead)},
		Handler: c.serveDeliveries,
	}}
}

// serveDeliveries answers a page of the deliveries the workspace recorded,
// newest first and, within a second, last recorded first, received at or
// after the query parameter since when it is given. The list's cursor is
// bound to the workspace and to since by the list helper. Other query
// parameters, target among them, are ignored.
func (c *Component) serveDeliveries(w http.ResponseWriter, r *http.Request) error {
	d, _ := access.DecisionOf(r)
	if err := refuseFork(d.Record); err != nil {
		return err
	}
	var q DeliveryQuery
	if query := r.URL.Query(); query.Has("since") {
		since, err := time.Parse(time.RFC3339, query.Get("since"))
		if err != nil {
			return server.InvalidRequest("since must be an RFC 3339 instant, such as 2026-10-21T09:00:00Z")
		}
		q.Since = since
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after DeliveryPosition
	if ok, err := page.After(&after); err != nil {
		return err
	} else if ok {
		q.After = &after
	}
	q.Limit = page.Limit + 1
	list, err := ListDeliveries(r.Context(), c.deps.Store.Main(), d.Record.Slug, q)
	if err != nil {
		return err
	}
	items := make([]deliveryItem, 0, len(list))
	for _, dl := range list {
		items = append(items, deliveryItem{
			DeliveryID: dl.DeliveryID, Forge: dl.Forge, Event: dl.Event, Action: dl.Action, Target: dl.Target,
			Outcome: dl.Outcome, Relayed: dl.Relayed, ReceivedAt: timefmt.Time(dl.ReceivedAt), position: dl.Position(),
		})
	}
	return server.WritePage(w, page, items, func(it deliveryItem) any { return it.position })
}
