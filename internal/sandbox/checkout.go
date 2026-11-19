package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Restorer restores a checkpoint into a new sandbox's working directory.
// The lifecycle feature registers it on the service; without one a sandbox
// created from a checkpoint fails to provision.
type Restorer interface {
	// Restore restores the latest checkpoint of the sandbox source into the
	// working directory of the sandbox rec, after its checkout and before
	// its main process starts.
	Restore(ctx context.Context, rec, source Record) error
}

// RegisterRestorer makes r the restorer of every provisioning; nil removes
// it. It is safe to call while the hub serves; a provisioning takes the
// restorer once.
func (s *Service) RegisterRestorer(r Restorer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restorer = r
}

// errNoRestorer fails the provisioning of a sandbox created from a
// checkpoint while no restorer is registered.
var errNoRestorer = errors.New("no restorer is registered")

// credentialHelper is the git credential helper of a checkout: asked for a
// credential, it answers the user name x-token-auth and, as the password,
// what PARLEY_API_KEY holds in the sandbox, the hub credential's
// placeholder the runtime replaces; it stores and erases nothing. It names
// the variable, so that no command carries the credential.
const credentialHelper = `!f() { test "$1" = get || exit 0; echo username=x-token-auth; echo "password=$` + HubCredentialVariable + `"; }; f`

// commitID is what git rev-parse HEAD prints: a SHA-1 or SHA-256 object id.
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// prepare readies the working directory before the main process starts:
// the checkout of the workspace, when the sandbox has one, then the restore
// of the checkpoint the sandbox was created from, when it was.
func (p *provisioning) prepare(ctx context.Context) error {
	if p.rec.Checkout != nil {
		if err := p.checkout(ctx); err != nil {
			return err
		}
	}
	if p.rec.RestoredFrom != nil {
		return p.restore(ctx)
	}
	return nil
}

// checkout checks the workspace out into the working directory with git
// commands run in the sandbox: a credential helper for the hub's scheme and
// host, a partial clone of the workspace's git URL without blobs and
// without checking out files, the checkout (checkoutCommand), the creating
// user as the clone's commit identity, then reading HEAD, which becomes the
// checkout's sha. A command that exits with another code than 0, or cannot
// be run, fails the step with ReasonCheckoutFailed.
func (p *provisioning) checkout(ctx context.Context) error {
	s, rec := p.s, p.rec
	u, err := url.Parse(s.deps.ExternalURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return failed(ReasonProvisionFailed, "prepare the checkout", fmt.Errorf("[server].external_url %q is not a URL", s.deps.ExternalURL))
	}
	remote, err := s.remoteOf(ctx, rec)
	if err != nil {
		return failed(ReasonProvisionFailed, "prepare the checkout", err)
	}
	checkout, err := s.checkoutCommand(ctx, rec)
	if err != nil {
		return failed(ReasonProvisionFailed, "prepare the checkout", err)
	}
	commitAs, err := s.commitIdentity(ctx, rec)
	if err != nil {
		return failed(ReasonProvisionFailed, "prepare the checkout", err)
	}
	commands := append([][]string{
		{"git", "config", "--global", "credential." + u.Scheme + "://" + u.Host + ".helper", credentialHelper},
		{"git", "clone", "--filter=blob:none", "--no-checkout", remote, WorkDir},
		checkout,
	}, commitAs...)
	for _, argv := range commands {
		if _, err := p.git(ctx, argv); err != nil {
			return err
		}
	}
	out, err := p.git(ctx, []string{"git", "rev-parse", "HEAD"})
	if err != nil {
		return err
	}
	head := strings.TrimSpace(out)
	if !commitID.MatchString(head) {
		return &stepError{reason: ReasonCheckoutFailed, err: fmt.Errorf("git rev-parse HEAD printed %q, not a commit id", head)}
	}
	c := *rec.Checkout
	c.SHA = head
	p.rec.Checkout = &c
	return nil
}

// git runs the git command argv in the sandbox's working directory and
// returns what it printed on stdout. An exec error, or an exit code other
// than 0, is a step failed with ReasonCheckoutFailed, whose text holds the
// command's stderr; the failure scrubs it.
func (p *provisioning) git(ctx context.Context, argv []string) (string, error) {
	what := strings.Join(argv[:2], " ")
	res, err := p.client.Exec(ctx, p.rec.ID, runtime.ExecRequest{Command: argv, WorkDir: WorkDir})
	if err != nil {
		return "", failed(ReasonCheckoutFailed, what, err)
	}
	if res.ExitCode != 0 {
		return "", &stepError{reason: ReasonCheckoutFailed, err: fmt.Errorf("%s exited with %d: %s", what, res.ExitCode, strings.TrimSpace(string(res.Stderr)))}
	}
	return string(res.Stdout), nil
}

