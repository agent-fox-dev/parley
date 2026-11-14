package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// history builds main (the default branch) with three commits, dev with one
// more on top of the second, and the tag v1 on main's first commit. It
// returns the heads of main and dev and the tag's commit.
func history(o *gitrepotest.Origin) (mainHead, devHead, v1 string) {
	v1 = o.Commit("main", "first")
	o.Branch("dev", o.Commit("main", "second"))
	devHead = o.Commit("dev", "on dev")
	mainHead = o.Commit("main", "third")
	o.Tag("v1", v1)
	return mainHead, devHead, v1
}

// createFrom creates the workspace slug of the user u whose origin is url,
// with members added to the body, and returns its record.
func (w *world) createFrom(t *testing.T, slug, url string, members map[string]any) workspace.Workspace {
	t.Helper()
	body := valid(slug)
	body["git_url"] = url
	for k, v := range members {
		body[k] = v
	}
	w.create(t, w.uKey, body).object(t)
	rec, _ := w.workspace(t, slug)
	return rec
}

// clone runs the clone work of slug and returns the record it leaves.
func (w *world) clone(t *testing.T, slug string) workspace.Workspace {
	t.Helper()
	if err := w.svc.Clone(context.Background(), slug); err != nil {
		t.Fatalf("clone work of %s: %v", slug, err)
	}
	rec, _ := w.workspace(t, slug)
	return rec
}

// leftovers are the entries of the repository root that belong to slug's
// copy or to a clone in progress.
func (w *world) leftovers(t *testing.T, slug string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(w.svc.RepoPath(slug)))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), slug) || strings.HasPrefix(e.Name(), ".clone-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// gitIn runs git on the repository dir and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

func TestClone_MarksTheWorkspaceCloningWithoutTheOldErrorBeforeAskingOrigin(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o))
	w.createFrom(t, "demo", o.URL(), nil)
	old := "old"
	if err := workspace.UpdateClone(context.Background(), w.Store.Main(), "demo",
		workspace.CloneFields{Status: workspace.StatusPending, CloneError: &old, UpdatedAt: w.clock()}); err != nil {
		t.Fatal(err)
	}
	o.Block()
	done := make(chan error, 1)
	go func() { done <- w.svc.Clone(context.Background(), "demo") }()
	for deadline := time.Now().Add(10 * time.Second); o.Requests() == 0; time.Sleep(5 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("clone work ended (%v) before it asked origin", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("origin was not asked within 10 s")
		}
	}
	if rec, _ := w.workspace(t, "demo"); rec.Status != workspace.StatusCloning || rec.CloneError != nil {
		t.Errorf("while origin holds the clone: status %s, clone_error %v; want cloning and null", rec.Status, rec.CloneError)
	}
	o.Release()
	if err := <-done; err != nil {
		t.Fatalf("clone work: %v", err)
	}
	if rec, _ := w.workspace(t, "demo"); rec.Status != workspace.StatusReady {
		t.Errorf("after the release: status %s, want ready", rec.Status)
	}
}

