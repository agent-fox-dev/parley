package workspace

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

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
// A failure is recorded, never retried, and Clone returns nil for it. When
// ctx ends first (the hub stops), the clone in progress is removed and the
// status is left as it is, so that the next start takes the work up again;
// Clone then returns ctx's error. Any other error means the outcome could
// not be recorded.
func (s *Service) Clone(ctx context.Context, slug string) error {
	started, err := StartClone(ctx, s.deps.Store.Main(), slug, s.deps.Now())
	if err != nil || !started {
		return err
	}
	w, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil || !found {
		return err
	}
	// The credential is resolved before git runs: a secret that cannot be
	// read fails the clone rather than turning into an anonymous request.
	cred, err := ResolveOriginCredential(ctx, s.deps.Secrets, slug)
	if err == nil {
		err = s.cloneFresh(ctx, w, cred)
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
	// What lies at the copy's place was not made by this clone; a fresh
	// clone replaces it.
	if err := gitrepo.Remove(dst); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.deps.Root, cloneDirPrefix)
	if err != nil {
		return fmt.Errorf("make a clone directory: %w", err)
	}
	defer func() { _ = gitrepo.Remove(tmp) }()
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
	head := branches[branch]
	w.DefaultBranch, w.HeadSHA, w.OriginHeadSHA, w.CloneError = &branch, &head, &head, nil
	if err := s.recordClone(ctx, w, StatusCloning); err != nil {
		return err
	}
	if step := s.postCloneStep(w.Mode); step != nil {
		copied, err := s.deps.Git.Open(ctx, dst)
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
