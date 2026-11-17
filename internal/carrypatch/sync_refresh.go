package carrypatch

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// BranchMoveNotifier tells the observers of a sync's branch moves of one
// move; the workspace service is the hub's.
type BranchMoveNotifier interface {
	NotifyBranchMove(ctx context.Context, m workspace.BranchMove)
}

// EventPatchSync is the type of the event that reports what a sync's
// origin refresh did with one patch branch.
const EventPatchSync = "parley.patch.sync"

// The states of a patch branch against origin after a sync's refresh,
// besides originInSync.
const (
	// originDiverged is a branch whose tip and origin's each hold a commit
	// the other lacks, or whose tip is ahead of origin's, left as it is
	// because the workspace's divergence policy is report.
	originDiverged = "diverged"
	// originMissing is a branch origin does not have.
	originMissing = "missing_on_origin"
)

// patchDivergencePolicyKey is the workspace setting that tells whether a
// sync's refresh replaces a patch branch that diverged from origin or
// reports it.
const patchDivergencePolicyKey = "patch_divergence_policy"

// divergenceReplaces reports whether the effective patch_divergence_policy
// of w is replace; a missing key or registry reads as the setting's
// default, replace.
func (c *Component) divergenceReplaces(w workspace.Workspace) bool {
	if c.d.Settings == nil {
		return true
	}
	policy, _ := c.d.Settings.EffectiveSettings(w.Settings, w.DefaultBranch)[patchDivergencePolicyKey].(string)
	return policy != "report"
}

// refreshable reports whether a patch of status takes part in a sync's
// origin refresh: a merged or deleted patch's branch is no longer the
// patch list's to follow origin with.
func refreshable(status string) bool {
	return status == StatusActive || status == StatusConflict || status == StatusDisabled
}

// refreshStep is the outcome a sync's origin refresh chose for one patch
// from the two tips it compared: the hub's (local) and origin's as the
// sync's fetch found it, each "" when that side has no such branch.
type refreshStep struct {
	p             Patch
	action, state string
	local, origin string
	// at is when the tips were compared, cut to the second as the store
	// keeps it.
	at time.Time
}

// moves reports whether the step creates or moves the patch's branch.
func (st refreshStep) moves() bool { return st.action != resetNone }

// entry is the step as the answer's patches_synced reports it: the
// branch's tip after the step, origin's tip, and the tip a replace saved.
func (st refreshStep) entry() syncEntry {
	e := syncEntry{BranchName: st.p.BranchName, Action: st.action, State: st.state}
	after := st.local
	if st.moves() {
		after = st.origin
	}
	if after != "" {
		e.LocalSHA = new(after)
	}
	if st.origin != "" {
		e.OriginSHA = new(st.origin)
	}
	if st.action == resetReplaced {
		e.ReplacedSHA = new(st.local)
	}
	return e
}

// refreshFromOrigin brings the branch of every active, conflict or
// disabled patch, in position order, in line with origin when the
// workspace's patch_branch_source is origin, and reports each outcome in
// patches_synced. Under the hub as the authority it reads and moves
// nothing. Origin's tip is the one this sync fetched, not what the copy's
// origin heads hold now, which a concurrent sync may have moved since. A
// patch branch is moved only by a compare-and-swap against the tip
// compared, whether or not protected_branches lists it: the refresh is a
// sync from origin, which a protected branch admits, so no push rule is
// asked. The first failure stops the refresh with an error naming the
// branch; what was done before it stays.
func (s *syncPass) refreshFromOrigin(ctx context.Context) error {
	w := s.run.Workspace
	if !s.c.fromOrigin(w) {
		return nil
	}
	replaces := s.c.divergenceReplaces(w)
	live, err := ListLivePatches(ctx, s.c.d.Store.Main(), w.Slug)
	if err != nil {
		return fmt.Errorf("list the patches to refresh from origin: %w", err)
	}
	for _, p := range live {
		if !refreshable(p.Status) {
			continue
		}
		st, err := s.compareWithOrigin(ctx, p, replaces)
		if err != nil {
			return err
		}
		recorded, err := s.applyRefresh(ctx, st)
		if err != nil {
			return err
		}
		if !recorded {
			continue
		}
		s.report.PatchesSynced = append(s.report.PatchesSynced, st.entry())
		if st.moves() {
			s.moved = append(s.moved, p)
			s.notifyMove(ctx, st)
		}
	}
	return nil
}

