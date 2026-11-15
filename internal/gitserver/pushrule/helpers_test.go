package pushrule_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/store"
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

// commit writes, in the repository at dir, a commit of parents (none for a
// root commit) whose tree is the first parent's changed by ops, fast-import
// file commands in which "M <path>" writes a file holding its path, and
// returns its id, which no ref reaches.
func commit(t *testing.T, dir string, parents []string, ops ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("commit refs/new\ncommitter P <p@example.invalid> now\ndata 0\n")
	for i, p := range parents {
		b.WriteString([]string{"from ", "merge "}[min(i, 1)] + p + "\n")
	}
	for _, op := range ops {
		if path, ok := strings.CutPrefix(op, "M "); ok {
			op = fmt.Sprintf("M 100644 inline %s\ndata %d\n%s", path, len(path), path)
		}
		b.WriteString(op + "\n")
	}
	return unreferenced(t, dir, "refs/new", b.String())
}

// unreferenced runs the fast-import stream, which writes ref, in the
// repository at dir, and returns what ref pointed at once it removed ref.
func unreferenced(t *testing.T, dir, ref, stream string) string {
	t.Helper()
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	id := gitrepotest.Git(t, dir, "rev-parse", ref)
	gitrepotest.Git(t, dir, "update-ref", "-d", ref)
	return id
}

