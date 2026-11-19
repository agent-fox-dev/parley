package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestRemoveWorkspaceRules_RemovesTheRulesOfTheWorkspaceInTheGivenTransaction(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	ctx := context.Background()
	for _, ws := range []string{"demo", "demo", "other"} {
		r := sandbox.SandboxRule{ID: sandbox.NewRuleID(), Workspace: ws, Rule: json.RawMessage(`{"id":"` + sandbox.NewRuleID() + `"}`), CreatedAt: recordStart}
		if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return sandbox.SaveRule(ctx, tx, r) }); err != nil {
			t.Fatal(err)
		}
	}
	count := func(ws string) int {
		t.Helper()
		n, err := sandbox.CountRules(ctx, h.Store.Main(), ws)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	remove := func(commit bool) {
		t.Helper()
		err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := sandbox.RemoveWorkspaceRules(ctx, tx, "demo"); err != nil {
				return err
			}
			if !commit {
				return errRollback
			}
			return nil
		})
		if err != nil && !errors.Is(err, errRollback) {
			t.Fatal(err)
		}
	}

	remove(false)
	if n := count("demo"); n != 2 {
		t.Errorf("rules of demo after a rolled back removal = %d; want 2", n)
	}
	remove(true)
	if n := count("demo"); n != 0 {
		t.Errorf("rules of demo = %d; want 0", n)
	}
	if n := count("other"); n != 1 {
		t.Errorf("rules of other = %d; want 1", n)
	}
	var events int
	if err := h.Store.Main().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE type = ?`, sandbox.EventRuleRemove).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Errorf("rule_remove events = %d; want 0", events)
	}
}
