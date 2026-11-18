package forge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// maxEventBytes is the most bytes of a delivery's event name the hub keeps,
// so that a record has a bounded size.
const maxEventBytes = 64

// Delivery is one recorded webhook delivery of a workspace. It holds what
// the hub made of the payload, never the payload itself.
type Delivery struct {
	// Seq is the delivery's place in the order deliveries were recorded
	// in, which orders deliveries received in the same second.
	Seq int64
	// DeliveryID is the forge's id of the delivery, unique per workspace.
	DeliveryID string
	Forge      ForgeKind
	// Event is the delivery's event name, cut to 64 bytes.
	Event string
	// Action is the payload's action, cut to 64 bytes; "" when it names
	// none.
	Action string
	// Target is the workspace's repository the payload names, nil for
	// neither.
	Target  *Target
	Outcome Outcome
	// Relayed are the types of the events the delivery relayed, in order;
	// empty, never nil, unless Outcome is OutcomeRelayed.
	Relayed []string
	// ReceivedAt is when the hub recorded the delivery, in whole seconds
	// UTC.
	ReceivedAt time.Time
}

// DeliveryPosition is a delivery's place in a workspace's list, which is
// newest first and, within a second, last recorded first. It encodes as
// JSON, so that it can be a list cursor's position.
type DeliveryPosition struct {
	ReceivedAt time.Time `json:"received_at"`
	Seq        int64     `json:"seq"`
}

// Position is the delivery's place in its workspace's list.
func (d Delivery) Position() DeliveryPosition {
	return DeliveryPosition{ReceivedAt: d.ReceivedAt, Seq: d.Seq}
}

// DeliveryQuery is what a list of a workspace's deliveries asks for.
type DeliveryQuery struct {
	// Since, unless zero, leaves out the deliveries received before it.
	Since time.Time
	// After, unless nil, starts the list after that position.
	After *DeliveryPosition
	// Limit is the most deliveries the list holds; 0 for no limit.
	Limit int
}

// deliverySchema holds the deliveries: per workspace and forge delivery id,
// which the unique key makes one record, what the hub made of the payload
// and when it received it. There is no column for the payload or the
// secret. seq is the order of recording; the indexes serve a workspace's
// list, newest first, and the pruning by age.
var deliverySchema = []string{
	`CREATE TABLE forge_deliveries (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace TEXT NOT NULL,
	delivery_id TEXT NOT NULL,
	forge TEXT NOT NULL,
	event TEXT NOT NULL,
	action TEXT NOT NULL,
	target TEXT,
	outcome TEXT NOT NULL,
	relayed TEXT NOT NULL,
	received_at TEXT NOT NULL,
	UNIQUE (workspace, delivery_id)
)`,
	`CREATE INDEX forge_deliveries_by_workspace ON forge_deliveries (workspace, received_at, seq)`,
	`CREATE INDEX forge_deliveries_by_age ON forge_deliveries (received_at)`,
}

// DeliverySchemaStep adds the webhook deliveries.
var DeliverySchemaStep = store.Step{
	Name:   "forge_deliveries",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range deliverySchema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	},
}