// The callers of the tests, by the credential they push with.
var (
	adminToken = pushrule.Caller{Level: access.LevelAdmin, CredentialType: pushrule.CredentialAdminToken}
	ownerKey   = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialAPIKey, CredentialID: "key-owner", UserID: "u-owner"}
	ownerPAT   = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-owner", UserID: "u-owner"}
	boundPAT   = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-bound", UserID: "u-owner", Bound: true}
	pathsPAT   = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-paths", UserID: "u-owner", Paths: []string{"**"}}
	docsPAT    = pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-docs", UserID: "u-owner", Paths: []string{"docs/**"}}
	memberKey  = pushrule.Caller{Level: access.LevelMember, CredentialType: pushrule.CredentialAPIKey, CredentialID: "key-member", UserID: "u-member"}
	memberPAT  = pushrule.Caller{Level: access.LevelMember, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-member", UserID: "u-member"}
)

// The workspaces of the tests: a registered repository and a task fork of it.
var (
	demo   = pushrule.Workspace{Slug: "demo", Kind: access.KindRepository, DefaultBranch: "main", Mode: workspace.ModeStandard}
	demoFk = pushrule.Workspace{Slug: "demo--k3x9w2ab", Kind: access.KindFork, Parent: "demo", DefaultBranch: "main", Mode: workspace.ModeStandard}
	demoCP = pushrule.Workspace{Slug: "demo", Kind: access.KindRepository, DefaultBranch: "main", Mode: workspace.ModeCarryPatch}
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

// controller is a carry-patch controller that answers each ref with its
// scripted answer, Allow for a ref it has none for, or fails with err, and
// records each question and each run of its forward.
type controller struct {
	answers map[string]pushrule.Answer
	err     error

	mu        sync.Mutex
	asked     []string
	forwarded int
}

func (c *controller) Decide(_ context.Context, ws pushrule.Workspace, caller pushrule.Caller, u pushrule.Update) (pushrule.Answer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, ws.Slug+":"+caller.CredentialID+":"+u.Ref)
	if c.err != nil {
		return pushrule.Answer{}, c.err
	}
	if a, ok := c.answers[u.Ref]; ok {
		return a, nil
	}
	return pushrule.Allow(), nil
}

func (c *controller) forward(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forwarded++
	return nil
}

func (c *controller) counts() (asked, forwarded int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.asked), c.forwarded
}

// errInjected is the failure a stub is told to answer with.
var errInjected = errors.New("injected failure")

// view is a repository view that fails the question named by fails
// ("IsAncestor", "NewCommits" or "ChangedPaths") and counts the questions
// about commits it is asked.
type view struct {
	pushrule.View
	fails string

	mu    sync.Mutex
	asked int
}

func (v *view) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if v.fails == "IsAncestor" {
		return false, errInjected
	}
	return v.View.IsAncestor(ctx, ancestor, descendant)
}

func (v *view) NewCommits(ctx context.Context, tips []string, parentDir string) ([]string, error) {
	if v.count("NewCommits") {
		return nil, errInjected
	}
	return v.View.NewCommits(ctx, tips, parentDir)
}

func (v *view) ChangedPaths(ctx context.Context, commit string) ([]string, error) {
	if v.count("ChangedPaths") {
		return nil, errInjected
	}
	return v.View.ChangedPaths(ctx, commit)
}

// count counts a question about commits and reports whether it fails.
func (v *view) count(question string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.asked++
	return v.fails == question
}

// versionFiles answers every question with files and found, or fails with
// err, and records each question.
type versionFiles struct {
	files []string
	found bool
	err   error
	asked []string
}

func (s *versionFiles) Files(_ context.Context, slug, branch, head string, opensChange bool) ([]string, bool, error) {
	s.asked = append(s.asked, fmt.Sprintf("%s %s %s %t", slug, branch, head, opensChange))
	return s.files, s.found, s.err
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

// allowed is the verdict that allows every update of us, as the engine
// would have, whatever the repository holds.
func allowed(us ...pushrule.Update) pushrule.Verdict {
	var v pushrule.Verdict
	for _, u := range us {
		v.Decisions = append(v.Decisions, pushrule.Decision{Update: u, Allowed: true, Created: u.Before == pushrule.ZeroID, Deleted: u.After == pushrule.ZeroID})
	}
	return v
}

// newStore opens a store in a temporary directory of t whose one table,
// stage_rows, is where the stages of the tests write.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces")},
		[]store.Step{{Name: "stage_rows", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE stage_rows (ref TEXT NOT NULL)`)
			return err
		}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// stageRows are the refs the stages wrote rows for and the store holds.
func stageRows(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Main().Query(`SELECT ref FROM stage_rows ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	return refs
}

// stage is a ref-updated stage that logs its name to log, writes a row of
// stage_rows for each update when write is set, and answers versions or
// err. It records what it was given, and what the repository at dir held
// when it ran: each update's ref, the value it pointed at and whether its
// after value could be read.
type stage struct {
	name     string
	log      *[]string
	dir      string
	write    bool
	versions []*pushrule.Version
	err      error

	inputs []pushrule.StageInput
	seen   []string
}

func (s *stage) run(ctx context.Context, tx *sql.Tx, in pushrule.StageInput) ([]*pushrule.Version, error) {
	*s.log = append(*s.log, s.name)
	s.inputs = append(s.inputs, in)
	for _, d := range in.Updates {
		at, _ := gitrepotest.Run(s.dir, "", "rev-parse", "--verify", "--quiet", d.Ref)
		_, unreadable := gitrepotest.Run(s.dir, "", "cat-file", "-e", d.After)
		s.seen = append(s.seen, fmt.Sprintf("%s=%.7s readable=%t", d.Ref, strings.TrimSpace(at), unreadable == nil))
		if s.write {
			if _, err := tx.ExecContext(ctx, `INSERT INTO stage_rows (ref) VALUES (?)`, d.Ref); err != nil {
				return nil, err
			}
		}
	}
	return s.versions, s.err
}

// reports records what the ref writer reports after a write: the fork
// activity of a slug, or the new head of a workspace as "slug id". The
// fork activity recorder fails with err.
type reports struct {
	err error
	got []string
}

func (r *reports) RecordActivity(_ context.Context, slug string) error {
	r.got = append(r.got, slug)
	return r.err
}

func (r *reports) SetHead(_ context.Context, slug, id string) error {
	r.got = append(r.got, slug+" "+id)
	return nil
}
