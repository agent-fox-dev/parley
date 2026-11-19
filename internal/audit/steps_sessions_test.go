package audit_test

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/audit"
)

func TestStaleSessions_TimesOutOnlyTheSessionsActiveLongerThanTheMaximumActiveAge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStores(t)
	sessions := []struct {
		id, status string
		age        time.Duration
		timedOut   bool
	}{
		{"active-8d", "active", 8 * day, true},
		{"active-exactly-7d", "active", 7 * day, false},
		{"active-6d", "active", 6 * day, false},
		{"completed-30d", "completed", 30 * day, false},
	}
	before := map[string][]string{}
	for _, ss := range sessions {
		s.session(t, ss.id, "ws1", ss.status, retentionNow.Add(-ss.age))
		before[ss.id] = dump(t, s.st.Main(), `SELECT * FROM sessions WHERE id = ?`, ss.id)
	}
	step := audit.SessionSteps(s.sessions, defaults, s.clock)["stale_sessions"]
	if err := step(ctx); err != nil {
		t.Fatalf("stale_sessions = %v", err)
	}

	for _, ss := range sessions {
		if !ss.timedOut {
			if got := dump(t, s.st.Main(), `SELECT * FROM sessions WHERE id = ?`, ss.id); !slices.Equal(got, before[ss.id]) {
				t.Errorf("%s changed: %v, was %v", ss.id, got, before[ss.id])
			}
			continue
		}
		var status string
		var ended, message sql.NullString
		var duration sql.NullInt64
		err := s.st.Main().QueryRowContext(ctx, `SELECT status, ended_at, error_message, duration_ms FROM sessions WHERE id = ?`,
			ss.id).Scan(&status, &ended, &message, &duration)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(ss.age / time.Millisecond)
		if status != "timeout" || ended.String != "2026-06-01T12:00:00Z" ||
			message.String != "session exceeded maximum active age" || !duration.Valid || duration.Int64 != want {
			t.Errorf("%s = %s, ended %v, %v, %v ms; want timeout, ended 2026-06-01T12:00:00Z, "+
				"\"session exceeded maximum active age\", %d ms", ss.id, status, ended, message, duration, want)
		}
	}
	if b, err := s.events.Bounds(ctx); err != nil || b.LastSeq != 0 {
		t.Errorf("event log bounds = %+v, %v; want no event written", b, err)
	}

	after := s.snapshot(t)
	if err := step(ctx); err != nil {
		t.Fatalf("second stale_sessions = %v", err)
	}
	if again := s.snapshot(t); !maps.EqualFunc(again, after, slices.Equal[[]string]) {
		t.Errorf("a second run changed the sessions: %v, was %v", again, after)
	}
}

func TestSessionsStep_DeletesTheEndedSessionsOlderThanTheMaximumAgeWithTheirUsage(t *testing.T) {
	t.Parallel()
	s := openStores(t)
	sessions := []struct {
		id, status string
		age        time.Duration
		kept       bool
	}{
		{"completed-91d", "completed", 91 * day, false},
		{"failed-91d", "failed", 91 * day, false},
		{"timeout-91d", "timeout", 91 * day, false},
		{"terminated-91d", "terminated", 91 * day, false},
		{"completed-exactly-90d", "completed", 90 * day, true},
		{"completed-10d", "completed", 10 * day, true},
		{"active-400d", "active", 400 * day, true},
	}
	var keptSessions, keptUsage []string
	for _, ss := range sessions {
		started := retentionNow.Add(-ss.age)
		s.session(t, ss.id, "ws1", ss.status, started)
		usage := []string{ss.id + "-usage-1", ss.id + "-usage-2"}
		for _, u := range usage {
			s.usage(t, u, ss.id, "ws1", started)
		}
		if ss.kept {
			keptSessions, keptUsage = append(keptSessions, ss.id), append(keptUsage, usage...)
		}
	}
	if err := audit.SessionSteps(s.sessions, defaults, s.clock)["sessions"](context.Background()); err != nil {
		t.Fatalf("sessions = %v", err)
	}

	slices.Sort(keptSessions)
	slices.Sort(keptUsage)
	if got := ids(t, s.st.Main(), "sessions"); !slices.Equal(got, keptSessions) {
		t.Errorf("sessions left = %v, want %v", got, keptSessions)
	}
	if got := ids(t, s.st.Main(), "session_usage"); !slices.Equal(got, keptUsage) {
		t.Errorf("usage records left = %v, want %v", got, keptUsage)
	}
}
