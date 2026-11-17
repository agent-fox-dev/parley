package carrypatch

import (
	"context"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// DefaultForwardLimit is how long a forward of a patch branch to origin may
// run, the limit the ref writer puts on the forward of a push too: the
// landing guard has no ref writer around it to keep one.
const DefaultForwardLimit = 120 * time.Second

// The messages a forward fails with. Every reason after
// msgOriginRejected is a fixed text, apart from what origin's own hooks
// wrote for a refused update, so that a message carries no credential and
// nothing of the hub's network.
const (
	msgNoOriginCredential = "failed to resolve origin credentials"
	msgOriginRejected     = "origin rejected push: "

	reasonTimedOut        = "timed out"
	reasonNonFastForward  = "non-fast-forward"
	reasonUnauthenticated = "authentication failed"
	reasonForbidden       = "permission denied"
	reasonNotFound        = "repository not found"
	reasonUnreachable     = "origin unreachable"
	reasonPushFailed      = "push failed"
)

// ForwardLimit is the limit of a forward: Deps.ForwardLimit when it is
// positive and shorter, else DefaultForwardLimit.
func (c *Component) ForwardLimit() time.Duration {
	if d := c.d.ForwardLimit; d > 0 && d < DefaultForwardLimit {
		return d
	}
	return DefaultForwardLimit
}

// Forward pushes commit to refs/heads/<branch> on the origin of the
// workspace w, without force and without a lease, so that origin takes it
// only as a fast-forward, before the hub accepts the update. It reads the
// commit from repo, the repository the update is written to, or from the
// hub's copy when repo is nil: for a git push repo is the view of its
// quarantine, since the pushed objects reach the copy only once the push is
// accepted, after its forwards. The origin credential is read
// from the workspace's secrets now, so that one changed since the last
// forward counts; one that cannot be read fails the forward before origin
// is contacted, never with an anonymous push. The push runs through RunGit
// within the forward limit and ctx: when either ends first git is killed
// and the forward fails as timed out.
//
// Forward returns nil once origin took the commit, and then records it as
// origin's head of branch in the hub's copy, when the copy holds it. It
// never writes the hub's branch. A failure is an error whose text is the
// message the update is refused with, "origin rejected push: <reason>" or
// the credential's, scrubbed of the credential and of URL user information
// and at most 1024 bytes long; the recorded origin head is left as it was.
func (c *Component) Forward(ctx context.Context, repo *gitrepo.Repo, w workspace.Workspace, branch, commit string) error {
	log := c.logger().With("workspace", w.Slug, "branch", branch)
	if w.GitURL == nil || *w.GitURL == "" {
		log.Warn("forward to origin failed: the workspace has no origin")
		return errors.New(msgOriginRejected + reasonPushFailed)
	}
	if c.d.Secrets == nil {
		log.Warn("forward to origin failed: no secret reader to read the origin credential with")
		return errors.New(msgNoOriginCredential)
	}
	cred, err := workspace.ResolveOriginCredential(ctx, c.d.Secrets, w.Slug)
	if err != nil {
		log.Warn("forward to origin failed: the origin credential cannot be read", "error", c.scrub(err.Error()))
		return errors.New(msgNoOriginCredential)
	}
	var copyRepo *gitrepo.Repo
	if c.d.RepoPath != nil {
		copyRepo, err = c.d.Git.Open(ctx, c.d.RepoPath(w.Slug))
	} else {
		err = errNotBound
	}
	if repo == nil {
		repo = copyRepo
	}
	if repo == nil {
		log.Warn("forward to origin failed: the hub's copy cannot be opened", "error", err)
		return errors.New(msgOriginRejected + reasonPushFailed)
	}
	req := gitrepo.PushRequest{URL: *w.GitURL, Credential: cred, Commit: commit, Branch: branch}
	err = RunGit(ctx, c.ForwardLimit(), cred, func(ctx context.Context, _ time.Duration) error {
		_, err := repo.Push(ctx, req)
		return err
	})
	if err != nil {
		msg := c.scrub(msgOriginRejected+forwardReason(ctx, err), cred.Username, cred.Password)
		log.Info("forward to origin failed", "reason", msg, "error", c.scrub(err.Error(), cred.Username, cred.Password))
		return errors.New(msg)
	}
	if copyRepo != nil {
		c.recordForwarded(ctx, copyRepo, w.Slug, branch, commit)
	}
	return nil
}

// forwardReason is the reason a forward whose push failed with err is
// refused with: timed out when the push's limit or ctx ended it, origin's
// own words for an update origin refused (those of its hooks, else the
// status it gave the ref), else the fixed text of the failure's class.
func forwardReason(ctx context.Context, err error) string {
	var pushErr *gitrepo.PushError
	switch {
	case errors.Is(err, ErrGitTimeout) || ctx.Err() != nil:
		return reasonTimedOut
	case errors.Is(err, gitrepo.ErrNonFastForward):
		return reasonNonFastForward
	case errors.As(err, &pushErr) && errors.Is(pushErr.Class, gitrepo.ErrOriginRejected):
		if pushErr.OriginMessage != "" {
			return pushErr.OriginMessage
		}
		if pushErr.Reason != "" {
			return pushErr.Reason
		}
		return reasonPushFailed
	case errors.Is(err, gitrepo.ErrUnauthenticated):
		return reasonUnauthenticated
	case errors.Is(err, gitrepo.ErrForbidden):
		return reasonForbidden
	case errors.Is(err, gitrepo.ErrRepositoryNotFound):
		return reasonNotFound
	case errors.Is(err, gitrepo.ErrUnreachable):
		return reasonUnreachable
	}
	return reasonPushFailed
}

// recordForwarded records commit, which origin took on branch, as origin's
// head of branch in the hub's copy, when the copy holds the commit. The
// commit of a git push is readable only in the push's quarantine until the
// push is accepted, and a ref of the copy must never name an object a
// refused push takes away again: such a forward leaves the record to the
// next refresh from origin. The record is written even when the caller has
// gone meanwhile, since origin holds the commit; one that fails is logged,
// and the forward stands.
func (c *Component) recordForwarded(ctx context.Context, copyRepo *gitrepo.Repo, slug, branch, commit string) {
	ctx = context.WithoutCancel(ctx)
	err := RunGit(ctx, c.ForwardLimit(), gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		missing, err := copyRepo.Missing(ctx, []string{commit})
		if err != nil || len(missing) > 0 {
			return err
		}
		return copyRepo.SetOriginHead(ctx, branch, commit)
	})
	if err != nil {
		c.logger().Warn("forward to origin succeeded and origin's head could not be recorded",
			"workspace", slug, "branch", branch, "error", err)
	}
}
