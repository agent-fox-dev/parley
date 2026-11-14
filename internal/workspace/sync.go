package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// syncFetchDeadline is how long the fetch of a sync may take, the deadline
// of a push to origin: a fetch without one could hold its request for ever,
// and the CLI's request timeout of three minutes leaves room for it.
const syncFetchDeadline = 2 * time.Minute

// Fetcher fetches a workspace's origin into its copy, as gitrepo's
// Git.FetchOrigin describes it. gitrepo.Git satisfies it.
type Fetcher interface {
	FetchOrigin(ctx context.Context, dir, url string, cred Credential, deadline time.Duration) (gitrepo.Fetched, error)
}

// SyncOptions are the choices a caller makes for one sync.
type SyncOptions struct {
	// ResetToOrigin moves every diverged branch to origin's head; without
	// it a diverged branch is left as it is.
	ResetToOrigin bool
	// Actor is the caller the sync's event names as its actor.
	Actor identity.Principal
}

// MoveCauseSync is the cause of a branch move a sync made, a move of the
// sync's own or of a sync extension.
const MoveCauseSync = "sync"

// BranchMove is one move of a branch of a workspace's copy, as observers are
// told of it after the ref changed.
type BranchMove struct {
	// Workspace is the workspace's slug and Branch the branch's name,
	// without refs/heads/.
	Workspace, Branch string
	// Before is the branch's head before the move, "" when the move created
	// the branch; After is its head after it.
	Before, After string
	// Cause is what moved the branch, such as MoveCauseSync.
	Cause string
}

// BranchMoveObserver is told of every branch move of the workspaces' copies
// that a sync makes, so that another feature can act on it (an open change
// whose source branch moved records a new version) without this package
// knowing that feature. An error is logged and changes nothing: the branch
// has moved already.
type BranchMoveObserver interface {
	BranchMoved(ctx context.Context, m BranchMove) error
}

// RegisterBranchMoveObserver adds o to the observers every branch move is
// reported to, in the order of registration; nil adds none. The features
// that observe moves register when the hub is composed. Without an observer
// a sync behaves the same.
func (s *Service) RegisterBranchMoveObserver(o BranchMoveObserver) {
	if o == nil {
		return
	}
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	s.moveObservers = append(s.moveObservers, o)
}

// NotifyBranchMove tells every registered observer of m, once each, in the
// order of registration. It is called right after the ref changed, by the
// sync for its own moves and by a sync extension for the moves it makes. An
// observer's error is logged with the workspace and the branch, and the
// remaining observers are told all the same.
func (s *Service) NotifyBranchMove(ctx context.Context, m BranchMove) {
	s.moveMu.RLock()
	observers := slices.Clone(s.moveObservers)
	s.moveMu.RUnlock()
	for _, o := range observers {
		if err := o.BranchMoved(ctx, m); err != nil {
			s.deps.Logger.Error("workspace branch-move observer failed", "slug", m.Workspace, "branch", m.Branch,
				"cause", m.Cause, "error", err)
		}
	}
}

// BranchOutcome is what a sync did with one branch of origin.
type BranchOutcome string

// The outcomes of a branch: created when the hub had no such branch,
// up_to_date when both heads are equal, fast_forward when the hub's head was
// an ancestor of origin's, ahead when origin's head is an ancestor of the
// hub's, and, when each side has commits the other lacks, diverged (left as
// it is) or, with ResetToOrigin, reset (moved to origin's head).
const (
	BranchCreated     BranchOutcome = "created"
	BranchUpToDate    BranchOutcome = "up_to_date"
	BranchFastForward BranchOutcome = "fast_forward"
	BranchAhead       BranchOutcome = "ahead"
	BranchDiverged    BranchOutcome = "diverged"
	BranchReset       BranchOutcome = "reset"
)

// BranchResult is one entry of a sync's branch list, as the sync answer
// marshals it.
type BranchResult struct {
	// Name is the branch's name, without refs/heads/.
	Name   string        `json:"name"`
	Result BranchOutcome `json:"result"`
	// LocalSHA is the hub's head of the branch after the sync handled it.
	LocalSHA string `json:"local_sha"`
	// OriginSHA is origin's head of the branch as the fetch found it.
	OriginSHA string `json:"origin_sha"`
}

// SyncResult is what one sync did.
type SyncResult struct {
	// Workspace is the workspace's record as the sync left it, read again
	// after its completion was recorded.
	Workspace Workspace
	// CreatedTags maps every tag the sync created in the copy to the object
	// id it created it at, origin's value.
	CreatedTags map[string]string
	// Branches has one entry per branch of origin, sorted by name in byte
	// order; it is empty, never nil, when origin has no branch, so that it
	// marshals as [].
	Branches []BranchResult
}

