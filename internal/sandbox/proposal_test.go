package sandbox_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestPurgeHooks_RemoveTheProposalsDenialsAndApprovedRulesOfTheRemovedSandbox(t *testing.T) {
	h := startCollectorHub(t)
	deleted := func(r *sandbox.Record) { r.State = sandbox.StateDeleted }
	s, d := h.sandbox(t, deleted), h.sandbox(t, deleted)
	h.seedPolicyRows(t, s)
	h.seedPolicyRows(t, d)
	ctx := context.Background()
	purge := func(id string, commit bool) {
		t.Helper()
		err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := h.svc.RunPurgeHooks(ctx, tx, id); err != nil {
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
	all := [3]int{1, 1, 1}

	purge(s, true)
	if got := h.policyRows(t, s); got != [3]int{} {
		t.Errorf("rows of the removed sandbox (proposals, denials, approved rules) = %v; want none", got)
	}
	if got := h.policyRows(t, d); got != all {
		t.Errorf("rows of the other sandbox = %v; want %v", got, all)
	}
	purge(d, false)
	if got := h.policyRows(t, d); got != all {
		t.Errorf("rows after a rolled back removal = %v; want %v", got, all)
	}
}
