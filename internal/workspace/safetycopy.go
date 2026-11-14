package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// BranchesPusher offers several branches of a workspace's copy to its
// origin in one push, as gitrepo's Git.PushBranches describes it.
// gitrepo.Git satisfies it.
type BranchesPusher interface {
	PushBranches(ctx context.Context, req gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error)
}

// The outcomes of a safety copy, the origin_push of the event of the
// operation that made it.
const (
	// originPushOK: origin took every branch offered, or there was none to
	// offer.
	originPushOK = "ok"
	// originPushPartial: origin took at least one branch and not another.
	originPushPartial = "partial"
	// originPushFailed: at least one branch was offered and origin took
	// none, or the offer could not be made.
	originPushFailed = "failed"
)

// safetyCopy offers every branch of w's copy to the branch of the same name
// on w's origin as a fast-forward, in one push under the push deadline,
// before an archive or a reclone puts commits that exist only in the hub
// out of reach. A carry-patch workspace's integration branch is left out,
// since the hub rebuilds it from upstream and the patches. The origin
// credential is resolved from w's secrets; when it cannot be read nothing
// is offered, never an anonymous push. Each branch origin took has its
// origin head, the ref refs/remotes/origin/<name> of the copy, set to the
// commit offered; a refused branch keeps its own. The outcome is one of
// ok, partial and failed, and nothing origin answers stops the caller:
// there is no error, a failure is logged with the slug, its text scrubbed
// of the credential. A workspace without a copy, whose clone failed, has
// nothing to offer.
func (s *Service) safetyCopy(ctx context.Context, w Workspace) string {
	dir := s.RepoPath(w.Slug)
	if w.Status == StatusFailed || w.GitURL == nil {
		return originPushOK
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return originPushOK
	}
	var cred Credential
	fail := func(what string, err error) {
		s.deps.Logger.Warn("workspace safety copy failed", "slug", w.Slug, "step", what,
			"error", s.deps.Scrubber.Scrub(err.Error(), cred.Password))
	}
	repo, err := s.deps.Git.Open(ctx, dir)
	if err != nil {
		fail("open the copy", err)
		return originPushFailed
	}
	heads, err := repo.Branches(ctx)
	if err != nil {
		fail("list the branches", err)
		return originPushFailed
	}
	if w.Mode == ModeCarryPatch && w.IntegrationBranch != nil {
		delete(heads, *w.IntegrationBranch)
	}
	if len(heads) == 0 {
		return originPushOK
	}
	if cred, err = ResolveOriginCredential(ctx, s.deps.Secrets, w.Slug); err != nil {
		fail("resolve the origin credential", err)
		return originPushFailed
	}
	pushCtx, cancel := context.WithTimeout(ctx, s.deps.PushDeadline)
	defer cancel()
	results, err := s.deps.SafetyPusher.PushBranches(pushCtx, gitrepo.PushBranchesRequest{
		Dir: repo.Dir(), URL: *w.GitURL, Credential: cred, Heads: heads,
	})
	accepted := 0
	var refused []string
	for _, r := range results {
		if r.Err != nil {
			refused = append(refused, r.Err.Error())
			continue
		}
		accepted++
		if err := repo.SetOriginHead(ctx, r.Branch, r.Commit); err != nil {
			fail("record the origin head", fmt.Errorf("branch %q: %w", r.Branch, err))
		}
	}
	if err != nil {
		refused = append(refused, err.Error())
	}
	if len(refused) > 0 {
		fail("push", errors.New(strings.Join(refused, "; ")))
	}
	switch {
	case accepted == len(heads):
		return originPushOK
	case accepted > 0:
		return originPushPartial
	}
	return originPushFailed
}
