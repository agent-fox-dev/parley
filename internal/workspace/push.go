package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Pusher pushes one commit of a workspace's copy to one branch of its
// origin, as gitrepo's Git.Push describes it. gitrepo.Git satisfies it.
type Pusher interface {
	Push(ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error)
}

// PatchBranchLookup tells which branches of a carry-patch workspace are
// patch branches, which reach origin only through the feature that keeps
// the patch list, never through the push route. IsPatchBranch reports
// whether branch, without refs/heads/, is the branch of a registered patch
// of the workspace slug that is not deleted; an error means the patch list
// could not be read.
type PatchBranchLookup interface {
	IsPatchBranch(ctx context.Context, slug, branch string) (bool, error)
}

// RegisterPatchBranchLookup makes l the patch-branch lookup of the push
// route, replacing the one registered before; nil removes it. The feature
// that keeps the patch list registers it when the hub is composed; without
// one no branch is a patch branch.
func (s *Service) RegisterPatchBranchLookup(l PatchBranchLookup) {
	s.patchMu.Lock()
	defer s.patchMu.Unlock()
	s.patchLookup = l
}

// remoteOrigin is the name a push answer gives the remote it pushed to.
const remoteOrigin = "origin"

// pushRoute is POST /workspaces/:slug/branches/:name/push. The scope check
// and then the access check run before the handler, as for a sync, so that
// a caller who may not push is refused before the body is looked at. The
// router hands the handler the branch name percent-decoded once.
func (s *Service) pushRoute() server.Route {
	return server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/branches/:name/push", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.servePush}
}

// pushAnswer is the answer of a push origin took: the branch, the commit
// pushed, the remote and its URL, and what the push did.
type pushAnswer struct {
	Branch    string             `json:"branch"`
	SHA       string             `json:"sha"`
	Remote    string             `json:"remote"`
	RemoteURL string             `json:"remote_url"`
	Result    gitrepo.PushResult `json:"result"`
}

// servePush pushes the route's branch of the route's workspace to the
// workspace's origin. It reads the branch's head once and pushes that
// commit, so that a branch that moves meanwhile changes neither what is
// pushed nor what is answered; it takes no lock. The origin credential is
// resolved from the workspace's secrets for this push.
func (s *Service) servePush(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	force, err := pushForce(r)
	if err != nil {
		return err
	}
	name := server.Param(r, "name")
	if err := ValidateBranchName(name); err != nil {
		return server.InvalidRequest(fmt.Sprintf("branch name %q: %v", name, err))
	}
	slug := server.Param(r, "slug")
	rec, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil {
		return err
	}
	if !found {
		// Deleted between the access check and this read: the answer is the
		// one the check gives for a slug no workspace has.
		return server.NotFound(ErrNotFound.Error())
	}
	if err := s.pushRefusal(ctx, rec, name, force); err != nil {
		return err
	}
	repo, err := s.deps.Git.Open(ctx, s.RepoPath(slug))
	if err != nil {
		return err
	}
	head, found, err := repo.ResolveBranch(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return server.NewError("branch_not_found", http.StatusNotFound,
			fmt.Sprintf("workspace %q has no branch %q", slug, name))
	}
	cred, err := ResolveOriginCredential(ctx, s.deps.Secrets, slug)
	if err != nil {
		return err
	}
	var lease string
	var recorded bool
	if force {
		// The lease is the origin head the hub last saw, so that a forced
		// push replaces only commits the hub has seen; with none recorded
		// origin must not have the branch.
		if lease, recorded, err = repo.OriginHead(ctx, name); err != nil {
			return err
		}
	}
	originURL := deref(rec.GitURL)
	result, err := s.deps.Pusher.Push(ctx, gitrepo.PushRequest{
		Dir: repo.Dir(), URL: originURL, Credential: cred, Commit: head, Branch: name, Force: force, Lease: lease,
	})
	if errors.Is(err, gitrepo.ErrNonFastForward) {
		return nonFastForward(ctx, repo, name, head)
	}
	if errors.Is(err, gitrepo.ErrOriginMoved) {
		return originMoved(name, lease, recorded)
	}
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, pushAnswer{
		Branch: name, SHA: head, Remote: remoteOrigin, RemoteURL: withoutUserinfo(originURL), Result: result,
	})
	return nil
}