// Sync brings the copy of the workspace slug up to date with its origin: it
// resolves the origin credential from the workspace's secrets at this
// moment, fetches every branch and tag of origin with it within
// syncFetchDeadline, which sets the copy's origin heads to origin's
// branches, and creates in the copy every tag origin has and the copy lacks,
// at origin's value. A tag the copy has keeps its value, whether origin has
// it at another value, has deleted it or never had it. It then evaluates
// every branch of origin in name order (see syncBranch), reports each in
// Branches and tells the branch-move observers of each move right after it.
// A branch only the hub has, never on origin or deleted there, is neither
// looked at nor reported. Last it records the completion (see
// completeSync). The caller has checked that the workspace may be synced;
// opts.Actor must be the admin token or a user's credential.
func (s *Service) Sync(ctx context.Context, slug string, opts SyncOptions) (SyncResult, error) {
	actorType, actorID, ok := eventActor(opts.Actor)
	if !ok {
		return SyncResult{}, fmt.Errorf("sync workspace %q: the sync names no caller", slug)
	}
	w, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil {
		return SyncResult{}, err
	}
	if !found {
		return SyncResult{}, fmt.Errorf("sync workspace %q: %w", slug, ErrNotFound)
	}
	fetched, err := s.fetchOrigin(ctx, w)
	if err != nil {
		return SyncResult{}, err
	}
	repo, err := s.deps.Git.Open(ctx, s.RepoPath(slug))
	if err != nil {
		return SyncResult{}, err
	}
	created, err := createTags(ctx, repo, fetched.Tags)
	if err != nil {
		return SyncResult{}, err
	}
	branches := make([]BranchResult, 0, len(fetched.Branches))
	var updated []refUpdate
	for _, name := range slices.Sorted(maps.Keys(fetched.Branches)) {
		b, before, err := syncBranch(ctx, repo, name, fetched.Branches[name], opts.ResetToOrigin)
		if err != nil {
			return SyncResult{}, err
		}
		branches = append(branches, b)
		if moves(b.Result) {
			s.NotifyBranchMove(ctx, BranchMove{Workspace: slug, Branch: name, Before: before, After: b.LocalSHA, Cause: MoveCauseSync})
			updated = append(updated, newRefUpdate("refs/heads/"+name, before, b.LocalSHA))
		}
	}
	for name, id := range created {
		updated = append(updated, newRefUpdate("refs/tags/"+name, "", id))
	}
	in := syncEventInput(slug, branches, updated)
	in.ActorType, in.ActorID = actorType, actorID
	rec, err := s.completeSync(ctx, repo, w, fetched.Branches, in)
	if err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Workspace: rec, CreatedTags: created, Branches: branches}, nil
}

// completeSync records that the sync of w completed, in one store
// transaction with its event in: last_sync_at the hub's time cut to the
// second, sync_error cleared, head_sha the copy's head of w's default branch
// and origin_head_sha origin's head of it in originHeads, each null when
// there is none. updated_at is not written. It returns the record read
// again after the transaction.
func (s *Service) completeSync(ctx context.Context, repo *gitrepo.Repo, w Workspace, originHeads map[string]string, in event.Input) (Workspace, error) {
	f := syncFields{LastSyncAt: s.deps.Now().UTC().Truncate(time.Second)}
	if w.DefaultBranch != nil {
		branch := *w.DefaultBranch
		head, found, err := repo.ResolveBranch(ctx, branch)
		if err != nil {
			return Workspace{}, err
		}
		if found {
			f.HeadSHA = &head
		}
		if id, ok := originHeads[branch]; ok {
			f.OriginHeadSHA = &id
		}
	}
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := recordSync(ctx, tx, w.Slug, f); err != nil {
			return err
		}
		_, err := s.deps.Events.Emit(ctx, tx, in)
		return err
	})
	if err != nil {
		return Workspace{}, fmt.Errorf("sync workspace %q: %w", w.Slug, err)
	}
	rec, found, err := BySlug(ctx, s.deps.Store.Main(), w.Slug)
	if err != nil {
		return Workspace{}, err
	}
	if !found {
		return Workspace{}, fmt.Errorf("sync workspace %q: %w", w.Slug, ErrNotFound)
	}
	return rec, nil
}

// eventSync is the event a completed sync writes.
const eventSync = "parley.workspace.sync"

// refUpdate is one ref a sync created or moved, as its event lists it:
// before is null for a ref the sync created.
type refUpdate struct {
	Ref    string  `json:"ref"`
	Before *string `json:"before"`
	After  string  `json:"after"`
}

// newRefUpdate is the update of ref from before ("" when the ref was
// created) to after.
func newRefUpdate(ref, before, after string) refUpdate {
	u := refUpdate{Ref: ref, After: after}
	if before != "" {
		u.Before = &before
	}
	return u
}

