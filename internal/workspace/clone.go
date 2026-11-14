package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// Git is git as the clone work runs it: it clones an origin into a
// directory and opens the copy. gitrepo.Git satisfies it.
type Git interface {
	Clone(ctx context.Context, url string, cred Credential, dir string) error
	Open(ctx context.Context, dir string) (*gitrepo.Repo, error)
}

// PostCloneStep is work a workspace mode needs on a fresh copy before the
// workspace is ready, such as the upstream set-up of a carry-patch
// workspace. It is given the record as the clone has written it (status
// cloning, default_branch, head_sha and origin_head_sha set) and the copy
// at its place. An error it returns is logged and does not fail the clone:
// the copy is complete without the step.
type PostCloneStep func(ctx context.Context, w Workspace, repo *gitrepo.Repo) error

// cloneDirPrefix starts the name of a clone in progress in the repository
// root. No slug starts with a dot, so no copy's name can carry it, and what
// a clone that stopped half-way left behind is never taken for a copy.
const cloneDirPrefix = ".clone-"

// instanceKey is the key of the copy's git configuration that holds the
// instance id of the workspace the copy was made for, so that a copy an
// earlier workspace of the same slug left behind can be told apart.
const instanceKey = "parley.workspace"

// errNoBranches fails the clone of an origin without a branch: a workspace
// needs a default branch that resolves.
var errNoBranches = errors.New("origin has no branches")

// RepoPath is the place of the copy of the workspace slug:
// <[workspace].path>/<slug>.git. It is the one place the path is built;
// every other part asks the service for it.
func (s *Service) RepoPath(slug string) string {
	return filepath.Join(s.deps.Root, slug+".git")
}

// RegisterPostCloneStep makes step the post-clone step of the workspaces
// whose workspace_mode is mode, replacing the one registered before; nil
// removes it. The feature that owns a mode registers its step when the hub
// is composed, so that this package knows no mode's work.
func (s *Service) RegisterPostCloneStep(mode string, step PostCloneStep) {
	s.stepMu.Lock()
	defer s.stepMu.Unlock()
	if s.postClone == nil {
		s.postClone = map[string]PostCloneStep{}
	}
	if step == nil {
		delete(s.postClone, mode)
		return
	}
	s.postClone[mode] = step
}

// postCloneStep is the step registered for mode, nil without one.
func (s *Service) postCloneStep(mode string) PostCloneStep {
	s.stepMu.RLock()
	defer s.stepMu.RUnlock()
	return s.postClone[mode]
}

