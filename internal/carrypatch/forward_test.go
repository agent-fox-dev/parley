package carrypatch_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// forwardToken is the origin credential of the forward tests, stored as the
// workspace's GIT_PAT.
const forwardToken = "tok-12345"

// forwardFixture is the workspace demo with a push-taking origin that asks
// for Basic authentication with the token, holding main and patch/a, and
// the hub's copy, which holds them too, records patch/a's origin head, and
// has commit, a commit on top of patch/a that no ref of it names.
type forwardFixture struct {
	origin  *gitrepotest.Origin
	copy    string
	secrets *originSecrets
	ws      workspace.Workspace
	base    string // patch/a on origin and in the copy
	commit  string
}

func newForwardFixture(t *testing.T) *forwardFixture {
	t.Helper()
	o := gitrepotest.SmartOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", forwardToken))
	main := gitrepotest.Commit(t, o.Dir(), "main", map[string]string{"README": "main\n"})
	base := gitrepotest.Commit(t, o.Dir(), "patch a", map[string]string{"a.txt": "a\n"}, main)
	gitrepotest.SetRef(t, o.Dir(), "refs/heads/main", main)
	gitrepotest.SetRef(t, o.Dir(), "refs/heads/patch/a", base)

	f := &forwardFixture{origin: o, copy: filepath.Join(t.TempDir(), "demo.git"), base: base}
	gitrepotest.InitBare(t, f.copy, "main")
	if _, err := gitrepotest.Run(f.copy, gitrepotest.Pack(t, o.Dir(), base), "index-pack", "--stdin"); err != nil {
		t.Fatalf("copy origin's objects: %v", err)
	}
	for ref, id := range map[string]string{
		"refs/heads/main": main, "refs/heads/patch/a": base,
		"refs/remotes/origin/main": main, "refs/remotes/origin/patch/a": base,
	} {
		gitrepotest.SetRef(t, f.copy, ref, id)
	}
	f.commit = gitrepotest.Commit(t, f.copy, "c", map[string]string{"a.txt": "c\n"}, base)
	f.secrets = &originSecrets{tokens: map[string]string{}, fail: map[string]error{}, reads: map[string]int{}}
	f.secrets.set("demo", forwardToken, nil)
	url := o.URL()
	f.ws = workspace.Workspace{Slug: "demo", Mode: workspace.ModeCarryPatch, GitURL: &url}
	return f
}

// component is a carry-patch component bound to the fixture's copy and
// secrets, whose git trusts the origin, with the forward limit given (0 for
// the default).
func (f *forwardFixture) component(limit time.Duration) *carrypatch.Component {
	c := &carrypatch.Component{}
	c.Bind(carrypatch.Deps{
		Secrets:      f.secrets,
		Git:          gitrepo.Git{Env: f.origin.Env()},
		RepoPath:     func(string) string { return f.copy },
		ForwardLimit: limit,
	})
	return c
}

// recorded is the origin head the copy records for branch, "" for none.
func (f *forwardFixture) recorded(branch string) string {
	return gitrepotest.Ref(f.copy, "refs/remotes/origin/"+branch)
}

// without is refs without the ref named.
func without(refs map[string]string, ref string) map[string]string {
	out := maps.Clone(refs)
	delete(out, ref)
	return out
}