// syncEventInput is the parley.workspace.sync event of a sync of slug,
// without its actor: the names of the branches per result except
// up_to_date, each list sorted and [] when empty, and updated, every ref
// the sync created or moved, sorted by its full name.
func syncEventInput(slug string, branches []BranchResult, updated []refUpdate) event.Input {
	lists := map[BranchOutcome][]string{}
	for _, o := range []BranchOutcome{BranchCreated, BranchFastForward, BranchAhead, BranchDiverged, BranchReset} {
		lists[o] = []string{}
	}
	// branches is sorted by name, so each list is too.
	for _, b := range branches {
		if b.Result != BranchUpToDate {
			lists[b.Result] = append(lists[b.Result], b.Name)
		}
	}
	slices.SortFunc(updated, func(a, b refUpdate) int { return strings.Compare(a.Ref, b.Ref) })
	meta := map[string]any{"updated": append([]refUpdate{}, updated...)}
	for o, names := range lists {
		meta[string(o)] = names
	}
	return event.Input{Type: eventSync, Workspace: &slug, ResourceType: "workspace", ResourceID: slug, Metadata: meta}
}

// moves reports whether a branch with the result o was moved by the sync.
func moves(o BranchOutcome) bool {
	return o == BranchCreated || o == BranchFastForward || o == BranchReset
}

// syncBranch evaluates the branch name of repo once against originHead,
// origin's head of it: it classifies the hub's head and, for created,
// fast_forward and reset, moves the branch to originHead only while it is
// still at the head it classified, so that a move by another writer
// meanwhile is never lost; that case is an error matching
// gitrepo.ErrRefMoved. A diverged branch moves only when reset is true.
// Whether the branch is protected does not matter: sync is one of the ways a
// protected branch may move. before is the hub's head as classified, "" when
// the hub had no such branch.
func syncBranch(ctx context.Context, repo *gitrepo.Repo, name, originHead string, reset bool) (_ BranchResult, before string, _ error) {
	local, found, err := repo.ResolveBranch(ctx, name)
	if err != nil {
		return BranchResult{}, "", err
	}
	before = local
	outcome, err := classifyBranch(ctx, repo, local, found, originHead, reset)
	if err != nil {
		return BranchResult{}, "", fmt.Errorf("sync branch %s: %w", name, err)
	}
	if moves(outcome) {
		if err := repo.CompareAndSwapRef(ctx, "refs/heads/"+name, originHead, local); err != nil {
			return BranchResult{}, "", fmt.Errorf("sync branch %s: %w", name, err)
		}
		local = originHead
	}
	return BranchResult{Name: name, Result: outcome, LocalSHA: local, OriginSHA: originHead}, before, nil
}

// classifyBranch is the outcome for a branch whose head in the hub is local
// (found false when the hub has no such branch) and on origin originHead.
func classifyBranch(ctx context.Context, repo *gitrepo.Repo, local string, found bool, originHead string, reset bool) (BranchOutcome, error) {
	if !found {
		return BranchCreated, nil
	}
	if local == originHead {
		return BranchUpToDate, nil
	}
	if ff, err := repo.IsAncestor(ctx, local, originHead); err != nil || ff {
		return BranchFastForward, err
	}
	if ahead, err := repo.IsAncestor(ctx, originHead, local); err != nil || ahead {
		return BranchAhead, err
	}
	if reset {
		return BranchReset, nil
	}
	return BranchDiverged, nil
}

// fetchOrigin resolves the origin credential of w now, so that a changed
// secret counts at the next sync, and fetches w's origin into its copy with
// it.
func (s *Service) fetchOrigin(ctx context.Context, w Workspace) (gitrepo.Fetched, error) {
	cred, err := ResolveOriginCredential(ctx, s.deps.Secrets, w.Slug)
	if err != nil {
		return gitrepo.Fetched{}, err
	}
	return s.deps.Fetcher.FetchOrigin(ctx, s.RepoPath(w.Slug), deref(w.GitURL), cred, syncFetchDeadline)
}

// createTags creates in repo, in name order, every tag of tags that it
// lacks, at the value tags gives, and returns those it created. Each is
// created only while it does not exist, so that a tag another writer creates
// meanwhile keeps that writer's value.
func createTags(ctx context.Context, repo *gitrepo.Repo, tags map[string]string) (map[string]string, error) {
	have, err := repo.Tags(ctx)
	if err != nil {
		return nil, err
	}
	created := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(tags)) {
		if _, exists := have[name]; exists {
			continue
		}
		err := repo.CompareAndSwapRef(ctx, "refs/tags/"+name, tags[name], "")
		if errors.Is(err, gitrepo.ErrRefMoved) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create tag %s: %w", name, err)
		}
		created[name] = tags[name]
	}
	return created, nil
}