// Clone does the clone work of the workspace slug: it makes the hub's copy
// of the workspace's origin and leaves the workspace ready, or failed with a
// clone_error and nothing on disk. A workspace in pending is moved to
// cloning first, its old clone_error cleared, before origin is contacted; a
// workspace already in cloning is taken up again, as after a stop of the
// hub; a workspace in any other status, or none, is left alone, so that a
// failed clone is never tried again on its own.
//
// A copy of this workspace already at its place, such as the one a hub that
// ended without its stop (killed) between the clone's move into place and
// ready leaves, is kept rather than cloned again (see keepCopy); anything
// else there is replaced by a fresh clone.
//
// A failure is recorded, never retried, and Clone returns nil for it. When
// ctx ends first (the hub stops), the clone in progress is removed, the
// copy too once it is at its place, and the status is left as it is, so
// that the next start takes the work up again; Clone then returns ctx's
// error. Any other error means the outcome could not be recorded.
func (s *Service) Clone(ctx context.Context, slug string) error {
	started, err := StartClone(ctx, s.deps.Store.Main(), slug, s.deps.Now())
	if err != nil || !started {
		return err
	}
	w, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil || !found {
		return err
	}
	var cred Credential
	kept, err := s.keepCopy(ctx, w)
	if err == nil && !kept {
		// The credential is resolved before git runs: a secret that cannot
		// be read fails the clone rather than turning into an anonymous
		// request.
		cred, err = ResolveOriginCredential(ctx, s.deps.Secrets, slug)
		if err == nil {
			err = s.cloneFresh(ctx, w, cred)
		}
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.failClone(ctx, w, cred, err)
}

// cloneFresh clones the origin of w into a directory beside the copy's
// place and renames it into place only once it is complete, so that a
// clone that fails or is interrupted leaves nothing a later check could
// take for a copy. It records the default branch and its head, runs the
// mode's post-clone step and sets the status to ready.
func (s *Service) cloneFresh(ctx context.Context, w Workspace, cred Credential) error {
	dst := s.RepoPath(w.Slug)
	// What lies at the copy's place is not a copy keepCopy kept; a fresh
	// clone replaces it.
	if err := gitrepo.Remove(dst); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.deps.Root, cloneDirPrefix)
	if err != nil {
		return fmt.Errorf("make a clone directory: %w", err)
	}
	defer func() {
		if err := gitrepo.Remove(tmp); err != nil {
			s.deps.Logger.Warn("workspace clone directory removal failed", "slug", w.Slug, "error", err)
		}
	}()
	if err := s.deps.Git.Clone(ctx, deref(w.GitURL), cred, tmp); err != nil {
		return err
	}
	repo, err := s.deps.Git.Open(ctx, tmp)
	if err != nil {
		return err
	}
	branches, err := repo.Branches(ctx)
	if err != nil {
		return err
	}
	if len(branches) == 0 {
		return errNoBranches
	}
	branch, err := cloneBranch(ctx, repo, w.DefaultBranch, branches)
	if err != nil {
		return err
	}
	for name, id := range branches {
		if err := repo.SetOriginHead(ctx, name, id); err != nil {
			return err
		}
	}
	if err := repo.SetConfig(ctx, instanceKey, w.InstanceID); err != nil {
		return err
	}
	if err := repo.SetDefaultBranch(ctx, branch); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("move the clone into place: %w", err)
	}
	err = s.finishClone(ctx, w, branch, branches[branch])
	if err != nil && ctx.Err() != nil {
		// A stop leaves nothing at the copy's place, so that the next start
		// clones afresh and runs the mode's post-clone step, which keepCopy
		// would skip on the copy left here.
		if rmErr := gitrepo.Remove(dst); rmErr != nil {
			s.deps.Logger.Warn("workspace clone cleanup failed", "slug", w.Slug, "error", rmErr)
		}
	}
	return err
}

// finishClone records the default branch branch and its head head of the
// copy of w now at its place, runs the mode's post-clone step and sets the
// status to ready.
func (s *Service) finishClone(ctx context.Context, w Workspace, branch, head string) error {
	w.DefaultBranch, w.HeadSHA, w.OriginHeadSHA, w.CloneError = &branch, &head, &head, nil
	if err := s.recordClone(ctx, w, StatusCloning); err != nil {
		return err
	}
	if step := s.postCloneStep(w.Mode); step != nil {
		copied, err := s.deps.Git.Open(ctx, s.RepoPath(w.Slug))
		if err == nil {
			err = step(ctx, w, copied)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			s.deps.Logger.Error("workspace post-clone step failed", "slug", w.Slug, "workspace_mode", w.Mode, "error", err)
		}
	}
	return s.recordClone(ctx, w, StatusReady)
}

// keepCopy keeps what lies at the place of w's copy when it is a complete
// copy of this workspace: it opens as a repository, the workspace's default
// branch (the copy's own while the record names none) resolves to a commit,
// its remote.origin.url is w's git_url and its parley.workspace is w's
// instance id, which only a finished clone for this very record writes. It
// then records the copy's head of that branch and the branch itself, sets
// the status to ready and reports kept, without asking origin and without
// the mode's post-clone step, which ran when the copy was made. Anything
// else at the place is left for a fresh clone; an error is the end of ctx
// or a write that failed.
func (s *Service) keepCopy(ctx context.Context, w Workspace) (kept bool, err error) {
	dst := s.RepoPath(w.Slug)
	if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	branch, head, origin, ok := s.validCopy(ctx, dst, w)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !ok {
		return false, nil
	}
	w.DefaultBranch, w.HeadSHA, w.CloneError = &branch, &head, nil
	if origin != "" {
		w.OriginHeadSHA = &origin
	}
	return true, s.recordClone(ctx, w, StatusReady)
}