func TestForward_PushesTheCommitToOriginAndRecordsItsHead(t *testing.T) {
	t.Parallel()
	f := newForwardFixture(t)
	c := f.component(0)
	ctx := context.Background()

	// Origin takes the commit on its branch, which the copy records; no
	// other ref of origin moves, and the copy's own branch is the hub's to
	// write, not the forward's.
	originBefore := gitrepotest.Refs(t, f.origin.Dir(), "")
	if err := c.Forward(ctx, nil, f.ws, "patch/a", f.commit); err != nil {
		t.Fatalf("the forward failed: %v", err)
	}
	if got := f.origin.Head("patch/a"); got != f.commit {
		t.Errorf("origin's patch/a = %.7s, want the forwarded %.7s", got, f.commit)
	}
	if got := f.recorded("patch/a"); got != f.commit {
		t.Errorf("the recorded origin head of patch/a = %.7s, want %.7s", got, f.commit)
	}
	if got := gitrepotest.Refs(t, f.origin.Dir(), ""); !maps.Equal(without(got, "refs/heads/patch/a"), without(originBefore, "refs/heads/patch/a")) {
		t.Errorf("origin's refs = %v, want all but patch/a as they were, %v", got, originBefore)
	}
	if got := gitrepotest.Ref(f.copy, "refs/heads/patch/a"); got != f.base {
		t.Errorf("the copy's patch/a = %.7s, want it left at %.7s", got, f.base)
	}

	// The credential is read at the forward: one rotated on both sides
	// since the last forward is the one presented.
	f.secrets.set("demo", "tok-67890", nil)
	f.origin.Accept("x-token-auth", "tok-67890")
	next := gitrepotest.Commit(t, f.copy, "c2", map[string]string{"a.txt": "c2\n"}, f.commit)
	if err := c.Forward(ctx, nil, f.ws, "patch/a", next); err != nil {
		t.Fatalf("the forward after the credential was rotated failed: %v", err)
	}
	if got := f.origin.Head("patch/a"); got != next {
		t.Errorf("origin's patch/a = %.7s after the rotation, want %.7s", got, next)
	}

	// A branch origin does not have is created.
	if err := c.Forward(ctx, nil, f.ws, "patch/new", f.commit); err != nil {
		t.Fatalf("the forward of a branch origin lacks failed: %v", err)
	}
	if got, rec := f.origin.Head("patch/new"), f.recorded("patch/new"); got != f.commit || rec != f.commit {
		t.Errorf("origin's patch/new = %.7s, recorded %.7s; want both %.7s", got, rec, f.commit)
	}

	// The commit of a git push is readable only in the push's quarantine
	// until the push is accepted: the forward pushes from the view of it
	// it is handed, and leaves the record to a later refresh, since the
	// copy cannot name a commit it may never hold.
	repo, err := gitrepo.Git{Env: f.origin.Env()}.Open(ctx, f.copy)
	if err != nil {
		t.Fatal(err)
	}
	q, err := repo.NewQuarantine()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Discard() })
	pushed := quarantined(t, f.copy, q, next)
	if err := c.Forward(ctx, q.View(), f.ws, "patch/a", pushed); err != nil {
		t.Fatalf("the forward of a quarantined commit failed: %v", err)
	}
	if got := f.origin.Head("patch/a"); got != pushed {
		t.Errorf("origin's patch/a = %.7s, want the quarantined %.7s", got, pushed)
	}
	if got := f.recorded("patch/a"); got != next {
		t.Errorf("the recorded origin head of patch/a = %.7s, want it left at %.7s", got, next)
	}
	if _, err := gitrepotest.Run(f.copy, "", "cat-file", "-e", pushed); err == nil {
		t.Errorf("the copy holds %.7s outside the quarantine", pushed)
	}

	// A write the ref writer forwards is not accepted yet when its forward
	// returns, even when the copy holds its commit already: the forward it
	// hands a repository records nothing, and the write's trigger records
	// the head once the write is accepted.
	gitrepotest.SetRef(t, f.origin.Dir(), "refs/heads/patch/a", next)
	written := gitrepotest.Commit(t, f.copy, "api", map[string]string{"api.txt": "api\n"}, next)
	if err := c.Forward(ctx, repo, f.ws, "patch/a", written); err != nil {
		t.Fatalf("the forward of a commit the copy holds failed: %v", err)
	}
	if got := f.origin.Head("patch/a"); got != written {
		t.Errorf("origin's patch/a = %.7s, want the forwarded %.7s", got, written)
	}
	if got := f.recorded("patch/a"); got != next {
		t.Errorf("the recorded origin head of patch/a = %.7s after a write's forward, want it left at %.7s", got, next)
	}
}

