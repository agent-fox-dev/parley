package awareness

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/event"
)

// EventOverlap is the type of the event that tells a change another change
// started to touch its files.
const EventOverlap = "parley.change.overlap"

// Partner is a change that overlaps another one, with the files the two
// have in common in ascending byte order.
type Partner struct {
	Number int64
	Paths  []string
}

// Partners are the changes that overlap the change number of workspace,
// read through q (the caller's transaction, or nil for the store): the
// other in-flight changes of the workspace with the same target branch and
// at least one indexed file in common, outside the change's stack, the
// most files in common first and, among equals, the lower number first.
// A change the index does not hold in flight has none.
//
// The index can lag the change records, so a change counts only while its
// record says draft or open: a change whose record says otherwise has no
// partners and is no one's, also before the index has followed it out of
// flight.
//
// Two changes are in one stack when following depends_on from each reaches
// the same bottom change. The stack is read from the change records at
// each call rather than kept in the index, so a depends_on edit needs no
// index update.
func (c *Component) Partners(ctx context.Context, q Querier, workspace string, number int64) ([]Partner, error) {
	found, err := c.partners(ctx, q, workspace, number)
	if err != nil {
		return nil, err
	}
	out := make([]Partner, 0, len(found))
	for _, p := range found {
		out = append(out, p.Partner)
	}
	return out, nil
}

// partner is a Partner with its change record, as Partners read it.
type partner struct {
	Partner
	record Change
}