func TestClone_HoldsEveryBranchAndTagWithFullHistoryAndTheOriginHeads(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	mainHead, devHead, v1 := history(o)
	w := newWorld(t, withGitOrigin(o))
	for _, c := range []struct {
		slug, branch, head string
		members            map[string]any
	}{
		{"demo", "main", mainHead, nil},
		{"on-dev", "dev", devHead, map[string]any{"default_branch": "dev"}},
	} {
		w.createFrom(t, c.slug, o.URL(), c.members)
		events := w.eventCount(t)
		rec := w.clone(t, c.slug)
		if rec.Status != workspace.StatusReady || rec.DefaultBranch == nil || *rec.DefaultBranch != c.branch || rec.CloneError != nil {
			t.Errorf("%s: status %s, default_branch %v, clone_error %v; want ready, %s, null",
				c.slug, rec.Status, rec.DefaultBranch, rec.CloneError, c.branch)
		}
		if rec.HeadSHA == nil || *rec.HeadSHA != c.head || len(*rec.HeadSHA) != 40 ||
			rec.OriginHeadSHA == nil || *rec.OriginHeadSHA != *rec.HeadSHA {
			t.Errorf("%s: head_sha %v, origin_head_sha %v; want both %s", c.slug, rec.HeadSHA, rec.OriginHeadSHA, c.head)
		}
		dir := w.svc.RepoPath(c.slug)
		if dir != filepath.Join(filepath.Dir(dir), c.slug+".git") {
			t.Errorf("%s: the copy lies at %s, want <root>/%s.git", c.slug, dir, c.slug)
		}
		for _, q := range []struct{ args, want string }{
			{"rev-parse --is-bare-repository", "true"},
			{"rev-parse --is-shallow-repository", "false"},
			{"rev-list --count main", "3"},
			{"symbolic-ref HEAD", "refs/heads/" + c.branch},
			{"config --local parley.workspace", rec.InstanceID},
			{"for-each-ref --format=%(refname)=%(objectname)", strings.Join([]string{
				"refs/heads/dev=" + devHead, "refs/heads/main=" + mainHead,
				"refs/remotes/origin/dev=" + devHead, "refs/remotes/origin/main=" + mainHead,
				"refs/tags/v1=" + v1}, "\n")},
		} {
			if got := gitIn(t, dir, strings.Fields(q.args)...); got != q.want {
				t.Errorf("%s: git %s = %q, want %q", c.slug, q.args, got, q.want)
			}
		}
		repo, err := gitrepo.Git{}.Open(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		heads, err := repo.OriginHeads(context.Background())
		if err != nil || len(heads) != 2 || heads["main"] != mainHead || heads["dev"] != devHead {
			t.Errorf("%s: OriginHeads = %v, %v; want main %s and dev %s", c.slug, heads, err, mainHead, devHead)
		}
		if after := w.eventCount(t); after != events {
			t.Errorf("%s: the event log went from %d to %d events, want no event", c.slug, events, after)
		}
	}
}

func TestClone_FailsNamingAGivenDefaultBranchOriginDoesNotHave(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o))
	w.createFrom(t, "demo", o.URL(), map[string]any{"default_branch": "nope"})
	rec := w.clone(t, "demo")
	if rec.Status != workspace.StatusFailed || rec.CloneError == nil || !strings.Contains(*rec.CloneError, "nope") {
		t.Errorf("status %s, clone_error %v; want failed naming nope", rec.Status, rec.CloneError)
	}
	if rec.DefaultBranch == nil || *rec.DefaultBranch != "nope" || rec.HeadSHA != nil {
		t.Errorf("default_branch %v, head_sha %v; want nope and null", rec.DefaultBranch, rec.HeadSHA)
	}
	if left := w.leftovers(t, "demo"); len(left) != 0 {
		t.Errorf("the repository root holds %v, want nothing of the clone", left)
	}
}

