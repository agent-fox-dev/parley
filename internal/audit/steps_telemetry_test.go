package audit_test

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/audit"
)

// telemetryTables are the tables of the six run telemetry kinds.
var telemetryTables = []string{"run_events", "session_outcomes", "tool_calls", "tool_errors", "traces", "postmortems"}

// remaining are the ids left in each table of the run telemetry.
func (s *stores) remaining(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range telemetryTables {
		out[table] = ids(t, s.st.Analytics(), table)
	}
	return out
}

func TestTelemetrySteps_DeleteOnlyTheirOwnRecordsOlderThanTheLimit(t *testing.T) {
	t.Parallel()
	// A seeded record: its table, its age (the age of its timestamp; a
	// postmortem is ingested ingested ago) and whether the step keeps it.
	type seed struct {
		table, id     string
		age, ingested time.Duration
		kept          bool
	}
	runRecords := []seed{
		{table: "traces", id: "trace-91d", age: 91 * day, kept: true},
		{table: "postmortems", id: "postmortem-91d", age: 91 * day, ingested: 91 * day, kept: true},
	}
	for _, table := range []string{"run_events", "session_outcomes", "tool_calls", "tool_errors"} {
		runRecords = append(runRecords,
			seed{table: table, id: table + "-91d", age: 91 * day},
			seed{table: table, id: table + "-exactly-90d", age: 90 * day, kept: true},
			seed{table: table, id: table + "-89d", age: 89 * day, kept: true})
	}
	for _, tc := range []struct {
		step  string
		seeds []seed
	}{
		{"run_records", runRecords},
		{"traces", []seed{
			{table: "traces", id: "trace-31d", age: 31 * day},
			{table: "traces", id: "trace-exactly-30d", age: 30 * day, kept: true},
			{table: "run_events", id: "run-event-31d", age: 31 * day, kept: true},
		}},
		{"postmortems", []seed{
			{table: "postmortems", id: "ingested-181d", age: 181 * day, ingested: 181 * day},
			{table: "postmortems", id: "ingested-exactly-180d", age: 180 * day, ingested: 180 * day, kept: true},
			{table: "postmortems", id: "ingested-10d-stamped-400d", age: 400 * day, ingested: 10 * day, kept: true},
		}},
	} {
		t.Run(tc.step, func(t *testing.T) {
			t.Parallel()
			s := openStores(t)
			want := map[string][]string{}
			for _, table := range telemetryTables {
				want[table] = nil
			}
			for _, sd := range tc.seeds {
				at := retentionNow.Add(-sd.age)
				if sd.table == "postmortems" {
					// The run is the record's id: a run has one postmortem.
					s.postmortem(t, sd.id, "ws1", sd.id, at, retentionNow.Add(-sd.ingested))
				} else {
					s.record(t, sd.table, sd.id, "ws1", "20260101_000000_000001", at)
				}
				if sd.kept {
					want[sd.table] = append(want[sd.table], sd.id)
				}
			}
			for _, kept := range want {
				slices.Sort(kept)
			}
			if err := audit.TelemetrySteps(s.telemetry, defaults, s.clock)[tc.step](context.Background()); err != nil {
				t.Fatalf("%s = %v", tc.step, err)
			}
			if got := s.remaining(t); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
				t.Errorf("records left = %v, want %v", got, want)
			}
		})
	}
}

func TestMaxRuns_KeepsTheRecordsOfTheNewestRunsOfEachWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStores(t)
	settings := defaults
	settings.MaxRuns = 2

	// Each run starts with one record of every kind and has a postmortem,
	// unless it is a run with only a postmortem.
	runs := []struct {
		workspace, run string
		earliest       time.Duration
		onlyPostmortem bool
		trimmed        bool
	}{
		{"ws1", "r1", 10 * day, false, true},
		{"ws1", "r2", 9 * day, false, true},
		{"ws1", "r3", 8 * day, false, false},
		{"ws1", "r4", 7 * day, false, false},
		{"ws2", "q1", 300 * day, false, false},
		{"ws2", "q2", 200 * day, false, false},
		{"ws3", "p1", 50 * day, false, false},
		{"ws3", "p2", 40 * day, false, false},
		{"ws3", "p3", 1 * day, true, false},
		// Two runs whose earliest records are as old: the lower run id is
		// the older.
		{"ws4", "t1", 5 * day, false, true},
		{"ws4", "t2", 5 * day, false, false},
		{"ws4", "t3", 4 * day, false, false},
	}
	want := map[string][]string{}
	for _, table := range telemetryTables {
		want[table] = nil
	}
	keep := func(table, id string, trimmed bool) {
		if !trimmed || table == "postmortems" {
			want[table] = append(want[table], id)
		}
	}
	for _, r := range runs {
		at := retentionNow.Add(-r.earliest)
		pm := r.workspace + "-" + r.run + "-postmortem"
		s.postmortem(t, pm, r.workspace, r.run, at, at)
		keep("postmortems", pm, r.trimmed)
		if r.onlyPostmortem {
			continue
		}
		for _, k := range runKinds {
			id := r.workspace + "-" + r.run + "-" + k.table
			s.record(t, k.table, id, r.workspace, r.run, at)
			keep(k.table, id, r.trimmed)
		}
	}
	// A late record of the oldest run, newer than the start of the newest,
	// does not make the run young.
	s.record(t, "run_events", "ws1-r1-late", "ws1", "r1", retentionNow.Add(-day))
	keep("run_events", "ws1-r1-late", true)
	for _, kept := range want {
		slices.Sort(kept)
	}
	s.session(t, "session-1", "ws1", "completed", retentionNow.Add(-400*day))
	s.session(t, "session-2", "ws1", "active", retentionNow.Add(-day))
	sessions := dump(t, s.st.Main(), `SELECT * FROM sessions ORDER BY id`)

	step := audit.TelemetrySteps(s.telemetry, settings, s.clock)["max_runs"]
	if err := step(ctx); err != nil {
		t.Fatalf("max_runs = %v", err)
	}
	if got := s.remaining(t); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("records left = %v, want %v", got, want)
	}
	if got := dump(t, s.st.Main(), `SELECT * FROM sessions ORDER BY id`); !slices.Equal(got, sessions) {
		t.Errorf("sessions = %v, were %v", got, sessions)
	}

	after := s.snapshot(t)
	if err := step(ctx); err != nil {
		t.Fatalf("second max_runs = %v", err)
	}
	if again := s.snapshot(t); !maps.EqualFunc(again, after, slices.Equal[[]string]) {
		t.Errorf("a second run changed the records: %v, was %v", again, after)
	}
}