// partners are the Partners of the change number of workspace, each with
// its change record, read through q.
func (c *Component) partners(ctx context.Context, q Querier, workspace string, number int64) ([]partner, error) {
	q = c.querier(q)
	rows, err := q.QueryContext(ctx, `SELECT o.number, o.path
FROM awareness_changes mc
JOIN awareness_files mf ON mf.workspace_slug = mc.workspace_slug AND mf.number = mc.number
JOIN awareness_files o ON o.workspace_slug = mf.workspace_slug AND o.path = mf.path AND o.number <> mf.number
JOIN awareness_changes oc ON oc.workspace_slug = o.workspace_slug AND oc.number = o.number
WHERE mc.workspace_slug = ? AND mc.number = ? AND mc.phase = 'in_flight'
	AND oc.phase = 'in_flight' AND oc.target_branch = mc.target_branch
ORDER BY o.number, o.path`, workspace, number)
	if err != nil {
		return nil, fmt.Errorf("read the overlaps of %s#%d: %w", workspace, number, err)
	}
	var out []Partner
	for rows.Next() {
		var n int64
		var path string
		if err := rows.Scan(&n, &path); err != nil {
			rows.Close()
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Number != n {
			out = append(out, Partner{Number: n})
		}
		last := &out[len(out)-1]
		last.Paths = append(last.Paths, path)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kept := []partner{}
	if len(out) == 0 {
		return kept, nil
	}

	w := stackWalk{workspace: workspace, bottoms: map[int64]int64{}, records: map[int64]Change{}}
	own, err := c.bottom(ctx, q, &w, number)
	if err != nil {
		return nil, err
	}
	if !inFlight(w.records[number].State) {
		return kept, nil
	}
	for _, p := range out {
		b, err := c.bottom(ctx, q, &w, p.Number)
		if err != nil {
			return nil, err
		}
		rec, found := w.records[p.Number]
		if !found || !inFlight(rec.State) || b == own {
			continue
		}
		rec.Workspace, rec.Number = workspace, p.Number
		kept = append(kept, partner{Partner: p, record: rec})
	}
	slices.SortStableFunc(kept, func(a, b partner) int {
		return cmp.Or(cmp.Compare(len(b.Paths), len(a.Paths)), cmp.Compare(a.Number, b.Number))
	})
	return kept, nil
}

// stackWalk is what the stack walks of one Partners call share: the bottom
// of each change of workspace found so far, and the record of each change
// read on the way; a change the reader does not find has no record.
type stackWalk struct {
	workspace string
	bottoms   map[int64]int64
	records   map[int64]Change
}

// bottom is the bottom change of the stack of the change number of w's
// workspace: the change reached by following depends_on from it until a
// change depends on none, or on one the reader does not find. It adds the
// bottoms and the records it finds to w. A depends_on that leads back to a
// change already passed ends at that change, so that a cycle cannot hold
// the worker.
func (c *Component) bottom(ctx context.Context, q Querier, w *stackWalk, number int64) (int64, error) {
	var path []int64
	visited := map[int64]bool{}
	n := number
	for {
		if b, ok := w.bottoms[n]; ok {
			n = b
			break
		}
		if visited[n] {
			break
		}
		visited[n] = true
		path = append(path, n)
		ch, err := c.deps.Changes.ReadChange(ctx, q, w.workspace, n)
		if errors.Is(err, ErrChangeNotFound) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read the stack of %s#%d: %w", w.workspace, number, err)
		}
		w.records[n] = ch
		if ch.DependsOn == nil {
			break
		}
		n = *ch.DependsOn
	}
	for _, p := range path {
		w.bottoms[p] = n
	}
	return n, nil
}

// announce brings the announced pairs that include the change ch up to
// date with its partners at the end of an index update, in the update's
// transaction tx: a partner without a recorded pair gets one, and the two
// changes a parley.change.overlap event each naming the other; a recorded
// pair whose other change is no longer a partner, because the files in
// common are gone, the target branches differ, the two are in one stack or
// either left flight, in the index or in its record, is forgotten, so that the next time they overlap is
// announced again.
func (c *Component) announce(ctx context.Context, tx *sql.Tx, ch Change) error {
	partners, err := c.partners(ctx, tx, ch.Workspace, ch.Number)
	if err != nil {
		return err
	}
	recorded, err := pairedWith(ctx, tx, ch.Workspace, ch.Number)
	if err != nil {
		return err
	}
	current := map[int64]bool{}
	for _, p := range partners {
		current[p.Number] = true
		if recorded[p.Number] {
			continue
		}
		other := p.record
		if err := c.emitOverlap(ctx, tx, ch, other, p.Paths); err != nil {
			return err
		}
		if err := c.emitOverlap(ctx, tx, other, ch, p.Paths); err != nil {
			return err
		}
		low, high := min(ch.Number, p.Number), max(ch.Number, p.Number)
		if _, err := tx.ExecContext(ctx, `INSERT INTO awareness_pairs (workspace_slug, low, high) VALUES (?, ?, ?)`,
			ch.Workspace, low, high); err != nil {
			return fmt.Errorf("record the pair %d, %d of %s: %w", low, high, ch.Workspace, err)
		}
	}
	for n := range recorded {
		if current[n] {
			continue
		}
		low, high := min(ch.Number, n), max(ch.Number, n)
		if _, err := tx.ExecContext(ctx, `DELETE FROM awareness_pairs WHERE workspace_slug = ? AND low = ? AND high = ?`,
			ch.Workspace, low, high); err != nil {
			return fmt.Errorf("forget the pair %d, %d of %s: %w", low, high, ch.Workspace, err)
		}
	}
	return nil
}

// pairedWith are the changes of workspace recorded as announced together
// with the change number, read through tx.
func pairedWith(ctx context.Context, tx *sql.Tx, workspace string, number int64) (map[int64]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN low = ? THEN high ELSE low END FROM awareness_pairs
WHERE workspace_slug = ? AND (low = ? OR high = ?)`, number, workspace, number, number)
	if err != nil {
		return nil, fmt.Errorf("read the pairs of %s#%d: %w", workspace, number, err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// emitOverlap writes, in tx, the parley.change.overlap event on the change
// on that tells it the change with started to touch the files paths
// (ascending) too. The workspace is the changes' target workspace and the
// session the one of the change the event is written on; the paths are
// cut as every path list of an event is.
func (c *Component) emitOverlap(ctx context.Context, tx *sql.Tx, on, with Change, paths []string) error {
	meta := map[string]any{
		"number":          on.Number,
		"with":            map[string]any{"number": with.Number, "ref": refOf(with)},
		"with_session_id": with.SessionID,
		"count":           len(paths),
	}
	event.PathList(meta, "paths", paths)
	_, err := c.deps.Emitter.Emit(ctx, tx, event.Input{
		Type: EventOverlap, Workspace: &on.Workspace, ActorType: event.ActorSystem,
		ResourceType: "change", ResourceID: strconv.FormatInt(on.Number, 10), SessionID: on.SessionID,
		Metadata: meta,
	})
	if err != nil {
		return fmt.Errorf("announce the overlap of %s#%d with #%d: %w", on.Workspace, on.Number, with.Number, err)
	}
	return nil
}