// compareWithOrigin chooses the refresh outcome of the patch p by the first
// rule that fits its branch's two tips: origin has none, the hub has none,
// the tips are equal, the hub's is an ancestor of origin's; any other pair,
// the hub's tip ahead of origin's among them, is replaced or reported as
// diverged as replaces says.
func (s *syncPass) compareWithOrigin(ctx context.Context, p Patch, replaces bool) (refreshStep, error) {
	st := refreshStep{p: p, origin: s.run.OriginHeads[p.BranchName]}
	var local gitrepo.Ref
	var exists, behind bool
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		var err error
		if local, exists, err = s.repo.LookupRef(ctx, headsPrefix+p.BranchName); err != nil || !exists || st.origin == "" || local.ID == st.origin {
			return err
		}
		behind, err = s.repo.IsAncestor(ctx, local.ID, st.origin)
		return err
	})
	if err != nil {
		return refreshStep{}, fmt.Errorf("compare the patch branch %q with origin: %w", p.BranchName, err)
	}
	st.at = s.c.now().UTC().Truncate(time.Second)
	if exists {
		st.local = local.ID
	}
	switch {
	case st.origin == "":
		st.action, st.state = resetNone, originMissing
	case !exists:
		st.action, st.state = resetCreated, originInSync
	case st.local == st.origin:
		st.action, st.state = resetNone, originInSync
	case behind:
		st.action, st.state = resetFastForwarded, originInSync
	case replaces:
		st.action, st.state = resetReplaced, originInSync
	default:
		st.action, st.state = resetNone, originDiverged
	}
	return st, nil
}

// applyRefresh records the refresh outcome st of one patch in one store
// transaction: the patch's origin fields, the events that report the
// outcome and, last, the branch's move, so that none of them exists
// without the others. It reports whether the outcome was recorded: a
// patch deleted, merged or otherwise no longer refreshable since it was
// listed is left out. A swap that loses to another writer
// rolls the transaction back and leaves the branch as that writer has it,
// while the backup a replace wrote stays; a commit that fails after the
// swap puts the branch back. The git work held in the transaction is
// local and short, two update-refs at most, which is why it may hold the
// store's write turn.
func (s *syncPass) applyRefresh(ctx context.Context, st refreshStep) (bool, error) {
	slug, branch := s.run.Workspace.Slug, st.p.BranchName
	if st.moves() && s.c.d.BeforeRefSwap != nil {
		// Outside the transaction below: a test writes the store through
		// the hook, and a Store.Tx inside another would wait for its own
		// turn forever.
		s.c.d.BeforeRefSwap(slug, branch)
	}
	var recorded, moved bool
	err := s.c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		recorded, moved = false, false
		p, found, err := ReadPatch(ctx, tx, slug, st.p.ID)
		if err != nil {
			return err
		}
		if !found || !refreshable(p.Status) {
			return nil
		}
		if err := s.recordOutcome(ctx, tx, p, st); err != nil {
			return err
		}
		if st.moves() {
			if err := s.moveToOrigin(ctx, st); err != nil {
				return err
			}
			moved = true
		}
		recorded = true
		return nil
	})
	if err != nil {
		if moved {
			// The commit failed after the branch moved: nothing records
			// the move, so it is taken back.
			s.c.undoMove(ctx, s.repo, slug, branch, st.local, st.origin)
		}
		return false, fmt.Errorf("refresh the patch branch %q from origin: %w", branch, err)
	}
	return recorded, nil
}

