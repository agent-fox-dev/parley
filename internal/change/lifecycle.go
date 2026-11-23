package change

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventRestore is the event of an abandoned change brought back to open.
const EventRestore = "parley.change.restore"

// What an abandon does with the draft and open changes that depend on the
// change, as the dependants member of its body names it.
const (
	// DependantsReparent makes each direct dependant depend on what the
	// abandoned change depended on, and has it restacked.
	DependantsReparent = "reparent"
	// DependantsAbandon abandons every change above the abandoned one in
	// its stack, direct or not, for the same reason.
	DependantsAbandon = "abandon"
)

// TriggerAbandon is the trigger of the restack an abandon asks for a
// reparented dependant.
const TriggerAbandon = "abandon"

// Restacker restacks changes, which the stack feature does. Without one an
// abandon that reparents a change's dependants schedules their
// mergeability checks instead.
type Restacker interface {
	// RequestRestack asks for the restack of the change ch, as it stands
	// in tx, for the trigger, such as TriggerAbandon, inside tx, the
	// transaction of what caused it. Its error undoes that transaction.
	RequestRestack(ctx context.Context, tx *sql.Tx, ch Change, trigger string) error
}

// RegisterRestacker makes r the restacker; nil removes it.
func (c *Component) RegisterRestacker(r Restacker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restacker = r
}

// abandonRoute abandons the change :number of the workspace :slug, through
// modify, as its body says. Once its transaction has committed, the
// waiting mergeability checks of the changes it abandoned are dropped; a
// transaction that does not commit leaves them waiting, since its changes
// are then still draft or open.
func (c *Component) abandonRoute(w http.ResponseWriter, r *http.Request) error {
	var abandoned []mergeKey
	if err := c.modify(w, r, func(r *http.Request) (operation, error) { return c.parseAbandon(r, &abandoned) }); err != nil {
		return err
	}
	for _, k := range abandoned {
		c.merge.drop(k)
	}
	return nil
}

// parseAbandon decodes the abandon's body, which may be absent: none, or
// one of white space only, is the same as an empty object. reason must be
// a string, "" when absent, and dependants DependantsReparent or
// DependantsAbandon. The operation adds to abandoned the changes it
// abandoned.
func (c *Component) parseAbandon(r *http.Request, abandoned *[]mergeKey) (operation, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeOptionalJSON(r, &body); err != nil {
		return nil, err
	}
	reason, err := member[string](body, "reason", "a string")
	if err != nil {
		return nil, err
	}
	choice, err := member[string](body, "dependants", fmt.Sprintf("%q or %q", DependantsReparent, DependantsAbandon))
	if err != nil {
		return nil, err
	}
	if choice != nil && *choice != DependantsReparent && *choice != DependantsAbandon {
		return nil, server.InvalidRequest(fmt.Sprintf("dependants: must be %q or %q", DependantsReparent, DependantsAbandon))
	}
	a := abandoning{co: c.collaborators(), abandoned: abandoned}
	if reason != nil {
		a.reason = *reason
	}
	if choice != nil {
		a.choice = *choice
	}
	return func(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) error {
		return c.abandonWith(ctx, tx, ch, caller, a)
	}, nil
}

// abandoning is an abandon as its body asks it.
type abandoning struct {
	reason, choice string
	co             collaborators
	abandoned      *[]mergeKey
}

// abandonWith abandons the change ch as caller, in tx, and settles its
// draft and open dependants as a.choice says: a change that is neither
// draft nor open is 409 change_closed, and one with dependants and no
// choice 409 has_dependants, with nothing written.
func (c *Component) abandonWith(ctx context.Context, tx *sql.Tx, ch Change, caller Caller, a abandoning) error {
	if ch.State != StateDraft && ch.State != StateOpen {
		return changeClosed(ch, "cannot be abandoned")
	}
	if len(ch.Dependants) > 0 && a.choice == "" {
		return server.NewError("has_dependants", http.StatusConflict,
			fmt.Sprintf("%s has dependants %v; name dependants %q or %q", ch.Ref, ch.Dependants, DependantsReparent, DependantsAbandon)).
			With("dependants", ch.Dependants)
	}
	// The stack above is collected first, though abandoning a change
	// leaves its dependants' depends_on as it is.
	var above []int64
	if a.choice == DependantsAbandon {
		var err error
		if above, err = stackAbove(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
			return err
		}
	}
	for _, n := range append([]int64{ch.Number}, above...) {
		if err := c.Abandon(ctx, tx, ch.WorkspaceSlug, n, a.reason, caller); err != nil {
			return err
		}
		*a.abandoned = append(*a.abandoned, mergeKey{ch.WorkspaceSlug, n})
	}
	if a.choice == DependantsReparent {
		return c.reparent(ctx, tx, a.co, ch)
	}
	return nil
}

