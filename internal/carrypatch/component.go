package carrypatch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Deps is what the component works with, given once by Bind.
type Deps struct {
	// Store holds the workspace records, whose integration_branch the
	// set-up writes and whose mode and state the guard reads, and the
	// patch lists.
	Store *store.Store
	// Secrets reads a workspace's secrets, among them its upstream and
	// origin credentials.
	Secrets workspace.SecretReader
	// Git runs git.
	Git gitrepo.Git
	// RepoPath is the git directory of the copy of the workspace slug.
	RepoPath func(slug string) string
	// FetchRemote, when set, replaces the git fetch of FetchUpstream,
	// gitrepo.Git.FetchRemote, and is called as RunGit calls that; only
	// tests set it, to make the fetch fail with the text they give.
	FetchRemote func(ctx context.Context, dir, url, name string, cred gitrepo.Credential, deadline time.Duration) (map[string]string, error)
	// UpstreamFetchDeadline, when positive and shorter, replaces
	// DefaultGitDeadline as the one deadline of a whole upstream fetch;
	// only tests set it.
	UpstreamFetchDeadline time.Duration
	// Settings is the registry of the workspace settings, through which
	// ResolveBranch reads a workspace's patch_branch_source; nil reads it
	// as hub for every workspace.
	Settings *workspace.SettingsRegistry
	// BranchFetchDeadline, when positive, replaces
	// DefaultBranchFetchDeadline as the deadline of the single-branch fetch
	// from origin of ResolveBranch; only tests set it.
	BranchFetchDeadline time.Duration
	// BeforeBranchCreate, when set, is called by ResolveBranch with the
	// branch after its lookups and before it creates the branch; only tests
	// set it, to write the branch in between as another writer would.
	BeforeBranchCreate func(branch string)
	// BeforeRefSwap, when set, is called by a reset to origin or a sync's
	// origin refresh with the workspace and the branch after it compared
	// the branch's tip with origin's and before it records the outcome,
	// writes the backup ref and moves the branch, outside any store
	// transaction; only tests set it, to move the branch or change the
	// patch in between as another writer would.
	BeforeRefSwap func(slug, branch string)
	// BranchMoves is told of every patch branch a sync's origin refresh
	// created or moved, after the move is recorded, so that the features
	// observing a sync's branch moves see those too; nil tells nobody.
	BranchMoves BranchMoveNotifier
	// BeforeRollbackSwap, when set, is called by a rollback with the
	// workspace and the full name of its integration branch after the
	// rollback read the branch's head and before its compare-and-swap,
	// outside any store transaction; only tests set it, to move the branch
	// in between as a publishing rebuild would.
	BeforeRollbackSwap func(slug, ref string)
	// CommitterName and CommitterEmail are the landing identity, the
	// committer of every commit a rebuild makes; empty, they are
	// DefaultCommitterName and DefaultCommitterEmail.
	CommitterName, CommitterEmail string
	// BeforeRebuildPatch, when set, is called by an attempt of the rebuild
	// job with the job's id and the index, from 0, of a patch of the run's
	// list before the run takes it up, outside any store transaction; only
	// tests set it, to look at the repository and the job between patches
	// or to change them as another writer would.
	BeforeRebuildPatch func(job string, i int)
	// BeforeRebuildPublish, when set, is called by an attempt of the
	// rebuild job with the job's id once every patch is processed and
	// before the publish reads the integration branch, outside any store
	// transaction; only tests set it.
	BeforeRebuildPublish func(job string)
	// BeforeRebuildSwap, when set, is called by an attempt of the rebuild
	// job with the job's id after the publish read the integration branch
	// and prepared the patch list's changes, and before its compare-and-swap.
	// The publish's store transaction is open then: the hook may read the
	// store and move refs, but a write through Store.Tx would wait for ever.
	// Only tests set it, to move the branch as another writer would or to
	// look at what a reader sees.
	BeforeRebuildSwap func(job string)
	// ConflictFiles describes the files of the conflict report of a patch
	// that did not apply in a rebuild; nil is GitConflictFiles.
	ConflictFiles ConflictFileDescriber
	// TrialMerge, when set, replaces gitrepo.Repo.TrialMerge as the trial
	// merge of a patch tip onto the predicted state in a preview, and is
	// called as RunGit calls that; only tests set it, to make the merge of
	// a patch fail.
	TrialMerge func(ctx context.Context, repo *gitrepo.Repo, ours, theirs string) (gitrepo.TrialMergeResult, error)
	// Push, when set, replaces gitrepo.Git.Push as the push of the
	// integration branch to origin after a rebuild's publish, and is called
	// as RunGit calls that; only tests set it, to see the push or make it
	// fail.
	Push func(ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error)
	// RebuildPushDeadline, when positive and shorter, replaces
	// DefaultRebuildPushDeadline as the deadline of that push; only tests
	// set it.
	RebuildPushDeadline time.Duration
	// RebuildGitDeadline, when positive and shorter, replaces
	// DefaultGitDeadline as the deadline of each git operation of a rebuild
	// attempt; only tests set it.
	RebuildGitDeadline time.Duration
	// ForwardLimit, when positive and shorter, replaces DefaultForwardLimit
	// as the limit of a forward of a patch branch to origin; only tests set
	// it.
	ForwardLimit time.Duration
	// Scrubber removes credentials from the text of a rebuild's failure and
	// of its events before they are stored; nil is a scrubber that knows no
	// secret value of the hub's, which still removes the values given to
	// each call and the user information of URLs.
	Scrubber *scrub.Scrubber
	// Events writes the events of the patch list in the transaction of the
	// change they report.
	Events Emitter
	// Jobs is the hub's job queue, which holds the rebuild jobs.
	Jobs *jobs.Queue
	// Now is the clock of the patch list's times; nil is time.Now.
	Now func() time.Time
	// Logger receives what fails after an answer is decided, such as the
	// deletion of a removed patch's backup ref; nil discards it.
	Logger *slog.Logger
}

