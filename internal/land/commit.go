package land

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/store"
)

// EventLand is the event of a change that landed.
const EventLand = "parley.change.land"

// errTargetMoved is commit's error when the target was no longer at the
// head the commits were built on: nothing moved and nothing was recorded.
var errTargetMoved = errors.New("the target moved since it was read")

// The points of the atomic step a test may stop the engine at.
const (
	faultBeforeJournal    = "before-journal"
	faultBeforeSwap       = "before-swap"
	faultAfterSwap        = "after-swap"
	faultInBookkeeping    = "in-bookkeeping"
	faultAfterBookkeeping = "after-bookkeeping"
)

// commit lands what b built for the attempt a of the target workspace ws:
// it moves the target from b.onto to b.head with a compare-and-swap, then
// records the land in one store transaction (settle), then has the push
// event of the move written. A land with nothing left moves nothing and
// writes no push event; it is recorded all the same, on the head it was
// built on.
//
// A git ref and a store row cannot share a transaction, so the move is
// entered in the land journal first, in a transaction of its own; the
// transaction that records the land removes the entry. A hub that stops
// between the two leaves the entry, which Reconcile completes or drops at
// the next start, so that the land is recorded whole or not at all. A
// target that is no longer at b.onto moves nothing: the entry is removed
// and the error is errTargetMoved.
//
// The target moves through the ref writer with a verdict built here that
// allows the one update, without asking the push rules, since a land is
// not a push and a protected target moves as any other
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
// moved reports whether the target moved; from then on the record and
// the push event are written whatever becomes of ctx, and an error leaves
// the attempt running.
func (c *Component) commit(ctx context.Context, repo *gitrepo.Repo, ws Workspace, a Attempt, b built) (moved bool, err error) {
	a.Status, a.OntoSHA, a.LandedSHA, a.Noop, a.Commits = StatusLanded, new(b.onto), new(b.head), b.noop(), b.commits
	a.FinishedAt = new(c.now())
	if a.Noop {
		return false, c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.settle(ctx, tx, a) })
	}
	ref := "refs/heads/" + a.TargetBranch
	verdict, err := pushrule.AllowedVerdict(ctx, repo, []pushrule.Update{{Ref: ref, Before: b.onto, After: b.head}})
	if err != nil {
		return false, err
	}
	if err := c.fault(faultBeforeJournal); err != nil {
		return false, err
	}
	if err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return journal(ctx, tx, a, ref) }); err != nil {
		return false, err
	}
	if err := c.fault(faultBeforeSwap); err != nil {
		return false, err
	}
	caller := callerOf(a)
	res, err := c.deps.RefWriter.Apply(ctx, pushrule.ApplyRequest{
		Verdict: verdict, Repo: repo, SessionID: a.SessionID, Via: pushrule.ViaLand, VersionKind: pushrule.VersionKindNone,
		Workspace: pushrule.Workspace{Slug: ws.Slug, Kind: ws.Kind, Parent: ws.Parent, DefaultBranch: ws.DefaultBranch, Mode: ws.Mode},
		Caller:    pushrule.Caller{CredentialType: caller.Credential.Type, CredentialID: caller.Credential.ID, UserID: caller.UserID},
	})
	switch res.Outcome {
	case pushrule.OutcomeAccepted:
	case pushrule.OutcomeRefMoved:
		if err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return unjournal(ctx, tx, a.ID) }); err != nil {
			return false, err
		}
		return false, errTargetMoved
	default:
		// The entry goes with the attempt's end.
		if err == nil {
			err = errors.New("the ref writer moved nothing")
		}
		return false, err
	}
	if err := c.fault(faultAfterSwap); err != nil {
		return true, err
	}
	ctx = context.WithoutCancel(ctx)
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := c.settle(ctx, tx, a); err != nil {
			return err
		}
		return c.fault(faultInBookkeeping)
	})
	if err == nil {
		err = c.fault(faultAfterBookkeeping)
	}
	if errors.Is(err, errStopped) {
		return true, err
	}
	c.deps.RefWriter.Finish(ctx, res)
	return true, err
}