func TestForward_ACallerGoneWhileTheCredentialIsReadTimesTheForwardOut(t *testing.T) {
	t.Parallel()
	f := newForwardFixture(t)
	c := f.component(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.secrets.set("demo", "", context.Canceled)
	requests := f.origin.Requests()
	err := c.Forward(ctx, nil, f.ws, "patch/a", f.commit)
	if err == nil || err.Error() != "origin rejected push: timed out" {
		t.Errorf("the forward of a caller that left = %v, want origin rejected push: timed out", err)
	}
	if n := f.origin.Requests() - requests; n != 0 {
		t.Errorf("origin received %d requests, want none", n)
	}
}

// quarantined is a commit on top of parent, a commit of the repository dir,
// as a client pushes it: its objects are received into q and exist nowhere
// else in dir.
func quarantined(t *testing.T, dir string, q *gitrepo.Quarantine, parent string) string {
	t.Helper()
	client := filepath.Join(t.TempDir(), "client.git")
	gitrepotest.InitBare(t, client, "main")
	info := filepath.Join(client, "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(dir, "objects")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := gitrepotest.Commit(t, client, "pushed", map[string]string{"a.txt": "pushed\n"}, parent)
	if err := q.Receive(context.Background(), strings.NewReader(gitrepotest.Pack(t, client, id, "^"+parent))); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestForward_FailsWithItsReasonAndLeavesOriginAndTheRecordAsTheyWere(t *testing.T) {
	t.Parallel()
	f := newForwardFixture(t)
	c := f.component(0)
	dead := gitrepotest.NewOrigin(t)
	dead.Close()
	deadURL := dead.URL()
	exact := func(want string) func(string) bool { return func(msg string) bool { return msg == want } }
	for _, tc := range []struct {
		name   string
		setup  func(ws *workspace.Workspace)
		want   func(msg string) bool
		wanted string // what want looks for, for the failure's text
		// silent is set where origin must not be contacted at all.
		silent bool
	}{
		{name: "a hook of origin refuses",
			setup: func(*workspace.Workspace) { f.origin.Reject("branch is frozen") },
			want:  exact("origin rejected push: branch is frozen"), wanted: "origin rejected push: branch is frozen"},
		{name: "the hook's text holds the credential",
			setup: func(*workspace.Workspace) { f.origin.Reject("refused for " + forwardToken) },
			want: func(msg string) bool {
				return strings.HasPrefix(msg, "origin rejected push: ") && strings.Contains(msg, "[redacted]") && !strings.Contains(msg, forwardToken)
			},
			wanted: "the hook's text with the credential [redacted]"},
		{name: "the hook's text is 3000 characters long",
			setup: func(*workspace.Workspace) { f.origin.Reject(strings.Repeat("x", 3000)) },
			want: func(msg string) bool {
				return strings.HasPrefix(msg, "origin rejected push: x") && len(msg) <= 1024
			},
			wanted: "the hook's text cut to at most 1024 bytes"},
		{name: "origin's branch has a commit the pushed one lacks",
			setup: func(*workspace.Workspace) { f.origin.Commit("patch/a", "elsewhere") },
			want:  exact("origin rejected push: non-fast-forward"), wanted: "origin rejected push: non-fast-forward"},
		{name: "origin answers 401",
			setup: func(*workspace.Workspace) { f.origin.Refuse(401) },
			want:  exact("origin rejected push: authentication failed"), wanted: "origin rejected push: authentication failed"},
		{name: "origin answers 403",
			setup: func(*workspace.Workspace) { f.origin.Refuse(403) },
			want:  exact("origin rejected push: permission denied"), wanted: "origin rejected push: permission denied"},
		{name: "origin answers 404",
			setup: func(*workspace.Workspace) { f.origin.Refuse(404) },
			want:  exact("origin rejected push: repository not found"), wanted: "origin rejected push: repository not found"},
		{name: "origin answers 500",
			setup: func(*workspace.Workspace) { f.origin.Refuse(500) },
			want:  exact("origin rejected push: push failed"), wanted: "origin rejected push: push failed"},
		{name: "nothing listens at origin's address",
			setup: func(ws *workspace.Workspace) { ws.GitURL = &deadURL },
			want:  exact("origin rejected push: origin unreachable"), wanted: "origin rejected push: origin unreachable"},
		{name: "the workspace has no origin",
			setup: func(ws *workspace.Workspace) { ws.GitURL = nil },
			want:  exact("origin rejected push: push failed"), wanted: "origin rejected push: push failed", silent: true},
		{name: "the origin credential cannot be read",
			setup: func(*workspace.Workspace) { f.secrets.set("demo", "", errors.New("secret store sealed")) },
			want:  exact("failed to resolve origin credentials"), wanted: "failed to resolve origin credentials", silent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := f.ws
			tc.setup(&ws)
			t.Cleanup(func() {
				f.origin.Reject("")
				f.origin.Refuse(0)
				f.secrets.set("demo", forwardToken, nil)
				gitrepotest.SetRef(t, f.origin.Dir(), "refs/heads/patch/a", f.base)
			})
			originBefore, recordBefore, requests := f.origin.Head("patch/a"), f.recorded("patch/a"), f.origin.Requests()
			err := c.Forward(context.Background(), nil, ws, "patch/a", f.commit)
			if err == nil {
				t.Fatalf("the forward succeeded, want it to fail with %s", tc.wanted)
			}
			if msg := err.Error(); !tc.want(msg) || strings.Contains(msg, "@") {
				t.Errorf("the forward failed with %q, want %s and no '@'", msg, tc.wanted)
			}
			if got := f.origin.Head("patch/a"); got != originBefore {
				t.Errorf("origin's patch/a = %.7s, want it left at %.7s", got, originBefore)
			}
			if got := f.recorded("patch/a"); got != recordBefore {
				t.Errorf("the recorded origin head of patch/a = %.7s, want it left at %.7s", got, recordBefore)
			}
			if n := f.origin.Requests() - requests; tc.silent && n != 0 {
				t.Errorf("origin received %d requests, want none", n)
			}
		})
	}
}

// Not parallel: it puts a git that records its process id first on the
// PATH of the test process, so that the test sees the forward's git itself.
func TestForward_EndsThePushWhenItsLimitPassesOrItsCallerCancels(t *testing.T) {
	f := newForwardFixture(t)
	gits := recordGits(t)
	f.origin.Block()
	for _, tc := range []struct {
		name   string
		limit  time.Duration
		cancel bool // whether the caller gives up while the push is under way
	}{
		// The limit is far longer than a push needs to start and far
		// shorter than the 5s the forward must return well within: a limit
		// of a few hundred milliseconds passes, on a loaded machine,
		// before git has even started.
		{"the limit passes", 2 * time.Second, false},
		{"the caller cancels", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := f.component(tc.limit)
			if tc.limit == 0 && c.ForwardLimit() != carrypatch.DefaultForwardLimit {
				t.Fatalf("the forward limit is %v, want the default %v", c.ForwardLimit(), carrypatch.DefaultForwardLimit)
			}
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				// The caller gives up once the push has reached origin,
				// which holds it. A fixed wait would, on a loaded machine,
				// end the forward before its git ever started, leaving
				// nothing for the cancellation to end.
				go cancelOnRequest(ctx, f.origin, f.origin.Requests(), cancel)
			}
			gits.reset(t)
			start := time.Now()
			err := c.Forward(ctx, nil, f.ws, "patch/a", f.commit)
			pushes := gits.started(t, "push")
			if len(pushes) == 0 {
				t.Fatalf("the forward started no git push (it returned %v)", err)
			}
			// The forward waited for its git: by its return git is gone, not
			// even left for its parent to reap. Its transport helpers ran in
			// git's process group, killed with it, and origin still holds
			// their requests, so one left alive would stay alive.
			for _, pid := range pushes {
				if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
					t.Errorf("the forward's git push (pid %d) still exists after the forward returned", pid)
				}
				if left := liveInGroup(t, pid, 2*time.Second); len(left) > 0 {
					t.Errorf("processes %v started by the forward's git push (pid %d) still run 2s after the forward returned", left, pid)
				}
			}
			// The limit or the cancellation ended the push, and not the
			// default limit of two minutes: how long the push took to
			// reach origin before that is the machine's business, so the
			// bound is far from both.
			if took := time.Since(start); took >= 30*time.Second {
				t.Errorf("the forward returned after %v, want well before the default limit %v", took, carrypatch.DefaultForwardLimit)
			}
			if err == nil || err.Error() != "origin rejected push: timed out" {
				t.Errorf("the forward returned %v, want origin rejected push: timed out", err)
			}
			if got := f.recorded("patch/a"); got != f.base {
				t.Errorf("the recorded origin head of patch/a = %.7s, want it left at %.7s", got, f.base)
			}
		})
	}
	// Once origin lets the held requests through, nobody is left to push.
	f.origin.Release()
	time.Sleep(200 * time.Millisecond)
	if got := f.origin.Head("patch/a"); got != f.base {
		t.Errorf("origin's patch/a = %.7s after the held requests were let through, want %.7s", got, f.base)
	}
}

