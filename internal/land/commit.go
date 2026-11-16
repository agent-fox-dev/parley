package land

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/store"
)

// EventLand is the event of a change that landed.
const EventLand = "parley.change.land"

// EventLandFail is the event of a land attempt that ended without landing.
const EventLandFail = "parley.change.land_fail"

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

// settleTries is how often the record of a land whose target moved is
// tried, settleBackoff the wait after the first failed try, which grows
// with each.
const (
	settleTries   = 3
	settleBackoff = 100 * time.Millisecond
)

// stillOpen refuses, in tx, the land of the attempt a when a change it
// covers is no longer open, as one abandoned or set back to draft while
// the attempt ran: a land that moved nothing ends then without moving the
// target. It is asked in the transaction that enters the move in the
// journal, the last one before the move. A change abandoned between that
// transaction and the record of the land is landed all the same: the
// target holds it by then, and the land is the one record that agrees
// with the target.
func (c *Component) stillOpen(ctx context.Context, tx *sql.Tx, a Attempt) error {
	for _, cov := range a.Covered {
		ch, err := c.deps.Changes.ReadChange(ctx, tx, a.Workspace, cov.Number)
		if err != nil {
			return err
		}
		if ch.State != change.StateOpen {
			return fmt.Errorf("%s is %s since the land attempt %s was created: only an open change lands", ch.Ref, ch.State, a.ID)
		}
	}
	return nil
}

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
// the attempt running. Once the land is recorded its follow-up runs and
// the final observers are told of it (concluded).
func (c *Component) commit(ctx context.Context, repo *gitrepo.Repo, ws Workspace, a Attempt, b built) (moved bool, err error) {
	a.Status, a.OntoSHA, a.LandedSHA, a.Noop, a.Commits = StatusLanded, new(b.onto), new(b.head), b.noop(), b.commits
	a.FinishedAt = new(c.now())
	if a.Noop {
		if err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := c.stillOpen(ctx, tx, a); err != nil {
				return err
			}
			return c.settle(ctx, tx, a)
		}); err != nil {
			return false, err
		}
		c.concluded(ctx, ws, a)
		return false, nil
	}
	ref := "refs/heads/" + a.TargetBranch
	verdict, err := pushrule.AllowedVerdict(ctx, repo, []pushrule.Update{{Ref: ref, Before: b.onto, After: b.head}})
	if err != nil {
		return false, err
	}
	if err := c.fault(faultBeforeJournal); err != nil {
		return false, err
	}
	if err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := c.stillOpen(ctx, tx, a); err != nil {
			return err
		}
		return journal(ctx, tx, a, ref)
	}); err != nil {
		return false, err
	}
	if err := c.fault(faultBeforeSwap); err != nil {
		return false, err
	}
	if err := c.enterSwap(ctx, a.ID); err != nil {
		// Past its deadline, or at the stop of the hub, the attempt moves
		// nothing, also when its run went on after it was ended.
		return false, errors.Join(err, c.deps.Store.Tx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
			return unjournal(ctx, tx, a.ID)
		}))
	}
	caller := callerOf(a)
	res, err := c.deps.RefWriter.Apply(ctx, pushrule.ApplyRequest{
		Verdict: verdict, Repo: repo, SessionID: a.SessionID, Via: pushrule.ViaLand, VersionKind: pushrule.VersionKindNone,
		Workspace: pushrule.Workspace{Slug: ws.Slug, Kind: ws.Kind, Parent: ws.Parent, DefaultBranch: ws.DefaultBranch, Mode: ws.Mode},
		Caller:    pushrule.Caller{CredentialType: caller.Credential.Type, CredentialID: caller.Credential.ID, UserID: caller.UserID},
	})
	if res.Outcome != pushrule.OutcomeAccepted {
		c.leaveSwap(a.ID)
	}
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
	// The target moved: recording the land is all that is left, so a
	// transaction that fails is tried again before the entry is left to
	// the next start. Only a running attempt is settled, so a try that
	// committed although it failed is not recorded twice.
	for try := 1; ; try++ {
		err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := c.settle(ctx, tx, a); err != nil {
				return err
			}
			return c.fault(faultInBookkeeping)
		})
		if err == nil || errors.Is(err, errStopped) || errors.Is(err, errFinal) || try == settleTries {
			break
		}
		c.deps.Logger.Warn("land attempt: recording the land failed, trying again", "workspace", a.Workspace,
			"land_id", a.ID, "try", try, "error", err)
		time.Sleep(time.Duration(try) * settleBackoff)
	}
	if err == nil {
		err = c.fault(faultAfterBookkeeping)
	}
	if errors.Is(err, errStopped) {
		return true, err
	}
	c.deps.RefWriter.Finish(ctx, res)
	if err == nil {
		c.concluded(ctx, ws, a)
	}
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
		if err := c.emit(ctx, tx, a, EventLand, cov.Number, map[string]any{
			"land_id": a.ID, "target_branch": a.TargetBranch, "onto_sha": a.OntoSHA, "landed_sha": a.LandedSHA,
			"strategy": a.Strategy, "attempts": a.Attempts, "noop": a.Noop, "trigger": a.Trigger, "override": a.Override,
			"policy_revision": a.PolicyRevision,
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
// the land moved it to, or a commit after it, has its land recorded as the engine would have
// recorded it (settle), its events written once; the push event of the
// move is the ref writer's own journal's. Any other entry moved nothing
// and is dropped, its attempt left running. An entry whose repository
// cannot be read, or that cannot be settled, is kept and logged for the
// next start, and its attempt stays running, not run again (Recover). A
// second run finds nothing left to do.
//
// A land it records has the follow-up a land recorded by its run has, and
// its final observers are told of it (concluded), since nothing did that
// before the stop: its source branch is deleted, the changes into its
// target are checked again and the after-landing hooks are called, with
// the effects those have, such as the restack of the landed changes'
// dependants. A stop after the record and before or during the follow-up
// leaves no entry, so that follow-up is not done again.
func (c *Component) Reconcile(ctx context.Context) error {
	entries, err := journalEntries(ctx, c.deps.Store.Main())
	if err != nil {
		return err
	}
	for _, e := range entries {
		var settled *Attempt
		moved, err := c.moved(ctx, e)
		if err == nil {
			err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
				settled = nil
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
				a = e.landed(a)
				settled = &a
				return c.settle(ctx, tx, a)
			})
		}
		if err != nil {
			c.deps.Logger.Error("land journal: entry kept, it cannot be completed", "workspace", e.workspace,
				"land_id", e.id, "error", err)
			continue
		}
		if settled != nil {
			ws, _, err := c.deps.Workspaces.Workspace(ctx, c.deps.Store.Main(), e.workspace)
			if err != nil {
				c.deps.Logger.Error("land journal: the target of a recorded land cannot be read", "workspace", e.workspace,
					"land_id", e.id, "error", err)
			}
			c.concluded(ctx, ws, *settled)
		}
	}
	return nil
}

// moved reports whether the ref of e holds the head the land moved it to,
// itself or a commit after it: other lands, or pushes, may have moved the
// target on since the swap when the record of the land failed.
func (c *Component) moved(ctx context.Context, e entry) (bool, error) {
	repo, err := c.deps.Repos(ctx, e.workspace)
	if err != nil {
		return false, err
	}
	values, err := repo.RefValues(ctx, []string{e.ref})
	if err != nil {
		return false, err
	}
	at, found := values[e.ref]
	switch {
	case !found:
		return false, nil
	case at == e.new:
		return true, nil
	}
	return repo.IsAncestor(ctx, e.new, at)
}
