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
	repo, err := s.deps.Git.Open(ctx, s.RepoPath(slug))
	if err != nil {
		return err
	}
	head, found, err := repo.ResolveBranch(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("push workspace %q: no branch %q", slug, name)
	}
	cred, err := ResolveOriginCredential(ctx, s.deps.Secrets, slug)
	if err != nil {
		return err
	}
	originURL := deref(rec.GitURL)
	result, err := s.deps.Pusher.Push(ctx, gitrepo.PushRequest{
		Dir: repo.Dir(), URL: originURL, Credential: cred, Commit: head, Branch: name, Force: force,
	})
	if errors.Is(err, gitrepo.ErrNonFastForward) {
		return nonFastForward(ctx, repo, name, head)
	}
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, pushAnswer{
		Branch: name, SHA: head, Remote: remoteOrigin, RemoteURL: withoutUserinfo(originURL), Result: result,
	})
	return nil
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