// validCopy reports whether dir holds a complete copy of w, as keepCopy
// describes it, and returns its default branch, that branch's head and the
// origin head it keeps for it ("" for none). A git error is no copy.
func (s *Service) validCopy(ctx context.Context, dir string, w Workspace) (branch, head, origin string, ok bool) {
	repo, err := s.deps.Git.Open(ctx, dir)
	if err != nil {
		return "", "", "", false
	}
	if id, found, err := repo.Config(ctx, instanceKey); err != nil || !found || id != w.InstanceID {
		return "", "", "", false
	}
	if url, err := repo.OriginURL(ctx); err != nil || url != deref(w.GitURL) {
		return "", "", "", false
	}
	branch = deref(w.DefaultBranch)
	if w.DefaultBranch == nil {
		if branch, err = repo.DefaultBranch(ctx); err != nil {
			return "", "", "", false
		}
	}
	head, found, err := repo.ResolveBranch(ctx, branch)
	if err != nil || !found {
		return "", "", "", false
	}
	if id, found, err := repo.OriginHead(ctx, branch); err == nil && found {
		origin = id
	}
	return branch, head, origin, true
}

// cloneBranch is the workspace's default branch on a fresh copy whose
// branches are branches: the branch given at creation, which origin must
// have, else origin's default branch, which the copy's HEAD names. When
// origin's HEAD names no branch it has, the first branch by name is taken,
// so that the choice does not depend on chance.
func cloneBranch(ctx context.Context, repo *gitrepo.Repo, given *string, branches map[string]string) (string, error) {
	if given != nil {
		if _, ok := branches[*given]; !ok {
			return "", fmt.Errorf("origin has no branch %q, the default_branch given at creation", *given)
		}
		return *given, nil
	}
	if b, err := repo.DefaultBranch(ctx); err == nil {
		if _, ok := branches[b]; ok {
			return b, nil
		}
	} else if ctx.Err() != nil {
		return "", err
	}
	return slices.Sorted(maps.Keys(branches))[0], nil
}

// recordClone writes the clone fields of w with status in one write.
func (s *Service) recordClone(ctx context.Context, w Workspace, status Status) error {
	return UpdateClone(ctx, s.deps.Store.Main(), w.Slug, CloneFields{
		Status: status, DefaultBranch: w.DefaultBranch, HeadSHA: w.HeadSHA,
		OriginHeadSHA: w.OriginHeadSHA, CloneError: w.CloneError, UpdatedAt: s.deps.Now(),
	})
}

// The waits between the tries of failUnrecorded: the first, doubled after
// each try up to the longest.
const (
	unrecordedRetryFirst = 100 * time.Millisecond
	unrecordedRetryMost  = 30 * time.Second
)

// failUnrecorded settles the clone work of the workspace slug whose outcome
// could not be recorded because of the store error cause, so that the
// workspace does not stay in pending or cloning, which the state guard
// answers as busy, until the next start. It sets the status to failed with
// cause's text as clone_error once the store takes the write, trying again
// with a growing wait until it does or ctx ends; the hub's stop leaves the
// work to the next start, as for an interrupted clone. Once failed, the copy
// is removed, as for any failed clone. A workspace no longer in pending or
// cloning, such as one whose outcome was written though the write reported
// an error, or one deleted, is left as it is.
func (s *Service) failUnrecorded(ctx context.Context, slug string, cause error) {
	msg := s.deps.Scrubber.Scrub("the outcome of the clone could not be recorded: " + cause.Error())
	for wait := unrecordedRetryFirst; ; wait = min(2*wait, unrecordedRetryMost) {
		failed, err := failUnfinished(ctx, s.deps.Store.Main(), slug, msg, s.deps.Now())
		if err == nil {
			if failed {
				if err := gitrepo.Remove(s.RepoPath(slug)); err != nil {
					s.deps.Logger.Error("workspace clone cleanup failed", "slug", slug, "error", err)
				}
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.deps.Logger.Warn("workspace clone failure not recorded", "slug", slug, "error", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// failClone ends the clone of w that failed with cause: it removes
// whatever lies at the copy's place, and sets the status to failed with
// cause's text as clone_error, scrubbed of the secret values the hub knows
// and of cred, with URL userinfo removed and cut to the scrubber's length.
// It writes no event and schedules no further attempt.
func (s *Service) failClone(ctx context.Context, w Workspace, cred Credential, cause error) error {
	if err := gitrepo.Remove(s.RepoPath(w.Slug)); err != nil {
		s.deps.Logger.Error("workspace clone cleanup failed", "slug", w.Slug, "error", err)
	}
	msg := s.deps.Scrubber.Scrub(cause.Error(), cred.Password)
	s.deps.Logger.Warn("workspace clone failed", "slug", w.Slug, "error", msg)
	w.HeadSHA, w.OriginHeadSHA, w.CloneError = nil, nil, &msg
	return s.recordClone(ctx, w, StatusFailed)
}
