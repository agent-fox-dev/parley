package workspace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
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

// pushDeadline is how long a push may take before git is killed with every
// process it started and the push is answered 504 origin_timeout.
const pushDeadline = 2 * time.Minute

// remoteOrigin is the name a push answer gives the remote it pushed to.
const remoteOrigin = "origin"

// originHeadRef is the prefix of the ref of the hub's copy that records a
// branch's origin head.
const originHeadRef = "refs/remotes/origin/"

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
// workspace's origin, as pushBranch describes it. Every message it answers
// is scrubbed, with the value of the origin credential resolved for the
// push as a secret once there is one: origin's own text may repeat the
// credential or the URL it was called with. The scrubber also cuts the
// message to the platform's limit on a character boundary.
func (s *Service) servePush(w http.ResponseWriter, r *http.Request) error {
	var cred Credential
	err := s.pushBranch(w, r, &cred)
	var answer *server.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &answer):
		return server.NewError(answer.Type(), answer.Status(), s.deps.Scrubber.Scrub(answer.Message(), cred.Password))
	}
	// The router answers it 500 internal and writes its text only to the log.
	return errors.New(s.deps.Scrubber.Scrub(err.Error(), cred.Password))
}

// pushBranch pushes the route's branch of the route's workspace to the
// workspace's origin. It reads the branch's head once and pushes that
// commit, so that a branch that moves meanwhile changes neither what is
// pushed nor what is answered; it takes no lock on the branch, and holds
// the workspace's work lock shared (see admitWork). The origin credential is
// resolved from the workspace's secrets for this push, and *resolved set to
// it. Every push handed to origin is recorded as one push event, and one
// origin took also as the branch's origin head; a refusal before the push
// records nothing.
func (s *Service) pushBranch(w http.ResponseWriter, r *http.Request, resolved *Credential) error {
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
	rec, release, err := s.admitWork(ctx, slug, func(w Workspace) error { return s.pushRefusal(ctx, w, name, force) })
	if err != nil {
		return err
	}
	defer release()
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
	*resolved = cred
	// The caller is the push event's actor; the access check has let only
	// the admin token or a user's credential through.
	p, _ := identity.PrincipalOf(ctx)
	actorType, actorID, ok := eventActor(p)
	if !ok {
		return fmt.Errorf("push branch %q of workspace %q: the push names no caller", name, slug)
	}
	// From here on a caller who disconnects changes neither the push, which
	// only its deadline ends, nor its records.
	ctx = context.WithoutCancel(ctx)
	// The origin head the hub last saw is the lease of a forced push, so
	// that a forced push replaces only commits the hub has seen (with none
	// recorded origin must not have the branch), and the value the record
	// is put back to when the records of a push origin took fail.
	seen, recorded, err := repo.OriginHead(ctx, name)
	if err != nil {
		return err
	}
	originURL := deref(rec.GitURL)
	scrub := func(text string) string { return s.deps.Scrubber.Scrub(text, cred.Password) }
	// One value for the answer and the event: the stored URL may hold a
	// secret outside its user information too.
	op := pushOp{slug: slug, branch: name, sha: head, force: force, remoteURL: scrub(withoutUserinfo(originURL)),
		actorType: actorType, actorID: actorID, scrub: scrub}
	pushCtx, cancel := context.WithTimeout(ctx, s.deps.PushDeadline)
	defer cancel()
	result, err := s.deps.Pusher.Push(pushCtx, gitrepo.PushRequest{
		Dir: repo.Dir(), URL: originURL, Credential: cred, Commit: head, Branch: name, Force: force, Lease: seen,
	})
	if err != nil {
		var answer *server.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			// The push operation killed git with every process it started
			// when the deadline passed, and waited for them. A git that
			// ended on its own as the deadline passed is answered by what
			// it reported.
			answer = server.NewError("origin_timeout", http.StatusGatewayTimeout, fmt.Sprintf(
				"the push of branch %q to origin did not finish within %v", name, s.deps.PushDeadline))
		case errors.Is(err, gitrepo.ErrNonFastForward):
			answer = nonFastForward(name, head, seen, recorded)
		case errors.Is(err, gitrepo.ErrOriginMoved):
			answer = originMoved(name, seen, recorded)
		default:
			answer = pushFailure(err, slug, name, op.remoteURL, cred)
		}
		if err := s.writePushRecords(ctx, op.event("", answer.Type()), nil); err != nil {
			return err
		}
		return answer
	}
	if err := s.recordPushed(ctx, repo, rec, op, result, seen, recorded); err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, pushAnswer{
		Branch: name, SHA: head, Remote: remoteOrigin, RemoteURL: op.remoteURL, Result: result,
	})
	return nil
}

