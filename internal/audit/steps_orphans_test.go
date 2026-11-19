package audit_test

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/audit"
)

func TestOrphans_DeletesTheOldRecordsOfWorkspacesThatAreGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStores(t)
	s.workspace(t, "ws1", "")

	// Each row seeds one record of every kind for the slug, all as old as
	// age: a session with a usage record, one record of each run telemetry
	// kind, a postmortem and a hub event.
	rows := []struct {
		name, slug string
		age        time.Duration
		status     string
		kept       bool
	}{
		{"existing-60d", "ws1", 60 * day, "completed", true},
		{"gone-31d", "gone", 31 * day, "completed", false},
		{"gone-active-31d", "gone", 31 * day, "active", false},
		{"gone-exactly-30d", "gone", 30 * day, "completed", true},
		{"gone-5d", "gone", 5 * day, "completed", true},
		{"deleted-fork-of-existing-60d", "ws1--f1", 60 * day, "completed", true},
		{"deleted-fork-of-gone-60d", "gone--f2", 60 * day, "completed", false},
	}
	want := map[string][]string{"hub_events": nil}
	for _, table := range append([]string{"sessions", "session_usage"}, telemetryTables...) {
		want[table] = nil
	}
	keep := func(table, id string, kept bool) {
		if kept {
			want[table] = append(want[table], id)
		}
	}
	for _, r := range rows {
		at := retentionNow.Add(-r.age)
		s.session(t, r.name+"-session", r.slug, r.status, at)
		keep("sessions", r.name+"-session", r.kept)
		s.usage(t, r.name+"-usage", r.name+"-session", r.slug, at)
		keep("session_usage", r.name+"-usage", r.kept)
		for _, k := range runKinds {
			s.record(t, k.table, r.name+"-"+k.table, r.slug, "20260101_000000_000001", at)
			keep(k.table, r.name+"-"+k.table, r.kept)
		}
		s.postmortem(t, r.name+"-postmortem", r.slug, r.name, at, at)
		keep("postmortems", r.name+"-postmortem", r.kept)
		keep("hub_events", s.hubEvent(t, r.slug, at).ID, r.kept)
	}
	// An event of no workspace is no orphan, and the newest event stays
	// whatever its workspace.
	keep("hub_events", s.hubEvent(t, "", retentionNow.Add(-60*day)).ID, true)
	keep("hub_events", s.hubEvent(t, "gone", retentionNow.Add(-60*day)).ID, true)
	for _, kept := range want {
		slices.Sort(kept)
	}
	// The events leave the event log before orphans runs, as the event_log
	// step does in a retention run: every one of them but the newest.
	if _, err := s.events.Expire(ctx, retentionNow); err != nil {
		t.Fatalf("expire = %v", err)
	}

	steps := audit.OrphanSteps(access.StoreRecords(s.st.Main()), []audit.OrphanStore{s.sessions, s.telemetry, s.events},
		defaults, s.clock)
	if err := steps["orphans"](ctx); err != nil {
		t.Fatalf("orphans = %v", err)
	}
	got := s.remaining(t)
	for _, table := range []string{"sessions", "session_usage"} {
		got[table] = ids(t, s.st.Main(), table)
	}
	got["hub_events"] = ids(t, s.st.Main(), "hub_events")
	if !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("records left = %v\nwant %v", got, want)
	}
}
