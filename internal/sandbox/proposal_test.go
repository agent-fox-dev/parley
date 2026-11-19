package sandbox_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestPurgeHooks_RemoveTheNetworkPolicyRowsOfTheRemovedSandbox(t *testing.T) {
	t.Parallel()
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
	all := [4]int{1, 1, 1, 1}

	purge(s, true)
	if got := h.policyRows(t, s); got != [4]int{} {
		t.Errorf("rows of the removed sandbox (proposals, denials, approved rules, log position) = %v; want none", got)
	}
	if got := h.policyRows(t, d); got != all {
		t.Errorf("rows of the other sandbox = %v; want %v", got, all)
	}
	purge(d, false)
	if got := h.policyRows(t, d); got != all {
		t.Errorf("rows after a rolled back removal = %v; want %v", got, all)
	}
}

func TestDeleteHooks_WithdrawTheOpenProposalsOfTheDeletedSandbox(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	ctx := context.Background()
	// seed stores a stopped sandbox with the proposals p1 (open), p2
	// (approved) and p3 (open), and deletes it in a transaction that runs
	// the delete hooks and commits when commit is true.
	seed := func(t *testing.T, commit bool) (id string, p1, p2, p3 sandbox.Proposal) {
		t.Helper()
		id = h.seedRecord(t, "demo", sandbox.StateStopped, nil)
		p1 = h.seedProposal(t, id, sandbox.ProposalOpen, recordStart.Add(-3*time.Second), nil)
		p2 = h.seedProposal(t, id, sandbox.ProposalApproved, recordStart.Add(-2*time.Second), func(p *sandbox.Proposal) {
			p.Scope, p.Comment = new(sandbox.ScopeSandbox), new("fine")
			p.DecidedBy = &sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}
			p.DecidedAt = new(recordStart.Add(-time.Second))
		})
		p3 = h.seedProposal(t, id, sandbox.ProposalOpen, recordStart.Add(-time.Second), nil)
		err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
			if _, _, err := h.svc.TransitionTx(ctx, tx, id, sandbox.StateStopped, sandbox.StateDeleted, "", nil); err != nil {
				return err
			}
			if err := h.svc.RunDeleteHooks(ctx, tx, id); err != nil {
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
		return id, p1, p2, p3
	}

	t.Run("committed", func(t *testing.T) {
		t.Parallel()
		id, p1, p2, p3 := seed(t, true)
		withdrawn := func(p sandbox.Proposal) sandbox.Proposal { p.State = sandbox.ProposalWithdrawn; return p }
		for _, tc := range []struct {
			state string
			want  []sandbox.Proposal
		}{
			{sandbox.ProposalWithdrawn, []sandbox.Proposal{withdrawn(p1), withdrawn(p3)}},
			{sandbox.ProposalOpen, []sandbox.Proposal{}},
			{sandbox.ProposalApproved, []sandbox.Proposal{p2}},
		} {
			items, _ := listItems(t, h.call(t, http.MethodGet, proposalsPath("demo", id)+"?state="+tc.state, h.key, nil))
			wantItems(t, items, tc.want)
		}
		for _, d := range decisionPaths {
			a := h.call(t, http.MethodPost, d.path("demo", id, p1.ID), h.key, nil)
			if a.status != http.StatusConflict || a.err.ErrorType != "proposal_decided" {
				t.Errorf("%s of a withdrawn proposal = %d %s; want 409 proposal_decided", d.name, a.status, a.body)
			}
		}
		if e := h.sandboxEvents(t, sandbox.EventProposalDecided, id); len(e) != 0 {
			t.Errorf("decision events %v; want none", e)
		}
	})
	t.Run("rolled back", func(t *testing.T) {
		t.Parallel()
		id, p1, _, p3 := seed(t, false)
		items, _ := listItems(t, h.call(t, http.MethodGet, proposalsPath("demo", id), h.key, nil))
		wantItems(t, items, []sandbox.Proposal{p1, p3})
	})
}