func TestClone_FailsWithACleanBoundedErrorLeavingNothingAndTryingNoMore(t *testing.T) {
	t.Parallel()
	trusted := gitrepotest.NewOrigin(t)
	w := newWorld(t, withGitOrigin(trusted))
	served := func(opts ...gitrepotest.Option) *gitrepotest.Origin {
		o := gitrepotest.NewOrigin(t, opts...)
		history(o)
		return o
	}
	type outcome struct {
		slug, msg string
		origin    *gitrepotest.Origin
		asked     int
	}
	var failed []outcome
	prefix := "fatal: tok-123 was refused by https://u:pw@host/x "
	long := prefix + strings.Repeat("x", 5000-len(prefix))
	for i, c := range []struct {
		name string
		// origin builds the workspace's origin.
		origin func() *gitrepotest.Origin
		// gitErr is what the hub's git answers a clone with, nil to run git.
		gitErr error
		// unreadable makes the secret store fail every read.
		unreadable bool
		// want is the whole clone_error, "" for any.
		want string
	}{
		{name: "a closed port", origin: func() *gitrepotest.Origin { o := served(); o.Close(); return o }},
		{name: "an origin refusing the stored token with 401", origin: func() *gitrepotest.Origin {
			return served(gitrepotest.WithBasicAuth("x-token-auth", "tok-other"))
		}},
		{name: "an origin answering 403", origin: func() *gitrepotest.Origin { o := served(); o.Refuse(http.StatusForbidden); return o }},
		{name: "an empty repository", origin: func() *gitrepotest.Origin {
			return gitrepotest.NewOrigin(t, gitrepotest.WithSmartHTTP())
		}, want: "origin has no branches"},
		{name: "a git error of 5000 bytes holding the token and userinfo", origin: func() *gitrepotest.Origin { return served() }, gitErr: errors.New(long)},
		{name: "a credential that cannot be read", origin: func() *gitrepotest.Origin { return served() }, unreadable: true},
	} {
		slug := fmt.Sprintf("broken-%d", i)
		o := c.origin()
		w.createFrom(t, slug, o.URL(), nil)
		w.putSecret(t, slug, workspace.SecretGitPAT, "tok-123")
		w.git.failClone(c.gitErr)
		if c.unreadable {
			w.fail(faultSecretRead)
		}
		events, gitCalls := w.eventCount(t), w.git.invocations()
		rec := w.clone(t, slug)
		w.git.failClone(nil)
		w.fail("")
		if rec.Status != workspace.StatusFailed || rec.CloneError == nil || *rec.CloneError == "" {
			t.Errorf("%s: status %s, clone_error %v; want failed with an error", c.name, rec.Status, rec.CloneError)
			continue
		}
		msg := *rec.CloneError
		if len(msg) > 1024 || strings.Contains(msg, "tok-123") || strings.Contains(msg, "u:pw@") {
			t.Errorf("%s: clone_error of %d bytes holds a credential or userinfo: %s", c.name, len(msg), msg)
		}
		if c.want != "" && msg != c.want {
			t.Errorf("%s: clone_error = %q, want %q", c.name, msg, c.want)
		}
		if c.gitErr != nil && !strings.Contains(msg, "[redacted]") {
			t.Errorf("%s: clone_error = %q, want the token replaced by [redacted]", c.name, msg)
		}
		if c.unreadable && w.git.invocations() != gitCalls {
			t.Errorf("%s: git ran %d times, want never", c.name, w.git.invocations()-gitCalls)
		}
		if left := w.leftovers(t, slug); len(left) != 0 {
			t.Errorf("%s: the repository root holds %v, want nothing of the clone", c.name, left)
		}
		if after := w.eventCount(t); after != events {
			t.Errorf("%s: the event log went from %d to %d events, want no event", c.name, events, after)
		}
		asked := o.Requests()
		if again := w.clone(t, slug); again.Status != workspace.StatusFailed || o.Requests() != asked ||
			again.CloneError == nil || *again.CloneError != msg {
			t.Errorf("%s: clone work again left status %s and asked origin %d more times, want failed as it was and no request",
				c.name, again.Status, o.Requests()-asked)
		}
		failed = append(failed, outcome{slug, msg, o, o.Requests()})
	}
	// Three rounds of the workers over the failed workspaces start no clone.
	runs := &runLog{}
	wk := w.startWorkers(t, 1, runs.around(w.svc.Clone))
	for round := 1; round <= 3; round++ {
		for _, f := range failed {
			wk.Enqueue(f.slug)
		}
		eventually(t, fmt.Sprintf("worker round %d", round), func() bool {
			_, finished, _ := runs.snapshot()
			return len(finished) == round*len(failed)
		})
	}
	for _, f := range failed {
		if rec, _ := w.workspace(t, f.slug); rec.Status != workspace.StatusFailed || rec.CloneError == nil ||
			*rec.CloneError != f.msg || f.origin.Requests() != f.asked {
			t.Errorf("%s after three worker rounds: status %s, origin asked %d more times; want failed as it was and no request",
				f.slug, rec.Status, f.origin.Requests()-f.asked)
		}
	}
}

func TestClone_RunsTheModesStepOnceOnTheFreshCopyAndIsReadyWhenItFails(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o))
	type seen struct {
		status workspace.Status
		copied bool
	}
	var runs []seen
	w.svc.RegisterPostCloneStep(workspace.ModeCarryPatch, func(_ context.Context, ws workspace.Workspace, repo *gitrepo.Repo) error {
		rec, _ := w.workspace(t, ws.Slug)
		_, err := os.Stat(w.svc.RepoPath(ws.Slug))
		runs = append(runs, seen{rec.Status, err == nil && repo.Dir() == w.svc.RepoPath(ws.Slug)})
		return errors.New("the post-clone step broke")
	})
	carryPatch := map[string]any{"workspace_mode": "carry_patch", "upstream_url": "https://example.com/up.git"}
	for _, c := range []struct {
		slug    string
		members map[string]any
		// kept runs the clone work on a valid copy of the workspace.
		kept bool
		runs int
	}{
		{"patched", carryPatch, false, 1},
		{"plain", nil, false, 0},
		{"kept", carryPatch, true, 0},
	} {
		w.createFrom(t, c.slug, o.URL(), c.members)
		if c.kept {
			w.clone(t, c.slug)
			w.setPending(t, c.slug, false)
		}
		runs = nil
		rec := w.clone(t, c.slug)
		if len(runs) != c.runs {
			t.Errorf("%s: the step ran %d times, want %d", c.slug, len(runs), c.runs)
		}
		for _, r := range runs {
			if r != (seen{workspace.StatusCloning, true}) {
				t.Errorf("%s: the step saw status %s with the copy in place %v, want cloning and true", c.slug, r.status, r.copied)
			}
		}
		if rec.Status != workspace.StatusReady {
			t.Errorf("%s: status %s, want ready", c.slug, rec.Status)
		}
	}
	if log := w.Log.String(); !strings.Contains(log, "the post-clone step broke") {
		t.Errorf("the hub's log holds no error of the step:\n%s", log)
	}
}