// RecordDelivery reduces a verified delivery of the workspace slug, of the
// forge kind, with the forge's delivery id, its event name and raw body,
// and records it with the events it relays, in one store transaction:
// either both are stored or neither is. A delivery id the workspace has
// already recorded is answered OutcomeDuplicate, with nothing written;
// concurrent copies of one delivery are told apart by the table's unique
// key under the store's one writer at a time. The step runs detached from
// ctx's cancellation, so that a forge that closes its connection early
// does not undo a delivery it will not send again. An error holds neither
// the payload nor a secret.
func (c *Component) RecordDelivery(ctx context.Context, slug string, kind ForgeKind, deliveryID, eventName string, body []byte) (Outcome, error) {
	ctx = context.WithoutCancel(ctx)
	ws, ok, err := c.deps.Workspaces(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("read workspace %s: %w", slug, err)
	}
	if !ok {
		return "", fmt.Errorf("workspace %s does not exist", slug)
	}
	red := Reduce(kind, eventName, body, reposOf(ws))
	relayed := make([]string, 0, len(red.Events))
	for _, ev := range red.Events {
		relayed = append(relayed, ev.Type)
	}
	relayedJSON, err := json.Marshal(relayed)
	if err != nil {
		return "", err
	}
	var target *string
	if red.Target != nil {
		target = new(string(*red.Target))
	}
	receivedAt := timefmt.Format(c.deps.Now())

	outcome := red.Outcome
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO forge_deliveries
			(workspace, delivery_id, forge, event, action, target, outcome, relayed, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (workspace, delivery_id) DO NOTHING`,
			slug, deliveryID, string(kind), cutUTF8(eventName, maxEventBytes), red.Action, target,
			string(red.Outcome), string(relayedJSON), receivedAt)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			outcome = OutcomeDuplicate
			return nil
		}
		for _, ev := range red.Events {
			if _, err := c.deps.Log.Emit(ctx, tx, ev.input(slug, kind, *red.Target, deliveryID)); err != nil {
				return fmt.Errorf("record event %s: %w", ev.Type, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("record delivery %q of %s: %w", deliveryID, slug, err)
	}
	return outcome, nil
}

// reposOf are the repositories ws receives deliveries for: its origin's
// and, in carry-patch mode, its upstream's.
func reposOf(ws workspace.Workspace) Repos {
	name := func(remote *string) string {
		if remote == nil {
			return ""
		}
		repo, ok := ParseRemote(*remote)
		if !ok {
			return ""
		}
		return repo.Owner + "/" + repo.Name
	}
	r := Repos{Origin: name(ws.GitURL)}
	if ws.Mode == workspace.ModeCarryPatch {
		r.Upstream = name(ws.UpstreamURL)
	}
	return r
}

// deliveryColumns are the columns scanDelivery reads, in its order.
const deliveryColumns = `seq, delivery_id, forge, event, action, target, outcome, relayed, received_at`

// scanDelivery reads one row of deliveryColumns.
func scanDelivery(row interface{ Scan(...any) error }) (Delivery, error) {
	var (
		d                                Delivery
		kind, outcome, relayed, received string
		target                           sql.NullString
	)
	if err := row.Scan(&d.Seq, &d.DeliveryID, &kind, &d.Event, &d.Action, &target, &outcome, &relayed, &received); err != nil {
		return Delivery{}, err
	}
	d.Forge, d.Outcome = ForgeKind(kind), Outcome(outcome)
	if target.Valid {
		d.Target = new(Target(target.String))
	}
	if err := json.Unmarshal([]byte(relayed), &d.Relayed); err != nil || d.Relayed == nil {
		return Delivery{}, fmt.Errorf("delivery %q has an unreadable relayed list %q", d.DeliveryID, relayed)
	}
	at, err := time.Parse(time.RFC3339, received)
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery %q has an unreadable time %q", d.DeliveryID, received)
	}
	d.ReceivedAt = at.UTC()
	return d, nil
}

// GetDelivery reads the delivery deliveryID of the workspace slug, and
// whether it is recorded.
func GetDelivery(ctx context.Context, db *sql.DB, slug, deliveryID string) (Delivery, bool, error) {
	d, err := scanDelivery(db.QueryRowContext(ctx,
		`SELECT `+deliveryColumns+` FROM forge_deliveries WHERE workspace = ? AND delivery_id = ?`, slug, deliveryID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Delivery{}, false, nil
	case err != nil:
		return Delivery{}, false, fmt.Errorf("read delivery %q of %s: %w", deliveryID, slug, err)
	}
	return d, true, nil
}

// ListDeliveries reads the deliveries of the workspace slug that q asks
// for, newest first and, within a second, last recorded first.
func ListDeliveries(ctx context.Context, db *sql.DB, slug string, q DeliveryQuery) ([]Delivery, error) {
	query, args := `SELECT `+deliveryColumns+` FROM forge_deliveries WHERE workspace = ?`, []any{slug}
	if !q.Since.IsZero() {
		query, args = query+` AND received_at >= ?`, append(args, timefmt.Format(ceilSecond(q.Since)))
	}
	if q.After != nil {
		at := timefmt.Format(q.After.ReceivedAt)
		query, args = query+` AND (received_at < ? OR (received_at = ? AND seq < ?))`, append(args, at, at, q.After.Seq)
	}
	query += ` ORDER BY received_at DESC, seq DESC`
	if q.Limit > 0 {
		query, args = query+` LIMIT ?`, append(args, q.Limit)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the deliveries of %s: %w", slug, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("list the deliveries of %s: %w", slug, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the deliveries of %s: %w", slug, err)
	}
	return out, nil
}

// LastDeliveryAt is when the newest delivery of the workspace slug was
// received, and whether one is recorded.
func LastDeliveryAt(ctx context.Context, db *sql.DB, slug string) (time.Time, bool, error) {
	var newest sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM forge_deliveries WHERE workspace = ?`, slug).Scan(&newest); err != nil {
		return time.Time{}, false, fmt.Errorf("read the newest delivery of %s: %w", slug, err)
	}
	if !newest.Valid {
		return time.Time{}, false, nil
	}
	at, err := time.Parse(time.RFC3339, newest.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("the newest delivery of %s has an unreadable time %q", slug, newest.String)
	}
	return at.UTC(), true, nil
}

// PruneDeliveries deletes, in every workspace, the deliveries received
// before the instant before. A pruned delivery id the forge sends again is
// a new delivery.
func PruneDeliveries(ctx context.Context, st *store.Store, before time.Time) error {
	return st.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM forge_deliveries WHERE received_at < ?`, timefmt.Format(ceilSecond(before)))
		return err
	})
}

// ceilSecond is t rounded up to a whole second. A delivery is received in
// whole seconds, so it is before t exactly when it is before ceilSecond(t),
// and the comparison can be made on the stored text.
func ceilSecond(t time.Time) time.Time {
	if whole := t.Truncate(time.Second); !whole.Equal(t) {
		return whole.Add(time.Second)
	}
	return t
}

// RemoveDeliveries deletes the deliveries of the workspace slug inside tx,
// the transaction that deletes the workspace; it is a workspace remover
// (docs/adr/07-register-the-teardown-collaborators-on-the-workspace-service.md).
func RemoveDeliveries(ctx context.Context, tx *sql.Tx, slug string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM forge_deliveries WHERE workspace = ?`, slug)
	return err
}
