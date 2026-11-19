package event

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// countingQuerier reads through db and counts the rows its queries return,
// and keeps every step of their plans that walks the whole of hub_events.
type countingQuerier struct {
	Querier
	rows  int
	scans []string
}

func (q *countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var n int
	if err := q.Querier.QueryRowContext(ctx, `SELECT count(*) FROM (`+query+`)`, args...).Scan(&n); err != nil {
		return nil, err
	}
	q.rows += n
	plan, err := q.Querier.QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		return nil, err
	}
	defer plan.Close()
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			return nil, err
		}
		if strings.HasPrefix(detail, "SCAN hub_events") {
			q.scans = append(q.scans, detail)
		}
	}
	if err := plan.Err(); err != nil {
		return nil, err
	}
	return q.Querier.QueryContext(ctx, query, args...)
}

func TestReadNewest_ReadsOnlyTheEventsACallerWhoSeesFewMaySee(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.events.SetCoverage(coverage{"api": {"api"}, "api-f1": {"api-f1"}})
	w.events.SetChangeSources(changeSources{"api": {42: "api-f1"}})
	change := func(slug, number string) Input {
		in := on("parley.change.version", slug)
		in.ResourceID = number
		return in
	}
	deletedFork := on("parley.fork.expire", "api-f2")
	deletedFork.Parent = "api"
	// The few events of every kind the rule decides, and many on web, which
	// only dev and the admin token see.
	few := []Input{
		on("parley.change.open", "api"), on("parley.token.bind", "api"), change("api", "43"),
		on("parley.change.open", "api-f1"), on("parley.token.bind", "api-f1"), deletedFork,
		on("parley.change.open", "gone"),
		scoped("parley.secret.create", "user", w.users["dev"]), scoped("parley.secret.create", "user", w.users["mem"]),
		scoped("parley.variable.update", "org", w.acme), scoped("parley.user.create", "", ""),
	}
	w.emitTx(few...)
	many := make([]Input, 3*newestBatch)
	for i := range many {
		many[i] = on("parley.change.open", "web")
	}
	w.emitTx(many...)
	w.emitTx(few...)
	all := w.readAll(0)
	slices.SortFunc(all, func(a, b Record) int {
		return cmp.Or(b.Time.Compare(a.Time), strings.Compare(b.ID, a.ID))
	})

	const limit = 51
	for _, tc := range []struct {
		name   string
		caller Caller
		// seesFew marks a caller who sees none of the events on web.
		seesFew bool
	}{
		{"admin token", w.caller("admin"), false},
		{"owner of api and web", w.user("dev"), false},
		{"member of api, owner of its fork", w.user("mem"), true},
		{"outsider", w.user("out"), true},
		{"token bound to api", Caller{UserID: w.users["dev"], CredentialID: "tok-dev", Workspace: "api"}, true},
		{"token bound to the fork", Caller{UserID: w.users["mem"], CredentialID: "tok-mem", Workspace: "api-f1"}, true},
	} {
		var want []string
		for _, rec := range all {
			if ok, err := w.events.Visible(context.Background(), tc.caller, rec); err != nil {
				t.Fatalf("%s: Visible: %v", tc.name, err)
			} else if ok && len(want) < limit {
				want = append(want, rec.ID)
			}
		}
		db := &countingQuerier{Querier: w.store.Main()}
		recs, err := w.events.readVisible(context.Background(), db, tc.caller, NewestQuery{Limit: limit})
		if err != nil {
			t.Fatalf("%s: ReadNewest: %v", tc.name, err)
		}
		var got []string
		for _, rec := range recs {
			got = append(got, rec.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: ReadNewest = %d events %v, want the %d it may see %v", tc.name, len(got), got, len(want), want)
		}
		if !tc.seesFew {
			continue
		}
		if most := 2 * len(few); db.rows > most {
			t.Errorf("%s: ReadNewest read %d stored events for a page of %d, want at most the %d of its workspaces and scopes",
				tc.name, db.rows, len(got), most)
		}
		if len(db.scans) > 0 {
			t.Errorf("%s: ReadNewest walks the whole table: %v", tc.name, db.scans)
		}
	}
}