// checkoutCommand is the git command that checks out what the sandbox rec
// asks for in the clone. On a fork whose own branches hold the work branch
// already, that branch as it is, tracking the remote's; on a fork without
// it, the work branch created at the starting point, the remote's
// checkout.ref, and checked out; on a repository, checkout.ref itself,
// detached, so that no branch is created. Whether the fork has the branch
// is read in the hub's copy of the fork, which the clone's remote serves.
// The commands end their options with --, so that no name is read as one.
func (s *Service) checkoutCommand(ctx context.Context, rec Record) ([]string, error) {
	w, found, err := workspace.BySlug(ctx, s.deps.Store.Main(), rec.Workspace)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, fmt.Errorf("workspace %s: %w", rec.Workspace, workspace.ErrNotFound)
	}
	c := rec.Checkout
	start, err := s.startingPoint(ctx, w, c.Ref)
	if err != nil {
		return nil, err
	}
	if c.Branch == nil {
		return []string{"git", "checkout", "--detach", start, "--"}, nil
	}
	has, err := s.forkHasBranch(ctx, w, *c.Branch)
	if err != nil {
		return nil, err
	}
	if has {
		// git checks a branch only the remote has out as a new branch of
		// that name tracking the remote's.
		return []string{"git", "checkout", *c.Branch, "--"}, nil
	}
	return []string{"git", "checkout", "--no-track", "-b", *c.Branch, start, "--"}, nil
}

// startingPoint is the name of ref in a clone of the workspace w: the
// remote-tracking branch origin/<ref> for a branch the remote serves (on a
// fork also each branch it tracks of its parent), ref itself for a tag or a
// commit id, which the clone holds under the same name.
func (s *Service) startingPoint(ctx context.Context, w workspace.Workspace, ref string) (string, error) {
	if w.Kind == access.KindFork {
		tracking, err := s.trackingOf(ctx, s.deps.Store.Main(), w)
		if err != nil {
			return "", err
		}
		if slices.Contains(tracking.branches, ref) {
			return "origin/" + ref, nil
		}
	}
	repo, err := s.deps.Repos(ctx, w.Slug)
	if err != nil {
		return "", err
	}
	_, found, err := repo.ResolveBranch(ctx, ref)
	if err != nil {
		return "", err
	}
	if found {
		return "origin/" + ref, nil
	}
	return ref, nil
}

// forkHasBranch reports whether the fork w has a branch of its own named
// branch, which the create took as the work branch: one it tracks of its
// parent is not the fork's.
func (s *Service) forkHasBranch(ctx context.Context, w workspace.Workspace, branch string) (bool, error) {
	tracking, err := s.trackingOf(ctx, s.deps.Store.Main(), w)
	if err != nil {
		return false, err
	}
	if slices.Contains(tracking.branches, branch) {
		return false, nil
	}
	repo, err := s.deps.Repos(ctx, w.Slug)
	if err != nil {
		return false, err
	}
	_, found, err := repo.ResolveBranch(ctx, branch)
	return found, err
}

// commitIdentity are the commands that make the creating user of rec the
// commit identity of the clone: user.name, the user's full name or else the
// user name, and user.email when the user has an e-mail address.
func (s *Service) commitIdentity(ctx context.Context, rec Record) ([][]string, error) {
	if s.deps.User == nil {
		return nil, errors.New("the sandbox service cannot read the creating user")
	}
	u, err := s.deps.User(ctx, rec.UserID)
	if err != nil {
		return nil, fmt.Errorf("the creating user %s: %w", rec.UserID, err)
	}
	name := u.FullName
	if name == "" {
		name = u.Username
	}
	out := [][]string{{"git", "config", "--", "user.name", name}}
	if u.Email != "" {
		out = append(out, []string{"git", "config", "--", "user.email", u.Email})
	}
	return out, nil
}

// restore has the registered restorer restore the checkpoint of the sandbox
// rec was created from. Without a restorer, or when the restorer or the
// source's record fails, the step fails with ReasonProvisionFailed.
func (p *provisioning) restore(ctx context.Context) error {
	s, rec := p.s, p.rec
	s.mu.Lock()
	r := s.restorer
	s.mu.Unlock()
	if r == nil {
		return failed(ReasonProvisionFailed, "restore the checkpoint of "+*rec.RestoredFrom, errNoRestorer)
	}
	source, err := Get(ctx, s.deps.Store.Main(), *rec.RestoredFrom)
	if err != nil {
		return failed(ReasonProvisionFailed, "restore the checkpoint of "+*rec.RestoredFrom, err)
	}
	if err := r.Restore(ctx, rec, source); err != nil {
		return failed(ReasonProvisionFailed, "restore the checkpoint of "+*rec.RestoredFrom, err)
	}
	return nil
}

// storeCheckout writes the checkout of rec, with the sha the checkout read,
// in tx; a sandbox without a checkout has nothing to write.
func storeCheckout(ctx context.Context, tx *sql.Tx, rec Record) error {
	if rec.Checkout == nil {
		return nil
	}
	return Update(ctx, tx, rec.ID, SetCheckout(rec.Checkout))
}