// eventPush is the event every push handed to origin writes.
const eventPush = "parley.workspace.push"

// pushOp is a push handed to origin as its event names it: the workspace,
// the branch, the hub's head offered, whether it was forced, origin's URL
// without user information, scrubbed, and the caller, and scrub, which
// cleans a text of the push's origin credential.
type pushOp struct {
	slug, branch, sha string
	force             bool
	remoteURL         string
	actorType         string
	actorID           *string
	scrub             func(string) string
}

// pushEventMeta is the metadata of a push event: result for a push origin
// took, error_type, the answer's, for one that failed, never both.
type pushEventMeta struct {
	Branch    string             `json:"branch"`
	SHA       string             `json:"sha"`
	Force     bool               `json:"force"`
	OK        bool               `json:"ok"`
	RemoteURL string             `json:"remote_url"`
	Result    gitrepo.PushResult `json:"result,omitempty"`
	ErrorType string             `json:"error_type,omitempty"`
}

// event is the push event of op, which ended with result when origin took
// it and with the answer of type errorType otherwise. Every text a push
// event holds is put in it here, and the texts that come from the caller or
// the workspace's record, the branch and origin's URL (scrubbed in op
// already), are scrubbed; the others are the hub's own words and ids.
func (op pushOp) event(result gitrepo.PushResult, errorType string) event.Input {
	slug, branch := op.slug, op.scrub(op.branch)
	return event.Input{Type: eventPush, Workspace: &slug, ActorType: op.actorType, ActorID: op.actorID,
		ResourceType: "branch", ResourceID: branch, Metadata: pushEventMeta{
			Branch: branch, SHA: op.sha, Force: op.force, OK: errorType == "", RemoteURL: op.remoteURL,
			Result: result, ErrorType: errorType,
		}}
}

// recordPushed records that origin took op with result: the branch's
// origin head, the ref refs/remotes/origin/<branch> of the hub's copy, set
// to the pushed commit, then in one store transaction the push event and,
// for w's default branch, w's origin_head_sha. A ref cannot be part of a
// store transaction, so when the transaction fails the ref is put back to
// seen, the value it had, in one step and only while it still holds the
// pushed commit: a sync that moved it meanwhile recorded what origin has
// since, which is kept. When recorded is false the ref is removed. The push
// is answered as a failure inside the hub; origin keeps the commit, and the
// next sync or push records it.
func (s *Service) recordPushed(ctx context.Context, repo *gitrepo.Repo, w Workspace, op pushOp, result gitrepo.PushResult, seen string, recorded bool) error {
	if err := repo.SetOriginHead(ctx, op.branch, op.sha); err != nil {
		return fmt.Errorf("record the origin head of branch %q of workspace %q: %w", op.branch, op.slug, err)
	}
	var originHead *string
	if op.branch == deref(w.DefaultBranch) {
		originHead = &op.sha
	}
	err := s.writePushRecords(ctx, op.event(result, ""), originHead)
	if err == nil {
		return nil
	}
	var restore error
	if recorded {
		restore = repo.CompareAndSwapRef(ctx, originHeadRef+op.branch, seen, op.sha)
	} else {
		restore = repo.DeleteOriginHead(ctx, op.branch)
	}
	if restore != nil && !errors.Is(restore, gitrepo.ErrRefMoved) {
		s.deps.Logger.Error("workspace origin head not put back after a push", "slug", op.slug, "branch", op.branch,
			"error", restore)
	}
	return err
}

