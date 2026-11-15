package gitserver_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// The end-to-end tests of what a push's options make of it: a stock git
// client sending push options to the hub served in-process, with the hub's
// own options stage and change component.

// pushSession is the session the tests name with parley.session.
const pushSession = "0f8fad5b-d9cb-469f-a165-70867728950e"

// acceptingSessions registers on the hub's options stage a session linker
// that accepts every session id.
func acceptingSessions(c *hub.Composition) {
	c.PushOptions().RegisterSessionLinker(&sessionStub{})
}

// branchWithCommit makes the branch name off main in the working tree dir,
// with one empty commit, whose body is the intent of a change opened from
// it, and returns the commit's id.
func branchWithCommit(t *testing.T, dir, name string) string {
	t.Helper()
	runGit(t, dir, "checkout", "--quiet", "-b", name, "main")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", name, "-m", "Because "+name+" must be.")
	return strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
}

// changeCount is the number of changes the hub holds.
func (w *world) changeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRow(`SELECT COUNT(*) FROM changes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// changeSessions is the change component's session linker in the tests: it
// accepts every session id and records nothing.
type changeSessions struct{}

func (changeSessions) Check(_ context.Context, _ change.Caller, id, _ string) (string, error) {
	return strings.ToLower(id), nil
}

func (changeSessions) Record(context.Context, *sql.Tx, string, string, int64, int64) error {
	return nil
}

// linesWith are the lines of git's output out that start with prefix,
// trimmed.
func linesWith(out, prefix string) []string {
	var lines []string
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// openChange opens a change of demo from its branch with the owner's key.
func (w *world) openChange(t *testing.T, branch string) {
	t.Helper()
	status, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", w.ownerKey, map[string]any{"source": map[string]any{"branch": branch}})
	if status != http.StatusCreated {
		t.Fatalf("open a change of %s = %d %s, want 201", branch, status, body)
	}
}

// demoChange is what the changes API shows, to the owner, of the change or
// version of demo at path under …/changes/, such as "1" or "1/versions/2";
// Number is a version's own number.
type demoChange struct {
	WorkspaceSlug string  `json:"workspace_slug"`
	Number        int     `json:"number"`
	Version       int     `json:"version"`
	Kind          string  `json:"kind"`
	HeadSHA       string  `json:"head_sha"`
	SessionID     *string `json:"session_id"`
	Title         string  `json:"title"`
	Intent        string  `json:"intent"`
	State         string  `json:"state"`
	TargetBranch  string  `json:"target_branch"`
	Source        struct {
		WorkspaceSlug string `json:"workspace_slug"`
		Branch        string `json:"branch"`
	} `json:"source"`
	Author struct {
		Username *string `json:"username"`
	} `json:"author"`
}

func (w *world) demoChange(t *testing.T, path string) demoChange {
	t.Helper()
	status, body := w.Call(t, http.MethodGet, "/api/v1/workspaces/demo/changes/"+path, w.ownerKey, nil)
	var c demoChange
	if status != http.StatusOK || json.Unmarshal(body, &c) != nil {
		t.Fatalf("GET changes/%s = %d %s, want 200", path, status, body)
	}
	return c
}

// receivePackStatus is the status line of the hub's answer to the last
// git-receive-pack request in the curl trace git wrote to path.
func receivePackStatus(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(raw)
	i := strings.LastIndex(trace, "POST /git/acme/demo.git/git-receive-pack")
	if i < 0 {
		t.Fatalf("the trace holds no receive-pack request:\n%s", trace)
	}
	_, after, _ := strings.Cut(trace[i:], "Recv header: ")
	line, _, _ := strings.Cut(after, "\n")
	return strings.TrimSpace(line)
}

// A push whose options are refused is answered before its pack is stored:
// no object, ref, event or change is written, and the client is told once
// on the side band and on every ref.
func TestPushChange_RefusesInvalidOptionsBeforeThePack(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	dir := w.cloneDemo(t, w.ownerKey)
	topic := branchWithCommit(t, dir, "topic")
	branchWithCommit(t, dir, "other")
	var many []string
	for i := 1; i <= 17; i++ {
		many = append(many, "-o", fmt.Sprintf("x%d", i))
	}
	for _, c := range []struct {
		name    string
		options []string
		message string
	}{
		{"an unknown parley option", []string{"-o", "parley.titel=x"}, "invalid push option parley.titel: unknown option"},
		{"too many options", many, "too many push options: at most 16 are accepted"},
		{"an option name longer than a pkt-line", []string{"-o", strings.Repeat("a", 65500)}, "invalid push option " + strings.Repeat("a", 1024) + "...: longer than 1024 bytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "trace")
			before, seq := w.git.Requests(), w.lastSeq(t)
			args := append(append([]string{"push"}, c.options...), "origin", "topic", "other")
			out, err := clientGit(t, dir, []string{"GIT_TRACE_CURL=" + trace, "GIT_TRACE_CURL_NO_DATA=1"}, args...)
			if err == nil {
				t.Fatalf("git push succeeded, want it refused:\n%s", out)
			}
			errLines := linesWith(out, "remote: error:")
			if want := []string{"remote: error: " + c.message}; !slices.Equal(errLines, want) {
				t.Errorf("error lines = %q, want %q", errLines, want)
			}
			for _, b := range []string{"topic", "other"} {
				if want := "! [remote rejected] " + b + " -> " + b + " (" + c.message + ")"; !strings.Contains(out, want) {
					t.Errorf("git's output lacks %q:\n%s", want, out)
				}
			}
			if got := receivePackStatus(t, trace); !strings.HasPrefix(got, "HTTP/1.1 200") {
				t.Errorf("receive-pack answered %q, want 200", got)
			}
			refs := gitrepotest.Refs(t, w.repo, "refs/heads/")
			if _, found := refs["topic"]; found || refs["other"] != "" || readable(w.repo, topic) {
				t.Errorf("hub branches %v, topic's commit readable %t: want neither branch nor the commit", refs, readable(w.repo, topic))
			}
			if got := w.lastSeq(t); got != seq || w.changeCount(t) != 0 {
				t.Errorf("event log moved from %d to %d, %d changes: want no event and no change", seq, got, w.changeCount(t))
			}
			refused := gitserver.RequestLabels{Service: gitserver.LabelReceivePack, Result: gitserver.ResultRefused}
			if got := grown(before, w.git.Requests())[refused]; got != 1 {
				t.Errorf("refused receive-pack requests grew by %d, want 1", got)
			}
		})
	}
}

// The session of parley.session goes on every parley.git.push event of the
// push, whatever ref it moves and whether or not a change is involved, and
// a push without the option has none.
func TestPushChange_GivesThePushSessionToEveryEvent(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{Compose: acceptingSessions})
	dir := w.cloneDemo(t, w.ownerKey)
	branchWithCommit(t, dir, "topic")
	runGit(t, dir, "tag", "v1")
	seq := w.lastSeq(t)
	runGit(t, dir, "push", "--quiet", "-o", "parley.session="+pushSession, "origin", "topic", "refs/tags/v1")
	evs := w.pushEvents(t, seq)
	if len(evs) != 2 {
		t.Fatalf("push events = %+v, want one per ref", evs)
	}
	for _, ev := range evs {
		if ev.SessionID == nil || *ev.SessionID != pushSession {
			t.Errorf("event of %s has session %v, want %s", ev.Ref, ev.SessionID, pushSession)
		}
	}
	if n := w.changeCount(t); n != 0 {
		t.Errorf("the hub holds %d changes, want none", n)
	}

	branchWithCommit(t, dir, "plain")
	seq = w.lastSeq(t)
	runGit(t, dir, "push", "--quiet", "origin", "plain")
	if evs := w.pushEvents(t, seq); len(evs) != 1 || evs[0].SessionID != nil {
		t.Errorf("push events = %+v, want one without a session", evs)
	}
}

// The lines of a push name each branch's change in the order of the push's
// commands: a version for the branch that has a change, an opened change for
// the one that asks for it, and nothing for a tag. Stock git sends the
// commands of refs the hub has before those of new refs, whatever the order
// of its command line, so the order is read from git's packet trace.
func TestPushChange_ReportsEachBranchInCommandOrder(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	dir := w.cloneDemo(t, w.ownerKey)
	branchWithCommit(t, dir, "a")
	runGit(t, dir, "push", "--quiet", "origin", "a")
	w.openChange(t, "a")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "a again")
	branchWithCommit(t, dir, "b")
	runGit(t, dir, "tag", "v1")
	trace := filepath.Join(t.TempDir(), "trace")
	out, err := clientGit(t, dir, []string{"GIT_TRACE_PACKET=" + trace}, "push", "-o", "parley.change", "origin", "b", "a", "refs/tags/v1")
	raw, rerr := os.ReadFile(trace)
	if err != nil || rerr != nil {
		t.Fatalf("git push: %v, trace: %v:\n%s", err, rerr, out)
	}
	lineOf := map[string]string{"refs/heads/a": "remote: change demo#1 version 2", "refs/heads/b": "remote: change demo#2 version 1 (opened)"}
	var want []string
	for _, m := range regexp.MustCompile(`git> [0-9a-f]{40} [0-9a-f]{40} (refs/[^\s\\]+)`).FindAllStringSubmatch(string(raw), -1) {
		if line, ok := lineOf[m[1]]; ok {
			want = append(want, line)
		}
	}
	if got := linesWith(out, "remote: change "); len(want) != 2 || !slices.Equal(got, want) {
		t.Errorf("change lines = %q, want %q, one per branch in the order git sent them:\n%s", got, want, out)
	}
	for _, line := range linesWith(out, "remote:") {
		if strings.Contains(line, "v1") {
			t.Errorf("the line %q names the tag", line)
		}
	}
}

// A client that asked for no side band gets the plain status report, and
// its push is mapped all the same.
func TestPushChange_MapsAPushWithoutASideBandAndSendsNoLine(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	cmds := []command{{zero, w.commit(t, w.head, "quiet\n\nBecause quiet must be."), "refs/heads/quiet"}}
	a := w.receive(t, "acme/demo.git", w.ownerKey,
		receiveRequest([]string{"report-status", "push-options"}, cmds, []string{"parley.change"}, w.packFor(t, cmds)))
	rep := parseReport(a.body)
	if a.status != http.StatusOK || !slices.Equal(rep.lines, []string{"unpack ok", "ok refs/heads/quiet"}) ||
		strings.ContainsAny(a.body, "\x01\x02") || strings.Contains(a.body, "change demo#") {
		t.Errorf("answer = %d %q, want 200 and the plain status report", a.status, a.body)
	}
	if n, c := w.changeCount(t), w.demoChange(t, "1"); n != 1 || c.Source.Branch != "quiet" {
		t.Errorf("the hub holds %d changes, the first from %q: want one from quiet", n, c.Source.Branch)
	}
}

// A push that is refused, whose pack is too large or that updates nothing
// opens no change and sends no change line, whatever it asks.
func TestPushChange_MapsNothingForAPushThatMovesNoRef(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{TOML: "[git]\nmax_push_size = \"4096\"\n"})
	dir := w.cloneDemo(t, w.ownerKey)
	noise := make([]byte, 16<<10)
	_, _ = rand.Read(noise)
	for _, c := range []struct {
		name    string
		prepare func()
		refs    []string
		refused bool
	}{
		{"a ref refused by a push rule", func() { branchWithCommit(t, dir, "topic") }, []string{"topic", "topic:refs/parley/x"}, true},
		{"a pack over the maximum push size", func() {
			branchWithCommit(t, dir, "big")
			writeFiles(t, dir, map[string]string{"big.txt": hex.EncodeToString(noise)})
			runGit(t, dir, "commit", "--quiet", "-m", "big")
		}, []string{"big"}, true},
		{"nothing to update", func() { runGit(t, dir, "checkout", "--quiet", "main") }, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.prepare()
			out, err := clientGit(t, dir, nil, append([]string{"push", "-o", "parley.change", "origin"}, c.refs...)...)
			if (err != nil) != c.refused {
				t.Fatalf("git push: %v, want refused %t:\n%s", err, c.refused, out)
			}
			if lines := append(linesWith(out, "remote: change "), linesWith(out, "remote: warning: no change")...); len(lines) != 0 {
				t.Errorf("change lines %q, want none", lines)
			}
			if n := w.changeCount(t); n != 0 {
				t.Errorf("the hub holds %d changes, want none", n)
			}
		})
	}
}

// The session of parley.session goes on the version a push records on its
// branch's change and on the change it opens; with none, the version has no
// session and the opened change the change component's default, none for a
// repository's branch.
func TestPushChange_GivesThePushSessionToVersionsAndOpenedChanges(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		acceptingSessions(c)
		c.Changes().RegisterSessionLinker(changeSessions{})
	}})
	dir := w.cloneDemo(t, w.ownerKey)
	branchWithCommit(t, dir, "fix")
	runGit(t, dir, "push", "--quiet", "origin", "fix")
	w.openChange(t, "fix")
	for i, c := range []struct {
		name    string
		session *string
	}{
		{"the push's session", ptr(pushSession)},
		{"no session", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			push := []string{"push", "--quiet"}
			if c.session != nil {
				push = append(push, "-o", "parley.session="+*c.session)
			}
			runGit(t, dir, "checkout", "--quiet", "fix")
			runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", c.name)
			head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			runGit(t, dir, append(push, "origin", "fix")...)
			v := w.demoChange(t, fmt.Sprintf("1/versions/%d", w.demoChange(t, "1").Version))
			if v.Kind != "push" || v.HeadSHA != head || session(v.SessionID) != session(c.session) {
				t.Errorf("newest version of change 1 = %+v, want kind push at %s with session %s", v, head, session(c.session))
			}

			other := fmt.Sprintf("other%d", i)
			branchWithCommit(t, dir, other)
			runGit(t, dir, append(push, "-o", "parley.change", "origin", other)...)
			if opened := w.demoChange(t, strconv.Itoa(2+i)); opened.Source.Branch != other || session(opened.SessionID) != session(c.session) {
				t.Errorf("change %d = %+v, want one from %s with session %s", 2+i, opened, other, session(c.session))
			}
		})
	}
}

// A change a push opens takes its target, title, intent and draft state
// from the push's options, and the defaults of a change for an option left
// out: the default branch, the head commit's subject and body, open. Its
// source is the pushed branch and its author the pusher.
func TestPushChange_OpensAChangeWithTheOptionsOfThePush(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	dir := w.cloneDemo(t, w.ownerKey)
	runGit(t, dir, "push", "--quiet", "origin", "main:release")
	const subject, body = "Fix retry loop", "Retries never stopped."
	for i, c := range []struct {
		name                         string
		body                         string
		options                      []string
		title, intent, state, target string
	}{
		{"no option but the change", body, []string{"parley.change"}, subject, body, "open", "main"},
		{"a target", body, []string{"parley.target=release"}, subject, body, "open", "release"},
		{"a title and an intent", body, []string{"parley.change", "parley.title=T", "parley.intent=I"}, "T", "I", "open", "main"},
		{"a draft", body, []string{"parley.change", "parley.draft"}, subject, body, "draft", "main"},
		// demo requires an intent, the default of changes.require_intent,
		// which a draft may still lack.
		{"a draft without an intent", "", []string{"parley.change", "parley.draft"}, subject, "", "draft", "main"},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := fmt.Sprintf("fix%d", i)
			runGit(t, dir, "checkout", "--quiet", "-b", branch, "main")
			commit := []string{"commit", "--quiet", "--allow-empty", "-m", subject}
			if c.body != "" {
				commit = append(commit, "-m", c.body)
			}
			runGit(t, dir, commit...)
			push := []string{"push"}
			for _, o := range c.options {
				push = append(push, "-o", o)
			}
			out := runGit(t, dir, append(push, "origin", branch)...)
			opened := "opened"
			if c.state == "draft" {
				opened += ", draft"
			}
			if want := fmt.Sprintf("remote: change demo#%d version 1 (%s)", i+1, opened); !slices.Contains(linesWith(out, "remote: change "), want) {
				t.Errorf("git's output lacks %q:\n%s", want, out)
			}
			got := w.demoChange(t, strconv.Itoa(i+1))
			if got.Title != c.title || got.Intent != c.intent || got.State != c.state || got.TargetBranch != c.target ||
				got.Source.WorkspaceSlug != "demo" || got.Source.Branch != branch || got.Author.Username == nil || *got.Author.Username != "owner" {
				t.Errorf("change %d = %+v, want title %q, intent %q, state %s, target %s, source demo:%s and author owner",
					i+1, got, c.title, c.intent, c.state, c.target, branch)
			}
		})
	}
}

// A push to a branch whose change is open adds a version and leaves the
// change's title, intent, target and state as they were, whatever options
// the push carries.
func TestPushChange_LeavesTheChangeOfAPushedBranchAsItWas(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	dir := w.cloneDemo(t, w.ownerKey)
	runGit(t, dir, "push", "--quiet", "origin", "main:release")
	branchWithCommit(t, dir, "fix")
	runGit(t, dir, "push", "--quiet", "-o", "parley.change", "-o", "parley.title=Old", "-o", "parley.intent=Why", "origin", "fix")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "again")
	out := runGit(t, dir, "push", "-o", "parley.title=New", "-o", "parley.intent=Other", "-o", "parley.target=release", "-o", "parley.draft", "origin", "fix")
	if got := linesWith(out, "remote: change "); !slices.Equal(got, []string{"remote: change demo#1 version 2"}) {
		t.Errorf("change lines = %q, want the second version of demo#1", got)
	}
	if c := w.demoChange(t, "1"); c.Version != 2 || c.Title != "Old" || c.Intent != "Why" || c.TargetBranch != "main" || c.State != "open" || w.changeCount(t) != 1 {
		t.Errorf("change 1 = %+v among %d changes, want the only one, at version 2, with title Old, intent Why, target main, open", c, w.changeCount(t))
	}
}

// A protected branch that is the source of an open change gets a version
// when its owner pushes it directly, whatever the push asks, and the
// client is told of the version rather than warned.
func TestPushChange_AddsAVersionForADirectPushOfAChangesSource(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	dir := w.cloneDemo(t, w.ownerKey)
	runGit(t, dir, "push", "--quiet", "origin", "main:release")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "first")
	runGit(t, dir, "push", "--quiet", "origin", "main")
	status, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", w.ownerKey, map[string]any{
		"source": map[string]any{"branch": "main"}, "target_branch": "release", "title": "Release main", "intent": "Ship it."})
	if status != http.StatusCreated {
		t.Fatalf("open a change of main onto release = %d %s, want 201", status, body)
	}
	for i, options := range [][]string{nil, {"parley.change"}} {
		runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", fmt.Sprintf("direct %d", i))
		head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		push := []string{"push"}
		for _, o := range options {
			push = append(push, "-o", o)
		}
		out := runGit(t, dir, append(push, "origin", "main")...)
		version := i + 2
		if got, want := linesWith(out, "remote: "), []string{fmt.Sprintf("remote: change demo#1 version %d", version)}; !slices.Equal(got, want) {
			t.Errorf("options %q: remote lines = %q, want %q", options, got, want)
		}
		if c := w.demoChange(t, "1"); c.Version != version || c.HeadSHA != head || c.State != "open" {
			t.Errorf("options %q: change 1 = %+v, want open at version %d with head %s", options, c, version, head)
		}
		if v := w.demoChange(t, fmt.Sprintf("1/versions/%d", version)); v.Kind != "push" || v.HeadSHA != head {
			t.Errorf("options %q: version %d = %+v, want kind push at %s", options, version, v, head)
		}
	}
}

// The change of a branch pushed to a task fork is opened in the fork's
// parent, from the fork's branch, onto the fork's base branch; the fork
// holds no change of its own.
func TestPushChange_OpensTheChangeOfAForkBranchInTheParent(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/release", w.head)
	w.addForkFrom(t, "release", []string{"release"}, nil)
	gitrepotest.Git(t, w.workspaces.RepoPath("demo--t1"), "update-ref", "refs/heads/release", w.head)
	token, _ := w.pat(t, w.member, "demo--t1", "git:write", "changes:write")
	dir := filepath.Join(t.TempDir(), "fork")
	runGit(t, t.TempDir(), "clone", "--quiet", w.remote(token, "acme/demo--t1.git"), dir)
	runGit(t, dir, "checkout", "--quiet", "-b", "work", "origin/release")
	runGit(t, dir, "-c", "user.name=P", "-c", "user.email=p@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "work", "-m", "Because work must be.")
	out := runGit(t, dir, "push", "-o", "parley.change", "origin", "work")
	if got := linesWith(out, "remote: change "); !slices.Equal(got, []string{"remote: change demo#1 version 1 (opened)"}) {
		t.Errorf("change lines = %q, want demo#1 opened:\n%s", got, out)
	}
	if c := w.demoChange(t, "1"); c.WorkspaceSlug != "demo" || c.Source.WorkspaceSlug != "demo--t1" || c.Source.Branch != "work" || c.TargetBranch != "release" {
		t.Errorf("change 1 = %+v, want one of demo from demo--t1:work onto release", c)
	}
	status, body := w.Call(t, http.MethodGet, "/api/v1/workspaces/demo--t1/changes", w.memberKey, nil)
	var list struct {
		Items []demoChange `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET demo--t1's changes = %d %s, want 200", status, body)
	}
	for _, c := range list.Items {
		if c.WorkspaceSlug != "demo" {
			t.Errorf("the fork's change list holds %+v, a change of %s, want only changes of demo", c, c.WorkspaceSlug)
		}
	}
}