// pushRefusal is nil when the hub may hand a push of branch of w, forced
// when force is set, to origin, and otherwise the refusal, the first
// failing check in this order: the workspace's state as RequireReady judges
// it, a task fork, which has no origin, the integration branch or a patch
// branch of a carry-patch workspace, which only the feature that rebuilds or
// keeps them pushes, then a forced push of a protected branch. None of them
// depends on the branch existing, and none contacts origin.
func (s *Service) pushRefusal(ctx context.Context, w Workspace, branch string, force bool) error {
	if err := RequireReady(w); err != nil {
		return err
	}
	if w.Kind == access.KindFork {
		return server.NewError("fork_has_no_origin", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is a task fork, which has no origin to push to", w.Slug))
	}
	if w.Mode == ModeCarryPatch {
		if b := deref(w.IntegrationBranch); b != "" && b == branch {
			return server.NewError("integration_branch", http.StatusForbidden, fmt.Sprintf(
				"branch %q is the integration branch of workspace %q, which only its rebuild pushes to origin", branch, w.Slug))
		}
		patch, err := s.isPatchBranch(ctx, w.Slug, branch)
		if err != nil {
			return err
		}
		if patch {
			return server.NewError("patch_branch", http.StatusForbidden, fmt.Sprintf(
				"branch %q is a patch branch of workspace %q, which only its patch list pushes to origin", branch, w.Slug))
		}
	}
	if force {
		globs, _ := s.settings.EffectiveSettings(w.Settings, w.DefaultBranch)["protected_branches"].([]string)
		if MatchAnyBranchGlob(globs, branch) {
			return server.NewError("protected_branch", http.StatusForbidden, fmt.Sprintf(
				"branch %q of workspace %q is protected and cannot be replaced by a forced push", branch, w.Slug))
		}
	}
	return nil
}

// isPatchBranch reports whether branch is a patch branch of the carry-patch
// workspace slug, as the registered patch-branch lookup says; with none
// registered no branch is. A lookup that fails is 503 unavailable, so that
// a patch branch never passes unchecked and the caller may try again; what
// it said is logged scrubbed, not answered, as it may hold what it read.
func (s *Service) isPatchBranch(ctx context.Context, slug, branch string) (bool, error) {
	s.patchMu.RLock()
	l := s.patchLookup
	s.patchMu.RUnlock()
	if l == nil {
		return false, nil
	}
	patch, err := l.IsPatchBranch(ctx, slug, branch)
	if err != nil {
		s.deps.Logger.Error("workspace patch-branch lookup failed", "slug", slug, "branch", branch,
			"error", s.deps.Scrubber.Scrub(err.Error()))
		return false, server.Unavailable(fmt.Sprintf(
			"the patch list of workspace %q cannot be read; try again later", slug))
	}
	return patch, nil
}

// pushForce is the force the body of r asks for: true only for a JSON
// object whose member force is true; false for no body, an empty one and an
// object without force. Members other than force are ignored. A body that
// is not a JSON object, or whose force is not a JSON boolean, null
// included, is 400 invalid_request.
func pushForce(r *http.Request) (bool, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return false, server.InvalidRequest("request body could not be read")
	}
	if len(raw) == 0 {
		return false, nil
	}
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return false, err
	}
	member, given := body["force"]
	if !given {
		return false, nil
	}
	var force *bool
	if err := json.Unmarshal(member, &force); err != nil || force == nil {
		return false, server.InvalidRequest("request body member \"force\" must be true or false")
	}
	return *force, nil
}

// nonFastForward is the refusal of a push of head to branch that origin did
// not take because its branch is not an ancestor of head. It names the
// origin head the hub last saw for branch, when it keeps one; the hub asks
// origin nothing more to learn the current one.
func nonFastForward(ctx context.Context, repo *gitrepo.Repo, branch, head string) error {
	seen, found, err := repo.OriginHead(ctx, branch)
	if err != nil {
		return err
	}
	last := "the hub has no record of origin's head of it"
	if found {
		last = "the origin head the hub last saw is " + seen
	}
	return server.NewError("non_fast_forward", http.StatusConflict, fmt.Sprintf(
		"origin's branch %q is not an ancestor of the hub's head %s (%s); sync the workspace to bring in origin's commits, or push with force to replace origin's branch",
		branch, head, last))
}

// originMoved is the refusal of a forced push of branch under lease that
// origin did not take because its branch is no longer in the state the
// lease names; recorded tells whether the lease is a recorded origin head
// or the statement that origin lacks the branch. Origin's branch and the
// recorded origin head are left as they are: a sync brings in what origin
// has, after which the push may be tried again.
func originMoved(branch, lease string, recorded bool) error {
	last := "the hub has no record of origin's head of it, so origin must not have it"
	if recorded {
		last = "the origin head the hub last saw is " + lease
	}
	return server.NewError("origin_moved", http.StatusConflict, fmt.Sprintf(
		"origin's branch %q changed since the hub last saw it (%s); sync the workspace to bring in origin's branch, then push again",
		branch, last))
}

// withoutUserinfo is the origin URL raw with any user information removed.
// A value that is not a URL with a host, such as the git@host:path form, is
// given as it is: its git@ is that form's fixed login, not a credential.
func withoutUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	return u.String()
}