// writePushRecords writes the push event in and, when originHead is not
// nil, the origin_head_sha of in's workspace, in one store transaction.
func (s *Service) writePushRecords(ctx context.Context, in event.Input, originHead *string) error {
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if originHead != nil {
			if err := recordOriginHead(ctx, tx, *in.Workspace, *originHead); err != nil {
				return err
			}
		}
		_, err := s.deps.Events.Emit(ctx, tx, in)
		return err
	})
	if err != nil {
		return fmt.Errorf("record the push of branch %q of workspace %q: %w", in.ResourceID, *in.Workspace, err)
	}
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
	// DecodeJSON is handed the bytes read here, not a body already drained.
	r.Body = io.NopCloser(bytes.NewReader(raw))
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
// not take because its branch is not an ancestor of head. It names seen,
// the origin head the hub last saw for branch, when recorded says it keeps
// one; the hub asks origin nothing more to learn the current one.
func nonFastForward(branch, head, seen string, recorded bool) *server.Error {
	last := "the hub has no record of origin's head of it"
	if recorded {
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
func originMoved(branch, lease string, recorded bool) *server.Error {
	last := "the hub has no record of origin's head of it, so origin must not have it"
	if recorded {
		last = "the origin head the hub last saw is " + lease
	}
	return server.NewError("origin_moved", http.StatusConflict, fmt.Sprintf(
		"origin's branch %q changed since the hub last saw it (%s); sync the workspace to bring in origin's branch, then push again",
		branch, last))
}

// pushFailure is the answer to the failure err of a push of branch of the
// workspace slug to its origin at remoteURL, cred being the credential the
// push presented: 422 origin_rejected with origin's reason when origin
// refused the update itself, otherwise 502 with the type of the failure's
// class, and 502 upstream_error for a failure of no known class.
func pushFailure(err error, slug, branch, remoteURL string, cred Credential) *server.Error {
	var pe *gitrepo.PushError
	if !errors.As(err, &pe) {
		return server.UpstreamError(fmt.Sprintf("the push of branch %q to origin at %s failed", branch, remoteURL))
	}
	switch {
	case errors.Is(pe.Class, gitrepo.ErrOriginRejected):
		return server.NewError("origin_rejected", http.StatusUnprocessableEntity, fmt.Sprintf(
			"origin refused the update of branch %q: %s", branch, pe.Reason))
	case errors.Is(pe.Class, gitrepo.ErrUnauthenticated) && cred.IsZero():
		return server.NewError("origin_unauthenticated", http.StatusBadGateway, fmt.Sprintf(
			"origin at %s asked for a credential and workspace %q has no credential; store GIT_PAT, or GIT_USERNAME and GIT_PASSWORD, as its secrets",
			remoteURL, slug))
	case errors.Is(pe.Class, gitrepo.ErrUnauthenticated):
		return server.NewError("origin_unauthenticated", http.StatusBadGateway, fmt.Sprintf(
			"origin at %s rejected the stored credential of workspace %q", remoteURL, slug))
	case errors.Is(pe.Class, gitrepo.ErrForbidden):
		return server.NewError("origin_forbidden", http.StatusBadGateway, fmt.Sprintf(
			"origin at %s does not let the credential of workspace %q push to the repository: %s", remoteURL, slug, pe.Reason))
	case errors.Is(pe.Class, gitrepo.ErrRepositoryNotFound):
		return server.NewError("origin_not_found", http.StatusBadGateway, fmt.Sprintf(
			"origin has no repository at %s, or the credential of workspace %q cannot see it: %s", remoteURL, slug, pe.Reason))
	case errors.Is(pe.Class, gitrepo.ErrUnreachable):
		return server.NewError("origin_unreachable", http.StatusBadGateway, fmt.Sprintf(
			"origin at %s cannot be reached: %s", remoteURL, pe.Reason))
	}
	return server.UpstreamError(fmt.Sprintf("the push of branch %q to origin at %s failed: %s", branch, remoteURL, pe.Reason))
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