// settle records, in tx, the land of the attempt a, final with status
// landed and its result members: each change it covers moves to landed,
// with its own landed commits mapped to it, the attempt is finished and
// each change gets its parley.change.land event, by whoever asked for the
// land, in the session they named.
func (c *Component) settle(ctx context.Context, tx *sql.Tx, a Attempt) error {
	for _, cov := range a.Covered {
		l := change.Landed{SHA: *a.LandedSHA, At: *a.FinishedAt, By: a.RequestedBy, LandID: a.ID}
		for _, cm := range a.Commits {
			if cm.Change == cov.Number {
				l.Commits = append(l.Commits, change.LandedCommit{VersionSHA: cm.VersionSHA, LandedSHA: cm.LandedSHA})
			}
		}
		if err := c.deps.Changes.SetLanded(ctx, tx, a.Workspace, cov.Number, l); err != nil {
			return err
		}
	}
	if err := c.finish(ctx, tx, a); err != nil {
		return err
	}
	for _, cov := range a.Covered {
		if _, err := c.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventLand, Workspace: &a.Workspace, ActorType: a.RequestedBy.ActorType, ActorID: a.RequestedBy.ActorID,
			ResourceType: "change", ResourceID: strconv.FormatInt(cov.Number, 10), SessionID: a.SessionID,
			Metadata: map[string]any{
				"land_id": a.ID, "target_branch": a.TargetBranch, "onto_sha": a.OntoSHA, "landed_sha": a.LandedSHA,
				"strategy": a.Strategy, "attempts": a.Attempts, "noop": a.Noop, "trigger": a.Trigger, "override": a.Override,
				"policy_revision": a.PolicyRevision, "number": cov.Number, "ref": change.Ref(a.Workspace, cov.Number),
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// JournalStep creates the store schema of the land journal: at most one
// entry per attempt, the move of its target it is about to make, with
// what the record of the land needs beyond the stored attempt. An entry
// goes with its attempt.
var JournalStep = store.Step{
	Name:   "land_journal",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE land_journal (
	land_id TEXT PRIMARY KEY REFERENCES land_attempts (id) ON DELETE CASCADE,
	workspace_slug TEXT NOT NULL,
	ref TEXT NOT NULL,
	old_sha TEXT NOT NULL,
	new_sha TEXT NOT NULL,
	outcome TEXT NOT NULL
)`)
		return err
	},
}

// outcome is what a journal entry keeps of a land beyond its ref's old
// and new heads, which the stored attempt does not hold while it runs.
type outcome struct {
	Attempts       int       `json:"attempts"`
	Commits        []Commit  `json:"commits"`
	PolicyRevision *int64    `json:"policy_revision"`
	Override       *Override `json:"override"`
	FinishedAt     int64     `json:"finished_at"`
}

// journal enters, in tx, the move of the target ref of the attempt a,
// whose result members hold the land it is about to record.
func journal(ctx context.Context, tx *sql.Tx, a Attempt, ref string) error {
	raw, err := json.Marshal(outcome{Attempts: a.Attempts, Commits: a.Commits, PolicyRevision: a.PolicyRevision,
		Override: a.Override, FinishedAt: a.FinishedAt.UnixNano()})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO land_journal (land_id, workspace_slug, ref, old_sha, new_sha, outcome)
VALUES (?, ?, ?, ?, ?, ?)`, a.ID, a.Workspace, ref, *a.OntoSHA, *a.LandedSHA, string(raw)); err != nil {
		return fmt.Errorf("journal land attempt %s: %w", a.ID, err)
	}
	return nil
}

// unjournal removes, in tx, the journal entry of the attempt id, if it has
// one.
func unjournal(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM land_journal WHERE land_id = ?`, id); err != nil {
		return fmt.Errorf("remove the journal entry of land attempt %s: %w", id, err)
	}
	return nil
}

// entry is one entry of the land journal.
type entry struct {
	id, workspace, ref, old, new string
	outcome                      outcome
}

// landed is the attempt a as the land of e records it.
func (e entry) landed(a Attempt) Attempt {
	o := e.outcome
	a.Status, a.Attempts, a.OntoSHA, a.LandedSHA, a.Noop = StatusLanded, o.Attempts, new(e.old), new(e.new), false
	a.Commits, a.PolicyRevision, a.Override = o.Commits, o.PolicyRevision, o.Override
	a.FinishedAt = new(time.Unix(0, o.FinishedAt).UTC())
	return a
}

// journalEntries reads every entry of the land journal through q.
func journalEntries(ctx context.Context, q Querier) ([]entry, error) {
	rows, err := q.QueryContext(ctx, `SELECT land_id, workspace_slug, ref, old_sha, new_sha, outcome FROM land_journal
ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("read the land journal: %w", err)
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var e entry
		var raw string
		if err := rows.Scan(&e.id, &e.workspace, &e.ref, &e.old, &e.new, &raw); err != nil {
			return nil, fmt.Errorf("read the land journal: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &e.outcome); err != nil {
			return nil, fmt.Errorf("read the journal entry of land attempt %s: %w", e.id, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Reconcile completes or drops, at the start before the hub serves, the
// lands a stop left in the land journal. An entry whose ref holds the head
// the land moved it to has its land recorded as the engine would have
// recorded it (settle), its events written once; the push event of the
// move is the ref writer's own journal's. Any other entry moved nothing
// and is dropped, its attempt left running. An entry whose repository
// cannot be read, or that cannot be settled, is kept and logged for the
// next start. A second run finds nothing left to do.
func (c *Component) Reconcile(ctx context.Context) error {
	entries, err := journalEntries(ctx, c.deps.Store.Main())
	if err != nil {
		return err
	}
	for _, e := range entries {
		moved, err := c.moved(ctx, e)
		if err == nil {
			err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
				if !moved {
					return unjournal(ctx, tx, e.id)
				}
				a, found, err := readAttempt(ctx, tx, e.workspace, e.id)
				switch {
				case err != nil:
					return err
				case !found || a.Status != StatusRunning:
					// Recorded already: nothing is written twice.
					return unjournal(ctx, tx, e.id)
				}
				return c.settle(ctx, tx, e.landed(a))
			})
		}
		if err != nil {
			c.deps.Logger.Error("land journal: entry kept, it cannot be completed", "workspace", e.workspace,
				"land_id", e.id, "error", err)
		}
	}
	return nil
}

// moved reports whether the ref of e holds the head the land moved it to.
func (c *Component) moved(ctx context.Context, e entry) (bool, error) {
	repo, err := c.deps.Repos(ctx, e.workspace)
	if err != nil {
		return false, err
	}
	values, err := repo.RefValues(ctx, []string{e.ref})
	if err != nil {
		return false, err
	}
	return values[e.ref] == e.new, nil
}
