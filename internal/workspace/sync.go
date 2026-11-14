package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
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

// ExcludedBranchProvider names the branches of a carry-patch workspace that
// sync leaves to the feature that maintains them, such as the workspace's
// patch branches, which the rebuild and the patch rules move and the plain
// per-branch pass must not overrule. ExcludedBranches is asked once per sync
// of a carry-patch workspace, with its slug, and returns the branch names,
// without refs/heads/; an error stops that sync before any branch is
// compared.
type ExcludedBranchProvider interface {
	ExcludedBranches(ctx context.Context, slug string) ([]string, error)
}

// RegisterExcludedBranchProvider makes p the provider of the branches a sync
// of a carry-patch workspace leaves out, replacing the one registered
// before; nil removes it. The feature that owns the patch branches registers
// it when the hub is composed; without one only a carry-patch workspace's
// integration branch is left out.
func (s *Service) RegisterExcludedBranchProvider(p ExcludedBranchProvider) {
	s.excludeMu.Lock()
	defer s.excludeMu.Unlock()
	s.excludeProvider = p
}

// excludedBranches is the set of the branches of w that the per-branch pass
// leaves out: for a carry-patch workspace its integration branch and the
// branches the registered provider names for it; none, without asking the
// provider, for any other workspace. A provider that fails stops the sync
// with an internal error before any branch is compared, since moving a patch
// branch by mistake would be worse than a sync the caller repeats; its text
// is scrubbed, as it may hold what the provider read.
func (s *Service) excludedBranches(ctx context.Context, w Workspace) (map[string]bool, error) {
	excluded := map[string]bool{}
	if w.Mode != ModeCarryPatch {
		return excluded, nil
	}
	if b := deref(w.IntegrationBranch); b != "" {
		excluded[b] = true
	}
	s.excludeMu.RLock()
	p := s.excludeProvider
	s.excludeMu.RUnlock()
	if p == nil {
		return excluded, nil
	}
	names, err := p.ExcludedBranches(ctx, w.Slug)
	if err != nil {
		return nil, server.Internal(s.deps.Scrubber.Scrub(fmt.Sprintf(
			"the sync of workspace %q stopped before any branch was compared: the excluded-branch provider failed: %v",
			w.Slug, err)))
	}
	for _, name := range names {
		excluded[name] = true
	}
	return excluded, nil
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
	// Branches has one entry per branch of origin that the sync did not
	// leave out, sorted by name in byte order; it is empty, never nil, when
	// there is none, so that it marshals as [].
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
// looked at nor reported, and neither is a branch of a carry-patch workspace
// that excludedBranches leaves out, although its origin head is fetched. Last it records the completion (see
// completeSync). The caller has checked that the workspace may be synced;
// opts.Actor must be the admin token or a user's credential.
//
// Sync takes no lock on the workspace and never writes its status: syncs of
// one workspace wait only for one another's fetch (see fetchOrigin), and
// beside those, pushes and landings they meet only at the compare-and-swap
// of single refs. A branch that keeps moving under the sync stops it with an
// internal error before anything is recorded; the moves made by then stay.
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
	excluded, err := s.excludedBranches(ctx, w)
	if err != nil {
		return SyncResult{}, err
	}
	branches := make([]BranchResult, 0, len(fetched.Branches))
	var updated []refUpdate
	for _, name := range slices.Sorted(maps.Keys(fetched.Branches)) {
		if excluded[name] {
			continue
		}
		b, before, err := s.syncBranch(ctx, repo, slug, name, fetched.Branches[name], opts.ResetToOrigin)
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

// maxBranchEvaluations is how often a sync evaluates one branch before it
// gives up on a branch that another writer keeps moving, so that a sync
// under constant writes ends; the caller syncs again.
const maxBranchEvaluations = 5

// syncBranch brings the branch name of repo, the copy of the workspace slug,
// up to date with originHead, origin's head of it. An evaluation classifies
// the hub's head and, for created, fast_forward and reset, moves the branch
// to originHead only while it is still at the head it classified. When
// another writer moved the branch meanwhile, its head stays and the branch is
// evaluated again from that head, against the same originHead, up to
// maxBranchEvaluations times; after that the error is an internal one naming
// the branch. A diverged branch moves only when reset is true. Whether the
// branch is protected does not matter: sync is one of the ways a protected
// branch may move. The result and before, the head the move replaced ("" when
// it created the branch), are those of the last evaluation.
func (s *Service) syncBranch(ctx context.Context, repo *gitrepo.Repo, slug, name, originHead string, reset bool) (_ BranchResult, before string, _ error) {
	for range maxBranchEvaluations {
		local, found, err := repo.ResolveBranch(ctx, name)
		if err != nil {
			return BranchResult{}, "", err
		}
		outcome, err := classifyBranch(ctx, repo, local, found, originHead, reset)
		if err != nil {
			return BranchResult{}, "", fmt.Errorf("sync branch %s: %w", name, err)
		}
		result := BranchResult{Name: name, Result: outcome, LocalSHA: local, OriginSHA: originHead}
		if !moves(outcome) {
			return result, local, nil
		}
		if s.deps.BeforeRefSwap != nil {
			s.deps.BeforeRefSwap(ctx, slug, name)
		}
		err = repo.CompareAndSwapRef(ctx, "refs/heads/"+name, originHead, local)
		if errors.Is(err, gitrepo.ErrRefMoved) {
			continue
		}
		if err != nil {
			return BranchResult{}, "", fmt.Errorf("sync branch %s: %w", name, err)
		}
		result.LocalSHA = originHead
		return result, local, nil
	}
	return BranchResult{}, "", server.Internal(fmt.Sprintf(
		"the sync of workspace %q stopped: branch %q moved in the hub during each of its %d evaluations; sync again",
		slug, name, maxBranchEvaluations))
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
// it. A credential that cannot be resolved, a fetch that fails and one cut
// off at its deadline are one failure, which failFetch records and turns
// into the answer; without a credential nothing is fetched, since an
// anonymous fetch would not be the one the owner configured. Two fetches
// into one copy collide on git's locks of the origin heads they both write,
// so the syncs of one workspace fetch one after the other: a sync waits for
// the other's fetch, never answers busy, and runs the rest of its work
// beside it.
func (s *Service) fetchOrigin(ctx context.Context, w Workspace) (gitrepo.Fetched, error) {
	cred, err := ResolveOriginCredential(ctx, s.deps.Secrets, w.Slug)
	if err != nil {
		return gitrepo.Fetched{}, s.failFetch(ctx, w.Slug, cred, err)
	}
	unlock := s.fetchLocks.lock(w.Slug)
	fetched, err := s.deps.Fetcher.FetchOrigin(ctx, s.RepoPath(w.Slug), deref(w.GitURL), cred, syncFetchDeadline)
	unlock()
	if err != nil {
		return gitrepo.Fetched{}, s.failFetch(ctx, w.Slug, cred, err)
	}
	return fetched, nil
}

// fetchFailurePrefix begins the text of a sync that could not fetch origin,
// so that a caller can tell it from a failure of another source a sync
// extension fetches.
const fetchFailurePrefix = "origin fetch failed: "

// failFetch records that the sync of the workspace slug could not fetch
// origin because of cause and returns the upstream error the route answers.
// The text is cause's behind fetchFailurePrefix, scrubbed of the secret
// values the hub knows and of cred's secret, with URL userinfo removed and
// cut to the scrubber's length; it is both sync_error and the answer's
// message. Only sync_error is written and no event: the fetch is atomic and
// tags are created only after it, so the copy is as it was.
func (s *Service) failFetch(ctx context.Context, slug string, cred Credential, cause error) error {
	text := s.deps.Scrubber.Scrub(fetchFailurePrefix+cause.Error(), cred.Password)
	s.deps.Logger.Warn("workspace sync could not fetch origin", "slug", slug, "error", text)
	if err := recordSyncError(ctx, s.deps.Store.Main(), slug, text); err != nil {
		return err
	}
	return server.UpstreamError(text)
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

// keyedLocks is a mutex per key, here a workspace's slug, that exists only
// while someone holds or waits for it. Its zero value is ready for use.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

// keyedLock is the mutex of one key and the number of its holders and
// waiters.
type keyedLock struct {
	sync.Mutex
	users int
}

// lock waits until key's mutex is free, takes it and returns the function
// that frees it.
func (k *keyedLocks) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		defer k.mu.Unlock()
		if l.users--; l.users == 0 {
			delete(k.locks, key)
		}
	}
}
