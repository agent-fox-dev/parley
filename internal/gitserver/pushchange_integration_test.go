package gitserver_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
// with one empty commit, and returns the commit's id.
func branchWithCommit(t *testing.T, dir, name string) string {
	t.Helper()
	runGit(t, dir, "checkout", "--quiet", "-b", name, "main")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", name)
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
	} {
		t.Run(c.name, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "trace")
			before, seq := w.git.Requests(), w.lastSeq(t)
			args := append(append([]string{"push"}, c.options...), "origin", "topic", "other")
			out, err := clientGit(t, dir, []string{"GIT_TRACE_CURL=" + trace, "GIT_TRACE_CURL_NO_DATA=1"}, args...)
			if err == nil {
				t.Fatalf("git push succeeded, want it refused:\n%s", out)
			}
			var errLines []string
			for line := range strings.Lines(out) {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "remote: error:") {
					errLines = append(errLines, line)
				}
			}
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