// A change the hub cannot open for a pushed branch costs the push nothing:
// git exits 0, the branch is at the pushed head on the hub, its push event
// is in the log, and the reason is on a warning line.
func TestPushChange_WarnsOfAChangeItCannotOpenAndKeepsThePush(t *testing.T) {
	w := newGitWorld(t, hubtest.Config{})
	dir := w.cloneDemo(t, w.ownerKey)
	gitOnly, _ := w.pat(t, w.owner, "", "git:write")
	for _, c := range []struct {
		name, branch string
		prepare      func()
		option       string
		password     string
		reason       string
	}{
		{"a target that does not exist", "fix", func() { branchWithCommit(t, dir, "fix") }, "parley.target=nope", w.ownerKey,
			"target branch nope does not exist (target_not_found)"},
		{"a branch at the target's head", "same", func() { runGit(t, dir, "checkout", "--quiet", "-b", "same", "main") }, "parley.change", w.ownerKey,
			"no commits ahead of main (nothing_to_land)"},
		{"a branch holding a merge commit", "merged", func() {
			branchWithCommit(t, dir, "side")
			branchWithCommit(t, dir, "merged")
			runGit(t, dir, "merge", "--quiet", "--no-ff", "-m", "merge side", "side")
		}, "parley.change", w.ownerKey, "merge commits are not supported (merge_commits_unsupported)"},
		{"a PAT without changes:write", "scoped", func() { branchWithCommit(t, dir, "scoped") }, "parley.change", gitOnly,
			"token lacks scope changes:write"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.prepare()
			head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			seq := w.lastSeq(t)
			out, err := clientGit(t, dir, nil, "push", "-o", c.option, w.remote(c.password, "acme/demo.git"), c.branch)
			if err != nil || strings.Contains(out, "[remote rejected]") {
				t.Fatalf("git push: %v, want it accepted:\n%s", err, out)
			}
			want := []string{"remote: warning: no change for " + c.branch + ": " + c.reason}
			if got := linesWith(out, "remote: warning: no change for "); !slices.Equal(got, want) {
				t.Errorf("warning lines = %q, want %q:\n%s", got, want, out)
			}
			if got := gitrepotest.Refs(t, w.repo, "refs/heads/")[c.branch]; got != head {
				t.Errorf("the hub's %s is at %q, want the pushed head %s", c.branch, got, head)
			}
			if evs := w.pushEvents(t, seq); len(evs) != 1 || evs[0].Ref != "refs/heads/"+c.branch {
				t.Errorf("push events = %+v, want one for refs/heads/%s", evs, c.branch)
			}
			if n := w.changeCount(t); n != 0 {
				t.Errorf("the hub holds %d changes, want none", n)
			}
		})
	}
}