// cancelOnRequest cancels once the origin has received a request beyond
// before, and gives up when ctx ends; it cancels anyway after a wait no
// push can need, so that a push that never reaches origin fails the test
// instead of holding it until the forward's own limit.
func cancelOnRequest(ctx context.Context, origin *gitrepotest.Origin, before int, cancel context.CancelFunc) {
	defer cancel()
	for deadline := time.Now().Add(30 * time.Second); origin.Requests() <= before; {
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// gitLog is the record of the git processes started while it is in place:
// a git first on the PATH of the test process that writes its process id
// and arguments to the log and then runs as the real git, in the same
// process.
type gitLog struct{ path string }

// recordGits puts the recording git in place until the test ends.
func recordGits(t *testing.T) *gitLog {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	l := &gitLog{path: filepath.Join(bin, "started")}
	script := "#!/bin/sh\necho \"$$ $*\" >> '" + l.path + "'\nexec '" + gitPath + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// macOS checks a new executable the first time it runs, which may take
	// longer than a forward's limit in the test: run it once beforehand.
	if out, err := exec.Command("git", "--version").CombinedOutput(); err != nil {
		t.Fatalf("the recording git does not run: %v: %s", err, out)
	}
	l.reset(t)
	return l
}

// reset forgets the git processes recorded so far.
func (l *gitLog) reset(t *testing.T) {
	t.Helper()
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// started is the process ids of the recorded git processes that ran the
// git command cmd.
func (l *gitLog) started(t *testing.T, cmd string) []int {
	t.Helper()
	b, err := os.ReadFile(l.path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var pids []int
	for line := range strings.Lines(string(b)) {
		fields := strings.Fields(line)
		if len(fields) < 2 || !slices.Contains(fields[1:], cmd) {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("the recording git wrote %q", line)
		}
		pids = append(pids, pid)
	}
	return pids
}

// liveInGroup is the processes of the process group pgid that still run,
// zombies left out, once they all ended or wait passed.
func liveInGroup(t *testing.T, pgid int, wait time.Duration) []int {
	t.Helper()
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,stat=").Output()
		if err != nil {
			t.Fatalf("ps: %v", err)
		}
		var live []int
		for line := range strings.Lines(string(out)) {
			f := strings.Fields(line)
			if len(f) == 3 && f[1] == strconv.Itoa(pgid) && !strings.HasPrefix(f[2], "Z") {
				pid, _ := strconv.Atoi(f[0])
				live = append(live, pid)
			}
		}
		if len(live) == 0 || time.Now().After(deadline) {
			return live
		}
	}
}