// recordOutcome stores the outcome st on the patch p, as read in tx
// before the outcome is recorded, and emits in tx the events that report
// it: parley.patch.sync when the branch is created or moved or the state
// differs from the one stored, a first refresh's included, and
// parley.patch.replace for a replaced branch.
func (s *syncPass) recordOutcome(ctx context.Context, tx *sql.Tx, p Patch, st refreshStep) error {
	slug := s.run.Workspace.Slug
	var originSHA any // null when origin lacks the branch
	if st.origin != "" {
		originSHA = st.origin
	}
	if _, err := tx.ExecContext(ctx, `UPDATE patches SET origin_sync_state = ?, origin_sha = ?, origin_synced_at = ?
WHERE workspace_slug = ? AND id = ?`, st.state, originSHA, st.at.UnixNano(), slug, p.ID); err != nil {
		return fmt.Errorf("record the origin state: %w", err)
	}
	var events []event.Input
	if st.moves() || p.OriginSyncState == nil || *p.OriginSyncState != st.state {
		e := st.entry()
		events = append(events, event.Input{
			Type: EventPatchSync, Workspace: &slug, ActorType: s.by.typ, ActorID: s.by.id,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": e.BranchName, "action": e.Action, "state": e.State,
				"local_sha": e.LocalSHA, "origin_sha": e.OriginSHA},
		})
	}
	if st.action == resetReplaced {
		events = append(events, event.Input{
			Type: EventPatchReplace, Workspace: &slug, ActorType: s.by.typ, ActorID: s.by.id,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": p.BranchName, "replaced_sha": st.local, "origin_sha": st.origin,
				"trigger": "sync"},
		})
	}
	for _, in := range events {
		if _, err := s.c.d.Events.Emit(ctx, tx, in); err != nil {
			return fmt.Errorf("emit %s: %w", in.Type, err)
		}
	}
	return nil
}

// clearOriginFields sets the three origin fields to null, without an
// event, on the patches whose branch does not follow origin: every patch of
// the workspace under the hub as the authority, and every merged_upstream
// patch under origin. Their branch is no longer compared with origin, so a
// stored comparison would describe a state the sync no longer keeps.
func (s *syncPass) clearOriginFields(ctx context.Context) error {
	query := `UPDATE patches SET origin_sync_state = NULL, origin_sha = NULL, origin_synced_at = NULL
WHERE workspace_slug = ? AND (origin_sync_state IS NOT NULL OR origin_sha IS NOT NULL OR origin_synced_at IS NOT NULL)`
	args := []any{s.run.Workspace.Slug}
	if s.c.fromOrigin(s.run.Workspace) {
		query += ` AND status = ?`
		args = append(args, StatusMergedUpstream)
	}
	return s.c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("clear the origin fields of the patches that do not follow origin: %w", err)
		}
		return nil
	})
}

// moveToOrigin moves the patch's branch from the hub's tip compared, or
// creates it when the hub had none, to origin's tip, saving the hub's tip
// to the backup ref first for a replace, so that a replaced tip stays
// reachable even when the swap then loses.
func (s *syncPass) moveToOrigin(ctx context.Context, st refreshStep) error {
	if st.action == resetReplaced {
		if err := WriteBackupRef(ctx, s.repo, st.p.BranchName, st.local); err != nil {
			return err
		}
	}
	return RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return s.repo.CompareAndSwapRef(ctx, headsPrefix+st.p.BranchName, st.origin, st.local)
	})
}

// notifyMove tells the sync's branch-move observers of the move st
// recorded, after its transaction committed, since an observer writes the
// store itself.
func (s *syncPass) notifyMove(ctx context.Context, st refreshStep) {
	if s.c.d.BranchMoves == nil {
		return
	}
	s.c.d.BranchMoves.NotifyBranchMove(ctx, workspace.BranchMove{
		Workspace: s.run.Workspace.Slug, Branch: st.p.BranchName,
		Before: st.local, After: st.origin, Cause: workspace.MoveCauseSync,
	})
}
