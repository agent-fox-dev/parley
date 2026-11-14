package access_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// sweepFixture is a guard hub's store with the ready workspace w1, the
// archived workspace w2 and alice's tokens, by name: two live tokens bound to
// gone, which has no workspace record, one that never expires and one that
// expires later, a token bound to gone revoked an hour ago, one bound to gone
// that expires at the instant of the sweep, a live token bound to w1, one
// bound to w2 and an unbound one. Its service runs on a clock that stands
// still and logs to log.
type sweepFixture struct {
	h     *guardHub
	svc   *access.Service
	clock *testClock
	log   *bytes.Buffer
	ids   map[string]string
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	h := newGuardHub(t)
	alice, org := h.user("alice")
	seed(t, h.st, repo("w1", alice, org, access.StatusReady), repo("w2", alice, org, access.StatusArchived))
	clock := newTestClock()
	now := clock.now()
	later := now.Add(time.Hour)
	f := &sweepFixture{h: h, svc: &access.Service{}, clock: clock, log: &bytes.Buffer{}, ids: map[string]string{}}
	for name, spec := range map[string]identity.PATSpec{
		"gone live":     {Workspace: "gone"},
		"gone expiring": {Workspace: "gone", ExpiresAt: &later},
		"gone revoked":  {Workspace: "gone"},
		"gone expired":  {Workspace: "gone", ExpiresAt: &now},
		"w1 live":       {Workspace: "w1"},
		"w2 live":       {Workspace: "w2"},
		"unbound":       {},
	} {
		spec.UserID, spec.Permissions = alice, []string{"git:read"}
		_, f.ids[name] = h.storePAT(spec)
	}
	err := h.st.Tx(context.Background(), func(tx *sql.Tx) error {
		return identity.RevokeTokenByID(context.Background(), tx, f.ids["gone revoked"], now.Add(-time.Hour))
	})
	if err != nil {
		t.Fatalf("revoke a token: %v", err)
	}
	f.svc.Bind(access.Deps{Store: h.st, Events: h.events, Now: clock.now, Logger: slog.New(slog.NewJSONHandler(f.log, nil))})
	return f
}

// logLines returns the lines the service logged, decoded.
func (f *sweepFixture) logLines() []map[string]any {
	f.h.t.Helper()
	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(f.log.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			f.h.t.Fatalf("log line %q: %v", line, err)
		}
		lines = append(lines, rec)
	}
	return lines
}

func TestSweepBindings_RevokesOnlyTheLiveTokensBoundToAWorkspaceWithoutARecord(t *testing.T) {
	f := newSweepFixture(t)
	before := f.h.revokedAt()
	if !before[f.ids["gone revoked"]].Valid || before[f.ids["gone expired"]].Valid {
		t.Fatalf("fixture: the revoked token must be revoked and the expired one not: %v", before)
	}

	n, err := f.svc.SweepBindings(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("SweepBindings = %d, %v, want 2 and no error", n, err)
	}
	after := f.h.revokedAt()
	at := f.clock.now().UnixNano()
	for name, id := range f.ids {
		switch name {
		case "gone live", "gone expiring":
			if !after[id].Valid || after[id].Int64 != at {
				t.Errorf("%s: revoked_at = %v, want the instant of the sweep, %d", name, after[id], at)
			}
		default:
			if after[id] != before[id] {
				t.Errorf("%s: revoked_at = %v, want it unchanged, %v", name, after[id], before[id])
			}
		}
	}

	f.clock.advance(time.Minute)
	if n, err := f.svc.SweepBindings(context.Background()); err != nil || n != 0 {
		t.Errorf("a second SweepBindings = %d, %v, want 0 and no error", n, err)
	}
	if again := f.h.revokedAt(); !maps.Equal(again, after) {
		t.Errorf("revoked_at after the second sweep = %v, want %v", again, after)
	}
	lines := f.logLines()
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["count"] != 2.0 {
		t.Errorf("log = %v, want one INFO line with count 2", lines)
	}
	if got := f.h.events.all(); len(got) != 0 {
		t.Errorf("events = %+v, want none", got)
	}
}

func TestSweepBindings_RollsBackAndReturnsTheFailureWhenTheSecondUpdateFails(t *testing.T) {
	f := newSweepFixture(t)
	// Each update of a PAT counts itself in a table of the test; the second
	// one is refused, after the first was written in the same transaction.
	if _, err := f.h.st.Main().Exec(`CREATE TABLE updates (n INTEGER);
CREATE TRIGGER fail_second_update BEFORE UPDATE ON pats BEGIN
  INSERT INTO updates VALUES (1);
  SELECT RAISE(ABORT, 'the second update fails') WHERE (SELECT count(*) FROM updates) >= 2;
END`); err != nil {
		t.Fatalf("create the failing trigger: %v", err)
	}
	before := f.h.revokedAt()

	n, err := f.svc.SweepBindings(context.Background())
	if err == nil || n != 0 || !strings.Contains(err.Error(), "the second update fails") {
		t.Errorf("SweepBindings on a failing store = %d, %v, want 0 and the store's error", n, err)
	}
	if after := f.h.revokedAt(); !maps.Equal(after, before) {
		t.Errorf("revoked_at after the failed sweep = %v, want %v", after, before)
	}
	if lines := f.logLines(); len(lines) != 0 {
		t.Errorf("log = %v, want nothing", lines)
	}
}