// The landing identity of a hub that names none, the same as the land
// component's.
const (
	DefaultCommitterName  = "Parley"
	DefaultCommitterEmail = "parley@localhost"
)

// Component holds the carry-patch operations of the hub. Its zero value
// is usable once Bind has given it its Deps.
type Component struct {
	d Deps

	// mu guards rebuilds, the registered rebuild enqueuer, and outcomes,
	// the outcome of the last attempt of each rebuild job whose settlement
	// has not yet reported it, by job id.
	mu       sync.Mutex
	rebuilds RebuildEnqueuer
	outcomes map[string]attemptOutcome

	// setups are held by a repeated set-up of a workspace's copy, a sync's
	// or a rebuild attempt's, which fetches upstream into it: two git
	// fetches into one repository lock the same refs and its configuration,
	// and one of them fails.
	setups slugLocks

	// triggers are held by the trigger of an accepted request, so that the
	// triggers of one workspace run one at a time; apart from setups, which
	// an upstream fetch holds for minutes.
	triggers slugLocks
}

// slugLocks is a mutex per workspace slug, which exists only while someone
// holds or waits for it. Its zero value is ready for use.
type slugLocks struct {
	mu    sync.Mutex
	locks map[string]*slugLock
}

// slugLock is the mutex of one slug, a channel that holds one token so that
// a waiter can give up when its context ends, and the number of its holders
// and waiters.
type slugLock struct {
	token chan struct{}
	users int
}

// lock waits until the mutex of slug is free or ctx ends, takes it and
// returns the function that frees it.
func (k *slugLocks) lock(ctx context.Context, slug string) (unlock func(), err error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*slugLock{}
	}
	l := k.locks[slug]
	if l == nil {
		l = &slugLock{token: make(chan struct{}, 1)}
		k.locks[slug] = l
	}
	l.users++
	k.mu.Unlock()
	leave := func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if l.users--; l.users == 0 {
			delete(k.locks, slug)
		}
	}
	select {
	case l.token <- struct{}{}:
		return func() {
			<-l.token
			leave()
		}, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}

// Bind gives the component its Deps, before its first use.
func (c *Component) Bind(d Deps) {
	c.d = d
}

// now is the time of the component's clock.
func (c *Component) now() time.Time {
	if c.d.Now == nil {
		return time.Now()
	}
	return c.d.Now()
}

// logger is the component's logger, one that discards when none is bound.
func (c *Component) logger() *slog.Logger {
	if c.d.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.d.Logger
}
