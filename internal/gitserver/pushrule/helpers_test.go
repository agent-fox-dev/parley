package pushrule_test

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// repo is a bare repository holding main, dev, feat and patch/a at head,
// whose parent is base, and side, a child of base that head does not reach.
type repo struct {
	dir              string
	view             *gitrepo.Repo
	base, head, side string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	stream := "commit refs/heads/main\nmark :1\ncommitter P <p@example.invalid> now\ndata 5\nbase\nM 100644 inline README\ndata 2\n1\n" +
		"commit refs/heads/main\nmark :2\ncommitter P <p@example.invalid> now\ndata 5\nhead\nfrom :1\nM 100644 inline README\ndata 2\n2\n" +
		"commit refs/heads/side\nmark :3\ncommitter P <p@example.invalid> now\ndata 5\nside\nfrom :1\nM 100644 inline src/a.go\ndata 2\n3\n" +
		"reset refs/heads/dev\nfrom :2\n\nreset refs/heads/feat\nfrom :2\n\nreset refs/heads/patch/a\nfrom :2\n\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	view, err := gitrepo.Git{}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	heads := gitrepotest.Refs(t, dir, "refs/heads/")
	base := gitrepotest.Git(t, dir, "rev-parse", "main~1")
	return &repo{dir: dir, view: view, base: base, head: heads["main"], side: heads["side"]}
}

// The callers of the tests, by the credential they push with.
var (
	adminToken = pushrule.Caller{Level: access.LevelAdmin, CredentialType: pushrule.CredentialAdminToken}
	ownerKey   = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialAPIKey, CredentialID: "key-owner", UserID: "u-owner"}
	ownerPAT   = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-owner", UserID: "u-owner"}
	boundPAT   = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-bound", UserID: "u-owner", Bound: true}
)

// The workspaces of the tests: a registered repository and a task fork of it.
var (
	demo   = pushrule.Workspace{Slug: "demo", Kind: access.KindRepository, DefaultBranch: "main", Mode: workspace.ModeStandard}
	demoFk = pushrule.Workspace{Slug: "demo--k3x9w2ab", Kind: access.KindFork, Parent: "demo", DefaultBranch: "main", Mode: workspace.ModeStandard}
)

// settings answers every workspace with its push settings, or with err.
type settings struct {
	pushrule.Settings
	err error
}

func (s settings) PushSettings(context.Context, pushrule.Workspace) (pushrule.Settings, error) {
	return s.Settings, s.err
}

// protecting is a settings reader whose protected_branches are globs and
// which allows direct pushes.
func protecting(globs ...string) settings {
	return settings{Settings: pushrule.Settings{ProtectedBranches: append([]string{}, globs...), OwnerPush: true}}
}

// tracking reports branches as the tracking branches of every fork slug it
// is asked about, and records each question.
type tracking struct {
	branches []string
	err      error

	mu    sync.Mutex
	asked []string
}

func (s *tracking) IsTracking(_ context.Context, slug, branch string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, slug+":"+branch)
	return slices.Contains(s.branches, branch), s.err
}

func engine(deps pushrule.EngineDeps) *pushrule.Engine {
	var e pushrule.Engine
	e.Bind(deps)
	return &e
}

func create(ref, after string) pushrule.Update {
	return pushrule.Update{Ref: ref, Before: pushrule.ZeroID, After: after}
}

func move(ref, before, after string) pushrule.Update {
	return pushrule.Update{Ref: ref, Before: before, After: after}
}

func remove(ref, before string) pushrule.Update {
	return pushrule.Update{Ref: ref, Before: before, After: pushrule.ZeroID}
}

// outcome is what a decision says, without the update it is about.
type outcome struct {
	Allowed, Created, Deleted, Forced, Direct bool
	Message, ErrorType, Path                  string
}

func outcomeOf(d pushrule.Decision) outcome {
	return outcome{d.Allowed, d.Created, d.Deleted, d.Forced, d.Direct, d.Message, d.ErrorType, d.Path}
}

func refused(message, errorType string) outcome {
	return outcome{Message: message, ErrorType: errorType}
}

// decide evaluates the one update u for caller on ws and returns what the
// engine decided about it.
func decide(t *testing.T, e *pushrule.Engine, r *repo, ws pushrule.Workspace, caller pushrule.Caller, u pushrule.Update) outcome {
	t.Helper()
	v := e.Evaluate(context.Background(), pushrule.Request{Workspace: ws, Caller: caller, Updates: []pushrule.Update{u}, Repo: r.view})
	if len(v.Decisions) != 1 || v.Decisions[0].Update != u {
		t.Fatalf("decisions for %+v = %+v, want one about the update", u, v.Decisions)
	}
	return outcomeOf(v.Decisions[0])
}
