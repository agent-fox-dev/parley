package access_test

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"
)

func TestSweepBindings_RevokesOnlyTheLiveTokensBoundToAWorkspaceWithoutARecord(t *testing.T) {
	b := newBindings(t)
	before := b.revokedAt()
	if n, err := b.svc.SweepBindings(context.Background()); err != nil || n != 2 {
		t.Fatalf("SweepBindings = %d, %v, want 2 and no error", n, err)
	}
	b.checkRevoked(t, before, "gone live", "gone expiring")
	after := b.revokedAt()
	b.clock.advance(time.Minute)
	if n, err := b.svc.SweepBindings(context.Background()); err != nil || n != 0 {
		t.Errorf("a second SweepBindings = %d, %v, want 0 and no error", n, err)
	}
	if again := b.revokedAt(); !maps.Equal(again, after) {
		t.Errorf("revoked_at after the second sweep = %v, want %v", again, after)
	}
	var line map[string]any
	if err := json.Unmarshal(b.log.Bytes(), &line); err != nil || line["level"] != "INFO" || line["count"] != 2.0 {
		t.Errorf("log = %s, want one INFO line with count 2", b.log.String())
	}
}

func TestSweepBindings_RollsBackAndReturnsTheFailureWhenTheSecondUpdateFails(t *testing.T) {
	b := newBindings(t)
	// Each update of a PAT counts itself in a table of the test; the second
	// one is refused, after the first was written in the same transaction.
	if _, err := b.st.Main().Exec(`CREATE TABLE updates (n INTEGER);
CREATE TRIGGER fail_second_update BEFORE UPDATE ON pats BEGIN
  INSERT INTO updates VALUES (1);
  SELECT RAISE(ABORT, 'the second update fails') WHERE (SELECT count(*) FROM updates) >= 2;
END`); err != nil {
		t.Fatalf("create the failing trigger: %v", err)
	}
	before := b.revokedAt()
	n, err := b.svc.SweepBindings(context.Background())
	if err == nil || n != 0 || !strings.Contains(err.Error(), "the second update fails") {
		t.Errorf("SweepBindings on a failing store = %d, %v, want 0 and the store's error", n, err)
	}
	if after := b.revokedAt(); !maps.Equal(after, before) || b.log.Len() != 0 {
		t.Errorf("revoked_at after the failed sweep = %v, want %v; log %q, want nothing", after, before, b.log.String())
	}
}