// stackAbove are the draft and open changes of the workspace above the
// change number in its stack, the direct dependants first.
func stackAbove(ctx context.Context, q Querier, workspace string, number int64) ([]int64, error) {
	var out []int64
	for next := []int64{number}; len(next) > 0; {
		n := next[0]
		next = next[1:]
		deps, err := dependants(ctx, q, workspace, n)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if d != number && !slices.Contains(out, d) {
				out = append(out, d)
				next = append(next, d)
			}
		}
	}
	return out, nil
}

// reparent makes each direct dependant of the abandoned change ch depend
// on what ch depended on, then asks the restacker to restack each one, or,
// without a restacker, schedules its mergeability check, since what it sits
// on changed.
func (c *Component) reparent(ctx context.Context, tx *sql.Tx, co collaborators, ch Change) error {
	for _, d := range ch.Dependants {
		if err := c.SetDependsOn(ctx, tx, ch.WorkspaceSlug, d, ch.DependsOn); err != nil {
			return err
		}
	}
	for _, d := range ch.Dependants {
		if co.restacker == nil {
			if err := c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, d); err != nil {
				return err
			}
			continue
		}
		dep, err := readChange(ctx, tx, ch.WorkspaceSlug, d)
		if err != nil {
			return err
		}
		if err := co.restacker.RequestRestack(ctx, tx, dep, TriggerAbandon); err != nil {
			return fmt.Errorf("restack %s: %w", dep.Ref, err)
		}
	}
	return nil
}

// Abandon moves the change number of the workspace to abandoned for
// reason, as actor did, in tx, and writes parley.change.abandon with the
// reason as given. The move goes through SetAbandoned, so a fork source's
// activity recorder is told; it forgets the change's announcement as
// landable, so that a restore announces it again. Its source branch and its
// version refs stay as they are, and its dependants as they were.
//
// Its waiting mergeability check is left waiting, since tx may yet be
// rolled back: a check that runs stores nothing for a change that is no
// longer draft or open, and a caller that owns the transaction may drop
// the check once it has committed.
func (c *Component) Abandon(ctx context.Context, tx *sql.Tx, workspace string, number int64, reason string, actor Caller) error {
	if err := c.SetAbandoned(ctx, tx, workspace, number, reason); err != nil {
		return err
	}
	if err := c.ForgetLandable(ctx, tx, workspace, number); err != nil {
		return err
	}
	return c.emitAbandon(ctx, tx, actor, workspace, number, reason)
}

// restoreRoute brings the change :number of the workspace :slug back to
// open, through modify. The restore takes no body; one that is sent is
// ignored.
func (c *Component) restoreRoute(w http.ResponseWriter, r *http.Request) error {
	return c.modify(w, r, func(*http.Request) (operation, error) { return c.restore, nil })
}

// restore moves the abandoned change ch back to open at the head of its
// source branch, as caller, in tx. Its refusals come in a fixed order, the
// first that applies answering: a change that is not abandoned (409
// change_not_abandoned), a source workspace that no longer exists (409
// source_workspace_gone), another draft or open change with the same
// source (409 change_exists), a source branch gone together with the last
// version's commit, so that there is nothing to restore it at (409
// conflict), and an empty intent where the workspace requires one (400
// intent_required). Nothing is written before the last refusal.
//
// A source branch that is gone is created at the last version's head, only
// if it is still absent; one created meanwhile is taken as it is. A branch
// the restore creates has the checks of the draft and open changes into it
// scheduled, since it bypasses the ref writer whose stage would. The
// change then becomes open, its landing is evaluated again, its
// mergeability is checked again, parley.change.restore is written for the
// last version and a fork source's activity recorder is told. A branch
// whose head is not the last version's has that head recorded as a new
// version of kind ref by caller, after the change is open, since only a
// draft or open change takes a version.
func (c *Component) restore(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) (err error) {
	if ch.State != StateAbandoned {
		return server.NewError("change_not_abandoned", http.StatusConflict, fmt.Sprintf("%s is %s; only an abandoned change is restored", ch.Ref, ch.State))
	}
	source := ch.Source
	if _, found, err := c.deps.Workspaces.Workspace(ctx, tx, source.WorkspaceSlug); err != nil || !found {
		if err != nil {
			return err
		}
		return server.NewError("source_workspace_gone", http.StatusConflict,
			fmt.Sprintf("%s cannot be restored: its source workspace %s no longer exists", ch.Ref, source.WorkspaceSlug))
	}
	if _, _, found, err := activeOfSource(ctx, tx, source.WorkspaceSlug, source.Branch); err != nil || found {
		if err != nil {
			return err
		}
		return c.changeExists(ctx, tx, source.WorkspaceSlug, source.Branch)
	}
	repo, err := c.deps.Repos(ctx, source.WorkspaceSlug)
	if err != nil {
		return err
	}
	head, found, err := repo.ResolveBranch(ctx, source.Branch)
	if err != nil {
		return err
	}
	if !found {
		missing, err := repo.Missing(ctx, []string{ch.HeadSHA})
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return server.Conflict(fmt.Sprintf("%s cannot be restored: its source branch %s:%s and the head %s of its version %d are gone",
				ch.Ref, source.WorkspaceSlug, source.Branch, ch.HeadSHA, ch.Version))
		}
	}
	if err := c.intentRule(ctx, tx, ch.WorkspaceSlug, ch.Intent); err != nil {
		return err
	}
	var created bool
	if !found {
		if head, created, err = recreateBranch(ctx, repo, source.Branch, ch.HeadSHA); err != nil {
			return err
		}
		if created {
			// A branch this restore created goes again with a restore
			// that does not complete.
			defer func() {
				if err != nil {
					if derr := repo.CompareAndSwapRef(context.WithoutCancel(ctx), "refs/heads/"+source.Branch, zeroID, head); derr != nil {
						c.deps.Logger.Warn("the recreated source branch was not removed", "workspace", source.WorkspaceSlug, "branch", source.Branch, "error", derr)
					}
				}
			}()
		}
	}
	if err := update(ctx, tx, ch.WorkspaceSlug, ch.Number, c.now(), `state = 'open', abandon_reason = NULL`); err != nil {
		if isUniqueViolation(err) {
			return c.changeExists(ctx, tx, source.WorkspaceSlug, source.Branch)
		}
		return err
	}
	if err := c.reevaluateLanding(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	if err := c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	if created {
		if err := c.ScheduleTargetMergeable(ctx, tx, source.WorkspaceSlug, source.Branch); err != nil {
			return err
		}
	}
	if err := c.emit(ctx, tx, caller, ch, EventRestore, map[string]any{"version": ch.Version, "head_sha": ch.HeadSHA}); err != nil {
		return err
	}
	// A change's source is its own workspace or a fork of it.
	if activity := c.collaborators().activity; activity != nil && source.WorkspaceSlug != ch.WorkspaceSlug {
		if err := activity.RecordActivity(ctx, tx, source.WorkspaceSlug); err != nil {
			c.deps.Logger.Warn("the fork's activity was not recorded", "fork", source.WorkspaceSlug, "change", ch.Ref, "error", err)
		}
	}
	if head != ch.HeadSHA {
		if _, err := c.RecordVersion(ctx, tx, VersionInput{Workspace: source.WorkspaceSlug, Branch: source.Branch, Head: head, Kind: KindRef, Caller: caller}); err != nil {
			return err
		}
	}
	return c.AnnounceLandable(ctx, tx, ch.WorkspaceSlug, ch.Number)
}

// zeroID is git's id of no object, which as a ref's new value deletes it.
const zeroID = "0000000000000000000000000000000000000000"

// recreateBranch creates the branch of repo at head, only while it does not
// exist, and returns the head the branch then has and whether this call
// created it: head, or the head of a branch created in the meantime, which
// is taken as it is.
func recreateBranch(ctx context.Context, repo *gitrepo.Repo, branch, head string) (string, bool, error) {
	err := repo.CompareAndSwapRef(ctx, "refs/heads/"+branch, head, "")
	switch {
	case err == nil:
		return head, true, nil
	case errors.Is(err, gitrepo.ErrRefNameConflict):
		return "", false, server.Conflict(fmt.Sprintf("source branch %s cannot be created: another branch is in the way of its name", branch))
	case !errors.Is(err, gitrepo.ErrRefMoved):
		return "", false, err
	}
	now, found, err := repo.ResolveBranch(ctx, branch)
	if err != nil {
		return "", false, err
	}
	if !found {
		// Locked by another writer, and not there yet.
		return "", false, server.Conflict(fmt.Sprintf("source branch %s is being written; try again", branch))
	}
	return now, false, nil
}
